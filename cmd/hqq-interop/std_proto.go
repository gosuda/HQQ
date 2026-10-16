package main

import (
	"fmt"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
	"gosuda.org/hqq/internal/protocol"
)

// stdReadBound bounds every Read of the crafted-peer receiver, so that a
// receive path that hangs fails the role instead of the watchdog.
const stdReadBound = 5 * time.Second

// X-21: protocol violations from a crafted peer. The creator holds the segment
// as Primary (Go) and writes its ring 0 with a raw producer (SPEC 7.4, claim by
// CAS, publish by a store of seq = pos+1). The opener, a Standard reader, must
// see each violation as the spec classifies it, one Read at a time, and keep
// receiving valid packets afterwards. The sequence written:
//
//	valid(0), opcode 0x42, valid(1), copy DataSize 0, copy DataSize bufferSize+1,
//	OpError code 5, tombstone, valid(2), OpConnCopy, valid(3)
//
// Expected reads: valid 0; ErrUnexpectedPacket; valid 1; ErrInvalidSize;
// ErrInvalidSize; ErrConnNotFound; valid 2 (tombstone skipped); ErrUnexpectedPacket;
// valid 3.
func init() {
	register(&scenario{
		ID: "X-21", Title: "protocol violations from a crafted peer on the Standard receive path", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 60,
		Variants: []variant{
			{Name: "violations", Params: params{BufferCount: 4, BufferSize: 256},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}}}},
		},
		Run: runX21,
	})
}

func runX21(e *env) error {
	if e.cfg.Role == "opener" {
		return runX21Opener(e)
	}
	return runX21Writer(e)
}

// runX21Writer is the creator: a Go Primary plus the raw ring-0 producer.
func runX21Writer(e *env) error {
	link, err := stdCreateSignal(e)
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()
	if err := e.barrier("joined"); err != nil {
		return err
	}
	seg, err := openRawSegment(e.cfg.Name, e.cfg.BufferCount, e.cfg.BufferSize, false)
	if err != nil {
		return err
	}
	defer seg.close()
	ring := seg.ring(0)
	size, seed := e.cfg.Size, e.cfg.Seed
	var copyID uint64
	nextID := func() uint64 { copyID++; return copyID }
	valid := func(seq uint64) error {
		return ring.put(e.ctx, rawPacket{op: protocol.OpStandardLinkCopy, ops: [7]uint64{nextID(), 0, uint64(size)}},
			conformance.Payload(seed, seq, size))
	}
	packets := []func() error{
		func() error { return valid(0) },
		func() error { return ring.put(e.ctx, rawPacket{op: 0x42}, nil) },
		func() error { return valid(1) },
		func() error {
			return ring.put(e.ctx, rawPacket{op: protocol.OpStandardLinkCopy, ops: [7]uint64{nextID(), 0, 0}}, nil)
		},
		func() error {
			return ring.put(e.ctx, rawPacket{op: protocol.OpStandardLinkCopy, ops: [7]uint64{nextID(), 0, uint64(e.cfg.BufferSize + 1)}}, nil)
		},
		func() error { return ring.put(e.ctx, rawPacket{op: protocol.OpError, ops: [7]uint64{0, 5}}, nil) },
		func() error {
			return ring.put(e.ctx, rawPacket{op: protocol.OpStandardLinkTombstone, ops: [7]uint64{nextID(), 0, 0x07}}, nil)
		},
		func() error { return valid(2) },
		func() error {
			return ring.put(e.ctx, rawPacket{op: protocol.OpConnCopy, ops: [7]uint64{0, 1, 1, 0, 1, 0, 0}}, nil)
		},
		func() error { return valid(3) },
	}
	for i, p := range packets {
		if err := p(); err != nil {
			return fmt.Errorf("packet %d: %w", i, err)
		}
		e.res.Sent++
	}
	return e.barrier("done")
}

// runX21Opener is the Standard reader under test.
func runX21Opener(e *env) error {
	link, err := stdOpenAfter(e)
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()
	if err := e.barrier("joined"); err != nil {
		return err
	}
	size, seed := e.cfg.Size, e.cfg.Seed
	buf := make([]byte, e.cfg.BufferSize)
	read := func() (int, error) {
		if err := link.SetReadDeadline(time.Now().Add(stdReadBound)); err != nil {
			return 0, err
		}
		return link.Read(buf)
	}
	expectValid := func(name string, seq uint64) {
		n, err := read()
		ok := err == nil && n == size && conformance.VerifyPayload(seed, seq, buf[:n]) == nil
		if ok {
			e.res.Received++
		}
		e.check(name, ok, errText(err))
	}
	expectErr := func(name string, want error) {
		_, err := read()
		e.checkErr(name, err, want)
	}
	expectValid("valid packet delivered", 0)
	expectErr("unknown opcode 0x42 -> ErrUnexpectedPacket", hqq.ErrUnexpectedPacket)
	expectValid("next valid packet still received after the bad one", 1)
	expectErr("OpStandardLinkCopy DataSize 0 -> ErrInvalidSize", hqq.ErrInvalidSize)
	expectErr("OpStandardLinkCopy DataSize > bufferSize -> ErrInvalidSize", hqq.ErrInvalidSize)
	expectErr("OpError code 5 -> ErrConnNotFound", hqq.ErrConnNotFound)
	expectValid("tombstone skipped silently", 2)
	expectErr("OpConnCopy on a raw Standard read -> ErrUnexpectedPacket", hqq.ErrUnexpectedPacket)
	expectValid("valid packet after the Advanced frame", 3)
	return e.barrier("done")
}
