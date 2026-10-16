//! The result line (INTEROP_CLI.md, section 4): rendering and the once-only emit.

use std::io::{self, Write};
use std::sync::atomic::{AtomicBool, Ordering};

use crate::cli::Config;

/// One named assertion of the role.
#[derive(Clone, Debug)]
pub struct Check {
    pub name: String,
    pub ok: bool,
    pub detail: String,
}

/// Everything the result line carries. `meta` values are pre-rendered JSON.
#[derive(Clone, Debug)]
pub struct ResultDoc {
    pub scenario: String,
    pub variant: String,
    pub step: usize,
    pub role: &'static str,
    pub impl_label: String,
    pub peer_impl: String,
    pub ok: bool,
    pub skipped: bool,
    pub crashed: bool,
    pub error_kind: String,
    pub error: String,
    pub checks: Vec<Check>,
    pub sent: u64,
    pub received: u64,
    pub elapsed_ms: u128,
    pub meta: Vec<(String, String)>,
}

/// Set by the first emit: the result line is printed exactly once per process.
static PRINTED: AtomicBool = AtomicBool::new(false);

impl ResultDoc {
    pub fn new(cfg: &Config) -> ResultDoc {
        ResultDoc {
            scenario: cfg.scenario.clone(),
            variant: cfg.variant.clone(),
            step: cfg.step,
            role: cfg.role.name(),
            impl_label: cfg.impl_label.clone(),
            peer_impl: cfg.peer_impl.clone(),
            ok: false,
            skipped: false,
            crashed: false,
            error_kind: String::new(),
            error: String::new(),
            checks: Vec::new(),
            sent: 0,
            received: 0,
            elapsed_ms: 0,
            meta: Vec::new(),
        }
    }

    fn render(&self) -> String {
        let checks: Vec<String> = self
            .checks
            .iter()
            .map(|c| {
                format!(
                    "{{\"name\":{},\"ok\":{},\"detail\":{}}}",
                    quote(&c.name),
                    c.ok,
                    quote(&c.detail)
                )
            })
            .collect();
        let mut line = format!(
            "{{\"scenario\":{},\"variant\":{},\"step\":{},\"role\":{},\"impl\":{},\"peer_impl\":{},\
             \"ok\":{},\"skipped\":{},\"crashed\":{},\"error_kind\":{},\"error\":{},\
             \"checks\":[{}],\"sent\":{},\"received\":{},\"elapsed_ms\":{}",
            quote(&self.scenario),
            quote(&self.variant),
            self.step,
            quote(self.role),
            quote(&self.impl_label),
            quote(&self.peer_impl),
            self.ok,
            self.skipped,
            self.crashed,
            quote(&self.error_kind),
            quote(&self.error),
            checks.join(","),
            self.sent,
            self.received,
            self.elapsed_ms,
        );
        if !self.meta.is_empty() {
            let meta: Vec<String> = self
                .meta
                .iter()
                .map(|(k, v)| format!("{}:{}", quote(k), v))
                .collect();
            line.push_str(&format!(",\"meta\":{{{}}}", meta.join(",")));
        }
        line.push('}');
        line
    }
}

/// Prints the result line to stdout, unless one was already printed. Returns
/// whether this call printed it.
pub fn emit(doc: &ResultDoc) -> bool {
    if PRINTED.swap(true, Ordering::SeqCst) {
        return false;
    }
    let mut out = io::stdout().lock();
    // A closed stdout leaves nothing to report to; the exit code still says the result.
    let _ = writeln!(out, "{}", doc.render());
    let _ = out.flush();
    true
}

/// A JSON string literal.
pub fn quote(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}
