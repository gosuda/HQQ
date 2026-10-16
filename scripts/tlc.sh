#!/bin/sh
# Model-check internal/mpmc/MPMC.tla with TLC.
#
# Usage: scripts/tlc.sh [extra TLC args...]
#
# Environment:
#   TLA2TOOLS_JAR  path to tla2tools.jar (downloaded there if missing;
#                  default: $XDG_CACHE_HOME/tlaplus/tla2tools-<version>.jar)
#   TLA2TOOLS_VERSION  release tag to download (default: v1.8.0)
#   TLA2TOOLS_URL  full download URL (overrides TLA2TOOLS_VERSION)
#   JAVA           java executable (default: $JAVA_HOME/bin/java, then java)
#   TLC_WORKERS    TLC worker count (default: auto)
#   TLC_CONFIG     config file, relative to internal/mpmc (default: MPMC.cfg)
#
# Exits non-zero if TLC reports any invariant/property violation, deadlock,
# or error.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
SPEC_DIR="$ROOT/internal/mpmc"
SPEC=MPMC.tla
CONFIG=${TLC_CONFIG:-MPMC.cfg}
WORKERS=${TLC_WORKERS:-auto}

VERSION=${TLA2TOOLS_VERSION:-v1.8.0}
URL=${TLA2TOOLS_URL:-https://github.com/tlaplus/tlaplus/releases/download/$VERSION/tla2tools.jar}
CACHE_DIR=${XDG_CACHE_HOME:-$HOME/.cache}/tlaplus
JAR=${TLA2TOOLS_JAR:-$CACHE_DIR/tla2tools-$VERSION.jar}

if [ -n "${JAVA:-}" ]; then
    :
elif [ -n "${JAVA_HOME:-}" ] && [ -x "$JAVA_HOME/bin/java" ]; then
    JAVA=$JAVA_HOME/bin/java
else
    JAVA=java
fi

if ! "$JAVA" -version >/dev/null 2>&1; then
    echo "tlc.sh: no working Java runtime ($JAVA); set JAVA or JAVA_HOME" >&2
    exit 2
fi

if [ ! -f "$JAR" ]; then
    mkdir -p "$(dirname -- "$JAR")"
    echo "tlc.sh: downloading $URL -> $JAR" >&2
    tmp="$JAR.tmp.$$"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 -o "$tmp" "$URL"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$tmp" "$URL"
    else
        echo "tlc.sh: need curl or wget to download tla2tools.jar" >&2
        exit 2
    fi
    mv "$tmp" "$JAR"
fi

# Keep TLC's state files out of the source tree.
METADIR=$(mktemp -d 2>/dev/null || mktemp -d -t tlc)
LOG="$METADIR/tlc.log"
cleanup() {
    rm -rf "$METADIR" "$SPEC_DIR/states"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

cd "$SPEC_DIR"
status=0
"$JAVA" -XX:+UseParallelGC -cp "$JAR" tlc2.TLC \
    -workers "$WORKERS" \
    -metadir "$METADIR/states" \
    -cleanup \
    -config "$CONFIG" \
    "$@" \
    "$SPEC" >"$LOG" 2>&1 || status=$?
cat "$LOG"

if [ "$status" -ne 0 ]; then
    echo "tlc.sh: TLC failed (exit $status)" >&2
    exit "$status"
fi
# Belt and braces: TLC should already exit non-zero on violations.
if ! grep -q "No error has been found" "$LOG"; then
    echo "tlc.sh: TLC did not report success" >&2
    exit 1
fi
echo "tlc.sh: OK" >&2
