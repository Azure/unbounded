// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use racer_object::cli::{self, Command};
use std::io::Write;
use std::process::ExitCode;
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::watch;

/// Worker threads cap. The sidecar is I/O bound and mostly moves data with
/// splice(2); a few threads saturate it, and fewer threads mean less
/// scheduling overhead and memory in every pod.
const MAX_WORKERS: usize = 4;

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
    let workers = std::thread::available_parallelism()
        .map_or(1, usize::from)
        .min(MAX_WORKERS);
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .worker_threads(workers)
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
