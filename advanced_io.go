package hqq

import (
	"context"
	"slices"
	"sync"
	"time"

	"gosuda.org/hqq/internal/mpmc"
	"gosuda.org/hqq/internal/protocol"
)

type advancedMessageKey struct {
	connectionID uint64
	messageID    uint64
	kind         uint64
}

type advancedCallKey struct {
	streamID  uint64
	messageID uint64
}

// advancedDispatchResult is what a pending Call receives: the response
// message, or the error that failed the call.
type advancedDispatchResult struct {
	msg *Message
	err error
}

// advancedAssembly is the state of one incomplete multi-chunk message: the
// pooled buffer the chunks are copied into, and the offset of the next
// chunk. Assemblies are recycled through assemblyPool.
type advancedAssembly struct {
	hdr  advHeader
	msg  *Message
	size uint64 // announced message size
	next uint64
	seen uint64 // l.chunks when its last chunk arrived

	recvAt time.Time // arrival of the first chunk, set only for AdvFlagMeta messages
}

var assemblyPool = sync.Pool{
	New: func() any { return new(advancedAssembly) },
}

// charge is the number of bytes the assembly holds against
// AdvancedLink.maxReassemblyBytes: the announced message size plus the
// fixed bookkeeping overhead.
func (a *advancedAssembly) charge() uint64 {
	return a.size + advancedAssemblyOverhead
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

// receiveQueued returns the next message of q, the queue of stream st (nil
// for the request queue). It receives synchronously when nobody else is
// receiving (lowest latency), routing every message it receives to its
// destination, and otherwise lets the dispatcher fill the queue.
func (l *AdvancedLink) receiveQueued(ctx context.Context, q chan *Message, st *Stream) (*Message, error) {
	// The request queue never closes; its consumer, Serve, stops when
	// Shutdown completed.
	done := l.serveStop
	if st != nil {
		done = st.done
	}
	for {
		if st != nil {
			if st.state.Load()&streamLocalClosed != 0 {
				return nil, ErrClosed
			}
		} else if l.serveStopped.Load() {
			return nil, errServeStopped
		}
		select {
		case m := <-q:
			return m, nil
		default:
		}
		if st != nil && st.state.Load()&streamPeerClosed != 0 {
			// The peer's close is processed after every message it sent
			// before: whatever is queued now is the last of the stream.
			select {
			case m := <-q:
				return m, nil
			default:
				return nil, ErrPeerClosed
			}
		}

		if l.dispatchStarted.Load() {
			select {
			case m := <-q:
				return m, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
			case <-l.dispatchDone:
				select {
				case m := <-q:
					return m, nil
				default:
				}
				return nil, l.dispatchFailure()
			}
			continue
		}
		if !l.receiveMu.TryLock() {
			// Another goroutine is receiving: route through the dispatcher
			// instead of competing for frames.
			if err := l.startDispatcher(); err != nil {
				return nil, err
			}
			continue
		}
		if l.dispatchStarted.Load() {
			l.receiveMu.Unlock()
			continue
		}
		// A concurrent receiver may have routed a message for us already.
		select {
		case m := <-q:
			l.receiveMu.Unlock()
			return m, nil
		default:
		}
		if err := l.receiveRoute(ctx, st == nil); err != nil {
			select {
			case m := <-q:
				return m, nil
			default:
			}
			return nil, err
		}
	}
}

// awaitResponse waits for the response to key. It receives synchronously
// when nobody else is receiving (lowest latency) and otherwise lets the
// dispatcher route the response. st is the stream the call was made on: the
// wait ends when it is closed.
func (l *AdvancedLink) awaitResponse(ctx context.Context, key advancedCallKey, pending chan advancedDispatchResult, st *Stream) (*Message, error) {
	for {
		select {
		case result := <-pending:
			pendingPool.Put(pending)
			return result.msg, result.err
		default:
		}
		if err := st.closedErr(); err != nil {
			l.abandonCall(key, pending)
			return nil, err
		}

		if l.dispatchStarted.Load() {
			select {
			case result := <-pending:
				pendingPool.Put(pending)
				return result.msg, result.err
			case <-ctx.Done():
				l.giveUp(key, pending, true)
				return nil, ctx.Err()
			case <-st.done:
			case <-l.dispatchDone:
				select {
				case result := <-pending:
					pendingPool.Put(pending)
					return result.msg, result.err
				default:
				}
				l.abandonCall(key, pending)
				return nil, l.dispatchFailure()
			}
			continue
		}

		if !l.receiveMu.TryLock() {
			if err := l.startDispatcher(); err != nil {
				l.unregisterPendingCall(key, pending)
				return nil, err
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
			return result.msg, result.err
		default:
		}
		if err := l.receiveRoute(ctx, false); err != nil {
			l.giveUp(key, pending, ctx.Err() != nil)
			return nil, err
		}
	}
}

// receiveRoute receives one message and routes it. The caller holds
// receiveMu, which receiveRoute releases (or passes on, see below) before it
// returns: the lock is held while the message is routed so that messages
// reach their queues in the order they arrived.
//
// A synchronous receiver must not wait for the consumer of a full queue
// (under OverflowBlock): the delivery is handed over to a goroutine that
// takes over receiveMu, which keeps the order, and the dispatcher started by
// the callers' next iteration continues after it.
//
// serving tells that the receiver is Serve: a completed Shutdown then ends
// its wait on the ring (see serveWakeDeadline).
func (l *AdvancedLink) receiveRoute(ctx context.Context, serving bool) error {
	m, hdr, abortErr, err := l.receiveNextLocked(ctx, serving)
	if err != nil {
		l.receiveMu.Unlock()
		return err
	}
	stall, err := l.route(ctx, m, hdr, abortErr, false)
	if stall.m != nil {
		go l.finishDelivery(stall)
		return nil
	}
	l.receiveMu.Unlock()
	return err
}

func (l *AdvancedLink) nextMessageID() uint64 {
	if id := l.messageIDGenerator.Add(1); id != 0 {
		return id
	}
	return l.messageIDGenerator.Add(1)
}

// sendAdvancedPayload publishes prefix followed by payload as one logical
// message (prefix is the metadata block of a request, nil otherwise).
func (l *AdvancedLink) sendAdvancedPayload(ctx context.Context, connectionID, messageID, baseFlags, aux uint64, prefix, payload []byte) error {
	totalSize := uint64(len(prefix)) + uint64(len(payload))
	if l.maxMessageSize > 0 && totalSize > l.maxMessageSize {
		return ErrBufferOverflow
	}
	s := l.slink
	if err := s.enterTx(true); err != nil {
		return err
	}
	defer s.exitTx()

	c := l.ringCancel(ctx)
	if totalSize == 0 {
		return mapCtxErr(ctx, l.sendAdvancedChunk(&c, connectionID, messageID, 0, 0, AdvFlagBegin|AdvFlagEnd|baseFlags, aux, nil, nil))
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
		// The chunk covers bytes [offset, end) of prefix||payload.
		var head, tail []byte
		if offset < uint64(len(prefix)) {
			head = prefix[offset:min(end, uint64(len(prefix)))]
		}
		if end > uint64(len(prefix)) {
			tail = payload[max(offset, uint64(len(prefix)))-uint64(len(prefix)) : end-uint64(len(prefix))]
		}
		if err := l.sendAdvancedChunk(&c, connectionID, messageID, totalSize, offset, flags, chunkAux, head, tail); err != nil {
			if offset > 0 {
				// Tell the receiver to drop the partial message. Use a short
				// independent deadline: ctx may already be done.
				abort := mpmc.Cancel{Closed: s.closeCh, Check: s.checkFn, Deadline: time.Now().Add(100 * time.Millisecond)}
				_ = l.sendAdvancedChunk(&abort, connectionID, messageID, 0, 0, AdvFlagAbort|baseFlags, 0, nil, nil)
			}
			return mapCtxErr(ctx, err)
		}
	}
	return nil
}

// sendAdvancedChunk publishes one frame whose payload is head followed by
// tail.
func (l *AdvancedLink) sendAdvancedChunk(c *mpmc.Cancel, connectionID, messageID, totalSize, chunkOffset, flags, aux uint64, head, tail []byte) error {
	s := l.slink
	return s.txRing.EnqueueZeroCopyCancel(c, func(slot uint64, p *protocol.Packet) {
		n := copy(s.txBufs[slot], head)
		n += copy(s.txBufs[slot][n:], tail)
		*p = protocol.NewConnCopyPacket(connectionID, messageID, totalSize, chunkOffset, uint64(n), flags, aux)
	})
}

// receiveNextLocked returns the next message that needs routing, handling
// control packets inline. The caller holds receiveMu.
//
// It returns a complete message m, or, for a response the sender abandoned
// or this endpoint refused or reclaimed it (F-09), m == nil with the response
// header hdr and the reason abortErr (ErrMessageAborted or
// ErrReassemblyLimit): the caller
// fails the waiting call with it. Aborted messages that are not responses
// and the remaining chunks of refused messages are dropped here; a refused
// request is answered with an error response queued on the control sender,
// so receiveNextLocked never waits for the tx ring.
func (l *AdvancedLink) receiveNextLocked(ctx context.Context, serving bool) (m *Message, hdr advHeader, abortErr error, err error) {
	rxBufs := l.slink.rxBufs
	for {
		if n := len(l.reclaimed); n > 0 {
			// Report an assembly reclaimed for idleness (F-09) the way a
			// refusal is reported: a response fails its call, and a request
			// gets the error response a refused request gets.
			h := l.reclaimed[n-1]
			l.reclaimed = l.reclaimed[:n-1]
			if h.isResponse() {
				return nil, h, ErrReassemblyLimit, nil
			}
			l.refuseRequest(h, ErrCodeBufferOverflow)
			continue
		}
		var (
			msg      *Message
			mh       advHeader
			outcome  chunkOutcome
			frameErr error
			control  protocol.Packet
			kind     int // 0: data/none, 1: control, 2: error packet, 3: cancel
		)
		err := l.dequeue(ctx, serving, func(slot uint64, p *protocol.Packet) {
			switch p.Op() {
			case protocol.OpStandardLinkTombstone:
			case protocol.OpConnCopy:
				msg, mh, outcome, frameErr = l.consumeAdvancedChunk(p, rxBufs[slot])
			case protocol.OpConnCreate, protocol.OpConnAccept, protocol.OpConnClose:
				control, kind = *p, 1
			case protocol.OpError:
				control, kind = *p, 2
			case protocol.OpCancel:
				if l.negotiated.Load()&FeatureCancel == 0 {
					frameErr = ErrUnexpectedPacket
				} else {
					control, kind = *p, 3
				}
			default:
				frameErr = ErrUnexpectedPacket
			}
		})
		switch {
		case err != nil:
			msg.Release()
			return nil, advHeader{}, nil, err
		case frameErr != nil:
			msg.Release()
			return nil, advHeader{}, nil, frameErr
		case kind == 1:
			if err := l.handleControlPacket(ctx, &control); err != nil {
				return nil, advHeader{}, nil, err
			}
		case kind == 2:
			if !l.routeControlError(&control) {
				return nil, advHeader{}, nil, errorFromCode(ErrorCode(control.Operand(1)))
			}
		case kind == 3:
			l.handleCancelPacket(&control)
		case outcome == chunkComplete:
			return msg, mh, nil, nil
		case outcome == chunkAborted:
			if mh.isResponse() {
				return nil, mh, ErrMessageAborted, nil
			}
			// An aborted request or data message was never complete.
		case outcome == chunkRefused:
			l.stats.droppedMessages.Add(1)
			switch {
			case mh.isResponse():
				return nil, mh, ErrReassemblyLimit, nil
			case mh.isRequest():
				// Best effort: the peer's Call otherwise waits for a
				// response that will never come.
				l.refuseRequest(mh, ErrCodeBufferOverflow)
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
	// chunkAborted: the sender abandoned the message. The returned header
	// carries AdvFlagAbort; a response fails its call with ErrMessageAborted.
	chunkAborted
	// chunkRefused: the first chunk would exceed maxReassemblyBytes and the
	// message was refused without allocating. The returned header carries
	// AdvFlagAbort; a response fails its call with ErrReassemblyLimit.
	chunkRefused
)

// consumeAdvancedChunk validates one OpConnCopy frame and adds it to its
// message assembly. It returns the assembled message (a pooled Message the
// caller owns) for chunkComplete, and only the header for chunkAborted and
// chunkRefused.
func (l *AdvancedLink) consumeAdvancedChunk(p *protocol.Packet, buffer []byte) (*Message, advHeader, chunkOutcome, error) {
	l.chunks++
	connectionID := p.Operand(0)
	messageID := p.Operand(1)
	totalSize := p.Operand(2)
	chunkOffset := p.Operand(3)
	chunkSize := p.Operand(4)
	flags := p.Operand(5)
	aux := p.Operand(6)
	known := advancedKnownFlags
	if l.negotiated.Load()&FeatureMetadata != 0 {
		known |= AdvFlagMeta
	}
	if flags&^known != 0 ||
		flags&AdvFlagRequest != 0 && flags&AdvFlagResponse != 0 ||
		flags&AdvFlagError != 0 && flags&AdvFlagResponse == 0 ||
		flags&AdvFlagMeta != 0 && flags&AdvFlagRequest == 0 {
		return nil, advHeader{}, chunkPartial, ErrProtocol
	}
	kind := flags & (AdvFlagRequest | AdvFlagResponse)
	key := advancedMessageKey{connectionID: connectionID, messageID: messageID, kind: kind}

	if flags&AdvFlagAbort != 0 {
		l.dropAssembly(key)
		if l.forgetRejected(key) {
			// Already surfaced when the message was refused.
			return nil, advHeader{}, chunkPartial, nil
		}
		return nil, advHeader{connectionID, messageID, flags, aux}, chunkAborted, nil
	}

	fail := func(err error) (*Message, advHeader, chunkOutcome, error) {
		l.dropAssembly(key)
		l.forgetRejected(key)
		return nil, advHeader{}, chunkPartial, err
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
			var recvAt time.Time
			if flags&AdvFlagMeta != 0 {
				recvAt = time.Now()
			}
			m := l.pool.get(int(totalSize))
			m.advHeader = advHeader{connectionID, messageID, flags, aux}
			copy(m.data, buffer[:chunkSize])
			if flags&AdvFlagMeta != 0 {
				if err := m.stripMeta(recvAt); err != nil {
					m.Release()
					return nil, advHeader{}, chunkPartial, err
				}
			}
			return m, m.advHeader, chunkComplete, nil
		}
		header := advHeader{connectionID, messageID, flags &^ (AdvFlagBegin | AdvFlagEnd), aux}
		charge := totalSize + advancedAssemblyOverhead
		l.reclaimIdle(charge)
		if charge > l.maxReassemblyBytes || l.assemblyBytes > l.maxReassemblyBytes-charge {
			l.rejectMessage(key)
			header.flags |= AdvFlagAbort
			return nil, header, chunkRefused, nil
		}
		assembly = assemblyPool.Get().(*advancedAssembly)
		assembly.hdr = header
		assembly.msg = l.pool.get(int(totalSize))
		assembly.size = totalSize
		assembly.next = 0
		if flags&AdvFlagMeta != 0 {
			assembly.recvAt = time.Now()
		}
		l.assemblies[key] = assembly
		l.assemblyBytes += charge
	}
	if assembly == nil {
		if l.isRejected(key) || l.rejectedEvicted {
			// Drain the remaining chunks of a refused message. Once an entry
			// was evicted from the set, any orphan continuation chunk is
			// drained too: it may be the rest of an evicted refusal (F-07).
			if flags&AdvFlagEnd != 0 {
				l.forgetRejected(key)
			}
			return nil, advHeader{}, chunkPartial, nil
		}
		return nil, advHeader{}, chunkPartial, ErrProtocol
	}
	assembly.seen = l.chunks
	if chunkOffset != assembly.next || assembly.size != totalSize {
		return fail(ErrInvalidSize)
	}
	copy(assembly.msg.data[chunkOffset:chunkOffset+chunkSize], buffer[:chunkSize])
	assembly.next += chunkSize

	if flags&AdvFlagEnd == 0 {
		return nil, advHeader{}, chunkPartial, nil
	}
	if assembly.next != totalSize {
		return fail(ErrInvalidSize)
	}
	m := assembly.msg
	m.advHeader = assembly.hdr
	m.flags |= AdvFlagBegin | AdvFlagEnd
	recvAt := assembly.recvAt
	assembly.msg = nil // the message leaves with the caller
	l.dropAssembly(key)
	if m.flags&AdvFlagMeta != 0 {
		if err := m.stripMeta(recvAt); err != nil {
			m.Release()
			return nil, advHeader{}, chunkPartial, err
		}
	}
	return m, m.advHeader, chunkComplete, nil
}

// dropAssembly discards the incomplete message key, recycling its buffer, and
// releases its charge.
func (l *AdvancedLink) dropAssembly(key advancedMessageKey) {
	if assembly := l.assemblies[key]; assembly != nil {
		l.assemblyBytes -= assembly.charge()
		delete(l.assemblies, key)
		assembly.msg.Release()
		*assembly = advancedAssembly{}
		assemblyPool.Put(assembly)
	}
}

// reclaimIdle makes room for a message of charge bytes by reclaiming the
// incomplete messages that have been idle for advancedAssemblyIdleChunks
// chunks, until the message fits (F-09). A sweep visits every assembly, so it
// runs at most once per max(advancedAssemblyIdleChunks, len(assemblies))
// chunks, which amortizes its cost over the chunks that caused it.
func (l *AdvancedLink) reclaimIdle(charge uint64) {
	if charge > l.maxReassemblyBytes || l.assemblyBytes <= l.maxReassemblyBytes-charge {
		return
	}
	if l.chunks-l.chunksAtSweep < max(advancedAssemblyIdleChunks, uint64(len(l.assemblies))) {
		return
	}
	l.chunksAtSweep = l.chunks
	for key, a := range l.assemblies {
		if l.assemblyBytes <= l.maxReassemblyBytes-charge {
			return
		}
		if l.chunks-a.seen >= advancedAssemblyIdleChunks {
			l.reclaim(key, a)
		}
	}
}

// reclaim abandons the incomplete message key: its budget is released and its
// remaining chunks are drained, as for a refused message. A request or
// response is queued for reporting (see receiveNextLocked).
func (l *AdvancedLink) reclaim(key advancedMessageKey, a *advancedAssembly) {
	hdr := a.hdr
	hdr.flags |= AdvFlagAbort
	l.dropAssembly(key)
	l.rejectMessage(key)
	l.stats.droppedMessages.Add(1)
	if hdr.isRequest() || hdr.isResponse() {
		l.reclaimed = append(l.reclaimed, hdr)
	}
}

func (l *AdvancedLink) isRejected(key advancedMessageKey) bool {
	_, ok := l.rejected[key]
	return ok
}

// rejectMessage remembers that the chunks of key must be discarded. At most
// maxRejectedMessages keys are remembered; a key pushed out of the set by
// newer refusals is forgotten, and its remaining chunks are then drained
// as orphans (rejectedEvicted, F-07).
func (l *AdvancedLink) rejectMessage(key advancedMessageKey) {
	if l.rejected == nil {
		l.rejected = make(map[advancedMessageKey]struct{})
	}
	if len(l.rejectedOrder) >= maxRejectedMessages {
		delete(l.rejected, l.rejectedOrder[0])
		l.rejectedOrder = l.rejectedOrder[1:]
		l.rejectedEvicted = true
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
