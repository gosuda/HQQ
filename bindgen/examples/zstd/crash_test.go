package zstd_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/examples/zstd"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/launch"
)

// killedServers holds the servers that a test killed, whose exit status the cleanup must not judge.
var killedServers sync.Map

// killServer sends SIGKILL to the server process and waits until it is gone.
func killServer(t *testing.T, srv *launch.Server) {
	t.Helper()
	killedServers.Store(srv, true)
	p, err := os.FindProcess(srv.PID())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("kill server %d: %v", srv.PID(), err)
	}
	select {
	case <-srv.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the killed server did not exit")
	}
	if srv.Err() == nil {
		t.Error("a killed server reports exit status 0")
	}
}

func wantPeerGone(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, hqq.ErrPeerGone) && !errors.Is(err, hqq.ErrPeerClosed) {
		t.Errorf("error = %v, want hqq.ErrPeerGone or hqq.ErrPeerClosed", err)
	}
	if !errors.Is(err, rt.ErrOutcomeUnknown) {
		t.Errorf("error = %v, want an unknown outcome (rt.ErrOutcomeUnknown)", err)
	}
}

// TestServerKilledWhileIdle kills the server between two calls. The next call fails with a transport
// error within the peer timeout, the session is then broken for good, a new Dial fails, and a new
// server works. The contexts of the dead session need no cleanup on the client.
func TestServerKilledWhileIdle(t *testing.T) {
	srv := ownServer(t)
	c := dialTo(t, srv.Listener())
	cctx := mustCCtx(t, c)
	stillWorks(t, c)

	killServer(t, srv)
	start := time.Now()
	_, err := c.Compress(make([]byte, 1000), textData(500, 31), 3)
	if err == nil {
		t.Fatal("a call to a killed server succeeded")
	}
	wantPeerGone(t, err)
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("the failure took %v to surface; the peer timeout is 2 s", d)
	}

	if c.Conn().Err() == nil {
		t.Error("the session is not marked broken")
	}
	_, err = c.CompressCCtx(cctx, make([]byte, 1000), textData(500, 31), 3)
	if !errors.Is(err, rt.ErrSessionBroken) {
		t.Errorf("call on a broken session: error = %v, want rt.ErrSessionBroken", err)
	}
	if err := c.FreeCCtx(cctx); !errors.Is(err, rt.ErrSessionBroken) {
		t.Errorf("FreeCCtx on a broken session: error = %v, want rt.ErrSessionBroken", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if c2, err := zstd.Dial(ctx, srv.Listener(), zstd.DefaultOptions()); err == nil {
		c2.Kill()
		t.Error("Dial to the listener of a killed server succeeded")
	}

	fresh := ownServer(t)
	stillWorks(t, dialTo(t, fresh.Listener()))
}

// TestServerKilledWithCallsInFlight queues four heavy calls (level 19 on 2 MiB takes tens of
// milliseconds each) and kills the server at once. Every Wait returns an error that says the outcome
// is unknown, none hangs, and the futures of calls that did not finish never return data.
func TestServerKilledWithCallsInFlight(t *testing.T) {
	srv := ownServer(t)
	c := dialTo(t, srv.Listener())
	checker := newClient(t) // on the shared server: decodes the frames of calls that finished
	src := textData(2<<20, 32)
	const calls = 4
	futs := make([]zstd.CompressFuture, calls)
	for i := range calls {
		var err error
		dst := make([]byte, zstd.CompressBound(uint64(len(src))))
		if futs[i], err = c.CompressAsync(dst, src, 19); err != nil {
			t.Fatal(err)
		}
	}
	killServer(t, srv)

	done := make(chan struct{})
	go func() {
		defer close(done)
		failed := 0
		for i, f := range futs {
			frame, err := f.Wait()
			if err == nil {
				// A call that finished before the kill delivered a complete frame.
				if out, derr := checker.Decompress(make([]byte, len(src)), frame); derr != nil || len(out) != len(src) {
					t.Errorf("call %d returned a frame that does not decode: %v", i, derr)
				}
				continue
			}
			failed++
			wantPeerGone(t, err)
		}
		if failed == 0 {
			t.Error("every call completed before the kill: the test did not exercise in-flight calls")
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Wait did not return after the server was killed")
	}
}

// TestServerStopClosesSessionsGracefully stops the server while a session is idle. The client sees the
// close, not a crash: the next call fails and the session is broken, and the server exits with status 0.
func TestServerStopClosesSessionsGracefully(t *testing.T) {
	srv := ownServer(t)
	c := dialTo(t, srv.Listener())
	stillWorks(t, c)
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	_, err := c.Compress(make([]byte, 100), []byte("x"), 1)
	if err == nil {
		t.Fatal("a call to a stopped server succeeded")
	}
	if !errors.Is(err, rt.ErrOutcomeUnknown) && !errors.Is(err, rt.ErrShuttingDown) && !errors.Is(err, hqq.ErrPeerClosed) && !errors.Is(err, hqq.ErrPeerGone) {
		t.Errorf("error = %v, want a transport or shutdown failure", err)
	}
	if c.Conn().Err() == nil {
		t.Error("the session is not marked broken after the server stopped")
	}
}
