#!/bin/sh
# Downloads Google's platform-tools (adb) for one OS into src-tauri/platform-tools/,
# where tauri.conf.json bundles it. Usage: scripts/fetch-platform-tools.sh linux|darwin|windows
set -eu
os="${1:?usage: $0 linux|darwin|windows}"
here="$(cd "$(dirname "$0")/.." && pwd)"
dest="$here/src-tauri/platform-tools"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/pt.zip" "https://dl.google.com/android/repository/platform-tools-latest-$os.zip"
unzip -q "$tmp/pt.zip" -d "$tmp"
rm -rf "$dest"
mkdir -p "$dest"
# adb and what it loads at runtime; fastboot, systrace etc. only add size.
case "$os" in
  windows) cp "$tmp"/platform-tools/adb.exe "$tmp"/platform-tools/AdbWinApi.dll "$tmp"/platform-tools/AdbWinUsbApi.dll "$dest"/ ;;
  *)       cp "$tmp"/platform-tools/adb "$dest"/ ; chmod +x "$dest/adb" ;;
esac
cp "$tmp"/platform-tools/NOTICE.txt "$dest"/ 2>/dev/null || true
echo "platform-tools ($os) -> $dest"
