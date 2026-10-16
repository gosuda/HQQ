//! Flags of `hqq-interop run` (INTEROP_CLI.md, section 2).

use std::collections::{HashMap, HashSet};
use std::fmt::Display;
use std::path::PathBuf;
use std::str::FromStr;
use std::time::Duration;

/// The role of this process: `creator` is the Primary, `opener` the Secondary.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Role {
    Creator,
    Opener,
}

impl Role {
    pub fn name(self) -> &'static str {
        match self {
            Role::Creator => "creator",
            Role::Opener => "opener",
        }
    }

    pub fn peer(self) -> Role {
        match self {
            Role::Creator => Role::Opener,
            Role::Opener => Role::Creator,
        }
    }
}

/// The parsed `run` configuration. Zero or empty means "library default" where the
/// contract says so; `size` is already defaulted to the buffer size by the caller.
#[derive(Clone, Debug)]
pub struct Config {
    pub scenario: String,
    pub variant: String,
    pub step: usize,
    pub role: Role,
    pub name: String,
    pub sync_dir: PathBuf,
    pub impl_label: String,
    pub peer_impl: String,
    pub buffer_count: u64,
    pub buffer_size: u64,
    pub size: u64,
    pub messages: u64,
    pub seed: u64,
    pub timeout: Duration,
    pub crash_after: String,
    pub heartbeat_ms: i64,
    pub peer_timeout_ms: i64,
    pub attach_timeout: Duration,
    pub spin: bool,
    pub long: bool,
    pub max_message: u64,
    pub max_reassembly: u64,
    pub queue_size: u64,
}

/// Names of the flags given explicitly on the command line (with hyphens).
pub type Explicit = HashSet<String>;

const FLAGS: &[&str] = &[
    "scenario",
    "variant",
    "step",
    "role",
    "name",
    "sync-dir",
    "impl",
    "peer-impl",
    "buffer-count",
    "buffer-size",
    "size",
    "messages",
    "seed",
    "timeout",
    "crash-after",
    "heartbeat",
    "peer-timeout",
    "attach-timeout",
    "wait",
    "long",
    "max-message",
    "max-reassembly",
    "queue-size",
];

const BOOL_FLAGS: &[&str] = &["long"];

/// The default of `--impl` is the contract's (section 2): `go`.
const DEFAULT_IMPL: &str = "go";

fn number<T>(vals: &HashMap<String, String>, key: &str, default: T) -> Result<T, String>
where
    T: FromStr,
    T::Err: Display,
{
    match vals.get(key) {
        None => Ok(default),
        Some(v) => v.parse::<T>().map_err(|e| format!("--{key}: {e}")),
    }
}

/// Parses the arguments of `run`. Unknown flags and malformed values are usage errors.
pub fn parse_run(args: &[String]) -> Result<(Config, Explicit), String> {
    let mut vals: HashMap<String, String> = HashMap::new();
    let mut explicit: Explicit = HashSet::new();
    let mut i = 0;
    while i < args.len() {
        let arg = &args[i];
        i += 1;
        let body = arg
            .strip_prefix("--")
            .ok_or_else(|| format!("unexpected argument {arg:?}"))?;
        let (key, inline) = match body.split_once('=') {
            Some((k, v)) => (k, Some(v.to_string())),
            None => (body, None),
        };
        if !FLAGS.contains(&key) {
            return Err(format!("unknown flag --{key}"));
        }
        let value = if BOOL_FLAGS.contains(&key) {
            inline.unwrap_or_else(|| "true".to_string())
        } else {
            match inline {
                Some(v) => v,
                None => {
                    let v = args
                        .get(i)
                        .ok_or_else(|| format!("flag --{key} needs a value"))?
                        .clone();
                    i += 1;
                    v
                }
            }
        };
        explicit.insert(key.to_string());
        vals.insert(key.to_string(), value);
    }

    let role = match vals.get("role").map(String::as_str) {
        Some("creator") | Some("a") => Role::Creator,
        Some("opener") | Some("b") => Role::Opener,
        other => {
            return Err(format!(
                "--role must be creator|opener|a|b, got {:?}",
                other.unwrap_or("")
            ))
        }
    };
    let scenario = vals
        .get("scenario")
        .cloned()
        .ok_or("--scenario is required")?;
    let name = vals.get("name").cloned().ok_or("--name is required")?;
    let step: usize = number(&vals, "step", 1)?;
    if step < 1 {
        return Err("--step must be >= 1".into());
    }
    let buffer_count: u64 = number(&vals, "buffer-count", 4)?;
    let buffer_size: u64 = number(&vals, "buffer-size", 256)?;
    let messages: u64 = number(&vals, "messages", 100)?;
    if buffer_count == 0 || buffer_size == 0 {
        return Err("sizes and counts must be positive".into());
    }
    let wait = vals.get("wait").map(String::as_str).unwrap_or("adaptive");
    if wait != "adaptive" && wait != "spin" {
        return Err(format!("--wait must be adaptive|spin, got {wait:?}"));
    }
    let secs: f64 = number(&vals, "timeout", 20.0)?;
    let timeout = Duration::try_from_secs_f64(secs)
        .map_err(|_| format!("--timeout: invalid duration {secs}"))?;
    let attach_ms: u64 = number(&vals, "attach-timeout", 0)?;

    let cfg = Config {
        scenario,
        variant: vals.get("variant").cloned().unwrap_or_default(),
        step,
        role,
        name,
        sync_dir: PathBuf::from(vals.get("sync-dir").cloned().unwrap_or_default()),
        impl_label: vals
            .get("impl")
            .cloned()
            .unwrap_or_else(|| DEFAULT_IMPL.to_string()),
        peer_impl: vals.get("peer-impl").cloned().unwrap_or_default(),
        buffer_count,
        buffer_size,
        size: number(&vals, "size", 0)?,
        messages,
        seed: number(&vals, "seed", 1)?,
        timeout,
        crash_after: vals.get("crash-after").cloned().unwrap_or_default(),
        heartbeat_ms: number(&vals, "heartbeat", 0)?,
        peer_timeout_ms: number(&vals, "peer-timeout", 0)?,
        attach_timeout: Duration::from_millis(attach_ms),
        spin: wait == "spin",
        long: vals.get("long").is_some_and(|v| v == "true"),
        max_message: number(&vals, "max-message", 0)?,
        max_reassembly: number(&vals, "max-reassembly", 0)?,
        queue_size: number(&vals, "queue-size", 0)?,
    };
    Ok((cfg, explicit))
}

/// Applies a variant's params as defaults: a flag given explicitly always wins
/// (INTEROP_CLI.md, section 2 and the Go reference's `applyParams`).
pub fn apply_params(cfg: &mut Config, explicit: &Explicit, params: &[(&str, i64)]) {
    for &(key, value) in params {
        // buffer_count -> buffer-count, heartbeat_ms -> heartbeat, attach_timeout_ms -> attach-timeout.
        let flag = key.trim_end_matches("_ms").replace('_', "-");
        if explicit.contains(&flag) {
            continue;
        }
        match key {
            "buffer_count" => cfg.buffer_count = value as u64,
            "buffer_size" => cfg.buffer_size = value as u64,
            "size" => cfg.size = value as u64,
            "messages" => cfg.messages = value as u64,
            "seed" => cfg.seed = value as u64,
            "heartbeat_ms" => cfg.heartbeat_ms = value,
            "peer_timeout_ms" => cfg.peer_timeout_ms = value,
            "attach_timeout_ms" => cfg.attach_timeout = Duration::from_millis(value as u64),
            "max_message" => cfg.max_message = value as u64,
            "max_reassembly" => cfg.max_reassembly = value as u64,
            "queue_size" => cfg.queue_size = value as u64,
            _ => {}
        }
    }
}
