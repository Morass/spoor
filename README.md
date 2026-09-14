# spoor

See what a command did to your machine, inspect every change, and undo any of it.

```
spoor run -- sh -c "$(curl -fsSL https://example.com/install.sh)"
spoor review            # interactive inspector
spoor revert HEAD --apply
```

spoor keeps its own **history of the parts of a system that installers
touch**: launch agents and daemons, login items, shell startup files,
commands on PATH, apps, config directories, crontab, loaded jobs,
listening ports. Every `run`, `snap`, `revert` and `restore` is a commit.
You can diff any two points, ask which commit put a file there, attach
notes to changes, and undo a whole commit or single paths. Undo is itself
a commit, so it can be undone too.

It is a single Go binary with no runtime dependencies. vim, `eslogger` and
`strace` are used when present, never required.

> Status: private work in progress. Not published.

## Quick start

```sh
go build -o spoor ./cmd/spoor

spoor snap -m baseline                 # record the machine as it is
spoor run -- brew install jq           # record what a command changes
spoor show                             # what HEAD changed, grouped by risk
spoor review                           # browse history, diffs, notes
spoor blame ~/Library/LaunchAgents/com.foo.updater.plist
spoor revert HEAD                      # dry-run plan
spoor revert HEAD --apply              # do it (recorded as a new commit)
```

After a run, spoor offers to open the review right away (`--review` opens
it without asking, `--no-review` skips the question).

### Package upgrades

`spoor run -- brew upgrade jq` automatically watches `jq`'s own Cellar
folder, with file contents. An upgrade creates a new version folder and
deletes the old one. The review pairs the two and shows each file as a real
diff under **UPGRADE**: changelog, man page, formula, headers. Binaries show
their size change. In the review, `]` / `[` jump between readable diffs.
Name the packages to get file diffs; a bare `brew upgrade` only records
which version folders appeared and disappeared.

Use it as a pure inspector without running anything through it:

```sh
spoor snap                 # now and then
spoor status               # what changed since, without recording
spoor review now           # inspect those live changes interactively
```

## Commands

| | |
|---|---|
| `run [-m MSG] [--trace] [--add-root SPEC] -- CMD` | record CMD's effects (exit code passed through), then offer the review |
| `snap [-m MSG]` | commit the current state if it changed |
| `try -- CMD` / `try apply REF` / `try discard REF` | Linux: run CMD on copy-on-write overlays, review, then apply or drop |
| `status [--patch]` | live changes since HEAD (nothing stored) |
| `log [-n N] [PATH]` | history, or the history of one path |
| `show [REF] [--patch] [--all]` | a commit's changes; `--all` includes noise |
| `diff A [B\|now] [--path P]` | compare any two points |
| `review [REF\|now]` | the interactive inspector |
| `blame PATH` | which commits touched PATH, and which process (with `--trace`) |
| `explain PATH` | what a path is, why it matters, what is inside |
| `note REF PATH [TEXT]` | read or write a note on one change |
| `revert REF [--apply] [--only CATS] [--path P] [--force] [--script F]` | undo a commit, all or part |
| `restore PATH --to REF [--before] [--apply]` | put one path back as it was at a commit |
| `quickfix REF [-o F]` | vim quickfix list of a commit's changes (`vim -q F`) |
| `export REF [--format md\|json] [--no-redact]` | shareable footprint, secrets and home paths redacted |
| `init [--root SPEC]... [--no-state] [--max-content N]` | configure what is watched |
| `roots`, `gc`, `version` | |

Refs: `HEAD`, `HEAD~2`, a commit id or a unique prefix.

## The inspector

`spoor review` opens the history. Press ⏎ on a commit for three panes: the
changes grouped by risk, the diff, and notes (an explanation of what the
path is, analyzer findings such as `KeepAlive: launchd restarts it whenever
it exits`, the process that wrote it, and your own note).

| key | |
|---|---|
| `j/k`, `tab` | move, switch pane |
| `]` / `[` | next / previous change with a readable diff |
| `J/K`, `space` | scroll the diff |
| `e` | open the live file in `$EDITOR` |
| `d` | `vimdiff` (or `nvim -d`, or `$SPOOR_DIFFTOOL`) of recorded before vs after |
| `o` / `Q` | open / write a vim quickfix list of the commit |
| `n` | write a note (ctrl+s saves) |
| `x` / `X` | mark a change / a whole category |
| `R` | revert the marked changes (or the selected one), with confirmation |
| `u` | write an undo shell script |
| `.` | show or hide noise (caches, logs, shell history) |
| `/` | filter |
| `n` (history) | review live changes since HEAD; `s` records a snapshot |

## How it works

**Watched roots.** A root is `PATH[:DEPTH[:content|meta]]`. Content roots
store file bodies of up to 1 MiB, so they can be diffed and restored.
Metadata roots record type, size, mtime and mode, which is enough to see
that something appeared or changed. The built-in profile per OS covers the
usual install targets. `spoor roots` prints it, and `spoor init --root ...`
replaces it. A full snapshot of the default macOS profile took 0.6 s and
2 MB; `status` took 0.04 s.

**Store.** `$SPOOR_HOME` (default `~/.local/share/spoor`, mode 0700) holds
content-addressed objects, compressed manifests, commits, notes and traces.
On APFS, bodies are captured with `clonefile(2)`, which is instant and shares
disk blocks until the file changes.

**System state.** Loaded launchd jobs, crontab and listening TCP ports
(systemd user units on Linux) are recorded as virtual `@state/...` entries
and diffed like files.

**Drift.** Before recording a command, spoor compares the machine with
HEAD. Anything that changed in between is committed first as `drift`, so
history stays continuous and `blame` can say "changed outside spoor".

**Classification.** A small knowledge base maps paths to categories
(persistence, trust, shell, path, apps, config, state, data, noise) with an
explanation and a risk level. Analyzers read inside the changes: launchd
plists (binary or XML), systemd units, lines added to shell files (PATH
edits, `eval`, `curl | sh`, secrets), binary kind and code signature.

## Undo semantics

Revert plans against the **live** machine, not just the recorded state:

- A path changed again after the commit is a **conflict** and is left alone
  unless `--force`.
- Added LaunchAgents and daemons are unloaded with `launchctl bootout`
  before their plists are removed.
- A directory the commit created is removed as a whole tree, including
  contents deeper than the watched depth such as a cloned toolchain. It is
  a conflict instead if anything inside was modified after the commit.
- Content that was never captured (metadata roots, files over the size cap,
  secrets) is reported as **unrestorable**. It is never guessed.
- Noise is not reverted by default, except things the commit created.
- Every applied revert or restore is a new commit.

## Privacy

- Bodies of key-like files (`~/.ssh/id_*`, `*.pem`, `.netrc`, credentials,
  anything matching `token` or `secret`) are hashed but never copied into
  the store. Changes are still detected.
- `export` redacts tokens, private keys, URL credentials and `key=value`
  secrets, and replaces the home directory with `~`.
- On macOS, folders protected by privacy controls are skipped with one
  warning. Grant your terminal Full Disk Access to include them.

## Tracing and previews

- `run --trace` attributes each write to a process. On macOS it uses
  `eslogger`, which needs root (`sudo -v` first). On Linux it uses `strace`.
- `try` (Linux) mounts overlays over the watched directories in a private
  mount namespace, so the command's writes land in a workspace. Review it
  like any commit, then `try apply` or `try discard`. It needs root, or
  unprivileged user namespaces. Ubuntu restricts those via AppArmor, and
  spoor says so. A user who is not root only gets overlays inside home. It
  is not a security sandbox: network and everything outside the overlays
  are untouched by it.

## Limits

- Only watched roots are seen. A write elsewhere is invisible unless you
  add a root, or `--trace` shows it.
- Inside metadata roots, a modified file can be detected but not restored.
- Loaded-job state covers your GUI session. System daemons need root to
  bootout.

## Development

```sh
go test ./...                 # unit, TUI model and sandboxed end-to-end tests
scripts/tui-smoke.sh          # drives the real UI in tmux (vimdiff, notes, revert)
```

The end-to-end tests run the real binary against a throwaway `HOME`. They
check byte-for-byte that `revert` restores the baseline tree, that reverting
the revert restores the installed tree, and that conflicts, forced reverts,
restore, drift, blame, gc and redaction behave as described.

Linux, cross-compiled from a Mac:

```sh
GOOS=linux GOARCH=amd64 go build -o spoor-linux ./cmd/spoor
GOOS=linux GOARCH=amd64 go test -c -o e2e.test ./e2e
# on the Linux box:
SPOOR_BIN=./spoor-linux ./e2e.test -test.v
sudo env SPOOR_BIN=./spoor-linux ./e2e.test -test.v -test.run 'Try|Trace'
```

Layout: `cmd/spoor` (CLI), `internal/app` (operations), `store`, `scan`,
`diff`, `kb` (knowledge base and analyzers), `revert`, `trace`, `try`,
`redact`, `tui`, `e2e/`.
