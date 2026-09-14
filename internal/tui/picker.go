package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PickItem is one row of the checklist.
type PickItem struct {
	Name     string
	From, To string
	Files    int
	Bytes    int64
	Disabled string // why it cannot be picked (pinned); empty if selectable
}

// Picker is a checklist: tick some rows, press enter.
type Picker struct {
	Title     string
	Items     []PickItem
	selected  map[int]bool
	cur, top  int
	w, h      int
	filtering bool
	filter    textinput.Model
	visible   []int
	done      bool
	cancelled bool
}

func NewPicker(title string, items []PickItem) *Picker {
	p := &Picker{Title: title, Items: items, selected: map[int]bool{}, w: 100, h: 30}
	p.filter = textinput.New()
	p.filter.Prompt = "/"
	p.refilter()
	return p
}

// Pick runs the checklist full-screen and returns the chosen names;
// ok is false when the user cancelled.
func Pick(title string, items []PickItem) ([]string, bool, error) {
	p := NewPicker(title, items)
	if _, err := tea.NewProgram(p, tea.WithAltScreen()).Run(); err != nil {
		return nil, false, err
	}
	return p.Selected(), !p.cancelled, nil
}

func (p *Picker) Selected() []string {
	var out []string
	for i, it := range p.Items {
		if p.selected[i] {
			out = append(out, it.Name)
		}
	}
	return out
}

func (p *Picker) refilter() {
	f := strings.ToLower(p.filter.Value())
	p.visible = p.visible[:0]
	for i, it := range p.Items {
		if f == "" || strings.Contains(strings.ToLower(it.Name), f) {
			p.visible = append(p.visible, i)
		}
	}
	if p.cur >= len(p.visible) {
		p.cur = max(0, len(p.visible)-1)
	}
}

func (p *Picker) Init() tea.Cmd { return nil }

func (p *Picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.w, p.h = msg.Width, msg.Height
	case tea.KeyMsg:
		return p.key(msg)
	}
	return p, nil
}

func (p *Picker) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if s == "ctrl+c" {
		p.cancelled = true
		return p, tea.Quit
	}
	if p.filtering {
		switch s {
		case "enter", "esc":
			p.filtering = false
			p.filter.Blur()
			if s == "esc" {
				p.filter.SetValue("")
				p.refilter()
			}
			return p, nil
		}
		var cmd tea.Cmd
		p.filter, cmd = p.filter.Update(k)
		p.refilter()
		return p, cmd
	}
	if k.Type == tea.KeyRunes && len(k.Runes) > 1 {
		var cmds []tea.Cmd
		for _, r := range k.Runes {
			_, c := p.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			cmds = append(cmds, c)
		}
		return p, tea.Batch(cmds...)
	}
	switch s {
	case "q", "esc":
		p.cancelled = true
		return p, tea.Quit
	case "enter":
		p.done = true
		return p, tea.Quit
	case "j", "down":
		if p.cur < len(p.visible)-1 {
			p.cur++
		}
	case "k", "up":
		if p.cur > 0 {
			p.cur--
		}
	case "g", "home":
		p.cur = 0
	case "G", "end":
		p.cur = max(0, len(p.visible)-1)
	case " ", "x":
		if len(p.visible) > 0 {
			i := p.visible[p.cur]
			if p.Items[i].Disabled == "" {
				p.selected[i] = !p.selected[i]
				if !p.selected[i] {
					delete(p.selected, i)
				}
			}
			if p.cur < len(p.visible)-1 {
				p.cur++
			}
		}
	case "a":
		for _, i := range p.visible {
			if p.Items[i].Disabled == "" {
				p.selected[i] = true
			}
		}
	case "n":
		for _, i := range p.visible {
			delete(p.selected, i)
		}
	case "/":
		p.filtering = true
		return p, p.filter.Focus()
	}
	return p, nil
}

func mb(b int64) string { return fmt.Sprintf("%.1f MB", float64(b)/(1<<20)) }

func (p *Picker) View() string {
	nameW, fromW := 4, 4
	for _, it := range p.Items {
		nameW = max(nameW, len(it.Name))
		fromW = max(fromW, len(it.From))
	}
	nameW, fromW = min(nameW, 32), min(fromW, 16)
	head := lipgloss.NewStyle().Bold(true).Foreground(accent).Render("spoor") + " " + p.Title
	keys := dim.Render("space tick · a all · n none · / filter · ⏎ continue · esc cancel")
	listH := max(p.h-5, 3)
	if p.cur < p.top {
		p.top = p.cur
	}
	if p.cur >= p.top+listH {
		p.top = p.cur - listH + 1
	}
	var lines []string
	for row := p.top; row < len(p.visible) && row < p.top+listH; row++ {
		i := p.visible[row]
		it := p.Items[i]
		box := "[ ]"
		if p.selected[i] {
			box = "[x]"
		}
		detail := fmt.Sprintf("%-*s → %-12s %4d files  %8s", fromW, trunc(it.From, fromW), trunc(it.To, 12), it.Files, mb(it.Bytes))
		if it.Disabled != "" {
			box, detail = "[-]", it.Disabled
		}
		line := fmt.Sprintf(" %s %-*s  %s", box, nameW, trunc(it.Name, nameW), detail)
		line = trunc(line, p.w)
		switch {
		case row == p.cur:
			line = sel.Render(line)
		case it.Disabled != "":
			line = dim.Render(line)
		case p.selected[i]:
			line = lipgloss.NewStyle().Foreground(addC).Render(line)
		}
		lines = append(lines, line)
	}
	if len(p.visible) == 0 {
		lines = append(lines, dim.Render("  nothing matches"))
	}
	for len(lines) < listH {
		lines = append(lines, "")
	}
	var total int64
	for i := range p.selected {
		total += p.Items[i].Bytes
	}
	status := fmt.Sprintf("%d of %d selected · %s of current files kept for diff and undo", len(p.selected), len(p.Items), mb(total))
	if p.filtering || p.filter.Value() != "" {
		status = p.filter.View() + "   " + status
	}
	return strings.Join([]string{head, keys, strings.Join(lines, "\n"), "", trunc(status, p.w)}, "\n")
}
