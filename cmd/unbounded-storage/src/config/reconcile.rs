// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Peer reconciliation surface.
//!
//! [`apply_peers_startup`] pushes an initial peer list into a freshly
//! constructed shard fabric; [`reconcile_peers`] is the steady-state
//! entry point used by the live config watcher to diff a new desired
//! set of peers against either the fabric's current connection list or
//! a caller-supplied cache of the previously-applied specs. The
//! [`PeerReconcileTarget`] trait is the seam both paths share so the
//! reconciler can be unit-tested without a real fabric.

use std::collections::{HashMap, HashSet};
use std::sync::Arc;

use crate::bufferpool::PeerId;
use crate::fabric::{ConnectionSpec, Fabric, FabricError};

use super::apply::peer_spec_to_connection;
use super::schema::PeerSpec;

pub trait PeerReconcileTarget {
    fn list(&self) -> Vec<PeerId>;
    fn add(&self, spec: ConnectionSpec) -> Result<(), FabricError>;
    fn remove(&self, peer: PeerId) -> Result<(), FabricError>;
}

impl PeerReconcileTarget for Arc<Fabric> {
    fn list(&self) -> Vec<PeerId> {
        Fabric::list_connections(self)
    }

    fn add(&self, spec: ConnectionSpec) -> Result<(), FabricError> {
        Fabric::add_connection(self, spec)
    }

    fn remove(&self, peer: PeerId) -> Result<(), FabricError> {
        Fabric::remove_connection(self, peer)
    }
}

#[derive(Debug, Default)]
pub struct ApplyReport {
    pub applied: usize,
    pub failures: Vec<(u64, String)>,
}

/// Outcome of one [`reconcile_peers`] pass. `applied` is the map the
/// caller should hand back on the next reconcile so address/numa drift
/// for an existing peer id can be detected without re-querying the
/// fabric for the stored spec.
#[derive(Debug, Default)]
pub struct ReconcileReport {
    pub added: usize,
    pub removed: usize,
    pub updated: usize,
    pub failures: Vec<(PeerId, String)>,
    pub applied: HashMap<PeerId, ConnectionSpec>,
}

pub fn apply_peers_startup(
    target: &dyn PeerReconcileTarget,
    peers: &[PeerSpec],
) -> ApplyReport {
    let mut report = ApplyReport::default();
    for p in peers {
        match target.add(peer_spec_to_connection(p)) {
            Ok(()) => report.applied += 1,
            Err(e) => report.failures.push((p.id, e.to_string())),
        }
    }
    report
}

/// Drive `target` toward the peer set described by `desired`.
///
/// When `last_applied` is `Some`, an existing peer whose `wire_addr` or
/// `hca_numa` has changed is treated as a remove+add (an "update").
/// When it is `None` we cannot tell stored specs from the trait, so
/// only id-level additions and removals are performed; this is the
/// startup path.
///
/// Operation order is: removals first, then additions. Failures of
/// either kind are accumulated and do not abort the pass. The returned
/// `applied` map reflects the specs the target should now hold (i.e.
/// it excludes peers whose `add` failed and includes peers that were
/// already present in the previous applied map and not removed).
///
/// On a failed `update-remove` or `remove`, the prior spec is
/// preserved in `applied` so the next reconcile can re-detect the
/// drift / retry the removal: the fabric still holds the peer at the
/// old spec because the remove did not complete. On a failed
/// `update-add` (remove succeeded, add failed) the prior spec is NOT
/// carried forward, because the fabric no longer holds it; the next
/// reconcile pass will see the id in `desired` but not in
/// `target.list()` and drive a fresh add through the additions
/// branch.
///
/// The `added` / `removed` / `updated` counters count only fully
/// completed operations. A failed `update-add` records a failure
/// but does not bump `updated`, even though its remove half
/// succeeded, because the peer was not actually updated end-to-end.
pub fn reconcile_peers(
    target: &dyn PeerReconcileTarget,
    desired: &[PeerSpec],
    last_applied: Option<&HashMap<PeerId, ConnectionSpec>>,
) -> ReconcileReport {
    let mut report = ReconcileReport::default();

    let desired_map: HashMap<PeerId, ConnectionSpec> = desired
        .iter()
        .map(|p| {
            let spec = peer_spec_to_connection(p);
            (spec.peer, spec)
        })
        .collect();

    let current: HashSet<PeerId> = target.list().into_iter().collect();

    // Drift detection: for ids present in both `current` and `desired`,
    // compare against the previous applied spec (if any). Differences
    // become a remove-then-add for that id.
    let mut to_update: HashSet<PeerId> = HashSet::new();
    if let Some(prev) = last_applied {
        for (id, new_spec) in &desired_map {
            if !current.contains(id) {
                continue;
            }
            if let Some(old_spec) = prev.get(id) {
                if old_spec.wire_addr != new_spec.wire_addr
                    || old_spec.hca_numa != new_spec.hca_numa
                {
                    to_update.insert(*id);
                }
            }
        }
    }

    // Removals: ids currently present but not desired, plus the
    // remove-half of updates.
    let mut removed_ok: HashSet<PeerId> = HashSet::new();
    let mut to_remove: Vec<PeerId> = current
        .iter()
        .copied()
        .filter(|id| !desired_map.contains_key(id) || to_update.contains(id))
        .collect();
    to_remove.sort_by_key(|p| p.0);
    for id in to_remove {
        let is_update = to_update.contains(&id);
        match target.remove(id) {
            Ok(()) => {
                // `update-remove` does not bump `updated` here; the
                // counter fires in the additions loop when the
                // matching add succeeds. A failed update-add must
                // not be reported as a successful update.
                if !is_update {
                    report.removed += 1;
                }
                removed_ok.insert(id);
            }
            Err(e) => {
                let op = if is_update { "update-remove" } else { "remove" };
                report
                    .failures
                    .push((id, format!("{op}: {e}")));
            }
        }
    }

    // Additions: ids in desired that are either not currently present
    // or that we successfully removed as part of an update.
    let mut to_add: Vec<PeerId> = desired_map
        .keys()
        .copied()
        .filter(|id| {
            !current.contains(id)
                || (to_update.contains(id) && removed_ok.contains(id))
        })
        .collect();
    to_add.sort_by_key(|p| p.0);
    for id in to_add {
        let spec = desired_map[&id].clone();
        let is_update = to_update.contains(&id);
        match target.add(spec.clone()) {
            Ok(()) => {
                if is_update {
                    report.updated += 1;
                } else {
                    report.added += 1;
                }
                report.applied.insert(id, spec);
            }
            Err(e) => {
                let op = if is_update { "update-add" } else { "add" };
                report.failures.push((id, format!("{op}: {e}")));
            }
        }
    }

    // Carry forward the prior spec for every id the fabric still
    // holds and that we have not already recorded under `applied`.
    // "Still holds" means the id was in `current` at the start of
    // the pass and we did not successfully remove it - which covers
    // both untouched steady-state peers and failed-remove (plain or
    // update-half) peers. Successful update-removes whose re-add
    // failed deliberately fall out here: the fabric does not have
    // the peer anymore, so the next pass should re-add via the
    // id-not-in-current branch rather than re-detect drift.
    if let Some(prev) = last_applied {
        for (id, spec) in prev {
            if current.contains(id)
                && !removed_ok.contains(id)
                && !report.applied.contains_key(id)
            {
                report.applied.insert(*id, spec.clone());
            }
        }
    }

    report
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::schema::PeerTransport;
    use std::cell::RefCell;

    #[derive(Debug, Clone, PartialEq, Eq)]
    enum Op {
        Add(u64),
        Remove(u64),
    }

    struct MockTarget {
        ops: RefCell<Vec<Op>>,
        present: RefCell<HashSet<PeerId>>,
        fail_add_on: Option<u64>,
        fail_remove_on: Option<u64>,
    }

    impl MockTarget {
        fn new(initial: &[u64]) -> Self {
            Self {
                ops: RefCell::new(Vec::new()),
                present: RefCell::new(
                    initial.iter().map(|i| PeerId(*i)).collect(),
                ),
                fail_add_on: None,
                fail_remove_on: None,
            }
        }

        fn with_add_failure(mut self, id: u64) -> Self {
            self.fail_add_on = Some(id);
            self
        }

        fn with_remove_failure(mut self, id: u64) -> Self {
            self.fail_remove_on = Some(id);
            self
        }
    }

    impl PeerReconcileTarget for MockTarget {
        fn list(&self) -> Vec<PeerId> {
            let mut v: Vec<PeerId> =
                self.present.borrow().iter().copied().collect();
            v.sort_by_key(|p| p.0);
            v
        }
        fn add(&self, spec: ConnectionSpec) -> Result<(), FabricError> {
            self.ops.borrow_mut().push(Op::Add(spec.peer.0));
            if Some(spec.peer.0) == self.fail_add_on {
                return Err(FabricError::BadConfig("forced failure"));
            }
            self.present.borrow_mut().insert(spec.peer);
            Ok(())
        }
        fn remove(&self, peer: PeerId) -> Result<(), FabricError> {
            self.ops.borrow_mut().push(Op::Remove(peer.0));
            if Some(peer.0) == self.fail_remove_on {
                return Err(FabricError::BadConfig("forced failure"));
            }
            self.present.borrow_mut().remove(&peer);
            Ok(())
        }
    }

    fn peer(id: u64) -> PeerSpec {
        PeerSpec {
            id,
            transport: PeerTransport::Tcp,
            address: format!("10.0.0.{id}:9000"),
            hca_numa: None,
        }
    }

    fn peer_addr(id: u64, addr: &str) -> PeerSpec {
        PeerSpec {
            id,
            transport: PeerTransport::Tcp,
            address: addr.to_string(),
            hca_numa: None,
        }
    }

    #[test]
    fn applies_each_peer_in_order() {
        let t = MockTarget::new(&[]);
        let peers = vec![peer(1), peer(2), peer(3)];
        let r = apply_peers_startup(&t, &peers);
        assert_eq!(r.applied, 3);
        assert!(r.failures.is_empty());
        assert_eq!(
            *t.ops.borrow(),
            vec![Op::Add(1), Op::Add(2), Op::Add(3)]
        );
    }

    #[test]
    fn one_failure_does_not_abort_others() {
        let t = MockTarget::new(&[]).with_add_failure(2);
        let peers = vec![peer(1), peer(2), peer(3)];
        let r = apply_peers_startup(&t, &peers);
        assert_eq!(r.applied, 2);
        assert_eq!(r.failures.len(), 1);
        assert_eq!(r.failures[0].0, 2);
        assert_eq!(
            *t.ops.borrow(),
            vec![Op::Add(1), Op::Add(2), Op::Add(3)]
        );
    }

    #[test]
    fn empty_peer_list_is_a_noop() {
        let t = MockTarget::new(&[]);
        let r = apply_peers_startup(&t, &[]);
        assert_eq!(r.applied, 0);
        assert!(r.failures.is_empty());
        assert!(t.ops.borrow().is_empty());
    }

    #[test]
    fn reconcile_from_empty_adds_all() {
        let t = MockTarget::new(&[]);
        let peers = vec![peer(1), peer(2), peer(3)];
        let r = reconcile_peers(&t, &peers, None);
        assert_eq!(r.added, 3);
        assert_eq!(r.removed, 0);
        assert_eq!(r.updated, 0);
        assert!(r.failures.is_empty());
        assert_eq!(r.applied.len(), 3);
        assert_eq!(
            *t.ops.borrow(),
            vec![Op::Add(1), Op::Add(2), Op::Add(3)]
        );
    }

    #[test]
    fn reconcile_swaps_one_peer() {
        let t = MockTarget::new(&[1, 2, 3]);
        let peers = vec![peer(2), peer(3), peer(4)];
        let r = reconcile_peers(&t, &peers, None);
        assert_eq!(r.added, 1);
        assert_eq!(r.removed, 1);
        assert_eq!(r.updated, 0);
        assert!(r.failures.is_empty());
        assert_eq!(
            *t.ops.borrow(),
            vec![Op::Remove(1), Op::Add(4)]
        );
    }

    #[test]
    fn reconcile_steady_state_is_noop() {
        let t = MockTarget::new(&[1, 2]);
        let mut prev = HashMap::new();
        prev.insert(PeerId(1), peer_spec_to_connection(&peer(1)));
        prev.insert(PeerId(2), peer_spec_to_connection(&peer(2)));
        let peers = vec![peer(1), peer(2)];
        let r = reconcile_peers(&t, &peers, Some(&prev));
        assert_eq!(r.added, 0);
        assert_eq!(r.removed, 0);
        assert_eq!(r.updated, 0);
        assert!(r.failures.is_empty());
        assert!(t.ops.borrow().is_empty());
        assert_eq!(r.applied.len(), 2);
    }

    #[test]
    fn reconcile_detects_address_drift_as_update() {
        let t = MockTarget::new(&[1]);
        let mut prev = HashMap::new();
        prev.insert(
            PeerId(1),
            peer_spec_to_connection(&peer_addr(1, "a:1")),
        );
        let peers = vec![peer_addr(1, "b:2")];
        let r = reconcile_peers(&t, &peers, Some(&prev));
        assert_eq!(r.added, 0);
        assert_eq!(r.removed, 0);
        assert_eq!(r.updated, 1);
        assert!(r.failures.is_empty());
        assert_eq!(
            *t.ops.borrow(),
            vec![Op::Remove(1), Op::Add(1)]
        );
        assert_eq!(r.applied[&PeerId(1)].wire_addr, "b:2");
    }

    #[test]
    fn reconcile_add_failure_recorded_others_succeed() {
        let t = MockTarget::new(&[]).with_add_failure(2);
        let peers = vec![peer(1), peer(2), peer(3)];
        let r = reconcile_peers(&t, &peers, None);
        assert_eq!(r.added, 2);
        assert_eq!(r.failures.len(), 1);
        assert_eq!(r.failures[0].0, PeerId(2));
        assert!(r.failures[0].1.starts_with("add: "));
        assert!(r.applied.contains_key(&PeerId(1)));
        assert!(!r.applied.contains_key(&PeerId(2)));
        assert!(r.applied.contains_key(&PeerId(3)));
    }

    #[test]
    fn reconcile_update_remove_failure_preserves_drift() {
        // First pass: remove fails on peer 1, so the fabric still
        // holds the OLD spec; `applied` must reflect that so the
        // next pass re-detects the drift.
        let t = MockTarget::new(&[1]).with_remove_failure(1);
        let mut prev = HashMap::new();
        prev.insert(
            PeerId(1),
            peer_spec_to_connection(&peer_addr(1, "a:1")),
        );
        let peers = vec![peer_addr(1, "b:2")];
        let r = reconcile_peers(&t, &peers, Some(&prev));
        assert_eq!(r.updated, 0);
        assert_eq!(r.added, 0);
        assert_eq!(r.removed, 0);
        assert_eq!(r.failures.len(), 1);
        assert_eq!(r.failures[0].0, PeerId(1));
        assert!(r.failures[0].1.starts_with("update-remove:"));
        assert_eq!(r.applied[&PeerId(1)].wire_addr, "a:1");
        assert_eq!(*t.ops.borrow(), vec![Op::Remove(1)]);

        // Second pass: with the recovered remove, drift detection
        // must fire again using the carried-forward old spec.
        let t2 = MockTarget::new(&[1]);
        let prev2 = r.applied;
        let r2 = reconcile_peers(&t2, &peers, Some(&prev2));
        assert_eq!(r2.updated, 1);
        assert!(r2.failures.is_empty());
        assert_eq!(r2.applied[&PeerId(1)].wire_addr, "b:2");
        assert_eq!(
            *t2.ops.borrow(),
            vec![Op::Remove(1), Op::Add(1)]
        );
    }

    #[test]
    fn reconcile_update_add_failure_does_not_carry_old_spec() {
        // First pass: remove succeeds, add fails. The fabric no
        // longer has the peer at any spec, so the old spec must NOT
        // be carried forward; the id simply drops out of `applied`.
        let t = MockTarget::new(&[1]).with_add_failure(1);
        let mut prev = HashMap::new();
        prev.insert(
            PeerId(1),
            peer_spec_to_connection(&peer_addr(1, "a:1")),
        );
        let peers = vec![peer_addr(1, "b:2")];
        let r = reconcile_peers(&t, &peers, Some(&prev));
        assert_eq!(r.updated, 0);
        assert_eq!(r.added, 0);
        assert_eq!(r.failures.len(), 1);
        assert_eq!(r.failures[0].0, PeerId(1));
        assert!(r.failures[0].1.starts_with("update-add:"));
        assert!(!r.applied.contains_key(&PeerId(1)));
        assert_eq!(
            *t.ops.borrow(),
            vec![Op::Remove(1), Op::Add(1)]
        );

        // Second pass: the id is in `desired` but not in
        // `target.list()` (fabric is empty post-remove), so the
        // additions branch fires - no drift bookkeeping needed.
        let t2 = MockTarget::new(&[]);
        let prev2 = r.applied;
        let r2 = reconcile_peers(&t2, &peers, Some(&prev2));
        assert_eq!(r2.added, 1);
        assert_eq!(r2.updated, 0);
        assert!(r2.failures.is_empty());
        assert_eq!(r2.applied[&PeerId(1)].wire_addr, "b:2");
        assert_eq!(*t2.ops.borrow(), vec![Op::Add(1)]);
    }

    #[test]
    fn reconcile_plain_remove_failure_preserves_old_spec() {
        // The peer is no longer desired but the remove failed, so
        // the fabric still holds it. Preserving the old spec in
        // `applied` is what lets the next pass keep retrying the
        // removal (the id will again be in `current` but not
        // `desired_map`, driving the same remove path).
        let t = MockTarget::new(&[1]).with_remove_failure(1);
        let mut prev = HashMap::new();
        prev.insert(
            PeerId(1),
            peer_spec_to_connection(&peer_addr(1, "a:1")),
        );
        let peers: Vec<PeerSpec> = vec![];
        let r = reconcile_peers(&t, &peers, Some(&prev));
        assert_eq!(r.removed, 0);
        assert_eq!(r.added, 0);
        assert_eq!(r.updated, 0);
        assert_eq!(r.failures.len(), 1);
        assert_eq!(r.failures[0].0, PeerId(1));
        assert!(r.failures[0].1.starts_with("remove:"));
        assert_eq!(r.applied[&PeerId(1)].wire_addr, "a:1");
        assert_eq!(*t.ops.borrow(), vec![Op::Remove(1)]);
    }
}
