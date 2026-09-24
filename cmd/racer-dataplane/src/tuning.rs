// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Startup-only co-location budgets. Overrides remain explicit; they are never
//! silently reduced. These are allocation/parallelism bounds, not RSS or CPU limits.
use std::{
    fs, io,
    num::NonZeroUsize,
    path::{Path, PathBuf},
};

const MIB: u64 = 1 << 20;
const BUFFER: u64 = crate::buffers::BUFFER_SIZE as u64;
const LOCK_MARGIN: u64 = 64 * MIB;

#[derive(Clone, Copy, Debug)]
pub struct Limits {
    pub cpu_quota: Option<usize>,
    pub available_memory: u64,
    pub memlock: u64,
}

impl Limits {
    pub fn discover() -> io::Result<Self> {
        let info = fs::read_to_string("/proc/meminfo")?;
        let available = info
            .lines()
            .find_map(|line| {
                line.strip_prefix("MemAvailable:")?
                    .split_whitespace()
                    .next()?
                    .parse::<u64>()
                    .ok()
            })
            .ok_or_else(|| invalid("MemAvailable missing"))?
            .saturating_mul(1024);
        let groups = fs::read_to_string("/proc/self/cgroup")?;
        let mounts = fs::read_to_string("/proc/self/mountinfo")?;
        let mut limits = discover_cgroups(available, &groups, &mounts)?;
        let mut limit = std::mem::MaybeUninit::<libc::rlimit>::uninit();
        // SAFETY: writable rlimit output, initialized on success.
        if unsafe { libc::getrlimit(libc::RLIMIT_MEMLOCK, limit.as_mut_ptr()) } != 0 {
            return Err(io::Error::last_os_error());
        }
        limits.memlock = unsafe { limit.assume_init() }.rlim_cur;
        Ok(limits)
    }

    fn pool_budget(self) -> u64 {
        (self.available_memory / 8).min(4 << 30)
    }

    pub fn max_nodes(self) -> usize {
        (self.pool_budget() / (4 * BUFFER)) as usize
    }

    pub fn max_io(self, registration_copies: usize, control_bytes: u64) -> usize {
        if registration_copies == 0 {
            return 0;
        }
        (self
            .memlock
            .saturating_sub(LOCK_MARGIN.saturating_add(control_bytes))
            / (4 * BUFFER).saturating_mul(registration_copies as u64)) as usize
    }

    pub fn buffers(
        self,
        io_per_node: &[usize],
        explicit: Option<NonZeroUsize>,
        registration_copies: usize,
        control_bytes: u64,
    ) -> io::Result<NonZeroUsize> {
        let workers = io_per_node.iter().sum::<usize>() as u64;
        let nodes = io_per_node.len() as u64;
        if nodes == 0 || workers == 0 || registration_copies == 0 {
            return Err(invalid("empty resource plan"));
        }
        let memory_cap = self.pool_budget() / nodes / BUFFER;
        // Linux may charge the same shared mapping once for every ring/RNIC
        // registration. Budget all copies even when a kernel deduplicates pins.
        let lock_cap = self
            .memlock
            .saturating_sub(LOCK_MARGIN.saturating_add(control_bytes))
            / workers
            / registration_copies as u64
            / BUFFER;
        let cap = memory_cap.min(lock_cap).min(65536);
        let target = io_per_node
            .iter()
            .max()
            .unwrap()
            .saturating_mul(8)
            .clamp(8, 32);
        let count = explicit.map_or((target as u64).min(cap), |n| n.get() as u64);
        if count < 4 || count > cap {
            return Err(invalid(format!(
                "RACER_BUFFERS_PER_NODE requires 4..={cap} buffers for {workers} I/O workers across {nodes} NUMA nodes (available memory={}, memlock={}); requested {count}",
                self.available_memory, self.memlock
            )));
        }
        Ok(NonZeroUsize::new(count as usize).unwrap())
    }
}

fn invalid(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message.into())
}

// Resolve membership against mount roots (including cgroup namespaces and v1
// split/combined controllers), then inspect every visible ancestor limit.
fn hierarchy(
    groups: &str,
    mounts: &str,
    controller: &str,
) -> io::Result<Option<(PathBuf, PathBuf, bool)>> {
    for line in groups.lines() {
        let fields: Vec<_> = line.splitn(3, ':').collect();
        if fields.len() != 3 {
            return Err(invalid("invalid cgroup membership"));
        }
        let v2 = fields[0] == "0" && fields[1].is_empty();
        if v2
            && groups.lines().any(|line| {
                line.split(':')
                    .nth(1)
                    .is_some_and(|controllers| controllers.split(',').any(|c| c == controller))
            })
        {
            continue;
        }
        if !v2 && !fields[1].split(',').any(|c| c == controller) {
            continue;
        }
        for mount in mounts.lines() {
            let Some((left, right)) = mount.split_once(" - ") else {
                continue;
            };
            let left: Vec<_> = left.split_whitespace().collect();
            let right: Vec<_> = right.split_whitespace().collect();
            if left.len() < 5 || right.len() < 3 {
                continue;
            }
            if (v2 && right[0] != "cgroup2")
                || (!v2 && (right[0] != "cgroup" || !right[2].split(',').any(|c| c == controller)))
            {
                continue;
            }
            let mount_root = unescape(left[3]);
            let root = PathBuf::from(unescape(left[4]));
            let membership = Path::new(fields[2]);
            let relative = membership
                .strip_prefix(&mount_root)
                .or_else(|_| membership.strip_prefix("/"))
                .map_err(|_| invalid("invalid cgroup path"))?;
            // Namespace-relative membership can contain host '..' components.
            // Never traverse outside the visible mount; its root is authoritative.
            let path = if relative
                .components()
                .any(|c| !matches!(c, std::path::Component::Normal(_)))
            {
                root.clone()
            } else {
                let path = root.join(relative);
                if path.is_dir() { path } else { root.clone() }
            };
            return Ok(Some((root, path, v2)));
        }
    }
    Ok(None)
}

fn unescape(value: &str) -> String {
    value
        .replace("\\040", " ")
        .replace("\\011", "\t")
        .replace("\\012", "\n")
        .replace("\\134", "\\")
}

fn read_optional(path: &Path) -> io::Result<Option<String>> {
    match fs::read_to_string(path) {
        Ok(value) => Ok(Some(value)),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(e),
    }
}

fn number(value: &str) -> io::Result<u64> {
    value
        .trim()
        .parse()
        .map_err(|_| invalid("invalid cgroup counter"))
}

fn quota(value: &str) -> io::Result<Option<usize>> {
    let fields: Vec<_> = value.split_whitespace().collect();
    if fields.len() != 2 {
        return Err(invalid("invalid CPU quota"));
    }
    let period = number(fields[1])?;
    if period == 0 {
        return Err(invalid("zero CPU quota period"));
    }
    if fields[0] == "max" || fields[0] == "-1" {
        return Ok(None);
    }
    let quota = number(fields[0])?;
    if quota == 0 {
        return Err(invalid("zero CPU quota"));
    }
    Ok(Some(
        usize::try_from((quota / period).max(1)).unwrap_or(usize::MAX),
    ))
}

fn discover_cgroups(available: u64, groups: &str, mounts: &str) -> io::Result<Limits> {
    let mut limits = Limits {
        cpu_quota: None,
        available_memory: available,
        memlock: 0,
    };
    for controller in ["cpu", "memory"] {
        let Some((root, mut path, v2)) = hierarchy(groups, mounts, controller)? else {
            continue;
        };
        loop {
            if controller == "cpu" {
                let value = if v2 {
                    read_optional(&path.join("cpu.max"))?
                } else {
                    read_optional(&path.join("cpu.cfs_quota_us"))?
                        .map(|q| {
                            fs::read_to_string(path.join("cpu.cfs_period_us"))
                                .map(|p| format!("{} {}", q.trim(), p.trim()))
                        })
                        .transpose()?
                };
                if let Some(value) = value
                    && let Some(q) = quota(&value)?
                {
                    limits.cpu_quota = Some(limits.cpu_quota.map_or(q, |old| old.min(q)));
                }
            } else {
                let (max, current) = if v2 {
                    ("memory.max", "memory.current")
                } else {
                    ("memory.limit_in_bytes", "memory.usage_in_bytes")
                };
                if let Some(max) = read_optional(&path.join(max))?
                    && max.trim() != "max"
                {
                    let max = number(&max)?;
                    let used = number(&fs::read_to_string(path.join(current))?)?;
                    // No reclaim credit for startup payload allocation.
                    limits.available_memory = limits.available_memory.min(max.saturating_sub(used));
                }
            }
            if path == root || !path.pop() || !path.starts_with(&root) {
                break;
            }
        }
    }
    Ok(limits)
}

#[cfg(test)]
#[path = "../tests/execution/tuning.rs"]
mod tests;
