# SQLite Service: C11 Server (`examples/sqlite`)

This directory contains the C11 SQLite server implementation in [`server/`](server/README.md) (`hqq-sqlite-server`, supporting Linux, macOS, and Windows). The pure-Go `database/sql` driver, wire codecs, contract suite, and end-to-end tests live in [`../../sqlite`](../../sqlite/README.md) (`gosuda.org/hqq/sqlite`), and the normative wire protocol specification lives in [`../../sqlite/PROTOCOL.md`](../../sqlite/PROTOCOL.md).

Build `hqq-sqlite-server` and run its C unit test suite from the repository root:

```sh
cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_WERROR=ON
cmake --build build --target hqq-sqlite-server hqqsqlite_server_unit_tests
ctest --test-dir build -R hqqsqlite_server --output-on-failure
```

The compiled server binary is written to `build/sqlite-server/hqq-sqlite-server`. Point `HQQ_SQLITE_SERVER` at that path to run the Go end-to-end test suites in [`../../sqlite/e2e`](../../sqlite/e2e) and [`../../sqlite/launcher`](../../sqlite/launcher/README.md).
