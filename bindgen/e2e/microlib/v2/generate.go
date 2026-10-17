// Package microlibv2 is a Go client generated from microlib_v2.idl, whose ml_add differs from
// microlib.idl in the type of one parameter. Its interface digest differs, so connecting it to the
// server built from microlib.idl must fail with *rt.MismatchError (BINDGEN.md 10.4).
package microlibv2

//go:generate go run gosuda.org/hqq/cmd/hqq-bindgen -go-out . -go-package microlibv2 -go-strip ml_ ../../../testdata/microlib/microlib_v2.idl
