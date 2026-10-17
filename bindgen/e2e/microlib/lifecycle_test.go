//go:build unix

package microlib_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	hqq "gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/e2e/microlib"
	"gosuda.org/hqq/bindgen/rt"
)

const (
	// stallDelta is the delta that makes the hook of ml_counter_add stall when ML_E2E_STALL_MS is set
	// (server/microlib_hooks.c). The hook writes stallLine to stderr first.
	stallDelta = 424242
	stallLine  = "microlib_hooks: stall"
	// stallForever keeps the hook asleep until the test ends the server.
	stallForever = "ML_E2E_STALL_MS=600000"
)

// TestServerKilledDuringPipelinedCalls: kill -9 of the server while 32 calls are in flight. The server
// is inside the library on call 11 (the hook stalls), so calls 0 to 9 are answered and committed in the
// response ring, call 10 and the 21 behind it are not. The answered futures keep their results; the
// others fail with hqq.ErrPeerGone and an unknown outcome; every later call fails with
// rt.ErrSessionBroken, which is a known outcome (nothing was committed).
func TestServerKilledDuringPipelinedCalls(t *testing.T) {
	s := startServer(t, serverOpts{args: fastServerArgs, env: []string{stallForever}})
	c := fastDial(t, s, rt.Options{})
	ctr, err := c.CounterNew(0)
	wantNoErr(t, "CounterNew", err)

	var answered []microlib.AddFuture
	for i := range 10 {
		f, err := c.AddAsync(int32(i), 100)
		wantNoErr(t, "AddAsync", err)
		answered = append(answered, f)
	}
	stalled, err := c.CounterAddAsync(ctr, stallDelta)
	wantNoErr(t, "CounterAddAsync", err)
	var behind []microlib.AddFuture
	for i := range 21 {
		f, err := c.AddAsync(int32(i), 1)
		wantNoErr(t, "AddAsync", err)
		behind = append(behind, f)
	}
	s.stderr.waitFor(t, stallLine) // the server is inside the library, with the 10 answers committed
	s.kill()

	for i, f := range answered {
		got, err := f.Wait()
		if err != nil || got != int32(i)+100 {
			t.Fatalf("answered call %d = (%d, %v), want (%d, nil): a response committed before the crash must be delivered", i, got, err, i+100)
		}
	}
	wantUnknownGone := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, hqq.ErrPeerGone) || !errors.Is(err, rt.ErrOutcomeUnknown) {
			t.Fatalf("%s: error = %v, want hqq.ErrPeerGone with rt.ErrOutcomeUnknown", what, err)
		}
		var te *rt.TransportError
		if !errors.As(err, &te) || !te.Unknown {
			t.Fatalf("%s: error = %v (%T), want *rt.TransportError with Unknown set", what, err, err)
		}
	}
	_, err = stalled.Wait()
	wantUnknownGone("the call the server was executing", err)
	for i, f := range behind {
		_, err := f.Wait()
		wantUnknownGone(fmt.Sprintf("call %d behind it", i), err)
	}
	_, err = c.Add(1, 1)
	if !errors.Is(err, rt.ErrSessionBroken) || errors.Is(err, rt.ErrOutcomeUnknown) {
		t.Fatalf("Add on a broken session = %v, want rt.ErrSessionBroken with a known outcome", err)
	}
	if c.Conn().Err() == nil {
		t.Error("Conn.Err() is nil after the server died")
	}
}

// TestServerSIGTERM: a graceful stop with a live session. The server tears the session down (the
// library sees its objects destroyed), exits with status 0, removes its listener name, and the
// client's next call ends in an error that says the server went away, without a hang.
func TestServerSIGTERM(t *testing.T) {
	s := startServer(t, serverOpts{})
	s.clientsMayFail = true
	c := dial(t, s, rt.Options{})
	ctr, err := c.CounterNew(1)
	wantNoErr(t, "CounterNew", err)
	_, err = c.CursorOpen(ctr, 1)
	wantNoErr(t, "CursorOpen", err)

	s.signal(syscall.SIGTERM)
	if err := s.waitExit(); err != nil {
		t.Fatalf("server exit after SIGTERM: %v\nstderr:\n%s", err, s.stderr.String())
	}
	want := []string{"ml_cursor_free cursor=1 ret=2", "ml_counter_free counter=1"}
	if got := s.traceFuncs(destroyFuncs...); !slices.Equal(got, want) {
		t.Fatalf("teardown at stop:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	_, err = c.CounterAdd(ctr, 1)
	if !errors.Is(err, rt.ErrShuttingDown) && !errors.Is(err, hqq.ErrPeerClosed) && !errors.Is(err, hqq.ErrPeerGone) {
		t.Fatalf("a call after the server stopped = %v, want rt.ErrShuttingDown or a closed peer", err)
	}
	// Close of a session whose server is gone returns an error instead of hanging.
	_ = c.Close()
}

// TestParentLifelineStop: launch.Server.Stop closes the server's standard input, the lifeline
// (--parent-stdin), which the server reads as "the parent is gone": it stops like on SIGTERM.
func TestParentLifelineStop(t *testing.T) {
	s := startServer(t, serverOpts{})
	s.clientsMayFail = true
	c := dial(t, s, rt.Options{})
	_, err := c.CounterNew(1)
	wantNoErr(t, "CounterNew", err)

	ctx, cancel := context.WithTimeout(context.Background(), failBound)
	defer cancel()
	if err := s.proc.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v\nstderr:\n%s", err, s.stderr.String())
	}
	if got := s.traceFuncs(destroyFuncs...); !slices.Equal(got, []string{"ml_counter_free counter=1"}) {
		t.Fatalf("teardown at stop: %q", got)
	}
	_ = c.Close() // the server is gone: the error is expected and not the subject
}

// TestParentDies: the process that launched the server is killed with SIGKILL. The lifeline pipe
// closes, the server shuts down gracefully by itself and removes its listener name.
func TestParentDies(t *testing.T) {
	bin := serverBinary(t)
	name := uniqueName(t)
	trace := t.TempDir() + "/trace"
	h := startHelper(t, "parent", envBinary+"="+bin, envListener+"="+name, envTrace+"="+trace)
	if len(h.pre) != 1 || !strings.HasPrefix(h.pre[0], "pid ") {
		t.Fatalf("parent helper printed %q before ready, want one pid line", h.pre)
	}
	// The server is up: a client can use it.
	ctx, cancel := context.WithTimeout(context.Background(), failBound)
	defer cancel()
	c, err := dialWith(ctx, name, rt.Options{})
	wantNoErr(t, "Dial", err)
	_, err = c.CounterNew(1)
	wantNoErr(t, "CounterNew", err)

	h.kill(t)
	eventually(t, "the server of the dead parent to stop and remove its listener", func() bool {
		return len(segmentsOf(t, name)) == 0
	})
	// The session was torn down by the server's shutdown: its counter was destroyed.
	b, err := os.ReadFile(trace)
	wantNoErr(t, "reading the trace", err)
	if !strings.Contains(string(b), "ml_counter_free counter=1") {
		t.Fatalf("trace after the parent died has no teardown of the session:\n%s", b)
	}
	_ = c.Close() // the server is gone: the error is expected and not the subject
}

// TestExecutionLockSerializesSessions: with @serialize(session) a library call of one session excludes
// every other session (BINDGEN.md 8.1). The hook of session 1 stalls inside the library; session 2's
// ml_add, issued meanwhile, must reach the library only after session 1's call finished, which the
// trace order shows.
func TestExecutionLockSerializesSessions(t *testing.T) {
	s := startServer(t, serverOpts{env: []string{"ML_E2E_STALL_MS=300"}})
	c1 := dial(t, s, rt.Options{})
	c2 := dial(t, s, rt.Options{})
	ctr, err := c1.CounterNew(0)
	wantNoErr(t, "CounterNew", err)

	f1, err := c1.CounterAddAsync(ctr, stallDelta)
	wantNoErr(t, "CounterAddAsync", err)
	s.stderr.waitFor(t, stallLine) // session 1 is inside the library, holding the execution lock
	f2, err := c2.AddAsync(40, 2)
	wantNoErr(t, "AddAsync", err)

	total, err := f1.Wait()
	wantNoErr(t, "session 1 call", err)
	if total != stallDelta {
		t.Fatalf("total = %d, want %d", total, stallDelta)
	}
	sum, err := f2.Wait()
	wantNoErr(t, "session 2 call", err)
	if sum != 42 {
		t.Fatalf("sum = %d, want 42", sum)
	}
	wantNoErr(t, "c1.Close", c1.Close())
	wantNoErr(t, "c2.Close", c2.Close())
	got := s.traceFuncs("ml_counter_add", "ml_add")
	want := []string{"ml_counter_add counter=1 delta=424242 ret=0 total=424242", "ml_add a=40 b=2 ret=42"}
	if !slices.Equal(got, want) {
		t.Fatalf("library call order %q, want %q: session 2 ran inside session 1's library call", got, want)
	}
}

// TestStalledLibraryDelaysShutdown: a library call that does not return cannot be cancelled. The
// server's stop waits shutdown_timeout (500 ms here), then the process exits with a failure status
// without freeing what a stuck thread may still use (BINDGEN.md 5.2); the caller of the stuck call
// sees the server go away.
func TestStalledLibraryDelaysShutdown(t *testing.T) {
	// The exit leaves the stuck thread's memory unreleased on purpose, so LeakSanitizer is off here.
	s := startServer(t, serverOpts{
		args: append([]string{"--shutdown-timeout-ms", "500"}, fastServerArgs...),
		env:  []string{stallForever, "ASAN_OPTIONS=halt_on_error=1:detect_leaks=0"},
	})
	s.expectDeath = true
	c := fastDial(t, s, rt.Options{})
	ctr, err := c.CounterNew(0)
	wantNoErr(t, "CounterNew", err)
	stuck, err := c.CounterAddAsync(ctr, stallDelta)
	wantNoErr(t, "CounterAddAsync", err)
	s.stderr.waitFor(t, stallLine)

	start := time.Now()
	s.signal(syscall.SIGTERM)
	err = s.waitExit()
	elapsed := time.Since(start)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("server exit = %v, want a nonzero exit status (shutdown timed out)", err)
	}
	if elapsed < 400*time.Millisecond {
		t.Fatalf("the server stopped %v after SIGTERM although a library call was stuck, want about the 500ms shutdown timeout", elapsed)
	}
	_, err = stuck.Wait()
	if !errors.Is(err, rt.ErrOutcomeUnknown) {
		t.Fatalf("the stuck call ended with %v, want an unknown outcome", err)
	}
}

// TestClientThatNeverReads: a client that ignores flow control (the generated client cannot) commits
// requests until the server's response ring is full. The server waits for space for
// write_stall_timeout, then ends that session with no final frame; a second session is unaffected,
// before, during and after.
func TestClientThatNeverReads(t *testing.T) {
	s := startServer(t, serverOpts{args: []string{"--write-stall-ms", "300"}})
	other := dial(t, s, rt.Options{})
	if got, err := other.Add(1, 1); err != nil || got != 2 {
		t.Fatalf("other.Add before = (%d, %v)", got, err)
	}

	// B = 2: the server publishes two responses, holds the third reservation and waits.
	bad := rawSession(t, s, 2, 4096)
	for id := uint32(1); id <= 5; id++ {
		if err := bad.trySend(kindRequest, 0, opAdd, id, 8, 0, addRequest(int32(id), 1)); err != nil {
			t.Fatalf("committing request %d: %v", id, err)
		}
	}
	// The other session works while the first one is stalled.
	if got, err := other.Add(20, 22); err != nil || got != 42 {
		t.Fatalf("other.Add during the stall = (%d, %v)", got, err)
	}
	// The stalled session ends: the two published responses are still delivered, then the link
	// closes, with no E_SHUTTING_DOWN or E_PROTOCOL frame in between.
	for id := uint32(1); id <= 2; id++ {
		f, err := bad.tryRecv(failBound)
		if err != nil {
			t.Fatalf("response %d: %v", id, err)
		}
		if f.kind != kindResponse || f.id != id || f.aux != 0 {
			t.Fatalf("response %d = %v", id, f)
		}
	}
	f, err := bad.tryRecv(failBound)
	if err == nil {
		t.Fatalf("a third frame %v arrived: the stalled session must end without a final frame", f)
	}
	if !errors.Is(err, hqq.ErrPeerClosed) && !errors.Is(err, hqq.ErrPeerGone) {
		t.Fatalf("after the stall the link reports %v, want a closed peer", err)
	}
	if got, err := other.Add(1, 2); err != nil || got != 3 {
		t.Fatalf("other.Add after = (%d, %v)", got, err)
	}
}

// TestProtocolViolations sends frames on a fresh session that break a rule of BINDGEN.md 4.4 or 4.12 and expects the
// server's reaction of 5.4: a RESPONSE with id 0, op 0 and status 7 (E_PROTOCOL), then the end of the
// session. The server must keep serving other sessions.
func TestProtocolViolations(t *testing.T) {
	s := startServer(t, serverOpts{args: []string{"--max-message", "4096"}, noTrace: true})
	healthy := dial(t, s, rt.Options{})
	digest := microlibDigest(t, "microlib.idl")

	cases := []struct {
		name string
		do   func(c *rawConn)
		// noHello: the case runs before HELLO
		noHello bool
	}{
		{"first request id is not 1", func(c *rawConn) { c.sendAdd(2, 1, 1) }, false},
		{"request id skips a number", func(c *rawConn) {
			c.sendAdd(1, 1, 1)
			c.sendAdd(3, 1, 1)
		}, false},
		{"reserved flag bit", func(c *rawConn) {
			c.send(kindRequest, 0x80, opAdd, 1, 8, 0, addRequest(1, 1))
		}, false},
		{"request total above max_message", func(c *rawConn) {
			c.send(kindRequest, 0, opAdd, 1, 5000, 0, make([]byte, 4080))
		}, false},
		{"data size does not match total", func(c *rawConn) {
			c.send(kindRequest, 0, opAdd, 1, 4, 0, addRequest(1, 1)) // 8 bytes of data, total 4
		}, false},
		{"batch request frame", func(c *rawConn) { c.send(0x06, 0, 0, 1, 8, 0, addRequest(1, 1)) }, false},
		{"a response frame sent to the server", func(c *rawConn) { c.send(kindResponse, 0, opAdd, 1, 4, 0, make([]byte, 4)) }, false},
		{"second HELLO", func(c *rawConn) { c.sendHello(digest, 4096, 64) }, false},
		{"request before HELLO", func(c *rawConn) { c.sendAdd(1, 1, 1) }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c *rawConn
			if tc.noHello {
				c = rawOpen(t, s, 64, 4096, hqq.LinkOptions{})
			} else {
				c = rawSession(t, s, 64, 4096)
			}
			tc.do(c)
			// The request that is valid on its own may be answered before the offending frame is read.
			f := c.recv(failBound)
			for f.kind == kindResponse && f.id != 0 && f.aux == 0 {
				f = c.recv(failBound) // the answer of a valid request that preceded the violation
			}
			if f.kind != kindResponse || f.id != 0 || f.op != 0 || f.aux != 7 {
				t.Fatalf("reaction = %v, want RESPONSE id 0 op 0 status 7 (E_PROTOCOL)", f)
			}
			if _, err := c.tryRecv(failBound); err == nil {
				t.Fatal("the session continued after a protocol violation")
			}
			if got, err := healthy.Add(3, 4); err != nil || got != 7 {
				t.Fatalf("healthy session after the violation = (%d, %v)", got, err)
			}
		})
	}
}

// TestMalformedBodyIsNotAViolation: a well-framed request with a bad body is answered with
// E_BAD_REQUEST (or E_UNKNOWN_OP), executes nothing, and the session goes on (BINDGEN.md 5.4).
func TestMalformedBodyIsNotAViolation(t *testing.T) {
	s := startServer(t, serverOpts{})
	c := rawSession(t, s, 64, 4096)

	reply := func(id uint32, status uint32) frame {
		t.Helper()
		f := c.recv(failBound)
		if f.kind != kindResponse || f.id != id || f.aux != status {
			t.Fatalf("answer to request %d = %v, want RESPONSE status %d", id, f, status)
		}
		return f
	}
	c.send(kindRequest, 0, opAdd, 1, 7, 0, make([]byte, 7)) // ml_add needs 8 bytes
	reply(1, 2)
	c.send(kindRequest, 0, 99, 2, 0, 0, nil) // no such function
	reply(2, 1)
	c.send(kindRequest, 0, 0, 3, 0, 0, nil) // op 0 is not a function either
	reply(3, 1)
	c.sendAdd(4, 40, 2)
	f := reply(4, 0)
	if len(f.body) != 4 || le.Uint32(f.body) != 42 {
		t.Fatalf("Add after the malformed requests answered %x, want 42", f.body)
	}
	c.send(kindClose, 0, 0, 0, 0, 0, nil)
	if f := c.recv(failBound); f.kind != kindCloseAck {
		t.Fatalf("answer to CLOSE = %v, want CLOSE_ACK", f)
	}
	if got := s.traceFuncs("ml_add"); !slices.Equal(got, []string{"ml_add a=40 b=2 ret=42"}) {
		t.Fatalf("library saw %q: the malformed requests must not reach it", got)
	}
}
