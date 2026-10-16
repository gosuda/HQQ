//! The Advanced layer (`hqq_advanced.h`): negotiation, streams, messages,
//! request/response calls and serving.

use std::ffi::{c_void, CStr};
use std::marker::PhantomData;
use std::mem::{forget, zeroed};
use std::os::raw::c_int;
use std::panic::{self, AssertUnwindSafe};
use std::ptr::NonNull;
use std::slice;
use std::time::Duration;

use hqq_sys as sys;

use crate::error::{check, Error, Result};
use crate::options::{c_name, AdvOptions, OpenInfo};
use crate::standard::{Mode, PeerState};
use crate::wait::{duration_ns, Wait};

/// Every feature of protocol 1.1 (`HQQ_FEATURES_ALL`).
pub const FEATURES_ALL: u64 = sys::HQQ_FEATURES_ALL;
/// Plain messages (`HQQ_FEATURE_MESSAGES`).
pub const FEATURE_MESSAGES: u64 = sys::HQQ_FEATURE_MESSAGES;
/// Request/response calls (`HQQ_FEATURE_REQUEST_RESPONSE`).
pub const FEATURE_REQUEST_RESPONSE: u64 = sys::HQQ_FEATURE_REQUEST_RESPONSE;
/// OpCancel (`HQQ_FEATURE_CANCEL`, 1.1).
pub const FEATURE_CANCEL: u64 = sys::HQQ_FEATURE_CANCEL;
/// Request metadata (`HQQ_FEATURE_METADATA`, 1.1).
pub const FEATURE_METADATA: u64 = sys::HQQ_FEATURE_METADATA;

/// Negotiation state of an Advanced link (`hqq_proto_state`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ProtocolState {
    NotStarted,
    Negotiating,
    Negotiated,
    /// Terminal: close the link and open a new one.
    Failed,
}

/// Counters of an Advanced link (`hqq_adv_stats`).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct AdvStats {
    pub negotiation_ns: u64,
    /// Messages sent with `Stream::send`.
    pub messages: u64,
    pub message_bytes: u64,
    pub requests: u64,
    pub responses: u64,
    pub dropped_messages: u64,
    pub abandoned_responses: u64,
    pub canceled_requests: u64,
    pub refused_requests: u64,
}

/// An Advanced link (`hqq_adv`). Owned: closed on drop.
///
/// Like [`Link`](crate::Link), every method takes `&self` and may run on any
/// thread. Streams borrow the link, so the link cannot close while they exist.
pub struct AdvLink {
    raw: NonNull<sys::hqq_adv>,
}

// SAFETY: hqq_advanced.h: every function may be called from any thread, concurrently
// with any other function on the same link (close is the only exception, and it takes the value).
unsafe impl Send for AdvLink {}
unsafe impl Sync for AdvLink {}

impl AdvLink {
    /// Creates the named Advanced link as the Primary (`hqq_adv_create`).
    pub fn create(
        name: &str,
        buffer_count: u64,
        buffer_size: u64,
        perm: u32,
        opts: &AdvOptions,
        info: Option<&mut OpenInfo>,
    ) -> Result<AdvLink> {
        let name = c_name(name)?;
        let mut out = std::ptr::null_mut();
        // SAFETY: `name` is NUL-terminated for the call; out pointers are valid.
        let status = unsafe {
            sys::hqq_adv_create(
                name.as_ptr(),
                buffer_count,
                buffer_size,
                perm,
                opts.as_ptr(),
                &mut out,
                OpenInfo::as_mut_ptr(info),
            )
        };
        AdvLink::from_status(status, out)
    }

    /// Attaches to an existing named Advanced link as the Secondary (`hqq_adv_open`).
    pub fn open(
        name: &str,
        buffer_count: u64,
        buffer_size: u64,
        opts: &AdvOptions,
        info: Option<&mut OpenInfo>,
    ) -> Result<AdvLink> {
        let name = c_name(name)?;
        let mut out = std::ptr::null_mut();
        // SAFETY: as in `create`.
        let status = unsafe {
            sys::hqq_adv_open(
                name.as_ptr(),
                buffer_count,
                buffer_size,
                opts.as_ptr(),
                &mut out,
                OpenInfo::as_mut_ptr(info),
            )
        };
        AdvLink::from_status(status, out)
    }

    /// Caller-owned memory variant of `open` (`hqq_adv_open_memory`).
    ///
    /// # Safety
    /// The memory rules of [`Link::open_memory`](crate::Link::open_memory) apply.
    pub unsafe fn open_memory(
        mem: *mut c_void,
        mem_size: usize,
        buffer_count: u64,
        buffer_size: u64,
        opts: &AdvOptions,
        info: Option<&mut OpenInfo>,
    ) -> Result<AdvLink> {
        let mut out = std::ptr::null_mut();
        let status = sys::hqq_adv_open_memory(
            mem,
            mem_size,
            buffer_count,
            buffer_size,
            opts.as_ptr(),
            &mut out,
            OpenInfo::as_mut_ptr(info),
        );
        AdvLink::from_status(status, out)
    }

    pub(crate) fn from_status(status: c_int, out: *mut sys::hqq_adv) -> Result<AdvLink> {
        check(status)?;
        NonNull::new(out)
            .map(|raw| AdvLink { raw })
            .ok_or(Error::InvalidArgument)
    }

    /// Negotiates with every feature in `features` (`hqq_adv_negotiate`). Both
    /// endpoints call it; the Primary sends the request. Blocks until done.
    pub fn negotiate(&self, features: u64, wait: Wait<'_>) -> Result<()> {
        let wait = wait.raw();
        // SAFETY: `wait` outlives the call.
        check(unsafe { sys::hqq_adv_negotiate(self.as_ptr(), features, &wait) })
    }

    /// Negotiates the feature set of protocol 1.1 (every feature).
    pub fn negotiate_all(&self, wait: Wait<'_>) -> Result<()> {
        self.negotiate(FEATURES_ALL, wait)
    }

    /// Negotiates with an explicit version word (`(major << 8) | minor`), for version tests.
    pub fn negotiate_protocol(&self, version: u16, features: u64, wait: Wait<'_>) -> Result<()> {
        let wait = wait.raw();
        // SAFETY: as in `negotiate`.
        check(unsafe { sys::hqq_adv_negotiate_protocol(self.as_ptr(), version, features, &wait) })
    }

    pub fn protocol_state(&self) -> ProtocolState {
        // SAFETY: read-only query on a live link.
        match unsafe { sys::hqq_adv_protocol_state(self.as_ptr()) } {
            sys::HQQ_PROTO_NEGOTIATING => ProtocolState::Negotiating,
            sys::HQQ_PROTO_NEGOTIATED => ProtocolState::Negotiated,
            sys::HQQ_PROTO_FAILED => ProtocolState::Failed,
            _ => ProtocolState::NotStarted,
        }
    }

    /// The negotiated version word, `(major << 8) | minor`.
    pub fn negotiated_version(&self) -> u16 {
        // SAFETY: read-only query on a live link.
        unsafe { sys::hqq_adv_negotiated_version(self.as_ptr()) }
    }

    pub fn negotiated_features(&self) -> u64 {
        // SAFETY: read-only query on a live link.
        unsafe { sys::hqq_adv_negotiated_features(self.as_ptr()) }
    }

    /// Stream 0, owned by the link (used for calls and messages without a stream of their own).
    pub fn stream0(&self) -> Stream<'_> {
        // SAFETY: the link is live; hqq_adv_stream0 is valid from creation on.
        let raw = unsafe { sys::hqq_adv_stream0(self.as_ptr()) };
        Stream::from_raw(raw).expect("hqq_adv_stream0 returned NULL on a live link")
    }

    /// Enables incoming streams (`hqq_adv_listen_streams`). Once per link.
    pub fn listen_streams(&self) -> Result<()> {
        // SAFETY: hqq_adv_listen_streams is thread-safe on a live link.
        check(unsafe { sys::hqq_adv_listen_streams(self.as_ptr()) })
    }

    /// Waits for an incoming stream (its open was already accepted).
    pub fn accept_stream(&self, wait: Wait<'_>) -> Result<Stream<'_>> {
        let wait = wait.raw();
        let mut out = std::ptr::null_mut();
        // SAFETY: out is a valid out pointer; wait outlives the call.
        check(unsafe { sys::hqq_adv_accept_stream(self.as_ptr(), &mut out, &wait) })?;
        Stream::from_raw(out).ok_or(Error::InvalidArgument)
    }

    /// Opens a stream of this endpoint's parity; waits for the peer's accept.
    pub fn open_stream(&self, wait: Wait<'_>) -> Result<Stream<'_>> {
        let wait = wait.raw();
        let mut out = std::ptr::null_mut();
        // SAFETY: as in `accept_stream`.
        check(unsafe { sys::hqq_adv_open_stream(self.as_ptr(), &mut out, &wait) })?;
        Stream::from_raw(out).ok_or(Error::InvalidArgument)
    }

    /// Serves requests until `wait` ends, the link is closed or shut down
    /// (then `Ok` once in-flight requests completed), or the receive path fails.
    ///
    /// `handler` runs on worker threads, concurrently, hence `Sync`. A panic in
    /// the handler is caught and answered with an error response (status
    /// `HQQ_ERRCODE_INVALID_OP`); it never unwinds into the C library.
    pub fn serve<H>(&self, handler: H, wait: Wait<'_>) -> Result<()>
    where
        H: Fn(&Request<'_>, &mut Responder<'_>) + Sync,
    {
        let wait = wait.raw();
        // SAFETY: `handler` outlives the call (serve does not return before the workers finish),
        // and the trampoline is instantiated for exactly this type.
        check(unsafe {
            sys::hqq_adv_serve(
                self.as_ptr(),
                serve_trampoline::<H>,
                (&handler as *const H).cast_mut().cast(),
                &wait,
            )
        })
    }

    /// Graceful shutdown of the responder role: later requests are refused with
    /// status 8; returns once in-flight requests completed or `wait` ended.
    pub fn shutdown(&self, wait: Wait<'_>) -> Result<()> {
        let wait = wait.raw();
        // SAFETY: as in `negotiate`.
        check(unsafe { sys::hqq_adv_shutdown(self.as_ptr(), &wait) })
    }

    pub fn peer_state(&self) -> PeerState {
        // SAFETY: hqq_adv_peer_state is thread-safe on a live link.
        match unsafe { sys::hqq_adv_peer_state(self.as_ptr()) } {
            sys::HQQ_PEER_CONNECTED => PeerState::Connected,
            sys::HQQ_PEER_CLOSED => PeerState::Closed,
            sys::HQQ_PEER_GONE => PeerState::Gone,
            _ => PeerState::NotConnected,
        }
    }

    pub fn mode(&self) -> Mode {
        // SAFETY: read-only query on a live link.
        if unsafe { sys::hqq_adv_mode(self.as_ptr()) } == sys::HQQ_MODE_PRIMARY {
            Mode::Primary
        } else {
            Mode::Secondary
        }
    }

    pub fn buffer_size(&self) -> usize {
        // SAFETY: read-only query on a live link.
        unsafe { sys::hqq_adv_buffer_size(self.as_ptr()) }
    }

    pub fn buffer_count(&self) -> usize {
        // SAFETY: read-only query on a live link.
        unsafe { sys::hqq_adv_buffer_count(self.as_ptr()) }
    }

    pub fn stats(&self) -> AdvStats {
        // SAFETY: a zeroed stats struct is a valid out buffer for hqq_adv_stats_get.
        let mut raw: sys::hqq_adv_stats = unsafe { zeroed() };
        unsafe { sys::hqq_adv_stats_get(self.as_ptr(), &mut raw) };
        AdvStats {
            negotiation_ns: raw.negotiation_ns,
            messages: raw.messages,
            message_bytes: raw.message_bytes,
            requests: raw.requests,
            responses: raw.responses,
            dropped_messages: raw.dropped_messages,
            abandoned_responses: raw.abandoned_responses,
            canceled_requests: raw.canceled_requests,
            refused_requests: raw.refused_requests,
        }
    }

    /// Removes the name of a named link when it is still this endpoint's instance.
    pub fn unlink(&self) -> Result<()> {
        // SAFETY: thread-safe on a live link.
        check(unsafe { sys::hqq_adv_unlink(self.as_ptr()) })
    }

    /// Closes the link: streams and messages become invalid, blocked calls fail
    /// with `Closed`, and the name is removed when this endpoint created it.
    /// Must not be called from a serve handler.
    pub fn close(self) -> Result<()> {
        let raw = self.raw.as_ptr();
        forget(self);
        // SAFETY: the handle is consumed, so no other use can follow.
        check(unsafe { sys::hqq_adv_close(raw) })
    }

    /// The raw handle, for interop with the C API. Valid while `self` lives.
    pub fn as_ptr(&self) -> *mut sys::hqq_adv {
        self.raw.as_ptr()
    }
}

impl Drop for AdvLink {
    fn drop(&mut self) {
        // SAFETY: the handle is owned and not used after this point.
        unsafe { sys::hqq_adv_close(self.raw.as_ptr()) };
    }
}

/// A stream of an Advanced link (`hqq_stream`). A borrowed, copyable handle: it
/// stays valid until the link is closed. Stream 0 is [`AdvLink::stream0`].
#[derive(Clone, Copy)]
pub struct Stream<'a> {
    raw: NonNull<sys::hqq_stream>,
    _link: PhantomData<&'a AdvLink>,
}

// SAFETY: the handle is a pointer into the link, which is Sync; every stream
// function may run on any thread (hqq_advanced.h).
unsafe impl Send for Stream<'_> {}
unsafe impl Sync for Stream<'_> {}

impl<'a> Stream<'a> {
    fn from_raw(raw: *mut sys::hqq_stream) -> Option<Stream<'a>> {
        NonNull::new(raw).map(|raw| Stream {
            raw,
            _link: PhantomData,
        })
    }

    pub fn id(&self) -> u64 {
        // SAFETY: the stream belongs to a live link (lifetime 'a).
        unsafe { sys::hqq_stream_id(self.raw.as_ptr()) }
    }

    /// Sends `data` as one message, chunked to the buffer size; empty is allowed.
    pub fn send(&self, data: &[u8], wait: Wait<'_>) -> Result<()> {
        let wait = wait.raw();
        // SAFETY: `data` is valid for `len` bytes; `wait` outlives the call.
        check(unsafe {
            sys::hqq_stream_send(self.raw.as_ptr(), data.as_ptr().cast(), data.len(), &wait)
        })
    }

    /// Receives the next data message of the stream.
    pub fn recv(&self, wait: Wait<'_>) -> Result<Message<'a>> {
        let wait = wait.raw();
        let mut out = std::ptr::null_mut();
        // SAFETY: out is a valid out pointer.
        check(unsafe { sys::hqq_stream_recv(self.raw.as_ptr(), &mut out, &wait) })?;
        Message::from_raw(out).ok_or(Error::InvalidArgument)
    }

    /// Calls `method` with `request` and waits for the response.
    pub fn call(
        &self,
        method: u64,
        request: &[u8],
        wait: Wait<'_>,
    ) -> std::result::Result<Message<'a>, CallError<'a>> {
        self.call_with(method, request, &CallOptions::new(), wait)
    }

    /// As [`call`](Self::call), with request metadata (caller ID, trace ID).
    pub fn call_with(
        &self,
        method: u64,
        request: &[u8],
        options: &CallOptions,
        wait: Wait<'_>,
    ) -> std::result::Result<Message<'a>, CallError<'a>> {
        let wait = wait.raw();
        let mut out = std::ptr::null_mut();
        // SAFETY: request is valid for `len` bytes; options and wait outlive the call.
        let status = unsafe {
            sys::hqq_stream_call_with(
                self.raw.as_ptr(),
                method,
                request.as_ptr().cast(),
                request.len(),
                &options.raw,
                &mut out,
                &wait,
            )
        };
        let response = Message::from_raw(out);
        match (Error::from_code(status), response) {
            (None, Some(message)) => Ok(message),
            (None, None) => Err(CallError {
                error: Error::InvalidArgument,
                response: None,
            }),
            (Some(error), response) => Err(CallError { error, response }),
        }
    }

    /// Closes the stream (not stream 0). A second close returns `Closed`.
    pub fn close(&self) -> Result<()> {
        // SAFETY: the stream belongs to a live link.
        check(unsafe { sys::hqq_stream_close(self.raw.as_ptr()) })
    }
}

/// A received message (`hqq_msg`), pooled by the link. Released on drop.
pub struct Message<'a> {
    raw: NonNull<sys::hqq_msg>,
    _link: PhantomData<&'a AdvLink>,
}

// SAFETY: messages are owned by one caller at a time, and hqq_msg_release may run on any thread.
unsafe impl Send for Message<'_> {}

impl<'a> Message<'a> {
    fn from_raw(raw: *mut sys::hqq_msg) -> Option<Message<'a>> {
        NonNull::new(raw).map(|raw| Message {
            raw,
            _link: PhantomData,
        })
    }

    /// The payload (for an error response, the error payload).
    pub fn data(&self) -> &[u8] {
        // SAFETY: the message is live until dropped; the payload pointer and length come from the C library.
        unsafe {
            let ptr = sys::hqq_msg_data(self.raw.as_ptr()).cast::<u8>();
            let len = sys::hqq_msg_len(self.raw.as_ptr());
            if len == 0 {
                &[]
            } else {
                slice::from_raw_parts(ptr, len)
            }
        }
    }

    pub fn stream_id(&self) -> u64 {
        // SAFETY: the message is live.
        unsafe { sys::hqq_msg_stream_id(self.raw.as_ptr()) }
    }

    /// The status of an error response; 0 for every other message.
    pub fn status(&self) -> u64 {
        // SAFETY: the message is live.
        unsafe { sys::hqq_msg_status(self.raw.as_ptr()) }
    }
}
impl std::fmt::Debug for Message<'_> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Message")
            .field("stream_id", &self.stream_id())
            .field("status", &self.status())
            .field("len", &self.data().len())
            .finish()
    }
}

impl Drop for Message<'_> {
    fn drop(&mut self) {
        // SAFETY: the message is owned and not used after this point.
        unsafe { sys::hqq_msg_release(self.raw.as_ptr()) };
    }
}

/// Request metadata of a call (`hqq_call_options`): caller ID and trace ID.
#[derive(Clone, Copy)]
pub struct CallOptions {
    raw: sys::hqq_call_options,
}

impl CallOptions {
    pub fn new() -> CallOptions {
        let mut raw: sys::hqq_call_options = unsafe { zeroed() };
        // SAFETY: `raw` is a valid, exclusively borrowed options struct.
        unsafe { sys::hqq_call_options_init(&mut raw) };
        CallOptions { raw }
    }

    /// The application caller ID (sent only when non-zero).
    pub fn caller(mut self, caller: u64) -> CallOptions {
        self.raw.caller = caller;
        self
    }

    /// The application trace ID (sent only when not all zero).
    pub fn trace(mut self, trace: [u8; 16]) -> CallOptions {
        self.raw.trace = trace;
        self
    }
}

impl Default for CallOptions {
    fn default() -> Self {
        CallOptions::new()
    }
}

/// A failed call: the error, and the error response when the peer sent one
/// (`error == Error::Response`; its status is `response.status()`).
pub struct CallError<'a> {
    pub error: Error,
    pub response: Option<Message<'a>>,
}

impl From<CallError<'_>> for Error {
    fn from(err: CallError<'_>) -> Error {
        err.error
    }
}

impl std::fmt::Debug for CallError<'_> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CallError")
            .field("error", &self.error)
            .field("status", &self.response.as_ref().map(Message::status))
            .finish()
    }
}

/// A request as seen by a serve handler (`hqq_request`) and its context
/// (`hqq_req_ctx`): cancellation and deadline. Valid only inside the handler.
pub struct Request<'a> {
    raw: *const sys::hqq_request,
    ctx: *mut sys::hqq_req_ctx,
    _link: PhantomData<&'a ()>,
}

impl Request<'_> {
    pub fn method(&self) -> u64 {
        // SAFETY: the request is valid for the duration of the handler.
        unsafe { sys::hqq_request_method(self.raw) }
    }

    pub fn data(&self) -> &[u8] {
        // SAFETY: as above; the payload is valid while the handler runs.
        unsafe {
            let len = sys::hqq_request_len(self.raw);
            if len == 0 {
                &[]
            } else {
                slice::from_raw_parts(sys::hqq_request_data(self.raw).cast::<u8>(), len)
            }
        }
    }

    pub fn stream_id(&self) -> u64 {
        // SAFETY: as above.
        unsafe { sys::hqq_request_stream_id(self.raw) }
    }

    /// The caller ID of the request metadata (0 when absent).
    pub fn caller(&self) -> u64 {
        // SAFETY: as above.
        unsafe { sys::hqq_request_caller(self.raw) }
    }

    /// The trace ID of the request metadata, if the request carried one.
    pub fn trace(&self) -> Option<[u8; 16]> {
        let mut trace = [0u8; 16];
        // SAFETY: `trace` is a 16-byte out buffer.
        if unsafe { sys::hqq_request_trace(self.raw, trace.as_mut_ptr()) } != 0 {
            Some(trace)
        } else {
            None
        }
    }

    /// `Ok` while the request is live; `Canceled` after OpCancel, `Timeout` past the budget.
    pub fn context_err(&self) -> Result<()> {
        // SAFETY: the context is valid for the duration of the handler.
        check(unsafe { sys::hqq_req_ctx_err(self.ctx) })
    }

    /// Blocks until the request ends or `timeout` passes.
    pub fn wait_done(&self, timeout: Duration) -> Result<()> {
        // SAFETY: as above.
        check(unsafe { sys::hqq_req_ctx_wait_done(self.ctx, duration_ns(timeout)) })
    }

    /// The monotonic deadline of the request (hqq's monotonic clock), if it has one.
    pub fn deadline_ns(&self) -> Option<u64> {
        let mut deadline = 0u64;
        // SAFETY: as above; `deadline` is a valid out pointer.
        if unsafe { sys::hqq_req_ctx_deadline(self.ctx, &mut deadline) } != 0 {
            Some(deadline)
        } else {
            None
        }
    }
}

/// The response writer of one request (`hqq_response`). The response is sent
/// when the handler returns, unless the request was canceled first.
pub struct Responder<'a> {
    raw: *mut sys::hqq_response,
    _link: PhantomData<&'a ()>,
}

impl Responder<'_> {
    /// Appends `data` to the response body (at most MaxMessageSize in total).
    pub fn write(&mut self, data: &[u8]) -> Result<()> {
        // SAFETY: the writer is valid for the duration of the handler.
        check(unsafe { sys::hqq_response_write(self.raw, data.as_ptr().cast(), data.len()) })
    }

    /// Replaces the body with an error payload and sets the response status.
    pub fn error(&mut self, status: u64, payload: &[u8]) -> Result<()> {
        // SAFETY: as in `write`.
        check(unsafe {
            sys::hqq_response_error(self.raw, status, payload.as_ptr().cast(), payload.len())
        })
    }
}

/// The C callback of `serve`: rebuilds the views and calls the Rust handler.
unsafe extern "C" fn serve_trampoline<H>(
    user: *mut c_void,
    ctx: *mut sys::hqq_req_ctx,
    writer: *mut sys::hqq_response,
    req: *const sys::hqq_request,
) where
    H: Fn(&Request<'_>, &mut Responder<'_>) + Sync,
{
    // SAFETY: `user` is the `&H` passed to hqq_adv_serve, alive until serve returns.
    let handler = &*(user as *const H);
    let request = Request {
        raw: req,
        ctx,
        _link: PhantomData,
    };
    let mut responder = Responder {
        raw: writer,
        _link: PhantomData,
    };
    let outcome = panic::catch_unwind(AssertUnwindSafe(|| handler(&request, &mut responder)));
    if outcome.is_err() {
        // Never unwind into C: answer the request with an error instead.
        const MESSAGE: &[u8] = b"handler panicked";
        sys::hqq_response_error(
            writer,
            u64::from(sys::HQQ_ERRCODE_INVALID_OP),
            MESSAGE.as_ptr().cast(),
            MESSAGE.len(),
        );
    }
}

/// The library version string (`hqq_version_string`).
pub fn version() -> &'static str {
    // SAFETY: a static, NUL-terminated string.
    unsafe { CStr::from_ptr(sys::hqq_version_string()) }
        .to_str()
        .unwrap_or("unknown")
}
