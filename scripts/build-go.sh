#!/usr/bin/env bash
# Build the sido binaries into ../dist/, next to the launchers.
# Produces four single binaries (one per OS/arch) + the platform-detecting
# shell launchers:
#   dist/sido-linux-amd64    dist/sido-linux-arm64
#   dist/sido-darwin-amd64   dist/sido-darwin-arm64
#   dist/sido         (shell launcher -> sido-<os>-<arch>)
#   dist/sido-askpass (shell launcher -> sido askpass)
# Pure Go (no cgo) so cross-compilation works from any host.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root/go-src"

version="$(grep -m1 '"version"' "$root/package.json" | sed -E 's/.*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"

mkdir -p "$root/dist"
rm -f "$root/dist"/sido "$root/dist"/sido-*
ldflags="-s -w -X sido-go/internal/sido.Version=${version}"
gcflags="all=-l" # Disabling inlining reduces the size of this command-line binary.

for os_arch in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os=${os_arch%/*}
	arch=${os_arch#*/}
	GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -gcflags "$gcflags" \
		-o "$root/dist/sido-$os-$arch" ./cmd/sido
done

cp "$root/scripts/sido.sh"         "$root/dist/sido";         chmod +x "$root/dist/sido"
cp "$root/scripts/sido-askpass.sh" "$root/dist/sido-askpass"; chmod +x "$root/dist/sido-askpass"

echo "built dist/{sido, sido-askpass, sido-{linux,darwin}-{amd64,arm64}} (${version})"
