#!/bin/sh
# Fetch the pinned Zstandard source, build the zstd example server and the zstd library, and print the
# environment that the Go tests and benchmarks of this directory read.
#
# Usage: bindgen/examples/zstd/build.sh [-b BUILD_DIR] [-d ZSTD_DIR] [-s SANITIZER]
#   -b BUILD_DIR  build directory root, outside the repository
#                 (default: ${TMPDIR:-/tmp}/hqq-zstd-build)
#   -d ZSTD_DIR   use this unpacked Zstandard source instead of running fetch-zstd.sh
#   -s SANITIZER  also build the server with this HQQ_SANITIZE value (for example address+undefined)
#                 and point HQQ_ZSTD_SERVER at it; the cgo library stays unsanitized
#
# stdout is a script for `eval`: exports of HQQ_ZSTD_SERVER (the server binary), CGO_CFLAGS and
# CGO_LDFLAGS (the zstd library built from the same source, for the cgozstd package). Progress goes
# to stderr.
#
#   eval "$(bindgen/examples/zstd/build.sh)"
#   go test -race -tags zstdcgo ./bindgen/examples/zstd/...
set -eu

script_dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$script_dir/../../.." && pwd)

die() {
	printf 'build.sh: %s\n' "$*" >&2
	exit 1
}

build=${TMPDIR:-/tmp}/hqq-zstd-build
zdir=
san=
while getopts "b:d:s:" opt; do
	case $opt in
	b) build=$OPTARG ;;
	d) zdir=$OPTARG ;;
	s) san=$OPTARG ;;
	*) die "usage: $0 [-b BUILD_DIR] [-d ZSTD_DIR] [-s SANITIZER]" ;;
	esac
done
shift $((OPTIND - 1))
[ $# -eq 0 ] || die "usage: $0 [-b BUILD_DIR] [-d ZSTD_DIR] [-s SANITIZER]"

command -v cmake >/dev/null 2>&1 || die "cmake is required"
command -v ninja >/dev/null 2>&1 || die "ninja is required"

case $build in /*) ;; *) build=$PWD/$build ;; esac
case $build in
"$root" | "$root"/*) die "build directory must be outside the repository: $build" ;;
esac

[ -n "$zdir" ] || zdir=$("$root/scripts/release/fetch-zstd.sh")
[ -f "$zdir/lib/zstd.h" ] || die "no lib/zstd.h in $zdir"

cmake_build() { # DIR [extra cmake args]
	dir=$1
	shift
	cmake -S "$script_dir/server" -B "$dir" -G Ninja -DHQQ_ZSTD_DIR="$zdir" "$@" >&2
	cmake --build "$dir" >&2
}

cmake_build "$build"
server=$build/zstd_server
if [ -n "$san" ]; then
	cmake_build "$build-$san" -DHQQ_SANITIZE="$san"
	server=$build-$san/zstd_server
fi

quote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
printf 'export HQQ_ZSTD_SERVER=%s\n' "$(quote "$server")"
printf 'export CGO_CFLAGS=%s\n' "$(quote "-I$zdir/lib")"
# cgo passes CGO_LDFLAGS twice when it links a package and its test binary; ld on macOS warns about it.
ldflags=$build/libhqqzstd.a
[ "$(uname -s)" != Darwin ] || ldflags="$ldflags -Wl,-no_warn_duplicate_libraries"
printf 'export CGO_LDFLAGS=%s\n' "$(quote "$ldflags")"
