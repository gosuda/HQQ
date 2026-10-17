#!/usr/bin/env bash
# GenMC checks of the C ring core (c/src/hqq_ring.h, through the harnesses in
# this directory) and of the slot/handshake proof of concept (mp_wake.c).
#
#   c/verify/genmc/run.sh [--genmc PATH] [TARGET...]  model check (needs GenMC: CI)
#   c/verify/genmc/run.sh --check                     host syntax check of every variant
#   c/verify/genmc/run.sh --host [ITERATIONS]         run the ring harnesses natively
#
# TARGET: poc (mp_wake.c), mp (ring_mp.c), wake (ring_wake.c),
#         reserve (ring_reserve.c), claim (ring_claim.c). Default: all.
#
# GenMC mode: a positive variant must verify (exit 0 and at least one complete
# execution). A negative variant must report a verification error (exit 42,
# GenMC's EVERIFY; exit 17 is a GenMC/user error such as an unknown external
# function, exit 124 is this script's timeout) and its output must contain every
# ';'-separated needle. Exit status 0 iff every expectation holds. On a mismatch
# or a timeout the command and the last GENMC_TAIL lines of GenMC's output are
# printed to stderr.
#
# GenMC flags (GenMC v0.19.0, source at commit 9f6c4c0; lli/main.cpp):
#   -rc11                  RC11 model (the default, stated explicitly)
#   -disable-mm-detector   no silent downgrade to RA (lli/main.cpp:224)
#   -disable-estimation    no state-space estimation pass (lli/main.cpp:77)
#   -disable-ipr           no in-place revisiting (lli/main.cpp:158). With IPR on,
#                          GenMC turns "unordered writes" on annotated reads into
#                          an error (genmc GenMCDriver.cpp checkIPRValidity): the
#                          reserve negative then fails for the wrong reason, and
#                          the weakened mp negative could report that error
#                          instead of the non-atomic race.
#   -unroll=128            every loop runs at most 128 times (doc/manual/cli.md).
#                          Not a model choice: the harness loops are constant
#                          (at most 64 iterations) or claim retries that terminate
#                          under the strong CAS (hqq_model.h). A bound-cut thread
#                          is a ThreadKill terminator, which counts as a complete
#                          execution, so this must never bind; it only guards
#                          against an unbounded loop becoming a silent timeout.
#
# GenMC is not available outside CI in this repository: see
# spec/toolchain/VERIFICATION_TOOLING.md.
#
# Environment: GENMC (binary, default genmc), CC (host compiler, default cc),
#   GENMC_TIMEOUT (seconds per GenMC run, default 300; enforced when timeout(1) exists),
#   GENMC_TAIL (output lines printed on a mismatch, default 200).
# Weakened variants need clang (they rebind the __c11_atomic builtins).

here="$(cd "$(dirname "$0")" && pwd)"
mode=genmc
genmc="${GENMC:-genmc}"
cc="${CC:-cc}"
iters=2000
genmc_timeout="${GENMC_TIMEOUT:-300}"
tail_lines="${GENMC_TAIL:-200}"
targets=""

while [ $# -gt 0 ]; do
  case "$1" in
    --genmc) genmc="$2"; shift 2 ;;
    --check) mode=check; shift ;;
    --host)
      mode=host
      shift
      case "${1:-}" in ''|*[!0-9]*) ;; *) iters="$1"; shift ;; esac
      ;;
    -h|--help) sed -n '2,/^$/p' "$0"; exit 0 ;;
    -*) echo "run.sh: unknown option $1" >&2; exit 2 ;;
    *) targets="$targets $1"; shift ;;
  esac
done

for t in $targets; do
  case "$t" in
    poc|mp|wake|reserve|claim) ;;
    *) echo "run.sh: unknown target '$t' (poc mp wake reserve claim)" >&2; exit 2 ;;
  esac
done

selected() { # target
  [ -z "$targets" ] && return 0
  case " $targets " in *" $1 "*) return 0 ;; esac
  return 1
}

genmc_flags=(-rc11 -disable-mm-detector -disable-estimation -disable-ipr -unroll=128)

# target|source|define (or empty)|expected pass|fail|needles (fail only)
cases=(
  "poc|mp_wake.c|-DVARIANT_SEQCST|pass|"
  "poc|mp_wake.c|-DVARIANT_FENCE|pass|"
  "poc|mp_wake.c|-DVARIANT_ACQREL|fail|Safety violation;(no_wake, 1);(would_park, 1)"
  "poc|mp_wake.c|-DVARIANT_RELAXED_PUB|fail|race"
  "mp|ring_mp.c||pass|"
  "mp|ring_mp.c|-DHQQ_VERIFY_WEAKEN_RELAXED|fail|race"
  "wake|ring_wake.c||pass|"
  "wake|ring_wake.c|-DHQQ_VERIFY_WEAKEN_ACQREL|fail|Safety violation"
  "reserve|ring_reserve.c||pass|"
  "reserve|ring_reserve.c|-DHQQ_VERIFY_UNGUARDED|fail|Safety violation"
  "claim|ring_claim.c||pass|"
)

rc=0

show_failure() { # command output: the diagnostics the next CI round needs
  echo "   command: $1" >&2
  printf '%s\n' "$2" | tail -n "$tail_lines" | sed 's/^/   > /' >&2
}

run_genmc() {
  local entry target src defs want needles out status got n rest missing tmo=() cmd
  if command -v timeout >/dev/null 2>&1; then
    tmo=(timeout "$genmc_timeout")
  fi
  for entry in "${cases[@]}"; do
    IFS='|' read -r target src defs want needles <<<"$entry"
    selected "$target" || continue
    cmd="$genmc ${genmc_flags[*]} -- ${defs:+$defs }$src"
    out="$("${tmo[@]}" "$genmc" "${genmc_flags[@]}" -- ${defs:+"$defs"} "$here/$src" 2>&1)"
    status=$?
    if [ "$status" -eq 0 ]; then got=pass; else got=fail; fi
    printf '== %-8s %-30s expected=%-4s got=%-4s exit=%s\n' "$target" "${defs:-(none)}" "$want" "$got" "$status"
    printf '%s\n' "$out" | sed 's/^/   | /' | grep -E 'Number of|Error|race|violation|\((no_wake|would_park), 1\)' || true
    if [ "$status" -eq 124 ]; then
      echo "   TIMEOUT: GenMC did not finish within ${genmc_timeout}s" >&2
      show_failure "$cmd" "$out"
      rc=1
      continue
    fi
    if [ "$want" = pass ]; then
      if [ "$status" -ne 0 ]; then
        echo "   MISMATCH: expected a verified program (exit 0)" >&2
        show_failure "$cmd" "$out"
        rc=1
      elif ! printf '%s' "$out" | grep -Eiq 'complete executions[^0-9]*[1-9]'; then
        echo "   MISMATCH: no complete execution was explored (vacuous pass)" >&2
        show_failure "$cmd" "$out"
        rc=1
      fi
    else
      if [ "$status" -ne 42 ]; then
        echo "   MISMATCH: expected a verification error (exit 42), got exit $status" >&2
        show_failure "$cmd" "$out"
        rc=1
      else
        rest="$needles"
        missing=""
        while [ -n "$rest" ]; do
          n="${rest%%;*}"
          case "$rest" in *';'*) rest="${rest#*;}" ;; *) rest="" ;; esac
          printf '%s' "$out" | grep -qF -- "$n" || missing="$missing '$n'"
        done
        if [ -n "$missing" ]; then
          echo "   MISMATCH: failure output lacks$missing" >&2
          show_failure "$cmd" "$out"
          rc=1
        fi
      fi
    fi
  done
}

run_check() {
  local entry target src defs
  for entry in "${cases[@]}"; do
    IFS='|' read -r target src defs _ _ <<<"$entry"
    selected "$target" || continue
    printf '== syntax %-8s %s\n' "$target" "${defs:-(none)}"
    # -I syntax: stand-in for GenMC's genmc.h, present only so that the GenMC
    # build can be syntax-checked on hosts without GenMC (see syntax/genmc.h).
    "$cc" -std=c11 -Wall -Wextra -Wpedantic -Werror -I "$here/syntax" -fsyntax-only \
      ${defs:+"$defs"} "$here/$src" || rc=1
    if [ "$target" != poc ]; then
      printf '== syntax %-8s %s -DHQQ_VERIFY_HOST\n' "$target" "${defs:-(none)}"
      "$cc" -std=c11 -Wall -Wextra -Wpedantic -Werror -fsyntax-only -DHQQ_VERIFY_HOST \
        ${defs:+"$defs"} "$here/$src" || rc=1
    fi
  done
}

run_host() {
  local h target src tmp bin out status
  tmp="$(mktemp -d)" || exit 1
  trap 'rm -rf "$tmp"' EXIT
  for h in mp:ring_mp.c wake:ring_wake.c reserve:ring_reserve.c claim:ring_claim.c; do
    target="${h%%:*}"
    src="${h#*:}"
    selected "$target" || continue
    bin="$tmp/$target"
    if ! "$cc" -std=c11 -O1 -Wall -Wextra -Wpedantic -Werror -DHQQ_VERIFY_HOST -pthread \
         "$here/$src" -o "$bin"; then
      echo "== host $target: BUILD FAILED" >&2
      rc=1
      continue
    fi
    out="$("$bin" "$iters" 2>&1)"
    status=$?
    printf '== host %-8s iterations=%s exit=%s %s\n' "$target" "$iters" "$status" "$out"
    [ "$status" -eq 0 ] || rc=1
  done
}

case "$mode" in
  genmc) run_genmc ;;
  check) run_check ;;
  host) run_host ;;
esac

if [ "$rc" -eq 0 ]; then
  echo "run.sh ($mode): all expectations hold"
else
  echo "run.sh ($mode): FAILED" >&2
fi
exit "$rc"
