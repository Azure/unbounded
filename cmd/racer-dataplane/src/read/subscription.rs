//! Compact, bounded node-wide subscription demand and page-credit accounting.
use crate::{
    error::{Error, Result},
    model::{ObjectVersion, PAGE_BYTES, PageId, PageNumber, ResolvedRange},
};
use flow_control::Window;
use std::{
    collections::{BTreeMap, BTreeSet, VecDeque},
    sync::{Arc, Mutex},
};

struct Demand {
    results: VecDeque<crate::memory::page::PageResult>,
    waker: Option<std::task::Waker>,
    version: ObjectVersion,
    range: ResolvedRange,
    next: u64,
    end: u64,
    selected: BTreeSet<u64>,
    credits: Window<PageNumber>,
    ordered: bool,
    turn: bool,
    gate_ticket: Option<u64>,
}
struct State {
    contracts: BTreeMap<
        (ObjectVersion, u64, crate::model::NodeId),
        (crate::peer::subscriptions::Subscription, std::time::Instant),
    >,
    selecting: BTreeSet<ObjectVersion>,
    fixed: BTreeMap<ObjectVersion, usize>,
    sequence: u64,
    demands: BTreeMap<u64, Demand>,
}
pub(crate) struct Scheduler {
    capacity: usize,
    state: Mutex<State>,
}
impl Scheduler {
    pub(crate) fn contract(
        &self,
        version: ObjectVersion,
        membership: crate::model::MembershipVersion,
        provider: crate::model::NodeId,
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
                fixed: BTreeMap::new(),
                sequence: 0,
                demands: BTreeMap::new(),
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
        if !(1..=64).contains(&pages) || !(PAGE_BYTES..=64 * PAGE_BYTES).contains(&bytes) {
            return Err(Error::InvalidRequest);
        }
        let credits = Window::new(pages, bytes, PAGE_BYTES).map_err(window_error)?;
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
                credits,
                ordered,
                turn: false,
                gate_ticket: None,
            },
        );
        Ok(DemandLease {
            scheduler: self.clone(),
            id,
        })
    }
}
pub(crate) struct DemandLease {
    scheduler: Arc<Scheduler>,
    id: u64,
}

pub(crate) enum Next {
    Page(crate::memory::page::PageResult),
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
    pub(crate) fn complete(self, page: crate::memory::page::PageResult) -> Result<()> {
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
            if demand.ordered || demand.version != self.version || !demand.eligible(number.0) {
                continue;
            }
            let length = demand
                .range
                .slice_at(number)?
                .ok_or(Error::InvalidRange)?
                .length;
            demand
                .credits
                .reserve(number, u64::from(length))
                .map_err(window_error)?;
            demand.turn = !demand.turn;
            demand.selected.insert(number.0);
            while demand.selected.remove(&demand.next) {
                demand.next += 1;
            }
            demand.results.push_back(page.clone());
            demand.gate_ticket = None;
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
    /// Reserve this reader's exact slice before dispatch. Fixed acquisitions may
    /// overlap each other (the stable owner singleflights each page), but not an
    /// unknown provider selection, which bypasses that singleflight.
    pub(crate) fn poll_ordered(
        &mut self,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Result<Option<(PageNumber, FixedAcquisition)>>> {
        use std::task::Poll;
        let mut state = self.scheduler.state.lock().unwrap();
        let demand = state.demands.get_mut(&self.id).ok_or(Error::Cancelled)?;
        demand.waker = Some(cx.waker().clone());
        if demand.next == demand.end {
            return Poll::Ready(Ok(None));
        }
        let version = demand.version.clone();
        if !demand.eligible(demand.next) {
            return Poll::Pending;
        }
        let ticket = state.gate_ticket(self.id)?;
        if state.selecting.contains(&version) || state.earlier_turn(&version, ticket, true) {
            return Poll::Pending;
        }
        let demand = state.demands.get_mut(&self.id).unwrap();
        let number = PageNumber(demand.next);
        let length = demand
            .range
            .slice_at(number)?
            .ok_or(Error::InvalidRange)?
            .length;
        demand
            .credits
            .reserve(number, u64::from(length))
            .map_err(window_error)?;
        demand.gate_ticket = None;
        demand.next += 1;
        *state.fixed.entry(version.clone()).or_default() += 1;
        Poll::Ready(Ok(Some((
            number,
            FixedAcquisition {
                scheduler: self.scheduler.clone(),
                version,
            },
        ))))
    }
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
        let ticket = state.gate_ticket(self.id)?;
        if state.fixed.contains_key(&version) || state.earlier_turn(&version, ticket, false) {
            return Poll::Pending;
        }
        let mut intervals = Vec::new();
        // The ticket belongs to this reader. Including disjoint lower demand
        // lets a provider spend every turn on somebody else's oldest page.
        // Completion still fans out to all eligible overlapping readers.
        {
            let demand = &state.demands[&self.id];
            if !demand.turn
                || demand.selected.len() >= 63
                || !demand.credits.can_reserve(PAGE_BYTES)
            {
                intervals.push(PageInterval {
                    start: demand.next,
                    end: demand.next + 1,
                });
                // With less than a full page of byte credit, only boundary
                // slices can fit. Do not advertise ineligible interior pages.
                let tail = demand.end - 1;
                if demand.turn && tail != demand.next && demand.eligible(tail) {
                    intervals.push(PageInterval {
                        start: tail,
                        end: tail + 1,
                    });
                }
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
        // Fragmented demand fails explicitly instead of dropping distant pages.
        let demand = WireDemand::new(merged).map_err(|_| Error::Overloaded)?;
        if !state.selecting.insert(version.clone()) {
            return Poll::Pending;
        }
        state.demands.get_mut(&self.id).unwrap().gate_ticket = None;
        Poll::Ready(Ok(Next::Select(Selection {
            scheduler: self.scheduler.clone(),
            version,
            demand,
        })))
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
            .map_err(window_error)
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
            .release(number, u64::from(length))
            .map_err(window_error)
    }
    pub(crate) fn ordered(&self) -> bool {
        self.scheduler.state.lock().unwrap().demands[&self.id].ordered
    }
    pub(crate) fn exhausted(&self) -> bool {
        let state = self.scheduler.state.lock().unwrap();
        let demand = &state.demands[&self.id];
        demand.next == demand.end
    }
}
impl State {
    /// One ticket per capacity-bearing polling demand, not per page or poll.
    /// Ordered work batches across earlier ordered tickets, but cannot extend
    /// its batch past a queued selection. Selection yields to all earlier tickets.
    /// Thus either mode can drain the other mode's finite accepted work without
    /// weakening the actual-completion exclusion held by the worker guards.
    fn gate_ticket(&mut self, id: u64) -> Result<u64> {
        let demand = self.demands.get_mut(&id).ok_or(Error::Cancelled)?;
        if let Some(ticket) = demand.gate_ticket {
            return Ok(ticket);
        }
        self.sequence = self.sequence.checked_add(1).ok_or(Error::Overloaded)?;
        demand.gate_ticket = Some(self.sequence);
        Ok(self.sequence)
    }
    fn earlier_turn(&self, version: &ObjectVersion, ticket: u64, ordered: bool) -> bool {
        self.demands.values().any(|demand| {
            &demand.version == version
                && (!ordered || !demand.ordered)
                && demand.gate_ticket.is_some_and(|other| other < ticket)
                && demand.results.is_empty()
                && demand.eligible(demand.next)
        })
    }
}
/// Kept by the worker command, not the client future. Detachment cannot permit
/// provider selection to race accepted fixed-page I/O before actual completion.
pub(crate) struct FixedAcquisition {
    scheduler: Arc<Scheduler>,
    version: ObjectVersion,
}
impl Drop for FixedAcquisition {
    fn drop(&mut self) {
        let wake = {
            let mut state = self.scheduler.state.lock().unwrap();
            let count = state.fixed.get_mut(&self.version).unwrap();
            *count -= 1;
            if *count != 0 {
                return;
            }
            state.fixed.remove(&self.version);
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
impl Demand {
    fn eligible(&self, number: u64) -> bool {
        if number < self.next
            || number >= self.end
            || self.selected.contains(&number)
            || (number != self.next && (self.ordered || !self.turn || self.selected.len() >= 64))
        {
            return false;
        }
        self.range
            .slice_at(PageNumber(number))
            .ok()
            .flatten()
            .is_some_and(|slice| self.credits.can_reserve(u64::from(slice.length)))
    }
}
impl Drop for DemandLease {
    fn drop(&mut self) {
        let mut state = self.scheduler.state.lock().unwrap();
        if let Some(demand) = state.demands.remove(&self.id) {
            // A canceled waiter must not strand the next ticket. Wake outside
            // the lock; accepted work still owns its separate completion guard.
            let wake = state
                .demands
                .values_mut()
                .filter(|other| other.version == demand.version)
                .filter_map(|other| other.waker.take())
                .collect::<Vec<_>>();
            drop(state);
            for waker in wake {
                waker.wake();
            }
        }
    }
}

// Invalid credit transitions are client request errors, not configuration errors.
fn window_error(error: flow_control::Error) -> Error {
    match error {
        flow_control::Error::InvalidInput => Error::InvalidRequest,
        other => other.into(),
    }
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;
    use crate::{
        model::{ByteRange, CacheId, CacheKey, ObjectId, StrongEtag},
        runtime::admission::AdmissionPolicy,
    };
    use flow_control::Quotas;
    use std::task::{Context, Poll};
    fn version() -> ObjectVersion {
        ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([1; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        }
    }

    pub(crate) fn ordered(reader: &mut DemandLease) -> Option<PageNumber> {
        match reader.poll_ordered(&mut Context::from_waker(futures::task::noop_waker_ref())) {
            Poll::Ready(Ok(Some((page, guard)))) => {
                drop(guard);
                Some(page)
            }
            Poll::Ready(Ok(None)) | Poll::Pending => None,
            Poll::Ready(Err(error)) => panic!("ordered assignment: {error:?}"),
        }
    }

    pub(crate) fn selection(reader: &mut DemandLease) -> Selection {
        match reader.poll_next(&mut Context::from_waker(futures::task::noop_waker_ref())) {
            Poll::Ready(Ok(Next::Select(selection))) => selection,
            _ => panic!("expected provider selection"),
        }
    }

    pub(crate) fn selected(reader: &mut DemandLease) -> Option<PageNumber> {
        match reader.poll_next(&mut Context::from_waker(futures::task::noop_waker_ref())) {
            Poll::Ready(Ok(Next::Page(page))) => Some(page.plaintext.page().number),
            Poll::Pending | Poll::Ready(Ok(Next::End)) => None,
            _ => panic!("expected completed provider page"),
        }
    }

    fn complete(selection: Selection, number: u64, length: u64) {
        let metadata = crate::model::ObjectMetadata {
            version: selection.version.clone(),
            length,
            content_type: None,
            expires_at: crate::model::ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        };
        let admission: Quotas<AdmissionPolicy> = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        selection
            .complete(super::super::range_stream::tests::page_result(
                &admission, &metadata, number,
            ))
            .unwrap();
    }

    #[test]
    fn mixed_gate_tickets_bound_turns_under_sustained_ordered_demand() {
        use std::task::Poll;
        let scheduler = Scheduler::new(4);
        let range = ByteRange::From(0).resolve(1_000_000 * PAGE_BYTES).unwrap();
        let mut a = scheduler
            .register(version(), range, 2, 2 * PAGE_BYTES, true)
            .unwrap();
        let mut b = scheduler
            .register(version(), range, 2, 2 * PAGE_BYTES, true)
            .unwrap();
        let mut unordered = scheduler
            .register(version(), range, 1, PAGE_BYTES, false)
            .unwrap();
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..32 {
            let Poll::Ready(Ok(Some((pa, ga)))) = a.poll_ordered(&mut cx) else {
                panic!("ordered resumes")
            };
            let Poll::Ready(Ok(Some((pb, gb)))) = b.poll_ordered(&mut cx) else {
                panic!("concurrent ordered resumes")
            };
            assert!(unordered.poll_next(&mut cx).is_pending());
            let mut newcomer = scheduler
                .register(version(), range, 1, PAGE_BYTES, true)
                .unwrap();
            assert!(
                newcomer.poll_ordered(&mut cx).is_pending(),
                "new readers cannot bypass a waiting selection"
            );
            drop(newcomer);
            // Keep both ordered readers capacity-bearing, and poll them first
            // at every boundary. Neither can refill ahead of the waiting ticket.
            assert!(a.poll_ordered(&mut cx).is_pending());
            assert!(b.poll_ordered(&mut cx).is_pending());
            drop(ga);
            a.issued(pa).unwrap();
            a.release(pa, PAGE_BYTES as u32).unwrap();
            assert!(a.poll_ordered(&mut cx).is_pending());
            assert!(
                unordered.poll_next(&mut cx).is_pending(),
                "last fence still live"
            );
            drop(gb);
            b.issued(pb).unwrap();
            b.release(pb, PAGE_BYTES as u32).unwrap();
            for _ in 0..8 {
                assert!(a.poll_ordered(&mut cx).is_pending());
                assert!(b.poll_ordered(&mut cx).is_pending());
            }
            let Poll::Ready(Ok(Next::Select(selection))) = unordered.poll_next(&mut cx) else {
                panic!("selection gets the very next turn after two fences")
            };
            assert!(a.poll_ordered(&mut cx).is_pending());
            // Simulate an unsuccessful selection. Even immediate retries cannot
            // starve the ordered tickets queued during the preceding batch.
            drop(selection);
            assert!(unordered.poll_next(&mut cx).is_pending());
        }
        drop((a, b, unordered));
        let state = scheduler.state.lock().unwrap();
        assert!(state.fixed.is_empty());
        assert!(state.selecting.is_empty());
        assert!(state.demands.is_empty());
    }

    #[test]
    fn canceled_gate_waiter_wakes_successor_without_releasing_live_fences() {
        use std::{
            sync::atomic::{AtomicUsize, Ordering},
            task::Poll,
        };
        #[derive(Default)]
        struct Wakes(AtomicUsize);
        impl futures::task::ArcWake for Wakes {
            fn wake_by_ref(this: &Arc<Self>) {
                this.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        let wakes = Arc::new(Wakes::default());
        let waker = futures::task::waker(wakes.clone());
        let mut cx = std::task::Context::from_waker(&waker);
        let scheduler = Scheduler::new(3);
        let range = ByteRange::From(0).resolve(100 * PAGE_BYTES).unwrap();
        let mut ordered = scheduler
            .register(version(), range, 4, 4 * PAGE_BYTES, true)
            .unwrap();
        let mut canceled = scheduler
            .register(version(), range, 1, PAGE_BYTES, false)
            .unwrap();
        let Poll::Ready(Ok(Some((_, first)))) = ordered.poll_ordered(&mut cx) else {
            panic!()
        };
        assert!(canceled.poll_next(&mut cx).is_pending());
        assert!(ordered.poll_ordered(&mut cx).is_pending());
        let before = wakes.0.load(Ordering::Relaxed);
        drop(canceled);
        assert!(wakes.0.load(Ordering::Relaxed) > before);
        let Poll::Ready(Ok(Some((_, second)))) = ordered.poll_ordered(&mut cx) else {
            panic!("canceled selection no longer blocks refill")
        };
        let mut successor = scheduler
            .register(version(), range, 1, PAGE_BYTES, false)
            .unwrap();
        assert!(successor.poll_next(&mut cx).is_pending());
        assert!(ordered.poll_ordered(&mut cx).is_pending());
        drop(ordered);
        assert!(successor.poll_next(&mut cx).is_pending());
        drop(first);
        assert!(successor.poll_next(&mut cx).is_pending());
        let before = wakes.0.load(Ordering::Relaxed);
        drop(second);
        assert!(wakes.0.load(Ordering::Relaxed) > before);
        assert!(matches!(
            successor.poll_next(&mut cx),
            Poll::Ready(Ok(Next::Select(_)))
        ));
        drop(successor);
        let state = scheduler.state.lock().unwrap();
        assert!(state.demands.is_empty());
        assert!(state.fixed.is_empty());
        assert!(state.selecting.is_empty());
    }

    #[test]
    fn ordered_reservations_are_exact_and_mixed_exclusion_outlives_demand() {
        let scheduler = Scheduler::new(3);
        let range = ByteRange::From(PAGE_BYTES - 3)
            .resolve(2 * PAGE_BYTES + 5)
            .unwrap();
        let mut ordered = scheduler
            .register(version(), range, 2, PAGE_BYTES, true)
            .unwrap();
        let mut unordered = scheduler
            .register(version(), range, 1, PAGE_BYTES, false)
            .unwrap();
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let std::task::Poll::Ready(Ok(Next::Select(selection))) = unordered.poll_next(&mut cx)
        else {
            panic!()
        };
        assert!(ordered.poll_ordered(&mut cx).is_pending());
        drop(selection);
        let std::task::Poll::Ready(Ok(Some((number, guard)))) = ordered.poll_ordered(&mut cx)
        else {
            panic!()
        };
        assert_eq!(number, PageNumber(0));
        {
            let state = scheduler.state.lock().unwrap();
            let credits = &state.demands[&ordered.id].credits;
            assert!(credits.can_reserve(PAGE_BYTES - 3));
            assert!(!credits.can_reserve(PAGE_BYTES - 2));
        }
        assert!(
            ordered.poll_ordered(&mut cx).is_pending(),
            "byte credit reserved before work"
        );
        assert!(unordered.poll_next(&mut cx).is_pending());
        drop(ordered);
        assert!(
            unordered.poll_next(&mut cx).is_pending(),
            "worker still owns exclusion"
        );
        drop(guard);
        assert!(matches!(
            unordered.poll_next(&mut cx),
            std::task::Poll::Ready(Ok(Next::Select(_)))
        ));
        drop(unordered);
        let state = scheduler.state.lock().unwrap();
        assert!(state.fixed.is_empty());
        assert!(state.selecting.is_empty());
        assert!(state.demands.is_empty());
    }

    #[test]
    fn production_aggregate_is_compact_exclusive_and_contracts_never_refill() {
        use crate::{
            model::{MembershipVersion, NodeId},
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
            &[PageInterval { start: 0, end: 1 }]
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
    fn production_ticket_cannot_be_spent_on_rotating_disjoint_low_readers() {
        use std::task::Poll;
        let scheduler = Scheduler::new(3);
        let length = 1000 * PAGE_BYTES;
        let mut high = scheduler
            .register(
                version(),
                ByteRange::From(900 * PAGE_BYTES).resolve(length).unwrap(),
                1,
                PAGE_BYTES,
                false,
            )
            .unwrap();
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        for low_page in 0..32 {
            let mut low = scheduler
                .register(
                    version(),
                    ByteRange::Closed {
                        first: low_page * PAGE_BYTES,
                        last: (low_page + 1) * PAGE_BYTES - 1,
                    }
                    .resolve(length)
                    .unwrap(),
                    1,
                    PAGE_BYTES,
                    false,
                )
                .unwrap();
            let Poll::Ready(Ok(Next::Select(selection))) = low.poll_next(&mut cx) else {
                panic!()
            };
            assert!(high.poll_next(&mut cx).is_pending());
            drop(selection);
            assert!(low.poll_next(&mut cx).is_pending());
            let Poll::Ready(Ok(Next::Select(selection))) = high.poll_next(&mut cx) else {
                panic!("oldest ticket must run")
            };
            assert_eq!(
                selection.demand.intervals(),
                &[crate::peer::subscriptions::PageInterval {
                    start: 900,
                    end: 901
                }]
            );
            assert!(!selection.demand.contains(low_page));
            drop((selection, low));
        }
    }

    #[test]
    fn production_selection_advertises_only_credit_eligible_slices() {
        let scheduler = Scheduler::new(1);
        let range = ByteRange::From(PAGE_BYTES - 1)
            .resolve(3 * PAGE_BYTES + 2)
            .unwrap();
        let mut reader = scheduler
            .register(version(), range, 2, PAGE_BYTES, false)
            .unwrap();
        {
            let mut state = scheduler.state.lock().unwrap();
            let demand = state.demands.get_mut(&reader.id).unwrap();
            demand.turn = true;
            demand
                .credits
                .reserve(PageNumber(99), PAGE_BYTES - 2)
                .unwrap();
        }
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let std::task::Poll::Ready(Ok(Next::Select(selection))) = reader.poll_next(&mut cx) else {
            panic!()
        };
        assert!(selection.demand.contains(0));
        assert!(selection.demand.contains(3));
        assert!(!selection.demand.contains(1));
        assert!(!selection.demand.contains(2));
    }

    #[test]
    fn pending_and_delivered_share_exact_once_credit() {
        let mut credits = Window::new(2, 2 * PAGE_BYTES, PAGE_BYTES).unwrap();
        credits.reserve(PageNumber(0), PAGE_BYTES).unwrap();
        assert_eq!(
            credits.release(PageNumber(0), PAGE_BYTES),
            Err(flow_control::Error::InvalidInput)
        );
        credits.reserve(PageNumber(1), PAGE_BYTES).unwrap();
        assert!(!credits.can_reserve(PAGE_BYTES));
        assert_eq!(
            credits.reserve(PageNumber(2), 1),
            Err(flow_control::Error::Overloaded)
        );
        credits.issued(PageNumber(0)).unwrap();
        assert_eq!(
            credits.release(PageNumber(0), 1),
            Err(flow_control::Error::InvalidInput)
        );
        credits.release(PageNumber(0), PAGE_BYTES).unwrap();
        assert_eq!(
            credits.release(PageNumber(0), PAGE_BYTES),
            Err(flow_control::Error::InvalidInput)
        );
        assert!(credits.can_reserve(PAGE_BYTES));
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
        assert_eq!(ordered(&mut first), Some(PageNumber(0)));
        assert_eq!(ordered(&mut first), Some(PageNumber(1)));
        assert_eq!(ordered(&mut second), Some(PageNumber(0)));
        assert_eq!(ordered(&mut second), Some(PageNumber(1)));
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
        assert!(state.fixed.is_empty());
    }
    #[test]
    fn unordered_provider_can_select_distant_pages_but_alternates_head_progress() {
        let scheduler = Scheduler::new(2);
        let range = ByteRange::From(0).resolve(100 * PAGE_BYTES).unwrap();
        let mut reader = scheduler
            .register(version(), range, 3, 3 * PAGE_BYTES, false)
            .unwrap();
        for number in [0, 5, 1] {
            let choice = selection(&mut reader);
            assert!(choice.demand.contains(number));
            if number != 5 {
                assert_eq!(choice.demand.page_count(), 1);
            }
            complete(choice, number, range.end());
            assert_eq!(selected(&mut reader), Some(PageNumber(number)));
        }
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
        complete(selection(&mut first), 0, range.end());
        assert_eq!(selected(&mut first), Some(PageNumber(0)));
        complete(selection(&mut first), 900_000, range.end());
        assert_eq!(selected(&mut first), Some(PageNumber(900_000)));
        assert_eq!(selected(&mut second), Some(PageNumber(0)));
        assert_eq!(selected(&mut second), Some(PageNumber(900_000)));
        assert_eq!(selected(&mut slow), Some(PageNumber(0)));
        assert_eq!(selected(&mut slow), None);
        assert!(!slow.exhausted());
        assert!(
            scheduler.state.lock().unwrap().demands[&other.id]
                .results
                .is_empty()
        );
        first.issued(PageNumber(0)).unwrap();
        first.release(PageNumber(0), PAGE_BYTES as u32).unwrap();
        let choice = selection(&mut first);
        assert_eq!(
            choice.demand.intervals(),
            &[crate::peer::subscriptions::PageInterval { start: 1, end: 2 }]
        );
        complete(choice, 1, range.end());
        assert_eq!(selected(&mut first), Some(PageNumber(1)));
        assert_eq!(
            selected(&mut slow),
            None,
            "slow subscriber reserves no additional work"
        );
        drop((first, second, slow, other));
        assert!(scheduler.state.lock().unwrap().demands.is_empty());
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
            let choice = selection(&mut reader);
            let hot = 900_000 + turn;
            let number = if choice.demand.contains(hot) {
                hot
            } else {
                choice.demand.intervals()[0].start
            };
            complete(choice, number, range.end());
            let number = selected(&mut reader).unwrap();
            assert!(seen.insert(number));
            reader.issued(number).unwrap();
            reader.release(number, PAGE_BYTES as u32).unwrap();
            let state = scheduler.state.lock().unwrap();
            let demand = &state.demands[&reader.id];
            assert!(demand.selected.len() <= 64);
            assert!(demand.results.is_empty());
            assert!(demand.credits.is_empty());
            assert!(state.selecting.is_empty());
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
        assert_eq!(ordered(&mut reader), Some(PageNumber(0)));
        assert_eq!(ordered(&mut reader), Some(PageNumber(1)));
        assert!(reader.exhausted());
        assert_eq!(ordered(&mut reader), None);
        assert!(
            !scheduler.state.lock().unwrap().demands[&reader.id]
                .credits
                .is_empty()
        );
        reader.issued(PageNumber(0)).unwrap();
        assert_eq!(reader.release(PageNumber(0), 5), Err(Error::InvalidRequest));
        reader.release(PageNumber(0), 3).unwrap();
        assert_eq!(reader.release(PageNumber(0), 3), Err(Error::InvalidRequest));
        reader.issued(PageNumber(1)).unwrap();
        assert_eq!(reader.release(PageNumber(1), 3), Err(Error::InvalidRequest));
        reader.release(PageNumber(1), 5).unwrap();
        assert!(
            scheduler.state.lock().unwrap().demands[&reader.id]
                .credits
                .is_empty()
        );
        drop(reader);
        assert!(scheduler.state.lock().unwrap().demands.is_empty());
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
        let pending = selection(&mut supplier);
        let mut reader = scheduler
            .register(version(), range, 2, 2 * PAGE_BYTES, false)
            .unwrap();
        assert_eq!(selected(&mut reader), None);
        complete(pending, 800_000, length);
        assert_eq!(selected(&mut supplier), Some(PageNumber(800_000)));
        complete(selection(&mut reader), 0, length);
        assert_eq!(selected(&mut reader), Some(PageNumber(0)));
        complete(selection(&mut reader), 800_000, length);
        assert_eq!(selected(&mut reader), Some(PageNumber(800_000)));
        let page = PageId {
            version: version(),
            number: PageNumber(800_000),
        };
        assert_eq!(scheduler.state.lock().unwrap().demands.len(), 2);
        drop(supplier);
        assert_eq!(scheduler.state.lock().unwrap().demands.len(), 1);
        reader.issued(page.number).unwrap();
        reader.release(page.number, PAGE_BYTES as u32).unwrap();
        complete(selection(&mut reader), 1, length);
        assert_eq!(selected(&mut reader), Some(PageNumber(1)));
    }

    #[test]
    fn credit_boundaries_and_invalid_transitions_preserve_request_errors() {
        let scheduler = Scheduler::new(1);
        let range = ByteRange::From(0).resolve(PAGE_BYTES).unwrap();
        for (pages, bytes) in [(1, PAGE_BYTES), (64, 64 * PAGE_BYTES)] {
            let mut reader = scheduler
                .register(version(), range, pages, bytes, true)
                .unwrap();
            assert!(reader.ordered());
            assert_eq!(reader.issued(PageNumber(0)), Err(Error::InvalidRequest));
            assert_eq!(ordered(&mut reader), Some(PageNumber(0)));
            assert_eq!(
                reader.release(PageNumber(0), PAGE_BYTES as u32),
                Err(Error::InvalidRequest)
            );
            reader.issued(PageNumber(0)).unwrap();
            assert_eq!(reader.issued(PageNumber(0)), Err(Error::InvalidRequest));
            reader.release(PageNumber(0), PAGE_BYTES as u32).unwrap();
            assert_eq!(
                reader.release(PageNumber(0), PAGE_BYTES as u32),
                Err(Error::InvalidRequest)
            );
        }
    }

    #[test]
    fn invalid_credit_contracts_never_admit_demand() {
        let scheduler = Scheduler::new(1);
        let range = ByteRange::From(0).resolve(1).unwrap();
        for (pages, bytes) in [
            (0, PAGE_BYTES),
            (65, PAGE_BYTES),
            (1, 0),
            (1, PAGE_BYTES - 1),
            (1, 64 * PAGE_BYTES + 1),
            (1, 65 * PAGE_BYTES),
        ] {
            assert!(matches!(
                scheduler.register(version(), range, pages, bytes, false),
                Err(Error::InvalidRequest)
            ));
        }
        assert!(scheduler.state.lock().unwrap().demands.is_empty());
        let mut credits = Window::new(1, PAGE_BYTES, PAGE_BYTES).unwrap();
        assert_eq!(
            credits.reserve(PageNumber(0), 0),
            Err(flow_control::Error::InvalidInput)
        );
        assert_eq!(
            credits.reserve(PageNumber(0), PAGE_BYTES + 1),
            Err(flow_control::Error::InvalidInput)
        );
        assert_eq!(
            credits.issued(PageNumber(0)),
            Err(flow_control::Error::InvalidInput)
        );
    }
}
