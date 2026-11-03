#!/bin/sh
# Install the latest Engram release for this machine:
#   curl -fsSL https://raw.githubusercontent.com/zakeripouya/EngramContextProtocol/main/install.sh | sh
# Set ENGRAM_VERSION=0.11.0 for a specific version, ENGRAM_BIN_DIR to choose where it goes.
set -eu
repo="zakeripouya/EngramContextProtocol"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "engram: unsupported architecture $arch; build from source: go install github.com/$repo/cmd/engram@latest" >&2; exit 1 ;;
esac
case "$os" in
  linux|darwin) ;;
  *) echo "engram: use the Windows .zip from https://github.com/$repo/releases" >&2; exit 1 ;;
esac
version="${ENGRAM_VERSION:-}"
if [ -z "$version" ]; then
  version="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n1)"
fi
[ -n "$version" ] || { echo "engram: could not find the latest release" >&2; exit 1; }
name="engram_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/v${version}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
echo "Downloading engram $version for $os/$arch"
curl -fsSL -o "$tmp/$name" "$base/$name"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
expected="$(grep " $name\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
if command -v sha256sum >/dev/null 2>&1; then actual="$(sha256sum "$tmp/$name" | cut -d' ' -f1)"; else actual="$(shasum -a 256 "$tmp/$name" | cut -d' ' -f1)"; fi
[ -n "$expected" ] && [ "$expected" = "$actual" ] || { echo "engram: checksum mismatch; not installing" >&2; exit 1; }
tar -xzf "$tmp/$name" -C "$tmp"
dir="${ENGRAM_BIN_DIR:-$HOME/.local/bin}"
mkdir -p "$dir"
install -m 0755 "$tmp/engram" "$dir/engram"
echo "Installed $dir/engram"
case ":$PATH:" in *":$dir:"*) ;; *) echo "Add $dir to your PATH, then run: engram init" ;; esac
case ":$PATH:" in *":$dir:"*) echo "Next: engram init" ;; esac
