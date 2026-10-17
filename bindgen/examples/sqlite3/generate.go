// Package sqlite3 is the hqq-bindgen example of a database engine: the Go client of the subset of the
// SQLite C API that a database/sql driver needs, the C server built from the matching generated glue
// (server/), and the tests that drive the server over shared memory. See README.md.
//
// Regenerate the client and the server glue after a change to the IDL or to the generator:
//
//	go generate ./bindgen/examples/sqlite3/...
//
// TestGeneratedFilesAreFresh runs the same command with -check.
package sqlite3

//go:generate go run gosuda.org/hqq/cmd/hqq-bindgen -go-out . -c-out server sqlite3.idl
