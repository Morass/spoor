// spoor records what commands do to your machine, lets you inspect and
// annotate every change, and undo any of it — version control for the
// parts of a system that installers touch.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/morass/spoor/internal/app"
	"github.com/morass/spoor/internal/diff"
	"github.com/morass/spoor/internal/kb"
	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/profile"
	"github.com/morass/spoor/internal/revert"
	"github.com/morass/spoor/internal/scan"
	"github.com/morass/spoor/internal/store"
	"github.com/morass/spoor/internal/try"
	"github.com/morass/spoor/internal/tui"
)

var version = "0.1.0-dev"

const usage = `spoor — see what commands do to your machine, inspect it, undo it.

Upgrade (Homebrew)
  spoor upgrade [NAME...] [--list] [--all] [-y] [--no-update] [--no-review]
                              no names: pick from a checklist; capture files, upgrade, open the review
  spoor upgrade NAME@VERSION  install a versioned formula Homebrew ships (e.g. python@3.12)

Record
  spoor run [-m MSG] [--trace] [--add-root SPEC] -- CMD...   record CMD's effects, then offer the review
  spoor snap [-m MSG]                                 commit the current state (if changed)
  spoor try -- CMD...                                 Linux: run CMD on overlays, review first
  spoor try apply|discard REF

Inspect
  spoor review [REF|now]      interactive inspector (history, diff, notes, vim hand-off)
  spoor status                live changes since the last commit
  spoor log [-n N] [PATH]     history (of one path)
  spoor show [REF] [--patch]  what a commit changed
  spoor diff A [B|now]        compare any two points in history
  spoor blame PATH            which commit (and process) put PATH there
  spoor explain PATH          what a path is and why it matters
  spoor note REF PATH [TEXT]  read or write a note on a change

Undo
  spoor revert REF [--apply] [--only CATS] [--path P] [--force] [--script FILE]
  spoor restore PATH --to REF [--before] [--apply]

Share & housekeeping
  spoor quickfix REF [-o FILE]            vim quickfix list of a commit (vim -q FILE)
  spoor export REF [--format md|json]     redacted, shareable footprint
  spoor init [--root SPEC]... [--no-state] [--max-content BYTES]
  spoor roots | gc | version

Root specs: PATH[:DEPTH[:content|meta]]   e.g. --root ~/.config:3 --root /Applications:1:meta
Repository: $SPOOR_HOME (default ~/.local/share/spoor)
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type rootsFlag []string

func (r *rootsFlag) String() string     { return strings.Join(*r, ",") }
func (r *rootsFlag) Set(v string) error { *r = append(*r, v); return nil }

type multiFlag []string

func (r *multiFlag) String() string     { return strings.Join(*r, ",") }
func (r *multiFlag) Set(v string) error { *r = append(*r, v); return nil }

// parse lets flags appear after positional args (`show abc --patch`).
// Everything after `--` is positional.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") {
				continue
			}
			if f := fs.Lookup(name); f != nil {
				if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
					continue
				}
				if i+1 < len(args) {
					flags = append(flags, args[i+1])
					i++
				}
			}
			continue
		}
		pos = append(pos, a)
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return append(pos, fs.Args()...), nil
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if args[0] == "__try-child" {
		if len(args) > 1 {
			try.Child(args[1])
		}
		return 201
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(stdout, "spoor", version)
		return 0
	}
	a, err := app.Open("")
	if err != nil {
		fmt.Fprintln(stderr, "spoor:", err)
		return 1
	}
	a.Out, a.Err = stdout, stderr
	cmd, rest := args[0], args[1:]
	fn, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(stderr, "spoor: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	code, err := fn(a, rest)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 2
		}
		fmt.Fprintln(stderr, "spoor:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

type command func(a *app.App, args []string) (int, error)

var commands map[string]command

func init() {
	commands = map[string]command{
		"init": cmdInit, "roots": cmdRoots, "snap": cmdSnap, "run": cmdRun, "status": cmdStatus,
		"log": cmdLog, "show": cmdShow, "diff": cmdDiff, "review": cmdReview, "blame": cmdBlame,
		"revert": cmdRevert, "restore": cmdRestore, "note": cmdNote, "explain": cmdExplain,
		"quickfix": cmdQuickfix, "export": cmdExport, "try": cmdTry, "gc": cmdGC, "upgrade": cmdUpgrade,
	}
}

func rootFlags(fs *flag.FlagSet) (*rootsFlag, *bool) {
	var r rootsFlag
	fs.Var(&r, "root", "watch PATH[:DEPTH[:content|meta]] (repeatable)")
	noState := fs.Bool("no-state", false, "skip system state (launchd, crontab, ports)")
	return &r, noState
}

func parseRoots(a *app.App, specs []string) ([]model.Root, error) {
	var out []model.Root
	for _, s := range specs {
		r, err := profile.Parse(a.ExpandPath(strings.SplitN(s, ":", 2)[0]) + suffix(s))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func suffix(spec string) string {
	if i := strings.Index(spec, ":"); i >= 0 {
		return spec[i:]
	}
	return ""
}

func newFS(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: spoor %s — see `spoor help`\n", name)
		fs.PrintDefaults()
	}
	return fs
}

func cmdInit(a *app.App, args []string) (int, error) {
	fs := newFS("init")
	roots, noState := rootFlags(fs)
	maxContent := fs.Int64("max-content", 0, "largest file body to capture, in bytes (default 1 MiB)")
	if _, err := parse(fs, args); err != nil {
		return 2, err
	}
	rs, err := parseRoots(a, *roots)
	if err != nil {
		return 2, err
	}
	cfg := &store.Config{Roots: rs, NoState: *noState, MaxContent: *maxContent}
	if err := a.St.SaveConfig(cfg); err != nil {
		return 1, err
	}
	fmt.Fprintf(a.Out, "repository %s configured\n", a.Tilde(a.St.Root))
	return cmdRoots(a, nil)
}

func cmdRoots(a *app.App, args []string) (int, error) {
	for _, r := range a.Roots(nil) {
		mode := "content"
		if !r.Content {
			mode = "meta"
		}
		depth := "any depth"
		if r.Depth >= 0 {
			depth = fmt.Sprintf("depth %d", r.Depth)
		}
		exists := ""
		if _, err := os.Lstat(r.Path); err != nil {
			exists = "  (absent)"
		}
		fmt.Fprintf(a.Out, "  %-58s %-9s %s%s\n", a.Tilde(r.Path), depth, mode, exists)
	}
	if a.State(false) {
		fmt.Fprintln(a.Out, "  + system state (loaded jobs, crontab, listening ports)")
	}
	return 0, nil
}

func cmdSnap(a *app.App, args []string) (int, error) {
	fs := newFS("snap")
	msg := fs.String("m", "", "message")
	roots, noState := rootFlags(fs)
	var addRoots multiFlag
	fs.Var(&addRoots, "add-root", "watch this too, on top of the configured roots (repeatable)")
	if _, err := parse(fs, args); err != nil {
		return 2, err
	}
	rs, err := parseRoots(a, *roots)
	if err != nil {
		return 2, err
	}
	extra, err := parseRoots(a, addRoots)
	if err != nil {
		return 2, err
	}
	c, err := a.Snap(app.MergeRoots(a.Roots(rs), extra), a.State(*noState), *msg)
	if err != nil {
		return 1, err
	}
	if c == nil {
		fmt.Fprintln(a.Out, "nothing changed since HEAD")
		return 0, nil
	}
	pre, post, changes, _ := a.CommitChanges(c)
	fmt.Fprintf(a.Out, "recorded %s (%d entries watched)\n", c.ID, len(post.Entries))
	if c.Pre != "" {
		a.PrintChanges(a.Out, a.View(pre, post, changes), false, nil)
	}
	return 0, nil
}

func cmdRun(a *app.App, args []string) (int, error) {
	fs := newFS("run")
	msg := fs.String("m", "", "message (default: the command)")
	traceF := fs.Bool("trace", false, "attribute writes to processes (eslogger/strace)")
	review := fs.Bool("review", false, "open the inspector afterwards without asking")
	noReview := fs.Bool("no-review", false, "do not offer the inspector afterwards")
	roots, noState := rootFlags(fs)
	var addRoots multiFlag
	fs.Var(&addRoots, "add-root", "watch this too, on top of the configured roots (repeatable)")
	i := indexOf(args, "--")
	if i < 0 {
		return 2, errors.New("usage: spoor run [flags] -- COMMAND...")
	}
	if _, err := parse(fs, args[:i]); err != nil {
		return 2, err
	}
	rs, err := parseRoots(a, *roots)
	if err != nil {
		return 2, err
	}
	extra, err := parseRoots(a, addRoots)
	if err != nil {
		return 2, err
	}
	argv := args[i+1:]
	auto, notes := a.AutoRoots(argv)
	for _, n := range notes {
		fmt.Fprintln(a.Err, "spoor: "+n)
	}
	watch := app.MergeRoots(a.Roots(rs), append(extra, auto...))
	c, exit, err := a.Run(app.RunOptions{Roots: watch, State: a.State(*noState), Trace: *traceF, Message: *msg, Argv: argv})
	if err != nil {
		return max(exit, 1), err
	}
	pre, post, changes, _ := a.CommitChanges(c)
	items := a.View(pre, post, changes)
	fmt.Fprintf(a.Err, "\nspoor: %s exited %d — commit %s\n", c.Command[0], exit, c.ID)
	a.PrintChanges(a.Err, items, false, a.St.Trace(c.ID))
	fmt.Fprintf(a.Err, "\n  inspect: spoor review %s    undo: spoor revert %s --apply\n", c.ID, c.ID)
	return exit, offerReview(a, c.ID, *review, *noReview, items)
}

// offerReview opens the inspector after a recording when a person is at
// the terminal: straight away with --review, otherwise after asking.
func offerReview(a *app.App, id string, force, skip bool, items []app.Item) error {
	interesting := 0
	for _, it := range items {
		if it.Rule.Category != kb.Noise {
			interesting++
		}
	}
	tty := isatty.IsTerminal(os.Stdout.Fd()) && isatty.IsTerminal(os.Stdin.Fd())
	if !tty || skip || (!force && (interesting == 0 || os.Getenv("SPOOR_NO_PROMPT") != "")) {
		return nil
	}
	if !force {
		fmt.Fprint(a.Err, "\n  Open the review now? [Y/n] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
		default:
			return nil
		}
	}
	return tui.Run(a, id)
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

func cmdStatus(a *app.App, args []string) (int, error) {
	fs := newFS("status")
	all := fs.Bool("all", false, "include noise")
	patch := fs.Bool("patch", false, "print diffs")
	if _, err := parse(fs, args); err != nil {
		return 2, err
	}
	head, changes, err := a.Status()
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(a.Out, "since %s (%s, %s):\n", head.ID, head.Kind, head.Time.Local().Format("2006-01-02 15:04"))
	items := a.Items(changes)
	a.PrintChanges(a.Out, items, *all, nil)
	if *patch {
		printPatches(a, items, *all, true)
	}
	return 0, nil
}

func cmdLog(a *app.App, args []string) (int, error) {
	fs := newFS("log")
	n := fs.Int("n", 30, "number of commits")
	pos, err := parse(fs, args)
	if err != nil {
		return 2, err
	}
	if len(pos) > 0 {
		return cmdBlame(a, pos)
	}
	cs, err := a.St.Commits()
	if err != nil {
		return 1, err
	}
	if len(cs) == 0 {
		fmt.Fprintln(a.Out, "no history yet: spoor snap")
	}
	head := a.St.Head()
	for i, c := range cs {
		if i >= *n {
			break
		}
		mark := "  "
		if c.ID == head {
			mark = "* "
		}
		fmt.Fprintln(a.Out, mark+a.CommitLine(c))
	}
	return 0, nil
}

func cmdShow(a *app.App, args []string) (int, error) {
	fs := newFS("show")
	all := fs.Bool("all", false, "include noise")
	patch := fs.Bool("patch", false, "print diffs")
	pos, err := parse(fs, args)
	if err != nil {
		return 2, err
	}
	ref := "HEAD"
	if len(pos) > 0 {
		ref = pos[0]
	}
	c, err := a.St.Resolve(ref)
	if err != nil {
		return 1, err
	}
	pre, post, changes, err := a.CommitChanges(c)
	if err != nil {
		return 1, err
	}
	items := a.View(pre, post, changes)
	fmt.Fprintf(a.Out, "commit %s  %s  %s\n", c.ID, c.Kind, c.Time.Local().Format("2006-01-02 15:04:05"))
	if c.Parent != "" {
		fmt.Fprintf(a.Out, "parent %s\n", c.Parent)
	}
	if len(c.Command) > 0 {
		fmt.Fprintf(a.Out, "command %s  (exit %d, %.1fs, in %s)\n", strings.Join(c.Command, " "), c.ExitCode, c.Duration, a.Tilde(c.Cwd))
	}
	if c.Reverts != "" {
		fmt.Fprintf(a.Out, "reverts %s\n", c.Reverts)
	}
	if c.Kind == model.KindTry {
		fmt.Fprintf(a.Out, "try %s  (workspace %s)\n", c.TryState, c.Workspace)
	}
	fmt.Fprintf(a.Out, "    %s\n", c.Message)
	a.PrintChanges(a.Out, items, *all, a.St.Trace(c.ID))
	notes := a.St.Notes(c.ID)
	if len(notes) > 0 {
		fmt.Fprintln(a.Out, "\n  NOTES")
		keys := make([]string, 0, len(notes))
		for k := range notes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(a.Out, "  %s: %s\n", a.Tilde(k), notes[k])
		}
	}
	if *patch {
		printPatches(a, items, *all, c.ID == a.St.Head())
	}
	return 0, nil
}

func printPatches(a *app.App, items []app.Item, all, live bool) {
	color := isatty.IsTerminal(os.Stdout.Fd())
	for _, it := range items {
		if it.Rule.Category == kb.Noise && !all {
			continue
		}
		u := diff.Unified(a.St, it.Change, live)
		if strings.TrimSpace(u) == "" {
			continue
		}
		if it.Virtual {
			fmt.Fprintf(a.Out, "\n=== %s %s: %s\n", it.Kind.Symbol(), it.Rule.Title, it.Label)
		} else {
			fmt.Fprintf(a.Out, "\n=== %s %s\n", it.Kind.Symbol(), a.Tilde(it.Path))
		}
		for _, l := range strings.Split(strings.TrimRight(u, "\n"), "\n") {
			if color {
				switch {
				case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
				case strings.HasPrefix(l, "+"):
					l = "\x1b[32m" + l + "\x1b[0m"
				case strings.HasPrefix(l, "-"):
					l = "\x1b[31m" + l + "\x1b[0m"
				case strings.HasPrefix(l, "@@"):
					l = "\x1b[36m" + l + "\x1b[0m"
				}
			}
			fmt.Fprintln(a.Out, l)
		}
	}
}

func cmdDiff(a *app.App, args []string) (int, error) {
	fs := newFS("diff")
	all := fs.Bool("all", false, "include noise")
	patch := fs.Bool("patch", false, "print diffs")
	var paths multiFlag
	fs.Var(&paths, "path", "limit to PATH (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return 2, err
	}
	if len(pos) == 0 || len(pos) > 2 {
		return 2, errors.New("usage: spoor diff A [B|now]")
	}
	if len(pos) == 1 {
		pos = append(pos, "now")
	}
	ma, _, err := a.ManifestAt(pos[0])
	if err != nil {
		return 1, err
	}
	mb, live, err := a.ManifestAt(pos[1])
	if err != nil {
		return 1, err
	}
	changes := diff.Compare(ma, mb)
	if len(paths) > 0 {
		want := map[string]bool{}
		for _, p := range paths {
			want[a.ExpandPath(p)] = true
		}
		var f []model.Change
		for _, c := range changes {
			if want[c.Path] {
				f = append(f, c)
			}
		}
		changes = f
	}
	fmt.Fprintf(a.Out, "%s → %s\n", pos[0], pos[1])
	items := a.View(ma, mb, changes)
	a.PrintChanges(a.Out, items, *all, nil)
	if *patch {
		printPatches(a, items, *all, live)
	}
	return 0, nil
}

func cmdReview(a *app.App, args []string) (int, error) {
	ref := ""
	if len(args) > 0 {
		ref = args[0]
	}
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		return 1, errors.New("review needs a terminal (use `spoor show` for plain output)")
	}
	return 0, tui.Run(a, ref)
}

func cmdBlame(a *app.App, args []string) (int, error) {
	if len(args) != 1 {
		return 2, errors.New("usage: spoor blame PATH")
	}
	p := a.ExpandPath(args[0])
	hits, err := a.Blame(p)
	if err != nil {
		return 1, err
	}
	if len(hits) == 0 {
		fmt.Fprintf(a.Out, "%s: no recorded change in history\n", a.Tilde(p))
		if _, err := os.Lstat(p); err == nil {
			fmt.Fprintln(a.Out, "  (it exists, so it predates the first snapshot or lies outside the watched roots)")
		}
		return 0, nil
	}
	for _, h := range hits {
		what := strings.Join(h.Commit.Command, " ")
		if what == "" {
			what = h.Commit.Message
		}
		kind := string(h.Change.Kind)
		if h.Change.OldPath == p {
			kind = "renamed away"
		}
		fmt.Fprintf(a.Out, "%s  %s  %-10s %-8s %s\n", h.Commit.ID, h.Commit.Time.Local().Format("2006-01-02 15:04"), kind, h.Commit.Kind, what)
		for _, w := range h.Writers {
			fmt.Fprintf(a.Out, "    by pid %d %s (%s)\n", w.Pid, a.Tilde(w.Exe), w.Op)
		}
		if n := a.St.Notes(h.Commit.ID)[p]; n != "" {
			fmt.Fprintf(a.Out, "    note: %s\n", n)
		}
	}
	return 0, nil
}

func cmdRevert(a *app.App, args []string) (int, error) {
	fs := newFS("revert")
	apply := fs.Bool("apply", false, "execute the plan (default: show it)")
	force := fs.Bool("force", false, "overwrite paths changed since the commit")
	only := fs.String("only", "", "comma-separated categories: persistence,trust,shell,path,apps,config,state,data,noise")
	script := fs.String("script", "", "write the plan as a shell script to FILE")
	var paths multiFlag
	fs.Var(&paths, "path", "limit to PATH (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return 2, err
	}
	if len(pos) != 1 {
		return 2, errors.New("usage: spoor revert REF [--apply]")
	}
	opt := revert.Options{Force: *force}
	if len(paths) > 0 {
		opt.Paths = map[string]bool{}
		for _, p := range paths {
			opt.Paths[a.ExpandPath(p)] = true
		}
	}
	if *only != "" {
		opt.Categories = map[kb.Category]bool{}
		for _, c := range strings.Split(*only, ",") {
			opt.Categories[kb.Category(strings.TrimSpace(c))] = true
		}
	}
	res, err := a.Revert(pos[0], opt, *apply)
	if err != nil {
		return 1, err
	}
	if *script != "" {
		c, _ := a.St.Resolve(pos[0])
		if err := os.WriteFile(*script, []byte(revert.Script(a.St, res.Plan, "undo "+c.ID+": "+c.Message)), 0o700); err != nil {
			return 1, err
		}
		fmt.Fprintf(a.Out, "wrote %s\n", *script)
	}
	if !*apply {
		fmt.Fprintln(a.Out, "plan (dry run — add --apply to execute):")
		a.PrintPlan(a.Out, res.Plan)
		return 0, nil
	}
	failed := 0
	for _, r := range res.Results {
		if !r.Action.Executes() {
			continue
		}
		status := "ok"
		if r.Err != nil {
			status = "FAILED: " + r.Err.Error()
			failed++
		} else if r.Note != "" {
			status = r.Note
		}
		target := a.Tilde(r.Action.Path)
		if r.Action.Label != "" {
			target = r.Action.Label
		}
		fmt.Fprintf(a.Out, "  %-10s %s  %s\n", r.Action.Op, target, status)
	}
	for _, act := range res.Plan {
		if act.Op == revert.Conflict || act.Op == revert.Unrestorable {
			fmt.Fprintf(a.Out, "  %-10s %s  — %s\n", act.Op, a.Tilde(act.Path), act.Reason)
		}
	}
	fmt.Fprintf(a.Out, "recorded as commit %s\n", res.Commit.ID)
	if failed > 0 {
		return 1, fmt.Errorf("%d action(s) failed", failed)
	}
	return 0, nil
}

func cmdRestore(a *app.App, args []string) (int, error) {
	fs := newFS("restore")
	to := fs.String("to", "", "commit to restore from")
	before := fs.Bool("before", false, "use the state before that commit instead of after")
	apply := fs.Bool("apply", false, "execute (default: show the plan)")
	pos, err := parse(fs, args)
	if err != nil {
		return 2, err
	}
	if len(pos) != 1 || *to == "" {
		return 2, errors.New("usage: spoor restore PATH --to REF [--before] [--apply]")
	}
	p := a.ExpandPath(pos[0])
	plan, c, m, err := a.RestorePlan(p, *to, *before)
	if err != nil {
		return 1, err
	}
	if !*apply {
		fmt.Fprintln(a.Out, "plan (dry run — add --apply to execute):")
		a.PrintPlan(a.Out, plan)
		return 0, nil
	}
	res, nc, err := a.ApplyRecorded(plan, "restore "+a.Tilde(p)+" from "+c.ID, "", m.Roots, m.State)
	if err != nil {
		return 1, err
	}
	for _, r := range res {
		if r.Err != nil {
			return 1, r.Err
		}
	}
	fmt.Fprintf(a.Out, "restored %s — recorded as commit %s\n", a.Tilde(p), nc.ID)
	return 0, nil
}

func cmdNote(a *app.App, args []string) (int, error) {
	if len(args) < 2 {
		return 2, errors.New("usage: spoor note REF PATH [TEXT...]")
	}
	c, err := a.St.Resolve(args[0])
	if err != nil {
		return 1, err
	}
	p := a.ExpandPath(args[1])
	if len(args) == 2 {
		if n := a.St.Notes(c.ID)[p]; n != "" {
			fmt.Fprintln(a.Out, n)
		}
		return 0, nil
	}
	return 0, a.St.SetNote(c.ID, p, strings.Join(args[2:], " "))
}

func cmdExplain(a *app.App, args []string) (int, error) {
	if len(args) != 1 {
		return 2, errors.New("usage: spoor explain PATH")
	}
	p := a.ExpandPath(args[0])
	rule := kb.Classify(p, a.Home, a.GOOS)
	fmt.Fprintf(a.Out, "%s %s  [%s]\n", rule.Risk.Icon(), rule.Title, rule.Category)
	if rule.Explain != "" {
		fmt.Fprintln(a.Out, "  "+rule.Explain)
	}
	if e, ok := scan.EntryFor(a.St, p, "", false, 0); ok {
		ch := model.Change{Path: p, Kind: model.Added, After: &e}
		if e.Type == model.File && e.Size <= scan.DefaultMaxContent && !scan.Sensitive(p) {
			if h, err := store.HashFile(p); err == nil {
				e.Hash, e.Stored = h, true
			}
		}
		for _, d := range kb.Analyze(nil, ch, true) {
			fmt.Fprintf(a.Out, "  %s: %s\n", d.Key, d.Value)
		}
	} else if !strings.HasPrefix(p, model.StatePrefix) {
		fmt.Fprintln(a.Out, "  (does not exist)")
	}
	if hits, err := a.Blame(p); err == nil && len(hits) > 0 {
		h := hits[0]
		what := strings.Join(h.Commit.Command, " ")
		if what == "" {
			what = h.Commit.Message
		}
		fmt.Fprintf(a.Out, "  last change: %s %s by %s (%s)\n", h.Change.Kind, h.Commit.Time.Local().Format("2006-01-02"), what, h.Commit.ID)
	}
	return 0, nil
}

func cmdQuickfix(a *app.App, args []string) (int, error) {
	fs := newFS("quickfix")
	out := fs.String("o", "", "write to FILE instead of stdout")
	pos, err := parse(fs, args)
	if err != nil {
		return 2, err
	}
	ref := "HEAD"
	if len(pos) > 0 {
		ref = pos[0]
	}
	c, err := a.St.Resolve(ref)
	if err != nil {
		return 1, err
	}
	_, _, changes, err := a.CommitChanges(c)
	if err != nil {
		return 1, err
	}
	qf := a.Quickfix(c, changes)
	if *out == "" {
		fmt.Fprint(a.Out, qf)
		return 0, nil
	}
	return 0, os.WriteFile(*out, []byte(qf), 0o600)
}

func cmdExport(a *app.App, args []string) (int, error) {
	fs := newFS("export")
	format := fs.String("format", "md", "md or json")
	noRedact := fs.Bool("no-redact", false, "keep secrets and personal paths (do not share the result)")
	out := fs.String("o", "", "write to FILE instead of stdout")
	pos, err := parse(fs, args)
	if err != nil {
		return 2, err
	}
	ref := "HEAD"
	if len(pos) > 0 {
		ref = pos[0]
	}
	c, err := a.St.Resolve(ref)
	if err != nil {
		return 1, err
	}
	_, _, changes, err := a.CommitChanges(c)
	if err != nil {
		return 1, err
	}
	fp := a.Export(c, changes, !*noRedact)
	var text string
	switch *format {
	case "md":
		text = fp.Markdown()
	case "json":
		text = fp.JSON()
	default:
		return 2, fmt.Errorf("unknown format %q", *format)
	}
	if *out == "" {
		fmt.Fprint(a.Out, text)
		return 0, nil
	}
	return 0, os.WriteFile(*out, []byte(text), 0o600)
}

func cmdTry(a *app.App, args []string) (int, error) {
	if len(args) > 0 {
		switch args[0] {
		case "apply":
			fs := newFS("try apply")
			force := fs.Bool("force", false, "apply even if files changed since the try")
			pos, err := parse(fs, args[1:])
			if err != nil {
				return 2, err
			}
			if len(pos) != 1 {
				return 2, errors.New("usage: spoor try apply REF")
			}
			c, err := a.TryApply(pos[0], *force)
			if err != nil {
				return 1, err
			}
			pre, post, changes, _ := a.CommitChanges(c)
			fmt.Fprintf(a.Out, "applied — recorded as commit %s\n", c.ID)
			a.PrintChanges(a.Out, a.View(pre, post, changes), false, nil)
			return 0, nil
		case "discard":
			if len(args) != 2 {
				return 2, errors.New("usage: spoor try discard REF")
			}
			if err := a.TryDiscard(args[1]); err != nil {
				return 1, err
			}
			fmt.Fprintln(a.Out, "discarded")
			return 0, nil
		}
	}
	fs := newFS("try")
	msg := fs.String("m", "", "message")
	review := fs.Bool("review", false, "open the inspector afterwards without asking")
	noReview := fs.Bool("no-review", false, "do not offer the inspector afterwards")
	roots, _ := rootFlags(fs)
	i := indexOf(args, "--")
	if i < 0 {
		return 2, errors.New("usage: spoor try [flags] -- COMMAND...   |   spoor try apply|discard REF")
	}
	if _, err := parse(fs, args[:i]); err != nil {
		return 2, err
	}
	rs, err := parseRoots(a, *roots)
	if err != nil {
		return 2, err
	}
	c, exit, err := a.Try(args[i+1:], a.Roots(rs), *msg)
	if err != nil {
		return max(exit, 1), err
	}
	pre, post, changes, _ := a.CommitChanges(c)
	items := a.View(pre, post, changes)
	fmt.Fprintf(a.Err, "\nspoor: try %s — exit %d, nothing on disk changed yet\n", c.ID, exit)
	a.PrintChanges(a.Err, items, false, nil)
	fmt.Fprintf(a.Err, "\n  inspect: spoor review %s    keep: spoor try apply %s    drop: spoor try discard %s\n", c.ID, c.ID, c.ID)
	return exit, offerReview(a, c.ID, *review, *noReview, items)
}

func cmdGC(a *app.App, args []string) (int, error) {
	n, freed, err := a.St.GC()
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(a.Out, "removed %d unreferenced object(s), %.1f MiB\n", n, float64(freed)/(1<<20))
	return 0, nil
}
