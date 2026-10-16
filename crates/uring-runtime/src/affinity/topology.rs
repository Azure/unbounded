//! Discovery of the CPUs, CPU limits and devices visible to the calling thread.

use std::fs;
use std::io;
use std::num::NonZeroU64;
use std::path::{Path, PathBuf};

use super::{CpuSet, cgroup, current_cpus};
use crate::{Error, ErrorKind, Result};

/// A logical CPU and its physical location.
///
/// Physical identifiers come from `/sys/devices/system/cpu/cpuN/topology`.
/// They are `None` when the kernel does not expose them or reports `-1`
/// (common on some virtual machines and non-x86 platforms). Core identifiers
/// are only unique within a package (and die): SMT siblings share the same
/// `(package, die, core)` triple.
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq, Hash)]
pub struct CpuLocation {
    /// Logical CPU number, as accepted by [`pin_cpu`](super::pin_cpu).
    pub cpu: usize,
    /// Physical package (socket) identifier.
    pub package: Option<usize>,
    /// Die identifier within the package.
    pub die: Option<usize>,
    /// Physical core identifier within the package.
    pub core: Option<usize>,
    /// NUMA node, if the kernel exposes one.
    pub numa_node: Option<usize>,
}

impl CpuLocation {
    /// Create a location with only the logical CPU number known.
    pub const fn new(cpu: usize) -> Self {
        Self {
            cpu,
            package: None,
            die: None,
            core: None,
            numa_node: None,
        }
    }
}

/// An effective cgroup CPU bandwidth limit: `quota` microseconds of CPU time
/// per `period` microseconds.
///
/// A quota of 150000 per 100000 allows the equivalent of 1.5 fully busy CPUs,
/// spread over however many CPUs the thread is allowed to run on.
#[non_exhaustive]
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub struct CpuQuota {
    /// CPU time available per period, in microseconds.
    pub quota: NonZeroU64,
    /// Accounting period, in microseconds.
    pub period: NonZeroU64,
}

impl CpuQuota {
    /// Create a quota.
    pub const fn new(quota: NonZeroU64, period: NonZeroU64) -> Self {
        Self { quota, period }
    }

    /// The number of whole CPUs' worth of time per period, rounded down.
    pub const fn whole_cpus(&self) -> u64 {
        self.quota.get() / self.period.get()
    }
}

/// A device and the NUMA node it is attached to.
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq, Hash)]
pub struct DeviceLocality {
    /// The sysfs class the device was found in: `"net"` or `"infiniband"`.
    pub class: String,
    /// The device name within its class, for example `"eth0"`.
    pub name: String,
    /// The device's NUMA node, if known. Kernels report `-1` for "no node";
    /// that is mapped to `None`.
    pub numa_node: Option<usize>,
}

/// The CPUs, CPU limits and devices visible to the calling thread.
///
/// Produced by [`Topology::discover`]. The fields are inputs for an
/// application's own placement policy; this crate does not choose CPUs.
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Topology {
    /// CPUs the thread may run on: its affinity mask, intersected with the
    /// online CPUs and with every visible ancestor cgroup cpuset. Sorted by
    /// CPU number.
    pub cpus: Vec<CpuLocation>,
    /// The tightest CPU bandwidth limit of any visible ancestor cgroup, or
    /// `None` when every hierarchy is unlimited.
    pub quota: Option<CpuQuota>,
    /// Network (`/sys/class/net`) and InfiniBand (`/sys/class/infiniband`)
    /// devices: network devices first, then InfiniBand, each sorted by name.
    pub devices: Vec<DeviceLocality>,
}

/// Device classes scanned for NUMA locality.
const DEVICE_CLASSES: [&str; 2] = ["net", "infiniband"];

impl Topology {
    /// Discover the topology of the calling thread on the running system.
    ///
    /// Equivalent to `Topology::discover_at(Path::new("/"), &current_cpus()?)`.
    ///
    /// # Errors
    ///
    /// See [`Topology::discover_at`].
    pub fn discover() -> Result<Self> {
        Self::discover_at(Path::new("/"), &current_cpus()?)
    }

    /// Discover the topology using `/sys` and `/proc` below `root`, starting
    /// from the affinity mask `allowed`.
    ///
    /// The calling thread's cgroup memberships are read from
    /// `<root>/proc/thread-self/cgroup` and resolved through
    /// `<root>/proc/self/mountinfo`; mount points are interpreted relative to
    /// `root`. Every ancestor directory up to the mount point is consulted,
    /// because a limit on a parent cgroup applies to its children. This lets
    /// tests run discovery against a fake tree.
    ///
    /// # Errors
    ///
    /// - [`ErrorKind::InvalidConfiguration`] if a cgroup membership cannot be
    ///   resolved to a visible directory (an invisible limit is treated as
    ///   unknown, not unlimited), if a kernel file is malformed, or if no CPU
    ///   remains after intersecting all constraints.
    /// - An I/O error if a required file cannot be read. Optional files that
    ///   are absent are skipped, and devices that disappear during the scan
    ///   are omitted.
    pub fn discover_at(root: &Path, allowed: &CpuSet) -> Result<Self> {
        let cpu_root = under(root, Path::new("/sys/devices/system/cpu"));
        let online: CpuSet = read(&cpu_root.join("online"))?.parse()?;
        let mut cpus = allowed.intersection(&online);

        let memberships = read(&under(root, Path::new("/proc/thread-self/cgroup")))?;
        let mounts = read(&under(root, Path::new("/proc/self/mountinfo")))?;
        let mut quota = None;
        for hierarchy in cgroup::hierarchies(&memberships, &mounts)? {
            let leaf = under(root, &hierarchy.leaf);
            let mount = under(root, &hierarchy.mount);
            if !fs::metadata(&leaf).map_err(io_error)?.is_dir() {
                return Err(Error::new(ErrorKind::InvalidConfiguration)
                    .with_context("cgroup membership is not a directory"));
            }
            for directory in leaf.ancestors().take_while(|path| path.starts_with(&mount)) {
                if hierarchy.cpu {
                    cgroup::tighten_quota(&mut quota, read_quota(directory, hierarchy.unified)?);
                }
                if hierarchy.cpuset {
                    let names = if hierarchy.unified {
                        ["cpuset.cpus.effective", "cpuset.cpus"]
                    } else {
                        ["cpuset.effective_cpus", "cpuset.cpus"]
                    };
                    for name in names {
                        // An empty cpuset file means "inherit from the parent".
                        if let Some(value) = read_optional(&directory.join(name))?
                            && !value.trim().is_empty()
                        {
                            cpus = cpus.intersection(&value.parse()?);
                        }
                    }
                }
            }
        }
        if cpus.is_empty() {
            return Err(Error::new(ErrorKind::InvalidConfiguration).with_context("no usable CPUs"));
        }

        let cpus = cpus
            .iter()
            .map(|cpu| locate(&cpu_root, cpu))
            .collect::<Result<Vec<_>>>()?;
        let mut devices = Vec::new();
        for class in DEVICE_CLASSES {
            devices.extend(discover_devices(root, class)?);
        }
        Ok(Self {
            cpus,
            quota,
            devices,
        })
    }

    /// The logical CPU numbers in [`Topology::cpus`].
    pub fn cpu_set(&self) -> CpuSet {
        self.cpus.iter().map(|cpu| cpu.cpu).collect()
    }
}

/// Read a cgroup directory's CPU bandwidth limit, if it has one.
fn read_quota(directory: &Path, unified: bool) -> Result<Option<CpuQuota>> {
    if unified {
        return match read_optional(&directory.join("cpu.max"))? {
            Some(value) => cgroup::parse_v2_quota(&value),
            None => Ok(None),
        };
    }
    match (
        read_optional(&directory.join("cpu.cfs_quota_us"))?,
        read_optional(&directory.join("cpu.cfs_period_us"))?,
    ) {
        (Some(quota), Some(period)) => cgroup::parse_v1_quota(&quota, &period),
        (None, None) => Ok(None),
        _ => Err(Error::new(ErrorKind::InvalidConfiguration)
            .with_context("incomplete cgroup v1 CPU quota")),
    }
}

/// Read the physical location of one CPU.
fn locate(cpu_root: &Path, cpu: usize) -> Result<CpuLocation> {
    let directory = cpu_root.join(format!("cpu{cpu}"));
    let topology = directory.join("topology");
    let numa_node = match fs::read_dir(&directory) {
        Ok(entries) => entries
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
        Err(error) if error.kind() == io::ErrorKind::NotFound => None,
        Err(error) => return Err(io_error(error)),
    };
    Ok(CpuLocation {
        cpu,
        package: read_id(&topology.join("physical_package_id"))?,
        die: read_id(&topology.join("die_id"))?,
        core: read_id(&topology.join("core_id"))?,
        numa_node,
    })
}

/// Scan `<root>/sys/class/<class>` for devices and their NUMA nodes.
///
/// Regular files (such as `bonding_masters`) are skipped; symlinks to device
/// directories are followed. A device that disappears mid-scan is omitted.
fn discover_devices(root: &Path, class: &str) -> Result<Vec<DeviceLocality>> {
    let directory = under(root, Path::new("/sys/class")).join(class);
    let entries = match fs::read_dir(&directory) {
        Ok(entries) => entries,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(error) => return Err(io_error(error)),
    };
    let mut devices = Vec::new();
    for entry in entries {
        let entry = entry.map_err(io_error)?;
        let path = entry.path();
        match fs::metadata(&path) {
            Ok(metadata) if metadata.is_dir() => {}
            Ok(_) => continue,
            Err(error) if error.kind() == io::ErrorKind::NotFound => continue,
            Err(error) => return Err(io_error(error)),
        }
        let numa_node = read_optional(&path.join("device/numa_node"))?
            .and_then(|value| value.trim().parse::<usize>().ok());
        devices.push(DeviceLocality {
            class: class.to_owned(),
            name: entry.file_name().to_string_lossy().into_owned(),
            numa_node,
        });
    }
    devices.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(devices)
}

/// Join an absolute system path below `root`.
fn under(root: &Path, path: &Path) -> PathBuf {
    root.join(path.strip_prefix("/").unwrap_or(path))
}

fn io_error(error: io::Error) -> Error {
    Error::from(error).with_context("read topology")
}

fn read(path: &Path) -> Result<String> {
    fs::read_to_string(path).map_err(io_error)
}

/// Read a file, treating absence as `None`.
fn read_optional(path: &Path) -> Result<Option<String>> {
    match fs::read_to_string(path) {
        Ok(value) => Ok(Some(value)),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(io_error(error)),
    }
}

/// Read a sysfs topology identifier; absent files and `-1` are `None`.
fn read_id(path: &Path) -> Result<Option<usize>> {
    let Some(value) = read_optional(path)? else {
        return Ok(None);
    };
    let value = value
        .trim()
        .parse::<i64>()
        .map_err(|_| Error::new(ErrorKind::InvalidConfiguration).with_context("topology id"))?;
    Ok(usize::try_from(value).ok())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// A temporary fake `/sys` + `/proc` tree, removed on drop.
    struct Tree(PathBuf);

    impl Tree {
        fn new() -> Self {
            static NEXT: AtomicUsize = AtomicUsize::new(0);
            let path = std::env::temp_dir().join(format!(
                "uring-runtime-topology-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
            let _ = fs::remove_dir_all(&path);
            fs::create_dir_all(&path).unwrap();
            Self(path)
        }

        fn write(&self, path: &str, value: &str) {
            let path = self.0.join(path);
            fs::create_dir_all(path.parent().unwrap()).unwrap();
            fs::write(path, value).unwrap();
        }

        fn dir(&self, path: &str) {
            fs::create_dir_all(self.0.join(path)).unwrap();
        }

        fn cpu(&self, cpu: usize, package: &str, core: &str, node: Option<usize>) {
            let base = format!("sys/devices/system/cpu/cpu{cpu}");
            self.write(&format!("{base}/topology/physical_package_id"), package);
            self.write(&format!("{base}/topology/core_id"), core);
            if let Some(node) = node {
                self.dir(&format!("{base}/node{node}"));
            }
        }

        /// A two-node, four-CPU machine in a cgroup v2 hierarchy.
        fn machine() -> Self {
            let tree = Self::new();
            tree.write("sys/devices/system/cpu/online", "0-3\n");
            tree.cpu(0, "0", "0", Some(0));
            tree.cpu(1, "0", "0", Some(0));
            tree.cpu(2, "0", "1", Some(1));
            tree.cpu(3, "-1", "-1", None);
            tree.write("proc/thread-self/cgroup", "0::/tenant/leaf\n");
            tree.write(
                "proc/self/mountinfo",
                "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
            );
            tree.dir("sys/fs/cgroup/tenant/leaf");
            tree
        }

        fn discover(&self, allowed: &[usize]) -> Result<Topology> {
            Topology::discover_at(&self.0, &allowed.iter().copied().collect())
        }
    }

    impl Drop for Tree {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    #[test]
    #[cfg_attr(miri, ignore = "filesystem access is not supported by Miri isolation")]
    fn discovers_cpus_with_unknown_ids() {
        let tree = Tree::machine();
        let topology = tree.discover(&[0, 1, 2, 3, 9]).unwrap();
        assert_eq!(topology.cpu_set(), [0, 1, 2, 3].into_iter().collect());
        assert_eq!(
            topology.cpus[1],
            CpuLocation {
                cpu: 1,
                package: Some(0),
                die: None,
                core: Some(0),
                numa_node: Some(0),
            }
        );
        assert_eq!(topology.cpus[2].numa_node, Some(1));
        // -1 identifiers and a missing node link are unknown, not errors.
        assert_eq!(topology.cpus[3], CpuLocation::new(3));
        assert_eq!(topology.quota, None);
        assert!(topology.devices.is_empty());
    }

    #[test]
    #[cfg_attr(miri, ignore = "filesystem access is not supported by Miri isolation")]
    fn ancestor_cpusets_and_quotas_apply() {
        let tree = Tree::machine();
        tree.write("sys/fs/cgroup/cpu.max", "max 100000\n");
        tree.write("sys/fs/cgroup/tenant/cpu.max", "150000 100000\n");
        tree.write("sys/fs/cgroup/tenant/cpuset.cpus.effective", "0-2\n");
        tree.write("sys/fs/cgroup/tenant/leaf/cpu.max", "400000 100000\n");
        // Empty means "inherit" and must not empty the set.
        tree.write("sys/fs/cgroup/tenant/leaf/cpuset.cpus", "\n");
        tree.write("sys/fs/cgroup/tenant/leaf/cpuset.cpus.effective", "1-3\n");
        let topology = tree.discover(&[0, 1, 2, 3]).unwrap();
        assert_eq!(topology.cpu_set(), [1, 2].into_iter().collect());
        let quota = topology.quota.unwrap();
        assert_eq!((quota.quota.get(), quota.period.get()), (150000, 100000));
        assert_eq!(quota.whole_cpus(), 1);
    }

    #[test]
    #[cfg_attr(miri, ignore = "filesystem access is not supported by Miri isolation")]
    fn v1_hierarchies_apply() {
        let tree = Tree::machine();
        tree.write(
            "proc/thread-self/cgroup",
            "3:cpu,cpuacct:/job\n2:cpuset:/job\n1:memory:/job\n",
        );
        tree.write(
            "proc/self/mountinfo",
            "1 0 0:1 / /cg/cpu rw - cgroup cgroup rw,cpu,cpuacct\n\
             2 0 0:2 / /cg/cpuset rw - cgroup cgroup rw,cpuset\n",
        );
        tree.write("cg/cpu/job/cpu.cfs_quota_us", "200000\n");
        tree.write("cg/cpu/job/cpu.cfs_period_us", "100000\n");
        tree.write("cg/cpu/cpu.cfs_quota_us", "-1\n");
        tree.write("cg/cpu/cpu.cfs_period_us", "100000\n");
        tree.write("cg/cpuset/job/cpuset.cpus", "2-3\n");
        let topology = tree.discover(&[0, 1, 2, 3]).unwrap();
        assert_eq!(topology.cpu_set(), [2, 3].into_iter().collect());
        assert_eq!(topology.quota.unwrap().whole_cpus(), 2);

        fs::remove_file(tree.0.join("cg/cpu/job/cpu.cfs_period_us")).unwrap();
        let error = tree.discover(&[0, 1, 2, 3]).unwrap_err();
        assert_eq!(error.kind(), ErrorKind::InvalidConfiguration);
    }

    #[test]
    #[cfg_attr(miri, ignore = "filesystem access is not supported by Miri isolation")]
    fn unresolvable_membership_and_empty_results_fail_closed() {
        let tree = Tree::machine();
        // Allowed CPUs that are all offline leave nothing usable.
        let error = tree.discover(&[7]).unwrap_err();
        assert_eq!(error.kind(), ErrorKind::InvalidConfiguration);

        // A membership whose directory is missing is not "unlimited".
        fs::remove_dir(tree.0.join("sys/fs/cgroup/tenant/leaf")).unwrap();
        let error = tree.discover(&[0]).unwrap_err();
        assert_eq!(error.kind(), ErrorKind::NotFound);

        // A hidden hierarchy fails closed.
        tree.write("proc/self/mountinfo", "");
        let error = tree.discover(&[0]).unwrap_err();
        assert_eq!(error.kind(), ErrorKind::InvalidConfiguration);
    }

    #[test]
    #[cfg_attr(miri, ignore = "filesystem access is not supported by Miri isolation")]
    fn device_discovery_skips_controls_and_follows_links() {
        let tree = Tree::machine();
        tree.write("devices/pci0/nic0/device/numa_node", "1\n");
        tree.write("devices/pci0/nic1/device/numa_node", "-1\n");
        tree.dir("sys/class/net/virtual0");
        tree.write("sys/class/net/bonding_masters", "bond0\n");
        let class = tree.0.join("sys/class/net");
        std::os::unix::fs::symlink(tree.0.join("devices/pci0/nic0"), class.join("eth0")).unwrap();
        std::os::unix::fs::symlink(tree.0.join("devices/pci0/nic1"), class.join("eth1")).unwrap();
        // A dangling link behaves like a device removed during the scan.
        std::os::unix::fs::symlink(tree.0.join("devices/gone"), class.join("eth9")).unwrap();
        let ib = tree.0.join("sys/class/infiniband");
        fs::create_dir_all(&ib).unwrap();
        std::os::unix::fs::symlink(tree.0.join("devices/pci0/nic0"), ib.join("ib0")).unwrap();

        let devices = tree.discover(&[0]).unwrap().devices;
        let summary: Vec<_> = devices
            .iter()
            .map(|d| (d.class.as_str(), d.name.as_str(), d.numa_node))
            .collect();
        assert_eq!(
            summary,
            [
                ("net", "eth0", Some(1)),
                ("net", "eth1", None),
                ("net", "virtual0", None),
                ("infiniband", "ib0", Some(1)),
            ]
        );
    }

    #[test]
    #[cfg_attr(miri, ignore = "filesystem access is not supported by Miri isolation")]
    fn malformed_files_are_rejected() {
        let tree = Tree::machine();
        tree.write("sys/devices/system/cpu/cpu0/topology/core_id", "zero\n");
        let error = tree.discover(&[0]).unwrap_err();
        assert_eq!(error.kind(), ErrorKind::InvalidConfiguration);

        let tree = Tree::machine();
        tree.write("sys/devices/system/cpu/online", "3-1\n");
        assert!(tree.discover(&[0]).is_err());

        let tree = Tree::machine();
        fs::remove_file(tree.0.join("sys/devices/system/cpu/online")).unwrap();
        assert_eq!(tree.discover(&[0]).unwrap_err().kind(), ErrorKind::NotFound);
    }

    #[test]
    #[cfg_attr(miri, ignore = "affinity syscalls are not supported by Miri")]
    fn real_discovery_matches_affinity() {
        std::thread::spawn(|| {
            let allowed = current_cpus().unwrap();
            let cpu = allowed.first().unwrap();
            super::super::pin_cpu(cpu).unwrap();
            let topology = Topology::discover().unwrap();
            assert_eq!(topology.cpu_set(), CpuSet::from_iter([cpu]));
        })
        .join()
        .unwrap();
    }
}
