// Package microlib is the end-to-end layer of hqq-bindgen (spec/toolchain/BINDGEN.md 10.4): the
// generated Go client of the MIT test library bindgen/testdata/microlib, the C server built from the
// matching generated glue (server/), and the tests that drive one against the other over real shared
// memory. The tests are in this directory; the test files are POSIX only, because the C server is.
//
// Regenerate the client and the server glue after a change to the IDL or to the generator:
//
//	go generate ./bindgen/e2e/microlib/...
//
// TestGeneratedFilesAreFresh runs the //go:generate line below with -check, so a stale committed file
// fails the test suite.
//
// Run the tests, with the race detector:
//
//	go test -race -count=1 ./bindgen/e2e/...
//
// The suite builds server/ with CMake and Ninja into a temporary directory (it skips with the reason
// when cmake, ninja or a C compiler is missing). HQQ_BINDGEN_MICROLIB_SERVER names a prebuilt
// microlib_server instead, and HQQ_BINDGEN_MICROLIB_SANITIZE=address+undefined builds and runs the
// whole suite against a server compiled with ASan and UBSan. The suite starts one server per test with
// launch.Start, verified against the SHA-256 of the binary, and removes everything it creates.
package microlib

//go:generate go run gosuda.org/hqq/cmd/hqq-bindgen -go-out . -go-strip ml_ -c-out server ../../testdata/microlib/microlib.idl
