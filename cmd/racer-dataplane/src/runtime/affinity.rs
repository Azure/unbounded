//! Racer NIC-local role assignment using runtime-discovered topology.
//!
//! I/O shards share NUMA-local crypto threads at approximately a 2:1 ratio.
//! Use every eligible physical core unless CPU or resource budgets limit workers.
//! On a one-CPU cpuset, pin both roles to that CPU to preserve progress.
//! Quotas limit execution time, not CPU affinity. Keep rail selection deterministic
//! across nodes; local pair placement must not change the page's selected rail.
//! Opt-in SMT counts allowed logical CPUs. Reactors prefer distinct physical cores.

use crate::{
    config::Config,
    error::{Error, Result},
    model::WorkerId,
    topology::rails::RailMapping,
};
use std::collections::{BTreeMap, HashSet};
pub use uring_runtime::affinity::{CpuLocation, CpuQuota, EffectiveTopology, NicLocality};
#[cfg(test)]
pub(crate) use uring_runtime::affinity::{current_cpus, pin_cpu, set_cpus};

/// One I/O shard and its crypto execution placement. Equal crypto CPU IDs across
/// assignments explicitly identify the same execution thread, not duplicate threads.
/// NIC locality never overrides the deterministic end-to-end rail.
#[derive(Clone, Debug)]
pub struct WorkerPair {
    pub worker: WorkerId,
    pub io: CpuLocation,
    pub crypto: CpuLocation,
    pub nic: Option<NicLocality>,
}

pub struct AffinityPlan {
    /// Per-I/O assignments with unique WorkerIds. Control/diagnostics run on I/O.
    pub pairs: Vec<WorkerPair>,
    /// Whole-process budget. `run` uses its caller as the first reactor. Owned
    /// startup must reserve one additional slot for the calling thread.
    pub max_threads: usize,
}

/// Pure sizing policy, not hardware discovery or a claim that threads have started.
/// Count I/O shards using the same physical-core and NUMA policy as placement.
pub fn pair_count(max_threads: usize, topology: &EffectiveTopology) -> Result<usize> {
    pair_count_with_policy(max_threads, topology, false)
}

fn pair_count_with_policy(
    max_threads: usize,
    topology: &EffectiveTopology,
    allow_smt: bool,
) -> Result<usize> {
    Ok(
        AffinityPlan::place_with_policy(max_threads, topology.clone(), &[], allow_smt)?
            .pairs
            .len(),
    )
}

impl AffinityPlan {
    /// Honor allowed CPUs, quotas, and the total thread cap.
    pub fn discover(config: &Config) -> Result<Self> {
        Self::from_topology(
            config,
            EffectiveTopology::discover().map_err(Error::from)?,
            &[],
        )
    }
    /// Plan from discovered constraints and already accepted rail mappings. Check
    /// NIC/NUMA compatibility locally, without changing membership or page-to-rail
    /// selection. Missing/incompatible hardware keeps HTTP fallback available.
    /// Validate unique workers, allowed CPUs, and pair_count before pinning. No
    /// additional control, telemetry, or helper threads may exceed max_threads.
    pub fn from_topology(
        config: &Config,
        topology: EffectiveTopology,
        rails: &[RailMapping],
    ) -> Result<Self> {
        Self::place_with_policy(config.max_threads, topology, rails, config.allow_smt)
    }
    #[cfg(test)]
    fn place(
        max_threads: usize,
        topology: EffectiveTopology,
        rails: &[RailMapping],
    ) -> Result<Self> {
        Self::place_with_policy(max_threads, topology, rails, false)
    }

    fn place_with_policy(
        max_threads: usize,
        topology: EffectiveTopology,
        rails: &[RailMapping],
        allow_smt: bool,
    ) -> Result<Self> {
        if max_threads < 2 || topology.cpus.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        // Fractional CPU capacity still permits one pair sharing allowed CPUs.
        let quota = topology.quota.map(|quota| {
            usize::try_from(quota.quota.get() / quota.period.get())
                .unwrap_or(usize::MAX)
                .max(1)
        });
        let mut cpus = topology.cpus;
        cpus.sort_by_key(|cpu| cpu.cpu);
        if cpus.windows(2).any(|pair| pair[0].cpu == pair[1].cpu) {
            return Err(Error::InvalidConfiguration);
        }
        if !allow_smt {
            let mut cores = HashSet::new();
            cpus.retain(|cpu| cores.insert((cpu.package, cpu.core)));
        }
        let mut nics = topology.nics;
        nics.sort_by(|a, b| a.device.cmp(&b.device));
        // RailMapping carries a fabric label, not a local device identifier. NUMA
        // compatibility is a placement hint only, never proof of RDMA eligibility.
        nics.retain(|nic| {
            nic.numa_node.is_some()
                && (rails.is_empty() || rails.iter().any(|rail| rail.numa_node == nic.numa_node))
        });
        let capacity = quota.unwrap_or(cpus.len()).min(cpus.len());
        let mut remaining = max_threads;
        let mut available = capacity;
        let mut nodes = BTreeMap::<_, Vec<_>>::new();
        for cpu in cpus {
            nodes.entry(cpu.numa_node).or_default().push(cpu);
        }
        let mut nodes = nodes.into_iter().collect::<Vec<_>>();
        nodes.sort_by_key(|(node, _)| (!nics.iter().any(|nic| nic.numa_node == *node), *node));
        let mut pairs = Vec::new();
        for (node, local) in nodes {
            if remaining < 2 || available == 0 {
                break;
            }
            // Prefer physical-core diversity among the reactors even with SMT.
            let mut cores = HashSet::new();
            let mut ordered = local
                .into_iter()
                .map(|cpu| (!cores.insert((cpu.package, cpu.core)), cpu))
                .collect::<Vec<_>>();
            ordered.sort_by_key(|(sibling, cpu)| (*sibling, cpu.cpu));
            let mut local = ordered.into_iter().map(|(_, cpu)| cpu).collect::<Vec<_>>();
            let count = local.len().min(remaining).min(available);
            local.truncate(count);
            // Nearest integral 2:1 split, spending both remainder cores when
            // n % 3 == 2 (eight cores become five I/O plus three crypto).
            let crypto_count = ((count + 1) / 3).max(1);
            let io_count = count.saturating_sub(crypto_count).max(1);
            let crypto = if count == 1 {
                &local[..]
            } else {
                &local[io_count..]
            };
            let nic = nics.iter().find(|nic| nic.numa_node == node).cloned();
            for (index, io) in local[..io_count].iter().enumerate() {
                if pairs.len() > usize::from(u16::MAX) {
                    break;
                }
                pairs.push(WorkerPair {
                    worker: WorkerId(pairs.len() as u16),
                    io: io.clone(),
                    crypto: crypto[index % crypto.len()].clone(),
                    nic: nic.clone(),
                });
            }
            remaining -= count.max(2);
            available -= count;
        }
        Ok(Self { pairs, max_threads })
    }

    /// Deterministic execution groups ordered by crypto CPU, then pair index.
    pub fn crypto_groups(&self) -> Vec<Vec<usize>> {
        let mut groups = BTreeMap::<usize, Vec<usize>>::new();
        for (index, pair) in self.pairs.iter().enumerate() {
            groups.entry(pair.crypto.cpu).or_default().push(index);
        }
        groups.into_values().collect()
    }

    /// Drop unfunded I/O shards and rebalance their surviving local crypto groups.
    /// Never add threads or move an assignment across a known NUMA boundary.
    pub(crate) fn reduce_workers(&mut self, count: usize) {
        self.pairs.truncate(count);
        let mut nodes = BTreeMap::<_, Vec<usize>>::new();
        for (index, pair) in self.pairs.iter().enumerate() {
            nodes.entry(pair.io.numa_node).or_default().push(index);
        }
        for indices in nodes.into_values() {
            let mut crypto = BTreeMap::new();
            for &index in &indices {
                let cpu = &self.pairs[index].crypto;
                crypto.entry(cpu.cpu).or_insert_with(|| cpu.clone());
            }
            let crypto = crypto
                .into_values()
                .take(indices.len().div_ceil(2))
                .collect::<Vec<_>>();
            for (offset, index) in indices.into_iter().enumerate() {
                self.pairs[index].crypto = crypto[offset % crypto.len()].clone();
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::DEFAULT_MAX_THREADS;
    use std::{collections::BTreeSet, num::NonZeroU64};

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
    fn one_through_nine_fill_eligible_cores_with_local_shared_crypto() {
        for (cores, io, crypto) in [
            (1, 1, 1),
            (2, 1, 1),
            (3, 2, 1),
            (4, 3, 1),
            (5, 3, 2),
            (6, 4, 2),
            (7, 5, 2),
            (8, 5, 3),
            (9, 6, 3),
        ] {
            for smt in [false, true] {
                let plan =
                    AffinityPlan::place_with_policy(usize::MAX, topology(cores, None), &[], smt)
                        .unwrap();
                assert_eq!(plan.pairs.len(), io, "cores={cores}, smt={smt}");
                assert_eq!(plan.crypto_groups().len(), crypto);
                let assigned = plan
                    .pairs
                    .iter()
                    .flat_map(|pair| [pair.io.cpu, pair.crypto.cpu])
                    .collect::<BTreeSet<_>>();
                assert_eq!(assigned, (0..cores).collect());
                assert!(
                    plan.pairs
                        .iter()
                        .all(|pair| pair.io.numa_node == pair.crypto.numa_node)
                );
                if cores > 1 {
                    assert!(plan.pairs.iter().all(|pair| pair.io.cpu != pair.crypto.cpu));
                }
            }
        }
    }

    #[test]
    fn uneven_numa_nodes_never_borrow_crypto_and_quota_is_global() {
        for sizes in [
            vec![1, 1],
            vec![1, 7],
            vec![2, 5, 9],
            vec![4, 4],
            vec![1, 1, 1],
        ] {
            let mut hardware = topology(sizes.iter().sum(), None);
            let mut offset = 0;
            for (node, size) in sizes.iter().enumerate() {
                for cpu in &mut hardware.cpus[offset..offset + size] {
                    cpu.numa_node = Some(node);
                }
                offset += size;
            }
            for quota in [None, Some((1, 2)), Some((3, 1)), Some((7, 1))] {
                let mut hardware = hardware.clone();
                hardware.quota = quota.map(|(q, p)| CpuQuota {
                    quota: NonZeroU64::new(q).unwrap(),
                    period: NonZeroU64::new(p).unwrap(),
                });
                let capacity = quota
                    .map_or(offset, |(q, p)| (q / p).max(1) as usize)
                    .min(offset);
                let plan = AffinityPlan::place(usize::MAX, hardware, &[]).unwrap();
                assert!(
                    plan.pairs
                        .iter()
                        .all(|pair| pair.io.numa_node == pair.crypto.numa_node)
                );
                let assigned = plan
                    .pairs
                    .iter()
                    .flat_map(|pair| [pair.io.cpu, pair.crypto.cpu])
                    .collect::<BTreeSet<_>>();
                assert_eq!(assigned.len(), capacity);
                for group in plan.crypto_groups() {
                    assert!(
                        group.iter().all(|index| plan.pairs[*index].crypto.cpu
                            == plan.pairs[group[0]].crypto.cpu)
                    );
                }
            }
        }
    }

    #[test]
    fn explicit_caps_bound_execution_threads_even_with_singleton_numa_nodes() {
        for sizes in [vec![1, 1, 1], vec![1, 8], vec![4, 5], vec![8, 1]] {
            for cap in 2..=12 {
                let mut hardware = topology(sizes.iter().sum(), None);
                let mut offset = 0;
                for (node, size) in sizes.iter().enumerate() {
                    for cpu in &mut hardware.cpus[offset..offset + size] {
                        cpu.numa_node = Some(node);
                    }
                    offset += size;
                }
                let plan = AffinityPlan::place(cap, hardware, &[]).unwrap();
                assert!(!plan.pairs.is_empty());
                assert!(plan.pairs.len() + plan.crypto_groups().len() <= cap);
                assert!(
                    plan.pairs
                        .iter()
                        .all(|pair| pair.io.numa_node == pair.crypto.numa_node)
                );
            }
        }
        let plan = AffinityPlan::place(usize::MAX, topology(300, None), &[]).unwrap();
        assert_eq!(plan.pairs.len(), 200);
        assert_eq!(plan.crypto_groups().len(), 100);
    }

    #[test]
    fn crypto_groups_are_cpu_sorted_and_reduction_rebalances_locally() {
        let mut hardware = topology(16, None);
        for cpu in &mut hardware.cpus {
            cpu.numa_node = Some(cpu.cpu / 8);
        }
        let mut plan = AffinityPlan::place(usize::MAX, hardware, &[]).unwrap();
        assert_eq!(
            plan.crypto_groups(),
            vec![
                vec![0, 3],
                vec![1, 4],
                vec![2],
                vec![5, 8],
                vec![6, 9],
                vec![7]
            ]
        );
        plan.reduce_workers(7);
        assert_eq!(
            plan.crypto_groups(),
            vec![vec![0, 3], vec![1, 4], vec![2], vec![5, 6]]
        );
        assert!(
            plan.pairs
                .iter()
                .all(|pair| pair.io.numa_node == pair.crypto.numa_node)
        );
        plan.reduce_workers(2);
        assert_eq!(plan.crypto_groups(), vec![vec![0, 1]]);
        plan.pairs.reverse();
        assert_eq!(plan.crypto_groups(), vec![vec![0, 1]]);
    }

    #[test]
    fn sizing_always_budgets_full_pairs_including_single_cpu() {
        for (cap, cores, expected) in [
            (8, 1, 1),
            (8, 2, 1),
            (8, 3, 2),
            (8, 7, 5),
            (7, 8, 5),
            (3, 8, 2),
            (2, 1, 1),
        ] {
            assert_eq!(pair_count(cap, &topology(cores, None)), Ok(expected));
        }
        assert_eq!(pair_count(DEFAULT_MAX_THREADS, &topology(64, None)), Ok(43));
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
        for (quota, period, expected) in [(1, 2, 1), (3, 1, 2), (7, 2, 2), (4, 1, 3), (9, 1, 5)] {
            assert_eq!(
                pair_count(8, &topology(16, Some((quota, period)))),
                Ok(expected)
            );
        }
        let mut smt = topology(8, None);
        for cpu in &mut smt.cpus {
            cpu.core /= 2;
        }
        assert_eq!(pair_count(8, &smt), Ok(3));
        // Core zero on different packages is not an SMT sibling.
        for cpu in &mut smt.cpus {
            cpu.package = cpu.cpu;
            cpu.core = 0;
        }
        assert_eq!(pair_count(8, &smt), Ok(5));
    }

    #[test]
    fn smt_places_four_unique_pairs_and_preserves_default_physical_policy() {
        let mut hardware = topology(8, None);
        for cpu in &mut hardware.cpus {
            cpu.core %= 4;
        }
        let physical = AffinityPlan::place(
            8,
            EffectiveTopology {
                cpus: hardware.cpus.clone(),
                quota: None,
                nics: vec![],
            },
            &[],
        )
        .unwrap();
        assert_eq!(
            physical
                .pairs
                .iter()
                .map(|p| (p.io.cpu, p.crypto.cpu))
                .collect::<Vec<_>>(),
            vec![(0, 3), (1, 3), (2, 3)]
        );
        let plan = AffinityPlan::place_with_policy(8, hardware, &[], true).unwrap();
        assert_eq!(
            plan.pairs
                .iter()
                .map(|p| (p.io.cpu, p.crypto.cpu))
                .collect::<Vec<_>>(),
            vec![(0, 5), (1, 6), (2, 7), (3, 5), (4, 6)]
        );
        assert_eq!(plan.max_threads, 8);
    }

    #[test]
    fn smt_sizing_honors_quotas_caps_and_complete_unique_pairs() {
        for (cap, logical, quota, expected) in [
            (8, 8, None, 5),
            (8, 64, None, 5),
            (7, 8, None, 5),
            (8, 7, None, 5),
            (8, 3, None, 2),
            (2, 8, None, 1),
            (8, 8, Some((7, 2)), 2),
            (8, 8, Some((4, 1)), 3),
            (8, 8, Some((7, 1)), 5),
            (8, 8, Some((1, 2)), 1),
        ] {
            let mut hardware = topology(logical, quota);
            for cpu in &mut hardware.cpus {
                cpu.core /= 2;
            }
            let plan = AffinityPlan::place_with_policy(cap, hardware, &[], true).unwrap();
            assert_eq!(plan.pairs.len(), expected);
            let assigned = plan
                .pairs
                .iter()
                .flat_map(|p| [p.io.cpu, p.crypto.cpu])
                .collect::<BTreeSet<_>>();
            let capacity = quota.map_or(logical, |(q, p)| (q / p).max(1) as usize);
            assert_eq!(assigned.len(), logical.min(cap).min(capacity));
            assert!(plan.pairs.len() + plan.crypto_groups().len() <= cap);
            assert!(assigned.iter().all(|cpu| *cpu < logical));
        }
        for (cap, logical) in [(1, 8), (8, 0)] {
            assert!(
                AffinityPlan::place_with_policy(cap, topology(logical, None), &[], true).is_err()
            );
        }
        let mut duplicate = topology(8, None);
        duplicate.cpus[7].cpu = 0;
        assert!(AffinityPlan::place_with_policy(8, duplicate, &[], true).is_err());
    }

    #[test]
    fn smt_irregular_cpuset_reserves_reactors_and_siblings_before_fallback() {
        // Sparse allowed IDs, package-local core IDs, missing siblings, and a
        // four-way SMT core. Only the online/affinity/cpuset intersection is input.
        let cpus = [
            (2, 0, 0),
            (8, 1, 0),
            (10, 0, 2),
            (14, 1, 2),
            (18, 0, 2),
            (22, 0, 2),
            (26, 0, 2),
            (30, 1, 2),
        ]
        .map(|(cpu, package, core)| CpuLocation {
            cpu,
            package,
            core,
            numa_node: Some(package),
        });
        for reverse in [false, true] {
            let mut allowed = cpus.to_vec();
            if reverse {
                allowed.reverse();
            }
            let hardware = EffectiveTopology {
                cpus: allowed,
                quota: None,
                nics: vec![
                    NicLocality {
                        device: "eth1".into(),
                        numa_node: Some(1),
                    },
                    NicLocality {
                        device: "eth0".into(),
                        numa_node: Some(0),
                    },
                ],
            };
            let plan = AffinityPlan::place_with_policy(8, hardware, &[], true).unwrap();
            assert_eq!(
                plan.pairs
                    .iter()
                    .map(|p| (p.io.cpu, p.crypto.cpu))
                    .collect::<Vec<_>>(),
                vec![(2, 22), (10, 26), (18, 22), (8, 30), (14, 30)]
            );
            for (index, pair) in plan.pairs.iter().enumerate() {
                assert_eq!(pair.worker, WorkerId(index as u16));
                assert!(pair.nic.is_some());
                assert_eq!(pair.io.numa_node, pair.crypto.numa_node);
            }
        }
        // All subsets model irregular allowed intersections, including SMT-only
        // cpusets. No rejected/offline CPU may be introduced to complete a pair.
        for mask in 0u16..256 {
            let allowed = cpus
                .iter()
                .enumerate()
                .filter(|(i, _)| mask & (1 << i) != 0)
                .map(|(_, cpu)| cpu.clone())
                .collect::<Vec<_>>();
            let ids = allowed.iter().map(|cpu| cpu.cpu).collect::<BTreeSet<_>>();
            let result = AffinityPlan::place_with_policy(
                8,
                EffectiveTopology {
                    cpus: allowed,
                    quota: None,
                    nics: vec![],
                },
                &[],
                true,
            );
            if ids.is_empty() {
                assert!(result.is_err());
                continue;
            }
            let plan = result.unwrap();
            let assigned = plan
                .pairs
                .iter()
                .flat_map(|p| [p.io.cpu, p.crypto.cpu])
                .collect::<BTreeSet<_>>();
            assert_eq!(assigned.len(), ids.len());
            assert!(assigned.is_subset(&ids));
            let reactor_cores = plan
                .pairs
                .iter()
                .map(|p| (p.io.package, p.io.core))
                .collect::<HashSet<_>>();
            let cores = cpus
                .iter()
                .filter(|cpu| ids.contains(&cpu.cpu))
                .map(|cpu| (cpu.package, cpu.core))
                .collect::<HashSet<_>>();
            assert!(reactor_cores.len() <= cores.len());
            assert!(
                plan.pairs
                    .iter()
                    .all(|pair| pair.io.numa_node == pair.crypto.numa_node)
            );
            for node in [Some(0), Some(1)] {
                let local_cores = cores
                    .iter()
                    .filter(|(package, _)| Some(*package) == node)
                    .count();
                let local_reactors = plan
                    .pairs
                    .iter()
                    .filter(|pair| pair.io.numa_node == node)
                    .count();
                let distinct = reactor_cores
                    .iter()
                    .filter(|(package, _)| Some(*package) == node)
                    .count();
                assert_eq!(distinct, local_reactors.min(local_cores));
            }
        }
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
        assert_eq!(pair.io.cpu, 0);
        assert_eq!(pair.crypto.cpu, 0);
    }

    #[test]
    fn placement_prefers_distinct_nic_local_cores_and_rejects_duplicates() {
        let mut hardware = topology(8, None);
        for cpu in &mut hardware.cpus {
            cpu.numa_node = Some(cpu.cpu / 4);
        }
        hardware.nics.push(NicLocality {
            device: "eth0".into(),
            numa_node: Some(1),
        });
        let plan = AffinityPlan::place(4, hardware, &[]).unwrap();
        assert_eq!(plan.pairs[0].io.cpu, 4);
        assert_eq!(plan.pairs[0].crypto.cpu, 7);
        assert_eq!(plan.pairs[1].io.cpu, 5);
        assert_eq!(plan.pairs[1].crypto.cpu, 7);
        let mut duplicate = topology(2, None);
        duplicate.cpus[1].cpu = 0;
        assert!(AffinityPlan::place(4, duplicate, &[]).is_err());
        let mut mismatch = topology(2, None);
        mismatch.nics.push(NicLocality {
            device: "eth0".into(),
            numa_node: Some(1),
        });
        assert!(
            AffinityPlan::place(4, mismatch, &[]).unwrap().pairs[0]
                .nic
                .is_none()
        );
    }

    #[test]
    fn actual_affinity_is_applied_on_the_calling_thread() {
        std::thread::spawn(|| {
            let allowed = current_cpus().unwrap();
            let cpu = *allowed.first().unwrap();
            pin_cpu(cpu).unwrap();
            let discovered = EffectiveTopology::discover().unwrap();
            let plan = AffinityPlan::place_with_policy(8, discovered, &[], true).unwrap();
            assert_eq!(plan.pairs.len(), 1);
            assert_eq!(plan.pairs[0].io.cpu, plan.pairs[0].crypto.cpu);
            set_cpus(&allowed).unwrap();
        })
        .join()
        .unwrap();
    }
}
