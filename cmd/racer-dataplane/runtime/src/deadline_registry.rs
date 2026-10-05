//! Externally admitted worker-local deadline registrations and bounded wakeups.

use std::cell::{Cell, RefCell};
use std::collections::BTreeMap;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};
use std::time::Instant;

type Pending = BTreeMap<(Instant, u64), Option<Waker>>;

#[derive(Default)]
pub struct Registry {
    next: Cell<u64>,
    pending: RefCell<Pending>,
}
pub struct Registration {
    table: Rc<Registry>,
    key: (Instant, u64),
}
impl Registry {
    #[cfg(any(test, feature = "test-util"))]
    pub fn with_next_id(next: u64) -> Self {
        Self {
            next: Cell::new(next),
            ..Self::default()
        }
    }

    /// Caller must bound registrations through admission before allocating one.
    pub fn register(self: &Rc<Self>, deadline: Instant) -> crate::Result<Registration> {
        let id = self.next.get();
        self.next
            .set(id.checked_add(1).ok_or(crate::Error::Overloaded)?);
        let key = (deadline, id);
        self.pending.borrow_mut().insert(key, None);
        Ok(Registration {
            table: self.clone(),
            key,
        })
    }

    pub fn len(&self) -> usize {
        self.pending.borrow().len()
    }
    pub fn is_empty(&self) -> bool {
        self.pending.borrow().is_empty()
    }

    /// Earliest pending deadline, including due entries left by a polling budget.
    pub fn next_deadline(&self) -> Option<Instant> {
        self.pending
            .borrow()
            .first_key_value()
            .map(|(key, _)| key.0)
    }

    pub fn poll(&self, now: Instant, budget: usize) -> usize {
        let mut wakers = Vec::new();
        let mut expired = 0;
        {
            let mut pending = self.pending.borrow_mut();
            while expired < budget
                && pending
                    .first_key_value()
                    .is_some_and(|(key, _)| key.0 <= now)
            {
                let (_, waker) = pending.pop_first().expect("due deadline");
                if let Some(waker) = waker {
                    wakers.push(waker);
                }
                expired += 1;
            }
        }
        for waker in wakers {
            waker.wake();
        }
        expired
    }
}
impl Registration {
    /// Observe driver-processed expiration and register the latest waiting task.
    /// This does not read the clock or apply scope policy. The owner must drive
    /// `Registry::poll`; callers choose cancellation and deadline error precedence.
    pub fn poll_expired(&self, cx: &mut Context<'_>) -> Poll<()> {
        // RawWaker clone/drop callbacks may reenter this registration or registry.
        let new = cx.waker().clone();
        let mut pending = self.table.pending.borrow_mut();
        let Some(waker) = pending.get_mut(&self.key) else {
            // Keys are never reused. Only expiry can remove a live handle's key.
            drop(pending);
            return Poll::Ready(());
        };
        let old = waker.replace(new);
        drop(pending);
        drop(old);
        Poll::Pending
    }

    /// Scope policy is checked by the caller before consulting this observation.
    pub fn check(&self, waker: &Waker) -> crate::Result<()> {
        match self.poll_expired(&mut Context::from_waker(waker)) {
            Poll::Ready(()) => Err(crate::Error::DeadlineExceeded),
            Poll::Pending => Ok(()),
        }
    }
}
impl Drop for Registration {
    fn drop(&mut self) {
        let removed = self.table.pending.borrow_mut().remove(&self.key);
        drop(removed);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deadlines_are_ordered_by_time_then_registration_and_drop_is_reentrant() {
        thread_local! {
            static TABLE: RefCell<Option<Rc<Registry>>> = const { RefCell::new(None) };
        }
        struct OnDrop;
        impl std::task::Wake for OnDrop {
            fn wake(self: std::sync::Arc<Self>) {
                panic!("replaced registration must not wake its retired owner");
            }
        }
        impl Drop for OnDrop {
            fn drop(&mut self) {
                TABLE.with(|slot| {
                    let table = slot.borrow().as_ref().unwrap().clone();
                    assert!(table.pending.try_borrow_mut().is_ok());
                });
            }
        }
        let table = Rc::new(Registry::default());
        let now = Instant::now();
        let late = table
            .register(now + std::time::Duration::from_secs(1))
            .unwrap();
        let first = table.register(now).unwrap();
        let second = table.register(now).unwrap();
        TABLE.with(|slot| slot.replace(Some(table.clone())));
        first
            .check(&Waker::from(std::sync::Arc::new(OnDrop)))
            .unwrap();
        first.check(Waker::noop()).unwrap();
        assert_eq!(table.poll(now, 1), 1);
        assert!(first.check(Waker::noop()).is_err());
        assert!(second.check(Waker::noop()).is_ok());
        assert!(late.check(Waker::noop()).is_ok());
        assert_eq!(table.poll(now, 8), 1);
        assert!(late.check(Waker::noop()).is_ok());
        TABLE.with(|slot| slot.take());
    }
    #[test]
    fn bounded_poll_drop_and_overflow() {
        let table = Rc::new(Registry::default());
        let now = Instant::now();
        let first = table.register(now).unwrap();
        let second = table.register(now).unwrap();
        assert_eq!(first.check(Waker::noop()), Ok(()));
        assert_eq!(table.poll(now, 0), 0);
        assert_eq!(table.poll(now, 1), 1);
        assert_eq!(
            first.check(Waker::noop()),
            Err(crate::Error::DeadlineExceeded)
        );
        assert_eq!(second.check(Waker::noop()), Ok(()));
        drop(second);
        assert!(table.is_empty());
        let table = Rc::new(Registry::with_next_id(u64::MAX));
        assert!(matches!(table.register(now), Err(crate::Error::Overloaded)));
        assert!(table.is_empty());
    }

    #[test]
    fn next_deadline_tracks_order_drop_and_budgeted_expiration() {
        let table = Rc::new(Registry::default());
        let now = Instant::now();
        let later = now + std::time::Duration::from_secs(1);
        assert_eq!(table.next_deadline(), None);
        let late = table.register(later).unwrap();
        assert_eq!(table.next_deadline(), Some(later));
        let first = table.register(now).unwrap();
        let second = table.register(now).unwrap();
        assert_eq!(table.next_deadline(), Some(now));
        assert_eq!(table.poll(now - std::time::Duration::from_nanos(1), 8), 0);
        assert_eq!(table.poll(now, 0), 0);
        assert_eq!(table.next_deadline(), Some(now));
        assert_eq!(table.poll(now, 1), 1);
        assert_eq!(table.next_deadline(), Some(now));
        let mut cx = Context::from_waker(Waker::noop());
        // Expiration does not require the handle to have been polled first.
        assert_eq!(first.poll_expired(&mut cx), Poll::Ready(()));
        assert_eq!(first.poll_expired(&mut cx), Poll::Ready(()));
        assert_eq!(second.poll_expired(&mut cx), Poll::Pending);
        drop(second);
        assert_eq!(table.next_deadline(), Some(later));
        drop(first);
        assert_eq!(table.next_deadline(), Some(later));
        assert_eq!(table.poll(later, 1), 1);
        assert_eq!(late.poll_expired(&mut cx), Poll::Ready(()));
        assert_eq!(table.next_deadline(), None);
        let removed = table.register(now).unwrap();
        drop(removed);
        assert_eq!(table.next_deadline(), None);
    }

    #[test]
    fn latest_waker_is_notified_once_and_drop_releases_it() {
        use crate::test_util::WakeCounter;
        use std::sync::Arc;
        let table = Rc::new(Registry::default());
        let now = Instant::now();
        let registration = table.register(now).unwrap();
        let old = Arc::new(WakeCounter::default());
        let latest = Arc::new(WakeCounter::default());
        for count in [&old, &latest] {
            assert!(
                registration
                    .poll_expired(&mut Context::from_waker(&Waker::from(count.clone())))
                    .is_pending()
            );
        }
        assert_eq!(table.poll(now, 1), 1);
        assert_eq!(old.count(), 0);
        assert_eq!(latest.count(), 1);
        assert_eq!(table.poll(now, 1), 0);
        assert_eq!(latest.count(), 1);
        let registration = table.register(now).unwrap();
        let weak = Arc::downgrade(&latest);
        registration.check(&Waker::from(latest)).unwrap();
        assert!(weak.upgrade().is_some());
        drop(registration);
        assert!(weak.upgrade().is_none());
    }

    #[test]
    fn waking_and_removing_entries_allow_registry_reentry() {
        thread_local! {
            static TABLE: RefCell<Option<Rc<Registry>>> = const { RefCell::new(None) };
        }
        struct Reentrant;
        impl Reentrant {
            fn access() {
                TABLE.with(|slot| {
                    let table = slot.borrow().as_ref().unwrap().clone();
                    assert!(table.is_empty());
                    let nested = table.register(Instant::now()).unwrap();
                    assert_eq!(table.len(), 1);
                    drop(nested);
                });
            }
        }
        impl std::task::Wake for Reentrant {
            fn wake(self: std::sync::Arc<Self>) {
                Self::access();
            }
        }
        impl Drop for Reentrant {
            fn drop(&mut self) {
                Self::access();
            }
        }
        let table = Rc::new(Registry::default());
        TABLE.with(|slot| slot.replace(Some(table.clone())));
        let now = Instant::now();
        let registration = table.register(now).unwrap();
        registration
            .check(&Waker::from(std::sync::Arc::new(Reentrant)))
            .unwrap();
        assert_eq!(table.poll(now, 1), 1);
        drop(registration);
        let registration = table.register(now).unwrap();
        registration
            .check(&Waker::from(std::sync::Arc::new(Reentrant)))
            .unwrap();
        drop(registration);
        assert!(table.is_empty());
        TABLE.with(|slot| slot.take());
    }
}
