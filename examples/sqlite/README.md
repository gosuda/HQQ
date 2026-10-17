# SQLite service: C server

This directory holds the C server of the SQLite service, [`server/`](server/README.md)
(`hqq-sqlite-server`, POSIX only). The Go driver, the protocol, and the tests live in
[`../../sqlite`](../../sqlite/README.md) (`gosuda.org/hqq/sqlite`). The wire protocol is in
[`../../sqlite/PROTOCOL.md`](../../sqlite/PROTOCOL.md).

Build the server and run its unit tests from the repository root:

```sh
cmake -S c -B build -DHQQ_BUILD_SQLITE_SERVER=ON -DHQQ_WERROR=ON
cmake --build build --target hqq-sqlite-server hqqsqlite_server_unit_tests
ctest --test-dir build -R hqqsqlite_server --output-on-failure
```

The binary is `build/sqlite-server/hqq-sqlite-server`. Point `HQQ_SQLITE_SERVER` at it to run
the Go end-to-end tests in [`../../sqlite/e2e`](../../sqlite/e2e).
