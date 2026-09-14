package kb

import (
	"strings"
	"testing"

	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/store"
)

func TestClassify(t *testing.T) {
	home := "/Users/u"
	cases := []struct {
		path, goos string
		cat        Category
		risk       Risk
	}{
		{"/Users/u/Library/LaunchAgents/com.x.plist", "darwin", Persistence, Warn},
		{"/Library/LaunchDaemons/com.x.helper.plist", "darwin", Persistence, Warn},
		{"/Users/u/.zshrc", "darwin", Shell, Notice},
		{"/Users/u/.ssh/authorized_keys", "darwin", Trust, Warn},
		{"/opt/homebrew/bin/jq", "darwin", Path, Notice},
		{"/Users/u/Library/Caches/com.x/blob", "darwin", Noise, Info},
		{"/Users/u/.config/nvim/init.lua", "darwin", Config, Info},
		{"/Users/u/random.txt", "darwin", Data, Info},
		{"/Users/u/.config/systemd/user/x.service", "linux", Persistence, Warn},
		{"/etc/apt/sources.list.d/x.list", "linux", Trust, Warn},
		{"/Users/u/Library/LaunchAgents/com.x.plist", "linux", Data, Info},
		{"@state/launchd-loaded", "darwin", Persistence, Warn},
	}
	for _, c := range cases {
		r := Classify(c.path, home, c.goos)
		if r.Category != c.cat || r.Risk != c.risk {
			t.Errorf("%s (%s): got %s/%d want %s/%d", c.path, c.goos, r.Category, r.Risk, c.cat, c.risk)
		}
	}
}

const agent = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>com.foo.updater</string>
<key>ProgramArguments</key><array><string>/opt/foo/updater</string><string>--daemon</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>StartInterval</key><integer>3600</integer>
</dict></plist>`

func TestAnalyzeLaunchAgent(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	h, _ := st.PutBytes([]byte(agent))
	c := model.Change{Path: "/x.plist", Kind: model.Added, After: &model.Entry{Path: "/x.plist", Type: model.File, Hash: h, Stored: true, Mode: 0o644}}
	got := map[string]string{}
	for _, d := range Analyze(st, c, false) {
		got[d.Key] = d.Value
	}
	if got["Label"] != "com.foo.updater" || !strings.Contains(got["Runs"], "/opt/foo/updater --daemon") {
		t.Errorf("label/program: %v", got)
	}
	if !strings.Contains(got["KeepAlive"], "restarts") || !strings.Contains(got["StartInterval"], "1 hour") || got["RunAtLoad"] == "" {
		t.Errorf("schedule keys: %v", got)
	}
}

func TestAnalyzeShellLines(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	before := "alias ll='ls -l'\n"
	after := before + "export PATH=\"$HOME/.foo/bin:$PATH\"\neval \"$(foo init zsh)\"\ncurl -fsSL https://x.sh | sh\n"
	hb, _ := st.PutBytes([]byte(before))
	ha, _ := st.PutBytes([]byte(after))
	c := model.Change{Path: "/h/.zshrc", Kind: model.Modified,
		Before: &model.Entry{Type: model.File, Hash: hb, Stored: true},
		After:  &model.Entry{Type: model.File, Hash: ha, Stored: true}}
	var text []string
	warn := false
	for _, d := range Analyze(st, c, false) {
		text = append(text, d.Value)
		if d.Risk == Warn {
			warn = true
		}
	}
	all := strings.Join(text, "\n")
	for _, want := range []string{"changes PATH", "evaluates generated code", "downloads and runs"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "alias") {
		t.Errorf("unchanged line reported:\n%s", all)
	}
	if !warn {
		t.Error("curl|sh should be a warning")
	}
}

func TestBrewPrefixFromEnvironment(t *testing.T) {
	t.Setenv("HOMEBREW_CELLAR", "/custom/brew/Cellar")
	if r := Classify("/custom/brew/opt/jq", "/Users/u", "darwin"); r.Title != "Homebrew active-version link" {
		t.Errorf("custom prefix link: %+v", r)
	}
	if r := Classify("/opt/homebrew/Cellar/jq/1.8.2", "/Users/u", "darwin"); r.Title != "Homebrew package version" {
		t.Errorf("standard prefix still classified: %+v", r)
	}
}
