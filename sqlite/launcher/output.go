package launcher

import (
	"bytes"
	"io"
	"sync"
)

// tailSize is how much of the server's output is kept for error messages.
const tailSize = 2048

// output routes the server's stdout and stderr. It recognises the ready line on
// stdout (consuming it), forwards everything else to the caller's writers, and
// keeps the end of the output for error messages.
//
// The stdout state (ready, pend) is touched only by the goroutine that copies
// stdout, and by flush after cmd.Wait has returned, which happens after that
// copy ended. The shared fields are guarded by mu.
type output struct {
	want  []byte        // the ready line, with its newline
	ready chan struct{} // closed when the ready line was seen

	stdout, stderr io.Writer // caller's writers, may be nil
	pend           []byte    // unfinished stdout line that may still be the ready line
	seen           bool      // the ready line was seen

	wmu  sync.Mutex // serialises writes to the caller's writers
	mu   sync.Mutex // guards tail
	tail []byte
}

func newOutput(listener string, stdout, stderr io.Writer) *output {
	return &output{
		want:   []byte("ready " + listener + "\n"),
		ready:  make(chan struct{}),
		stdout: stdout,
		stderr: stderr,
	}
}

// outWriter is the io.Writer of one stream. Stdout scans for the ready line.
type outWriter struct {
	o      *output
	stdout bool
}

func (w outWriter) Write(p []byte) (int, error) {
	if w.stdout {
		w.o.scanStdout(p)
	} else {
		w.o.forward(w.o.stderr, p)
	}
	// Errors of the caller's writers are ignored: a broken log must not stop
	// the server (see Config.Stdout).
	return len(p), nil
}

// scanStdout looks for the ready line in p. It holds back only a partial line
// that is a prefix of the ready line, so other output is forwarded promptly.
func (o *output) scanStdout(p []byte) {
	if o.seen {
		o.forward(o.stdout, p)
		return
	}
	buf := append(o.pend, p...)
	o.pend = nil
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		line := buf[:i+1]
		if bytes.Equal(line, o.want) {
			o.seen = true
			close(o.ready)
			o.forward(o.stdout, buf[i+1:])
			return
		}
		o.forward(o.stdout, line)
		buf = buf[i+1:]
	}
	if len(buf) > 0 && len(buf) < len(o.want) && bytes.HasPrefix(o.want, buf) {
		o.pend = append([]byte(nil), buf...)
		return
	}
	o.forward(o.stdout, buf)
}

// flush forwards a held-back partial line. Call it only after the copy of
// stdout has ended.
func (o *output) flush() {
	if len(o.pend) > 0 {
		o.forward(o.stdout, o.pend)
		o.pend = nil
	}
}

// forward appends p to the tail and writes it to w, when w is set.
func (o *output) forward(w io.Writer, p []byte) {
	if len(p) == 0 {
		return
	}
	o.mu.Lock()
	o.tail = append(o.tail, p...)
	if len(o.tail) > tailSize {
		o.tail = append([]byte(nil), o.tail[len(o.tail)-tailSize:]...)
	}
	o.mu.Unlock()
	if w == nil {
		return
	}
	o.wmu.Lock()
	_, _ = w.Write(p)
	o.wmu.Unlock()
}

// tailString returns the kept end of the output.
func (o *output) tailString() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.tail)
}
