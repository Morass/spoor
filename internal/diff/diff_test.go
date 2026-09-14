package diff

import (
	"strings"
	"testing"

	"github.com/morass/spoor/internal/model"
	"howett.net/plist"
)

func man(roots []model.Root, es ...model.Entry) *model.Manifest {
	return &model.Manifest{Roots: roots, Entries: es}
}

func kinds(cs []model.Change) map[string]model.ChangeKind {
	out := map[string]model.ChangeKind{}
	for _, c := range cs {
		out[c.Path] = c.Kind
	}
	return out
}

func TestCompareKinds(t *testing.T) {
	roots := []model.Root{{Path: "/h", Depth: -1, Content: true}}
	a := man(roots,
		model.Entry{Path: "/h", Type: model.Dir, Mode: 0o755},
		model.Entry{Path: "/h/keep", Type: model.File, Hash: "k", Mode: 0o644},
		model.Entry{Path: "/h/mod", Type: model.File, Hash: "m1", Mode: 0o644},
		model.Entry{Path: "/h/gone", Type: model.File, Hash: "g", Mode: 0o644},
		model.Entry{Path: "/h/chmod", Type: model.File, Hash: "c", Mode: 0o644},
		model.Entry{Path: "/h/old", Type: model.File, Hash: "r", Mode: 0o644},
		model.Entry{Path: "/h/meta-only", Type: model.File, Size: 10, MTime: 1},
		model.Entry{Path: "/h/ln", Type: model.Symlink, Link: "a"},
	)
	b := man(roots,
		model.Entry{Path: "/h", Type: model.Dir, Mode: 0o755},
		model.Entry{Path: "/h/keep", Type: model.File, Hash: "k", Mode: 0o644},
		model.Entry{Path: "/h/mod", Type: model.File, Hash: "m2", Mode: 0o644},
		model.Entry{Path: "/h/new", Type: model.File, Hash: "n", Mode: 0o644},
		model.Entry{Path: "/h/chmod", Type: model.File, Hash: "c", Mode: 0o600},
		model.Entry{Path: "/h/renamed", Type: model.File, Hash: "r", Mode: 0o644},
		model.Entry{Path: "/h/meta-only", Type: model.File, Size: 10, MTime: 2},
		model.Entry{Path: "/h/ln", Type: model.Symlink, Link: "b"},
	)
	got := kinds(Compare(a, b))
	want := map[string]model.ChangeKind{
		"/h/mod": model.Modified, "/h/gone": model.Removed, "/h/new": model.Added,
		"/h/chmod": model.Meta, "/h/renamed": model.Renamed, "/h/meta-only": model.Modified, "/h/ln": model.Modified,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for p, k := range want {
		if got[p] != k {
			t.Errorf("%s: got %s want %s", p, got[p], k)
		}
	}
	for _, c := range Compare(a, b) {
		if c.Kind == model.Renamed && c.OldPath != "/h/old" {
			t.Errorf("rename source = %q", c.OldPath)
		}
	}
}

func TestCompareDifferentRootsOnlyComparesSharedCoverage(t *testing.T) {
	a := man([]model.Root{{Path: "/h", Depth: 1}},
		model.Entry{Path: "/h", Type: model.Dir},
		model.Entry{Path: "/h/x", Type: model.File, Hash: "1"})
	b := man([]model.Root{{Path: "/h", Depth: 1}, {Path: "/opt", Depth: 1}},
		model.Entry{Path: "/h", Type: model.Dir},
		model.Entry{Path: "/h/x", Type: model.File, Hash: "2"},
		model.Entry{Path: "/opt", Type: model.Dir},
		model.Entry{Path: "/opt/tool", Type: model.File, Hash: "t"})
	got := kinds(Compare(a, b))
	if len(got) != 1 || got["/h/x"] != model.Modified {
		t.Fatalf("widening roots must not report /opt as added: %v", got)
	}
}

func TestStateIgnoredWhenOneSideHasNone(t *testing.T) {
	a := man(nil, model.Entry{Path: "@state/crontab", Type: model.State, Hash: "1"})
	a.State = true
	b := man(nil)
	if cs := Compare(a, b); len(cs) != 0 {
		t.Fatalf("state disappearing because collection was off is not a change: %v", cs)
	}
}

func TestPlistTextSameForBinaryAndXML(t *testing.T) {
	v := map[string]any{"Label": "x.y", "RunAtLoad": true, "ProgramArguments": []string{"/bin/a", "-b"}, "StartInterval": 60}
	xml, err := plist.MarshalIndent(v, plist.XMLFormat, "  ")
	if err != nil {
		t.Fatal(err)
	}
	bin, err := plist.Marshal(v, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	if !IsPlist(xml) || !IsPlist(bin) {
		t.Fatal("plist detection")
	}
	tx, err1 := PlistText(xml)
	tb, err2 := PlistText(bin)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if tx != tb {
		t.Fatalf("binary and XML render differently:\n%s\n---\n%s", tx, tb)
	}
	if !strings.Contains(tx, `Label = "x.y"`) || !strings.Contains(tx, `- "/bin/a"`) {
		t.Fatalf("unexpected rendering:\n%s", tx)
	}
}

func TestIsBinary(t *testing.T) {
	if IsBinary([]byte("hello\nworld ✓\n")) {
		t.Error("utf-8 text flagged binary")
	}
	if !IsBinary([]byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0, 1}) {
		t.Error("Mach-O not flagged binary")
	}
}
