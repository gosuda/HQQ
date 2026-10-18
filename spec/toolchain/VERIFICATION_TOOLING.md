# Memory-Model Verification Tooling

**Status**: Active verification suite for `libhqq` (`c/verify/`), executed in the `memory-model` job of [`.github/workflows/c.yml`](../../.github/workflows/c.yml).

While Go's `sync/atomic` operations provide sequential consistency by default, a C11, C++, or Rust implementation chooses an explicit memory order (`relaxed`, `acquire`, `release`, `acq_rel`, or `seq_cst`) for each shared-memory access. That choice is critical in the **Dekker wait/wake handshake** (`SPEC.md` section 8 and `MEMORY_MODEL.md` section 4) and in **ring slot publication and wraparound** (`MEMORY_MODEL.md` section 3).

This document describes the formal and empirical verification tools in [`c/verify/`](../../c/verify), how to install and run them reproducibly, and the exact invariants and counterexamples they check.

## 1. Verification Layers and Tooling Summary

No single tool checks both source-level C11 weak-memory axiomatics and compiled machine-code execution on real CPUs. HQQ combines five complementary verification layers:

| Layer | Tool | Memory Model | Defects Detected | Repository Location |
| --- | --- | --- | --- | --- |
| Ring state machine | **TLC** (TLA+) | Sequentially consistent interleavings | Protocol state-machine bugs | `internal/mpmc/MPMC.tla`, `scripts/tlc.sh` |
| Exhaustive C11 source verification | **GenMC** (v0.19.0) | RC11 (plus RA, IMM, TSO, SC) | Assertion failures, lost wake-ups, and non-atomic payload races across **all** C11-permitted executions | `c/verify/genmc/` |
| Axiomatic litmus models | **herd7** (`herdtools7` 7.58) | `rc11.cat` (C11 cat models) | Reachability of forbidden states and undefined data races in `.litmus` models | `c/verify/litmus/`, `spec/litmus/` |
| Native hardware stress | **`hqq_stress_litmus`** | Physical `arm64` and `x86_64` CPUs | Compiler lowering bugs or missing hardware barriers under high-contention thread races | `c/verify/stress/` |
| Runtime sanitizers | **ASan, UBSan, TSan** | Instrumented execution | Out-of-bounds accesses, undefined behavior, memory leaks, and data races | CMake `-DHQQ_SANITIZE=...` |

GenMC and `herd7` verify the **source-level C11 contract** across all executions permitted by the RC11 memory model, while `hqq_stress_litmus` and the ARM64 sanitizer jobs verify the **compiled binary** on real weakly ordered hardware.

## 2. Core Invariants Under Test

Both the standalone proof-of-concept model (`c/verify/genmc/mp_wake.c`) and the production ring models (`c/verify/genmc/ring_*.c`) verify two coupled properties:

```text
Producer                                   Consumer
-----------------------------------------  -----------------------------------------
data = 42            (plain store)         waiters += 1                [W]
seq  = 1             [P]                   s = seq                     [R]
w = waiters          [V]                   if (s == 1) read data (plain load)
if (w == 0) no_wake = 1                    else would_park = 1
```

1. **No lost wake-up**: `would_park && no_wake` is unreachable across all executions.
2. **Race-free payload publication**: Whenever the consumer observes `seq == 1`, it reads `data == 42`, and the plain payload accesses never form a C11 data race.

Because `[P]`, `[V]`, `[W]`, and `[R]` form a store-buffering (Dekker) pair, release/acquire ordering is insufficient: all four accesses must be `memory_order_seq_cst` (or release/acquire separated by `atomic_thread_fence(memory_order_seq_cst)` on both sides), `[P]` must be at least a release store, and `[R]` must be at least an acquire load.

## 3. Stateless Model Checking With GenMC

[GenMC](https://github.com/MPI-SWS/genmc) is a stateless model checker operating on LLVM IR under the RC11 memory model.

Script [`c/verify/install-genmc.sh`](../../c/verify/install-genmc.sh) builds and installs GenMC `v0.19.0` (pinned at commit `9f6c4c0772d0c42b325681581acc6a0f3eb9b5f7`) against Ubuntu 24.04's `llvm-19` and `clang-19` packages. To run the GenMC suite locally inside Docker on either `amd64` or `arm64`:

```sh
docker build -t hqq-genmc -f c/verify/genmc/Dockerfile c/verify
docker run --rm -v "$PWD":/src:ro hqq-genmc /src/c/verify/genmc/run.sh --genmc genmc
```

### 3.1 Proof-of-Concept Model (`mp_wake.c`)

Script [`c/verify/genmc/run.sh`](../../c/verify/genmc/run.sh) runs GenMC with `-rc11 -disable-mm-detector -disable-estimation -disable-ipr` against four variants of `mp_wake.c`:

| Variant | Ordering Configuration | Expected Verdict | Observed GenMC v0.19.0 Result |
| --- | --- | --- | --- |
| `VARIANT_SEQCST` | `[P]`, `[V]`, `[W]`, `[R]` all `memory_order_seq_cst` | **Pass** | `Number of complete executions explored: 3`, zero errors (exit `0`). |
| `VARIANT_FENCE` | Release store, acquire load, and `seq_cst` fence between store and load on both sides | **Pass** | `Number of complete executions explored: 3`, zero errors (exit `0`). |
| `VARIANT_ACQREL` | Release / acquire / acq_rel only (no `seq_cst`) | **Fail** (lost wake-up) | `Error: Safety violation!` with counterexample where producer reads `waiters == 0` and consumer reads `seq == 0` (exit `42`). |
| `VARIANT_RELAXED_PUB` | `seq_cst` wake check, but `[P]` weakened to `memory_order_relaxed` | **Fail** (data race) | `Error: Non-atomic race!` between producer `Wna (data, 42)` and consumer `Rna (data, 42)` (exit `42`). |

### 3.2 Production Ring-Core Verification Programs (`ring_*.c`)

In addition to `mp_wake.c`, four verification programs include the real production ring implementation (`c/src/hqq_ring.h`) directly via `c/verify/genmc/hqq_model.h` and verify both positive (unmodified `c/src/hqq_ring.h`) and negative (intentionally weakened) builds:

| Program | Scenario Modeled | Invariants Verified in Every Execution | Positive Target (Must Pass) | Negative Target (Must Fail with Exit `42`) |
| --- | --- | --- | --- | --- |
| `ring_mp.c` | 1 producer, 1 consumer, 2-slot ring, 4 messages (wrapping across two full laps) | FIFO delivery, exact payload values, and race-free plain payload reads/writes across slot reuse | `run.sh mp` | `-DHQQ_VERIFY_WEAKEN_RELAXED` (triggers non-atomic payload race) |
| `ring_wake.c` | Consumer (`peek`, `hqq_ring_park`, `peek`) vs producer (`claim`, write, `hqq_ring_publish`) on a 2-slot ring | Zero lost wake-ups: whenever the consumer sleeps on futex compare value `v`, the producer increments the wake word after the consumer's snapshot | `run.sh wake` | `-DHQQ_VERIFY_WEAKEN_ACQREL` (triggers lost wake-up assertion failure) |
| `ring_reserve.c` | Single reservation claimed, two concurrent threads race to call `hqq_ring_publish_guarded` on copies of the handle | Exactly one completion succeeds; the stale handle copy is rejected and the slot is published once | `run.sh reserve` | `-DHQQ_VERIFY_UNGUARDED` (triggers double completion failure) |
| `ring_claim.c` | 2 concurrent producer threads race on the producer cursor CAS (`w`), followed by consumer drain | Both claims obtain distinct positions and both payloads are delivered intact | `run.sh claim` | None (cursor CAS remains atomic under `acq_rel`) |

How weakened builds and model hooks work:

- **Selective order weakening (`hqq_model.h`)**: In positive runs, `c/src/hqq_ring.h` compiles with its exact production `<stdatomic.h>` operations. When `-DHQQ_VERIFY_WEAKEN_ACQREL` is defined, `hqq_model.h` rebinds `memory_order_seq_cst` operations in the ring core to their strongest non-SC equivalents (`acquire` for loads, `release` for stores, `acq_rel` for RMWs). When `-DHQQ_VERIFY_WEAKEN_RELAXED` is defined, those operations become `memory_order_relaxed`.
- **Strong CAS and loop bounding under GenMC**: Under GenMC (`__GENMC__`), `c/verify/genmc/hqq_model.h` includes `<genmc.h>` for `__VERIFIER_assume` and maps `atomic_compare_exchange_weak_explicit` to `atomic_compare_exchange_strong_explicit` (following GenMC's `StrengthenCASPass` recommendation so spurious weak-CAS failures do not create unbounded retry trees). `run.sh` invokes GenMC with `-rc11 -disable-mm-detector -disable-estimation -disable-ipr -unroll=128` and verifies that every positive target exits `0` with at least one complete execution and every negative target exits `42` (`EVERIFY`) with the expected diagnostic string.
- **Native pthread mode (`-DHQQ_VERIFY_HOST`)**: The exact same `ring_*.c` programs also compile as native POSIX-thread executables (`run.sh --check` and `run.sh --host 20000`) with a 5-second watchdog timer on condition-variable waits.

## 4. Axiomatic Litmus Verification With `herd7`

`herd7` (`herdtools7` 7.58) evaluates the axiomatic C11 `.litmus` models in `c/verify/litmus/*.litmus` and `spec/litmus/*.litmus` against `rc11.cat`. Script [`c/verify/install-herd.sh`](../../c/verify/install-herd.sh) installs `herdtools7.7.58` into a cached `opam` switch, and [`c/verify/litmus/run-herd.sh`](../../c/verify/litmus/run-herd.sh) checks every model against `expected.txt` and `spec-expected.txt`:

```sh
c/verify/install-herd.sh
eval "$(opam env --switch=herd)"
c/verify/litmus/run-herd.sh "$(command -v herd7)"
```

Observed output from `herd7` (`herdtools7` 7.58, `rc11.cat`):

```text
herd7 model: rc11.cat
SB+sc                                  states=3  expected=Never     got=Never     flags=-
SB+acqrel                              states=4  expected=Sometimes got=Sometimes flags=-
SB+fences                              states=3  expected=Never     got=Never     flags=-
MP+relacq                              states=3  expected=Never     got=Never     flags=-
MP+rlx                                 states=4  expected=Sometimes got=Sometimes flags=-
mp_publish_acqrel                      states=2  expected=Never     got=Never     flags=-
mp_publish_relaxed_bug                 states=3  expected=Bug       got=Sometimes flags=*undef*,
slot_wraparound_handoff                states=2  expected=Never     got=Never     flags=-
slot_wraparound_handoff_relaxed_bug    states=2  expected=Bug       got=Never     flags=*undef*,
wake_handshake_sc                      states=3  expected=Never     got=Never     flags=-
wake_handshake_acqrel                  states=4  expected=Sometimes got=Sometimes flags=-
wake_handshake_fences                  states=3  expected=Never     got=Never     flags=-
```

The `*undef*` flag emitted by `herd7` on `mp_publish_relaxed_bug` and `slot_wraparound_handoff_relaxed_bug` confirms that weakening the sequence store to `relaxed` introduces a C11 data race on the non-atomic payload bytes.

## 5. Hardware Weak-Memory Stress Runner (`hqq_stress_litmus`)

Program [`c/verify/stress/stress_litmus.c`](../../c/verify/stress/stress_litmus.c) runs two synchronized POSIX threads across a tight spin barrier for `N` iterations per test (default `200,000` iterations or `1s` per test, configurable up to `50,000,000` iterations / `60s`), testing both store-buffering (SB) and message-passing (MP) patterns on the host CPU:

- **Strong variants (`SB seq_cst`, `MP rel/acq`, `MP seq_cst`)**: Must observe `weak=0` forbidden outcomes; any non-zero count exits with status `1` and fails `ctest`.
- **Control variants (`SB acq/rel`, `SB relaxed`, `MP relaxed`)**: Intentionally omit required barriers to demonstrate that the test runner regularly observes hardware store-buffer and weak-memory reordering on the host CPU.

Observed run on Apple Silicon (`arm64`, 100,000 iterations per test):

```text
hqq stress litmus: hqq 0.0.0-scaffold, arm64
SB seq_cst   (strong)    iterations=100000   weak=0        PASS
SB acq/rel   (control)   iterations=100000   weak=81784    weak outcomes observed (expected for a control)
SB relaxed   (control)   iterations=100000   weak=70774    weak outcomes observed (expected for a control)
MP rel/acq   (strong)    iterations=100000   weak=0        PASS
MP seq_cst   (strong)    iterations=100000   weak=0        PASS
MP relaxed   (control)   iterations=100000   weak=78       weak outcomes observed (expected for a control)
```

On the `ubuntu-24.04-arm` CI runner, `.github/workflows/c.yml` runs `ctest` plus three additional 3-second passes of `hqq_stress_litmus`.

## 6. Runtime Sanitizers

CMake option `-DHQQ_SANITIZE=address+undefined|thread` (documented in [`C_TOOLCHAIN.md`](C_TOOLCHAIN.md)) instruments `libhqq` and all 28 C tests with Clang AddressSanitizer, UndefinedBehaviorSanitizer, and ThreadSanitizer on both `x86_64` and `arm64`. Note that because Clang/GCC ThreadSanitizer does not model standalone `atomic_thread_fence` instructions, fence-based variants (`VARIANT_FENCE`) are verified via GenMC and `herd7`.

## 7. Running All Verification Suites

Execute the complete verification suite locally with the following commands:

```sh
# 1. GenMC formal model checking (requires installed GenMC or Docker image)
c/verify/genmc/run.sh --genmc "$HOME/.local/genmc/bin/genmc"
c/verify/genmc/run.sh --genmc "$HOME/.local/genmc/bin/genmc" wake
CC=clang c/verify/genmc/run.sh --check
c/verify/genmc/run.sh --host 20000

# 2. herd7 axiomatic litmus verification
c/verify/litmus/run-herd.sh "$(command -v herd7)"

# 3. Hardware weak-memory stress runner and ctest suite
cmake -S c -B build -DHQQ_WERROR=ON
cmake --build build
ctest --test-dir build --output-on-failure
build/verify/stress/hqq_stress_litmus
```
