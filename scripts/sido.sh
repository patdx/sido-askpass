#!/bin/sh
# sido — OS-detecting launcher. Resolves symlinks (so it works behind npm's bin
# symlinks), then execs the platform binary (sido-mac / sido-linux) sitting next
# to it in the package.
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
exe=$bin/sido-linux
[ "$(uname)" = Darwin ] && exe=$bin/sido-mac
exec "$exe" "$@"
