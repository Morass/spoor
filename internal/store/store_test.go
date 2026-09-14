package store

import (
	"strings"
	"testing"
	"time"

	"github.com/morass/spoor/internal/model"
)

func TestResolveAndGC(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keep, _ := st.PutBytes([]byte("referenced"))
	drop, _ := st.PutBytes([]byte("orphan"))
	m := &model.Manifest{ID: "m1", Entries: []model.Entry{{Path: "/a", Type: model.File, Hash: keep, Stored: true}}}
	if err := st.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c1 := &model.Commit{ID: "aaaa1111", Kind: model.KindSnap, Time: now, Post: "m1"}
	c2 := &model.Commit{ID: "aaaa2222", Parent: c1.ID, Kind: model.KindSnap, Time: now.Add(time.Second), Post: "m1"}
	st.SaveCommit(c1)
	st.SaveCommit(c2)
	st.SetHead(c2.ID)

	if c, err := st.Resolve("HEAD~1"); err != nil || c.ID != c1.ID {
		t.Errorf("HEAD~1: %v %v", c, err)
	}
	if _, err := st.Resolve("HEAD~2"); err == nil {
		t.Error("HEAD~2 beyond history accepted")
	}
	if c, err := st.Resolve("aaaa2"); err != nil || c.ID != c2.ID {
		t.Errorf("prefix: %v %v", c, err)
	}
	if _, err := st.Resolve("aaaa"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous prefix: %v", err)
	}
	n, _, err := st.GC()
	if err != nil || n != 1 || !st.HasObject(keep) || st.HasObject(drop) {
		t.Errorf("gc: n=%d err=%v keep=%v drop=%v", n, err, st.HasObject(keep), st.HasObject(drop))
	}
}

func TestPutFileMatchesContentHash(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir + "/repo")
	p := dir + "/f"
	if err := writeFile(p, "hello"); err != nil {
		t.Fatal(err)
	}
	h, err := st.PutFile(p)
	if err != nil || h != HashBytes([]byte("hello")) {
		t.Fatalf("PutFile hash %s err %v", h, err)
	}
	b, _ := st.ReadObject(h)
	if string(b) != "hello" {
		t.Errorf("object content %q", b)
	}
}

func TestScrubDropsSecretBodies(t *testing.T) {
	st, _ := Open(t.TempDir())
	secret, _ := st.PutBytes([]byte("SECRET-BODY"))
	plain, _ := st.PutBytes([]byte("plain"))
	m := &model.Manifest{ID: "m1", Entries: []model.Entry{
		{Path: "/h/a", Type: model.File, Hash: secret, Stored: true},
		{Path: "/h/b", Type: model.File, Hash: plain, Stored: true},
	}}
	st.SaveManifest(m)
	n, err := st.Scrub(func(p string, b []byte) bool { return string(b) == "SECRET-BODY" })
	if err != nil || n != 1 {
		t.Fatalf("scrub n=%d err=%v", n, err)
	}
	got, _ := st.LoadManifest("m1")
	if got.Entries[0].Stored || got.Entries[0].Hash != secret || !got.Entries[1].Stored {
		t.Errorf("entries after scrub: %+v", got.Entries)
	}
	st.GC()
	if st.HasObject(secret) || !st.HasObject(plain) {
		t.Error("gc after scrub should delete only the secret object")
	}
}
