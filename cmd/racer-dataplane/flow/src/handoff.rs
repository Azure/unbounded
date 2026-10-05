//! Round-robin handoff bounded by target-owned reservations, not queue slots.
use crate::{Error, Result};
use std::{
    collections::VecDeque,
    sync::{Arc, Mutex},
    task::Waker,
};

/// The admission source bounds all outstanding reservations, including queued
/// items and offers not yet delivered. Callbacks must not reenter the handoff.
pub trait Admission {
    type Reservation;
    fn register(&self, waker: &Waker);
    fn reserve(&self) -> Result<Self::Reservation>;
}
struct Target<A, T> {
    admission: Option<A>,
    queue: VecDeque<T>,
    waker: Option<Waker>,
    closed: bool,
}
struct State<K, A, T> {
    targets: Vec<(K, Target<A, T>)>,
    cursor: usize,
}
pub struct Handoff<K, A, T>(Mutex<State<K, A, T>>);
pub struct Offer<K, A: Admission, T> {
    handoff: Arc<Handoff<K, A, T>>,
    target: usize,
    reservation: A::Reservation,
}
impl<K: Eq + Clone, A: Admission, T> Handoff<K, A, T> {
    pub fn new(keys: &[K]) -> Self {
        Self(Mutex::new(State {
            targets: keys
                .iter()
                .map(|key| {
                    (
                        key.clone(),
                        Target {
                            admission: None,
                            queue: VecDeque::new(),
                            waker: None,
                            closed: false,
                        },
                    )
                })
                .collect(),
            cursor: 0,
        }))
    }
    pub fn install(&self, key: &K, admission: A) -> Result<()> {
        let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
        let target = &mut state
            .targets
            .iter_mut()
            .find(|(k, _)| k == key)
            .ok_or(Error::InvalidInput)?
            .1;
        if target.admission.is_some() || target.closed {
            return Err(Error::InvalidInput);
        }
        target.admission = Some(admission);
        Ok(())
    }
    pub fn reserve(self: &Arc<Self>, waker: &Waker) -> Result<Offer<K, A, T>> {
        let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
        for offset in 0..state.targets.len() {
            let index = (state.cursor + offset) % state.targets.len();
            let (_, target) = &state.targets[index];
            if target.closed {
                continue;
            }
            let Some(admission) = &target.admission else {
                continue;
            };
            admission.register(waker);
            if let Ok(reservation) = admission.reserve() {
                state.cursor = (index + 1) % state.targets.len();
                return Ok(Offer {
                    handoff: self.clone(),
                    target: index,
                    reservation,
                });
            }
        }
        Err(Error::Overloaded)
    }
    pub fn pop_batch<const N: usize>(
        &self,
        key: &K,
        waker: &Waker,
        budget: usize,
    ) -> Result<[Option<T>; N]> {
        let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
        let target = &mut state
            .targets
            .iter_mut()
            .find(|(k, _)| k == key)
            .ok_or(Error::InvalidInput)?
            .1;
        if let Some(old) = &mut target.waker {
            old.clone_from(waker);
        } else {
            target.waker = Some(waker.clone());
        }
        Ok(std::array::from_fn(|index| {
            if index < budget {
                target.queue.pop_front()
            } else {
                None
            }
        }))
    }
    pub fn close(&self, key: &K) {
        let queued = {
            let mut state = self.0.lock().unwrap_or_else(|e| e.into_inner());
            let Some((_, target)) = state.targets.iter_mut().find(|(k, _)| k == key) else {
                return;
            };
            target.closed = true;
            std::mem::take(&mut target.queue)
        };
        drop(queued);
    }
}
impl<K, A: Admission, T> Offer<K, A, T> {
    /// Build the queued item only after checking that the target is still open.
    /// The item must retain the reservation for its entire admitted lifetime.
    /// `item` executes under the handoff lock and must not reenter it.
    pub fn deliver(self, item: impl FnOnce(A::Reservation) -> T) -> Result<()> {
        let mut state = self.handoff.0.lock().map_err(|_| Error::Unavailable)?;
        let target = &mut state.targets[self.target].1;
        if target.closed {
            return Err(Error::Unavailable);
        }
        target.queue.push_back(item(self.reservation));
        let waker = target.waker.clone();
        drop(state);
        if let Some(waker) = waker {
            waker.wake();
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    struct Quota(Arc<AtomicUsize>);
    struct Held(Arc<AtomicUsize>);
    impl Drop for Held {
        fn drop(&mut self) {
            self.0.fetch_sub(1, Ordering::SeqCst);
        }
    }
    impl Admission for Quota {
        type Reservation = Held;
        fn register(&self, _: &Waker) {}
        fn reserve(&self) -> Result<Held> {
            self.0
                .compare_exchange(0, 1, Ordering::SeqCst, Ordering::SeqCst)
                .map_err(|_| Error::Overloaded)?;
            Ok(Held(self.0.clone()))
        }
    }
    #[test]
    fn round_robin_reserves_before_delivery_and_releases_after_close() {
        let handoff = Arc::new(Handoff::<_, _, (u8, Held)>::new(&[1, 2]));
        let counts = [Arc::new(AtomicUsize::new(0)), Arc::new(AtomicUsize::new(0))];
        let waker = Waker::noop();
        assert!(matches!(handoff.reserve(waker), Err(Error::Overloaded)));
        for (key, count) in [1, 2].into_iter().zip(&counts) {
            handoff.install(&key, Quota(count.clone())).unwrap();
        }
        assert_eq!(
            handoff.install(&1, Quota(counts[0].clone())),
            Err(Error::InvalidInput)
        );
        let first = handoff.reserve(waker).unwrap();
        let second = handoff.reserve(waker).unwrap();
        assert!(matches!(handoff.reserve(waker), Err(Error::Overloaded)));
        first.deliver(|held| (7, held)).unwrap();
        assert!(
            handoff
                .pop_batch::<2>(&1, waker, 0)
                .unwrap()
                .iter()
                .all(Option::is_none)
        );
        let [item, empty] = handoff.pop_batch::<2>(&1, waker, 1).unwrap();
        assert_eq!(item.as_ref().unwrap().0, 7);
        assert!(empty.is_none());
        handoff.close(&1);
        assert_eq!(counts[0].load(Ordering::SeqCst), 1);
        drop(item);
        assert_eq!(counts[0].load(Ordering::SeqCst), 0);
        handoff.close(&2);
        assert_eq!(
            second.deliver(|_| panic!("closed target built item")),
            Err(Error::Unavailable)
        );
        assert_eq!(counts[1].load(Ordering::SeqCst), 0);
        assert!(matches!(
            handoff.pop_batch::<1>(&3, waker, 1),
            Err(Error::InvalidInput)
        ));
        assert!(matches!(
            Arc::new(Handoff::<u8, Quota, Held>::new(&[])).reserve(waker),
            Err(Error::Overloaded)
        ));
    }
    #[test]
    fn close_drains_queued_reservations_and_abandoned_offer_releases() {
        let handoff = Arc::new(Handoff::<_, _, Held>::new(&[1]));
        let count = Arc::new(AtomicUsize::new(0));
        handoff.install(&1, Quota(count.clone())).unwrap();
        drop(handoff.reserve(Waker::noop()).unwrap());
        assert_eq!(count.load(Ordering::SeqCst), 0);
        handoff
            .reserve(Waker::noop())
            .unwrap()
            .deliver(|held| held)
            .unwrap();
        handoff.close(&1);
        handoff.close(&1);
        handoff.close(&2);
        assert_eq!(count.load(Ordering::SeqCst), 0);
        assert_eq!(handoff.install(&1, Quota(count)), Err(Error::InvalidInput));
    }
}
