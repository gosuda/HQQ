#!/bin/sh
# Fetch, verify and unpack the pinned SQLite amalgamation (third_party/sqlite/).
#
# Usage: scripts/release/fetch-sqlite.sh [-d DIR] [-a ARCHIVE]
#   -d DIR      unpack directory, outside the repository
#               (default: ${TMPDIR:-/tmp}/hqq-sqlite-<version>)
#   -a ARCHIVE  verify and unpack this local copy instead of downloading
#
# On success the amalgamation directory (sqlite3.c, sqlite3.h) is the only
# thing printed on stdout; pass it as -DHQQ_SQLITE_AMALGAMATION_DIR=... .
# The archive is checked against the pinned SHA256 before anything is
# unpacked. SHA3-256 is checked too when openssl can compute it.
# PowerShell variant for Windows runners: fetch-sqlite.ps1.
set -eu

script_dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$script_dir/../.." && pwd)
pin_dir=$root/third_party/sqlite

die() {
	printf 'fetch-sqlite: %s\n' "$*" >&2
	exit 1
}

note() {
	printf 'fetch-sqlite: %s\n' "$*" >&2
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

# Pins: VERSION is key=value; the hash files are sha*sum style lines.
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
case $url in
https://*) ;;
*) die "url must be https: $url" ;;
esac

pinned_hash() { # FILE NAME: print the hex digest pinned for NAME
	h=$(awk -v n="$2" '$2 == n { print $1 }' "$1")
	case $h in
	'' | *[!0-9a-f]*) die "no valid entry for $2 in $1" ;;
	esac
	[ "${#h}" -eq 64 ] || die "digest for $2 in $1 is not 64 hex digits"
	printf '%s\n' "$h"
}
pin_sha256=$(pinned_hash "$pin_dir/SHA256" "$archive")
pin_sha3=$(pinned_hash "$pin_dir/SHA3-256" "$archive")

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	else
		openssl dgst -sha256 -r "$1" | awk '{ print $1 }'
	fi
}

sha3_available() {
	printf '' | openssl dgst -sha3-256 >/dev/null 2>&1
}

sha3_256_of() {
	openssl dgst -sha3-256 -r "$1" | awk '{ print $1 }'
}

# check_archive FILE: succeeds only when every pinned digest the host can
# compute matches. An empty computed digest never matches.
check_archive() {
	[ "$(sha256_of "$1")" = "$pin_sha256" ] || return 1
	if sha3_available; then
		[ "$(sha3_256_of "$1")" = "$pin_sha3" ] || return 1
	fi
	return 0
}

command -v unzip >/dev/null 2>&1 || die "unzip is required"

[ -n "$dest" ] || dest=${TMPDIR:-/tmp}/hqq-sqlite-$version
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
	zip=$archive_arg
	check_archive "$zip" || die "checksum mismatch: $zip (pinned SHA256 $pin_sha256)"
else
	zip=$dest/$archive
	if [ -f "$zip" ] && check_archive "$zip"; then
		note "reusing verified $zip"
	else
		note "downloading $url"
		command -v curl >/dev/null 2>&1 || die "curl is required to download"
		rm -f "$zip.part"
		curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --retry-delay 2 -o "$zip.part" "$url"
		if ! check_archive "$zip.part"; then
			rm -f "$zip.part"
			die "checksum mismatch for the download from $url; nothing was unpacked"
		fi
		mv -f "$zip.part" "$zip"
	fi
fi

if sha3_available; then
	note "checksum ok: SHA256 and SHA3-256 match the pins"
else
	note "checksum ok: SHA256 matches the pin; SHA3-256 not checked (openssl has no sha3-256)"
fi

# Refuse names that would escape the unpack directory.
if unzip -Z1 "$zip" | grep -E -q '^/|(^|/)\.\.(/|$)'; then
	die "archive has absolute or parent-relative member names: $zip"
fi

base=${archive%.zip}
unpacked=$dest/$base
stage=$dest/.stage-$$
rm -rf "$stage"
mkdir "$stage"
trap 'rm -rf "$stage"' EXIT
unzip -q "$zip" -d "$stage"
[ -d "$stage/$base" ] || die "archive does not contain $base/"
rm -rf "$unpacked"
mv "$stage/$base" "$unpacked"
for f in sqlite3.c sqlite3.h; do
	[ -f "$unpacked/$f" ] || die "missing $f in $unpacked"
done

note "SQLite $version unpacked in $unpacked"
printf '%s\n' "$unpacked"
