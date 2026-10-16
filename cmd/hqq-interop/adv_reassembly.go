package main

import (
	"context"
	"fmt"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
)

func init() {
	p := advBase()
	p.MaxMessage = 65536
	p.MaxReassembly = 8192
	register(&scenario{
		ID: "X-A03",
		Title: "reassembly limit (X-17, SPEC 10.5): max_reassembly 8192 with max_message 65536, buffer_size 256. " +
			"Creator sends three data messages of 1.5 x limit (refused, dropped) and one of limit/2 (accepted). " +
			"A request of 1.5 x limit is answered with status 7 (ErrCodeBufferOverflow, ResponseError.Status). " +
			"A response of 1.5 x limit fails the creator's call with ErrReassemblyLimit. The link survives. " +
			"Checks: the first delivered data message is the accepted one, DroppedMessages counts the refusals. " +
			"Payload stream: creator seed",
		Kind:      "advanced",
		Platforms: []string{"any"},
		TimeoutS:  30,
		Variants:  []variant{advVariant("reassembly-limit", p)},
		Run:       runAdvReassembly,
	})
}

func runAdvReassembly(e *env) error {
	limit := int(e.cfg.AdvMaxReassembly)
	if limit == 0 {
		return failf("X-A03 needs --max-reassembly (params max_reassembly)")
	}
	big := limit + limit/2
	small := limit / 2
	nl, err := advEstablish(e)
	if err != nil {
		return err
	}
	defer nl.Close()
	l := nl.AdvancedLink
	if err := advNegotiateAll(e, l); err != nil {
		return err
	}
	seed := e.cfg.Seed

	if e.cfg.Role == "creator" {
		// Data messages refused by the opener's limit: dropped there.
		for i := range 3 {
			if err := l.Send(e.ctx, conformance.Payload(seed, uint64(i), big)); err != nil {
				return err
			}
		}
		// Accepted: it fits the limit on its own.
		if err := l.Send(e.ctx, conformance.Payload(seed, 3, small)); err != nil {
			return err
		}
		// Request refused by the opener's limit: an error response, status 7.
		_, err := l.Call(e.ctx, advMethodSink, conformance.Payload(seed, 4, big))
		st, ok := advStatus(err)
		e.check("refused request is answered with status 7 (ErrCodeBufferOverflow)",
			ok && st == uint64(hqq.ErrCodeBufferOverflow), errText(err))

		m, err := l.Call(e.ctx, advMethodEcho, []byte("ping"))
		if err == nil {
			e.check("request within the limit is answered", string(m.Bytes()) == "ping", string(m.Bytes()))
			m.Release()
		} else {
			e.check("request within the limit is answered", false, errText(err))
		}

		// Response refused by the creator's own limit: the call fails.
		_, err = l.Call(e.ctx, advMethodBig, []byte("big"))
		e.checkErr("refused response fails the waiting call with ErrReassemblyLimit", err, hqq.ErrReassemblyLimit)

		m, err = l.Call(e.ctx, advMethodEcho, []byte("again"))
		if err == nil {
			e.check("link survives the refused response", string(m.Bytes()) == "again", string(m.Bytes()))
			m.Release()
		} else {
			e.check("link survives the refused response", false, errText(err))
		}
		drops := l.Stats().DroppedMessages
		e.check("refused response is counted in DroppedMessages", drops >= 1, fmt.Sprintf("dropped %d", drops))
		e.res.Sent = 5
	} else {
		mux := hqq.NewMux()
		mux.HandleFunc(advMethodEcho, func(_ context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
			_, _ = w.Write(req.Payload())
		})
		mux.HandleFunc(advMethodBig, func(_ context.Context, w *hqq.ResponseWriter, _ *hqq.Request) {
			_, _ = w.Write(conformance.Payload(seed, 99, big))
		})
		mux.HandleFunc(advMethodSink, func(_ context.Context, w *hqq.ResponseWriter, _ *hqq.Request) {
			_, _ = w.Write([]byte("sink ran although its request was refused"))
		})
		advServeAsync(e, l, mux)

		// The three refused data messages never surface; the accepted one is first.
		if err := advRecvCheck(e, l, seed, 3, small); err != nil {
			return err
		}
		e.res.Received = 1
		if err := e.barrier("end"); err != nil {
			return err
		}
		drops := l.Stats().DroppedMessages
		// Three refused data messages and one refused request.
		e.check("refused data and request are counted in DroppedMessages", drops >= 4, fmt.Sprintf("dropped %d", drops))
		return nil
	}
	return e.barrier("end")
}
