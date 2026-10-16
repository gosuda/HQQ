package supervisor

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"sync"
)

// rotWriter appends to a file and rotates it when it would grow past max
// bytes: path becomes path.1, path.1 becomes path.2, and so on, keeping keep
// rotated files. It is safe for concurrent use.
type rotWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

func openRotWriter(path string, max int64, keep int) (*rotWriter, error) {
	w := &rotWriter{path: path, max: max, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f = f
	w.size = st.Size()
	return nil
}

// Write appends p, rotating first when p would exceed the size limit and the
// file is not empty.
func (w *rotWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size > 0 && w.size+int64(len(p)) > w.max {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotWriter) rotate() error {
	_ = w.f.Close()
	for i := w.keep - 1; i >= 1; i-- {
		_ = os.Rename(w.path+"."+strconv.Itoa(i), w.path+"."+strconv.Itoa(i+1))
	}
	if w.keep >= 1 {
		_ = os.Rename(w.path, w.path+".1")
	}
	if w.keep < 1 {
		_ = os.Remove(w.path)
	}
	return w.open()
}

// Close closes the file.
func (w *rotWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// prefixWriter writes complete lines to dst, each prefixed. A partial last
// line is kept until its newline arrives or Flush is called.
type prefixWriter struct {
	mu     sync.Mutex
	prefix []byte
	dst    io.Writer
	buf    []byte
}

func newPrefixWriter(prefix string, dst io.Writer) *prefixWriter {
	return &prefixWriter{prefix: []byte(prefix), dst: dst}
}

// Write splits p into lines and forwards each complete line.
func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i+1])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) == 0 {
		w.buf = nil
	}
	return len(p), nil
}

// Flush forwards a pending partial line, with a newline added.
func (w *prefixWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(append(w.buf, '\n'))
		w.buf = nil
	}
}

func (w *prefixWriter) emit(line []byte) {
	out := make([]byte, 0, len(w.prefix)+len(line))
	out = append(out, w.prefix...)
	out = append(out, line...)
	_, _ = w.dst.Write(out)
}

// prefixFor is the line prefix of a service in prefix mode.
func prefixFor(service string) string {
	return service + " | "
}
