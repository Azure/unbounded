//! Racer NIC-local role assignment using runtime-discovered topology.
//!
//! I/O shards share NUMA-local crypto threads at approximately a 2:1 ratio.
//! Use every eligible physical core unless CPU or resource budgets limit workers.
//! On a one-CPU cpuset, pin both roles to that CPU to preserve progress.
//! Quotas limit execution time, not CPU affinity. Keep rail selection deterministic
//! across nodes; local pair placement must not change the page's selected rail.
//! Opt-in SMT counts allowed logical CPUs. Reactors prefer distinct physical cores.

use crate::worker::*;
#[cfg(test)]
use uring_runtime::affinity::CpuQuota;
use uring_runtime::affinity::current_cpus;
use uring_runtime::affinity::pin_cpu;
use uring_runtime::affinity::set_cpus;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::DEFAULT_MAX_THREADS;
    use std::collections::BTreeSet;
    use std::num::NonZeroU64;

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
