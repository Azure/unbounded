//! Linux CPU, cgroup, NUMA, and NIC discovery and calling-thread affinity.
//! Placement policy belongs to the caller, not the runtime. Thread affinity
//! itself is `racer_io::pin`.
pub use racer_io::pin::{current_cpus, pin_cpu, set_cpus};
use racer_io::{Error, Result};
use std::{
    collections::{BTreeMap, BTreeSet, HashSet},
    fs,
    num::NonZeroU64,
    path::{Path, PathBuf},
};

/// Logical CPU identity and its physical placement, without scheduling policy.
#[derive(Clone, Debug)]
pub struct CpuLocation {
    /// Logical CPU number accepted by Linux affinity syscalls.
    pub cpu: usize,

    /// Physical core IDs are package-local; SMT siblings share this pair.
    pub package: usize,

    /// Physical core number within the package.
    pub core: usize,

    /// NUMA node when sysfs exposes one.
    pub numa_node: Option<usize>,
}

/// Tightest applicable effective cgroup CPU-time quota. None means unlimited.
#[derive(Clone, Copy, Debug)]
pub struct CpuQuota {
    /// CPU time available during each accounting period, in microseconds.
    pub quota: NonZeroU64,

    /// Accounting period in microseconds.
    pub period: NonZeroU64,
}

/// Discovered local hardware, without application placement policy.
#[derive(Clone, Debug)]
pub struct NicLocality {
    /// Network or InfiniBand device name reported by sysfs.
    pub device: String,

    /// Device-local NUMA node, if known.
    pub numa_node: Option<usize>,
}

/// Effective CPU constraints and local devices visible to the calling thread.
#[derive(Clone)]
pub struct EffectiveTopology {
    /// Online CPUs intersected with process affinity and effective cpuset.
    pub cpus: Vec<CpuLocation>,

    /// Tightest ancestor CPU-time quota, or none when every hierarchy is unlimited.
    pub quota: Option<CpuQuota>,

    /// Network and InfiniBand devices, with available NUMA locality.
    pub nics: Vec<NicLocality>,
}

/// One lane and its NUMA-local shared helper execution placement.
pub struct Placement {
    /// CPU that owns the lane's local graph.
    pub lane: CpuLocation,

    /// CPU shared by the lane's helper service.
    pub helper: CpuLocation,

    /// Optional caller-filtered local device hint.
    pub nic: Option<NicLocality>,
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
    nics.retain(|nic| nic.numa_node.is_some());
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
        // Prefer physical-core diversity among lanes even with SMT enabled.
        let mut cores = HashSet::new();
        let mut ordered = local
            .into_iter()
            .map(|cpu| (!cores.insert((cpu.package, cpu.core)), cpu))
            .collect::<Vec<_>>();
        ordered.sort_by_key(|(sibling, cpu)| (*sibling, cpu.cpu));
        let mut local = ordered.into_iter().map(|(_, cpu)| cpu).collect::<Vec<_>>();
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
    mut topology: EffectiveTopology,
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
    topology.cpus.sort_by_key(|cpu| cpu.cpu);
    if topology
        .cpus
        .windows(2)
        .any(|pair| pair[0].cpu == pair[1].cpu)
    {
        return Err(Error::InvalidConfiguration);
    }
    if !allow_smt {
        let mut cores = HashSet::new();
        topology
            .cpus
            .retain(|cpu| cores.insert((cpu.package, cpu.core)));
    }
    topology.nics.sort_by(|a, b| a.device.cmp(&b.device));
    topology.nics.retain(|nic| nic.numa_node.is_some());
    let mut nodes = BTreeMap::<_, Vec<_>>::new();
    for cpu in topology.cpus {
        nodes.entry(cpu.numa_node).or_default().push(cpu);
    }
    let mut nodes = nodes.into_iter().collect::<Vec<_>>();
    nodes.sort_by_key(|(node, _)| {
        (
            !topology.nics.iter().any(|nic| nic.numa_node == *node),
            *node,
        )
    });
    let (node, local) = nodes
        .into_iter()
        .next()
        .ok_or(Error::InvalidConfiguration)?;
    if node.is_none() || local.len() < total {
        return Err(Error::InvalidConfiguration);
    }
    let mut cores = HashSet::new();
    let mut ordered = local
        .into_iter()
        .map(|cpu| (!cores.insert((cpu.package, cpu.core)), cpu))
        .collect::<Vec<_>>();
    ordered.sort_by_key(|(sibling, cpu)| (*sibling, cpu.cpu));
    let local = ordered.into_iter().map(|(_, cpu)| cpu).collect::<Vec<_>>();
    let nic = topology.nics.into_iter().find(|nic| nic.numa_node == node);
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
                let topology = EffectiveTopology {
                    cpus: (0..cores)
                        .map(|cpu| CpuLocation {
                            cpu,
                            package: 0,
                            core: cpu,
                            numa_node: Some(0),
                        })
                        .collect(),
                    quota: None,
                    nics: vec![],
                };
                let plan = place(topology, usize::MAX, usize::MAX, smt).unwrap();
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
        let hardware = EffectiveTopology {
            cpus: (0..9)
                .map(|cpu| CpuLocation {
                    cpu,
                    package: 0,
                    core: cpu,
                    numa_node: Some(0),
                })
                .collect(),
            quota: None,
            nics: vec![],
        };
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
}

impl EffectiveTopology {
    /// Discover constraints from the calling thread's actual Linux namespace.
    /// Walk every visible ancestor: leaf cpu.max alone misses parent restrictions.
    pub fn discover() -> Result<Self> {
        let mut allowed = current_cpus()?;
        let online = parse_cpu_list(
            &fs::read_to_string("/sys/devices/system/cpu/online").map_err(|_| Error::Io)?,
        )?;
        allowed.retain(|cpu| online.contains(cpu));
        let mut quota = None;
        let memberships = fs::read_to_string("/proc/thread-self/cgroup").map_err(|_| Error::Io)?;
        let mounts = fs::read_to_string("/proc/self/mountinfo").map_err(|_| Error::Io)?;
        for (leaf, root, v2, cpu, cpuset) in cgroup_paths(&memberships, &mounts)? {
            // Unresolved membership must never be interpreted as unlimited.
            if !fs::metadata(&leaf).map_err(|_| Error::Io)?.is_dir() {
                return Err(Error::InvalidConfiguration);
            }
            for directory in leaf.ancestors().take_while(|path| path.starts_with(&root)) {
                if cpu {
                    let candidate = if v2 {
                        optional_text(&directory.join("cpu.max"))?
                            .map(|value| parse_v2_quota(&value))
                            .transpose()?
                            .flatten()
                    } else {
                        match (
                            optional_text(&directory.join("cpu.cfs_quota_us"))?,
                            optional_text(&directory.join("cpu.cfs_period_us"))?,
                        ) {
                            (Some(q), Some(p)) => parse_v1_quota(&q, &p)?,
                            (None, None) => None,
                            _ => return Err(Error::InvalidConfiguration),
                        }
                    };
                    tighten_quota(&mut quota, candidate);
                }
                if cpuset {
                    let names: &[&str] = if v2 {
                        &["cpuset.cpus.effective", "cpuset.cpus"]
                    } else {
                        &["cpuset.effective_cpus", "cpuset.cpus"]
                    };
                    for name in names {
                        if let Some(value) = optional_text(&directory.join(name))?
                            && !value.trim().is_empty()
                        {
                            let set = parse_cpu_list(&value)?;
                            allowed.retain(|cpu| set.contains(cpu));
                        }
                    }
                }
            }
        }
        if allowed.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        let cpus = allowed
            .into_iter()
            .map(|cpu| {
                let path = PathBuf::from(format!("/sys/devices/system/cpu/cpu{cpu}"));
                Ok(CpuLocation {
                    cpu,
                    package: read_number(&path.join("topology/physical_package_id"))?,
                    core: read_number(&path.join("topology/core_id"))?,
                    numa_node: fs::read_dir(&path)
                        .map_err(|_| Error::Io)?
                        .filter_map(|entry| entry.ok())
                        .filter_map(|entry| {
                            entry
                                .file_name()
                                .to_str()?
                                .strip_prefix("node")?
                                .parse()
                                .ok()
                        })
                        .min(),
                })
            })
            .collect::<Result<Vec<_>>>()?;
        let mut nics = Vec::new();
        for directory in ["/sys/class/net", "/sys/class/infiniband"] {
            nics.extend(discover_nics(Path::new(directory))?);
        }
        Ok(Self { cpus, quota, nics })
    }
}

fn discover_nics(directory: &Path) -> Result<Vec<NicLocality>> {
    let entries = match fs::read_dir(directory) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(_) => return Err(Error::Io),
    };
    let mut nics = Vec::new();
    for entry in entries {
        let entry = entry.map_err(|_| Error::Io)?;
        // Sysfs class directories also contain controls such as bonding_masters.
        // Follow device symlinks, but do not treat regular files as devices.
        if !fs::metadata(entry.path()).map_err(|_| Error::Io)?.is_dir() {
            continue;
        }
        let numa_node = optional_text(&entry.path().join("device/numa_node"))?
            .and_then(|value| value.trim().parse::<usize>().ok());
        nics.push(NicLocality {
            device: entry.file_name().to_string_lossy().into_owned(),
            numa_node,
        });
    }
    Ok(nics)
}

/// Read an optional kernel attribute, distinguishing absence from I/O failure.
fn optional_text(path: &Path) -> Result<Option<String>> {
    match fs::read_to_string(path) {
        Ok(value) => Ok(Some(value)),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(_) => Err(Error::Io),
    }
}

/// Parse a required nonnegative sysfs topology attribute.
fn read_number(path: &Path) -> Result<usize> {
    fs::read_to_string(path)
        .map_err(|_| Error::Io)?
        .trim()
        .parse()
        .map_err(|_| Error::InvalidConfiguration)
}

/// Parse Linux's comma-separated CPU ranges, rejecting reversal and huge indices.
fn parse_cpu_list(value: &str) -> Result<BTreeSet<usize>> {
    let mut cpus = BTreeSet::new();
    if value.trim().is_empty() {
        return Ok(cpus);
    }
    for part in value.trim().split(',') {
        let (start, end) = part.split_once('-').unwrap_or((part, part));
        let start = start
            .parse::<usize>()
            .map_err(|_| Error::InvalidConfiguration)?;
        let end = end
            .parse::<usize>()
            .map_err(|_| Error::InvalidConfiguration)?;
        if start > end || end > 1_048_575 {
            return Err(Error::InvalidConfiguration);
        }
        cpus.extend(start..=end);
    }
    Ok(cpus)
}

/// Parse exactly two cpu.max fields without allocating an intermediate collection.
fn parse_v2_quota(value: &str) -> Result<Option<CpuQuota>> {
    let mut fields = value.split_whitespace();
    let (Some(quota), Some(period), None) = (fields.next(), fields.next(), fields.next()) else {
        return Err(Error::InvalidConfiguration);
    };
    parse_v1_quota(if quota == "max" { "-1" } else { quota }, period)
}

/// Validate a quota and positive period, including the v1 unlimited sentinel.
fn parse_v1_quota(quota: &str, period: &str) -> Result<Option<CpuQuota>> {
    let period = period
        .trim()
        .parse::<NonZeroU64>()
        .map_err(|_| Error::InvalidConfiguration)?;
    if quota.trim() == "-1" {
        return Ok(None);
    }
    Ok(Some(CpuQuota {
        quota: quota
            .trim()
            .parse()
            .map_err(|_| Error::InvalidConfiguration)?,
        period,
    }))
}

/// Retain the smaller exact ratio using wide products to avoid rounding or overflow.
fn tighten_quota(current: &mut Option<CpuQuota>, candidate: Option<CpuQuota>) {
    if let Some(candidate) = candidate
        && current.is_none_or(|old| {
            u128::from(candidate.quota.get()) * u128::from(old.period.get())
                < u128::from(old.quota.get()) * u128::from(candidate.period.get())
        })
    {
        *current = Some(candidate);
    }
}

/// Membership leaf, mount boundary, unified flag, and CPU/cpuset controller flags.
type CgroupPath = (PathBuf, PathBuf, bool, bool, bool);

/// Resolve only covering cgroup mounts and reject hidden applicable controllers.
fn cgroup_paths(memberships: &str, mounts: &str) -> Result<Vec<CgroupPath>> {
    let mut paths = Vec::new();
    let memberships = memberships
        .lines()
        .map(|line| {
            let fields = line.splitn(3, ':').collect::<Vec<_>>();
            if fields.len() != 3
                || !Path::new(fields[2]).is_absolute()
                || Path::new(fields[2])
                    .components()
                    .any(|part| matches!(part, std::path::Component::ParentDir))
            {
                return Err(Error::InvalidConfiguration);
            }
            Ok(fields)
        })
        .collect::<Result<Vec<_>>>()?;
    let mut covered = HashSet::new();
    for line in mounts.lines() {
        let Some((before, after)) = line.split_once(" - ") else {
            continue;
        };
        let before = before.split_whitespace().collect::<Vec<_>>();
        let after = after.split_whitespace().collect::<Vec<_>>();
        if before.len() < 5 || after.len() < 3 {
            continue;
        }
        let v2 = after[0] == "cgroup2";
        if !v2 && after[0] != "cgroup" {
            continue;
        }
        let controllers = after[2].split(',').collect::<HashSet<_>>();
        let cpu = v2 || controllers.contains("cpu");
        let cpuset = v2 || controllers.contains("cpuset");
        if !cpu && !cpuset {
            continue;
        }
        for (index, fields) in memberships.iter().enumerate() {
            let matches = if v2 {
                fields[1].is_empty()
            } else {
                fields[1]
                    .split(',')
                    .any(|controller| controllers.contains(controller))
            };
            if !matches {
                continue;
            }
            let root = PathBuf::from(unescape_mount(before[4]));
            let mount_root = PathBuf::from(unescape_mount(before[3]));
            let membership = PathBuf::from(fields[2]);
            if !root.is_absolute() || !mount_root.is_absolute() {
                return Err(Error::InvalidConfiguration);
            }
            // Both proc files use the calling namespace. A noncovering subtree
            // mount must not act as a fallback root for this membership.
            let Ok(relative) = membership.strip_prefix(&mount_root) else {
                continue;
            };
            if v2 {
                covered.insert((index, ""));
            }
            if cpu {
                covered.insert((index, "cpu"));
            }
            if cpuset {
                covered.insert((index, "cpuset"));
            }
            paths.push((root.join(relative), root, v2, cpu, cpuset));
        }
    }
    for (index, fields) in memberships.iter().enumerate() {
        for controller in fields[1].split(',') {
            if matches!(controller, "" | "cpu" | "cpuset")
                && !covered.contains(&(index, controller))
            {
                // Hidden applicable hierarchies are unknown, not unlimited.
                return Err(Error::InvalidConfiguration);
            }
        }
    }
    Ok(paths)
}

/// Decode proc mountinfo escapes once, leaving escaped backslashes uninterpreted.
fn unescape_mount(value: &str) -> String {
    value
        .replace("\\040", " ")
        .replace("\\011", "\t")
        .replace("\\012", "\n")
        .replace("\\134", "\\")
}

/// Linux affinity and cgroup parsing boundary and state-space regressions.
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nic_discovery_skips_controls_and_follows_device_links() {
        let root = std::env::temp_dir().join(format!("racer-nic-discovery-{}", std::process::id()));
        fs::create_dir(&root).unwrap();
        let class = root.join("class");
        fs::create_dir(&class).unwrap();
        fs::create_dir_all(root.join("nic/device")).unwrap();
        fs::write(root.join("nic/device/numa_node"), "1\n").unwrap();
        std::os::unix::fs::symlink(root.join("nic"), class.join("mlx5_00")).unwrap();
        fs::write(class.join("bonding_masters"), "bond0\n").unwrap();
        fs::create_dir(class.join("virtual")).unwrap();
        let mut nics = discover_nics(&class).unwrap();
        nics.sort_by(|a, b| a.device.cmp(&b.device));
        assert_eq!(nics.len(), 2);
        assert_eq!(nics[0].device, "mlx5_00");
        assert_eq!(nics[0].numa_node, Some(1));
        assert_eq!(nics[1].device, "virtual");
        assert_eq!(nics[1].numa_node, None);
        assert!(discover_nics(&root.join("absent")).unwrap().is_empty());
        assert!(matches!(
            discover_nics(&class.join("bonding_masters")),
            Err(Error::Io)
        ));
        fs::remove_dir_all(root).unwrap();
    }

    /// Ignore noncovering mounts and reject hidden applicable CPU controllers.
    #[test]
    fn noncovering_mounts_are_skipped_and_hidden_hierarchies_fail_closed() {
        let unrelated = "1 0 0:1 /other /unrelated rw - cgroup2 cgroup rw\n";
        let covering = "2 0 0:1 /tenant /visible\\040group rw - cgroup2 cgroup rw\n";
        let paths = cgroup_paths("0::/tenant/leaf", &format!("{unrelated}{covering}")).unwrap();
        assert_eq!(paths.len(), 1);
        assert_eq!(paths[0].0, PathBuf::from("/visible group/leaf"));
        assert!(cgroup_paths("0::/tenant/leaf", unrelated).is_err());
        assert!(cgroup_paths("0::/tenant/leaf", "").is_err());
        assert!(cgroup_paths("2:cpu:/tenant/leaf", "").is_err());
        assert!(cgroup_paths("2:cpuset:/tenant/leaf", "").is_err());
        assert!(
            cgroup_paths("2:memory:/tenant/leaf", "")
                .unwrap()
                .is_empty()
        );
        let paths = cgroup_paths("0::/", "1 0 0:1 / /visible rw - cgroup2 cgroup rw").unwrap();
        assert_eq!(paths[0].0, PathBuf::from("/visible"));
    }

    /// Resolve all applicable hierarchies and retain the tightest ancestor quota.
    #[test]
    fn cgroup_mounts_and_ancestor_quotas_are_resolved_exactly() {
        let mounts = "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n\
            2 0 0:2 /tenant /cpu rw - cgroup cgroup rw,cpu,cpuacct\n\
            3 0 0:3 / /sets rw - cgroup cgroup rw,cpuset\n";
        let paths = cgroup_paths(
            "0::/tenant/leaf\n2:cpu,cpuacct:/tenant/leaf\n3:cpuset:/tenant/leaf\n",
            mounts,
        )
        .unwrap();
        assert_eq!(
            paths,
            vec![
                (
                    "/sys/fs/cgroup/tenant/leaf".into(),
                    "/sys/fs/cgroup".into(),
                    true,
                    true,
                    true
                ),
                ("/cpu/leaf".into(), "/cpu".into(), false, true, false),
                (
                    "/sets/tenant/leaf".into(),
                    "/sets".into(),
                    false,
                    false,
                    true
                ),
            ]
        );
        let mut quota = parse_v2_quota("400000 100000").unwrap();
        tighten_quota(&mut quota, parse_v1_quota("150000", "100000").unwrap());
        tighten_quota(&mut quota, parse_v2_quota("max 100000").unwrap());
        tighten_quota(&mut quota, parse_v2_quota("200000 100000").unwrap());
        assert_eq!(quota.unwrap().quota.get(), 150000);
        assert!(parse_v2_quota("0 100000").is_err());
        assert!(parse_v2_quota("max 0").is_err());
        assert!(parse_v1_quota("-2", "100000").is_err());
        assert!(cgroup_paths("0::/../escape", mounts).is_err());
    }

    /// Pin only the calling thread and restore its complete original affinity.
    #[test]
    fn actual_affinity_is_applied_on_the_calling_thread() {
        std::thread::spawn(|| {
            let allowed = current_cpus().unwrap();
            let cpu = *allowed.first().unwrap();
            pin_cpu(cpu).unwrap();
            assert_eq!(current_cpus().unwrap(), BTreeSet::from([cpu]));
            let discovered = EffectiveTopology::discover().unwrap();
            assert_eq!(discovered.cpus.len(), 1);
            assert_eq!(discovered.cpus[0].cpu, cpu);
            set_cpus(&allowed).unwrap();
            assert_eq!(current_cpus().unwrap(), allowed);
        })
        .join()
        .unwrap();
    }

    /// Accept bounded CPU ranges and reject malformed or reversed intervals.
    #[test]
    fn constrained_cpuset_ranges_are_validated() {
        for value in ["", " \n"] {
            assert_eq!(parse_cpu_list(value).unwrap(), BTreeSet::new());
        }
        assert_eq!(
            parse_cpu_list(" 3,1-3,2,0,1048575\n").unwrap(),
            BTreeSet::from([0, 1, 2, 3, 1_048_575])
        );
        assert_eq!(
            parse_cpu_list("1-3,8,10-11\n").unwrap(),
            BTreeSet::from([1, 2, 3, 8, 10, 11])
        );
        for value in [
            "4-1",
            "-1",
            "a",
            "1-2-3",
            "1048576",
            "999999999",
            "1,,2",
            "1,",
        ] {
            assert_eq!(
                parse_cpu_list(value),
                Err(Error::InvalidConfiguration),
                "{value}"
            );
        }
    }

    /// Validate both cgroup quota formats, including unlimited and overflow cases.
    #[test]
    fn quota_formats_validate_fields_and_unlimited_periods() {
        for (quota, period, expected) in [
            ("150000", "100000", Some((150000, 100000))),
            (" 1\n", " 2\n", Some((1, 2))),
            ("-1", "100000", None),
        ] {
            let v1 = parse_v1_quota(quota, period).unwrap();
            let v2 = parse_v2_quota(&format!(
                "{} {period}",
                if quota == "-1" { "max" } else { quota }
            ))
            .unwrap();
            let pair = |value: Option<CpuQuota>| value.map(|q| (q.quota.get(), q.period.get()));
            assert_eq!(pair(v1), expected);
            assert_eq!(pair(v2), expected);
        }
        for value in [
            "",
            "max",
            "1 2 3",
            "0 1",
            "-2 1",
            "1 0",
            "max 0",
            "max nope",
            "1 -1",
            "nope 1",
            "18446744073709551616 1",
            "1 18446744073709551616",
        ] {
            assert!(
                matches!(parse_v2_quota(value), Err(Error::InvalidConfiguration)),
                "{value}"
            );
        }
    }

    /// Compare quota ratios exactly even near the integer representation limit.
    #[test]
    fn quotas_compare_ratios_without_rounding_or_overflow() {
        let mut quota = None;
        for (candidate, expected) in [
            ("max 10", None),
            ("3 2", Some((3, 2))),
            ("4 3", Some((4, 3))),
            ("8 6", Some((4, 3))),
            ("max 10", Some((4, 3))),
            (
                "18446744073709551615 18446744073709551614",
                Some((u64::MAX, u64::MAX - 1)),
            ),
            (
                "18446744073709551614 18446744073709551615",
                Some((u64::MAX - 1, u64::MAX)),
            ),
            ("2 1", Some((u64::MAX - 1, u64::MAX))),
        ] {
            tighten_quota(&mut quota, parse_v2_quota(candidate).unwrap());
            assert_eq!(
                quota.map(|q| (q.quota.get(), q.period.get())),
                expected,
                "{candidate}"
            );
        }
    }

    /// Resolve namespace-relative membership and decode mount escapes only once.
    #[test]
    fn cgroup_namespace_paths_and_mount_escapes_are_resolved() {
        let mounts = "malformed\n1 0 0:1 / /ignored rw - tmpfs tmpfs rw\n\
            2 0 0:2 / /memory rw - cgroup cgroup rw,memory\n\
            3 0 0:3 /host\\040root /group\\040mount rw - cgroup2 cgroup rw\n";
        assert_eq!(
            cgroup_paths("0::/host root/leaf", mounts).unwrap(),
            vec![(
                "/group mount/leaf".into(),
                "/group mount".into(),
                true,
                true,
                true,
            )]
        );
        for membership in [
            "0::/leaf",
            "0::/",
            "malformed",
            "0::relative",
            "0::/leaf/../escape",
        ] {
            assert_eq!(
                cgroup_paths(membership, mounts),
                Err(Error::InvalidConfiguration)
            );
        }
        assert_eq!(unescape_mount(r"a\040b\011c\012d\134040"), "a b\tc\nd\\040");
    }

    /// Invalid masks must fail without changing the calling thread's affinity.
    #[test]
    fn invalid_affinity_masks_return_generic_errors_without_changing_affinity() {
        let allowed = current_cpus().unwrap();
        assert_eq!(set_cpus(&BTreeSet::new()), Err(Error::InvalidConfiguration));
        assert_eq!(pin_cpu(1_048_576), Err(Error::InvalidConfiguration));
        assert_eq!(current_cpus().unwrap(), allowed);
    }
}
