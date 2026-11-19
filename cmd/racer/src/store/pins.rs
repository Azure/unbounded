//! Worker-local fill pins. A pin holds pages for a cache fill and, unlike a
//! spill hold, does not count against `RACER_RETAINED_SEGMENT_PERCENT`.
//! Pinned records are written into their own segment class, so a held page
//! does not consume a whole shared segment. Live expiry follows the monotonic
//! clock only; restart drops every pin and recovery clears pinned flags.

use super::pages::Pages;
use crate::error::{Error, Result};
use crate::model::{ObjectVersion, PageId, PageNumber};
use crate::runtime::HashMap;
use page_alloc::SegmentId;
use page_alloc::Segments;
use page_alloc::disk_index::{HoldId, PageSlot};
use std::rc::Rc;
use std::time::{Duration, Instant, SystemTime};

pub const MAX_PIN_TTL: Duration = Duration::from_secs(30 * 24 * 60 * 60);

/// Default pin cap, as a percentage of page slots and segments.
pub const DEFAULT_PIN_PERCENT: usize = 50;

/// Opaque local identity of one cache fill. The fill controller chooses it and
/// uses it to reserve, renew, and release its pages.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct FillId(pub u64);

/// Identity of one pin reservation, like [`super::holds::HoldEpoch`].
#[derive(Clone, Copy, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct PinEpoch(pub(super) HoldId);

/// Holders of one pinned page.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct PinHolder {
    /// Reader reservations covering the page.
    pub count: u32,
    /// The pin reservation that made this page a member.
    pub since: PinEpoch,
    /// The page's interned slot.
    pub slot: PageSlot,
    /// Segment whose pinned-page count includes this holder, if the page is
    /// stored now. A rewrite into another pinned segment moves the count.
    pub counted: Option<SegmentId>,
}

/// One fill's pin of one version.
pub(super) struct Pin {
    /// Wall-clock deadline a client asked for, for reporting only.
    pub until: SystemTime,
    pub monotonic: Instant,
    /// Pages this fill promised to retain.
    pub pages: super::holds::HoldPages,
    /// Holders per member page; the keys are exactly `pages`.
    pub holders: HashMap<PageNumber, PinHolder>,
    /// Live disk index holds of this entry.
    pub ids: Vec<HoldId>,
}

impl Pin {
    /// Whether the pin has not expired. Expiry follows the monotonic clock
    /// only, like a spill hold.
    pub fn live(&self, clock: &racer_io::Clock) -> bool {
        clock.now() < self.monotonic
    }
}

/// Pins are held in a table separate from [`super::holds::Holds`]: they must
/// not consume the spill-hold cap.
pub(super) struct Pins {
    pub entries: HashMap<(FillId, ObjectVersion), Pin>,
    /// Key of each live disk index hold.
    pub owners: HashMap<HoldId, (FillId, ObjectVersion)>,
    /// Member pages across all entries.
    pub members: usize,
    pub segments: Option<Rc<Segments>>,
    /// Stored pinned pages per segment, one per holder entry. A segment loses
    /// its pinned flag when this reaches zero. A zero count whose clear was
    /// refused by a frozen table stays here until a maintenance sweep retries,
    /// so the segment keeps counting against the pin budget meanwhile.
    pub segment_pages: HashMap<SegmentId, usize>,
    pub percent: usize,
}

impl Pins {
    pub fn new(_clock: &racer_io::Clock) -> Self {
        Self {
            entries: HashMap::default(),
            owners: HashMap::default(),
            members: 0,
            segments: None,
            segment_pages: HashMap::default(),
            percent: DEFAULT_PIN_PERCENT,
        }
    }

    pub fn deadline(until: SystemTime, clock: &racer_io::Clock) -> Result<Instant> {
        let ttl = until
            .duration_since(clock.wall_now())
            .map_err(|_| Error::InvalidConfiguration)?;
        if ttl.is_zero() || ttl > MAX_PIN_TTL {
            return Err(Error::InvalidConfiguration);
        }
        clock
            .now()
            .checked_add(ttl)
            .ok_or(Error::InvalidConfiguration)
    }

    /// Note that a holder stops protecting `segment`. The segment becomes
    /// evictable once no holder counts a stored page there.
    fn segment_released(&mut self, segment: SegmentId) {
        let Some(count) = self.segment_pages.get_mut(&segment) else {
            return;
        };
        *count = count.saturating_sub(1);
        if *count != 0 {
            return;
        }
        self.clear_released(segment);
    }

    /// Clear a segment whose last holder went away. A checkpoint freeze makes
    /// [`Segments::clear_pinned`] return `Busy`; keep the zero count then, so
    /// the segment still counts as pinned and [`Self::expire`] retries after
    /// the thaw instead of leaving the table protected forever.
    fn clear_released(&mut self, segment: SegmentId) {
        let Some(segments) = self.segments.clone() else {
            self.segment_pages.remove(&segment);
            return;
        };
        if matches!(segments.clear_pinned(segment), Err(page_alloc::Error::Busy)) {
            return;
        }
        self.segment_pages.remove(&segment);
    }

    /// Retry segment clears a checkpoint freeze deferred.
    fn retry_released(&mut self) {
        let pending: Vec<_> = self
            .segment_pages
            .iter()
            .filter(|(_, count)| **count == 0)
            .map(|(segment, _)| *segment)
            .collect();
        for segment in pending {
            self.clear_released(segment);
        }
    }

    /// Whether a live pin covers `page`, stored or not.
    pub fn is_pinned(&self, page: &PageId, clock: &racer_io::Clock) -> bool {
        self.entries.iter().any(|((_, version), pin)| {
            version == &page.version && pin.live(clock) && pin.pages.contains(page.number)
        })
    }

    /// Latest live pin deadline for `page`, or `None` when it is not pinned.
    pub fn expiry(&self, page: &PageId, clock: &racer_io::Clock) -> Option<SystemTime> {
        self.entries
            .iter()
            .filter(|((_, version), pin)| {
                version == &page.version && pin.live(clock) && pin.pages.contains(page.number)
            })
            .map(|(_, pin)| pin.until)
            .max()
    }

    /// Count a newly stored pinned page for every live holder of it.
    pub(super) fn note_stored(&mut self, page: &PageId, segment: SegmentId) {
        let mut moved = Vec::new();
        for ((_, version), pin) in &mut self.entries {
            if version != &page.version {
                continue;
            }
            let Some(holder) = pin.holders.get_mut(&page.number) else {
                continue;
            };
            if holder.counted != Some(segment) {
                moved.push(holder.counted.replace(segment));
            }
        }
        for previous in moved {
            if let Some(previous) = previous {
                self.segment_released(previous);
            }
            *self.segment_pages.entry(segment).or_insert(0) += 1;
            if let Some(segments) = &self.segments {
                let _ = segments.pin_segment(segment);
            }
        }
    }

    /// Remove one page from one entry, releasing its disk holders.
    pub(super) fn drop_holders(
        &mut self,
        pages: &mut Pages,
        key: &(FillId, ObjectVersion),
        numbers: &[PageNumber],
    ) {
        let Some(pin) = self.entries.get_mut(key) else {
            return;
        };
        let mut released: Vec<(HoldId, PageSlot)> = Vec::new();
        let mut segments = Vec::new();
        for number in numbers {
            if let Some(holder) = pin.holders.remove(number) {
                released.push((holder.since.0, holder.slot));
                if let Some(segment) = holder.counted {
                    segments.push(segment);
                }
            }
        }
        released.sort_unstable();
        let slots: Vec<PageSlot> = released.iter().map(|(_, slot)| *slot).collect();
        let mut gone = Vec::new();
        let mut start = 0;
        while start < released.len() {
            let id = released[start].0;
            let end = start + released[start..].partition_point(|(held, _)| *held == id);
            if !pages.release_hold_slots(id, &slots[start..end]) {
                gone.push(id);
            }
            start = end;
        }
        if !gone.is_empty() {
            pin.ids.retain(|held| gone.binary_search(held).is_err());
        }
        let holders = &pin.holders;
        let before = pin.pages.len();
        pin.pages.retain(|page| holders.contains_key(&page));
        let removed = before - pin.pages.len();
        let empty = pin.pages.is_empty();
        self.members -= removed;
        for id in gone {
            self.owners.remove(&id);
        }
        if empty {
            self.entries.remove(key);
        }
        for segment in segments {
            self.segment_released(segment);
        }
    }

    /// Remove a whole entry and release its disk holds.
    pub(super) fn drop_key(&mut self, pages: &mut Pages, key: &(FillId, ObjectVersion)) {
        let Some(pin) = self.entries.remove(key) else {
            return;
        };
        self.members -= pin.pages.len();
        let segments: Vec<_> = pin
            .holders
            .values()
            .filter_map(|holder| holder.counted)
            .collect();
        for id in pin.ids {
            self.owners.remove(&id);
            pages.release_hold(id);
        }
        for segment in segments {
            self.segment_released(segment);
        }
    }

    /// Remove every entry whose version belongs to one of `caches`.
    pub(super) fn drop_caches(
        &mut self,
        pages: &mut Pages,
        caches: &std::collections::HashSet<racer_wire::control::CacheId>,
    ) {
        let keys: Vec<_> = self
            .entries
            .keys()
            .filter(|(_, version)| caches.contains(&version.object.cache))
            .cloned()
            .collect();
        for key in keys {
            self.drop_key(pages, &key);
        }
    }

    /// Remove every pin. Recovery calls this before installing a cut: pins
    /// are not persisted and must be re-established by the fill controller.
    pub(super) fn clear(&mut self, pages: &mut Pages) {
        let keys: Vec<_> = self.entries.keys().cloned().collect();
        for key in keys {
            self.drop_key(pages, &key);
        }
    }

    /// Release up to `budget` entries due at `now`, reporting each. Also
    /// retries segment clears a checkpoint freeze deferred; those do not use
    /// the expiry budget.
    pub(super) fn expire(
        &mut self,
        pages: &mut Pages,
        clock: &racer_io::Clock,
        budget: usize,
        mut expired: impl FnMut((FillId, ObjectVersion)),
    ) -> usize {
        self.retry_released();
        let due: Vec<_> = self
            .entries
            .iter()
            .filter(|(_, pin)| !pin.live(clock))
            .map(|(key, _)| key.clone())
            .take(budget)
            .collect();
        let count = due.len();
        for key in due {
            self.drop_key(pages, &key);
            expired(key);
        }
        count
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fill_identities_are_copyable_and_ordered() {
        let fill = FillId(7);
        let copy = fill;
        assert_eq!(fill, copy);
        assert!(FillId(1) < FillId(2));
        assert_eq!(PinEpoch::default(), PinEpoch(HoldId::default()));
    }
}
