//! Full fidelity control plane: real `racer-cluster-control` authorities
//! against the in-memory Kubernetes API, as roles of one world.
//!
//! Node 0 runs the fake API server ([`KubeRole`]); node `i` (1-based) runs
//! the [`Authority`] of Pod `p{i}` ([`AuthorityRole`]). All nodes share one
//! simulated network ([`Scenario::shared_network`]); each node keeps its own
//! [`racer_io::ctx::VirtualClock`], so clock rate faults skew one authority
//! against the others and the API server. Node 0 is never faulted
//! ([`crate::scenario::FaultMix::first_node`]).
//!
//! Faults, all drawn from the world [`crate::Choices`] stream:
//! - crash and restart of an authority (the restart builds a new
//!   [`Authority`] with the same Pod identity: name, UID, Node, address);
//! - partition and heal between an authority and the API server or another
//!   authority;
//! - clock rate skew of an authority, within +-10%;
//! - spurious 409 Conflict on the next API write and dropped watch streams,
//!   drawn by the API server role on its steps ([`ApiFaults`]).
//!
//! Invariants ([`invariants`]) observe every live authority and the
//! committed state record after every step. See `dst/MODEL.md` (section
//! "Full fidelity: control plane").
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
    rc::Rc,
};

use racer_cluster_control::{
    Authority, Next, Role,
    rules::state::Term,
    simulation::{self, Committed, FakeKube, KubeView},
};
use racer_io::ring::SocketAddress;

use crate::{
    Scenario,
    choices::Choices,
    engine::{Nanos, SECONDS},
    invariants::{Class, Invariant, Probe},
    scenario::FaultMix,
    world::{NodeEnv, Roles, SimRole, Trace},
};

/// Expiry of every Pod's control token (Unix seconds), far past any virtual
/// wall time a run reaches.
const TOKEN_EXP: u64 = 4_000_000_000;

/// Longest a cluster may go without a primary once every authority is up,
/// unpartitioned and no fault happened: Lease expiry (30 s) on a clock up
/// to 10% slow, claim jitter, bootstrap and a failed first attempt.
pub const PRIMARY_BOUND: Nanos = 120 * SECONDS;

const TAG_TERM: u64 = 0x4155_5448_0001;
const TAG_COMMIT: u64 = 0x4155_5448_0002;
const TAG_API_FAULT: u64 = 0x4155_5448_0003;

/// API faults the server role draws on each of its steps.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct ApiFaults {
    /// Chance per million API server steps of a spurious 409 on the next
    /// write.
    pub conflict_ppm: u64,
    /// Chance per million API server steps of ending every watch stream.
    pub watch_drop_ppm: u64,
}

/// What the control plane invariants have observed so far in one run.
#[derive(Default)]
struct Observed {
    /// Every primary term seen, keyed by `(lease UID, transitions)`, with
    /// the node that led it.
    leaders: BTreeMap<(String, u64), u32>,
    /// Newest term each node led, by `(lease UID, transitions)`.
    last_term: BTreeMap<u32, (String, u64)>,
    /// Newest committed record.
    committed: Option<Committed>,
    /// World time a primary was last seen.
    last_primary: Option<Nanos>,
}

/// Per-run control plane state on the world [`crate::world::Board`].
#[derive(Default)]
pub struct ControlPlane {
    kube: RefCell<Option<KubeView>>,
    live: RefCell<BTreeMap<u32, Rc<RefCell<Authority>>>>,
    observed: RefCell<Observed>,
    trace: RefCell<Option<Trace>>,
}

impl ControlPlane {
    /// The API server's view, once node 0 started.
    pub fn kube(&self) -> Option<KubeView> {
        self.kube.borrow().clone()
    }

    /// Fold an observation into the world trace, so a divergence in what
    /// the control plane did (not only in the schedule) fails the
    /// determinism double-run.
    fn record(&self, tag: u64, a: u64, b: u64) {
        if let Some(t) = self.trace.borrow().as_ref() {
            t.record(tag, a, b);
        }
    }

    /// Run `f` over every live authority, by node.
    pub fn each(&self, mut f: impl FnMut(u32, &Authority)) {
        for (node, a) in self.live.borrow().iter() {
            f(*node, &a.borrow());
        }
    }
}

/// Pod index of node `node` (identity); node 0 is the API server.
pub fn endpoint(node: u32) -> SocketAddress {
    if node == 0 {
        simulation::api_endpoint()
    } else {
        simulation::pod_endpoint(node as usize)
    }
}

/// Whether `next` asks to run earlier than `last` did, so the scheduler
/// must look at the role again even though its last step said otherwise.
fn earlier(next: Next, last: Next) -> bool {
    match (next, last) {
        (Next::Ready, _) => true,
        (Next::WaitUntil(a), Next::WaitUntil(b)) => a < b,
        (Next::WaitUntil(_), Next::Idle) => true,
        _ => false,
    }
}

/// The fake API server.
pub struct KubeRole {
    kube: FakeKube,
    view: KubeView,
    choices: Choices,
    faults: ApiFaults,
    trace: Trace,
    last: Cell<Next>,
}

impl SimRole for KubeRole {
    fn step(&mut self) -> racer_io::Result<Next> {
        if self.faults.conflict_ppm > 0 && self.choices.chance(self.faults.conflict_ppm, 1_000_000)
        {
            self.trace.record(TAG_API_FAULT, 1, 0);
            self.view.conflict_next_write();
        }
        if self.faults.watch_drop_ppm > 0
            && self.choices.chance(self.faults.watch_drop_ppm, 1_000_000)
        {
            self.trace.record(TAG_API_FAULT, 2, 0);
            self.view.drop_watches();
        }
        let next = self.kube.turn().map_err(racer_io::Error::from_io)?;
        self.last.set(next);
        Ok(next)
    }

    fn woken(&self) -> bool {
        earlier(self.kube.next(), self.last.get())
    }

    fn pending(&self) -> bool {
        false
    }
}

/// One Pod's authority.
pub struct AuthorityRole {
    node: u32,
    os: racer_io::sim::SimOs,
    authority: Rc<RefCell<Authority>>,
    plane: Rc<ControlPlane>,
    last: Cell<Next>,
}

impl SimRole for AuthorityRole {
    fn step(&mut self) -> racer_io::Result<Next> {
        let _label = self.os.enter_endpoint(endpoint(self.node));
        let next = self.authority.borrow_mut().turn();
        self.last.set(next);
        Ok(next)
    }

    fn woken(&self) -> bool {
        earlier(self.authority.borrow().next(), self.last.get())
    }

    fn pending(&self) -> bool {
        !matches!(self.last.get(), Next::Done)
    }
}

impl Drop for AuthorityRole {
    /// A crash drops the role: the authority leaves the observed set.
    fn drop(&mut self) {
        let mut live = self.plane.live.borrow_mut();
        if live
            .get(&self.node)
            .is_some_and(|a| Rc::ptr_eq(a, &self.authority))
        {
            live.remove(&self.node);
        }
    }
}

/// Node factory: node 0 serves the fake API for `pods` Pods, node `i`
/// runs Pod `i`'s authority.
pub fn factory(pods: u32, faults: ApiFaults) -> impl Fn(&NodeEnv) -> racer_io::Result<Roles> {
    move |env| {
        let plane = env.board.get::<ControlPlane>();
        // Seeds come from the choice stream, so key ids, jitter and claim
        // delays replay with the run.
        let seed = env.choices.raw();
        if env.index == 0 {
            let kube = FakeKube::new(env.os.clone(), &env.clock, seed, pods as usize)
                .map_err(racer_io::Error::from_io)?;
            let view = kube.view();
            *plane.kube.borrow_mut() = Some(view.clone());
            *plane.trace.borrow_mut() = Some(env.trace.clone());
            let role = KubeRole {
                kube,
                view,
                choices: env.choices.clone(),
                faults,
                trace: env.trace.clone(),
                last: Cell::new(Next::Ready),
            };
            return Ok(vec![(
                "kube-api".into(),
                Box::new(role) as Box<dyn SimRole>,
            )]);
        }
        let i = env.index as usize;
        let config =
            simulation::pod_config(&env.os, i, TOKEN_EXP).map_err(racer_io::Error::from_io)?;
        let authority = {
            let _label = env.os.enter_endpoint(endpoint(env.index));
            Authority::simulated(config, env.os.clone(), &env.clock, seed).map_err(|e| {
                env.report
                    .violation(format!("node {i} authority start: {e:?}"));
                racer_io::Error::Unavailable
            })?
        };
        let authority = Rc::new(RefCell::new(authority));
        plane.live.borrow_mut().insert(env.index, authority.clone());
        let role = AuthorityRole {
            node: env.index,
            os: env.os.clone(),
            authority,
            plane,
            last: Cell::new(Next::Ready),
        };
        Ok(vec![(
            "authority".into(),
            Box::new(role) as Box<dyn SimRole>,
        )])
    }
}

fn plane(probe: &Probe<'_>) -> Rc<ControlPlane> {
    probe.board.get::<ControlPlane>()
}

fn key(term: &Term) -> (String, u64) {
    (term.lease_uid.clone(), term.transitions)
}

fn pod_holds(holder: &str, node: u32) -> bool {
    let name = format!("p{node}");
    holder == name || holder.starts_with(&format!("{name}/"))
}

/// The control plane invariants, all checked after every step.
pub mod invariants {
    use super::*;

    /// At most one authority acts as primary at any instant of world time,
    /// and every term is led by one Pod only.
    pub fn single_primary() -> Invariant {
        Invariant {
            name: "single-primary".into(),
            class: Class::Safety,
            check: Rc::new(|probe| {
                let plane = plane(probe);
                let mut primaries = Vec::new();
                plane.each(|node, a| {
                    if let Some(t) = a.primary_term() {
                        primaries.push((node, t));
                    }
                });
                if primaries.len() > 1 {
                    return Err(format!("two primaries: {primaries:?}"));
                }
                let mut obs = plane.observed.borrow_mut();
                if !primaries.is_empty() {
                    obs.last_primary = Some(probe.now);
                }
                for (node, term) in primaries {
                    let k = key(&term);
                    match obs.leaders.get(&k) {
                        Some(n) if *n != node => {
                            return Err(format!("term {k:?} led by node {n} and node {node}"));
                        }
                        Some(_) => {}
                        None => {
                            plane.record(TAG_TERM, u64::from(node), k.1);
                            obs.leaders.insert(k, node);
                        }
                    }
                }
                Ok(())
            }),
        }
    }

    /// Each node only ever moves to newer terms of a Lease (a recreated
    /// Lease, a new UID, starts over).
    pub fn term_monotonic() -> Invariant {
        Invariant {
            name: "term-monotonic".into(),
            class: Class::Safety,
            check: Rc::new(|probe| {
                let plane = plane(probe);
                let mut out = Ok(());
                let mut obs = plane.observed.borrow_mut();
                plane.each(|node, a| {
                    let Some(term) = a.primary_term() else {
                        return;
                    };
                    let k = key(&term);
                    if let Some(last) = obs.last_term.get(&node)
                        && last.0 == k.0
                        && last.1 > k.1
                    {
                        out = Err(format!("node {node} led term {last:?} then {k:?}"));
                    }
                    obs.last_term.insert(node, k);
                });
                out
            }),
        }
    }

    /// The committed record never goes back: its sequence, keyring
    /// generation and term transitions only grow (for one primary Lease).
    pub fn committed_monotonic() -> Invariant {
        Invariant {
            name: "committed-monotonic".into(),
            class: Class::Safety,
            check: Rc::new(|probe| {
                let plane = plane(probe);
                let Some(kube) = plane.kube() else {
                    return Ok(());
                };
                let Some(now) = kube.committed() else {
                    return Ok(());
                };
                let mut obs = plane.observed.borrow_mut();
                if let Some(last) = &obs.committed {
                    if *last == now {
                        return Ok(());
                    }
                    let same_lease = last.term.lease_uid == now.term.lease_uid;
                    if now.sequence < last.sequence
                        || now.keyring_generation < last.keyring_generation
                        || (same_lease && now.term.transitions < last.term.transitions)
                    {
                        return Err(format!("committed {last:?} then {now:?}"));
                    }
                }
                plane.record(TAG_COMMIT, now.sequence, now.keyring_generation);
                obs.committed = Some(now);
                Ok(())
            }),
        }
    }

    /// A ready (serving) seat holds its seat Lease in the API, and no two
    /// ready authorities serve the same seat.
    pub fn seat_fresh() -> Invariant {
        Invariant {
            name: "seat-serves-only-while-fresh".into(),
            class: Class::Safety,
            check: Rc::new(|probe| {
                let plane = plane(probe);
                let Some(kube) = plane.kube() else {
                    return Ok(());
                };
                let mut seats: BTreeMap<usize, u32> = BTreeMap::new();
                let mut out = Ok(());
                plane.each(|node, a| {
                    let d = a.diagnostics();
                    let (Role::Seat(i) | Role::Primary(i)) = d.role else {
                        return;
                    };
                    if !d.ready || out.is_err() {
                        return;
                    }
                    if let Some(other) = seats.insert(i, node) {
                        out = Err(format!("nodes {other} and {node} both serve seat {i}"));
                        return;
                    }
                    let holder = kube.seat_holder(i);
                    if !pod_holds(&holder, node) {
                        out = Err(format!(
                            "node {node} serves seat {i} held by {holder:?} in the API"
                        ));
                    }
                });
                out
            }),
        }
    }

    /// Every live authority has a wake source: a stopped or idle authority
    /// with nothing scheduled would never renew its Leases.
    pub fn has_wake() -> Invariant {
        Invariant {
            name: "authority-has-wake".into(),
            class: Class::Safety,
            check: Rc::new(|probe| {
                let plane = plane(probe);
                let mut out = Ok(());
                plane.each(|node, a| {
                    if matches!(a.next(), Next::Done | Next::Idle) && out.is_ok() {
                        out = Err(format!("node {node} authority is {:?}", a.next()));
                    }
                });
                out
            }),
        }
    }

    /// Bounded liveness: once every node is up, nothing is partitioned and
    /// no fault happened for `bound`, a primary was seen within the last
    /// `bound`.
    pub fn eventual_primary(bound: Nanos) -> Invariant {
        Invariant {
            name: "eventual-primary".into(),
            class: Class::Safety,
            check: Rc::new(move |probe| {
                if probe.partitions > 0 || probe.up.iter().any(|u| !u) {
                    return Ok(());
                }
                let quiet = probe.last_fault.unwrap_or(0);
                if probe.now < quiet.saturating_add(bound) {
                    return Ok(());
                }
                let plane = plane(probe);
                let seen = plane.observed.borrow().last_primary;
                match seen {
                    Some(at) if at.saturating_add(bound) >= probe.now => Ok(()),
                    _ => Err(format!(
                        "no primary since {seen:?}; last fault at {quiet} ns"
                    )),
                }
            }),
        }
    }
}

/// A control plane scenario: `pods` authorities plus the API server.
pub fn scenario(name: &str, pods: u32, mix: FaultMix, api: ApiFaults, end: Nanos) -> Scenario {
    Scenario::new(name)
        .nodes(pods + 1)
        .shared_network(endpoint)
        .roles(factory(pods, api))
        .fault_mix(FaultMix {
            first_node: 1,
            ..mix
        })
        .with(invariants::single_primary())
        .with(invariants::term_monotonic())
        .with(invariants::committed_monotonic())
        .with(invariants::seat_fresh())
        .with(invariants::has_wake())
        .with(invariants::eventual_primary(PRIMARY_BOUND))
        .end(end)
        .max_steps(5_000_000)
        .charge_bytes(false)
}
