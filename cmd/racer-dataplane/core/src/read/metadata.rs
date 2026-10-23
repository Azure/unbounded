//! Sans-IO metadata refresh cohorts.
//!
//! Concurrent metadata resolutions for one object and purpose share one
//! refresh: the first poller leads, others wait, and the leader either
//! finishes the cohort or releases leadership for a retry. Elections are
//! bounded by [`MAX_REFRESH_ATTEMPTS`]; afterwards waiters receive the
//! exhausted value. This mirrors `flow_control::coalesce::Table`, which
//! production `MetadataService` uses, with plain [`Token`] wakes.
//! [`refresh_settlement`] is the shared rule for a leader's outcome.

use super::error::Error;
use racer_object_wire::model::{MetadataSelector, StrongEtag};
use std::collections::hash_map::RandomState;
use std::collections::{BTreeMap, HashMap};
use std::hash::{BuildHasher, Hash};

pub use super::flight::Token;

/// Waiters per refresh cohort.
pub const MAX_WAITERS: usize = 64;

/// Leader elections per cohort before broadcasting exhaustion.
pub const MAX_REFRESH_ATTEMPTS: usize = 8;

/// The work a refresh cohort may perform. A pin never bootstraps a current body.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub enum RefreshPurpose {
    /// Observe the current version without requesting its body (including HEAD).
    Current,
    /// Resolve immutable metadata for exactly this version, even after expiry.
    Pinned(StrongEtag),
    /// Observe the current version and allow acquisition of page zero.
    Bootstrap,
}

impl From<MetadataSelector> for RefreshPurpose {
    /// Metadata-only callers cannot opt into a body-fetching cohort.
    fn from(selector: MetadataSelector) -> Self {
        match selector {
            MetadataSelector::Fresh => Self::Current,
            MetadataSelector::Pinned(etag) => Self::Pinned(etag),
        }
    }
}

impl RefreshPurpose {
    /// Select metadata without changing whether the cohort may fetch a body.
    pub fn selector(&self) -> MetadataSelector {
        match self {
            Self::Current | Self::Bootstrap => MetadataSelector::Fresh,
            Self::Pinned(etag) => MetadataSelector::Pinned(etag.clone()),
        }
    }

    /// Whether this purpose may acquire page zero (a bootstrap operation).
    pub fn bootstrap(&self) -> bool {
        matches!(self, Self::Bootstrap)
    }
}

/// How a refresh leader settles its cohort.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Settlement {
    /// Broadcast the result to every waiter.
    Finish,
    /// Release leadership so another waiter may try with its own credentials
    /// or budget.
    Retry,
}

/// Settle a failed refresh. Credential-specific origin rejections and
/// failures of the leader's own budget let another waiter retry; other
/// failures finish the cohort.
pub fn refresh_settlement(error: Error, remaining_attempts: u32) -> Settlement {
    let rejected = matches!(error, Error::OriginRejected | Error::OriginForbidden);
    let caller_budget = matches!(
        error,
        Error::Cancelled | Error::DeadlineExceeded | Error::HopBudgetExhausted
    ) || (error == Error::Unavailable && remaining_attempts == 0);
    if rejected || caller_budget {
        Settlement::Retry
    } else {
        Settlement::Finish
    }
}

/// Cohort join rejected by a key or waiter bound.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CapacityError;

/// What a waiter should do next.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Event<V> {
    Lead,
    Complete(V),
    Pending,
}

struct Cohort<V> {
    leader: Option<u64>,
    attempts: usize,
    waiters: BTreeMap<u64, Option<Token>>,
    result: Option<V>,
}

/// Handle for one registration.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Registration<K> {
    pub key: K,
    pub id: u64,
    cohort: u64,
}

/// Sans-IO keyed cohorts with bounded registration and leader re-election.
pub struct MetadataCohorts<K, V, S = RandomState> {
    active: HashMap<K, u64, S>,
    cohorts: BTreeMap<u64, Cohort<V>>,
    registrations: usize,
    waiters_per_cohort: usize,
    attempts_per_cohort: usize,
    exhausted: V,
    next_cohort: u64,
    next_id: u64,
}

impl<K: Clone + Eq + Hash, V: Clone> MetadataCohorts<K, V> {
    /// Production bounds with `exhausted` broadcast after the last election.
    pub fn new(exhausted: V) -> Self {
        Self::with_hasher(
            MAX_WAITERS,
            MAX_REFRESH_ATTEMPTS,
            exhausted,
            RandomState::new(),
        )
    }
}

impl<K: Clone + Eq + Hash, V: Clone, S: BuildHasher> MetadataCohorts<K, V, S> {
    pub fn with_hasher(
        waiters_per_cohort: usize,
        attempts_per_cohort: usize,
        exhausted: V,
        hasher: S,
    ) -> Self {
        Self {
            active: HashMap::with_hasher(hasher),
            cohorts: BTreeMap::new(),
            registrations: 0,
            waiters_per_cohort,
            attempts_per_cohort,
            exhausted,
            next_cohort: 0,
            next_id: 0,
        }
    }

    /// Live registrations, including readers of completed cohorts.
    pub fn registration_count(&self) -> usize {
        self.registrations
    }

    /// Cohorts still accepting registrations.
    pub fn active_count(&self) -> usize {
        self.active.len()
    }

    /// Join the active cohort for `key`, creating one if needed. `capacity`
    /// bounds active keys and, times the waiter bound, all registrations.
    pub fn join(&mut self, key: K, capacity: usize) -> Result<Registration<K>, CapacityError> {
        if self.registrations >= capacity.saturating_mul(self.waiters_per_cohort) {
            return Err(CapacityError);
        }
        let cohort = match self.active.get(&key) {
            Some(cohort) => *cohort,
            None => {
                if self.active.len() >= capacity {
                    return Err(CapacityError);
                }
                let cohort = self.next_cohort;
                self.next_cohort += 1;
                self.active.insert(key.clone(), cohort);
                self.cohorts.insert(
                    cohort,
                    Cohort {
                        leader: None,
                        attempts: 0,
                        waiters: BTreeMap::new(),
                        result: None,
                    },
                );
                cohort
            }
        };
        let state = self.cohorts.get_mut(&cohort).expect("active cohort");
        if state.waiters.len() >= self.waiters_per_cohort {
            return Err(CapacityError);
        }
        let id = self.next_id;
        self.next_id += 1;
        state.waiters.insert(id, None);
        self.registrations += 1;
        Ok(Registration { key, id, cohort })
    }

    /// Park `token` and elect at most once per attempt.
    pub fn poll(&mut self, registration: &Registration<K>, token: Token) -> Event<V> {
        let attempts_per_cohort = self.attempts_per_cohort;
        let Some(state) = self.cohorts.get_mut(&registration.cohort) else {
            return Event::Pending;
        };
        if let Some(result) = &state.result {
            return Event::Complete(result.clone());
        }
        state.waiters.insert(registration.id, Some(token));
        if state.leader.is_none() {
            if state.attempts >= attempts_per_cohort {
                let result = self.exhausted.clone();
                let mut wakes = Vec::new();
                self.finish(registration, result.clone(), &mut wakes);
                return Event::Complete(result);
            }
            state.attempts += 1;
            state.leader = Some(registration.id);
            return Event::Lead;
        }
        Event::Pending
    }

    /// Release leadership after an attempt and wake waiters.
    pub fn retry(&mut self, registration: &Registration<K>, wakes: &mut Vec<Token>) {
        if let Some(state) = self.cohorts.get_mut(&registration.cohort) {
            if state.leader == Some(registration.id) {
                state.leader = None;
            }
            take(state, wakes);
        }
    }

    /// Broadcast `result`, close admission, and wake waiters.
    pub fn finish(&mut self, registration: &Registration<K>, result: V, wakes: &mut Vec<Token>) {
        if let Some(state) = self.cohorts.get_mut(&registration.cohort) {
            state.result = Some(result);
            state.leader = None;
            take(state, wakes);
        }
        self.remove_active(registration);
    }

    /// Drop a registration; a departing leader wakes the others.
    pub fn detach(&mut self, registration: Registration<K>, wakes: &mut Vec<Token>) {
        let Some(state) = self.cohorts.get_mut(&registration.cohort) else {
            return;
        };
        if state.waiters.remove(&registration.id).is_none() {
            return;
        }
        if state.leader == Some(registration.id) {
            state.leader = None;
            take(state, wakes);
        }
        self.registrations -= 1;
        if state.waiters.is_empty() {
            self.cohorts.remove(&registration.cohort);
            self.remove_active(&registration);
        }
    }

    fn remove_active(&mut self, registration: &Registration<K>) {
        if self.active.get(&registration.key) == Some(&registration.cohort) {
            self.active.remove(&registration.key);
        }
    }
}

fn take<V>(state: &mut Cohort<V>, wakes: &mut Vec<Token>) {
    wakes.extend(state.waiters.values_mut().filter_map(Option::take));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn leader_finishes_followers_complete_and_admission_closes() {
        let mut cohorts = MetadataCohorts::<u8, u32>::new(0);
        let a = cohorts.join(1, 4).unwrap();
        let b = cohorts.join(1, 4).unwrap();
        assert_eq!(cohorts.active_count(), 1);
        assert_eq!(cohorts.poll(&a, 10), Event::Lead);
        assert_eq!(cohorts.poll(&b, 11), Event::Pending);
        let mut wakes = Vec::new();
        cohorts.finish(&a, 7, &mut wakes);
        assert_eq!(wakes, [10, 11]);
        assert_eq!(cohorts.poll(&b, 11), Event::Complete(7));
        assert_eq!(cohorts.active_count(), 0);
        // A new join starts a fresh cohort.
        let c = cohorts.join(1, 4).unwrap();
        assert_eq!(cohorts.poll(&c, 12), Event::Lead);
        cohorts.detach(a, &mut wakes);
        cohorts.detach(b, &mut wakes);
        cohorts.detach(c, &mut wakes);
        assert_eq!(cohorts.registration_count(), 0);
        assert_eq!(cohorts.active_count(), 0);
    }

    #[test]
    fn retry_reelects_until_attempts_exhaust() {
        let mut cohorts = MetadataCohorts::<u8, u32>::with_hasher(4, 2, 99, RandomState::new());
        let a = cohorts.join(1, 4).unwrap();
        let b = cohorts.join(1, 4).unwrap();
        let mut wakes = Vec::new();
        assert_eq!(cohorts.poll(&a, 1), Event::Lead);
        cohorts.retry(&a, &mut wakes);
        assert_eq!(cohorts.poll(&b, 2), Event::Lead);
        cohorts.retry(&b, &mut wakes);
        assert_eq!(cohorts.poll(&a, 1), Event::Complete(99));
        assert_eq!(cohorts.poll(&b, 2), Event::Complete(99));
    }

    #[test]
    fn departing_leader_wakes_followers() {
        let mut cohorts = MetadataCohorts::<u8, u32>::new(0);
        let a = cohorts.join(1, 4).unwrap();
        let b = cohorts.join(1, 4).unwrap();
        assert_eq!(cohorts.poll(&a, 1), Event::Lead);
        assert_eq!(cohorts.poll(&b, 2), Event::Pending);
        let mut wakes = Vec::new();
        cohorts.detach(a, &mut wakes);
        assert_eq!(wakes, [2]);
        assert_eq!(cohorts.poll(&b, 2), Event::Lead);
    }

    #[test]
    fn capacity_bounds() {
        let mut cohorts = MetadataCohorts::<u8, u32>::with_hasher(1, 8, 0, RandomState::new());
        cohorts.join(1, 2).unwrap();
        assert_eq!(cohorts.join(1, 2), Err(CapacityError));
        cohorts.join(2, 2).unwrap();
        assert_eq!(cohorts.join(3, 2), Err(CapacityError));
    }

    #[test]
    fn settlement_rules() {
        for error in [
            Error::OriginRejected,
            Error::OriginForbidden,
            Error::Cancelled,
            Error::DeadlineExceeded,
            Error::HopBudgetExhausted,
        ] {
            assert_eq!(refresh_settlement(error, 3), Settlement::Retry, "{error:?}");
        }
        assert_eq!(refresh_settlement(Error::Unavailable, 0), Settlement::Retry);
        assert_eq!(
            refresh_settlement(Error::Unavailable, 1),
            Settlement::Finish
        );
        assert_eq!(refresh_settlement(Error::NotFound, 0), Settlement::Finish);
    }

    #[test]
    fn purposes() {
        let etag = StrongEtag::test_value("v");
        assert_eq!(
            RefreshPurpose::from(MetadataSelector::Pinned(etag.clone())),
            RefreshPurpose::Pinned(etag.clone())
        );
        assert!(matches!(
            RefreshPurpose::Bootstrap.selector(),
            MetadataSelector::Fresh
        ));
        assert!(matches!(
            RefreshPurpose::Pinned(etag).selector(),
            MetadataSelector::Pinned(_)
        ));
        assert!(RefreshPurpose::Bootstrap.bootstrap());
        assert!(!RefreshPurpose::Current.bootstrap());
    }
}
