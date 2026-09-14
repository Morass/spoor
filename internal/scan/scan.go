// Package scan records what the watched roots look like right now.
package scan

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/store"
)

const DefaultMaxContent = 1 << 20

type Options struct {
	Roots      []model.Root
	MaxContent int64
	// Store receives file bodies. When nil the scan only hashes (used by
	// `status` and `diff ... now`, which must not grow the repository).
	Store *store.Store
	// Prev lets unchanged files (same size, mtime, mode) reuse their hash
	// without being read again.
	Prev  *model.Manifest
	// HashOnly hashes file bodies without storing them (state text, which
	// is tiny and cannot be re-read later, is still stored).
	HashOnly bool
	State    bool
	// Skip excludes paths (the repository itself, a try workspace).
	Skip []string
}

type Result struct {
	Manifest *model.Manifest
	Warnings []string
}

var sensitive = regexp.MustCompile(`(?i)(/\.ssh/id_[^/]*$|/\.ssh/.*_key$|\.pem$|\.key$|\.p12$|\.pfx$|/\.netrc$|/\.pgpass$|/\.gnupg/|/credentials(\.json)?$|\.keychain(-db)?$|/\.aws/|/\.docker/config\.json$|/\.kube/config$|/secrets?(\.|/|$)|token)`)

// Sensitive reports whether a path's body must never be copied into the store.
func Sensitive(path string) bool { return sensitive.MatchString(path) }

func Scan(opt Options) (*Result, error) {
	if opt.MaxContent == 0 {
		opt.MaxContent = DefaultMaxContent
	}
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	m := &model.Manifest{
		ID: store.NewID(), Created: time.Now().UTC(), Host: host, OS: runtime.GOOS,
		Home: home, Roots: opt.Roots, State: opt.State,
	}
	prev := map[string]*model.Entry{}
	if opt.Prev != nil {
		for i := range opt.Prev.Entries {
			prev[opt.Prev.Entries[i].Path] = &opt.Prev.Entries[i]
		}
	}
	s := &scanner{opt: opt, prev: prev, seen: map[string]int{}, m: m}
	for _, root := range opt.Roots {
		s.walkRoot(root)
	}
	if opt.State {
		for _, c := range collectors() {
			text, err := c.run()
			if err != nil {
				s.warn("state " + c.name + ": " + err.Error())
				continue
			}
			e := model.Entry{Path: model.StatePrefix + c.name, Type: model.State, Size: int64(len(text))}
			e.Hash = store.HashBytes([]byte(text))
			if opt.Store != nil {
				if _, err := opt.Store.PutBytes([]byte(text)); err == nil {
					e.Stored = true
				}
			} else {
				e.Stored = true // content is reproducible from the live query
			}
			s.add(e, true)
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return &Result{Manifest: m, Warnings: s.warnings}, nil
}

type scanner struct {
	opt      Options
	prev     map[string]*model.Entry
	seen     map[string]int
	m        *model.Manifest
	warnings []string
	tccWarn  bool
}

func (s *scanner) warn(w string) {
	if len(s.warnings) < 50 {
		s.warnings = append(s.warnings, w)
	}
}

// add merges entries from overlapping roots: the richer record wins.
func (s *scanner) add(e model.Entry, content bool) {
	if i, ok := s.seen[e.Path]; ok {
		if !s.m.Entries[i].Stored && e.Stored {
			s.m.Entries[i] = e
		}
		return
	}
	s.seen[e.Path] = len(s.m.Entries)
	s.m.Entries = append(s.m.Entries, e)
}

func (s *scanner) skipped(p string) bool {
	for _, sk := range s.opt.Skip {
		if p == sk || strings.HasPrefix(p, strings.TrimSuffix(sk, "/")+"/") {
			return true
		}
	}
	return false
}

func (s *scanner) walkRoot(root model.Root) {
	fi, err := os.Lstat(root.Path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.permWarn(root.Path, err)
		}
		return
	}
	s.visit(root, root.Path, fi, 0)
}

func (s *scanner) permWarn(p string, err error) {
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EPERM) {
		if runtime.GOOS == "darwin" && !s.tccWarn && strings.Contains(err.Error(), "operation not permitted") {
			s.tccWarn = true
			s.warn("some folders are protected by macOS privacy controls; grant your terminal Full Disk Access to watch them (first: " + p + ")")
			return
		}
		s.warn("unreadable: " + p)
		return
	}
	s.warn(p + ": " + err.Error())
}

func (s *scanner) visit(root model.Root, p string, fi fs.FileInfo, depth int) {
	if s.skipped(p) {
		return
	}
	e := model.Entry{Path: p, Mode: uint32(fi.Mode().Perm()) | uint32(fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))}
	switch {
	case fi.Mode().IsRegular():
		e.Type = model.File
		e.Size = fi.Size()
		e.MTime = fi.ModTime().UnixNano()
		s.fileContent(root, &e)
	case fi.IsDir():
		e.Type = model.Dir
	case fi.Mode()&fs.ModeSymlink != 0:
		e.Type = model.Symlink
		e.Link, _ = os.Readlink(p)
	default:
		e.Type = model.Other
	}
	s.add(e, root.Content)
	if !fi.IsDir() || (root.Depth >= 0 && depth >= root.Depth) {
		return
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		s.permWarn(p, err)
		return
	}
	for _, de := range ents {
		cp := filepath.Join(p, de.Name())
		cfi, err := os.Lstat(cp)
		if err != nil {
			continue
		}
		s.visit(root, cp, cfi, depth+1)
	}
}

func (s *scanner) fileContent(root model.Root, e *model.Entry) {
	if !root.Content {
		e.Skipped = "metadata"
		return
	}
	if e.Size > s.opt.MaxContent {
		e.Skipped = "size"
		return
	}
	sens := Sensitive(e.Path)
	if pe, ok := s.prev[e.Path]; ok && pe.Type == model.File && pe.Size == e.Size && pe.MTime == e.MTime && pe.Mode == e.Mode && pe.Hash != "" {
		if sens || (pe.Stored && (s.opt.Store == nil || s.opt.HashOnly || s.opt.Store.HasObject(pe.Hash))) {
			e.Hash, e.Stored, e.Skipped = pe.Hash, pe.Stored, pe.Skipped
			return
		}
	}
	if sens || s.opt.Store == nil || s.opt.HashOnly {
		h, err := store.HashFile(e.Path)
		if err != nil {
			e.Skipped = "unreadable"
			return
		}
		e.Hash = h
		if sens {
			e.Skipped = "sensitive"
		} else {
			e.Stored = true // hash-only scan; body is the live file
		}
		return
	}
	h, err := s.opt.Store.PutFile(e.Path)
	if err != nil {
		e.Skipped = "unreadable"
		return
	}
	e.Hash, e.Stored = h, true
}

// EntryFor records one path (optionally reading it from src instead) as a
// manifest entry, storing its body when content is true.
func EntryFor(st *store.Store, path, src string, content bool, maxContent int64) (model.Entry, bool) {
	if src == "" {
		src = path
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return model.Entry{}, false
	}
	if maxContent == 0 {
		maxContent = DefaultMaxContent
	}
	e := model.Entry{Path: path, Mode: uint32(fi.Mode().Perm()) | uint32(fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))}
	switch {
	case fi.Mode().IsRegular():
		e.Type, e.Size, e.MTime = model.File, fi.Size(), fi.ModTime().UnixNano()
		switch {
		case !content:
			e.Skipped = "metadata"
		case e.Size > maxContent:
			e.Skipped = "size"
		case Sensitive(path):
			if h, err := store.HashFile(src); err == nil {
				e.Hash = h
			}
			e.Skipped = "sensitive"
		default:
			if h, err := st.PutFile(src); err == nil {
				e.Hash, e.Stored = h, true
			} else {
				e.Skipped = "unreadable"
			}
		}
	case fi.IsDir():
		e.Type = model.Dir
	case fi.Mode()&fs.ModeSymlink != 0:
		e.Type = model.Symlink
		e.Link, _ = os.Readlink(src)
	default:
		e.Type = model.Other
	}
	return e, true
}
