package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPicker(t *testing.T) {
	p := NewPicker("upgrade", []PickItem{
		{Name: "fzf", From: "0.65.1", To: "0.74.3", Files: 19, Bytes: 300 << 10},
		{Name: "jq", From: "1.8.1", To: "1.8.2", Files: 19, Bytes: 1 << 20},
		{Name: "pinnedpkg", Disabled: "pinned"},
		{Name: "tree", From: "2.2.1", To: "2.3.2", Files: 9, Bytes: 200 << 10},
	})
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	k := func(s string) { p.Update(key(s)) }

	k(" ") // fzf, cursor moves to jq
	k("j") // pinnedpkg
	k(" ") // disabled: stays unticked, cursor to tree
	k(" ") // tree
	if got := strings.Join(p.Selected(), ","); got != "fzf,tree" {
		t.Fatalf("selected %q", got)
	}
	v := p.View()
	for _, want := range []string{"[x] fzf", "[ ] jq", "[-] pinnedpkg", "[x] tree", "2 of 4 selected", "0.5 MB"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}

	k("n")
	k("/")
	p.Update(key("j"))
	k("enter")
	k("a") // only the filtered row
	if got := strings.Join(p.Selected(), ","); got != "jq" {
		t.Fatalf("a within filter %q", got)
	}
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !p.done || p.cancelled {
		t.Error("enter should finish")
	}

	c := NewPicker("x", []PickItem{{Name: "a"}})
	c.Update(key("esc"))
	if !c.cancelled {
		t.Error("esc should cancel")
	}
}
