//! Leader election, coalescing, partial results, failure, cancellation,
//! expiry, caps, and admission for the sans-IO page flight table.

// The attribute below also marks this file as test-only for tests/purity.rs.
#[cfg(test)]
use super::*;
use racer_control_wire::CacheId;
use racer_object_wire::model::{CacheKey, ObjectId, ObjectVersion, PageNumber, StrongEtag};
use std::cell::Cell;
use std::collections::hash_map::RandomState;
use std::rc::Rc;
use std::time::Duration;

#[derive(Clone, Debug, Eq, PartialEq)]
enum Acquired {
    Plain(u32),
    Cipher(u32),
}

fn split(page: Acquired) -> Published<u32, u32> {
    match page {
        Acquired::Plain(v) => Published::Complete(v),
        Acquired::Cipher(v) => Published::Partial(v),
    }
}

fn page(n: u64) -> PageId {
    PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("c".into()),
                key: CacheKey([1; 32]),
            },
            etag: StrongEtag::test_value("v"),
        },
        number: PageNumber(n),
    }
}

const REJECTED: std::result::Result<Acquired, FailureKind> = Err(FailureKind::OriginRejected);
const COMPLETE: fn(u32) -> Wait<u32, u32> = |v| Wait::Ready(Settled::Complete(v));

fn failed(error: Error) -> Result<Wait<u32, u32>> {
    Ok(Wait::Failed(error))
}

fn terminal(error: Error) -> std::result::Result<Acquired, FailureKind> {
    Err(FailureKind::Terminal(error))
}

/// A flight table at `now` with admission `admit`, and its wake list. Times
/// are whole seconds after `now`; pages are numbers.
struct Fx<R> {
    t: PageFlights<u32, u32, Acquired, R, RandomState>,
    now: Instant,
    wakes: Vec<u64>,
    admit: Box<dyn FnMut(Reserve) -> Result<R>>,
}

fn fx() -> Fx<()> {
    limited(FlightLimits::default())
}

fn limited(limits: FlightLimits) -> Fx<()> {
    admitted(limits, Box::new(|_| Ok(())))
}

fn admitted<R>(limits: FlightLimits, admit: Box<dyn FnMut(Reserve) -> Result<R>>) -> Fx<R> {
    let now = Instant::now();
    Fx {
        t: PageFlights::new(limits, split, now, RandomState::new()),
        now,
        wakes: Vec::new(),
        admit,
    }
}

impl<R> Fx<R> {
    fn at(&self, secs: u64) -> Instant {
        self.now + Duration::from_secs(secs)
    }

    fn join_at(
        &mut self,
        p: u64,
        at: u64,
        deadline: u64,
        attempts: u32,
        plain: bool,
    ) -> Result<Joined<u32, u32>> {
        let (at, deadline) = (self.at(at), self.at(deadline));
        let admit = &mut *self.admit;
        let wakes = &mut self.wakes;
        self.t
            .join(&page(p), at, deadline, attempts, plain, admit, wakes)
    }

    fn join(&mut self, p: u64, attempts: u32, plain: bool) -> Result<Joined<u32, u32>> {
        self.join_at(p, 0, 10, attempts, plain)
    }

    fn copy(&mut self, p: u64) -> Result<Joined<u32, u32>> {
        let (at, deadline) = (self.now, self.at(10));
        let admit = &mut *self.admit;
        self.t
            .join_copy(&page(p), at, deadline, admit, &mut self.wakes)
    }

    /// A registered waiter on `p` with deadline `deadline`, joined at `at`.
    fn waiter_at(&mut self, p: u64, at: u64, deadline: u64) -> u64 {
        waiter(self.join_at(p, at, deadline, 2, true))
    }

    fn waiter(&mut self, p: u64) -> u64 {
        self.waiter_at(p, 0, 10)
    }

    fn poll(&mut self, p: u64, id: u64, at: u64, wake: u64) -> Result<Wait<u32, u32>> {
        let at = self.at(at);
        self.t.poll(&page(p), id, at, 2, wake, &mut self.wakes)
    }

    fn finish(
        &mut self,
        p: u64,
        id: u64,
        result: std::result::Result<Acquired, FailureKind>,
    ) -> Result<()> {
        self.t.finish(&page(p), id, result, &mut self.wakes)
    }

    fn refresh(&mut self, p: u64, at: u64) {
        let at = self.at(at);
        self.t.refresh(&page(p), at, &mut self.wakes);
    }

    fn retain(&mut self, p: u64, id: u64) -> Result<()> {
        self.t.retain_operation(&page(p), id, &mut self.wakes)
    }

    fn complete(&mut self, p: u64) -> Result<()> {
        self.t.complete_operation(&page(p), &mut self.wakes)
    }

    fn handover(&mut self, p: u64, id: u64) {
        self.t
            .allow_handover(&page(p), id, &mut self.wakes)
            .unwrap();
    }

    fn cancel(&mut self, p: u64, id: u64) {
        self.t.cancel(&page(p), id, &mut self.wakes);
    }

    fn detach(&mut self, p: u64, id: u64) {
        self.t.detach(&page(p), id, &mut self.wakes);
    }

    /// Clear the wake list, run `f`, and return what it woke.
    fn woken(&mut self, f: impl FnOnce(&mut Self)) -> Vec<u64> {
        self.wakes.clear();
        f(self);
        std::mem::take(&mut self.wakes)
    }
}

fn waiter(joined: Result<Joined<u32, u32>>) -> u64 {
    match joined.unwrap() {
        Joined::Waiter(id) => id,
        other => panic!("{other:?}"),
    }
}

#[test]
fn first_caller_leads_followers_share_completion() {
    let mut f = fx();
    assert!(!f.t.acquiring(&page(0)));
    let (a, b) = (f.waiter(0), f.waiter(0));
    assert_eq!((f.t.len(), f.t.waiters(&page(0))), (1, 2));
    assert_eq!(f.poll(0, a, 0, 10), Ok(Wait::Lead));
    assert!(f.t.acquiring(&page(0)));
    assert!(!f.t.acquiring(&page(1)));
    assert_eq!(f.poll(0, b, 0, 11), Ok(Wait::Pending));
    f.finish(0, a, Ok(Acquired::Plain(7))).unwrap();
    assert!(!f.t.acquiring(&page(0)));
    assert_eq!(f.wakes, [11]);
    assert_eq!(f.poll(0, b, 0, 11), Ok(COMPLETE(7)));
    // Late joiners see the completed result without registering.
    assert_eq!(
        f.join(0, 0, true),
        Ok(Joined::Settled(Settled::Complete(7)))
    );
    f.detach(0, a);
    f.detach(0, b);
    assert!(f.t.is_empty());
}

#[test]
fn partial_result_serves_ciphertext_callers_and_reelects_plaintext() {
    let mut f = fx();
    let (a, b) = (f.waiter(1), f.waiter(1));
    assert_eq!(f.poll(1, a, 0, 1), Ok(Wait::Lead));
    f.finish(1, a, Ok(Acquired::Cipher(3))).unwrap();
    // Ciphertext callers take the partial; with zero attempts a plaintext caller may still join
    // because a partial exists.
    assert_eq!(
        f.join(1, 0, false),
        Ok(Joined::Settled(Settled::Partial(3)))
    );
    let c = waiter(f.join(1, 0, true));
    // Plaintext waiters regain an acquisition opportunity.
    assert_eq!(f.poll(1, a, 0, 1), Ok(Wait::Lead));
    assert_eq!(f.poll(1, b, 0, 2), Ok(Wait::Pending));
    assert_eq!(f.poll(1, c, 0, 3), Ok(Wait::Pending));
    f.finish(1, a, Ok(Acquired::Plain(4))).unwrap();
    assert_eq!(f.poll(1, c, 0, 3), Ok(COMPLETE(4)));
}

#[test]
fn failure_and_retry_semantics() {
    let mut f = fx();
    let (a, b) = (f.waiter(2), f.waiter(2));
    assert_eq!(f.poll(2, a, 0, 1), Ok(Wait::Lead));
    // A rejection retries: the next eligible waiter leads.
    f.finish(2, a, REJECTED).unwrap();
    assert_eq!(f.poll(2, b, 0, 2), Ok(Wait::Lead));
    // A terminal failure fails the cohort.
    f.finish(2, b, terminal(Error::OriginForbidden)).unwrap();
    assert_eq!(f.poll(2, b, 0, 2), failed(Error::OriginForbidden));
    assert_eq!(f.join(2, 2, true), Err(Error::OriginForbidden));
    // Retry with no eligible waiter fails as unavailable.
    let c = f.waiter(3);
    assert_eq!(f.poll(3, c, 0, 1), Ok(Wait::Lead));
    let d = waiter(f.copy(3));
    f.finish(3, c, REJECTED).unwrap();
    let at = f.now;
    assert_eq!(
        f.t.poll(&page(3), d, at, 0, 2, &mut f.wakes),
        failed(Error::Unavailable)
    );
    assert_eq!(f.finish(3, c, REJECTED), Err(Error::Internal));
}

#[test]
fn rejected_leader_fails_and_next_waiter_leads() {
    let mut f = fx();
    let (a, b) = (f.waiter(4), f.waiter(4));
    assert_eq!(f.poll(4, a, 0, 1), Ok(Wait::Lead));
    assert_eq!(f.poll(4, b, 0, 2), Ok(Wait::Pending));
    // Only leader-held flights accept a rejection.
    assert_eq!(f.finish(4, b, REJECTED), Err(Error::Internal));
    let woken = f.woken(|f| f.finish(4, a, REJECTED).unwrap());
    assert!(woken.contains(&2), "{woken:?}");
    assert_eq!(f.poll(4, a, 0, 1), failed(Error::OriginRejected));
    assert_eq!(f.poll(4, b, 0, 2), Ok(Wait::Lead));
    f.finish(4, b, Ok(Acquired::Plain(5))).unwrap();
    // The rejected caller is not served another caller's result.
    assert_eq!(f.poll(4, a, 0, 1), failed(Error::OriginRejected));
    assert_eq!(f.poll(4, b, 0, 2), Ok(COMPLETE(5)));
    // A forbidden sole leader fails with its own error; no waiter remains.
    let c = f.waiter(5);
    assert_eq!(f.poll(5, c, 0, 1), Ok(Wait::Lead));
    f.finish(5, c, Err(FailureKind::OriginForbidden)).unwrap();
    assert_eq!(f.poll(5, c, 0, 1), failed(Error::OriginForbidden));
}

#[test]
fn canceled_leader_is_revoked_and_follower_takes_over() {
    let mut f = fx();
    let (a, b) = (f.waiter(4), f.waiter(4));
    assert_eq!(f.poll(4, a, 0, 1), Ok(Wait::Lead));
    assert_eq!(f.poll(4, b, 0, 2), Ok(Wait::Pending));
    assert!(f.woken(|f| f.cancel(4, a)).contains(&2));
    assert_eq!(f.poll(4, b, 0, 2), Ok(Wait::Lead));
    assert_eq!(f.poll(4, a, 0, 1), failed(Error::Cancelled));
}

#[test]
fn deadlines_expire_waiters_at_explicit_time() {
    let mut f = fx();
    let (a, b) = (f.waiter_at(5, 0, 1), f.waiter_at(5, 0, 5));
    assert_eq!(f.poll(5, a, 0, 1), Ok(Wait::Lead));
    assert_eq!(f.poll(5, b, 0, 2), Ok(Wait::Pending));
    f.refresh(5, 1);
    assert_eq!(f.poll(5, a, 1, 1), failed(Error::DeadlineExceeded));
    assert_eq!(f.poll(5, b, 1, 2), Ok(Wait::Lead));
}

#[test]
fn gates_follow_production_order() {
    let mut f = limited(FlightLimits {
        entries: 1,
        waiters_per_flight: 1,
        generations_per_flight: 1,
        ..FlightLimits::default()
    });
    assert_eq!(f.join_at(6, 10, 10, 2, true), Err(Error::DeadlineExceeded));
    assert_eq!(f.join(6, 0, true), Err(Error::Unavailable));
    f.waiter(6);
    assert_eq!(f.join(6, 2, true), Err(Error::Overloaded));
    assert_eq!(f.join(7, 2, true), Err(Error::Overloaded));
    // One generation: the first election succeeds, a retry cannot elect another waiter and fails
    // the cohort.
    let mut f = limited(FlightLimits {
        generations_per_flight: 1,
        ..FlightLimits::default()
    });
    let (a, b) = (f.waiter(6), f.waiter(6));
    assert_eq!(f.poll(6, a, 0, 1), Ok(Wait::Lead));
    f.finish(6, a, REJECTED).unwrap();
    assert_eq!(f.poll(6, b, 0, 2), failed(Error::Unavailable));
    assert_eq!(f.join(6, 2, true), Err(Error::Unavailable));
}

#[test]
fn budget_gates() {
    let now = Instant::now();
    let later = now + Duration::from_secs(1);
    assert_eq!(
        join_budget_gate(now, now, 1, false),
        Err(Error::DeadlineExceeded)
    );
    assert_eq!(
        join_budget_gate(now, later, 0, false),
        Err(Error::Unavailable)
    );
    assert_eq!(join_budget_gate(now, later, 0, true), Ok(()));
    let limits = FlightLimits::default();
    assert_eq!(Error::from(JoinReject::WaiterCap), Error::Overloaded);
    assert_eq!(Error::from(JoinReject::Failed(Error::Io)), Error::Io);
    assert_eq!(entry_cap_gate(1023, &limits), Ok(()));
    assert_eq!(entry_cap_gate(1024, &limits), Err(Error::Overloaded));
}

/// Waiter and flight counts with a flight cap; charges release on drop.
#[derive(Clone, Default)]
struct Quota(Rc<Cell<(usize, usize, usize)>>);

impl Quota {
    /// A quota of `max_flights` and a fixture admitting through it.
    fn new(max_flights: usize) -> (Self, Fx<Charge>) {
        let quota = Self(Rc::new(Cell::new((0, 0, max_flights))));
        let q = quota.clone();
        let admit = Box::new(move |kind| {
            let (mut waiters, mut flights, max) = q.0.get();
            match kind {
                Reserve::Waiter => waiters += 1,
                Reserve::Flight if flights == max => return Err(Error::Overloaded),
                Reserve::Flight => flights += 1,
            }
            q.0.set((waiters, flights, max));
            Ok(Charge(q.clone(), kind))
        });
        (quota, admitted(FlightLimits::default(), admit))
    }

    fn used(&self) -> (usize, usize) {
        let (waiters, flights, _) = self.0.get();
        (waiters, flights)
    }
}

struct Charge(Quota, Reserve);

impl Drop for Charge {
    fn drop(&mut self) {
        let (mut waiters, mut flights, max) = self.0.0.get();
        match self.1 {
            Reserve::Waiter => waiters -= 1,
            Reserve::Flight => flights -= 1,
        }
        self.0.0.set((waiters, flights, max));
    }
}

#[test]
fn admission_is_released_on_rejection_and_detach() {
    let (quota, mut f) = Quota::new(1);
    let a = f.waiter(8);
    assert_eq!(f.join(9, 2, true), Err(Error::Overloaded));
    assert_eq!(quota.used(), (1, 1));
    f.detach(8, a);
    assert_eq!(quota.used(), (0, 0));
    assert!(f.t.is_empty());
}

/// A leader whose deadline passes while its fill still runs is revoked. With the fill retained as
/// an operation (production `Flights::retain_operation`), the flight keeps draining: the expired
/// leader's detach does not remove it, a new caller waits instead of starting a second concurrent
/// fill, the stale leader's finish is rejected (`Internal`, production `StaleFlight`), and only
/// completing the operation elects the new caller.
#[test]
fn expired_leader_with_retained_operation_blocks_reelection_until_complete() {
    let (quota, mut f) = Quota::new(4);
    let a = f.waiter_at(20, 0, 1);
    assert_eq!(f.poll(20, a, 0, 1), Ok(Wait::Lead));
    f.retain(20, a).unwrap();
    assert!(f.t.is_leader(&page(20), a));
    f.refresh(20, 1);
    assert!(!f.t.is_leader(&page(20), a));
    assert_eq!(f.poll(20, a, 1, 1), failed(Error::DeadlineExceeded));
    f.detach(20, a);
    assert_eq!(quota.used(), (0, 1), "entry kept");
    let b = f.waiter_at(20, 1, 6);
    assert_eq!(f.poll(20, b, 1, 2), Ok(Wait::Pending));
    assert_eq!(
        f.finish(20, a, Ok(Acquired::Plain(1))),
        Err(Error::Internal)
    );
    assert_eq!(f.poll(20, b, 1, 2), Ok(Wait::Pending));
    assert!(f.woken(|f| f.complete(20).unwrap()).contains(&2));
    assert_eq!(f.poll(20, b, 1, 2), Ok(Wait::Lead));
    f.retain(20, b).unwrap();
    f.complete(20).unwrap();
    f.finish(20, b, Ok(Acquired::Plain(2))).unwrap();
    assert_eq!(f.poll(20, b, 1, 2), Ok(COMPLETE(2)));
    f.detach(20, b);
    assert_eq!(quota.used(), (0, 0));
    assert!(f.t.is_empty());
}

/// Without a retained operation the expired leader's detach removes the entry, a new caller leads
/// a second fill, and the first leader's finish is stale.
#[test]
fn expired_leader_without_operation_is_stale_after_new_leader() {
    let mut f = fx();
    let a = f.waiter_at(21, 0, 1);
    assert_eq!(f.poll(21, a, 0, 1), Ok(Wait::Lead));
    f.refresh(21, 1);
    f.detach(21, a);
    assert!(f.t.is_empty());
    let b = f.waiter_at(21, 1, 6);
    assert_eq!(f.poll(21, b, 1, 2), Ok(Wait::Lead));
    assert!(!f.t.is_leader(&page(21), a));
    assert_eq!(
        f.finish(21, a, Ok(Acquired::Plain(1))),
        Err(Error::Internal)
    );
    assert!(f.t.is_leader(&page(21), b));
}

#[test]
fn operation_errors_and_completed_operation_settles_published_result() {
    let mut f = fx();
    assert_eq!(f.retain(22, 0), Err(Error::Internal));
    assert_eq!(f.complete(22), Err(Error::Internal));
    let (a, b) = (f.waiter(22), f.waiter(22));
    // Not elected yet.
    assert_eq!(f.retain(22, a), Err(Error::Internal));
    assert_eq!(f.complete(22), Err(Error::Internal));
    assert_eq!(f.poll(22, a, 0, 1), Ok(Wait::Lead));
    assert_eq!(f.retain(22, b), Err(Error::Internal));
    f.retain(22, a).unwrap();
    // Publication while the operation is still retained stays draining.
    f.finish(22, a, Ok(Acquired::Plain(7))).unwrap();
    assert_eq!(f.poll(22, b, 0, 2), Ok(Wait::Pending));
    assert!(f.woken(|f| f.complete(22).unwrap()).contains(&2));
    assert_eq!(f.poll(22, b, 0, 2), Ok(COMPLETE(7)));
}

/// Production checks a waiter's scope before its recorded error: a rejected leader polled after
/// its deadline reports `DeadlineExceeded`, and a canceled waiter stays `Cancelled`.
#[test]
fn deadline_takes_precedence_over_recorded_rejection() {
    let mut f = fx();
    let (a, b) = (f.waiter_at(23, 0, 1), f.waiter_at(23, 0, 1));
    assert_eq!(f.poll(23, a, 0, 1), Ok(Wait::Lead));
    f.finish(23, a, REJECTED).unwrap();
    f.cancel(23, b);
    assert_eq!(f.poll(23, a, 0, 1), failed(Error::OriginRejected));
    assert_eq!(f.poll(23, a, 1, 1), failed(Error::DeadlineExceeded));
    assert_eq!(f.poll(23, b, 1, 2), failed(Error::Cancelled));
}

/// With handover allowed, an expired leader's running fill continues for the live waiter with the
/// latest deadline. That waiter is not re-elected (no second fill), the elected capability still
/// publishes, and every remaining waiter completes from the one fill.
#[test]
fn expired_leader_hands_running_fill_to_latest_waiter() {
    let (quota, mut f) = Quota::new(4);
    let p = page(30);
    let a = f.waiter_at(30, 0, 1);
    assert_eq!(f.poll(30, a, 0, 1), Ok(Wait::Lead));
    f.retain(30, a).unwrap();
    f.handover(30, a);
    let (b, c) = (f.waiter_at(30, 0, 3), f.waiter_at(30, 0, 2));
    assert_eq!(f.poll(30, b, 0, 2), Ok(Wait::Pending));
    assert_eq!(f.poll(30, c, 0, 3), Ok(Wait::Pending));
    assert_eq!(f.t.leader_deadline(&p), Some(f.at(1)));
    assert!(!f.t.handed_over(&p));
    f.refresh(30, 1);
    assert!(f.t.handed_over(&p));
    assert!(f.t.is_leader(&p, a), "the elected capability survives");
    assert_eq!(
        f.t.leader_deadline(&p),
        Some(f.at(3)),
        "latest deadline adopts"
    );
    assert_eq!(f.poll(30, a, 1, 1), failed(Error::DeadlineExceeded));
    f.detach(30, a);
    assert_eq!(f.poll(30, b, 1, 2), Ok(Wait::Pending));
    assert_eq!(f.poll(30, c, 1, 3), Ok(Wait::Pending));
    f.complete(30).unwrap();
    f.finish(30, a, Ok(Acquired::Plain(7))).unwrap();
    assert_eq!(f.poll(30, b, 1, 2), Ok(COMPLETE(7)));
    assert_eq!(f.poll(30, c, 1, 3), Ok(COMPLETE(7)));
    f.detach(30, b);
    f.detach(30, c);
    assert!(f.t.is_empty());
    assert_eq!(quota.used(), (0, 0));
}

/// A handed-over fill that ends with the elected caller's deadline retries for the adopting
/// waiter, which leads with its own unspent budget; once the last adopter expires the fill is
/// revoked.
#[test]
fn handed_over_fill_deadline_retries_and_last_expiry_revokes() {
    let (_quota, mut f) = Quota::new(4);
    let p = page(31);
    let a = f.waiter_at(31, 0, 1);
    assert_eq!(f.poll(31, a, 0, 1), Ok(Wait::Lead));
    f.retain(31, a).unwrap();
    f.handover(31, a);
    let b = f.waiter_at(31, 0, 3);
    f.refresh(31, 1);
    assert!(f.t.handed_over(&p));
    f.complete(31).unwrap();
    f.finish(31, a, terminal(Error::DeadlineExceeded)).unwrap();
    assert_eq!(f.poll(31, b, 1, 2), Ok(Wait::Lead));
    f.retain(31, b).unwrap();
    f.handover(31, b);
    f.refresh(31, 3);
    assert!(!f.t.is_leader(&p, b), "no waiter left to adopt");
    assert_eq!(f.t.leader_deadline(&p), None);
    f.complete(31).unwrap();
    assert_eq!(f.poll(31, b, 3, 2), failed(Error::DeadlineExceeded));
    f.detach(31, a);
    f.detach(31, b);
    assert!(f.t.is_empty());
}

/// Terminal failures other than the elected caller's own deadline or cancellation still fail the
/// cohort after a handover.
#[test]
fn handover_failure_retries_only_caller_scope_failures() {
    for (handed_over, failure, retry) in [
        (true, FailureKind::Terminal(Error::DeadlineExceeded), true),
        (true, FailureKind::Terminal(Error::Cancelled), true),
        (true, FailureKind::Terminal(Error::Io), false),
        (false, FailureKind::Terminal(Error::DeadlineExceeded), false),
        (false, FailureKind::Terminal(Error::Cancelled), false),
        (false, FailureKind::OriginRejected, true),
    ] {
        let now = Rc::new(Cell::new(Instant::now()));
        let mut state: State<u8, u8, u8, Waiter<()>, Token> = State::default();
        for id in 0..2 {
            let deadline = now.get() + Duration::from_secs(1);
            let now = now.clone();
            let waiter = Waiter {
                deadline,
                now,
                _charge: (),
            };
            state.register(id, waiter, true, true);
        }
        let mut identity = Identity {
            owner: Rc::new(()),
            incarnation: 0,
            generation: 0,
        };
        assert_eq!(state.elect(0, &mut identity, 1), Ok(true));
        if handed_over {
            state.leader = Some(1);
        }
        assert_eq!(state.handed_over(), handed_over);
        fail(
            &mut state,
            0,
            failure,
            true,
            Published::Complete,
            &mut Vec::new(),
        );
        let what = format!("{handed_over} {failure:?}");
        assert_eq!(matches!(state.phase, Phase::RetryPending), retry, "{what}");
    }
    let now = Instant::now();
    let d = now + Duration::from_secs(2);
    assert_eq!(handover_ceiling(now, d, false), d);
    assert_eq!(
        handover_ceiling(now, d, true),
        d + Duration::from_secs(2) * HANDOVER_SPANS
    );
    assert_eq!(handover_ceiling(d, now, true), now, "expired leader");
}

/// A copy-only request joins the running acquisition like production `Flights::join_copy`, waits
/// for its result, and never leads; it misses without a flight or after a failure, takes partial
/// results, respects the waiter cap, and fails when the leader goes away.
#[test]
fn copy_joins() {
    let mut f = fx();
    // No flight: a miss, and no flight is created.
    assert_eq!(f.copy(0), Ok(Joined::Miss));
    assert!(f.t.is_empty());
    let a = f.waiter(0);
    assert_eq!(f.poll(0, a, 0, 10), Ok(Wait::Lead));
    let c = waiter(f.copy(0));
    assert_eq!(f.t.waiters(&page(0)), 2);
    assert_eq!(f.poll(0, c, 0, 11), Ok(Wait::Pending));
    f.finish(0, a, Ok(Acquired::Plain(7))).unwrap();
    assert_eq!(f.wakes, [11]);
    assert_eq!(f.poll(0, c, 0, 11), Ok(COMPLETE(7)));
    // Settled results are returned without registering.
    assert_eq!(f.copy(0), Ok(Joined::Settled(Settled::Complete(7))));
    f.detach(0, a);
    f.detach(0, c);
    assert!(f.t.is_empty());

    let a = f.waiter(1);
    assert_eq!(f.poll(1, a, 0, 1), Ok(Wait::Lead));
    f.finish(1, a, Ok(Acquired::Cipher(3))).unwrap();
    assert_eq!(f.copy(1), Ok(Joined::Settled(Settled::Partial(3))));
    let b = f.waiter(2);
    assert_eq!(f.poll(2, b, 0, 2), Ok(Wait::Lead));
    f.finish(2, b, terminal(Error::Unavailable)).unwrap();
    assert_eq!(f.copy(2), Ok(Joined::Miss));

    let mut f = limited(FlightLimits {
        waiters_per_flight: 2,
        ..FlightLimits::default()
    });
    let a = f.waiter(3);
    assert_eq!(f.poll(3, a, 0, 1), Ok(Wait::Lead));
    let c = waiter(f.copy(3));
    assert_eq!(f.copy(3), Err(Error::Overloaded));
    assert_eq!(f.poll(3, c, 0, 2), Ok(Wait::Pending));
    // The leader goes away; the copy waiter cannot adopt or lead.
    f.t.abandon(&page(3), a, &mut f.wakes);
    f.detach(3, a);
    assert!(matches!(f.poll(3, c, 0, 2), Ok(Wait::Failed(_))));
}
