#!/bin/sh
# Install hermes-go from a GitHub release on Linux or macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/umi008/hermes-agent-go/main/scripts/install.sh | sh
#
# Environment:
#   HERMES_GO_VERSION      release tag to install, e.g. v0.2.0 (default: latest)
#   HERMES_GO_INSTALL_DIR  target directory (default: ~/.local/bin)
#   HERMES_GO_DOWNLOAD_URL base URL of the release assets (default: GitHub)
#
# The archive is verified against the release's checksums.txt before
# anything is installed.
set -eu

REPO="umi008/hermes-agent-go"
INSTALL_DIR="${HERMES_GO_INSTALL_DIR:-$HOME/.local/bin}"
DOWNLOAD_URL="${HERMES_GO_DOWNLOAD_URL:-https://github.com/$REPO/releases/download}"

say() { printf 'hermes-go install: %s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }

# fetch URL FILE downloads with curl or wget; FILE "-" writes to stdout.
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "curl or wget is required"
	fi
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d ' ' -f 1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d ' ' -f 1
	else
		die "sha256sum or shasum is required to verify the download"
	fi
}

case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	MINGW* | MSYS* | CYGWIN*) die "on Windows use scripts/install.ps1 from PowerShell" ;;
	*) die "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture: $(uname -m)" ;;
esac

version="${HERMES_GO_VERSION:-}"
if [ -z "$version" ]; then
	version="$(fetch "https://api.github.com/repos/$REPO/releases/latest" - |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
	[ -n "$version" ] || die "could not find the latest release; set HERMES_GO_VERSION"
fi
case "$version" in v*) ;; *) version="v$version" ;; esac

archive="hermes-go_${version#v}_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "downloading $archive ($version)"
fetch "$DOWNLOAD_URL/$version/$archive" "$tmp/$archive" || die "download failed: $DOWNLOAD_URL/$version/$archive"
fetch "$DOWNLOAD_URL/$version/checksums.txt" "$tmp/checksums.txt" || die "download failed: checksums.txt"

want="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
got="$(sha256 "$tmp/$archive")"
[ "$got" = "$want" ] || die "checksum mismatch for $archive (want $want, got $got); nothing was installed"

tar -xzf "$tmp/$archive" -C "$tmp" hermes-go || die "archive has no hermes-go binary"
mkdir -p "$INSTALL_DIR"
# Copy then rename, so a running hermes-go is never half-overwritten.
cp "$tmp/hermes-go" "$INSTALL_DIR/.hermes-go.new"
chmod 755 "$INSTALL_DIR/.hermes-go.new"
mv -f "$INSTALL_DIR/.hermes-go.new" "$INSTALL_DIR/hermes-go"
say "installed $("$INSTALL_DIR/hermes-go" version) to $INSTALL_DIR/hermes-go"

case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*) say "$INSTALL_DIR is not on your PATH; add this to your shell profile:"
	   say "  export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
say "next: set a provider key (e.g. OPENROUTER_API_KEY) and run: hermes-go doctor"
