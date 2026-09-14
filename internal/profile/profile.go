// Package profile decides which parts of a machine spoor watches by
// default: the places installers actually write to, not the whole disk.
package profile

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/morass/spoor/internal/model"
)

func r(path string, depth int, content bool) model.Root {
	return model.Root{Path: path, Depth: depth, Content: content}
}

// Default returns the watched roots for goos with ~ expanded to home.
// Content roots capture file bodies (small config-like files); the rest
// record metadata only, which is enough to see that something appeared.
func Default(goos, home string) []model.Root {
	h := func(p string) string { return filepath.Join(home, p) }
	common := []model.Root{
		r(home, 1, true), // dotfiles: .zshrc, .bashrc, .profile, .gitconfig ...
		r(h(".config"), 4, true),
		r(h(".ssh"), 1, true), // key bodies are never stored (sensitive rule)
		r(h(".local/bin"), 1, false),
		r(h(".local/share"), 2, false),
		r(h("bin"), 1, false),
		r(h(".cargo/bin"), 1, false),
		r(h("go/bin"), 1, false),
		r(h(".npm-global/bin"), 1, false),
	}
	switch goos {
	case "darwin":
		return append(common,
			r(h("Library/LaunchAgents"), 1, true),
			r(h("Library/Application Support/com.apple.backgroundtaskmanagementagent"), 1, true),
			r(h("Library/Application Support"), 2, false),
			r(h("Library/Preferences"), 1, false),
			r(h("Library/Services"), 1, false),
			r(h("Library/Internet Plug-Ins"), 1, false),
			r(h("Applications"), 1, false),
			r("/Applications", 1, false),
			r("/Library/LaunchAgents", 1, true),
			r("/Library/LaunchDaemons", 1, true),
			r("/Library/PrivilegedHelperTools", 1, false),
			r("/Library/StartupItems", 2, false),
			r("/Library/Extensions", 1, false),
			r("/Library/Application Support", 1, false),
			r("/etc/paths", 0, true),
			r("/etc/paths.d", 1, true),
			r("/etc/zshrc", 0, true),
			r("/etc/profile", 0, true),
			r("/etc/hosts", 0, true),
			r("/opt/homebrew/bin", 1, false),
			r("/usr/local/bin", 1, false),
		)
	case "linux":
		return append(common,
			r(h(".local/share/applications"), 1, true),
			r("/etc", 3, true),
			r("/usr/local", 3, false),
			r("/opt", 2, false),
			r("/usr/bin", 1, false),
			r("/usr/share/keyrings", 1, false),
		)
	}
	return common
}

// Parse reads PATH[:DEPTH[:content|meta]] as given to --root.
func Parse(spec string) (model.Root, error) {
	parts := strings.Split(spec, ":")
	root := model.Root{Path: parts[0], Depth: -1, Content: true}
	if len(parts) > 1 && parts[1] != "" {
		d, err := strconv.Atoi(parts[1])
		if err != nil {
			return root, fmt.Errorf("--root %s: depth must be a number", spec)
		}
		root.Depth = d
	}
	if len(parts) > 2 {
		switch parts[2] {
		case "content":
		case "meta":
			root.Content = false
		default:
			return root, fmt.Errorf("--root %s: mode must be content or meta", spec)
		}
	}
	abs, err := filepath.Abs(root.Path)
	if err != nil {
		return root, err
	}
	root.Path = abs
	return root, nil
}

// Covers reports whether path lies inside root within its depth.
func Covers(root model.Root, path string) bool {
	if path == root.Path {
		return true
	}
	prefix := strings.TrimSuffix(root.Path, "/") + "/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if root.Depth < 0 {
		return true
	}
	return strings.Count(path[len(prefix):], "/")+1 <= root.Depth
}

func CoveredBy(roots []model.Root, path string) bool {
	for _, rt := range roots {
		if Covers(rt, path) {
			return true
		}
	}
	return false
}
