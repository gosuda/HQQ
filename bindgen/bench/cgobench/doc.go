// Package cgobench calls the functions of the MIT test library bindgen/testdata/microlib through
// cgo. It is the baseline of the benchmarks of spec/toolchain/BINDGEN.md 10.5: the generated client
// calls the same microlib.c over shared memory, this package calls it in process.
//
// cgo cannot be used in a _test.go file, so the wrappers live in cgobench.go, which carries the cgo
// build tag; without cgo this package is empty and CGO_ENABLED=0 go build ./... still works.
package cgobench
