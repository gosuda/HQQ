//! The Standard link: named or caller-owned shared memory with two endpoints,
//! whole-message write and read (`hqq.h`).

use std::ffi::{c_void, CStr};
use std::mem::{forget, zeroed};
use std::os::raw::c_int;
use std::ptr::NonNull;
use std::slice;

use hqq_sys as sys;

use crate::error::{check, Error, Result};
use crate::options::{c_name, OpenInfo, Options};
use crate::wait::Wait;

/// Which endpoint of a link this is (`hqq_mode`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Mode {
    /// Initialized the segment.
    Primary,
    /// Joined an initialized segment.
    Secondary,
}

/// What this endpoint knows about its peer (`hqq_peer_status`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PeerState {
    NotConnected,
    Connected,
    Closed,
    Gone,
}

fn mode_of(raw: c_int) -> Mode {
    if raw == sys::HQQ_MODE_PRIMARY {
        Mode::Primary
    } else {
        Mode::Secondary
    }
}

fn peer_state_of(raw: c_int) -> PeerState {
    match raw {
        sys::HQQ_PEER_CONNECTED => PeerState::Connected,
        sys::HQQ_PEER_CLOSED => PeerState::Closed,
        sys::HQQ_PEER_GONE => PeerState::Gone,
        _ => PeerState::NotConnected,
    }
}

/// A Standard link (`hqq_link`). Owned: the handle is closed on drop.
///
/// Every method takes `&self`; the C library allows any number of threads to
/// use one link at once, so `Link` is `Send` and `Sync`.
pub struct Link {
    raw: NonNull<sys::hqq_link>,
}

// SAFETY: hqq.h: "A hqq_link may be used from any number of threads at once."
// Only close() and drop (which take the value) end the handle's life.
unsafe impl Send for Link {}
unsafe impl Sync for Link {}

impl Link {
    /// Creates the named segment as the Primary (`hqq_standard_create`).
    /// `perm` is the POSIX mode of the created object, for example `0o600`.
    pub fn create(
        name: &str,
        buffer_count: u64,
        buffer_size: u64,
        perm: u32,
        opts: &Options,
        info: Option<&mut OpenInfo>,
    ) -> Result<Link> {
        let name = c_name(name)?;
        let mut out = std::ptr::null_mut();
        // SAFETY: `name` is NUL-terminated and outlives the call; the out pointers are valid.
        let status = unsafe {
            sys::hqq_standard_create(
                name.as_ptr(),
                buffer_count,
                buffer_size,
                perm,
                opts.as_ptr(),
                &mut out,
                OpenInfo::as_mut_ptr(info),
            )
        };
        Link::from_status(status, out)
    }

    /// Attaches to an existing named segment as the Secondary (`hqq_standard_open`).
    pub fn open(
        name: &str,
        buffer_count: u64,
        buffer_size: u64,
        opts: &Options,
        info: Option<&mut OpenInfo>,
    ) -> Result<Link> {
        let name = c_name(name)?;
        let mut out = std::ptr::null_mut();
        // SAFETY: as in `create`.
        let status = unsafe {
            sys::hqq_standard_open(
                name.as_ptr(),
                buffer_count,
                buffer_size,
                opts.as_ptr(),
                &mut out,
                OpenInfo::as_mut_ptr(info),
            )
        };
        Link::from_status(status, out)
    }

    /// Creates or attaches to a link in caller-owned memory (`hqq_standard_open_memory`).
    ///
    /// # Safety
    /// `mem` must be page aligned, point to at least `mem_size` bytes that stay
    /// mapped and are not otherwise accessed until the link is closed, and
    /// `mem_size` must be at least `hqq_standard_size(buffer_count, buffer_size)`.
    pub unsafe fn open_memory(
        mem: *mut c_void,
        mem_size: usize,
        buffer_count: u64,
        buffer_size: u64,
        opts: &Options,
        info: Option<&mut OpenInfo>,
    ) -> Result<Link> {
        let mut out = std::ptr::null_mut();
        let status = sys::hqq_standard_open_memory(
            mem,
            mem_size,
            buffer_count,
            buffer_size,
            opts.as_ptr(),
            &mut out,
            OpenInfo::as_mut_ptr(info),
        );
        Link::from_status(status, out)
    }

    fn from_status(status: c_int, out: *mut sys::hqq_link) -> Result<Link> {
        check(status)?;
        NonNull::new(out)
            .map(|raw| Link { raw })
            .ok_or(Error::InvalidArgument)
    }

    /// Publishes `data` as one message (1..=buffer_size bytes; empty sends nothing).
    pub fn write(&self, data: &[u8], wait: Wait<'_>) -> Result<()> {
        let wait = wait.raw();
        // SAFETY: `data` is valid for `len` bytes; `wait` outlives the call.
        check(unsafe { sys::hqq_write(self.as_ptr(), data.as_ptr().cast(), data.len(), &wait) })
    }

    /// Consumes one message into `buf`, returning its size. A message larger than
    /// `buf` is not consumed and the result is [`Error::BufferTooSmall`]; use
    /// [`receive`](Self::receive) to size the buffer automatically.
    pub fn read(&self, buf: &mut [u8], wait: Wait<'_>) -> Result<usize> {
        let wait = wait.raw();
        let mut len = 0usize;
        // SAFETY: `buf` is valid for `buf.len()` bytes and `len` is a valid out pointer.
        check(unsafe {
            sys::hqq_read(
                self.as_ptr(),
                buf.as_mut_ptr().cast(),
                buf.len(),
                &mut len,
                &wait,
            )
        })?;
        Ok(len)
    }

    /// Consumes one whole message into a new `Vec`, growing the buffer to the
    /// size the C library reports when it is too small.
    pub fn receive(&self, wait: Wait<'_>) -> Result<Vec<u8>> {
        let mut buf = vec![0u8; self.buffer_size()];
        loop {
            let raw_wait = wait.raw();
            let mut len = 0usize;
            // SAFETY: as in `read`.
            let status = unsafe {
                sys::hqq_read(
                    self.as_ptr(),
                    buf.as_mut_ptr().cast(),
                    buf.len(),
                    &mut len,
                    &raw_wait,
                )
            };
            if status == sys::HQQ_ERR_BUFFER_TOO_SMALL {
                if len <= buf.len() {
                    return Err(Error::InvalidSize);
                }
                buf.resize(len, 0);
                continue;
            }
            check(status)?;
            buf.truncate(len);
            return Ok(buf);
        }
    }

    /// Claims a write slot (`hqq_write_reserve`). Complete it with
    /// [`WriteReservation::commit`] or [`WriteReservation::abort`].
    pub fn reserve_write(&self, wait: Wait<'_>) -> Result<WriteReservation<'_>> {
        let wait = wait.raw();
        // SAFETY: a zeroed reservation is a valid out buffer for hqq_write_reserve.
        let mut res: sys::hqq_write_reservation = unsafe { zeroed() };
        check(unsafe { sys::hqq_write_reserve(self.as_ptr(), &mut res, &wait) })?;
        Ok(WriteReservation {
            link: self,
            res,
            pending: true,
        })
    }

    /// Claims the next message (`hqq_read_reserve`). The payload is valid until
    /// the reservation is released or dropped.
    pub fn reserve_read(&self, wait: Wait<'_>) -> Result<ReadReservation<'_>> {
        let wait = wait.raw();
        // SAFETY: as in `reserve_write`.
        let mut res: sys::hqq_read_reservation = unsafe { zeroed() };
        check(unsafe { sys::hqq_read_reserve(self.as_ptr(), &mut res, &wait) })?;
        Ok(ReadReservation {
            link: self,
            res,
            pending: true,
        })
    }

    pub fn peer_state(&self) -> PeerState {
        // SAFETY: hqq_peer_state is documented thread-safe on a live link.
        peer_state_of(unsafe { sys::hqq_peer_state(self.as_ptr()) })
    }

    pub fn mode(&self) -> Mode {
        // SAFETY: read-only query on a live link.
        mode_of(unsafe { sys::hqq_link_mode(self.as_ptr()) })
    }

    pub fn buffer_size(&self) -> usize {
        // SAFETY: read-only query on a live link.
        unsafe { sys::hqq_link_buffer_size(self.as_ptr()) }
    }

    pub fn buffer_count(&self) -> usize {
        // SAFETY: read-only query on a live link.
        unsafe { sys::hqq_link_buffer_count(self.as_ptr()) }
    }

    /// The name of a named link; `None` for caller-owned memory.
    pub fn name(&self) -> Option<&str> {
        // SAFETY: the pointer is valid until the link is closed, which the borrow of `self` prevents.
        let name = unsafe { sys::hqq_link_name(self.as_ptr()) };
        if name.is_null() {
            None
        } else {
            unsafe { CStr::from_ptr(name) }.to_str().ok()
        }
    }

    /// Stops the link: blocked calls fail with `Closed` and the peer sees `PeerClosed`.
    /// The handle stays valid until dropped or closed.
    pub fn shutdown(&self) {
        // SAFETY: hqq_shutdown is safe to call concurrently with other calls.
        unsafe { sys::hqq_shutdown(self.as_ptr()) }
    }

    /// Closes the link (`hqq_close`) and reports its failure, if any.
    pub fn close(self) -> Result<()> {
        let raw = self.raw.as_ptr();
        forget(self);
        // SAFETY: the handle is consumed here, so no other use can follow.
        check(unsafe { sys::hqq_close(raw) })
    }

    /// The raw handle, for interop with the C API. Valid while `self` lives.
    pub fn as_ptr(&self) -> *mut sys::hqq_link {
        self.raw.as_ptr()
    }
}

impl Drop for Link {
    fn drop(&mut self) {
        // SAFETY: the handle is owned and not used after this point.
        unsafe { sys::hqq_close(self.raw.as_ptr()) };
    }
}

/// A claimed write slot. Commit or abort it; dropping it while pending
/// publishes a tombstone (readers skip it), so the direction never stalls.
pub struct WriteReservation<'a> {
    link: &'a Link,
    res: sys::hqq_write_reservation,
    pending: bool,
}

impl WriteReservation<'_> {
    /// The slot-owned payload buffer (`buffer_size` bytes).
    pub fn payload_mut(&mut self) -> &mut [u8] {
        // SAFETY: while the reservation is pending the slot's buffer is ours and
        // holds `capacity` bytes; the exclusive borrow of `self` gives exclusive access.
        unsafe { slice::from_raw_parts_mut(self.res.data.cast::<u8>(), self.res.capacity) }
    }

    /// Publishes the first `len` bytes of the payload.
    pub fn commit(mut self, len: usize) -> Result<()> {
        self.pending = false;
        // SAFETY: the reservation is pending and owned by this value.
        check(unsafe { sys::hqq_write_commit(&mut self.res, len) })
    }

    /// Publishes a tombstone with `reason` (an `HQQ_ERRCODE_*` value; 0 means InvalidOp).
    pub fn abort(mut self, reason: u32) -> Result<()> {
        self.pending = false;
        // SAFETY: as in `commit`.
        check(unsafe { sys::hqq_write_abort(&mut self.res, reason) })
    }

    /// The link this reservation belongs to.
    pub fn link(&self) -> &Link {
        self.link
    }
}

impl Drop for WriteReservation<'_> {
    fn drop(&mut self) {
        if self.pending {
            // SAFETY: the reservation is pending and owned here; the outcome of an abort cannot be reported from drop.
            unsafe { sys::hqq_write_abort(&mut self.res, sys::HQQ_ERRCODE_INVALID_OP) };
        }
    }
}

/// A claimed message of the read direction. Released on drop.
pub struct ReadReservation<'a> {
    link: &'a Link,
    res: sys::hqq_read_reservation,
    pending: bool,
}

impl ReadReservation<'_> {
    /// The message payload, valid until the reservation is released.
    pub fn payload(&self) -> &[u8] {
        // SAFETY: while pending, the slot holds `size` bytes of this message; `&self` keeps the slot claimed.
        unsafe { slice::from_raw_parts(self.res.data.cast::<u8>(), self.res.size) }
    }

    /// Releases the slot (`hqq_read_release`).
    pub fn release(mut self) -> Result<()> {
        self.pending = false;
        // SAFETY: the reservation is pending and owned by this value.
        check(unsafe { sys::hqq_read_release(&mut self.res) })
    }

    /// The link this reservation belongs to.
    pub fn link(&self) -> &Link {
        self.link
    }
}

impl Drop for ReadReservation<'_> {
    fn drop(&mut self) {
        if self.pending {
            // SAFETY: as in `WriteReservation::drop`.
            unsafe { sys::hqq_read_release(&mut self.res) };
        }
    }
}
