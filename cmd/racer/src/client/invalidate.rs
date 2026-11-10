//! Serve `DELETE /v1/objects/{key}` on the client socket: drop one cached
//! version. Answered `204` once local state is dropped; the connection then
//! closes.

use super::Responses;
use crate::error::Result;
use crate::http::{ConnectionLease, HttpIo};
use crate::model::{ObjectId, ObjectVersion};
use crate::read::Coordinator;
use crate::runtime::Cancellation;
use http1::MessageHead;
use racer_wire::control::CacheId;
use std::time::Duration;

/// Whether a request head selects the invalidation route.
pub(super) fn is_invalidate(head: &MessageHead) -> bool {
    racer_wire::object::invalidate::is_invalidate(head)
}

#[allow(clippy::too_many_arguments)]
pub(super) async fn serve(
    mut connection: ConnectionLease,
    head: MessageHead,
    cache: &CacheId,
    header_limit: usize,
    reads: &Coordinator,
    responses: &Responses,
    io: &HttpIo,
    timeout: Duration,
    cancellation: Cancellation,
) -> Result<()> {
    let scope = super::listener::new_scope(io.reactor().ctx(), timeout, cancellation)?;
    let result = match racer_wire::object::invalidate::parse(&head, header_limit) {
        Err(error) => Err(error.into()),
        Ok(request) => {
            let version = ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: request.key,
                },
                etag: request.etag,
            };
            reads.invalidate(version, &scope).await
        }
    };
    if let Err(error) = result {
        responses.send_error(connection, error, &scope).await?;
        return Ok(());
    }
    scope.check()?;
    connection.poison();
    io.send_head(
        connection,
        racer_wire::object::invalidate::success_head(),
        &scope,
    )
    .await?;
    Ok(())
}
