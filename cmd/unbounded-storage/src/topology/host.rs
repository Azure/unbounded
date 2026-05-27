// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
    pub numa: Option<u16>,
    pub ports_active: bool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Nvme {
    pub dev_name: String,
    pub pci_bdf: Option<String>,
    pub numa: Option<u16>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Host {
    pub cpus: BTreeMap<u32, Cpu>,
    pub numa_nodes: Vec<NumaNode>,
    pub hcas: Vec<Hca>,
    pub nvmes: Vec<Nvme>,
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

        Host {
            cpus,
            numa_nodes,
            hcas,
            nvmes,
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
        let ports_active = has_active_port(sys_root, &dev);
        out.push(Hca {
            dev_name: dev,
            pci_bdf,
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
        out.push(Nvme {
            dev_name: dev,
            pci_bdf,
            numa,
        });
    }
    out
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
}
