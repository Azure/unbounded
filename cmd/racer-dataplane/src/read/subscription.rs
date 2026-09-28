//! Compact, bounded node-wide subscription demand and page-credit accounting.
use crate::{
    error::{Error, Result},
    model::{
        identity::{ObjectVersion, PageId, PageNumber},
        range::{PAGE_BYTES, ResolvedRange},
    },
};
use std::{
    collections::{BTreeMap, BTreeSet, VecDeque},
    sync::{Arc, Mutex},
};

struct Demand {
    results: VecDeque<super::fill::PageResult>,
    waker: Option<std::task::Waker>,
    version: ObjectVersion,
    range: ResolvedRange,
    next: u64,
    end: u64,
    selected: BTreeSet<u64>,
    queued: VecDeque<PageNumber>,
    pending: BTreeSet<PageNumber>,
    credits: Credits,
    turn: bool,
}
struct State {
    contracts: BTreeMap<
        (ObjectVersion, u64, crate::model::identity::NodeId),
        (crate::peer::subscriptions::Subscription, std::time::Instant),
    >,
    selecting: BTreeSet<ObjectVersion>,
    sequence: u64,
    demands: BTreeMap<u64, Demand>,
    pending: BTreeMap<PageId, usize>,
    resident: VecDeque<PageId>,
}
pub(crate) struct Scheduler {
    capacity: usize,
    state: Mutex<State>,
}
impl Scheduler {
    pub(crate) fn contract(
        &self,
        version: ObjectVersion,
        membership: crate::model::identity::MembershipVersion,
        provider: crate::model::identity::NodeId,
        demand: crate::peer::subscriptions::Demand,
        deadline: std::time::Instant,
    ) -> Result<(crate::peer::subscriptions::Subscription, std::time::Instant)> {
        let mut state = self.state.lock().unwrap();
        let now = crate::runtime::environment::now();
        state.contracts.retain(|_, (_, deadline)| *deadline > now);
        let key = (version.clone(), membership.0, provider);
        if !state.contracts.contains_key(&key) {
            if state.contracts.len() >= self.capacity.saturating_mul(3) {
                return Err(Error::Overloaded);
            }
            let mut id = [0; 16];
            crate::runtime::environment::fill_random(&mut id).map_err(|_| Error::Unavailable)?;
            state.contracts.insert(
                key.clone(),
                (
                    crate::peer::subscriptions::Subscription {
                        id,
                        version,
                        demand: Default::default(),
                        sequence: 0,
                        page_budget: u32::MAX,
                        byte_budget: u64::from(u32::MAX) * (PAGE_BYTES + 16),
                    },
                    deadline,
                ),
            );
        }
        let (contract, expires) = state.contracts.get_mut(&key).unwrap();
        *expires = (*expires).min(deadline);
        contract.sequence = contract.sequence.checked_add(1).ok_or(Error::Overloaded)?;
        contract.demand = demand;
        let outgoing = contract.clone();
        contract.page_budget = contract
            .page_budget
            .checked_sub(1)
            .ok_or(Error::Unavailable)?;
        contract.byte_budget = contract
            .byte_budget
            .checked_sub(PAGE_BYTES + 16)
            .ok_or(Error::Unavailable)?;
        Ok((outgoing, *expires))
    }
    pub(crate) fn new(capacity: usize) -> Arc<Self> {
        Arc::new(Self {
            capacity,
            state: Mutex::new(State {
                contracts: BTreeMap::new(),
                selecting: BTreeSet::new(),
                sequence: 0,
                demands: BTreeMap::new(),
                pending: BTreeMap::new(),
                resident: VecDeque::new(),
            }),
        })
    }
    pub(crate) fn register(
        self: &Arc<Self>,
        version: ObjectVersion,
        range: ResolvedRange,
        pages: usize,
        bytes: u64,
        ordered: bool,
    ) -> Result<DemandLease> {
        let credits = Credits::new(pages, bytes, ordered)?;
        let mut state = self.state.lock().unwrap();
        if state.demands.len() >= self.capacity {
            return Err(Error::Overloaded);
        }
        state.sequence = state.sequence.checked_add(1).ok_or(Error::Overloaded)?;
        let id = state.sequence;
        state.demands.insert(
            id,
            Demand {
                results: VecDeque::new(),
                waker: None,
                version,
                range,
                next: range.first_page().0,
                end: range.last_page().0 + 1,
                selected: BTreeSet::new(),
                queued: VecDeque::new(),
                pending: BTreeSet::new(),
                credits,
                turn: false,
            },
        );
        Ok(DemandLease {
            scheduler: self.clone(),
            id,
        })
    }
    /// A bounded hint only. The stable page owner still validates every acquisition.
    /// Publication never retains payloads or caller context in the scheduler.
    pub(crate) fn resident(&self, page: PageId) {
        let mut state = self.state.lock().unwrap();
        self.note_resident(&mut state, page);
    }
    fn note_resident(&self, state: &mut State, page: PageId) {
        state.resident.retain(|p| p != &page);
        state.resident.push_back(page);
        while state.resident.len() > self.capacity.saturating_mul(64) {
            state.resident.pop_front();
        }
    }
}
pub(crate) struct DemandLease {
    scheduler: Arc<Scheduler>,
    id: u64,
}

pub(crate) enum Next {
    Page(super::fill::PageResult),
    Select(Selection),
    End,
}

/// Node-wide exclusive ownership of the next receiving transfer for this version.
/// Dropping a stream detaches this selection and wakes other subscribers; all
/// accepted I/O still retains its ordinary runtime completion fences.
pub(crate) struct Selection {
    scheduler: Arc<Scheduler>,
    version: ObjectVersion,
    pub(crate) demand: crate::peer::subscriptions::Demand,
}
impl Selection {
    pub(crate) fn complete(self, page: super::fill::PageResult) -> Result<()> {
        let number = page.plaintext.page().number;
        if page.metadata.version != self.version || !self.demand.contains(number.0) {
            return Err(Error::CorruptRecord);
        }
        page.validate_for(&PageId {
            version: self.version.clone(),
            number,
        })?;
        let mut state = self.scheduler.state.lock().unwrap();
        for demand in state.demands.values_mut() {
            if demand.version != self.version || !demand.eligible(number.0) {
                continue;
            }
            let length = demand
                .range
                .slice_at(number)?
                .ok_or(Error::InvalidRange)?
                .length;
            demand.credits.reserve(number, length)?;
            demand.turn = !demand.turn;
            demand.selected.insert(number.0);
            while demand.selected.remove(&demand.next) {
                demand.next += 1;
            }
            demand.results.push_back(page.clone());
        }
        drop(state);
        Ok(())
    }
}
impl Drop for Selection {
    fn drop(&mut self) {
        let wake = {
            let mut state = self.scheduler.state.lock().unwrap();
            state.selecting.remove(&self.version);
            state
                .demands
                .values_mut()
                .filter(|d| d.version == self.version)
                .filter_map(|d| d.waker.take())
                .collect::<Vec<_>>()
        };
        for waker in wake {
            waker.wake();
        }
    }
}
impl DemandLease {
    pub(crate) fn poll_next(
        &mut self,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Result<Next>> {
        use crate::peer::subscriptions::{Demand as WireDemand, PageInterval};
        use std::task::Poll;
        let mut state = self.scheduler.state.lock().unwrap();
        let demand = state.demands.get_mut(&self.id).ok_or(Error::Cancelled)?;
        if let Some(page) = demand.results.pop_front() {
            return Poll::Ready(Ok(Next::Page(page)));
        }
        if demand.next == demand.end {
            return Poll::Ready(Ok(Next::End));
        }
        demand.waker = Some(cx.waker().clone());
        if !demand.eligible(demand.next) {
            return Poll::Pending;
        }
        let version = demand.version.clone();
        let mut intervals = Vec::new();
        for demand in state
            .demands
            .values()
            .filter(|other| other.version == version && other.eligible(other.next))
        {
            if demand.credits.ordered || !demand.turn || demand.selected.len() >= 63 {
                intervals.push(PageInterval {
                    start: demand.next,
                    end: demand.next + 1,
                });
            } else {
                let mut start = demand.next;
                for hole in &demand.selected {
                    if start < *hole {
                        intervals.push(PageInterval { start, end: *hole });
                    }
                    start = *hole + 1;
                }
                if start < demand.end {
                    intervals.push(PageInterval {
                        start,
                        end: demand.end,
                    });
                }
            }
        }
        intervals.sort_unstable_by_key(|interval| interval.start);
        let mut merged: Vec<PageInterval> = Vec::new();
        for interval in intervals {
            if let Some(last) = merged.last_mut()
                && interval.start <= last.end
            {
                last.end = last.end.max(interval.end);
            } else {
                merged.push(interval);
            }
        }
        // Fragmented aggregates fail explicitly instead of dropping distant demand.
        let demand = WireDemand::new(merged).map_err(|_| Error::Overloaded)?;
        if !state.selecting.insert(version.clone()) {
            return Poll::Pending;
        }
        Poll::Ready(Ok(Next::Select(Selection {
            scheduler: self.scheduler.clone(),
            version,
            demand,
        })))
    }
    /// Select from the entire compact demand, not a consumer sliding window.
    /// At most 64 holes and 64 outstanding assignments per subscriber are retained.
    /// Every other assignment advances its oldest head, even under hot-page load.
    pub(crate) fn select(&mut self) -> Option<PageNumber> {
        let mut state = self.scheduler.state.lock().unwrap();
        if let Some(number) = state.demands.get_mut(&self.id)?.queued.pop_front() {
            return Some(number);
        }
        let demand = state.demands.get(&self.id)?;
        if demand.next == demand.end || !demand.eligible(demand.next) {
            return None;
        }
        let mut number = demand.next;
        if !demand.credits.ordered && demand.turn {
            let mut best = (0, 0, 0);
            // Candidate state is bounded by admitted subscribers, never object size.
            // Hot and in-flight pages may be arbitrarily far from the ordered head.
            for candidate in state
                .resident
                .iter()
                .chain(state.pending.keys())
                .filter(|p| p.version == demand.version)
                .map(|p| p.number.0)
                .chain(
                    state
                        .demands
                        .values()
                        .filter(|d| d.version == demand.version)
                        .map(|d| d.next),
                )
            {
                if !demand.eligible(candidate) {
                    continue;
                }
                let page = PageId {
                    version: demand.version.clone(),
                    number: PageNumber(candidate),
                };
                let shared = state
                    .demands
                    .values()
                    .filter(|other| other.version == demand.version && other.eligible(candidate))
                    .count();
                let score = (
                    usize::from(state.resident.contains(&page)),
                    usize::from(state.pending.contains_key(&page)),
                    shared,
                );
                if score > best {
                    best = score;
                    number = candidate;
                }
            }
        }
        let page = PageId {
            version: demand.version.clone(),
            number: PageNumber(number),
        };
        // Fan out the same assignment to every capacity-bearing eligible subscriber.
        // Each one subsequently enters Fill with its OWN request context and budget.
        let mut assigned = 0;
        for demand in state.demands.values_mut() {
            if demand.version != page.version || !demand.eligible(number) {
                continue;
            }
            let length = demand
                .range
                .slice_at(page.number)
                .expect("eligible assignment has a valid slice")
                .expect("eligible assignment is inside demand")
                .length;
            demand
                .credits
                .reserve(page.number, length)
                .expect("eligibility reserved exact capacity under the same lock");
            demand.turn = !demand.turn;
            demand.selected.insert(number);
            while demand.selected.remove(&demand.next) {
                demand.next += 1;
            }
            demand.pending.insert(page.number);
            demand.queued.push_back(page.number);
            assigned += 1;
        }
        *state.pending.entry(page).or_default() += assigned;
        state.demands.get_mut(&self.id)?.queued.pop_front()
    }
    pub(crate) fn completed(&mut self, number: PageNumber) {
        let mut state = self.scheduler.state.lock().unwrap();
        let Some(demand) = state.demands.get_mut(&self.id) else {
            return;
        };
        if !demand.pending.remove(&number) {
            return;
        }
        let page = PageId {
            version: demand.version.clone(),
            number,
        };
        remove_pending(&mut state, &page);
        self.scheduler.note_resident(&mut state, page);
    }
    pub(crate) fn issued(&mut self, number: PageNumber) -> Result<()> {
        self.scheduler
            .state
            .lock()
            .unwrap()
            .demands
            .get_mut(&self.id)
            .ok_or(Error::Cancelled)?
            .credits
            .issued(number)
    }
    pub(crate) fn release(&mut self, number: PageNumber, length: u32) -> Result<()> {
        self.scheduler
            .state
            .lock()
            .unwrap()
            .demands
            .get_mut(&self.id)
            .ok_or(Error::Cancelled)?
            .credits
            .release(number, length)
    }
    pub(crate) fn ordered(&self) -> bool {
        self.scheduler.state.lock().unwrap().demands[&self.id]
            .credits
            .ordered
    }
    pub(crate) fn exhausted(&self) -> bool {
        let state = self.scheduler.state.lock().unwrap();
        let demand = &state.demands[&self.id];
        demand.next == demand.end && demand.queued.is_empty()
    }
    pub(crate) fn ready_to_select(&self) -> bool {
        let state = self.scheduler.state.lock().unwrap();
        let demand = &state.demands[&self.id];
        !demand.queued.is_empty() || demand.eligible(demand.next)
    }
}
impl Demand {
    fn eligible(&self, number: u64) -> bool {
        if number < self.next
            || number >= self.end
            || self.selected.contains(&number)
            || (number != self.next
                && (self.credits.ordered || !self.turn || self.selected.len() >= 64))
        {
            return false;
        }
        self.range
            .slice_at(PageNumber(number))
            .ok()
            .flatten()
            .is_some_and(|slice| self.credits.can_reserve(slice.length))
    }
}
fn remove_pending(state: &mut State, page: &PageId) {
    if let Some(count) = state.pending.get_mut(page) {
        *count -= 1;
        if *count == 0 {
            state.pending.remove(page);
        }
    }
}
impl Drop for DemandLease {
    fn drop(&mut self) {
        let mut state = self.scheduler.state.lock().unwrap();
        if let Some(demand) = state.demands.remove(&self.id) {
            for number in demand.pending {
                remove_pending(
                    &mut state,
                    &PageId {
                        version: demand.version.clone(),
                        number,
                    },
                );
            }
        }
    }
}

pub(crate) struct Credits {
    pages: usize,
    bytes: u64,
    used: u64,
    outstanding: BTreeMap<PageNumber, (u32, bool)>,
    pub(crate) ordered: bool,
}
impl Credits {
    pub(crate) fn new(pages: usize, bytes: u64, ordered: bool) -> Result<Self> {
        if !(1..=64).contains(&pages) || !(PAGE_BYTES..=64 * PAGE_BYTES).contains(&bytes) {
            return Err(Error::InvalidRequest);
        }
        Ok(Self {
            pages,
            bytes,
            used: 0,
            outstanding: BTreeMap::new(),
            ordered,
        })
    }
    fn can_reserve(&self, length: u32) -> bool {
        self.outstanding.len() < self.pages && u64::from(length) <= self.bytes - self.used
    }
    pub(crate) fn reserve(&mut self, page: PageNumber, length: u32) -> Result<()> {
        if length == 0 || u64::from(length) > PAGE_BYTES {
            return Err(Error::InvalidRequest);
        }
        if !self.can_reserve(length) || self.outstanding.contains_key(&page) {
            return Err(Error::Overloaded);
        }
        self.outstanding.insert(page, (length, false));
        self.used += u64::from(length);
        Ok(())
    }
    pub(crate) fn issued(&mut self, page: PageNumber) -> Result<()> {
        let entry = self
            .outstanding
            .get_mut(&page)
            .ok_or(Error::InvalidRequest)?;
        if entry.1 {
            return Err(Error::InvalidRequest);
        }
        entry.1 = true;
        Ok(())
    }
    pub(crate) fn release(&mut self, page: PageNumber, length: u32) -> Result<()> {
        if self.outstanding.get(&page) != Some(&(length, true)) {
            return Err(Error::InvalidRequest);
        }
        self.outstanding.remove(&page);
        self.used -= u64::from(length);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{
        identity::{CacheId, CacheKey, ObjectId, StrongEtag},
        range::ByteRange,
    };
    fn version() -> ObjectVersion {
        ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([1; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        }
    }

    #[test]
    fn production_aggregate_is_compact_exclusive_and_contracts_never_refill() {
        use crate::{
            model::identity::{MembershipVersion, NodeId},
            peer::subscriptions::PageInterval,
        };
        let scheduler = Scheduler::new(4);
        let range = ByteRange::From(0).resolve(1_000_000 * PAGE_BYTES).unwrap();
        let mut first = scheduler
            .register(version(), range, 1, PAGE_BYTES, false)
            .unwrap();
        let mut second = scheduler
            .register(
                version(),
                ByteRange::From(900_000 * PAGE_BYTES)
                    .resolve(range.end())
                    .unwrap(),
                1,
                PAGE_BYTES,
                false,
            )
            .unwrap();
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let std::task::Poll::Ready(Ok(Next::Select(selection))) = first.poll_next(&mut cx) else {
            panic!()
        };
        assert_eq!(
            selection.demand.intervals(),
            &[
                PageInterval { start: 0, end: 1 },
                PageInterval {
                    start: 900_000,
                    end: 900_001
                }
            ]
        );
        assert!(second.poll_next(&mut cx).is_pending());
        let deadline = crate::runtime::environment::now() + std::time::Duration::from_secs(30);
        let (one, _) = scheduler
            .contract(
                version(),
                MembershipVersion(1),
                NodeId("provider".into()),
                selection.demand.clone(),
                deadline,
            )
            .unwrap();
        let (two, expires) = scheduler
            .contract(
                version(),
                MembershipVersion(1),
                NodeId("provider".into()),
                selection.demand.clone(),
                deadline + std::time::Duration::from_secs(30),
            )
            .unwrap();
        assert_eq!(one.id, two.id);
        assert_eq!(two.sequence, one.sequence + 1);
        assert_eq!(two.page_budget + 1, one.page_budget);
        assert_eq!(two.byte_budget + PAGE_BYTES + 16, one.byte_budget);
        assert_eq!(expires, deadline);
        drop(first);
        assert!(
            second.poll_next(&mut cx).is_pending(),
            "detached consumer cannot release owned work"
        );
        drop(selection);
        assert!(matches!(
            second.poll_next(&mut cx),
            std::task::Poll::Ready(Ok(Next::Select(_)))
        ));
    }
    #[test]
    fn pending_and_delivered_share_exact_once_credit() {
        let mut credits = Credits::new(2, 2 * PAGE_BYTES, false).unwrap();
        credits.reserve(PageNumber(0), PAGE_BYTES as u32).unwrap();
        assert_eq!(
            credits.release(PageNumber(0), PAGE_BYTES as u32),
            Err(Error::InvalidRequest)
        );
        credits.reserve(PageNumber(1), PAGE_BYTES as u32).unwrap();
        assert!(!credits.can_reserve(PAGE_BYTES as u32));
        assert_eq!(credits.reserve(PageNumber(2), 1), Err(Error::Overloaded));
        credits.issued(PageNumber(0)).unwrap();
        assert_eq!(
            credits.release(PageNumber(0), 1),
            Err(Error::InvalidRequest)
        );
        credits.release(PageNumber(0), PAGE_BYTES as u32).unwrap();
        assert_eq!(
            credits.release(PageNumber(0), PAGE_BYTES as u32),
            Err(Error::InvalidRequest)
        );
        assert!(credits.can_reserve(PAGE_BYTES as u32));
    }
    #[test]
    fn scheduler_is_compact_bounded_and_preserves_ordered_head() {
        let scheduler = Scheduler::new(2);
        let range = ByteRange::From(0).resolve(1_000_000 * PAGE_BYTES).unwrap();
        let mut first = scheduler
            .register(version(), range, 2, 2 * PAGE_BYTES, true)
            .unwrap();
        let mut second = scheduler
            .register(version(), range, 2, 2 * PAGE_BYTES, true)
            .unwrap();
        assert!(matches!(
            scheduler.register(version(), range, 2, 2 * PAGE_BYTES, true),
            Err(Error::Overloaded)
        ));
        assert_eq!(first.select(), Some(PageNumber(0)));
        assert_eq!(first.select(), Some(PageNumber(1)));
        first.completed(PageNumber(1));
        assert_eq!(second.select(), Some(PageNumber(0)));
        assert_eq!(second.select(), Some(PageNumber(1)));
        let state = scheduler.state.lock().unwrap();
        assert_eq!(state.demands.len(), 2);
        assert!(
            state
                .demands
                .values()
                .all(|demand| demand.selected.is_empty())
        );
        drop(state);
        drop(first);
        drop(second);
        let state = scheduler.state.lock().unwrap();
        assert!(state.demands.is_empty());
        assert!(state.pending.is_empty());
    }
    #[test]
    fn unordered_ranks_recent_verified_pages_but_alternates_head_progress() {
        let scheduler = Scheduler::new(2);
        let range = ByteRange::From(0).resolve(100 * PAGE_BYTES).unwrap();
        let mut hot = scheduler
            .register(
                version(),
                ByteRange::From(5 * PAGE_BYTES)
                    .resolve(100 * PAGE_BYTES)
                    .unwrap(),
                2,
                2 * PAGE_BYTES,
                true,
            )
            .unwrap();
        assert_eq!(hot.select(), Some(PageNumber(5)));
        hot.completed(PageNumber(5));
        let mut reader = scheduler
            .register(version(), range, 3, 3 * PAGE_BYTES, false)
            .unwrap();
        assert_eq!(reader.select(), Some(PageNumber(0)));
        assert_eq!(reader.select(), Some(PageNumber(5)));
        assert_eq!(reader.select(), Some(PageNumber(1)));
    }

    #[test]
    fn whole_demand_hot_assignment_fans_out_without_slow_reader_backpressure() {
        let scheduler = Scheduler::new(4);
        let range = ByteRange::From(0).resolve(1_000_000 * PAGE_BYTES).unwrap();
        let mut first = scheduler
            .register(version(), range, 2, 2 * PAGE_BYTES, false)
            .unwrap();
        let mut second = scheduler
            .register(version(), range, 2, 2 * PAGE_BYTES, false)
            .unwrap();
        let mut slow = scheduler
            .register(version(), range, 1, PAGE_BYTES, false)
            .unwrap();
        let mut other_version = version();
        other_version.etag = StrongEtag::test_value("v2");
        let other = scheduler
            .register(other_version, range, 2, 2 * PAGE_BYTES, false)
            .unwrap();
        assert_eq!(first.select(), Some(PageNumber(0)));
        scheduler.resident(PageId {
            version: version(),
            number: PageNumber(900_000),
        });
        assert_eq!(first.select(), Some(PageNumber(900_000)));
        assert_eq!(second.select(), Some(PageNumber(0)));
        assert_eq!(second.select(), Some(PageNumber(900_000)));
        assert_eq!(slow.select(), Some(PageNumber(0)));
        assert_eq!(slow.select(), None);
        assert!(!slow.ready_to_select());
        assert!(!slow.exhausted());
        assert!(
            scheduler.state.lock().unwrap().demands[&other.id]
                .queued
                .is_empty()
        );
        first.completed(PageNumber(0));
        first.issued(PageNumber(0)).unwrap();
        first.release(PageNumber(0), PAGE_BYTES as u32).unwrap();
        assert_eq!(
            first.select(),
            Some(PageNumber(1)),
            "head progress despite hot demand"
        );
        assert_eq!(
            slow.select(),
            None,
            "slow subscriber reserves no additional work"
        );
        drop((first, second, slow, other));
        assert!(scheduler.state.lock().unwrap().pending.is_empty());
    }

    #[test]
    fn bounded_holes_and_credit_state_survive_adversarial_hot_pages() {
        let scheduler = Scheduler::new(1);
        let range = ByteRange::From(0).resolve(1_000_000 * PAGE_BYTES).unwrap();
        let mut reader = scheduler
            .register(version(), range, 1, PAGE_BYTES, false)
            .unwrap();
        let mut seen = BTreeSet::new();
        for turn in 0..1000 {
            scheduler.resident(PageId {
                version: version(),
                number: PageNumber(900_000 + turn),
            });
            let number = reader.select().unwrap();
            assert!(seen.insert(number));
            reader.completed(number);
            reader.issued(number).unwrap();
            reader.release(number, PAGE_BYTES as u32).unwrap();
            let state = scheduler.state.lock().unwrap();
            let demand = &state.demands[&reader.id];
            assert!(demand.selected.len() <= 64);
            assert!(demand.pending.is_empty());
            assert!(demand.credits.outstanding.is_empty());
            assert!(state.resident.len() <= 64);
            assert!(state.pending.is_empty());
        }
        assert!(scheduler.state.lock().unwrap().demands[&reader.id].next >= 500);
    }

    #[test]
    fn partial_final_page_uses_exact_bytes_and_completion_does_not_require_release() {
        let scheduler = Scheduler::new(1);
        let range = ByteRange::From(PAGE_BYTES - 3)
            .resolve(PAGE_BYTES + 5)
            .unwrap();
        let mut reader = scheduler
            .register(version(), range, 2, PAGE_BYTES, true)
            .unwrap();
        assert_eq!(reader.select(), Some(PageNumber(0)));
        assert_eq!(reader.select(), Some(PageNumber(1)));
        assert!(reader.exhausted());
        assert_eq!(reader.select(), None);
        assert_eq!(
            scheduler.state.lock().unwrap().demands[&reader.id]
                .credits
                .used,
            8
        );
        reader.completed(PageNumber(0));
        reader.issued(PageNumber(0)).unwrap();
        assert_eq!(reader.release(PageNumber(0), 5), Err(Error::InvalidRequest));
        reader.release(PageNumber(0), 3).unwrap();
        assert_eq!(reader.release(PageNumber(0), 3), Err(Error::InvalidRequest));
        drop(reader);
        assert!(scheduler.state.lock().unwrap().pending.is_empty());
    }

    #[test]
    fn inflight_page_outside_prefix_is_shared_and_drop_preserves_other_subscriber() {
        let scheduler = Scheduler::new(2);
        let length = 1_000_000 * PAGE_BYTES;
        let range = ByteRange::From(0).resolve(length).unwrap();
        let mut supplier = scheduler
            .register(
                version(),
                ByteRange::From(800_000 * PAGE_BYTES)
                    .resolve(length)
                    .unwrap(),
                1,
                PAGE_BYTES,
                false,
            )
            .unwrap();
        assert_eq!(supplier.select(), Some(PageNumber(800_000)));
        let mut reader = scheduler
            .register(version(), range, 2, 2 * PAGE_BYTES, false)
            .unwrap();
        assert_eq!(reader.select(), Some(PageNumber(0)));
        assert_eq!(reader.select(), Some(PageNumber(800_000)));
        let page = PageId {
            version: version(),
            number: PageNumber(800_000),
        };
        assert_eq!(scheduler.state.lock().unwrap().pending[&page], 2);
        drop(supplier);
        assert_eq!(scheduler.state.lock().unwrap().pending[&page], 1);
        reader.completed(page.number);
        reader.issued(page.number).unwrap();
        reader.release(page.number, PAGE_BYTES as u32).unwrap();
        assert_eq!(reader.select(), Some(PageNumber(1)));
    }

    #[test]
    fn invalid_credit_contracts_never_admit_demand() {
        let scheduler = Scheduler::new(1);
        let range = ByteRange::From(0).resolve(1).unwrap();
        for (pages, bytes) in [
            (0, PAGE_BYTES),
            (65, PAGE_BYTES),
            (1, 0),
            (1, 65 * PAGE_BYTES),
        ] {
            assert!(matches!(
                scheduler.register(version(), range, pages, bytes, false),
                Err(Error::InvalidRequest)
            ));
        }
        assert!(scheduler.state.lock().unwrap().demands.is_empty());
        let mut credits = Credits::new(1, PAGE_BYTES, false).unwrap();
        assert_eq!(
            credits.reserve(PageNumber(0), 0),
            Err(Error::InvalidRequest)
        );
        assert_eq!(
            credits.reserve(PageNumber(0), PAGE_BYTES as u32 + 1),
            Err(Error::InvalidRequest)
        );
        assert_eq!(credits.issued(PageNumber(0)), Err(Error::InvalidRequest));
    }
}
