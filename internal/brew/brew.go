// Package brew asks Homebrew what it would change, so spoor can watch the
// right package folders (with contents) before an upgrade runs.
package brew

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type Brew struct {
	Path   string
	Cellar string
}

type Outdated struct {
	Name      string   `json:"name"`
	Installed []string `json:"installed_versions"`
	Current   string   `json:"current_version"`
	Pinned    bool     `json:"pinned"`
}

// Name is what Homebrew accepts: letters, digits and @ . _ + -, optionally
// prefixed by a tap (user/tap/name).
var Name = regexp.MustCompile(`^([a-z0-9-]+/[a-z0-9-]+/)?[A-Za-z0-9][A-Za-z0-9@._+-]*$`)

// Find locates brew: $SPOOR_BREW, PATH, then the standard prefixes.
func Find() (*Brew, error) {
	cands := []string{os.Getenv("SPOOR_BREW")}
	if p, err := exec.LookPath("brew"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, "/opt/homebrew/bin/brew", "/usr/local/bin/brew", "/home/linuxbrew/.linuxbrew/bin/brew")
	for _, c := range cands {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			b := &Brew{Path: c}
			out, err := b.output("--cellar")
			if err != nil {
				return nil, fmt.Errorf("brew --cellar: %w", err)
			}
			b.Cellar = strings.TrimSpace(out)
			return b, nil
		}
	}
	return nil, errors.New("Homebrew not found (set SPOOR_BREW to its path)")
}

func (b *Brew) command(args ...string) *exec.Cmd {
	c := exec.Command(b.Path, args...)
	c.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1")
	return c
}

func (b *Brew) output(args ...string) (string, error) {
	var stderr strings.Builder
	c := b.command(args...)
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// Update runs `brew update` with its output shown.
func (b *Brew) Update() error {
	c := exec.Command(b.Path, "update")
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	return c.Run()
}

func (b *Brew) Outdated() ([]Outdated, error) {
	out, err := b.output("outdated", "--json=v2", "--formula")
	if err != nil && out == "" {
		return nil, err
	}
	var v struct {
		Formulae []Outdated `json:"formulae"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("brew outdated: %w", err)
	}
	return v.Formulae, nil
}

// Deps lists the installed dependencies of name (recursively).
func (b *Brew) Deps(name string) []string {
	out, err := b.output("deps", "--installed", name)
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// Installed reports whether any version of name is in the Cellar.
func (b *Brew) Installed(name string) bool {
	ents, err := os.ReadDir(filepath.Join(b.Cellar, short(name)))
	return err == nil && len(ents) > 0
}

// Exists reports whether Homebrew knows a formula called name.
func (b *Brew) Exists(name string) bool {
	_, err := b.output("info", "--json=v2", "--formula", name)
	return err == nil
}

func short(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// Size sums the bytes a capture of name's Cellar folder would keep: files
// up to maxFile (bigger ones are recorded without content).
func (b *Brew) Size(name string, maxFile int64) (int64, int) {
	var total int64
	files := 0
	filepath.WalkDir(filepath.Join(b.Cellar, short(name)), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			files++
			if fi.Size() <= maxFile {
				total += fi.Size()
			}
		}
		return nil
	})
	return total, files
}

// Folder is the Cellar folder of a formula.
func (b *Brew) Folder(name string) string { return filepath.Join(b.Cellar, short(name)) }
