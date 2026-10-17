package lifeline

import (
	"context"
	"os"
)

// WatchOption configures Watch.
type WatchOption func(*watchConfig)

type watchConfig struct {
	terminal bool
}

// WatchTerminal watches standard input even when it is a terminal. Use it when
// a service must stop with the terminal that started it, for example under a
// debugger harness; by default a terminal is not watched.
func WatchTerminal() WatchOption {
	return func(c *watchConfig) { c.terminal = true }
}

// Watch starts reading standard input in the background, discarding every
// byte. The returned context is canceled when standard input reaches end of
// file or a read fails (for example, when the descriptor is closed). It is
// also canceled when parent is done, and when the returned stop function is
// called. Stop does not interrupt the background read; the read ends with the
// process.
//
// If standard input is a terminal, Watch does not watch it (see
// [WatchTerminal]): the returned context is then canceled only by parent or
// stop. Call Watch once per process; a second reader would split the input.
//
// Watch is for Go services. A non-Go service reads its standard input itself
// (the C server: --parent-stdin). Parent must not be nil.
func Watch(parent context.Context, opts ...WatchOption) (context.Context, func()) {
	var c watchConfig
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	ctx, cancel := context.WithCancel(parent)
	if !c.terminal && stdinIsTerminal(os.Stdin) {
		return ctx, cancel
	}
	go func() {
		defer cancel()
		drain(os.Stdin)
	}()
	return ctx, cancel
}

// drain reads r until it fails, discarding the data. A read error includes end
// of file.
func drain(r *os.File) {
	buf := make([]byte, 512)
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
	}
}
