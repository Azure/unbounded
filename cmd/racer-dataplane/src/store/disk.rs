//! Volume-backed disk pages: the per-worker extent volume, the hard hold table
//! over its storage tier, and the version table that maps key hashes back to
//! full versions.
//!
//! The index exists before `Store::open` mounts the volume
//! ([`Index::install_volume`]): it is wired into readers, writers, and recovery
//! at construction. Until the mount the disk is empty, so lookups miss and
//! allocations fail with `Unavailable`.

use super::catalog::DiskPage;
use super::catalog::HeldSnapshot;
use super::catalog::Index;
use super::holds::{Hold, HoldGuard, HoldTicket, Holds, MAX_HOLD_TTL};
use super::key::{CACHE_HASH_BYTES, VersionKey, cache_hash, version_key, volume_key};
use super::versions::VersionEntry;
use crate::admission::AdmissionPolicy;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::VersionMetadata;
use crate::runtime::HashMap;
use crate::runtime::HashSet;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use page_alloc::volume::{Key, Limits, MountReport, Reservation, SLOT_BYTES, Tier, Value, Volume};
use racer_control_wire::CacheId;
use racer_control_wire::KeyId;
use std::ops::Bound;
use std::rc::Rc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

/// Accounting guard type of the worker's slab buffers.
pub type Charge = flow_control::Charge<AdmissionPolicy>;

/// The worker's extent volume.
pub type DiskVolume = Volume<Charge>;

/// Slots a foreground allocation may evict before giving up with `Overloaded`.
pub const EVICTION_BUDGET: usize = 64;

/// Index entries visited per batch by full scans.
const SCAN_BATCH: usize = 4096;

/// Slots kept out of the hold quota: the meta slot and one cache slot, so cache
/// writes and commits can always make progress.
const RESERVED_SLOTS: u64 = 2;

/// Seconds since the Unix epoch, now.
fn now_unix() -> u64 {
    uring_runtime::environment::wall_now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

/// Whole seconds since the Unix epoch, rounded up, so protection never ends early.
fn ceil_unix(time: SystemTime) -> u64 {
    time.duration_since(UNIX_EPOCH).map_or(0, |d| {
        d.as_secs().saturating_add(u64::from(d.subsec_nanos() != 0))
    })
}

fn split(key: &VersionKey) -> ([u8; 8], [u8; 12]) {
    let mut cache = [0; 8];
    let mut version = [0; 12];
    cache.copy_from_slice(&key[..CACHE_HASH_BYTES]);
    version.copy_from_slice(&key[CACHE_HASH_BYTES..]);
    (cache, version)
}

fn page_key(version: &VersionKey, page: u32) -> Key {
    let (cache, hash) = split(version);
    Key::new(cache, hash, page)
}

fn low(version: &VersionKey) -> Bound<Key> {
    Bound::Included(page_key(version, 0))
}

fn high(version: &VersionKey) -> Bound<Key> {
    Bound::Included(page_key(version, u32::MAX))
}

/// Version key of a volume key.
fn key_version(key: &Key) -> VersionKey {
    let mut out = [0; 20];
    out.copy_from_slice(&key.0[..20]);
    out
}

impl Index {
    /// Keep recovered descriptors only for versions with pages on the volume.
    pub(super) fn restore_versions(&self, entries: Vec<VersionEntry>) {
        let volume = self.volume();
        let mut versions = self.versions.borrow_mut();
        versions.clear();
        let Some(volume) = volume else {
            return;
        };
        for entry in entries {
            let key = version_key(&entry.version);
            if !volume.range(low(&key), high(&key), 1).is_empty() {
                // A hash collision keeps the first entry; reads verify identity.
                let _ = versions.insert(entry);
            }
        }
    }

    pub(crate) fn volume(&self) -> Option<Rc<DiskVolume>> {
        self.volume.borrow().clone()
    }

    /// Whether the extent volume is mounted.
    pub fn is_mounted(&self) -> bool {
        self.volume.borrow().is_some()
    }

    /// Allocation limits for a volume of `slot_count` slots under the current
    /// hold percentage.
    pub(super) fn volume_limits(&self, slot_count: u64) -> Limits {
        Limits {
            storage_slots: self.hold_slot_limit(slot_count) as u64,
            small_bytes: u64::MAX,
        }
    }

    fn hold_slot_limit(&self, slot_count: u64) -> usize {
        let percent = self.holds.borrow().percent;
        let slots = usize::try_from(slot_count).unwrap_or(usize::MAX);
        (slots.saturating_mul(percent) / 100)
            .min(slots.saturating_sub(RESERVED_SLOTS as usize))
            .min(self.page_capacity().saturating_mul(percent) / 100)
    }

    fn hold_limit(&self) -> usize {
        self.volume()
            .map_or(0, |volume| self.hold_slot_limit(volume.slot_count()))
    }

    pub(super) fn update_volume_limits(&self) {
        if let Some(volume) = self.volume() {
            volume.set_limits(self.volume_limits(volume.slot_count()));
        }
    }

    /// Bind the mounted volume and rebuild hard holds from its live storage
    /// entries. Startup only, before requests are admitted.
    pub(crate) fn install_volume(&self, volume: Rc<DiskVolume>, report: &MountReport) {
        if report.formatted {
            eprintln!(
                "racer: worker {} formatted a new disk volume of {} slots",
                self.worker().0,
                volume.slot_count()
            );
        }
        if report.fell_back || report.root_lost || !report.lost.is_empty() {
            eprintln!(
                "racer: worker {} disk index repaired: fell_back={} root_lost={} lost_ranges={} salvaged={}",
                self.worker().0,
                report.fell_back,
                report.root_lost,
                report.lost.len(),
                report.salvaged
            );
        }
        if !report.dropped.is_empty() {
            eprintln!(
                "racer: worker {} disk index dropped {} malformed entries",
                self.worker().0,
                report.dropped.len()
            );
        }
        let wall = uring_runtime::environment::wall_now();
        let now = now_unix();
        let monotonic = uring_runtime::environment::now();
        let ceiling = now.saturating_add(MAX_HOLD_TTL.as_secs());
        let mut entries: HashMap<VersionKey, Hold> = HashMap::default();
        let mut clamped = 0u64;
        let mut cursor: Option<Key> = None;
        loop {
            let batch = volume.next_after(cursor.as_ref(), SCAN_BATCH);
            let Some((last, _)) = batch.last() else {
                break;
            };
            cursor = Some(*last);
            for (key, value) in batch {
                if value.tier != Tier::Storage || value.retain_until <= now {
                    continue;
                }
                let mut until_secs = value.retain_until;
                if until_secs > ceiling {
                    clamped += 1;
                    until_secs = ceiling;
                    volume.set_retain(&key, until_secs);
                }
                let until = UNIX_EPOCH + Duration::from_secs(until_secs);
                let ttl = until.duration_since(wall).unwrap_or_default();
                let Some(deadline) = monotonic.checked_add(ttl) else {
                    continue;
                };
                let hold = entries
                    .entry(key_version(&key))
                    .or_insert_with(|| Hold::new(until, deadline));
                hold.until = hold.until.max(until);
                hold.monotonic = hold.monotonic.max(deadline);
                hold.pages += 1;
                hold.published.insert(key.page());
            }
        }
        if clamped != 0 {
            eprintln!(
                "racer: worker {} clamped {clamped} disk hold deadlines to the maximum hold TTL",
                self.worker().0
            );
        }
        let pages: usize = entries.values().map(|hold| hold.pages).sum();
        self.holds.borrow_mut().entries = entries;
        *self.volume.borrow_mut() = Some(volume);
        let limit = self.hold_limit();
        if pages > limit {
            eprintln!(
                "racer: worker {} recovered {pages} held pages above the hold limit {limit}; new holds wait until expiry",
                self.worker().0
            );
        }
        self.update_volume_limits();
    }

    /// Volume entry of a page, if any, ignoring key availability.
    pub(super) fn disk_entry(&self, page: &PageId) -> Option<(Key, Value)> {
        let key = volume_key(page)?;
        let value = self.volume()?.get(&key)?;
        Some((key, value))
    }

    /// Whether some recorded page key of the version is still available. Unknown
    /// versions only require the cache to be available; reads check the header.
    fn version_available(&self, version: &ObjectVersion) -> bool {
        let cache = &version.object.cache;
        match self.versions.borrow().get_version(version) {
            Some(entry) => entry
                .key_ids
                .iter()
                .any(|key| self.page_available(cache, *key)),
            None => self.available(cache),
        }
    }

    /// Volume value of an available page on disk.
    pub fn lookup(&self, page: &PageId) -> Result<Option<Value>> {
        Ok(self
            .disk_entry(page)
            .filter(|_| self.version_available(&page.version))
            .map(|(_, value)| value))
    }

    /// Whether a write of `page` would add nothing: an available copy is on
    /// disk, durable enough for any live hold on it.
    pub(super) fn already_on_disk(&self, page: &PageId) -> Result<bool> {
        Ok(self
            .lookup(page)?
            .is_some_and(|value| value.tier == Tier::Storage || !self.is_held(page)))
    }

    /// Reserve a record, evicting at most [`EVICTION_BUDGET`] slots with the
    /// volume CLOCK. Fails with `Overloaded` when no space can be found.
    pub(super) fn allocate(&self, tier: Tier, length: u64) -> Result<Reservation> {
        let volume = self.volume().ok_or(Error::Unavailable)?;
        let (result, evicted) = volume.allocate_evicting(tier, length, now_unix(), EVICTION_BUDGET);
        self.forget_pages(&evicted, true);
        Ok(result?)
    }

    /// Next write id for a record header.
    pub(super) fn next_generation(&self) -> Result<u64> {
        Ok(self.volume().ok_or(Error::Unavailable)?.next_generation())
    }

    /// Drop retention owners and hold publications of removed entries.
    fn forget_pages(&self, entries: &[(Key, Value)], evicted: bool) {
        if entries.is_empty() {
            return;
        }
        let retention = self.retention();
        let mut residents = self.residents.borrow_mut();
        let mut holds = self.holds.borrow_mut();
        for (key, _) in entries {
            if let Some((resident, bytes)) = residents.remove(key)
                && evicted
            {
                retention.evicted(&resident.page, bytes, true);
            }
            if let Some(hold) = holds.entries.get_mut(&key_version(key)) {
                hold.published.remove(&key.page());
            }
        }
    }

    /// Publish a written record as `page`. Returns whether it was published;
    /// a revoked key drops the record instead.
    pub(super) fn publish_page(
        &self,
        page: &PageId,
        reservation: Reservation,
        length: u32,
        generation: u64,
        metadata: &VersionMetadata,
        key_id: KeyId,
    ) -> Result<bool> {
        if !self.page_available(&page.version.object.cache, key_id) {
            return Ok(false);
        }
        Self::validate_descriptor(metadata)?;
        let payload = u64::from(metadata.page_length(page)?);
        self.check_length(metadata)?;
        let key = volume_key(page).ok_or(Error::InvalidConfiguration)?;
        let volume = self.volume().ok_or(Error::Unavailable)?;
        let version = version_key(&page.version);
        let retain_until = if reservation.tier() == Tier::Storage {
            self.holds
                .borrow()
                .entries
                .get(&version)
                .filter(|hold| hold.live())
                .map_or(0, |hold| ceil_unix(hold.until))
        } else {
            0
        };
        volume.publish(key, reservation, length, generation, retain_until)?;
        if retain_until != 0
            && let Some(hold) = self.holds.borrow_mut().entries.get_mut(&version)
        {
            hold.published.insert(key.page());
        }
        self.learn_version(metadata, key_id);
        let mut residents = self.residents.borrow_mut();
        if let std::collections::hash_map::Entry::Vacant(slot) = residents.entry(key) {
            // Residency is best effort: heat tracking is bounded separately.
            if let Ok(mut resident) = self.track(page) {
                resident.payload = Some(self.retention().payload(payload, true));
                slot.insert((resident, payload));
            }
        }
        Ok(true)
    }

    /// Record `key_id` for a version, adding its descriptor when unknown. Used
    /// on publication and for record headers read back from disk whose version
    /// descriptor was not recovered.
    pub(super) fn learn_version(&self, metadata: &VersionMetadata, key_id: KeyId) {
        let version = version_key(&metadata.version);
        let mut versions = self.versions.borrow_mut();
        match versions.get_mut(&version) {
            Some(entry) if entry.version == metadata.version => entry.add_key_id(key_id),
            Some(_) => {}
            None => {
                let mut entry = VersionEntry::new(metadata.clone());
                entry.add_key_id(key_id);
                // A hash collision keeps the first version; reads verify identity.
                let _ = versions.insert(entry);
            }
        }
    }

    /// Remove `key` only while it still has `generation`, preserving replacements.
    pub(super) fn remove_if_generation(&self, key: &Key, generation: u64) {
        let Some(volume) = self.volume() else {
            return;
        };
        if let Some(value) = volume.get(key)
            && value.generation == generation
            && volume.remove_if(key, &value)
        {
            self.forget_pages(&[(*key, value)], false);
        }
    }

    /// Remove every disk page, hold, and version descriptor of one cache.
    pub(super) fn remove_disk_cache(&self, cache: &CacheId) {
        let hash = cache_hash(cache);
        self.holds
            .borrow_mut()
            .entries
            .retain(|version, _| version[..CACHE_HASH_BYTES] != hash[..]);
        self.versions.borrow_mut().remove_cache(&hash);
        if let Some(volume) = self.volume() {
            let removed = volume.remove_range(
                Bound::Included(Key::new(hash, [0; 12], 0)),
                Bound::Included(Key::new(hash, [0xff; 12], u32::MAX)),
            );
            self.forget_pages(&removed, false);
        }
    }

    /// Remove every disk page, hold, and version descriptor of caches outside
    /// `caches`. Run at startup after recovery selects the accepted cache set,
    /// since lazily committed removals may come back after a crash. Returns the
    /// count of removed entries.
    pub fn retain_caches(&self, caches: &[CacheId]) -> usize {
        let allowed: HashSet<[u8; CACHE_HASH_BYTES]> = caches.iter().map(cache_hash).collect();
        self.holds
            .borrow_mut()
            .entries
            .retain(|version, _| allowed.contains(&split(version).0));
        {
            let mut versions = self.versions.borrow_mut();
            let removed: Vec<_> = versions
                .iter()
                .filter(|(key, _)| !allowed.contains(&split(key).0))
                .map(|(key, _)| *key)
                .collect();
            for key in removed {
                versions.remove(&key);
            }
        }
        let Some(volume) = self.volume() else {
            return 0;
        };
        let mut removed = 0;
        let mut cursor: Option<Key> = None;
        loop {
            let batch = volume.next_after(cursor.as_ref(), SCAN_BATCH);
            let Some((last, _)) = batch.last() else {
                break;
            };
            cursor = Some(*last);
            let mut doomed = Vec::new();
            for (key, value) in batch {
                if !allowed.contains(&key.cache()) {
                    doomed.push((key, value));
                }
            }
            for (key, value) in &doomed {
                volume.remove_if(key, value);
            }
            removed += doomed.len();
            self.forget_pages(&doomed, false);
        }
        removed
    }

    /// Make every disk index change so far durable. A no-op before mount.
    pub fn commit<'a>(
        &'a self,
        reactor: &'a Reactor,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        let volume = self.volume();
        Box::pin(async move {
            let Some(volume) = volume else {
                return Ok(());
            };
            // Callers classify failures with `is_disposable_commit_error`.
            volume.commit(reactor, scope).await
        })
    }

    /// Whether the volume has index changes that are not durable yet.
    pub fn is_dirty(&self) -> bool {
        self.volume().is_some_and(|volume| volume.is_dirty())
    }

    /// Entries in the volume index.
    pub fn disk_len(&self) -> u64 {
        self.volume().map_or(0, |volume| volume.len())
    }

    /// Every page on this worker's disk whose version is known, for
    /// diagnostics and tests. Scans the whole index.
    pub fn disk_pages(&self) -> Vec<DiskPage> {
        let Some(volume) = self.volume() else {
            return Vec::new();
        };
        let versions = self.versions.borrow();
        let mut pages = Vec::new();
        let mut cursor: Option<Key> = None;
        loop {
            let batch = volume.next_after(cursor.as_ref(), SCAN_BATCH);
            let Some((last, _)) = batch.last() else {
                break;
            };
            cursor = Some(*last);
            for (key, value) in batch {
                let Some(entry) = versions.get(&key_version(&key)) else {
                    continue;
                };
                let Some(key_id) = entry.key_ids.first() else {
                    continue;
                };
                pages.push(DiskPage {
                    page: PageId {
                        version: entry.version.clone(),
                        number: PageNumber(u64::from(key.page())),
                    },
                    key_id: *key_id,
                    offset: value.offset(),
                    length: u64::from(value.length),
                });
            }
        }
        pages
    }

    /// Walk disk pages in key order: up to `limit` index entries after
    /// `after`. Returns the last visited key (`None` once the index is
    /// exhausted) and the pages among them whose version is known.
    pub fn resident_pages_after(
        &self,
        after: Option<&Key>,
        limit: usize,
    ) -> (Option<Key>, Vec<PageId>) {
        let Some(volume) = self.volume() else {
            return (None, Vec::new());
        };
        let batch = volume.next_after(after, limit);
        let cursor = batch.last().map(|(key, _)| *key);
        let versions = self.versions.borrow();
        let pages = batch
            .into_iter()
            .filter_map(|(key, _)| {
                versions.get(&key_version(&key)).map(|entry| PageId {
                    version: entry.version.clone(),
                    number: PageNumber(u64::from(key.page())),
                })
            })
            .collect();
        (cursor, pages)
    }

    /// Bytes of each slot range, in order: `(size, used)` per group of
    /// `counts[i]` consecutive slots. O(groups * log slots).
    pub(crate) fn disk_usage(&self, counts: &[usize]) -> Vec<(u64, u64)> {
        let Some(volume) = self.volume() else {
            return Vec::new();
        };
        let mut start = 0u32;
        counts
            .iter()
            .map(|count| {
                let end = start.saturating_add(u32::try_from(*count).unwrap_or(u32::MAX));
                let used = volume.used_bytes(start..end);
                start = end;
                (*count as u64 * SLOT_BYTES, used)
            })
            .collect()
    }

    /// Startup only. Cap hard holds as a percentage of both data slots and disk
    /// page entries. The default is 50. One reserved page charges one data slot.
    /// Holds recovered from the volume stay; the new limit applies to new holds.
    pub fn set_hold_capacity_percent(&self, percent: usize) -> Result<()> {
        if percent > 100 {
            return Err(Error::InvalidConfiguration);
        }
        self.holds.borrow_mut().percent = percent;
        self.update_volume_limits();
        Ok(())
    }

    /// Reserve this worker's page count at HEAD, before fetching any payload.
    /// Repeated versions share capacity; extension never shortens an existing hold.
    /// Existing storage-tier pages are protected atomically, or this call fails
    /// unchanged. Cache-tier pages are protected once rewritten by `persist_held`.
    ///
    /// Returns whether the `retain_until` of a storage-tier page was raised.
    /// That change is in memory only until the next volume commit (the lazy
    /// commit runs about once a second); a crash before it rebuilds the hold
    /// from the previous deadline. Callers that report the extension as
    /// durable must commit first.
    pub fn reserve_hold(
        &self,
        version: &ObjectVersion,
        pages: usize,
        until: SystemTime,
    ) -> Result<bool> {
        let monotonic = Holds::deadline(until)?;
        if !self.available(&version.object.cache) {
            return Err(Error::MissingKey);
        }
        let limit = self.hold_limit();
        let key = version_key(version);
        let existing: Vec<(Key, Value)> = self.volume().map_or_else(Vec::new, |volume| {
            volume
                .range(low(&key), high(&key), usize::MAX)
                .into_iter()
                .filter(|(_, value)| value.tier == Tier::Storage)
                .collect()
        });
        let mut holds = self.holds.borrow_mut();
        holds.expire();
        let old = holds.entries.get(&key);
        let mut published: HashSet<u32> = existing.iter().map(|(key, _)| key.page()).collect();
        if let Some(old) = old {
            published.extend(old.published.iter().copied());
        }
        let count = pages
            .max(published.len())
            .max(old.map_or(0, |hold| hold.pages));
        // Zero-page objects need no disk guarantee or retained reservation entry.
        if count == 0 {
            return Ok(false);
        }
        let total = holds
            .entries
            .values()
            .try_fold(0usize, |sum, hold| sum.checked_add(hold.pages))
            .ok_or(Error::Overloaded)?;
        let total = total
            .checked_sub(old.map_or(0, |hold| hold.pages))
            .and_then(|n| n.checked_add(count))
            .ok_or(Error::Overloaded)?;
        if total > limit {
            return Err(Error::Overloaded);
        }
        let inflight = old.map_or_else(
            || Rc::new(std::cell::Cell::new(0)),
            |hold| hold.inflight.clone(),
        );
        let active = old.map_or_else(
            || Rc::new(std::cell::Cell::new(0)),
            |hold| hold.active.clone(),
        );
        let until = old.map_or(until, |hold| hold.until.max(until));
        let monotonic = old.map_or(monotonic, |hold| hold.monotonic.max(monotonic));
        holds.entries.insert(
            key,
            Hold {
                until,
                monotonic,
                pages: count,
                inflight,
                active,
                published,
            },
        );
        drop(holds);
        let mut raised = false;
        if let Some(volume) = self.volume() {
            let retain = ceil_unix(until);
            for (key, value) in existing {
                if value.retain_until < retain {
                    volume.set_retain(&key, retain);
                    raised = true;
                }
            }
        }
        Ok(raised)
    }

    /// Active reservation deadline, not proof that every requested page is on disk.
    pub fn hold_expiry(&self, version: &ObjectVersion) -> Option<SystemTime> {
        self.holds
            .borrow()
            .entries
            .get(&version_key(version))
            .filter(|hold| hold.live())
            .map(|hold| hold.until)
    }

    /// Scan only bounded hold publications, not the full index or payloads.
    /// Scratch space contains distinct slots, bounded by held publications.
    pub fn held_snapshot(&self) -> HeldSnapshot {
        let Some(volume) = self.volume() else {
            return HeldSnapshot::default();
        };
        let holds = self.holds.borrow();
        let versions = self.versions.borrow();
        let mut snapshot = HeldSnapshot::default();
        let mut slots = HashSet::default();
        for (version, hold) in holds.entries.iter().filter(|(_, hold)| hold.live()) {
            let known = versions.get(version);
            for number in &hold.published {
                let Some(value) = volume.get(&page_key(version, *number)) else {
                    continue;
                };
                if value.tier != Tier::Storage {
                    continue;
                }
                let bytes = known
                    .and_then(|entry| {
                        entry
                            .metadata
                            .page_length(&PageId {
                                version: entry.version.clone(),
                                number: PageNumber(u64::from(*number)),
                            })
                            .ok()
                    })
                    .map_or(u64::from(value.length), u64::from);
                snapshot.pages = snapshot.pages.saturating_add(1);
                snapshot.payload_bytes = snapshot.payload_bytes.saturating_add(bytes);
                slots.insert(value.slot);
            }
        }
        snapshot.slots = slots.len() as u64;
        snapshot
    }

    /// Extend an existing reservation without adding page capacity. Returns
    /// whether a storage-tier deadline changed; see [`Index::reserve_hold`] for
    /// when that becomes durable.
    pub fn extend_hold(&self, version: &ObjectVersion, until: SystemTime) -> Result<bool> {
        let pages = self
            .holds
            .borrow()
            .entries
            .get(&version_key(version))
            .filter(|hold| hold.live())
            .map(|hold| hold.pages)
            .ok_or(Error::Unavailable)?;
        self.reserve_hold(version, pages, until)
    }

    /// Release only unused capacity after a partial failure. Completed pages stay
    /// held until the deadline. Call after this version's persistence futures and
    /// pending writes have finished.
    pub fn release_hold(&self, version: &ObjectVersion) -> Result<()> {
        let key = version_key(version);
        let mut holds = self.holds.borrow_mut();
        if let Some(hold) = holds.entries.get_mut(&key) {
            if hold.inflight.get() != 0 || hold.active.get() != 0 {
                return Err(Error::Overloaded);
            }
            hold.pages = hold.published.len();
            if hold.pages == 0 {
                holds.entries.remove(&key);
            }
        }
        Ok(())
    }

    pub fn is_held(&self, page: &PageId) -> bool {
        self.hold_expiry(&page.version).is_some()
    }

    pub(super) fn protect_hold(&self, version: &ObjectVersion) -> Result<HoldGuard> {
        let holds = self.holds.borrow();
        let hold = holds
            .entries
            .get(&version_key(version))
            .filter(|hold| hold.live())
            .ok_or(Error::Unavailable)?;
        hold.active
            .set(hold.active.get().checked_add(1).ok_or(Error::Overloaded)?);
        Ok(HoldGuard(hold.active.clone()))
    }

    /// Deadline of a published held page. A HEAD reservation alone returns None.
    pub fn held_page_expiry(&self, page: &PageId) -> Option<SystemTime> {
        let number = u32::try_from(page.number.0).ok()?;
        self.holds
            .borrow()
            .entries
            .get(&version_key(&page.version))
            .filter(|hold| hold.live() && hold.published.contains(&number))
            .map(|hold| hold.until)
    }

    /// Take one unused page of a live hold for a storage-tier write of `page`,
    /// or `None` when the page must be written to the cache tier.
    pub(super) fn hold_ticket(&self, page: &PageId) -> Option<HoldTicket> {
        let number = u32::try_from(page.number.0).ok()?;
        let holds = self.holds.borrow();
        let hold = holds
            .entries
            .get(&version_key(&page.version))
            .filter(|hold| {
                hold.live() && !hold.published.contains(&number) && hold.unused() != 0
            })?;
        hold.inflight.set(hold.inflight.get() + 1);
        Some(HoldTicket(hold.inflight.clone()))
    }
}
