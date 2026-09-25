//! Local link circuits, bounded probes, and backoff. Never edits placement.
use super::hash;
use crate::{
    error::{Error, Result},
    model::identity::NodeId,
};
use sha2::Digest;
use std::{
    cell::RefCell,
    collections::BTreeMap,
    time::{Duration, Instant},
};

/// One worker's observations of its own links. Never a cluster-wide node status.
pub struct LinkHealth {
    capacity: usize,
    states: RefCell<BTreeMap<NodeId, Circuit>>,
}

// Preserve the scaffold's `Rc::new(LinkHealth)` expression with isolated state.
#[allow(non_upper_case_globals, clippy::declare_interior_mutable_const)]
pub const LinkHealth: LinkHealth = LinkHealth::new(36);

struct Circuit {
    failures: u32,
    retry_at: Instant,
    probe_until: Option<Instant>,
}

#[derive(Clone, Copy, Debug)]
pub enum LinkOutcome {
    Success,
    Timeout,
    Refused,
    ProtocolFailure,
}
impl LinkHealth {
    pub const fn new(capacity: usize) -> Self {
        Self {
            capacity,
            states: RefCell::new(BTreeMap::new()),
        }
    }

    pub fn observe(&self, neighbor: &NodeId, outcome: LinkOutcome) -> Result<()> {
        self.observe_at(neighbor, outcome, Instant::now())
    }

    pub fn observe_at(&self, neighbor: &NodeId, outcome: LinkOutcome, now: Instant) -> Result<()> {
        let mut states = self.states.borrow_mut();
        if matches!(outcome, LinkOutcome::Success) {
            states.remove(neighbor);
            return Ok(());
        }
        if !states.contains_key(neighbor) && states.len() >= self.capacity {
            return Err(Error::Overloaded);
        }
        let state = states.entry(neighbor.clone()).or_insert(Circuit {
            failures: 0,
            retry_at: now,
            probe_until: None,
        });
        state.failures = state.failures.saturating_add(1);
        state.retry_at = now + backoff(neighbor, state.failures);
        state.probe_until = None;
        Ok(())
    }

    /// Read-only routing hint. Actual sends must call `try_acquire` to serialize
    /// half-open probes; successful closed circuits permit pooled concurrent I/O.
    pub fn available(&self, neighbor: &NodeId) -> Result<bool> {
        self.available_at(neighbor, Instant::now())
    }

    pub fn available_at(&self, neighbor: &NodeId, now: Instant) -> Result<bool> {
        Ok(self.states.borrow().get(neighbor).is_none_or(|state| {
            now >= state.retry_at && state.probe_until.is_none_or(|until| now >= until)
        }))
    }

    pub fn try_acquire(&self, neighbor: &NodeId) -> Result<bool> {
        self.try_acquire_at(neighbor, Instant::now())
    }

    pub fn try_acquire_at(&self, neighbor: &NodeId, now: Instant) -> Result<bool> {
        let mut states = self.states.borrow_mut();
        let Some(state) = states.get_mut(neighbor) else {
            return Ok(true);
        };
        if now < state.retry_at || state.probe_until.is_some_and(|until| now < until) {
            return Ok(false);
        }
        // A dropped or hung probe releases eligibility only after this timeout.
        state.probe_until = Some(now + Duration::from_secs(1));
        Ok(true)
    }

    /// Called on a topology transition to discard circuits for former neighbors.
    pub fn retain_neighbors(&self, neighbors: &[NodeId]) {
        self.states
            .borrow_mut()
            .retain(|node, _| neighbors.contains(node));
    }

    pub fn tracked_links(&self) -> usize {
        self.states.borrow().len()
    }
}

fn backoff(node: &NodeId, failures: u32) -> Duration {
    let base = (100u64 << failures.saturating_sub(1).min(8)).min(20_000);
    let mut digest = hash::domain(b"racer/link-backoff/v1\0");
    hash::bytes(&mut digest, node.0.as_bytes());
    digest.update(failures.to_be_bytes());
    let digest = hash::finish(digest);
    let jitter = u64::from(u16::from_be_bytes([digest[0], digest[1]])) % (base / 4 + 1);
    Duration::from_millis(base + jitter)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        model::identity::PageNumber,
        topology::{
            fixtures::{membership, object},
            placement::Placement,
        },
    };

    #[test]
    fn circuit_backoff_probes_success_and_isolation() {
        let first = LinkHealth::new(2);
        let second = LinkHealth::new(2);
        let node = NodeId("a".into());
        let now = Instant::now();
        assert!(first.try_acquire_at(&node, now).unwrap());
        first.observe_at(&node, LinkOutcome::Timeout, now).unwrap();
        assert!(!first.available_at(&node, now).unwrap());
        assert!(second.available_at(&node, now).unwrap());
        let retry = now + backoff(&node, 1);
        assert!(
            !first
                .available_at(&node, retry - Duration::from_nanos(1))
                .unwrap()
        );
        assert!(first.available_at(&node, retry).unwrap());
        assert!(first.try_acquire_at(&node, retry).unwrap());
        assert!(!first.try_acquire_at(&node, retry).unwrap());
        assert!(
            first
                .try_acquire_at(&node, retry + Duration::from_secs(1))
                .unwrap()
        );
        first
            .observe_at(&node, LinkOutcome::Refused, retry)
            .unwrap();
        assert!(backoff(&node, 2) > backoff(&node, 1));
        first
            .observe_at(&node, LinkOutcome::Success, retry)
            .unwrap();
        assert!(first.available_at(&node, retry).unwrap());
        assert_eq!(first.tracked_links(), 0);
        for failures in [1, 2, 10, u32::MAX] {
            assert!(backoff(&node, failures) <= Duration::from_secs(25));
        }
        assert_ne!(backoff(&node, 3), backoff(&NodeId("b".into()), 3));
    }

    #[test]
    fn capacity_and_placement_are_independent() {
        let health = LinkHealth::new(1);
        let members = membership(3);
        let placement = Placement::new(1);
        let before = placement
            .rank(members.clone(), &object(), PageNumber(0))
            .unwrap()
            .ordered;
        health
            .observe(&before[0], LinkOutcome::ProtocolFailure)
            .unwrap();
        assert_eq!(
            health.observe(&before[1], LinkOutcome::Timeout),
            Err(Error::Overloaded)
        );
        assert_eq!(
            before,
            placement
                .rank(members, &object(), PageNumber(0))
                .unwrap()
                .ordered
        );
        health.retain_neighbors(&[]);
        assert_eq!(health.tracked_links(), 0);
    }
}
