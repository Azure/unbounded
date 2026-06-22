// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Linux io_uring disk-open path plus file provisioning helpers.
//!
//! [`UringDevice::open`] is the production entry point for bringing a
//! disk online on a pinned storage core: it builds the [`StorageRing`]
//! the core drives, opens and sizes the backing file (regular file or
//! raw block device via `BLKGETSIZE64`), registers the file descriptor
//! to obtain a `Fixed` index, and binds the geometry into a
//! `Send + Sync` [`CoreLocalDevice`] that resolves the ring from the
//! thread-local registry at call time. The ring, device, and owned file
//! are returned together in an [`OpenDisk`] so the caller controls
//! installation into the registry and teardown order.
//!
//! [`provision_file`] and the private capacity helpers round out the
//! file disk lifecycle: create-and-size a backing file on startup and
//! derive its page capacity.

use std::ffi::CString;
use std::fs::File;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, RawFd};
use std::path::Path;

use super::CoreLocalDevice;
use crate::ring::{StorageRing, StorageRingConfig};
use crate::storage::types::Error;

/// Opens a disk for a pinned storage core.
///
/// This path produces a [`StorageRing`] the storage core owns
/// separately (so it can install the ring into the thread-local
/// registry and drive it alongside the engine) plus a `Send + Sync`
/// [`CoreLocalDevice`] that resolves that ring at call time. The whole
/// block-device lifecycle - io_uring setup flags, `O_DIRECT`, the
/// `BLKGETSIZE64` capacity probe, and file registration - lives here
/// rather than in the supervisor.
pub struct UringDevice;

impl UringDevice {
    /// Open `path` for a storage core: build the ring per `ring_cfg`,
    /// open the file (`O_DIRECT` when `o_direct` is set), size it (via
    /// `metadata().len()`, falling back to `BLKGETSIZE64` for raw block
    /// devices), register its fd to get a `Fixed` index, and bind the
    /// geometry into a [`CoreLocalDevice`].
    ///
    /// `o_direct` is decoupled from the ring's `IOPOLL` flag so callers
    /// can choose them independently (a tmpfile benchmark may want
    /// neither, production NVMe wants both). `IOPOLL` still requires
    /// `O_DIRECT`, so production callers pass `o_direct = ring_cfg.iopoll`.
    ///
    /// The syscall order is exactly: ring construction, file open,
    /// capacity probe, file registration. The returned [`OpenDisk`]
    /// hands the ring back un-wrapped; the caller is responsible for
    /// installing it into the thread-local registry
    /// ([`set_current_storage_ring`](crate::ring::set_current_storage_ring))
    /// on the storage-core thread, and for keeping [`OpenDisk::file`]
    /// alive for as long as the ring's registered file table addresses
    /// it.
    pub fn open(
        path: &Path,
        ring_cfg: StorageRingConfig,
        o_direct: bool,
        page_size: usize,
    ) -> Result<OpenDisk, OpenError> {
        let ring = StorageRing::new(ring_cfg).map_err(OpenError::Ring)?;
        let file = open_file(path, o_direct).map_err(OpenError::OpenFile)?;
        let capacity_pages = file_capacity_pages(&file, page_size).map_err(OpenError::Capacity)?;
        let file_index = ring
            .register_file(file.as_raw_fd())
            .map_err(OpenError::RegisterFile)?;
        let device =
            CoreLocalDevice::new(file_index, page_size, capacity_pages, ring_cfg.queue_depth);
        Ok(OpenDisk { device, ring, file })
    }
}

/// Product of [`UringDevice::open`]: the engine-facing device, the ring
/// it resolves I/O through, and the owned backing file.
pub struct OpenDisk {
    /// `Send + Sync` device the engine is built on. Resolves the ring
    /// from the thread-local registry at call time.
    pub device: CoreLocalDevice,
    /// Ring the storage core installs into the registry and drives via
    /// [`StorageRing::progress`].
    pub ring: StorageRing,
    /// Owned fd backing the registered `Fixed` file. The kernel holds
    /// its own reference once the fd is registered, but the caller keeps
    /// this for the storage core's lifetime so the fd is not closed out
    /// from under the ring.
    pub file: File,
}

/// Failure from [`UringDevice::open`], tagged by the phase that failed
/// so the supervisor surfaces the same diagnostic it did when this
/// logic was inline on the storage-core thread.
#[derive(Debug)]
pub enum OpenError {
    Ring(Error),
    OpenFile(Error),
    Capacity(Error),
    RegisterFile(Error),
}

impl std::fmt::Display for OpenError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            OpenError::Ring(e) => write!(f, "storage ring: {e}"),
            OpenError::OpenFile(e) => write!(f, "open disk: {e}"),
            OpenError::Capacity(e) => write!(f, "disk capacity: {e}"),
            OpenError::RegisterFile(e) => write!(f, "register file: {e}"),
        }
    }
}

fn open_file(path: &Path, o_direct: bool) -> Result<File, Error> {
    let cpath =
        CString::new(path.as_os_str().as_encoded_bytes()).map_err(|_| Error::Io(libc::EINVAL))?;
    let mut flags = libc::O_RDWR | libc::O_CLOEXEC;
    if o_direct {
        flags |= libc::O_DIRECT;
    }
    // SAFETY: cpath is null-terminated and outlives the call.
    let fd = unsafe { libc::open(cpath.as_ptr(), flags) };
    if fd < 0 {
        return Err(Error::Io(
            io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO),
        ));
    }
    // SAFETY: fd is freshly opened by us and not aliased elsewhere.
    Ok(unsafe { File::from_raw_fd(fd as RawFd) })
}

/// Create `path` if absent and size it to exactly `size_bytes`.
///
/// `ftruncate` sets the file length (growing or shrinking as needed);
/// then `fallocate` (mode 0) backs the range with real blocks so a
/// later write cannot hit ENOSPC mid-run. On filesystems that do not
/// support fallocate (for example tmpfs) the allocation step is
/// skipped and the file is left sparse at the requested length.
///
/// Intended for file disks; production block devices never go through here.
pub fn provision_file(path: &Path, size_bytes: u64) -> Result<(), Error> {
    let cpath =
        CString::new(path.as_os_str().as_encoded_bytes()).map_err(|_| Error::Io(libc::EINVAL))?;
    // SAFETY: cpath is null-terminated and outlives the call. The mode
    // argument is consumed because O_CREAT is set.
    let fd = unsafe {
        libc::open(
            cpath.as_ptr(),
            libc::O_RDWR | libc::O_CREAT | libc::O_CLOEXEC,
            0o644 as libc::c_uint,
        )
    };
    if fd < 0 {
        return Err(Error::Io(
            io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO),
        ));
    }
    // Wrap the fd immediately so it is closed on every return path.
    // SAFETY: fd is freshly opened by us and not aliased elsewhere.
    let file = unsafe { File::from_raw_fd(fd as RawFd) };

    // SAFETY: file.as_raw_fd() is a valid, owned descriptor.
    let rc = unsafe { libc::ftruncate(file.as_raw_fd(), size_bytes as libc::off_t) };
    if rc != 0 {
        return Err(Error::Io(
            io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO),
        ));
    }

    // SAFETY: same owned descriptor; mode 0 is a plain allocation.
    let rc = unsafe { libc::fallocate(file.as_raw_fd(), 0, 0, size_bytes as libc::off_t) };
    if rc != 0 {
        let err = io::Error::last_os_error()
            .raw_os_error()
            .unwrap_or(libc::EIO);
        // tmpfs and some other backends do not implement fallocate;
        // the file keeps its truncated length and stays sparse.
        if err == libc::EOPNOTSUPP || err == libc::ENOTSUP || err == libc::ENOSYS {
            return Ok(());
        }
        return Err(Error::Io(err));
    }
    Ok(())
}

fn file_capacity_pages(file: &File, page_size: usize) -> Result<u64, Error> {
    let meta = file.metadata().map_err(io_err_to_storage)?;
    let mut len = meta.len();
    // For raw block devices `metadata().len()` is 0 on Linux; fall
    // back to the BLKGETSIZE64 ioctl.
    if len == 0 {
        len = blkgetsize64(file.as_raw_fd())?;
    }
    Ok(len / page_size as u64)
}

fn blkgetsize64(fd: RawFd) -> Result<u64, Error> {
    // _IOR('o', 0x40 + 18, size_of::<u64>()) - we hardcode the value
    // rather than computing it from the `nix` crate to avoid pulling
    // in another dependency for one constant.
    const BLKGETSIZE64: libc::c_ulong = 0x80081272;
    let mut out: u64 = 0;
    // SAFETY: out is a writable u64 of the correct size.
    let rc = unsafe { libc::ioctl(fd, BLKGETSIZE64, &mut out as *mut u64) };
    if rc != 0 {
        return Err(Error::Io(
            io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO),
        ));
    }
    Ok(out)
}

fn io_err_to_storage(e: io::Error) -> Error {
    Error::Io(e.raw_os_error().unwrap_or(libc::EIO))
}

#[cfg(test)]
mod tests {
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU64, Ordering};

    use super::*;

    static SEQ: AtomicU64 = AtomicU64::new(0);

    struct TempPath(PathBuf);
    impl Drop for TempPath {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.0);
        }
    }

    fn unique_path(name: &str) -> PathBuf {
        let n = SEQ.fetch_add(1, Ordering::Relaxed);
        let mut p = std::env::temp_dir();
        p.push(format!(
            "uring-provision-{}-{}-{}.bin",
            name,
            std::process::id(),
            n
        ));
        p
    }

    #[test]
    fn provision_file_creates_and_sizes() {
        const PAGE: usize = 4096;
        let path = TempPath(unique_path("creates"));
        assert!(!path.0.exists());

        provision_file(&path.0, (16 * PAGE) as u64).expect("provision");

        let len = std::fs::metadata(&path.0).unwrap().len();
        assert_eq!(len, (16 * PAGE) as u64);

        let file = open_file(&path.0, false).expect("open");
        let pages = file_capacity_pages(&file, PAGE).expect("capacity");
        assert_eq!(pages, 16);
    }

    #[test]
    fn provision_file_grows_then_shrinks() {
        const PAGE: usize = 4096;
        let path = TempPath(unique_path("resize"));

        provision_file(&path.0, (8 * PAGE) as u64).expect("provision 8");
        assert_eq!(std::fs::metadata(&path.0).unwrap().len(), (8 * PAGE) as u64);

        provision_file(&path.0, (4 * PAGE) as u64).expect("provision 4");
        assert_eq!(std::fs::metadata(&path.0).unwrap().len(), (4 * PAGE) as u64);

        provision_file(&path.0, (12 * PAGE) as u64).expect("provision 12");
        assert_eq!(
            std::fs::metadata(&path.0).unwrap().len(),
            (12 * PAGE) as u64
        );
    }
}
