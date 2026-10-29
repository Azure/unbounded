//! Volume-backed disk pages: the per-worker extent volume, the hard hold table
//! over its storage tier, and the version table that maps key hashes back to
//! full versions.
//!
//! The index exists before `Store::open` mounts the volume
//! ([`Index::install_volume`]): it is wired into readers, writers, and recovery
//! at construction. Until the mount the disk is empty, so lookups miss and
//! allocations fail with `Unavailable`.
//!
//! The volume's index lives on disk behind a bounded page cache, so lookups
//! that miss the cache read pages. Async paths (`lookup`, `allocate`,
//! `already_on_disk`, holds) await those reads. Synchronous callers use
//! memory-only answers ([`Index::already_on_disk_cached`],
//! [`Index::resident_pages_after`], [`Index::held_snapshot`]) and queue the
//! rest as maintenance, which the lazy committer runs
//! ([`Index::maintain`]): cache removals, record invalidations, page
//! prefetches, the startup restore of version descriptors and residency, and
//! a small reserve of free slots evicted ahead of allocation.

use super::catalog::DiskPage;
use super::catalog::HeldSnapshot;
use super::catalog::Index;
use super::holds::{
    Hold, HoldEpoch, HoldGuard, HoldPages, HoldTicket, HolderKind, Holds, MAX_HOLD_TTL, PageHolders,
};
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

/// Index entries visited per batch by scans.
const SCAN_BATCH: usize = 4096;

/// Slots kept out of the hold quota: the meta slot and one cache slot, so cache
/// writes and commits can always make progress.
const RESERVED_SLOTS: u64 = 2;

/// Free slots maintenance keeps ahead of allocation, so foreground writes
/// rarely evict.
const FREE_SLOT_RESERVE: u64 = 2;

/// Slots maintenance evicts per run to keep the reserve.
const RESERVE_EVICTIONS: usize = 2;

/// Version descriptors the startup restore handles per maintenance run.
const RESTORE_BATCH: usize = 256;

/// Most queued page prefetches and invalidations kept; more are dropped
/// (prefetches) or retried by the reader (invalidations).
const MAINTENANCE_QUEUE: usize = 4096;

/// Default byte budget of the volume's metadata page cache per worker.
pub const DEFAULT_METADATA_CACHE_BYTES: usize = 256 << 20;

/// How long a test scope lives.
#[cfg(test)]
const MAINTENANCE_TIMEOUT: Duration = Duration::from_secs(600);

/// Work queued by synchronous callers for the async maintenance pass.
#[derive(Default)]
pub(crate) struct Maintenance {
    /// Caches whose pages are being removed: cache hash, the first
    /// generation not covered (newer pages are kept), and the resume cursor.
    removals: Vec<([u8; CACHE_HASH_BYTES], u64, Option<Key>)>,
    /// Entries to remove while they still have this generation.
    invalidations: Vec<(Key, u64)>,
    /// Index keys whose pages memory-only callers needed.
    prefetch: Vec<Key>,
    /// Version descriptors restored at startup and not yet checked against
    /// the volume.
    restore: Vec<VersionKey>,
    /// Storage entries whose last holder was released: their `retain_until`
    /// is cleared unless a hold took them again.
    unretain: Vec<Key>,
}

impl Maintenance {
    fn is_empty(&self) -> bool {
        self.removals.is_empty()
            && self.invalidations.is_empty()
            && self.prefetch.is_empty()
            && self.restore.is_empty()
            && self.unretain.is_empty()
    }
}

/// Seconds since the Unix epoch, now.
fn now_unix() -> u64 {
    uring_runtime::environment::wall_now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

/// Eviction time for wall time `wall`, read at monotonic `now`, given the
/// previous reading `anchor`. Storage entries are kept until their
/// `retain_until` passes this time. It follows the wall clock, except that it
/// never runs ahead of the monotonic time elapsed since the anchor: a held page
/// stays live until both clocks pass its deadline, so a forward wall-clock jump
/// must not let eviction remove it early. After such a jump the result lags the
/// wall clock by the jump, which only retains storage slots longer. Returns the
/// time and the next anchor.
fn eviction_time(
    anchor: Option<(u64, std::time::Instant)>,
    wall: u64,
    now: std::time::Instant,
) -> (u64, (u64, std::time::Instant)) {
    match anchor {
        Some((secs, at)) => {
            let limit = secs.saturating_add(now.saturating_duration_since(at).as_secs());
            if wall <= limit {
                (wall, (wall, now))
            } else {
                (limit, (secs, at))
            }
        }
        None => (wall, (wall, now)),
    }
}

/// Whole seconds since the Unix epoch, rounded up, so protection never ends early.
pub(super) fn ceil_unix(time: SystemTime) -> u64 {
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

/// A scope for test wrappers.
#[cfg(test)]
fn maintenance_scope() -> Result<RequestScope> {
    RequestScope::new(
        crate::model::RequestId([0; 16]),
        uring_runtime::environment::now() + MAINTENANCE_TIMEOUT,
    )
}

impl Index {
    pub(crate) fn volume(&self) -> Option<Rc<DiskVolume>> {
        self.volume.borrow().clone()
    }

    /// The mounted volume and the reactor its I/O runs on.
    fn io(&self) -> Option<(Rc<DiskVolume>, Rc<Reactor>)> {
        Some((self.volume()?, self.reactor.borrow().clone()?))
    }

    /// Eviction time in Unix seconds; see [`eviction_time`].
    fn eviction_now(&self) -> u64 {
        let (now, anchor) = eviction_time(
            self.eviction_clock.get(),
            now_unix(),
            uring_runtime::environment::now(),
        );
        self.eviction_clock.set(Some(anchor));
        now
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

    /// This worker's payload capacity for fair shares: the tighter of disk
    /// page entries times a full page and volume slots times slot bytes.
    /// Either one bounds how much payload the worker can keep resident. Slot
    /// space is ignored until the volume is mounted.
    pub fn disk_capacity(&self) -> u64 {
        let pages = (self.page_capacity() as u64).saturating_mul(crate::model::PAGE_BYTES);
        self.volume().map_or(pages, |volume| {
            pages.min(volume.slot_count().saturating_mul(SLOT_BYTES))
        })
    }

    /// Whether free slots are down to the reserve maintenance keeps ahead of
    /// allocation, so new pages displace resident ones.
    pub fn free_slots_at_reserve(&self) -> bool {
        self.volume()
            .is_some_and(|volume| volume.free_slots() <= FREE_SLOT_RESERVE)
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
    ///
    /// Only storage slots whose retention bound has not passed can hold live
    /// holds; their entries come from the volume's record map, so this reads
    /// no cache-tier index pages.
    pub(crate) async fn install_volume(
        &self,
        volume: Rc<DiskVolume>,
        reactor: Rc<Reactor>,
        report: &MountReport,
        scope: &RequestScope,
    ) -> Result<()> {
        if report.formatted {
            eprintln!(
                "racer: worker {} formatted a new disk volume of {} slots",
                self.worker().0,
                volume.slot_count()
            );
        }
        if report.fell_back || report.root_lost || !report.lost.is_empty() {
            eprintln!(
                "racer: worker {} disk metadata repaired: fell_back={} root_lost={} lost_ranges={} rebuilt_bitmaps={}",
                self.worker().0,
                report.fell_back,
                report.root_lost,
                report.lost.len(),
                report.rebuilt_bitmaps
            );
        }
        let wall = uring_runtime::environment::wall_now();
        let now = now_unix();
        let monotonic = uring_runtime::environment::now();
        let ceiling = now.saturating_add(MAX_HOLD_TTL.as_secs());
        let mut entries: HashMap<VersionKey, Hold> = HashMap::default();
        let mut clamped = 0u64;
        for slot in volume.retained_storage_slots(now.saturating_add(1)) {
            for (key, value) in volume.slot_entries(&*reactor, slot, scope).await? {
                if value.tier != Tier::Storage || value.retain_until <= now {
                    continue;
                }
                let mut until_secs = value.retain_until;
                if until_secs > ceiling {
                    clamped += 1;
                    until_secs = ceiling;
                    volume
                        .set_retain(&*reactor, &key, until_secs, scope)
                        .await?;
                }
                let until = UNIX_EPOCH + Duration::from_secs(until_secs);
                let ttl = until.duration_since(wall).unwrap_or_default();
                let Some(deadline) = monotonic.checked_add(ttl) else {
                    continue;
                };
                let hold = entries
                    .entry(key_version(&key))
                    .or_insert_with(|| Hold::new(until, deadline, HoldEpoch::default()));
                hold.until = hold.until.max(until);
                hold.monotonic = hold.monotonic.max(deadline);
                hold.pages.insert(PageNumber(u64::from(key.page())));
                hold.published.insert(key.page());
            }
        }
        if clamped != 0 {
            eprintln!(
                "racer: worker {} clamped {clamped} disk hold deadlines to the maximum hold TTL",
                self.worker().0
            );
        }
        let pages: usize = entries.values().map(|hold| hold.pages.len()).sum();
        {
            let mut holds = self.holds.borrow_mut();
            // Restored entries are new lifetimes; releases from before cannot
            // match. Holder identities do not survive restart; each restored
            // page keeps one reader holder until expiry or `release_hold_pages`.
            for hold in entries.values_mut() {
                hold.epoch = holds.next_epoch();
                hold.holders = Hold::single_holders(&hold.pages, hold.epoch);
            }
            holds.entries = entries;
        }
        *self.volume.borrow_mut() = Some(volume);
        *self.reactor.borrow_mut() = Some(reactor);
        let limit = self.hold_limit();
        if pages > limit {
            eprintln!(
                "racer: worker {} recovered {pages} held pages above the hold limit {limit}; new holds wait until expiry",
                self.worker().0
            );
        }
        self.update_volume_limits();
        Ok(())
    }

    /// Keep recovered descriptors; the next maintenance runs check each one
    /// against the volume, a batch at a time, dropping versions without
    /// pages and restoring residency for the pages of the others.
    pub(super) fn restore_versions(&self, entries: Vec<VersionEntry>) {
        let mut versions = self.versions.borrow_mut();
        versions.clear();
        let mut queued = Vec::with_capacity(entries.len());
        for entry in entries {
            let key = version_key(&entry.version);
            // A hash collision keeps the first entry; reads verify identity.
            if versions.insert(entry).is_ok() {
                queued.push(key);
            }
        }
        drop(versions);
        self.maintenance.borrow_mut().restore = queued;
    }

    /// Check one restored descriptor: drop it when the version has no pages
    /// on the volume, else track retention residency of its pages.
    async fn restore_version(
        &self,
        version: VersionKey,
        volume: &DiskVolume,
        reactor: &Reactor,
        scope: &RequestScope,
    ) -> Result<()> {
        let mut cursor = low(&version);
        let mut found = false;
        loop {
            let batch = volume
                .range(reactor, cursor, high(&version), SCAN_BATCH, scope)
                .await?;
            let Some((last, _)) = batch.last().copied() else {
                break;
            };
            found = true;
            cursor = Bound::Excluded(last);
            let versions = self.versions.borrow();
            let Some(entry) = versions.get(&version) else {
                return Ok(());
            };
            let retention = self.retention();
            let mut residents = self.residents.borrow_mut();
            for (key, _) in batch {
                let page = PageId {
                    version: entry.version.clone(),
                    number: PageNumber(u64::from(key.page())),
                };
                let Ok(payload) = entry.metadata.page_length(&page) else {
                    continue;
                };
                if let std::collections::hash_map::Entry::Vacant(slot) = residents.entry(key)
                    && let Ok(mut resident) = self.track(&page)
                {
                    let payload = u64::from(payload);
                    resident.payload =
                        Some(retention.indexed_payload(&page.version.object.cache, payload));
                    slot.insert((resident, payload));
                }
            }
        }
        if !found {
            self.versions.borrow_mut().remove(&version);
        }
        Ok(())
    }

    /// Whether `value` of `key` belongs to a cache removal still in progress
    /// (it reads as absent until maintenance deletes it).
    fn removing(&self, key: &Key, value: &Value) -> bool {
        let cache = key.cache();
        self.maintenance
            .borrow()
            .removals
            .iter()
            .any(|(hash, bound, _)| *hash == cache && value.generation < *bound)
    }

    /// Volume entry of a page, if any, ignoring key availability.
    pub(super) async fn disk_entry(
        &self,
        page: &PageId,
        scope: &RequestScope,
    ) -> Result<Option<(Key, Value)>> {
        let Some(key) = volume_key(page) else {
            return Ok(None);
        };
        let Some((volume, reactor)) = self.io() else {
            return Ok(None);
        };
        Ok(volume
            .get(&*reactor, &key, scope)
            .await?
            .filter(|value| !self.removing(&key, value))
            .map(|value| (key, value)))
    }

    /// Volume entry of a page from memory only: `Ok(None)` when it is not on
    /// disk, `Err(Unavailable)` when answering needs a page read.
    fn disk_entry_cached(&self, page: &PageId) -> Result<Option<(Key, Value)>> {
        let Some(key) = volume_key(page) else {
            return Ok(None);
        };
        let Some(volume) = self.volume() else {
            return Ok(None);
        };
        match volume.try_get(&key) {
            Some(value) => Ok(value
                .filter(|value| !self.removing(&key, value))
                .map(|value| (key, value))),
            None => {
                self.queue_prefetch(key);
                Err(Error::Unavailable)
            }
        }
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
    pub async fn lookup_value(&self, page: &PageId, scope: &RequestScope) -> Result<Option<Value>> {
        Ok(self
            .disk_entry(page, scope)
            .await?
            .filter(|_| self.version_available(&page.version))
            .map(|(_, value)| value))
    }

    /// Whether an available copy of `page` is on disk, from memory only:
    /// `false` when that needs a page read (a prefetch is queued).
    pub fn on_disk_cached(&self, page: &PageId) -> bool {
        self.disk_entry_cached(page)
            .ok()
            .flatten()
            .is_some_and(|_| self.version_available(&page.version))
    }

    /// Whether a write of `page` would add nothing: an available copy is on
    /// disk, durable enough for any live hold on it.
    pub(super) async fn already_on_disk_async(
        &self,
        page: &PageId,
        scope: &RequestScope,
    ) -> Result<bool> {
        Ok(self
            .lookup_value(page, scope)
            .await?
            .is_some_and(|value| value.tier == Tier::Storage || !self.is_held(page)))
    }

    /// [`Index::already_on_disk_async`] from memory only. Answers `false`
    /// when that needs a page read (the writer checks again before writing)
    /// and queues a prefetch.
    pub(super) fn already_on_disk_cached(&self, page: &PageId) -> bool {
        self.disk_entry_cached(page)
            .ok()
            .flatten()
            .is_some_and(|(_, value)| {
                self.version_available(&page.version)
                    && (value.tier == Tier::Storage || !self.is_held(page))
            })
    }

    /// Reserve a record, committing first when the volume's dirty limit is
    /// reached, then reopening partly filled slots and evicting at most
    /// [`EVICTION_BUDGET`] slots with the volume CLOCK. Fails with
    /// `Overloaded` when no space can be found.
    pub(super) async fn allocate(
        &self,
        tier: Tier,
        length: u64,
        scope: &RequestScope,
    ) -> Result<Reservation> {
        let (volume, reactor) = self.io().ok_or(Error::Unavailable)?;
        // A full overlay of uncommitted changes makes allocation return
        // `Busy` until a commit drains it. Commit here (concurrent writers
        // share the attempt) instead of failing or spinning.
        if volume.needs_commit() {
            volume.commit(&*reactor, scope).await?;
        }
        self.reserve_stuck.set(false);
        let allocated = volume
            .allocate_evicting(
                &*reactor,
                tier,
                length,
                self.eviction_now(),
                EVICTION_BUDGET,
                scope,
            )
            .await;
        // A failed eviction may have removed entries before it stopped.
        self.forget_pages(&volume.take_evicted(), true);
        let (result, evicted) = allocated?;
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
                .filter(|hold| hold.live() && hold.pages.contains(page.number))
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
                resident.payload = Some(
                    self.retention()
                        .indexed_payload(&page.version.object.cache, payload),
                );
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

    /// Remove `key` only while it still has `generation`, preserving
    /// replacements. Done at once when the entry is in memory (it normally
    /// is: the caller just read it); otherwise queued for maintenance.
    pub(super) fn remove_if_generation(&self, key: &Key, generation: u64) {
        let Some(volume) = self.volume() else {
            return;
        };
        match volume.try_get(key) {
            Some(Some(value)) if value.generation == generation => {
                if volume.remove_cached(key, &value) {
                    self.forget_pages(&[(*key, value)], false);
                }
            }
            Some(_) => {}
            None => {
                let mut maintenance = self.maintenance.borrow_mut();
                if maintenance.invalidations.len() < MAINTENANCE_QUEUE {
                    maintenance.invalidations.push((*key, generation));
                }
            }
        }
    }

    /// Remove every disk page, hold, and version descriptor of one cache.
    /// Holds and descriptors go at once, which makes the pages unavailable
    /// to lookups; maintenance removes the index entries in batches. Pages
    /// published after this call (with a newer generation) are kept.
    pub(super) fn remove_disk_cache(&self, cache: &CacheId) {
        let hash = cache_hash(cache);
        self.holds
            .borrow_mut()
            .entries
            .retain(|version, _| version[..CACHE_HASH_BYTES] != hash[..]);
        self.versions.borrow_mut().remove_cache(&hash);
        if let Some(volume) = self.volume() {
            let bound = volume.next_generation();
            self.maintenance
                .borrow_mut()
                .removals
                .push((hash, bound, None));
        }
    }

    /// Remove every disk page, hold, and version descriptor of caches outside
    /// `caches`. Run at startup after recovery selects the accepted cache set,
    /// since lazily committed removals may come back after a crash. Returns the
    /// count of removed entries.
    pub async fn retain_caches(&self, caches: &[CacheId], scope: &RequestScope) -> Result<usize> {
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
        let Some((volume, reactor)) = self.io() else {
            return Ok(0);
        };
        let mut removed = 0;
        let mut cursor: Option<Key> = None;
        loop {
            let batch = volume
                .next_after(&*reactor, cursor.as_ref(), SCAN_BATCH, scope)
                .await?;
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
                volume.remove_if(&*reactor, key, value, scope).await?;
            }
            removed += doomed.len();
            self.forget_pages(&doomed, false);
        }
        Ok(removed)
    }

    /// Queue a page prefetch for a memory-only caller.
    fn queue_prefetch(&self, key: Key) {
        let mut maintenance = self.maintenance.borrow_mut();
        if maintenance.prefetch.len() < MAINTENANCE_QUEUE && !maintenance.prefetch.contains(&key) {
            maintenance.prefetch.push(key);
        }
    }

    /// Whether work is queued for [`Index::maintain`] (not counting the
    /// free-slot reserve).
    #[cfg(test)]
    pub(crate) fn maintenance_queued(&self) -> bool {
        self.is_mounted() && !self.maintenance.borrow().is_empty()
    }

    /// Whether queued maintenance waits for [`Index::maintain`], or the
    /// free-slot reserve is low and eviction may refill it.
    pub fn maintenance_due(&self) -> bool {
        self.volume().is_some_and(|volume| {
            !self.maintenance.borrow().is_empty()
                || (volume.free_slots() < FREE_SLOT_RESERVE && !self.reserve_stuck.get())
        })
    }

    /// Run queued maintenance, bounded per call: invalidations, one batch of
    /// cache removal, one batch of the startup restore, page prefetches, and
    /// (when `evict` is set) evictions that keep a few slots free ahead of
    /// allocation.
    pub async fn maintain(&self, scope: &RequestScope, evict: bool) -> Result<()> {
        let Some((volume, reactor)) = self.io() else {
            return Ok(());
        };
        let reactor = &*reactor;
        let invalidations = std::mem::take(&mut self.maintenance.borrow_mut().invalidations);
        for (key, generation) in invalidations {
            if let Some(value) = volume.get(reactor, &key, scope).await?
                && value.generation == generation
                && volume.remove_if(reactor, &key, &value, scope).await?
            {
                self.forget_pages(&[(key, value)], false);
            }
        }
        let removal = self.maintenance.borrow_mut().removals.first().copied();
        if let Some((hash, bound, cursor)) = removal {
            let low = cursor.map_or(Bound::Included(Key::new(hash, [0; 12], 0)), Bound::Excluded);
            let batch = volume
                .range(
                    reactor,
                    low,
                    Bound::Included(Key::new(hash, [0xff; 12], u32::MAX)),
                    SCAN_BATCH,
                    scope,
                )
                .await?;
            let mut removed = Vec::new();
            for (key, value) in &batch {
                if value.generation < bound && volume.remove_if(reactor, key, value, scope).await? {
                    removed.push((*key, *value));
                }
            }
            self.forget_pages(&removed, false);
            let mut maintenance = self.maintenance.borrow_mut();
            if let Some(entry) = maintenance.removals.iter_mut().find(|e| e.0 == hash) {
                match batch.last() {
                    Some((last, _)) if batch.len() == SCAN_BATCH => entry.2 = Some(*last),
                    _ => {
                        maintenance.removals.retain(|e| e.0 != hash);
                    }
                }
            }
        }
        let restore: Vec<VersionKey> = {
            let mut maintenance = self.maintenance.borrow_mut();
            let take = maintenance.restore.len().min(RESTORE_BATCH);
            maintenance.restore.drain(..take).collect()
        };
        for version in restore {
            self.restore_version(version, &volume, reactor, scope)
                .await?;
        }
        let unretain = std::mem::take(&mut self.maintenance.borrow_mut().unretain);
        for key in unretain {
            volume.prefetch(reactor, &key, scope).await?;
            // Best effort: a page read out of the cache again keeps its
            // deadline, which still bounds how long its slot stays retained.
            let _ = self.unretain_cached(&volume, &key);
        }
        let prefetch = std::mem::take(&mut self.maintenance.borrow_mut().prefetch);
        for key in prefetch {
            volume.prefetch(reactor, &key, scope).await?;
        }
        if evict && !volume.needs_commit() {
            let evicted = volume
                .reserve_free(
                    reactor,
                    FREE_SLOT_RESERVE,
                    self.eviction_now(),
                    RESERVE_EVICTIONS,
                    scope,
                )
                .await;
            // A failed eviction may have removed entries before it stopped.
            self.forget_pages(&volume.take_evicted(), true);
            let evicted = evicted?;
            self.reserve_stuck
                .set(volume.free_slots() < FREE_SLOT_RESERVE && evicted.is_empty());
            self.forget_pages(&evicted, true);
        }
        Ok(())
    }

    /// Make every disk index change so far durable, after running queued
    /// invalidations, removals, and restore work. A no-op before mount.
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
            self.maintain(scope, false).await?;
            // Callers classify failures with `is_disposable_commit_error`.
            volume.commit(reactor, scope).await
        })
    }

    /// Whether the volume has commit work (index changes that are not
    /// durable yet, or freed space that only later commits release), or
    /// maintenance is queued.
    pub fn is_dirty(&self) -> bool {
        self.volume().is_some_and(|volume| volume.is_dirty()) || self.maintenance_due()
    }

    /// Whether uncommitted index changes reached the volume's dirty limit:
    /// allocation waits for a commit.
    pub fn needs_commit(&self) -> bool {
        self.volume().is_some_and(|volume| volume.needs_commit())
    }

    /// Entries in the volume index. Exact after a commit.
    pub fn disk_len(&self) -> u64 {
        self.volume().map_or(0, |volume| volume.len())
    }

    /// Every page on this worker's disk whose version is known, for
    /// diagnostics and tests. Scans the whole index from memory only: it
    /// stops at the first index page that is not cached (queuing a prefetch),
    /// so the list is complete only when the index is cached.
    pub fn disk_pages(&self) -> Vec<DiskPage> {
        let Some(volume) = self.volume() else {
            return Vec::new();
        };
        let mut pages = Vec::new();
        let mut cursor: Option<Key> = None;
        loop {
            let low = cursor.map_or(Bound::Unbounded, Bound::Excluded);
            let Some(batch) = volume.try_range(low, Bound::Unbounded, SCAN_BATCH) else {
                self.queue_prefetch(cursor.unwrap_or(Key::MIN));
                break;
            };
            let Some((last, _)) = batch.last() else {
                break;
            };
            cursor = Some(*last);
            let versions = self.versions.borrow();
            for (key, value) in batch {
                if self.removing(&key, &value) {
                    continue;
                }
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

    /// Index entries on the disk volume, including unknown versions.
    pub fn resident_page_count(&self) -> u64 {
        self.disk_len()
    }

    /// Walk disk pages in key order: up to `limit` index entries after
    /// `after`. Returns the last visited key (`None` once the index is
    /// exhausted) and the pages among them whose version is known.
    ///
    /// Memory only: when the next entries are not cached this returns
    /// `after` unchanged and no pages, and queues a prefetch for
    /// maintenance.
    pub fn resident_pages_after(
        &self,
        after: Option<&Key>,
        limit: usize,
    ) -> (Option<Key>, Vec<PageId>) {
        let Some(volume) = self.volume() else {
            return (None, Vec::new());
        };
        let low = after.map_or(Bound::Unbounded, |key| Bound::Excluded(*key));
        let Some(batch) = volume.try_range(low, Bound::Unbounded, limit) else {
            self.queue_prefetch(after.copied().unwrap_or(Key::MIN));
            return (Some(after.copied().unwrap_or(Key::MIN)), Vec::new());
        };
        let cursor = batch.last().map(|(key, _)| *key);
        let versions = self.versions.borrow();
        let pages = batch
            .into_iter()
            .filter(|(key, value)| !self.removing(key, value))
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

    /// Hold pages `0..pages` of `version` for a warm job. See
    /// [`Index::reserve_hold_kind`]. Returns whether a storage-tier deadline
    /// was raised.
    pub async fn reserve_hold(
        &self,
        version: &ObjectVersion,
        pages: usize,
        until: SystemTime,
        scope: &RequestScope,
    ) -> Result<bool> {
        if pages > self.page_capacity() {
            return Err(Error::Overloaded);
        }
        self.reserve_hold_inner(
            version,
            &HoldPages::prefix(pages as u64),
            until,
            Some(HolderKind::Warm),
            scope,
        )
        .await
        .map(|(_, raised)| raised)
    }

    /// Reserve exactly `pages` of `version` at HEAD, before fetching any
    /// payload, with one holder of `kind` on each page. The set merges with
    /// any existing hold for the version, so earlier promises stay held and
    /// extension never shortens a deadline. Capacity is charged once per
    /// member page. Existing storage-tier members are protected atomically, or
    /// this call fails unchanged. Cache-tier members are protected once
    /// rewritten by `persist_held`. Pages outside the set are not pinned.
    ///
    /// Each call takes a fresh epoch, which identifies this reservation and
    /// is returned. Readers give their holders back with
    /// [`Index::release_hold_pages`] and that epoch. Warm holders are also
    /// dropped from unpublished pages by [`Index::release_hold`], and every
    /// holder by expiry.
    ///
    /// A raised `retain_until` of a storage-tier page is in memory only until
    /// the next volume commit (the lazy commit runs about once a second); a
    /// crash before it rebuilds the hold from the previous deadline. Callers
    /// that report the hold as durable must commit after every successful
    /// call: a raise whose commit failed stays in memory, so a later call with
    /// the same deadline changes nothing while the change is still not
    /// durable. `StoreWriter::reserve_hold` does both.
    pub async fn reserve_hold_kind(
        &self,
        version: &ObjectVersion,
        pages: &HoldPages,
        until: SystemTime,
        kind: HolderKind,
        scope: &RequestScope,
    ) -> Result<HoldEpoch> {
        self.reserve_hold_inner(version, pages, until, Some(kind), scope)
            .await
            .map(|(epoch, _)| epoch)
    }

    /// [`Index::reserve_hold_kind`] with reader holders.
    pub async fn reserve_hold_pages(
        &self,
        version: &ObjectVersion,
        pages: &HoldPages,
        until: SystemTime,
        scope: &RequestScope,
    ) -> Result<HoldEpoch> {
        self.reserve_hold_kind(version, pages, until, HolderKind::Reader, scope)
            .await
    }

    /// [`Index::reserve_hold_kind`] with warm holders.
    pub async fn reserve_warm_hold_pages(
        &self,
        version: &ObjectVersion,
        pages: &HoldPages,
        until: SystemTime,
        scope: &RequestScope,
    ) -> Result<HoldEpoch> {
        self.reserve_hold_kind(version, pages, until, HolderKind::Warm, scope)
            .await
    }

    /// Reserve `pages`, adding a holder of `kind` to each, or only renew the
    /// deadline when `kind` is `None`. Returns the entry's epoch and whether
    /// the `retain_until` of a storage-tier page was raised.
    async fn reserve_hold_inner(
        &self,
        version: &ObjectVersion,
        pages: &HoldPages,
        until: SystemTime,
        kind: Option<HolderKind>,
        scope: &RequestScope,
    ) -> Result<(HoldEpoch, bool)> {
        let monotonic = Holds::deadline(until)?;
        if !self.available(&version.object.cache) {
            return Err(Error::MissingKey);
        }
        let limit = self.hold_limit();
        let key = version_key(version);
        let members = self
            .holds
            .borrow()
            .entries
            .get(&key)
            .map_or_else(|| pages.clone(), |hold| hold.pages.union(pages));
        let existing: Vec<(Key, Value)> = match self.io() {
            Some((volume, reactor)) if !members.is_empty() => volume
                .range(&*reactor, low(&key), high(&key), usize::MAX, scope)
                .await?
                .into_iter()
                .filter(|(key, value)| {
                    value.tier == Tier::Storage
                        && members.contains(PageNumber(u64::from(key.page())))
                        && !self.removing(key, value)
                })
                .collect(),
            _ => Vec::new(),
        };
        let (epoch, until) = {
            let mut holds = self.holds.borrow_mut();
            holds.expire();
            // Every holder-adding reservation gets a fresh epoch. Pages that
            // join here record it, so releases issued before they joined
            // (including for an earlier, trimmed membership of the same page)
            // cannot reach them.
            let fresh = kind.map(|_| holds.next_epoch());
            let old = holds.entries.get(&key);
            let epoch = fresh.or(old.map(|hold| hold.epoch)).unwrap_or_default();
            // A release or another reservation may have changed the hold
            // while the scan above waited; merge against the current one.
            let members = old.map_or_else(|| pages.clone(), |hold| hold.pages.union(pages));
            // Empty sets need no disk guarantee or retained reservation entry.
            if members.is_empty() {
                return Ok((epoch, false));
            }
            let mut published: HashSet<u32> = existing
                .iter()
                .map(|(key, _)| key.page())
                .filter(|page| members.contains(PageNumber(u64::from(*page))))
                .collect();
            if let Some(old) = old {
                published.extend(old.published.iter().copied());
            }
            let count = members.len();
            let total = holds
                .entries
                .values()
                .try_fold(0usize, |sum, hold| sum.checked_add(hold.pages.len()))
                .ok_or(Error::Overloaded)?;
            let total = total
                .checked_sub(old.map_or(0, |hold| hold.pages.len()))
                .and_then(|n| n.checked_add(count))
                .ok_or(Error::Overloaded)?;
            if total > limit {
                return Err(Error::Overloaded);
            }
            let mut holders = old.map_or_else(HashMap::default, |hold| hold.holders.clone());
            if let Some(kind) = kind {
                for number in pages.iter() {
                    let holder = holders.entry(number).or_insert(PageHolders {
                        count: 0,
                        warm: 0,
                        since: epoch,
                    });
                    holder.count = holder.count.checked_add(1).ok_or(Error::Overloaded)?;
                    if kind == HolderKind::Warm {
                        holder.warm += 1;
                    }
                }
            }
            let inflight = old.map_or_else(Rc::default, |hold| hold.inflight.clone());
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
                    pages: members,
                    holders,
                    epoch,
                    inflight,
                    active,
                    published,
                },
            );
            (epoch, until)
        };
        let mut raised = false;
        if let Some((volume, reactor)) = self.io() {
            let retain = ceil_unix(until);
            for (key, value) in existing {
                if value.retain_until < retain {
                    volume.set_retain(&*reactor, &key, retain, scope).await?;
                    raised = true;
                }
            }
        }
        Ok((epoch, raised))
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
    /// Memory only: publications whose index page is not cached are skipped
    /// and queued for prefetch, so the next snapshot counts them.
    pub fn held_snapshot(&self) -> HeldSnapshot {
        let Some(volume) = self.volume() else {
            return HeldSnapshot::default();
        };
        let holds = self.holds.borrow();
        let versions = self.versions.borrow();
        let mut snapshot = HeldSnapshot::default();
        let mut slots = HashSet::default();
        let mut missing = Vec::new();
        for (version, hold) in holds.entries.iter().filter(|(_, hold)| hold.live()) {
            let known = versions.get(version);
            for number in &hold.published {
                let key = page_key(version, *number);
                let value = match volume.try_get(&key) {
                    Some(Some(value)) if !self.removing(&key, &value) => value,
                    Some(_) => continue,
                    None => {
                        missing.push(key);
                        continue;
                    }
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
        drop(holds);
        drop(versions);
        for key in missing {
            self.queue_prefetch(key);
        }
        snapshot.slots = slots.len() as u64;
        snapshot
    }

    /// Extend an existing reservation without adding page capacity. Returns
    /// whether a storage-tier deadline changed; see [`Index::reserve_hold`] for
    /// when that becomes durable.
    pub async fn extend_hold(
        &self,
        version: &ObjectVersion,
        until: SystemTime,
        scope: &RequestScope,
    ) -> Result<bool> {
        let pages = self
            .holds
            .borrow()
            .entries
            .get(&version_key(version))
            .filter(|hold| hold.live())
            .map(|hold| hold.pages.clone())
            .ok_or(Error::Unavailable)?;
        // Extension renews the deadline only; it adds no holders.
        self.reserve_hold_inner(version, &pages, until, None, scope)
            .await
            .map(|(_, raised)| raised)
    }

    /// Release only unused capacity after a partial failure. Completed pages stay
    /// held until the deadline. Call after this version's persistence futures and
    /// pending writes have finished. Unpublished members lose their warm holders
    /// and leave the hold unless a reader still holds them; published members
    /// keep all their holders.
    pub fn release_hold(&self, version: &ObjectVersion) -> Result<()> {
        let key = version_key(version);
        let mut holds = self.holds.borrow_mut();
        if let Some(hold) = holds.entries.get_mut(&key) {
            if !hold.inflight.is_empty() || hold.active.get() != 0 {
                return Err(Error::Overloaded);
            }
            let published = &hold.published;
            let is_published =
                |page: PageNumber| u32::try_from(page.0).is_ok_and(|n| published.contains(&n));
            hold.holders.retain(|page, holder| {
                if !is_published(*page) {
                    holder.count -= holder.warm;
                    holder.warm = 0;
                }
                holder.count != 0
            });
            let holders = &hold.holders;
            hold.pages = hold
                .pages
                .iter()
                .filter(|page| holders.contains_key(page) || is_published(*page))
                .collect();
            if hold.pages.is_empty() {
                holds.entries.remove(&key);
            }
        }
        Ok(())
    }

    /// Drop one holder from each of `pages` taken under `epoch`. A page whose
    /// count reaches zero leaves the hold and returns its charged capacity; a
    /// published one has its `retain_until` lowered by maintenance, so its
    /// slot can be evicted like any other. Pages that are not members are
    /// ignored, so a release after expiry or [`Index::release_hold`] is a
    /// no-op. A page that joined the hold after `epoch` was issued (it left
    /// and was reserved again, or the entry expired, was released or
    /// restored) is skipped, so a stale release cannot drop a later
    /// reservation's holder. Releasing a page whose held write is still in
    /// flight is allowed; that write then publishes as an unheld page.
    ///
    /// `kind` must match the reservation's kind. A warm release drops a warm
    /// holder and a reader release drops a reader holder; a page with no
    /// holder of that kind is skipped, so one kind's release never consumes
    /// the other's holder, and later warm cleanup ([`Index::release_hold`])
    /// removes only the warm holders that remain.
    pub fn release_hold_pages(
        &self,
        version: &ObjectVersion,
        pages: &HoldPages,
        epoch: HoldEpoch,
        kind: HolderKind,
    ) {
        let key = version_key(version);
        let mut holds = self.holds.borrow_mut();
        let Some(hold) = holds.entries.get_mut(&key) else {
            return;
        };
        let mut released = Vec::new();
        let mut unretain = Vec::new();
        for number in pages.iter() {
            if let Some(holder) = hold
                .holders
                .get_mut(&number)
                .filter(|holder| holder.since <= epoch)
            {
                match kind {
                    HolderKind::Warm if holder.warm != 0 => holder.warm -= 1,
                    HolderKind::Reader if holder.count > holder.warm => {}
                    _ => continue,
                }
                holder.count -= 1;
                if holder.count == 0 {
                    hold.holders.remove(&number);
                    if let Ok(page) = u32::try_from(number.0)
                        && hold.published.remove(&page)
                    {
                        unretain.push(page_key(&key, page));
                    }
                    released.push(number);
                }
            }
        }
        if released.is_empty() {
            return;
        }
        let released: HoldPages = released.into_iter().collect();
        hold.pages = hold
            .pages
            .iter()
            .filter(|page| !released.contains(*page))
            .collect();
        if hold.pages.is_empty() && hold.inflight.is_empty() && hold.active.get() == 0 {
            holds.entries.remove(&key);
        }
        drop(holds);
        let Some(volume) = self.volume() else {
            return;
        };
        for key in unretain {
            if self.unretain_cached(&volume, &key).is_none() {
                let mut maintenance = self.maintenance.borrow_mut();
                if maintenance.unretain.len() < MAINTENANCE_QUEUE {
                    maintenance.unretain.push(key);
                }
            }
        }
    }

    /// Clear the `retain_until` of a storage entry no hold publishes, from
    /// memory only. `None` when that needs a page read.
    fn unretain_cached(&self, volume: &DiskVolume, key: &Key) -> Option<()> {
        if self.held_key(key) {
            return Some(());
        }
        match volume.try_get(key)? {
            Some(value) if value.tier == Tier::Storage && value.retain_until != 0 => {
                volume.set_retain_cached(key, 0).map(drop)
            }
            _ => Some(()),
        }
    }

    /// Epoch of the latest reservation into `version`'s hold, live or not.
    #[cfg(test)]
    pub fn hold_epoch(&self, version: &ObjectVersion) -> Option<HoldEpoch> {
        self.holds
            .borrow()
            .entries
            .get(&version_key(version))
            .map(|hold| hold.epoch)
    }

    /// Holder count of `page` in a live hold, or zero when it is not held.
    pub fn hold_holders(&self, page: &PageId) -> u32 {
        self.holds
            .borrow()
            .entries
            .get(&version_key(&page.version))
            .filter(|hold| hold.live())
            .and_then(|hold| hold.holders.get(&page.number).map(|holder| holder.count))
            .unwrap_or(0)
    }

    /// Whether `key` is a published member of a live hold.
    fn held_key(&self, key: &Key) -> bool {
        self.holds
            .borrow()
            .entries
            .get(&key_version(key))
            .is_some_and(|hold| hold.live() && hold.published.contains(&key.page()))
    }

    /// Whether `page` is a member of a live hold, published or not.
    pub fn is_held(&self, page: &PageId) -> bool {
        self.holds
            .borrow()
            .entries
            .get(&version_key(&page.version))
            .is_some_and(|hold| hold.live() && hold.pages.contains(page.number))
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
                hold.live()
                    && hold.pages.contains(page.number)
                    && !hold.published.contains(&number)
                    && hold.unused() != 0
            })?;
        hold.inflight.add(page.number);
        Some(HoldTicket(hold.inflight.clone(), page.number))
    }
}

/// Synchronous wrappers for tests: they drive the worker reactor until the
/// async call completes. Never call them from inside a task.
#[cfg(test)]
impl Index {
    pub(crate) fn block<T>(&self, future: impl std::future::Future<Output = T>) -> T {
        let reactor = self.reactor.borrow().clone();
        let mut future = std::pin::pin!(future);
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..10_000_000 {
            if let std::task::Poll::Ready(value) = future.as_mut().poll(&mut cx) {
                return value;
            }
            if let Some(reactor) = &reactor {
                reactor.poll_budgeted(64).unwrap();
            }
        }
        panic!("index operation did not complete");
    }

    /// [`Index::lookup_value`] for tests.
    pub fn lookup(&self, page: &PageId) -> Result<Option<Value>> {
        let scope = maintenance_scope()?;
        self.block(self.lookup_value(page, &scope))
    }

    /// [`Index::already_on_disk_async`] for tests.
    pub(super) fn already_on_disk(&self, page: &PageId) -> Result<bool> {
        let scope = maintenance_scope()?;
        self.block(self.already_on_disk_async(page, &scope))
    }

    /// [`Index::disk_entry`] for tests.
    pub(super) fn disk_entry_now(&self, page: &PageId) -> Option<(Key, Value)> {
        let scope = maintenance_scope().ok()?;
        self.block(self.disk_entry(page, &scope)).ok().flatten()
    }

    /// [`Index::allocate`] for tests.
    pub(super) fn allocate_now(&self, tier: Tier, length: u64) -> Result<Reservation> {
        let scope = maintenance_scope()?;
        self.block(self.allocate(tier, length, &scope))
    }

    /// [`Index::reserve_hold`] for tests.
    pub fn reserve_hold_now(
        &self,
        version: &ObjectVersion,
        pages: usize,
        until: SystemTime,
    ) -> Result<bool> {
        let scope = maintenance_scope()?;
        self.block(self.reserve_hold(version, pages, until, &scope))
    }

    /// [`Index::reserve_hold_pages`] for tests.
    pub fn reserve_hold_pages_now(
        &self,
        version: &ObjectVersion,
        pages: &HoldPages,
        until: SystemTime,
    ) -> Result<HoldEpoch> {
        let scope = maintenance_scope()?;
        self.block(self.reserve_hold_pages(version, pages, until, &scope))
    }

    /// [`Index::reserve_warm_hold_pages`] for tests.
    pub fn reserve_warm_hold_pages_now(
        &self,
        version: &ObjectVersion,
        pages: &HoldPages,
        until: SystemTime,
    ) -> Result<HoldEpoch> {
        let scope = maintenance_scope()?;
        self.block(self.reserve_warm_hold_pages(version, pages, until, &scope))
    }

    /// [`Index::extend_hold`] for tests.
    pub fn extend_hold_now(&self, version: &ObjectVersion, until: SystemTime) -> Result<bool> {
        let scope = maintenance_scope()?;
        self.block(self.extend_hold(version, until, &scope))
    }

    /// [`Index::retain_caches`] for tests.
    pub fn retain_caches_now(&self, caches: &[CacheId]) -> usize {
        let scope = maintenance_scope().unwrap();
        self.block(self.retain_caches(caches, &scope)).unwrap()
    }

    /// Run maintenance until nothing is queued, for tests.
    pub(crate) fn maintain_now(&self) {
        let scope = maintenance_scope().unwrap();
        while self.maintenance_queued() {
            self.block(self.maintain(&scope, false)).unwrap();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::eviction_time;
    use std::time::{Duration, Instant};

    #[test]
    fn eviction_time_follows_wall_clock_but_caps_forward_jumps() {
        let start = Instant::now();
        // The first reading is the wall clock.
        let (now, anchor) = eviction_time(None, 1000, start);
        assert_eq!((now, anchor), (1000, (1000, start)));
        // Normal progress follows the wall clock and moves the anchor.
        let later = start + Duration::from_secs(10);
        let (now, anchor) = eviction_time(Some(anchor), 1010, later);
        assert_eq!((now, anchor), (1010, (1010, later)));
        // A forward jump is capped by monotonic progress and keeps the anchor.
        let jumped = later + Duration::from_secs(5);
        let (now, kept) = eviction_time(Some(anchor), 5000, jumped);
        assert_eq!((now, kept), (1015, anchor));
        // The cap keeps advancing with monotonic time after the jump.
        let (now, _) = eviction_time(Some(kept), 5010, jumped + Duration::from_secs(10));
        assert_eq!(now, 1025);
        // A backward jump is followed at once.
        let (now, anchor) = eviction_time(Some(kept), 900, jumped);
        assert_eq!((now, anchor), (900, (900, jumped)));
    }
}
