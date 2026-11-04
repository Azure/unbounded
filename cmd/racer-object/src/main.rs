// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use racer_object::cli::{self, Command};
use std::io::Write;
use std::process::ExitCode;

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match cli::parse(&args) {
        Ok(Command::Print(text)) => {
            let mut out = std::io::stdout().lock();
            if out.write_all(text.as_bytes()).and_then(|()| out.flush()).is_err() {
                eprintln!("racer-object: could not write CLI output");
                return ExitCode::FAILURE;
            }
            ExitCode::SUCCESS
        }
        Ok(Command::Run(_)) => {
            eprintln!("racer-object: sidecar not implemented yet");
            ExitCode::FAILURE
        }
        Err(message) => {
            eprintln!("racer-object: {message}");
            ExitCode::FAILURE
        }
    }
}
