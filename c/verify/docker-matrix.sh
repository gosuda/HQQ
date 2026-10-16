#!/usr/bin/env bash
# Local reproduction of the Linux jobs of .github/workflows/c.yml inside an
# Ubuntu 24.04 container (native architecture of the Docker host, or force one
# with PLATFORM=linux/amd64 / linux/arm64).
#
#   c/verify/docker-matrix.sh [compiler ...]     default: gcc-13 gcc-14 clang-18 clang-20
#   SANITIZE="address+undefined thread" c/verify/docker-matrix.sh clang-18
#
# Builds are out-of-tree inside the container; nothing is written to the repo.
set -eu
root="$(cd "$(dirname "$0")/../.." && pwd)"
compilers=("$@")
[ ${#compilers[@]} -gt 0 ] || compilers=(gcc-13 gcc-14 clang-18 clang-20)
platform="${PLATFORM:-}"
sanitize="${SANITIZE:-none}"

docker run --rm ${platform:+--platform "$platform"} \
  -e "COMPILERS=${compilers[*]}" -e "SANITIZE=$sanitize" -e "JOBS=${JOBS:-2}" \
  -v "$root":/src:ro ubuntu:24.04 bash -euc '
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
pkgs=(cmake ninja-build)
for cc in $COMPILERS; do
  case "$cc" in
    gcc-*)   pkgs+=("$cc" "g++-${cc#gcc-}") ;;
    clang-*) pkgs+=("$cc" "libclang-rt-${cc#clang-}-dev") ;;
  esac
done
apt-get install -y -qq --no-install-recommends "${pkgs[@]}" >/dev/null
echo "arch: $(uname -m)"
for cc in $COMPILERS; do
  case "$cc" in
    gcc-*)   cxx="g++-${cc#gcc-}" ;;
    clang-*) cxx="clang++-${cc#clang-}" ;;
  esac
  for san in $SANITIZE; do
    echo "=== $cc sanitize=$san"
    b="/tmp/build-$cc-$san"
    CC="$cc" CXX="$cxx" cmake -S /src/c -B "$b" -G Ninja -DCMAKE_BUILD_TYPE=Release \
      -DHQQ_WERROR=ON -DHQQ_SANITIZE="$san" >/dev/null
    cmake --build "$b" --parallel "$JOBS"
    ctest --test-dir "$b" --output-on-failure --timeout 300 | tail -8
  done
done
'
