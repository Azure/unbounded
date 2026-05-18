// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::collections::BTreeSet;
use std::fs;
use std::path::Path;

/// One transport shard - the cores assigned to drive a single HCA.
/// `dev_name == None` is the TCP fallback used when no RDMA NICs
/// are discoverable.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NicShard {
    pub dev_name: Option<String>,
    pub pci_bdf: Option<String>,
    pub numa: Option<u16>,
    pub cpus: Vec<u32>,
}

/// Per-thread placement record produced by [`flatten_shards`]. One
/// `ProgressSlot` becomes one pinned OS thread and one Mercury Class.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ProgressSlot {
    pub shard: usize,
    pub thread_in_shard: u16,
    pub cpu: u32,
    pub numa: Option<u16>,
}

/// Knobs that govern shard discovery. Defaults are picked for GB200
/// + 4x 400G but are sensible on any datacenter host.
#[derive(Clone, Debug)]
pub struct TopologyConfig {
    /// Upper bound on progress threads assigned to any single NIC.
    /// The actual count may be lower if the NIC's local pool is
    /// smaller. Default: 8.
    pub threads_per_nic_cap: usize,
    /// If true, use both SMT siblings of each physical core. The
    /// default (false) keeps one logical CPU per physical core,
    /// which is what RDMA progress workloads typically want.
    pub use_smt_siblings: bool,
    /// Honor `/sys/devices/system/cpu/isolated` when non-empty:
    /// restrict per-NIC pools to its intersection.
    pub respect_isolated: bool,
    /// When `isolated` is empty (or `respect_isolated` is false),
    /// drop CPU 0 of every NUMA node the pool spans. Cheap defense
    /// against contending with kernel housekeeping.
    pub exclude_node_cpu0: bool,
    /// Require `/sys/class/infiniband/<dev>/node_type` to contain
    /// `CA` (filters out soft-RoCE/iWARP shims and switch ports).
    pub require_node_type_ca: bool,
    /// Require at least one port whose `state` contains `ACTIVE`.
    pub require_active_port: bool,
}

impl Default for TopologyConfig {
    fn default() -> Self {
        Self {
            threads_per_nic_cap: 8,
            use_smt_siblings: false,
            respect_isolated: true,
            exclude_node_cpu0: true,
            require_node_type_ca: true,
            require_active_port: true,
        }
    }
}

/// Discover one shard per usable RDMA HCA on the running host.
///
/// Returns an empty vec when sysfs lacks `class/infiniband/` (no
/// kernel IB stack) or when every device fails the health/affinity
/// filters. Callers that need a non-empty result should substitute
/// [`fallback_shard`].
pub fn discover_nic_shards() -> Vec<NicShard> {
    discover_with(&TopologyConfig::default(), Path::new("/sys"))
}

/// Single TCP fallback shard for hosts without RDMA NICs. Not pinned
/// to any specific NUMA node. `threads` controls how many progress
/// threads the embedder will spawn for it.
pub fn fallback_shard(threads: usize) -> NicShard {
    let n = threads.max(1);
    NicShard {
        dev_name: None,
        pci_bdf: None,
        numa: None,
        cpus: (0..n as u32).collect(),
    }
}

/// Sysfs-root-parameterized variant of [`discover_nic_shards`].
/// Production passes `/sys`; tests pass a staged temp dir.
pub fn discover_with(cfg: &TopologyConfig, sys_root: &Path) -> Vec<NicShard> {
    let mut candidates = enumerate_hcas(cfg, sys_root);
    // Stable order across runs so log lines and worker indices line
    // up with what an operator sees in /sys and `lspci`.
    candidates.sort_by(|a, b| a.pci_bdf.cmp(&b.pci_bdf).then(a.dev_name.cmp(&b.dev_name)));

    let isolated = if cfg.respect_isolated {
        read_cpulist_file(&sys_root.join("devices/system/cpu/isolated")).unwrap_or_default()
    } else {
        Vec::new()
    };

    // Build per-NIC pools.
    let mut pools: Vec<Vec<u32>> = Vec::with_capacity(candidates.len());
    for hca in &candidates {
        let raw = nic_local_cpus(sys_root, hca);
        let after_iso = if !isolated.is_empty() {
            intersect(&raw, &isolated)
        } else if cfg.exclude_node_cpu0 {
            exclude_node_cpu0(sys_root, &raw)
        } else {
            raw
        };
        let collapsed = if cfg.use_smt_siblings {
            after_iso
        } else {
            collapse_smt(sys_root, &after_iso)
        };
        pools.push(collapsed);
    }

    let assigned = allocate_disjoint(&pools, cfg.threads_per_nic_cap);

    candidates
        .into_iter()
        .zip(assigned)
        .filter_map(|(hca, cpus)| {
            if cpus.is_empty() {
                eprintln!(
                    "topology: dropping HCA {} (numa={:?}): no usable CPUs after filtering",
                    hca.dev_name.as_deref().unwrap_or("?"),
                    hca.numa,
                );
                return None;
            }
            Some(NicShard {
                dev_name: hca.dev_name,
                pci_bdf: hca.pci_bdf,
                numa: hca.numa,
                cpus,
            })
        })
        .collect()
}

/// Expand `shards` into one [`ProgressSlot`] per assigned CPU.
/// Per-slot `numa` is derived from the CPU when sysfs is available,
/// falling back to the shard's NIC-level hint.
pub fn flatten_shards(shards: &[NicShard]) -> Vec<ProgressSlot> {
    flatten_with(shards, Path::new("/sys"))
}

/// Sysfs-root-parameterized variant of [`flatten_shards`].
pub fn flatten_with(shards: &[NicShard], sys_root: &Path) -> Vec<ProgressSlot> {
    let mut slots = Vec::new();
    for (idx, shard) in shards.iter().enumerate() {
        for (t, &cpu) in shard.cpus.iter().enumerate() {
            let numa = numa_of_cpu(sys_root, cpu).or(shard.numa);
            slots.push(ProgressSlot {
                shard: idx,
                thread_in_shard: u16::try_from(t).expect("threads per shard fit in u16"),
                cpu,
                numa,
            });
        }
    }
    slots
}

// ---------------------------------------------------------------------------
// HCA enumeration
// ---------------------------------------------------------------------------

#[derive(Clone, Debug)]
struct Hca {
    dev_name: Option<String>,
    pci_bdf: Option<String>,
    numa: Option<u16>,
}

fn enumerate_hcas(cfg: &TopologyConfig, sys_root: &Path) -> Vec<Hca> {
    let ib_root = sys_root.join("class/infiniband");
    let entries = match fs::read_dir(&ib_root) {
        Ok(e) => e,
        Err(_) => return Vec::new(),
    };
    let mut out = Vec::new();
    for entry in entries.flatten() {
        let name_os = entry.file_name();
        let dev = match name_os.to_str() {
            Some(s) if !s.is_empty() => s.to_owned(),
            _ => continue,
        };
        if cfg.require_node_type_ca && !node_type_is_ca(sys_root, &dev) {
            eprintln!("topology: skipping {dev}: node_type is not CA");
            continue;
        }
        if cfg.require_active_port && !has_active_port(sys_root, &dev) {
            eprintln!("topology: skipping {dev}: no ACTIVE port");
            continue;
        }
        let numa = read_nic_numa(sys_root, &dev);
        let pci_bdf = read_pci_bdf(sys_root, &dev);
        out.push(Hca {
            dev_name: Some(dev),
            pci_bdf,
            numa,
        });
    }
    out
}

fn node_type_is_ca(sys_root: &Path, dev: &str) -> bool {
    let path = sys_root.join(format!("class/infiniband/{dev}/node_type"));
    match fs::read_to_string(path) {
        Ok(s) => s.contains("CA"),
        // Older kernels may omit node_type; treat as CA so we
        // don't regress on hosts the previous logic accepted.
        Err(_) => true,
    }
}

fn has_active_port(sys_root: &Path, dev: &str) -> bool {
    let ports_dir = sys_root.join(format!("class/infiniband/{dev}/ports"));
    let entries = match fs::read_dir(&ports_dir) {
        Ok(e) => e,
        // If `ports/` is absent (test fixture or odd driver), don't
        // block discovery on it.
        Err(_) => return true,
    };
    for entry in entries.flatten() {
        let state_path = entry.path().join("state");
        if let Ok(s) = fs::read_to_string(&state_path) {
            if s.contains("ACTIVE") {
                return true;
            }
        }
    }
    false
}

fn read_nic_numa(sys_root: &Path, dev: &str) -> Option<u16> {
    let path = sys_root.join(format!("class/infiniband/{dev}/device/numa_node"));
    let s = fs::read_to_string(path).ok()?;
    match s.trim().parse::<i32>().ok()? {
        n if n < 0 => None,
        n => Some(n as u16),
    }
}

fn read_pci_bdf(sys_root: &Path, dev: &str) -> Option<String> {
    let path = sys_root.join(format!("class/infiniband/{dev}/device/uevent"));
    let s = fs::read_to_string(path).ok()?;
    for line in s.lines() {
        if let Some(v) = line.strip_prefix("PCI_SLOT_NAME=") {
            return Some(v.trim().to_string());
        }
    }
    None
}

// ---------------------------------------------------------------------------
// CPU set resolution
// ---------------------------------------------------------------------------

fn nic_local_cpus(sys_root: &Path, hca: &Hca) -> Vec<u32> {
    let dev = match &hca.dev_name {
        Some(d) => d,
        None => return read_online_cpus(sys_root),
    };
    let local = sys_root.join(format!("class/infiniband/{dev}/device/local_cpulist"));
    if let Some(cpus) = read_cpulist_file(&local) {
        if !cpus.is_empty() {
            return cpus;
        }
    }
    if let Some(node) = hca.numa {
        let node_path = sys_root.join(format!("devices/system/node/node{node}/cpulist"));
        if let Some(cpus) = read_cpulist_file(&node_path) {
            if !cpus.is_empty() {
                return cpus;
            }
        }
    }
    read_online_cpus(sys_root)
}

fn read_online_cpus(sys_root: &Path) -> Vec<u32> {
    read_cpulist_file(&sys_root.join("devices/system/cpu/online")).unwrap_or_default()
}

fn read_cpulist_file(path: &Path) -> Option<Vec<u32>> {
    let raw = fs::read_to_string(path).ok()?;
    Some(parse_cpulist(&raw))
}

/// Parse a Linux cpulist like `"0-71,144-215"` into a sorted, deduped
/// Vec. Returns empty for empty input or unparseable fragments.
fn parse_cpulist(s: &str) -> Vec<u32> {
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

// ---------------------------------------------------------------------------
// Pool refinement
// ---------------------------------------------------------------------------

fn intersect(a: &[u32], b: &[u32]) -> Vec<u32> {
    let set: BTreeSet<u32> = b.iter().copied().collect();
    a.iter().copied().filter(|c| set.contains(c)).collect()
}

fn exclude_node_cpu0(sys_root: &Path, pool: &[u32]) -> Vec<u32> {
    let mut node_to_first: std::collections::HashMap<u16, u32> = std::collections::HashMap::new();
    for &cpu in pool {
        if let Some(n) = numa_of_cpu(sys_root, cpu) {
            node_to_first
                .entry(n)
                .and_modify(|c| {
                    if cpu < *c {
                        *c = cpu
                    }
                })
                .or_insert(cpu);
        }
    }
    if node_to_first.is_empty() {
        // No node info -> conservatively drop CPU 0 if present.
        return pool.iter().copied().filter(|c| *c != 0).collect();
    }
    let drop: BTreeSet<u32> = node_to_first.into_values().collect();
    pool.iter().copied().filter(|c| !drop.contains(c)).collect()
}

fn collapse_smt(sys_root: &Path, pool: &[u32]) -> Vec<u32> {
    let pool_set: BTreeSet<u32> = pool.iter().copied().collect();
    let mut keep = BTreeSet::new();
    let mut seen_core = BTreeSet::new();
    for &cpu in pool {
        let siblings = thread_siblings(sys_root, cpu);
        let core_id = siblings.iter().copied().min().unwrap_or(cpu);
        if seen_core.insert(core_id) && pool_set.contains(&core_id) {
            keep.insert(core_id);
        }
    }
    keep.into_iter().collect()
}

fn thread_siblings(sys_root: &Path, cpu: u32) -> Vec<u32> {
    let path = sys_root.join(format!(
        "devices/system/cpu/cpu{cpu}/topology/thread_siblings_list"
    ));
    match read_cpulist_file(&path) {
        Some(v) if !v.is_empty() => v,
        _ => vec![cpu],
    }
}

fn numa_of_cpu(sys_root: &Path, cpu: u32) -> Option<u16> {
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

// ---------------------------------------------------------------------------
// Global disjoint allocation
// ---------------------------------------------------------------------------

/// Given per-NIC candidate pools, assign each NIC up to `cap` CPUs
/// such that assignments across NICs are disjoint when possible. If
/// the union of pools is too small to satisfy every NIC, the over-
/// flow NICs receive an oversubscribed (re-used) slice from their
/// own pool; this is logged but never silently drops a NIC.
///
/// The allocator processes NICs in order of pool size ascending so
/// the most constrained NICs get first pick.
fn allocate_disjoint(pools: &[Vec<u32>], cap: usize) -> Vec<Vec<u32>> {
    let n = pools.len();
    let cap = cap.max(1);
    let mut order: Vec<usize> = (0..n).collect();
    order.sort_by_key(|&i| pools[i].len());

    let mut claimed: BTreeSet<u32> = BTreeSet::new();
    let mut out: Vec<Vec<u32>> = vec![Vec::new(); n];

    for &i in &order {
        let pool = &pools[i];
        if pool.is_empty() {
            continue;
        }
        let want = cap.min(pool.len()).max(1);
        let mut picked: Vec<u32> = pool
            .iter()
            .copied()
            .filter(|c| !claimed.contains(c))
            .take(want)
            .collect();

        if picked.len() < want {
            // Oversubscription: fall back to the NIC's full pool,
            // round-robin from where we left off, allowing overlap.
            let already: BTreeSet<u32> = picked.iter().copied().collect();
            let leftover: Vec<u32> = pool
                .iter()
                .copied()
                .filter(|c| !already.contains(c))
                .collect();
            let mut j = 0usize;
            while picked.len() < want && !leftover.is_empty() {
                picked.push(leftover[j % leftover.len()]);
                j += 1;
            }
            if picked.len() < want {
                eprintln!(
                    "topology: NIC index {i} oversubscribed (pool={}, want={want}); duplicates allowed",
                    pool.len()
                );
            }
        }

        for c in &picked {
            claimed.insert(*c);
        }
        out[i] = picked;
    }
    out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    struct FakeSys {
        root: PathBuf,
    }

    impl FakeSys {
        fn new() -> Self {
            // Per-test unique dir under the workspace target/ so we
            // don't touch /tmp (project rule) and avoid collisions
            // across parallel tests.
            let pid = std::process::id();
            let counter = NEXT_ID.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            let root = std::env::current_dir()
                .unwrap()
                .join("target")
                .join("tmp-topology-tests")
                .join(format!("{pid}-{counter}"));
            let _ = fs::remove_dir_all(&root);
            fs::create_dir_all(&root).unwrap();
            Self { root }
        }

        fn write(&self, rel: &str, contents: &str) {
            let p = self.root.join(rel);
            fs::create_dir_all(p.parent().unwrap()).unwrap();
            fs::write(p, contents).unwrap();
        }

        fn touch_dir(&self, rel: &str) {
            fs::create_dir_all(self.root.join(rel)).unwrap();
        }

        /// Stage a complete-looking HCA. `local_cpulist` may be `None`
        /// to force fallback to the NUMA-node cpulist.
        fn add_hca(
            &self,
            dev: &str,
            bdf: &str,
            numa: i32,
            local_cpulist: Option<&str>,
            active: bool,
        ) {
            self.touch_dir(&format!("class/infiniband/{dev}/device"));
            self.write(&format!("class/infiniband/{dev}/node_type"), "1: CA\n");
            self.write(
                &format!("class/infiniband/{dev}/device/numa_node"),
                &format!("{numa}\n"),
            );
            self.write(
                &format!("class/infiniband/{dev}/device/uevent"),
                &format!("PCI_SLOT_NAME={bdf}\n"),
            );
            if let Some(list) = local_cpulist {
                self.write(
                    &format!("class/infiniband/{dev}/device/local_cpulist"),
                    &format!("{list}\n"),
                );
            }
            self.write(
                &format!("class/infiniband/{dev}/ports/1/state"),
                if active { "4: ACTIVE\n" } else { "1: DOWN\n" },
            );
        }

        /// Register a CPU under its NUMA node and (optionally) its
        /// SMT siblings list. Without siblings the CPU is treated as
        /// single-threaded.
        fn add_cpu(&self, cpu: u32, node: u16, siblings: Option<&str>) {
            self.touch_dir(&format!("devices/system/cpu/cpu{cpu}/node{node}"));
            self.touch_dir(&format!("devices/system/node/node{node}"));
            if let Some(s) = siblings {
                self.write(
                    &format!("devices/system/cpu/cpu{cpu}/topology/thread_siblings_list"),
                    &format!("{s}\n"),
                );
            }
        }

        fn add_node_cpulist(&self, node: u16, list: &str) {
            self.write(
                &format!("devices/system/node/node{node}/cpulist"),
                &format!("{list}\n"),
            );
        }
    }

    impl Drop for FakeSys {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.root);
        }
    }

    static NEXT_ID: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

    fn dev_names(shards: &[NicShard]) -> Vec<String> {
        shards
            .iter()
            .map(|s| s.dev_name.clone().unwrap_or_else(|| "tcp".into()))
            .collect()
    }

    // -------- parse_cpulist --------

    #[test]
    fn parse_cpulist_basics() {
        assert_eq!(parse_cpulist("0-3"), vec![0, 1, 2, 3]);
        assert_eq!(parse_cpulist("0-1,4-5"), vec![0, 1, 4, 5]);
        assert_eq!(parse_cpulist("  5  "), vec![5]);
        assert_eq!(parse_cpulist(""), Vec::<u32>::new());
        assert_eq!(parse_cpulist("\n"), Vec::<u32>::new());
        // Out-of-order groups end up sorted and deduped.
        assert_eq!(parse_cpulist("5,0-1,5"), vec![0, 1, 5]);
    }

    // -------- empty / fallback --------

    #[test]
    fn no_infiniband_dir_returns_empty() {
        let s = FakeSys::new();
        assert!(discover_with(&TopologyConfig::default(), &s.root).is_empty());
    }

    #[test]
    fn fallback_shard_threads() {
        let f = fallback_shard(4);
        assert!(f.dev_name.is_none());
        assert_eq!(f.cpus, vec![0, 1, 2, 3]);
        // Always at least one progress thread.
        assert_eq!(fallback_shard(0).cpus, vec![0]);
    }

    // -------- single NIC, local_cpulist drives affinity --------

    #[test]
    fn single_nic_uses_local_cpulist() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:af:00.0", 0, Some("4-11"), true);
        // No SMT, simple node layout.
        for cpu in 0u32..72 {
            s.add_cpu(cpu, 0, None);
        }
        let cfg = TopologyConfig {
            threads_per_nic_cap: 4,
            respect_isolated: false,
            exclude_node_cpu0: false,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].dev_name.as_deref(), Some("mlx5_0"));
        assert_eq!(got[0].pci_bdf.as_deref(), Some("0000:af:00.0"));
        assert_eq!(got[0].numa, Some(0));
        // 4 cores drawn from the local_cpulist (4-11) in order.
        assert_eq!(got[0].cpus, vec![4, 5, 6, 7]);
    }

    // -------- NUMA-only fallback when local_cpulist missing --------

    #[test]
    fn falls_back_to_node_cpulist_when_local_missing() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:af:00.0", 1, None, true);
        for cpu in 72u32..80 {
            s.add_cpu(cpu, 1, None);
        }
        s.add_node_cpulist(1, "72-79");
        let cfg = TopologyConfig {
            threads_per_nic_cap: 3,
            respect_isolated: false,
            exclude_node_cpu0: false,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].cpus, vec![72, 73, 74]);
    }

    // -------- NUMA -1 + global online fallback --------

    #[test]
    fn nic_numa_minus_one_uses_online_cpus() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:af:00.0", -1, None, true);
        s.write("devices/system/cpu/online", "0-3\n");
        for cpu in 0u32..4 {
            s.add_cpu(cpu, 0, None);
        }
        let cfg = TopologyConfig {
            threads_per_nic_cap: 2,
            respect_isolated: false,
            exclude_node_cpu0: false,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].numa, None);
        assert_eq!(got[0].cpus, vec![0, 1]);
    }

    // -------- GB200-like: 4 NICs, 2 NUMA, disjoint assignment --------

    #[test]
    fn gb200_like_four_nic_two_numa_disjoint() {
        let s = FakeSys::new();
        // Two NICs per NUMA, each pair shares a local_cpulist.
        s.add_hca("mlx5_0", "0000:01:00.0", 0, Some("0-35"), true);
        s.add_hca("mlx5_1", "0000:02:00.0", 0, Some("0-35"), true);
        s.add_hca("mlx5_2", "0000:81:00.0", 1, Some("36-71"), true);
        s.add_hca("mlx5_3", "0000:82:00.0", 1, Some("36-71"), true);
        for cpu in 0u32..36 {
            s.add_cpu(cpu, 0, None);
        }
        for cpu in 36u32..72 {
            s.add_cpu(cpu, 1, None);
        }
        let cfg = TopologyConfig {
            threads_per_nic_cap: 8,
            respect_isolated: false,
            exclude_node_cpu0: false,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        assert_eq!(
            dev_names(&got),
            vec!["mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3"]
        );
        // Each NIC has 8 cores.
        for sh in &got {
            assert_eq!(sh.cpus.len(), 8, "shard {sh:?}");
        }
        // Pair within NUMA 0 is disjoint and on the right side.
        let n0: BTreeSet<u32> = got[0]
            .cpus
            .iter()
            .chain(got[1].cpus.iter())
            .copied()
            .collect();
        assert_eq!(n0.len(), 16);
        assert!(n0.iter().all(|c| *c < 36));
        // Pair within NUMA 1 is disjoint and on the right side.
        let n1: BTreeSet<u32> = got[2]
            .cpus
            .iter()
            .chain(got[3].cpus.iter())
            .copied()
            .collect();
        assert_eq!(n1.len(), 16);
        assert!(n1.iter().all(|c| (36..72).contains(c)));
    }

    // -------- EPYC-like: many CCDs, one NIC per CCD --------

    #[test]
    fn epyc_like_eight_nic_eight_ccd() {
        let s = FakeSys::new();
        for i in 0u32..8 {
            let dev = format!("mlx5_{i}");
            let bdf = format!("0000:{:02x}:00.0", 0x10 + i);
            let lo = i * 8;
            let hi = lo + 7;
            s.add_hca(&dev, &bdf, i as i32, Some(&format!("{lo}-{hi}")), true);
            for cpu in lo..=hi {
                s.add_cpu(cpu, i as u16, None);
            }
        }
        let cfg = TopologyConfig {
            threads_per_nic_cap: 4,
            respect_isolated: false,
            exclude_node_cpu0: false,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        assert_eq!(got.len(), 8);
        for (i, sh) in got.iter().enumerate() {
            let lo = (i as u32) * 8;
            assert_eq!(sh.cpus, vec![lo, lo + 1, lo + 2, lo + 3]);
            assert_eq!(sh.numa, Some(i as u16));
        }
    }

    // -------- CPU-less NUMA node (GB200 HBM) handling --------

    #[test]
    fn cpuless_node_yields_empty_pool_and_dropped() {
        let s = FakeSys::new();
        // mlx5_0 valid; mlx5_1 reports NUMA=2 which has no CPUs.
        s.add_hca("mlx5_0", "0000:01:00.0", 0, None, true);
        s.add_hca("mlx5_1", "0000:02:00.0", 2, None, true);
        for cpu in 0u32..8 {
            s.add_cpu(cpu, 0, None);
        }
        s.add_node_cpulist(0, "0-7");
        // node2 is CPU-less; no cpulist file, no online entries.
        s.write("devices/system/cpu/online", "0-7\n");
        let cfg = TopologyConfig {
            threads_per_nic_cap: 2,
            respect_isolated: false,
            exclude_node_cpu0: false,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        // The CPU-less NIC falls back to the online list (0-7), so it
        // is *not* dropped; it shares the online pool. This is the
        // intentional, documented behavior: HBM-only nodes still need
        // to participate, just not strictly NUMA-local.
        assert_eq!(got.len(), 2);
        // But assignments are disjoint across the two NICs.
        let a: BTreeSet<u32> = got[0].cpus.iter().copied().collect();
        let b: BTreeSet<u32> = got[1].cpus.iter().copied().collect();
        assert!(a.is_disjoint(&b));
    }

    // -------- node_type filter --------

    #[test]
    fn rxe_filtered_out() {
        let s = FakeSys::new();
        s.touch_dir("class/infiniband/rxe0/device");
        s.write("class/infiniband/rxe0/node_type", "2: SWITCH\n");
        s.write("class/infiniband/rxe0/device/numa_node", "0\n");
        s.write("class/infiniband/rxe0/ports/1/state", "4: ACTIVE\n");
        let got = discover_with(&TopologyConfig::default(), &s.root);
        assert!(got.is_empty());
    }

    // -------- port-state filter --------

    #[test]
    fn down_port_filtered_out() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:01:00.0", 0, Some("0-3"), false);
        for cpu in 0u32..4 {
            s.add_cpu(cpu, 0, None);
        }
        let got = discover_with(&TopologyConfig::default(), &s.root);
        assert!(got.is_empty());
    }

    // -------- isolcpus respected --------

    #[test]
    fn isolated_intersected_into_pool() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:01:00.0", 0, Some("0-15"), true);
        for cpu in 0u32..16 {
            s.add_cpu(cpu, 0, None);
        }
        s.write("devices/system/cpu/isolated", "8-15\n");
        let cfg = TopologyConfig {
            threads_per_nic_cap: 4,
            respect_isolated: true,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        assert_eq!(got[0].cpus, vec![8, 9, 10, 11]);
    }

    // -------- housekeeping exclusion (no isolcpus) --------

    #[test]
    fn excludes_first_cpu_of_each_node() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:01:00.0", 0, Some("0-7"), true);
        s.add_hca("mlx5_1", "0000:81:00.0", 1, Some("16-23"), true);
        for cpu in 0u32..8 {
            s.add_cpu(cpu, 0, None);
        }
        for cpu in 16u32..24 {
            s.add_cpu(cpu, 1, None);
        }
        let cfg = TopologyConfig {
            threads_per_nic_cap: 3,
            respect_isolated: true,
            exclude_node_cpu0: true,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        // First CPU of each node (0 and 16) dropped.
        assert!(!got[0].cpus.contains(&0));
        assert!(!got[1].cpus.contains(&16));
        assert_eq!(got[0].cpus, vec![1, 2, 3]);
        assert_eq!(got[1].cpus, vec![17, 18, 19]);
    }

    // -------- SMT collapse --------

    #[test]
    fn smt_siblings_collapsed_by_default() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:01:00.0", 0, Some("0-7"), true);
        // 4 physical cores, pairs (0,4), (1,5), (2,6), (3,7).
        for (cpu, sib) in [
            (0, "0,4"),
            (4, "0,4"),
            (1, "1,5"),
            (5, "1,5"),
            (2, "2,6"),
            (6, "2,6"),
            (3, "3,7"),
            (7, "3,7"),
        ] {
            s.add_cpu(cpu, 0, Some(sib));
        }
        let cfg = TopologyConfig {
            threads_per_nic_cap: 8,
            respect_isolated: false,
            exclude_node_cpu0: false,
            use_smt_siblings: false,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        assert_eq!(got[0].cpus, vec![0, 1, 2, 3]);

        let cfg2 = TopologyConfig {
            use_smt_siblings: true,
            ..cfg
        };
        let got2 = discover_with(&cfg2, &s.root);
        assert_eq!(got2[0].cpus, vec![0, 1, 2, 3, 4, 5, 6, 7]);
    }

    // -------- oversubscription path --------

    #[test]
    fn oversubscribed_pool_assigns_with_duplicates() {
        let s = FakeSys::new();
        // Two NICs share a 4-cpu local pool; cap=4 -> total demand 8
        // > pool size 4. First NIC gets disjoint 4, second NIC gets
        // the same 4 reused.
        s.add_hca("mlx5_0", "0000:01:00.0", 0, Some("0-3"), true);
        s.add_hca("mlx5_1", "0000:02:00.0", 0, Some("0-3"), true);
        for cpu in 0u32..4 {
            s.add_cpu(cpu, 0, None);
        }
        let cfg = TopologyConfig {
            threads_per_nic_cap: 4,
            respect_isolated: false,
            exclude_node_cpu0: false,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        assert_eq!(got.len(), 2);
        assert_eq!(got[0].cpus.len(), 4);
        assert_eq!(got[1].cpus.len(), 4);
        let a: BTreeSet<u32> = got[0].cpus.iter().copied().collect();
        let b: BTreeSet<u32> = got[1].cpus.iter().copied().collect();
        // No NIC is empty; second NIC overlaps with the first.
        assert!(!a.is_disjoint(&b));
    }

    // -------- sort by PCI BDF, not lex dev_name --------

    #[test]
    fn sorts_by_pci_bdf() {
        let s = FakeSys::new();
        // Lex order would put mlx5_10 before mlx5_2; BDF order does
        // not.
        s.add_hca("mlx5_2", "0000:02:00.0", 0, Some("0-3"), true);
        s.add_hca("mlx5_10", "0000:10:00.0", 0, Some("4-7"), true);
        for cpu in 0u32..8 {
            s.add_cpu(cpu, 0, None);
        }
        let cfg = TopologyConfig {
            threads_per_nic_cap: 2,
            respect_isolated: false,
            exclude_node_cpu0: false,
            ..TopologyConfig::default()
        };
        let got = discover_with(&cfg, &s.root);
        assert_eq!(dev_names(&got), vec!["mlx5_2", "mlx5_10"]);
    }

    // -------- flatten --------

    #[test]
    fn flatten_emits_one_slot_per_cpu() {
        let s = FakeSys::new();
        for cpu in 0u32..4 {
            s.add_cpu(cpu, 0, None);
        }
        for cpu in 8u32..12 {
            s.add_cpu(cpu, 1, None);
        }
        let shards = vec![
            NicShard {
                dev_name: Some("mlx5_0".into()),
                pci_bdf: Some("0000:01:00.0".into()),
                numa: Some(0),
                cpus: vec![1, 2],
            },
            NicShard {
                dev_name: Some("mlx5_1".into()),
                pci_bdf: Some("0000:81:00.0".into()),
                numa: Some(1),
                cpus: vec![9, 10],
            },
        ];
        let slots = flatten_with(&shards, &s.root);
        assert_eq!(slots.len(), 4);
        assert_eq!(
            slots[0],
            ProgressSlot {
                shard: 0,
                thread_in_shard: 0,
                cpu: 1,
                numa: Some(0)
            }
        );
        assert_eq!(
            slots[3],
            ProgressSlot {
                shard: 1,
                thread_in_shard: 1,
                cpu: 10,
                numa: Some(1)
            }
        );
    }
}
