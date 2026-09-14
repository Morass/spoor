package store

import (
	"os"
	"path/filepath"
	"strings"
)

// Scrub drops stored bodies that sensitive(path, body) flags from every
// manifest (the entry keeps its hash, so history still shows the change)
// and deletes view copies left in tmp. Run GC afterwards to delete the
// objects. Returns how many entries were scrubbed.
func (s *Store) Scrub(sensitive func(path string, body []byte) bool) (int, error) {
	if err := s.Lock(); err != nil {
		return 0, err
	}
	defer s.Unlock()
	ents, err := os.ReadDir(filepath.Join(s.Root, "manifests"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, de := range ents {
		if !strings.HasSuffix(de.Name(), ".json.gz") {
			continue
		}
		m, err := s.LoadManifest(strings.TrimSuffix(de.Name(), ".json.gz"))
		if err != nil {
			return n, err
		}
		changed := false
		for i := range m.Entries {
			e := &m.Entries[i]
			if !e.Stored || e.Hash == "" {
				continue
			}
			body, _ := s.ReadObject(e.Hash)
			if sensitive(e.Path, body) {
				e.Stored, e.Skipped = false, "sensitive"
				changed = true
				n++
			}
		}
		if changed {
			if err := s.SaveManifest(m); err != nil {
				return n, err
			}
		}
	}
	os.RemoveAll(filepath.Join(s.Root, "tmp"))
	return n, os.MkdirAll(filepath.Join(s.Root, "tmp"), 0o700)
}
