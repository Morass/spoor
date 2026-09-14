// Package scan records what the watched roots look like right now.
package scan

import (
	"errors"
	"fmt"
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
	"github.com/morass/spoor/internal/redact"
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

// File names whose bodies are credentials, keys or private history.
var sensitiveName = regexp.MustCompile(`(?i)^(id_[^/]*|.*_key|.*\.(pem|key|p8|p12|pfx|jks|keystore|kdbx|keychain|keychain-db|gpg|asc|age|ovpn)|\.netrc|\.pgpass|\.my\.cnf|\.npmrc|\.yarnrc(\.yml)?|\.pypirc|\.git-credentials|\.gem-credentials|\.env(\..*)?|\.envrc|\.vault-token|\.s3cfg|\.boto|\.?secrets?(\..*)?|credentials(\..*)?|.*token.*|.*passw(or)?d.*|shadow-?|gshadow-?|master\.key|\.htpasswd|.*_history|\.viminfo|\.lesshst|hosts\.ya?ml|application_default_credentials\.json|access_tokens\.db|credentials\.db|keys\.txt|rclone\.conf)$`)

// Folders whose every file is treated as secret.
var sensitiveDir = regexp.MustCompile(`(?i)/(\.ssh|\.gnupg|\.aws|\.azure|\.kube|\.docker|\.password-store|\.config/gh|\.config/gcloud|\.config/op|\.config/rclone|\.config/sops|\.config/doctl|\.config/configstore|\.terraform\.d|\.local/share/keyrings|Library/Keychains|etc/ssl/private)/`)

// Sensitive reports whether a path's body must never be copied into the
// store. It looks at the file NAME and at a list of known credential
// folders, never at arbitrary directory names (a folder called "secret"
// does not make its children secret). Content is checked separately.
func Sensitive(path string) bool {
	base := filepath.Base(path)
	if sensitiveName.MatchString(base) {
		return true
	}
	if strings.HasPrefix(path, "/etc/ssh/ssh_host_") && !strings.HasSuffix(base, ".pub") {
		return true
	}
	if sensitiveDir.MatchString(path) {
		if strings.Contains(path, "/.ssh/") && (base == "config" || strings.HasPrefix(base, "known_hosts") || strings.HasPrefix(base, "authorized_keys") || strings.HasSuffix(base, ".pub")) {
			return false
		}
		return true
	}
	return false
}

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
			switch {
			case redact.ContainsSecret([]byte(text)):
				e.Skipped = "sensitive" // e.g. a crontab line carrying a token
			case opt.Store != nil:
				if _, err := opt.Store.PutBytes([]byte(text)); err == nil {
					e.Stored = true
				}
			default:
				e.Stored = true // content is reproducible from the live query
			}
			s.add(e, true)
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	switch n := len(s.unreadable); {
	case n > 0 && n <= 3:
		for _, p := range s.unreadable {
			s.warnings = append(s.warnings, "unreadable: "+p)
		}
	case n > 3:
		s.warnings = append(s.warnings, fmt.Sprintf("%d paths unreadable without more privileges (e.g. %s)", n, strings.Join(s.unreadable[:3], ", ")))
	}
	if s.tcc > 0 {
		s.warnings = append(s.warnings, fmt.Sprintf("%d folder(s) are protected by macOS privacy controls and were skipped (e.g. %s); grant your terminal Full Disk Access to watch them", s.tcc, s.tccFirst))
	}
	return &Result{Manifest: m, Warnings: s.warnings}, nil
}

type scanner struct {
	opt      Options
	prev     map[string]*model.Entry
	seen     map[string]int
	m        *model.Manifest
	warnings []string
	tcc        int
	tccFirst   string
	unreadable []string
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
	if runtime.GOOS == "darwin" && strings.Contains(err.Error(), "operation not permitted") {
		if s.tcc == 0 {
			s.tccFirst = p
		}
		s.tcc++
		return
	}
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EPERM) {
		s.unreadable = append(s.unreadable, p)
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
		if sens || strings.HasPrefix(pe.Skipped, "sensitive") || (pe.Stored && (s.opt.Store == nil || s.opt.HashOnly || s.opt.Store.HasObject(pe.Hash))) {
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
	h, stored, err := s.opt.Store.PutFileFiltered(e.Path, redact.ContainsSecret)
	if err != nil {
		e.Skipped = "unreadable"
		return
	}
	e.Hash, e.Stored = h, stored
	if !stored {
		e.Skipped = "sensitive content"
	}
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
			if h, stored, err := st.PutFileFiltered(src, redact.ContainsSecret); err != nil {
				e.Skipped = "unreadable"
			} else if e.Hash, e.Stored = h, stored; !stored {
				e.Skipped = "sensitive content"
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
