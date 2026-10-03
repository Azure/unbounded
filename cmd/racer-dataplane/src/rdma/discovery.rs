//! Pre-enrollment inventory and deterministic per-worker physical NIC selection.
use super::{RailId, RailMapping};
use crate::error::{Error, Result};
use rdma_verbs::PortInfo;
use std::path::Path;
use std::sync::{Arc, Mutex};

/// Shared durable-journal bound for admission, restoration, and enrollment I/O.
pub const MAX_JOURNAL_BYTES: usize = 64 * 1024;

// Native ABI names have 64 bytes including the trailing NUL. These local names
// are also sysfs path components, unlike unrestricted authenticated wire names.
fn valid_device(device: &str) -> bool {
    !device.is_empty()
        && device.len() <= 63
        && !device.contains(['/', '\0', '\r', '\n'])
        && device != "."
        && device != ".."
}

/// One process-wide inventory, shared by enrollment and every worker. Withdrawn
/// ports keep their rail reservation: neither outages nor GID changes renumber
/// surviving ports. Enrollment persists reservations across process restarts.
#[derive(Default)]
pub struct Inventory(Mutex<InventoryState>);
#[derive(Default)]
struct InventoryState {
    assigned: Vec<(String, u8, u16)>,
    snapshot: Snapshot,
}
#[derive(Clone, Default)]
pub struct Snapshot {
    pub generation: u64,
    pub nics: Vec<RailMapping>,
}
impl Inventory {
    pub fn snapshot(&self) -> Result<Snapshot> {
        Ok(self
            .0
            .lock()
            .map_err(|_| Error::Unavailable)?
            .snapshot
            .clone())
    }
    pub fn refresh(&self) -> Result<Snapshot> {
        self.update(inventory())
    }
    pub fn update(&self, mut nics: Vec<RailMapping>) -> Result<Snapshot> {
        let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
        let mut seen = std::collections::BTreeSet::new();
        if nics.len() > 64
            || nics.iter().any(|n| {
                n.port == 0 || !valid_device(&n.device) || !seen.insert((n.device.clone(), n.port))
            })
        {
            nics.clear();
        }
        let mut encoded_bytes = serde_json::to_vec(&state.assigned)
            .map_err(|_| Error::InvalidConfiguration)?
            .len();
        nics.retain_mut(|nic| {
            let rail = state
                .assigned
                .iter()
                .find(|(d, p, _)| *d == nic.device && *p == nic.port)
                .map(|(_, _, r)| *r);
            let rail = match rail {
                Some(rail) => rail,
                // Bound retained tombstones and never recycle an old rail.
                None if state.assigned.len() < 1024 => {
                    let rail = state.assigned.len() as u16;
                    // Account for JSON escaping and the separating comma before
                    // mutating reservations. Full journals withdraw only unknown
                    // ports; known IDs remain usable and renewals remain writable.
                    let Ok(entry) = serde_json::to_vec(&(&nic.device, nic.port, rail)) else {
                        return false;
                    };
                    let added = entry.len() + usize::from(!state.assigned.is_empty());
                    if added > MAX_JOURNAL_BYTES.saturating_sub(encoded_bytes) {
                        return false;
                    }
                    encoded_bytes += added;
                    state.assigned.push((nic.device.clone(), nic.port, rail));
                    rail
                }
                None => return false,
            };
            nic.rail = RailId(rail);
            true
        });
        nics.sort_by_key(|n| n.rail);
        if state.snapshot.generation == 0 || state.snapshot.nics != nics {
            state.snapshot.generation += 1;
            state.snapshot.nics = nics;
        }
        Ok(state.snapshot.clone())
    }
    pub fn reservations(&self) -> Result<Vec<u8>> {
        serde_json::to_vec(&self.0.lock().map_err(|_| Error::Unavailable)?.assigned)
            .map_err(|_| Error::InvalidConfiguration)
    }
    /// Restore before issuance, never discard a corrupt journal and renumber.
    pub fn restore(&self, bytes: &[u8]) -> Result<()> {
        if bytes.len() > MAX_JOURNAL_BYTES {
            return Err(Error::CorruptRecord);
        }
        let assigned: Vec<(String, u8, u16)> =
            serde_json::from_slice(bytes).map_err(|_| Error::CorruptRecord)?;
        let mut seen = std::collections::BTreeSet::new();
        if assigned.len() > 1024
            || assigned.iter().enumerate().any(|(i, (d, p, r))| {
                !valid_device(d) || *p == 0 || usize::from(*r) != i || !seen.insert((d, p))
            })
        {
            return Err(Error::CorruptRecord);
        }
        if serde_json::to_vec(&assigned)
            .map_err(|_| Error::CorruptRecord)?
            .len()
            > MAX_JOURNAL_BYTES
        {
            return Err(Error::CorruptRecord);
        }
        let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
        state.assigned = assigned;
        state.snapshot.nics.clear();
        state.snapshot.generation += 1;
        Ok(())
    }
    pub fn shared() -> Arc<Self> {
        Arc::new(Self::default())
    }
}

/// Discovery failures are optional-transport failures, never HTTP startup failures.
pub fn inventory() -> Vec<RailMapping> {
    inventory_at(
        rdma_verbs::inventory().unwrap_or_default(),
        Path::new("/sys/class/infiniband"),
    )
}

fn inventory_at(mut ports: Vec<PortInfo>, root: &Path) -> Vec<RailMapping> {
    // Names come from the native provider, not membership. Still reject path
    // components before joining sysfs paths.
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
    ports
        .into_iter()
        .take(64)
        .enumerate()
        .map(|(i, (_, p))| RailMapping {
            device: p.device,
            port: p.port,
            rail: RailId(i as u16),
            gid: Some(p.gid),
            numa_node: p.numa_node,
        })
        .collect()
}

/// Select at most one device per rail. Explicit NUMA overrides detected locality;
/// prefer local, then unknown, then remote, spreading equal candidates by worker.
pub fn select_worker(
    published: &[RailMapping],
    discovered: &[RailMapping],
    worker: usize,
    numa: Option<usize>,
    capacity: usize,
) -> Vec<RailMapping> {
    let mut rails = std::collections::BTreeMap::<_, Vec<(u8, RailMapping)>>::new();
    for nic in published {
        let mut matches = discovered.iter().filter(|d| {
            d.device == nic.device
                && d.port == nic.port
                && nic.gid.is_none_or(|gid| d.gid == Some(gid))
        });
        let Some(detected) = matches.next() else {
            continue;
        };
        if matches.next().is_some() {
            continue;
        }
        let mut actual = nic.clone();
        actual.numa_node = nic.numa_node.or(detected.numa_node);
        // Bind the chosen physical GID through activation and revalidation.
        actual.gid = detected.gid;
        let preference = match (numa, actual.numa_node) {
            (Some(a), Some(b)) if a == b => 0,
            (_, None) | (None, _) => 1,
            _ => 2,
        };
        rails
            .entry(nic.rail)
            .or_default()
            .push((preference, actual));
    }
    let mut selected = Vec::new();
    for (_, mut candidates) in rails {
        candidates.sort_by(|(a, x), (b, y)| (a, &x.device, x.port).cmp(&(b, &y.device, y.port)));
        let count = candidates
            .iter()
            .take_while(|(rank, _)| *rank == candidates[0].0)
            .count();
        selected.push(candidates[worker % count].1.clone());
    }
    if !selected.is_empty() {
        let count = selected.len();
        selected.rotate_left(worker % count);
        selected.truncate(capacity);
        selected.sort_by_key(|n| n.rail);
    }
    selected
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn journal_byte_admission_preserves_known_ports_and_restart_at_saturation() {
        // Exercise both maximal ordinary names and JSON-escaped local names.
        for fill in ['x', '"'] {
            let inventory = Inventory::default();
            let nic = |i| RailMapping {
                device: format!("{i:04}{}", fill.to_string().repeat(59)),
                port: 255,
                rail: RailId(0),
                gid: Some([1; 16]),
                numa_node: None,
            };
            let mut accepted = 0;
            for batch in 0..20 {
                let snapshot = inventory
                    .update((batch * 64..(batch + 1) * 64).map(nic).collect())
                    .unwrap();
                accepted += snapshot.nics.len();
                assert!(inventory.reservations().unwrap().len() <= MAX_JOURNAL_BYTES);
            }
            assert!(
                accepted > 0 && accepted < 1024,
                "byte bound must precede identity cap"
            );
            let journal = inventory.reservations().unwrap();
            assert!(inventory.update(vec![nic(2000)]).unwrap().nics.is_empty());
            assert_eq!(inventory.reservations().unwrap(), journal);
            let restarted = Inventory::default();
            restarted.restore(&journal).unwrap();
            let mut old = nic(0);
            old.gid = Some([2; 16]);
            let snapshot = restarted.update(vec![nic(2000), old]).unwrap();
            assert_eq!(snapshot.nics.len(), 1);
            assert_eq!(snapshot.nics[0].rail, RailId(0));
            assert_eq!(snapshot.nics[0].gid, Some([2; 16]));
            assert_eq!(restarted.reservations().unwrap(), journal);
        }
    }
    #[test]
    fn journal_restore_validates_names_ports_and_encoded_bounds_atomically() {
        let inventory = Inventory::default();
        inventory.restore(br#"[["valid",1,0]]"#).unwrap();
        let original = inventory.reservations().unwrap();
        for device in [
            "".to_owned(),
            ".".into(),
            "..".into(),
            "a/b".into(),
            "a\0b".into(),
            "a\nb".into(),
            "a\rb".into(),
            "x".repeat(64),
        ] {
            let bytes = serde_json::to_vec(&vec![(device.clone(), 1u8, 0u16)]).unwrap();
            assert_eq!(inventory.restore(&bytes), Err(Error::CorruptRecord));
            assert!(
                inventory
                    .update(vec![RailMapping {
                        device,
                        port: 1,
                        rail: RailId(0),
                        gid: None,
                        numa_node: None
                    }])
                    .unwrap()
                    .nics
                    .is_empty()
            );
            assert_eq!(inventory.reservations().unwrap(), original);
        }
        assert_eq!(
            inventory.restore(br#"[["valid",0,0]]"#),
            Err(Error::CorruptRecord)
        );
        let oversized = serde_json::to_vec(
            &(0..1024)
                .map(|i| (format!("{i:04}{}", "x".repeat(59)), 255u8, i as u16))
                .collect::<Vec<_>>(),
        )
        .unwrap();
        assert!(oversized.len() > MAX_JOURNAL_BYTES);
        assert_eq!(inventory.restore(&oversized), Err(Error::CorruptRecord));
        let mut exact = original.clone();
        exact.resize(MAX_JOURNAL_BYTES, b' ');
        inventory.restore(&exact).unwrap();
        exact.push(b' ');
        assert_eq!(inventory.restore(&exact), Err(Error::CorruptRecord));
        assert_eq!(inventory.reservations().unwrap(), original);
    }
    #[test]
    fn reservations_survive_withdrawal_gid_change_hotplug_and_restart() {
        let nic = |device: &str, gid| RailMapping {
            device: device.into(),
            port: 1,
            rail: RailId(0),
            gid: Some([gid; 16]),
            numa_node: None,
        };
        let inventory = Inventory::default();
        assert!(inventory.update(vec![]).unwrap().nics.is_empty());
        let first = inventory.update(vec![nic("a", 1), nic("b", 2)]).unwrap();
        assert_eq!(first.nics[1].rail, RailId(1));
        let withdrawn = inventory.update(vec![nic("b", 3)]).unwrap();
        assert_eq!(withdrawn.nics[0].rail, RailId(1));
        assert!(withdrawn.generation > first.generation);
        let journal = inventory.reservations().unwrap();
        let restarted = Inventory::default();
        restarted.restore(&journal).unwrap();
        let next = restarted.update(vec![nic("b", 3), nic("c", 4)]).unwrap();
        assert_eq!(
            next.nics.iter().map(|n| n.rail).collect::<Vec<_>>(),
            vec![RailId(1), RailId(2)]
        );
        assert_eq!(
            restarted.update(vec![nic("a", 5)]).unwrap().nics[0].rail,
            RailId(0)
        );
        assert!(
            restarted
                .update(vec![nic("a", 5), nic("a", 5)])
                .unwrap()
                .nics
                .is_empty()
        );
        assert!(restarted.restore(b"bad").is_err());
        assert!(restarted.restore(br#"[["a",1,0],["b",1,0]]"#).is_err());
    }
    #[test]
    fn sysfs_pci_order_precedes_device_names_and_ports_have_ordinal_rails() {
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                std::fs::remove_dir_all(&self.0).unwrap();
            }
        }
        let dir = Scratch(
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("nic-sysfs-{}", std::process::id())),
        );
        std::fs::create_dir_all(&dir.0).unwrap();
        for (name, bdf, numa) in [
            ("z", "0000:01:00.0", "2\n"),
            ("a", "0000:02:00.0", "-1\n"),
            ("b", "0000:03:00.0", "bad"),
        ] {
            std::fs::create_dir_all(dir.0.join(bdf)).unwrap();
            std::fs::create_dir_all(dir.0.join(name)).unwrap();
            std::fs::write(dir.0.join(bdf).join("numa_node"), numa).unwrap();
            symlink(dir.0.join(bdf), dir.0.join(name).join("device")).unwrap();
        }
        let port = |name: &str, port| PortInfo {
            device: name.into(),
            port,
            gid: [1; 16],
            numa_node: None,
        };
        let nics = inventory_at(
            vec![port("a", 1), port("z", 2), port("b", 1), port("z", 1)],
            &dir.0,
        );
        assert_eq!(
            nics.iter()
                .map(|n| (n.device.as_str(), n.port, n.rail.0, n.numa_node))
                .collect::<Vec<_>>(),
            vec![
                ("z", 1, 0, Some(2)),
                ("z", 2, 1, Some(2)),
                ("a", 1, 2, None),
                ("b", 1, 3, None)
            ]
        );
    }
    #[test]
    fn inventory_absence_and_unknown_numa_are_safe() {
        let root = Path::new("/nonexistent-racer-infiniband");
        assert!(inventory_at(vec![], root).is_empty());
        let port = |device: &str, port| PortInfo {
            device: device.into(),
            port,
            gid: [1; 16],
            numa_node: Some(99),
        };
        let nics = inventory_at(vec![port("z", 2), port("a", 1), port("../bad", 1)], root);
        assert_eq!(nics.len(), 2);
        assert_eq!(nics[0].device, "a");
        assert_eq!(nics[1].rail, RailId(1));
        assert!(nics.iter().all(|n| n.numa_node.is_none()));
    }
    #[test]
    fn worker_local_unknown_remote_override_and_same_rail_spreading() {
        let nic = |device: &str, numa| RailMapping {
            device: device.into(),
            port: 1,
            rail: RailId(7),
            gid: Some([1; 16]),
            numa_node: numa,
        };
        let detected = vec![
            nic("a", Some(0)),
            nic("b", Some(0)),
            nic("c", None),
            nic("d", Some(2)),
        ];
        let selected = |worker, numa| select_worker(&detected, &detected, worker, numa, 64);
        assert_eq!(selected(0, Some(0))[0].device, "a");
        assert_eq!(selected(1, Some(0))[0].device, "b");
        assert_eq!(selected(0, Some(1))[0].device, "c");
        assert_eq!(selected(0, Some(2))[0].device, "d");
        let override_nic = nic("a", Some(3));
        assert_eq!(
            select_worker(&[override_nic], &detected, 0, Some(3), 1)[0].numa_node,
            Some(3)
        );
        assert_eq!(
            select_worker(&detected[..2], &detected, 0, Some(9), 1).len(),
            1
        );
        assert!(select_worker(&detected, &[], 0, None, 64).is_empty());
        assert!(select_worker(&[], &detected, 0, None, 64).is_empty());
        assert!(select_worker(&detected, &detected, 0, None, 0).is_empty());
    }
}
