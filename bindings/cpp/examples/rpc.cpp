// hqq_cpp_rpc - request/response over a named Advanced link (libhqq through hqq.hpp).
//
//   hqq_cpp_rpc --create NAME [--timeout-s S]   service: negotiate, serve until method 3 (stop)
//   hqq_cpp_rpc --open NAME [--timeout-s S]     client: negotiate, call the methods, stop the service
//   hqq_cpp_rpc --selftest                      both roles in one process (used by ctest)
//
// Methods: 1 = upper-case the request body (response), 2 = error response
// (status 42, payload "bad request"), 3 = stop the service (response "stopping").
// Exit status: 0 success, 1 usage, 2 library error, 3 unexpected response.
#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <string>
#include <string_view>
#include <thread>

#include <unistd.h>

#include "hqq/hqq.hpp"

namespace {

constexpr std::uint64_t kUpper = 1;
constexpr std::uint64_t kFail = 2;
constexpr std::uint64_t kStop = 3;
constexpr std::uint64_t kFailStatus = 42;

enum Exit : int { kOk = 0, kUsage = 1, kLibrary = 2, kUnexpected = 3 };

// Handlers run on worker threads: they touch only the atomic flag.
void handle(hqq::RequestContext& /*ctx*/, const hqq::Request& req, hqq::Response& resp,
            std::atomic<bool>& stop) {
    switch (req.method()) {
    case kUpper: {
        std::string s(req.data().as_string_view());
        for (char& c : s) {
            if (c >= 'a' && c <= 'z') c = static_cast<char>(c - 'a' + 'A');
        }
        resp.write(s);
        break;
    }
    case kFail:
        resp.error(kFailStatus, std::string("bad request"));
        break;
    case kStop:
        resp.write(std::string("stopping"));
        stop.store(true);
        break;
    default:
        resp.error(HQQ_ERRCODE_INVALID_OP);
        break;
    }
}

hqq::Result<hqq::AdvLink> open_retrying(const std::string& name, std::chrono::seconds budget) {
    const auto deadline = std::chrono::steady_clock::now() + budget;
    for (;;) {
        auto link = hqq::AdvLink::open(name, 8, 4096);
        if (link.ok() || !link.status().transient_open() ||
            std::chrono::steady_clock::now() >= deadline) {
            return link;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
}

int run_service(const std::string& name, std::chrono::seconds timeout) {
    auto created = hqq::AdvLink::create(name, 8, 4096);
    if (!created.ok()) {
        std::fprintf(stderr, "create %s: %s\n", name.c_str(), created.status().message());
        return kLibrary;
    }
    hqq::AdvLink& link = created.value();
    hqq::Status neg = link.negotiate(HQQ_FEATURES_ALL, hqq::Wait::for_duration(timeout));
    if (!neg.ok()) {
        std::fprintf(stderr, "negotiate: %s\n", neg.message());
        return kLibrary;
    }

    std::atomic<bool> stop{false};
    hqq::Status serve_st;
    std::thread server([&] {
        serve_st = link.serve(
            [&stop](hqq::RequestContext& ctx, const hqq::Request& req, hqq::Response& resp) {
                handle(ctx, req, resp, stop);
            },
            hqq::Wait::for_duration(timeout));
    });

    const auto deadline = std::chrono::steady_clock::now() + timeout;
    while (!stop.load() && std::chrono::steady_clock::now() < deadline) {
        std::this_thread::sleep_for(std::chrono::milliseconds(2));
    }
    // Graceful shutdown: refuses new requests and waits for the in-flight stop response.
    hqq::Status shut = link.shutdown(hqq::Wait::for_duration(timeout));
    server.join();
    // The client closes its link right after the stop response, which ends serving as PeerClosed.
    const bool served_ok = serve_st.ok() || serve_st == HQQ_ERR_PEER_CLOSED;
    if (!shut.ok() || !served_ok) {
        std::fprintf(stderr, "shutdown: %s / serve: %s\n", shut.message(), serve_st.message());
        return kLibrary;
    }
    std::printf("service stopped (%s)\n", stop.load() ? "by client" : "by timeout");
    return stop.load() ? kOk : kUnexpected;
}

int run_client(const std::string& name, std::chrono::seconds timeout) {
    auto opened = open_retrying(name, timeout);
    if (!opened.ok()) {
        std::fprintf(stderr, "open %s: %s\n", name.c_str(), opened.status().message());
        return kLibrary;
    }
    hqq::AdvLink& link = opened.value();
    const auto wait = hqq::Wait::for_duration(timeout);
    hqq::Status neg = link.negotiate(HQQ_FEATURES_ALL, wait);
    if (!neg.ok()) {
        std::fprintf(stderr, "negotiate: %s\n", neg.message());
        return kLibrary;
    }

    hqq::Stream stream = link.stream0();
    hqq::Message resp;

    hqq::Status st = stream.call(kUpper, std::string("hello, rpc"), resp, wait);
    if (!st.ok() || resp.data().as_string_view() != "HELLO, RPC") {
        std::fprintf(stderr, "upper: %s\n", st.message());
        return kUnexpected;
    }
    std::printf("upper: %.*s\n", static_cast<int>(resp.data().size()),
                static_cast<const char*>(resp.data().data()));

    st = stream.call(kFail, std::string(), resp, wait);
    if (st != HQQ_ERR_RESPONSE || resp.status() != kFailStatus) {
        std::fprintf(stderr, "fail: %s status=%llu\n", st.message(),
                     static_cast<unsigned long long>(resp.status()));
        return kUnexpected;
    }
    std::printf("error response: status %llu, payload \"%.*s\"\n",
                static_cast<unsigned long long>(resp.status()),
                static_cast<int>(resp.data().size()), static_cast<const char*>(resp.data().data()));

    st = stream.call(kStop, std::string(), resp, wait);
    if (!st.ok()) {
        std::fprintf(stderr, "stop: %s\n", st.message());
        return kUnexpected;
    }
    return kOk;
}

int run_selftest() {
    const std::string name = "hqqcpp-rpc-" + std::to_string(static_cast<long>(::getpid()));
    int service_rc = kOk;
    std::thread service([&] { service_rc = run_service(name, std::chrono::seconds(20)); });
    const int client_rc = run_client(name, std::chrono::seconds(20));
    service.join();
    return client_rc != kOk ? client_rc : service_rc;
}

void usage() {
    std::fprintf(stderr,
                 "usage: hqq_cpp_rpc (--create | --open) NAME [--timeout-s S]\n"
                 "       hqq_cpp_rpc --selftest\n");
}

}  // namespace

int main(int argc, char** argv) {
    std::string name;
    bool create = false;
    bool open = false;
    bool selftest = false;
    long timeout_s = 30;

    for (int i = 1; i < argc; ++i) {
        const std::string_view arg(argv[i]);
        auto next = [&]() -> const char* {
            if (i + 1 >= argc) {
                usage();
                std::exit(kUsage);
            }
            return argv[++i];
        };
        if (arg == "--create") {
            create = true;
            name = next();
        } else if (arg == "--open") {
            open = true;
            name = next();
        } else if (arg == "--selftest") {
            selftest = true;
        } else if (arg == "--timeout-s") {
            timeout_s = std::strtol(next(), nullptr, 10);
        } else {
            usage();
            return kUsage;
        }
    }
    const auto timeout = std::chrono::seconds(timeout_s > 0 ? timeout_s : 30);
    if (selftest) {
        return run_selftest();
    }
    if (create == open) {
        usage();
        return kUsage;
    }
    return create ? run_service(name, timeout) : run_client(name, timeout);
}
