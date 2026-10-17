#!/bin/sh
# Fetch the pinned SQLite amalgamation and build the sqlite3 example server from it, and print the
# environment that the Go tests of this directory read.
#
# Usage: bindgen/examples/sqlite3/build.sh [-T] [-b BUILD_DIR] [-d AMALGAMATION_DIR] [-s SANITIZER]
#   -b BUILD_DIR  build directory root, outside the repository
#                 (default: ${TMPDIR:-/tmp}/hqq-sqlite3gen-build)
#   -d DIR        use this unpacked SQLite amalgamation (the directory with sqlite3.c and sqlite3.h)
#                 instead of running scripts/release/fetch-sqlite.sh
#   -T            test build: compile the test-only hooks (SQLITE3GEN_TEST_HOOKS), which the tests of the
#                 retain-on-fail path of sqlite3_close need. Never for a server that serves anything.
#   -s SANITIZER  also build the server with this HQQ_SANITIZE value (for example address+undefined)
#                 and point HQQ_SQLITE3GEN_SERVER at it
#
# stdout is a script for `eval`: an export of HQQ_SQLITE3GEN_SERVER (the server binary). Progress
# goes to stderr.
#
#   eval "$(bindgen/examples/sqlite3/build.sh)"
#   go test -race -count=1 ./bindgen/examples/sqlite3/...
set -eu

script_dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$script_dir/../../.." && pwd)

die() {
	printf 'build.sh: %s\n' "$*" >&2
	exit 1
}

build=${TMPDIR:-/tmp}/hqq-sqlite3gen-build
adir=
san=
hooks=OFF
while getopts "Tb:d:s:" opt; do
	case $opt in
	T) hooks=ON ;;
	b) build=$OPTARG ;;
	d) adir=$OPTARG ;;
	s) san=$OPTARG ;;
	*) die "usage: $0 [-T] [-b BUILD_DIR] [-d AMALGAMATION_DIR] [-s SANITIZER]" ;;
	esac
done
shift $((OPTIND - 1))
[ $# -eq 0 ] || die "usage: $0 [-T] [-b BUILD_DIR] [-d AMALGAMATION_DIR] [-s SANITIZER]"

command -v cmake >/dev/null 2>&1 || die "cmake is required"
command -v ninja >/dev/null 2>&1 || die "ninja is required"

case $build in /*) ;; *) build=$PWD/$build ;; esac
case $build in
"$root" | "$root"/*) die "build directory must be outside the repository: $build" ;;
esac

[ -n "$adir" ] || adir=$("$root/scripts/release/fetch-sqlite.sh")
[ -f "$adir/sqlite3.c" ] && [ -f "$adir/sqlite3.h" ] || die "no sqlite3.c and sqlite3.h in $adir"

cmake_build() { # DIR [extra cmake args]
	dir=$1
	shift
	cmake -S "$script_dir/server" -B "$dir" -G Ninja -DHQQ_SQLITE_AMALGAMATION_DIR="$adir" -DSQLITE3GEN_TEST_HOOKS="$hooks" "$@" >&2
	cmake --build "$dir" >&2
}

cmake_build "$build"
server=$build/sqlite3_server
if [ -n "$san" ]; then
	cmake_build "$build-$san" -DHQQ_SANITIZE="$san"
	server=$build-$san/sqlite3_server
fi

quote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
printf 'export HQQ_SQLITE3GEN_SERVER=%s\n' "$(quote "$server")"
