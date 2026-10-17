package lifeline

import "os"

// stdinIsTerminal decides whether Watch leaves standard input alone. It is a
// variable so that tests can force either answer.
//
// Without cgo or x/sys there is no isatty. The check is: a character device
// that is not the null device. On Unix this covers terminals and pseudo-
// terminals (and a few other character devices such as /dev/zero); on Windows it
// covers the console. Pipes, sockets and regular files are never terminals.
var stdinIsTerminal = isTerminal

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}
