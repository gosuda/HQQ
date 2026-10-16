//! Zig binding of libhqq (C reference library, see c/include/hqq/*.h).
//!
//! Target: Zig 0.15.x. UNVERIFIED: not compiled or run in the authoring
//! environment (Zig is not installed there).
//!
//! Conventions
//! - Errors: every fallible C call returns `hqq_status`; `check` maps HQQ_OK to
//!   success and every other status to a member of `Error` (1:1).
//! - Waits: every blocking call takes a `Wait`. `Wait.forever` blocks without
//!   timeout, `Wait.none` never blocks (fails with error.Timeout).
//! - Handles: `Link`, `AdvLink`, `Acceptor` own their C handle; `close` frees
//!   it (idempotent), `deinit` is `close` ignoring errors, for `defer`.
//!   `Stream` and `Request` are non-owning views; `Message` owns a pooled message.
//! - `c` is the raw cImport namespace (constants and types not wrapped here).
const std = @import("std");

pub const c = @cImport({
    @cInclude("hqq/hqq.h");
    @cInclude("hqq/hqq_advanced.h");
    @cInclude("hqq/hqq_listener.h");
});

pub const Error = error{
    Closed,
    PeerClosed,
    PeerGone,
    Timeout,
    LinkBusy,
    IncompatibleLayout,
    FailedInit,
    SegmentNotReady,
    InitContended,
    NoLiveEndpoint,
    InvalidSize,
    GeometryMismatch,
    MemoryAlign,
    MemorySmall,
    BufferOverflow,
    BufferTooSmall,
    ConnNotFound,
    InvalidOp,
    UnexpectedPacket,
    Protocol,
    InvalidName,
    NotExist,
    Exists,
    System,
    Canceled,
    NoMemory,
    InvalidArgument,
    Unsupported,
    NotNegotiated,
    AlreadyNegotiated,
    NegotiationFailed,
    NegotiationRejected,
    MessageAborted,
    IncomingOverflow,
    ReassemblyLimit,
    ShuttingDown,
    Response,
    NotListening,
    AlreadyListening,
    ListenerBusy,
    ListenerGone,
    ListenerClosed,
    AcceptorClosed,
    ListenerNotReady,
};

/// One row per non-OK hqq_status (hqq.h). Keep in sync with the header.
const status_table = [_]struct { err: Error, status: c.hqq_status }{
    .{ .err = error.Closed, .status = c.HQQ_ERR_CLOSED },
    .{ .err = error.PeerClosed, .status = c.HQQ_ERR_PEER_CLOSED },
    .{ .err = error.PeerGone, .status = c.HQQ_ERR_PEER_GONE },
    .{ .err = error.Timeout, .status = c.HQQ_ERR_TIMEOUT },
    .{ .err = error.LinkBusy, .status = c.HQQ_ERR_LINK_BUSY },
    .{ .err = error.IncompatibleLayout, .status = c.HQQ_ERR_INCOMPATIBLE_LAYOUT },
    .{ .err = error.FailedInit, .status = c.HQQ_ERR_FAILED_INIT },
    .{ .err = error.SegmentNotReady, .status = c.HQQ_ERR_SEGMENT_NOT_READY },
    .{ .err = error.InitContended, .status = c.HQQ_ERR_INIT_CONTENDED },
    .{ .err = error.NoLiveEndpoint, .status = c.HQQ_ERR_NO_LIVE_ENDPOINT },
    .{ .err = error.InvalidSize, .status = c.HQQ_ERR_INVALID_SIZE },
    .{ .err = error.GeometryMismatch, .status = c.HQQ_ERR_GEOMETRY_MISMATCH },
    .{ .err = error.MemoryAlign, .status = c.HQQ_ERR_MEMORY_ALIGN },
    .{ .err = error.MemorySmall, .status = c.HQQ_ERR_MEMORY_SMALL },
    .{ .err = error.BufferOverflow, .status = c.HQQ_ERR_BUFFER_OVERFLOW },
    .{ .err = error.BufferTooSmall, .status = c.HQQ_ERR_BUFFER_TOO_SMALL },
    .{ .err = error.ConnNotFound, .status = c.HQQ_ERR_CONN_NOT_FOUND },
    .{ .err = error.InvalidOp, .status = c.HQQ_ERR_INVALID_OP },
    .{ .err = error.UnexpectedPacket, .status = c.HQQ_ERR_UNEXPECTED_PACKET },
    .{ .err = error.Protocol, .status = c.HQQ_ERR_PROTOCOL },
    .{ .err = error.InvalidName, .status = c.HQQ_ERR_INVALID_NAME },
    .{ .err = error.NotExist, .status = c.HQQ_ERR_NOT_EXIST },
    .{ .err = error.Exists, .status = c.HQQ_ERR_EXISTS },
    .{ .err = error.System, .status = c.HQQ_ERR_SYSTEM },
    .{ .err = error.Canceled, .status = c.HQQ_ERR_CANCELED },
    .{ .err = error.NoMemory, .status = c.HQQ_ERR_NO_MEMORY },
    .{ .err = error.InvalidArgument, .status = c.HQQ_ERR_INVALID_ARGUMENT },
    .{ .err = error.Unsupported, .status = c.HQQ_ERR_UNSUPPORTED },
    .{ .err = error.NotNegotiated, .status = c.HQQ_ERR_NOT_NEGOTIATED },
    .{ .err = error.AlreadyNegotiated, .status = c.HQQ_ERR_ALREADY_NEGOTIATED },
    .{ .err = error.NegotiationFailed, .status = c.HQQ_ERR_NEGOTIATION_FAILED },
    .{ .err = error.NegotiationRejected, .status = c.HQQ_ERR_NEGOTIATION_REJECTED },
    .{ .err = error.MessageAborted, .status = c.HQQ_ERR_MESSAGE_ABORTED },
    .{ .err = error.IncomingOverflow, .status = c.HQQ_ERR_INCOMING_OVERFLOW },
    .{ .err = error.ReassemblyLimit, .status = c.HQQ_ERR_REASSEMBLY_LIMIT },
    .{ .err = error.ShuttingDown, .status = c.HQQ_ERR_SHUTTING_DOWN },
    .{ .err = error.Response, .status = c.HQQ_ERR_RESPONSE },
    .{ .err = error.NotListening, .status = c.HQQ_ERR_NOT_LISTENING },
    .{ .err = error.AlreadyListening, .status = c.HQQ_ERR_ALREADY_LISTENING },
    .{ .err = error.ListenerBusy, .status = c.HQQ_ERR_LISTENER_BUSY },
    .{ .err = error.ListenerGone, .status = c.HQQ_ERR_LISTENER_GONE },
    .{ .err = error.ListenerClosed, .status = c.HQQ_ERR_LISTENER_CLOSED },
    .{ .err = error.AcceptorClosed, .status = c.HQQ_ERR_ACCEPTOR_CLOSED },
    .{ .err = error.ListenerNotReady, .status = c.HQQ_ERR_LISTENER_NOT_READY },
};

fn toError(status: c.hqq_status) Error {
    for (status_table) |row| {
        if (row.status == status) return row.err;
    }
    unreachable; // every non-OK status is in status_table
}

fn statusOf(err: Error) c.hqq_status {
    for (status_table) |row| {
        if (row.err == err) return row.status;
    }
    unreachable; // every Error member is in status_table
}

fn check(status: c.hqq_status) Error!void {
    if (status == c.HQQ_OK) return;
    return toError(status);
}

/// True when `err` is `base` or wraps it in the Go error hierarchy (hqq_error_is).
pub fn errorIs(err: Error, base: Error) bool {
    return c.hqq_error_is(statusOf(err), statusOf(base)) != 0;
}

/// The libhqq version string ("0.1.0").
pub fn version() []const u8 {
    return std.mem.span(c.hqq_version_string());
}

/// Per-call wait control. timeout_ns < 0: no timeout; 0: never block;
/// > 0: relative timeout. The cancel token, when set, must outlive the call.
pub const Wait = struct {
    timeout_ns: i64 = -1,
    cancel: ?*const c.hqq_cancel = null,

    pub const forever: Wait = .{};
    pub const none: Wait = .{ .timeout_ns = 0 };

    /// Native struct. Keep the Wait (and the returned value) alive during the call.
    pub fn native(self: Wait) c.hqq_wait {
        return .{ .timeout_ns = self.timeout_ns, .cancel = self.cancel };
    }
};

/// Standard link options (hqq_options). Init with `init`; set fields on `raw`.
pub const Options = struct {
    raw: c.hqq_options,

    pub fn init() Options {
        var raw: c.hqq_options = undefined;
        c.hqq_options_init(&raw);
        return .{ .raw = raw };
    }
};

/// Advanced link options (hqq_adv_options); `raw.link` holds the link options.
pub const AdvOptions = struct {
    raw: c.hqq_adv_options,

    pub fn init() AdvOptions {
        var raw: c.hqq_adv_options = undefined;
        c.hqq_adv_options_init(&raw);
        c.hqq_options_init(&raw.link);
        return .{ .raw = raw };
    }
};

pub const AcceptorOptions = struct {
    raw: c.hqq_acceptor_options,

    pub fn init() AcceptorOptions {
        var raw: c.hqq_acceptor_options = undefined;
        c.hqq_acceptor_options_init(&raw);
        c.hqq_adv_options_init(&raw.link);
        return .{ .raw = raw };
    }
};

pub const ConnectOptions = struct {
    raw: c.hqq_connect_options,

    pub fn init() ConnectOptions {
        var raw: c.hqq_connect_options = undefined;
        c.hqq_connect_options_init(&raw);
        c.hqq_adv_options_init(&raw.link);
        return .{ .raw = raw };
    }
};

/// Caller options of a call (Go CallOptions). Zero means "none".
pub const CallOptions = struct {
    raw: c.hqq_call_options,

    pub fn init() CallOptions {
        var raw: c.hqq_call_options = undefined;
        c.hqq_call_options_init(&raw);
        return .{ .raw = raw };
    }
};

/// Statistics of an Advanced link (hqq_adv_stats fields).
pub const Stats = c.hqq_adv_stats;

/// Negotiated protocol state of an Advanced link.
pub const ProtocolState = enum { none, negotiating, negotiated, failed };

fn optPtr(comptime T: type, opts: ?*const T) ?*const c.hqq_options {
    _ = T;
    return if (opts) |o| &o.raw else null;
}

// ---------------------------------------------------------------------------
// Standard link
// ---------------------------------------------------------------------------

/// A named or caller-memory Standard link (hqq_link). One message per write.
pub const Link = struct {
    raw: *c.hqq_link,
    closed: bool = false,

    /// Creates the named segment and becomes its Primary. `name` is at most 30
    /// bytes, [A-Za-z0-9_-]. `perm` is the POSIX mode, e.g. 0o600. `opts` may be null.
    pub fn create(name: [:0]const u8, buffer_count: u64, buffer_size: u64, perm: u32, opts: ?*const Options) Error!Link {
        var out: ?*c.hqq_link = null;
        try check(c.hqq_standard_create(name.ptr, buffer_count, buffer_size, perm, optPtr(Options, opts), &out, null));
        return .{ .raw = out.? };
    }

    /// Attaches to an existing named segment as Secondary.
    pub fn open(name: [:0]const u8, buffer_count: u64, buffer_size: u64, opts: ?*const Options) Error!Link {
        var out: ?*c.hqq_link = null;
        try check(c.hqq_standard_open(name.ptr, buffer_count, buffer_size, optPtr(Options, opts), &out, null));
        return .{ .raw = out.? };
    }

    /// Publishes `bytes` (1..buffer_size) as one message.
    pub fn write(self: Link, bytes: []const u8, wait: Wait) Error!void {
        const nw = wait.native();
        try check(c.hqq_write(self.raw, bytes.ptr, bytes.len, &nw));
    }

    /// Consumes the next message into `buf` and returns its length. A message
    /// larger than `buf` is not consumed: error.BufferTooSmall (see readAlloc).
    pub fn read(self: Link, buf: []u8, wait: Wait) Error!usize {
        const nw = wait.native();
        var n: usize = 0;
        try check(c.hqq_read(self.raw, buf.ptr, buf.len, &n, &nw));
        return n;
    }

    /// Consumes the next message into a new slice of exactly its size. A probe
    /// read with capacity 0 reports the size (the message stays queued).
    pub fn readAlloc(self: Link, allocator: std.mem.Allocator, wait: Wait) (Error || std.mem.Allocator.Error)![]u8 {
        const nw = wait.native();
        var need: usize = 0;
        const probe = c.hqq_read(self.raw, null, 0, &need, &nw);
        if (probe != c.HQQ_ERR_BUFFER_TOO_SMALL) {
            try check(probe);
            return allocator.alloc(u8, 0);
        }
        const buf = try allocator.alloc(u8, need);
        errdefer allocator.free(buf);
        var n: usize = 0;
        try check(c.hqq_read(self.raw, buf.ptr, buf.len, &n, &nw));
        std.debug.assert(n == need);
        return buf;
    }

    pub fn peerState(self: Link) PeerState {
        return switch (@as(i64, c.hqq_peer_state(self.raw))) {
            c.HQQ_PEER_NOT_CONNECTED => .not_connected,
            c.HQQ_PEER_CONNECTED => .connected,
            c.HQQ_PEER_CLOSED => .closed,
            c.HQQ_PEER_GONE => .gone,
            else => unreachable,
        };
    }

    /// Stops the link and frees the handle (hqq_close). Idempotent.
    pub fn close(self: *Link) Error!void {
        if (self.closed) return;
        self.closed = true;
        try check(c.hqq_close(self.raw));
    }

    /// `close` ignoring errors; for `defer link.deinit()`.
    pub fn deinit(self: *Link) void {
        self.close() catch {};
    }
};

pub const PeerState = enum { not_connected, connected, closed, gone };

// ---------------------------------------------------------------------------
// Advanced link
// ---------------------------------------------------------------------------

/// Context of a Serve handler's request (hqq_request + its hqq_req_ctx).
pub const Request = struct {
    raw: *const c.hqq_request,
    ctx: *c.hqq_req_ctx,

    pub fn method(self: Request) u64 {
        return c.hqq_request_method(self.raw);
    }

    pub fn data(self: Request) []const u8 {
        const n = c.hqq_request_len(self.raw);
        if (n == 0) return &.{};
        const p: [*]const u8 = @ptrCast(c.hqq_request_data(self.raw).?);
        return p[0..n];
    }

    pub fn streamId(self: Request) u64 {
        return c.hqq_request_stream_id(self.raw);
    }

    pub fn caller(self: Request) u64 {
        return c.hqq_request_caller(self.raw);
    }

    /// The trace ID, or null when the request carried none.
    pub fn trace(self: Request) ?[16]u8 {
        var out: [16]u8 = undefined;
        return if (c.hqq_request_trace(self.raw, &out) != 0) out else null;
    }

    /// True when the request ended (canceled or deadline) within `timeout_ns`.
    pub fn waitDone(self: Request, timeout_ns: i64) bool {
        return c.hqq_req_ctx_wait_done(self.ctx, timeout_ns) == c.HQQ_OK;
    }
};

/// Response writer of one request (hqq_response).
pub const Response = struct {
    raw: *c.hqq_response,

    /// Appends to the response body; the response is sent when the handler returns.
    pub fn write(self: Response, bytes: []const u8) Error!void {
        try check(c.hqq_response_write(self.raw, bytes.ptr, bytes.len));
    }

    /// Replaces the body with an error payload and sets the status (non-zero).
    pub fn writeError(self: Response, status: u64, bytes: []const u8) Error!void {
        try check(c.hqq_response_error(self.raw, status, bytes.ptr, bytes.len));
    }
};

/// A received message (hqq_msg). Release with `deinit`.
pub const Message = struct {
    raw: *c.hqq_msg,

    pub fn data(self: Message) []const u8 {
        const n = c.hqq_msg_len(self.raw);
        if (n == 0) return &.{};
        const p: [*]const u8 = @ptrCast(c.hqq_msg_data(self.raw).?);
        return p[0..n];
    }

    /// Status of an error response; 0 for every other message.
    pub fn status(self: Message) u64 {
        return c.hqq_msg_status(self.raw);
    }

    pub fn streamId(self: Message) u64 {
        return c.hqq_msg_stream_id(self.raw);
    }

    pub fn deinit(self: Message) void {
        c.hqq_msg_release(self.raw);
    }
};

/// A stream of an Advanced link (non-owning; valid until the link is closed).
pub const Stream = struct {
    raw: *c.hqq_stream,

    pub fn id(self: Stream) u64 {
        return c.hqq_stream_id(self.raw);
    }

    pub fn send(self: Stream, bytes: []const u8, wait: Wait) Error!void {
        const nw = wait.native();
        try check(c.hqq_stream_send(self.raw, bytes.ptr, bytes.len, &nw));
    }

    pub fn recv(self: Stream, wait: Wait) Error!Message {
        const nw = wait.native();
        var m: ?*c.hqq_msg = null;
        try check(c.hqq_stream_recv(self.raw, &m, &nw));
        return .{ .raw = m.? };
    }

    /// Calls `method`. An error response is returned as a Message whose
    /// status() is non-zero (the error payload is its data()).
    pub fn callRaw(self: Stream, method: u64, req: []const u8, wait: Wait) Error!Message {
        return self.callWithRaw(method, req, null, wait);
    }

    /// Calls `method` and returns the response; an error response is error.Response.
    pub fn call(self: Stream, method: u64, req: []const u8, wait: Wait) Error!Message {
        return self.callWith(method, req, CallOptions.init(), wait);
    }

    pub fn callWith(self: Stream, method: u64, req: []const u8, opts: CallOptions, wait: Wait) Error!Message {
        return self.callWithRaw(method, req, &opts, wait);
    }

    fn callWithRaw(self: Stream, method: u64, req: []const u8, opts: ?*const CallOptions, wait: Wait) Error!Message {
        const nw = wait.native();
        var m: ?*c.hqq_msg = null;
        const st = if (opts) |o|
            c.hqq_stream_call_with(self.raw, method, req.ptr, req.len, &o.raw, &m, &nw)
        else
            c.hqq_stream_call(self.raw, method, req.ptr, req.len, &m, &nw);
        if (st == c.HQQ_ERR_RESPONSE) return .{ .raw = m.? };
        try check(st);
        return .{ .raw = m.? };
    }

    pub fn close(self: Stream) Error!void {
        try check(c.hqq_stream_close(self.raw));
    }
};

/// An Advanced link (hqq_adv): negotiation, streams, requests and shutdown.
/// Calls may run concurrently; `close` must not run from a Serve handler.
pub const AdvLink = struct {
    raw: *c.hqq_adv,
    closed: bool = false,

    pub fn create(name: [:0]const u8, buffer_count: u64, buffer_size: u64, perm: u32, opts: ?*const AdvOptions) Error!AdvLink {
        var out: ?*c.hqq_adv = null;
        try check(c.hqq_adv_create(name.ptr, buffer_count, buffer_size, perm, if (opts) |o| &o.raw else null, &out, null));
        return .{ .raw = out.? };
    }

    pub fn open(name: [:0]const u8, buffer_count: u64, buffer_size: u64, opts: ?*const AdvOptions) Error!AdvLink {
        var out: ?*c.hqq_adv = null;
        try check(c.hqq_adv_open(name.ptr, buffer_count, buffer_size, if (opts) |o| &o.raw else null, &out, null));
        return .{ .raw = out.? };
    }

    /// Closes the link and frees the handle (hqq_adv_close). Idempotent.
    pub fn close(self: *AdvLink) Error!void {
        if (self.closed) return;
        self.closed = true;
        try check(c.hqq_adv_close(self.raw));
    }

    pub fn deinit(self: *AdvLink) void {
        self.close() catch {};
    }

    /// Negotiates the features. `features == 0` requests every feature of
    /// protocol 1.1. Use negotiateProtocol to request the empty feature set.
    pub fn negotiate(self: AdvLink, features: u64, wait: Wait) Error!void {
        const nw = wait.native();
        try check(c.hqq_adv_negotiate(self.raw, features, &nw));
    }

    /// Like negotiate, with an explicit version word. `features` is used as
    /// given, so 0 requests the empty feature set.
    pub fn negotiateProtocol(self: AdvLink, version: u16, features: u64, wait: Wait) Error!void {
        const nw = wait.native();
        try check(c.hqq_adv_negotiate_protocol(self.raw, version, features, &nw));
    }

    pub fn protocolState(self: AdvLink) ProtocolState {
        return switch (@as(i64, c.hqq_adv_protocol_state(self.raw))) {
            c.HQQ_PROTO_NONE => .none,
            c.HQQ_PROTO_NEGOTIATING => .negotiating,
            c.HQQ_PROTO_NEGOTIATED => .negotiated,
            c.HQQ_PROTO_FAILED => .failed,
            else => unreachable,
        };
    }

    /// Negotiated version word, (major << 8) | minor; 0x0101 is 1.1.
    pub fn negotiatedVersion(self: AdvLink) u16 {
        return c.hqq_adv_negotiated_version(self.raw);
    }

    pub fn negotiatedFeatures(self: AdvLink) u64 {
        return c.hqq_adv_negotiated_features(self.raw);
    }

    pub fn stats(self: AdvLink) Stats {
        var out: Stats = undefined;
        c.hqq_adv_stats_get(self.raw, &out);
        return out;
    }

    /// Stream 0: the link-level stream (messages and requests of the link).
    pub fn stream0(self: AdvLink) Stream {
        return .{ .raw = c.hqq_adv_stream0(self.raw).? };
    }

    pub fn listenStreams(self: AdvLink) Error!void {
        try check(c.hqq_adv_listen_streams(self.raw));
    }

    pub fn openStream(self: AdvLink, wait: Wait) Error!Stream {
        const nw = wait.native();
        var out: ?*c.hqq_stream = null;
        try check(c.hqq_adv_open_stream(self.raw, &out, &nw));
        return .{ .raw = out.? };
    }

    pub fn acceptStream(self: AdvLink, wait: Wait) Error!Stream {
        const nw = wait.native();
        var out: ?*c.hqq_stream = null;
        try check(c.hqq_adv_accept_stream(self.raw, &out, &nw));
        return .{ .raw = out.? };
    }

    /// Serves requests until `wait` ends, the link closes or shuts down (then
    /// returns after in-flight requests complete). `H` must declare
    /// `pub fn handle(self: *H, req: *Request, resp: *Response) void`; it runs
    /// on Serve's worker threads, so it must be thread-safe. Only one Serve per link.
    pub fn serve(self: AdvLink, comptime H: type, handler: *H, wait: Wait) Error!void {
        const Tramp = struct {
            // Parameter types are those of c.hqq_handler_fn; the callback is
            // cast to that type below, so pointer-typed parameters are generic here.
            fn run(user: ?*anyopaque, ctx: ?*anyopaque, w: ?*anyopaque, req: ?*const anyopaque) callconv(.c) void {
                var request = Request{
                    .raw = @ptrCast(@alignCast(req.?)),
                    .ctx = @ptrCast(@alignCast(ctx.?)),
                };
                var response = Response{ .raw = @ptrCast(@alignCast(w.?)) };
                const h: *H = @ptrCast(@alignCast(user.?));
                h.handle(&request, &response);
            }
        };
        const cb: c.hqq_handler_fn = @ptrCast(&Tramp.run);
        const nw = wait.native();
        try check(c.hqq_adv_serve(self.raw, cb, handler, &nw));
    }

    /// Graceful shutdown of the responder role: later requests are refused
    /// with status 8. The link stays usable until closed.
    pub fn shutdown(self: AdvLink, wait: Wait) Error!void {
        const nw = wait.native();
        try check(c.hqq_adv_shutdown(self.raw, &nw));
    }
};

// ---------------------------------------------------------------------------
// Acceptor and client connect (ext/LISTENER_DRAFT.md)
// ---------------------------------------------------------------------------

pub const Acceptor = struct {
    raw: *c.hqq_acceptor,
    closed: bool = false,

    pub fn create(name: [:0]const u8, opts: ?*const AcceptorOptions) Error!Acceptor {
        var out: ?*c.hqq_acceptor = null;
        try check(c.hqq_acceptor_create(name.ptr, if (opts) |o| &o.raw else null, &out, null));
        return .{ .raw = out.? };
    }

    pub fn accept(self: Acceptor, wait: Wait) Error!AdvLink {
        const nw = wait.native();
        var out: ?*c.hqq_adv = null;
        try check(c.hqq_acceptor_accept(self.raw, &out, &nw));
        return .{ .raw = out.? };
    }

    pub fn stats(self: Acceptor) c.hqq_acceptor_stats {
        var out: c.hqq_acceptor_stats = undefined;
        c.hqq_acceptor_stats_get(self.raw, &out);
        return out;
    }

    pub fn close(self: *Acceptor) Error!void {
        if (self.closed) return;
        self.closed = true;
        try check(c.hqq_acceptor_close(self.raw));
    }

    pub fn deinit(self: *Acceptor) void {
        self.close() catch {};
    }
};

/// Client side: attaches to a listener and returns a negotiated Advanced link (Primary).
pub fn connect(listener_name: [:0]const u8, opts: ?*const ConnectOptions, wait: Wait) Error!AdvLink {
    const nw = wait.native();
    var out: ?*c.hqq_adv = null;
    try check(c.hqq_connect(listener_name.ptr, if (opts) |o| &o.raw else null, &nw, &out));
    return .{ .raw = out.? };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

/// Unique segment name: "hqqz-<pid hex>-<time hex>", at most 30 bytes (macOS limit).
fn uniqueName(buf: *[32]u8) ![:0]const u8 {
    const pid: u32 = if (@import("builtin").os.tag == .windows)
        std.os.windows.GetCurrentProcessId()
    else
        @intCast(std.c.getpid());
    const ns: u64 = @truncate(@as(u128, @intCast(std.time.nanoTimestamp())));
    return std.fmt.bufPrintZ(buf, "hqqz-{x}-{x}", .{ pid, ns });
}

test "error mapping is 1:1 and errorIs follows the Go hierarchy" {
    try std.testing.expectEqual(error.Closed, toError(c.HQQ_ERR_CLOSED));
    try std.testing.expectEqual(c.HQQ_ERR_LISTENER_NOT_READY, statusOf(error.ListenerNotReady));
    try check(c.HQQ_OK);
    try std.testing.expectError(error.PeerGone, check(c.HQQ_ERR_PEER_GONE));
    try std.testing.expect(errorIs(error.LinkBusy, error.FailedInit));
    try std.testing.expect(errorIs(error.PeerGone, error.ConnNotFound));
    try std.testing.expect(!errorIs(error.Closed, error.Timeout));
}

test "standard link round trip over a named segment" {
    var name_buf: [32]u8 = undefined;
    const name = try uniqueName(&name_buf);

    var creator = try Link.create(name, 4, 256, 0o600, null);
    defer creator.deinit();
    var opener = try Link.open(name, 4, 256, null);
    defer opener.deinit();

    try creator.write("hello", Wait.forever);
    var buf: [64]u8 = undefined;
    const n = try opener.read(&buf, Wait.forever);
    try std.testing.expectEqualStrings("hello", buf[0..n]);

    try opener.write("world!", Wait.forever);
    const reply = try creator.readAlloc(std.testing.allocator, Wait.forever);
    defer std.testing.allocator.free(reply);
    try std.testing.expectEqualStrings("world!", reply);
}

const EchoHandler = struct {
    fn handle(_: *EchoHandler, req: *Request, resp: *Response) void {
        resp.write(req.data()) catch {};
    }
};

fn negotiateThread(link: *AdvLink, out: *Error!void) void {
    out.* = link.negotiate(0, Wait.forever);
}

fn serveThread(link: *AdvLink, handler: *EchoHandler, out: *Error!void) void {
    // Bounded, so a failing test cannot hang on join.
    out.* = link.serve(EchoHandler, handler, .{ .timeout_ns = 10 * std.time.ns_per_s });
}

test "advanced negotiate, call and serve in process" {
    var name_buf: [32]u8 = undefined;
    const name = try uniqueName(&name_buf);

    var primary = try AdvLink.create(name, 4, 256, 0o600, null);
    defer primary.deinit();
    var secondary = try AdvLink.open(name, 4, 256, null);
    defer secondary.deinit();

    // Both endpoints negotiate concurrently; the Secondary answers the request.
    var neg_result: Error!void = {};
    var neg = try std.Thread.spawn(.{}, negotiateThread, .{ &secondary, &neg_result });
    try primary.negotiate(0, Wait.forever);
    neg.join();
    try neg_result;
    try std.testing.expectEqual(@as(u16, 0x0101), primary.negotiatedVersion());
    try std.testing.expectEqual(@as(u64, 0x0F), primary.negotiatedFeatures());

    var echo: EchoHandler = .{};
    var served: Error!void = {};
    var srv = try std.Thread.spawn(.{}, serveThread, .{ &secondary, &echo, &served });

    var reply = try primary.stream0().call(1, "ping", Wait.forever);
    defer reply.deinit();
    try std.testing.expectEqualStrings("ping", reply.data());

    try secondary.shutdown(Wait.forever);
    srv.join();
    try served;
}
