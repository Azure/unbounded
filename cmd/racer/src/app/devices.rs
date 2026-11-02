//! Startup-only raw-device discovery on administrator-approved whole disks.
//!
//! Only the reserved guard regions are read. They are written only to wipe an
//! approved disk whose guards are not already zero.

use crate::config::Config;
use crate::error::{Error, Result};
use page_alloc::Alignment;
use racer_cluster_control::disks::DiskReason;
use racer_control_wire::valid_disk_id;
use sha2::{Digest, Sha256};
use std::fs::{self, File};
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::fs::{FileExt, FileTypeExt, MetadataExt};
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

#[cfg(test)]
mod loop_test;

pub const MAX_SEGMENTS: u64 = 1_000_000;
const MAX_PAGES: u64 = 1_000_000;
const MAX_CHECKPOINT: usize = 512 * 1024 * 1024;
pub(crate) const GUARD_BYTES: u64 = 1024 * 1024;

pub struct Device {
    file: Arc<File>,
    id: String,
    bytes: u64,
    alignment: Alignment,
}

pub struct Placement {
    pub file: Arc<File>,
    pub offset: u64,
}

pub struct WorkerDevices {
    pub placements: Vec<Placement>,
    pub disk_groups: Vec<(Arc<str>, usize)>,
    pub alignment: Alignment,
    pub digest: [u8; 32],
    pub page_entries: usize,
}

pub struct Plan {
    pub workers: Vec<WorkerDevices>,
    pub checkpoint_bytes: usize,
}

impl Plan {
    /// Distinct claimed devices by id, with `MAJ:MIN` from the already-open descriptor.
    pub fn claimed_disks(&self) -> Vec<crate::hardware::ClaimedDisk> {
        use std::os::unix::fs::MetadataExt;
        let mut claimed = std::collections::BTreeMap::new();
        for worker in &self.workers {
            let mut start = 0;
            for (group, count) in &worker.disk_groups {
                let id = group.strip_prefix("device:").unwrap_or(group);
                let dev = worker
                    .placements
                    .get(start)
                    .and_then(|placement| placement.file.metadata().ok())
                    .map(|metadata| crate::hardware::format_dev(metadata.rdev()));
                claimed.entry(id.to_owned()).or_insert(dev);
                start += count;
            }
        }
        claimed
            .into_iter()
            .map(|(id, dev)| crate::hardware::ClaimedDisk { id, dev })
            .collect()
    }
}

/// What discovery did with one approved disk ID.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Outcome {
    /// Opened, validated, and used as cache storage.
    Claimed,
    /// Present, but refused for the reason given.
    Rejected(DiskReason),
    /// No `disk/by-id` entry with this name.
    Missing,
}

/// One approved disk ID and its discovery outcome.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Approval {
    /// Approved `disk/by-id` basename.
    pub id: String,
    /// Device number the ID resolved to, when it resolved.
    pub rdev: Option<u64>,
    pub outcome: Outcome,
}

impl Approval {
    /// The `/debug/hardware` and Pod report view of this outcome.
    pub fn hardware(&self) -> crate::hardware::Approved {
        use racer_cluster_control::disks::DiskState;
        let (state, reason) = match self.outcome {
            Outcome::Claimed => (DiskState::Claimed, None),
            Outcome::Rejected(reason) => (DiskState::Rejected, Some(reason)),
            Outcome::Missing => (DiskState::Missing, None),
        };
        crate::hardware::Approved {
            id: self.id.clone(),
            dev: self.rdev.map(crate::hardware::format_dev),
            state,
            reason,
        }
    }
}

/// Startup discovery result: the storage plan, if any disk was usable, and
/// the outcome of every approved ID.
#[derive(Default)]
pub struct Discovery {
    pub plan: Option<Plan>,
    pub approvals: Vec<Approval>,
}

/// A refused disk: the reported reason and the underlying error for logs.
#[derive(Debug)]
struct Refusal {
    reason: DiskReason,
    error: io::Error,
}

impl Refusal {
    fn new(reason: DiskReason, error: impl Into<Box<dyn std::error::Error + Send + Sync>>) -> Self {
        Self {
            reason,
            error: io::Error::other(error),
        }
    }

    fn io(reason: DiskReason) -> impl FnOnce(io::Error) -> Self {
        move |error| Self { reason, error }
    }
}

/// Open, prepare, and plan storage on the approved whole disks.
///
/// Each approved ID must be an exact `disk/by-id` basename under the device
/// directory. Aliases of one disk are opened once. Approval is consent to
/// wipe: a disk whose guard regions are not zero has them zeroed (see
/// [`prepare_guards`]). Disks that are mounted, held, read-only, too small,
/// busy, or partitions are refused. When no disk is usable, the plan is
/// `None` and storage falls back to slab files.
pub fn discover(config: &Config, approved: &[String], workers: usize) -> Discovery {
    let mut ids: Vec<&String> = Vec::new();
    for id in approved {
        if !ids.contains(&id) {
            ids.push(id);
        }
    }
    if ids.is_empty() {
        return Discovery::default();
    }
    let mut approvals: Vec<Approval> = ids
        .iter()
        .map(|id| Approval {
            id: (*id).clone(),
            rdev: None,
            outcome: Outcome::Missing,
        })
        .collect();
    let root = match fs::canonicalize(&config.device_directory) {
        Ok(root) => root,
        Err(error) => {
            eprintln!(
                "racer: device directory {:?} unavailable; using slab files: {error}",
                config.device_directory
            );
            return Discovery {
                plan: None,
                approvals,
            };
        }
    };
    let sys = Path::new(SYS_DEV_BLOCK);
    // Resolve every approved ID without opening anything.
    let mut candidates = Vec::new();
    let mut wwids: Vec<(String, u64)> = Vec::new();
    let mut first_by_rdev: Vec<(u64, usize)> = Vec::new();
    for (index, approval) in approvals.iter_mut().enumerate() {
        if !valid_disk_id(&approval.id) {
            approval.outcome = Outcome::Rejected(DiskReason::NotWholeDisk);
            continue;
        }
        let path = root.join("disk/by-id").join(&approval.id);
        match fs::symlink_metadata(&path) {
            Err(error) if error.kind() == io::ErrorKind::NotFound => continue,
            Err(error) => {
                eprintln!("racer: disk {:?}: {error}", approval.id);
                approval.outcome = Outcome::Rejected(DiskReason::IoError);
                continue;
            }
            Ok(_) => {}
        }
        let metadata = match fs::metadata(&path) {
            Ok(metadata) => metadata,
            Err(error) => {
                eprintln!("racer: disk {:?}: {error}", approval.id);
                approval.outcome = if error.kind() == io::ErrorKind::NotFound {
                    Outcome::Missing
                } else {
                    Outcome::Rejected(DiskReason::IoError)
                };
                continue;
            }
        };
        if !metadata.file_type().is_block_device() {
            eprintln!("racer: disk {:?}: not a block device", approval.id);
            approval.outcome = Outcome::Rejected(DiskReason::NotWholeDisk);
            continue;
        }
        let rdev = metadata.rdev();
        approval.rdev = Some(rdev);
        if first_by_rdev.iter().any(|(seen, _)| *seen == rdev) {
            // An alias of an earlier approved ID; it takes that outcome below.
            approval.outcome = Outcome::Rejected(DiskReason::Duplicate);
            continue;
        }
        first_by_rdev.push((rdev, index));
        let wwid = sys_text(&sys.join(dev_number(rdev)).join("wwid"))
            .or_else(|| sys_text(&sys.join(dev_number(rdev)).join("device/wwid")));
        if let Some(wwid) = wwid {
            if wwids
                .iter()
                .any(|(seen, other)| *seen == wwid && *other != rdev)
            {
                eprintln!(
                    "racer: disk {:?} is another path to an approved disk; refusing it",
                    approval.id
                );
                approval.outcome = Outcome::Rejected(DiskReason::Duplicate);
                continue;
            }
            wwids.push((wwid, rdev));
        }
        candidates.push((approval.id.clone(), path));
    }
    let mut devices = Vec::new();
    let mut refused: Vec<(String, DiskReason)> = Vec::new();
    let still_busy = open_with_busy_wait(
        candidates,
        config.device_busy_wait,
        &mut SystemClock(std::time::Instant::now()),
        |(id, path): &(String, std::path::PathBuf)| match open_device(
            &root,
            sys,
            path,
            id,
            config.segment_bytes,
        ) {
            Ok(device) => {
                devices.push(device);
                Ok(())
            }
            Err(refusal) if refusal.reason == DiskReason::Busy && is_busy(&refusal.error) => {
                Err(refusal.error)
            }
            Err(refusal) => {
                refused.retain(|(other, _)| other != id);
                refused.push((id.clone(), refusal.reason));
                Err(refusal.error)
            }
        },
    );
    for (id, _) in still_busy {
        refused.push((id, DiskReason::Busy));
    }
    let set = |approvals: &mut Vec<Approval>, id: &str, outcome: Outcome| {
        if let Some(approval) = approvals.iter_mut().find(|a| a.id == id) {
            approval.outcome = outcome;
        }
    };
    for (id, reason) in refused {
        set(&mut approvals, &id, Outcome::Rejected(reason));
    }
    let claimed: Vec<String> = devices.iter().map(|d| d.id.clone()).collect();
    let plan = if devices.is_empty() {
        None
    } else {
        match plan(devices, config, workers) {
            Ok(plan) => {
                for id in &claimed {
                    set(&mut approvals, id, Outcome::Claimed);
                }
                Some(plan)
            }
            Err(error) => {
                eprintln!(
                    "racer: approved disks cannot hold the cache layout; using slab files: {error}"
                );
                for id in &claimed {
                    set(&mut approvals, id, Outcome::Rejected(DiskReason::TooSmall));
                }
                None
            }
        }
    };
    // Aliases share the outcome of the first approved ID for the same disk.
    for index in 0..approvals.len() {
        let Some(rdev) = approvals[index].rdev else {
            continue;
        };
        if let Some((_, first)) = first_by_rdev.iter().find(|(seen, _)| *seen == rdev)
            && *first != index
            && approvals[index].outcome == Outcome::Rejected(DiskReason::Duplicate)
        {
            approvals[index].outcome = approvals[*first].outcome;
        }
    }
    for approval in &approvals {
        match approval.outcome {
            Outcome::Claimed => {}
            Outcome::Missing => eprintln!(
                "racer: approved disk {:?} not found under {}",
                approval.id,
                root.join("disk/by-id").display()
            ),
            Outcome::Rejected(reason) => eprintln!(
                "racer: approved disk {:?} rejected: {}",
                approval.id,
                reason_name(reason)
            ),
        }
    }
    if plan.is_none() {
        eprintln!("racer: no approved disk is usable; using slab files");
    }
    Discovery { plan, approvals }
}

/// Wire name of a reason, for logs.
pub fn reason_name(reason: DiskReason) -> String {
    serde_json::to_value(reason)
        .ok()
        .and_then(|v| v.as_str().map(str::to_owned))
        .unwrap_or_default()
}

/// Sysfs directory of block devices by `MAJ:MIN`.
const SYS_DEV_BLOCK: &str = "/sys/dev/block";

/// `MAJ:MIN` for a device number.
fn dev_number(rdev: u64) -> String {
    format!("{}:{}", libc::major(rdev), libc::minor(rdev))
}

/// Trimmed, nonempty sysfs attribute text.
fn sys_text(path: &Path) -> Option<String> {
    let text = fs::read_to_string(path).ok()?;
    let text = text.trim();
    (!text.is_empty()).then(|| text.to_owned())
}

/// Interval between exclusive-open retries for busy devices.
const BUSY_POLL: Duration = Duration::from_millis(250);

trait Clock {
    fn elapsed(&self) -> Duration;
    fn sleep(&mut self, duration: Duration);
}

struct SystemClock(std::time::Instant);

impl Clock for SystemClock {
    fn elapsed(&self) -> Duration {
        self.0.elapsed()
    }

    fn sleep(&mut self, duration: Duration) {
        std::thread::sleep(duration);
    }
}

/// Only EBUSY from the exclusive open is treated as transient. A restarted pod
/// can start while its predecessor still holds the `O_EXCL` claim. Sysfs
/// holders (`check_device_state`) are stacked dm/md devices, not process
/// claims, so they remain permanent rejections.
fn is_busy(error: &io::Error) -> bool {
    error.raw_os_error() == Some(libc::EBUSY)
}

/// Opens every candidate once in order, then retries only busy candidates
/// under one shared deadline so devices wait concurrently, not sequentially.
/// Non-busy errors are skipped immediately. Returns the candidates still busy
/// at the deadline, or after the first pass when `wait` is zero, so the caller
/// records them as busy rather than missing.
fn open_with_busy_wait<C: Candidate>(
    candidates: Vec<C>,
    wait: Duration,
    clock: &mut impl Clock,
    mut open: impl FnMut(&C) -> io::Result<()>,
) -> Vec<C> {
    let mut busy = Vec::new();
    for candidate in candidates {
        match open(&candidate) {
            Ok(()) => {}
            Err(error) if is_busy(&error) => busy.push(candidate),
            Err(error) => {
                eprintln!("racer: skipping block device {:?}: {error}", candidate.id());
            }
        }
    }
    if busy.is_empty() {
        return busy;
    }
    if wait.is_zero() {
        for candidate in &busy {
            eprintln!(
                "racer: skipping block device {:?}: busy and busy wait is disabled",
                candidate.id()
            );
        }
        return busy;
    }
    eprintln!(
        "racer: waiting up to {}s for {} busy block device(s) to be released",
        wait.as_secs(),
        busy.len()
    );
    let start = clock.elapsed();
    loop {
        let waited = clock.elapsed().saturating_sub(start);
        if waited >= wait {
            break;
        }
        clock.sleep(BUSY_POLL.min(wait - waited));
        let mut still = Vec::new();
        for candidate in busy {
            match open(&candidate) {
                Ok(()) => {}
                Err(error) if is_busy(&error) => still.push(candidate),
                Err(error) => {
                    eprintln!("racer: skipping block device {:?}: {error}", candidate.id());
                }
            }
        }
        busy = still;
        if busy.is_empty() {
            eprintln!(
                "racer: busy block devices released after {}ms",
                clock.elapsed().saturating_sub(start).as_millis()
            );
            return busy;
        }
    }
    for candidate in &busy {
        eprintln!(
            "racer: skipping block device {:?}: still busy after {}s",
            candidate.id(),
            wait.as_secs()
        );
    }
    busy
}

trait Candidate {
    fn id(&self) -> &str;
}

impl Candidate for (String, std::path::PathBuf) {
    fn id(&self) -> &str {
        &self.0
    }
}

fn open_device(
    root: &Path,
    sys: &Path,
    path: &Path,
    id: &str,
    segment_bytes: u64,
) -> std::result::Result<Device, Refusal> {
    let io_error = Refusal::io(DiskReason::IoError);
    let path = fs::canonicalize(path).map_err(Refusal::io(DiskReason::IoError))?;
    let relative = path
        .strip_prefix(root)
        .map_err(|e| Refusal::new(DiskReason::IoError, e))?;
    let metadata = fs::metadata(&path).map_err(Refusal::io(DiskReason::IoError))?;
    if !metadata.file_type().is_block_device() {
        return Err(Refusal::new(DiskReason::NotWholeDisk, "not a block device"));
    }
    let dev = metadata.rdev();
    check_unused(sys, dev)?;
    let root_fd = File::open(root).map_err(Refusal::io(DiskReason::IoError))?;
    let relative = std::ffi::CString::new(relative.as_os_str().as_encoded_bytes())
        .map_err(|e| Refusal::new(DiskReason::IoError, e))?;
    // openat2 prevents a renamed parent or symlink from escaping the device root.
    #[repr(C)]
    struct OpenHow {
        flags: u64,
        mode: u64,
        resolve: u64,
    }
    let how = OpenHow {
        flags: (libc::O_RDWR | libc::O_DIRECT | libc::O_EXCL | libc::O_CLOEXEC) as u64,
        mode: 0,
        resolve: 0x08 | 0x04, // RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS
    };
    // SAFETY: valid directory fd and initialized, bounded openat2 arguments.
    let fd = unsafe {
        libc::syscall(
            libc::SYS_openat2,
            root_fd.as_raw_fd(),
            relative.as_ptr(),
            &how,
            std::mem::size_of::<OpenHow>(),
        )
    };
    if fd < 0 {
        let error = io::Error::last_os_error();
        let reason = match error.raw_os_error() {
            Some(libc::EBUSY) => DiskReason::Busy,
            Some(libc::EROFS) => DiskReason::ReadOnly,
            _ => DiskReason::IoError,
        };
        return Err(Refusal { reason, error });
    }
    // SAFETY: openat2 returned a new owned descriptor.
    let file = unsafe { <File as std::os::fd::FromRawFd>::from_raw_fd(fd as i32) };
    let opened = file.metadata().map_err(Refusal::io(DiskReason::IoError))?;
    if !opened.file_type().is_block_device() || opened.rdev() != dev {
        return Err(Refusal::new(
            DiskReason::IoError,
            "device changed while opening",
        ));
    }
    // Recheck under the exclusive claim.
    let partitions = check_unused(sys, dev)?;
    let mut bytes = 0u64;
    // SAFETY: BLKGETSIZE64 writes one u64 into valid storage.
    if unsafe { libc::ioctl(file.as_raw_fd(), 0x80081272, &mut bytes) } < 0 {
        return Err(io_error(io::Error::last_os_error()));
    }
    let alignment = device_alignment(&file).map_err(Refusal::io(DiskReason::IoError))?;
    guarded_capacity(bytes, segment_bytes, alignment)
        .map_err(|e| Refusal::new(DiskReason::TooSmall, e))?;
    prepare_guards(&BlockGuards(&file), id, bytes, alignment, partitions)?;
    Ok(Device {
        file: Arc::new(file),
        id: id.into(),
        bytes,
        alignment,
    })
}

/// Guard-region I/O on an exclusively opened disk. Tests substitute files.
trait GuardIo {
    fn read_at(&self, buffer: &mut [u8], offset: u64) -> io::Result<usize>;
    fn write_at(&self, buffer: &[u8], offset: u64) -> io::Result<usize>;
    fn sync(&self) -> io::Result<()>;
    /// Ask the kernel to reread the partition table (`BLKRRPART`).
    fn reread_partitions(&self) -> io::Result<()>;
}

struct BlockGuards<'a>(&'a File);

impl GuardIo for BlockGuards<'_> {
    fn read_at(&self, buffer: &mut [u8], offset: u64) -> io::Result<usize> {
        self.0.read_at(buffer, offset)
    }

    fn write_at(&self, buffer: &[u8], offset: u64) -> io::Result<usize> {
        self.0.write_at(buffer, offset)
    }

    fn sync(&self) -> io::Result<()> {
        self.0.sync_data()
    }

    fn reread_partitions(&self) -> io::Result<()> {
        const BLKRRPART: libc::c_ulong = 0x125f;
        // SAFETY: BLKRRPART takes no argument.
        if unsafe { libc::ioctl(self.0.as_raw_fd(), BLKRRPART as _, 0) } < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }
}

/// Ensure both guard regions are zero, wiping them when they are not.
///
/// Approval is consent to wipe. Zeroing the first and last [`GUARD_BYTES`]
/// destroys partition tables (MBR and both GPT copies) and the filesystem,
/// LVM, and RAID signatures kept there. Guards that are already zero are not
/// written, so restarts never rewrite a disk. When the kernel still lists
/// partitions, the partition table is reread so stale partitions disappear;
/// a busy reread refuses the disk.
fn prepare_guards(
    disk: &impl GuardIo,
    id: &str,
    bytes: u64,
    alignment: Alignment,
    partitions: bool,
) -> std::result::Result<(), Refusal> {
    guarded_capacity(bytes, GUARD_BYTES, alignment)
        .map_err(|e| Refusal::new(DiskReason::TooSmall, e))?;
    let mut buffer = alignment
        .allocate(GUARD_BYTES as usize, ())
        .map_err(|e| Refusal::new(DiskReason::IoError, e))?;
    let mut dirty = Vec::new();
    for offset in [0, bytes - GUARD_BYTES] {
        buffer.as_mut_slice().fill(0xff);
        let n = disk
            .read_at(buffer.as_mut_slice(), offset)
            .map_err(Refusal::io(DiskReason::IoError))?;
        if n != GUARD_BYTES as usize {
            return Err(Refusal::new(DiskReason::IoError, "short guard read"));
        }
        if buffer.as_slice().iter().any(|b| *b != 0) {
            dirty.push(offset);
        }
    }
    if !dirty.is_empty() {
        eprintln!(
            "racer: wiping approved disk {id:?}: zeroing the first and last {GUARD_BYTES} bytes (partition table and signatures)"
        );
        buffer.as_mut_slice().fill(0);
        for offset in dirty {
            let n = disk
                .write_at(buffer.as_slice(), offset)
                .map_err(Refusal::io(DiskReason::WipeFailed))?;
            if n != GUARD_BYTES as usize {
                return Err(Refusal::new(DiskReason::WipeFailed, "short guard write"));
            }
        }
        disk.sync().map_err(Refusal::io(DiskReason::WipeFailed))?;
    }
    if partitions {
        match disk.reread_partitions() {
            Ok(()) => {}
            Err(error) if error.raw_os_error() == Some(libc::EBUSY) => {
                return Err(Refusal::new(
                    DiskReason::Busy,
                    format!("partition table reread: {error}"),
                ));
            }
            Err(error) => return Err(Refusal::io(DiskReason::WipeFailed)(error)),
        }
    }
    validate_guards(bytes, alignment, |buffer, offset| {
        disk.read_at(buffer, offset)
    })
    .map_err(Refusal::io(DiskReason::WipeFailed))
}

fn guarded_capacity(bytes: u64, segment: u64, alignment: Alignment) -> Result<u64> {
    let usable = bytes
        .checked_sub(2 * GUARD_BYTES)
        .ok_or(Error::InvalidConfiguration)?;
    if segment == 0
        || usable < segment
        || bytes > i64::MAX as u64
        || alignment.memory() > GUARD_BYTES as usize
        || !GUARD_BYTES.is_multiple_of(alignment.offset())
        || !GUARD_BYTES.is_multiple_of(alignment.length() as u64)
        || !(bytes - GUARD_BYTES).is_multiple_of(alignment.offset())
        || !segment.is_multiple_of(alignment.offset())
        || !segment.is_multiple_of(alignment.length() as u64)
    {
        return Err(Error::InvalidConfiguration);
    }
    Ok(usable / segment)
}

fn validate_guards(
    bytes: u64,
    alignment: Alignment,
    mut read: impl FnMut(&mut [u8], u64) -> io::Result<usize>,
) -> io::Result<()> {
    guarded_capacity(bytes, GUARD_BYTES, alignment).map_err(io::Error::other)?;
    let mut buffer = alignment
        .allocate(GUARD_BYTES as usize, ())
        .map_err(io::Error::other)?;
    for offset in [0, bytes - GUARD_BYTES] {
        buffer.as_mut_slice().fill(0xff);
        let n = read(buffer.as_mut_slice(), offset)?;
        if n != GUARD_BYTES as usize {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "short guard read",
            ));
        }
        if buffer.as_slice().iter().any(|b| *b != 0) {
            return Err(io::Error::other("nonzero device guard"));
        }
    }
    Ok(())
}

/// Refuse a disk in use; returns whether the kernel lists partitions on it.
fn check_unused(sys: &Path, dev: u64) -> std::result::Result<bool, Refusal> {
    let mounts =
        fs::read_to_string("/proc/self/mountinfo").map_err(Refusal::io(DiskReason::IoError))?;
    check_device_state(sys, dev, &mounts)
}

/// Whether `MAJ:MIN` is the source of any mount in `mountinfo` text.
pub(crate) fn is_mounted(mounts: &str, number: &str) -> bool {
    mounts
        .lines()
        .any(|line| line.split_whitespace().nth(2) == Some(number))
}

/// Refuse a read-only, held, or mounted disk, or one with a held or mounted
/// partition, and refuse partitions themselves. Idle partitions are allowed,
/// because approval is consent to wipe them. Returns whether the disk has
/// partitions.
fn check_device_state(sys: &Path, dev: u64, mounts: &str) -> std::result::Result<bool, Refusal> {
    let number = dev_number(dev);
    let path = sys.join(&number);
    let io = Refusal::io(DiskReason::IoError);
    if path.join("partition").try_exists().map_err(io)? {
        return Err(Refusal::new(
            DiskReason::NotWholeDisk,
            "device is a partition",
        ));
    }
    if fs::read_to_string(path.join("ro"))
        .map_err(Refusal::io(DiskReason::IoError))?
        .trim()
        != "0"
    {
        return Err(Refusal::new(DiskReason::ReadOnly, "read-only device"));
    }
    let held = |dir: &Path| -> std::result::Result<bool, Refusal> {
        match fs::read_dir(dir.join("holders")) {
            Ok(mut entries) => Ok(entries.next().is_some()),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(false),
            Err(error) => Err(Refusal::io(DiskReason::IoError)(error)),
        }
    };
    if held(&path)? {
        return Err(Refusal::new(DiskReason::Held, "device has holders"));
    }
    if is_mounted(mounts, &number) {
        return Err(Refusal::new(DiskReason::Mounted, "device is mounted"));
    }
    let mut partitions = false;
    for child in fs::read_dir(&path).map_err(Refusal::io(DiskReason::IoError))? {
        let child = child.map_err(Refusal::io(DiskReason::IoError))?;
        let child = child.path();
        if !child.is_dir() || !child.join("partition").try_exists().unwrap_or(false) {
            continue;
        }
        partitions = true;
        if held(&child)? {
            return Err(Refusal::new(DiskReason::Held, "a partition has holders"));
        }
        if let Some(number) = sys_text(&child.join("dev"))
            && is_mounted(mounts, &number)
        {
            return Err(Refusal::new(DiskReason::Mounted, "a partition is mounted"));
        }
    }
    Ok(partitions)
}

fn device_alignment(file: &File) -> io::Result<Alignment> {
    // SAFETY: statx writes only to the initialized output; empty path addresses fd.
    let mut stat: libc::statx = unsafe { std::mem::zeroed() };
    let result = unsafe {
        libc::statx(
            file.as_raw_fd(),
            c"".as_ptr(),
            libc::AT_EMPTY_PATH,
            libc::STATX_DIOALIGN,
            &mut stat,
        )
    };
    let (memory, offset) = if result == 0
        && stat.stx_mask & libc::STATX_DIOALIGN != 0
        && stat.stx_dio_mem_align != 0
        && stat.stx_dio_offset_align != 0
    {
        (
            stat.stx_dio_mem_align as usize,
            stat.stx_dio_offset_align as u64,
        )
    } else {
        let mut sector = 0u32;
        // SAFETY: BLKSSZGET writes one integer into valid storage.
        if unsafe { libc::ioctl(file.as_raw_fd(), 0x1268, &mut sector) } < 0 {
            return Err(io::Error::last_os_error());
        }
        (sector as usize, sector as u64)
    };
    Alignment::new(memory, offset, offset as usize).map_err(io::Error::other)
}

fn plan(mut devices: Vec<Device>, config: &Config, workers: usize) -> Result<Plan> {
    if workers == 0 || devices.is_empty() || config.segment_bytes == 0 {
        return Err(Error::InvalidConfiguration);
    }
    devices.sort_by(|a, b| a.id.cmp(&b.id));
    let total = devices.iter().try_fold(0u64, |n, d| {
        n.checked_add(guarded_capacity(
            d.bytes,
            config.segment_bytes,
            d.alignment,
        )?)
        .ok_or(Error::InvalidConfiguration)
    })?;
    let worker_limit = MAX_SEGMENTS.min(i64::MAX as u64 / config.segment_bytes);
    let usable = total.min(
        worker_limit
            .checked_mul(workers as u64)
            .ok_or(Error::InvalidConfiguration)?,
    );
    if usable / workers as u64 <= config.free_segment_reserve as u64 {
        return Err(Error::InvalidConfiguration);
    }
    if usable != total {
        eprintln!(
            "racer: segment limit discards {} whole device segments",
            total - usable
        );
    }
    let mut alignment = Alignment::new(1, 1, 1)?;
    for device in &devices {
        alignment = Alignment::new(
            alignment.memory().max(device.alignment.memory()),
            lcm(alignment.offset(), device.alignment.offset())?,
            usize::try_from(lcm(
                alignment.length() as u64,
                device.alignment.length() as u64,
            )?)
            .map_err(|_| Error::InvalidConfiguration)?,
        )?;
    }
    for device in &devices {
        guarded_capacity(device.bytes, config.segment_bytes, alignment)?;
    }
    let record = alignment
        .extent(
            0,
            crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
        )?
        .length() as u64;
    if record > config.segment_bytes {
        return Err(Error::InvalidConfiguration);
    }
    let mut device_index = 0;
    let mut offset = GUARD_BYTES;
    let mut plans = Vec::new();
    let mut max_snapshot = 0usize;
    for worker in 0..workers {
        let count = usable / workers as u64 + u64::from((worker as u64) < usable % workers as u64);
        let capacity = (count - config.free_segment_reserve as u64)
            .saturating_mul(config.segment_bytes / record);
        let requested = capacity.max((config.disk_page_entries.get() / workers).max(1) as u64);
        let pages = requested.min(MAX_PAGES) as usize;
        if requested > MAX_PAGES {
            eprintln!(
                "racer: worker={worker} disk page index capped at {MAX_PAGES}; requested={requested}"
            );
        }
        let mut hash = Sha256::new();
        hash.update(b"racer-raw-layout-v1\0");
        hash.update(GUARD_BYTES.to_le_bytes());
        hash.update(GUARD_BYTES.to_le_bytes());
        let mut placements = Vec::with_capacity(count as usize);
        let mut disk_groups: Vec<(Arc<str>, usize)> = Vec::new();
        let mut last_device = None;
        for _ in 0..count {
            while offset
                .checked_add(config.segment_bytes)
                .ok_or(Error::InvalidConfiguration)?
                > devices[device_index].bytes - GUARD_BYTES
            {
                device_index += 1;
                offset = GUARD_BYTES;
            }
            let device = &devices[device_index];
            if last_device == Some(device_index) {
                disk_groups.last_mut().unwrap().1 += 1;
            } else {
                disk_groups.push((format!("device:{}", device.id).into(), 1));
                last_device = Some(device_index);
            }
            hash.update((device.id.len() as u64).to_le_bytes());
            hash.update(device.id.as_bytes());
            hash.update(device.bytes.to_le_bytes());
            hash.update(offset.to_le_bytes());
            placements.push(Placement {
                file: device.file.clone(),
                offset,
            });
            offset = offset
                .checked_add(config.segment_bytes)
                .ok_or(Error::InvalidConfiguration)?;
        }
        // Snapshot charges include each descriptor's largest accepted strings.
        let entries = pages.saturating_add((config.limits.metadata_entries.get() / workers).max(1));
        let snapshot = entries
            .saturating_mul(2048 + 3 * 8192 * 4)
            .saturating_add(count as usize * 1024)
            .saturating_add(4096);
        max_snapshot = max_snapshot.max(snapshot);
        plans.push(WorkerDevices {
            placements,
            disk_groups,
            alignment,
            digest: hash.finalize().into(),
            page_entries: pages,
        });
    }
    let requested = max_snapshot
        .saturating_mul(workers)
        .saturating_mul(4)
        .max(config.checkpoint_bytes.get());
    if requested > MAX_CHECKPOINT {
        eprintln!(
            "racer: checkpoint working-set capped at {MAX_CHECKPOINT}; worst-case demand={requested}; oversized cuts will be skipped"
        );
    }
    eprintln!(
        "racer: raw-device storage devices={} workers={workers} segments={usable} segment_bytes={}",
        devices.len(),
        config.segment_bytes
    );
    Ok(Plan {
        workers: plans,
        checkpoint_bytes: requested.min(MAX_CHECKPOINT),
    })
}

fn lcm(a: u64, b: u64) -> Result<u64> {
    let (mut x, mut y) = (a, b);
    while y != 0 {
        (x, y) = (y, x % y);
    }
    (a / x).checked_mul(b).ok_or(Error::InvalidConfiguration)
}

#[cfg(test)]
pub(super) fn recovery_layout_fixture() -> Vec<crate::store::checkpoint::CheckpointGeometry> {
    let c = crate::test_support::cluster::config(false);
    let bytes = 6 * c.segment_bytes + 2 * GUARD_BYTES;
    let devices: Vec<_> = ["cache-a", "cache-b"]
        .into_iter()
        .map(|id| Device {
            file: Arc::new(File::open("/dev/null").unwrap()),
            id: id.into(),
            bytes,
            alignment: Alignment::new(4096, 4096, 4096).unwrap(),
        })
        .collect();
    let p = plan(devices, &c, 2).unwrap();
    p.workers
        .into_iter()
        .map(|worker| {
            assert_eq!(worker.placements.len(), 6);
            let mut geometry = crate::store::checkpoint::CheckpointGeometry::new(
                6 * c.segment_bytes,
                c.segment_bytes,
                6,
                worker.alignment,
            )
            .unwrap();
            geometry.layout_digest = worker.digest;
            geometry
        })
        .collect()
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;
    use std::collections::HashSet;

    struct FakeClock(Duration);

    impl Clock for FakeClock {
        fn elapsed(&self) -> Duration {
            self.0
        }

        fn sleep(&mut self, duration: Duration) {
            self.0 += duration;
        }
    }

    fn candidates(ids: &[&str]) -> Vec<(String, std::path::PathBuf)> {
        ids.iter().map(|id| ((*id).into(), id.into())).collect()
    }

    fn busy() -> io::Error {
        io::Error::from_raw_os_error(libc::EBUSY)
    }

    #[test]
    fn busy_device_released_within_deadline_is_used() {
        let mut clock = FakeClock(Duration::from_secs(5));
        let mut opened = Vec::new();
        let mut attempts = 0;
        let left = open_with_busy_wait(
            candidates(&["a", "b"]),
            Duration::from_secs(120),
            &mut clock,
            |(id, _)| {
                if id == "a" {
                    attempts += 1;
                    if attempts <= 3 {
                        return Err(busy());
                    }
                }
                opened.push(id.clone());
                Ok(())
            },
        );
        assert!(left.is_empty());
        assert_eq!(opened, ["b", "a"]);
        assert_eq!(attempts, 4);
        assert_eq!(clock.0, Duration::from_secs(5) + 3 * BUSY_POLL);
    }

    #[test]
    fn busy_devices_share_one_deadline_then_fall_back() {
        let wait = Duration::from_millis(1_100);
        let mut clock = FakeClock(Duration::ZERO);
        let mut calls = 0;
        let left = open_with_busy_wait(candidates(&["a", "b", "c"]), wait, &mut clock, |_| {
            calls += 1;
            Err(busy())
        });
        assert_eq!(left, candidates(&["a", "b", "c"]));
        // One shared deadline: total wait does not scale with device count.
        assert_eq!(clock.0, wait);
        // Initial pass plus five polls (250 ms x4, then the 100 ms remainder).
        assert_eq!(calls, 3 * 6);
        // With no device opened, planning rejects the empty set, so discovery
        // falls back to slab files.
        assert!(plan(Vec::new(), &config(), 1).is_err());
    }

    #[test]
    fn non_busy_errors_are_not_retried() {
        let mut clock = FakeClock(Duration::ZERO);
        let mut calls = 0;
        let left = open_with_busy_wait(
            candidates(&["a", "b"]),
            Duration::from_secs(120),
            &mut clock,
            |(id, _)| {
                calls += 1;
                if id == "a" {
                    Err(io::Error::from_raw_os_error(libc::EIO))
                } else {
                    Err(io::Error::other("nonzero device guard"))
                }
            },
        );
        assert!(left.is_empty());
        assert_eq!(calls, 2);
        assert_eq!(clock.0, Duration::ZERO);
    }

    #[test]
    fn busy_then_permanent_error_stops_retrying() {
        let mut clock = FakeClock(Duration::ZERO);
        let mut calls = 0;
        let left = open_with_busy_wait(
            candidates(&["a"]),
            Duration::from_secs(120),
            &mut clock,
            |_| {
                calls += 1;
                if calls == 1 {
                    Err(busy())
                } else {
                    Err(io::Error::other("device is mounted"))
                }
            },
        );
        assert!(left.is_empty());
        assert_eq!(calls, 2);
        assert_eq!(clock.0, BUSY_POLL);
    }

    #[test]
    fn zero_wait_disables_busy_retry() {
        let mut clock = FakeClock(Duration::ZERO);
        let mut calls = 0;
        let left = open_with_busy_wait(
            candidates(&["a", "b", "c"]),
            Duration::ZERO,
            &mut clock,
            |(id, _)| {
                calls += 1;
                match id.as_str() {
                    "a" => Err(busy()),
                    "b" => Err(io::Error::from_raw_os_error(libc::EIO)),
                    _ => Ok(()),
                }
            },
        );
        // Busy devices are returned so discovery reports them as busy, not
        // missing; other failures and successes are not.
        assert_eq!(left, candidates(&["a"]));
        assert_eq!(calls, 3);
        assert_eq!(clock.0, Duration::ZERO);
    }

    #[test]
    fn only_ebusy_is_transient() {
        assert!(is_busy(&busy()));
        assert!(!is_busy(&io::Error::from_raw_os_error(libc::EIO)));
        assert!(!is_busy(&io::Error::other(
            "read-only device or active holders"
        )));
    }

    fn config() -> Config {
        crate::test_support::cluster::config(false)
    }

    fn device(id: &str, count: u64, segment: u64) -> Device {
        // Planner fixtures never open or write a block device.
        Device {
            file: Arc::new(File::open("/dev/null").unwrap()),
            id: id.into(),
            bytes: count * segment + 2 * GUARD_BYTES + 4096,
            alignment: Alignment::new(4096, 4096, 4096).unwrap(),
        }
    }

    #[test]
    fn split_sorted_devices_preserves_remainders_and_shared_claims() {
        let c = config();
        let s = c.segment_bytes;
        let p = plan(vec![device("z", 6, s), device("a", 5, s)], &c, 3).unwrap();
        assert_eq!(
            p.workers
                .iter()
                .map(|w| w.placements.len())
                .collect::<Vec<_>>(),
            [4, 4, 3]
        );
        assert_eq!(p.workers[1].placements[0].offset, GUARD_BYTES + 4 * s);
        assert_eq!(p.workers[1].placements[1].offset, GUARD_BYTES);
        assert_eq!(p.workers[0].disk_groups, vec![(Arc::from("device:a"), 4)]);
        assert_eq!(
            p.workers[1].disk_groups,
            vec![(Arc::from("device:a"), 1), (Arc::from("device:z"), 3)]
        );
        assert_eq!(p.workers[2].disk_groups, vec![(Arc::from("device:z"), 3)]);
        assert!(Arc::ptr_eq(
            &p.workers[0].placements[0].file,
            &p.workers[1].placements[0].file
        ));
        assert_ne!(p.workers[0].digest, p.workers[1].digest);
        let q = plan(vec![device("a", 5, s), device("z", 6, s)], &c, 3).unwrap();
        assert_eq!(p.workers[0].digest, q.workers[0].digest);
    }

    #[test]
    fn exact_helper_counts_preserve_twenty_worker_device_ownership() {
        let make = |helpers| {
            let mut c = Config::from_lookup(|name| {
                Ok(match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                    "RACER_ENABLE_RDMA" => Some("false".into()),
                    _ => None,
                })
            })
            .unwrap();
            c.io_workers = Some(20);
            c.crypto_helpers = Some(helpers);
            c.max_threads = 20 + helpers;
            c.validate().unwrap();
            plan(
                (0..5)
                    .map(|i| device(&format!("disk-{i}"), 100, c.segment_bytes))
                    .collect(),
                &c,
                c.io_workers.unwrap(),
            )
            .unwrap()
        };
        let a = make(10);
        let b = make(20);
        assert_eq!(a.workers.len(), 20);
        assert_eq!(b.workers.len(), 20);
        assert_eq!(a.checkpoint_bytes, b.checkpoint_bytes);
        for (a, b) in a.workers.iter().zip(&b.workers) {
            assert_eq!(a.digest, b.digest);
            assert_eq!(a.disk_groups, b.disk_groups);
            assert_eq!(a.page_entries, b.page_entries);
            assert_eq!(
                a.placements.iter().map(|p| p.offset).collect::<Vec<_>>(),
                b.placements.iter().map(|p| p.offset).collect::<Vec<_>>()
            );
        }
    }

    #[test]
    fn disk_usage_shared_device_plan_exports_each_assigned_segment_once() {
        let c = config();
        let s = c.segment_bytes;
        let p = plan(vec![device("z", 6, s), device("a", 5, s)], &c, 3).unwrap();
        let metrics = crate::telemetry::Metrics::for_workers(3).unwrap();
        for (worker, metric) in p.workers.iter().zip(&metrics) {
            let segments = page_alloc::Segments::new(s);
            segments
                .configure(
                    worker.placements.len() as u64 * s,
                    worker.placements.len(),
                    worker.alignment,
                )
                .unwrap();
            segments
                .configure_usage_groups(
                    &worker
                        .disk_groups
                        .iter()
                        .map(|(_, count)| *count)
                        .collect::<Vec<_>>(),
                )
                .unwrap();
            drop(segments.append(4096).unwrap());
            metric.observe_disks(
                worker
                    .disk_groups
                    .iter()
                    .zip(segments.usage())
                    .map(|((disk, _), (size, used))| crate::telemetry::DiskUsage {
                        disk: disk.clone(),
                        size,
                        used,
                    })
                    .collect(),
            );
        }
        let mut output = String::new();
        metrics[0].write_prometheus(&mut output).unwrap();
        for (disk, size, used) in [("a", 5 * s, 8192), ("z", 6 * s, 4096)] {
            assert!(output.contains(&format!(
                "racer_disk_size_bytes{{disk=\"device:{disk}\"}} {size}\n"
            )));
            assert!(output.contains(&format!(
                "racer_disk_used_bytes{{disk=\"device:{disk}\"}} {used}\n"
            )));
        }
    }

    #[test]
    fn layout_binds_stable_id_size_and_offset_not_fd() {
        let c = config();
        let s = c.segment_bytes;
        let digest = |id, count| plan(vec![device(id, count, s)], &c, 1).unwrap().workers[0].digest;
        assert_eq!(digest("a", 8), digest("a", 8));
        assert_ne!(digest("a", 8), digest("b", 8));
        assert_ne!(digest("a", 8), digest("a", 9));
        assert_ne!(digest("a", 8), crate::store::checkpoint::FILE_LAYOUT_DIGEST);
        let original = digest("a", 8);
        let mut resized = device("a", 8, s);
        resized.bytes += 4096;
        assert_ne!(
            original,
            plan(vec![resized], &c, 1).unwrap().workers[0].digest
        );
        let mut changed = config();
        changed.segment_bytes /= 2;
        assert_ne!(
            original,
            plan(vec![device("a", 8, s)], &changed, 1).unwrap().workers[0].digest
        );
    }

    #[test]
    fn guarded_geometry_boundaries_and_overflow() {
        let a = Alignment::new(4096, 4096, 4096).unwrap();
        let s = 64 * 1024 * 1024;
        assert_eq!(guarded_capacity(2 * GUARD_BYTES + s, s, a).unwrap(), 1);
        assert_eq!(
            guarded_capacity(2 * GUARD_BYTES + 2 * s - 4096, s, a).unwrap(),
            1
        );
        for bytes in [
            0,
            GUARD_BYTES,
            2 * GUARD_BYTES,
            2 * GUARD_BYTES + s - 4096,
            u64::MAX,
        ] {
            assert!(guarded_capacity(bytes, s, a).is_err());
        }
        for segment in [0, 1, s + 1, u64::MAX] {
            assert!(guarded_capacity(2 * GUARD_BYTES + 2 * s, segment, a).is_err());
        }
        for a in [
            Alignment::new(4096, 3, 4096).unwrap(),
            Alignment::new(4096, 4096, 3).unwrap(),
            Alignment::new(2 * GUARD_BYTES as usize, 4096, 4096).unwrap(),
        ] {
            assert!(guarded_capacity(2 * GUARD_BYTES + s, s, a).is_err());
        }
        assert!(guarded_capacity(2 * GUARD_BYTES + s + 1, s, a).is_err());
        let c = config();
        assert!(plan(vec![device("a", 8, c.segment_bytes)], &c, usize::MAX).is_err());
        assert!(plan(vec![device("a", 8, c.segment_bytes)], &c, 0).is_err());
        assert_eq!(lcm(6, 8).unwrap(), 24);
        assert!(lcm(u64::MAX, 2).is_err());
    }

    #[test]
    fn every_worker_stays_inside_each_devices_guards_and_digest() {
        let c = config();
        let s = c.segment_bytes;
        let devices = vec![device("a", 5, s), device("b", 6, s)];
        let files: Vec<_> = devices
            .iter()
            .map(|d| (d.file.clone(), d.bytes, d.id.clone()))
            .collect();
        let p = plan(devices, &c, 3).unwrap();
        let mut used = HashSet::new();
        for worker in &p.workers {
            let mut digest = Sha256::new();
            digest.update(b"racer-raw-layout-v1\0");
            digest.update(GUARD_BYTES.to_le_bytes());
            digest.update(GUARD_BYTES.to_le_bytes());
            for placement in &worker.placements {
                let (file, bytes, id) = files
                    .iter()
                    .find(|(f, _, _)| Arc::ptr_eq(f, &placement.file))
                    .unwrap();
                assert!(placement.offset >= GUARD_BYTES);
                assert!(placement.offset + s <= bytes - GUARD_BYTES);
                assert_eq!((placement.offset - GUARD_BYTES) % s, 0);
                assert!(used.insert((file.as_raw_fd(), placement.offset)));
                digest.update((id.len() as u64).to_le_bytes());
                digest.update(id.as_bytes());
                digest.update(bytes.to_le_bytes());
                digest.update(placement.offset.to_le_bytes());
            }
            assert_eq!(worker.digest, <[u8; 32]>::from(digest.finalize()));
        }
        assert_eq!(used.len(), 11);
    }

    #[test]
    fn combined_alignment_applies_to_all_workers_and_devices() {
        let c = config();
        let s = c.segment_bytes;
        let a = device("a", 5, s);
        let mut b = device("b", 6, s);
        b.alignment = Alignment::new(8192, 8192, 8192).unwrap();
        b.bytes += 4096;
        assert!(plan(vec![a, b], &c, 3).is_err());
        let mut a = device("a", 5, s);
        a.bytes += 4096;
        let mut b = device("b", 6, s);
        b.bytes += 4096;
        b.alignment = Alignment::new(8192, 8192, 8192).unwrap();
        let p = plan(vec![a, b], &c, 3).unwrap();
        for worker in &p.workers {
            assert_eq!(worker.alignment.memory(), 8192);
            assert_eq!(worker.alignment.offset(), 8192);
            assert_eq!(worker.alignment.length(), 8192);
            assert!(worker.placements.iter().all(|p| p.offset % 8192 == 0));
        }
    }

    #[test]
    fn guard_reader_requires_complete_zero_aligned_reads_at_both_ends() {
        let a = Alignment::new(4096, 4096, 4096).unwrap();
        let bytes = 8 * GUARD_BYTES;
        let mut offsets = Vec::new();
        validate_guards(bytes, a, |buffer, offset| {
            assert_eq!(buffer.len(), GUARD_BYTES as usize);
            assert_eq!(buffer.as_ptr() as usize % a.memory(), 0);
            offsets.push(offset);
            buffer.fill(0);
            Ok(buffer.len())
        })
        .unwrap();
        assert_eq!(offsets, [0, bytes - GUARD_BYTES]);
        for bad_end in [0, bytes - GUARD_BYTES] {
            for failure in 0..6 {
                assert!(
                    validate_guards(bytes, a, |buffer, offset| {
                        buffer.fill(0);
                        if offset == bad_end {
                            match failure {
                                0 => buffer[0] = 1,
                                1 => buffer[buffer.len() / 2] = 1,
                                2 => buffer[buffer.len() - 1] = 1,
                                3 => return Ok(buffer.len() - 1),
                                4 => return Ok(0),
                                _ => return Err(io::Error::from_raw_os_error(libc::EIO)),
                            }
                        }
                        Ok(buffer.len())
                    })
                    .is_err()
                );
            }
        }
        assert!(validate_guards(1, a, |_, _| panic!("invalid geometry must not read")).is_err());
    }

    #[test]
    fn guard_validation_uses_read_only_file_without_changes() {
        let fixture = Fixture::new();
        let path = fixture.0.join("guards");
        let mut data = vec![0; 4 * GUARD_BYTES as usize];
        data[GUARD_BYTES as usize..3 * GUARD_BYTES as usize].fill(42);
        fs::write(&path, &data).unwrap();
        let file = File::open(&path).unwrap();
        validate_guards(
            data.len() as u64,
            Alignment::new(4096, 4096, 4096).unwrap(),
            |buffer, offset| file.read_at(buffer, offset),
        )
        .unwrap();
        assert_eq!(fs::read(&path).unwrap(), data);
        for start in [0, 3 * GUARD_BYTES as usize] {
            for position in [0, GUARD_BYTES as usize / 2, GUARD_BYTES as usize - 1] {
                data[start + position] = 1;
                fs::write(&path, &data).unwrap();
                assert!(
                    validate_guards(
                        data.len() as u64,
                        Alignment::new(4096, 4096, 4096).unwrap(),
                        |buffer, offset| file.read_at(buffer, offset)
                    )
                    .is_err()
                );
                assert_eq!(fs::read(&path).unwrap(), data);
                data[start + position] = 0;
            }
        }
    }

    #[test]
    fn empty_too_small_invalid_and_missing_selectors_fall_back() {
        let mut c = config();
        let s = c.segment_bytes;
        assert!(plan(Vec::new(), &c, 1).is_err());
        assert!(plan(vec![device("a", 2, s)], &c, 1).is_err());
        assert!(plan(vec![device("a", 5, s)], &c, 2).is_err());
        assert!(discover(&c, &[], 1).plan.is_none());
        assert!(discover(&c, &[], 1).approvals.is_empty());
        c.device_directory = Path::new(env!("CARGO_MANIFEST_DIR")).join("nonexistent-device-root");
        let d = discover(&c, &["nvme-a".into()], 1);
        assert!(d.plan.is_none());
        assert_eq!(d.approvals[0].outcome, Outcome::Missing);
    }

    #[test]
    fn sizing_uses_capacity_or_configured_share_and_caps_checkpoint_memory() {
        let mut c = config();
        c.disk_page_entries = std::num::NonZeroUsize::new(1).unwrap();
        let s = c.segment_bytes;
        let p = plan(vec![device("a", 9, s)], &c, 2).unwrap();
        let record = p.workers[0]
            .alignment
            .extent(
                0,
                crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
            )
            .unwrap()
            .length() as u64;
        assert_eq!(
            p.workers[0].page_entries as u64,
            (5 - c.free_segment_reserve as u64) * (s / record)
        );
        c.disk_page_entries = std::num::NonZeroUsize::new(100_000).unwrap();
        let p = plan(vec![device("a", 9, s)], &c, 2).unwrap();
        assert_eq!(p.workers[0].page_entries, 50_000);
        assert_eq!(p.checkpoint_bytes, MAX_CHECKPOINT);
        c.slab_bytes = 1;
        assert!(plan(vec![device("a", 9, s)], &c, 2).is_ok());
    }

    #[test]
    fn non_block_device_and_escaped_path_are_rejected_before_open() {
        let root = Path::new(env!("CARGO_MANIFEST_DIR"));
        let sys = Path::new(SYS_DEV_BLOCK);
        let Err(refusal) = open_device(root, sys, &root.join("Cargo.toml"), "file", 4096) else {
            panic!("a regular file must be refused");
        };
        assert_eq!(refusal.reason, DiskReason::NotWholeDisk);
        assert!(open_device(root, sys, Path::new("/dev/null"), "escape", 4096).is_err());
    }

    pub(crate) struct Fixture(pub(crate) std::path::PathBuf);
    impl Fixture {
        pub(crate) fn new() -> Self {
            use std::os::unix::fs::DirBuilderExt;
            static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
            // The repo tmp/ directory is gitignored, so it is absent on a fresh
            // checkout (for example in CI). Create it before the per-test leaf.
            let parent = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../tmp");
            fs::create_dir_all(&parent).unwrap();
            let path = parent.join(format!(
                "device-test-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
            ));
            fs::DirBuilder::new().mode(0o700).create(&path).unwrap();
            Self(fs::canonicalize(path).unwrap())
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn reason(result: std::result::Result<bool, Refusal>) -> Option<DiskReason> {
        result.err().map(|refusal| refusal.reason)
    }

    #[test]
    fn refuses_in_use_disks_and_partitions_but_allows_idle_partitions() {
        let f = Fixture::new();
        let dev = libc::makedev(7, 0);
        let path = f.0.join("7:0");
        fs::create_dir_all(path.join("holders")).unwrap();
        fs::write(path.join("ro"), "0\n").unwrap();
        assert!(!check_device_state(&f.0, dev, "").unwrap());
        // Mounted, read-only, and held disks are refused even when approved.
        assert_eq!(
            reason(check_device_state(
                &f.0,
                dev,
                "1 0 7:0 / /data rw - ext4 /dev/loop0 rw"
            )),
            Some(DiskReason::Mounted)
        );
        fs::write(path.join("ro"), "1\n").unwrap();
        assert_eq!(
            reason(check_device_state(&f.0, dev, "")),
            Some(DiskReason::ReadOnly)
        );
        fs::write(path.join("ro"), "0\n").unwrap();
        fs::write(path.join("holders/dm-0"), "").unwrap();
        assert_eq!(
            reason(check_device_state(&f.0, dev, "")),
            Some(DiskReason::Held)
        );
        fs::remove_file(path.join("holders/dm-0")).unwrap();
        // An idle partition is consented-to wipe.
        let part = path.join("loop0p1");
        fs::create_dir_all(part.join("holders")).unwrap();
        fs::write(part.join("partition"), "1\n").unwrap();
        fs::write(part.join("dev"), "7:1\n").unwrap();
        assert!(check_device_state(&f.0, dev, "").unwrap());
        // A mounted or held partition is not.
        assert_eq!(
            reason(check_device_state(
                &f.0,
                dev,
                "1 0 7:1 / /data rw - ext4 /dev/loop0p1 rw"
            )),
            Some(DiskReason::Mounted)
        );
        fs::write(part.join("holders/md0"), "").unwrap();
        assert_eq!(
            reason(check_device_state(&f.0, dev, "")),
            Some(DiskReason::Held)
        );
        // A partition itself is never a whole disk.
        let partition = f.0.join("7:1");
        fs::create_dir_all(partition.join("holders")).unwrap();
        fs::write(partition.join("ro"), "0\n").unwrap();
        fs::write(partition.join("partition"), "1\n").unwrap();
        assert_eq!(
            reason(check_device_state(&f.0, libc::makedev(7, 1), "")),
            Some(DiskReason::NotWholeDisk)
        );
    }

    /// File-backed guard I/O that records writes and fakes `BLKRRPART`.
    struct FileGuards {
        file: File,
        writes: std::cell::Cell<usize>,
        rereads: std::cell::Cell<usize>,
        reread: fn() -> io::Result<()>,
    }

    impl FileGuards {
        fn new(path: &Path, reread: fn() -> io::Result<()>) -> Self {
            Self {
                file: fs::OpenOptions::new()
                    .read(true)
                    .write(true)
                    .open(path)
                    .unwrap(),
                writes: Default::default(),
                rereads: Default::default(),
                reread,
            }
        }
    }

    impl GuardIo for FileGuards {
        fn read_at(&self, buffer: &mut [u8], offset: u64) -> io::Result<usize> {
            self.file.read_at(buffer, offset)
        }

        fn write_at(&self, buffer: &[u8], offset: u64) -> io::Result<usize> {
            self.writes.set(self.writes.get() + 1);
            self.file.write_at(buffer, offset)
        }

        fn sync(&self) -> io::Result<()> {
            self.file.sync_data()
        }

        fn reread_partitions(&self) -> io::Result<()> {
            self.rereads.set(self.rereads.get() + 1);
            (self.reread)()
        }
    }

    fn guarded_image(f: &Fixture, dirty: bool) -> (std::path::PathBuf, Vec<u8>) {
        let path = f.0.join("disk");
        let mut data = vec![0; 4 * GUARD_BYTES as usize];
        data[GUARD_BYTES as usize..3 * GUARD_BYTES as usize].fill(42);
        if dirty {
            data[0..512].fill(0x55);
            let end = data.len();
            data[end - 1] = 1;
        }
        fs::write(&path, &data).unwrap();
        (path, data)
    }

    #[test]
    fn nonzero_guards_are_wiped_then_accepted_without_touching_the_payload() {
        let f = Fixture::new();
        let (path, data) = guarded_image(&f, true);
        let disk = FileGuards::new(&path, || Ok(()));
        let a = Alignment::new(4096, 4096, 4096).unwrap();
        prepare_guards(&disk, "wwn-1", data.len() as u64, a, false).unwrap();
        assert_eq!(disk.writes.get(), 2);
        assert_eq!(disk.rereads.get(), 0);
        let after = fs::read(&path).unwrap();
        assert!(after[..GUARD_BYTES as usize].iter().all(|b| *b == 0));
        assert!(after[3 * GUARD_BYTES as usize..].iter().all(|b| *b == 0));
        assert_eq!(
            after[GUARD_BYTES as usize..3 * GUARD_BYTES as usize],
            data[GUARD_BYTES as usize..3 * GUARD_BYTES as usize]
        );
    }

    #[test]
    fn zero_guards_are_never_rewritten() {
        let f = Fixture::new();
        let (path, data) = guarded_image(&f, false);
        let disk = FileGuards::new(&path, || Ok(()));
        let a = Alignment::new(4096, 4096, 4096).unwrap();
        prepare_guards(&disk, "wwn-1", data.len() as u64, a, false).unwrap();
        prepare_guards(&disk, "wwn-1", data.len() as u64, a, false).unwrap();
        assert_eq!(disk.writes.get(), 0);
        assert_eq!(fs::read(&path).unwrap(), data);
    }

    #[test]
    fn partitioned_disks_reread_the_table_and_busy_rereads_refuse() {
        let f = Fixture::new();
        let a = Alignment::new(4096, 4096, 4096).unwrap();
        let (path, data) = guarded_image(&f, true);
        let disk = FileGuards::new(&path, || Ok(()));
        prepare_guards(&disk, "wwn-1", data.len() as u64, a, true).unwrap();
        assert_eq!(disk.rereads.get(), 1);
        let (path, data) = guarded_image(&f, true);
        let busy = FileGuards::new(&path, || Err(io::Error::from_raw_os_error(libc::EBUSY)));
        let refusal = prepare_guards(&busy, "wwn-1", data.len() as u64, a, true).unwrap_err();
        assert_eq!(refusal.reason, DiskReason::Busy);
        assert!(!is_busy(&refusal.error));
        let (path, data) = guarded_image(&f, false);
        let failed = FileGuards::new(&path, || Err(io::Error::from_raw_os_error(libc::ENOTTY)));
        let refusal = prepare_guards(&failed, "wwn-1", data.len() as u64, a, true).unwrap_err();
        assert_eq!(refusal.reason, DiskReason::WipeFailed);
    }

    #[test]
    fn wipe_and_size_failures_have_reasons() {
        let f = Fixture::new();
        let a = Alignment::new(4096, 4096, 4096).unwrap();
        let (path, data) = guarded_image(&f, true);
        // A read-only descriptor cannot wipe.
        let disk = FileGuards {
            file: File::open(&path).unwrap(),
            writes: Default::default(),
            rereads: Default::default(),
            reread: || Ok(()),
        };
        let refusal = prepare_guards(&disk, "wwn-1", data.len() as u64, a, false).unwrap_err();
        assert_eq!(refusal.reason, DiskReason::WipeFailed);
        assert_eq!(fs::read(&path).unwrap(), data);
        let refusal = prepare_guards(&disk, "wwn-1", GUARD_BYTES, a, false).unwrap_err();
        assert_eq!(refusal.reason, DiskReason::TooSmall);
    }

    #[test]
    fn no_matching_or_usable_devices_falls_back_without_opening_payloads() {
        let f = Fixture::new();
        fs::create_dir_all(f.0.join("disk/by-id")).unwrap();
        fs::write(f.0.join("payload"), "do not overwrite").unwrap();
        std::os::unix::fs::symlink("../../payload", f.0.join("disk/by-id/cache")).unwrap();
        std::os::unix::fs::symlink("../../payload", f.0.join("disk/by-id/cache-alias")).unwrap();
        let mut c = config();
        c.device_directory = f.0.clone();
        let d = discover(&c, &["missing".into()], 1);
        assert!(d.plan.is_none());
        assert_eq!(d.approvals[0].outcome, Outcome::Missing);
        // Exact names only: a prefix of an entry does not match it.
        let d = discover(&c, &["cach".into()], 1);
        assert_eq!(d.approvals[0].outcome, Outcome::Missing);
        let d = discover(
            &c,
            &[
                "cache".into(),
                "cache".into(),
                "cache-alias".into(),
                "../payload".into(),
            ],
            1,
        );
        assert!(d.plan.is_none());
        assert_eq!(
            d.approvals
                .iter()
                .map(|a| (a.id.as_str(), a.outcome))
                .collect::<Vec<_>>(),
            [
                ("cache", Outcome::Rejected(DiskReason::NotWholeDisk)),
                ("cache-alias", Outcome::Rejected(DiskReason::NotWholeDisk)),
                ("../payload", Outcome::Rejected(DiskReason::NotWholeDisk)),
            ]
        );
        assert_eq!(fs::read(f.0.join("payload")).unwrap(), b"do not overwrite");
    }

    #[test]
    fn claimed_disks_dedupe_ids_across_workers_and_report_rdev() {
        let file = Arc::new(File::open("/dev/null").unwrap());
        let worker = |groups: &[(&str, usize)]| WorkerDevices {
            placements: (0..groups.iter().map(|g| g.1).sum::<usize>())
                .map(|offset| Placement {
                    file: file.clone(),
                    offset: offset as u64,
                })
                .collect(),
            disk_groups: groups.iter().map(|(id, n)| (Arc::from(*id), *n)).collect(),
            alignment: Alignment::new(1, 1, 1).unwrap(),
            digest: [0; 32],
            page_entries: 0,
        };
        let plan = Plan {
            workers: vec![
                worker(&[("device:b", 2), ("device:a", 1)]),
                worker(&[("device:a", 3)]),
            ],
            checkpoint_bytes: 0,
        };
        let claimed = plan.claimed_disks();
        // /dev/null is character device 1:3 on Linux.
        assert_eq!(
            claimed,
            vec![
                crate::hardware::ClaimedDisk {
                    id: "a".into(),
                    dev: Some("1:3".into())
                },
                crate::hardware::ClaimedDisk {
                    id: "b".into(),
                    dev: Some("1:3".into())
                },
            ]
        );
        assert!(
            Plan {
                workers: vec![],
                checkpoint_bytes: 0
            }
            .claimed_disks()
            .is_empty()
        );
    }
}
