//! Live block-device and RDMA topology report for `/debug/hardware`.
//!
//! Discovery reads sysfs attributes and `disk/by-id` symlinks only. It never
//! opens a block device and holds no cache; every call reflects the host at that
//! moment. Missing or malformed individual values become `null`, while a section
//! that cannot be listed records a message in `errors` and the rest of the
//! report is still produced.
//!
//! Every walk runs under a [`Walk`]: each retained allocation is charged to a
//! fixed working-memory budget before it is kept, and the cancellation flag is
//! checked before every read. Exhausting the budget aborts with
//! [`Abort::TooLarge`] instead of materializing the full report first.

use std::cell::Cell;
use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::fs;
use std::io;
use std::io::Read;
use std::path::Path;
use std::sync::Arc;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::Ordering;

use serde::Serialize;

/// Working-memory budget for one discovery.
///
/// The encoded response is capped at `MAX_BODY_BYTES` (about 64 KiB). The
/// in-memory report is charged with per-entry overhead (string and vector
/// headers, struct sizes) that pretty JSON does not carry, while JSON adds keys
/// and indentation the structs do not, so the two are of similar magnitude. Four
/// times the body bound leaves room for that overhead and for the by-id map,
/// which is dropped before encoding, while still aborting long before a host
/// whose report could never fit in one response exhausts memory. Defined here so
/// the diagnostic memory reservation can charge it.
pub const WORKING_BYTES: usize = 4 * ::telemetry::server::MAX_BODY_BYTES;

/// Largest sysfs attribute read. Longer files are treated as malformed (`null`).
pub const MAX_ATTRIBUTE_BYTES: usize = 4096;

/// Charged per retained string, vector slot, or map node on top of its bytes.
const ENTRY_OVERHEAD: usize = 64;

/// Slots added the first time [`Walk::push`] grows an empty vector.
const MIN_GROWTH: usize = 4;

/// Why discovery stopped before producing a report.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Abort {
    /// The working-memory budget was exhausted.
    TooLarge,
    /// The request was cancelled or timed out.
    Cancelled,
}

/// Cancellation flag and remaining working-memory budget for one discovery.
pub struct Walk {
    cancelled: Arc<AtomicBool>,
    remaining: Cell<usize>,
    reads: Cell<usize>,
    #[cfg(test)]
    cancel_after_reads: Option<usize>,
}

impl Walk {
    /// A walk that may retain at most `budget` bytes and stops once `cancelled` is set.
    pub fn new(cancelled: Arc<AtomicBool>, budget: usize) -> Self {
        Self {
            cancelled,
            remaining: Cell::new(budget),
            reads: Cell::new(0),
            #[cfg(test)]
            cancel_after_reads: None,
        }
    }

    /// Fail if the request has been cancelled.
    pub fn check(&self) -> Result<(), Abort> {
        if self.cancelled.load(Ordering::Acquire) {
            Err(Abort::Cancelled)
        } else {
            Ok(())
        }
    }

    /// Charge `bytes` before retaining them; fails without charging on exhaustion.
    pub fn charge(&self, bytes: usize) -> Result<(), Abort> {
        self.check()?;
        let remaining = self
            .remaining
            .get()
            .checked_sub(bytes)
            .ok_or(Abort::TooLarge)?;
        self.remaining.set(remaining);
        Ok(())
    }

    /// Bytes still available.
    pub fn remaining(&self) -> usize {
        self.remaining.get()
    }

    /// Charge one string about to be copied from `value`; `to_owned` on a
    /// `str` allocates exactly `value.len()` bytes.
    fn charge_string(&self, value: &str) -> Result<(), Abort> {
        self.charge(value.len().saturating_add(ENTRY_OVERHEAD))
    }

    /// Charge an already built string by its allocated capacity, which may
    /// exceed its length (for example after `format!`).
    fn charge_owned(&self, value: &String) -> Result<(), Abort> {
        self.charge(value.capacity().saturating_add(ENTRY_OVERHEAD))
    }

    /// Charge `slots` elements of `T` with checked arithmetic.
    fn charge_slots<T>(&self, slots: usize) -> Result<(), Abort> {
        self.charge(
            slots
                .checked_mul(std::mem::size_of::<T>())
                .ok_or(Abort::TooLarge)?,
        )
    }

    /// An empty vector with room for exactly `slots` elements, charged before
    /// it is allocated.
    fn vec<T>(&self, slots: usize) -> Result<Vec<T>, Abort> {
        self.charge_slots::<T>(slots)?;
        let mut vec = Vec::new();
        vec.reserve_exact(slots);
        Ok(vec)
    }

    /// Push onto a vector of unknown final length. When it is full, the new
    /// slots are charged before they are allocated, so every allocated slot,
    /// used or spare, has been charged.
    fn push<T>(&self, vec: &mut Vec<T>, item: T) -> Result<(), Abort> {
        if vec.len() == vec.capacity() {
            let extra = vec.capacity().max(MIN_GROWTH);
            self.charge_slots::<T>(extra)?;
            vec.reserve_exact(extra);
        }
        vec.push(item);
        Ok(())
    }

    /// Check cancellation before one filesystem read and count it.
    fn read(&self) -> Result<(), Abort> {
        self.check()?;
        let reads = self.reads.get() + 1;
        self.reads.set(reads);
        #[cfg(test)]
        if self.cancel_after_reads == Some(reads) {
            self.cancelled.store(true, Ordering::Release);
        }
        Ok(())
    }
}

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
    walk: &Walk,
    sys_root: &Path,
    dev_root: &Path,
    context: &Context,
    inventory: &crate::rdma::Inventory,
) -> Result<Report, Abort> {
    report_with_snapshot(
        walk,
        sys_root,
        dev_root,
        context,
        inventory.snapshot().map_err(|error| format!("{error:?}")),
    )
}

/// Build the full report from an already captured rails snapshot. The inputs are
/// owned and `Send`, so a helper thread can run the sysfs walk off the I/O worker.
pub fn report_with_snapshot(
    walk: &Walk,
    sys_root: &Path,
    dev_root: &Path,
    context: &Context,
    snapshot: Result<crate::rdma::Snapshot, String>,
) -> Result<Report, Abort> {
    let mut report = discover(walk, sys_root, dev_root)?;
    let rdma_rails = match snapshot {
        Ok(snapshot) => {
            let mut rails = walk.vec::<Rail>(snapshot.nics.len())?;
            for nic in &snapshot.nics {
                let gid = nic.gid.as_ref().map(format_gid);
                walk.charge_string(&nic.device)?;
                if let Some(gid) = &gid {
                    walk.charge_owned(gid)?;
                }
                rails.push(Rail {
                    rail: nic.rail.0,
                    device: nic.device.clone(),
                    port: nic.port,
                    gid,
                    numa_node: nic.numa_node,
                });
            }
            Some(Rails {
                generation: snapshot.generation,
                rails,
            })
        }
        Err(error) => {
            push_error(
                walk,
                &mut report.errors,
                format!("racer.rdma_rails: inventory snapshot: {error}"),
            )?;
            None
        }
    };
    for value in context.block_device_selector.iter() {
        walk.charge_string(value)?;
    }
    // `Vec::clone` allocates exactly `len` slots.
    walk.charge_slots::<ClaimedDisk>(context.claimed_disks.len())?;
    for disk in &context.claimed_disks {
        walk.charge_string(&disk.id)?;
        if let Some(dev) = &disk.dev {
            walk.charge_string(dev)?;
        }
    }
    report.racer = Some(Racer {
        block_device_selector: context.block_device_selector.clone(),
        claimed_disks: context.claimed_disks.clone(),
        rdma_rails,
    });
    Ok(report)
}

/// Discover block and RDMA devices from sysfs (`sys_root`) and by-id links (`dev_root`).
pub fn discover(walk: &Walk, sys_root: &Path, dev_root: &Path) -> Result<Report, Abort> {
    let mut report = Report::default();
    let by_id = by_id_links(walk, dev_root, &mut report.errors)?;
    report.block_devices = block_devices(walk, sys_root, &by_id, &mut report.errors)?;
    // The map is dropped before encoding, but it was charged while it lived.
    drop(by_id);
    report.rdma_devices = rdma_devices(walk, sys_root, &mut report.errors)?;
    Ok(report)
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

/// Charge and record one error message.
fn push_error(walk: &Walk, errors: &mut Vec<String>, message: String) -> Result<(), Abort> {
    walk.charge_owned(&message)?;
    walk.push(errors, message)
}

/// Record a fixed-form section failure.
fn section_error(
    walk: &Walk,
    errors: &mut Vec<String>,
    section: &str,
    path: &Path,
    error: &io::Error,
) -> Result<(), Abort> {
    push_error(
        walk,
        errors,
        format!("{section}: {}: {error}", path.display()),
    )
}

/// A directory listing failure.
enum ListError {
    Io(io::Error),
    Abort(Abort),
}

impl From<Abort> for ListError {
    fn from(abort: Abort) -> Self {
        Self::Abort(abort)
    }
}

/// Sorted names of the entries in a directory that `keep` accepts. Entries are
/// read one at a time and each kept name is charged before it is retained.
fn list(walk: &Walk, path: &Path, keep: impl Fn(&str) -> bool) -> Result<Vec<String>, ListError> {
    walk.read()?;
    let mut names = Vec::new();
    for entry in fs::read_dir(path).map_err(ListError::Io)? {
        walk.read()?;
        let Ok(entry) = entry else { continue };
        let Ok(name) = entry.file_name().into_string() else {
            continue;
        };
        if keep(&name) {
            walk.charge_owned(&name)?;
            walk.push(&mut names, name)?;
        }
    }
    names.sort();
    Ok(names)
}

/// Like [`list`] with every entry kept, but a missing or unlistable directory is empty.
fn list_or_empty(walk: &Walk, path: &Path) -> Result<Vec<String>, Abort> {
    match list(walk, path, |_| true) {
        Ok(names) => Ok(names),
        Err(ListError::Io(_)) => Ok(Vec::new()),
        Err(ListError::Abort(abort)) => Err(abort),
    }
}

/// Attribute text up to [`MAX_ATTRIBUTE_BYTES`], or `None` when absent,
/// unreadable, oversized, or not UTF-8. Not charged; callers charge what they keep.
fn raw(walk: &Walk, path: &Path) -> Result<Option<String>, Abort> {
    walk.read()?;
    let Ok(file) = fs::File::open(path) else {
        return Ok(None);
    };
    // Read into a fixed stack buffer so no heap buffer grows past what is kept.
    let mut buffer = [0u8; MAX_ATTRIBUTE_BYTES + 1];
    let mut file = file.take(MAX_ATTRIBUTE_BYTES as u64 + 1);
    let mut len = 0;
    loop {
        match file.read(&mut buffer[len..]) {
            Ok(0) => break,
            Ok(n) => len += n,
            Err(error) if error.kind() == io::ErrorKind::Interrupted => {}
            Err(_) => return Ok(None),
        }
        if len > MAX_ATTRIBUTE_BYTES {
            return Ok(None);
        }
    }
    Ok(std::str::from_utf8(&buffer[..len]).ok().map(str::to_owned))
}

/// Trimmed, charged attribute text, or `None` when absent, unreadable, or empty.
fn text(walk: &Walk, path: &Path) -> Result<Option<String>, Abort> {
    let Some(value) = raw(walk, path)? else {
        return Ok(None);
    };
    let value = value.trim();
    if value.is_empty() {
        return Ok(None);
    }
    walk.charge_string(value)?;
    Ok(Some(value.to_owned()))
}

/// Unsigned decimal attribute.
fn number(walk: &Walk, path: &Path) -> Result<Option<u64>, Abort> {
    Ok(raw(walk, path)?.and_then(|value| value.trim().parse().ok()))
}

/// `0`/`1` attribute.
fn flag(walk: &Walk, path: &Path) -> Result<Option<bool>, Abort> {
    Ok(match raw(walk, path)?.as_deref().map(str::trim) {
        Some("0") => Some(false),
        Some("1") => Some(true),
        _ => None,
    })
}

/// Final path component of a symlink target, charged.
fn link_name(walk: &Walk, path: &Path) -> Result<Option<String>, Abort> {
    walk.read()?;
    let Some(name) = fs::read_link(path)
        .ok()
        .and_then(|target| target.file_name()?.to_str().map(str::to_owned))
    else {
        return Ok(None);
    };
    walk.charge_string(&name)?;
    Ok(Some(name))
}

/// Map kernel device names to the `disk/by-id` link names that point at them.
fn by_id_links(
    walk: &Walk,
    dev_root: &Path,
    errors: &mut Vec<String>,
) -> Result<BTreeMap<String, Vec<String>>, Abort> {
    let directory = dev_root.join("disk/by-id");
    let mut links: BTreeMap<String, Vec<String>> = BTreeMap::new();
    let names = match list(walk, &directory, |_| true) {
        Ok(names) => names,
        Err(ListError::Abort(abort)) => return Err(abort),
        Err(ListError::Io(error)) if error.kind() == io::ErrorKind::NotFound => {
            return Ok(links);
        }
        Err(ListError::Io(error)) => {
            section_error(walk, errors, "by_id", &directory, &error)?;
            return Ok(links);
        }
    };
    for name in names {
        if let Some(target) = link_name(walk, &directory.join(&name))? {
            // Map node; vector slots are charged by `push` and the name by `list`.
            walk.charge(ENTRY_OVERHEAD)?;
            walk.push(links.entry(target).or_default(), name)?;
        }
    }
    Ok(links)
}

/// Charged copy of the by-id names for one device.
fn by_id_for(
    walk: &Walk,
    by_id: &BTreeMap<String, Vec<String>>,
    name: &str,
) -> Result<Vec<String>, Abort> {
    let Some(names) = by_id.get(name) else {
        return Ok(Vec::new());
    };
    let mut copy = walk.vec::<String>(names.len())?;
    for name in names {
        walk.charge_string(name)?;
        copy.push(name.clone());
    }
    Ok(copy)
}

/// Enumerate every entry under `<sys>/block` without filtering.
fn block_devices(
    walk: &Walk,
    sys_root: &Path,
    by_id: &BTreeMap<String, Vec<String>>,
    errors: &mut Vec<String>,
) -> Result<Vec<BlockDevice>, Abort> {
    let root = sys_root.join("block");
    let names = match list(walk, &root, |_| true) {
        Ok(names) => names,
        Err(ListError::Abort(abort)) => return Err(abort),
        Err(ListError::Io(error)) => {
            section_error(walk, errors, "block_devices", &root, &error)?;
            return Ok(Vec::new());
        }
    };
    let mut devices = walk.vec::<BlockDevice>(names.len())?;
    for name in names {
        let path = root.join(&name);
        // Only partition children are retained; other entries are skipped
        // unread and uncharged.
        let children = match list(walk, &path, |child| {
            path.join(child).join("partition").is_file()
        }) {
            Ok(children) => children,
            Err(ListError::Abort(abort)) => return Err(abort),
            Err(ListError::Io(_)) => Vec::new(),
        };
        let mut partitions = walk.vec::<Partition>(children.len())?;
        for child in children {
            let part = path.join(&child);
            partitions.push(Partition {
                dev: text(walk, &part.join("dev"))?,
                size_bytes: sectors(walk, &part.join("size"))?,
                by_id: by_id_for(walk, by_id, &child)?,
                name: child,
            });
        }
        let wwid = match text(walk, &path.join("wwid"))? {
            Some(wwid) => Some(wwid),
            None => text(walk, &path.join("device/wwid"))?,
        };
        devices.push(BlockDevice {
            dev: text(walk, &path.join("dev"))?,
            size_bytes: sectors(walk, &path.join("size"))?,
            logical_block_size: number(walk, &path.join("queue/logical_block_size"))?,
            physical_block_size: number(walk, &path.join("queue/physical_block_size"))?,
            rotational: flag(walk, &path.join("queue/rotational"))?,
            removable: flag(walk, &path.join("removable"))?,
            read_only: flag(walk, &path.join("ro"))?,
            model: text(walk, &path.join("device/model"))?,
            serial: text(walk, &path.join("device/serial"))?,
            wwid,
            by_id: by_id_for(walk, by_id, &name)?,
            partitions,
            holders: list_or_empty(walk, &path.join("holders"))?,
            name,
        });
    }
    Ok(devices)
}

/// Sysfs `size` is always counted in 512-byte sectors.
fn sectors(walk: &Walk, path: &Path) -> Result<Option<u64>, Abort> {
    Ok(number(walk, path)?.and_then(|value| value.checked_mul(512)))
}

/// Strip the numeric prefix sysfs puts on port states, such as `4: ACTIVE`.
fn state(walk: &Walk, path: &Path) -> Result<Option<String>, Abort> {
    let Some(value) = raw(walk, path)? else {
        return Ok(None);
    };
    let value = value.trim();
    let value = match value.split_once(':') {
        Some((_, name)) => name.trim(),
        None => value,
    };
    if value.is_empty() {
        return Ok(None);
    }
    walk.charge_string(value)?;
    Ok(Some(value.to_owned()))
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
fn rdma_devices(
    walk: &Walk,
    sys_root: &Path,
    errors: &mut Vec<String>,
) -> Result<Vec<RdmaDevice>, Abort> {
    let root = sys_root.join("class/infiniband");
    let names = match list(walk, &root, |_| true) {
        Ok(names) => names,
        Err(ListError::Abort(abort)) => return Err(abort),
        Err(ListError::Io(error)) if error.kind() == io::ErrorKind::NotFound => {
            return Ok(Vec::new());
        }
        Err(ListError::Io(error)) => {
            section_error(walk, errors, "rdma_devices", &root, &error)?;
            return Ok(Vec::new());
        }
    };
    let mut devices = walk.vec::<RdmaDevice>(names.len())?;
    for name in names {
        let path = root.join(&name);
        walk.read()?;
        let pci_address = fs::canonicalize(path.join("device"))
            .ok()
            .and_then(|target| target.file_name()?.to_str().map(str::to_owned))
            .filter(|address| is_pci_address(address));
        if let Some(address) = &pci_address {
            walk.charge_string(address)?;
        }
        let numa_node = raw(walk, &path.join("device/numa_node"))?
            .and_then(|value| value.trim().parse::<i64>().ok())
            .and_then(|value| u32::try_from(value).ok());
        let pci_netdevs = list_or_empty(walk, &path.join("device/net"))?;
        let port_names = list_or_empty(walk, &path.join("ports"))?;
        // Upper bound: non-numeric entries are skipped.
        let mut ports = walk.vec::<RdmaPort>(port_names.len())?;
        for port in port_names {
            let Ok(port_number) = port.parse::<u32>() else {
                continue;
            };
            let port = path.join("ports").join(port);
            let mut netdevs = BTreeSet::new();
            // Every GID slot may name a netdev (for example a RoCE VLAN
            // interface absent from device/net). Unused slots fail to read in
            // sysfs and are skipped by `text`.
            let ndevs = port.join("gid_attrs/ndevs");
            for entry in list_or_empty(walk, &ndevs)? {
                if let Some(netdev) = raw(walk, &ndevs.join(entry))? {
                    let netdev = netdev.trim();
                    if !netdev.is_empty() && !netdevs.contains(netdev) {
                        walk.charge_string(netdev)?;
                        netdevs.insert(netdev.to_owned());
                    }
                }
            }
            for netdev in &pci_netdevs {
                // dev_port is zero-based; keep netdevs without it.
                let dev_port =
                    number(walk, &path.join("device/net").join(netdev).join("dev_port"))?;
                if dev_port.is_none_or(|dev_port| dev_port + 1 == u64::from(port_number))
                    && !netdevs.contains(netdev)
                {
                    walk.charge_string(netdev)?;
                    netdevs.insert(netdev.clone());
                }
            }
            let mut netdev_list = walk.vec::<String>(netdevs.len())?;
            netdev_list.extend(netdevs);
            ports.push(RdmaPort {
                port: port_number,
                state: state(walk, &port.join("state"))?,
                phys_state: state(walk, &port.join("phys_state"))?,
                link_layer: text(walk, &port.join("link_layer"))?,
                rate: text(walk, &port.join("rate"))?,
                gid0: text(walk, &port.join("gids/0"))?,
                gid0_type: text(walk, &port.join("gid_attrs/types/0"))?,
                netdevs: netdev_list,
            });
        }
        ports.sort_by_key(|port| port.port);
        devices.push(RdmaDevice {
            node_guid: text(walk, &path.join("node_guid"))?,
            pci_address,
            numa_node,
            ports,
            name,
        });
    }
    Ok(devices)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;
    use std::path::PathBuf;

    fn walk() -> Walk {
        Walk::new(Arc::default(), WORKING_BYTES)
    }

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
        let report = discover(&walk(), &f.sys(), &f.dev()).unwrap();
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
        let report = discover(&walk(), &f.sys(), &f.dev()).unwrap();
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
        let report = discover(&walk(), &f.sys(), &f.dev()).unwrap();
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
        let report = discover(&walk(), &f.sys(), &f.dev()).unwrap();
        assert!(report.errors.is_empty());
        assert!(report.block_devices.is_empty() && report.rdma_devices.is_empty());
    }

    #[test]
    fn unlistable_infiniband_is_an_error() {
        let f = Fixture::new();
        fs::create_dir_all(f.sys().join("block")).unwrap();
        f.write("sys/class/infiniband", "not a directory");
        let report = discover(&walk(), &f.sys(), &f.dev()).unwrap();
        assert_eq!(report.errors.len(), 1);
        assert!(report.errors[0].starts_with("rdma_devices: "));
    }

    #[test]
    fn rdma_ports_states_numa_and_netdevs() {
        let f = Fixture::new();
        fs::create_dir_all(f.sys().join("block")).unwrap();
        rdma_tree(&f);
        let report = discover(&walk(), &f.sys(), &f.dev()).unwrap();
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
        let report = discover(&walk(), &f.sys(), &f.dev()).unwrap();
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
    fn shared_context_is_not_copied_and_report_copy_is_charged() {
        let f = Fixture::new();
        let context = Arc::new(Context {
            block_device_selector: Some("^nvme-".into()),
            claimed_disks: (0..4)
                .map(|i| ClaimedDisk {
                    id: format!("nvme-disk-{i}"),
                    dev: Some(format!("259:{i}")),
                })
                .collect(),
        });
        // Each request clones the pointer, so repeated requests share one context.
        let request = Arc::clone(&context);
        assert!(Arc::ptr_eq(&request, &context));
        let snapshot = || Ok(crate::rdma::Inventory::default().snapshot().unwrap());
        let ok = report_with_snapshot(&walk(), &f.sys(), &f.dev(), &request, snapshot()).unwrap();
        assert_eq!(ok.racer.unwrap().claimed_disks.len(), 4);
        assert_eq!(Arc::strong_count(&context), 2);

        // A context larger than the walk budget still aborts as too large.
        let big = Arc::new(Context {
            block_device_selector: None,
            claimed_disks: vec![
                ClaimedDisk {
                    id: "x".repeat(WORKING_BYTES),
                    dev: None,
                };
                1
            ],
        });
        let shared = Arc::clone(&big);
        assert!(matches!(
            report_with_snapshot(&walk(), &f.sys(), &f.dev(), &shared, snapshot()),
            Err(Abort::TooLarge)
        ));
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
        let report = report(&walk(), &f.sys(), &f.dev(), &context, &inventory).unwrap();
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
        let report = report(&walk(), &f.sys(), &f.dev(), &Context::default(), &inventory).unwrap();
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
        let walk = walk();
        let report = discover(&walk, Path::new("/sys"), Path::new("/dev"))
            .expect("live host fits WORKING_BYTES");
        println!("{}", serde_json::to_string_pretty(&report).unwrap());
        println!(
            "charged {} of {WORKING_BYTES} working bytes",
            WORKING_BYTES - walk.remaining()
        );
    }

    /// Bytes a full fixture walk charges.
    fn charged(f: &Fixture) -> usize {
        let walk = walk();
        discover(&walk, &f.sys(), &f.dev()).unwrap();
        WORKING_BYTES - walk.remaining()
    }

    #[test]
    fn exact_budget_fits_and_one_byte_less_is_too_large() {
        let f = Fixture::new();
        block_tree(&f);
        rdma_tree(&f);
        let needed = charged(&f);
        assert!(needed > 0);
        let exact = Walk::new(Arc::default(), needed);
        let report = discover(&exact, &f.sys(), &f.dev()).unwrap();
        assert_eq!(exact.remaining(), 0);
        assert_eq!(report.block_devices.len(), 2);
        assert_eq!(report.rdma_devices.len(), 2);
        let short = Walk::new(Arc::default(), needed - 1);
        assert_eq!(
            discover(&short, &f.sys(), &f.dev()).unwrap_err(),
            Abort::TooLarge
        );
    }

    #[test]
    fn vec_charges_exact_capacity_before_allocating() {
        let slots = 3;
        let bytes = slots * std::mem::size_of::<BlockDevice>();
        let exact = Walk::new(Arc::default(), bytes);
        let vec = exact.vec::<BlockDevice>(slots).unwrap();
        assert_eq!(vec.capacity(), slots);
        assert_eq!(exact.remaining(), 0);
        let short = Walk::new(Arc::default(), bytes - 1);
        assert_eq!(
            short.vec::<BlockDevice>(slots).unwrap_err(),
            Abort::TooLarge
        );
        assert_eq!(short.remaining(), bytes - 1, "nothing charged on failure");
        let overflow = Walk::new(Arc::default(), WORKING_BYTES);
        assert_eq!(
            overflow.vec::<BlockDevice>(usize::MAX).unwrap_err(),
            Abort::TooLarge
        );
        assert_eq!(overflow.remaining(), WORKING_BYTES);
    }

    #[test]
    fn push_charges_each_growth_before_allocating() {
        let slot = std::mem::size_of::<String>();
        // First growth adds MIN_GROWTH slots, the second doubles to 2 * MIN_GROWTH.
        let budget = 2 * MIN_GROWTH * slot;
        let walk = Walk::new(Arc::default(), budget);
        let mut vec = Vec::new();
        walk.push(&mut vec, String::new()).unwrap();
        assert_eq!(vec.capacity(), MIN_GROWTH);
        assert_eq!(walk.remaining(), budget - MIN_GROWTH * slot);
        for _ in 1..MIN_GROWTH {
            walk.push(&mut vec, String::new()).unwrap();
        }
        assert_eq!(vec.capacity(), MIN_GROWTH, "no growth until full");
        walk.push(&mut vec, String::new()).unwrap();
        assert_eq!(vec.capacity(), 2 * MIN_GROWTH);
        assert_eq!(walk.remaining(), 0);
        for _ in MIN_GROWTH + 1..2 * MIN_GROWTH {
            walk.push(&mut vec, String::new()).unwrap();
        }
        assert_eq!(
            walk.push(&mut vec, String::new()).unwrap_err(),
            Abort::TooLarge
        );
        assert_eq!(vec.capacity(), 2 * MIN_GROWTH, "not grown on failure");

        let short = Walk::new(Arc::default(), MIN_GROWTH * slot - 1);
        let mut vec = Vec::new();
        assert_eq!(
            short.push(&mut vec, String::new()).unwrap_err(),
            Abort::TooLarge
        );
        assert_eq!(vec.capacity(), 0);
    }

    #[test]
    fn walk_vectors_are_allocated_at_their_charged_capacity() {
        let f = Fixture::new();
        block_tree(&f);
        rdma_tree(&f);
        let report = discover(&walk(), &f.sys(), &f.dev()).unwrap();
        assert_eq!(report.block_devices.capacity(), report.block_devices.len());
        for disk in &report.block_devices {
            assert_eq!(disk.partitions.capacity(), disk.partitions.len());
            assert_eq!(disk.by_id.capacity(), disk.by_id.len());
            for part in &disk.partitions {
                assert_eq!(part.by_id.capacity(), part.by_id.len());
            }
        }
        assert_eq!(report.rdma_devices.capacity(), report.rdma_devices.len());
        for device in &report.rdma_devices {
            assert!(device.ports.capacity() >= device.ports.len());
            for port in &device.ports {
                assert_eq!(port.netdevs.capacity(), port.netdevs.len());
            }
        }
    }

    #[test]
    fn budget_exhausted_mid_walk_stops_before_later_reads() {
        let f = Fixture::new();
        block_tree(&f);
        rdma_tree(&f);
        let full = walk();
        discover(&full, &f.sys(), &f.dev()).unwrap();
        let half = Walk::new(Arc::default(), (WORKING_BYTES - full.remaining()) / 2);
        assert_eq!(
            discover(&half, &f.sys(), &f.dev()).unwrap_err(),
            Abort::TooLarge
        );
        assert!(half.reads.get() < full.reads.get());
    }

    #[test]
    fn errors_are_charged() {
        let f = Fixture::new();
        let walk = Walk::new(Arc::default(), 8);
        assert_eq!(
            discover(&walk, &f.sys(), &f.dev()).unwrap_err(),
            Abort::TooLarge
        );
    }

    #[test]
    fn many_entries_exceed_the_budget() {
        let f = Fixture::new();
        let directory = f.0.join("sys/block");
        fs::create_dir_all(&directory).unwrap();
        // Enough empty device directories that their names alone exceed the budget.
        for i in 0..WORKING_BYTES / ENTRY_OVERHEAD + 1 {
            fs::create_dir(directory.join(format!("d{i}"))).unwrap();
        }
        assert_eq!(
            discover(&walk(), &f.sys(), &f.dev()).unwrap_err(),
            Abort::TooLarge
        );
    }

    #[test]
    fn cancelled_before_walk_reads_nothing() {
        let f = Fixture::new();
        block_tree(&f);
        let walk = walk();
        walk.cancelled.store(true, Ordering::Release);
        assert_eq!(
            discover(&walk, &f.sys(), &f.dev()).unwrap_err(),
            Abort::Cancelled
        );
        assert_eq!(walk.reads.get(), 0);
        assert_eq!(walk.remaining(), WORKING_BYTES);
    }

    #[test]
    fn cancelled_during_walk_stops_at_the_next_read() {
        let f = Fixture::new();
        block_tree(&f);
        rdma_tree(&f);
        let mut walk = walk();
        walk.cancel_after_reads = Some(5);
        assert_eq!(
            discover(&walk, &f.sys(), &f.dev()).unwrap_err(),
            Abort::Cancelled
        );
        assert_eq!(walk.reads.get(), 5, "no read after cancellation");
    }

    #[test]
    fn oversized_attribute_is_null() {
        let f = Fixture::new();
        f.write("sys/block/sda/dev", "8:0\n");
        f.write(
            "sys/block/sda/device/model",
            &"m".repeat(MAX_ATTRIBUTE_BYTES + 1),
        );
        f.write(
            "sys/block/sda/device/serial",
            &"s".repeat(MAX_ATTRIBUTE_BYTES),
        );
        let report = discover(&walk(), &f.sys(), &f.dev()).unwrap();
        let disk = &report.block_devices[0];
        assert_eq!(disk.dev.as_deref(), Some("8:0"));
        assert_eq!(disk.model, None);
        assert_eq!(
            disk.serial.as_deref(),
            Some("s".repeat(MAX_ATTRIBUTE_BYTES).as_str())
        );
    }
}
