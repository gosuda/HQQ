// hqq_interop.cpp - the hqq-interop helper of the C++ binding.
//
// One invocation runs one role of one scenario in this process, as specified by
// conformance/INTEROP_CLI.md. Scenario checks and payloads follow the Go
// reference (cmd/hqq-interop); the wire behaviour is that of libhqq, reached
// through the header-only wrapper hqq/hqq.hpp.
//
// Subcommands: run, list [--impl LABEL], version. Stdout carries machine output
// only; the result line of `run` is the last line on stdout. Diagnostics go to
// stderr.
//
// Only the scenarios and variants listed by `list` are implemented. Variants that
// need raw protocol frames (OpCancel, OpConnCreate, OpProtoAck with a bad major,
// OpError, abort frames), the raw segment view, or the platform-specific
// primitives are not listed; the driver never asks for them.

#include "hqq/hqq.hpp"

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <csignal>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#if !defined(_WIN32)
#  include <time.h>
#endif
#include <filesystem>
#include <map>
#include <memory>
#include <mutex>
#include <optional>
#include <set>
#include <string>
#include <string_view>
#include <thread>
#include <utility>
#include <vector>

#if defined(_WIN32)
#  ifndef NOMINMAX
#    define NOMINMAX
#  endif
#  include <windows.h>
#endif

namespace {

using namespace std::chrono_literals;
using Clock = std::chrono::steady_clock;
using Nanos = std::chrono::nanoseconds;

constexpr const char* kDefaultImpl = "cpp";
constexpr const char* kProtocol = "1.1";
constexpr const char* kSegmentLayout = "HQQ_STL3";
constexpr int kRingLayout = 2;

constexpr int kExitOK = 0;
constexpr int kExitFailed = 1;
constexpr int kExitUsage = 2;
constexpr int kExitTimeout = 3;

// Slack added to the bounds the scenarios assert (Go: stdSlack).
constexpr Nanos kStdSlack = 1500ms;

// Advanced scenario constants (Go: adv_setup.go, adv_rpc.go).
constexpr std::uint64_t kMethodSleepEcho = 1;  // X-A04: sleeps inversely to the request id, echoes
constexpr std::uint64_t kMethodFail = 2;       // X-A04: error response with status 42 and payload
constexpr std::uint64_t kMethodSlow = 3;       // X-A04: answers after 300 ms
constexpr std::uint64_t kMethodEcho = 4;       // echoes the payload
constexpr std::uint64_t kMethodBlock = 7;      // X-A07/X-A09: blocks until released
constexpr std::uint64_t kMethodMeta = 8;       // X-A08: records caller, trace and deadline
constexpr int kRPCConcurrency = 16;
constexpr int kAdvClients = 3;

// ---- Results, failures and output -------------------------------------------

// A role error: the process fails with error_kind `kind` (INTEROP_CLI.md 4.1).
struct Failure {
    std::string kind;
    std::string text;
    std::optional<std::uint64_t> status;  // ResponseError: the status of the error response
};

// Maps a wrapper status to its canonical error_kind (INTEROP_CLI.md 4.1; Go errors.go).
std::string kind_of(hqq_status code) {
    switch (code) {
    case HQQ_ERR_LISTENER_BUSY: return "ErrListenerBusy";
    case HQQ_ERR_LISTENER_GONE: return "ErrListenerGone";
    case HQQ_ERR_LISTENER_CLOSED: return "ErrListenerClosed";
    case HQQ_ERR_ACCEPTOR_CLOSED: return "ErrAcceptorClosed";
    case HQQ_ERR_SHUTTING_DOWN: return "ErrShuttingDown";
    case HQQ_ERR_PEER_CLOSED: return "ErrPeerClosed";
    case HQQ_ERR_PEER_GONE: return "ErrPeerGone";
    case HQQ_ERR_CLOSED: return "ErrClosed";
    case HQQ_ERR_TIMEOUT: return "ErrTimeout";
    case HQQ_ERR_LINK_BUSY: return "ErrLinkBusy";
    case HQQ_ERR_INCOMPATIBLE_LAYOUT: return "ErrIncompatibleLayout";
    case HQQ_ERR_NOT_NEGOTIATED: return "ErrNotNegotiated";
    case HQQ_ERR_ALREADY_NEGOTIATED: return "ErrAlreadyNegotiated";
    case HQQ_ERR_NEGOTIATION_FAILED: return "ErrNegotiationFailed";
    case HQQ_ERR_NEGOTIATION_REJECTED: return "ErrNegotiationRejected";
    case HQQ_ERR_UNEXPECTED_PACKET: return "ErrUnexpectedPacket";
    case HQQ_ERR_NOT_LISTENING: return "ErrNotListening";
    case HQQ_ERR_ALREADY_LISTENING: return "ErrAlreadyListening";
    case HQQ_ERR_MESSAGE_ABORTED: return "ErrMessageAborted";
    case HQQ_ERR_PROTOCOL: return "ErrProtocol";
    case HQQ_ERR_INCOMING_OVERFLOW: return "ErrIncomingOverflow";
    case HQQ_ERR_REASSEMBLY_LIMIT: return "ErrReassemblyLimit";
    case HQQ_ERR_BUFFER_OVERFLOW: return "ErrBufferOverflow";
    case HQQ_ERR_INVALID_SIZE:
    case HQQ_ERR_GEOMETRY_MISMATCH: return "ErrInvalidSize";
    case HQQ_ERR_MEMORY_ALIGN: return "ErrMemoryAlign";
    case HQQ_ERR_MEMORY_SMALL: return "ErrMemorySmall";
    case HQQ_ERR_INVALID_NAME: return "ErrInvalidName";
    case HQQ_ERR_INVALID_OP: return "ErrInvalidOp";
    case HQQ_ERR_FAILED_INIT:
    case HQQ_ERR_SEGMENT_NOT_READY:
    case HQQ_ERR_INIT_CONTENDED:
    case HQQ_ERR_NO_LIVE_ENDPOINT:
    case HQQ_ERR_LISTENER_NOT_READY: return "ErrFailedInit";
    case HQQ_ERR_CONN_NOT_FOUND: return "ErrConnNotFound";
    case HQQ_ERR_NOT_EXIST: return "ErrNotExist";
    case HQQ_ERR_RESPONSE: return "ResponseError";
    default: return "Unknown";
    }
}

[[noreturn]] void fail_status(const char* what, hqq::Status st, std::optional<std::uint64_t> status = {}) {
    throw Failure{kind_of(st.native()), std::string(what) + ": " + st.message(), status};
}

// Throws the failure of st (a call that must succeed). For a response error the
// status of resp is reported in meta.status.
void must(const char* what, hqq::Status st, const hqq::Message* resp = nullptr) {
    if (st.ok()) {
        return;
    }
    if (st == HQQ_ERR_RESPONSE && resp != nullptr) {
        fail_status(what, st, resp->status());
    }
    fail_status(what, st);
}

// The value of a Result that must succeed.
template <class T>
T take(hqq::Result<T> r, const char* what) {
    if (!r.ok()) {
        fail_status(what, r.status());
    }
    return std::move(r).value();
}

using Bytes = hqq::Bytes;
using MutableBytes = hqq::MutableBytes;
using Byte = std::byte;

Bytes view(const std::vector<Byte>& v) { return Bytes(v.data(), v.size()); }

std::vector<Byte> to_bytes(std::string_view s) {
    std::vector<Byte> out(s.size());
    for (std::size_t i = 0; i < s.size(); ++i) {
        out[i] = Byte(static_cast<unsigned char>(s[i]));
    }
    return out;
}

Bytes sv_view(std::string_view s) { return Bytes(s.data(), s.size()); }

// The monotonic clock of libhqq (hqq_plat_mono_ns): the deadlines of request
// contexts are on this clock, which is not std::chrono::steady_clock's epoch.
std::uint64_t mono_ns() {
#if defined(_WIN32)
    LARGE_INTEGER freq;
    LARGE_INTEGER count;
    QueryPerformanceFrequency(&freq);
    QueryPerformanceCounter(&count);
    const auto f = static_cast<std::uint64_t>(freq.QuadPart);
    const auto c = static_cast<std::uint64_t>(count.QuadPart);
    return (c / f) * 1'000'000'000ull + (c % f) * 1'000'000'000ull / f;
#else
    timespec ts{};
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return static_cast<std::uint64_t>(ts.tv_sec) * 1'000'000'000ull + static_cast<std::uint64_t>(ts.tv_nsec);
#endif
}

// ---- JSON ---------------------------------------------------------------------

std::string json_str(std::string_view s) {
    std::string out = "\"";
    for (unsigned char c : s) {
        switch (c) {
        case '"': out += "\\\""; break;
        case '\\': out += "\\\\"; break;
        case '\n': out += "\\n"; break;
        case '\r': out += "\\r"; break;
        case '\t': out += "\\t"; break;
        default:
            if (c < 0x20) {
                char buf[8];
                std::snprintf(buf, sizeof buf, "\\u%04x", static_cast<unsigned>(c));
                out += buf;
            } else {
                out += static_cast<char>(c);
            }
        }
    }
    out += '"';
    return out;
}

std::string bool_json(bool b) { return b ? "true" : "false"; }

std::string join(const std::vector<std::string>& parts, const char* sep) {
    std::string out;
    for (std::size_t i = 0; i < parts.size(); ++i) {
        if (i != 0) {
            out += sep;
        }
        out += parts[i];
    }
    return out;
}

// ---- Payloads (INTEROP_CLI.md section 5) ---------------------------------------

std::uint64_t splitmix_next(std::uint64_t& s) {
    s += 0x9E3779B97F4A7C15ull;
    std::uint64_t z = s;
    z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ull;
    z = (z ^ (z >> 27)) * 0x94D049BB133111EBull;
    return z ^ (z >> 31);
}

// body(n): the first n bytes of the SplitMix64 stream of (seed, seq).
void fill_body(std::uint64_t seed, std::uint64_t seq, Byte* out, std::size_t n) {
    std::uint64_t s = seed ^ (seq * 0xD1B54A32D192ED03ull);
    std::size_t i = 0;
    while (i < n) {
        const std::uint64_t w = splitmix_next(s);
        for (int k = 0; k < 8 && i < n; ++k, ++i) {
            out[i] = Byte(static_cast<unsigned char>((w >> (8 * k)) & 0xFFu));
        }
    }
}

std::uint32_t crc32_ieee(const Byte* p, std::size_t n) {
    static const std::array<std::uint32_t, 256> table = [] {
        std::array<std::uint32_t, 256> t{};
        for (std::uint32_t i = 0; i < 256; ++i) {
            std::uint32_t c = i;
            for (int k = 0; k < 8; ++k) {
                c = (c & 1u) != 0 ? (0xEDB88320u ^ (c >> 1)) : (c >> 1);
            }
            t[i] = c;
        }
        return t;
    }();
    std::uint32_t c = 0xFFFFFFFFu;
    for (std::size_t i = 0; i < n; ++i) {
        c = table[(c ^ std::to_integer<std::uint32_t>(p[i])) & 0xFFu] ^ (c >> 8);
    }
    return c ^ 0xFFFFFFFFu;
}

// Payload(seed, seq, size): size 0 empty; 1..15 body(size); >= 16 seq (8, LE) |
// crc32(body) (4, LE) | body(size - 12).
std::vector<Byte> payload(std::uint64_t seed, std::uint64_t seq, std::size_t size) {
    std::vector<Byte> out(size);
    if (size == 0) {
        return out;
    }
    if (size < 16) {
        fill_body(seed, seq, out.data(), size);
        return out;
    }
    std::vector<Byte> body(size - 12);
    fill_body(seed, seq, body.data(), body.size());
    for (std::size_t i = 0; i < 8; ++i) {
        out[i] = Byte(static_cast<unsigned char>((seq >> (8 * i)) & 0xFFu));
    }
    const std::uint32_t crc = crc32_ieee(body.data(), body.size());
    for (std::size_t i = 0; i < 4; ++i) {
        out[8 + i] = Byte(static_cast<unsigned char>((crc >> (8 * i)) & 0xFFu));
    }
    std::copy(body.begin(), body.end(), out.begin() + 12);
    return out;
}

// True when got is exactly Payload(seed, seq, got.size()).
bool intact(std::uint64_t seed, std::uint64_t seq, Bytes got) {
    const std::vector<Byte> want = payload(seed, seq, got.size());
    return got.size() == want.size() && (want.empty() || std::memcmp(got.data(), want.data(), want.size()) == 0);
}

// ---- Configuration ----------------------------------------------------------------

struct Config {
    std::string scenario;
    std::string variant;
    int step = 1;
    std::string role;  // "creator" or "opener"
    std::string name;
    std::string sync_dir;
    std::string impl = kDefaultImpl;
    std::string peer_impl;
    long long buffer_count = 4;
    long long buffer_size = 256;
    long long size = 0;  // 0: buffer_size
    long long messages = 100;
    std::uint64_t seed = 1;
    double timeout_s = 20;
    std::string crash_after;
    long long heartbeat_ms = 0;
    long long peer_timeout_ms = 0;
    long long attach_timeout_ms = 0;
    std::string wait = "adaptive";
    bool long_soak = false;
    std::uint64_t max_message = 0;
    std::uint64_t max_reassembly = 0;
    long long queue_size = 0;
    std::set<std::string> explicit_flags;  // flags given on the command line
};

// Per-variant parameters (INTEROP_CLI.md section 7). Zero keeps the helper default.
struct Params {
    long long buffer_count = 0;
    long long buffer_size = 0;
    long long size = 0;
    long long messages = 0;
    std::uint64_t seed = 0;
    long long heartbeat_ms = 0;
    long long peer_timeout_ms = 0;
    long long attach_timeout_ms = 0;
    std::uint64_t max_message = 0;
    std::uint64_t max_reassembly = 0;
    long long queue_size = 0;
};

struct FlagSpec {
    const char* name;
    bool takes_value;
};

const std::vector<FlagSpec>& run_flags() {
    static const std::vector<FlagSpec> specs = {
        {"scenario", true},     {"variant", true},      {"step", true},
        {"role", true},         {"name", true},         {"sync-dir", true},
        {"impl", true},         {"peer-impl", true},    {"buffer-count", true},
        {"buffer-size", true},  {"size", true},         {"messages", true},
        {"seed", true},         {"timeout", true},      {"crash-after", true},
        {"heartbeat", true},    {"peer-timeout", true}, {"attach-timeout", true},
        {"wait", true},         {"long", false},        {"max-message", true},
        {"max-reassembly", true}, {"queue-size", true},
    };
    return specs;
}

// Parses "--name value" and "--name=value" into out. Returns an error message or "".
std::string parse_flags(const std::vector<std::string>& args, const std::vector<FlagSpec>& specs,
                        std::map<std::string, std::string>& out) {
    for (std::size_t i = 0; i < args.size(); ++i) {
        const std::string& a = args[i];
        if (a.size() < 3 || a.compare(0, 2, "--") != 0) {
            return "unexpected argument: " + a;
        }
        std::string name = a.substr(2);
        std::optional<std::string> inline_value;
        if (const auto eq = name.find('='); eq != std::string::npos) {
            inline_value = name.substr(eq + 1);
            name.resize(eq);
        }
        const FlagSpec* spec = nullptr;
        for (const auto& s : specs) {
            if (name == s.name) {
                spec = &s;
            }
        }
        if (spec == nullptr) {
            return "unknown flag: --" + name;
        }
        if (!spec->takes_value) {
            if (inline_value) {
                return "flag --" + name + " takes no value";
            }
            out[name] = "";
            continue;
        }
        if (inline_value) {
            out[name] = *inline_value;
        } else {
            if (i + 1 >= args.size()) {
                return "flag --" + name + " needs a value";
            }
            out[name] = args[++i];
        }
    }
    return "";
}

bool parse_ll(const std::string& s, long long& out) {
    try {
        std::size_t pos = 0;
        out = std::stoll(s, &pos, 10);
        return pos == s.size();
    } catch (...) {
        return false;
    }
}

bool parse_u64(const std::string& s, std::uint64_t& out) {
    if (s.empty() || s[0] == '-') {
        return false;
    }
    try {
        std::size_t pos = 0;
        out = std::stoull(s, &pos, 10);
        return pos == s.size();
    } catch (...) {
        return false;
    }
}

bool parse_seconds(const std::string& s, double& out) {
    try {
        std::size_t pos = 0;
        out = std::stod(s, &pos);
        return pos == s.size() && out > 0;
    } catch (...) {
        return false;
    }
}

// Builds the config of `run` from its flags. Returns an error message or "".
std::string parse_run(const std::vector<std::string>& args, Config& c) {
    std::map<std::string, std::string> v;
    if (std::string err = parse_flags(args, run_flags(), v); !err.empty()) {
        return err;
    }
    for (const auto& kv : v) {
        c.explicit_flags.insert(kv.first);
    }
    std::string err;
    auto str = [&](const char* f, std::string& out) {
        if (auto it = v.find(f); it != v.end()) {
            out = it->second;
        }
    };
    auto num = [&](const char* f, long long& out) {
        if (auto it = v.find(f); it != v.end() && err.empty() && !parse_ll(it->second, out)) {
            err = std::string("invalid integer for --") + f;
        }
    };
    auto u64 = [&](const char* f, std::uint64_t& out) {
        if (auto it = v.find(f); it != v.end() && err.empty() && !parse_u64(it->second, out)) {
            err = std::string("invalid unsigned integer for --") + f;
        }
    };
    str("scenario", c.scenario);
    str("variant", c.variant);
    str("role", c.role);
    str("name", c.name);
    str("sync-dir", c.sync_dir);
    str("impl", c.impl);
    str("peer-impl", c.peer_impl);
    str("crash-after", c.crash_after);
    str("wait", c.wait);
    long long step = c.step;
    num("step", step);
    c.step = static_cast<int>(step);
    num("buffer-count", c.buffer_count);
    num("buffer-size", c.buffer_size);
    num("size", c.size);
    num("messages", c.messages);
    u64("seed", c.seed);
    num("heartbeat", c.heartbeat_ms);
    num("peer-timeout", c.peer_timeout_ms);
    num("attach-timeout", c.attach_timeout_ms);
    u64("max-message", c.max_message);
    u64("max-reassembly", c.max_reassembly);
    num("queue-size", c.queue_size);
    if (auto it = v.find("timeout"); it != v.end() && err.empty() && !parse_seconds(it->second, c.timeout_s)) {
        err = "--timeout must be a positive number of seconds";
    }
    c.long_soak = v.count("long") != 0;
    if (!err.empty()) {
        return err;
    }
    if (c.scenario.empty()) {
        return "--scenario is required";
    }
    if (c.name.empty()) {
        return "--name is required";
    }
    if (c.role == "a") {
        c.role = "creator";
    } else if (c.role == "b") {
        c.role = "opener";
    }
    if (c.role != "creator" && c.role != "opener") {
        return "--role must be creator|opener|a|b, got \"" + c.role + "\"";
    }
    if (c.step < 1) {
        return "--step must be >= 1";
    }
    if (c.messages < 0 || c.buffer_count <= 0 || c.buffer_size <= 0 || c.size < 0) {
        return "sizes and counts must be positive";
    }
    if (c.wait != "adaptive" && c.wait != "spin") {
        return "--wait must be adaptive|spin, got \"" + c.wait + "\"";
    }
    return "";
}

// ---- Catalogue (INTEROP_CLI.md section 7) ----------------------------------------

using ExpectMap = std::map<std::string, std::string>;
using ArgsMap = std::map<std::string, std::vector<std::string>>;

struct StepDef {
    std::vector<std::string> roles;
    ExpectMap expect;
    ArgsMap args;
};

class Env;
using RunFn = void (*)(Env&);

struct VariantDef {
    std::string name;
    Params params;
    std::vector<StepDef> steps;
};

struct ScenarioDef {
    std::string id;
    std::string title;
    std::string kind;
    long long timeout_s;
    std::vector<VariantDef> variants;
    RunFn run;
    // Listed by `list`. The driver runs a scenario only when both helpers list
    // identical variants and steps; a scenario with a different variant set is
    // not listed (it still runs through `run`).
    bool listed = true;
};

// ---- Environment of one role ------------------------------------------------------

struct Check {
    std::string name;
    bool ok;
    std::string detail;
};

class Env {
public:
    explicit Env(Config c)
        : cfg(std::move(c)),
          start(Clock::now()),
          deadline(start + std::chrono::duration_cast<Clock::duration>(
                               std::chrono::duration<double>(cfg.timeout_s))) {}
    Env(const Env&) = delete;
    Env& operator=(const Env&) = delete;

    Config cfg;
    Clock::time_point start;
    Clock::time_point deadline;  // the process deadline (--timeout)
    std::atomic<long long> sent{0};
    std::atomic<long long> received{0};

    bool is_creator() const { return cfg.role == "creator"; }

    void check(std::string name, bool ok, std::string detail = {}) {
        std::lock_guard<std::mutex> lk(mu_);
        checks_.push_back(Check{std::move(name), ok, std::move(detail)});
        if (!ok) {
            failed_ = true;
        }
    }

    // Records a check that st matches want (Go errors.Is); HQQ_OK means st must succeed.
    void check_status(const std::string& name, hqq::Status st, hqq_status want) {
        const bool ok = want == HQQ_OK ? st.ok() : st.matches(want);
        check(name, ok, st.ok() ? std::string() : std::string(st.message()));
    }

    bool failed() const {
        std::lock_guard<std::mutex> lk(mu_);
        return failed_;
    }

    void add_meta(std::string key, std::string json) {
        std::lock_guard<std::mutex> lk(mu_);
        meta_.emplace_back(std::move(key), std::move(json));
    }

    // Bound of every blocking call: the time left until the process deadline.
    hqq::Wait wait() const { return hqq::Wait{remaining().count(), nullptr}; }

    Nanos remaining() const {
        const auto left = std::chrono::duration_cast<Nanos>(deadline - Clock::now());
        return left.count() > 0 ? left : Nanos(1);
    }

    // Barrier of INTEROP_CLI.md section 6: create my marker, wait for the peer's.
    void barrier(const char* phase) {
        namespace fs = std::filesystem;
        const std::string base = "s" + std::to_string(cfg.step) + "-" + phase;
        const fs::path dir(cfg.sync_dir);
        touch(dir / (base + "." + cfg.role));
        const fs::path peer = dir / (base + "." + (is_creator() ? "opener" : "creator"));
        for (;;) {
            std::error_code ec;
            if (fs::exists(peer, ec)) {
                return;
            }
            if (Clock::now() >= deadline) {
                throw Failure{"ErrTimeout", std::string("barrier ") + phase + ": waiting for peer", std::nullopt};
            }
            std::this_thread::sleep_for(std::chrono::milliseconds(2));
        }
    }

    // Prints the result line and kills this process with SIGKILL when point is the
    // configured crash point (INTEROP_CLI.md section 8). Returns otherwise.
    void maybe_crash(const std::string& point);

    // Result line of this role (see render_result).
    std::string render(bool ok, bool crashed, const std::string& kind, const std::string& error,
                       std::optional<std::uint64_t> status) const;

private:
    static void touch(const std::filesystem::path& p) {
        std::FILE* f = nullptr;
#if defined(_WIN32)
        // fopen is deprecated on Windows; with clang-cl and -Werror that is an error.
        if (fopen_s(&f, p.string().c_str(), "wb") != 0) f = nullptr;
#else
        f = std::fopen(p.string().c_str(), "wb");
#endif
        if (f == nullptr) {
            throw Failure{"Unknown", "barrier: cannot create " + p.string(), std::nullopt};
        }
        std::fclose(f);
    }

    mutable std::mutex mu_;  // guards checks_, failed_, meta_
    std::vector<Check> checks_;
    bool failed_ = false;
    std::vector<std::pair<std::string, std::string>> meta_;  // value: JSON text
};

// Serialises the result line; the checks and meta are read under the env lock.
std::string Env::render(bool ok, bool crashed, const std::string& kind, const std::string& error,
                        std::optional<std::uint64_t> status) const {
    std::lock_guard<std::mutex> lk(mu_);
    std::string out = "{\"scenario\":" + json_str(cfg.scenario) + ",\"variant\":" + json_str(cfg.variant) +
                      ",\"step\":" + std::to_string(cfg.step) + ",\"role\":" + json_str(cfg.role) +
                      ",\"impl\":" + json_str(cfg.impl) + ",\"peer_impl\":" + json_str(cfg.peer_impl) +
                      ",\"ok\":" + bool_json(ok) + ",\"skipped\":false,\"crashed\":" + bool_json(crashed) +
                      ",\"error_kind\":" + json_str(kind) + ",\"error\":" + json_str(error) + ",\"checks\":[";
    for (std::size_t i = 0; i < checks_.size(); ++i) {
        const Check& c = checks_[i];
        if (i != 0) {
            out += ',';
        }
        out += "{\"name\":" + json_str(c.name) + ",\"ok\":" + bool_json(c.ok);
        if (!c.detail.empty()) {
            out += ",\"detail\":" + json_str(c.detail);
        }
        out += '}';
    }
    const auto elapsed = std::chrono::duration_cast<std::chrono::milliseconds>(Clock::now() - start).count();
    out += "],\"sent\":" + std::to_string(sent.load()) + ",\"received\":" + std::to_string(received.load()) +
           ",\"elapsed_ms\":" + std::to_string(elapsed);
    std::vector<std::string> meta;
    for (const auto& m : meta_) {
        meta.push_back(json_str(m.first) + ":" + m.second);
    }
    if (status) {
        meta.push_back("\"status\":" + std::to_string(*status));
    }
    if (!meta.empty()) {
        out += ",\"meta\":{" + join(meta, ",") + "}";
    }
    out += "}";
    return out;
}

std::mutex g_out_mu;
bool g_printed = false;

// Writes one result line to stdout. Only the first call prints; returns whether it did.
bool emit_once(const std::string& line) {
    std::lock_guard<std::mutex> lk(g_out_mu);
    if (g_printed) {
        return false;
    }
    g_printed = true;
    std::fwrite(line.data(), 1, line.size(), stdout);
    std::fputc('\n', stdout);
    std::fflush(stdout);
    return true;
}

[[noreturn]] void kill_self() {
#if defined(_WIN32)
    TerminateProcess(GetCurrentProcess(), 137);
#else
    std::raise(SIGKILL);
#endif
    std::_Exit(137);
}

void Env::maybe_crash(const std::string& point) {
    if (cfg.crash_after != point) {
        return;
    }
    emit_once(render(!failed(), true, "", "", std::nullopt));
    kill_self();
}

// Joins a thread when the scope ends (also when a failure unwinds it).
struct JoinOnExit {
    std::thread& t;
    ~JoinOnExit() {
        if (t.joinable()) {
            t.join();
        }
    }
};

// A one-shot signal that waiters can block on with a timeout.
class Gate {
public:
    void open() {
        {
            std::lock_guard<std::mutex> lk(mu_);
            open_ = true;
        }
        cv_.notify_all();
    }
    bool wait_for(Nanos d) {
        std::unique_lock<std::mutex> lk(mu_);
        return cv_.wait_for(lk, d, [this] { return open_; });
    }

private:
    std::mutex mu_;
    std::condition_variable cv_;
    bool open_ = false;
};

// A serve loop on a worker thread. The loop ends when its link shuts down, when
// stop() signals the cancel token, or when the link closes.
class Server {
public:
    Server() = default;
    Server(const Server&) = delete;
    Server& operator=(const Server&) = delete;
    ~Server() { stop(); }

    template <class Handler>
    void start(hqq::AdvLink& link, Handler handler) {
        thread_ = std::thread([this, &link, h = std::move(handler)]() mutable {
            status_ = link.serve(h, hqq::Wait::forever(&token_));
        });
    }
    // Waits until the serve loop returns on its own.
    void join() {
        if (thread_.joinable()) {
            thread_.join();
        }
    }
    // Ends the serve loop and joins it.
    void stop() {
        token_.signal();
        join();
    }
    // Status of the serve loop; valid after join() or stop().
    hqq::Status status() const { return status_; }

private:
    hqq::CancelToken token_;
    hqq::Status status_;
    std::thread thread_;
};

// ---- Standard layer ------------------------------------------------------------------

hqq::LinkOptions std_options(const Config& c) {
    hqq::LinkOptions o;
    o.heartbeat_interval_ns = c.heartbeat_ms * 1'000'000;
    if (c.peer_timeout_ms > 0) {
        o.peer_timeout_ns = c.peer_timeout_ms * 1'000'000;
    }
    o.attach_timeout_ns = c.attach_timeout_ms * 1'000'000;
    if (c.wait == "spin") {
        o.wait_policy = HQQ_WAIT_SPIN;
    }
    return o;
}

hqq::Result<hqq::Link> create_link(const Config& c) {
    return hqq::Link::create(c.name, static_cast<std::uint64_t>(c.buffer_count),
                             static_cast<std::uint64_t>(c.buffer_size), 0600, std_options(c));
}

hqq::Result<hqq::Link> open_link(const Config& c) {
    return hqq::Link::open(c.name, static_cast<std::uint64_t>(c.buffer_count),
                           static_cast<std::uint64_t>(c.buffer_size), std_options(c));
}

// The Primary creates the segment and then posts "created", which makes it openable.
hqq::Link std_create_signal(Env& e) {
    hqq::Link link = take(create_link(e.cfg), "create");
    e.barrier("created");
    return link;
}

// The Secondary waits for "created" and opens the segment.
hqq::Link std_open_after(Env& e) {
    e.barrier("created");
    return take(open_link(e.cfg), "open");
}

// A shared-memory name outlives its last handle only on POSIX (Go shm.NamesPersist).
// On Windows the name lives while any handle to it is open.
#if defined(_WIN32)
constexpr bool kNamesPersist = false;
#else
constexpr bool kNamesPersist = true;
#endif

// Probes the name: NotExist once removed, LinkBusy while attached.
hqq::Status probe_name(const Config& c) {
    hqq::Result<hqq::Link> probe = open_link(c);
    return probe.status();
}

// Stale window of a peer (SPEC 6.3): max(peer timeout, 4 x heartbeat interval).
Nanos stale_after(const Config& c) {
    const std::int64_t peer = c.peer_timeout_ms > 0 ? c.peer_timeout_ms * 1'000'000 : HQQ_DEFAULT_PEER_TIMEOUT_NS;
    const std::int64_t interval =
        c.heartbeat_ms > 0 ? c.heartbeat_ms * 1'000'000 : HQQ_DEFAULT_HEARTBEAT_INTERVAL_NS;
    return Nanos(std::max(peer, 4 * interval));
}

Nanos peer_bound(const Config& c) { return stale_after(c) + kStdSlack; }

// Ping-pong over a link: the creator sleeps idle, sends Payload(seed, i), waits for
// the echo; the opener echoes every message. The creator checks that the worst
// round trip is at most max_rtt.
void ping_pong(Env& e, hqq::Link& link, std::int64_t n, Nanos idle, Nanos max_rtt) {
    const std::size_t size = static_cast<std::size_t>(e.cfg.size);
    const std::uint64_t seed = e.cfg.seed;
    std::vector<Byte> buf(size);
    Nanos worst{0};
    bool verify_ok = true;
    std::string verify_err;
    for (std::int64_t i = 0; i < n; ++i) {
        const auto seq = static_cast<std::uint64_t>(i);
        std::size_t len = 0;
        if (e.is_creator()) {
            std::this_thread::sleep_for(idle);
            const auto t0 = Clock::now();
            must("send", link.write(view(payload(seed, seq, size)), e.wait()));
            ++e.sent;
            must("echo", link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait()));
            ++e.received;
            worst = std::max(worst, std::chrono::duration_cast<Nanos>(Clock::now() - t0));
            if (verify_ok && !intact(seed, seq, Bytes(buf.data(), len))) {
                verify_ok = false;
                verify_err = "round " + std::to_string(i) + " is not intact";
            }
            continue;
        }
        must("receive", link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait()));
        ++e.received;
        if (verify_ok && !intact(seed, seq, Bytes(buf.data(), len))) {
            verify_ok = false;
            verify_err = "round " + std::to_string(i) + " is not intact";
        }
        must("echo", link.write(Bytes(buf.data(), len), e.wait()));
        ++e.sent;
    }
    e.check("every message intact", verify_ok, verify_err);
    if (e.is_creator()) {
        e.check("every round trip <= " + std::to_string(max_rtt.count() / 1'000'000) + "ms",
                worst <= max_rtt, "worst " + std::to_string(worst.count() / 1'000'000) + "ms");
        e.add_meta("worst_rtt_ms", std::to_string(worst.count() / 1'000'000));
    }
}

// Ping-pong run of X-12 and X-14: create or open, ping-pong, then the "done" barrier.
void ping_run(Env& e, std::int64_t n, Nanos idle, Nanos max_rtt) {
    hqq::Link link = e.is_creator() ? std_create_signal(e) : std_open_after(e);
    ping_pong(e, link, n, idle, max_rtt);
    e.barrier("done");
}

// Both sides send n messages while they read n messages from the peer. The creator
// streams with seed, the opener with seed + 1.
void bidirectional(Env& e, hqq::Link& link, std::int64_t n) {
    const std::size_t size = static_cast<std::size_t>(e.cfg.size);
    const std::uint64_t seed = e.cfg.seed;
    const std::uint64_t out_seed = e.is_creator() ? seed : seed + 1;
    const std::uint64_t in_seed = e.is_creator() ? seed + 1 : seed;
    hqq::Status wst;
    hqq::Status rst;
    bool verify_failed = false;
    std::string verify_err;
    std::thread writer([&] {
        for (std::int64_t i = 0; i < n; ++i) {
            const std::vector<Byte> p = payload(out_seed, static_cast<std::uint64_t>(i), size);
            wst = link.write(view(p), e.wait());
            if (!wst.ok()) {
                return;
            }
            ++e.sent;
        }
    });
    std::thread reader([&] {
        std::vector<Byte> buf(size);
        for (std::int64_t i = 0; i < n; ++i) {
            std::size_t len = 0;
            rst = link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait());
            if (!rst.ok()) {
                return;
            }
            if (!intact(in_seed, static_cast<std::uint64_t>(i), Bytes(buf.data(), len))) {
                verify_failed = true;
                verify_err = "message " + std::to_string(i) + " is not intact";
                return;
            }
            ++e.received;
        }
    });
    writer.join();
    reader.join();
    if (verify_failed) {
        e.check("both streams intact and in order", false, verify_err);
        return;
    }
    if (!wst.ok()) {
        fail_status("write", wst);
    }
    if (!rst.ok()) {
        fail_status("read", rst);
    }
    e.check("both streams intact and in order", true);
}

// ---- Standard scenarios -----------------------------------------------------------------

// X-01: create, open, exchange, close.
void run_x01(Env& e) {
    const bool creator = e.is_creator();
    const bool creator_first = e.cfg.variant == "creator-closes-first";
    const std::size_t size = static_cast<std::size_t>(e.cfg.size);
    const std::size_t cap = std::max(size, static_cast<std::size_t>(e.cfg.buffer_size));
    const std::uint64_t seed = e.cfg.seed;
    hqq::Link link = creator ? std_create_signal(e) : std_open_after(e);
    std::vector<Byte> buf(cap);
    std::size_t len = 0;
    if (creator) {
        must("write", link.write(view(payload(seed, 0, size)), e.wait()));
        ++e.sent;
        must("read reply", link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait()));
        ++e.received;
        e.check("creator receives the reply intact", intact(seed + 1, 0, Bytes(buf.data(), len)));
    } else {
        must("read", link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait()));
        ++e.received;
        e.check("opener receives the message intact", intact(seed, 0, Bytes(buf.data(), len)));
        must("reply", link.write(view(payload(seed + 1, 0, size)), e.wait()));
        ++e.sent;
    }
    e.barrier("replied");

    if (creator == creator_first) {
        // This side closes first.
        must("close", link.close());
        const hqq::Status probe = probe_name(e.cfg);
        if (creator) {
            if (kNamesPersist) {
                e.check_status("creator close removes the name", probe, HQQ_ERR_NOT_EXIST);
            } else {
                // Windows: the opener still holds its endpoint, so the name is there and busy.
                e.check_status("creator close keeps the name while the opener holds it (Windows)", probe,
                               HQQ_ERR_LINK_BUSY);
            }
        } else {
            e.check_status("opener close leaves the name in place", probe, HQQ_ERR_LINK_BUSY);
        }
        e.barrier("closed");
        return;
    }
    // The other side sees the peer's close after the buffered data.
    e.check_status("peer close is seen as io.EOF after buffered data",
                   link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait()), HQQ_ERR_PEER_CLOSED);
    e.barrier("closed");
    if (creator) {
        must("close", link.close());
        e.check_status("creator close removes the name", probe_name(e.cfg), HQQ_ERR_NOT_EXIST);
    }
}

// X-02: geometry mismatch. The opener's buffer count or size differs from the creator's.
void run_x02(Env& e) {
    if (e.is_creator()) {
        hqq::Link link = std_create_signal(e);
        e.check("creator creates the segment", true);
        e.barrier("checked");
        return;
    }
    e.barrier("created");
    hqq::Result<hqq::Link> r = open_link(e.cfg);
    e.check_status("mismatched geometry fails with ErrInvalidSize", r.status(), HQQ_ERR_INVALID_SIZE);
    e.add_meta("geometry_error", bool_json(r.status() == HQQ_ERR_GEOMETRY_MISMATCH));
    e.barrier("checked");
}

// X-03: third endpoint, re-join after own close, creator vs live segment.
void run_x03(Env& e) {
    if (e.is_creator()) {
        hqq::Link link = std_create_signal(e);
        e.barrier("joined");
        hqq::Result<hqq::Link> again = create_link(e.cfg);
        e.check_status("creator on a live name fails with ErrLinkBusy", again.status(), HQQ_ERR_LINK_BUSY);
        e.barrier("creator-checked");
        e.barrier("reopen-done");
        return;
    }
    hqq::Link first = std_open_after(e);
    e.barrier("joined");
    hqq::Result<hqq::Link> third = open_link(e.cfg);
    e.check_status("third endpoint fails with ErrLinkBusy", third.status(), HQQ_ERR_LINK_BUSY);
    e.barrier("creator-checked");
    must("close", first.close());
    hqq::Result<hqq::Link> again = open_link(e.cfg);
    e.check_status("re-join after own close fails with ErrLinkBusy", again.status(), HQQ_ERR_LINK_BUSY);
    e.barrier("reopen-done");
}

// X-04 (before-creator): the opener's first attempt fails transiently, the creator
// comes up, the opener joins as Secondary.
void run_x04_before(Env& e) {
    if (!e.is_creator()) {
        const auto start = Clock::now();
        {
            hqq::Result<hqq::Link> first = open_link(e.cfg);
            e.check("first attempt before the creator fails transiently",
                    first.status().matches(HQQ_ERR_NOT_EXIST) || first.status().matches(HQQ_ERR_FAILED_INIT),
                    std::string(first.status().message()));
        }
        e.add_meta("first_attempt_ms",
                   std::to_string(std::chrono::duration_cast<std::chrono::milliseconds>(Clock::now() - start).count()));
        e.barrier("attempt");
        e.barrier("created");
        hqq::Link link = take(open_link(e.cfg), "open");
        e.check("opener joins as Secondary", link.mode() == HQQ_MODE_SECONDARY);
        e.barrier("joined");
        return;
    }
    e.barrier("attempt");
    hqq::Link link = take(create_link(e.cfg), "create");
    e.barrier("created");
    e.barrier("joined");
}

// X-04 (dead): step 1 joins and the creator dies at exit; step 2 is an opener on the
// dead segment, which must fail with ErrFailedInit quickly.
void run_x04_dead(Env& e) {
    if (e.cfg.step == 2) {
        const auto start = Clock::now();
        hqq::Result<hqq::Link> r = open_link(e.cfg);
        const Nanos elapsed = std::chrono::duration_cast<Nanos>(Clock::now() - start);
        // Windows: the dead segment's name is gone, which is ErrNotExist (as in the Go helper).
        const bool vanished = !kNamesPersist && r.status().matches(HQQ_ERR_NOT_EXIST);
        e.check("opener on a dead segment fails with ErrFailedInit (ErrNotExist on Windows)",
                r.status().matches(HQQ_ERR_FAILED_INIT) || vanished, std::string(r.status().message()));
        const Nanos bound = stale_after(e.cfg) + 3s;
        e.check("failure is quick (errNoLiveEndpoint)", elapsed <= bound,
                std::to_string(elapsed.count() / 1'000'000) + "ms, bound " +
                    std::to_string(bound.count() / 1'000'000) + "ms");
        return;
    }
    if (e.is_creator()) {
        hqq::Link link = std_create_signal(e);
        e.barrier("joined");
        e.maybe_crash("exit");
        return;
    }
    hqq::Link link = std_open_after(e);
    e.barrier("joined");
}

void run_x04(Env& e) {
    if (e.cfg.variant == "dead") {
        run_x04_dead(e);
    } else {
        run_x04_before(e);
    }
}

// X-05: ring full and backpressure. The creator writes two messages into a 2-slot
// ring; a third write with a 200 ms deadline must time out without losing data.
void run_x05(Env& e) {
    const std::size_t size = static_cast<std::size_t>(e.cfg.size);
    const std::uint64_t seed = e.cfg.seed;
    if (e.is_creator()) {
        hqq::Link link = std_create_signal(e);
        for (std::uint64_t i = 0; i < 2; ++i) {
            must("write", link.write(view(payload(seed, i, size)), e.wait()));
            ++e.sent;
        }
        e.check_status("third write on a full ring times out",
                       link.write(view(payload(seed, 2, size)), hqq::Wait::for_duration(200ms)), HQQ_ERR_TIMEOUT);
        e.barrier("filled");
        e.barrier("drained");
        must("write after drain", link.write(view(payload(seed, 2, size)), e.wait()));
        ++e.sent;
        e.barrier("received");
        return;
    }
    hqq::Link link = std_open_after(e);
    e.barrier("filled");
    std::vector<Byte> buf(size);
    bool in_order = true;
    for (std::uint64_t i = 0; i < 2; ++i) {
        std::size_t len = 0;
        must("read", link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait()));
        ++e.received;
        if (!intact(seed, i, Bytes(buf.data(), len))) {
            in_order = false;
        }
    }
    e.check("the two buffered messages arrive in order and intact", in_order);
    e.barrier("drained");
    std::size_t len = 0;
    must("read third", link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait()));
    ++e.received;
    e.check("the third message arrives intact after the drain", intact(seed, 2, Bytes(buf.data(), len)));
    e.barrier("received");
}

// X-06: bidirectional stream, sequence and CRC.
void run_x06(Env& e) {
    const std::int64_t n = e.cfg.long_soak ? 100000 : e.cfg.messages;
    hqq::Link link = e.is_creator() ? std_create_signal(e) : std_open_after(e);
    bidirectional(e, link, n);
    e.barrier("done");
}

// X-07: kill -9 of the peer, running (creator) and parked (opener).
void run_x07(Env& e) {
    const std::int64_t n = e.cfg.messages;
    const std::size_t size = static_cast<std::size_t>(e.cfg.size);
    const std::uint64_t seed = e.cfg.seed;
    hqq::Link link = e.is_creator() ? std_create_signal(e) : std_open_after(e);
    e.barrier("joined");
    const Nanos bound = peer_bound(e.cfg);
    std::vector<Byte> buf(size);
    std::size_t len = 0;

    if (e.is_creator()) {
        for (std::int64_t i = 0; i < n; ++i) {
            must("write", link.write(view(payload(seed, static_cast<std::uint64_t>(i), size)), e.wait()));
            ++e.sent;
        }
        e.maybe_crash("msgs:" + std::to_string(n));
        // The opener was killed: its blocked read must end with ErrPeerGone.
        const auto start = Clock::now();
        const hqq::Status st = link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait());
        const Nanos elapsed = std::chrono::duration_cast<Nanos>(Clock::now() - start);
        e.check_status("blocked read after the opener died gets ErrPeerGone", st, HQQ_ERR_PEER_GONE);
        e.check("peer loss detected within the bound", elapsed <= bound,
                std::to_string(elapsed.count() / 1'000'000) + "ms, bound " +
                    std::to_string(bound.count() / 1'000'000) + "ms");
        return;
    }

    bool intact_all = true;
    for (std::int64_t i = 0; i < n; ++i) {
        must("read", link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait()));
        ++e.received;
        if (!intact(seed, static_cast<std::uint64_t>(i), Bytes(buf.data(), len))) {
            intact_all = false;
        }
    }
    e.check("all buffered messages delivered intact before the peer loss",
            intact_all && e.received.load() == n, std::to_string(e.received.load()) + " of " + std::to_string(n));
    e.maybe_crash("blocked");
    // The creator was killed right after its last send: ErrPeerGone follows the buffered data.
    const auto start = Clock::now();
    const hqq::Status st = link.read(MutableBytes(buf.data(), buf.size()), &len, e.wait());
    const Nanos elapsed = std::chrono::duration_cast<Nanos>(Clock::now() - start);
    e.check_status("read after the buffered messages gets ErrPeerGone", st, HQQ_ERR_PEER_GONE);
    e.check("peer loss detected within the bound", elapsed <= bound,
            std::to_string(elapsed.count() / 1'000'000) + "ms, bound " +
                std::to_string(bound.count() / 1'000'000) + "ms");
}

// X-12: parked consumer woken by a producer (ping-pong, ring of 2, 50 ms idle).
void run_x12(Env& e) {
    ping_run(e, e.cfg.messages, std::chrono::milliseconds(50), std::chrono::milliseconds(250));
}

// X-14: simple-mode peer, round trips with 300 ms idle (the opener may run --wait spin).
void run_x14(Env& e) {
    ping_run(e, e.cfg.messages, std::chrono::milliseconds(300), std::chrono::milliseconds(150));
}

// ---- Advanced layer -----------------------------------------------------------------

hqq::AdvOptions adv_options(const Config& c) {
    hqq::AdvOptions o;
    o.max_message_size = c.max_message;
    o.max_reassembly_bytes = c.max_reassembly;
    o.queue_size = static_cast<std::uint64_t>(c.queue_size);
    o.link.heartbeat_interval_ns = c.heartbeat_ms * 1'000'000;
    o.link.peer_timeout_ns = c.peer_timeout_ms * 1'000'000;
    o.link.attach_timeout_ns = c.attach_timeout_ms * 1'000'000;
    if (c.wait == "spin") {
        o.link.wait_policy = HQQ_WAIT_SPIN;
    }
    return o;
}

std::uint64_t adv_max_message(const Config& c) {
    return c.max_message != 0 ? c.max_message : HQQ_ADV_DEFAULT_MAX_MESSAGE_SIZE;
}

// Makes the named Advanced link available to both roles (the creator is the Primary).
hqq::AdvLink adv_establish(Env& e) {
    if (e.is_creator()) {
        hqq::AdvLink link = take(hqq::AdvLink::create(e.cfg.name, static_cast<std::uint64_t>(e.cfg.buffer_count),
                                                      static_cast<std::uint64_t>(e.cfg.buffer_size), 0600,
                                                      adv_options(e.cfg)),
                                 "create advanced link");
        e.barrier("linked");
        return link;
    }
    e.barrier("linked");
    return take(hqq::AdvLink::open(e.cfg.name, static_cast<std::uint64_t>(e.cfg.buffer_count),
                                   static_cast<std::uint64_t>(e.cfg.buffer_size), adv_options(e.cfg)),
                "open advanced link");
}

// Receives the next message of st and checks it is Payload(seed, seq, size).
void adv_recv_check(Env& e, const hqq::Stream& st, std::uint64_t seed, std::uint64_t seq, std::size_t size) {
    hqq::Message m = take(st.recv(e.wait()), "receive");
    const Bytes got = m.data();
    e.check("message " + std::to_string(seq) + " has " + std::to_string(size) + " bytes and is intact",
            got.size() == size && intact(seed, seq, got),
            "len=" + std::to_string(got.size()));
}

// Echo method of the default mux.
void echo_serve(hqq::RequestContext&, const hqq::Request& req, hqq::Response& w) {
    (void)w.write(req.data());
}

// X-A01: negotiation. Each variant states how both roles negotiate and which
// feature set must result.
void negotiate_variant(Env& e, hqq::AdvLink& link) {
    const std::string& v = e.cfg.variant;
    const bool creator = e.is_creator();
    constexpr std::uint16_t version = HQQ_PROTOCOL_1_1;
    hqq::Status st;
    if (v == "all-features") {
        st = link.negotiate(HQQ_FEATURES_ALL, e.wait());
    } else if (v == "opener-initiates") {
        st = creator ? link.negotiate(HQQ_FEATURES_ALL, e.wait())
                     : link.negotiate_protocol(version, HQQ_FEATURES_ALL, e.wait());
    } else if (v == "simultaneous") {
        st = link.negotiate_protocol(version, HQQ_FEATURES_ALL, e.wait());
    } else if (v == "messages-only") {
        st = link.negotiate(HQQ_FEATURE_MESSAGES, e.wait());
    } else if (v == "request-response-only") {
        st = link.negotiate(HQQ_FEATURE_REQUEST_RESPONSE, e.wait());
    } else if (v == "none") {
        // features 0 means "every feature" in negotiate(); negotiate_protocol() sends
        // the empty set as is.
        st = creator ? link.negotiate_protocol(version, 0, e.wait()) : link.negotiate(HQQ_FEATURES_ALL, e.wait());
    } else {
        throw Failure{"Unknown", "unknown variant " + v, std::nullopt};
    }
    must("negotiation", st);
}

std::uint64_t negotiated_want(const std::string& v) {
    if (v == "messages-only") {
        return HQQ_FEATURE_MESSAGES;
    }
    if (v == "request-response-only") {
        return HQQ_FEATURE_REQUEST_RESPONSE;
    }
    if (v == "none") {
        return 0;
    }
    return HQQ_FEATURES_ALL;
}

// Send needs FeatureMessages; Call and Serve need FeatureRequestResponse.
void adv_nego_exchange(Env& e, hqq::AdvLink& link, std::uint64_t want, Server& srv) {
    const hqq::Stream s0 = link.stream0();
    const std::size_t size = static_cast<std::size_t>(e.cfg.size);
    if (e.is_creator()) {
        if ((want & HQQ_FEATURE_MESSAGES) != 0) {
            e.check_status("send with FeatureMessages", s0.send(view(payload(e.cfg.seed, 0, size)), e.wait()), HQQ_OK);
        } else {
            e.check_status("send without FeatureMessages is ErrNotNegotiated",
                           s0.send(sv_view("x"), e.wait()), HQQ_ERR_NOT_NEGOTIATED);
        }
        hqq::Message m;
        if ((want & HQQ_FEATURE_REQUEST_RESPONSE) != 0) {
            const hqq::Status st = s0.call(kMethodEcho, sv_view("ping"), m, e.wait());
            if (st.ok()) {
                e.check("call with FeatureRequestResponse is echoed", m.data().as_string_view() == "ping",
                        std::string(m.data().as_string_view()));
            } else {
                e.check("call with FeatureRequestResponse is echoed", false, std::string(st.message()));
            }
        } else {
            e.check_status("call without FeatureRequestResponse is ErrNotNegotiated",
                           s0.call(kMethodEcho, sv_view("ping"), m, e.wait()), HQQ_ERR_NOT_NEGOTIATED);
        }
        return;
    }
    if ((want & HQQ_FEATURE_MESSAGES) != 0) {
        adv_recv_check(e, s0, e.cfg.seed, 0, size);
    }
    if ((want & HQQ_FEATURE_REQUEST_RESPONSE) != 0) {
        srv.start(link, echo_serve);
    }
}

void run_a01(Env& e) {
    hqq::AdvLink link = adv_establish(e);
    negotiate_variant(e, link);
    const std::uint64_t want = negotiated_want(e.cfg.variant);
    e.check("negotiated version is 1.1", link.negotiated_version() == HQQ_PROTOCOL_1_1,
            "version " + std::to_string(link.negotiated_version()));
    e.check("negotiated features match the request", link.negotiated_features() == want,
            "got " + std::to_string(link.negotiated_features()) + " want " + std::to_string(want));
    e.check("protocol state is Negotiated", link.protocol_state() == HQQ_PROTO_NEGOTIATED,
            "state " + std::to_string(static_cast<int>(link.protocol_state())));
    Server srv;  // declared after link: stopped before the link closes
    adv_nego_exchange(e, link, want, srv);
    e.barrier("end");
}

// X-A02: message sizes 0 .. MaxMessageSize, and the local refusal of MaxMessageSize+1.
void run_a02(Env& e) {
    hqq::AdvLink link = adv_establish(e);
    must("negotiate", link.negotiate(HQQ_FEATURES_ALL, e.wait()));
    const std::uint64_t bs = static_cast<std::uint64_t>(e.cfg.buffer_size);
    const std::uint64_t max_size = adv_max_message(e.cfg);
    const std::vector<std::uint64_t> sizes = {0, 1, bs, bs + 1, 3 * bs + 7, max_size};
    const int rounds = e.cfg.long_soak ? 20 : 1;
    const std::uint64_t seed = e.cfg.seed;
    const hqq::Stream s0 = link.stream0();
    if (e.is_creator()) {
        std::uint64_t seq = 0;
        for (int r = 0; r < rounds; ++r) {
            for (std::uint64_t n : sizes) {
                must("send", s0.send(view(payload(seed, seq, static_cast<std::size_t>(n))), e.wait()));
                ++seq;
            }
        }
        e.sent = static_cast<long long>(seq);
        const std::vector<Byte> too_big(static_cast<std::size_t>(max_size + 1));
        e.check_status("MaxMessageSize+1 is refused locally", s0.send(view(too_big), e.wait()),
                       HQQ_ERR_BUFFER_OVERFLOW);
        // Nothing of the refused message was sent: the sentinel takes its sequence number.
        must("sentinel", s0.send(view(payload(seed, seq, 1)), e.wait()));
        e.sent = static_cast<long long>(seq) + 1;
    } else {
        std::uint64_t seq = 0;
        for (int r = 0; r < rounds; ++r) {
            for (std::uint64_t n : sizes) {
                adv_recv_check(e, s0, seed, seq, static_cast<std::size_t>(n));
                ++seq;
            }
        }
        adv_recv_check(e, s0, seed, seq, 1);
        e.received = static_cast<long long>(seq) + 1;
    }
    e.barrier("end");
}

// Request/response handler of X-A04.
void rpc_serve(hqq::RequestContext& ctx, const hqq::Request& req, hqq::Response& w) {
    switch (req.method()) {
    case kMethodSleepEcho: {
        // Inverse sleep: later ids finish first.
        const Bytes d = req.data();
        std::uint32_t id = 0;
        if (d.size() >= 4) {
            const auto* p = static_cast<const unsigned char*>(d.data());
            id = (std::uint32_t{p[0]} << 24) | (std::uint32_t{p[1]} << 16) | (std::uint32_t{p[2]} << 8) | p[3];
        }
        const long long ms = std::max<long long>(0, kRPCConcurrency - static_cast<long long>(id)) * 5;
        (void)ctx.wait_done(ms * 1'000'000);
        (void)w.write(d);
        return;
    }
    case kMethodFail:
        (void)w.error(42, sv_view("boom payload"));
        return;
    case kMethodSlow:
        (void)ctx.wait_done(300'000'000);
        (void)w.write(sv_view("late"));
        return;
    case kMethodEcho:
        (void)w.write(req.data());
        return;
    default:
        (void)w.error(HQQ_ERRCODE_INVALID_OP, Bytes());
        return;
    }
}

std::vector<Byte> be32_bytes(std::uint32_t v) {
    return {Byte(static_cast<unsigned char>(v >> 24)), Byte(static_cast<unsigned char>(v >> 16)),
            Byte(static_cast<unsigned char>(v >> 8)), Byte(static_cast<unsigned char>(v))};
}

// X-A04: request/response: concurrent calls, error payload, abandoned call, unknown method.
void run_a04(Env& e) {
    hqq::AdvLink link = adv_establish(e);
    must("negotiate", link.negotiate(HQQ_FEATURE_MESSAGES | HQQ_FEATURE_REQUEST_RESPONSE, e.wait()));
    Server srv;  // declared after link
    if (!e.is_creator()) {
        srv.start(link, rpc_serve);
        e.barrier("end");
        return;
    }
    const hqq::Stream s0 = link.stream0();

    // Concurrent calls, answered out of order.
    std::mutex mu;
    std::vector<int> order;
    bool all_good = true;
    std::string first_err;
    std::vector<std::thread> callers;
    for (int i = 0; i < kRPCConcurrency; ++i) {
        callers.emplace_back([&, i] {
            const std::vector<Byte> p = be32_bytes(static_cast<std::uint32_t>(i));
            hqq::Message m;
            const hqq::Status st = s0.call(kMethodSleepEcho, view(p), m, e.wait());
            const bool good = st.ok() && m.data().as_string_view() ==
                                             std::string_view(reinterpret_cast<const char*>(p.data()), p.size());
            std::lock_guard<std::mutex> lk(mu);
            order.push_back(i);
            if (!good) {
                all_good = false;
                if (first_err.empty()) {
                    first_err = st.ok() ? std::string("echo mismatch") : std::string(st.message());
                }
            }
        });
    }
    for (auto& t : callers) {
        t.join();
    }
    e.check("all concurrent calls are answered with their own payload", all_good, first_err);
    e.check("responses complete out of order", !std::is_sorted(order.begin(), order.end()),
            std::to_string(order.size()) + " responses");
    e.sent = kRPCConcurrency;

    // Error response with a payload.
    hqq::Message em;
    const hqq::Status est = s0.call(kMethodFail, Bytes(), em, e.wait());
    e.check("error response carries status 42 and its payload",
            est == HQQ_ERR_RESPONSE && em.status() == 42 && em.data().as_string_view() == "boom payload",
            std::string(est.message()));

    // Cancelled call: its late response is discarded (no FeatureCancel, so no OpCancel).
    hqq::Message cm;
    const hqq::Status cst = s0.call(kMethodSlow, sv_view("slow"), cm, hqq::Wait::for_duration(50ms));
    e.check_status("cancelled call returns its context error", cst, HQQ_ERR_TIMEOUT);
    // Receiving routes the late response: pump calls until it is discarded.
    const auto until = Clock::now() + 3s;
    while (link.stats().abandoned_responses == 0 && Clock::now() < until) {
        hqq::Message pm;
        must("pump", s0.call(kMethodEcho, sv_view("pump"), pm, e.wait()), &pm);
    }
    const auto abandoned = link.stats().abandoned_responses;
    e.check("late response of the cancelled call is discarded (AbandonedResponses)", abandoned >= 1,
            "abandoned " + std::to_string(abandoned));

    // Unknown method: the mux answers with status 6.
    hqq::Message um;
    const hqq::Status ust = s0.call(99, Bytes(), um, e.wait());
    e.check("unknown method is answered with status 6 (ErrCodeInvalidOp)",
            ust == HQQ_ERR_RESPONSE && um.status() == HQQ_ERRCODE_INVALID_OP, std::string(ust.message()));
    e.barrier("end");
}

// X-A05 (basic): streams: refusal while not listening, ids 2/4/6, close semantics.
void run_a05(Env& e) {
    hqq::AdvLink link = adv_establish(e);
    must("negotiate", link.negotiate(HQQ_FEATURES_ALL, e.wait()));
    const std::uint64_t seed = e.cfg.seed;
    const std::size_t size = static_cast<std::size_t>(e.cfg.size);
    if (!e.is_creator()) {
        e.check_status("AcceptStream without ListenStreams is ErrNotListening",
                       link.accept_stream(hqq::Wait::for_duration(1s)).status(), HQQ_ERR_NOT_LISTENING);
        e.barrier("p1");
        e.barrier("p2");
        must("listen streams", link.listen_streams());
        e.barrier("p3");

        hqq::Stream st = take(link.accept_stream(e.wait()), "accept stream");
        e.check("first accepted stream has id 4 (the refused open consumed 2)", st.id() == 4,
                std::to_string(st.id()));
        adv_recv_check(e, st, seed, 0, size);
        adv_recv_check(e, st, seed, 1, size);
        must("reply", st.send(view(payload(seed + 1, 0, 10)), e.wait()));
        e.check_status("peer close is seen after the queued data as ErrPeerClosed",
                       st.recv(e.wait()).status(), HQQ_ERR_PEER_CLOSED);

        hqq::Stream st2 = take(link.accept_stream(e.wait()), "accept second stream");
        e.check("second accepted stream has id 6 (ids are not reused)", st2.id() == 6, std::to_string(st2.id()));
        adv_recv_check(e, st2, seed, 2, size);
        e.received = 3;
        e.barrier("end");
        return;
    }

    e.barrier("p1");
    e.check_status("OpenStream refused while the peer does not listen is ErrConnNotFound",
                   link.open_stream(e.wait()).status(), HQQ_ERR_CONN_NOT_FOUND);
    e.barrier("p2");
    e.barrier("p3");

    hqq::Stream st = take(link.open_stream(e.wait()), "open stream");
    e.check("first stream has id 4 (the refused open consumed 2)", st.id() == 4, std::to_string(st.id()));
    for (std::uint64_t i = 0; i < 2; ++i) {
        must("send", st.send(view(payload(seed, i, size)), e.wait()));
        ++e.sent;
    }
    hqq::Message reply = take(st.recv(e.wait()), "receive reply");
    e.check("reply from the peer's stream is intact", intact(seed + 1, 0, reply.data()));
    e.check_status("stream Close", st.close(), HQQ_OK);
    e.check_status("second Close is ErrClosed", st.close(), HQQ_ERR_CLOSED);
    e.check_status("Recv on a closed stream is ErrClosed", st.recv(e.wait()).status(), HQQ_ERR_CLOSED);
    e.check_status("stream 0 cannot be closed", link.stream0().close(), HQQ_ERR_INVALID_OP);

    hqq::Stream st2 = take(link.open_stream(e.wait()), "open second stream");
    e.check("second stream has id 6 (ids are not reused)", st2.id() == 6, std::to_string(st2.id()));
    must("send", st2.send(view(payload(seed, 2, size)), e.wait()));
    ++e.sent;
    e.check_status("stream 4 Close", st2.close(), HQQ_OK);
    e.barrier("end");
}

// X-A08: request metadata (caller, trace, time budget) with and without FeatureMetadata.
struct MetaSeen {
    std::uint64_t caller = 0;
    std::array<std::uint8_t, 16> trace{};
    bool has_deadline = false;
    std::int64_t remaining_ns = 0;
};

std::array<std::uint8_t, 16> advance_trace() {
    std::array<std::uint8_t, 16> t{};
    for (std::size_t i = 0; i < t.size(); ++i) {
        t[i] = static_cast<std::uint8_t>(i + 1);
    }
    return t;
}

void run_a08(Env& e) {
    const bool with_meta = e.cfg.variant == "metadata";
    const std::uint64_t features =
        with_meta ? HQQ_FEATURES_ALL : (HQQ_FEATURE_MESSAGES | HQQ_FEATURE_REQUEST_RESPONSE | HQQ_FEATURE_CANCEL);
    hqq::AdvLink link = adv_establish(e);
    must("negotiate", link.negotiate(features, e.wait()));

    if (!e.is_creator()) {
        std::mutex mu;
        std::vector<MetaSeen> seen;
        Server srv;  // declared after the state its handler uses
        srv.start(link, [&](hqq::RequestContext& ctx, const hqq::Request& req, hqq::Response& w) {
            MetaSeen rec;
            rec.caller = req.caller();
            if (auto t = req.trace()) {
                rec.trace = *t;
            }
            if (auto dl = ctx.deadline()) {
                rec.has_deadline = true;
                rec.remaining_ns = static_cast<std::int64_t>(*dl) - static_cast<std::int64_t>(mono_ns());
            }
            {
                std::lock_guard<std::mutex> lk(mu);
                seen.push_back(rec);
            }
            (void)w.write(req.data());
        });
        e.barrier("end");
        srv.stop();
        std::vector<MetaSeen> got;
        {
            std::lock_guard<std::mutex> lk(mu);
            got = seen;
        }
        const std::array<std::uint8_t, 16> zero{};
        if (with_meta) {
            e.check("responder saw 2 requests (the expired call was not sent)", got.size() == 2,
                    std::to_string(got.size()));
            if (got.size() == 2) {
                e.check("caller ID and trace ID arrive", got[0].caller == 42 && got[0].trace == advance_trace(),
                        std::to_string(got[0].caller));
                e.check("request without options has caller 0", got[1].caller == 0, std::to_string(got[1].caller));
                e.check("deadline reaches the handler within the budget",
                        got[1].has_deadline && got[1].remaining_ns > 1'000'000'000 &&
                            got[1].remaining_ns <= 2'000'000'000,
                        "has=" + std::to_string(got[1].has_deadline) + " remaining_ns=" +
                            std::to_string(got[1].remaining_ns));
            }
        } else {
            e.check("responder saw 2 requests", got.size() == 2, std::to_string(got.size()));
            if (got.size() == 2) {
                e.check("options are ignored without FeatureMetadata: caller 0, trace zero",
                        got[0].caller == 0 && got[0].trace == zero, std::to_string(got[0].caller));
                e.check("no deadline reaches the handler without FeatureMetadata", !got[1].has_deadline,
                        std::to_string(got[1].has_deadline));
            }
        }
        return;
    }

    const hqq::Stream s0 = link.stream0();
    hqq::CallOptions opts;
    opts.caller = 42;
    const auto trace = advance_trace();
    std::copy(trace.begin(), trace.end(), opts.trace);
    hqq::Message r1;
    must("call with metadata", s0.call_with(kMethodMeta, sv_view("a"), opts, r1, e.wait()), &r1);
    hqq::Message r2;
    must("call with a 2 s deadline", s0.call_with(kMethodMeta, sv_view("b"), hqq::CallOptions{}, r2,
                                                   hqq::Wait::for_duration(2s)),
         &r2);
    if (with_meta) {
        const auto before = link.stats().requests;
        hqq::Message r3;
        e.check_status("call with an expired deadline fails locally with DeadlineExceeded",
                       s0.call_with(kMethodMeta, sv_view("c"), hqq::CallOptions{}, r3, hqq::Wait::poll()),
                       HQQ_ERR_TIMEOUT);
        const auto after = link.stats().requests;
        e.check("the expired call sent no request", after == before,
                "requests " + std::to_string(before) + " -> " + std::to_string(after));
    }
    e.barrier("end");
}

// Gate-driven handler state of X-A09 (declared before the Server that uses it).
void run_a09_responder(Env& e, hqq::AdvLink& link) {
    Gate started;
    Gate release;
    Server srv;  // declared after the gates its handler uses
    srv.start(link, [&started, &release](hqq::RequestContext&, const hqq::Request& req, hqq::Response& w) {
        if (req.method() == kMethodBlock) {
            started.open();
            release.wait_for(10s);
            (void)w.write(sv_view("A-done"));
        } else if (req.method() == kMethodEcho) {
            (void)w.write(req.data());
        } else {
            (void)w.error(HQQ_ERRCODE_INVALID_OP, Bytes());
        }
    });
    if (!started.wait_for(e.remaining())) {
        throw Failure{"ErrTimeout", "request A never started", std::nullopt};
    }
    std::atomic<bool> sd_done{false};
    hqq::Status sd_status;
    std::thread sd([&] {
        sd_status = link.shutdown(e.wait());
        sd_done = true;
    });
    JoinOnExit join_sd{sd};
    e.barrier("refused");
    e.check("Shutdown waits for the in-flight request", !sd_done.load(),
            sd_done.load() ? "returned while A was in flight" : "");
    release.open();
    sd.join();
    e.check_status("Shutdown returns nil after the in-flight request ended", sd_status, HQQ_OK);
    srv.join();
    // A requester that closed right after the exchange may end Serve with PEER_CLOSED.
    const hqq::Status serve_st = srv.status();
    e.check("Serve returns nil after shutdown", serve_st.ok() || serve_st == HQQ_ERR_PEER_CLOSED,
            std::string(serve_st.message()));
    const auto refused = link.stats().refused_requests;
    e.check("refused requests are counted", refused >= 1, std::to_string(refused));

    // The link stays open: data flows both ways.
    hqq::Message m = take(link.stream0().recv(e.wait()), "receive after shutdown");
    e.check("data from the peer after shutdown", m.data().as_string_view() == "after-shutdown",
            std::string(m.data().as_string_view()));
    must("reply", link.stream0().send(view(to_bytes("reply")), e.wait()));
    e.barrier("end");
}

// X-A09: graceful shutdown: refusal with status 8, in-flight request completes.
void run_a09_requester(Env& e, hqq::AdvLink& link) {
    const hqq::Stream s0 = link.stream0();
    hqq::Status a_status;
    std::string a_body;
    std::thread a_call([&] {
        hqq::Message m;
        a_status = s0.call(kMethodBlock, sv_view("A"), m, e.wait());
        if (a_status.ok()) {
            a_body = std::string(m.data().as_string_view());
        }
    });
    JoinOnExit join_a{a_call};
    bool refused = false;
    hqq::Status last;
    const auto until = Clock::now() + 10s;
    while (!refused && Clock::now() < until) {
        hqq::Message m;
        const hqq::Status st = s0.call(kMethodEcho, sv_view("B"), m, e.wait());
        if (st.ok()) {
            continue;  // answered before the shutdown started
        }
        last = st;
        refused = st == HQQ_ERR_SHUTTING_DOWN || (st == HQQ_ERR_RESPONSE && m.status() == HQQ_ERRCODE_SHUTTING_DOWN);
        if (!refused) {
            break;
        }
    }
    e.check("request after shutdown started is refused with status 8 (ErrShuttingDown)", refused,
            std::string(last.message()));
    e.barrier("refused");
    a_call.join();
    e.check("in-flight request completes normally", a_status.ok() && a_body == "A-done",
            std::string(a_status.message()) + " " + a_body);
    must("send", s0.send(view(to_bytes("after-shutdown")), e.wait()));
    hqq::Message m = take(s0.recv(e.wait()), "receive reply");
    e.check("data from the responder after shutdown", m.data().as_string_view() == "reply",
            std::string(m.data().as_string_view()));
    e.barrier("end");
}

void run_a09(Env& e) {
    hqq::AdvLink link = adv_establish(e);
    must("negotiate", link.negotiate(HQQ_FEATURES_ALL, e.wait()));
    if (e.is_creator()) {
        run_a09_responder(e, link);
    } else {
        run_a09_requester(e, link);
    }
}

// X-A10: acceptor: Listen/Dial/Connect, busy listener, acceptor close.
hqq::AcceptorOptions acceptor_options(const Config& c) {
    hqq::AcceptorOptions o;
    o.link = adv_options(c);
    return o;
}

struct ServedLink {
    std::unique_ptr<hqq::AdvLink> link;
    Server server;  // declared after link: stopped before the link closes
};

void run_a10_acceptor(Env& e) {
    hqq::Acceptor acc = take(hqq::Acceptor::create(e.cfg.name, acceptor_options(e.cfg)), "create acceptor");
    e.barrier("listening");
    {
        hqq::Result<hqq::Acceptor> second = hqq::Acceptor::create(e.cfg.name, acceptor_options(e.cfg));
        e.check_status("second acceptor on the same name is ErrListenerBusy", second.status(), HQQ_ERR_LISTENER_BUSY);
    }

    std::vector<std::unique_ptr<ServedLink>> served;
    for (int i = 0; i < kAdvClients; ++i) {
        auto s = std::make_unique<ServedLink>();
        s->link = std::make_unique<hqq::AdvLink>(take(acc.accept(e.wait()), "accept"));
        s->server.start(*s->link, echo_serve);
        served.push_back(std::move(s));
    }
    e.received = kAdvClients;
    e.barrier("clients-done");

    // A pending Accept fails with ErrAcceptorClosed once the acceptor closes.
    std::atomic<bool> entering{false};
    hqq::Status pending;
    std::thread pend([&] {
        entering = true;
        pending = acc.accept(e.wait()).status();
    });
    JoinOnExit join_pend{pend};
    while (!entering.load()) {
        std::this_thread::yield();
    }
    must("close acceptor", acc.close());
    pend.join();
    e.check_status("pending Accept fails with ErrAcceptorClosed after Close", pending, HQQ_ERR_ACCEPTOR_CLOSED);
    e.barrier("end");
}

void run_a10_client(Env& e) {
    e.barrier("listening");
    hqq::ConnectOptions copts;
    copts.buffer_count = static_cast<std::uint64_t>(e.cfg.buffer_count);
    copts.buffer_size = static_cast<std::uint64_t>(e.cfg.buffer_size);
    copts.link = adv_options(e.cfg);
    for (int i = 0; i < kAdvClients; ++i) {
        hqq::AdvLink link = take(hqq::connect(e.cfg.name, copts, e.wait()), "connect");
        const std::string text = "client " + std::to_string(i);
        hqq::Message m;
        must("call", link.stream0().call(kMethodEcho, sv_view(text), m, e.wait()), &m);
        e.check("client " + std::to_string(i) + ": the accepted link answers its request",
                m.data().as_string_view() == text, std::string(m.data().as_string_view()));
    }
    e.sent = kAdvClients;
    e.barrier("clients-done");
    e.barrier("end");
}

void run_a10(Env& e) {
    if (e.is_creator()) {
        run_a10_acceptor(e);
    } else {
        run_a10_client(e);
    }
}

// ---- Catalogue ------------------------------------------------------------------------

Params params(long long buffer_count, long long buffer_size) {
    Params p;
    p.buffer_count = buffer_count;
    p.buffer_size = buffer_size;
    return p;
}

StepDef pair_step(std::string creator, std::string opener, ArgsMap args = {}) {
    return StepDef{{"creator", "opener"}, {{"creator", std::move(creator)}, {"opener", std::move(opener)}},
                   std::move(args)};
}

StepDef ok_pair() { return pair_step("ok", "ok"); }

VariantDef variant(std::string name, Params p, std::vector<StepDef> steps) {
    return VariantDef{std::move(name), p, std::move(steps)};
}

const std::vector<ScenarioDef>& catalogue() {
    static const std::vector<ScenarioDef> cat = [] {
        std::vector<ScenarioDef> c;

        Params x01 = params(0, 256);
        x01.messages = 1;
        c.push_back({"X-01", "create, open, exchange, close", "standard", 30,
                     {variant("creator-closes-first", x01, {ok_pair()}),
                      variant("opener-closes-first", x01, {ok_pair()})},
                     run_x01});

        c.push_back({"X-02", "geometry mismatch (count, size)", "standard", 30,
                     {variant("count-double", params(4, 256), {pair_step("ok", "ok", {{"opener", {"--buffer-count", "8"}}})}),
                      variant("size-double", params(4, 256), {pair_step("ok", "ok", {{"opener", {"--buffer-size", "512"}}})}),
                      variant("count-half", params(4, 256), {pair_step("ok", "ok", {{"opener", {"--buffer-count", "2"}}})}),
                      variant("size-half", params(4, 256), {pair_step("ok", "ok", {{"opener", {"--buffer-size", "128"}}})})},
                     run_x02});

        c.push_back({"X-03", "third endpoint, re-join after own close, creator vs live segment", "standard", 30,
                     {variant("default", params(4, 256), {ok_pair()})}, run_x03});

        Params before = params(4, 256);
        before.attach_timeout_ms = 300;
        Params dead = params(4, 256);
        dead.heartbeat_ms = 50;
        dead.peer_timeout_ms = 200;
        c.push_back({"X-04", "opener before creator, opener on an initializing or dead segment", "standard", 30,
                     {variant("before-creator", before, {ok_pair()}),
                      variant("dead", dead,
                              {pair_step("crash", "ok", {{"creator", {"--crash-after", "exit"}}}),
                               StepDef{{"opener"}, {{"opener", "ok"}}, {}}})},
                     run_x04, false});

        c.push_back({"X-05", "ring full / backpressure, deadline on a full ring", "standard", 30,
                     {variant("ring2", params(2, 256), {ok_pair()})}, run_x05});

        Params x06ring2 = params(2, 256);
        x06ring2.messages = 20000;
        Params x06ring256 = params(256, 256);
        x06ring256.messages = 20000;
        c.push_back({"X-06", "bidirectional stream, sequence and CRC, ring of 2 and 256", "standard", 120,
                     {variant("ring2", x06ring2, {ok_pair()}), variant("ring256", x06ring256, {ok_pair()})},
                     run_x06});

        Params x07 = params(4, 256);
        x07.messages = 50;
        Params x07short = x07;
        x07short.heartbeat_ms = 50;
        x07short.peer_timeout_ms = 300;
        c.push_back({"X-07", "kill -9 of the peer (running and parked), default and short peer timeout", "standard", 60,
                     {variant("creator-killed-running", x07,
                              {pair_step("crash", "ok", {{"creator", {"--crash-after", "msgs:50"}}})}),
                      variant("opener-killed-parked", x07,
                              {pair_step("ok", "crash", {{"opener", {"--crash-after", "blocked"}}})}),
                      variant("short-peer-timeout", x07short,
                              {pair_step("crash", "ok", {{"creator", {"--crash-after", "msgs:50"}}})})},
                     run_x07});

        Params x12 = params(2, 256);
        x12.messages = 20;
        c.push_back({"X-12", "parked consumer woken by a producer (ping-pong, ring of 2)", "standard", 60,
                     {variant("ring2", x12, {ok_pair()})}, run_x12});

        Params x14 = params(4, 256);
        x14.messages = 5;
        c.push_back({"X-14", "simple-mode peer: round trips with 300 ms idle", "standard", 60,
                     {variant("adaptive", x14, {ok_pair()}),
                      variant("spin-peer", x14, {pair_step("ok", "ok", {{"opener", {"--wait", "spin"}}})})},
                     run_x14});

        const std::vector<std::string> a01_names = {"all-features",   "opener-initiates", "simultaneous",
                                                    "messages-only",  "request-response-only", "none"};
        std::vector<VariantDef> a01;
        for (const auto& n : a01_names) {
            a01.push_back(variant(n, params(4, 256), {ok_pair()}));
        }
        c.push_back({"X-A01", "negotiation (X-15, SPEC 10.1): creator-initiated, opener-initiated, simultaneous requests, "
                              "feature subsets (messages only, request-response only, none)",
                     "advanced", 30, a01, run_a01, false});

        Params sizes = params(4, 256);
        sizes.max_message = 65536;
        c.push_back({"X-A02", "message sizes (X-16, SPEC 10.4-10.5): sizes 0, 1, bufferSize, bufferSize+1, 3 x "
                              "bufferSize+7 and MaxMessageSize, local refusal of MaxMessageSize+1",
                     "advanced", 30, {variant("sizes", sizes, {ok_pair()})}, run_a02, false});

        c.push_back({"X-A04", "request/response (X-18, SPEC 10.6): 16 concurrent calls whose handlers sleep inversely "
                              "to the id; an error response with status 42 and payload; a call cancelled after 50 ms "
                              "whose late response is discarded (AbandonedResponses >= 1); an unknown method answered "
                              "with status 6 (ErrCodeInvalidOp). Features: messages and request-response (1.0 set, so "
                              "late responses are sent)",
                     "advanced", 30, {variant("default", params(4, 256), {ok_pair()})}, run_a04});

        c.push_back({"X-A05", "streams (X-19, SPEC 10.2-10.3): basic: OpenStream refused while not listening "
                              "(ErrConnNotFound), then ids 2 and 4 (Primary parity, never reused), data both ways, peer "
                              "sees ErrPeerClosed after the queued data, stream 0 not closable (ErrInvalidOp), second "
                              "Close ErrClosed",
                     "advanced", 30, {variant("basic", params(4, 256), {ok_pair()})}, run_a05, false});

        c.push_back({"X-A08", "request metadata (1.1 FeatureMetadata, PROTOCOL_1_1 section 3): metadata variant: CallWith "
                              "caller 42 and a trace ID reach Request.Caller and Request.Trace; a 2 s deadline reaches the "
                              "handler's context as a deadline within (1 s, 2 s]; a call whose deadline already passed "
                              "fails locally and sends nothing. without-metadata variant (FeatureMetadata not negotiated): "
                              "the options are ignored and no deadline reaches the handler",
                     "advanced", 30,
                     {variant("metadata", params(4, 256), {ok_pair()}),
                      variant("without-metadata", params(4, 256), {ok_pair()})},
                     run_a08});

        c.push_back({"X-A09", "graceful shutdown (1.1 section 6): the responder runs a request A that blocks until "
                              "released; the requester calls B until it is refused with status 8 (ErrShuttingDown). "
                              "Shutdown does not return while A is in flight; after release A completes normally, "
                              "Shutdown and Serve return nil, RefusedRequests >= 1; data flows both ways afterwards",
                     "advanced", 30, {variant("default", params(4, 256), {ok_pair()})}, run_a09});

        c.push_back({"X-A10", "acceptor (LISTENER_DRAFT, Listen/Dial/Connect): the creator runs NewAcceptor under the "
                              "name; a second NewAcceptor on the same name fails with ErrListenerBusy; 3 clients Connect "
                              "one after the other, each link answers one request (echo) and is closed; the pending "
                              "Accept fails with ErrAcceptorClosed after Close",
                     "acceptor", 30, {variant("default", params(4, 256), {ok_pair()})}, run_a10});

        return c;
    }();
    return cat;
}

std::string params_json(const Params& p) {
    std::vector<std::string> kv;
    auto add = [&kv](const char* k, long long v) {
        if (v != 0) {
            kv.push_back(json_str(k) + ":" + std::to_string(v));
        }
    };
    add("buffer_count", p.buffer_count);
    add("buffer_size", p.buffer_size);
    add("size", p.size);
    add("messages", p.messages);
    add("seed", static_cast<long long>(p.seed));
    add("heartbeat_ms", p.heartbeat_ms);
    add("peer_timeout_ms", p.peer_timeout_ms);
    add("attach_timeout_ms", p.attach_timeout_ms);
    add("max_message", static_cast<long long>(p.max_message));
    add("max_reassembly", static_cast<long long>(p.max_reassembly));
    add("queue_size", p.queue_size);
    return "{" + join(kv, ",") + "}";
}

std::string string_array(const std::vector<std::string>& v) {
    std::vector<std::string> parts;
    for (const auto& s : v) {
        parts.push_back(json_str(s));
    }
    return "[" + join(parts, ",") + "]";
}

std::string step_json(const StepDef& s) {
    std::vector<std::string> expect;
    for (const auto& kv : s.expect) {
        expect.push_back(json_str(kv.first) + ":" + json_str(kv.second));
    }
    std::string out = "{\"roles\":" + string_array(s.roles) + ",\"expect\":{" + join(expect, ",") + "}";
    if (!s.args.empty()) {
        std::vector<std::string> args;
        for (const auto& kv : s.args) {
            args.push_back(json_str(kv.first) + ":" + string_array(kv.second));
        }
        out += ",\"args\":{" + join(args, ",") + "}";
    }
    return out + "}";
}

std::string variant_json(const VariantDef& v) {
    std::vector<std::string> steps;
    for (const auto& s : v.steps) {
        steps.push_back(step_json(s));
    }
    return "{\"name\":" + json_str(v.name) + ",\"params\":" + params_json(v.params) + ",\"steps\":[" +
           join(steps, ",") + "]}";
}

std::string list_json(const std::string& impl) {
    std::vector<std::string> scenarios;
    for (const auto& s : catalogue()) {
        if (!s.listed) {
            continue;
        }
        std::vector<std::string> variants;
        for (const auto& v : s.variants) {
            variants.push_back(variant_json(v));
        }
        scenarios.push_back("{\"id\":" + json_str(s.id) + ",\"title\":" + json_str(s.title) +
                            ",\"kind\":" + json_str(s.kind) + ",\"platforms\":[\"any\"],\"timeout_s\":" +
                            std::to_string(s.timeout_s) + ",\"variants\":[" + join(variants, ",") + "]}");
    }
    return "{\"helper\":\"hqq-interop\",\"impl\":" + json_str(impl) + ",\"protocol\":" + json_str(kProtocol) +
           ",\"layout\":" + json_str(kSegmentLayout) + ",\"runtime\":\"c++17\",\"scenarios\":[" +
           join(scenarios, ",") + "]}";
}

const ScenarioDef* find_scenario(const std::string& id) {
    for (const auto& s : catalogue()) {
        if (s.id == id) {
            return &s;
        }
    }
    return nullptr;
}

const VariantDef* find_variant(const ScenarioDef& s, const std::string& name) {
    if (name.empty()) {
        return &s.variants.front();
    }
    for (const auto& v : s.variants) {
        if (v.name == name) {
            return &v;
        }
    }
    return nullptr;
}

// Variant parameters fill the flags that were not given explicitly (INTEROP_CLI.md 2).
void apply_params(Config& c, const Params& p) {
    auto given = [&c](const char* f) { return c.explicit_flags.count(f) != 0; };
    if (p.buffer_count != 0 && !given("buffer-count")) c.buffer_count = p.buffer_count;
    if (p.buffer_size != 0 && !given("buffer-size")) c.buffer_size = p.buffer_size;
    if (p.size != 0 && !given("size")) c.size = p.size;
    if (p.messages != 0 && !given("messages")) c.messages = p.messages;
    if (p.seed != 0 && !given("seed")) c.seed = p.seed;
    if (p.heartbeat_ms != 0 && !given("heartbeat")) c.heartbeat_ms = p.heartbeat_ms;
    if (p.peer_timeout_ms != 0 && !given("peer-timeout")) c.peer_timeout_ms = p.peer_timeout_ms;
    if (p.attach_timeout_ms != 0 && !given("attach-timeout")) c.attach_timeout_ms = p.attach_timeout_ms;
    if (p.max_message != 0 && !given("max-message")) c.max_message = p.max_message;
    if (p.max_reassembly != 0 && !given("max-reassembly")) c.max_reassembly = p.max_reassembly;
    if (p.queue_size != 0 && !given("queue-size")) c.queue_size = p.queue_size;
}

// Kills the process at its deadline plus 2 s, reporting HarnessTimeout (exit 3).
void start_watchdog(Env* e) {
    std::thread([e] {
        std::this_thread::sleep_until(e->deadline + 2s);
        if (emit_once(e->render(false, false, "HarnessTimeout", "process exceeded its deadline", std::nullopt))) {
            std::_Exit(kExitTimeout);
        }
    }).detach();
}

int run_command(const std::vector<std::string>& args) {
    Config cfg;
    if (std::string err = parse_run(args, cfg); !err.empty()) {
        std::fprintf(stderr, "hqq-interop: %s\n", err.c_str());
        return kExitUsage;
    }
    const ScenarioDef* scenario = find_scenario(cfg.scenario);
    if (scenario == nullptr) {
        std::fprintf(stderr, "hqq-interop: unknown scenario \"%s\"\n", cfg.scenario.c_str());
        return kExitUsage;
    }
    const VariantDef* variant_def = find_variant(*scenario, cfg.variant);
    if (variant_def == nullptr) {
        std::fprintf(stderr, "hqq-interop: scenario %s has no variant \"%s\"\n", scenario->id.c_str(),
                     cfg.variant.c_str());
        return kExitUsage;
    }
    if (static_cast<std::size_t>(cfg.step) > variant_def->steps.size()) {
        std::fprintf(stderr, "hqq-interop: scenario %s variant \"%s\" has %zu steps\n", scenario->id.c_str(),
                     variant_def->name.c_str(), variant_def->steps.size());
        return kExitUsage;
    }
    const StepDef& step = variant_def->steps[static_cast<std::size_t>(cfg.step - 1)];
    if (std::find(step.roles.begin(), step.roles.end(), cfg.role) == step.roles.end()) {
        std::fprintf(stderr, "hqq-interop: role \"%s\" is not part of step %d of %s\n", cfg.role.c_str(), cfg.step,
                     scenario->id.c_str());
        return kExitUsage;
    }
    if (cfg.sync_dir.empty() && step.roles.size() > 1) {
        std::fprintf(stderr, "hqq-interop: --sync-dir is required for multi-process steps\n");
        return kExitUsage;
    }
    cfg.variant = variant_def->name;
    apply_params(cfg, variant_def->params);
    if (cfg.size == 0) {
        cfg.size = cfg.buffer_size;
    }

    Env env(cfg);
    start_watchdog(&env);
    bool ok = false;
    std::string kind;
    std::string error;
    std::optional<std::uint64_t> status;
    try {
        scenario->run(env);
        if (env.failed()) {
            kind = "CheckFailed";
            error = "one or more checks failed";
        } else {
            ok = true;
        }
    } catch (const Failure& f) {
        kind = f.kind;
        error = f.text;
        status = f.status;
    }
    emit_once(env.render(ok, false, kind, error, status));
    return ok ? kExitOK : kExitFailed;
}

int list_command(const std::vector<std::string>& args) {
    std::map<std::string, std::string> v;
    if (std::string err = parse_flags(args, {{"impl", true}}, v); !err.empty()) {
        std::fprintf(stderr, "hqq-interop: %s\n", err.c_str());
        return kExitUsage;
    }
    const std::string impl = v.count("impl") != 0 ? v["impl"] : kDefaultImpl;
    const std::string doc = list_json(impl) + "\n";
    std::fwrite(doc.data(), 1, doc.size(), stdout);
    std::fflush(stdout);
    return kExitOK;
}

int version_command(const std::vector<std::string>& args) {
    if (!args.empty()) {
        std::fprintf(stderr, "hqq-interop: version takes no arguments\n");
        return kExitUsage;
    }
    const std::string doc = std::string("{\"helper\":\"hqq-interop\",\"impl\":\"") + kDefaultImpl +
                            "\",\"protocol\":\"" + kProtocol + "\",\"segment_layout\":\"" + kSegmentLayout +
                            "\",\"ring_layout\":" + std::to_string(kRingLayout) + "}\n";
    std::fwrite(doc.data(), 1, doc.size(), stdout);
    std::fflush(stdout);
    return kExitOK;
}

void usage(std::FILE* f) {
    std::fprintf(f, "usage:\n"
                    "  hqq-interop run --scenario ID --role creator|opener|a|b --name NAME [flags]\n"
                    "  hqq-interop list [--impl LABEL]\n"
                    "  hqq-interop version\n");
}

}  // namespace

int main(int argc, char** argv) {
    std::vector<std::string> args(argv + 1, argv + argc);
    if (args.empty()) {
        usage(stderr);
        return kExitUsage;
    }
    const std::string cmd = args.front();
    const std::vector<std::string> rest(args.begin() + 1, args.end());
    if (cmd == "run") {
        return run_command(rest);
    }
    if (cmd == "list") {
        return list_command(rest);
    }
    if (cmd == "version") {
        return version_command(rest);
    }
    if (cmd == "-h" || cmd == "--help" || cmd == "help") {
        usage(stdout);
        return kExitOK;
    }
    std::fprintf(stderr, "hqq-interop: unknown subcommand \"%s\"\n", cmd.c_str());
    usage(stderr);
    return kExitUsage;
}
