// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Low-level sysfs parsing helpers used by topology discovery. All
//! functions are path-parameterized so tests can stage trees under
//! `target/tmp-topology-tests/...` instead of touching `/sys`.

use std::collections::BTreeSet;
use std::fs;
use std::path::Path;

/// Parse a Linux cpulist like `"0-71,144-215"` into a sorted, deduped
/// Vec. Returns empty for empty input or unparseable fragments.
pub(super) fn parse_cpulist(s: &str) -> Vec<u32> {
    let mut set = BTreeSet::new();
    for group in s.trim().split(',') {
        let group = group.trim();
        if group.is_empty() {
            continue;
        }
        let mut parts = group.splitn(2, '-');
        let lo = match parts.next().and_then(|s| s.trim().parse::<u32>().ok()) {
            Some(v) => v,
            None => continue,
        };
        let hi = match parts.next() {
            Some(s) => s.trim().parse::<u32>().ok().unwrap_or(lo),
            None => lo,
        };
        for cpu in lo..=hi {
            set.insert(cpu);
        }
    }
    set.into_iter().collect()
}

pub(super) fn read_cpulist_file(path: &Path) -> Option<Vec<u32>> {
    let raw = fs::read_to_string(path).ok()?;
    Some(parse_cpulist(&raw))
}

/// NUMA node id of a given CPU, derived from the
/// `devices/system/cpu/cpu{N}/node{M}/` symlink convention. Returns
/// `None` when sysfs has no node mapping for the CPU.
pub(super) fn numa_of_cpu(sys_root: &Path, cpu: u32) -> Option<u16> {
    let dir = sys_root.join(format!("devices/system/cpu/cpu{cpu}"));
    for entry in fs::read_dir(&dir).ok()?.flatten() {
        let name = entry.file_name();
        if let Some(rest) = name.to_string_lossy().strip_prefix("node") {
            if let Ok(n) = rest.parse::<u16>() {
                return Some(n);
            }
        }
    }
    None
}

/// SMT siblings of `cpu` from
/// `cpu{N}/topology/thread_siblings_list`. Falls back to `[cpu]`
/// when the file is missing or empty so callers can treat the result
/// as canonical.
pub(super) fn thread_siblings(sys_root: &Path, cpu: u32) -> Vec<u32> {
    let path = sys_root.join(format!(
        "devices/system/cpu/cpu{cpu}/topology/thread_siblings_list"
    ));
    match read_cpulist_file(&path) {
        Some(v) if !v.is_empty() => v,
        _ => vec![cpu],
    }
}

pub(super) fn read_online_cpus(sys_root: &Path) -> Vec<u32> {
    read_cpulist_file(&sys_root.join("devices/system/cpu/online")).unwrap_or_default()
}

pub(super) fn read_isolated_cpus(sys_root: &Path) -> BTreeSet<u32> {
    read_cpulist_file(&sys_root.join("devices/system/cpu/isolated"))
        .unwrap_or_default()
        .into_iter()
        .collect()
}

/// Extract `PCI_SLOT_NAME=...` from a uevent file. Returns `None`
/// when the file is missing or contains no such line.
pub(super) fn read_pci_bdf(uevent_path: &Path) -> Option<String> {
    let s = fs::read_to_string(uevent_path).ok()?;
    for line in s.lines() {
        if let Some(v) = line.strip_prefix("PCI_SLOT_NAME=") {
            return Some(v.trim().to_string());
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU64, Ordering};

    static NEXT_ID: AtomicU64 = AtomicU64::new(0);

    fn staging_root() -> PathBuf {
        let pid = std::process::id();
        let counter = NEXT_ID.fetch_add(1, Ordering::Relaxed);
        let root = std::env::current_dir()
            .unwrap()
            .join("target")
            .join("tmp-topology-tests")
            .join(format!("sysfs-{pid}-{counter}"));
        let _ = fs::remove_dir_all(&root);
        fs::create_dir_all(&root).unwrap();
        root
    }

    #[test]
    fn parse_cpulist_empty() {
        assert_eq!(parse_cpulist(""), Vec::<u32>::new());
        assert_eq!(parse_cpulist("\n"), Vec::<u32>::new());
        assert_eq!(parse_cpulist("  "), Vec::<u32>::new());
    }

    #[test]
    fn parse_cpulist_single() {
        assert_eq!(parse_cpulist("5"), vec![5]);
        assert_eq!(parse_cpulist("  5  "), vec![5]);
    }

    #[test]
    fn parse_cpulist_range() {
        assert_eq!(parse_cpulist("0-3"), vec![0, 1, 2, 3]);
    }

    #[test]
    fn parse_cpulist_mixed() {
        assert_eq!(parse_cpulist("0-1,4-5"), vec![0, 1, 4, 5]);
        // Out-of-order groups end up sorted and deduped.
        assert_eq!(parse_cpulist("5,0-1,5"), vec![0, 1, 5]);
    }

    #[test]
    fn read_pci_bdf_extracts_value() {
        let root = staging_root();
        let path = root.join("uevent");
        fs::write(
            &path,
            "DRIVER=mlx5_core\nPCI_SLOT_NAME=0000:af:00.0\nMODALIAS=pci:foo\n",
        )
        .unwrap();
        assert_eq!(read_pci_bdf(&path).as_deref(), Some("0000:af:00.0"),);
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn read_pci_bdf_missing_line_returns_none() {
        let root = staging_root();
        let path = root.join("uevent");
        fs::write(&path, "DRIVER=mlx5_core\n").unwrap();
        assert!(read_pci_bdf(&path).is_none());
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn read_pci_bdf_missing_file_returns_none() {
        let root = staging_root();
        assert!(read_pci_bdf(&root.join("nope")).is_none());
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn numa_of_cpu_and_thread_siblings_round_trip() {
        let root = staging_root();
        // cpu5 -> node1, siblings 5,21
        fs::create_dir_all(root.join("devices/system/cpu/cpu5/node1")).unwrap();
        fs::create_dir_all(root.join("devices/system/cpu/cpu5/topology")).unwrap();
        fs::write(
            root.join("devices/system/cpu/cpu5/topology/thread_siblings_list"),
            "5,21\n",
        )
        .unwrap();
        assert_eq!(numa_of_cpu(&root, 5), Some(1));
        assert_eq!(thread_siblings(&root, 5), vec![5, 21]);

        // cpu6 has neither -> fallbacks.
        fs::create_dir_all(root.join("devices/system/cpu/cpu6")).unwrap();
        assert_eq!(numa_of_cpu(&root, 6), None);
        assert_eq!(thread_siblings(&root, 6), vec![6]);
        let _ = fs::remove_dir_all(&root);
    }
}
