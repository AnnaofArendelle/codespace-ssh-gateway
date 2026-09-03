#!/bin/sh
# Install ssh-gateway.
#
#   curl -fsSL https://raw.githubusercontent.com/AnnaofArendelle/codespace-ssh-gateway/main/install.sh | sh
#
# Downloads the released binary for this platform, or builds from source when Go
# is available. Set PREFIX to choose the install directory (default ~/.local/bin).
set -eu

REPO="AnnaofArendelle/codespace-ssh-gateway"
PREFIX="${PREFIX:-$HOME/.local/bin}"
NAME="gateway"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported OS $(uname -s); build from source with: go install github.com/$REPO@latest" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac

asset="${NAME}_${os}_${arch}"
url="https://github.com/$REPO/releases/latest/download/$asset"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

say "Fetching $asset ..."
if command -v curl >/dev/null 2>&1 && curl -fsSL "$url" -o "$tmp/$NAME"; then
	:
elif command -v wget >/dev/null 2>&1 && wget -qO "$tmp/$NAME" "$url"; then
	:
elif command -v go >/dev/null 2>&1; then
	say "No release binary available; building from source with Go ..."
	GOBIN="$tmp" go install "github.com/$REPO@latest" || die "go install failed"
	mv "$tmp/codespace-ssh-gateway" "$tmp/$NAME" 2>/dev/null || true
else
	die "could not download $url and Go is not installed"
fi

mkdir -p "$PREFIX"
chmod +x "$tmp/$NAME"
mv "$tmp/$NAME" "$PREFIX/$NAME"
say "Installed $PREFIX/$NAME"

case ":$PATH:" in
*":$PREFIX:"*) ;;
*) say ""
	say "$PREFIX is not on your PATH. Add this to your shell profile:"
	say "  export PATH=\"$PREFIX:\$PATH\"" ;;
esac

say ""
say "Next: run '$NAME' — it walks you through setup and then starts serving."
