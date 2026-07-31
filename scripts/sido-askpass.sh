#!/bin/sh
# sido-askpass — thin alias for `sido askpass`. sudo points SUDO_ASKPASS here;
# this launcher picks the right platform binary and forwards to askpass mode.
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
exec "$exe" askpass "$@"
