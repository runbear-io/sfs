#!/bin/sh
# Build a stamped, universal BearDrive.app and zip it; optionally Taildrop it
# to a test machine.
#
#   ./dist.sh                 # build + zip
#   ./dist.sh macbook-pro-6   # build + zip + `tailscale file cp` to that host
#
# The version is <tauri.conf version>-dev+g<sha>[.dirty] so any machine can
# answer "which build is this?" via `BearDrive.app/Contents/MacOS/bdrive
# version` — never ship two different builds under one version string.
# Prereqs: go, node (npm install run once in desktop/), rustup with the
# x86_64-apple-darwin target (brew's keg lives off PATH, handled below).
set -e
cd "$(dirname "$0")/.."

export PATH="/opt/homebrew/opt/rustup/bin:$PATH"

BASE=$(python3 -c "import json;print(json.load(open('desktop/src-tauri/tauri.conf.json'))['version'])")
SHA=$(git rev-parse --short HEAD)
DIRTY=""
[ -n "$(git status --porcelain)" ] && DIRTY=".dirty"
V="${BASE}-dev+g${SHA}${DIRTY}"
LD="-s -w -X main.version=$V"
BIN=desktop/src-tauri/binaries

echo "building bdrive $V"
GOARCH=arm64 go build -ldflags "$LD" -o "$BIN/bdrive-aarch64-apple-darwin" ./cmd/bdrive
GOARCH=amd64 go build -ldflags "$LD" -o "$BIN/bdrive-x86_64-apple-darwin" ./cmd/bdrive
lipo -create -output "$BIN/bdrive-universal-apple-darwin" \
  "$BIN/bdrive-aarch64-apple-darwin" "$BIN/bdrive-x86_64-apple-darwin"

(cd desktop && npx tauri build --target universal-apple-darwin --bundles app)

B=desktop/src-tauri/target/universal-apple-darwin/release/bundle/macos
ZIP="BearDrive-$(printf %s "$V" | tr '+' '-').zip"
ditto -c -k --keepParent "$B/BearDrive.app" "$B/$ZIP"
echo "built $B/$ZIP"

if [ -n "$1" ]; then
  tailscale file cp "$B/$ZIP" "$1:"
  echo "sent to $1 (lands in its Downloads via Taildrop)"
fi
