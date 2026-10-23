//! Sans-IO range stream scheduling: which window slots to fill directly,
//! whether to start another provider selection, how many holes it demands,
//! and the per-page acquisition budget.
//!
//! Production `RangeStream` (src/read/range_stream.rs) calls these with its
//! window state; the simulator calls them with its own slot model.

use super::budget::AcquisitionBudget;
use super::error::{Error, Result};
use std::collections::BTreeSet;
use std::ops::Range;
use std::time::{Duration, Instant};

/// Direct fills a read keeps in flight beyond its head page.
pub const DIRECT_AHEAD: usize = 1;

/// Provider selections a read keeps in flight. Beyond the first, a read only
/// starts another while some contract of its version has an unused granted
/// flight. Each selection demands a disjoint share of the selectable holes.
pub const SELECTIONS: usize = 4;

/// Attempts and links each independently admitted page receives.
pub const PAGE_ATTEMPTS: u32 = 8;
pub const PAGE_LINKS: u8 = 16;

/// Scheduling-relevant state of one window slot.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SlotKind {
    Probing,
    /// Not in memory and not requested; `select` is false once a selection
    /// covering the page failed.
    Missing {
        select: bool,
    },
    Fetching,
    Selecting,
    Ready,
    Failed,
}

/// Slots to start direct fills for, in order: the head when it is a hole or
/// only selected, then up to [`DIRECT_AHEAD`] more missing holes that no other
/// window is busy with. Nothing at or after the first failed slot is planned.
/// The driver stops at the first fill the budget refuses.
pub fn direct_fills(slots: &[SlotKind], elsewhere: impl Fn(usize) -> bool) -> Vec<usize> {
    let limit = first_failure(slots).unwrap_or(slots.len());
    let mut plan = Vec::new();
    if limit > 0 && matches!(slots[0], SlotKind::Missing { .. } | SlotKind::Selecting) {
        plan.push(0);
    }
    let mut ahead = slots
        .iter()
        .skip(1)
        .filter(|slot| **slot == SlotKind::Fetching)
        .count();
    let mut index = 1;
    while ahead < DIRECT_AHEAD && index < limit {
        if matches!(slots[index], SlotKind::Missing { .. }) && !elsewhere(index) {
            plan.push(index);
            ahead += 1;
        }
        index += 1;
    }
    plan
}

/// Position of the first failed slot; admission stops there.
pub fn first_failure(slots: &[SlotKind]) -> Option<usize> {
    slots.iter().position(|slot| *slot == SlotKind::Failed)
}

/// Whether a read may start another selection: its first always, more only
/// while a contract of the version has an unused granted flight.
pub fn may_select(selections: usize, spare: usize) -> bool {
    selections < SELECTIONS && (selections == 0 || spare > 0)
}

/// Holes the next selection may demand: an even share of the selectable holes
/// over the selections the read can start now.
pub fn share(holes: usize, selections: usize, spare: usize) -> usize {
    let free = SELECTIONS.saturating_sub(selections);
    // The read's first selection may run before any spare flight exists.
    let startable = if selections == 0 {
        free.min(spare + 1)
    } else {
        free.min(spare)
    };
    holes.div_ceil(startable.max(1)).max(1)
}

/// Up to `limit` selectable holes starting at page `next`, excluding pages
/// another window is busy with, as at most `max_intervals - 1` contiguous
/// page intervals plus the page set.
pub fn selection_demand(
    slots: &[SlotKind],
    next: u64,
    elsewhere: impl Fn(u64) -> bool,
    limit: usize,
    max_intervals: usize,
) -> Option<(Vec<Range<u64>>, BTreeSet<u64>)> {
    let mut intervals: Vec<Range<u64>> = Vec::new();
    let mut pages = BTreeSet::new();
    for (index, slot) in slots.iter().enumerate() {
        if pages.len() >= limit {
            break;
        }
        let number = next + index as u64;
        if *slot != (SlotKind::Missing { select: true }) || elsewhere(number) {
            continue;
        }
        let full = intervals.len() == max_intervals - 1;
        match intervals.last_mut() {
            Some(last) if last.end == number => last.end += 1,
            _ if full => break,
            _ => intervals.push(number..number + 1),
        }
        pages.insert(number);
    }
    (!pages.is_empty()).then_some((intervals, pages))
}

/// How each admitted page of a range is budgeted.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PageBudget {
    /// Client range: every page shares the client's deadline.
    Client { deadline: Instant },
    /// Progress-bounded stream: each page gets `timeout` from its start.
    Progressing { timeout: Duration },
}

impl PageBudget {
    /// Budget for the next independently admitted page at `now`.
    pub fn next_page(&self, now: Instant) -> Result<AcquisitionBudget> {
        match *self {
            Self::Progressing { timeout } => Ok(AcquisitionBudget::new(
                now + timeout,
                PAGE_ATTEMPTS,
                PAGE_LINKS,
            )),
            Self::Client { deadline } => {
                if now >= deadline {
                    return Err(Error::DeadlineExceeded);
                }
                Ok(AcquisitionBudget::new(deadline, PAGE_ATTEMPTS, PAGE_LINKS))
            }
        }
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;
    use SlotKind::*;

    const HOLE: SlotKind = Missing { select: true };

    #[test]
    fn head_and_one_ahead() {
        assert_eq!(direct_fills(&[HOLE, HOLE, HOLE], |_| false), [0, 1]);
        assert_eq!(direct_fills(&[Selecting, HOLE], |_| false), [0, 1]);
        assert_eq!(direct_fills(&[Ready, HOLE, HOLE], |_| false), [1]);
        assert_eq!(direct_fills(&[HOLE, Fetching, HOLE], |_| false), [0]);
        assert_eq!(direct_fills(&[HOLE, HOLE, HOLE], |i| i == 1), [0, 2]);
        assert_eq!(
            direct_fills(&[Probing, Missing { select: false }], |_| false),
            [1]
        );
        assert!(direct_fills(&[], |_| false).is_empty());
    }

    #[test]
    fn failure_stops_admission() {
        assert_eq!(direct_fills(&[HOLE, Failed, HOLE], |_| false), [0]);
        assert!(direct_fills(&[Failed, HOLE], |_| false).is_empty());
        assert_eq!(first_failure(&[Ready, Failed]), Some(1));
    }

    #[test]
    fn selection_counts() {
        assert!(may_select(0, 0));
        assert!(!may_select(1, 0));
        assert!(may_select(1, 1));
        assert!(!may_select(SELECTIONS, 9));
        assert_eq!(share(10, 0, 0), 10);
        assert_eq!(share(10, 0, 9), 3);
        assert_eq!(share(10, 1, 1), 10);
        assert_eq!(share(10, 1, 3), 4);
        assert_eq!(share(0, 0, 0), 1);
        assert_eq!(share(5, 4, 9), 5);
    }

    #[test]
    fn demand_intervals() {
        let slots = [HOLE, HOLE, Fetching, HOLE, Missing { select: false }, HOLE];
        let (intervals, pages) = selection_demand(&slots, 10, |_| false, 99, 64).unwrap();
        assert_eq!(intervals, [10..12, 13..14, 15..16]);
        assert_eq!(pages.into_iter().collect::<Vec<_>>(), [10, 11, 13, 15]);
        let (intervals, _) = selection_demand(&slots, 10, |n| n == 11, 2, 64).unwrap();
        assert_eq!(intervals, [10..11, 13..14]);
        let (intervals, _) = selection_demand(&slots, 10, |_| false, 99, 2).unwrap();
        assert_eq!(intervals.len(), 1);
        assert_eq!(intervals[0], 10..12);
        assert!(selection_demand(&[Ready], 0, |_| false, 9, 64).is_none());
    }

    #[test]
    fn page_budgets() {
        let now = Instant::now();
        let budget = PageBudget::Progressing {
            timeout: Duration::from_secs(3),
        }
        .next_page(now)
        .unwrap();
        assert_eq!(budget.deadline(), now + Duration::from_secs(3));
        assert_eq!(
            (budget.remaining_attempts(), budget.remaining_links()),
            (8, 16)
        );
        let client = PageBudget::Client { deadline: now };
        assert!(matches!(
            client.next_page(now),
            Err(Error::DeadlineExceeded)
        ));
        let client = PageBudget::Client {
            deadline: now + Duration::from_secs(1),
        };
        assert_eq!(
            client.next_page(now).unwrap().deadline(),
            now + Duration::from_secs(1)
        );
    }
}
