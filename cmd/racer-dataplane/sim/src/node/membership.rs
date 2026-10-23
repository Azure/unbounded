//! Shared membership versions and per-node adoption.
//!
//! The cluster has one live set. Changes (churn) are batched and published
//! as a new `topology::Membership` built with `new_with_predecessor`, shared
//! by every node through an `Rc`. Each node adopts a publication after a
//! deterministic per-node lag, so at any instant nodes may disagree by one
//! version; this is what drives `StaleMembership` answers and placement
//! maintenance. A node resolves its adopted version and up to
//! `retained` older ones, like production `retained_snapshots`.

use std::collections::VecDeque;
use std::num::NonZeroU32;
use std::rc::Rc;

use topology::Member;

use crate::engine::{Nanos, mix};

/// A simulated member: node id `n` has the 4-byte big-endian id of `n`, so
/// positions sort in node order.
#[derive(Clone, Copy, Debug)]
pub struct SimMember {
    pub node: u32,
    id: [u8; 4],
}

impl SimMember {
    pub fn new(node: u32) -> Self {
        Self {
            node,
            id: node.to_be_bytes(),
        }
    }
}

impl Member for SimMember {
    const DOMAIN: &'static str = "racer-sim";

    fn id(&self) -> &[u8] {
        &self.id
    }

    fn weight(&self) -> NonZeroU32 {
        NonZeroU32::MIN
    }
}

pub type Members = topology::Membership<SimMember>;

/// Position of `node` in `members`, if it is a member.
pub fn position(members: &Members, node: u32) -> Option<usize> {
    members
        .members()
        .binary_search_by_key(&node, |m| m.node)
        .ok()
}

pub struct Version {
    pub number: u64,
    pub published: Nanos,
    pub members: Rc<Members>,
}

pub struct Versions {
    live: Vec<bool>,
    history: VecDeque<Version>,
    dirty: bool,
    lag: Nanos,
    retained: u64,
}

impl Versions {
    /// Version 1, published at time 0, with every node live.
    pub fn new(nodes: u32, lag: Nanos, retained: u64) -> Self {
        let members = Members::new((0..nodes).map(SimMember::new).collect()).expect("valid ids");
        let mut history = VecDeque::new();
        history.push_back(Version {
            number: 1,
            published: 0,
            members: Rc::new(members),
        });
        Self {
            live: vec![true; nodes as usize],
            history,
            dirty: false,
            lag,
            retained,
        }
    }

    pub fn is_live(&self, node: u32) -> bool {
        self.live.get(node as usize).copied().unwrap_or(false)
    }

    /// Mark a change for the next publication. Returns whether it changed.
    pub fn set(&mut self, node: u32, live: bool) -> bool {
        let Some(slot) = self.live.get_mut(node as usize) else {
            return false;
        };
        if *slot == live {
            return false;
        }
        *slot = live;
        self.dirty = true;
        true
    }

    pub fn pending(&self) -> bool {
        self.dirty
    }

    /// Publish pending changes as a new version. Keeps enough history for
    /// every node to resolve its adopted version and `retained` older ones;
    /// returns true when versions were dropped (callers prune shared memos).
    pub fn publish(&mut self, now: Nanos) -> bool {
        if !self.dirty {
            return false;
        }
        self.dirty = false;
        let latest = self.latest();
        let members: Vec<SimMember> = (0..self.live.len() as u32)
            .filter(|n| self.live[*n as usize])
            .map(SimMember::new)
            .collect();
        let next = Members::new_with_predecessor(members, &latest.members).expect("valid ids");
        let number = latest.number + 1;
        self.history.push_back(Version {
            number,
            published: now,
            members: Rc::new(next),
        });
        // Nodes lag at most one batch, so they adopt at least `number - 1`;
        // they resolve `retained` versions older than that.
        let keep = self.retained as usize + 2;
        let mut dropped = false;
        while self.history.len() > keep {
            self.history.pop_front();
            dropped = true;
        }
        dropped
    }

    pub fn latest(&self) -> &Version {
        self.history.back().expect("at least one version")
    }

    pub fn get(&self, number: u64) -> Option<&Version> {
        let first = self.history.front()?.number;
        self.history.get(number.checked_sub(first)? as usize)
    }

    /// Per-node adoption lag for `version` (version 1 has none).
    pub fn lag(&self, node: u32, version: u64) -> Nanos {
        if version <= 1 || self.lag == 0 {
            return 0;
        }
        mix((u64::from(node) << 32) ^ version) % (self.lag + 1)
    }

    /// Newest version `node` has adopted at `now`.
    pub fn adopted(&self, node: u32, now: Nanos) -> &Version {
        self.history
            .iter()
            .rev()
            .find(|v| v.published + self.lag(node, v.number) <= now)
            .unwrap_or_else(|| self.history.front().expect("nonempty"))
    }

    /// Whether `node` can resolve `version` at `now` (its adopted version or
    /// one of the `retained` before it).
    pub fn known(&self, node: u32, version: u64, now: Nanos) -> bool {
        let adopted = self.adopted(node, now).number;
        version <= adopted && adopted - version <= self.retained && self.get(version).is_some()
    }

    /// Membership identities to keep in shared placement memos.
    pub fn identities(&self) -> Vec<[u8; 32]> {
        self.history.iter().map(|v| v.members.identity()).collect()
    }

    /// Graph identities to keep in shared route memos.
    pub fn topology_identities(&self) -> Vec<[u8; 32]> {
        self.history
            .iter()
            .map(|v| v.members.topology_identity())
            .collect()
    }

    pub fn approximate_bytes(&self) -> usize {
        self.live.capacity()
            + self
                .history
                .iter()
                .map(|v| v.members.retained_bytes())
                .sum::<usize>()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn publish_adopt_and_resolve() {
        let mut v = Versions::new(8, 10, 1);
        assert!(!v.publish(5), "nothing pending");
        assert!(v.set(3, false));
        assert!(!v.set(3, false));
        assert!(!v.set(99, false));
        assert!(v.pending());
        assert!(!v.publish(100));
        assert_eq!(v.latest().number, 2);
        assert_eq!(v.latest().members.members().len(), 7);
        assert_eq!(position(&v.latest().members, 3), None);
        assert_eq!(position(&v.latest().members, 4), Some(3));
        // Adoption is lagged per node, never before publication.
        for node in 0..8 {
            assert_eq!(v.adopted(node, 99).number, 1);
            assert_eq!(v.adopted(node, 110).number, 2);
            let at = 100 + v.lag(node, 2);
            assert_eq!(v.adopted(node, at).number, 2);
            assert!(v.known(node, 1, 110));
            assert!(!v.known(node, 2, 99), "not adopted yet");
        }
        // History is bounded; old versions become unknown.
        for (i, (node, live)) in [(3u32, true), (4, false), (5, false)]
            .into_iter()
            .enumerate()
        {
            assert!(v.set(node, live));
            v.publish(200 + 100 * i as u64);
        }
        assert_eq!(v.latest().number, 5);
        assert!(v.get(1).is_none());
        assert!(v.get(2).is_none());
        assert!(v.get(3).is_some());
        assert!(!v.known(0, 3, 1000), "older than retained");
        assert!(v.known(0, 4, 1000));
        assert_eq!(v.identities().len(), 3);
        assert_eq!(v.topology_identities().len(), 3);
        assert!(v.approximate_bytes() > 0);
    }
}
