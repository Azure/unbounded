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
    error::{Error, Result, pending},
    model::identity::WorkerId,
    topology::rails::RailMapping,
};
use std::{collections::HashSet, num::NonZeroU64};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Role {
    Reactor,
    Crypto,
}
#[derive(Clone, Debug)]
pub struct CpuLocation {
    pub cpu: usize,
    /// Physical core IDs are package-local; SMT siblings share this pair.
    pub package: usize,
    pub core: usize,
    pub numa_node: Option<usize>,
}

/// Tightest applicable effective cgroup CPU-time quota. None means unlimited.
/// Fractional CPU capacity still permits one pair sharing allowed CPUs.
#[derive(Clone, Copy, Debug)]
pub struct CpuQuota {
    pub quota: NonZeroU64,
    pub period: NonZeroU64,
}

/// Discovered local hardware only, not a new controller rail specification.
#[derive(Clone, Debug)]
pub struct NicLocality {
    pub device: String,
    pub numa_node: Option<usize>,
}

pub struct EffectiveTopology {
    /// Online CPUs intersected with process affinity and effective cpuset.
    pub cpus: Vec<CpuLocation>,
    pub quota: Option<CpuQuota>,
    pub nics: Vec<NicLocality>,
}

/// Exactly two roles by construction, even if io.cpu == crypto.cpu. Prefer
/// distinct physical cores on the selected NIC's NUMA node, then allowed local
/// fallback. NIC locality never overrides the deterministic end-to-end rail.
#[derive(Clone, Debug)]
pub struct WorkerPair {
    pub worker: WorkerId,
    pub io: CpuLocation,
    pub crypto: CpuLocation,
    pub nic: Option<NicLocality>,
}

impl WorkerPair {
    pub fn location(&self, role: Role) -> &CpuLocation {
        match role {
            Role::Reactor => &self.io,
            Role::Crypto => &self.crypto,
        }
    }
}

pub struct AffinityPlan {
    /// Unique WorkerIds, complete pairs only. Control/diagnostics run on I/O.
    pub pairs: Vec<WorkerPair>,
}

/// Pure sizing policy, not hardware discovery or a claim that threads have started.
/// Count physical cores rather than SMT siblings; floor odd/fractional capacity
/// to complete pairs, except that any positive CPU budget supports one pair.
pub fn pair_count(max_threads: usize, topology: &EffectiveTopology) -> Result<usize> {
    if max_threads < 2 || topology.cpus.is_empty() {
        return Err(Error::InvalidConfiguration);
    }
    let cores = topology
        .cpus
        .iter()
        .map(|cpu| (cpu.package, cpu.core))
        .collect::<HashSet<_>>()
        .len();
    let quota_cores = topology
        .quota
        .map(|quota| {
            usize::try_from(quota.quota.get() / quota.period.get())
                .unwrap_or(usize::MAX)
                .max(1)
        })
        .unwrap_or(cores);
    // WorkerId is u16; every pair needs its own stable shard ID.
    Ok((max_threads / 2)
        .min((cores.min(quota_cores) / 2).max(1))
        .min(usize::from(u16::MAX) + 1))
}

impl AffinityPlan {
    /// Honor allowed CPUs, quotas, and the total thread cap; always form full pairs.
    pub fn discover(_config: &Config) -> Result<Self> {
        pending("affinity.discover")
    }
    /// Plan from discovered constraints and already accepted rail mappings. Check
    /// NIC/NUMA compatibility locally, without changing membership or page-to-rail
    /// selection. Missing/incompatible hardware keeps HTTP fallback available.
    /// Validate unique workers, allowed CPUs, and pair_count before pinning. No
    /// additional control, telemetry, or helper threads may exceed max_threads.
    pub fn from_topology(
        _config: &Config,
        _topology: EffectiveTopology,
        _rails: &[RailMapping],
    ) -> Result<Self> {
        pending("affinity.plan")
    }
    pub fn pin_current_thread(&self, _worker: WorkerId, _role: Role) -> Result<()> {
        pending("affinity.pin")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::DEFAULT_MAX_THREADS;

    fn topology(cores: usize, quota: Option<(u64, u64)>) -> EffectiveTopology {
        EffectiveTopology {
            cpus: (0..cores)
                .map(|cpu| CpuLocation {
                    cpu,
                    package: 0,
                    core: cpu,
                    numa_node: Some(0),
                })
                .collect(),
            quota: quota.map(|(quota, period)| CpuQuota {
                quota: NonZeroU64::new(quota).unwrap(),
                period: NonZeroU64::new(period).unwrap(),
            }),
            nics: vec![],
        }
    }

    #[test]
    fn sizing_always_budgets_full_pairs_including_single_cpu() {
        for (cap, cores, expected) in [
            (8, 1, 1),
            (8, 2, 1),
            (8, 3, 1),
            (8, 7, 3),
            (7, 8, 3),
            (3, 8, 1),
            (2, 1, 1),
        ] {
            assert_eq!(pair_count(cap, &topology(cores, None)), Ok(expected));
        }
        assert_eq!(pair_count(DEFAULT_MAX_THREADS, &topology(64, None)), Ok(4));
        assert_eq!(
            pair_count(1, &topology(4, None)),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(
            pair_count(8, &topology(0, None)),
            Err(Error::InvalidConfiguration)
        );
    }

    #[test]
    fn effective_quota_and_physical_cores_limit_pairs() {
        for (quota, period, expected) in [(1, 2, 1), (3, 1, 1), (7, 2, 1), (4, 1, 2), (9, 1, 4)] {
            assert_eq!(
                pair_count(8, &topology(16, Some((quota, period)))),
                Ok(expected)
            );
        }
        let mut smt = topology(8, None);
        for cpu in &mut smt.cpus {
            cpu.core /= 2;
        }
        assert_eq!(pair_count(8, &smt), Ok(2));
        // Core zero on different packages is not an SMT sibling.
        for cpu in &mut smt.cpus {
            cpu.package = cpu.cpu;
            cpu.core = 0;
        }
        assert_eq!(pair_count(8, &smt), Ok(4));
    }

    #[test]
    fn single_cpu_pair_retains_two_role_assignments() {
        let cpu = topology(1, None).cpus.remove(0);
        let pair = WorkerPair {
            worker: WorkerId(0),
            io: cpu.clone(),
            crypto: cpu,
            nic: None,
        };
        assert_eq!(pair.location(Role::Reactor).cpu, 0);
        assert_eq!(pair.location(Role::Crypto).cpu, 0);
    }

    // Cover constrained cpusets, one-core progress, and mismatched NIC locality.
}
