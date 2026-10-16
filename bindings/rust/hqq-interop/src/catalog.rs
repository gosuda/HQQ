//! The catalogue of `hqq-interop list` (INTEROP_CLI.md, section 7). Only the variants
//! this helper implements are listed; their params, steps, args and expectations are
//! the Go reference's, so the driver sees identical variant definitions in both helpers.

use crate::result::quote;

pub struct Step {
    pub roles: &'static [&'static str],
    pub expect: &'static [(&'static str, &'static str)],
    pub args: &'static [(&'static str, &'static [&'static str])],
}

pub struct Variant {
    pub name: &'static str,
    pub params: &'static [(&'static str, i64)],
    pub steps: &'static [Step],
}

pub struct Scenario {
    pub id: &'static str,
    pub title: &'static str,
    pub kind: &'static str,
    pub platforms: &'static [&'static str],
    pub timeout_s: u32,
    pub variants: &'static [Variant],
}

/// Canonical order of the params object (the Go struct's field order).
const PARAM_ORDER: &[&str] = &[
    "buffer_count",
    "buffer_size",
    "size",
    "messages",
    "seed",
    "heartbeat_ms",
    "peer_timeout_ms",
    "attach_timeout_ms",
    "max_message",
    "max_reassembly",
    "queue_size",
];

const BOTH: &[&str] = &["creator", "opener"];
const OPENER: &[&str] = &["opener"];
const OPENER_ARGS: &[(&str, &[&str])] = &[];

/// creator ok, opener ok.
const OK_OK: &[(&str, &str)] = &[("creator", "ok"), ("opener", "ok")];
/// creator crash, opener ok.
const CRASH_OK: &[(&str, &str)] = &[("creator", "crash"), ("opener", "ok")];
/// creator ok, opener crash.
const OK_CRASH: &[(&str, &str)] = &[("creator", "ok"), ("opener", "crash")];

const STEP_OK: &[Step] = &[Step {
    roles: BOTH,
    expect: OK_OK,
    args: OPENER_ARGS,
}];

const ADV_PARAMS: &[(&str, i64)] = &[("buffer_count", 4), ("buffer_size", 256)];
const ADV_SIZES_PARAMS: &[(&str, i64)] = &[
    ("buffer_count", 4),
    ("buffer_size", 256),
    ("max_message", 65536),
];

const X01_STEPS: &[Step] = STEP_OK;

const X02_COUNT_DOUBLE: &[Step] = &[Step {
    roles: BOTH,
    expect: OK_OK,
    args: &[("opener", &["--buffer-count", "8"])],
}];
const X02_SIZE_DOUBLE: &[Step] = &[Step {
    roles: BOTH,
    expect: OK_OK,
    args: &[("opener", &["--buffer-size", "512"])],
}];
const X02_COUNT_HALF: &[Step] = &[Step {
    roles: BOTH,
    expect: OK_OK,
    args: &[("opener", &["--buffer-count", "2"])],
}];
const X02_SIZE_HALF: &[Step] = &[Step {
    roles: BOTH,
    expect: OK_OK,
    args: &[("opener", &["--buffer-size", "128"])],
}];

const X04_DEAD_STEPS: &[Step] = &[
    Step {
        roles: BOTH,
        expect: CRASH_OK,
        args: &[("creator", &["--crash-after", "exit"])],
    },
    Step {
        roles: OPENER,
        expect: &[("opener", "ok")],
        args: OPENER_ARGS,
    },
];

const X07_CREATOR_CRASH: &[Step] = &[Step {
    roles: BOTH,
    expect: CRASH_OK,
    args: &[("creator", &["--crash-after", "msgs:50"])],
}];
const X07_OPENER_CRASH: &[Step] = &[Step {
    roles: BOTH,
    expect: OK_CRASH,
    args: &[("opener", &["--crash-after", "blocked"])],
}];

const X14_SPIN_STEPS: &[Step] = &[Step {
    roles: BOTH,
    expect: OK_OK,
    args: &[("opener", &["--wait", "spin"])],
}];

const X01_VARIANTS: &[Variant] = &[
    Variant {
        name: "creator-closes-first",
        params: &[("buffer_size", 256), ("messages", 1)],
        steps: X01_STEPS,
    },
    Variant {
        name: "opener-closes-first",
        params: &[("buffer_size", 256), ("messages", 1)],
        steps: X01_STEPS,
    },
];

const X02_VARIANTS: &[Variant] = &[
    Variant {
        name: "count-double",
        params: ADV_PARAMS,
        steps: X02_COUNT_DOUBLE,
    },
    Variant {
        name: "size-double",
        params: ADV_PARAMS,
        steps: X02_SIZE_DOUBLE,
    },
    Variant {
        name: "count-half",
        params: ADV_PARAMS,
        steps: X02_COUNT_HALF,
    },
    Variant {
        name: "size-half",
        params: ADV_PARAMS,
        steps: X02_SIZE_HALF,
    },
];

const X03_VARIANTS: &[Variant] = &[Variant {
    name: "default",
    params: ADV_PARAMS,
    steps: STEP_OK,
}];

const X04_VARIANTS: &[Variant] = &[
    Variant {
        name: "before-creator",
        params: &[
            ("buffer_count", 4),
            ("buffer_size", 256),
            ("attach_timeout_ms", 300),
        ],
        steps: STEP_OK,
    },
    Variant {
        name: "dead",
        params: &[
            ("buffer_count", 4),
            ("buffer_size", 256),
            ("heartbeat_ms", 50),
            ("peer_timeout_ms", 200),
        ],
        steps: X04_DEAD_STEPS,
    },
];

const X05_VARIANTS: &[Variant] = &[Variant {
    name: "ring2",
    params: &[("buffer_count", 2), ("buffer_size", 256)],
    steps: STEP_OK,
}];

const X06_VARIANTS: &[Variant] = &[
    Variant {
        name: "ring2",
        params: &[
            ("buffer_count", 2),
            ("buffer_size", 256),
            ("messages", 20000),
        ],
        steps: STEP_OK,
    },
    Variant {
        name: "ring256",
        params: &[
            ("buffer_count", 256),
            ("buffer_size", 256),
            ("messages", 20000),
        ],
        steps: STEP_OK,
    },
];

const X07_VARIANTS: &[Variant] = &[
    Variant {
        name: "creator-killed-running",
        params: &[("buffer_count", 4), ("buffer_size", 256), ("messages", 50)],
        steps: X07_CREATOR_CRASH,
    },
    Variant {
        name: "opener-killed-parked",
        params: &[("buffer_count", 4), ("buffer_size", 256), ("messages", 50)],
        steps: X07_OPENER_CRASH,
    },
    Variant {
        name: "short-peer-timeout",
        params: &[
            ("buffer_count", 4),
            ("buffer_size", 256),
            ("messages", 50),
            ("heartbeat_ms", 50),
            ("peer_timeout_ms", 300),
        ],
        steps: X07_CREATOR_CRASH,
    },
];

const X12_VARIANTS: &[Variant] = &[Variant {
    name: "ring2",
    params: &[("buffer_count", 2), ("buffer_size", 256), ("messages", 20)],
    steps: STEP_OK,
}];

const X14_VARIANTS: &[Variant] = &[
    Variant {
        name: "adaptive",
        params: &[("buffer_count", 4), ("buffer_size", 256), ("messages", 5)],
        steps: STEP_OK,
    },
    Variant {
        name: "spin-peer",
        params: &[("buffer_count", 4), ("buffer_size", 256), ("messages", 5)],
        steps: X14_SPIN_STEPS,
    },
];

const X_A01_VARIANTS: &[Variant] = &[
    Variant {
        name: "all-features",
        params: ADV_PARAMS,
        steps: STEP_OK,
    },
    Variant {
        name: "opener-initiates",
        params: ADV_PARAMS,
        steps: STEP_OK,
    },
    Variant {
        name: "simultaneous",
        params: ADV_PARAMS,
        steps: STEP_OK,
    },
    Variant {
        name: "messages-only",
        params: ADV_PARAMS,
        steps: STEP_OK,
    },
    Variant {
        name: "request-response-only",
        params: ADV_PARAMS,
        steps: STEP_OK,
    },
    Variant {
        name: "none",
        params: ADV_PARAMS,
        steps: STEP_OK,
    },
];

const X_A02_VARIANTS: &[Variant] = &[Variant {
    name: "sizes",
    params: ADV_SIZES_PARAMS,
    steps: STEP_OK,
}];

const X_A04_VARIANTS: &[Variant] = &[Variant {
    name: "default",
    params: ADV_PARAMS,
    steps: STEP_OK,
}];

const X_A05_VARIANTS: &[Variant] = &[Variant {
    name: "basic",
    params: ADV_PARAMS,
    steps: STEP_OK,
}];

const X_A08_VARIANTS: &[Variant] = &[
    Variant {
        name: "metadata",
        params: ADV_PARAMS,
        steps: STEP_OK,
    },
    Variant {
        name: "without-metadata",
        params: ADV_PARAMS,
        steps: STEP_OK,
    },
];

const X_A09_VARIANTS: &[Variant] = &[Variant {
    name: "default",
    params: ADV_PARAMS,
    steps: STEP_OK,
}];

const X_A10_VARIANTS: &[Variant] = &[Variant {
    name: "default",
    params: ADV_PARAMS,
    steps: STEP_OK,
}];

/// Scenarios that `run` implements but `list` does not advertise: their variant or
/// step sets differ from the Go helper's (X-04 initializing, X-A01 op-error and
/// bad-major, X-A02 sender-abort, X-A05 bad-parity and duplicate are not implemented
/// either). The driver fails a pair when a listed scenario's sets differ from Go's.
const NOT_LISTED: &[&str] = &["X-04", "X-A01", "X-A02", "X-A05"];

const ANY: &[&str] = &["any"];

/// The implemented scenarios, in ID order (the Go list's order).
const SCENARIOS: &[Scenario] = &[
    Scenario {
        id: "X-01",
        title: "create, open, exchange, close",
        kind: "standard",
        platforms: ANY,
        timeout_s: 30,
        variants: X01_VARIANTS,
    },
    Scenario {
        id: "X-02",
        title: "geometry mismatch (count, size)",
        kind: "standard",
        platforms: ANY,
        timeout_s: 30,
        variants: X02_VARIANTS,
    },
    Scenario {
        id: "X-03",
        title: "third endpoint, re-join after own close, creator vs live segment",
        kind: "standard",
        platforms: ANY,
        timeout_s: 30,
        variants: X03_VARIANTS,
    },
    Scenario {
        id: "X-04",
        title: "opener before creator, opener on an initializing or dead segment",
        kind: "standard",
        platforms: ANY,
        timeout_s: 30,
        variants: X04_VARIANTS,
    },
    Scenario {
        id: "X-05",
        title: "ring full / backpressure, deadline on a full ring",
        kind: "standard",
        platforms: ANY,
        timeout_s: 30,
        variants: X05_VARIANTS,
    },
    Scenario {
        id: "X-06",
        title: "bidirectional stream, sequence and CRC, ring of 2 and 256",
        kind: "standard",
        platforms: ANY,
        timeout_s: 120,
        variants: X06_VARIANTS,
    },
    Scenario {
        id: "X-07",
        title: "kill -9 of the peer (running and parked), default and short peer timeout",
        kind: "standard",
        platforms: ANY,
        timeout_s: 60,
        variants: X07_VARIANTS,
    },
    Scenario {
        id: "X-12",
        title: "parked consumer woken by a producer (ping-pong, ring of 2)",
        kind: "standard",
        platforms: ANY,
        timeout_s: 60,
        variants: X12_VARIANTS,
    },
    Scenario {
        id: "X-14",
        title: "simple-mode peer: round trips with 300 ms idle",
        kind: "standard",
        platforms: ANY,
        timeout_s: 60,
        variants: X14_VARIANTS,
    },
    Scenario {
        id: "X-A01",
        title: "negotiation (X-15, SPEC 10.1)",
        kind: "advanced",
        platforms: ANY,
        timeout_s: 30,
        variants: X_A01_VARIANTS,
    },
    Scenario {
        id: "X-A02",
        title: "message sizes (X-16, SPEC 10.4-10.5)",
        kind: "advanced",
        platforms: ANY,
        timeout_s: 30,
        variants: X_A02_VARIANTS,
    },
    Scenario {
        id: "X-A04",
        title: "request/response (X-18, SPEC 10.6)",
        kind: "advanced",
        platforms: ANY,
        timeout_s: 30,
        variants: X_A04_VARIANTS,
    },
    Scenario {
        id: "X-A05",
        title: "streams (X-19, SPEC 10.2-10.3)",
        kind: "advanced",
        platforms: ANY,
        timeout_s: 30,
        variants: X_A05_VARIANTS,
    },
    Scenario {
        id: "X-A08",
        title: "request metadata (1.1 FeatureMetadata, PROTOCOL_1_1 section 3)",
        kind: "advanced",
        platforms: ANY,
        timeout_s: 30,
        variants: X_A08_VARIANTS,
    },
    Scenario {
        id: "X-A09",
        title: "graceful shutdown (1.1 section 6)",
        kind: "advanced",
        platforms: ANY,
        timeout_s: 30,
        variants: X_A09_VARIANTS,
    },
    Scenario {
        id: "X-A10",
        title: "acceptor (LISTENER_DRAFT, Listen/Dial/Connect)",
        kind: "acceptor",
        platforms: ANY,
        timeout_s: 30,
        variants: X_A10_VARIANTS,
    },
];

pub fn scenario(id: &str) -> Option<&'static Scenario> {
    SCENARIOS.iter().find(|s| s.id == id)
}

/// The variant named `name`; the empty name selects the first variant.
pub fn variant(sc: &'static Scenario, name: &str) -> Option<&'static Variant> {
    if name.is_empty() {
        return sc.variants.first();
    }
    sc.variants.iter().find(|v| v.name == name)
}

fn json_list(items: &[&str]) -> String {
    let parts: Vec<String> = items.iter().map(|s| quote(s)).collect();
    format!("[{}]", parts.join(","))
}

fn json_params(params: &[(&str, i64)]) -> String {
    let parts: Vec<String> = PARAM_ORDER
        .iter()
        .filter_map(|key| {
            params
                .iter()
                .find(|(k, v)| k == key && *v != 0)
                .map(|(k, v)| format!("{}:{}", quote(k), v))
        })
        .collect();
    format!("{{{}}}", parts.join(","))
}

fn json_step(step: &Step) -> String {
    let expect: Vec<String> = step
        .expect
        .iter()
        .map(|(role, what)| format!("{}:{}", quote(role), quote(what)))
        .collect();
    let mut out = format!(
        "{{\"roles\":{},\"expect\":{{{}}}",
        json_list(step.roles),
        expect.join(",")
    );
    if !step.args.is_empty() {
        let args: Vec<String> = step
            .args
            .iter()
            .map(|(role, flags)| format!("{}:{}", quote(role), json_list(flags)))
            .collect();
        out.push_str(&format!(",\"args\":{{{}}}", args.join(",")));
    }
    out.push('}');
    out
}

fn json_variant(v: &Variant) -> String {
    let steps: Vec<String> = v.steps.iter().map(json_step).collect();
    format!(
        "{{\"name\":{},\"params\":{},\"steps\":[{}]}}",
        quote(v.name),
        json_params(v.params),
        steps.join(",")
    )
}

/// The `list` output for the helper labelled `impl_label`.
pub fn list_json(impl_label: &str) -> String {
    let scenarios: Vec<String> = SCENARIOS
        .iter()
        .filter(|s| !NOT_LISTED.contains(&s.id))
        .map(|s| {
            let variants: Vec<String> = s.variants.iter().map(json_variant).collect();
            format!(
                "{{\"id\":{},\"title\":{},\"kind\":{},\"platforms\":{},\"timeout_s\":{},\"variants\":[{}]}}",
                quote(s.id),
                quote(s.title),
                quote(s.kind),
                json_list(s.platforms),
                s.timeout_s,
                variants.join(",")
            )
        })
        .collect();
    format!(
        "{{\"helper\":\"hqq-interop\",\"impl\":{},\"protocol\":\"1.1\",\"layout\":\"HQQ_STL3\",\"runtime\":\"rust\",\"scenarios\":[{}]}}",
        quote(impl_label),
        scenarios.join(",")
    )
}
