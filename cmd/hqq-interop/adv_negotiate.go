package main

import (
	"context"
	"fmt"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
	"gosuda.org/hqq/internal/protocol"
)

func init() {
	register(&scenario{
		ID: "X-A01",
		Title: "negotiation (X-15, SPEC 10.1): creator-initiated, opener-initiated, simultaneous requests, " +
			"feature subsets (messages only, request-response only, none), major 2 ack and OpError from the opener, " +
			"Failed is terminal. Checks: negotiated version 1.1 and feature set, ErrNotNegotiated for the " +
			"features not negotiated, ErrNegotiationRejected, ErrNegotiationFailed. Raw frames: opener only (bad-major, op-error). " +
			"Payload stream: creator seed, 1 message of params size",
		Kind:      "advanced",
		Platforms: []string{"any"},
		TimeoutS:  30,
		Variants: []variant{
			advVariant("all-features", advBase()),
			advVariant("opener-initiates", advBase()),
			advVariant("simultaneous", advBase()),
			advVariant("messages-only", advBase()),
			advVariant("request-response-only", advBase()),
			advVariant("none", advBase()),
			advVariant("bad-major", advBase()),
			advVariant("op-error", advBase()),
		},
		Run: runAdvNegotiation,
	})
}

func runAdvNegotiation(e *env) error {
	switch e.cfg.Variant {
	case "bad-major", "op-error":
		return advNegotiationRejected(e)
	}
	nl, err := advEstablish(e)
	if err != nil {
		return err
	}
	defer nl.Close()
	l := nl.AdvancedLink

	want, err := advNegotiateVariant(e, l)
	if err != nil {
		return err
	}
	e.check("negotiated version is 1.1", l.NegotiatedVersion() == (hqq.ProtocolVersion{Major: 1, Minor: 1}),
		fmt.Sprintf("version %v", l.NegotiatedVersion()))
	e.check("negotiated features match the request", l.NegotiatedFeatures() == want,
		fmt.Sprintf("got %#x want %#x", l.NegotiatedFeatures(), want))
	e.check("protocol state is Negotiated", l.ProtocolState() == hqq.ProtocolStateNegotiated,
		fmt.Sprintf("state %d", l.ProtocolState()))

	if err := advNegoExchange(e, l, want); err != nil {
		return err
	}
	return e.barrier("end")
}

// advNegotiateVariant runs the negotiation of the variant and returns the
// feature set it must produce.
func advNegotiateVariant(e *env, l *hqq.AdvancedLink) (uint64, error) {
	ctx := e.ctx
	creator := e.cfg.Role == "creator"
	version := hqq.CurrentProtocolVersion()
	switch e.cfg.Variant {
	case "all-features":
		// Creator and opener both call Negotiate; the creator's request wins.
		return advAllFeatures, l.Negotiate(ctx, hqq.FeatureNone)
	case "opener-initiates":
		if creator {
			return advAllFeatures, l.Negotiate(ctx, advAllFeatures)
		}
		// Negotiate on the Secondary only answers; NegotiateProtocol sends
		// the Secondary's own request first.
		ok, err := l.NegotiateProtocol(ctx, version, advAllFeatures)
		if err == nil && !ok {
			err = hqq.ErrNegotiationRejected
		}
		return advAllFeatures, err
	case "simultaneous":
		// Both endpoints send a request at once; each answers or ignores the
		// other's request as its role says.
		ok, err := l.NegotiateProtocol(ctx, version, advAllFeatures)
		if err == nil && !ok {
			err = hqq.ErrNegotiationRejected
		}
		return advAllFeatures, err
	case "messages-only":
		return hqq.FeatureMessages, l.Negotiate(ctx, hqq.FeatureMessages)
	case "request-response-only":
		return hqq.FeatureRequestResponse, l.Negotiate(ctx, hqq.FeatureRequestResponse)
	case "none":
		// Zero features cannot be asked through Negotiate (zero means all),
		// so the creator requests the empty set with NegotiateProtocol; the
		// opener's answer is the intersection, the empty set.
		if creator {
			ok, err := l.NegotiateProtocol(ctx, version, hqq.FeatureNone)
			if err == nil && !ok {
				err = hqq.ErrNegotiationRejected
			}
			return hqq.FeatureNone, err
		}
		return hqq.FeatureNone, l.Negotiate(ctx, hqq.FeatureNone)
	}
	return 0, failf("unknown variant %q", e.cfg.Variant)
}

// advNegoExchange checks what the negotiated features allow: Send needs
// FeatureMessages, Call and Serve need FeatureRequestResponse.
func advNegoExchange(e *env, l *hqq.AdvancedLink, want uint64) error {
	ctx := e.ctx
	if e.cfg.Role == "creator" {
		if want&hqq.FeatureMessages != 0 {
			err := l.Send(ctx, conformance.Payload(advSendSeed(e), 0, e.cfg.Size))
			e.checkErr("send with FeatureMessages", err, nil)
		} else {
			e.checkErr("send without FeatureMessages is ErrNotNegotiated", l.Send(ctx, []byte("x")), hqq.ErrNotNegotiated)
		}
		if want&hqq.FeatureRequestResponse != 0 {
			m, err := l.Call(ctx, advMethodEcho, []byte("ping"))
			if err == nil {
				e.check("call with FeatureRequestResponse is echoed", string(m.Bytes()) == "ping", string(m.Bytes()))
				m.Release()
			} else {
				e.check("call with FeatureRequestResponse is echoed", false, errText(err))
			}
		} else {
			_, err := l.Call(ctx, advMethodEcho, []byte("ping"))
			e.checkErr("call without FeatureRequestResponse is ErrNotNegotiated", err, hqq.ErrNotNegotiated)
		}
		return nil
	}
	if want&hqq.FeatureMessages != 0 {
		if err := advRecvCheck(e, l, advRecvSeed(e), 0, e.cfg.Size); err != nil {
			return err
		}
	}
	if want&hqq.FeatureRequestResponse != 0 {
		advServeAsync(e, l, advEchoMux())
	}
	return nil
}

// advEchoMux answers method advMethodEcho with the request payload.
func advEchoMux() *hqq.Mux {
	mux := hqq.NewMux()
	mux.HandleFunc(advMethodEcho, func(_ context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
		_, _ = w.Write(req.Payload())
	})
	return mux
}

// advNegotiationRejected is the opener side of bad-major and op-error: the
// opener never negotiates, it answers the creator's request with a frame
// the Go API cannot produce, written through the raw ring writer.
// The creator checks the rejection, then both roles meet at barrier "end".
func advNegotiationRejected(e *env) error {
	nl, err := advEstablish(e)
	if err != nil {
		return err
	}
	defer nl.Close()
	l := nl.AdvancedLink
	if e.cfg.Role == "opener" {
		w, err := advRawOpen(e)
		if err != nil {
			return err
		}
		defer w.close()
		var p protocol.Packet
		if e.cfg.Variant == "bad-major" {
			// Version 2.0 in operand 0: major 2 is not accepted.
			p = protocol.NewPacket(protocol.OpProtoAck, 0x0200, advAllFeatures, 0)
		} else {
			p = protocol.NewPacket(protocol.OpError, 0, uint64(hqq.ErrCodeInvalidOp))
		}
		if err := w.send(e.ctx, p, nil); err != nil {
			return err
		}
		return e.barrier("end")
	}
	err1 := l.Negotiate(e.ctx, hqq.FeatureNone)
	e.checkErr("negotiation rejected by the peer's answer", err1, hqq.ErrNegotiationRejected)
	e.check("protocol state is Failed", l.ProtocolState() == hqq.ProtocolStateFailed,
		fmt.Sprintf("state %d", l.ProtocolState()))
	err2 := l.Negotiate(e.ctx, hqq.FeatureNone)
	e.checkErr("Failed is terminal: next negotiation is ErrNegotiationFailed", err2, hqq.ErrNegotiationFailed)
	return e.barrier("end")
}
