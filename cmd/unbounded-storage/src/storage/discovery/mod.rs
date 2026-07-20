// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Safe automatic discovery of completely unused whole block devices.

mod blkid;

use std::collections::HashSet;
use std::fs;
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::path::{Path, PathBuf};
use std::sync::mpsc;

use crate::config::LoadedConfig;
use crate::config::RuntimeDisk;
use crate::config::schema::{BlockDiskConfig, DiskDiscovery, DiskSpec, disk_spec};
use notify::{RecommendedWatcher, RecursiveMode, Watcher};

pub use blkid::{ProbeResult, probe_fd};

#[derive(Debug, Clone)]
pub struct DiscoveryRoots {
    pub dev: PathBuf,
    pub sys_class_block: PathBuf,
    pub mountinfo: PathBuf,
    pub swaps: PathBuf,
}

impl Default for DiscoveryRoots {
    fn default() -> Self {
        Self {
            dev: PathBuf::from("/dev"),
            sys_class_block: PathBuf::from("/sys/class/block"),
            mountinfo: PathBuf::from("/proc/self/mountinfo"),
            swaps: PathBuf::from("/proc/swaps"),
        }
    }
}

#[derive(Debug, Clone)]
pub struct DiskScanner {
    roots: DiscoveryRoots,
}

impl Default for DiskScanner {
    fn default() -> Self {
        Self::new(DiscoveryRoots::default())
    }
}

impl DiskScanner {
    pub fn new(roots: DiscoveryRoots) -> Self {
        Self { roots }
    }

    pub fn scan(
        &self,
        policy: &DiskDiscovery,
        active: &[RuntimeDisk],
    ) -> Result<Vec<RuntimeDisk>, String> {
        let mounted = parse_mountinfo(
            &fs::read_to_string(&self.roots.mountinfo)
                .map_err(|e| format!("read {}: {e}", self.roots.mountinfo.display()))?,
        );
        let swaps = self.swap_identities(
            &fs::read_to_string(&self.roots.swaps)
                .map_err(|e| format!("read {}: {e}", self.roots.swaps.display()))?,
        )?;
        let denied = self.denied_identities(&policy.deny_paths);
        let active: HashSet<DeviceId> = active
            .iter()
            .filter(|disk| disk.exclusive)
            .filter_map(|disk| disk.spec.path())
            .filter_map(|path| device_identity(Path::new(path)))
            .collect();
        let mut disks = Vec::new();

        let entries = fs::read_dir(&self.roots.sys_class_block)
            .map_err(|e| format!("read {}: {e}", self.roots.sys_class_block.display()))?;
        for entry in entries {
            let Ok(entry) = entry else { continue };
            if let Some(disk) = self.eligible(entry.path(), &mounted, &swaps, &denied, &active) {
                disks.push(disk);
            }
        }
        disks.sort_by(|a, b| a.spec.path().cmp(&b.spec.path()));
        Ok(disks)
    }

    fn eligible(
        &self,
        sys_path: PathBuf,
        mounted: &HashSet<DeviceId>,
        swaps: &HashSet<DeviceId>,
        denied: &HashSet<DeviceId>,
        active: &HashSet<DeviceId>,
    ) -> Option<RuntimeDisk> {
        let name = sys_path.file_name()?.to_str()?;
        if excluded_name(name) || sys_path.join("partition").exists() {
            return None;
        }
        if read_u64(sys_path.join("size"))? == 0
            || read_u64(sys_path.join("ro"))? != 0
            || read_u64(sys_path.join("removable"))? != 0
            || dir_nonempty(&sys_path.join("holders"))
            || dir_nonempty(&sys_path.join("slaves"))
            || has_partition_children(&sys_path)
        {
            return None;
        }

        let id = parse_device_id(&fs::read_to_string(sys_path.join("dev")).ok()?)?;
        if mounted.contains(&id) || swaps.contains(&id) || denied.contains(&id) {
            return None;
        }
        let path = self.roots.dev.join(name);
        if device_identity(&path)? != id {
            return None;
        }
        if !active.contains(&id) && blkid::probe_path(&path).ok()? != ProbeResult::Empty {
            return None;
        }

        let numa = fs::read_to_string(sys_path.join("device/numa_node"))
            .ok()
            .and_then(|v| v.trim().parse::<i32>().ok())
            .filter(|v| *v >= 0)
            .map(|v| v as u32);
        Some(RuntimeDisk::discovered(DiskSpec {
            config: Some(disk_spec::Config::Block(BlockDiskConfig {
                numa,
                path: path.to_string_lossy().into_owned(),
            })),
            ..Default::default()
        }))
    }

    fn denied_identities(&self, paths: &[String]) -> HashSet<DeviceId> {
        paths
            .iter()
            .filter_map(|path| device_identity(Path::new(path)))
            .collect()
    }

    fn swap_identities(&self, contents: &str) -> Result<HashSet<DeviceId>, String> {
        let mut out = HashSet::new();
        for line in contents.lines().skip(1) {
            let Some(path) = line.split_whitespace().next() else {
                continue;
            };
            if path.starts_with('/') {
                let metadata =
                    fs::metadata(path).map_err(|e| format!("identify swap path {path}: {e}"))?;
                if metadata.file_type().is_block_device() {
                    let rdev = metadata.rdev();
                    let id = DeviceId {
                        major: libc::major(rdev) as u64,
                        minor: libc::minor(rdev) as u64,
                    };
                    out.insert(id);
                }
            }
        }
        Ok(out)
    }
}

pub fn resolve_loaded(
    loaded: LoadedConfig,
    scanner: &DiskScanner,
    retained: Option<&[RuntimeDisk]>,
) -> (LoadedConfig, Option<String>) {
    let Some(policy) = loaded.config().disk_discovery.clone() else {
        return (loaded, None);
    };
    let active = retained.unwrap_or_default();
    match scanner.scan(&policy, active) {
        Ok(disks) if !disks.is_empty() => (loaded.with_runtime_disks(disks), None),
        Ok(_) => (
            loaded.with_runtime_disks(vec![fallback_disk(&policy)]),
            None,
        ),
        Err(e) => {
            let disks = retained
                .filter(|disks| !disks.is_empty())
                .map(<[RuntimeDisk]>::to_vec)
                .unwrap_or_else(|| vec![fallback_disk(&policy)]);
            (loaded.with_runtime_disks(disks), Some(e))
        }
    }
}

fn fallback_disk(policy: &DiskDiscovery) -> RuntimeDisk {
    RuntimeDisk::explicit(DiskSpec {
        config: Some(disk_spec::Config::File(
            policy
                .fallback
                .clone()
                .expect("discovery fallback populated by config defaults"),
        )),
        ..Default::default()
    })
}

pub struct DeviceWatcher {
    _watcher: RecommendedWatcher,
}

impl DeviceWatcher {
    pub fn new() -> Result<(Self, mpsc::Receiver<()>), notify::Error> {
        let (tx, rx) = mpsc::channel();
        let mut watcher =
            notify::recommended_watcher(move |event: Result<notify::Event, notify::Error>| {
                if event.is_ok() {
                    let _ = tx.send(());
                }
            })?;
        watcher.watch(Path::new("/dev"), RecursiveMode::NonRecursive)?;
        Ok((Self { _watcher: watcher }, rx))
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
struct DeviceId {
    major: u64,
    minor: u64,
}

fn parse_mountinfo(contents: &str) -> HashSet<DeviceId> {
    contents
        .lines()
        .filter_map(|line| line.split_whitespace().nth(2))
        .filter_map(parse_device_id)
        .collect()
}

fn parse_device_id(raw: &str) -> Option<DeviceId> {
    let (major, minor) = raw.trim().split_once(':')?;
    Some(DeviceId {
        major: major.parse().ok()?,
        minor: minor.parse().ok()?,
    })
}

fn device_identity(path: &Path) -> Option<DeviceId> {
    let metadata = fs::metadata(path).ok()?;
    if !metadata.file_type().is_block_device() {
        return None;
    }
    let rdev = metadata.rdev();
    Some(DeviceId {
        major: libc::major(rdev) as u64,
        minor: libc::minor(rdev) as u64,
    })
}

fn excluded_name(name: &str) -> bool {
    ["loop", "ram", "zram", "sr", "fd", "dm-", "md"]
        .iter()
        .any(|prefix| name.starts_with(prefix))
}

fn read_u64(path: PathBuf) -> Option<u64> {
    fs::read_to_string(path).ok()?.trim().parse().ok()
}

fn dir_nonempty(path: &Path) -> bool {
    fs::read_dir(path)
        .ok()
        .and_then(|mut entries| entries.next())
        .is_some()
}

fn has_partition_children(path: &Path) -> bool {
    fs::read_dir(path).ok().is_some_and(|entries| {
        entries
            .filter_map(Result::ok)
            .any(|entry| entry.path().join("partition").exists())
    })
}

#[cfg(test)]
mod tests {
    use std::fs::File;
    use std::io::Write;

    use tempfile::TempDir;

    use super::*;
    use crate::config::{Config, LoadedConfig};

    #[test]
    fn parses_mount_device_identities() {
        let ids = parse_mountinfo(
            "36 25 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n\
             37 25 0:32 / /proc rw,nosuid - proc proc rw\n",
        );
        assert!(ids.contains(&DeviceId { major: 8, minor: 1 }));
        assert!(ids.contains(&DeviceId {
            major: 0,
            minor: 32
        }));
    }

    #[test]
    fn excludes_synthetic_and_virtual_device_names() {
        for name in ["loop0", "ram0", "zram0", "sr0", "fd0", "dm-0", "md0"] {
            assert!(excluded_name(name), "{name}");
        }
        for name in ["sda", "vda", "nvme0n1"] {
            assert!(!excluded_name(name), "{name}");
        }
    }

    #[test]
    fn libblkid_classifies_empty_regular_file() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        file.write_all(&vec![0_u8; 1024 * 1024]).unwrap();
        file.flush().unwrap();
        assert_eq!(blkid::probe_path(file.path()).unwrap(), ProbeResult::Empty);
    }

    #[test]
    fn empty_scan_resolves_to_default_fallback() {
        let fixture = ScanFixture::new();
        let mut config = Config {
            disk_discovery: Some(DiskDiscovery::default()),
            ..Default::default()
        };
        config.apply_defaults();
        let loaded = LoadedConfig::from_config(config).unwrap();

        let (resolved, error) = resolve_loaded(loaded, &fixture.scanner(), None);

        assert!(error.is_none());
        assert_eq!(resolved.runtime().disks.len(), 1);
        let disk = &resolved.runtime().disks[0];
        assert!(!disk.exclusive);
        assert!(disk.spec.is_file());
        assert_eq!(
            disk.spec.path(),
            Some(crate::config::schema::DEFAULT_DISCOVERY_FALLBACK_PATH)
        );
    }

    #[test]
    fn scan_failure_retains_last_good_discovery_targets() {
        let fixture = ScanFixture::new();
        fs::remove_file(&fixture.mountinfo).unwrap();
        let mut config = Config {
            disk_discovery: Some(DiskDiscovery::default()),
            ..Default::default()
        };
        config.apply_defaults();
        let loaded = LoadedConfig::from_config(config).unwrap();
        let retained = vec![RuntimeDisk::discovered(DiskSpec {
            config: Some(disk_spec::Config::Block(BlockDiskConfig {
                numa: Some(1),
                path: "/dev/nvme9n1".to_string(),
            })),
            ..Default::default()
        })];

        let (resolved, error) = resolve_loaded(loaded, &fixture.scanner(), Some(&retained));

        assert!(error.is_some());
        assert_eq!(resolved.runtime().disks, retained);
    }

    struct ScanFixture {
        _temp: TempDir,
        dev: PathBuf,
        sys: PathBuf,
        mountinfo: PathBuf,
        swaps: PathBuf,
    }

    impl ScanFixture {
        fn new() -> Self {
            let temp = tempfile::tempdir().unwrap();
            let dev = temp.path().join("dev");
            let sys = temp.path().join("sys-class-block");
            let mountinfo = temp.path().join("mountinfo");
            let swaps = temp.path().join("swaps");
            fs::create_dir(&dev).unwrap();
            fs::create_dir(&sys).unwrap();
            File::create(&mountinfo).unwrap();
            fs::write(&swaps, "Filename Type Size Used Priority\n").unwrap();
            Self {
                _temp: temp,
                dev,
                sys,
                mountinfo,
                swaps,
            }
        }

        fn scanner(&self) -> DiskScanner {
            DiskScanner::new(DiscoveryRoots {
                dev: self.dev.clone(),
                sys_class_block: self.sys.clone(),
                mountinfo: self.mountinfo.clone(),
                swaps: self.swaps.clone(),
            })
        }
    }
}
