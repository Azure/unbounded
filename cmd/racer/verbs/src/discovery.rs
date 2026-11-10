//! Raw port enumeration helpers: device name validation and sysfs PCI and
//! NUMA normalization. Rail labels, the reservation journal, and port
//! selection are the caller's policy (Racer keeps them in `src/rdma`).

use crate::PortInfo;
use std::path::Path;

/// True if `device` is a safe sysfs name: 1 to 63 bytes, no path tricks.
pub fn valid_device(device: &str) -> bool {
    !device.is_empty()
        && device.len() <= 63
        && !device.contains(['/', '\0', '\r', '\n'])
        && device != "."
        && device != ".."
}

/// Filter, sort, and dedup ports, reading PCI and NUMA info under `root`.
/// Drops invalid names, port zero, and zero GIDs. Unknown PCI addresses sort last.
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
