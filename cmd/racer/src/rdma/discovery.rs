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

    /// The hardware key reserved for `rail` (`pci:<address>` or a device
    /// name), if any.
    pub fn rail_key(&self, rail: RailId) -> Option<String> {
        self.lock()
            .ok()?
            .assigned
            .iter()
            .find(|(_, _, r)| *r == rail.0)
            .map(|(key, _, _)| key.clone())
    }

    /// The hardware key reserved for `device`'s `port` (`pci:<address>` or
    /// a device name), if published. Look up by device, not by rail: the
    /// active rails come from the node's published mapping, whose rail
    /// numbers need not match this inventory's reservations (for example
    /// when the inventory also holds ports the mapping excludes).
    pub fn device_key(&self, device: &str, port: u8) -> Option<String> {
        let state = self.lock().ok()?;
        let rail = state
            .snapshot
            .nics
            .iter()
            .find(|n| n.device == device && n.port == port)?
            .rail;
        state
            .assigned
            .iter()
            .find(|(_, p, r)| *r == rail.0 && *p == port)
            .map(|(key, _, _)| key.clone())
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

/// Where sysfs puts RDMA devices.
pub const SYSFS_INFINIBAND: &str = "/sys/class/infiniband";

/// What sysfs says about an active rail's device, without touching the
/// device itself (no verbs call, so it is cheap and cannot hang on a sick
/// device).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RailPresence {
    /// The device is there under its activated name (and, for a PCI key,
    /// at its PCI address).
    Present,

    /// The device's PCI address is back under another name, as after an
    /// Azure VF hot-unplug and replug (`mana_0` becoming `roceP30832p0s0`).
    Renamed,

    /// Nothing at the device's name or PCI address.
    Missing,
}

/// The PCI address sysfs reports for `device` under `root`.
fn sysfs_pci(root: &Path, device: &str) -> Option<String> {
    if !valid_device(device) {
        return None;
    }
    std::fs::canonicalize(root.join(device).join("device"))
        .ok()
        .and_then(|p| p.file_name().map(|s| s.to_string_lossy().into_owned()))
}

/// Check `device`, reserved under hardware `key` (see
/// [`Inventory::rail_key`]), in sysfs under `root`.
pub fn rail_presence(root: &Path, device: &str, key: &str) -> RailPresence {
    let Some(bdf) = key.strip_prefix(PCI_KEY_PREFIX) else {
        // Keyed by name: only the name can be checked.
        return if valid_device(device) && root.join(device).exists() {
            RailPresence::Present
        } else {
            RailPresence::Missing
        };
    };
    if sysfs_pci(root, device).as_deref() == Some(bdf) {
        return RailPresence::Present;
    }
    let renamed = std::fs::read_dir(root).ok().is_some_and(|entries| {
        entries
            .filter_map(|entry| entry.ok()?.file_name().into_string().ok())
            .any(|name| sysfs_pci(root, &name).as_deref() == Some(bdf))
    });
    if renamed {
        RailPresence::Renamed
    } else {
        RailPresence::Missing
    }
}

/// Names sysfs under `root` lists for hardware `key` (see
/// [`Inventory::rail_key`]), sorted: every device at a PCI key's address
/// (a VF can carry several, such as a null-GID twin), or the device itself
/// for a name key when it exists.
pub fn rail_devices(root: &Path, key: &str) -> Vec<String> {
    let mut names: Vec<String> = std::fs::read_dir(root)
        .map(|entries| {
            entries
                .filter_map(|entry| entry.ok()?.file_name().into_string().ok())
                .filter(|name| match key.strip_prefix(PCI_KEY_PREFIX) {
                    Some(bdf) => sysfs_pci(root, name).as_deref() == Some(bdf),
                    None => name == key && valid_device(name),
                })
                .collect()
        })
        .unwrap_or_default();
    names.sort();
    names
}

/// Whether sysfs under `root` shows any port of `device` ACTIVE. Tests
/// whose tree has no `ports` directory count as active.
pub fn device_active(root: &Path, device: &str) -> bool {
    if !valid_device(device) {
        return false;
    }
    let Ok(ports) = std::fs::read_dir(root.join(device).join("ports")) else {
        return root.join(device).exists() && !root.join(device).join("ports").exists();
    };
    ports.filter_map(|port| port.ok()).any(|port| {
        std::fs::read_to_string(port.path().join("state"))
            .is_ok_and(|state| state.contains("ACTIVE"))
    })
}

/// Where sysfs lists verbs character devices (`uverbsN/dev` is `major:minor`).
pub const SYSFS_INFINIBAND_VERBS: &str = "/sys/class/infiniband_verbs";

/// Where rdma-core looks for a verbs device by number when its
/// `/dev/infiniband/uverbsN` node cannot be opened.
pub const DEV_CHAR: &str = "/dev/char";

/// `(major, minor)` of every verbs character device sysfs under `root`
/// lists, sorted. Entries without a well-formed `dev` are skipped.
pub fn verbs_char_devices(root: &Path) -> Vec<(u32, u32)> {
    let mut devices: Vec<(u32, u32)> = std::fs::read_dir(root)
        .map(|entries| {
            entries
                .filter_map(|entry| {
                    let entry = entry.ok()?;
                    if !entry.file_name().to_str()?.starts_with("uverbs") {
                        return None;
                    }
                    let dev = std::fs::read_to_string(entry.path().join("dev")).ok()?;
                    let (major, minor) = dev.trim().split_once(':')?;
                    Some((major.parse().ok()?, minor.parse().ok()?))
                })
                .collect()
        })
        .unwrap_or_default();
    devices.sort_unstable();
    devices
}

/// Create a fresh `<dir>/<major>:<minor>` node for every verbs character
/// device sysfs under `sysfs` lists, replacing any old one. Returns how many
/// were created.
///
/// A container's `/dev/infiniband/uverbsN` is a bind mount of the node the
/// runtime saw at start. After a VF replug, opening it keeps reaching the
/// removed device while this process still holds contexts on it (the
/// connection manager's among them), so the device cannot be reopened until
/// the process restarts. rdma-core then falls back to `/dev/char/M:m`
/// (`util/open_cdev.c`), which udev maintains on hosts but which a container
/// lacks; a node created after the replug reaches the new device.
pub fn refresh_char_devices(sysfs: &Path, dir: &Path) -> std::io::Result<usize> {
    let devices = verbs_char_devices(sysfs);
    if devices.is_empty() {
        return Ok(0);
    }
    std::fs::create_dir_all(dir)?;
    for &(major, minor) in &devices {
        let path = dir.join(format!("{major}:{minor}"));
        match std::fs::remove_file(&path) {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error),
        }
        let c_path = std::ffi::CString::new(path.as_os_str().as_encoded_bytes())
            .map_err(|_| std::io::Error::from(std::io::ErrorKind::InvalidInput))?;
        // SAFETY: `c_path` is a valid NUL-terminated path for the call.
        let made = unsafe {
            libc::mknod(
                c_path.as_ptr(),
                libc::S_IFCHR | 0o600,
                libc::makedev(major, minor),
            )
        };
        if made != 0 {
            return Err(std::io::Error::last_os_error());
        }
    }
    Ok(devices.len())
}

/// [`inventory`] with each port's PCI address, when sysfs reports one.
fn keyed_inventory() -> Vec<(RailMapping, Option<String>)> {
    inventory_keyed_at(
        rdma_verbs::inventory().unwrap_or_default(),
        Path::new(SYSFS_INFINIBAND),
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
    /// sysfs lists every device name at a PCI address, and a device counts
    /// as active only once one of its ports is ACTIVE.
    fn rail_devices_list_every_name_at_an_address_and_port_state() {
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("rail-devices-{}", std::process::id())),
        );
        let root = dir.0.join("infiniband");
        let pci = dir.0.join("7870:00:00.0");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::create_dir_all(&pci).unwrap();
        let key = "pci:7870:00:00.0";
        assert!(rail_devices(&root, key).is_empty());
        assert!(rail_devices(&root, "mana_0").is_empty());
        for name in ["roceP30832p0s0", "rdmaP30832p0s0"] {
            std::fs::create_dir_all(root.join(name).join("ports").join("1")).unwrap();
            symlink(&pci, root.join(name).join("device")).unwrap();
        }
        std::fs::create_dir_all(root.join("other")).unwrap();
        assert_eq!(
            rail_devices(&root, key),
            vec!["rdmaP30832p0s0".to_owned(), "roceP30832p0s0".to_owned()]
        );
        assert_eq!(rail_devices(&root, "other"), vec!["other".to_owned()]);
        assert!(rail_devices(&root, "pci:0000:01:00.0").is_empty());
        // Ports exist but none is up yet.
        let state = root.join("roceP30832p0s0/ports/1/state");
        assert!(!device_active(&root, "roceP30832p0s0"));
        std::fs::write(&state, "1: DOWN\n").unwrap();
        assert!(!device_active(&root, "roceP30832p0s0"));
        std::fs::write(&state, "4: ACTIVE\n").unwrap();
        assert!(device_active(&root, "roceP30832p0s0"));
        assert!(device_active(&root, "other"), "no ports directory");
        assert!(!device_active(&root, "missing"));
        assert!(!device_active(&root, "../escape"));
    }

    #[test]
    /// Verbs character device numbers are read from sysfs, and a refresh
    /// replaces stale nodes.
    fn verbs_char_devices_read_numbers_and_refresh_replaces_nodes() {
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("char-devices-{}", std::process::id())),
        );
        let sysfs = dir.0.join("infiniband_verbs");
        let dev = dir.0.join("char");
        assert!(verbs_char_devices(&sysfs).is_empty(), "no sysfs");
        assert_eq!(refresh_char_devices(&sysfs, &dev).unwrap(), 0);
        assert!(!dev.exists(), "nothing to create, no directory");
        for (name, value) in [
            ("uverbs1", "231:193\n"),
            ("uverbs0", "231:192\n"),
            ("uverbs2", "garbage\n"),
            ("abi_version", "1\n"),
        ] {
            std::fs::create_dir_all(sysfs.join(name)).unwrap();
            std::fs::write(sysfs.join(name).join("dev"), value).unwrap();
        }
        assert_eq!(verbs_char_devices(&sysfs), vec![(231, 192), (231, 193)]);
        // Creating device nodes needs CAP_MKNOD: check what is possible.
        std::fs::create_dir_all(&dev).unwrap();
        std::fs::write(dev.join("231:192"), b"stale").unwrap();
        match refresh_char_devices(&sysfs, &dev) {
            Ok(count) => {
                use std::os::unix::fs::{FileTypeExt, MetadataExt};
                assert_eq!(count, 2);
                for minor in [192, 193] {
                    let meta = std::fs::metadata(dev.join(format!("231:{minor}"))).unwrap();
                    assert!(meta.file_type().is_char_device(), "stale node replaced");
                    assert_eq!(meta.rdev(), libc::makedev(231, minor));
                }
            }
            Err(error) => {
                assert_eq!(error.raw_os_error(), Some(libc::EPERM), "{error}");
                assert!(!dev.join("231:192").exists(), "the stale node is gone");
            }
        }
    }

    #[test]
    /// The sysfs probe tells a present device from one renamed at the same
    /// PCI address (a VF replug) and from a missing one, and a name key only
    /// checks the name.
    fn rail_presence_tracks_vf_unplug_and_rename() {
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("rail-presence-{}", std::process::id())),
        );
        let root = dir.0.join("infiniband");
        let pci = dir.0.join("7870:00:00.0");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::create_dir_all(&pci).unwrap();
        let plug = |name: &str| {
            std::fs::create_dir_all(root.join(name)).unwrap();
            symlink(&pci, root.join(name).join("device")).unwrap();
        };
        let key = "pci:7870:00:00.0";
        plug("mana_0");
        assert_eq!(rail_presence(&root, "mana_0", key), RailPresence::Present);
        assert_eq!(
            rail_presence(&root, "mana_0", "pci:0000:01:00.0"),
            RailPresence::Missing,
            "a different device at the name"
        );
        std::fs::remove_dir_all(root.join("mana_0")).unwrap();
        assert_eq!(rail_presence(&root, "mana_0", key), RailPresence::Missing);
        assert_eq!(
            rail_presence(&root, "mana_0", "mana_0"),
            RailPresence::Missing
        );
        plug("roceP30832p0s0");
        assert_eq!(rail_presence(&root, "mana_0", key), RailPresence::Renamed);
        assert_eq!(
            rail_presence(&root, "roceP30832p0s0", key),
            RailPresence::Present
        );
        assert_eq!(
            rail_presence(&root, "roceP30832p0s0", "roceP30832p0s0"),
            RailPresence::Present
        );
        assert_eq!(
            rail_presence(&dir.0.join("absent"), "mana_0", key),
            RailPresence::Missing
        );
    }

    #[test]
    /// A rail's reservation key is readable for the presence probe.
    fn rail_key_names_the_reserved_hardware() {
        let inventory = Inventory::default();
        inventory
            .update_keyed(vec![keyed("mana_0", "7870:00:00.0")])
            .unwrap();
        assert_eq!(
            inventory.rail_key(RailId(0)).as_deref(),
            Some("pci:7870:00:00.0")
        );
        assert_eq!(inventory.rail_key(RailId(1)), None);
    }

    #[test]
    /// A device's key is found by name even when its inventory rail differs
    /// from the published rail: extra ports sorted first must not shift it.
    fn device_key_ignores_rail_numbering() {
        let inventory = Inventory::default();
        inventory
            .update_keyed(vec![
                keyed("ibp210s0f0", "0000:d2:00.0"),
                keyed("mlx5_00", "0000:04:00.0"),
            ])
            .unwrap();
        assert_eq!(
            inventory.device_key("mlx5_00", 1).as_deref(),
            Some("pci:0000:04:00.0")
        );
        assert_eq!(
            inventory.rail_key(RailId(0)).as_deref(),
            Some("pci:0000:d2:00.0"),
            "rail 0 is another device in this inventory"
        );
        assert_eq!(inventory.device_key("mlx5_00", 2), None);
        assert_eq!(inventory.device_key("missing", 1), None);
    }

    #[test]
    /// After a MANA VF replug sysfs lists two devices at the old PCI address:
    /// `rdmaP...` with a null GID and `roceP...` with the RoCE GID. Only the
    /// usable one is inventoried, so the key is not ambiguous and the
    /// replacement keeps the lost device's rail.
    fn vf_replug_with_null_gid_twin_keeps_rail() {
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("vf-replug-{}", std::process::id())),
        );
        let root = dir.0.join("infiniband");
        let pci = dir.0.join("7870:00:00.0");
        std::fs::create_dir_all(&pci).unwrap();
        for name in ["mana_0", "rdmaP30832p0s0", "roceP30832p0s0"] {
            std::fs::create_dir_all(root.join(name)).unwrap();
            symlink(&pci, root.join(name).join("device")).unwrap();
        }
        let port = |device: &str, gid: [u8; 16]| PortInfo {
            device: device.into(),
            port: 1,
            gid,
            numa_node: None,
        };
        let inventory = Inventory::default();
        let before = inventory
            .update_keyed(inventory_keyed_at(vec![port("mana_0", [1; 16])], &root))
            .unwrap();
        assert_eq!(rails(&before), vec![("mana_0".into(), 0)]);
        let after = inventory
            .update_keyed(inventory_keyed_at(
                vec![
                    port("rdmaP30832p0s0", [0; 16]),
                    port("roceP30832p0s0", [1; 16]),
                ],
                &root,
            ))
            .unwrap();
        assert_eq!(rails(&after), vec![("roceP30832p0s0".into(), 0)]);
        assert_eq!(
            inventory.device_key("roceP30832p0s0", 1).as_deref(),
            Some("pci:7870:00:00.0")
        );
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
