package supervisor

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"time"
)

// State is the lifecycle state of a service.
type State string

const (
	// StateStopped: not running and not wanted to run.
	StateStopped State = "stopped"
	// StateStarting: the process runs, readiness is not reached yet.
	StateStarting State = "starting"
	// StateReady: the process runs and its health checks pass (or it has no
	// listener).
	StateReady State = "ready"
	// StateUnhealthy: the process runs, but health checks fail. With a
	// restart policy the service is restarted after Health.Failures failures.
	StateUnhealthy State = "unhealthy"
	// StateBackoff: the process exited, and a restart is scheduled.
	StateBackoff State = "backoff"
	// StateStopping: a graceful stop (or a restart) of the process is in
	// progress.
	StateStopping State = "stopping"
	// StateFailed: the service is not running and will not be restarted
	// automatically: it exited under a policy that does not restart it, the
	// restart limit was reached, or it could not be started.
	StateFailed State = "failed"
)

// Exit describes how the last process of a service ended.
type Exit struct {
	Code   int       `json:"code"`             // exit code; -1 if killed by a signal
	Signal string    `json:"signal,omitempty"` // terminating signal, if any
	At     time.Time `json:"at"`
}

// HealthResult is the outcome of the last health check.
type HealthResult struct {
	OK      bool      `json:"ok"`
	At      time.Time `json:"at"`
	Latency Duration  `json:"latency"`
	Error   string    `json:"error,omitempty"`
}

// Status is a snapshot of one service.
type Status struct {
	Name        string        `json:"name"`
	State       State         `json:"state"`
	PID         int           `json:"pid,omitempty"`
	StartedAt   *time.Time    `json:"started_at,omitempty"`
	Uptime      Duration      `json:"uptime,omitempty"`
	Restarts    int           `json:"restarts"`
	NextRestart *time.Time    `json:"next_restart,omitempty"`
	LastExit    *Exit         `json:"last_exit,omitempty"`
	Health      *HealthResult `json:"health,omitempty"`
	LastError   string        `json:"last_error,omitempty"`
}

// Event reports a state change of a service.
type Event struct {
	Time    time.Time `json:"time"`
	Service string    `json:"service"`
	State   State     `json:"state"`
	PID     int       `json:"pid,omitempty"`
	Message string    `json:"message,omitempty"`
}

var (
	// ErrUnknownService is returned for a service name the Supervisor does not
	// manage.
	ErrUnknownService = errors.New("supervisor: unknown service")
	// ErrShutdown is returned by operations after Shutdown or after Run ended.
	ErrShutdown = errors.New("supervisor: shut down")
	// ErrLockHeld is returned when a service's exclusive_lock is held by
	// another process.
	ErrLockHeld = errors.New("supervisor: exclusive lock held by another process")
)

// Option configures a Supervisor.
type Option func(*Supervisor)

// WithEvents registers fn to receive the events of all services. fn is called
// from the goroutines of the services, possibly concurrently; it must not
// block, and must not call methods of the Supervisor, which would deadlock.
func WithEvents(fn func(Event)) Option {
	return func(s *Supervisor) { s.events = fn }
}

// Supervisor runs and watches a set of services. The zero value is not
// usable; create one with New. All methods are safe for concurrent use.
type Supervisor struct {
	events func(Event)

	mu       sync.Mutex
	cfg      Config
	services map[string]*service
	closed   bool
}

// New validates cfg and creates a Supervisor for it. The services are not
// started: call Run, or Start for each one. New starts one goroutine per
// service, which Shutdown (or the end of Run) stops.
func New(cfg Config, opts ...Option) (*Supervisor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s := &Supervisor{services: make(map[string]*service, len(cfg.Services))}
	for _, o := range opts {
		o(s)
	}
	s.cfg = cfg.clone()
	for _, spec := range s.cfg.Services {
		s.services[spec.Name] = s.newService(spec)
	}
	return s, nil
}

// Run starts the services with autostart set and blocks until ctx is done,
// then shuts every service down gracefully and returns nil. A service that
// fails to start is reported as failed; Run does not stop for it.
func (s *Supervisor) Run(ctx context.Context) error {
	s.mu.Lock()
	var auto []string
	for _, spec := range s.cfg.Services {
		if spec.autostart() {
			auto = append(auto, spec.Name)
		}
	}
	s.mu.Unlock()
	for _, name := range auto {
		_ = s.Start(ctx, name)
	}
	<-ctx.Done()
	return s.Shutdown(context.Background())
}

// Shutdown stops every service gracefully (see ServiceSpec.StopTimeout) and
// ends their goroutines. It is idempotent; the operations called after it
// return ErrShutdown.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	svcs := s.allLocked()
	s.mu.Unlock()
	return stopAll(ctx, svcs)
}

// Start starts the service, or does nothing if it runs. It returns once the
// process is spawned (not once it is ready), or the error that prevented it.
func (s *Supervisor) Start(ctx context.Context, name string) error {
	return s.request(ctx, name, cmdStart)
}

// Stop stops the service gracefully and returns once its process exited. A
// stopped service is not restarted, and its exclusive lock is released.
func (s *Supervisor) Stop(ctx context.Context, name string) error {
	return s.request(ctx, name, cmdStop)
}

// Restart stops the service if it runs and starts it again. The restart does
// not count toward the restart limit.
func (s *Supervisor) Restart(ctx context.Context, name string) error {
	return s.request(ctx, name, cmdRestart)
}

// Status returns the snapshot of one service.
func (s *Supervisor) Status(name string) (Status, error) {
	s.mu.Lock()
	sv, ok := s.services[name]
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return Status{}, ErrShutdown
	}
	if !ok {
		return Status{}, ErrUnknownService
	}
	return sv.status(time.Now()), nil
}

// List returns the snapshots of all services, sorted by name.
func (s *Supervisor) List() []Status {
	s.mu.Lock()
	svcs := s.allLocked()
	s.mu.Unlock()
	now := time.Now()
	out := make([]Status, 0, len(svcs))
	for _, sv := range svcs {
		out = append(out, sv.status(now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Config returns a copy of the configuration in effect.
func (s *Supervisor) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.clone()
}

// Reload applies cfg. Services that are new are added (and started when
// autostart is set), services that are gone are stopped and removed, and
// services whose definition changed are restarted with the new definition if
// they run. Unchanged services are left alone. A service whose definition
// changes while it is stopped keeps being stopped. cfg must be valid.
func (s *Supervisor) Reload(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg = cfg.clone()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrShutdown
	}
	old := make(map[string]ServiceSpec, len(s.cfg.Services))
	for _, spec := range s.cfg.Services {
		old[spec.Name] = spec
	}
	next := make(map[string]*service, len(cfg.Services))
	var changed, added, autostart []*service
	var specs []ServiceSpec
	for _, spec := range cfg.Services {
		if sv, ok := s.services[spec.Name]; ok {
			next[spec.Name] = sv
			if !reflect.DeepEqual(old[spec.Name], spec) {
				changed = append(changed, sv)
				specs = append(specs, spec)
			}
			continue
		}
		sv := s.newService(spec)
		next[spec.Name] = sv
		added = append(added, sv)
		if spec.autostart() {
			autostart = append(autostart, sv)
		}
	}
	var removed []*service
	for name, sv := range s.services {
		if _, ok := next[name]; !ok {
			removed = append(removed, sv)
		}
	}
	s.services = next
	s.cfg = cfg
	s.mu.Unlock()

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	keep(stopAll(ctx, removed))
	for i, sv := range changed {
		keep(sv.request(ctx, command{kind: cmdReload, spec: specs[i]}))
	}
	for _, sv := range autostart {
		keep(sv.request(ctx, command{kind: cmdStart}))
	}
	return firstErr
}

// request sends kind to the named service and waits for its reply.
func (s *Supervisor) request(ctx context.Context, name string, kind cmdKind) error {
	s.mu.Lock()
	closed := s.closed
	sv, ok := s.services[name]
	s.mu.Unlock()
	if closed {
		return ErrShutdown
	}
	if !ok {
		return ErrUnknownService
	}
	return sv.request(ctx, command{kind: kind})
}

// allLocked returns the services; s.mu must be held.
func (s *Supervisor) allLocked() []*service {
	out := make([]*service, 0, len(s.services))
	for _, sv := range s.services {
		out = append(out, sv)
	}
	return out
}

// emit delivers an event to the registered callback, if any.
func (s *Supervisor) emit(ev Event) {
	if s.events != nil {
		s.events(ev)
	}
}

// stopAll asks every service to shut down and waits until all loops ended.
// A service that already ended is not an error.
func stopAll(ctx context.Context, svcs []*service) error {
	var firstErr error
	for _, sv := range svcs {
		if err := sv.send(ctx, command{kind: cmdShutdown}); err != nil && err != ErrShutdown && firstErr == nil {
			firstErr = err
		}
	}
	for _, sv := range svcs {
		select {
		case <-sv.done:
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			return firstErr
		}
	}
	return firstErr
}
