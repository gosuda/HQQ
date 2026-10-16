//! Builds libhqq from the in-tree C sources and links it statically.
//!
//! No crates.io dependencies and no network access: the build invokes the C
//! compiler and archiver directly with std::process::Command.
//!
//! * The source list is read from `c/CMakeLists.txt` (the `HQQ_SOURCES` block),
//!   so the Rust build cannot drift from the CMake build. The platform file is
//!   chosen from the target OS (`hqq_platform_posix.c` or `hqq_platform_win32.c`).
//! * Unix-like targets (GCC, Clang, MinGW): `$HQQ_CC` / `$CC` (default `cc`) and
//!   `$HQQ_AR` / `$AR` (default `ar`).
//! * `*-msvc` targets: `cl.exe` and `lib.exe` (C11 atomics need VS 17.5+ and
//!   `/experimental:c11atomics`, as in c/CMakeLists.txt).
//! * `HQQ_C_SOURCE_DIR` overrides the location of the C tree (default
//!   `../../../c` relative to this crate, i.e. the repository's `c/` directory).
use std::env;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

fn main() {
    println!("cargo:rerun-if-env-changed=HQQ_C_SOURCE_DIR");
    println!("cargo:rerun-if-env-changed=HQQ_CC");
    println!("cargo:rerun-if-env-changed=HQQ_AR");
    println!("cargo:rerun-if-env-changed=CC");
    println!("cargo:rerun-if-env-changed=AR");

    let manifest_dir = PathBuf::from(env_var("CARGO_MANIFEST_DIR"));
    let c_root = match env::var_os("HQQ_C_SOURCE_DIR") {
        Some(dir) => PathBuf::from(dir),
        None => manifest_dir.join("..").join("..").join("..").join("c"),
    };
    let c_root = c_root
        .canonicalize()
        .unwrap_or_else(|e| panic!("hqq-sys: C source tree {}: {e}", c_root.display()));

    let out_dir = PathBuf::from(env_var("OUT_DIR"));
    let target_os = env_var("CARGO_CFG_TARGET_OS");
    let target_env = env::var("CARGO_CFG_TARGET_ENV").unwrap_or_default();
    let optimized = env::var("OPT_LEVEL").map(|v| v != "0").unwrap_or(true);
    let windows = target_os == "windows";

    let include_dir = c_root.join("include");
    let src_dir = c_root.join("src");
    watch_tree(&c_root.join("include"));
    watch_tree(&src_dir);
    println!(
        "cargo:rerun-if-changed={}",
        c_root.join("CMakeLists.txt").display()
    );

    let sources = library_sources(&c_root, windows);
    let objects: Vec<PathBuf> = sources
        .iter()
        .map(|rel| {
            let src = c_root.join(rel);
            let stem = Path::new(rel).file_stem().expect("source name");
            let obj_name = if target_env == "msvc" {
                format!("{}.obj", stem.to_string_lossy())
            } else {
                format!("{}.o", stem.to_string_lossy())
            };
            let obj = out_dir.join(obj_name);
            if target_env == "msvc" {
                compile_msvc(&src, &include_dir, &src_dir, &obj, optimized);
            } else {
                compile_cc(&src, &include_dir, &src_dir, &obj, optimized);
            }
            obj
        })
        .collect();

    if target_env == "msvc" {
        let lib = out_dir.join("hqq.lib");
        let mut cmd = Command::new(tool("HQQ_AR", "AR", "lib"));
        cmd.arg("/nologo").arg(format!("/OUT:{}", lib.display()));
        cmd.args(&objects);
        run(cmd, "lib.exe");
        println!("cargo:rustc-link-lib=static=hqq");
        println!("cargo:rustc-link-lib=advapi32");
        println!("cargo:rustc-link-lib=kernel32");
    } else {
        let lib = out_dir.join("libhqq.a");
        let _ = fs::remove_file(&lib);
        let mut cmd = Command::new(tool("HQQ_AR", "AR", "ar"));
        cmd.arg("rcs").arg(&lib);
        cmd.args(&objects);
        run(cmd, "ar");
        println!("cargo:rustc-link-lib=static=hqq");
        match target_os.as_str() {
            "windows" => {
                println!("cargo:rustc-link-lib=advapi32");
                println!("cargo:rustc-link-lib=kernel32");
            }
            "linux" | "android" => {
                println!("cargo:rustc-link-lib=pthread");
                println!("cargo:rustc-link-lib=rt");
            }
            _ => println!("cargo:rustc-link-lib=pthread"),
        }
    }
    println!("cargo:rustc-link-search=native={}", out_dir.display());
}

fn env_var(name: &str) -> String {
    env::var(name).unwrap_or_else(|_| panic!("hqq-sys: {name} is not set (run through cargo)"))
}

fn tool(override_var: &str, fallback_var: &str, default: &str) -> String {
    env::var(override_var)
        .or_else(|_| env::var(fallback_var))
        .unwrap_or_else(|_| default.to_string())
}

fn run(mut cmd: Command, what: &str) {
    let output = cmd
        .output()
        .unwrap_or_else(|e| panic!("hqq-sys: cannot run {what}: {e}"));
    if !output.status.success() {
        panic!(
            "hqq-sys: {what} failed ({}):\n{}\n{}",
            output.status,
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
    }
}

/// The `HQQ_SOURCES` list of c/CMakeLists.txt, with the platform file of the target.
fn library_sources(c_root: &Path, windows: bool) -> Vec<String> {
    let cmake = fs::read_to_string(c_root.join("CMakeLists.txt"))
        .unwrap_or_else(|e| panic!("hqq-sys: reading CMakeLists.txt: {e}"));
    let mut sources = Vec::new();
    let mut in_block = false;
    for line in cmake.lines() {
        let line = line.trim();
        let body = if in_block {
            line
        } else if let Some(rest) = line.strip_prefix("set(HQQ_SOURCES") {
            in_block = true;
            rest
        } else {
            continue;
        };
        for token in body.split(|c: char| c.is_whitespace() || c == '(' || c == ')') {
            if token.starts_with("src/") && token.ends_with(".c") {
                sources.push(token.to_string());
            }
        }
        if body.contains(')') {
            break;
        }
    }
    sources.retain(|s| !s.contains("platform"));
    sources.push(
        if windows {
            "src/hqq_platform_win32.c"
        } else {
            "src/hqq_platform_posix.c"
        }
        .to_string(),
    );
    assert!(
        sources.len() >= 12,
        "hqq-sys: too few sources in CMakeLists.txt"
    );
    for s in &sources {
        println!("cargo:rerun-if-changed={}", c_root.join(s).display());
    }
    sources
}

fn compile_cc(src: &Path, include: &Path, src_dir: &Path, obj: &Path, optimized: bool) {
    let mut cmd = Command::new(tool("HQQ_CC", "CC", "cc"));
    cmd.arg("-std=c11")
        .arg("-fPIC")
        .arg("-Wall")
        .arg("-Wextra")
        .arg("-Wpedantic")
        .arg(if optimized { "-O2" } else { "-O0" })
        .arg("-g")
        .arg("-I")
        .arg(include)
        .arg("-I")
        .arg(src_dir)
        .arg("-c")
        .arg(src)
        .arg("-o")
        .arg(obj);
    run(cmd, "C compiler");
}

fn compile_msvc(src: &Path, include: &Path, src_dir: &Path, obj: &Path, optimized: bool) {
    let mut cmd = Command::new(tool("HQQ_CC", "CC", "cl"));
    cmd.arg("/nologo")
        .arg("/c")
        .arg("/std:c11")
        .arg("/experimental:c11atomics")
        .arg("/DHQQ_FORCE_C11_ATOMICS=1")
        .arg("/W4")
        .arg(if optimized { "/O2" } else { "/Od" })
        .arg(format!("/I{}", include.display()))
        .arg(format!("/I{}", src_dir.display()))
        .arg(src)
        .arg(format!("/Fo{}", obj.display()));
    run(cmd, "cl.exe");
}

/// Reruns the build script when any file below `dir` changes.
fn watch_tree(dir: &Path) {
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        if path.is_dir() {
            watch_tree(&path);
        } else {
            println!("cargo:rerun-if-changed={}", path.display());
        }
    }
}
