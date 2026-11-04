// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use racer_object::cli::{self, Command};
use std::io::Write;
use std::process::ExitCode;
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::watch;

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let options = match cli::parse(&args) {
        Ok(Command::Print(text)) => {
            let mut out = std::io::stdout().lock();
            if out
                .write_all(text.as_bytes())
                .and_then(|()| out.flush())
                .is_err()
            {
                eprintln!("racer-object: could not write CLI output");
                return ExitCode::FAILURE;
            }
            return ExitCode::SUCCESS;
        }
        Ok(Command::Run(options)) => options,
        Err(message) => {
            eprintln!("racer-object: {message}");
            return ExitCode::FAILURE;
        }
    };
    // A single-threaded runtime. The sidecar is syscall bound and moves
    // large bodies with splice(2), so one thread already matches the Go
    // sidecar's throughput, while extra worker threads measurably raised
    // CPU per request and per byte (work stealing and cross-thread wakeups).
    // See README.md for the measurements.
    let runtime = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(_) => {
            eprintln!("racer-object: could not start runtime");
            return ExitCode::FAILURE;
        }
    };
    let result = runtime.block_on(async {
        let (stop, shutdown) = watch::channel(false);
        let (Ok(mut term), Ok(mut int)) = (
            signal(SignalKind::terminate()),
            signal(SignalKind::interrupt()),
        ) else {
            return Err("could not install signal handlers".to_string());
        };
        tokio::spawn(async move {
            tokio::select! {
                _ = term.recv() => {}
                _ = int.recv() => {}
            }
            let _ = stop.send(true);
        });
        let socket = racer_object::socket_path(&options.cache);
        racer_object::run(options, socket, shutdown).await
    });
    // Do not wait for straggling tasks past the shutdown grace period.
    runtime.shutdown_background();
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(message) => {
            eprintln!("racer-object: {message}");
            ExitCode::FAILURE
        }
    }
}
