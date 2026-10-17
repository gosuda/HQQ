//go:build unix

package hqq

import (
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// Comparison benchmarks: HQQ versus a Unix-domain stream socket pair and a
// pair of pipes. Both ends run in this process on separate goroutines, which
// measures the transport itself (copies, syscalls, wake-ups) with identical
// scheduling for all candidates.
//
//	go test -run '^$' -bench Compare -benchtime 2s

type duplexPair struct {
	a, b  io.ReadWriter
	close func()
}

func hqqPair(b *testing.B, opts LinkOptions, size int) duplexPair {
	const bufferCount = 256
	mem := AlignedBuffer(SizeStandardLink(bufferCount, size))
	x, err := NewStandardLink(mem, bufferCount, size, opts)
	if err != nil {
		b.Fatal(err)
	}
	y, err := NewStandardLink(mem, bufferCount, size, opts)
	if err != nil {
		b.Fatal(err)
	}
	return duplexPair{a: x, b: y, close: func() { y.Close(); x.Close() }}
}

func unixSocketPair(b *testing.B) duplexPair {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		b.Skipf("socketpair: %v", err)
	}
	conn := func(fd int) net.Conn {
		f := os.NewFile(uintptr(fd), "socketpair")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			b.Fatal(err)
		}
		return c
	}
	x, y := conn(fds[0]), conn(fds[1])
	return duplexPair{a: x, b: y, close: func() { x.Close(); y.Close() }}
}

type pipeEnd struct {
	r *os.File
	w *os.File
}

func (p pipeEnd) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p pipeEnd) Write(b []byte) (int, error) { return p.w.Write(b) }

func pipePair(b *testing.B) duplexPair {
	r1, w1, err := os.Pipe()
	if err != nil {
		b.Fatal(err)
	}
	r2, w2, err := os.Pipe()
	if err != nil {
		b.Fatal(err)
	}
	return duplexPair{
		a:     pipeEnd{r: r2, w: w1},
		b:     pipeEnd{r: r1, w: w2},
		close: func() { r1.Close(); w1.Close(); r2.Close(); w2.Close() },
	}
}

func comparePairs(b *testing.B, size int) map[string]func() duplexPair {
	return map[string]func() duplexPair{
		"hqq-adaptive": func() duplexPair { return hqqPair(b, LinkOptions{WaitPolicy: WaitAdaptive}, size) },
		"hqq-spin":     func() duplexPair { return hqqPair(b, LinkOptions{WaitPolicy: WaitSpin}, size) },
		"unix-socket":  func() duplexPair { return unixSocketPair(b) },
		"pipe":         func() duplexPair { return pipePair(b) },
	}
}

var compareOrder = []string{"hqq-adaptive", "hqq-spin", "unix-socket", "pipe"}

// BenchmarkCompareRoundTrip measures request/response latency (ns/op is one
// round trip) for small messages.
func BenchmarkCompareRoundTrip(b *testing.B) {
	const size = 64
	pairs := comparePairs(b, size)
	for _, name := range compareOrder {
		b.Run(name+"/64B", func(b *testing.B) {
			p := pairs[name]()
			defer p.close()
			done := make(chan error, 1)
			go func() {
				buf := make([]byte, size)
				for i := 0; i < b.N; i++ {
					if _, err := io.ReadFull(p.b, buf); err != nil {
						done <- err
						return
					}
					if _, err := p.b.Write(buf); err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			msg := make([]byte, size)
			buf := make([]byte, size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := p.a.Write(msg); err != nil {
					b.Fatal(err)
				}
				if _, err := io.ReadFull(p.a, buf); err != nil {
					b.Fatal(err)
				}
			}
			if err := <-done; err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkCompareRoundTripZeroCopy is BenchmarkCompareRoundTrip over the
// reservation API that a zero-copy request/response layer uses: the sender
// fills the slot-owned payload buffer in place and commits it, the receiver
// reads the payload in place and releases the slot. The echoing end holds
// the request while it writes the response.
func BenchmarkCompareRoundTripZeroCopy(b *testing.B) {
	const size = 64
	b.Run("hqq-spin/64B", func(b *testing.B) {
		p := hqqPair(b, LinkOptions{WaitPolicy: WaitSpin}, size)
		defer p.close()
		client, server := p.a.(*StandardLink), p.b.(*StandardLink)
		done := make(chan error, 1)
		go func() {
			for range b.N {
				req, err := server.ReserveRead()
				if err != nil {
					done <- err
					return
				}
				resp, err := server.ReserveWrite()
				if err != nil {
					done <- err
					return
				}
				n := copy(resp.Buffer(), req.Buffer())
				if err := resp.Commit(n); err != nil {
					done <- err
					return
				}
				if err := req.Release(); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
		msg := make([]byte, size)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			w, err := client.ReserveWrite()
			if err != nil {
				b.Fatal(err)
			}
			copy(w.Buffer(), msg)
			if err := w.Commit(size); err != nil {
				b.Fatal(err)
			}
			r, err := client.ReserveRead()
			if err != nil {
				b.Fatal(err)
			}
			if len(r.Buffer()) != size {
				b.Fatalf("response of %d bytes, want %d", len(r.Buffer()), size)
			}
			if err := r.Release(); err != nil {
				b.Fatal(err)
			}
		}
		if err := <-done; err != nil {
			b.Fatal(err)
		}
	})
}

// BenchmarkCompareThroughput measures one-way streaming throughput.
func BenchmarkCompareThroughput(b *testing.B) {
	for _, size := range []int{4096, 65536} {
		pairs := comparePairs(b, size)
		for _, name := range compareOrder {
			b.Run(name+"/"+sizeLabel(size), func(b *testing.B) {
				p := pairs[name]()
				defer p.close()
				done := make(chan error, 1)
				go func() {
					buf := make([]byte, size)
					for i := 0; i < b.N; i++ {
						if _, err := io.ReadFull(p.b, buf); err != nil {
							done <- err
							return
						}
					}
					done <- nil
				}()
				msg := make([]byte, size)
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := p.a.Write(msg); err != nil {
						b.Fatal(err)
					}
				}
				if err := <-done; err != nil {
					b.Fatal(err)
				}
			})
		}
	}
}

func sizeLabel(n int) string {
	if n >= 1024 {
		return itoa(n/1024) + "KiB"
	}
	return itoa(n) + "B"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func processCPUTime(t testing.TB) time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// TestIdleLinkDoesNotBurnCPU guards the end-to-end idle behavior: a link with
// a blocked reader and a blocked writer (heartbeats running) must stay far
// below one busy core. It compares CPU time with wall time, which is only
// meaningful on a quiet machine, so it runs only with HQQ_PERF_TESTS=1.
func TestIdleLinkDoesNotBurnCPU(t *testing.T) {
	if os.Getenv("HQQ_PERF_TESTS") != "1" {
		t.Skip("CPU-ratio measurement; set HQQ_PERF_TESTS=1 to run it on a quiet machine")
	}
	_, primary, secondary := newMemoryPair(t, 4, 64, LinkOptions{})
	for i := 0; i < 4; i++ {
		primary.Write([]byte("fill")) // fill primary->secondary
	}
	const wait = 500 * time.Millisecond
	before := processCPUTime(t)
	start := time.Now()
	primary.SetDeadline(start.Add(wait))
	readErr := make(chan error, 1)
	go func() {
		_, err := primary.Read(make([]byte, 64)) // nothing from secondary
		readErr <- err
	}()
	_, writeErr := primary.Write([]byte("blocked")) // ring is full
	rErr := <-readErr
	wall := time.Since(start)
	cpu := processCPUTime(t) - before
	if writeErr != ErrTimeout || rErr != ErrTimeout {
		t.Fatalf("blocked calls = %v / %v, want ErrTimeout", writeErr, rErr)
	}
	_ = secondary
	t.Logf("2 blocked calls for %v: %v CPU (%.1f%%)", wall, cpu, 100*float64(cpu)/float64(wall))
	if cpu > wall/4 {
		t.Fatalf("idle link used %v CPU over %v", cpu, wall)
	}
}
