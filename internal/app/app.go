// Package app implements spoor's operations on top of the lower-level
// packages; the CLI and the TUI both call into it.
package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/morass/spoor/internal/diff"
	"github.com/morass/spoor/internal/kb"
	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/profile"
	"github.com/morass/spoor/internal/revert"
	"github.com/morass/spoor/internal/scan"
	"github.com/morass/spoor/internal/store"
	"github.com/morass/spoor/internal/trace"
)

type App struct {
	St   *store.Store
	Home string
	GOOS string
	Out  io.Writer
	Err  io.Writer
}

func Open(root string) (*App, error) {
	if root == "" {
		root = store.DefaultRoot()
	}
	st, err := store.Open(root)
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	return &App{St: st, Home: home, GOOS: runtime.GOOS, Out: os.Stdout, Err: os.Stderr}, nil
}

// Roots resolves the watch list: explicit override, then the
// repository's saved config, then the built-in profile.
func (a *App) Roots(override []model.Root) []model.Root {
	if len(override) > 0 {
		return override
	}
	if cfg, ok := a.St.Config(); ok && len(cfg.Roots) > 0 {
		return cfg.Roots
	}
	return profile.Default(a.GOOS, a.Home)
}

func (a *App) State(noState bool) bool {
	if noState || os.Getenv("SPOOR_NO_STATE") != "" {
		return false
	}
	cfg, _ := a.St.Config()
	return !cfg.NoState
}

func (a *App) maxContent() int64 {
	cfg, _ := a.St.Config()
	return cfg.MaxContent
}

func (a *App) Scan(roots []model.Root, state bool, prev *model.Manifest, hashOnly bool) (*model.Manifest, []string, error) {
	res, err := scan.Scan(scan.Options{
		Roots: roots, State: state, Prev: prev, Store: a.St, HashOnly: hashOnly,
		MaxContent: a.maxContent(), Skip: []string{a.St.Root},
	})
	if err != nil {
		return nil, nil, err
	}
	if !hashOnly {
		if err := a.St.SaveManifest(res.Manifest); err != nil {
			return nil, nil, err
		}
	}
	return res.Manifest, res.Warnings, nil
}

func (a *App) warn(ws []string) {
	for _, w := range ws {
		fmt.Fprintln(a.Err, "spoor: warning:", w)
	}
}

// Head returns HEAD and its post manifest (nil, empty manifest if none).
func (a *App) Head() (*model.Commit, *model.Manifest, error) {
	id := a.St.Head()
	if id == "" {
		return nil, nil, nil
	}
	c, err := a.St.LoadCommit(id)
	if err != nil {
		return nil, nil, err
	}
	m, err := a.St.LoadManifest(c.Post)
	return c, m, err
}

func (a *App) newCommit(kind model.CommitKind, msg string, pre, post *model.Manifest) *model.Commit {
	c := &model.Commit{ID: store.NewID(), Parent: a.St.Head(), Kind: kind, Time: time.Now().UTC(), Message: msg, Post: post.ID}
	if pre != nil {
		c.Pre = pre.ID
	}
	return c
}

func (a *App) save(c *model.Commit, moveHead bool) error {
	if err := a.St.SaveCommit(c); err != nil {
		return err
	}
	if moveHead {
		return a.St.SetHead(c.ID)
	}
	return nil
}

// baseline records what the machine looks like before an operation. If
// it differs from HEAD, the difference is committed as drift first, so
// history stays continuous and blame can name "outside spoor" changes.
func (a *App) baseline(roots []model.Root, state bool) (*model.Manifest, error) {
	head, headM, err := a.Head()
	if err != nil {
		return nil, err
	}
	pre, ws, err := a.Scan(roots, state, headM, false)
	if err != nil {
		return nil, err
	}
	a.warn(ws)
	switch {
	case head == nil:
		c := a.newCommit(model.KindSnap, "baseline", nil, pre)
		if err := a.save(c, true); err != nil {
			return nil, err
		}
	case len(diff.Compare(headM, pre)) > 0:
		c := a.newCommit(model.KindDrift, "changes made outside spoor", headM, pre)
		if err := a.save(c, true); err != nil {
			return nil, err
		}
	}
	return pre, nil
}

type RunOptions struct {
	Roots   []model.Root
	State   bool
	Trace   bool
	Message string
	Argv    []string
}

// Run records the machine, runs the command, records it again and commits.
func (a *App) Run(o RunOptions) (*model.Commit, int, error) {
	if len(o.Argv) == 0 {
		return nil, 0, errors.New("nothing to run: spoor run -- <command>")
	}
	if err := a.St.Lock(); err != nil {
		return nil, 0, err
	}
	defer a.St.Unlock()
	fmt.Fprintln(a.Err, "spoor: recording before state…")
	pre, err := a.baseline(o.Roots, o.State)
	if err != nil {
		return nil, 0, err
	}
	cwd, _ := os.Getwd()
	argv := o.Argv
	var ts *trace.Session
	if o.Trace {
		if ts, argv, err = trace.Prepare(o.Argv, cwd, filepath.Join(a.St.Root, "tmp")); err != nil {
			return nil, 0, err
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM) // the child gets them from the terminal; we must survive to record
	defer signal.Stop(sigs)
	start := time.Now()
	exit := 0
	if err := cmd.Start(); err != nil {
		if ts != nil {
			ts.Finish()
		}
		return nil, 127, fmt.Errorf("start %s: %w", argv[0], err)
	}
	if ts != nil {
		ts.SetRoot(cmd.Process.Pid)
	}
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = 1
		}
	}
	dur := time.Since(start)
	var writers map[string][]model.Writer
	if ts != nil {
		if writers, err = ts.Finish(); err != nil {
			fmt.Fprintln(a.Err, "spoor: warning: trace:", err)
		}
	}
	fmt.Fprintln(a.Err, "spoor: recording after state…")
	post, ws, err := a.Scan(o.Roots, o.State, pre, false)
	if err != nil {
		return nil, exit, err
	}
	a.warn(ws)
	msg := o.Message
	if msg == "" {
		msg = strings.Join(o.Argv, " ")
	}
	c := a.newCommit(model.KindRun, msg, pre, post)
	c.Command, c.Cwd, c.ExitCode, c.Duration = o.Argv, cwd, exit, dur.Seconds()
	c.Traced = ts != nil
	if writers != nil {
		if err := a.St.SaveTrace(c.ID, writers); err != nil {
			return nil, exit, err
		}
	}
	return c, exit, a.save(c, true)
}

// Snap commits the current state if it differs from HEAD.
func (a *App) Snap(roots []model.Root, state bool, msg string) (*model.Commit, error) {
	if err := a.St.Lock(); err != nil {
		return nil, err
	}
	defer a.St.Unlock()
	head, headM, err := a.Head()
	if err != nil {
		return nil, err
	}
	m, ws, err := a.Scan(roots, state, headM, false)
	if err != nil {
		return nil, err
	}
	a.warn(ws)
	if head != nil && len(diff.Compare(headM, m)) == 0 {
		os.Remove(filepath.Join(a.St.Root, "manifests", m.ID+".json.gz"))
		return nil, nil
	}
	if msg == "" {
		msg = "snapshot"
		if head == nil {
			msg = "baseline"
		}
	}
	c := a.newCommit(model.KindSnap, msg, headM, m)
	if head == nil {
		c.Pre = ""
	}
	return c, a.save(c, true)
}

// Status compares HEAD with the live machine without storing anything.
func (a *App) Status() (*model.Commit, []model.Change, error) {
	head, headM, err := a.Head()
	if err != nil {
		return nil, nil, err
	}
	if head == nil {
		return nil, nil, errors.New("no history yet: run `spoor snap` to record a baseline")
	}
	live, ws, err := a.Scan(headM.Roots, headM.State, headM, true)
	if err != nil {
		return nil, nil, err
	}
	a.warn(ws)
	return head, diff.Compare(headM, live), nil
}

// CommitChanges loads a commit's before/after manifests and their diff.
func (a *App) CommitChanges(c *model.Commit) (*model.Manifest, *model.Manifest, []model.Change, error) {
	pre, err := a.St.LoadManifest(c.Pre)
	if err != nil {
		return nil, nil, nil, err
	}
	post, err := a.St.LoadManifest(c.Post)
	if err != nil {
		return nil, nil, nil, err
	}
	if c.Pre == "" {
		return pre, post, diff.Compare(nil, post), nil
	}
	return pre, post, diff.Compare(pre, post), nil
}

// ManifestAt resolves a ref to a manifest; "now" scans the live machine
// (hash-only) with the roots of HEAD.
func (a *App) ManifestAt(ref string) (*model.Manifest, bool, error) {
	if ref == "now" {
		_, headM, err := a.Head()
		if err != nil {
			return nil, false, err
		}
		roots, state := a.Roots(nil), a.State(false)
		if headM != nil {
			roots, state = headM.Roots, headM.State
		}
		m, ws, err := a.Scan(roots, state, headM, true)
		a.warn(ws)
		return m, true, err
	}
	c, err := a.St.Resolve(ref)
	if err != nil {
		return nil, false, err
	}
	m, err := a.St.LoadManifest(c.Post)
	return m, false, err
}

type BlameHit struct {
	Commit  *model.Commit
	Change  model.Change
	Writers []model.Writer
}

// Blame lists the commits on HEAD's history that touched path, newest first.
func (a *App) Blame(path string) ([]BlameHit, error) {
	chain, err := a.St.Chain()
	if err != nil {
		return nil, err
	}
	var hits []BlameHit
	for _, c := range chain {
		_, _, changes, err := a.CommitChanges(c)
		if err != nil {
			continue
		}
		for _, ch := range changes {
			if ch.Path == path || ch.OldPath == path {
				hits = append(hits, BlameHit{Commit: c, Change: ch, Writers: a.St.Trace(c.ID)[path]})
			}
		}
	}
	return hits, nil
}

type RevertResult struct {
	Plan    []revert.Action
	Results []revert.Result
	Commit  *model.Commit
}

// Revert plans (and with apply, executes and commits) undoing a commit.
func (a *App) Revert(ref string, opt revert.Options, apply bool) (*RevertResult, error) {
	c, err := a.St.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if c.Kind == model.KindTry {
		return nil, errors.New("a try was never applied to the machine; use `spoor try discard`")
	}
	_, post, changes, err := a.CommitChanges(c)
	if err != nil {
		return nil, err
	}
	opt.Home, opt.GOOS = a.Home, a.GOOS
	plan := revert.Plan(a.St, changes, opt)
	res := &RevertResult{Plan: plan}
	if !apply {
		return res, nil
	}
	title := fmt.Sprintf("revert %s: %s", c.ID, c.Message)
	res.Results, res.Commit, err = a.ApplyRecorded(plan, title, c.ID, post.Roots, post.State)
	return res, err
}

// ApplyRecorded runs actions between two scans and commits the result,
// so every undo is itself in history and can be undone.
func (a *App) ApplyRecorded(plan []revert.Action, msg, reverts string, roots []model.Root, state bool) ([]revert.Result, *model.Commit, error) {
	if err := a.St.Lock(); err != nil {
		return nil, nil, err
	}
	defer a.St.Unlock()
	pre, err := a.baseline(roots, state)
	if err != nil {
		return nil, nil, err
	}
	results := revert.Apply(a.St, plan)
	post, ws, err := a.Scan(roots, state, pre, false)
	if err != nil {
		return results, nil, err
	}
	a.warn(ws)
	nc := a.newCommit(model.KindRevert, msg, pre, post)
	nc.Reverts = reverts
	return results, nc, a.save(nc, true)
}

// RestorePlan builds the action that puts path back as it was in a commit.
func (a *App) RestorePlan(path, ref string, before bool) ([]revert.Action, *model.Commit, *model.Manifest, error) {
	c, err := a.St.Resolve(ref)
	if err != nil {
		return nil, nil, nil, err
	}
	id := c.Post
	if before {
		id = c.Pre
	}
	m, err := a.St.LoadManifest(id)
	if err != nil {
		return nil, nil, nil, err
	}
	for i := range m.Entries {
		e := &m.Entries[i]
		if e.Path != path {
			continue
		}
		switch e.Type {
		case model.Dir:
			return []revert.Action{{Op: revert.Mkdir, Path: path, Mode: e.Mode}}, c, m, nil
		case model.Symlink:
			return []revert.Action{{Op: revert.Restore, Path: path, Link: e.Link}}, c, m, nil
		case model.File:
			if !e.Stored {
				return nil, c, m, fmt.Errorf("%s: content was not captured in %s (%s)", path, c.ID, e.Skipped)
			}
			return []revert.Action{{Op: revert.Restore, Path: path, Hash: e.Hash, Mode: e.Mode}}, c, m, nil
		}
	}
	if profile.CoveredBy(m.Roots, path) {
		return []revert.Action{{Op: revert.Delete, Path: path, Reason: "did not exist then"}}, c, m, nil
	}
	return nil, c, m, fmt.Errorf("%s is not inside any watched root of %s", path, c.ID)
}

// Item is a change with its classification, the unit of every listing.
type Item struct {
	model.Change
	Rule kb.Rule
}

func (a *App) Items(changes []model.Change) []Item {
	items := make([]Item, len(changes))
	for i, c := range changes {
		r := kb.Classify(c.Path, a.Home, a.GOOS)
		if r.Title == "File" && isDir(c) {
			r.Title = "Directory"
		}
		items[i] = Item{c, r}
	}
	sort.SliceStable(items, func(i, j int) bool {
		oi, oj := kb.Rank(items[i].Rule.Category), kb.Rank(items[j].Rule.Category)
		if oi != oj {
			return oi < oj
		}
		return items[i].Path < items[j].Path
	})
	return items
}

// Tilde shortens home paths for display.
func (a *App) Tilde(p string) string {
	if a.Home != "" && (p == a.Home || strings.HasPrefix(p, a.Home+"/")) {
		return "~" + p[len(a.Home):]
	}
	return p
}

// ExpandPath turns ~ and relative paths into absolute ones.
func (a *App) ExpandPath(p string) string {
	if p == "~" {
		return a.Home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(a.Home, p[2:])
	}
	if strings.HasPrefix(p, model.StatePrefix) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
