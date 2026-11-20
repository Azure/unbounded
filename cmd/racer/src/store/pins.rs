//! Worker-local fill pins. A pin holds pages for a cache fill and, unlike a
//! spill hold, does not count against `RACER_RETAINED_SEGMENT_PERCENT`.
//! Pinned records are written into their own segment class, so a held page
//! does not consume a whole shared segment. Live expiry follows the monotonic
//! clock only; restart drops every pin and recovery clears pinned flags.

use super::pages::Pages;
use crate::error::{Error, Result};
use crate::model::{ObjectVersion, PageId, PageNumber};
use crate::runtime::{HashMap, HashSet};
use page_alloc::SegmentId;
use page_alloc::Segments;
use page_alloc::disk_index::{HoldId, PageSlot};
use std::collections::{BTreeMap, BTreeSet};
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
    pub pages: BTreeSet<PageNumber>,
    /// Holders per member page; the keys are exactly `pages`.
    pub holders: HashMap<PageNumber, PinHolder>,
    /// Live disk index holds of this entry.
    pub ids: HashSet<HoldId>,
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
    /// Ordered live deadlines. Renewal replaces the old deadline rather than
    /// accumulating stale work for a later maintenance turn.
    pub expirations: BTreeMap<Instant, HashSet<(FillId, ObjectVersion)>>,
    /// Only zero-count segments whose clear a checkpoint freeze deferred.
    pub deferred: HashSet<SegmentId>,
    /// Reverse index of member pages: version -> page -> fills whose entry
    /// contains that page. Lookups visit only the fills listed here instead of
    /// scanning every entry. Expired but unswept entries stay indexed until
    /// [`Self::expire`], [`Self::drop_key`], or [`Self::clear`] removes them,
    /// so readers must still check [`Pin::live`].
    pub page_fills: HashMap<ObjectVersion, HashMap<PageNumber, Vec<FillId>>>,
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
            expirations: BTreeMap::new(),
            deferred: HashSet::default(),
            page_fills: HashMap::default(),
            owners: HashMap::default(),
            members: 0,
            segments: None,
            segment_pages: HashMap::default(),
            percent: DEFAULT_PIN_PERCENT,
        }
    }

    pub(super) fn note_deadline(
        &mut self,
        key: &(FillId, ObjectVersion),
        previous: Option<Instant>,
        deadline: Instant,
    ) {
        if previous == Some(deadline) {
            return;
        }
        if let Some(previous) = previous {
            self.forget_deadline(key, previous);
        }
        self.expirations
            .entry(deadline)
            .or_default()
            .insert(key.clone());
    }

    fn forget_deadline(&mut self, key: &(FillId, ObjectVersion), deadline: Instant) {
        if let Some(keys) = self.expirations.get_mut(&deadline) {
            keys.remove(key);
            if keys.is_empty() {
                self.expirations.remove(&deadline);
            }
        }
    }

    /// Index `pages` as member pages of one reservation.
    pub(super) fn note_members(&mut self, fill: FillId, version: &ObjectVersion, pages: &[PageId]) {
        for page in pages {
            let fills = self
                .page_fills
                .entry(version.clone())
                .or_default()
                .entry(page.number)
                .or_default();
            if !fills.contains(&fill) {
                fills.push(fill);
            }
        }
    }

    /// Remove one fill from the reverse index of one page.
    fn forget_member(&mut self, version: &ObjectVersion, number: PageNumber, fill: FillId) {
        let Some(pages) = self.page_fills.get_mut(version) else {
            return;
        };
        let Some(fills) = pages.get_mut(&number) else {
            return;
        };
        fills.retain(|held| *held != fill);
        if fills.is_empty() {
            pages.remove(&number);
        }
        if pages.is_empty() {
            self.page_fills.remove(version);
        }
    }

    /// Fills whose entry contains `page`, live or not.
    fn holders_of<'a>(&'a self, page: &PageId) -> impl Iterator<Item = FillId> + 'a {
        self.page_fills
            .get(&page.version)
            .and_then(|pages| pages.get(&page.number))
            .into_iter()
            .flat_map(|fills| fills.iter().copied())
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
            self.deferred.remove(&segment);
            self.segment_pages.remove(&segment);
            return;
        };
        if matches!(segments.clear_pinned(segment), Err(page_alloc::Error::Busy)) {
            self.deferred.insert(segment);
            return;
        }
        self.deferred.remove(&segment);
        self.segment_pages.remove(&segment);
    }

    /// Retry segment clears a checkpoint freeze deferred.
    fn retry_released(&mut self) {
        if self
            .segments
            .as_ref()
            .is_some_and(|segments| segments.reuse_frozen())
        {
            return;
        }
        let pending: Vec<_> = self.deferred.iter().copied().collect();
        for segment in pending {
            if self.segment_pages.get(&segment) == Some(&0) {
                self.clear_released(segment);
            } else {
                // A new holder may have reclaimed the segment before thaw.
                self.deferred.remove(&segment);
            }
        }
    }

    /// Whether a live pin covers `page`, stored or not.
    pub fn is_pinned(&self, page: &PageId, clock: &racer_io::Clock) -> bool {
        self.holders_of(page).any(|fill| {
            self.entries
                .get(&(fill, page.version.clone()))
                .is_some_and(|pin| pin.live(clock) && pin.pages.contains(&page.number))
        })
    }

    /// Latest live pin deadline for `page`, or `None` when it is not pinned.
    pub fn expiry(&self, page: &PageId, clock: &racer_io::Clock) -> Option<SystemTime> {
        self.holders_of(page)
            .filter_map(|fill| self.entries.get(&(fill, page.version.clone())))
            .filter(|pin| pin.live(clock) && pin.pages.contains(&page.number))
            .map(|pin| pin.until)
            .max()
    }

    /// Count a newly stored pinned page for every live holder of it.
    pub(super) fn note_stored(
        &mut self,
        page: &PageId,
        segment: SegmentId,
        clock: &racer_io::Clock,
    ) {
        let fills: Vec<_> = self.holders_of(page).collect();
        let mut moved = Vec::new();
        for fill in fills {
            let Some(pin) = self.entries.get_mut(&(fill, page.version.clone())) else {
                continue;
            };
            if !pin.live(clock) {
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
                segments
                    .pin_segment(segment)
                    .expect("stored pinned page must reference a live segment");
            }
        }
    }

    /// Invalidation removes stored ownership, not the promise to retain the
    /// page. Every holder must count the replacement when it is published.
    pub(super) fn note_removed(&mut self, page: &PageId, segment: SegmentId) {
        let fills: Vec<_> = self.holders_of(page).collect();
        let mut removed = 0;
        for fill in fills {
            if let Some(holder) = self
                .entries
                .get_mut(&(fill, page.version.clone()))
                .and_then(|pin| pin.holders.get_mut(&page.number))
                .filter(|holder| holder.counted == Some(segment))
            {
                holder.counted = None;
                removed += 1;
            }
        }
        for _ in 0..removed {
            self.segment_released(segment);
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
        let mut dropped = Vec::new();
        let mut segments = Vec::new();
        for number in numbers {
            if let Some(holder) = pin.holders.remove(number) {
                released.push((holder.since.0, holder.slot));
                dropped.push(*number);
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
        for id in &gone {
            pin.ids.remove(id);
        }
        for number in &dropped {
            assert!(pin.pages.remove(number), "holder must be a pin member");
        }
        let empty = pin.pages.is_empty();
        self.members -= dropped.len();
        for id in gone {
            self.owners.remove(&id);
        }
        if empty {
            let pin = self.entries.remove(key).expect("empty pin");
            self.forget_deadline(key, pin.monotonic);
        }
        for number in dropped {
            self.forget_member(&key.1, number, key.0);
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
        self.forget_deadline(key, pin.monotonic);
        self.members -= pin.pages.len();
        for number in pin.pages.iter().copied() {
            self.forget_member(&key.1, number, key.0);
        }
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

    /// Whether maintenance can change any pin or segment state now.
    pub(super) fn maintenance_due(&self, now: Instant, budget: usize) -> bool {
        (budget != 0
            && self
                .expirations
                .first_key_value()
                .is_some_and(|(at, _)| *at <= now))
            || (!self.deferred.is_empty()
                && self
                    .segments
                    .as_ref()
                    .is_none_or(|segments| !segments.reuse_frozen()))
    }

    /// Release up to `budget` due entries, reporting each. Also retry segment
    /// clears a checkpoint freeze deferred; those do not use the expiry budget.
    pub(super) fn expire(
        &mut self,
        pages: &mut Pages,
        clock: &racer_io::Clock,
        budget: usize,
        mut expired: impl FnMut((FillId, ObjectVersion)),
    ) -> usize {
        self.retry_released();
        let now = clock.now();
        let mut count = 0;
        while count < budget {
            let Some((deadline, keys)) = self.expirations.first_key_value() else {
                break;
            };
            if *deadline > now {
                break;
            }
            let key = keys.iter().next().expect("deadline has pins").clone();
            self.drop_key(pages, &key);
            expired(key);
            count += 1;
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
