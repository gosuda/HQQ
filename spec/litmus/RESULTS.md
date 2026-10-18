# Litmus Test Results

This document records the verification expectations and operational model results for the C11 weak-memory litmus tests in [`spec/litmus/`](.) covering HQQ ring slot publication, slot reuse wraparound, and the Dekker wait/wake handshake ([`MEMORY_MODEL.md`](../MEMORY_MODEL.md) sections 3 and 4).

## `herd7` (`herdtools7`) Verification

To execute the `.litmus` models directly with `herd7` (`herdtools7` 7.56 or newer) under the RC11 / C11 memory model (`c11_lahav.cat`, `c11_partialSC.cat`, or `rc11.cat`), run:

```sh
opam install herdtools7
for f in spec/litmus/*.litmus; do
    echo "== $f"
    herd7 -model c11_lahav.cat "$f" || herd7 "$f"
done
```

Note: The `.github/workflows/c.yml` (`memory-model`) workflow runs `herd7` in CI whenever `herd7` is installed on the runner; see [`spec/toolchain/VERIFICATION_TOOLING.md`](../toolchain/VERIFICATION_TOOLING.md) for the automated GenMC and hardware stress test suites that run on every commit.

### Expected Outcomes Under the C11 Memory Model

Each positive `.litmus` model encodes the memory orders required by `MEMORY_MODEL.md`, while each negative (`*_bug` or `*_acqrel`) model weakens a single ordering edge to demonstrate that the weakened order allows a forbidden execution:

| File | `exists` Clause | Expected Outcome Under C11 | Rationale |
| --- | --- | --- | --- |
| `mp_publish_acqrel.litmus` | `1:r0=1 /\ 1:r1=0` (consumer observed published `seq`, read stale payload) | **Never**, no data-race flag | The producer's release store to `seq` synchronizes-with the consumer's acquire load. |
| `mp_publish_relaxed_bug.litmus` | Same (`1:r0=1 /\ 1:r1=0`) | **Sometimes** (and/or data-race flag) | A relaxed store to `seq` establishes no happens-before edge, turning the payload access into a data race. |
| `wake_handshake_sc.litmus` | `0:r0=0 /\ 1:r1=0` (waker sees no waiter, waiter sees old `seq`: lost wake-up) | **Never** | Sequentially consistent (`seq_cst`) total order across `W1`, `W3`, `K1`, and `K2` (`MEMORY_MODEL.md` section 4.2). |
| `wake_handshake_acqrel.litmus` | Same (`0:r0=0 /\ 1:r1=0`) | **Sometimes** (allowed) | Release/acquire ordering permits store buffering, allowing both loads to read the initial values. |
| `wake_handshake_fences.litmus` | Same (`0:r0=0 /\ 1:r1=0`) | **Never** | `seq_cst` fences placed between each side's store and subsequent load restore sequential consistency for the pair. |
| `slot_wraparound_handoff.litmus` | `1:r0=1 /\ 1:r2=2` (consumer's payload read observes the next-lap write) | **Never**, no data-race flag | The next-lap producer reuses the slot only after acquiring the consumer's release store on `seq`. |
| `slot_wraparound_handoff_relaxed_bug.litmus` | Same (`1:r0=1 /\ 1:r2=2`) | **Sometimes** / data-race flag | A relaxed release of the slot removes the happens-before edge between the consumer's read and the next lap's write. |

## Executed Check: x86-TSO Model of the Wake Handshake

Script [`tso_sb_check.py`](tso_sb_check.py) is an exhaustive operational state-space enumerator for an **x86-TSO machine model** (per-thread FIFO store buffers where `lock xadd`, `xchg`, and `mfence` drain the store buffer). It demonstrates that the lost-wake-up state (`r0=0, r1=0`) is reachable on real x86-64 hardware when the waker publishes `seq` using a plain `memory_order_release` store (`mov`), and unreachable when compiled with `seq_cst` (`xchg`) or `mfence`.

Observed output from `python3 spec/litmus/tso_sb_check.py`:

```text
release/acquire compilation (plain mov + lock xadd): outcomes (r0=waiters seen by waker, r1=seq seen by waiter) = [(0, 0), (0, 1), (1, 0), (1, 1)]
    lost wake-up outcome (r0=0, r1=0) reachable: True   [expected True]
seq_cst compilation (xchg store, lock xadd): outcomes (r0=waiters seen by waker, r1=seq seen by waiter) = [(0, 1), (1, 0), (1, 1)]
    lost wake-up outcome (r0=0, r1=0) reachable: False   [expected False]
release/acquire + seq_cst fences (mfence): outcomes (r0=waiters seen by waker, r1=seq seen by waiter) = [(0, 1), (1, 0), (1, 1)]
    lost wake-up outcome (r0=0, r1=0) reachable: False   [expected False]
RESULT: as expected
```

This operational check corroborates `wake_handshake_*.litmus` on x86-64; weakly ordered architectures (AArch64 with RCpc loads, POWER, and RISC-V) are covered by the C11 total-order proof in [`MEMORY_MODEL.md`](../MEMORY_MODEL.md) section 4.2 and by the GenMC model-checking suite in [`c/verify/genmc/`](../../c/verify/genmc).
