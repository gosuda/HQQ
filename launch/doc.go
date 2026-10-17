// Package launch starts an HQQ server (a C program built on libhqq, such as a
// bindgen-generated server or hqq-sqlite-server) as a child process that the Go
// program can rely on, and does not restart or supervise it.
//
// Start verifies the server binary with secureexec before it runs: the expected
// SHA-256 digest comes from Config.SHA256 or from a signed release manifest
// (secureexec/manifest) whose signature is checked first. It then starts the
// server with a parent lifeline (lifeline), so the server shuts down gracefully
// when this process exits, including when it is killed, and waits for the ready
// line "ready <listener>" on stdout (ReadyPrefix). The Server it returns stops
// the child with a graceful signal first and kills it after StopTimeout.
//
// The package knows nothing about the server's own arguments: the caller puts
// the listener name and the server's options in Config.Args and Start's
// arguments. The command line is Config.Args, the lifeline flag
// (DefaultLifelineFlag), then Start's arguments.
//
// See README.md for the threat model, the lifecycle guarantees and limits.
package launch
