// Package interop is the test-only driver of the HQQ interoperability matrix.
//
// The driver lists the scenarios each hqq-interop helper implements
// (conformance/INTEROP_CLI.md, section 7), runs every scenario that both
// helpers of a pair list, in both role assignments, and starts one helper
// process per role of each step. Each process prints one JSON result line,
// which the driver checks against the step's expectations.
//
// Environment:
//
//	HQQ_INTEROP_IMPLS  comma-separated label=path list, for example
//	                   "go=,c=build-c/tools/hqq-interop". Labels match [a-z0-9]+.
//	                   An empty path ("go=") or an unset variable builds the Go
//	                   helper from the module root into a temporary directory;
//	                   only the go label may have an empty path. Relative paths
//	                   are relative to the module root. A path that does not
//	                   exist fails the test.
//	HQQ_INTEROP_LONG   "1" passes --long to every helper (larger soak sizes).
//
// Adding an implementation:
//
//  1. Build an executable named hqq-interop (hqq-interop.exe on Windows) that
//     follows conformance/INTEROP_CLI.md: `list --impl LABEL` prints the
//     catalogue, `run` runs one role of one scenario and prints one result line.
//  2. Choose a label of [a-z0-9]+ and append label=path to HQQ_INTEROP_IMPLS.
//  3. Add the build to the C helper steps (or a new step) of
//     .github/workflows/interop.yml.
//
// Run the matrix with:
//
//	go test ./interop/... -count=1 -timeout 30m
package interop
