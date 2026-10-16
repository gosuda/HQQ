//! Safe Rust wrapper for libhqq, the C library of the HQQ shared-memory IPC protocol.
//!
//! The three layers of the C library are wrapped one to one:
//!
//! * [`Link`] (Standard, `hqq.h`): one message per write, whole-message reads, zero-copy
//!   reservations ([`WriteReservation`], [`ReadReservation`]).
//! * [`AdvLink`] (Advanced, `hqq_advanced.h`): negotiation, [`Stream`]s, [`Message`]s,
//!   request/response calls ([`Stream::call`]) and [`AdvLink::serve`].
//! * [`Acceptor`] and [`connect`] (`hqq_listener.h`): listeners that hand out links.
//!
//! Every blocking call takes a [`Wait`]: a deadline, optionally with a [`CancelToken`].
//! Failures are [`Error`] values, one per `hqq_status`. Handles are closed on drop;
//! the `close` methods additionally report the failure.
//!
//! ```no_run
//! use hqq::{Link, Options, OpenInfo, Wait};
//!
//! # fn main() -> hqq::Result<()> {
//! let link = Link::create("demo-link", 8, 4096, 0o600, &Options::new(), None)?;
//! link.write(b"hello", Wait::timeout(std::time::Duration::from_secs(5)))?;
//! # Ok(())
//! # }
//! ```
//!
//! The safe API keeps the C rules: links and acceptors are `Send + Sync`
//! (the C API allows any thread on one handle), messages and reservations borrow
//! the link they come from, and no C callback can unwind into C.

mod advanced;
mod error;
mod listener;
mod options;
mod standard;
mod wait;

pub use advanced::version;
pub use advanced::{
    AdvLink, AdvStats, CallError, CallOptions, Message, ProtocolState, Request, Responder, Stream,
    FEATURES_ALL, FEATURE_CANCEL, FEATURE_MESSAGES, FEATURE_METADATA, FEATURE_REQUEST_RESPONSE,
};
pub use error::{Error, Result};
pub use listener::{connect, Acceptor, AcceptorStats};
pub use options::{
    AcceptorOptions, AdvOptions, ConnectOptions, Geometry, OpenInfo, Options, Overflow, WaitPolicy,
};
pub use standard::{Link, Mode, PeerState, ReadReservation, WriteReservation};
pub use wait::{CancelToken, Wait};

/// The numeric version of the linked library (`major * 10000 + minor * 100 + patch`).
pub fn version_number() -> u32 {
    // SAFETY: plain function without arguments.
    unsafe { hqq_sys::hqq_version_number() }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::mem::size_of;

    // The C *_init functions write struct_size; a mismatch means the Rust
    // layout of a struct differs from the C struct it mirrors.
    #[test]
    fn struct_sizes_match_the_c_library() {
        let mut opts: hqq_sys::hqq_options = unsafe { std::mem::zeroed() };
        unsafe { hqq_sys::hqq_options_init(&mut opts) };
        assert_eq!(opts.struct_size, size_of::<hqq_sys::hqq_options>());

        let mut adv: hqq_sys::hqq_adv_options = unsafe { std::mem::zeroed() };
        unsafe { hqq_sys::hqq_adv_options_init(&mut adv) };
        assert_eq!(adv.struct_size, size_of::<hqq_sys::hqq_adv_options>());

        let mut acc: hqq_sys::hqq_acceptor_options = unsafe { std::mem::zeroed() };
        unsafe { hqq_sys::hqq_acceptor_options_init(&mut acc) };
        assert_eq!(acc.struct_size, size_of::<hqq_sys::hqq_acceptor_options>());

        let mut conn: hqq_sys::hqq_connect_options = unsafe { std::mem::zeroed() };
        unsafe { hqq_sys::hqq_connect_options_init(&mut conn) };
        assert_eq!(conn.struct_size, size_of::<hqq_sys::hqq_connect_options>());

        let mut info: hqq_sys::hqq_open_info = unsafe { std::mem::zeroed() };
        unsafe { hqq_sys::hqq_open_info_init(&mut info) };
        assert_eq!(info.struct_size, size_of::<hqq_sys::hqq_open_info>());

        let mut call: hqq_sys::hqq_call_options = unsafe { std::mem::zeroed() };
        unsafe { hqq_sys::hqq_call_options_init(&mut call) };
        assert_eq!(call.struct_size, size_of::<hqq_sys::hqq_call_options>());
    }

    #[test]
    fn errors_round_trip_through_codes() {
        for code in 1..=44 {
            let err = Error::from_code(code).expect("known status");
            assert_eq!(err.code(), code, "status {code} round trips");
            assert_ne!(err.to_string(), "", "status {code} has a message");
        }
        assert_eq!(Error::from_code(hqq_sys::HQQ_OK), None);
        assert_eq!(Error::from_code(9999), Some(Error::Unknown(9999)));
        assert!(Error::LinkBusy.matches(Error::FailedInit));
        assert!(Error::GeometryMismatch.matches(Error::InvalidSize));
        assert!(!Error::Timeout.matches(Error::FailedInit));
        assert!(Error::NotExist.is_transient_open());
    }

    #[test]
    fn version_is_the_c_version() {
        assert_eq!(version_number(), 100);
        assert_eq!(version(), "0.1.0");
    }
}
