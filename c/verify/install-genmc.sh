#!/usr/bin/env bash
# Build GenMC from a pinned commit against a pinned LLVM major version.
#
# Usage: install-genmc.sh [PREFIX]            (default PREFIX: $HOME/.local/genmc)
#
# Prerequisites (Debian/Ubuntu 24.04, apt):
#   cmake make llvm-${LLVM_MAJOR}-dev clang-${LLVM_MAJOR} libclang-${LLVM_MAJOR}-dev libffi-dev zlib1g-dev
#   libedit-dev git ca-certificates bc
#
# The result is reproducible: the commit is pinned (GenMC v0.19.0) and the
# build is verified against the checked-out HEAD, not a floating branch.
set -euo pipefail

GENMC_REPO="${GENMC_REPO:-https://github.com/MPI-SWS/genmc.git}"
GENMC_TAG="${GENMC_TAG:-v0.19.0}"
GENMC_COMMIT="${GENMC_COMMIT:-9f6c4c0772d0c42b325681581acc6a0f3eb9b5f7}"
LLVM_MAJOR="${LLVM_MAJOR:-19}"
JOBS="${JOBS:-2}"
PREFIX="${1:-$HOME/.local/genmc}"
SRC="${GENMC_SRC:-${TMPDIR:-/tmp}/genmc-src}"

if [ ! -d "$SRC/.git" ]; then
  git clone --quiet "$GENMC_REPO" "$SRC"
fi
git -C "$SRC" fetch --quiet --tags origin
git -C "$SRC" checkout --quiet "$GENMC_COMMIT"
head="$(git -C "$SRC" rev-parse HEAD)"
if [ "$head" != "$GENMC_COMMIT" ]; then
  echo "genmc: HEAD $head != pinned $GENMC_COMMIT" >&2
  exit 1
fi

LLVM_DIR="/usr/lib/llvm-${LLVM_MAJOR}"
# clang is the default host compiler: it needs far less memory than g++ for
# GenMCDriver.cpp (about 1 GB instead of > 2 GB), which matters on small
# Docker VMs. Any C++20 compiler works: CC=gcc-14 CXX=g++-14 install-genmc.sh
CXX_BIN="${CXX:-clang++-${LLVM_MAJOR}}"
CC_BIN="${CC:-clang-${LLVM_MAJOR}}"
# A build tree configured with another compiler cannot be reused.
if [ -f "$SRC/build/CMakeCache.txt" ] &&
   ! grep -q "^CMAKE_CXX_COMPILER:.*/${CXX_BIN##*/}\$" "$SRC/build/CMakeCache.txt"; then
  rm -rf "$SRC/build"
fi
cmake -S "$SRC" -B "$SRC/build" \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_C_COMPILER="$CC_BIN" \
  -DCMAKE_CXX_COMPILER="$CXX_BIN" \
  -DCMAKE_PREFIX_PATH="$LLVM_DIR/cmake" \
  -DCMAKE_INSTALL_PREFIX="$PREFIX"
cmake --build "$SRC/build" --parallel "$JOBS"
cmake --install "$SRC/build"
echo "genmc: installed $GENMC_TAG ($GENMC_COMMIT) into $PREFIX"
