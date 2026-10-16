package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"time"

	"gosuda.org/hqq"
)

// advLateFeatures negotiates the 1.0 features only. Without FeatureCancel a
// cancelled call sends no OpCancel, so the handler's late response reaches
// the requester and is discarded there (AbandonedResponses).
const advLateFeatures = hqq.FeatureMessages | hqq.FeatureRequestResponse

const advRPCConcurrency = 16

func init() {
	register(&scenario{
		ID: "X-A04",
		Title: "request/response (X-18, SPEC 10.6): 16 concurrent calls whose handlers sleep inversely to the id " +
			"(every answer is echoed, completion order is not the issue order); an error response with status 42 and " +
			"payload; a call cancelled after 50 ms whose late response is discarded (AbandonedResponses >= 1); " +
			"an unknown method answered with status 6 (ErrCodeInvalidOp). Features: messages and request-response " +
			"(1.0 set, so late responses are sent)",
		Kind:      "advanced",
		Platforms: []string{"any"},
		TimeoutS:  30,
		Variants:  []variant{advVariant("default", advBase())},
		Run:       runAdvRPC,
	})
}

func runAdvRPC(e *env) error {
	nl, err := advEstablish(e)
	if err != nil {
		return err
	}
	defer nl.Close()
	l := nl.AdvancedLink
	if err := l.Negotiate(e.ctx, advLateFeatures); err != nil {
		return err
	}
	if e.cfg.Role == "opener" {
		advServeAsync(e, l, advRPCMux())
		return e.barrier("end")
	}

	// Concurrent calls, answered out of order.
	type doneCall struct {
		id  int
		err error
	}
	done := make(chan doneCall, advRPCConcurrency)
	for i := range advRPCConcurrency {
		go func() {
			payload := binary.BigEndian.AppendUint32(nil, uint32(i))
			m, err := l.Call(e.ctx, advMethodSleepEcho, payload)
			if err == nil {
				if string(m.Bytes()) != string(payload) {
					err = fmt.Errorf("call %d: echo mismatch", i)
				}
				m.Release()
			}
			done <- doneCall{id: i, err: err}
		}()
	}
	order := make([]int, 0, advRPCConcurrency)
	var firstErr error
	for range advRPCConcurrency {
		d := <-done
		order = append(order, d.id)
		if d.err != nil && firstErr == nil {
			firstErr = d.err
		}
	}
	e.check("all concurrent calls are answered with their own payload", firstErr == nil, errText(firstErr))
	e.check("responses complete out of order", !sort.IntsAreSorted(order), fmt.Sprint(order))
	e.res.Sent = advRPCConcurrency

	// Error response with a payload.
	_, err = l.Call(e.ctx, advMethodFail, nil)
	var re hqq.ResponseError
	e.check("error response carries status 42 and its payload",
		errors.As(err, &re) && re.Status == 42 && string(re.Payload) == "boom payload", errText(err))

	// Cancelled call: its late response is discarded.
	cctx, cancel := context.WithTimeout(e.ctx, 50*time.Millisecond)
	_, err = l.Call(cctx, advMethodSlow, []byte("slow"))
	cancel()
	e.checkErr("cancelled call returns its context error", err, context.DeadlineExceeded)
	// Receiving routes the late response; pump calls until it is discarded.
	deadline := time.Now().Add(3 * time.Second)
	for l.Stats().AbandonedResponses == 0 && time.Now().Before(deadline) {
		m, err := l.Call(e.ctx, advMethodEcho, []byte("pump"))
		if err != nil {
			return err
		}
		m.Release()
	}
	abandoned := l.Stats().AbandonedResponses
	e.check("late response of the cancelled call is discarded (AbandonedResponses)", abandoned >= 1,
		fmt.Sprintf("abandoned %d", abandoned))

	// Unknown method: the mux answers with the default error response.
	_, err = l.Call(e.ctx, 99, nil)
	st, ok := advStatus(err)
	e.check("unknown method is answered with status 6 (ErrCodeInvalidOp)",
		ok && st == uint64(hqq.ErrCodeInvalidOp), errText(err))
	return e.barrier("end")
}

// advRPCMux serves the methods of X-A04.
func advRPCMux() *hqq.Mux {
	mux := advEchoMux()
	mux.HandleFunc(advMethodSleepEcho, func(ctx context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
		id := binary.BigEndian.Uint32(req.Payload())
		// Inverse sleep: later ids finish first.
		advSleep(ctx, time.Duration(max(0, advRPCConcurrency-int(id)))*5*time.Millisecond)
		_, _ = w.Write(req.Payload())
	})
	mux.HandleFunc(advMethodFail, func(_ context.Context, w *hqq.ResponseWriter, _ *hqq.Request) {
		w.WriteError(42, []byte("boom payload"))
	})
	mux.HandleFunc(advMethodSlow, func(ctx context.Context, w *hqq.ResponseWriter, _ *hqq.Request) {
		advSleep(ctx, 300*time.Millisecond)
		_, _ = w.Write([]byte("late"))
	})
	return mux
}
