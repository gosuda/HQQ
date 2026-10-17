// Package rt is the Go runtime of hqq-bindgen: the client side of the wire
// protocol of spec/toolchain/BINDGEN.md section 4, over one HQQ Standard link
// per session.
//
// Generated clients call the exported API of section 9.3: Dial opens a
// session, BeginRequest and Submit write one request frame straight into a
// ring slot, Wait returns the response body in place, and Enc and Dec helpers
// encode and decode the values of section 4.5. The runtime starts no goroutine
// and a Conn is not safe for concurrent use; Kill is the only method that
// another goroutine may call.
package rt
