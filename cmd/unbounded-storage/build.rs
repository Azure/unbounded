// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Locates Mercury via `pkg-config` and compiles the small C shim that
//! exposes Mercury's `static HG_INLINE` proc helpers as real symbols
//! our Rust FFI can call. The shim is the only C we write; everything
//! else is plain Rust `extern "C"`.

use std::env;
use std::path::PathBuf;

fn main() {
    // Allow developers to point at a local mercury install (e.g. the
    // in-tree `tmp/mercury-prefix/`) without touching system pkg-config
    // paths.
    if let Ok(extra) = env::var("MERCURY_PKG_CONFIG_PATH") {
        let cur = env::var("PKG_CONFIG_PATH").unwrap_or_default();
        let combined = if cur.is_empty() {
            extra
        } else {
            format!("{extra}:{cur}")
        };
        // SAFETY: build scripts run single-threaded before any Rust code
        // in this crate links against libc; setting an env var here is
        // standard practice in Cargo build scripts.
        unsafe {
            env::set_var("PKG_CONFIG_PATH", combined);
        }
    }

    let mercury = pkg_config::Config::new()
        .probe("mercury")
        .expect("pkg-config could not locate `mercury` (try setting MERCURY_PKG_CONFIG_PATH)");
    pkg_config::Config::new()
        .probe("na")
        .expect("pkg-config could not locate `na` (Mercury Network Abstraction)");

    let include_paths: Vec<PathBuf> = mercury.include_paths.clone();

    let mut build = cc::Build::new();
    build.file("src/mercury/shim.c");
    for p in &include_paths {
        build.include(p);
    }
    build.warnings(true).compile("unbounded_mercury_shim");

    println!("cargo:rerun-if-changed=src/mercury/shim.c");
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rerun-if-env-changed=MERCURY_PKG_CONFIG_PATH");
    println!("cargo:rerun-if-env-changed=PKG_CONFIG_PATH");
}
