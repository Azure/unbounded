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

type Table<R = ()> = PageFlights<u32, u32, Acquired, R, RandomState>;

fn table<R>(limits: FlightLimits, now: Instant) -> Table<R> {
    PageFlights::new(limits, split, now, RandomState::new())
}

/// Admission that never rejects.
fn free(_: Reserve) -> Result<()> {
    Ok(())
}

fn published(v: Acquired) -> std::result::Result<Acquired, FailureKind> {
    Ok(v)
}

fn waiter(joined: Result<Joined<u32, u32>>) -> u64 {
    match joined.unwrap() {
        Joined::Waiter(id) => id,
        other => panic!("{other:?}"),
    }
}

#[test]
fn first_caller_leads_followers_share_completion() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(0);
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.len(), 1);
    assert_eq!(t.waiters(&p), 2);
    assert_eq!(t.poll(&p, a, now, 2, 10, &mut wakes), Ok(Wait::Lead));
    assert_eq!(t.poll(&p, b, now, 2, 11, &mut wakes), Ok(Wait::Pending));
    t.finish(&p, a, published(Acquired::Plain(7)), &mut wakes)
        .unwrap();
    assert_eq!(wakes, [11]);
    assert_eq!(
        t.poll(&p, b, now, 2, 11, &mut wakes),
        Ok(Wait::Ready(Settled::Complete(7)))
    );
    // Late joiners see the completed result without registering.
    assert_eq!(
        t.join(&p, now, deadline, 0, true, free, &mut wakes),
        Ok(Joined::Settled(Settled::Complete(7)))
    );
    t.detach(&p, a, &mut wakes);
    t.detach(&p, b, &mut wakes);
    assert!(t.is_empty());
}

#[test]
fn acquiring_only_while_a_leader_fetches() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(0);
    assert!(!t.acquiring(&p));
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 10, &mut wakes), Ok(Wait::Lead));
    assert!(t.acquiring(&p));
    assert!(!t.acquiring(&page(1)));
    t.finish(&p, a, published(Acquired::Plain(7)), &mut wakes)
        .unwrap();
    assert!(!t.acquiring(&p));
}

#[test]
fn partial_result_serves_ciphertext_callers_and_reelects_plaintext() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(1);
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    t.finish(&p, a, published(Acquired::Cipher(3)), &mut wakes)
        .unwrap();
    // Ciphertext callers take the partial; with zero attempts a plaintext
    // caller may still join because a partial exists.
    assert_eq!(
        t.join(&p, now, deadline, 0, false, free, &mut wakes),
        Ok(Joined::Settled(Settled::Partial(3)))
    );
    let c = waiter(t.join(&p, now, deadline, 0, true, free, &mut wakes));
    // Plaintext waiters regain an acquisition opportunity.
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    assert_eq!(t.poll(&p, b, now, 2, 2, &mut wakes), Ok(Wait::Pending));
    assert_eq!(t.poll(&p, c, now, 2, 3, &mut wakes), Ok(Wait::Pending));
    t.finish(&p, a, published(Acquired::Plain(4)), &mut wakes)
        .unwrap();
    assert_eq!(
        t.poll(&p, c, now, 2, 3, &mut wakes),
        Ok(Wait::Ready(Settled::Complete(4)))
    );
}

#[test]
fn failure_and_retry_semantics() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(2);
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    // A rejection retries: the next eligible waiter leads.
    t.finish(&p, a, Err(FailureKind::OriginRejected), &mut wakes)
        .unwrap();
    assert_eq!(t.poll(&p, b, now, 2, 2, &mut wakes), Ok(Wait::Lead));
    // A terminal failure fails the cohort.
    t.finish(
        &p,
        b,
        Err(FailureKind::Terminal(Error::OriginForbidden)),
        &mut wakes,
    )
    .unwrap();
    assert_eq!(
        t.poll(&p, b, now, 2, 2, &mut wakes),
        Ok(Wait::Failed(Error::OriginForbidden))
    );
    assert_eq!(
        t.join(&p, now, deadline, 2, true, free, &mut wakes),
        Err(Error::OriginForbidden)
    );
    // Retry with no eligible waiter fails as unavailable.
    let q = page(3);
    let c = waiter(t.join(&q, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&q, c, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    let d = waiter(t.join_copy(&q, now, deadline, free, &mut wakes));
    t.finish(&q, c, Err(FailureKind::OriginRejected), &mut wakes)
        .unwrap();
    assert_eq!(
        t.poll(&q, d, now, 0, 2, &mut wakes),
        Ok(Wait::Failed(Error::Unavailable))
    );
    assert_eq!(
        t.finish(&q, c, Err(FailureKind::OriginRejected), &mut wakes),
        Err(Error::Internal)
    );
}

#[test]
fn rejected_leader_fails_and_next_waiter_leads() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(4);
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    assert_eq!(t.poll(&p, b, now, 2, 2, &mut wakes), Ok(Wait::Pending));
    // Only leader-held flights accept a rejection.
    assert_eq!(
        t.finish(&p, b, Err(FailureKind::OriginRejected), &mut wakes),
        Err(Error::Internal)
    );
    wakes.clear();
    t.finish(&p, a, Err(FailureKind::OriginRejected), &mut wakes)
        .unwrap();
    assert!(wakes.contains(&2), "{wakes:?}");
    assert_eq!(
        t.poll(&p, a, now, 2, 1, &mut wakes),
        Ok(Wait::Failed(Error::OriginRejected))
    );
    assert_eq!(t.poll(&p, b, now, 2, 2, &mut wakes), Ok(Wait::Lead));
    t.finish(&p, b, published(Acquired::Plain(5)), &mut wakes)
        .unwrap();
    // The rejected caller is not served another caller's result.
    assert_eq!(
        t.poll(&p, a, now, 2, 1, &mut wakes),
        Ok(Wait::Failed(Error::OriginRejected))
    );
    assert_eq!(
        t.poll(&p, b, now, 2, 2, &mut wakes),
        Ok(Wait::Ready(Settled::Complete(5)))
    );
    // A forbidden sole leader fails with its own error; no waiter remains.
    let q = page(5);
    let c = waiter(t.join(&q, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&q, c, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    t.finish(&q, c, Err(FailureKind::OriginForbidden), &mut wakes)
        .unwrap();
    assert_eq!(
        t.poll(&q, c, now, 2, 1, &mut wakes),
        Ok(Wait::Failed(Error::OriginForbidden))
    );
}

#[test]
fn canceled_leader_is_revoked_and_follower_takes_over() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(4);
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    assert_eq!(t.poll(&p, b, now, 2, 2, &mut wakes), Ok(Wait::Pending));
    wakes.clear();
    t.cancel(&p, a, &mut wakes);
    assert!(wakes.contains(&2));
    assert_eq!(t.poll(&p, b, now, 2, 2, &mut wakes), Ok(Wait::Lead));
    assert_eq!(
        t.poll(&p, a, now, 2, 1, &mut wakes),
        Ok(Wait::Failed(Error::Cancelled))
    );
}

#[test]
fn deadlines_expire_waiters_at_explicit_time() {
    let now = Instant::now();
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(5);
    let a = waiter(t.join(
        &p,
        now,
        now + Duration::from_secs(1),
        2,
        true,
        free,
        &mut wakes,
    ));
    let b = waiter(t.join(
        &p,
        now,
        now + Duration::from_secs(5),
        2,
        true,
        free,
        &mut wakes,
    ));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    assert_eq!(t.poll(&p, b, now, 2, 2, &mut wakes), Ok(Wait::Pending));
    let later = now + Duration::from_secs(1);
    t.refresh(&p, later, &mut wakes);
    assert_eq!(
        t.poll(&p, a, later, 2, 1, &mut wakes),
        Ok(Wait::Failed(Error::DeadlineExceeded))
    );
    assert_eq!(t.poll(&p, b, later, 2, 2, &mut wakes), Ok(Wait::Lead));
}

#[test]
fn gates_follow_production_order() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let limits = FlightLimits {
        entries: 1,
        waiters_per_flight: 1,
        generations_per_flight: 1,
        ..FlightLimits::default()
    };
    let mut t = table(limits, now);
    let mut wakes = Vec::new();
    let p = page(6);
    assert_eq!(
        t.join(&p, deadline, deadline, 2, true, free, &mut wakes),
        Err(Error::DeadlineExceeded)
    );
    assert_eq!(
        t.join(&p, now, deadline, 0, true, free, &mut wakes),
        Err(Error::Unavailable)
    );
    let _a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(
        t.join(&p, now, deadline, 2, true, free, &mut wakes),
        Err(Error::Overloaded)
    );
    assert_eq!(
        t.join(&page(7), now, deadline, 2, true, free, &mut wakes),
        Err(Error::Overloaded)
    );
    // One generation: the first election succeeds, a retry cannot elect
    // another waiter and fails the cohort.
    let mut t = table(
        FlightLimits {
            generations_per_flight: 1,
            ..FlightLimits::default()
        },
        now,
    );
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    t.finish(&p, a, Err(FailureKind::OriginRejected), &mut wakes)
        .unwrap();
    assert_eq!(
        t.poll(&p, b, now, 2, 2, &mut wakes),
        Ok(Wait::Failed(Error::Unavailable))
    );
    assert_eq!(
        t.join(&p, now, deadline, 2, true, free, &mut wakes),
        Err(Error::Unavailable)
    );
}

/// Waiter and flight counts with a flight cap; charges release on drop.
#[derive(Clone, Default)]
struct Quota(Rc<Cell<(usize, usize, usize)>>);

impl Quota {
    fn new(max_flights: usize) -> Self {
        Self(Rc::new(Cell::new((0, 0, max_flights))))
    }

    fn used(&self) -> (usize, usize) {
        let (waiters, flights, _) = self.0.get();
        (waiters, flights)
    }

    fn reserve(&self) -> impl FnMut(Reserve) -> Result<Charge> + use<> {
        let quota = self.clone();
        move |kind| {
            let (mut waiters, mut flights, max) = quota.0.get();
            match kind {
                Reserve::Waiter => waiters += 1,
                Reserve::Flight if flights == max => return Err(Error::Overloaded),
                Reserve::Flight => flights += 1,
            }
            quota.0.set((waiters, flights, max));
            Ok(Charge(quota.clone(), kind))
        }
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
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let quota = Quota::new(1);
    let a = waiter(t.join(
        &page(8),
        now,
        deadline,
        2,
        true,
        quota.reserve(),
        &mut wakes,
    ));
    assert_eq!(
        t.join(
            &page(9),
            now,
            deadline,
            2,
            true,
            quota.reserve(),
            &mut wakes
        ),
        Err(Error::Overloaded)
    );
    assert_eq!(quota.used(), (1, 1));
    t.detach(&page(8), a, &mut wakes);
    assert_eq!(quota.used(), (0, 0));
    assert!(t.is_empty());
}

#[test]
fn budget_gates() {
    let now = Instant::now();
    assert_eq!(
        join_budget_gate(now, now, 1, false),
        Err(Error::DeadlineExceeded)
    );
    assert_eq!(
        join_budget_gate(now, now + Duration::from_secs(1), 0, false),
        Err(Error::Unavailable)
    );
    assert_eq!(
        join_budget_gate(now, now + Duration::from_secs(1), 0, true),
        Ok(())
    );
    let limits = FlightLimits::default();
    assert_eq!(Error::from(JoinReject::WaiterCap), Error::Overloaded);
    assert_eq!(Error::from(JoinReject::Failed(Error::Io)), Error::Io);
    assert_eq!(entry_cap_gate(1023, &limits), Ok(()));
    assert_eq!(entry_cap_gate(1024, &limits), Err(Error::Overloaded));
}

/// Known issue 24: a leader whose deadline passes while its fill still runs
/// is revoked. With the fill retained as an operation (production
/// `Flights::retain_operation`), the flight keeps draining: the expired
/// leader's detach does not remove it, a new caller waits instead of starting
/// a second concurrent fill, the stale leader's finish is rejected
/// (`Internal`, production `StaleFlight`), and only completing the operation
/// elects the new caller.
#[test]
fn expired_leader_with_retained_operation_blocks_reelection_until_complete() {
    let now = Instant::now();
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let quota = Quota::new(4);
    let p = page(20);
    let a = waiter(t.join(
        &p,
        now,
        now + Duration::from_secs(1),
        2,
        true,
        quota.reserve(),
        &mut wakes,
    ));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    t.retain_operation(&p, a, &mut wakes).unwrap();
    assert!(t.is_leader(&p, a));

    let later = now + Duration::from_secs(1);
    t.refresh(&p, later, &mut wakes);
    assert!(!t.is_leader(&p, a));
    assert_eq!(
        t.poll(&p, a, later, 2, 1, &mut wakes),
        Ok(Wait::Failed(Error::DeadlineExceeded))
    );
    t.detach(&p, a, &mut wakes);
    assert_eq!(quota.used(), (0, 1), "entry kept");

    let deadline = later + Duration::from_secs(5);
    let b = waiter(t.join(&p, later, deadline, 2, true, quota.reserve(), &mut wakes));
    assert_eq!(t.poll(&p, b, later, 2, 2, &mut wakes), Ok(Wait::Pending));
    assert_eq!(
        t.finish(&p, a, published(Acquired::Plain(1)), &mut wakes),
        Err(Error::Internal)
    );
    assert_eq!(t.poll(&p, b, later, 2, 2, &mut wakes), Ok(Wait::Pending));

    wakes.clear();
    t.complete_operation(&p, &mut wakes).unwrap();
    assert!(wakes.contains(&2));
    assert_eq!(t.poll(&p, b, later, 2, 2, &mut wakes), Ok(Wait::Lead));
    t.retain_operation(&p, b, &mut wakes).unwrap();
    t.complete_operation(&p, &mut wakes).unwrap();
    t.finish(&p, b, published(Acquired::Plain(2)), &mut wakes)
        .unwrap();
    assert_eq!(
        t.poll(&p, b, later, 2, 2, &mut wakes),
        Ok(Wait::Ready(Settled::Complete(2)))
    );
    t.detach(&p, b, &mut wakes);
    assert_eq!(quota.used(), (0, 0));
    assert!(t.is_empty());
}

/// Without a retained operation the same sequence reproduces the simulator's
/// original Known issue 24 failure: the expired leader's detach removes the
/// entry, a new caller leads a second fill, and the first leader's finish is
/// stale.
#[test]
fn expired_leader_without_operation_is_stale_after_new_leader() {
    let now = Instant::now();
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(21);
    let a = waiter(t.join(
        &p,
        now,
        now + Duration::from_secs(1),
        2,
        true,
        free,
        &mut wakes,
    ));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    let later = now + Duration::from_secs(1);
    t.refresh(&p, later, &mut wakes);
    t.detach(&p, a, &mut wakes);
    assert!(t.is_empty());
    let b = waiter(t.join(
        &p,
        later,
        later + Duration::from_secs(5),
        2,
        true,
        free,
        &mut wakes,
    ));
    assert_eq!(t.poll(&p, b, later, 2, 2, &mut wakes), Ok(Wait::Lead));
    assert!(!t.is_leader(&p, a));
    assert_eq!(
        t.finish(&p, a, published(Acquired::Plain(1)), &mut wakes),
        Err(Error::Internal)
    );
    assert!(t.is_leader(&p, b));
}

#[test]
fn operation_errors_and_completed_operation_settles_published_result() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(22);
    assert_eq!(t.retain_operation(&p, 0, &mut wakes), Err(Error::Internal));
    assert_eq!(t.complete_operation(&p, &mut wakes), Err(Error::Internal));
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    // Not elected yet.
    assert_eq!(t.retain_operation(&p, a, &mut wakes), Err(Error::Internal));
    assert_eq!(t.complete_operation(&p, &mut wakes), Err(Error::Internal));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    assert_eq!(t.retain_operation(&p, b, &mut wakes), Err(Error::Internal));
    t.retain_operation(&p, a, &mut wakes).unwrap();
    // Publication while the operation is still retained stays draining.
    t.finish(&p, a, published(Acquired::Plain(7)), &mut wakes)
        .unwrap();
    assert_eq!(t.poll(&p, b, now, 2, 2, &mut wakes), Ok(Wait::Pending));
    wakes.clear();
    t.complete_operation(&p, &mut wakes).unwrap();
    assert!(wakes.contains(&2));
    assert_eq!(
        t.poll(&p, b, now, 2, 2, &mut wakes),
        Ok(Wait::Ready(Settled::Complete(7)))
    );
}

/// Production checks a waiter's scope before its recorded error: a rejected
/// leader polled after its deadline reports `DeadlineExceeded`, before it
/// reports `OriginRejected`, and a canceled waiter stays `Cancelled`.
#[test]
fn deadline_takes_precedence_over_recorded_rejection() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(1);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(23);
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    t.finish(&p, a, Err(FailureKind::OriginRejected), &mut wakes)
        .unwrap();
    t.cancel(&p, b, &mut wakes);
    assert_eq!(
        t.poll(&p, a, now, 2, 1, &mut wakes),
        Ok(Wait::Failed(Error::OriginRejected))
    );
    assert_eq!(
        t.poll(&p, a, deadline, 2, 1, &mut wakes),
        Ok(Wait::Failed(Error::DeadlineExceeded))
    );
    assert_eq!(
        t.poll(&p, b, deadline, 2, 2, &mut wakes),
        Ok(Wait::Failed(Error::Cancelled))
    );
}

/// Known issue 30: with handover allowed, an expired leader's running fill
/// continues for the live waiter with the latest deadline. That waiter is not
/// re-elected (no second fill), the elected capability still publishes, and
/// every remaining waiter completes from the one fill.
#[test]
fn expired_leader_hands_running_fill_to_latest_waiter() {
    let now = Instant::now();
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let quota = Quota::new(4);
    let p = page(30);
    let secs = |s| now + Duration::from_secs(s);
    let a = waiter(t.join(&p, now, secs(1), 2, true, quota.reserve(), &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    t.retain_operation(&p, a, &mut wakes).unwrap();
    t.allow_handover(&p, a, &mut wakes).unwrap();
    let b = waiter(t.join(&p, now, secs(3), 2, true, quota.reserve(), &mut wakes));
    let c = waiter(t.join(&p, now, secs(2), 2, true, quota.reserve(), &mut wakes));
    assert_eq!(t.poll(&p, b, now, 2, 2, &mut wakes), Ok(Wait::Pending));
    assert_eq!(t.poll(&p, c, now, 2, 3, &mut wakes), Ok(Wait::Pending));
    assert_eq!(t.leader_deadline(&p), Some(secs(1)));
    assert!(!t.handed_over(&p));

    t.refresh(&p, secs(1), &mut wakes);
    assert!(t.handed_over(&p));
    assert!(t.is_leader(&p, a), "the elected capability survives");
    assert_eq!(
        t.leader_deadline(&p),
        Some(secs(3)),
        "latest deadline adopts"
    );
    assert_eq!(
        t.poll(&p, a, secs(1), 2, 1, &mut wakes),
        Ok(Wait::Failed(Error::DeadlineExceeded))
    );
    t.detach(&p, a, &mut wakes);
    assert_eq!(t.poll(&p, b, secs(1), 2, 2, &mut wakes), Ok(Wait::Pending));
    assert_eq!(t.poll(&p, c, secs(1), 2, 3, &mut wakes), Ok(Wait::Pending));

    t.complete_operation(&p, &mut wakes).unwrap();
    t.finish(&p, a, published(Acquired::Plain(7)), &mut wakes)
        .unwrap();
    assert_eq!(
        t.poll(&p, b, secs(1), 2, 2, &mut wakes),
        Ok(Wait::Ready(Settled::Complete(7)))
    );
    assert_eq!(
        t.poll(&p, c, secs(1), 2, 3, &mut wakes),
        Ok(Wait::Ready(Settled::Complete(7)))
    );
    t.detach(&p, b, &mut wakes);
    t.detach(&p, c, &mut wakes);
    assert!(t.is_empty());
    assert_eq!(quota.used(), (0, 0));
}

/// A handed-over fill that ends with the elected caller's deadline retries
/// for the adopting waiter, which leads with its own unspent budget; once the
/// last adopter expires the fill is revoked like before.
#[test]
fn handed_over_fill_deadline_retries_and_last_expiry_revokes() {
    let now = Instant::now();
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let quota = Quota::new(4);
    let p = page(31);
    let secs = |s| now + Duration::from_secs(s);
    let a = waiter(t.join(&p, now, secs(1), 2, true, quota.reserve(), &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    t.retain_operation(&p, a, &mut wakes).unwrap();
    t.allow_handover(&p, a, &mut wakes).unwrap();
    let b = waiter(t.join(&p, now, secs(3), 2, true, quota.reserve(), &mut wakes));
    t.refresh(&p, secs(1), &mut wakes);
    assert!(t.handed_over(&p));
    t.complete_operation(&p, &mut wakes).unwrap();
    t.finish(
        &p,
        a,
        Err(FailureKind::Terminal(Error::DeadlineExceeded)),
        &mut wakes,
    )
    .unwrap();
    assert_eq!(t.poll(&p, b, secs(1), 2, 2, &mut wakes), Ok(Wait::Lead));
    t.retain_operation(&p, b, &mut wakes).unwrap();
    t.allow_handover(&p, b, &mut wakes).unwrap();
    t.refresh(&p, secs(3), &mut wakes);
    assert!(!t.is_leader(&p, b), "no waiter left to adopt");
    assert_eq!(t.leader_deadline(&p), None);
    t.complete_operation(&p, &mut wakes).unwrap();
    assert_eq!(
        t.poll(&p, b, secs(3), 2, 2, &mut wakes),
        Ok(Wait::Failed(Error::DeadlineExceeded))
    );
    t.detach(&p, a, &mut wakes);
    t.detach(&p, b, &mut wakes);
    assert!(t.is_empty());
}

/// Terminal failures other than the elected caller's own deadline or
/// cancellation still fail the cohort after a handover.
#[test]
fn handover_failure_retries_only_caller_scope_failures() {
    // Whether `fail` settles to a retry (`RetryPending`) or the cohort fails.
    let retry = |handed_over: bool, failure: FailureKind| {
        let now = Rc::new(Cell::new(Instant::now()));
        let mut state: State<u8, u8, u8, Waiter<()>, Token> = State::default();
        for id in 0..2 {
            let deadline = now.get() + Duration::from_secs(1);
            let now = now.clone();
            state.register(
                id,
                Waiter {
                    deadline,
                    now,
                    _charge: (),
                },
                true,
                true,
            );
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
        matches!(state.phase, Phase::RetryPending)
    };
    assert!(retry(true, FailureKind::Terminal(Error::DeadlineExceeded)));
    assert!(retry(true, FailureKind::Terminal(Error::Cancelled)));
    assert!(!retry(true, FailureKind::Terminal(Error::Io)));
    assert!(!retry(
        false,
        FailureKind::Terminal(Error::DeadlineExceeded)
    ));
    assert!(!retry(false, FailureKind::Terminal(Error::Cancelled)));
    assert!(retry(false, FailureKind::OriginRejected));
    let now = Instant::now();
    let d = now + Duration::from_secs(2);
    assert_eq!(handover_ceiling(now, d, false), d);
    assert_eq!(
        handover_ceiling(now, d, true),
        d + Duration::from_secs(2) * HANDOVER_SPANS
    );
    assert_eq!(handover_ceiling(d, now, true), now, "expired leader");
}

fn copy_waiter(joined: Result<Joined<u32, u32>>) -> u64 {
    match joined.unwrap() {
        Joined::Waiter(id) => id,
        other => panic!("{other:?}"),
    }
}

/// Known issue 31: a copy-only request joins the running acquisition like
/// production `Flights::join_copy`, waits for its result, and never leads.
#[test]
fn copy_join_waits_for_the_running_acquisition_and_never_leads() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(0);
    // No flight: a miss, and no flight is created.
    assert_eq!(
        t.join_copy(&p, now, deadline, free, &mut wakes),
        Ok(Joined::Miss)
    );
    assert!(t.is_empty());
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 10, &mut wakes), Ok(Wait::Lead));
    let c = copy_waiter(t.join_copy(&p, now, deadline, free, &mut wakes));
    assert_eq!(t.waiters(&p), 2);
    assert_eq!(t.poll(&p, c, now, 2, 11, &mut wakes), Ok(Wait::Pending));
    t.finish(&p, a, published(Acquired::Plain(7)), &mut wakes)
        .unwrap();
    assert_eq!(wakes, [11]);
    assert_eq!(
        t.poll(&p, c, now, 2, 11, &mut wakes),
        Ok(Wait::Ready(Settled::Complete(7)))
    );
    // Settled results are returned without registering.
    assert_eq!(
        t.join_copy(&p, now, deadline, free, &mut wakes),
        Ok(Joined::Settled(Settled::Complete(7)))
    );
    t.detach(&p, a, &mut wakes);
    t.detach(&p, c, &mut wakes);
    assert!(t.is_empty());
}

#[test]
fn copy_join_misses_failed_and_takes_partial_results() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(1);
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    t.finish(&p, a, published(Acquired::Cipher(3)), &mut wakes)
        .unwrap();
    assert_eq!(
        t.join_copy(&p, now, deadline, free, &mut wakes),
        Ok(Joined::Settled(Settled::Partial(3)))
    );
    let q = page(2);
    let b = waiter(t.join(&q, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&q, b, now, 2, 2, &mut wakes), Ok(Wait::Lead));
    t.finish(
        &q,
        b,
        Err(FailureKind::Terminal(Error::Unavailable)),
        &mut wakes,
    )
    .unwrap();
    assert_eq!(
        t.join_copy(&q, now, deadline, free, &mut wakes),
        Ok(Joined::Miss)
    );
}

#[test]
fn copy_join_respects_the_waiter_cap_and_a_lost_leader_fails_copy_waiters() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let limits = FlightLimits {
        waiters_per_flight: 2,
        ..FlightLimits::default()
    };
    let mut t = table(limits, now);
    let mut wakes = Vec::new();
    let p = page(3);
    let a = waiter(t.join(&p, now, deadline, 2, true, free, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 2, 1, &mut wakes), Ok(Wait::Lead));
    let c = copy_waiter(t.join_copy(&p, now, deadline, free, &mut wakes));
    assert_eq!(
        t.join_copy(&p, now, deadline, free, &mut wakes),
        Err(Error::Overloaded)
    );
    assert_eq!(t.poll(&p, c, now, 2, 2, &mut wakes), Ok(Wait::Pending));
    // The leader goes away; the copy waiter cannot adopt or lead.
    t.abandon(&p, a, &mut wakes);
    t.detach(&p, a, &mut wakes);
    assert!(matches!(
        t.poll(&p, c, now, 2, 2, &mut wakes),
        Ok(Wait::Failed(_))
    ));
}
