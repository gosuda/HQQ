# libhqq (C reference library)

Scaffold for the C reference implementation of the HQQ shared-memory protocol.
The superblock and ring code is not here yet; this tree currently provides the
build system, a smoke test, the CI matrix and the memory-model verification
tooling. See [`spec/toolchain/C_TOOLCHAIN.md`](../spec/toolchain/C_TOOLCHAIN.md)
and [`spec/toolchain/VERIFICATION_TOOLING.md`](../spec/toolchain/VERIFICATION_TOOLING.md).

```sh
cmake -S c -B build -G Ninja -DCMAKE_BUILD_TYPE=Release -DHQQ_WERROR=ON
cmake --build build
ctest --test-dir build --output-on-failure
```

Sanitizers: `-DHQQ_SANITIZE=address+undefined` or `-DHQQ_SANITIZE=thread`.

## Layout

| Path | Content |
|---|---|
| `include/hqq/hqq.h` | Public header (C11 and C++17): version and feature-test macros |
| `src/` | Library sources (currently a smoke implementation) |
| `tests/` | Dependency-free test runner and smoke / C++17 tests |
| `cmake/` | Compiler flags, atomics requirements, sanitizer selection |
| `verify/stress/` | Bounded weak-memory stress litmus harness (POSIX threads) |
| `verify/genmc/` | GenMC proof of concept (message passing + Dekker wake handshake) |
| `verify/litmus/` | herd7 litmus tests and runner (also runs `spec/litmus/`) |
| `verify/install-genmc.sh`, `verify/install-herd.sh` | Pinned installers used by CI and Docker |
| `verify/docker-matrix.sh` | Reproduce the Linux CI jobs in a container |

## Rules

- C11, `<stdatomic.h>`, always-lock-free 32/64-bit atomics only.
- The public header must stay valid C++17 (`extern "C"`) and must not include
  `<stdatomic.h>`.
- Warnings are errors in CI (`-Wall -Wextra -Wpedantic -Werror`, `/W4 /WX`).
