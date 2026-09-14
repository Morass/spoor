// Package tui is spoor's interactive inspector: a history list, and a
// three-pane review of one commit (changes · diff · notes) with hand-off
// to $EDITOR, vimdiff and vim's quickfix list.
package tui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/morass/spoor/internal/app"
	"github.com/morass/spoor/internal/diff"
	"github.com/morass/spoor/internal/kb"
	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/revert"
	"github.com/morass/spoor/internal/store"
)

type screen int

const (
	logScreen screen = iota
	reviewScreen
)

type focus int

const (
	focusList focus = iota
	focusDiff
	focusNotes
)

type row struct {
	header bool
	cat    kb.Category
	n      int
	item   int
}

type confirm struct {
	title string
	body  []string
	yes   tea.Cmd
}

type Model struct {
	app    *app.App
	w, h   int
	screen screen

	commits []*model.Commit
	logCur  int
	logTop  int

	commit      *model.Commit
	synthetic   bool
	live        bool
	postRoots   []model.Root
	postTime    time.Time
	postState   bool
	items       []app.Item
	rows        []row
	cur         int
	top         int
	showNoise   bool
	hiddenNoise int
	filter      string
	marks       map[string]bool
	notes       map[string]string
	writers     map[string][]model.Writer
	focus       focus
	narrowNotes bool

	diffVP    viewport.Model
	notesVP   viewport.Model
	diffCache map[string]string
	detCache  map[string][]kb.Detail

	editing   bool
	note      textarea.Model
	filtering bool
	finput    textinput.Model
	confirm   *confirm
	help      bool
	status    string
	busy      bool
}

type execDoneMsg struct {
	path string
	err  error
	what string
}
type revertDoneMsg struct {
	results []revert.Result
	commit  *model.Commit
	err     error
}
type snapDoneMsg struct {
	commit *model.Commit
	err    error
}
type nowMsg struct {
	head    *model.Commit
	changes []model.Change
	err     error
}

var (
	accent  = lipgloss.Color("39")
	dimC    = lipgloss.Color("244")
	addC    = lipgloss.Color("42")
	delC    = lipgloss.Color("203")
	modC    = lipgloss.Color("221")
	hunkC   = lipgloss.Color("45")
	warnC   = lipgloss.Color("214")
	noticeC = lipgloss.Color("111")
	bold    = lipgloss.NewStyle().Bold(true)
	dim     = lipgloss.NewStyle().Foreground(dimC)
	sel     = lipgloss.NewStyle().Reverse(true)
)

// New builds the model. ref "" opens the history list, "now" the live
// changes since HEAD, anything else that commit.
func New(a *app.App, ref string) (*Model, error) {
	a.Err = io.Discard // scan warnings would corrupt the screen
	m := &Model{app: a, marks: map[string]bool{}, diffCache: map[string]string{}, detCache: map[string][]kb.Detail{}}
	m.diffVP = viewport.New(40, 10)
	m.notesVP = viewport.New(30, 10)
	m.note = textarea.New()
	m.note.Placeholder = "why this change matters, what to do about it…"
	m.note.ShowLineNumbers = false
	m.finput = textinput.New()
	m.finput.Prompt = "/"
	if err := m.loadCommits(); err != nil {
		return nil, err
	}
	switch ref {
	case "":
		m.screen = logScreen
	case "now":
		head, changes, err := a.Status()
		if err != nil {
			return nil, err
		}
		m.openNow(head, changes)
	default:
		c, err := a.St.Resolve(ref)
		if err != nil {
			return nil, err
		}
		if err := m.openCommit(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func Run(a *app.App, ref string) error {
	m, err := New(a, ref)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m *Model) loadCommits() error {
	cs, err := m.app.St.Commits()
	m.commits = cs
	if m.logCur >= len(cs) {
		m.logCur = max(0, len(cs)-1)
	}
	return err
}

func (m *Model) openCommit(c *model.Commit) error {
	_, post, changes, err := m.app.CommitChanges(c)
	if err != nil {
		return err
	}
	m.commit, m.synthetic = c, false
	m.live = c.ID == m.app.St.Head()
	m.postRoots, m.postState, m.postTime = post.Roots, post.State, post.Created
	m.notes = m.app.St.Notes(c.ID)
	m.writers = m.app.St.Trace(c.ID)
	m.setItems(changes)
	return nil
}

func (m *Model) openNow(head *model.Commit, changes []model.Change) {
	m.commit = &model.Commit{ID: "now", Kind: "live", Message: "changes since " + head.ID + " (not recorded yet)", Time: head.Time}
	m.synthetic, m.live, m.postTime = true, true, time.Now()
	if hm, err := m.app.St.LoadManifest(head.Post); err == nil {
		m.postRoots, m.postState = hm.Roots, hm.State
	}
	m.notes, m.writers = map[string]string{}, map[string][]model.Writer{}
	m.setItems(changes)
}

func (m *Model) setItems(changes []model.Change) {
	m.items = m.app.Items(changes)
	m.marks = map[string]bool{}
	m.diffCache, m.detCache = map[string]string{}, map[string][]kb.Detail{}
	m.cur, m.top, m.focus = 0, 0, focusList
	m.screen = reviewScreen
	m.rebuild()
}

func (m *Model) current() *app.Item {
	if m.cur < 0 || m.cur >= len(m.rows) || m.rows[m.cur].header {
		return nil
	}
	return &m.items[m.rows[m.cur].item]
}

func (m *Model) rebuild() {
	keep := ""
	if it := m.current(); it != nil {
		keep = it.Path
	}
	m.rows = nil
	m.hiddenNoise = 0
	var cat kb.Category = "\x00"
	hdr := -1
	f := strings.ToLower(m.filter)
	for i, it := range m.items {
		if it.Rule.Category == kb.Noise && !m.showNoise {
			m.hiddenNoise++
			continue
		}
		if f != "" && !strings.Contains(strings.ToLower(it.Path+" "+it.Rule.Title), f) {
			continue
		}
		if it.Rule.Category != cat {
			cat = it.Rule.Category
			m.rows = append(m.rows, row{header: true, cat: cat})
			hdr = len(m.rows) - 1
		}
		m.rows[hdr].n++
		m.rows = append(m.rows, row{item: i})
	}
	m.cur = -1
	for i, r := range m.rows {
		if r.header {
			continue
		}
		if m.cur < 0 || m.items[r.item].Path == keep {
			m.cur = i
			if m.items[r.item].Path == keep {
				break
			}
		}
	}
	m.refresh()
}

func (m *Model) move(delta int) {
	i := m.cur
	for {
		i += delta
		if i < 0 || i >= len(m.rows) {
			return
		}
		if !m.rows[i].header {
			m.cur = i
			m.refresh()
			return
		}
	}
}

func (m *Model) jump(toEnd bool) {
	if toEnd {
		m.cur = len(m.rows)
		m.move(-1)
	} else {
		m.cur = -1
		m.move(1)
	}
}

// ---- layout ----

func (m *Model) layout() (listW, diffW, notesW, bodyH int) {
	bodyH = max(m.h-3, 5)
	if m.w >= 110 {
		listW = m.w * 30 / 100
		notesW = m.w * 28 / 100
		diffW = m.w - listW - notesW
		return
	}
	listW = m.w * 38 / 100
	diffW = m.w - listW
	return
}

func (m *Model) resize() {
	_, diffW, notesW, bodyH := m.layout()
	m.diffVP.Width, m.diffVP.Height = max(diffW-2, 10), max(bodyH-2, 3)
	nw := notesW
	if nw == 0 {
		nw = diffW
	}
	m.notesVP.Width, m.notesVP.Height = max(nw-2, 10), max(bodyH-2, 3)
	m.note.SetWidth(max(nw-4, 10))
	m.note.SetHeight(6)
	m.refresh()
}

func (m *Model) refresh() {
	it := m.current()
	if it == nil {
		m.diffVP.SetContent(dim.Render("nothing selected"))
		m.notesVP.SetContent("")
		return
	}
	m.diffVP.SetContent(m.renderDiff(it))
	m.diffVP.GotoTop()
	m.notesVP.SetContent(m.renderNotes(it))
	m.notesVP.GotoTop()
}

func trunc(s string, w int) string {
	s = strings.ReplaceAll(s, "\t", "    ")
	r := []rune(s)
	if w <= 0 || len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

func truncLeft(s string, w int) string {
	r := []rune(s)
	if len(r) <= w || w < 2 {
		return s
	}
	return "…" + string(r[len(r)-w+1:])
}

func (m *Model) rawDiff(it *app.Item) string {
	if d, ok := m.diffCache[it.Path]; ok {
		return d
	}
	d := diff.Unified(m.app.St, it.Change, m.live)
	if strings.TrimSpace(d) == "" {
		d = "(no content difference to show)"
	}
	m.diffCache[it.Path] = d
	return d
}

func (m *Model) renderDiff(it *app.Item) string {
	w := m.diffVP.Width
	var sb strings.Builder
	for _, l := range strings.Split(strings.TrimRight(m.rawDiff(it), "\n"), "\n") {
		t := trunc(l, w)
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
			t = bold.Render(t)
		case strings.HasPrefix(l, "+"):
			t = lipgloss.NewStyle().Foreground(addC).Render(t)
		case strings.HasPrefix(l, "-"):
			t = lipgloss.NewStyle().Foreground(delC).Render(t)
		case strings.HasPrefix(l, "@@"):
			t = lipgloss.NewStyle().Foreground(hunkC).Render(t)
		case !strings.HasPrefix(l, " "):
			t = lipgloss.NewStyle().Foreground(modC).Render(t)
		}
		sb.WriteString(t + "\n")
	}
	return sb.String()
}

func riskColor(r kb.Risk) lipgloss.Color {
	switch r {
	case kb.Warn:
		return warnC
	case kb.Notice:
		return noticeC
	}
	return dimC
}

func (m *Model) details(it *app.Item) []kb.Detail {
	if d, ok := m.detCache[it.Path]; ok {
		return d
	}
	d := kb.Analyze(m.app.St, it.Change, m.live)
	m.detCache[it.Path] = d
	return d
}

func (m *Model) renderNotes(it *app.Item) string {
	w := m.notesVP.Width
	wrap := lipgloss.NewStyle().Width(w)
	var parts []string
	parts = append(parts, lipgloss.NewStyle().Bold(true).Foreground(riskColor(it.Rule.Risk)).Width(w).Render(it.Rule.Risk.Icon()+" "+it.Rule.Title))
	parts = append(parts, dim.Width(w).Render(fmt.Sprintf("%s · %s", it.Kind, it.Rule.Category)))
	path := m.app.Tilde(it.Path)
	if it.OldPath != "" {
		path = m.app.Tilde(it.OldPath) + " → " + path
	}
	parts = append(parts, wrap.Render(path), "")
	if it.Rule.Explain != "" {
		parts = append(parts, wrap.Render(it.Rule.Explain), "")
	}
	if ds := m.details(it); len(ds) > 0 {
		for _, d := range ds {
			k := lipgloss.NewStyle().Foreground(riskColor(d.Risk)).Render(d.Key + ":")
			parts = append(parts, wrap.Render(k+" "+d.Value))
		}
		parts = append(parts, "")
	}
	for _, wr := range m.writers[it.Path] {
		parts = append(parts, wrap.Render(fmt.Sprintf("%s pid %d %s", wr.Op, wr.Pid, m.app.Tilde(wr.Exe))))
	}
	if len(m.writers[it.Path]) > 0 {
		parts = append(parts, "")
	}
	parts = append(parts, dim.Render(trunc("── your note "+strings.Repeat("─", w), w)))
	if n := m.notes[it.Path]; n != "" {
		parts = append(parts, wrap.Render(n))
	} else if m.synthetic {
		parts = append(parts, dim.Width(w).Render("(notes attach to recorded commits)"))
	} else {
		parts = append(parts, dim.Render("(n to add)"))
	}
	return strings.Join(parts, "\n")
}

// ---- commands ----

func (m *Model) editorCmd(path string) tea.Cmd {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vi"
	}
	f := strings.Fields(ed)
	c := exec.Command(f[0], append(f[1:], path)...)
	return tea.ExecProcess(c, func(err error) tea.Msg { return execDoneMsg{path: path, err: err, what: "editor"} })
}

func diffTool() []string {
	if t := os.Getenv("SPOOR_DIFFTOOL"); t != "" {
		return strings.Fields(t)
	}
	if _, err := exec.LookPath("vimdiff"); err == nil {
		return []string{"vimdiff"}
	}
	if _, err := exec.LookPath("nvim"); err == nil {
		return []string{"nvim", "-d"}
	}
	return nil
}

func vimTool() string {
	if _, err := exec.LookPath("vim"); err == nil {
		return "vim"
	}
	if _, err := exec.LookPath("nvim"); err == nil {
		return "nvim"
	}
	return ""
}

func (m *Model) tmpFile(name string, body []byte) (string, error) {
	dir := filepath.Join(m.app.St.Root, "tmp", "view-"+store.NewID())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	return p, os.WriteFile(p, body, 0o600)
}

func (m *Model) vimdiffCmd(it *app.Item) (tea.Cmd, string) {
	tool := diffTool()
	if tool == nil {
		return nil, "no vimdiff or nvim found — the built-in diff pane is all there is"
	}
	base := filepath.Base(it.Path)
	b, _ := diff.Content(m.app.St, it.Before, false)
	a, _ := diff.Content(m.app.St, it.After, m.live)
	afterPlist := diff.IsPlist(a)
	bt, bok := diff.Readable(b)
	at, aok := diff.Readable(a)
	if (b != nil && !bok) || (a != nil && !aok) {
		return nil, "binary content: nothing a text diff tool can show"
	}
	b, a = []byte(bt), []byte(at)
	before, err := m.tmpFile(base+".before", b)
	if err != nil {
		return nil, err.Error()
	}
	after := ""
	if m.live && it.After != nil && it.After.Type == model.File && !afterPlist {
		if h, err := store.HashFile(it.Path); err == nil && h == it.After.Hash {
			after = it.Path // edit the real file directly
		}
	}
	if after == "" {
		if after, err = m.tmpFile(base+".after", a); err != nil {
			return nil, err.Error()
		}
	}
	c := exec.Command(tool[0], append(tool[1:], before, after)...)
	return tea.ExecProcess(c, func(err error) tea.Msg { return execDoneMsg{path: it.Path, err: err, what: "diff tool"} }), ""
}

func (m *Model) visibleChanges(onlyMarked bool) []model.Change {
	var out []model.Change
	for _, r := range m.rows {
		if r.header {
			continue
		}
		it := m.items[r.item]
		if onlyMarked && !m.marks[it.Path] {
			continue
		}
		out = append(out, it.Change)
	}
	return out
}

func (m *Model) targetPaths() map[string]bool {
	paths := map[string]bool{}
	for p, v := range m.marks {
		if v {
			paths[p] = true
		}
	}
	if len(paths) == 0 {
		if it := m.current(); it != nil {
			paths[it.Path] = true
		}
	}
	return paths
}

func (m *Model) allChanges() []model.Change {
	out := make([]model.Change, len(m.items))
	for i, it := range m.items {
		out[i] = it.Change
	}
	return out
}

func (m *Model) exportPath(suffix string) string {
	return filepath.Join(m.app.St.Root, "exports", m.commit.ID+suffix)
}

func (m *Model) startRevert() {
	if m.commit.Kind == model.KindTry {
		m.status = "a try never touched the machine — use `spoor try apply` or `spoor try discard`"
		return
	}
	paths := m.targetPaths()
	plan := revert.Plan(m.app.St, m.allChanges(), revert.Options{Paths: paths, Home: m.app.Home, GOOS: m.app.GOOS, Since: m.postTime})
	var body []string
	exec := 0
	for _, a := range plan {
		t := m.app.Tilde(a.Path)
		if a.Label != "" {
			t = a.Label
		}
		line := fmt.Sprintf("%-12s %s", a.Op, t)
		if a.Reason != "" {
			line += " — " + a.Reason
		}
		body = append(body, line)
		if a.Executes() {
			exec++
		}
	}
	if exec == 0 {
		m.status = "nothing to undo: " + strings.Join(body, "; ")
		return
	}
	a := m.app
	title := fmt.Sprintf("revert %d path(s) of %s", len(paths), m.commit.ID)
	roots, state, reverts := m.postRoots, m.postState, m.commit.ID
	if m.synthetic {
		reverts = ""
		title = fmt.Sprintf("restore %d path(s) to HEAD", len(paths))
	}
	m.confirm = &confirm{
		title: title + fmt.Sprintf(" — %d action(s). Apply? [y/N]", exec),
		body:  body,
		yes: func() tea.Msg {
			res, c, err := a.ApplyRecorded(plan, title, reverts, roots, state)
			return revertDoneMsg{results: res, commit: c, err: err}
		},
	}
}

// ---- update ----

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.resize()
		return m, nil
	case execDoneMsg:
		delete(m.diffCache, msg.path)
		delete(m.detCache, msg.path)
		m.status = msg.what + " closed"
		if msg.err != nil {
			m.status = msg.what + ": " + msg.err.Error()
		}
		m.refresh()
		return m, nil
	case revertDoneMsg:
		m.busy = false
		m.loadCommits()
		if msg.err != nil {
			m.status = "revert failed: " + msg.err.Error()
			return m, nil
		}
		errs := 0
		first := ""
		for _, r := range msg.results {
			if r.Err != nil {
				errs++
				if first == "" {
					first = r.Err.Error()
				}
			}
		}
		m.status = fmt.Sprintf("applied %d action(s), recorded as commit %s", len(msg.results)-errs, msg.commit.ID)
		if errs > 0 {
			m.status += fmt.Sprintf(" — %d failed: %s", errs, first)
		}
		m.marks = map[string]bool{}
		m.diffCache, m.detCache = map[string]string{}, map[string][]kb.Detail{}
		m.refresh()
		return m, nil
	case snapDoneMsg:
		m.busy = false
		m.loadCommits()
		switch {
		case msg.err != nil:
			m.status = "snapshot failed: " + msg.err.Error()
		case msg.commit == nil:
			m.status = "nothing changed since HEAD"
		default:
			m.status = "recorded snapshot " + msg.commit.ID
			m.logCur = 0
		}
		return m, nil
	case nowMsg:
		m.busy = false
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		m.openNow(msg.head, msg.changes)
		m.status = fmt.Sprintf("%d live change(s) since HEAD", len(msg.changes))
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if s == "ctrl+c" {
		return m, tea.Quit
	}
	// Keys typed quickly (or sent by a script) can arrive as one message
	// with several runes; outside text fields each rune is a command.
	if k.Type == tea.KeyRunes && len(k.Runes) > 1 && !m.editing && !m.filtering {
		var cmds []tea.Cmd
		for _, r := range k.Runes {
			_, c := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)
	}
	if m.busy {
		return m, nil
	}
	if m.editing {
		switch s {
		case "esc":
			m.editing = false
			m.note.Blur()
			m.focus = focusList
			m.status = "note discarded"
		case "ctrl+s":
			it := m.current()
			m.editing = false
			m.note.Blur()
			m.focus = focusList
			if it != nil {
				val := strings.TrimSpace(m.note.Value())
				if err := m.app.St.SetNote(m.commit.ID, it.Path, val); err != nil {
					m.status = "saving note: " + err.Error()
				} else {
					if val == "" {
						delete(m.notes, it.Path)
					} else {
						m.notes[it.Path] = val
					}
					m.status = "note saved"
				}
				m.refresh()
			}
		default:
			var cmd tea.Cmd
			m.note, cmd = m.note.Update(k)
			return m, cmd
		}
		return m, nil
	}
	if m.filtering {
		switch s {
		case "enter":
			m.filtering = false
			m.finput.Blur()
		case "esc":
			m.filtering = false
			m.finput.Blur()
			m.filter = ""
			m.rebuild()
		default:
			var cmd tea.Cmd
			m.finput, cmd = m.finput.Update(k)
			m.filter = m.finput.Value()
			m.rebuild()
			return m, cmd
		}
		return m, nil
	}
	if m.confirm != nil {
		c := m.confirm
		m.confirm = nil
		if s == "y" || s == "Y" {
			m.busy = true
			m.status = "applying…"
			return m, c.yes
		}
		m.status = "cancelled"
		return m, nil
	}
	if m.help {
		m.help = false
		return m, nil
	}
	if s == "?" {
		m.help = true
		return m, nil
	}
	if m.screen == logScreen {
		return m.logKey(s)
	}
	return m.reviewKey(s)
}

func (m *Model) logKey(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "q", "esc":
		return m, tea.Quit
	case "j", "down":
		if m.logCur < len(m.commits)-1 {
			m.logCur++
		}
	case "k", "up":
		if m.logCur > 0 {
			m.logCur--
		}
	case "g", "home":
		m.logCur = 0
	case "G", "end":
		m.logCur = max(0, len(m.commits)-1)
	case "enter", "l", "right":
		if len(m.commits) == 0 {
			return m, nil
		}
		if err := m.openCommit(m.commits[m.logCur]); err != nil {
			m.status = err.Error()
		} else {
			m.status = ""
		}
	case "n":
		m.busy = true
		m.status = "scanning live state…"
		a := m.app
		return m, func() tea.Msg {
			head, ch, err := a.Status()
			return nowMsg{head, ch, err}
		}
	case "s":
		m.busy = true
		m.status = "recording snapshot…"
		a := m.app
		return m, func() tea.Msg {
			_, headM, _ := a.Head()
			roots, state := a.Roots(nil), a.State(false)
			if headM != nil {
				roots, state = headM.Roots, headM.State
			}
			c, err := a.Snap(roots, state, "snapshot (from review)")
			return snapDoneMsg{c, err}
		}
	case "r":
		m.loadCommits()
		m.status = "reloaded"
	}
	return m, nil
}

func (m *Model) reviewKey(s string) (tea.Model, tea.Cmd) {
	it := m.current()
	switch s {
	case "q":
		return m, tea.Quit
	case "esc", "backspace", "left":
		m.screen = logScreen
		m.loadCommits()
		m.status = ""
		return m, nil
	case "tab":
		if m.w >= 110 {
			m.focus = (m.focus + 1) % 3
		} else if m.focus == focusList {
			m.focus = focusDiff
		} else {
			m.focus = focusList
		}
	case "shift+tab":
		if m.w >= 110 {
			m.focus = (m.focus + 2) % 3
		} else if m.focus == focusList {
			m.focus = focusDiff
		} else {
			m.focus = focusList
		}
	case "i":
		m.narrowNotes = !m.narrowNotes
	case "j", "down":
		switch m.focus {
		case focusList:
			m.move(1)
		case focusDiff:
			m.scrollActive(1)
		case focusNotes:
			m.notesVP.LineDown(1)
		}
	case "k", "up":
		switch m.focus {
		case focusList:
			m.move(-1)
		case focusDiff:
			m.scrollActive(-1)
		case focusNotes:
			m.notesVP.LineUp(1)
		}
	case "J":
		m.scrollActive(1)
	case "K":
		m.scrollActive(-1)
	case "ctrl+d", "pgdown", " ":
		m.diffVP.HalfViewDown()
	case "ctrl+u", "pgup":
		m.diffVP.HalfViewUp()
	case "g", "home":
		m.jump(false)
	case "G", "end":
		m.jump(true)
	case "enter", "l", "right":
		m.focus = focusDiff
	case ".":
		m.showNoise = !m.showNoise
		m.rebuild()
		if m.showNoise {
			m.status = "showing noise"
		} else {
			m.status = "noise hidden"
		}
	case "/":
		m.filtering = true
		m.finput.SetValue(m.filter)
		return m, m.finput.Focus()
	case "x":
		if it != nil {
			m.marks[it.Path] = !m.marks[it.Path]
			if !m.marks[it.Path] {
				delete(m.marks, it.Path)
			}
			m.move(1)
		}
	case "X":
		if it != nil {
			cat := it.Rule.Category
			all := true
			for _, r := range m.rows {
				if !r.header && m.items[r.item].Rule.Category == cat && !m.marks[m.items[r.item].Path] {
					all = false
				}
			}
			for _, r := range m.rows {
				if !r.header && m.items[r.item].Rule.Category == cat {
					if all {
						delete(m.marks, m.items[r.item].Path)
					} else {
						m.marks[m.items[r.item].Path] = true
					}
				}
			}
		}
	case "n":
		if it == nil {
			return m, nil
		}
		if m.synthetic {
			m.status = "notes attach to recorded commits — press s in history (or run `spoor snap`) first"
			return m, nil
		}
		m.editing = true
		m.note.SetValue(m.notes[it.Path])
		m.focus = focusNotes
		m.narrowNotes = true
		return m, m.note.Focus()
	case "e":
		if it == nil {
			return m, nil
		}
		if strings.HasPrefix(it.Path, model.StatePrefix) {
			m.status = "system state has no file to open"
			return m, nil
		}
		if fi, err := os.Stat(it.Path); err != nil || fi.IsDir() {
			m.status = "not a file on disk now (use d to view the recorded versions)"
			return m, nil
		}
		return m, m.editorCmd(it.Path)
	case "d":
		if it == nil {
			return m, nil
		}
		cmd, why := m.vimdiffCmd(it)
		if cmd == nil {
			m.status = why
		}
		return m, cmd
	case "Q", "o":
		qf := m.app.Quickfix(m.commit, m.visibleChanges(false))
		if qf == "" {
			m.status = "no file changes to list"
			return m, nil
		}
		p := m.exportPath(".qf")
		if err := os.WriteFile(p, []byte(qf), 0o600); err != nil {
			m.status = err.Error()
			return m, nil
		}
		if s == "Q" {
			m.status = "quickfix list written: " + m.app.Tilde(p) + "  (vim -q " + m.app.Tilde(p) + ")"
			return m, nil
		}
		v := vimTool()
		if v == "" {
			m.status = "vim not found; quickfix list written to " + m.app.Tilde(p)
			return m, nil
		}
		return m, tea.ExecProcess(exec.Command(v, "-q", p), func(err error) tea.Msg { return execDoneMsg{err: err, what: v} })
	case "u":
		paths := map[string]bool{}
		for p := range m.marks {
			paths[p] = true
		}
		var opt revert.Options
		opt.Home, opt.GOOS, opt.Since = m.app.Home, m.app.GOOS, m.postTime
		if len(paths) > 0 {
			opt.Paths = paths
		}
		plan := revert.Plan(m.app.St, m.allChanges(), opt)
		p := m.exportPath("-undo.sh")
		script := revert.Script(m.app.St, plan, "undo "+m.commit.ID+": "+m.commit.Message)
		if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
			m.status = err.Error()
		} else {
			m.status = fmt.Sprintf("undo script (%d actions) written: %s", len(plan), m.app.Tilde(p))
		}
	case "R":
		if it != nil {
			m.startRevert()
		}
	}
	return m, nil
}

func (m *Model) scrollActive(n int) {
	vp := &m.diffVP
	if m.w < 110 && m.narrowNotes {
		vp = &m.notesVP
	}
	if n > 0 {
		vp.LineDown(n)
	} else {
		vp.LineUp(-n)
	}
}

// ---- view ----

func (m *Model) View() string {
	if m.w == 0 {
		return "loading…"
	}
	var body string
	switch {
	case m.help:
		body = m.helpView()
	case m.confirm != nil:
		body = m.confirmView()
	case m.screen == logScreen:
		body = m.logView()
	default:
		body = m.reviewView()
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.headerView(), body, m.footerView())
}

func (m *Model) headerView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(accent).Render("spoor")
	var rest string
	if m.screen == logScreen || m.commit == nil {
		rest = fmt.Sprintf("history · %d commit(s) · HEAD %s", len(m.commits), m.app.St.Head())
	} else {
		c := m.app.Count(m.items)
		rest = fmt.Sprintf("%s · %s · %s", m.commit.ID, m.commit.Kind, c.String())
		if n := len(m.marks); n > 0 {
			rest += fmt.Sprintf(" · %d marked", n)
		}
		rest += " · " + m.commit.Message
	}
	return trunc(title+" "+rest, m.w+20)
}

func (m *Model) footerView() string {
	status := m.status
	if m.filtering {
		status = m.finput.View()
	} else if m.editing {
		status = "editing note — ctrl+s save · esc discard"
	}
	var keys string
	if m.screen == logScreen {
		keys = "⏎ review · n live changes · s snapshot · r reload · ? help · q quit"
	} else {
		keys = "j/k move · tab pane · e edit · d vimdiff · o quickfix · n note · x mark · R revert · u undo.sh · . noise · / filter · esc back · ?"
	}
	return dim.Render(trunc(status, m.w)) + "\n" + dim.Render(trunc(keys, m.w))
}

func (m *Model) logView() string {
	h := max(m.h-3, 3)
	if len(m.commits) == 0 {
		return lipgloss.NewStyle().Height(h).Render("\n  No history yet. Press s to record a baseline, or run `spoor run -- <install command>`.")
	}
	if m.logCur < m.logTop {
		m.logTop = m.logCur
	}
	if m.logCur >= m.logTop+h {
		m.logTop = m.logCur - h + 1
	}
	var lines []string
	head := m.app.St.Head()
	for i := m.logTop; i < len(m.commits) && i < m.logTop+h; i++ {
		c := m.commits[i]
		mark := "  "
		if c.ID == head {
			mark = "▸ "
		}
		l := trunc(mark+m.app.CommitLine(c), m.w)
		if i == m.logCur {
			l = sel.Render(l)
		}
		lines = append(lines, l)
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m *Model) listView(w, h int) string {
	inner := h - 2
	if m.cur >= 0 {
		if m.cur < m.top {
			m.top = m.cur
			if m.top > 0 && m.rows[m.top-1].header {
				m.top--
			}
		}
		if m.cur >= m.top+inner {
			m.top = m.cur - inner + 1
		}
	}
	var lines []string
	iw := w - 2
	for i := m.top; i < len(m.rows) && len(lines) < inner; i++ {
		r := m.rows[i]
		if r.header {
			lines = append(lines, bold.Render(trunc(fmt.Sprintf("▾ %s (%d)", strings.ToUpper(string(r.cat)), r.n), iw)))
			continue
		}
		it := m.items[r.item]
		mark := " "
		if m.marks[it.Path] {
			mark = "●"
		}
		kc := modC
		switch it.Kind {
		case model.Added:
			kc = addC
		case model.Removed:
			kc = delC
		}
		p := truncLeft(m.app.Tilde(it.Path), iw-6)
		plain := fmt.Sprintf("%s %s %s %s", mark, it.Rule.Risk.Icon(), it.Kind.Symbol(), p)
		var l string
		if i == m.cur {
			if m.focus == focusList {
				l = sel.Render(trunc(plain, iw))
			} else {
				l = lipgloss.NewStyle().Underline(true).Render(trunc(plain, iw))
			}
		} else {
			l = mark + " " + lipgloss.NewStyle().Foreground(riskColor(it.Rule.Risk)).Render(it.Rule.Risk.Icon()) + " " +
				lipgloss.NewStyle().Foreground(kc).Render(it.Kind.Symbol()) + " " + p
		}
		lines = append(lines, l)
	}
	if len(m.rows) == 0 {
		msg := "no changes"
		if m.filter != "" {
			msg = "nothing matches /" + m.filter
		}
		lines = append(lines, dim.Render(msg))
	}
	if m.hiddenNoise > 0 && len(lines) < inner {
		lines = append(lines, dim.Render(trunc(fmt.Sprintf("  %d noise hidden (.)", m.hiddenNoise), iw)))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) box(content string, w, h int, focused bool) string {
	c := dimC
	if focused {
		c = accent
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c).
		Width(w - 2).Height(h - 2).MaxHeight(h).Render(content)
}

func (m *Model) reviewView() string {
	listW, diffW, notesW, bodyH := m.layout()
	list := m.box(m.listView(listW, bodyH), listW, bodyH, m.focus == focusList && !m.editing)
	notesContent := m.notesVP.View()
	if m.editing {
		notesContent = lipgloss.JoinVertical(lipgloss.Left, m.notesVP.View(), m.note.View())
		notesContent = lastLines(notesContent, bodyH-2)
	}
	if notesW == 0 {
		if m.narrowNotes || m.editing {
			return lipgloss.JoinHorizontal(lipgloss.Top, list, m.box(notesContent, diffW, bodyH, m.focus != focusList))
		}
		return lipgloss.JoinHorizontal(lipgloss.Top, list, m.box(m.diffVP.View(), diffW, bodyH, m.focus != focusList))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, list,
		m.box(m.diffVP.View(), diffW, bodyH, m.focus == focusDiff),
		m.box(notesContent, notesW, bodyH, m.focus == focusNotes || m.editing))
}

func lastLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

func (m *Model) confirmView() string {
	h := max(m.h-3, 3)
	lines := []string{bold.Foreground(warnC).Render(trunc(m.confirm.title, m.w-4)), ""}
	for i, b := range m.confirm.body {
		if i >= h-4 {
			lines = append(lines, dim.Render(fmt.Sprintf("… %d more", len(m.confirm.body)-i)))
			break
		}
		lines = append(lines, trunc("  "+b, m.w-4))
	}
	return lipgloss.NewStyle().Height(h).Padding(0, 2).Render(strings.Join(lines, "\n"))
}

func (m *Model) helpView() string {
	h := max(m.h-3, 3)
	text := `REVIEW
  j/k ↑/↓      move in the focused pane          tab / shift+tab  cycle panes
  J/K  space   scroll the diff                   g/G              first/last change
  e            open the live file in $EDITOR     d                vimdiff recorded before vs after
  o / Q        open / write vim quickfix list    n                write a note on this change
  x / X        mark change / whole category      R                revert marked (or current) now
  u            write an undo shell script        .                show/hide noise (caches, logs)
  /            filter by path or title           i                narrow screens: diff ↔ notes
  esc          back to history                   q                quit

HISTORY
  ⏎ review a commit   n review live changes since HEAD   s record a snapshot   r reload

Every revert is itself recorded, so it can be reverted too.
Set SPOOR_DIFFTOOL to use something other than vimdiff (e.g. "nvim -d").

press any key`
	return lipgloss.NewStyle().Height(h).Padding(1, 2).Render(text)
}
