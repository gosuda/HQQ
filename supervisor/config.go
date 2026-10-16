package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"
)

// Config is the set of services one Supervisor manages.
type Config struct {
	Services []ServiceSpec `json:"services"`
}

// ServiceSpec describes one supervised process. Durations are strings such as
// "250ms"; every optional field has the default documented on it.
type ServiceSpec struct {
	// Name identifies the service in Status, events and ctl commands.
	// Required, unique, made of letters, digits, '-', '_' and '.'.
	Name string `json:"name"`

	// Command is the executable (a path, or a name looked up in PATH). It is
	// not run through a shell. Args are its arguments.
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`

	// Env is added to the environment of hqqd; a key already present is
	// overridden. Dir is the working directory (default: hqqd's).
	Env map[string]string `json:"env,omitempty"`
	Dir string            `json:"dir,omitempty"`

	// Listener is the HQQ listener name the service serves. With a listener,
	// readiness is a successful health check (see Health); without one, the
	// service is ready as soon as it started. Listener names must be unique
	// across services.
	Listener string `json:"listener,omitempty"`

	// Autostart starts the service when the supervisor runs or the service
	// is added by Reload. Default: true.
	Autostart *bool `json:"autostart,omitempty"`

	// Restart is the restart policy.
	Restart RestartSpec `json:"restart"`

	// StopTimeout bounds a graceful stop; the process is killed when it does
	// not exit in time. Default: 10s.
	StopTimeout Duration `json:"stop_timeout,omitempty"`

	// StopSignal is the signal of a graceful stop on unix: SIGTERM (default),
	// SIGINT, SIGHUP, SIGQUIT or SIGKILL. On Windows there is no graceful
	// signal; a service without StopMethod is killed.
	StopSignal string `json:"stop_signal,omitempty"`

	// StopMethod, when not 0, is an HQQ method ID the service serves to shut
	// itself down gracefully. hqqd calls it (empty payload) on the listener
	// before it falls back to StopSignal. It requires Listener.
	StopMethod uint64 `json:"stop_method,omitempty"`

	// Health configures readiness and liveness checks. It requires Listener.
	Health HealthSpec `json:"health"`

	// ExclusiveLock is the path of an advisory lock file. hqqd holds it from
	// the first start of the service until the service is stopped or failed,
	// so that only one supervisor runs the service at a time. Start fails
	// with ErrLockHeld while another process holds it.
	ExclusiveLock string `json:"exclusive_lock,omitempty"`

	// Output selects where the standard output and error of the service go.
	Output OutputSpec `json:"output"`
}

// RestartSpec is the restart policy of a service.
type RestartSpec struct {
	// Policy is "never", "on-failure" (default: restart after a non-zero exit,
	// a signal or a health restart) or "always" (restart after any exit).
	// "never" also disables health-triggered restarts.
	Policy string `json:"policy,omitempty"`

	// Backoff is the delay before a restart: Initial, doubled after every
	// consecutive restart, capped at Max, with Jitter.
	Backoff BackoffSpec `json:"backoff"`

	// MaxRestarts is the number of automatic restarts allowed within Window
	// (default 5). Reaching it moves the service to "failed". Explicit
	// Start, Restart and Reload do not count.
	MaxRestarts int `json:"max_restarts,omitempty"`

	// Window is the sliding window of MaxRestarts (default 1m).
	Window Duration `json:"window,omitempty"`

	// ResetAfter: a run that lasted at least this long resets the backoff
	// (default 30s).
	ResetAfter Duration `json:"reset_after,omitempty"`
}

// BackoffSpec is the exponential restart delay.
type BackoffSpec struct {
	Initial Duration `json:"initial,omitempty"` // default 500ms
	Max     Duration `json:"max,omitempty"`     // default 30s
	// Jitter is the random spread around the delay, as a fraction in [0, 1].
	// Default 0.2; an explicit 0 disables jitter.
	Jitter *float64 `json:"jitter,omitempty"`
}

// HealthSpec configures the health checks of a service with a Listener. A
// check connects to the listener and, when PingMethod is not 0, calls it.
type HealthSpec struct {
	Interval   Duration `json:"interval,omitempty"`    // default 1s
	Timeout    Duration `json:"timeout,omitempty"`     // default 1s
	Failures   int      `json:"failures,omitempty"`    // consecutive failures before a restart; default 3
	StartGrace Duration `json:"start_grace,omitempty"` // time to become ready; default 10s
	PingMethod uint64   `json:"ping_method,omitempty"` // 0: connect only
}

// OutputSpec selects the destination of the service's stdout and stderr.
type OutputSpec struct {
	// Mode is "inherit" (default: hqqd's own stdout and stderr), "file"
	// (append to Path, rotated by size) or "prefix" (hqqd's stdout and
	// stderr, every line prefixed with "<service> | ").
	Mode string `json:"mode,omitempty"`

	// Path is the log file of mode "file".
	Path string `json:"path,omitempty"`

	// MaxBytes is the size at which Path is rotated (default 10 MiB).
	MaxBytes int64 `json:"max_bytes,omitempty"`

	// MaxFiles is the number of rotated files kept: Path.1 .. Path.N
	// (default 3).
	MaxFiles int `json:"max_files,omitempty"`
}

// Duration is a time.Duration that is written and read as a string such as
// "250ms" or "1m30s".
type Duration time.Duration

// MarshalJSON encodes d as a quoted duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON decodes a quoted duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"250ms\": %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

const (
	policyNever     = "never"
	policyOnFailure = "on-failure"
	policyAlways    = "always"

	modeInherit = "inherit"
	modeFile    = "file"
	modePrefix  = "prefix"

	defaultStopTimeout   = 10 * time.Second
	defaultBackoffInit   = 500 * time.Millisecond
	defaultBackoffMax    = 30 * time.Second
	defaultJitter        = 0.2
	defaultMaxRestarts   = 5
	defaultWindow        = time.Minute
	defaultResetAfter    = 30 * time.Second
	defaultHealthIntvl   = time.Second
	defaultHealthTmo     = time.Second
	defaultFailures      = 3
	defaultStartGrace    = 10 * time.Second
	defaultMaxBytes      = 10 << 20
	defaultMaxFiles      = 3
	defaultStopSignal    = "SIGTERM"
	pollWhileStarting    = 100 * time.Millisecond
	stopPipeWaitDelay    = 2 * time.Second
	defaultRestartPolicy = policyOnFailure
)

var signalByName = map[string]syscall.Signal{
	"SIGHUP":  syscall.SIGHUP,
	"SIGINT":  syscall.SIGINT,
	"SIGQUIT": syscall.SIGQUIT,
	"SIGTERM": syscall.SIGTERM,
	"SIGKILL": syscall.SIGKILL,
}

// DecodeConfig parses a JSON configuration strictly (unknown fields are an
// error) and validates it.
func DecodeConfig(data []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if dec.More() {
		return Config{}, errors.New("parse config: trailing data after the JSON object")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate checks the configuration: names, commands, listener and lock
// uniqueness, and every value range.
func (c Config) Validate() error {
	names := make(map[string]bool, len(c.Services))
	listeners := make(map[string]string)
	locks := make(map[string]string)
	for _, s := range c.Services {
		if err := s.validate(); err != nil {
			return fmt.Errorf("service %q: %w", s.Name, err)
		}
		if names[s.Name] {
			return fmt.Errorf("service %q: duplicate name", s.Name)
		}
		names[s.Name] = true
		if s.Listener != "" {
			if other, ok := listeners[s.Listener]; ok {
				return fmt.Errorf("service %q: listener %q is already used by service %q", s.Name, s.Listener, other)
			}
			listeners[s.Listener] = s.Name
		}
		if s.ExclusiveLock != "" {
			if other, ok := locks[s.ExclusiveLock]; ok {
				return fmt.Errorf("service %q: exclusive_lock %q is already used by service %q", s.Name, s.ExclusiveLock, other)
			}
			locks[s.ExclusiveLock] = s.Name
		}
	}
	return nil
}

func (s ServiceSpec) validate() error {
	if s.Name == "" {
		return errors.New("name is required")
	}
	for _, r := range s.Name {
		ok := r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("name %q may only contain letters, digits, '-', '_' and '.'", s.Name)
		}
	}
	if s.Command == "" {
		return errors.New("command is required")
	}
	for k := range s.Env {
		if k == "" || strings.Contains(k, "=") {
			return fmt.Errorf("invalid env key %q", k)
		}
	}
	switch s.Restart.policy() {
	case policyNever, policyOnFailure, policyAlways:
	default:
		return fmt.Errorf("restart.policy %q: want never, on-failure or always", s.Restart.Policy)
	}
	if s.Restart.MaxRestarts < 0 {
		return errors.New("restart.max_restarts must not be negative")
	}
	if s.Restart.Window < 0 || s.Restart.ResetAfter < 0 {
		return errors.New("restart.window and restart.reset_after must not be negative")
	}
	if s.Restart.Backoff.Initial < 0 || s.Restart.Backoff.Max < 0 {
		return errors.New("restart.backoff values must not be negative")
	}
	if b := s.Restart.Backoff; b.Initial != 0 && b.Max != 0 && b.Initial > b.Max {
		return errors.New("restart.backoff.initial must not exceed restart.backoff.max")
	}
	if j := s.Restart.Backoff.jitter(); j < 0 || j > 1 {
		return fmt.Errorf("restart.backoff.jitter %v: want a value in [0, 1]", j)
	}
	if s.StopTimeout < 0 {
		return errors.New("stop_timeout must not be negative")
	}
	if _, err := s.stopSignal(); err != nil {
		return err
	}
	if s.Listener == "" && (s.StopMethod != 0 || s.Health.PingMethod != 0 || s.Health.hasSettings()) {
		return errors.New("stop_method, health settings and ping_method require a listener")
	}
	if s.Health.Interval < 0 || s.Health.Timeout < 0 || s.Health.StartGrace < 0 || s.Health.Failures < 0 {
		return errors.New("health values must not be negative")
	}
	switch s.Output.mode() {
	case modeInherit, modePrefix:
	case modeFile:
		if s.Output.Path == "" {
			return errors.New("output.path is required for mode \"file\"")
		}
	default:
		return fmt.Errorf("output.mode %q: want inherit, file or prefix", s.Output.Mode)
	}
	if s.Output.MaxBytes < 0 || s.Output.MaxFiles < 0 {
		return errors.New("output.max_bytes and output.max_files must not be negative")
	}
	return nil
}

func (h HealthSpec) hasSettings() bool {
	return h.Interval != 0 || h.Timeout != 0 || h.Failures != 0 || h.StartGrace != 0
}

func (r RestartSpec) policy() string {
	if r.Policy == "" {
		return defaultRestartPolicy
	}
	return r.Policy
}

func (r RestartSpec) maxRestarts() int {
	if r.MaxRestarts == 0 {
		return defaultMaxRestarts
	}
	return r.MaxRestarts
}

func (r RestartSpec) window() time.Duration {
	if r.Window == 0 {
		return defaultWindow
	}
	return time.Duration(r.Window)
}

func (r RestartSpec) resetAfter() time.Duration {
	if r.ResetAfter == 0 {
		return defaultResetAfter
	}
	return time.Duration(r.ResetAfter)
}

// initial is the first delay: the default, capped at max when only max is set.
func (b BackoffSpec) initial() time.Duration {
	if b.Initial == 0 {
		return min(defaultBackoffInit, b.max())
	}
	return time.Duration(b.Initial)
}

func (b BackoffSpec) max() time.Duration {
	if b.Max == 0 {
		return defaultBackoffMax
	}
	return time.Duration(b.Max)
}

func (b BackoffSpec) jitter() float64 {
	if b.Jitter == nil {
		return defaultJitter
	}
	return *b.Jitter
}

// delay returns the backoff before restart attempt n (0-based), before jitter
// is applied to the random factor r in [0, 1).
func (b BackoffSpec) delay(n int, r float64) time.Duration {
	d := b.initial()
	limit := b.max()
	for range n {
		if d >= limit/2 {
			d = limit
			break
		}
		d *= 2
	}
	if d > limit {
		d = limit
	}
	j := b.jitter()
	f := 1 + j*(2*r-1)
	out := time.Duration(float64(d) * f)
	if out < 0 {
		out = 0
	}
	return out
}

func (s ServiceSpec) autostart() bool {
	return s.Autostart == nil || *s.Autostart
}

func (s ServiceSpec) stopTimeout() time.Duration {
	if s.StopTimeout == 0 {
		return defaultStopTimeout
	}
	return time.Duration(s.StopTimeout)
}

func (s ServiceSpec) stopSignal() (syscall.Signal, error) {
	name := s.StopSignal
	if name == "" {
		name = defaultStopSignal
	}
	sig, ok := signalByName[name]
	if !ok {
		return 0, fmt.Errorf("stop_signal %q: want SIGTERM, SIGINT, SIGHUP, SIGQUIT or SIGKILL", s.StopSignal)
	}
	return sig, nil
}

func (h HealthSpec) interval() time.Duration {
	if h.Interval == 0 {
		return defaultHealthIntvl
	}
	return time.Duration(h.Interval)
}

func (h HealthSpec) timeout() time.Duration {
	if h.Timeout == 0 {
		return defaultHealthTmo
	}
	return time.Duration(h.Timeout)
}

func (h HealthSpec) failures() int {
	if h.Failures == 0 {
		return defaultFailures
	}
	return h.Failures
}

func (h HealthSpec) startGrace() time.Duration {
	if h.StartGrace == 0 {
		return defaultStartGrace
	}
	return time.Duration(h.StartGrace)
}

func (o OutputSpec) mode() string {
	if o.Mode == "" {
		return modeInherit
	}
	return o.Mode
}

func (o OutputSpec) maxBytes() int64 {
	if o.MaxBytes == 0 {
		return defaultMaxBytes
	}
	return o.MaxBytes
}

func (o OutputSpec) maxFiles() int {
	if o.MaxFiles == 0 {
		return defaultMaxFiles
	}
	return o.MaxFiles
}

// clone returns a copy of c that shares no slices or maps with it.
func (c Config) clone() Config {
	out := Config{Services: make([]ServiceSpec, len(c.Services))}
	for i, s := range c.Services {
		out.Services[i] = s.clone()
	}
	return out
}

func (s ServiceSpec) clone() ServiceSpec {
	s.Args = append([]string(nil), s.Args...)
	if s.Env != nil {
		env := make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			env[k] = v
		}
		s.Env = env
	}
	if s.Autostart != nil {
		v := *s.Autostart
		s.Autostart = &v
	}
	if s.Restart.Backoff.Jitter != nil {
		v := *s.Restart.Backoff.Jitter
		s.Restart.Backoff.Jitter = &v
	}
	return s
}
