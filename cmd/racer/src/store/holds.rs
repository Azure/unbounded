//! Worker-local hard holds. Each reserved page charges one whole segment.
//! Live expiry follows the monotonic clock only. Restart uses wall time
//! only; checkpoints and payloads keep the store's existing no-fsync crash limits.

use super::pages::Pages;
use crate::error::{Error, Result};
use crate::model::{ObjectVersion, PageNumber};
use crate::runtime::HashMap;
use page_alloc::Segments;
use page_alloc::disk_index::{HoldId, Monotonic, PageSlot};
use std::cell::Cell;
use std::rc::Rc;
use std::time::{Duration, Instant, SystemTime};

pub const MAX_HOLD_TTL: Duration = Duration::from_secs(30 * 24 * 60 * 60);

/// Identity of one reservation on a worker: the newest disk index hold
/// identity when the reservation was made. Each page remembers the hold
/// that made it a member ([`PageHolders::since`]); identities only grow, so a
/// release only applies to pages whose `since` is not newer than the
/// release's epoch, and a release taken before a page was trimmed and
/// re-reserved (or before an expiry, whole release or restart) cannot drop a
/// holder that the later reservation added. Epochs are process-local.
#[derive(Clone, Copy, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct HoldEpoch(pub(super) HoldId);

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
    /// Reader holders; each releases its pages individually.
    pub count: u32,
    /// The disk index hold that made this page a member.
    pub since: HoldEpoch,
    /// The page's interned slot.
    pub slot: PageSlot,
}

/// One version's hard hold. The disk index holds every member page (one
/// disk hold per reservation that added members); this entry keeps reader
/// counts and the deadlines.
pub(super) struct Hold {
    /// Wall-clock deadline a client asked for, for reporting only.
    pub until: SystemTime,
    pub monotonic: Instant,
    /// Pages this worker promised to retain. Capacity is charged per member.
    pub pages: HoldPages,
    /// Holders per member page; the keys are exactly `pages`. Each
    /// reservation adds one per requested page and each `release_hold_pages`
    /// removes one. Not persisted: restored pages start at one reader.
    pub holders: HashMap<PageNumber, PageHolders>,
    /// Live disk index holds of this entry.
    pub ids: Vec<HoldId>,
    pub active: Rc<Cell<usize>>,
}

impl Hold {
    /// Whether the hold has not expired. Expiry follows the monotonic clock
    /// only: `until` is the wall-clock deadline a client asked for, kept for
    /// persistence and reporting, and a wall-clock step must not extend it.
    pub fn live(&self, clock: &racer_io::Clock) -> bool {
        clock.now() < self.monotonic
    }
}

/// Default hold cap, as a percentage of page slots and segments.
pub const DEFAULT_HOLD_PERCENT: usize = 50;

pub(super) struct Holds {
    pub entries: HashMap<ObjectVersion, Hold>,
    /// Version of each live disk index hold.
    pub owners: HashMap<HoldId, ObjectVersion>,
    /// Member pages across all entries.
    pub members: usize,
    pub segments: Option<Rc<Segments>>,
    pub free_reserve: usize,
    pub percent: usize,
    /// Origin of disk index deadlines.
    base: Instant,
}

impl Holds {
    pub fn new(clock: &racer_io::Clock) -> Self {
        Self {
            base: clock.now(),
            entries: HashMap::default(),
            owners: HashMap::default(),
            members: 0,
            segments: None,
            free_reserve: 1,
            percent: DEFAULT_HOLD_PERCENT,
        }
    }

    /// `at` on the disk index's monotonic scale.
    pub fn at(&self, at: Instant) -> Monotonic {
        let nanos = at.saturating_duration_since(self.base).as_nanos();
        Monotonic(u64::try_from(nanos).unwrap_or(u64::MAX))
    }

    /// Forget a disk index hold that was released or expired, with the
    /// members it added. An entry without disk holds is removed.
    pub fn forget(&mut self, id: HoldId) {
        let Some(version) = self.owners.remove(&id) else {
            return;
        };
        let Some(hold) = self.entries.get_mut(&version) else {
            return;
        };
        hold.ids.retain(|held| *held != id);
        let before = hold.holders.len();
        hold.holders.retain(|_, holder| holder.since.0 != id);
        self.members -= before - hold.holders.len();
        let holders = &hold.holders;
        hold.pages = hold
            .pages
            .iter()
            .filter(|page| holders.contains_key(page))
            .collect();
        if hold.ids.is_empty() {
            self.entries.remove(&version);
        }
    }

    /// Remove `version`'s entry and release its disk holds.
    pub fn drop_version(&mut self, pages: &mut Pages, version: &ObjectVersion) {
        let Some(hold) = self.entries.remove(version) else {
            return;
        };
        self.members -= hold.pages.len();
        for id in hold.ids {
            self.owners.remove(&id);
            pages.release_hold(id);
        }
    }

    pub fn deadline(until: SystemTime, clock: &racer_io::Clock) -> Result<Instant> {
        let ttl = until
            .duration_since(clock.wall_now())
            .map_err(|_| Error::InvalidConfiguration)?;
        if ttl.is_zero() || ttl > MAX_HOLD_TTL {
            return Err(Error::InvalidConfiguration);
        }
        clock
            .now()
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

#[cfg(test)]
mod tests {
    use super::*;

    fn hold(clock: &racer_io::Clock, ttl: Duration) -> Hold {
        let until = clock.wall_now() + ttl;
        Hold {
            until,
            monotonic: Holds::deadline(until, clock).unwrap(),
            pages: HoldPages::default(),
            holders: HashMap::default(),
            ids: Vec::new(),
            active: Rc::default(),
        }
    }
    #[test]
    fn live_follows_monotonic_time_and_ignores_wall_steps() {
        let virtual_clock = racer_io::ctx::VirtualClock::new();
        let clock = racer_io::Clock::from(virtual_clock.clone());
        let held = hold(&clock, Duration::from_secs(10));
        assert!(held.live(&clock));
        // A backward wall step does not extend the hold past its monotonic deadline.
        virtual_clock
            .set_wall_time(clock.wall_now() - Duration::from_secs(3600))
            .unwrap();
        virtual_clock.advance(Duration::from_secs(10)).unwrap();
        assert!(clock.wall_now() < held.until);
        assert!(!held.live(&clock));
        // A forward wall step does not end a hold whose monotonic deadline is ahead.
        let held = hold(&clock, Duration::from_secs(10));
        virtual_clock
            .set_wall_time(clock.wall_now() + Duration::from_secs(3600))
            .unwrap();
        assert!(clock.wall_now() > held.until);
        assert!(held.live(&clock));
    }
}
