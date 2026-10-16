#!/usr/bin/env bash
# Install herdtools7 (herd7, diy7, litmus7, ...) from the pinned opam release
# into an opam switch named "herd", using the system OCaml compiler.
#
# Usage: install-herd.sh
# Afterwards:  eval "$(opam env --switch=herd)" && herd7 -version
#
# Prerequisites (Debian/Ubuntu 24.04, apt):
#   opam ocaml-nox build-essential libgmp-dev pkg-config m4 unzip curl git
set -euo pipefail

HERDTOOLS_VERSION="${HERDTOOLS_VERSION:-7.58}"
SWITCH="${HERD_SWITCH:-herd}"

if ! opam switch list --short 2>/dev/null | grep -qx "$SWITCH"; then
  [ -f "$HOME/.opam/config" ] || opam init --bare --disable-sandboxing --no-setup --yes
  opam switch create "$SWITCH" ocaml-system --yes
fi
eval "$(opam env --switch="$SWITCH")"
# HERD_JOBS bounds the build parallelism (the herdtools7 Makefile runs
# `dune build -j $(nproc)` and ocamlopt on herd is memory hungry on small VMs).
# `nproc` honours OMP_NUM_THREADS. Unset = all CPUs.
if [ -n "${HERD_JOBS:-}" ]; then
  export OMP_NUM_THREADS="$HERD_JOBS"
fi
opam install --yes "herdtools7.${HERDTOOLS_VERSION}"
herd7 -version
