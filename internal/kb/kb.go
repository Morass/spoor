// Package kb is spoor's knowledge base: what a changed path means, how
// risky that kind of change is, and what to look at inside it.
package kb

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

type Category string

const (
	Persistence Category = "persistence"
	Trust       Category = "trust"
	Shell       Category = "shell"
	Path        Category = "path"
	Apps        Category = "apps"
	Config      Category = "config"
	State       Category = "state"
	Data        Category = "data"
	Noise       Category = "noise"
)

type Risk int

const (
	Info Risk = iota
	Notice
	Warn
)

func (r Risk) Icon() string {
	switch r {
	case Warn:
		return "⚠"
	case Notice:
		return "◆"
	}
	return "·"
}

// Rule matches paths. Patterns: ~ is the home dir, * one path segment
// part, ** any depth.
type Rule struct {
	Pattern  string
	OS       string // "", "darwin" or "linux"
	Category Category
	Risk     Risk
	Title    string
	Explain  string
}

var Rules = []Rule{
	// --- noise first: churn that happens during any run
	{Pattern: "**/.DS_Store", Category: Noise, Title: "Finder metadata"},
	{Pattern: "**/Caches/**", Category: Noise, Title: "Cache"},
	{Pattern: "~/.cache/**", Category: Noise, Title: "Cache"},
	{Pattern: "~/Library/Logs/**", Category: Noise, Title: "Log"},
	{Pattern: "**/*.log", Category: Noise, Title: "Log file"},
	{Pattern: "~/Library/Saved Application State/**", Category: Noise, Title: "Window state"},
	{Pattern: "~/Library/Application Support/*/Crashpad/**", Category: Noise, Title: "Crash reports"},
	{Pattern: "~/.zsh_history", Category: Noise, Title: "Shell history"},
	{Pattern: "~/.bash_history", Category: Noise, Title: "Shell history"},
	{Pattern: "~/.zsh_sessions/**", Category: Noise, Title: "Shell sessions"},
	{Pattern: "~/.lesshst", Category: Noise, Title: "less history"},
	{Pattern: "~/.viminfo", Category: Noise, Title: "vim history"},
	{Pattern: "~/.python_history", Category: Noise, Title: "Python history"},
	{Pattern: "~/.local/share/spoor/**", Category: Noise, Title: "spoor's own repository"},
	{Pattern: "~/.local/share/nvim/**", Category: Noise, Title: "Neovim state"},
	{Pattern: "~/.local/state/**", Category: Noise, Title: "Application state"},
	{Pattern: "**/__pycache__/**", Category: Noise, Title: "Python bytecode"},
	{Pattern: "~/Library/Preferences/*.plist", Category: Noise, Title: "App preferences", Explain: "Apps rewrite these whenever a setting or window position changes; rarely interesting on their own."},

	// --- persistence: things that start by themselves
	{Pattern: "~/Library/LaunchAgents/*", OS: "darwin", Category: Persistence, Risk: Warn, Title: "User LaunchAgent",
		Explain: "launchd starts this program for your user account — at login (RunAtLoad), on a timer, or when a watched path changes. It survives reboots until the plist is removed."},
	{Pattern: "/Library/LaunchAgents/*", OS: "darwin", Category: Persistence, Risk: Warn, Title: "System-wide LaunchAgent",
		Explain: "Starts for every user who logs in. Writing here needs admin rights, so an installer asked for (or had) your password."},
	{Pattern: "/Library/LaunchDaemons/*", OS: "darwin", Category: Persistence, Risk: Warn, Title: "LaunchDaemon",
		Explain: "Runs as root at boot, even when nobody is logged in. The most powerful kind of background job on a Mac."},
	{Pattern: "/Library/PrivilegedHelperTools/*", OS: "darwin", Category: Persistence, Risk: Warn, Title: "Privileged helper tool",
		Explain: "A root-owned helper an app talks to for admin tasks (updates, network setup). Normally paired with a LaunchDaemon of the same name."},
	{Pattern: "/Library/StartupItems/**", OS: "darwin", Category: Persistence, Risk: Warn, Title: "Legacy StartupItem",
		Explain: "A pre-launchd startup mechanism. Modern software should not install these."},
	{Pattern: "~/Library/Application Support/com.apple.backgroundtaskmanagementagent/**", OS: "darwin", Category: Persistence, Risk: Warn, Title: "Login Items database",
		Explain: "macOS's record of Login Items and background items (System Settings › General › Login Items). A change means something registered to run at login."},
	{Pattern: "/Library/Extensions/**", OS: "darwin", Category: Persistence, Risk: Warn, Title: "Kernel extension",
		Explain: "Code loaded into the kernel. Requires explicit approval in System Settings on modern macOS."},
	{Pattern: "~/Library/Services/**", OS: "darwin", Category: Persistence, Risk: Notice, Title: "macOS Service",
		Explain: "Adds an entry to the Services menu of every app."},
	{Pattern: "@state/launchd-loaded", OS: "darwin", Category: Persistence, Risk: Warn, Title: "Loaded launchd jobs",
		Explain: "Jobs currently loaded in your login session (from `launchctl list`, Apple's own omitted). A label here is running or armed right now, whether or not its plist is still on disk."},
	{Pattern: "@state/crontab", Category: Persistence, Risk: Warn, Title: "Your crontab",
		Explain: "Commands cron runs on a schedule as you."},
	{Pattern: "~/.config/systemd/user/**", OS: "linux", Category: Persistence, Risk: Warn, Title: "systemd user unit",
		Explain: "A service or timer systemd runs for your user; `systemctl --user enable` links it into a target so it starts automatically."},
	{Pattern: "/etc/systemd/system/**", OS: "linux", Category: Persistence, Risk: Warn, Title: "systemd system unit",
		Explain: "A system service or timer, usually running as root at boot."},
	{Pattern: "/etc/cron*/**", OS: "linux", Category: Persistence, Risk: Warn, Title: "System cron job"},
	{Pattern: "/etc/crontab", OS: "linux", Category: Persistence, Risk: Warn, Title: "System crontab"},
	{Pattern: "~/.config/autostart/*", OS: "linux", Category: Persistence, Risk: Warn, Title: "Desktop autostart entry",
		Explain: "Started by the desktop session at every graphical login."},
	{Pattern: "@state/systemd-user-units", OS: "linux", Category: Persistence, Risk: Warn, Title: "systemd user unit states",
		Explain: "Which user units exist and whether they are enabled (from `systemctl --user list-unit-files`)."},

	// --- trust: who and what your machine believes
	{Pattern: "~/.ssh/authorized_keys", Category: Trust, Risk: Warn, Title: "SSH authorized_keys",
		Explain: "Every key listed here can log in to this account over SSH."},
	{Pattern: "~/.ssh/**", Category: Trust, Risk: Warn, Title: "SSH configuration",
		Explain: "Which hosts you trust and which keys you use. Key bodies are never copied into spoor's store."},
	{Pattern: "/etc/hosts", Category: Trust, Risk: Warn, Title: "hosts file",
		Explain: "Overrides DNS for the names listed — can silently redirect a domain."},
	{Pattern: "/etc/sudoers.d/**", OS: "linux", Category: Trust, Risk: Warn, Title: "sudo rules"},
	{Pattern: "/etc/apt/sources.list.d/**", OS: "linux", Category: Trust, Risk: Warn, Title: "APT package source",
		Explain: "Adds a software repository: every future `apt upgrade` may install packages from it."},
	{Pattern: "/etc/apt/trusted.gpg.d/**", OS: "linux", Category: Trust, Risk: Warn, Title: "APT signing key"},
	{Pattern: "/usr/share/keyrings/*", OS: "linux", Category: Trust, Risk: Warn, Title: "Package signing keyring"},
	{Pattern: "/etc/ld.so.preload", OS: "linux", Category: Trust, Risk: Warn, Title: "Preloaded libraries",
		Explain: "Libraries injected into every dynamically linked program."},

	// --- shell startup
	{Pattern: "~/.zshrc", Category: Shell, Risk: Notice, Title: "zsh startup file", Explain: shellExplain},
	{Pattern: "~/.zprofile", Category: Shell, Risk: Notice, Title: "zsh login file", Explain: shellExplain},
	{Pattern: "~/.zshenv", Category: Shell, Risk: Notice, Title: "zsh env file", Explain: "Read by EVERY zsh, including scripts — the widest-reaching shell file."},
	{Pattern: "~/.zlogin", Category: Shell, Risk: Notice, Title: "zsh login file", Explain: shellExplain},
	{Pattern: "~/.bashrc", Category: Shell, Risk: Notice, Title: "bash startup file", Explain: shellExplain},
	{Pattern: "~/.bash_profile", Category: Shell, Risk: Notice, Title: "bash login file", Explain: shellExplain},
	{Pattern: "~/.profile", Category: Shell, Risk: Notice, Title: "POSIX login profile", Explain: shellExplain},
	{Pattern: "~/.config/fish/**", Category: Shell, Risk: Notice, Title: "fish configuration", Explain: shellExplain},
	{Pattern: "/etc/zshrc", Category: Shell, Risk: Warn, Title: "System zsh startup file", Explain: "Runs for every user's zsh."},
	{Pattern: "/etc/profile", Category: Shell, Risk: Warn, Title: "System login profile", Explain: "Runs for every user's login shell."},
	{Pattern: "/etc/profile.d/*", OS: "linux", Category: Shell, Risk: Warn, Title: "System profile snippet", Explain: "Sourced by every login shell."},
	{Pattern: "/etc/bash.bashrc", OS: "linux", Category: Shell, Risk: Warn, Title: "System bashrc"},

	// --- PATH and commands
	{Pattern: "/etc/paths", OS: "darwin", Category: Path, Risk: Notice, Title: "System PATH list", Explain: "path_helper builds everyone's PATH from this file and /etc/paths.d."},
	{Pattern: "/etc/paths.d/*", OS: "darwin", Category: Path, Risk: Notice, Title: "System PATH entry", Explain: "Each line becomes a PATH directory for every user."},
	{Pattern: "/opt/homebrew/bin/*", Category: Path, Risk: Notice, Title: "Command on PATH", Explain: pathExplain},
	{Pattern: "/usr/local/bin/*", Category: Path, Risk: Notice, Title: "Command on PATH", Explain: pathExplain},
	{Pattern: "/usr/bin/*", Category: Path, Risk: Notice, Title: "System command", Explain: pathExplain},
	{Pattern: "~/.local/bin/*", Category: Path, Risk: Notice, Title: "Command on PATH", Explain: pathExplain},
	{Pattern: "~/bin/*", Category: Path, Risk: Notice, Title: "Command on PATH", Explain: pathExplain},
	{Pattern: "~/.cargo/bin/*", Category: Path, Risk: Notice, Title: "Cargo-installed command", Explain: pathExplain},
	{Pattern: "~/go/bin/*", Category: Path, Risk: Notice, Title: "Go-installed command", Explain: pathExplain},
	{Pattern: "~/.npm-global/bin/*", Category: Path, Risk: Notice, Title: "npm global command", Explain: pathExplain},

	// --- apps
	{Pattern: "/Applications/*", OS: "darwin", Category: Apps, Title: "Application", Explain: "An app bundle in /Applications."},
	{Pattern: "~/Applications/*", OS: "darwin", Category: Apps, Title: "Application (user)"},
	{Pattern: "~/.local/share/applications/*", OS: "linux", Category: Apps, Title: "Desktop launcher entry"},
	{Pattern: "~/Library/Internet Plug-Ins/**", OS: "darwin", Category: Apps, Risk: Notice, Title: "Browser plug-in"},

	// --- config
	{Pattern: "~/.gitconfig", Category: Config, Risk: Notice, Title: "Git configuration", Explain: "Can set hooks paths, credential helpers and URL rewrites for every repository."},
	{Pattern: "~/.npmrc", Category: Config, Risk: Notice, Title: "npm configuration", Explain: "Registry URLs and often auth tokens (redacted in exports)."},
	{Pattern: "~/.config/**", Category: Config, Title: "Configuration file"},
	{Pattern: "/etc/**", Category: Config, Risk: Notice, Title: "System configuration"},
	{Pattern: "@state/listening-ports", Category: State, Risk: Notice, Title: "Listening TCP ports",
		Explain: "Programs accepting network connections (only processes you can see without sudo)."},
	{Pattern: "~/Library/Application Support/*", OS: "darwin", Category: Data, Title: "App data folder"},
	{Pattern: "~/.local/share/*", Category: Data, Title: "App data folder"},
}

const shellExplain = "Runs in every new terminal. Installers add PATH exports, `eval \"$(tool init)\"` hooks and completions here — each line executes with your permissions."
const pathExplain = "A new command here becomes something you can type — and can shadow an existing command of the same name earlier in PATH."

type compiled struct {
	re   *regexp.Regexp
	rule Rule
}

var (
	cacheMu sync.Mutex
	cache   = map[string][]compiled{}
)

func compile(home, goos string) []compiled {
	key := home + "\x00" + goos
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if c, ok := cache[key]; ok {
		return c
	}
	var out []compiled
	for _, r := range Rules {
		if r.OS != "" && r.OS != goos {
			continue
		}
		out = append(out, compiled{globRE(r.Pattern, home), r})
	}
	cache[key] = out
	return out
}

func globRE(p, home string) *regexp.Regexp {
	if strings.HasPrefix(p, "~/") {
		p = strings.TrimSuffix(home, "/") + p[1:]
	}
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch {
		case strings.HasPrefix(p[i:], "**/"):
			sb.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(p[i:], "**"):
			sb.WriteString(".*")
			i++
		case p[i] == '*':
			sb.WriteString("[^/]*")
		case p[i] == '?':
			sb.WriteString("[^/]")
		default:
			sb.WriteString(regexp.QuoteMeta(string(p[i])))
		}
	}
	sb.WriteString("$")
	return regexp.MustCompile(sb.String())
}

var fallback = Rule{Category: Data, Risk: Info, Title: "File"}

// Classify returns the first rule matching path.
func Classify(path, home, goos string) Rule {
	for _, c := range compile(home, goos) {
		if c.re.MatchString(path) {
			return c.rule
		}
	}
	return fallback
}

var categoryOrder = map[Category]int{Persistence: 0, Trust: 1, Shell: 2, Path: 3, Apps: 4, Config: 5, State: 6, Data: 7, Noise: 8}

// SortCategories orders categories from most to least important.
func SortCategories(cs []Category) {
	sort.Slice(cs, func(i, j int) bool { return categoryOrder[cs[i]] < categoryOrder[cs[j]] })
}

// Rank is a category's position in that order.
func Rank(c Category) int { return categoryOrder[c] }
