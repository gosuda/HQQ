//! Option builders over the C option structs. Every builder starts from the
//! C defaults (`*_init`), so unset fields keep the library defaults.

use std::ffi::CString;
use std::mem::zeroed;
use std::time::Duration;

use hqq_sys as sys;

use crate::error::{Error, Result};
use crate::wait::duration_ns;

/// Wait policy of a link (`hqq_wait_policy`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WaitPolicy {
    /// Spin, yield, then park on a kernel wait (the default).
    Adaptive,
    /// Never park; lowest latency, one busy core while blocked.
    Spin,
    /// Spin, yield, then sleep with exponential backoff (20 us .. 2 ms).
    Simple,
}

/// What to do when a routed message's queue is full (`hqq_overflow`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Overflow {
    /// Backpressure: the receive path waits (the default).
    Block,
    /// Drop the message; a stream open is refused.
    Drop,
    /// Fail the receive path with `IncomingOverflow`.
    Fail,
}

/// Options of a Standard link (`hqq_options`).
#[derive(Clone, Copy)]
pub struct Options {
    pub(crate) raw: sys::hqq_options,
}

impl Options {
    /// The C defaults.
    pub fn new() -> Options {
        Options {
            raw: init_options(),
        }
    }

    pub fn wait_policy(mut self, policy: WaitPolicy) -> Options {
        self.raw.wait_policy = match policy {
            WaitPolicy::Adaptive => sys::HQQ_WAIT_ADAPTIVE,
            WaitPolicy::Spin => sys::HQQ_WAIT_SPIN,
            WaitPolicy::Simple => sys::HQQ_WAIT_SIMPLE,
        };
        self
    }

    /// Heartbeat interval (zero selects the 250 ms default).
    pub fn heartbeat_interval(mut self, interval: Duration) -> Options {
        self.raw.heartbeat_interval_ns = duration_ns(interval);
        self
    }

    /// Disables heartbeats: the peer cannot detect this endpoint's crash.
    pub fn disable_heartbeat(mut self) -> Options {
        self.raw.heartbeat_interval_ns = -1;
        self
    }

    /// How long the peer's heartbeat may stall before `PeerGone` (zero: 5 s).
    pub fn peer_timeout(mut self, timeout: Duration) -> Options {
        self.raw.peer_timeout_ns = duration_ns(timeout);
        self
    }

    /// Bound on waiting for another endpoint that is initializing the segment (zero: 5 s).
    pub fn attach_timeout(mut self, timeout: Duration) -> Options {
        self.raw.attach_timeout_ns = duration_ns(timeout);
        self
    }

    /// Hard bound for the whole open call (zero: none).
    pub fn open_timeout(mut self, timeout: Duration) -> Options {
        self.raw.open_timeout_ns = duration_ns(timeout);
        self
    }

    pub(crate) fn as_ptr(&self) -> *const sys::hqq_options {
        &self.raw
    }
}

impl Default for Options {
    fn default() -> Self {
        Options::new()
    }
}

/// Options of an Advanced link (`hqq_adv_options`).
#[derive(Clone, Copy)]
pub struct AdvOptions {
    pub(crate) raw: sys::hqq_adv_options,
}

impl AdvOptions {
    pub fn new() -> AdvOptions {
        let mut raw: sys::hqq_adv_options = unsafe { zeroed() };
        // SAFETY: `raw` is a valid, exclusively borrowed options struct.
        unsafe { sys::hqq_adv_options_init(&mut raw) };
        AdvOptions { raw }
    }

    /// The options of the underlying Standard link (wait policy, timeouts).
    pub fn link(mut self, link: Options) -> AdvOptions {
        self.raw.link = link.raw;
        self
    }

    /// Largest message accepted or sent (zero: 64 MiB).
    pub fn max_message_size(mut self, bytes: u64) -> AdvOptions {
        self.raw.max_message_size = bytes;
        self
    }

    /// Bound on incomplete multi-chunk messages (zero: 4 x max message size).
    pub fn max_reassembly_bytes(mut self, bytes: u64) -> AdvOptions {
        self.raw.max_reassembly_bytes = bytes;
        self
    }

    /// Capacity of every incoming queue (zero: the buffer count).
    pub fn queue_size(mut self, slots: u64) -> AdvOptions {
        self.raw.queue_size = slots;
        self
    }

    pub fn overflow(mut self, overflow: Overflow) -> AdvOptions {
        self.raw.overflow = match overflow {
            Overflow::Block => sys::HQQ_OVERFLOW_BLOCK,
            Overflow::Drop => sys::HQQ_OVERFLOW_DROP,
            Overflow::Fail => sys::HQQ_OVERFLOW_FAIL,
        };
        self
    }

    /// How long a stream request that arrives before `listen_streams` is held (zero: 2 s).
    pub fn stream_listen_grace(mut self, grace: Duration) -> AdvOptions {
        self.raw.stream_listen_grace_ns = duration_ns(grace);
        self
    }

    /// Refuses a stream request that arrives before `listen_streams` at once.
    pub fn refuse_early_streams(mut self) -> AdvOptions {
        self.raw.stream_listen_grace_ns = -1;
        self
    }

    pub(crate) fn as_ptr(&self) -> *const sys::hqq_adv_options {
        &self.raw
    }
}

impl Default for AdvOptions {
    fn default() -> Self {
        AdvOptions::new()
    }
}

/// Options of an acceptor (`hqq_acceptor_options`).
#[derive(Clone, Copy)]
pub struct AcceptorOptions {
    pub(crate) raw: sys::hqq_acceptor_options,
}

impl AcceptorOptions {
    pub fn new() -> AcceptorOptions {
        let mut raw: sys::hqq_acceptor_options = unsafe { zeroed() };
        // SAFETY: `raw` is a valid, exclusively borrowed options struct.
        unsafe { sys::hqq_acceptor_options_init(&mut raw) };
        AcceptorOptions { raw }
    }

    /// Permission of the listener segment (zero: 0600).
    pub fn perm(mut self, perm: u32) -> AcceptorOptions {
        self.raw.perm = perm;
        self
    }

    /// Options of the links handed out by `accept`.
    pub fn link(mut self, link: AdvOptions) -> AcceptorOptions {
        self.raw.link = link.raw;
        self
    }

    /// Features offered on accepted links (zero: all of protocol 1.1).
    pub fn features(mut self, features: u64) -> AcceptorOptions {
        self.raw.features = features;
        self
    }

    /// Negotiated links waiting for `accept` (zero: 16).
    pub fn backlog(mut self, links: u64) -> AcceptorOptions {
        self.raw.backlog = links;
        self
    }

    /// Concurrent handshakes (zero: 16).
    pub fn max_handshakes(mut self, handshakes: u64) -> AcceptorOptions {
        self.raw.max_handshakes = handshakes;
        self
    }

    pub(crate) fn as_ptr(&self) -> *const sys::hqq_acceptor_options {
        &self.raw
    }
}

impl Default for AcceptorOptions {
    fn default() -> Self {
        AcceptorOptions::new()
    }
}

/// Options of a client connect (`hqq_connect_options`).
#[derive(Clone, Copy)]
pub struct ConnectOptions {
    pub(crate) raw: sys::hqq_connect_options,
}

impl ConnectOptions {
    pub fn new() -> ConnectOptions {
        let mut raw: sys::hqq_connect_options = unsafe { zeroed() };
        // SAFETY: `raw` is a valid, exclusively borrowed options struct.
        unsafe { sys::hqq_connect_options_init(&mut raw) };
        ConnectOptions { raw }
    }

    /// Slots of the client's link segment (zero: 256).
    pub fn buffer_count(mut self, slots: u64) -> ConnectOptions {
        self.raw.buffer_count = slots;
        self
    }

    /// Payload bytes per slot of the client's link (zero: 4096).
    pub fn buffer_size(mut self, bytes: u64) -> ConnectOptions {
        self.raw.buffer_size = bytes;
        self
    }

    /// Permission of the client's link segment (zero: 0600).
    pub fn perm(mut self, perm: u32) -> ConnectOptions {
        self.raw.perm = perm;
        self
    }

    /// Features requested (zero: all of protocol 1.1).
    pub fn features(mut self, features: u64) -> ConnectOptions {
        self.raw.features = features;
        self
    }

    /// Options of the client's link.
    pub fn link(mut self, link: AdvOptions) -> ConnectOptions {
        self.raw.link = link.raw;
        self
    }

    pub(crate) fn as_ptr(&self) -> *const sys::hqq_connect_options {
        &self.raw
    }
}

impl Default for ConnectOptions {
    fn default() -> Self {
        ConnectOptions::new()
    }
}

/// Segment geometry: slot count, payload size, total bytes, page size.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Geometry {
    pub buffer_count: u64,
    pub buffer_size: u64,
    pub layout_size: u64,
    pub page_size: u64,
}

/// Detailed result of an open call (`hqq_open_info`). Pass `Some(&mut info)` to
/// the create, open and connect functions to learn the geometries and the OS
/// error of a failed open.
#[derive(Clone, Copy)]
pub struct OpenInfo {
    raw: sys::hqq_open_info,
}

impl OpenInfo {
    pub fn new() -> OpenInfo {
        let mut raw: sys::hqq_open_info = unsafe { zeroed() };
        // SAFETY: `raw` is a valid, exclusively borrowed info struct.
        unsafe { sys::hqq_open_info_init(&mut raw) };
        OpenInfo { raw }
    }

    /// The geometry the caller asked for (filled on mismatch and on success).
    pub fn want(&self) -> Geometry {
        geometry(self.raw.want)
    }

    /// The geometry recorded in the segment (filled on a mismatch).
    pub fn got(&self) -> Geometry {
        geometry(self.raw.got)
    }

    /// `errno` (POSIX) or `GetLastError()` (Windows) when the error is `System`; else 0.
    pub fn os_error(&self) -> i32 {
        self.raw.os_error
    }

    pub(crate) fn as_mut_ptr(info: Option<&mut OpenInfo>) -> *mut sys::hqq_open_info {
        info.map_or(std::ptr::null_mut(), |info| &mut info.raw)
    }
}

impl Default for OpenInfo {
    fn default() -> Self {
        OpenInfo::new()
    }
}

fn geometry(raw: sys::hqq_geometry) -> Geometry {
    Geometry {
        buffer_count: raw.buffer_count,
        buffer_size: raw.buffer_size,
        layout_size: raw.layout_size,
        page_size: raw.page_size,
    }
}

fn init_options() -> sys::hqq_options {
    let mut raw: sys::hqq_options = unsafe { zeroed() };
    // SAFETY: `raw` is a valid, exclusively borrowed options struct.
    unsafe { sys::hqq_options_init(&mut raw) };
    raw
}

/// A segment or listener name as the NUL-terminated string the C API takes.
pub(crate) fn c_name(name: &str) -> Result<CString> {
    CString::new(name).map_err(|_| Error::InvalidName)
}
