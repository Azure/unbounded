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
use std::sync::Arc;
use std::sync::atomic::AtomicUsize;
use tokio::sync::watch;
use tokio::sync::{Semaphore, mpsc};

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

/// Bind the sidecar and diagnostics listeners for `run_workers`, with the
/// same addresses and error messages as `run`.
pub async fn bind(
    options: &cli::Options,
) -> Result<(std::net::TcpListener, Option<std::net::TcpListener>), String> {
    let debug = match &options.debug_listen {
        Some(address) => Some(
            debug::bind(address)
                .await
                .and_then(tokio::net::TcpListener::into_std)
                .map_err(|_| "could not bind debug listener".to_string())?,
        ),
        None => None,
    };
    let listener = tokio::net::TcpListener::bind(options.listen)
        .await
        .and_then(tokio::net::TcpListener::into_std)
        .map_err(|_| "could not bind sidecar listener".to_string())?;
    Ok((listener, debug))
}

/// Serve on already bound listeners (tests bind port 0 and read the address)
/// on the current runtime: a single worker.
pub async fn run_with(
    listener: tokio::net::TcpListener,
    debug: Option<tokio::net::TcpListener>,
    options: &cli::Options,
    socket: PathBuf,
    shutdown: watch::Receiver<bool>,
) {
    let client = racer::Client::new(socket, options.max_connections);
    let slots = Arc::new(Semaphore::new(options.max_connections));
    let source = server::Source::Listener {
        listener,
        peers: Vec::new(),
    };
    let active = Arc::new(AtomicUsize::new(0));
    serve_worker(source, debug, options, client, slots, active, shutdown).await;
}

async fn serve_worker(
    source: server::Source,
    debug: Option<tokio::net::TcpListener>,
    options: &cli::Options,
    client: racer::Client,
    slots: Arc<Semaphore>,
    active: Arc<AtomicUsize>,
    shutdown: watch::Receiver<bool>,
) {
    let debug_task = debug.map(|l| tokio::spawn(debug::serve(l, shutdown.clone())));
    server::serve(
        source,
        client,
        server::Config {
            api: options.api,
            request_timeout: options.request_timeout,
            slots,
            max_connections: options.max_connections,
        },
        active,
        shutdown,
    )
    .await;
    if let Some(task) = debug_task {
        task.abort();
    }
}

/// Serve with `options.workers` workers, each an OS thread running its own
/// single-threaded runtime, until `shutdown` turns true and every worker has
/// drained. All workers accept from the one shared `listener` (nonblocking);
/// the connection and Racer read limits are global. `runtime` becomes the
/// first worker on the calling thread: it also serves `debug` and runs
/// `setup` first (for example to install signal handlers).
pub fn run_workers(
    runtime: &tokio::runtime::Runtime,
    listener: std::net::TcpListener,
    debug: Option<std::net::TcpListener>,
    options: &cli::Options,
    socket: PathBuf,
    shutdown: watch::Receiver<bool>,
    setup: impl FnOnce() -> Result<(), String>,
) -> Result<(), String> {
    let start_failed = || "could not start workers".to_string();
    let client = racer::Client::new(socket, options.max_connections);
    let slots = Arc::new(Semaphore::new(options.max_connections));
    // Workers watch an internal channel so a failed start stops the rest.
    let (abort, stop) = watch::channel(false);
    let abort = Arc::new(abort);
    std::thread::scope(|scope| {
        let mut threads = Vec::with_capacity(options.workers.saturating_sub(1));
        let mut peers = Vec::with_capacity(options.workers.saturating_sub(1));
        for index in 1..options.workers {
            let (inbox, handoffs) = mpsc::unbounded_channel();
            let active = Arc::new(AtomicUsize::new(0));
            peers.push(server::Peer {
                inbox,
                active: active.clone(),
            });
            let client = client.sibling();
            let slots = slots.clone();
            let shutdown = stop.clone();
            let failed = abort.clone();
            let thread = std::thread::Builder::new()
                .name(format!("racer-object-{index}"))
                .spawn_scoped(scope, move || {
                    let Ok(runtime) = tokio::runtime::Builder::new_current_thread()
                        .enable_all()
                        .build()
                    else {
                        let _ = failed.send(true);
                        return Err(());
                    };
                    let source = server::Source::Inbox(handoffs);
                    runtime.block_on(serve_worker(
                        source, None, options, client, slots, active, shutdown,
                    ));
                    // Do not wait for straggling tasks past the grace period.
                    runtime.shutdown_background();
                    Ok(())
                });
            match thread {
                Ok(thread) => threads.push(thread),
                Err(_) => {
                    let _ = abort.send(true);
                    return Err(start_failed());
                }
            }
        }
        let first = runtime.block_on(async {
            setup()?;
            let mut shutdown = shutdown;
            let forward = abort.clone();
            tokio::spawn(async move {
                stopped(&mut shutdown).await;
                let _ = forward.send(true);
            });
            let listener =
                tokio::net::TcpListener::from_std(listener).map_err(|_| start_failed())?;
            let debug = match debug.map(tokio::net::TcpListener::from_std) {
                Some(Ok(l)) => Some(l),
                Some(Err(_)) => return Err(start_failed()),
                None => None,
            };
            let source = server::Source::Listener { listener, peers };
            let active = Arc::new(AtomicUsize::new(0));
            serve_worker(source, debug, options, client, slots, active, stop.clone()).await;
            Ok(())
        });
        let _ = abort.send(true);
        let mut result = first;
        for thread in threads {
            if !matches!(thread.join(), Ok(Ok(()))) && result.is_ok() {
                result = Err("worker failed".to_string());
            }
        }
        result
    })
}

/// Resolve once `shutdown` is true (or its sender is gone). The guard from
/// `wait_for` is dropped here so callers' futures stay `Send`.
pub async fn stopped(shutdown: &mut watch::Receiver<bool>) {
    let _ = shutdown.wait_for(|s| *s).await;
}
