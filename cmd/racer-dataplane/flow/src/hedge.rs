//! Shared speculative capacity and explicitly driven alarms.
//!
//! Costs and due times belong to the caller. An elapsed alarm does not release
//! capacity: retain the permit until all submitted work reaches its fence.
use crate::{Error, Result};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    task::{Context, Poll, Waker},
    time::Instant,
};

struct Alarm {
    due: Instant,
    cost: usize,
    wake: Option<Waker>,
}

#[derive(Default)]
struct State {
    next: u64,
    used: usize,
    alarms: BTreeMap<u64, Alarm>,
}

/// Node/process-shared capacity, not a worker-local registry or rate limiter.
pub struct Hedges {
    slots: usize,
    capacity: usize,
    state: Mutex<State>,
}

#[must_use = "retain the permit until both contenders are fenced"]
pub struct Permit {
    owner: Arc<Hedges>,
    id: u64,
}

impl Hedges {
    pub fn new(slots: usize, capacity: usize) -> Arc<Self> {
        Arc::new(Self {
            slots,
            capacity,
            state: Mutex::new(State::default()),
        })
    }

    pub fn acquire(self: &Arc<Self>, cost: usize, due: Instant) -> Result<Permit> {
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        if state.alarms.len() >= self.slots || cost > self.capacity - state.used {
            return Err(Error::Overloaded);
        }
        let id = state.next.checked_add(1).ok_or(Error::Unavailable)?;
        state.next = id;
        state.used += cost;
        state.alarms.insert(
            id,
            Alarm {
                due,
                cost,
                wake: None,
            },
        );
        Ok(Permit {
            owner: self.clone(),
            id,
        })
    }

    /// Wake due registrations outside the shared lock, once per registration.
    pub fn poll(&self, now: Instant) {
        let wakes: Vec<_> = self
            .state
            .lock()
            .map(|mut state| {
                state
                    .alarms
                    .values_mut()
                    .filter(|a| now >= a.due)
                    .filter_map(|a| a.wake.take())
                    .collect()
            })
            .unwrap_or_default();
        for wake in wakes {
            wake.wake();
        }
    }
}

impl Permit {
    pub fn delay(&self, now: Instant, cx: &mut Context<'_>) -> Poll<()> {
        let mut state = self.owner.state.lock().expect("hedge alarm lock");
        let alarm = state.alarms.get_mut(&self.id).expect("live hedge alarm");
        if now >= alarm.due {
            Poll::Ready(())
        } else {
            alarm.wake = Some(cx.waker().clone());
            Poll::Pending
        }
    }
}

impl Drop for Permit {
    fn drop(&mut self) {
        if let Ok(mut state) = self.owner.state.lock()
            && let Some(alarm) = state.alarms.remove(&self.id)
        {
            state.used -= alarm.cost;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        sync::atomic::{AtomicUsize, Ordering},
        task::Wake,
        time::Duration,
    };

    #[derive(Default)]
    struct Counter(AtomicUsize);
    impl Wake for Counter {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    #[test]
    fn hedge_shared_slots_costs_and_release_are_independent_of_alarm() {
        fn shared<T: Send + Sync>() {}
        shared::<Hedges>();
        shared::<Permit>();
        let now = Instant::now();
        let owner = Hedges::new(2, 10);
        let a = owner.acquire(7, now).unwrap();
        assert!(matches!(owner.acquire(4, now), Err(Error::Overloaded)));
        let b = std::thread::scope(|s| s.spawn(|| owner.acquire(3, now)).join().unwrap().unwrap());
        assert!(matches!(owner.acquire(0, now), Err(Error::Overloaded)));
        owner.poll(now);
        assert!(matches!(owner.acquire(1, now), Err(Error::Overloaded)));
        drop(a);
        let c = owner.acquire(7, now).unwrap();
        drop((b, c));
        assert!(owner.acquire(10, now).is_ok());
        assert!(matches!(
            Hedges::new(0, 10).acquire(0, now),
            Err(Error::Overloaded)
        ));
        let max = Hedges::new(2, usize::MAX);
        let _all = max.acquire(usize::MAX, now).unwrap();
        assert!(matches!(max.acquire(1, now), Err(Error::Overloaded)));
    }

    #[test]
    fn hedge_alarm_replaces_waker_and_drop_removes_registration() {
        let now = Instant::now();
        let due = now + Duration::from_secs(1);
        let owner = Hedges::new(1, 1);
        let permit = owner.acquire(1, due).unwrap();
        let old = Arc::new(Counter::default());
        let current = Arc::new(Counter::default());
        assert!(
            permit
                .delay(now, &mut Context::from_waker(&Waker::from(old.clone())))
                .is_pending()
        );
        assert!(
            permit
                .delay(now, &mut Context::from_waker(&Waker::from(current.clone())))
                .is_pending()
        );
        owner.poll(now);
        assert_eq!(current.0.load(Ordering::SeqCst), 0);
        owner.poll(due);
        owner.poll(due);
        assert_eq!(old.0.load(Ordering::SeqCst), 0);
        assert_eq!(current.0.load(Ordering::SeqCst), 1);
        assert!(
            permit
                .delay(due, &mut Context::from_waker(Waker::noop()))
                .is_ready()
        );
        drop(permit);
        let permit = owner.acquire(1, due).unwrap();
        assert!(
            permit
                .delay(now, &mut Context::from_waker(&Waker::from(current.clone())))
                .is_pending()
        );
        drop(permit);
        owner.poll(due);
        assert_eq!(current.0.load(Ordering::SeqCst), 1);
    }
}
