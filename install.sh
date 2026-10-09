#!/usr/bin/env bash
# Installs the latest ptywrap release binary.
#
#   curl -fsSL https://raw.githubusercontent.com/DSNR/ptywrap/main/install.sh | bash
#
# Env vars:
#   PTYWRAP_VERSION   release tag to install (default: latest)
#   PTYWRAP_INSTALL_DIR  install directory (default: ~/.local/bin, falls back to /usr/local/bin with sudo)

set -euo pipefail

REPO="DSNR/ptywrap"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  linux|darwin) ;;
  *) echo "unsupported OS: $os" >&2; exit 1 ;;
esac

version="${PTYWRAP_VERSION:-latest}"
if [ "$version" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/ptywrap-${os}-${arch}.tar.gz"
else
  url="https://github.com/${REPO}/releases/download/${version}/ptywrap-${os}-${arch}.tar.gz"
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

echo "downloading $url" >&2
curl -fsSL "$url" -o "$tmpdir/ptywrap.tar.gz"
tar -xzf "$tmpdir/ptywrap.tar.gz" -C "$tmpdir"

bin="$tmpdir/ptywrap-${os}-${arch}"
chmod +x "$bin"

install_dir="${PTYWRAP_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$install_dir"
mv "$bin" "$install_dir/ptywrap"

echo "installed ptywrap to $install_dir/ptywrap" >&2
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "note: $install_dir is not on your PATH" >&2 ;;
esac
