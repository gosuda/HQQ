// Package cgozstd calls Zstandard in process through cgo. The example tests use it as the reference
// that the server's results must equal byte for byte, and the benchmarks use it as the cgo side of
// the comparison with the generated client.
//
// The implementation is compiled only with the build tag zstdcgo and cgo enabled, so that a plain
// `go build ./...` needs no zstd headers:
//
//	eval "$(bindgen/examples/zstd/build.sh)"
//	go test -race -tags zstdcgo ./bindgen/examples/zstd/...
//
// build.sh exports CGO_CFLAGS and CGO_LDFLAGS for the same pinned source that the server is built
// from. Pointing them at another libzstd works as well; the tests then compare two releases and
// may report a byte difference that is not a defect (README.md, "Verification").
package cgozstd
