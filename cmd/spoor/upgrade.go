package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/morass/spoor/internal/app"
	"github.com/morass/spoor/internal/brew"
	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/scan"
	"github.com/morass/spoor/internal/tui"
)

// cmdUpgrade is the one-command package upgrade: ask Homebrew what will
// change, capture those packages' files, upgrade, open the review.
func cmdUpgrade(a *app.App, args []string) (int, error) {
	fs := newFS("upgrade")
	list := fs.Bool("list", false, "only show what would be upgraded")
	noUpdate := fs.Bool("no-update", false, "skip running brew update first")
	yes := fs.Bool("y", false, "do not ask; with no names, upgrade everything outdated")
	all := fs.Bool("all", false, "upgrade every outdated formula without the picker")
	noReview := fs.Bool("no-review", false, "do not open the review afterwards")
	msg := fs.String("m", "", "message")
	names, err := parse(fs, args)
	if err != nil {
		return 2, err
	}
	for _, n := range names {
		if !brew.Name.MatchString(n) {
			return 2, fmt.Errorf("%q is not a Homebrew formula name", n)
		}
	}
	b, err := brew.Find()
	if err != nil {
		return 1, err
	}
	if !*noUpdate && !*list {
		fmt.Fprintln(a.Err, "spoor: brew update…")
		if err := b.Update(); err != nil {
			return 1, fmt.Errorf("brew update: %w", err)
		}
	}
	outdated, err := b.Outdated()
	if err != nil {
		return 1, err
	}
	// Names come from brew's JSON; they are passed to brew as separate
	// arguments (never through a shell), and anything that is not a
	// well-formed formula name is dropped.
	valid := outdated[:0]
	for _, o := range outdated {
		if brew.Name.MatchString(o.Name) {
			valid = append(valid, o)
		} else {
			fmt.Fprintf(a.Err, "spoor: ignoring malformed formula name from brew: %q\n", o.Name)
		}
	}
	outdated = valid
	byName := map[string]brew.Outdated{}
	for _, o := range outdated {
		byName[o.Name] = o
	}

	tty := isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
	if len(names) == 0 && !*list && !*all && !*yes && tty {
		var items []tui.PickItem
		for _, o := range outdated {
			it := tui.PickItem{Name: o.Name, From: strings.Join(o.Installed, ","), To: o.Current}
			if o.Pinned {
				it.Disabled = "pinned (brew unpin " + o.Name + ")"
			} else {
				it.Bytes, it.Files = b.Size(o.Name, scan.DefaultMaxContent)
			}
			items = append(items, it)
		}
		if len(items) == 0 {
			fmt.Fprintln(a.Err, "nothing to upgrade")
			return 0, nil
		}
		sel, ok, err := tui.Pick(fmt.Sprintf("upgrade — %d outdated formulae, tick the ones to upgrade", len(items)), items)
		if err != nil {
			return 1, err
		}
		if !ok || len(sel) == 0 {
			fmt.Fprintln(a.Err, "nothing selected")
			return 0, nil
		}
		names = sel
	}

	var upgrades, installs, skipped []string
	if len(names) == 0 {
		for _, o := range outdated {
			if o.Pinned {
				skipped = append(skipped, o.Name+" (pinned)")
				continue
			}
			upgrades = append(upgrades, o.Name)
		}
	}
	for _, n := range names {
		short := n[strings.LastIndex(n, "/")+1:]
		switch o, ok := byName[short]; {
		case ok && o.Pinned:
			skipped = append(skipped, short+" (pinned: brew unpin "+short+" first)")
		case ok:
			upgrades = append(upgrades, short)
		case b.Installed(n):
			skipped = append(skipped, short+" (already up to date)")
		case strings.Contains(short, "@") && b.Exists(n):
			installs = append(installs, n)
		case b.Exists(n):
			return 1, fmt.Errorf("%s is not installed — spoor upgrade only upgrades; record an install with: spoor run -- brew install %s", n, n)
		default:
			if base := strings.SplitN(short, "@", 2)[0]; base != short {
				return 1, fmt.Errorf("Homebrew has no formula %s: it cannot install arbitrary versions, only versioned formulae it ships (brew search %s@)", n, base)
			}
			return 1, fmt.Errorf("Homebrew has no formula %s", n)
		}
	}

	// Brew upgrades outdated dependencies along with a package; capture them too.
	capture := map[string]string{}
	for _, n := range upgrades {
		capture[n] = "upgrade"
		if len(names) > 0 {
			for _, d := range b.Deps(n) {
				if _, ok := byName[d]; ok && capture[d] == "" {
					capture[d] = "dependency of " + n
				}
			}
		}
	}
	for _, n := range installs {
		capture[n] = "install"
		base := strings.SplitN(n[strings.LastIndex(n, "/")+1:], "@", 2)[0]
		if b.Installed(base) {
			capture[base] = "other version of " + n
		}
	}
	for _, s := range skipped {
		fmt.Fprintln(a.Err, "  skip   "+s)
	}
	if len(capture) == 0 {
		fmt.Fprintln(a.Err, "nothing to upgrade")
		return 0, nil
	}

	keys := make([]string, 0, len(capture))
	for k := range capture {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var bytes int64
	fmt.Fprintln(a.Err, "")
	for _, k := range keys {
		size, files := b.Size(k, scan.DefaultMaxContent)
		bytes += size
		line := fmt.Sprintf("  %-26s", k)
		if o, ok := byName[k]; ok {
			line += fmt.Sprintf(" %-14s → %-14s", strings.Join(o.Installed, ","), o.Current)
		} else {
			line += fmt.Sprintf(" %-31s", "")
		}
		if why := capture[k]; why != "upgrade" {
			line += "  (" + why + ")"
		}
		if files > 0 {
			line += fmt.Sprintf("  %d files", files)
		}
		fmt.Fprintln(a.Err, line)
	}
	fmt.Fprintf(a.Err, "\n  spoor keeps the current files (%.1f MB) so every change can be diffed and undone\n", float64(bytes)/(1<<20))
	if *list {
		return 0, nil
	}
	if !*yes && tty {
		fmt.Fprintf(a.Err, "\n  Upgrade %d package(s)? [Y/n] ", len(upgrades)+len(installs))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
		default:
			return 0, nil
		}
	}

	roots := []model.Root{
		{Path: b.Cellar, Depth: 2},
		{Path: filepath.Join(filepath.Dir(b.Cellar), "opt"), Depth: 1},
		{Path: filepath.Join(filepath.Dir(b.Cellar), "bin"), Depth: 1},
	}
	for _, k := range keys {
		roots = append(roots, model.Root{Path: b.Folder(k), Depth: -1, Content: true})
	}
	var runs [][]string
	if len(upgrades) > 0 {
		runs = append(runs, append([]string{b.Path, "upgrade", "--formula"}, upgrades...))
	}
	if len(installs) > 0 {
		runs = append(runs, append([]string{b.Path, "install", "--formula"}, installs...))
	}
	os.Setenv("HOMEBREW_NO_AUTO_UPDATE", "1")
	var c *model.Commit
	exit := 0
	for _, argv := range runs {
		m := *msg
		if m == "" {
			m = "brew " + strings.Join(argv[1:2], "") + " " + strings.Join(argv[3:], " ")
			if len(names) == 0 && argv[1] == "upgrade" {
				m = fmt.Sprintf("brew upgrade (all %d outdated)", len(upgrades))
			}
		}
		var err error
		c, exit, err = a.Run(app.RunOptions{Roots: app.MergeRoots(a.Roots(nil), roots), State: a.State(false), Message: m, Argv: argv})
		if err != nil {
			return max(exit, 1), err
		}
		if exit != 0 {
			break
		}
	}
	pre, post, changes, _ := a.CommitChanges(c)
	items := a.View(pre, post, changes)
	fmt.Fprintf(a.Err, "\nspoor: brew exited %d — commit %s\n", exit, c.ID)
	a.PrintChanges(a.Err, items, false, nil)
	fmt.Fprintf(a.Err, "\n  review: spoor review %s\n", c.ID)
	if exit != 0 {
		err = errors.New("brew reported an error (changes it made are recorded above)")
	}
	if !*noReview && tty {
		if rerr := tui.Run(a, c.ID); rerr != nil {
			return exit, rerr
		}
	}
	return exit, err
}
