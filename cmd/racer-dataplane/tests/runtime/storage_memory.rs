// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

fn stat(
    file: u64,
    shmem: u64,
    active: u64,
    inactive: u64,
    dirty: u64,
    writeback: u64,
    locked: u64,
) -> String {
    format!(
        "file {file}\nshmem {shmem}\nactive_file {active}\ninactive_file {inactive}\nfile_dirty {dirty}\nfile_writeback {writeback}\nunevictable {locked}\n"
    )
}

struct Fixture(PathBuf);
impl Fixture {
    fn new() -> Self {
        static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let path = std::env::temp_dir().join(format!(
            "racer-memory-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir(&path).unwrap();
        Self(path)
    }
    fn group(&self, name: &str, limit: &str, current: &str, stat: &str) {
        let path = self.0.join(name);
        std::fs::create_dir_all(&path).unwrap();
        for (file, value) in [
            ("memory.max", limit),
            ("memory.current", current),
            ("memory.stat", stat),
        ] {
            std::fs::write(path.join(file), value).unwrap();
        }
    }
    fn available(&self, host: u64, group: &str) -> io::Result<u64> {
        cgroup_available_memory(host, &self.0, &format!("0::/{group}\n"))
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        std::fs::remove_dir_all(&self.0).unwrap();
    }
}

#[test]
fn clean_cache_excludes_overlapping_and_unreclaimable_counters() {
    for (counters, expected) in [
        (stat(1000, 200, 300, 500, 50, 70, 80), 600),
        // Never add LRU counters to file, and cap inconsistent snapshots by
        // the smaller view. Shmem stays excluded even when LRU counts are high.
        (stat(1000, 900, 300, 500, 0, 0, 0), 100),
        (stat(1000, 0, 100, 200, 0, 0, 0), 300),
        (stat(100, 200, 300, 500, 0, 0, 0), 0),
        (stat(1000, 0, 300, 500, 400, 400, 400), 0),
        (stat(u64::MAX, 0, u64::MAX, u64::MAX, u64::MAX, 1, 1), 0),
    ] {
        assert_eq!(clean_file_cache(&counters).unwrap(), expected);
    }
    // Unknown kernel counters are not an additional cache credit.
    assert_eq!(
        clean_file_cache(&(stat(1000, 0, 0, 1000, 0, 0, 0) + "slab_reclaimable 5000\n")).unwrap(),
        1000
    );
}

#[test]
fn full_four_gib_cgroup_admits_grow_and_shrink_only_with_clean_cache() {
    let fixture = Fixture::new();
    let limit = (4u64 << 30).to_string();
    let old = LayoutPlan::new(2 << 40, 1).unwrap().resources();
    let cached = stat(3 << 30, 0, 1 << 30, 2 << 30, 0, 0, 0);
    fixture.group("", &limit, &limit, &cached);
    let available = fixture.available(8 << 30, "").unwrap();
    assert_eq!(available, 3 << 30);
    for capacity in [32 << 20, 4 << 40] {
        let plan = LayoutPlan::new(capacity, 1).unwrap();
        assert!(validate_resources(old, plan, available).is_ok());
        for blocked in [
            stat(0, 0, 0, 0, 0, 0, 0),
            stat(3 << 30, 3 << 30, 0, 0, 0, 0, 0),
            stat(3 << 30, 0, 0, 3 << 30, 3 << 30, 0, 0),
            stat(3 << 30, 0, 0, 3 << 30, 0, 3 << 30, 0),
            stat(3 << 30, 0, 0, 3 << 30, 0, 0, 3 << 30),
        ] {
            fixture.group("", &limit, &limit, &blocked);
            let available = fixture.available(8 << 30, "").unwrap();
            assert_eq!(available, 0);
            assert!(validate_resources(old, plan, available).is_err());
        }
    }
}

#[test]
fn headroom_is_capped_by_host_and_each_ancestor_without_double_credit() {
    let fixture = Fixture::new();
    let cached = stat(600, 0, 200, 400, 0, 0, 0);
    fixture.group("", "max", "0", "");
    fixture.group("parent", "1000", "900", &cached);
    fixture.group("parent/leaf", "1000", "1000", &cached);
    assert_eq!(fixture.available(2000, "parent/leaf").unwrap(), 600);
    assert_eq!(fixture.available(500, "parent/leaf").unwrap(), 500);
    fixture.group("parent", "1000", "1000", &stat(50, 0, 0, 50, 0, 0, 0));
    assert_eq!(fixture.available(2000, "parent/leaf").unwrap(), 50);
    fixture.group("parent", "1000", "1100", &stat(50, 0, 0, 50, 0, 0, 0));
    assert_eq!(fixture.available(2000, "parent/leaf").unwrap(), 0);
    fixture.group("parent", "max", "invalid", "invalid");
    assert_eq!(fixture.available(2000, "parent/leaf").unwrap(), 600);
    // Namespace-relative fallback still evaluates its finite root.
    fixture.group("", "1000", "1200", &cached);
    assert_eq!(fixture.available(2000, "not-visible/leaf").unwrap(), 400);
    fixture.group("", "1000", "100", &cached);
    assert_eq!(fixture.available(2000, "").unwrap(), 1000);
    fixture.group("", "1000", "100", &stat(0, 0, 0, 0, 0, 0, 0));
    assert_eq!(fixture.available(2000, "").unwrap(), 900);
    fixture.group("", "0", "100", &cached);
    assert_eq!(fixture.available(2000, "").unwrap(), 0);
    std::fs::remove_file(fixture.0.join("memory.max")).unwrap();
    assert_eq!(fixture.available(2000, "").unwrap(), 2000);
    assert_eq!(
        cgroup_available_memory(2000, &fixture.0, "1:memory:/legacy\n").unwrap(),
        2000
    );
}

#[test]
fn malformed_finite_limits_usage_and_cache_stats_fail_closed_including_ancestors() {
    let fixture = Fixture::new();
    let valid = stat(600, 0, 200, 400, 0, 0, 0);
    fixture.group("leaf", "1000", "900", &valid);
    for (limit, current, counters) in [
        ("garbage", "900", valid.clone()),
        ("18446744073709551616", "900", valid.clone()),
        ("-1", "900", valid.clone()),
        ("", "900", valid.clone()),
        ("1000", "max", valid.clone()),
        ("1000", "-1", valid.clone()),
        ("1000", "900", String::new()),
        ("1000", "900", valid.replace("shmem 0\n", "")),
        ("1000", "900", valid.replace("file 600", "file invalid")),
        ("1000", "900", valid.replace("file 600", "file 600 extra")),
        ("1000", "900", valid.clone() + "file 600\n"),
    ] {
        fixture.group("", limit, current, &counters);
        assert!(
            fixture.available(2000, "leaf").is_err(),
            "{limit:?} {current:?} {counters:?}"
        );
    }
    fixture.group("", "1000", "900", &valid);
    std::fs::remove_file(fixture.0.join("memory.stat")).unwrap();
    assert!(fixture.available(2000, "leaf").is_err());
    fixture.group("", "1000", "900", &valid);
    std::fs::remove_file(fixture.0.join("memory.current")).unwrap();
    assert!(fixture.available(2000, "leaf").is_err());
}

#[test]
#[ignore = "opt-in real buffered-file measurement; needs a quiet cgroup and 256 MiB workspace disk"]
fn real_buffered_file_cache_is_credited_after_sync() {
    use std::io::Write;
    let fixture = Fixture::new();
    let cgroups = std::fs::read_to_string("/proc/self/cgroup").unwrap();
    let relative = cgroups.lines().find_map(|l| l.strip_prefix("0::")).unwrap();
    let root = Path::new("/sys/fs/cgroup");
    let mut group = root.join(relative.trim_start_matches('/'));
    if !group.join("memory.current").exists() {
        group = root.to_path_buf();
    }
    let read_cache =
        || clean_file_cache(&std::fs::read_to_string(group.join("memory.stat")).unwrap()).unwrap();
    let before = read_cache();
    let mut file = File::create(fixture.0.join("payload")).unwrap();
    let buffer = vec![0x5a; 4 << 20];
    for _ in 0..64 {
        file.write_all(&buffer).unwrap();
    }
    file.sync_all().unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    let after = loop {
        let after = read_cache();
        if after >= before + (128 << 20) {
            break after;
        }
        assert!(
            Instant::now() < deadline,
            "clean cache did not increase: before={before} after={after}"
        );
        thread::sleep(Duration::from_millis(50));
    };
    eprintln!(
        "real buffered-file clean cache: before={before} after={after} delta={}",
        after - before
    );
    assert!(available_memory().unwrap() > 0);
}
