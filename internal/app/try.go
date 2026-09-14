package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/redact"
	"github.com/morass/spoor/internal/revert"
	"github.com/morass/spoor/internal/scan"
	"github.com/morass/spoor/internal/store"
	"github.com/morass/spoor/internal/try"
)

// TryRoots picks overlay roots: directories only, nested ones pruned, and
// for a normal user only those inside home (the rest could not be applied).
func (a *App) TryRoots(roots []model.Root) []string {
	var paths []string
	for _, r := range roots {
		fi, err := os.Stat(r.Path)
		if err != nil || !fi.IsDir() {
			continue
		}
		if os.Geteuid() != 0 && !(r.Path == a.Home || strings.HasPrefix(r.Path, a.Home+"/")) {
			continue
		}
		paths = append(paths, r.Path)
	}
	return try.PruneNested(paths)
}

// privateDir makes sure base exists as a directory owned by the current
// user with no group/other access. /var/tmp is shared: another account
// could otherwise pre-create the parent and swap workspaces around.
func privateDir(base string) error {
	if err := os.Mkdir(base, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	fi, err := os.Lstat(base)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o077 != 0 || !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("unsafe try workspace %s (must be a directory owned by you with mode 700); set SPOOR_TRY_DIR", base)
	}
	return nil
}

func tryWorkspace(id string) string {
	base := os.Getenv("SPOOR_TRY_DIR")
	if base == "" {
		base = filepath.Join("/var/tmp", "spoor-try-"+strconv.Itoa(os.Getuid()))
	}
	return filepath.Join(base, id)
}

// Try runs argv against overlays and records the result as a pending try
// commit that does not move HEAD.
func (a *App) Try(argv []string, roots []model.Root, msg string) (*model.Commit, int, error) {
	if err := try.Supported(); err != nil {
		return nil, 0, err
	}
	if len(argv) == 0 {
		return nil, 0, errors.New("nothing to run: spoor try -- <command>")
	}
	paths := a.TryRoots(roots)
	if len(paths) == 0 {
		return nil, 0, errors.New("no usable overlay roots (need existing directories you can write)")
	}
	if err := a.St.Lock(); err != nil {
		return nil, 0, err
	}
	defer a.St.Unlock()
	id := store.NewID()
	cwd, _ := os.Getwd()
	spec := try.Spec{ID: id, Roots: paths, Workspace: tryWorkspace(id), Argv: argv, Cwd: cwd}
	if strings.HasPrefix(spec.Workspace+"/", a.Home+"/") {
		return nil, 0, errors.New("SPOOR_TRY_DIR must be outside the overlaid home directory")
	}
	if err := privateDir(filepath.Dir(spec.Workspace)); err != nil {
		return nil, 0, err
	}
	if err := os.MkdirAll(spec.Workspace, 0o700); err != nil {
		return nil, 0, err
	}
	start := time.Now()
	exit, err := try.Run(spec)
	if err != nil {
		os.RemoveAll(spec.Workspace)
		return nil, exit, err
	}
	changes, err := try.Changes(spec)
	if err != nil {
		return nil, exit, err
	}
	pre, post := a.tryManifests(paths, changes)
	if err := a.St.SaveManifest(pre); err != nil {
		return nil, exit, err
	}
	if err := a.St.SaveManifest(post); err != nil {
		return nil, exit, err
	}
	if msg == "" {
		msg = strings.Join(redact.Argv(argv), " ")
	} else {
		msg, _ = redact.Text(msg, "")
	}
	c := a.newCommit(model.KindTry, msg, pre, post)
	c.Command, c.Cwd, c.ExitCode, c.Duration = redact.Argv(argv), cwd, exit, time.Since(start).Seconds()
	c.TryState, c.Workspace = "pending", spec.Workspace
	return c, exit, a.save(c, false)
}

// tryManifests builds partial before/after manifests covering only the
// paths the command touched, so the normal review and diff tools work.
func (a *App) tryManifests(paths []string, changes []try.Change) (*model.Manifest, *model.Manifest) {
	host, _ := os.Hostname()
	var roots []model.Root
	for _, p := range paths {
		roots = append(roots, model.Root{Path: p, Depth: -1, Content: true})
	}
	mk := func() *model.Manifest {
		return &model.Manifest{ID: store.NewID(), Created: time.Now().UTC(), Host: host, OS: a.GOOS, Home: a.Home, Roots: roots}
	}
	pre, post := mk(), mk()
	max := a.maxContent()
	seen := map[string]bool{}
	addPre := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		if e, ok := scan.EntryFor(a.St, p, "", true, max); ok {
			pre.Entries = append(pre.Entries, e)
		}
	}
	for _, c := range changes {
		switch c.Kind {
		case try.Removed:
			addPre(c.Path)
		case try.Modified:
			addPre(c.Path)
			if e, ok := scan.EntryFor(a.St, c.Path, c.Upper, true, max); ok {
				post.Entries = append(post.Entries, e)
			}
		case try.Added:
			if e, ok := scan.EntryFor(a.St, c.Path, c.Upper, true, max); ok {
				post.Entries = append(post.Entries, e)
			}
		case try.Opaque:
			for _, g := range try.OpaqueRemovals(c) {
				addPre(g)
			}
		}
	}
	sortEntries(pre)
	sortEntries(post)
	return pre, post
}

func sortEntries(m *model.Manifest) {
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
}

func (a *App) loadTry(ref string) (*model.Commit, try.Spec, error) {
	var spec try.Spec
	c, err := a.St.Resolve(ref)
	if err != nil {
		return nil, spec, err
	}
	if c.Kind != model.KindTry {
		return nil, spec, fmt.Errorf("%s is a %s commit, not a try", c.ID, c.Kind)
	}
	if c.TryState != "pending" {
		return nil, spec, fmt.Errorf("try %s is already %s", c.ID, c.TryState)
	}
	if err := privateDir(filepath.Dir(c.Workspace)); err != nil {
		return nil, spec, err
	}
	b, err := os.ReadFile(filepath.Join(c.Workspace, "spec.json"))
	if err != nil {
		return nil, spec, fmt.Errorf("try workspace is gone (%s): %w", c.Workspace, err)
	}
	return c, spec, jsonUnmarshal(b, &spec)
}

// TryApply writes a pending try onto the real files as a recorded commit.
func (a *App) TryApply(ref string, force bool) (*model.Commit, error) {
	if err := try.Supported(); err != nil {
		return nil, err
	}
	c, spec, err := a.loadTry(ref)
	if err != nil {
		return nil, err
	}
	pre, _, _, err := a.CommitChanges(c)
	if err != nil {
		return nil, err
	}
	changes, err := try.Changes(spec)
	if err != nil {
		return nil, err
	}
	if !force {
		idx := map[string]*model.Entry{}
		for i := range pre.Entries {
			idx[pre.Entries[i].Path] = &pre.Entries[i]
		}
		var stale []string
		for _, ch := range changes {
			if e := idx[ch.Path]; e != nil && e.Type == model.File && e.Hash != "" {
				if h, err := store.HashFile(ch.Path); err != nil || h != e.Hash {
					stale = append(stale, a.Tilde(ch.Path))
				}
			} else if e == nil && (ch.Kind == try.Added) {
				if _, err := os.Lstat(ch.Path); err == nil && !ch.IsDir {
					stale = append(stale, a.Tilde(ch.Path))
				}
			}
		}
		if len(stale) > 0 {
			return nil, fmt.Errorf("these files changed since the try ran (use --force to overwrite): %s", strings.Join(stale, ", "))
		}
	}
	if err := a.St.Lock(); err != nil {
		return nil, err
	}
	roots, state := a.Roots(nil), a.State(false)
	base, err := a.baseline(roots, state)
	if err != nil {
		a.St.Unlock()
		return nil, err
	}
	idx := map[string]*model.Entry{}
	for i := range pre.Entries {
		idx[pre.Entries[i].Path] = &pre.Entries[i]
	}
	mayRemove := func(p string) bool {
		if force {
			return true
		}
		e := idx[p]
		return e != nil && revert.Matches(p, e)
	}
	if err := try.Apply(spec, changes, mayRemove); err != nil {
		a.St.Unlock()
		return nil, fmt.Errorf("apply failed part-way (history not updated; inspect with spoor status): %w", err)
	}
	post, ws, err := a.Scan(roots, state, base, false)
	if err != nil {
		a.St.Unlock()
		return nil, err
	}
	a.warn(ws)
	nc := a.newCommit(model.KindRun, "apply try "+c.ID+": "+c.Message, base, post)
	nc.Command, nc.Cwd, nc.ExitCode = c.Command, c.Cwd, c.ExitCode
	if err := a.save(nc, true); err != nil {
		a.St.Unlock()
		return nil, err
	}
	a.St.Unlock()
	c.TryState = "applied"
	a.St.SaveCommit(c)
	os.RemoveAll(spec.Workspace)
	return nc, nil
}

func (a *App) TryDiscard(ref string) error {
	c, err := a.St.Resolve(ref)
	if err != nil {
		return err
	}
	if c.Kind != model.KindTry || c.TryState != "pending" {
		return fmt.Errorf("%s is not a pending try", c.ID)
	}
	if c.Workspace != "" {
		os.RemoveAll(c.Workspace)
	}
	c.TryState = "discarded"
	return a.St.SaveCommit(c)
}
