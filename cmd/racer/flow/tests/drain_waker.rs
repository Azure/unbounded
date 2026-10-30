//! Drain registration must not invoke waker callbacks under the owner borrow.

use flow_control::coalesce::flight::{self, Table};
use std::cell::{Cell, RefCell};
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::task::{RawWaker, RawWakerVTable, Wake, Waker};

thread_local! {
    static ON_CLONE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
    static ON_DROP: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
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
    |_| {},
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
    drop(flight::update(owner, |table, _| {
        table.register_drain(waker)
    }));
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
