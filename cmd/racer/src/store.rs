//! Worker-local encrypted slab storage. No HTTP, plaintext, or origin credentials.
//! Versioned little-endian records protect framing with header SHA-256. The stored
//! CRC is checked before AEAD at the fill boundary; padding is never returned.
use self::catalog::Index;
use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::BufferPool;
use crate::memory::CiphertextCopy;
use crate::model::CacheKey;
use crate::model::Nonce;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::StrongEtag;
use crate::model::VersionMetadata;
use crate::runtime::HashMap;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use page_alloc::AlignedBuffer;
use page_alloc::Alignment;
use page_alloc::Extent;
use page_alloc::Slab;
use page_alloc::volume::Key;
use page_alloc::volume::SLOT_BYTES;
use page_alloc::volume::Tier;
use page_alloc::volume::Volume;
use page_alloc::volume::VolumeOptions;
use racer_control_wire::CacheId;
use racer_control_wire::KeyId;
use sha2::Digest;
use sha2::Sha256;
use std::cell::Cell;
use std::cell::RefCell;
use std::collections::VecDeque;
use std::rc::Rc;

pub mod catalog;
pub mod checkpoint;
pub mod disk;
pub mod holds;
pub(crate) mod key;
#[cfg(test)]
pub(crate) mod test_disk;
pub mod versions;

/// Whether a failed disk index commit only costs cache contents and must not
/// fail the worker. A failed commit leaves the volume usable and the previous
/// commit durable, so the next mount just finds fewer cache pages; storage
/// writes report their own commit failure to the caller.
pub fn is_disposable_commit_error(error: &Error) -> bool {
    matches!(
        error,
        Error::Io
            | Error::Os(_)
            | Error::CorruptRecord
            | Error::Unavailable
            | Error::Overloaded
            | Error::Cancelled
            | Error::DeadlineExceeded
    )
}

/// Write id stored in a record header; equals the volume entry's generation.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct Generation(pub u64);

pub struct Store {
    pub reader: Rc<StoreReader>,
    pub writer: Rc<StoreWriter>,
    pub checkpoint: Rc<checkpoint::Checkpointer>,
    pub recovery: checkpoint::Recovery,
}

impl Store {
    /// Side-effect-free resource wiring, before startup and request admission.
    pub fn configure(
        &self,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        queue_entries: usize,
        page_entries: usize,
    ) -> Result<()> {
        self.writer
            .configure(admission, queue_entries, page_entries)
    }

    /// Open the slab, mount (or format) its extent volume, and configure the
    /// checkpoint layout. Drive the worker reactor while this runs.
    pub fn open<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, Alignment> {
        Box::pin(async move {
            let alignment = self.writer.open(scope).await?;
            let layout = self.checkpoint.configure_layout()?;
            self.recovery.configure_layout(layout);
            Ok(alignment)
        })
    }
}

/// Read records through the volume and return validated ciphertext framing.
/// AEAD validation remains owned by fill.
pub struct StoreReader {
    metrics: crate::telemetry::Metrics,
    index: Rc<Index>,
    slabs: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    buffers: BufferPool,
}
#[derive(Clone)]
/// Permits conditional invalidation without removing a replacement mapping.
pub struct ReadToken {
    key: Key,
    generation: u64,
}
#[cfg(test)]
impl ReadToken {
    /// A token for an entry that need not exist, for tests that only route it.
    pub(crate) fn for_test(page: PageId) -> Self {
        Self {
            key: key::volume_key(&page).expect("test page in key range"),
            generation: 1,
        }
    }
}
impl StoreReader {
    pub fn metadata(
        &self,
        version: &crate::model::ObjectVersion,
    ) -> Result<Option<crate::model::VersionMetadata>> {
        self.index.version(version)
    }
    pub fn new(
        index: Rc<Index>,
        slabs: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        buffers: BufferPool,
    ) -> Self {
        Self {
            metrics: crate::telemetry::Metrics::default(),
            index,
            slabs,
            admission,
            reactor,
            buffers,
        }
    }
    pub fn with_metrics(mut self, metrics: crate::telemetry::Metrics) -> Self {
        self.metrics = metrics;
        self
    }
    fn corrupt_miss(&self) {
        self.metrics.record(crate::telemetry::Event::CorruptMiss, 1);
    }
    /// Miss on a read that failed with a transient I/O error. A cache-tier
    /// entry is removed. A storage-tier entry is a hard hold's durable copy
    /// that may still be intact, so it stays in the index and in its hold; the
    /// read just misses.
    fn transient_miss(
        &self,
        token: &ReadToken,
        tier: Tier,
    ) -> Result<Option<(CiphertextCopy, ReadToken)>> {
        if tier == Tier::Cache {
            self.invalidate(token)?;
        }
        Ok(None)
    }
    /// Miss on a record that is definitely unusable (damaged, of another
    /// format version, or contradicting the known version metadata). The entry
    /// is removed whatever its tier, so the next fill can repair it.
    fn corrupt_record(&self, token: &ReadToken) -> Result<Option<(CiphertextCopy, ReadToken)>> {
        self.invalidate(token)?;
        Ok(None)
    }
    /// Remove the entry `token` read, unless a newer write replaced it.
    ///
    /// This applies to both tiers. Removing a held storage-tier page drops
    /// only its publication: the hold (keyed by version) stays, and the page
    /// returns to the hold's unused capacity, so the next fill or warm of the
    /// page writes it again to the storage tier with the hold's deadline as
    /// `retain_until`. The removal becomes durable with the next volume
    /// commit; until then a crash may bring back the bad entry, which the next
    /// read detects again.
    pub fn invalidate(&self, token: &ReadToken) -> Result<()> {
        self.index
            .remove_if_generation(&token.key, token.generation);
        Ok(())
    }
    pub fn read_with_token<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<(CiphertextCopy, ReadToken)>> {
        self.read_with_token_reclaim(page, scope, |amount| {
            self.admission
                .reserve(
                    Some(&page.version.object.cache),
                    ResourceClass::Ciphertext,
                    amount,
                )
                .map_err(Into::into)
        })
    }
    /// Admit staging and decoded ciphertext together before submitting disk I/O.
    pub(crate) fn read_with_token_reclaim<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
        reserve: impl Fn(usize) -> Result<flow_control::Charge<AdmissionPolicy>> + 'a,
    ) -> Operation<'a, Option<(CiphertextCopy, ReadToken)>> {
        Box::pin(async move {
            scope.check()?;
            // Pages beyond the key's page range are never stored.
            let Some(key) = key::volume_key(page) else {
                return Ok(None);
            };
            let value = match self.metrics.lookup(
                crate::telemetry::LookupTier::DiskIndex,
                self.index.lookup_value(page, scope).await,
            )? {
                Some(value) => value,
                None => return Ok(None),
            };
            let Some(volume) = self.index.volume() else {
                return Ok(None);
            };
            let token = ReadToken {
                key,
                generation: value.generation,
            };
            let length = value.length as usize;
            let amount = staging_capacity(length)?;
            let mut reserved = reserve(amount);
            if matches!(reserved, Err(Error::Overloaded)) {
                self.slabs.reclaim_idle();
                reserved = reserve(amount);
            }
            let reservation = reserved?;
            reservation.validate(ResourceClass::Ciphertext, amount)?;
            if !self.admission.owns(&reservation)
                || reservation.key() != Some(&page.version.object.cache)
            {
                return Err(Error::InvalidConfiguration);
            }
            let buffer = self.slabs.allocate(length, reservation)?;
            // The volume leases the slot for the read, so eviction cannot reuse
            // it mid-transfer; the header check below catches earlier reuse.
            let buffer = match volume.read(&*self.reactor, &value, buffer, scope).await {
                Ok(b) => b,
                Err(Error::Io | Error::Os(_)) => return self.transient_miss(&token, value.tier),
                Err(Error::CorruptRecord) => {
                    self.corrupt_miss();
                    return self.corrupt_record(&token);
                }
                Err(Error::Unavailable) => return Ok(None),
                Err(e) => return Err(e),
            };
            // A record with this magic but a different format version (for
            // example, written by a newer release) is a cold miss, not evidence
            // of damaged storage. Never parse its remaining fields. This release
            // cannot use it, so drop it and let the next fill rewrite the page.
            let bytes = buffer.bytes()?;
            if bytes.get(..8) == Some(MAGIC)
                && bytes
                    .get(8..12)
                    .is_some_and(|version| version != FORMAT_VERSION.to_le_bytes())
            {
                return self.corrupt_record(&token);
            }
            let extent = Extent::new(0, length)?;
            let decoded = match parse(&buffer, extent) {
                Ok(d) => d,
                Err(_) => {
                    self.corrupt_miss();
                    return self.corrupt_record(&token);
                }
            };
            if decoded.header.generation.0 != value.generation
                || decoded.header.envelope.page != *page
            {
                // Another record now occupies the slot: a stale entry, not
                // corruption. Remove it only if the index still points here.
                self.invalidate(&token)?;
                return Ok(None);
            }
            let key_id = decoded.header.envelope.key_id;
            if !self
                .index
                .page_available(&page.version.object.cache, key_id)
            {
                self.invalidate(&token)?;
                return Ok(None);
            }
            if self
                .index
                .version(&page.version)?
                .is_some_and(|known| !known.compatible(&decoded.header.metadata))
            {
                self.corrupt_miss();
                return self.corrupt_record(&token);
            }
            // A concurrent retirement/removal must not resurrect a completed copy.
            if self
                .index
                .lookup_value(page, scope)
                .await?
                .is_none_or(|current| current != value)
            {
                return Ok(None);
            }
            self.index.learn_version(&decoded.header.metadata, key_id);
            let metadata = decoded.header.metadata.clone();
            let ciphertext = self.buffers.slab_ciphertext(
                buffer,
                decoded.header.envelope,
                decoded.ciphertext,
            )?;
            ciphertext.expected_checksum(decoded.checksum)?;
            self.index
                .retention()
                .disk_read(page, u64::from(metadata.page_length(page)?));
            Ok(Some((
                CiphertextCopy {
                    metadata: metadata.for_pin(),
                    ciphertext,
                },
                token,
            )))
        })
    }
    pub fn read<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<CiphertextCopy>> {
        Box::pin(async move {
            Ok(self
                .read_with_token(page, scope)
                .await?
                .map(|(copy, _)| copy))
        })
    }

    /// Locate a published v2 record's ciphertext on its device for a direct
    /// (cuFile) read, reading and validating only the header through host
    /// memory. Returns None on a miss, for v1 records, and for any record
    /// this read path would also treat as a miss. The ciphertext checksum is
    /// not verified here: the consumer must check
    /// [`DirectRecord::checksum`] before trusting the bytes.
    pub(crate) fn direct_read<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
        reserve: impl Fn(usize) -> Result<flow_control::Charge<AdmissionPolicy>> + 'a,
    ) -> Operation<'a, Option<DirectRecord>> {
        Box::pin(async move {
            scope.check()?;
            let Some(key) = key::volume_key(page) else {
                return Ok(None);
            };
            let Some(value) = self.metrics.lookup(
                crate::telemetry::LookupTier::DiskIndex,
                self.index.lookup_value(page, scope).await,
            )?
            else {
                return Ok(None);
            };
            let Some(volume) = self.index.volume() else {
                return Ok(None);
            };
            let token = ReadToken {
                key,
                generation: value.generation,
            };
            // The volume leases the slot for the header read; this lease keeps
            // the slot from eviction and reuse until the caller's direct
            // transfer completes.
            let Some(hold) = volume.lease(value.slot) else {
                return Ok(None);
            };
            let length = value.length as usize;
            let prefix = length.min(MAX_HEADER_BYTES);
            let mut reserved = reserve(prefix);
            if matches!(reserved, Err(Error::Overloaded)) {
                self.slabs.reclaim_idle();
                reserved = reserve(prefix);
            }
            let reservation = reserved?;
            let Ok(buffer) = self.slabs.allocate(prefix, reservation) else {
                return Ok(None);
            };
            let buffer = match volume.read(&*self.reactor, &value, buffer, scope).await {
                Ok(buffer) => buffer,
                Err(
                    Error::Io
                    | Error::Os(_)
                    | Error::CorruptRecord
                    | Error::InvalidConfiguration
                    | Error::Unavailable,
                ) => {
                    return Ok(None);
                }
                Err(error) => return Err(error),
            };
            let extent = Extent::new(0, length)?;
            let decoded = match parse_prefix(buffer.bytes()?, extent) {
                Ok(decoded) => decoded,
                // A short prefix of a valid v1 record, another version, or
                // damage: the host read path classifies and reports it.
                Err(_) => return Ok(None),
            };
            drop(buffer);
            if decoded.header.envelope.page != *page
                || decoded.header.generation.0 != value.generation
            {
                // Another record now occupies the slot: a stale entry.
                self.invalidate(&token)?;
                return Ok(None);
            }
            let key_id = decoded.header.envelope.key_id;
            if !self
                .index
                .page_available(&page.version.object.cache, key_id)
            {
                self.invalidate(&token)?;
                return Ok(None);
            }
            if self
                .index
                .version(&page.version)?
                .is_some_and(|known| !known.compatible(&decoded.header.metadata))
            {
                self.corrupt_miss();
                self.invalidate(&token)?;
                return Ok(None);
            }
            if self
                .index
                .lookup_value(page, scope)
                .await?
                .is_none_or(|current| current != value)
            {
                return Ok(None);
            }
            let Some(ciphertext) = direct_ciphertext(&self.slabs, &value, &decoded)? else {
                return Ok(None);
            };
            // cuFile reads whole 4 KiB blocks. v2 extents are 4 KiB multiples
            // and the ciphertext ends the record, so rounding up stays inside it.
            let rounded = ciphertext.length.next_multiple_of(CIPHERTEXT_ALIGNMENT);
            let read_length = if decoded.ciphertext.start + rounded <= length {
                rounded
            } else {
                ciphertext.length
            };
            self.index.learn_version(&decoded.header.metadata, key_id);
            let metadata = decoded.header.metadata.clone();
            self.index
                .retention()
                .disk_read(page, u64::from(metadata.page_length(page)?));
            Ok(Some(DirectRecord {
                token,
                metadata: metadata.for_pin(),
                envelope: decoded.header.envelope,
                checksum: decoded.checksum,
                ciphertext,
                read_length,
                lease: hold,
            }))
        })
    }
}

/// A published v2 record located for a direct read by [`StoreReader::direct_read`].
pub(crate) struct DirectRecord {
    /// Invalidates exactly this mapping if the bytes fail verification.
    pub token: ReadToken,
    pub metadata: crate::model::ObjectMetadata,
    pub envelope: PageEnvelope,
    /// The stored CRC-64 of the sealed page (ciphertext and tag).
    pub checksum: u64,
    pub ciphertext: DirectCiphertext,
    /// Bytes to transfer from `ciphertext.offset`: the ciphertext length
    /// rounded up to whole 4 KiB blocks when the record holds them.
    pub read_length: usize,
    /// Keeps the slot, and so the bytes, from being evicted and reused.
    /// Hold it until the direct transfer completes.
    pub lease: page_alloc::volume::SlotLease,
}

struct Dirty {
    hold: Option<holds::HoldGuard>,
    _resident: catalog::Resident,

    _payload: crate::retention::Payload,

    _metric: ::telemetry::Lease,

    ticket: u64,

    page: CiphertextCopy,

    _reservation: Rc<flow_control::Charge<AdmissionPolicy>>,

    staging: Option<flow_control::Charge<AdmissionPolicy>>,
}

impl Dirty {
    /// Bytes released by dropping this queued write's staging charge.
    fn staging_bytes(&self) -> usize {
        self.staging
            .as_ref()
            .map_or(0, flow_control::Charge::amount)
    }
}

/// Foreground queue selection and positional removal inspect only this prefix.
/// VecDeque::remove shifts the shorter side, so a prefix victim also bounds moves.
const QUEUE_RECLAIM_CANDIDATES: usize = 64;
/// Record bytes one writer keeps in flight, so large records cannot build a
/// deep device queue ahead of small and metadata I/O.
pub(crate) const WRITER_IN_FLIGHT_BYTES: usize = 64 << 20;
/// Writes one writer keeps in flight.
pub(crate) const WRITER_IN_FLIGHT_WRITES: usize = 8;
/// Writes one background `progress` call starts before it returns, so periodic
/// checkpoints and other callers regain control.
pub(crate) const WRITER_PROGRESS_BUDGET: usize = 4 * WRITER_IN_FLIGHT_WRITES;

/// Locate queued work without inspecting the unselected suffix.
fn queued_position(queue: &VecDeque<PageId>, page: &PageId) -> Option<usize> {
    queue
        .iter()
        .take(QUEUE_RECLAIM_CANDIDATES)
        .position(|queued| queued == page)
}

/// Bounded dirty copies persist asynchronously, with publication after full I/O.
pub struct StoreWriter {
    metrics: crate::telemetry::Metrics,
    disk_groups: Vec<(std::sync::Arc<str>, usize)>,
    index: Rc<Index>,
    slabs: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    pending: RefCell<HashMap<PageId, Dirty>>,
    queue: RefCell<VecDeque<PageId>>,
    next: Cell<u64>,
    capacity: Cell<usize>,
    busy: Cell<bool>,
    waiters: RefCell<HashMap<u64, Option<std::task::Waker>>>,
    next_waiter: Cell<u64>,
    active_scope: RefCell<Option<RequestScope>>,
    availability: Rc<crate::control::Availability>,
    discarded: Cell<u64>,
    closed: Cell<bool>,
    /// Byte budget of the volume's metadata page cache, used at mount.
    metadata_cache_bytes: Cell<usize>,
}
impl StoreWriter {
    pub fn retention(&self) -> Rc<crate::retention::Retention> {
        self.index.retention()
    }
    /// One ranking pass over this worker's disk shares.
    pub(crate) fn scoring<'a>(
        &self,
        retention: &'a crate::retention::Retention,
    ) -> crate::retention::Scoring<'a> {
        retention.scoring_at(
            uring_runtime::environment::now(),
            self.index.disk_capacity(),
        )
    }
    /// Install before read engines capture the shared policy.
    pub fn set_retention(&self, retention: Rc<crate::retention::Retention>) {
        self.index.set_retention(retention);
    }
    pub fn new(
        index: Rc<Index>,
        slabs: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        availability: Rc<crate::control::Availability>,
    ) -> Self {
        Self {
            metrics: crate::telemetry::Metrics::default(),
            disk_groups: Vec::new(),
            index,
            slabs,
            admission,
            reactor,
            pending: RefCell::new(HashMap::default()),
            queue: RefCell::new(VecDeque::new()),
            next: Cell::new(1),
            capacity: Cell::new(64),
            busy: Cell::new(false),
            waiters: RefCell::new(HashMap::default()),
            next_waiter: Cell::new(0),
            active_scope: RefCell::new(None),
            availability,
            discarded: Cell::new(0),
            closed: Cell::new(false),
            metadata_cache_bytes: Cell::new(crate::store::disk::DEFAULT_METADATA_CACHE_BYTES),
        }
    }
    /// Startup only, before `open`: the byte budget of the volume's metadata
    /// page cache. A mounted volume keeps its budget.
    pub fn set_metadata_cache_bytes(&self, bytes: usize) {
        self.metadata_cache_bytes.set(bytes.max(1));
        if let Some(volume) = self.index.volume() {
            volume.set_metadata_cache_bytes(bytes.max(1));
        }
    }
    /// Run queued index maintenance (see `Index::maintain`), including
    /// evictions that keep free slots ahead of allocation, then commit when
    /// the volume is dirty. The lazy committer drives this.
    pub fn maintain<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.index.maintain(scope, true).await?;
            if self.index.is_dirty() {
                self.commit(scope).await?;
            }
            Ok(())
        })
    }
    pub fn with_metrics(mut self, metrics: crate::telemetry::Metrics) -> Self {
        self.metrics = metrics;
        self
    }
    pub fn configure(
        &self,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        queue_entries: usize,
        page_entries: usize,
    ) -> Result<()> {
        if queue_entries == 0 || self.busy.get() || !self.pending.borrow().is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        if !Rc::ptr_eq(&self.admission, &admission) {
            return Err(Error::InvalidConfiguration);
        }
        self.index.set_page_capacity(page_entries)?;
        self.capacity.set(queue_entries);
        Ok(())
    }
    /// Mount the extent volume on the slab, formatting it when no valid
    /// superblock is found (for example a slab of an older release, whose data
    /// is discarded). Drive the worker reactor while this runs.
    pub fn open<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, Alignment> {
        Box::pin(async move {
            if let Some(volume) = self.index.volume() {
                return Ok(volume.alignment());
            }
            let slots = self.slabs.capacity_bytes() / SLOT_BYTES;
            if !self.disk_groups.is_empty()
                && self
                    .disk_groups
                    .iter()
                    .map(|(_, count)| *count as u64)
                    .sum::<u64>()
                    != slots
            {
                return Err(Error::InvalidConfiguration);
            }
            let mut volume_id = [0; 16];
            getrandom::getrandom(&mut volume_id).map_err(|_| Error::Unavailable)?;
            let admission = self.admission.clone();
            let mut options = VolumeOptions::new(
                self.index.volume_limits(slots),
                volume_id,
                true,
                Rc::new(move |length| {
                    admission
                        .reserve_completion(None, ResourceClass::Ciphertext, length)
                        .map_err(|_| page_alloc::Error::Busy)
                }),
            );
            options.metadata_cache_bytes = self.metadata_cache_bytes.get();
            let (volume, report) =
                Volume::mount(&*self.reactor, self.slabs.clone(), options, scope).await?;
            let alignment = volume.alignment();
            self.index
                .install_volume(volume, self.reactor.clone(), &report, scope)
                .await?;
            // Mount buffers return to the slab's idle pool still charged.
            self.slabs.reclaim_idle();
            Ok(alignment)
        })
    }
    pub(crate) fn with_disk_groups(mut self, groups: Vec<(std::sync::Arc<str>, usize)>) -> Self {
        self.disk_groups = groups;
        self
    }
    pub(crate) fn observe_disks(&self) {
        let counts: Vec<usize> = self.disk_groups.iter().map(|(_, count)| *count).collect();
        let samples = self
            .disk_groups
            .iter()
            .zip(self.index.disk_usage(&counts))
            .map(|((disk, _), (size, used))| crate::telemetry::DiskUsage {
                disk: disk.clone(),
                size,
                used,
            })
            .collect();
        self.metrics.observe_disks(samples);
    }
    /// Sample per-cache resident bytes and fair shares for published caches.
    pub(crate) fn observe_cache_disks(&self, caches: &[racer_control_wire::CacheDefinition]) {
        let usage = self.retention().cache_disk(self.index.disk_capacity());
        self.metrics
            .observe_cache_disks(cache_disk_samples(caches, usage));
    }
    /// Whether optional pages of `cache` should skip disk admission: the cache
    /// is over its fair share and no free slot remains beyond the reserve.
    pub(crate) fn over_share_under_pressure(&self, cache: &CacheId) -> bool {
        self.index.free_slots_at_reserve()
            && self
                .retention()
                .over_share(cache, self.index.disk_capacity())
    }
    pub fn slabs(&self) -> &Rc<Slab<flow_control::Charge<AdmissionPolicy>>> {
        &self.slabs
    }
    pub fn index(&self) -> &Rc<Index> {
        &self.index
    }
    fn allowed(&self, page: &CiphertextCopy) -> bool {
        let e = page.ciphertext.envelope();
        self.availability
            .page(&e.page.version.object.cache, e.key_id)
    }
    pub fn enqueue(
        &self,
        page: CiphertextCopy,
        dirty: flow_control::Charge<AdmissionPolicy>,
    ) -> Result<u64> {
        let cache = page.metadata.version.object.cache.clone();
        self.enqueue_reclaiming(page, dirty, |length| self.reserve_staging(length, &cache))
    }

    /// Force an encrypted copy to disk, regardless of ownership or second sight.
    /// Reserve the version first. Success means a published storage-tier write
    /// made durable by a volume commit, not queue acceptance. Poll with the
    /// worker reactor.
    /// Cancellation waits for accepted disk work to finish. Keep polling this
    /// future and the background writer until completion, even after canceling.
    pub fn persist_held<'a>(
        &'a self,
        page: CiphertextCopy,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            logical_length(&page)?;
            let id = page.ciphertext.envelope().page.clone();
            let _hold = self.index.protect_hold(&id.version)?;
            if !self.index.is_held(&id) {
                return Err(Error::Unavailable);
            }
            if self
                .index
                .lookup_value(&id, scope)
                .await?
                .is_some_and(|value| value.tier == Tier::Storage)
                && self.index.held_page_expiry(&id).is_some()
            {
                if self
                    .index
                    .version(&id.version)?
                    .is_some_and(|known| !known.compatible(&page.metadata.immutable()))
                {
                    return Err(Error::CorruptRecord);
                }
                // An earlier publication may not be committed yet.
                return self.commit(scope).await;
            }
            let waiter = WriterWaiter::new(self)?;
            let dirty = self.admission.reserve_completion(
                Some(&id.version.object.cache),
                ResourceClass::DirtyCiphertext,
                page.ciphertext.bytes().len(),
            )?;
            let ticket = self.enqueue(page, dirty)?;
            loop {
                waiter.ready().await;
                if let Err(error) = scope.check() {
                    self.remove_queued_ticket(&id, ticket);
                    self.slabs.fence_writes().await?;
                    return Err(error);
                }
                if self
                    .pending
                    .borrow()
                    .get(&id)
                    .is_none_or(|dirty| dirty.ticket != ticket)
                {
                    break;
                }
                if let Err(error) = self.progress(WRITER_PROGRESS_BUDGET, scope).await {
                    self.remove_queued_ticket(&id, ticket);
                    self.slabs.fence_writes().await?;
                    return Err(error);
                }
            }
            self.slabs.fence_writes().await?;
            scope.check()?;
            // `_hold` keeps the hold entry from being dropped while this call
            // runs, but not its deadline from passing. If the hold expires
            // during the write, the record is published without a retention
            // deadline (an ordinary evictable storage entry) and is not added
            // to the hold, so this reports `Unavailable` instead of a durable
            // held copy. The same applies when the page lost its storage-tier
            // publication for another reason, such as cache or key removal.
            let value = self
                .index
                .lookup_value(&id, scope)
                .await?
                .ok_or(Error::Io)?;
            if value.tier != Tier::Storage || self.index.held_page_expiry(&id).is_none() {
                return Err(Error::Unavailable);
            }
            self.commit(scope).await
        })
    }

    fn remove_queued_ticket(&self, page: &PageId, ticket: u64) {
        let position = self.queue.borrow().iter().position(|queued| queued == page);
        if self
            .pending
            .borrow()
            .get(page)
            .is_some_and(|dirty| dirty.ticket == ticket)
            && let Some(position) = position
        {
            self.queue.borrow_mut().remove(position);
            self.pending.borrow_mut().remove(page);
            self.note_discard(1);
        }
    }

    fn protected_pending(&self, page: &PageId) -> bool {
        self.index.is_held(page)
            || self
                .pending
                .borrow()
                .get(page)
                .is_some_and(|dirty| dirty.hold.is_some())
    }
    /// Accepted fills may complete after request admission closes, within quota.
    fn reserve_staging(
        &self,
        length: usize,
        cache: &CacheId,
    ) -> Result<flow_control::Charge<AdmissionPolicy>> {
        let reserve = || {
            self.admission
                .reserve_completion(Some(cache), ResourceClass::Ciphertext, length)
                .map_err(Error::from)
        };
        let result = reserve();
        if matches!(result, Err(Error::Overloaded)) {
            self.slabs.reclaim_idle();
            return reserve();
        }
        result
    }
    /// The fill owner may reclaim idle cached bytes for exact aligned staging.
    /// Reclamation is synchronous and happens before queue acceptance; no writer
    /// map borrow may span it because it can release other unsubmitted copies.
    pub(crate) fn enqueue_reclaiming(
        &self,
        mut page: CiphertextCopy,
        dirty: flow_control::Charge<AdmissionPolicy>,
        mut reserve_staging: impl FnMut(usize) -> Result<flow_control::Charge<AdmissionPolicy>>,
    ) -> Result<u64> {
        if self.closed.get() {
            return Err(Error::Unavailable);
        }
        let logical = logical_length(&page)?;
        if !matches!(dirty.class(), ResourceClass::DirtyCiphertext)
            || !self.admission.owns(&dirty)
            || dirty.key() != Some(&page.metadata.version.object.cache)
            || dirty.amount() < page.ciphertext.bytes().len()
            || logical > MAX_RECORD_BYTES
        {
            return Err(Error::InvalidConfiguration);
        }
        if !self.allowed(&page) {
            return Err(Error::MissingKey);
        }
        let id = page.ciphertext.envelope().page.clone();
        if let Some(metadata) = self.index.version(&id.version)?
            && !metadata.compatible(&page.metadata.immutable())
        {
            return Err(Error::CorruptRecord);
        }
        if let Some(metadata) = self.metadata(&id.version)?
            && !metadata.compatible(&page.metadata.immutable())
        {
            return Err(Error::CorruptRecord);
        }
        // Zero denotes already on disk: no pending ticket or second write. A
        // held page in the cache tier is rewritten to the storage tier. Only
        // pages whose index entry is in memory are known here; the writer
        // checks again before writing.
        if self.index.already_on_disk_cached(&id) {
            return Ok(0);
        }
        let mut pending = self.pending.borrow_mut();
        if let Some(existing) = pending.get_mut(&id) {
            if existing.hold.is_none() && self.index.is_held(&id) {
                existing.hold = Some(self.index.protect_hold(&id.version)?);
            }
            return Ok(existing.ticket);
        }
        drop(pending);
        // The queue outlives the request: never pin a registered RDMA buffer.
        // The copy-out uses completion admission like disk staging, because an
        // accepted fill must still persist after request admission stops. It goes
        // through the caller's callback so cached pages can be reclaimed first.
        if let Some(copy) = page.ciphertext.retained_copy()
            && self.admission.owns(copy.inner.reservation())
            && copy.inner.reservation().key() == Some(&page.metadata.version.object.cache)
        {
            // An earlier retaining path (the memory cache) already copied it
            // out under this admission: share that copy.
            page.ciphertext = copy;
        }
        if page.ciphertext.is_registered() {
            let length = page.ciphertext.bytes().len();
            let reservation = reserve_staging(length)?;
            reservation.validate(ResourceClass::Ciphertext, length)?;
            if !self.admission.owns(&reservation)
                || reservation.key() != Some(&page.metadata.version.object.cache)
            {
                return Err(Error::InvalidConfiguration);
            }
            page.ciphertext = page.ciphertext.rehome(reservation)?;
        }
        let full = self.pending.borrow().len() >= self.capacity.get();
        if full {
            let retention = self.retention();
            let scoring = self.scoring(&retention);
            // An unheld over-share page never displaces an under-share page.
            let victim = self
                .queue
                .borrow()
                .iter()
                .enumerate()
                .take(QUEUE_RECLAIM_CANDIDATES)
                .filter(|(_, page)| !self.protected_pending(page))
                .map(|(position, page)| (position, page.clone(), scoring.rank(page)))
                .min_by_key(|(_, _, rank)| *rank);
            let Some((position, victim, rank)) = victim else {
                return Err(Error::Overloaded);
            };
            if !self.index.is_held(&id) && scoring.rank(&id) <= rank {
                return Err(Error::Overloaded);
            }
            self.queue.borrow_mut().remove(position);
            self.pending.borrow_mut().remove(&victim);
            self.note_discard(1);
        }
        // Reserve the staging persist will need before queue acceptance. Thus
        // accepted dirty copies never wait for ciphertext holders to release
        // memory. Computed after any rehome above, so a registered page that
        // became aligned only reserves its header block.
        let capacity = staging_bytes(&page, self.slabs.alignment()?)?;
        let staging = reserve_staging(capacity)?;
        staging.validate(ResourceClass::Ciphertext, capacity)?;
        if !self.admission.owns(&staging) || staging.key() != Some(&id.version.object.cache) {
            return Err(Error::InvalidConfiguration);
        }
        let ticket = self.next.get();
        self.next
            .set(ticket.checked_add(1).ok_or(Error::Unavailable)?);
        self.pending.borrow_mut().insert(
            id.clone(),
            Dirty {
                hold: if self.index.is_held(&id) {
                    Some(self.index.protect_hold(&id.version)?)
                } else {
                    None
                },
                _resident: self.index.track(&id)?,
                _payload: self.retention().payload(
                    u64::from(page.ciphertext.envelope().plaintext_length),
                    false,
                ),
                _metric: self
                    .metrics
                    .lease(crate::telemetry::Gauge::PendingDiskWrites)?,
                ticket,
                page,
                _reservation: Rc::new(dirty),
                staging: Some(staging),
            },
        );
        self.queue.borrow_mut().push_back(id);
        Ok(ticket)
    }
    pub fn copy_only(&self, page: &PageId) -> Result<Option<CiphertextCopy>> {
        Ok(self
            .pending
            .borrow()
            .get(page)
            .filter(|d| self.allowed(&d.page))
            .map(|d| d.page.clone()))
    }
    pub fn metadata(&self, version: &ObjectVersion) -> Result<Option<VersionMetadata>> {
        Ok(self
            .pending
            .borrow()
            .iter()
            .find(|(p, d)| &p.version == version && self.allowed(&d.page))
            .map(|(_, d)| d.page.metadata.immutable()))
    }
    pub fn pending_count(&self) -> usize {
        self.pending.borrow().len()
    }
    pub(crate) fn can_start_progress(&self) -> bool {
        !self.busy.get()
    }
    pub fn queued_count(&self) -> usize {
        self.queue.borrow().len()
    }
    pub fn writes_in_flight(&self) -> usize {
        self.slabs.writes_in_flight()
    }
    pub fn discarded_count(&self) -> u64 {
        self.discarded.get()
    }
    fn note_discard(&self, count: usize) {
        self.metrics
            .record(crate::telemetry::Event::DirtyDiscard, count as u64);
        self.discarded
            .set(self.discarded.get().saturating_add(count as u64));
    }
    /// Reject new enqueue calls while preserving previously accepted copies.
    pub fn stop_admission(&self) {
        self.closed.set(true);
    }
    /// Shutdown deadline: abandon queued persistence and request cancellation of
    /// active I/O. Keep polling progress/reactor until is_idle; this is not a fence.
    pub fn cancel_pending_writes(&self) -> Result<usize> {
        self.stop_admission();
        let discarded = self.discard_unsubmitted();
        if let Some(scope) = self.active_scope.borrow().as_ref() {
            scope.cancel()?;
        }
        Ok(discarded)
    }
    /// Discard only work not taken by progress. Submitted owners remain fenced.
    pub fn discard_unsubmitted(&self) -> usize {
        let queued: Vec<_> = self.queue.borrow_mut().drain(..).collect();
        let mut pending = self.pending.borrow_mut();
        let mut removed = 0;
        for page in queued {
            if self.index.is_held(&page)
                || pending.get(&page).is_some_and(|dirty| dirty.hold.is_some())
            {
                self.queue.borrow_mut().push_back(page);
                continue;
            }
            removed += usize::from(pending.remove(&page).is_some());
        }
        self.note_discard(removed);
        removed
    }
    /// Unpin exactly one otherwise-idle memory bundle. The two ciphertext owners
    /// must be that bundle and this queued write, not a submitted operation/reader.
    pub(crate) fn discard_idle_copy(
        &self,
        id: &PageId,
        ciphertext: &crate::memory::CiphertextPage,
    ) -> usize {
        if self.protected_pending(id) {
            return 0;
        }
        let mut queue = self.queue.borrow_mut();
        let Some(position) = queued_position(&queue, id) else {
            return 0;
        };
        let mut pending = self.pending.borrow_mut();
        if pending.get(id).is_some_and(|dirty| {
            std::sync::Arc::ptr_eq(&dirty.page.ciphertext.inner, &ciphertext.inner)
                && std::sync::Arc::strong_count(&ciphertext.inner) == 2
        }) {
            queue.remove(position);
            let dirty = pending.remove(id).expect("located queued copy");
            self.note_discard(1);
            return dirty.staging_bytes();
        }
        0
    }
    /// Release only enough queued ciphertext/staging charges to cover a deficit.
    /// Ownership-only classification never inserts a sighting or touches heat.
    pub fn reclaim_optional_for_owned(
        &self,
        incoming: &PageId,
        cache: Option<&CacheId>,
        dirty_bytes: usize,
        staging_bytes: usize,
    ) -> usize {
        let retention = self.retention();
        if (dirty_bytes == 0 && staging_bytes == 0) || !retention.is_owned(incoming) {
            return 0;
        }
        let scoring = self.scoring(&retention);
        // Mirror the full-queue rule in `enqueue`: an unheld over-share page
        // never displaces an under-share page, so it may only take over-share
        // victims. A held incoming page is exempt there too.
        let over_share_only =
            !self.index.is_held(incoming) && scoring.over_share(&incoming.version.object.cache);
        let mut queue = self.queue.borrow_mut();
        let mut pending = self.pending.borrow_mut();
        let mut candidates: Vec<_> = queue
            .iter()
            .take(QUEUE_RECLAIM_CANDIDATES)
            .filter(|id| {
                cache.is_none_or(|cache| cache == &id.version.object.cache)
                    && (!over_share_only || scoring.over_share(&id.version.object.cache))
                    && !retention.is_owned(id)
                    && !self.index.is_held(id)
                    && pending.get(*id).is_none_or(|dirty| dirty.hold.is_none())
            })
            .cloned()
            .collect();
        // Release over-share pages first; the stable sort keeps queue order otherwise.
        candidates.sort_by_cached_key(|id| !scoring.over_share(&id.version.object.cache));
        let (mut dirty_released, mut staging_released, mut removed) = (0usize, 0usize, 0usize);
        for id in candidates {
            if dirty_released >= dirty_bytes && staging_released >= staging_bytes {
                break;
            }
            if let Some(dirty) = pending.remove(&id) {
                dirty_released = dirty_released.saturating_add(dirty._reservation.amount());
                staging_released = staging_released.saturating_add(dirty.staging_bytes());
                removed += 1;
            }
            if let Some(position) = queued_position(&queue, &id) {
                queue.remove(position);
            }
        }
        self.note_discard(removed);
        removed
    }
    /// Release only enough queued ciphertext/staging charges to cover a deficit.
    /// Submitted writes are absent from queue and keep all completion-owned charges.
    pub(crate) fn reclaim_ciphertext(&self, cache: Option<&CacheId>, bytes: usize) -> usize {
        let pooled = self.slabs.reclaim_idle();
        if pooled >= bytes {
            return pooled;
        }
        let mut released = pooled;
        let mut removed = 0;
        let mut pending = self.pending.borrow_mut();
        let retention = self.retention();
        let scoring = self.scoring(&retention);
        let mut queue = self.queue.borrow_mut();
        let mut candidates: Vec<_> = queue
            .iter()
            .take(QUEUE_RECLAIM_CANDIDATES)
            .filter(|id| cache.is_none_or(|cache| cache == &id.version.object.cache))
            .filter(|id| !self.index.is_held(id))
            .filter(|id| pending.get(*id).is_none_or(|dirty| dirty.hold.is_none()))
            .map(|id| (scoring.rank(id), id.clone()))
            .collect();
        candidates.sort_by_key(|(rank, _)| *rank);
        for (_, id) in candidates {
            if released >= bytes {
                break;
            }
            if let Some(dirty) = pending.remove(&id) {
                released = released.saturating_add(dirty.staging_bytes());
                if std::sync::Arc::strong_count(&dirty.page.ciphertext.inner) == 1 {
                    released =
                        released.saturating_add(dirty.page.ciphertext.inner.reservation().amount());
                }
                removed += 1;
            }
            // Every candidate came from this prefix and removals only move it
            // closer to the front. Never scan the unselected queue suffix.
            if let Some(position) = queued_position(&queue, &id) {
                queue.remove(position);
            }
        }
        self.note_discard(removed);
        released
    }
    /// Drive at most `budget` writes. At most [`WRITER_IN_FLIGHT_WRITES`] writes
    /// and [`WRITER_IN_FLIGHT_BYTES`] record bytes are in flight, and the next
    /// write starts as soon as any completes. Keep polling this future through
    /// reactor completion.
    pub fn progress<'a>(&'a self, budget: usize, scope: &'a RequestScope) -> Operation<'a, usize> {
        self.progress_bounded(
            budget,
            WRITER_IN_FLIGHT_WRITES,
            WRITER_IN_FLIGHT_BYTES,
            scope,
        )
    }
    /// Like [`Self::progress`] with explicit in-flight count and byte limits.
    /// One write is always admitted, so an oversized record still progresses.
    pub(crate) fn progress_bounded<'a>(
        &'a self,
        budget: usize,
        lanes: usize,
        max_bytes: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, usize> {
        Box::pin(async move {
            if self.busy.replace(true) {
                return Err(Error::Overloaded);
            }
            *self.active_scope.borrow_mut() = Some(scope.clone());
            let _busy = Busy(self);
            use futures::StreamExt;
            use futures::stream::FuturesUnordered;
            let mut writes = FuturesUnordered::new();
            let mut started = 0;
            let mut in_flight_bytes = 0usize;
            let mut stopped = false;
            let mut completed = 0;
            let mut failure = None;
            loop {
                while !stopped && writes.len() < lanes.max(1) && started < budget {
                    if scope.check().is_err() {
                        self.discard_unsubmitted();
                        stopped = true;
                        break;
                    }
                    let position = {
                        let queue = self.queue.borrow();
                        let retention = self.retention();
                        let scoring = self.scoring(&retention);
                        // Reverse the tie breaker to keep FIFO within equal value.
                        queue
                            .iter()
                            .enumerate()
                            .take(QUEUE_RECLAIM_CANDIDATES)
                            .max_by_key(|(position, id)| {
                                (scoring.rank(id), std::cmp::Reverse(*position))
                            })
                            .map(|(position, _)| position)
                    };
                    let Some(position) = position else {
                        break;
                    };
                    let Some(id) = self.queue.borrow().get(position).cloned() else {
                        break;
                    };
                    // Errors below stop new work but drain the writes in
                    // flight: returning early would drop them part way.
                    let copy = match self.copy_only(&id) {
                        Ok(copy) => copy,
                        Err(error) => {
                            failure = Some(error);
                            stopped = true;
                            break;
                        }
                    };
                    let bytes = copy
                        .as_ref()
                        .map_or(0, |page| logical_length(page).unwrap_or(0));
                    // Always admit one write so an oversized record still makes progress.
                    if !writes.is_empty() && in_flight_bytes.saturating_add(bytes) > max_bytes {
                        break;
                    }
                    self.queue.borrow_mut().remove(position);
                    started += 1;
                    let page = match copy {
                        Some(p) => p,
                        None => {
                            self.pending.borrow_mut().remove(&id);
                            continue;
                        }
                    };
                    // Install cleanup BEFORE allocation, reclamation or submission.
                    // Every attempted copy leaves the queue, including disposable failures.
                    let Some(ticket) = self.pending.borrow().get(&id).map(|d| d.ticket) else {
                        failure = Some(Error::Unavailable);
                        stopped = true;
                        break;
                    };
                    let mut cleanup = DirtyCleanup {
                        writer: self,
                        persisted: false,
                        page: id.clone(),
                        ticket,
                    };
                    in_flight_bytes += bytes;
                    writes.push(async move {
                        let result = self.persist(&id, &page, scope).await;
                        cleanup.persisted = result.is_ok();
                        drop(cleanup);
                        (bytes, result)
                    });
                }
                let Some((bytes, result)) = writes.next().await else {
                    break;
                };
                in_flight_bytes -= bytes;
                completed += 1;
                // Quota/space pressure and cache I/O failures never fail the node.
                if let Err(error) = result {
                    if !matches!(
                        error,
                        Error::Overloaded
                            | Error::Io
                            | Error::Os(_)
                            | Error::Unavailable
                            | Error::MissingKey
                            | Error::Cancelled
                            | Error::DeadlineExceeded
                    ) {
                        failure = Some(error);
                        stopped = true;
                    }
                    if matches!(error, Error::Cancelled | Error::DeadlineExceeded) {
                        self.discard_unsubmitted();
                        stopped = true;
                    }
                }
            }
            failure.map_or(Ok(completed), Err)
        })
    }
    async fn persist(
        &self,
        id: &PageId,
        page: &CiphertextCopy,
        scope: &RequestScope,
    ) -> Result<()> {
        // Pages beyond the key's page range are never stored.
        if key::volume_key(id).is_none() {
            return Ok(());
        }
        // Recovery or another publisher may have written the page since enqueue.
        if self.index.already_on_disk_async(id, scope).await? {
            return Ok(());
        }
        let alignment = self.slabs.alignment()?;
        let disk_bytes = record_length(page, alignment)?;
        let (staging, dirty, ticket) = {
            let mut pending = self.pending.borrow_mut();
            let entry = pending.get_mut(id).ok_or(Error::Unavailable)?;
            (
                entry.staging.take().ok_or(Error::InvalidConfiguration)?,
                entry._reservation.clone(),
                entry.ticket,
            )
        };
        if !self.admission.owns(&staging) {
            return Err(Error::InvalidConfiguration);
        }
        let zero_copy = zero_copy_body(page, alignment)?.is_some();
        // Zero-copy pages stage only the header block; enqueue reserved exactly that.
        let staged = if zero_copy {
            header_block_len(page, alignment)?
        } else {
            disk_bytes
        };
        let mut buffer = self.slabs.allocate(staged, staging)?;
        buffer.retain(dirty);
        // A page of a live hold with unused capacity goes to the storage tier.
        let hold = self.index.hold_ticket(id);
        let tier = if hold.is_some() {
            Tier::Storage
        } else {
            Tier::Cache
        };
        let reservation = self.index.allocate(tier, disk_bytes as u64, scope).await?;
        let generation = self.index.next_generation()?;
        let volume = self.index.volume().ok_or(Error::Unavailable)?;
        if zero_copy {
            write_header(page, Generation(generation), alignment, buffer.bytes_mut()?)?;
            self.metrics
                .record(crate::telemetry::Event::PersistZeroCopy, 1);
            let body = DiskBody {
                page: page.ciphertext.clone(),
                alignment,
                length: disk_bytes - header_block_len(page, alignment)?,
            };
            let _ = volume
                .write_vectored(&*self.reactor, &reservation, buffer, body, scope)
                .await?;
        } else {
            self.metrics
                .record(crate::telemetry::Event::PersistCopyFallback, 1);
            let encoded = encode_at(page, Generation(generation), alignment, 0, buffer)?;
            let _ = volume
                .write(&*self.reactor, &reservation, encoded.buffer, scope)
                .await?;
        }
        if self.allowed(page)
            && self
                .pending
                .borrow()
                .get(id)
                .is_some_and(|d| d.ticket == ticket)
            && self.index.publish_page(
                id,
                reservation,
                u32::try_from(disk_bytes).map_err(|_| Error::InvalidConfiguration)?,
                generation,
                &page.metadata.immutable(),
                page.ciphertext.envelope().key_id,
            )?
        {
            drop(hold);
            self.metrics
                .record(crate::telemetry::Event::DiskPublication, 1);
            self.retention()
                .published(id, u64::from(page.ciphertext.envelope().plaintext_length));
        } else if self.pending.borrow().contains_key(id) {
            self.note_discard(1);
        }
        Ok(())
    }
    pub fn drain<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.stop_admission();
            if scope.check().is_err() {
                self.cancel_pending_writes()?;
            }
            // An application-held progress future must be polled alongside drain.
            std::future::poll_fn(|cx| {
                if scope.check().is_err()
                    && let Err(error) = self.cancel_pending_writes()
                {
                    return std::task::Poll::Ready(Err(error));
                }
                if !self.busy.get() {
                    std::task::Poll::Ready(Ok(()))
                } else {
                    cx.waker().wake_by_ref();
                    std::task::Poll::Pending
                }
            })
            .await?;
            while self.pending_count() != 0 {
                scope.check()?;
                self.progress(WRITER_PROGRESS_BUDGET, scope).await?;
            }
            self.slabs.fence_writes().await?;
            // Make cache writes durable on a clean shutdown. Cache I/O failures
            // are not fatal; the next mount simply finds fewer pages.
            match self.commit(scope).await {
                Err(error) if !is_disposable_commit_error(&error) => return Err(error),
                _ => {}
            }
            // Commit node buffers return to the slab's idle pool still charged.
            self.slabs.reclaim_idle();
            Ok(())
        })
    }
    /// Reserve or extend a hold with holders of `kind` and make every
    /// retention change so far durable before returning the reservation's
    /// epoch. The commit runs even when this call changed nothing on disk: an
    /// earlier extension whose commit failed is still in memory, and a caller
    /// acknowledging the same or a shorter deadline must not report it before
    /// it is durable. Committing a clean volume returns immediately.
    pub fn reserve_hold<'a>(
        &'a self,
        version: &'a ObjectVersion,
        pages: &'a holds::HoldPages,
        until: std::time::SystemTime,
        kind: holds::HolderKind,
        scope: &'a RequestScope,
    ) -> Operation<'a, holds::HoldEpoch> {
        Box::pin(async move {
            let epoch = self
                .index
                .reserve_hold_kind(version, pages, until, kind, scope)
                .await?;
            self.commit(scope).await?;
            Ok(epoch)
        })
    }
    /// Make every disk index change so far durable. The commit's node buffers
    /// return to the slab's idle pool still charged; admission pressure and
    /// drain reclaim them.
    /// A failure that is not an I/O error of its own (for example a damaged
    /// node read back) is reported as `Io`: the commit failed, not the data.
    pub fn commit<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.index
                .commit(&self.reactor, scope)
                .await
                .map_err(|error| match error {
                    Error::CorruptRecord => Error::Io,
                    other => other,
                })
        })
    }
    pub fn remove_cache(&self, cache: &CacheId) -> Result<()> {
        let mut pending = self.pending.borrow_mut();
        let before = pending.len();
        pending.retain(|p, _| &p.version.object.cache != cache);
        self.note_discard(before - pending.len());
        self.queue
            .borrow_mut()
            .retain(|p| &p.version.object.cache != cache);
        self.index.remove_cache(cache);
        Ok(())
    }
    pub fn is_idle(&self) -> bool {
        !self.busy.get() && self.pending_count() == 0 && self.writes_in_flight() == 0
    }
    /// Idle, zeroized staging remains charged until reuse, pressure, or drain.
    pub fn retained_staging_bytes(&self) -> usize {
        self.slabs.idle_bytes()
    }
    /// Read a page's record from the slab, edit its bytes, and write it back
    /// in place, bypassing the volume, to simulate media corruption.
    #[cfg(test)]
    pub(crate) fn rewrite_record<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
        edit: impl FnOnce(&mut [u8]) + 'a,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            let (offset, length) = self.index.record_extent(page).ok_or(Error::Unavailable)?;
            let length = length as usize;
            let charge = self
                .admission
                .reserve(None, ResourceClass::Ciphertext, length)?;
            let buffer = self.slabs.allocate(length, charge)?;
            let mut buffer = self
                .slabs
                .read_at(&*self.reactor, offset, buffer, (), scope)
                .await?;
            edit(buffer.bytes_mut()?);
            let _ = self
                .slabs
                .write_at(&*self.reactor, offset, buffer, (), scope)
                .await?;
            Ok(())
        })
    }
    /// Run idle staging demand maintenance; O(1) between windows.
    pub fn maintain_staging(&self, now: std::time::Instant) -> usize {
        self.slabs.maintain_idle(now)
    }
    /// Hold idle staging empty while memory is constrained.
    pub fn constrain_staging(&self, constrained: bool) {
        self.slabs.constrain_idle(constrained);
    }
    /// Idle staging size, demand target, and cumulative counters.
    pub fn staging_pool_stats(&self) -> flow_control::PoolStats {
        self.slabs.idle_pool_stats()
    }
    #[cfg(test)]
    fn reclaim_idle_buffer(&self) -> usize {
        self.slabs.reclaim_idle()
    }

    fn notify_waiters(&self) {
        let wakes: Vec<_> = self
            .waiters
            .borrow_mut()
            .values_mut()
            .filter_map(Option::take)
            .collect();
        for wake in wakes {
            wake.wake();
        }
    }
}

struct Busy<'a>(&'a StoreWriter);
impl Drop for Busy<'_> {
    fn drop(&mut self) {
        self.0.busy.set(false);
        self.0.active_scope.borrow_mut().take();
        self.0.notify_waiters();
    }
}

struct WriterWaiter<'a> {
    writer: &'a StoreWriter,
    id: u64,
}

impl<'a> WriterWaiter<'a> {
    fn new(writer: &'a StoreWriter) -> Result<Self> {
        let mut waiters = writer.waiters.borrow_mut();
        if waiters.len() >= writer.capacity.get() {
            return Err(Error::Overloaded);
        }
        let id = writer.next_waiter.get();
        writer
            .next_waiter
            .set(id.checked_add(1).ok_or(Error::Unavailable)?);
        waiters.insert(id, None);
        Ok(Self { writer, id })
    }

    async fn ready(&self) {
        std::future::poll_fn(|cx| {
            if !self.writer.busy.get() {
                return std::task::Poll::Ready(());
            }
            let wake = cx.waker().clone();
            let old = self
                .writer
                .waiters
                .borrow_mut()
                .get_mut(&self.id)
                .expect("registered writer waiter")
                .replace(wake);
            drop(old);
            std::task::Poll::Pending
        })
        .await;
    }
}

impl Drop for WriterWaiter<'_> {
    fn drop(&mut self) {
        let removed = self.writer.waiters.borrow_mut().remove(&self.id);
        drop(removed);
    }
}
struct DirtyCleanup<'a> {
    writer: &'a StoreWriter,
    persisted: bool,
    page: PageId,
    ticket: u64,
}
impl Drop for DirtyCleanup<'_> {
    fn drop(&mut self) {
        let mut pending = self.writer.pending.borrow_mut();
        if pending
            .get(&self.page)
            .is_some_and(|dirty| dirty.ticket == self.ticket)
        {
            pending.remove(&self.page);
            if !self.persisted {
                self.writer.note_discard(1);
            }
        }
    }
}
// Page record format version, stored after the magic. Readers treat any other
// version as a cold miss.
//
// Version 2 layout:
// - `[0, real)`: header fields followed by a SHA-256 digest of the preceding
//   header bytes. `real` is at most `MAX_RAW_HEADER_BYTES`.
// - `[real, header_len)`: zero padding. `header_len` (stored at bytes 12..16)
//   is `real` rounded up to the store's record unit, and the digest does not
//   cover the padding; readers require it to be zero.
// - `[header_len, header_len + ciphertext_length)`: ciphertext, so it starts at
//   a disk-aligned offset and can be written from its own aligned buffer.
// - The remainder up to the record extent is zero padding.
pub const FORMAT_VERSION: u32 = 2;
pub const MAX_ID_BYTES: usize = 4096;
pub const MAX_ETAG_BYTES: usize = 8192;
/// Upper bound on the unpadded header, including its digest.
pub const MAX_RAW_HEADER_BYTES: usize = 16384;
/// Upper bound on the padded header block that precedes the ciphertext.
/// Record size budgets use this as the per-record overhead.
pub const MAX_HEADER_BYTES: usize = MAX_RAW_HEADER_BYTES + 4096;
/// Smallest record unit; every stored `header_len` is a multiple of it.
const MIN_RECORD_UNIT: usize = 512;
/// Ciphertext alignment that cuFile needs for direct-to-GPU reads. A record
/// whose ciphertext starts at a multiple of this on the device can be read
/// straight into GPU memory; others are read through host memory.
pub const CIPHERTEXT_ALIGNMENT: usize = 4096;

/// Largest encoded page record: a full page of ciphertext, its 16-byte AEAD
/// tag, and the largest header.
pub const MAX_RECORD_BYTES: usize = crate::model::PAGE_BYTES as usize + 16 + MAX_HEADER_BYTES;

/// Ciphertext body that can be written in place, without a staging copy.
///
/// Returns the padded body when the page already lives in direct-I/O aligned
/// storage whose padded length matches the record body for `alignment`.
fn zero_copy_body(page: &CiphertextCopy, alignment: Alignment) -> Result<Option<&[u8]>> {
    Ok(disk_body_prefix(
        &page.ciphertext,
        alignment,
        body_len(page, alignment)?,
    ))
}
/// The first `length` bytes of the aligned padded body: the ciphertext followed
/// only by zeros. The pool may pad to a coarser unit than the device, so a
/// longer padded body still serves a shorter unit-aligned record body.
fn disk_body_prefix(
    page: &crate::memory::CiphertextPage,
    alignment: Alignment,
    length: usize,
) -> Option<&[u8]> {
    page.disk_body(alignment)
        .filter(|body| body.len() >= length && length.is_multiple_of(alignment.length()))
        .map(|body| &body[..length])
}
/// Staging bytes reserved before queue acceptance and consumed by persist.
///
/// Zero-copy pages only need the padded header block because the page owns
/// its aligned body; other pages need the full encoded record.
pub(crate) fn staging_bytes(page: &CiphertextCopy, alignment: Alignment) -> Result<usize> {
    if zero_copy_body(page, alignment)?.is_some() {
        staging_capacity(header_block_len(page, alignment)?)
    } else {
        staging_capacity(record_length(page, alignment)?)
    }
}
/// Immutable, Arc-held page body handed to a vectored device write.
struct DiskBody {
    page: crate::memory::CiphertextPage,
    alignment: Alignment,
    length: usize,
}
// SAFETY: the Arc-held ciphertext storage is never mutated after
// construction, so the returned slice is stable while `self` is alive.
unsafe impl uring_runtime::reactor::SharedWriteBuffer for DiskBody {
    type Error = Error;
    fn write_bytes(&self) -> Result<&[u8]> {
        disk_body_prefix(&self.page, self.alignment, self.length).ok_or(Error::InvalidConfiguration)
    }
}
/// Large records share capacity classes while their disk extents stay exact.
fn staging_capacity(length: usize) -> Result<usize> {
    if length < 2 * 1024 * 1024 {
        return Ok(length);
    }
    page_alloc::buffer_capacity(length).ok_or(Error::Overloaded)
}
const MAGIC: &[u8; 8] = b"RCRPAGE1";
const HEADER_PREFIX_BYTES: usize = 128;
const HEADER_DIGEST_BYTES: usize = 32;
#[derive(Clone, Debug)]
pub struct RecordHeader {
    pub format_version: u32,
    pub generation: Generation,
    pub envelope: PageEnvelope,
    pub metadata: VersionMetadata,
    pub logical_bytes: u64,
    pub extent: Extent,
}
pub struct EncodedRecord {
    pub header: RecordHeader,
    pub buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
}
pub struct DecodedRecord {
    pub header: RecordHeader,
    pub ciphertext: std::ops::Range<usize>,
    pub checksum: u64,
}
impl DecodedRecord {
    /// Ciphertext byte range relative to the record start, if the record's
    /// header block places the ciphertext at a CIPHERTEXT_ALIGNMENT boundary
    /// and so supports direct-to-GPU reads.
    pub fn direct_range(&self) -> Option<std::ops::Range<u64>> {
        (self.header.format_version == FORMAT_VERSION
            && self.ciphertext.start.is_multiple_of(CIPHERTEXT_ALIGNMENT))
        .then_some(self.ciphertext.start as u64..self.ciphertext.end as u64)
    }
}
/// Where a published record's ciphertext lives on its backing file or device.
/// Returned only when the header block is a multiple of CIPHERTEXT_ALIGNMENT.
/// The descriptor is the worker-local O_DIRECT
/// descriptor the slab reads through, suitable for registration with cuFile.
pub struct DirectCiphertext {
    pub descriptor: Rc<uring_runtime::reactor::descriptor::Descriptor>,
    /// Absolute byte offset of the first ciphertext byte on `descriptor`.
    pub offset: u64,
    /// Ciphertext length in bytes, including the AEAD tag.
    pub length: usize,
    /// Whether `offset` is a multiple of CIPHERTEXT_ALIGNMENT. This holds for
    /// every such record whose physical start is 4 KiB aligned. See
    /// `direct_ciphertext` for when that is guaranteed.
    pub aligned: bool,
}
/// Translate a parsed record into its absolute on-device ciphertext location.
///
/// Returns Ok(None) when the record's header block is not a multiple of
/// CIPHERTEXT_ALIGNMENT; such records must be read through host memory. The
/// caller must hold a lease on `value`'s slot until any direct transfer
/// completes. Record extents and header blocks are multiples of the record
/// unit, so the ciphertext is 4 KiB aligned on disk whenever the record unit is
/// a multiple of 4 KiB and the slot's physical start is 4 KiB aligned.
/// `aligned` reports the actual result rather than assuming it.
pub fn direct_ciphertext(
    slabs: &Slab<flow_control::Charge<AdmissionPolicy>>,
    value: &page_alloc::volume::Value,
    record: &DecodedRecord,
) -> Result<Option<DirectCiphertext>> {
    if record.header.extent.length() != value.length as usize {
        return Err(Error::CorruptRecord);
    }
    let Some(range) = record.direct_range() else {
        return Ok(None);
    };
    let (descriptor, physical) = slabs.locate_at(value.offset(), value.length as usize)?;
    let offset = physical
        .offset()
        .checked_add(range.start)
        .ok_or(Error::CorruptRecord)?;
    Ok(Some(DirectCiphertext {
        descriptor,
        offset,
        length: (range.end - range.start) as usize,
        aligned: offset.is_multiple_of(CIPHERTEXT_ALIGNMENT as u64),
    }))
}
struct RecordLayout {
    /// Unpadded header length, including the digest.
    header_bytes: usize,
    /// Header length padded to the record unit; ciphertext starts here.
    header_block: usize,
    /// Ciphertext length padded to the record unit.
    body_bytes: usize,
    /// `header_block` plus the unpadded ciphertext length.
    logical_bytes: usize,
}
/// Record unit: a multiple of the extent granularity, so every in-record
/// offset that is a multiple of it is both offset- and length-aligned on disk.
/// It is at least CIPHERTEXT_ALIGNMENT, so header blocks and record extents are
/// 4 KiB multiples and ciphertext stays eligible for direct-to-GPU reads on
/// devices with smaller sectors.
fn record_unit(alignment: Alignment) -> Result<usize> {
    let unit = alignment.extent(0, 1)?.length();
    if !unit.is_multiple_of(MIN_RECORD_UNIT) {
        return Err(Error::InvalidConfiguration);
    }
    if CIPHERTEXT_ALIGNMENT.is_multiple_of(unit) {
        Ok(CIPHERTEXT_ALIGNMENT)
    } else {
        Ok(unit)
    }
}
fn align_up(len: usize, unit: usize) -> Result<usize> {
    len.checked_next_multiple_of(unit)
        .ok_or(Error::CorruptRecord)
}
fn layout(
    page: &CiphertextCopy,
    generation: Generation,
    alignment: Alignment,
) -> Result<RecordLayout> {
    layout_with_unit(page, generation, record_unit(alignment)?)
}
fn layout_with_unit(
    page: &CiphertextCopy,
    generation: Generation,
    unit: usize,
) -> Result<RecordLayout> {
    let raw = raw_layout(page, generation)?;
    let header_block = align_up(raw, unit)?;
    if header_block > MAX_HEADER_BYTES {
        return Err(Error::InvalidConfiguration);
    }
    let ciphertext = page.ciphertext.bytes().len();
    let body_bytes = align_up(ciphertext, unit)?;
    Ok(RecordLayout {
        header_bytes: raw,
        header_block,
        body_bytes,
        logical_bytes: header_block
            .checked_add(ciphertext)
            .ok_or(Error::CorruptRecord)?,
    })
}
/// Validate the page and return its unpadded header length.
fn raw_layout(page: &CiphertextCopy, generation: Generation) -> Result<usize> {
    let envelope = page.ciphertext.envelope();
    let metadata = page.metadata.immutable();
    metadata.validate_page(envelope)?;
    if generation.0 == 0 || page.ciphertext.bytes().len() != envelope.ciphertext_length as usize {
        return Err(Error::CorruptRecord);
    }
    let cache = envelope.page.version.object.cache.0.as_bytes();
    let etag = envelope.page.version.etag.as_bytes();
    if cache.is_empty()
        || cache.len() > MAX_ID_BYTES
        || etag.is_empty()
        || etag.len() > MAX_ETAG_BYTES
    {
        return Err(Error::CorruptRecord);
    }
    let header_bytes = HEADER_PREFIX_BYTES
        .checked_add(12)
        .ok_or(Error::CorruptRecord)?
        .checked_add(cache.len())
        .and_then(|len| len.checked_add(etag.len()))
        .and_then(|len| {
            len.checked_add(
                metadata
                    .content_type
                    .as_ref()
                    .map_or(4, |v| 4 + v.as_bytes().len()),
            )
        })
        .and_then(|len| len.checked_add(HEADER_DIGEST_BYTES))
        .filter(|&len| len <= MAX_RAW_HEADER_BYTES)
        .ok_or(Error::CorruptRecord)?;
    Ok(header_bytes)
}
/// Padded header block length; the ciphertext starts at this record offset.
pub(crate) fn header_block_len(page: &CiphertextCopy, alignment: Alignment) -> Result<usize> {
    Ok(layout(page, Generation(1), alignment)?.header_block)
}
/// Ciphertext length padded to the record unit.
pub(crate) fn body_len(page: &CiphertextCopy, alignment: Alignment) -> Result<usize> {
    Ok(layout(page, Generation(1), alignment)?.body_bytes)
}
/// Full on-disk record length: header block plus padded ciphertext.
pub fn record_length(page: &CiphertextCopy, alignment: Alignment) -> Result<usize> {
    let layout = layout(page, Generation(1), alignment)?;
    layout
        .header_block
        .checked_add(layout.body_bytes)
        .ok_or(Error::CorruptRecord)
}
/// Write the header and its zero padding into `out`, which must be exactly
/// `header_block_len(page, alignment)` bytes.
pub(crate) fn write_header(
    page: &CiphertextCopy,
    generation: Generation,
    alignment: Alignment,
    out: &mut [u8],
) -> Result<()> {
    let layout = layout(page, generation, alignment)?;
    write_header_layout(page, generation, &layout, out)
}
fn write_header_layout(
    page: &CiphertextCopy,
    generation: Generation,
    layout: &RecordLayout,
    out: &mut [u8],
) -> Result<()> {
    if out.len() != layout.header_block {
        return Err(Error::InvalidConfiguration);
    }
    let header = header_bytes(page, generation, layout);
    out[..header.len()].copy_from_slice(&header);
    out[header.len()..].fill(0);
    Ok(())
}
fn header_bytes(page: &CiphertextCopy, generation: Generation, layout: &RecordLayout) -> Vec<u8> {
    let envelope = page.ciphertext.envelope();
    let cache = envelope.page.version.object.cache.0.as_bytes();
    let etag = envelope.page.version.etag.as_bytes();
    let mut out = Vec::with_capacity(layout.header_bytes);
    out.extend_from_slice(MAGIC);
    out.extend_from_slice(&FORMAT_VERSION.to_le_bytes());
    out.extend_from_slice(&(layout.header_block as u32).to_le_bytes());
    out.extend_from_slice(&generation.0.to_le_bytes());
    out.extend_from_slice(&page.metadata.length.to_le_bytes());
    out.extend_from_slice(&envelope.page.number.0.to_le_bytes());
    out.extend_from_slice(&envelope.plaintext_length.to_le_bytes());
    out.extend_from_slice(&envelope.ciphertext_length.to_le_bytes());
    out.extend_from_slice(&envelope.key_id.0);
    out.extend_from_slice(&envelope.nonce.0);
    out.extend_from_slice(&envelope.page.version.object.key.0);
    out.extend_from_slice(&(cache.len() as u32).to_le_bytes());
    out.extend_from_slice(&(etag.len() as u32).to_le_bytes());
    out.extend_from_slice(cache);
    out.extend_from_slice(etag);
    out.extend_from_slice(&page.ciphertext.checksum().to_le_bytes());
    out.extend_from_slice(
        &(page
            .metadata
            .content_type
            .as_ref()
            .map_or(0, |v| v.as_bytes().len()) as u32)
            .to_le_bytes(),
    );
    if let Some(content_type) = &page.metadata.content_type {
        out.extend_from_slice(content_type.as_bytes());
    }
    out.extend_from_slice(&(envelope.cipher as u32).to_le_bytes());
    debug_assert_eq!(out.len() + HEADER_DIGEST_BYTES, layout.header_bytes);
    let digest = Sha256::digest(&out);
    out.extend_from_slice(&digest);
    out
}
/// Unpadded header plus ciphertext length. This validates the page and bounds
/// its size independent of disk alignment; use [`record_length`] for the
/// on-disk size.
pub fn logical_length(page: &CiphertextCopy) -> Result<usize> {
    raw_layout(page, Generation(1))?
        .checked_add(page.ciphertext.bytes().len())
        .ok_or(Error::CorruptRecord)
}
pub fn encode(
    page: &CiphertextCopy,
    generation: Generation,
    alignment: Alignment,
    buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
) -> Result<EncodedRecord> {
    encode_at(page, generation, alignment, 0, buffer)
}
pub fn encode_at(
    page: &CiphertextCopy,
    generation: Generation,
    alignment: Alignment,
    offset: u64,
    buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
) -> Result<EncodedRecord> {
    let layout = layout(page, generation, alignment)?;
    encode_layout(page, generation, alignment, offset, buffer, layout)
}
/// Encode with the extent unit as the record unit, the layout written before
/// the record unit had a 4 KiB floor. Readers must keep accepting it.
#[cfg(test)]
pub(crate) fn encode_unit_aligned_at(
    page: &CiphertextCopy,
    generation: Generation,
    alignment: Alignment,
    offset: u64,
    buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
) -> Result<EncodedRecord> {
    let layout = layout_with_unit(page, generation, alignment.extent(0, 1)?.length())?;
    encode_layout(page, generation, alignment, offset, buffer, layout)
}
fn encode_layout(
    page: &CiphertextCopy,
    generation: Generation,
    alignment: Alignment,
    offset: u64,
    mut buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
    layout: RecordLayout,
) -> Result<EncodedRecord> {
    let logical_bytes = layout.logical_bytes;
    let extent = alignment.extent(offset, layout.header_block + layout.body_bytes)?;
    if extent.length() != layout.header_block + layout.body_bytes {
        return Err(Error::InvalidConfiguration);
    }
    alignment.check(extent, &buffer)?;
    let bytes = buffer.bytes_mut()?;
    write_header_layout(page, generation, &layout, &mut bytes[..layout.header_block])?;
    bytes[layout.header_block..logical_bytes].copy_from_slice(page.ciphertext.bytes());
    bytes[logical_bytes..].fill(0);
    Ok(EncodedRecord {
        header: RecordHeader {
            format_version: FORMAT_VERSION,
            generation,
            envelope: page.ciphertext.envelope().clone(),
            metadata: page.metadata.immutable(),
            logical_bytes: logical_bytes as u64,
            extent,
        },
        buffer,
    })
}
pub fn parse(
    buffer: &AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
    extent: Extent,
) -> Result<DecodedRecord> {
    parse_bytes(buffer.bytes()?, extent)
}
/// Parse and validate a whole record of `extent`'s length.
pub fn parse_bytes(bytes: &[u8], extent: Extent) -> Result<DecodedRecord> {
    if bytes.len() != extent.length() {
        return Err(Error::CorruptRecord);
    }
    parse_header(bytes, extent)
}
/// Parse and validate a record header from a prefix of the record at `extent`,
/// without touching the ciphertext. The prefix must cover the whole header
/// block; reading min(MAX_HEADER_BYTES, extent length) bytes always does. This
/// lets a direct-to-GPU reader validate framing through host memory and then
/// transfer only `DecodedRecord::ciphertext`. The ciphertext checksum is not
/// verified here.
pub fn parse_prefix(prefix: &[u8], extent: Extent) -> Result<DecodedRecord> {
    if prefix.len() > extent.length() {
        return Err(Error::CorruptRecord);
    }
    parse_header(prefix, extent)
}
fn parse_header(bytes: &[u8], extent: Extent) -> Result<DecodedRecord> {
    let mut r = Decoder(bytes);
    if r.take(8)? != MAGIC {
        return Err(Error::CorruptRecord);
    }
    let format_version = r.u32()?;
    if format_version != FORMAT_VERSION {
        return Err(Error::CorruptRecord);
    }
    let header_len = r.u32()? as usize;
    if !(MIN_RECORD_UNIT..=MAX_HEADER_BYTES).contains(&header_len)
        || !header_len.is_multiple_of(MIN_RECORD_UNIT)
        || header_len > bytes.len()
    {
        return Err(Error::CorruptRecord);
    }
    // Fields are self-delimiting; the digest ends the real header.
    let mut fields = Decoder(&bytes[16..header_len]);
    let real = 16 + header_fields_len(&mut fields)? + HEADER_DIGEST_BYTES;
    if real > header_len || real > MAX_RAW_HEADER_BYTES {
        return Err(Error::CorruptRecord);
    }
    let digest = Sha256::digest(&bytes[..real - HEADER_DIGEST_BYTES]);
    if digest[..] != bytes[real - HEADER_DIGEST_BYTES..real]
        || bytes[real..header_len].iter().any(|&b| b != 0)
    {
        return Err(Error::CorruptRecord);
    }
    r.0 = &bytes[16..real - HEADER_DIGEST_BYTES];
    let generation = Generation(r.u64()?);
    let length = r.u64()?;
    let number = PageNumber(r.u64()?);
    let plaintext_length = r.u32()?;
    let ciphertext_length = r.u32()?;
    let key_id = KeyId(r.array()?);
    let nonce = Nonce(r.array()?);
    let key = CacheKey(r.array()?);
    let cache_len = r.u32()? as usize;
    let etag_len = r.u32()? as usize;
    if cache_len == 0 || cache_len > MAX_ID_BYTES || etag_len == 0 || etag_len > MAX_ETAG_BYTES {
        return Err(Error::CorruptRecord);
    }
    let cache = CacheId(
        std::str::from_utf8(r.take(cache_len)?)
            .map_err(|_| Error::CorruptRecord)?
            .to_owned(),
    );
    let etag = StrongEtag::parse(r.take(etag_len)?).map_err(|_| Error::CorruptRecord)?;
    let checksum = r.u64()?;
    let content_type_length = r.u32()? as usize;
    let content_type = if content_type_length == 0 {
        None
    } else {
        Some(
            crate::model::ContentType::parse(r.take(content_type_length)?)
                .map_err(|_| Error::CorruptRecord)?,
        )
    };
    let cipher = crate::model::PageCipher::try_from(r.u32()?)?;
    if !r.0.is_empty() || generation.0 == 0 {
        return Err(Error::CorruptRecord);
    }
    let version = ObjectVersion {
        object: ObjectId { cache, key },
        etag,
    };
    let metadata = VersionMetadata {
        content_type,
        version: version.clone(),
        length,
    };
    let envelope = PageEnvelope {
        cipher,
        page: PageId { version, number },
        key_id,
        nonce,
        plaintext_length,
        ciphertext_length,
    };
    metadata.validate_page(&envelope)?;
    let logical_bytes = header_len
        .checked_add(ciphertext_length as usize)
        .ok_or(Error::CorruptRecord)?;
    if logical_bytes > extent.length() {
        return Err(Error::CorruptRecord);
    }
    Ok(DecodedRecord {
        header: RecordHeader {
            format_version,
            generation,
            envelope,
            metadata,
            logical_bytes: logical_bytes as u64,
            extent,
        },
        ciphertext: header_len..logical_bytes,
        checksum,
    })
}
/// Length of the variable header fields after the 16-byte fixed prefix and
/// before the digest, read without validating their contents.
fn header_fields_len(r: &mut Decoder<'_>) -> Result<usize> {
    let start = r.0.len();
    r.take(8 + 8 + 8 + 4 + 4)?;
    r.take(std::mem::size_of::<KeyId>())?;
    r.take(std::mem::size_of::<Nonce>())?;
    r.take(std::mem::size_of::<CacheKey>())?;
    let cache_len = r.u32()? as usize;
    let etag_len = r.u32()? as usize;
    if cache_len > MAX_ID_BYTES || etag_len > MAX_ETAG_BYTES {
        return Err(Error::CorruptRecord);
    }
    r.take(cache_len)?;
    r.take(etag_len)?;
    r.take(8)?;
    let content_type_len = r.u32()? as usize;
    if content_type_len > MAX_RAW_HEADER_BYTES {
        return Err(Error::CorruptRecord);
    }
    r.take(content_type_len)?;
    r.take(4)?;
    Ok(start - r.0.len())
}
pub fn decode(
    buffer: &AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
    expected: &RecordHeader,
) -> Result<PageEnvelope> {
    let actual = parse(buffer, expected.extent)?.header;
    if actual.format_version != expected.format_version
        || actual.generation != expected.generation
        || actual.envelope != expected.envelope
        || actual.metadata != expected.metadata
        || actual.logical_bytes != expected.logical_bytes
    {
        return Err(Error::CorruptRecord);
    }
    Ok(actual.envelope)
}
pub(super) struct Decoder<'a>(pub(super) &'a [u8]);
impl<'a> Decoder<'a> {
    fn read<T>(
        &mut self,
        read: impl FnOnce(&mut wire_codec::Reader<'a>) -> wire_codec::Result<T>,
    ) -> Result<T> {
        let mut reader = wire_codec::Reader::new(self.0, wire_codec::Endian::Little);
        let result = read(&mut reader);
        self.0 = reader.remaining();
        result.map_err(|_| Error::CorruptRecord)
    }

    pub(super) fn take(&mut self, len: usize) -> Result<&'a [u8]> {
        self.read(|reader| reader.take(len))
    }
    pub(super) fn array<const N: usize>(&mut self) -> Result<[u8; N]> {
        self.read(|reader| reader.array())
    }
    pub(super) fn u32(&mut self) -> Result<u32> {
        self.read(|reader| reader.u32())
    }
    pub(super) fn u64(&mut self) -> Result<u64> {
        self.read(|reader| reader.u64())
    }
}

/// Label per-cache disk usage with published cache names.
///
/// Builds one id-to-name map per sample so the join stays linear in the
/// catalog size and the usage list. Usage for caches that are no longer
/// published is dropped.
fn cache_disk_samples(
    caches: &[racer_control_wire::CacheDefinition],
    usage: Vec<crate::retention::CacheDisk>,
) -> Vec<crate::telemetry::CacheDiskUsage> {
    let names: std::collections::HashMap<&CacheId, &str> = caches
        .iter()
        .map(|cache| (&cache.id, cache.name.as_str()))
        .collect();
    usage
        .into_iter()
        .filter_map(|usage| {
            let name = names.get(&usage.cache)?;
            Some(crate::telemetry::CacheDiskUsage {
                cache: (*name).into(),
                resident: usage.resident_bytes,
                share: usage.share_bytes,
            })
        })
        .collect()
}
#[cfg(test)]
mod tests;
