//! Rekeying of written pages (WP8).
//!
//! A written page can stay pinned far longer than a page key lives: the
//! keyring keeps an active page key plus at most
//! `retained_page_keys - 1` (default 6) decrypt keys per cache, rotates
//! daily by default, and drops the oldest decrypt key on each activation.
//! A pinned page sealed under a dropped key is unreadable and cannot be
//! uploaded.
//!
//! Overlap: rekeying needs the previous page key until every written page
//! is resealed. The authority keeps at least one decrypt key for every
//! write-enabled cache whatever `retained_page_keys` says
//! (`MIN_WRITE_RETAINED_PAGE_KEYS`), so a holder has one rotation interval
//! after each activation to rekey its pages. At [`BATCH`] pages per
//! maintenance turn per worker that is far more than any store holds.
//!
//! A written page whose key already left the keyring (a holder that was
//! down for longer than the retention window) is invisible to the index and
//! cannot be rekeyed. Each scan counts such pages per cache in the
//! `racer_write_unreadable_pages` gauge, so the loss of this copy is visible
//! instead of silent. The copy stays pinned until its hold ends; other
//! holders' copies are unaffected if they rekeyed in time.
//!
//! Threshold: a holder rekeys every written page whose key is not the
//! cache's active page key. The node cannot see the authority's retention
//! setting, which may be as low as one key (no decrypt keys at all), so
//! "not active" is the only threshold that is safe for every setting. The
//! cost is one decrypt, one encrypt, and one record write per written page
//! per rotation, against a default rotation interval of a day.
//!
//! Evacuation: near the dirty cap the disk may have little more than the
//! rekey reserve free. Rekeying pages in arbitrary order would spread the
//! new records over that space without emptying any source segment, and
//! stall. A scan therefore queues whole source segments, those with the
//! fewest live written records first, and moves every live written record
//! of a segment (current-key records too, see
//! `StoreWriter::relocate_written`) before the next. New written pages leave
//! `REKEY_RESERVE_SEGMENTS` segments unpinned, so one segment's records always
//! fit; once it is empty its segment becomes evictable and refills the reserve.
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
use crate::store::catalog::RecordLocation;
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
    /// Pages to move, with the record each is expected at.
    queue: VecDeque<(PageId, RecordLocation)>,
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
        let mut unreadable: std::collections::BTreeMap<String, u64> = Default::default();
        // Live written records by source segment, and the segments with a
        // record under a non-active key.
        let mut segments: std::collections::BTreeMap<u64, Vec<(PageId, RecordLocation)>> =
            Default::default();
        let mut sources = std::collections::BTreeSet::new();
        for (page, entry) in writer.written_pages() {
            match writer.index().lookup(&page) {
                Ok(Some(_)) => {
                    let segment = entry.location.segment.0;
                    if stale(local, &page) {
                        sources.insert(segment);
                    }
                    segments
                        .entry(segment)
                        .or_default()
                        .push((page, entry.location));
                }
                // Pinned but hidden: its page key left the keyring.
                Ok(None) => {
                    *unreadable
                        .entry(page.version.object.cache.0.clone())
                        .or_default() += 1;
                }
                Err(_) => {}
            }
        }
        let queue = evacuation_order(segments, &sources);
        let unreadable: Vec<(String, u64)> = unreadable.into_iter().collect();
        local
            .fill
            .metrics
            .writes()
            .sample_unreadable(writer.index().worker().0, &unreadable);
        let mut state = local.writes.rekey.borrow_mut();
        state.generation = Some(generation);
        state.next_scan = Some(now + RESCAN);
        state.queue = queue;
    }
    let mut done = 0;
    for _ in 0..BATCH {
        let Some((page, from)) = local.writes.rekey.borrow_mut().queue.pop_front() else {
            break;
        };
        match move_page(local, &page, &from, scope).await {
            Ok(true) => done += 1,
            Ok(false) => {}
            // Out of buffers or segments: retry this page next turn, so the
            // segment being evacuated is still finished first.
            Err(Error::Overloaded) => {
                local
                    .writes
                    .rekey
                    .borrow_mut()
                    .queue
                    .push_front((page, from));
                break;
            }
            Err(_) => {}
        }
    }
    Ok(done)
}

/// Evacuation queue: every record of each segment in `sources`, segments
/// with the fewest records first (ties by segment id), records by offset.
fn evacuation_order(
    segments: std::collections::BTreeMap<u64, Vec<(PageId, RecordLocation)>>,
    sources: &std::collections::BTreeSet<u64>,
) -> VecDeque<(PageId, RecordLocation)> {
    let mut order: Vec<_> = segments
        .into_iter()
        .filter(|(segment, _)| sources.contains(segment))
        .collect();
    order.sort_by_key(|(segment, records)| (records.len(), *segment));
    order
        .into_iter()
        .flat_map(|(_, mut records)| {
            records.sort_by_key(|(_, location)| location.extent.offset());
            records
        })
        .collect()
}

/// Move written `page` off `from` under its cache's active key, rekeying it
/// if stale. `false` when there was nothing to do (moved, released, or the
/// cache has no active key).
async fn move_page(
    local: &Coordinator,
    page: &PageId,
    from: &RecordLocation,
    scope: &RequestScope,
) -> Result<bool> {
    let deps = &local.fill.dependencies;
    if deps
        .writer
        .written(page)
        .is_none_or(|entry| entry.location != *from)
    {
        return Ok(false);
    }
    if local
        .availability
        .active_page_key(&page.version.object.cache)
        .is_none()
    {
        return Ok(false);
    }
    let copy = reseal(local, page, scope).await?;
    match deps
        .writer
        .relocate_written(copy, from.clone(), scope)
        .await
    {
        Ok(entry) => Ok(entry.is_some()),
        Err(error) => Err(error),
    }
}

/// Decrypt written `page` (verifying its tag) and seal it under the cache's
/// active page key.
async fn reseal(
    local: &Coordinator,
    page: &PageId,
    scope: &RequestScope,
) -> Result<CiphertextCopy> {
    let deps = &local.fill.dependencies;
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
    Ok(CiphertextCopy {
        metadata,
        ciphertext,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use page_alloc::Extent;
    use page_alloc::Generation;
    use page_alloc::SegmentId;
    use std::collections::BTreeMap;
    use std::collections::BTreeSet;

    fn page(number: u64) -> PageId {
        PageId {
            version: crate::model::ObjectVersion {
                object: crate::model::ObjectId {
                    cache: racer_wire::control::CacheId("cache".into()),
                    key: crate::model::CacheKey([7; 32]),
                },
                etag: crate::model::StrongEtag::test_value("v1"),
            },
            number: crate::model::PageNumber(number),
        }
    }

    fn at(segment: u64, offset: u64) -> RecordLocation {
        RecordLocation {
            segment: SegmentId(segment),
            generation: Generation(1),
            extent: Extent::new(segment * 4096 + offset, 512).unwrap(),
        }
    }

    /// Whole source segments are queued, fewest records first, each in
    /// offset order and including its current-key records, so the first
    /// segment empties before the next one is started. Segments without a
    /// stale record are left alone.
    #[test]
    fn evacuation_drains_the_smallest_source_segment_first() {
        let mut segments = BTreeMap::new();
        segments.insert(
            1,
            vec![
                (page(1), at(1, 1024)),
                (page(2), at(1, 0)),
                (page(3), at(1, 512)),
            ],
        );
        segments.insert(2, vec![(page(4), at(2, 512)), (page(5), at(2, 0))]);
        segments.insert(3, vec![(page(6), at(3, 0))]);
        segments.insert(4, vec![(page(7), at(4, 0))]);
        let sources = BTreeSet::from([1, 2, 4]);
        let order: Vec<u64> = evacuation_order(segments, &sources)
            .into_iter()
            .map(|(page, _)| page.number.0)
            .collect();
        assert_eq!(order, vec![7, 5, 4, 2, 3, 1]);
        assert!(evacuation_order(BTreeMap::new(), &sources).is_empty());
    }
}
