//! Durable written (write-back) pages on one storage worker.
//!
//! A written page is an ordinary encrypted `RCRPAGE1` record, so the existing
//! read path serves it unchanged once it is published. What differs is where
//! and how long it lives:
//!
//! - It is appended to a dedicated written-record segment
//!   ([`page_alloc::Segments::append_written`]), never mixed with cache pages.
//! - It is durable before it is acknowledged: the record write completes, the
//!   slab descriptor is `fdatasync`ed, and a [`JournalRecord::Page`] is
//!   group-committed to the worker's [`WriteJournal`].
//! - While it is live (not yet released), its segment carries one pin per
//!   written record, and pinned segments are never evicted.
//! - Live written bytes count against the cache's dirty cap, a percentage
//!   (`spec.writes.maxDirtyPercent`) of the worker's segment capacity.
//!
//! Releasing a page ([`StoreWriter::release_written`]) journals a
//! [`JournalRecord::PageRelease`] and then drops the pin: the record stays
//! readable and becomes ordinary evictable cache. Dropping a page
//! ([`StoreWriter::drop_written`]) also removes its catalog entry first, for
//! deletes, superseded versions, and orphans.
//!
//! At startup, [`StoreWriter::recover_written`] replays the journal after the
//! checkpoint shard is installed (accepted or not) and before the writer
//! appends anything: it re-adopts each live record's segment, verifies the
//! record header, pins the segment, and republishes the page.
use super::Error;
use super::Operation;
use super::Result;
use super::StoreWriter;
use super::catalog::IndexedPage;
use super::catalog::RecordLocation;
use super::journal::JournalPage;
use super::journal::JournalState;
use super::journal::PageKey;
use super::journal::WriteJournal;
use super::journal::page_record;
use crate::admission::ResourceClass;
use crate::memory::CiphertextCopy;
use crate::model::CacheKey;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::StrongEtag;
use crate::runtime::HashMap;
use crate::runtime::HashSet;
use crate::runtime::RequestScope;
use page_alloc::Extent;
use page_alloc::Generation;
use page_alloc::SegmentId;
use racer_core::write::JournalRecord;
use racer_core::write::SessionId;
use racer_wire::control::CacheId;
use std::cell::RefCell;
use std::path::Path;
use std::rc::Rc;
use std::time::Duration;
use std::time::SystemTime;
use std::time::UNIX_EPOCH;

/// One live written page.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WrittenEntry {
    /// Where the durable record lives.
    pub location: RecordLocation,
    /// The write session that stored it.
    pub session: SessionId,
    /// When the hold on the page ends, unless extended.
    pub hold_until: SystemTime,
    /// Record bytes on disk, charged to the cache's dirty cap.
    pub disk_bytes: u64,
}

/// Why a journaled page could not be restored at startup.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum LostReason {
    /// The journal was bound to another storage layout or worker.
    LayoutChanged,
    /// The journal names an impossible page or location.
    Malformed,
    /// The segment was reused since the record was written.
    Stale,
    /// Reading the record header failed.
    Unreadable,
    /// The record header failed its digest or names another page.
    Corrupt,
}

/// A journaled page that recovery reports lost, for repair.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct LostPage {
    /// The journal's identity of the page.
    pub key: PageKey,
    /// The page, when the identity parses.
    pub page: Option<PageId>,
    pub reason: LostReason,
}

/// What [`StoreWriter::recover_written`] restored and lost.
#[derive(Clone, Debug, Default)]
pub struct WriteRecovery {
    /// Pages verified, pinned, and republished in the catalog.
    pub restored: Vec<PageId>,
    /// Pages verified and pinned whose catalog publication was refused (for
    /// example, their cache or key is not published yet). They stay durable
    /// and listed by [`StoreWriter::written_pages`], but are not readable.
    pub unindexed: Vec<PageId>,
    /// Pages that could not be restored.
    pub lost: Vec<LostPage>,
    /// The journal's live state after recovery, including write sessions.
    pub state: JournalState,
    /// Bytes of a torn or corrupt journal tail that were discarded.
    pub torn_bytes: u64,
}

/// In-memory written state of one worker; the journal is authoritative.
#[derive(Default)]
pub(crate) struct Writes {
    journal: RefCell<Option<Rc<WriteJournal>>>,
    pages: RefCell<HashMap<PageId, WrittenEntry>>,
    dirty: RefCell<HashMap<CacheId, u64>>,
    persisting: RefCell<HashSet<PageId>>,
}

impl Writes {
    fn journal(&self) -> Result<Rc<WriteJournal>> {
        self.journal.borrow().clone().ok_or(Error::Unavailable)
    }

    fn get(&self, page: &PageId) -> Option<WrittenEntry> {
        self.pages.borrow().get(page).cloned()
    }

    fn charge(&self, cache: &CacheId, add: u64, remove: u64) {
        let mut dirty = self.dirty.borrow_mut();
        let entry = dirty.entry(cache.clone()).or_default();
        *entry = entry.saturating_add(add).saturating_sub(remove);
        if *entry == 0 {
            dirty.remove(cache);
        }
    }

    /// Track `entry`, returning the entry it replaced.
    fn insert(&self, page: PageId, entry: WrittenEntry) -> Option<WrittenEntry> {
        let cache = page.version.object.cache.clone();
        let bytes = entry.disk_bytes;
        let previous = self.pages.borrow_mut().insert(page, entry);
        self.charge(&cache, bytes, previous.as_ref().map_or(0, |p| p.disk_bytes));
        previous
    }

    /// Stop tracking `page` if it is still at `location`.
    fn remove_if(&self, page: &PageId, location: &RecordLocation) -> Option<WrittenEntry> {
        let mut pages = self.pages.borrow_mut();
        if pages
            .get(page)
            .is_none_or(|entry| entry.location != *location)
        {
            return None;
        }
        let entry = pages.remove(page)?;
        drop(pages);
        self.charge(&page.version.object.cache, 0, entry.disk_bytes);
        Some(entry)
    }

    fn dirty(&self, cache: &CacheId) -> u64 {
        self.dirty.borrow().get(cache).copied().unwrap_or(0)
    }

    fn begin(&self, page: &PageId) -> Result<Persisting<'_>> {
        if !self.persisting.borrow_mut().insert(page.clone()) {
            return Err(Error::Overloaded);
        }
        Ok(Persisting {
            writes: self,
            page: page.clone(),
        })
    }
}

/// Marks one page as being persisted, so a concurrent store of the same page
/// is refused instead of racing it.
struct Persisting<'a> {
    writes: &'a Writes,
    page: PageId,
}

impl Drop for Persisting<'_> {
    fn drop(&mut self) {
        self.writes.persisting.borrow_mut().remove(&self.page);
    }
}

/// The journal identity of `page`.
pub fn page_key(page: &PageId) -> Result<PageKey> {
    Ok(PageKey {
        cache: page.version.object.cache.0.clone(),
        key: page.version.object.key.0,
        etag: String::from_utf8(page.version.etag.as_bytes().to_vec())
            .map_err(|_| Error::InvalidRequest)?,
        page: page.number.0,
    })
}

/// The page a journal identity names.
pub fn page_id(key: &PageKey) -> Result<PageId> {
    Ok(PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId(key.cache.clone()),
                key: CacheKey(key.key),
            },
            etag: StrongEtag::parse(key.etag.as_bytes()).map_err(|_| Error::InvalidRequest)?,
        },
        number: PageNumber(key.page),
    })
}

/// The layout a write journal is bound to: the checkpoint layout digest
/// (device identities in raw mode), the slab geometry, and the worker count.
/// Any change reports every journaled page lost.
pub fn journal_layout(geometry: &super::checkpoint::CheckpointGeometry, workers: u64) -> [u8; 32] {
    use sha2::Digest;
    let mut digest = sha2::Sha256::new();
    digest.update(b"racer/write-journal/layout/v1");
    digest.update(geometry.layout_digest);
    for value in [
        geometry.slab_bytes,
        geometry.segment_bytes,
        geometry.segment_count,
        geometry.memory_alignment,
        geometry.offset_alignment,
        geometry.length_alignment,
        workers,
    ] {
        digest.update(value.to_be_bytes());
    }
    digest.finalize().into()
}

/// Milliseconds since the Unix epoch, saturating.
pub fn unix_millis(time: SystemTime) -> u64 {
    time.duration_since(UNIX_EPOCH)
        .map_or(0, |d| u64::try_from(d.as_millis()).unwrap_or(u64::MAX))
}

/// The time `millis` after the Unix epoch.
pub fn from_unix_millis(millis: u64) -> SystemTime {
    UNIX_EPOCH + Duration::from_millis(millis)
}

impl StoreWriter {
    /// The worker's write journal, once [`Self::recover_written`] bound it.
    /// The write service journals session records (`Open`, `Commit`,
    /// `State`, `Abort`, `Delete`) through it.
    pub fn write_journal(&self) -> Option<Rc<WriteJournal>> {
        self.writes.journal.borrow().clone()
    }

    /// Open and replay this worker's write journal under `slab_directory`,
    /// then bind it. Run once at startup after the checkpoint shard is
    /// installed (or rejected) and before any write can append. Every live
    /// page record's segment is re-adopted, its header verified, its segment
    /// pinned, and the page republished. A journal bound to another `layout`
    /// reports all its pages lost but keeps its sessions. The journal is then
    /// rewritten with only the restored state. Failures other than per-page
    /// loss leave the journal file untouched and the writer without writes.
    pub fn recover_written<'a>(
        &'a self,
        slab_directory: &'a Path,
        layout: [u8; 32],
        compact_bytes: u64,
        scope: &'a RequestScope,
    ) -> Operation<'a, WriteRecovery> {
        Box::pin(async move {
            if self.writes.journal.borrow().is_some() {
                return Err(Error::InvalidConfiguration);
            }
            let worker = self.index.worker().0;
            let (journal, opened) = WriteJournal::open(
                self.reactor.clone(),
                slab_directory,
                worker,
                layout,
                compact_bytes,
            )
            .await?;
            let mut recovery = WriteRecovery {
                torn_bytes: opened.torn_bytes,
                ..WriteRecovery::default()
            };
            for key in opened.unbound_pages {
                recovery.lost.push(LostPage {
                    page: page_id(&key).ok(),
                    key,
                    reason: LostReason::LayoutChanged,
                });
            }
            let mut lost = Vec::new();
            for (key, entry) in opened.state.pages {
                scope.check()?;
                match self.restore_written(&key, &entry, scope).await? {
                    Ok((page, true)) => recovery.restored.push(page),
                    Ok((page, false)) => recovery.unindexed.push(page),
                    Err(reason) => {
                        lost.push(key.clone());
                        recovery.lost.push(LostPage {
                            page: page_id(&key).ok(),
                            key,
                            reason,
                        });
                    }
                }
            }
            journal.forget_pages(&lost);
            journal.rewrite().await?;
            recovery.state = journal.state();
            *self.writes.journal.borrow_mut() = Some(Rc::new(journal));
            Ok(recovery)
        })
    }

    /// Verify and reinstate one journaled page. The outer error aborts
    /// recovery; the inner one reports the page lost.
    async fn restore_written(
        &self,
        key: &PageKey,
        entry: &JournalPage,
        scope: &RequestScope,
    ) -> Result<std::result::Result<(PageId, bool), LostReason>> {
        let Ok(id) = page_id(key) else {
            return Ok(Err(LostReason::Malformed));
        };
        let Ok(extent) = Extent::new(entry.offset, entry.length as usize) else {
            return Ok(Err(LostReason::Malformed));
        };
        let segment = SegmentId(entry.segment);
        let generation = Generation(entry.generation);
        let segment_bytes = self.segments.segment_bytes();
        let Some(start) = entry.segment.checked_mul(segment_bytes) else {
            return Ok(Err(LostReason::Malformed));
        };
        let end = entry.offset.saturating_add(u64::from(entry.length));
        if entry.offset < start || end > start.saturating_add(segment_bytes) {
            return Ok(Err(LostReason::Malformed));
        }
        match self
            .segments
            .adopt_written(segment, generation, end - start)
        {
            Ok(()) => {}
            Err(page_alloc::Error::Stale) => return Ok(Err(LostReason::Stale)),
            Err(page_alloc::Error::Corrupt) => return Ok(Err(LostReason::Malformed)),
            Err(error) => return Err(error.into()),
        }
        let Ok(lease) = self.segments.lease(segment, generation) else {
            return Ok(Err(LostReason::Stale));
        };
        let prefix = extent.length().min(super::MAX_HEADER_BYTES);
        let Ok(prefix_extent) = Extent::new(extent.offset(), prefix) else {
            return Ok(Err(LostReason::Malformed));
        };
        let charge = self.admission.reserve_completion(
            Some(&id.version.object.cache),
            ResourceClass::Ciphertext,
            prefix,
        )?;
        let buffer = self.slabs.allocate(prefix, charge)?;
        let buffer = match self
            .slabs
            .read(&self.reactor, prefix_extent, buffer, lease, scope)
            .await
        {
            Ok(buffer) => buffer,
            Err(Error::Io | Error::Os(_) | Error::CorruptRecord | Error::InvalidConfiguration) => {
                return Ok(Err(LostReason::Unreadable));
            }
            Err(error) => return Err(error),
        };
        let Ok(decoded) = super::parse_prefix(buffer.bytes()?, extent) else {
            return Ok(Err(LostReason::Corrupt));
        };
        drop(buffer);
        let envelope = &decoded.header.envelope;
        if envelope.page != id || decoded.header.generation != generation {
            return Ok(Err(LostReason::Corrupt));
        }
        let location = RecordLocation {
            segment,
            generation,
            extent,
        };
        self.index.pin_segment(segment, self.segments.count())?;
        if let Some(previous) = self.writes.insert(
            id.clone(),
            WrittenEntry {
                location: location.clone(),
                session: entry.session,
                hold_until: from_unix_millis(entry.hold_until_millis),
                disk_bytes: extent.length() as u64,
            },
        ) {
            self.index.unpin_segment(previous.location.segment)?;
        }
        let cache = &id.version.object.cache;
        let indexed = self.availability.page(cache, envelope.key_id)
            && !self.availability.passthrough(cache)
            && self
                .index
                .publish(
                    id.clone(),
                    IndexedPage {
                        location,
                        page_size: envelope.page_size,
                        metadata: decoded.header.metadata.clone(),
                        key_id: envelope.key_id,
                    },
                )
                .is_ok();
        if indexed {
            self.retention()
                .published(&id, u64::from(envelope.plaintext_length));
        }
        Ok(Ok((id, indexed)))
    }

    /// Store a verified ciphertext page as a durable written page: append it
    /// to a written-record segment, write it, `fdatasync` the slab, journal
    /// it (group-committed `fdatasync`), pin its segment, and publish it to
    /// the catalog. Resolves only once the page is crash durable.
    ///
    /// Idempotent: storing a page that is already written and indexed only
    /// extends its hold to `hold_until` when that is later. A written page
    /// that lost its catalog entry is written again.
    ///
    /// Errors: `Unavailable` before [`Self::recover_written`], after the
    /// writer closed, or for a passthrough cache; `MissingKey` without the
    /// page's cache or key; `Overloaded` when the cache is over its dirty cap,
    /// no written segment or index entry is free, appends are frozen by a
    /// checkpoint, or the same page is being stored concurrently (all map to
    /// `WriteReply::Full`); I/O errors when the write or a barrier fails, in
    /// which case nothing is journaled, or the journal is poisoned.
    pub fn persist_written<'a>(
        &'a self,
        page: CiphertextCopy,
        session: SessionId,
        hold_until: SystemTime,
        scope: &'a RequestScope,
    ) -> Operation<'a, WrittenEntry> {
        Box::pin(async move {
            self.persist_inner(page, session, hold_until, None, scope)
                .await?
                .ok_or(Error::Unavailable)
        })
    }

    /// Replace the record of written page `page` with `page` (the same
    /// plaintext sealed under another key), keeping its session and hold:
    /// write and journal the new record, then unpin the old one, which stays
    /// on disk as evictable cache. Used by rekeying (WP8). Returns `None` when
    /// the page is not written here, or was released while the new record
    /// was written (the new record is then released too).
    ///
    /// Errors as [`Self::persist_written`]; `InvalidRequest` when the copy
    /// uses the key the current record already uses.
    pub fn rekey_written<'a>(
        &'a self,
        page: CiphertextCopy,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<WrittenEntry>> {
        Box::pin(async move {
            let id = page.ciphertext.envelope().page.clone();
            let Some(existing) = self.writes.get(&id) else {
                return Ok(None);
            };
            if self
                .index
                .lookup(&id)?
                .is_some_and(|indexed| indexed.key_id == page.ciphertext.envelope().key_id)
            {
                return Err(Error::InvalidRequest);
            }
            self.persist_inner(
                page,
                existing.session,
                existing.hold_until,
                Some(existing.location),
                scope,
            )
            .await
        })
    }

    /// [`Self::persist_written`], or with `replace` (the location of the
    /// record being replaced) a forced rewrite for [`Self::rekey_written`].
    async fn persist_inner(
        &self,
        page: CiphertextCopy,
        session: SessionId,
        hold_until: SystemTime,
        replace: Option<RecordLocation>,
        scope: &RequestScope,
    ) -> Result<Option<WrittenEntry>> {
        {
            scope.check()?;
            super::logical_length(&page)?;
            let journal = self.writes.journal()?;
            if self.closed.get() {
                return Err(Error::Unavailable);
            }
            let id = page.ciphertext.envelope().page.clone();
            let cache = id.version.object.cache.clone();
            if self.availability.passthrough(&cache) {
                return Err(Error::Unavailable);
            }
            if !self.allowed(&page) {
                return Err(Error::MissingKey);
            }
            let key = page_key(&id)?;
            if replace.is_none()
                && let Some(existing) = self.writes.get(&id)
                && let Some(indexed) = self.index.lookup(&id)?
                && indexed.location == existing.location
            {
                if !indexed.metadata.compatible(&page.metadata.immutable()) {
                    return Err(Error::CorruptRecord);
                }
                if hold_until > existing.hold_until {
                    self.set_written_hold(&id, hold_until).await?;
                }
                return self.writes.get(&id).ok_or(Error::Unavailable).map(Some);
            }
            let _persisting = self.writes.begin(&id)?;
            let alignment = self.slabs.alignment()?;
            let disk_bytes = super::record_length(&page, alignment)?;
            let replaced = self.writes.get(&id).map_or(0, |entry| entry.disk_bytes);
            if self
                .writes
                .dirty(&cache)
                .saturating_sub(replaced)
                .saturating_add(disk_bytes as u64)
                > self.dirty_limit(&cache)
            {
                return Err(Error::Overloaded);
            }
            let mut ticket = match self.index.reserve_page_for(&id) {
                Err(Error::Overloaded) if self.clock.borrow().is_some() => {
                    let clock = self.clock.borrow().clone().expect("checked clock");
                    let _ = clock.reclaim_entries();
                    self.index.reserve_page_for(&id)?
                }
                ticket => ticket?,
            };
            let (lease, location) = match ticket.append_written(&id, &self.segments, disk_bytes) {
                Ok(appended) => appended,
                Err(Error::Overloaded) => {
                    if let Some(clock) = self.clock.borrow().clone() {
                        let _ = clock.reclaim_now();
                    }
                    ticket.append_written(&id, &self.segments, disk_bytes)?
                }
                Err(error) => return Err(error),
            };
            let sync_lease = self.segments.lease(location.segment, location.generation)?;
            let staging = self.reserve_staging(disk_bytes, &cache)?;
            let buffer = self.slabs.allocate(disk_bytes, staging)?;
            let encoded = super::encode_at(
                &page,
                location.generation,
                alignment,
                location.extent.offset(),
                buffer,
            )?;
            let _ = self
                .slabs
                .write(&self.reactor, location.extent, encoded.buffer, lease, scope)
                .await?;
            let (descriptor, _) = self.slabs.locate(location.extent, &sync_lease)?;
            let entry = WrittenEntry {
                location: location.clone(),
                session,
                hold_until,
                disk_bytes: disk_bytes as u64,
            };
            journal
                .append(
                    page_record(
                        &key,
                        &JournalPage {
                            segment: location.segment.0,
                            generation: location.generation.0,
                            offset: location.extent.offset(),
                            length: location.extent.length() as u32,
                            session,
                            hold_until_millis: unix_millis(hold_until),
                        },
                    ),
                    Some(descriptor),
                )
                .await?;
            if let Some(replaced) = &replace
                && self
                    .writes
                    .get(&id)
                    .is_none_or(|current| current.location != *replaced)
            {
                // Released (or replaced) while this record was written: the
                // new Page record must not resurrect the pin.
                let key = page_key(&id)?;
                journal
                    .append(
                        JournalRecord::PageRelease {
                            cache: key.cache,
                            key: key.key,
                            etag: key.etag,
                            page: key.page,
                        },
                        None,
                    )
                    .await?;
                drop(ticket);
                return Ok(None);
            }
            // Durable: pin before anything else can fail.
            self.index
                .pin_segment(location.segment, self.segments.count())?;
            if let Some(previous) = self.writes.insert(id.clone(), entry.clone()) {
                // The new Page record superseded the old one in the journal.
                self.index.unpin_segment(previous.location.segment)?;
            }
            let envelope = page.ciphertext.envelope();
            let published = if self.allowed(&page) {
                ticket.publish(
                    id.clone(),
                    IndexedPage {
                        page_size: envelope.page_size,
                        location,
                        metadata: page.metadata.immutable(),
                        key_id: envelope.key_id,
                    },
                )
            } else {
                Err(Error::MissingKey)
            };
            if let Err(error) = published {
                // Durable but unreadable (its cache or key went away): let
                // it go rather than pin a record nobody can serve.
                self.release_inner(&id, false).await?;
                return Err(error);
            }
            self.metrics
                .record(crate::telemetry::Event::DiskPublication, 1);
            self.retention()
                .published(&id, u64::from(envelope.plaintext_length));
            drop(sync_lease);
            Ok(Some(entry))
        }
    }

    /// Release a written page once its object is uploaded or committed
    /// elsewhere: journal a `PageRelease`, then unpin it. The record stays
    /// readable as ordinary, evictable cache. Returns whether `page` was
    /// written here.
    pub fn release_written<'a>(&'a self, page: &'a PageId) -> Operation<'a, bool> {
        Box::pin(self.release_inner(page, false))
    }

    /// Drop a written page (deleted, superseded, or orphaned): remove its
    /// catalog entry at once, journal a `PageRelease`, then unpin it. Returns
    /// whether `page` was written here.
    pub fn drop_written<'a>(&'a self, page: &'a PageId) -> Operation<'a, bool> {
        Box::pin(self.release_inner(page, true))
    }

    async fn release_inner(&self, page: &PageId, remove: bool) -> Result<bool> {
        let Some(entry) = self.writes.get(page) else {
            return Ok(false);
        };
        if remove {
            self.index.remove_if_matches(page, &entry.location);
        }
        let key = page_key(page)?;
        self.writes
            .journal()?
            .append(
                JournalRecord::PageRelease {
                    cache: key.cache,
                    key: key.key,
                    etag: key.etag,
                    page: key.page,
                },
                None,
            )
            .await?;
        if self.writes.remove_if(page, &entry.location).is_some() {
            self.index.unpin_segment(entry.location.segment)?;
        }
        Ok(true)
    }

    /// Set a written page's hold deadline (extend or shorten) durably.
    /// Returns whether `page` is written here.
    pub fn set_written_hold<'a>(
        &'a self,
        page: &'a PageId,
        hold_until: SystemTime,
    ) -> Operation<'a, bool> {
        Box::pin(async move {
            let Some(entry) = self.writes.get(page) else {
                return Ok(false);
            };
            if entry.hold_until == hold_until {
                return Ok(true);
            }
            let key = page_key(page)?;
            let record = page_record(
                &key,
                &JournalPage {
                    segment: entry.location.segment.0,
                    generation: entry.location.generation.0,
                    offset: entry.location.extent.offset(),
                    length: entry.location.extent.length() as u32,
                    session: entry.session,
                    hold_until_millis: unix_millis(hold_until),
                },
            );
            self.writes.journal()?.append(record, None).await?;
            if let Some(current) = self.writes.pages.borrow_mut().get_mut(page)
                && current.location == entry.location
            {
                current.hold_until = hold_until;
            }
            Ok(true)
        })
    }

    /// The live written page `page`, if any.
    pub fn written(&self, page: &PageId) -> Option<WrittenEntry> {
        self.writes.get(page)
    }

    /// Every live written page, for repair and upload after a restart.
    pub fn written_pages(&self) -> Vec<(PageId, WrittenEntry)> {
        self.writes
            .pages
            .borrow()
            .iter()
            .map(|(page, entry)| (page.clone(), entry.clone()))
            .collect()
    }

    /// Live written pages whose hold ended at or before `now`. The caller
    /// (the per-worker hold timer) drops them unless the coordinator extended
    /// them.
    pub fn expired_written(&self, now: SystemTime) -> Vec<PageId> {
        self.writes
            .pages
            .borrow()
            .iter()
            .filter(|(_, entry)| entry.hold_until <= now)
            .map(|(page, _)| page.clone())
            .collect()
    }

    /// Live written (not yet released) record bytes of `cache` on this worker.
    pub fn dirty_bytes(&self, cache: &CacheId) -> u64 {
        self.writes.dirty(cache)
    }

    /// The dirty cap of `cache` on this worker: its published
    /// `maxDirtyPercent` (or the default) of the worker's segment capacity.
    pub fn dirty_limit(&self, cache: &CacheId) -> u64 {
        let percent = self.availability.writes(cache).map_or(
            racer_wire::control::DEFAULT_WRITE_MAX_DIRTY_PERCENT,
            |writes| writes.max_dirty_percent,
        );
        (u128::from(self.segments.capacity_bytes()) * u128::from(percent) / 100) as u64
    }
}
