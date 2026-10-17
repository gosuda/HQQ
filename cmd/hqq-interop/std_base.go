package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
)

// stdSlack is the timing slack added to every bound the scenarios assert.
const stdSlack = 1500 * time.Millisecond

// envExclusive, set to "1" in the environment of the helper, opens every
// Standard endpoint with LinkOptions.Exclusive (the store-based ring
// protocol of SPEC 7.6), so the matrix covers an exclusive Go endpoint
// against CAS and store-based peers. Every Standard scenario uses at most one
// writing and one reading goroutine per link (stdBidirectional runs exactly
// one of each), which is what Exclusive requires. Advanced scenarios ignore
// it: AdvancedLink rejects the option.
const envExclusive = "HQQ_INTEROP_EXCLUSIVE"

// stdOptions maps the configuration to the LinkOptions of a Standard endpoint
// (INTEROP_CLI.md, section 2). A zero field keeps the library default.
func stdOptions(c config) hqq.LinkOptions {
	o := hqq.LinkOptions{AttachTimeout: c.AttachTimeout, Exclusive: os.Getenv(envExclusive) == "1"}
	if c.HeartbeatMs != 0 {
		o.HeartbeatInterval = time.Duration(c.HeartbeatMs) * time.Millisecond
	}
	if c.PeerTimeoutMs > 0 {
		o.PeerTimeout = time.Duration(c.PeerTimeoutMs) * time.Millisecond
	}
	if c.Wait == "spin" {
		o.WaitPolicy = hqq.WaitSpin
	}
	return o
}

// stdCreate creates the configured named segment as Primary.
func stdCreate(e *env) (*hqq.NamedStandardLink, error) {
	return hqq.CreateStandardLink(e.cfg.Name, e.cfg.BufferCount, e.cfg.BufferSize, 0o600, stdOptions(e.cfg))
}

// stdOpen opens the configured named segment as Secondary.
func stdOpen(e *env) (*hqq.NamedStandardLink, error) {
	return hqq.OpenStandardLink(e.cfg.Name, e.cfg.BufferCount, e.cfg.BufferSize, stdOptions(e.cfg))
}

// stdCreateSignal creates the segment and then posts the "created" barrier,
// which makes it openable (INTEROP_CLI.md, section 6).
func stdCreateSignal(e *env) (*hqq.NamedStandardLink, error) {
	l, err := stdCreate(e)
	if err != nil {
		return nil, err
	}
	if err := e.barrier("created"); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// stdOpenAfter waits for the creator's "created" barrier and opens the segment.
func stdOpenAfter(e *env) (*hqq.NamedStandardLink, error) {
	if err := e.barrier("created"); err != nil {
		return nil, err
	}
	return stdOpen(e)
}

// stdCreateRetry creates the segment, retrying while the endpoints of the
// previous session are still alive (ErrLinkBusy): their heartbeats must age
// out before the segment can be reused. It gives up after limit.
func stdCreateRetry(e *env, limit time.Duration) (*hqq.NamedStandardLink, error) {
	deadline := time.Now().Add(limit)
	for {
		l, err := stdCreate(e)
		if !errors.Is(err, hqq.ErrLinkBusy) || time.Now().After(deadline) {
			return l, err
		}
		if err := stdSleep(e, 20*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

// stdBind bounds every blocking call of link by the process deadline, so a
// stalled peer fails this role with an error instead of the watchdog.
func stdBind(e *env, link *hqq.StandardLink) {
	if d, ok := e.ctx.Deadline(); ok {
		_ = link.SetDeadline(d)
	}
}

// stdSleep waits for d or until the process deadline.
func stdSleep(e *env, d time.Duration) error {
	return stdWait(e.ctx, d)
}

func stdWait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// stdPeerBound is the bound of ErrPeerGone detection (SPEC 6.3): the peer
// timeout or four heartbeat intervals, whichever is larger, plus slack.
func stdPeerBound(c config) time.Duration {
	return stdStaleAfter(c) + stdSlack
}

// stdStaleAfter is max(peer timeout, 4 x heartbeat interval) (SPEC 6.3).
func stdStaleAfter(c config) time.Duration {
	peer := stdPeerTimeout(c)
	interval := hqq.DefaultHeartbeatInterval
	if c.HeartbeatMs > 0 {
		interval = time.Duration(c.HeartbeatMs) * time.Millisecond
	}
	return max(peer, 4*interval)
}

// stdPeerTimeout is the peer timeout of the configuration (library default 5 s).
func stdPeerTimeout(c config) time.Duration {
	if c.PeerTimeoutMs > 0 {
		return time.Duration(c.PeerTimeoutMs) * time.Millisecond
	}
	return hqq.DefaultPeerTimeout
}

// stdPingPong runs n round trips over link. The creator sleeps idle, sends
// Payload(seed, i), and waits for the echo; the opener echoes every message.
// The creator checks each echo and that the worst round trip is at most maxRTT.
func stdPingPong(e *env, link *hqq.StandardLink, n int, idle, maxRTT time.Duration) error {
	size, seed := e.cfg.Size, e.cfg.Seed
	buf := make([]byte, size)
	var (
		worst     time.Duration
		verifyErr error
	)
	for i := range n {
		seq := uint64(i)
		if e.cfg.Role == "creator" {
			if err := stdSleep(e, idle); err != nil {
				return err
			}
			t0 := time.Now()
			if _, err := link.Write(conformance.Payload(seed, seq, size)); err != nil {
				return fmt.Errorf("round %d: send: %w", i, err)
			}
			e.res.Sent++
			if _, err := io.ReadFull(link, buf); err != nil {
				return fmt.Errorf("round %d: echo: %w", i, err)
			}
			worst = max(worst, time.Since(t0))
			if err := conformance.VerifyPayload(seed, seq, buf); err != nil && verifyErr == nil {
				verifyErr = fmt.Errorf("round %d: %w", i, err)
			}
			e.res.Received++
			continue
		}
		if _, err := io.ReadFull(link, buf); err != nil {
			return fmt.Errorf("round %d: receive: %w", i, err)
		}
		e.res.Received++
		if err := conformance.VerifyPayload(seed, seq, buf); err != nil && verifyErr == nil {
			verifyErr = fmt.Errorf("round %d: %w", i, err)
		}
		if _, err := link.Write(buf); err != nil {
			return fmt.Errorf("round %d: echo: %w", i, err)
		}
		e.res.Sent++
	}
	if verifyErr != nil {
		e.check("every message intact", false, verifyErr.Error())
	} else {
		e.check("every message intact", true, "")
	}
	if e.cfg.Role == "creator" {
		e.check(fmt.Sprintf("every round trip <= %v", maxRTT), worst <= maxRTT, fmt.Sprintf("worst %v", worst))
		e.meta("worst_rtt_ms", worst.Milliseconds())
	}
	return nil
}

// stdBidirectional sends n messages each way concurrently. The creator's stream
// uses seed and the opener's seed+1; each side verifies message i of the peer's
// stream as Payload(peerSeed, i, size).
func stdBidirectional(e *env, link *hqq.StandardLink, n int) error {
	size, seed := e.cfg.Size, e.cfg.Seed
	outSeed, inSeed := seed, seed+1
	if e.cfg.Role == "opener" {
		outSeed, inSeed = inSeed, outSeed
	}
	var (
		wg           sync.WaitGroup
		sent, got    atomic.Int64
		wErr, rErr   error
		verifyFailed bool
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range n {
			if _, err := link.Write(conformance.Payload(outSeed, uint64(i), size)); err != nil {
				wErr = fmt.Errorf("write %d: %w", i, err)
				return
			}
			sent.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, size)
		for i := range n {
			if _, err := io.ReadFull(link, buf); err != nil {
				rErr = fmt.Errorf("read %d: %w", i, err)
				return
			}
			if err := conformance.VerifyPayload(inSeed, uint64(i), buf); err != nil {
				verifyFailed = true
				rErr = fmt.Errorf("message %d: %w", i, err)
				return
			}
			got.Add(1)
		}
	}()
	wg.Wait()
	e.res.Sent, e.res.Received = int(sent.Load()), int(got.Load())
	if verifyFailed {
		e.check("both streams intact and in order", false, rErr.Error())
		return nil
	}
	if err := errors.Join(wErr, rErr); err != nil {
		return err
	}
	e.check("both streams intact and in order", true, "")
	return nil
}
