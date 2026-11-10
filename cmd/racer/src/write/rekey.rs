//! Rekeying of written pages (WP8).
//!
//! A written page can stay pinned far longer than a page key lives: the
//! keyring keeps an active page key plus at most
//! `retained_page_keys - 1` (default 6) decrypt keys per cache, rotates
//! daily by default, and drops the oldest decrypt key on each activation.
//! A pinned page sealed under a dropped key is unreadable and cannot be
//! uploaded.
//!
//! Threshold: a holder rekeys every written page whose key is not the
//! cache's active page key. The node cannot see the authority's retention
//! setting, which may be as low as one key (no decrypt keys at all), so
//! "not active" is the only threshold that is safe for every setting. The
//! cost is one decrypt, one encrypt, and one record write per written page
//! per rotation, against a default rotation interval of a day.
//!
//! The page is decrypted (which verifies its tag), sealed under the active
//! key, written as a new record and journaled with its session and hold
//! (`StoreWriter::rekey_written`), and the old record is unpinned; it stays
//! on disk as evictable cache until its segment is reclaimed. Every holder
//! rekeys its own copy; copies on different holders may use different keys,
//! which readers and uploads already handle per page.

use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Result;
use crate::memory::CiphertextCopy;
use crate::model::PageId;
use crate::read::Coordinator;
use crate::runtime::RequestScope;
use std::collections::VecDeque;
use std::time::Duration;
use std::time::Instant;

/// Pages rekeyed per maintenance turn on one worker.
pub const BATCH: usize = 8;

/// Full rescan interval, which also catches pages written under a key
/// that rotated while the write was in flight.
pub const RESCAN: Duration = Duration::from_secs(60);

/// Per-worker rekey state.
#[derive(Default)]
pub struct RekeyState {
    /// Keyring generation of the last scan.
    generation: Option<Option<u64>>,
    next_scan: Option<Instant>,
    queue: VecDeque<PageId>,
}

/// Whether `page` (written here) is sealed under a key other than its
/// cache's active page key.
fn stale(local: &Coordinator, page: &PageId) -> bool {
    let Some(active) = local
        .availability
        .active_page_key(&page.version.object.cache)
    else {
        return false;
    };
    local
        .fill
        .dependencies
        .writer
        .index()
        .lookup(page)
        .ok()
        .flatten()
        .is_some_and(|indexed| indexed.key_id != active)
}

/// One rekey turn: rescan after a keyring change or every [`RESCAN`], then
/// rekey up to [`BATCH`] queued pages. Returns the pages rekeyed.
pub(crate) async fn run(local: &Coordinator, scope: &RequestScope) -> Result<usize> {
    let writer = &local.fill.dependencies.writer;
    if writer.write_journal().is_none() {
        return Ok(0);
    }
    let now = scope.clock().now();
    let generation = local.availability.key_generation();
    let rescan = {
        let state = local.writes.rekey.borrow();
        state.generation != Some(generation) || state.next_scan.is_none_or(|at| now >= at)
    };
    if rescan {
        let queue: VecDeque<PageId> = writer
            .written_pages()
            .into_iter()
            .map(|(page, _)| page)
            .filter(|page| stale(local, page))
            .collect();
        let mut state = local.writes.rekey.borrow_mut();
        state.generation = Some(generation);
        state.next_scan = Some(now + RESCAN);
        state.queue = queue;
    }
    let mut done = 0;
    for _ in 0..BATCH {
        let Some(page) = local.writes.rekey.borrow_mut().queue.pop_front() else {
            break;
        };
        match rekey_page(local, &page, scope).await {
            Ok(true) => done += 1,
            Ok(false) => {}
            // Out of buffers or segments: try again on the next scan.
            Err(Error::Overloaded) => break,
            Err(_) => {}
        }
    }
    Ok(done)
}

/// Rekey one written page under its cache's active key. `false` when there
/// was nothing to do (not written here any more, or already current).
pub(crate) async fn rekey_page(
    local: &Coordinator,
    page: &PageId,
    scope: &RequestScope,
) -> Result<bool> {
    let deps = &local.fill.dependencies;
    if deps.writer.written(page).is_none() || !stale(local, page) {
        return Ok(false);
    }
    let indexed = deps.writer.index().lookup(page)?.ok_or(Error::NotFound)?;
    let cache = &page.version.object.cache;
    // Decrypting verifies the tag of the old record.
    let result = local.fill.warm_local(page, scope).await?;
    let bytes = result.plaintext.bytes();
    let charge = deps
        .admission
        .reserve(Some(cache), ResourceClass::Plaintext, bytes.len())
        .map_err(|_| Error::Overloaded)?;
    let mut plaintext = deps
        .buffers
        .plaintext(charge, bytes.len())
        .map_err(|_| Error::Overloaded)?;
    racer_io::ring::IoBuffer::bytes_mut(&mut plaintext)?.copy_from_slice(bytes);
    let charge = deps
        .admission
        .reserve(
            Some(cache),
            ResourceClass::Ciphertext,
            bytes.len() + racer_crypto::TAG_LEN,
        )
        .map_err(|_| Error::Overloaded)?;
    let metadata = result.metadata.clone();
    drop(result);
    let (_verified, ciphertext) = deps
        .crypto
        .encrypt(page.clone(), indexed.page_size, plaintext, charge, scope)
        .await?;
    let copy = CiphertextCopy {
        metadata,
        ciphertext,
    };
    match deps.writer.rekey_written(copy, scope).await {
        Ok(entry) => Ok(entry.is_some()),
        // The key rotated again or another turn got there first.
        Err(Error::InvalidRequest) => Ok(false),
        Err(error) => Err(error),
    }
}
