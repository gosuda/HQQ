//! Advanced RPC example (UNVERIFIED: not compiled, see build.zig).
//!
//!   rpc --create NAME    server: serves method 1 (upper-case of the request);
//!                        runs until the peer closes the link (Ctrl-C to stop)
//!   rpc --open NAME      client: calls method 1 with "hello" and prints the reply
const std = @import("std");
const hqq = @import("hqq");

const method_upper: u64 = 1;

const Upper = struct {
    fn handle(_: *Upper, req: *hqq.Request, resp: *hqq.Response) void {
        if (req.method() != method_upper) {
            resp.writeError(hqq.c.HQQ_ERRCODE_INVALID_OP, &.{}) catch {};
            return;
        }
        var out: [4096]u8 = undefined;
        const data = req.data();
        const n = @min(data.len, out.len);
        for (data[0..n], out[0..n]) |src, *dst| dst.* = std.ascii.toUpper(src);
        resp.write(out[0..n]) catch {};
    }
};

pub fn main() !void {
    var arena = std.heap.ArenaAllocator.init(std.heap.page_allocator);
    defer arena.deinit();
    const args = try std.process.argsAlloc(arena.allocator());

    if (args.len >= 3 and std.mem.eql(u8, args[1], "--create")) {
        var link = try hqq.AdvLink.create(args[2], 4, 256, 0o600, null);
        defer link.deinit();
        try link.negotiate(0, hqq.Wait.forever);
        var upper: Upper = .{};
        try link.serve(Upper, &upper, hqq.Wait.forever);
        return;
    }
    if (args.len >= 3 and std.mem.eql(u8, args[1], "--open")) {
        var link = try hqq.AdvLink.open(args[2], 4, 256, null);
        defer link.deinit();
        try link.negotiate(0, hqq.Wait.forever);
        var reply = try link.stream0().call(method_upper, "hello", hqq.Wait.forever);
        defer reply.deinit();
        std.debug.print("reply: {s}\n", .{reply.data()});
        return;
    }
    std.debug.print("usage: rpc --create NAME | --open NAME\n", .{});
    std.process.exit(2);
}
