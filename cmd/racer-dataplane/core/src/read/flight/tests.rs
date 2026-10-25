//! Leader election, coalescing, partial results, failure, cancellation,
//! expiry, caps, and admission for the sans-IO page flight table.

// The attribute below also marks this file as test-only for tests/purity.rs.
#[cfg(test)]
use super::*;
use racer_control_wire::CacheId;
use racer_object_wire::model::{CacheKey, ObjectId, ObjectVersion, PageNumber, StrongEtag};
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

type Table = PageFlights<u32, u32, Acquired>;

fn table(limits: FlightLimits, now: Instant) -> Table {
    PageFlights::with_hasher(limits, split, now, RandomState::new())
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
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.len(), 1);
    assert_eq!(t.waiters(&p), 2);
    assert_eq!(t.poll(&p, a, now, 10, &mut wakes), Ok(Event::Lead));
    assert_eq!(t.poll(&p, b, now, 11, &mut wakes), Ok(Event::Pending));
    t.finish(&p, a, Outcome::Published(Acquired::Plain(7)), &mut wakes)
        .unwrap();
    assert_eq!(wakes, [11]);
    assert_eq!(t.poll(&p, b, now, 11, &mut wakes), Ok(Event::Complete(7)));
    // Late joiners see the completed result without registering.
    assert_eq!(
        t.join(&p, now, deadline, 0, true, &mut Unlimited, &mut wakes),
        Ok(Joined::Complete(7))
    );
    t.detach(&p, a, &mut Unlimited, &mut wakes);
    t.detach(&p, b, &mut Unlimited, &mut wakes);
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
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 10, &mut wakes), Ok(Event::Lead));
    assert!(t.acquiring(&p));
    assert!(!t.acquiring(&page(1)));
    t.finish(&p, a, Outcome::Published(Acquired::Plain(7)), &mut wakes)
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
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    t.finish(&p, a, Outcome::Published(Acquired::Cipher(3)), &mut wakes)
        .unwrap();
    // Ciphertext callers take the partial; with zero attempts a plaintext
    // caller may still join because a partial exists.
    assert_eq!(
        t.join(&p, now, deadline, 0, false, &mut Unlimited, &mut wakes),
        Ok(Joined::Partial(3))
    );
    let c = waiter(t.join(&p, now, deadline, 0, true, &mut Unlimited, &mut wakes));
    // Plaintext waiters regain an acquisition opportunity.
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Pending));
    assert_eq!(t.poll(&p, c, now, 3, &mut wakes), Ok(Event::Pending));
    t.finish(&p, a, Outcome::Published(Acquired::Plain(4)), &mut wakes)
        .unwrap();
    assert_eq!(t.poll(&p, c, now, 3, &mut wakes), Ok(Event::Complete(4)));
}

#[test]
fn failure_and_retry_semantics() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(2);
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    // Retry hands the next eligible waiter leadership.
    t.finish(&p, a, Outcome::Retry, &mut wakes).unwrap();
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Lead));
    t.finish(&p, b, Outcome::Failed(Error::OriginForbidden), &mut wakes)
        .unwrap();
    assert_eq!(
        t.poll(&p, a, now, 1, &mut wakes),
        Ok(Event::Failed(Error::OriginForbidden))
    );
    assert_eq!(
        t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes),
        Err(Error::OriginForbidden)
    );
    // Retry with no eligible waiter fails as unavailable.
    let q = page(3);
    let c = waiter(t.join(&q, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&q, c, now, 1, &mut wakes), Ok(Event::Lead));
    t.finish(&q, c, Outcome::Retry, &mut wakes).unwrap();
    assert_eq!(
        t.poll(&q, c, now, 1, &mut wakes),
        Ok(Event::Failed(Error::Unavailable))
    );
    assert_eq!(
        t.finish(&q, c, Outcome::Retry, &mut wakes),
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
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Pending));
    // Only leader-held flights accept a rejection, and only origin errors.
    assert_eq!(
        t.reject(&p, b, Error::OriginRejected, &mut wakes),
        Err(Error::Internal)
    );
    assert_eq!(t.reject(&p, a, Error::Io, &mut wakes), Err(Error::Internal));
    wakes.clear();
    t.reject(&p, a, Error::OriginRejected, &mut wakes).unwrap();
    assert!(wakes.contains(&2), "{wakes:?}");
    assert_eq!(
        t.poll(&p, a, now, 1, &mut wakes),
        Ok(Event::Failed(Error::OriginRejected))
    );
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Lead));
    t.finish(&p, b, Outcome::Published(Acquired::Plain(5)), &mut wakes)
        .unwrap();
    // The rejected caller is not served another caller's result.
    assert_eq!(
        t.poll(&p, a, now, 1, &mut wakes),
        Ok(Event::Failed(Error::OriginRejected))
    );
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Complete(5)));
    // A forbidden sole leader fails with its own error; no waiter remains.
    let q = page(5);
    let c = waiter(t.join(&q, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&q, c, now, 1, &mut wakes), Ok(Event::Lead));
    t.reject(&q, c, Error::OriginForbidden, &mut wakes).unwrap();
    assert_eq!(
        t.poll(&q, c, now, 1, &mut wakes),
        Ok(Event::Failed(Error::OriginForbidden))
    );
}

#[test]
fn canceled_leader_is_revoked_and_follower_takes_over() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(4);
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Pending));
    wakes.clear();
    t.cancel(&p, a, &mut wakes);
    assert!(wakes.contains(&2));
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Lead));
    assert_eq!(
        t.poll(&p, a, now, 1, &mut wakes),
        Ok(Event::Failed(Error::Cancelled))
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
        &mut Unlimited,
        &mut wakes,
    ));
    let b = waiter(t.join(
        &p,
        now,
        now + Duration::from_secs(5),
        2,
        true,
        &mut Unlimited,
        &mut wakes,
    ));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Pending));
    let later = now + Duration::from_secs(1);
    t.refresh(&p, later, &mut wakes);
    assert_eq!(
        t.poll(&p, a, later, 1, &mut wakes),
        Ok(Event::Failed(Error::DeadlineExceeded))
    );
    assert_eq!(t.poll(&p, b, later, 2, &mut wakes), Ok(Event::Lead));
}

#[test]
fn gates_follow_production_order() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let limits = FlightLimits {
        entries: 1,
        waiters_per_flight: 1,
        generations_per_flight: 1,
    };
    let mut t = table(limits, now);
    let mut wakes = Vec::new();
    let p = page(6);
    assert_eq!(
        t.join(&p, deadline, deadline, 2, true, &mut Unlimited, &mut wakes),
        Err(Error::DeadlineExceeded)
    );
    assert_eq!(
        t.join(&p, now, deadline, 0, true, &mut Unlimited, &mut wakes),
        Err(Error::Unavailable)
    );
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(
        t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes),
        Err(Error::Overloaded)
    );
    assert_eq!(
        t.join(&page(7), now, deadline, 2, true, &mut Unlimited, &mut wakes),
        Err(Error::Overloaded)
    );
    // One generation: the first election succeeds, a retry cannot re-elect.
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    t.finish(&p, a, Outcome::Retry, &mut wakes).unwrap();
    assert_eq!(
        t.poll(&p, a, now, 1, &mut wakes),
        Ok(Event::Failed(Error::Unavailable))
    );
}

#[derive(Default)]
struct Quota {
    waiters: usize,
    flights: usize,
    max_flights: usize,
}

impl FlightAdmission for Quota {
    fn waiter(&mut self, _: &PageId) -> Result<()> {
        self.waiters += 1;
        Ok(())
    }
    fn flight(&mut self, _: &PageId) -> Result<()> {
        if self.flights == self.max_flights {
            return Err(Error::Overloaded);
        }
        self.flights += 1;
        Ok(())
    }
    fn release_waiter(&mut self, _: &PageId) {
        self.waiters -= 1;
    }
    fn release_flight(&mut self, _: &PageId) {
        self.flights -= 1;
    }
}

#[test]
fn admission_is_released_on_rejection_and_detach() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let mut quota = Quota {
        max_flights: 1,
        ..Quota::default()
    };
    let a = waiter(t.join(&page(8), now, deadline, 2, true, &mut quota, &mut wakes));
    assert_eq!(
        t.join(&page(9), now, deadline, 2, true, &mut quota, &mut wakes),
        Err(Error::Overloaded)
    );
    assert_eq!((quota.waiters, quota.flights), (1, 1));
    t.detach(&page(8), a, &mut quota, &mut wakes);
    assert_eq!((quota.waiters, quota.flights), (0, 0));
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
    assert_eq!(waiter_cap_gate(256, &limits), Err(Error::Overloaded));
    assert_eq!(waiter_cap_gate(255, &limits), Ok(()));
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
    let mut quota = Quota {
        max_flights: 4,
        ..Quota::default()
    };
    let p = page(20);
    let a = waiter(t.join(
        &p,
        now,
        now + Duration::from_secs(1),
        2,
        true,
        &mut quota,
        &mut wakes,
    ));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    t.retain_operation(&p, a).unwrap();
    assert!(t.is_leader(&p, a));

    let later = now + Duration::from_secs(1);
    t.refresh(&p, later, &mut wakes);
    assert!(!t.is_leader(&p, a));
    assert_eq!(
        t.poll(&p, a, later, 1, &mut wakes),
        Ok(Event::Failed(Error::DeadlineExceeded))
    );
    t.detach(&p, a, &mut quota, &mut wakes);
    assert_eq!((quota.waiters, quota.flights), (0, 1), "entry kept");

    let deadline = later + Duration::from_secs(5);
    let b = waiter(t.join(&p, later, deadline, 2, true, &mut quota, &mut wakes));
    assert_eq!(t.poll(&p, b, later, 2, &mut wakes), Ok(Event::Pending));
    assert_eq!(
        t.finish(&p, a, Outcome::Published(Acquired::Plain(1)), &mut wakes),
        Err(Error::Internal)
    );
    assert_eq!(t.poll(&p, b, later, 2, &mut wakes), Ok(Event::Pending));

    wakes.clear();
    t.complete_operation(&p, &mut quota, &mut wakes).unwrap();
    assert!(wakes.contains(&2));
    assert_eq!(t.poll(&p, b, later, 2, &mut wakes), Ok(Event::Lead));
    t.retain_operation(&p, b).unwrap();
    t.complete_operation(&p, &mut quota, &mut wakes).unwrap();
    t.finish(&p, b, Outcome::Published(Acquired::Plain(2)), &mut wakes)
        .unwrap();
    assert_eq!(t.poll(&p, b, later, 2, &mut wakes), Ok(Event::Complete(2)));
    t.detach(&p, b, &mut quota, &mut wakes);
    assert_eq!((quota.waiters, quota.flights), (0, 0));
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
        &mut Unlimited,
        &mut wakes,
    ));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    let later = now + Duration::from_secs(1);
    t.refresh(&p, later, &mut wakes);
    t.detach(&p, a, &mut Unlimited, &mut wakes);
    assert!(t.is_empty());
    let b = waiter(t.join(
        &p,
        later,
        later + Duration::from_secs(5),
        2,
        true,
        &mut Unlimited,
        &mut wakes,
    ));
    assert_eq!(t.poll(&p, b, later, 2, &mut wakes), Ok(Event::Lead));
    assert!(!t.is_leader(&p, a));
    assert_eq!(
        t.finish(&p, a, Outcome::Published(Acquired::Plain(1)), &mut wakes),
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
    assert_eq!(t.retain_operation(&p, 0), Err(Error::Internal));
    assert_eq!(
        t.complete_operation(&p, &mut Unlimited, &mut wakes),
        Err(Error::Internal)
    );
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    // Not elected yet.
    assert_eq!(t.retain_operation(&p, a), Err(Error::Internal));
    assert_eq!(
        t.complete_operation(&p, &mut Unlimited, &mut wakes),
        Err(Error::Internal)
    );
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    assert_eq!(t.retain_operation(&p, b), Err(Error::Internal));
    t.retain_operation(&p, a).unwrap();
    // Publication while the operation is still retained stays draining.
    t.finish(&p, a, Outcome::Published(Acquired::Plain(7)), &mut wakes)
        .unwrap();
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Pending));
    wakes.clear();
    t.complete_operation(&p, &mut Unlimited, &mut wakes)
        .unwrap();
    assert!(wakes.contains(&2));
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Complete(7)));
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
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    let b = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    t.reject(&p, a, Error::OriginRejected, &mut wakes).unwrap();
    t.cancel(&p, b, &mut wakes);
    assert_eq!(
        t.poll(&p, a, now, 1, &mut wakes),
        Ok(Event::Failed(Error::OriginRejected))
    );
    assert_eq!(
        t.poll(&p, a, deadline, 1, &mut wakes),
        Ok(Event::Failed(Error::DeadlineExceeded))
    );
    assert_eq!(
        t.poll(&p, b, deadline, 2, &mut wakes),
        Ok(Event::Failed(Error::Cancelled))
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
    let mut quota = Quota {
        max_flights: 4,
        ..Quota::default()
    };
    let p = page(30);
    let secs = |s| now + Duration::from_secs(s);
    let a = waiter(t.join(&p, now, secs(1), 2, true, &mut quota, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    t.retain_operation(&p, a).unwrap();
    t.allow_handover(&p, a).unwrap();
    let b = waiter(t.join(&p, now, secs(3), 2, true, &mut quota, &mut wakes));
    let c = waiter(t.join(&p, now, secs(2), 2, true, &mut quota, &mut wakes));
    assert_eq!(t.poll(&p, b, now, 2, &mut wakes), Ok(Event::Pending));
    assert_eq!(t.poll(&p, c, now, 3, &mut wakes), Ok(Event::Pending));
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
        t.poll(&p, a, secs(1), 1, &mut wakes),
        Ok(Event::Failed(Error::DeadlineExceeded))
    );
    t.detach(&p, a, &mut quota, &mut wakes);
    assert_eq!(t.poll(&p, b, secs(1), 2, &mut wakes), Ok(Event::Pending));
    assert_eq!(t.poll(&p, c, secs(1), 3, &mut wakes), Ok(Event::Pending));

    t.complete_operation(&p, &mut quota, &mut wakes).unwrap();
    t.finish(&p, a, Outcome::Published(Acquired::Plain(7)), &mut wakes)
        .unwrap();
    assert_eq!(
        t.poll(&p, b, secs(1), 2, &mut wakes),
        Ok(Event::Complete(7))
    );
    assert_eq!(
        t.poll(&p, c, secs(1), 3, &mut wakes),
        Ok(Event::Complete(7))
    );
    t.detach(&p, b, &mut quota, &mut wakes);
    t.detach(&p, c, &mut quota, &mut wakes);
    assert!(t.is_empty());
    assert_eq!((quota.waiters, quota.flights), (0, 0));
}

/// A handed-over fill that ends with the elected caller's deadline retries
/// for the adopting waiter, which leads with its own unspent budget; once the
/// last adopter expires the fill is revoked like before.
#[test]
fn handed_over_fill_deadline_retries_and_last_expiry_revokes() {
    let now = Instant::now();
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let mut quota = Quota {
        max_flights: 4,
        ..Quota::default()
    };
    let p = page(31);
    let secs = |s| now + Duration::from_secs(s);
    let a = waiter(t.join(&p, now, secs(1), 2, true, &mut quota, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    t.retain_operation(&p, a).unwrap();
    t.allow_handover(&p, a).unwrap();
    let b = waiter(t.join(&p, now, secs(3), 2, true, &mut quota, &mut wakes));
    t.refresh(&p, secs(1), &mut wakes);
    assert!(t.handed_over(&p));
    t.complete_operation(&p, &mut quota, &mut wakes).unwrap();
    t.finish(&p, a, Outcome::Failed(Error::DeadlineExceeded), &mut wakes)
        .unwrap();
    assert_eq!(t.poll(&p, b, secs(1), 2, &mut wakes), Ok(Event::Lead));
    t.retain_operation(&p, b).unwrap();
    t.allow_handover(&p, b).unwrap();
    t.refresh(&p, secs(3), &mut wakes);
    assert!(!t.is_leader(&p, b), "no waiter left to adopt");
    assert_eq!(t.leader_deadline(&p), None);
    t.complete_operation(&p, &mut quota, &mut wakes).unwrap();
    assert_eq!(
        t.poll(&p, b, secs(3), 2, &mut wakes),
        Ok(Event::Failed(Error::DeadlineExceeded))
    );
    t.detach(&p, a, &mut quota, &mut wakes);
    t.detach(&p, b, &mut quota, &mut wakes);
    assert!(t.is_empty());
}

/// Terminal failures other than the elected caller's own deadline or
/// cancellation still fail the cohort after a handover.
#[test]
fn handover_outcome_maps_only_caller_scope_failures() {
    let retry = |o: Outcome<u8, Error>| matches!(o, Outcome::Retry);
    assert!(retry(handover_outcome(
        true,
        Outcome::Failed(Error::DeadlineExceeded)
    )));
    assert!(retry(handover_outcome(
        true,
        Outcome::Failed(Error::Cancelled)
    )));
    assert!(matches!(
        handover_outcome(true, Outcome::<u8, _>::Failed(Error::Io)),
        Outcome::Failed(Error::Io)
    ));
    assert!(matches!(
        handover_outcome(false, Outcome::<u8, _>::Failed(Error::DeadlineExceeded)),
        Outcome::Failed(Error::DeadlineExceeded)
    ));
    assert!(matches!(
        handover_outcome(true, Outcome::Published(3u8)),
        Outcome::Published(3)
    ));
    let now = Instant::now();
    let d = now + Duration::from_secs(2);
    assert_eq!(handover_ceiling(now, d, false), d);
    assert_eq!(
        handover_ceiling(now, d, true),
        d + Duration::from_secs(2) * HANDOVER_SPANS
    );
    assert_eq!(handover_ceiling(d, now, true), now, "expired leader");
}

fn copy_waiter(joined: Result<JoinedCopy<u32, u32>>) -> u64 {
    match joined.unwrap() {
        JoinedCopy::Waiter(id) => id,
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
        t.join_copy(&p, now, deadline, &mut Unlimited, &mut wakes),
        Ok(JoinedCopy::Miss)
    );
    assert!(t.is_empty());
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 10, &mut wakes), Ok(Event::Lead));
    let c = copy_waiter(t.join_copy(&p, now, deadline, &mut Unlimited, &mut wakes));
    assert_eq!(t.waiters(&p), 2);
    assert_eq!(t.poll(&p, c, now, 11, &mut wakes), Ok(Event::Pending));
    t.finish(&p, a, Outcome::Published(Acquired::Plain(7)), &mut wakes)
        .unwrap();
    assert_eq!(wakes, [11]);
    assert_eq!(t.poll(&p, c, now, 11, &mut wakes), Ok(Event::Complete(7)));
    // Settled results are returned without registering.
    assert_eq!(
        t.join_copy(&p, now, deadline, &mut Unlimited, &mut wakes),
        Ok(JoinedCopy::Complete(7))
    );
    t.detach(&p, a, &mut Unlimited, &mut wakes);
    t.detach(&p, c, &mut Unlimited, &mut wakes);
    assert!(t.is_empty());
}

#[test]
fn copy_join_misses_failed_and_takes_partial_results() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(10);
    let mut t = table(FlightLimits::default(), now);
    let mut wakes = Vec::new();
    let p = page(1);
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    t.finish(&p, a, Outcome::Published(Acquired::Cipher(3)), &mut wakes)
        .unwrap();
    assert_eq!(
        t.join_copy(&p, now, deadline, &mut Unlimited, &mut wakes),
        Ok(JoinedCopy::Partial(3))
    );
    let q = page(2);
    let b = waiter(t.join(&q, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&q, b, now, 2, &mut wakes), Ok(Event::Lead));
    t.finish(&q, b, Outcome::Failed(Error::Unavailable), &mut wakes)
        .unwrap();
    assert_eq!(
        t.join_copy(&q, now, deadline, &mut Unlimited, &mut wakes),
        Ok(JoinedCopy::Miss)
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
    let a = waiter(t.join(&p, now, deadline, 2, true, &mut Unlimited, &mut wakes));
    assert_eq!(t.poll(&p, a, now, 1, &mut wakes), Ok(Event::Lead));
    let c = copy_waiter(t.join_copy(&p, now, deadline, &mut Unlimited, &mut wakes));
    assert_eq!(
        t.join_copy(&p, now, deadline, &mut Unlimited, &mut wakes),
        Err(Error::Overloaded)
    );
    assert_eq!(t.poll(&p, c, now, 2, &mut wakes), Ok(Event::Pending));
    // The leader goes away; the copy waiter cannot adopt or lead.
    t.abandon(&p, a, &mut wakes);
    t.detach(&p, a, &mut Unlimited, &mut wakes);
    assert!(matches!(
        t.poll(&p, c, now, 2, &mut wakes),
        Ok(Event::Failed(_))
    ));
}
