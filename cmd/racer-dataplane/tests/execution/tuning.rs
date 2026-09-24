// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0
use super::*;

struct TempDir(PathBuf);
impl TempDir {
    fn new(name: &str) -> Self {
        let path = std::env::temp_dir().join(format!("racer-tuning-{name}-{}", std::process::id()));
        fs::create_dir(&path).unwrap();
        Self(path)
    }
    fn path(&self) -> &Path {
        &self.0
    }
}
impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

#[test]
fn pools_bound_memory_and_each_registration_without_clamping_overrides() {
    let limits = Limits {
        cpu_quota: None,
        available_memory: 29 << 30,
        memlock: 8 << 30,
    };
    assert_eq!(limits.buffers(&[2], None, 1, 0).unwrap().get(), 16);
    assert_eq!(limits.buffers(&[4], None, 1, 0).unwrap().get(), 31);
    assert_eq!(limits.buffers(&[2, 2], None, 1, 0).unwrap().get(), 16);
    assert_eq!(
        limits
            .buffers(&[2], NonZeroUsize::new(24), 1, 0)
            .unwrap()
            .get(),
        24
    );
    assert!(limits.buffers(&[2], NonZeroUsize::new(64), 1, 0).is_err());
    assert_eq!(limits.buffers(&[4], None, 2, 256 << 20).unwrap().get(), 15);
    let low = Limits {
        available_memory: 2 << 30,
        memlock: 320 << 20,
        ..limits
    };
    assert_eq!(low.buffers(&[1], None, 1, 0).unwrap().get(), 4);
    assert!(low.buffers(&[2], None, 1, 0).is_err());
    assert!(low.buffers(&[1, 1], None, 1, 0).is_err());
    assert!(low.buffers(&[1], NonZeroUsize::new(5), 1, 0).is_err());
    assert!(low.buffers(&[], None, 1, 0).is_err());
    assert!(low.buffers(&[1], None, 0, 0).is_err());
}

#[test]
fn quotas_round_down_except_for_minimum_execution_and_reject_malformed_limits() {
    for (value, expected) in [
        ("max 100000", None),
        ("-1 100000", None),
        ("350000 100000", Some(3)),
        ("50000 100000", Some(1)),
    ] {
        assert_eq!(quota(value).unwrap(), expected);
    }
    for value in ["max", "1 0", "0 100", "bad 100", "1 2 3"] {
        assert!(quota(value).is_err());
    }
}

#[test]
fn cgroup_ancestors_and_namespaced_mounts_bound_cpu_and_headroom() {
    let dir = TempDir::new("v2");
    let root = dir.path();
    fs::create_dir(root.join("child")).unwrap();
    fs::write(root.join("cpu.max"), "250000 100000").unwrap();
    fs::write(root.join("memory.max"), "1000").unwrap();
    fs::write(root.join("memory.current"), "800").unwrap();
    fs::write(root.join("child/cpu.max"), "max 100000").unwrap();
    fs::write(root.join("child/memory.max"), "5000").unwrap();
    fs::write(root.join("child/memory.current"), "100").unwrap();
    let mount = format!(
        "1 2 0:1 /host/pod {} rw - cgroup2 cgroup rw",
        root.display()
    );
    for group in ["0::/host/pod/child", "0::/child"] {
        let limits = discover_cgroups(9000, group, &mount).unwrap();
        assert_eq!(limits.cpu_quota, Some(2));
        assert_eq!(limits.available_memory, 200);
    }
    fs::write(root.join("memory.current"), "1100").unwrap();
    assert_eq!(
        discover_cgroups(9000, "0::/", &mount)
            .unwrap()
            .available_memory,
        0
    );
    fs::write(root.join("cpu.max"), "broken").unwrap();
    assert!(discover_cgroups(9000, "0::/", &mount).is_err());
}

#[test]
fn cgroup_v1_combined_controllers_and_unlimited_values() {
    let dir = TempDir::new("v1");
    let root = dir.path();
    fs::write(root.join("cpu.cfs_quota_us"), "-1").unwrap();
    fs::write(root.join("cpu.cfs_period_us"), "100000").unwrap();
    fs::write(root.join("memory.limit_in_bytes"), u64::MAX.to_string()).unwrap();
    fs::write(root.join("memory.usage_in_bytes"), "100").unwrap();
    let mount = format!(
        "1 2 0:1 / {} rw - cgroup cgroup rw,cpu,cpuacct,memory",
        root.display()
    );
    let limits = discover_cgroups(9000, "3:cpu,cpuacct,memory:/", &mount).unwrap();
    assert_eq!(limits.cpu_quota, None);
    assert_eq!(limits.available_memory, 9000);
    fs::write(root.join("cpu.cfs_quota_us"), "150000").unwrap();
    assert_eq!(
        discover_cgroups(9000, "3:cpu,cpuacct,memory:/", &mount)
            .unwrap()
            .cpu_quota,
        Some(1)
    );
    let hybrid_mounts = format!(
        "1 2 0:1 / {} rw - cgroup2 cgroup rw\n{mount}",
        root.display()
    );
    assert_eq!(
        discover_cgroups(9000, "0::/\n3:cpu,cpuacct,memory:/", &hybrid_mounts)
            .unwrap()
            .cpu_quota,
        Some(1)
    );
}
