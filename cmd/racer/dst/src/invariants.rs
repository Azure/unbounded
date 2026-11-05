//! Invariant registration.
//!
//! Three classes, checked at different points by [`crate::World`]:
//!
//! - [`Class::Safety`]: after every role step (and roles report violations
//!   observed inside their own code through [`crate::world::Reporter`]).
//! - [`Class::Resource`]: when the world is quiescent (every charge, slot and
//!   flight released; filled in by Phase 3).
//! - [`Class::Liveness`]: at the end of the run. The built-in liveness check,
//!   "pending work with no wake scheduled", runs whenever the world runs out of
//!   timers, events and ready roles; a second built-in, "busy-loop", bounds the
//!   steps taken without virtual time advancing.
//!
//! Checks see a [`Probe`]: world time, step count and per-role scheduler state.
//! Application-level observations (bytes delivered, keys installed) arrive in
//! later phases as typed observers on the probe; until then roles fold them
//! into the trace and report violations through the reporter.
use std::rc::Rc;

use racer_io::exec::Next;

use crate::{engine::Nanos, world::Board};

/// When an invariant is checked.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Class {
    /// After every step.
    Safety,
    /// At quiescence.
    Resource,
    /// At the end of the run.
    Liveness,
}

/// Scheduler state of one role.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RoleState {
    /// Node index.
    pub node: u32,
    /// Role name.
    pub name: String,
    /// What its last step returned.
    pub next: Next,
    /// Whether it holds unfinished work.
    pub pending: bool,
}

/// What an invariant check sees.
#[derive(Debug)]
pub struct Probe<'a> {
    /// World time.
    pub now: Nanos,
    /// Steps taken.
    pub steps: u64,
    /// Whether the world is quiescent.
    pub quiescent: bool,
    /// Every role of every running node.
    pub roles: &'a [RoleState],
    /// Whether each node is up (not crashed).
    pub up: Vec<bool>,
    /// The world's per-run shared state (application observers).
    pub board: &'a Board,
    /// Partitions in effect.
    pub partitions: usize,
    /// World time of the last fault applied, if any.
    pub last_fault: Option<Nanos>,
}

/// A check: `Err(message)` fails the run.
pub type Check = Rc<dyn Fn(&Probe<'_>) -> Result<(), String>>;

/// A registered invariant.
#[derive(Clone)]
pub struct Invariant {
    /// Name in failure reports.
    pub name: String,
    /// When it runs.
    pub class: Class,
    /// The check.
    pub check: Check,
}

impl std::fmt::Debug for Invariant {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Invariant")
            .field("name", &self.name)
            .field("class", &self.class)
            .finish()
    }
}

/// Built-in resource invariant: at quiescence every role on an up node has
/// finished (`Done`) or is idle without pending work.
pub fn all_roles_settled() -> Invariant {
    Invariant {
        name: "all-roles-settled".into(),
        class: Class::Resource,
        check: Rc::new(|probe| {
            let open: Vec<String> = probe
                .roles
                .iter()
                .filter(|r| r.pending && r.next != Next::Done)
                .map(|r| format!("node {} role {}", r.node, r.name))
                .collect();
            if open.is_empty() {
                Ok(())
            } else {
                Err(format!("unsettled at quiescence: {}", open.join(", ")))
            }
        }),
    }
}
