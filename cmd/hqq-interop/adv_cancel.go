package main

import (
	"context"
	"fmt"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/internal/protocol"
)

// advNoCancelFeatures is every feature of 1.1 except FeatureCancel.
const advNoCancelFeatures = hqq.FeatureMessages | hqq.FeatureRequestResponse | hqq.FeatureMetadata

func init() {
	register(&scenario{
		ID: "X-A07",
		Title: "cancel (1.1 FeatureCancel, PROTOCOL_1_1 section 2 and 4.2): cancel variant: a call without a deadline is " +
			"cancelled after 200 ms and ends with context.Canceled; the responder's handler sees context.Canceled " +
			"and CanceledRequests >= 1; an " +
			"OpCancel for an unknown key (raw) is ignored and the link keeps serving calls. nocancel variant " +
			"(FeatureCancel not negotiated): the cancelled call sends no OpCancel (the handler's context stays alive for " +
			"800 ms); a raw OpCancel from the peer fails the requester's receive path with ErrUnexpectedPacket. " +
			"Raw frames used: creator (cancel), opener (nocancel)",
		Kind:      "advanced",
		Platforms: []string{"any"},
		TimeoutS:  30,
		Variants: []variant{
			advVariant("cancel", advBase()),
			advVariant("nocancel", advBase()),
		},
		Run: runAdvCancel,
	})
}

func runAdvCancel(e *env) error {
	nl, err := advEstablish(e)
	if err != nil {
		return err
	}
	defer nl.Close()
	l := nl.AdvancedLink
	features := advAllFeatures
	if e.cfg.Variant == "nocancel" {
		features = advNoCancelFeatures
	}
	if err := l.Negotiate(e.ctx, features); err != nil {
		return err
	}
	if e.cfg.Variant == "nocancel" {
		return advCancelNoCancel(e, l)
	}
	return advCancelWithCancel(e, l)
}

// advBlockMux serves advMethodBlock: the handler blocks until its context
// ends (or 3 s pass) and reports whether the context was canceled. Echo
// answers advMethodEcho.
func advBlockMux(blocked chan<- bool, block time.Duration) *hqq.Mux {
	mux := advEchoMux()
	mux.HandleFunc(advMethodBlock, func(ctx context.Context, _ *hqq.ResponseWriter, _ *hqq.Request) {
		select {
		case <-ctx.Done():
			blocked <- ctx.Err() == context.Canceled
		case <-time.After(block):
			blocked <- ctx.Err() != nil
		}
	})
	return mux
}

func advCancelWithCancel(e *env, l *hqq.AdvancedLink) error {
	if e.cfg.Role == "opener" {
		blocked := make(chan bool, 1)
		advServeAsync(e, l, advBlockMux(blocked, 3*time.Second))
		if err := e.barrier("end"); err != nil {
			return err
		}
		select {
		case canceled := <-blocked:
			e.check("handler context is canceled by the peer's OpCancel", canceled, "")
		case <-time.After(5 * time.Second):
			e.check("handler context is canceled by the peer's OpCancel", false, "handler did not finish")
		}
		canceledReqs := l.Stats().CanceledRequests
		e.check("responder counts the canceled request", canceledReqs >= 1, fmt.Sprint(canceledReqs))
		return nil
	}

	// No deadline on the call: a deadline would send a time budget (1.1
	// FeatureMetadata) and the handler would end by deadline, not by OpCancel.
	cctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(200*time.Millisecond, cancel)
	_, err := l.Call(cctx, advMethodBlock, []byte("block"))
	timer.Stop()
	cancel()
	e.checkErr("cancelled call ends with context.Canceled", err, context.Canceled)

	// An OpCancel whose key names no request is ignored.
	w, err := advRawOpen(e)
	if err != nil {
		return err
	}
	err = w.send(e.ctx, protocol.NewPacket(protocol.OpCancel, 0, 999999), nil)
	w.close()
	if err != nil {
		return err
	}
	m, err := l.Call(e.ctx, advMethodEcho, []byte("after"))
	if err != nil {
		return err
	}
	e.check("link keeps serving calls after an ignored OpCancel", string(m.Bytes()) == "after", string(m.Bytes()))
	m.Release()
	return e.barrier("end")
}

func advCancelNoCancel(e *env, l *hqq.AdvancedLink) error {
	if e.cfg.Role == "opener" {
		blocked := make(chan bool, 1)
		advServeAsync(e, l, advBlockMux(blocked, 800*time.Millisecond))
		if err := e.barrier("cancelled"); err != nil {
			return err
		}
		// The requester's receive path must fail on this frame.
		w, err := advRawOpen(e)
		if err != nil {
			return err
		}
		err = w.send(e.ctx, protocol.NewPacket(protocol.OpCancel, 0, 1), nil)
		w.close()
		if err != nil {
			return err
		}
		select {
		case canceled := <-blocked:
			e.check("no OpCancel was sent: the handler context stays alive", !canceled, "")
		case <-time.After(5 * time.Second):
			e.check("no OpCancel was sent: the handler context stays alive", false, "handler did not finish")
		}
		return e.barrier("end")
	}

	cctx, cancel := context.WithTimeout(e.ctx, 200*time.Millisecond)
	_, err := l.Call(cctx, advMethodBlock, []byte("block"))
	cancel()
	e.checkErr("cancelled call ends with DeadlineExceeded", err, context.DeadlineExceeded)
	if err := e.barrier("cancelled"); err != nil {
		return err
	}
	_, err = l.Recv(e.ctx)
	e.checkErr("OpCancel without FeatureCancel fails the receive path with ErrUnexpectedPacket", err, hqq.ErrUnexpectedPacket)
	return e.barrier("end")
}
