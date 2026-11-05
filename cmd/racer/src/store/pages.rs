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
use page_alloc::disk_index::AgeCursor;
use page_alloc::disk_index::DiskIndex;
use page_alloc::disk_index::EntryView;
use page_alloc::disk_index::PageSlot;
use page_alloc::intern::CacheTable;
use page_alloc::intern::SlotInterner;
use racer_control_wire::CacheId;
use racer_control_wire::KeyId;

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
    pub fn into_owned(self) -> (PageId, IndexedPage) {
        (
            self.page.clone(),
            IndexedPage {
                location: self.location,
                metadata: self.metadata.clone(),
                key_id: self.key_id,
            },
        )
    }
}

struct Inner {
    disk: DiskIndex,
    slots: Interner,
    caches: CacheTable<CacheId>,
    keys: Vec<KeyId>,
}

/// Page table sized at first use; see the module docs.
pub(super) struct Pages {
    inner: Option<Inner>,
    slots: u32,
    versions: HashMap<ObjectVersion, (VersionMetadata, usize)>,
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
            disk: DiskIndex::new(slots, slots as usize, 0).map_err(Error::from)?,
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
            versions: HashMap::default(),
        }
    }

    pub fn len(&self) -> usize {
        self.inner.as_ref().map_or(0, |inner| inner.disk.len())
    }

    /// Interned page capacity.
    #[cfg(test)]
    pub fn slots(&self) -> u32 {
        self.slots
    }

    /// Resize the slot arena, re-inserting every page in publish order.
    /// Startup only: O(pages).
    pub fn resize(&mut self, slots: u32) -> Result<()> {
        if slots as usize > u32::MAX as usize - 1 || (slots as usize) < self.len() {
            return Err(Error::InvalidConfiguration);
        }
        let entries = self.drain_all();
        self.slots = slots;
        self.inner = None;
        for (page, entry) in entries {
            self.insert(page, entry)?;
        }
        Ok(())
    }

    fn drain_all(&mut self) -> Vec<(PageId, IndexedPage)> {
        let Some(inner) = self.inner.as_mut() else {
            return Vec::new();
        };
        inner.disk.age_reset(Walk::Rebuild.cursor());
        let entries: Vec<_> = inner
            .disk
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
        self.versions.clear();
        entries
    }

    fn slot(&self, page: &PageId) -> Option<(&Inner, PageSlot, EntryView)> {
        let inner = self.inner.as_ref()?;
        let slot = inner.slots.get(page)?;
        let view = inner.disk.get(slot)?;
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

    /// Store `entry`, replacing any record of `page` (which moves to the
    /// publish-order tail). `Overloaded` when the slot arena or the cache
    /// table is full; nothing changes on error.
    pub fn insert(&mut self, page: PageId, entry: IndexedPage) -> Result<()> {
        let IndexedPage {
            location,
            metadata,
            key_id,
        } = entry;
        let versions = &mut self.versions;
        if self.inner.is_none() {
            self.inner = Some(Inner::new(self.slots)?);
        }
        let inner = self.inner.as_mut().expect("initialized page table");
        let cache = inner
            .caches
            .add(&mut inner.disk, &page.version.object.cache)
            .map_err(|_| Error::Overloaded)?;
        let slot = inner.slots.intern(&page).map_err(|_| Error::Overloaded)?;
        let replaced = match inner.disk.place(
            slot,
            cache,
            location.segment,
            location.generation,
            location.extent,
        ) {
            Ok(replaced) => replaced,
            Err(error) => {
                inner.slots.release_unused(&inner.disk, slot);
                return Err(error.into());
            }
        };
        inner.keys[slot.0 as usize] = key_id;
        if replaced.is_none() {
            versions
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
        let Some(view) = inner.disk.get(slot) else {
            return false;
        };
        if expected.is_some_and(|expected| *expected != location(&view)) {
            return false;
        }
        inner.disk.remove(slot).expect("stored page");
        Self::forget(&mut self.versions, &mut inner.slots, &inner.disk, slot);
        true
    }

    /// Remove every page of `caches` (cache lists only; no full scan).
    pub fn remove_caches<'a>(&mut self, caches: impl IntoIterator<Item = &'a CacheId>) {
        let Some(inner) = self.inner.as_mut() else {
            return;
        };
        let mut any = false;
        for cache in caches {
            any |= inner.caches.remove(cache);
        }
        if !any {
            return;
        }
        let Inner {
            disk,
            slots,
            caches,
            ..
        } = inner;
        let versions = &mut self.versions;
        let mut removed = Vec::new();
        caches.drain(disk, usize::MAX, |slot, _| removed.push(slot));
        for slot in removed {
            Self::forget(versions, slots, disk, slot);
        }
    }

    /// Every stored page, in slot order.
    pub fn iter(&self) -> impl Iterator<Item = PageView<'_>> {
        self.inner.iter().flat_map(move |inner| {
            inner
                .disk
                .iter()
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
        let Some(inner) = self.inner.as_mut() else {
            return (token, Vec::new());
        };
        if token == 0 {
            inner.disk.age_reset(cursor);
        }
        let inner = self.inner.as_ref().expect("initialized page table");
        let mut last = None;
        let mut out = Vec::new();
        for (slot, view) in inner.disk.age_entries(cursor).take(limit) {
            let Some(item) = visit(self.view(inner, slot, &view)) else {
                break;
            };
            out.push(item);
            last = Some(slot);
        }
        let Some(last) = last else {
            return (token, out);
        };
        self.inner
            .as_mut()
            .expect("initialized page table")
            .disk
            .age_seek(cursor, last)
            .expect("walked page is stored");
        (token.saturating_add(out.len() as u64).max(1), out)
    }

    /// Up to `budget` stored pages of `segment`.
    pub fn segment(&self, segment: SegmentId, budget: usize) -> Vec<(PageId, RecordLocation)> {
        let Some(inner) = self.inner.as_ref() else {
            return Vec::new();
        };
        inner
            .disk
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
            .is_none_or(|inner| inner.disk.segment_usage(segment).0 == 0)
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
        let count = inner.disk.segment_usage(segment).0 as usize;
        let known =
            inner
                .disk
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
        assert_eq!(inner.slots.len(), inner.disk.len());
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
