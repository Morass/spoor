package profile

import (
	"testing"

	"github.com/morass/spoor/internal/model"
)

func TestCovers(t *testing.T) {
	r := model.Root{Path: "/h", Depth: 1}
	for p, want := range map[string]bool{"/h": true, "/h/a": true, "/h/a/b": false, "/hx": false, "/other": false} {
		if Covers(r, p) != want {
			t.Errorf("depth 1 %s: want %v", p, want)
		}
	}
	if !Covers(model.Root{Path: "/h", Depth: -1}, "/h/a/b/c") {
		t.Error("unlimited depth")
	}
	if Covers(model.Root{Path: "/h/f", Depth: 0}, "/h/f/x") {
		t.Error("depth 0 covers only the root")
	}
}

func TestParse(t *testing.T) {
	r, err := Parse("/opt/x:2:meta")
	if err != nil || r.Path != "/opt/x" || r.Depth != 2 || r.Content {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := Parse("/x:deep"); err == nil {
		t.Error("bad depth accepted")
	}
	r, _ = Parse("/y")
	if r.Depth != -1 || !r.Content {
		t.Errorf("defaults: %+v", r)
	}
}
