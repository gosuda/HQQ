package main

import (
	"fmt"
	"time"

	"gosuda.org/hqq/conformance"
)

func init() {
	flood := advBase()
	flood.BufferCount = 2
	flood.Messages = 100
	chunked := flood
	chunked.Size = 3*flood.BufferSize + 7
	register(&scenario{
		ID: "X-A06",
		Title: "flood (X-20, SPEC 10.3): both endpoints send `messages` messages to each other on a 2-slot ring " +
			"while each consumes one message per 1 ms. Both deliver every message intact (sequence and CRC) and finish; " +
			"no deadlock: the receive path never waits for the tx ring. Variants: flood (size = bufferSize), " +
			"flood-chunked (size 3 x bufferSize + 7). --long multiplies messages by 10. Payload stream: creator seed " +
			"for the creator's messages, seed + 1 for the opener's",
		Kind:      "advanced",
		Platforms: []string{"any"},
		TimeoutS:  30,
		Variants: []variant{
			advVariant("flood", flood),
			advVariant("flood-chunked", chunked),
		},
		Run: runAdvFlood,
	})
}

func runAdvFlood(e *env) error {
	nl, err := advEstablish(e)
	if err != nil {
		return err
	}
	defer nl.Close()
	l := nl.AdvancedLink
	if err := l.Negotiate(e.ctx, advAllFeatures); err != nil {
		return err
	}
	n := e.cfg.Messages
	if e.cfg.Long {
		n *= 10
	}
	size := e.cfg.Size
	sendSeed := advSendSeed(e)
	recvSeed := advRecvSeed(e)

	sendErr := make(chan error, 1)
	go func() {
		for i := range n {
			if err := l.Send(e.ctx, conformance.Payload(sendSeed, uint64(i), size)); err != nil {
				sendErr <- fmt.Errorf("send %d: %w", i, err)
				return
			}
		}
		sendErr <- nil
	}()

	bad := 0
	var firstBad string
	for i := range n {
		time.Sleep(time.Millisecond) // slow consumer
		m, err := l.Recv(e.ctx)
		if err != nil {
			return fmt.Errorf("recv %d: %w", i, err)
		}
		if verr := conformance.VerifyPayload(recvSeed, uint64(i), m.Bytes()); verr != nil || m.Len() != size {
			bad++
			if firstBad == "" {
				firstBad = fmt.Sprintf("message %d: len=%d %s", i, m.Len(), errText(verr))
			}
		}
		m.Release()
	}
	e.check(fmt.Sprintf("all %d messages from the peer arrived intact", n), bad == 0, firstBad)
	if err := <-sendErr; err != nil {
		return err
	}
	e.res.Sent, e.res.Received = n, n
	return e.barrier("end")
}
