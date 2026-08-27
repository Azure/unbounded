use std::io;
use std::path::Path;
use std::sync::Arc;

use crate::runtime::Limiter;

/// A page-aligned heap buffer, which O_DIRECT requires.
pub(crate) struct Aligned {
    ptr: *mut u8,
    len: usize,
}

impl Aligned {
    pub(crate) fn new(len: usize) -> Aligned {
        let layout = std::alloc::Layout::from_size_align(len, 4096).unwrap();
        // Safety: `len` is a nonzero multiple of the alignment at every call site.
        let ptr = unsafe { std::alloc::alloc(layout) };
        assert!(!ptr.is_null());
        Aligned { ptr, len }
    }

    pub(crate) fn as_ref(&self) -> &[u8] {
        unsafe { std::slice::from_raw_parts(self.ptr, self.len) }
    }

    pub(crate) fn as_mut(&mut self) -> &mut [u8] {
        unsafe { std::slice::from_raw_parts_mut(self.ptr, self.len) }
    }
}

impl Drop for Aligned {
    fn drop(&mut self) {
        let layout = std::alloc::Layout::from_size_align(self.len, 4096).unwrap();
        unsafe { std::alloc::dealloc(self.ptr, layout) };
    }
}

/// An open store, for control-plane paths only: format, superblock reads, the startup
/// scan. The hot path uses the runtime's `Disk`. A limiter, when present, is shared across
/// the scan's threads.
pub(crate) struct Dev(crate::kernel::File, Option<Arc<Limiter>>);

impl Dev {
    /// Pace this handle's transfers against a shared budget.
    pub(crate) fn meter(self, limit: Arc<Limiter>) -> Dev {
        Dev(self.0, Some(limit))
    }

    /// Wait out what the budget owes before a transfer of `len` bytes.
    ///
    /// Blocking; each caller has a thread. Which is why it goes through the kernel: a
    /// simulated thread that waits here is spending the run's own clock, and the
    /// scheduler is the only thing that can decide what else runs while it does.
    fn pace(&self, len: usize) {
        if let Some(d) = self.1.as_ref().and_then(|l| l.admit(len as u32)) {
            crate::kernel::sleep_blocking(d);
        }
    }
}

pub(crate) fn open_direct(path: &Path, write: bool) -> io::Result<Dev> {
    let flags = if write { libc::O_RDWR } else { libc::O_RDONLY };
    Ok(Dev(
        crate::kernel::open(path, flags | libc::O_DIRECT | libc::O_CLOEXEC, 0)?,
        None,
    ))
}

/// Hold the backing store at `node.store.size_bytes`, creating the file and its parent
/// directory the first time. Called before every `format` and every `grow`.
///
/// Grow only: every layout offset is absolute, so a page past a smaller end would vanish
/// rather than move; a longer file is refused, naming both sizes. Space is reserved with
/// `fallocate`, not just declared with `ftruncate`, because a store that finds the
/// filesystem full mid-write cannot report it to a guest whose write was acknowledged.
/// Filesystems without it fall back to a plain length change.
pub fn size_if_needed(path: &Path, cfg: &crate::config::Config) -> io::Result<()> {
    let want = cfg.node.store_bytes();
    if let Some(dir) = path.parent().filter(|d| !d.as_os_str().is_empty()) {
        crate::kernel::create_dir_all(dir)?;
    }
    // Never truncate: an existing store is being measured, not replaced.
    let f = crate::kernel::open(path, libc::O_RDWR | libc::O_CREAT | libc::O_CLOEXEC, 0o600)?;

    let have = crate::kernel::file_len(&f)?;
    if have > want {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            format!(
                "store {} is {have} B, node.store.size_bytes is {want} B; a store cannot shrink",
                path.display()
            ),
        ));
    }
    if have == want {
        return Ok(());
    }

    crate::kernel::allocate(&f, want)?;
    // The length is metadata the superblock is addressed relative to, so land it first.
    crate::kernel::fdatasync(&f)
}

/// Put everything written so far on stable storage; `format` and `grow` need this order,
/// since a superblock naming blocks the store lacks cannot be opened.
pub(super) fn sync(d: &Dev) -> io::Result<()> {
    crate::kernel::fdatasync(&d.0)
}

pub(crate) fn write_at(d: &Dev, b: &[u8], off: u64) -> io::Result<()> {
    d.pace(b.len());
    crate::kernel::pwrite(&d.0, b, off)
}

pub(crate) fn read_at(d: &Dev, b: &mut [u8], off: u64) -> io::Result<()> {
    d.pace(b.len());
    crate::kernel::pread(&d.0, b, off)
}
