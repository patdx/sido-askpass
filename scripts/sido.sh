#!/bin/sh
# sido — platform launcher. Resolves its own symlink (so it works behind npm's
# bin symlinks), picks the sido-<os>-<arch> binary sitting next to it, and execs
# it. The only shell involved at runtime; no detection logic lives in Go.
self=$0
i=0
while [ -L "$self" ] && [ "$i" -lt 20 ]; do
	l=$(readlink "$self") || break
	case $l in
		/*) self=$l ;;
		*) self=$(dirname "$self")/$l ;;
	esac
	i=$((i + 1))
done
bin=$(cd "$(dirname "$self")" && pwd)

case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*)
		echo "[sido] unsupported OS: $(uname -s); supported: Linux, Darwin" >&2
		exit 1
		;;
esac

case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*)
		echo "[sido] unsupported architecture: $(uname -m); supported: x86_64/amd64, arm64/aarch64" >&2
		exit 1
		;;
esac

exe=$bin/sido-$os-$arch
if [ ! -x "$exe" ]; then
	echo "[sido] missing binary for $os/$arch: $exe" >&2
	exit 1
fi
exec "$exe" "$@"
