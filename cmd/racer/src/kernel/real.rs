//! The real kernel: these are the syscalls.
//!
//! What state there is, a real process holds for the one node it is: an epoch no two
//! nodes can disagree about, and the counters that node keeps while it runs.

pub(crate) mod ring;

use std::ffi::CString;
use std::io;
use std::os::fd::{AsRawFd, BorrowedFd, FromRawFd, OwnedFd, RawFd};
use std::path::Path;
use std::sync::atomic::{AtomicU64, Ordering};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

/// The host's monotonic clock.
pub(super) fn now() -> Instant {
    Instant::now()
}

/// Nanoseconds since the first call in this process.
///
/// `Instant` has no numeric value, so the origin has to be captured once and elapsed from.
/// Process-wide is safe here in a way it is not elsewhere: the origin is only an origin,
/// and two nodes sharing one costs nothing but a common zero.
pub(super) fn now_ns() -> u64 {
    use std::sync::OnceLock;
    static BASE: OnceLock<Instant> = OnceLock::new();
    BASE.get_or_init(Instant::now).elapsed().as_nanos() as u64
}

pub(super) fn sleep_blocking(d: Duration) {
    std::thread::sleep(d);
}

pub(super) fn page_size() -> usize {
    unsafe { libc::sysconf(libc::_SC_PAGESIZE) as usize }
}

/// The logical CPUs this process is allowed to run on.
pub(super) fn affinity() -> io::Result<Vec<usize>> {
    let mut set: libc::cpu_set_t = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::sched_getaffinity(0, size_of::<libc::cpu_set_t>(), &mut set) };
    if rc != 0 {
        return Err(io::Error::last_os_error());
    }

    Ok((0..libc::CPU_SETSIZE as usize)
        .filter(|&c| unsafe { libc::CPU_ISSET(c, &set) })
        .collect())
}

/// The logical CPUs sharing `cpu`'s physical core, including `cpu` itself.
pub(super) fn siblings(cpu: usize) -> Option<Vec<usize>> {
    let p = format!("/sys/devices/system/cpu/cpu{cpu}/topology/thread_siblings_list");
    Some(super::parse_cpu_list(
        std::fs::read_to_string(p).ok()?.trim(),
    ))
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

/// An anonymous mapping backed by huge pages: `MAP_HUGETLB` first, which needs a reserved
/// pool, else `MADV_HUGEPAGE`. The calling thread first-touches it, fixing the NUMA node.
pub(super) fn map_anon(len: usize) -> io::Result<*mut u8> {
    let prot = libc::PROT_READ | libc::PROT_WRITE;
    let base = libc::MAP_PRIVATE | libc::MAP_ANONYMOUS;

    let mut ptr = unsafe {
        libc::mmap(
            std::ptr::null_mut(),
            len,
            prot,
            base | libc::MAP_HUGETLB,
            -1,
            0,
        )
    };
    let huge = ptr != libc::MAP_FAILED;
    if !huge {
        ptr = unsafe { libc::mmap(std::ptr::null_mut(), len, prot, base, -1, 0) };
    }

    if ptr == libc::MAP_FAILED {
        return Err(io::Error::last_os_error());
    }

    if !huge {
        unsafe { libc::madvise(ptr, len, libc::MADV_HUGEPAGE) };
    }

    unsafe { std::ptr::write_bytes(ptr as *mut u8, 0, len) };

    Ok(ptr as *mut u8)
}

/// A read-only shared mapping of `len` bytes at `offset` in `fd`, prefaulted.
pub(super) fn map_read(fd: RawFd, offset: u64, len: usize) -> io::Result<*mut u8> {
    let ptr = unsafe {
        libc::mmap(
            std::ptr::null_mut(),
            len,
            libc::PROT_READ,
            libc::MAP_SHARED | libc::MAP_POPULATE,
            fd,
            offset as libc::off_t,
        )
    };
    if ptr == libc::MAP_FAILED {
        return Err(io::Error::last_os_error());
    }

    Ok(ptr as *mut u8)
}

/// # Safety
///
/// `ptr` and `len` must name a mapping this module returned, and nothing may still be
/// borrowing it.
pub(super) unsafe fn unmap(ptr: *mut u8, len: usize) {
    unsafe { libc::munmap(ptr as *mut libc::c_void, len) };
}

/// Opens a path. `mode` applies only when `flags` carries `O_CREAT`.
pub(super) fn open(path: &Path, flags: i32, mode: u32) -> io::Result<OwnedFd> {
    let c = CString::new(path.as_os_str().as_encoded_bytes())
        .map_err(|_| io::Error::from_raw_os_error(libc::EINVAL))?;
    let fd = unsafe { libc::open(c.as_ptr(), flags, mode as libc::c_uint) };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }

    // SAFETY: `open` returned a fresh descriptor that nothing else owns.
    Ok(unsafe { OwnedFd::from_raw_fd(fd) })
}

/// Fills `buf` from `off`. A short read is an error: every caller here knows the extent it
/// asked for is present, so a partial answer is a corrupt store, not a smaller one.
pub(super) fn pread(fd: BorrowedFd<'_>, buf: &mut [u8], off: u64) -> io::Result<()> {
    let n = unsafe {
        libc::pread(
            fd.as_raw_fd(),
            buf.as_mut_ptr().cast(),
            buf.len(),
            off as i64,
        )
    };
    if n < 0 {
        return Err(io::Error::last_os_error());
    }

    if n as usize != buf.len() {
        return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "short read"));
    }

    Ok(())
}

/// Writes all of `buf` at `off`. A short write is an error, for the same reason.
pub(super) fn pwrite(fd: BorrowedFd<'_>, buf: &[u8], off: u64) -> io::Result<()> {
    let n = unsafe { libc::pwrite(fd.as_raw_fd(), buf.as_ptr().cast(), buf.len(), off as i64) };
    if n < 0 {
        return Err(io::Error::last_os_error());
    }

    if n as usize != buf.len() {
        return Err(io::Error::new(io::ErrorKind::WriteZero, "short write"));
    }

    Ok(())
}

pub(super) fn fdatasync(fd: BorrowedFd<'_>) -> io::Result<()> {
    if unsafe { libc::fdatasync(fd.as_raw_fd()) } != 0 {
        return Err(io::Error::last_os_error());
    }

    Ok(())
}

/// Reserves `len` bytes of blocks, falling back to a bare resize where the filesystem
/// cannot. Reserving matters: a store that finds the filesystem full halfway through a
/// write cannot report it to a guest whose write was already acknowledged.
pub(super) fn allocate(fd: BorrowedFd<'_>, len: u64) -> io::Result<()> {
    if unsafe { libc::fallocate(fd.as_raw_fd(), 0, 0, len as libc::off_t) } == 0 {
        return Ok(());
    }

    let e = io::Error::last_os_error();
    match e.raw_os_error() {
        Some(libc::EOPNOTSUPP) | Some(libc::ENOSYS) => {
            if unsafe { libc::ftruncate(fd.as_raw_fd(), len as libc::off_t) } != 0 {
                return Err(io::Error::last_os_error());
            }

            Ok(())
        }
        _ => Err(e),
    }
}

pub(super) fn file_len(fd: BorrowedFd<'_>) -> io::Result<u64> {
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    if unsafe { libc::fstat(fd.as_raw_fd(), &mut st) } != 0 {
        return Err(io::Error::last_os_error());
    }

    Ok(st.st_size as u64)
}

pub(super) fn create_dir_all(path: &Path) -> io::Result<()> {
    std::fs::create_dir_all(path)
}

/// Every counter this process keeps. One process is one node, so process-wide is the
/// node's own scope, and `Relaxed` is enough: nothing is ordered against these.
fn counters() -> &'static [AtomicU64; super::COUNTERS] {
    static COUNTERS: [AtomicU64; super::COUNTERS] = [const { AtomicU64::new(0) }; super::COUNTERS];
    &COUNTERS
}

pub(super) fn counter(i: usize) -> u64 {
    counters()[i].load(Ordering::Relaxed)
}

pub(super) fn set_counter(i: usize, v: u64) {
    counters()[i].store(v, Ordering::Relaxed);
}

pub(super) fn add_counter(i: usize, v: u64) {
    counters()[i].fetch_add(v, Ordering::Relaxed);
}

pub(super) fn swap_counter(i: usize, v: u64) -> u64 {
    counters()[i].swap(v, Ordering::AcqRel)
}

/// A panicking worker leaves the ring, op slab and ublk queue unrecoverable, so abort.
pub(super) fn on_worker_panic(core: usize) {
    eprintln!("racer: worker {core} panicked; aborting");
    std::process::abort();
}

/// The open `/dev/ublk-control`, with the ring every command rides on.
///
/// Depth one is all this needs: each command is submitted and waited for before the next
/// is built, because the node's control plane is a single thread doing one thing at a time
/// and the answers are what it sequences on.
pub(crate) struct UblkControl {
    ring: io_uring::IoUring<io_uring::squeue::Entry128, io_uring::cqueue::Entry>,
    fd: OwnedFd,
}

pub(super) fn ublk_control_open() -> io::Result<UblkControl> {
    let fd = open(
        Path::new("/dev/ublk-control"),
        libc::O_RDWR | libc::O_CLOEXEC,
        0,
    )?;
    // 128-byte SQEs: a `ublksrv_ctrl_cmd` is 32 bytes and rides in the command area, which
    // a 64-byte SQE does not have.
    let ring = io_uring::IoUring::builder().build(8)?;

    Ok(UblkControl { ring, fd })
}

pub(super) fn ublk_exec(c: &mut UblkControl, op: u32, cmd: &[u8; 80]) -> io::Result<i32> {
    let e = io_uring::opcode::UringCmd80::new(io_uring::types::Fd(c.fd.as_raw_fd()), op)
        .cmd(*cmd)
        .build()
        .user_data(0);
    // SAFETY: the command buffer is copied into the SQE, and the buffers it points at
    // outlive the wait below.
    unsafe { c.ring.submission().push(&e) }
        .map_err(|_| io::Error::from_raw_os_error(libc::ENOSPC))?;
    c.ring.submit_and_wait(1)?;
    let cqe = c.ring.completion().next().expect("cqe");
    if cqe.result() < 0 {
        return Err(io::Error::from_raw_os_error(-cqe.result()));
    }

    Ok(cqe.result())
}

/// Whether `pid` is a process we could signal, which is as close to "still running" as
/// anyone gets. `EPERM` counts: it is someone else's, so it is certainly there.
pub(super) fn process_alive(pid: i32) -> bool {
    unsafe { libc::kill(pid, 0) == 0 || *libc::__errno_location() == libc::EPERM }
}

/// Starts `f` on a named thread.
pub(super) fn spawn(
    name: String,
    mut t: impl super::Task + Send + 'static,
) -> io::Result<JoinHandle<()>> {
    std::thread::Builder::new().name(name).spawn(move || {
        t.start();
        while t.turn() != super::Turn::Done {}
        t.finish();
    })
}

/// Runs the pieces at once and waits for all of them.
///
/// Scoped, so the work may borrow what the caller has on its stack: the scan borrows the
/// geometry and the rate budget, and neither has any business being an `Arc`.
pub(super) fn parallel<T, F>(threads: usize, f: F) -> Vec<T>
where
    F: Fn(usize) -> T + Sync,
    T: Send,
{
    let f = &f;
    std::thread::scope(|s| {
        let hs: Vec<_> = (0..threads).map(|t| s.spawn(move || f(t))).collect();
        hs.into_iter().map(|h| h.join().unwrap()).collect()
    })
}
