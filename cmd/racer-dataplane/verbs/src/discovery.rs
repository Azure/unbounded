//! Synchronous physical inventory normalization, without application rail IDs.
use crate::PortInfo;
use std::path::Path;

/// Native ABI names have 64 bytes including NUL and are sysfs path components.
pub fn valid_device(device: &str) -> bool {
    !device.is_empty()
        && device.len() <= 63
        && !device.contains(['/', '\0', '\r', '\n'])
        && device != "."
        && device != ".."
}

/// Read PCI and NUMA metadata and return unique eligible physical ports.
/// Known PCI paths sort first, then PCI path, port number, and device name.
/// Equal entries retain provider order, including which duplicate GID survives.
/// Missing/invalid NUMA metadata replaces provider locality with unknown.
/// The caller applies its own inventory bound and assigns logical identities.
pub fn inventory_at(mut ports: Vec<PortInfo>, root: &Path) -> Vec<PortInfo> {
    ports.retain(|p| valid_device(&p.device) && p.port != 0 && p.gid != [0; 16]);
    let mut ports: Vec<_> = ports
        .into_iter()
        .map(|mut port| {
            let device = root.join(&port.device).join("device");
            let bdf = std::fs::canonicalize(&device)
                .ok()
                .and_then(|p| p.file_name().map(|s| s.to_string_lossy().into_owned()));
            port.numa_node = std::fs::read_to_string(device.join("numa_node"))
                .ok()
                .and_then(|s| s.trim().parse::<u32>().ok())
                .map(|n| n as usize);
            (bdf, port)
        })
        .collect();
    ports.sort_by(|(a, x), (b, y)| {
        (a.is_none(), a, x.port, &x.device).cmp(&(b.is_none(), b, y.port, &y.device))
    });
    let mut physical = std::collections::BTreeSet::new();
    ports.retain(|(_, p)| physical.insert((p.device.clone(), p.port)));
    ports.into_iter().map(|(_, p)| p).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn port(device: &str, port: u8, gid: u8) -> PortInfo {
        PortInfo {
            device: device.into(),
            port,
            gid: [gid; 16],
            numa_node: Some(99),
        }
    }

    #[test]
    fn names_obey_native_and_path_bounds() {
        for name in [
            "",
            ".",
            "..",
            "a/b",
            "a\0b",
            "a\rb",
            "a\nb",
            &"x".repeat(64),
        ] {
            assert!(!valid_device(name));
        }
        for name in ["mlx5_0", "a b", "a\"b", &"x".repeat(63)] {
            assert!(valid_device(name));
        }
    }

    #[test]
    fn unknown_pci_sorts_by_port_then_name_and_keeps_first_duplicate() {
        let ports = inventory_at(
            vec![
                port("z", 2, 1),
                port("b", 1, 2),
                port("a", 1, 3),
                port("b", 1, 4),
                port("../bad", 1, 1),
                port("zero", 0, 1),
                port("gid", 1, 0),
            ],
            Path::new("/nonexistent-rdma-inventory"),
        );
        assert_eq!(
            ports
                .iter()
                .map(|p| (p.device.as_str(), p.port, p.gid[0], p.numa_node))
                .collect::<Vec<_>>(),
            [("a", 1, 3, None), ("b", 1, 2, None), ("z", 2, 1, None)]
        );
        assert!(inventory_at(vec![], Path::new(".")).is_empty());
    }

    #[test]
    fn pci_order_and_numa_metadata_are_preserved_without_a_policy_cap() {
        struct Fixture(std::path::PathBuf);
        impl Drop for Fixture {
            fn drop(&mut self) {
                std::fs::remove_dir_all(&self.0).unwrap();
            }
        }
        let root = Fixture(
            std::env::current_dir()
                .unwrap()
                .join(format!(".inventory-test-{}", std::process::id())),
        );
        std::fs::create_dir(&root.0).unwrap();
        for (name, bdf, numa) in [("z", "0000:01:00.0", " 7\n"), ("a", "0000:02:00.0", "-1\n")] {
            let physical = root.0.join(bdf);
            std::fs::create_dir(&physical).unwrap();
            std::fs::write(physical.join("numa_node"), numa).unwrap();
            std::fs::create_dir(root.0.join(name)).unwrap();
            std::os::unix::fs::symlink(&physical, root.0.join(name).join("device")).unwrap();
        }
        let mut input = vec![port("a", 1, 1), port("z", 2, 1), port("z", 1, 1)];
        input.extend((0..65).map(|i| port(&format!("unknown{i}"), 1, 1)));
        let ports = inventory_at(input, &root.0);
        assert_eq!(ports.len(), 68);
        assert_eq!(
            ports[..3]
                .iter()
                .map(|p| (p.device.as_str(), p.port, p.numa_node))
                .collect::<Vec<_>>(),
            [("z", 1, Some(7)), ("z", 2, Some(7)), ("a", 1, None)]
        );
    }
}
