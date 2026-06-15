// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shared test fixtures for the topology module.
//!
//! `cores` and `filters` both drive their unit tests off hand-built
//! `Host` snapshots; these builders keep that fixture in one place so the
//! two files cannot drift apart.

use std::collections::{BTreeMap, BTreeSet};

use crate::topology::{Cpu, Hca, Host, NumaNode, Nvme};

/// Builds a `Host` snapshot directly without touching sysfs.
/// `nodes` is `(numa_id, cpu_ids, smt_groups)` where `smt_groups`
/// is a list of sibling sets covering the CPUs on that node; pass an
/// empty Vec for "every CPU is its own group".
pub(super) fn fake_host(
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
        nics: vec![],
        isolated,
    }
}

pub(super) fn hca(dev: &str, bdf: &str, numa: Option<u16>, active: bool) -> Hca {
    Hca {
        dev_name: dev.to_string(),
        pci_bdf: Some(bdf.to_string()),
        pcie_root: None,
        numa,
        ports_active: active,
    }
}

pub(super) fn nvme(dev: &str, bdf: &str, numa: Option<u16>) -> Nvme {
    Nvme {
        dev_name: dev.to_string(),
        pci_bdf: Some(bdf.to_string()),
        pcie_root: None,
        numa,
    }
}
