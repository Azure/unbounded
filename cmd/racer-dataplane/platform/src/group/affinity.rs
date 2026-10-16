//! Linux CPU discovery and calling-thread affinity, plus Racer placement.
//!
//! Discovery and pinning are the library's ([`uring::affinity`], re-exported
//! here). [`place`] and [`place_exact`] are Racer's lane/helper placement
//! policy over an [`EffectiveTopology`].

use crate::{Error, Result};
use std::collections::{BTreeMap, HashSet};

pub use uring::affinity::*;

/// Discovered local hardware, without application placement policy.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NicLocality {
    /// Network or InfiniBand device name reported by sysfs.
    pub device: String,

    /// Device-local NUMA node, if known.
    pub numa_node: Option<usize>,
}

/// Effective CPU constraints and local devices visible to the calling thread.
///
/// Built from the library [`Topology`]; constructible directly for tests and
/// for callers that filter discovered hardware.
#[derive(Clone, Debug, Default)]
pub struct EffectiveTopology {
    /// Online CPUs intersected with process affinity and effective cpuset.
    pub cpus: Vec<CpuLocation>,

    /// Tightest ancestor CPU-time quota, or none when every hierarchy is unlimited.
    pub quota: Option<CpuQuota>,

    /// Network and InfiniBand devices, with available NUMA locality.
    pub nics: Vec<NicLocality>,
}

impl EffectiveTopology {
    /// Discover constraints from the calling thread's actual Linux namespace.
    pub fn discover() -> Result<Self> {
        Ok(Topology::discover()?.into())
    }
}

impl From<Topology> for EffectiveTopology {
    /// Keep every discovered CPU, the quota, and each device's NUMA locality.
    fn from(topology: Topology) -> Self {
        Self {
            cpus: topology.cpus,
            quota: topology.quota,
            nics: topology
                .devices
                .into_iter()
                .map(|device| NicLocality {
                    device: device.name,
                    numa_node: device.numa_node,
                })
                .collect(),
        }
    }
}

/// One lane and its NUMA-local shared helper execution placement.
#[derive(Clone, Debug)]
pub struct Placement {
    /// CPU that owns the lane's local graph.
    pub lane: CpuLocation,

    /// CPU shared by the lane's helper service.
    pub helper: CpuLocation,

    /// Optional caller-filtered local device hint.
    pub nic: Option<NicLocality>,
}

/// Identity of the physical core a logical CPU runs on, for SMT filtering.
///
/// SMT siblings share `(package, die, core)`. When the kernel does not expose a
/// core id, each logical CPU is treated as its own core.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
enum CoreKey {
    /// A known physical core within an optional package and die.
    Physical(Option<usize>, Option<usize>, usize),
    /// No physical core id: the logical CPU number stands in for it.
    Logical(usize),
}

impl CoreKey {
    /// Derive the physical-core identity of a logical CPU.
    fn of(cpu: &CpuLocation) -> Self {
        match cpu.core {
            Some(core) => Self::Physical(cpu.package, cpu.die, core),
            None => Self::Logical(cpu.cpu),
        }
    }
}

/// Sort by CPU number and reject duplicate logical CPUs.
fn sorted_unique(cpus: &mut [CpuLocation]) -> Result<()> {
    cpus.sort_by_key(|cpu| cpu.cpu);
    if cpus.windows(2).any(|pair| pair[0].cpu == pair[1].cpu) {
        return Err(Error::InvalidConfiguration);
    }
    Ok(())
}

/// Keep only the first logical CPU of each physical core.
fn without_smt(cpus: &mut Vec<CpuLocation>) {
    let mut cores = HashSet::new();
    cpus.retain(|cpu| cores.insert(CoreKey::of(cpu)));
}

/// Order CPUs so one CPU per physical core comes first, each group by CPU number.
fn core_diverse(local: Vec<CpuLocation>) -> Vec<CpuLocation> {
    let mut cores = HashSet::new();
    let mut ordered = local
        .into_iter()
        .map(|cpu| (!cores.insert(CoreKey::of(&cpu)), cpu))
        .collect::<Vec<_>>();
    ordered.sort_by_key(|(sibling, cpu)| (*sibling, cpu.cpu));
    ordered.into_iter().map(|(_, cpu)| cpu).collect()
}

/// Group CPUs by NUMA node, NIC-local nodes first, then by node id.
fn nodes_by_locality(
    cpus: Vec<CpuLocation>,
    nics: &[NicLocality],
) -> Vec<(Option<usize>, Vec<CpuLocation>)> {
    let mut nodes = BTreeMap::<_, Vec<_>>::new();
    for cpu in cpus {
        nodes.entry(cpu.numa_node).or_default().push(cpu);
    }
    let mut nodes = nodes.into_iter().collect::<Vec<_>>();
    nodes.sort_by_key(|(node, _)| (!nics.iter().any(|nic| nic.numa_node == *node), *node));
    nodes
}

/// Sort NICs by name and drop those without NUMA locality.
fn local_nics(mut nics: Vec<NicLocality>) -> Vec<NicLocality> {
    nics.sort_by(|a, b| a.device.cmp(&b.device));
    nics.retain(|nic| nic.numa_node.is_some());
    nics
}

/// Place roughly two lanes per helper within effective CPU and thread budgets.
/// Device filtering and lane identity limits remain caller policy.
pub fn place(
    topology: EffectiveTopology,
    max_threads: usize,
    max_lanes: usize,
    allow_smt: bool,
) -> Result<Vec<Placement>> {
    if max_threads < 2 || topology.cpus.is_empty() {
        return Err(Error::InvalidConfiguration);
    }
    // Fractional CPU capacity still permits two roles sharing one allowed CPU.
    let quota = topology.quota.map(|quota| {
        usize::try_from(quota.whole_cpus())
            .unwrap_or(usize::MAX)
            .max(1)
    });
    let mut cpus = topology.cpus;
    sorted_unique(&mut cpus)?;
    if !allow_smt {
        without_smt(&mut cpus);
    }
    let nics = local_nics(topology.nics);
    let capacity = quota.unwrap_or(cpus.len()).min(cpus.len());
    let mut remaining = max_threads;
    let mut available = capacity;
    let mut pairs = Vec::new();
    for (node, local) in nodes_by_locality(cpus, &nics) {
        if remaining < 2 || available == 0 {
            break;
        }
        // Prefer physical-core diversity among lanes even with SMT enabled.
        let mut local = core_diverse(local);
        let count = local.len().min(remaining).min(available);
        local.truncate(count);
        // Nearest integral 2:1 split spends both remainder cores for n%3=2.
        let helper_count = ((count + 1) / 3).max(1);
        let lane_count = count.saturating_sub(helper_count).max(1);
        let helpers = if count == 1 {
            &local[..]
        } else {
            &local[lane_count..]
        };
        let nic = nics.iter().find(|nic| nic.numa_node == node).cloned();
        for (index, lane) in local[..lane_count].iter().enumerate() {
            if pairs.len() >= max_lanes {
                break;
            }
            pairs.push(Placement {
                lane: lane.clone(),
                helper: helpers[index % helpers.len()].clone(),
                nic: nic.clone(),
            });
        }
        remaining -= count.max(2);
        available -= count;
    }
    Ok(pairs)
}

/// Exact roles on the first preferred NUMA node, without fallback or sharing CPUs.
pub fn place_exact(
    topology: EffectiveTopology,
    max_threads: usize,
    lanes: usize,
    helpers: usize,
    allow_smt: bool,
) -> Result<Vec<Placement>> {
    let total = lanes
        .checked_add(helpers)
        .ok_or(Error::InvalidConfiguration)?;
    if lanes == 0 || helpers == 0 || helpers > lanes || total > max_threads {
        return Err(Error::InvalidConfiguration);
    }
    if topology
        .quota
        .is_some_and(|q| u128::from(q.quota.get()) / u128::from(q.period.get()) < total as u128)
    {
        return Err(Error::InvalidConfiguration);
    }
    let mut cpus = topology.cpus;
    sorted_unique(&mut cpus)?;
    if !allow_smt {
        without_smt(&mut cpus);
    }
    let nics = local_nics(topology.nics);
    let (node, local) = nodes_by_locality(cpus, &nics)
        .into_iter()
        .next()
        .ok_or(Error::InvalidConfiguration)?;
    if node.is_none() || local.len() < total {
        return Err(Error::InvalidConfiguration);
    }
    let local = core_diverse(local);
    let nic = nics.into_iter().find(|nic| nic.numa_node == node);
    Ok((0..lanes)
        .map(|index| Placement {
            lane: local[index].clone(),
            helper: local[lanes + index % helpers].clone(),
            nic: nic.clone(),
        })
        .collect())
}

/// Placement regressions independent of application worker IDs and rails.
#[cfg(test)]
mod placement_tests {
    use super::*;
    use std::{collections::BTreeSet, num::NonZeroU64};

    /// A CPU on package 0 with the given physical core and NUMA node.
    fn cpu(cpu: usize, core: Option<usize>, numa_node: Option<usize>) -> CpuLocation {
        let mut location = CpuLocation::new(cpu);
        location.package = Some(0);
        location.core = core;
        location.numa_node = numa_node;
        location
    }

    /// `cores` CPUs, each its own physical core, on NUMA node 0.
    fn flat(cores: usize) -> EffectiveTopology {
        EffectiveTopology {
            cpus: (0..cores).map(|n| cpu(n, Some(n), Some(0))).collect(),
            quota: None,
            nics: vec![],
        }
    }

    /// Nearest integral 2:1 assignment uses every eligible core and keeps helpers local.
    #[test]
    fn one_through_nine_fill_eligible_cores_with_local_shared_crypto() {
        for (cores, lanes, helpers) in [
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
                let plan = place(flat(cores), usize::MAX, usize::MAX, smt).unwrap();
                assert_eq!(plan.len(), lanes, "cores={cores}, smt={smt}");
                assert_eq!(
                    plan.iter()
                        .map(|p| p.helper.cpu)
                        .collect::<BTreeSet<_>>()
                        .len(),
                    helpers
                );
                let assigned = plan
                    .iter()
                    .flat_map(|p| [p.lane.cpu, p.helper.cpu])
                    .collect::<BTreeSet<_>>();
                assert_eq!(assigned, (0..cores).collect());
                assert!(plan.iter().all(|p| p.lane.numa_node == p.helper.numa_node));
                if cores > 1 {
                    assert!(plan.iter().all(|p| p.lane.cpu != p.helper.cpu));
                }
            }
        }
    }

    /// Caller identity limits do not overflow, and malformed CPU sets fail before placement.
    #[test]
    fn lane_caps_and_invalid_topology_are_explicit() {
        let hardware = flat(9);
        for cap in 0..8 {
            assert_eq!(
                place(hardware.clone(), 9, cap, false).unwrap().len(),
                cap.min(6)
            );
        }
        assert!(matches!(
            place(hardware.clone(), 1, 9, false),
            Err(Error::InvalidConfiguration)
        ));
        let mut duplicate = hardware.clone();
        duplicate.cpus[1].cpu = 0;
        assert!(matches!(
            place(duplicate, 9, 9, true),
            Err(Error::InvalidConfiguration)
        ));
        let mut empty = hardware;
        empty.cpus.clear();
        assert!(matches!(
            place(empty, 9, 9, false),
            Err(Error::InvalidConfiguration)
        ));
    }

    /// SMT siblings are filtered by physical core; unknown core ids count as distinct cores.
    #[test]
    fn smt_filtering_uses_core_ids_and_falls_back_to_cpu_numbers() {
        // CPUs 0/1 and 2/3 are SMT siblings.
        let siblings = EffectiveTopology {
            cpus: (0..4).map(|n| cpu(n, Some(n / 2), Some(0))).collect(),
            quota: None,
            nics: vec![],
        };
        let plan = place(siblings.clone(), usize::MAX, usize::MAX, false).unwrap();
        let used: BTreeSet<_> = plan
            .iter()
            .flat_map(|p| [p.lane.cpu, p.helper.cpu])
            .collect();
        assert_eq!(used, BTreeSet::from([0, 2]));
        // With SMT allowed, lanes still prefer distinct physical cores first.
        let plan = place(siblings, usize::MAX, usize::MAX, true).unwrap();
        assert_eq!(plan[0].lane.cpu, 0);
        assert_eq!(plan[1].lane.cpu, 2);

        let unknown = EffectiveTopology {
            cpus: (0..4).map(|n| cpu(n, None, Some(0))).collect(),
            quota: None,
            nics: vec![],
        };
        let plan = place(unknown, usize::MAX, usize::MAX, false).unwrap();
        let used: BTreeSet<_> = plan
            .iter()
            .flat_map(|p| [p.lane.cpu, p.helper.cpu])
            .collect();
        assert_eq!(used, (0..4).collect());
    }

    /// Quotas cap the CPUs used, and NIC-local nodes are placed first.
    #[test]
    fn quota_caps_and_nic_local_nodes_come_first() {
        let mut topology = EffectiveTopology {
            cpus: (0..8)
                .map(|n| cpu(n, Some(n), Some(usize::from(n >= 4))))
                .collect(),
            quota: Some(CpuQuota::new(
                NonZeroU64::new(300_000).unwrap(),
                NonZeroU64::new(100_000).unwrap(),
            )),
            nics: vec![
                NicLocality {
                    device: "eth1".into(),
                    numa_node: Some(1),
                },
                NicLocality {
                    device: "eth0".into(),
                    numa_node: None,
                },
            ],
        };
        let plan = place(topology.clone(), usize::MAX, usize::MAX, false).unwrap();
        assert_eq!(plan.len(), 2);
        assert!(plan.iter().all(|p| p.lane.numa_node == Some(1)));
        assert_eq!(plan[0].nic.as_ref().unwrap().device, "eth1");
        topology.quota = None;
        assert_eq!(
            place(topology, usize::MAX, usize::MAX, false)
                .unwrap()
                .len(),
            6
        );
    }

    /// Exact placement uses the preferred node only and validates every bound.
    #[test]
    fn exact_placement_validates_bounds_and_uses_one_node() {
        let topology = EffectiveTopology {
            cpus: (0..6)
                .map(|n| cpu(n, Some(n), Some(usize::from(n >= 3))))
                .collect(),
            quota: None,
            nics: vec![NicLocality {
                device: "ib0".into(),
                numa_node: Some(1),
            }],
        };
        let plan = place_exact(topology.clone(), 3, 2, 1, false).unwrap();
        assert_eq!(
            plan.iter().map(|p| p.lane.cpu).collect::<Vec<_>>(),
            vec![3, 4]
        );
        assert!(plan.iter().all(|p| p.helper.cpu == 5));
        assert!(plan.iter().all(|p| p.nic.as_ref().unwrap().device == "ib0"));
        for (max_threads, lanes, helpers) in [(3, 0, 1), (3, 1, 0), (3, 1, 2), (2, 2, 1), (9, 3, 1)]
        {
            assert_eq!(
                place_exact(topology.clone(), max_threads, lanes, helpers, false).unwrap_err(),
                Error::InvalidConfiguration,
                "{max_threads} {lanes} {helpers}"
            );
        }
        let mut quota = topology.clone();
        quota.quota = Some(CpuQuota::new(
            NonZeroU64::new(200_000).unwrap(),
            NonZeroU64::new(100_000).unwrap(),
        ));
        assert!(place_exact(quota, 3, 2, 1, false).is_err());
        let mut unknown = topology;
        for cpu in &mut unknown.cpus {
            cpu.numa_node = None;
        }
        unknown.nics.clear();
        assert!(place_exact(unknown, 3, 2, 1, false).is_err());
        assert!(place_exact(EffectiveTopology::default(), 2, 1, 1, false).is_err());
    }

    /// Discovery on the host yields at least the calling thread's CPU.
    #[test]
    fn host_discovery_finds_cpus() {
        let topology = EffectiveTopology::discover().unwrap();
        assert!(!topology.cpus.is_empty());
    }
}
