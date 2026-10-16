#!/usr/bin/env bash
# Run the litmus tests under herd7 and compare each verdict with the expected
# one.
#   - c/verify/litmus/*.litmus          (verdicts in expected.txt)
#   - spec/litmus/*.litmus, if present  (verdicts in spec-expected.txt)
#
#   c/verify/litmus/run-herd.sh [path/to/herd7]
#
# Environment: HERD7 (binary), HERD_MODEL (cat model, default rc11.cat).
# Expected values:
#   Never      the `exists` condition is unreachable and herd7 raised no flag
#              (a plain-memory race shows up as `Flag *undef*`)
#   Sometimes  the `exists` condition is reachable (and no flag)
#   Bug        a negative test: the condition is reachable OR herd7 flagged an
#              undefined behaviour (data race)
# Exit status 0 iff every expectation holds.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
herd="${1:-${HERD7:-herd7}}"
model="${HERD_MODEL:-rc11.cat}"

rc=0
run_set() { # dir expected-file
  local dir="$1" list="$2" name want _ out got states flags
  while read -r name want _; do
    case "$name" in ''|'#'*) continue ;; esac
    out="$("$herd" -model "$model" "$dir/$name.litmus" 2>&1)"
    got="$(printf '%s\n' "$out" | sed -n 's/^Observation [^ ]* \([A-Za-z]*\) .*/\1/p')"
    states="$(printf '%s\n' "$out" | sed -n 's/^States \([0-9]*\)/\1/p')"
    flags="$(printf '%s\n' "$out" | sed -n 's/^Flag \(.*\)/\1/p' | tr '\n' ',')"
    printf '%-38s states=%-2s expected=%-9s got=%-9s flags=%s\n' \
      "$name" "$states" "$want" "${got:-ERROR}" "${flags:--}"
    ok=0
    case "$want" in
      Never|Sometimes) [ "$got" = "$want" ] && [ -z "$flags" ] && ok=1 ;;
      Bug) { [ "$got" = Sometimes ] || [ -n "$flags" ]; } && ok=1 ;;
    esac
    if [ "$ok" -ne 1 ]; then
      printf '%s\n' "$out" | sed 's/^/   | /' >&2
      rc=1
    fi
  done < "$list"
}

echo "herd7 model: $model"
run_set "$here" "$here/expected.txt"
if [ -d "$here/../../../spec/litmus" ]; then
  run_set "$(cd "$here/../../../spec/litmus" && pwd)" "$here/spec-expected.txt"
fi
exit $rc
