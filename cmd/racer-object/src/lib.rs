// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! racer-object: an S3 and Azure Blob compatible, read-only sidecar that
//! serves objects from a Racer cache over its local client socket.

pub mod cli;
pub mod debug;
pub mod http;
pub mod object;
pub mod racer;
pub mod server;
pub mod splice;

use std::path::PathBuf;
use tokio::sync::watch;

/// The Racer client socket for a cache.
pub fn socket_path(cache: &str) -> PathBuf {
    PathBuf::from(format!("/run/racer/{cache}/client/socket"))
}

/// Run the sidecar (and the optional diagnostics listener) until `shutdown`
/// turns true. `socket` is the Racer client socket.
pub async fn run(
    options: cli::Options,
    socket: PathBuf,
    shutdown: watch::Receiver<bool>,
) -> Result<(), String> {
    let debug = match &options.debug_listen {
        Some(address) => Some(
            debug::bind(address)
                .await
                .map_err(|_| "could not bind debug listener".to_string())?,
        ),
        None => None,
    };
    let listener = tokio::net::TcpListener::bind(options.listen)
        .await
        .map_err(|_| "could not bind sidecar listener".to_string())?;
    run_with(listener, debug, &options, socket, shutdown).await;
    Ok(())
}

/// Serve on already bound listeners (tests bind port 0 and read the address).
pub async fn run_with(
    listener: tokio::net::TcpListener,
    debug: Option<tokio::net::TcpListener>,
    options: &cli::Options,
    socket: PathBuf,
    shutdown: watch::Receiver<bool>,
) {
    let debug_task = debug.map(|l| tokio::spawn(debug::serve(l, shutdown.clone())));
    let client = racer::Client::new(socket, options.max_connections);
    server::serve(
        listener,
        client,
        server::Config {
            api: options.api,
            request_timeout: options.request_timeout,
            max_connections: options.max_connections,
        },
        shutdown,
    )
    .await;
    if let Some(task) = debug_task {
        task.abort();
    }
}

/// Resolve once `shutdown` is true (or its sender is gone). The guard from
/// `wait_for` is dropped here so callers' futures stay `Send`.
pub async fn stopped(shutdown: &mut watch::Receiver<bool>) {
    let _ = shutdown.wait_for(|s| *s).await;
}
