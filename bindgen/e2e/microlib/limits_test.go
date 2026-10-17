//go:build unix

package microlib_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	hqq "gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/e2e/microlib"
	microlibv2 "gosuda.org/hqq/bindgen/e2e/microlib/v2"
	"gosuda.org/hqq/bindgen/rt"
)

// TestDigestMismatch: a client generated from microlib_v2.idl (ml_add takes an i64 as its second
// parameter) against the server built from microlib.idl. The handshake is refused with the reason
// "digest" and both digests, the refused attempts leak neither a session slot nor a segment, and the
// right client still connects.
func TestDigestMismatch(t *testing.T) {
	s := startServer(t, serverOpts{args: []string{"--max-sessions", "2"}, noTrace: true})
	local := microlibDigest(t, "microlib_v2.idl")
	remote := microlibDigest(t, "microlib.idl")
	if local == remote {
		t.Fatal("microlib_v2.idl has the digest of microlib.idl: the mismatch test would test nothing")
	}
	for range 20 { // more refusals than max_sessions: a refused session must give its slot back
		ctx, cancel := contextWithFail()
		err := dialV2(ctx, s.name)
		cancel()
		var me *rt.MismatchError
		if !errors.As(err, &me) {
			t.Fatalf("Dial error = %v (%T), want *rt.MismatchError", err, err)
		}
		if me.Reason != 1 || me.Local != local || me.Remote != remote {
			t.Fatalf("MismatchError{Reason: %d, Local: %x, Remote: %x}, want reason 1 (digest) with client %x and server %x",
				me.Reason, me.Local, me.Remote, local, remote)
		}
		if !errors.Is(err, rt.ErrInterfaceMismatch) {
			t.Fatalf("error %v does not match rt.ErrInterfaceMismatch", err)
		}
	}
	c := dial(t, s, rt.Options{})
	if got, err := c.Add(1, 2); err != nil || got != 3 {
		t.Fatalf("Add after the refusals = (%d, %v)", got, err)
	}
	if left := segmentsOf(t, s.name); len(left) > 1 {
		t.Fatalf("segments left by the refused handshakes: %v", left)
	}
}

// errConnected is what dialV2 returns when the v2 client connected, which must not happen.
var errConnected = errors.New("the v2 client connected to the v1 server")

// dialV2 dials with the client generated from microlib_v2.idl and returns the error of the dial.
func dialV2(ctx context.Context, listener string) error {
	c, err := microlibv2.Dial(ctx, listener, rt.Options{})
	if err != nil {
		return err
	}
	_ = c.Close()
	return errConnected
}

// TestHelloRefusals: each check of the HELLO of BINDGEN.md 4.11 in its own order, with the reply's
// status and the server's digest, and the zeroed limits of a refusal.
func TestHelloRefusals(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	good := microlibDigest(t, "microlib.idl")
	other := microlibDigest(t, "microlib_v2.idl")
	base := helloFields{major: 1, size: 4096, count: 64, digest: good}
	with := func(f func(h *helloFields)) helloFields { h := base; f(&h); return h }

	for _, tc := range []struct {
		name   string
		hello  helloFields
		status uint32
	}{
		{"accepted", base, 0},
		{"newer client minor is accepted", with(func(h *helloFields) { h.minor = 5 }), 0},
		{"flags set", with(func(h *helloFields) { h.flags = 1 }), 5},
		{"reserved set", with(func(h *helloFields) { h.reserved = 1 }), 5},
		{"major 2", with(func(h *helloFields) { h.major = 2 }), 2},
		{"digest", with(func(h *helloFields) { h.digest = other }), 1},
		{"buffer size differs from the link", with(func(h *helloFields) { h.size = 8192 }), 3},
		{"buffer count differs from the link", with(func(h *helloFields) { h.count = 32 }), 3},
		{"max_message below 4096", with(func(h *helloFields) { h.maxMessage = 100 }), 4},
		{"max_message above the hard maximum", with(func(h *helloFields) { h.maxMessage = 1<<30 + 1 }), 4},
		{"max_in_flight above B", with(func(h *helloFields) { h.maxInFlight = 65 }), 4},
		{"bad hello wins over a bad digest", with(func(h *helloFields) { h.flags = 1; h.digest = other }), 5},
		{"version wins over a bad digest", with(func(h *helloFields) { h.major = 2; h.digest = other }), 2},
		{"digest wins over a bad geometry", with(func(h *helloFields) { h.digest = other; h.size = 8192 }), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := rawOpen(t, s, 64, 4096, hqq.LinkOptions{})
			c.sendHelloFields(tc.hello)
			rep := c.recv(failBound)
			if rep.kind != kindHelloReply || rep.aux != tc.status || len(rep.body) != 64 {
				t.Fatalf("HELLO answered %v, want HELLO_REPLY status %d with a 64-byte body", rep, tc.status)
			}
			if got := [32]byte(rep.body[32:64]); got != good {
				t.Fatalf("HELLO_REPLY digest %x, want the server's %x", got, good)
			}
			maxInFlight, maxMessage, maxHandles := le.Uint32(rep.body[12:]), le.Uint32(rep.body[16:]), le.Uint32(rep.body[20:])
			if tc.status != 0 {
				if maxInFlight != 0 || maxMessage != 0 || maxHandles != 0 {
					t.Fatalf("a refusal granted limits (%d, %d, %d), want zeros", maxInFlight, maxMessage, maxHandles)
				}
				if _, err := c.tryRecv(failBound); err == nil {
					t.Fatal("the session continued after a refused HELLO")
				}
				return
			}
			if maxInFlight != 64 || maxMessage != 16<<20 || maxHandles != 4096 {
				t.Fatalf("granted (max_in_flight %d, max_message %d, max_handles %d), want the defaults (64, 16 MiB, 4096)", maxInFlight, maxMessage, maxHandles)
			}
		})
	}
}

// TestSessionSegmentNameIsUnlinkedAtHello: the server removes the session name as soon as it has read
// HELLO (BINDGEN.md 4.11), and the name has the documented shape.
func TestSessionSegmentNameIsUnlinkedAtHello(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true})
	ctl := controlLink(t, s)
	defer ctl.Close()
	r := callOpen(t, ctl, openMethod, openRequestBytes(1, 0, 64, 4096))
	if r.code != 0 {
		t.Fatalf("OPEN refused: code %d %q", r.code, r.msg)
	}
	if want := s.name + ".s"; len(r.name) != len(want)+16 || r.name[:len(want)] != want {
		t.Fatalf("session segment name %q, want %q followed by 16 hex digits", r.name, want)
	}
	if !nameExists(t, r.name) {
		t.Fatalf("session segment %q does not exist between OPEN and HELLO", r.name)
	}
	link, err := hqq.OpenStandardLink(r.name, int(r.count), int(r.size), hqq.LinkOptions{})
	wantNoErr(t, "attaching the session link", err)
	defer link.Close()
	c := &rawConn{t: t, nl: link, link: link.StandardLink}
	rep := c.hello(microlibDigest(t, "microlib.idl"), r.size, r.count)
	if rep.kind != kindHelloReply || rep.aux != 0 {
		t.Fatalf("HELLO answered %v", rep)
	}
	if nameExists(t, r.name) {
		t.Fatalf("session segment %q still exists after HELLO_REPLY", r.name)
	}
}

// TestHelloTimeoutRemovesTheSegment: a client that opens a session and never says HELLO is dropped
// after open_timeout, and the server removes the segment it created.
func TestHelloTimeoutRemovesTheSegment(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true, args: []string{"--open-timeout-ms", "300"}})
	ctl := controlLink(t, s)
	defer ctl.Close()
	r := callOpen(t, ctl, openMethod, openRequestBytes(1, 0, 64, 4096))
	if r.code != 0 {
		t.Fatalf("OPEN refused: code %d %q", r.code, r.msg)
	}
	if !nameExists(t, r.name) {
		t.Fatalf("session segment %q does not exist after OPEN", r.name)
	}
	eventually(t, "the unused session segment to be removed after open_timeout", func() bool { return !nameExists(t, r.name) })
}

// TestBootstrapRefusals: the OPEN request of 4.11 against every bound, with the documented codes.
func TestBootstrapRefusals(t *testing.T) {
	// The server's own default geometry (B = 32, S = 16384, 1 MiB) must fit max_segment_bytes.
	s := startServer(t, serverOpts{noTrace: true, args: []string{"--max-segment-bytes", "1048576", "--buffer-count", "32"}})
	open := func(major uint16, count, size uint32) []byte { return openRequestBytes(major, 0, count, size) }
	for _, tc := range []struct {
		name    string
		method  uint64
		payload []byte
		code    uint32
		msg     string
	}{
		{"proto major 2", openMethod, open(2, 8, 4096), openCodeVersion, ""},
		{"proto major 0", openMethod, open(0, 8, 4096), openCodeVersion, ""},
		{"B not a power of two", openMethod, open(1, 3, 4096), openCodeGeometry, ""},
		{"B 1", openMethod, open(1, 1, 4096), openCodeGeometry, ""},
		{"B above the maximum", openMethod, open(1, 8192, 128), openCodeGeometry, ""},
		{"S not a multiple of 64", openMethod, open(1, 8, 4100), openCodeGeometry, ""},
		{"S below the minimum", openMethod, open(1, 8, 64), openCodeGeometry, ""},
		{"S above the maximum", openMethod, open(1, 8, 2<<20), openCodeGeometry, ""},
		{"2*B*S just above max_segment_bytes", openMethod, open(1, 64, 8256), openCodeGeometry, ""},
		{"B*S overflowing 32 bits", openMethod, open(1, 4096, 1<<20), openCodeGeometry, ""},
		{"request of 15 bytes", openMethod, open(1, 8, 4096)[:15], openCodeInternal, "bad open request"},
		{"request of 17 bytes", openMethod, append(open(1, 8, 4096), 0), openCodeInternal, "bad open request"},
		{"wrong magic", openMethod, append([]byte("XXXX"), open(1, 8, 4096)[4:]...), openCodeInternal, "bad open request"},
		{"unknown method", 2, open(1, 8, 4096), openCodeInternal, "unknown method"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := openRefused(t, s, tc.method, tc.payload, tc.code)
			if tc.msg != "" && r.msg != tc.msg {
				t.Fatalf("refusal text %q, want %q", r.msg, tc.msg)
			}
		})
	}

	t.Run("exactly max_segment_bytes is accepted", func(t *testing.T) {
		ctl := controlLink(t, s)
		defer ctl.Close()
		r := callOpen(t, ctl, openMethod, open(1, 64, 8192))
		if r.code != 0 || r.count != 64 || r.size != 8192 {
			t.Fatalf("OPEN of 2*64*8192 = max_segment_bytes answered %+v, want the geometry granted", r)
		}
		wantNoErr(t, "unlinking the unused session segment", hqq.Unlink(r.name))
	})

	t.Run("second OPEN on one control link", func(t *testing.T) {
		ctl := controlLink(t, s)
		defer ctl.Close()
		r := callOpen(t, ctl, openMethod, open(1, 8, 4096))
		if r.code != 0 {
			t.Fatalf("first OPEN refused: %+v", r)
		}
		wantNoErr(t, "unlinking the unused session segment", hqq.Unlink(r.name))
		if r2 := callOpen(t, ctl, openMethod, open(1, 8, 4096)); r2.code != openCodeInternal || r2.msg != "control link already used" {
			t.Fatalf("second OPEN answered %+v, want code 4 %q", r2, "control link already used")
		}
	})

	t.Run("the generated client reports the refusal as rt.OpenError", func(t *testing.T) {
		// The generated client's default geometry, B = 64 and S = 16384, needs 2 MiB: above this
		// server's max_segment_bytes.
		_, err := dialErr(s, rt.Options{})
		var oe *rt.OpenError
		if !errors.As(err, &oe) || oe.Code != openCodeGeometry {
			t.Fatalf("Dial = %v (%T), want *rt.OpenError with code %d", err, err, openCodeGeometry)
		}
		if !errors.Is(err, rt.ErrLimit) {
			t.Fatalf("error %v does not match rt.ErrLimit", err)
		}
		c := dial(t, s, rt.Options{BufferCount: 16, BufferSize: 4096})
		if got, err := c.Add(1, 2); err != nil || got != 3 {
			t.Fatalf("Add on the smaller geometry = (%d, %v)", got, err)
		}
	})
}

// TestMaxSessions: the session count is bounded, the extra client gets BUSY (matching rt.ErrLimit), and
// a closed session gives its slot back every time.
func TestMaxSessions(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true, args: []string{"--max-sessions", "3"}})
	clients := make([]*microlib.Client, 0, 3)
	for range 3 {
		clients = append(clients, dial(t, s, rt.Options{}))
	}
	_, err := dialErr(s, rt.Options{})
	var oe *rt.OpenError
	if !errors.As(err, &oe) || oe.Code != openCodeBusy || !errors.Is(err, rt.ErrLimit) {
		t.Fatalf("fourth Dial = %v, want *rt.OpenError code %d matching rt.ErrLimit", err, openCodeBusy)
	}

	// The slot is released after CLOSE_ACK, a moment after Close returns: retry the dial.
	wantNoErr(t, "Close", clients[0].Close())
	wantNoErr(t, "Close", dialUntil(t, s, rt.Options{}, isBusy).Close())

	// Churn: many more sessions than the limit over the life of the server, with one slot free.
	for range 200 {
		c := dialUntil(t, s, rt.Options{}, isBusy)
		if got, err := c.Add(1, 1); err != nil || got != 2 {
			t.Fatalf("Add on a churned session = (%d, %v)", got, err)
		}
		wantNoErr(t, "Close", c.Close())
	}
	s.requireNoSessionSegments()
}

// TestControlLinkLimit: at most max_control_links idle control links are served; one beyond the limit
// is closed at once, and no control link segment stays in the namespace after the server accepted it
// (BINDGEN.md 4.11). The idle ones are closed after open_timeout, which frees their slots.
func TestControlLinkLimit(t *testing.T) {
	s := startServer(t, serverOpts{noTrace: true, args: []string{"--max-control-links", "2", "--open-timeout-ms", "600"}})
	idle := []*hqq.NamedAdvancedLink{controlLink(t, s), controlLink(t, s)}
	third := controlLink(t, s)
	all := append(idle, third)

	// The server unlinks every control link right after it accepts it, the ones it serves and the one
	// it drops.
	for i, l := range all {
		name := l.Name()
		eventually(t, fmt.Sprintf("the segment of control link %d to be unlinked", i), func() bool { return !nameExists(t, name) })
	}
	// The third is closed by the server: a call on it fails instead of being answered.
	ctx, cancel := contextWithFail()
	defer cancel()
	if _, err := third.Call(ctx, openMethod, openRequestBytes(1, 0, 8, 4096)); err == nil {
		t.Fatal("a control link beyond max_control_links was served")
	}
	for _, l := range all {
		_ = l.Close()
	}
	// With the two idle links gone (the client closed them, or open_timeout did), sessions open again.
	c := dialUntil(t, s, rt.Options{}, func(error) bool { return true })
	if got, err := c.Add(2, 3); err != nil || got != 5 {
		t.Fatalf("Add = (%d, %v)", got, err)
	}
	wantNoErr(t, "Close", c.Close())
}
