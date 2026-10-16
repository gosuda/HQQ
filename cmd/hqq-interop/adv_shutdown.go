package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gosuda.org/hqq"
)

func init() {
	register(&scenario{
		ID: "X-A09",
		Title: "graceful shutdown (1.1 section 6): the responder (creator) runs a request A that blocks until released. " +
			"The requester (opener) calls B until it is refused with status 8 (ErrShuttingDown). Checks: Shutdown does " +
			"not return while A is in flight; after release A completes normally (\"A-done\"), Shutdown returns nil, " +
			"Serve returns nil, RefusedRequests >= 1; the link stays usable: data flows both ways after shutdown",
		Kind:      "advanced",
		Platforms: []string{"any"},
		TimeoutS:  30,
		Variants:  []variant{advVariant("default", advBase())},
		Run:       runAdvShutdown,
	})
}

func runAdvShutdown(e *env) error {
	nl, err := advEstablish(e)
	if err != nil {
		return err
	}
	defer nl.Close()
	l := nl.AdvancedLink
	if err := l.Negotiate(e.ctx, hqq.FeatureNone); err != nil {
		return err
	}
	if e.cfg.Role == "opener" {
		return advShutdownRequester(e, l)
	}
	return advShutdownResponder(e, l)
}

// advShutdownResponder is the creator: it serves, shuts down while A is in
// flight, releases A, and then exchanges data.
func advShutdownResponder(e *env, l *hqq.AdvancedLink) error {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	mux := hqq.NewMux()
	mux.HandleFunc(advMethodBlock, func(ctx context.Context, w *hqq.ResponseWriter, _ *hqq.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		_, _ = w.Write([]byte("A-done"))
	})
	mux.HandleFunc(advMethodEcho, func(_ context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
		_, _ = w.Write(req.Payload())
	})
	serveDone := advServeAsync(e, l, mux)

	select {
	case <-started:
	case <-e.ctx.Done():
		return e.ctx.Err()
	}
	shutdownDone := make(chan error, 1)
	go func() {
		sctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
		defer cancel()
		shutdownDone <- l.Shutdown(sctx)
	}()
	if err := e.barrier("refused"); err != nil {
		return err
	}
	select {
	case err := <-shutdownDone:
		e.check("Shutdown waits for the in-flight request", false, "returned early: "+errText(err))
	default:
		e.check("Shutdown waits for the in-flight request", true, "")
	}
	close(release)
	select {
	case err := <-shutdownDone:
		e.checkErr("Shutdown returns nil after the in-flight request ended", err, nil)
	case <-e.ctx.Done():
		return e.ctx.Err()
	}
	serveErr := <-serveDone
	e.checkErr("Serve returns nil after shutdown", serveErr, nil)
	refused := l.Stats().RefusedRequests
	e.check("refused requests are counted", refused >= 1, fmt.Sprint(refused))

	// The link stays open: data flows both ways.
	m, err := l.Recv(e.ctx)
	if err != nil {
		return err
	}
	e.check("data from the peer after shutdown", string(m.Bytes()) == "after-shutdown", string(m.Bytes()))
	m.Release()
	if err := l.Send(e.ctx, []byte("reply")); err != nil {
		return err
	}
	return e.barrier("end")
}

// advShutdownRequester is the opener: it sends A, then calls B until the
// responder refuses it, then checks A's answer and exchanges data.
func advShutdownRequester(e *env, l *hqq.AdvancedLink) error {
	type callResult struct {
		body string
		err  error
	}
	aDone := make(chan callResult, 1)
	go func() {
		m, err := l.Call(e.ctx, advMethodBlock, []byte("A"))
		if err != nil {
			aDone <- callResult{err: err}
			return
		}
		body := string(m.Bytes())
		m.Release()
		aDone <- callResult{body: body}
	}()

	refused := false
	var lastErr error
	deadline := time.Now().Add(10 * time.Second)
	for !refused && time.Now().Before(deadline) {
		m, err := l.Call(e.ctx, advMethodEcho, []byte("B"))
		if err == nil {
			m.Release() // answered before shutdown started
			continue
		}
		lastErr = err
		st, ok := advStatus(err)
		refused = ok && st == uint64(hqq.ErrCodeShuttingDown) && errors.Is(err, hqq.ErrShuttingDown)
		if !refused {
			return err
		}
	}
	e.check("request after shutdown started is refused with status 8 (ErrShuttingDown)", refused, errText(lastErr))
	if err := e.barrier("refused"); err != nil {
		return err
	}
	res := <-aDone
	e.check("in-flight request completes normally", res.err == nil && res.body == "A-done", errText(res.err)+" "+res.body)

	if err := l.Send(e.ctx, []byte("after-shutdown")); err != nil {
		return err
	}
	m, err := l.Recv(e.ctx)
	if err != nil {
		return err
	}
	e.check("data from the responder after shutdown", string(m.Bytes()) == "reply", string(m.Bytes()))
	m.Release()
	return e.barrier("end")
}
