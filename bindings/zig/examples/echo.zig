//! Echo over a named Standard link (UNVERIFIED: not compiled, see build.zig).
//!
//!   echo --create NAME          server: echoes every message until "END"
//!   echo --open NAME [COUNT]    client: sends COUNT messages (default 10),
//!                               checks each echo, then sends "END"
//!
//! Start the server first; it creates the segment.
const std = @import("std");
const hqq = @import("hqq");

pub fn main() !void {
    var arena = std.heap.ArenaAllocator.init(std.heap.page_allocator);
    defer arena.deinit();
    const args = try std.process.argsAlloc(arena.allocator());

    if (args.len >= 3 and std.mem.eql(u8, args[1], "--create")) {
        return server(args[2]);
    }
    if (args.len >= 3 and std.mem.eql(u8, args[1], "--open")) {
        const count = if (args.len >= 4) try std.fmt.parseInt(usize, args[3], 10) else 10;
        return client(args[2], count);
    }
    std.debug.print("usage: echo --create NAME | --open NAME [COUNT]\n", .{});
    std.process.exit(2);
}

fn server(name: [:0]const u8) !void {
    var link = try hqq.Link.create(name, 4, 256, 0o600, null);
    defer link.deinit();

    var buf: [256]u8 = undefined;
    while (true) {
        const n = try link.read(&buf, hqq.Wait.forever);
        if (std.mem.eql(u8, buf[0..n], "END")) break;
        try link.write(buf[0..n], hqq.Wait.forever);
    }
    std.debug.print("server: END received\n", .{});
}

fn client(name: [:0]const u8, count: usize) !void {
    var link = try hqq.Link.open(name, 4, 256, null);
    defer link.deinit();

    var msg_buf: [64]u8 = undefined;
    var buf: [256]u8 = undefined;
    for (0..count) |i| {
        const msg = try std.fmt.bufPrint(&msg_buf, "message {d}", .{i});
        try link.write(msg, hqq.Wait.forever);
        const n = try link.read(&buf, hqq.Wait.forever);
        if (!std.mem.eql(u8, buf[0..n], msg)) return error.EchoMismatch;
    }
    try link.write("END", hqq.Wait.forever);
    std.debug.print("client: {d} messages echoed\n", .{count});
}
