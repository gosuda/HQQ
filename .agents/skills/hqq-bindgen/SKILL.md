---
name: hqq-bindgen
description: "Bootstraps HQQ IDL schemas from C headers (.h), generates zero-cgo Go clients and C11 shared-memory IPC servers with hqq-bindgen, writes C batch shims and security interceptors, and designs idiomatic Go wrapper APIs. Use whenever wrapping a C library for Go or modifying an hqq-bindgen service."
license: MIT
---

# Wrapping C Libraries in Go with `hqq-bindgen`

Build zero-`cgo` Go bindings for C libraries by isolating the C library inside a dedicated C11 server process communicating over HQQ shared-memory rings. This skill guides the complete workflow: bootstrapping the `.idl` from a `.h` file, choosing between direct bindings and C batch shims, generating the Go client and C server, and designing a hard-to-misuse Go API over the generated bindings.

---

## 1. Use This Skill When

Activate this skill when:

- Wrapping a new C or C++ (C ABI) library for use from Go without `cgo`.
- Bootstrapping an initial `.idl` file from an existing C header (`.h`) via `hqq-bindgen init` or `hqq-bindgen -from-header`.
- Adding functions, handles, structs, tagged unions, error conventions, or `@intercept` hooks to an existing `.idl` specification.
- Writing C batch shims (`shim fn`) to amortize chatty C APIs or flatten pointer-bearing C structs.
- Designing idiomatic Go packages (`io.Reader`, `io.Writer`, `database/sql/driver`, or domain clients) on top of `hqq-bindgen` generated code.
- Packaging a standalone HQQ C library repository with `launch`, `secureexec` SHA-256 hash pinning, and normalized upstream version tags.

Combine this skill with `api-design` for public Go signature design and `technical-writing` for package documentation.

---

## 2. Architecture & Performance Economics

Before designing the `.idl` and Go API, evaluate the call pattern of the target C library against the HQQ transport characteristics:

| Call Pattern | Transport Cost | Design Rule |
| :--- | :--- | :--- |
| **Single synchronous call** (fits one slot) | ~190 to 335 ns/op, `0 allocs/op` | Bind directly with `fn` when the C function performs coarse work (>1 us) or runs infrequently. |
| **Independent sequential calls** | ~110 ns/op pipelined | Use generated `<Method>Async` methods returning `*rt.Future[T]` to fill the ring without waiting per call. |
| **Chatty state-machine loops** (e.g., per-row `step` + `column_*`) | N * 300 ns if called 1-by-1 | Do not expose per-cell calls on the hot path. Write a `shim fn` in C that loops in-process and returns a batch `struct`/`union` with `@max_out(BOUND)`. |
| **Large buffer processing** (64 KiB to 16 MiB) | 25+ GB/s shared-memory copy | Declare `out dst: bytes<=MAX, cap: size @len_of(dst)` so the server writes directly into the response slot when it fits one frame. |

---

## 3. Step-by-Step Workflow

### Step 1: Bootstrap the `.idl` from the C Header

Run `hqq-bindgen init` (or `hqq-bindgen -from-header`) against the library's public `.h` file to extract `#define` integer macros, `enum` constants, opaque `struct` handles, fixed-layout structs, and function prototypes:

```bash
go run gosuda.org/hqq/cmd/hqq-bindgen init \
  -service mylib \
  -include "mylib.h" \
  -prefix "mylib_" \
  -serialize-none \
  -idl-out mylib.idl \
  /path/to/mylib.h
```

Review and refine the generated `mylib.idl` across five dimensions:

1. **Concurrency (`@serialize`)**:
   - Keep `@serialize(none)` if separate handles/contexts can be used concurrently by different server threads (one OS thread per HQQ session), such as `zstd` contexts or `sqlite3` connections compiled with `SQLITE_THREADSAFE=1`.
   - Omit `@serialize(none)` (defaulting to `@serialize(session)`) if the C library mutates unsynchronized process-global state.
2. **Handle Lifecycles (`handle`, `owned_by`, `destroy`, `@cancel`)**:
   - Every opaque C pointer (`@c("mylib_ctx_t *")`) must name a `destroy` function taking one `in` parameter of that handle type (`handle Ctx destroy mylib_free;`).
   - If a child handle (`Stmt`) is invalidated whenever its parent handle (`Db`) closes, declare `handle Stmt owned_by Db destroy mylib_stmt_free;`. The C runtime automatically destroys surviving children in reverse creation order before destroying the parent.
   - If the C library provides a thread-safe asynchronous cancellation function (`void sqlite3_interrupt(sqlite3 *)`), attach `@cancel(sqlite3_interrupt)` to the handle declaration so `conn.Cancel(ctx)` interrupts in-flight calls over the control link.
3. **Error Conventions (`@error`)**:
   - Pointer constructors returning `NULL` on failure: `@error(null)`.
   - Integer status codes with known success values: `@error(status, ok(0), detail(mylib_errmsg, ctx))`.
   - Unsigned `size_t` returns with an error predicate function: `@error(predicate(ZSTD_isError), detail(ZSTD_getErrorName, ret))` paired with `@allow(server)` declarations for the predicate and detail functions.
4. **Buffer Bounds and Lengths (`<=`, `@len_of`, `@max_out`)**:
   - Replace generic `MAX_STR_BYTES` and `MAX_BUF_BYTES` constants with domain-specific upper bounds.
   - Verify that every `in` `bytes` or `T[]` parameter has an accompanying integer parameter annotated with `@len_of(buf)`.
   - Verify that every caller-supplied `out` `bytes` parameter has both a capacity `in` scalar (`@len_of(dst)`) and a produced-length `out` scalar or return (`@len_of(dst)`).
5. **Security Interceptors (`@intercept`)**:
   - Annotate functions that open filesystem paths, load plugins, or configure unbounded memory allocations (such as window logarithms or thread pools) with `@intercept`. Implement `<service>_intercept_<fn>` in C to return `HQQRPC_E_DENIED` on disallowed inputs.

---

### Step 2: Write C Shims for Unmappable or Chatty Functions

Look at any `// skipped <fn>:` comments emitted by `hqq-bindgen init`. Replace skipped or chatty functions with `shim fn` declarations when needed:

| C Header Pattern | Why Direct `fn` Cannot Express It | `shim fn` Solution |
| :--- | :--- | :--- |
| **Struct `in` parameter or pointer-bearing struct** (`ZSTD_inBuffer`, `ZSTD_outBuffer`) | Rule `V38` forbids composite `in` parameters; pointers have no wire form. | Flatten the struct fields into explicit `in` / `out` buffer and position parameters on a `shim fn`. |
| **Callback or destructor pointer** (`sqlite3_bind_text64(..., SQLITE_TRANSIENT)`) | Function pointers cannot cross process boundaries. | Declare `shim fn bind_text(stmt: Stmt, idx: i32, text: bytes<=MAX, n: size @len_of(text)) -> i32;` and pass `SQLITE_TRANSIENT` inside the C shim. |
| **High-frequency iteration** (`sqlite3_step` + `sqlite3_column_*`) | Per-cell IPC round-trips dominate execution time. | Define a `union Value` and `struct Rows` in the IDL, declare `shim fn fetch_rows(stmt: Stmt, max: i32) -> Rows @max_out(65536);`, and encode rows in C using `hqqrpc_wr_*` writers. |

---

### Step 3: Generate Code and Build the C11 Server

Add a `generate.go` file to the Go package so regeneration and CI drift checks are reproducible:

```go
//go:generate go run gosuda.org/hqq/cmd/hqq-bindgen -go-out . -go-package mylib -go-strip mylib_ -c-out ./server mylib.idl
package mylib
```

Create the C server entrypoint in `server/main.c`:

```c
#include "mylib_server.h"

int main(int argc, char **argv) {
    return hqqrpc_main(argc, argv, &mylib_iface);
}
```

Implement any `@intercept` hooks and `shim fn` functions in `server/mylib_hooks.c` (including `"mylib_server.h"` so the C compiler checks every prototype). Build the server with CMake linking against `hqq::hqq` and the target C library.

---

### Step 4: Design an Idiomatic Go API Over the Generated Client

Do not force application callers to manage raw `uint64` handle IDs, manual capacity slices, or `*rt.Conn` plumbing unless they specifically want the low-level wire client. Design a high-level Go wrapper following these rules:

1. **Encapsulate Handle Lifecycles**:
   - Wrap generated handle types in Go structs that hold the parent `*Client`, guard against concurrent or duplicate `Close()` calls, and release the underlying server handle on `Close()`.
   - When the C library is stateless or context-pooled (such as `zstd.Compress`), hide handle creation and pooling inside the Go client so callers can invoke top-level methods safely from multiple goroutines.
2. **Eliminate Temporal Initialization**:
   - Constructors (`Dial`, `Open`, `NewWriter`) must return a ready-to-use object or an error. Never expose an uninitialized struct requiring a separate `.Init()` call.
3. **Provide Idiomatic Go Interfaces**:
   - For compression, hashing, or codecs: implement `io.Reader`, `io.Writer`, `io.ReadCloser`, `io.WriteCloser`, and append-style `Compress(dst, src []byte) ([]byte, error)` methods that reuse caller-supplied `dst` capacity when large enough.
   - For databases or storage engines: implement `database/sql/driver` (`Driver`, `Conn`, `Stmt`, `Rows`, `Tx`, `Validator`, `SessionResetter`) inside the top-level package while keeping the generated RPC bindings in a dedicated subpackage (`sqlite3srv`).
4. **Wire `context.Context` Cancellation to `@cancel`**:
   - For blocking or long-running calls on handles with `@cancel`, watch `ctx.Done()` in a companion goroutine (or `context.AfterFunc`) and call `client.Conn().Cancel(cancelCtx)` so the C server invokes the `@cancel` callback without tearing down the session.
5. **Preserve Structured Errors**:
   - Inspect `*rt.LibError` (carrying the library's integer `Code` and `Detail` string) and `*rt.StatusError` (carrying `rt.Status` codes such as `StatusDenied` or `StatusLimit`) with `errors.As`, and expose typed domain errors that work with `errors.Is` and `errors.As`.

---

### Step 5: Process Supervision, `secureexec` Hash Pinning, and Versioning

When packaging a C library wrapper in its own repository (such as `hqq-sqlite3` or `hqq-zstd`):

1. **Supervise Child Processes with `launch` and `secureexec`**:
   - Start the C server binary via `gosuda.org/hqq/launch.Start` with `--parent-stdin` enabled so the C server terminates immediately if the Go parent process dies (including via `SIGKILL`).
   - Pin the expected SHA-256 digests of the official release binaries per `runtime.GOOS + "/" + runtime.GOARCH` in Go code (`hashes.go`), and pass the digest to `launch.Config` / `secureexec.Verify` so the binary is snapshotted into a sealed `memfd`, macOS/BSD `vault`, or locked Windows file handle before execution.
2. **Use Normalized Upstream Versioning**:
   - Tag releases using `v<WrapperMajor>.<WrapperMinor>.<NormalizedCVersion>`:
     - Increment `<WrapperMajor>.<WrapperMinor>` when the Go wrapper API or C shim changes.
     - Set `<NormalizedCVersion>` to the upstream C library's canonical integer version (for example, `3490100` for SQLite `3.49.1.0` -> `v0.1.3490100`; `10507` for Zstandard `1.5.7` -> `v0.1.10507`).

---

## 4. Verification Checklist

Before completing any `hqq-bindgen` task, run and verify:

- [ ] `go run gosuda.org/hqq/cmd/hqq-bindgen -check ...` exits `0` (generated `.go`, `.h`, and `.c` files match the `.idl` byte-for-byte).
- [ ] All handle types in the `.idl` have a valid `destroy` function, and child handles declare `owned_by <Parent>`.
- [ ] Every `str`, `bytes`, and `T[]` parameter and return has an explicit `<=` upper bound, and every composite shim return has `@max_out(...)`.
- [ ] Security-sensitive operations (filesystem paths, dynamic loading, unbounded memory parameters) are guarded by `@intercept` on the server.
- [ ] `go test -race ./...` passes against the compiled C server binary, including crash-recovery tests (`client.Kill()`) and context cancellation tests.
