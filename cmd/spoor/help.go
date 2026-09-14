package main

import (
	"fmt"
	"io"
	"strings"
)

type commandDoc struct {
	usage    string
	summary  string
	details  string
	examples []string
}

var docs = map[string]commandDoc{
	"run": {
		usage:   "spoor run [flags] -- COMMAND [ARGS...]",
		summary: "Record what a command changes, then offer to open the review.",
		details: `Snapshots the watched folders, runs COMMAND (its output and exit code pass
straight through), snapshots again and commits the difference. Anything that
changed since the last commit is recorded first as a separate "drift" commit.
Secret-looking arguments (--token X) are stored as [REDACTED].

For "brew install|upgrade|reinstall NAME" the package's own folder is watched
too, with file contents, so the upgrade can be read as file diffs.`,
		examples: []string{
			`spoor run -- sh -c "$(curl -fsSL https://example.com/install.sh)"`,
			`spoor run -m "set up node" -- brew install node`,
			`spoor run --add-root /opt/acme:3 -- ./install.sh`,
			`spoor run --trace -- npm install -g some-cli     # who wrote what (eslogger/strace)`,
		},
	},
	"snap": {
		usage:   "spoor snap [-m MESSAGE] [--root SPEC]... [--add-root SPEC]...",
		summary: "Record the current state as a commit, if anything changed.",
		details: `Use it as a checkpoint: take a snap now and then, and "spoor status" or
"spoor review now" shows what changed since, whoever changed it.`,
		examples: []string{`spoor snap -m "before trying the beta driver"`},
	},
	"upgrade": {
		usage:   "spoor upgrade [NAME...] [flags]",
		summary: "Upgrade Homebrew packages with every changed file captured, then review.",
		details: `Runs "brew update", reads which formulae are outdated and, with no NAME on a
terminal, shows a checklist to tick the ones you want. It captures the files
of every package that will change (including outdated dependencies), runs the
upgrade and opens the review grouped as package › files › diff.

NAME@VERSION installs a versioned formula Homebrew ships (python@3.12).
Homebrew cannot install arbitrary versions; spoor says so instead of guessing.
Undo an upgrade with brew itself: brew removes old versions.`,
		examples: []string{
			`spoor upgrade                 # checklist of outdated formulae`,
			`spoor upgrade jq ripgrep      # just these (and their outdated dependencies)`,
			`spoor upgrade --list          # what would change, touches nothing`,
			`spoor upgrade --all -y        # everything, no questions`,
		},
	},
	"try": {
		usage:   "spoor try [flags] -- COMMAND   |   spoor try apply REF [--force]   |   spoor try discard REF",
		summary: "Linux: run a command on copy-on-write overlays and review it before it lands.",
		details: `The command sees the real files but its writes go to a private workspace.
Review the result like any commit, then apply it or throw it away. Needs root,
or unprivileged user namespaces (spoor explains when a distribution blocks
them). Not a security sandbox: network and anything outside the overlays are
real.`,
		examples: []string{`sudo spoor try -- ./install.sh`, `spoor review <id>`, `sudo spoor try apply <id>`},
	},
	"review": {
		usage:   "spoor review [REF|now]",
		summary: "Open the interactive inspector.",
		details: `Without REF: the history. With a commit: three panes, changes grouped by risk,
the diff, and notes (what the path is, what the analyzers found, your note).
"now" reviews live changes since the last commit.

  j/k ↑/↓  move            ]/[   next/previous diff    tab   switch pane
  J/K      scroll diff     e     open in $EDITOR       d     vimdiff before/after
  o / Q    quickfix list   n     write a note          x/X   mark change/category
  R        revert marked   u     write undo script     .     show noise
  /        filter          esc   back                  ?     all keys`,
		examples: []string{`spoor review`, `spoor review HEAD`, `spoor review now`},
	},
	"status": {
		usage:    "spoor status [--patch] [--all]",
		summary:  "Show what changed since the last commit, without recording anything.",
		examples: []string{`spoor status`, `spoor status --patch`},
	},
	"log": {
		usage:    "spoor log [-n N] [PATH]",
		summary:  "List commits, newest first. With PATH, only the commits that touched it.",
		examples: []string{`spoor log -n 10`, `spoor log ~/.zshrc`},
	},
	"show": {
		usage:    "spoor show [REF] [--patch] [--all]",
		summary:  "Show one commit: command, exit code, changes grouped by risk, notes.",
		details:  `--patch adds the diffs; --all includes noise (caches, logs, shell history).`,
		examples: []string{`spoor show`, `spoor show HEAD~2 --patch`},
	},
	"diff": {
		usage:    "spoor diff A [B|now] [--path PATH]... [--patch] [--all]",
		summary:  "Compare any two points in history, or a commit with the live machine.",
		examples: []string{`spoor diff HEAD~5 now`, `spoor diff 3f1a HEAD --path ~/.zshrc --patch`},
	},
	"blame": {
		usage:    "spoor blame PATH",
		summary:  "Which commits put PATH there or changed it, and which process wrote it (with --trace).",
		examples: []string{`spoor blame ~/Library/LaunchAgents/com.example.updater.plist`},
	},
	"explain": {
		usage:    "spoor explain PATH",
		summary:  "What a path is, why it matters, what is inside, and when it last changed.",
		examples: []string{`spoor explain ~/.zshenv`, `spoor explain /Library/LaunchDaemons/com.example.helper.plist`},
	},
	"note": {
		usage:    "spoor note REF PATH [TEXT...]",
		summary:  "Read or write your note on one change of a commit.",
		examples: []string{`spoor note HEAD ~/.local/bin/acme "shim for the acme CLI, keep"`},
	},
	"revert": {
		usage:   "spoor revert REF [--apply] [--only CATEGORIES] [--path PATH]... [--force] [--script FILE]",
		summary: "Undo a commit, whole or in part. Shows the plan unless --apply.",
		details: `The plan is checked against the live machine: paths edited since the commit
are conflicts (left alone without --force), launch agents are unloaded before
their plists are removed, folders the commit created are removed as a tree
unless something inside changed later, and content that was never captured is
reported as unrestorable. Every applied revert is itself a commit.

Categories: persistence, trust, shell, path, apps, config, state, data, noise.`,
		examples: []string{
			`spoor revert HEAD                              # the plan`,
			`spoor revert HEAD --apply`,
			`spoor revert 3f1a --only persistence --apply   # just what starts by itself`,
			`spoor revert HEAD --script undo.sh             # a reviewable shell script`,
		},
	},
	"restore": {
		usage:    "spoor restore PATH --to REF [--before] [--apply]",
		summary:  "Put one path back as it was after (or --before) a commit.",
		examples: []string{`spoor restore ~/.zshrc --to HEAD~3 --apply`},
	},
	"quickfix": {
		usage:    "spoor quickfix [REF] [-o FILE]",
		summary:  "A vim quickfix list of a commit's changed files, with your notes.",
		examples: []string{`spoor quickfix HEAD -o changes.qf && vim -q changes.qf`},
	},
	"export": {
		usage:   "spoor export [REF] [--format md|json] [-o FILE] [--no-redact]",
		summary: "A shareable summary of a commit, with secrets and personal paths redacted.",
		details: `Redaction is pattern based (tokens, keys, passwords, authorization headers,
e-mail addresses, SSH hosts, home directories). Read an export before you
publish it.`,
		examples: []string{`spoor export HEAD > footprint.md`, `spoor export HEAD --format json -o footprint.json`},
	},
	"scrub": {
		usage:   "spoor scrub",
		summary: "Forget stored file bodies that look secret, then delete them.",
		details: `History keeps their hashes, so changes stay visible; the contents are gone.
Useful for repositories created by older versions.`,
	},
	"init": {
		usage:   "spoor init [--root SPEC]... [--no-state] [--max-content BYTES]",
		summary: "Choose what this repository watches (replaces the built-in profile).",
		details: `SPEC is PATH[:DEPTH[:content|meta]]. Content roots keep file bodies (up to
1 MiB) so they can be diffed and restored; meta roots record type, size,
mtime and mode. --no-state skips launchd jobs, crontab and listening ports.`,
		examples: []string{`spoor init --root ~:1 --root ~/.config:4 --root /Applications:1:meta`},
	},
	"roots":   {usage: "spoor roots", summary: "Print the watched roots and whether system state is recorded."},
	"gc":      {usage: "spoor gc", summary: "Delete stored objects no longer referenced by any commit."},
	"version": {usage: "spoor version", summary: "Print the version."},
}

func printCommandHelp(w io.Writer, name string) bool {
	name = strings.Fields(name + " ")[0]
	d, ok := docs[name]
	if !ok {
		return false
	}
	fmt.Fprintf(w, "%s\n\n  %s\n", d.summary, d.usage)
	if d.details != "" {
		fmt.Fprintf(w, "\n%s\n", d.details)
	}
	if len(d.examples) > 0 {
		fmt.Fprintln(w, "\nExamples:")
		for _, e := range d.examples {
			fmt.Fprintln(w, "  "+e)
		}
	}
	return true
}

func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" || a == "-help" {
			return true
		}
	}
	return false
}
