// hqq_cpp_echo - echo over a named Standard link (libhqq through hqq.hpp).
//
//   hqq_cpp_echo --create NAME [--timeout-s S]   server: echo every message until "END"
//   hqq_cpp_echo --open NAME [--messages N] [--size B] [--timeout-s S]
//                                                client: send N messages, check each echo, send "END"
//   hqq_cpp_echo --selftest                      both roles in one process (used by ctest)
//
// Message i of the client is its index (8 bytes, little endian) followed by a
// pattern, so every echo is checked byte for byte. Exit status: 0 success,
// 1 usage, 2 library error, 3 echo mismatch.
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <string_view>
#include <thread>
#include <vector>

#include "hqq/hqq.hpp"
#include "../support/process_id.hpp"

namespace {

constexpr std::string_view kEnd = "END";
constexpr std::uint64_t kBufferCount = 8;

enum Exit : int { kOk = 0, kUsage = 1, kLibrary = 2, kMismatch = 3 };

// Opens the named link as Secondary, retrying while the creator is not ready.
hqq::Result<hqq::Link> open_retrying(const std::string& name, std::uint64_t buffer_size,
                                     std::chrono::seconds budget) {
    const auto deadline = std::chrono::steady_clock::now() + budget;
    for (;;) {
        auto link = hqq::Link::open(name, kBufferCount, buffer_size);
        if (link.ok() || !link.status().transient_open() ||
            std::chrono::steady_clock::now() >= deadline) {
            return link;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
}

std::vector<std::byte> make_message(std::uint64_t index, std::size_t size) {
    std::vector<std::byte> msg(size);
    for (std::size_t i = 0; i < size; ++i) {
        const std::uint64_t byte = i < 8 ? (index >> (8 * i)) & 0xFFu : (index * 31 + i) & 0xFFu;
        msg[i] = static_cast<std::byte>(byte);
    }
    return msg;
}

// Echoes every message back until END; returns the number of echoed messages.
hqq::Result<std::uint64_t> serve_echo(hqq::Link& link, std::chrono::seconds timeout) {
    std::uint64_t echoed = 0;
    for (;;) {
        auto msg = link.receive(hqq::Wait::for_duration(timeout));
        if (!msg.ok()) {
            return msg.status();
        }
        const auto& bytes = msg.value();
        if (std::string_view(reinterpret_cast<const char*>(bytes.data()), bytes.size()) == kEnd) {
            return echoed;
        }
        hqq::Status st = link.write(bytes, hqq::Wait::for_duration(timeout));
        if (!st.ok()) {
            return st;
        }
        ++echoed;
    }
}

int run_server(const std::string& name, std::chrono::seconds timeout) {
    auto created = hqq::Link::create(name, kBufferCount, 4096);
    if (!created.ok()) {
        std::fprintf(stderr, "create %s: %s\n", name.c_str(), created.status().message());
        return kLibrary;
    }
    auto echoed = serve_echo(created.value(), timeout);
    if (!echoed.ok()) {
        std::fprintf(stderr, "serve: %s\n", echoed.status().message());
        return kLibrary;
    }
    std::printf("echoed %llu messages\n", static_cast<unsigned long long>(echoed.value()));
    return kOk;
}

int run_client(const std::string& name, std::uint64_t messages, std::size_t size,
               std::chrono::seconds timeout) {
    auto opened = open_retrying(name, 4096, timeout);
    if (!opened.ok()) {
        std::fprintf(stderr, "open %s: %s\n", name.c_str(), opened.status().message());
        return kLibrary;
    }
    hqq::Link& link = opened.value();
    const auto wait = hqq::Wait::for_duration(timeout);
    for (std::uint64_t i = 0; i < messages; ++i) {
        const std::size_t n = (i % 10 == 9) ? 4096 : (size == 0 ? 16 : size);
        auto out = make_message(i, n);
        hqq::Status st = link.write(out, wait);
        if (!st.ok()) {
            std::fprintf(stderr, "write %llu: %s\n", static_cast<unsigned long long>(i), st.message());
            return kLibrary;
        }
        auto back = link.receive(wait);
        if (!back.ok()) {
            std::fprintf(stderr, "receive %llu: %s\n", static_cast<unsigned long long>(i),
                         back.status().message());
            return kLibrary;
        }
        if (back.value() != out) {
            std::fprintf(stderr, "echo %llu differs\n", static_cast<unsigned long long>(i));
            return kMismatch;
        }
    }
    hqq::Status st = link.write(kEnd, wait);
    if (!st.ok()) {
        std::fprintf(stderr, "write END: %s\n", st.message());
        return kLibrary;
    }
    std::printf("verified %llu echoes\n", static_cast<unsigned long long>(messages));
    return kOk;
}

int run_selftest() {
    const std::string name = "hqqcpp-echo-" + std::to_string(hqq_support::process_id());
    int server_rc = kOk;
    std::thread server([&] { server_rc = run_server(name, std::chrono::seconds(20)); });
    const int client_rc = run_client(name, 25, 300, std::chrono::seconds(20));
    server.join();
    return client_rc != kOk ? client_rc : server_rc;
}

void usage() {
    std::fprintf(stderr,
                 "usage: hqq_cpp_echo (--create | --open) NAME [--messages N] [--size B] [--timeout-s S]\n"
                 "       hqq_cpp_echo --selftest\n");
}

}  // namespace

int main(int argc, char** argv) {
    std::string name;
    bool create = false;
    bool open = false;
    bool selftest = false;
    std::uint64_t messages = 100;
    std::size_t size = 64;
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
        } else if (arg == "--messages") {
            messages = std::strtoull(next(), nullptr, 10);
        } else if (arg == "--size") {
            size = std::strtoull(next(), nullptr, 10);
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
    return create ? run_server(name, timeout) : run_client(name, messages, size, timeout);
}
