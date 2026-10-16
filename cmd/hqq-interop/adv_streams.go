package main

import (
	"context"
	"fmt"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/conformance"
	"gosuda.org/hqq/internal/protocol"
)

func init() {
	register(&scenario{
		ID: "X-A05",
		Title: "streams (X-19, SPEC 10.2-10.3): basic: OpenStream refused while not listening (ErrConnNotFound), " +
			"then ids 2 and 4 (Primary parity, never reused), data both ways, peer sees ErrPeerClosed after the queued " +
			"data, stream 0 not closable (ErrInvalidOp), second Close ErrClosed. bad-parity: a raw OpConnCreate with an odd " +
			"id is refused with ErrCodeInvalidOp (the creator's receive fails with ErrInvalidOp, nothing is accepted). " +
			"duplicate: a raw OpConnCreate of an existing id is refused with ErrCodeInvalidOp. " +
			"Payload stream: creator seed (2 messages on stream 2, 1 on stream 4), opener's reply seed + 1",
		Kind:      "advanced",
		Platforms: []string{"any"},
		TimeoutS:  30,
		Variants: []variant{
			advVariant("basic", advBase()),
			advVariant("bad-parity", advBase()),
			advVariant("duplicate", advBase()),
		},
		Run: runAdvStreams,
	})
}

func runAdvStreams(e *env) error {
	switch e.cfg.Variant {
	case "basic":
		return advStreamsBasic(e)
	case "bad-parity", "duplicate":
		return advStreamsInvalid(e)
	}
	return failf("unknown variant %q", e.cfg.Variant)
}

// advStreamsBasic is the main flow of X-A05.
func advStreamsBasic(e *env) error {
	nl, err := advEstablish(e)
	if err != nil {
		return err
	}
	defer nl.Close()
	l := nl.AdvancedLink
	if err := l.Negotiate(e.ctx, hqq.FeatureNone); err != nil {
		return err
	}
	seed := e.cfg.Seed
	if e.cfg.Role == "opener" {
		// Starts the dispatcher; not listening yet.
		shortCtx, cancel := context.WithTimeout(e.ctx, time.Second)
		_, err := l.AcceptStream(shortCtx)
		cancel()
		e.checkErr("AcceptStream without ListenStreams is ErrNotListening", err, hqq.ErrNotListening)
		if err := e.barrier("p1"); err != nil {
			return err
		}
		if err := e.barrier("p2"); err != nil {
			return err
		}
		if err := l.ListenStreams(e.ctx); err != nil {
			return err
		}
		if err := e.barrier("p3"); err != nil {
			return err
		}

		st, err := l.AcceptStream(e.ctx)
		if err != nil {
			return err
		}
		e.check("first accepted stream has id 4 (the refused open consumed 2)", st.ID() == 4, fmt.Sprint(st.ID()))
		for i := range 2 {
			if err := advStreamRecvCheck(e, st, seed, uint64(i), e.cfg.Size); err != nil {
				return err
			}
		}
		if err := st.Send(e.ctx, conformance.Payload(seed+1, 0, 10)); err != nil {
			return err
		}
		_, err = st.Recv(e.ctx)
		e.checkErr("peer close is seen after the queued data as ErrPeerClosed", err, hqq.ErrPeerClosed)

		st2, err := l.AcceptStream(e.ctx)
		if err != nil {
			return err
		}
		e.check("second accepted stream has id 6 (ids are not reused)", st2.ID() == 6, fmt.Sprint(st2.ID()))
		if err := advStreamRecvCheck(e, st2, seed, 2, e.cfg.Size); err != nil {
			return err
		}
		e.res.Received = 3
		return e.barrier("end")
	}

	// Creator: the Primary allocates even ids.
	if err := e.barrier("p1"); err != nil {
		return err
	}
	_, err = l.OpenStream(e.ctx)
	e.checkErr("OpenStream refused while the peer does not listen is ErrConnNotFound", err, hqq.ErrConnNotFound)
	if err := e.barrier("p2"); err != nil {
		return err
	}
	if err := e.barrier("p3"); err != nil {
		return err
	}

	st, err := l.OpenStream(e.ctx)
	if err != nil {
		return err
	}
	e.check("first stream has id 4 (the refused open consumed 2)", st.ID() == 4, fmt.Sprint(st.ID()))
	for i := range 2 {
		if err := st.Send(e.ctx, conformance.Payload(seed, uint64(i), e.cfg.Size)); err != nil {
			return err
		}
	}
	reply, err := st.Recv(e.ctx)
	if err != nil {
		return err
	}
	e.check("reply from the peer's stream is intact", conformance.VerifyPayload(seed+1, 0, reply.Bytes()) == nil, "")
	reply.Release()
	e.checkErr("stream Close", st.Close(), nil)
	e.checkErr("second Close is ErrClosed", st.Close(), hqq.ErrClosed)
	_, err = st.Recv(e.ctx)
	e.checkErr("Recv on a closed stream is ErrClosed", err, hqq.ErrClosed)
	e.checkErr("stream 0 cannot be closed", l.Stream().Close(), hqq.ErrInvalidOp)

	st2, err := l.OpenStream(e.ctx)
	if err != nil {
		return err
	}
	e.check("second stream has id 6 (ids are not reused)", st2.ID() == 6, fmt.Sprint(st2.ID()))
	if err := st2.Send(e.ctx, conformance.Payload(seed, 2, e.cfg.Size)); err != nil {
		return err
	}
	e.checkErr("stream 4 Close", st2.Close(), nil)
	e.res.Sent = 3
	return e.barrier("end")
}

// advStreamRecvCheck is advRecvCheck on a stream: it receives the next
// message of st and checks it is message seq of the stream seed.
func advStreamRecvCheck(e *env, st *hqq.Stream, seed, seq uint64, size int) error {
	m, err := st.Recv(e.ctx)
	if err != nil {
		return err
	}
	defer m.Release()
	got := m.Bytes()
	verr := conformance.VerifyPayload(seed, seq, got)
	e.check(fmt.Sprintf("stream message %d has %d bytes and is intact", seq, size), len(got) == size && verr == nil,
		fmt.Sprintf("len=%d %s", len(got), errText(verr)))
	return nil
}

// advStreamsInvalid covers the two refusals with ErrCodeInvalidOp: an odd
// id from the Primary (bad parity) and an id that is already registered
// (duplicate). The Go endpoint of the creator crafts the frame raw.
func advStreamsInvalid(e *env) error {
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
		if err := l.ListenStreams(e.ctx); err != nil {
			return err
		}
		if err := e.barrier("p1"); err != nil {
			return err
		}
		if e.cfg.Variant == "bad-parity" {
			shortCtx, cancel := context.WithTimeout(e.ctx, 300*time.Millisecond)
			_, err := l.AcceptStream(shortCtx)
			cancel()
			e.checkErr("refused stream is not accepted", err, context.DeadlineExceeded)
		} else {
			st, err := l.AcceptStream(e.ctx)
			if err != nil {
				return err
			}
			e.check("first stream has id 2", st.ID() == 2, fmt.Sprint(st.ID()))
		}
		return e.barrier("end")
	}

	if err := e.barrier("p1"); err != nil {
		return err
	}
	w, err := advRawOpen(e)
	if err != nil {
		return err
	}
	defer w.close()
	if e.cfg.Variant == "bad-parity" {
		// Odd ids belong to the Secondary: the Primary must not send one.
		if err := w.send(e.ctx, protocol.NewPacket(protocol.OpConnCreate, 3), nil); err != nil {
			return err
		}
	} else {
		st, err := l.OpenStream(e.ctx)
		if err != nil {
			return err
		}
		e.check("first stream has id 2", st.ID() == 2, fmt.Sprint(st.ID()))
		// Id 2 is registered at the peer: the duplicate is refused.
		if err := w.send(e.ctx, protocol.NewPacket(protocol.OpConnCreate, st.ID()), nil); err != nil {
			return err
		}
	}
	// The refusal is an OpError that matches no pending OpenStream: the
	// receive path of the sender fails with the error of its code.
	_, err = l.Recv(e.ctx)
	e.checkErr("refusal with ErrCodeInvalidOp fails the sender's receive path", err, hqq.ErrInvalidOp)
	return e.barrier("end")
}
