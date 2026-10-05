//! Externally admitted worker-local deadline registrations and bounded wakeups.

use std::cell::{Cell, RefCell};
use std::collections::BTreeMap;
use std::rc::Rc;
use std::task::Waker;
use std::time::Instant;

#[derive(Default)]
pub struct Registry {
    next: Cell<u64>,
    pending: RefCell<BTreeMap<(Instant, u64), Rc<RefCell<State>>>>,
}
#[derive(Default)]
struct State {
    expired: bool,
    waker: Option<Waker>,
}
pub struct Registration {
    table: Rc<Registry>,
    key: (Instant, u64),
    state: Rc<RefCell<State>>,
}
impl Registry {
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
        let state = Rc::new(RefCell::new(State::default()));
        self.pending.borrow_mut().insert(key, state.clone());
        Ok(Registration {
            table: self.clone(),
            key,
            state,
        })
    }

    pub fn len(&self) -> usize {
        self.pending.borrow().len()
    }
    pub fn is_empty(&self) -> bool {
        self.pending.borrow().is_empty()
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
                let (_, state) = pending.pop_first().expect("due deadline");
                let mut state = state.borrow_mut();
                state.expired = true;
                if let Some(waker) = state.waker.take() {
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
    /// Scope policy is checked by the caller before consulting this observation.
    pub fn check(&self, waker: &Waker) -> crate::Result<()> {
        let mut state = self.state.borrow_mut();
        if state.expired {
            return Err(crate::Error::DeadlineExceeded);
        }
        if state.waker.as_ref().is_none_or(|old| !old.will_wake(waker)) {
            state.waker = Some(waker.clone());
        }
        Ok(())
    }
}
impl Drop for Registration {
    fn drop(&mut self) {
        self.table.pending.borrow_mut().remove(&self.key);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
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
}
