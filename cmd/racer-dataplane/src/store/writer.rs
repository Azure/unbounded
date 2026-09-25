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
    retired: RefCell<HashSet<(CacheId, KeyId)>>,
    removed: RefCell<HashSet<CacheId>>,
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
            retired: RefCell::new(HashSet::new()),
            removed: RefCell::new(HashSet::new()),
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
        let ticket = self.next.get();
        self.next
            .set(ticket.checked_add(1).ok_or(Error::Unavailable)?);
        pending.insert(
            id.clone(),
            Dirty {
                ticket,
                page,
                _reservation: Rc::new(dirty),
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
    /// Drive at most `budget` writes. Keep polling this future through reactor completion.
    pub fn progress<'a>(&'a self, budget: usize, scope: &'a RequestScope) -> Operation<'a, usize> {
        Box::pin(async move {
            if self.busy.replace(true) {
                return Err(Error::Overloaded);
            }
            let _busy = Busy(&self.busy);
            let mut completed = 0;
            for _ in 0..budget {
                scope.check()?;
                let id = match self.queue.borrow().front().cloned() {
                    Some(id) => id,
                    None => break,
                };
                let page = match self.copy_only(&id)? {
                    Some(p) => p,
                    None => {
                        self.queue.borrow_mut().pop_front();
                        self.pending.borrow_mut().remove(&id);
                        continue;
                    }
                };
                let alignment = self.slabs.alignment()?;
                let disk_bytes = alignment
                    .extent(0, RecordCodec.logical_length(&page)?)?
                    .length();
                // Reserve staging before consuming append space. Original copy remains independently charged.
                let mut buffer = self
                    .slabs
                    .allocate(disk_bytes, Some(&id.version.object.cache))?;
                buffer.retain_charge(
                    self.pending
                        .borrow()
                        .get(&id)
                        .ok_or(Error::Unavailable)?
                        ._reservation
                        .clone(),
                );
                let append = match self.segments.append(disk_bytes) {
                    Ok(a) => a,
                    Err(Error::Overloaded) => {
                        let clock = self.clock.borrow().clone();
                        if let Some(clock) = clock {
                            clock.reclaim_now()?;
                        }
                        self.segments.append(disk_bytes)?
                    }
                    Err(e) => return Err(e),
                };
                let location = RecordLocation {
                    segment: append.segment.id(),
                    generation: append.segment.generation(),
                    location: append.location,
                };
                // Keep the publication window fenced even after Slabs returns its I/O lease.
                let _publication_lease =
                    self.segments.lease(location.segment, location.generation)?;
                let encoded = RecordCodec.encode_at(
                    &page,
                    location.generation,
                    alignment,
                    location.location.extent.offset(),
                    buffer,
                )?;
                self.queue.borrow_mut().pop_front();
                // On abandoned/error futures remove this dirty entry. The reactor still owns submitted staging and lease.
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
                let result = self
                    .slabs
                    .write(append.location, encoded.buffer, append.segment, scope)
                    .await;
                if result.is_ok()
                    && self.allowed(&page)
                    && self.segments.validate_location(&location).is_ok()
                {
                    self.index.publish(
                        id.clone(),
                        IndexedPage {
                            location,
                            metadata: page.metadata.immutable(),
                            key_id: page.ciphertext.envelope().key_id,
                        },
                    )?;
                }
                completed += 1;
                if let Some(clock) = self.clock.borrow().as_ref() {
                    let _ = clock.reclaim_now();
                }
                // Disk failures discard this copy; callers already have verified plaintext.
                if let Err(error) = result {
                    return Err(error);
                }
            }
            Ok(completed)
        })
    }
    pub fn drain<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            while self.pending_count() != 0 {
                self.progress(1, scope).await?;
            }
            Ok(())
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
        !self.busy.get() && self.pending_count() == 0
    }
}
struct Busy<'a>(&'a Cell<bool>);
impl Drop for Busy<'_> {
    fn drop(&mut self) {
        self.0.set(false);
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
