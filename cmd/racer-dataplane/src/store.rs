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
    /// Miss on a read that failed or found an unusable record. A cache-tier
    /// entry is removed. A storage-tier entry is a hard hold's durable copy, so
    /// a transient I/O error or a damaged record must not delete it or drop it
    /// from its hold; the read just misses.
    fn miss(&self, token: &ReadToken, tier: Tier) -> Result<Option<(CiphertextCopy, ReadToken)>> {
        if tier == Tier::Cache {
            self.invalidate(token)?;
        }
        Ok(None)
    }
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
                self.index.lookup(page),
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
                Err(error @ (Error::Io | Error::Os(_) | Error::CorruptRecord)) => {
                    if error == Error::CorruptRecord {
                        self.corrupt_miss();
                    }
                    return self.miss(&token, value.tier);
                }
                Err(Error::Unavailable) => return Ok(None),
                Err(e) => return Err(e),
            };
            // A record with this magic but a different format version (for
            // example, written by a newer release) is a cold cache miss, not
            // evidence of damaged storage. Never parse its remaining fields.
            let bytes = buffer.bytes()?;
            if bytes.get(..8) == Some(MAGIC)
                && bytes
                    .get(8..12)
                    .is_some_and(|version| version != FORMAT_VERSION.to_le_bytes())
            {
                return self.miss(&token, value.tier);
            }
            let extent = Extent::new(0, length)?;
            let decoded = match parse(&buffer, extent) {
                Ok(d) => d,
                Err(_) => {
                    self.corrupt_miss();
                    return self.miss(&token, value.tier);
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
                return self.miss(&token, value.tier);
            }
            // A concurrent retirement/removal must not resurrect a completed copy.
            if self
                .index
                .lookup(page)?
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
    /// File-mode slab path, resized at open when an older layout left a file
    /// of another size.
    slab_file: Option<std::path::PathBuf>,
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
}
impl StoreWriter {
    pub fn retention(&self) -> Rc<crate::retention::Retention> {
        self.index.retention()
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
            slab_file: None,
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
        }
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
            if let Some(path) = &self.slab_file {
                resize_legacy_slab(path, self.slabs.capacity_bytes())?;
            }
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
            let options = VolumeOptions {
                limits: self.index.volume_limits(slots),
                volume_id,
                format: true,
                charge: Rc::new(move |length| {
                    admission
                        .reserve_completion(None, ResourceClass::Ciphertext, length)
                        .map_err(|_| page_alloc::Error::Busy)
                }),
            };
            let (volume, report) =
                Volume::mount(&*self.reactor, self.slabs.clone(), options, scope).await?;
            let alignment = volume.alignment();
            self.index.install_volume(volume, &report);
            // Mount buffers return to the slab's idle pool still charged.
            self.slabs.reclaim_idle();
            Ok(alignment)
        })
    }
    /// Remember the file-mode slab path for legacy size repair at open.
    pub(crate) fn with_slab_file(mut self, path: std::path::PathBuf) -> Self {
        self.slab_file = Some(path);
        self
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
                .lookup(&id)?
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
                if let Err(error) = self.progress(8, scope).await {
                    self.remove_queued_ticket(&id, ticket);
                    self.slabs.fence_writes().await?;
                    return Err(error);
                }
            }
            self.slabs.fence_writes().await?;
            scope.check()?;
            let value = self.index.lookup(&id)?.ok_or(Error::Io)?;
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
        page: CiphertextCopy,
        dirty: flow_control::Charge<AdmissionPolicy>,
        reserve_staging: impl FnOnce(usize) -> Result<flow_control::Charge<AdmissionPolicy>>,
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
        // held page in the cache tier is rewritten to the storage tier.
        if self.index.already_on_disk(&id)? {
            return Ok(0);
        }
        let mut pending = self.pending.borrow_mut();
        if let Some(existing) = pending.get_mut(&id) {
            if existing.hold.is_none() && self.index.is_held(&id) {
                existing.hold = Some(self.index.protect_hold(&id.version)?);
            }
            return Ok(existing.ticket);
        }
        let full = pending.len() >= self.capacity.get();
        drop(pending);
        if full {
            let retention = self.retention();
            let victim = self
                .queue
                .borrow()
                .iter()
                .enumerate()
                .take(QUEUE_RECLAIM_CANDIDATES)
                .filter(|(_, page)| !self.protected_pending(page))
                .min_by_key(|(_, page)| retention.score(page))
                .map(|(position, page)| (position, page.clone(), retention.score(page)));
            let Some((position, victim, score)) = victim else {
                return Err(Error::Overloaded);
            };
            if !self.index.is_held(&id) && retention.score(&id) <= score {
                return Err(Error::Overloaded);
            }
            self.queue.borrow_mut().remove(position);
            self.pending.borrow_mut().remove(&victim);
            self.note_discard(1);
        }
        // Reserve the full staging capacity before queue acceptance. Thus
        // accepted dirty copies never wait for ciphertext holders to release memory.
        let disk_bytes = self.slabs.alignment()?.extent(0, logical)?.length();
        let capacity = staging_capacity(disk_bytes)?;
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
    pub(crate) fn discard_idle_copy(&self, page: &crate::memory::PageResult) -> usize {
        let id = page.plaintext.page();
        if self.protected_pending(id) {
            return 0;
        }
        let mut queue = self.queue.borrow_mut();
        let Some(position) = queued_position(&queue, id) else {
            return 0;
        };
        let mut pending = self.pending.borrow_mut();
        if pending.get(id).is_some_and(|dirty| {
            std::sync::Arc::ptr_eq(&dirty.page.ciphertext.inner, &page.ciphertext.inner)
                && std::sync::Arc::strong_count(&page.ciphertext.inner) == 2
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
        let mut queue = self.queue.borrow_mut();
        let mut pending = self.pending.borrow_mut();
        let candidates: Vec<_> = queue
            .iter()
            .take(QUEUE_RECLAIM_CANDIDATES)
            .filter(|id| {
                cache.is_none_or(|cache| cache == &id.version.object.cache)
                    && !retention.is_owned(id)
                    && !self.index.is_held(id)
                    && pending.get(*id).is_none_or(|dirty| dirty.hold.is_none())
            })
            .cloned()
            .collect();
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
        let mut queue = self.queue.borrow_mut();
        let mut candidates: Vec<_> = queue
            .iter()
            .take(QUEUE_RECLAIM_CANDIDATES)
            .filter(|id| cache.is_none_or(|cache| cache == &id.version.object.cache))
            .filter(|id| !self.index.is_held(id))
            .filter(|id| pending.get(*id).is_none_or(|dirty| dirty.hold.is_none()))
            .map(|id| (retention.score(id), id.clone()))
            .collect();
        candidates.sort_by_key(|(score, _)| *score);
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
    /// Drive at most `budget` writes. Keep polling this future through reactor completion.
    pub fn progress<'a>(&'a self, budget: usize, scope: &'a RequestScope) -> Operation<'a, usize> {
        Box::pin(async move {
            if self.busy.replace(true) {
                return Err(Error::Overloaded);
            }
            *self.active_scope.borrow_mut() = Some(scope.clone());
            let _busy = Busy(self);
            use futures::StreamExt;
            use futures::stream::FuturesUnordered;
            let mut writes = FuturesUnordered::new();
            for _ in 0..budget {
                if scope.check().is_err() {
                    self.discard_unsubmitted();
                    break;
                }
                let position = {
                    let queue = self.queue.borrow();
                    let retention = self.retention();
                    // Reverse the tie breaker to keep FIFO within equal value.
                    queue
                        .iter()
                        .enumerate()
                        .take(QUEUE_RECLAIM_CANDIDATES)
                        .max_by_key(|(position, id)| {
                            (retention.score(id), std::cmp::Reverse(*position))
                        })
                        .map(|(position, _)| position)
                };
                let id =
                    match position.and_then(|position| self.queue.borrow_mut().remove(position)) {
                        Some(id) => id,
                        None => break,
                    };
                let page = match self.copy_only(&id)? {
                    Some(p) => p,
                    None => {
                        self.pending.borrow_mut().remove(&id);
                        continue;
                    }
                };
                // Install cleanup BEFORE allocation, reclamation or submission.
                // Every attempted copy leaves the queue, including disposable failures.
                let mut cleanup = DirtyCleanup {
                    writer: self,
                    persisted: false,
                    page: id.clone(),
                    ticket: self
                        .pending
                        .borrow()
                        .get(&id)
                        .ok_or(Error::Unavailable)?
                        .ticket,
                };
                writes.push(async move {
                    let result = self.persist(&id, &page, scope).await;
                    cleanup.persisted = result.is_ok();
                    drop(cleanup);
                    result
                });
            }
            let mut completed = 0;
            let mut failure = None;
            while let Some(result) = writes.next().await {
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
                    }
                    if matches!(error, Error::Cancelled | Error::DeadlineExceeded) {
                        self.discard_unsubmitted();
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
        if self.index.already_on_disk(id)? {
            return Ok(());
        }
        let alignment = self.slabs.alignment()?;
        let disk_bytes = alignment.extent(0, logical_length(page)?)?.length();
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
        let mut buffer = self.slabs.allocate(disk_bytes, staging)?;
        buffer.retain(dirty);
        // A page of a live hold with unused capacity goes to the storage tier.
        let hold = self.index.hold_ticket(id);
        let tier = if hold.is_some() {
            Tier::Storage
        } else {
            Tier::Cache
        };
        let reservation = self.index.allocate(tier, disk_bytes as u64)?;
        let generation = self.index.next_generation()?;
        let encoded = encode_at(page, Generation(generation), alignment, 0, buffer)?;
        let volume = self.index.volume().ok_or(Error::Unavailable)?;
        let _ = volume
            .write(&*self.reactor, &reservation, encoded.buffer, scope)
            .await?;
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
                self.progress(8, scope).await?;
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
pub const FORMAT_VERSION: u32 = 1;
pub const MAX_ID_BYTES: usize = 4096;
pub const MAX_ETAG_BYTES: usize = 8192;
pub const MAX_HEADER_BYTES: usize = 16384;

/// Largest encoded page record: a full page of ciphertext, its 16-byte AEAD
/// tag, and the largest header.
pub const MAX_RECORD_BYTES: usize = crate::model::PAGE_BYTES as usize + 16 + MAX_HEADER_BYTES;

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
struct RecordLayout {
    header_bytes: usize,
    logical_bytes: usize,
}
fn layout(page: &CiphertextCopy, generation: Generation) -> Result<RecordLayout> {
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
        .filter(|&len| len <= MAX_HEADER_BYTES)
        .ok_or(Error::CorruptRecord)?;
    let logical_bytes = header_bytes
        .checked_add(page.ciphertext.bytes().len())
        .ok_or(Error::CorruptRecord)?;
    Ok(RecordLayout {
        header_bytes,
        logical_bytes,
    })
}
fn header_bytes(page: &CiphertextCopy, generation: Generation, layout: &RecordLayout) -> Vec<u8> {
    let envelope = page.ciphertext.envelope();
    let cache = envelope.page.version.object.cache.0.as_bytes();
    let etag = envelope.page.version.etag.as_bytes();
    let mut out = Vec::with_capacity(layout.header_bytes);
    out.extend_from_slice(MAGIC);
    out.extend_from_slice(&FORMAT_VERSION.to_le_bytes());
    out.extend_from_slice(&(layout.header_bytes as u32).to_le_bytes());
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
pub fn logical_length(page: &CiphertextCopy) -> Result<usize> {
    Ok(layout(page, Generation(1))?.logical_bytes)
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
    mut buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
) -> Result<EncodedRecord> {
    let layout = layout(page, generation)?;
    let logical_bytes = layout.logical_bytes;
    let extent = alignment.extent(offset, logical_bytes)?;
    alignment.check(extent, &buffer)?;
    let header = header_bytes(page, generation, &layout);
    let bytes = buffer.bytes_mut()?;
    bytes[..header.len()].copy_from_slice(&header);
    bytes[header.len()..logical_bytes].copy_from_slice(page.ciphertext.bytes());
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
pub fn parse_bytes(bytes: &[u8], extent: Extent) -> Result<DecodedRecord> {
    if bytes.len() != extent.length() {
        return Err(Error::CorruptRecord);
    }
    let mut r = Decoder(bytes);
    if r.take(8)? != MAGIC {
        return Err(Error::CorruptRecord);
    }
    let format_version = r.u32()?;
    if format_version != FORMAT_VERSION {
        return Err(Error::CorruptRecord);
    }
    let header_len = r.u32()? as usize;
    if !(160..=MAX_HEADER_BYTES).contains(&header_len) || header_len > bytes.len() {
        return Err(Error::CorruptRecord);
    }
    let digest = Sha256::digest(&bytes[..header_len - 32]);
    if digest[..] != bytes[header_len - 32..header_len] {
        return Err(Error::CorruptRecord);
    }
    r.0 = &bytes[16..header_len - 32];
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
    if logical_bytes > bytes.len() {
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

#[cfg(test)]
mod tests;

/// Give a file-mode slab of another size (left by an older layout) the
/// configured size, so the volume mount formats it fresh. A missing or empty
/// file is created by the slab itself.
fn resize_legacy_slab(path: &std::path::Path, capacity: u64) -> Result<()> {
    let Ok(metadata) = std::fs::metadata(path) else {
        return Ok(());
    };
    if !metadata.is_file() || metadata.len() == 0 || metadata.len() == capacity {
        return Ok(());
    }
    eprintln!(
        "racer: resizing slab {} from {} to {capacity} bytes for the extent volume; its pages are discarded",
        path.display(),
        metadata.len()
    );
    std::fs::OpenOptions::new()
        .write(true)
        .open(path)
        .and_then(|file| file.set_len(capacity))
        .map_err(|error| {
            eprintln!("racer: cannot resize slab {}: {error}", path.display());
            Error::Io
        })
}
