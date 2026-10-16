# C toolchain and CI matrix

Status: draft for milestone M3a (scaffold). The C reference library (`libhqq`)
lives in [`c/`](../../c). This document fixes the language level, the build
options, the compiler/atomics support policy, and the CI matrix
([`.github/workflows/c.yml`](../../.github/workflows/c.yml)). The Go CI
(`ci.yml`) is unaffected; `c.yml` only triggers on changes to `c/**`,
`spec/**` and itself.

## 1. Language level and layout

- **C11 with `<stdatomic.h>`** (`CMAKE_C_STANDARD 11`, extensions off). All
  shared-memory words are C11 atomics (`_Atomic uint32_t`, `_Atomic uint64_t`)
  accessed with explicit memory orders. The orders required by the protocol are
  defined by the spec (see `spec/` and
  [`VERIFICATION_TOOLING.md`](VERIFICATION_TOOLING.md)); the key one is that
  the wake handshake (slot sequence vs. waiter count) needs `seq_cst`.
- **Always-lock-free 32/64-bit atomics are mandatory.** An atomic in shared
  memory must not use a process-local lock. `c/src` refuses to compile unless
  `ATOMIC_INT_LOCK_FREE == 2 && ATOMIC_LLONG_LOCK_FREE == 2`, and CMake probes
  this before building anything. Consequently only 64-bit targets (x86-64,
  arm64) are in scope.
- **C++17 consumers.** `c/include/hqq/hqq.h` is valid C11 *and* C++17
  (`extern "C"`). It deliberately does not include `<stdatomic.h>`, which
  C++17 does not provide; the C++ side uses `<atomic>` where it needs atomics
  (C and C++ atomics of the same lock-free type are layout-compatible on all
  supported compilers). `c/tests/test_cxx17.cpp` and the `cxx17` CI job check
  this with g++ and clang++ (`-std=c++17`, plus `-fsyntax-only` of the header
  under C++20/23 and C11).
- Feature-test macros in `hqq.h`: `HQQ_VERSION_*`, `HQQ_C11`, `HQQ_CXX17`,
  `HQQ_C11_ATOMICS`, `HQQ_COMPILER_{GCC,CLANG,MSVC}`, `HQQ_OS_{LINUX,DARWIN,WINDOWS}`,
  `HQQ_ARCH_{X86_64,ARM64}`, `HQQ_API`.

## 2. CMake options

```sh
cmake -S c -B build -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_WERROR=ON
cmake --build build
ctest --test-dir build --output-on-failure
```

| Option | Default | Meaning |
|---|---|---|
| `HQQ_WERROR` | `OFF` | Warnings as errors. CI sets it to `ON`. |
| `HQQ_SANITIZE` | `none` | `none`, `address`, `undefined`, `address+undefined`, `thread`. |
| `HQQ_BUILD_TESTS` | `ON` (top level) | Unit tests and smoke tests. |
| `HQQ_BUILD_CXX_CHECK` | `ON` | C++17 consumer compile+run check. |
| `HQQ_BUILD_STRESS` | `ON` (non-Windows) | Bounded weak-memory stress litmus harness (POSIX threads), registered as the `stress_litmus` ctest. |
| `HQQ_BUILD_SHARED` | `OFF` | Build `libhqq` as a shared library. |

Warning flags: GCC/Clang/Apple clang `-Wall -Wextra -Wpedantic [-Werror]`;
MSVC and clang-cl `/W4 [/WX]`. Sanitizers compile and link with
`-fsanitize=...` (UBSan is configured fatal via `-fno-sanitize-recover`).
`cl.exe` accepts only `HQQ_SANITIZE=address`.

## 3. Compiler and atomics support policy

*Supported* = built and tested on every push, failure blocks the merge.
*Best effort* = built in CI with `continue-on-error`; failures are reported but
do not block, and are triaged, not ignored.

| Platform | Compiler | Atomics | Status |
|---|---|---|---|
| Linux x86-64 (`ubuntu-24.04`) | gcc 13, gcc 14 | `<stdatomic.h>` (libgcc/inline) | **Supported** |
| Linux x86-64 | clang 18, clang 20 | `<stdatomic.h>` (clang builtin header) | **Supported** |
| Linux arm64 (`ubuntu-24.04-arm`) | gcc 13, gcc 14, clang 18, clang 20 | same | **Supported** (real weak-memory hardware; also runs the bounded stress litmus) |
| macOS arm64 (`macos-latest`) | Apple clang | `<stdatomic.h>` | **Supported** |
| macOS x86-64 (`macos-15-intel`) | Apple clang | `<stdatomic.h>` | **Supported** |
| Windows x64 (`windows-latest`) | **clang-cl** (LLVM 20 on the runner) | clang's own `<stdatomic.h>` (`__c11_atomic_*` builtins) | **Supported** |
| Windows x64 | MSVC `cl` 19.4x (VS 2022 17.14) | `/std:c11 /experimental:c11atomics` | **Best effort** |
| Windows arm64 (`windows-11-arm`) | clang-cl (LLVM 22), MSVC `cl` | as above | **Best effort** (runner label exists; neither compiler has been exercised on Windows by the authors, see below) |
| Linux/macOS | g++ 14, clang++ 18 | headers only, C++17 | **Supported** (compile check) |
| Sanitizers: Linux x86-64 and arm64 | clang 18: ASan+UBSan, TSan | | **Supported** |
| Sanitizers: macOS arm64 | Apple clang: ASan+UBSan, TSan | | **Best effort** (TSan on hosted macOS depends on the OS release) |

Runner labels were taken from the GitHub-hosted runners reference
(`ubuntu-24.04`, `ubuntu-24.04-arm`, `macos-latest` = arm64, `macos-15-intel`,
`windows-latest`, `windows-11-arm`).

### 3.1 Why MSVC `cl` is best effort and clang-cl is the supported Windows compiler

Facts from Microsoft's documentation (C++ Team Blog "C11 Atomics in Visual
Studio 2022 version 17.5 Preview 2"; `/std` option reference):

- C11 atomics need **VS 2022 17.5 or later**, `/std:c11` (or later) **and**
  `/experimental:c11atomics`. The feature is still flagged experimental.
- `__STDC_NO_ATOMICS__` is **still defined** even with the flag. Probes must
  check that a translation unit including `<stdatomic.h>` compiles (CMake does
  this via `check_c_source_compiles`; `hqq.h` honours `HQQ_FORCE_C11_ATOMICS`).
- Only **lock-free** atomics are supported (objects of size <= 8, power of two).
  That is exactly what `libhqq` needs: 32- and 64-bit words. The C11
  implementation has the **same ABI as the MSVC STL's C++ `std::atomic`**, and
  its seq_cst ordering is compatible with it, so C and C++ code in one process
  or in two processes sharing a mapping agree on the memory model.
- Plain `=`/`+=` on `_Atomic` objects are seq_cst; `atomic_*_explicit` honours
  explicit orders; `ATOMIC_VAR_INIT` is not provided.
- The `/std` documentation still lists "no conforming multithreading, atomic,
  or complex number support" among the unimplemented *optional* C11 features,
  i.e. the support is outside the conformance guarantees.

Assessment. A lock-free 64-bit seq_cst atomic in shared memory is technically
within what the MSVC implementation provides on x64 and arm64, so MSVC is not
*known* to be inadequate. But the implementation is flag-gated and
experimental, it relies on `__STDC_NO_ATOMICS__` workarounds, and nobody has
verified its seq_cst code generation for the handshake in
[`VERIFICATION_TOOLING.md`](VERIFICATION_TOOLING.md) (the stress harness is
POSIX-threads only today; a Windows-native stress variant is open work).
Therefore:

1. **Fallback / supported configuration on Windows: clang-cl** (also covers the
   Visual Studio "C++ Clang tools" workflow). Its atomics are clang's own
   lock-free `__c11_atomic_*` lowering, the same front end and code generator as
   Linux/macOS clang.
2. **MSVC `cl` is accepted as best effort** with
   `/std:c11 /experimental:c11atomics /W4 /WX`. It is promoted to *supported*
   once (a) the Windows job has been green for a release and (b) the stress
   litmus harness has been ported to Win32 threads and run on x64 and arm64.
3. If an MSVC version is found to mis-order a seq_cst access, the
   protocol-level requirement (seq_cst on the four handshake accesses) is not
   weakened; the compiler is dropped from the support list and clang-cl is the
   only Windows C compiler.
4. Windows consumers written in C++ may always use `<atomic>`
   (`std::atomic<uint32_t/uint64_t>`), which is the stable MSVC implementation
   of the same ABI.

[INFERENCE] The above statement that MSVC's seq_cst lowering is sound is based
on documentation, not on inspection of generated code or an executed test; no
Windows runner was available while writing this.

### 3.2 Other known compiler caveats

- **gcc and TSan.** GCC >= 11 warns (`-Wtsan`) on `atomic_thread_fence` under
  `-fsanitize=thread` because TSan does not model standalone fences. With
  `-Werror` that is fatal, so the CI runs TSan with clang only. Code that is
  specified to rely on fences must also be checked with the model checkers
  ([`VERIFICATION_TOOLING.md`](VERIFICATION_TOOLING.md)), not TSan alone.
- **TSan on Linux** needs `vm.mmap_rnd_bits <= 28` on current runner kernels;
  the job sets it.
- **Apple clang** reports `__STDC_NO_ATOMICS__` as undefined and ships a normal
  `<stdatomic.h>`; no special handling.
- **32-bit targets** are out of scope (64-bit atomics are not always lock-free).

## 4. CI jobs (`c.yml`)

| Job | Runner(s) | What |
|---|---|---|
| `linux` | `ubuntu-24.04`, `ubuntu-24.04-arm` x gcc-13, gcc-14, clang-18, clang-20 | Release build with `HQQ_WERROR=ON`, `ctest`; on arm64 additionally three bounded 3-s passes of `hqq_stress_litmus` |
| `linux-sanitizers` | both architectures x (`address+undefined`, `thread`) | clang-18 Debug builds, `ctest` |
| `macos` | `macos-latest` (arm64), `macos-15-intel` | Apple clang Release, `ctest` |
| `macos-sanitizers` | `macos-latest` | ASan+UBSan and TSan, best effort |
| `windows` | `windows-latest`: clang-cl, MSVC; `windows-11-arm`: clang-cl, MSVC | Ninja + `ilammy/msvc-dev-cmd`; only x64 clang-cl is blocking |
| `cxx17` | `ubuntu-24.04` | g++-14 and clang++-18: header-only `-fsyntax-only` checks (C++17/20/23, C11) and the C++17 consumer test |
| `memory-model` | `ubuntu-24.04` | GenMC proof of concept and herd7 litmus tests |

Local reproduction of the Linux jobs in a container:
`c/verify/docker-matrix.sh [gcc-13 gcc-14 clang-18 clang-20]`
(`PLATFORM=linux/amd64` to force an architecture, `SANITIZE="address+undefined thread"`).

## 5. Not covered yet

- No Windows-native stress harness (POSIX threads only).
- No cross-process shared-memory tests (they come with the ring implementation).
- The Windows jobs and the macOS/Windows Intel/arm64 runners have not been
  executed by the authors of this scaffold; their first run on GitHub is the
  first verification.
