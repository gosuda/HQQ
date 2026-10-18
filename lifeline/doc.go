// Package lifeline ties the life of a child process to its parent through the
// child's standard input.
//
// The parent creates an OS pipe, gives its read end to the child as standard
// input and keeps the write end. The child reads its standard input until end
// of file. End of file comes when every holder of the write end has closed it:
// the parent calls [Lifeline.Close], or the parent process dies for any reason,
// including SIGKILL, because the kernel closes its descriptors. The write end is
// close-on-exec (os.Pipe), so other children do not inherit it.
//
// Parent side (any Go program that starts a child):
//
//	cmd := exec.Command(server, "--parent-stdin")
//	ll, err := lifeline.Attach(cmd)     // sets cmd.Stdin
//	if err != nil { return err }
//	defer ll.Close()                    // ends the child's life when the parent returns
//	if err := cmd.Start(); err != nil { return err }
//
// Child side (a Go service). Call Watch once, early in main:
//
//	ctx, stop := lifeline.Watch(context.Background())
//	defer stop()
//	<-ctx.Done()                        // standard input reached end of file or failed
//
// The child does not need to be written in Go to use the protocol: it only
// needs to read standard input until end of file. The C server does this with
// libhqq's parent lifeline (--parent-stdin); see bindgen/examples/sqlite3/README.md.
//
// Keepalives. [WithKeepalive] makes the parent write one byte every interval.
// The bytes let a child that cares notice a wedged parent (a stopped process
// keeps the pipe open, so only a timeout on the child side can catch it). [Watch]
// discards every byte and only reacts to end of file or an error.
//
// Terminals. A terminal on standard input is not watched by default: closing a
// terminal would otherwise stop an interactive run. [WatchTerminal] forces the
// watch. A standard input that is a character device other than the null
// device counts as a terminal; the null device is watched, so a child started
// with /dev/null stops at once, as the C server does.
//
// Limits. The child must read its standard input for the whole of its life,
// otherwise a keepalive only fills the pipe. A grandchild that inherits the
// read end keeps the child's stdin open, and the child sees end of file only
// after that grandchild exits too. Watch cannot interrupt a blocked read, so
// its goroutine ends with the process; the returned stop function only cancels
// the context.
//
// The package uses only the standard library and works the same on Unix and
// Windows: os.Pipe handles are passed to the child by os/exec.
package lifeline
