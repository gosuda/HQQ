//go:build !race

package rt

import (
	"testing"

	hqq "gosuda.org/hqq"
)

// zeroAllocAddServer answers Add requests without allocating, so that the
// process-wide allocation count of AllocsPerRun sees only the client.
func zeroAllocAddServer(link *hqq.StandardLink, done chan<- struct{}) {
	defer close(done)
	for {
		r, err := link.ReserveRead()
		if err != nil {
			return
		}
		b := r.Buffer()
		if len(b) == HeaderSize {
			_ = r.Release() // CLOSE
			w, err := link.ReserveWrite()
			if err != nil {
				return
			}
			putHeader(w.Buffer(), header{kind: kindCloseAck})
			_ = w.Commit(HeaderSize)
			continue
		}
		h := loadHeader(b)
		sum := le.Uint32(b[HeaderSize:]) + le.Uint32(b[HeaderSize+4:])
		_ = r.Release()
		w, err := link.ReserveWrite()
		if err != nil {
			return
		}
		out := w.Buffer()
		putHeader(out, header{kind: kindResponse, op: h.op, id: h.id, total: 4})
		le.PutUint32(out[HeaderSize:], sum)
		if err := w.Commit(HeaderSize + 4); err != nil {
			return
		}
	}
}

// TestHotPathAllocs: BeginRequest, Submit and an in-order Wait, with small
// payloads and CallTimeout == 0, allocate nothing after warm-up (8.2, 11.2).
func TestHotPathAllocs(t *testing.T) {
	p := newPair(t, 64, 4096)
	done := make(chan struct{})
	go zeroAllocAddServer(p.server, done)
	c := newTestConn(t, p, connOpts{})
	cl := &stubClient{c}

	t.Run("sync", func(t *testing.T) {
		var sink int32
		allocs := testing.AllocsPerRun(2000, func() {
			v, err := cl.Add(20, 22)
			if err != nil || v != 42 {
				t.Fatalf("Add = %d, %v", v, err)
			}
			sink += v
		})
		if allocs != 0 {
			t.Errorf("Add allocates %v times per call", allocs)
		}
		_ = sink
	})
	t.Run("pipelined", func(t *testing.T) {
		var futs [8]addFuture
		allocs := testing.AllocsPerRun(500, func() {
			for i := range futs {
				f, err := cl.AddAsync(int32(i), 1)
				if err != nil {
					t.Fatal(err)
				}
				futs[i] = f
			}
			for i, f := range futs {
				v, err := f.Wait()
				if err != nil || v != int32(i)+1 {
					t.Fatalf("Wait = %d, %v", v, err)
				}
			}
		})
		if allocs != 0 {
			t.Errorf("an 8-call in-order pipeline allocates %v times per round", allocs)
		}
	})
	t.Run("beyond the window", func(t *testing.T) {
		// More calls than MaxInFlight: BeginRequest buffers a response
		// into a recycled per-call buffer, which allocates only until the
		// buffers exist.
		c2 := newTestConn(t, newPairWithServer(t), connOpts{maxInFlight: 4})
		cl2 := &stubClient{c2}
		round := func() {
			var futs [12]addFuture
			for i := range futs {
				f, err := cl2.AddAsync(int32(i), 2)
				if err != nil {
					t.Fatal(err)
				}
				futs[i] = f
			}
			for i, f := range futs {
				if v, err := f.Wait(); err != nil || v != int32(i)+2 {
					t.Fatalf("Wait = %d, %v", v, err)
				}
			}
		}
		round()
		if allocs := testing.AllocsPerRun(200, round); allocs != 0 {
			t.Errorf("windowed pipeline allocates %v times per round after warm-up", allocs)
		}
	})
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	_ = p.server.Close()
	<-done
}

// newPairWithServer is a pair whose server is a zeroAllocAddServer.
func newPairWithServer(t *testing.T) *pair {
	p := newPair(t, 64, 4096)
	done := make(chan struct{})
	go zeroAllocAddServer(p.server, done)
	t.Cleanup(func() {
		_ = p.client.Close()
		_ = p.server.Close()
		<-done
	})
	return p
}
