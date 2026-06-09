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

use crate::fabric::PeerId;
use crate::fabric::{ConnectionSpec, Fabric, FabricError};

use super::apply::peer_spec_to_connection;
use super::schema::{BackendSpec, FrontendSpec, PeerSpec};

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

pub fn apply_peers_startup(target: &dyn PeerReconcileTarget, peers: &[PeerSpec]) -> ApplyReport {
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
                    || old_spec.labels != new_spec.labels
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
                report.failures.push((id, format!("{op}: {e}")));
            }
        }
    }

    // Additions: ids in desired that are either not currently present
    // or that we successfully removed as part of an update.
    let mut to_add: Vec<PeerId> = desired_map
        .keys()
        .copied()
        .filter(|id| !current.contains(id) || (to_update.contains(id) && removed_ok.contains(id)))
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

    // Carry forward a spec for every id the fabric still holds and
    // that we have not already recorded under `applied`. "Still
    // holds" means the id was in `current` at the start of the pass
    // and we did not successfully remove it - which covers both
    // untouched steady-state peers and failed-remove (plain or
    // update-half) peers. Successful update-removes whose re-add
    // failed deliberately fall out here: the fabric does not have
    // the peer anymore, so the next pass should re-add via the
    // id-not-in-current branch rather than re-detect drift.
    //
    // For a peer that is still desired and was NOT part of an update
    // (`to_update`), every observable field - addr, hca, and labels -
    // already matches the last applied spec by construction, so
    // recording the desired spec is equivalent to keeping the old
    // one. Ids that are not desired (plain failed-remove) or that are
    // in `to_update` with a failed remove (failed update-remove) keep
    // the old spec so the next pass can retry the removal / re-detect
    // the drift; the fabric still holds the peer at that old spec.
    if let Some(prev) = last_applied {
        for (id, old_spec) in prev {
            if current.contains(id) && !removed_ok.contains(id) && !report.applied.contains_key(id)
            {
                let spec = match desired_map.get(id) {
                    Some(desired_spec) if !to_update.contains(id) => desired_spec.clone(),
                    _ => old_spec.clone(),
                };
                report.applied.insert(*id, spec);
            }
        }
    }

    report
}

/// Reconciliation target for the origin-tier backends, keyed by
/// `id`. Mirrors [`PeerReconcileTarget`]: the live config watcher
/// drives the runtime registry toward a desired set, the trait is the
/// seam so the reconciler is unit-testable without a real registry.
/// The backend runtime does not exist yet, so today the only
/// implementor is the test mock; the trait is the contract the
/// future `backend::BackendRegistry` will satisfy.
pub trait BackendReconcileTarget {
    fn list(&self) -> Vec<String>;
    fn add(&self, spec: &BackendSpec) -> Result<(), String>;
    fn remove(&self, id: &str) -> Result<(), String>;
}

/// Reconciliation target for the workload-facing frontends, keyed by
/// `id`. Mirrors [`PeerReconcileTarget`]; see
/// [`BackendReconcileTarget`] for the rationale on why only a test
/// mock implements it today.
pub trait FrontendReconcileTarget {
    fn list(&self) -> Vec<String>;
    fn add(&self, spec: &FrontendSpec) -> Result<(), String>;
    fn remove(&self, id: &str) -> Result<(), String>;
}

/// Outcome of one [`reconcile_backends`] / [`reconcile_frontends`]
/// pass. Shares the shape of [`ReconcileReport`] (add/remove/update
/// counters, accumulated failures keyed by the collection's id type,
/// and the `applied` map to hand back on the next pass for drift
/// detection) specialized to the string-keyed backend/frontend specs.
#[derive(Debug)]
pub struct SpecReconcileReport<S> {
    pub added: usize,
    pub removed: usize,
    pub updated: usize,
    pub failures: Vec<(String, String)>,
    /// Ids whose `add` was intentionally skipped this pass (rather than
    /// failed): used by the combined backend/frontend orchestrator to
    /// record a frontend deferred because its referenced backend was
    /// not yet present. Empty for plain `reconcile_backends` /
    /// `reconcile_frontends` passes.
    pub deferred: Vec<(String, String)>,
    pub applied: HashMap<String, S>,
}

impl<S> Default for SpecReconcileReport<S> {
    fn default() -> Self {
        Self {
            added: 0,
            removed: 0,
            updated: 0,
            failures: Vec::new(),
            deferred: Vec::new(),
            applied: HashMap::new(),
        }
    }
}

pub type BackendReconcileReport = SpecReconcileReport<BackendSpec>;
pub type FrontendReconcileReport = SpecReconcileReport<FrontendSpec>;

/// Drive `target` toward the backend set described by `desired`,
/// keyed by `id`.
///
/// The semantics mirror [`reconcile_peers`] exactly, specialized to
/// the string-keyed `BackendSpec`: removals first, then additions; an
/// id present in both `current` and `desired` whose spec differs from
/// `last_applied` is a remove-then-add ("update"); failures are
/// accumulated and never abort the pass; the returned `applied` map is
/// the set the target now holds and is the caller's input on the next
/// pass. The failure carry-forward rules match `reconcile_peers`: a
/// failed update-remove or plain remove preserves the old spec so the
/// next pass retries, while a failed update-add (remove succeeded)
/// drops the id so the next pass re-adds via the not-current branch.
pub fn reconcile_backends(
    target: &dyn BackendReconcileTarget,
    desired: &[BackendSpec],
    last_applied: Option<&HashMap<String, BackendSpec>>,
) -> BackendReconcileReport {
    let desired_map: HashMap<String, BackendSpec> =
        desired.iter().map(|b| (b.id.clone(), b.clone())).collect();
    reconcile_specs(
        &desired_map,
        last_applied,
        || target.list(),
        |spec| target.add(spec),
        |id| target.remove(id),
    )
}

/// Drive `target` toward the frontend set described by `desired`,
/// keyed by `id`. Semantics mirror [`reconcile_backends`] /
/// [`reconcile_peers`].
pub fn reconcile_frontends(
    target: &dyn FrontendReconcileTarget,
    desired: &[FrontendSpec],
    last_applied: Option<&HashMap<String, FrontendSpec>>,
) -> FrontendReconcileReport {
    let desired_map: HashMap<String, FrontendSpec> =
        desired.iter().map(|f| (f.id.clone(), f.clone())).collect();
    reconcile_specs(
        &desired_map,
        last_applied,
        || target.list(),
        |spec| target.add(spec),
        |id| target.remove(id),
    )
}

/// Shared string-keyed reconcile core for backends and frontends.
///
/// Factored out because the two specs differ only in their concrete
/// type, not in the diff algorithm. An id whose desired spec is not
/// equal to its `last_applied` spec is treated as an update
/// (remove-then-add). The carry-forward rules are identical to
/// [`reconcile_peers`].
///
/// Internally this is the remove phase followed immediately by the add
/// phase with a no-op gate, so its observable behavior is exactly the
/// pre-split single pass. The phases are also exposed (privately) so
/// [`reconcile_backends_and_frontends`] can interleave them across the
/// two resource kinds to honor the cross-resource ordering contract.
fn reconcile_specs<S: Clone + PartialEq>(
    desired_map: &HashMap<String, S>,
    last_applied: Option<&HashMap<String, S>>,
    list: impl Fn() -> Vec<String>,
    add: impl Fn(&S) -> Result<(), String>,
    remove: impl Fn(&str) -> Result<(), String>,
) -> SpecReconcileReport<S> {
    let state = reconcile_remove_phase(desired_map, last_applied, list, remove);
    reconcile_add_phase(state, add, |_id, _spec| Ok(()))
}

/// Intermediate state carried from the remove phase into the add phase
/// of a single string-keyed reconcile pass. Holds everything the add
/// phase needs to reproduce the original single-pass behavior:
/// `current`/`to_update`/`removed_ok` plus the in-progress report.
struct SpecReconcileState<'a, S> {
    desired_map: &'a HashMap<String, S>,
    last_applied: Option<&'a HashMap<String, S>>,
    current: HashSet<String>,
    to_update: HashSet<String>,
    removed_ok: HashSet<String>,
    report: SpecReconcileReport<S>,
}

/// Removal half of a string-keyed reconcile pass: compute drift,
/// remove ids that are no longer desired or that need a remove-then-add
/// update, and accumulate failures. Mirrors the first half of the old
/// monolithic `reconcile_specs`.
fn reconcile_remove_phase<'a, S: Clone + PartialEq>(
    desired_map: &'a HashMap<String, S>,
    last_applied: Option<&'a HashMap<String, S>>,
    list: impl Fn() -> Vec<String>,
    remove: impl Fn(&str) -> Result<(), String>,
) -> SpecReconcileState<'a, S> {
    let mut report = SpecReconcileReport::<S>::default();

    let current: HashSet<String> = list().into_iter().collect();

    let mut to_update: HashSet<String> = HashSet::new();
    if let Some(prev) = last_applied {
        for (id, new_spec) in desired_map {
            if !current.contains(id) {
                continue;
            }
            if let Some(old_spec) = prev.get(id) {
                if old_spec != new_spec {
                    to_update.insert(id.clone());
                }
            }
        }
    }

    let mut removed_ok: HashSet<String> = HashSet::new();
    let mut to_remove: Vec<String> = current
        .iter()
        .filter(|id| !desired_map.contains_key(*id) || to_update.contains(*id))
        .cloned()
        .collect();
    to_remove.sort();
    for id in to_remove {
        let is_update = to_update.contains(&id);
        match remove(&id) {
            Ok(()) => {
                if !is_update {
                    report.removed += 1;
                }
                removed_ok.insert(id);
            }
            Err(e) => {
                let op = if is_update { "update-remove" } else { "remove" };
                report.failures.push((id, format!("{op}: {e}")));
            }
        }
    }

    SpecReconcileState {
        desired_map,
        last_applied,
        current,
        to_update,
        removed_ok,
        report,
    }
}

/// Addition half of a string-keyed reconcile pass. `can_add` gates each
/// candidate add: `Ok(())` proceeds, `Err(reason)` defers the id
/// (recorded in `report.deferred`, never added, never counted). The
/// default no-op gate (`|_, _| Ok(())`) reproduces the original
/// single-pass behavior. Carry-forward is identical to
/// [`reconcile_peers`].
fn reconcile_add_phase<S: Clone + PartialEq>(
    state: SpecReconcileState<'_, S>,
    add: impl Fn(&S) -> Result<(), String>,
    can_add: impl Fn(&str, &S) -> Result<(), String>,
) -> SpecReconcileReport<S> {
    let SpecReconcileState {
        desired_map,
        last_applied,
        current,
        to_update,
        removed_ok,
        mut report,
    } = state;

    let mut to_add: Vec<String> = desired_map
        .keys()
        .filter(|id| {
            !current.contains(*id) || (to_update.contains(*id) && removed_ok.contains(*id))
        })
        .cloned()
        .collect();
    to_add.sort();
    for id in to_add {
        let spec = desired_map[&id].clone();
        if let Err(reason) = can_add(&id, &spec) {
            report.deferred.push((id, reason));
            continue;
        }
        let is_update = to_update.contains(&id);
        match add(&spec) {
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

    // Carry-forward identical in spirit to `reconcile_peers`: ids the
    // target still holds (in `current`, not successfully removed) that
    // we have not already recorded. A still-desired, non-updated id
    // records the freshest desired spec; otherwise the old spec is
    // preserved so the next pass can retry the removal / re-detect the
    // drift. A deferred id falls out exactly like a failed update-add:
    // if it was an update its remove already landed (in `removed_ok`)
    // so it is dropped here, and the next pass re-adds it once the gate
    // opens.
    if let Some(prev) = last_applied {
        for (id, old_spec) in prev {
            if current.contains(id) && !removed_ok.contains(id) && !report.applied.contains_key(id)
            {
                let spec = match desired_map.get(id) {
                    Some(desired_spec) if !to_update.contains(id) => desired_spec.clone(),
                    _ => old_spec.clone(),
                };
                report.applied.insert(id.clone(), spec);
            }
        }
    }

    report
}

/// Combined outcome of one cross-resource reconcile pass driven by
/// [`reconcile_backends_and_frontends`].
#[derive(Debug)]
pub struct ResourceReconcileReport {
    pub backends: BackendReconcileReport,
    pub frontends: FrontendReconcileReport,
}

/// Reconcile backends and frontends together with a cross-resource
/// ordering and referential guarantee that neither single-resource
/// pass can provide on its own.
///
/// Load-time validation guarantees every frontend references a defined
/// backend within one snapshot, but the two resource sets are applied
/// to independent runtime registries; without ordering a frontend
/// could be registered pointing at a backend that is not yet present
/// (or already gone). This driver interleaves the four half-passes so
/// that:
///
/// 1. frontend removals run first (a frontend never references an
///    already-removed backend),
/// 2. backend removals run next,
/// 3. backend additions run before any frontend addition (so a
///    referenced backend is present first), and
/// 4. frontend additions run last and are *gated*: a frontend whose
///    referenced backend is not present after steps 2-3 (never defined,
///    removed, or whose `add` failed) is deferred - recorded in
///    `frontends.deferred` and left unregistered - rather than added
///    dangling. The next pass re-adds it once the backend is present.
///
/// Each half preserves the exact drift/counter/carry-forward semantics
/// of [`reconcile_backends`] / [`reconcile_frontends`].
pub fn reconcile_backends_and_frontends(
    backend_target: &dyn BackendReconcileTarget,
    frontend_target: &dyn FrontendReconcileTarget,
    desired_backends: &[BackendSpec],
    desired_frontends: &[FrontendSpec],
    last_backends: Option<&HashMap<String, BackendSpec>>,
    last_frontends: Option<&HashMap<String, FrontendSpec>>,
) -> ResourceReconcileReport {
    let desired_backend_map: HashMap<String, BackendSpec> = desired_backends
        .iter()
        .map(|b| (b.id.clone(), b.clone()))
        .collect();
    let desired_frontend_map: HashMap<String, FrontendSpec> = desired_frontends
        .iter()
        .map(|f| (f.id.clone(), f.clone()))
        .collect();

    // 1. Frontend removals, so they land before any backend removal.
    let frontend_state = reconcile_remove_phase(
        &desired_frontend_map,
        last_frontends,
        || frontend_target.list(),
        |id| frontend_target.remove(id),
    );

    // 2. Backend removals.
    let backend_state = reconcile_remove_phase(
        &desired_backend_map,
        last_backends,
        || backend_target.list(),
        |id| backend_target.remove(id),
    );

    // 3. Backend additions, before any frontend addition.
    let backends = reconcile_add_phase(
        backend_state,
        |spec| backend_target.add(spec),
        |_id, _spec| Ok(()),
    );

    // 4. Frontend additions, gated on the referenced backend being
    //    present after steps 2-3.
    let present_backends: HashSet<String> = backend_target.list().into_iter().collect();
    let frontends = reconcile_add_phase(
        frontend_state,
        |spec| frontend_target.add(spec),
        |_id, spec: &FrontendSpec| {
            if present_backends.contains(&spec.backend) {
                Ok(())
            } else {
                Err(format!(
                    "deferred: referenced backend {:?} not present",
                    spec.backend
                ))
            }
        },
    );

    ResourceReconcileReport {
        backends,
        frontends,
    }
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
                present: RefCell::new(initial.iter().map(|i| PeerId(*i)).collect()),
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
            let mut v: Vec<PeerId> = self.present.borrow().iter().copied().collect();
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
            transport: PeerTransport::Tcp as i32,
            address: format!("10.0.0.{id}:9000"),
            hca_numa: None,
            labels: Vec::new(),
        }
    }

    fn peer_addr(id: u64, addr: &str) -> PeerSpec {
        PeerSpec {
            id,
            transport: PeerTransport::Tcp as i32,
            address: addr.to_string(),
            hca_numa: None,
            labels: Vec::new(),
        }
    }

    fn peer_labels(id: u64, labels: &[&str]) -> PeerSpec {
        PeerSpec {
            id,
            transport: PeerTransport::Tcp as i32,
            address: format!("10.0.0.{id}:9000"),
            hca_numa: None,
            labels: labels.iter().map(|s| s.to_string()).collect(),
        }
    }

    #[test]
    fn applies_each_peer_in_order() {
        let t = MockTarget::new(&[]);
        let peers = vec![peer(1), peer(2), peer(3)];
        let r = apply_peers_startup(&t, &peers);
        assert_eq!(r.applied, 3);
        assert!(r.failures.is_empty());
        assert_eq!(*t.ops.borrow(), vec![Op::Add(1), Op::Add(2), Op::Add(3)]);
    }

    #[test]
    fn one_failure_does_not_abort_others() {
        let t = MockTarget::new(&[]).with_add_failure(2);
        let peers = vec![peer(1), peer(2), peer(3)];
        let r = apply_peers_startup(&t, &peers);
        assert_eq!(r.applied, 2);
        assert_eq!(r.failures.len(), 1);
        assert_eq!(r.failures[0].0, 2);
        assert_eq!(*t.ops.borrow(), vec![Op::Add(1), Op::Add(2), Op::Add(3)]);
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
        assert_eq!(*t.ops.borrow(), vec![Op::Add(1), Op::Add(2), Op::Add(3)]);
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
        assert_eq!(*t.ops.borrow(), vec![Op::Remove(1), Op::Add(4)]);
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
        prev.insert(PeerId(1), peer_spec_to_connection(&peer_addr(1, "a:1")));
        let peers = vec![peer_addr(1, "b:2")];
        let r = reconcile_peers(&t, &peers, Some(&prev));
        assert_eq!(r.added, 0);
        assert_eq!(r.removed, 0);
        assert_eq!(r.updated, 1);
        assert!(r.failures.is_empty());
        assert_eq!(*t.ops.borrow(), vec![Op::Remove(1), Op::Add(1)]);
        assert_eq!(r.applied[&PeerId(1)].wire_addr, "b:2");
    }

    #[test]
    fn reconcile_label_only_change_churns_connection() {
        // A relabel with identical wire_addr/hca_numa must drive a
        // remove-then-add so the new labels reach the runtime, and
        // the tracked applied spec must reflect the NEW labels.
        let t = MockTarget::new(&[1]);
        let mut prev = HashMap::new();
        prev.insert(
            PeerId(1),
            peer_spec_to_connection(&peer_labels(1, &["us-west"])),
        );
        let peers = vec![peer_labels(1, &["us-east", "rack9"])];
        let r = reconcile_peers(&t, &peers, Some(&prev));
        assert_eq!(r.added, 0);
        assert_eq!(r.removed, 0);
        assert_eq!(r.updated, 1);
        assert!(r.failures.is_empty());
        // The connection was churned via remove-then-add.
        assert_eq!(*t.ops.borrow(), vec![Op::Remove(1), Op::Add(1)]);
        // Applied state carries the freshest desired labels.
        assert_eq!(
            r.applied[&PeerId(1)].labels,
            vec!["us-east".to_string(), "rack9".to_string()]
        );
        // Address/HCA are unchanged.
        assert_eq!(r.applied[&PeerId(1)].wire_addr, "10.0.0.1:9000");
        assert_eq!(r.applied[&PeerId(1)].hca_numa, None);
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
        prev.insert(PeerId(1), peer_spec_to_connection(&peer_addr(1, "a:1")));
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
        assert_eq!(*t2.ops.borrow(), vec![Op::Remove(1), Op::Add(1)]);
    }

    #[test]
    fn reconcile_update_add_failure_does_not_carry_old_spec() {
        // First pass: remove succeeds, add fails. The fabric no
        // longer has the peer at any spec, so the old spec must NOT
        // be carried forward; the id simply drops out of `applied`.
        let t = MockTarget::new(&[1]).with_add_failure(1);
        let mut prev = HashMap::new();
        prev.insert(PeerId(1), peer_spec_to_connection(&peer_addr(1, "a:1")));
        let peers = vec![peer_addr(1, "b:2")];
        let r = reconcile_peers(&t, &peers, Some(&prev));
        assert_eq!(r.updated, 0);
        assert_eq!(r.added, 0);
        assert_eq!(r.failures.len(), 1);
        assert_eq!(r.failures[0].0, PeerId(1));
        assert!(r.failures[0].1.starts_with("update-add:"));
        assert!(!r.applied.contains_key(&PeerId(1)));
        assert_eq!(*t.ops.borrow(), vec![Op::Remove(1), Op::Add(1)]);

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
        prev.insert(PeerId(1), peer_spec_to_connection(&peer_addr(1, "a:1")));
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

    // ---- backend / frontend reconcile ----

    use crate::config::schema::{BackendKind, BackendSpec, FrontendKind, FrontendSpec};

    #[derive(Debug, Clone, PartialEq, Eq)]
    enum SpecOp {
        Add(String),
        Remove(String),
    }

    struct SpecMock {
        ops: RefCell<Vec<SpecOp>>,
        present: RefCell<HashSet<String>>,
        fail_add_on: Option<String>,
        fail_remove_on: Option<String>,
    }

    impl SpecMock {
        fn new(initial: &[&str]) -> Self {
            Self {
                ops: RefCell::new(Vec::new()),
                present: RefCell::new(initial.iter().map(|s| s.to_string()).collect()),
                fail_add_on: None,
                fail_remove_on: None,
            }
        }

        fn with_add_failure(mut self, id: &str) -> Self {
            self.fail_add_on = Some(id.to_string());
            self
        }

        fn with_remove_failure(mut self, id: &str) -> Self {
            self.fail_remove_on = Some(id.to_string());
            self
        }

        fn do_list(&self) -> Vec<String> {
            let mut v: Vec<String> = self.present.borrow().iter().cloned().collect();
            v.sort();
            v
        }

        fn do_add(&self, id: &str) -> Result<(), String> {
            self.ops.borrow_mut().push(SpecOp::Add(id.to_string()));
            if self.fail_add_on.as_deref() == Some(id) {
                return Err("forced failure".to_string());
            }
            self.present.borrow_mut().insert(id.to_string());
            Ok(())
        }

        fn do_remove(&self, id: &str) -> Result<(), String> {
            self.ops.borrow_mut().push(SpecOp::Remove(id.to_string()));
            if self.fail_remove_on.as_deref() == Some(id) {
                return Err("forced failure".to_string());
            }
            self.present.borrow_mut().remove(id);
            Ok(())
        }
    }

    impl BackendReconcileTarget for SpecMock {
        fn list(&self) -> Vec<String> {
            self.do_list()
        }
        fn add(&self, spec: &BackendSpec) -> Result<(), String> {
            self.do_add(&spec.id)
        }
        fn remove(&self, id: &str) -> Result<(), String> {
            self.do_remove(id)
        }
    }

    impl FrontendReconcileTarget for SpecMock {
        fn list(&self) -> Vec<String> {
            self.do_list()
        }
        fn add(&self, spec: &FrontendSpec) -> Result<(), String> {
            self.do_add(&spec.id)
        }
        fn remove(&self, id: &str) -> Result<(), String> {
            self.do_remove(id)
        }
    }

    fn backend(id: &str, endpoint: &str) -> BackendSpec {
        BackendSpec {
            id: id.to_string(),
            kind: BackendKind::Http as i32,
            endpoint: endpoint.to_string(),
            stripe_size_bytes: 4 * 1024 * 1024,
            http_concurrency: 64,
            bucket: None,
        }
    }

    fn frontend(id: &str, backend_id: &str) -> FrontendSpec {
        FrontendSpec {
            id: id.to_string(),
            kind: FrontendKind::Http as i32,
            bind: "0.0.0.0:9000".to_string(),
            backend: backend_id.to_string(),
        }
    }

    #[test]
    fn reconcile_backends_from_empty_adds_all() {
        let t = SpecMock::new(&[]);
        let desired = vec![backend("a", "e1"), backend("b", "e2")];
        let r = reconcile_backends(&t, &desired, None);
        assert_eq!(r.added, 2);
        assert_eq!(r.removed, 0);
        assert_eq!(r.updated, 0);
        assert!(r.failures.is_empty());
        assert_eq!(
            *t.ops.borrow(),
            vec![SpecOp::Add("a".into()), SpecOp::Add("b".into())]
        );
    }

    #[test]
    fn reconcile_backends_swaps_one() {
        let t = SpecMock::new(&["a", "b", "c"]);
        let desired = vec![backend("b", "e"), backend("c", "e"), backend("d", "e")];
        let r = reconcile_backends(&t, &desired, None);
        assert_eq!(r.added, 1);
        assert_eq!(r.removed, 1);
        assert_eq!(r.updated, 0);
        assert!(r.failures.is_empty());
        assert_eq!(
            *t.ops.borrow(),
            vec![SpecOp::Remove("a".into()), SpecOp::Add("d".into())]
        );
    }

    #[test]
    fn reconcile_backends_detects_change_as_update() {
        let t = SpecMock::new(&["a"]);
        let mut prev = HashMap::new();
        prev.insert("a".to_string(), backend("a", "old-endpoint"));
        let desired = vec![backend("a", "new-endpoint")];
        let r = reconcile_backends(&t, &desired, Some(&prev));
        assert_eq!(r.added, 0);
        assert_eq!(r.removed, 0);
        assert_eq!(r.updated, 1);
        assert!(r.failures.is_empty());
        assert_eq!(
            *t.ops.borrow(),
            vec![SpecOp::Remove("a".into()), SpecOp::Add("a".into())]
        );
        assert_eq!(r.applied["a"].endpoint, "new-endpoint");
    }

    #[test]
    fn reconcile_backends_steady_state_is_noop() {
        let t = SpecMock::new(&["a", "b"]);
        let mut prev = HashMap::new();
        prev.insert("a".to_string(), backend("a", "e"));
        prev.insert("b".to_string(), backend("b", "e"));
        let desired = vec![backend("a", "e"), backend("b", "e")];
        let r = reconcile_backends(&t, &desired, Some(&prev));
        assert_eq!(r.added, 0);
        assert_eq!(r.removed, 0);
        assert_eq!(r.updated, 0);
        assert!(r.failures.is_empty());
        assert!(t.ops.borrow().is_empty());
        assert_eq!(r.applied.len(), 2);
    }

    #[test]
    fn reconcile_backends_add_failure_recorded() {
        let t = SpecMock::new(&[]).with_add_failure("b");
        let desired = vec![backend("a", "e"), backend("b", "e"), backend("c", "e")];
        let r = reconcile_backends(&t, &desired, None);
        assert_eq!(r.added, 2);
        assert_eq!(r.failures.len(), 1);
        assert_eq!(r.failures[0].0, "b");
        assert!(r.failures[0].1.starts_with("add: "));
        assert!(r.applied.contains_key("a"));
        assert!(!r.applied.contains_key("b"));
        assert!(r.applied.contains_key("c"));
    }

    #[test]
    fn reconcile_backends_update_remove_failure_preserves_old_spec() {
        let t = SpecMock::new(&["a"]).with_remove_failure("a");
        let mut prev = HashMap::new();
        prev.insert("a".to_string(), backend("a", "old"));
        let desired = vec![backend("a", "new")];
        let r = reconcile_backends(&t, &desired, Some(&prev));
        assert_eq!(r.updated, 0);
        assert_eq!(r.failures.len(), 1);
        assert!(r.failures[0].1.starts_with("update-remove:"));
        assert_eq!(r.applied["a"].endpoint, "old");
        assert_eq!(*t.ops.borrow(), vec![SpecOp::Remove("a".into())]);
    }

    #[test]
    fn reconcile_frontends_add_remove_update() {
        // Add.
        let t = SpecMock::new(&[]);
        let desired = vec![frontend("f1", "b1")];
        let r = reconcile_frontends(&t, &desired, None);
        assert_eq!(r.added, 1);
        assert_eq!(*t.ops.borrow(), vec![SpecOp::Add("f1".into())]);

        // Update: the backend reference changed.
        let t2 = SpecMock::new(&["f1"]);
        let mut prev = HashMap::new();
        prev.insert("f1".to_string(), frontend("f1", "b1"));
        let desired2 = vec![frontend("f1", "b2")];
        let r2 = reconcile_frontends(&t2, &desired2, Some(&prev));
        assert_eq!(r2.updated, 1);
        assert_eq!(
            *t2.ops.borrow(),
            vec![SpecOp::Remove("f1".into()), SpecOp::Add("f1".into())]
        );
        assert_eq!(r2.applied["f1"].backend, "b2");

        // Remove: no longer desired.
        let t3 = SpecMock::new(&["f1"]);
        let r3 = reconcile_frontends(&t3, &[], None);
        assert_eq!(r3.removed, 1);
        assert_eq!(*t3.ops.borrow(), vec![SpecOp::Remove("f1".into())]);
    }

    // ---- combined backend + frontend reconcile (ordering + gating) ----

    /// A single mock that satisfies both reconcile target traits with
    /// *separate* backend and frontend registries and a *shared*
    /// ordered op log, so cross-resource ordering can be asserted.
    struct ComboMock {
        log: RefCell<Vec<String>>,
        backends: RefCell<HashSet<String>>,
        frontends: RefCell<HashSet<String>>,
        fail_backend_add: Option<String>,
    }

    impl ComboMock {
        fn new(backends: &[&str], frontends: &[&str]) -> Self {
            Self {
                log: RefCell::new(Vec::new()),
                backends: RefCell::new(backends.iter().map(|s| s.to_string()).collect()),
                frontends: RefCell::new(frontends.iter().map(|s| s.to_string()).collect()),
                fail_backend_add: None,
            }
        }

        fn with_backend_add_failure(mut self, id: &str) -> Self {
            self.fail_backend_add = Some(id.to_string());
            self
        }

        fn log_pos(&self, entry: &str) -> Option<usize> {
            self.log.borrow().iter().position(|s| s == entry)
        }
    }

    impl BackendReconcileTarget for ComboMock {
        fn list(&self) -> Vec<String> {
            let mut v: Vec<String> = self.backends.borrow().iter().cloned().collect();
            v.sort();
            v
        }
        fn add(&self, spec: &BackendSpec) -> Result<(), String> {
            self.log
                .borrow_mut()
                .push(format!("backend-add:{}", spec.id));
            if self.fail_backend_add.as_deref() == Some(spec.id.as_str()) {
                return Err("forced failure".to_string());
            }
            self.backends.borrow_mut().insert(spec.id.clone());
            Ok(())
        }
        fn remove(&self, id: &str) -> Result<(), String> {
            self.log.borrow_mut().push(format!("backend-remove:{id}"));
            self.backends.borrow_mut().remove(id);
            Ok(())
        }
    }

    impl FrontendReconcileTarget for ComboMock {
        fn list(&self) -> Vec<String> {
            let mut v: Vec<String> = self.frontends.borrow().iter().cloned().collect();
            v.sort();
            v
        }
        fn add(&self, spec: &FrontendSpec) -> Result<(), String> {
            self.log
                .borrow_mut()
                .push(format!("frontend-add:{}", spec.id));
            self.frontends.borrow_mut().insert(spec.id.clone());
            Ok(())
        }
        fn remove(&self, id: &str) -> Result<(), String> {
            self.log.borrow_mut().push(format!("frontend-remove:{id}"));
            self.frontends.borrow_mut().remove(id);
            Ok(())
        }
    }

    #[test]
    fn combined_adds_backend_before_frontend() {
        let combo = ComboMock::new(&[], &[]);
        let backends = vec![backend("b", "e")];
        let frontends = vec![frontend("f", "b")];
        let r = reconcile_backends_and_frontends(&combo, &combo, &backends, &frontends, None, None);
        assert_eq!(r.backends.added, 1);
        assert_eq!(r.frontends.added, 1);
        assert!(r.frontends.deferred.is_empty());
        assert!(r.backends.failures.is_empty());
        assert!(r.frontends.failures.is_empty());
        // Both are now present.
        assert!(combo.backends.borrow().contains("b"));
        assert!(combo.frontends.borrow().contains("f"));
        // The backend add was ordered before the frontend add.
        let bi = combo.log_pos("backend-add:b").expect("backend add logged");
        let fi = combo
            .log_pos("frontend-add:f")
            .expect("frontend add logged");
        assert!(
            bi < fi,
            "expected backend-add before frontend-add: {:?}",
            combo.log.borrow()
        );
    }

    #[test]
    fn combined_removes_frontend_before_backend() {
        let combo = ComboMock::new(&["b"], &["f"]);
        let mut last_backends = HashMap::new();
        last_backends.insert("b".to_string(), backend("b", "e"));
        let mut last_frontends = HashMap::new();
        last_frontends.insert("f".to_string(), frontend("f", "b"));
        let r = reconcile_backends_and_frontends(
            &combo,
            &combo,
            &[],
            &[],
            Some(&last_backends),
            Some(&last_frontends),
        );
        assert_eq!(r.backends.removed, 1);
        assert_eq!(r.frontends.removed, 1);
        assert!(combo.backends.borrow().is_empty());
        assert!(combo.frontends.borrow().is_empty());
        // The frontend removal was ordered before the backend removal.
        let fr = combo
            .log_pos("frontend-remove:f")
            .expect("frontend remove logged");
        let br = combo
            .log_pos("backend-remove:b")
            .expect("backend remove logged");
        assert!(
            fr < br,
            "expected frontend-remove before backend-remove: {:?}",
            combo.log.borrow()
        );
    }

    #[test]
    fn combined_defers_frontend_when_backend_add_fails() {
        let combo = ComboMock::new(&[], &[]).with_backend_add_failure("b");
        let backends = vec![backend("b", "e")];
        let frontends = vec![frontend("f", "b")];
        let r = reconcile_backends_and_frontends(&combo, &combo, &backends, &frontends, None, None);
        // Backend add failed; backend is not present.
        assert_eq!(r.backends.added, 0);
        assert_eq!(r.backends.failures.len(), 1);
        assert!(!combo.backends.borrow().contains("b"));
        // Frontend was deferred, not added: no dangling registration.
        assert_eq!(r.frontends.added, 0);
        assert_eq!(r.frontends.deferred.len(), 1);
        assert_eq!(r.frontends.deferred[0].0, "f");
        assert!(!combo.frontends.borrow().contains("f"));
        assert!(
            combo.log_pos("frontend-add:f").is_none(),
            "frontend add must not have been attempted: {:?}",
            combo.log.borrow()
        );
    }

    #[test]
    fn combined_defers_frontend_when_backend_absent() {
        // Backend referenced by the frontend is not defined at all.
        let combo = ComboMock::new(&[], &[]);
        let r = reconcile_backends_and_frontends(
            &combo,
            &combo,
            &[],
            &[frontend("f", "ghost")],
            None,
            None,
        );
        assert_eq!(r.frontends.added, 0);
        assert_eq!(r.frontends.deferred.len(), 1);
        assert_eq!(r.frontends.deferred[0].0, "f");
        assert!(!combo.frontends.borrow().contains("f"));
    }

    #[test]
    fn combined_adds_frontend_when_backend_already_present() {
        // Backend already present from a prior pass; the frontend
        // references it and is added (gate passes) without deferral.
        let combo = ComboMock::new(&["b"], &[]);
        let mut last_backends = HashMap::new();
        last_backends.insert("b".to_string(), backend("b", "e"));
        let backends = vec![backend("b", "e")];
        let frontends = vec![frontend("f", "b")];
        let r = reconcile_backends_and_frontends(
            &combo,
            &combo,
            &backends,
            &frontends,
            Some(&last_backends),
            None,
        );
        assert_eq!(r.frontends.added, 1);
        assert!(r.frontends.deferred.is_empty());
        assert!(combo.frontends.borrow().contains("f"));
    }
}
