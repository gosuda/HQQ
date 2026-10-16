//! Request/response over a named Advanced link.
//!
//!   cargo run -p hqq --example rpc -- --create NAME [--timeout-s S]   service: serve until method 3 (stop)
//!   cargo run -p hqq --example rpc -- --open NAME [--timeout-s S]     client: call the methods, stop the service
//!   cargo run -p hqq --example rpc -- --selftest                      both roles in one process (used by tests)
//!
//! Methods: 1 = upper-case the body, 2 = error response (status 42, "bad request"),
//! 3 = stop the service (response "stopping"). Exit status: 0 success, 1 usage,
//! 2 library error, 3 unexpected response.

use std::process::exit;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;
use std::time::{Duration, Instant};

use hqq::{AdvLink, AdvOptions, Error, Request, Responder, Wait, FEATURES_ALL};

const UPPER: u64 = 1;
const FAIL: u64 = 2;
const STOP: u64 = 3;
const FAIL_STATUS: u64 = 42;

fn fail(code: i32, what: &str, err: impl std::fmt::Display) -> i32 {
    eprintln!("{what}: {err}");
    code
}

/// Runs on worker threads: the only shared state is the atomic stop flag.
fn handle(stop: &AtomicBool, req: &Request<'_>, resp: &mut Responder<'_>) {
    let _ = match req.method() {
        UPPER => resp.write(&req.data().to_ascii_uppercase()),
        FAIL => resp.error(FAIL_STATUS, b"bad request"),
        STOP => {
            stop.store(true, Ordering::SeqCst);
            resp.write(b"stopping")
        }
        _ => resp.error(6, b"unknown method"),
    };
}

fn open_retrying(name: &str, budget: Duration) -> hqq::Result<AdvLink> {
    let deadline = Instant::now() + budget;
    loop {
        match AdvLink::open(name, 8, 4096, &AdvOptions::new(), None) {
            Err(err) if err.is_transient_open() && Instant::now() < deadline => {
                thread::sleep(Duration::from_millis(10));
            }
            other => return other,
        }
    }
}

fn run_service(name: &str, timeout: Duration) -> i32 {
    let link = match AdvLink::create(name, 8, 4096, 0o600, &AdvOptions::new(), None) {
        Ok(link) => link,
        Err(err) => return fail(2, "create", err),
    };
    if let Err(err) = link.negotiate(FEATURES_ALL, Wait::timeout(timeout)) {
        return fail(2, "negotiate", err);
    }
    let stop = AtomicBool::new(false);
    let deadline = Instant::now() + timeout;
    let served = thread::scope(|s| {
        let server =
            s.spawn(|| link.serve(|req, resp| handle(&stop, req, resp), Wait::timeout(timeout)));
        while !stop.load(Ordering::SeqCst) && Instant::now() < deadline {
            thread::sleep(Duration::from_millis(2));
        }
        // Graceful shutdown: refuses new requests and waits for the in-flight stop response.
        let shut = link.shutdown(Wait::timeout(timeout));
        let served = server.join().unwrap_or(Err(Error::InvalidOp));
        // The client closes its link right after the stop response, which ends serving as PeerClosed.
        let served = match served {
            Err(Error::PeerClosed) => Ok(()),
            other => other,
        };
        shut.and(served)
    });
    if let Err(err) = served {
        return fail(2, "serve", err);
    }
    let by_client = stop.load(Ordering::SeqCst);
    println!(
        "service stopped ({})",
        if by_client { "by client" } else { "by timeout" }
    );
    if by_client {
        0
    } else {
        3
    }
}

fn run_client(name: &str, timeout: Duration) -> i32 {
    let link = match open_retrying(name, timeout) {
        Ok(link) => link,
        Err(err) => return fail(2, "open", err),
    };
    let wait = Wait::timeout(timeout);
    if let Err(err) = link.negotiate(FEATURES_ALL, wait) {
        return fail(2, "negotiate", err);
    }
    let stream = link.stream0();

    match stream.call(UPPER, b"hello, rpc", wait) {
        Ok(reply) if reply.data() == b"HELLO, RPC" => {
            println!("upper: {}", String::from_utf8_lossy(reply.data()));
        }
        Ok(_) => return fail(3, "upper", "unexpected body"),
        Err(err) => return fail(3, "upper", err.error),
    }
    match stream.call(FAIL, b"", wait) {
        Err(err) if err.error == Error::Response => {
            let payload = err
                .response
                .as_ref()
                .map(|m| (m.status(), m.data().to_vec()));
            match payload {
                Some((status, body)) if status == FAIL_STATUS => {
                    println!(
                        "error response: status {status}, payload \"{}\"",
                        String::from_utf8_lossy(&body)
                    );
                }
                _ => return fail(3, "fail", "unexpected error payload"),
            }
        }
        _ => return fail(3, "fail", "expected an error response"),
    }
    let stopped = match stream.call(STOP, b"", wait) {
        Ok(_) => 0,
        Err(err) => fail(3, "stop", err.error),
    };
    stopped
}

fn run_selftest() -> i32 {
    let name = format!("hqqr-rpc-{}", std::process::id());
    let service_name = name.clone();
    let service = thread::spawn(move || run_service(&service_name, Duration::from_secs(20)));
    let client_rc = run_client(&name, Duration::from_secs(20));
    let service_rc = service.join().unwrap_or(2);
    if client_rc != 0 {
        client_rc
    } else {
        service_rc
    }
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    const USAGE: &str =
        "usage: rpc (--create | --open) NAME [--timeout-s S]\n       rpc --selftest";
    let mut name = None;
    let mut create = false;
    let mut selftest = false;
    let mut timeout_s = 30u64;

    let mut it = args.iter();
    while let Some(arg) = it.next() {
        match arg.as_str() {
            "--create" | "--open" => {
                create = arg == "--create";
                name = it.next().cloned();
            }
            "--selftest" => selftest = true,
            "--timeout-s" => {
                timeout_s = it.next().and_then(|v| v.parse().ok()).unwrap_or(timeout_s)
            }
            _ => {
                eprintln!("{USAGE}");
                exit(1);
            }
        }
    }
    let timeout = Duration::from_secs(timeout_s.max(1));
    let rc = if selftest {
        run_selftest()
    } else {
        match name {
            Some(name) if create => run_service(&name, timeout),
            Some(name) => run_client(&name, timeout),
            None => {
                eprintln!("{USAGE}");
                1
            }
        }
    };
    exit(rc);
}
