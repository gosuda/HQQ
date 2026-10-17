// Package bench holds the benchmarks of the generated client against cgo (spec/toolchain/BINDGEN.md
// 10.5). Everything is in the test files, which need cgo and a built bindgen/bench/server
// (README.md): the generated client in ../microbench talks to that C server over HQQ shared memory,
// ../cgobench calls the same microlib.c in process.
//
// The benchmarks verify every result, and bench_test.go and smoke_test.go run every operation once
// through both paths in a normal `go test`.
package bench
