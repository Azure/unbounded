// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Three-class core plan derived from a [`Host`](super::Host) snapshot.
//!
//! [`CorePlan::for_host`] decouples serving capacity from the HCA count
//! by partitioning the host's usable CPUs into three pinned classes,
//! scheduled in order of how constrained they are:
//!
//! 1. **Storage cores** (most constrained): exactly one CPU per NVMe
//!    drive, preferring a CPU local to the drive's `(numa, pcie_root)`.
//! 2. **NIC workers**: `nic_workers` CPUs per active HCA, preferring
//!    CPUs that share the HCA's `(numa, pcie_root)`, round-robin within
//!    the local pool.
//! 3. **Serving shards**: every remaining usable CPU, spread across the
//!    NUMA nodes. An optional `serving_cores` cap bounds the count;
//!    the default claims all of them.
//!
//! All three classes are drawn from a single per-NUMA pool that already
//! honors SMT collapse, isolcpus, and core-0 exclusion (the shared
//! filtering engine in [`filters`](super::filters)). Because every CPU is
//! handed out at most once, the three classes are mutually disjoint by
//! construction. When a pool is exhausted the allocator oversubscribes
//! rather than panicking, mirroring the legacy planner's contract.
//!
//! Note on `pcie_root`: Linux exposes CPU affinity at NUMA-node
//! granularity, not per PCIe complex, so "prefer the device's
//! `(numa, pcie_root)`" reduces to "prefer the device's NUMA node" when
//! selecting CPUs. `pcie_root` is retained on the discovered devices for
//! NUMA-local fetch routing (NIC <-> NVMe pairing) in a later phase; it
//! does not subdivide the CPU pool here.

use std::collections::{BTreeMap, VecDeque};

use super::Host;
use super::filters::{self, Filters};

/// A CPU dedicated to one NVMe drive's storage engine and io_uring
/// ring. `drive` indexes into `host.nvmes`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct StorageCore {
    pub cpu: u32,
    pub numa: Option<u16>,
    pub drive: usize,
}

/// A single CPU dedicated to driving one HCA's fabric (libfabric
/// progress + RPC handling).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct NicWorker {
    pub cpu: u32,
    pub numa: Option<u16>,
}

/// The NIC workers bound to one active HCA. `hca` indexes into
/// `host.hcas`; `numa` is the HCA's NUMA node (the preferred locality
/// for its workers).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NicWorkerGroup {
    pub hca: usize,
    pub numa: Option<u16>,
    pub workers: Vec<NicWorker>,
}

/// A CPU dedicated to a serving shard (HTTP frontend + pool + socket
/// ring). `numa` is the node the CPU was drawn from.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ServingShard {
    pub cpu: u32,
    pub numa: Option<u16>,
}

/// Per-NUMA count of pinned cores, summarizing a [`CorePlan`] by node.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NumaPool {
    pub numa: u16,
    pub workers: usize,
}

/// A CPU slot reserved for a disk storage core: one per disk path (per
/// NVMe drive), drawn from the plan's [`StorageCore`] allocation. These
/// slots come out of the same disjoint, NUMA-local, SMT-collapsed,
/// cpu0-excluded allocator that places the serving shards and NIC
/// workers, so a disk pinned to one of these CPUs never collides with a
/// serving or fabric core.
///
/// This is topology's own small slot type rather than a
/// `runtime::WorkerSpec`: the dependency direction is
/// `runtime -> topology`, so topology must not name runtime types.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct DiskCpuSlot {
    pub cpu: u32,
    pub numa: Option<u16>,
}

/// The three-class placement plan: typed CPU lists instead of a
/// role-tagged worker soup. Every CPU appears in at most one class.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct CorePlan {
    pub storage_cores: Vec<StorageCore>,
    pub nic_workers: Vec<NicWorkerGroup>,
    pub serving_shards: Vec<ServingShard>,
    /// Per-NUMA count of all pinned cores (storage + NIC + serving).
    pub numa_pools: Vec<NumaPool>,
}

/// Knobs for the three-class allocator. The filtering flags feed the
/// shared [`Filters`](super::filters) engine; the capacity knobs
/// (`nic_workers`, `serving_cores`) size the NIC-worker and serving
/// classes.
#[derive(Clone, Copy, Debug)]
pub struct CorePlanConfig {
    /// CPUs to pin per active HCA for fabric work.
    pub nic_workers: usize,
    /// Optional cap on serving shards. `None` claims every remaining
    /// usable CPU (the default).
    pub serving_cores: Option<usize>,
    /// Maximum number of HCAs to use per NUMA node. Defaults to 1.
    /// Raise to use more. HCAs with an unknown NUMA node are never capped.
    pub hcas_per_numa: usize,
    pub use_smt_siblings: bool,
    pub respect_isolated: bool,
    pub exclude_node_cpu0: bool,
    pub require_node_type_ca: bool,
    pub require_active_port: bool,
    pub disable_rdma: bool,
}

impl Default for CorePlanConfig {
    fn default() -> Self {
        Self {
            nic_workers: 4,
            serving_cores: None,
            hcas_per_numa: 1,
            use_smt_siblings: false,
            respect_isolated: true,
            exclude_node_cpu0: true,
            require_node_type_ca: true,
            require_active_port: true,
            disable_rdma: false,
        }
    }
}

impl CorePlanConfig {
    fn filters(&self) -> Filters {
        Filters {
            use_smt_siblings: self.use_smt_siblings,
            respect_isolated: self.respect_isolated,
            exclude_node_cpu0: self.exclude_node_cpu0,
            require_node_type_ca: self.require_node_type_ca,
            require_active_port: self.require_active_port,
            disable_rdma: self.disable_rdma,
            hcas_per_numa: self.hcas_per_numa,
        }
    }
}

impl CorePlan {
    /// Build a three-class core plan for `host` under `cfg`. The result
    /// is deterministic: NVMes and HCAs are consumed in the sorted order
    /// `Host::discover` produced, and CPUs come out of each per-NUMA
    /// pool in ascending id order.
    pub fn for_host(host: &Host, cfg: &CorePlanConfig) -> Self {
        let filters = cfg.filters();
        let kept_hcas = filters::filter_hcas(&host.hcas, &filters);
        let mut alloc = CoreAllocator::new(host, &filters);

        // 1. Storage cores first (most constrained): one CPU per drive,
        //    local to the drive's NUMA node when possible.
        let mut storage_cores = Vec::with_capacity(host.nvmes.len());
        for (drive, nvme) in host.nvmes.iter().enumerate() {
            let (cpu, numa) = alloc.take(nvme.numa);
            storage_cores.push(StorageCore { cpu, numa, drive });
        }

        // 2. NIC workers: nic_workers CPUs per active HCA, local to the
        //    HCA's NUMA node, round-robin within that local pool.
        let mut nic_workers = Vec::with_capacity(kept_hcas.len());
        for &(hca, dev) in &kept_hcas {
            let mut workers = Vec::with_capacity(cfg.nic_workers);
            for _ in 0..cfg.nic_workers {
                let (cpu, numa) = alloc.take(dev.numa);
                workers.push(NicWorker { cpu, numa });
            }
            nic_workers.push(NicWorkerGroup {
                hca,
                numa: dev.numa,
                workers,
            });
        }

        // 3. Serving shards: everything left, spread across NUMA nodes,
        //    optionally capped by serving_cores.
        let serving_shards = fill_serving(alloc.drain_remaining(), cfg.serving_cores);

        let numa_pools = summarize(&storage_cores, &nic_workers, &serving_shards);
        CorePlan {
            storage_cores,
            nic_workers,
            serving_shards,
            numa_pools,
        }
    }

    /// Total CPUs pinned across all three classes.
    pub fn total_cores(&self) -> usize {
        self.storage_cores.len()
            + self
                .nic_workers
                .iter()
                .map(|g| g.workers.len())
                .sum::<usize>()
            + self.serving_shards.len()
    }
}

/// Owns one filtered CPU pool per known NUMA node, the single source of
/// truth from which all three classes are drawn. `take` pops a CPU
/// (preferring a target node), and `drain_remaining` hands back whatever
/// is left for the serving shards. CPUs are never handed out twice, so
/// the classes stay disjoint; an exhausted host oversubscribes from the
/// unfiltered CPU set rather than panicking.
struct CoreAllocator<'a> {
    host: &'a Host,
    pools: BTreeMap<u16, VecDeque<u32>>,
    node_order: Vec<u16>,
    warned: bool,
}

impl<'a> CoreAllocator<'a> {
    fn new(host: &'a Host, filters: &Filters) -> Self {
        let mut pools: BTreeMap<u16, VecDeque<u32>> = BTreeMap::new();
        let mut node_order = Vec::with_capacity(host.numa_nodes.len());
        for node in &host.numa_nodes {
            let pool = filters::build_node_pool(host, node.id, filters);
            pools.insert(node.id, pool.into_iter().collect());
            node_order.push(node.id);
        }
        Self {
            host,
            pools,
            node_order,
            warned: false,
        }
    }

    /// Take one CPU, preferring `numa`. Falls back to any node with
    /// capacity, then oversubscribes. Never panics.
    fn take(&mut self, numa: Option<u16>) -> (u32, Option<u16>) {
        if let Some(n) = numa {
            if let Some(cpu) = self.pop_from(n) {
                return (cpu, Some(n));
            }
        }
        // Either the target node is unknown/empty: take from the first
        // node (ascending) that still has capacity.
        for n in self.node_order.clone() {
            if let Some(cpu) = self.pop_from(n) {
                return (cpu, Some(n));
            }
        }
        self.oversubscribe(numa)
    }

    fn pop_from(&mut self, node: u16) -> Option<u32> {
        self.pools.get_mut(&node).and_then(|q| q.pop_front())
    }

    /// Every pool is empty. Warn once, then re-seed the preferred node
    /// (or the first known node) from its unfiltered CPU set so picks
    /// cycle through real CPU ids instead of panicking.
    fn oversubscribe(&mut self, numa: Option<u16>) -> (u32, Option<u16>) {
        if !self.warned {
            self.warned = true;
            eprintln!("topology: CPU pool exhausted; oversubscribing (cores will share CPUs)");
        }
        let node = numa.or_else(|| self.node_order.first().copied());
        if let Some(n) = node {
            let mut refill: VecDeque<u32> = self.host.cpus_on(Some(n)).into_iter().collect();
            if let Some(cpu) = refill.pop_front() {
                self.pools.insert(n, refill);
                return (cpu, Some(n));
            }
        }
        // Truly degenerate: the node reports zero CPUs. Pick any online
        // CPU as a stable sentinel.
        let cpu = self.host.cpus.keys().copied().next().unwrap_or(0);
        (cpu, numa)
    }

    /// Hand back every CPU still unclaimed, grouped by node in ascending
    /// order (CPUs within a node stay ascending).
    fn drain_remaining(&mut self) -> Vec<(u16, Vec<u32>)> {
        self.node_order
            .iter()
            .map(|&n| {
                let cpus = self
                    .pools
                    .get_mut(&n)
                    .map(|q| q.drain(..).collect())
                    .unwrap_or_default();
                (n, cpus)
            })
            .collect()
    }
}

/// Turn the per-node leftovers into serving shards, round-robin across
/// nodes so a `serving_cores` cap stays balanced. `None` claims all.
fn fill_serving(remaining: Vec<(u16, Vec<u32>)>, cap: Option<usize>) -> Vec<ServingShard> {
    let cap = cap.unwrap_or(usize::MAX);
    let mut queues: Vec<(u16, VecDeque<u32>)> = remaining
        .into_iter()
        .map(|(n, cpus)| (n, cpus.into_iter().collect()))
        .collect();
    let mut out = Vec::new();
    let mut progress = true;
    while out.len() < cap && progress {
        progress = false;
        for (node, q) in queues.iter_mut() {
            if out.len() >= cap {
                break;
            }
            if let Some(cpu) = q.pop_front() {
                out.push(ServingShard {
                    cpu,
                    numa: Some(*node),
                });
                progress = true;
            }
        }
    }
    out
}

fn summarize(
    storage: &[StorageCore],
    nic: &[NicWorkerGroup],
    serving: &[ServingShard],
) -> Vec<NumaPool> {
    let mut counts: BTreeMap<u16, usize> = BTreeMap::new();
    for s in storage {
        if let Some(n) = s.numa {
            *counts.entry(n).or_insert(0) += 1;
        }
    }
    for g in nic {
        for w in &g.workers {
            if let Some(n) = w.numa {
                *counts.entry(n).or_insert(0) += 1;
            }
        }
    }
    for s in serving {
        if let Some(n) = s.numa {
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
    use crate::topology::testutil::{fake_host, hca, nvme};
    use std::collections::BTreeSet;

    fn defaults() -> CorePlanConfig {
        CorePlanConfig::default()
    }

    /// Collect every CPU across all three classes.
    fn all_cpus(plan: &CorePlan) -> Vec<u32> {
        let mut v: Vec<u32> = plan.storage_cores.iter().map(|s| s.cpu).collect();
        v.extend(
            plan.nic_workers
                .iter()
                .flat_map(|g| g.workers.iter().map(|w| w.cpu)),
        );
        v.extend(plan.serving_shards.iter().map(|s| s.cpu));
        v
    }

    fn set(cpus: impl IntoIterator<Item = u32>) -> BTreeSet<u32> {
        cpus.into_iter().collect()
    }

    #[test]
    fn default_nic_workers_is_four() {
        assert_eq!(CorePlanConfig::default().nic_workers, 4);
        assert_eq!(CorePlanConfig::default().serving_cores, None);
    }

    #[test]
    fn single_hca_single_numa_fills_serving() {
        // 8 cpus node 0, no NVMe, 1 HCA, nic_workers=4. cpu0 excluded
        // leaves {1..=7} = 7 usable. NIC takes 4, serving claims 3.
        let host = fake_host(
            vec![(0, (0..8).collect(), vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            vec![],
        );
        let plan = CorePlan::for_host(&host, &defaults());

        assert!(plan.storage_cores.is_empty());
        assert_eq!(plan.nic_workers.len(), 1);
        assert_eq!(plan.nic_workers[0].hca, 0);
        assert_eq!(plan.nic_workers[0].numa, Some(0));
        assert_eq!(plan.nic_workers[0].workers.len(), 4);
        assert!(
            plan.nic_workers[0]
                .workers
                .iter()
                .all(|w| w.numa == Some(0))
        );
        assert_eq!(plan.serving_shards.len(), 3);
        assert!(plan.serving_shards.iter().all(|s| s.numa == Some(0)));

        let cpus = all_cpus(&plan);
        assert_eq!(cpus.len(), 7, "all usable cpus claimed");
        assert_eq!(set(cpus.clone()).len(), 7, "classes are disjoint");
        assert!(!set(cpus).contains(&0), "cpu0 excluded");
    }

    #[test]
    fn gb200_shape_two_nodes_two_hcas_each() {
        // 2 NUMA nodes, 16 cpus each, 2 active HCAs per node, no NVMe.
        // hcas_per_numa=2 opts into using both HCAs on each node (the
        // default of 1 is covered by the test below).
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
        let cfg = CorePlanConfig {
            hcas_per_numa: 2,
            ..defaults()
        };
        let plan = CorePlan::for_host(&host, &cfg);

        // 4 groups of 4 nic workers, each local to its HCA's node.
        assert_eq!(plan.nic_workers.len(), 4);
        for g in &plan.nic_workers {
            assert_eq!(g.workers.len(), 4);
            assert!(g.workers.iter().all(|w| w.numa == g.numa));
        }
        // Each node: 16 cpus, cpu0/cpu16 excluded -> 15 usable; 8 go to
        // nic workers (2 HCAs * 4), 7 remain for serving.
        for node in [0u16, 1] {
            let nic = plan
                .nic_workers
                .iter()
                .filter(|g| g.numa == Some(node))
                .flat_map(|g| g.workers.iter().map(|w| w.cpu))
                .count();
            let serving = plan
                .serving_shards
                .iter()
                .filter(|s| s.numa == Some(node))
                .count();
            assert_eq!(nic, 8, "node {node} nic workers");
            assert_eq!(serving, 7, "node {node} serving shards");
        }
        // Everything disjoint.
        let cpus = all_cpus(&plan);
        assert_eq!(set(cpus.clone()).len(), cpus.len());
        assert_eq!(cpus.len(), 30);
    }

    #[test]
    fn gb200_shape_default_caps_one_hca_per_node() {
        // Same shape as above, but with the default hcas_per_numa=1 the
        // planner keeps only the lowest-BDF HCA on each node: 2 nic
        // groups total, and the freed CPUs become serving shards.
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
        let plan = CorePlan::for_host(&host, &defaults());

        // One HCA per node survives: mlx5_0 (node 0), mlx5_2 (node 1).
        assert_eq!(plan.nic_workers.len(), 2);
        let kept: Vec<usize> = plan.nic_workers.iter().map(|g| g.hca).collect();
        assert_eq!(kept, vec![0, 2]);
        for node in [0u16, 1] {
            let nic = plan
                .nic_workers
                .iter()
                .filter(|g| g.numa == Some(node))
                .flat_map(|g| g.workers.iter().map(|w| w.cpu))
                .count();
            let serving = plan
                .serving_shards
                .iter()
                .filter(|s| s.numa == Some(node))
                .count();
            // 15 usable per node; 4 nic workers (1 HCA), 11 serving.
            assert_eq!(nic, 4, "node {node} nic workers");
            assert_eq!(serving, 11, "node {node} serving shards");
        }
        let cpus = all_cpus(&plan);
        assert_eq!(set(cpus.clone()).len(), cpus.len());
        assert_eq!(cpus.len(), 30);
    }

    #[test]
    fn epyc_64_cpus_8_nvme_1_hca() {
        // Single node, 64 cpus, 8 NVMe, 1 HCA, nic_workers=4. cpu0
        // excluded -> 63 usable. Storage takes 8, nic takes 4, serving
        // claims the rest (51).
        let mut nvmes = Vec::new();
        for i in 0..8 {
            nvmes.push(nvme(
                &format!("nvme{i}"),
                &format!("0000:c{i}:00.0"),
                Some(0),
            ));
        }
        let host = fake_host(
            vec![(0, (0..64).collect(), vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            nvmes,
        );
        let plan = CorePlan::for_host(&host, &defaults());

        assert_eq!(plan.storage_cores.len(), 8);
        // One storage core per drive, distinct cpus, drive index matches.
        for (i, sc) in plan.storage_cores.iter().enumerate() {
            assert_eq!(sc.drive, i);
            assert_eq!(sc.numa, Some(0));
        }
        assert_eq!(plan.nic_workers.len(), 1);
        assert_eq!(plan.nic_workers[0].workers.len(), 4);
        assert_eq!(plan.serving_shards.len(), 51);

        let cpus = all_cpus(&plan);
        assert_eq!(cpus.len(), 63);
        assert_eq!(set(cpus.clone()).len(), 63, "all classes disjoint");
        assert!(!set(cpus).contains(&0));
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
        cfg.nic_workers = 2;
        let plan = CorePlan::for_host(&host, &cfg);

        let allowed = set([2, 3, 6, 7]);
        for cpu in all_cpus(&plan) {
            assert!(allowed.contains(&cpu), "cpu {cpu} outside isolated set");
        }
        // 2 nic + 2 serving = the 4 isolated cpus.
        assert_eq!(plan.nic_workers[0].workers.len(), 2);
        assert_eq!(plan.serving_shards.len(), 2);
    }

    #[test]
    fn smt_collapse_drops_siblings() {
        // Pairs (c, c+8). use_smt_siblings=false keeps one sibling per
        // pair. cpu0 is excluded *before* collapse, so the (0,8) group's
        // surviving representative is 8, giving the usable pool
        // {1,2,3,4,5,6,7,8} (8 cpus). The invariant under test is that
        // no SMT pair is ever fully claimed.
        let smt: Vec<Vec<u32>> = (0..8).map(|c| vec![c, c + 8]).collect();
        let host = fake_host(
            vec![(0, (0..16).collect(), smt)],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            vec![],
        );
        let mut cfg = defaults();
        cfg.nic_workers = 2;
        let plan = CorePlan::for_host(&host, &cfg);

        let claimed = set(all_cpus(&plan));
        for c in 0..8u32 {
            assert!(
                !(claimed.contains(&c) && claimed.contains(&(c + 8))),
                "both SMT siblings {c} and {} were claimed",
                c + 8
            );
        }
        // 8 usable: 2 nic + 6 serving.
        assert_eq!(claimed.len(), 8);
        assert_eq!(plan.serving_shards.len(), 6);
    }

    #[test]
    fn three_classes_mutually_disjoint() {
        // Combined host: 2 nodes, 1 NVMe + 1 HCA per node.
        let host = fake_host(
            vec![
                (0, (0..16).collect(), vec![]),
                (1, (16..32).collect(), vec![]),
            ],
            vec![],
            vec![
                hca("mlx5_0", "0000:01:00.0", Some(0), true),
                hca("mlx5_1", "0000:81:00.0", Some(1), true),
            ],
            vec![
                nvme("nvme0", "0000:02:00.0", Some(0)),
                nvme("nvme1", "0000:82:00.0", Some(1)),
            ],
        );
        let plan = CorePlan::for_host(&host, &defaults());

        let storage = set(plan.storage_cores.iter().map(|s| s.cpu));
        let nic = set(plan
            .nic_workers
            .iter()
            .flat_map(|g| g.workers.iter().map(|w| w.cpu)));
        let serving = set(plan.serving_shards.iter().map(|s| s.cpu));

        assert!(storage.is_disjoint(&nic), "storage vs nic overlap");
        assert!(storage.is_disjoint(&serving), "storage vs serving overlap");
        assert!(nic.is_disjoint(&serving), "nic vs serving overlap");
        assert_eq!(storage.len(), 2);
        assert_eq!(nic.len(), 8);
    }

    #[test]
    fn serving_claims_all_remaining() {
        // total usable = filtered pool size; storage + nic + serving
        // must account for exactly that.
        let host = fake_host(
            vec![(0, (0..32).collect(), vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            vec![nvme("nvme0", "0000:02:00.0", Some(0))],
        );
        let plan = CorePlan::for_host(&host, &defaults());
        // cpu0 excluded -> 31 usable. 1 storage + 4 nic + 26 serving.
        assert_eq!(plan.total_cores(), 31);
        assert_eq!(plan.storage_cores.len(), 1);
        assert_eq!(plan.nic_workers[0].workers.len(), 4);
        assert_eq!(plan.serving_shards.len(), 26);
    }

    #[test]
    fn serving_cores_cap_spreads_across_nodes() {
        // 2 nodes, no NVMe/HCA, cap serving to 4. Round-robin spread
        // gives 2 per node.
        let host = fake_host(
            vec![
                (0, (0..8).collect(), vec![]),
                (1, (8..16).collect(), vec![]),
            ],
            vec![],
            vec![],
            vec![],
        );
        let mut cfg = defaults();
        cfg.serving_cores = Some(4);
        let plan = CorePlan::for_host(&host, &cfg);

        assert_eq!(plan.serving_shards.len(), 4);
        let n0 = plan
            .serving_shards
            .iter()
            .filter(|s| s.numa == Some(0))
            .count();
        let n1 = plan
            .serving_shards
            .iter()
            .filter(|s| s.numa == Some(1))
            .count();
        assert_eq!(n0, 2, "serving shards should spread across nodes");
        assert_eq!(n1, 2);
    }

    #[test]
    fn disable_rdma_drops_nic_workers_all_serving() {
        let host = fake_host(
            vec![(0, (0..8).collect(), vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            vec![],
        );
        let mut cfg = defaults();
        cfg.disable_rdma = true;
        let plan = CorePlan::for_host(&host, &cfg);

        assert!(plan.nic_workers.is_empty());
        // cpu0 excluded -> 7 usable, all become serving shards.
        assert_eq!(plan.serving_shards.len(), 7);
        assert!(plan.storage_cores.is_empty());
    }

    #[test]
    fn inactive_hca_dropped() {
        let host = fake_host(
            vec![(0, (0..16).collect(), vec![])],
            vec![],
            vec![
                hca("mlx5_0", "0000:01:00.0", Some(0), false), // inactive
                hca("mlx5_1", "0000:02:00.0", Some(0), true),
            ],
            vec![],
        );
        let plan = CorePlan::for_host(&host, &defaults());
        // Only the active HCA (index 1) contributes a group.
        assert_eq!(plan.nic_workers.len(), 1);
        assert_eq!(plan.nic_workers[0].hca, 1);
    }

    #[test]
    fn oversubscription_never_panics() {
        // Only cpus {0,1}; cpu0 excluded leaves a single usable cpu, but
        // nic_workers=4 demands more. The allocator must oversubscribe
        // without panicking and only ever emit valid online cpu ids.
        let host = fake_host(
            vec![(0, vec![0, 1], vec![])],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", Some(0), true)],
            vec![],
        );
        let plan = CorePlan::for_host(&host, &defaults());

        assert_eq!(plan.nic_workers.len(), 1);
        assert_eq!(plan.nic_workers[0].workers.len(), 4);
        let online = set([0u32, 1]);
        for cpu in all_cpus(&plan) {
            assert!(online.contains(&cpu), "cpu {cpu} is not online");
        }
    }

    #[test]
    fn storage_cores_are_numa_local() {
        let host = fake_host(
            vec![
                (0, (0..16).collect(), vec![]),
                (1, (16..32).collect(), vec![]),
            ],
            vec![],
            vec![],
            vec![
                nvme("nvme0", "0000:02:00.0", Some(0)),
                nvme("nvme1", "0000:82:00.0", Some(1)),
            ],
        );
        let plan = CorePlan::for_host(&host, &defaults());
        assert_eq!(plan.storage_cores.len(), 2);
        for sc in &plan.storage_cores {
            match sc.numa {
                Some(0) => assert!((0..16).contains(&sc.cpu)),
                Some(1) => assert!((16..32).contains(&sc.cpu)),
                other => panic!("unexpected storage core numa {other:?}"),
            }
        }
    }

    #[test]
    fn numa_pools_count_all_classes_per_node() {
        // gb200 shape: per node 15 usable cpus all pinned (8 nic + 7
        // serving). numa_pools should report 15 per node.
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
        let plan = CorePlan::for_host(&host, &defaults());
        assert_eq!(
            plan.numa_pools,
            vec![
                NumaPool {
                    numa: 0,
                    workers: 15,
                },
                NumaPool {
                    numa: 1,
                    workers: 15,
                },
            ]
        );
    }

    #[test]
    fn hca_with_unknown_numa_uses_any_pool() {
        let host = fake_host(
            vec![
                (0, (0..8).collect(), vec![]),
                (1, (8..16).collect(), vec![]),
            ],
            vec![],
            vec![hca("mlx5_0", "0000:01:00.0", None, true)],
            vec![],
        );
        let mut cfg = defaults();
        cfg.nic_workers = 2;
        let plan = CorePlan::for_host(&host, &cfg);
        assert_eq!(plan.nic_workers[0].workers.len(), 2);
        // Workers land on a real node even though the HCA numa is None.
        for w in &plan.nic_workers[0].workers {
            assert!(w.numa == Some(0) || w.numa == Some(1));
        }
    }
}
