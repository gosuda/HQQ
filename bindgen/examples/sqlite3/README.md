# `bindgen/examples/sqlite3`: SQLite Service Built with `hqq-bindgen`

This directory implements the out-of-process SQLite service using `hqq-bindgen`: a pure-Go client and the C server (`hqq-sqlite-server` / `sqlite3_server`) generated from [`sqlite3.idl`](sqlite3.idl), paired with a concise C hook file (`server/sqlite3_hooks.c`) and path-policy modules (`server/sqlsrv_policy.c`, `server/sqlsrv_pathrules.c`). Go applications invoke SQLite over HQQ shared memory without linking cgo, and a crash inside SQLite terminates only the worker process.

The `database/sql` driver (`gosuda.org/hqq/sqlite`, driver name `"hqq-sqlite"`) is layered directly on top of this generated client; see [`sqlite/README.md`](../../../sqlite/README.md).

| Path | Role |
| :--- | :--- |
| `sqlite3.idl` | Declarative interface: `Db` (`@cancel(sqlite3_interrupt)`) and `Stmt` handles, the `Value` tagged union, core SQLite functions, and batching shims. |
| `sqlite3_client.go` | Generated pure-Go client (`package sqlite3`). |
| `server/sqlite3_server.{h,c}` | Generated C server dispatch and wire codecs. |
| `server/sqlite3_hooks.c` | Hand-written `sqlite3_open_v2` interceptor, `prepare_one` / `bind_values` / `exec_stmt` / `fetch_rows` / `column_chunk` shims, and per-connection hardening setup. |
| `server/sqlsrv_policy.{h,c}`, `server/sqlsrv_pathrules.{h,c}`, `server/sqlsrv_port*.{h,c}` | Database alias and `--allow-dir` path sandboxing (Linux, macOS, and Windows) with unit tests in `server/tests/test_units.c`. |
| `server/main.c`, `server/sqlite3srv.h` | Process entry point layered on `hqqrpc_main`. |
| `server/CMakeLists.txt` | CMake build producing `hqq-sqlite-server`, `sqlite3_server`, and `hqqsqlite_server_unit_tests`. |
| `build.sh` | Fetches the pinned SQLite amalgamation, builds `sqlite3_server`, and prints `export HQQ_SQLITE3GEN_SERVER=...`. |
| `sqlite3srv/` | Reusable Go test helper that locates or builds `sqlite3_server`, starts it via `launch.Start`, and connects. |
| `generate.go` | `go:generate` directive for `hqq-bindgen`. |
| `*_test.go` | End-to-end integration tests and benchmarks. |

## Build and run

Fetch the pinned SQLite amalgamation, build the C server, and run the test suite:

```sh
eval "$(bindgen/examples/sqlite3/build.sh)"     # fetches pinned SQLite (cached in $TMPDIR), builds server, exports env
go test -race -count=1 ./bindgen/examples/sqlite3/...
```

Or build `hqq-sqlite-server` and its C unit tests from the top-level `c/` CMake project (using system SQLite or `-DHQQ_SQLITE_AMALGAMATION_DIR=...`):

```sh
cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_WERROR=ON
cmake --build build --target hqq-sqlite-server hqqsqlite_server_unit_tests
ctest --test-dir build -R hqqsqlite_server --output-on-failure
```

Passing `-s address+undefined` to `build.sh` compiles the server with AddressSanitizer and UndefinedBehaviorSanitizer; passing `-T` enables a test-only statement-leak hook used by `TestCloseRetainOnFail`. When `HQQ_SQLITE3GEN_SERVER` (or `HQQ_SQLITE_SERVER`) is unset, `go test` compiles the server automatically (with `-T`) on first run and skips cleanly if `cmake`, `ninja`, `unzip`, or network access is unavailable. Set `HQQ_SQLITE3GEN_SANITIZE=address+undefined` to run the auto-built server under sanitizers, or `HQQ_SQLITE_AMALGAMATION_DIR` to point at an already unpacked SQLite amalgamation.

Regenerate bindings after editing `sqlite3.idl`:

```sh
go generate ./bindgen/examples/sqlite3/...
```

(`TestGeneratedFilesAreFresh` runs `hqq-bindgen -check` in CI.)

Server CLI synopsis:

```text
hqq-sqlite-server <listener> [--allow-dir DIR] [--alias NAME=PATH]... [--max-sessions N]
                  [hqqrpc_main flags: --perm --max-handles --max-message --buffer-size --buffer-count
                   --heartbeat-ms --peer-timeout-ms --shutdown-timeout-ms --parent-stdin[=force] ...]
```

On startup the server prints `ready <listener>` on standard output, and logs `sqlite3_server: session started` and `session ended` on standard error as sessions open and finish rolling back or closing their connections.

## Exposed IDL surface

`sqlite3.idl` acts as a strict allowlist: any C function omitted from the IDL has no wire opcode and cannot be invoked (`TestDefaultDeny` verifies `E_UNKNOWN_OP` on unknown opcodes).

| Category | Exposed Functions and Shims |
| :--- | :--- |
| **Connection** | `sqlite3_open_v2` (`@intercept`), `sqlite3_close` (`Db` destructor with `@retain_on_fail` and `@cancel(sqlite3_interrupt)`), `sqlite3_busy_timeout`, `sqlite3_changes64`, `sqlite3_last_insert_rowid`, `sqlite3_get_autocommit`, `sqlite3_errcode`, `sqlite3_extended_errcode`, `sqlite3_errmsg`, `sqlite3_libversion`, `sqlite3_libversion_number` |
| **Statements** | `sqlite3_prepare_v2` (raw multi-statement tail support), `prepare_one` (shim enforcing the single-statement rule), `sqlite3_step`, `sqlite3_reset`, `sqlite3_finalize` (`Stmt` destructor), `sqlite3_clear_bindings`, `sqlite3_stmt_readonly` |
| **Parameter binding** | `sqlite3_bind_parameter_count`, `sqlite3_bind_parameter_index`, `sqlite3_bind_parameter_name`, `sqlite3_bind_int64`, `sqlite3_bind_double`, `sqlite3_bind_null`, `sqlite3_bind_zeroblob64`, `bind_text`, `bind_blob`, `bind_values` (bulk array shim) |
| **Composite operations** | `exec_stmt` (atomic reset + bind + step-to-done + changes + last_insert_rowid), `stmt_info` (parameter names, column names/decltypes, readonly flag), `fetch_rows` (batched row fetch), `column_chunk` (chunked large-cell reader) |

Deliberately omitted from the IDL: `sqlite3_exec`, `sqlite3_load_extension`, `sqlite3_enable_load_extension`, `sqlite3_config`, `sqlite3_db_config`, callback registration (`sqlite3_set_authorizer`, `sqlite3_create_function`, trace hooks), `sqlite3_open` / `sqlite3_open16`, backup/blob/VFS APIs, and `sqlite3_get_table`. SQL-level `ATTACH`, `DETACH`, and `load_extension()` are blocked by the connection authorizer.

Because SQLite `TEXT` values can contain embedded NUL bytes that IDL `str` parameters forbid, `bind_text` and `bind_blob` accept `bytes` and bind with `SQLITE_TRANSIENT` so SQLite copies the bytes before the request ring slot is released.

### Error mapping

Functions annotated with `@error(status, ...)` return `*rt.LibError` on failure, where `Code` carries the **extended** SQLite result code (enabled on every connection at open time) and `Detail` carries `sqlite3_errmsg(db)`. Service-specific validation failures use synthetic constants declared in the IDL (`SVC_SQL_EMPTY`, `SVC_SQL_MULTIPLE`, `SVC_BAD_ARGS`). Pre-call interceptor refusals return `*rt.StatusError` (`rt.ErrDenied` for path-policy violations, `0x0100 + SQLITE_CANTOPEN` when the parent directory does not exist, `SVC_STATUS_NAME_TOO_LONG`, and `SVC_STATUS_META_TOO_LARGE`).

## Key design decisions

Five architectural choices shape how `sqlite3.idl` and `sqlite3_hooks.c` map SQLite's C API onto HQQ sessions:

### 1. `@serialize(none)` for concurrent multi-session execution

When compiled with `SQLITE_THREADSAFE=1` (Multi-thread or Serialized mode), SQLite safely allows multiple threads to use distinct `sqlite3*` connections concurrently. Because each `hqq-bindgen` session runs on its own dedicated server thread that exclusively owns its `Db` and `Stmt` handles, `sqlite3.idl` specifies `@serialize(none)` so sessions execute in parallel across cores without a global server mutex.

Using `@serialize(session)` (which acquires a server-wide lock around every C call unless overridden) would cause a lock-inversion deadlock under contention: a session sleeping inside `sqlite3_step`'s busy handler while waiting for another session to `COMMIT` would hold the very server lock that the committing session needs to run its `COMMIT`. `TestBusyWithTwoSessions` verifies that session B waiting on a 20 s `busy_timeout` succeeds in ~300 ms once session A commits.

### 2. Batched row fetching (`fetch_rows` and `column_chunk`)

```idl
struct Rows { rc: i32; ncols: u32; cells: Value[]; errmsg: str<=1024; }
shim fn fetch_rows(stmt: Stmt, max: i32) -> Rows @max_out(65536);
```

- **Row-major `Value[]` encoding**: `Value` is a tagged union covering SQLite's five storage classes (`Int`, `Float`, `Text`, `Blob`, `Null`) plus `TextRaw` (for `TEXT` containing NUL bytes) and oversized cell markers (`BigText`, `BigBlob`). The number of returned rows is `len(cells) / ncols`.
- **Terminal status (`rc` and `errmsg`)**: `rc` is `SQLITE_ROW` when the batch filled before reaching the end of the result set, `SQLITE_DONE` when the query finished, or the extended error code if a step failed mid-batch (preserving all rows stepped prior to the error).
- **64 KiB response bound (`@max_out(65536)`, `rf = 5`)**: under the default session geometry (`S = 16384`, `CHUNK = 16368`), a 65,536-byte response bound requires $\lceil 65536 / 16368 \rceil = 5$ frames, permitting $\lfloor 64 / 5 \rfloor = 12$ pipelined calls in flight (`spec/toolchain/BINDGEN.md` §4.12). A typical 70-byte row fits 256 rows into ~18 KiB (`TestFetchTypicalBatchFits`).
- **Lookahead-free space reservation**: before calling `sqlite3_step` for the next row, `fetch_rows` verifies that at least `ncols * 24 + 1040` bytes remain in the 64 KiB response buffer (enough to encode a full row of markers plus the `errmsg` trailer). It never steps a row that it cannot serialize.
- **Oversized cells (`BigText` / `BigBlob` + `column_chunk`)**: when a `TEXT` or `BLOB` value does not fit inline alongside the remaining column markers of the current row, `fetch_rows` writes a `BigText` or `BigBlob` marker recording the total byte length, ends the batch on that row, and leaves the `sqlite3_stmt` positioned on that row. The client then reads the payload in chunks of up to 1 MiB using `column_chunk(stmt, col, offset, dst)` (which is idempotent and pipelineable; `TestPipelinedChunkReads`) before issuing the next `fetch_rows`.
- **Prefetch rule**: because `fetch_rows` advances the statement cursor, a client must decode the current batch and finish any `column_chunk` reads before issuing the next `fetch_rows` (`TestFetchPrefetch` vs `TestPipelinedFetchStepsOverBigRow`).

### 3. Single-call atomic parameter binding (`bind_values` and `exec_stmt`)

Because validation rule V38 forbids arrays of unions (`Value[]`) as input parameters, bulk parameter binding uses three parallel primitive arrays permitted by V38: `bind_values(stmt, kinds: u8[], words: u64[], data: bytes)`, where `kinds[i]` selects the storage class, `words[i]` holds the integer value, IEEE-754 float bits, or byte length, and `data` concatenates all text and blob bytes. `exec_stmt` combines `reset`, `bind_values`, `step`-to-completion, `changes64`, and `last_insert_rowid` into a single atomic round trip.

### 4. Single-statement enforcement (`prepare_one`)

`prepare_one` wraps `sqlite3_prepare_v2` and enforces the single-statement rule from [`sqlite/PROTOCOL.md`](../../../sqlite/PROTOCOL.md) §6.2: trailing whitespace, SQL comments, and a single semicolon are permitted, while a second SQL statement fails with `SVC_SQL_MULTIPLE` (finalizing the prepared handle immediately so no statement leaks) and an empty string fails with `SVC_SQL_EMPTY`. Raw `sqlite3_prepare_v2` is also exposed for callers that intentionally step through multi-statement scripts via the returned `tail` offset.

### 5. Connection open policy and hardening (`sqlite3srv_open_v2`)

`sqlite3_intercept_sqlite3_open_v2` runs on private copies of the request arguments before calling SQLite (`spec/toolchain/BINDGEN.md` §7.2–§7.3). It validates open flags via `sqlsrv_valid_open_flags`, resolves database aliases or `--allow-dir` relative paths via `sqlsrv_policy_resolve` (rejecting `..` traversal, out-of-tree paths, and symlinks), requires `zvfs == NULL`, and rewrites `filename` to the canonical resolved path.

Post-open connection hardening (enabling extended result codes, setting `sqlite3_limit` bounds on SQL length, bind variables, columns, and attached databases, disabling `SQLITE_DBCONFIG_ENABLE_LOAD_EXTENSION`, and registering the authorizer that denies `ATTACH` and `DETACH`) runs inside `sqlite3srv_open_v2` before any connection handle is returned to the session table.

## Policy verification

`server/CMakeLists.txt` compiles `sqlsrv_pathrules.c`, `sqlsrv_policy.c`, `sqlsrv_proto.c`, and `sqlsrv_port_{posix,win}.c` into `sqlite3gen_policy` and runs the C unit tests in `server/tests/test_units.c` (`hqqsqlite_server_unit_tests`). Every security refusal is tested both in C and end-to-end from Go:

| Policy Rule | C & Driver Suite (`sqlite/e2e`) | Low-Level Client Suite (`bindgen/examples/sqlite3`) |
| :--- | :--- | :--- |
| Reject `../x`, `a/../../x`, `..`, and paths outside `--allow-dir` | `TestPathPolicy`; `test_policy`, `test_path_rules` | `TestPathPolicy` (`rt.ErrDenied`) |
| Missing parent directory returns `CANTOPEN` (`0x010E`), not `DENIED` | `test_policy` | `TestPathPolicy` (status `0x010E`) |
| Reject symlinks and dangling links escaping `--allow-dir` | `TestPathPolicySymlinks`; `test_policy` | `TestPathPolicySymlinks` |
| Without `--allow-dir`, allow only registered `--alias` names | `TestAliasOnlyPolicy` | `TestAliasOnlyPolicy`, `TestAliasAndAllowDir` |
| Open flags restricted to `READONLY`, `READWRITE`, and optional `CREATE` | `test_validation` | `TestOpenFlags` |
| Reject URI filenames and `:memory:` databases | `test_validation` | `TestNoURIAndNoMemory` |
| Missing file without `CREATE` returns `CANTOPEN` | `PROTOCOL.md` §6.1 | `TestOpenMissingFile` |
| Block `ATTACH`, `DETACH`, and `load_extension()` | `PROTOCOL.md` §12 | `TestAttachAndExtensions` |
| Enforce 2,000 columns, 32,766 parameters, and 1 MiB SQL length | `PROTOCOL.md` §11 | `TestSqlAndColumnLimits` |
| Invalid CLI flags exit with status `2` | `TestStartupRejectsBadArguments` | `TestStartupRejectsBadArguments` |
| Enforce `--max-sessions` limit | `TestSessionLimit` | `TestSessionLimit` |
| Read-only connections reject writes | `PROTOCOL.md` §6.1 | `TestOpenReadOnly` |

## Test suite coverage

Every test launches an isolated `sqlite3_server` instance verified via `launch.Start`:

| Test Group | What it verifies |
| :--- | :--- |
| `TestValueRoundTrip`, `TestBindPathsAgree` | Tagged `Value` union alignment, embedded NULs (`TextRaw`), empty strings vs `NULL`, `NaN`/`-0.0`/`Inf`, `int64` extremes, and equivalence across individual and bulk bind calls. |
| `TestLargeValues`, `TestBigCellsInManyRows`, `TestBigRowAndBatchLimit`, `TestBindLimits` | Multi-frame `CONT` transfers up to 7 MiB, the 64 KiB inline-to-`BigText`/`BigBlob` transition, `column_chunk` offset reads, and `SVC_MAX_BIND_BYTES` (`16,776,960 B`) enforcement. |
| `TestFetchBatching`, `TestFetchTypicalBatchFits`, `TestFetchByteBound`, `TestFetchMaxClamp`, `TestFetchErrorAfterRows`, `TestFetchAfterDoneRestarts`, `TestFetchStatementWithoutColumns`, `TestFetchPrefetch`, `TestPipelinedFetchStepsOverBigRow` | Batch boundaries (`0, 1, 255, 256, 257, 512, 1000` rows), 64 KiB byte-bound early stop, partial rows preserved before a step error, safe prefetching, and the speculative-fetch hazard on big rows. |
| `TestConstraintErrors`, `TestSyntaxAndMissingTable`, `TestBindErrors`, `TestBindValuesClearsBindings`, `TestExecStmtResults` | Extended error codes and messages, atomic failure on bad bind arrays, and `changes64` / `last_insert_rowid` accuracy. |
| `TestSingleStatementRule`, `TestRawPrepareTail` | `SVC_SQL_EMPTY` and `SVC_SQL_MULTIPLE` rejection without leaked statement handles, plus raw `sqlite3_prepare_v2` script iteration. |
| `TestBusyWithTwoSessions` | Concurrent sessions with `@serialize(none)` resolving lock contention within `busy_timeout`. |
| `TestBadHandles`, `TestCloseConnectionWithLiveStatements`, `TestCloseRetainOnFail` | Stale, foreign, wrong-type, and nil handle rejection; automatic child statement finalization on connection close; and `@retain_on_fail` preservation when `sqlite3_close` returns `SQLITE_BUSY`. |
| `TestClientKilledMidTransaction`, `TestClientProcessKilledMidTransaction`, `TestCloseTearsDownBeforeTheAck` | Automatic transaction rollback and handle teardown on link close or client `SIGKILL`. |
| `TestServerKilled`, `TestGracefulShutdown` | Client error reporting on server `SIGKILL`, subsequent server recovery, and clean `SIGTERM` shutdown. |
| `TestSessionLimit`, `TestParallelSessions`, `TestDialBurstBeyondControlLinks` | `--max-sessions` enforcement, 16 concurrent writers + 4 readers under WAL, and `--max-control-links` burst handling. |
| `TestPipelinedBindsAndStep`, `TestPipelinedBindFailureDoesNotAbortStep`, `TestPipelinedChunkReads`, `TestSmallGeometry` | Credit flow control across 25 pipelined calls per row, pipelined `column_chunk` reads, and small ring geometries (`B=2, S=256`). |
| `TestAttachAndExtensions`, `TestDefaultDeny`, `TestGeneratedFilesAreFresh` | Authorizer enforcement, opcode allowlist, and up-to-date generated code. |

## License

The IDL, generated bindings, and C hooks in this directory are licensed under the repository's main license ([`LICENSE`](../../../LICENSE)). SQLite is in the public domain (<https://www.sqlite.org/copyright.html>) and is fetched and verified at build time by `scripts/release/fetch-sqlite.sh` from the digests pinned in `third_party/sqlite/`.
