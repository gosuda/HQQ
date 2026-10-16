//! The error type: every non-OK `hqq_status`, one variant each.

use std::ffi::CStr;
use std::fmt;
use std::os::raw::c_int;

use hqq_sys as sys;

/// `Result` with [`Error`] as the default error type.
pub type Result<T, E = Error> = std::result::Result<T, E>;

macro_rules! errors {
    ($( $(#[$doc:meta])* $variant:ident => $code:ident, )*) => {
        /// A failure reported by libhqq (`hqq_status`, other than `HQQ_OK`).
        ///
        /// `Error::Unknown` only appears when the C library reports a status this
        /// binding does not know (a version mismatch between hqq-sys and libhqq).
        #[derive(Clone, Copy, PartialEq, Eq, Hash)]
        #[non_exhaustive]
        pub enum Error {
            $( $(#[$doc])* $variant, )*
            /// A status code this binding does not know.
            Unknown(c_int),
        }

        impl Error {
            /// The `hqq_status` value of this error.
            pub fn code(self) -> c_int {
                match self {
                    $( Error::$variant => sys::$code, )*
                    Error::Unknown(code) => code,
                }
            }

            /// The error for a status code, or `None` for `HQQ_OK`.
            pub fn from_code(code: c_int) -> Option<Error> {
                match code {
                    sys::HQQ_OK => None,
                    $( sys::$code => Some(Error::$variant), )*
                    other => Some(Error::Unknown(other)),
                }
            }
        }
    };
}

errors! {
    /// The local link or acceptor was closed (`ErrClosed`).
    Closed => HQQ_ERR_CLOSED,
    /// The peer detached gracefully and every buffered message was consumed (`ErrPeerClosed`, `io.EOF`).
    PeerClosed => HQQ_ERR_PEER_CLOSED,
    /// The peer stopped heartbeating, was replaced, or the segment was re-initialized (`ErrPeerGone`).
    PeerGone => HQQ_ERR_PEER_GONE,
    /// The deadline passed, or a non-blocking call would block (`ErrTimeout`).
    Timeout => HQQ_ERR_TIMEOUT,
    /// A live endpoint is present (`ErrLinkBusy`, matches `FailedInit`).
    LinkBusy => HQQ_ERR_LINK_BUSY,
    /// Foreign magic, other layout, or unknown state (`ErrIncompatibleLayout`).
    IncompatibleLayout => HQQ_ERR_INCOMPATIBLE_LAYOUT,
    /// Initialization failed (`ErrFailedInit`).
    FailedInit => HQQ_ERR_FAILED_INIT,
    /// Transient: the object exists but is not sized or claimed yet.
    SegmentNotReady => HQQ_ERR_SEGMENT_NOT_READY,
    /// Transient: another endpoint kept initializing.
    InitContended => HQQ_ERR_INIT_CONTENDED,
    /// Transient: the segment has no live endpoint.
    NoLiveEndpoint => HQQ_ERR_NO_LIVE_ENDPOINT,
    /// Invalid geometry, a too-small object, or a geometry mismatch (`ErrInvalidSize`).
    InvalidSize => HQQ_ERR_INVALID_SIZE,
    /// The geometry differs from the segment's; see `OpenInfo` (matches `InvalidSize`).
    GeometryMismatch => HQQ_ERR_GEOMETRY_MISMATCH,
    /// Caller memory is not page aligned (`ErrMemoryAlign`).
    MemoryAlign => HQQ_ERR_MEMORY_ALIGN,
    /// Caller memory is smaller than the layout (`ErrMemorySmall`).
    MemorySmall => HQQ_ERR_MEMORY_SMALL,
    /// The payload is larger than the buffer size or the maximum message size (`ErrBufferOverflow`).
    BufferOverflow => HQQ_ERR_BUFFER_OVERFLOW,
    /// The reader's buffer is smaller than the message; nothing was consumed.
    BufferTooSmall => HQQ_ERR_BUFFER_TOO_SMALL,
    /// Stream or connection not found (`ErrConnNotFound`).
    ConnNotFound => HQQ_ERR_CONN_NOT_FOUND,
    /// Protocol-state violation, or a stale or foreign reservation (`ErrInvalidOp`).
    InvalidOp => HQQ_ERR_INVALID_OP,
    /// An Advanced frame arrived on a Standard reader (`ErrUnexpectedPacket`).
    UnexpectedPacket => HQQ_ERR_UNEXPECTED_PACKET,
    /// The peer reported an unknown error code (`ErrProtocol`).
    Protocol => HQQ_ERR_PROTOCOL,
    /// The name is invalid (`ErrInvalidName`).
    InvalidName => HQQ_ERR_INVALID_NAME,
    /// The named object does not exist (`os.ErrNotExist`).
    NotExist => HQQ_ERR_NOT_EXIST,
    /// The named object already exists (`os.ErrExist`).
    Exists => HQQ_ERR_EXISTS,
    /// An operating-system failure; `errno` or `GetLastError` is in `OpenInfo::os_error`.
    System => HQQ_ERR_SYSTEM,
    /// The cancellation token was signalled (`context.Canceled`).
    Canceled => HQQ_ERR_CANCELED,
    /// The allocator failed.
    NoMemory => HQQ_ERR_NO_MEMORY,
    /// A bad argument, such as a NUL byte in a name.
    InvalidArgument => HQQ_ERR_INVALID_ARGUMENT,
    /// Not available on this platform.
    Unsupported => HQQ_ERR_UNSUPPORTED,
    /// An Advanced operation before negotiation (`ErrNotNegotiated`).
    NotNegotiated => HQQ_ERR_NOT_NEGOTIATED,
    /// Negotiation was already started or completed (`ErrAlreadyNegotiated`).
    AlreadyNegotiated => HQQ_ERR_ALREADY_NEGOTIATED,
    /// Negotiation reached the terminal failed state (`ErrNegotiationFailed`).
    NegotiationFailed => HQQ_ERR_NEGOTIATION_FAILED,
    /// The peer refused the negotiation (`ErrNegotiationRejected`).
    NegotiationRejected => HQQ_ERR_NEGOTIATION_REJECTED,
    /// The peer abandoned a response mid-transfer (`ErrMessageAborted`).
    MessageAborted => HQQ_ERR_MESSAGE_ABORTED,
    /// The incoming queue overflowed under the fail policy (`ErrIncomingOverflow`).
    IncomingOverflow => HQQ_ERR_INCOMING_OVERFLOW,
    /// A response exceeded the reassembly limit (`ErrReassemblyLimit`).
    ReassemblyLimit => HQQ_ERR_REASSEMBLY_LIMIT,
    /// Refused during graceful shutdown (`ErrShuttingDown`).
    ShuttingDown => HQQ_ERR_SHUTTING_DOWN,
    /// The peer sent an error response; its status and payload are in the `CallError`.
    Response => HQQ_ERR_RESPONSE,
    /// `listen_streams` was not called (`ErrNotListening`).
    NotListening => HQQ_ERR_NOT_LISTENING,
    /// `listen_streams` was already called (`ErrAlreadyListening`).
    AlreadyListening => HQQ_ERR_ALREADY_LISTENING,
    /// A live acceptor serves the listener name (`ErrListenerBusy`).
    ListenerBusy => HQQ_ERR_LISTENER_BUSY,
    /// The listener's acceptor is gone (`ErrListenerGone`).
    ListenerGone => HQQ_ERR_LISTENER_GONE,
    /// The listener is closed (`ErrListenerClosed`).
    ListenerClosed => HQQ_ERR_LISTENER_CLOSED,
    /// The acceptor is closed (`ErrAcceptorClosed`).
    AcceptorClosed => HQQ_ERR_ACCEPTOR_CLOSED,
    /// Transient: the listener is not ready for a client yet.
    ListenerNotReady => HQQ_ERR_LISTENER_NOT_READY,
}

impl Error {
    /// The Go-style wrapping relation of the C library (`hqq_error_is`): true when
    /// `self` is `base` or wraps it (for example `LinkBusy` matches `FailedInit`).
    pub fn matches(self, base: Error) -> bool {
        // SAFETY: plain function on integer codes; no pointers involved.
        unsafe { sys::hqq_error_is(self.code(), base.code()) != 0 }
    }

    /// True for the statuses a retrying opener should retry (SPEC 4).
    pub fn is_transient_open(self) -> bool {
        // SAFETY: plain function on an integer code.
        unsafe { sys::hqq_error_is_transient_open(self.code()) != 0 }
    }

    fn message(self) -> String {
        // SAFETY: hqq_strerror returns a static, never-NULL, NUL-terminated string.
        unsafe { CStr::from_ptr(sys::hqq_strerror(self.code())) }
            .to_string_lossy()
            .into_owned()
    }

    fn name(self) -> &'static str {
        macro_rules! names {
            ($($variant:ident => $code:ident,)*) => {
                match self { $( Error::$variant => stringify!($code), )* Error::Unknown(_) => "HQQ_ERR_UNKNOWN" }
            };
        }
        names! {
            Closed => HQQ_ERR_CLOSED, PeerClosed => HQQ_ERR_PEER_CLOSED, PeerGone => HQQ_ERR_PEER_GONE,
            Timeout => HQQ_ERR_TIMEOUT, LinkBusy => HQQ_ERR_LINK_BUSY,
            IncompatibleLayout => HQQ_ERR_INCOMPATIBLE_LAYOUT, FailedInit => HQQ_ERR_FAILED_INIT,
            SegmentNotReady => HQQ_ERR_SEGMENT_NOT_READY, InitContended => HQQ_ERR_INIT_CONTENDED,
            NoLiveEndpoint => HQQ_ERR_NO_LIVE_ENDPOINT, InvalidSize => HQQ_ERR_INVALID_SIZE,
            GeometryMismatch => HQQ_ERR_GEOMETRY_MISMATCH, MemoryAlign => HQQ_ERR_MEMORY_ALIGN,
            MemorySmall => HQQ_ERR_MEMORY_SMALL, BufferOverflow => HQQ_ERR_BUFFER_OVERFLOW,
            BufferTooSmall => HQQ_ERR_BUFFER_TOO_SMALL, ConnNotFound => HQQ_ERR_CONN_NOT_FOUND,
            InvalidOp => HQQ_ERR_INVALID_OP, UnexpectedPacket => HQQ_ERR_UNEXPECTED_PACKET,
            Protocol => HQQ_ERR_PROTOCOL, InvalidName => HQQ_ERR_INVALID_NAME,
            NotExist => HQQ_ERR_NOT_EXIST, Exists => HQQ_ERR_EXISTS, System => HQQ_ERR_SYSTEM,
            Canceled => HQQ_ERR_CANCELED, NoMemory => HQQ_ERR_NO_MEMORY,
            InvalidArgument => HQQ_ERR_INVALID_ARGUMENT, Unsupported => HQQ_ERR_UNSUPPORTED,
            NotNegotiated => HQQ_ERR_NOT_NEGOTIATED, AlreadyNegotiated => HQQ_ERR_ALREADY_NEGOTIATED,
            NegotiationFailed => HQQ_ERR_NEGOTIATION_FAILED,
            NegotiationRejected => HQQ_ERR_NEGOTIATION_REJECTED,
            MessageAborted => HQQ_ERR_MESSAGE_ABORTED, IncomingOverflow => HQQ_ERR_INCOMING_OVERFLOW,
            ReassemblyLimit => HQQ_ERR_REASSEMBLY_LIMIT, ShuttingDown => HQQ_ERR_SHUTTING_DOWN,
            Response => HQQ_ERR_RESPONSE, NotListening => HQQ_ERR_NOT_LISTENING,
            AlreadyListening => HQQ_ERR_ALREADY_LISTENING, ListenerBusy => HQQ_ERR_LISTENER_BUSY,
            ListenerGone => HQQ_ERR_LISTENER_GONE, ListenerClosed => HQQ_ERR_LISTENER_CLOSED,
            AcceptorClosed => HQQ_ERR_ACCEPTOR_CLOSED, ListenerNotReady => HQQ_ERR_LISTENER_NOT_READY,
        }
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message())
    }
}

impl fmt::Debug for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "Error::{:?}({} {})",
            self.name(),
            self.code(),
            self.message()
        )
    }
}

impl std::error::Error for Error {}

/// Maps a status to `Ok(())` or the error.
pub(crate) fn check(status: c_int) -> Result<()> {
    match Error::from_code(status) {
        None => Ok(()),
        Some(err) => Err(err),
    }
}
