// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Host hardware snapshot. `Host::discover` walks sysfs once and
//! returns an immutable view of the CPUs, NUMA nodes, RDMA HCAs,
//! and NVMe controllers visible to this process. Filtering and
//! placement decisions live in `plan.rs`; this file only records.

use std::collections::{BTreeMap, BTreeSet};
use std::fs;
use std::path::Path;

use super::sysfs;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Cpu {
    pub id: u32,
    pub numa: Option<u16>,
    pub smt_siblings: Vec<u32>,
    pub isolated: bool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NumaNode {
    pub id: u16,
    pub cpus: Vec<u32>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Hca {
    pub dev_name: String,
    pub pci_bdf: Option<String>,
    pub pcie_root: Option<String>,
    pub numa: Option<u16>,
    pub ports_active: bool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Nvme {
    pub dev_name: String,
    pub pci_bdf: Option<String>,
    pub pcie_root: Option<String>,
    pub numa: Option<u16>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BlockDevice {
    pub dev_name: String,
    pub size_bytes: Option<u64>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Nic {
    pub dev_name: String,
    pub pci_bdf: Option<String>,
    pub pcie_root: Option<String>,
    pub numa: Option<u16>,
    pub rx_queues: usize,
    pub msi_irqs: Vec<u32>,
    pub operstate_up: bool,
}

/// A `(numa, pcie_root)` locality domain: the HCAs, NVMes, and NICs
/// that share a NUMA node and PCIe root port, plus the CPUs resident
/// on that NUMA node. Devices whose root port is unknown collapse into
/// a NUMA-only domain (`pcie_root = None`). Planning uses this to pair
/// NICs with NVMes on the same PCIe complex and to pin device workers
/// to complex-local CPUs.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LocalityGroup {
    pub numa: Option<u16>,
    pub pcie_root: Option<String>,
    pub cpus: Vec<u32>,
    pub hcas: Vec<String>,
    pub nvmes: Vec<String>,
    pub nics: Vec<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Host {
    pub cpus: BTreeMap<u32, Cpu>,
    pub numa_nodes: Vec<NumaNode>,
    pub hcas: Vec<Hca>,
    pub nvmes: Vec<Nvme>,
    pub block_devices: Vec<BlockDevice>,
    pub nics: Vec<Nic>,
    pub isolated: BTreeSet<u32>,
}

impl Host {
    /// Discover the host using the real `/sys`.
    pub fn discover() -> Self {
        Self::discover_with(Path::new("/sys"))
    }

    /// Sysfs-root parameterized variant of [`Host::discover`]. Tests
    /// pass a staged temp directory; production passes `/sys`.
    pub fn discover_with(sys_root: &Path) -> Self {
        let isolated = sysfs::read_isolated_cpus(sys_root);
        let online = sysfs::read_online_cpus(sys_root);

        let mut cpus = BTreeMap::new();
        for cpu in &online {
            let numa = sysfs::numa_of_cpu(sys_root, *cpu);
            let smt_siblings = sysfs::thread_siblings(sys_root, *cpu);
            cpus.insert(
                *cpu,
                Cpu {
                    id: *cpu,
                    numa,
                    smt_siblings,
                    isolated: isolated.contains(cpu),
                },
            );
        }

        let numa_nodes = discover_numa_nodes(sys_root);
        let mut hcas = discover_hcas(sys_root);
        hcas.sort_by(|a, b| a.pci_bdf.cmp(&b.pci_bdf).then(a.dev_name.cmp(&b.dev_name)));
        let mut nvmes = discover_nvmes(sys_root);
        nvmes.sort_by(|a, b| a.pci_bdf.cmp(&b.pci_bdf).then(a.dev_name.cmp(&b.dev_name)));
        let mut block_devices = discover_block_devices(sys_root);
        block_devices.sort_by(|a, b| a.dev_name.cmp(&b.dev_name));
        let mut nics = discover_nics(sys_root);
        nics.sort_by(|a, b| a.pci_bdf.cmp(&b.pci_bdf).then(a.dev_name.cmp(&b.dev_name)));

        Host {
            cpus,
            numa_nodes,
            hcas,
            nvmes,
            block_devices,
            nics,
            isolated,
        }
    }

    /// CPUs on `numa`, or all online CPUs when `numa` is `None`.
    pub fn cpus_on(&self, numa: Option<u16>) -> Vec<u32> {
        match numa {
            None => self.cpus.keys().copied().collect(),
            Some(id) => self
                .numa_nodes
                .iter()
                .find(|n| n.id == id)
                .map(|n| n.cpus.clone())
                .unwrap_or_default(),
        }
    }

    /// Group HCAs, NVMes, and NICs into `(numa, pcie_root)` locality
    /// domains, attaching the CPUs resident on each domain's NUMA node.
    /// Groups are ordered by `(numa, pcie_root)`; device names within a
    /// group keep host discovery order (PCI-BDF sorted).
    pub fn locality_groups(&self) -> Vec<LocalityGroup> {
        let mut groups: BTreeMap<(Option<u16>, Option<String>), LocalityGroup> = BTreeMap::new();
        for h in &self.hcas {
            self.locality_entry(&mut groups, h.numa, &h.pcie_root)
                .hcas
                .push(h.dev_name.clone());
        }
        for n in &self.nvmes {
            self.locality_entry(&mut groups, n.numa, &n.pcie_root)
                .nvmes
                .push(n.dev_name.clone());
        }
        for n in &self.nics {
            self.locality_entry(&mut groups, n.numa, &n.pcie_root)
                .nics
                .push(n.dev_name.clone());
        }
        groups.into_values().collect()
    }

    fn locality_entry<'g>(
        &self,
        groups: &'g mut BTreeMap<(Option<u16>, Option<String>), LocalityGroup>,
        numa: Option<u16>,
        pcie_root: &Option<String>,
    ) -> &'g mut LocalityGroup {
        groups
            .entry((numa, pcie_root.clone()))
            .or_insert_with(|| LocalityGroup {
                numa,
                pcie_root: pcie_root.clone(),
                cpus: self.cpus_on(numa),
                hcas: Vec::new(),
                nvmes: Vec::new(),
                nics: Vec::new(),
            })
    }
}

fn discover_numa_nodes(sys_root: &Path) -> Vec<NumaNode> {
    let node_root = sys_root.join("devices/system/node");
    let entries = match fs::read_dir(&node_root) {
        Ok(e) => e,
        Err(_) => return Vec::new(),
    };
    let mut nodes = Vec::new();
    for entry in entries.flatten() {
        let name = entry.file_name();
        let id = match name
            .to_string_lossy()
            .strip_prefix("node")
            .and_then(|s| s.parse::<u16>().ok())
        {
            Some(id) => id,
            None => continue,
        };
        let cpus = sysfs::read_cpulist_file(&entry.path().join("cpulist")).unwrap_or_default();
        nodes.push(NumaNode { id, cpus });
    }
    nodes.sort_by_key(|n| n.id);
    nodes
}

fn discover_hcas(sys_root: &Path) -> Vec<Hca> {
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
        let numa =
            read_numa_node(&sys_root.join(format!("class/infiniband/{dev}/device/numa_node")));
        let pci_bdf =
            sysfs::read_pci_bdf(&sys_root.join(format!("class/infiniband/{dev}/device/uevent")));
        let pcie_root = pci_bdf
            .as_deref()
            .and_then(|bdf| sysfs::pcie_root_port(sys_root, bdf));
        let ports_active = has_active_port(sys_root, &dev);
        out.push(Hca {
            dev_name: dev,
            pci_bdf,
            pcie_root,
            numa,
            ports_active,
        });
    }
    out
}

fn discover_nvmes(sys_root: &Path) -> Vec<Nvme> {
    let nvme_root = sys_root.join("class/nvme");
    let entries = match fs::read_dir(&nvme_root) {
        Ok(e) => e,
        Err(_) => return Vec::new(),
    };
    let mut out = Vec::new();
    for entry in entries.flatten() {
        let name_os = entry.file_name();
        let dev = match name_os.to_str() {
            // Controllers are `nvmeN`; namespaces (`nvmeNnM`) live
            // under `class/block`, not here, but be defensive.
            Some(s) if is_nvme_controller(s) => s.to_owned(),
            _ => continue,
        };
        let numa = read_numa_node(&sys_root.join(format!("class/nvme/{dev}/device/numa_node")));
        let pci_bdf =
            sysfs::read_pci_bdf(&sys_root.join(format!("class/nvme/{dev}/device/uevent")));
        let pcie_root = pci_bdf
            .as_deref()
            .and_then(|bdf| sysfs::pcie_root_port(sys_root, bdf));
        out.push(Nvme {
            dev_name: dev,
            pci_bdf,
            pcie_root,
            numa,
        });
    }
    out
}

fn discover_nics(sys_root: &Path) -> Vec<Nic> {
    let net_root = sys_root.join("class/net");
    let entries = match fs::read_dir(&net_root) {
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
        // Skip virtual devices (lo, bridges, veth, ...) that have no
        // backing `device` symlink. Only physical NICs are recorded.
        let device_dir = sys_root.join(format!("class/net/{dev}/device"));
        if !device_dir.exists() {
            continue;
        }
        let numa = read_numa_node(&sys_root.join(format!("class/net/{dev}/device/numa_node")));
        let pci_bdf = sysfs::read_pci_bdf(&sys_root.join(format!("class/net/{dev}/device/uevent")));
        let pcie_root = pci_bdf
            .as_deref()
            .and_then(|bdf| sysfs::pcie_root_port(sys_root, bdf));
        let rx_queues = count_rx_queues(&sys_root.join(format!("class/net/{dev}/queues")));
        let msi_irqs = read_msi_irqs(&sys_root.join(format!("class/net/{dev}/device/msi_irqs")));
        let operstate_up = read_operstate_up(&sys_root.join(format!("class/net/{dev}/operstate")));
        out.push(Nic {
            dev_name: dev,
            pci_bdf,
            pcie_root,
            numa,
            rx_queues,
            msi_irqs,
            operstate_up,
        });
    }
    out
}

fn discover_block_devices(sys_root: &Path) -> Vec<BlockDevice> {
    let block_root = sys_root.join("class/block");
    let entries = match fs::read_dir(&block_root) {
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
        if is_ignored_block_device(&dev) || entry.path().join("partition").exists() {
            continue;
        }
        out.push(BlockDevice {
            dev_name: dev,
            size_bytes: read_block_size_bytes(&entry.path().join("size")),
        });
    }
    out
}

fn count_rx_queues(queues_dir: &Path) -> usize {
    let entries = match fs::read_dir(queues_dir) {
        Ok(e) => e,
        Err(_) => return 0,
    };
    entries
        .flatten()
        .filter(|e| {
            e.file_name()
                .to_str()
                .map(|n| n.starts_with("rx-"))
                .unwrap_or(false)
        })
        .count()
}

fn read_msi_irqs(msi_dir: &Path) -> Vec<u32> {
    let entries = match fs::read_dir(msi_dir) {
        Ok(e) => e,
        Err(_) => return Vec::new(),
    };
    let mut irqs: Vec<u32> = entries
        .flatten()
        .filter_map(|e| e.file_name().to_str().and_then(|n| n.parse::<u32>().ok()))
        .collect();
    irqs.sort_unstable();
    irqs
}

fn read_operstate_up(path: &Path) -> bool {
    match fs::read_to_string(path) {
        Ok(s) => s.trim() == "up",
        Err(_) => false,
    }
}

fn is_ignored_block_device(name: &str) -> bool {
    name.starts_with("loop") || name.starts_with("ram")
}

fn read_block_size_bytes(path: &Path) -> Option<u64> {
    let sectors = fs::read_to_string(path).ok()?.trim().parse::<u64>().ok()?;
    sectors.checked_mul(512)
}

fn is_nvme_controller(name: &str) -> bool {
    let rest = match name.strip_prefix("nvme") {
        Some(r) => r,
        None => return false,
    };
    !rest.is_empty() && rest.chars().all(|c| c.is_ascii_digit())
}

fn read_numa_node(path: &Path) -> Option<u16> {
    let s = fs::read_to_string(path).ok()?;
    match s.trim().parse::<i32>().ok()? {
        n if n < 0 => None,
        n => Some(n as u16),
    }
}

fn has_active_port(sys_root: &Path, dev: &str) -> bool {
    let ports_dir = sys_root.join(format!("class/infiniband/{dev}/ports"));
    let entries = match fs::read_dir(&ports_dir) {
        Ok(e) => e,
        Err(_) => return false,
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU64, Ordering};

    static NEXT_ID: AtomicU64 = AtomicU64::new(0);

    struct FakeSys {
        root: PathBuf,
    }

    impl FakeSys {
        fn new() -> Self {
            let pid = std::process::id();
            let counter = NEXT_ID.fetch_add(1, Ordering::Relaxed);
            let root = std::env::current_dir()
                .unwrap()
                .join("target")
                .join("tmp-topology-tests")
                .join(format!("host-{pid}-{counter}"));
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

        fn add_online(&self, list: &str) {
            self.write("devices/system/cpu/online", &format!("{list}\n"));
        }

        fn add_isolated(&self, list: &str) {
            self.write("devices/system/cpu/isolated", &format!("{list}\n"));
        }

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

        fn add_hca(&self, dev: &str, bdf: &str, numa: i32, active: bool) {
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
            self.write(
                &format!("class/infiniband/{dev}/ports/1/state"),
                if active { "4: ACTIVE\n" } else { "1: DOWN\n" },
            );
        }

        fn add_nvme(&self, dev: &str, bdf: &str, numa: i32) {
            self.touch_dir(&format!("class/nvme/{dev}/device"));
            self.write(
                &format!("class/nvme/{dev}/device/numa_node"),
                &format!("{numa}\n"),
            );
            self.write(
                &format!("class/nvme/{dev}/device/uevent"),
                &format!("PCI_SLOT_NAME={bdf}\n"),
            );
        }

        fn add_block(&self, dev: &str, sectors: Option<u64>) {
            self.touch_dir(&format!("class/block/{dev}"));
            if let Some(sectors) = sectors {
                self.write(&format!("class/block/{dev}/size"), &format!("{sectors}\n"));
            }
        }

        fn add_partition(&self, dev: &str, sectors: u64) {
            self.add_block(dev, Some(sectors));
            self.write(&format!("class/block/{dev}/partition"), "1\n");
        }

        #[allow(clippy::too_many_arguments)]
        fn add_nic(
            &self,
            dev: &str,
            bdf: Option<&str>,
            numa: i32,
            rx_queues: usize,
            msi_irqs: &[u32],
            operstate: &str,
        ) {
            self.touch_dir(&format!("class/net/{dev}/device"));
            self.write(
                &format!("class/net/{dev}/device/numa_node"),
                &format!("{numa}\n"),
            );
            if let Some(bdf) = bdf {
                self.write(
                    &format!("class/net/{dev}/device/uevent"),
                    &format!("PCI_SLOT_NAME={bdf}\n"),
                );
            }
            for q in 0..rx_queues {
                self.touch_dir(&format!("class/net/{dev}/queues/rx-{q}"));
            }
            for irq in msi_irqs {
                self.write(&format!("class/net/{dev}/device/msi_irqs/{irq}"), "");
            }
            self.write(
                &format!("class/net/{dev}/operstate"),
                &format!("{operstate}\n"),
            );
        }

        /// Stage a virtual net device (no `device` symlink), e.g. `lo`.
        fn add_virtual_nic(&self, dev: &str) {
            self.touch_dir(&format!("class/net/{dev}"));
            self.write(&format!("class/net/{dev}/operstate"), "up\n");
        }

        /// Stage the `bus/pci/devices/<bdf>` symlink so `pcie_root`
        /// discovery resolves `bdf` to the given root port beneath a
        /// host bridge. The target need not exist on disk.
        fn link_pcie(&self, bdf: &str, root_port: &str) {
            let link = self.root.join("bus/pci/devices").join(bdf);
            fs::create_dir_all(link.parent().unwrap()).unwrap();
            let target = format!("../../devices/pci0000:00/{root_port}/{bdf}");
            std::os::unix::fs::symlink(target, &link).unwrap();
        }
    }

    impl Drop for FakeSys {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.root);
        }
    }

    #[test]
    fn empty_sysfs_yields_empty_host() {
        let s = FakeSys::new();
        let h = Host::discover_with(&s.root);
        assert!(h.cpus.is_empty());
        assert!(h.numa_nodes.is_empty());
        assert!(h.hcas.is_empty());
        assert!(h.nvmes.is_empty());
        assert!(h.block_devices.is_empty());
        assert!(h.nics.is_empty());
        assert!(h.isolated.is_empty());
    }

    #[test]
    fn single_hca_recorded_with_numa_and_bdf() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:af:00.0", 0, true);
        let h = Host::discover_with(&s.root);
        assert_eq!(h.hcas.len(), 1);
        let hca = &h.hcas[0];
        assert_eq!(hca.dev_name, "mlx5_0");
        assert_eq!(hca.numa, Some(0));
        assert_eq!(hca.pci_bdf.as_deref(), Some("0000:af:00.0"));
        assert!(hca.ports_active);
    }

    #[test]
    fn hca_numa_minus_one_yields_none() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:af:00.0", -1, true);
        let h = Host::discover_with(&s.root);
        assert_eq!(h.hcas[0].numa, None);
    }

    #[test]
    fn multiple_hcas_sort_by_bdf_then_dev() {
        let s = FakeSys::new();
        s.add_hca("mlx5_10", "0000:10:00.0", 0, true);
        s.add_hca("mlx5_2", "0000:02:00.0", 0, true);
        let h = Host::discover_with(&s.root);
        assert_eq!(
            h.hcas
                .iter()
                .map(|x| x.dev_name.as_str())
                .collect::<Vec<_>>(),
            vec!["mlx5_2", "mlx5_10"]
        );
    }

    #[test]
    fn hca_with_no_active_port_still_recorded() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:01:00.0", 0, false);
        let h = Host::discover_with(&s.root);
        assert_eq!(h.hcas.len(), 1);
        assert!(!h.hcas[0].ports_active);
    }

    #[test]
    fn single_nvme_recorded() {
        let s = FakeSys::new();
        s.add_nvme("nvme0", "0000:c1:00.0", 1);
        let h = Host::discover_with(&s.root);
        assert_eq!(h.nvmes.len(), 1);
        assert_eq!(h.nvmes[0].dev_name, "nvme0");
        assert_eq!(h.nvmes[0].numa, Some(1));
        assert_eq!(h.nvmes[0].pci_bdf.as_deref(), Some("0000:c1:00.0"));
    }

    #[test]
    fn nvme_namespaces_not_discovered() {
        let s = FakeSys::new();
        s.add_nvme("nvme0", "0000:c1:00.0", 0);
        // Stage a namespace-style directory; must be ignored.
        s.touch_dir("class/nvme/nvme0n1/device");
        s.write("class/nvme/nvme0n1/device/numa_node", "0\n");
        let h = Host::discover_with(&s.root);
        assert_eq!(h.nvmes.len(), 1);
        assert_eq!(h.nvmes[0].dev_name, "nvme0");
    }

    #[test]
    fn whole_block_devices_recorded_with_size_bytes() {
        let s = FakeSys::new();
        s.add_block("sdb", Some(8));
        s.add_block("nvme0n1", Some(16));
        let h = Host::discover_with(&s.root);
        assert_eq!(
            h.block_devices,
            vec![
                BlockDevice {
                    dev_name: "nvme0n1".to_string(),
                    size_bytes: Some(8192),
                },
                BlockDevice {
                    dev_name: "sdb".to_string(),
                    size_bytes: Some(4096),
                },
            ]
        );
    }

    #[test]
    fn block_device_missing_or_invalid_size_is_recorded_without_size() {
        let s = FakeSys::new();
        s.add_block("sdb", None);
        s.write("class/block/sdc/size", "not-a-number\n");
        let h = Host::discover_with(&s.root);
        assert_eq!(
            h.block_devices,
            vec![
                BlockDevice {
                    dev_name: "sdb".to_string(),
                    size_bytes: None,
                },
                BlockDevice {
                    dev_name: "sdc".to_string(),
                    size_bytes: None,
                },
            ]
        );
    }

    #[test]
    fn block_partitions_loop_and_ram_devices_are_skipped() {
        let s = FakeSys::new();
        s.add_block("sdb", Some(8));
        s.add_partition("sdb1", 4);
        s.add_block("loop0", Some(8));
        s.add_block("ram0", Some(8));
        let h = Host::discover_with(&s.root);
        assert_eq!(
            h.block_devices,
            vec![BlockDevice {
                dev_name: "sdb".to_string(),
                size_bytes: Some(4096),
            }]
        );
    }

    #[test]
    fn single_nic_recorded() {
        let s = FakeSys::new();
        s.add_nic("eth0", Some("0000:01:00.0"), 0, 4, &[40, 41, 42], "up");
        let h = Host::discover_with(&s.root);
        assert_eq!(h.nics.len(), 1);
        let nic = &h.nics[0];
        assert_eq!(nic.dev_name, "eth0");
        assert_eq!(nic.pci_bdf.as_deref(), Some("0000:01:00.0"));
        assert_eq!(nic.numa, Some(0));
        assert_eq!(nic.rx_queues, 4);
        assert_eq!(nic.msi_irqs, vec![40, 41, 42]);
        assert!(nic.operstate_up);
    }

    #[test]
    fn nic_numa_minus_one_yields_none() {
        let s = FakeSys::new();
        s.add_nic("eth0", Some("0000:01:00.0"), -1, 1, &[], "up");
        let h = Host::discover_with(&s.root);
        assert_eq!(h.nics.len(), 1);
        assert_eq!(h.nics[0].numa, None);
    }

    #[test]
    fn virtual_nic_without_device_is_skipped() {
        let s = FakeSys::new();
        s.add_virtual_nic("lo");
        s.add_nic("eth0", Some("0000:01:00.0"), 0, 2, &[10], "up");
        let h = Host::discover_with(&s.root);
        assert_eq!(h.nics.len(), 1);
        assert_eq!(h.nics[0].dev_name, "eth0");
    }

    #[test]
    fn multiple_nics_sort_by_bdf_then_dev() {
        let s = FakeSys::new();
        s.add_nic("eth1", Some("0000:10:00.0"), 0, 1, &[], "up");
        s.add_nic("eth0", Some("0000:02:00.0"), 0, 1, &[], "up");
        let h = Host::discover_with(&s.root);
        assert_eq!(
            h.nics
                .iter()
                .map(|n| n.dev_name.as_str())
                .collect::<Vec<_>>(),
            vec!["eth0", "eth1"]
        );
    }

    #[test]
    fn nic_msi_irqs_sorted() {
        let s = FakeSys::new();
        s.add_nic("eth0", Some("0000:01:00.0"), 0, 1, &[99, 3, 50, 7], "up");
        let h = Host::discover_with(&s.root);
        assert_eq!(h.nics[0].msi_irqs, vec![3, 7, 50, 99]);
    }

    #[test]
    fn nic_operstate_down_recorded_as_false() {
        let s = FakeSys::new();
        s.add_nic("eth0", Some("0000:01:00.0"), 0, 1, &[], "down");
        let h = Host::discover_with(&s.root);
        assert!(!h.nics[0].operstate_up);
    }

    #[test]
    fn isolated_cpus_parsed_and_flagged() {
        let s = FakeSys::new();
        s.add_online("0-3");
        s.add_isolated("2-3");
        for cpu in 0u32..4 {
            s.add_cpu(cpu, 0, None);
        }
        s.add_node_cpulist(0, "0-3");
        let h = Host::discover_with(&s.root);
        assert_eq!(h.isolated.iter().copied().collect::<Vec<_>>(), vec![2, 3]);
        assert!(!h.cpus[&0].isolated);
        assert!(!h.cpus[&1].isolated);
        assert!(h.cpus[&2].isolated);
        assert!(h.cpus[&3].isolated);
    }

    #[test]
    fn smt_siblings_populated() {
        let s = FakeSys::new();
        s.add_online("0,4");
        s.add_cpu(0, 0, Some("0,4"));
        s.add_cpu(4, 0, Some("0,4"));
        s.add_node_cpulist(0, "0,4");
        let h = Host::discover_with(&s.root);
        assert_eq!(h.cpus[&0].smt_siblings, vec![0, 4]);
        assert_eq!(h.cpus[&4].smt_siblings, vec![0, 4]);
    }

    #[test]
    fn cpus_on_specific_numa_and_all() {
        let s = FakeSys::new();
        s.add_online("0-3,8-11");
        for cpu in 0u32..4 {
            s.add_cpu(cpu, 0, None);
        }
        for cpu in 8u32..12 {
            s.add_cpu(cpu, 1, None);
        }
        s.add_node_cpulist(0, "0-3");
        s.add_node_cpulist(1, "8-11");
        let h = Host::discover_with(&s.root);
        assert_eq!(h.cpus_on(Some(0)), vec![0, 1, 2, 3]);
        assert_eq!(h.cpus_on(Some(1)), vec![8, 9, 10, 11]);
        assert_eq!(h.cpus_on(None), vec![0, 1, 2, 3, 8, 9, 10, 11]);
        assert_eq!(h.numa_nodes.len(), 2);
        assert_eq!(h.numa_nodes[0].id, 0);
        assert_eq!(h.numa_nodes[1].id, 1);
    }

    #[test]
    fn pcie_root_recorded_for_devices_with_links() {
        let s = FakeSys::new();
        s.add_hca("mlx5_0", "0000:af:00.0", 0, true);
        s.link_pcie("0000:af:00.0", "0000:ae:01.0");
        s.add_nvme("nvme0", "0000:c1:00.0", 1);
        s.link_pcie("0000:c1:00.0", "0000:c0:02.0");
        s.add_nic("eth0", Some("0000:01:00.0"), 0, 1, &[], "up");
        s.link_pcie("0000:01:00.0", "0000:00:01.0");
        let h = Host::discover_with(&s.root);
        assert_eq!(h.hcas[0].pcie_root.as_deref(), Some("0000:ae:01.0"));
        assert_eq!(h.nvmes[0].pcie_root.as_deref(), Some("0000:c0:02.0"));
        assert_eq!(h.nics[0].pcie_root.as_deref(), Some("0000:00:01.0"));
    }

    #[test]
    fn pcie_root_none_when_bdf_or_link_missing() {
        let s = FakeSys::new();
        // HCA has a BDF but no pci symlink -> degrade to NUMA-only.
        s.add_hca("mlx5_0", "0000:af:00.0", 0, true);
        // NIC has no BDF at all.
        s.add_nic("eth0", None, 0, 1, &[], "up");
        let h = Host::discover_with(&s.root);
        assert_eq!(h.hcas[0].pcie_root, None);
        assert_eq!(h.nics[0].pci_bdf, None);
        assert_eq!(h.nics[0].pcie_root, None);
    }

    #[test]
    fn locality_groups_split_by_pcie_root_within_numa() {
        let s = FakeSys::new();
        s.add_online("0-7");
        for cpu in 0u32..8 {
            s.add_cpu(cpu, 0, None);
        }
        s.add_node_cpulist(0, "0-7");
        // Two devices on the same NUMA node but different root ports
        // form two distinct complexes; a NIC shares the NVMe's root
        // port and so pairs with it.
        s.add_hca("mlx5_0", "0000:af:00.0", 0, true);
        s.link_pcie("0000:af:00.0", "0000:ae:01.0");
        s.add_nvme("nvme0", "0000:c1:00.0", 0);
        s.link_pcie("0000:c1:00.0", "0000:c0:02.0");
        s.add_nic("eth0", Some("0000:c2:00.0"), 0, 1, &[], "up");
        s.link_pcie("0000:c2:00.0", "0000:c0:02.0");
        let h = Host::discover_with(&s.root);
        let groups = h.locality_groups();
        assert_eq!(groups.len(), 2);

        let hca_grp = groups
            .iter()
            .find(|g| g.pcie_root.as_deref() == Some("0000:ae:01.0"))
            .unwrap();
        assert_eq!(hca_grp.numa, Some(0));
        assert_eq!(hca_grp.hcas, vec!["mlx5_0".to_string()]);
        assert!(hca_grp.nvmes.is_empty());
        assert!(hca_grp.nics.is_empty());
        assert_eq!(hca_grp.cpus, (0u32..8).collect::<Vec<_>>());

        let storage_grp = groups
            .iter()
            .find(|g| g.pcie_root.as_deref() == Some("0000:c0:02.0"))
            .unwrap();
        assert_eq!(storage_grp.nvmes, vec!["nvme0".to_string()]);
        assert_eq!(storage_grp.nics, vec!["eth0".to_string()]);
        assert!(storage_grp.hcas.is_empty());
    }

    #[test]
    fn locality_groups_degrade_to_numa_only_without_links() {
        let s = FakeSys::new();
        s.add_online("0-3");
        for cpu in 0u32..4 {
            s.add_cpu(cpu, 0, None);
        }
        s.add_node_cpulist(0, "0-3");
        // No pci symlinks: both devices fall into the NUMA-only domain.
        s.add_hca("mlx5_0", "0000:af:00.0", 0, true);
        s.add_nvme("nvme0", "0000:c1:00.0", 0);
        let h = Host::discover_with(&s.root);
        let groups = h.locality_groups();
        assert_eq!(groups.len(), 1);
        assert_eq!(groups[0].numa, Some(0));
        assert_eq!(groups[0].pcie_root, None);
        assert_eq!(groups[0].hcas, vec!["mlx5_0".to_string()]);
        assert_eq!(groups[0].nvmes, vec!["nvme0".to_string()]);
    }
}
