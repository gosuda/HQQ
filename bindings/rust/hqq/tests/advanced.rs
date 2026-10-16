//! Advanced link tests: negotiation, calls, metadata, streams, a panicking
//! handler and the acceptor, over in-process named segments.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;
use std::time::Duration;

use hqq::{
    AcceptorOptions, AdvLink, AdvOptions, CallOptions, ConnectOptions, Error, Message, Options,
    ProtocolState, Request, Responder, Wait,
};

const UPPER: u64 = 1;
const FAIL: u64 = 2;
const CALLER: u64 = 3;
const PANIC: u64 = 4;

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

/// The service handler. It is stateless, so it is safe to run on worker threads.
fn handle(req: &Request<'_>, resp: &mut Responder<'_>) {
    match req.method() {
        UPPER => {
            let upper = String::from_utf8_lossy(req.data()).to_ascii_uppercase();
            resp.write(upper.as_bytes()).expect("write response");
        }
        FAIL => resp.error(42, b"bad request").expect("error response"),
        CALLER => {
            let mut text = req.caller().to_string();
            if req.trace().is_some() {
                text.push_str("+trace");
            }
            resp.write(text.as_bytes()).expect("write response");
        }
        PANIC => panic!("handler failure (expected by the test)"),
        _ => resp.error(6, b"unknown method").expect("error response"),
    }
}

/// Creates a negotiated pair (Primary, Secondary).
fn negotiated_pair(tag: &str) -> (AdvLink, AdvLink) {
    let name = unique(tag);
    let a = AdvLink::create(&name, 8, 4096, 0o600, &AdvOptions::new(), None).expect("create");
    let b = AdvLink::open(&name, 8, 4096, &AdvOptions::new(), None).expect("open");
    std::thread::scope(|s| {
        let other = s.spawn(|| b.negotiate_all(secs(10)));
        a.negotiate_all(secs(10)).expect("negotiate primary");
        other.join().expect("join").expect("negotiate secondary");
    });
    assert_eq!(a.protocol_state(), ProtocolState::Negotiated);
    (a, b)
}

#[test]
fn calls_errors_and_metadata() {
    let (a, b) = negotiated_pair("call");
    assert_eq!(a.negotiated_version(), 0x0101); // protocol 1.1
    std::thread::scope(|s| {
        // The secondary serves on worker threads until it is shut down.
        let server = s.spawn(|| b.serve(handle, secs(30)));
        let stream = a.stream0();

        let reply = stream.call(UPPER, b"hello", secs(10)).expect("call");
        assert_eq!(reply.data(), b"HELLO");
        assert_eq!(reply.status(), 0);

        let err = stream
            .call(FAIL, b"", secs(10))
            .expect_err("error response");
        assert_eq!(err.error, Error::Response);
        let payload = err.response.expect("error payload");
        assert_eq!(payload.status(), 42);
        assert_eq!(payload.data(), b"bad request");

        // Metadata: the caller ID and the trace ID reach the handler.
        let options = CallOptions::new().caller(7).trace([0xAB; 16]);
        let traced = stream
            .call_with(CALLER, b"", &options, secs(10))
            .expect("call with metadata");
        assert_eq!(traced.data(), b"7+trace");

        // A panicking handler becomes an error response and does not unwind into C.
        let crashed = stream
            .call(PANIC, b"", secs(10))
            .expect_err("panic becomes a response");
        assert_eq!(crashed.error, Error::Response);
        assert_eq!(crashed.response.expect("panic response").status(), 6);

        b.shutdown(secs(10)).expect("shutdown");
        server
            .join()
            .expect("serve thread")
            .expect("serve returns Ok after shutdown");
    });
    a.close().expect("close a");
    b.close().expect("close b");
}

#[test]
fn streams_open_accept_send_and_recv() {
    let (a, b) = negotiated_pair("stream");
    a.listen_streams().expect("listen");
    assert_eq!(a.listen_streams(), Err(Error::AlreadyListening));

    std::thread::scope(|s| {
        let opener = s.spawn(|| {
            let stream = b.open_stream(secs(10))?;
            stream.send(b"stream payload", secs(10))?;
            Ok::<_, Error>(stream.id())
        });
        let accepted = a.accept_stream(secs(10)).expect("accept");
        let opened_id = opener.join().expect("join").expect("open and send");
        assert_eq!(accepted.id(), opened_id);
        let msg: Message<'_> = accepted.recv(secs(10)).expect("recv");
        assert_eq!(msg.data(), b"stream payload");
        assert_eq!(msg.stream_id(), opened_id);
        accepted.close().expect("close stream");
        assert_eq!(accepted.close(), Err(Error::Closed));
    });
    a.close().expect("close a");
    b.close().expect("close b");
}

#[test]
fn negotiation_rules_are_enforced() {
    let name = unique("neg");
    let a = AdvLink::create(&name, 8, 4096, 0o600, &AdvOptions::new(), None).expect("create");
    let b = AdvLink::open(&name, 8, 4096, &AdvOptions::new(), None).expect("open");
    assert_eq!(a.protocol_state(), ProtocolState::NotStarted);
    // A bad major version is rejected by the peer (version interop).
    std::thread::scope(|s| {
        let other = s.spawn(|| b.negotiate_protocol(0x0200, hqq::FEATURES_ALL, secs(10)));
        let primary = a.negotiate_protocol(0x0200, hqq::FEATURES_ALL, secs(10));
        let secondary = other.join().expect("join");
        assert!(primary.is_err() || secondary.is_err());
    });
    a.close().expect("close a");
    b.close().expect("close b");
}

#[test]
fn acceptor_hands_out_negotiated_links() {
    let listener = unique("lsn");
    let acceptor =
        hqq::Acceptor::create(&listener, &AcceptorOptions::new(), None).expect("acceptor");
    let accepted: Mutex<Option<hqq::Result<AdvLink>>> = Mutex::new(None);
    std::thread::scope(|s| {
        s.spawn(|| {
            *accepted.lock().expect("lock") = Some(acceptor.accept(secs(15)));
        });
        let client = hqq::connect(&listener, &ConnectOptions::new(), secs(15)).expect("connect");
        assert_eq!(client.protocol_state(), ProtocolState::Negotiated);
        // The accept thread finishes once the handshake is complete.
        while accepted.lock().expect("lock").is_none() {
            std::thread::sleep(Duration::from_millis(1));
        }
        let link = accepted
            .lock()
            .expect("lock")
            .take()
            .expect("accept result")
            .expect("accepted link");
        assert_eq!(link.protocol_state(), ProtocolState::Negotiated);
        assert!(acceptor.stats().accepted >= 1);
        link.close().expect("close accepted");
        client.close().expect("close client");
    });
    acceptor.close().expect("close acceptor");
}

#[test]
fn advanced_options_are_accepted() {
    let opts = AdvOptions::new()
        .link(Options::new().disable_heartbeat())
        .max_message_size(1 << 20)
        .queue_size(4);
    let name = unique("opts");
    let a = AdvLink::create(&name, 4, 256, 0o600, &opts, None).expect("create with options");
    assert_eq!(a.buffer_size(), 256);
    assert_eq!(a.buffer_count(), 4);
    a.close().expect("close");
}
