// Package revert undoes a commit: it plans the inverse of each change,
// checks the live machine so nothing edited since gets clobbered, and
// can apply the plan or render it as a shell script.
package revert

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/morass/spoor/internal/diff"
	"github.com/morass/spoor/internal/kb"
	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/store"
)

type Op string

const (
	Bootout      Op = "bootout"
	Delete       Op = "delete"
	DeleteTree   Op = "delete-tree"
	Rmdir        Op = "rmdir"
	Mkdir        Op = "mkdir"
	Restore      Op = "restore"
	Chmod        Op = "chmod"
	Bootstrap    Op = "bootstrap"
	Crontab      Op = "crontab"
	Conflict     Op = "conflict"
	Unrestorable Op = "unrestorable"
	Skip         Op = "skip"
)

var order = map[Op]int{Bootout: 0, Delete: 1, DeleteTree: 2, Rmdir: 2, Mkdir: 3, Restore: 4, Chmod: 5, Bootstrap: 6, Crontab: 7, Conflict: 8, Unrestorable: 9, Skip: 10}

type Action struct {
	Op     Op     `json:"op"`
	Path   string `json:"path,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Link   string `json:"link,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Label  string `json:"label,omitempty"`
	Domain string `json:"domain,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Executes reports whether the action changes the machine.
func (a Action) Executes() bool { return order[a.Op] < order[Conflict] }

type Options struct {
	// Paths restricts the plan to these paths (nil means all).
	Paths map[string]bool
	// Categories restricts the plan to these categories (nil means all
	// except noise).
	Categories map[kb.Category]bool
	Force      bool
	Home, GOOS string
	// Since is when the commit's after-state was recorded. An added
	// directory is removed as a whole tree only if nothing inside it was
	// modified after this moment (its deeper contents were never scanned).
	Since time.Time
}

// wants decides whether a change is part of the undo. By default noise is
// left alone, except things the commit created (caches, logs): deleting
// those is safe, while restoring an appended history file would lose data.
func (o Options) wants(c model.Change) bool {
	if o.Paths != nil {
		return o.Paths[c.Path]
	}
	cat := kb.Classify(c.Path, o.Home, o.GOOS).Category
	if o.Categories != nil {
		return o.Categories[cat]
	}
	return cat != kb.Noise || c.Kind == model.Added
}

// Plan builds the actions that undo changes (the pre→post diff of a commit).
func Plan(st *store.Store, changes []model.Change, opt Options) []Action {
	if opt.GOOS == "" {
		opt.GOOS = runtime.GOOS
	}
	if opt.Home == "" {
		opt.Home, _ = os.UserHomeDir()
	}
	var acts []Action
	labels := map[string]bool{}
	for _, c := range changes {
		if !opt.wants(c) {
			continue
		}
		if strings.HasPrefix(c.Path, model.StatePrefix) {
			acts = append(acts, planState(st, c, changes, opt)...)
			continue
		}
		if a, ok := launchdBootout(st, c, opt); ok && !labels[a.Label] {
			labels[a.Label] = true
			acts = append(acts, a)
		}
		acts = append(acts, planFile(c, opt)...)
	}
	dedupeBootouts(&acts)
	sort.SliceStable(acts, func(i, j int) bool {
		oi, oj := order[acts[i].Op], order[acts[j].Op]
		if oi != oj {
			return oi < oj
		}
		di, dj := strings.Count(acts[i].Path, "/"), strings.Count(acts[j].Path, "/")
		switch acts[i].Op {
		case Delete, Rmdir:
			return di > dj // children before parents
		case Mkdir, Restore:
			return di < dj // parents before children
		}
		return false
	})
	return acts
}

func dedupeBootouts(acts *[]Action) {
	seen := map[string]bool{}
	out := (*acts)[:0]
	for _, a := range *acts {
		if a.Op == Bootout {
			if seen[a.Label] {
				continue
			}
			seen[a.Label] = true
		}
		out = append(out, a)
	}
	*acts = out
}

type live struct {
	exists bool
	fi     os.FileInfo
}

func stat(p string) live {
	fi, err := os.Lstat(p)
	return live{err == nil, fi}
}

// matches reports whether the live path is what entry e recorded.
func matches(p string, e *model.Entry) bool {
	l := stat(p)
	if e == nil {
		return !l.exists
	}
	if !l.exists {
		return false
	}
	switch e.Type {
	case model.Dir:
		return l.fi.IsDir()
	case model.Symlink:
		t, err := os.Readlink(p)
		return err == nil && l.fi.Mode()&os.ModeSymlink != 0 && t == e.Link
	case model.File:
		if !l.fi.Mode().IsRegular() {
			return false
		}
		if e.Hash != "" {
			h, err := store.HashFile(p)
			return err == nil && h == e.Hash
		}
		return l.fi.Size() == e.Size && l.fi.ModTime().UnixNano() == e.MTime
	}
	return false
}

func restoreAction(p string, e *model.Entry, why string) Action {
	switch e.Type {
	case model.Dir:
		return Action{Op: Mkdir, Path: p, Mode: e.Mode}
	case model.Symlink:
		return Action{Op: Restore, Path: p, Link: e.Link}
	case model.File:
		if e.Stored && e.Hash != "" {
			return Action{Op: Restore, Path: p, Hash: e.Hash, Mode: e.Mode, Reason: why}
		}
		r := "earlier content was not captured"
		if e.Skipped != "" {
			r += " (" + e.Skipped + ")"
		}
		return Action{Op: Unrestorable, Path: p, Reason: r}
	}
	return Action{Op: Unrestorable, Path: p, Reason: "unsupported file type"}
}

func removeAction(p string, e *model.Entry) Action {
	if e.Type == model.Dir {
		return Action{Op: Rmdir, Path: p}
	}
	return Action{Op: Delete, Path: p}
}

// treeAction plans removing a directory the commit created, including
// contents deeper than the watched depth (a cloned repo, a toolchain).
func treeAction(p string, opt Options) Action {
	clean := filepath.Clean(p)
	if clean == "/" || clean == filepath.Clean(opt.Home) || strings.Count(clean, "/") < 2 {
		return Action{Op: Unrestorable, Path: p, Reason: "refusing to remove a top-level directory"}
	}
	n := 0
	newer := ""
	limit := opt.Since.Add(2 * time.Second)
	filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		n++
		if !opt.Since.IsZero() && newer == "" {
			if fi, err := d.Info(); err == nil && fi.ModTime().After(limit) {
				if q == p && !fi.IsDir() {
					return nil
				}
				if !fi.IsDir() {
					newer = q
				}
			}
		}
		return nil
	})
	if newer != "" {
		a := Action{Op: DeleteTree, Path: p, Reason: fmt.Sprintf("%d entries", n-1)}
		if opt.Force {
			a.Reason = "forced: contains " + newer + " modified after the commit"
			return a
		}
		return Action{Op: Conflict, Path: p, Reason: "contains " + newer + ", modified after the commit"}
	}
	return Action{Op: DeleteTree, Path: p, Reason: fmt.Sprintf("created by the commit, %d entries inside", n-1)}
}

func planFile(c model.Change, opt Options) []Action {
	p := c.Path
	conflict := func(a Action, why string) Action {
		if opt.Force {
			a.Reason = "forced: " + why
			return a
		}
		return Action{Op: Conflict, Path: p, Reason: why}
	}
	switch c.Kind {
	case model.Added:
		switch {
		case !stat(p).exists:
			return []Action{{Op: Skip, Path: p, Reason: "already gone"}}
		case c.After.Type == model.Dir && matches(p, c.After):
			return []Action{treeAction(p, opt)}
		case matches(p, c.After):
			return []Action{removeAction(p, c.After)}
		default:
			return []Action{conflict(removeAction(p, c.After), "changed since the commit")}
		}
	case model.Removed:
		switch {
		case matches(p, c.Before):
			return []Action{{Op: Skip, Path: p, Reason: "already restored"}}
		case !stat(p).exists:
			return []Action{restoreAction(p, c.Before, "")}
		default:
			return []Action{conflict(restoreAction(p, c.Before, ""), "something else exists there now")}
		}
	case model.Modified, model.TypeChange:
		switch {
		case matches(p, c.Before):
			return []Action{{Op: Skip, Path: p, Reason: "already at the earlier version"}}
		case matches(p, c.After):
			if c.Kind == model.TypeChange {
				return []Action{removeAction(p, c.After), restoreAction(p, c.Before, "")}
			}
			return []Action{restoreAction(p, c.Before, "")}
		default:
			return []Action{conflict(restoreAction(p, c.Before, ""), "edited again since the commit")}
		}
	case model.Meta:
		l := stat(p)
		if !l.exists {
			return []Action{{Op: Skip, Path: p, Reason: "no longer exists"}}
		}
		return []Action{{Op: Chmod, Path: p, Mode: c.Before.Mode}}
	case model.Renamed:
		var acts []Action
		if stat(c.OldPath).exists {
			acts = append(acts, Action{Op: Skip, Path: c.OldPath, Reason: "original name already exists"})
		} else {
			acts = append(acts, restoreAction(c.OldPath, c.Before, "rename back"))
		}
		if matches(p, c.After) {
			acts = append(acts, Action{Op: Delete, Path: p})
		} else if stat(p).exists {
			acts = append(acts, conflict(Action{Op: Delete, Path: p}, "renamed file changed since"))
		}
		return acts
	}
	return nil
}

func domainFor(path string) string {
	if strings.HasPrefix(path, "/Library/LaunchDaemons/") {
		return "system"
	}
	return "gui/" + strconv.Itoa(os.Getuid())
}

// launchdBootout unloads a LaunchAgent/Daemon the commit added, so undoing
// the file also stops the job it armed.
func launchdBootout(st *store.Store, c model.Change, opt Options) (Action, bool) {
	if opt.GOOS != "darwin" || c.After == nil || c.Kind == model.Removed || c.Kind == model.Meta {
		return Action{}, false
	}
	if kb.Classify(c.Path, opt.Home, opt.GOOS).Category != kb.Persistence || !strings.HasSuffix(c.Path, ".plist") {
		return Action{}, false
	}
	label := plistLabel(st, c.After)
	if label == "" {
		return Action{}, false
	}
	if c.Kind == model.Modified && c.Before != nil && plistLabel(st, c.Before) == label {
		return Action{}, false // same job before and after: reloading is the user's call
	}
	return Action{Op: Bootout, Label: label, Domain: domainFor(c.Path), Path: c.Path}, true
}

func plistLabel(st *store.Store, e *model.Entry) string {
	b, _ := diff.Content(st, e, true)
	if b == nil || !diff.IsPlist(b) {
		return ""
	}
	v, err := diff.DecodePlist(b)
	if err != nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok {
		if l, ok := m["Label"].(string); ok {
			return l
		}
	}
	return ""
}

func lines(st *store.Store, e *model.Entry) map[string]bool {
	out := map[string]bool{}
	b, _ := diff.Content(st, e, false)
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out[l] = true
		}
	}
	return out
}

func planState(st *store.Store, c model.Change, all []model.Change, opt Options) []Action {
	name := strings.TrimPrefix(c.Path, model.StatePrefix)
	switch name {
	case "launchd-loaded":
		if opt.GOOS != "darwin" {
			return nil
		}
		before, after := lines(st, c.Before), lines(st, c.After)
		var acts []Action
		for l := range after {
			if !before[l] {
				acts = append(acts, Action{Op: Bootout, Label: l, Domain: "gui/" + strconv.Itoa(os.Getuid()), Path: c.Path})
			}
		}
		for l := range before {
			if after[l] {
				continue
			}
			plist := ""
			for _, o := range all {
				if o.Before != nil && strings.HasSuffix(o.Path, ".plist") && plistLabel(st, o.Before) == l {
					plist = o.Path
				}
			}
			if plist == "" {
				acts = append(acts, Action{Op: Unrestorable, Path: c.Path, Label: l, Reason: "job " + l + " was unloaded; its plist is not part of this commit, reload it manually"})
			} else {
				acts = append(acts, Action{Op: Bootstrap, Label: l, Domain: domainFor(plist), Path: plist})
			}
		}
		return acts
	case "crontab":
		if c.Before == nil {
			return []Action{{Op: Crontab, Path: c.Path, Reason: "remove crontab"}}
		}
		if !c.Before.Stored {
			return []Action{{Op: Unrestorable, Path: c.Path, Reason: "earlier crontab not captured"}}
		}
		return []Action{{Op: Crontab, Path: c.Path, Hash: c.Before.Hash}}
	}
	return []Action{{Op: Skip, Path: c.Path, Reason: "informational state, nothing to undo"}}
}

type Result struct {
	Action Action
	Err    error
	Note   string
}

// Apply executes the plan. Non-executing actions are passed through.
func Apply(st *store.Store, acts []Action) []Result {
	var out []Result
	for _, a := range acts {
		r := Result{Action: a}
		switch a.Op {
		case Bootout:
			err := launchctl("bootout", a.Domain+"/"+a.Label)
			if err != nil && (strings.Contains(err.Error(), "No such process") || strings.Contains(err.Error(), "Could not find")) {
				r.Note = "was not loaded"
				err = nil
			}
			r.Err = err
		case Bootstrap:
			r.Err = launchctl("bootstrap", a.Domain, a.Path)
		case Delete:
			r.Err = os.Remove(a.Path)
			if os.IsNotExist(r.Err) {
				r.Err, r.Note = nil, "already gone"
			}
		case DeleteTree:
			r.Err = os.RemoveAll(a.Path)
		case Rmdir:
			if err := os.Remove(a.Path); err != nil && !os.IsNotExist(err) {
				r.Note = "left in place: not empty"
			}
		case Mkdir:
			r.Err = os.MkdirAll(a.Path, os.FileMode(a.Mode&0o7777|0o700))
		case Restore:
			r.Err = restore(st, a)
		case Chmod:
			r.Err = os.Chmod(a.Path, os.FileMode(a.Mode&0o7777))
		case Crontab:
			r.Err = crontab(st, a)
		}
		out = append(out, r)
	}
	return out
}

func restore(st *store.Store, a Action) error {
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
		return err
	}
	if a.Link != "" {
		os.Remove(a.Path)
		return os.Symlink(a.Link, a.Path)
	}
	b, err := st.ReadObject(a.Hash)
	if err != nil {
		return fmt.Errorf("stored content missing (was the repository gc'd?): %w", err)
	}
	tmp := filepath.Join(filepath.Dir(a.Path), ".spoor-restore-"+store.NewID())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, os.FileMode(a.Mode&0o7777)); err != nil {
		os.Remove(tmp)
		return err
	}
	if fi, err := os.Lstat(a.Path); err == nil && fi.IsDir() {
		os.Remove(tmp)
		return fmt.Errorf("a directory is in the way")
	}
	return os.Rename(tmp, a.Path)
}

// Script renders the plan as a POSIX shell script a human can read and run.
func Script(st *store.Store, acts []Action, title string) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\n# " + title + "\n# Generated by spoor. Read before running.\nset -u\n\n")
	for _, a := range acts {
		q := shq(a.Path)
		switch a.Op {
		case Bootout:
			fmt.Fprintf(&sb, "launchctl bootout %s || true\n", shq(a.Domain+"/"+a.Label))
		case Bootstrap:
			fmt.Fprintf(&sb, "launchctl bootstrap %s %s\n", shq(a.Domain), q)
		case Delete:
			fmt.Fprintf(&sb, "rm -f -- %s\n", q)
		case DeleteTree:
			fmt.Fprintf(&sb, "rm -rf -- %s\n", q)
		case Rmdir:
			fmt.Fprintf(&sb, "rmdir -- %s 2>/dev/null || true\n", q)
		case Mkdir:
			fmt.Fprintf(&sb, "mkdir -p -- %s\n", q)
		case Restore:
			if a.Link != "" {
				fmt.Fprintf(&sb, "ln -sfn -- %s %s\n", shq(a.Link), q)
			} else {
				fmt.Fprintf(&sb, "mkdir -p -- %s && cp -- %s %s && chmod %o %s\n", shq(filepath.Dir(a.Path)), shq(st.ObjectPath(a.Hash)), q, a.Mode&0o7777, q)
			}
		case Chmod:
			fmt.Fprintf(&sb, "chmod %o %s\n", a.Mode&0o7777, q)
		case Crontab:
			if a.Hash == "" {
				sb.WriteString("crontab -r\n")
			} else {
				fmt.Fprintf(&sb, "crontab %s\n", shq(st.ObjectPath(a.Hash)))
			}
		default:
			fmt.Fprintf(&sb, "# %s %s: %s\n", a.Op, a.Path, a.Reason)
		}
	}
	return sb.String()
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
