// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Rebuild when version metadata changes; src/cli.rs reads it with option_env!.
fn main() {
    for name in [
        "RACER_OBJECT_VERSION",
        "RACER_OBJECT_GIT_COMMIT",
        "RACER_OBJECT_BUILD_TIME",
    ] {
        println!("cargo:rerun-if-env-changed={name}");
    }
}
