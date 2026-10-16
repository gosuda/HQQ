//! hqq-interop: the Rust helper of the HQQ interoperability contract
//! (conformance/INTEROP_CLI.md), built on the safe `hqq` binding.
//!
//! One invocation runs one role of one scenario in this process:
//!
//!   hqq-interop run --scenario X-01 --variant ... --step N --role creator|opener ...
//!   hqq-interop list [--impl LABEL]
//!   hqq-interop version

mod advanced;
mod catalog;
mod cli;
mod env;
mod payload;
mod result;
mod standard;

use std::process;
use std::thread;
use std::time::{Duration, Instant};

use env::{Env, Fail};
use result::ResultDoc;

const EXIT_OK: i32 = 0;
const EXIT_FAILED: i32 = 1;
const EXIT_USAGE: i32 = 2;
const EXIT_TIMEOUT: i32 = 3;
const EXIT_SKIPPED: i32 = 4;

/// The watchdog fires this long after the process deadline (the Go reference's grace).
const WATCHDOG_GRACE: Duration = Duration::from_secs(2);

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let code = match args.first().map(String::as_str) {
        Some("run") => run_command(&args[1..]),
        Some("list") => list_command(&args[1..]),
        Some("version") => {
            println!(
                "{{\"helper\":\"hqq-interop\",\"impl\":\"rust\",\"protocol\":\"1.1\",\"segment_layout\":\"HQQ_STL3\",\"ring_layout\":2}}"
            );
            EXIT_OK
        }
        Some("-h") | Some("--help") | Some("help") => {
            print_usage();
            EXIT_OK
        }
        Some(other) => {
            eprintln!("hqq-interop: unknown subcommand {other:?}");
            print_usage();
            EXIT_USAGE
        }
        None => {
            print_usage();
            EXIT_USAGE
        }
    };
    process::exit(code);
}

fn print_usage() {
    eprintln!(
        "usage:\n  hqq-interop run --scenario ID --role creator|opener|a|b --name NAME [flags]\n  hqq-interop list [--impl LABEL]\n  hqq-interop version"
    );
}

fn list_command(args: &[String]) -> i32 {
    let mut impl_label = "rust".to_string();
    let mut i = 0;
    while i < args.len() {
        let arg = &args[i];
        i += 1;
        if let Some(v) = arg.strip_prefix("--impl=") {
            impl_label = v.to_string();
        } else if arg == "--impl" {
            match args.get(i) {
                Some(v) => {
                    impl_label = v.clone();
                    i += 1;
                }
                None => {
                    eprintln!("hqq-interop: --impl needs a value");
                    return EXIT_USAGE;
                }
            }
        } else {
            eprintln!("hqq-interop: unknown flag {arg:?}");
            return EXIT_USAGE;
        }
    }
    println!("{}", catalog::list_json(&impl_label));
    EXIT_OK
}

fn run_command(args: &[String]) -> i32 {
    let (mut cfg, explicit) = match cli::parse_run(args) {
        Ok(parsed) => parsed,
        Err(e) => {
            eprintln!("hqq-interop: {e}");
            return EXIT_USAGE;
        }
    };
    let Some(sc) = catalog::scenario(&cfg.scenario) else {
        eprintln!("hqq-interop: unknown scenario {:?}", cfg.scenario);
        return EXIT_USAGE;
    };
    let Some(variant) = catalog::variant(sc, &cfg.variant) else {
        eprintln!(
            "hqq-interop: scenario {} has no variant {:?}",
            sc.id, cfg.variant
        );
        return EXIT_USAGE;
    };
    if cfg.step > variant.steps.len() {
        eprintln!(
            "hqq-interop: scenario {} variant {:?} has {} steps",
            sc.id,
            variant.name,
            variant.steps.len()
        );
        return EXIT_USAGE;
    }
    let step = &variant.steps[cfg.step - 1];
    if !step.roles.contains(&cfg.role.name()) {
        eprintln!(
            "hqq-interop: role {:?} is not part of step {} of {}",
            cfg.role.name(),
            cfg.step,
            sc.id
        );
        return EXIT_USAGE;
    }
    cfg.variant = variant.name.to_string();
    cli::apply_params(&mut cfg, &explicit, variant.params);
    if cfg.size == 0 {
        cfg.size = cfg.buffer_size;
    }
    if step.roles.len() > 1 && cfg.sync_dir.as_os_str().is_empty() {
        eprintln!("hqq-interop: --sync-dir is required for multi-process steps");
        return EXIT_USAGE;
    }

    let base = ResultDoc::new(&cfg);
    let timeout = cfg.timeout;
    let crash_after = cfg.crash_after.clone();
    let mut env = Env::new(cfg);
    let start = env.start;

    if !platform_ok(sc.platforms) {
        let msg = format!("scenario not applicable on {}", std::env::consts::OS);
        return finish_skip(&env, &msg);
    }
    if !crash_point_supported(&crash_after) {
        let msg = format!("crash point {crash_after:?} is not implemented by this helper");
        return finish_skip(&env, &msg);
    }

    arm_watchdog(base, timeout, start);
    let outcome = dispatch(&mut env, sc.id);
    match outcome {
        Err(Fail::Skip(msg)) => finish_skip(&env, &msg),
        Ok(()) if !env.failed => {
            result::emit(&env.doc(true, false, false, None));
            EXIT_OK
        }
        Ok(()) => {
            let f = Fail::err("CheckFailed", "one or more checks failed");
            result::emit(&env.doc(false, false, false, Some(&f)));
            EXIT_FAILED
        }
        Err(f) => {
            result::emit(&env.doc(false, false, false, Some(&f)));
            EXIT_FAILED
        }
    }
}

fn finish_skip(env: &Env, msg: &str) -> i32 {
    let f = Fail::Skip(msg.to_string());
    result::emit(&env.doc(true, true, false, Some(&f)));
    EXIT_SKIPPED
}

/// The scenario runs on this GOOS; every listed scenario is platform "any" today.
fn platform_ok(platforms: &[&str]) -> bool {
    platforms
        .iter()
        .any(|p| *p == "any" || *p == std::env::consts::OS)
}

/// Crash points this helper implements: msgs:N, blocked and exit. The raw points
/// (claim, format, publish, zeroing) need protocol writes below the public API.
fn crash_point_supported(point: &str) -> bool {
    point.is_empty()
        || point == "blocked"
        || point == "exit"
        || point
            .strip_prefix("msgs:")
            .is_some_and(|n| n.parse::<u64>().is_ok())
}

/// Prints a HarnessTimeout result and exits 3 if the role is still running
/// `WATCHDOG_GRACE` after its deadline (INTEROP_CLI.md, section 3).
fn arm_watchdog(mut base: ResultDoc, timeout: Duration, start: Instant) {
    thread::spawn(move || {
        thread::sleep(timeout + WATCHDOG_GRACE);
        base.ok = false;
        base.error_kind = "HarnessTimeout".to_string();
        base.error = "process exceeded its deadline".to_string();
        base.elapsed_ms = start.elapsed().as_millis();
        if result::emit(&base) {
            process::exit(EXIT_TIMEOUT);
        }
    });
}

fn dispatch(env: &mut Env, scenario: &str) -> Result<(), Fail> {
    let variant = env.cfg.variant.clone();
    match (scenario, variant.as_str()) {
        ("X-01", _) => standard::x01(env),
        ("X-02", _) => standard::x02(env),
        ("X-03", _) => standard::x03(env),
        ("X-04", "before-creator") => standard::x04_before(env),
        ("X-04", "dead") => standard::x04_dead(env),
        ("X-05", _) => standard::x05(env),
        ("X-06", _) => standard::x06(env),
        ("X-07", _) => standard::kill_peer(env),
        ("X-12", _) => standard::x12(env),
        ("X-14", _) => standard::x14(env),
        ("X-A01", _) => advanced::negotiation(env),
        ("X-A02", _) => advanced::sizes(env),
        ("X-A04", _) => advanced::rpc(env),
        ("X-A05", _) => advanced::streams_basic(env),
        ("X-A08", _) => advanced::metadata(env),
        ("X-A09", _) => advanced::shutdown(env),
        ("X-A10", _) => advanced::acceptor(env),
        _ => Err(Fail::Skip(format!(
            "variant {variant:?} of {scenario} is not implemented"
        ))),
    }
}
