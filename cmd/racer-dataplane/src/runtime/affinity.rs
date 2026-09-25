//! CPU/cpuset discovery and NIC-local role assignment before workers start.

use crate::{
    config::Config,
    error::{Result, pending},
    model::identity::WorkerId,
};

#[derive(Clone, Copy, Debug)]
pub enum Role {
    Reactor,
    Crypto,
    Combined,
}
#[derive(Clone, Debug)]
pub struct Assignment {
    pub worker: WorkerId,
    pub cpu: usize,
    pub numa_node: Option<usize>,
    pub role: Role,
}
pub struct AffinityPlan {
    pub assignments: Vec<Assignment>,
}

impl AffinityPlan {
    /// Honor allowed CPUs and the configured thread cap. One core combines roles.
    pub fn discover(_config: &Config) -> Result<Self> {
        pending("affinity.discover")
    }
    pub fn pin_current_thread(&self, _worker: WorkerId) -> Result<()> {
        pending("affinity.pin")
    }
}

#[cfg(test)]
mod tests {
    // Cover constrained cpusets, one-core progress, and mismatched NIC locality.
}
