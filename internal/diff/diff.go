// Package diff compares manifests and renders content differences.
package diff

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	udiff "github.com/aymanbagabas/go-udiff"
	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/profile"
	"github.com/morass/spoor/internal/store"
)

// Compare lists what changed from a to b. When the two manifests watched
// different roots, only paths both of them covered are compared, so
// widening or narrowing the watch list never reads as mass add/remove.
func Compare(a, b *model.Manifest) []model.Change {
	restrict := a != nil && b != nil && len(a.Entries) > 0 && !sameRoots(a.Roots, b.Roots)
	am := index(a)
	bm := index(b)
	var out []model.Change
	for p, be := range bm {
		ae, ok := am[p]
		if restrict && !covered(a, p) {
			continue
		}
		if !ok {
			out = append(out, model.Change{Path: p, Kind: model.Added, After: be})
			continue
		}
		if k, changed := classify(ae, be); changed {
			out = append(out, model.Change{Path: p, Kind: k, Before: ae, After: be})
		}
	}
	for p, ae := range am {
		if _, ok := bm[p]; ok {
			continue
		}
		if restrict && !covered(b, p) {
			continue
		}
		if strings.HasPrefix(p, model.StatePrefix) && b != nil && !b.State {
			continue
		}
		out = append(out, model.Change{Path: p, Kind: model.Removed, Before: ae})
	}
	if a != nil && b != nil && a.State != b.State {
		out = dropState(out)
	}
	out = detectRenames(out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func dropState(cs []model.Change) []model.Change {
	var out []model.Change
	for _, c := range cs {
		if !strings.HasPrefix(c.Path, model.StatePrefix) {
			out = append(out, c)
		}
	}
	return out
}

func covered(m *model.Manifest, p string) bool {
	if strings.HasPrefix(p, model.StatePrefix) {
		return m.State
	}
	return profile.CoveredBy(m.Roots, p)
}

func sameRoots(a, b []model.Root) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func index(m *model.Manifest) map[string]*model.Entry {
	out := map[string]*model.Entry{}
	if m == nil {
		return out
	}
	for i := range m.Entries {
		out[m.Entries[i].Path] = &m.Entries[i]
	}
	return out
}

func classify(a, b *model.Entry) (model.ChangeKind, bool) {
	if a.Type != b.Type {
		return model.TypeChange, true
	}
	switch a.Type {
	case model.File:
		if a.Hash != "" && b.Hash != "" {
			if a.Hash != b.Hash {
				return model.Modified, true
			}
		} else if a.Size != b.Size || a.MTime != b.MTime {
			return model.Modified, true
		}
	case model.Symlink:
		if a.Link != b.Link {
			return model.Modified, true
		}
	case model.State:
		if a.Hash != b.Hash {
			return model.Modified, true
		}
		return "", false
	}
	if a.Mode != b.Mode {
		return model.Meta, true
	}
	return "", false
}

func detectRenames(cs []model.Change) []model.Change {
	removed := map[string][]int{}
	for i, c := range cs {
		if c.Kind == model.Removed && c.Before.Type == model.File && c.Before.Hash != "" {
			removed[c.Before.Hash] = append(removed[c.Before.Hash], i)
		}
	}
	drop := map[int]bool{}
	for i, c := range cs {
		if c.Kind != model.Added || c.After.Type != model.File || c.After.Hash == "" {
			continue
		}
		if idx := removed[c.After.Hash]; len(idx) > 0 {
			j := idx[0]
			removed[c.After.Hash] = idx[1:]
			cs[i].Kind = model.Renamed
			cs[i].OldPath = cs[j].Path
			cs[i].Before = cs[j].Before
			drop[j] = true
		}
	}
	var out []model.Change
	for i, c := range cs {
		if !drop[i] {
			out = append(out, c)
		}
	}
	return out
}

// Content loads an entry's body: from the store when captured, or from
// the live file when the manifest came from a hash-only scan (live=true).
func Content(st *store.Store, e *model.Entry, live bool) ([]byte, string) {
	if e == nil {
		return nil, ""
	}
	if !e.Stored {
		switch e.Type {
		case model.Dir:
			return nil, "directory"
		case model.Symlink:
			return []byte("-> " + e.Link + "\n"), ""
		}
		if e.Skipped != "" {
			return nil, "content not captured (" + e.Skipped + ")"
		}
		return nil, "content not captured"
	}
	if st != nil && st.HasObject(e.Hash) {
		b, err := st.ReadObject(e.Hash)
		if err == nil {
			return b, ""
		}
	}
	if live && e.Type == model.File {
		if b, err := os.ReadFile(e.Path); err == nil && store.HashBytes(b) == e.Hash {
			return b, ""
		}
	}
	return nil, "content no longer available"
}

// IsBinary guesses whether bytes are not text.
func IsBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	if bytes.IndexByte(b[:n], 0) >= 0 {
		return true
	}
	return !utf8.Valid(b[:n]) && !utf8.Valid(b[:max(0, n-4)])
}

// Readable turns bytes into the text a human should compare: plists are
// decoded, binaries summarised.
func Readable(b []byte) (string, bool) {
	if IsPlist(b) {
		if t, err := PlistText(b); err == nil {
			return t, true
		}
	}
	if IsBinary(b) {
		return "", false
	}
	return string(b), true
}

// Unified renders the change as a unified diff, or a short explanation
// when there is nothing textual to compare.
func Unified(st *store.Store, c model.Change, live bool) string {
	before, whyB := Content(st, c.Before, live && c.After == nil)
	after, whyA := Content(st, c.After, live)
	name := c.Path
	old := c.Path
	if c.OldPath != "" {
		old = c.OldPath
	}
	var head []string
	if c.Kind == model.Renamed {
		head = append(head, fmt.Sprintf("renamed from %s", old))
	}
	if c.Kind == model.Meta || (c.Before != nil && c.After != nil && c.Before.Mode != c.After.Mode) {
		head = append(head, fmt.Sprintf("mode %s -> %s", modeStr(c.Before), modeStr(c.After)))
	}
	if whyB != "" && c.Before != nil && c.Before.Type != model.Dir {
		head = append(head, "before: "+whyB)
	}
	if whyA != "" && c.After != nil && c.After.Type != model.Dir {
		head = append(head, "after: "+whyA)
	}
	if (c.Before != nil && c.Before.Type == model.Dir) || (c.After != nil && c.After.Type == model.Dir) {
		head = append(head, "directory "+string(c.Kind))
	}
	bt, bok := Readable(before)
	at, aok := Readable(after)
	body := ""
	switch {
	case (before != nil && !bok) || (after != nil && !aok):
		body = fmt.Sprintf("binary content: %s -> %s\n", sizeHash(c.Before, before), sizeHash(c.After, after))
	case before == nil && after == nil:
	default:
		if bt != at {
			body = udiff.Unified("a"+old, "b"+name, bt, at)
		}
	}
	out := strings.Join(head, "\n")
	if out != "" && body != "" {
		out += "\n"
	}
	return out + body
}

func modeStr(e *model.Entry) string {
	if e == nil {
		return "none"
	}
	return fmt.Sprintf("%04o", e.Mode)
}

func sizeHash(e *model.Entry, b []byte) string {
	if e == nil {
		return "none"
	}
	h := e.Hash
	if len(h) > 12 {
		h = h[:12]
	}
	return fmt.Sprintf("%d bytes sha256:%s", e.Size, h)
}
