// Package fakeserver is a protocol-compatible stand-in for the C SQLite
// service (examples/sqlite/server). It speaks the hqq-sqlite protocol of
// examples/sqlite/PROTOCOL.md over real HQQ links, and runs each session on an
// in-memory engine with a small SQL subset (see sql.go). It exists so the Go
// driver can be tested end to end without cgo or a C toolchain.
//
// The engine is not SQLite. It implements the protocol's observable behavior
// that the driver depends on: sessions, transactions with a single writer,
// busy timeouts, interrupts on request cancellation, statement and cursor ids,
// batches, and result codes for constraint violations.
//
// Requests run on the worker pool of hqq.AdvancedLink.Serve. Requests of one
// session are serialized by the session's mutex, so a session never runs two
// requests at once (PROTOCOL.md section 13). Unlike the recommended C design,
// the session has no dedicated worker goroutine.
package fakeserver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"gosuda.org/hqq"
	"gosuda.org/hqq/examples/sqlite/wire"
)

// Options configures a Server. The zero value serves raw database names and
// hqq's default heartbeat settings.
type Options struct {
	// Aliases maps database aliases to the names the engine opens. An Open
	// whose database is an alias uses the mapped name.
	Aliases map[string]string

	// RequireAlias rejects any database name that is not an alias with
	// FORBIDDEN. Without it, every name is opened as given.
	RequireAlias bool

	// MaxSessions bounds the open sessions. Zero selects wire.MaxSessions.
	MaxSessions int

	// Link configures the heartbeat and timeouts of every accepted link.
	Link hqq.LinkOptions
}

// Stats are counters of a Server, for tests and monitoring.
type Stats struct {
	Sessions    int    // sessions open now
	Opened      uint64 // sessions opened in total
	Requests    uint64 // requests received
	Interrupted uint64 // requests that ended with SQLITE_INTERRUPT
	RolledBack  uint64 // sessions torn down with an active transaction
}

// Server is a running fake service.
type Server struct {
	name string
	opts Options
	acc  *hqq.Acceptor
	eng  *engine
	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu    sync.Mutex
	links map[*linkState]struct{}

	sessions    atomic.Int64
	opened      atomic.Uint64
	requests    atomic.Uint64
	interrupted atomic.Uint64
	rolledBack  atomic.Uint64
}

// Listen starts a server on the HQQ listener name and returns once the listener
// is published. Clients may Connect as soon as Listen returns. ctx bounds only
// the start; the server runs until Close.
func Listen(ctx context.Context, name string, opts Options) (*Server, error) {
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = wire.MaxSessions
	}
	acc, err := hqq.NewAcceptorContext(ctx, name, hqq.AcceptorConfig{
		Features: hqq.FeatureRequestResponse | hqq.FeatureCancel | hqq.FeatureMetadata,
		Advanced: hqq.AdvancedOptions{
			MaxMessageSize: wire.MaxMessageBytes,
			Link:           opts.Link,
		},
		Backlog:       64,
		MaxHandshakes: 64,
	})
	if err != nil {
		return nil, err
	}
	sctx, stop := context.WithCancel(context.Background())
	s := &Server{
		name:  name,
		opts:  opts,
		acc:   acc,
		eng:   newEngine(),
		ctx:   sctx,
		stop:  stop,
		links: map[*linkState]struct{}{},
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Name returns the listener name.
func (s *Server) Name() string { return s.name }

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	return Stats{
		Sessions:    int(s.sessions.Load()),
		Opened:      s.opened.Load(),
		Requests:    s.requests.Load(),
		Interrupted: s.interrupted.Load(),
		RolledBack:  s.rolledBack.Load(),
	}
}

// Close stops accepting, closes every link (their sessions roll back, as in a
// client crash) and waits for the server goroutines. It is idempotent.
func (s *Server) Close() error {
	s.stop()
	err := s.acc.Close()
	s.mu.Lock()
	for ls := range s.links {
		_ = ls.link.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	if errors.Is(err, hqq.ErrAcceptorClosed) {
		return nil
	}
	return err
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		link, err := s.acc.Accept(s.ctx)
		if err != nil {
			return
		}
		ls := &linkState{
			srv:      s,
			link:     link,
			sessions: map[uint64]*session{},
			changed:  make(chan struct{}),
		}
		s.mu.Lock()
		if s.ctx.Err() != nil {
			s.mu.Unlock()
			_ = link.Close()
			return
		}
		s.links[ls] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ls.run(s.ctx)
			s.mu.Lock()
			delete(s.links, ls)
			s.mu.Unlock()
		}()
	}
}

// linkState is one accepted link: the sessions (streams) of one client.
type linkState struct {
	srv  *Server
	link *hqq.NamedAdvancedLink

	mu       sync.Mutex
	sessions map[uint64]*session
	changed  chan struct{} // closed and replaced when a session is registered
}

// run serves the link until it ends. Sessions end with it.
func (ls *linkState) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer ls.link.Close()
	if err := ls.link.ListenStreams(ctx); err != nil {
		return
	}
	var serveWG sync.WaitGroup
	serveWG.Add(1)
	go func() {
		defer serveWG.Done()
		_ = ls.link.Serve(ctx, hqq.HandlerFunc(ls.serveRequest))
		cancel()
	}()
	for {
		st, err := ls.link.AcceptStream(ctx)
		if err != nil {
			break
		}
		sess := newSession(ls.srv, ls, st)
		ls.register(sess)
		go sess.watch(ctx)
	}
	cancel()
	serveWG.Wait()
}

func (ls *linkState) register(sess *session) {
	ls.mu.Lock()
	ls.sessions[sess.st.ID()] = sess
	close(ls.changed)
	ls.changed = make(chan struct{})
	ls.mu.Unlock()
}

// lookup returns the session of a stream. A request can arrive before the
// accept loop has registered its stream, so lookup waits for the registration.
// It returns when ctx ends, which happens when the stream or the link closes.
func (ls *linkState) lookup(ctx context.Context, id uint64) (*session, error) {
	if id == 0 {
		return nil, errNoSession
	}
	for {
		ls.mu.Lock()
		sess := ls.sessions[id]
		changed := ls.changed
		ls.mu.Unlock()
		if sess != nil {
			return sess, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

var errNoSession = wire.Errorf(wire.ProtoNoSession, "no session on this stream")

// serveRequest is the handler of every request of the link.
func (ls *linkState) serveRequest(ctx context.Context, w *hqq.ResponseWriter, req *hqq.Request) {
	ls.srv.requests.Add(1)
	sess, err := ls.lookup(ctx, req.StreamID())
	if err != nil {
		if ctx.Err() == nil {
			writeError(w, err)
		}
		return
	}
	body, err := sess.handle(ctx, wire.Method(req.Method()), req.Payload())
	if ctx.Err() != nil {
		return // the caller gave up: HQQ discards the response anyway
	}
	if err != nil {
		writeError(w, err)
		return
	}
	_, _ = w.Write(body)
}

// writeError sends err as an error response with its status and message.
func writeError(w *hqq.ResponseWriter, err error) {
	status, msg := statusOf(err)
	w.WriteError(uint64(status), []byte(msg))
}

// statusOf maps an error of the session to a wire status and message.
func statusOf(err error) (wire.Status, string) {
	var pe *wire.ProtocolError
	if errors.As(err, &pe) {
		return wire.ProtocolStatus(pe.Code), pe.Message
	}
	var se *sqlError
	if errors.As(err, &se) {
		return wire.SQLiteStatus(se.rc), se.msg
	}
	return wire.ProtocolStatus(wire.ProtoInternal), err.Error()
}

// resolve maps a database name from Open to the engine name.
func (s *Server) resolve(db string) (string, error) {
	if name, ok := s.opts.Aliases[db]; ok {
		return name, nil
	}
	if s.opts.RequireAlias {
		return "", wire.Errorf(wire.ProtoForbidden, "database %q is not an alias", db)
	}
	return db, nil
}

// reserveSession takes a session slot, or reports the limit.
func (s *Server) reserveSession() bool {
	for {
		n := s.sessions.Load()
		if int(n) >= s.opts.MaxSessions {
			return false
		}
		if s.sessions.CompareAndSwap(n, n+1) {
			return true
		}
	}
}
