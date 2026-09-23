// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let schema = "../../api/racer/control.proto";
    println!("cargo:rerun-if-changed={schema}");
    prost_build::Config::new()
        .protoc_executable(protoc_bin_vendored::protoc_bin_path()?)
        .type_attribute(".", "#[derive(serde::Serialize, serde::Deserialize)]")
        .compile_protos(&[schema], &["../../api/racer"])?;
    Ok(())
}
