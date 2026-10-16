# hqq-sqlite C server (not implemented yet)

This directory is reserved for the C server of the hqq-sqlite service. It is
a stub: nothing here builds yet. The Go fake server in `../fakeserver` is the
reference for the observable behavior, and `../PROTOCOL.md` is the contract.

## What it must implement

1. **Listener.** Create an HQQ acceptor on the configured listener name. Each
   accepted link gets `ListenStreams`, then an `AcceptStream` loop. Each
   accepted stream is one session.
2. **Sessions.** One SQLite connection per session, opened with
   `sqlite3_open_v2` on Open (PROTOCOL.md section 6.1). The database name is
   resolved through the alias table of the server configuration, and raw paths
   are used only if the policy allows them. Apply the busy timeout with
   `sqlite3_busy_timeout`, and apply the pragmas with validated names and
   values only.
3. **Methods.** Handle all fourteen methods of PROTOCOL.md section 6, with the
   byte layouts of section 3 and the rules of sections 4 to 6. Build responses
   and errors exactly as sections 5 and 7 describe.
4. **Errors.** Send SQLite errors as `0x10000 + rc` with `sqlite3_errmsg()`,
   and protocol errors as `0x20000 + code` (section 7).
5. **Cancellation.** When a request's handler context ends (cancel or
   deadline), call `sqlite3_interrupt()` on the session connection, check the
   context between steps, and send no response (section 9).
6. **Teardown.** When a stream closes, or the link is lost, roll back an open
   transaction, finalize statements and cursors, and close the connection with
   `sqlite3_close_v2` (section 8.3).
7. **Limits and the trust model.** Set the `sqlite3_limit()` values, disable
   ATTACH, DETACH, and loadable extensions, and enforce the limits of section
   11 (section 12).

## Dependencies

- **libhqq Advanced layer (draft, in progress).** The working tree has a draft
  of `c/include/hqq/hqq_advanced.h` and `hqq_listener.h`. The calls this server
  needs are:
  - listener: `hqq_acceptor_create`, `hqq_acceptor_accept`, `hqq_acceptor_close`,
    and `hqq_connect` on the client side;
  - streams: `hqq_adv_listen_streams`, `hqq_adv_accept_stream`, `hqq_stream_recv`
    (session watcher), `hqq_stream_close`;
  - requests: `hqq_adv_serve` with `hqq_request_method`, `hqq_request_data`,
    `hqq_request_stream_id`, and the response calls;
  - cancellation and deadlines: `hqq_req_ctx_wait_done` (the hook that calls
    `sqlite3_interrupt`), `hqq_req_ctx_err`, and `hqq_req_ctx_deadline`.
  The names are draft and may change. The protocol does not depend on them.
- **SQLite amalgamation.** `sqlite3.c` and `sqlite3.h`, vendored or fetched by
  the build. No system SQLite is assumed.

## Done means

- The Go driver test suite (`../driver`) passes when pointed at this server.
  That needs a small harness that starts the C server instead of
  `fakeserver.Listen`, and it is not written yet.
- The golden vectors of PROTOCOL.md section 16 decode identically in C.
