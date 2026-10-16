// Build script for the Zig binding of libhqq.
//
// Target Zig version: 0.15.x (stable build API: addExecutable/addLibrary with
// root_module, Module.addCSourceFiles, Module.addIncludePath).
// UNVERIFIED: this file has not been compiled (Zig is not installed in the
// authoring environment).
//
// libhqq is compiled directly from the in-tree C sources (no CMake). Sources
// and flags follow c/CMakeLists.txt (HQQ_SOURCES) and
// c/cmake/HqqCompilerOptions.cmake. The C root defaults to ../../c and can be
// overridden with -Dhqq-root=<path>.
//
// Steps:
//   zig build              install libhqq, echo, rpc, hqq-interop
//   zig build test         unit tests of src/hqq.zig (includes in-process tests)
//   zig build run-echo     -- --create NAME | --open NAME [COUNT]
//   zig build run-rpc      -- --create NAME | --open NAME
const std = @import("std");

const shared_sources = [_][]const u8{
    "hqq_util.c",
    "hqq_names.c",
    "hqq_superblock.c",
    "hqq_link.c",
    "hqq_named.c",
    "hqq_adv_core.c",
    "hqq_adv_rx.c",
    "hqq_adv_route.c",
    "hqq_adv_link.c",
    "hqq_adv_stream.c",
    "hqq_adv_msg.c",
    "hqq_listener.c",
};

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});

    const hqq_root: std.Build.LazyPath = if (b.option([]const u8, "hqq-root", "Path to the libhqq C root (contains include/ and src/)")) |p|
        .{ .cwd_relative = p }
    else
        b.path("../../c");
    const include_dir = hqq_root.path(b, "include");
    const src_dir = hqq_root.path(b, "src");

    const os = target.result.os.tag;
    const c_flags = [_][]const u8{ "-std=c11", "-fPIC", "-Wall", "-Wextra", "-Wpedantic" };

    // libhqq (static), compiled from the C sources.
    const lib_mod = b.createModule(.{
        .target = target,
        .optimize = optimize,
        .link_libc = true,
    });
    lib_mod.addIncludePath(include_dir);
    lib_mod.addIncludePath(src_dir);
    var sources: std.ArrayList([]const u8) = .empty;
    defer sources.deinit(b.allocator);
    for (shared_sources) |s| sources.append(b.allocator, s) catch @panic("OOM");
    sources.append(b.allocator, if (os == .windows) "hqq_platform_win32.c" else "hqq_platform_posix.c") catch @panic("OOM");
    lib_mod.addCSourceFiles(.{
        .root = src_dir,
        .files = sources.items,
        .flags = &c_flags,
    });
    switch (os) {
        .windows => {
            lib_mod.linkSystemLibrary("kernel32", .{});
            lib_mod.linkSystemLibrary("advapi32", .{});
        },
        .linux => {
            lib_mod.linkSystemLibrary("rt", .{});
            lib_mod.linkSystemLibrary("pthread", .{});
        },
        else => {},
    }
    const libhqq = b.addLibrary(.{
        .linkage = .static,
        .name = "hqq",
        .root_module = lib_mod,
    });
    b.installArtifact(libhqq);

    // Zig module `hqq`: the wrapper over the C API.
    const hqq_mod = b.addModule("hqq", .{
        .root_source_file = b.path("src/hqq.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
    });
    hqq_mod.addIncludePath(include_dir);
    hqq_mod.linkLibrary(libhqq);

    // Unit tests of src/hqq.zig.
    const tests = b.addTest(.{ .root_module = hqq_mod });
    const run_tests = b.addRunArtifact(tests);
    b.step("test", "Run the unit tests of the hqq module").dependOn(&run_tests.step);

    // Examples and the interop helper.
    const echo = addExe(b, "echo", "examples/echo.zig", hqq_mod, target, optimize);
    const rpc = addExe(b, "rpc", "examples/rpc.zig", hqq_mod, target, optimize);
    const interop = addExe(b, "hqq-interop", "interop/hqq_interop.zig", hqq_mod, target, optimize);
    b.installArtifact(echo);
    b.installArtifact(rpc);
    b.installArtifact(interop);

    inline for (.{ .{ "run-echo", echo }, .{ "run-rpc", rpc } }) |entry| {
        const run = b.addRunArtifact(entry[1]);
        if (b.args) |args| run.addArgs(args);
        b.step(entry[0], "Run the " ++ entry[0][4..] ++ " example (pass arguments after --)").dependOn(&run.step);
    }
}

fn addExe(
    b: *std.Build,
    name: []const u8,
    root: []const u8,
    hqq_mod: *std.Build.Module,
    target: std.Build.ResolvedTarget,
    optimize: std.builtin.OptimizeMode,
) *std.Build.Step.Compile {
    const mod = b.createModule(.{
        .root_source_file = b.path(root),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .imports = &.{.{ .name = "hqq", .module = hqq_mod }},
    });
    return b.addExecutable(.{ .name = name, .root_module = mod });
}
