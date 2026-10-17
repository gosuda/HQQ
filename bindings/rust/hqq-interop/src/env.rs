//! State of one role in one process: configuration, checks, barriers, options,
//! error kinds and the self-injected crash (INTEROP_CLI.md, sections 3, 4, 6, 8).

use std::fs;
use std::thread;
use std::time::{Duration, Instant};

use hqq::{AdvOptions, Error as HqqError, Options, Wait, WaitPolicy};

use crate::cli::{Config, Role};
use crate::result::{Check, ResultDoc};

/// Timing slack added to every bound the scenarios assert (Go reference: stdSlack).
pub const STD_SLACK: Duration = Duration::from_millis(1500);

/// Why a role did not pass. `Skip` exits 4; `Error` exits 1 with its kind.
#[derive(Debug)]
pub enum Fail {
    Skip(String),
    Error {
        kind: &'static str,
        msg: String,
        status: Option<u64>,
    },
}

impl Fail {
    pub fn err(kind: &'static str, msg: impl Into<String>) -> Fail {
        Fail::Error {
            kind,
            msg: msg.into(),
            status: None,
        }
    }

    pub fn timeout(msg: impl Into<String>) -> Fail {
        Fail::err("ErrTimeout", msg)
    }
}

impl From<HqqError> for Fail {
    fn from(e: HqqError) -> Fail {
        Fail::err(kind_of(e), e.to_string())
    }
}

/// The canonical `error_kind` of a libhqq error (INTEROP_CLI.md, section 4.1). The
/// most specific name comes first; statuses that wrap another one map to the wrapper.
pub fn kind_of(e: HqqError) -> &'static str {
    use HqqError::*;
    match e {
        Closed => "ErrClosed",
        PeerClosed => "ErrPeerClosed",
        PeerGone => "ErrPeerGone",
        Timeout => "ErrTimeout",
        LinkBusy => "ErrLinkBusy",
        IncompatibleLayout => "ErrIncompatibleLayout",
        FailedInit | SegmentNotReady | InitContended | NoLiveEndpoint => "ErrFailedInit",
        InvalidSize | GeometryMismatch => "ErrInvalidSize",
        MemoryAlign => "ErrMemoryAlign",
        MemorySmall => "ErrMemorySmall",
        BufferOverflow | BufferTooSmall => "ErrBufferOverflow",
        IncomingOverflow => "ErrIncomingOverflow",
        ReassemblyLimit => "ErrReassemblyLimit",
        ConnNotFound => "ErrConnNotFound",
        InvalidOp => "ErrInvalidOp",
        NotNegotiated => "ErrNotNegotiated",
        AlreadyNegotiated => "ErrAlreadyNegotiated",
        NegotiationFailed => "ErrNegotiationFailed",
        NegotiationRejected => "ErrNegotiationRejected",
        UnexpectedPacket => "ErrUnexpectedPacket",
        Protocol => "ErrProtocol",
        MessageAborted => "ErrMessageAborted",
        ShuttingDown => "ErrShuttingDown",
        Response => "ResponseError",
        NotListening => "ErrNotListening",
        AlreadyListening => "ErrAlreadyListening",
        ListenerBusy => "ErrListenerBusy",
        ListenerGone => "ErrListenerGone",
        ListenerClosed => "ErrListenerClosed",
        AcceptorClosed => "ErrAcceptorClosed",
        InvalidName => "ErrInvalidName",
        NotExist => "ErrNotExist",
        _ => "Unknown",
    }
}

/// A wait that ends at the process deadline.
pub fn wait_until(deadline: Instant) -> Wait<'static> {
    Wait::timeout(deadline.saturating_duration_since(Instant::now()))
}

pub struct Env {
    pub cfg: Config,
    pub start: Instant,
    pub deadline: Instant,
    pub checks: Vec<Check>,
    pub failed: bool,
    pub sent: u64,
    pub received: u64,
    pub meta: Vec<(String, String)>,
}

impl Env {
    pub fn new(cfg: Config) -> Env {
        let start = Instant::now();
        let deadline = start + cfg.timeout;
        Env {
            cfg,
            start,
            deadline,
            checks: Vec::new(),
            failed: false,
            sent: 0,
            received: 0,
            meta: Vec::new(),
        }
    }

    pub fn creator(&self) -> bool {
        self.cfg.role == Role::Creator
    }

    pub fn size(&self) -> usize {
        self.cfg.size as usize
    }

    pub fn remaining(&self) -> Duration {
        self.deadline.saturating_duration_since(Instant::now())
    }

    /// A blocking-call bound: the rest of the process deadline.
    pub fn wait(&self) -> Wait<'static> {
        wait_until(self.deadline)
    }

    /// `MaxMessageSize` in force (the library default when not configured).
    pub fn max_message(&self) -> usize {
        if self.cfg.max_message != 0 {
            self.cfg.max_message as usize
        } else {
            64 << 20
        }
    }

    /// max(peer timeout, 4 x heartbeat interval) (SPEC 6.3; library defaults 5 s, 250 ms).
    pub fn stale_after(&self) -> Duration {
        let peer = if self.cfg.peer_timeout_ms > 0 {
            Duration::from_millis(self.cfg.peer_timeout_ms as u64)
        } else {
            Duration::from_secs(5)
        };
        let interval = if self.cfg.heartbeat_ms > 0 {
            Duration::from_millis(self.cfg.heartbeat_ms as u64)
        } else {
            Duration::from_millis(250)
        };
        peer.max(4 * interval)
    }

    /// The bound of ErrPeerGone detection: stale window plus slack.
    pub fn peer_bound(&self) -> Duration {
        self.stale_after() + STD_SLACK
    }

    /// Standard link options of the configuration (INTEROP_CLI.md, section 2).
    pub fn std_options(&self) -> Options {
        let c = &self.cfg;
        let mut o = Options::new()
            .wait_policy(if c.spin {
                WaitPolicy::Spin
            } else {
                WaitPolicy::Adaptive
            })
            .attach_timeout(c.attach_timeout);
        o = if c.heartbeat_ms < 0 {
            o.disable_heartbeat()
        } else {
            o.heartbeat_interval(Duration::from_millis(c.heartbeat_ms as u64))
        };
        if c.peer_timeout_ms > 0 {
            o = o.peer_timeout(Duration::from_millis(c.peer_timeout_ms as u64));
        }
        o
    }

    /// Advanced link options of the configuration.
    pub fn adv_options(&self) -> AdvOptions {
        AdvOptions::new()
            .link(self.std_options())
            .max_message_size(self.cfg.max_message)
            .max_reassembly_bytes(self.cfg.max_reassembly)
            .queue_size(self.cfg.queue_size)
    }

    /// Records an assertion. A failed check makes the role fail.
    pub fn check(&mut self, name: &str, ok: bool, detail: &str) {
        self.checks.push(Check {
            name: name.to_string(),
            ok,
            detail: detail.to_string(),
        });
        if !ok {
            self.failed = true;
        }
    }

    /// Asserts the error is `want` (or wraps it, as Go's `errors.Is`); `None` wants success.
    pub fn check_err(&mut self, name: &str, got: Option<HqqError>, want: Option<HqqError>) {
        let ok = match (got, want) {
            (None, None) => true,
            (Some(g), Some(w)) => g.matches(w),
            _ => false,
        };
        let detail = got.map(|e| e.to_string()).unwrap_or_default();
        self.check(name, ok, &detail);
    }

    pub fn meta_num(&mut self, key: &str, value: impl std::fmt::Display) {
        self.meta.push((key.to_string(), value.to_string()));
    }

    pub fn meta_bool(&mut self, key: &str, value: bool) {
        self.meta.push((key.to_string(), value.to_string()));
    }

    /// Sleeps `d`, or fails with ErrTimeout when the process deadline comes first.
    pub fn sleep(&self, d: Duration) -> Result<(), Fail> {
        let rem = self.remaining();
        if d >= rem {
            thread::sleep(rem);
            return Err(Fail::timeout("sleep ran into the process deadline"));
        }
        thread::sleep(d);
        Ok(())
    }

    /// Synchronizes with the peer process of the same step (INTEROP_CLI.md, section 6).
    pub fn barrier(&self, phase: &str) -> Result<(), Fail> {
        let base = format!("s{}-{}", self.cfg.step, phase);
        let dir = &self.cfg.sync_dir;
        let mine = dir.join(format!("{base}.{}", self.cfg.role.name()));
        fs::File::create(&mine)
            .map_err(|e| Fail::err("Unknown", format!("barrier {phase}: {e}")))?;
        let peer = dir.join(format!("{base}.{}", self.cfg.role.peer().name()));
        loop {
            if peer.exists() {
                return Ok(());
            }
            if Instant::now() >= self.deadline {
                return Err(Fail::timeout(format!("barrier {phase}: waiting for peer")));
            }
            thread::sleep(Duration::from_millis(2));
        }
    }

    /// Kills this process with SIGKILL when `point` is the crash point of the run.
    /// The result line is printed first, with `crashed=true` (INTEROP_CLI.md, section 8).
    pub fn maybe_crash(&mut self, point: &str) {
        if self.cfg.crash_after != point {
            return;
        }
        let doc = self.doc(!self.failed, false, true, None);
        crate::result::emit(&doc);
        kill_self();
    }

    /// The result line of this role.
    pub fn doc(&self, ok: bool, skipped: bool, crashed: bool, fail: Option<&Fail>) -> ResultDoc {
        let mut d = ResultDoc::new(&self.cfg);
        d.ok = ok;
        d.skipped = skipped;
        d.crashed = crashed;
        d.checks = self.checks.clone();
        d.sent = self.sent;
        d.received = self.received;
        d.elapsed_ms = self.start.elapsed().as_millis();
        d.meta = self.meta.clone();
        match fail {
            Some(Fail::Skip(msg)) => d.error = msg.clone(),
            Some(Fail::Error { kind, msg, status }) => {
                d.error_kind = kind.to_string();
                d.error = msg.clone();
                if let Some(s) = status {
                    d.meta.push(("status".to_string(), s.to_string()));
                }
            }
            None => {}
        }
        d
    }
}

/// Delivers SIGKILL to this process; it does not return. On Unix this is the
/// kill(2) system call, declared directly: std links the C library already, and
/// no crate is needed for one call. On Windows it is TerminateProcess on the
/// current process (a child `taskkill` would print its own line on stdout and
/// hide the result line the driver looks for).
pub fn kill_self() -> ! {
    #[cfg(unix)]
    {
        extern "C" {
            fn kill(pid: i32, sig: i32) -> i32;
        }
        const SIGKILL: i32 = 9;
        // SAFETY: kill(2) with our own pid and a constant signal; no memory is involved.
        unsafe {
            kill(std::process::id() as i32, SIGKILL);
        }
    }
    #[cfg(windows)]
    {
        use std::ffi::c_void;
        extern "system" {
            fn GetCurrentProcess() -> *mut c_void;
            fn TerminateProcess(process: *mut c_void, exit_code: u32) -> i32;
        }
        // SAFETY: both calls take only the pseudo handle of this process and a
        // constant exit code; no memory is involved.
        unsafe {
            TerminateProcess(GetCurrentProcess(), 1);
        }
    }
    loop {
        thread::park();
    }
}
