// Package try runs a command against copy-on-write overlays of the
// watched roots (Linux), so its effects can be reviewed before they touch
// the real files — then applied or thrown away.
package try

import "path/filepath"

type Spec struct {
	ID        string   `json:"id"`
	Roots     []string `json:"roots"`
	Workspace string   `json:"workspace"`
	Argv      []string `json:"argv"`
	Cwd       string   `json:"cwd"`
	UserNS    bool     `json:"userns"`
}

type ChangeKind string

const (
	Added    ChangeKind = "added"
	Modified ChangeKind = "modified"
	Removed  ChangeKind = "removed"
	// Opaque: a directory was deleted and recreated; lower children not
	// present in the upper layer are gone.
	Opaque ChangeKind = "opaque"
)

type Change struct {
	Path  string     // real path
	Upper string     // path inside the upper layer ("" for removals)
	Kind  ChangeKind //
	IsDir bool
}

func (s Spec) Upper(i int) string { return filepath.Join(s.Workspace, "upper", itoa(i)) }
func (s Spec) Work(i int) string  { return filepath.Join(s.Workspace, "work", itoa(i)) }

func itoa(i int) string {
	const d = "0123456789"
	if i < 10 {
		return d[i : i+1]
	}
	return itoa(i/10) + d[i%10:i%10+1]
}

// PruneNested drops roots inside other roots: an overlay on the parent
// already covers them.
func PruneNested(roots []string) []string {
	var out []string
	for _, r := range roots {
		nested := false
		for _, o := range roots {
			if o != r && len(r) > len(o) && r[:len(o)] == o && (o == "/" || r[len(o)] == '/') {
				nested = true
			}
		}
		if !nested {
			out = append(out, filepath.Clean(r))
		}
	}
	return out
}
