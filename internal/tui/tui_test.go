package tui

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/morass/spoor/internal/app"
	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/store"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(t *testing.T, m *Model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		_, cmd := m.Update(key(k))
		_ = cmd
	}
}

func setup(t *testing.T) (*app.App, string, *model.Commit) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(root, "home")
	os.MkdirAll(filepath.Join(home, ".config"), 0o755)
	t.Setenv("HOME", home)
	t.Setenv("SPOOR_NO_STATE", "1")
	os.WriteFile(filepath.Join(home, ".zshrc"), []byte("alias ll='ls -l'\n"), 0o644)
	os.WriteFile(filepath.Join(home, "notes.txt"), []byte("n\n"), 0o644)
	a, err := app.Open(filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	a.Out, a.Err = io.Discard, io.Discard
	roots := []model.Root{{Path: home, Depth: 1, Content: true}, {Path: filepath.Join(home, ".config"), Depth: 2, Content: true}}
	a.St.SaveConfig(&store.Config{Roots: roots, NoState: true})
	if _, err := a.Snap(roots, false, "base"); err != nil {
		t.Fatal(err)
	}
	c, _, err := a.Run(app.RunOptions{Roots: roots, Argv: []string{"sh", "-c",
		`printf 'export PATH="$HOME/.x/bin:$PATH"\n' >> "$HOME/.zshrc"; echo 'k = 1' > "$HOME/.config/app.toml"; rm "$HOME/notes.txt"; mkdir -p "$HOME/Library/Caches"; echo c > "$HOME/.DS_Store"`}})
	if err != nil {
		t.Fatal(err)
	}
	return a, home, c
}

func TestReviewNavigateNoteRevert(t *testing.T) {
	a, home, c := setup(t)
	m, err := New(a, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 40})
	v := m.View()
	for _, want := range []string{"SHELL (1)", "~/.zshrc", "+export PATH", "zsh startup file", "~/.config/app.toml", "noise hidden"} {
		if !strings.Contains(v, want) {
			t.Errorf("initial view missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, ".DS_Store") {
		t.Error("noise shown by default")
	}
	press(t, m, ".")
	if !strings.Contains(m.View(), ".DS_Store") {
		t.Error("noise toggle")
	}
	press(t, m, ".")

	press(t, m, "j")
	if it := m.current(); it == nil || it.Path != filepath.Join(home, ".config/app.toml") {
		t.Fatalf("j should select app.toml, got %+v", it)
	}
	if !strings.Contains(m.View(), "Configuration file") {
		t.Error("notes pane did not follow selection")
	}

	press(t, m, "n", "remember to check this", "ctrl+s")
	if got := a.St.Notes(c.ID)[filepath.Join(home, ".config/app.toml")]; got != "remember to check this" {
		t.Errorf("note not saved: %q", got)
	}
	if !strings.Contains(m.View(), "remember to check this") {
		t.Error("saved note not rendered")
	}

	press(t, m, "/", "zsh")
	if len(m.rows) != 2 || m.current() == nil || filepath.Base(m.current().Path) != ".zshrc" {
		t.Errorf("filter: rows=%d\n%s", len(m.rows), m.View())
	}
	press(t, m, "esc")
	press(t, m, "g", "j") // back on app.toml

	press(t, m, "x")
	if !strings.Contains(m.View(), "1 marked") {
		t.Error("mark not shown in header")
	}
	press(t, m, "R")
	if v := m.View(); !strings.Contains(v, "Apply? [y/N]") || !strings.Contains(v, "delete") {
		t.Fatalf("confirm view:\n%s", v)
	}
	_, cmd := m.Update(key("y"))
	if cmd == nil {
		t.Fatal("y returned no command")
	}
	m.Update(cmd())
	if !strings.Contains(m.status, "recorded as commit") {
		t.Fatalf("status after revert: %q", m.status)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/app.toml")); !os.IsNotExist(err) {
		t.Error("marked file not reverted on disk")
	}
	if _, err := os.Stat(filepath.Join(home, "notes.txt")); !os.IsNotExist(err) {
		t.Error("unmarked change was reverted too")
	}

	press(t, m, "?")
	if !strings.Contains(m.View(), "REVIEW") {
		t.Error("help")
	}
	press(t, m, "q") // closes help, does not quit
	press(t, m, "esc")
	if v := m.View(); m.screen != logScreen || !strings.Contains(v, c.ID) || !strings.Contains(v, "revert") {
		t.Errorf("log view:\n%s", v)
	}
}

func TestBurstOfKeysIsSeveralCommands(t *testing.T) {
	a, _, c := setup(t)
	m, err := New(a, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 40})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("jxR")}) // one read from a fast terminal
	if len(m.marks) != 1 || m.confirm == nil {
		t.Fatalf("burst not split: marks=%v confirm=%v status=%q", m.marks, m.confirm != nil, m.status)
	}
}

func TestLiveChangesScreen(t *testing.T) {
	a, home, _ := setup(t)
	os.WriteFile(filepath.Join(home, ".config/new.conf"), []byte("fresh\n"), 0o644)
	m, err := New(a, "now")
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 30}) // narrow layout
	v := m.View()
	if !strings.Contains(v, "~/.config/new.conf") || !strings.Contains(v, "+fresh") {
		t.Errorf("live view:\n%s", v)
	}
	press(t, m, "i")
	if !strings.Contains(m.View(), "Configuration file") {
		t.Errorf("narrow notes toggle:\n%s", m.View())
	}
	press(t, m, "n")
	if !strings.Contains(m.status, "notes attach to recorded commits") {
		t.Errorf("notes on live view should explain: %q", m.status)
	}
}

func TestUpgradeComparisonOpensFirst(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(root, "home")
	os.MkdirAll(filepath.Join(home, "pkg/1.0"), 0o755)
	os.WriteFile(filepath.Join(home, "pkg/1.0/NEWS"), []byte("1.0 initial\n"), 0o644)
	os.WriteFile(filepath.Join(home, "pkg/1.0/bin"), []byte("\x00x"), 0o755)
	t.Setenv("HOME", home)
	t.Setenv("SPOOR_NO_STATE", "1")
	a, err := app.Open(filepath.Join(root, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	a.Out, a.Err = io.Discard, io.Discard
	roots := []model.Root{{Path: home, Depth: -1, Content: true}}
	a.Snap(roots, false, "base")
	c, _, err := a.Run(app.RunOptions{Roots: roots, Argv: []string{"sh", "-c",
		`mkdir -p "$HOME/pkg/1.1" && printf '1.1 faster\n1.0 initial\n' > "$HOME/pkg/1.1/NEWS" && printf '\000yy' > "$HOME/pkg/1.1/bin" && rm -rf "$HOME/pkg/1.0"`}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(a, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 40})
	v := m.View()
	for _, want := range []string{"UPGRADE", "▸ pkg 1.0 → 1.1 (2)", "  NEWS", "+1.1 faster"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
	if it := m.current(); it == nil || !it.Virtual || it.Label != "NEWS" {
		t.Fatalf("should open on the NEWS comparison, got %+v", it)
	}
	press(t, m, "x")
	if !strings.Contains(m.status, "compares two version folders") || len(m.marks) != 0 {
		t.Errorf("marking a comparison row: %q %v", m.status, m.marks)
	}
	press(t, m, "]")
	if !strings.Contains(m.status, "no more text diffs") {
		t.Errorf("] with no further text diff: %q", m.status)
	}
}
