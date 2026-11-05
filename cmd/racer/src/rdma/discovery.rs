//! Racer's RDMA port policy: durable rail labels, publication matching, and
//! NUMA-aware per-worker selection (`designs/racer-verbs.md`, "Discovery").
//!
//! `rdma_verbs` only enumerates ports ([`rdma_verbs::inventory`]) and
//! normalizes them ([`rdma_verbs::discovery::inventory_at`]); every policy
//! decision here works on Racer's [`RailMapping`] directly.

use std::path::Path;
use std::sync::Mutex;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;

use racer_control_wire::RailId;
use racer_control_wire::RailMapping;
use rdma_verbs::PortInfo;
use rdma_verbs::discovery::valid_device;

use crate::error::Error;
use crate::error::Result;

/// Maximum encoded size of the durable rail reservation journal. Shared by
/// admission, restoration, and enrollment I/O.
pub const MAX_JOURNAL_BYTES: usize = 64 * 1024;

/// Most ports published or labeled at once.
const MAX_PORTS: usize = 64;

/// Most rail labels ever reserved.
const MAX_RAILS: usize = 1024;

/// A consistent view of currently available ports and its change generation.
#[derive(Clone, Default)]
pub struct Snapshot {
    /// Increases when the available ports change or a journal is restored.
    pub generation: u64,

    /// Available ports, ordered by their reserved rails.
    pub nics: Vec<RailMapping>,
}

/// One process-wide inventory, shared by enrollment and every worker.
/// Withdrawn ports keep their rail reservation: neither outages nor GID
/// changes renumber surviving ports. Enrollment persists reservations across
/// process restarts.
#[derive(Default)]
pub struct Inventory {
    state: Mutex<InventoryState>,

    /// The published snapshot's generation, readable without the lock.
    generation: AtomicU64,
}

/// Reservations include withdrawn ports so rails are never recycled.
#[derive(Default)]
struct InventoryState {
    /// `(device, port, rail)`; rail `i` is entry `i`.
    assigned: Vec<(String, u8, u16)>,

    snapshot: Snapshot,
}

impl Inventory {
    fn lock(&self) -> Result<std::sync::MutexGuard<'_, InventoryState>> {
        self.state.lock().map_err(|_| Error::Unavailable)
    }

    /// The published generation, without locking or cloning ports.
    pub fn generation(&self) -> u64 {
        self.generation.load(Ordering::Acquire)
    }

    /// The last published snapshot, without touching hardware.
    pub fn snapshot(&self) -> Result<Snapshot> {
        Ok(self.lock()?.snapshot.clone())
    }

    /// Discover ports and publish them. Discovery failures withdraw RDMA,
    /// not HTTP service.
    pub fn refresh(&self) -> Result<Snapshot> {
        self.update(inventory())
    }

    /// Publish available ports, reserving bounded, permanent rails for new
    /// ones. Invalid input withdraws every port without erasing existing
    /// reservations. A port that would overflow the journal is dropped.
    pub fn update(&self, mut nics: Vec<RailMapping>) -> Result<Snapshot> {
        let mut state = self.lock()?;
        let mut seen = std::collections::BTreeSet::new();
        if nics.len() > MAX_PORTS
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
                None if state.assigned.len() < MAX_RAILS => {
                    let rail = state.assigned.len() as u16;
                    // Include JSON escaping and the comma before reserving a rail.
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
        self.generation
            .store(state.snapshot.generation, Ordering::Release);
        Ok(state.snapshot.clone())
    }

    /// The reservations as a JSON array of `[device, port, rail]` tuples.
    pub fn reservations(&self) -> Result<Vec<u8>> {
        serde_json::to_vec(&self.lock()?.assigned).map_err(|_| Error::InvalidConfiguration)
    }

    /// Restore reservations before issuing rails. A malformed journal is
    /// `CorruptRecord` and leaves existing state intact: a corrupt journal is
    /// never discarded and renumbered.
    pub fn restore(&self, bytes: &[u8]) -> Result<()> {
        if bytes.len() > MAX_JOURNAL_BYTES {
            return Err(Error::CorruptRecord);
        }
        let assigned: Vec<(String, u8, u16)> =
            serde_json::from_slice(bytes).map_err(|_| Error::CorruptRecord)?;
        let mut seen = std::collections::BTreeSet::new();
        if assigned.len() > MAX_RAILS
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
        let mut state = self.lock()?;
        state.assigned = assigned;
        state.snapshot.nics.clear();
        state.snapshot.generation += 1;
        self.generation
            .store(state.snapshot.generation, Ordering::Release);
        Ok(())
    }
}

/// Discovered ports with sequential provisional rails. Discovery failures
/// are optional-transport failures, never HTTP startup failures.
pub fn inventory() -> Vec<RailMapping> {
    inventory_at(
        rdma_verbs::inventory().unwrap_or_default(),
        Path::new("/sys/class/infiniband"),
    )
}

/// [`inventory`] over `ports`, reading PCI and NUMA info under `root`.
pub(crate) fn inventory_at(ports: Vec<PortInfo>, root: &Path) -> Vec<RailMapping> {
    rdma_verbs::discovery::inventory_at(ports, root)
        .into_iter()
        .take(MAX_PORTS)
        .enumerate()
        .map(|(i, p)| RailMapping {
            device: p.device,
            port: p.port,
            rail: RailId(i as u16),
            gid: Some(p.gid),
            numa_node: p.numa_node,
        })
        .collect()
}

/// Match authenticated publication entries against exactly one live
/// eligible port each. Returns each entry, with its live GID and locality
/// (an explicit NUMA node wins), and the port's index. `InvalidConfiguration`
/// for a malformed or duplicate publication, `Unavailable` when a port is
/// missing, ambiguous, or claimed twice.
pub fn match_publication(
    publication: &[RailMapping],
    discovered: &[PortInfo],
) -> Result<Vec<(RailMapping, usize)>> {
    if publication.len() > MAX_PORTS || discovered.len() > MAX_PORTS {
        return Err(Error::InvalidConfiguration);
    }
    let mut result: Vec<(RailMapping, usize)> = Vec::new();
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

/// Select at most one device per rail, up to `capacity` rails. An explicit
/// NUMA node overrides detected locality; prefer local, then unknown, then
/// remote. Equal candidates and capacity-limited rails are spread by
/// `worker` ordinal. Ambiguous matches are vetoed.
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

    /// A port with a distinct GID and known locality.
    fn nic(device: &str, rail: u16, numa_node: Option<usize>) -> RailMapping {
        RailMapping {
            device: device.into(),
            rail: RailId(rail),
            port: 1,
            gid: Some([1; 16]),
            numa_node,
        }
    }

    #[test]
    /// The generation reads without the lock and moves only on published
    /// changes, invalid input (which withdraws), and restores.
    fn generation_is_lock_free_and_tracks_only_published_changes() {
        let inventory = Inventory::default();
        assert_eq!(inventory.generation(), 0);
        let nics = vec![nic("a", 0, None)];
        assert_eq!(inventory.update(nics.clone()).unwrap().generation, 1);
        let guard = inventory.state.lock().unwrap();
        assert_eq!(inventory.generation(), 1);
        drop(guard);
        inventory.update(nics).unwrap();
        assert_eq!(inventory.generation(), 1);
        assert_eq!(inventory.restore(b"bad"), Err(Error::CorruptRecord));
        assert_eq!(inventory.generation(), 1);
        inventory.update(vec![nic("../bad", 0, None)]).unwrap();
        assert_eq!(inventory.generation(), 2);
        assert!(inventory.snapshot().unwrap().nics.is_empty());
        inventory.restore(br#"[["a",1,0]]"#).unwrap();
        assert_eq!(inventory.generation(), 3);
        assert_eq!(inventory.snapshot().unwrap().generation, 3);
    }

    #[test]
    /// The journal encoding is stable and a failed restore changes nothing.
    fn journal_format_and_failed_restore_are_stable() {
        let inventory = Inventory::default();
        inventory
            .update(vec![nic("a", 9, None), nic("b", 9, None)])
            .unwrap();
        let journal = inventory.reservations().unwrap();
        assert_eq!(journal, br#"[["a",1,0],["b",1,1]]"#);
        let generation = inventory.snapshot().unwrap().generation;
        for invalid in [
            b"bad".as_slice(),
            br#"[["a",1,1]]"#,
            br#"[["a",1,0],["a",1,1]]"#,
        ] {
            assert_eq!(inventory.restore(invalid), Err(Error::CorruptRecord));
            assert_eq!(inventory.reservations().unwrap(), journal);
            assert_eq!(inventory.snapshot().unwrap().generation, generation);
        }
        inventory.update(vec![]).unwrap();
        inventory.restore(&journal).unwrap();
        assert_eq!(
            inventory.update(vec![nic("b", 9, None)]).unwrap().nics[0].rail,
            RailId(1)
        );
    }

    #[test]
    /// Selection vetoes ambiguity and rotates capacity-limited rails by
    /// worker; matching keeps an explicit NUMA node and rejects duplicates.
    fn selection_vetoes_ambiguity_and_rotates_capacity() {
        let nics = vec![nic("a", 2, Some(0)), nic("b", 7, None)];
        assert_eq!(
            select_worker(&nics, &nics, 0, Some(0), 1)[0].rail,
            RailId(2)
        );
        assert_eq!(
            select_worker(&nics, &nics, 1, Some(0), 1)[0].rail,
            RailId(7)
        );
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
            match_publication(&nics[..1], std::slice::from_ref(&port)).unwrap()[0]
                .0
                .numa_node,
            Some(0)
        );
        assert_eq!(
            match_publication(&nics[..1], &[port.clone(), port]),
            Err(Error::Unavailable)
        );
        assert_eq!(match_publication(&duplicate, &[]), Err(Error::Unavailable));
    }
}
