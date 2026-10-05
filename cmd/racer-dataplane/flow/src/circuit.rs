//! Worker-local bounded circuits with caller-owned retry policy and clock.
use crate::{Error, Result};
use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet},
    time::{Duration, Instant},
};

pub struct Circuits<K> {
    capacity: usize,
    probe_timeout: Duration,
    states: RefCell<BTreeMap<K, Circuit>>,
    probes: RefCell<BTreeSet<K>>,
}
struct Circuit {
    failures: u32,
    retry_at: Instant,
    probe_until: Option<Instant>,
}
pub struct Probe<'a, K: Ord> {
    health: &'a Circuits<K>,
    key: Option<K>,
}
impl<K: Ord> Drop for Probe<'_, K> {
    fn drop(&mut self) {
        if let Some(key) = &self.key {
            self.health.probes.borrow_mut().remove(key);
        }
    }
}
impl<K: Ord + Clone> Circuits<K> {
    pub const fn new(capacity: usize, probe_timeout: Duration) -> Self {
        Self {
            capacity,
            probe_timeout,
            states: RefCell::new(BTreeMap::new()),
            probes: RefCell::new(BTreeSet::new()),
        }
    }
    pub fn acquire(&self, key: &K, now: Instant) -> Result<Probe<'_, K>> {
        if !self.try_acquire(key, now) {
            return Err(Error::Unavailable);
        }
        let probe = self.states.borrow().contains_key(key);
        if probe {
            if self.probes.borrow().len() >= self.capacity {
                return Err(Error::Overloaded);
            }
            self.probes.borrow_mut().insert(key.clone());
        }
        Ok(Probe {
            health: self,
            key: probe.then(|| key.clone()),
        })
    }
    pub fn success(&self, key: &K) {
        self.states.borrow_mut().remove(key);
    }
    /// The caller classifies failures and supplies its retry delay, including jitter.
    pub fn failure(
        &self,
        key: &K,
        now: Instant,
        backoff: impl FnOnce(&K, u32) -> Duration,
    ) -> Result<()> {
        let mut states = self.states.borrow_mut();
        if !states.contains_key(key) && states.len() >= self.capacity {
            return Err(Error::Overloaded);
        }
        let state = states.entry(key.clone()).or_insert(Circuit {
            failures: 0,
            retry_at: now,
            probe_until: None,
        });
        state.failures = state.failures.saturating_add(1);
        state.retry_at = now + backoff(key, state.failures);
        state.probe_until = None;
        Ok(())
    }
    /// Routing hint only; actual work must acquire an exclusive probe.
    pub fn available(&self, key: &K, now: Instant) -> bool {
        !self.probes.borrow().contains(key)
            && self
                .states
                .borrow()
                .get(key)
                .is_none_or(|s| now >= s.retry_at && s.probe_until.is_none_or(|until| now >= until))
    }
    /// An unowned probe becomes eligible again after the configured timeout.
    pub fn try_acquire(&self, key: &K, now: Instant) -> bool {
        if self.probes.borrow().contains(key) {
            return false;
        }
        let mut states = self.states.borrow_mut();
        let Some(state) = states.get_mut(key) else {
            return true;
        };
        if now < state.retry_at || state.probe_until.is_some_and(|until| now < until) {
            return false;
        }
        state.probe_until = Some(now + self.probe_timeout);
        true
    }
    pub fn retain(&self, keys: &[K]) {
        self.states.borrow_mut().retain(|key, _| keys.contains(key));
    }
    pub fn len(&self) -> usize {
        self.states.borrow().len()
    }
    pub fn is_empty(&self) -> bool {
        self.states.borrow().is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn bounded_backoff_and_owned_probe_survive_retention_and_success() {
        let now = Instant::now();
        let health = Circuits::new(1, Duration::from_secs(1));
        assert!(health.try_acquire(&7, now));
        health
            .failure(&7, now, |key, failures| {
                assert_eq!((*key, failures), (7, 1));
                Duration::from_secs(2)
            })
            .unwrap();
        assert_eq!(
            health.failure(&8, now, |_, _| Duration::ZERO),
            Err(Error::Overloaded)
        );
        assert!(!health.available(&7, now));
        let retry = now + Duration::from_secs(2);
        let probe = health.acquire(&7, retry).unwrap();
        assert!(!health.try_acquire(&7, retry + Duration::from_secs(5)));
        health.success(&7);
        health.retain(&[]);
        assert!(!health.available(&7, retry));
        drop(probe);
        assert!(health.available(&7, retry));
        assert!(health.is_empty());
    }
    #[test]
    fn dropped_probe_preserves_timeout_and_failures_saturate() {
        let now = Instant::now();
        let health = Circuits::new(1, Duration::from_secs(1));
        health.failure(&(), now, |_, _| Duration::ZERO).unwrap();
        drop(health.acquire(&(), now).unwrap());
        assert!(!health.try_acquire(&(), now));
        assert!(health.try_acquire(&(), now + Duration::from_secs(1)));
        health.states.borrow_mut().get_mut(&()).unwrap().failures = u32::MAX;
        health
            .failure(&(), now, |_, n| {
                assert_eq!(n, u32::MAX);
                Duration::ZERO
            })
            .unwrap();
        health.retain(&[]);
        assert_eq!(health.len(), 0);
        assert_eq!(
            Circuits::new(0, Duration::ZERO).failure(&(), now, |_, _| Duration::ZERO),
            Err(Error::Overloaded)
        );
    }
}
