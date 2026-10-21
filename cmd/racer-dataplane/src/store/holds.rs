//! Worker-local hard holds. Each reserved page charges one whole segment.
//! Live expiry requires both clocks to pass the deadline. Restart uses wall time
//! only; checkpoints and payloads keep the store's existing no-fsync crash limits.

use super::catalog::RecordLocation;
use crate::error::{Error, Result};
use crate::model::{ObjectVersion, PageNumber};
use crate::runtime::HashMap;
use page_alloc::{SegmentId, Segments};
use std::cell::Cell;
use std::rc::Rc;
use std::time::{Duration, Instant, SystemTime};

pub const MAX_HOLD_TTL: Duration = Duration::from_secs(30 * 24 * 60 * 60);

/// Identity of one hold entry's lifetime on a worker. A version's hold gets a
/// new epoch whenever its entry is created, including after expiry, whole
/// release or restart, so a page release carrying an older epoch cannot drop a
/// holder that a later reservation added. Epochs are process-local.
#[derive(Clone, Copy, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct HoldEpoch(u64);

/// Sorted, deduplicated page numbers covered by one version's hold.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct HoldPages(Vec<PageNumber>);

impl HoldPages {
    /// Pages `0..count`, for callers that hold a whole prefix of a version.
    pub fn prefix(count: u64) -> Self {
        Self((0..count).map(PageNumber).collect())
    }

    pub fn len(&self) -> usize {
        self.0.len()
    }

    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    pub fn contains(&self, page: PageNumber) -> bool {
        self.0.binary_search(&page).is_ok()
    }

    pub fn iter(&self) -> impl Iterator<Item = PageNumber> + '_ {
        self.0.iter().copied()
    }

    /// Append a page. Ascending input stays linear; anything else is re-sorted.
    pub fn insert(&mut self, page: PageNumber) {
        match self.0.last() {
            Some(last) if *last >= page => {
                if let Err(at) = self.0.binary_search(&page) {
                    self.0.insert(at, page);
                }
            }
            _ => self.0.push(page),
        }
    }

    /// Sorted merge of two page sets.
    pub fn union(&self, other: &Self) -> Self {
        let mut merged = Vec::with_capacity(self.0.len() + other.0.len());
        let (mut a, mut b) = (self.0.iter().peekable(), other.0.iter().peekable());
        loop {
            let next = match (a.peek(), b.peek()) {
                (Some(x), Some(y)) if x < y => a.next(),
                (Some(x), Some(y)) if x > y => b.next(),
                (Some(_), Some(_)) => {
                    b.next();
                    a.next()
                }
                (Some(_), None) => a.next(),
                (None, Some(_)) => b.next(),
                (None, None) => break,
            };
            merged.extend(next.copied());
        }
        Self(merged)
    }
}

impl FromIterator<PageNumber> for HoldPages {
    fn from_iter<I: IntoIterator<Item = PageNumber>>(iter: I) -> Self {
        let mut pages: Vec<_> = iter.into_iter().collect();
        pages.sort_unstable();
        pages.dedup();
        Self(pages)
    }
}

pub(super) struct Hold {
    pub until: SystemTime,
    pub monotonic: Instant,
    /// Pages this worker promised to retain. Capacity is charged per member.
    pub pages: HoldPages,
    /// Holder count per member page; the keys are exactly `pages`. Each
    /// `reserve_hold_pages` adds one per requested page and each
    /// `release_hold_pages` removes one. Not persisted: restored pages start at one.
    pub holders: HashMap<PageNumber, u32>,
    /// Lifetime identity returned by reservation and required by page release.
    pub epoch: HoldEpoch,
    pub inflight: Rc<Cell<usize>>,
    pub active: Rc<Cell<usize>>,
    pub locations: HashMap<PageNumber, RecordLocation>,
}

impl Hold {
    /// Counts of one for every page, used when no holder history is known.
    pub fn single_holders(pages: &HoldPages) -> HashMap<PageNumber, u32> {
        pages.iter().map(|page| (page, 1)).collect()
    }

    pub fn live(&self) -> bool {
        uring_runtime::environment::wall_now() < self.until
            || uring_runtime::environment::now() < self.monotonic
    }

    pub fn unused(&self) -> usize {
        self.pages
            .len()
            .saturating_sub(self.locations.len() + self.inflight.get())
    }
}

pub(super) struct Holds {
    pub entries: HashMap<ObjectVersion, Hold>,
    pub segments: Option<Rc<Segments>>,
    pub free_reserve: usize,
    pub percent: usize,
    /// Last epoch handed to a newly created entry.
    pub last_epoch: u64,
}

impl Default for Holds {
    fn default() -> Self {
        Self {
            entries: HashMap::default(),
            segments: None,
            free_reserve: 1,
            percent: 50,
            last_epoch: 0,
        }
    }
}

impl Holds {
    /// A fresh epoch for a newly created hold entry.
    pub fn next_epoch(&mut self) -> HoldEpoch {
        self.last_epoch += 1;
        HoldEpoch(self.last_epoch)
    }

    pub fn expire(&mut self) {
        self.entries
            .retain(|_, hold| hold.live() || hold.inflight.get() != 0 || hold.active.get() != 0);
    }

    pub fn unused(&self) -> usize {
        self.entries.values().map(Hold::unused).sum()
    }

    pub fn segment_held(&self, segment: SegmentId) -> bool {
        self.entries.values().any(|hold| {
            hold.live()
                && hold
                    .locations
                    .values()
                    .any(|location| location.segment == segment)
        })
    }

    pub fn deadline(until: SystemTime) -> Result<Instant> {
        let ttl = until
            .duration_since(uring_runtime::environment::wall_now())
            .map_err(|_| Error::InvalidConfiguration)?;
        if ttl.is_zero() || ttl > MAX_HOLD_TTL {
            return Err(Error::InvalidConfiguration);
        }
        uring_runtime::environment::now()
            .checked_add(ttl)
            .ok_or(Error::InvalidConfiguration)
    }
}

pub(super) struct HoldGuard(pub Rc<Cell<usize>>);

impl Drop for HoldGuard {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}
