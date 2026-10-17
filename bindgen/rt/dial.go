package rt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	hqq "gosuda.org/hqq"
)

var errConnectTimeout = fmt.Errorf("hqqbg: connect timeout: %w", hqq.ErrTimeout)

// grant is the OPEN response of 4.11.
type grant struct {
	count, size int
	name        string
}

// Dial runs the bootstrap and the handshake of 4.11 and returns a ready Conn.
func Dial(ctx context.Context, listener string, iface *Iface, opts Options) (_ *Conn, err error) {
	if iface == nil {
		return nil, ErrInvalidArg
	}
	cfg, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, cfg.connectTimeout)
	defer cancel()

	ctl, err := hqq.Connect(ctx, listener, hqq.Config{
		BufferCount: 4,
		BufferSize:  4096,
		Features:    hqq.FeatureRequestResponse,
		Advanced:    hqq.AdvancedOptions{Link: cfg.link},
	})
	if err != nil {
		return nil, dialError(parent, ctx, iface, err)
	}
	// The session link does not depend on the control link, so a failure to
	// close it only matters when the dial failed anyway.
	defer func() {
		if cerr := ctl.Close(); cerr != nil && err != nil {
			err = errors.Join(err, cerr)
		}
	}()

	g, err := openRequest(ctx, ctl, iface, cfg)
	if err != nil {
		return nil, dialError(parent, ctx, iface, err)
	}
	nl, err := openSession(ctx, g, cfg.link)
	if err != nil {
		return nil, errors.Join(dialError(parent, ctx, iface, err), cleanupName(g.name, err))
	}
	lim, seenReply, err := handshake(ctx, nl, iface, cfg, g)
	if err != nil {
		err = dialError(parent, ctx, iface, err)
		err = errors.Join(err, nl.Close())
		if !seenReply {
			err = errors.Join(err, cleanupName(g.name, nil))
		}
		return nil, err
	}
	return newConn(nl.StandardLink, nl.Close, iface, cfg, lim), nil
}

// dialError reports a connect failure. Errors that carry their own meaning
// are returned as they are; everything else is a transport error.
func dialError(parent, ctx context.Context, iface *Iface, err error) error {
	var (
		oe *OpenError
		me *MismatchError
		pe *ProtocolError
	)
	switch {
	case errors.As(err, &oe), errors.As(err, &me), errors.As(err, &pe):
		return err
	case parent.Err() != nil:
		return &TransportError{Err: parent.Err(), svc: iface.Service}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &TransportError{Err: errConnectTimeout, svc: iface.Service}
	}
	return &TransportError{Err: err, svc: iface.Service}
}

// openRequest sends BG_OPEN on the control link and parses the answer.
func openRequest(ctx context.Context, ctl *hqq.NamedAdvancedLink, iface *Iface, cfg config) (grant, error) {
	var req [openReq]byte
	copy(req[0:], openMagic[:])
	le.PutUint16(req[4:], protoMajor)
	le.PutUint16(req[6:], protoMinor)
	le.PutUint32(req[8:], uint32(cfg.bufferCount))
	le.PutUint32(req[12:], uint32(cfg.bufferSize))
	msg, err := ctl.Call(ctx, openMeth, req[:])
	if err != nil {
		var re hqq.ResponseError
		if errors.As(err, &re) && re.Status > openStatusBase && re.Status < openStatusBase+0x10000 {
			text := re.Payload[:min(len(re.Payload), 64)]
			return grant{}, &OpenError{Code: uint32(re.Status - openStatusBase), Msg: string(text)}
		}
		return grant{}, err
	}
	defer msg.Release()
	return parseOpenResponse(msg.Bytes(), iface)
}

func parseOpenResponse(b []byte, iface *Iface) (grant, error) {
	bad := func(reason string) (grant, error) {
		return grant{}, &ProtocolError{Reason: "OPEN response: " + reason, svc: iface.Service}
	}
	if len(b) < openResp {
		return bad("shorter than the fixed part")
	}
	count, size := le.Uint32(b[0:]), le.Uint32(b[4:])
	nameLen := int(le.Uint16(b[8:]))
	if nameLen < 1 || nameLen > openName || len(b) != openResp+nameLen {
		return bad("name length")
	}
	if count > maxBufferCount || size > maxBufferSize || !validBufferCount(int(count)) || !validBufferSize(int(size)) {
		return bad("granted geometry")
	}
	name := b[openResp:]
	for _, ch := range name {
		if ch <= ' ' || ch >= 0x7f || ch == '/' || ch == '\\' {
			return bad("name characters")
		}
	}
	return grant{count: int(count), size: int(size), name: string(name)}, nil
}

// openSession opens the session link with the retry loop of 4.11: the server
// creates the segment before it answers OPEN, but the client may still find it
// not initialized yet. Other errors are final.
func openSession(ctx context.Context, g grant, link hqq.LinkOptions) (*hqq.NamedStandardLink, error) {
	deadline, _ := ctx.Deadline()
	delay := time.Millisecond
	for {
		o := link
		if o.AttachTimeout <= 0 {
			o.AttachTimeout = hqq.DefaultAttachTimeout
		}
		if !deadline.IsZero() {
			o.AttachTimeout = min(o.AttachTimeout, max(time.Until(deadline), time.Millisecond))
		}
		nl, err := hqq.OpenStandardLink(g.name, g.count, g.size, o)
		if err == nil {
			return nl, nil
		}
		if !retryableOpen(err) {
			return nil, err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		delay = min(delay*2, 100*time.Millisecond)
	}
}

func retryableOpen(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return errors.Is(err, hqq.ErrFailedInit) &&
		!errors.Is(err, hqq.ErrIncompatibleLayout) && !errors.Is(err, hqq.ErrLinkBusy)
}

// cleanupName removes the session name when the client gives up before the
// server read HELLO (4.11). A name that is busy or has another layout is not
// the session of this server and is left alone.
func cleanupName(name string, cause error) error {
	if cause != nil && (errors.Is(cause, hqq.ErrLinkBusy) || errors.Is(cause, hqq.ErrIncompatibleLayout)) {
		return nil
	}
	if err := hqq.Unlink(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// handshake sends HELLO and reads HELLO_REPLY. seenReply reports whether the
// server answered, which means it read HELLO and removed the segment name.
func handshake(ctx context.Context, nl *hqq.NamedStandardLink, iface *Iface, cfg config, g grant) (lim Limits, seenReply bool, err error) {
	link := nl.StandardLink
	stop := context.AfterFunc(ctx, func() { _ = link.SetReadDeadline(time.Now()) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		if err := link.SetReadDeadline(dl); err != nil {
			return lim, false, err
		}
		if err := link.SetWriteDeadline(dl); err != nil {
			return lim, false, err
		}
	}

	w, err := link.ReserveWrite()
	if err != nil {
		return lim, false, err
	}
	putHello(w.Buffer(), iface.Digest, g, cfg)
	if err := w.Commit(HeaderSize + helloBody); err != nil {
		return lim, false, err
	}

	r, err := link.ReserveRead()
	if err != nil {
		return lim, false, err
	}
	frame := r.Buffer()
	var reply [HeaderSize + helloBody]byte
	n := len(frame)
	copy(reply[:], frame)
	if err := r.Release(); err != nil {
		return lim, true, err
	}
	lim, err = parseHelloReply(reply[:], n, iface, cfg, g)
	if err != nil {
		return lim, true, err
	}
	// The deadlines were the connect deadline only.
	if err := link.SetReadDeadline(time.Time{}); err != nil {
		return lim, true, err
	}
	if err := link.SetWriteDeadline(time.Time{}); err != nil {
		return lim, true, err
	}
	return lim, true, nil
}

// putHello writes the HELLO frame of 4.11 into b.
func putHello(b []byte, digest [32]byte, g grant, cfg config) {
	putHeader(b, header{kind: kindHello, total: helloBody})
	p := b[HeaderSize : HeaderSize+helloBody]
	clear(p)
	copy(p[0:], magic[:])
	le.PutUint16(p[4:], protoMajor)
	le.PutUint16(p[6:], protoMinor)
	le.PutUint32(p[12:], uint32(g.size))
	le.PutUint32(p[16:], uint32(g.count))
	le.PutUint32(p[20:], uint32(cfg.maxInFlight))
	le.PutUint32(p[24:], uint32(cfg.maxMessage))
	copy(p[32:], digest[:])
}

// parseHelloReply checks a HELLO_REPLY frame. n is the length of the frame as
// received, which may exceed len(frame).
func parseHelloReply(frame []byte, n int, iface *Iface, cfg config, g grant) (Limits, error) {
	bad := func(reason string) (Limits, error) {
		return Limits{}, &ProtocolError{Reason: "HELLO_REPLY: " + reason, svc: iface.Service}
	}
	if n != HeaderSize+helloBody {
		return bad("frame length")
	}
	h := loadHeader(frame)
	if h.kind != kindHelloReply || h.flags != 0 || h.op != 0 || h.id != 0 || h.total != helloBody {
		return bad("header")
	}
	p := frame[HeaderSize:n]
	if [4]byte(p[0:4]) != magic {
		return bad("magic")
	}
	var remote [32]byte
	copy(remote[:], p[32:64])
	if h.aux != helloOK {
		return Limits{}, &MismatchError{Reason: h.aux, Local: iface.Digest, Remote: remote}
	}
	if le.Uint16(p[4:]) != protoMajor || le.Uint16(p[6:]) > protoMinor {
		return bad("protocol revision")
	}
	if le.Uint32(p[8:]) != 0 {
		return bad("flags granted that were not requested")
	}
	if remote != iface.Digest {
		return Limits{}, &MismatchError{Reason: helloDigestMismatch, Local: iface.Digest, Remote: remote}
	}
	mif, mm, mh := le.Uint32(p[12:]), le.Uint32(p[16:]), le.Uint32(p[20:])
	if mif < 1 || uint64(mif) > uint64(cfg.maxInFlight) || uint64(mif) > uint64(g.count) {
		return bad("granted max_in_flight")
	}
	if mm < minMaxMessage || uint64(mm) > uint64(cfg.maxMessage) {
		return bad("granted max_message")
	}
	if mh < 1 || mh > maxHandleCount {
		return bad("granted max_handles")
	}
	return Limits{
		BufferCount: g.count,
		BufferSize:  g.size,
		MaxInFlight: int(mif),
		MaxMessage:  int(mm),
		MaxHandles:  int(mh),
	}, nil
}
