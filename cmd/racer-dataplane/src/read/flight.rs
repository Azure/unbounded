//! Worker-owned singleflight keyed exactly by page identity, with bounded waiters.
//!
//! Lifecycle contract (operational methods remain fail-closed):
//! - join/wait elects one acquisition caller; other callers independently wait.
//! - publish shares a complete, validated PageResult, never a request context.
//! - origin rejection fails only its supplying caller; another live acquisition
//!   waiter can be elected with its own context and unreset budget. Copy waiters
//!   cannot be elected. Other terminal failures wake all current waiters.
//! - cancellation/drop detaches one caller. Abandoned leadership enters Draining;
//!   only actual I/O/crypto completion fences permit RetryPending or removal.
//! - removal requires no waiters AND no retained operations. A replacement entry
//!   gets a new incarnation; every acquisition/retry gets a new generation.
//!
//! The eventual worker table owns operations, reservations, and completion fences,
//! independently of futures/tokens. Dropping a token must schedule detach/abandon
//! on that worker, never free live resources. Register/recheck wakeups before
//! parking. Bound entries, waiters, retry attempts, and retained completions.
use crate::{
    error::{Error, Operation, Result, deferred, pending},
    memory::page::PageResult,
    model::{context::OriginContext, identity::PageId},
    runtime::{admission::Admission, deadline::RequestScope},
    topology::membership::MembershipLease,
};
use std::{rc::Rc, time::Instant};

pub struct Flights {
    admission: Rc<Admission>,
    owner: Rc<()>,
}

/// Original logical-call budget, lent exclusively to one page acquisition at a
/// time. Retry never reconstructs it from defaults. Fanout must partition credits;
/// route attempts must charge actual forwarded links here as well as on the wire.
/// No Clone: a replacement caller uses its own remaining credits, not fresh ones.
/// ```compile_fail
/// use racer_dataplane::read::flight::AcquisitionBudget;
/// fn duplicate(budget: AcquisitionBudget) { let _other = budget.clone(); }
/// ```
pub struct AcquisitionBudget {
    deadline: Instant,
    attempts: u32,
    links: u8,
}
impl AcquisitionBudget {
    pub fn new(deadline: Instant, attempts: u32, links: u8) -> Self {
        Self {
            deadline,
            attempts,
            links,
        }
    }

    /// Pure budget validation/debit. The driver also checks original cancellation
    /// and candidate authority before I/O. Return the tighter original deadline.
    pub fn begin_attempt(&mut self, now: Instant, caller_deadline: Instant) -> Result<Instant> {
        let deadline = self.deadline.min(caller_deadline);
        if now >= deadline {
            return Err(Error::DeadlineExceeded);
        }
        self.attempts = self.attempts.checked_sub(1).ok_or(Error::Unavailable)?;
        Ok(deadline)
    }

    pub fn charge_links(&mut self, links: u8) -> Result<()> {
        self.links = self
            .links
            .checked_sub(links)
            .ok_or(Error::HopBudgetExhausted)?;
        Ok(())
    }
}

/// Includes table identity to reject tokens from another worker/table, plus entry
/// incarnation and acquisition generation to fence remove/rejoin and retry races.
/// Counters must never wrap; drain/restart the owner before exhaustion.
#[derive(Clone)]
struct Fence {
    owner: Rc<()>,
    page: PageId,
    incarnation: u64,
    generation: u64,
}
impl Fence {
    fn validate(&self, current: &Self) -> Result<()> {
        if !Rc::ptr_eq(&self.owner, &current.owner)
            || self.page != current.page
            || self.incarnation != current.incarnation
            || self.generation != current.generation
        {
            return Err(Error::StaleFlight);
        }
        Ok(())
    }
}

/// Unique publication capability. Resources live in the worker table, not here.
/// Membership/authority are acquisition state, never part of the flight key.
/// ```compile_fail
/// use racer_dataplane::read::flight::FlightLeader;
/// fn duplicate(leader: FlightLeader) { let _other = leader.clone(); }
/// ```
pub struct FlightLeader {
    flights: Rc<Flights>,
    fence: Fence,
    caller: u64,
}

/// Per-request registration; cannot be stored in a completed flight/cache entry.
/// The non-cloneable OriginContext stays with its original request. Only a short
/// borrow is exposed to the elected driver; owned per-attempt sealing/I/O messages
/// retain their own admitted allocations until actual completion. Detach drops
/// these borrows, not another caller's context or outstanding transport owners.
/// ```compile_fail
/// use racer_dataplane::read::flight::AcquisitionWaiter;
/// fn retain(waiter: AcquisitionWaiter<'_>) -> AcquisitionWaiter<'static> { waiter }
/// ```
pub struct AcquisitionWaiter<'a> {
    registration: Registration,
    context: &'a OriginContext,
    scope: &'a RequestScope,
    membership: MembershipLease,
    budget: &'a mut AcquisitionBudget,
}

struct Registration {
    flights: Rc<Flights>,
    // Waiters survive acquisition generations. Match owner/page/incarnation/id
    // for registration, then refresh this generation only on a new election.
    fence: Fence,
    id: u64,
}

/// No credentials, membership, budget, or API to obtain a leader capability.
/// ```compile_fail
/// use racer_dataplane::read::flight::{CopyWaiter, FlightLeader};
/// fn acquire(mut waiter: CopyWaiter<'_>, leader: &FlightLeader) {
///     let _context = waiter.acquisition(leader);
/// }
/// ```
pub struct CopyWaiter<'a> {
    registration: Registration,
    scope: &'a RequestScope,
}

pub enum JoinedFlight<'a> {
    Waiter(AcquisitionWaiter<'a>),
    Complete(PageResult),
}
pub enum JoinedCopy<'a> {
    Miss,
    Waiter(CopyWaiter<'a>),
    Complete(PageResult),
}
pub enum AcquisitionEvent {
    /// Initial election or retry. Only this caller can lend the matching context.
    Lead(FlightLeader),
    Complete(PageResult),
    Failed(Error),
}

/// Borrowed only after checking the leader, registration, page/object association,
/// cancellation, and generation. It does not grant origin permission: Fill must
/// resolve candidates under this membership and obtain a new OriginAuthority.
pub struct AcquisitionContext<'a> {
    pub origin: &'a OriginContext,
    pub scope: &'a RequestScope,
    pub membership: &'a MembershipLease,
    pub budget: &'a mut AcquisitionBudget,
}

pub enum AcquisitionFailure {
    /// A validated credential-specific origin rejection, not generic peer auth
    /// failure, a copy miss, or proof that this version is unavailable.
    OriginRejected,
    /// Terminal only for this cohort; never negative-cache a page/version here.
    Terminal(Error),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FlightState {
    Acquiring,
    RetryPending,
    Draining,
    Complete,
    Failed,
    Removed,
}

/// Non-cloneable observation of retained canceled/abandoned work. Dropping this
/// ticket or its drain future leaves the worker responsible for reaping it.
pub struct DrainTicket {
    flights: Rc<Flights>,
    fence: Fence,
}

impl AcquisitionWaiter<'_> {
    /// Independent deadline/cancellation. Lead is emitted once per election;
    /// retry elects only a still-live caller with remaining original credits.
    /// A rejected caller receives Unauthorized and is ineligible for this cohort.
    pub fn wait(&mut self) -> Operation<'_, AcquisitionEvent> {
        deferred("flight.wait")
    }

    pub fn acquisition<'a>(&'a mut self, _leader: &FlightLeader) -> Result<AcquisitionContext<'a>> {
        pending("flight.acquisition_context")
    }

    /// Detach only this registration. If supplying an active acquisition, cancel
    /// that attempt and drain it before electing another caller. Drop has the same
    /// required effect. A completed entry retains no OriginContext borrows.
    pub fn detach(self) -> Result<FlightState> {
        pending("flight.detach_acquisition")
    }
}
impl CopyWaiter<'_> {
    /// Observe existing work only. Failure/no eligible acquisition callers ends
    /// the wait; it never creates a retry. Results include original ciphertext.
    pub fn wait(&mut self) -> Operation<'_, PageResult> {
        deferred("flight.wait_copy")
    }
    pub fn detach(self) -> Result<FlightState> {
        pending("flight.detach_copy")
    }
}

impl Flights {
    pub fn new(admission: Rc<Admission>) -> Self {
        Self {
            admission,
            owner: Rc::new(()),
        }
    }

    /// Validate context.object against page.version.object before registration.
    /// Admit a bounded independent waiter. Initial election also reserves fill
    /// progress capacity. Context/header differences do not split page identity.
    pub fn join<'a>(
        self: &Rc<Self>,
        page: PageId,
        _membership: MembershipLease,
        context: &'a OriginContext,
        _scope: &'a RequestScope,
        _budget: &'a mut AcquisitionBudget,
    ) -> Result<JoinedFlight<'a>> {
        validate_context(&page, context)?;
        pending("flight.join")
    }

    /// Never creates an entry, elects a leader, or revives failed/draining work.
    /// It may observe retries funded by independently registered acquisition callers.
    pub fn join_copy<'a>(
        self: &Rc<Self>,
        _page: &PageId,
        _scope: &'a RequestScope,
    ) -> Result<JoinedCopy<'a>> {
        pending("flight.join_copy")
    }

    /// Check live leader fence before publication, then validate_for its exact
    /// page. Wake all readers with clones of the complete credential-free bundle.
    /// This cannot update a current-version freshness pointer. Release raw caller
    /// borrows at request completion; owned I/O/crypto state still obeys its fences.
    pub fn publish(&self, _leader: FlightLeader, _page: PageResult) -> Result<FlightState> {
        pending("flight.publish")
    }

    /// Accept only once the worker confirms actual completions are reaped; a
    /// caller's report alone is not a completion fence. OriginRejected
    /// marks only its caller failed and moves to RetryPending if an eligible caller
    /// remains; otherwise fail remaining copy waiters with Unavailable. Terminal
    /// errors fail the cohort. Neither result is cached as evidence about the page.
    /// Stale fences return StaleFlight without mutating the current generation.
    pub fn fail(&self, _leader: FlightLeader, _failure: AcquisitionFailure) -> Result<FlightState> {
        pending("flight.fail")
    }

    /// Acquiring -> Draining. Revoke publication, request cancellation, and retain
    /// all accepted operations/reservations until their actual completions. Token
    /// drop, driver-future drop, and supplying-caller cancellation use this path.
    /// The abandoned supplier becomes ineligible (Cancelled); independent callers
    /// remain registered and can be elected only after the old generation drains.
    pub fn abandon(&self, _leader: FlightLeader) -> Result<DrainTicket> {
        pending("flight.abandon")
    }

    /// Draining -> RetryPending (eligible live acquisition caller), Failed (only
    /// copy waiters remain), or Removed (last waiter gone). Retain the page slot
    /// while draining: a new join cannot start overlapping acquisition. The worker
    /// drives completion accounting even if nobody polls this observation future.
    pub fn finish_draining(&self, _ticket: DrainTicket) -> Operation<'_, FlightState> {
        deferred("flight.finish_draining")
    }

    /// I/O worker lifecycle hook, called even with no remaining request futures.
    /// Process detach/abandon notifications and already-reaped runtime completions,
    /// check their fences, wake/elect live callers, and remove quiescent entries.
    /// Never substitute a timeout or cancellation request for an actual fence.
    pub fn poll_budgeted(&self, _work_budget: usize) -> Result<()> {
        pending("flight.poll_budgeted")
    }

    /// Stop new joins/elections, detach callers, request cancellation, and keep
    /// reaping accepted work. Expiring shutdown scope cannot free live owners.
    pub fn drain<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("flight.drain")
    }
}

fn validate_context(page: &PageId, context: &OriginContext) -> Result<()> {
    if page.version.object != context.object {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::identity::{
        CacheId, CacheKey, ObjectId, ObjectVersion, PageNumber, StrongEtag,
    };
    use std::time::Duration;

    fn fence() -> Fence {
        Fence {
            owner: Rc::new(()),
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId("cache".into()),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value("v1"),
                },
                number: PageNumber(0),
            },
            incarnation: 1,
            generation: 1,
        }
    }

    #[test]
    fn fences_reject_other_tables_pages_recreated_entries_and_late_retries() {
        let current = fence();
        assert_eq!(current.validate(&current.clone()), Ok(()));
        let mut stale = current.clone();
        stale.owner = Rc::new(());
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
        let mut stale = current.clone();
        stale.page.number = PageNumber(1);
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
        let mut stale = current.clone();
        stale.incarnation += 1;
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
        let mut stale = current.clone();
        stale.generation += 1;
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
    }

    #[test]
    fn request_context_must_match_both_cache_and_key_before_join() {
        let page = fence().page;
        let mut context = OriginContext {
            object: page.version.object.clone(),
            metadata: None,
            authorization: None,
        };
        assert_eq!(validate_context(&page, &context), Ok(()));
        context.object.key = CacheKey([1; 32]);
        assert_eq!(
            validate_context(&page, &context),
            Err(Error::InvalidRequest)
        );
        context.object = page.version.object.clone();
        context.object.cache = CacheId("other".into());
        assert_eq!(
            validate_context(&page, &context),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn retry_debits_original_attempts_and_links_without_extending_deadline() {
        let now = Instant::now();
        let original = now + Duration::from_secs(5);
        let later = original + Duration::from_secs(10);
        let mut budget = AcquisitionBudget::new(original, 2, 4);
        assert_eq!(budget.begin_attempt(now, later), Ok(original));
        assert_eq!(budget.charge_links(3), Ok(()));
        assert_eq!(budget.charge_links(2), Err(Error::HopBudgetExhausted));
        assert_eq!(budget.charge_links(1), Ok(()));
        assert_eq!(budget.begin_attempt(now, later), Ok(original));
        assert_eq!(budget.begin_attempt(now, later), Err(Error::Unavailable));
        assert_eq!(budget.charge_links(1), Err(Error::HopBudgetExhausted));
    }

    #[test]
    fn remaining_callers_keep_independent_budgets_and_inclusive_deadlines() {
        let now = Instant::now();
        let deadline = now + Duration::from_secs(5);
        let mut first = AcquisitionBudget::new(deadline, 1, 0);
        let mut remaining = AcquisitionBudget::new(deadline, 1, 4);
        assert_eq!(first.begin_attempt(now, deadline), Ok(deadline));
        assert_eq!(first.begin_attempt(now, deadline), Err(Error::Unavailable));
        assert_eq!(
            remaining.begin_attempt(now, now),
            Err(Error::DeadlineExceeded)
        );
        // Expiry checks and failed charges do not consume unrelated credits.
        assert_eq!(remaining.begin_attempt(now, deadline), Ok(deadline));
        assert_eq!(remaining.charge_links(4), Ok(()));
        assert_eq!(
            first.begin_attempt(deadline, deadline),
            Err(Error::DeadlineExceeded)
        );
    }

    // Compile the ownership/data path rather than asserting Unimplemented errors.
    async fn acquisition_api(
        flights: Rc<Flights>,
        page: PageId,
        membership: MembershipLease,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        result: PageResult,
    ) -> Result<PageResult> {
        match flights.join(page.clone(), membership, context, scope, budget)? {
            JoinedFlight::Complete(result) => Ok(result),
            JoinedFlight::Waiter(mut caller) => match caller.wait().await? {
                AcquisitionEvent::Complete(result) => Ok(result),
                AcquisitionEvent::Failed(error) => Err(error),
                AcquisitionEvent::Lead(leader) => {
                    let acquisition = caller.acquisition(&leader)?;
                    let _: &OriginContext = acquisition.origin;
                    let _: &MembershipLease = acquisition.membership;
                    acquisition
                        .budget
                        .begin_attempt(Instant::now(), acquisition.scope.deadline.0)?;
                    result.validate_for(&page)?;
                    flights.publish(leader, result.clone())?;
                    caller.detach()?;
                    Ok(result)
                }
            },
        }
    }

    async fn failure_and_copy_api(
        flights: Rc<Flights>,
        leader: FlightLeader,
        abandoned: FlightLeader,
        page: PageId,
        scope: &RequestScope,
    ) -> Result<Option<PageResult>> {
        flights.fail(leader, AcquisitionFailure::OriginRejected)?;
        let ticket = flights.abandon(abandoned)?;
        flights.finish_draining(ticket).await?;
        match flights.join_copy(&page, scope)? {
            JoinedCopy::Miss => Ok(None),
            JoinedCopy::Complete(result) => Ok(Some(result)),
            JoinedCopy::Waiter(mut waiter) => {
                let result = waiter.wait().await?;
                waiter.detach()?;
                Ok(Some(result))
            }
        }
    }
}
