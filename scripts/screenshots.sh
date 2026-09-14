#!/bin/bash
# Regenerates the README screenshots in docs/images/.
#
# Everything runs in a throwaway sandbox: a fictional installer ("acme"), a
# simulated Homebrew with a handful of outdated formulae, a HOME under /tmp,
# no system state. spoor itself is the real binary, driven through tmux; each
# screen is captured with colours and rendered to SVG by scripts/ansi2svg.
# Needs: go, tmux, perl.
set -eu
REPO=$(cd "$(dirname "$0")/.." && pwd)
OUT="$REPO/docs/images"
W=$(mktemp -d /tmp/spoorshots.XXXXXX)
DEMO_HOME=/tmp/spoordemo
PREFIX=/tmp/spoorhbw # as long as /opt/homebrew, so captures can be relabelled without shifting columns
SOCK="$W/tmux.sock"
TMUX_BIN=$(command -v tmux)
T="$TMUX_BIN -S $SOCK"
cleanup() { $T kill-server 2>/dev/null || true; rm -rf "$W" "$DEMO_HOME" "$PREFIX"; }
trap cleanup EXIT
rm -rf "$DEMO_HOME" "$PREFIX"
mkdir -p "$OUT" "$W/bin" "$DEMO_HOME/Downloads" "$PREFIX/Cellar" "$PREFIX/opt"

(cd "$REPO" && go build -o "$W/bin/spoor" ./cmd/spoor && go build -o "$W/ansi2svg" ./scripts/ansi2svg)

nap() { perl -e "select(undef,undef,undef,$1)"; }

# ---------------------------------------------------------------- simulated Homebrew
keg() { # keg NAME VERSION: create an installed version with a few realistic files
	local d="$PREFIX/Cellar/$1/$2"
	mkdir -p "$d/bin" "$d/.brew" "$d/share/man/man1"
	printf 'class %s < Formula\n  desc "%s"\n  url "https://example.org/%s-%s.tar.gz"\n  sha256 "%s"\n  license "MIT"\nend\n' \
		"$(echo "$1" | perl -pe 's/^(.)/\u$1/')" "$1 command-line tool" "$1" "$2" "$(printf '%s%s' "$1" "$2" | shasum -a 256 | cut -c1-64)" >"$d/.brew/$1.rb"
	printf '{\n  "homebrew_version": "4.6.3",\n  "built_as_bottle": true,\n  "installed_on_request": true,\n  "source": {"version": "%s"}\n}\n' "$2" >"$d/INSTALL_RECEIPT.json"
	head -c 180000 /dev/urandom >"$d/bin/$1"
	chmod 755 "$d/bin/$1"
	printf '.TH %s 1 "%s"\n.SH NAME\n%s \\- %s command-line tool\n.SH OPTIONS\n.TP\n\\fB-a\\fR\nAll files are listed.\n' "$(echo "$1" | tr a-z A-Z)" "$2" "$1" "$1" >"$d/share/man/man1/$1.1"
	ln -sfn "../Cellar/$1/$2" "$PREFIX/opt/$1"
}
keg tree 2.2.1
printf 'Version 2.2.1 (07/09/2024)\n  - Fixed -J output for empty directories.\n  - Man page typos.\n' >"$PREFIX/Cellar/tree/2.2.1/CHANGES"
keg fzf 0.65.1
mkdir -p "$PREFIX/Cellar/fzf/0.65.1/shell"
printf 'bindkey -M emacs "^T" fzf-file-widget\nbindkey -M emacs "^R" fzf-history-widget\n' >"$PREFIX/Cellar/fzf/0.65.1/shell/key-bindings.zsh"
printf '0.65.1\n------\n- Bug fixes\n' >"$PREFIX/Cellar/fzf/0.65.1/CHANGELOG.md"
keg jq 1.8.1
keg htop 3.4.1
keg ripgrep 14.1.1
keg zlib 1.3.1

cat >"$W/bin/brew" <<'EOF'
#!/bin/sh
C="$HOMEBREW_CELLAR"; P=$(dirname "$C")
current() { case "$1" in tree) echo 2.3.2;; fzf) echo 0.74.3;; jq) echo 1.8.2;; htop) echo 3.5.3;; ripgrep) echo 15.1.0;; zlib) echo 1.3.2;; esac; }
installed() { ls "$C/$1" 2>/dev/null | tail -1; }
bump() {
	old=$(installed "$1"); new=$(current "$1"); [ "$old" = "$new" ] && return 0
	echo "==> Upgrading $1 $old -> $new"
	mkdir -p "$C/$1/$new"; cp -R "$C/$1/$old/." "$C/$1/$new/"
	d="$C/$1/$new"
	sed -i '' "s/$old/$new/g" "$d/.brew/$1.rb" "$d/INSTALL_RECEIPT.json" "$d/share/man/man1/$1.1"
	head -c 190000 /dev/urandom > "$d/bin/$1"; chmod 755 "$d/bin/$1"
	case "$1" in
	tree)
		printf 'Version 2.3.2 (08/20/2026)\n  - Added --gitignore-exclude to hide ignored files.\n  - -J now prints a trailing newline.\n\n' | cat - "$d/CHANGES" > "$d/CHANGES.new" && mv "$d/CHANGES.new" "$d/CHANGES"
		printf '.TP\n\\fB--gitignore-exclude\\fR\nHide files ignored by .gitignore.\n' >> "$d/share/man/man1/tree.1" ;;
	fzf)
		printf 'bindkey -M emacs "^T" fzf-file-widget\nbindkey -M emacs "^R" fzf-history-widget\nbindkey -M emacs "\\ec" fzf-cd-widget\n' > "$d/shell/key-bindings.zsh"
		printf '0.74.3\n------\n- New --tmux popup options\n- ALT-C now also bound in emacs mode\n\n' | cat - "$d/CHANGELOG.md" > "$d/c.new" && mv "$d/c.new" "$d/CHANGELOG.md" ;;
	esac
	ln -sfn "../Cellar/$1/$new" "$P/opt/$1"
	rm -rf "$C/$1/$old"
	echo "🍺  $C/$1/$new"
}
case "$1" in
--cellar) echo "$C" ;;
update) echo "Already up-to-date." ;;
outdated)
	o=""
	for n in fzf htop jq ripgrep tree zlib; do
		i=$(installed $n); c=$(current $n)
		[ -n "$i" ] && [ "$i" != "$c" ] && o="$o{\"name\":\"$n\",\"installed_versions\":[\"$i\"],\"current_version\":\"$c\",\"pinned\":false},"
	done
	echo "{\"formulae\":[${o%,}],\"casks\":[]}" ;;
deps) exit 0 ;;
info) exit 0 ;;
upgrade) shift 2; for n in "$@"; do bump "$n"; done ;;
esac
EOF
chmod 755 "$W/bin/brew"

# ---------------------------------------------------------------- fictional installer
cat >"$DEMO_HOME/Downloads/acme-install.sh" <<'EOF'
#!/bin/sh
echo "==> Installing acme 1.4.0"
mkdir -p "$HOME/.acme/bin" "$HOME/.local/bin" "$HOME/.config/acme" "$HOME/Library/LaunchAgents"
printf '#!/bin/sh\necho acme 1.4.0\n' > "$HOME/.acme/bin/acme" && chmod 755 "$HOME/.acme/bin/acme"
ln -sf "$HOME/.acme/bin/acme" "$HOME/.local/bin/acme"
cat > "$HOME/Library/LaunchAgents/com.acme.updater.plist" <<'P'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>Label</key><string>com.acme.updater</string>
  <key>ProgramArguments</key><array><string>/opt/acme/bin/acme-updater</string><string>--background</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StartInterval</key><integer>21600</integer>
</dict></plist>
P
printf '\n# >>> acme >>>\nexport PATH="$HOME/.acme/bin:$PATH"\neval "$(acme shell-init zsh)"\n# <<< acme <<<\n' >> "$HOME/.zshrc"
printf 'telemetry = true\nupdate_channel = "stable"\n' > "$HOME/.config/acme/config.toml"
echo "==> acme is ready. Restart your shell."
EOF
printf "alias ll='ls -lh'\nexport EDITOR=vim\n" >"$DEMO_HOME/.zshrc"
printf '[user]\n\tname = You\n' >"$DEMO_HOME/.gitconfig"

ENV=(env -i HOME="$DEMO_HOME" PATH="$W/bin:/usr/bin:/bin" TERM=xterm-256color LANG=en_US.UTF-8
	SPOOR_NO_STATE=1 HOMEBREW_CELLAR="$PREFIX/Cellar" EDITOR=vim)
"${ENV[@]}" "$W/bin/spoor" init --root "~:1" --root "~/.config:3" --root "~/Library/LaunchAgents:1" --root "~/.local/bin:1" --no-state >/dev/null
"${ENV[@]}" "$W/bin/spoor" snap -m "baseline" >/dev/null

# ---------------------------------------------------------------- driving the terminal
COLS=126 ROWS=34
"${ENV[@]}" "$TMUX_BIN" -S "$SOCK" -f /dev/null new-session -d -s t -x $COLS -y $ROWS \
	"PS1='\[\e[1;32m\]\$\[\e[0m\] ' bash --noprofile --norc"
$T set -g status off
nap 0.5
type_() { $T send-keys -t t -l "$1"; $T send-keys -t t Enter; }
keys() { for k in "$@"; do $T send-keys -t t "$k"; nap 0.35; done; }
wait_for() { # wait_for TEXT
	for _ in $(seq 1 80); do
		$T capture-pane -p -t t | grep -qF -- "$1" && { nap 0.4; return 0; }
		nap 0.25
	done
	echo "screenshots: timed out waiting for: $1" >&2
	$T capture-pane -p -t t >&2
	exit 1
}
shot() { # shot NAME TITLE
	$T capture-pane -e -p -t t |
		sed -e "s#$PREFIX#/opt/homebrew#g" -e "s#$DEMO_HOME#~#g" |
		"$W/ansi2svg" -title "$2" -cols $COLS >"$OUT/$1.svg"
	echo "  docs/images/$1.svg"
}

type_ 'clear; spoor run -m "curl -fsSL https://acme.example/install.sh | sh" -- sh ~/Downloads/acme-install.sh'
wait_for "Open the review now?"
shot run "spoor run"
keys Enter
wait_for "User LaunchAgent"
shot review "spoor review"
keys j
wait_for "zsh startup file"
shot review-shell "spoor review"
keys k x R
wait_for "Apply? [y/N]"
shot revert "spoor review — revert"
keys y
wait_for "recorded as commit"
keys Escape
wait_for "history ·"
shot history "spoor review — history"
keys q
nap 0.5
type_ 'clear; spoor upgrade'
wait_for "tick the ones to upgrade"
keys j j j j Space g Space
wait_for "2 of 6 selected"
shot upgrade-pick "spoor upgrade"
keys Enter
wait_for "Upgrade 2 package(s)?"
shot upgrade-plan "spoor upgrade"
keys Enter
wait_for "UPGRADE"
shot upgrade-review "spoor upgrade — review"
keys "]"
nap 0.5
wait_for "CHANGELOG.md"
shot upgrade-review-changelog "spoor upgrade — review"
keys q
nap 0.5
type_ 'clear; spoor log && echo && spoor blame ~/.local/bin/acme && echo && spoor explain ~/.zshrc'
wait_for "last change"
shot history-cli "spoor log · blame · explain"
type_ 'clear; spoor help revert'
wait_for "Examples:"
shot help "spoor help"
echo "done"
