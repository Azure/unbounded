//! Versioned memberships (racer-sim `node/state.rs` `Versions`, standalone mode).
//!
//! Membership changes are batched into published versions; each node adopts a version after a
//! per-node lag and still resolves the `retained` versions before the one it adopted. Members
//! are node ids; a member's position in a version is not its node id once a node leaves, so every
//! topology call maps ids to positions first ([`Version::position`], [`Version::node`]).

use std::{collections::VecDeque, num::NonZeroU32, rc::Rc};

use topology::Member;

use crate::engine::{MILLIS, Nanos};

/// A model node as a topology member: node `n` has the 4-byte big-endian id of `n`.
#[derive(Clone, Copy, Debug)]
pub struct ModelMember(pub [u8; 4]);

impl Member for ModelMember {
    const DOMAIN: &'static str = "racer-sim";

    fn id(&self) -> &[u8] {
        &self.0
    }

    fn weight(&self) -> NonZeroU32 {
        NonZeroU32::MIN
    }
}

/// A Model membership.
pub type Members = topology::Membership<ModelMember>;

/// Membership changes are batched and published at this period (racer-sim
/// `NodeConfig::membership_batch_ns`).
pub const BATCH: Nanos = 250 * MILLIS;
/// Most adoption lag of a node (racer-sim `NodeConfig::membership_lag_ns`).
pub const LAG: Nanos = 100 * MILLIS;
/// Old versions a node still resolves (production `retained_snapshots`, racer-sim
/// `retained_versions`).
pub const RETAINED: u64 = 2;

/// One published membership.
pub struct Version {
    /// Version number, from 1.
    pub number: u64,
    /// When it was published.
    pub published: Nanos,
    /// Its members, node ids in increasing order.
    pub members: Rc<Members>,
}

impl Version {
    /// `node`'s position in this version, if it is a member.
    pub fn position(&self, node: u32) -> Option<usize> {
        self.members.position(&node.to_be_bytes())
    }

    /// The node at `position`.
    pub fn node(&self, position: usize) -> u32 {
        u32::from_be_bytes(self.members.members()[position].0)
    }

    /// Member count.
    pub fn count(&self) -> usize {
        self.members.members().len()
    }
}

/// Published membership versions and the clock adoption is measured against.
pub struct Versions {
    live: Vec<bool>,
    history: VecDeque<Version>,
    dirty: bool,
    lag: Nanos,
    retained: u64,
    /// Combined mode (racer-sim `Versions::external`): the version each node's control client
    /// applied (0: none yet). Versions are committed by the control plane, not published here.
    external: Option<Vec<u64>>,
    /// Combined mode: version 0, the empty membership of a node without a snapshot.
    none: Option<Version>,
    /// The Model's current time, set before every event so the cores' routing closures see the
    /// version their node adopted.
    pub now: Nanos,
}

/// racer-sim's `mix` (SplitMix64 finalizer): the per-node lag draw.
fn mix(mut z: u64) -> u64 {
    z = z.wrapping_add(0x9e37_79b9_7f4a_7c15);
    z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
    z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
    z ^ (z >> 31)
}

impl Versions {
    /// Version 1, published at time 0, with every node live.
    pub fn new(nodes: u32, lag: Nanos, retained: u64) -> Self {
        let members = Members::new((0..nodes).map(|n| ModelMember(n.to_be_bytes())).collect())
            .expect("valid membership");
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
            none: None,
            now: 0,
        }
    }

    /// Combined mode: no node has a membership until its control client applies a committed
    /// one. Version 0 is an empty placeholder no node is a member of.
    pub fn external(nodes: u32, retained: u64) -> Self {
        let empty = || Version {
            number: 0,
            published: 0,
            members: Rc::new(Members::new(Vec::new()).expect("empty membership")),
        };
        let mut history = VecDeque::new();
        history.push_back(empty());
        Self {
            live: vec![false; nodes as usize],
            history,
            dirty: false,
            lag: 0,
            retained,
            external: Some(vec![0; nodes as usize]),
            none: Some(empty()),
            now: 0,
        }
    }

    /// Combined mode: the control plane committed membership `number` with `members` at `now`.
    /// Returns whether it is new.
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
        let list: Vec<ModelMember> = members
            .into_iter()
            .map(|n| ModelMember(n.to_be_bytes()))
            .collect();
        let latest = self.latest();
        let next = Members::new_with_predecessor(list, &latest.members).expect("valid ids");
        self.history.push_back(Version {
            number,
            published: now,
            members: Rc::new(next),
        });
        true
    }

    /// Combined mode: `node`'s control client applied `number` (0 after a crash). Versions never
    /// committed here are ignored.
    pub fn apply(&mut self, node: u32, number: u64) {
        if number != 0 && self.get(number).is_none() {
            return;
        }
        if let Some(slot) = self
            .external
            .as_mut()
            .and_then(|e| e.get_mut(node as usize))
        {
            *slot = number;
        }
    }

    /// Combined mode: drop versions older than every live node's applied version, keeping
    /// `retained` before the oldest. Returns whether any was dropped.
    pub fn prune(&mut self, down: impl Fn(u32) -> bool) -> bool {
        let Some(external) = &self.external else {
            return false;
        };
        let latest = self.latest().number;
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

    /// The version `node`'s client applied in combined mode (0: none); `None` standalone.
    pub fn applied(&self, node: u32) -> Option<u64> {
        self.external
            .as_ref()
            .map(|e| e.get(node as usize).copied().unwrap_or(0))
    }

    /// Versions retained.
    pub fn retained_versions(&self) -> usize {
        self.history.len()
    }

    /// Mark a change for the next publication; returns whether it changed.
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

    /// Publish pending changes as a new version; returns whether one was published.
    pub fn publish(&mut self, now: Nanos) -> bool {
        if !self.dirty || self.external.is_some() {
            return false;
        }
        self.dirty = false;
        let latest = self.latest();
        let members: Vec<ModelMember> = (0..self.live.len() as u32)
            .filter(|n| self.live[*n as usize])
            .map(|n| ModelMember(n.to_be_bytes()))
            .collect();
        let next = Members::new_with_predecessor(members, &latest.members).expect("valid ids");
        let number = latest.number + 1;
        self.history.push_back(Version {
            number,
            published: now,
            members: Rc::new(next),
        });
        // Nodes lag at most one batch, so they adopt at least `number - 1`; they resolve
        // `retained` versions older than that.
        while self.history.len() > self.retained as usize + 2 {
            self.history.pop_front();
        }
        true
    }

    /// The newest published version.
    pub fn latest(&self) -> &Version {
        self.history.back().expect("at least one version")
    }

    /// Version `number`, while it is retained.
    pub fn get(&self, number: u64) -> Option<&Version> {
        let first = self.history.front()?.number;
        self.history.get(number.checked_sub(first)? as usize)
    }

    /// Per-node adoption lag of `version` (version 1 has none).
    fn lag(&self, node: u32, version: u64) -> Nanos {
        if version <= 1 || self.lag == 0 {
            return 0;
        }
        mix((u64::from(node) << 32) ^ version) % (self.lag + 1)
    }

    /// The newest version `node` has adopted now.
    pub fn adopted(&self, node: u32) -> &Version {
        if let Some(applied) = self.applied(node) {
            if applied == 0 {
                return self.none.as_ref().expect("combined mode");
            }
            return self
                .get(applied)
                .unwrap_or_else(|| self.history.front().expect("nonempty"));
        }
        self.history
            .iter()
            .rev()
            .find(|v| v.published + self.lag(node, v.number) <= self.now)
            .unwrap_or_else(|| self.history.front().expect("nonempty"))
    }

    /// Whether `node` resolves `version` now: its adopted version or one of the `retained`
    /// before it.
    pub fn known(&self, node: u32, version: u64) -> bool {
        let adopted = self.adopted(node).number;
        version <= adopted && adopted - version <= self.retained && self.get(version).is_some()
    }

    /// Whether `version`'s predecessor lists `node` and `version` replaced it within
    /// [`racer_core::read::walk::PREVIOUS_MEMBERSHIP_WITHIN`] (racer-sim `previous_facts`).
    pub fn previous_recent_member(&self, node: u32, version: u64) -> bool {
        let within = racer_core::read::walk::PREVIOUS_MEMBERSHIP_WITHIN.as_nanos() as Nanos;
        let member = version
            .checked_sub(1)
            .and_then(|v| self.get(v))
            .is_some_and(|v| v.position(node).is_some());
        let recent = self.retained >= 1
            && self
                .get(version)
                .is_some_and(|v| self.now.saturating_sub(v.published) <= within);
        member && recent
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn publish_adopt_and_resolve() {
        let mut v = Versions::new(8, LAG, RETAINED);
        assert!(!v.publish(0));
        assert!(v.set(3, false));
        assert!(!v.set(3, false));
        v.now = BATCH;
        assert!(v.publish(BATCH));
        let latest = v.latest();
        assert_eq!((latest.number, latest.count()), (2, 7));
        // Positions shift past the node that left.
        assert_eq!(latest.position(3), None);
        assert_eq!(latest.position(4), Some(3));
        assert_eq!(latest.node(3), 4);
        // Every node adopts version 2 within the lag, and resolves version 1 after it.
        v.now = BATCH + LAG;
        for node in 0..8 {
            assert_eq!(v.adopted(node).number, 2);
            assert!(v.known(node, 1) && v.known(node, 2));
            assert!(!v.known(node, 3));
        }
        // Node 3 left: it is not a member of version 2, but was of version 1.
        assert!(v.previous_recent_member(3, 2));
        assert!(!v.previous_recent_member(3, 1));
    }

    #[test]
    fn a_lagging_node_does_not_know_the_new_version() {
        let mut v = Versions::new(64, LAG, RETAINED);
        v.set(5, false);
        v.now = BATCH;
        v.publish(BATCH);
        let lagging = (0..64).filter(|n| !v.known(*n, 2)).count();
        assert!(lagging > 0, "every node adopted at once");
    }

    #[test]
    fn external_versions_follow_applied_clients() {
        let mut v = Versions::external(4, 1);
        // No snapshot yet: nobody is a member.
        assert_eq!(v.adopted(2).position(2), None);
        assert!(v.commit(1, vec![0, 1, 2], 10));
        assert!(!v.commit(1, vec![0], 11));
        v.apply(2, 1);
        v.apply(3, 9);
        assert_eq!(v.applied(2), Some(1));
        assert_eq!(v.applied(3), Some(0));
        assert_eq!(v.adopted(2).position(2), Some(2));
        assert!(v.known(2, 1) && !v.known(3, 1));
        for n in 2..=4 {
            assert!(v.commit(n, vec![0, 1, 2, 3], 10 * n));
        }
        v.apply(2, 4);
        assert!(v.prune(|_| false));
        // Version 4 and one retained before it.
        assert!(v.get(3).is_some() && v.get(2).is_none());
        assert!(v.known(2, 3) && !v.known(2, 2));
        // A node back at no snapshot does not hold versions back.
        v.apply(2, 0);
        assert_eq!(v.adopted(2).number, 0);
    }

    #[test]
    fn old_versions_are_dropped() {
        let mut v = Versions::new(4, 0, RETAINED);
        for i in 1..=5u64 {
            v.set(0, i % 2 == 0);
            v.now = i * BATCH;
            v.publish(v.now);
        }
        assert_eq!(v.latest().number, 6);
        assert!(v.get(2).is_none() && v.get(3).is_some());
        assert!(!v.known(0, 3) && v.known(0, 4));
    }
}
