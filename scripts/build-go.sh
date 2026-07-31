#!/usr/bin/env bash
# Build the sido binaries into ../dist/, next to the launchers.
# Produces two single binaries (one per OS) + the OS-detecting shell launchers:
#   dist/sido-mac     (darwin/arm64)
#   dist/sido-linux   (linux/amd64)
#   dist/sido         (shell launcher -> sido-mac|sido-linux)
#   dist/sido-askpass (shell launcher -> <bin> askpass)
# Pure Go (no cgo) so cross-compilation works from any host.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root/go-src"

version="$(grep -m1 '"version"' "$root/package.json" | sed -E 's/.*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"

mkdir -p "$root/dist"
rm -f "$root/dist"/sido "$root/dist"/sido-askpass "$root/dist"/sido-mac "$root/dist"/sido-linux
ldflags="-s -w -X sido-go/internal/sido.Version=${version}"

GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$ldflags" -o "$root/dist/sido-mac"   ./cmd/sido
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$ldflags" -o "$root/dist/sido-linux" ./cmd/sido

cp "$root/scripts/sido.sh"         "$root/dist/sido";         chmod +x "$root/dist/sido"
cp "$root/scripts/sido-askpass.sh" "$root/dist/sido-askpass"; chmod +x "$root/dist/sido-askpass"

echo "built dist/{sido, sido-askpass, sido-mac, sido-linux} (${version})"
