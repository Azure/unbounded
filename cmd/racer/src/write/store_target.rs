//! Store targets: pull a written page from its source, verify it, and make
//! it durable; release or drop written pages.

use super::PageOutcome;
use super::WriteWork;
use super::coordinator::page_id;
use super::coordinator::writes_enabled;
use crate::error::Error;
use crate::error::Result;
use crate::memory::CiphertextCopy;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::peer::protocol::FetchMode;
use crate::peer::protocol::Operation as PeerOperation;
use crate::peer::protocol::PeerResponse;
use crate::peer::protocol::WriteStore;
use crate::read::Coordinator;
use crate::runtime::RequestScope;
use crate::topology::Membership;
use racer_core::write::SessionId;
use racer_core::write::WriteReply;
use std::sync::Arc;

/// Retries of a store refused for a transient reason.
const TRANSIENT_RETRIES: u32 = 16;

/// Base wait between those retries; it doubles per attempt up to
/// [`TRANSIENT_WAIT_MAX`], about 3 s in total. A checkpoint freeze or a 16 MiB
/// staging reservation can outlast the 180 ms the first build allowed, which
/// the kind e2e saw as a `503 dirty-cap` far below the cap.
const TRANSIENT_WAIT: std::time::Duration = std::time::Duration::from_millis(5);

/// Longest single wait between transient retries.
const TRANSIENT_WAIT_MAX: std::time::Duration = std::time::Duration::from_millis(250);

/// Serve `write-store` on the object's metadata worker: each page is stored
/// on its owning worker. Replies `stored` with the pages made durable, or
/// `full` when none were and a target was over its dirty cap.
pub async fn serve_store(
    local: &Coordinator,
    version: ObjectVersion,
    store: WriteStore,
    membership: Arc<Membership>,
    scope: &RequestScope,
) -> Result<WriteReply> {
    if !writes_enabled(local, &version.object)
        || store.page_size != local.page_size()?
        || local.writes.directory().tombstoned(&version)
    {
        // A deleted version takes no new copies (a repair or rebalance that
        // raced the delete).
        return Ok(WriteReply::Failed);
    }
    let directory = local.prefetch_directory();
    let outcomes = futures::future::join_all(store.pages.iter().map(|&number| {
        let page = page_id(&version, number);
        let work = WriteWork::Store {
            page: page.clone(),
            store: store.clone(),
            membership: membership.clone(),
        };
        async move { (number, directory.write_page(&page, work, scope).await) }
    }))
    .await;
    let mut stored = Vec::new();
    let mut full = false;
    for (number, outcome) in outcomes {
        match outcome {
            Ok(PageOutcome::Done) => stored.push(number),
            Ok(PageOutcome::Full) | Err(Error::Overloaded) => full = true,
            _ => {}
        }
    }
    Ok(if !stored.is_empty() {
        WriteReply::Stored { pages: stored }
    } else if full {
        WriteReply::Full
    } else {
        WriteReply::Failed
    })
}

/// Pull `page` from `store.source` (or this node's staging), verify its
/// envelope against the version's metadata and its AEAD tag, and store it
/// durably until `store.hold_until_millis`. Runs on the page's owning worker.
pub async fn store_page(
    local: &Coordinator,
    page: PageId,
    store: &WriteStore,
    membership: Arc<Membership>,
    scope: &RequestScope,
) -> Result<PageOutcome> {
    scope.check()?;
    let writer = &local.fill.dependencies.writer;
    let staged = local.prefetch_directory().write_staging.get(&page);
    let (copy, verified) = match staged {
        Some(copy) if store.source == local.local_node() => (copy, true),
        _ if store.source == local.local_node() => return Ok(PageOutcome::Failed),
        _ => match pull(local, &membership, store, &page, scope).await {
            Ok(copy) => (copy, false),
            Err(_) => return Ok(PageOutcome::Failed),
        },
    };
    if !matches_store(&copy, &page, store) {
        return Ok(PageOutcome::Failed);
    }
    if !verified
        && local
            .fill
            .dependencies
            .crypto
            .verify_tag(copy.ciphertext.clone(), scope)
            .await
            .is_err()
    {
        return Ok(PageOutcome::Failed);
    }
    let hold = crate::store::writes::from_unix_millis(store.hold_until_millis);
    let clock = local.fill.dependencies.candidates.ctx().clock.clone();
    let start = clock.now();
    let cache = page.version.object.cache.clone();
    let bytes = copy.ciphertext.bytes().len() as u64;
    let mut attempt = 0;
    let result = loop {
        match writer
            .persist_written(copy.clone(), store.session, hold, scope)
            .await
        {
            // `Overloaded` is either the dirty cap (final) or a transient
            // condition: a checkpoint froze appends, or the written tail is
            // rolling to a new segment. Retry the transient ones briefly.
            Err(Error::Overloaded)
                if attempt < TRANSIENT_RETRIES
                    && writer.dirty_bytes(&cache).saturating_add(bytes)
                        <= writer.dirty_limit(&cache) =>
            {
                attempt += 1;
                let Some(reactor) = local.writes.reactor() else {
                    break Err(Error::Overloaded);
                };
                reactor
                    .sleep_until(
                        clock.now()
                            + (TRANSIENT_WAIT * (1 << attempt.min(8))).min(TRANSIENT_WAIT_MAX),
                        scope,
                    )
                    .await?;
            }
            result => break result,
        }
    };
    match result {
        Ok(_) => {
            local
                .fill
                .metrics
                .writes()
                .fsync(clock.now().saturating_duration_since(start));
            Ok(PageOutcome::Done)
        }
        Err(Error::Overloaded) => Ok(PageOutcome::Full),
        Err(Error::DeadlineExceeded | Error::Canceled) => Err(Error::DeadlineExceeded),
        Err(_) => Ok(PageOutcome::Failed),
    }
}

/// Whether a pulled copy is exactly `page` of the version `store` describes.
fn matches_store(copy: &CiphertextCopy, page: &PageId, store: &WriteStore) -> bool {
    let envelope = copy.ciphertext.envelope();
    envelope.page == *page
        && copy.metadata.version == page.version
        && copy.metadata.length == store.length
        && copy.metadata.content_type == store.content_type
        && envelope.page_size == store.page_size
        && copy.validate_metadata().is_ok()
}

/// A directed copy-only `page` request to the writer (or a surviving holder).
async fn pull(
    local: &Coordinator,
    membership: &Arc<Membership>,
    store: &WriteStore,
    page: &PageId,
    scope: &RequestScope,
) -> Result<CiphertextCopy> {
    let context = super::bare_context(&page.version.object);
    let operation = PeerOperation::Page {
        page: page.clone(),
        mode: FetchMode::CopyOnly,
    };
    let mut budget = crate::read::default_budget(scope);
    let response = local
        .fill
        .dependencies
        .candidates
        .request(
            membership,
            &store.source,
            &context,
            &operation,
            FetchMode::CopyOnly,
            scope,
            &mut budget,
            1,
        )
        .await?;
    match response.response() {
        PeerResponse::Page {
            metadata,
            ciphertext,
        } => Ok(CiphertextCopy {
            metadata: metadata.clone(),
            ciphertext: ciphertext.clone(),
        }),
        _ => Err(Error::Unavailable),
    }
}

/// Serve `write-release` on the object's metadata worker.
pub async fn serve_release(
    local: &Coordinator,
    version: ObjectVersion,
    session: SessionId,
    pages: Vec<u64>,
    keep: bool,
    scope: &RequestScope,
) -> Result<WriteReply> {
    let directory = local.prefetch_directory();
    futures::future::join_all(pages.iter().map(|&number| {
        let page = page_id(&version, number);
        let work = WriteWork::Release {
            page: page.clone(),
            session,
            keep,
        };
        async move { directory.write_page(&page, work, scope).await }
    }))
    .await;
    Ok(WriteReply::Released)
}

/// Release (`keep`: stays readable as ordinary cache) or drop one written
/// page of `session` on its owning worker. Pages of other sessions are left
/// alone.
pub async fn release_page(
    local: &Coordinator,
    page: &PageId,
    session: SessionId,
    keep: bool,
) -> Result<PageOutcome> {
    let writer = &local.fill.dependencies.writer;
    if writer
        .written(page)
        .is_none_or(|entry| entry.session != session)
    {
        return Ok(PageOutcome::Done);
    }
    if keep {
        writer.release_written(page).await?;
    } else {
        writer.drop_written(page).await?;
    }
    Ok(PageOutcome::Done)
}
