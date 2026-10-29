//! Startup-only raw-device discovery. Only reserved guards are read; nothing is written.

use crate::config::Config;
use crate::error::{Error, Result};
use page_alloc::Alignment;
use page_alloc::volume::{MIN_SLOTS, SLOT_BYTES, UNIT_BYTES};
use regex::Regex;
use sha2::{Digest, Sha256};
use std::collections::HashSet;
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
const GUARD_BYTES: u64 = 1024 * 1024;

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

pub fn discover(config: &Config, selector: Option<&str>, workers: usize) -> Option<Plan> {
    let selector = selector.filter(|s| !s.is_empty())?;
    let result = (|| {
        if selector.len() > 1024 {
            return Err(io::Error::other("selector exceeds 1024 bytes"));
        }
        let regex = Regex::new(selector).map_err(io::Error::other)?;
        let root = fs::canonicalize(&config.device_directory)?;
        let mut paths = fs::read_dir(root.join("disk/by-id"))?.collect::<io::Result<Vec<_>>>()?;
        paths.sort_by_key(|entry| entry.file_name());
        let candidates = paths
            .into_iter()
            .filter_map(|entry| {
                let id = entry.file_name().to_str()?.to_owned();
                regex.is_match(&id).then(|| (id, entry.path()))
            })
            .collect();
        let mut seen = HashSet::new();
        let mut devices = Vec::new();
        let mut failure = None;
        open_with_busy_wait(
            candidates,
            config.device_busy_wait,
            &mut SystemClock(std::time::Instant::now()),
            |(id, path): &(String, std::path::PathBuf)| {
                // Avoid reopening an alias while the first exclusive claim is held.
                if fs::metadata(path).is_ok_and(|m| seen.contains(&m.rdev())) {
                    return Ok(());
                }
                let device = open_device(&root, path, id, SLOT_BYTES)?;
                match device.file.metadata() {
                    Ok(metadata) => {
                        if seen.insert(metadata.rdev()) {
                            devices.push(device);
                        }
                    }
                    Err(error) => {
                        failure.get_or_insert(error);
                    }
                }
                Ok(())
            },
        );
        if let Some(error) = failure {
            return Err(error);
        }
        plan(devices, config, workers).map_err(io::Error::other)
    })();
    match result {
        Ok(plan) => Some(plan),
        Err(error) => {
            eprintln!("racer: block device discovery failed; using slab files: {error}");
            None
        }
    }
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
/// Non-busy errors, and busy errors when `wait` is zero, are skipped
/// immediately. Returns the candidates still busy at the deadline.
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
            Err(error) if is_busy(&error) && !wait.is_zero() => busy.push(candidate),
            Err(error) => {
                eprintln!("racer: skipping block device {:?}: {error}", candidate.id());
            }
        }
    }
    if busy.is_empty() {
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

fn open_device(root: &Path, path: &Path, id: &str, segment_bytes: u64) -> io::Result<Device> {
    let path = fs::canonicalize(path)?;
    let relative = path.strip_prefix(root).map_err(io::Error::other)?;
    let metadata = fs::metadata(&path)?;
    if !metadata.file_type().is_block_device() {
        return Err(io::Error::other("not a block device"));
    }
    let dev = metadata.rdev();
    check_unused(Path::new("/sys/dev/block"), dev)?;
    let root_fd = File::open(root)?;
    let relative = std::ffi::CString::new(relative.as_os_str().as_encoded_bytes())?;
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
        return Err(io::Error::last_os_error());
    }
    // SAFETY: openat2 returned a new owned descriptor.
    let file = unsafe { <File as std::os::fd::FromRawFd>::from_raw_fd(fd as i32) };
    let opened = file.metadata()?;
    if !opened.file_type().is_block_device() || opened.rdev() != dev {
        return Err(io::Error::other("device changed while opening"));
    }
    check_unused(Path::new("/sys/dev/block"), dev)?;
    let mut bytes = 0u64;
    // SAFETY: BLKGETSIZE64 writes one u64 into valid storage.
    if unsafe { libc::ioctl(file.as_raw_fd(), 0x80081272, &mut bytes) } < 0 {
        return Err(io::Error::last_os_error());
    }
    let alignment = device_alignment(&file)?;
    guarded_capacity(bytes, segment_bytes, alignment).map_err(io::Error::other)?;
    validate_guards(bytes, alignment, |buffer, offset| {
        file.read_at(buffer, offset)
    })?;
    Ok(Device {
        file: Arc::new(file),
        id: id.into(),
        bytes,
        alignment,
    })
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

fn check_unused(sys: &Path, dev: u64) -> io::Result<()> {
    check_device_state(sys, dev, &fs::read_to_string("/proc/self/mountinfo")?)
}

fn check_device_state(sys: &Path, dev: u64, mounts: &str) -> io::Result<()> {
    let path = sys.join(format!("{}:{}", libc::major(dev), libc::minor(dev)));
    if fs::read_to_string(path.join("ro"))?.trim() != "0"
        || fs::read_dir(path.join("holders"))?.next().is_some()
    {
        return Err(io::Error::other("read-only device or active holders"));
    }
    for child in fs::read_dir(&path)? {
        let child = child?;
        if child.file_type()?.is_dir() && child.path().join("partition").try_exists()? {
            return Err(io::Error::other("device has child partitions"));
        }
    }
    let number = format!("{}:{}", libc::major(dev), libc::minor(dev));
    if mounts
        .lines()
        .any(|line| line.split_whitespace().nth(2) == Some(number.as_str()))
    {
        return Err(io::Error::other("device is mounted"));
    }
    Ok(())
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
    if workers == 0 || devices.is_empty() {
        return Err(Error::InvalidConfiguration);
    }
    devices.sort_by(|a, b| a.id.cmp(&b.id));
    let total = devices.iter().try_fold(0u64, |n, d| {
        n.checked_add(guarded_capacity(d.bytes, SLOT_BYTES, d.alignment)?)
            .ok_or(Error::InvalidConfiguration)
    })?;
    let worker_limit = MAX_SEGMENTS.min(i64::MAX as u64 / SLOT_BYTES);
    let usable = total.min(
        worker_limit
            .checked_mul(workers as u64)
            .ok_or(Error::InvalidConfiguration)?,
    );
    if usable / (workers as u64) < MIN_SLOTS {
        return Err(Error::InvalidConfiguration);
    }
    if usable != total {
        eprintln!(
            "racer: slot limit discards {} whole device slots",
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
    // The extent volume transfers whole 4096 byte units.
    if alignment.memory() > UNIT_BYTES
        || !(UNIT_BYTES as u64).is_multiple_of(alignment.offset())
        || !UNIT_BYTES.is_multiple_of(alignment.length())
    {
        return Err(Error::InvalidConfiguration);
    }
    for device in &devices {
        guarded_capacity(device.bytes, SLOT_BYTES, alignment)?;
    }
    let record = alignment
        .extent(0, crate::store::MAX_RECORD_BYTES)?
        .length() as u64;
    if record > SLOT_BYTES {
        return Err(Error::InvalidConfiguration);
    }
    let mut device_index = 0;
    let mut offset = GUARD_BYTES;
    let mut plans = Vec::new();
    let mut max_snapshot = 0usize;
    for worker in 0..workers {
        let count = usable / workers as u64 + u64::from((worker as u64) < usable % workers as u64);
        // The meta slot holds no pages.
        let capacity = (count - 1).saturating_mul(SLOT_BYTES / record);
        let requested = capacity.max((config.disk_page_entries.get() / workers).max(1) as u64);
        let pages = requested.min(MAX_PAGES) as usize;
        if requested > MAX_PAGES {
            eprintln!(
                "racer: worker={worker} disk page index capped at {MAX_PAGES}; requested={requested}"
            );
        }
        let mut hash = Sha256::new();
        hash.update(b"racer-raw-layout-v2\0");
        hash.update(GUARD_BYTES.to_le_bytes());
        hash.update(GUARD_BYTES.to_le_bytes());
        let mut placements = Vec::with_capacity(count as usize);
        let mut disk_groups: Vec<(Arc<str>, usize)> = Vec::new();
        let mut last_device = None;
        for _ in 0..count {
            while offset
                .checked_add(SLOT_BYTES)
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
                .checked_add(SLOT_BYTES)
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
        "racer: raw-device storage devices={} workers={workers} slots={usable} slot_bytes={SLOT_BYTES}",
        devices.len()
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
pub(super) fn recovery_layout_fixture() -> Vec<crate::store::checkpoint::CheckpointLayout> {
    let c = crate::test_support::cluster::config(false);
    let bytes = 6 * SLOT_BYTES + 2 * GUARD_BYTES;
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
            crate::store::checkpoint::CheckpointLayout {
                layout_digest: worker.digest,
            }
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

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
        let left = open_with_busy_wait(candidates(&["a"]), Duration::ZERO, &mut clock, |_| {
            calls += 1;
            Err(busy())
        });
        assert!(left.is_empty());
        assert_eq!(calls, 1);
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
        let s = SLOT_BYTES;
        let p = plan(vec![device("z", 9, s), device("a", 7, s)], &c, 3).unwrap();
        assert_eq!(
            p.workers
                .iter()
                .map(|w| w.placements.len())
                .collect::<Vec<_>>(),
            [6, 5, 5]
        );
        assert_eq!(p.workers[1].placements[0].offset, GUARD_BYTES + 6 * s);
        assert_eq!(p.workers[1].placements[1].offset, GUARD_BYTES);
        assert_eq!(p.workers[0].disk_groups, vec![(Arc::from("device:a"), 6)]);
        assert_eq!(
            p.workers[1].disk_groups,
            vec![(Arc::from("device:a"), 1), (Arc::from("device:z"), 4)]
        );
        assert_eq!(p.workers[2].disk_groups, vec![(Arc::from("device:z"), 5)]);
        assert!(Arc::ptr_eq(
            &p.workers[0].placements[0].file,
            &p.workers[1].placements[0].file
        ));
        assert_ne!(p.workers[0].digest, p.workers[1].digest);
        let q = plan(vec![device("a", 7, s), device("z", 9, s)], &c, 3).unwrap();
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
                    .map(|i| device(&format!("disk-{i}"), 100, SLOT_BYTES))
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
    fn disk_usage_shared_device_plan_exports_each_assigned_slot_once() {
        let c = config();
        let s = SLOT_BYTES;
        let p = plan(vec![device("z", 9, s), device("a", 7, s)], &c, 3).unwrap();
        let metrics = crate::telemetry::Metrics::for_workers(3).unwrap();
        // Worker 1 spans both devices: one slot of `a` (the meta slot) and four of `z`.
        let worker = &p.workers[1];
        let index = crate::store::catalog::Index::new(
            crate::model::WorkerId(1),
            16,
            crate::test_support::availability(),
        );
        let disk = crate::store::test_disk::TestDisk::mount(&index, worker.placements.len() as u64);
        let counts: Vec<usize> = worker.disk_groups.iter().map(|(_, count)| *count).collect();
        let usage = index.disk_usage(&counts);
        let meta =
            index.volume().unwrap().usage().node_units * page_alloc::volume::UNIT_BYTES as u64;
        assert!(meta > 0);
        drop(disk);
        metrics[1].observe_disks(
            worker
                .disk_groups
                .iter()
                .zip(usage)
                .map(|((disk, _), (size, used))| crate::telemetry::DiskUsage {
                    disk: disk.clone(),
                    size,
                    used,
                })
                .collect(),
        );
        let mut output = String::new();
        metrics[1].write_prometheus(&mut output).unwrap();
        for (disk, size) in [("a", s), ("z", 4 * s)] {
            assert!(output.contains(&format!(
                "racer_disk_size_bytes{{disk=\"device:{disk}\"}} {size}\n"
            )));
        }
        // Only the meta slot's node units are in use after format, and they
        // all belong to `a`.
        assert!(output.contains(&format!(
            "racer_disk_used_bytes{{disk=\"device:a\"}} {meta}\n"
        )));
        assert!(output.contains("racer_disk_used_bytes{disk=\"device:z\"} 0\n"));
    }

    #[test]
    fn layout_binds_stable_id_size_and_offset_not_fd() {
        let c = config();
        let s = SLOT_BYTES;
        let digest = |id, count| plan(vec![device(id, count, s)], &c, 1).unwrap().workers[0].digest;
        assert_eq!(digest("a", 8), digest("a", 8));
        assert_ne!(digest("a", 8), digest("b", 8));
        assert_ne!(digest("a", 8), digest("a", 9));
        assert_ne!(digest("a", 8), crate::store::checkpoint::FILE_LAYOUT_DIGEST);
        // The extent volume layout (v2) never matches a v1 segment layout of
        // the same devices, so old checkpoints are rejected.
        let tagged = |tag: &[u8]| {
            let worker = plan(vec![device("a", 8, s)], &c, 1)
                .unwrap()
                .workers
                .remove(0);
            let device = device("a", 8, s);
            let mut hash = Sha256::new();
            hash.update(tag);
            hash.update(GUARD_BYTES.to_le_bytes());
            hash.update(GUARD_BYTES.to_le_bytes());
            for placement in &worker.placements {
                hash.update((device.id.len() as u64).to_le_bytes());
                hash.update(device.id.as_bytes());
                hash.update(device.bytes.to_le_bytes());
                hash.update(placement.offset.to_le_bytes());
            }
            <[u8; 32]>::from(hash.finalize())
        };
        assert_eq!(tagged(b"racer-raw-layout-v2\0"), digest("a", 8));
        assert_ne!(tagged(b"racer-raw-layout-v1\0"), digest("a", 8));
        let original = digest("a", 8);
        let mut resized = device("a", 8, s);
        resized.bytes += 4096;
        assert_ne!(
            original,
            plan(vec![resized], &c, 1).unwrap().workers[0].digest
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
        assert!(plan(vec![device("a", 8, SLOT_BYTES)], &c, usize::MAX).is_err());
        assert!(plan(vec![device("a", 8, SLOT_BYTES)], &c, 0).is_err());
        assert_eq!(lcm(6, 8).unwrap(), 24);
        assert!(lcm(u64::MAX, 2).is_err());
    }

    #[test]
    fn every_worker_stays_inside_each_devices_guards_and_digest() {
        let c = config();
        let s = SLOT_BYTES;
        let devices = vec![device("a", 7, s), device("b", 8, s)];
        let files: Vec<_> = devices
            .iter()
            .map(|d| (d.file.clone(), d.bytes, d.id.clone()))
            .collect();
        let p = plan(devices, &c, 3).unwrap();
        let mut used = HashSet::new();
        for worker in &p.workers {
            let mut digest = Sha256::new();
            digest.update(b"racer-raw-layout-v2\0");
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
        assert_eq!(used.len(), 15);
    }

    #[test]
    fn combined_alignment_applies_to_all_workers_and_devices() {
        let c = config();
        let s = SLOT_BYTES;
        // The volume transfers 4096 byte units, so coarser alignment is rejected.
        let mut a = device("a", 7, s);
        a.bytes += 4096;
        let mut b = device("b", 8, s);
        b.bytes += 4096;
        b.alignment = Alignment::new(8192, 8192, 8192).unwrap();
        assert!(plan(vec![a, b], &c, 3).is_err());
        let a = device("a", 7, s);
        let mut b = device("b", 8, s);
        b.alignment = Alignment::new(512, 512, 512).unwrap();
        let p = plan(vec![a, b], &c, 3).unwrap();
        for worker in &p.workers {
            assert_eq!(worker.alignment.memory(), 4096);
            assert_eq!(worker.alignment.offset(), 4096);
            assert_eq!(worker.alignment.length(), 4096);
            assert!(worker.placements.iter().all(|p| p.offset % 4096 == 0));
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
        let s = SLOT_BYTES;
        assert!(plan(Vec::new(), &c, 1).is_err());
        assert!(plan(vec![device("a", 2, s)], &c, 1).is_err());
        assert!(plan(vec![device("a", 5, s)], &c, 2).is_err());
        assert!(discover(&c, None, 1).is_none());
        assert!(discover(&c, Some(""), 1).is_none());
        assert!(discover(&c, Some("["), 1).is_none());
        c.device_directory = Path::new(env!("CARGO_MANIFEST_DIR")).join("nonexistent-device-root");
        assert!(discover(&c, Some("nvme"), 1).is_none());
    }

    #[test]
    fn regex_matches_unanchored_basename_and_allows_partitions() {
        let r = Regex::new("nvme.*cache").unwrap();
        assert!(r.is_match("prefix-nvme-cache-part1"));
        assert!(!r.is_match("ata-system"));
        let r = Regex::new("^nvme-cache$").unwrap();
        assert!(!r.is_match("nvme-cache-part1"));
    }

    #[test]
    fn sizing_uses_capacity_or_configured_share_and_caps_checkpoint_memory() {
        let mut c = config();
        c.disk_page_entries = std::num::NonZeroUsize::new(1).unwrap();
        let s = SLOT_BYTES;
        let p = plan(vec![device("a", 9, s)], &c, 2).unwrap();
        let record = p.workers[0]
            .alignment
            .extent(0, crate::store::MAX_RECORD_BYTES)
            .unwrap()
            .length() as u64;
        assert_eq!(p.workers[0].page_entries as u64, (5 - 1) * (s / record));
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
        assert!(open_device(root, &root.join("Cargo.toml"), "file", 4096).is_err());
        assert!(open_device(root, Path::new("/dev/null"), "escape", 4096).is_err());
    }

    pub(super) struct Fixture(pub(super) std::path::PathBuf);
    impl Fixture {
        pub(super) fn new() -> Self {
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

    #[test]
    fn rejects_children_holders_read_only_and_mounts_but_allows_partition_itself() {
        let f = Fixture::new();
        let dev = libc::makedev(7, 1);
        let path = f.0.join("7:1");
        fs::create_dir_all(path.join("holders")).unwrap();
        fs::write(path.join("ro"), "0\n").unwrap();
        fs::write(path.join("partition"), "1\n").unwrap();
        assert!(check_device_state(&f.0, dev, "").is_ok());
        assert!(check_device_state(&f.0, dev, "1 0 7:1 / /data rw - ext4 /dev/loop1 rw").is_err());
        fs::write(path.join("ro"), "1\n").unwrap();
        assert!(check_device_state(&f.0, dev, "").is_err());
        fs::write(path.join("ro"), "0\n").unwrap();
        fs::write(path.join("holders/dm-0"), "").unwrap();
        assert!(check_device_state(&f.0, dev, "").is_err());
        fs::remove_file(path.join("holders/dm-0")).unwrap();
        fs::create_dir(path.join("child")).unwrap();
        fs::write(path.join("child/partition"), "1\n").unwrap();
        assert!(check_device_state(&f.0, dev, "").is_err());
    }

    #[test]
    fn no_matching_or_usable_devices_falls_back_without_opening_payloads() {
        let f = Fixture::new();
        fs::create_dir_all(f.0.join("disk/by-id")).unwrap();
        fs::write(f.0.join("payload"), "do not overwrite").unwrap();
        std::os::unix::fs::symlink("../../payload", f.0.join("disk/by-id/cache")).unwrap();
        let mut c = config();
        c.device_directory = f.0.clone();
        assert!(discover(&c, Some("missing"), 1).is_none());
        assert!(discover(&c, Some("cache"), 1).is_none());
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
