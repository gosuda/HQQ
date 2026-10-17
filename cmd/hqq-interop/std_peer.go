package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
)

// X-07: kill -9 of the peer, parked and running, default and short peer timeout.
// The creator sends N messages and the opener reads them all. Then either
//   - the creator is killed right after its last send (--crash-after msgs:N); the
//     opener, which has read everything, must get ErrPeerGone within the bound; or
//   - the opener is killed while parked (--crash-after blocked); the creator's
//     blocked Read must get ErrPeerGone within the bound.
//
// The bound is max(peer timeout, 4 x heartbeat interval) plus slack (SPEC 6.3).
// Named scenario X-13 (Windows) reuses runKillPeer for its step 2.
func init() {
	register(&scenario{
		ID: "X-07", Title: "kill -9 of the peer (running and parked), default and short peer timeout", Kind: "standard",
		Platforms: []string{"any"}, TimeoutS: 60,
		Variants: []variant{
			{Name: "creator-killed-running", Params: params{BufferCount: 4, BufferSize: 256, Messages: 50},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "crash", "opener": "ok"},
					Args: map[string][]string{"creator": {"--crash-after", "msgs:50"}}}}},
			{Name: "opener-killed-parked", Params: params{BufferCount: 4, BufferSize: 256, Messages: 50},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "ok", "opener": "crash"},
					Args: map[string][]string{"opener": {"--crash-after", "blocked"}}}}},
			{Name: "short-peer-timeout", Params: params{BufferCount: 4, BufferSize: 256, Messages: 50, HeartbeatMs: 50, PeerTimeoutMs: 300},
				Steps: []step{{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "crash", "opener": "ok"},
					Args: map[string][]string{"creator": {"--crash-after", "msgs:50"}}}}},
		},
		Run: runKillPeer,
	})
}

// runKillPeer is the body of X-07 and of X-13 step 2.
func runKillPeer(e *env) error {
	n := e.cfg.Messages
	size, seed := e.cfg.Size, e.cfg.Seed
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
	if err := e.barrier("joined"); err != nil {
		return err
	}
	bound := stdPeerBound(e.cfg)
	buf := make([]byte, size)

	if e.cfg.Role == "creator" {
		for i := range n {
			if _, err := link.Write(conformance.Payload(seed, uint64(i), size)); err != nil {
				return fmt.Errorf("message %d: %w", i, err)
			}
			e.res.Sent++
		}
		e.maybeCrash(fmt.Sprintf("msgs:%d", n))
		// The opener was killed: its blocked read must end with ErrPeerGone.
		start := time.Now()
		_, err := link.Read(buf)
		elapsed := time.Since(start)
		e.checkErr("blocked read after the opener died gets ErrPeerGone", err, hqq.ErrPeerGone)
		e.check("peer loss detected within the bound", elapsed <= bound, fmt.Sprintf("%v, bound %v", elapsed, bound))
		return nil
	}

	intact := true
	for i := range n {
		if _, err := io.ReadFull(link, buf); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
		e.res.Received++
		if conformance.VerifyPayload(seed, uint64(i), buf) != nil {
			intact = false
		}
	}
	e.check("all buffered messages delivered intact before the peer loss", intact && e.res.Received == n, fmt.Sprintf("%d of %d", e.res.Received, n))
	e.maybeCrash("blocked")
	// The creator was killed right after its last send: ErrPeerGone must follow
	// the buffered messages within the bound.
	start := time.Now()
	_, err = link.Read(buf)
	elapsed := time.Since(start)
	e.checkErr("read after the buffered messages gets ErrPeerGone", err, hqq.ErrPeerGone)
	e.check("peer loss detected within the bound", elapsed <= bound, fmt.Sprintf("%v, bound %v", elapsed, bound))
	return nil
}

// X-09: reuse of a name after both endpoints died. Step 1: creator and opener
// exchange one message and both die at exit; the creator records its epoch and
// creatorToken in <sync-dir>/x09-1.token. Step 2: a new creator reuses the name
// (after the old heartbeats aged out) and checks epoch = old+1 and a new
// creatorToken; a new opener joins, and both die again (x09-2.token). Step 3: an
// opener, started with the old epoch in its meta, finds no live endpoint
// (ErrFailedInit) and the epoch is still the step-2 one.
//
// Step 1's opener, after the creator's death, must see ErrPeerGone within the
// bound (the old endpoints' loss observed for real). Step 2 does not repeat it.
//
// Not on Windows: a name lives only while a handle to it is open, so once both
// endpoints died the name is gone and the next creator starts a new object.
func init() {
	crash := map[string][]string{"creator": {"--crash-after", "exit"}, "opener": {"--crash-after", "exit"}}
	register(&scenario{
		ID: "X-09", Title: "reuse of a name after both endpoints died (epoch and creatorToken change)", Kind: "standard",
		Platforms: []string{"linux", "darwin", "freebsd"}, TimeoutS: 60,
		Variants: []variant{
			{Name: "reuse", Params: params{BufferCount: 4, BufferSize: 256, HeartbeatMs: 50, PeerTimeoutMs: 300},
				Steps: []step{
					{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "crash", "opener": "crash"}, Args: crash},
					{Roles: []string{"creator", "opener"}, Expect: map[string]string{"creator": "crash", "opener": "crash"}, Args: crash},
					{Roles: []string{"opener"}, Expect: map[string]string{"opener": "ok"}},
				}},
		},
		Run: runX09,
	})
}

func runX09(e *env) error {
	switch e.cfg.Step {
	case 1:
		return runX09Session(e, "x09-1", false)
	case 2:
		return runX09Session(e, "x09-2", true)
	default:
		return runX09Reopen(e)
	}
}

// runX09Session is one session of steps 1 and 2. A reusing creator waits for
// the old endpoints to die (ErrLinkBusy until their heartbeats age out).
func runX09Session(e *env, tag string, reuse bool) error {
	size, seed := e.cfg.Size, e.cfg.Seed
	var (
		link *hqq.NamedStandardLink
		err  error
	)
	if e.cfg.Role == "creator" {
		if reuse {
			link, err = stdCreateRetry(e, 15*time.Second)
		} else {
			link, err = stdCreate(e)
		}
		if err != nil {
			return err
		}
		if err := e.barrier("created"); err != nil {
			_ = link.Close()
			return err
		}
	} else {
		if err := e.barrier("created"); err != nil {
			return err
		}
		link, err = stdOpen(e)
		if err != nil {
			return err
		}
	}
	stdBind(e, link.StandardLink)
	if err := e.barrier("joined"); err != nil {
		return err
	}

	view, err := rawObserve(e.cfg.Name)
	if err != nil {
		return err
	}
	if e.cfg.Role == "creator" {
		if reuse {
			oldEpoch, oldToken, err := readX09Token(e, "x09-1")
			if err != nil {
				return err
			}
			e.check("reuse advances the epoch by one", view.Epoch == oldEpoch+1, fmt.Sprintf("old %d new %d", oldEpoch, view.Epoch))
			e.check("reuse takes a new creatorToken", view.CreatorToken != 0 && view.CreatorToken != oldToken,
				fmt.Sprintf("old %#x new %#x", oldToken, view.CreatorToken))
			e.meta("old_epoch", oldEpoch)
		}
		e.meta("epoch", view.Epoch)
		e.meta("creator_token", fmt.Sprintf("%#x", view.CreatorToken))
		if _, err := link.Write(conformance.Payload(seed, 0, size)); err != nil {
			return err
		}
		e.res.Sent++
		if err := os.WriteFile(filepath.Join(e.cfg.SyncDir, tag+".token"),
			[]byte(fmt.Sprintf("%d %d\n", view.Epoch, view.CreatorToken)), 0o644); err != nil {
			return err
		}
	} else {
		buf := make([]byte, size)
		if _, err := io.ReadFull(link, buf); err != nil {
			return err
		}
		e.res.Received++
		e.check("opener reads the message intact", conformance.VerifyPayload(seed, 0, buf) == nil, "")
		e.meta("epoch", view.Epoch)
	}
	if err := e.barrier("exchanged"); err != nil {
		return err
	}
	if e.cfg.Role == "opener" && !reuse {
		// The creator dies at exit right after the barrier. Its heartbeat stops,
		// so this read must end with ErrPeerGone within the bound.
		buf := make([]byte, size)
		start := time.Now()
		_, err = link.Read(buf)
		elapsed := time.Since(start)
		bound := stdPeerBound(e.cfg)
		e.checkErr("creator's death seen as ErrPeerGone", err, hqq.ErrPeerGone)
		e.check("peer loss detected within the bound", elapsed <= bound, fmt.Sprintf("%v, bound %v", elapsed, bound))
	}
	e.maybeCrash("exit")
	return nil
}

// runX09Reopen is step 3: an opener on a segment whose endpoints are all dead.
func runX09Reopen(e *env) error {
	oldEpoch, _, err := readX09Token(e, "x09-1")
	if err != nil {
		return err
	}
	newEpoch, _, err := readX09Token(e, "x09-2")
	if err != nil {
		return err
	}
	e.meta("old_epoch", oldEpoch)
	e.meta("epoch", newEpoch)
	// Both endpoints of step 2 are dead: wait until their heartbeats age out.
	if err := stdSleep(e, stdPeerBound(e.cfg)); err != nil {
		return err
	}
	link, err := stdOpen(e)
	if link != nil {
		_ = link.Close()
	}
	e.checkErr("open after both endpoints died fails with ErrFailedInit", err, hqq.ErrFailedInit)
	view, err := rawObserve(e.cfg.Name)
	if err != nil {
		return err
	}
	e.check("segment still carries the step-2 epoch", view.Epoch == newEpoch && newEpoch == oldEpoch+1,
		fmt.Sprintf("old %d, step 2 %d, now %d", oldEpoch, newEpoch, view.Epoch))
	return nil
}

// readX09Token reads "epoch creatorToken" written by a creator of step 1 or 2.
func readX09Token(e *env, tag string) (uint64, uint64, error) {
	raw, err := os.ReadFile(filepath.Join(e.cfg.SyncDir, tag+".token"))
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("%s.token: want \"epoch token\", got %q", tag, string(raw))
	}
	epoch, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	token, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return epoch, token, nil
}
