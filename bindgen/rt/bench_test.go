//go:build !race

package rt

import "testing"

func benchPair(b *testing.B) (*stubClient, func()) {
	b.Helper()
	p := newPair(b, 64, 4096)
	done := make(chan struct{})
	go zeroAllocAddServer(p.server, done)
	c := newTestConn(b, p, connOpts{})
	return &stubClient{c}, func() {
		_ = c.Close()
		_ = p.server.Close()
		<-done
	}
}

func BenchmarkAddSync(b *testing.B) {
	cl, stop := benchPair(b)
	defer stop()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := cl.Add(20, 22); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAddPipelined8(b *testing.B) {
	cl, stop := benchPair(b)
	defer stop()
	var futs [8]addFuture
	b.ReportAllocs()
	for b.Loop() {
		for i := range futs {
			f, err := cl.AddAsync(int32(i), 1)
			if err != nil {
				b.Fatal(err)
			}
			futs[i] = f
		}
		for _, f := range futs {
			if _, err := f.Wait(); err != nil {
				b.Fatal(err)
			}
		}
	}
}
