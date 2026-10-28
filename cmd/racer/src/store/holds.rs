//! Worker-local hard holds. Each reserved page charges one whole segment.
//! Live expiry requires both clocks to pass the deadline. Restart uses wall time
//! only; checkpoints and payloads keep the store's existing no-fsync crash limits.

use super::catalog::RecordLocation;
use crate::error::{Error, Result};
use crate::model::{ObjectVersion, PageNumber};
use crate::runtime::HashMap;
use page_alloc::{SegmentId, Segments};
use std::cell::{Cell, RefCell};
use std::rc::Rc;
use std::time::{Duration, Instant, SystemTime};

pub const MAX_HOLD_TTL: Duration = Duration::from_secs(30 * 24 * 60 * 60);

/// Identity of one reservation on a worker. Every holder-adding reservation and
/// every restored page gets a fresh, increasing epoch, and each page remembers
/// the epoch that added it to its hold ([`PageHolders::since`]). A page release
/// only applies to pages whose `since` is not newer than the release's epoch,
/// so a release taken before a page was trimmed and re-reserved (or before an
/// expiry, whole release or restart) cannot drop a holder that the later
/// reservation added. Epochs are process-local.
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

/// Holders of one member page.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct PageHolders {
    /// All holders, including `warm`.
    pub count: u32,
    /// Holders added by warm jobs. Warm cleanup ([`super::catalog::Index::release_hold`])
    /// drops only these from unpublished pages, so independent readers keep theirs.
    pub warm: u32,
    /// Epoch of the reservation that made this page a member.
    pub since: HoldEpoch,
}

/// Who a reservation adds as a holder.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum HolderKind {
    /// A reader that releases its pages individually.
    Reader,
    /// A warm job, released by warm cleanup and expiry.
    Warm,
}

/// Held writes in flight, per page. A page released from the hold keeps its
/// entry here until its write completes (the I/O still owns the entry), but it
/// no longer offsets the capacity reserved for the remaining members.
#[derive(Debug, Default)]
pub(super) struct Inflight(RefCell<HashMap<PageNumber, usize>>);

impl Inflight {
    pub fn add(&self, page: PageNumber) {
        *self.0.borrow_mut().entry(page).or_insert(0) += 1;
    }

    pub fn remove(&self, page: PageNumber) {
        let mut pages = self.0.borrow_mut();
        let count = pages.get_mut(&page).expect("tracked held write");
        *count -= 1;
        if *count == 0 {
            pages.remove(&page);
        }
    }

    pub fn is_empty(&self) -> bool {
        self.0.borrow().is_empty()
    }

    /// In-flight held writes for pages in `members`.
    pub fn within(&self, members: &HoldPages) -> usize {
        self.0
            .borrow()
            .iter()
            .filter(|(page, _)| members.contains(**page))
            .map(|(_, count)| count)
            .sum()
    }
}

pub(super) struct Hold {
    pub until: SystemTime,
    pub monotonic: Instant,
    /// Pages this worker promised to retain. Capacity is charged per member.
    pub pages: HoldPages,
    /// Holders per member page; the keys are exactly `pages`. Each
    /// reservation adds one per requested page and each `release_hold_pages`
    /// removes one. Not persisted: restored pages start at one reader.
    pub holders: HashMap<PageNumber, PageHolders>,
    /// Epoch of the latest reservation into this entry.
    pub epoch: HoldEpoch,
    pub inflight: Rc<Inflight>,
    pub active: Rc<Cell<usize>>,
    pub locations: HashMap<PageNumber, RecordLocation>,
}

impl Hold {
    /// One reader for every page, used when no holder history is known.
    pub fn single_holders(pages: &HoldPages, since: HoldEpoch) -> HashMap<PageNumber, PageHolders> {
        pages
            .iter()
            .map(|page| {
                (
                    page,
                    PageHolders {
                        count: 1,
                        warm: 0,
                        since,
                    },
                )
            })
            .collect()
    }

    pub fn live(&self) -> bool {
        uring_runtime::environment::wall_now() < self.until
            || uring_runtime::environment::now() < self.monotonic
    }

    pub fn unused(&self) -> usize {
        self.pages
            .len()
            .saturating_sub(self.locations.len() + self.inflight.within(&self.pages))
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
    /// A fresh epoch for a reservation or restored entry.
    pub fn next_epoch(&mut self) -> HoldEpoch {
        self.last_epoch += 1;
        HoldEpoch(self.last_epoch)
    }

    pub fn expire(&mut self) {
        self.entries
            .retain(|_, hold| hold.live() || !hold.inflight.is_empty() || hold.active.get() != 0);
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
