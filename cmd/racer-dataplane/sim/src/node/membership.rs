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
    /// Combined mode: the version each node's control client has applied
    /// (0 before its first snapshot). `None` in standalone mode, where
    /// adoption follows the synthetic per-node lag.
    external: Option<Vec<u64>>,
    /// Combined mode: members of the latest version whose client has
    /// applied nothing, so they refuse peer links (Known issue 14).
    unready: u32,
    /// Combined mode: per node, the generation of the first version its
    /// control client applied since (re)start, where production's
    /// membership chain begins (Known issue 25). `None` before any.
    chain_base: Vec<Option<u32>>,
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
            external: None,
            unready: 0,
            chain_base: Vec::new(),
        }
    }

    /// Combined mode: no version until the control plane commits one, and
    /// every node adopts only what its control client applied
    /// ([`Self::commit`], [`Self::apply`]).
    pub fn external(nodes: u32, retained: u64) -> Self {
        Self {
            live: vec![false; nodes as usize],
            history: VecDeque::new(),
            dirty: false,
            lag: 0,
            retained,
            external: Some(vec![0; nodes as usize]),
            unready: 0,
            chain_base: vec![None; nodes as usize],
        }
    }

    /// Combined mode: record the committed membership `number` with
    /// `members` (node ids). Returns false (and changes nothing) when
    /// `number` is not newer than the latest known version.
    pub fn commit(&mut self, number: u64, mut members: Vec<u32>, now: Nanos) -> bool {
        if self.history.back().is_some_and(|v| v.number >= number) {
            return false;
        }
        members.sort_unstable();
        members.dedup();
        members.retain(|n| (*n as usize) < self.live.len());
        self.live.iter_mut().for_each(|l| *l = false);
        for n in &members {
            self.live[*n as usize] = true;
        }
        self.unready = members.iter().filter(|n| !self.enrolled(**n)).count() as u32;
        let list: Vec<SimMember> = members.into_iter().map(SimMember::new).collect();
        let next = match self.history.back() {
            Some(prev) => Members::new_with_predecessor(list, &prev.members),
            None => Members::new(list),
        }
        .expect("valid ids");
        self.history.push_back(Version {
            number,
            published: now,
            members: Rc::new(next),
        });
        true
    }

    /// Combined mode: `node`'s control client applied `number` (0 after
    /// a crash). Versions never committed here are ignored.
    pub fn apply(&mut self, node: u32, number: u64) {
        if number != 0 && self.get(number).is_none() {
            return;
        }
        let generation = self.get(number).map(|v| v.members.generation());
        if let Some(base) = self.chain_base.get_mut(node as usize)
            && (number == 0 || base.is_none())
        {
            *base = generation;
        }
        let live = self.is_live(node);
        if let Some(slot) = self
            .external
            .as_mut()
            .and_then(|e| e.get_mut(node as usize))
        {
            if live && (*slot == 0) != (number == 0) {
                if number == 0 {
                    self.unready += 1;
                } else {
                    self.unready -= 1;
                }
            }
            *slot = number;
        }
    }

    /// Combined mode: where `node`'s membership chain begins (see
    /// `chain_base`); `None` in standalone mode or before it applied any.
    pub fn chain_base(&self, node: u32) -> Option<u32> {
        self.chain_base.get(node as usize).copied().flatten()
    }

    /// Combined mode: members of the latest version that cannot serve yet
    /// (their client has applied nothing: enrolling, or restarted).
    pub fn unready_members(&self) -> u32 {
        self.unready
    }

    /// Combined mode: drop versions no live node can resolve any more (older
    /// than the oldest applied version minus `retained`), keeping the latest.
    /// `down` reports crashed nodes, whose applied version does not count.
    /// Returns whether anything was dropped.
    pub fn prune(&mut self, down: impl Fn(u32) -> bool) -> bool {
        let Some(external) = &self.external else {
            return false;
        };
        let Some(latest) = self.history.back().map(|v| v.number) else {
            return false;
        };
        let oldest = external
            .iter()
            .enumerate()
            .filter(|(n, v)| **v > 0 && !down(*n as u32))
            .map(|(_, v)| *v)
            .min()
            .unwrap_or(latest);
        let mut dropped = false;
        while self.history.len() > 1
            && self
                .history
                .get(self.retained as usize + 1)
                .is_some_and(|v| v.number <= oldest)
        {
            self.history.pop_front();
            dropped = true;
        }
        dropped
    }

    /// Whether `node` has a membership to work with: always in standalone
    /// mode; in combined mode once its control client applied a version.
    pub fn enrolled(&self, node: u32) -> bool {
        match &self.external {
            None => true,
            Some(e) => e.get(node as usize).is_some_and(|v| *v > 0),
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

    /// Latest version number, 0 before any (combined mode).
    pub fn latest_number(&self) -> u64 {
        self.history.back().map_or(0, |v| v.number)
    }

    pub fn get(&self, number: u64) -> Option<&Version> {
        // Numbers are increasing but may skip in combined mode.
        let i = self
            .history
            .binary_search_by_key(&number, |v| v.number)
            .ok()?;
        self.history.get(i)
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
        if let Some(e) = &self.external {
            let applied = e.get(node as usize).copied().unwrap_or(0);
            return self
                .get(applied)
                .unwrap_or_else(|| self.history.front().expect("nonempty"));
        }
        self.history
            .iter()
            .rev()
            .find(|v| v.published + self.lag(node, v.number) <= now)
            .unwrap_or_else(|| self.history.front().expect("nonempty"))
    }

    /// Whether `node` can resolve `version` at `now` (its adopted version or
    /// one of the `retained` before it).
    pub fn known(&self, node: u32, version: u64, now: Nanos) -> bool {
        if !self.enrolled(node) {
            return false;
        }
        let adopted = self.adopted(node, now).number;
        let retained = match &self.external {
            // Count retained versions, not numbers (numbers may skip).
            Some(_) => {
                let lo = self.history.partition_point(|v| v.number < version);
                let hi = self.history.partition_point(|v| v.number < adopted);
                return version <= adopted
                    && (hi - lo) as u64 <= self.retained
                    && self.get(version).is_some();
            }
            None => self.retained,
        };
        version <= adopted && adopted - version <= retained && self.get(version).is_some()
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
            + self.external.as_ref().map_or(0, |e| e.capacity() * 8)
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
    fn external_versions_follow_the_control_plane() {
        let mut v = Versions::external(6, 1);
        assert_eq!(v.latest_number(), 0);
        assert!(!v.enrolled(0));
        assert!(!v.known(0, 1, 0));
        assert!(v.commit(3, vec![0, 1, 2, 9], 10));
        assert!(!v.commit(3, vec![0], 11), "not newer");
        assert_eq!(
            v.latest().members.members().len(),
            3,
            "out of range dropped"
        );
        assert!(v.is_live(2) && !v.is_live(3));
        v.apply(0, 3);
        v.apply(1, 7); // never committed: ignored
        assert!(v.enrolled(0) && !v.enrolled(1));
        assert_eq!(v.adopted(0, 0).number, 3);
        assert!(v.commit(5, vec![0, 1, 2, 3], 20));
        assert!(v.commit(8, vec![0, 1, 3], 30));
        v.apply(1, 8);
        // Node 0 is at 3: it resolves 3 only; node 1 resolves 8 and 5.
        assert!(v.known(0, 3, 0) && !v.known(0, 5, 0));
        assert!(v.known(1, 5, 0) && v.known(1, 8, 0));
        assert!(!v.known(1, 3, 0), "older than retained");
        assert!(!v.prune(|_| false), "node 0 still needs 3");
        v.apply(0, 8);
        assert!(v.prune(|_| false));
        assert!(v.get(3).is_none());
        assert!(v.get(5).is_some(), "retained before the oldest applied");
        // A crashed node does not pin old versions.
        v.apply(2, 5);
        assert!(!v.prune(|n| n == 2));
        assert!(v.get(5).is_some() && v.get(8).is_some());
        v.apply(0, 0);
        assert!(!v.enrolled(0));
    }

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
