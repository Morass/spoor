package revert

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/store"
)

// Plans are made against the live machine and then applied, possibly after
// a confirmation prompt. Between the two, a file can be edited or a parent
// folder swapped for a symlink that points somewhere unrecorded. Every
// file-touching action therefore carries the watched root it belongs to and
// a fingerprint of what the path looked like at planning time; Apply
// refuses when either no longer holds.

func touchesFiles(op Op) bool {
	switch op {
	case Delete, DeleteTree, Rmdir, Mkdir, Restore, Chmod:
		return true
	}
	return false
}

// Guard fills in Root and Expect for file-touching actions.
func Guard(acts []Action, roots []model.Root) []Action {
	for i := range acts {
		if !touchesFiles(acts[i].Op) {
			continue
		}
		acts[i].Root = rootFor(roots, acts[i].Path)
		acts[i].Expect = fingerprint(acts[i].Path)
	}
	return acts
}

func rootFor(roots []model.Root, p string) string {
	best := ""
	for _, r := range roots {
		if (p == r.Path || strings.HasPrefix(p, strings.TrimSuffix(r.Path, "/")+"/")) && len(r.Path) > len(best) {
			best = r.Path
		}
	}
	return best
}

// fingerprint describes a path's current state compactly.
func fingerprint(p string) string {
	fi, err := os.Lstat(p)
	switch {
	case err != nil:
		return "absent"
	case fi.Mode()&fs.ModeSymlink != 0:
		t, _ := os.Readlink(p)
		return "link:" + t
	case fi.IsDir():
		return "dir"
	case fi.Mode().IsRegular():
		h, err := store.HashFile(p)
		if err != nil {
			return "unreadable"
		}
		return "file:" + h
	}
	return "other"
}

// safeParents refuses when any folder between the root and the path is now a
// symlink: the scan never follows symlinks, so a recorded path never ran
// through one.
func safeParents(root, p string) error {
	if root == "" || p == root {
		return nil
	}
	rel, err := filepath.Rel(root, filepath.Dir(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s is outside its watched root %s", p, root)
	}
	if rel == "." {
		return nil
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // missing parents are recreated, not traversed
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing: %s is now a symlink, not the folder that was recorded", cur)
		}
	}
	return nil
}

// fileMode converts a manifest mode (permission bits plus fs.Mode* special
// bits) into an fs.FileMode, keeping setuid, setgid and sticky.
func fileMode(m uint32) fs.FileMode {
	out := fs.FileMode(m & 0o777)
	for _, b := range []fs.FileMode{fs.ModeSetuid, fs.ModeSetgid, fs.ModeSticky} {
		if m&uint32(b) != 0 {
			out |= b
		}
	}
	return out
}

// octal renders the same mode for chmod(1).
func octal(m uint32) string {
	o := m & 0o777
	if m&uint32(fs.ModeSetuid) != 0 {
		o |= 0o4000
	}
	if m&uint32(fs.ModeSetgid) != 0 {
		o |= 0o2000
	}
	if m&uint32(fs.ModeSticky) != 0 {
		o |= 0o1000
	}
	return fmt.Sprintf("%04o", o)
}

// comment makes text safe to place after "# " in a shell script: a newline
// in a recorded path or message must not start a new command.
func comment(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// Matches reports whether the live path is what entry e recorded.
func Matches(p string, e *model.Entry) bool { return matches(p, e) }
