//! Echo over a named Standard link.
//!
//!   cargo run -p hqq --example echo -- --create NAME [--timeout-s S]   server: echo until "END"
//!   cargo run -p hqq --example echo -- --open NAME [--messages N] [--size B] [--timeout-s S]
//!                                                                     client: send, check every echo, send "END"
//!   cargo run -p hqq --example echo -- --selftest                      both roles in one process (used by tests)
//!
//! Message i of the client carries its index (8 bytes, little endian) followed by a
//! pattern, and every echo is compared byte for byte. Exit status: 0 success, 1 usage,
//! 2 library error, 3 echo mismatch.

use std::process::exit;
use std::thread;
use std::time::{Duration, Instant};

use hqq::{Link, Options, Wait};

const END: &[u8] = b"END";
const BUFFER_COUNT: u64 = 8;
const BUFFER_SIZE: u64 = 4096;
const USAGE: &str = "usage: echo (--create | --open) NAME [--messages N] [--size B] [--timeout-s S]\n       echo --selftest";

fn fail(code: i32, what: &str, err: impl std::fmt::Display) -> i32 {
    eprintln!("{what}: {err}");
    code
}

/// Opens as Secondary, retrying while the creator is not ready yet.
fn open_retrying(name: &str, budget: Duration) -> hqq::Result<Link> {
    let deadline = Instant::now() + budget;
    loop {
        match Link::open(name, BUFFER_COUNT, BUFFER_SIZE, &Options::new(), None) {
            Err(err) if err.is_transient_open() && Instant::now() < deadline => {
                thread::sleep(Duration::from_millis(10));
            }
            other => return other,
        }
    }
}

fn message(index: u64, size: usize) -> Vec<u8> {
    (0..size)
        .map(|i| {
            if i < 8 {
                (index >> (8 * i)) as u8
            } else {
                (index.wrapping_mul(31).wrapping_add(i as u64)) as u8
            }
        })
        .collect()
}

fn run_server(name: &str, timeout: Duration) -> i32 {
    let link = match Link::create(
        name,
        BUFFER_COUNT,
        BUFFER_SIZE,
        0o600,
        &Options::new(),
        None,
    ) {
        Ok(link) => link,
        Err(err) => return fail(2, "create", err),
    };
    let mut echoed = 0u64;
    loop {
        let bytes = match link.receive(Wait::timeout(timeout)) {
            Ok(bytes) => bytes,
            Err(err) => return fail(2, "receive", err),
        };
        if bytes == END {
            break;
        }
        if let Err(err) = link.write(&bytes, Wait::timeout(timeout)) {
            return fail(2, "echo", err);
        }
        echoed += 1;
    }
    println!("echoed {echoed} messages");
    0
}

fn run_client(name: &str, messages: u64, size: usize, timeout: Duration) -> i32 {
    let link = match open_retrying(name, timeout) {
        Ok(link) => link,
        Err(err) => return fail(2, "open", err),
    };
    let wait = Wait::timeout(timeout);
    for i in 0..messages {
        let n = if i % 10 == 9 {
            BUFFER_SIZE as usize
        } else {
            size.max(16)
        };
        let out = message(i, n);
        if let Err(err) = link.write(&out, wait) {
            return fail(2, "write", err);
        }
        match link.receive(wait) {
            Ok(back) if back == out => {}
            Ok(_) => return fail(3, "echo mismatch", format!("message {i}")),
            Err(err) => return fail(2, "receive", err),
        }
    }
    if let Err(err) = link.write(END, wait) {
        return fail(2, "write END", err);
    }
    println!("verified {messages} echoes");
    0
}

fn run_selftest() -> i32 {
    let name = format!("hqqr-echo-{}", std::process::id());
    let server_name = name.clone();
    let server = thread::spawn(move || run_server(&server_name, Duration::from_secs(20)));
    let client_rc = run_client(&name, 25, 300, Duration::from_secs(20));
    let server_rc = server.join().unwrap_or(2);
    if client_rc != 0 {
        client_rc
    } else {
        server_rc
    }
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let mut name = None;
    let mut create = false;
    let mut selftest = false;
    let mut messages = 100u64;
    let mut size = 64usize;
    let mut timeout_s = 30u64;

    let mut it = args.iter();
    while let Some(arg) = it.next() {
        let mut value = || {
            it.next().cloned().unwrap_or_else(|| {
                eprintln!("{USAGE}");
                exit(1)
            })
        };
        match arg.as_str() {
            "--create" => {
                create = true;
                name = Some(value());
            }
            "--open" => {
                create = false;
                name = Some(value());
            }
            "--selftest" => selftest = true,
            "--messages" => messages = value().parse().unwrap_or(messages),
            "--size" => size = value().parse().unwrap_or(size),
            "--timeout-s" => timeout_s = value().parse().unwrap_or(timeout_s),
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
            Some(name) if create => run_server(&name, timeout),
            Some(name) => run_client(&name, messages, size, timeout),
            None => {
                eprintln!("{USAGE}");
                1
            }
        }
    };
    exit(rc);
}
