//! CPU/cpuset discovery and NIC-local role assignment before workers start.
//!
//! Each logical worker has exactly two threads: one reactor and one crypto thread.
//! Prefer separate physical cores in the same NIC-local NUMA node. On a one-CPU
//! cpuset, pin both threads to that CPU and use bounded work with wakeable waits.
//! Size complete pairs using allowed CPUs, applicable cgroup quotas, and the total
//! thread cap (eight by default). An odd spare CPU does not create a partial pair.
//! Quotas limit execution time, not CPU affinity. Keep rail selection deterministic
//! across nodes; local pair placement must not change the page's selected rail.

use crate::{
    config::Config,
    error::{Result, pending},
    model::identity::WorkerId,
};

#[derive(Clone, Copy, Debug)]
pub enum Role {
    Reactor,
    Crypto,
}
#[derive(Clone, Debug)]
pub struct Assignment {
    pub worker: WorkerId,
    pub cpu: usize,
    pub numa_node: Option<usize>,
    pub role: Role,
}
pub struct AffinityPlan {
    /// Exactly one Reactor and one Crypto assignment per logical WorkerId.
    pub assignments: Vec<Assignment>,
}

impl AffinityPlan {
    /// Honor allowed CPUs, quotas, and the total thread cap; always form full pairs.
    pub fn discover(_config: &Config) -> Result<Self> {
        pending("affinity.discover")
    }
    pub fn pin_current_thread(&self, _worker: WorkerId, _role: Role) -> Result<()> {
        pending("affinity.pin")
    }
}

#[cfg(test)]
mod tests {
    // Cover constrained cpusets, one-core progress, and mismatched NIC locality.
}
