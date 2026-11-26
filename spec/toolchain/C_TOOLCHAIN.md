# C Toolchain and CI Matrix

**Status**: Active specification for `libhqq` in [`c/`](../../c).

This document defines the C language standard, CMake build options, compiler and C11 `<stdatomic.h>` support policy, and CI test matrix ([`.github/workflows/c.yml`](../../.github/workflows/c.yml)) for the `libhqq` C11 reference library.

## 1. Language Standard and Memory Layout Contract

`libhqq` is written in strict ISO C11 and exposes a header (`c/include/hqq/hqq.h`) that compiles cleanly under both C11 and C++17:

- **ISO C11 with `<stdatomic.h>` (`CMAKE_C_STANDARD 11`, compiler extensions disabled)**: Every shared-memory word is declared as a C11 `_Atomic uint32_t` or `_Atomic uint64_t` and accessed with explicit `memory_order_*` operations as specified in [`spec/MEMORY_MODEL.md`](../MEMORY_MODEL.md) and verified by [`VERIFICATION_TOOLING.md`](VERIFICATION_TOOLING.md). In particular, the four accesses of the Dekker wait/wake handshake (`W1`, `W3`, `K1`, `K2`) use `memory_order_seq_cst`.
- **Mandatory always-lock-free 32-bit and 64-bit atomics**: Shared-memory atomic operations across independent processes must never fall back to a process-private spinlock table. Both CMake configure checks and compile-time `#if` guards in `c/src` require `ATOMIC_INT_LOCK_FREE == 2 && ATOMIC_LLONG_LOCK_FREE == 2`. Consequently, only 64-bit targets (`x86_64` and `arm64`) are supported.
- **C++17 header compatibility**: `c/include/hqq/hqq.h` is wrapped in `extern "C"` and intentionally omits `<stdatomic.h>` from the public header so C++17 translation units can include it directly without C/C++ atomic keyword conflicts. C++ consumers use `<atomic>` (`std::atomic<uint32_t>`, `std::atomic<uint64_t>`), which shares the exact lock-free hardware representation on all supported compilers. The `c/tests/test_cxx17.cpp` test and the `cxx17` CI job verify header compatibility across `g++` and `clang++` under `-std=c++17`, `-std=c++20`, `-std=c++23`, and `-std=c11`.
- **Feature-detection macros in `hqq.h`**: `HQQ_VERSION_*`, `HQQ_C11`, `HQQ_CXX17`, `HQQ_C11_ATOMICS`, `HQQ_COMPILER_{GCC,CLANG,MSVC}`, `HQQ_OS_{LINUX,DARWIN,WINDOWS}`, `HQQ_ARCH_{X86_64,ARM64}`, and `HQQ_API`.

## 2. CMake Configuration Options

Configure, build, and test `libhqq` with CMake 3.21+ and Ninja:

```sh
cmake -S c -B build -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_WERROR=ON
cmake --build build
ctest --test-dir build --output-on-failure
```

The build supports the following CMake cache options:

| Option | Default | Description |
| --- | --- | --- |
| `HQQ_WERROR` | `OFF` | Treat compiler warnings as errors (`-Werror` on GCC/Clang, `/WX` on MSVC/`clang-cl`). Enabled in CI. |
| `HQQ_SANITIZE` | `none` | Enable runtime sanitizers: `none`, `address`, `undefined`, `address+undefined`, or `thread`. |
| `HQQ_BUILD_TESTS` | `ON` (top-level) | Build the 28 unit, lifecycle, fuzz, and Golden-vector test executables. |
| `HQQ_BUILD_CXX_CHECK` | `ON` | Build and run the C++17 consumer compatibility test (`test_cxx17`). |
| `HQQ_BUILD_STRESS` | `ON` (non-Windows) | Build the bounded weak-memory stress litmus test runner (`stress_litmus`, POSIX threads). |
| `HQQ_BUILD_SHARED` | `OFF` | Build `libhqq` as a shared library (`.so` / `.dylib` / `.dll`) instead of a static archive. |

Compiler flag conventions:

- **Warnings**: GCC, Clang, and Apple Clang compile with `-Wall -Wextra -Wpedantic` (plus `-Werror` when `HQQ_WERROR=ON`). MSVC `cl.exe` and `clang-cl` compile with `/W4` (plus `/WX` when `HQQ_WERROR=ON`).
- **Sanitizers**: Sanitizers pass `-fsanitize=...` at both compile and link stages, with `-fno-sanitize-recover=all` enabled for UBSan so any undefined behavior fails the test immediately. MSVC `cl.exe` supports `HQQ_SANITIZE=address`.
- **Platform feature macros**: POSIX targets compile with `-std=c11 -D_DEFAULT_SOURCE` so `glibc` exposes POSIX.1-2008 and BSD symbols (`O_CLOEXEC`, `ftruncate`, `nanosleep`, `MAP_ANON`). Windows targets define `_CRT_SECURE_NO_WARNINGS` and `_CRT_NONSTDC_NO_WARNINGS`, and avoid the identifier `small` (which Windows `<rpcndr.h>` defines as a macro).

## 3. Compiler and Atomics Support Policy

Compilers are divided into two tiers:

- **Supported**: Built and tested at the cadence in the CI table below. Failures remain blocking for each scheduled job.
- **Best effort**: Available for manual local verification. These compiler variants do not allocate CI runners.

| Platform | Compiler | Atomics implementation | CI cadence |
| --- | --- | --- | --- |
| Linux x86_64 | GCC 14, Clang 18 | C11 atomics | Pushes and pull requests |
| Linux arm64 | Clang 18 | C11 atomics; native weak-memory stress | Pushes and pull requests |
| macOS arm64 | Apple Clang | C11 atomics | Weekly and manual |
| Windows x64 | clang-cl | Clang C11 atomics | Weekly and manual |
| Linux x86_64 and arm64 sanitizers | Clang 18 | ASan/UBSan and TSan | Pushes and pull requests |
| Linux C++ header consumers | g++ 14, clang++ 18 | C++17, C++20, C++23 and C11 headers | Pushes and pull requests |

GCC 13, Clang 20, MSVC, macOS sanitizers, native Intel macOS, and native
Windows arm64 are outside the automated matrix. This is a CI coverage
reduction, not a claim that those compiler or runtime combinations passed.

### 3.1 Windows Compiler Policy: `clang-cl` (Supported) and MSVC `cl.exe` (Best Effort)

Microsoft Visual Studio 2022 17.5+ and Visual Studio 2026 provide lock-free C11 `<stdatomic.h>` support behind `/std:c11 /experimental:c11atomics` with the following characteristics:

- **Experimental flag requirement**: MSVC requires both `/std:c11` (or `/std:c17`) and `/experimental:c11atomics`. In `c/CMakeLists.txt`, the `HQQ_HAVE_C11_ATOMICS` probe sets `CMAKE_REQUIRED_FLAGS "/std:c11 /experimental:c11atomics"` before running `check_c_source_compiles` so the probe accurately detects MSVC's C11 atomics support.
- **`__STDC_NO_ATOMICS__` macro**: Even with `/experimental:c11atomics` enabled, `cl.exe` continues to predefine `__STDC_NO_ATOMICS__`. When `HQQ_HAVE_C11_ATOMICS` succeeds on MSVC, `c/CMakeLists.txt` defines `HQQ_FORCE_C11_ATOMICS=1`, which `c/include/hqq/hqq.h` honors.
- **Lock-free ABI compatibility**: MSVC's C11 atomics support power-of-two types up to 8 bytes (`ATOMIC_INT_LOCK_FREE == 2` and `ATOMIC_LLONG_LOCK_FREE == 2`) using the exact same ABI and `seq_cst` code generation (`xchg` / `lock cmpxchg` on x64, `ldar` / `stlr` on ARM64) as MSVC's C++ `std::atomic<T>`.

Because Microsoft still marks `/experimental:c11atomics` as experimental and the weak-memory stress suite (`c/verify/stress/`) currently uses POSIX threads, **`clang-cl` is the primary supported Windows C compiler** and **MSVC `cl.exe` remains a manual best-effort target**. Windows applications written in C++ can always consume `libhqq` using standard C++17 `std::atomic`.

### 3.2 Additional Compiler Notes

- **GCC and ThreadSanitizer (`-Wtsan`)**: GCC 11+ emits a `-Wtsan` warning on `atomic_thread_fence` under `-fsanitize=thread` because TSan does not model standalone fences. Under `-Werror`, this warning is fatal, so CI runs TSan jobs using Clang (`clang-18`) and verifies standalone-fence litmus models with GenMC (`c/verify/genmc/`).
- **Linux TSan ASLR setting**: Modern Linux kernels require `sysctl vm.mmap_rnd_bits=28` for Clang ThreadSanitizer v2 mappings; the `linux-sanitizers` CI job configures this automatically.
- **Apple Clang**: Ships a complete C11 `<stdatomic.h>` with no special flags required.
- **32-bit architectures**: Out of scope by design (`sizeof(void*) == 8` and `ATOMIC_LLONG_LOCK_FREE == 2` are enforced at compile time).

## 4. CI Jobs (`c.yml`)

Workflow [`.github/workflows/c.yml`](../../.github/workflows/c.yml) runs the following verification jobs:

| Job | Depot runners | Verification |
| --- | --- | --- |
| `linux` | `depot-ubuntu-24.04` and `depot-ubuntu-24.04-arm` | Three compiler/architecture cells; full release `ctest`; bounded ARM weak-memory stress |
| `linux-sanitizers` | Both Linux architectures | ASan/UBSan and TSan; full debug `ctest` |
| `macos` | `depot-macos-latest` | Weekly/manual Apple Clang arm64 release `ctest` |
| `windows` | `depot-windows-2025` | Weekly/manual clang-cl x64 release `ctest` |
| `cxx17` | `depot-ubuntu-24.04` | Both compiler families; all header standards and the C++17 consumer |
| `memory-model` | `depot-ubuntu-24.04` | GenMC and herd7 checks |

To reproduce the full Linux compiler and sanitizer matrix locally inside Docker, run:

```sh
c/verify/docker-matrix.sh [gcc-13 gcc-14 clang-18 clang-20]
```

Set `PLATFORM=linux/amd64` to force an architecture or `SANITIZE="address+undefined thread"` to run the sanitizer builds.
