// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Probes the C libraries this crate links against. We compile the
//! `fabric` module's shim against the headers reported by pkg-config
//! for `libfabric`.

use std::env;

fn prepend_pkg_config_path(extra_var: &str) {
    if let Ok(extra) = env::var(extra_var) {
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
}

fn main() {
    prepend_pkg_config_path("LIBFABRIC_PKG_CONFIG_PATH");

    let libfabric = pkg_config::Config::new()
        .probe("libfabric")
        .expect("pkg-config could not locate `libfabric` (try setting LIBFABRIC_PKG_CONFIG_PATH)");

    let mut fabric_build = cc::Build::new();
    fabric_build.file("src/fabric/shim.c");
    for p in &libfabric.include_paths {
        fabric_build.include(p);
    }
    fabric_build.warnings(true).compile("unbounded_fabric_shim");

    println!("cargo:rerun-if-changed=src/fabric/shim.c");
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rerun-if-env-changed=LIBFABRIC_PKG_CONFIG_PATH");
    println!("cargo:rerun-if-env-changed=PKG_CONFIG_PATH");
}
