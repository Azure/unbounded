//! Linux CPU, cgroup, NUMA, and NIC discovery and calling-thread affinity.
//! Placement policy belongs to the caller, not the runtime.

use crate::{Error, Result};
use std::{
    collections::{BTreeSet, HashSet},
    fs,
    num::NonZeroU64,
    path::{Path, PathBuf},
};

#[derive(Clone, Debug)]
pub struct CpuLocation {
    pub cpu: usize,
    /// Physical core IDs are package-local; SMT siblings share this pair.
    pub package: usize,
    pub core: usize,
    pub numa_node: Option<usize>,
}

/// Tightest applicable effective cgroup CPU-time quota. None means unlimited.
#[derive(Clone, Copy, Debug)]
pub struct CpuQuota {
    pub quota: NonZeroU64,
    pub period: NonZeroU64,
}

/// Discovered local hardware, without application placement policy.
#[derive(Clone, Debug)]
pub struct NicLocality {
    pub device: String,
    pub numa_node: Option<usize>,
}

#[derive(Clone)]
pub struct EffectiveTopology {
    /// Online CPUs intersected with process affinity and effective cpuset.
    pub cpus: Vec<CpuLocation>,
    pub quota: Option<CpuQuota>,
    pub nics: Vec<NicLocality>,
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
    if let Some(candidate) = candidate
        && current.is_none_or(|old| {
            u128::from(candidate.quota.get()) * u128::from(old.period.get())
                < u128::from(old.quota.get()) * u128::from(candidate.period.get())
        })
    {
        *current = Some(candidate);
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

/// Read the calling thread's allowed logical CPU IDs.
pub fn current_cpus() -> Result<BTreeSet<usize>> {
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

/// Set the calling thread's allowed logical CPU IDs.
pub fn set_cpus(cpus: &BTreeSet<usize>) -> Result<()> {
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

/// Pin the calling thread to one logical CPU.
pub fn pin_cpu(cpu: usize) -> Result<()> {
    set_cpus(&BTreeSet::from([cpu]))
}

#[cfg(test)]
mod tests {
    use super::*;

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

    #[test]
    fn cgroup_namespace_paths_and_mount_escapes_are_resolved() {
        let mounts = "malformed\n1 0 0:1 / /ignored rw - tmpfs tmpfs rw\n\
            2 0 0:2 / /memory rw - cgroup cgroup rw,memory\n\
            3 0 0:3 /host\\040root /group\\040mount rw - cgroup2 cgroup rw\n";
        for (membership, leaf) in [
            ("/host root/leaf", "/group mount/leaf"),
            ("/leaf", "/group mount/leaf"),
            ("/", "/group mount/"),
        ] {
            assert_eq!(
                cgroup_paths(&format!("0::{membership}"), mounts).unwrap(),
                vec![(leaf.into(), "/group mount".into(), true, true, true)]
            );
        }
        for membership in ["malformed", "0::relative", "0::/leaf/../escape"] {
            assert_eq!(
                cgroup_paths(membership, mounts),
                Err(Error::InvalidConfiguration)
            );
        }
        assert_eq!(unescape_mount(r"a\040b\011c\012d\134040"), "a b\tc\nd\\040");
    }

    #[test]
    fn invalid_affinity_masks_return_generic_errors_without_changing_affinity() {
        let allowed = current_cpus().unwrap();
        assert_eq!(set_cpus(&BTreeSet::new()), Err(Error::InvalidConfiguration));
        assert_eq!(pin_cpu(1_048_576), Err(Error::InvalidConfiguration));
        assert_eq!(current_cpus().unwrap(), allowed);
    }
}
