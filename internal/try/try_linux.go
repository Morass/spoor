package try

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func Supported() error { return nil }

// Run executes spec.Argv in a private mount namespace where every root is
// an overlay whose writes land in the workspace. Returns the exit code.
func Run(spec Spec) (int, error) {
	for _, r := range spec.Roots {
		if strings.ContainsAny(r, ",:") {
			return 0, fmt.Errorf("try: root %q contains ',' or ':' which overlay options cannot express", r)
		}
	}
	for i := range spec.Roots {
		if err := os.MkdirAll(spec.Upper(i), 0o755); err != nil {
			return 0, err
		}
		if err := os.MkdirAll(spec.Work(i), 0o755); err != nil {
			return 0, err
		}
	}
	spec.UserNS = os.Geteuid() != 0
	specFile := filepath.Join(spec.Workspace, "spec.json")
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(specFile, b, 0o600); err != nil {
		return 0, err
	}
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(self, "__try-child", specFile)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	attr := &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if spec.UserNS {
		attr.Cloneflags |= syscall.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
	}
	cmd.SysProcAttr = attr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ee.ExitCode() == 201 {
				return 0, errors.New("try: could not set up overlays (see message above)")
			}
			return ee.ExitCode(), nil
		}
		if spec.UserNS && (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || strings.Contains(err.Error(), "operation not permitted")) {
			return 0, errors.New("try: unprivileged user namespaces are blocked here (Ubuntu: kernel.apparmor_restrict_unprivileged_userns=1). Run spoor try with sudo, or allow user namespaces for the spoor binary via an AppArmor profile")
		}
		return 0, err
	}
	return 0, nil
}

// Child runs inside the new namespace: mount overlays, then exec.
func Child(specFile string) {
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "spoor try: "+format+"\n", a...)
		os.Exit(201)
	}
	b, err := os.ReadFile(specFile)
	if err != nil {
		fail("%v", err)
	}
	var spec Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		fail("%v", err)
	}
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fail("make mounts private: %v", err)
	}
	for i, root := range spec.Roots {
		opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", root, spec.Upper(i), spec.Work(i))
		if spec.UserNS {
			opts += ",userxattr"
		}
		if err := unix.Mount("overlay", root, "overlay", 0, opts); err != nil {
			fail("overlay on %s: %v", root, err)
		}
	}
	if err := os.Chdir(spec.Cwd); err != nil {
		os.Chdir("/")
	}
	path, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoor try: %v\n", err)
		os.Exit(127)
	}
	err = syscall.Exec(path, spec.Argv, os.Environ())
	fmt.Fprintf(os.Stderr, "spoor try: exec %s: %v\n", path, err)
	os.Exit(126)
}

func opaque(p string, userns bool) bool {
	buf := make([]byte, 8)
	for _, name := range []string{"user.overlay.opaque", "trusted.overlay.opaque"} {
		if n, err := unix.Lgetxattr(p, name, buf); err == nil && n > 0 && buf[0] == 'y' {
			return true
		}
	}
	return false
}

// Changes reads the upper layers and reports what the command changed.
func Changes(spec Spec) ([]Change, error) {
	var out []Change
	for i, root := range spec.Roots {
		upper := spec.Upper(i)
		err := filepath.WalkDir(upper, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if p == upper {
				return nil
			}
			rel, _ := filepath.Rel(upper, p)
			real := filepath.Join(root, rel)
			info, err := os.Lstat(p)
			if err != nil {
				return nil
			}
			_, lerr := os.Lstat(real)
			lowerExists := lerr == nil
			if info.Mode()&fs.ModeCharDevice != 0 {
				if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Rdev == 0 {
					out = append(out, Change{Path: real, Kind: Removed})
					return nil
				}
			}
			if d.IsDir() {
				if opaque(p, spec.UserNS) && lowerExists {
					out = append(out, Change{Path: real, Upper: p, Kind: Opaque, IsDir: true})
				} else if !lowerExists {
					out = append(out, Change{Path: real, Upper: p, Kind: Added, IsDir: true})
				}
				return nil
			}
			k := Added
			if lowerExists {
				k = Modified
			}
			out = append(out, Change{Path: real, Upper: p, Kind: k})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// OpaqueRemovals lists real children of an opaque dir that the upper layer
// no longer has.
func OpaqueRemovals(c Change) []string {
	var gone []string
	ents, err := os.ReadDir(c.Path)
	if err != nil {
		return nil
	}
	for _, e := range ents {
		if _, err := os.Lstat(filepath.Join(c.Upper, e.Name())); err != nil {
			gone = append(gone, filepath.Join(c.Path, e.Name()))
		}
	}
	return gone
}

// Apply copies the upper layers onto the real roots.
func Apply(spec Spec, changes []Change) error {
	sort.SliceStable(changes, func(i, j int) bool {
		ri, rj := rank(changes[i]), rank(changes[j])
		if ri != rj {
			return ri < rj
		}
		di, dj := strings.Count(changes[i].Path, "/"), strings.Count(changes[j].Path, "/")
		if changes[i].Kind == Removed {
			return di > dj
		}
		return di < dj
	})
	for _, c := range changes {
		switch c.Kind {
		case Removed:
			if err := os.RemoveAll(c.Path); err != nil {
				return err
			}
		case Opaque:
			for _, g := range OpaqueRemovals(c) {
				if err := os.RemoveAll(g); err != nil {
					return err
				}
			}
		case Added, Modified:
			if c.IsDir {
				fi, err := os.Lstat(c.Upper)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(c.Path, fi.Mode().Perm()); err != nil {
					return err
				}
				continue
			}
			if err := copyEntry(c.Upper, c.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

func rank(c Change) int {
	switch c.Kind {
	case Removed, Opaque:
		return 0
	}
	if c.IsDir {
		return 1
	}
	return 2
}

func copyEntry(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		t, err := os.Readlink(src)
		if err != nil {
			return err
		}
		os.Remove(dst)
		return os.Symlink(t, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".spoor-try-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	os.Chmod(tmp, fi.Mode().Perm())
	return os.Rename(tmp, dst)
}
