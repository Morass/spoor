package app

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/morass/spoor/internal/kb"
	"github.com/morass/spoor/internal/model"
)

// A package manager upgrade does not modify files in place: it creates a
// new version directory next to the old one (Cellar/tree/2.2.1 → 2.3.2)
// and usually deletes the old one. Listed raw, that is "a directory added,
// a directory removed" — true but useless. Pairing the two directories
// turns it into what a person means by the upgrade: these files changed.

var versionDir = regexp.MustCompile(`^v?\d+(\.\d+)*([._+-][0-9A-Za-z]+)*$`)

const upgradeExplain = "The same file in the old and the new version directory, compared. On disk the upgrade created a new version directory (and usually deleted the old one); those raw entries are hidden under noise (press . to show them) and are what revert acts on."

type versionPair struct{ parent, old, new string }

var versionPart = regexp.MustCompile(`\d+|\D+`)

func versionLess(a, b string) bool {
	as, bs := versionPart.FindAllString(a, -1), versionPart.FindAllString(b, -1)
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		ai, aerr := strconv.Atoi(as[i])
		bi, berr := strconv.Atoi(bs[i])
		if aerr == nil && berr == nil {
			return ai < bi
		}
		return as[i] < bs[i]
	}
	return len(as) < len(bs)
}

// versionPairs finds version-named directories a commit added that have an
// older sibling: preferably one the same commit removed, else one that was
// already there before.
func versionPairs(pre, post *model.Manifest, changes []model.Change) []versionPair {
	if pre == nil || post == nil {
		return nil
	}
	removed := map[string][]string{}
	for _, c := range changes {
		if c.Kind == model.Removed && c.Before != nil && c.Before.Type == model.Dir && versionDir.MatchString(filepath.Base(c.Path)) {
			removed[filepath.Dir(c.Path)] = append(removed[filepath.Dir(c.Path)], filepath.Base(c.Path))
		}
	}
	existing := map[string][]string{}
	for _, e := range pre.Entries {
		if e.Type == model.Dir && versionDir.MatchString(filepath.Base(e.Path)) {
			existing[filepath.Dir(e.Path)] = append(existing[filepath.Dir(e.Path)], filepath.Base(e.Path))
		}
	}
	var out []versionPair
	for _, c := range changes {
		if c.Kind != model.Added || c.After == nil || c.After.Type != model.Dir || !versionDir.MatchString(filepath.Base(c.Path)) {
			continue
		}
		parent, name := filepath.Dir(c.Path), filepath.Base(c.Path)
		cands := removed[parent]
		if len(cands) == 0 {
			cands = existing[parent]
		}
		old := ""
		for _, cand := range cands {
			if cand != name && (old == "" || versionLess(old, cand)) {
				old = cand
			}
		}
		if old != "" {
			out = append(out, versionPair{parent, old, name})
		}
	}
	return out
}

func entriesUnder(m *model.Manifest, dir string) map[string]*model.Entry {
	out := map[string]*model.Entry{}
	prefix := dir + "/"
	for i := range m.Entries {
		e := &m.Entries[i]
		if (e.Type == model.File || e.Type == model.Symlink) && strings.HasPrefix(e.Path, prefix) {
			out[e.Path[len(prefix):]] = e
		}
	}
	return out
}

func sameContent(a, b *model.Entry) bool {
	if a.Type != b.Type {
		return false
	}
	if a.Type == model.Symlink {
		return a.Link == b.Link
	}
	if a.Hash != "" && b.Hash != "" {
		return a.Hash == b.Hash
	}
	return a.Size == b.Size // metadata only: best available guess
}

// View is Items plus upgrade pairing; use it wherever a commit's before and
// after manifests are known.
func (a *App) View(pre, post *model.Manifest, changes []model.Change) []Item {
	items := a.Items(changes)
	pairs := versionPairs(pre, post, changes)
	if len(pairs) == 0 {
		return items
	}
	var up []Item
	hide := map[string]string{}
	for _, vp := range pairs {
		oldDir, newDir := filepath.Join(vp.parent, vp.old), filepath.Join(vp.parent, vp.new)
		label := fmt.Sprintf("%s %s → %s", filepath.Base(vp.parent), vp.old, vp.new)
		hide[oldDir], hide[newDir] = label, label
		olds, news := entriesUnder(pre, oldDir), entriesUnder(post, newDir)
		rels := map[string]bool{}
		for r := range olds {
			rels[r] = true
		}
		for r := range news {
			rels[r] = true
		}
		sorted := make([]string, 0, len(rels))
		for r := range rels {
			sorted = append(sorted, r)
		}
		sort.Strings(sorted)
		rule := kb.Rule{Category: kb.Upgrade, Risk: kb.Notice, Title: label, Explain: upgradeExplain}
		if len(sorted) == 0 {
			up = append(up, Item{Change: model.Change{Path: newDir, OldPath: oldDir, Kind: model.Modified}, Rule: rule, Virtual: true,
				Label: "(version folders only: files were not captured)"})
			continue
		}
		for _, rel := range sorted {
			o, n := olds[rel], news[rel]
			var ch model.Change
			switch {
			case o != nil && n != nil:
				if sameContent(o, n) {
					continue
				}
				ch = model.Change{Path: filepath.Join(newDir, rel), OldPath: filepath.Join(oldDir, rel), Kind: model.Modified, Before: o, After: n}
			case n != nil:
				ch = model.Change{Path: filepath.Join(newDir, rel), Kind: model.Added, After: n}
			default:
				ch = model.Change{Path: filepath.Join(oldDir, rel), Kind: model.Removed, Before: o}
			}
			up = append(up, Item{Change: ch, Rule: rule, Virtual: true, Label: rel})
		}
	}
	for i := range items {
		for dir, label := range hide {
			if items[i].Path == dir || strings.HasPrefix(items[i].Path, dir+"/") {
				if items[i].Path != dir {
					items[i].Rule = kb.Rule{Category: kb.Noise, Title: "inside " + label, Explain: upgradeExplain}
				}
			}
		}
	}
	all := append(up, items...)
	sort.SliceStable(all, func(i, j int) bool {
		ri, rj := kb.Rank(all[i].Rule.Category), kb.Rank(all[j].Rule.Category)
		if ri != rj {
			return ri < rj
		}
		if all[i].Virtual != all[j].Virtual {
			return all[i].Virtual
		}
		if all[i].Virtual {
			return false // keep file order within a pair
		}
		return all[i].Path < all[j].Path
	})
	return all
}
