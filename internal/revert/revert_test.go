package revert

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/store"
)

func TestScriptCommentsCannotRunCommands(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	acts := []Action{
		{Op: Conflict, Path: "/h/x\nprintf INJECTED1\n#", Reason: "edited\nprintf INJECTED2"},
		{Op: Unrestorable, Path: "/h/y", Reason: "big\r\nprintf INJECTED3"},
	}
	script := Script(st, acts, "revert abc: innocent\nprintf INJECTED4\n")
	out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "INJECTED") {
		t.Fatalf("a recorded path or message executed as a command:\n%s\n---\n%s", script, out)
	}
}

func setupAdded(t *testing.T) (*store.Store, string, model.Change) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	st, _ := store.Open(filepath.Join(t.TempDir(), "repo"))
	p := filepath.Join(root, "app", "x")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("installed\n"), 0o644)
	h, _ := store.HashFile(p)
	return st, root, model.Change{Path: p, Kind: model.Added, After: &model.Entry{Path: p, Type: model.File, Hash: h, Mode: 0o644}}
}

func TestApplyRefusesSymlinkedParent(t *testing.T) {
	st, root, c := setupAdded(t)
	opt := Options{Paths: map[string]bool{c.Path: true}, Roots: []model.Root{{Path: root, Depth: -1}}, GOOS: runtime.GOOS}
	plan := Plan(st, []model.Change{c}, opt)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	os.Rename(filepath.Join(root, "app"), filepath.Join(outside, "app"))
	os.Symlink(filepath.Join(outside, "app"), filepath.Join(root, "app"))
	res := Apply(st, plan)
	if len(res) != 1 || res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "symlink") {
		t.Fatalf("expected a refusal, got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(outside, "app", "x")); err != nil {
		t.Fatal("file outside the watched root was deleted")
	}
}

func TestApplyRefusesPathChangedAfterPlan(t *testing.T) {
	st, root, c := setupAdded(t)
	plan := Plan(st, []model.Change{c}, Options{Paths: map[string]bool{c.Path: true}, Roots: []model.Root{{Path: root, Depth: -1}}, GOOS: runtime.GOOS})
	if len(plan) != 1 || plan[0].Op != Delete {
		t.Fatalf("plan: %+v", plan)
	}
	os.WriteFile(c.Path, []byte("user edited this in the meantime\n"), 0o644)
	res := Apply(st, plan)
	if res[0].Err == nil {
		t.Fatal("apply deleted a file edited after the plan was made")
	}
	if b, _ := os.ReadFile(c.Path); !strings.Contains(string(b), "meantime") {
		t.Fatal("edited file lost")
	}
}

func TestTreeWithLaterFileIsConflictEvenWithOldMtime(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	st, _ := store.Open(filepath.Join(t.TempDir(), "repo"))
	dir := filepath.Join(root, "tool")
	os.MkdirAll(filepath.Join(dir, "lib"), 0o755)
	os.WriteFile(filepath.Join(dir, "lib", "a"), []byte("a"), 0o644)
	time.Sleep(20 * time.Millisecond)
	since := time.Now()
	time.Sleep(20 * time.Millisecond)
	c := model.Change{Path: dir, Kind: model.Added, After: &model.Entry{Path: dir, Type: model.Dir}}
	opt := Options{Paths: map[string]bool{dir: true}, Since: since, Roots: []model.Root{{Path: root, Depth: 1}}, GOOS: runtime.GOOS, Home: "/nonexistent"}
	if plan := Plan(st, []model.Change{c}, opt); plan[0].Op != DeleteTree {
		t.Fatalf("untouched tree should be deletable: %+v", plan)
	}
	later := filepath.Join(dir, "lib", "precious")
	os.WriteFile(later, []byte("user data"), 0o644)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	os.Chtimes(later, old, old) // what cp -p would leave
	if plan := Plan(st, []model.Change{c}, opt); plan[0].Op != Conflict {
		t.Fatalf("a file copied in after the commit must block tree removal: %+v", plan)
	}
}

func TestModeKeepsSpecialBits(t *testing.T) {
	m := uint32(0o755) | uint32(fs.ModeSetuid)
	if fileMode(m) != 0o755|fs.ModeSetuid || octal(m) != "4755" {
		t.Errorf("fileMode=%v octal=%s", fileMode(m), octal(m))
	}
}
