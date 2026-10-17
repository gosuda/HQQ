# sqlite3: SQLite through hqq-bindgen

This directory rebuilds a SQLite service on top of the generator: a Go client and a C server that
`hqq-bindgen` generates from [`sqlite3.idl`](sqlite3.idl), with a few hand-written pieces (an interceptor,
shims, `main.c`) that make the C API fit a `database/sql` driver. Go calls SQLite over HQQ shared memory
without cgo, and a crash of SQLite cannot take the Go process down.

It is a **second implementation next to the hand-written SQLite service** (`sqlite/`,
[`sqlite/PROTOCOL.md`](../../../sqlite/PROTOCOL.md), `examples/sqlite/server/`). Nothing of that service was
changed or removed. The two share one thing on purpose: the database path and open policy, which this
server compiles from the hand-written server's own C files (see "The policy"). The `database/sql` driver on
top of this client is built elsewhere; this directory ends at the generated client and its tests.

| Path | What it is |
| --- | --- |
| `sqlite3.idl` | the interface: handles, the `Value` union, the functions, the shims |
| `sqlite3_client.go` | the generated Go client (package `sqlite3`) |
| `server/sqlite3_server.{h,c}` | the generated C glue |
| `server/sqlite3_hooks.c` | hand-written: the `sqlite3_open_v2` interceptor, the shims, the per-connection setup |
| `server/main.c`, `server/sqlite3srv.h` | hand-written process entry over `hqqrpc_main` |
| `server/CMakeLists.txt` | builds the server from the pinned SQLite amalgamation and the policy sources |
| `build.sh` | fetches the pinned SQLite, builds the server, prints `HQQ_SQLITE3GEN_SERVER` |
| `sqlite3srv/` | test support that other packages can import: finds or builds the server, starts it, dials it |
| `generate.go` | the `go:generate` line |
| `*_test.go` | the tests and benchmarks (unix only, like the server) |

## Build and run

```sh
eval "$(bindgen/examples/sqlite3/build.sh)"     # fetches the pinned SQLite (cached in $TMPDIR), builds, exports
go test -race -count=1 ./bindgen/examples/sqlite3/...
```

`build.sh -s address+undefined` also builds the server with ASan and UBSan and exports that one. `-T` adds
the test-only hooks that one test needs (below). Without `HQQ_SQLITE3GEN_SERVER`, the tests build the
server once per run (with `-T`) and skip with the reason when `cmake`, `ninja`, `unzip` or the network are
missing, and with `-short`. `HQQ_SQLITE3GEN_SANITIZE=address+undefined` runs the whole suite against the
sanitized build. `HQQ_SQLITE_AMALGAMATION_DIR` uses an unpacked amalgamation instead of downloading one.

Regenerate after a change to the IDL: `go generate ./bindgen/examples/sqlite3/...`
(`TestGeneratedFilesAreFresh` runs the same line with `-check`).

The server:

```text
sqlite3_server <listener> [--allow-dir DIR] [--alias NAME=PATH]... [--max-sessions N]
               [any flag of hqqrpc_main: --perm --max-handles --max-message --buffer-size --buffer-count
                --heartbeat-ms --peer-timeout-ms --shutdown-timeout-ms --parent-stdin[=force] ...]
```

It prints `ready <listener>` when it listens, and one line per session on stderr
(`sqlite3_server: session started` / `session ended`; the end line is printed after the session's
connections were closed and its transaction rolled back, which the tests wait for).

### Differences from `hqq-sqlite-server`

| | hqq-sqlite-server | sqlite3_server |
| --- | --- | --- |
| `--alias`, `--allow-dir` | yes | same functions, same meaning (`sqlsrv_policy.c`) |
| `--max-sessions` | default 256, refused at `Open` with `LIMIT` | default 256, refused at **`Dial`** with `*rt.OpenError` code 3 (matches `rt.ErrLimit`) |
| statement and cursor limits | 256 statements, 64 cursors per session | none of those: one handle table per session, `--max-handles`, default 4096 |
| listener | an Advanced-link listener | one control link per connecting client, at most `--max-control-links` at a time (default **256** here; the runtime default is 16, which a burst of 20 dials exceeds, see "Findings") |
| flags only here | | `--max-handles --max-message --buffer-size --buffer-count --max-in-flight --max-control-links --max-segment-bytes --open-timeout-ms --write-stall-ms --idle-timeout-ms --copy-request --spin` |
| flags of the hand-written server | `--alias --allow-dir --max-sessions --perm --heartbeat-ms --peer-timeout-ms --shutdown-timeout-ms --parent-stdin[=force] --exit-on-stdin-eof --lifeline-grace-ms` | all accepted (the first two by this `main.c`, the rest by `hqqrpc_main`) |
| `Open` pragmas | validated list in the `Open` request | none: the client runs `PRAGMA ...` as SQL |
| SQLite | system SQLite or the amalgamation | the pinned amalgamation only, built with `SQLITE_THREADSAFE=1`, `SQLITE_OMIT_LOAD_EXTENSION`, `SQLITE_DEFAULT_MEMSTATUS=0` (the system SQLite of macOS lacks `sqlite3_auto_extension`) |
| SQL text above 1 MiB, name above 4,096 bytes | `LIMIT` | the codec refuses before the call: `rt.ErrInvalidArg` at the client |

## What is exposed

The IDL is the allowlist: a function that is not declared has no opcode, so no client can call it
(`TestDefaultDeny` sends operations that are not in the interface and gets `E_UNKNOWN_OP`).

| Group | Functions |
| --- | --- |
| Connection | `sqlite3_open_v2` (interceptor), `sqlite3_close` (destroy of `Db`, `@retain_on_fail`), `sqlite3_busy_timeout`, `sqlite3_changes64`, `sqlite3_last_insert_rowid`, `sqlite3_get_autocommit`, `sqlite3_errcode`, `sqlite3_extended_errcode`, `sqlite3_errmsg`, `sqlite3_libversion`, `sqlite3_libversion_number` |
| Statement | `sqlite3_prepare_v2` (raw, with the tail), `prepare_one` (shim: exactly one statement), `sqlite3_step`, `sqlite3_reset`, `sqlite3_finalize` (destroy of `Stmt`), `sqlite3_clear_bindings`, `sqlite3_stmt_readonly` |
| Parameters | `sqlite3_bind_parameter_count/_index/_name`, `sqlite3_bind_int64/_double/_null/_zeroblob64`, `bind_text`, `bind_blob`, `bind_values` (shims) |
| One-call composites | `exec_stmt` (reset, bind, step to the end, changes, rowid; atomic), `stmt_info` (parameter names, columns, read-only), `fetch_rows`, `column_chunk` (shims) |

Not exposed, deliberately: `sqlite3_exec`, `sqlite3_load_extension`, `sqlite3_enable_load_extension`,
`sqlite3_config`, `sqlite3_db_config`, `sqlite3_set_authorizer`, `sqlite3_create_function` and every other
callback registration, `sqlite3_open`/`sqlite3_open16`, `sqlite3_interrupt`, backup, blob I/O, VFS and file
control, `sqlite3_serialize`, tracing, `sqlite3_get_table`. SQL-level `ATTACH`, `DETACH` and
`load_extension()` are blocked inside SQLite (below).

`bind_text`, `bind_blob` take `bytes`, not `str`: SQLite text may contain NUL bytes, which a wire `str` cannot
carry. They bind with `SQLITE_TRANSIENT` (a copy), because a request slot is recycled after the call; the
destructor argument of `sqlite3_bind_text64` is not expressible (callbacks are out of scope).

### Error convention

`@error(status, ...)` functions fail with a `*rt.LibError` whose `Code` is the **extended** result code (the
server turns on extended result codes on every connection; `sqlite3_errcode` is therefore extended too) and
whose `Detail` is `sqlite3_errmsg` of the connection. Codes that SQLite never returns are consts of the
IDL: `SVC_SQL_EMPTY`, `SVC_SQL_MULTIPLE` (the `BAD_REQUEST`s of PROTOCOL.md 6.2) and `SVC_BAD_ARGS`.
A refusal before SQLite is called is a `*rt.StatusError`: `ErrDenied` (open policy), status
`0x0100 + SQLITE_CANTOPEN` (the parent directory of the database does not exist, from the shared policy),
`SVC_STATUS_NAME_TOO_LONG`, `SVC_STATUS_META_TOO_LARGE`.

## Design decisions

### `@serialize(none)`

SQLite built with `SQLITE_THREADSAFE=1` is safe when each connection is used by one thread at a time, and a
session is one server thread that owns its connections and statements; the shims keep no shared state (the
message of a failed shim is thread-local; the policy is read-only after start). So the library may be
entered by every session at once, and sessions run on all cores.

`@serialize(session)` would not only be slower, it would be wrong: it holds one lock around every library
call of every session, so a session that waits inside `sqlite3_step` for its busy timeout holds the lock that the session it
waits for needs to run its `COMMIT`; the busy timeout could only ever expire. `TestBusyWithTwoSessions`
has session B wait 20 s for a lock that session A releases after 300 ms and checks that B's write succeeds
after about 300 ms.

### `fetch_rows`: layout, bound, geometry

```idl
struct Rows { rc: i32; ncols: u32; cells: Value[]; errmsg: str<=1024; }
shim fn fetch_rows(stmt: Stmt, max: i32) -> Rows @max_out(65536);
```

* **Cells.** `Value` is the union of the SQLite fundamental types (Int, Float, Text, Blob, Null) plus
  `TextRaw` (a TEXT with a NUL byte), `BigText` and `BigBlob` (below). Rows are row-major; `len(cells) / ncols`
  is the row count.
* **`rc` and `errmsg`.** `rc` is `SQLITE_ROW` (the batch stopped, more may follow: the shim never steps
  ahead to look), `SQLITE_DONE`, or the extended error code of the failing step. Rows read before the
  failure are delivered with it (the hand-written server drops the rows of the failing batch). `errmsg` is
  written after the cells because the failure comes after the rows.
* **Bound 65,536, `rf = 5`.** At the default geometry (`S = 16384`, `CHUNK = 16368`) a bound of 65,536 needs
  `ceil(65536 / 16368) = 5` frames, so `floor(64 / 5) = 12` calls can be in flight (BINDGEN.md 4.12). A
  typical row (an integer, a text of about 25 bytes, a float) is about 70 bytes on the wire, so a batch of 256
  rows is 15 to 18 KiB: one `fetch_rows` call with 3.5 times the room (`TestFetchTypicalBatchFits`). Rows of
  up to about 250 bytes still make a full batch of 256. A bound of 1 MiB (the `MaxBatchBytes` of the
  hand-written protocol) would be `rf = 65 > 64`: every call synchronous, and a megabyte of scratch per call.
  A bound of 16 KiB (`rf = 1`, written in place in the response slot) cannot hold a row of 2,000 columns
  (the limit this service keeps), whose worst case the shim reserves before it steps. At `S = 65600`
  the same bound is `rf = 1` **[not measured]**.
* **The shim never steps a row it cannot write.** Before each step it checks that `ncols * 24 + 1040` bytes
  are left (a row of markers plus the epilogue); a batch therefore ends early on fat rows
  (`TestFetchByteBound`) and a result of 2,000 columns is one row per batch
  (`TestSqlAndColumnLimits`). There is no state between calls: a row that was stepped and could not be
  written would be lost.
* **Cells that do not fit.** A TEXT or BLOB cell is written inline while it fits next to the markers of the
  cells that follow. Otherwise the cell is a `BigText` / `BigBlob` marker that carries the byte length, the
  row is the last of the batch, and the statement stays on it. `column_chunk(stmt, col, offset, dst)` then
  reads the value in pieces of up to 1 MiB (`cap` is the length of `dst`; the response bound is
  `cap + 19`, so the client picks `rf`). Reads are idempotent and may be pipelined (`TestPipelinedChunkReads`).
  Values of 3 MiB and 7 MiB round-trip in the tests; a request carries at most
  `SVC_MAX_BIND_BYTES = 16,776,960` bytes of text and blob (the 16 MiB `max_message`).
* **The rule that comes with it.** Do not issue `fetch_rows` ahead of a response that you have not read:
  if that response ends in a marker, the speculative call steps the statement past the row whose cell
  you still have to read, and `column_chunk` then reads another row's cell with no error
  (`TestPipelinedFetchStepsOverBigRow` demonstrates exactly this). Issuing the next `fetch_rows` after the
  previous batch was decoded and its markers read is safe and still overlaps the server's step with the
  client's use of the rows (`TestFetchPrefetch`). After `SQLITE_DONE` or an error call `sqlite3_reset`; a
  further step would run the statement again (that is SQLite's rule).

### Binding, V38 and the cost of per-parameter binds

A `Value[]` as an in-parameter is a composite in-parameter and **V38 forbids it**, so a tagged-value array
cannot be passed to a shim. A bulk bind **is** expressible with arrays of fixed-size elements, which V38
allows: `bind_values(stmt, kinds: u8[], words: u64[], data: bytes)`: `kinds[i]` is the type, `words[i]`
the integer, the float's bit pattern or the byte length, `data` the concatenated text and blob bytes. The
shim checks the three arrays against each other before it touches the statement (`SVC_BAD_ARGS`), resets,
clears and binds positions 1..n. `exec_stmt` takes the same arrays, binds, steps to the end, and returns
`changes` and `last_rowid`: **one call, atomic**.

The alternative, per-parameter binds in flight together with the step (`sqlite3_bind_int64Async`, ...,
`sqlite3_stepAsync`), works (`TestPipelinedBindsAndStep`: 25 calls per row, more than the 64 ring slots) but
has a cost beyond the extra requests: the server executes every request that was committed, so **a bind that
fails does not stop the step behind it**, and the statement runs with the parameter unbound
(`TestPipelinedBindFailureDoesNotAbortStep`). A driver that pipelines per-parameter binds has to wait for
the binds before it issues the step. The generator has no dependent calls (BINDGEN.md 4.13, reserved), so
this cannot be expressed in the interface. The benchmarks (`bench_test.go`: `BenchmarkInsertEightParams`)
compare the three ways; no number is recorded here.

### The single-statement rule

`prepare_one` is `sqlite3_prepare_v2` followed by the check of PROTOCOL.md 6.2, with the algorithm of
`sqlsrv_session.c`: what follows the first statement may be white space, comments and `;`; anything else is
`SVC_SQL_MULTIPLE` and no statement at all is `SVC_SQL_EMPTY`. Both leave no statement behind
(`TestSingleStatementRule` runs the failures more often than the handle table has entries and then closes the
connection, which SQLite answers with `SQLITE_BUSY` if a statement leaked). An interceptor cannot do this
(it runs before the call and has no tail), so it is a shim. The raw `sqlite3_prepare_v2` is also
declared and does not enforce the rule: it is how a client runs a script, statement by statement, from the
`tail` offset.

### Open: the interceptor and the connection setup

`sqlite3_intercept_sqlite3_open_v2` runs on private copies of the arguments (BINDGEN.md 7.2, 7.3) and does
what `do_open` of the hand-written server does before it calls SQLite, in the same order: the flags
(`sqlsrv_valid_open_flags`), the name (`sqlsrv_policy_resolve`: an alias, or a path relative to the allow
directory, with `..`, absolute paths outside, symbolic links and missing parents handled by the shared
code), and then it hands the canonical path to SQLite by replacing the `filename` argument. It adds one
refusal that the hand-written protocol has no field for: the `zvfs` argument must be NULL.

What `do_open` does **after** `sqlite3_open_v2` (extended result codes, `sqlite3_limit` for SQL length,
variables, columns and attached databases, extension loading off, an authorizer that denies `ATTACH` and
`DETACH`) cannot be a post-call hook, which the interceptor contract does not have (BINDGEN.md 7.3, 12.2).
It is `conn_setup`, registered with `sqlite3_auto_extension`, which SQLite runs inside `sqlite3_open_v2`
for every connection before it returns, so no connection is ever visible to a client without it.
`sqlite3_busy_timeout` is a call of the service. Pragmas are SQL.

## The policy, shared with the hand-written server

`server/CMakeLists.txt` compiles `sqlsrv_pathrules.c`, `sqlsrv_policy.c`, `sqlsrv_proto.c` and
`sqlsrv_port_posix.c` from `examples/sqlite/server/src` through a relative path; nothing is copied
(`TestPolicyIsNotForked`). A change to the hand-written policy is a change here. The refusals and the
tests that mirror them:

| Refusal | Hand-written source of the case | Test here |
| --- | --- | --- |
| `../x`, `a/../../x`, `..`, an absolute path outside the allow directory | `TestPathPolicy` (`sqlite/e2e/e2e_test.go`); `test_policy`, `test_path_rules` (`tests/test_units.c`) | `TestPathPolicy` (`E_DENIED`) |
| a missing parent directory is `CANTOPEN`, not an escape | `test_policy` | `TestPathPolicy` (status `0x010E`) |
| a symbolic link or a dangling link out of the directory | `TestPathPolicySymlinks`; `test_policy` | `TestPathPolicySymlinks`; a link inside is followed |
| only aliases when there is no `--allow-dir`; every raw path refused | `TestAliasOnlyPolicy` | `TestAliasOnlyPolicy`; `TestAliasAndAllowDir` (alias wins; may point outside) |
| open flags: exactly one of READONLY, READWRITE, optional CREATE | `test_validation` | `TestOpenFlags` (the same accepted and refused sets, plus URI, MEMORY, mutex, cache, NOFOLLOW, EXRESCODE, negative) |
| no URI, no in-memory database | the comment "No SQLITE_OPEN_URI" in `do_open` | `TestNoURIAndNoMemory` |
| `CANTOPEN` for a missing file without CREATE | PROTOCOL.md 6.1 | `TestOpenMissingFile` |
| `ATTACH`, `DETACH` denied; no extension loading | PROTOCOL.md 12 | `TestAttachAndExtensions` |
| limits: 2,000 columns, 32,766 variables, 1 MiB SQL | PROTOCOL.md 11 | `TestSqlAndColumnLimits` |
| bad command line exits with 2 | `TestStartupRejectsBadArguments` | `TestStartupRejectsBadArguments` |
| `--max-sessions` | `TestSessionLimit` | `TestSessionLimit` |
| a read-only open cannot write | PROTOCOL.md 6.1 | `TestOpenReadOnly` |

The Windows rules of `sqlsrv_pathrules.c` are compiled but not reachable here: the server is POSIX only.

## Tests and what each catches

All tests start their own server (`sqlite3srv.Start`, through `launch.Start` with the SHA-256 of the
binary) and use only the generated client.

| Test | Catches |
| --- | --- |
| `TestValueRoundTrip`, `TestBindPathsAgree` | a wrong tag or alignment of a `Value`, a lost NUL, text returned as blob, `''` turned into NULL, NaN, -0.0, Inf, int64 limits; a bind path that disagrees with another |
| `TestLargeValues`, `TestBigCellsInManyRows`, `TestBigRowAndBatchLimit`, `TestBindLimits` | CONT frames in both directions (up to 7 MiB), the inline/marker switch around 64 KiB, a batch that steps over a big row, a chunk read at the wrong position, the largest bind and the refusal above it |
| `TestFetchBatching`, `TestFetchTypicalBatchFits`, `TestFetchByteBound`, `TestFetchMaxClamp`, `TestFetchErrorAfterRows`, `TestFetchAfterDoneRestarts`, `TestFetchStatementWithoutColumns`, `TestFetchPrefetch`, `TestPipelinedFetchStepsOverBigRow` | batch boundaries (0, 1, 255, 256, 257, 512, 1000 rows, several `max`), the byte bound, the clamp, rows before an error, prefetch order, the documented pipelining hazard |
| `TestConstraintErrors`, `TestSyntaxAndMissingTable`, `TestBindErrors`, `TestBindValuesClearsBindings`, `TestExecStmtResults` | extended codes and messages from the right connection, the failure of each bind path, no run after a refused bind, stale detail text |
| `TestSingleStatementRule`, `TestRawPrepareTail` | the rule of 6.2, leaks on the failing paths, the tail of the raw prepare |
| `TestBusyWithTwoSessions` | `SQLITE_BUSY`, and a busy wait that the other session ends: `@serialize(none)` |
| `TestBadHandles`, `TestCloseConnectionWithLiveStatements`, `TestCloseRetainOnFail` | stale, foreign, mistyped and nil handles; the cascade at close; `@retain_on_fail` (reached with the test hook) |
| `TestClientKilledMidTransaction`, `TestClientProcessKilledMidTransaction`, `TestCloseTearsDownBeforeTheAck` | session death (a closed link, a SIGKILLed process, an orderly close with live statements): rollback, finalize, close, and the ack after the teardown, observed by a second session |
| `TestServerKilled`, `TestGracefulShutdown` | a killed server (unknown outcome, recovery by the next server), a graceful stop |
| `TestSessionLimit`, `TestParallelSessions`, `TestDialBurstBeyondControlLinks` | `--max-sessions`; 16 writers and 4 readers on one file; the control-link refusal |
| `TestPipelinedBindsAndStep`, `TestPipelinedBindFailureDoesNotAbortStep`, `TestPipelinedChunkReads`, `TestSmallGeometry` | flow control with more calls than slots, the failed-bind hazard, pipelined chunk reads, geometries from `B=2, S=256` to `B=128, S=1024` (every bound above the ring is synchronous) |
| `TestAttachAndExtensions`, `TestDefaultDeny`, `TestPolicyIsNotForked`, `TestGeneratedFilesAreFresh` | the authorizer, the allowlist (unknown operations are `E_UNKNOWN_OP`), the shared policy, stale generated files |

`TestCloseRetainOnFail` needs a statement that SQLite knows and the session does not, because the server
finalizes the children of a connection before it closes it, so `sqlite3_close` never answers `BUSY` for an
honest sequence of calls. A test build (`build.sh -T`, `-DSQLITE3GEN_TEST_HOOKS=ON`) leaks one statement per
`prepare_one` when `SQLITE3GEN_TEST_LEAK_STMT` is set. A server built without `-T` skips that test.

Benchmarks (`bench_test.go`) are not run by `go test`; `go test -run '^$' -bench . -benchtime 1x` proves
that they run. No number is recorded in this repository.

## Findings and limits

* **No cancellation (D15).** `context` cancellation cannot interrupt a running `sqlite3_step`: a call that
  timed out is abandoned and its outcome is unknown. `sqlite3_interrupt` is not exposed (it would need a
  second thread inside the session; under one thread per session there is none), so `SQLITE_INTERRUPT` is
  unreachable and a client that closes mid-statement ends its session only after the statement returns. The
  hand-written service has Advanced cancel and `sqlite3_interrupt`.
* **Pipelining costs correctness unless the driver cooperates** (two hazards, above): `fetch_rows` ahead of an
  unread response, and binds in flight with a step.
* **Values above the batch bound take more calls**: a cell of 64 KiB or more is read with `column_chunk`
  after the batch that carried its marker; the hand-written protocol sends it in the batch.
* **Dial storms.** More simultaneous dials than control links are closed at once and the client sees
  `hqq.ErrPeerClosed` (or a connect timeout), not a status to retry on. The default of 16 is exceeded by 20
  parallel dials; this server defaults to 256 and `TestDialBurstBeyondControlLinks` documents the failure
  with `--max-control-links 1`. A connection pool has to retry `Dial` or size the server.
* **`SQLITE_OMIT_LOAD_EXTENSION` and the system SQLite.** The server needs the amalgamation (see the table of
  differences).
* **Not run here:** Linux, the `-short` path, a build against a SQLite other than 3.54.1.

## License

The generated files and the hand-written files of this directory are part of this repository and follow its
license. SQLite is in the public domain (<https://www.sqlite.org/copyright.html>); its source is not in the
repository. `scripts/release/fetch-sqlite.sh` downloads the pinned amalgamation (`third_party/sqlite/`) and
verifies SHA-256 and SHA3-256 before it unpacks anything. The path and open policy is the hand-written
server's own code, compiled from `examples/sqlite/server/src`.
