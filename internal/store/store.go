// Package store is spoor's repository: a content-addressed object store
// plus manifests, commits, notes and traces, all under one directory.
package store

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/morass/spoor/internal/model"
)

type Store struct {
	Root string
	lock *os.File
}

// DefaultRoot is $SPOOR_HOME or ~/.local/share/spoor.
func DefaultRoot() string {
	if v := os.Getenv("SPOOR_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "spoor")
}

func Open(root string) (*Store, error) {
	for _, d := range []string{"", "objects", "manifests", "commits", "notes", "traces", "exports", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{Root: root}, nil
}

// Lock takes an exclusive advisory lock so two spoor processes never
// write the same repository at once.
func (s *Store) Lock() error {
	f, err := os.OpenFile(filepath.Join(s.Root, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return errors.New("another spoor process holds the repository lock")
	}
	s.lock = f
	return nil
}

func (s *Store) Unlock() {
	if s.lock != nil {
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
		s.lock = nil
	}
}

func NewID() string {
	b := make([]byte, 5)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) ObjectPath(hash string) string {
	return filepath.Join(s.Root, "objects", hash[:2], hash[2:])
}

func (s *Store) HasObject(hash string) bool {
	_, err := os.Stat(s.ObjectPath(hash))
	return err == nil
}

// PutFile captures the current content of path. It first makes a
// private copy (an APFS clone where possible, so it is instant and
// consistent even if the file changes meanwhile), hashes that copy and
// files it under its hash.
func (s *Store) PutFile(path string) (string, error) {
	tmp := filepath.Join(s.Root, "tmp", NewID())
	if err := cloneOrCopy(path, tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	f, err := os.Open(tmp)
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return sum, s.settle(tmp, sum)
}

func (s *Store) PutBytes(b []byte) (string, error) {
	sum := HashBytes(b)
	if s.HasObject(sum) {
		return sum, nil
	}
	tmp := filepath.Join(s.Root, "tmp", NewID())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	return sum, s.settle(tmp, sum)
}

func (s *Store) settle(tmp, sum string) error {
	dst := s.ObjectPath(sum)
	if s.HasObject(sum) {
		return os.Remove(tmp)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	os.Chmod(tmp, 0o600)
	return os.Rename(tmp, dst)
}

func (s *Store) ReadObject(hash string) ([]byte, error) {
	return os.ReadFile(s.ObjectPath(hash))
}

func HashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- manifests ----

func (s *Store) SaveManifest(m *model.Manifest) error {
	p := filepath.Join(s.Root, "manifests", m.ID+".json.gz")
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if err := json.NewEncoder(zw).Encode(m); err != nil {
		f.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Store) LoadManifest(id string) (*model.Manifest, error) {
	if id == "" {
		return &model.Manifest{}, nil
	}
	f, err := os.Open(filepath.Join(s.Root, "manifests", id+".json.gz"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var m model.Manifest
	if err := json.NewDecoder(zr).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ---- commits ----

func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Store) SaveCommit(c *model.Commit) error {
	return writeJSON(filepath.Join(s.Root, "commits", c.ID+".json"), c)
}

func (s *Store) LoadCommit(id string) (*model.Commit, error) {
	b, err := os.ReadFile(filepath.Join(s.Root, "commits", id+".json"))
	if err != nil {
		return nil, err
	}
	var c model.Commit
	return &c, json.Unmarshal(b, &c)
}

// Commits returns every commit, newest first.
func (s *Store) Commits() ([]*model.Commit, error) {
	ents, err := os.ReadDir(filepath.Join(s.Root, "commits"))
	if err != nil {
		return nil, err
	}
	var out []*model.Commit
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		c, err := s.LoadCommit(strings.TrimSuffix(e.Name(), ".json"))
		if err == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

func (s *Store) Head() string {
	b, _ := os.ReadFile(filepath.Join(s.Root, "HEAD"))
	return strings.TrimSpace(string(b))
}

func (s *Store) SetHead(id string) error {
	return os.WriteFile(filepath.Join(s.Root, "HEAD"), []byte(id+"\n"), 0o600)
}

// Resolve turns HEAD, HEAD~N, a full id or a unique id prefix into a commit.
func (s *Store) Resolve(ref string) (*model.Commit, error) {
	if ref == "" {
		ref = "HEAD"
	}
	if ref == "HEAD" || strings.HasPrefix(ref, "HEAD~") {
		n := 0
		if strings.HasPrefix(ref, "HEAD~") {
			var err error
			if n, err = strconv.Atoi(ref[5:]); err != nil {
				return nil, fmt.Errorf("bad ref %q", ref)
			}
		}
		id := s.Head()
		if id == "" {
			return nil, errors.New("no history yet: run `spoor snap` first")
		}
		for i := 0; i < n; i++ {
			c, err := s.LoadCommit(id)
			if err != nil {
				return nil, err
			}
			if c.Parent == "" {
				return nil, fmt.Errorf("%s: history is shorter than that", ref)
			}
			id = c.Parent
		}
		return s.LoadCommit(id)
	}
	all, err := s.Commits()
	if err != nil {
		return nil, err
	}
	var hit []*model.Commit
	for _, c := range all {
		if c.ID == ref {
			return c, nil
		}
		if strings.HasPrefix(c.ID, ref) {
			hit = append(hit, c)
		}
	}
	switch len(hit) {
	case 0:
		return nil, fmt.Errorf("no commit matches %q", ref)
	case 1:
		return hit[0], nil
	}
	return nil, fmt.Errorf("%q is ambiguous (%d commits)", ref, len(hit))
}

// Chain walks parents from HEAD, newest first.
func (s *Store) Chain() ([]*model.Commit, error) {
	var out []*model.Commit
	id := s.Head()
	seen := map[string]bool{}
	for id != "" && !seen[id] {
		seen[id] = true
		c, err := s.LoadCommit(id)
		if err != nil {
			return out, err
		}
		out = append(out, c)
		id = c.Parent
	}
	return out, nil
}

// ---- notes & traces ----

func (s *Store) Notes(commit string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(filepath.Join(s.Root, "notes", commit+".json"))
	if err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (s *Store) SetNote(commit, path, text string) error {
	m := s.Notes(commit)
	if strings.TrimSpace(text) == "" {
		delete(m, path)
	} else {
		m[path] = text
	}
	return writeJSON(filepath.Join(s.Root, "notes", commit+".json"), m)
}

func (s *Store) SaveTrace(commit string, w map[string][]model.Writer) error {
	return writeJSON(filepath.Join(s.Root, "traces", commit+".json"), w)
}

func (s *Store) Trace(commit string) map[string][]model.Writer {
	m := map[string][]model.Writer{}
	b, err := os.ReadFile(filepath.Join(s.Root, "traces", commit+".json"))
	if err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// ---- config ----

type Config struct {
	Roots      []model.Root `json:"roots,omitempty"`
	NoState    bool         `json:"no_state,omitempty"`
	MaxContent int64        `json:"max_content,omitempty"`
}

func (s *Store) Config() (*Config, bool) {
	var c Config
	b, err := os.ReadFile(filepath.Join(s.Root, "config.json"))
	if err != nil {
		return &c, false
	}
	return &c, json.Unmarshal(b, &c) == nil
}

func (s *Store) SaveConfig(c *Config) error {
	return writeJSON(filepath.Join(s.Root, "config.json"), c)
}

// GC deletes objects no manifest references. Returns objects and bytes freed.
func (s *Store) GC() (int, int64, error) {
	keep := map[string]bool{}
	ents, err := os.ReadDir(filepath.Join(s.Root, "manifests"))
	if err != nil {
		return 0, 0, err
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json.gz") {
			continue
		}
		m, err := s.LoadManifest(strings.TrimSuffix(e.Name(), ".json.gz"))
		if err != nil {
			return 0, 0, fmt.Errorf("manifest %s unreadable, refusing to gc: %w", e.Name(), err)
		}
		for _, en := range m.Entries {
			if en.Stored {
				keep[en.Hash] = true
			}
		}
	}
	n, freed := 0, int64(0)
	err = filepath.WalkDir(filepath.Join(s.Root, "objects"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		h := filepath.Base(filepath.Dir(p)) + d.Name()
		if !keep[h] {
			if fi, err := d.Info(); err == nil {
				freed += fi.Size()
			}
			if os.Remove(p) == nil {
				n++
			}
		}
		return nil
	})
	os.RemoveAll(filepath.Join(s.Root, "tmp"))
	os.MkdirAll(filepath.Join(s.Root, "tmp"), 0o700)
	return n, freed, err
}
