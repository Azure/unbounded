//! Bounded dirty copies persist asynchronously, with publication after full I/O.
use super::{
    eviction::SegmentClock,
    format::RecordCodec,
    index::{Index, IndexedPage, RecordLocation},
    segment::Segments,
    slab::Slabs,
};
use crate::{
    error::{Error, Operation, Result},
    memory::page::CiphertextCopy,
    model::{
        envelope::KeyId,
        identity::{CacheId, ObjectVersion, PageId},
        limits::ResourceClass,
        metadata::VersionMetadata,
    },
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
    },
};
use std::{
    cell::{Cell, RefCell},
    collections::{HashMap, HashSet, VecDeque},
    rc::Rc,
};
struct Dirty {
    ticket: u64,
    page: CiphertextCopy,
    _reservation: Rc<Reservation>,
    staging: Option<Reservation>,
}
pub struct StoreWriter {
    index: Rc<Index>,
    segments: Rc<Segments>,
    slabs: Rc<Slabs>,
    clock: RefCell<Option<Rc<SegmentClock>>>,
    pending: RefCell<HashMap<PageId, Dirty>>,
    queue: RefCell<VecDeque<PageId>>,
    next: Cell<u64>,
    capacity: Cell<usize>,
    busy: Cell<bool>,
    active_scope: RefCell<Option<RequestScope>>,
    retired: RefCell<HashSet<(CacheId, KeyId)>>,
    removed: RefCell<HashSet<CacheId>>,
    discarded: Cell<u64>,
    closed: Cell<bool>,
}
pub struct DirtyTicket {
    id: u64,
}
impl DirtyTicket {
    pub fn id(&self) -> u64 {
        self.id
    }
}
impl StoreWriter {
    pub fn new(index: Rc<Index>, segments: Rc<Segments>, slabs: Rc<Slabs>) -> Self {
        Self {
            index,
            segments,
            slabs,
            clock: RefCell::new(None),
            pending: RefCell::new(HashMap::new()),
            queue: RefCell::new(VecDeque::new()),
            next: Cell::new(1),
            capacity: Cell::new(64),
            busy: Cell::new(false),
            active_scope: RefCell::new(None),
            retired: RefCell::new(HashSet::new()),
            removed: RefCell::new(HashSet::new()),
            discarded: Cell::new(0),
            closed: Cell::new(false),
        }
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
        self.index.set_page_capacity(page_entries)?;
        self.slabs.set_admission(admission);
        *self.clock.borrow_mut() = Some(clock);
        self.capacity.set(queue_entries);
        Ok(())
    }
    pub fn open(&self) -> Operation<'_, super::direct::DirectAlignment> {
        Box::pin(async move {
            let alignment = self.slabs.open().await?;
            if self.segments.snapshot()?.is_empty() {
                self.segments.configure(
                    self.slabs.slab_bytes(),
                    usize::try_from(self.slabs.slab_bytes() / self.slabs.segment_bytes())
                        .map_err(|_| Error::InvalidConfiguration)?,
                    alignment,
                )?;
            }
            Ok(alignment)
        })
    }
    pub fn slabs(&self) -> &Rc<Slabs> {
        &self.slabs
    }
    #[cfg(test)]
    pub(super) fn segments_for_test(&self) -> &Rc<Segments> {
        &self.segments
    }
    pub fn index(&self) -> &Rc<Index> {
        &self.index
    }
    pub fn lease(&self, location: &RecordLocation) -> Result<super::segment::SegmentLease> {
        self.segments.validate_location(location)?;
        self.segments.lease(location.segment, location.generation)
    }
    fn allowed(&self, page: &CiphertextCopy) -> bool {
        let e = page.ciphertext.envelope();
        !self
            .retired
            .borrow()
            .contains(&(e.page.version.object.cache.clone(), e.key_id))
            && !self.removed.borrow().contains(&e.page.version.object.cache)
    }
    pub fn enqueue(&self, page: CiphertextCopy, dirty: Reservation) -> Result<DirtyTicket> {
        if self.closed.get() {
            return Err(Error::Unavailable);
        }
        self.index
            .preflight_capacity(&page.ciphertext.envelope().page)?;
        let logical = RecordCodec.logical_length(&page)?;
        if !matches!(dirty.class(), ResourceClass::DirtyCiphertext)
            || !self.slabs.owns_reservation(&dirty)
            || dirty.cache() != Some(&page.metadata.version.object.cache)
            || dirty.amount() < page.ciphertext.bytes().len()
            || logical
                > crate::model::range::PAGE_BYTES as usize + super::format::MAX_HEADER_BYTES + 16
        {
            return Err(Error::InvalidConfiguration);
        }
        if !self.allowed(&page) {
            return Err(Error::MissingKey);
        }
        let id = page.ciphertext.envelope().page.clone();
        if let Some(metadata) = self.index.version(&id.version)? {
            if metadata != page.metadata.immutable() {
                return Err(Error::CorruptRecord);
            }
        }
        if let Some(metadata) = self.metadata(&id.version)? {
            if metadata != page.metadata.immutable() {
                return Err(Error::CorruptRecord);
            }
        }
        let mut pending = self.pending.borrow_mut();
        if let Some(existing) = pending.get(&id) {
            return Ok(DirtyTicket {
                id: existing.ticket,
            });
        }
        if pending.len() >= self.capacity.get() {
            return Err(Error::Overloaded);
        }
        // Reserve the exact padded staging bytes before queue acceptance. Thus
        // accepted dirty copies never wait for ciphertext holders to release memory.
        let disk_bytes = self.slabs.alignment()?.extent(0, logical)?.length();
        let staging = self
            .slabs
            .reserve_staging(disk_bytes, &id.version.object.cache)?;
        let ticket = self.next.get();
        self.next
            .set(ticket.checked_add(1).ok_or(Error::Unavailable)?);
        pending.insert(
            id.clone(),
            Dirty {
                ticket,
                page,
                _reservation: Rc::new(dirty),
                staging: Some(staging),
            },
        );
        self.queue.borrow_mut().push_back(id);
        Ok(DirtyTicket { id: ticket })
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
    /// Drive at most `budget` writes. Keep polling this future through reactor completion.
    pub fn progress<'a>(&'a self, budget: usize, scope: &'a RequestScope) -> Operation<'a, usize> {
        Box::pin(async move {
            if self.busy.replace(true) {
                return Err(Error::Overloaded);
            }
            *self.active_scope.borrow_mut() = Some(scope.clone());
            let _busy = Busy(self);
            let mut completed = 0;
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
                let _cleanup = DirtyCleanup {
                    writer: self,
                    page: id.clone(),
                    ticket: self
                        .pending
                        .borrow()
                        .get(&id)
                        .ok_or(Error::Unavailable)?
                        .ticket,
                };
                let result = self.persist(&id, &page, scope).await;
                completed += 1;
                if let Some(clock) = self.clock.borrow().as_ref() {
                    let _ = clock.reclaim_now();
                }
                // Quota/space pressure and cache I/O failures never fail the node.
                if let Err(error) = result {
                    self.note_discard(1);
                    if !matches!(
                        error,
                        Error::Overloaded
                            | Error::Io
                            | Error::Unavailable
                            | Error::MissingKey
                            | Error::Cancelled
                            | Error::DeadlineExceeded
                    ) {
                        return Err(error);
                    }
                    if matches!(error, Error::Cancelled | Error::DeadlineExceeded) {
                        self.discard_unsubmitted();
                        break;
                    }
                }
            }
            Ok(completed)
        })
    }
    async fn persist(
        &self,
        id: &PageId,
        page: &CiphertextCopy,
        scope: &RequestScope,
    ) -> Result<()> {
        // Enqueue does not reserve slots: earlier queued pages may fill the index.
        // Production has one writer per index, and progress holds Busy across this
        // await through publication, so no other writer can consume a free slot.
        self.index.preflight_capacity(id)?;
        let alignment = self.slabs.alignment()?;
        let disk_bytes = alignment
            .extent(0, RecordCodec.logical_length(page)?)?
            .length();
        let (staging, dirty) = {
            let mut pending = self.pending.borrow_mut();
            let entry = pending.get_mut(id).ok_or(Error::Unavailable)?;
            (
                entry.staging.take().ok_or(Error::InvalidConfiguration)?,
                entry._reservation.clone(),
            )
        };
        let mut buffer = alignment.allocate(disk_bytes, staging)?;
        buffer.retain_charge(dirty);
        let append = match self.segments.append(disk_bytes) {
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
            segment: append.segment.id(),
            generation: append.segment.generation(),
            location: append.location,
        };
        let _publication_lease = self.segments.lease(location.segment, location.generation)?;
        let encoded = RecordCodec.encode_at(
            page,
            location.generation,
            alignment,
            location.location.extent.offset(),
            buffer,
        )?;
        self.slabs
            .write(append.location, encoded.buffer, append.segment, scope)
            .await?;
        if self.allowed(page) && self.segments.validate_location(&location).is_ok() {
            self.index.publish(
                id.clone(),
                IndexedPage {
                    location,
                    metadata: page.metadata.immutable(),
                    key_id: page.ciphertext.envelope().key_id,
                },
            )?;
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
                self.progress(1, scope).await?;
            }
            self.slabs.fence_writes().await
        })
    }
    /// Stops new/late publication immediately. Call drain/fence before releasing keys.
    pub fn retire_key(&self, cache: &CacheId, key: KeyId) -> Result<usize> {
        // Bounded tombstones fail closed. Epoch compaction is an explicit startup operation.
        if self.retired.borrow().len() >= 65536
            && !self.retired.borrow().contains(&(cache.clone(), key))
        {
            return Err(Error::Overloaded);
        }
        self.retired.borrow_mut().insert((cache.clone(), key));
        let mut pending = self.pending.borrow_mut();
        pending.retain(|_, d| {
            let e = d.page.ciphertext.envelope();
            &e.page.version.object.cache != cache || e.key_id != key
        });
        self.queue.borrow_mut().retain(|p| pending.contains_key(p));
        Ok(self.index.retire_key(cache, key))
    }
    pub fn remove_cache(&self, cache: &CacheId) -> Result<()> {
        if self.removed.borrow().len() >= 65536 && !self.removed.borrow().contains(cache) {
            return Err(Error::Overloaded);
        }
        self.removed.borrow_mut().insert(cache.clone());
        self.pending
            .borrow_mut()
            .retain(|p, _| &p.version.object.cache != cache);
        self.queue
            .borrow_mut()
            .retain(|p| &p.version.object.cache != cache);
        self.index.remove_cache(cache);
        Ok(())
    }
    pub fn is_idle(&self) -> bool {
        !self.busy.get() && self.pending_count() == 0 && self.writes_in_flight() == 0
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
        }
    }
}
#[cfg(test)]
mod tests { /* Persistence and quota ownership exercised by store integration tests. */
}
