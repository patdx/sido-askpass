#!/bin/sh
# sido-askpass — thin alias for `sido askpass`. sudo points SUDO_ASKPASS here.
# It resolves its own symlink and hands off to the sibling `sido` launcher, so
# platform detection lives in exactly one place.
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
exec "$bin/sido" askpass "$@"
