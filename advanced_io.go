package hqq

import (
	"context"
	"slices"
	"sync"
	"time"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
	"gosuda.org/hqq/internal/race"
)

// AdvancedMessage is one complete Advanced Protocol message.
type AdvancedMessage struct {
	ConnectionID uint64 // logical stream, 0 for link-level traffic
	MessageID    uint64 // sender-assigned ID; responses reuse the request's ID
	MethodID     uint64 // method selector of a request
	Status       uint64 // status of a response (error code if IsError)
	Flags        uint64 // AdvFlag* bits
	Payload      []byte // owned by the receiver

	// abortErr is why an aborted response fails its waiting call
	// (ErrMessageAborted or ErrReassemblyLimit). It is never set on messages
	// handed to applications.
	abortErr error
}

// IsData reports whether m is a plain large-copy message (SendLarge).
func (m AdvancedMessage) IsData() bool {
	return m.Flags&(AdvFlagRequest|AdvFlagResponse|AdvFlagAbort) == 0
}

// IsRequest reports whether m is a request (answer it with Respond or
// RespondError).
func (m AdvancedMessage) IsRequest() bool {
	return m.Flags&AdvFlagRequest != 0
}

// IsResponse reports whether m is a response to a request.
func (m AdvancedMessage) IsResponse() bool {
	return m.Flags&AdvFlagResponse != 0
}

// IsError reports whether m is an error response.
func (m AdvancedMessage) IsError() bool {
	return m.Flags&AdvFlagError != 0
}

// IsAbort reports whether m is the abort notification of a message the
// sender abandoned mid-transfer. The package consumes these internally:
// Receive, Serve handlers and CallOn never return an aborted message (see
// Receive and ErrMessageAborted), so applications only see IsAbort false.
func (m AdvancedMessage) IsAbort() bool {
	return m.Flags&AdvFlagAbort != 0
}

type advancedMessageKey struct {
	connectionID uint64
	messageID    uint64
	kind         uint64
}

type advancedCallKey struct {
	connectionID uint64
	messageID    uint64
}

type advancedDispatchResult struct {
	message AdvancedMessage
	err     error
}

type advancedAssembly struct {
	message AdvancedMessage
	data    []byte
	next    uint64
	charge  uint64 // bytes charged against AdvancedLink.maxReassemblyBytes
}

// pendingPool recycles the one-shot channels used by Call. A channel may be
// returned only while it is guaranteed empty and unreachable: after a
// waiter received its single value, or after the waiter itself removed the
// call's entry from pendingCalls (see unregisterPendingCall). Whoever
// removes an entry is the only goroutine that may send on its channel, so a
// channel whose entry was removed by someone else may still receive a value
// and must be left to the garbage collector.
var pendingPool = sync.Pool{
	New: func() any { return make(chan advancedDispatchResult, 1) },
}

// SendLarge publishes payload as one Advanced large-copy message, split into
// OpConnCopy chunks of at most BufferSize bytes. The peer receives it as one
// AdvancedMessage from Receive.
func (l *AdvancedLink) SendLarge(ctx context.Context, connectionID uint64, payload []byte) (uint64, error) {
	if err := l.requireFeature(FeatureLargeCopy); err != nil {
		return 0, err
	}
	messageID := l.nextMessageID()
	if err := l.sendAdvancedPayload(orBackground(ctx), connectionID, messageID, 0, 0, payload); err != nil {
		return 0, err
	}
	l.stats.largeMessages.Add(1)
	l.stats.largeBytes.Add(uint64(len(payload)))
	return messageID, nil
}

// Receive returns the next complete Advanced Protocol message that is not a
// response to one of this endpoint's calls: data messages and requests.
//
// Messages the sender abandoned mid-transfer (AdvFlagAbort, after a failed
// multi-chunk send) were never complete and are dropped silently: Receive
// and Serve never deliver them, not even to a MessageHandler. An aborted
// response instead fails the waiting Call or CallOn with ErrMessageAborted.
func (l *AdvancedLink) Receive(ctx context.Context) (AdvancedMessage, error) {
	ctx = orBackground(ctx)
	if err := l.requireNegotiated(); err != nil {
		return AdvancedMessage{}, err
	}
	for {
		if l.dispatchStarted.Load() {
			return l.receiveFromDispatcher(ctx)
		}
		// A synchronous Call may have queued messages it received for us.
		select {
		case result := <-l.incoming:
			return result.message, result.err
		default:
		}
		if !l.receiveMu.TryLock() {
			// Another goroutine is receiving: route through the dispatcher
			// instead of competing for frames.
			if err := l.startDispatcher(); err != nil {
				return AdvancedMessage{}, err
			}
			continue
		}
		if l.dispatchStarted.Load() {
			l.receiveMu.Unlock()
			continue
		}
		message, err := l.receiveNextLocked(ctx)
		l.receiveMu.Unlock()
		if err != nil {
			return AdvancedMessage{}, err
		}
		if message.IsResponse() && l.routeResponse(message) {
			continue
		}
		return message, nil
	}
}

func (l *AdvancedLink) receiveFromDispatcher(ctx context.Context) (AdvancedMessage, error) {
	select {
	case result := <-l.incoming:
		return result.message, result.err
	case <-ctx.Done():
		return AdvancedMessage{}, ctx.Err()
	case <-l.dispatchDone:
		// Deliver messages queued before the dispatcher stopped first.
		select {
		case result := <-l.incoming:
			return result.message, result.err
		default:
		}
		return AdvancedMessage{}, l.dispatchFailure()
	}
}

// Call sends a link-level request and waits for its response. An error
// response is returned as AdvancedResponseError; a response the peer
// abandoned mid-transfer fails with ErrMessageAborted, and one this
// endpoint refused under AdvancedOptions.MaxReassemblyBytes with
// ErrReassemblyLimit.
func (l *AdvancedLink) Call(ctx context.Context, methodID uint64, request []byte) ([]byte, error) {
	response, err := l.CallOn(ctx, 0, methodID, request)
	if err != nil {
		return nil, err
	}
	if response.IsError() {
		return nil, AdvancedResponseError{Status: response.Status, Payload: response.Payload}
	}
	return response.Payload, nil
}

// CallOn sends a request on connectionID and waits for the matching response.
// Many calls may be outstanding at once; responses may arrive in any order.
// If ctx ends first, the call returns ctx.Err() and a late response is
// discarded. CallOn returns ErrMessageAborted or ErrReassemblyLimit under
// the same conditions as Call.
func (l *AdvancedLink) CallOn(ctx context.Context, connectionID uint64, methodID uint64, request []byte) (AdvancedMessage, error) {
	if err := l.requireFeature(FeatureRequestResponse); err != nil {
		return AdvancedMessage{}, err
	}
	ctx = orBackground(ctx)
	messageID := l.nextMessageID()
	key := advancedCallKey{connectionID: connectionID, messageID: messageID}
	pending := l.registerPendingCall(key)

	if err := l.sendAdvancedPayload(ctx, connectionID, messageID, AdvFlagRequest, methodID, request); err != nil {
		l.unregisterPendingCall(key, pending)
		return AdvancedMessage{}, err
	}
	l.stats.requests.Add(1)
	return l.awaitResponse(ctx, key, pending)
}

// awaitResponse waits for the response to key. It receives synchronously
// when nobody else is receiving (lowest latency) and otherwise lets the
// dispatcher route the response.
func (l *AdvancedLink) awaitResponse(ctx context.Context, key advancedCallKey, pending chan advancedDispatchResult) (AdvancedMessage, error) {
	for {
		select {
		case result := <-pending:
			pendingPool.Put(pending)
			return result.message, result.err
		default:
		}

		if l.dispatchStarted.Load() {
			select {
			case result := <-pending:
				pendingPool.Put(pending)
				return result.message, result.err
			case <-ctx.Done():
				l.abandonCall(key, pending)
				return AdvancedMessage{}, ctx.Err()
			case <-l.dispatchDone:
				l.abandonCall(key, pending)
				return AdvancedMessage{}, l.dispatchFailure()
			}
		}

		if !l.receiveMu.TryLock() {
			if err := l.startDispatcher(); err != nil {
				l.unregisterPendingCall(key, pending)
				return AdvancedMessage{}, err
			}
			continue
		}
		if l.dispatchStarted.Load() {
			l.receiveMu.Unlock()
			continue
		}
		// A concurrent receiver may have routed our response already.
		select {
		case result := <-pending:
			l.receiveMu.Unlock()
			pendingPool.Put(pending)
			return result.message, result.err
		default:
		}
		message, err := l.receiveNextLocked(ctx)
		l.receiveMu.Unlock()
		if err != nil {
			l.abandonCall(key, pending)
			return AdvancedMessage{}, err
		}
		if message.IsResponse() {
			if message.ConnectionID == key.connectionID && message.MessageID == key.messageID {
				l.unregisterPendingCall(key, pending)
				if message.IsAbort() {
					return AdvancedMessage{}, message.abortError()
				}
				return message, nil
			}
			if l.routeResponse(message) {
				continue
			}
		}
		if err := l.deliverIncoming(ctx, message); err != nil {
			l.abandonCall(key, pending)
			return AdvancedMessage{}, err
		}
	}
}

// Respond sends a successful response for request.
func (l *AdvancedLink) Respond(ctx context.Context, request AdvancedMessage, status uint64, response []byte) error {
	if !request.IsRequest() {
		return ErrInvalidOp
	}
	if err := l.requireFeature(FeatureRequestResponse); err != nil {
		return err
	}
	if err := l.sendAdvancedPayload(orBackground(ctx), request.ConnectionID, request.MessageID, AdvFlagResponse, status, response); err != nil {
		return err
	}
	l.stats.responses.Add(1)
	return nil
}

// RespondError sends an error response for request. A zero status is sent as
// ErrCodeInvalidOp.
func (l *AdvancedLink) RespondError(ctx context.Context, request AdvancedMessage, status uint64, response []byte) error {
	if !request.IsRequest() {
		return ErrInvalidOp
	}
	if err := l.requireFeature(FeatureRequestResponse); err != nil {
		return err
	}
	if status == 0 {
		status = uint64(ErrCodeInvalidOp)
	}
	if err := l.sendAdvancedPayload(orBackground(ctx), request.ConnectionID, request.MessageID, AdvFlagResponse|AdvFlagError, status, response); err != nil {
		return err
	}
	l.stats.responses.Add(1)
	return nil
}

func (l *AdvancedLink) nextMessageID() uint64 {
	if id := l.messageIDGenerator.Add(1); id != 0 {
		return id
	}
	return l.messageIDGenerator.Add(1)
}

// sendAdvancedPayload publishes payload as one logical message.
func (l *AdvancedLink) sendAdvancedPayload(ctx context.Context, connectionID, messageID, baseFlags, aux uint64, payload []byte) error {
	totalSize := uint64(len(payload))
	if l.maxMessageSize > 0 && totalSize > l.maxMessageSize {
		return ErrBufferOverflow
	}
	s := l.slink
	if err := s.enterTx(true); err != nil {
		return err
	}
	defer s.exitTx()

	c := mpmc.Cancel{Ctx: ctx, Closed: s.closeCh, Check: s.checkFn}
	if totalSize == 0 {
		return mapCtxErr(ctx, l.sendAdvancedChunk(&c, connectionID, messageID, 0, 0, AdvFlagBegin|AdvFlagEnd|baseFlags, aux, nil))
	}

	chunkSize := uint64(s.bufferSize)
	for offset := uint64(0); offset < totalSize; offset += chunkSize {
		end := min(offset+chunkSize, totalSize)
		flags := baseFlags
		if offset == 0 {
			flags |= AdvFlagBegin
		}
		if end == totalSize {
			flags |= AdvFlagEnd
		}
		chunkAux := uint64(0)
		if offset == 0 {
			chunkAux = aux
		}
		if err := l.sendAdvancedChunk(&c, connectionID, messageID, totalSize, offset, flags, chunkAux, payload[offset:end]); err != nil {
			if offset > 0 {
				// Tell the receiver to drop the partial message. Use a short
				// independent deadline: ctx may already be done.
				abort := mpmc.Cancel{Closed: s.closeCh, Check: s.checkFn, Deadline: time.Now().Add(100 * time.Millisecond)}
				_ = l.sendAdvancedChunk(&abort, connectionID, messageID, 0, 0, AdvFlagAbort|baseFlags, 0, nil)
			}
			return mapCtxErr(ctx, err)
		}
	}
	return nil
}

func (l *AdvancedLink) sendAdvancedChunk(c *mpmc.Cancel, connectionID, messageID, totalSize, chunkOffset, flags, aux uint64, chunk []byte) error {
	s := l.slink
	return s.txRing.EnqueueZeroCopyCancel(c, func(slot uint64, p *protocol.Packet) {
		if len(chunk) > 0 {
			race.Copy(s.txBufs[slot], chunk)
		}
		*p = protocol.NewConnCopyPacket(connectionID, messageID, totalSize, chunkOffset, uint64(len(chunk)), flags, aux)
	})
}

// receiveNextLocked returns the next message that needs routing, handling
// control packets inline. The caller holds receiveMu.
//
// It returns complete messages and aborted responses (abortErr set), which
// the caller routes to the waiting call. Aborted messages that are not
// responses and the remaining chunks of refused messages are dropped here;
// a refused request is answered with an error response.
func (l *AdvancedLink) receiveNextLocked(ctx context.Context) (AdvancedMessage, error) {
	rxBufs := l.slink.rxBufs
	for {
		var (
			message  AdvancedMessage
			outcome  chunkOutcome
			frameErr error
			control  protocol.Packet
			kind     int // 0: data/none, 1: control, 2: error packet
		)
		err := l.dequeue(ctx, func(slot uint64, p *protocol.Packet) {
			switch p.Op() {
			case protocol.OpStandardLinkTombstone:
			case protocol.OpConnCopy:
				message, outcome, frameErr = l.consumeAdvancedChunk(p, rxBufs[slot])
			case protocol.OpConnCreate, protocol.OpConnAccept, protocol.OpConnClose:
				control, kind = *p, 1
			case protocol.OpError:
				control, kind = *p, 2
			default:
				frameErr = ErrUnexpectedPacket
			}
		})
		switch {
		case err != nil:
			return AdvancedMessage{}, err
		case frameErr != nil:
			return AdvancedMessage{}, frameErr
		case kind == 1:
			if err := l.handleControlPacket(ctx, &control); err != nil {
				return AdvancedMessage{}, err
			}
		case kind == 2:
			if !l.routeControlError(&control) {
				return AdvancedMessage{}, errorFromCode(ErrorCode(control.Operand(1)))
			}
		case outcome == chunkComplete:
			return message, nil
		case outcome == chunkAborted:
			if message.IsResponse() {
				return message, nil
			}
			// An aborted request or data message was never complete.
		case outcome == chunkRefused:
			l.stats.droppedMessages.Add(1)
			switch {
			case message.IsResponse():
				return message, nil
			case message.IsRequest():
				// Best effort: the peer's Call otherwise waits for a
				// response that will never come.
				_ = l.RespondError(ctx, message, uint64(ErrCodeBufferOverflow), nil)
			}
		}
	}
}

// chunkOutcome tells what consumeAdvancedChunk did with a frame.
type chunkOutcome uint8

const (
	// chunkPartial: the frame was consumed (or drained) and nothing is
	// ready for the caller.
	chunkPartial chunkOutcome = iota
	// chunkComplete: the final chunk arrived; the message is assembled.
	chunkComplete
	// chunkAborted: the sender abandoned the message. The returned message
	// carries AdvFlagAbort; for a response its abortErr is ErrMessageAborted.
	chunkAborted
	// chunkRefused: the first chunk would exceed maxReassemblyBytes and the
	// message was refused without allocating. The returned message carries
	// AdvFlagAbort; for a response its abortErr is ErrReassemblyLimit.
	chunkRefused
)

// consumeAdvancedChunk validates one OpConnCopy frame and adds it to its
// message assembly. The returned message is meaningful for every outcome
// except chunkPartial.
func (l *AdvancedLink) consumeAdvancedChunk(p *protocol.Packet, buffer []byte) (AdvancedMessage, chunkOutcome, error) {
	connectionID := p.Operand(0)
	messageID := p.Operand(1)
	totalSize := p.Operand(2)
	chunkOffset := p.Operand(3)
	chunkSize := p.Operand(4)
	flags := p.Operand(5)
	aux := p.Operand(6)
	if flags&^advancedKnownFlags != 0 ||
		flags&AdvFlagRequest != 0 && flags&AdvFlagResponse != 0 ||
		flags&AdvFlagError != 0 && flags&AdvFlagResponse == 0 {
		return AdvancedMessage{}, chunkPartial, ErrProtocol
	}
	kind := flags & (AdvFlagRequest | AdvFlagResponse)
	key := advancedMessageKey{connectionID: connectionID, messageID: messageID, kind: kind}
	header := AdvancedMessage{
		ConnectionID: connectionID,
		MessageID:    messageID,
		MethodID:     methodIDFromFlags(flags, aux),
		Status:       statusFromFlags(flags, aux),
		Flags:        flags,
	}

	if flags&AdvFlagAbort != 0 {
		l.dropAssembly(key)
		if l.forgetRejected(key) {
			// Already surfaced when the message was refused.
			return AdvancedMessage{}, chunkPartial, nil
		}
		if flags&AdvFlagResponse != 0 {
			header.abortErr = ErrMessageAborted
		}
		return header, chunkAborted, nil
	}

	fail := func(err error) (AdvancedMessage, chunkOutcome, error) {
		l.dropAssembly(key)
		l.forgetRejected(key)
		return AdvancedMessage{}, chunkPartial, err
	}
	switch {
	case chunkSize > uint64(len(buffer)) || chunkOffset > totalSize || chunkSize > totalSize-chunkOffset:
		return fail(ErrInvalidSize)
	case l.maxMessageSize > 0 && totalSize > l.maxMessageSize:
		return fail(ErrBufferOverflow)
	case totalSize == 0 && (chunkOffset != 0 || chunkSize != 0 || flags&(AdvFlagBegin|AdvFlagEnd) != AdvFlagBegin|AdvFlagEnd):
		return fail(ErrInvalidSize)
	case totalSize > 0 && chunkSize == 0:
		return fail(ErrInvalidSize)
	case chunkOffset != 0 && aux != 0:
		return fail(ErrInvalidSize)
	case totalSize > uint64(^uint(0)>>1):
		return fail(ErrBufferOverflow)
	}

	assembly := l.assemblies[key]
	if flags&AdvFlagBegin != 0 {
		if chunkOffset != 0 || assembly != nil || l.isRejected(key) {
			return fail(ErrInvalidSize)
		}
		if flags&AdvFlagEnd != 0 {
			// Single-chunk fast path: no assembly state.
			if chunkSize != totalSize {
				return fail(ErrInvalidSize)
			}
			header.Payload = make([]byte, int(totalSize))
			race.Copy(header.Payload, buffer[:chunkSize])
			return header, chunkComplete, nil
		}
		header.Flags &^= AdvFlagBegin | AdvFlagEnd
		charge := totalSize + advancedAssemblyOverhead
		if charge > l.maxReassemblyBytes || l.assemblyBytes > l.maxReassemblyBytes-charge {
			l.rejectMessage(key)
			header.Flags |= AdvFlagAbort
			if flags&AdvFlagResponse != 0 {
				header.abortErr = ErrReassemblyLimit
			}
			return header, chunkRefused, nil
		}
		assembly = &advancedAssembly{message: header, data: make([]byte, int(totalSize)), charge: charge}
		l.assemblies[key] = assembly
		l.assemblyBytes += charge
	}
	if assembly == nil {
		if l.isRejected(key) {
			// Drain the remaining chunks of a refused message.
			if flags&AdvFlagEnd != 0 {
				l.forgetRejected(key)
			}
			return AdvancedMessage{}, chunkPartial, nil
		}
		return AdvancedMessage{}, chunkPartial, ErrProtocol
	}
	if chunkOffset != assembly.next || uint64(len(assembly.data)) != totalSize {
		return fail(ErrInvalidSize)
	}
	race.Copy(assembly.data[chunkOffset:chunkOffset+chunkSize], buffer[:chunkSize])
	assembly.next += chunkSize

	if flags&AdvFlagEnd == 0 {
		return AdvancedMessage{}, chunkPartial, nil
	}
	if assembly.next != totalSize {
		return fail(ErrInvalidSize)
	}
	l.dropAssembly(key)
	assembly.message.Flags |= AdvFlagBegin | AdvFlagEnd
	assembly.message.Payload = assembly.data
	return assembly.message, chunkComplete, nil
}

// dropAssembly discards the incomplete message key and releases its charge.
func (l *AdvancedLink) dropAssembly(key advancedMessageKey) {
	if assembly := l.assemblies[key]; assembly != nil {
		l.assemblyBytes -= assembly.charge
		delete(l.assemblies, key)
	}
}

func (l *AdvancedLink) isRejected(key advancedMessageKey) bool {
	_, ok := l.rejected[key]
	return ok
}

// rejectMessage remembers that the chunks of key must be discarded. At most
// maxRejectedMessages keys are remembered; a message pushed out of the set
// by that many newer refusals fails with ErrProtocol on its next chunk.
func (l *AdvancedLink) rejectMessage(key advancedMessageKey) {
	if l.rejected == nil {
		l.rejected = make(map[advancedMessageKey]struct{})
	}
	if len(l.rejectedOrder) >= maxRejectedMessages {
		delete(l.rejected, l.rejectedOrder[0])
		l.rejectedOrder = l.rejectedOrder[1:]
	}
	l.rejected[key] = struct{}{}
	l.rejectedOrder = append(l.rejectedOrder, key)
}

// forgetRejected stops discarding key and reports whether it was refused.
func (l *AdvancedLink) forgetRejected(key advancedMessageKey) bool {
	if !l.isRejected(key) {
		return false
	}
	delete(l.rejected, key)
	if i := slices.Index(l.rejectedOrder, key); i >= 0 {
		l.rejectedOrder = slices.Delete(l.rejectedOrder, i, i+1)
	}
	return true
}

// abortError returns why an aborted response fails its call.
func (m AdvancedMessage) abortError() error {
	if m.abortErr != nil {
		return m.abortErr
	}
	return ErrMessageAborted
}

func methodIDFromFlags(flags, aux uint64) uint64 {
	if flags&AdvFlagRequest != 0 {
		return aux
	}
	return 0
}

func statusFromFlags(flags, aux uint64) uint64 {
	if flags&AdvFlagResponse != 0 {
		return aux
	}
	return 0
}
