//! hqq-interop: the Zig helper of conformance/INTEROP_CLI.md (UNVERIFIED: not
//! compiled or run; Zig 0.15.x target).
//!
//! Implements: `version`, `list [--impl LABEL]`, `run [flags]`.
//! Not implemented (not listed, `run` exits 2 for them): gen-vectors.
//! Scenarios listed (ID: variants), mirroring cmd/hqq-interop/*.go:
//!   X-01: creator-closes-first, opener-closes-first      (std_lifecycle.go)
//!   X-06: ring2, ring256                                 (std_flow.go)
//!   X-A01 and X-A05 are run-only (not listed): the driver needs the Go variant set.
//!   Not implemented: X-A01 variants `opener-initiates`, `simultaneous`, `messages-only`,
//!   `request-response-only`, `bad-major`, `op-error`; X-A05 `bad-parity`, `duplicate`;
//!   X-07, X-08 (crash points: `--crash-after` exits 4); all other scenarios.
const std = @import("std");
const hqq = @import("hqq");

const impl_default = "zig";
const protocol = "1.1";
const layout = "HQQ_STL3";
const ring_layout = 2;
const exit_ok: u8 = 0;
const exit_failed: u8 = 1;
const exit_usage: u8 = 2;
const exit_timeout: u8 = 3;
const exit_skipped: u8 = 4;
const exit_internal: u8 = 5;

// ---------------------------------------------------------------------------
// Deterministic payloads (INTEROP_CLI section 5)
// ---------------------------------------------------------------------------

fn splitmixNext(state: *u64) u64 {
    state.* +%= 0x9E3779B97F4A7C15;
    var z = state.*;
    z = (z ^ (z >> 30)) *% 0xBF58476D1CE4E5B9;
    z = (z ^ (z >> 27)) *% 0x94D049BB133111EB;
    return z ^ (z >> 31);
}

/// body(out.len): the stream of (seed, seq), words written little-endian.
fn fillBody(seed: u64, seq: u64, out: []u8) void {
    var state = seed ^ (seq *% 0xD1B54A32D192ED03);
    var word: u64 = 0;
    var left: usize = 0;
    for (out) |*b| {
        if (left == 0) {
            word = splitmixNext(&state);
            left = 8;
        }
        b.* = @truncate(word);
        word >>= 8;
        left -= 1;
    }
}

const crc_table: [256]u32 = blk: {
    @setEvalBranchQuota(20000);
    var t: [256]u32 = undefined;
    for (0..256) |i| {
        var v: u32 = @intCast(i);
        for (0..8) |_| {
            v = if ((v & 1) != 0) 0xEDB88320 ^ (v >> 1) else v >> 1;
        }
        t[i] = v;
    }
    break :blk t;
};

/// crc32-IEEE (zlib polynomial, reflected).
fn crc32(data: []const u8) u32 {
    var crc: u32 = 0xFFFFFFFF;
    for (data) |b| crc = crc_table[(crc ^ b) & 0xFF] ^ (crc >> 8);
    return ~crc;
}

/// Payload(seed, seq, out.len) written into `out`.
fn fillPayload(seed: u64, seq: u64, out: []u8) void {
    if (out.len == 0) return;
    if (out.len < 16) {
        fillBody(seed, seq, out);
        return;
    }
    std.mem.writeInt(u64, out[0..8], seq, .little);
    fillBody(seed, seq, out[12..]);
    std.mem.writeInt(u32, out[8..12], crc32(out[12..]), .little);
}

fn matchesHex(got: []const u8, hex: []const u8) bool {
    var want: [64]u8 = undefined;
    const w = std.fmt.hexToBytes(&want, hex) catch return false;
    return std.mem.eql(u8, got, w);
}

/// Golden vectors of INTEROP_CLI section 5, checked before any scenario runs.
fn goldenOk() bool {
    var b1: [1]u8 = undefined;
    fillPayload(1, 0, &b1);
    if (!matchesHex(&b1, "c1")) return false;
    var b8: [8]u8 = undefined;
    fillPayload(1, 0, &b8);
    if (!matchesHex(&b8, "c15c0289ec2d0a91")) return false;
    var b16: [16]u8 = undefined;
    fillPayload(1, 0, &b16);
    if (!matchesHex(&b16, "0000000000000000add8412cc15c0289")) return false;
    var b40: [40]u8 = undefined;
    fillPayload(0xdeadbeef, 7, &b40);
    return matchesHex(&b40, "0700000000000000" ++ "47a0b112" ++
        "7536cc3826b6b2d2c8375c13ebdfc0ab9ca7d35f20744cb4782227b3");
}

// ---------------------------------------------------------------------------
// Catalogue
// ---------------------------------------------------------------------------

const Params = struct {
    buffer_count: u64 = 0,
    buffer_size: u64 = 0,
    messages: u64 = 0,
};

const Variant = struct { name: []const u8, params: Params };

const Scenario = struct {
    id: []const u8,
    title: []const u8,
    kind: []const u8,
    timeout_s: u64,
    variants: []const Variant,
};

const adv_base = Params{ .buffer_count = 4, .buffer_size = 256 };

const catalogue = [_]Scenario{
    .{ .id = "X-01", .title = "create, open, exchange, close", .kind = "standard", .timeout_s = 30, .variants = &.{
        .{ .name = "creator-closes-first", .params = .{ .buffer_size = 256, .messages = 1 } },
        .{ .name = "opener-closes-first", .params = .{ .buffer_size = 256, .messages = 1 } },
    } },
    .{ .id = "X-06", .title = "bidirectional stream, sequence and CRC, ring of 2 and 256", .kind = "standard", .timeout_s = 120, .variants = &.{
        .{ .name = "ring2", .params = .{ .buffer_count = 2, .buffer_size = 256, .messages = 20000 } },
        .{ .name = "ring256", .params = .{ .buffer_count = 256, .buffer_size = 256, .messages = 20000 } },
    } },
    .{ .id = "X-12", .title = "parked consumer woken by a producer (ping-pong, ring of 2)", .kind = "standard", .timeout_s = 60, .variants = &.{
        .{ .name = "ring2", .params = .{ .buffer_count = 2, .buffer_size = 256, .messages = 20 } },
    } },
    .{ .id = "X-A04", .title = "request/response (X-18, SPEC 10.6): concurrent calls, error payload, cancelled call, unknown method", .kind = "advanced", .timeout_s = 30, .variants = &.{
        .{ .name = "default", .params = adv_base },
    } },
};

// Run-only scenarios: implemented and checked by hand, not listed. The driver runs a
// listed scenario only when its variant and step set equal the Go helper's, and these
// are partial variant sets.
const run_only = [_]Scenario{
    .{ .id = "X-A01", .title = "negotiation (X-15, SPEC 10.1): all features, and the empty feature set", .kind = "advanced", .timeout_s = 30, .variants = &.{
        .{ .name = "all-features", .params = adv_base },
        .{ .name = "none", .params = adv_base },
    } },
    .{ .id = "X-A05", .title = "streams (X-19, SPEC 10.2-10.3): basic", .kind = "advanced", .timeout_s = 30, .variants = &.{
        .{ .name = "basic", .params = adv_base },
    } },
};

fn findScenario(id: []const u8) ?Scenario {
    for (catalogue) |s| {
        if (std.mem.eql(u8, s.id, id)) return s;
    }
    for (run_only) |s| {
        if (std.mem.eql(u8, s.id, id)) return s;
    }
    return null;
}

// ---------------------------------------------------------------------------
// Configuration (INTEROP_CLI section 2)
// ---------------------------------------------------------------------------

const Role = enum { creator, opener };

const Config = struct {
    scenario: []const u8 = "",
    variant: []const u8 = "",
    step: u64 = 1,
    role: Role = .creator,
    name: []const u8 = "",
    sync_dir: []const u8 = "",
    impl: []const u8 = impl_default,
    peer_impl: []const u8 = "",
    buffer_count: ?u64 = null,
    buffer_size: ?u64 = null,
    size: ?u64 = null,
    messages: ?u64 = null,
    seed: u64 = 1,
    timeout_s: f64 = 20,
    crash_after: []const u8 = "",
    heartbeat_ms: i64 = 0,
    peer_timeout_ms: i64 = 0,
    attach_timeout_ms: i64 = 0,
    spin: bool = false,
    long: bool = false,
    max_message: u64 = 0,
    max_reassembly: u64 = 0,
    queue_size: u64 = 0,
};

const UsageError = error{Usage};

fn usage(msg: []const u8) UsageError {
    std.debug.print("hqq-interop: {s}\n", .{msg});
    return error.Usage;
}

fn setFlag(cfg: *Config, key: []const u8, v: []const u8) !void {
    const eql = std.mem.eql;
    if (eql(u8, key, "scenario")) {
        cfg.scenario = v;
    } else if (eql(u8, key, "variant")) {
        cfg.variant = v;
    } else if (eql(u8, key, "step")) {
        cfg.step = try std.fmt.parseInt(u64, v, 10);
    } else if (eql(u8, key, "role")) {
        if (eql(u8, v, "creator") or eql(u8, v, "a")) {
            cfg.role = .creator;
        } else if (eql(u8, v, "opener") or eql(u8, v, "b")) {
            cfg.role = .opener;
        } else return usage("--role must be creator|opener|a|b");
    } else if (eql(u8, key, "name")) {
        cfg.name = v;
    } else if (eql(u8, key, "sync-dir")) {
        cfg.sync_dir = v;
    } else if (eql(u8, key, "impl")) {
        cfg.impl = v;
    } else if (eql(u8, key, "peer-impl")) {
        cfg.peer_impl = v;
    } else if (eql(u8, key, "buffer-count")) {
        cfg.buffer_count = try std.fmt.parseInt(u64, v, 10);
    } else if (eql(u8, key, "buffer-size")) {
        cfg.buffer_size = try std.fmt.parseInt(u64, v, 10);
    } else if (eql(u8, key, "size")) {
        cfg.size = try std.fmt.parseInt(u64, v, 10);
    } else if (eql(u8, key, "messages")) {
        cfg.messages = try std.fmt.parseInt(u64, v, 10);
    } else if (eql(u8, key, "seed")) {
        cfg.seed = try std.fmt.parseInt(u64, v, 10);
    } else if (eql(u8, key, "timeout")) {
        cfg.timeout_s = try std.fmt.parseFloat(f64, v);
    } else if (eql(u8, key, "crash-after")) {
        cfg.crash_after = v;
    } else if (eql(u8, key, "heartbeat")) {
        cfg.heartbeat_ms = try std.fmt.parseInt(i64, v, 10);
    } else if (eql(u8, key, "peer-timeout")) {
        cfg.peer_timeout_ms = try std.fmt.parseInt(i64, v, 10);
    } else if (eql(u8, key, "attach-timeout")) {
        cfg.attach_timeout_ms = try std.fmt.parseInt(i64, v, 10);
    } else if (eql(u8, key, "wait")) {
        if (eql(u8, v, "spin")) {
            cfg.spin = true;
        } else if (eql(u8, v, "adaptive")) {
            cfg.spin = false;
        } else return usage("--wait must be adaptive|spin");
    } else if (eql(u8, key, "max-message")) {
        cfg.max_message = try std.fmt.parseInt(u64, v, 10);
    } else if (eql(u8, key, "max-reassembly")) {
        cfg.max_reassembly = try std.fmt.parseInt(u64, v, 10);
    } else if (eql(u8, key, "queue-size")) {
        cfg.queue_size = try std.fmt.parseInt(u64, v, 10);
    } else {
        return usage("unknown flag");
    }
}

/// Parses `run` flags. Long flags: `--name value` or `--name=value`.
fn parseRunFlags(args: []const [:0]u8, cfg: *Config) !void {
    var i: usize = 0;
    while (i < args.len) : (i += 1) {
        const arg: []const u8 = args[i];
        if (!std.mem.startsWith(u8, arg, "--")) return usage("unexpected argument");
        var key = arg[2..];
        var val: ?[]const u8 = null;
        if (std.mem.indexOfScalar(u8, key, '=')) |eq| {
            val = key[eq + 1 ..];
            key = key[0..eq];
        }
        if (std.mem.eql(u8, key, "long")) {
            cfg.long = true;
            continue;
        }
        if (val == null) {
            i += 1;
            if (i >= args.len) return usage("missing value for flag");
            val = args[i];
        }
        try setFlag(cfg, key, val.?);
    }
}

// ---------------------------------------------------------------------------
// JSON output (fixed buffer; field order as INTEROP_CLI section 4)
// ---------------------------------------------------------------------------

const Json = struct {
    buf: []u8,
    len: usize = 0,

    fn raw(self: *Json, s: []const u8) !void {
        if (self.len + s.len > self.buf.len) return error.NoSpaceLeft;
        @memcpy(self.buf[self.len..][0..s.len], s);
        self.len += s.len;
    }

    fn fmt(self: *Json, comptime f: []const u8, args: anytype) !void {
        const s = try std.fmt.bufPrint(self.buf[self.len..], f, args);
        self.len += s.len;
    }

    fn str(self: *Json, s: []const u8) !void {
        try self.raw("\"");
        for (s) |ch| switch (ch) {
            '"' => try self.raw("\\\""),
            '\\' => try self.raw("\\\\"),
            0...0x1f => try self.fmt("\\u{x:0>4}", .{ch}),
            else => try self.raw(&.{ch}),
        };
        try self.raw("\"");
    }

    fn field(self: *Json, key: []const u8) !void {
        try self.str(key);
        try self.raw(":");
    }
};

const Check = struct { name: []const u8, ok: bool, detail: []const u8 };

fn streq(a: []const u8, b: []const u8) bool {
    return std.mem.eql(u8, a, b);
}

// ---------------------------------------------------------------------------
// Run state
// ---------------------------------------------------------------------------

const max_checks = 64;

const Env = struct {
    cfg: *const Config,
    gpa: std.mem.Allocator,
    start: i128,
    deadline: i128,
    bc: u64,
    bs: u64,
    size: u64,
    messages: u64,
    checks: [max_checks]Check = undefined,
    nchecks: usize = 0,
    failed: bool = false,
    sent: u64 = 0,
    received: u64 = 0,
    meta_status: ?u64 = null,
    worst_rtt_ms: ?i64 = null,

    /// Time left until the process deadline, in ns (0 when passed).
    fn remaining(self: *const Env) i64 {
        const left = self.deadline - std.time.nanoTimestamp();
        if (left <= 0) return 0;
        return @intCast(@min(left, std.math.maxInt(i64)));
    }

    /// Wait bound of a blocking call: the rest of the process deadline.
    fn wait(self: *const Env) hqq.Wait {
        return .{ .timeout_ns = self.remaining() };
    }

    fn check(self: *Env, name: []const u8, ok: bool, detail: []const u8) void {
        if (self.nchecks < max_checks) {
            self.checks[self.nchecks] = .{ .name = name, .ok = ok, .detail = detail };
            self.nchecks += 1;
        }
        if (!ok) self.failed = true;
    }

    /// Records that `result` failed with an error matching `want` (errors.Is).
    fn expectErr(self: *Env, name: []const u8, result: anytype, want: hqq.Error) void {
        if (result) |_| {
            self.check(name, false, "no error");
        } else |err| {
            self.check(name, hqq.errorIs(err, want), kindName(err));
        }
    }

    /// Checks that `got` equals Payload(seed, seq, got.len).
    fn verifySeq(self: *Env, seed: u64, seq: u64, got: []const u8) !bool {
        const exp = try self.gpa.alloc(u8, got.len);
        fillPayload(seed, seq, exp);
        return std.mem.eql(u8, exp, got);
    }

    fn syncPath(self: *const Env, phase: []const u8, role: []const u8) ![]u8 {
        return std.fmt.allocPrint(self.gpa, "{s}/s{d}-{s}.{s}", .{ self.cfg.sync_dir, self.cfg.step, phase, role });
    }

    /// INTEROP_CLI section 6: create my marker, poll for the peer's (2 ms).
    fn barrier(self: *Env, phase: []const u8) !void {
        const mine_role = if (self.cfg.role == .creator) "creator" else "opener";
        const peer_role = if (self.cfg.role == .creator) "opener" else "creator";
        const mine = try self.syncPath(phase, mine_role);
        (try std.fs.cwd().createFile(mine, .{})).close();
        const peer = try self.syncPath(phase, peer_role);
        while (true) {
            std.fs.cwd().access(peer, .{}) catch {
                if (self.remaining() <= 0) return error.Timeout;
                std.Thread.sleep(2 * std.time.ns_per_ms);
                continue;
            };
            return;
        }
    }
};

/// error_kind of INTEROP_CLI section 4.1 for an error (most specific name).
fn kindName(err: anyerror) []const u8 {
    return switch (err) {
        error.Closed => "ErrClosed",
        error.PeerClosed => "ErrPeerClosed",
        error.PeerGone => "ErrPeerGone",
        error.Timeout => "ErrTimeout",
        error.LinkBusy => "ErrLinkBusy",
        error.IncompatibleLayout => "ErrIncompatibleLayout",
        error.FailedInit, error.SegmentNotReady, error.InitContended, error.NoLiveEndpoint, error.ListenerNotReady => "ErrFailedInit",
        error.InvalidSize, error.GeometryMismatch => "ErrInvalidSize",
        error.MemoryAlign => "ErrMemoryAlign",
        error.MemorySmall => "ErrMemorySmall",
        error.BufferOverflow => "ErrBufferOverflow",
        error.ConnNotFound => "ErrConnNotFound",
        error.InvalidOp => "ErrInvalidOp",
        error.UnexpectedPacket => "ErrUnexpectedPacket",
        error.Protocol => "ErrProtocol",
        error.InvalidName => "ErrInvalidName",
        error.NotExist => "ErrNotExist",
        error.NotNegotiated => "ErrNotNegotiated",
        error.AlreadyNegotiated => "ErrAlreadyNegotiated",
        error.NegotiationFailed => "ErrNegotiationFailed",
        error.NegotiationRejected => "ErrNegotiationRejected",
        error.MessageAborted => "ErrMessageAborted",
        error.IncomingOverflow => "ErrIncomingOverflow",
        error.ReassemblyLimit => "ErrReassemblyLimit",
        error.ShuttingDown => "ErrShuttingDown",
        error.Response => "ResponseError",
        error.NotListening => "ErrNotListening",
        error.AlreadyListening => "ErrAlreadyListening",
        error.ListenerBusy => "ErrListenerBusy",
        error.ListenerGone => "ErrListenerGone",
        error.ListenerClosed => "ErrListenerClosed",
        error.AcceptorClosed => "ErrAcceptorClosed",
        else => "Unknown",
    };
}

fn stdOptions(cfg: *const Config) hqq.Options {
    var o = hqq.Options.init();
    if (cfg.heartbeat_ms != 0) o.raw.heartbeat_interval_ns = cfg.heartbeat_ms * std.time.ns_per_ms;
    if (cfg.peer_timeout_ms > 0) o.raw.peer_timeout_ns = cfg.peer_timeout_ms * std.time.ns_per_ms;
    o.raw.attach_timeout_ns = cfg.attach_timeout_ms * std.time.ns_per_ms;
    if (cfg.spin) o.raw.wait_policy = hqq.c.HQQ_WAIT_SPIN;
    return o;
}

fn advOptions(cfg: *const Config) hqq.AdvOptions {
    var o = hqq.AdvOptions.init();
    o.raw.max_message_size = cfg.max_message;
    o.raw.max_reassembly_bytes = cfg.max_reassembly;
    o.raw.queue_size = cfg.queue_size;
    if (cfg.heartbeat_ms != 0) o.raw.link.heartbeat_interval_ns = cfg.heartbeat_ms * std.time.ns_per_ms;
    if (cfg.peer_timeout_ms > 0) o.raw.link.peer_timeout_ns = cfg.peer_timeout_ms * std.time.ns_per_ms;
    o.raw.link.attach_timeout_ns = cfg.attach_timeout_ms * std.time.ns_per_ms;
    if (cfg.spin) o.raw.link.wait_policy = hqq.c.HQQ_WAIT_SPIN;
    return o;
}

/// Standard endpoint setup: the creator creates and posts "created"; the
/// opener waits for it and opens (INTEROP_CLI section 6).
fn openStd(env: *Env) !hqq.Link {
    const opts = stdOptions(env.cfg);
    if (env.cfg.role == .creator) {
        var l = try hqq.Link.create(try nameZ(env), env.bc, env.bs, 0o600, &opts);
        errdefer l.deinit();
        try env.barrier("created");
        return l;
    }
    try env.barrier("created");
    return hqq.Link.open(try nameZ(env), env.bc, env.bs, &opts);
}

/// Advanced endpoint setup (Go advEstablish): create/open, both meet at "linked".
fn openAdv(env: *Env) !hqq.AdvLink {
    const opts = advOptions(env.cfg);
    if (env.cfg.role == .creator) {
        var l = try hqq.AdvLink.create(try nameZ(env), env.bc, env.bs, 0o600, &opts);
        errdefer l.deinit();
        try env.barrier("linked");
        return l;
    }
    try env.barrier("linked");
    return hqq.AdvLink.open(try nameZ(env), env.bc, env.bs, &opts);
}

// ---------------------------------------------------------------------------
// X-01: create, open, exchange, close (std_lifecycle.go)
// ---------------------------------------------------------------------------

fn nameZ(env: *Env) ![:0]u8 {
    return env.gpa.dupeZ(u8, env.cfg.name);
}

fn probeName(env: *Env, name: []const u8, want: hqq.Error) !void {
    var probe = hqq.Link.open(try nameZ(env), env.bc, env.bs, null);
    if (probe) |*p| p.deinit() else |_| {}
    env.expectErr(name, probe, want);
}

fn runX01(env: *Env) !void {
    const creator_first = streq(env.cfg.variant, "creator-closes-first");
    const is_creator = env.cfg.role == .creator;
    const seed = env.cfg.seed;
    var link = try openStd(env);
    defer link.deinit();

    const buf = try env.gpa.alloc(u8, env.bs);
    const payload = try env.gpa.alloc(u8, env.size);
    if (is_creator) {
        fillPayload(seed, 0, payload);
        try link.write(payload, env.wait());
        env.sent += 1;
        const n = try link.read(buf, env.wait());
        env.received += 1;
        env.check("creator receives the reply intact", try env.verifySeq(seed + 1, 0, buf[0..n]), "");
    } else {
        const n = try link.read(buf, env.wait());
        env.received += 1;
        env.check("opener receives the message intact", try env.verifySeq(seed, 0, buf[0..n]), "");
        fillPayload(seed + 1, 0, payload);
        try link.write(payload, env.wait());
        env.sent += 1;
    }
    try env.barrier("replied");

    if (is_creator == creator_first) {
        // This side closes first.
        try link.close();
        if (is_creator) {
            try probeName(env, "creator close removes the name", error.NotExist);
        } else {
            try probeName(env, "opener close leaves the name in place", error.LinkBusy);
        }
        return env.barrier("closed");
    }

    // The peer closes first: its close is seen as end of stream after the buffered data.
    const r = link.read(buf, env.wait());
    env.expectErr("peer close is seen as io.EOF after buffered data", r, error.PeerClosed);
    try env.barrier("closed");
    if (is_creator) {
        try link.close();
        try probeName(env, "creator close removes the name", error.NotExist);
    }
}

// ---------------------------------------------------------------------------
// X-06 and X-12: streams and ping-pong (std_flow.go)
// ---------------------------------------------------------------------------

const WriterJob = struct {
    link: hqq.Link,
    seed: u64,
    size: usize,
    n: u64,
    wait: hqq.Wait,
    sent: u64 = 0,
    err: ?anyerror = null,
};

fn writerMain(job: *WriterJob) void {
    const out = std.heap.c_allocator.alloc(u8, job.size) catch {
        job.err = error.OutOfMemory;
        return;
    };
    defer std.heap.c_allocator.free(out);
    var i: u64 = 0;
    while (i < job.n) : (i += 1) {
        fillPayload(job.seed, i, out);
        job.link.write(out, job.wait) catch |err| {
            job.err = err;
            return;
        };
        job.sent += 1;
    }
}

/// X-06: each side writes n messages while it reads n from the peer. The
/// creator's stream uses seed, the opener's seed+1.
fn runX06(env: *Env) !void {
    const n = if (env.cfg.long) @as(u64, 100000) else env.messages;
    var link = try openStd(env);
    defer link.deinit();

    const out_seed = if (env.cfg.role == .creator) env.cfg.seed else env.cfg.seed + 1;
    const in_seed = if (env.cfg.role == .creator) env.cfg.seed + 1 else env.cfg.seed;
    var job = WriterJob{ .link = link, .seed = out_seed, .size = @intCast(env.size), .n = n, .wait = env.wait() };
    var writer = try std.Thread.spawn(.{}, writerMain, .{&job});

    const buf = try env.gpa.alloc(u8, env.bs);
    var got: u64 = 0;
    var intact = true;
    var read_err: ?anyerror = null;
    var i: u64 = 0;
    while (i < n) : (i += 1) {
        const len = link.read(buf, env.wait()) catch |err| {
            read_err = err;
            break;
        };
        if (!try env.verifySeq(in_seed, i, buf[0..len])) {
            intact = false;
            break;
        }
        got += 1;
    }
    writer.join();
    env.sent = job.sent;
    env.received = got;
    if (job.err) |err| return err;
    if (read_err) |err| return err;
    env.check("both streams intact and in order", intact and got == n, "");
    try env.barrier("done");
}

/// Ping-pong: the creator sleeps `idle`, sends Payload(seed, i) and waits for
/// the echo; the opener echoes every message. The creator checks the worst
/// round trip against `max_rtt_ms`.
fn pingPong(env: *Env, link: *hqq.Link, n: u64, idle_ms: u64, max_rtt_ms: i64) !void {
    const seed = env.cfg.seed;
    const out = try env.gpa.alloc(u8, env.size);
    const buf = try env.gpa.alloc(u8, env.bs);
    var worst_ms: i64 = 0;
    var intact = true;
    var i: u64 = 0;
    while (i < n) : (i += 1) {
        if (env.cfg.role == .creator) {
            std.Thread.sleep(idle_ms * std.time.ns_per_ms);
            const t0 = std.time.nanoTimestamp();
            fillPayload(seed, i, out);
            try link.write(out, env.wait());
            env.sent += 1;
            const len = try link.read(buf, env.wait());
            const rtt_ms: i64 = @intCast(@divTrunc(std.time.nanoTimestamp() - t0, std.time.ns_per_ms));
            worst_ms = @max(worst_ms, rtt_ms);
            if (!try env.verifySeq(seed, i, buf[0..len])) intact = false;
            env.received += 1;
        } else {
            const len = try link.read(buf, env.wait());
            env.received += 1;
            if (!try env.verifySeq(seed, i, buf[0..len])) intact = false;
            try link.write(buf[0..len], env.wait());
            env.sent += 1;
        }
    }
    env.check("every message intact", intact, "");
    if (env.cfg.role == .creator) {
        const detail = try std.fmt.allocPrint(env.gpa, "worst {d}ms", .{worst_ms});
        env.check("every round trip <= 250ms", worst_ms <= max_rtt_ms, detail);
        env.worst_rtt_ms = worst_ms;
    }
}

/// X-12: parked consumer woken by a producer (ping-pong over a ring of 2, 50 ms idle).
fn runX12(env: *Env) !void {
    var link = try openStd(env);
    defer link.deinit();
    try pingPong(env, &link, env.messages, 50, 250);
    try env.barrier("done");
}

// ---------------------------------------------------------------------------
// Advanced scenarios: shared mux and serve thread
// ---------------------------------------------------------------------------

const method_sleep_echo: u64 = 1;
const method_fail: u64 = 2;
const method_slow: u64 = 3;
const method_echo: u64 = 4;
const err_code_invalid_op: u64 = hqq.c.HQQ_ERRCODE_INVALID_OP;

/// Handler of the scenarios: sleep-echo, error response, slow answer, echo;
/// any other method is refused with status 6 (ErrCodeInvalidOp).
const Mux = struct {
    pub fn handle(_: *Mux, req: *hqq.Request, resp: *hqq.Response) void {
        switch (req.method()) {
            method_sleep_echo => {
                // Inverse sleep: later ids finish first.
                const d = req.data();
                var k: i64 = 16;
                if (d.len >= 4) k -= @as(i64, std.mem.readInt(u32, d[0..4], .big));
                _ = req.waitDone(@max(0, k) * 5 * std.time.ns_per_ms);
                resp.write(d) catch {};
            },
            method_fail => resp.writeError(42, "boom payload") catch {},
            method_slow => {
                _ = req.waitDone(300 * std.time.ns_per_ms);
                resp.write("late") catch {};
            },
            method_echo => resp.write(req.data()) catch {},
            else => resp.writeError(err_code_invalid_op, &.{}) catch {},
        }
    }
};

fn serveMain(link: hqq.AdvLink, mux: *Mux) void {
    // The serve result is not reported: it ends when the link is closed.
    link.serve(Mux, mux, hqq.Wait.forever) catch {};
}

// ---------------------------------------------------------------------------
// X-A01: negotiation, all features (adv_negotiate.go)
// ---------------------------------------------------------------------------

const all_features: u64 = hqq.c.HQQ_FEATURES_ALL;
const protocol_1_1: u16 = 0x0101; // protocol 1.1 version word

fn runX_A01(env: *Env) !void {
    const none = streq(env.cfg.variant, "none");
    const want: u64 = if (none) 0 else all_features;
    var link = try openAdv(env);
    defer link.deinit();
    // The creator requests the empty set explicitly (0 through negotiate means
    // "all"); the opener answers whatever the creator asks.
    if (none and env.cfg.role == .creator) {
        try link.negotiateProtocol(protocol_1_1, 0, env.wait());
    } else {
        try link.negotiate(0, env.wait());
    }
    env.check("negotiated version is 1.1", link.negotiatedVersion() == protocol_1_1, "");
    env.check("negotiated features match the request", link.negotiatedFeatures() == want, "");
    env.check("protocol state is Negotiated", link.protocolState() == .negotiated, "");

    const s0 = link.stream0();
    const seed = env.cfg.seed;
    if (env.cfg.role == .opener) {
        var mux: Mux = .{};
        var srv: ?std.Thread = null;
        if ((want & hqq.c.HQQ_FEATURE_MESSAGES) != 0) {
            var m = try s0.recv(env.wait());
            defer m.deinit();
            env.received += 1;
            const d = m.data();
            const ok = d.len == env.size and try env.verifySeq(seed, 0, d);
            const name = try std.fmt.allocPrint(env.gpa, "message 0 has {d} bytes and is intact", .{env.size});
            env.check(name, ok, "");
        }
        if ((want & hqq.c.HQQ_FEATURE_REQUEST_RESPONSE) != 0) {
            srv = try std.Thread.spawn(.{}, serveMain, .{ link, &mux });
        }
        try env.barrier("end");
        try link.close();
        if (srv) |t| t.join();
        return;
    }

    if ((want & hqq.c.HQQ_FEATURE_MESSAGES) != 0) {
        const payload = try env.gpa.alloc(u8, env.size);
        fillPayload(seed, 0, payload);
        const send_ok = if (s0.send(payload, env.wait())) |_| true else |_| false;
        env.check("send with FeatureMessages", send_ok, "");
        env.sent += 1;
    } else {
        env.expectErr("send without FeatureMessages is ErrNotNegotiated", s0.send("x", env.wait()), error.NotNegotiated);
    }
    if ((want & hqq.c.HQQ_FEATURE_REQUEST_RESPONSE) != 0) {
        var reply = try s0.callRaw(method_echo, "ping", env.wait());
        defer reply.deinit();
        env.check("call with FeatureRequestResponse is echoed", reply.status() == 0 and streq(reply.data(), "ping"), "");
    } else {
        env.expectErr("call without FeatureRequestResponse is ErrNotNegotiated", s0.callRaw(method_echo, "ping", env.wait()), error.NotNegotiated);
    }
    try env.barrier("end");
}

// ---------------------------------------------------------------------------
// X-A04: request/response (adv_rpc.go)
// ---------------------------------------------------------------------------

const rpc_concurrency = 16;

const RpcRun = struct {
    link: hqq.AdvLink,
    wait: hqq.Wait,
    mutex: std.Thread.Mutex = .{},
    order: [rpc_concurrency]usize = undefined,
    norder: usize = 0,
    errs: [rpc_concurrency]?anyerror = [_]?anyerror{null} ** rpc_concurrency,
    bad: [rpc_concurrency]bool = [_]bool{false} ** rpc_concurrency,
};

fn rpcWorker(run: *RpcRun, id: usize) void {
    defer {
        run.mutex.lock();
        run.order[run.norder] = id;
        run.norder += 1;
        run.mutex.unlock();
    }
    var p: [4]u8 = undefined;
    std.mem.writeInt(u32, &p, @intCast(id), .big);
    var m = run.link.stream0().callRaw(method_sleep_echo, &p, run.wait) catch |err| {
        run.errs[id] = err;
        return;
    };
    defer m.deinit();
    run.bad[id] = m.status() != 0 or !std.mem.eql(u8, m.data(), &p);
}

fn runRpcCreator(env: *Env, link: hqq.AdvLink) !void {
    const s0 = link.stream0();
    var run = RpcRun{ .link = link, .wait = env.wait() };
    var threads: [rpc_concurrency]std.Thread = undefined;
    var started: usize = 0;
    var spawn_err: ?anyerror = null;
    while (started < rpc_concurrency) : (started += 1) {
        threads[started] = std.Thread.spawn(.{}, rpcWorker, .{ &run, started }) catch |err| {
            spawn_err = err;
            break;
        };
    }
    for (threads[0..started]) |t| t.join();
    if (spawn_err) |err| return err;

    var first_err: ?anyerror = null;
    var any_bad = false;
    for (0..rpc_concurrency) |i| {
        if (first_err == null) first_err = run.errs[i];
        any_bad = any_bad or run.bad[i];
    }
    const all_ok = first_err == null and !any_bad;
    const detail: []const u8 = if (first_err) |err| kindName(err) else if (any_bad) "echo mismatch" else "";
    env.check("all concurrent calls are answered with their own payload", all_ok, detail);
    var sorted = true;
    for (1..run.norder) |i| {
        if (run.order[i] < run.order[i - 1]) sorted = false;
    }
    env.check("responses complete out of order", !sorted, "");
    env.sent = rpc_concurrency;

    // Error response with a payload.
    var fail = try s0.callRaw(method_fail, "", env.wait());
    defer fail.deinit();
    env.meta_status = fail.status();
    env.check("error response carries status 42 and its payload", fail.status() == 42 and streq(fail.data(), "boom payload"), "");

    // Cancelled call: 50 ms budget; the late response is discarded later.
    env.expectErr("cancelled call returns its context error", s0.callRaw(method_slow, "slow", hqq.Wait{ .timeout_ns = 50 * std.time.ns_per_ms }), error.Timeout);
    const pump_until = std.time.nanoTimestamp() + 3 * std.time.ns_per_s;
    while (link.stats().abandoned_responses == 0 and std.time.nanoTimestamp() < pump_until) {
        var m = try s0.callRaw(method_echo, "pump", env.wait());
        m.deinit();
    }
    const abandoned = link.stats().abandoned_responses;
    const detail_abandoned = try std.fmt.allocPrint(env.gpa, "abandoned {d}", .{abandoned});
    env.check("late response of the cancelled call is discarded (AbandonedResponses)", abandoned >= 1, detail_abandoned);

    // Unknown method: refused with status 6.
    var unknown = try s0.callRaw(99, "", env.wait());
    defer unknown.deinit();
    env.check("unknown method is answered with status 6 (ErrCodeInvalidOp)", unknown.status() == err_code_invalid_op, "");
}

fn runX_A04(env: *Env) !void {
    var link = try openAdv(env);
    defer link.deinit();
    // Messages and request-response only (1.0 set): late responses are sent.
    try link.negotiate(hqq.c.HQQ_FEATURE_MESSAGES | hqq.c.HQQ_FEATURE_REQUEST_RESPONSE, env.wait());
    if (env.cfg.role == .opener) {
        var mux: Mux = .{};
        var srv = try std.Thread.spawn(.{}, serveMain, .{ link, &mux });
        try env.barrier("end");
        try link.close();
        srv.join();
        return;
    }
    try runRpcCreator(env, link);
    try env.barrier("end");
}

// ---------------------------------------------------------------------------
// X-A05: streams, basic (adv_streams.go)
// ---------------------------------------------------------------------------

fn streamRecvCheck(env: *Env, st: hqq.Stream, seed: u64, seq: u64) !void {
    var m = try st.recv(env.wait());
    defer m.deinit();
    const d = m.data();
    const ok = d.len == env.size and try env.verifySeq(seed, seq, d);
    const name = try std.fmt.allocPrint(env.gpa, "stream message {d} has {d} bytes and is intact", .{ seq, env.size });
    env.check(name, ok, "");
}

fn runX_A05(env: *Env) !void {
    var link = try openAdv(env);
    defer link.deinit();
    try link.negotiate(0, env.wait());
    const seed = env.cfg.seed;
    const payload = try env.gpa.alloc(u8, env.size);

    if (env.cfg.role == .opener) {
        // Starts the dispatcher; not listening yet.
        const refused = link.acceptStream(hqq.Wait{ .timeout_ns = std.time.ns_per_s });
        env.expectErr("AcceptStream without ListenStreams is ErrNotListening", refused, error.NotListening);
        if (refused) |st| st.close() catch {} else |_| {}
        try env.barrier("p1");
        try env.barrier("p2");
        try link.listenStreams();
        try env.barrier("p3");

        const st = try link.acceptStream(env.wait());
        env.check("first accepted stream has id 4 (the refused open consumed 2)", st.id() == 4, "");
        try streamRecvCheck(env, st, seed, 0);
        try streamRecvCheck(env, st, seed, 1);
        fillPayload(seed + 1, 0, payload[0..10]);
        try st.send(payload[0..10], env.wait());
        const r = st.recv(env.wait());
        env.expectErr("peer close is seen after the queued data as ErrPeerClosed", r, error.PeerClosed);

        const st2 = try link.acceptStream(env.wait());
        env.check("second accepted stream has id 6 (ids are not reused)", st2.id() == 6, "");
        try streamRecvCheck(env, st2, seed, 2);
        env.received = 3;
        return env.barrier("end");
    }

    // Creator: the Primary allocates even ids.
    try env.barrier("p1");
    const refused = link.openStream(env.wait());
    env.expectErr("OpenStream refused while the peer does not listen is ErrConnNotFound", refused, error.ConnNotFound);
    if (refused) |st| st.close() catch {} else |_| {}
    try env.barrier("p2");
    try env.barrier("p3");

    const st = try link.openStream(env.wait());
    env.check("first stream has id 4 (the refused open consumed 2)", st.id() == 4, "");
    for (0..2) |i| {
        fillPayload(seed, i, payload);
        try st.send(payload, env.wait());
        env.sent += 1;
    }
    var reply = try st.recv(env.wait());
    defer reply.deinit();
    env.check("reply from the peer's stream is intact", try env.verifySeq(seed + 1, 0, reply.data()), "");
    st.close() catch |err| return err;
    env.expectErr("second Close is ErrClosed", st.close(), error.Closed);
    env.expectErr("Recv on a closed stream is ErrClosed", st.recv(env.wait()), error.Closed);
    env.expectErr("stream 0 cannot be closed", link.stream0().close(), error.InvalidOp);

    const st2 = try link.openStream(env.wait());
    env.check("second stream has id 6 (ids are not reused)", st2.id() == 6, "");
    fillPayload(seed, 2, payload);
    try st2.send(payload, env.wait());
    env.sent += 1;
    try st2.close();
    env.sent = 3;
    try env.barrier("end");
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

fn runScenario(env: *Env) !void {
    const id = env.cfg.scenario;
    if (streq(id, "X-01")) return runX01(env);
    if (streq(id, "X-06")) return runX06(env);
    if (streq(id, "X-12")) return runX12(env);
    if (streq(id, "X-A01")) return runX_A01(env);
    if (streq(id, "X-A04")) return runX_A04(env);
    if (streq(id, "X-A05")) return runX_A05(env);
    unreachable; // runCommand only passes IDs that findScenario found
}

fn writeStdout(bytes: []const u8) void {
    std.fs.File.stdout().writeAll(bytes) catch {};
}

/// Result line (section 4), written as one line.
fn resultLine(buf: []u8, cfg: *const Config, res: Result) ![]u8 {
    var j = Json{ .buf = buf };
    try j.raw("{");
    try j.field("scenario");
    try j.str(cfg.scenario);
    try j.raw(",");
    try j.field("variant");
    try j.str(cfg.variant);
    try j.fmt(",\"step\":{d},", .{cfg.step});
    try j.field("role");
    try j.str(if (cfg.role == .creator) "creator" else "opener");
    try j.raw(",");
    try j.field("impl");
    try j.str(cfg.impl);
    try j.raw(",");
    try j.field("peer_impl");
    try j.str(cfg.peer_impl);
    try j.fmt(",\"ok\":{},\"skipped\":{},\"crashed\":{},", .{ res.ok, res.skipped, res.crashed });
    try j.field("error_kind");
    try j.str(res.error_kind);
    try j.raw(",");
    try j.field("error");
    try j.str(res.error_text);
    try j.raw(",\"checks\":[");
    for (res.checks, 0..) |ch, i| {
        if (i > 0) try j.raw(",");
        try j.raw("{");
        try j.field("name");
        try j.str(ch.name);
        try j.fmt(",\"ok\":{},", .{ch.ok});
        try j.field("detail");
        try j.str(ch.detail);
        try j.raw("}");
    }
    try j.fmt("],\"sent\":{d},\"received\":{d},\"elapsed_ms\":{d}", .{ res.sent, res.received, res.elapsed_ms });
    if (res.meta_status != null or res.worst_rtt_ms != null) {
        try j.raw(",\"meta\":{");
        var first = true;
        if (res.meta_status) |s| {
            try j.fmt("\"status\":{d}", .{s});
            first = false;
        }
        if (res.worst_rtt_ms) |w| {
            if (!first) try j.raw(",");
            try j.fmt("\"worst_rtt_ms\":{d}", .{w});
        }
        try j.raw("}");
    }
    try j.raw("}\n");
    return buf[0..j.len];
}

const Result = struct {
    ok: bool = false,
    skipped: bool = false,
    crashed: bool = false,
    error_kind: []const u8 = "",
    error_text: []const u8 = "",
    checks: []const Check = &.{},
    sent: u64 = 0,
    received: u64 = 0,
    elapsed_ms: i64 = 0,
    meta_status: ?u64 = null,
    worst_rtt_ms: ?i64 = null,
};

/// Process deadline (--timeout) backstop: 2 s after it, print HarnessTimeout and exit 3.
const Watchdog = struct {
    mutex: std.Thread.Mutex = .{},
    done: bool = false,
    cfg: *const Config,
    start: i128,
    deadline: i128,
};

fn watchdogMain(w: *Watchdog) void {
    const fire_at = w.deadline + 2 * std.time.ns_per_s;
    while (std.time.nanoTimestamp() < fire_at) std.Thread.sleep(10 * std.time.ns_per_ms);
    w.mutex.lock();
    if (w.done) {
        w.mutex.unlock();
        return;
    }
    var buf: [4096]u8 = undefined;
    const line = resultLine(&buf, w.cfg, .{
        .error_kind = "HarnessTimeout",
        .error_text = "process exceeded its deadline",
        .elapsed_ms = @intCast(@divTrunc(std.time.nanoTimestamp() - w.start, std.time.ns_per_ms)),
    }) catch buf[0..0];
    writeStdout(line);
    std.process.exit(exit_timeout);
}

fn runCommand(gpa: std.mem.Allocator, args: []const [:0]u8) !u8 {
    var cfg = Config{};
    parseRunFlags(args, &cfg) catch return exit_usage;

    const scen = findScenario(cfg.scenario) orelse {
        std.debug.print("hqq-interop: unknown scenario {s}\n", .{cfg.scenario});
        return exit_usage;
    };
    const variant: Variant = blk: {
        for (scen.variants) |v| {
            if (cfg.variant.len == 0 or streq(v.name, cfg.variant)) break :blk v;
        }
        std.debug.print("hqq-interop: scenario {s} has no variant {s}\n", .{ scen.id, cfg.variant });
        return exit_usage;
    };
    if (cfg.step != 1) {
        std.debug.print("hqq-interop: scenario {s} has 1 step\n", .{scen.id});
        return exit_usage;
    }
    if (cfg.name.len == 0) {
        std.debug.print("hqq-interop: --name is required\n", .{});
        return exit_usage;
    }
    if (cfg.sync_dir.len == 0) {
        std.debug.print("hqq-interop: --sync-dir is required for multi-process steps\n", .{});
        return exit_usage;
    }
    cfg.scenario = scen.id;
    cfg.variant = variant.name;

    var res = Result{ .sent = 0 };
    if (cfg.crash_after.len != 0) {
        // Crash points are not implemented (X-07, X-08 are not listed).
        res.skipped = true;
        res.ok = true;
        res.error_text = "crash points are not implemented by this helper";
        var buf: [4096]u8 = undefined;
        writeStdout(try resultLine(&buf, &cfg, res));
        return exit_skipped;
    }
    if (!goldenOk()) {
        std.debug.print("hqq-interop: payload golden vectors do not match\n", .{});
        return exit_internal;
    }

    const bc = cfg.buffer_count orelse variant.params.buffer_count;
    const bs = cfg.buffer_size orelse variant.params.buffer_size;
    const buffer_count = if (bc != 0) bc else 4;
    const buffer_size = if (bs != 0) bs else 256;
    const messages = cfg.messages orelse variant.params.messages;
    const size = cfg.size orelse buffer_size;

    const start = std.time.nanoTimestamp();
    const timeout_ns: i128 = @intFromFloat(cfg.timeout_s * std.time.ns_per_s);
    var wd = Watchdog{ .cfg = &cfg, .start = start, .deadline = start + timeout_ns };
    const wd_thread = try std.Thread.spawn(.{}, watchdogMain, .{&wd});
    wd_thread.detach();

    var env = Env{
        .cfg = &cfg,
        .gpa = gpa,
        .start = start,
        .deadline = start + timeout_ns,
        .bc = buffer_count,
        .bs = buffer_size,
        .size = size,
        .messages = if (messages != 0) messages else 100,
    };

    var run_err: ?anyerror = null;
    runScenario(&env) catch |err| {
        run_err = err;
    };
    if (run_err == null and env.failed) run_err = error.CheckFailed;

    res.ok = run_err == null;
    res.checks = env.checks[0..env.nchecks];
    res.sent = env.sent;
    res.received = env.received;
    res.meta_status = env.meta_status;
    res.worst_rtt_ms = env.worst_rtt_ms;
    res.elapsed_ms = @intCast(@divTrunc(std.time.nanoTimestamp() - start, std.time.ns_per_ms));
    if (run_err) |err| {
        res.error_kind = if (err == error.CheckFailed) "CheckFailed" else kindName(err);
        res.error_text = @errorName(err);
    }

    var buf: [65536]u8 = undefined;
    const line = try resultLine(&buf, &cfg, res);
    wd.mutex.lock();
    if (wd.done) {
        // The watchdog has fired and owns the exit.
        wd.mutex.unlock();
        while (true) std.Thread.sleep(std.time.ns_per_s);
    }
    wd.done = true;
    wd.mutex.unlock();
    writeStdout(line);
    return if (res.ok) exit_ok else exit_failed;
}

fn printList(buf: []u8, impl: []const u8) ![]u8 {
    var j = Json{ .buf = buf };
    try j.raw("{");
    try j.field("helper");
    try j.str("hqq-interop");
    try j.raw(",");
    try j.field("impl");
    try j.str(impl);
    try j.raw(",");
    try j.field("protocol");
    try j.str(protocol);
    try j.raw(",");
    try j.field("layout");
    try j.str(layout);
    try j.raw(",");
    try j.field("runtime");
    try j.str("zig " ++ @import("builtin").zig_version_string);
    try j.raw(",\"scenarios\":[");
    for (catalogue, 0..) |s, si| {
        if (si > 0) try j.raw(",");
        try j.raw("{");
        try j.field("id");
        try j.str(s.id);
        try j.raw(",");
        try j.field("title");
        try j.str(s.title);
        try j.raw(",");
        try j.field("kind");
        try j.str(s.kind);
        try j.raw(",\"platforms\":[\"any\"],");
        try j.fmt("\"timeout_s\":{d},\"variants\":[", .{s.timeout_s});
        for (s.variants, 0..) |v, vi| {
            if (vi > 0) try j.raw(",");
            try j.raw("{");
            try j.field("name");
            try j.str(v.name);
            try j.raw(",\"params\":{");
            var first = true;
            const p = v.params;
            if (p.buffer_count != 0) {
                try j.fmt("\"buffer_count\":{d}", .{p.buffer_count});
                first = false;
            }
            if (p.buffer_size != 0) {
                if (!first) try j.raw(",");
                try j.fmt("\"buffer_size\":{d}", .{p.buffer_size});
                first = false;
            }
            if (p.messages != 0) {
                if (!first) try j.raw(",");
                try j.fmt("\"messages\":{d}", .{p.messages});
            }
            try j.raw("},\"steps\":[{\"roles\":[\"creator\",\"opener\"],\"expect\":{\"creator\":\"ok\",\"opener\":\"ok\"}}]}");
        }
        try j.raw("]}");
    }
    try j.raw("]}\n");
    return buf[0..j.len];
}

pub fn main() void {
    var arena = std.heap.ArenaAllocator.init(std.heap.c_allocator);
    const gpa = arena.allocator();
    const args = std.process.argsAlloc(gpa) catch std.process.exit(exit_internal);
    if (args.len < 2) {
        std.debug.print("usage:\n  hqq-interop run --scenario ID --role creator|opener --name NAME --sync-dir DIR [flags]\n  hqq-interop list [--impl LABEL]\n  hqq-interop version\n", .{});
        std.process.exit(exit_usage);
    }
    const cmd: []const u8 = args[1];
    var buf: [65536]u8 = undefined;
    if (streq(cmd, "version")) {
        var j = Json{ .buf = &buf };
        j.raw("{\"helper\":\"hqq-interop\",\"impl\":\"zig\",\"protocol\":\"1.1\",\"segment_layout\":\"HQQ_STL3\",\"ring_layout\":2}\n") catch std.process.exit(exit_internal);
        writeStdout(buf[0..j.len]);
        std.process.exit(exit_ok);
    }
    if (streq(cmd, "list")) {
        var impl: []const u8 = impl_default;
        var i: usize = 2;
        while (i < args.len) : (i += 1) {
            if (streq(args[i], "--impl") and i + 1 < args.len) {
                i += 1;
                impl = args[i];
            } else if (std.mem.startsWith(u8, args[i], "--impl=")) {
                impl = args[i][7..];
            } else std.process.exit(exit_usage);
        }
        const out = printList(&buf, impl) catch std.process.exit(exit_internal);
        writeStdout(out);
        std.process.exit(exit_ok);
    }
    if (streq(cmd, "run")) {
        const code = runCommand(gpa, args[2..]) catch |err| {
            std.debug.print("hqq-interop: {s}\n", .{@errorName(err)});
            std.process.exit(exit_internal);
        };
        std.process.exit(code);
    }
    std.debug.print("hqq-interop: unknown subcommand {s}\n", .{cmd});
    std.process.exit(exit_usage);
}
