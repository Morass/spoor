package app

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/morass/spoor/internal/diff"
	"github.com/morass/spoor/internal/kb"
	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/redact"
	"github.com/morass/spoor/internal/revert"
)

// Counts summarises a change list.
type Counts struct {
	Added, Removed, Modified, Other, Warn, Noise int
}

func (a *App) Count(items []Item) Counts {
	var c Counts
	for _, it := range items {
		if it.Rule.Category == kb.Noise {
			c.Noise++
			continue
		}
		switch it.Kind {
		case model.Added:
			c.Added++
		case model.Removed:
			c.Removed++
		case model.Modified:
			c.Modified++
		default:
			c.Other++
		}
		if it.Rule.Risk == kb.Warn {
			c.Warn++
		}
	}
	return c
}

func (c Counts) String() string {
	s := fmt.Sprintf("+%d ~%d -%d", c.Added, c.Modified, c.Removed)
	if c.Other > 0 {
		s += fmt.Sprintf(" m%d", c.Other)
	}
	if c.Warn > 0 {
		s += fmt.Sprintf(" ⚠%d", c.Warn)
	}
	return s
}

// PrintChanges writes a grouped, human summary of changes.
func (a *App) PrintChanges(w io.Writer, changes []model.Change, showNoise bool, writers map[string][]model.Writer) {
	items := a.Items(changes)
	if len(items) == 0 {
		fmt.Fprintln(w, "  no changes")
		return
	}
	var cat kb.Category = "-"
	hidden := 0
	for _, it := range items {
		if it.Rule.Category == kb.Noise && !showNoise {
			hidden++
			continue
		}
		if it.Rule.Category != cat {
			cat = it.Rule.Category
			fmt.Fprintf(w, "\n  %s\n", strings.ToUpper(string(cat)))
		}
		p := a.Tilde(it.Path)
		if it.OldPath != "" {
			p = a.Tilde(it.OldPath) + " → " + p
		}
		fmt.Fprintf(w, "  %s %s %-50s %s\n", it.Rule.Risk.Icon(), it.Kind.Symbol(), p, it.Rule.Title)
		if it.Rule.Risk == kb.Warn {
			for i, d := range kb.Analyze(a.St, it.Change, true) {
				if i == 4 {
					break
				}
				fmt.Fprintf(w, "        %s: %s\n", d.Key, d.Value)
			}
		}
		for _, wr := range writers[it.Path] {
			fmt.Fprintf(w, "        written by pid %d %s (%s)\n", wr.Pid, a.Tilde(wr.Exe), wr.Op)
			break
		}
	}
	if hidden > 0 {
		fmt.Fprintf(w, "\n  (%d noise changes hidden — caches, logs, history; --all shows them)\n", hidden)
	}
}

func (a *App) CommitLine(c *model.Commit) string {
	items := []Item{}
	if _, _, ch, err := a.CommitChanges(c); err == nil {
		items = a.Items(ch)
	}
	cnt := a.Count(items)
	counts := cnt.String()
	if cnt.Added+cnt.Removed+cnt.Modified+cnt.Other == 0 && cnt.Noise > 0 {
		counts = fmt.Sprintf("(%d noise)", cnt.Noise)
	}
	msg := c.Message
	if c.Kind == model.KindRun && c.ExitCode != 0 {
		msg += fmt.Sprintf("  (exit %d)", c.ExitCode)
	}
	kind := string(c.Kind)
	if c.Kind == model.KindTry {
		kind = "try:" + c.TryState
	}
	return fmt.Sprintf("%s  %s  %-12s %-18s %s", c.ID, c.Time.Local().Format("2006-01-02 15:04"), kind, counts, msg)
}

// Quickfix renders changes as a vim quickfix list (`vim -q file`).
func (a *App) Quickfix(c *model.Commit, changes []model.Change) string {
	notes := a.St.Notes(c.ID)
	var sb strings.Builder
	for _, it := range a.Items(changes) {
		if it.Rule.Category == kb.Noise || strings.HasPrefix(it.Path, model.StatePrefix) || isDir(it.Change) {
			continue
		}
		line := 1
		if it.Kind == model.Modified {
			line = firstChangedLine(a, it.Change)
		}
		text := fmt.Sprintf("[%s %s] %s", it.Kind, it.Rule.Category, it.Rule.Title)
		if n := notes[it.Path]; n != "" {
			text += " — " + strings.ReplaceAll(n, "\n", " ")
		}
		fmt.Fprintf(&sb, "%s:%d:1: %s\n", it.Path, line, text)
	}
	return sb.String()
}

func isDir(c model.Change) bool {
	e := c.After
	if e == nil {
		e = c.Before
	}
	return e != nil && e.Type == model.Dir
}

// firstChangedLine finds the first added (or, failing that, the line after
// the first removed) line in the new version, for jumping there in vim.
func firstChangedLine(a *App, c model.Change) int {
	n := 0
	for _, l := range strings.Split(diff.Unified(a.St, c, true), "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			f := strings.Fields(l)
			if len(f) >= 3 {
				start := strings.SplitN(strings.TrimPrefix(f[2], "+"), ",", 2)[0]
				fmt.Sscanf(start, "%d", &n)
			}
		case n == 0 || strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			return n
		case strings.HasPrefix(l, "-"):
			return max(n, 1)
		default:
			n++
		}
	}
	return 1
}

type Footprint struct {
	Tool     string            `json:"tool"`
	Command  string            `json:"command,omitempty"`
	Message  string            `json:"message,omitempty"`
	Time     time.Time         `json:"time"`
	OS       string            `json:"os"`
	ExitCode int               `json:"exit_code"`
	Summary  string            `json:"summary"`
	Changes  []FootprintChange `json:"changes"`
	Redacted int               `json:"secrets_redacted"`
}

type FootprintChange struct {
	Path     string   `json:"path"`
	Kind     string   `json:"kind"`
	Category string   `json:"category"`
	Title    string   `json:"title"`
	Details  []string `json:"details,omitempty"`
	Diff     string   `json:"diff,omitempty"`
}

// Export builds a shareable footprint of a commit: noise dropped, paths
// generalised, secrets redacted, diffs only for small text changes in
// categories where the content is the point.
func (a *App) Export(c *model.Commit, changes []model.Change, doRedact bool) *Footprint {
	fp := &Footprint{Tool: "spoor", Message: c.Message, Time: c.Time, ExitCode: c.ExitCode}
	fp.OS = a.GOOS
	clean := func(s string) string {
		if !doRedact {
			return s
		}
		out, n := redact.Text(s)
		fp.Redacted += n
		return out
	}
	fp.Command = clean(strings.Join(c.Command, " "))
	fp.Message = clean(fp.Message)
	items := a.Items(changes)
	fp.Summary = a.Count(items).String()
	for _, it := range items {
		if it.Rule.Category == kb.Noise {
			continue
		}
		fc := FootprintChange{Path: clean(it.Path), Kind: string(it.Kind), Category: string(it.Rule.Category), Title: it.Rule.Title}
		for _, d := range kb.Analyze(a.St, it.Change, false) {
			fc.Details = append(fc.Details, clean(d.Key+": "+d.Value))
		}
		switch it.Rule.Category {
		case kb.Persistence, kb.Trust, kb.Shell, kb.Path, kb.Config:
			if it.Rule.Category == kb.Path && it.Kind == model.Added {
				break
			}
			if u := diff.Unified(a.St, it.Change, false); u != "" {
				ls := strings.Split(u, "\n")
				if len(ls) > 120 {
					ls = append(ls[:120], fmt.Sprintf("… %d more lines", len(ls)-120))
				}
				fc.Diff = clean(strings.Join(ls, "\n"))
			}
		}
		fp.Changes = append(fp.Changes, fc)
	}
	return fp
}

func (fp *Footprint) JSON() string {
	b, _ := json.MarshalIndent(fp, "", "  ")
	return string(b) + "\n"
}

func (fp *Footprint) Markdown() string {
	var sb strings.Builder
	title := fp.Command
	if title == "" {
		title = fp.Message
	}
	fmt.Fprintf(&sb, "# Footprint: `%s`\n\n", title)
	fmt.Fprintf(&sb, "- recorded %s on %s, exit code %d\n- changes: %s\n", fp.Time.Format("2006-01-02"), fp.OS, fp.ExitCode, fp.Summary)
	if fp.Redacted > 0 {
		fmt.Fprintf(&sb, "- %d secret-looking values redacted\n", fp.Redacted)
	}
	cat := ""
	for _, c := range fp.Changes {
		if c.Category != cat {
			cat = c.Category
			fmt.Fprintf(&sb, "\n## %s\n\n", strings.Title(cat))
		}
		fmt.Fprintf(&sb, "- **%s** `%s` — %s\n", c.Kind, c.Path, c.Title)
		for _, d := range c.Details {
			fmt.Fprintf(&sb, "  - %s\n", d)
		}
		if c.Diff != "" {
			fmt.Fprintf(&sb, "\n  ```diff\n")
			for _, l := range strings.Split(strings.TrimRight(c.Diff, "\n"), "\n") {
				sb.WriteString("  " + l + "\n")
			}
			sb.WriteString("  ```\n")
		}
	}
	return sb.String()
}

func (a *App) PrintPlan(w io.Writer, plan []revert.Action) {
	if len(plan) == 0 {
		fmt.Fprintln(w, "  nothing to undo")
		return
	}
	for _, act := range plan {
		target := a.Tilde(act.Path)
		if act.Label != "" {
			target = act.Label + "  (" + target + ")"
		}
		line := fmt.Sprintf("  %-12s %s", act.Op, target)
		if act.Reason != "" {
			line += "  — " + act.Reason
		}
		fmt.Fprintln(w, line)
	}
}
