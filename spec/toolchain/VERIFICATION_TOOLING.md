# Memory-model verification tooling

Status: milestone M3c. The GenMC harnesses for the real ring core are in
section 3.2 (GenMC runs only in CI). The Go implementation gets sequential
consistency for free (all Go atomics are seq_cst). A C (or Rust/Zig/C++)
implementation chooses a memory order per access, and the choice is
load-bearing in exactly one place that is easy to get wrong: the **wake
handshake** (a store-buffering / Dekker pair), see
[`PROTOCOL.md`](../../PROTOCOL.md) "Parking and process-local aggregation".

This document describes the tools that check the C11 memory orders, how to
install and run them reproducibly, and what they were observed to do. Inputs
and scripts live under [`c/verify/`](../../c/verify); CI runs them in the
`memory-model` job of [`.github/workflows/c.yml`](../../.github/workflows/c.yml).

## 1. What is checked, and by which tool

| Layer | Tool | Model | Finds | Where |
|---|---|---|---|---|
| Ring state machine | TLC (existing) | sequentially consistent interleavings | protocol logic bugs | `internal/mpmc/MPMC.tla`, `scripts/tlc.sh` |
| C11 code, exhaustive, small programs | **GenMC** | RC11 (also RA, IMM, TSO, SC) | assertion failures, lost wake-ups, races on plain data, under *all* C11-allowed executions | `c/verify/genmc/` |
| Axiomatic litmus shapes | **herd7** (herdtools7) | `rc11.cat` (C11 variants available) | whether an outcome is allowed by the model; also flags undefined behaviour (races) | `c/verify/litmus/`, `spec/litmus/` |
| Real hardware | stress litmus harness | actual CPU (arm64 and x86-64) | compiler/hardware mis-mappings of the orders (smoke, not proof) | `c/verify/stress/` |
| Memory safety / data races in the build | ASan, UBSan, TSan | instrumented execution | UB, leaks, races observed at runtime | CMake `HQQ_SANITIZE` |

None of the model checkers verifies compiled code or real hardware; they check
the C11 *source-level* contract. The stress harness and the arm64 CI runners
are the empirical complement. CBMC and similar bounded model checkers were
considered and **not** adopted: [INFERENCE, from CBMC's documentation, not
tried here] its weak-memory support targets hardware-style models (TSO, PSO,
Power via `--mm`) rather than the C11/RC11 orders the code is written against,
and its sequential bounded checking duplicates what TLC and GenMC already give.
Revisit only if a sequential property (for example buffer arithmetic overflow
in the superblock parser) needs it.

## 2. The invariant under test

Slot publication and wake handshake, modelled in `c/verify/genmc/mp_wake.c`:

```
producer                          consumer
data = 42            (plain)      waiters += 1                [W]
seq  = 1             [P]          s = seq                     [R]
w = waiters          [V]          if (s == 1) read data (plain)
if (w == 0) no_wake = 1           else would_park = 1
```

Invariants: (1) **no lost wake-up**: never `would_park && no_wake`;
(2) **publication**: if the consumer sees `seq == 1`, it reads `data == 42`
and the plain accesses do not race.

`[P] [V] [W] [R]` form a store-buffering pair, so release/acquire is **not
enough**; they must be `seq_cst`, or release/acquire separated by
`atomic_thread_fence(memory_order_seq_cst)` on both sides. The publication
store `[P]` must additionally be at least a *release* (it is, in both correct
variants), and the consumer's slot check an *acquire*. A C port must keep
exactly one of these two forms for the four accesses.

## 3. GenMC

[GenMC](https://github.com/MPI-SWS/genmc) is a stateless model checker that
works on LLVM IR and supports RC11, RA, IMM, TSO and SC.

**Reproducible install (Ubuntu 24.04, apt LLVM 19)** is
[`c/verify/install-genmc.sh`](../../c/verify/install-genmc.sh): it checks out the
pinned commit `9f6c4c0772d0c42b325681581acc6a0f3eb9b5f7` (tag `v0.19.0`, verified
after checkout), configures with `CMAKE_PREFIX_PATH=/usr/lib/llvm-19/cmake` and
Release, builds with clang-19 and installs into a prefix. GenMC v0.19.0 supports LLVM
19-22 and needs a C++20 compiler. Build dependencies:
`cmake make bc llvm-19-dev clang-19 libclang-19-dev libffi-dev zlib1g-dev libedit-dev git`.

Local reproduction in Docker (arm64 or amd64 host):

```sh
docker build -t hqq-genmc -f c/verify/genmc/Dockerfile c/verify
docker run --rm -v "$PWD":/src:ro hqq-genmc /src/c/verify/genmc/run.sh --genmc genmc
```

Notes learned while building it: `GenMCDriver.cpp` needs more than 2 GB of RAM
with g++-14, which was OOM-killed on a 4 GB Docker VM; clang-19 compiles it in
well under that, so clang is the script's default host compiler (`CC=`/`CXX=`
override it). Use `JOBS=1` on small machines. The result is not cached by the
Dockerfile layers on failure, so the Dockerfile uses a BuildKit cache mount for
the build tree. CI caches the installed prefix keyed on commit, LLVM version and
the script hash.

**Proof of concept**: [`c/verify/genmc/mp_wake.c`](../../c/verify/genmc/mp_wake.c),
run by [`c/verify/genmc/run.sh`](../../c/verify/genmc/run.sh) with
`genmc -rc11 -disable-mm-detector --disable-estimation -- -DVARIANT_x mp_wake.c`.
(`-disable-mm-detector` stops GenMC from silently downgrading the model to RA
for programs that only use acquire/release.)

| Variant | Orders | Expected | Observed (GenMC v0.19.0, LLVM 19.1.1, arm64 container) |
|---|---|---|---|
| `VARIANT_SEQCST` | `[P] [V] [W] [R]` all `seq_cst` | pass | `Number of complete executions explored: 3`, no error |
| `VARIANT_FENCE` | release store, acquire check, `seq_cst` fence between store and load on both sides, relaxed waiter ops | pass | `Number of complete executions explored: 3`, no error |
| `VARIANT_ACQREL` | release / acquire / acq_rel only | **fail** (lost wake-up) | `Error: Safety violation!`, counterexample graph: producer `Rrlx/Racq (waiters, 0) [INIT]` then `Wna (no_wake, 1)`; consumer `UWar (waiters, 1)` then `Racq (seq, 0) [INIT]` then `Wna (would_park, 1)`; exit status 42 |
| `VARIANT_RELAXED_PUB` | correct handshake, but `[P]` is a relaxed store | **fail** (publication race) | `Error: Non-atomic race! Event (2, 5) conflicts with event (1, 1)`: the consumer's `Rna (data, 42)` races with the producer's `Wna (data, 42)`; exit status 42 |

Interpretation: the seq_cst and fence forms are the two acceptable
implementations of the handshake; weakening them to acquire/release breaks the
no-lost-wake-up invariant, and weakening the publication store breaks payload
visibility. `run.sh` asserts all four verdicts (and the key lines of the two
counterexamples), so a regression in a future edit of the PoC, or in GenMC,
turns the CI job red.

Limits: GenMC checks a program of two threads and one slot, not the library; it
treats `waiters`/`seq` as ints and does not model futex/semaphore blocking, a
process dying while parked, or mixed-size accesses. The ring-core harnesses of
section 3.2 use the real header, with the same blind spots: the wait hook is a
recording stub, not a blocking kernel call.

### 3.2 Harnesses for the real ring core (M3c)

GenMC runs only in the CI `memory-model` job. It is not run on the development
hosts (no docker, no heavy installs), so nothing in this section has been
observed under GenMC yet.

The harnesses check the protocol of the real header, `c/src/hqq_ring.h`, which
they include unmodified through `c/verify/genmc/hqq_model.h`. Each drives the ring
through a small fixed scenario:

| Harness | Scenario | Checked in every execution | Positive (must pass) | Negative (must fail) |
|---|---|---|---|---|
| `ring_mp.c` | 1 producer, 1 consumer, 2 slots, 4 messages (messages 2 and 3 wrap onto slots 0 and 1) | FIFO delivery, each payload once; the payload read is ordered after its publish (otherwise a race on plain memory) | `run.sh mp` | `-DHQQ_VERIFY_WEAKEN_RELAXED`: publication race |
| `ring_wake.c` | waiter (peek, `hqq_ring_park`, peek) and waker (claim, write, `hqq_ring_publish`), 2 slots | no lost wake-up: if the waiter sleeps on word `v`, the waker bumps the word after the waiter's read | `run.sh wake` | `-DHQQ_VERIFY_WEAKEN_ACQREL`: lost wake-up |
| `ring_reserve.c` | main claims one reservation; two completers, each with its own copy of the handle, call `hqq_ring_publish_guarded` | exactly one completion; a stale copy changes nothing; the payload is consumed once from the reserved position | `run.sh reserve` | `-DHQQ_VERIFY_UNGUARDED` (plain `hqq_ring_publish`): two completions |
| `ring_claim.c` | 2 producers race for the producer cursor (CAS); the main thread consumes after the join | the two claims are distinct; each payload arrives once, from the position its producer got | `run.sh claim` | none: the cursor CAS is an RMW and stays atomic under acquire/release |

The PoC `mp_wake.c` is still run, as target `poc`.

**Weakened builds.** `hqq_model.h` rebinds the atomic macros only in the weakened
builds (through the clang `__c11_atomic` builtins). `HQQ_VERIFY_WEAKEN_ACQREL`
turns every seq_cst access of the ring core into the strongest acquire, release,
or acq_rel order valid for that operation (CAS: acq_rel on success, acquire on
failure). `HQQ_VERIFY_WEAKEN_RELAXED` makes them all relaxed. The header's own
acquire/release/relaxed accesses are not changed, and the harness code never uses
seq_cst. The LLVM IR of the ACQREL build was inspected on the host: the handshake
accesses are acquire loads, release stores and acq_rel RMWs.

**Hooks.** In GenMC, `hqq_ring_ops.wait` stays asleep until the word differs from `v`
or the waker has finished (`HQV_PEER_DONE()`, called after its last publish). A kernel
`wait(word, v)` blocks forever only if the word still equals `v` when the waiter
sleeps and nobody bumps it later, and after the waker's last publish nobody can. So
`ring_wake.c` asserts, whenever the waiter went to sleep, that the word differs from
`v` at the end: exactly "no lost wake-up". An earlier stub returned at once, so the
waiter left the park (decremented `waiters`) before the waker read the count, and
the correct protocol was reported as a lost wake-up (CI run 38945948026, `wake (none)`).
Producer and consumer retries use `__VERIFIER_assume` (`HQV_AWAIT`):
an execution in which a retry still fails is discarded, not reported. run.sh
requires at least one complete execution for every positive variant, so a
vacuous pass fails.

**Host build** (`-DHQQ_VERIFY_HOST`). The same files compile as ordinary pthread
programs. The hooks are a mutex and condition variable, and a waiter still asleep
on an unchanged word after 5 s aborts. `run.sh --host N` runs each harness N times.

```sh
c/verify/genmc/run.sh --genmc PATH [poc|mp|wake|reserve|claim ...]   # GenMC, CI only
CC=clang c/verify/genmc/run.sh --check      # host syntax, every variant (weakened: clang)
c/verify/genmc/run.sh --host 20000          # host pthread run
```

**Observed on the host** (macOS arm64, Apple clang 21): `run.sh --check` passes
for every variant under `-std=c11 -Wall -Wextra -Wpedantic -Werror`, with and
without `-DHQQ_VERIFY_HOST`. Each of the four harnesses ran 10 000 iterations
natively with exit status 0.

**GenMC verdicts (CI run 38945948026, job `memory-model`).** The four PoC verdicts
matched the table above. Three ring-harness verdicts did not: `mp (none)` and
`mp -DHQQ_VERIFY_WEAKEN_RELAXED` failed without the expected output (no
`Number of complete executions` and no `Error` line, so GenMC stopped abnormally;
the cause is not known yet), and `wake (none)` was the stub defect fixed above.
`reserve (none)` did not finish within the 60-minute job limit. `run.sh` now bounds
each GenMC run with `GENMC_TIMEOUT` (300 s in CI; a hang reports `TIMEOUT`) and prints
the last 40 lines of GenMC's output on every mismatch. The first suspect for the
`mp` failure is the unbounded CAS retry loops of the claim functions (risk (1) above);
a loop-bounding option must be checked against GenMC v0.19.0 before use. The herd7
steps run regardless of GenMC (`if: always()`); neither tool has a passing CI verdict yet.

### 3.3 Status summary

| Item | Status |
|---|---|
| PoC `mp_wake.c` (4 variants) | written; GenMC verdicts observed (section 3) |
| Harnesses `ring_mp`, `ring_wake`, `ring_reserve`, `ring_claim` (positive and negative) | written; host syntax and native run observed; GenMC **not run** |


## 4. herd7 (herdtools7)

`herd7` evaluates litmus tests against axiomatic cat models. The C11 tests are
in `c/verify/litmus/*.litmus` (store buffering with `seq_cst`, acq/rel, and
fences; message passing with release/acquire and relaxed) and the handshake /
publication / slot-wraparound tests in `spec/litmus/` (owned by the spec
baseline). [`c/verify/litmus/run-herd.sh`](../../c/verify/litmus/run-herd.sh) runs
both sets under `rc11.cat` and checks the expectations in `expected.txt` and
`spec-expected.txt` (`Never`, `Sometimes`, or `Bug` = reachable or flagged as
undefined behaviour).

**Install.** The Debian/Ubuntu archive does not ship herdtools7. CI and Docker use
[`c/verify/install-herd.sh`](../../c/verify/install-herd.sh): opam with the system OCaml
(`ocaml-system`, 4.14 on Ubuntu 24.04) and the pinned `herdtools7.7.58`
(`dune`, `menhir`, `zarith` are pulled in by opam). Build dependencies:
`opam ocaml-nox build-essential libgmp-dev pkg-config m4 unzip curl git`.
`HERD_JOBS=1` limits build parallelism: the herdtools7 Makefile runs
`dune build -j $(nproc)` and the native build of `herd7` was OOM-killed on a
4 GB Docker VM with 4 jobs (the script exports `OMP_NUM_THREADS`, which `nproc`
honours). The opam root is cached by CI.

```sh
c/verify/install-herd.sh                       # installs into the opam switch "herd"
eval "$(opam env --switch=herd)"
c/verify/litmus/run-herd.sh "$(command -v herd7)"
```

**Observed** (herdtools7 7.58, `rc11.cat`, Ubuntu 24.04 arm64 container):

```
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

The `*undef*` flag on the `_relaxed_bug` tests is the data race on the plain
payload; `slot_wraparound_handoff_relaxed_bug` is therefore recorded as a
race (`Bug`), not as a reachable condition.

## 5. Stress litmus harness (hardware)

[`c/verify/stress/stress_litmus.c`](../../c/verify/stress/stress_litmus.c) runs
two threads through N iterations per test, with a spin barrier at every
iteration so both threads enter the racy window together (bounded by an
iteration count and a time budget; default 200 000 iterations or 1 s per test;
at most 50 M iterations / 60 s). It checks store buffering (SB, the Dekker
core) and message passing (MP) for:

- *strong* variants that must never show the forbidden outcome: SB `seq_cst`,
  MP release/acquire, MP `seq_cst`; the process fails (exit 1) if one does;
- *control* variants that are allowed to (SB acq/rel, SB relaxed, MP relaxed):
  they prove the harness can observe reordering on the machine. A control that
  never shows a weak outcome is only reported (`--require-control` makes it an
  error). If `seq_cst` were mis-compiled to a plain store/load (no `dmb`/`mfence`/
  `xchg`), the SB `seq_cst` line would turn from `weak=0` into the SB
  control's order of magnitude (tens of percent).

It is built by CMake (`HQQ_BUILD_STRESS`, POSIX threads only) and registered
as the `stress_litmus` ctest; on the arm64 CI runners an extra step runs three
passes of 3 s per test.

**Observed** (100 000 iterations per test, 0.5 s budget):

macOS arm64 (Apple M-series host, Apple clang):

```
hqq stress litmus: hqq 0.0.0-scaffold, arm64
SB seq_cst   (strong)    iterations=100000   weak=0        PASS
SB acq/rel   (control)   iterations=100000   weak=81784    weak outcomes observed (expected for a control)
SB relaxed   (control)   iterations=100000   weak=70774    weak outcomes observed (expected for a control)
MP rel/acq   (strong)    iterations=100000   weak=0        PASS
MP seq_cst   (strong)    iterations=100000   weak=0        PASS
MP relaxed   (control)   iterations=100000   weak=78       weak outcomes observed (expected for a control)
```

Three consecutive host runs gave the same picture (SB controls 67 000 to
82 000 weak outcomes per 100 000, MP relaxed 51 to 96, strong variants 0).
Inside a Linux arm64 Docker VM (4 shared vCPUs under heavy load from other
containers, gcc, same harness) the controls did **not** fire
(`weak=0, no weak outcome observed` for all three, strong variants `PASS`): with
oversubscribed vCPUs the two threads rarely overlap inside the window. This is
the expected limitation of a stress test; it is why controls are informational
by default and why the arm64 CI step repeats the run three times with a longer
budget.

The stress harness is a smoke test: absence of a weak outcome proves nothing,
and presence in a *strong* variant is a real bug. Not available on Windows
(Win32 threads port is open work).

## 6. Sanitizers

`HQQ_SANITIZE=address+undefined|thread|...` (see
[`C_TOOLCHAIN.md`](C_TOOLCHAIN.md)). Observed locally: macOS arm64 Apple clang
ASan+UBSan and TSan, Linux arm64 clang-18 ASan+UBSan and TSan, all `ctest`
green. TSan does not model standalone `atomic_thread_fence`; fence-based
protocol code (`VARIANT_FENCE`) must be validated with GenMC/herd7, and
GCC rejects such code under `-fsanitize=thread -Werror`, so CI uses clang for
TSan.

## 7. Running everything

```sh
# GenMC (needs the installed prefix or the hqq-genmc image)
c/verify/genmc/run.sh --genmc "$HOME/.local/genmc/bin/genmc"     # all GenMC targets (CI)
c/verify/genmc/run.sh --genmc "$HOME/.local/genmc/bin/genmc" wake # one harness
CC=clang c/verify/genmc/run.sh --check   # host syntax of every variant
c/verify/genmc/run.sh --host 20000       # host pthread run of the ring harnesses
# herd7
c/verify/litmus/run-herd.sh "$(command -v herd7)"
# stress on this machine
cmake -S c -B build -DHQQ_WERROR=ON && cmake --build build && build/verify/stress/hqq_stress_litmus
```

## 8. Open items

- The real ring core is driven by the GenMC harnesses of section 3.2. They have
  not been run under GenMC yet: the first CI run of the `memory-model` job is
  the first observation, and its verdicts are to be recorded in section 3.2.
- Port the stress harness to Windows threads (needed to promote MSVC).
- A cross-process stress run over a shared mapping (two processes) once
  the segment code exists; the stress harness above uses threads.
