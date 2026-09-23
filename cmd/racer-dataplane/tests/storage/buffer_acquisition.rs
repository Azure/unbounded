// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use std::task::{Poll, Wake};
use std::time::{Duration, Instant};

#[derive(Default)]
struct Count(AtomicUsize);
impl Wake for Count {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}
fn counter() -> (Arc<Count>, Waker) {
    let count = Arc::new(Count::default());
    (count.clone(), Waker::from(count))
}
fn deadline() -> Instant {
    Instant::now() + Duration::from_secs(10)
}
fn ready(wait: &mut Acquisition, waker: &Waker) -> Fill {
    match wait.poll(waker) {
        Poll::Ready(Ok(fill)) => fill,
        _ => panic!("expected buffer grant"),
    }
}

#[test]
fn fifo_handoff_cancellation_and_no_barging() {
    let pool = io_test_pool(1);
    let held = pool.private_fill().unwrap();
    let (count, waker) = counter();
    let mut waits: Vec<_> = (0..3).map(|_| pool.wait_private_fill(deadline())).collect();
    for wait in &mut waits {
        assert!(wait.poll(&waker).is_pending());
    }
    drop(waits.remove(1)); // Cancel a queued waiter.
    drop(held);
    assert_eq!(count.0.load(Ordering::Relaxed), 1);
    pool.invariant_snapshot();
    assert!(pool.private_fill().is_err(), "newcomer stole a grant");
    assert!(waits[1].poll(&waker).is_pending());
    drop(waits.remove(0)); // Cancel an assigned, unclaimed slot.
    assert_eq!(count.0.load(Ordering::Relaxed), 2);
    drop(ready(&mut waits[0], &waker));
    drop(waits);
    pool.assert_recovered();
}

#[test]
fn deadline_bounds_fast_path_queued_wait_and_unclaimed_grant() {
    for grant in [false, true] {
        let pool = io_test_pool(1);
        let held = pool.private_fill().unwrap();
        let end = Instant::now() + Duration::from_millis(30);
        let mut expired = pool.wait_private_fill(end);
        let mut next = pool.wait_private_fill(deadline());
        assert!(expired.poll(Waker::noop()).is_pending());
        assert!(next.poll(Waker::noop()).is_pending());
        let held = if grant {
            drop(held);
            None
        } else {
            Some(held)
        };
        std::thread::sleep(end.saturating_duration_since(Instant::now()));
        assert!(
            matches!(expired.poll(Waker::noop()), Poll::Ready(Err(e)) if e.kind() == io::ErrorKind::TimedOut)
        );
        drop(held);
        drop(ready(&mut next, Waker::noop()));
        let mut expired = pool.wait_private_fill(Instant::now());
        assert!(
            matches!(expired.poll(Waker::noop()), Poll::Ready(Err(e)) if e.kind() == io::ErrorKind::TimedOut)
        );
        pool.assert_recovered();
    }
}

#[test]
fn release_skips_expired_waiters_and_wakes_the_next_live_waiter() {
    let pool = io_test_pool(1);
    let held = pool.private_fill().unwrap();
    let end = Instant::now() + Duration::from_millis(30);
    let mut expired = pool.wait_private_fill(end);
    let mut live = pool.wait_private_fill(deadline());
    let (count, waker) = counter();
    assert!(expired.poll(&waker).is_pending());
    assert!(live.poll(&waker).is_pending());
    std::thread::sleep(end.saturating_duration_since(Instant::now()));
    drop(held);
    assert_eq!(count.0.load(Ordering::Relaxed), 2);
    drop(ready(&mut live, &waker));
    assert!(
        matches!(expired.poll(&waker), Poll::Ready(Err(e)) if e.kind() == io::ErrorKind::TimedOut)
    );
    pool.assert_recovered();
}

#[test]
fn four_buffers_preserve_downstream_progress_and_eligible_fifo() {
    let pool = io_test_pool(4);
    let other = pool.test_other_worker();
    let mut held: Vec<_> = (0..4).map(|_| pool.private_fill().unwrap()).collect();
    let mut distant = other.wait_stage_reserved(Key::new([1; 32]), 3, deadline());
    let mut owner = pool.wait_stage_reserved(Key::new([2; 32]), 0, deadline());
    assert!(distant.poll(Waker::noop()).is_pending());
    assert!(owner.poll(Waker::noop()).is_pending());
    drop(held.pop());
    assert!(distant.poll(Waker::noop()).is_pending());
    let owner = ready(&mut owner, Waker::noop());
    drop(held);
    assert!(distant.poll(Waker::noop()).is_pending());
    let mut later = pool.wait_stage_reserved(Key::new([3; 32]), 3, deadline());
    assert!(later.poll(Waker::noop()).is_pending());
    drop(owner);
    let fill = ready(&mut distant, Waker::noop());
    assert!(fill.matches_key(Key::new([1; 32])));
    assert!(later.poll(Waker::noop()).is_pending());
    drop(fill);
    drop(ready(&mut later, Waker::noop()));
    pool.assert_recovered();
}

#[test]
fn final_compute_holder_wakes_updated_worker_once() {
    let pool = io_test_pool(1);
    let buffer = pool.private_fill().unwrap().publish(0).unwrap();
    let compute = buffer.compute_read();
    let mut wait = pool.test_other_worker().wait_private_fill(deadline());
    let (old, old_waker) = counter();
    let (new, new_waker) = counter();
    assert!(wait.poll(&old_waker).is_pending());
    assert!(wait.poll(&new_waker).is_pending());
    drop(buffer);
    assert_eq!(new.0.load(Ordering::Relaxed), 0);
    std::thread::spawn(move || drop(compute)).join().unwrap();
    assert_eq!(old.0.load(Ordering::Relaxed), 0);
    assert_eq!(new.0.load(Ordering::Relaxed), 1);
    drop(ready(&mut wait, &new_waker));
    pool.assert_recovered();
}

#[test]
fn concurrent_release_registration_and_cancellation_recover_capacity() {
    let pool = io_test_pool(1);
    let barrier = Arc::new(std::sync::Barrier::new(2));
    let (send, receive) = std::sync::mpsc::sync_channel(1);
    let worker_barrier = barrier.clone();
    let worker = std::thread::spawn(move || {
        for compute in receive {
            worker_barrier.wait();
            drop(compute);
        }
    });
    for i in 0..1000 {
        send.send(pool.private_fill().unwrap().into_compute())
            .unwrap();
        let mut wait = pool.wait_private_fill(deadline());
        barrier.wait();
        let result = wait.poll(Waker::noop());
        if i % 2 == 0 {
            drop(wait);
            drop(result);
        } else {
            match result {
                Poll::Ready(Ok(fill)) => drop(fill),
                Poll::Pending => loop {
                    match wait.poll(Waker::noop()) {
                        Poll::Ready(Ok(fill)) => {
                            drop(fill);
                            break;
                        }
                        Poll::Pending => std::thread::yield_now(),
                        _ => panic!("lost release"),
                    }
                },
                _ => panic!("unexpected timeout"),
            }
        }
        // A canceled ticket does not retire the compute holder itself.
        let mut wait = pool.wait_private_fill(deadline());
        loop {
            match wait.poll(Waker::noop()) {
                Poll::Ready(Ok(fill)) => {
                    drop(fill);
                    break;
                }
                Poll::Pending => std::thread::yield_now(),
                _ => panic!("capacity did not recover"),
            }
        }
        pool.assert_recovered();
    }
    drop(send);
    worker.join().unwrap();
}

#[test]
fn wake_callback_can_reenter_and_panic_without_losing_a_grant() {
    struct Reenter(TestPoolLink);
    impl Wake for Reenter {
        fn wake(self: Arc<Self>) {
            assert!(self.0.for_worker().private_fill().is_err());
            panic!("wake callback panic");
        }
    }
    let pool = io_test_pool(1);
    let held = pool.private_fill().unwrap();
    let mut wait = pool.wait_private_fill(deadline());
    let waker = Waker::from(Arc::new(Reenter(pool.test_link())));
    assert!(wait.poll(&waker).is_pending());
    drop(held);
    drop(ready(&mut wait, Waker::noop()));
    pool.assert_recovered();
}

#[test]
#[ignore = "opt-in buffer acquisition microbenchmark; run with --release --ignored --nocapture"]
fn acquisition_throughput() {
    use std::hint::black_box;
    let pool = io_test_pool(1);
    let iterations = 1_000_000;
    let end = Instant::now() + Duration::from_secs(120);
    let start = Instant::now();
    for _ in 0..iterations {
        drop(black_box(pool.private_fill().unwrap()));
    }
    let immediate = start.elapsed();
    let start = Instant::now();
    for _ in 0..iterations {
        drop(black_box(ready(
            &mut pool.wait_private_fill(end),
            Waker::noop(),
        )));
    }
    let uncontended = start.elapsed();
    let (count, waker) = counter();
    let start = Instant::now();
    for _ in 0..iterations {
        let held = pool.private_fill().unwrap();
        let mut wait = pool.wait_private_fill(end);
        assert!(wait.poll(&waker).is_pending());
        drop(held);
        drop(black_box(ready(&mut wait, &waker)));
    }
    let handoff = start.elapsed();
    assert_eq!(count.0.load(Ordering::Relaxed), iterations);
    eprintln!(
        "buffer ns/op: immediate={:.1} uncontended={:.1} contended-handoff={:.1}; exactly one wake per handoff",
        immediate.as_nanos() as f64 / iterations as f64,
        uncontended.as_nanos() as f64 / iterations as f64,
        handoff.as_nanos() as f64 / iterations as f64
    );
    pool.assert_recovered();
}
