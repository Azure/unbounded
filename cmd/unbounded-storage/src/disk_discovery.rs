// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::collections::{BTreeSet, HashSet};
use std::fmt;
use std::fs;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::path::{Path, PathBuf};

use crate::config::{self, BlockDiskConfig, Config, DiskDiscoveryCfg, DiskSpec};

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct DeviceId {
    pub major: u32,
    pub minor: u32,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AutoDisk {
    pub path: String,
    pub numa: Option<u16>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ExclusionReason {
    Denied,
    Mounted,
    Swap,
    Held,
    ZeroCapacity,
    DeviceIdentityMismatch,
    SafetyStateUnavailable,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ExcludedDisk {
    pub path: String,
    pub reason: ExclusionReason,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct DiscoveryReport {
    pub eligible: Vec<AutoDisk>,
    pub excluded: Vec<ExcludedDisk>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DiskSource {
    Explicit,
    Automatic,
    Fallback,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct DiskResolution {
    pub source: DiskSource,
    pub report: Option<DiscoveryReport>,
}

#[derive(Default)]
pub struct DiskResolver {
    cached: Option<CachedResolution>,
}

impl DiskResolver {
    pub fn resolve(&mut self, config: &mut Config) -> Result<DiskResolution, DiscoveryError> {
        self.resolve_with(config, discover)
    }

    pub fn resolve_with(
        &mut self,
        config: &mut Config,
        mut scan: impl FnMut(&[String]) -> Result<DiscoveryReport, DiscoveryError>,
    ) -> Result<DiskResolution, DiscoveryError> {
        if !config.disks.is_empty() {
            self.cached = None;
            return Ok(DiskResolution {
                source: DiskSource::Explicit,
                report: None,
            });
        }

        let policy = config.disk_discovery().clone();
        if let Some(cached) = self
            .cached
            .as_ref()
            .filter(|cached| cached.policy == policy)
        {
            config.disks.clone_from(&cached.disks);
            return Ok(cached.resolution.clone());
        }

        let report = scan(&policy.denied_paths)?;
        let (source, disks) = if report.eligible.is_empty() {
            (DiskSource::Fallback, vec![fallback_spec(&policy)])
        } else {
            (
                DiskSource::Automatic,
                report.eligible.iter().map(block_spec).collect(),
            )
        };
        let resolution = DiskResolution {
            source,
            report: Some(report),
        };
        config.disks.clone_from(&disks);
        self.cached = Some(CachedResolution {
            policy,
            disks,
            resolution: resolution.clone(),
        });
        Ok(resolution)
    }
}

struct CachedResolution {
    policy: DiskDiscoveryCfg,
    disks: Vec<DiskSpec>,
    resolution: DiskResolution,
}

#[derive(Clone, Debug)]
pub struct DiscoveryRoots {
    pub sys: PathBuf,
    pub proc: PathBuf,
    pub dev: PathBuf,
}

impl DiscoveryRoots {
    pub fn host() -> Self {
        Self {
            sys: PathBuf::from("/sys"),
            proc: PathBuf::from("/proc"),
            dev: PathBuf::from("/dev"),
        }
    }
}

#[derive(Debug)]
pub enum DiscoveryError {
    ReadBlockDevices(io::Error),
    ReadMountInfo(io::Error),
    ReadSwaps(io::Error),
    InvalidMountInfo(String),
    InvalidSwapDevice(String),
}

impl fmt::Display for DiscoveryError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::ReadBlockDevices(error) => write!(f, "read block devices: {error}"),
            Self::ReadMountInfo(error) => write!(f, "read mount information: {error}"),
            Self::ReadSwaps(error) => write!(f, "read swap information: {error}"),
            Self::InvalidMountInfo(line) => write!(f, "invalid mountinfo entry: {line}"),
            Self::InvalidSwapDevice(path) => write!(f, "cannot identify swap device: {path}"),
        }
    }
}

impl std::error::Error for DiscoveryError {}

pub trait DeviceProbe {
    fn identity(&self, path: &Path) -> io::Result<Option<DeviceId>>;

    fn backing_identity(&self, path: &Path) -> io::Result<DeviceId>;
}

pub struct SystemDeviceProbe;

impl DeviceProbe for SystemDeviceProbe {
    fn identity(&self, path: &Path) -> io::Result<Option<DeviceId>> {
        let metadata = fs::metadata(path)?;
        if !metadata.file_type().is_block_device() {
            return Ok(None);
        }
        let device = metadata.rdev();
        Ok(Some(DeviceId {
            major: libc::major(device) as u32,
            minor: libc::minor(device) as u32,
        }))
    }

    fn backing_identity(&self, path: &Path) -> io::Result<DeviceId> {
        let device = fs::metadata(path)?.dev();
        Ok(DeviceId {
            major: libc::major(device) as u32,
            minor: libc::minor(device) as u32,
        })
    }
}

#[derive(Debug)]
struct BlockEntry {
    name: String,
    id: Option<DeviceId>,
    partition: bool,
}

pub fn discover(denied_paths: &[String]) -> Result<DiscoveryReport, DiscoveryError> {
    discover_with(DiscoveryRoots::host(), &SystemDeviceProbe, denied_paths)
}

pub fn discover_with(
    roots: DiscoveryRoots,
    device_probe: &impl DeviceProbe,
    denied_paths: &[String],
) -> Result<DiscoveryReport, DiscoveryError> {
    let mount_ids = read_mount_ids(&roots.proc.join("self/mountinfo"))?;
    let swap_ids = read_swap_ids(&roots, device_probe)?;
    let block_entries = read_block_entries(&roots.sys.join("class/block"))?;
    let denied = denied_paths
        .iter()
        .map(String::as_str)
        .collect::<HashSet<_>>();
    let mut report = DiscoveryReport::default();

    for entry in block_entries
        .iter()
        .filter(|entry| !entry.partition && nvme_controller(&entry.name).is_some())
    {
        let path = format!("/dev/{}", entry.name);
        let descendants = block_entries
            .iter()
            .filter(|child| child.partition && is_partition_of(&child.name, &entry.name))
            .collect::<Vec<_>>();
        let mut identities = descendants
            .iter()
            .filter_map(|child| child.id)
            .collect::<BTreeSet<_>>();
        if let Some(id) = entry.id {
            identities.insert(id);
        }
        let sectors = read_sectors(&roots.sys.join("class/block").join(&entry.name).join("size"));

        let reason = if denied.contains(path.as_str()) {
            Some(ExclusionReason::Denied)
        } else if entry.id.is_none() || descendants.iter().any(|child| child.id.is_none()) {
            Some(ExclusionReason::SafetyStateUnavailable)
        } else if sectors == Some(0) {
            Some(ExclusionReason::ZeroCapacity)
        } else if device_probe
            .identity(&roots.dev.join(&entry.name))
            .ok()
            .flatten()
            != entry.id
        {
            Some(ExclusionReason::DeviceIdentityMismatch)
        } else if identities.iter().any(|id| mount_ids.contains(id)) {
            Some(ExclusionReason::Mounted)
        } else if identities.iter().any(|id| swap_ids.contains(id)) {
            Some(ExclusionReason::Swap)
        } else if has_holders(&roots.sys, &entry.name)
            || descendants
                .iter()
                .any(|child| has_holders(&roots.sys, &child.name))
        {
            Some(ExclusionReason::Held)
        } else if sectors.is_none() {
            Some(ExclusionReason::SafetyStateUnavailable)
        } else {
            None
        };

        if let Some(reason) = reason {
            report.excluded.push(ExcludedDisk { path, reason });
            continue;
        }

        let controller = nvme_controller(&entry.name).expect("validated namespace name");
        report.eligible.push(AutoDisk {
            path,
            numa: read_numa(
                &roots
                    .sys
                    .join("class/nvme")
                    .join(controller)
                    .join("device/numa_node"),
            ),
        });
    }

    report.eligible.sort_by(|a, b| a.path.cmp(&b.path));
    report.excluded.sort_by(|a, b| a.path.cmp(&b.path));
    Ok(report)
}

fn read_block_entries(path: &Path) -> Result<Vec<BlockEntry>, DiscoveryError> {
    let entries = fs::read_dir(path).map_err(DiscoveryError::ReadBlockDevices)?;
    let mut blocks = Vec::new();
    for entry in entries.flatten() {
        let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
            continue;
        };
        blocks.push(BlockEntry {
            id: fs::read_to_string(entry.path().join("dev"))
                .ok()
                .and_then(|value| parse_device_id(value.trim())),
            partition: entry.path().join("partition").exists(),
            name,
        });
    }
    blocks.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(blocks)
}

fn read_mount_ids(path: &Path) -> Result<HashSet<DeviceId>, DiscoveryError> {
    let contents = fs::read_to_string(path).map_err(DiscoveryError::ReadMountInfo)?;
    contents
        .lines()
        .map(|line| {
            line.split_whitespace()
                .nth(2)
                .and_then(parse_device_id)
                .ok_or_else(|| DiscoveryError::InvalidMountInfo(line.to_string()))
        })
        .collect()
}

fn read_swap_ids(
    roots: &DiscoveryRoots,
    device_probe: &impl DeviceProbe,
) -> Result<HashSet<DeviceId>, DiscoveryError> {
    let contents =
        fs::read_to_string(roots.proc.join("swaps")).map_err(DiscoveryError::ReadSwaps)?;
    let mut identities = HashSet::new();
    for line in contents.lines().skip(1) {
        let Some(path) = line.split_whitespace().next() else {
            continue;
        };
        let swap_path = Path::new(path);
        let rooted = swap_path
            .strip_prefix("/dev")
            .map(|relative| roots.dev.join(relative))
            .unwrap_or_else(|_| roots.proc.join("root").join(path.trim_start_matches('/')));
        let id = match device_probe.identity(&rooted) {
            Ok(Some(id)) => Ok(id),
            Ok(None) => device_probe.backing_identity(&rooted),
            Err(error) => Err(error),
        }
        .map_err(|_| DiscoveryError::InvalidSwapDevice(path.to_string()))?;
        identities.insert(id);
    }
    Ok(identities)
}

fn has_holders(sys_root: &Path, name: &str) -> bool {
    fs::read_dir(sys_root.join("class/block").join(name).join("holders"))
        .map(|mut entries| entries.next().is_some())
        .unwrap_or(true)
}

fn read_sectors(path: &Path) -> Option<u64> {
    fs::read_to_string(path).ok()?.trim().parse().ok()
}

fn read_numa(path: &Path) -> Option<u16> {
    let value = fs::read_to_string(path).ok()?.trim().parse::<i32>().ok()?;
    u16::try_from(value).ok()
}

fn parse_device_id(value: &str) -> Option<DeviceId> {
    let (major, minor) = value.split_once(':')?;
    Some(DeviceId {
        major: major.parse().ok()?,
        minor: minor.parse().ok()?,
    })
}

fn nvme_controller(name: &str) -> Option<&str> {
    let suffix = name.strip_prefix("nvme")?;
    let controller_digits = suffix.chars().take_while(char::is_ascii_digit).count();
    if controller_digits == 0 {
        return None;
    }
    let namespace = suffix.get(controller_digits..)?.strip_prefix('n')?;
    if namespace.is_empty() || !namespace.chars().all(|c| c.is_ascii_digit()) {
        return None;
    }
    name.get(.."nvme".len() + controller_digits)
}

fn is_partition_of(name: &str, namespace: &str) -> bool {
    name.strip_prefix(namespace)
        .and_then(|suffix| suffix.strip_prefix('p'))
        .is_some_and(|number| !number.is_empty() && number.chars().all(|c| c.is_ascii_digit()))
}

fn block_spec(disk: &AutoDisk) -> DiskSpec {
    DiskSpec {
        config: Some(config::disk_spec::Config::Block(BlockDiskConfig {
            path: disk.path.clone(),
            numa: disk.numa.map(u32::from),
        })),
        ..DiskSpec::default()
    }
}

fn fallback_spec(policy: &DiskDiscoveryCfg) -> DiskSpec {
    DiskSpec {
        config: Some(config::disk_spec::Config::File(
            policy
                .fallback
                .clone()
                .expect("fallback populated by Config::apply_defaults"),
        )),
        ..DiskSpec::default()
    }
}

#[cfg(test)]
mod tests {
    use std::cell::Cell;
    use std::collections::HashMap;
    use std::fs;
    use std::io;
    use std::path::{Path, PathBuf};

    use super::*;

    struct Fixture {
        root: tempfile::TempDir,
        devices: HashMap<PathBuf, DeviceId>,
    }

    impl Fixture {
        fn new() -> Self {
            let fixture = Self {
                root: tempfile::tempdir().expect("create fixture"),
                devices: HashMap::new(),
            };
            fixture.write("proc/self/mountinfo", "");
            fixture.write("proc/swaps", "Filename\tType\tSize\tUsed\tPriority\n");
            fixture
        }

        fn roots(&self) -> DiscoveryRoots {
            DiscoveryRoots {
                sys: self.root.path().join("sys"),
                proc: self.root.path().join("proc"),
                dev: self.root.path().join("dev"),
            }
        }

        fn write(&self, relative: &str, contents: &str) {
            let path = self.root.path().join(relative);
            fs::create_dir_all(path.parent().expect("fixture file parent"))
                .expect("create fixture directory");
            fs::write(path, contents).expect("write fixture file");
        }

        fn add_namespace(&mut self, name: &str, id: DeviceId, sectors: u64, numa: i16) {
            self.write(
                &format!("sys/class/block/{name}/dev"),
                &format!("{}:{}\n", id.major, id.minor),
            );
            self.write(
                &format!("sys/class/block/{name}/size"),
                &format!("{sectors}\n"),
            );
            fs::create_dir_all(
                self.root
                    .path()
                    .join(format!("sys/class/block/{name}/holders")),
            )
            .expect("create holders directory");
            let controller = nvme_controller(name).expect("NVMe namespace name");
            self.write(
                &format!("sys/class/nvme/{controller}/device/numa_node"),
                &format!("{numa}\n"),
            );
            let device_path = self.root.path().join("dev").join(name);
            self.write(&format!("dev/{name}"), "");
            self.devices.insert(device_path, id);
        }

        fn add_partition(&mut self, namespace: &str, number: u16, id: DeviceId) -> String {
            let name = format!("{namespace}p{number}");
            self.write(
                &format!("sys/class/block/{name}/partition"),
                &format!("{number}\n"),
            );
            self.write(
                &format!("sys/class/block/{name}/dev"),
                &format!("{}:{}\n", id.major, id.minor),
            );
            fs::create_dir_all(
                self.root
                    .path()
                    .join(format!("sys/class/block/{name}/holders")),
            )
            .expect("create partition holders directory");
            let device_path = self.root.path().join("dev").join(&name);
            self.write(&format!("dev/{name}"), "");
            self.devices.insert(device_path, id);
            name
        }

        fn add_holder(&self, device: &str, holder: &str) {
            fs::create_dir_all(
                self.root
                    .path()
                    .join(format!("sys/class/block/{device}/holders/{holder}")),
            )
            .expect("create holder");
        }
    }

    impl DeviceProbe for Fixture {
        fn identity(&self, path: &Path) -> io::Result<Option<DeviceId>> {
            Ok(self.devices.get(path).copied())
        }

        fn backing_identity(&self, path: &Path) -> io::Result<DeviceId> {
            self.devices
                .get(path)
                .copied()
                .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "unknown backing device"))
        }
    }

    fn id(major: u32, minor: u32) -> DeviceId {
        DeviceId { major, minor }
    }

    #[test]
    fn discovers_safe_nvme_namespaces_in_path_order() {
        let mut fixture = Fixture::new();
        fixture.add_namespace("nvme10n2", id(259, 2), 16, -1);
        fixture.add_namespace("nvme2n1", id(259, 1), 8, 3);
        fixture.add_partition("nvme2n1", 1, id(259, 3));
        fixture.write("sys/class/block/sda/dev", "8:0\n");

        let report = discover_with(fixture.roots(), &fixture, &[]).expect("discover disks");

        assert_eq!(
            report.eligible,
            vec![
                AutoDisk {
                    path: "/dev/nvme10n2".to_string(),
                    numa: None,
                },
                AutoDisk {
                    path: "/dev/nvme2n1".to_string(),
                    numa: Some(3),
                },
            ]
        );
        assert!(report.excluded.is_empty());
    }

    #[test]
    fn excludes_mounted_swap_held_denied_and_invalid_namespaces() {
        let mut fixture = Fixture::new();
        for index in 0..8 {
            fixture.add_namespace(
                &format!("nvme{index}n1"),
                id(259, index),
                if index == 7 { 0 } else { 8 },
                0,
            );
        }
        let mounted_partition = fixture.add_partition("nvme2n1", 1, id(259, 20));
        let swap_partition = fixture.add_partition("nvme3n1", 1, id(259, 30));
        fixture.add_partition("nvme6n1", 1, id(259, 60));
        fixture.add_holder("nvme4n1", "dm-0");
        fixture
            .devices
            .insert(fixture.root.path().join("dev/nvme6n1"), id(259, 99));
        fixture.write(
            "proc/self/mountinfo",
            &format!(
                "20 1 259:1 / /mnt/direct rw - ext4 /dev/nvme1n1 rw\n21 1 259:20 / /mnt/partition rw - ext4 /dev/{mounted_partition} rw\n"
            ),
        );
        fixture.write(
            "proc/swaps",
            &format!(
                "Filename\tType\tSize\tUsed\tPriority\n/dev/{swap_partition}\tpartition\t1024\t0\t-2\n"
            ),
        );

        let report = discover_with(fixture.roots(), &fixture, &["/dev/nvme5n1".to_string()])
            .expect("discover disks");

        assert_eq!(
            report.eligible,
            vec![AutoDisk {
                path: "/dev/nvme0n1".to_string(),
                numa: Some(0),
            }]
        );
        assert_eq!(
            report
                .excluded
                .iter()
                .map(|disk| (disk.path.as_str(), disk.reason))
                .collect::<Vec<_>>(),
            vec![
                ("/dev/nvme1n1", ExclusionReason::Mounted),
                ("/dev/nvme2n1", ExclusionReason::Mounted),
                ("/dev/nvme3n1", ExclusionReason::Swap),
                ("/dev/nvme4n1", ExclusionReason::Held),
                ("/dev/nvme5n1", ExclusionReason::Denied),
                ("/dev/nvme6n1", ExclusionReason::DeviceIdentityMismatch),
                ("/dev/nvme7n1", ExclusionReason::ZeroCapacity),
            ]
        );
    }

    #[test]
    fn swap_file_excludes_its_backing_namespace() {
        let mut fixture = Fixture::new();
        fixture.add_namespace("nvme0n1", id(259, 0), 8, 0);
        let swap_file = fixture.root.path().join("proc/root/swapfile");
        fixture.write("proc/root/swapfile", "");
        fixture.devices.insert(swap_file, id(259, 0));
        fixture.write(
            "proc/swaps",
            "Filename\tType\tSize\tUsed\tPriority\n/swapfile\tfile\t1024\t0\t-2\n",
        );

        let report = discover_with(fixture.roots(), &fixture, &[]).expect("discover disks");

        assert!(report.eligible.is_empty());
        assert_eq!(report.excluded[0].reason, ExclusionReason::Swap);
    }

    #[test]
    fn unresolved_swap_file_fails_closed() {
        let mut fixture = Fixture::new();
        fixture.add_namespace("nvme0n1", id(259, 0), 8, 0);
        fixture.write(
            "proc/swaps",
            "Filename\tType\tSize\tUsed\tPriority\n/missing.swap\tfile\t1024\t0\t-2\n",
        );

        assert!(matches!(
            discover_with(fixture.roots(), &fixture, &[]),
            Err(DiscoveryError::InvalidSwapDevice(path)) if path == "/missing.swap"
        ));
    }

    #[test]
    fn partition_holders_exclude_the_namespace() {
        let mut fixture = Fixture::new();
        fixture.add_namespace("nvme0n1", id(259, 0), 8, 0);
        let partition = fixture.add_partition("nvme0n1", 1, id(259, 1));
        fixture.add_holder(&partition, "md0");

        let report = discover_with(fixture.roots(), &fixture, &[]).expect("discover disks");

        assert!(report.eligible.is_empty());
        assert_eq!(report.excluded[0].reason, ExclusionReason::Held);
    }

    #[test]
    fn unreadable_partition_identity_excludes_the_namespace() {
        let mut fixture = Fixture::new();
        fixture.add_namespace("nvme0n1", id(259, 0), 8, 0);
        let partition = fixture.add_partition("nvme0n1", 1, id(259, 1));
        fs::remove_file(
            fixture
                .root
                .path()
                .join(format!("sys/class/block/{partition}/dev")),
        )
        .expect("remove partition identity");

        let report = discover_with(fixture.roots(), &fixture, &[]).expect("discover disks");

        assert!(report.eligible.is_empty());
        assert_eq!(
            report.excluded[0].reason,
            ExclusionReason::SafetyStateUnavailable
        );
    }

    #[test]
    fn fails_closed_when_usage_state_cannot_be_read() {
        let mut fixture = Fixture::new();
        fixture.add_namespace("nvme0n1", id(259, 0), 8, 0);
        fs::remove_file(fixture.root.path().join("proc/self/mountinfo")).expect("remove mountinfo");

        assert!(matches!(
            discover_with(fixture.roots(), &fixture, &[]),
            Err(DiscoveryError::ReadMountInfo(_))
        ));
    }

    #[test]
    fn materializer_preserves_explicit_disks_without_scanning() {
        let mut config = default_config();
        config.disks.push(block_spec("/dev/nvme9n1", Some(9)));
        config.disk_discovery.as_mut().unwrap().denied_paths = vec!["/dev/nvme9n1".into()];
        let mut scans = 0;
        let mut resolver = DiskResolver::default();

        let resolution = resolver
            .resolve_with(&mut config, |_| {
                scans += 1;
                Ok(DiscoveryReport::default())
            })
            .expect("resolve explicit disks");

        assert_eq!(resolution.source, DiskSource::Explicit);
        assert_eq!(scans, 0);
        assert_eq!(config.disks, vec![block_spec("/dev/nvme9n1", Some(9))]);
    }

    #[test]
    fn materializer_uses_all_discovered_disks_or_fallback() {
        let mut resolver = DiskResolver::default();
        let mut auto_config = default_config();
        let report = DiscoveryReport {
            eligible: vec![
                AutoDisk {
                    path: "/dev/nvme0n1".into(),
                    numa: Some(2),
                },
                AutoDisk {
                    path: "/dev/nvme1n1".into(),
                    numa: None,
                },
            ],
            excluded: Vec::new(),
        };

        let resolution = resolver
            .resolve_with(&mut auto_config, |_| Ok(report.clone()))
            .expect("resolve automatic disks");

        assert_eq!(resolution.source, DiskSource::Automatic);
        assert_eq!(resolution.report, Some(report));
        assert_eq!(
            auto_config.disks,
            vec![
                block_spec("/dev/nvme0n1", Some(2)),
                block_spec("/dev/nvme1n1", None),
            ]
        );

        let mut fallback_config = default_config();
        fallback_config
            .disk_discovery
            .as_mut()
            .unwrap()
            .fallback
            .as_mut()
            .unwrap()
            .path = "/var/cache/custom.disk".into();
        let resolution = resolver
            .resolve_with(&mut fallback_config, |_| Ok(DiscoveryReport::default()))
            .expect("resolve fallback disk");

        assert_eq!(resolution.source, DiskSource::Fallback);
        assert_eq!(
            fallback_config.disks,
            vec![file_spec("/var/cache/custom.disk", 20 * 1024 * 1024 * 1024)]
        );
    }

    #[test]
    fn materializer_reuses_unchanged_policy_and_rescans_after_explicit_config() {
        let mut resolver = DiskResolver::default();
        let scans = Cell::new(0);
        let mut scan = |_: &[String]| {
            scans.set(scans.get() + 1);
            Ok(DiscoveryReport {
                eligible: vec![AutoDisk {
                    path: format!("/dev/nvme{}n1", scans.get()),
                    numa: None,
                }],
                excluded: Vec::new(),
            })
        };

        let mut first = default_config();
        resolver.resolve_with(&mut first, &mut scan).unwrap();
        let mut unchanged = default_config();
        resolver.resolve_with(&mut unchanged, &mut scan).unwrap();
        assert_eq!(scans.get(), 1);
        assert_eq!(unchanged.disks, first.disks);

        let mut changed = default_config();
        changed.disk_discovery.as_mut().unwrap().denied_paths = vec!["/dev/nvme0n1".into()];
        resolver.resolve_with(&mut changed, &mut scan).unwrap();
        assert_eq!(scans.get(), 2);

        let mut explicit = default_config();
        explicit.disks.push(block_spec("/dev/nvme9n1", None));
        resolver.resolve_with(&mut explicit, &mut scan).unwrap();
        assert_eq!(scans.get(), 2);

        let mut auto_again = default_config();
        resolver.resolve_with(&mut auto_again, &mut scan).unwrap();
        assert_eq!(scans.get(), 3);
    }

    fn default_config() -> crate::config::Config {
        let mut config = crate::config::Config::default();
        config.apply_defaults();
        config
    }

    fn block_spec(path: &str, numa: Option<u32>) -> crate::config::DiskSpec {
        crate::config::DiskSpec {
            config: Some(crate::config::disk_spec::Config::Block(
                crate::config::BlockDiskConfig {
                    numa,
                    path: path.into(),
                },
            )),
            ..Default::default()
        }
    }

    fn file_spec(path: &str, size: u64) -> crate::config::DiskSpec {
        crate::config::DiskSpec {
            config: Some(crate::config::disk_spec::Config::File(
                crate::config::FileDiskConfig {
                    path: path.into(),
                    size: Some(size),
                },
            )),
            ..Default::default()
        }
    }
}
