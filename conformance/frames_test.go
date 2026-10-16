package conformance

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/internal/protocol"
)

// Geometry of the frame matrix images: 16 slots of 64 bytes, enough for the
// negotiate frame, a row's frames and the sentinel.
const (
	frameCount = 16
	frameSize  = 64
	// frameSentinelID is the message ID of the sentinel that proves the
	// receive path survived a row.
	frameSentinelID = 0x7e57
	// frameWait bounds one step of a row (a delivery, a call, a handler).
	frameWait = 5 * time.Second
)

var frameSentinelPayload = []byte("sentinel")

// sentinelFrame returns the sentinel packet of a row: a data message for
// recv and call rows, a request with method 0x5e for serve rows.
func sentinelFrame(target string) ringPacket {
	if target == "serve" {
		return ringPacket{
			pkt:     protocol.NewConnCopyPacket(0, frameSentinelID, uint64(len(frameSentinelPayload)), 0, uint64(len(frameSentinelPayload)), flagBegin|flagEnd|flagRequest, 0x5e),
			payload: frameSentinelPayload,
		}
	}
	return ringPacket{
		pkt:     protocol.NewConnCopyPacket(0, frameSentinelID, uint64(len(frameSentinelPayload)), 0, uint64(len(frameSentinelPayload)), flagBegin|flagEnd, 0),
		payload: frameSentinelPayload,
	}
}

// TestAdvancedFrameMatrix (G-10) runs every row of the frame matrix on a
// fresh in-memory image: ring 0 holds the row's frames, a Secondary joins,
// negotiates on the first frame (the OpProtoAck on ring 1 is checked), and
// the row's target (Recv, Call or Serve) must produce the expected deliveries,
// error or drop. A sentinel message after the frames must still arrive when
// the row says so.
func TestAdvancedFrameMatrix(t *testing.T) {
	m := loadManifest(t)
	if len(m.Frames) == 0 {
		t.Fatal("manifest has no frame rows")
	}
	for _, row := range m.Frames {
		t.Run(row.ID, func(t *testing.T) {
			t.Parallel()
			runFrameRow(t, row)
		})
	}
}

// runFrameRow executes one row of the frame matrix.
func runFrameRow(t *testing.T, row FrameRow) {
	t.Helper()
	ring := make([]ringPacket, 0, len(row.Frames)+1)
	for _, f := range row.Frames {
		ring = append(ring, ringPacket{pkt: packetOf(t, f.Packet), payload: mustDecodeHex(t, f.Payload)})
	}
	if row.Sentinel {
		ring = append(ring, sentinelFrame(row.Target))
	}
	image := liveImage(t, frameCount, frameSize, ring)
	mem := callerMemory(t, frameCount, frameSize, image)
	link, err := hqq.NewAdvancedLink(mem, frameCount, frameSize, hqq.AdvancedOptions{
		MaxReassemblyBytes: row.MaxReassemblyBytes,
		Link:               testOpts(),
	})
	if err != nil {
		t.Fatalf("NewAdvancedLink: %v", err)
	}
	t.Cleanup(func() { _ = link.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), frameWait)
	defer cancel()
	if err := link.Negotiate(ctx, 0); err != nil {
		t.Fatalf("negotiate: %v", err)
	}
	checkAck(t, mem, row)

	switch row.Target {
	case "recv":
		runRecvRow(t, ctx, link, row)
	case "call":
		runCallRow(t, ctx, link, row)
	case "serve":
		runServeRow(t, ctx, link, row, mem, len(ring))
	default:
		t.Fatalf("unknown target %q", row.Target)
	}
}

// checkAck reads the OpProtoAck the secondary published on ring 1 (slot 0):
// version 1.1 and the negotiated features of the row.
func checkAck(t *testing.T, mem []byte, row FrameRow) {
	t.Helper()
	reg, _ := ComputeRegions(hostPage(), frameCount, frameSize)
	if seq := segmentWord(mem, reg.Ring1+RingHeaderSize+RingSlotSeq); seq != 1 {
		t.Fatalf("ring 1 slot 0 sequence %d after negotiation, want 1 (one ack)", seq)
	}
	var ack protocol.Packet
	copy(ack[:], mem[reg.Ring1+RingHeaderSize+RingSlotPacket:])
	if ack.Op() != protocol.OpProtoAck {
		t.Fatalf("ring 1 slot 0 opcode %#x, want OpProtoAck", ack.Op())
	}
	if v := ack.Operand(0); v != protoVersion11 {
		t.Errorf("ack version %#x, want %#x", v, protoVersion11)
	}
	if f := ack.Operand(1); f != row.AckFeatures {
		t.Errorf("ack features %#x, want %#x", f, row.AckFeatures)
	}
}

// checkDelivery compares the payload of one delivery.
func checkDelivery(t *testing.T, i int, d Delivery, got []byte) {
	t.Helper()
	if h := hexOf(got); h != d.Payload {
		t.Errorf("delivery %d (%s): payload %s, want %s", i, d.Kind, h, d.Payload)
	}
}

// runRecvRow drives a row through Recv (data messages and the sentinel).
func runRecvRow(t *testing.T, ctx context.Context, link *hqq.AdvancedLink, row FrameRow) {
	t.Helper()
	if row.Effect == "fail" {
		msg, err := link.Recv(ctx)
		msg.Release()
		if !matchesKind(err, row.Error) {
			t.Fatalf("Recv error %v, want %s", err, row.Error)
		}
		return
	}
	for i, d := range row.Deliver {
		if d.Kind != "data" {
			t.Fatalf("delivery %d of a recv row has kind %q", i, d.Kind)
		}
		msg, err := link.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
		checkDelivery(t, i, d, msg.Bytes())
		msg.Release()
	}
	if row.Sentinel {
		checkSentinelRecv(t, ctx, link)
	}
}

// checkSentinelRecv checks that the next data message is the sentinel.
func checkSentinelRecv(t *testing.T, ctx context.Context, link *hqq.AdvancedLink) {
	t.Helper()
	msg, err := link.Recv(ctx)
	if err != nil {
		t.Fatalf("sentinel Recv: %v", err)
	}
	if !bytes.Equal(msg.Bytes(), frameSentinelPayload) {
		t.Errorf("sentinel: got %q, want %q (an extra or missing delivery)", msg.Bytes(), frameSentinelPayload)
	}
	msg.Release()
}

// runCallRow drives a row through Call: each response delivery is the result
// of one Call of message 1 (the first message ID of the link).
func runCallRow(t *testing.T, ctx context.Context, link *hqq.AdvancedLink, row FrameRow) {
	t.Helper()
	if row.Effect == "fail" {
		resp, err := link.Call(ctx, 1, []byte("ping"))
		if resp != nil {
			resp.Release()
		}
		if !matchesKind(err, row.Error) {
			t.Fatalf("Call error %v, want %s", err, row.Error)
		}
		if row.Error == "ResponseError" {
			var re hqq.ResponseError
			if errors.As(err, &re) && re.Status != 42 {
				t.Errorf("ResponseError status %d, want 42", re.Status)
			}
		}
	}
	for i, d := range row.Deliver {
		if d.Kind != "response" {
			t.Fatalf("delivery %d of a call row has kind %q", i, d.Kind)
		}
		resp, err := link.Call(ctx, 1, []byte("ping"))
		if err != nil {
			t.Fatalf("Call %d: %v", i, err)
		}
		checkDelivery(t, i, d, resp.Bytes())
		resp.Release()
	}
	if row.Sentinel {
		checkSentinelRecv(t, ctx, link)
	}
}

// requestRecord is one request a Serve handler received.
type requestRecord struct {
	payload []byte
	method  uint64
	caller  uint64
	trace   string
}

// runServeRow drives a row through Serve. The handler records every request.
// For rows with late frames, the test publishes them into ring 0 once the
// first request's handler has started, and then expects the handler's
// context to be canceled.
func runServeRow(t *testing.T, ctx context.Context, link *hqq.AdvancedLink, row FrameRow, mem []byte, firstFree int) {
	t.Helper()
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	recorded := make(chan requestRecord, 16)
	canceled := make(chan struct{}, 16)
	waitForCancel := len(row.LateFrames) > 0
	handler := hqq.HandlerFunc(func(hctx context.Context, _ *hqq.ResponseWriter, req *hqq.Request) {
		recorded <- requestRecord{
			payload: append([]byte(nil), req.Payload()...),
			method:  req.Method(),
			caller:  req.Caller(),
			trace:   traceHex(req.Trace()),
		}
		if waitForCancel {
			select {
			case <-hctx.Done():
				canceled <- struct{}{}
			case <-time.After(frameWait):
			}
		}
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- link.Serve(serveCtx, handler) }()

	if row.Effect == "fail" {
		select {
		case err := <-serveErr:
			if !matchesKind(err, row.Error) {
				t.Fatalf("Serve error %v, want %s", err, row.Error)
			}
		case <-time.After(frameWait):
			t.Fatal("Serve did not fail")
		}
		return
	}
	// Handlers run concurrently (Serve runs up to bufferCount of them), so
	// requests are matched by payload, not by arrival order.
	pending := append([]Delivery(nil), row.Deliver...)
	for _, d := range pending {
		if d.Kind != "request" {
			t.Fatalf("delivery of a serve row has kind %q", d.Kind)
		}
	}
	sentinelPending := row.Sentinel
	lateSent := false
	total := len(row.Deliver)
	if row.Sentinel {
		total++
	}
	for range total {
		got := nextRequest(t, recorded)
		if sentinelPending && bytes.Equal(got.payload, frameSentinelPayload) {
			sentinelPending = false
			if got.method != 0x5e {
				t.Errorf("sentinel request: method %#x, want 0x5e", got.method)
			}
			continue
		}
		idx := slices.IndexFunc(pending, func(d Delivery) bool { return hexOf(got.payload) == d.Payload })
		if idx < 0 {
			t.Errorf("unexpected request payload %q (method %#x)", got.payload, got.method)
			continue
		}
		d := pending[idx]
		pending = slices.Delete(pending, idx, idx+1)
		if got.method != d.Method || got.caller != d.Caller || got.trace != d.Trace {
			t.Errorf("delivery %s: method %d caller %#x trace %s; want method %d caller %#x trace %s",
				d.Payload, got.method, got.caller, got.trace, d.Method, d.Caller, d.Trace)
		}
		if waitForCancel && !lateSent {
			lateSent = true
			publishLate(t, mem, row.LateFrames, firstFree)
			select {
			case <-canceled:
			case <-time.After(frameWait):
				t.Fatal("the late cancel did not cancel the running handler")
			}
		}
	}
	select {
	case <-recorded:
		t.Errorf("an unexpected request was delivered to the handler")
	default:
	}
	stop()
	if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Serve returned %v after the row", err)
	}
}

// nextRequest returns the next recorded request or fails the test.
func nextRequest(t *testing.T, recorded <-chan requestRecord) requestRecord {
	t.Helper()
	select {
	case r := <-recorded:
		return r
	case <-time.After(frameWait):
		t.Fatal("no request delivered to the handler")
	}
	return requestRecord{}
}

// traceHex returns a request's trace as hex, "" when it is all zero (absent).
func traceHex(tr [16]byte) string {
	if tr == ([16]byte{}) {
		return ""
	}
	return hexOf(tr[:])
}

// publishLate publishes frames into ring 0 at the positions after the
// negotiation frame and the row's frames, as a producer would: packet and
// payload first, then the slot sequence, then the write cursor (SPEC 7.4).
// The test is the only producer of the row's ring.
func publishLate(t *testing.T, mem []byte, late []Frame, first int) {
	t.Helper()
	reg, _ := ComputeRegions(hostPage(), frameCount, frameSize)
	for i, f := range late {
		pos := uint64(first + i)
		slot := int(pos & (frameCount - 1))
		so := reg.Ring0 + RingHeaderSize + slot*RingSlotStride
		pkt := packetOf(t, f.Packet)
		copy(mem[so+RingSlotPacket:so+RingSlotPacket+PacketSize], pkt[:])
		copy(mem[reg.Buf0+slot*frameSize:], mustDecodeHex(t, f.Payload))
		storeWord(mem, so+RingSlotSeq, pos+1)
		storeWord(mem, reg.Ring0+RingW, pos+1)
	}
}
