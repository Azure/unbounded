//! Bounded dirty copies persist asynchronously, with publication after full I/O.
use super::{
    catalog::{Index, IndexedPage, RecordLocation, SegmentClock},
    format,
};
use crate::runtime::collections::HashMap;
use crate::runtime::reactor::Reactor;
use crate::{
    error::{Error, Operation, Result},
    memory::page::CiphertextCopy,
    model::{CacheId, ObjectVersion, PageId, ResourceClass, VersionMetadata},
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
    },
};
use page_alloc::{Alignment, SegmentLease, Segments, Slab};
use std::{
    cell::{Cell, RefCell},
    collections::VecDeque,
    rc::Rc,
};
struct Dirty {
    _metric: crate::telemetry::metrics::GaugeLease,
    ticket: u64,
    page: CiphertextCopy,
    _reservation: Rc<Reservation>,
    staging: Option<Reservation>,
}
pub struct StoreWriter {
    metrics: crate::telemetry::metrics::Metrics,
    index: Rc<Index>,
    segments: Rc<Segments>,
    slabs: Rc<Slab<Reservation>>,
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
    clock: RefCell<Option<Rc<SegmentClock>>>,
    pending: RefCell<HashMap<PageId, Dirty>>,
    queue: RefCell<VecDeque<PageId>>,
    next: Cell<u64>,
    capacity: Cell<usize>,
    busy: Cell<bool>,
    active_scope: RefCell<Option<RequestScope>>,
    availability: Rc<crate::control::state::Availability>,
    discarded: Cell<u64>,
    closed: Cell<bool>,
}
impl StoreWriter {
    pub fn new(
        index: Rc<Index>,
        segments: Rc<Segments>,
        slabs: Rc<Slab<Reservation>>,
        admission: Rc<Admission>,
        reactor: Rc<Reactor>,
        availability: Rc<crate::control::state::Availability>,
    ) -> Self {
        Self {
            metrics: crate::telemetry::metrics::Metrics::default(),
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
    pub fn with_metrics(mut self, metrics: crate::telemetry::metrics::Metrics) -> Self {
        self.metrics = metrics;
        self
    }
    pub fn configure(
        &self,
        admission: Rc<Admission>,
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
    pub fn slabs(&self) -> &Rc<Slab<Reservation>> {
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
    pub fn enqueue(&self, page: CiphertextCopy, dirty: Reservation) -> Result<u64> {
        let cache = page.metadata.version.object.cache.clone();
        self.enqueue_reclaiming(page, dirty, |length| self.reserve_staging(length, &cache))
    }
    /// Accepted fills may complete after request admission closes, within quota.
    fn reserve_staging(&self, length: usize, cache: &CacheId) -> Result<Reservation> {
        let reserve = || {
            self.admission
                .reserve_completion(Some(cache), ResourceClass::Ciphertext, length)
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
        dirty: Reservation,
        reserve_staging: impl FnOnce(usize) -> Result<Reservation>,
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
        let logical = format::logical_length(&page)?;
        if !matches!(dirty.class(), ResourceClass::DirtyCiphertext)
            || !self.admission.owns(&dirty)
            || dirty.cache() != Some(&page.metadata.version.object.cache)
            || dirty.amount() < page.ciphertext.bytes().len()
            || logical > crate::model::PAGE_BYTES as usize + super::format::MAX_HEADER_BYTES + 16
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
        if !self.admission.owns(&staging) || staging.cache() != Some(&id.version.object.cache) {
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
                    .lease(crate::telemetry::metrics::Gauge::PendingDiskWrites)?,
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
            .record(crate::telemetry::metrics::Event::DirtyDiscard, count as u64);
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
            return dirty.staging.as_ref().map_or(0, Reservation::amount);
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
                released =
                    released.saturating_add(dirty.staging.as_ref().map_or(0, Reservation::amount));
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
            use futures::{StreamExt, stream::FuturesUnordered};
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
        let disk_bytes = alignment.extent(0, format::logical_length(page)?)?.length();
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
        let encoded = format::encode_at(
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
                .record(crate::telemetry::metrics::Event::DiskPublication, 1)?;
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
    pub(super) fn reclaim_idle_buffer(&self) -> usize {
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
#[cfg(test)]
mod tests { /* Persistence and quota ownership exercised by store integration tests. */
}
