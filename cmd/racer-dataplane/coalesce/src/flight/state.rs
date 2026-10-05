//! Waiter lifecycle and result-independent flight transitions.

use super::{Cursor, Exhausted, Identity, Stale};
use std::collections::BTreeMap;
use std::ops::{Deref, DerefMut};
use std::task::Waker;
use std::time::Instant;

/// Request-owned policy facts, not acquisition credits or result interpretation.
pub trait WaiterPolicy {
    type Error: Copy;
    fn check(&self) -> Option<Self::Error>;
    fn deadline(&self) -> Instant;
}

pub struct Waiter<W: WaiterPolicy> {
    pub policy: W,
    pub acquisition: bool,
    pub complete: bool,
    pub issued: bool,
    pub error: Option<W::Error>,
    pub waker: Option<Waker>,
}
impl<W: WaiterPolicy> Deref for Waiter<W> {
    type Target = W;
    fn deref(&self) -> &W {
        &self.policy
    }
}
impl<W: WaiterPolicy> DerefMut for Waiter<W> {
    fn deref_mut(&mut self) -> &mut W {
        &mut self.policy
    }
}
impl<W: WaiterPolicy> Waiter<W> {
    pub fn eligible(&self) -> bool {
        self.acquisition && !self.issued && self.error.is_none()
    }
}

pub enum Outcome<O, E> {
    Published(O),
    Failed(E),
    Retry,
}
pub enum Phase<C, O, E> {
    Acquiring,
    RetryPending,
    Draining(Outcome<O, E>),
    Complete(C),
    Failed(E),
}
pub enum Published<C, P> {
    Complete(C),
    Partial(P),
}

pub fn store_waker(slot: &mut Option<Waker>, waker: &Waker) {
    if slot.as_ref().is_none_or(|old| !old.will_wake(waker)) {
        *slot = Some(waker.clone());
    }
}

pub struct State<C, P, O, W: WaiterPolicy> {
    pub phase: Phase<C, O, W::Error>,
    pub leader: Option<u64>,
    pub waiters: BTreeMap<u64, Waiter<W>>,
    pub deadlines: BTreeMap<(Instant, u64), ()>,
    pub partial: Option<P>,
    cursor: Cursor,
}
impl<C, P, O, W: WaiterPolicy> Default for State<C, P, O, W> {
    fn default() -> Self {
        Self {
            phase: Phase::RetryPending,
            leader: None,
            waiters: BTreeMap::new(),
            deadlines: BTreeMap::new(),
            partial: None,
            cursor: Cursor::default(),
        }
    }
}
impl<C, P, O, W: WaiterPolicy> State<C, P, O, W> {
    pub fn register(&mut self, id: u64, policy: W, acquisition: bool, complete: bool) {
        self.deadlines.insert((policy.deadline(), id), ());
        self.waiters.insert(
            id,
            Waiter {
                policy,
                acquisition,
                complete,
                issued: false,
                error: None,
                waker: None,
            },
        );
    }

    pub fn detach(&mut self, id: u64) {
        if let Some(waiter) = self.waiters.remove(&id) {
            self.deadlines.remove(&(waiter.deadline(), id));
        }
    }

    pub fn validate_registration(
        &self,
        id: u64,
        attached: bool,
        registered: &Identity,
        current: &Identity,
    ) -> Result<(), Stale> {
        if !attached || !registered.same_registration(current) || !self.waiters.contains_key(&id) {
            return Err(Stale);
        }
        Ok(())
    }

    /// Mark only the supplying caller; refresh decides whether to revoke a leader.
    pub fn cancel(&mut self, id: u64, error: W::Error) {
        if let Some(waiter) = self.waiters.get_mut(&id) {
            waiter.error = Some(error);
        }
    }

    /// Caller validates its leader capability and publication before this step.
    /// Notification timing remains explicit: publication wakes only on settlement,
    /// while rejection/revocation also notifies before retained operations finish.
    pub fn begin_completion(&mut self, outcome: Outcome<O, W::Error>) {
        self.phase = Phase::Draining(outcome);
    }

    pub fn notify(&mut self, wakes: &mut Vec<Waker>) {
        for waiter in self.waiters.values_mut() {
            if let Some(waker) = waiter.waker.take() {
                wakes.push(waker);
            }
        }
    }

    pub fn refresh_waiter(&mut self, id: u64, wakes: &mut Vec<Waker>) {
        let Some(waiter) = self.waiters.get_mut(&id) else {
            return;
        };
        if waiter.error.is_none() {
            waiter.error = waiter.check();
        }
        if waiter.error.is_some() {
            self.deadlines.remove(&(waiter.deadline(), id));
            if let Some(waker) = waiter.waker.take() {
                wakes.push(waker);
            }
        }
    }

    pub fn sweep_waiter(&mut self, wakes: &mut Vec<Waker>) {
        if let Some((id, _)) = self.cursor.next(&self.waiters) {
            self.refresh_waiter(id, wakes);
        }
    }

    /// Validate policy/budget first in the adapter. Only an eligible waiter can
    /// receive a new generation; retained operations must have settled first.
    pub fn elect(
        &mut self,
        id: u64,
        identity: &mut Identity,
        limit: u64,
    ) -> Result<bool, Exhausted> {
        if !matches!(self.phase, Phase::RetryPending)
            || !self.waiters.get(&id).is_some_and(Waiter::eligible)
        {
            return Ok(false);
        }
        identity.advance(limit)?;
        self.phase = Phase::Acquiring;
        self.leader = Some(id);
        self.waiters.get_mut(&id).unwrap().issued = true;
        Ok(true)
    }

    pub fn validate_leader(&self, id: u64, active: bool) -> Result<(), Stale> {
        if !active || !matches!(self.phase, Phase::Acquiring) || self.leader != Some(id) {
            return Err(Stale);
        }
        Ok(())
    }

    pub fn settle(
        &mut self,
        idle: bool,
        unavailable: W::Error,
        split: fn(O) -> Published<C, P>,
        wakes: &mut Vec<Waker>,
    ) {
        if !idle || !matches!(self.phase, Phase::Draining(_)) {
            return;
        }
        let Phase::Draining(outcome) = std::mem::replace(&mut self.phase, Phase::RetryPending)
        else {
            unreachable!()
        };
        self.leader = None;
        self.phase = match outcome {
            Outcome::Published(value) => match split(value) {
                Published::Complete(value) => {
                    self.partial = None;
                    Phase::Complete(value)
                }
                Published::Partial(value) => {
                    self.partial = Some(value);
                    for waiter in self.waiters.values_mut() {
                        if waiter.complete {
                            waiter.issued = false;
                        }
                    }
                    Phase::RetryPending
                }
            },
            Outcome::Failed(error) => Phase::Failed(error),
            Outcome::Retry if self.waiters.values().any(Waiter::eligible) => Phase::RetryPending,
            Outcome::Retry => Phase::Failed(unavailable),
        };
        self.notify(wakes);
    }

    pub fn revoke(&mut self, error: W::Error, wakes: &mut Vec<Waker>) {
        if let Some(waiter) = self.leader.and_then(|id| self.waiters.get_mut(&id)) {
            waiter.error = Some(error);
        }
        self.phase = Phase::Draining(Outcome::Retry);
        self.notify(wakes);
    }

    pub fn refresh(
        &mut self,
        idle: bool,
        unavailable: W::Error,
        canceled: W::Error,
        now: impl Fn() -> Instant,
        split: fn(O) -> Published<C, P>,
        wakes: &mut Vec<Waker>,
    ) {
        if let Some(id) = self.leader {
            self.refresh_waiter(id, wakes);
        }
        for _ in 0..64 {
            let Some((&(deadline, id), _)) = self.deadlines.first_key_value() else {
                break;
            };
            if deadline > now() {
                break;
            }
            self.deadlines.remove(&(deadline, id));
            self.refresh_waiter(id, wakes);
        }
        if matches!(self.phase, Phase::Acquiring)
            && self
                .leader
                .and_then(|id| self.waiters.get(&id))
                .is_none_or(|w| w.error.is_some())
        {
            let error = self
                .leader
                .and_then(|id| self.waiters.get(&id))
                .and_then(|w| w.error)
                .unwrap_or(canceled);
            self.revoke(error, wakes);
        }
        self.settle(idle, unavailable, split, wakes);
        if matches!(self.phase, Phase::RetryPending)
            && self.partial.is_none()
            && !self.waiters.values().any(Waiter::eligible)
        {
            self.phase = Phase::Draining(Outcome::Failed(unavailable));
            self.settle(idle, unavailable, split, wakes);
        }
    }

    pub fn stop(&mut self, error: W::Error, wakes: &mut Vec<Waker>) {
        for waiter in self.waiters.values_mut() {
            waiter.error = Some(error);
        }
        self.phase = Phase::Draining(Outcome::Failed(error));
        self.notify(wakes);
        self.waiters.clear();
        self.deadlines.clear();
    }
}

#[cfg(test)]
mod tests;
