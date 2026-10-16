//! Advanced-link scenarios (SPEC 10, PROTOCOL_1_1 draft): X-A01 (negotiation), X-A02
//! (sizes), X-A04 (request/response), X-A05 (basic streams), X-A08 (request metadata),
//! X-A09 (graceful shutdown). Follows the Go reference (cmd/hqq-interop/adv_*.go).

use std::sync::{mpsc, Arc, Condvar, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use hqq::{
    Acceptor, AcceptorOptions, AdvLink, CallOptions, CancelToken, ConnectOptions,
    Error as HqqError, ProtocolState, Request, Responder, Stream, Wait, FEATURES_ALL,
    FEATURE_CANCEL, FEATURE_MESSAGES, FEATURE_REQUEST_RESPONSE,
};

use crate::env::{wait_until, Env, Fail};
use crate::payload::{intact, payload};

type Res = Result<(), Fail>;

/// Protocol 1.1 as a version word (`(major << 8) | minor`).
const VERSION_1_1: u16 = 0x0101;
const ALL: u64 = FEATURES_ALL;
const METHOD_SLEEP_ECHO: u64 = 1;
const METHOD_FAIL: u64 = 2;
const METHOD_SLOW: u64 = 3;
const METHOD_ECHO: u64 = 4;
const METHOD_BLOCK: u64 = 7;
const METHOD_META: u64 = 8;
/// ErrCodeInvalidOp: the status of an unknown method.
const STATUS_INVALID_OP: u64 = 6;
/// ErrCodeShuttingDown: the status of a request refused during shutdown.
const STATUS_SHUTTING_DOWN: u64 = 8;
const RPC_CONCURRENCY: usize = 16;

fn joined<T>(r: thread::Result<T>) -> T {
    r.unwrap_or_else(|panic| std::panic::resume_unwind(panic))
}

/// The creator (Primary) creates the named link and both roles meet at "linked".
fn establish(env: &mut Env) -> Result<AdvLink, Fail> {
    let c = env.cfg.clone();
    if env.creator() {
        let link = AdvLink::create(
            &c.name,
            c.buffer_count,
            c.buffer_size,
            0o600,
            &env.adv_options(),
            None,
        )?;
        env.barrier("linked")?;
        Ok(link)
    } else {
        env.barrier("linked")?;
        Ok(AdvLink::open(
            &c.name,
            c.buffer_count,
            c.buffer_size,
            &env.adv_options(),
            None,
        )?)
    }
}

/// A one-shot signal with a timed wait.
#[derive(Default)]
struct Latch {
    open: Mutex<bool>,
    cv: Condvar,
}

impl Latch {
    fn open(&self) {
        *self.open.lock().unwrap_or_else(|p| p.into_inner()) = true;
        self.cv.notify_all();
    }

    /// Waits up to `d`; true once the latch is open.
    fn wait(&self, d: Duration) -> bool {
        let guard = self.open.lock().unwrap_or_else(|p| p.into_inner());
        let (guard, _) = self
            .cv
            .wait_timeout_while(guard, d, |open| !*open)
            .unwrap_or_else(|p| p.into_inner());
        *guard
    }
}

/// Signals the serve token when dropped, so the serve thread ends before its scope joins.
struct StopOnDrop<'a>(&'a CancelToken);

impl Drop for StopOnDrop<'_> {
    fn drop(&mut self) {
        self.0.signal();
    }
}

/// Runs `link.serve(handler)` on a scoped thread while `body` runs. The serve ends
/// when `body` returns. Returns the body's value and the serve's result.
fn serving<H, R>(link: &AdvLink, handler: H, body: impl FnOnce() -> R) -> (R, hqq::Result<()>)
where
    H: Fn(&Request<'_>, &mut Responder<'_>) + Sync,
{
    // No process deadline on the serve: a request's deadline must come from the protocol only.
    let token = CancelToken::new();
    thread::scope(|s| {
        let server = s.spawn(|| link.serve(&handler, Wait::forever().cancel_with(&token)));
        let stop = StopOnDrop(&token);
        let out = body();
        drop(stop);
        (out, joined(server.join()))
    })
}

/// Echoes every request payload.
fn echo_handler(req: &Request<'_>, resp: &mut Responder<'_>) {
    let _ = resp.write(req.data());
}

/// The mux of X-A04: sleep-echo, error response, slow answer, echo; default error for unknown methods.
fn rpc_handler(req: &Request<'_>, resp: &mut Responder<'_>) {
    match req.method() {
        METHOD_SLEEP_ECHO => {
            // Inverse sleep: later ids finish first.
            let id = be_u32(req.data());
            let ms = (RPC_CONCURRENCY as u64).saturating_sub(u64::from(id)) * 5;
            let _ = req.wait_done(Duration::from_millis(ms));
            let _ = resp.write(req.data());
        }
        METHOD_FAIL => {
            let _ = resp.error(42, b"boom payload");
        }
        METHOD_SLOW => {
            let _ = req.wait_done(Duration::from_millis(300));
            let _ = resp.write(b"late");
        }
        METHOD_ECHO => {
            let _ = resp.write(req.data());
        }
        _ => {
            let _ = resp.error(STATUS_INVALID_OP, &[]);
        }
    }
}

fn be_u32(data: &[u8]) -> u32 {
    match data {
        [a, b, c, d, ..] => u32::from_be_bytes([*a, *b, *c, *d]),
        _ => 0,
    }
}

/// The outcome of a failed call: the error, its status and payload when the peer answered.
struct CallFail {
    err: HqqError,
    status: Option<u64>,
    payload: Vec<u8>,
}

impl From<CallFail> for Fail {
    fn from(c: CallFail) -> Fail {
        match c.err {
            HqqError::Response => Fail::Error {
                kind: "ResponseError",
                msg: format!("error response, status {}", c.status.unwrap_or(0)),
                status: c.status,
            },
            e => Fail::from(e),
        }
    }
}

/// A call on a stream, with the response copied out.
fn call(
    s: &Stream<'_>,
    method: u64,
    req: &[u8],
    opts: Option<&CallOptions>,
    wait: Wait<'_>,
) -> Result<Vec<u8>, CallFail> {
    let r = match opts {
        Some(o) => s.call_with(method, req, o, wait),
        None => s.call(method, req, wait),
    };
    match r {
        Ok(m) => Ok(m.data().to_vec()),
        Err(e) => {
            let (status, payload) = match &e.response {
                Some(m) => (Some(m.status()), m.data().to_vec()),
                None => (None, Vec::new()),
            };
            Err(CallFail {
                err: e.error,
                status,
                payload,
            })
        }
    }
}

/// The feature set of the variant, and the negotiation it performs.
fn negotiate_variant(env: &Env, l: &AdvLink) -> Result<u64, Fail> {
    let wait = env.wait();
    let creator = env.creator();
    let version = VERSION_1_1;
    match env.cfg.variant.as_str() {
        "all-features" => {
            l.negotiate(0, wait)?;
            Ok(ALL)
        }
        "opener-initiates" => {
            if creator {
                l.negotiate(ALL, wait)?;
            } else {
                l.negotiate_protocol(version, ALL, wait)?;
            }
            Ok(ALL)
        }
        "simultaneous" => {
            l.negotiate_protocol(version, ALL, wait)?;
            Ok(ALL)
        }
        "messages-only" => {
            l.negotiate(FEATURE_MESSAGES, wait)?;
            Ok(FEATURE_MESSAGES)
        }
        "request-response-only" => {
            l.negotiate(FEATURE_REQUEST_RESPONSE, wait)?;
            Ok(FEATURE_REQUEST_RESPONSE)
        }
        "none" => {
            // Zero means "every feature" to negotiate(); the creator asks for the
            // empty set explicitly, and the opener's answer is the intersection.
            if creator {
                l.negotiate_protocol(version, 0, wait)?;
            } else {
                l.negotiate(0, wait)?;
            }
            Ok(0)
        }
        other => Err(Fail::err("Unknown", format!("unknown variant {other:?}"))),
    }
}

/// X-A01: negotiation. Checks the negotiated version and features, then what the
/// negotiated features allow (Send needs messages, Call needs request/response).
pub fn negotiation(env: &mut Env) -> Res {
    let l = establish(env)?;
    let want = negotiate_variant(env, &l)?;
    let version = l.negotiated_version();
    env.check(
        "negotiated version is 1.1",
        version == VERSION_1_1,
        &format!("version {version:#06x}"),
    );
    let features = l.negotiated_features();
    env.check(
        "negotiated features match the request",
        features == want,
        &format!("got {features:#x} want {want:#x}"),
    );
    let state = l.protocol_state();
    env.check(
        "protocol state is Negotiated",
        state == ProtocolState::Negotiated,
        &format!("{state:?}"),
    );

    if !env.creator() && want & FEATURE_REQUEST_RESPONSE != 0 {
        // The opener answers calls while it waits for the creator's "end".
        let (res, _) = serving(&l, echo_handler, || negotiation_exchange(env, &l, want));
        return res;
    }
    negotiation_exchange(env, &l, want)
}

/// The data exchange that the negotiated features allow, then the "end" barrier.
fn negotiation_exchange(env: &mut Env, l: &AdvLink, want: u64) -> Res {
    let s0 = l.stream0();
    let size = env.size();
    let seed = env.cfg.seed;
    if env.creator() {
        if want & FEATURE_MESSAGES != 0 {
            let r = s0.send(&payload(seed, 0, size), env.wait());
            env.check_err("send with FeatureMessages", r.err(), None);
        } else {
            let r = s0.send(b"x", env.wait());
            env.check_err(
                "send without FeatureMessages is ErrNotNegotiated",
                r.err(),
                Some(HqqError::NotNegotiated),
            );
        }
        if want & FEATURE_REQUEST_RESPONSE != 0 {
            match call(&s0, METHOD_ECHO, b"ping", None, env.wait()) {
                Ok(d) => {
                    let ok = d == b"ping";
                    env.check(
                        "call with FeatureRequestResponse is echoed",
                        ok,
                        &String::from_utf8_lossy(&d),
                    );
                }
                Err(e) => env.check(
                    "call with FeatureRequestResponse is echoed",
                    false,
                    &e.err.to_string(),
                ),
            }
        } else {
            let r = call(&s0, METHOD_ECHO, b"ping", None, env.wait());
            env.check_err(
                "call without FeatureRequestResponse is ErrNotNegotiated",
                r.err().map(|e| e.err),
                Some(HqqError::NotNegotiated),
            );
        }
    } else if want & FEATURE_MESSAGES != 0 {
        let m = s0.recv(env.wait())?;
        env.check(
            &format!("message 0 has {size} bytes and is intact"),
            intact(seed, 0, size, m.data()),
            &format!("len={}", m.data().len()),
        );
    }
    env.barrier("end")
}

/// The MaxMessageSize (via the variant's max_message) of the sizes scenario.
fn sizes_for(env: &Env) -> Vec<usize> {
    let bs = env.cfg.buffer_size as usize;
    vec![0, 1, bs, bs + 1, 3 * bs + 7, env.max_message()]
}

/// X-A02: message sizes 0 .. MaxMessageSize arrive intact and in order; MaxMessageSize+1
/// is refused locally with ErrBufferOverflow and nothing is sent (a 1-byte sentinel arrives next).
pub fn sizes(env: &mut Env) -> Res {
    let l = establish(env)?;
    l.negotiate(0, env.wait())?;
    let max_size = env.max_message();
    let sizes = sizes_for(env);
    let rounds = if env.cfg.long { 20 } else { 1 };
    let seed = env.cfg.seed;
    let s0 = l.stream0();
    if env.creator() {
        let mut seq = 0u64;
        for _ in 0..rounds {
            for &n in &sizes {
                s0.send(&payload(seed, seq, n), env.wait())?;
                seq += 1;
            }
        }
        env.sent = seq;
        let r = s0.send(&vec![0u8; max_size + 1], env.wait());
        env.check_err(
            "MaxMessageSize+1 is refused locally",
            r.err(),
            Some(HqqError::BufferOverflow),
        );
        // Nothing of the refused message was sent: the sentinel takes its sequence number.
        s0.send(&payload(seed, seq, 1), env.wait())?;
        env.sent = seq + 1;
    } else {
        let mut seq = 0u64;
        for _ in 0..rounds {
            for &n in &sizes {
                let m = s0.recv(env.wait())?;
                env.check(
                    &format!("message {seq} has {n} bytes and is intact"),
                    intact(seed, seq, n, m.data()),
                    &format!("len={}", m.data().len()),
                );
                seq += 1;
            }
        }
        let m = s0.recv(env.wait())?;
        env.check(
            &format!("message {seq} has 1 bytes and is intact"),
            intact(seed, seq, 1, m.data()),
            &format!("len={}", m.data().len()),
        );
        env.received = seq + 1;
    }
    env.barrier("end")
}

/// X-A04: request/response. Sixteen concurrent calls answered out of order, an
/// error response with a payload, a call cancelled after 50 ms whose late response is
/// discarded, and an unknown method answered with status 6.
pub fn rpc(env: &mut Env) -> Res {
    let l = establish(env)?;
    l.negotiate(FEATURE_MESSAGES | FEATURE_REQUEST_RESPONSE, env.wait())?;
    if !env.creator() {
        let (res, _) = serving(&l, rpc_handler, || env.barrier("end"));
        return res;
    }

    let s0 = l.stream0();
    let deadline = env.deadline;
    let (order, first_err) = thread::scope(|t| {
        let (tx, rx) = mpsc::channel::<(usize, Result<(), String>)>();
        for i in 0..RPC_CONCURRENCY {
            let tx = tx.clone();
            t.spawn(move || {
                let p = (i as u32).to_be_bytes();
                let res = match call(&s0, METHOD_SLEEP_ECHO, &p, None, wait_until(deadline)) {
                    Ok(d) if d == p => Ok(()),
                    Ok(_) => Err(format!("call {i}: echo mismatch")),
                    Err(e) => Err(format!("call {i}: {}", e.err)),
                };
                let _ = tx.send((i, res));
            });
        }
        drop(tx);
        let mut order = Vec::with_capacity(RPC_CONCURRENCY);
        let mut first_err: Option<String> = None;
        for (i, res) in rx {
            order.push(i);
            if let Err(what) = res {
                first_err.get_or_insert(what);
            }
        }
        (order, first_err)
    });
    env.check(
        "all concurrent calls are answered with their own payload",
        first_err.is_none(),
        first_err.as_deref().unwrap_or(""),
    );
    let sorted = order.windows(2).all(|w| w[0] <= w[1]);
    env.check(
        "responses complete out of order",
        !sorted,
        &format!("{order:?}"),
    );
    env.sent = RPC_CONCURRENCY as u64;

    // Error response with a payload.
    let r = call(&s0, METHOD_FAIL, &[], None, env.wait());
    let ok = matches!(&r, Err(e) if e.err == HqqError::Response
        && e.status == Some(42) && e.payload == b"boom payload");
    env.check(
        "error response carries status 42 and its payload",
        ok,
        &r.err().map(|e| e.err.to_string()).unwrap_or_default(),
    );

    // Cancelled call: its late response is discarded.
    let r = call(
        &s0,
        METHOD_SLOW,
        b"slow",
        None,
        Wait::timeout(Duration::from_millis(50)),
    );
    env.check_err(
        "cancelled call returns its context error",
        r.err().map(|e| e.err),
        Some(HqqError::Timeout),
    );
    // Receiving routes the late response; pump calls until it is discarded.
    let until = Instant::now() + Duration::from_secs(3);
    while l.stats().abandoned_responses == 0 && Instant::now() < until {
        call(&s0, METHOD_ECHO, b"pump", None, env.wait()).map_err(Fail::from)?;
    }
    let abandoned = l.stats().abandoned_responses;
    env.check(
        "late response of the cancelled call is discarded (AbandonedResponses)",
        abandoned >= 1,
        &format!("abandoned {abandoned}"),
    );

    // Unknown method: the default error response.
    let r = call(&s0, 99, &[], None, env.wait());
    let ok = matches!(&r, Err(e) if e.status == Some(STATUS_INVALID_OP));
    env.check(
        "unknown method is answered with status 6 (ErrCodeInvalidOp)",
        ok,
        &r.err().map(|e| e.err.to_string()).unwrap_or_default(),
    );
    env.barrier("end")
}

/// X-A05 basic: streams. OpenStream is refused while the peer does not listen; the
/// ids are 2 then 4 (the Primary's parity, never reused); data flows both ways; the
/// peer sees ErrPeerClosed after the queued data; stream 0 cannot be closed.
pub fn streams_basic(env: &mut Env) -> Res {
    let l = establish(env)?;
    l.negotiate(0, env.wait())?;
    let seed = env.cfg.seed;
    let size = env.size();

    if !env.creator() {
        let r = l.accept_stream(Wait::timeout(Duration::from_secs(1)));
        env.check_err(
            "AcceptStream without ListenStreams is ErrNotListening",
            r.err(),
            Some(HqqError::NotListening),
        );
        env.barrier("p1")?;
        env.barrier("p2")?;
        l.listen_streams()?;
        env.barrier("p3")?;

        let st = l.accept_stream(env.wait())?;
        env.check(
            "first accepted stream has id 4 (the refused open consumed 2)",
            st.id() == 4,
            &st.id().to_string(),
        );
        for i in 0..2 {
            let m = st.recv(env.wait())?;
            env.check(
                &format!("stream message {i} has {size} bytes and is intact"),
                intact(seed, i, size, m.data()),
                &format!("len={}", m.data().len()),
            );
        }
        st.send(&payload(seed + 1, 0, 10), env.wait())?;
        let r = st.recv(env.wait());
        env.check_err(
            "peer close is seen after the queued data as ErrPeerClosed",
            r.err(),
            Some(HqqError::PeerClosed),
        );

        let st2 = l.accept_stream(env.wait())?;
        env.check(
            "second accepted stream has id 6 (ids are not reused)",
            st2.id() == 6,
            &st2.id().to_string(),
        );
        let m = st2.recv(env.wait())?;
        env.check(
            &format!("stream message 2 has {size} bytes and is intact"),
            intact(seed, 2, size, m.data()),
            &format!("len={}", m.data().len()),
        );
        env.received = 3;
        return env.barrier("end");
    }

    // Creator: the Primary allocates even ids.
    env.barrier("p1")?;
    let r = l.open_stream(env.wait());
    env.check_err(
        "OpenStream refused while the peer does not listen is ErrConnNotFound",
        r.err(),
        Some(HqqError::ConnNotFound),
    );
    env.barrier("p2")?;
    env.barrier("p3")?;

    let st = l.open_stream(env.wait())?;
    env.check(
        "first stream has id 4 (the refused open consumed 2)",
        st.id() == 4,
        &st.id().to_string(),
    );
    for i in 0..2 {
        st.send(&payload(seed, i, size), env.wait())?;
        env.sent += 1;
    }
    let reply = st.recv(env.wait())?;
    env.check(
        "reply from the peer's stream is intact",
        intact(seed + 1, 0, 10, reply.data()),
        "",
    );
    drop(reply);
    env.check_err("stream Close", st.close().err(), None);
    env.check_err(
        "second Close is ErrClosed",
        st.close().err(),
        Some(HqqError::Closed),
    );
    let r = st.recv(env.wait());
    env.check_err(
        "Recv on a closed stream is ErrClosed",
        r.err(),
        Some(HqqError::Closed),
    );
    env.check_err(
        "stream 0 cannot be closed",
        l.stream0().close().err(),
        Some(HqqError::InvalidOp),
    );

    let st2 = l.open_stream(env.wait())?;
    env.check(
        "second stream has id 6 (ids are not reused)",
        st2.id() == 6,
        &st2.id().to_string(),
    );
    st2.send(&payload(seed, 2, size), env.wait())?;
    env.check_err("stream 4 Close", st2.close().err(), None);
    env.sent = 3;
    env.barrier("end")
}

/// What the responder's handler of X-A08 saw of one request.
#[derive(Clone, Copy, Debug)]
struct Seen {
    caller: u64,
    trace: [u8; 16],
    has_deadline: bool,
    /// The request was still live after one second (remaining budget > 1 s). The
    /// upper bound of the budget is not probed: the client's own deadline is 2 s, and
    /// no public clock compares `deadline_ns` with the time.
    alive_after_1s: bool,
}

fn trace_id() -> [u8; 16] {
    let mut t = [0u8; 16];
    for (i, b) in t.iter_mut().enumerate() {
        *b = (i + 1) as u8;
    }
    t
}

/// X-A08: request metadata. With FeatureMetadata the caller, the trace and the
/// time budget reach the handler; an expired call sends nothing. Without it the options
/// are ignored and no deadline reaches the handler.
pub fn metadata(env: &mut Env) -> Res {
    let with_meta = env.cfg.variant == "metadata";
    let l = establish(env)?;
    let features = if with_meta {
        ALL
    } else {
        FEATURE_MESSAGES | FEATURE_REQUEST_RESPONSE | FEATURE_CANCEL
    };
    l.negotiate(features, env.wait())?;

    if !env.creator() {
        let seen: Mutex<Vec<Seen>> = Mutex::new(Vec::new());
        let handler = |req: &Request<'_>, resp: &mut Responder<'_>| {
            let mut rec = Seen {
                caller: req.caller(),
                trace: req.trace().unwrap_or([0u8; 16]),
                has_deadline: req.deadline_ns().is_some(),
                alive_after_1s: false,
            };
            if req.data() == b"b" {
                rec.alive_after_1s = matches!(
                    req.wait_done(Duration::from_secs(1)),
                    Err(HqqError::Timeout)
                );
            }
            seen.lock().unwrap_or_else(|p| p.into_inner()).push(rec);
            let _ = resp.write(req.data());
        };
        let (res, _) = serving(&l, handler, || env.barrier("end"));
        res?;
        let got = seen.into_inner().unwrap_or_else(|p| p.into_inner());
        env.meta_num("requests_seen", got.len());
        if with_meta {
            env.check(
                "responder saw 2 requests (the expired call was not sent)",
                got.len() == 2,
                &got.len().to_string(),
            );
            if let [first, second, ..] = got.as_slice() {
                env.check(
                    "caller ID and trace ID arrive",
                    first.caller == 42 && first.trace == trace_id(),
                    &first.caller.to_string(),
                );
                env.check(
                    "request without options has caller 0",
                    second.caller == 0,
                    &second.caller.to_string(),
                );
                env.check(
                    "deadline reaches the handler with a budget beyond 1 s",
                    second.has_deadline && second.alive_after_1s,
                    &format!(
                        "has={} alive>1s={}",
                        second.has_deadline, second.alive_after_1s
                    ),
                );
            }
        } else {
            env.check(
                "responder saw 2 requests",
                got.len() == 2,
                &got.len().to_string(),
            );
            if let [first, second, ..] = got.as_slice() {
                env.check(
                    "options are ignored without FeatureMetadata: caller 0, trace zero",
                    first.caller == 0 && first.trace == [0u8; 16],
                    &first.caller.to_string(),
                );
                env.check(
                    "no deadline reaches the handler without FeatureMetadata",
                    !second.has_deadline,
                    &second.has_deadline.to_string(),
                );
            }
        }
        return Ok(());
    }

    let s0 = l.stream0();
    let opts = CallOptions::new().caller(42).trace(trace_id());
    call(&s0, METHOD_META, b"a", Some(&opts), env.wait()).map_err(Fail::from)?;
    call(
        &s0,
        METHOD_META,
        b"b",
        None,
        Wait::timeout(Duration::from_secs(2)),
    )
    .map_err(Fail::from)?;
    if with_meta {
        let before = l.stats().requests;
        let r = call(&s0, METHOD_META, b"c", None, Wait::timeout(Duration::ZERO));
        let after = l.stats().requests;
        env.check_err(
            "call with an expired deadline fails locally with DeadlineExceeded",
            r.err().map(|e| e.err),
            Some(HqqError::Timeout),
        );
        env.check(
            "the expired call sent no request",
            after == before,
            &format!("requests {before} -> {after}"),
        );
    }
    env.barrier("end")
}

/// X-A09: graceful shutdown. The responder runs request A until it is released;
/// Shutdown must not return while A is in flight. The requester's calls are refused
/// with status 8 once shutdown started, and A completes normally.
pub fn shutdown(env: &mut Env) -> Res {
    let l = establish(env)?;
    l.negotiate(0, env.wait())?;
    if env.creator() {
        shutdown_responder(env, &l)
    } else {
        shutdown_requester(env, &l)
    }
}

fn shutdown_responder(env: &mut Env, l: &AdvLink) -> Res {
    let started = Latch::default();
    let release = Latch::default();
    let handler = |req: &Request<'_>, resp: &mut Responder<'_>| match req.method() {
        METHOD_BLOCK => {
            started.open();
            release.wait(Duration::from_secs(10));
            let _ = resp.write(b"A-done");
        }
        METHOD_ECHO => {
            let _ = resp.write(req.data());
        }
        _ => {
            let _ = resp.error(STATUS_INVALID_OP, &[]);
        }
    };
    let (res, served) = serving(l, handler, || -> Res {
        if !started.wait(env.remaining()) {
            return Err(Fail::timeout("request A never started"));
        }
        thread::scope(|t| -> Res {
            let sd = t.spawn(|| l.shutdown(Wait::timeout(Duration::from_secs(10))));
            env.barrier("refused")?;
            env.check(
                "Shutdown waits for the in-flight request",
                !sd.is_finished(),
                "",
            );
            release.open();
            let sres = joined(sd.join());
            env.check_err(
                "Shutdown returns nil after the in-flight request ended",
                sres.err(),
                None,
            );
            let refused = l.stats().refused_requests;
            env.check(
                "refused requests are counted",
                refused >= 1,
                &refused.to_string(),
            );
            // The link stays open: data flows both ways.
            let m = l.stream0().recv(env.wait())?;
            env.check(
                "data from the peer after shutdown",
                m.data() == b"after-shutdown",
                &String::from_utf8_lossy(m.data()),
            );
            drop(m);
            l.stream0().send(b"reply", env.wait())?;
            env.barrier("end")
        })
    });
    res?;
    // Serve ends with nil after shutdown; a client that closed first shows as PeerClosed.
    match served {
        Ok(()) => env.check("Serve returns nil after shutdown", true, ""),
        Err(e) => env.check(
            "Serve returns nil after shutdown",
            e == HqqError::PeerClosed,
            &e.to_string(),
        ),
    }
    Ok(())
}

fn shutdown_requester(env: &mut Env, l: &AdvLink) -> Res {
    let s0 = l.stream0();
    let deadline = env.deadline;
    thread::scope(|t| -> Res {
        let a = t.spawn(move || call(&s0, METHOD_BLOCK, b"A", None, wait_until(deadline)));

        let until = Instant::now() + Duration::from_secs(10);
        let mut refused = false;
        let mut detail = String::new();
        while !refused && Instant::now() < until {
            match call(&s0, METHOD_ECHO, b"B", None, env.wait()) {
                // Answered before shutdown started.
                Ok(_) => continue,
                Err(e) => {
                    refused = e.status == Some(STATUS_SHUTTING_DOWN)
                        && matches!(e.err, HqqError::Response | HqqError::ShuttingDown);
                    detail = e.err.to_string();
                    if !refused {
                        break;
                    }
                }
            }
        }
        env.check(
            "request after shutdown started is refused with status 8 (ErrShuttingDown)",
            refused,
            &detail,
        );
        env.barrier("refused")?;

        match joined(a.join()) {
            Ok(body) => env.check(
                "in-flight request completes normally",
                body == b"A-done",
                &String::from_utf8_lossy(&body),
            ),
            Err(e) => env.check(
                "in-flight request completes normally",
                false,
                &e.err.to_string(),
            ),
        }

        l.stream0().send(b"after-shutdown", env.wait())?;
        let m = l.stream0().recv(env.wait())?;
        env.check(
            "data from the responder after shutdown",
            m.data() == b"reply",
            &String::from_utf8_lossy(m.data()),
        );
        drop(m);
        env.barrier("end")
    })
}

/// Clients the opener connects, one after the other.
const ACCEPTOR_CLIENTS: usize = 3;

/// X-A10: acceptor (LISTENER_DRAFT). The creator runs the acceptor under the name; a
/// second acceptor on the name fails with ErrListenerBusy; three clients connect one
/// after the other, each link answers one echo request and is then closed. A pending
/// Accept fails with ErrAcceptorClosed after the acceptor closes.
pub fn acceptor(env: &mut Env) -> Res {
    if env.creator() {
        acceptor_side(env)
    } else {
        acceptor_client(env)
    }
}

/// A link served on its own thread until the role ends.
struct Server {
    token: Arc<CancelToken>,
    handle: thread::JoinHandle<()>,
}

fn spawn_echo_server(link: Arc<AdvLink>) -> Server {
    let token = Arc::new(CancelToken::new());
    let stop = Arc::clone(&token);
    let handle = thread::spawn(move || {
        let _ = link.serve(echo_handler, Wait::forever().cancel_with(&stop));
    });
    Server { token, handle }
}

fn acceptor_side(env: &mut Env) -> Res {
    let name = env.cfg.name.clone();
    let opts = AcceptorOptions::new().link(env.adv_options());
    let acc = Acceptor::create(&name, &opts, None)?;
    env.barrier("listening")?;

    let second = Acceptor::create(&name, &opts, None);
    env.check_err(
        "second acceptor on the same name is ErrListenerBusy",
        second.as_ref().err().copied(),
        Some(HqqError::ListenerBusy),
    );
    drop(second);

    let mut servers = Vec::with_capacity(ACCEPTOR_CLIENTS);
    for _ in 0..ACCEPTOR_CLIENTS {
        let link = Arc::new(acc.accept(env.wait())?);
        servers.push(spawn_echo_server(link));
    }
    env.received = ACCEPTOR_CLIENTS as u64;
    env.barrier("clients-done")?;

    // The pending Accept runs on another thread while the acceptor closes.
    let deadline = env.deadline;
    let (pending, closed) = thread::scope(|s| {
        let pending = s.spawn(|| acc.accept(wait_until(deadline)).err());
        let closed = acc.close_shared();
        (joined(pending.join()), closed)
    });
    closed?;
    env.check_err(
        "pending Accept fails with ErrAcceptorClosed after Close",
        pending,
        Some(HqqError::AcceptorClosed),
    );
    for server in servers {
        server.token.signal();
        let _ = server.handle.join();
    }
    env.barrier("end")
}

fn acceptor_client(env: &mut Env) -> Res {
    env.barrier("listening")?;
    let opts = ConnectOptions::new()
        .buffer_count(env.cfg.buffer_count)
        .buffer_size(env.cfg.buffer_size)
        .link(env.adv_options());
    for i in 0..ACCEPTOR_CLIENTS {
        let link = hqq::connect(&env.cfg.name, &opts, env.wait())?;
        let payload = format!("client {i}");
        let reply = call(
            &link.stream0(),
            METHOD_ECHO,
            payload.as_bytes(),
            None,
            env.wait(),
        )
        .map_err(Fail::from)?;
        env.check(
            &format!("client {i}: the accepted link answers its request"),
            reply == payload.as_bytes(),
            &String::from_utf8_lossy(&reply),
        );
        drop(link);
    }
    env.sent = ACCEPTOR_CLIENTS as u64;
    env.barrier("clients-done")?;
    env.barrier("end")
}
