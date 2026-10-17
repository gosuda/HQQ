// Package launcher starts the HQQ SQLite server (examples/sqlite/server) as a
// supervised child process and returns a database/sql handle to it.
//
// Start verifies the server binary with secureexec before it runs: the expected
// digest comes from Config.SHA256 or from a signed release manifest
// (secureexec/manifest) whose signature is checked first. The server is started
// with a parent lifeline (--parent-stdin), so it shuts down gracefully when this
// process exits, including when it is killed.
//
// Open and OpenManaged combine Start with sqlite.NewConnector.
//
// See README.md for the threat model, the lifecycle guarantees and limits, and
// how to obtain the binary and the manifest from a release.
package launcher
