# Litmus test results

## herd7 (herdtools7): **NOT RUN**

`herd7` was not available and could not be installed without heavy side effects on this machine:

* Homebrew has no `herdtools7` formula; Debian/Ubuntu (`debian:trixie`) has no `herdtools7` package (`apt-cache policy herdtools7` is empty).
* `opam install herdtools7` was attempted inside a throwaway `debian:trixie` container (one CPU, opam with `ocaml-system`, `dune -j 1`). The dependencies (dune, menhir, zarith) built, but the `herdtools7` build itself was killed by the kernel OOM killer (exit 137) three times: the Docker VM has 3.8 GiB in total and is shared with a dozen unrelated running containers of the machine's owner, which must not be stopped. The attempt was abandoned and the container, image and temporary files were removed.

So **the outputs below are expectations derived by hand, not observations.** Run them on a machine with herdtools7 (7.56+):

```sh
opam install herdtools7
for f in spec/litmus/*.litmus; do echo "== $f"; herd7 -model c11_lahav.cat "$f" || herd7 "$f"; done
```

(Use the C11/RC11 model shipped with herdtools7: `c11_lahav.cat`, or `c11_partialSC.cat`, or `rc11.cat` depending on the version; all agree on these shapes.) The files use the herd7 C syntax (`C name`, `{ }`, `P0 (atomic_int* x, ...) { ... }`, `exists (...)`); **their syntax has not been machine-checked** because herd7 was unavailable.

### Expected results (analysis, not observed)

| File | `exists` clause | Expected under C11 | Why |
| --- | --- | --- | --- |
| `mp_publish_acqrel.litmus` | `1:r0=1 /\ 1:r1=0` (consumer saw seq published, read stale payload) | **Never**, no data-race flag | release store of `seq` synchronizes-with the acquire load that reads it |
| `mp_publish_relaxed_bug.litmus` | same | **Sometimes** (and/or a data-race flag) | relaxed `seq` gives no happens-before; payload access races |
| `wake_handshake_sc.litmus` | `0:r0=0 /\ 1:r1=0` (waker sees no waiter, waiter sees old seq: lost wake-up) | **Never** | SC total order, SPEC/MEMORY_MODEL 4.2 |
| `wake_handshake_acqrel.litmus` | same | **Sometimes** (allowed) | store buffering is allowed by release/acquire |
| `wake_handshake_fences.litmus` | same | **Never** | `seq_cst` fences between each store and the following load restore SC for the pair |
| `slot_wraparound_handoff.litmus` | `1:r0=1 /\ 1:r2=2` (consumer's payload read observes the next-lap write) | **Never**, no data-race flag | the producer reuses the slot only after acquiring the consumer's release store |
| `slot_wraparound_handoff_relaxed_bug.litmus` | same | **Sometimes** / data-race flag | relaxed release of the slot |

## Executed check: x86-TSO model of the wake handshake

`tso_sb_check.py` is a small exhaustive enumerator of an **x86-TSO operational model** (per-thread FIFO store buffers; `lock xadd`, `xchg` and `mfence` drain the buffer). It is *not* herd7 and *not* the C11 model; it shows that the lost-wake-up outcome is reachable on real x86 hardware when the handshake is compiled from release/acquire, and unreachable for the seq_cst and fenced compilations. Observed output (`python3 spec/litmus/tso_sb_check.py`):

```text
release/acquire compilation (plain mov + lock xadd): outcomes (r0=waiters seen by waker, r1=seq seen by waiter) = [(0, 0), (0, 1), (1, 0), (1, 1)]
    lost wake-up outcome (r0=0, r1=0) reachable: True   [expected True]
seq_cst compilation (xchg store, lock xadd): outcomes (r0=waiters seen by waker, r1=seq seen by waiter) = [(0, 1), (1, 0), (1, 1)]
    lost wake-up outcome (r0=0, r1=0) reachable: False   [expected False]
release/acquire + seq_cst fences (mfence): outcomes (r0=waiters seen by waker, r1=seq seen by waiter) = [(0, 1), (1, 0), (1, 1)]
    lost wake-up outcome (r0=0, r1=0) reachable: False   [expected False]
RESULT: as expected
```

This corroborates `wake_handshake_*.litmus` on x86-64 only; weakly ordered machines (AArch64 with RCpc loads, POWER, RISC-V) are covered by the C11 argument in MEMORY_MODEL 4.2, which herd7 should confirm.
