//! Acceptor and client connect (`hqq_listener.h`, `ext/LISTENER_DRAFT.md`).

use std::mem::zeroed;
use std::ptr::NonNull;

use std::sync::atomic::{AtomicBool, Ordering};

use hqq_sys as sys;

use crate::advanced::AdvLink;
use crate::error::{check, Error, Result};
use crate::options::{c_name, AcceptorOptions, ConnectOptions, OpenInfo};
use crate::wait::Wait;

/// Counters of an acceptor (`hqq_acceptor_stats`).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct AcceptorStats {
    /// Connect records taken from the queue.
    pub records: u64,
    /// Links returned by `accept`.
    pub accepted: u64,
    /// Records dropped by validation.
    pub malformed: u64,
    /// Valid records whose link segment could not be attached.
    pub unreachable: u64,
    pub handshake_failed: u64,
    /// Slots published on a dead producer's behalf.
    pub stuck_slots: u64,
}

/// The service side of a listener (`hqq_acceptor`): hands out negotiated links.
pub struct Acceptor {
    raw: NonNull<sys::hqq_acceptor>,
    /// Set by the first close; read by accept and drop, so the handle is closed once.
    closed: AtomicBool,
}

// SAFETY: hqq_listener.h: every function may be called from any thread; Accept may run
// concurrently from several threads.
unsafe impl Send for Acceptor {}
unsafe impl Sync for Acceptor {}

impl Acceptor {
    /// Creates the listener `name` (`hqq_acceptor_create`).
    pub fn create(
        name: &str,
        opts: &AcceptorOptions,
        info: Option<&mut OpenInfo>,
    ) -> Result<Acceptor> {
        let name = c_name(name)?;
        let mut out = std::ptr::null_mut();
        // SAFETY: `name` is NUL-terminated for the call; out pointers are valid.
        let status = unsafe {
            sys::hqq_acceptor_create(
                name.as_ptr(),
                opts.as_ptr(),
                &mut out,
                OpenInfo::as_mut_ptr(info),
            )
        };
        check(status)?;
        NonNull::new(out)
            .map(|raw| Acceptor {
                raw,
                closed: AtomicBool::new(false),
            })
            .ok_or(Error::InvalidArgument)
    }

    /// Returns the next negotiated link; it outlives the acceptor.
    pub fn accept(&self, wait: Wait<'_>) -> Result<AdvLink> {
        if self.closed.load(Ordering::Acquire) {
            return Err(Error::AcceptorClosed);
        }
        let wait = wait.raw();
        let mut out = std::ptr::null_mut();
        // SAFETY: out is a valid out pointer; wait outlives the call.
        let status = unsafe { sys::hqq_acceptor_accept(self.raw.as_ptr(), &mut out, &wait) };
        AdvLink::from_status(status, out)
    }

    pub fn stats(&self) -> AcceptorStats {
        // SAFETY: a zeroed struct is a valid out buffer for hqq_acceptor_stats_get.
        let mut raw: sys::hqq_acceptor_stats = unsafe { zeroed() };
        unsafe { sys::hqq_acceptor_stats_get(self.raw.as_ptr(), &mut raw) };
        AcceptorStats {
            records: raw.records,
            accepted: raw.accepted,
            malformed: raw.malformed,
            unreachable: raw.unreachable,
            handshake_failed: raw.handshake_failed,
            stuck_slots: raw.stuck_slots,
        }
    }

    /// Stops the listener and frees it, from any thread: a blocked `accept` on another
    /// thread returns `AcceptorClosed` (the C library waits for accepts in progress).
    /// Links already delivered stay open. Closing twice returns `AcceptorClosed`.
    pub fn close_shared(&self) -> Result<()> {
        if self.closed.swap(true, Ordering::AcqRel) {
            return Err(Error::AcceptorClosed);
        }
        // SAFETY: the flag guarantees a single close of the handle; the C library
        // waits for the accept calls in progress before it frees the object.
        check(unsafe { sys::hqq_acceptor_close(self.raw.as_ptr()) })
    }

    /// Stops the listener (see [`close_shared`](Self::close_shared)) and consumes it.
    pub fn close(self) -> Result<()> {
        self.close_shared()
    }
}

impl Drop for Acceptor {
    fn drop(&mut self) {
        if !self.closed.swap(true, Ordering::AcqRel) {
            // SAFETY: the handle is owned, closed here for the first time, and not used afterwards.
            unsafe { sys::hqq_acceptor_close(self.raw.as_ptr()) };
        }
    }
}

/// Client side: attaches to `listener`, creates this client's link, announces it
/// and waits for the negotiation answer (`hqq_connect`). Returns the negotiated
/// Primary link.
pub fn connect(listener: &str, opts: &ConnectOptions, wait: Wait<'_>) -> Result<AdvLink> {
    let listener = c_name(listener)?;
    let wait = wait.raw();
    let mut out = std::ptr::null_mut();
    // SAFETY: the name is NUL-terminated for the call; the other pointers outlive it.
    let status = unsafe { sys::hqq_connect(listener.as_ptr(), opts.as_ptr(), &wait, &mut out) };
    AdvLink::from_status(status, out)
}
