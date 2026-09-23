// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::{env, path::PathBuf};

fn main() {
    let openssl = pkg_config::Config::new()
        .atleast_version("3.0.0")
        .probe("openssl")
        .expect("OpenSSL 3 development headers and libraries are required");
    println!("cargo:rerun-if-changed=src/tls_native.c");
    cc::Build::new()
        .file("src/tls_native.c")
        .includes(openssl.include_paths)
        .flag_if_supported("-std=c11")
        .warnings(true)
        .warnings_into_errors(true)
        .compile("racer_tls");
    // Match internal/version's unstamped Go defaults. Track each input so a
    // cached Cargo build cannot retain metadata from a previous release.
    for (name, default) in [
        ("VERSION", "dev"),
        ("GIT_COMMIT", "unknown"),
        ("BUILD_TIME", "unknown"),
    ] {
        println!("cargo:rerun-if-env-changed={name}");
        let value = env::var(name).unwrap_or_else(|_| default.into());
        let value = if value.is_empty() { default } else { &value };
        assert!(
            !value.contains(['\r', '\n']),
            "{name} must be a single line"
        );
        println!("cargo:rustc-env=RACER_BUILD_{name}={value}");
    }

    println!("cargo:rerun-if-changed=../../api/racer/control.proto");
    let proto_out = PathBuf::from(env::var_os("OUT_DIR").unwrap());
    let descriptor = proto_out.join("control.bin");
    prost_build::Config::new()
        .protoc_executable(protoc_bin_vendored::protoc_bin_path().unwrap())
        .file_descriptor_set_path(&descriptor)
        .compile_protos(&["../../api/racer/control.proto"], &["../../api/racer"])
        .expect("compile control-plane protobuf");
    pbjson_build::Builder::new()
        .register_descriptors(&std::fs::read(descriptor).unwrap())
        .unwrap()
        .build(&[".racer.control.v1"])
        .expect("generate ProtoJSON codec");
    println!("cargo:rerun-if-changed=src/rdma_verbs.c");
    // Keep the verbs ABI firewall optimized in every Rust profile. cc supplies
    // target-aware compiler/archive selection and tracks native build inputs.
    cc::Build::new()
        .file("src/rdma_verbs.c")
        .std("c11")
        .opt_level(2)
        .pic(true)
        .warnings(true)
        .extra_warnings(true)
        .warnings_into_errors(true)
        .compile("racer_verbs");
    println!("cargo:rustc-link-lib=ibverbs");
    println!("cargo:rustc-link-lib=pthread");
}
