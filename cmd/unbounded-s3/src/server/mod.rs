// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

mod handlers;
mod range;
mod response;
mod router;

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use crate::catalog::{Catalog, YamlCatalog};
use crate::object::ObjectSource;

/// Hard deadline for graceful shutdown to complete after a signal.
/// Anything still in flight past this point is **aborted** (not
/// merely detached), so `run_server` is guaranteed to return within
/// this budget once a signal arrives. Without this bound, a single
/// wedged `read_page` (or long-poll consumer) would pin the daemon
/// in shutdown forever and only SIGKILL could escape.
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(30);

/// Start the S3-compatible HTTP server.
///
/// Binds `addr` and runs until either:
/// - the underlying `axum::serve` future returns (typically an
///   error, e.g. the accept loop dies), in which case the error
///   propagates immediately; or
/// - a shutdown signal arrives, in which case `run_server` performs
///   a graceful drain bounded by [`SHUTDOWN_TIMEOUT`] and aborts any
///   remaining in-flight connections past that point.
pub async fn run_server(
    addr: SocketAddr,
    catalog_path: Option<&std::path::Path>,
    source: Arc<dyn ObjectSource>,
) -> Result<(), Box<dyn std::error::Error>> {
    let catalog: Arc<dyn Catalog> = match catalog_path {
        Some(p) => Arc::new(YamlCatalog::load(p)?),
        None => Arc::new(YamlCatalog::empty()),
    };

    let app = router::build_router(catalog, source);
    let listener = tokio::net::TcpListener::bind(addr).await?;

    tracing::info!("listening on {}", addr);

    // Drive the server in a background task triggered by an internal
    // oneshot. Phase 1 races the server's own completion against the
    // shutdown signal, so a serve error (accept loop death, listener
    // closed under us) surfaces immediately rather than sitting in
    // the JoinHandle until SIGTERM. Phase 2 races the drain against
    // a bounded deadline; on timeout, the spawned task is explicitly
    // `abort()`ed and `await`ed, because dropping a `JoinHandle`
    // detaches the task rather than canceling it - using
    // `tokio::time::timeout(d, server)` would consume the handle and
    // leak the task past the deadline.
    let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel::<()>();
    let mut server = tokio::spawn(async move {
        axum::serve(listener, app)
            .with_graceful_shutdown(async {
                let _ = shutdown_rx.await;
            })
            .await
    });

    // Phase 1: serve errors before signal must propagate promptly.
    tokio::select! {
        res = &mut server => {
            return match res {
                Ok(Ok(())) => Ok(()),
                Ok(Err(e)) => Err(e.into()),
                Err(je) => Err(format!("server task panicked: {je}").into()),
            };
        }
        _ = shutdown_signal() => {}
    }

    // Phase 2: shutdown signal received; trigger drain, then bound it.
    tracing::info!("shutdown signal received; draining (up to {SHUTDOWN_TIMEOUT:?})");
    let _ = shutdown_tx.send(());

    let deadline = tokio::time::sleep(SHUTDOWN_TIMEOUT);
    tokio::pin!(deadline);
    tokio::select! {
        res = &mut server => {
            match res {
                Ok(Ok(())) => {}
                Ok(Err(e)) => return Err(e.into()),
                Err(je) => return Err(format!("server task panicked: {je}").into()),
            }
        }
        _ = &mut deadline => {
            tracing::error!(
                "graceful shutdown exceeded {SHUTDOWN_TIMEOUT:?}; aborting in-flight connections"
            );
            server.abort();
            // Wait for the task to actually wind down before
            // returning so we don't leak it past `run_server`.
            // Usually the result is `Err(JoinError::Cancelled)`
            // because we just aborted; but if the task raced to
            // completion just before `abort()` we still want any
            // serve error or panic in the log instead of dropping
            // it silently. `run_server` still returns `Ok(())` from
            // this branch - we've already committed to the timeout
            // error path - but the secondary log line preserves
            // observability into the race window.
            match server.await {
                Ok(Ok(())) => {}
                Ok(Err(e)) => {
                    tracing::error!(
                        error = %e,
                        "serve completed with error during shutdown timeout race",
                    );
                }
                Err(je) if je.is_cancelled() => {}
                Err(je) => {
                    tracing::error!(
                        panic = %je,
                        "server task panicked during shutdown timeout race",
                    );
                }
            }
        }
    }

    tracing::info!("server stopped");
    Ok(())
}

async fn shutdown_signal() {
    let ctrl_c = tokio::signal::ctrl_c();
    let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        .expect("install SIGTERM handler");

    tokio::select! {
        _ = ctrl_c => {},
        _ = term.recv() => {},
    }
}

#[cfg(test)]
mod tests;
