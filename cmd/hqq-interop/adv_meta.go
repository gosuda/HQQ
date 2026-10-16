package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gosuda.org/hqq"
)

// advNoMetaFeatures is every feature of 1.1 except FeatureMetadata.
const advNoMetaFeatures = hqq.FeatureMessages | hqq.FeatureRequestResponse | hqq.FeatureCancel

func init() {
	register(&scenario{
		ID: "X-A08",
		Title: "request metadata (1.1 FeatureMetadata, PROTOCOL_1_1 section 3): metadata variant: CallWith caller 42 and " +
			"a trace ID reach Request.Caller and Request.Trace; a 2 s deadline reaches the handler's context as a deadline " +
			"within (1 s, 2 s]; a call whose deadline already passed fails locally with DeadlineExceeded and sends nothing " +
			"(responder handles 2 requests, requester Stats().Requests unchanged). without-metadata variant (FeatureMetadata " +
			"not negotiated): the options are ignored (Caller 0, Trace zero) and no deadline reaches the handler",
		Kind:      "advanced",
		Platforms: []string{"any"},
		TimeoutS:  30,
		Variants: []variant{
			advVariant("metadata", advBase()),
			advVariant("without-metadata", advBase()),
		},
		Run: runAdvMeta,
	})
}

// advMetaSeen is what the responder's handler saw of one request.
type advMetaSeen struct {
	caller    uint64
	trace     [16]byte
	hasDL     bool
	remaining time.Duration
}

func runAdvMeta(e *env) error {
	nl, err := advEstablish(e)
	if err != nil {
		return err
	}
	defer nl.Close()
	l := nl.AdvancedLink
	features := advAllFeatures
	if e.cfg.Variant == "without-metadata" {
		features = advNoMetaFeatures
	}
	if err := l.Negotiate(e.ctx, features); err != nil {
		return err
	}
	withMeta := e.cfg.Variant == "metadata"

	if e.cfg.Role == "opener" {
		var (
			mu   sync.Mutex
			seen []advMetaSeen
		)
		mux := hqq.NewMux()
		mux.HandleFunc(advMethodMeta, func(ctx context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
			rec := advMetaSeen{caller: req.Caller(), trace: req.Trace()}
			if dl, ok := ctx.Deadline(); ok {
				rec.hasDL = true
				rec.remaining = time.Until(dl)
			}
			mu.Lock()
			seen = append(seen, rec)
			mu.Unlock()
			_, _ = w.Write(req.Payload())
		})
		// Serve without the process deadline: a handler context must carry
		// only the deadline the protocol sends (the time budget, 1.1 metadata).
		go func() { _ = l.Serve(context.WithoutCancel(e.ctx), mux) }()
		if err := e.barrier("end"); err != nil {
			return err
		}
		mu.Lock()
		got := append([]advMetaSeen(nil), seen...)
		mu.Unlock()
		if withMeta {
			e.check("responder saw 2 requests (the expired call was not sent)", len(got) == 2, fmt.Sprint(len(got)))
			if len(got) == 2 {
				e.check("caller ID and trace ID arrive", got[0].caller == 42 && got[0].trace == advTrace(), fmt.Sprint(got[0].caller))
				e.check("request without options has caller 0", got[1].caller == 0, fmt.Sprint(got[1].caller))
				e.check("deadline reaches the handler within the budget",
					got[1].hasDL && got[1].remaining > time.Second && got[1].remaining <= 2*time.Second,
					fmt.Sprintf("has=%v remaining=%v", got[1].hasDL, got[1].remaining))
			}
		} else {
			e.check("responder saw 2 requests", len(got) == 2, fmt.Sprint(len(got)))
			if len(got) == 2 {
				e.check("options are ignored without FeatureMetadata: caller 0, trace zero",
					got[0].caller == 0 && got[0].trace == [16]byte{}, fmt.Sprint(got[0].caller))
				e.check("no deadline reaches the handler without FeatureMetadata", !got[1].hasDL, fmt.Sprint(got[1].hasDL))
			}
		}
		return nil
	}

	ctx := e.ctx
	if _, err := l.CallWith(ctx, advMethodMeta, []byte("a"), hqq.CallOptions{Caller: 42, Trace: advTrace()}); err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	_, err = l.CallWith(dctx, advMethodMeta, []byte("b"), hqq.CallOptions{})
	cancel()
	if err != nil {
		return err
	}
	if withMeta {
		before := l.Stats().Requests
		ectx, ecancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		_, err := l.CallWith(ectx, advMethodMeta, []byte("c"), hqq.CallOptions{})
		ecancel()
		e.checkErr("call with an expired deadline fails locally with DeadlineExceeded", err, context.DeadlineExceeded)
		after := l.Stats().Requests
		e.check("the expired call sent no request", after == before, fmt.Sprintf("requests %d -> %d", before, after))
	}
	return e.barrier("end")
}

// advTrace is the trace ID sent by X-A08.
func advTrace() [16]byte {
	var t [16]byte
	for i := range t {
		t[i] = byte(i + 1)
	}
	return t
}
