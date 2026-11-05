//! Serve `/v1/prefetches` on the client socket. Each request gets a JSON
//! response and the connection then closes.

use crate::error::Result;
use crate::http::{ConnectionLease, HttpIo};
use crate::read::Coordinator;
use crate::runtime::Cancellation;
use http1::MessageHead;
use racer_io::ring::IoBuffer;
use racer_wire::control::CacheId;
use racer_wire::object::prefetch::PrefetchRequest;
use std::task::Poll;
use std::time::Duration;

/// Scope for a PUT, which resolves versions before answering.
const PUT_TIMEOUT: Duration = Duration::from_secs(70);

/// Extra time a `?wait` request gets past its wait.
const WAIT_SLACK: Duration = Duration::from_secs(5);

/// Whether a request head selects the prefetch route.
pub(super) fn is_prefetch(head: &MessageHead) -> bool {
    matches!(
        &head.start,
        http1::StartLine::Request { target, .. }
            if racer_wire::object::prefetch::is_prefetch_target(target)
    )
}

#[allow(clippy::too_many_arguments)]
pub(super) async fn serve(
    mut connection: ConnectionLease,
    head: MessageHead,
    cache: &CacheId,
    header_limit: usize,
    reads: &Coordinator,
    io: &HttpIo,
    timeout: Duration,
    cancellation: Cancellation,
) -> Result<()> {
    let limits = reads.prefetch_directory().prefetch.config().limits();
    let parsed = racer_wire::object::prefetch::parse(&head, &limits, header_limit);
    let budget = match &parsed {
        Ok(PrefetchRequest::Get { query, .. }) => {
            timeout.max(query.wait.unwrap_or_default() + WAIT_SLACK)
        }
        Ok(PrefetchRequest::Put { .. }) => timeout.max(PUT_TIMEOUT),
        _ => timeout,
    };
    let scope = super::listener::new_scope(io.reactor().ctx(), budget, cancellation)?;
    let response = match parsed {
        Err(error) => crate::prefetch::service::error(error),
        Ok(request) => {
            // Keep the admitted buffer, and its charge, alive until the
            // handler finishes instead of copying into uncharged memory.
            let mut owned = None;
            if let PrefetchRequest::Put { content_length, .. } = &request {
                let length = usize::try_from(*content_length)
                    .map_err(|_| crate::error::Error::InvalidRequest)?;
                let buffer = io.buffer(length)?;
                let completed = io.read_body_exact(connection, buffer, &scope).await?;
                connection = completed.lease;
                owned = Some(completed.buffer);
            }
            let body: &[u8] = match &owned {
                Some(buffer) => buffer.bytes()?,
                None => &[],
            };
            // A client disconnect ends a long-poll wait.
            let watch = scope.child(scope.request, scope.deadline.0)?;
            let socket = connection.socket();
            let mut disconnected = Some(io.disconnected(&connection, &watch));
            let mut handled = Box::pin(crate::prefetch::service::handle(
                reads, cache, request, body, &scope,
            ));
            let response = std::future::poll_fn(|cx| {
                if let Some(watcher) = disconnected.as_mut()
                    && let Poll::Ready(result) = watcher.as_mut().poll(cx)
                {
                    disconnected = None;
                    if result.is_ok_and(|events| events & libc::POLLHUP as u32 != 0) {
                        let _ = scope.cancel();
                    }
                }
                if socket.peer_disconnected() {
                    let _ = scope.cancel();
                }
                handled.as_mut().poll(cx)
            })
            .await;
            let _ = watch.cancel();
            if let Some(watcher) = disconnected.take() {
                let _ = watcher.await;
            }
            drop(handled);
            drop(owned);
            response
        }
    };
    if scope.check().is_err() {
        // The client went away or the request expired; nothing to answer.
        return Ok(());
    }
    connection.poison();
    let sent = io.send_head(connection, response.head, &scope).await?;
    let _ = io
        .write_body_bytes(sent.connection, &response.body, &scope)
        .await?;
    Ok(())
}
