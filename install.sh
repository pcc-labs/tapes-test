#!/bin/sh
# Installs the latest tapes-skill-report release. No Go needed.
#
#   curl -fsSL https://raw.githubusercontent.com/pcc-labs/tapes-skill-report/main/install.sh | sh
#
# TAPES_INSTALL_DIR picks the directory (default ~/.local/bin).
set -eu

repo="pcc-labs/tapes-skill-report"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "install: unsupported CPU: $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin | linux) ;;
  *) echo "install: unsupported OS: $os (macOS and Linux only; on Windows use WSL)" >&2; exit 1 ;;
esac

name="tapes-skill-report-$os-$arch"
url="https://github.com/$repo/releases/latest/download/$name"
dir="${TAPES_INSTALL_DIR:-$HOME/.local/bin}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading $name"
curl -fsSL "$url" -o "$tmp/$name"
curl -fsSL "$url.sha256" -o "$tmp/$name.sha256"
if command -v shasum >/dev/null 2>&1; then
  (cd "$tmp" && shasum -a 256 -c "$name.sha256" >/dev/null)
else
  (cd "$tmp" && sha256sum -c "$name.sha256" >/dev/null)
fi

mkdir -p "$dir"
mv "$tmp/$name" "$dir/tapes-skill-report"
chmod +x "$dir/tapes-skill-report"
echo "installed $dir/tapes-skill-report"

case ":$PATH:" in
  *":$dir:"*) echo "next: tapes-skill-report check" ;;
  *)
    echo "$dir is not on your PATH. Add it, then open a new terminal:"
    echo "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.zshrc"
    echo "or run it by its full path: $dir/tapes-skill-report check"
    ;;
esac
