//! Public ownership boundaries and synchronous reentrant notification contracts.

use flow_control::coalesce::flight::{self, Entry, Operations, Stale};
use flow_control::coalesce::{CapacityError, Event, Limits, Table, shared};
use futures::executor::block_on;
use std::cell::{Cell, RefCell};
use std::future::Future;
use std::rc::Rc;
use std::sync::Arc;
use std::task::{Context, Poll, Wake, Waker};

thread_local! {
    /// Callback invoked only by synchronous wakes on the current test worker.
    static ON_WAKE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
}

/// Safe thread-local callback dispatch; no non-Send data enters the Waker itself.
struct Reenter;

impl Wake for Reenter {
    /// Invoke the current worker's callback after releasing its registry borrow.
    fn wake(self: Arc<Self>) {
        let callback = ON_WAKE.with(|slot| slot.borrow_mut().take());
        if let Some(callback) = callback {
            callback();
        }
    }
}

/// Install a callback and return a wake target that dispatches it synchronously.
fn on_wake(callback: impl FnOnce() + 'static) -> Waker {
    ON_WAKE.with(|slot| assert!(slot.borrow_mut().replace(Box::new(callback)).is_none()));
    Waker::from(Arc::new(Reenter))
}

/// Build a table with enough attempts to test retry and final-owner-drop elections.
fn table(waiters: usize) -> Rc<Table<u32, u32>> {
    Rc::new(Table::new(
        Limits {
            waiters_per_cohort: waiters,
            attempts_per_cohort: 4,
        },
        99,
    ))
}

/// Joining and cloning handles do not require a cloneable result value.
#[test]
fn registration_clones_neither_keys_nor_results() {
    /// Key whose clone records the allocation-time copy only.
    #[derive(Eq, PartialEq)]
    struct Key(Rc<Cell<usize>>);

    impl Clone for Key {
        /// Count each actual key copy.
        fn clone(&self) -> Self {
            self.0.set(self.0.get() + 1);
            Self(self.0.clone())
        }
    }

    impl std::hash::Hash for Key {
        /// Give the single logical test key a stable hash.
        fn hash<H: std::hash::Hasher>(&self, state: &mut H) {
            // This test uses exactly one logical key, independent of the counter.
            state.write_u8(0);
        }
    }

    /// Deliberately non-Clone result; only polling needs result cloning.
    struct ResultValue;

    let table = Rc::new(Table::<Key, ResultValue>::new(
        Limits {
            waiters_per_cohort: 1,
            attempts_per_cohort: 1,
        },
        ResultValue,
    ));
    let copies = Rc::new(Cell::new(0));
    let first = table.join(Key(copies.clone()), 1).unwrap();
    assert_eq!(copies.get(), 1);
    let second = first.clone();
    let third = second.clone();
    assert_eq!(copies.get(), 1);
    first.finish(ResultValue);
    drop((first, second));
    assert_eq!(table.registration_count(), 1);
    drop(third);
    assert_eq!(table.registration_count(), 0);
}

/// Arbitrarily many cloned handles consume one charge until their final drop.
#[test]
fn many_clones_retain_completed_capacity_and_do_not_remove_replacements() {
    let table = table(2);
    let first = table.join(1, 1).unwrap();
    let mut clones = (0..32).map(|_| first.clone()).collect::<Vec<_>>();
    first.finish(7);
    let next = table.join(1, 1).unwrap();
    drop(first);
    while clones.len() > 1 {
        drop(clones.pop());
        assert_eq!(table.registration_count(), 2);
        assert!(matches!(table.join(1, 1), Err(CapacityError)));
    }
    assert!(clones[0].is_only_handle());
    assert_eq!(
        clones[0].event(Waker::noop()),
        Poll::Ready(Event::Complete(7))
    );
    drop(clones);
    assert_eq!(table.registration_count(), 1);
    assert_eq!(table.active_count(), 1);
    let follower = table.join(1, 1).unwrap();
    assert_eq!(next.event(Waker::noop()), Poll::Ready(Event::Lead));
    drop((next, follower));
    assert_eq!(table.registration_count(), 0);
    assert_eq!(table.active_count(), 0);
}

/// Retry and final-owner drop release all borrows before reentrant leader election.
#[test]
fn retry_and_final_drop_allow_reentrant_election() {
    for drop_leader in [false, true] {
        let table = table(2);
        let leader = table.join(1, 1).unwrap();
        let follower = table.join(1, 1).unwrap();
        assert_eq!(leader.event(Waker::noop()), Poll::Ready(Event::Lead));
        let called = Rc::new(Cell::new(false));
        let waker = on_wake({
            let table = table.clone();
            let follower = follower.clone();
            let called = called.clone();
            move || {
                assert_eq!(table.registration_count(), if drop_leader { 1 } else { 2 });
                assert_eq!(follower.event(Waker::noop()), Poll::Ready(Event::Lead));
                called.set(true);
            }
        });
        assert!(follower.event(&waker).is_pending());
        if drop_leader {
            drop(leader);
        } else {
            leader.retry();
        }
        assert!(called.get());
    }
}

/// Completion removes old admission before any reader wake can admit new work.
#[test]
fn finish_allows_reentrant_admission_before_old_readers_detach() {
    let table = table(3);
    let leader = table.join(1, 1).unwrap();
    let follower = table.join(1, 1).unwrap();
    assert_eq!(leader.event(Waker::noop()), Poll::Ready(Event::Lead));
    let replacement = Rc::new(RefCell::new(None));
    let waker = on_wake({
        let table = table.clone();
        let follower = follower.clone();
        let replacement = replacement.clone();
        move || {
            assert_eq!(table.active_count(), 0);
            assert_eq!(
                follower.event(Waker::noop()),
                Poll::Ready(Event::Complete(7))
            );
            replacement.replace(Some(table.join(1, 1).unwrap()));
        }
    });
    assert!(follower.event(&waker).is_pending());
    leader.finish(7);
    assert!(replacement.borrow().is_some());
    drop((leader, follower));
    assert_eq!(table.active_count(), 1);
    assert_eq!(table.registration_count(), 1);
}

/// Shared result notification can synchronously start replacement work.
#[test]
fn shared_completion_wakes_after_removal_and_keeps_new_owner() {
    let table = Rc::new(shared::Table::default());
    let (mut receive, complete) = table.start(1, 99);
    let replacement = Rc::new(RefCell::new(None));
    let waker = on_wake({
        let table = table.clone();
        let replacement = replacement.clone();
        move || {
            assert!(table.is_empty());
            replacement.replace(Some(table.start(1, 99)));
        }
    });
    assert!(
        std::pin::Pin::new(&mut receive)
            .poll(&mut Context::from_waker(&waker))
            .is_pending()
    );
    complete.finish(7);
    assert_eq!(block_on(receive), 7);
    let (receive, complete) = replacement.borrow_mut().take().unwrap();
    assert_eq!(table.len(), 1);
    drop(complete);
    assert_eq!(block_on(receive), 99);
    assert_eq!(
        table.len(),
        1,
        "sender loss cannot pretend execution completed"
    );
}

/// Resource whose destructor can synchronously inspect its owning table.
struct ReentrantResource(Option<Box<dyn FnOnce()>>);

impl Drop for ReentrantResource {
    /// Reenter the owner while its operation tombstone must still be present.
    fn drop(&mut self) {
        self.0.take().unwrap()();
    }
}

/// Only operation completion makes this deliberately waiter-free entry removable.
#[derive(Default)]
struct OwnedEntry(Operations<ReentrantResource>);

impl Entry for OwnedEntry {
    /// This fixture has no caller policy to refresh.
    fn refresh(&mut self, _: &mut Vec<Waker>) {}

    /// Require explicit completion of every retained operation.
    fn quiescent(&self) -> bool {
        self.0.is_empty()
    }
}

/// Resource drop reentrancy cannot erase the tombstone, and drain wakes run unlocked.
#[test]
fn completion_tombstone_survives_reentrant_destructor_and_shutdown() {
    let table = Rc::new(RefCell::new(flight::Table::<u32, OwnedEntry>::default()));
    let dropped = Rc::new(Cell::new(false));
    let resource = ReentrantResource(Some(Box::new({
        let table = Rc::downgrade(&table);
        let dropped = dropped.clone();
        move || {
            let table = table.upgrade().unwrap();
            flight::update(&table, |table, wakes| {
                table.sweep(1, wakes);
                assert_eq!(table.len(), 1);
                assert!(!table.remove_quiescent(&1));
            });
            dropped.set(true);
        }
    })));
    let id = flight::update(&table, |table, _| {
        assert_eq!(table.next_waiter_id(), Ok(1));
        assert_eq!(table.next_waiter_id(), Ok(2));
        let id = table.next_operation_id().unwrap();
        table.insert(1, OwnedEntry::default());
        table.get_mut(&1).unwrap().0.insert(id, resource);
        assert_eq!(table.get_mut(&1).unwrap().0.complete(id), Err(Stale));
        id
    });
    flight::update(&table, |table, wakes| table.stop(wakes, |_, _| {}));
    assert!(table.borrow().is_stopping());
    let resource = flight::update(&table, |table, _| {
        table.get_mut(&1).unwrap().0.take(id).unwrap()
    });
    drop(resource);
    assert!(dropped.get());
    assert_eq!(table.borrow().len(), 1);
    let notified = Rc::new(Cell::new(false));
    let waker = on_wake({
        let table = table.clone();
        let notified = notified.clone();
        move || {
            assert!(table.borrow().is_empty());
            notified.set(true);
        }
    });
    flight::update(&table, |table, wakes| {
        table.register_drain(&waker);
        table.get_mut(&1).unwrap().0.complete(id).unwrap();
        table.sweep(1, wakes);
    });
    assert!(notified.get());
}
