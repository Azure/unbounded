//! Stable physical-port identities, bounded journals, and NUMA-aware selection.

use crate::{Error, PortInfo, Result};
use std::path::Path;
use std::sync::Mutex;

/// Maximum encoded size of the durable port reservation journal.
pub const MAX_JOURNAL_BYTES: usize = 64 * 1024;

/// A caller-labeled physical port. Labels do not imply authorization or topology.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Binding {
    /// Stable caller label, independent of device enumeration order.
    pub rail: u16,

    /// Local verbs device name.
    pub device: String,

    /// Physical port number.
    pub port: u8,

    /// Required GID, or no GID constraint.
    pub gid: Option<[u8; 16]>,

    /// Explicit locality overrides discovered locality.
    pub numa_node: Option<usize>,
}

/// A consistent view of currently available ports and its change generation.
#[derive(Clone, Default)]
pub struct Snapshot {
    /// Increases when the available bindings change or a journal is restored.
    pub generation: u64,

    /// Available ports, ordered by their reserved labels.
    pub nics: Vec<Binding>,
}

/// Process-wide port inventory retaining labels across withdrawals and restarts.
#[derive(Default)]
pub struct Inventory(Mutex<InventoryState>);

/// Reservations include withdrawn ports so labels are never recycled.
#[derive(Default)]
struct InventoryState {
    assigned: Vec<(String, u8, u16)>,

    snapshot: Snapshot,
}

impl Inventory {
    /// Read the last published snapshot without touching hardware.
    pub fn snapshot(&self) -> Result<Snapshot> {
        Ok(self
            .0
            .lock()
            .map_err(|_| Error::Unavailable)?
            .snapshot
            .clone())
    }

    /// Publish available ports, allocating bounded, permanent physical labels.
    /// Invalid input withdraws all ports without erasing existing reservations.
    pub fn update(&self, mut nics: Vec<Binding>) -> Result<Snapshot> {
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
                None if state.assigned.len() < 1024 => {
                    let rail = state.assigned.len() as u16;
                    // Include JSON escaping and the comma before reserving an ID.
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
            nic.rail = rail;
            true
        });
        nics.sort_by_key(|n| n.rail);
        if state.snapshot.generation == 0 || state.snapshot.nics != nics {
            state.snapshot.generation += 1;
            state.snapshot.nics = nics;
        }
        Ok(state.snapshot.clone())
    }

    /// Encode reservations as JSON arrays of device, port, and label tuples.
    pub fn reservations(&self) -> Result<Vec<u8>> {
        serde_json::to_vec(&self.0.lock().map_err(|_| Error::Unavailable)?.assigned)
            .map_err(|_| Error::InvalidConfiguration)
    }

    /// Restore before issuing labels. Invalid journals leave existing state intact.
    /// Malformed data returns `InvalidRequest`; a poisoned lock is `Unavailable`.
    pub fn restore(&self, bytes: &[u8]) -> Result<()> {
        if bytes.len() > MAX_JOURNAL_BYTES {
            return Err(Error::InvalidRequest);
        }
        let assigned: Vec<(String, u8, u16)> =
            serde_json::from_slice(bytes).map_err(|_| Error::InvalidRequest)?;
        let mut seen = std::collections::BTreeSet::new();
        if assigned.len() > 1024
            || assigned.iter().enumerate().any(|(i, (d, p, r))| {
                !valid_device(d) || *p == 0 || usize::from(*r) != i || !seen.insert((d, p))
            })
        {
            return Err(Error::InvalidRequest);
        }
        if serde_json::to_vec(&assigned)
            .map_err(|_| Error::InvalidRequest)?
            .len()
            > MAX_JOURNAL_BYTES
        {
            return Err(Error::InvalidRequest);
        }
        let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
        state.assigned = assigned;
        state.snapshot.nics.clear();
        state.snapshot.generation += 1;
        Ok(())
    }
}

/// Match physical bindings against exactly one live eligible port each.
pub fn match_ports(
    publication: &[Binding],
    discovered: &[PortInfo],
) -> Result<Vec<(Binding, usize)>> {
    if publication.len() > 64 || discovered.len() > 64 {
        return Err(Error::InvalidConfiguration);
    }
    let mut result: Vec<(Binding, usize)> = Vec::new();
    for published in publication {
        if published.device.is_empty() || published.port == 0 {
            return Err(Error::InvalidConfiguration);
        }
        if result
            .iter()
            .any(|(r, _)| r.device == published.device && r.port == published.port)
        {
            return Err(Error::InvalidConfiguration);
        }
        let candidates: Vec<_> = discovered
            .iter()
            .enumerate()
            .filter(|(_, d)| {
                d.device == published.device
                    && d.port == published.port
                    && d.gid != [0; 16]
                    && published.gid.is_none_or(|gid| d.gid == gid)
            })
            .collect();
        if candidates.len() != 1 || result.iter().any(|(_, index)| *index == candidates[0].0) {
            return Err(Error::Unavailable);
        }
        let mut actual = published.clone();
        actual.numa_node = published.numa_node.or(candidates[0].1.numa_node);
        actual.gid = Some(candidates[0].1.gid);
        result.push((actual, candidates[0].0));
    }
    Ok(result)
}

/// Select at most one port per label, preferring local, unknown, then remote NUMA.
/// Equal candidates and capacity-limited labels are spread by worker ordinal.
pub fn select_worker(
    published: &[Binding],
    discovered: &[Binding],
    worker: usize,
    numa: Option<usize>,
    capacity: usize,
) -> Vec<Binding> {
    let mut rails = std::collections::BTreeMap::<_, Vec<(u8, Binding)>>::new();
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

#[cfg(test)]
mod tests {
    use super::*;

    /// Construct a physical binding with a distinct GID and known locality.
    fn binding(device: &str, rail: u16, numa_node: Option<usize>) -> Binding {
        Binding {
            device: device.into(),
            rail,
            port: 1,
            gid: Some([1; 16]),
            numa_node,
        }
    }

    #[test]
    fn journal_format_and_failed_restore_are_stable() {
        let inventory = Inventory::default();
        inventory
            .update(vec![binding("a", 9, None), binding("b", 9, None)])
            .unwrap();
        let journal = inventory.reservations().unwrap();
        assert_eq!(journal, br#"[["a",1,0],["b",1,1]]"#);
        let generation = inventory.snapshot().unwrap().generation;
        for invalid in [
            b"bad".as_slice(),
            br#"[["a",1,1]]"#,
            br#"[["a",1,0],["a",1,1]]"#,
        ] {
            assert_eq!(inventory.restore(invalid), Err(Error::InvalidRequest));
            assert_eq!(inventory.reservations().unwrap(), journal);
            assert_eq!(inventory.snapshot().unwrap().generation, generation);
        }
        inventory.update(vec![]).unwrap();
        inventory.restore(&journal).unwrap();
        assert_eq!(
            inventory.update(vec![binding("b", 9, None)]).unwrap().nics[0].rail,
            1
        );
    }

    #[test]
    fn selection_vetoes_ambiguity_and_rotates_capacity() {
        let nics = vec![binding("a", 2, Some(0)), binding("b", 7, None)];
        assert_eq!(select_worker(&nics, &nics, 0, Some(0), 1)[0].rail, 2);
        assert_eq!(select_worker(&nics, &nics, 1, Some(0), 1)[0].rail, 7);
        assert!(select_worker(&nics, &nics, 0, None, 0).is_empty());
        let duplicate = vec![nics[0].clone(), nics[0].clone()];
        assert!(select_worker(&nics[..1], &duplicate, 0, None, 64).is_empty());
        let port = PortInfo {
            device: "a".into(),
            port: 1,
            gid: [1; 16],
            numa_node: Some(3),
        };
        assert_eq!(
            match_ports(&nics[..1], std::slice::from_ref(&port)).unwrap()[0]
                .0
                .numa_node,
            Some(0)
        );
        assert_eq!(
            match_ports(&nics[..1], &[port.clone(), port]),
            Err(Error::Unavailable)
        );
        assert_eq!(match_ports(&duplicate, &[]), Err(Error::Unavailable));
    }
}
