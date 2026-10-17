package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
	"gosuda.org/hqq/internal/shm"
)

// X-04: opener before creator, opener on an initializing segment, opener on a
// dead segment. Transient errors only; the opener writes nothing while it
// fails (the first page is compared before and after through a read-only mapping).
// On Windows a name lives only while a handle to it is open, so the segment of
// a crashed initializer or of dead endpoints is gone there: the opener then
// finds no name (the transient ErrNotExist) and the first-page comparison has
// nothing to compare.
//
//	before-creator: the opener's first attempt (short attach timeout) finds no
//	  segment or an unclaimed one; after the creator is up the open succeeds.
//	initializing:  a raw initializer claims the segment and dies at "claim"; the
//	  opener's open fails with ErrFailedInit within its attach timeout.
//	dead:          a creator dies at "exit" after the join barrier (its heartbeat
//	  is 50 ms, the opener's peer timeout 200 ms); the opener, after the joined
//	  opener closed, fails with ErrFailedInit (errNoLiveEndpoint) quickly.
func init() {
	register(&scenario{
		ID: "X-04", Title: "opener before creator, opener on an initializing or dead segment", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 30,
		Variants: []variant{
			{Name: "before-creator", Params: params{BufferCount: 4, BufferSize: 256, AttachTimeoutMs: 300},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"}}}},
			{Name: "initializing", Params: params{BufferCount: 4, BufferSize: 256, AttachTimeoutMs: 300},
				Steps: []step{
					{Roles: []string{"creator"}, Expect: map[string]string{"creator": "crash"},
						Args: map[string][]string{"creator": {"--crash-after", "claim"}}},
					{Roles: []string{"opener"}, Expect: map[string]string{"opener": "ok"}},
				}},
			{Name: "dead", Params: params{BufferCount: 4, BufferSize: 256, HeartbeatMs: 50, PeerTimeoutMs: 200},
				Steps: []step{
					{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "crash", "opener": "ok"},
						Args: map[string][]string{"creator": {"--crash-after", "exit"}}},
					{Roles: []string{"opener"}, Expect: map[string]string{"opener": "ok"}},
				}},
		},
		Run: runX04,
	})
}

func runX04(e *env) error {
	switch e.cfg.Variant {
	case "initializing":
		if e.cfg.Role == "creator" {
			return runRawInitVictim(e)
		}
		if !shm.NamesPersist {
			return stdOpenVanished(e)
		}
		return stdOpenDuringInit(e)
	case "dead":
		return runX04Dead(e)
	default:
		return runX04Before(e)
	}
}

func runX04Before(e *env) error {
	if e.cfg.Role == "opener" {
		start := time.Now()
		link, err := stdOpen(e)
		if link != nil {
			_ = link.Close()
		}
		e.check("first attempt before the creator fails transiently", errors.Is(err, os.ErrNotExist) || errors.Is(err, hqq.ErrFailedInit), errText(err))
		e.meta("first_attempt_ms", time.Since(start).Milliseconds())
		if err := e.barrier("attempt"); err != nil {
			return err
		}
		if err := e.barrier("created"); err != nil {
			return err
		}
		link, err = stdOpen(e)
		if err != nil {
			return err
		}
		defer func() { _ = link.Close() }()
		e.check("opener joins as Secondary", link.Mode() == hqq.LinkModeSecondary, link.Mode().String())
		return e.barrier("joined")
	}
	if err := e.barrier("attempt"); err != nil {
		return err
	}
	link, err := stdCreate(e)
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()
	if err := e.barrier("created"); err != nil {
		return err
	}
	return e.barrier("joined")
}

// runX04Dead: step 1 joins and kills the creator at exit; step 2 is the opener
// on the dead segment.
func runX04Dead(e *env) error {
	if e.cfg.Step == 2 {
		start := time.Now()
		link, err := stdOpen(e)
		elapsed := time.Since(start)
		if link != nil {
			_ = link.Close()
		}
		// Windows: the dead segment's name is gone, which is ErrNotExist.
		vanished := !shm.NamesPersist && errors.Is(err, os.ErrNotExist)
		e.check("opener on a dead segment fails with ErrFailedInit (ErrNotExist on Windows)",
			errors.Is(err, hqq.ErrFailedInit) || vanished, errText(err))
		bound := stdStaleAfter(e.cfg) + 3*time.Second
		e.check("failure is quick (errNoLiveEndpoint)", elapsed <= bound, fmt.Sprintf("%v, bound %v", elapsed, bound))
		return nil
	}
	if e.cfg.Role == "creator" {
		link, err := stdCreateSignal(e)
		if err != nil {
			return err
		}
		defer func() { _ = link.Close() }()
		if err := e.barrier("joined"); err != nil {
			return err
		}
		e.maybeCrash("exit")
		return nil
	}
	link, err := stdOpenAfter(e)
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()
	if err := e.barrier("joined"); err != nil {
		return err
	}
	return nil
}

// stdOpenDuringInit opens an initializing segment with the configured attach
// timeout. It must fail with ErrFailedInit within that timeout and must leave
// the first page of the segment unchanged.
func stdOpenDuringInit(e *env) error {
	before, err := rawObserve(e.cfg.Name)
	if err != nil {
		return err
	}
	attach := e.cfg.AttachTimeout
	if attach == 0 {
		attach = hqq.DefaultAttachTimeout
	}
	start := time.Now()
	link, err := stdOpen(e)
	elapsed := time.Since(start)
	if link != nil {
		_ = link.Close()
	}
	e.checkErr("open of an initializing segment fails with ErrFailedInit", err, hqq.ErrFailedInit)
	e.check("failure comes within the attach timeout", elapsed <= attach+stdSlack, fmt.Sprintf("%v, attach %v", elapsed, attach))
	after, err := rawObserve(e.cfg.Name)
	if err != nil {
		return err
	}
	e.check("opener wrote nothing to the first page", bytes.Equal(before.Page, after.Page), "")
	return nil
}

// stdOpenVanished is the opener of the initializing variant on Windows: the
// initializer died with its handle, so the name is gone and the open fails at
// once with the transient missing-name error.
func stdOpenVanished(e *env) error {
	start := time.Now()
	link, err := stdOpen(e)
	elapsed := time.Since(start)
	if link != nil {
		_ = link.Close()
	}
	e.check("open of a segment whose initializer died fails with ErrNotExist (no name without a handle)",
		errors.Is(err, os.ErrNotExist), errText(err))
	e.check("failure is quick", elapsed <= hqq.DefaultAttachTimeout+stdSlack, fmt.Sprintf("%v", elapsed))
	return nil
}

// X-08: kill -9 of the initializer between claim and publish. A raw initializer
// (see runRawInitVictim) dies at the variant's point. The surviving creator takes
// the initialization over after the stale window (max(peer timeout, 2 s)) and
// completes it; an opener with a short attach timeout fails with ErrFailedInit
// and writes nothing. Not on Windows: the name of a segment lives only while a
// handle to it is open, so a dead initializer leaves no segment to take over.
func init() {
	steps := func(point string) []step {
		return []step{
			{Roles: []string{"creator"}, Expect: map[string]string{"creator": "crash"},
				Args: map[string][]string{"creator": {"--crash-after", point}}},
			{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "ok"},
				Args: map[string][]string{"opener": {"--attach-timeout", "300"}}},
		}
	}
	register(&scenario{
		ID: "X-08", Title: "kill -9 of the initializer between claim and publish", Kind: "standard",
		Platforms: []string{"linux", "darwin", "freebsd"}, TimeoutS: 60,
		Variants: []variant{
			{Name: "claim", Params: params{BufferCount: 4, BufferSize: 256, PeerTimeoutMs: 2000}, Steps: steps("claim")},
			{Name: "format", Params: params{BufferCount: 4, BufferSize: 256, PeerTimeoutMs: 2000}, Steps: steps("format")},
			{Name: "publish", Params: params{BufferCount: 4, BufferSize: 256, PeerTimeoutMs: 2000}, Steps: steps("publish")},
			// 4 x 4 MiB per pool: the zeroing crash happens after the first 8 MiB chunk
			// of buffer pool 0 (about 32 MiB of buffers in total).
			{Name: "zeroing", Params: params{BufferCount: 4, BufferSize: 4 << 20, PeerTimeoutMs: 2000}, Steps: steps("zeroing")},
		},
		Run: runX08,
	})
}

func runX08(e *env) error {
	if e.cfg.Role == "opener" {
		if err := stdOpenDuringInit(e); err != nil {
			return err
		}
		return e.barrier("opener-done")
	}
	if e.cfg.CrashAfter != "" {
		return runRawInitVictim(e)
	}
	// Survivor: a creator that waits for the stale window and takes over.
	start := time.Now()
	link, err := stdCreate(e)
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()
	elapsed := time.Since(start)
	// initStale = max(peer timeout, 2 s); the library default peer timeout is 5 s.
	staleWindow := max(stdPeerTimeout(e.cfg), 2*time.Second)
	e.check("takeover waited for the stale window", elapsed >= staleWindow-200*time.Millisecond, fmt.Sprintf("%v, window %v", elapsed, staleWindow))
	v, err := rawObserve(e.cfg.Name)
	if err != nil {
		return err
	}
	e.check("survivor completed the initialization (Ready, epoch 1)",
		v.State == conformance.StateReady && v.Magic == conformance.MagicSuperblock && v.Epoch == 1,
		fmt.Sprintf("state %d magic %#x epoch %d", v.State, v.Magic, v.Epoch))
	e.meta("epoch", v.Epoch)
	return e.barrier("opener-done")
}
