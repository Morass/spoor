// Package e2e drives the real spoor binary against a sandboxed HOME.
package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var bin string

func TestMain(m *testing.M) {
	if b := os.Getenv("SPOOR_BIN"); b != "" {
		bin = b
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "spoor-e2e-bin")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "spoor")
	out, err := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, "../cmd/spoor").CombinedOutput()
	if err != nil {
		fmt.Println(string(out))
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type sandbox struct {
	t          *testing.T
	root, home string
	repo       string
}

func newSandbox(t *testing.T) *sandbox {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	s := &sandbox{t: t, root: root, home: filepath.Join(root, "home"), repo: filepath.Join(root, "repo")}
	os.MkdirAll(s.home, 0o755)
	return s
}

func (s *sandbox) spoor(args ...string) (string, string, int) {
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{
		"HOME=" + s.home, "SPOOR_HOME=" + s.repo, "SPOOR_NO_STATE=1", "TERM=dumb",
		"PATH=" + os.Getenv("PATH"), "SPOOR_TRY_DIR=" + filepath.Join(s.root, "try"),
	}
	cmd.Dir = s.root
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatalf("spoor %v: %v", args, err)
	}
	return o.String(), e.String(), code
}

func (s *sandbox) must(args ...string) string {
	s.t.Helper()
	o, e, code := s.spoor(args...)
	if code != 0 {
		s.t.Fatalf("spoor %s exited %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, o, e)
	}
	return o
}

func (s *sandbox) write(rel, content string, mode os.FileMode) {
	p := filepath.Join(s.home, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		s.t.Fatal(err)
	}
	os.Chmod(p, mode)
}

func (s *sandbox) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(s.home, rel))
	return string(b)
}

func (s *sandbox) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(s.home, rel))
	return err == nil
}

// fingerprint captures every path under home: type, permission bits,
// content hash and link target.
func (s *sandbox) fingerprint() map[string]string {
	out := map[string]string{}
	filepath.WalkDir(s.home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(s.home, p)
		fi, _ := os.Lstat(p)
		v := fmt.Sprintf("%s %o", fi.Mode().Type(), fi.Mode().Perm())
		switch {
		case fi.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			h := sha256.Sum256(b)
			v += " " + hex.EncodeToString(h[:8])
		case fi.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			v += " -> " + l
		}
		out[rel] = v
		return nil
	})
	return out
}

func sameTree(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	var diffs []string
	for k, v := range want {
		if got[k] != v {
			diffs = append(diffs, fmt.Sprintf("  %s: want %q got %q", k, v, got[k]))
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("  %s: unexpected %q", k, v))
		}
	}
	if len(diffs) > 0 {
		t.Fatalf("%s: tree differs:\n%s", what, strings.Join(diffs, "\n"))
	}
}

func headID(t *testing.T, s *sandbox) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.repo, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

const installer = `set -e
mkdir -p "$HOME/Library/LaunchAgents" "$HOME/.local/bin" "$HOME/.config/foo"
cat > "$HOME/Library/LaunchAgents/dev.spoor.e2e.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>dev.spoor.e2e</string>
<key>ProgramArguments</key><array><string>/usr/bin/true</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
</dict></plist>
EOF
printf '\n# added by foo\nexport PATH="$HOME/.foo/bin:$PATH"\nexport FOO_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789\n' >> "$HOME/.zshrc"
printf '#!/bin/sh\necho foo\n' > "$HOME/.local/bin/foo"
chmod 755 "$HOME/.local/bin/foo"
echo 'a = 1' > "$HOME/.config/foo/config.toml"
rm "$HOME/.oldrc"
chmod 600 "$HOME/.gitconfig"
mv "$HOME/.config/keep.txt" "$HOME/.config/moved.txt"
echo "installer done"
exit 3
`

func (s *sandbox) initDefault(extra ...string) {
	args := []string{"init",
		"--root", s.home + ":1",
		"--root", filepath.Join(s.home, ".config") + ":4",
		"--root", filepath.Join(s.home, "Library/LaunchAgents") + ":1",
		"--root", filepath.Join(s.home, ".local/bin") + ":1",
		"--no-state"}
	s.must(append(args, extra...)...)
}

func TestInstallInspectRevertRoundTrip(t *testing.T) {
	s := newSandbox(t)
	s.write(".zshrc", "alias ll='ls -l'\n", 0o644)
	s.write(".oldrc", "old settings\n", 0o644)
	s.write(".gitconfig", "[user]\n\tname = u\n", 0o644)
	s.write(".config/keep.txt", "keep me\n", 0o644)
	s.initDefault()
	s.must("snap", "-m", "baseline")
	base := s.fingerprint()
	baseID := headID(t, s)
	zshBase := s.read(".zshrc")

	script := filepath.Join(s.root, "install.sh")
	os.WriteFile(script, []byte(installer), 0o755)
	out, errOut, code := s.spoor("run", "--", "sh", script)
	if code != 3 {
		t.Fatalf("exit code not passed through: %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "installer done") || !strings.Contains(errOut, "exited 3") {
		t.Fatalf("command output / summary missing:\nstdout %s\nstderr %s", out, errOut)
	}
	runID := headID(t, s)
	post := s.fingerprint()
	zshPost := s.read(".zshrc")

	show := s.must("show", runID)
	for _, want := range []string{"~ ~/.zshrc", "+ ~/.local/bin/foo", "- ~/.oldrc", "m ~/.gitconfig", "~/.config/keep.txt → ~/.config/moved.txt", "+ ~/.config/foo/config.toml"} {
		if !strings.Contains(show, want) {
			t.Errorf("show missing %q:\n%s", want, show)
		}
	}
	if runtime.GOOS == "darwin" {
		for _, want := range []string{"PERSISTENCE", "User LaunchAgent", "KeepAlive", "launchd restarts it"} {
			if !strings.Contains(show, want) {
				t.Errorf("show missing %q:\n%s", want, show)
			}
		}
	}
	patch := s.must("show", runID, "--patch")
	if !strings.Contains(patch, `+export PATH="$HOME/.foo/bin:$PATH"`) || !strings.Contains(patch, `Label = "dev.spoor.e2e"`) {
		t.Errorf("patch missing rc line or decoded plist:\n%s", patch)
	}

	if b := s.must("blame", "~/.local/bin/foo"); !strings.Contains(b, runID) || !strings.Contains(b, "added") {
		t.Errorf("blame:\n%s", b)
	}
	s.must("note", runID, "~/.local/bin/foo", "shim", "from", "foo")
	if n := s.must("note", runID, "~/.local/bin/foo"); strings.TrimSpace(n) != "shim from foo" {
		t.Errorf("note roundtrip: %q", n)
	}
	qf := s.must("quickfix", runID)
	if !strings.Contains(qf, filepath.Join(s.home, ".zshrc")+":2:1: [modified shell]") || !strings.Contains(qf, "— shim from foo") {
		t.Errorf("quickfix:\n%s", qf)
	}
	md := s.must("export", runID)
	if !strings.Contains(md, "[REDACTED]") || strings.Contains(md, "ghp_") || strings.Contains(md, s.home) {
		t.Errorf("export not redacted:\n%s", md)
	}
	var fp map[string]any
	if err := json.Unmarshal([]byte(s.must("export", runID, "--format", "json")), &fp); err != nil || fp["secrets_redacted"].(float64) < 1 {
		t.Errorf("json export: %v %v", err, fp["secrets_redacted"])
	}

	plan := s.must("revert", runID)
	if !strings.Contains(plan, "dry run") || !strings.Contains(plan, "delete") || !strings.Contains(plan, "restore") {
		t.Errorf("plan:\n%s", plan)
	}
	sameTree(t, "dry run must not touch anything", s.fingerprint(), post)

	s.must("revert", runID, "--apply")
	sameTree(t, "after revert", s.fingerprint(), base)
	revertID := headID(t, s)

	s.must("revert", revertID, "--apply")
	sameTree(t, "after reverting the revert", s.fingerprint(), post)

	// A later hand edit must not be clobbered by an unforced revert.
	os.WriteFile(filepath.Join(s.home, ".zshrc"), []byte(zshPost+"echo mine\n"), 0o644)
	plan = s.must("revert", runID)
	if !strings.Contains(plan, "conflict") || !strings.Contains(plan, "edited again since") {
		t.Errorf("conflict not planned:\n%s", plan)
	}
	s.must("revert", runID, "--apply")
	if !strings.HasSuffix(s.read(".zshrc"), "echo mine\n") {
		t.Error("conflicting .zshrc was overwritten without --force")
	}
	if s.exists(".local/bin/foo") || !s.exists(".oldrc") {
		t.Error("non-conflicting changes were not reverted")
	}
	s.must("revert", runID, "--apply", "--force", "--path", "~/.zshrc")
	if s.read(".zshrc") != zshBase {
		t.Errorf("forced path revert: %q", s.read(".zshrc"))
	}

	s.must("restore", "~/.zshrc", "--to", runID, "--apply")
	if s.read(".zshrc") != zshPost {
		t.Error("restore --to run did not bring back the installer's version")
	}
	s.must("restore", "~/.zshrc", "--to", runID, "--before", "--apply")
	if s.read(".zshrc") != zshBase {
		t.Error("restore --before did not bring back the baseline version")
	}

	// Drift: a change made outside spoor shows in status and becomes a drift commit.
	s.write(".config/drift.txt", "outside\n", 0o644)
	if st := s.must("status"); !strings.Contains(st, "~/.config/drift.txt") {
		t.Errorf("status:\n%s", st)
	}
	s.must("run", "-m", "noop", "--", "true")
	if lg := s.must("log"); !strings.Contains(lg, "drift") || !strings.Contains(lg, "noop") {
		t.Errorf("log:\n%s", lg)
	}
	if b := s.must("blame", "~/.config/drift.txt"); !strings.Contains(b, "drift") {
		t.Errorf("blame drift:\n%s", b)
	}
	if d := s.must("diff", baseID, "now"); !strings.Contains(d, "~/.config/drift.txt") {
		t.Errorf("diff base now:\n%s", d)
	}

	s.must("gc")
	if p := s.must("show", runID, "--patch"); !strings.Contains(p, "FOO_TOKEN") {
		t.Error("gc removed content still referenced by history")
	}
	if o, _, _ := s.spoor("snap"); !strings.Contains(o, "nothing changed") {
		t.Errorf("snap without changes: %s", o)
	}
}

func TestMetadataRootsAreHonestAboutUndo(t *testing.T) {
	s := newSandbox(t)
	s.write("tools/tool", "v1\n", 0o755)
	s.must("init", "--root", filepath.Join(s.home, "tools")+":1:meta", "--no-state")
	s.must("snap")
	s.must("run", "--", "sh", "-c", `echo v2 > "$HOME/tools/tool"; echo new > "$HOME/tools/extra"`)
	id := headID(t, s)
	plan := s.must("revert", id)
	if !strings.Contains(plan, "unrestorable") || !strings.Contains(plan, "not captured (metadata)") {
		t.Errorf("modified metadata-only file must be reported unrestorable:\n%s", plan)
	}
	if !strings.Contains(plan, "delete") {
		t.Errorf("added file in a metadata root can still be deleted:\n%s", plan)
	}
}

func TestSecretsNeverCopiedIntoRepository(t *testing.T) {
	s := newSandbox(t)
	s.write(".ssh/id_ed25519", "PRIVATE-KEY-BODY-1\n", 0o600)
	s.write(".ssh/config", "Host x\n", 0o644)
	s.must("init", "--root", filepath.Join(s.home, ".ssh")+":1", "--no-state")
	s.must("snap")
	s.write(".ssh/id_ed25519", "PRIVATE-KEY-BODY-2\n", 0o600)
	if st := s.must("status"); !strings.Contains(st, "~/.ssh/id_ed25519") {
		t.Errorf("change to a sensitive file must still be detected:\n%s", st)
	}
	s.must("snap")
	filepath.WalkDir(s.repo, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			if bytes.Contains(b, []byte("PRIVATE-KEY-BODY")) {
				t.Errorf("secret body copied into %s", p)
			}
		}
		return nil
	})
}

func TestTraceAttributesWriters(t *testing.T) {
	if runtime.GOOS == "darwin" && os.Geteuid() != 0 && exec.Command("sudo", "-n", "true").Run() != nil {
		t.Skip("eslogger needs root; run with passwordless sudo to exercise --trace")
	}
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("strace"); err != nil {
			t.Skip("strace not installed")
		}
	}
	s := newSandbox(t)
	s.must("init", "--root", s.home+":2", "--no-state")
	s.must("snap")
	_, errOut, code := s.spoor("run", "--trace", "--", "sh", "-c", `mkdir -p "$HOME/.foo" && echo hi > "$HOME/.foo/bar"`)
	if code != 0 {
		t.Fatalf("traced run failed: %s", errOut)
	}
	b := s.must("blame", "~/.foo/bar")
	if !strings.Contains(b, "by pid") {
		t.Errorf("no writer attributed:\n%s\n%s", b, errOut)
	}
}

func TestTryPreviewApplyDiscard(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("try needs Linux overlayfs")
	}
	if os.Geteuid() != 0 && os.Getenv("SPOOR_E2E_TRY_USERNS") == "" {
		t.Skip("run as root (or set SPOOR_E2E_TRY_USERNS=1 where unprivileged user namespaces are allowed)")
	}
	s := newSandbox(t)
	s.write(".zshrc", "alias a=b\n", 0o644)
	s.write(".oldrc", "old\n", 0o644)
	s.write("dir/keep", "k\n", 0o644)
	s.must("init", "--root", s.home, "--no-state")
	s.must("snap")
	base := s.fingerprint()

	_, errOut, code := s.spoor("try", "--", "sh", "-c", `echo x >> "$HOME/.zshrc"; echo n > "$HOME/new"; rm "$HOME/.oldrc"; rm -rf "$HOME/dir"; mkdir "$HOME/dir"; echo f > "$HOME/dir/fresh"`)
	if code != 0 {
		t.Fatalf("try failed: %s", errOut)
	}
	sameTree(t, "try must not touch real files", s.fingerprint(), base)
	cs, _ := os.ReadDir(filepath.Join(s.repo, "commits"))
	var tryID string
	for _, c := range cs {
		b, _ := os.ReadFile(filepath.Join(s.repo, "commits", c.Name()))
		if strings.Contains(string(b), `"kind": "try"`) {
			tryID = strings.TrimSuffix(c.Name(), ".json")
		}
	}
	show := s.must("show", tryID, "--patch")
	for _, want := range []string{"~ ~/.zshrc", "+ ~/new", "- ~/.oldrc", "- ~/dir/keep", "+ ~/dir/fresh", "+x"} {
		if !strings.Contains(show, want) {
			t.Errorf("try show missing %q:\n%s", want, show)
		}
	}
	s.must("try", "apply", tryID)
	if s.read(".zshrc") != "alias a=b\nx\n" || s.read("new") != "n\n" || s.exists(".oldrc") || s.exists("dir/keep") || s.read("dir/fresh") != "f\n" {
		t.Errorf("apply result wrong: %v", s.fingerprint())
	}
	if lg := s.must("log"); !strings.Contains(lg, "apply try "+tryID) {
		t.Errorf("log:\n%s", lg)
	}
	after := s.fingerprint()
	_, errOut, code = s.spoor("try", "--", "sh", "-c", `echo junk > "$HOME/junk"`)
	if code != 0 {
		t.Fatalf("second try: %s", errOut)
	}
	cs, _ = os.ReadDir(filepath.Join(s.repo, "commits"))
	for _, c := range cs {
		b, _ := os.ReadFile(filepath.Join(s.repo, "commits", c.Name()))
		if strings.Contains(string(b), `"try_state": "pending"`) {
			s.must("try", "discard", strings.TrimSuffix(c.Name(), ".json"))
		}
	}
	sameTree(t, "discarded try", s.fingerprint(), after)
}
