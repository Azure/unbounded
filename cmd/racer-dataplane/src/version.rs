// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Daemon build identity, independent of runtime setup.

use std::{ffi::OsString, io};

const IDENTITY: &str = concat!(
    env!("RACER_BUILD_VERSION"),
    " (commit: ",
    env!("RACER_BUILD_GIT_COMMIT"),
    ", built: ",
    env!("RACER_BUILD_BUILD_TIME"),
    ")"
);

/// Print the Go-compatible identity and return true when startup should stop.
pub fn print_requested(mut args: impl Iterator<Item = OsString>) -> io::Result<bool> {
    let Some(arg) = args.next() else {
        return Ok(false);
    };
    if (arg == "version" || arg == "--version" || arg == "-version") && args.next().is_none() {
        println!("{IDENTITY}");
        return Ok(true);
    }
    Err(io::Error::new(
        io::ErrorKind::InvalidInput,
        "expected no arguments, version, or --version",
    ))
}
