#!/usr/bin/env bash
# Run the GenMC proof-of-concept and check the expected verdict per variant.
#   c/verify/genmc/run.sh [path/to/genmc]
# Exit status 0 iff every variant gave its expected verdict.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
genmc="${1:-${GENMC:-genmc}}"
src="$here/mp_wake.c"

# variant : expected (pass|fail) : ';'-separated substrings that must all appear
# in GenMC's output for a failure. The lost wake-up counterexample contains both
# the producer's no_wake and the consumer's would_park write; the publication
# race is reported as a non-atomic race.
cases=(
  "VARIANT_SEQCST:pass:"
  "VARIANT_FENCE:pass:"
  "VARIANT_ACQREL:fail:Safety violation;(no_wake, 1);(would_park, 1)"
  "VARIANT_RELAXED_PUB:fail:race"
)

rc=0
for entry in "${cases[@]}"; do
  IFS=: read -r variant want needle <<<"$entry"
  out="$("$genmc" -rc11 -disable-mm-detector --disable-estimation -- "-D$variant" "$src" 2>&1)"
  status=$?
  if [ "$status" -eq 0 ]; then got=pass; else got=fail; fi
  printf '== %-22s expected=%-4s got=%-4s\n' "$variant" "$want" "$got"
  printf '%s\n' "$out" | sed 's/^/   | /' | grep -E 'Number of|Error|race|\((no_wake|would_park), 1\)' || true
  if [ "$got" != "$want" ]; then
    echo "   MISMATCH: expected $want" >&2
    rc=1
  elif [ "$want" = fail ]; then
    IFS=';' read -ra needles <<<"$needle"
    for n in "${needles[@]}"; do
      if ! printf '%s' "$out" | grep -qF -- "$n"; then
        echo "   MISMATCH: failure output lacks '$n'" >&2
        rc=1
      fi
    done
  fi
done
exit $rc
