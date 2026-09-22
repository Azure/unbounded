// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::{env, path::PathBuf, process::Command};

fn run(command: &mut Command) {
    assert!(
        command.status().expect("run RDMA build tool").success(),
        "RDMA build failed (install rdma-core/libibverbs development headers)"
    );
}

fn main() {
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
    println!("cargo:rerun-if-env-changed=CC");
    println!("cargo:rerun-if-env-changed=AR");
    let out = PathBuf::from(env::var_os("OUT_DIR").unwrap());
    let object = out.join("rdma_verbs.o");
    run(
        Command::new(env::var_os("CC").unwrap_or_else(|| "cc".into()))
            .args([
                "-std=c11",
                "-O2",
                "-fPIC",
                "-Wall",
                "-Wextra",
                "-Werror",
                "-c",
                "src/rdma_verbs.c",
                "-o",
            ])
            .arg(&object),
    );
    run(
        Command::new(env::var_os("AR").unwrap_or_else(|| "ar".into()))
            .arg("crs")
            .arg(out.join("libracer_verbs.a"))
            .arg(object),
    );
    println!("cargo:rustc-link-search=native={}", out.display());
    println!("cargo:rustc-link-lib=static=racer_verbs");
    println!("cargo:rustc-link-lib=ibverbs");
    println!("cargo:rustc-link-lib=pthread");
}
