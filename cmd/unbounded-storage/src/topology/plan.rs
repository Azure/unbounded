// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Placement plan derived from a [`Host`](super::Host) snapshot.
//!
//! [`Plan::for_host`] computes a NUMA-local, disjoint CPU allocation
//! for storage daemon workers. The schedule prioritizes the most
//! constrained roles (NVMe io_uring threads) first, then RDMA progress
//! engines, then RDMA handlers; later demands consume whatever each
//! per-NUMA pool has left. When a node's pool is exhausted the
//! allocator spills into a per-node oversubscription pool rather than
//! silently dropping workers.

use std::collections::{BTreeMap, VecDeque};

use super::{Hca, Host};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Role {
    RdmaProgress { hca: usize },
    RdmaHandler { hca: usize },
    NvmeIoUring { nvme: usize },
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Worker {
    pub cpu: u32,
    pub numa: Option<u16>,
    pub role: Role,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NumaPool {
    pub numa: u16,
    pub workers: usize,
}

#[derive(Clone, Debug)]
pub struct PlanConfig {
    pub rdma_progress_per_hca: usize,
    pub rdma_handlers_per_hca: usize,
    pub nvme_threads_per_drive: usize,
    pub use_smt_siblings: bool,
    pub respect_isolated: bool,
    pub exclude_node_cpu0: bool,
    pub require_node_type_ca: bool,
    pub require_active_port: bool,
    pub tcp_fallback_threads: usize,
}

impl Default for PlanConfig {
    fn default() -> Self {
        Self {
            rdma_progress_per_hca: 1,
            rdma_handlers_per_hca: 4,
            nvme_threads_per_drive: 2,
            use_smt_siblings: false,
            respect_isolated: true,
            exclude_node_cpu0: true,
            require_node_type_ca: true,
            require_active_port: true,
            tcp_fallback_threads: 1,
        }
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Plan {
    pub workers: Vec<Worker>,
    pub numa_pools: Vec<NumaPool>,
}

impl Plan {
    /// Build a placement plan for `host` under `cfg`. The result is
    /// deterministic: HCAs and NVMes are consumed in the sorted order
    /// `Host::discover` produced, and CPUs come out of each per-NUMA
    /// pool in ascending id order.
    pub fn for_host(host: &Host, cfg: &PlanConfig) -> Self {
        let kept_hcas = filter_hcas(&host.hcas, cfg);
        let mut allocator = Allocator::new(host, cfg);

        // Schedule order: NVMe -> RDMA progress -> RDMA handlers.
        // Lower-priority roles only see CPUs left over by higher
        // ones, so the most constrained engines win the disjoint
        // slots first.
        let mut workers = Vec::new();

        for (idx, nvme) in host.nvmes.iter().enumerate() {
            for _ in 0..cfg.nvme_threads_per_drive {
                workers.push(allocator.place(nvme.numa, Role::NvmeIoUring { nvme: idx }));
            }
        }

        for &(idx, hca) in &kept_hcas {
            for _ in 0..cfg.rdma_progress_per_hca {
                workers.push(allocator.place(hca.numa, Role::RdmaProgress { hca: idx }));
            }
        }

        for &(idx, hca) in &kept_hcas {
            for _ in 0..cfg.rdma_handlers_per_hca {
                workers.push(allocator.place(hca.numa, Role::RdmaHandler { hca: idx }));
            }
        }

        if kept_hcas.is_empty() && cfg.tcp_fallback_threads > 0 {
            for _ in 0..cfg.tcp_fallback_threads {
                workers.push(allocator.place(None, Role::RdmaProgress { hca: usize::MAX }));
            }
        }

        let numa_pools = summarize_pools(&workers);
        Plan {
            workers,
            numa_pools,
        }
    }

    pub fn rdma_progress(&self) -> impl Iterator<Item = &Worker> {
        self.workers
            .iter()
            .filter(|w| matches!(w.role, Role::RdmaProgress { .. }))
    }

    pub fn rdma_handlers(&self) -> impl Iterator<Item = &Worker> {
        self.workers
            .iter()
            .filter(|w| matches!(w.role, Role::RdmaHandler { .. }))
    }

    pub fn nvme(&self) -> impl Iterator<Item = &Worker> {
        self.workers
            .iter()
            .filter(|w| matches!(w.role, Role::NvmeIoUring { .. }))
    }
}

/// Owns every per-NUMA primary pool, the cross-NUMA pool used when a
/// device's NUMA is unknown, and the spill pools that kick in once a
/// primary is exhausted. `place` is the only mutating entry point.
struct Allocator<'a> {
    host: &'a Host,
    /// Filtered primary pool for each known NUMA id (front = next
    /// pick).
    primary: BTreeMap<u16, VecDeque<u32>>,
    /// Filtered cross-NUMA pool (union of every node's primary).
    cross_primary: VecDeque<u32>,
    /// Spill pool for each NUMA id, lazily populated from
    /// `host.cpus_on(Some(id))` on first exhaustion.
    spill: BTreeMap<u16, VecDeque<u32>>,
    /// Cross-NUMA spill pool, lazily populated from
    /// `host.cpus_on(None)`.
    cross_spill: VecDeque<u32>,
    /// NUMA ids (including a synthetic sentinel for the `None` case)
    /// that have already emitted an oversubscription warning.
    warned_spill: std::collections::BTreeSet<i32>,
}

impl<'a> Allocator<'a> {
    fn new(host: &'a Host, cfg: &PlanConfig) -> Self {
        let mut primary: BTreeMap<u16, VecDeque<u32>> = BTreeMap::new();
        for node in &host.numa_nodes {
            let pool = build_node_pool(host, node.id, cfg);
            primary.insert(node.id, pool.into_iter().collect());
        }
        let cross_primary: VecDeque<u32> = primary
            .values()
            .flat_map(|v| v.iter().copied())
            .collect::<std::collections::BTreeSet<_>>()
            .into_iter()
            .collect();
        Self {
            host,
            primary,
            cross_primary,
            spill: BTreeMap::new(),
            cross_spill: VecDeque::new(),
            warned_spill: std::collections::BTreeSet::new(),
        }
    }

    fn place(&mut self, target: Option<u16>, role: Role) -> Worker {
        let cpu = match target {
            Some(node) => self.pop_node(node),
            None => self.pop_cross(),
        };
        let numa = target.or_else(|| self.host.cpus.get(&cpu).and_then(|c| c.numa));
        Worker { cpu, numa, role }
    }

    fn pop_node(&mut self, node: u16) -> u32 {
        if let Some(q) = self.primary.get_mut(&node) {
            if let Some(cpu) = q.pop_front() {
                return cpu;
            }
        }
        self.warn_spill(node as i32, format!("node {node}"));
        let spill = self
            .spill
            .entry(node)
            .or_insert_with(|| self.host.cpus_on(Some(node)).into_iter().collect());
        if let Some(cpu) = spill.pop_front() {
            return cpu;
        }
        // Spill exhausted too; replenish from the same source so the
        // allocator never panics on absurd configs. Picks become
        // increasingly degenerate but remain valid CPU ids when the
        // node is populated at all.
        let refill: VecDeque<u32> = self.host.cpus_on(Some(node)).into_iter().collect();
        if !refill.is_empty() {
            *spill = refill;
            return spill.pop_front().unwrap();
        }
        // Last resort: any online CPU. Used only when a NUMA node
        // exists but reports zero CPUs - exotic but possible on
        // memory-only nodes.
        self.any_online_cpu()
    }

    fn pop_cross(&mut self) -> u32 {
        if let Some(cpu) = self.cross_primary.pop_front() {
            return cpu;
        }
        self.warn_spill(-1, "cross-NUMA".to_string());
        if self.cross_spill.is_empty() {
            self.cross_spill = self.host.cpus_on(None).into_iter().collect();
        }
        if let Some(cpu) = self.cross_spill.pop_front() {
            return cpu;
        }
        let refill: VecDeque<u32> = self.host.cpus_on(None).into_iter().collect();
        if !refill.is_empty() {
            self.cross_spill = refill;
            return self.cross_spill.pop_front().unwrap();
        }
        self.any_online_cpu()
    }

    fn warn_spill(&mut self, key: i32, label: String) {
        if self.warned_spill.insert(key) {
            eprintln!(
                "topology: {label} CPU pool exhausted; oversubscribing (workers will share CPUs)"
            );
        }
    }

    fn any_online_cpu(&self) -> u32 {
        // Truly degenerate path: no CPUs anywhere. Tests build hosts
        // with at least one CPU when they exercise allocation, so
        // this only fires under outright misuse. Pick 0 as a stable
        // sentinel rather than panicking.
        self.host.cpus.keys().copied().next().unwrap_or(0)
    }
}

/// Apply HCA filters from `cfg`. Preserves the input order and
/// keeps the original slice index so worker roles still point at
/// the right `host.hcas[i]`.
fn filter_hcas<'a>(hcas: &'a [Hca], cfg: &PlanConfig) -> Vec<(usize, &'a Hca)> {
    hcas.iter()
        .enumerate()
        .filter(|(_, h)| {
            // require_node_type_ca: discovery already records only
            // CA-shaped devices today, so the flag is a no-op now.
            // Kept gated for future hardware that exposes router or
            // switch node types in the same sysfs class.
            let _ = cfg.require_node_type_ca;
            if cfg.require_active_port && !h.ports_active {
                return false;
            }
            true
        })
        .collect()
}

/// Build the filtered per-NUMA primary pool for `node`.
fn build_node_pool(host: &Host, node: u16, cfg: &PlanConfig) -> Vec<u32> {
    let raw = host.cpus_on(Some(node));
    let mut filtered = filter_node_cpus(host, &raw, cfg);
    if filtered.is_empty() && !raw.is_empty() {
        eprintln!(
            "topology: node {node} has no CPUs left after filtering; falling back to unfiltered set"
        );
        filtered = raw;
    }
    filtered
}

fn filter_node_cpus(host: &Host, raw: &[u32], cfg: &PlanConfig) -> Vec<u32> {
    let mut cpus: Vec<u32> = raw.to_vec();

    if cfg.respect_isolated && !host.isolated.is_empty() {
        cpus.retain(|c| host.isolated.contains(c));
    } else if cfg.exclude_node_cpu0 {
        if let Some(min) = cpus.iter().copied().min() {
            cpus.retain(|c| *c != min);
        }
    }

    if !cfg.use_smt_siblings {
        cpus = collapse_smt(host, &cpus);
    }

    cpus
}

/// Drop SMT siblings, keeping only the smallest cpu id of each
/// `smt_siblings` group that intersects `cpus`.
fn collapse_smt(host: &Host, cpus: &[u32]) -> Vec<u32> {
    use std::collections::BTreeSet;
    let pool: BTreeSet<u32> = cpus.iter().copied().collect();
    let mut keep: BTreeSet<u32> = BTreeSet::new();
    let mut seen: BTreeSet<u32> = BTreeSet::new();
    for &cpu in cpus {
        if seen.contains(&cpu) {
            continue;
        }
        let siblings = host
            .cpus
            .get(&cpu)
            .map(|c| c.smt_siblings.clone())
            .unwrap_or_else(|| vec![cpu]);
        // Smallest sibling that is actually in the candidate pool.
        let representative = siblings
            .iter()
            .copied()
            .filter(|s| pool.contains(s))
            .min()
            .unwrap_or(cpu);
        keep.insert(representative);
        for s in siblings {
            seen.insert(s);
        }
    }
    keep.into_iter().collect()
}

fn summarize_pools(workers: &[Worker]) -> Vec<NumaPool> {
    let mut counts: BTreeMap<u16, usize> = BTreeMap::new();
    for w in workers {
        if let Some(n) = w.numa {
            *counts.entry(n).or_insert(0) += 1;
        }
    }
    counts
        .into_iter()
        .map(|(numa, workers)| NumaPool { numa, workers })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::topology::{Cpu, Hca, Host, NumaNode, Nvme};
    use std::collections::{BTreeMap, BTreeSet};

    /// Builds a `Host` snapshot directly without touching sysfs.
    /// `nodes` is `(numa_id, cpu_ids, smt_groups)` where `smt_groups`
    /// is a list of sibling sets that cover the CPUs on that node;
    /// pass an empty Vec for "every CPU is its own group".
    fn fake_host(
        nodes: Vec<(u16, Vec<u32>, Vec<Vec<u32>>)>,
        isolated: Vec<u32>,
        hcas: Vec<Hca>,
        nvmes: Vec<Nvme>,
    ) -> Host {
        let isolated: BTreeSet<u32> = isolated.into_iter().collect();
        let mut cpus: BTreeMap<u32, Cpu> = BTreeMap::new();
        let mut numa_nodes = Vec::new();
        for (numa_id, cpu_ids, smt_groups) in nodes {
            let groups = if smt_groups.is_empty() {
                cpu_ids.iter().map(|c| vec![*c]).collect::<Vec<_>>()
            } else {
                smt_groups
            };
            for cpu in &cpu_ids {
                let siblings = groups
                    .iter()
                    .find(|g| g.contains(cpu))
                    .cloned()
                    .unwrap_or_else(|| vec![*cpu]);
                cpus.insert(
                    *cpu,
                    Cpu {
                        id: *cpu,
                        numa: Some(numa_id),
                        smt_siblings: siblings,
                        isolated: isolated.contains(cpu),
                    },
                );
            }
            numa_nodes.push(NumaNode {
                id: numa_id,
                cpus: cpu_ids,
            });
        }
        numa_nodes.sort_by_key(|n| n.id);
        Host {
            cpus,
            numa_nodes,
            hcas,
            nvmes,
            isolated,
        }
    }

    fn hca(dev: &str, bdf: &str, numa: Option<u16>, active: bool) -> Hca {
        Hca {
            dev_name: dev.to_string(),
            pci_bdf: Some(bdf.to_string()),
            numa,
            ports_active: active,
        }
    }

    fn nvme(dev: &str, bdf: &str, numa: Option<u16>) -> Nvme {
        Nvme {
            dev_name: dev.to_string(),
            pci_bdf: Some(bdf.to_string()),
            numa,
        }
    }

    fn defaults() -> PlanConfig {
        PlanConfig::default()
    }

    #[test]
    fn empty_host_with_tcp_fallback() {
        let host = fake_host(vec![], vec![], vec![], vec![]);
        let mut cfg = defaults();
        cfg.tcp_fallback_threads = 1;
        let plan = Plan::for_host(&host, &cfg);
        assert_eq!(plan.workers.len(), 1);
        assert!(matches!(
            plan.workers[0].role,
            Role::RdmaProgress { hca } if hca == usize::MAX
        ));
    }

    #[test]
    fn empty_host_no_fallback() {
        let host = fake_host(vec![], vec![], vec![], vec![]);
        let mut cfg = defaults();
        cfg.tcp_fallback_threads = 0;
        let plan = Plan::for_host(&host, &cfg);
        assert!(plan.workers.is_empty());
        assert!(plan.numa_pools.is_empty());
    }

    #[test]
    fn single_hca_single_numa_defaults() {
        // 8 CPUs, no SMT; defaults: progress=1, handlers=4, exclude
        // cpu0, isolated empty so cpu0 dropped.
        let host = fake_host(
            vec![(0, (0..8).collect(), vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            vec![],
        );
        let plan = Plan::for_host(&host, &defaults());
        assert_eq!(plan.workers.len(), 5);
        // 1 progress + 4 handlers, all on node 0, all distinct.
        let cpus: BTreeSet<u32> = plan.workers.iter().map(|w| w.cpu).collect();
        assert_eq!(cpus.len(), 5);
        assert!(plan.workers.iter().all(|w| w.numa == Some(0)));
        assert_eq!(plan.rdma_progress().count(), 1);
        assert_eq!(plan.rdma_handlers().count(), 4);
        // cpu0 excluded by exclude_node_cpu0.
        assert!(!cpus.contains(&0));
    }

    #[test]
    fn gb200_shape_disjoint_per_node() {
        // 2 NUMA nodes, 16 cpus each, 2 HCAs per node, no NVMe.
        let node0_cpus: Vec<u32> = (0..16).collect();
        let node1_cpus: Vec<u32> = (16..32).collect();
        let host = fake_host(
            vec![
                (0, node0_cpus.clone(), vec![]),
                (1, node1_cpus.clone(), vec![]),
            ],
            vec![],
            vec![
                hca("mlx5_0", "0000:01:00.0", Some(0), true),
                hca("mlx5_1", "0000:02:00.0", Some(0), true),
                hca("mlx5_2", "0000:81:00.0", Some(1), true),
                hca("mlx5_3", "0000:82:00.0", Some(1), true),
            ],
            vec![],
        );
        let plan = Plan::for_host(&host, &defaults());
        // 4 HCAs * (1 progress + 4 handlers) = 20 workers.
        assert_eq!(plan.workers.len(), 20);
        // Within each node, no two workers share a CPU.
        for node in [0u16, 1] {
            let cpus: Vec<u32> = plan
                .workers
                .iter()
                .filter(|w| w.numa == Some(node))
                .map(|w| w.cpu)
                .collect();
            let unique: BTreeSet<u32> = cpus.iter().copied().collect();
            assert_eq!(
                cpus.len(),
                unique.len(),
                "node {node} workers share CPUs: {cpus:?}"
            );
            assert_eq!(cpus.len(), 10);
        }
    }

    #[test]
    fn epyc_8ccd_64_cpus_8_nvme_1_hca() {
        // Single NUMA node, 64 cpus, 8 NVMe, 1 HCA. Defaults give:
        // 8 * 2 = 16 NVMe + 1 progress + 4 handlers = 21. Available
        // pool after dropping cpu0 = 63 cpus, easily fits.
        let cpus: Vec<u32> = (0..64).collect();
        let mut nvmes = Vec::new();
        for i in 0..8 {
            nvmes.push(nvme(
                &format!("nvme{i}"),
                &format!("0000:c{i}:00.0"),
                Some(0),
            ));
        }
        let host = fake_host(
            vec![(0, cpus, vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            nvmes,
        );
        let plan = Plan::for_host(&host, &defaults());
        assert_eq!(plan.workers.len(), 21);
        let unique: BTreeSet<u32> = plan.workers.iter().map(|w| w.cpu).collect();
        assert_eq!(unique.len(), 21, "all workers should land on disjoint CPUs");
        // NVMe gets the lowest cpu ids (after cpu0 exclusion).
        let nvme_cpus: BTreeSet<u32> = plan.nvme().map(|w| w.cpu).collect();
        let progress_min = plan.rdma_progress().map(|w| w.cpu).min().unwrap();
        // Every NVMe CPU should be smaller than the smallest progress
        // CPU because demand schedule is NVMe -> progress.
        assert!(
            nvme_cpus.iter().all(|c| *c < progress_min),
            "nvme cpus {nvme_cpus:?} should precede progress cpu {progress_min}"
        );
    }

    #[test]
    fn isolcpus_restricts_pool() {
        let host = fake_host(
            vec![(0, (0..8).collect(), vec![])],
            vec![2, 3, 6, 7],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            vec![],
        );
        let mut cfg = defaults();
        cfg.respect_isolated = true;
        cfg.rdma_handlers_per_hca = 3;
        let plan = Plan::for_host(&host, &cfg);
        let allowed: BTreeSet<u32> = [2, 3, 6, 7].iter().copied().collect();
        for w in &plan.workers {
            assert!(
                allowed.contains(&w.cpu),
                "worker on cpu {} outside isolated set",
                w.cpu
            );
        }
        // 1 progress + 3 handlers = 4, fills the 4 isolated cpus.
        assert_eq!(plan.workers.len(), 4);
    }

    #[test]
    fn exclude_node_cpu0_drops_zero() {
        let host = fake_host(
            vec![(0, (0..8).collect(), vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            vec![],
        );
        let mut cfg = defaults();
        cfg.exclude_node_cpu0 = true;
        cfg.respect_isolated = true; // isolated empty so this branch
        // is bypassed and exclude path
        // applies.
        let plan = Plan::for_host(&host, &cfg);
        for w in &plan.workers {
            assert_ne!(w.cpu, 0, "cpu0 should be excluded");
        }
    }

    #[test]
    fn smt_collapse_drops_siblings() {
        // Pairs (0,8), (1,9), ..., (7,15). With use_smt_siblings=false
        // only the lower half (0..8) survives. With cpu0 excluded
        // additionally (defaults), pool becomes {1..8}.
        let cpus: Vec<u32> = (0..16).collect();
        let smt = (0..8).map(|c| vec![c, c + 8]).collect();
        let host = fake_host(
            vec![(0, cpus, smt)],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            vec![],
        );
        let mut cfg = defaults();
        cfg.use_smt_siblings = false;
        cfg.rdma_handlers_per_hca = 4;
        let plan = Plan::for_host(&host, &cfg);
        for w in &plan.workers {
            assert!(
                w.cpu < 8,
                "cpu {} is an SMT sibling that should have been dropped",
                w.cpu
            );
        }
    }

    #[test]
    fn oversubscription_when_pool_too_small() {
        // Only cpus 0,1 on node 0. exclude_node_cpu0 drops cpu0,
        // leaving just cpu1. Demand: 1 progress + 4 handlers = 5.
        // SMT collapse on a no-sibling host is a no-op.
        let host = fake_host(
            vec![(0, vec![0, 1], vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            vec![],
        );
        let mut cfg = defaults();
        cfg.rdma_progress_per_hca = 1;
        cfg.rdma_handlers_per_hca = 4;
        let plan = Plan::for_host(&host, &cfg);
        assert_eq!(plan.workers.len(), 5);
        let online: BTreeSet<u32> = host.cpus.keys().copied().collect();
        for w in &plan.workers {
            assert!(online.contains(&w.cpu));
        }
        // Reuse must happen because the pool is too small.
        let unique: BTreeSet<u32> = plan.workers.iter().map(|w| w.cpu).collect();
        assert!(unique.len() < plan.workers.len());
    }

    #[test]
    fn hca_order_follows_pci_bdf() {
        // Caller passes hcas already sorted by (bdf, dev) per
        // `Host::discover` contract; assert the plan respects it.
        let host = fake_host(
            vec![(0, (0..16).collect(), vec![])],
            vec![],
            vec![
                hca("mlx5_99", "0000:01:00.0", Some(0), true),
                hca("mlx5_2", "0000:50:00.0", Some(0), true),
                hca("mlx5_5", "0000:c0:00.0", Some(0), true),
            ],
            vec![],
        );
        let mut cfg = defaults();
        cfg.rdma_progress_per_hca = 1;
        cfg.rdma_handlers_per_hca = 0;
        let plan = Plan::for_host(&host, &cfg);
        let progress: Vec<usize> = plan
            .rdma_progress()
            .map(|w| match w.role {
                Role::RdmaProgress { hca } => hca,
                _ => unreachable!(),
            })
            .collect();
        assert_eq!(progress, vec![0, 1, 2]);
    }

    #[test]
    fn inactive_hca_dropped_but_others_survive() {
        let host = fake_host(
            vec![(0, (0..16).collect(), vec![])],
            vec![],
            vec![
                hca("mlx5_0", "0000:01:00.0", Some(0), false), // inactive
                hca("mlx5_1", "0000:02:00.0", Some(0), true),
            ],
            vec![],
        );
        let mut cfg = defaults();
        cfg.require_active_port = true;
        cfg.rdma_handlers_per_hca = 1;
        let plan = Plan::for_host(&host, &cfg);
        // Only the active HCA contributes: 1 progress + 1 handler.
        assert_eq!(plan.workers.len(), 2);
        for w in &plan.workers {
            match w.role {
                Role::RdmaProgress { hca } | Role::RdmaHandler { hca } => assert_eq!(hca, 1),
                _ => panic!("unexpected nvme role"),
            }
        }
        // No TCP fallback because hca 1 survived.
        assert!(
            plan.workers
                .iter()
                .all(|w| !matches!(w.role, Role::RdmaProgress { hca } if hca == usize::MAX))
        );
    }

    #[test]
    fn all_hcas_filtered_triggers_tcp_fallback() {
        let host = fake_host(
            vec![(0, (0..8).collect(), vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), false)],
            vec![],
        );
        let mut cfg = defaults();
        cfg.require_active_port = true;
        cfg.tcp_fallback_threads = 2;
        let plan = Plan::for_host(&host, &cfg);
        assert_eq!(plan.workers.len(), 2);
        for w in &plan.workers {
            assert!(matches!(w.role, Role::RdmaProgress { hca } if hca == usize::MAX));
        }
    }

    #[test]
    fn hca_with_unknown_numa_uses_cross_pool() {
        // Two nodes, one HCA without numa info. Cross pool is the
        // union of per-node pools so the worker's numa field reflects
        // whichever node the picked cpu actually belongs to.
        let node0: Vec<u32> = (0..8).collect();
        let node1: Vec<u32> = (8..16).collect();
        let host = fake_host(
            vec![(0, node0.clone(), vec![]), (1, node1.clone(), vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", None, true)],
            vec![],
        );
        let mut cfg = defaults();
        cfg.rdma_handlers_per_hca = 1;
        let plan = Plan::for_host(&host, &cfg);
        assert_eq!(plan.workers.len(), 2);
        for w in &plan.workers {
            assert!(
                w.numa == Some(0) || w.numa == Some(1),
                "worker numa should be derived from the cpu's actual node"
            );
            // The cpu must come from one of the two node sets.
            assert!(node0.contains(&w.cpu) || node1.contains(&w.cpu));
        }
    }

    #[test]
    fn numa_pools_count_per_node_workers() {
        // Reuse the GB200 shape: 10 workers per node.
        let host = fake_host(
            vec![
                (0, (0..16).collect(), vec![]),
                (1, (16..32).collect(), vec![]),
            ],
            vec![],
            vec![
                hca("mlx5_0", "0000:01:00.0", Some(0), true),
                hca("mlx5_1", "0000:02:00.0", Some(0), true),
                hca("mlx5_2", "0000:81:00.0", Some(1), true),
                hca("mlx5_3", "0000:82:00.0", Some(1), true),
            ],
            vec![],
        );
        let plan = Plan::for_host(&host, &defaults());
        assert_eq!(
            plan.numa_pools,
            vec![
                NumaPool {
                    numa: 0,
                    workers: 10,
                },
                NumaPool {
                    numa: 1,
                    workers: 10,
                },
            ]
        );
    }
}
