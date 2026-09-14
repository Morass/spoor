package scan

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/store"
)

func TestSensitivePaths(t *testing.T) {
	yes := []string{
		"/h/.ssh/id_ed25519", "/h/.ssh/github", "/h/.npmrc", "/h/.pypirc", "/h/.env", "/h/.env.local",
		"/h/.git-credentials", "/h/.config/gh/hosts.yml", "/h/.config/gcloud/credentials.db",
		"/h/.aws/config", "/h/.secret", "/h/project/secrets.yaml", "/h/.zsh_history", "/h/x.pem",
		"/etc/shadow", "/etc/ssh/ssh_host_ed25519_key", "/h/.kube/config", "/h/.config/sops/age/keys.txt",
	}
	no := []string{
		"/h/.ssh/config", "/h/.ssh/known_hosts", "/h/.zshrc", "/home/secret/notes.txt",
		"/h/.config/nvim/init.lua", "/etc/ssh/ssh_host_ed25519_key.pub", "/opt/homebrew/Cellar/tree/2.3.2/CHANGES",
	}
	for _, p := range yes {
		if !Sensitive(p) {
			t.Errorf("%s should be sensitive", p)
		}
	}
	for _, p := range no {
		if Sensitive(p) {
			t.Errorf("%s should not be sensitive", p)
		}
	}
}

func TestContentScreening(t *testing.T) {
	root := t.TempDir()
	pem := "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAA\n-----END " + "OPENSSH PRIVATE KEY-----\n"
	os.WriteFile(filepath.Join(root, "plain.conf"), []byte("color = auto\n"), 0o644)
	os.WriteFile(filepath.Join(root, "app.json"), []byte(strings.Repeat("{}\n", 110)+pem), 0o644)
	os.WriteFile(filepath.Join(root, "rc"), []byte("export GH_TOKEN=\"$(pass gh)\"\n"), 0o644)
	st, _ := store.Open(filepath.Join(t.TempDir(), "repo"))
	res, err := Scan(Options{Roots: []model.Root{{Path: root, Depth: -1, Content: true}}, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]model.Entry{}
	for _, e := range res.Manifest.Entries {
		got[filepath.Base(e.Path)] = e
	}
	if e := got["app.json"]; e.Stored || e.Hash == "" || e.Skipped != "sensitive content" {
		t.Errorf("key hidden in a config must be hashed, not stored: %+v", e)
	}
	if !got["plain.conf"].Stored || !got["rc"].Stored {
		t.Errorf("ordinary files (and variable references) must be stored: %+v %+v", got["plain.conf"], got["rc"])
	}
	filepath.WalkDir(st.Root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "b3BlbnNzaC1rZXktdjEAAAA") {
				t.Errorf("key material copied into %s", p)
			}
		}
		return nil
	})
}
