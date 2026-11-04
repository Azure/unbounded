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
    // Each worker is an OS thread with its own single-threaded runtime and
    // Racer connection pool; workers never steal work from each other. The
    // calling thread is the first worker and accepts connections, handing
    // each to the worker serving the fewest. A work-stealing runtime raised
    // CPU per request; separate runtimes keep the single-thread cost per
    // request while adding throughput. With one worker this is a plain
    // current_thread runtime. See README.md for the measurements.
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
    let socket = racer_object::socket_path(&options.cache);
    let (stop, shutdown) = watch::channel(false);
    let on_signal = move || {
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
        Ok(())
    };
    let result = if options.workers == 1 {
        runtime.block_on(async {
            on_signal()?;
            racer_object::run(options, socket, shutdown).await
        })
    } else {
        runtime
            .block_on(racer_object::bind(&options))
            .and_then(|(listener, debug)| {
                racer_object::run_workers(
                    &runtime, listener, debug, &options, socket, shutdown, on_signal,
                )
            })
    };
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
