# hqqd

`hqqd` supervises processes that serve HQQ listeners. It starts each service,
decides when it is ready with HQQ health checks, restarts it with backoff when
it exits or stops answering, stops it gracefully, and answers commands over an
HQQ admin listener. `hqqd ctl` is the command-line client of that listener.

```
hqqd [run] [-config hqqd.json]      run the supervisor
hqqd check [-config hqqd.json]      validate a configuration file
hqqd ctl [flags] <command> [args]   control a running hqqd
hqqd help
```

`hqqd` with no command runs the supervisor with `hqqd.json` in the current
directory.

## Configuration

The configuration is one JSON document. Unknown fields are errors, so a typo
never silently disables a setting. Durations are strings such as `"250ms"`,
`"10s"` or `"1m30s"`. Octal modes are strings such as `"0600"`.

A complete example, with every setting spelled out:

```json
{
  "admin": {
    "listener": "hqqd-admin",
    "perm": "0600"
  },
  "services": [
    {
      "name": "echo",
      "command": "/usr/local/bin/hqq-echo",
      "sha256": "0000000000000000000000000000000000000000000000000000000000000000",
      "args": ["--listener", "echo", "--verbose"],
      "env": {"HQQ_LOG": "info"},
      "dir": "/var/lib/echo",
      "listener": "echo",
      "autostart": true,
      "restart": {
        "policy": "on-failure",
        "backoff": {"initial": "250ms", "max": "10s", "jitter": 0.2},
        "max_restarts": 5,
        "window": "1m",
        "reset_after": "30s"
      },
      "stop_timeout": "5s",
      "stop_signal": "SIGTERM",
      "stop_method": 7,
      "health": {
        "interval": "1s",
        "timeout": "500ms",
        "failures": 3,
        "start_grace": "5s",
        "ping_method": 1
      },
      "exclusive_lock": "/var/lock/echo.lock",
      "output": {
        "mode": "file",
        "path": "/var/log/echo.log",
        "max_bytes": 10485760,
        "max_files": 3
      }
    },
    {
      "name": "worker",
      "command": "/usr/local/bin/hqq-worker",
      "parent_pipe": true,
      "manifest": {"file": "/var/lib/hqq/dist/manifest.json", "signature": "/var/lib/hqq/dist/manifest.json.sig", "public_key": "0000000000000000000000000000000000000000000000000000000000000000"},
      "restart": {"policy": "always"},
      "output": {"mode": "prefix"}
    }
  ]
}
```

The two 64-digit zero values are placeholders, and `hqqd check` accepts the example as written. Before use, replace the `sha256` of `echo` with the SHA-256 of its `command` (`hqq-manifest hash FILE` prints it), and the `public_key` of `worker` with the hex text of the release public key.

### Top level

| Key        | Meaning                                                              |
|------------|----------------------------------------------------------------------|
| `admin`    | The admin listener (see below). Optional.                            |
| `services` | The supervised services. Names must be unique.                       |

### `admin`

| Key        | Default   | Meaning                                                                 |
|------------|-----------|-------------------------------------------------------------------------|
| `listener` | `hqqd`    | HQQ listener name of the admin protocol. Must differ from every service listener. |
| `perm`     | `"0600"`  | Permission of the listener segment. Clients need read and write access. |

### `services[]`

| Key              | Default        | Meaning |
|------------------|----------------|---------|
| `name`           | required       | Letters, digits, `-`, `_` and `.`. Used in status, events and ctl. |
| `command`        | required       | Executable: a path, or a name looked up in `PATH`. Not run through a shell. |
| `args`           | none           | Arguments. |
| `env`            | none           | Added to hqqd's environment; a key already set is overridden. |
| `dir`            | hqqd's dir     | Working directory. |
| `listener`       | none           | HQQ listener name the service serves. With a listener, readiness is a successful health check; without one, a started process is ready. Unique across services. |
| `autostart`      | `true`         | Start with hqqd (or when added by a reload). |
| `restart`        | see below      | Restart policy. |
| `stop_timeout`   | `"10s"`        | Time a graceful stop may take before the process is killed. |
| `stop_signal`    | `"SIGTERM"`    | Graceful stop signal on unix: `SIGTERM`, `SIGINT`, `SIGHUP`, `SIGQUIT` or `SIGKILL`. |
| `stop_method`    | none           | HQQ method ID the service serves to shut itself down. Tried before the signal. Requires `listener`. `0` means none. |
| `health`         | see below      | Health checks. Requires `listener`. |
| `exclusive_lock` | none           | Path of an advisory lock file that hqqd holds while the service is managed. |
| `output`         | see below      | Where stdout and stderr go. |
| `sha256`         | none           | Expected SHA-256 digest of `command`: 64 hex digits. The command is verified and started through `secureexec`. Exclusive with `manifest`. See "Verified executables". |
| `manifest`       | none           | Signed release manifest that gives the digest of `command` for this platform: `file`, `signature` and `public_key`, all required. Exclusive with `sha256`. |
| `parent_pipe`    | `false`        | Ties the standard input of the service to hqqd. See "Parent pipe". |

### `restart`

| Key            | Default        | Meaning |
|----------------|----------------|---------|
| `policy`       | `"on-failure"` | `never`: no automatic restart at all, including health restarts. `on-failure`: restart after a non-zero exit, a signal, or a health restart. `always`: restart after any exit. |
| `backoff`      | see below      | Delay before a restart. |
| `max_restarts` | `5`            | Automatic restarts allowed within `window`. Reaching it moves the service to `failed`. |
| `window`       | `"1m"`         | Sliding window of `max_restarts`. |
| `reset_after`  | `"30s"`        | A run that lasted this long resets the backoff. |

`backoff`:

| Key       | Default | Meaning |
|-----------|---------|---------|
| `initial` | `"500ms"` | First delay. It doubles after every consecutive restart. |
| `max`     | `"30s"`   | Cap of the delay. |
| `jitter`  | `0.2`     | Random spread around the delay, a fraction in `[0, 1]`. `0` disables it. |

Explicit `start`, `restart` and `reload` operations do not count toward
`max_restarts`.

### `health`

| Key           | Default | Meaning |
|---------------|---------|---------|
| `interval`    | `"1s"`  | Time between checks. While a service starts, checks run every 100 ms at most. |
| `timeout`     | `"1s"`  | Bound of one check. |
| `failures`    | `3`     | Consecutive failed checks of a ready service before it is restarted. |
| `start_grace` | `"10s"` | Time a starting service has to become ready. Afterwards it is restarted (or stays `unhealthy` with `policy: never`). |
| `ping_method` | none    | HQQ method ID called after the connect of each check. `0` means connect only. |

A check connects to the listener with default HQQ settings and closes the link
again. The service therefore has to accept (`Acceptor.Accept`) and serve
(`Serve`) connections for its checks to pass.

### `output`

| Key         | Default   | Meaning |
|-------------|-----------|---------|
| `mode`      | `"inherit"` | `inherit`: hqqd's own stdout and stderr. `file`: append to `path`. `prefix`: hqqd's stdout and stderr, each line prefixed with `<name> | `. |
| `path`      | required for `file` | Log file. |
| `max_bytes` | `10485760` | Size at which the file is rotated. |
| `max_files` | `3`        | Rotated files kept: `path.1` … `path.N`. |

### Verified executables

With `sha256` or `manifest`, hqqd verifies the executable before every start
and runs the verified bytes, so a file replaced after the check does not run.
The check is repeated at each start: a restart verifies again, and a rotated
manifest takes effect at the next start. The protection is the one of
`gosuda.org/hqq/secureexec` for the platform, and its README has the threat
model. `verified` and `mechanism` in Status name it:

- Linux: `memfd`, a sealed in-kernel copy (`vault` when memfd is unavailable).
- macOS and other unix: `vault`, a private verified copy in a 0700 directory.
- Windows: `windows-share-lock`, the file held open without write or delete sharing.

A `manifest` names three files: `file` (`manifest.json`), `signature`
(`manifest.json.sig`) and `public_key` (the hex text of the release's ed25519
public key). The signature is checked before the manifest is parsed. The entry
for hqqd's `GOOS/GOARCH` must name the base name of `command`. Paths are
relative to hqqd's working directory.

A mismatch, a bad signature, a missing platform entry or a different artifact
name makes the start fail, and no process runs. A start that is requested
(`hqqd ctl start`, or autostart) reports the error and leaves the service
`failed`. A restart after an exit counts as a failed start: it is retried with
backoff until the restart limit is reached. `last_error` says why. `hqqd check`
validates the fields but not the files, which are read at each start.

The digests come from the release. `hqq-manifest hash FILE` prints
`SHA256  FILE`, as `sha256sum` does, and `hqq-manifest verify` checks a release
directory against its signed manifest (see `secureexec/README.md`).

### Parent pipe

With `parent_pipe: true`, hqqd gives the service's standard input the read end
of a pipe and keeps the write end while the service runs. When hqqd exits for
any reason, including SIGKILL, the kernel closes the write end and the service
reads end of file on its standard input. A Go service calls `lifeline.Watch`
(package `gosuda.org/hqq/lifeline`) early in `main` and exits when the returned
context ends. Any other service must read its standard input until end of file;
the C server takes `--parent-stdin` for this. A service that never reads its
standard input does not end with hqqd.

## States

| State       | Meaning |
|-------------|---------|
| `stopped`   | Not running, not wanted. |
| `starting`  | Process running, not yet ready. |
| `ready`     | Running; health checks pass (or there is no listener). |
| `unhealthy` | Running; health checks fail, but not enough to restart yet (or the policy forbids restarts). |
| `backoff`   | Exited; a restart is scheduled. `next_restart` tells when. |
| `stopping`  | A graceful stop or a restart is in progress. |
| `failed`    | Not running and not restarted: the process exited under a policy that does not restart it, the restart limit was reached, it could not be started, or its lock is held. `last_error` says why. |

Status (`hqqd ctl status <name>`, or `list`) reports the state, `pid`,
`started_at`, `uptime`, `restarts`, `next_restart`, `last_exit` (`code`,
`signal`, `at`), `health` (`ok`, `at`, `latency`, `error`) and `last_error`.

While a process runs, Status also reports `verified` (true when the process was started from a verified executable) and `mechanism` (the `secureexec` mechanism that protects it). Both are absent when no process runs or it was not verified.

## Stopping and signals

A stop, a restart or a health restart goes like this:

1. If `stop_method` is set, hqqd connects to the listener and calls it. The
   service answers and shuts itself down.
2. If that is not set, or the call fails, hqqd sends `stop_signal` (unix).
   On Windows there is no such signal: without `stop_method` the process is
   killed at once.
3. When the process has not exited after `stop_timeout`, it is killed (with
   its process group on unix).

On unix, services run in their own process group, so a Ctrl-C in hqqd's
terminal does not reach them directly; hqqd stops them itself.

When hqqd receives SIGINT or SIGTERM (Ctrl-C and Ctrl-Break on Windows), it
stops all services with the sequence above and exits. SIGHUP re-reads the
configuration file and applies it, like `hqqd ctl reload`.

A service whose HQQ acceptor is closing waits for the handshakes in progress
(`Acceptor.Close`), so a stop can take that long before the process exits. The
stop timeout bounds it.

## Exclusive locks

`exclusive_lock` names a file. hqqd takes an advisory lock on it when the
service is first started and keeps it until the service is stopped or failed,
across restarts. On unix the lock is `flock(2)`; on Windows it is `LockFileEx`.
While another process holds the lock, the start fails with `failed` and the
message `exclusive lock ... held by another process`. The lock is released when
the process of hqqd exits.

## Stale listener names

Before hqqd restarts a service that it ran earlier, it probes the service's
listener name with a short connect. If the probe times out (no acceptor
answers) or the listener is gone, hqqd removes the name with `hqq.Unlink`, so
the restarted service creates a fresh listener. A live acceptor is left alone. A
name that is not an HQQ listener of this layout is never touched. The first
start of a service relies on `hqq.NewAcceptor`, which reclaims a dead listener
name by itself.

## Admin protocol and `hqqd ctl`

The admin listener serves JSON requests. Each method is an HQQ method ID:

| ID | Method    | Request                         | Response |
|----|-----------|---------------------------------|----------|
| 1  | `list`    | none                            | `{"services": [Status…]}` |
| 2  | `status`  | `{"name": "<service>"}`         | `Status` |
| 3  | `start`   | `{"name": "<service>"}`         | `Status` |
| 4  | `stop`    | `{"name": "<service>"}`         | `Status` |
| 5  | `restart` | `{"name": "<service>"}`         | `Status` |
| 6  | `reload`  | a configuration, or none (re-read the file) | `{"services": [Status…]}` |

Errors are HQQ error responses whose payload is `{"error": "…"}`. Their status
is `0x101` (bad request), `0x102` (unknown service) or `0x103` (failed).

`hqqd ctl` prints the JSON response:

```
hqqd ctl list
hqqd ctl status echo
hqqd ctl start echo | stop echo | restart echo
hqqd ctl reload                  # hqqd re-reads its configuration file
hqqd ctl reload new-config.json  # sends this file's services to hqqd
```

Flags, before the command:

| Flag        | Default | Meaning |
|-------------|---------|---------|
| `-listener` | `hqqd`  | Admin listener name. |
| `-config`   | none    | Read the admin listener name from this configuration file. |
| `-timeout`  | `30s`   | Bound of the whole command. |

`reload` with a file checks it locally and sends only its `services`. Its
`admin` block is ignored by hqqd: changing the admin listener needs a restart
of hqqd.

## Running it

```
hqqd check -config /etc/hqqd/hqqd.json
hqqd run -config /etc/hqqd/hqqd.json
```

Events are written to stderr, one line per state change:

```
hqqd: echo: starting
hqqd: echo: ready
hqqd: echo: backoff: killed by SIGKILL; restarting in 260ms
```

## Platforms

hqqd builds on unix and on Windows. Differences on Windows: no graceful
signal (use `stop_method`), and no process group. The exclusive lock uses
`LockFileEx`.

## Embedding

The engine is the package `gosuda.org/hqq/supervisor`. `supervisor.New` creates
a `Supervisor` from a `Config`, `Run` starts the autostart services and blocks
until its context ends, `Reload` applies a new configuration, and `ServeAdmin`
and `DialAdmin` provide the admin protocol. See the package documentation.
