//! Raw FFI declarations for libhqq (`c/include/hqq/hqq.h`, `hqq_advanced.h`,
//! `hqq_listener.h`), written by hand (no bindgen).
//!
//! The library itself is compiled from the in-tree C sources by `build.rs` and
//! linked statically. Every function is `unsafe`; the safe API is the `hqq`
//! crate. Enum types are `c_int` constants: the values are the C enumerators.
//! Struct layouts mirror the C structs field by field; `hqq_options_init` and
//! the other `*_init` functions write `struct_size`, which the `hqq` crate's
//! tests compare with `size_of` to catch layout drift.
#![allow(non_camel_case_types, non_upper_case_globals, dead_code)]

use std::os::raw::{c_char, c_int, c_uint, c_void};

// ---- Status codes (hqq_status) ---------------------------------------------

pub type hqq_status = c_int;

pub const HQQ_OK: hqq_status = 0;
pub const HQQ_ERR_CLOSED: hqq_status = 1;
pub const HQQ_ERR_PEER_CLOSED: hqq_status = 2;
pub const HQQ_ERR_PEER_GONE: hqq_status = 3;
pub const HQQ_ERR_TIMEOUT: hqq_status = 4;
pub const HQQ_ERR_LINK_BUSY: hqq_status = 5;
pub const HQQ_ERR_INCOMPATIBLE_LAYOUT: hqq_status = 6;
pub const HQQ_ERR_FAILED_INIT: hqq_status = 7;
pub const HQQ_ERR_SEGMENT_NOT_READY: hqq_status = 8;
pub const HQQ_ERR_INIT_CONTENDED: hqq_status = 9;
pub const HQQ_ERR_NO_LIVE_ENDPOINT: hqq_status = 10;
pub const HQQ_ERR_INVALID_SIZE: hqq_status = 11;
pub const HQQ_ERR_GEOMETRY_MISMATCH: hqq_status = 12;
pub const HQQ_ERR_MEMORY_ALIGN: hqq_status = 13;
pub const HQQ_ERR_MEMORY_SMALL: hqq_status = 14;
pub const HQQ_ERR_BUFFER_OVERFLOW: hqq_status = 15;
pub const HQQ_ERR_BUFFER_TOO_SMALL: hqq_status = 16;
pub const HQQ_ERR_CONN_NOT_FOUND: hqq_status = 17;
pub const HQQ_ERR_INVALID_OP: hqq_status = 18;
pub const HQQ_ERR_UNEXPECTED_PACKET: hqq_status = 19;
pub const HQQ_ERR_PROTOCOL: hqq_status = 20;
pub const HQQ_ERR_INVALID_NAME: hqq_status = 21;
pub const HQQ_ERR_NOT_EXIST: hqq_status = 22;
pub const HQQ_ERR_EXISTS: hqq_status = 23;
pub const HQQ_ERR_SYSTEM: hqq_status = 24;
pub const HQQ_ERR_CANCELED: hqq_status = 25;
pub const HQQ_ERR_NO_MEMORY: hqq_status = 26;
pub const HQQ_ERR_INVALID_ARGUMENT: hqq_status = 27;
pub const HQQ_ERR_UNSUPPORTED: hqq_status = 28;
pub const HQQ_ERR_NOT_NEGOTIATED: hqq_status = 29;
pub const HQQ_ERR_ALREADY_NEGOTIATED: hqq_status = 30;
pub const HQQ_ERR_NEGOTIATION_FAILED: hqq_status = 31;
pub const HQQ_ERR_NEGOTIATION_REJECTED: hqq_status = 32;
pub const HQQ_ERR_MESSAGE_ABORTED: hqq_status = 33;
pub const HQQ_ERR_INCOMING_OVERFLOW: hqq_status = 34;
pub const HQQ_ERR_REASSEMBLY_LIMIT: hqq_status = 35;
pub const HQQ_ERR_SHUTTING_DOWN: hqq_status = 36;
pub const HQQ_ERR_RESPONSE: hqq_status = 37;
pub const HQQ_ERR_NOT_LISTENING: hqq_status = 38;
pub const HQQ_ERR_ALREADY_LISTENING: hqq_status = 39;
pub const HQQ_ERR_LISTENER_BUSY: hqq_status = 40;
pub const HQQ_ERR_LISTENER_GONE: hqq_status = 41;
pub const HQQ_ERR_LISTENER_CLOSED: hqq_status = 42;
pub const HQQ_ERR_ACCEPTOR_CLOSED: hqq_status = 43;
pub const HQQ_ERR_LISTENER_NOT_READY: hqq_status = 44;

// Protocol error codes (SPEC 12): tombstone reasons and error-response codes.
pub const HQQ_ERRCODE_MEMORY_ALIGN: c_uint = 0x01;
pub const HQQ_ERRCODE_INVALID_SIZE: c_uint = 0x02;
pub const HQQ_ERRCODE_MEMORY_SMALL: c_uint = 0x03;
pub const HQQ_ERRCODE_FAILED_INIT: c_uint = 0x04;
pub const HQQ_ERRCODE_CONN_NOT_FOUND: c_uint = 0x05;
pub const HQQ_ERRCODE_INVALID_OP: c_uint = 0x06;
pub const HQQ_ERRCODE_BUFFER_OVERFLOW: c_uint = 0x07;
pub const HQQ_ERRCODE_SHUTTING_DOWN: c_uint = 0x08;

// ---- Enumerations (c_int) ----------------------------------------------------

pub const HQQ_MODE_PRIMARY: c_int = 0;
pub const HQQ_MODE_SECONDARY: c_int = 1;

pub const HQQ_PEER_NOT_CONNECTED: c_int = 0;
pub const HQQ_PEER_CONNECTED: c_int = 1;
pub const HQQ_PEER_CLOSED: c_int = 2;
pub const HQQ_PEER_GONE: c_int = 3;

pub const HQQ_WAIT_ADAPTIVE: c_int = 0;
pub const HQQ_WAIT_SPIN: c_int = 1;
pub const HQQ_WAIT_SIMPLE: c_int = 2;

pub const HQQ_FLAG_DARWIN_FORCE_ULOCK: c_uint = 0x1;
pub const HQQ_FLAG_DARWIN_FORCE_OSSYNC: c_uint = 0x2;

pub const HQQ_OVERFLOW_BLOCK: c_int = 0;
pub const HQQ_OVERFLOW_DROP: c_int = 1;
pub const HQQ_OVERFLOW_FAIL: c_int = 2;

pub const HQQ_PROTO_NONE: c_int = 0;
pub const HQQ_PROTO_NEGOTIATING: c_int = 1;
pub const HQQ_PROTO_NEGOTIATED: c_int = 2;
pub const HQQ_PROTO_FAILED: c_int = 3;

// Feature bits and protocol versions (hqq_advanced.h).
pub const HQQ_PROTOCOL_1_0: u16 = 0x0100;
pub const HQQ_PROTOCOL_1_1: u16 = 0x0101;
pub const HQQ_FEATURE_MESSAGES: u64 = 0x01;
pub const HQQ_FEATURE_REQUEST_RESPONSE: u64 = 0x02;
pub const HQQ_FEATURE_CANCEL: u64 = 0x04;
pub const HQQ_FEATURE_METADATA: u64 = 0x08;
pub const HQQ_FEATURES_ALL: u64 = 0x0F;

// Defaults (hqq.h, hqq_advanced.h, hqq_listener.h).
pub const HQQ_MIN_BUFFER_COUNT: u64 = 2;
pub const HQQ_MAX_BUFFER_COUNT: u64 = 1 << 24;
pub const HQQ_MIN_BUFFER_SIZE: u64 = 8;
pub const HQQ_MAX_BUFFER_SIZE: u64 = 1 << 30;
pub const HQQ_ADV_DEFAULT_MAX_MESSAGE_SIZE: u64 = 64 << 20;

// ---- Opaque handles ------------------------------------------------------------

macro_rules! opaque {
    ($($name:ident)*) => {$(
        #[repr(C)]
        pub struct $name {
            _private: [u8; 0],
        }
    )*};
}

opaque!(hqq_link hqq_adv hqq_stream hqq_msg hqq_request hqq_req_ctx hqq_response hqq_acceptor);

// ---- Plain structs (layout mirrors the C structs) ---------------------------

#[repr(C)]
#[derive(Clone, Copy, Debug, Default)]
pub struct hqq_geometry {
    pub buffer_count: u64,
    pub buffer_size: u64,
    pub layout_size: u64,
    pub page_size: u64,
}

#[repr(C)]
#[derive(Clone, Copy, Debug, Default)]
pub struct hqq_cancel {
    pub state: u32,
    pub reserved: [u32; 3],
}

#[repr(C)]
#[derive(Clone, Copy, Debug)]
pub struct hqq_wait {
    pub timeout_ns: i64,
    pub cancel: *const hqq_cancel,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct hqq_allocator {
    pub ctx: *mut c_void,
    pub alloc: Option<unsafe extern "C" fn(ctx: *mut c_void, size: usize) -> *mut c_void>,
    pub free: Option<unsafe extern "C" fn(ctx: *mut c_void, ptr: *mut c_void, size: usize)>,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct hqq_options {
    pub struct_size: usize,
    pub wait_policy: c_int,
    pub flags: u32,
    pub heartbeat_interval_ns: i64,
    pub peer_timeout_ns: i64,
    pub attach_timeout_ns: i64,
    pub open_timeout_ns: i64,
    pub init_stale_ns: i64,
    pub cancel: *const hqq_cancel,
    pub allocator: *const hqq_allocator,
}

#[repr(C)]
#[derive(Clone, Copy, Debug, Default)]
pub struct hqq_open_info {
    pub struct_size: usize,
    pub want: hqq_geometry,
    pub got: hqq_geometry,
    pub os_error: c_int,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct hqq_adv_options {
    pub struct_size: usize,
    pub max_message_size: u64,
    pub max_reassembly_bytes: u64,
    pub queue_size: u64,
    pub overflow: c_int,
    pub link: hqq_options,
    pub stream_listen_grace_ns: i64,
}

#[repr(C)]
#[derive(Clone, Copy, Debug, Default)]
pub struct hqq_adv_stats {
    pub negotiation_ns: u64,
    pub messages: u64,
    pub message_bytes: u64,
    pub requests: u64,
    pub responses: u64,
    pub dropped_messages: u64,
    pub abandoned_responses: u64,
    pub canceled_requests: u64,
    pub refused_requests: u64,
}

#[repr(C)]
#[derive(Clone, Copy, Debug)]
pub struct hqq_call_options {
    pub struct_size: usize,
    pub caller: u64,
    pub trace: [u8; 16],
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct hqq_acceptor_options {
    pub struct_size: usize,
    pub perm: c_uint,
    pub queue_size: u64,
    pub features: u64,
    pub max_buffer_count: u64,
    pub max_buffer_size: u64,
    pub max_link_size: u64,
    pub backlog: u64,
    pub max_handshakes: u64,
    pub handshake_timeout_ns: i64,
    pub stuck_slot_timeout_ns: i64,
    pub link: hqq_adv_options,
}

#[repr(C)]
#[derive(Clone, Copy, Debug, Default)]
pub struct hqq_acceptor_stats {
    pub records: u64,
    pub accepted: u64,
    pub malformed: u64,
    pub unreachable: u64,
    pub handshake_failed: u64,
    pub stuck_slots: u64,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct hqq_connect_options {
    pub struct_size: usize,
    pub buffer_count: u64,
    pub buffer_size: u64,
    pub perm: c_uint,
    pub features: u64,
    pub link: hqq_adv_options,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct hqq_write_reservation {
    pub link: *mut hqq_link,
    pub pos: u64,
    pub copy_id: u64,
    pub data: *mut c_void,
    pub capacity: usize,
    pub slot: u32,
    pub reserved: u32,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct hqq_read_reservation {
    pub link: *mut hqq_link,
    pub pos: u64,
    pub data: *const c_void,
    pub size: usize,
    pub slot: u32,
    pub reserved: u32,
}

// ---- Callback types -----------------------------------------------------------

pub type hqq_read_fn =
    unsafe extern "C" fn(user: *mut c_void, data: *const c_void, len: usize) -> hqq_status;
pub type hqq_write_fn = unsafe extern "C" fn(
    user: *mut c_void,
    buf: *mut c_void,
    cap: usize,
    n: *mut usize,
) -> hqq_status;
pub type hqq_handler_fn = unsafe extern "C" fn(
    user: *mut c_void,
    ctx: *mut hqq_req_ctx,
    w: *mut hqq_response,
    req: *const hqq_request,
);

// ---- Functions ------------------------------------------------------------------

extern "C" {
    // hqq.h: status, version, smoke test
    pub fn hqq_strerror(status: hqq_status) -> *const c_char;
    pub fn hqq_error_is(err: hqq_status, base: hqq_status) -> c_int;
    pub fn hqq_error_is_transient_open(err: hqq_status) -> c_int;
    pub fn hqq_version_string() -> *const c_char;
    pub fn hqq_version_number() -> c_uint;
    pub fn hqq_selftest_atomics() -> c_int;

    // hqq.h: cancellation, options, open info
    pub fn hqq_cancel_init(token: *mut hqq_cancel);
    pub fn hqq_cancel_signal(token: *mut hqq_cancel);
    pub fn hqq_cancel_is_signalled(token: *const hqq_cancel) -> c_int;
    pub fn hqq_options_init(opts: *mut hqq_options);
    pub fn hqq_open_info_init(info: *mut hqq_open_info);

    // hqq.h: standard link
    pub fn hqq_standard_size(buffer_count: u64, buffer_size: u64) -> usize;
    pub fn hqq_standard_open_memory(
        mem: *mut c_void,
        mem_size: usize,
        buffer_count: u64,
        buffer_size: u64,
        opts: *const hqq_options,
        out: *mut *mut hqq_link,
        info: *mut hqq_open_info,
    ) -> hqq_status;
    pub fn hqq_standard_create(
        name: *const c_char,
        buffer_count: u64,
        buffer_size: u64,
        perm: c_uint,
        opts: *const hqq_options,
        out: *mut *mut hqq_link,
        info: *mut hqq_open_info,
    ) -> hqq_status;
    pub fn hqq_standard_open(
        name: *const c_char,
        buffer_count: u64,
        buffer_size: u64,
        opts: *const hqq_options,
        out: *mut *mut hqq_link,
        info: *mut hqq_open_info,
    ) -> hqq_status;
    pub fn hqq_write(
        link: *mut hqq_link,
        data: *const c_void,
        len: usize,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_read(
        link: *mut hqq_link,
        buf: *mut c_void,
        cap: usize,
        len: *mut usize,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_read_zc(
        link: *mut hqq_link,
        f: hqq_read_fn,
        user: *mut c_void,
        len: *mut usize,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_write_zc(
        link: *mut hqq_link,
        f: hqq_write_fn,
        user: *mut c_void,
        len: *mut usize,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_write_reserve(
        link: *mut hqq_link,
        res: *mut hqq_write_reservation,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_write_commit(res: *mut hqq_write_reservation, len: usize) -> hqq_status;
    pub fn hqq_write_abort(res: *mut hqq_write_reservation, reason: c_uint) -> hqq_status;
    pub fn hqq_read_reserve(
        link: *mut hqq_link,
        res: *mut hqq_read_reservation,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_read_release(res: *mut hqq_read_reservation) -> hqq_status;
    pub fn hqq_peer_state(link: *mut hqq_link) -> c_int;
    pub fn hqq_link_mode(link: *const hqq_link) -> c_int;
    pub fn hqq_link_buffer_size(link: *const hqq_link) -> usize;
    pub fn hqq_link_buffer_count(link: *const hqq_link) -> usize;
    pub fn hqq_link_name(link: *const hqq_link) -> *const c_char;
    pub fn hqq_shutdown(link: *mut hqq_link);
    pub fn hqq_close(link: *mut hqq_link) -> hqq_status;
    pub fn hqq_link_unlink(link: *mut hqq_link) -> hqq_status;
    pub fn hqq_unlink(name: *const c_char) -> hqq_status;

    // hqq_advanced.h: options, opening, closing
    pub fn hqq_adv_options_init(opts: *mut hqq_adv_options);
    pub fn hqq_adv_stats_get(link: *const hqq_adv, out: *mut hqq_adv_stats);
    pub fn hqq_adv_create(
        name: *const c_char,
        buffer_count: u64,
        buffer_size: u64,
        perm: c_uint,
        opts: *const hqq_adv_options,
        out: *mut *mut hqq_adv,
        info: *mut hqq_open_info,
    ) -> hqq_status;
    pub fn hqq_adv_open(
        name: *const c_char,
        buffer_count: u64,
        buffer_size: u64,
        opts: *const hqq_adv_options,
        out: *mut *mut hqq_adv,
        info: *mut hqq_open_info,
    ) -> hqq_status;
    pub fn hqq_adv_open_memory(
        mem: *mut c_void,
        mem_size: usize,
        buffer_count: u64,
        buffer_size: u64,
        opts: *const hqq_adv_options,
        out: *mut *mut hqq_adv,
        info: *mut hqq_open_info,
    ) -> hqq_status;
    pub fn hqq_adv_close(link: *mut hqq_adv) -> hqq_status;
    pub fn hqq_adv_unlink(link: *mut hqq_adv) -> hqq_status;
    pub fn hqq_adv_base(link: *mut hqq_adv) -> *mut hqq_link;
    pub fn hqq_adv_mode(link: *const hqq_adv) -> c_int;
    pub fn hqq_adv_peer_state(link: *mut hqq_adv) -> c_int;
    pub fn hqq_adv_buffer_size(link: *const hqq_adv) -> usize;
    pub fn hqq_adv_buffer_count(link: *const hqq_adv) -> usize;

    // hqq_advanced.h: negotiation and streams
    pub fn hqq_adv_negotiate(
        link: *mut hqq_adv,
        features: u64,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_adv_negotiate_protocol(
        link: *mut hqq_adv,
        version: u16,
        features: u64,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_adv_protocol_state(link: *mut hqq_adv) -> c_int;
    pub fn hqq_adv_negotiated_version(link: *const hqq_adv) -> u16;
    pub fn hqq_adv_negotiated_features(link: *const hqq_adv) -> u64;
    pub fn hqq_adv_stream0(link: *mut hqq_adv) -> *mut hqq_stream;
    pub fn hqq_adv_listen_streams(link: *mut hqq_adv) -> hqq_status;
    pub fn hqq_adv_accept_stream(
        link: *mut hqq_adv,
        out: *mut *mut hqq_stream,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_adv_open_stream(
        link: *mut hqq_adv,
        out: *mut *mut hqq_stream,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_stream_id(s: *const hqq_stream) -> u64;
    pub fn hqq_stream_close(s: *mut hqq_stream) -> hqq_status;

    // hqq_advanced.h: messages and calls
    pub fn hqq_stream_send(
        s: *mut hqq_stream,
        data: *const c_void,
        len: usize,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_stream_recv(
        s: *mut hqq_stream,
        out: *mut *mut hqq_msg,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_msg_data(m: *const hqq_msg) -> *const c_void;
    pub fn hqq_msg_len(m: *const hqq_msg) -> usize;
    pub fn hqq_msg_stream_id(m: *const hqq_msg) -> u64;
    pub fn hqq_msg_status(m: *const hqq_msg) -> u64;
    pub fn hqq_msg_release(m: *mut hqq_msg);
    pub fn hqq_call_options_init(opts: *mut hqq_call_options);
    pub fn hqq_stream_call(
        s: *mut hqq_stream,
        method: u64,
        req: *const c_void,
        len: usize,
        resp: *mut *mut hqq_msg,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_stream_call_with(
        s: *mut hqq_stream,
        method: u64,
        req: *const c_void,
        len: usize,
        opts: *const hqq_call_options,
        resp: *mut *mut hqq_msg,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_adv_serve(
        link: *mut hqq_adv,
        f: hqq_handler_fn,
        user: *mut c_void,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_request_method(req: *const hqq_request) -> u64;
    pub fn hqq_request_data(req: *const hqq_request) -> *const c_void;
    pub fn hqq_request_len(req: *const hqq_request) -> usize;
    pub fn hqq_request_stream_id(req: *const hqq_request) -> u64;
    pub fn hqq_request_caller(req: *const hqq_request) -> u64;
    pub fn hqq_request_trace(req: *const hqq_request, out: *mut u8) -> c_int;
    pub fn hqq_response_write(w: *mut hqq_response, data: *const c_void, len: usize) -> hqq_status;
    pub fn hqq_response_error(
        w: *mut hqq_response,
        status: u64,
        data: *const c_void,
        len: usize,
    ) -> hqq_status;
    pub fn hqq_req_ctx_err(ctx: *const hqq_req_ctx) -> hqq_status;
    pub fn hqq_req_ctx_wait_done(ctx: *mut hqq_req_ctx, timeout_ns: i64) -> hqq_status;
    pub fn hqq_req_ctx_deadline(ctx: *const hqq_req_ctx, mono_ns: *mut u64) -> c_int;
    pub fn hqq_adv_shutdown(link: *mut hqq_adv, wait: *const hqq_wait) -> hqq_status;

    // hqq_listener.h: acceptor and connect
    pub fn hqq_acceptor_options_init(opts: *mut hqq_acceptor_options);
    pub fn hqq_connect_options_init(opts: *mut hqq_connect_options);
    pub fn hqq_acceptor_create(
        name: *const c_char,
        opts: *const hqq_acceptor_options,
        out: *mut *mut hqq_acceptor,
        info: *mut hqq_open_info,
    ) -> hqq_status;
    pub fn hqq_acceptor_accept(
        acc: *mut hqq_acceptor,
        out: *mut *mut hqq_adv,
        wait: *const hqq_wait,
    ) -> hqq_status;
    pub fn hqq_acceptor_close(acc: *mut hqq_acceptor) -> hqq_status;
    pub fn hqq_acceptor_stats_get(acc: *const hqq_acceptor, out: *mut hqq_acceptor_stats);
    pub fn hqq_connect(
        listener_name: *const c_char,
        opts: *const hqq_connect_options,
        wait: *const hqq_wait,
        out: *mut *mut hqq_adv,
    ) -> hqq_status;
}
