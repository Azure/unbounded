//! Scenario description: a small builder, not a DSL file.
//!
//! A [`Scenario`] says how many nodes exist, what roles each node runs (the
//! node factory), which faults happen when (fixed and drawn from
//! [`Choices`]), which invariants hold, the cost table, and the run bounds.
//! It is immutable and reusable: every run builds a fresh [`World`], which is
//! what makes the determinism double-run meaningful.
//!
//! ```
//! use racer_dst::{Scenario, world::Fault, engine::MILLIS};
//! let s = Scenario::new("example")
//!     .nodes(3)
//!     .fault(5 * MILLIS, Fault::Crash { node: 1 })
//!     .fault(9 * MILLIS, Fault::Restart { node: 1 })
//!     .end(100 * MILLIS);
//! assert_eq!(s.nodes, 3);
//! ```
use std::rc::Rc;

use crate::{
    choices::Choices,
    costs::Costs,
    engine::{MILLIS, Nanos, SECONDS},
    invariants::{Class, Invariant, Probe},
    model::ModelSpec,
    world::{Fault, NodeFactory, Outcome, World},
};

/// Which interpreter runs the nodes. Both share the choice stream, engine,
/// cost table and determinism checks.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Fidelity {
    /// Real application roles on `Driver::Sim`.
    #[default]
    Full,
    /// Byte-free interpreter over `WorkerCore` on the FIFO cost model
    /// ([`crate::model`]); needs a [`Scenario::model`] spec.
    Model,
}

/// Random fault generation drawn from [`Choices`]. Each kind's weight is
/// relative; `max_faults` bounds how many are drawn (the count itself is a
/// choice, so shrinking removes faults first).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct FaultMix {
    /// Upper bound on drawn faults.
    pub max_faults: u32,
    /// Crash followed by a restart after up to `max_downtime`.
    pub crash_restart: u64,
    /// Partition followed by a heal. Only takes effect with a shared network
    /// ([`Scenario::shared_network`]); otherwise it is recorded in the trace only.
    pub partition: u64,
    /// Slow peer.
    pub slow_peer: u64,
    /// Wall-clock skew of up to +-`max_skew_ns`.
    pub clock_skew: u64,
    /// Clock rate drift of up to +-`max_rate_ppm`.
    pub clock_rate: u64,
    /// Longest crash downtime or partition.
    pub max_downtime: Nanos,
    /// Largest wall-clock skew.
    pub max_skew_ns: u64,
    /// Largest clock rate drift in parts per million (capped at 999_999).
    pub max_rate_ppm: u32,
    /// Lowest node index a fault targets; nodes below it (for example a
    /// simulated API server) are never crashed, slowed or skewed. The second
    /// node of a partition may be any node.
    pub first_node: u32,
}

/// One scenario.
#[derive(Clone)]
pub struct Scenario {
    /// Name used by `DST_FILTER` and seed files.
    pub name: String,
    /// Node count.
    pub nodes: u32,
    /// Sites for the RTT matrix.
    pub sites: u32,
    /// Fidelity.
    pub fidelity: Fidelity,
    /// Builds every node incarnation.
    pub factory: NodeFactory,
    /// Fixed faults.
    pub faults: Vec<(Nanos, Fault)>,
    /// Random faults.
    pub fault_mix: FaultMix,
    /// Invariants.
    pub invariants: Vec<Invariant>,
    /// Cost table.
    pub costs: Costs,
    /// Virtual end time and fault horizon: drawn faults are scheduled before
    /// it.
    pub end: Nanos,
    /// Extra virtual time after `end` in which a full-fidelity run that
    /// requires quiescence may finish work already in flight (for example a
    /// node restarted shortly before `end`). Liveness checks run at the end
    /// of this window, not at `end`.
    pub settle: Nanos,
    /// Step budget (`DST_STEPS` overrides it in the runner).
    pub max_steps: u64,
    /// Steps allowed without virtual time advancing (busy-loop detector).
    pub busy_limit: u64,
    /// Whether exhausting `max_steps` fails the run (on by default). Off
    /// lets ad-hoc short runs (`DST_STEPS`) stop early without failing.
    pub step_budget_fails: bool,
    /// Whether a Full-fidelity run must go quiescent before `end`. Implied
    /// by any [`Class::Resource`](crate::invariants::Class) invariant, since
    /// those only run at quiescence.
    pub expect_quiescent: bool,
    /// Whether node operating systems draw operation delays, completion
    /// order and cancel order from [`Choices`] (on by default). Off keeps
    /// FIFO order.
    pub seeded_os: bool,
    /// Whether node OS operations are charged by size as well as kind (on
    /// by default). Off charges every operation as if it moved no bytes,
    /// for roles whose message sizes carry host entropy (TLS handshakes
    /// with randomized signatures) so sizes must not move time.
    pub charge_bytes: bool,
    /// The Model fidelity workload ([`Fidelity::Model`] only).
    pub model: Option<Rc<ModelSpec>>,
    /// One simulated OS (network and disk) shared by every node, with each
    /// node's endpoint label for [`Fault::Partition`]; `None` gives every
    /// node its own OS.
    pub network: Option<Endpoints>,
}

/// Endpoint label of each node on a shared network.
pub type Endpoints = Rc<dyn Fn(u32) -> racer_io::ring::SocketAddress>;

impl std::fmt::Debug for Scenario {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Scenario")
            .field("name", &self.name)
            .field("nodes", &self.nodes)
            .field("fidelity", &self.fidelity)
            .field("faults", &self.faults)
            .field("fault_mix", &self.fault_mix)
            .field("invariants", &self.invariants)
            .field("end", &self.end)
            .finish()
    }
}

impl Scenario {
    /// One node with no roles, ending after 10 s of virtual time.
    pub fn new(name: impl Into<String>) -> Self {
        Self {
            name: name.into(),
            nodes: 1,
            sites: 1,
            fidelity: Fidelity::Full,
            factory: Rc::new(|_| Ok(Vec::new())),
            faults: Vec::new(),
            fault_mix: FaultMix::default(),
            invariants: Vec::new(),
            costs: Costs::checked_in(),
            end: 10 * SECONDS,
            settle: 0,
            max_steps: 1_000_000,
            busy_limit: 100_000,
            step_budget_fails: true,
            expect_quiescent: false,
            seeded_os: true,
            charge_bytes: true,
            model: None,
            network: None,
        }
    }

    /// Run at Model fidelity with `spec`.
    pub fn model(mut self, spec: ModelSpec) -> Self {
        self.fidelity = Fidelity::Model;
        self.model = Some(Rc::new(spec));
        self
    }

    /// Node count.
    pub fn nodes(mut self, nodes: u32) -> Self {
        self.nodes = nodes;
        self
    }

    /// Site count.
    pub fn sites(mut self, sites: u32) -> Self {
        self.sites = sites;
        self
    }

    /// Fidelity.
    pub fn fidelity(mut self, fidelity: Fidelity) -> Self {
        self.fidelity = fidelity;
        self
    }

    /// Node factory.
    pub fn roles(
        mut self,
        factory: impl Fn(&crate::world::NodeEnv) -> racer_io::Result<crate::world::Roles> + 'static,
    ) -> Self {
        self.factory = Rc::new(factory);
        self
    }

    /// A fault at `at`.
    pub fn fault(mut self, at: Nanos, fault: Fault) -> Self {
        self.faults.push((at, fault));
        self
    }

    /// Random faults.
    pub fn fault_mix(mut self, mix: FaultMix) -> Self {
        self.fault_mix = mix;
        self
    }

    /// An invariant.
    pub fn invariant(
        mut self,
        name: impl Into<String>,
        class: Class,
        check: impl Fn(&Probe<'_>) -> Result<(), String> + 'static,
    ) -> Self {
        self.invariants.push(Invariant {
            name: name.into(),
            class,
            check: Rc::new(check),
        });
        self
    }

    /// A prebuilt invariant.
    pub fn with(mut self, invariant: Invariant) -> Self {
        self.invariants.push(invariant);
        self
    }

    /// Cost table.
    pub fn costs(mut self, costs: Costs) -> Self {
        self.costs = costs;
        self
    }

    /// Virtual end time.
    pub fn end(mut self, end: Nanos) -> Self {
        self.end = end;
        self
    }

    /// Extra virtual time after [`Scenario::end`] in which a run that must be
    /// quiescent may finish work already in flight.
    pub fn settle(mut self, settle: Nanos) -> Self {
        self.settle = settle;
        self
    }

    /// Step budget.
    pub fn max_steps(mut self, steps: u64) -> Self {
        self.max_steps = steps;
        self
    }

    /// Let a run that exhausts `max_steps` stop without failing.
    pub fn allow_truncation(mut self) -> Self {
        self.step_budget_fails = false;
        self
    }

    /// Fail a Full-fidelity run that reaches `end` without going quiescent.
    pub fn expect_quiescent(mut self) -> Self {
        self.expect_quiescent = true;
        self
    }

    /// Draw node OS operation delays, completion order and cancel order from
    /// [`Choices`] (`SimOs::with_choices`), or keep FIFO order.
    pub fn seeded_os(mut self, seeded: bool) -> Self {
        self.seeded_os = seeded;
        self
    }

    /// Share one simulated OS between all nodes; `endpoint(node)` is the
    /// label the node's sockets carry, which partitions cut.
    pub fn shared_network(
        mut self,
        endpoint: impl Fn(u32) -> racer_io::ring::SocketAddress + 'static,
    ) -> Self {
        self.network = Some(Rc::new(endpoint));
        self
    }

    /// Charge node OS operations by size ([`Scenario::charge_bytes`]).
    pub fn charge_bytes(mut self, by_size: bool) -> Self {
        self.charge_bytes = by_size;
        self
    }

    /// Busy-loop limit.
    pub fn busy_limit(mut self, steps: u64) -> Self {
        self.busy_limit = steps;
        self
    }

    /// Fixed faults plus faults drawn from `choices`, in schedule order.
    pub fn fault_schedule(&self, choices: &Choices) -> Vec<(Nanos, Fault)> {
        let mut out = self.faults.clone();
        let mix = self.fault_mix;
        let weights = [
            mix.crash_restart,
            mix.partition,
            mix.slow_peer,
            mix.clock_skew,
            mix.clock_rate,
        ];
        let count = choices.below(u64::from(mix.max_faults) + 1);
        if self.nodes == 0 || weights.iter().all(|w| *w == 0) {
            return out;
        }
        let first = mix.first_node.min(self.nodes - 1);
        let node = || first + choices.below(u64::from(self.nodes - first)) as u32;
        let any = || choices.below(u64::from(self.nodes)) as u32;
        let downtime = mix.max_downtime.max(MILLIS);
        for _ in 0..count {
            let at = choices.below(self.end);
            match choices.weighted(&weights) {
                0 => {
                    let node = node();
                    out.push((at, Fault::Crash { node }));
                    out.push((at + 1 + choices.below(downtime), Fault::Restart { node }));
                }
                1 => {
                    let (a, b) = (node(), any());
                    out.push((at, Fault::Partition { a, b }));
                    out.push((at + 1 + choices.below(downtime), Fault::Heal { a, b }));
                }
                2 => out.push((
                    at,
                    Fault::SlowPeer {
                        node: node(),
                        factor: choices.range(1, 16) as u8,
                    },
                )),
                4 => {
                    let max = u64::from(mix.max_rate_ppm.min(999_999));
                    let ppm = choices.below(max * 2 + 1) as i64 - max as i64;
                    out.push((
                        at,
                        Fault::ClockRate {
                            node: node(),
                            ppm: ppm as i32,
                        },
                    ));
                }
                _ => {
                    let skew = choices.below(mix.max_skew_ns.saturating_mul(2) + 1) as i64
                        - mix.max_skew_ns as i64;
                    out.push((
                        at,
                        Fault::ClockSkew {
                            node: node(),
                            offset_ns: skew,
                        },
                    ));
                }
            }
        }
        out
    }

    /// Run once over `choices`.
    pub fn run(&self, choices: Choices) -> Outcome {
        if self.fidelity == Fidelity::Model {
            return crate::model::run(self, choices).0;
        }
        match World::new(self, choices.clone()) {
            Ok(world) => world.run(),
            Err(failure) => Outcome {
                trace: 0,
                steps: 0,
                end: 0,
                draws: choices.drawn(),
                quiescent: false,
                failure: Some(failure),
            },
        }
    }
}
