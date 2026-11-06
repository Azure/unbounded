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

use racer_wire::control::RailId;
use racer_wire::control::RailMapping;
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
///
/// A rail is reserved for a stable hardware key: the port's PCI address and
/// port number when sysfs reports one, else its device name and port. A
/// device renamed by a VF hot-unplug/replug (for example `mana_0` becoming
/// `roceP30832p0s0` at the same PCI address) therefore keeps its rail. A
/// withdrawn port keeps its reservation until a new port needs a rail; the
/// new port then takes the lowest rail whose port is absent, so a replaced
/// device reuses its predecessor's rail instead of growing the rail space.
/// On a node with no journal, rails follow PCI/port order, so homogeneous
/// nodes agree on what rail N means. Enrollment persists reservations across
/// process restarts.
#[derive(Default)]
pub struct Inventory {
    state: Mutex<InventoryState>,

    /// The published snapshot's generation, readable without the lock.
    generation: AtomicU64,
}

/// Journal format version written by [`Inventory::reservations`].
const JOURNAL_VERSION: u32 = 2;

/// Prefix of a PCI-address hardware key.
const PCI_KEY_PREFIX: &str = "pci:";

/// The durable journal: `rails[i]` is `(key, port, i)`.
#[derive(serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct Journal {
    version: u32,
    rails: Vec<(String, u8, u16)>,
}

/// Reservations include withdrawn ports until a replacement needs the rail.
#[derive(Default)]
struct InventoryState {
    /// `(key, port, rail)`; rail `i` is entry `i`.
    assigned: Vec<(String, u8, u16)>,

    snapshot: Snapshot,
}

/// The hardware key for a port: `pci:<bdf>` when known, else the device name.
fn hardware_key(device: &str, pci: Option<&str>) -> String {
    match pci {
        Some(bdf) => format!("{PCI_KEY_PREFIX}{bdf}"),
        None => device.to_owned(),
    }
}

/// True if `key` is a device name or a `pci:` key with a safe address.
fn valid_key(key: &str) -> bool {
    match key.strip_prefix(PCI_KEY_PREFIX) {
        Some(bdf) => valid_device(bdf),
        None => valid_device(key),
    }
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
        self.update_keyed(keyed_inventory())
    }

    /// [`Self::update_keyed`] keyed by device name alone.
    pub fn update(&self, nics: Vec<RailMapping>) -> Result<Snapshot> {
        self.update_keyed(nics.into_iter().map(|n| (n, None)).collect())
    }

    /// Publish available ports with their PCI addresses, reserving bounded
    /// rails for new ones. Invalid input withdraws every port without
    /// erasing existing reservations. A port that would overflow the journal
    /// is dropped.
    pub fn update_keyed(&self, mut nics: Vec<(RailMapping, Option<String>)>) -> Result<Snapshot> {
        let mut state = self.lock()?;
        let mut seen = std::collections::BTreeSet::new();
        let mut keys = std::collections::BTreeSet::new();
        if nics.len() > MAX_PORTS
            || nics.iter().any(|(n, pci)| {
                n.port == 0
                    || !valid_device(&n.device)
                    || pci.as_deref().is_some_and(|p| !valid_device(p))
                    || !seen.insert((n.device.clone(), n.port))
                    || !keys.insert((hardware_key(&n.device, pci.as_deref()), n.port))
            })
        {
            nics.clear();
            keys.clear();
        }
        let mut published = Vec::with_capacity(nics.len());
        for (mut nic, pci) in nics {
            let key = hardware_key(&nic.device, pci.as_deref());
            let known = state
                .assigned
                .iter()
                .find(|(k, p, _)| *k == key && *p == nic.port)
                .map(|(_, _, r)| *r);
            let rail = match known {
                Some(rail) => rail,
                None => {
                    // Reuse the lowest rail whose port is absent, else append.
                    let reusable = state
                        .assigned
                        .iter()
                        .find(|(k, p, _)| !keys.contains(&(k.clone(), *p)))
                        .map(|(_, _, r)| *r);
                    let rail = match reusable {
                        Some(rail) => rail,
                        None if state.assigned.len() < MAX_RAILS => state.assigned.len() as u16,
                        None => continue,
                    };
                    let index = usize::from(rail);
                    let reservation = (key, nic.port, rail);
                    let previous = if index < state.assigned.len() {
                        Some(std::mem::replace(&mut state.assigned[index], reservation))
                    } else {
                        state.assigned.push(reservation);
                        None
                    };
                    // Keep the encoded journal within bounds, or undo.
                    if journal_bytes(&state.assigned).map_or(true, |b| b.len() > MAX_JOURNAL_BYTES)
                    {
                        match previous {
                            Some(old) => state.assigned[index] = old,
                            None => drop(state.assigned.pop()),
                        }
                        continue;
                    }
                    rail
                }
            };
            nic.rail = RailId(rail);
            published.push(nic);
        }
        published.sort_by_key(|n| n.rail);
        if state.snapshot.generation == 0 || state.snapshot.nics != published {
            state.snapshot.generation += 1;
            state.snapshot.nics = published;
        }
        self.generation
            .store(state.snapshot.generation, Ordering::Release);
        Ok(state.snapshot.clone())
    }

    /// The reservations as a versioned JSON journal of `[key, port, rail]`
    /// tuples, where `key` is `pci:<address>` or a device name.
    pub fn reservations(&self) -> Result<Vec<u8>> {
        journal_bytes(&self.lock()?.assigned)
    }

    /// Restore reservations before issuing rails. A malformed journal is
    /// `CorruptRecord` and leaves existing state intact: a corrupt journal is
    /// never discarded and renumbered. A legacy (version 1) journal, a bare
    /// array keyed by device name, is discarded: its names do not survive
    /// device renames and may have drifted from other nodes, so rails are
    /// renumbered from PCI/port order.
    pub fn restore(&self, bytes: &[u8]) -> Result<()> {
        if bytes.len() > MAX_JOURNAL_BYTES {
            return Err(Error::CorruptRecord);
        }
        let assigned = match serde_json::from_slice::<Journal>(bytes) {
            Ok(journal) if journal.version == JOURNAL_VERSION => journal.rails,
            Ok(_) => return Err(Error::CorruptRecord),
            Err(_) => {
                let legacy: Vec<(String, u8, u16)> =
                    serde_json::from_slice(bytes).map_err(|_| Error::CorruptRecord)?;
                validate(&legacy)?;
                Vec::new()
            }
        };
        validate(&assigned)?;
        if journal_bytes(&assigned)
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

/// Reject a journal with an invalid key or port, a gap, or a duplicate.
fn validate(assigned: &[(String, u8, u16)]) -> Result<()> {
    let mut seen = std::collections::BTreeSet::new();
    if assigned.len() > MAX_RAILS
        || assigned.iter().enumerate().any(|(i, (k, p, r))| {
            !valid_key(k) || *p == 0 || usize::from(*r) != i || !seen.insert((k, p))
        })
    {
        return Err(Error::CorruptRecord);
    }
    Ok(())
}

/// Encode the versioned journal.
fn journal_bytes(assigned: &[(String, u8, u16)]) -> Result<Vec<u8>> {
    serde_json::to_vec(&Journal {
        version: JOURNAL_VERSION,
        rails: assigned.to_vec(),
    })
    .map_err(|_| Error::InvalidConfiguration)
}

/// Discovered ports with sequential provisional rails. Discovery failures
/// are optional-transport failures, never HTTP startup failures.
pub fn inventory() -> Vec<RailMapping> {
    keyed_inventory().into_iter().map(|(n, _)| n).collect()
}

/// [`inventory`] with each port's PCI address, when sysfs reports one.
fn keyed_inventory() -> Vec<(RailMapping, Option<String>)> {
    inventory_keyed_at(
        rdma_verbs::inventory().unwrap_or_default(),
        Path::new("/sys/class/infiniband"),
    )
}

/// [`inventory`] over `ports`, reading PCI and NUMA info under `root`.
#[cfg(test)]
pub(crate) fn inventory_at(ports: Vec<PortInfo>, root: &Path) -> Vec<RailMapping> {
    inventory_keyed_at(ports, root)
        .into_iter()
        .map(|(n, _)| n)
        .collect()
}

/// [`inventory_at`] with each port's PCI address, when sysfs reports one.
pub(crate) fn inventory_keyed_at(
    ports: Vec<PortInfo>,
    root: &Path,
) -> Vec<(RailMapping, Option<String>)> {
    rdma_verbs::discovery::inventory_at(ports, root)
        .into_iter()
        .take(MAX_PORTS)
        .enumerate()
        .map(|(i, p)| {
            let pci = std::fs::canonicalize(root.join(&p.device).join("device"))
                .ok()
                .and_then(|p| p.file_name().map(|s| s.to_string_lossy().into_owned()));
            let nic = RailMapping {
                device: p.device,
                port: p.port,
                rail: RailId(i as u16),
                gid: Some(p.gid),
                numa_node: p.numa_node,
            };
            (nic, pci)
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
        assert_eq!(journal, br#"{"version":2,"rails":[["a",1,0],["b",1,1]]}"#);
        let generation = inventory.snapshot().unwrap().generation;
        for invalid in [
            b"bad".as_slice(),
            br#"[["a",1,1]]"#,
            br#"[["a",1,0],["a",1,1]]"#,
            br#"{"version":2,"rails":[["a",1,1]]}"#,
            br#"{"version":3,"rails":[]}"#,
            br#"{"version":2,"rails":[["pci:",1,0]]}"#,
            br#"{"version":2,"rails":[],"extra":1}"#,
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

    /// A keyed port at `pci` with a distinct GID.
    fn keyed(device: &str, pci: &str) -> (RailMapping, Option<String>) {
        (nic(device, 0, None), Some(pci.into()))
    }

    fn rails(snapshot: &Snapshot) -> Vec<(String, u16)> {
        snapshot
            .nics
            .iter()
            .map(|n| (n.device.clone(), n.rail.0))
            .collect()
    }

    #[test]
    /// A VF hot-replug that renames the device at the same PCI address keeps
    /// its rail, in process and across a restart.
    fn renamed_device_keeps_rail() {
        let inventory = Inventory::default();
        inventory
            .update_keyed(vec![keyed("mana_0", "7870:00:00.0")])
            .unwrap();
        inventory.update_keyed(vec![]).unwrap();
        let renamed = inventory
            .update_keyed(vec![keyed("roceP30832p0s0", "7870:00:00.0")])
            .unwrap();
        assert_eq!(rails(&renamed), vec![("roceP30832p0s0".into(), 0)]);
        let journal = inventory.reservations().unwrap();
        assert_eq!(
            journal,
            br#"{"version":2,"rails":[["pci:7870:00:00.0",1,0]]}"#
        );
        let restarted = Inventory::default();
        restarted.restore(&journal).unwrap();
        let snapshot = restarted
            .update_keyed(vec![keyed("mana_0", "7870:00:00.0")])
            .unwrap();
        assert_eq!(rails(&snapshot), vec![("mana_0".into(), 0)]);
    }

    #[test]
    /// A replacement at a new address reuses the withdrawn port's rail;
    /// present ports keep theirs.
    fn replaced_device_reuses_rail() {
        let inventory = Inventory::default();
        inventory
            .update_keyed(vec![keyed("a", "0000:01:00.0"), keyed("b", "0000:02:00.0")])
            .unwrap();
        let replaced = inventory
            .update_keyed(vec![keyed("b", "0000:02:00.0"), keyed("c", "0000:09:00.0")])
            .unwrap();
        assert_eq!(rails(&replaced), vec![("c".into(), 0), ("b".into(), 1)]);
        let journal: serde_json::Value =
            serde_json::from_slice(&inventory.reservations().unwrap()).unwrap();
        assert_eq!(journal["rails"].as_array().unwrap().len(), 2);
    }

    #[test]
    /// Nodes with the same hardware publish the same rails regardless of
    /// journal history, and a drifted legacy journal self-corrects.
    fn homogeneous_nodes_agree_and_legacy_journal_self_corrects() {
        let fresh = Inventory::default();
        let hardware = vec![keyed("roceP30832p0s0", "7870:00:00.0")];
        let expected = rails(&fresh.update_keyed(hardware.clone()).unwrap());
        assert_eq!(expected, vec![("roceP30832p0s0".into(), 0)]);
        // The live drift: the old name holds rail 0, the new name rail 1.
        let drifted = Inventory::default();
        drifted
            .restore(br#"[["mana_0",1,0],["roceP30832p0s0",1,1]]"#)
            .unwrap();
        assert_eq!(rails(&drifted.update_keyed(hardware).unwrap()), expected);
        assert_eq!(
            drifted.reservations().unwrap(),
            fresh.reservations().unwrap()
        );
        // A renamed node with a version 2 journal also agrees.
        let renamed = Inventory::default();
        renamed
            .restore(br#"{"version":2,"rails":[["pci:7870:00:00.0",1,0]]}"#)
            .unwrap();
        let snapshot = renamed
            .update_keyed(vec![keyed("mana_0", "7870:00:00.0")])
            .unwrap();
        assert_eq!(snapshot.nics[0].rail, RailId(0));
    }

    #[test]
    /// Two names at one PCI address and port are ambiguous and withdraw.
    fn duplicate_hardware_keys_withdraw() {
        let inventory = Inventory::default();
        let snapshot = inventory
            .update_keyed(vec![keyed("a", "0000:01:00.0"), keyed("b", "0000:01:00.0")])
            .unwrap();
        assert!(snapshot.nics.is_empty());
        assert!(
            inventory
                .update_keyed(vec![keyed("a", "../x")])
                .unwrap()
                .nics
                .is_empty()
        );
    }
}
