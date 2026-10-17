#!/bin/sh
# Writes the dynamic dependencies of one built binary, as the platform tools
# report them, so the notices can be checked against what was built.
#
# Usage: scripts/release/linkage.sh BINARY OUTFILE
# Linux: readelf -d (NEEDED) and ldd. macOS: otool -L. Windows (Git Bash
# with the MSVC environment): dumpbin /DEPENDENTS.
set -eu

[ $# -eq 2 ] || { printf 'usage: %s BINARY OUTFILE\n' "$0" >&2; exit 2; }
bin=$1
out=$2
[ -f "$bin" ] || { printf 'linkage: no such file: %s\n' "$bin" >&2; exit 1; }

{
	printf 'file: %s\n' "$(basename "$bin")"
	printf 'uname: %s %s\n' "$(uname -s)" "$(uname -m)"
	printf 'sha256: %s\n' "$(if command -v sha256sum >/dev/null 2>&1; then sha256sum "$bin" | awk '{ print $1 }'; else shasum -a 256 "$bin" | awk '{ print $1 }'; fi)"
	case $(uname -s) in
	Linux)
		printf '\n# readelf -d (NEEDED)\n'
		readelf -d "$bin" | grep NEEDED || true
		printf '\n# ldd\n'
		ldd "$bin" || true
		;;
	Darwin)
		printf '\n# otool -L\n'
		otool -L "$bin"
		;;
	MINGW* | MSYS* | CYGWIN* | Windows_NT)
		printf '\n# dumpbin /DEPENDENTS\n'
		dumpbin //DEPENDENTS "$bin"
		;;
	*)
		printf 'linkage: unsupported host %s\n' "$(uname -s)" >&2
		exit 1
		;;
	esac
} >"$out"
