// Package supervisor runs and watches processes that serve HQQ listeners. It
// is the engine behind cmd/hqqd, and it can be embedded in other programs.
//
// A Supervisor manages a set of services, each one a command with optional
// arguments, environment, working directory and output handling. For every
// service it:
//
//   - starts the process, and tracks it through the states of State;
//   - decides readiness with health checks: a connect to the service's HQQ
//     listener, and optionally a call to a ping method (Health);
//   - restarts the process when it exits or when its health checks fail,
//     according to the restart policy: exponential backoff with jitter, and a
//     limit on the number of automatic restarts within a sliding window, after
//     which the service is failed (RestartSpec);
//   - stops it gracefully: the stop method the service serves over HQQ, or
//     the stop signal on unix, then a kill after the stop timeout (ServiceSpec);
//   - holds an advisory lock file for the lifetime of the service, so that a
//     single supervisor runs it (ServiceSpec.ExclusiveLock);
//   - reports every state change as an Event, and answers snapshots as Status.
//
// Configuration is plain data: Config, ServiceSpec and the types it embeds,
// with durations written as strings such as "250ms". DecodeConfig parses and
// validates a JSON document strictly; Config.Validate checks a value built in
// code. Supervisor.Reload applies a new configuration to a running engine:
// services that changed are restarted, new ones are added and removed ones
// are stopped.
//
// The engine runs one goroutine per service. Every operation that changes a
// service goes through that goroutine, so the state machine has a single
// writer; Status and List read a snapshot that is published after each change.
//
// ServeAdmin exposes a Supervisor over an HQQ listener with a small JSON
// protocol (list, status, start, stop, restart, reload), and DialAdmin is the
// matching client. cmd/hqqd combines both into a daemon with a command-line
// control tool.
//
// The package is supported on unix and on windows. On windows there is no
// graceful signal: a service stops through its StopMethod, or is killed.
package supervisor
