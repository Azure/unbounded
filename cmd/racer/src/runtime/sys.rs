//! Memory and CPU topology: worker CPU selection, pinning, and large node-local memory.
//!
//! The policy lives here and the syscalls live behind `kernel`, so a simulated node picks
//! its workers by the same rules a real one does, off a topology the simulator described.

use std::io;
use std::path::Path;

use crate::kernel;

pub(super) use crate::kernel::{page_size, pin, siblings};

/// Logical CPUs this process is allowed to run on, one per *physical* core: the
/// `sched_getaffinity` set, with the sysfs sibling list folding SMT pairs to their lowest
/// allowed member. Never configured; narrow it with `taskset` or a cgroup cpuset.
pub(super) fn worker_cpus() -> io::Result<Vec<usize>> {
    let allowed = kernel::affinity()?;
    let mut out = Vec::new();

    for &cpu in &allowed {
        let leader = siblings(cpu)
            .unwrap_or_default()
            .into_iter()
            .filter(|s| allowed.contains(s))
            .min()
            .unwrap_or(cpu);
        if leader == cpu {
            out.push(cpu);
        }
    }

    if out.is_empty() {
        return Err(io::Error::other("no usable CPUs"));
    }

    Ok(out)
}

/// The sibling of `cpu` no worker owns, if any; used to park the control thread.
pub(super) fn sibling_of(cpu: usize) -> Option<usize> {
    siblings(cpu)?.into_iter().find(|&s| s != cpu)
}

/// Anonymous mapping of node-local memory, huge-page backed where the host allows it.
pub(super) struct Region {
    ptr: *mut u8,
    len: usize,
}

// SAFETY: the owner is responsible for the aliasing rules of whatever lives inside.
unsafe impl Send for Region {}
unsafe impl Sync for Region {}

impl Region {
    pub(super) fn new(len: usize) -> io::Result<Region> {
        // Rounded to a huge page so the mapping can be backed by one.
        let len = len.next_multiple_of(2 << 20);

        Ok(Region {
            ptr: kernel::map_anon(len)?,
            len,
        })
    }

    pub(super) fn as_ptr(&self) -> *mut u8 {
        self.ptr
    }
}

impl Drop for Region {
    fn drop(&mut self) {
        unsafe { kernel::unmap(self.ptr, self.len) };
    }
}

/// Opens a file or block device for direct IO. All handler-visible disks use this.
pub(super) fn open_direct(path: &Path) -> io::Result<kernel::File> {
    kernel::open(path, libc::O_RDWR | libc::O_DIRECT | libc::O_CLOEXEC, 0)
}

/// A read-only mapping of a ublk queue's io_desc array.
pub(super) struct Mapping {
    ptr: *mut u8,
    len: usize,
}

unsafe impl Send for Mapping {}
unsafe impl Sync for Mapping {}

impl Mapping {
    pub(super) fn map_read(f: kernel::FileRef, offset: u64, len: usize) -> io::Result<Mapping> {
        let len = len.next_multiple_of(page_size());

        Ok(Mapping {
            ptr: kernel::map_read(f, offset, len)?,
            len,
        })
    }

    pub(super) fn as_ptr(&self) -> *const u8 {
        self.ptr
    }
}

impl Drop for Mapping {
    fn drop(&mut self) {
        unsafe { kernel::unmap(self.ptr, self.len) };
    }
}
