//! Catalog page table on [`page_alloc::disk_index::DiskIndex`].
//!
//! Pages are interned once (`PageId -> PageSlot`), caches once
//! (`CacheId -> CacheIdx`). Each stored slot keeps only its page key ID; the
//! immutable version descriptor lives once per version in a refcounted table
//! instead of being copied into every page.
use super::catalog::IndexedPage;
use super::catalog::RecordLocation;
use crate::error::Error;
use crate::error::Result;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::VersionMetadata;
use crate::runtime::HashMap;
use page_alloc::SegmentId;
use page_alloc::Segments;
use page_alloc::disk_index::AgeCursor;
use page_alloc::disk_index::DiskIndex;
use page_alloc::disk_index::EntryView;
use page_alloc::disk_index::PageSlot;
use page_alloc::intern::CacheTable;
use page_alloc::intern::SlotInterner;
use page_alloc::pending::PendingWrite;
use racer_control_wire::CacheId;
use racer_control_wire::KeyId;
use std::cell::RefCell;
use std::rc::Rc;

/// Production interning uses randomized hashing, as the page map did.
#[cfg(not(test))]
type Interner = SlotInterner<PageId, std::hash::RandomState>;
/// Simulation keeps its deterministic hash state.
#[cfg(test)]
type Interner = SlotInterner<PageId, crate::runtime::HashState>;

/// Independent publish-order walks; see [`Pages::walk`].
#[derive(Clone, Copy, Debug)]
pub(super) enum Walk {
    /// Unavailable-key sweep.
    KeySweep,
    /// Ownership refresh of resident pages.
    Ownership,
    /// Checkpoint traversal.
    Checkpoint,
    /// Startup-only rebuilds and whole-table scans.
    Rebuild,
}
impl Walk {
    fn cursor(self) -> AgeCursor {
        AgeCursor(match self {
            Self::KeySweep => 0,
            Self::Ownership => 1,
            Self::Checkpoint => 2,
            Self::Rebuild => 3,
        })
    }
}

/// A stored page borrowed from the table.
#[derive(Clone)]
pub(super) struct PageView<'a> {
    pub page: &'a PageId,
    pub location: RecordLocation,
    pub metadata: &'a VersionMetadata,
    pub key_id: KeyId,
}
impl PageView<'_> {
    /// Clone the stored entry without its page identity.
    pub fn into_entry(self) -> IndexedPage {
        IndexedPage {
            location: self.location,
            metadata: self.metadata.clone(),
            key_id: self.key_id,
        }
    }

    pub fn into_owned(self) -> (PageId, IndexedPage) {
        (self.page.clone(), self.into_entry())
    }
}

struct Inner {
    disk: Rc<RefCell<DiskIndex>>,
    slots: Interner,
    caches: CacheTable<CacheId>,
    keys: Vec<KeyId>,
}

/// Page table sized at first use; see the module docs.
pub(super) struct Pages {
    inner: Option<Inner>,
    slots: u32,
    /// Stored pages (in-flight reservations excluded).
    stored: usize,
    versions: HashMap<ObjectVersion, (VersionMetadata, usize)>,
}

/// A reserved publication of one page: the disk index reservation, then
/// the appended record while its write is in flight. Dropping it cancels
/// the reservation or fails the write; [`Pages::release`] then frees the
/// interned slot.
pub(super) struct Reservation {
    write: PendingWrite,
    disk: Rc<RefCell<DiskIndex>>,
    slot: PageSlot,
    location: Option<RecordLocation>,
}
impl Reservation {
    /// Append `length` bytes; the reservation survives an error for a retry.
    pub fn append(
        &mut self,
        segments: &Segments,
        length: usize,
    ) -> Result<(page_alloc::SegmentLease, RecordLocation)> {
        if self.location.is_some() {
            return Err(Error::InvalidConfiguration);
        }
        self.disk.borrow_mut().ensure_segments(segments.count())?;
        let (lease, location) = self.write.append(segments, length)?;
        let location = RecordLocation {
            segment: location.segment(),
            generation: location.generation(),
            extent: location.extent(),
        };
        self.location = Some(location.clone());
        Ok((lease, location))
    }
}

fn location(view: &EntryView) -> RecordLocation {
    RecordLocation {
        segment: view.location.segment(),
        generation: view.location.generation(),
        extent: view.location.extent(),
    }
}

impl Inner {
    fn new(slots: u32) -> Result<Self> {
        Ok(Self {
            disk: Rc::new(RefCell::new(
                DiskIndex::new(slots, slots as usize, 0).map_err(Error::from)?,
            )),
            slots: Interner::new(slots),
            caches: CacheTable::default(),
            keys: vec![KeyId([0; 16]); slots as usize],
        })
    }
}

impl Pages {
    /// An empty table of `slots` interned pages.
    pub fn new(slots: u32) -> Self {
        Self {
            inner: None,
            slots,
            stored: 0,
            versions: HashMap::default(),
        }
    }

    /// Stored pages, excluding in-flight reservations.
    pub fn len(&self) -> usize {
        self.stored
    }

    /// Interned page capacity.
    #[cfg(test)]
    pub fn slots(&self) -> u32 {
        self.slots
    }

    /// Resize the slot arena, re-inserting every page in publish order.
    /// Startup only (O(pages)); in-flight reservations are refused.
    pub fn resize(&mut self, slots: u32) -> Result<()> {
        if slots == u32::MAX || (slots as usize) < self.len() {
            return Err(Error::InvalidConfiguration);
        }
        if self
            .inner
            .as_ref()
            .is_some_and(|inner| inner.disk.borrow().len() != self.stored)
        {
            return Err(Error::Overloaded);
        }
        let entries = self.drain_all();
        self.slots = slots;
        self.inner = None;
        self.stored = 0;
        for (page, entry) in entries {
            self.insert(page, entry)?;
        }
        Ok(())
    }

    fn drain_all(&mut self) -> Vec<(PageId, IndexedPage)> {
        let Some(inner) = self.inner.as_mut() else {
            return Vec::new();
        };
        let mut disk = inner.disk.borrow_mut();
        disk.age_reset(Walk::Rebuild.cursor());
        let entries: Vec<_> = disk
            .age_entries(Walk::Rebuild.cursor())
            .map(|(slot, view)| {
                let page = inner.slots.key(slot).expect("interned page");
                (
                    page.clone(),
                    IndexedPage {
                        location: location(&view),
                        metadata: self.versions[&page.version].0.clone(),
                        key_id: inner.keys[slot.0 as usize],
                    },
                )
            })
            .collect();
        drop(disk);
        self.versions.clear();
        entries
    }

    fn inner_mut(&mut self) -> Result<&mut Inner> {
        if self.inner.is_none() {
            self.inner = Some(Inner::new(self.slots)?);
        }
        Ok(self.inner.as_mut().expect("initialized page table"))
    }

    fn slot(&self, page: &PageId) -> Option<(&Inner, PageSlot, EntryView)> {
        let inner = self.inner.as_ref()?;
        let slot = inner.slots.get(page)?;
        let view = inner.disk.borrow().get(slot)?;
        Some((inner, slot, view))
    }

    fn view<'a>(&'a self, inner: &'a Inner, slot: PageSlot, view: &EntryView) -> PageView<'a> {
        let page = inner.slots.key(slot).expect("interned page");
        PageView {
            page,
            location: location(view),
            metadata: &self.versions[&page.version].0,
            key_id: inner.keys[slot.0 as usize],
        }
    }

    pub fn get(&self, page: &PageId) -> Option<PageView<'_>> {
        let (inner, slot, view) = self.slot(page)?;
        Some(self.view(inner, slot, &view))
    }

    pub fn location(&self, page: &PageId) -> Option<RecordLocation> {
        self.slot(page).map(|(_, _, view)| location(&view))
    }

    pub fn contains_key(&self, page: &PageId) -> bool {
        self.slot(page).is_some()
    }

    /// Descriptor shared by the stored pages of `version`.
    pub fn version(&self, version: &ObjectVersion) -> Option<&VersionMetadata> {
        self.versions.get(version).map(|(metadata, _)| metadata)
    }

    /// Whether no in-flight write targets `segment`.
    pub fn can_reclaim(&self, segment: SegmentId) -> bool {
        self.inner.as_ref().is_none_or(|inner| {
            let disk = inner.disk.borrow();
            // Segments beyond the table have never been appended to.
            disk.can_reclaim(segment) || segment.0 >= disk.segment_count() as u64
        })
    }

    fn intern(&mut self, page: &PageId) -> Result<(PageSlot, page_alloc::disk_index::CacheKey)> {
        let inner = self.inner_mut()?;
        let cache = {
            let mut disk = inner.disk.borrow_mut();
            inner
                .caches
                .add(&mut disk, &page.version.object.cache)
                .map_err(|_| Error::Overloaded)?
        };
        let slot = inner.slots.intern(page).map_err(|_| Error::Overloaded)?;
        Ok((slot, cache))
    }

    /// Reserve a publication of `page`. `Overloaded` when the slot arena or
    /// cache table is full, `Busy` when `page` is already being written.
    pub fn reserve(&mut self, page: &PageId) -> Result<Reservation> {
        let (slot, cache) = self.intern(page)?;
        let inner = self.inner.as_mut().expect("initialized page table");
        match PendingWrite::reserve(&inner.disk, slot, cache) {
            Ok(write) => Ok(Reservation {
                write,
                disk: inner.disk.clone(),
                slot,
                location: None,
            }),
            Err(error) => {
                inner.slots.release_unused(&inner.disk.borrow(), slot);
                Err(error.into())
            }
        }
    }

    /// Free what a dropped reservation of this table left: its interned slot
    /// and any cache that finished draining.
    pub fn release(&mut self, reservation: Reservation) {
        let Reservation {
            write, disk, slot, ..
        } = reservation;
        drop(write);
        let Some(inner) = self.inner.as_mut() else {
            return;
        };
        if !Rc::ptr_eq(&inner.disk, &disk) {
            return;
        }
        inner.slots.release_unused(&inner.disk.borrow(), slot);
        self.settle();
    }

    /// Publish a written reservation. Returns `Ok(false)` without change when
    /// its cache was removed or the table was rebuilt meanwhile.
    pub fn publish(
        &mut self,
        reservation: Reservation,
        page: &PageId,
        entry: IndexedPage,
    ) -> Result<bool> {
        let IndexedPage {
            location,
            metadata,
            key_id,
        } = entry;
        let same = self
            .inner
            .as_ref()
            .is_some_and(|inner| Rc::ptr_eq(&inner.disk, &reservation.disk))
            && self.inner.as_ref().and_then(|inner| inner.slots.get(page))
                == Some(reservation.slot);
        if !same || reservation.location.as_ref() != Some(&location) {
            let mismatch = same;
            self.release(reservation);
            return if mismatch {
                Err(Error::InvalidConfiguration)
            } else {
                Ok(false)
            };
        }
        let Reservation { write, slot, .. } = reservation;
        let inner = self.inner.as_mut().expect("initialized page table");
        let replaced = match write.publish() {
            Ok(replaced) => replaced,
            Err(page_alloc::Error::Stale) => {
                inner.slots.release_unused(&inner.disk.borrow(), slot);
                self.settle();
                return Ok(false);
            }
            Err(error) => {
                inner.slots.release_unused(&inner.disk.borrow(), slot);
                return Err(error.into());
            }
        };
        inner.keys[slot.0 as usize] = key_id;
        if replaced.is_none() {
            self.stored += 1;
            self.versions
                .entry(page.version.clone())
                .or_insert_with(|| (metadata, 0))
                .1 += 1;
        }
        Ok(true)
    }

    /// Store `entry` at its already validated location, replacing any
    /// record of `page` (which moves to the publish-order tail). `Overloaded`
    /// when the slot arena or the cache table is full; nothing changes on error.
    pub fn insert(&mut self, page: PageId, entry: IndexedPage) -> Result<()> {
        let IndexedPage {
            location,
            metadata,
            key_id,
        } = entry;
        let (slot, cache) = self.intern(&page)?;
        let inner = self.inner.as_mut().expect("initialized page table");
        let placed = inner.disk.borrow_mut().place(
            slot,
            cache,
            location.segment,
            location.generation,
            location.extent,
        );
        let replaced = match placed {
            Ok(replaced) => replaced,
            Err(error) => {
                inner.slots.release_unused(&inner.disk.borrow(), slot);
                return Err(error.into());
            }
        };
        inner.keys[slot.0 as usize] = key_id;
        if replaced.is_none() {
            self.stored += 1;
            self.versions
                .entry(page.version.clone())
                .or_insert_with(|| (metadata, 0))
                .1 += 1;
        }
        Ok(())
    }

    fn forget(
        versions: &mut HashMap<ObjectVersion, (VersionMetadata, usize)>,
        slots: &mut Interner,
        disk: &DiskIndex,
        slot: PageSlot,
    ) {
        let page = slots.key(slot).expect("interned page");
        if let Some((_, count)) = versions.get_mut(&page.version) {
            *count -= 1;
            if *count == 0 {
                versions.remove(&page.version);
            }
        }
        slots.release_unused(disk, slot);
    }

    /// Remove `page` (only when stored at `expected`, if given).
    pub fn remove(&mut self, page: &PageId, expected: Option<&RecordLocation>) -> bool {
        let Some(inner) = self.inner.as_mut() else {
            return false;
        };
        let Some(slot) = inner.slots.get(page) else {
            return false;
        };
        let mut disk = inner.disk.borrow_mut();
        let Some(view) = disk.get(slot) else {
            return false;
        };
        if expected.is_some_and(|expected| *expected != location(&view)) {
            return false;
        }
        disk.remove(slot).expect("stored page");
        self.stored -= 1;
        Self::forget(&mut self.versions, &mut inner.slots, &disk, slot);
        true
    }

    /// Remove every page of `caches` (cache lists only; no full scan). A
    /// cache with writes in flight finishes draining when they end.
    pub fn remove_caches<'a>(&mut self, caches: impl IntoIterator<Item = &'a CacheId>) {
        let Some(inner) = self.inner.as_mut() else {
            return;
        };
        let mut any = false;
        for cache in caches {
            any |= inner.caches.remove(cache);
        }
        if any {
            self.settle();
        }
    }

    fn settle(&mut self) {
        let Some(inner) = self.inner.as_mut() else {
            return;
        };
        if inner.caches.draining() == 0 {
            return;
        }
        let Inner {
            disk,
            slots,
            caches,
            ..
        } = inner;
        let mut disk = disk.borrow_mut();
        let mut removed = Vec::new();
        caches.drain(&mut disk, usize::MAX, |slot, _| removed.push(slot));
        self.stored -= removed.len();
        for slot in removed {
            Self::forget(&mut self.versions, slots, &disk, slot);
        }
    }

    /// Every stored page, in slot order.
    pub fn iter(&self) -> impl Iterator<Item = PageView<'_>> {
        self.inner.iter().flat_map(move |inner| {
            let entries: Vec<_> = inner.disk.borrow().iter().collect();
            entries
                .into_iter()
                .map(move |(slot, view)| self.view(inner, slot, &view))
        })
    }

    /// Resume `walk` in publish order. Token 0 restarts at the oldest page;
    /// any other token continues where the previous call stopped. `visit`
    /// sees each page until it returns `None` or `limit` pages were seen;
    /// those are consumed. Returns a nonzero token after progress, or `token`
    /// unchanged when nothing was consumed.
    pub fn walk<T>(
        &mut self,
        walk: Walk,
        token: u64,
        limit: usize,
        mut visit: impl FnMut(PageView<'_>) -> Option<T>,
    ) -> (u64, Vec<T>) {
        let cursor = walk.cursor();
        let Some(inner) = self.inner.as_ref() else {
            return (token, Vec::new());
        };
        let mut disk = inner.disk.borrow_mut();
        if token == 0 {
            disk.age_reset(cursor);
        }
        let mut last = None;
        let mut out = Vec::new();
        for (slot, view) in disk.age_entries(cursor).take(limit) {
            let Some(item) = visit(self.view(inner, slot, &view)) else {
                break;
            };
            out.push(item);
            last = Some(slot);
        }
        let Some(last) = last else {
            return (token, out);
        };
        disk.age_seek(cursor, last).expect("walked page is stored");
        (token.saturating_add(out.len() as u64).max(1), out)
    }

    /// Up to `budget` stored pages of `segment`.
    pub fn segment(&self, segment: SegmentId, budget: usize) -> Vec<(PageId, RecordLocation)> {
        let Some(inner) = self.inner.as_ref() else {
            return Vec::new();
        };
        inner
            .disk
            .borrow()
            .segment_entries(segment)
            .take(budget)
            .map(|(slot, view)| {
                (
                    inner.slots.key(slot).expect("interned page").clone(),
                    location(&view),
                )
            })
            .collect()
    }

    pub fn segment_empty(&self, segment: SegmentId) -> bool {
        self.inner
            .as_ref()
            .is_none_or(|inner| inner.disk.borrow().segment_usage(segment).0 == 0)
    }

    /// Sum a bounded prefix and conservatively charge the unseen suffix.
    pub fn segment_score(
        &self,
        segment: SegmentId,
        budget: usize,
        unseen_value: u64,
        mut score: impl FnMut(&PageId, &VersionMetadata) -> u64,
    ) -> u64 {
        let Some(inner) = self.inner.as_ref() else {
            return 0;
        };
        let disk = inner.disk.borrow();
        let count = disk.segment_usage(segment).0 as usize;
        let known = disk
            .segment_entries(segment)
            .take(budget)
            .fold(0u64, |sum, (slot, _)| {
                let page = inner.slots.key(slot).expect("interned page");
                sum.saturating_add(score(page, &self.versions[&page.version].0))
            });
        known.saturating_add((count.saturating_sub(budget) as u64).saturating_mul(unseen_value))
    }

    #[cfg(test)]
    pub fn check(&self) {
        let Some(inner) = self.inner.as_ref() else {
            return;
        };
        let disk = inner.disk.borrow();
        assert_eq!(inner.slots.len(), disk.len());
        assert_eq!(self.stored, disk.iter().count());
        drop(disk);
        let mut counts: HashMap<&ObjectVersion, usize> = HashMap::default();
        for view in self.iter() {
            *counts.entry(&view.page.version).or_default() += 1;
        }
        assert_eq!(counts.len(), self.versions.len());
        for (version, count) in counts {
            assert_eq!(self.versions[version].1, count);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::ObjectId;
    use crate::model::PageNumber;
    use crate::model::StrongEtag;
    use page_alloc::Extent;
    use page_alloc::Generation;

    fn page(cache: &str, etag: &str, number: u64) -> PageId {
        PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(cache.into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            number: PageNumber(number),
        }
    }

    fn entry(page: &PageId, segment: u64) -> IndexedPage {
        IndexedPage {
            location: RecordLocation {
                segment: SegmentId(segment),
                generation: Generation(1),
                extent: Extent::new(segment * 4096, 512).unwrap(),
            },
            metadata: VersionMetadata {
                content_type: None,
                version: page.version.clone(),
                length: 1 << 30,
            },
            key_id: KeyId([7; 16]),
        }
    }

    #[test]
    fn versions_are_shared_and_released_with_their_last_page() {
        let mut pages = Pages::new(4);
        let (a0, a1) = (page("c", "a", 0), page("c", "a", 1));
        pages.insert(a0.clone(), entry(&a0, 0)).unwrap();
        pages.insert(a1.clone(), entry(&a1, 1)).unwrap();
        pages.insert(a1.clone(), entry(&a1, 2)).unwrap();
        assert_eq!(pages.versions[&a0.version].1, 2);
        assert_eq!(pages.location(&a1).unwrap().segment, SegmentId(2));
        pages.check();
        assert!(!pages.remove(&a0, Some(&entry(&a0, 5).location)));
        assert!(pages.remove(&a0, None));
        assert!(pages.version(&a0.version).is_some());
        assert!(pages.remove(&a1, None));
        assert!(pages.version(&a0.version).is_none());
        assert_eq!(pages.len(), 0);
        pages.check();
    }

    #[test]
    fn full_arena_overloads_without_change() {
        let mut pages = Pages::new(1);
        let (a, b) = (page("c", "a", 0), page("c", "b", 0));
        pages.insert(a.clone(), entry(&a, 0)).unwrap();
        assert_eq!(
            pages.insert(b.clone(), entry(&b, 0)),
            Err(Error::Overloaded)
        );
        assert!(!pages.contains_key(&b));
        assert!(pages.version(&b.version).is_none());
        pages.check();
    }

    #[test]
    fn walks_resume_independently_and_restart_at_zero() {
        let mut pages = Pages::new(8);
        let ids: Vec<_> = (0..4).map(|n| page("c", "a", n)).collect();
        for (n, id) in ids.iter().enumerate() {
            pages.insert(id.clone(), entry(id, n as u64)).unwrap();
        }
        let numbers = |pages: &mut Pages, walk, token, limit| {
            let (token, out) = pages.walk(walk, token, limit, |view| Some(view.page.number.0));
            (token, out)
        };
        let (t, out) = numbers(&mut pages, Walk::Checkpoint, 0, 2);
        assert_eq!(out, [0, 1]);
        assert_ne!(t, 0);
        let (_, other) = numbers(&mut pages, Walk::KeySweep, 0, 3);
        assert_eq!(other, [0, 1, 2]);
        // Removing the parked page skips nothing; a replacement moves to the tail.
        pages.remove(&ids[1], None);
        pages.insert(ids[0].clone(), entry(&ids[0], 6)).unwrap();
        let (t2, out) = numbers(&mut pages, Walk::Checkpoint, t, 8);
        assert_eq!(out, [2, 3, 0]);
        assert_eq!(numbers(&mut pages, Walk::Checkpoint, t2, 8), (t2, vec![]));
        assert_eq!(numbers(&mut pages, Walk::Checkpoint, 0, 8).1, [2, 3, 0]);
    }

    #[test]
    fn cache_removal_and_resize_keep_order_and_counts() {
        let mut pages = Pages::new(4);
        let (a, b, c) = (page("x", "a", 0), page("y", "b", 0), page("x", "c", 0));
        for (n, id) in [&a, &b, &c].into_iter().enumerate() {
            pages.insert(id.clone(), entry(id, n as u64)).unwrap();
        }
        pages.resize(8).unwrap();
        assert_eq!(pages.slots(), 8);
        assert_eq!(
            pages
                .walk(Walk::Ownership, 0, 8, |v| Some(v.page.clone()))
                .1,
            [a.clone(), b.clone(), c.clone()]
        );
        assert_eq!(pages.resize(2), Err(Error::InvalidConfiguration));
        pages.remove_caches([&CacheId("x".into())]);
        assert_eq!(pages.len(), 1);
        assert!(pages.contains_key(&b));
        assert!(pages.segment_empty(SegmentId(0)));
        assert_eq!(pages.segment(SegmentId(1), 4).len(), 1);
        pages.check();
        // A removed cache can return under a fresh index.
        pages.insert(a.clone(), entry(&a, 0)).unwrap();
        pages.check();
    }
}
