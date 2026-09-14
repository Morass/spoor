# Developing spoor

How spoor is put together, why it behaves the way it does, and how a change is
tested and released. The [README](../README.md) is for users; this page is for
people changing the code.

## Architecture

```
cmd/spoor/          CLI: command dispatch, per-command help (help.go), `spoor upgrade` (upgrade.go)
internal/app/       operations shared by the CLI and the TUI: run, snap, status, revert,
                    restore, try, blame, export, upgrade pairing (pair.go), auto roots (auto.go)
internal/store/     content-addressed objects, gz manifests, commits, notes, traces, config,
                    lock, gc, scrub, atomic writes
internal/scan/      walks watched roots into a manifest; secret screening; system-state collectors
internal/diff/      manifest comparison, rename detection, unified text diffs, plist decoding
internal/kb/        knowledge base (path → category, risk, explanation) and analyzers
internal/revert/    undo planning against the live machine, guards, apply, undo scripts
internal/trace/     eslogger (macOS) and strace (Linux) attribution
internal/try/       Linux overlay previews (namespaces + overlayfs)
internal/brew/      what Homebrew would change: outdated, deps, cellar, sizes
internal/redact/    secret detection for storage and redaction for export
internal/tui/       bubbletea review UI and the upgrade checklist
e2e/                end-to-end tests that drive the real binary in a sandboxed HOME
scripts/            tui-smoke.sh, screenshots.sh, ansi2svg, prepublish-check.sh
```

The TUI never talks to the store directly: everything goes through `internal/app`,
so the CLI and the review screen cannot disagree.

## Data model

- **Root**: `PATH`, depth and whether file bodies are kept (content) or only type,
  size, mtime and mode (meta). The built-in profile per OS lives in
  `internal/profile`; `spoor init` replaces it; `--add-root` and automatic roots
  (`brew install|upgrade NAME` watches that package's Cellar folder) extend it for a
  single command.
- **Manifest**: every entry under the roots at one moment, plus virtual
  `@state/…` entries (loaded launchd jobs, crontab, listening ports, systemd user
  units), stored gzipped under `manifests/`.
- **Commit**: a before and an after manifest. Kinds: `snap`, `run`, `drift`
  (anything that changed since the last commit, recorded before a new operation so
  history stays continuous), `revert` (including `restore`) and `try` (a Linux
  preview that never moves `HEAD`). `HEAD`, `HEAD~n` and id prefixes resolve like
  git.
- **Objects**: file bodies addressed by sha256. On APFS they are captured with
  `clonefile(2)`, which is instant and shares blocks until the source changes.
- **Notes and traces**: per commit, keyed by path.

Comparing manifests with different roots only compares paths both of them cover,
so widening the watch list never reads as mass additions.

**Upgrade pairing** (`internal/app/pair.go`): a package manager does not edit files
in place. It creates `Cellar/<pkg>/<new>` and usually deletes `<old>`. The view pairs
version-named sibling folders and lists each file as a comparison under
**UPGRADE**. Those comparison rows are *virtual*: revert never uses them, and the
raw entries (still what revert acts on) fold into noise.

## Security model

spoor reads sensitive parts of a machine and can delete files, so the rules are
strict and each one has a test.

**What is never stored** (`internal/scan`, `internal/redact`):

1. By file name: SSH keys (anything in `~/.ssh` except `config`, `known_hosts`,
   `authorized_keys`, `*.pub`), key and certificate extensions, `.env*`, `.npmrc`,
   `.pypirc`, `.netrc`, `.git-credentials`, credential folders of common CLIs,
   password stores, keychains, shell histories, `/etc/shadow`, SSH host keys. Rules
   match file names and known folders, never arbitrary directory names.
2. By content, whatever the name: private-key blocks, known token formats,
   credentials in URLs, `Authorization` headers, and literal values assigned to
   secret-named keys. Variable references (`TOKEN="$VAR"`) and placeholders are not
   secrets, so ordinary shell files stay diffable.

Rejected files are hashed, so changes are still detected. The same content check
applies to state text (a crontab line with a token), and recorded command lines
go through `redact.Argv`. `diff.Content` withholds anything that looks secret
even when it comes from an older repository; `spoor scrub` removes such copies.

**Export** (`redact.Text`) redacts tokens, keys, passwords, authorization headers,
netrc entries, webhooks, e-mail addresses, SSH host details and home directories
(using the home recorded in the manifest). It redacts the whole diff before
truncating it, so a key cannot be split from its delimiter.

**Undo guards** (`internal/revert/guard.go`):

- Each file action carries its watched root and a fingerprint of the path when the
  plan was made. `Apply` refuses if a parent folder is now a symlink, or if the
  path changed since planning (between a plan and a confirmation prompt, a daemon
  or a person may have touched it).
- Removing a folder the commit created checks every descendant's ctime. Unlike
  mtime, a user cannot set it (`cp -p`, `touch -d`), and an unreadable subtree
  counts as changed. Anything newer than the commit is a conflict.
- Undo scripts never let recorded text reach a command position: comment text is
  stripped of control characters, paths are single-quoted, restores go through
  `mktemp` and `mv -f` so a symlink at the target is replaced rather than followed.
- Modes keep setuid, setgid and sticky bits.

**Other surfaces**: exports, quickfix lists and undo scripts are written through a
fresh temporary file and renamed, so an existing file's permissions or symlink are
never inherited. vimdiff copies live in a private folder that is removed when the
diff tool exits, and vim runs with `-n -i NONE`. `try` requires a workspace
directory owned by the user with mode 700, removes only what was reviewed, and
keeps owners and special bits when running as root. Homebrew is executed with an
argument vector, never through a shell, and formula names reported by brew are
validated.

## Testing

| Layer | Command | What it covers |
|---|---|---|
| Unit | `go test ./internal/...` | diff kinds and renames, plist rendering, knowledge base, analyzers, secret detection and redaction, trace parsers, store resolve/gc/scrub, revert guards, script injection, tree conflicts, version ordering |
| TUI model | `go test ./internal/tui` | navigation, notes, filtering, marks, revert confirmation, live view, key bursts, upgrade grouping, the checklist |
| End to end | `go test ./e2e` | the real binary against a throwaway `HOME`: install → review → revert → revert the revert, byte-identical trees, conflicts, `--force`, restore, drift, blame, metadata-only roots, secrets never stored or shown, directory trees, a simulated Homebrew upgrade and `spoor upgrade`, help |
| Real terminal | `scripts/tui-smoke.sh` | drives the review in tmux and asserts on the screen, including the vimdiff hand-off |
| Screenshots | `scripts/screenshots.sh` | regenerates `docs/images/*.svg` from real captures in a sandbox |

Linux coverage needs a Linux machine. Cross-compile from macOS and run the test
binaries there; run the end-to-end tests again as root to include `try`:

```sh
GOOS=linux GOARCH=amd64 go build -o spoor-linux ./cmd/spoor
GOOS=linux GOARCH=amd64 go test -c -o e2e.test ./e2e
# on the Linux machine
SPOOR_BIN=$PWD/spoor-linux ./e2e.test -test.v
sudo env SPOOR_BIN=$PWD/spoor-linux ./e2e.test -test.v
```

Useful environment variables for tests and sandboxes: `SPOOR_HOME` (repository),
`SPOOR_NO_STATE=1` (skip system state), `SPOOR_TRY_DIR` (try workspace),
`SPOOR_BREW` and `HOMEBREW_CELLAR` (a fake Homebrew), `SPOOR_DIFFTOOL`,
`SPOOR_NO_PROMPT`.

Before trusting a change to secrets or undo, add a test that fails without it.
Several of the rules above started as a reviewer's concrete trigger that
reproduced before it was fixed.

## Screenshots

`scripts/screenshots.sh` builds spoor and `scripts/ansi2svg`, creates a sandbox
(`HOME` under `/tmp`, a fictional installer, a simulated Homebrew whose prefix has
the same length as `/opt/homebrew`), drives a tmux session through the tour and
captures each screen with colours. `ansi2svg` renders the capture as SVG with real
colours and vector box borders. Captures are relabelled (`/tmp/…` → `/opt/homebrew`)
without shifting columns, which is why the sandbox prefix length matters. System
state is disabled so nothing from the machine running the script appears.

## Releasing

1. `go vet ./...` and `go test ./...` on macOS; the Linux test binaries as user and
   root.
2. `scripts/tui-smoke.sh`.
3. If the UI changed, `scripts/screenshots.sh` and look at the images.
4. `scripts/prepublish-check.sh`. It fails on commit identities that are not
   GitHub noreply addresses, agent instruction files, home paths, IP addresses,
   token-shaped literals and e-mail addresses. Machine-specific names belong in
   `.git/info/private-patterns`, which is never committed.
5. Push.

Token-shaped test fixtures are assembled at run time so the source never contains
a literal that secret scanners flag.

## Known gaps and ideas

- `--trace` on macOS has only been verified against a fixture shaped like
  eslogger's JSON, not a live capture.
- `try` without root has only been exercised where the distribution blocks
  unprivileged user namespaces.
- Unloading system launch daemons during revert needs root and is untested.
- Undo of a Homebrew upgrade is deliberately left to brew.
- Ideas: continuous integration on macOS and Ubuntu runners; more knowledge-base
  rules (browser extensions, Homebrew services, language package managers); a
  shareable "footprint" registry of what popular installers do; a pruning command
  for old captured versions.
