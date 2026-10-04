//! Worker-local encrypted slab storage. No HTTP, plaintext, or origin credentials.
//! Versioned little-endian records protect framing with header SHA-256. The stored
//! CRC is checked before AEAD at the fill boundary; padding is never returned.
use self::catalog::Index;
use self::catalog::IndexedPage;
use self::catalog::RecordLocation;
use self::catalog::SegmentClock;
use crate::admission::AdmissionPolicy;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::BufferPool;
use crate::memory::page::CiphertextCopy;
use crate::model::CacheId;
use crate::model::CacheKey;
use crate::model::KeyId;
use crate::model::Nonce;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::ResourceClass;
use crate::model::StrongEtag;
use crate::model::VersionMetadata;
use crate::runtime::HashMap;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use page_alloc::AlignedBuffer;
use page_alloc::Alignment;
use page_alloc::Extent;
use page_alloc::Generation;
use page_alloc::SegmentLease;
use page_alloc::Segments;
use page_alloc::Slab;
use sha2::Digest;
use sha2::Sha256;
use std::cell::Cell;
use std::cell::RefCell;
use std::collections::VecDeque;
use std::rc::Rc;
pub mod catalog;
pub mod checkpoint;

pub struct Store {
    pub reader: Rc<StoreReader>,
    pub writer: Rc<StoreWriter>,
    pub checkpoint: Rc<checkpoint::Checkpointer>,
    pub recovery: checkpoint::Recovery,
    pub eviction: Rc<catalog::SegmentClock>,
}

impl Store {
    /// Side-effect-free resource wiring, before startup and request admission.
    pub fn configure(
        &self,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        queue_entries: usize,
        page_entries: usize,
    ) -> Result<()> {
        self.writer.configure(
            admission,
            self.eviction.clone(),
            queue_entries,
            page_entries,
        )
    }

    /// Open slabs and configure the actual live shard and checkpoint geometry.
    pub fn open(&self) -> Operation<'_, Alignment> {
        Box::pin(async move {
            let alignment = self.writer.open().await?;
            let slabs = self.writer.slabs();
            let geometry = checkpoint::CheckpointGeometry::new(
                slabs.capacity_bytes(),
                slabs.segment_bytes(),
                slabs.capacity_bytes() / slabs.segment_bytes(),
                alignment,
            )?;
            self.checkpoint.configure_geometry(geometry)?;
            self.recovery.configure_geometry(geometry)?;
            Ok(alignment)
        })
    }
}

/// Lease mappings before awaiting I/O and return validated ciphertext framing.
/// AEAD validation remains owned by fill.
pub struct StoreReader {
    metrics: crate::telemetry::Metrics,
    clock: Rc<catalog::SegmentClock>,
    index: Rc<Index>,
    segments: Rc<Segments>,
    slabs: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    buffers: BufferPool,
}
#[derive(Clone)]
/// Permits conditional invalidation without removing a replacement mapping.
pub struct ReadToken {
    page: PageId,
    location: RecordLocation,
}
impl StoreReader {
    pub fn metadata(
        &self,
        version: &crate::model::ObjectVersion,
    ) -> Result<Option<crate::model::VersionMetadata>> {
        self.index.version(version)
    }
    pub fn new(
        clock: Rc<catalog::SegmentClock>,
        index: Rc<Index>,
        segments: Rc<Segments>,
        slabs: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        buffers: BufferPool,
    ) -> Self {
        Self {
            metrics: crate::telemetry::Metrics::default(),
            clock,
            index,
            segments,
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
        let _ = self.metrics.record(crate::telemetry::Event::CorruptMiss, 1);
    }
    pub fn invalidate(&self, token: &ReadToken) -> Result<()> {
        self.index.remove_if_matches(&token.page, &token.location);
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
            let entry = match self.metrics.lookup(
                crate::telemetry::LookupTier::DiskIndex,
                self.index.lookup(page),
            )? {
                Some(e) => e,
                None => return Ok(None),
            };
            let token = ReadToken {
                page: page.clone(),
                location: entry.location.clone(),
            };
            // Both checks happen without yielding, so eviction cannot interleave.
            if self
                .segments
                .validate(
                    entry.location.segment,
                    entry.location.generation,
                    &entry.location.extent,
                )
                .is_err()
            {
                self.invalidate(&token)?;
                return Ok(None);
            }
            let lease = match self
                .segments
                .lease(entry.location.segment, entry.location.generation)
            {
                Ok(l) => l,
                Err(_) => {
                    self.invalidate(&token)?;
                    return Ok(None);
                }
            };
            let length = entry.location.extent.length();
            let decoded_length = entry.metadata.page_length(page)? as usize + 16;
            let amount = length
                .checked_add(decoded_length)
                .ok_or(Error::Overloaded)?;
            let mut reserved = reserve(amount);
            if matches!(reserved, Err(Error::Overloaded)) {
                self.slabs.reclaim_idle();
                reserved = reserve(amount);
            }
            let mut decoded_reservation = reserved?;
            decoded_reservation.validate(ResourceClass::Ciphertext, amount)?;
            if !self.admission.owns(&decoded_reservation)
                || decoded_reservation.key() != Some(&page.version.object.cache)
            {
                return Err(Error::InvalidConfiguration);
            }
            let staging = decoded_reservation.split(length)?;
            let buffer = self.slabs.allocate(length, staging)?;
            let buffer = match self
                .slabs
                .read(&self.reactor, entry.location.extent, buffer, lease, scope)
                .await
            {
                Ok(b) => b,
                Err(error @ (Error::Io | Error::CorruptRecord)) => {
                    if error == Error::CorruptRecord {
                        self.corrupt_miss();
                    }
                    self.invalidate(&token)?;
                    return Ok(None);
                }
                Err(e) => return Err(e),
            };
            let decoded = match parse(&buffer, entry.location.extent) {
                Ok(d) => d,
                Err(_) => {
                    self.corrupt_miss();
                    self.invalidate(&token)?;
                    return Ok(None);
                }
            };
            if decoded.header.envelope.page != *page
                || decoded.header.generation != entry.location.generation
                || decoded.header.metadata != entry.metadata
                || decoded.header.envelope.key_id != entry.key_id
            {
                self.corrupt_miss();
                self.invalidate(&token)?;
                return Ok(None);
            }
            // A concurrent retirement/removal must not resurrect a completed copy.
            if self.index.lookup(page)?.is_none_or(|current| {
                current.location != entry.location || current.key_id != entry.key_id
            }) {
                return Ok(None);
            }
            let ciphertext = self.buffers.ciphertext(
                decoded_reservation,
                decoded.header.envelope,
                buffer.bytes()?[decoded.ciphertext].to_vec(),
            )?;
            ciphertext.expected_checksum(decoded.checksum)?;
            self.clock.mark_read(entry.location.segment)?;
            Ok(Some((
                CiphertextCopy {
                    metadata: entry.metadata.for_pin(),
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
    _metric: ::telemetry::Lease,
    ticket: u64,
    page: CiphertextCopy,
    _reservation: Rc<flow_control::Charge<AdmissionPolicy>>,
    staging: Option<flow_control::Charge<AdmissionPolicy>>,
}
/// Bounded dirty copies persist asynchronously, with publication after full I/O.
pub struct StoreWriter {
    metrics: crate::telemetry::Metrics,
    index: Rc<Index>,
    segments: Rc<Segments>,
    slabs: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    clock: RefCell<Option<Rc<SegmentClock>>>,
    pending: RefCell<HashMap<PageId, Dirty>>,
    queue: RefCell<VecDeque<PageId>>,
    next: Cell<u64>,
    capacity: Cell<usize>,
    busy: Cell<bool>,
    active_scope: RefCell<Option<RequestScope>>,
    availability: Rc<crate::control::Availability>,
    discarded: Cell<u64>,
    closed: Cell<bool>,
}
impl StoreWriter {
    pub fn new(
        index: Rc<Index>,
        segments: Rc<Segments>,
        slabs: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        availability: Rc<crate::control::Availability>,
    ) -> Self {
        Self {
            metrics: crate::telemetry::Metrics::default(),
            index,
            segments,
            slabs,
            admission,
            reactor,
            clock: RefCell::new(None),
            pending: RefCell::new(HashMap::default()),
            queue: RefCell::new(VecDeque::new()),
            next: Cell::new(1),
            capacity: Cell::new(64),
            busy: Cell::new(false),
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
        clock: Rc<SegmentClock>,
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
        *self.clock.borrow_mut() = Some(clock);
        self.capacity.set(queue_entries);
        Ok(())
    }
    pub fn open(&self) -> Operation<'_, Alignment> {
        Box::pin(async move {
            let alignment = self.slabs.open().await?;
            if self.segments.snapshot()?.is_empty() {
                self.segments.configure(
                    self.slabs.capacity_bytes(),
                    usize::try_from(self.slabs.capacity_bytes() / self.slabs.segment_bytes())
                        .map_err(|_| Error::InvalidConfiguration)?,
                    alignment,
                )?;
            }
            Ok(alignment)
        })
    }
    pub fn slabs(&self) -> &Rc<Slab<flow_control::Charge<AdmissionPolicy>>> {
        &self.slabs
    }
    pub fn index(&self) -> &Rc<Index> {
        &self.index
    }
    pub fn lease(&self, location: &RecordLocation) -> Result<SegmentLease> {
        self.segments
            .validate(location.segment, location.generation, &location.extent)?;
        Ok(self.segments.lease(location.segment, location.generation)?)
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
        // Configured writers can turn over the index in serialized progress.
        // Queue/staging bounds still apply; enqueue itself never evicts mappings.
        if self.clock.borrow().is_none() {
            self.index
                .preflight_capacity(&page.ciphertext.envelope().page)?;
        }
        let logical = logical_length(&page)?;
        if !matches!(dirty.class(), ResourceClass::DirtyCiphertext)
            || !self.admission.owns(&dirty)
            || dirty.key() != Some(&page.metadata.version.object.cache)
            || dirty.amount() < page.ciphertext.bytes().len()
            || logical > crate::model::PAGE_BYTES as usize + MAX_HEADER_BYTES + 16
        {
            return Err(Error::InvalidConfiguration);
        }
        if !self.allowed(&page) {
            return Err(Error::MissingKey);
        }
        let id = page.ciphertext.envelope().page.clone();
        if let Some(metadata) = self.index.version(&id.version)? {
            if !metadata.compatible(&page.metadata.immutable()) {
                return Err(Error::CorruptRecord);
            }
        }
        if let Some(metadata) = self.metadata(&id.version)? {
            if !metadata.compatible(&page.metadata.immutable()) {
                return Err(Error::CorruptRecord);
            }
        }
        let pending = self.pending.borrow();
        if let Some(existing) = pending.get(&id) {
            return Ok(existing.ticket);
        }
        if pending.len() >= self.capacity.get() {
            return Err(Error::Overloaded);
        }
        drop(pending);
        // Reserve the exact padded staging bytes before queue acceptance. Thus
        // accepted dirty copies never wait for ciphertext holders to release memory.
        let disk_bytes = self.slabs.alignment()?.extent(0, logical)?.length();
        let staging = reserve_staging(disk_bytes)?;
        staging.validate(ResourceClass::Ciphertext, disk_bytes)?;
        if !self.admission.owns(&staging) || staging.key() != Some(&id.version.object.cache) {
            return Err(Error::InvalidConfiguration);
        }
        let ticket = self.next.get();
        self.next
            .set(ticket.checked_add(1).ok_or(Error::Unavailable)?);
        self.pending.borrow_mut().insert(
            id.clone(),
            Dirty {
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
        let _ = self
            .metrics
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
            removed += usize::from(pending.remove(&page).is_some());
        }
        self.note_discard(removed);
        removed
    }
    /// Unpin exactly one otherwise-idle memory bundle. The two ciphertext owners
    /// must be that bundle and this queued write, not a submitted operation/reader.
    pub(crate) fn discard_idle_copy(&self, page: &crate::memory::page::PageResult) -> usize {
        let id = page.plaintext.page();
        let mut queue = self.queue.borrow_mut();
        let Some(position) = queue.iter().position(|queued| queued == id) else {
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
            return dirty
                .staging
                .as_ref()
                .map_or(0, flow_control::Charge::amount);
        }
        0
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
        self.queue.borrow_mut().retain(|id| {
            if released >= bytes || cache.is_some_and(|cache| cache != &id.version.object.cache) {
                return true;
            }
            if let Some(dirty) = pending.remove(id) {
                released = released.saturating_add(
                    dirty
                        .staging
                        .as_ref()
                        .map_or(0, flow_control::Charge::amount),
                );
                if std::sync::Arc::strong_count(&dirty.page.ciphertext.inner) == 1 {
                    released =
                        released.saturating_add(dirty.page.ciphertext.inner.reservation.amount());
                }
                removed += 1;
            }
            false
        });
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
                let id = match self.queue.borrow_mut().pop_front() {
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
                if let Some(clock) = self.clock.borrow().as_ref() {
                    let _ = clock.reclaim_now();
                }
                // Quota/space pressure and cache I/O failures never fail the node.
                if let Err(error) = result {
                    if !matches!(
                        error,
                        Error::Overloaded
                            | Error::Io
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
        // Capacity is owned across the await, including invalidation/replacement.
        let index_ticket = self.index.reserve_page(self.clock.borrow().is_some())?;
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
        let (segment, extent) = match self.segments.append(disk_bytes).map_err(Error::from) {
            Ok(append) => append,
            Err(Error::Overloaded) => {
                if let Some(clock) = self.clock.borrow().clone() {
                    clock.reclaim_now()?;
                }
                self.segments.append(disk_bytes)?
            }
            Err(error) => return Err(error),
        };
        let location = RecordLocation {
            segment: segment.id(),
            generation: segment.generation(),
            extent,
        };
        let _publication_lease = self.segments.lease(location.segment, location.generation)?;
        let encoded = encode_at(
            page,
            location.generation,
            alignment,
            location.extent.offset(),
            buffer,
        )?;
        self.slabs
            .write(&self.reactor, extent, encoded.buffer, segment, scope)
            .await?;
        if self.allowed(page)
            && self
                .pending
                .borrow()
                .get(id)
                .is_some_and(|d| d.ticket == ticket)
            && self
                .segments
                .validate(location.segment, location.generation, &location.extent)
                .is_ok()
        {
            index_ticket.publish(
                id.clone(),
                IndexedPage {
                    location,
                    metadata: page.metadata.immutable(),
                    key_id: page.ciphertext.envelope().key_id,
                },
            )?;
            self.metrics
                .record(crate::telemetry::Event::DiskPublication, 1);
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
                if scope.check().is_err() {
                    if let Err(error) = self.cancel_pending_writes() {
                        return std::task::Poll::Ready(Err(error));
                    }
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
                self.progress(8, scope).await?;
            }
            self.slabs.fence_writes().await?;
            self.slabs.reclaim_idle();
            Ok(())
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
    #[cfg(test)]
    fn reclaim_idle_buffer(&self) -> usize {
        self.slabs.reclaim_idle()
    }
}
struct Busy<'a>(&'a StoreWriter);
impl Drop for Busy<'_> {
    fn drop(&mut self) {
        self.0.busy.set(false);
        self.0.active_scope.borrow_mut().take();
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
// Version 4 is the sole format: mandatory CRC-64/XZ plus optional content type.
pub const FORMAT_VERSION: u32 = 4;
pub const MAX_ID_BYTES: usize = 4096;
pub const MAX_ETAG_BYTES: usize = 8192;
pub const MAX_HEADER_BYTES: usize = 16384;
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
        .checked_add(8)
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
    pub(super) fn take(&mut self, len: usize) -> Result<&'a [u8]> {
        let (value, rest) = self.0.split_at_checked(len).ok_or(Error::CorruptRecord)?;
        self.0 = rest;
        Ok(value)
    }
    pub(super) fn array<const N: usize>(&mut self) -> Result<[u8; N]> {
        self.take(N)?.try_into().map_err(|_| Error::CorruptRecord)
    }
    pub(super) fn u32(&mut self) -> Result<u32> {
        Ok(u32::from_le_bytes(self.array()?))
    }
    pub(super) fn u64(&mut self) -> Result<u64> {
        Ok(u64::from_le_bytes(self.array()?))
    }
}

#[cfg(test)]
mod tests;
