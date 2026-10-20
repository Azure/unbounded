//! Backing devices and the production-like slot plan.
//!
//! Mirrors `plan()` in `cmd/racer-dataplane/src/app/devices.rs`: every device
//! keeps a 1 MiB guard at its start and end, the usable whole slots of all
//! devices are concatenated in order and split contiguously across workers,
//! one shared `Arc<File>` is used per device, and each slot becomes one
//! `DevicePlacement`. Differences: devices keep command line order (production
//! sorts them by device id), and `--slots-per-worker` keeps only the first
//! slots of each worker's slice.

use std::fs::{File, OpenOptions};
use std::os::fd::AsRawFd;
use std::os::unix::fs::{FileTypeExt, MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::sync::Arc;

use page_alloc::volume::{MAX_SLOTS, MIN_SLOTS, SLOT_BYTES, UNIT_BYTES};
use page_alloc::{Alignment, DevicePlacement};

use crate::stats::DeviceStat;

/// Guard bytes at each device start and end, as in production.
pub const GUARD_BYTES: u64 = 1024 * 1024;

/// One opened backing device or file.
pub struct Device {
    pub path: PathBuf,
    pub file: Arc<File>,
    pub bytes: u64,
    pub alignment: Alignment,
    pub stat: DeviceStat,
}

/// One worker's share of the plan.
#[derive(Clone)]
pub struct WorkerPlan {
    pub placements: Vec<DevicePlacement>,
    pub alignment: Alignment,
    /// Device path and slot count per contiguous run, for display.
    pub runs: Vec<(String, u64)>,
}

type Result<T> = std::result::Result<T, String>;

/// Open a block device like production: read/write, direct, exclusive.
pub fn open_device(path: &Path) -> Result<Device> {
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .custom_flags(libc::O_DIRECT | libc::O_EXCL | libc::O_CLOEXEC)
        .open(path)
        .map_err(|e| format!("open {}: {e}", path.display()))?;
    let metadata = file
        .metadata()
        .map_err(|e| format!("stat {}: {e}", path.display()))?;
    if !metadata.file_type().is_block_device() {
        return Err(format!("{} is not a block device", path.display()));
    }
    let mut bytes = 0u64;
    // SAFETY: BLKGETSIZE64 writes one u64 into valid storage.
    if unsafe { libc::ioctl(file.as_raw_fd(), 0x8008_1272, &mut bytes) } < 0 {
        return Err(format!(
            "BLKGETSIZE64 {}: {}",
            path.display(),
            std::io::Error::last_os_error()
        ));
    }
    let alignment = alignment(&file)?;
    Ok(Device {
        path: path.to_owned(),
        stat: DeviceStat::for_dev(name(path), metadata.rdev()),
        file: Arc::new(file),
        bytes,
        alignment,
    })
}

/// Create (or reuse) a preallocated direct file of `bytes`.
pub fn open_file(path: &Path, bytes: u64) -> Result<Device> {
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .custom_flags(libc::O_DIRECT | libc::O_CLOEXEC)
        .open(path)
        .map_err(|e| format!("open {}: {e}", path.display()))?;
    file.set_len(bytes)
        .map_err(|e| format!("truncate {}: {e}", path.display()))?;
    // Allocate blocks up front so writes do not pay for extent allocation.
    // SAFETY: valid descriptor and range.
    let result = unsafe { libc::fallocate(file.as_raw_fd(), 0, 0, bytes as libc::off_t) };
    if result != 0 {
        eprintln!(
            "warning: fallocate {}: {}",
            path.display(),
            std::io::Error::last_os_error()
        );
    }
    let metadata = file
        .metadata()
        .map_err(|e| format!("stat {}: {e}", path.display()))?;
    let alignment = alignment(&file).unwrap_or_else(|_| Alignment::new(4096, 4096, 4096).unwrap());
    Ok(Device {
        path: path.to_owned(),
        stat: DeviceStat::for_dev(name(path), metadata.dev()),
        file: Arc::new(file),
        bytes,
        alignment,
    })
}

fn name(path: &Path) -> String {
    path.file_name().map_or_else(
        || path.display().to_string(),
        |n| n.to_string_lossy().into(),
    )
}

/// Direct I/O alignment from `statx`, else the logical block size.
fn alignment(file: &File) -> Result<Alignment> {
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
            u64::from(stat.stx_dio_offset_align),
        )
    } else {
        let mut sector = 0u32;
        // SAFETY: BLKSSZGET writes one integer into valid storage.
        if unsafe { libc::ioctl(file.as_raw_fd(), 0x1268, &mut sector) } < 0 {
            return Err(format!("BLKSSZGET: {}", std::io::Error::last_os_error()));
        }
        (sector as usize, u64::from(sector))
    };
    Alignment::new(memory, offset, offset as usize).map_err(|e| format!("alignment: {e:?}"))
}

fn gcd(a: u64, b: u64) -> u64 {
    if b == 0 { a } else { gcd(b, a % b) }
}

fn lcm(a: u64, b: u64) -> u64 {
    a / gcd(a, b) * b
}

/// Whole slots of a device after guards.
fn usable_slots(device: &Device) -> u64 {
    device.bytes.saturating_sub(2 * GUARD_BYTES) / SLOT_BYTES
}

/// Split the devices' slots across `workers`.
pub fn plan(devices: &[Device], workers: usize, cap: Option<u64>) -> Result<Vec<WorkerPlan>> {
    if workers == 0 || devices.is_empty() {
        return Err("need at least one worker and one device".into());
    }
    let mut memory = 1usize;
    let mut offset = 1u64;
    let mut length = 1u64;
    for device in devices {
        memory = memory.max(device.alignment.memory());
        offset = lcm(offset, device.alignment.offset());
        length = lcm(length, device.alignment.length() as u64);
    }
    if memory > UNIT_BYTES
        || !(UNIT_BYTES as u64).is_multiple_of(offset)
        || !(UNIT_BYTES as u64).is_multiple_of(length)
    {
        return Err(format!(
            "alignment memory={memory} offset={offset} length={length} does not divide 4096"
        ));
    }
    let alignment =
        Alignment::new(memory, offset, length as usize).map_err(|e| format!("alignment: {e:?}"))?;
    let total: u64 = devices.iter().map(usable_slots).sum();
    let usable = total.min(MAX_SLOTS * workers as u64);
    if usable / (workers as u64) < MIN_SLOTS {
        return Err(format!(
            "{usable} slots of {SLOT_BYTES} bytes are too few for {workers} workers (need {MIN_SLOTS} each)"
        ));
    }
    let mut device_index = 0;
    let mut position = GUARD_BYTES;
    let mut plans = Vec::with_capacity(workers);
    for worker in 0..workers {
        let count = usable / workers as u64 + u64::from((worker as u64) < usable % workers as u64);
        let keep = cap.map_or(count, |cap| cap.min(count));
        let mut placements = Vec::with_capacity(keep as usize);
        let mut runs: Vec<(String, u64)> = Vec::new();
        for slot in 0..count {
            while position + SLOT_BYTES > devices[device_index].bytes - GUARD_BYTES {
                device_index += 1;
                position = GUARD_BYTES;
            }
            let device = &devices[device_index];
            if slot < keep {
                let label = device.path.display().to_string();
                match runs.last_mut() {
                    Some((last, n)) if *last == label => *n += 1,
                    _ => runs.push((label, 1)),
                }
                placements.push(DevicePlacement {
                    file: device.file.clone(),
                    offset: position,
                });
            }
            position += SLOT_BYTES;
        }
        if (placements.len() as u64) < MIN_SLOTS {
            return Err(format!("worker {worker} has fewer than {MIN_SLOTS} slots"));
        }
        plans.push(WorkerPlan {
            placements,
            alignment,
            runs,
        });
    }
    Ok(plans)
}
