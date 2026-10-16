package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
)

// advPerm is the permission of the segments the Advanced scenarios create.
const advPerm os.FileMode = 0o600

// advAllFeatures is every feature of protocol 1.1.
const advAllFeatures = hqq.FeatureMessages | hqq.FeatureRequestResponse | hqq.FeatureCancel | hqq.FeatureMetadata

// Method selectors of the Advanced scenarios (Request.Method).
const (
	advMethodSleepEcho uint64 = 1 // X-A04: sleeps inversely to the request id, echoes the payload
	advMethodFail      uint64 = 2 // X-A04: error response with status 42 and a payload
	advMethodSlow      uint64 = 3 // X-A04: answers after 300 ms
	advMethodEcho      uint64 = 4 // echoes the payload
	advMethodBig       uint64 = 5 // answers with a body larger than the reassembly limit (X-A03)
	advMethodSink      uint64 = 6 // must never run when its request was refused (X-A03)
	advMethodBlock     uint64 = 7 // blocks until its context ends (X-A07, X-A09)
	advMethodMeta      uint64 = 8 // records caller, trace and deadline (X-A08)
)

// advOptions returns the AdvancedOptions of this process. Both roles of a
// variant derive them from the same params, so they agree.
func advOptions(c config) hqq.AdvancedOptions {
	opts := hqq.AdvancedOptions{
		MaxMessageSize:     c.AdvMaxMessage,
		MaxReassemblyBytes: c.AdvMaxReassembly,
		IncomingQueueSize:  c.AdvQueueSize,
	}
	// Zero keeps the library default; a negative heartbeat disables heartbeats.
	opts.Link.HeartbeatInterval = time.Duration(c.HeartbeatMs) * time.Millisecond
	opts.Link.PeerTimeout = time.Duration(c.PeerTimeoutMs) * time.Millisecond
	opts.Link.AttachTimeout = c.AttachTimeout
	if c.Wait == "spin" {
		opts.Link.WaitPolicy = hqq.WaitSpin
	}
	return opts
}

// advMaxMessage returns the MaxMessageSize in force: the configured one, or
// the library default when none was configured.
func advMaxMessage(c config) int {
	if c.AdvMaxMessage != 0 {
		return int(c.AdvMaxMessage)
	}
	return int(hqq.DefaultMaxAdvancedMessageSize)
}

// advEstablish makes the named Advanced link of the scenario available to
// both roles. The creator (Primary) creates it and both roles meet at barrier
// "linked", so the opener (Secondary) never opens a segment that does not
// exist yet.
func advEstablish(e *env) (*hqq.NamedAdvancedLink, error) {
	if e.cfg.Role == "creator" {
		nl, err := hqq.CreateAdvancedLink(e.cfg.Name, e.cfg.BufferCount, e.cfg.BufferSize, advPerm, advOptions(e.cfg))
		if err != nil {
			return nil, err
		}
		if err := e.barrier("linked"); err != nil {
			_ = nl.Close()
			return nil, err
		}
		return nl, nil
	}
	if err := e.barrier("linked"); err != nil {
		return nil, err
	}
	return hqq.OpenAdvancedLink(e.cfg.Name, e.cfg.BufferCount, e.cfg.BufferSize, advOptions(e.cfg))
}

// advRoleIndex is 0 for the creator and 1 for the opener. Each role sends
// its own payload stream with seed + index; the peer verifies it with the
// peer's index.
func advRoleIndex(role string) uint64 {
	if role == "creator" {
		return 0
	}
	return 1
}

// advSendSeed is the payload seed of the stream this role sends.
func advSendSeed(e *env) uint64 {
	return e.cfg.Seed + advRoleIndex(e.cfg.Role)
}

// advRecvSeed is the payload seed of the stream the peer sends.
func advRecvSeed(e *env) uint64 {
	return e.cfg.Seed + advRoleIndex(other(e.cfg.Role))
}

// advRecvCheck receives the next message of l and checks that it has size
// bytes and is message seq of the stream seed (Payload(seed, seq, size)).
func advRecvCheck(e *env, l *hqq.AdvancedLink, seed, seq uint64, size int) error {
	m, err := l.Recv(e.ctx)
	if err != nil {
		return err
	}
	defer m.Release()
	got := m.Bytes()
	verr := conformance.VerifyPayload(seed, seq, got)
	e.check(fmt.Sprintf("message %d has %d bytes and is intact", seq, size), len(got) == size && verr == nil,
		fmt.Sprintf("len=%d %s", len(got), errText(verr)))
	return nil
}

// advServeAsync runs l.Serve(h) on its own goroutine. The result arrives on
// the returned channel; Serve ends when the link closes. Handlers must not
// call the scenario's env: they run concurrently with the role.
func advServeAsync(e *env, l *hqq.AdvancedLink, h hqq.Handler) <-chan error {
	done := make(chan error, 1)
	go func() { done <- l.Serve(e.ctx, h) }()
	return done
}

// advSleep waits for d or until ctx ends.
func advSleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// advStatus returns the status of an error response, if err is one.
func advStatus(err error) (uint64, bool) {
	var re hqq.ResponseError
	if errors.As(err, &re) {
		return re.Status, true
	}
	return 0, false
}

// advVariant is a two-role variant: one step runs the creator and the
// opener, and both must pass.
func advVariant(name string, p params) variant {
	return variant{
		Name:   name,
		Params: p,
		Steps: []step{{
			Roles:  []string{"creator", "opener"},
			Expect: map[string]string{"creator": "ok", "opener": "ok"},
		}},
	}
}

// advBase is the parameter set of the Advanced scenarios; variants override
// what they need.
func advBase() params {
	return params{BufferCount: 4, BufferSize: 256}
}
