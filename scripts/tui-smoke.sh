#!/bin/bash
# Drives the real review UI in a detached tmux session against a sandboxed
# HOME and asserts on what is actually drawn: navigation, vimdiff hand-off,
# notes, mark + revert. Complements the model tests in internal/tui.
set -u
REPO=$(cd "$(dirname "$0")/.." && pwd)
W=$(mktemp -d)
SOCK="$W/s"
T="tmux -S $SOCK"
fails=0
trap '$T kill-server 2>/dev/null; rm -rf "$W"' EXIT

nap() { perl -e "select(undef,undef,undef,$1)"; }
screen() { $T capture-pane -p -t t; }
expect() { # expect LABEL PATTERN
	if screen | grep -qF -- "$2"; then echo "ok   $1"; else echo "FAIL $1 (no \"$2\")"; screen | sed 's/^/     | /' | head -20; fails=$((fails + 1)); fi
}

(cd "$REPO" && go build -o "$W/spoor" ./cmd/spoor) || exit 1
export HOME="$W/home" SPOOR_HOME="$W/repo" SPOOR_NO_STATE=1
mkdir -p "$HOME"
printf "alias ll='ls -l'\n" > "$HOME/.zshrc"
"$W/spoor" init --root "$HOME:1" --root "$HOME/.config:2" --no-state > /dev/null
"$W/spoor" snap > /dev/null
"$W/spoor" run -- sh -c 'mkdir -p "$HOME/.config"; echo "k = 1" > "$HOME/.config/app.toml"; printf "export PATH=\$HOME/.x/bin:\$PATH\n" >> "$HOME/.zshrc"' 2> /dev/null
ID=$(cat "$SPOOR_HOME/HEAD")

env -i HOME="$HOME" SPOOR_HOME="$SPOOR_HOME" SPOOR_NO_STATE=1 TERM=xterm-256color LANG=en_US.UTF-8 \
	PATH="$PATH" EDITOR=vim tmux -S "$SOCK" new-session -d -s t -x 160 -y 40 "$W/spoor review $ID; sleep 5"
nap 1.5
expect "review opens on the commit" "$ID"
expect "shell change listed" "~/.zshrc"
expect "diff pane shows added line" '+export PATH=$HOME/.x/bin:$PATH'
expect "notes pane explains" "zsh startup file"

$T send-keys -t t j; nap 0.4
expect "j moves selection" "Configuration file"

if command -v vimdiff > /dev/null; then
	$T send-keys -t t d; nap 1.2
	expect "d opens vimdiff" "k = 1"
	$T send-keys -t t ':qa!' Enter; nap 0.8
	expect "back from vimdiff" "Configuration file"
fi

$T send-keys -t t n; nap 0.3
$T send-keys -t t -l "created by the test"
$T send-keys -t t C-s; nap 0.5
expect "note saved" "created by the test"

$T send-keys -t t x R; nap 0.6
expect "confirm lists the plan" "Apply? [y/N]"
$T send-keys -t t y; nap 1.5
expect "revert applied and recorded" "recorded as commit"
[ -e "$HOME/.config/app.toml" ] && { echo "FAIL marked file still on disk"; fails=$((fails + 1)); } || echo "ok   marked file reverted on disk"
[ "$(cat "$HOME/.zshrc")" = "alias ll='ls -l'" ] && { echo "FAIL unmarked change reverted too"; fails=$((fails + 1)); } || echo "ok   unmarked change kept"

$T send-keys -t t Escape; nap 0.6
expect "history shows the revert" "revert 1 path(s) of $ID"
$T send-keys -t t q; nap 0.5

[ "$("$W/spoor" note "$ID" "$HOME/.config/app.toml")" = "created by the test" ] && echo "ok   note persisted" || { echo "FAIL note not persisted"; fails=$((fails + 1)); }
echo "$fails failure(s)"
exit $((fails > 0))
