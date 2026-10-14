//! Drain registration must not invoke waker callbacks under the owner borrow.

use flow_control::coalesce::flight::{self, Entry, Table, Txn};
use std::cell::{Cell, RefCell};
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::task::{RawWaker, RawWakerVTable, Wake, Waker};
use std::time::Instant;

thread_local! {
    static ON_CLONE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
    static ON_DROP: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
    static ON_WAKE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
}

/// No pointer is dereferenced or owned; callbacks belong to the current thread.
static VTABLE: RawWakerVTable = RawWakerVTable::new(
    |_| {
        let callback = ON_CLONE.with(|slot| slot.borrow_mut().take());
        if let Some(callback) = callback {
            callback();
        }
        raw_waker()
    },
    |_| {
        let callback = ON_WAKE.with(|slot| slot.borrow_mut().take());
        if let Some(callback) = callback {
            callback();
        }
    },
    |_| {},
    |_| {
        let callback = ON_DROP.with(|slot| slot.borrow_mut().take());
        if let Some(callback) = callback {
            callback();
        }
    },
);

fn raw_waker() -> RawWaker {
    RawWaker::new(std::ptr::null(), &VTABLE)
}

fn callback_waker() -> Waker {
    // SAFETY: The vtable owns no data and only accesses thread-local callbacks.
    unsafe { Waker::from_raw(raw_waker()) }
}

fn register(owner: &RefCell<Table<u32, ()>>, waker: &Waker) {
    let waker = waker.clone();
    flight::update(owner, |table, txn| txn.register_drain(table, waker));
}

#[test]
fn drain_waker_clone_can_reenter_owner() {
    let owner = Rc::new(RefCell::new(Table::<u32, ()>::default()));
    let called = Rc::new(Cell::new(false));
    ON_CLONE.with(|slot| {
        slot.replace(Some(Box::new({
            let owner = owner.clone();
            let called = called.clone();
            move || {
                flight::update(&owner, |table, _| assert!(table.is_empty()));
                called.set(true);
            }
        })));
    });
    register(&owner, &callback_waker());
    assert!(called.get());
}

#[test]
fn drain_waker_replacement_drop_can_reenter_owner() {
    let owner = Rc::new(RefCell::new(Table::<u32, ()>::default()));
    register(&owner, &callback_waker());
    let called = Rc::new(Cell::new(false));
    ON_DROP.with(|slot| {
        slot.replace(Some(Box::new({
            let owner = owner.clone();
            let called = called.clone();
            move || {
                flight::update(&owner, |table, _| assert!(table.is_empty()));
                called.set(true);
            }
        })));
    });
    register(&owner, Waker::noop());
    assert!(called.get());
}

#[derive(Default)]
struct CountWake(AtomicUsize);

impl Wake for CountWake {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}

#[test]
fn drain_waker_notifies_latest_target_once() {
    let owner = RefCell::new(Table::<u32, ()>::default());
    let old = Arc::new(CountWake::default());
    let latest = Arc::new(CountWake::default());
    let latest_waker = Waker::from(latest.clone());
    register(&owner, &Waker::from(old.clone()));
    register(&owner, &latest_waker);
    register(&owner, &latest_waker);
    flight::update(&owner, |table, wakes| {
        table.notify_drain(wakes);
        table.notify_drain(wakes);
    });
    assert_eq!(old.0.load(Ordering::Relaxed), 0);
    assert_eq!(latest.0.load(Ordering::Relaxed), 1);
    register(&owner, &latest_waker);
    flight::update(&owner, |table, wakes| table.notify_drain(wakes));
    assert_eq!(latest.0.load(Ordering::Relaxed), 2);
}

/// An owned value whose destruction must be deferred until after the borrow.
struct Retired(Option<Box<dyn FnOnce()>>);

impl Drop for Retired {
    fn drop(&mut self) {
        if let Some(callback) = self.0.take() {
            callback();
        }
    }
}

#[test]
fn transaction_releases_before_notify_on_return_and_unwind() {
    for panic in [false, true] {
        let owner = Rc::new(RefCell::new(0));
        let released = Rc::new(Cell::new(false));
        let notified = Rc::new(Cell::new(false));
        let retired = Retired(Some(Box::new({
            let owner = owner.clone();
            let released = released.clone();
            move || {
                assert_eq!(*owner.borrow_mut(), 1);
                released.set(true);
            }
        })));
        ON_WAKE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new({
                let owner = owner.clone();
                let released = released.clone();
                let notified = notified.clone();
                move || {
                    assert!(released.get(), "release must precede notification");
                    *owner.borrow_mut() = 2;
                    notified.set(true);
                }
            }));
        });
        let now = Instant::now();
        let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            flight::update_at(&owner, now, |state, txn| {
                assert_eq!(txn.now(), now);
                *state = 1;
                txn.retire(retired);
                txn.push(callback_waker());
                assert!(!released.get());
                assert!(!notified.get());
                assert!(!panic, "transaction callback failed");
            });
        }));
        assert_eq!(outcome.is_err(), panic);
        assert!(released.get());
        assert!(notified.get());
        assert_eq!(*owner.borrow(), 2);
    }
}

/// Public replacements retire callback owners before waking, even after hook panic.
#[test]
fn state_replacements_retire_before_wake_on_return_and_unwind() {
    use flight::state::{Phase, State, WaiterPolicy};

    struct Policy {
        due: Instant,
        _retired: Retired,
    }

    impl WaiterPolicy for Policy {
        type Error = ();

        fn check(&self, _: Instant) -> Option<()> {
            None
        }

        fn deadline(&self) -> Instant {
            self.due
        }
    }

    for panic in [false, true] {
        let owner = Rc::new(RefCell::new(
            State::<Retired, Retired, (), Policy>::default(),
        ));
        let released = Rc::new(Cell::new(0));
        let notified = Rc::new(Cell::new(false));
        let old_due = Instant::now();
        let new_due = old_due + std::time::Duration::from_secs(1);
        let probe = || {
            Retired(Some(Box::new({
                let owner = Rc::downgrade(&owner);
                let released = released.clone();
                move || {
                    let owner = owner.upgrade().unwrap();
                    let state = owner.borrow_mut();
                    assert!(matches!(state.phase(), Phase::RetryPending));
                    assert!(state.partial().is_none());
                    assert!(!state.deadlines().contains_key(&(old_due, 1)));
                    assert!(state.deadlines().contains_key(&(new_due, 1)));
                    released.set(released.get() + 1);
                }
            })))
        };
        flight::update(&owner, |state, txn| {
            state.replace_phase(Phase::Complete(probe()), txn);
            state.replace_partial(Some(probe()), txn);
            assert!(
                state
                    .register(
                        1,
                        Policy {
                            due: old_due,
                            _retired: probe()
                        },
                        true,
                        true
                    )
                    .is_ok()
            );
            state.register_waker(1, callback_waker(), txn);
        });
        ON_DROP.with(|slot| {
            *slot.borrow_mut() = Some(Box::new({
                let owner = owner.clone();
                let released = released.clone();
                move || {
                    assert!(owner.try_borrow_mut().is_ok());
                    released.set(released.get() + 1);
                }
            }));
        });
        ON_WAKE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new({
                let owner = owner.clone();
                let released = released.clone();
                let notified = notified.clone();
                move || {
                    assert!(owner.try_borrow_mut().is_ok());
                    assert_eq!(released.get(), 5);
                    notified.set(true);
                }
            }));
        });
        let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            flight::update(&owner, |state, txn| {
                // Queue first to prove all retirement still precedes notification.
                txn.push(callback_waker());
                state.replace_phase(Phase::RetryPending, txn);
                state.replace_partial(None, txn);
                state
                    .replace_policy(
                        1,
                        Policy {
                            due: new_due,
                            _retired: Retired(None),
                        },
                        txn,
                    )
                    .unwrap();
                assert_eq!(
                    state.replace_policy(
                        99,
                        Policy {
                            due: new_due,
                            _retired: probe()
                        },
                        txn
                    ),
                    Err(flight::Stale)
                );
                state.register_waker(1, Waker::noop().clone(), txn);
                assert_eq!(released.get(), 0);
                assert!(!notified.get());
                assert!(!panic, "replacement hook failed");
            });
        }));
        assert_eq!(outcome.is_err(), panic);
        assert_eq!(released.get(), 5);
        assert!(notified.get());
    }
}

#[test]
fn waiter_waker_registration_retires_redundant_and_stale_targets() {
    use flight::state::{State, WaiterPolicy};

    struct Policy;
    impl WaiterPolicy for Policy {
        type Error = ();
        fn check(&self, _: Instant) -> Option<()> {
            None
        }
        fn deadline(&self) -> Instant {
            Instant::now()
        }
    }

    let owner = Rc::new(RefCell::new(State::<(), (), (), Policy>::default()));
    flight::update(&owner, |state, txn| {
        assert!(state.register(1, Policy, true, true).is_ok());
        state.register_waker(1, callback_waker(), txn);
    });
    for id in [1, 99] {
        let dropped = Rc::new(Cell::new(false));
        ON_DROP.with(|slot| {
            *slot.borrow_mut() = Some(Box::new({
                let owner = owner.clone();
                let dropped = dropped.clone();
                move || {
                    assert!(owner.borrow_mut().waiters()[&1].waker().is_some());
                    dropped.set(true);
                }
            }));
        });
        flight::update(&owner, |state, txn| {
            state.register_waker(id, callback_waker(), txn);
            assert!(!dropped.get());
        });
        assert!(dropped.get());
    }
}

/// Hooks retire application values through the same transaction as table cleanup.
struct HookEntry {
    retired: Option<Retired>,

    now: Instant,

    panic: bool,
}

impl Entry for HookEntry {
    fn refresh(&mut self, txn: &mut Txn) {
        assert_eq!(txn.now(), self.now);
        txn.retire(self.retired.take());
        assert!(!self.panic, "refresh failed after retirement");
    }

    fn quiescent(&self) -> bool {
        self.retired.is_none()
    }
}

struct HookAccess<'a>(&'a mut HookEntry);

impl HookAccess<'_> {
    fn refresh(&mut self, txn: &mut Txn) {
        self.0.refresh(txn);
    }

    fn allow_refresh(&mut self) {
        self.0.panic = false;
    }
}

impl flight::Access for HookEntry {
    type View<'a> = HookAccess<'a>;

    fn access(&mut self) -> HookAccess<'_> {
        HookAccess(self)
    }
}

#[test]
fn lifecycle_hooks_retire_before_drain_and_survive_hook_unwind() {
    for (stop, panic) in [(false, false), (true, false), (false, true), (true, true)] {
        let now = Instant::now();
        let owner = Rc::new(RefCell::new(Table::<u32, HookEntry>::default()));
        let released = Rc::new(Cell::new(0));
        for key in 1..=2 {
            let retired = Retired(Some(Box::new({
                let owner = Rc::downgrade(&owner);
                let released = released.clone();
                move || {
                    assert!(owner.upgrade().unwrap().try_borrow_mut().is_ok());
                    released.set(released.get() + 1);
                }
            })));
            assert!(
                owner
                    .borrow_mut()
                    .insert(
                        key,
                        HookEntry {
                            retired: Some(retired),
                            now,
                            panic: panic && key == 2,
                        }
                    )
                    .is_ok()
            );
        }
        let notified = Rc::new(Cell::new(false));
        ON_WAKE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new({
                let owner = owner.clone();
                let released = released.clone();
                let notified = notified.clone();
                move || {
                    assert!(released.get() >= 1);
                    assert!(owner.try_borrow_mut().is_ok());
                    notified.set(true);
                }
            }));
        });
        let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            flight::update_at(&owner, now, |table, txn| {
                txn.push(callback_waker());
                if stop {
                    table.stop(txn, |mut entry, txn| entry.refresh(txn));
                } else {
                    txn.sweep(table, 2);
                }
            });
        }));
        assert_eq!(outcome.is_err(), panic);
        assert!(notified.get());
        if !stop {
            assert_eq!(released.get(), 2);
            assert_eq!(owner.borrow().len(), usize::from(panic));
        }
        // Finish any unvisited stop hooks before dropping the owner.
        flight::update_at(&owner, now, |table, txn| {
            table.stop(txn, |mut entry, txn| {
                entry.allow_refresh();
                entry.refresh(txn);
            });
            txn.sweep(table, 2);
        });
        assert!(owner.borrow().is_empty());
        assert_eq!(released.get(), 2);
    }
}

#[test]
fn removal_stages_map_sweep_and_temporary_keys_before_drain() {
    thread_local! {
        static KEY_DROP: RefCell<Option<Box<dyn Fn()>>> = RefCell::new(None);
    }
    #[derive(Clone, Hash, Eq, PartialEq)]
    struct Key(u32);
    impl Drop for Key {
        fn drop(&mut self) {
            KEY_DROP.with(|slot| {
                if let Some(callback) = slot.borrow().as_ref() {
                    callback();
                }
            });
        }
    }
    for sweep in [false, true] {
        let now = Instant::now();
        let owner = Rc::new(RefCell::new(Table::<Key, HookEntry>::default()));
        let drops = Rc::new(Cell::new(0));
        assert!(
            owner
                .borrow_mut()
                .insert(
                    Key(1),
                    HookEntry {
                        retired: None,
                        now,
                        panic: false,
                    }
                )
                .is_ok()
        );
        KEY_DROP.with(|slot| {
            *slot.borrow_mut() = Some(Box::new({
                let owner = owner.clone();
                let drops = drops.clone();
                move || {
                    assert!(owner.borrow_mut().is_empty());
                    drops.set(drops.get() + 1);
                }
            }));
        });
        let notified = Rc::new(Cell::new(false));
        ON_WAKE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new({
                let drops = drops.clone();
                let notified = notified.clone();
                move || {
                    assert_eq!(drops.get(), if sweep { 3 } else { 2 });
                    notified.set(true);
                }
            }));
        });
        let key = Key(1);
        flight::update_at(&owner, now, |table, txn| {
            txn.register_drain(table, callback_waker());
            if sweep {
                txn.sweep(table, 1);
            } else {
                assert!(txn.remove_quiescent(table, &key));
                table.notify_drain(txn);
            }
            assert_eq!(drops.get(), 0);
        });
        assert!(notified.get());
        KEY_DROP.with(|slot| slot.borrow_mut().take());
    }
}
