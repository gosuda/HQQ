/*
 * hqq.hpp - header-only C++17 RAII wrapper over libhqq (the C library).
 *
 * Covers the Standard link (hqq.h), the Advanced layer (hqq_advanced.h) and
 * the acceptor / connect of the listener draft (hqq_listener.h). Every C
 * handle is owned by exactly one C++ object that releases it in its
 * destructor; messages (hqq_msg) are released by ~Message().
 *
 * Errors
 * ------
 *  Functions return hqq::Status (a hqq_status with std::error_code
 *  integration), or hqq::Result<T> (a value or a Status). Nothing throws
 *  unless HQQ_CPP_EXCEPTIONS is defined to 1 before including this header:
 *  then Result::value() and Status::throw_if_error() throw hqq::Error (a
 *  std::system_error). With exceptions OFF (the default) a value() on a failed
 *  Result prints the status and calls std::abort(), so check first.
 *
 * Views
 * -----
 *  Bytes / MutableBytes are non-owning views (pointer + size) that accept any
 *  contiguous container of bytes (std::string_view, std::string,
 *  std::vector<char|unsigned char|std::byte>, std::array, std::span when
 *  available). They never own memory.
 *
 * Lifetimes and threads
 * ---------------------
 *  Link, AdvLink and Acceptor are move-only. Their member functions may be
 *  called from several threads at once, as the C API documents. Moving or
 *  destroying a handle must not overlap with any use of it. Stream and
 *  Acceptor/AdvLink views borrow: a Stream is valid until its AdvLink is
 *  closed or destroyed. Message owns an hqq_msg and may outlive its link
 *  (the C API allows releasing it afterwards). Request, Response and
 *  RequestContext are only valid inside the serve handler.
 *
 * Waits
 * -----
 *  Every blocking call takes a hqq::Wait: timeout_ns < 0 blocks forever
 *  (the default), 0 never blocks, > 0 is a relative timeout; an optional
 *  CancelToken aborts the call with HQQ_ERR_CANCELED.
 *
 * Not covered: zero-copy callbacks (hqq_read_zc / hqq_write_zc) and the
 * direct hqq_adv_base() view are available through the C header only.
 */
#ifndef HQQ_HQQ_HPP
#define HQQ_HQQ_HPP

#ifndef HQQ_CPP_EXCEPTIONS
#  define HQQ_CPP_EXCEPTIONS 0
#endif

#include <array>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <system_error>
#include <type_traits>
#include <utility>
#include <vector>

#if __cplusplus >= 202002L && __has_include(<span>)
#  include <span>
#  define HQQ_CPP_HAS_SPAN 1
#else
#  define HQQ_CPP_HAS_SPAN 0
#endif

#include "hqq/hqq.h"
#include "hqq/hqq_advanced.h"
#include "hqq/hqq_listener.h"

#if !defined(__cplusplus) || __cplusplus < 201703L
#  if !(defined(_MSVC_LANG) && _MSVC_LANG >= 201703L)
#    error "hqq.hpp requires C++17"
#  endif
#endif

namespace hqq {

// ---- Status and errors ----------------------------------------------------

namespace detail {

class StatusCategory final : public std::error_category {
public:
    const char* name() const noexcept override { return "hqq"; }
    std::string message(int ev) const override {
        if (ev < 0 || ev > static_cast<int>(HQQ_ERR_LISTENER_NOT_READY)) {
            return "unknown hqq status";
        }
        return hqq_strerror(static_cast<hqq_status>(ev));
    }
};

}  // namespace detail

/* The std::error_category of hqq_status values (name "hqq"). */
inline const std::error_category& error_category() noexcept {
    static const detail::StatusCategory instance;
    return instance;
}

class Status {
public:
    constexpr Status() noexcept = default;
    constexpr explicit Status(hqq_status code) noexcept : code_(code) {}

    constexpr hqq_status native() const noexcept { return code_; }
    constexpr bool ok() const noexcept { return code_ == HQQ_OK; }
    constexpr explicit operator bool() const noexcept { return ok(); }

    /* Go errors.Is relation of hqq_error_is(): true when this status is `base` or wraps it. */
    bool matches(hqq_status base) const noexcept { return hqq_error_is(code_, base) != 0; }
    /* True for the statuses a retrying opener should retry (SPEC 4). */
    bool transient_open() const noexcept { return hqq_error_is_transient_open(code_) != 0; }

    const char* message() const noexcept { return hqq_strerror(code_); }
    std::error_code error_code() const noexcept {
        return {static_cast<int>(code_), error_category()};
    }

    /* Throws hqq::Error when HQQ_CPP_EXCEPTIONS is 1; otherwise aborts on failure. */
    inline void throw_if_error() const;

    friend constexpr bool operator==(Status a, Status b) noexcept { return a.code_ == b.code_; }
    friend constexpr bool operator!=(Status a, Status b) noexcept { return !(a == b); }
    friend constexpr bool operator==(Status a, hqq_status code) noexcept { return a.code_ == code; }
    friend constexpr bool operator!=(Status a, hqq_status code) noexcept { return !(a == code); }

private:
    hqq_status code_ = HQQ_OK;
};

/* Exception type of the failure paths when HQQ_CPP_EXCEPTIONS is 1. */
class Error : public std::system_error {
public:
    explicit Error(Status status)
        : std::system_error(status.error_code(), status.message()), status_(status) {}
    Status status() const noexcept { return status_; }

private:
    Status status_;
};

[[noreturn]] inline void fail(Status status) {
#if HQQ_CPP_EXCEPTIONS
    throw Error(status);
#else
    std::fprintf(stderr, "hqq: unhandled error: %s\n", status.message());
    std::abort();
#endif
}

inline void Status::throw_if_error() const {
    if (!ok()) {
        fail(*this);
    }
}

/* A value of T, or the Status that failed to produce it. */
template <class T>
class Result {
public:
    Result(T value) : value_(std::move(value)) {}  // NOLINT(google-explicit-constructor)
    Result(Status status) : status_(status) {}     // NOLINT(google-explicit-constructor)

    bool ok() const noexcept { return value_.has_value(); }
    explicit operator bool() const noexcept { return ok(); }
    Status status() const noexcept { return status_; }

    T& value() & {
        check();
        return *value_;
    }
    const T& value() const& {
        check();
        return *value_;
    }
    T&& value() && {
        check();
        return std::move(*value_);
    }

    T* operator->() {
        check();
        return std::addressof(*value_);
    }
    const T* operator->() const {
        check();
        return std::addressof(*value_);
    }

private:
    void check() const {
        if (!value_) {
            fail(status_);
        }
    }

    Status status_;
    std::optional<T> value_;
};

// ---- Views ------------------------------------------------------------------

namespace detail {

template <class C>
using DataOf = decltype(std::declval<const C&>().data());
template <class C>
using SizeOf = decltype(std::declval<const C&>().size());
template <class C>
using MutDataOf = decltype(std::declval<C&>().data());
template <class C>
using MutSizeOf = decltype(std::declval<C&>().size());
template <class C>
using ConstElem = std::remove_pointer_t<DataOf<C>>;
template <class C>
using MutElem = std::remove_pointer_t<MutDataOf<C>>;

}  // namespace detail

/* Non-owning view of const bytes. */
class Bytes {
public:
    constexpr Bytes() noexcept = default;
    constexpr Bytes(const void* data, std::size_t size) noexcept : data_(data), size_(size) {}

    /* Any contiguous container with byte-sized elements (std::string_view, std::vector, ...). */
    template <class C, class = detail::DataOf<C>, class = detail::SizeOf<C>,
              class = std::enable_if_t<!std::is_void_v<detail::ConstElem<C>>>>
    Bytes(const C& c) noexcept : Bytes(c.data(), c.size()) {  // NOLINT(google-explicit-constructor)
        static_assert(sizeof(detail::ConstElem<C>) == 1, "hqq::Bytes needs byte-sized elements");
    }

    /* Byte arrays (std::byte[N], unsigned char[N], ...). */
    template <class T, std::size_t N, class = std::enable_if_t<sizeof(T) == 1>>
    Bytes(const T (&array)[N]) noexcept : Bytes(array, N) {}  // NOLINT(google-explicit-constructor)

    constexpr const void* data() const noexcept { return data_; }
    constexpr std::size_t size() const noexcept { return size_; }
    constexpr bool empty() const noexcept { return size_ == 0; }
    const std::byte* begin() const noexcept { return static_cast<const std::byte*>(data_); }
    const std::byte* end() const noexcept { return begin() + size_; }
    std::string_view as_string_view() const noexcept {
        return {static_cast<const char*>(data_), size_};
    }
#if HQQ_CPP_HAS_SPAN
    std::span<const std::byte> as_span() const noexcept { return {begin(), size_}; }
#endif

private:
    const void* data_ = nullptr;
    std::size_t size_ = 0;
};

/* Non-owning view of writable bytes. */
class MutableBytes {
public:
    constexpr MutableBytes() noexcept = default;
    constexpr MutableBytes(void* data, std::size_t size) noexcept : data_(data), size_(size) {}

    /* Byte arrays (std::byte[N], unsigned char[N], ...). */
    template <class T, std::size_t N, class = std::enable_if_t<sizeof(T) == 1 && !std::is_const_v<T>>>
    MutableBytes(T (&array)[N]) noexcept : MutableBytes(array, N) {}  // NOLINT(google-explicit-constructor)

    template <class C, class = detail::MutDataOf<C>, class = detail::MutSizeOf<C>,
              class = std::enable_if_t<!std::is_void_v<detail::MutElem<C>> &&
                                       !std::is_const_v<detail::MutElem<C>>>>
    MutableBytes(C& c) noexcept : MutableBytes(c.data(), c.size()) {  // NOLINT(google-explicit-constructor)
        static_assert(sizeof(detail::MutElem<C>) == 1, "hqq::MutableBytes needs byte-sized elements");
    }

    constexpr void* data() const noexcept { return data_; }
    constexpr std::size_t size() const noexcept { return size_; }
    std::byte* begin() const noexcept { return static_cast<std::byte*>(data_); }
    std::byte* end() const noexcept { return begin() + size_; }
    operator Bytes() const noexcept { return Bytes(data_, size_); }  // NOLINT(google-explicit-constructor)

private:
    void* data_ = nullptr;
    std::size_t size_ = 0;
};

// ---- Cancellation, waits, options ---------------------------------------------

/* A cancellation token (hqq_cancel). Signalling is idempotent and thread-safe. */
class CancelToken {
public:
    CancelToken() noexcept { hqq_cancel_init(&token_); }
    CancelToken(const CancelToken&) = delete;
    CancelToken& operator=(const CancelToken&) = delete;

    void signal() noexcept { hqq_cancel_signal(&token_); }
    bool signalled() const noexcept { return hqq_cancel_is_signalled(&token_) != 0; }
    const hqq_cancel* native() const noexcept { return &token_; }

private:
    hqq_cancel token_{};
};

/* Per-call wait control (hqq_wait). Converted to the C struct by native(). */
struct Wait {
    std::int64_t timeout_ns = -1;         /* < 0: no timeout, 0: never block, > 0: relative */
    const CancelToken* cancel = nullptr;  /* optional */

    static Wait forever(const CancelToken* token = nullptr) noexcept { return Wait{-1, token}; }
    static Wait poll() noexcept { return Wait{0, nullptr}; }
    template <class Rep, class Period>
    static Wait for_duration(std::chrono::duration<Rep, Period> d,
                             const CancelToken* token = nullptr) noexcept {
        return Wait{static_cast<std::int64_t>(
                        std::chrono::duration_cast<std::chrono::nanoseconds>(d).count()),
                    token};
    }

    hqq_wait native() const noexcept {
        hqq_wait w{};
        w.timeout_ns = timeout_ns;
        w.cancel = cancel ? cancel->native() : nullptr;
        return w;
    }
};

/* Option structs: the C defaults are set by the constructor; fields are the C fields. */
struct LinkOptions : hqq_options {
    LinkOptions() noexcept { hqq_options_init(this); }
};
struct AdvOptions : hqq_adv_options {
    AdvOptions() noexcept { hqq_adv_options_init(this); }
};
struct CallOptions : hqq_call_options {
    CallOptions() noexcept { hqq_call_options_init(this); }
};
struct AcceptorOptions : hqq_acceptor_options {
    AcceptorOptions() noexcept { hqq_acceptor_options_init(this); }
};
struct ConnectOptions : hqq_connect_options {
    ConnectOptions() noexcept { hqq_connect_options_init(this); }
};

/* Detailed result of an open call (hqq_open_info). */
struct OpenInfo : hqq_open_info {
    OpenInfo() noexcept { hqq_open_info_init(this); }
};

using AdvStats = hqq_adv_stats;
using AcceptorStats = hqq_acceptor_stats;

/* Version of the linked library. */
inline std::string_view version() noexcept { return hqq_version_string(); }
inline unsigned version_number() noexcept { return hqq_version_number(); }

// ---- Messages -------------------------------------------------------------------

/* A received message (hqq_msg). Released in the destructor. Move-only. */
class Message {
public:
    Message() noexcept = default;
    explicit Message(hqq_msg* msg) noexcept : msg_(msg) {}
    Message(Message&& other) noexcept : msg_(std::exchange(other.msg_, nullptr)) {}
    Message& operator=(Message&& other) noexcept {
        if (this != &other) {
            reset();
            msg_ = std::exchange(other.msg_, nullptr);
        }
        return *this;
    }
    Message(const Message&) = delete;
    Message& operator=(const Message&) = delete;
    ~Message() { reset(); }

    explicit operator bool() const noexcept { return msg_ != nullptr; }
    Bytes data() const noexcept {
        return msg_ ? Bytes(hqq_msg_data(msg_), hqq_msg_len(msg_)) : Bytes();
    }
    std::uint64_t stream_id() const noexcept { return msg_ ? hqq_msg_stream_id(msg_) : 0; }
    /* Status of an error response (0 for every other message). */
    std::uint64_t status() const noexcept { return msg_ ? hqq_msg_status(msg_) : 0; }

    /* Returns the message to its pool now; the destructor does the same. */
    void reset() noexcept {
        hqq_msg_release(msg_);
        msg_ = nullptr;
    }
    hqq_msg* native() const noexcept { return msg_; }

private:
    hqq_msg* msg_ = nullptr;
};

// ---- Standard layer -----------------------------------------------------------

class Link;

/* A claimed slot of a link's write direction (hqq_write_reserve). Completed by
 * commit() or abort(); destroyed while pending, it publishes a tombstone. */
class WriteReservation {
public:
    WriteReservation(WriteReservation&& other) noexcept
        : res_(other.res_), pending_(std::exchange(other.pending_, false)) {}
    WriteReservation& operator=(WriteReservation&& other) noexcept {
        if (this != &other) {
            abort_pending();
            res_ = other.res_;
            pending_ = std::exchange(other.pending_, false);
        }
        return *this;
    }
    WriteReservation(const WriteReservation&) = delete;
    WriteReservation& operator=(const WriteReservation&) = delete;
    ~WriteReservation() { abort_pending(); }

    /* The slot-owned payload buffer (capacity bytes). */
    MutableBytes payload() const noexcept { return MutableBytes(res_.data, res_.capacity); }
    /* Publishes the first len bytes of payload(). */
    Status commit(std::size_t len) noexcept {
        pending_ = false;
        return Status(hqq_write_commit(&res_, len));
    }
    /* Publishes a tombstone; reason is an HQQ_ERRCODE_* value (0: HQQ_ERRCODE_INVALID_OP). */
    Status abort(unsigned reason = 0) noexcept {
        pending_ = false;
        return Status(hqq_write_abort(&res_, reason));
    }

private:
    friend class Link;
    explicit WriteReservation(const hqq_write_reservation& res) noexcept : res_(res), pending_(true) {}
    void abort_pending() noexcept {
        if (pending_) {
            (void)hqq_write_abort(&res_, HQQ_ERRCODE_INVALID_OP);
            pending_ = false;
        }
    }

    hqq_write_reservation res_{};
    bool pending_ = false;
};

/* A claimed message of a link's read direction (hqq_read_reserve). Released in
 * the destructor. */
class ReadReservation {
public:
    ReadReservation(ReadReservation&& other) noexcept
        : res_(other.res_), pending_(std::exchange(other.pending_, false)) {}
    ReadReservation& operator=(ReadReservation&& other) noexcept {
        if (this != &other) {
            release_pending();
            res_ = other.res_;
            pending_ = std::exchange(other.pending_, false);
        }
        return *this;
    }
    ReadReservation(const ReadReservation&) = delete;
    ReadReservation& operator=(const ReadReservation&) = delete;
    ~ReadReservation() { release_pending(); }

    /* The message payload, valid until release(). */
    Bytes payload() const noexcept { return Bytes(res_.data, res_.size); }
    Status release() noexcept {
        pending_ = false;
        return Status(hqq_read_release(&res_));
    }

private:
    friend class Link;
    explicit ReadReservation(const hqq_read_reservation& res) noexcept : res_(res), pending_(true) {}
    void release_pending() noexcept {
        if (pending_) {
            (void)hqq_read_release(&res_);
            pending_ = false;
        }
    }

    hqq_read_reservation res_{};
    bool pending_ = false;
};

/* A Standard link (hqq_link): two endpoints, one message per write. */
class Link {
public:
    /* Creates the named segment as the Primary (hqq_standard_create). */
    static Result<Link> create(std::string_view name, std::uint64_t buffer_count,
                               std::uint64_t buffer_size, unsigned perm = 0600,
                               const LinkOptions& opts = {}, OpenInfo* info = nullptr) {
        const std::string n(name);
        hqq_link* out = nullptr;
        Status st(hqq_standard_create(n.c_str(), buffer_count, buffer_size, perm, &opts, &out, info));
        return adopt(st, out);
    }
    /* Attaches to an existing named segment as the Secondary (hqq_standard_open). */
    static Result<Link> open(std::string_view name, std::uint64_t buffer_count,
                             std::uint64_t buffer_size, const LinkOptions& opts = {},
                             OpenInfo* info = nullptr) {
        const std::string n(name);
        hqq_link* out = nullptr;
        Status st(hqq_standard_open(n.c_str(), buffer_count, buffer_size, &opts, &out, info));
        return adopt(st, out);
    }
    /* Caller-owned page-aligned memory of hqq_standard_size() bytes (hqq_standard_open_memory). */
    static Result<Link> open_memory(void* mem, std::size_t mem_size, std::uint64_t buffer_count,
                                    std::uint64_t buffer_size, const LinkOptions& opts = {},
                                    OpenInfo* info = nullptr) {
        hqq_link* out = nullptr;
        Status st(hqq_standard_open_memory(mem, mem_size, buffer_count, buffer_size, &opts, &out, info));
        return adopt(st, out);
    }

    Link(Link&& other) noexcept : link_(std::exchange(other.link_, nullptr)) {}
    Link& operator=(Link&& other) noexcept {
        if (this != &other) {
            close_quietly();
            link_ = std::exchange(other.link_, nullptr);
        }
        return *this;
    }
    Link(const Link&) = delete;
    Link& operator=(const Link&) = delete;
    ~Link() { close_quietly(); }

    /* Publishes bytes as one message (1..buffer_size bytes; 0 sends nothing). */
    Status write(Bytes data, const Wait& wait = {}) noexcept {
        const hqq_wait w = wait.native();
        return Status(hqq_write(link_, data.data(), data.size(), &w));
    }
    /* Consumes one whole message into buf; *len receives its size. A message
     * larger than buf is not consumed: HQQ_ERR_BUFFER_TOO_SMALL with *len = size. */
    Status read(MutableBytes buf, std::size_t* len, const Wait& wait = {}) noexcept {
        std::size_t scratch = 0;
        const hqq_wait w = wait.native();
        return Status(hqq_read(link_, buf.data(), buf.size(), len ? len : &scratch, &w));
    }
    /* Consumes one whole message into a new vector (allocates; retries with the
     * size reported by HQQ_ERR_BUFFER_TOO_SMALL). */
    Result<std::vector<std::byte>> receive(const Wait& wait = {}) {
        std::vector<std::byte> buf(buffer_size());
        for (;;) {
            std::size_t len = 0;
            Status st = read(buf, &len, wait);
            if (st.ok()) {
                buf.resize(len);
                return buf;
            }
            if (st != HQQ_ERR_BUFFER_TOO_SMALL) {
                return st;
            }
            buf.resize(len);
        }
    }

    Result<WriteReservation> reserve_write(const Wait& wait = {});
    Result<ReadReservation> reserve_read(const Wait& wait = {});

    hqq_peer_status peer_state() noexcept { return hqq_peer_state(link_); }
    hqq_mode mode() const noexcept { return hqq_link_mode(link_); }
    std::size_t buffer_size() const noexcept { return hqq_link_buffer_size(link_); }
    std::size_t buffer_count() const noexcept { return hqq_link_buffer_count(link_); }
    /* Name of a named link, nullptr for caller memory. */
    const char* name() const noexcept { return hqq_link_name(link_); }

    /* Stops the link (hqq_shutdown): wakes and fails blocked calls, keeps the handle. */
    void shutdown() noexcept { hqq_shutdown(link_); }
    /* Closes the link (hqq_close). The object is empty afterwards. */
    Status close() noexcept {
        Status st(link_ ? hqq_close(link_) : HQQ_OK);
        link_ = nullptr;
        return st;
    }

    hqq_link* native() const noexcept { return link_; }

private:
    explicit Link(hqq_link* link) noexcept : link_(link) {}

    static Result<Link> adopt(Status st, hqq_link* out) {
        if (!st.ok()) {
            return st;
        }
        return Link(out);
    }
    void close_quietly() noexcept {
        if (link_) {
            (void)hqq_close(link_);
            link_ = nullptr;
        }
    }

    hqq_link* link_ = nullptr;
};

inline Result<WriteReservation> Link::reserve_write(const Wait& wait) {
    hqq_write_reservation res{};
    const hqq_wait w = wait.native();
    Status st(hqq_write_reserve(link_, &res, &w));
    if (!st.ok()) {
        return st;
    }
    return WriteReservation(res);
}

inline Result<ReadReservation> Link::reserve_read(const Wait& wait) {
    hqq_read_reservation res{};
    const hqq_wait w = wait.native();
    Status st(hqq_read_reserve(link_, &res, &w));
    if (!st.ok()) {
        return st;
    }
    return ReadReservation(res);
}

// ---- Advanced layer ---------------------------------------------------------------

class AdvLink;

/* A stream of an Advanced link (hqq_stream). A borrowed handle: it stays valid
 * until the owning AdvLink is closed. Stream 0 is AdvLink::stream0(). */
class Stream {
public:
    Stream() noexcept = default;
    explicit Stream(hqq_stream* stream) noexcept : stream_(stream) {}

    explicit operator bool() const noexcept { return stream_ != nullptr; }
    hqq_stream* native() const noexcept { return stream_; }
    std::uint64_t id() const noexcept { return hqq_stream_id(stream_); }

    /* Sends bytes as one message (chunked to the buffer size; empty is allowed). */
    Status send(Bytes data, const Wait& wait = {}) const noexcept {
        const hqq_wait w = wait.native();
        return Status(hqq_stream_send(stream_, data.data(), data.size(), &w));
    }
    /* Receives the next data message of the stream. */
    Result<Message> recv(const Wait& wait = {}) const {
        hqq_msg* msg = nullptr;
        const hqq_wait w = wait.native();
        Status st(hqq_stream_recv(stream_, &msg, &w));
        if (!st.ok()) {
            return st;
        }
        return Message(msg);
    }
    /* Calls method and waits for the response in resp. On an error response the
     * status is HQQ_ERR_RESPONSE and resp holds it (resp.status(), resp.data()). */
    Status call(std::uint64_t method, Bytes request, Message& resp, const Wait& wait = {}) const {
        return call_with(method, request, CallOptions{}, resp, wait);
    }
    Status call_with(std::uint64_t method, Bytes request, const CallOptions& opts,
                     Message& resp, const Wait& wait = {}) const {
        hqq_msg* msg = nullptr;
        const hqq_wait w = wait.native();
        Status st(hqq_stream_call_with(stream_, method, request.data(), request.size(), &opts, &msg, &w));
        resp = Message(msg);
        return st;
    }
    /* Closes the stream (not stream 0). */
    Status close() const noexcept { return Status(hqq_stream_close(stream_)); }

private:
    hqq_stream* stream_ = nullptr;
};

/* A request as seen by a serve handler (hqq_request). Valid inside the handler. */
class Request {
public:
    explicit Request(const hqq_request* req) noexcept : req_(req) {}
    std::uint64_t method() const noexcept { return hqq_request_method(req_); }
    Bytes data() const noexcept { return Bytes(hqq_request_data(req_), hqq_request_len(req_)); }
    std::uint64_t stream_id() const noexcept { return hqq_request_stream_id(req_); }
    std::uint64_t caller() const noexcept { return hqq_request_caller(req_); }
    std::optional<std::array<std::uint8_t, 16>> trace() const noexcept {
        std::array<std::uint8_t, 16> t{};
        if (hqq_request_trace(req_, t.data()) != 0) {
            return t;
        }
        return std::nullopt;
    }

private:
    const hqq_request* req_;
};

/* The context of one request (hqq_req_ctx): cancellation and deadline. */
class RequestContext {
public:
    explicit RequestContext(hqq_req_ctx* ctx) noexcept : ctx_(ctx) {}
    /* HQQ_OK while live; HQQ_ERR_CANCELED after OpCancel; HQQ_ERR_TIMEOUT past the budget. */
    Status err() const noexcept { return Status(hqq_req_ctx_err(ctx_)); }
    /* Blocks until the context ends or timeout_ns passes (< 0: forever). */
    Status wait_done(std::int64_t timeout_ns = -1) noexcept {
        return Status(hqq_req_ctx_wait_done(ctx_, timeout_ns));
    }
    /* The monotonic deadline (hqq_plat_mono_ns scale) when the request has one. */
    std::optional<std::uint64_t> deadline() const noexcept {
        std::uint64_t d = 0;
        if (hqq_req_ctx_deadline(ctx_, &d) != 0) {
            return d;
        }
        return std::nullopt;
    }

private:
    hqq_req_ctx* ctx_;
};

/* The response writer of one request (hqq_response). Sent when the handler returns. */
class Response {
public:
    explicit Response(hqq_response* writer) noexcept : writer_(writer) {}
    /* Appends to the response body (at most MaxMessageSize in total). */
    Status write(Bytes data) noexcept {
        return Status(hqq_response_write(writer_, data.data(), data.size()));
    }
    /* Replaces the body with an error payload and sets the status. */
    Status error(std::uint64_t status, Bytes payload = {}) noexcept {
        return Status(hqq_response_error(writer_, status, payload.data(), payload.size()));
    }

private:
    hqq_response* writer_;
};

namespace detail {

/* Virtual dispatch for serve handlers: the C callback is a plain extern "C" function. */
class ServeBase {
public:
    virtual void invoke(hqq_req_ctx* ctx, hqq_response* writer, const hqq_request* req) = 0;

protected:
    ~ServeBase() = default;
};

template <class F>
class ServeImpl final : public ServeBase {
public:
    explicit ServeImpl(F& fn) noexcept : fn_(fn) {}
    void invoke(hqq_req_ctx* ctx, hqq_response* writer, const hqq_request* req) override {
        RequestContext c(ctx);
        Request r(req);
        Response w(writer);
        fn_(c, r, w);
    }

private:
    F& fn_;
};

extern "C" inline void hqq_cpp_serve_thunk(void* user, hqq_req_ctx* ctx, hqq_response* writer,
                                           const hqq_request* req) {
    static_cast<ServeBase*>(user)->invoke(ctx, writer, req);
}

}  // namespace detail

/* An Advanced link (hqq_adv): negotiation, streams, calls and serving. */
class AdvLink {
public:
    static Result<AdvLink> create(std::string_view name, std::uint64_t buffer_count,
                                  std::uint64_t buffer_size, unsigned perm = 0600,
                                  const AdvOptions& opts = {}, OpenInfo* info = nullptr) {
        const std::string n(name);
        hqq_adv* out = nullptr;
        Status st(hqq_adv_create(n.c_str(), buffer_count, buffer_size, perm, &opts, &out, info));
        return adopt(st, out);
    }
    static Result<AdvLink> open(std::string_view name, std::uint64_t buffer_count,
                                std::uint64_t buffer_size, const AdvOptions& opts = {},
                                OpenInfo* info = nullptr) {
        const std::string n(name);
        hqq_adv* out = nullptr;
        Status st(hqq_adv_open(n.c_str(), buffer_count, buffer_size, &opts, &out, info));
        return adopt(st, out);
    }
    static Result<AdvLink> open_memory(void* mem, std::size_t mem_size, std::uint64_t buffer_count,
                                       std::uint64_t buffer_size, const AdvOptions& opts = {},
                                       OpenInfo* info = nullptr) {
        hqq_adv* out = nullptr;
        Status st(hqq_adv_open_memory(mem, mem_size, buffer_count, buffer_size, &opts, &out, info));
        return adopt(st, out);
    }

    AdvLink(AdvLink&& other) noexcept
        : link_(std::exchange(other.link_, nullptr)), closed_(std::exchange(other.closed_, false)) {}
    AdvLink& operator=(AdvLink&& other) noexcept {
        if (this != &other) {
            close_quietly();
            link_ = std::exchange(other.link_, nullptr);
            closed_ = std::exchange(other.closed_, false);
        }
        return *this;
    }
    AdvLink(const AdvLink&) = delete;
    AdvLink& operator=(const AdvLink&) = delete;
    ~AdvLink() { close_quietly(); }

    /* Negotiates the protocol; features 0 requests every feature of protocol 1.1. */
    Status negotiate(std::uint64_t features = HQQ_FEATURES_ALL, const Wait& wait = {}) {
        const hqq_wait w = wait.native();
        return Status(hqq_adv_negotiate(link_, features, &w));
    }
    Status negotiate_protocol(std::uint16_t version, std::uint64_t features, const Wait& wait = {}) {
        const hqq_wait w = wait.native();
        return Status(hqq_adv_negotiate_protocol(link_, version, features, &w));
    }
    hqq_proto_state protocol_state() noexcept { return hqq_adv_protocol_state(link_); }
    std::uint16_t negotiated_version() const noexcept { return hqq_adv_negotiated_version(link_); }
    std::uint64_t negotiated_features() const noexcept { return hqq_adv_negotiated_features(link_); }

    /* Stream 0 is owned by the link and must not be closed. */
    Stream stream0() const noexcept { return Stream(hqq_adv_stream0(link_)); }
    Status listen_streams() noexcept { return Status(hqq_adv_listen_streams(link_)); }
    Result<Stream> accept_stream(const Wait& wait = {}) {
        hqq_stream* out = nullptr;
        const hqq_wait w = wait.native();
        Status st(hqq_adv_accept_stream(link_, &out, &w));
        if (!st.ok()) {
            return st;
        }
        return Stream(out);
    }
    Result<Stream> open_stream(const Wait& wait = {}) {
        hqq_stream* out = nullptr;
        const hqq_wait w = wait.native();
        Status st(hqq_adv_open_stream(link_, &out, &w));
        if (!st.ok()) {
            return st;
        }
        return Stream(out);
    }

    /* Serves requests until wait ends, the link closes or shuts down. handler is
     * callable as void(RequestContext&, const Request&, Response&) and runs
     * concurrently on worker threads: it must be thread-safe. Only one serve per link. */
    template <class F>
    Status serve(F&& handler, const Wait& wait = {}) {
        detail::ServeImpl<std::remove_reference_t<F>> impl(handler);
        detail::ServeBase* base = &impl;
        const hqq_wait w = wait.native();
        return Status(hqq_adv_serve(link_, &detail::hqq_cpp_serve_thunk, base, &w));
    }
    /* Graceful shutdown of the responder role (refusals with status 8). */
    Status shutdown(const Wait& wait = {}) {
        const hqq_wait w = wait.native();
        return Status(hqq_adv_shutdown(link_, &w));
    }

    hqq_peer_status peer_state() noexcept { return hqq_adv_peer_state(link_); }
    hqq_mode mode() const noexcept { return hqq_adv_mode(link_); }
    std::size_t buffer_size() const noexcept { return hqq_adv_buffer_size(link_); }
    std::size_t buffer_count() const noexcept { return hqq_adv_buffer_count(link_); }
    AdvStats stats() const noexcept {
        AdvStats s{};
        hqq_adv_stats_get(link_, &s);
        return s;
    }

    /* Removes the name of a named link, when it is still ours. */
    Status unlink() noexcept { return Status(hqq_adv_unlink(link_)); }
    /* Closes the link; streams and messages of it become invalid. The C library waits for
     * calls in progress on other threads, so a concurrent call never races with close();
     * the object must not be used after close. */
    Status close() noexcept {
        if (closed_) {
            return Status(HQQ_ERR_CLOSED);
        }
        closed_ = true;
        return Status(hqq_adv_close(link_));
    }

    hqq_adv* native() const noexcept { return link_; }

private:
    friend Result<AdvLink> connect(std::string_view listener, const ConnectOptions& opts, const Wait& wait);
    friend class Acceptor;
    explicit AdvLink(hqq_adv* link) noexcept : link_(link) {}

    static Result<AdvLink> adopt(Status st, hqq_adv* out) {
        if (!st.ok()) {
            return st;
        }
        return AdvLink(out);
    }
    void close_quietly() noexcept {
        if (link_ && !closed_) {
            hqq_adv_close(link_);
            closed_ = true;
        }
    }

    hqq_adv* link_ = nullptr;
    bool closed_ = false;  /* owner-only: written by close() and the destructor */
};

// ---- Acceptor and connect (LISTENER_DRAFT) ------------------------------------------

/* Service side: a listener that hands out negotiated Advanced links. */
class Acceptor {
public:
    static Result<Acceptor> create(std::string_view name, const AcceptorOptions& opts = {},
                                   OpenInfo* info = nullptr) {
        const std::string n(name);
        hqq_acceptor* out = nullptr;
        Status st(hqq_acceptor_create(n.c_str(), &opts, &out, info));
        if (!st.ok()) {
            return st;
        }
        return Acceptor(out);
    }

    Acceptor(Acceptor&& other) noexcept
        : acc_(std::exchange(other.acc_, nullptr)), closed_(std::exchange(other.closed_, false)) {}
    Acceptor& operator=(Acceptor&& other) noexcept {
        if (this != &other) {
            close_quietly();
            acc_ = std::exchange(other.acc_, nullptr);
            closed_ = std::exchange(other.closed_, false);
        }
        return *this;
    }
    Acceptor(const Acceptor&) = delete;
    Acceptor& operator=(const Acceptor&) = delete;
    ~Acceptor() { close_quietly(); }

    /* Returns the next negotiated link; the link outlives the acceptor. */
    Result<AdvLink> accept(const Wait& wait = {}) {
        hqq_adv* out = nullptr;
        const hqq_wait w = wait.native();
        Status st(hqq_acceptor_accept(acc_, &out, &w));
        return AdvLink::adopt(st, out);
    }
    AcceptorStats stats() const noexcept {
        AcceptorStats s{};
        hqq_acceptor_stats_get(acc_, &s);
        return s;
    }
    /* Stops the listener and frees it (the C library waits for Accept calls in progress,
     * which return HQQ_ERR_ACCEPTOR_CLOSED). Links already delivered stay open. The handle
     * pointer is not written here, so a concurrent accept() never races with close();
     * the object must not be used after close. */
    Status close() noexcept {
        if (closed_) {
            return Status(HQQ_ERR_ACCEPTOR_CLOSED);
        }
        closed_ = true;
        return Status(hqq_acceptor_close(acc_));
    }

private:
    explicit Acceptor(hqq_acceptor* acc) noexcept : acc_(acc) {}
    void close_quietly() noexcept {
        if (acc_ && !closed_) {
            (void)hqq_acceptor_close(acc_);
            closed_ = true;
        }
    }

    hqq_acceptor* acc_ = nullptr;
    bool closed_ = false;  /* owner-only: written by close() and the destructor */
};

/* Client side: attaches to a listener, negotiates and returns the Primary link. */
inline Result<AdvLink> connect(std::string_view listener, const ConnectOptions& opts = {},
                               const Wait& wait = {}) {
    const std::string n(listener);
    hqq_adv* out = nullptr;
    const hqq_wait w = wait.native();
    Status st(hqq_connect(n.c_str(), &opts, &w, &out));
    return AdvLink::adopt(st, out);
}

}  // namespace hqq

#endif  /* HQQ_HQQ_HPP */
