package main

import (
	"fmt"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
	"gosuda.org/hqq/internal/protocol"
)

func init() {
	sizesBase := advBase()
	sizesBase.MaxMessage = 65536
	abortBase := sizesBase
	register(&scenario{
		ID: "X-A02",
		Title: "message sizes (X-16, SPEC 10.4-10.5): sizes 0, 1, bufferSize, bufferSize+1, 3 x bufferSize+7 and " +
			"MaxMessageSize (params max_message 65536, buffer_size 256) arrive intact in order; MaxMessageSize+1 is " +
			"refused locally with ErrBufferOverflow and nothing is sent (a 1-byte sentinel arrives next). " +
			"sender-abort-after-chunk-1: a raw first chunk and an abort frame are dropped, the next message arrives intact. " +
			"Payload stream: creator seed. --long repeats the size list 20 times",
		Kind:      "advanced",
		Platforms: []string{"any"},
		TimeoutS:  30,
		Variants: []variant{
			advVariant("sizes", sizesBase),
			advVariant("sender-abort-after-chunk-1", abortBase),
		},
		Run: runAdvSizes,
	})
}

func runAdvSizes(e *env) error {
	nl, err := advEstablish(e)
	if err != nil {
		return err
	}
	defer nl.Close()
	l := nl.AdvancedLink
	if err := advNegotiateAll(e, l); err != nil {
		return err
	}

	bs := e.cfg.BufferSize
	maxSize := advMaxMessage(e.cfg)
	sizes := []int{0, 1, bs, bs + 1, 3*bs + 7, maxSize}
	if e.cfg.Variant == "sender-abort-after-chunk-1" {
		sizes = []int{3*bs + 7}
	}
	rounds := 1
	if e.cfg.Long {
		rounds = 20
	}
	seed := e.cfg.Seed // the creator's stream

	if e.cfg.Role == "creator" {
		if e.cfg.Variant == "sender-abort-after-chunk-1" {
			if err := advSendAbortedChunk(e); err != nil {
				return err
			}
		}
		seq := uint64(0)
		for range rounds {
			for _, n := range sizes {
				if err := l.Send(e.ctx, conformance.Payload(seed, seq, n)); err != nil {
					return fmt.Errorf("send %d bytes (message %d): %w", n, seq, err)
				}
				seq++
			}
		}
		e.res.Sent = int(seq)
		e.checkErr("MaxMessageSize+1 is refused locally", l.Send(e.ctx, make([]byte, maxSize+1)), hqq.ErrBufferOverflow)
		// Nothing of the refused message was sent: the sentinel takes its sequence number.
		if err := l.Send(e.ctx, conformance.Payload(seed, seq, 1)); err != nil {
			return err
		}
		e.res.Sent = int(seq) + 1
	} else {
		seq := uint64(0)
		for range rounds {
			for _, n := range sizes {
				if err := advRecvCheck(e, l, seed, seq, n); err != nil {
					return err
				}
				seq++
			}
		}
		if err := advRecvCheck(e, l, seed, seq, 1); err != nil {
			return err
		}
		e.res.Received = int(seq) + 1
	}
	return e.barrier("end")
}

// advNegotiateAll negotiates every feature (the scenario uses messages only).
func advNegotiateAll(e *env, l *hqq.AdvancedLink) error {
	return l.Negotiate(e.ctx, hqq.FeatureNone)
}

// advSendAbortedChunk writes the first chunk of a 3 x bufferSize+7 message and
// then its abort frame, through the raw ring writer. The message ID is one
// the Go sender never reaches, so it cannot collide with a real message.
func advSendAbortedChunk(e *env) error {
	w, err := advRawOpen(e)
	if err != nil {
		return err
	}
	defer w.close()
	bs := e.cfg.BufferSize
	const msgID = 1 << 40
	first := protocol.NewConnCopyPacket(0, msgID, uint64(3*bs+7), 0, uint64(bs), hqq.AdvFlagBegin, 0)
	if err := w.send(e.ctx, first, conformance.Payload(e.cfg.Seed, 1<<20, bs)); err != nil {
		return err
	}
	abort := protocol.NewConnCopyPacket(0, msgID, 0, 0, 0, hqq.AdvFlagAbort, 0)
	return w.send(e.ctx, abort, nil)
}
