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
    pub inflight: Rc<Cell<usize>>,
    pub active: Rc<Cell<usize>>,
    pub locations: HashMap<PageNumber, RecordLocation>,
}

impl Hold {
    pub fn live(&self) -> bool {
        racer_platform::environment::wall_now() < self.until
            || racer_platform::environment::now() < self.monotonic
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
}

impl Default for Holds {
    fn default() -> Self {
        Self {
            entries: HashMap::default(),
            segments: None,
            free_reserve: 1,
            percent: 50,
        }
    }
}

impl Holds {
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
            .duration_since(racer_platform::environment::wall_now())
            .map_err(|_| Error::InvalidConfiguration)?;
        if ttl.is_zero() || ttl > MAX_HOLD_TTL {
            return Err(Error::InvalidConfiguration);
        }
        racer_platform::environment::now()
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
