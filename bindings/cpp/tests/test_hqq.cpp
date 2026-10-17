// test_hqq.cpp - tests of the C++ binding (bindings/cpp/include/hqq/hqq.hpp).
//
// Dependency-free: CHECK() reports file:line and the binary exits non-zero on
// any failure. CHECK() is used on the main thread only; worker threads store
// their results and the main thread checks them after join(). Every test uses
// its own named segments (unique per process), so the tests may run in any order.
#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

#include "../support/process_id.hpp"
#include "hqq/hqq.hpp"

namespace {

int g_failures = 0;
int g_checks = 0;

#define CHECK(cond)                                                                   \
    do {                                                                              \
        ++g_checks;                                                                   \
        if (!(cond)) {                                                                \
            ++g_failures;                                                             \
            std::fprintf(stderr, "%s:%d: CHECK failed: %s\n", __FILE__, __LINE__, #cond); \
        }                                                                             \
    } while (0)

#define CHECK_OK(st) CHECK((st).ok())

// A unique, short segment name (macOS allows 30 bytes).
std::string unique_name(const char* tag) {
    static std::atomic<int> counter{0};
    return std::string("hqqc-") + tag + "-" + std::to_string(hqq_support::process_id()) + "-" +
           std::to_string(counter.fetch_add(1));
}

hqq::Wait wait_s(long seconds) { return hqq::Wait::for_duration(std::chrono::seconds(seconds)); }

// ---- Status, Result, views --------------------------------------------------

void test_status_and_views() {
    hqq::Status ok;
    CHECK(ok.ok());
    CHECK(static_cast<bool>(ok));
    CHECK(ok == HQQ_OK);

    hqq::Status busy(HQQ_ERR_LINK_BUSY);
    CHECK(!busy.ok());
    CHECK(busy == HQQ_ERR_LINK_BUSY);
    CHECK(busy.matches(HQQ_ERR_FAILED_INIT));  // Go wraps relation: LinkBusy is a FailedInit
    CHECK(!busy.matches(HQQ_ERR_TIMEOUT));

    CHECK(hqq::Status(HQQ_ERR_SEGMENT_NOT_READY).transient_open());
    CHECK(!hqq::Status(HQQ_ERR_CLOSED).transient_open());

    std::error_code ec = busy.error_code();
    CHECK(ec.value() == static_cast<int>(HQQ_ERR_LINK_BUSY));
    CHECK(ec.category() == hqq::error_category());
    CHECK(std::strcmp(ec.category().name(), "hqq") == 0);
    CHECK(ec.message() == std::string(hqq_strerror(HQQ_ERR_LINK_BUSY)));
    CHECK(std::string(hqq::version()) == std::string(hqq_version_string()));

    // Result carries either a value or the failing status.
    hqq::Result<int> bad = hqq::Status(HQQ_ERR_TIMEOUT);
    CHECK(!bad.ok());
    CHECK(bad.status() == HQQ_ERR_TIMEOUT);
    hqq::Result<int> good = 7;
    CHECK(good.ok() && good.value() == 7);

    // Views: a string, a vector of bytes and a mutable view all convert.
    std::string text = "abc";
    hqq::Bytes b1 = text;
    CHECK(b1.size() == 3 && b1.as_string_view() == "abc");
    std::vector<std::byte> v{std::byte{1}, std::byte{2}};
    hqq::Bytes b2 = v;
    CHECK(b2.size() == 2 && b2.begin()[1] == std::byte{2});
    unsigned char raw[4] = {9, 8, 7, 6};
    hqq::MutableBytes m(raw, sizeof raw);
    hqq::Bytes b3 = m;
    CHECK(b3.size() == 4 && b3.begin()[0] == std::byte{9});
    CHECK(hqq::Bytes().empty());

    // Wait conversion keeps the timeout and the token.
    hqq::CancelToken token;
    hqq::Wait w = hqq::Wait::for_duration(std::chrono::milliseconds(5), &token);
    hqq_wait native = w.native();
    CHECK(native.timeout_ns == 5000000);
    CHECK(native.cancel == token.native());
    CHECK(hqq::Wait{}.native().timeout_ns == -1);
    CHECK(hqq::Wait::poll().native().timeout_ns == 0);
}

// ---- Standard layer -----------------------------------------------------------

void test_standard_roundtrip() {
    const std::string name = unique_name("std");
    auto created = hqq::Link::create(name, 4, 256, 0600);
    CHECK_OK(created.status());
    if (!created.ok()) return;
    hqq::Link& a = created.value();
    CHECK(a.mode() == HQQ_MODE_PRIMARY);
    CHECK(a.buffer_size() == 256);
    CHECK(a.buffer_count() == 4);
    CHECK(std::string(a.name()) == name);

    auto opened = hqq::Link::open(name, 4, 256);
    CHECK_OK(opened.status());
    if (!opened.ok()) return;
    hqq::Link& b = opened.value();
    CHECK(b.mode() == HQQ_MODE_SECONDARY);

    // One message each way.
    const std::string msg = "hello over the standard link";
    CHECK_OK(a.write(msg, wait_s(5)));
    auto got = b.receive(wait_s(5));
    CHECK(got.ok());
    if (got.ok()) {
        const auto& bytes = got.value();
        CHECK(std::string(reinterpret_cast<const char*>(bytes.data()), bytes.size()) == msg);
    }

    // A too-small buffer does not consume the message.
    CHECK_OK(b.write(std::string(100, 'x'), wait_s(5)));
    std::byte small[10];
    std::size_t len = 0;
    hqq::Status st = a.read(small, &len, wait_s(5));
    CHECK(st == HQQ_ERR_BUFFER_TOO_SMALL);
    CHECK(len == 100);
    std::vector<std::byte> big(len);
    st = a.read(big, &len, wait_s(5));
    CHECK_OK(st);
    CHECK(len == 100 && big[99] == std::byte{'x'});

    // Oversized writes are refused locally; zero-length writes succeed.
    CHECK(a.write(std::string(300, 'y'), wait_s(1)) == HQQ_ERR_BUFFER_OVERFLOW);
    CHECK_OK(a.write(std::string_view{}, wait_s(1)));

    // An empty ring with a zero timeout never blocks.
    CHECK(b.read(small, &len, hqq::Wait::poll()) == HQQ_ERR_TIMEOUT);

    // A signalled token cancels a blocked call.
    hqq::CancelToken token;
    token.signal();
    CHECK(token.signalled());
    CHECK(b.read(small, &len, hqq::Wait::forever(&token)) == HQQ_ERR_CANCELED);

    // Zero-copy reservations: commit one message, then read it back.
    auto wr = a.reserve_write(wait_s(5));
    CHECK(wr.ok());
    if (wr.ok()) {
        hqq::WriteReservation& r = wr.value();
        CHECK(r.payload().size() == 256);
        std::memcpy(r.payload().data(), "reserved", 8);
        CHECK_OK(r.commit(8));
    }
    auto rr = b.reserve_read(wait_s(5));
    CHECK(rr.ok());
    if (rr.ok()) {
        CHECK(rr.value().payload().as_string_view() == "reserved");
        CHECK_OK(rr.value().release());
    }

    // Peer close: buffered messages are delivered first, then PEER_CLOSED.
    CHECK_OK(a.write(std::string("last"), wait_s(5)));
    CHECK_OK(a.close());
    CHECK(!a.native());
    auto last = b.receive(wait_s(5));
    CHECK(last.ok() && last.value().size() == 4);
    CHECK(b.read(small, &len, wait_s(5)) == HQQ_ERR_PEER_CLOSED);
    CHECK(b.close().ok());
}

void test_standard_move_and_open_errors() {
    // Opening a missing name is a normal error, not an exception.
    auto missing = hqq::Link::open(unique_name("missing"), 4, 256);
    CHECK(!missing.ok());
    CHECK(missing.status() == HQQ_ERR_NOT_EXIST);
    CHECK(missing.status().transient_open());

    // Geometry mismatch fills OpenInfo.
    const std::string name = unique_name("geo");
    auto created = hqq::Link::create(name, 4, 256);
    CHECK_OK(created.status());
    hqq::OpenInfo info;
    auto mismatch = hqq::Link::open(name, 8, 256, hqq::LinkOptions{}, &info);
    CHECK(mismatch.status() == HQQ_ERR_GEOMETRY_MISMATCH);
    CHECK(info.got.buffer_count == 4);
    CHECK(info.want.buffer_count == 8);

    // Move: the source becomes empty and the destination owns the handle.
    hqq::Link moved = std::move(created).value();
    CHECK(moved.native() != nullptr);
    hqq::Link other = std::move(moved);
    CHECK(moved.native() == nullptr);
    CHECK(other.native() != nullptr);
}

// ---- Advanced layer -------------------------------------------------------------

constexpr std::uint64_t kUpper = 1;
constexpr std::uint64_t kFail = 2;
constexpr std::uint64_t kCaller = 3;

// Runs on worker threads: stateless, so it is thread-safe.
void rpc_handler(hqq::RequestContext& /*ctx*/, const hqq::Request& req, hqq::Response& resp) {
    if (req.method() == kUpper) {
        std::string s(req.data().as_string_view());
        for (char& c : s) {
            if (c >= 'a' && c <= 'z') c = static_cast<char>(c - 'a' + 'A');
        }
        resp.write(s);
    } else if (req.method() == kFail) {
        resp.error(42, std::string("bad request"));
    } else if (req.method() == kCaller) {
        std::string s = std::to_string(req.caller());
        if (req.trace()) s += "+trace";
        resp.write(s);
    } else {
        resp.error(HQQ_ERRCODE_INVALID_OP);
    }
}

void test_advanced_calls_and_streams() {
    const std::string name = unique_name("adv");
    auto created = hqq::AdvLink::create(name, 8, 4096);
    CHECK_OK(created.status());
    auto opened = hqq::AdvLink::open(name, 8, 4096);
    CHECK_OK(opened.status());
    if (!created.ok() || !opened.ok()) return;
    hqq::AdvLink& a = created.value();  // Primary
    hqq::AdvLink& b = opened.value();   // Secondary

    CHECK(a.protocol_state() == HQQ_PROTO_NONE);

    // Negotiation: the Primary sends the request, the Secondary answers.
    hqq::Status neg_b;
    std::thread neg_thread([&] { neg_b = b.negotiate(HQQ_FEATURES_ALL, wait_s(10)); });
    hqq::Status neg_a = a.negotiate(HQQ_FEATURES_ALL, wait_s(10));
    neg_thread.join();
    CHECK_OK(neg_a);
    CHECK_OK(neg_b);
    CHECK(a.protocol_state() == HQQ_PROTO_NEGOTIATED);
    CHECK(a.negotiated_version() == HQQ_PROTOCOL_1_1);
    CHECK(a.negotiated_features() == HQQ_FEATURES_ALL);
    CHECK(a.negotiate(HQQ_FEATURES_ALL, wait_s(1)) == HQQ_ERR_ALREADY_NEGOTIATED);

    // Calls: the Secondary serves on a worker thread, the Primary calls on stream 0.
    hqq::Status serve_st;
    std::thread server([&] { serve_st = b.serve(rpc_handler, wait_s(30)); });

    hqq::Stream s0 = a.stream0();
    hqq::Message resp;
    CHECK_OK(s0.call(kUpper, std::string("hello"), resp, wait_s(10)));
    CHECK(resp.data().as_string_view() == "HELLO");
    CHECK(resp.status() == 0);

    hqq::Status failed = s0.call(kFail, std::string(), resp, wait_s(10));
    CHECK(failed == HQQ_ERR_RESPONSE);
    CHECK(resp.status() == 42);
    CHECK(resp.data().as_string_view() == "bad request");

    // Request metadata reaches the handler: caller ID and trace ID.
    hqq::CallOptions co;
    co.caller = 7;
    co.trace[0] = 0xAB;
    CHECK_OK(s0.call_with(kCaller, std::string(), co, resp, wait_s(10)));
    CHECK(resp.data().as_string_view() == "7+trace");

    // Streams: the Primary listens and accepts, the Secondary opens and sends.
    CHECK_OK(a.listen_streams());
    CHECK(a.listen_streams() == HQQ_ERR_ALREADY_LISTENING);
    hqq::Status open_st;
    hqq::Status send_st;
    hqq::Stream sent;
    std::thread opener([&] {
        auto st = b.open_stream(wait_s(10));
        open_st = st.status();
        if (st.ok()) {
            sent = st.value();
            send_st = sent.send(std::string("stream payload"), wait_s(10));
        }
    });
    auto accepted = a.accept_stream(wait_s(10));
    opener.join();
    CHECK_OK(open_st);
    CHECK_OK(send_st);
    CHECK_OK(accepted.status());
    if (accepted.ok()) {
        auto msg = accepted.value().recv(wait_s(10));
        CHECK(msg.ok());
        if (msg.ok()) {
            CHECK(msg.value().data().as_string_view() == "stream payload");
            CHECK(msg.value().stream_id() == accepted.value().id());
        }
        CHECK_OK(accepted.value().close());
    }

    // Shutdown ends Serve with HQQ_OK; the link remains usable for closing.
    CHECK_OK(b.shutdown(wait_s(10)));
    server.join();
    CHECK_OK(serve_st);
    CHECK_OK(a.close());
    CHECK_OK(b.close());
}

void test_acceptor_and_connect() {
    const std::string listener = unique_name("lsn");
    auto acc = hqq::Acceptor::create(listener);
    CHECK_OK(acc.status());
    if (!acc.ok()) return;

    hqq::Result<hqq::AdvLink> accepted = hqq::Status(HQQ_ERR_TIMEOUT);
    std::thread srv([&] { accepted = acc.value().accept(wait_s(15)); });
    auto client = hqq::connect(listener, hqq::ConnectOptions{}, wait_s(15));
    srv.join();
    CHECK_OK(client.status());
    CHECK_OK(accepted.status());
    if (client.ok()) {
        CHECK(client.value().protocol_state() == HQQ_PROTO_NEGOTIATED);
    }
    if (accepted.ok()) {
        CHECK(accepted.value().protocol_state() == HQQ_PROTO_NEGOTIATED);
    }
    CHECK(acc.value().stats().accepted >= 1);
    CHECK_OK(acc.value().close());
}

// With HQQ_CPP_EXCEPTIONS=1 the value() of a failed Result throws hqq::Error.
void test_error_reporting() {
#if HQQ_CPP_EXCEPTIONS
    bool thrown = false;
    try {
        auto missing = hqq::Link::open(unique_name("throw"), 4, 256);
        (void)std::move(missing).value();
    } catch (const hqq::Error& e) {
        thrown = e.status() == HQQ_ERR_NOT_EXIST && e.code().value() == HQQ_ERR_NOT_EXIST;
    }
    CHECK(thrown);
#endif
    CHECK(hqq::Status(HQQ_ERR_UNSUPPORTED).error_code().message() ==
          std::string(hqq_strerror(HQQ_ERR_UNSUPPORTED)));
}

}  // namespace

int main() {
    test_status_and_views();
    test_standard_roundtrip();
    test_standard_move_and_open_errors();
    test_advanced_calls_and_streams();
    test_acceptor_and_connect();
    test_error_reporting();

    if (g_failures != 0) {
        std::fprintf(stderr, "hqq_cpp_test: %d of %d checks FAILED\n", g_failures, g_checks);
        return 1;
    }
    std::printf("hqq_cpp_test: %d checks passed (libhqq %.*s)\n", g_checks,
                static_cast<int>(hqq::version().size()), hqq::version().data());
    return 0;
}
