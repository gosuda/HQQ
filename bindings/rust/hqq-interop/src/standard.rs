//! Standard-link scenarios (SPEC 9): X-01, X-02, X-03, X-04 (before-creator, dead),
//! X-05, X-06, X-07, X-12, X-14. Each follows the Go reference (cmd/hqq-interop/std_*.go).

use std::thread;
use std::time::{Duration, Instant};

use hqq::{Error as HqqError, Link, Mode, Wait};

use crate::env::{wait_until, Env, Fail};
use crate::payload::{intact, payload};

/// A shared-memory name outlives its last handle only on POSIX (Go shm.NamesPersist);
/// on Windows it lives while any handle to it is open.
const NAMES_PERSIST: bool = !cfg!(windows);

type Res = Result<(), Fail>;

fn create(env: &Env) -> hqq::Result<Link> {
    let c = &env.cfg;
    Link::create(
        &c.name,
        c.buffer_count,
        c.buffer_size,
        0o600,
        &env.std_options(),
        None,
    )
}

fn open(env: &Env) -> hqq::Result<Link> {
    let c = &env.cfg;
    Link::open(
        &c.name,
        c.buffer_count,
        c.buffer_size,
        &env.std_options(),
        None,
    )
}

/// Creates the segment, then posts the "created" barrier that makes it openable.
fn create_signal(env: &mut Env) -> Result<Link, Fail> {
    let link = create(env)?;
    env.barrier("created")?;
    Ok(link)
}

/// Waits for the creator's "created" barrier, then opens the segment.
fn open_after(env: &mut Env) -> Result<Link, Fail> {
    env.barrier("created")?;
    Ok(open(env)?)
}

/// Opens the name and reports the open error only; a successful probe is closed.
fn probe_open(env: &Env) -> Option<HqqError> {
    match open(env) {
        Ok(link) => {
            drop(link);
            None
        }
        Err(e) => Some(e),
    }
}

fn joined<T>(r: thread::Result<T>) -> T {
    r.unwrap_or_else(|panic| std::panic::resume_unwind(panic))
}

/// X-01: create, open, exchange, close. The creator writes Payload(seed, 0, size),
/// the opener verifies it and replies with Payload(seed+1, 0, size).
pub fn x01(env: &mut Env) -> Res {
    let size = env.size();
    let seed = env.cfg.seed;
    let creator = env.creator();
    let creator_first = env.cfg.variant == "creator-closes-first";
    let link = if creator {
        create_signal(env)?
    } else {
        open_after(env)?
    };
    if creator {
        link.write(&payload(seed, 0, size), env.wait())?;
        env.sent += 1;
        let reply = link.receive(env.wait())?;
        env.received += 1;
        env.check(
            "creator receives the reply intact",
            intact(seed + 1, 0, size, &reply),
            "",
        );
    } else {
        let msg = link.receive(env.wait())?;
        env.received += 1;
        env.check(
            "opener receives the message intact",
            intact(seed, 0, size, &msg),
            "",
        );
        link.write(&payload(seed + 1, 0, size), env.wait())?;
        env.sent += 1;
    }
    env.barrier("replied")?;

    if creator == creator_first {
        // This side closes first.
        link.close()?;
        let probe = probe_open(env);
        if creator {
            if NAMES_PERSIST {
                env.check_err(
                    "creator close removes the name",
                    probe,
                    Some(HqqError::NotExist),
                );
            } else {
                // Windows: the opener still holds its endpoint, so the name is there and busy.
                env.check_err(
                    "creator close keeps the name while the opener holds it (Windows)",
                    probe,
                    Some(HqqError::LinkBusy),
                );
            }
        } else {
            env.check_err(
                "opener close leaves the name in place",
                probe,
                Some(HqqError::LinkBusy),
            );
        }
        return env.barrier("closed");
    }

    // The other side waits for the peer's close, then sees the end of the stream.
    let r = link.receive(env.wait());
    env.check_err(
        "peer close is seen as io.EOF after buffered data",
        r.err(),
        Some(HqqError::PeerClosed),
    );
    env.barrier("closed")?;
    if creator {
        link.close()?;
        let probe = probe_open(env);
        env.check_err(
            "creator close removes the name",
            probe,
            Some(HqqError::NotExist),
        );
    }
    Ok(())
}

/// X-02: geometry mismatch. The opener uses another buffer count or size; it must
/// fail with ErrInvalidSize (a geometry mismatch matches it).
pub fn x02(env: &mut Env) -> Res {
    if env.creator() {
        let link = create_signal(env)?;
        env.check("creator creates the segment", true, "");
        env.barrier("checked")?;
        drop(link);
        return Ok(());
    }
    env.barrier("created")?;
    let r = open(env);
    let err = r.as_ref().err().copied();
    drop(r);
    env.check_err(
        "mismatched geometry fails with ErrInvalidSize",
        err,
        Some(HqqError::InvalidSize),
    );
    env.meta_bool("geometry_error", err == Some(HqqError::GeometryMismatch));
    env.barrier("checked")
}

/// X-03: third endpoint, re-join after own close, creator vs live segment.
pub fn x03(env: &mut Env) -> Res {
    if env.creator() {
        let link = create_signal(env)?;
        env.barrier("joined")?;
        let again = create(env);
        env.check_err(
            "creator on a live name fails with ErrLinkBusy",
            again.as_ref().err().copied(),
            Some(HqqError::LinkBusy),
        );
        drop(again);
        env.barrier("creator-checked")?;
        env.barrier("reopen-done")?;
        drop(link);
        return Ok(());
    }

    let first = open_after(env)?;
    env.barrier("joined")?;
    let third = open(env);
    env.check_err(
        "third endpoint fails with ErrLinkBusy",
        third.as_ref().err().copied(),
        Some(HqqError::LinkBusy),
    );
    drop(third);
    env.barrier("creator-checked")?;
    first.close()?;
    let again = open(env);
    env.check_err(
        "re-join after own close fails with ErrLinkBusy",
        again.as_ref().err().copied(),
        Some(HqqError::LinkBusy),
    );
    drop(again);
    env.barrier("reopen-done")
}

/// X-04 before-creator: the opener's first attempt finds no segment; after the
/// creator is up the open succeeds.
pub fn x04_before(env: &mut Env) -> Res {
    if !env.creator() {
        let start = Instant::now();
        let r = open(env);
        let elapsed = start.elapsed();
        let transient = match &r {
            Ok(_) => false,
            Err(e) => e.matches(HqqError::NotExist) || e.matches(HqqError::FailedInit),
        };
        let detail = r.as_ref().err().map(|e| e.to_string()).unwrap_or_default();
        drop(r);
        env.check(
            "first attempt before the creator fails transiently",
            transient,
            &detail,
        );
        env.meta_num("first_attempt_ms", elapsed.as_millis());
        env.barrier("attempt")?;
        env.barrier("created")?;
        let link = open(env)?;
        let mode = link.mode();
        env.check(
            "opener joins as Secondary",
            mode == Mode::Secondary,
            &format!("{mode:?}"),
        );
        return env.barrier("joined");
    }
    env.barrier("attempt")?;
    let link = create(env)?;
    env.barrier("created")?;
    env.barrier("joined")?;
    drop(link);
    Ok(())
}

/// X-04 dead: step 1 joins and the creator dies at exit; step 2 is the opener on
/// the dead segment, which must fail with ErrFailedInit quickly.
pub fn x04_dead(env: &mut Env) -> Res {
    if env.cfg.step == 2 {
        let start = Instant::now();
        let r = open(env);
        let elapsed = start.elapsed();
        let err = r.as_ref().err().copied();
        drop(r);
        // Windows: the dead segment's name is gone, which is ErrNotExist (as in the Go helper).
        let vanished = !NAMES_PERSIST && err == Some(HqqError::NotExist);
        env.check(
            "opener on a dead segment fails with ErrFailedInit (ErrNotExist on Windows)",
            err.is_some_and(|e| e.matches(HqqError::FailedInit)) || vanished,
            &format!("{err:?}"),
        );
        let bound = env.stale_after() + Duration::from_secs(3);
        env.check(
            "failure is quick (errNoLiveEndpoint)",
            elapsed <= bound,
            &format!("{elapsed:?}, bound {bound:?}"),
        );
        return Ok(());
    }
    if env.creator() {
        let link = create_signal(env)?;
        env.barrier("joined")?;
        env.maybe_crash("exit");
        drop(link);
        return Ok(());
    }
    let link = open_after(env)?;
    env.barrier("joined")?;
    drop(link);
    Ok(())
}

/// X-05: ring full and backpressure. The creator (2 slots) fills the ring; its third
/// write with a 200 ms deadline must time out without losing data.
pub fn x05(env: &mut Env) -> Res {
    let size = env.size();
    let seed = env.cfg.seed;
    if env.creator() {
        let link = create_signal(env)?;
        for i in 0..2 {
            link.write(&payload(seed, i, size), env.wait())?;
            env.sent += 1;
        }
        let r = link.write(
            &payload(seed, 2, size),
            Wait::timeout(Duration::from_millis(200)),
        );
        env.check_err(
            "third write on a full ring times out",
            r.err(),
            Some(HqqError::Timeout),
        );
        env.barrier("filled")?;
        env.barrier("drained")?;
        link.write(&payload(seed, 2, size), env.wait())?;
        env.sent += 1;
        return env.barrier("received");
    }

    let link = open_after(env)?;
    env.barrier("filled")?;
    let mut in_order = true;
    for i in 0..2 {
        let m = link.receive(env.wait())?;
        env.received += 1;
        if !intact(seed, i, size, &m) {
            in_order = false;
        }
    }
    env.check(
        "the two buffered messages arrive in order and intact",
        in_order,
        "",
    );
    env.barrier("drained")?;
    let m = link.receive(env.wait())?;
    env.received += 1;
    env.check(
        "the third message arrives intact after the drain",
        intact(seed, 2, size, &m),
        "",
    );
    env.barrier("received")
}

/// X-06: bidirectional stream, sequence and CRC. Each side writes n messages while
/// it reads n from the peer; the creator's stream uses seed, the opener's seed+1.
pub fn x06(env: &mut Env) -> Res {
    let n = if env.cfg.long {
        100_000
    } else {
        env.cfg.messages
    };
    let link = if env.creator() {
        create_signal(env)?
    } else {
        open_after(env)?
    };
    bidirectional(env, &link, n)?;
    env.barrier("done")
}

fn bidirectional(env: &mut Env, link: &Link, n: u64) -> Res {
    let size = env.size();
    let seed = env.cfg.seed;
    let (out_seed, in_seed) = if env.creator() {
        (seed, seed + 1)
    } else {
        (seed + 1, seed)
    };
    let deadline = env.deadline;

    type WriterOut = (u64, Result<(), Fail>);
    type ReaderOut = (u64, Option<String>, Result<(), Fail>);
    let (writer, reader): (WriterOut, ReaderOut) = thread::scope(|s| {
        let w = s.spawn(move || {
            let mut sent = 0u64;
            for i in 0..n {
                match link.write(&payload(out_seed, i, size), wait_until(deadline)) {
                    Ok(()) => sent += 1,
                    Err(e) => return (sent, Err(Fail::from(e))),
                }
            }
            (sent, Ok(()))
        });
        let r = s.spawn(move || {
            let mut got = 0u64;
            for i in 0..n {
                match link.receive(wait_until(deadline)) {
                    Ok(msg) => {
                        if !intact(in_seed, i, size, &msg) {
                            return (got, Some(format!("message {i} of the peer stream")), Ok(()));
                        }
                        got += 1;
                    }
                    Err(e) => return (got, None, Err(Fail::from(e))),
                }
            }
            (got, None, Ok(()))
        });
        (joined(w.join()), joined(r.join()))
    });

    env.sent = writer.0;
    env.received = reader.0;
    writer.1?;
    reader.2?;
    match reader.1 {
        Some(what) => env.check(
            "both streams intact and in order",
            false,
            &format!("{what} is not intact"),
        ),
        None => env.check("both streams intact and in order", true, ""),
    }
    Ok(())
}

/// X-07 (and the body of the X-07 kill tests): kill -9 of the peer, parked or
/// running. The creator sends N messages and may be killed right after the last
/// one (`msgs:N`); the opener may be killed while parked (`blocked`). The surviving
/// side must get ErrPeerGone within the bound.
pub fn kill_peer(env: &mut Env) -> Res {
    let n = env.cfg.messages;
    let size = env.size();
    let seed = env.cfg.seed;
    let link = if env.creator() {
        create_signal(env)?
    } else {
        open_after(env)?
    };
    env.barrier("joined")?;
    let bound = env.peer_bound();

    if env.creator() {
        for i in 0..n {
            link.write(&payload(seed, i, size), env.wait())?;
            env.sent += 1;
        }
        env.maybe_crash(&format!("msgs:{n}"));
        // The opener was killed: its blocked read must end with ErrPeerGone.
        let start = Instant::now();
        let r = link.receive(env.wait());
        let elapsed = start.elapsed();
        env.check_err(
            "blocked read after the opener died gets ErrPeerGone",
            r.err(),
            Some(HqqError::PeerGone),
        );
        env.check(
            "peer loss detected within the bound",
            elapsed <= bound,
            &format!("{elapsed:?}, bound {bound:?}"),
        );
        return Ok(());
    }

    let mut intact_all = true;
    for i in 0..n {
        let m = link.receive(env.wait())?;
        env.received += 1;
        if !intact(seed, i, size, &m) {
            intact_all = false;
        }
    }
    env.check(
        "all buffered messages delivered intact before the peer loss",
        intact_all && env.received == n,
        &format!("{} of {n}", env.received),
    );
    env.maybe_crash("blocked");
    // The creator was killed right after its last send: ErrPeerGone follows the
    // buffered messages within the bound.
    let start = Instant::now();
    let r = link.receive(env.wait());
    let elapsed = start.elapsed();
    env.check_err(
        "read after the buffered messages gets ErrPeerGone",
        r.err(),
        Some(HqqError::PeerGone),
    );
    env.check(
        "peer loss detected within the bound",
        elapsed <= bound,
        &format!("{elapsed:?}, bound {bound:?}"),
    );
    Ok(())
}

/// X-12: parked consumer woken by a producer. Ping-pong over a ring of 2 with 50 ms
/// idle; every round trip must stay under 250 ms.
pub fn x12(env: &mut Env) -> Res {
    let n = env.cfg.messages;
    ping_run(
        env,
        n,
        Duration::from_millis(50),
        Duration::from_millis(250),
    )
}

/// X-14: simple-mode peer. Ping-pong with 300 ms idle; the round trip must stay
/// within 150 ms. The spin-peer variant runs the opener with `--wait spin`.
pub fn x14(env: &mut Env) -> Res {
    let n = env.cfg.messages;
    ping_run(
        env,
        n,
        Duration::from_millis(300),
        Duration::from_millis(150),
    )
}

fn ping_run(env: &mut Env, n: u64, idle: Duration, max_rtt: Duration) -> Res {
    let link = if env.creator() {
        create_signal(env)?
    } else {
        open_after(env)?
    };
    ping_pong(env, &link, n, idle, max_rtt)?;
    env.barrier("done")
}

/// The creator sleeps idle, sends Payload(seed, i) and waits for the echo; the
/// opener echoes every message. The creator checks each echo and the worst round trip.
fn ping_pong(env: &mut Env, link: &Link, n: u64, idle: Duration, max_rtt: Duration) -> Res {
    let size = env.size();
    let seed = env.cfg.seed;
    let creator = env.creator();
    let mut worst = Duration::ZERO;
    let mut verify_err: Option<String> = None;
    for i in 0..n {
        if creator {
            env.sleep(idle)?;
            let t0 = Instant::now();
            link.write(&payload(seed, i, size), env.wait())?;
            env.sent += 1;
            let echo = link.receive(env.wait())?;
            worst = worst.max(t0.elapsed());
            if !intact(seed, i, size, &echo) && verify_err.is_none() {
                verify_err = Some(format!("round {i}: echo is not intact"));
            }
            env.received += 1;
        } else {
            let msg = link.receive(env.wait())?;
            env.received += 1;
            if !intact(seed, i, size, &msg) && verify_err.is_none() {
                verify_err = Some(format!("round {i}: message is not intact"));
            }
            link.write(&msg, env.wait())?;
            env.sent += 1;
        }
    }
    match verify_err {
        Some(what) => env.check("every message intact", false, &what),
        None => env.check("every message intact", true, ""),
    }
    if creator {
        env.check(
            &format!("every round trip <= {max_rtt:?}"),
            worst <= max_rtt,
            &format!("worst {worst:?}"),
        );
        env.meta_num("worst_rtt_ms", worst.as_millis());
    }
    Ok(())
}
