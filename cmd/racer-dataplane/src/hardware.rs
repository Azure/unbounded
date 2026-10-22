//! Live block-device and RDMA topology report for `/debug/hardware`.
//!
//! Discovery reads sysfs attributes and `disk/by-id` symlinks only. It never
//! opens a block device, holds no cache, and applies no entry caps; every call
//! reflects the host at that moment. Missing or malformed individual values
//! become `null`, while a section that cannot be listed records a message in
//! `errors` and the rest of the report is still produced.

use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::fs;
use std::io;
use std::path::Path;

use serde::Serialize;

/// Complete hardware report served as JSON.
#[derive(Debug, Default, Serialize)]
pub struct Report {
    pub block_devices: Vec<BlockDevice>,
    pub rdma_devices: Vec<RdmaDevice>,
    pub racer: Option<Racer>,
    pub errors: Vec<String>,
}

/// One entry under `<sys>/block`.
#[derive(Debug, Default, Serialize)]
pub struct BlockDevice {
    pub name: String,
    pub dev: Option<String>,
    pub size_bytes: Option<u64>,
    pub logical_block_size: Option<u64>,
    pub physical_block_size: Option<u64>,
    pub rotational: Option<bool>,
    pub removable: Option<bool>,
    pub read_only: Option<bool>,
    pub model: Option<String>,
    pub serial: Option<String>,
    pub wwid: Option<String>,
    pub by_id: Vec<String>,
    pub partitions: Vec<Partition>,
    pub holders: Vec<String>,
}

/// One partition of a block device.
#[derive(Debug, Default, Serialize)]
pub struct Partition {
    pub name: String,
    pub dev: Option<String>,
    pub size_bytes: Option<u64>,
    pub by_id: Vec<String>,
}

/// One entry under `<sys>/class/infiniband`.
#[derive(Debug, Default, Serialize)]
pub struct RdmaDevice {
    pub name: String,
    pub node_guid: Option<String>,
    pub pci_address: Option<String>,
    pub numa_node: Option<u32>,
    pub ports: Vec<RdmaPort>,
}

/// One port of an RDMA device.
#[derive(Debug, Default, Serialize)]
pub struct RdmaPort {
    pub port: u32,
    pub state: Option<String>,
    pub phys_state: Option<String>,
    pub link_layer: Option<String>,
    pub rate: Option<String>,
    pub gid0: Option<String>,
    pub gid0_type: Option<String>,
    pub netdevs: Vec<String>,
}

/// Racer's own view of the hardware it uses.
#[derive(Debug, Default, Serialize)]
pub struct Racer {
    pub block_device_selector: Option<String>,
    pub claimed_disks: Vec<ClaimedDisk>,
    pub rdma_rails: Option<Rails>,
}

/// A block device claimed for the disk tier.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct ClaimedDisk {
    pub id: String,
    pub dev: Option<String>,
}

/// The process-wide RDMA rail inventory snapshot.
#[derive(Debug, Default, Serialize)]
pub struct Rails {
    pub generation: u64,
    pub rails: Vec<Rail>,
}

/// One published RDMA rail.
#[derive(Debug, Default, Serialize)]
pub struct Rail {
    pub rail: u16,
    pub device: String,
    pub port: u8,
    pub gid: Option<String>,
    pub numa_node: Option<usize>,
}

/// Startup-fixed inputs for the Racer section of the report.
#[derive(Clone, Debug, Default)]
pub struct Context {
    pub block_device_selector: Option<String>,
    pub claimed_disks: Vec<ClaimedDisk>,
}

/// Build the full report, including Racer's selector, claimed disks, and rails.
pub fn report(
    sys_root: &Path,
    dev_root: &Path,
    context: &Context,
    inventory: &crate::rdma::Inventory,
) -> Report {
    report_with_snapshot(
        sys_root,
        dev_root,
        context,
        inventory.snapshot().map_err(|error| format!("{error:?}")),
    )
}

/// Build the full report from an already captured rails snapshot. The inputs are
/// owned and `Send`, so a helper thread can run the sysfs walk off the I/O worker.
pub fn report_with_snapshot(
    sys_root: &Path,
    dev_root: &Path,
    context: &Context,
    snapshot: Result<crate::rdma::Snapshot, String>,
) -> Report {
    let mut report = discover(sys_root, dev_root);
    let rdma_rails = match snapshot {
        Ok(snapshot) => Some(Rails {
            generation: snapshot.generation,
            rails: snapshot
                .nics
                .iter()
                .map(|nic| Rail {
                    rail: nic.rail.0,
                    device: nic.device.clone(),
                    port: nic.port,
                    gid: nic.gid.as_ref().map(format_gid),
                    numa_node: nic.numa_node,
                })
                .collect(),
        }),
        Err(error) => {
            report
                .errors
                .push(format!("racer.rdma_rails: inventory snapshot: {error}"));
            None
        }
    };
    report.racer = Some(Racer {
        block_device_selector: context.block_device_selector.clone(),
        claimed_disks: context.claimed_disks.clone(),
        rdma_rails,
    });
    report
}

/// Discover block and RDMA devices from sysfs (`sys_root`) and by-id links (`dev_root`).
pub fn discover(sys_root: &Path, dev_root: &Path) -> Report {
    let mut report = Report::default();
    let by_id = by_id_links(dev_root, &mut report.errors);
    report.block_devices = block_devices(sys_root, &by_id, &mut report.errors);
    report.rdma_devices = rdma_devices(sys_root, &mut report.errors);
    report
}

/// Format a device number as the `MAJ:MIN` text used by sysfs `dev` files.
pub fn format_dev(rdev: u64) -> String {
    let major = ((rdev >> 8) & 0xfff) | ((rdev >> 32) & !0xfff);
    let minor = (rdev & 0xff) | ((rdev >> 12) & !0xff);
    format!("{major}:{minor}")
}

/// Format a raw GID as eight colon-separated groups of four hex digits, as sysfs does.
pub fn format_gid(gid: &[u8; 16]) -> String {
    gid.chunks_exact(2)
        .map(|pair| format!("{:02x}{:02x}", pair[0], pair[1]))
        .collect::<Vec<_>>()
        .join(":")
}

/// Record a fixed-form section failure.
fn section_error(errors: &mut Vec<String>, section: &str, path: &Path, error: &io::Error) {
    errors.push(format!("{section}: {}: {error}", path.display()));
}

/// Sorted entry names of a directory.
fn list(path: &Path) -> io::Result<Vec<String>> {
    let mut names = fs::read_dir(path)?
        .filter_map(|entry| entry.ok())
        .filter_map(|entry| entry.file_name().into_string().ok())
        .collect::<Vec<_>>();
    names.sort();
    Ok(names)
}

/// Trimmed attribute text, or `None` when absent, unreadable, or empty.
fn text(path: &Path) -> Option<String> {
    let value = fs::read_to_string(path).ok()?;
    let value = value.trim();
    (!value.is_empty()).then(|| value.to_owned())
}

/// Unsigned decimal attribute.
fn number(path: &Path) -> Option<u64> {
    text(path)?.parse().ok()
}

/// `0`/`1` attribute.
fn flag(path: &Path) -> Option<bool> {
    match text(path)?.as_str() {
        "0" => Some(false),
        "1" => Some(true),
        _ => None,
    }
}

/// Final path component of a symlink target.
fn link_name(path: &Path) -> Option<String> {
    fs::read_link(path)
        .ok()?
        .file_name()?
        .to_str()
        .map(str::to_owned)
}

/// Map kernel device names to the `disk/by-id` link names that point at them.
fn by_id_links(dev_root: &Path, errors: &mut Vec<String>) -> BTreeMap<String, Vec<String>> {
    let directory = dev_root.join("disk/by-id");
    let mut links: BTreeMap<String, Vec<String>> = BTreeMap::new();
    let names = match list(&directory) {
        Ok(names) => names,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return links,
        Err(error) => {
            section_error(errors, "by_id", &directory, &error);
            return links;
        }
    };
    for name in names {
        if let Some(target) = link_name(&directory.join(&name)) {
            links.entry(target).or_default().push(name);
        }
    }
    links
}

/// Enumerate every entry under `<sys>/block` without filtering.
fn block_devices(
    sys_root: &Path,
    by_id: &BTreeMap<String, Vec<String>>,
    errors: &mut Vec<String>,
) -> Vec<BlockDevice> {
    let root = sys_root.join("block");
    let names = match list(&root) {
        Ok(names) => names,
        Err(error) => {
            section_error(errors, "block_devices", &root, &error);
            return Vec::new();
        }
    };
    names
        .into_iter()
        .map(|name| {
            let path = root.join(&name);
            let children = list(&path).unwrap_or_default();
            let partitions = children
                .iter()
                .filter(|child| path.join(child).join("partition").is_file())
                .map(|child| {
                    let part = path.join(child);
                    Partition {
                        name: child.clone(),
                        dev: text(&part.join("dev")),
                        size_bytes: sectors(&part.join("size")),
                        by_id: by_id.get(child).cloned().unwrap_or_default(),
                    }
                })
                .collect();
            BlockDevice {
                dev: text(&path.join("dev")),
                size_bytes: sectors(&path.join("size")),
                logical_block_size: number(&path.join("queue/logical_block_size")),
                physical_block_size: number(&path.join("queue/physical_block_size")),
                rotational: flag(&path.join("queue/rotational")),
                removable: flag(&path.join("removable")),
                read_only: flag(&path.join("ro")),
                model: text(&path.join("device/model")),
                serial: text(&path.join("device/serial")),
                wwid: text(&path.join("wwid")).or_else(|| text(&path.join("device/wwid"))),
                by_id: by_id.get(&name).cloned().unwrap_or_default(),
                partitions,
                holders: list(&path.join("holders")).unwrap_or_default(),
                name,
            }
        })
        .collect()
}

/// Sysfs `size` is always counted in 512-byte sectors.
fn sectors(path: &Path) -> Option<u64> {
    number(path)?.checked_mul(512)
}

/// Strip the numeric prefix sysfs puts on port states, such as `4: ACTIVE`.
fn state(path: &Path) -> Option<String> {
    let value = text(path)?;
    Some(match value.split_once(':') {
        Some((_, name)) => name.trim().to_owned(),
        None => value,
    })
}

/// Whether a name has the `DDDD:BB:DD.F` PCI address form.
fn is_pci_address(name: &str) -> bool {
    let bytes = name.as_bytes();
    bytes.len() == 12
        && bytes[4] == b':'
        && bytes[7] == b':'
        && bytes[10] == b'.'
        && bytes
            .iter()
            .enumerate()
            .all(|(i, b)| matches!(i, 4 | 7 | 10) || b.is_ascii_hexdigit())
}

/// Enumerate `<sys>/class/infiniband`; an absent class means no RDMA hardware.
fn rdma_devices(sys_root: &Path, errors: &mut Vec<String>) -> Vec<RdmaDevice> {
    let root = sys_root.join("class/infiniband");
    let names = match list(&root) {
        Ok(names) => names,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Vec::new(),
        Err(error) => {
            section_error(errors, "rdma_devices", &root, &error);
            return Vec::new();
        }
    };
    names
        .into_iter()
        .map(|name| {
            let path = root.join(&name);
            let pci_address = fs::canonicalize(path.join("device"))
                .ok()
                .and_then(|target| target.file_name()?.to_str().map(str::to_owned))
                .filter(|address| is_pci_address(address));
            let numa_node = text(&path.join("device/numa_node"))
                .and_then(|value| value.parse::<i64>().ok())
                .and_then(|value| u32::try_from(value).ok());
            let pci_netdevs = list(&path.join("device/net")).unwrap_or_default();
            let mut ports = list(&path.join("ports"))
                .unwrap_or_default()
                .into_iter()
                .filter_map(|port| {
                    let number = port.parse::<u32>().ok()?;
                    let port = path.join("ports").join(port);
                    let mut netdevs = BTreeSet::new();
                    // Every GID slot may name a netdev (for example a RoCE
                    // VLAN interface absent from device/net). Unused slots
                    // fail to read in sysfs and are skipped by `text`.
                    let ndevs = port.join("gid_attrs/ndevs");
                    for entry in list(&ndevs).unwrap_or_default() {
                        netdevs.extend(text(&ndevs.join(entry)));
                    }
                    for netdev in &pci_netdevs {
                        // dev_port is zero-based; keep netdevs without it.
                        let dev_port = number_at(&path.join("device/net").join(netdev));
                        if dev_port.is_none_or(|dev_port| dev_port + 1 == u64::from(number)) {
                            netdevs.insert(netdev.clone());
                        }
                    }
                    Some(RdmaPort {
                        port: number,
                        state: state(&port.join("state")),
                        phys_state: state(&port.join("phys_state")),
                        link_layer: text(&port.join("link_layer")),
                        rate: text(&port.join("rate")),
                        gid0: text(&port.join("gids/0")),
                        gid0_type: text(&port.join("gid_attrs/types/0")),
                        netdevs: netdevs.into_iter().collect(),
                    })
                })
                .collect::<Vec<_>>();
            ports.sort_by_key(|port| port.port);
            RdmaDevice {
                node_guid: text(&path.join("node_guid")),
                pci_address,
                numa_node,
                ports,
                name,
            }
        })
        .collect()
}

/// The `dev_port` attribute of a network interface directory.
fn number_at(netdev: &Path) -> Option<u64> {
    number(&netdev.join("dev_port"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;
    use std::path::PathBuf;

    struct Fixture(PathBuf);

    impl Fixture {
        fn new() -> Self {
            static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
            let parent = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../tmp");
            fs::create_dir_all(&parent).unwrap();
            let path = parent.join(format!(
                "hardware-test-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
            ));
            fs::create_dir(&path).unwrap();
            Self(fs::canonicalize(path).unwrap())
        }

        fn sys(&self) -> PathBuf {
            self.0.join("sys")
        }

        fn dev(&self) -> PathBuf {
            self.0.join("dev")
        }

        fn write(&self, path: &str, value: &str) {
            let path = self.0.join(path);
            fs::create_dir_all(path.parent().unwrap()).unwrap();
            fs::write(path, value).unwrap();
        }

        fn link(&self, target: &str, path: &str) {
            let path = self.0.join(path);
            fs::create_dir_all(path.parent().unwrap()).unwrap();
            symlink(target, path).unwrap();
        }
    }

    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    /// One NVMe disk with a partition, a holder, and by-id links, plus a loop device.
    fn block_tree(f: &Fixture) {
        let disk = "sys/block/nvme0n1";
        f.write(&format!("{disk}/dev"), "259:0\n");
        f.write(&format!("{disk}/size"), "7501476528\n");
        f.write(&format!("{disk}/queue/logical_block_size"), "512\n");
        f.write(&format!("{disk}/queue/physical_block_size"), "4096\n");
        f.write(&format!("{disk}/queue/rotational"), "0\n");
        f.write(&format!("{disk}/removable"), "0\n");
        f.write(&format!("{disk}/ro"), "0\n");
        f.write(
            &format!("{disk}/device/model"),
            "Microsoft NVMe Direct Disk  \n",
        );
        f.write(&format!("{disk}/device/serial"), "  SN123 \n");
        f.write(&format!("{disk}/wwid"), "eui.0001\n");
        f.write(&format!("{disk}/holders/dm-0"), "");
        f.write(&format!("{disk}/nvme0n1p1/partition"), "1\n");
        f.write(&format!("{disk}/nvme0n1p1/dev"), "259:1\n");
        f.write(&format!("{disk}/nvme0n1p1/size"), "2048\n");
        // A child directory without a partition file is not a partition.
        f.write(&format!("{disk}/mq/0/cpu_list"), "0\n");
        f.write("sys/block/loop0/dev", "7:0\n");
        f.write("sys/block/loop0/size", "not-a-number\n");
        f.write("sys/block/loop0/ro", "2\n");
        f.write("sys/block/loop0/device/wwid", "naa.loop\n");
        f.link("../../nvme0n1", "dev/disk/by-id/nvme-Microsoft_NVMe_SN123");
        f.link("../../nvme0n1", "dev/disk/by-id/nvme-eui.0001");
        f.link(
            "../../nvme0n1p1",
            "dev/disk/by-id/nvme-Microsoft_NVMe_SN123-part1",
        );
    }

    /// Two RDMA devices: one active RoCE port, one inactive port with unknown NUMA.
    fn rdma_tree(f: &Fixture) {
        f.write("sys/devices/pci0000:40/0000:41:00.0/numa_node", "1\n");
        f.write(
            "sys/devices/pci0000:40/0000:41:00.0/net/eth2/dev_port",
            "0\n",
        );
        f.write(
            "sys/devices/pci0000:40/0000:41:00.0/net/eth9/dev_port",
            "1\n",
        );
        f.link(
            "../../../devices/pci0000:40/0000:41:00.0",
            "sys/class/infiniband/mlx5_0/device",
        );
        let port = "sys/class/infiniband/mlx5_0/ports/1";
        f.write(
            "sys/class/infiniband/mlx5_0/node_guid",
            "0c42:a103:0012:3456\n",
        );
        f.write(&format!("{port}/state"), "4: ACTIVE\n");
        f.write(&format!("{port}/phys_state"), "5: LinkUp\n");
        f.write(&format!("{port}/link_layer"), "Ethernet\n");
        f.write(&format!("{port}/rate"), "200 Gb/sec (4X HDR)\n");
        f.write(
            &format!("{port}/gids/0"),
            "fe80:0000:0000:0000:0e42:a1ff:fe12:3456\n",
        );
        f.write(&format!("{port}/gid_attrs/types/0"), "RoCE v2\n");
        f.write(&format!("{port}/gid_attrs/ndevs/0"), "eth2\n");
        f.write("sys/devices/virtual/ib1/numa_node", "-1\n");
        f.link(
            "../../../devices/virtual/ib1",
            "sys/class/infiniband/mlx5_1/device",
        );
        let port = "sys/class/infiniband/mlx5_1/ports/1";
        f.write(&format!("{port}/state"), "1: DOWN\n");
        f.write(&format!("{port}/phys_state"), "3: Disabled\n");
        f.write(&format!("{port}/link_layer"), "InfiniBand\n");
    }

    #[test]
    fn full_tree_reports_disks_partitions_holders_and_by_id() {
        let f = Fixture::new();
        block_tree(&f);
        let report = discover(&f.sys(), &f.dev());
        assert!(report.errors.is_empty(), "{:?}", report.errors);
        assert_eq!(
            report
                .block_devices
                .iter()
                .map(|d| d.name.as_str())
                .collect::<Vec<_>>(),
            ["loop0", "nvme0n1"]
        );
        let disk = &report.block_devices[1];
        assert_eq!(disk.dev.as_deref(), Some("259:0"));
        assert_eq!(disk.size_bytes, Some(7501476528 * 512));
        assert_eq!(disk.logical_block_size, Some(512));
        assert_eq!(disk.physical_block_size, Some(4096));
        assert_eq!(disk.rotational, Some(false));
        assert_eq!(disk.removable, Some(false));
        assert_eq!(disk.read_only, Some(false));
        assert_eq!(disk.model.as_deref(), Some("Microsoft NVMe Direct Disk"));
        assert_eq!(disk.serial.as_deref(), Some("SN123"));
        assert_eq!(disk.wwid.as_deref(), Some("eui.0001"));
        assert_eq!(disk.by_id, ["nvme-Microsoft_NVMe_SN123", "nvme-eui.0001"]);
        assert_eq!(disk.holders, ["dm-0"]);
        assert_eq!(disk.partitions.len(), 1);
        let part = &disk.partitions[0];
        assert_eq!(part.name, "nvme0n1p1");
        assert_eq!(part.dev.as_deref(), Some("259:1"));
        assert_eq!(part.size_bytes, Some(2048 * 512));
        assert_eq!(part.by_id, ["nvme-Microsoft_NVMe_SN123-part1"]);
    }

    #[test]
    fn malformed_and_missing_values_become_null() {
        let f = Fixture::new();
        block_tree(&f);
        let report = discover(&f.sys(), &f.dev());
        let loop0 = &report.block_devices[0];
        assert_eq!(loop0.dev.as_deref(), Some("7:0"));
        assert_eq!(loop0.size_bytes, None);
        assert_eq!(loop0.read_only, None);
        assert_eq!(loop0.removable, None);
        assert_eq!(loop0.logical_block_size, None);
        assert_eq!(loop0.model, None);
        assert_eq!(loop0.wwid.as_deref(), Some("naa.loop"));
        assert!(loop0.by_id.is_empty() && loop0.partitions.is_empty());
        assert!(loop0.holders.is_empty());
    }

    #[test]
    fn empty_tree_reports_block_error_but_no_rdma_or_by_id_error() {
        let f = Fixture::new();
        let report = discover(&f.sys(), &f.dev());
        assert!(report.block_devices.is_empty());
        assert!(report.rdma_devices.is_empty());
        assert_eq!(report.errors.len(), 1);
        assert!(report.errors[0].starts_with("block_devices: "));
        assert!(report.errors[0].contains("sys/block"));
    }

    #[test]
    fn present_block_dir_without_infiniband_is_clean() {
        let f = Fixture::new();
        fs::create_dir_all(f.sys().join("block")).unwrap();
        let report = discover(&f.sys(), &f.dev());
        assert!(report.errors.is_empty());
        assert!(report.block_devices.is_empty() && report.rdma_devices.is_empty());
    }

    #[test]
    fn unlistable_infiniband_is_an_error() {
        let f = Fixture::new();
        fs::create_dir_all(f.sys().join("block")).unwrap();
        f.write("sys/class/infiniband", "not a directory");
        let report = discover(&f.sys(), &f.dev());
        assert_eq!(report.errors.len(), 1);
        assert!(report.errors[0].starts_with("rdma_devices: "));
    }

    #[test]
    fn rdma_ports_states_numa_and_netdevs() {
        let f = Fixture::new();
        fs::create_dir_all(f.sys().join("block")).unwrap();
        rdma_tree(&f);
        let report = discover(&f.sys(), &f.dev());
        assert!(report.errors.is_empty(), "{:?}", report.errors);
        let [active, inactive] = &report.rdma_devices[..] else {
            panic!("two devices expected");
        };
        assert_eq!(active.name, "mlx5_0");
        assert_eq!(active.node_guid.as_deref(), Some("0c42:a103:0012:3456"));
        assert_eq!(active.pci_address.as_deref(), Some("0000:41:00.0"));
        assert_eq!(active.numa_node, Some(1));
        let port = &active.ports[0];
        assert_eq!(port.port, 1);
        assert_eq!(port.state.as_deref(), Some("ACTIVE"));
        assert_eq!(port.phys_state.as_deref(), Some("LinkUp"));
        assert_eq!(port.link_layer.as_deref(), Some("Ethernet"));
        assert_eq!(port.rate.as_deref(), Some("200 Gb/sec (4X HDR)"));
        assert_eq!(
            port.gid0.as_deref(),
            Some("fe80:0000:0000:0000:0e42:a1ff:fe12:3456")
        );
        assert_eq!(port.gid0_type.as_deref(), Some("RoCE v2"));
        // eth9 belongs to dev_port 1 (port 2), so it is excluded from port 1.
        assert_eq!(port.netdevs, ["eth2"]);
        assert_eq!(inactive.name, "mlx5_1");
        assert_eq!(inactive.pci_address, None);
        assert_eq!(inactive.numa_node, None);
        assert_eq!(inactive.node_guid, None);
        let port = &inactive.ports[0];
        assert_eq!(port.state.as_deref(), Some("DOWN"));
        assert_eq!(port.phys_state.as_deref(), Some("Disabled"));
        assert_eq!(port.gid0, None);
        assert!(port.netdevs.is_empty());
    }

    #[test]
    fn netdevs_include_nonzero_gid_entries() {
        let f = Fixture::new();
        fs::create_dir_all(f.sys().join("block")).unwrap();
        rdma_tree(&f);
        let port = "sys/class/infiniband/mlx5_0/ports/1";
        // vlan100 appears only in a nonzero GID entry, not in device/net.
        f.write(&format!("{port}/gid_attrs/ndevs/3"), "vlan100\n");
        f.write(&format!("{port}/gid_attrs/ndevs/2"), "eth2\n");
        // Unused slot: empty content is skipped.
        f.write(&format!("{port}/gid_attrs/ndevs/7"), "");
        f.write(&format!("{port}/gid_attrs/types/3"), "RoCE v2\n");
        let report = discover(&f.sys(), &f.dev());
        assert!(report.errors.is_empty(), "{:?}", report.errors);
        let port = &report.rdma_devices[0].ports[0];
        assert_eq!(port.netdevs, ["eth2", "vlan100"]);
        assert_eq!(port.gid0_type.as_deref(), Some("RoCE v2"));
        assert_eq!(
            port.gid0.as_deref(),
            Some("fe80:0000:0000:0000:0e42:a1ff:fe12:3456")
        );
    }

    #[test]
    fn formats_gids_device_numbers_and_pci_addresses() {
        let mut gid = [0; 16];
        gid[0] = 0xfe;
        gid[1] = 0x80;
        gid[15] = 0x01;
        assert_eq!(format_gid(&gid), "fe80:0000:0000:0000:0000:0000:0000:0001");
        assert_eq!(format_dev(libc::makedev(259, 0)), "259:0");
        assert_eq!(format_dev(libc::makedev(4095, 1_048_575)), "4095:1048575");
        assert!(is_pci_address("0000:41:00.0"));
        assert!(!is_pci_address("ib1"));
        assert!(!is_pci_address("0000:41:00:0"));
    }

    #[test]
    fn json_shape_has_stable_keys() {
        let f = Fixture::new();
        block_tree(&f);
        rdma_tree(&f);
        let inventory = crate::rdma::Inventory::default();
        let context = Context {
            block_device_selector: Some("^nvme-".into()),
            claimed_disks: vec![ClaimedDisk {
                id: "nvme-Microsoft_NVMe_SN123".into(),
                dev: Some("259:0".into()),
            }],
        };
        let report = report(&f.sys(), &f.dev(), &context, &inventory);
        let value: serde_json::Value =
            serde_json::from_str(&serde_json::to_string_pretty(&report).unwrap()).unwrap();
        let keys = |v: &serde_json::Value| {
            let mut keys = v.as_object().unwrap().keys().cloned().collect::<Vec<_>>();
            keys.sort();
            keys
        };
        assert_eq!(
            keys(&value),
            ["block_devices", "errors", "racer", "rdma_devices"]
        );
        assert_eq!(
            keys(&value["block_devices"][1]),
            [
                "by_id",
                "dev",
                "holders",
                "logical_block_size",
                "model",
                "name",
                "partitions",
                "physical_block_size",
                "read_only",
                "removable",
                "rotational",
                "serial",
                "size_bytes",
                "wwid"
            ]
        );
        assert_eq!(
            keys(&value["block_devices"][1]["partitions"][0]),
            ["by_id", "dev", "name", "size_bytes"]
        );
        assert_eq!(
            keys(&value["rdma_devices"][0]["ports"][0]),
            [
                "gid0",
                "gid0_type",
                "link_layer",
                "netdevs",
                "phys_state",
                "port",
                "rate",
                "state"
            ]
        );
        assert_eq!(
            keys(&value["racer"]),
            ["block_device_selector", "claimed_disks", "rdma_rails"]
        );
        assert_eq!(value["racer"]["claimed_disks"][0]["dev"], "259:0");
        assert_eq!(value["racer"]["rdma_rails"]["generation"], 0);
        assert!(value["block_devices"][0]["size_bytes"].is_null());
    }

    #[test]
    fn racer_rails_use_sysfs_gid_format() {
        let inventory = crate::rdma::Inventory::default();
        let mut gid = [0; 16];
        gid[0] = 0xfe;
        gid[1] = 0x80;
        gid[15] = 0x02;
        inventory
            .update(vec![racer_control_wire::RailMapping {
                rail: racer_control_wire::RailId(0),
                device: "mlx5_0".into(),
                port: 1,
                gid: Some(gid),
                numa_node: Some(1),
            }])
            .unwrap();
        let f = Fixture::new();
        let report = report(&f.sys(), &f.dev(), &Context::default(), &inventory);
        let rails = report.racer.unwrap().rdma_rails.unwrap();
        assert_eq!(rails.rails.len(), 1);
        assert_eq!(rails.rails[0].device, "mlx5_0");
        assert_eq!(
            rails.rails[0].gid.as_deref(),
            Some("fe80:0000:0000:0000:0000:0000:0000:0002")
        );
        assert_eq!(rails.rails[0].numa_node, Some(1));
    }

    /// Print the live host report; run with `--ignored --nocapture`.
    #[test]
    #[ignore = "reads the live host sysfs"]
    fn live_host_report() {
        let report = discover(Path::new("/sys"), Path::new("/dev"));
        println!("{}", serde_json::to_string_pretty(&report).unwrap());
    }
}
