//! Upload of written objects: holders send Put Block for their pages, the
//! coordinator drives them and commits the block list.

use super::OPERATION_PAGES;
use super::PageOutcome;
use super::WriteWork;
use super::coordinator::background_event;
use super::coordinator::background_scope;
use super::coordinator::page_id;
use super::send;
use crate::error::Error;
use crate::error::Result;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::origin::upload::Admission;
use crate::origin::upload::RetryPolicy;
use crate::peer::protocol::Operation as PeerOperation;
use crate::read::Coordinator;
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use crate::topology::Membership;
use racer_core::write::NodeIdBytes;
use racer_core::write::SessionEvent;
use racer_core::write::SessionId;
use racer_core::write::WriteReply;
use racer_wire::control::NodeId;
use std::sync::Arc;
use std::time::Duration;

/// Wait between upload limiter retries when every slot is busy.
const BUSY_WAIT: Duration = Duration::from_millis(10);

fn retry_policy(local: &Coordinator) -> RetryPolicy {
    RetryPolicy {
        attempts: local.writes.settings().upload_attempts.max(1),
        ..RetryPolicy::default()
    }
}

fn entropy(local: &Coordinator) -> u64 {
    let mut bytes = [0; 8];
    let _ = local
        .fill
        .dependencies
        .candidates
        .ctx()
        .rng
        .fill(&mut bytes);
    u64::from_le_bytes(bytes)
}

async fn sleep(local: &Coordinator, until: std::time::Instant, scope: &RequestScope) -> Result<()> {
    match local.writes.reactor() {
        Some(reactor) => reactor.sleep_until(until, scope).await,
        None => Err(Error::Unavailable),
    }
}

/// Serve `write-upload` on the object's metadata worker of a holder: each
/// page is uploaded on its owning worker. Replies `uploaded` with the pages
/// sent, or `failed` when none were.
pub async fn serve_upload(
    local: &Coordinator,
    version: ObjectVersion,
    session: SessionId,
    pages: Vec<u64>,
    context: &OriginContext,
    scope: &RequestScope,
) -> Result<WriteReply> {
    if !super::coordinator::writes_enabled(local, &version.object) {
        return Ok(WriteReply::Failed);
    }
    let directory = local.prefetch_directory();
    let mut uploaded = Vec::new();
    for &number in &pages {
        let page = page_id(&version, number);
        let sealed = directory.seal_for_worker(context, scope)?;
        let work = WriteWork::Upload {
            page: page.clone(),
            session,
            context: sealed,
        };
        if let Ok(PageOutcome::Done) = directory.write_page(&page, work, scope).await {
            uploaded.push(number);
        }
    }
    Ok(if uploaded.is_empty() {
        WriteReply::Failed
    } else {
        WriteReply::Uploaded { pages: uploaded }
    })
}

/// Upload one written page with Put Block on its owning worker, paced by
/// the cache's upload limiter and retried by the upload retry policy.
pub async fn upload_page(
    local: &Coordinator,
    page: &PageId,
    session: SessionId,
    context: &OriginContext,
    scope: &RequestScope,
) -> Result<PageOutcome> {
    let Some(sink) = local.writes.sink() else {
        return Ok(PageOutcome::Failed);
    };
    let writer = &local.fill.dependencies.writer;
    if writer
        .written(page)
        .is_none_or(|entry| entry.session != session)
    {
        return Ok(PageOutcome::Failed);
    }
    let Some(hex) = super::version_hex(&page.version.etag) else {
        return Ok(PageOutcome::Failed);
    };
    let block = racer_core::write::block_id(&hex, page.number.0);
    let result = local.fill.warm_local(page, scope).await?;
    let plaintext = result.plaintext.bytes();
    let cache = &page.version.object.cache;
    let definition = local.availability.writes(cache).ok_or(Error::Unavailable)?;
    let limiter = local.writes.limits().map(|limits| {
        limits.limiter(
            cache,
            definition.upload_max_bytes_per_second,
            definition.upload_max_concurrent_blocks,
        )
    });
    let _permit = match &limiter {
        None => None,
        Some(limiter) => loop {
            let clock = scope.clock().clone();
            match limiter.try_acquire(plaintext.len() as u64, clock.now()) {
                Admission::Ready(permit) => break Some(permit),
                Admission::Wait(until) => sleep(local, until, scope).await?,
                Admission::Busy => sleep(local, clock.now() + BUSY_WAIT, scope).await?,
            }
        },
    };
    retry_policy(local)
        .run(
            scope,
            entropy(local),
            |until| sleep(local, until, scope),
            |_| sink.put_block(context, &block, plaintext, scope),
        )
        .await?;
    local
        .fill
        .metrics
        .writes()
        .upload_bytes(&cache.0, plaintext.len() as u64);
    Ok(PageOutcome::Done)
}

/// Drive the upload of `pages` by `node` in the background on the
/// coordinator: send `write-upload` in batches, feeding `PageUploaded`
/// events, and fall back to the page's other holders when `node` fails.
pub fn spawn_upload(
    local: &Coordinator,
    object: &ObjectId,
    membership: &Arc<Membership>,
    node: NodeIdBytes,
    pages: Vec<u64>,
) {
    let Ok(local) = local.prefetch_directory().local_coordinator() else {
        return;
    };
    let Ok(permit) = local.drivers().reserve() else {
        return;
    };
    let object = object.clone();
    let membership = membership.clone();
    permit.submit_detached(async move {
        let scope = background_scope(&local)?;
        let Some((version, session, holders, context)) =
            local.writes.directory().with(&object, |e| {
                let e = e?;
                Some((
                    e.version.clone(),
                    e.session.identity().session,
                    e.session.holders().clone(),
                    e.context
                        .as_ref()
                        .and_then(|c| super::duplicate_context(c).ok()),
                ))
            })
        else {
            return Ok(());
        };
        let Some(context) = context else {
            // No credentials since a restart: the upload stays pending
            // until a client `persist` supplies them.
            return background_event(
                &local,
                &object,
                session,
                SessionEvent::UploadFailed,
                &membership,
                &scope,
            )
            .await;
        };
        let upload = |holder: NodeIdBytes, batch: Vec<u64>| {
            let operation = PeerOperation::WriteUpload {
                version: version.clone(),
                session,
                pages: batch,
            };
            let local = &local;
            let membership = &membership;
            let context = &context;
            let scope = &scope;
            async move {
                let target = NodeId(String::from_utf8(holder).map_err(|_| Error::Internal)?);
                match send(local, membership, &target, operation, context, scope).await {
                    Ok(WriteReply::Uploaded { pages }) => Ok(pages),
                    _ => Ok::<Vec<u64>, Error>(Vec::new()),
                }
            }
        };
        for chunk in pages.chunks(OPERATION_PAGES) {
            // The assigned holder takes the batch; a page it could not send
            // goes, alone, to each of its other holders in turn.
            let mut uploaded = upload(node.clone(), chunk.to_vec()).await?;
            for &page in chunk {
                if uploaded.contains(&page) {
                    continue;
                }
                for holder in holders.page(page).iter().filter(|h| **h != node) {
                    if upload(holder.clone(), vec![page]).await?.contains(&page) {
                        uploaded.push(page);
                        break;
                    }
                }
            }
            for &page in chunk {
                let event = if uploaded.contains(&page) {
                    SessionEvent::PageUploaded(page)
                } else {
                    SessionEvent::UploadFailed
                };
                let failed = matches!(event, SessionEvent::UploadFailed);
                background_event(&local, &object, session, event, &membership, &scope).await?;
                if failed {
                    return Ok(());
                }
            }
        }
        Ok::<(), Error>(())
    });
}

/// Commit the block list in the background once every page uploaded.
pub fn spawn_commit(local: &Coordinator, object: &ObjectId, membership: &Arc<Membership>) {
    let Ok(local) = local.prefetch_directory().local_coordinator() else {
        return;
    };
    let Ok(permit) = local.drivers().reserve() else {
        return;
    };
    let object = object.clone();
    let membership = membership.clone();
    permit.submit_detached(async move {
        let scope = background_scope(&local)?;
        let Some((version, session, pages, content_type, context)) =
            local.writes.directory().with(&object, |e| {
                let e = e?;
                Some((
                    e.version.clone(),
                    e.session.identity().session,
                    e.session.spec().pages(),
                    e.session.spec().content_type.clone(),
                    e.context
                        .as_ref()
                        .and_then(|c| super::duplicate_context(c).ok()),
                ))
            })
        else {
            return Ok(());
        };
        let event = match (
            local.writes.sink(),
            context,
            super::version_hex(&version.etag),
        ) {
            (Some(sink), Some(context), Some(hex)) => {
                let ids: Vec<[u8; 64]> = (0..pages)
                    .map(|p| racer_core::write::block_id(&hex, p))
                    .collect();
                let result = retry_policy(&local)
                    .run(
                        &scope,
                        entropy(&local),
                        |until| sleep(&local, until, &scope),
                        |_| sink.put_block_list(&context, &ids, content_type.as_deref(), &scope),
                    )
                    .await;
                match result {
                    Ok(Ok(etag)) => SessionEvent::BlockListCommitted {
                        blob_etag: String::from_utf8_lossy(etag.as_bytes()).into_owned(),
                    },
                    Ok(Err(_conflict)) => SessionEvent::BlockListConflict,
                    Err(_) => SessionEvent::UploadFailed,
                }
            }
            _ => SessionEvent::UploadFailed,
        };
        background_event(&local, &object, session, event, &membership, &scope).await
    });
}
