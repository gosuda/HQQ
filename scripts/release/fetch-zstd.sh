#!/bin/sh
# Fetch, verify and unpack the pinned Zstandard source (third_party/zstd/).
#
# Usage: scripts/release/fetch-zstd.sh [-d DIR] [-a ARCHIVE]
#   -d DIR      unpack directory, outside the repository
#               (default: ${TMPDIR:-/tmp}/hqq-zstd-<version>)
#   -a ARCHIVE  verify and unpack this local copy instead of downloading
#
# On success the unpacked source directory (the one that holds lib/zstd.h) is the
# only thing printed on stdout; pass it as -DHQQ_ZSTD_DIR=... to
# bindgen/examples/zstd/server. The archive is checked against the pinned SHA256
# before anything is unpacked, and the LICENSE of the unpacked tree must equal
# third_party/zstd/LICENSE. The tarball is the official release asset; its digest
# is also published as <archive>.sha256 next to it. The same pin pattern as
# fetch-sqlite.sh, with a .tar.gz archive. There is no PowerShell variant: the
# server that uses this source is POSIX only (spec/toolchain/BINDGEN.md 1.2).
set -eu

script_dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$script_dir/../.." && pwd)
pin_dir=$root/third_party/zstd

die() {
	printf 'fetch-zstd: %s\n' "$*" >&2
	exit 1
}

note() {
	printf 'fetch-zstd: %s\n' "$*" >&2
}

usage() {
	printf 'usage: %s [-d DIR] [-a ARCHIVE]\n' "$0" >&2
	exit 2
}

dest=
archive_arg=
while getopts "d:a:" opt; do
	case $opt in
	d) dest=$OPTARG ;;
	a) archive_arg=$OPTARG ;;
	*) usage ;;
	esac
done
shift $((OPTIND - 1))
[ $# -eq 0 ] || usage

# Pins: VERSION is key=value; SHA256 is a sha256sum style file.
version=
archive=
url=
while IFS='=' read -r key value; do
	case $key in
	version) version=$value ;;
	archive) archive=$value ;;
	url) url=$value ;;
	'' | '#'*) ;;
	*) die "unknown key in $pin_dir/VERSION: $key" ;;
	esac
done <"$pin_dir/VERSION"
[ -n "$version" ] && [ -n "$archive" ] && [ -n "$url" ] || die "incomplete $pin_dir/VERSION"
case $archive in
*/* | '' | .*) die "archive must be a plain file name: $archive" ;;
esac
case $archive in
*.tar.gz) ;;
*) die "archive must be a .tar.gz: $archive" ;;
esac
case $url in
https://*) ;;
*) die "url must be https: $url" ;;
esac

pin_sha256=$(awk -v n="$archive" '$2 == n { print $1 }' "$pin_dir/SHA256")
case $pin_sha256 in
'' | *[!0-9a-f]*) die "no valid entry for $archive in $pin_dir/SHA256" ;;
esac
[ "${#pin_sha256}" -eq 64 ] || die "digest for $archive in $pin_dir/SHA256 is not 64 hex digits"

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	else
		openssl dgst -sha256 -r "$1" | awk '{ print $1 }'
	fi
}

# check_archive FILE: succeeds only when the digest matches the pin. An empty
# computed digest never matches.
check_archive() {
	[ "$(sha256_of "$1")" = "$pin_sha256" ]
}

command -v tar >/dev/null 2>&1 || die "tar is required"

[ -n "$dest" ] || dest=${TMPDIR:-/tmp}/hqq-zstd-$version
case $dest in /*) ;; *) dest=$PWD/$dest ;; esac
case $dest in
"$root" | "$root"/*) die "refusing to unpack inside the repository: $dest" ;;
esac
mkdir -p "$dest"
dest=$(cd "$dest" && pwd)
case $dest in
"$root" | "$root"/*) die "refusing to unpack inside the repository: $dest" ;;
esac

if [ -n "$archive_arg" ]; then
	[ -f "$archive_arg" ] || die "no such archive: $archive_arg"
	tgz=$archive_arg
	check_archive "$tgz" || die "checksum mismatch: $tgz (pinned SHA256 $pin_sha256)"
else
	tgz=$dest/$archive
	if [ -f "$tgz" ] && check_archive "$tgz"; then
		note "reusing verified $tgz"
	else
		note "downloading $url"
		command -v curl >/dev/null 2>&1 || die "curl is required to download"
		rm -f "$tgz.part"
		curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --retry-delay 2 -o "$tgz.part" "$url"
		if ! check_archive "$tgz.part"; then
			rm -f "$tgz.part"
			die "checksum mismatch for the download from $url; nothing was unpacked"
		fi
		mv -f "$tgz.part" "$tgz"
	fi
fi
note "checksum ok: SHA256 matches the pin"

# Refuse names that would escape the unpack directory.
if tar -tzf "$tgz" | grep -E -q '^/|(^|/)\.\.(/|$)'; then
	die "archive has absolute or parent-relative member names: $tgz"
fi

base=${archive%.tar.gz}
unpacked=$dest/$base
stage=$dest/.stage-$$
rm -rf "$stage"
mkdir "$stage"
trap 'rm -rf "$stage"' EXIT
tar -xzf "$tgz" -C "$stage"
[ -d "$stage/$base" ] || die "archive does not contain $base/"
for f in lib/zstd.h lib/zstd_errors.h lib/compress/zstd_compress.c lib/decompress/zstd_decompress.c; do
	[ -f "$stage/$base/$f" ] || die "missing $f in $tgz"
done
cmp -s "$pin_dir/LICENSE" "$stage/$base/LICENSE" || die "LICENSE of the archive differs from $pin_dir/LICENSE"
rm -rf "$unpacked"
mv "$stage/$base" "$unpacked"

note "Zstandard $version unpacked in $unpacked"
printf '%s\n' "$unpacked"
