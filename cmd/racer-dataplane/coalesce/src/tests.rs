use super::*;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::task::Wake;

#[derive(Default)]
struct Counter(AtomicUsize);
impl Wake for Counter {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}

type TestTable = Table<&'static str, Result<u32, &'static str>>;

fn table(waiters: usize, attempts: usize) -> Rc<TestTable> {
    Rc::new(Table::new(
        Limits {
            waiters_per_cohort: waiters,
            attempts_per_cohort: attempts,
        },
        Err("exhausted"),
    ))
}

#[test]
fn broadcasts_success_and_failure_before_or_after_poll() {
    for result in [Ok(42), Err("failed")] {
        for poll_first in [false, true] {
            let table = table(4, 2);
            let leader = table.join("key", 1).unwrap();
            let follower = table.join("key", 1).unwrap();
            let count = Arc::new(Counter::default());
            let waker = Waker::from(count.clone());
            assert_eq!(leader.event(Waker::noop()), Poll::Ready(Event::Lead));
            if poll_first {
                assert_eq!(follower.event(&waker), Poll::Pending);
            }
            leader.finish(result);
            assert_eq!(count.0.load(Ordering::Relaxed), usize::from(poll_first));
            assert_eq!(follower.event(&waker), Poll::Ready(Event::Complete(result)));
            assert_eq!(table.active_count(), 0);
            let next = table.join("key", 1).unwrap();
            assert_eq!(next.event(Waker::noop()), Poll::Ready(Event::Lead));
            drop(leader);
            drop(follower);
            assert_eq!(table.active_count(), 1, "old cohort cannot remove new key");
        }
    }
}

#[test]
fn last_handle_drop_releases_leadership_and_registration() {
    let table = table(2, 3);
    let request = table.join("key", 1).unwrap();
    let follower = table.join("key", 1).unwrap();
    assert_eq!(request.event(Waker::noop()), Poll::Ready(Event::Lead));
    let driver = request.clone();
    assert!(!driver.is_only_handle());
    drop(request);
    assert!(driver.is_only_handle());
    assert_eq!(table.registration_count(), 2);
    let old = Arc::new(Counter::default());
    let latest = Arc::new(Counter::default());
    assert!(follower.event(&Waker::from(old.clone())).is_pending());
    assert!(follower.event(&Waker::from(latest.clone())).is_pending());
    drop(driver);
    assert_eq!(old.0.load(Ordering::Relaxed), 0);
    assert_eq!(latest.0.load(Ordering::Relaxed), 1);
    assert_eq!(table.registration_count(), 1);
    assert_eq!(follower.event(Waker::noop()), Poll::Ready(Event::Lead));
    drop(follower);
    assert_eq!(table.registration_count(), 0);
    assert_eq!(table.active_count(), 0);
}

#[test]
fn retry_and_drop_share_bounded_attempts_and_broadcast_exhaustion() {
    let table = table(3, 2);
    let first = table.join("key", 1).unwrap();
    let second = table.join("key", 1).unwrap();
    let observer = table.join("key", 1).unwrap();
    assert_eq!(first.event(Waker::noop()), Poll::Ready(Event::Lead));
    assert!(first.event(Waker::noop()).is_pending());
    first.retry();
    assert_eq!(second.event(Waker::noop()), Poll::Ready(Event::Lead));
    drop(second);
    let exhausted = Poll::Ready(Event::Complete(Err("exhausted")));
    assert_eq!(observer.event(Waker::noop()), exhausted);
    assert_eq!(first.event(Waker::noop()), exhausted);
    assert_eq!(table.active_count(), 0);
}

#[test]
fn capacity_bounds_keys_waiters_and_completed_readers() {
    let table = table(2, 1);
    assert!(matches!(table.join("a", 0), Err(CapacityError)));
    let a = table.join("a", 2).unwrap();
    let b = table.join("b", 2).unwrap();
    assert!(matches!(table.join("c", 2), Err(CapacityError)));
    let a2 = table.join("a", 2).unwrap();
    assert!(matches!(table.join("a", 2), Err(CapacityError)));
    let b2 = table.join("b", 2).unwrap();
    a.finish(Ok(1));
    b.finish(Ok(2));
    assert_eq!(table.active_count(), 0);
    assert!(matches!(table.join("a", 2), Err(CapacityError)));
    drop(a2);
    let next = table.join("a", 2).unwrap();
    drop((a, b, b2, next));
    assert_eq!(table.registration_count(), 0);
    assert_eq!(table.active_count(), 0);
}

#[test]
fn zero_limits_and_id_exhaustion_never_wrap_or_leak_registrations() {
    assert!(matches!(table(0, 1).join("key", 1), Err(CapacityError)));
    let zero = table(1, 0);
    let waiter = zero.join("key", 1).unwrap();
    assert_eq!(
        waiter.event(Waker::noop()),
        Poll::Ready(Event::Complete(Err("exhausted")))
    );
    let table = table(2, 1);
    let first = table.join("key", usize::MAX).unwrap();
    first.cohort.borrow_mut().next = u64::MAX;
    assert!(matches!(table.join("key", usize::MAX), Err(CapacityError)));
    assert_eq!(table.registration_count(), 1);
    drop(first);
    assert_eq!(table.active_count(), 0);
}
