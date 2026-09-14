package app

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/morass/spoor/internal/model"
)

// AutoRoots widens the watch list for commands whose effects land outside
// the default profile. For `brew install|upgrade|reinstall|uninstall NAME`
// the package's Cellar directory is watched with contents, so the upgrade
// can be read as file diffs; without names, every package's version
// directories are watched (metadata only).
func (a *App) AutoRoots(argv []string) ([]model.Root, []string) {
	if len(argv) == 0 || filepath.Base(argv[0]) != "brew" {
		return nil, nil
	}
	sub := ""
	var names []string
	for _, x := range argv[1:] {
		if strings.HasPrefix(x, "-") {
			if x == "--cask" || x == "--casks" {
				return nil, nil // casks land in /Applications, already watched
			}
			continue
		}
		if sub == "" {
			sub = x
			continue
		}
		names = append(names, x[strings.LastIndex(x, "/")+1:])
	}
	switch sub {
	case "install", "upgrade", "reinstall", "uninstall", "remove", "rm":
	default:
		return nil, nil
	}
	cellar := os.Getenv("HOMEBREW_CELLAR")
	if cellar == "" {
		for _, c := range []string{"/opt/homebrew/Cellar", "/usr/local/Cellar", "/home/linuxbrew/.linuxbrew/Cellar"} {
			if fi, err := os.Stat(c); err == nil && fi.IsDir() {
				cellar = c
				break
			}
		}
	}
	if cellar == "" {
		return nil, nil
	}
	prefix := filepath.Dir(cellar)
	roots := []model.Root{
		{Path: filepath.Join(prefix, "opt"), Depth: 1},
		{Path: filepath.Join(prefix, "bin"), Depth: 1},
	}
	if len(names) == 0 {
		roots = append(roots, model.Root{Path: cellar, Depth: 2})
		return roots, []string{"brew: watching every package's version folders (name the packages, e.g. `brew upgrade jq`, to also diff their files)"}
	}
	for _, n := range names {
		roots = append(roots, model.Root{Path: filepath.Join(cellar, n), Depth: -1, Content: true})
	}
	return roots, []string{"brew: capturing the files of " + strings.Join(names, ", ") + " so the upgrade can be diffed"}
}

// MergeRoots adds extra roots to base; an extra root with the same path
// replaces the base one.
func MergeRoots(base, extra []model.Root) []model.Root {
	out := append([]model.Root(nil), base...)
	for _, e := range extra {
		replaced := false
		for i := range out {
			if out[i].Path == e.Path {
				out[i], replaced = e, true
			}
		}
		if !replaced {
			out = append(out, e)
		}
	}
	return out
}
