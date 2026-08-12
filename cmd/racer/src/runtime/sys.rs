//! Memory and CPU topology: worker CPU selection, pinning, and large node-local memory.

use std::fs;
use std::io;
use std::os::fd::{AsRawFd, BorrowedFd, FromRawFd, OwnedFd};
use std::path::Path;

/// Logical CPUs this process is allowed to run on, one per *physical* core: the
/// `sched_getaffinity` set, with the sysfs sibling list folding SMT pairs to their lowest
/// allowed member. Never configured; narrow it with `taskset` or a cgroup cpuset.
pub(super) fn worker_cpus() -> io::Result<Vec<usize>> {
    let allowed = affinity()?;
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

fn affinity() -> io::Result<Vec<usize>> {
    let mut set: libc::cpu_set_t = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::sched_getaffinity(0, size_of::<libc::cpu_set_t>(), &mut set) };
    if rc != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok((0..libc::CPU_SETSIZE as usize)
        .filter(|&c| unsafe { libc::CPU_ISSET(c, &set) })
        .collect())
}

/// Logical CPUs sharing `cpu`'s physical core, including `cpu` itself.
pub(super) fn siblings(cpu: usize) -> Option<Vec<usize>> {
    let p = format!("/sys/devices/system/cpu/cpu{cpu}/topology/thread_siblings_list");
    Some(parse_cpu_list(fs::read_to_string(p).ok()?.trim()))
}

/// Parses the kernel's "0-3,8" CPU list format.
fn parse_cpu_list(s: &str) -> Vec<usize> {
    let mut out = Vec::new();
    for part in s.split(',') {
        match part.split_once('-') {
            Some((a, b)) => {
                if let (Ok(a), Ok(b)) = (a.parse(), b.parse::<usize>()) {
                    out.extend(a..=b);
                }
            }
            None => {
                if let Ok(a) = part.parse() {
                    out.push(a);
                }
            }
        }
    }
    out
}

/// Pins the calling thread to a single CPU. Workers never migrate after this.
pub(super) fn pin(cpu: usize) -> io::Result<()> {
    let mut set: libc::cpu_set_t = unsafe { std::mem::zeroed() };
    unsafe { libc::CPU_ZERO(&mut set) };
    unsafe { libc::CPU_SET(cpu, &mut set) };
    let rc = unsafe { libc::sched_setaffinity(0, size_of::<libc::cpu_set_t>(), &set) };
    if rc != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

/// Anonymous mapping backed by huge pages: `MAP_HUGETLB` first (needs a reserved pool),
/// else `MADV_HUGEPAGE`. The constructing thread first-touches it, fixing the NUMA node.
pub(super) struct Region {
    ptr: *mut u8,
    len: usize,
}

// SAFETY: the owner is responsible for the aliasing rules of whatever lives inside.
unsafe impl Send for Region {}
unsafe impl Sync for Region {}

impl Region {
    pub(super) fn new(len: usize) -> io::Result<Region> {
        let len = len.next_multiple_of(2 << 20);
        let prot = libc::PROT_READ | libc::PROT_WRITE;
        let base = libc::MAP_PRIVATE | libc::MAP_ANONYMOUS;
        // Simulated workers barely touch a region: no huge pages and no first-touch pass.
        let want_huge = !cfg!(feature = "sim");
        let mut ptr = libc::MAP_FAILED;
        if want_huge {
            ptr = unsafe {
                libc::mmap(
                    std::ptr::null_mut(),
                    len,
                    prot,
                    base | libc::MAP_HUGETLB,
                    -1,
                    0,
                )
            };
        }
        let mut huge = ptr != libc::MAP_FAILED;
        if !huge {
            ptr = unsafe { libc::mmap(std::ptr::null_mut(), len, prot, base, -1, 0) };
        }
        if ptr == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        if want_huge {
            if !huge {
                unsafe { libc::madvise(ptr, len, libc::MADV_HUGEPAGE) };
            }
            unsafe { std::ptr::write_bytes(ptr as *mut u8, 0, len) };
            huge = true;
        }
        let _ = huge;
        Ok(Region {
            ptr: ptr as *mut u8,
            len,
        })
    }

    pub(super) fn as_ptr(&self) -> *mut u8 {
        self.ptr
    }
}

impl Drop for Region {
    fn drop(&mut self) {
        unsafe { libc::munmap(self.ptr as *mut libc::c_void, self.len) };
    }
}

/// Opens a file or block device for direct IO. All handler-visible disks use this.
pub(super) fn open_direct(path: &Path) -> io::Result<OwnedFd> {
    open_flags(path, libc::O_RDWR | libc::O_DIRECT | libc::O_CLOEXEC)
}

pub(super) fn open_flags(path: &Path, flags: i32) -> io::Result<OwnedFd> {
    let c = std::ffi::CString::new(path.as_os_str().as_encoded_bytes())
        .map_err(|_| io::Error::from_raw_os_error(libc::EINVAL))?;
    let fd = unsafe { libc::open(c.as_ptr(), flags) };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(unsafe { OwnedFd::from_raw_fd(fd) })
}

/// A read-only mapping of a ublk queue's io_desc array.
pub(super) struct Mapping {
    ptr: *mut u8,
    len: usize,
}

unsafe impl Send for Mapping {}
unsafe impl Sync for Mapping {}

impl Mapping {
    pub(super) fn map_read(fd: BorrowedFd<'_>, offset: u64, len: usize) -> io::Result<Mapping> {
        let len = len.next_multiple_of(page_size());
        let ptr = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ,
                libc::MAP_SHARED | libc::MAP_POPULATE,
                fd.as_raw_fd(),
                offset as libc::off_t,
            )
        };
        if ptr == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        Ok(Mapping {
            ptr: ptr as *mut u8,
            len,
        })
    }

    pub(super) fn as_ptr(&self) -> *const u8 {
        self.ptr
    }
}

impl Drop for Mapping {
    fn drop(&mut self) {
        unsafe { libc::munmap(self.ptr as *mut libc::c_void, self.len) };
    }
}

pub(super) fn page_size() -> usize {
    unsafe { libc::sysconf(libc::_SC_PAGESIZE) as usize }
}
