//! Per-call deadlines and cancellation (`hqq_wait`, `hqq_cancel`).

use std::cell::UnsafeCell;
use std::time::Duration;

use hqq_sys as sys;

/// A cancellation token (`hqq_cancel`). Signalling is idempotent and may happen
/// from any thread; a blocked call that was given the token returns
/// [`Error::Canceled`](crate::Error::Canceled) within about 10 ms.
pub struct CancelToken {
    raw: UnsafeCell<sys::hqq_cancel>,
}

// SAFETY: the C library reads and writes the token only through atomics
// (hqq_cancel documents "accessed atomically by the library"), so sharing it
// between threads is the intended use.
unsafe impl Sync for CancelToken {}

impl CancelToken {
    /// A token that is not signalled.
    pub fn new() -> CancelToken {
        let mut raw = sys::hqq_cancel::default();
        // SAFETY: `raw` is a valid, exclusively borrowed hqq_cancel.
        unsafe { sys::hqq_cancel_init(&mut raw) };
        CancelToken {
            raw: UnsafeCell::new(raw),
        }
    }

    /// Signals the token; every call given this token returns `Canceled`.
    pub fn signal(&self) {
        // SAFETY: the pointer is the token's own storage; the C side uses atomics.
        unsafe { sys::hqq_cancel_signal(self.raw.get()) }
    }

    /// True once [`signal`](Self::signal) was called.
    pub fn is_signalled(&self) -> bool {
        // SAFETY: as in `signal`.
        unsafe { sys::hqq_cancel_is_signalled(self.raw.get()) != 0 }
    }

    pub(crate) fn as_ptr(&self) -> *const sys::hqq_cancel {
        self.raw.get()
    }
}

impl Default for CancelToken {
    fn default() -> Self {
        CancelToken::new()
    }
}

/// Deadline and optional cancellation of one blocking call.
///
/// [`Wait::forever`] (the default) blocks until the call completes,
/// [`Wait::poll`] never blocks, and [`Wait::timeout`] gives a relative deadline
/// measured on the monotonic clock from the call's entry.
#[derive(Clone, Copy)]
pub struct Wait<'a> {
    timeout_ns: i64,
    cancel: Option<&'a CancelToken>,
}

impl<'a> Wait<'a> {
    /// Block until done (the default).
    pub const fn forever() -> Wait<'a> {
        Wait {
            timeout_ns: -1,
            cancel: None,
        }
    }

    /// Never block: fail with `Timeout` instead of waiting.
    pub const fn poll() -> Wait<'a> {
        Wait {
            timeout_ns: 0,
            cancel: None,
        }
    }

    /// Fail with `Timeout` when `duration` passes.
    pub fn timeout(duration: Duration) -> Wait<'a> {
        Wait {
            timeout_ns: duration_ns(duration),
            cancel: None,
        }
    }

    /// The same deadline, also aborted when `token` is signalled.
    pub fn cancel_with(self, token: &'a CancelToken) -> Wait<'a> {
        Wait {
            cancel: Some(token),
            ..self
        }
    }

    pub(crate) fn raw(&self) -> sys::hqq_wait {
        sys::hqq_wait {
            timeout_ns: self.timeout_ns,
            cancel: self.cancel.map_or(std::ptr::null(), CancelToken::as_ptr),
        }
    }
}

impl Default for Wait<'_> {
    fn default() -> Self {
        Wait::forever()
    }
}

/// Nanoseconds of a duration, saturated to the `i64` the C API takes.
pub(crate) fn duration_ns(duration: Duration) -> i64 {
    i64::try_from(duration.as_nanos()).unwrap_or(i64::MAX)
}
