// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shared CPU/HCA filtering engine for the core planner.
//!
//! These helpers decide which CPUs and HCAs are *usable* on a host,
//! independent of how many cores any class demands. [`CorePlanConfig`]
//! (in [`cores`](super::cores)) projects down to [`Filters`] so the
//! filtering rules - SMT collapse, isolcpus handling, core-0 exclusion,
//! and inactive-port rejection - live in exactly one place.

use super::{Hca, Host};

/// The subset of planner knobs that govern which CPUs and HCAs are
/// usable, independent of how many cores each class demands.
/// [`CorePlanConfig`](super::CorePlanConfig) projects down to this so
/// the CPU/HCA filtering engine ([`build_node_pool`], [`filter_hcas`],
/// `collapse_smt`) has a single source of truth.
#[derive(Clone, Copy, Debug)]
pub(super) struct Filters {
    pub use_smt_siblings: bool,
    pub respect_isolated: bool,
    pub exclude_node_cpu0: bool,
    pub require_node_type_ca: bool,
    pub require_active_port: bool,
    pub disable_rdma: bool,
}

/// Apply HCA filters from `f`. Preserves the input order and keeps the
/// original slice index so callers can still point at the right
/// `host.hcas[i]`.
pub(super) fn filter_hcas<'a>(hcas: &'a [Hca], f: &Filters) -> Vec<(usize, &'a Hca)> {
    if f.disable_rdma {
        return Vec::new();
    }

    hcas.iter()
        .enumerate()
        .filter(|(_, h)| {
            // require_node_type_ca: discovery already records only
            // CA-shaped devices today, so the flag is a no-op now.
            // Kept gated for future hardware that exposes router or
            // switch node types in the same sysfs class.
            let _ = f.require_node_type_ca;
            if f.require_active_port && !h.ports_active {
                return false;
            }

            true
        })
        .collect()
}

/// Build the filtered per-NUMA primary pool for `node`.
pub(super) fn build_node_pool(host: &Host, node: u16, f: &Filters) -> Vec<u32> {
    let raw = host.cpus_on(Some(node));
    let mut filtered = filter_node_cpus(host, &raw, f);

    if filtered.is_empty() && !raw.is_empty() {
        eprintln!(
            "topology: node {node} has no CPUs left after filtering; falling back to unfiltered set"
        );
        filtered = raw;
    }

    filtered
}

fn filter_node_cpus(host: &Host, raw: &[u32], f: &Filters) -> Vec<u32> {
    let mut cpus: Vec<u32> = raw.to_vec();

    if f.respect_isolated && !host.isolated.is_empty() {
        cpus.retain(|c| host.isolated.contains(c));
    } else if f.exclude_node_cpu0 {
        if let Some(min) = cpus.iter().copied().min() {
            cpus.retain(|c| *c != min);
        }
    }

    if !f.use_smt_siblings {
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::topology::{Cpu, Hca, Host, NumaNode};
    use std::collections::{BTreeMap, BTreeSet};

    /// Builds a `Host` snapshot directly without touching sysfs.
    /// `nodes` is `(numa_id, cpu_ids, smt_groups)` where `smt_groups`
    /// is a list of sibling sets that cover the CPUs on that node;
    /// pass an empty Vec for "every CPU is its own group".
    fn fake_host(
        nodes: Vec<(u16, Vec<u32>, Vec<Vec<u32>>)>,
        isolated: Vec<u32>,
        hcas: Vec<Hca>,
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
            nvmes: vec![],
            nics: vec![],
            isolated,
        }
    }

    fn hca(dev: &str, bdf: &str, numa: Option<u16>, active: bool) -> Hca {
        Hca {
            dev_name: dev.to_string(),
            pci_bdf: Some(bdf.to_string()),
            pcie_root: None,
            numa,
            ports_active: active,
        }
    }

    fn filters() -> Filters {
        Filters {
            use_smt_siblings: false,
            respect_isolated: true,
            exclude_node_cpu0: true,
            require_node_type_ca: true,
            require_active_port: true,
            disable_rdma: false,
        }
    }

    #[test]
    fn filter_hcas_disable_rdma_drops_everything() {
        let hcas = vec![
            hca("mlx5_0", "0000:01:00.0", Some(0), true),
            hca("mlx5_1", "0000:02:00.0", Some(0), true),
        ];
        let mut f = filters();
        f.disable_rdma = true;
        assert!(filter_hcas(&hcas, &f).is_empty());
    }

    #[test]
    fn filter_hcas_drops_inactive_keeps_index() {
        let hcas = vec![
            hca("mlx5_0", "0000:01:00.0", Some(0), false), // inactive
            hca("mlx5_1", "0000:02:00.0", Some(0), true),
        ];
        let kept = filter_hcas(&hcas, &filters());
        assert_eq!(kept.len(), 1);
        // The surviving HCA keeps its original slice index (1).
        assert_eq!(kept[0].0, 1);
        assert_eq!(kept[0].1.dev_name, "mlx5_1");
    }

    #[test]
    fn filter_hcas_allow_inactive_keeps_all() {
        let hcas = vec![
            hca("mlx5_0", "0000:01:00.0", Some(0), false),
            hca("mlx5_1", "0000:02:00.0", Some(0), true),
        ];
        let mut f = filters();
        f.require_active_port = false;
        let kept = filter_hcas(&hcas, &f);
        assert_eq!(kept.len(), 2);
        assert_eq!(kept[0].0, 0);
        assert_eq!(kept[1].0, 1);
    }

    #[test]
    fn build_node_pool_excludes_cpu0() {
        let host = fake_host(vec![(0, (0..8).collect(), vec![])], vec![], vec![]);
        let pool = build_node_pool(&host, 0, &filters());
        assert_eq!(pool, vec![1, 2, 3, 4, 5, 6, 7]);
    }

    #[test]
    fn build_node_pool_respects_isolated() {
        // isolcpus wins over cpu0 exclusion: only the isolated cpus
        // survive, even cpu0 if it is isolated.
        let host = fake_host(vec![(0, (0..8).collect(), vec![])], vec![0, 3, 5], vec![]);
        let pool = build_node_pool(&host, 0, &filters());
        assert_eq!(pool, vec![0, 3, 5]);
    }

    #[test]
    fn build_node_pool_collapses_smt_siblings() {
        // Pairs (c, c+8). With use_smt_siblings=false only one sibling
        // per pair survives. cpu0 is excluded before collapse, so the
        // (0,8) pair's surviving representative is 8.
        let smt: Vec<Vec<u32>> = (0..8).map(|c| vec![c, c + 8]).collect();
        let host = fake_host(vec![(0, (0..16).collect(), smt)], vec![], vec![]);
        let pool = build_node_pool(&host, 0, &filters());
        let set: BTreeSet<u32> = pool.iter().copied().collect();
        // No SMT pair fully present.
        for c in 0..8u32 {
            assert!(
                !(set.contains(&c) && set.contains(&(c + 8))),
                "both SMT siblings {c} and {} survived",
                c + 8
            );
        }
        // 8 usable cpus: representative of (0,8) is 8 (0 excluded),
        // the rest keep their low sibling {1..=7}.
        assert_eq!(set, BTreeSet::from([1, 2, 3, 4, 5, 6, 7, 8]));
    }

    #[test]
    fn build_node_pool_keeps_smt_when_requested() {
        let smt: Vec<Vec<u32>> = (0..4).map(|c| vec![c, c + 4]).collect();
        let host = fake_host(vec![(0, (0..8).collect(), smt)], vec![], vec![]);
        let mut f = filters();
        f.use_smt_siblings = true;
        let pool = build_node_pool(&host, 0, &f);
        // cpu0 still excluded, but no sibling collapse: {1..=7}.
        assert_eq!(pool, vec![1, 2, 3, 4, 5, 6, 7]);
    }

    #[test]
    fn build_node_pool_falls_back_when_filters_empty_the_node() {
        // respect_isolated with an isolated set that lives entirely on
        // another node leaves node 0 empty; the fallback returns the
        // unfiltered raw set rather than nothing.
        let host = fake_host(
            vec![(0, (0..4).collect(), vec![]), (1, (4..8).collect(), vec![])],
            vec![4, 5, 6, 7],
            vec![],
        );
        let pool = build_node_pool(&host, 0, &filters());
        assert_eq!(pool, vec![0, 1, 2, 3], "fallback to unfiltered node set");
    }
}
