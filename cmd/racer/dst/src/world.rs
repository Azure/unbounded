//! The simulated world: nodes, their roles, and the one-thread scheduler.
//!
//! A [`World`] owns N nodes. Each node is a set of roles ([`SimRole`]) built
//! by the scenario's node factory from a [`NodeEnv`]: the node's simulated
//! operating system ([`SimOs`], disk and network), its virtual clock, the
//! shared [`Choices`] stream and the trace. Production code runs each role on
//! its own pinned thread with `racer_io::exec::run`; here one thread steps all
//! roles of all nodes, and the scheduler, not the roles, moves time.
//!
//! # Scheduling
//!
//! Each iteration:
//! 1. Deliver world events (faults) due at the current instant.
//! 2. Collect runnable roles: last step said `Ready`, its `WaitUntil` deadline
//!    has passed, or a wake source fired ([`SimRole::woken`]: a CQE, a
//!    cross-role mailbox wake, a ready task). Order is `(node, role)` index.
//! 3. If any are runnable, [`Choices`] picks one and it takes one bounded
//!    step. Safety invariants and role-reported violations are checked.
//! 4. Otherwise time jumps to the earliest of every role's deadline and the
//!    next world event. With nothing left at all the world is quiescent: a
//!    role that still reports pending work ([`SimRole::pending`]) has no wake
//!    source that could ever finish it, which is a liveness failure.
//!
//! [`Scenario::end`] is the fault horizon. A scenario with `settle` keeps
//! stepping up to `end + settle` so work already in flight (for example a
//! node restarted just before `end`) can finish before quiescence is
//! required.
//!
//! Roles never `park`: the simulated reactor's park would advance the shared
//! clock on its own. Time only moves in step 4.
//!
//! # Clocks
//!
//! Every node has its own [`VirtualClock`] anchored at one world origin. World
//! time is each clock's reference time, so all clocks advance together. Wall
//! time can be shifted per node ([`Fault::ClockSkew`]) and local monotonic
//! time can drift ([`Fault::ClockRate`]) or step forward
//! ([`Fault::ClockJump`]); role deadlines are mapped back to world time
//! through their node's clock.
use std::{
    any::Any,
    cell::{Cell, RefCell},
    collections::{BTreeMap, BTreeSet},
    rc::Rc,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use racer_io::{
    Driver, Reactor, Rng, WorkerCtx,
    ctx::VirtualClock,
    exec::{Lane, Next, Role, Tick},
    ring::Config,
    sim::SimOs,
};

use crate::{
    choices::Choices,
    engine::{Engine, Nanos, TraceHash},
    invariants::{Class, Probe, RoleState},
    resources::Fabric,
    scenario::Scenario,
};

/// A role as the DST scheduler sees it.
///
/// [`LaneRole`] adapts a `racer_io` [`Lane`]. Other roles (crypto helpers,
/// RDMA owners) implement this directly over their racer-io parts.
pub trait SimRole {
    /// One bounded nonblocking step (`racer_io::exec::Role::step`).
    fn step(&mut self) -> racer_io::Result<Next>;

    /// Whether a wake source fired since the last step: a completion is
    /// ready, a task was woken, or a cross-thread wake is pending.
    fn woken(&self) -> bool;

    /// Whether the role holds unfinished work (live tasks or in-flight I/O).
    /// Used only for the "pending work with no wake" liveness check.
    fn pending(&self) -> bool;
}

/// [`SimRole`] over a racer-io lane.
pub struct LaneRole<T>(pub Lane<T>);

impl<T: Tick> SimRole for LaneRole<T> {
    fn step(&mut self) -> racer_io::Result<Next> {
        self.0.step()
    }

    fn woken(&self) -> bool {
        self.0.has_pending_wake()
    }

    fn pending(&self) -> bool {
        self.0.has_pending_work()
    }
}

/// A lane [`Tick`] that finishes when its flag is set.
#[derive(Clone, Debug, Default)]
pub struct DoneFlag(pub Rc<Cell<bool>>);

impl DoneFlag {
    /// Mark the lane finished.
    pub fn set(&self) {
        self.0.set(true);
    }
}

impl Tick for DoneFlag {
    fn done(&self) -> bool {
        self.0.get()
    }
}

/// World faults. Crash, restart, slow peer and clock faults always act.
/// Partition and heal act when the scenario shares one simulated network
/// between nodes ([`crate::Scenario::shared_network`]); otherwise they are
/// recorded in the trace only.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Fault {
    /// Power loss: drop every role and crash the node's simulated disk.
    Crash {
        /// Node index.
        node: u32,
    },
    /// Rebuild the node's roles through the factory on the surviving disk.
    Restart {
        /// Node index.
        node: u32,
    },
    /// Cut the network between two nodes (needs a shared network).
    Partition {
        /// First node.
        a: u32,
        /// Second node.
        b: u32,
    },
    /// Undo [`Fault::Partition`].
    Heal {
        /// First node.
        a: u32,
        /// Second node.
        b: u32,
    },
    /// Multiply the node's cost-model service times (1 = normal).
    SlowPeer {
        /// Node index.
        node: u32,
        /// Slowdown factor.
        factor: u8,
    },
    /// Shift the node's wall clock by `offset_ns` (monotonic time unchanged).
    ClockSkew {
        /// Node index.
        node: u32,
        /// Signed wall-clock offset.
        offset_ns: i64,
    },
    /// Run the node's monotonic and wall clocks `ppm` parts per million fast
    /// (positive) or slow (negative) against world time from now on.
    ClockRate {
        /// Node index.
        node: u32,
        /// Rate deviation, `-999_999..=1_000_000`.
        ppm: i32,
    },
    /// Step the node's monotonic and wall clocks forward by `ns`.
    ClockJump {
        /// Node index.
        node: u32,
        /// Forward step.
        ns: u64,
    },
}

impl Fault {
    fn code(self) -> (u64, u64, u64) {
        match self {
            Self::Crash { node } => (1, node.into(), 0),
            Self::Restart { node } => (2, node.into(), 0),
            Self::Partition { a, b } => (3, a.into(), b.into()),
            Self::Heal { a, b } => (4, a.into(), b.into()),
            Self::SlowPeer { node, factor } => (5, node.into(), factor.into()),
            Self::ClockSkew { node, offset_ns } => (6, node.into(), offset_ns as u64),
            Self::ClockRate { node, ppm } => (7, node.into(), i64::from(ppm) as u64),
            Self::ClockJump { node, ns } => (8, node.into(), ns),
        }
    }
}

/// Shared, world-clocked trace digest. Roles fold what they observe into it so
/// a data divergence (not only a schedule divergence) fails the double-run.
#[derive(Clone, Default)]
pub struct Trace {
    hash: Rc<RefCell<TraceHash>>,
    now: Rc<Cell<Nanos>>,
}

impl Trace {
    /// Fold a record at the current world time.
    pub fn record(&self, tag: u64, a: u64, b: u64) {
        self.hash.borrow_mut().record(self.now.get(), tag, a, b);
    }

    /// Fold bytes at the current world time.
    pub fn record_bytes(&self, tag: u64, bytes: &[u8]) {
        self.hash
            .borrow_mut()
            .record_bytes(self.now.get(), tag, bytes);
    }

    /// The digest so far.
    pub fn value(&self) -> u64 {
        self.hash.borrow().value()
    }

    /// Current world time.
    pub fn now(&self) -> Nanos {
        self.now.get()
    }
}

/// Violations reported from inside roles (an `assert` that must not unwind
/// through the executor). The world fails the run after the step that
/// reported one.
#[derive(Clone, Default)]
pub struct Reporter(Rc<RefCell<Vec<String>>>);

impl Reporter {
    /// Report a safety violation.
    pub fn violation(&self, message: impl Into<String>) {
        self.0.borrow_mut().push(message.into());
    }

    fn take(&self) -> Option<String> {
        let mut list = self.0.borrow_mut();
        (!list.is_empty())
            .then(|| list.join("; "))
            .inspect(|_| list.clear())
    }
}

/// Per-run shared state for one world: typed slots that node factories,
/// roles and invariant checks of the same run share (for example observers
/// of an application-level invariant). A fresh world starts with an empty
/// board, so state never leaks between runs of a scenario.
#[derive(Clone, Default)]
pub struct Board(Rc<RefCell<BTreeMap<&'static str, Rc<dyn Any>>>>);

impl Board {
    /// The slot of type `T`, created with `T::default()` on first use.
    pub fn get<T: Default + 'static>(&self) -> Rc<T> {
        let key = std::any::type_name::<T>();
        let any = self
            .0
            .borrow_mut()
            .entry(key)
            .or_insert_with(|| Rc::new(T::default()))
            .clone();
        any.downcast::<T>()
            .unwrap_or_else(|_| unreachable!("board slot {key} has another type"))
    }
}

impl std::fmt::Debug for Board {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_list().entries(self.0.borrow().keys()).finish()
    }
}

/// What a node factory gets to build one node incarnation.
#[derive(Clone)]
pub struct NodeEnv {
    /// Node index.
    pub index: u32,
    /// 0 at first start, +1 per restart.
    pub incarnation: u32,
    /// The node's simulated OS; survives crashes (the disk is crashed, not
    /// replaced). With [`crate::Scenario::shared_network`] every node gets
    /// the same `SimOs`: one network and one disk, and roles label their
    /// sockets with [`SimOs::enter_endpoint`].
    pub os: SimOs,
    /// The node's clock.
    pub clock: VirtualClock,
    /// The world choice stream.
    pub choices: Choices,
    /// The world trace.
    pub trace: Trace,
    /// The world violation sink.
    pub report: Reporter,
    /// The node's slowdown factor ([`Fault::SlowPeer`]); scales its OS
    /// operation costs.
    pub slow: Rc<Cell<u8>>,
    /// The world's per-run shared state.
    pub board: Board,
}

impl NodeEnv {
    /// A simulated worker context: this node's clock and entropy drawn
    /// straight from [`Choices`]. `stream` is kept for call-site stability;
    /// every role shares the one world stream.
    pub fn worker_ctx(&self, stream: u64) -> WorkerCtx {
        let _ = stream;
        WorkerCtx {
            clock: self.clock.clone().into(),
            rng: Rng::from_source(self.choices.clone()),
        }
    }

    /// A reactor on this node's `Driver::Sim` with `entries` operations.
    pub fn reactor(&self, stream: u64, entries: u32) -> racer_io::Result<Reactor> {
        Reactor::new(
            Driver::Sim(self.os.clone()),
            Config::new(entries),
            self.worker_ctx(stream),
        )
    }
}

/// Named roles of one node incarnation.
pub type Roles = Vec<(String, Box<dyn SimRole>)>;

/// Builds one node incarnation. Called at start and at every restart.
pub type NodeFactory = Rc<dyn Fn(&NodeEnv) -> racer_io::Result<Roles>>;

struct Slot {
    name: String,
    role: Box<dyn SimRole>,
    next: Next,
}

struct Node {
    env: NodeEnv,
    roles: Vec<Slot>,
    up: bool,
}

/// Why a run failed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Failure {
    /// Invariant class.
    pub class: Class,
    /// Invariant or check name.
    pub name: String,
    /// World time.
    pub at: Nanos,
    /// Steps taken.
    pub step: u64,
    /// Details.
    pub message: String,
}

impl std::fmt::Display for Failure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "{:?} invariant {:?} failed at {} ns (step {}): {}",
            self.class, self.name, self.at, self.step, self.message
        )
    }
}

/// Result of one run.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Outcome {
    /// Trace digest (schedule, faults and role observations).
    pub trace: u64,
    /// Role steps taken.
    pub steps: u64,
    /// World time at the end.
    pub end: Nanos,
    /// Raw choices drawn.
    pub draws: u64,
    /// Whether the run ended quiescent (nothing runnable or scheduled).
    pub quiescent: bool,
    /// First failure, if any.
    pub failure: Option<Failure>,
}

const TAG_STEP: u64 = 1;
const TAG_FAULT: u64 = 2;
const TAG_END: u64 = 3;

/// A running world.
pub struct World<'s> {
    scenario: &'s Scenario,
    choices: Choices,
    engine: Engine<Fault>,
    nodes: Vec<Node>,
    trace: Trace,
    report: Reporter,
    /// The FIFO cost model (Model fidelity charges it). Full fidelity charges
    /// node OS operations from the same cost table through
    /// `SimOs::set_op_cost` ([`crate::costs::Costs::op`]).
    pub fabric: Fabric,
    steps: u64,
    steps_here: u64,
    board: Board,
    /// Active partitions as `(min, max)` node pairs.
    partitions: BTreeSet<(u32, u32)>,
    last_fault: Option<Nanos>,
}

impl<'s> World<'s> {
    /// Build the world for `scenario` over `choices`, starting every node.
    pub fn new(scenario: &'s Scenario, choices: Choices) -> Result<Self, Failure> {
        let origin = Instant::now();
        let wall = UNIX_EPOCH + Duration::from_secs(1_800_000_000);
        let trace = Trace::default();
        let report = Reporter::default();
        let fabric = Fabric::new(scenario.nodes, scenario.sites, &scenario.costs, &choices);
        let mut engine = Engine::new();
        for (at, fault) in scenario.fault_schedule(&choices) {
            engine.at(at, fault);
        }
        let mut world = Self {
            scenario,
            choices: choices.clone(),
            engine,
            nodes: Vec::new(),
            trace,
            report,
            fabric,
            steps: 0,
            steps_here: 0,
            board: Board::default(),
            partitions: BTreeSet::new(),
            last_fault: None,
        };
        let new_os = || {
            if scenario.seeded_os {
                SimOs::with_choices(choices.clone())
            } else {
                SimOs::new()
            }
        };
        let shared = scenario.network.is_some().then(new_os);
        for index in 0..scenario.nodes {
            let os = shared.clone().unwrap_or_else(new_os);
            let slow = Rc::new(Cell::new(1u8));
            let (costs, factor) = (scenario.costs, slow.clone());
            let by_size = scenario.charge_bytes;
            os.set_op_cost(move |op| {
                Duration::from_nanos(
                    costs
                        .op(op.kind, if by_size { op.bytes } else { 0 })
                        .saturating_mul(u64::from(factor.get())),
                )
            });
            let env = NodeEnv {
                index,
                incarnation: 0,
                os,
                slow,
                clock: VirtualClock::new_at(origin, wall),
                choices: choices.clone(),
                trace: world.trace.clone(),
                report: world.report.clone(),
                board: world.board.clone(),
            };
            world.nodes.push(Node {
                env,
                roles: Vec::new(),
                up: false,
            });
            world.start(index as usize)?;
        }
        Ok(world)
    }

    fn fail(&self, class: Class, name: &str, message: impl Into<String>) -> Failure {
        Failure {
            class,
            name: name.to_owned(),
            at: self.engine.now(),
            step: self.steps,
            message: message.into(),
        }
    }

    fn start(&mut self, node: usize) -> Result<(), Failure> {
        let env = self.nodes[node].env.clone();
        let roles = (self.scenario.factory)(&env).map_err(|e| {
            self.fail(
                Class::Safety,
                "node-start",
                match self.report.take() {
                    Some(why) => {
                        format!("node {node} incarnation {}: {e:?}: {why}", env.incarnation)
                    }
                    None => format!("node {node} incarnation {}: {e:?}", env.incarnation),
                },
            )
        })?;
        let n = &mut self.nodes[node];
        n.roles = roles
            .into_iter()
            .map(|(name, role)| Slot {
                name,
                role,
                next: Next::Ready,
            })
            .collect();
        n.up = true;
        Ok(())
    }

    /// World time at which `node`'s local clock reaches `at`.
    fn nanos(&self, node: usize, at: Instant) -> Nanos {
        self.nodes[node]
            .env
            .clock
            .reference_at(at)
            .map_or(0, |d| d.as_nanos().min(u128::from(Nanos::MAX)) as Nanos)
    }

    fn apply(&mut self, fault: Fault) -> Result<(), Failure> {
        let (kind, a, b) = fault.code();
        self.trace.record(TAG_FAULT, kind, a ^ (b << 32));
        self.last_fault = Some(self.engine.now());
        let valid = |n: u32| (n as usize) < self.nodes.len();
        match fault {
            Fault::Crash { node } if valid(node) => {
                let n = &mut self.nodes[node as usize];
                let was_up = n.up;
                n.roles.clear();
                n.up = false;
                // A shared disk belongs to every node; crashing it would
                // crash them all.
                if was_up && self.scenario.network.is_none() {
                    self.nodes[node as usize]
                        .env
                        .os
                        .disk()
                        .crash()
                        .map_err(|e| self.fail(Class::Safety, "crash", e.to_string()))?;
                }
            }
            Fault::Restart { node } if valid(node) => {
                if !self.nodes[node as usize].up {
                    self.nodes[node as usize].env.incarnation += 1;
                    self.start(node as usize)?;
                }
            }
            Fault::SlowPeer { node, factor } if valid(node) => {
                self.fabric.slow[node as usize] = factor.max(1);
                self.nodes[node as usize].env.slow.set(factor.max(1));
            }
            Fault::ClockSkew { node, offset_ns } if valid(node) => {
                let clock = &self.nodes[node as usize].env.clock;
                let wall = clock.wall_now();
                let shifted = if offset_ns >= 0 {
                    wall.checked_add(Duration::from_nanos(offset_ns as u64))
                } else {
                    wall.checked_sub(Duration::from_nanos(offset_ns.unsigned_abs()))
                };
                shifted
                    .and_then(|w| clock.set_wall_time(w).ok())
                    .ok_or_else(|| self.fail(Class::Safety, "clock-skew", "out of range"))?;
            }
            Fault::ClockRate { node, ppm } if valid(node) => {
                self.nodes[node as usize]
                    .env
                    .clock
                    .set_rate_ppm(i64::from(ppm))
                    .map_err(|e| self.fail(Class::Safety, "clock-rate", format!("{e:?}")))?;
            }
            Fault::ClockJump { node, ns } if valid(node) => {
                self.nodes[node as usize]
                    .env
                    .clock
                    .jump(Duration::from_nanos(ns))
                    .map_err(|e| self.fail(Class::Safety, "clock-jump", format!("{e:?}")))?;
            }
            Fault::Partition { a, b } | Fault::Heal { a, b } if valid(a) && valid(b) => {
                let Some(endpoint) = self.scenario.network.as_ref() else {
                    return Ok(());
                };
                if a == b {
                    return Ok(());
                }
                let (ea, eb) = (endpoint(a), endpoint(b));
                let os = &self.nodes[0].env.os;
                let pair = (a.min(b), a.max(b));
                if matches!(fault, Fault::Partition { .. }) {
                    os.partition(ea, eb);
                    self.partitions.insert(pair);
                } else {
                    os.heal(ea, eb);
                    self.partitions.remove(&pair);
                }
            }
            _ => {
                return Err(self.fail(
                    Class::Safety,
                    "fault",
                    format!("fault names a missing node: {fault:?}"),
                ));
            }
        }
        Ok(())
    }

    fn advance(&mut self, to: Nanos) -> Result<(), Failure> {
        self.engine.advance_to(to);
        self.trace.now.set(self.engine.now());
        let at = Duration::from_nanos(self.engine.now());
        for node in &self.nodes {
            node.env
                .clock
                .advance_reference_to(at)
                .map_err(|e| self.fail(Class::Safety, "clock", format!("{e:?}")))?;
        }
        self.steps_here = 0;
        Ok(())
    }

    /// Snapshot of every role for invariant checks.
    pub fn roles(&self) -> Vec<RoleState> {
        let mut out = Vec::new();
        for node in &self.nodes {
            for slot in &node.roles {
                out.push(RoleState {
                    node: node.env.index,
                    name: slot.name.clone(),
                    next: slot.next,
                    pending: slot.role.pending(),
                });
            }
        }
        out
    }

    fn check(&self, class: Class, quiescent: bool) -> Result<(), Failure> {
        let checks: Vec<_> = self
            .scenario
            .invariants
            .iter()
            .filter(|i| i.class == class)
            .collect();
        if checks.is_empty() {
            return Ok(());
        }
        let roles = self.roles();
        let probe = Probe {
            now: self.engine.now(),
            steps: self.steps,
            quiescent,
            roles: &roles,
            up: self.nodes.iter().map(|n| n.up).collect(),
            board: &self.board,
            partitions: self.partitions.len(),
            last_fault: self.last_fault,
        };
        for inv in checks {
            (inv.check)(&probe).map_err(|m| self.fail(class, &inv.name, m))?;
        }
        Ok(())
    }

    /// Run until quiescence, the scenario end time, or the step budget.
    pub fn run(mut self) -> Outcome {
        let result = self.run_inner();
        let quiescent = matches!(result, Ok(true));
        let failure = result.err();
        self.trace.record(TAG_END, self.steps, self.choices.drawn());
        Outcome {
            trace: self.trace.value(),
            steps: self.steps,
            end: self.engine.now(),
            draws: self.choices.drawn(),
            quiescent,
            failure,
        }
    }

    /// `Ok(true)` when the world went quiescent.
    fn run_inner(&mut self) -> Result<bool, Failure> {
        let end = self.scenario.end;
        // Faults stop at `end`; work already in flight gets `settle` more
        // virtual time to finish before quiescence is required.
        let hard_end = end.saturating_add(self.scenario.settle);
        loop {
            while let Some(fault) = self.engine.pop_until(self.engine.now()) {
                self.apply(fault)?;
            }
            if self.steps >= self.scenario.max_steps {
                if self.scenario.step_budget_fails {
                    return Err(self.fail(
                        Class::Liveness,
                        "step-budget",
                        format!("step budget of {} exhausted", self.scenario.max_steps),
                    ));
                }
                return Ok(false);
            }
            let now = self.engine.now();
            let mut runnable = Vec::new();
            for (n, node) in self.nodes.iter().enumerate() {
                for (r, slot) in node.roles.iter().enumerate() {
                    let due = match slot.next {
                        Next::Ready => true,
                        Next::Done => false,
                        Next::WaitUntil(at) => self.nanos(n, at) <= now,
                        Next::Idle => false,
                    };
                    if due || (slot.next != Next::Done && slot.role.woken()) {
                        runnable.push((n, r));
                    }
                }
            }
            if !runnable.is_empty() {
                let (n, r) = runnable[self.choices.index(runnable.len())];
                let slot = &mut self.nodes[n].roles[r];
                let next = slot.role.step();
                self.steps += 1;
                self.steps_here += 1;
                let next = match next {
                    Ok(next) => next,
                    Err(e) => {
                        let name = slot.name.clone();
                        return Err(self.fail(
                            Class::Safety,
                            "role-error",
                            format!("node {n} role {name}: {e:?}"),
                        ));
                    }
                };
                slot.next = next;
                let code = match next {
                    Next::Ready => 0,
                    Next::Idle => 1,
                    Next::Done => 2,
                    Next::WaitUntil(at) => 3 + self.nanos(n, at),
                };
                self.trace
                    .record(TAG_STEP, ((n as u64) << 32) | r as u64, code);
                if let Some(message) = self.report.take() {
                    return Err(self.fail(Class::Safety, "role-assert", message));
                }
                self.check(Class::Safety, false)?;
                if self.steps_here > self.scenario.busy_limit {
                    return Err(self.fail(
                        Class::Liveness,
                        "busy-loop",
                        format!(
                            "{} steps without time advancing (limit {})",
                            self.steps_here, self.scenario.busy_limit
                        ),
                    ));
                }
                continue;
            }
            let deadline = self
                .nodes
                .iter()
                .enumerate()
                .flat_map(|(n, node)| node.roles.iter().map(move |s| (n, s)))
                .filter_map(|(n, s)| match s.next {
                    Next::WaitUntil(at) => Some(self.nanos(n, at)),
                    _ => None,
                })
                .min();
            let next = match (deadline, self.engine.next_at()) {
                (Some(a), Some(b)) => Some(a.min(b)),
                (a, b) => a.or(b),
            };
            match next {
                Some(at) if at <= hard_end => self.advance(at)?,
                Some(_) => {
                    self.advance(hard_end)?;
                    self.check(Class::Safety, false)?;
                    self.check(Class::Liveness, false)?;
                    let expect = self.scenario.expect_quiescent
                        || self
                            .scenario
                            .invariants
                            .iter()
                            .any(|i| i.class == Class::Resource);
                    if expect {
                        let busy: Vec<String> = self
                            .nodes
                            .iter()
                            .enumerate()
                            .flat_map(|(n, node)| {
                                node.roles.iter().map(move |s| (n, node.env.index, s))
                            })
                            .filter(|(_, _, s)| s.next != Next::Done && s.next != Next::Idle)
                            .map(|(n, index, s)| match s.next {
                                Next::WaitUntil(at) => format!(
                                    "node {index} role {} WaitUntil({} ns)",
                                    s.name,
                                    self.nanos(n, at)
                                ),
                                next => format!("node {index} role {} {next:?}", s.name),
                            })
                            .collect();
                        return Err(self.fail(
                            Class::Liveness,
                            "not-quiescent",
                            format!(
                                "reached end without going quiescent; still scheduled: {}",
                                busy.join(", ")
                            ),
                        ));
                    }
                    return Ok(false);
                }
                None => {
                    let stuck: Vec<String> = self
                        .nodes
                        .iter()
                        .flat_map(|n| n.roles.iter().map(move |s| (n.env.index, s)))
                        .filter(|(_, s)| s.next != Next::Done && s.role.pending())
                        .map(|(node, s)| format!("node {node} role {}", s.name))
                        .collect();
                    if !stuck.is_empty() {
                        return Err(self.fail(
                            Class::Liveness,
                            "pending-without-wake",
                            format!(
                                "pending work with no timer, I/O or wake scheduled: {}",
                                stuck.join(", ")
                            ),
                        ));
                    }
                    self.check(Class::Resource, true)?;
                    self.check(Class::Liveness, true)?;
                    return Ok(true);
                }
            }
        }
    }
}

/// Wall time of a node, for tests of [`Fault::ClockSkew`].
pub fn wall_nanos(clock: &VirtualClock) -> u128 {
    clock
        .wall_now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .map_or(0, |d| d.as_nanos())
}
