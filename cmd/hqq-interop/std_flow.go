package main

import (
	"fmt"
	"io"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
)

// X-05: ring full and backpressure. The creator (2 slots) writes two messages,
// its third write with a 200 ms deadline must return ErrTimeout without losing
// data. After the opener drained the two messages, the third one goes through.
func init() {
	register(&scenario{
		ID: "X-05", Title: "ring full / backpressure, deadline on a full ring", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 30,
		Variants: []variant{
			{Name: "ring2", Params: params{BufferCount: 2, BufferSize: 256},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}}}},
		},
		Run: runX05,
	})
}

func runX05(e *env) error {
	size, seed := e.cfg.Size, e.cfg.Seed
	if e.cfg.Role == "creator" {
		link, err := stdCreateSignal(e)
		if err != nil {
			return err
		}
		defer func() { _ = link.Close() }()
		stdBind(e, link.StandardLink)
		for i := range 2 {
			if _, err := link.Write(conformance.Payload(seed, uint64(i), size)); err != nil {
				return fmt.Errorf("message %d: %w", i, err)
			}
			e.res.Sent++
		}
		if err := link.SetWriteDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			return err
		}
		_, err = link.Write(conformance.Payload(seed, 2, size))
		e.checkErr("third write on a full ring times out", err, hqq.ErrTimeout)
		stdBind(e, link.StandardLink)
		if err := e.barrier("filled"); err != nil {
			return err
		}
		if err := e.barrier("drained"); err != nil {
			return err
		}
		if _, err := link.Write(conformance.Payload(seed, 2, size)); err != nil {
			return fmt.Errorf("message 2 after drain: %w", err)
		}
		e.res.Sent++
		return e.barrier("received")
	}

	link, err := stdOpenAfter(e)
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()
	stdBind(e, link.StandardLink)
	if err := e.barrier("filled"); err != nil {
		return err
	}
	buf := make([]byte, size)
	inOrder := true
	for i := range 2 {
		if _, err := io.ReadFull(link, buf); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
		e.res.Received++
		if conformance.VerifyPayload(seed, uint64(i), buf) != nil {
			inOrder = false
		}
	}
	e.check("the two buffered messages arrive in order and intact", inOrder, "")
	if err := e.barrier("drained"); err != nil {
		return err
	}
	if _, err := io.ReadFull(link, buf); err != nil {
		return fmt.Errorf("message 2: %w", err)
	}
	e.res.Received++
	e.check("the third message arrives intact after the drain", conformance.VerifyPayload(seed, 2, buf) == nil, "")
	return e.barrier("received")
}

// X-06: concurrent streams. Each side writes n messages while it reads n
// messages from the peer. The creator's stream uses seed, the opener's seed+1.
// Default 20000 messages; --long uses 100000.
func init() {
	register(&scenario{
		ID: "X-06", Title: "bidirectional stream, sequence and CRC, ring of 2 and 256", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 120,
		Variants: []variant{
			{Name: "ring2", Params: params{BufferCount: 2, BufferSize: 256, Messages: 20000},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}}}},
			{Name: "ring256", Params: params{BufferCount: 256, BufferSize: 256, Messages: 20000},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}}}},
		},
		Run: runX06,
	})
}

func runX06(e *env) error {
	n := e.cfg.Messages
	if e.cfg.Long {
		n = 100000
	}
	var (
		link *hqq.NamedStandardLink
		err  error
	)
	if e.cfg.Role == "creator" {
		link, err = stdCreateSignal(e)
	} else {
		link, err = stdOpenAfter(e)
	}
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()
	stdBind(e, link.StandardLink)
	if err := stdBidirectional(e, link.StandardLink, n); err != nil {
		return err
	}
	return e.barrier("done")
}

// X-12: parked consumer woken by the other implementation's producer. Ping-pong
// over a ring of 2 with 50 ms idle between messages: the consumer parks, and
// every round trip must stay under 250 ms with no lost wake-up.
func init() {
	register(&scenario{
		ID: "X-12", Title: "parked consumer woken by a producer (ping-pong, ring of 2)", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 60,
		Variants: []variant{
			{Name: "ring2", Params: params{BufferCount: 2, BufferSize: 256, Messages: 20},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}}}},
		},
		Run: func(e *env) error {
			return stdPingRun(e, e.cfg.Messages, 50*time.Millisecond, 250*time.Millisecond)
		},
	})
}

// stdPingRun creates or opens the segment and runs a ping-pong of n round trips.
func stdPingRun(e *env, n int, idle, maxRTT time.Duration) error {
	var (
		link *hqq.NamedStandardLink
		err  error
	)
	if e.cfg.Role == "creator" {
		link, err = stdCreateSignal(e)
	} else {
		link, err = stdOpenAfter(e)
	}
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()
	stdBind(e, link.StandardLink)
	if err := stdPingPong(e, link.StandardLink, n, idle, maxRTT); err != nil {
		return err
	}
	return e.barrier("done")
}

// X-13: Windows named semaphores and leftover tokens. Windows only. Step 1 is a
// 1000-message ping-pong (2 ms idle, so the consumer parks between messages).
// Step 2 is a peer kill of the parked opener (see runKillPeer). Step 3 is a
// new session that reuses the name, after the step-2 creator closed, with a
// 100-message ping-pong. Documented limit: the leftover semaphore tokens of
// step 2 are only reachable while the step-2 creator holds the mapping, so the
// reuse checks the name and the semaphores of a fresh session, not the old tokens.
func init() {
	register(&scenario{
		ID: "X-13", Title: "Windows named semaphores: ping-pong, parked kill, session reuse", Kind: "standard",
		Platforms: []string{"windows"}, TimeoutS: 120,
		Variants: []variant{
			{Name: "semaphores", Params: params{BufferCount: 4, BufferSize: 256, Messages: 1000, HeartbeatMs: 50, PeerTimeoutMs: 300},
				Steps: []step{
					{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}},
					{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "crash"},
						Args: map[string][]string{"opener": {"--crash-after", "blocked"}}},
					{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}},
				}},
		},
		Run: runX13,
	})
}

func runX13(e *env) error {
	switch e.cfg.Step {
	case 1:
		return stdPingRun(e, e.cfg.Messages, 2*time.Millisecond, 250*time.Millisecond)
	case 2:
		return runKillPeer(e)
	default:
		return stdPingRun(e, 100, 2*time.Millisecond, 250*time.Millisecond)
	}
}

// X-14: simple-mode peer. Ping-pong with 300 ms idle between messages: the
// parked side wakes at once, and the round trip must stay within 150 ms (the
// 100 ms park bound plus slack). The spin-peer variant runs the opener with
// --wait spin, which never parks (the simple-mode behavior).
func init() {
	steps := []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}}}
	register(&scenario{
		ID: "X-14", Title: "simple-mode peer: round trips with 300 ms idle", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 60,
		Variants: []variant{
			{Name: "adaptive", Params: params{BufferCount: 4, BufferSize: 256, Messages: 5}, Steps: steps},
			{Name: "spin-peer", Params: params{BufferCount: 4, BufferSize: 256, Messages: 5},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"},
					Args: map[string][]string{"opener": {"--wait", "spin"}}}}},
		},
		Run: func(e *env) error {
			return stdPingRun(e, e.cfg.Messages, 300*time.Millisecond, 150*time.Millisecond)
		},
	})
}
