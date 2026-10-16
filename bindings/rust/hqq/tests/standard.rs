//! Standard link tests over in-process named segments (the in-tree C library).
//! Every test uses names of its own (process id and a counter, at most 30 bytes).

use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use hqq::{CancelToken, Error, Link, Mode, OpenInfo, Options, PeerState, Wait};

fn unique(tag: &str) -> String {
    static COUNTER: AtomicUsize = AtomicUsize::new(0);
    format!(
        "hqqr-{tag}-{}-{}",
        std::process::id(),
        COUNTER.fetch_add(1, Ordering::Relaxed)
    )
}

fn secs(n: u64) -> Wait<'static> {
    Wait::timeout(Duration::from_secs(n))
}

#[test]
fn round_trip_both_directions() {
    let name = unique("rt");
    let a = Link::create(&name, 4, 256, 0o600, &Options::new(), None).expect("create");
    let b = Link::open(&name, 4, 256, &Options::new(), None).expect("open");
    assert_eq!(a.mode(), Mode::Primary);
    assert_eq!(b.mode(), Mode::Secondary);
    assert_eq!(a.buffer_size(), 256);
    assert_eq!(a.buffer_count(), 4);
    assert_eq!(a.name(), Some(name.as_str()));

    a.write(b"hello from a", secs(5)).expect("write");
    assert_eq!(b.receive(secs(5)).expect("receive"), b"hello from a");
    b.write(b"and back", secs(5)).expect("write back");
    assert_eq!(a.receive(secs(5)).expect("receive back"), b"and back");

    // Messages are whole: a small buffer does not consume the message.
    b.write(&[7u8; 100], secs(5)).expect("write 100");
    let mut small = [0u8; 10];
    assert_eq!(a.read(&mut small, secs(5)), Err(Error::BufferTooSmall));
    let mut big = [0u8; 100];
    assert_eq!(a.read(&mut big, secs(5)), Ok(100));
    assert!(big.iter().all(|&x| x == 7));

    // Oversized writes are refused; empty writes succeed; a poll on an empty ring times out.
    assert_eq!(a.write(&[0u8; 300], secs(1)), Err(Error::BufferOverflow));
    a.write(&[], secs(1)).expect("empty write");
    assert_eq!(b.read(&mut small, Wait::poll()), Err(Error::Timeout));

    // A signalled token cancels a blocked call.
    let token = CancelToken::new();
    token.signal();
    assert!(token.is_signalled());
    assert_eq!(
        b.read(&mut small, Wait::forever().cancel_with(&token)),
        Err(Error::Canceled)
    );

    // Peer close: buffered messages are delivered first, then PeerClosed.
    a.write(b"last", secs(5)).expect("last write");
    a.close().expect("close a");
    assert_eq!(b.receive(secs(5)).expect("drain"), b"last");
    assert_eq!(b.read(&mut small, secs(5)), Err(Error::PeerClosed));
    assert_eq!(b.peer_state(), PeerState::Closed);
    b.close().expect("close b");
}

#[test]
fn reservations_commit_and_release() {
    let name = unique("res");
    let a = Link::create(&name, 4, 256, 0o600, &Options::new(), None).expect("create");
    let b = Link::open(&name, 4, 256, &Options::new(), None).expect("open");

    let mut w = a.reserve_write(secs(5)).expect("reserve write");
    assert_eq!(w.payload_mut().len(), 256);
    w.payload_mut()[..8].copy_from_slice(b"reserved");
    w.commit(8).expect("commit");

    let r = b.reserve_read(secs(5)).expect("reserve read");
    assert_eq!(r.payload(), b"reserved");
    r.release().expect("release");

    // A dropped pending write publishes a tombstone: the ring keeps moving.
    let dropped = a.reserve_write(secs(5)).expect("reserve again");
    drop(dropped);
    a.write(b"after", secs(5)).expect("write after tombstone");
    assert_eq!(
        b.receive(secs(5)).expect("receive after tombstone"),
        b"after"
    );
}

#[test]
fn open_errors_are_reported() {
    assert_eq!(
        Link::open(&unique("missing"), 4, 256, &Options::new(), None).err(),
        Some(Error::NotExist)
    );
    assert_eq!(
        Link::create("bad\0name", 4, 256, 0o600, &Options::new(), None).err(),
        Some(Error::InvalidName)
    );

    let name = unique("geo");
    let _a = Link::create(&name, 4, 256, 0o600, &Options::new(), None).expect("create");
    let mut info = OpenInfo::new();
    let err = Link::open(&name, 8, 256, &Options::new(), Some(&mut info)).err();
    assert_eq!(err, Some(Error::GeometryMismatch));
    assert_eq!(info.got().buffer_count, 4);
    assert_eq!(info.want().buffer_count, 8);
    assert!(Error::GeometryMismatch.matches(Error::InvalidSize));
    assert!(Error::NotExist.is_transient_open());
}

#[test]
fn full_size_message_round_trip() {
    // A message of exactly buffer_size bytes is the largest one the link accepts.
    let name = unique("big");
    let a = Link::create(&name, 2, 256, 0o600, &Options::new(), None).expect("create");
    let b = Link::open(&name, 2, 256, &Options::new(), None).expect("open");
    let payload: Vec<u8> = (0..=255u8).collect();
    a.write(&payload, secs(5)).expect("write full");
    assert_eq!(b.receive(secs(5)).expect("receive full"), payload);
}
