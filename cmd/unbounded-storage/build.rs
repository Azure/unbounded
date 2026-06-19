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

    generate_config_schema();

    println!("cargo:rerun-if-changed=src/fabric/shim.c");
    println!("cargo:rerun-if-changed=../../api/unbounded-storage/config.proto");
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rerun-if-env-changed=LIBFABRIC_PKG_CONFIG_PATH");
    println!("cargo:rerun-if-env-changed=PKG_CONFIG_PATH");
}

/// Generates the prost types for the daemon's config schema from
/// `../../api/unbounded-storage/config.proto`. That proto is the shared
/// schema source of truth (the supervisor's Go bindings are generated from
/// the same file). The generated code is `include!`d by
/// `src/config/schema.rs`.
///
/// Every message derives serde `Deserialize` with a container-level
/// `#[serde(default, deny_unknown_fields)]` so the daemon can still load a
/// partial TOML file: any omitted key falls back to the proto3 zero value,
/// which `Config::apply_defaults` then promotes to the documented default.
/// `deny_unknown_fields` makes the TOML loader reject typo'd keys loudly at
/// parse time; this is the serde/TOML path only - decoding a protobuf wire
/// message keeps protobuf's forward-compatible unknown-field semantics.
/// Oneofs deserialize as tagged TOML tables, and byte sizes as plain integers.
fn generate_config_schema() {
    // Use the vendored protoc so no system protoc install is required.
    let protoc =
        protoc_bin_vendored::protoc_bin_path().expect("vendored protoc binary is unavailable");
    // SAFETY: build scripts run single-threaded before any Rust code in
    // this crate executes; setting an env var here is standard practice.
    unsafe {
        env::set_var("PROTOC", protoc);
    }

    let mut prost = prost_build::Config::new();

    for msg in [
        "Config",
        "RoutingPlan",
        "PeerSpec",
        "TcpPeerConfig",
        "RdmaPeerConfig",
        "DiskSpec",
        "BlockDiskConfig",
        "FileDiskConfig",
        "CacheSpec",
        "NeighborhoodSpec",
        "BackendSpec",
        "HttpBackendConfig",
        "S3BackendConfig",
        "AzureBackendConfig",
        "FakeBackendConfig",
        "FrontendSpec",
        "HttpFrontendConfig",
        "S3FrontendConfig",
        "LoadgenFrontendConfig",
        "StartupCfg",
        "MemoryCfg",
        "FabricCfg",
        "TcpFabricBinds",
        "RdmaFabricBinds",
        "RdmaFabricBind",
        "TopologyCfg",
        "MetricsCfg",
    ] {
        prost.type_attribute(msg, "#[derive(::serde::Deserialize)]");
        prost.type_attribute(msg, "#[serde(default, deny_unknown_fields)]");
    }

    for oneof in [
        "PeerSpec.config",
        "FabricCfg.binds",
        "DiskSpec.config",
        "BackendSpec.config",
        "FrontendSpec.config",
    ] {
        prost.type_attribute(oneof, "#[derive(::serde::Deserialize)]");
        prost.type_attribute(oneof, "#[serde(rename_all = \"lowercase\")]");
    }

    prost
        .compile_protos(
            &["../../api/unbounded-storage/config.proto"],
            &["../../api/unbounded-storage"],
        )
        .expect("prost-build failed to compile api/unbounded-storage/config.proto");
}
