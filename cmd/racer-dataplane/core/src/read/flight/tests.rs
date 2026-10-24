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
