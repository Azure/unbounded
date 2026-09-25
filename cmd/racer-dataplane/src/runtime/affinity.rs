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
    error::{Error, Result},
    model::identity::WorkerId,
    topology::rails::RailMapping,
};
use std::{
    collections::{BTreeSet, HashSet},
    fs,
    num::NonZeroU64,
    path::{Path, PathBuf},
};

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
    /// Whole-process budget. `run` uses its caller as the first reactor. Owned
    /// startup must reserve one additional slot for the calling thread.
    pub max_threads: usize,
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
    pub fn discover(config: &Config) -> Result<Self> {
        Self::from_topology(config, EffectiveTopology::discover()?, &[])
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
        Self::place(config.max_threads, topology, rails)
    }
    pub fn pin_current_thread(&self, worker: WorkerId, role: Role) -> Result<()> {
        let pair = self
            .pairs
            .iter()
            .find(|pair| pair.worker == worker)
            .ok_or(Error::InvalidConfiguration)?;
        pin_cpu(pair.location(role).cpu)
    }

    fn place(
        max_threads: usize,
        topology: EffectiveTopology,
        rails: &[RailMapping],
    ) -> Result<Self> {
        let count = pair_count(max_threads, &topology)?;
        let mut cpus = topology.cpus;
        cpus.sort_by_key(|cpu| cpu.cpu);
        if cpus.windows(2).any(|pair| pair[0].cpu == pair[1].cpu) {
            return Err(Error::InvalidConfiguration);
        }
        let mut nics = topology.nics;
        nics.sort_by(|a, b| a.device.cmp(&b.device));
        // RailMapping carries a fabric label, not a local device identifier. NUMA
        // compatibility is a placement hint only, never proof of RDMA eligibility.
        nics.retain(|nic| {
            nic.numa_node.is_some()
                && (rails.is_empty() || rails.iter().any(|rail| rail.numa_node == nic.numa_node))
        });
        let mut used = HashSet::new();
        let mut pairs = Vec::with_capacity(count);
        for index in 0..count {
            let nic = nics.get(index % nics.len().max(1)).cloned();
            let node = nic.as_ref().and_then(|nic| nic.numa_node);
            let mut choose = || {
                let cpu = cpus
                    .iter()
                    .min_by_key(|cpu| {
                        (
                            used.contains(&(cpu.package, cpu.core)),
                            node.is_some() && cpu.numa_node != node,
                            cpu.cpu,
                        )
                    })
                    .expect("nonempty topology")
                    .clone();
                used.insert((cpu.package, cpu.core));
                cpu
            };
            let io = choose();
            let crypto = choose();
            let nic = nic
                .filter(|nic| io.numa_node == nic.numa_node && crypto.numa_node == nic.numa_node);
            pairs.push(WorkerPair {
                worker: WorkerId(index as u16),
                io,
                crypto,
                nic,
            });
        }
        Ok(Self { pairs, max_threads })
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
            // Never interpret an unresolved membership as an unlimited quota.
            if !fs::metadata(&leaf).map_err(|_| Error::Io)?.is_dir() {
                return Err(Error::InvalidConfiguration);
            }
            for directory in leaf.ancestors() {
                if !directory.starts_with(&root) {
                    break;
                }
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
                        if let Some(value) = optional_text(&directory.join(name))? {
                            if !value.trim().is_empty() {
                                let set = parse_cpu_list(&value)?;
                                allowed.retain(|cpu| set.contains(cpu));
                            }
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
            let entries = match fs::read_dir(directory) {
                Ok(entries) => entries,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
                Err(_) => return Err(Error::Io),
            };
            for entry in entries {
                let entry = entry.map_err(|_| Error::Io)?;
                let numa_node = optional_text(&entry.path().join("device/numa_node"))?
                    .and_then(|value| value.trim().parse::<usize>().ok());
                nics.push(NicLocality {
                    device: entry.file_name().to_string_lossy().into_owned(),
                    numa_node,
                });
            }
        }
        Ok(Self { cpus, quota, nics })
    }
}

fn optional_text(path: &Path) -> Result<Option<String>> {
    match fs::read_to_string(path) {
        Ok(value) => Ok(Some(value)),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(_) => Err(Error::Io),
    }
}

fn read_number(path: &Path) -> Result<usize> {
    fs::read_to_string(path)
        .map_err(|_| Error::Io)?
        .trim()
        .parse()
        .map_err(|_| Error::InvalidConfiguration)
}

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

fn parse_v2_quota(value: &str) -> Result<Option<CpuQuota>> {
    let fields = value.split_whitespace().collect::<Vec<_>>();
    if fields.len() != 2 {
        return Err(Error::InvalidConfiguration);
    }
    parse_v1_quota(if fields[0] == "max" { "-1" } else { fields[0] }, fields[1])
}

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

fn tighten_quota(current: &mut Option<CpuQuota>, candidate: Option<CpuQuota>) {
    if let Some(candidate) = candidate {
        if current.is_none_or(|old| {
            u128::from(candidate.quota.get()) * u128::from(old.period.get())
                < u128::from(old.quota.get()) * u128::from(candidate.period.get())
        }) {
            *current = Some(candidate);
        }
    }
}

// (membership leaf, mount boundary, unified, CPU controller, cpuset controller).
type CgroupPath = (PathBuf, PathBuf, bool, bool, bool);

fn cgroup_paths(memberships: &str, mounts: &str) -> Result<Vec<CgroupPath>> {
    let mut paths = Vec::new();
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
        for membership in memberships.lines() {
            let fields = membership.splitn(3, ':').collect::<Vec<_>>();
            if fields.len() != 3 {
                return Err(Error::InvalidConfiguration);
            }
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
            // A cgroup namespace reports paths relative to its own root. A bind
            // mount may instead expose a subtree of the host hierarchy.
            let relative = membership
                .strip_prefix(&mount_root)
                .or_else(|_| membership.strip_prefix("/"))
                .map_err(|_| Error::InvalidConfiguration)?;
            if relative
                .components()
                .any(|part| matches!(part, std::path::Component::ParentDir))
            {
                return Err(Error::InvalidConfiguration);
            }
            paths.push((root.join(relative), root, v2, cpu, cpuset));
        }
    }
    Ok(paths)
}

fn unescape_mount(value: &str) -> String {
    value
        .replace("\\040", " ")
        .replace("\\011", "\t")
        .replace("\\012", "\n")
        .replace("\\134", "\\")
}

pub(crate) fn current_cpus() -> Result<BTreeSet<usize>> {
    let mut mask = vec![0usize; 16];
    loop {
        // SAFETY: the kernel receives the size of the writable, word-aligned mask.
        let result = unsafe {
            libc::sched_getaffinity(
                0,
                std::mem::size_of_val(mask.as_slice()),
                mask.as_mut_ptr().cast(),
            )
        };
        if result == 0 {
            break;
        }
        if std::io::Error::last_os_error().raw_os_error() != Some(libc::EINVAL)
            || mask.len() >= 16384
        {
            return Err(Error::Io);
        }
        mask.resize(mask.len() * 2, 0);
    }
    Ok(mask
        .iter()
        .enumerate()
        .flat_map(|(word, bits)| {
            (0..usize::BITS as usize)
                .filter(move |bit| bits & (1usize << bit) != 0)
                .map(move |bit| word * usize::BITS as usize + bit)
        })
        .collect())
}

pub(crate) fn set_cpus(cpus: &BTreeSet<usize>) -> Result<()> {
    let max = *cpus.last().ok_or(Error::InvalidConfiguration)?;
    if max > 1_048_575 {
        return Err(Error::InvalidConfiguration);
    }
    let mut mask = vec![0usize; (max / usize::BITS as usize + 1).max(16)];
    for cpu in cpus {
        mask[cpu / usize::BITS as usize] |= 1usize << (cpu % usize::BITS as usize);
    }
    // SAFETY: the kernel only reads the sized, word-aligned affinity mask.
    if unsafe {
        libc::sched_setaffinity(
            0,
            std::mem::size_of_val(mask.as_slice()),
            mask.as_ptr().cast(),
        )
    } != 0
    {
        return Err(Error::Io);
    }
    Ok(())
}

pub(crate) fn pin_cpu(cpu: usize) -> Result<()> {
    set_cpus(&BTreeSet::from([cpu]))
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
        assert_eq!(paths.len(), 3);
        assert_eq!(paths[0].0, PathBuf::from("/sys/fs/cgroup/tenant/leaf"));
        assert_eq!(paths[1].0, PathBuf::from("/cpu/leaf"));
        assert_eq!(paths[2].0, PathBuf::from("/sets/tenant/leaf"));
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
        assert_eq!(plan.pairs[0].crypto.cpu, 5);
        assert_eq!(plan.pairs[1].io.cpu, 6);
        assert_eq!(plan.pairs[1].crypto.cpu, 7);
        let mut duplicate = topology(2, None);
        duplicate.cpus[1].cpu = 0;
        assert!(AffinityPlan::place(4, duplicate, &[]).is_err());
        let mut mismatch = topology(2, None);
        mismatch.nics.push(NicLocality {
            device: "eth0".into(),
            numa_node: Some(1),
        });
        assert!(AffinityPlan::place(4, mismatch, &[]).unwrap().pairs[0]
            .nic
            .is_none());
    }

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
        })
        .join()
        .unwrap();
    }

    #[test]
    fn constrained_cpuset_ranges_are_validated() {
        assert_eq!(
            parse_cpu_list("1-3,8,10-11\n").unwrap(),
            BTreeSet::from([1, 2, 3, 8, 10, 11])
        );
        for value in ["4-1", "-1", "a", "1-2-3", "999999999", "1,,2", "1,"] {
            assert!(parse_cpu_list(value).is_err(), "{value}");
        }
    }
}
