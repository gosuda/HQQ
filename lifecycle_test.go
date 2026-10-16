package hqq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastLiveness makes peer failures observable within a fraction of a second.
var fastLiveness = LinkOptions{
	HeartbeatInterval: 10 * time.Millisecond,
	PeerTimeout:       150 * time.Millisecond,
	AttachTimeout:     time.Second,
}

func newMemoryPair(t testing.TB, bufferCount, bufferSize int, opts LinkOptions) ([]byte, *StandardLink, *StandardLink) {
	t.Helper()
	mem := AlignedBuffer(SizeStandardLink(bufferCount, bufferSize))
	primary, err := OpenStandardLinkMemory(mem, bufferCount, bufferSize, opts)
	if err != nil {
		t.Fatalf("open primary: %v", err)
	}
	secondary, err := OpenStandardLinkMemory(mem, bufferCount, bufferSize, opts)
	if err != nil {
		primary.Close()
		t.Fatalf("open secondary: %v", err)
	}
	t.Cleanup(func() {
		secondary.Close()
		primary.Close()
	})
	return mem, primary, secondary
}

// simulateCrash stops an endpoint's heartbeat without detaching it, which is
// what a killed process leaves behind.
func simulateCrash(l *StandardLink) {
	if l.hbStop != nil {
		close(l.hbStop)
		<-l.hbDone
		l.hbStop = nil
	}
}

func waitErr(t *testing.T, ch <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(within):
		t.Fatalf("operation did not return within %v", within)
		return nil
	}
}

func TestStandardLinkCloseWakesBlockedRead(t *testing.T) {
	_, _, secondary := newMemoryPair(t, 4, 64, LinkOptions{})
	errCh := make(chan error, 1)
	go func() {
		_, err := secondary.Read(make([]byte, 64))
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond) // let Read park
	if err := secondary.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := waitErr(t, errCh, time.Second)
	if !errors.Is(err, ErrClosed) || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("blocked Read after Close = %v, want ErrClosed (net.ErrClosed)", err)
	}
	if _, err := secondary.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close = %v, want ErrClosed", err)
	}
	if _, err := secondary.ReserveRead(); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReserveRead after Close = %v, want ErrClosed", err)
	}
	if err := secondary.SetReadDeadline(time.Now()); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetReadDeadline after Close = %v, want ErrClosed", err)
	}
	if err := secondary.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStandardLinkCloseWakesBlockedWrite(t *testing.T) {
	_, primary, _ := newMemoryPair(t, 2, 64, LinkOptions{})
	for i := 0; i < 2; i++ {
		if _, err := primary.Write([]byte("fill")); err != nil {
			t.Fatalf("fill Write: %v", err)
		}
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := primary.Write([]byte("blocked"))
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	primary.Close()
	if err := waitErr(t, errCh, time.Second); !errors.Is(err, ErrClosed) {
		t.Fatalf("blocked Write after Close = %v, want ErrClosed", err)
	}
}

// TestNamedLinkCloseWithBlockedReadIsSafe checks that Close does not unmap
// the shared memory while another goroutine is still inside the ring.
func TestNamedLinkCloseWithBlockedReadIsSafe(t *testing.T) {
	name := newPortableSharedMemoryName()
	primary, err := CreateStandardLink(name, 4, 64, 0o600)
	if err != nil {
		t.Fatalf("CreateStandardLink: %v", err)
	}
	defer primary.Close()
	secondary, err := OpenNamedStandardLink(name, 4, 64)
	if err != nil {
		t.Fatalf("OpenNamedStandardLink: %v", err)
	}

	errCh := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, err := secondary.Read(make([]byte, 64))
			errCh <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	if err := secondary.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := waitErr(t, errCh, time.Second); !errors.Is(err, ErrClosed) {
			t.Fatalf("Read after Close = %v, want ErrClosed", err)
		}
	}
}

func TestPeerClosedIsReportedAfterBufferedData(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 8, 64, fastLiveness)
	if got := primary.PeerState(); got != PeerConnected {
		t.Fatalf("primary PeerState = %v, want connected", got)
	}
	if _, err := secondary.Write([]byte("last words")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	secondary.Close()

	buf := make([]byte, 64)
	n, err := primary.Read(buf)
	if err != nil || string(buf[:n]) != "last words" {
		t.Fatalf("Read buffered message = %q, %v", buf[:n], err)
	}
	_, err = primary.Read(buf)
	if !errors.Is(err, ErrPeerClosed) || !errors.Is(err, io.EOF) {
		t.Fatalf("Read after peer Close = %v, want ErrPeerClosed (io.EOF)", err)
	}
	if _, err := primary.Write([]byte("x")); !errors.Is(err, ErrPeerClosed) {
		t.Fatalf("Write after peer Close = %v, want ErrPeerClosed", err)
	}
	if got := primary.PeerState(); got != PeerClosed {
		t.Fatalf("PeerState = %v, want closed", got)
	}
}

func TestPeerGoneAfterCrash(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 8, 64, fastLiveness)
	simulateCrash(secondary)

	start := time.Now()
	_, err := primary.Read(make([]byte, 64))
	if !errors.Is(err, ErrPeerGone) {
		t.Fatalf("Read with crashed peer = %v, want ErrPeerGone", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("crash detected after %v", d)
	}
	if got := primary.PeerState(); got != PeerGone {
		t.Fatalf("PeerState = %v, want gone", got)
	}
}

func TestPeerNotConnectedDoesNotFailReads(t *testing.T) {
	mem := AlignedBuffer(SizeStandardLink(4, 64))
	primary, err := OpenStandardLinkMemory(mem, 4, 64, fastLiveness)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	if got := primary.PeerState(); got != PeerNotConnected {
		t.Fatalf("PeerState = %v, want not-connected", got)
	}
	primary.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := primary.Read(make([]byte, 64)); err != ErrTimeout {
		t.Fatalf("Read without peer = %v, want ErrTimeout (no peer failure before the peer attached)", err)
	}
}

func TestStaleSegmentIsReinitialized(t *testing.T) {
	mem, primary, secondary := newMemoryPair(t, 4, 64, fastLiveness)
	if _, err := primary.Write([]byte("stale")); err != nil {
		t.Fatal(err)
	}
	oldEpoch := primary.epoch
	simulateCrash(primary)
	simulateCrash(secondary)
	time.Sleep(200 * time.Millisecond)

	fresh, err := OpenStandardLinkMemory(mem, 4, 64, fastLiveness)
	if err != nil {
		t.Fatalf("open over stale segment: %v", err)
	}
	defer fresh.Close()
	if fresh.Mode() != LinkModePrimary {
		t.Fatalf("mode over stale segment = %v, want primary", fresh.Mode())
	}
	if fresh.epoch != oldEpoch+1 {
		t.Fatalf("epoch = %d, want %d", fresh.epoch, oldEpoch+1)
	}
	peer, err := OpenStandardLinkMemory(mem, 4, 64, fastLiveness)
	if err != nil {
		t.Fatalf("attach to re-initialized segment: %v", err)
	}
	defer peer.Close()

	peer.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := peer.Read(make([]byte, 64)); err != ErrTimeout {
		t.Fatalf("stale data survived re-initialization: %v", err)
	}
	if _, err := fresh.Write([]byte("fresh")); err != nil {
		t.Fatal(err)
	}
	peer.SetReadDeadline(time.Time{})
	buf := make([]byte, 64)
	if n, err := peer.Read(buf); err != nil || string(buf[:n]) != "fresh" {
		t.Fatalf("Read on re-initialized segment = %q, %v", buf[:n], err)
	}

	// The old endpoints notice that they lost their segment.
	if _, err := secondary.Read(buf); !errors.Is(err, ErrPeerGone) {
		t.Fatalf("old endpoint Read = %v, want ErrPeerGone", err)
	}
}

func TestGracefullyClosedSegmentIsReusable(t *testing.T) {
	mem := AlignedBuffer(SizeStandardLink(4, 64))
	for round := 0; round < 3; round++ {
		a, err := OpenStandardLinkMemory(mem, 4, 64, LinkOptions{})
		if err != nil {
			t.Fatalf("round %d open a: %v", round, err)
		}
		b, err := OpenStandardLinkMemory(mem, 4, 64, LinkOptions{})
		if err != nil {
			t.Fatalf("round %d open b: %v", round, err)
		}
		if a.Mode() != LinkModePrimary || b.Mode() != LinkModeSecondary {
			t.Fatalf("round %d modes = %v/%v", round, a.Mode(), b.Mode())
		}
		msg := fmt.Sprintf("round-%d", round)
		if _, err := a.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		if n, err := b.Read(buf); err != nil || string(buf[:n]) != msg {
			t.Fatalf("round %d Read = %q, %v", round, buf[:n], err)
		}
		b.Close()
		a.Close()
	}
}

func TestThirdEndpointGetsLinkBusy(t *testing.T) {
	mem, _, _ := newMemoryPair(t, 4, 64, LinkOptions{})
	_, err := OpenStandardLinkMemory(mem, 4, 64, LinkOptions{})
	if !errors.Is(err, ErrLinkBusy) || !errors.Is(err, ErrFailedInit) {
		t.Fatalf("third endpoint = %v, want ErrLinkBusy", err)
	}
}

func TestOpenStandardLinkMemoryValidation(t *testing.T) {
	size := SizeStandardLink(4, 64)
	mem := AlignedBuffer(size + pagesize)
	if _, err := OpenStandardLinkMemory(mem[:size-1], 4, 64, LinkOptions{}); err != ErrMemorySmall {
		t.Fatalf("short memory = %v, want ErrMemorySmall", err)
	}
	if _, err := OpenStandardLinkMemory(mem[1:], 4, 64, LinkOptions{}); err != ErrMemoryAlign {
		t.Fatalf("misaligned memory = %v, want ErrMemoryAlign", err)
	}
	if _, err := OpenStandardLinkMemory(mem, 4, 12, LinkOptions{}); err != ErrInvalidSize {
		t.Fatalf("unaligned bufferSize = %v, want ErrInvalidSize", err)
	}
	if _, err := OpenStandardLinkMemory(nil, 4, 64, LinkOptions{}); err != ErrMemorySmall {
		t.Fatalf("nil memory = %v, want ErrMemorySmall", err)
	}
	backing, offset := createAlignedBuffer(t, size)
	defer func() { _ = backing }()
	if _, err := OpenStandardLink(offset, 4, 12); err != ErrInvalidSize {
		t.Fatalf("OpenStandardLink unaligned bufferSize = %v, want ErrInvalidSize", err)
	}
}

func TestDeadlineChangeWakesBlockedRead(t *testing.T) {
	_, _, secondary := newMemoryPair(t, 4, 64, LinkOptions{})
	errCh := make(chan error, 1)
	go func() {
		_, err := secondary.Read(make([]byte, 64))
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	secondary.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	err := waitErr(t, errCh, time.Second)
	var netErr net.Error
	if err != ErrTimeout || !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("Read after deadline change = %v, want ErrTimeout (net.Error timeout)", err)
	}
}

func TestConcurrentDeadlinesAndReads(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 16, 64, LinkOptions{})
	const messages = 2000
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				secondary.SetReadDeadline(time.Now().Add(time.Second))
				secondary.SetWriteDeadline(time.Now().Add(time.Second))
				primary.WriteTo(nil, &HQQAddr{LinkID: "x"})
			}
		}
	}()
	go func() {
		for i := 0; i < messages; i++ {
			primary.Write([]byte{byte(i)})
		}
	}()
	buf := make([]byte, 64)
	for i := 0; i < messages; i++ {
		n, _, err := secondary.ReadFrom(buf)
		if err != nil || n != 1 || buf[0] != byte(i) {
			t.Fatalf("ReadFrom(%d) = %d %v %v", i, n, buf[:n], err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestConcurrentReadersReceiveEachMessageOnce(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 64, 64, LinkOptions{})
	const (
		readers  = 4
		writers  = 4
		perWrite = 1000
	)
	var seen sync.Map
	var count atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		go func(w int) {
			for i := 0; i < perWrite; i++ {
				primary.Write([]byte(fmt.Sprintf("%d-%d", w, i)))
			}
		}(w)
	}
	errCh := make(chan error, readers)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 64)
			for count.Load() < writers*perWrite {
				secondary.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				n, err := secondary.Read(buf)
				if err == ErrTimeout {
					continue
				}
				if err != nil {
					errCh <- err
					return
				}
				if _, dup := seen.LoadOrStore(string(buf[:n]), true); dup {
					errCh <- fmt.Errorf("duplicate message %q", buf[:n])
					return
				}
				count.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := count.Load(); got != writers*perWrite {
		t.Fatalf("received %d messages, want %d", got, writers*perWrite)
	}
}

func TestPartialReadRemainderReusesBuffer(t *testing.T) {
	_, primary, secondary := newMemoryPair(t, 4, 64, LinkOptions{})
	payload := bytes.Repeat([]byte("abcdefgh"), 8)
	small := make([]byte, 16)
	rest := make([]byte, 64)
	roundTrip := func() {
		if _, err := primary.Write(payload); err != nil {
			t.Fatal(err)
		}
		if n, err := secondary.Read(small); err != nil || n != 16 {
			t.Fatalf("partial Read = %d, %v", n, err)
		}
		if n, err := secondary.Read(rest); err != nil || n != 48 {
			t.Fatalf("tail Read = %d, %v", n, err)
		}
	}
	roundTrip() // warm up: allocates the remainder buffer once
	if allocs := testing.AllocsPerRun(100, roundTrip); allocs != 0 {
		t.Fatalf("partial read path allocates %.1f times per message, want 0", allocs)
	}

	// Zero-copy and reservation consumers borrow and return the buffer.
	primary.Write(payload)
	secondary.Read(small)
	if n, err := secondary.ReadZeroCopy(func(b []byte) error {
		if !bytes.Equal(b, payload[16:]) {
			return fmt.Errorf("remainder = %q", b)
		}
		return nil
	}); err != nil || n != 48 {
		t.Fatalf("ReadZeroCopy remainder = %d, %v", n, err)
	}
	primary.Write(payload)
	secondary.Read(small)
	res, err := secondary.ReserveRead()
	if err != nil || !bytes.Equal(res.Buffer(), payload[16:]) {
		t.Fatalf("ReserveRead remainder = %q, %v", res.Buffer(), err)
	}
	if err := res.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestStandardReadsRejectAdvancedFrames(t *testing.T) {
	mem := AlignedBuffer(SizeStandardLink(8, 16))
	adv, err := NewAdvancedLinkMemory(mem, 8, 16, AdvancedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer adv.Close()
	std, err := OpenStandardLinkMemory(mem, 8, 16, LinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer std.Close()
	adv.setNegotiated(CurrentProtocolVersion, supportedFeatures, 0)

	ctx := context.Background()
	for _, read := range []func() error{
		func() error { _, err := std.Read(make([]byte, 64)); return err },
		func() error { _, err := std.ReadZeroCopy(func([]byte) error { return nil }); return err },
		func() error { _, err := std.ReserveRead(); return err },
	} {
		if _, err := adv.SendLarge(ctx, 0, []byte("tiny")); err != nil {
			t.Fatal(err)
		}
		if err := read(); !errors.Is(err, ErrUnexpectedPacket) || !errors.Is(err, ErrInvalidOp) {
			t.Fatalf("standard read of an Advanced frame = %v, want ErrUnexpectedPacket", err)
		}
	}
	// The ring keeps working after rejected frames.
	if _, err := adv.slink.Write([]byte("plain")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if n, err := std.Read(buf); err != nil || string(buf[:n]) != "plain" {
		t.Fatalf("Read after rejected frames = %q, %v", buf[:n], err)
	}
}

func newNegotiatedPair(t testing.TB, bufferCount, bufferSize int, primaryOpts, secondaryOpts AdvancedOptions) (*AdvancedLink, *AdvancedLink) {
	t.Helper()
	mem := AlignedBuffer(SizeStandardLink(bufferCount, bufferSize))
	primary, err := NewAdvancedLinkMemory(mem, bufferCount, bufferSize, primaryOpts)
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := NewAdvancedLinkMemory(mem, bufferCount, bufferSize, secondaryOpts)
	if err != nil {
		primary.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		secondary.Close()
		primary.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- primary.Negotiate(ctx, 0) }()
	go func() { errCh <- secondary.Negotiate(ctx, 0) }()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("Negotiate: %v", err)
		}
	}
	return primary, secondary
}

func TestNegotiateChoosesRolesAutomatically(t *testing.T) {
	primary, secondary := newNegotiatedPair(t, 8, 32, AdvancedOptions{}, AdvancedOptions{})
	for _, l := range []*AdvancedLink{primary, secondary} {
		if l.GetProtocolState() != ProtocolStateNegotiated || !l.IsLargeCopyEnabled() || !l.IsRequestResponseEnabled() {
			t.Fatalf("%v: state=%v features=%#x", l.Mode(), l.GetProtocolState(), l.GetNegotiatedFeatures())
		}
	}
	if err := primary.Negotiate(context.Background(), 0); !errors.Is(err, ErrAlreadyNegotiated) {
		t.Fatalf("second Negotiate = %v, want ErrAlreadyNegotiated", err)
	}
	if _, err := primary.Write([]byte("x")); !errors.Is(err, ErrAdvancedActive) {
		t.Fatalf("Standard Write after negotiation = %v, want ErrAdvancedActive", err)
	}
	if _, err := secondary.Read(make([]byte, 8)); !errors.Is(err, ErrAdvancedActive) {
		t.Fatalf("Standard Read after negotiation = %v, want ErrAdvancedActive", err)
	}
}

func TestNegotiateFeatureIntersection(t *testing.T) {
	mem := AlignedBuffer(SizeStandardLink(8, 32))
	primary, _ := NewAdvancedLinkMemory(mem, 8, 32, AdvancedOptions{})
	secondary, _ := NewAdvancedLinkMemory(mem, 8, 32, AdvancedOptions{})
	defer primary.Close()
	defer secondary.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- secondary.Negotiate(ctx, FeatureLargeCopy) }()
	if err := primary.Negotiate(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if primary.IsRequestResponseEnabled() || !primary.IsLargeCopyEnabled() {
		t.Fatalf("negotiated features = %#x, want only FeatureLargeCopy", primary.GetNegotiatedFeatures())
	}
	if _, err := primary.Call(ctx, 1, nil); !errors.Is(err, ErrNotNegotiated) {
		t.Fatalf("Call without FeatureRequestResponse = %v, want ErrNotNegotiated", err)
	}
}

func TestSimultaneousNegotiateProtocolDoesNotDeadlock(t *testing.T) {
	mem := AlignedBuffer(SizeStandardLink(8, 32))
	primary, _ := NewAdvancedLinkMemory(mem, 8, 32, AdvancedOptions{})
	secondary, _ := NewAdvancedLinkMemory(mem, 8, 32, AdvancedOptions{})
	defer primary.Close()
	defer secondary.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errCh := make(chan error, 2)
	for _, l := range []*AdvancedLink{primary, secondary} {
		go func(l *AdvancedLink) {
			ok, err := l.NegotiateProtocol(ctx, CurrentProtocolVersion, supportedFeatures)
			if err == nil && !ok {
				err = errors.New("negotiation not ok")
			}
			errCh <- err
		}(l)
	}
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("simultaneous NegotiateProtocol: %v", err)
		}
	}
	// The link is usable afterwards (the Secondary's own request must not
	// be left behind as garbage in the Primary's receive direction).
	go func() {
		req, err := secondary.Receive(ctx)
		if err == nil {
			secondary.Respond(ctx, req, 0, []byte("pong"))
		}
	}()
	if resp, err := primary.Call(ctx, 1, []byte("ping")); err != nil || string(resp) != "pong" {
		t.Fatalf("Call after simultaneous negotiation = %q, %v", resp, err)
	}
}

func TestCanceledCallLateResponseIsDiscarded(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 32, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	callCtx, callCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer callCancel()
	requests := make(chan AdvancedMessage, 1)
	go func() {
		req, err := server.Receive(ctx)
		if err == nil {
			requests <- req
		}
	}()
	if _, err := client.Call(callCtx, 5, []byte("slow")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call = %v, want deadline exceeded", err)
	}
	req := <-requests
	if err := server.Respond(ctx, req, 0, []byte("too late")); err != nil {
		t.Fatal(err)
	}

	recvCtx, recvCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer recvCancel()
	if msg, err := client.Receive(recvCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Receive got %+v, %v; the late response must be discarded", msg, err)
	}
	if got := client.GetAdvancedStats().AbandonedResponses; got != 1 {
		t.Fatalf("AbandonedResponses = %d, want 1", got)
	}
}

func TestOverflowDropKeepsResponsesFlowing(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 32,
		AdvancedOptions{IncomingQueueSize: 1, IncomingOverflow: OverflowDrop}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		req, err := server.Receive(ctx)
		if err != nil {
			return
		}
		for i := 0; i < 3; i++ {
			server.SendLarge(ctx, 0, []byte("unsolicited"))
		}
		server.Respond(ctx, req, 0, []byte("answer"))
	}()
	resp, err := client.Call(ctx, 1, []byte("question"))
	if err != nil || string(resp) != "answer" {
		t.Fatalf("Call = %q, %v", resp, err)
	}
	if got := client.GetAdvancedStats().DroppedMessages; got != 2 {
		t.Fatalf("DroppedMessages = %d, want 2", got)
	}
	msg, err := client.Receive(ctx)
	if err != nil || string(msg.Payload) != "unsolicited" {
		t.Fatalf("queued message = %q, %v", msg.Payload, err)
	}
}

func TestOverflowFailStopsDispatcher(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 32,
		AdvancedOptions{IncomingQueueSize: 1, IncomingOverflow: OverflowFail}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		if _, err := server.Receive(ctx); err == nil {
			server.SendLarge(ctx, 0, []byte("one"))
			server.SendLarge(ctx, 0, []byte("two"))
		}
	}()
	if _, err := client.Call(ctx, 1, nil); !errors.Is(err, ErrIncomingOverflow) {
		t.Fatalf("Call = %v, want ErrIncomingOverflow", err)
	}
}

func TestMuxServe(t *testing.T) {
	client, server := newNegotiatedPair(t, 16, 32, AdvancedOptions{}, AdvancedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var messages atomic.Int32
	mux := NewMux()
	mux.HandleFunc(1, func(_ context.Context, req AdvancedMessage) ([]byte, error) {
		return append([]byte("echo:"), req.Payload...), nil
	})
	mux.HandleFunc(2, func(context.Context, AdvancedMessage) ([]byte, error) {
		return nil, AdvancedResponseError{Status: 404, Payload: []byte("not here")}
	})
	mux.HandleFunc(3, func(context.Context, AdvancedMessage) ([]byte, error) {
		return nil, errors.New("boom")
	})
	mux.OnMessage = func(context.Context, AdvancedMessage) { messages.Add(1) }

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ctx, mux) }()

	large := bytes.Repeat([]byte("x"), 1000)
	if resp, err := client.Call(ctx, 1, large); err != nil || !bytes.Equal(resp, append([]byte("echo:"), large...)) {
		t.Fatalf("method 1 = %d bytes, %v", len(resp), err)
	}
	var re AdvancedResponseError
	if _, err := client.Call(ctx, 2, nil); !errors.As(err, &re) || re.Status != 404 || string(re.Payload) != "not here" {
		t.Fatalf("method 2 = %v", err)
	}
	if !strings.Contains(re.Error(), "404") {
		t.Fatalf("AdvancedResponseError.Error() = %q, want the status", re.Error())
	}
	if _, err := client.Call(ctx, 3, nil); !errors.As(err, &re) || re.Status != uint64(ErrCodeInvalidOp) || string(re.Payload) != "boom" {
		t.Fatalf("method 3 = %v", err)
	}
	if _, err := client.Call(ctx, 99, nil); !errors.As(err, &re) || re.Status != uint64(ErrCodeInvalidOp) {
		t.Fatalf("unknown method = %v", err)
	}

	// Concurrent calls are served concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("echo:%d", i)
			if resp, err := client.Call(ctx, 1, []byte(fmt.Sprint(i))); err != nil || string(resp) != want {
				t.Errorf("concurrent call %d = %q, %v", i, resp, err)
			}
		}(i)
	}
	wg.Wait()

	if _, err := client.SendLarge(ctx, 0, []byte("note")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if messages.Load() != 1 {
		t.Fatalf("OnMessage called %d times, want 1", messages.Load())
	}

	client.Close()
	if err := waitErr(t, serveErr, 2*time.Second); err != nil {
		t.Fatalf("Serve after peer Close = %v, want nil", err)
	}
}

func TestListenDial(t *testing.T) {
	name := newPortableSharedMemoryName()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := Config{BufferCount: 16, BufferSize: 64}

	type result struct {
		link *NamedAdvancedLink
		err  error
	}
	dialed := make(chan result, 1)
	go func() {
		// Dial first: it must wait for the listener to appear.
		l, err := Dial(ctx, name, cfg)
		dialed <- result{l, err}
	}()
	time.Sleep(20 * time.Millisecond)
	server, err := Listen(ctx, name, cfg)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	r := <-dialed
	if r.err != nil {
		server.Close()
		t.Fatalf("Dial: %v", r.err)
	}
	client := r.link

	go server.Serve(ctx, HandlerFunc(func(_ context.Context, req AdvancedMessage) ([]byte, error) {
		return bytes.ToUpper(req.Payload), nil
	}))
	payload := bytes.Repeat([]byte("hqq"), 100) // larger than BufferSize
	resp, err := client.Call(ctx, 1, payload)
	if err != nil || !bytes.Equal(resp, bytes.ToUpper(payload)) {
		t.Fatalf("Call = %d bytes, %v", len(resp), err)
	}
	if _, err := Listen(ctx, name, cfg); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second Listen = %v, want os.ErrExist", err)
	}

	client.Close()
	server.Close()
	if _, err := OpenNamedAdvancedLink(name, 16, 64); err == nil {
		t.Fatal("segment name still exists after the listener closed")
	}
}

func TestDialTimesOutWithoutListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Dial(ctx, newPortableSharedMemoryName(), Config{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial without listener = %v, want deadline exceeded", err)
	}
}

func TestUnlinkRemovesStaleName(t *testing.T) {
	name := newPortableSharedMemoryName()
	link, err := CreateStandardLink(name, 4, 64, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crashed creator: the mapping is released without unlinking.
	link.unlinkOnClose = false
	link.Close()

	// On Windows the mapping died with its last handle, so this create
	// succeeds; close it, or the leaked handle keeps the name alive.
	if dup, err := CreateStandardLink(name, 4, 64, 0o600); err == nil {
		dup.Close()
		if !isWindows() {
			t.Fatal("CreateStandardLink succeeded over an existing name")
		}
	}
	if err := Unlink(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Unlink: %v", err)
	}
	again, err := CreateStandardLink(name, 4, 64, 0o600)
	if err != nil {
		t.Fatalf("CreateStandardLink after Unlink: %v", err)
	}
	again.Close()
}

func TestInvalidSharedMemoryNames(t *testing.T) {
	for _, name := range []string{"", "/", "..", "/../etc/passwd", "a/b", "a\\b", "x\x00y"} {
		if _, err := CreateStandardLink(name, 4, 64, 0o600); !errors.Is(err, ErrInvalidName) {
			t.Errorf("CreateStandardLink(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	if err := Unlink("../x"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Unlink(../x) = %v, want ErrInvalidName", err)
	}
}

func TestErrorCompatibility(t *testing.T) {
	checks := []struct {
		err, base error
	}{
		{ErrClosed, net.ErrClosed},
		{ErrPeerClosed, io.EOF},
		{ErrPeerGone, ErrConnNotFound},
		{ErrLinkBusy, ErrFailedInit},
		{ErrNotNegotiated, ErrInvalidOp},
		{ErrUnexpectedPacket, ErrInvalidOp},
		{ErrIncomingOverflow, ErrBufferOverflow},
		{ErrTimeout, os.ErrDeadlineExceeded},
	}
	for _, c := range checks {
		if !errors.Is(c.err, c.base) {
			t.Errorf("errors.Is(%v, %v) = false", c.err, c.base)
		}
	}
	if err := errorFromCode(0x99); err == nil || !strings.Contains(err.Error(), "0x99") {
		t.Errorf("unknown error code = %v", err)
	}
}

func isWindows() bool {
	return os.PathSeparator == '\\'
}
