//! Shared adaptive admission with fenced permits and caller-classified outcomes.
use crate::{Error, Result};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

#[derive(Clone, Copy)]
pub struct Config {
    pub total: usize,
    pub per_key: usize,
    pub capacity: usize,
    pub backoff: Duration,
    pub recovery: Duration,
    pub retire_after: Duration,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Outcome {
    Verified,
    PeerFailure,
    LocalPressure,
    Neutral,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Event {
    Rejected,
    CircuitRejected,
    Accepted,
    Probe,
    LocalPressure,
    Verified,
    LinkFailure,
}
/// Callbacks run synchronously under the admission lock and must not reenter it.
pub trait Observer {
    fn event(&self, event: Event);
    fn active(&self, active: usize);
    fn limit(&self, limit: usize);
}
pub struct Adaptive<K, O> {
    config: Config,
    state: Mutex<State<K>>,
    observer: O,
    now: fn() -> Instant,
}
struct State<K> {
    active: usize,
    limit: usize,
    updated: Instant,
    peers: BTreeMap<K, Peer>,
}
struct Peer {
    active: usize,
    limit: usize,
    generation: u64,
    retry: Option<Instant>,
    probe: bool,
    updated: Instant,
}
pub struct Permit<K: Ord + Clone, O: Observer> {
    owner: Arc<Adaptive<K, O>>,
    key: K,
    generation: u64,
    probe: bool,
}
impl<K: Ord + Clone, O: Observer> Adaptive<K, O> {
    pub fn new(config: Config, observer: O, now: fn() -> Instant) -> Result<Arc<Self>> {
        if config.total == 0
            || config.per_key == 0
            || config.per_key > config.total
            || config.capacity == 0
        {
            return Err(Error::InvalidInput);
        }
        observer.limit(config.total);
        Ok(Arc::new(Self {
            config,
            observer,
            now,
            state: Mutex::new(State {
                active: 0,
                limit: config.total,
                updated: now(),
                peers: BTreeMap::new(),
            }),
        }))
    }
    pub fn hedge_available(&self, key: &K) -> bool {
        self.state.lock().is_ok_and(|s| {
            s.limit == self.config.total
                && s.active.saturating_add(1) < s.limit
                && s.peers.get(key).is_none_or(|p| {
                    p.retry.is_none()
                        && !p.probe
                        && p.limit == self.config.per_key
                        && p.active < p.limit
                })
        })
    }
    pub fn available(&self, key: &K) -> bool {
        let now = (self.now)();
        self.state.lock().is_ok_and(|s| {
            s.peers
                .get(key)
                .is_none_or(|p| !p.probe && p.retry.is_none_or(|at| now >= at))
        })
    }
    pub fn acquire(self: &Arc<Self>, key: &K) -> Result<Arc<Permit<K, O>>> {
        let now = (self.now)();
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        if state.active >= state.limit {
            self.observer.event(Event::Rejected);
            return Err(Error::Overloaded);
        }
        if !state.peers.contains_key(key) && state.peers.len() == self.config.capacity {
            let retired = state
                .peers
                .iter()
                .find(|(_, p)| {
                    p.active == 0
                        && !p.probe
                        && p.retry.is_none_or(|retry| now >= retry)
                        && now.saturating_duration_since(p.updated) >= self.config.retire_after
                })
                .map(|(k, _)| k.clone());
            if let Some(retired) = retired {
                state.peers.remove(&retired);
            } else {
                self.observer.event(Event::Rejected);
                return Err(Error::Overloaded);
            }
        }
        let peer = state.peers.entry(key.clone()).or_insert(Peer {
            active: 0,
            limit: self.config.per_key,
            generation: 0,
            retry: None,
            probe: false,
            updated: now,
        });
        if peer.probe || peer.retry.is_some_and(|at| now < at) {
            self.observer.event(Event::CircuitRejected);
            return Err(Error::Unavailable);
        }
        if peer.active >= peer.limit {
            self.observer.event(Event::Rejected);
            return Err(Error::Overloaded);
        }
        let probe = peer.retry.is_some();
        peer.probe = probe;
        peer.active += 1;
        let generation = peer.generation;
        state.active += 1;
        self.observer.active(state.active);
        self.observer.event(Event::Accepted);
        if probe {
            self.observer.event(Event::Probe);
        }
        Ok(Arc::new(Permit {
            owner: self.clone(),
            key: key.clone(),
            generation,
            probe,
        }))
    }
}
impl<K: Ord + Clone, O: Observer> Permit<K, O> {
    pub fn observe(&self, outcome: Outcome) {
        let now = (self.owner.now)();
        let Ok(mut state) = self.owner.state.lock() else {
            return;
        };
        let config = self.owner.config;
        if outcome == Outcome::LocalPressure {
            self.owner.observer.event(Event::LocalPressure);
            if now.saturating_duration_since(state.updated) >= config.backoff {
                state.limit = (state.limit / 2).max(1);
                state.updated = now;
                self.owner.observer.limit(state.limit);
            }
            return;
        }
        if outcome == Outcome::Verified
            && now.saturating_duration_since(state.updated) >= config.recovery
        {
            state.limit = state.limit.saturating_add(1).min(config.total);
            state.updated = now;
            self.owner.observer.limit(state.limit);
        }
        let peer = state
            .peers
            .get_mut(&self.key)
            .expect("live permit retains key");
        let event = match outcome {
            Outcome::Verified => Event::Verified,
            Outcome::PeerFailure => Event::LinkFailure,
            Outcome::LocalPressure => Event::LocalPressure,
            Outcome::Neutral => return,
        };
        self.owner.observer.event(event);
        if peer.generation != self.generation {
            return;
        }
        match outcome {
            Outcome::PeerFailure => {
                peer.limit = (peer.limit / 2).max(1);
                peer.generation = peer.generation.saturating_add(1);
                peer.retry = Some(now + config.backoff);
                peer.updated = now;
            }
            Outcome::Verified => {
                if peer.retry.is_some() && !self.probe {
                    return;
                }
                peer.retry = None;
                if now.saturating_duration_since(peer.updated) >= config.recovery {
                    peer.limit = peer.limit.saturating_add(1).min(config.per_key);
                    peer.updated = now;
                }
            }
            Outcome::LocalPressure | Outcome::Neutral => {}
        }
    }
}
impl<K: Ord + Clone, O: Observer> Drop for Permit<K, O> {
    fn drop(&mut self) {
        let Ok(mut state) = self.owner.state.lock() else {
            return;
        };
        let peer = state
            .peers
            .get_mut(&self.key)
            .expect("live permit retains key");
        peer.active -= 1;
        if self.probe {
            peer.probe = false;
            if peer.retry.is_some() {
                let now = (self.owner.now)();
                peer.retry = Some(now + self.owner.config.backoff);
                peer.updated = now;
            }
        }
        state.active -= 1;
        self.owner.observer.active(state.active);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[derive(Default)]
    struct Counts {
        events: Mutex<Vec<Event>>,
        active: Mutex<usize>,
        limit: Mutex<usize>,
    }
    impl Observer for Counts {
        fn event(&self, event: Event) {
            self.events.lock().unwrap().push(event);
        }
        fn active(&self, active: usize) {
            *self.active.lock().unwrap() = active;
        }
        fn limit(&self, limit: usize) {
            *self.limit.lock().unwrap() = limit;
        }
    }
    fn config() -> Config {
        Config {
            total: 4,
            per_key: 4,
            capacity: 2,
            backoff: Duration::from_millis(250),
            recovery: Duration::from_secs(1),
            retire_after: Duration::from_secs(60),
        }
    }
    #[test]
    fn fences_generation_exclusivity_and_local_pressure() {
        let owner = Adaptive::new(config(), Counts::default(), Instant::now).unwrap();
        assert!(owner.hedge_available(&1));
        let old = owner.acquire(&1).unwrap();
        let failed = owner.acquire(&1).unwrap();
        failed.observe(Outcome::PeerFailure);
        old.observe(Outcome::Verified);
        assert!(!owner.available(&1));
        let fence = failed.clone();
        drop((old, failed));
        assert_eq!(*owner.observer.active.lock().unwrap(), 1);
        drop(fence);
        owner.state.lock().unwrap().peers.get_mut(&1).unwrap().retry = Some(Instant::now());
        let probe = owner.acquire(&1).unwrap();
        assert!(matches!(owner.acquire(&1), Err(Error::Unavailable)));
        probe.observe(Outcome::Verified);
        assert!(!owner.available(&1));
        drop(probe);
        assert!(owner.available(&1));
        let work = owner.acquire(&2).unwrap();
        owner.state.lock().unwrap().updated = Instant::now() - config().backoff;
        work.observe(Outcome::LocalPressure);
        assert_eq!(*owner.observer.limit.lock().unwrap(), 2);
        assert!(owner.available(&2));
        owner.state.lock().unwrap().updated = Instant::now() - config().recovery;
        work.observe(Outcome::Verified);
        assert_eq!(*owner.observer.limit.lock().unwrap(), 3);
    }
    #[test]
    fn capacity_preserves_live_work_and_stale_backoff_then_retires_idle() {
        let owner = Adaptive::new(config(), Counts::default(), Instant::now).unwrap();
        let one = owner.acquire(&1).unwrap();
        let two = owner.acquire(&2).unwrap();
        assert!(matches!(owner.acquire(&3), Err(Error::Overloaded)));
        one.observe(Outcome::PeerFailure);
        drop(one);
        owner
            .state
            .lock()
            .unwrap()
            .peers
            .get_mut(&1)
            .unwrap()
            .updated = Instant::now() - Duration::from_secs(61);
        assert!(matches!(owner.acquire(&3), Err(Error::Overloaded)));
        assert!(owner.state.lock().unwrap().peers.contains_key(&1));
        owner.state.lock().unwrap().peers.get_mut(&1).unwrap().retry = Some(Instant::now());
        let probe = owner.acquire(&1).unwrap();
        drop(probe);
        assert!(!owner.available(&1));
        assert!(matches!(owner.acquire(&3), Err(Error::Overloaded)));
        drop(two);
        owner
            .state
            .lock()
            .unwrap()
            .peers
            .get_mut(&2)
            .unwrap()
            .updated = Instant::now() - Duration::from_secs(61);
        assert!(owner.acquire(&3).is_ok());
        assert_eq!(owner.state.lock().unwrap().peers.len(), 2);
    }
    #[test]
    fn validation_and_caps() {
        for (total, per_key, capacity) in [(0, 1, 1), (1, 0, 1), (1, 2, 1), (1, 1, 0)] {
            assert!(matches!(
                Adaptive::<u8, _>::new(
                    Config {
                        total,
                        per_key,
                        capacity,
                        ..config()
                    },
                    Counts::default(),
                    Instant::now
                ),
                Err(Error::InvalidInput)
            ));
        }
        let owner = Adaptive::new(
            Config {
                total: 2,
                per_key: 1,
                ..config()
            },
            Counts::default(),
            Instant::now,
        )
        .unwrap();
        let one = owner.acquire(&1).unwrap();
        assert!(matches!(owner.acquire(&1), Err(Error::Overloaded)));
        let two = owner.acquire(&2).unwrap();
        assert!(!owner.hedge_available(&3));
        assert!(matches!(owner.acquire(&3), Err(Error::Overloaded)));
        one.observe(Outcome::Neutral);
        drop((one, two));
        assert_eq!(*owner.observer.active.lock().unwrap(), 0);
    }

    #[test]
    fn full_width_limits_recover_without_overflow() {
        let owner = Adaptive::new(
            Config {
                total: usize::MAX,
                per_key: usize::MAX,
                recovery: Duration::ZERO,
                ..config()
            },
            Counts::default(),
            Instant::now,
        )
        .unwrap();
        let permit = owner.acquire(&()).unwrap();
        permit.observe(Outcome::Verified);
        assert_eq!(*owner.observer.limit.lock().unwrap(), usize::MAX);
        assert_eq!(owner.state.lock().unwrap().peers[&()].limit, usize::MAX);
    }
}
