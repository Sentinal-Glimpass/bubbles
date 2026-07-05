#!/usr/bin/env bash
#
# Bubbles one-command installer. Installs everything needed and builds bubbles:
#   - Go (build toolchain)            -> ~/.local/go   (only if missing/too old)
#   - Claude Code (the `claude` CLI)  -> official installer (only if missing)
#   - ngrok (optional, public webhooks) -> ~/.local/bin (only if missing)
#   - bubbles                         -> ~/.local/bin
#
# No sudo: everything lands under $HOME. Idempotent: re-run any time.
#
# Quick start:
#   curl -fsSL https://raw.githubusercontent.com/Sentinal-Glimpass/bubbles/main/install.sh | bash
# Options (append after `bash -s --`): --no-ngrok
set -euo pipefail

GO_MIN="1.25"
REPO="github.com/Sentinal-Glimpass/bubbles"
BIN_DIR="$HOME/.local/bin"
GO_DIR="$HOME/.local/go"
WITH_NGROK=1
ORIG_PATH="$PATH" # the user's shell PATH, before this script prepends its own dirs

for a in "$@"; do
	case "$a" in
	--no-ngrok) WITH_NGROK=0 ;;
	--with-ngrok) WITH_NGROK=1 ;;
	esac
done

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
log()  { printf '  \033[32m›\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
die()  { printf '  \033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# on_path tests the USER's original shell PATH (not the script's mutated one), so
# "does the command work after this exits" is judged correctly.
on_path() { case ":$ORIG_PATH:" in *":$1:"*) return 0 ;; *) return 1 ;; esac; }

# pick_bindir chooses where to put `bubbles` so it works in the CURRENT shell
# right away: a writable directory already on PATH (preferring ~/.local/bin, the
# canonical spot where claude/ngrok also land). Only if none is on PATH do we fall
# back to ~/.local/bin and update the shell rc.
pick_bindir() {
	local d
	for d in "$HOME/.local/bin" "$HOME/bin" "$HOME/go/bin"; do
		if on_path "$d" && { [ -d "$d" ] && [ -w "$d" ] || mkdir -p "$d" 2>/dev/null; }; then
			echo "$d"; return
		fi
	done
	local p
	IFS=: read -ra _p <<<"$ORIG_PATH"
	for p in "${_p[@]}"; do
		[ -n "$p" ] && [ -d "$p" ] && [ -w "$p" ] && case "$p" in "$HOME"/*) echo "$p"; return ;; esac
	done
	echo "$HOME/.local/bin"
}

detect_platform() {
	OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
	case "$OS" in linux | darwin) ;; *) die "unsupported OS: $OS (Linux/macOS only)";; esac
	ARCH="$(uname -m)"
	case "$ARCH" in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*) die "unsupported arch: $ARCH" ;;
	esac
}

# ver_ge A B -> true if A >= B (dotted versions)
ver_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]; }

ensure_go() {
	local v=""
	if have go; then v="$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"; fi
	if [ -n "$v" ] && ver_ge "$v" "$GO_MIN"; then
		log "Go $v already present"
		return
	fi
	if [ -x "$GO_DIR/bin/go" ]; then
		export PATH="$GO_DIR/bin:$PATH"
		v="$($GO_DIR/bin/go version | awk '{print $3}' | sed 's/^go//')"
		if ver_ge "$v" "$GO_MIN"; then log "Go $v already present (~/.local/go)"; return; fi
	fi
	local ver url
	ver="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)" || die "could not reach go.dev"
	url="https://go.dev/dl/${ver}.${OS}-${ARCH}.tar.gz"
	log "installing $ver -> $GO_DIR"
	rm -rf "$GO_DIR"
	mkdir -p "$HOME/.local"
	curl -fsSL "$url" | tar -C "$HOME/.local" -xzf - || die "Go download failed ($url)"
	export PATH="$GO_DIR/bin:$PATH"
	PERSIST_GO=1
}

ensure_claude() {
	if have claude; then
		log "Claude Code present ($(claude --version 2>/dev/null | head -1))"
		return
	fi
	log "installing Claude Code (claude CLI)"
	curl -fsSL https://claude.ai/install.sh | bash || die "Claude Code install failed"
	have claude || export PATH="$HOME/.local/bin:$PATH"
	NEED_CLAUDE_AUTH=1
}

install_bubbles() {
	mkdir -p "$BIN_DIR"
	if [ -f go.mod ] && grep -q "$REPO" go.mod 2>/dev/null; then
		log "building bubbles from source (this repo)"
		go build -o "$BIN_DIR/bubbles" ./cmd/bubbles || die "build failed"
	else
		log "installing bubbles from $REPO"
		GOBIN="$BIN_DIR" go install "$REPO/cmd/bubbles@latest" || die "go install failed"
	fi
	log "installed -> $BIN_DIR/bubbles"
}

ensure_ngrok() {
	[ "$WITH_NGROK" = 1 ] || { log "ngrok skipped (--no-ngrok)"; return; }
	if have ngrok; then log "ngrok present"; return; fi
	log "installing ngrok (optional — for public webhook URLs)"
	local url="https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-${OS}-${ARCH}.tgz"
	if curl -fsSL "$url" | tar -C "$BIN_DIR" -xzf - 2>/dev/null && [ -x "$BIN_DIR/ngrok" ]; then
		log "ngrok installed -> $BIN_DIR/ngrok"
		warn "ngrok needs an authtoken: sign up free at ngrok.com, then 'ngrok config add-authtoken <token>'"
	else
		warn "ngrok install skipped (optional) — bubbles works without it; add it later for 'bubbles --ngrok'"
	fi
}

rc_file() {
	case "$(basename "${SHELL:-bash}")" in
	zsh) echo "$HOME/.zshrc" ;;
	*) echo "$HOME/.bashrc" ;;
	esac
}

ensure_path() {
	local rc; rc="$(rc_file)"
	# Persist Go's bin for future shells if we installed it.
	if [ "${PERSIST_GO:-0}" = 1 ] && ! on_path "$GO_DIR/bin"; then
		echo "export PATH=\"$GO_DIR/bin:\$PATH\"" >>"$rc"
	fi
	# bubbles was placed in an on-PATH dir when one was available (see pick_bindir),
	# so the common case needs nothing. Only if BIN_DIR is off PATH do we fix the rc.
	if on_path "$BIN_DIR"; then
		BUBBLES_READY=1
		return
	fi
	echo "export PATH=\"$BIN_DIR:\$PATH\"" >>"$rc"
	NEED_PATH_FIX=1
}

main() {
	bold "Bubbles installer"
	detect_platform
	BIN_DIR="$(pick_bindir)"
	log "platform: $OS/$ARCH   install dir: $BIN_DIR"
	ensure_go
	ensure_claude
	install_bubbles
	ensure_ngrok
	ensure_path
	echo
	bold "Done."
	if [ "${NEED_CLAUDE_AUTH:-0}" = 1 ]; then
		warn "Authenticate Claude Code first: run 'claude' once and sign in."
	fi
	if [ "${NEED_PATH_FIX:-0}" = 1 ]; then
		warn "Make 'bubbles' available in THIS shell now:"
		echo "      export PATH=\"$BIN_DIR:\$PATH\"        # (already added to $(rc_file) for new shells)"
	fi
	echo "  Then, from any project directory:  bubbles"
	echo "  Public webhooks (needs ngrok authtoken):  bubbles --ngrok"
}

if [ "${BASH_SOURCE[0]:-$0}" = "$0" ]; then main "$@"; fi
