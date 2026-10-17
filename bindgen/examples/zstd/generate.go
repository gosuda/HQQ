// Package zstd is the hqq-bindgen example of a real library: the Go client of a subset of Zstandard,
// the C server built from the matching generated glue (server/), and the tests and benchmarks that
// drive the server over shared memory and compare it with the same library called through cgo.
// See README.md.
package zstd

//go:generate go run gosuda.org/hqq/cmd/hqq-bindgen -go-out . -go-strip ZSTD_ -c-out server zstd.idl
