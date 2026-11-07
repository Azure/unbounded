//! Decrypt sealed pages straight into a `host-read` destination.
//!
//! When the client's file is a sealed memfd that cannot shrink, the dataplane
//! maps the destination window and a crypto worker writes each page there:
//! it copies the page ciphertext into the mapping and authenticates and
//! decrypts it in place. Compared with decrypting into a pooled plaintext
//! buffer and writing that with io_uring, this skips the plaintext copy, the
//! kernel copy into the file, and the wipe of the pooled buffer. No plaintext
//! of these pages stays in dataplane memory, except for partial pages at the
//! ends of a range, which are decrypted into a scratch buffer that is wiped
//! on drop.
//!
//! A failed authentication zeroes the page's bytes in the destination. The
//! dataplane then fails the request: it sends no page frame for the page and
//! closes the connection without the completion frame, which the protocol
//! already defines as "the file may hold partial data".
//!
//! Mapping a client file is only safe when the client cannot make the
//! dataplane fault: [`HostMapping::new`] accepts only a tmpfs file (a memfd)
//! that carries `F_SEAL_SHRINK`, is at least as long as the window, and is
//! open for reading and writing. Anything else keeps positioned writes.
//!
//! The authentication result covers the bytes the crypto worker read back
//! from the client's mapping. A client that rewrites its own buffer during
//! the decrypt can only change what it receives itself.

use crate::error::Error;
use crate::error::Result;
use crate::read::sealed::SealedPage;
use crate::read::sealed::SealedSource;
use racer_wire::object::client::Destination;
use std::os::fd::AsRawFd;
use std::ptr::NonNull;
use std::sync::Arc;

/// `madvise` advice that faults pages in writable; Linux 5.14 and later.
const MADV_POPULATE_WRITE: libc::c_int = 23;

/// `statfs.f_type` of tmpfs, which holds memfds.
const TMPFS_MAGIC: i64 = 0x0102_1994;

/// A shared, writable mapping of a client file's destination window.
/// Unmapped on drop. Crypto jobs hold it through an [`Arc`], so it outlives a
/// request that is dropped while a decrypt is still running.
pub(crate) struct HostMapping {
    base: NonNull<u8>,

    length: usize,

    /// File offset of `base`.
    start: u64,

    /// The destination window, in file offsets.
    window: Destination,
}

// SAFETY: the mapping is plain shared memory with no thread affinity. Jobs
// write disjoint page ranges, one job per range at a time.
unsafe impl Send for HostMapping {}
unsafe impl Sync for HostMapping {}

impl HostMapping {
    /// Map `window` of `file` when it is safe to: a tmpfs file sealed against
    /// shrinking, not sealed against writes, open read-write, and at least
    /// `window.end()` bytes long. None means use positioned writes instead.
    pub(crate) fn new(file: &impl AsRawFd, window: Destination) -> Option<Self> {
        let fd = file.as_raw_fd();
        if window.length == 0 {
            return None;
        }
        let end = window.end();
        // SAFETY: fstatfs and fstat write only into the zeroed buffers.
        let mut fs: libc::statfs = unsafe { std::mem::zeroed() };
        if unsafe { libc::fstatfs(fd, &mut fs) } != 0 || fs.f_type as i64 != TMPFS_MAGIC {
            return None;
        }
        // SAFETY: F_GET_SEALS and F_GETFL only read descriptor state.
        let seals = unsafe { libc::fcntl(fd, libc::F_GET_SEALS) };
        if seals < 0
            || seals & libc::F_SEAL_SHRINK == 0
            || seals & (libc::F_SEAL_WRITE | libc::F_SEAL_FUTURE_WRITE) != 0
        {
            return None;
        }
        let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
        if flags < 0 || flags & libc::O_ACCMODE != libc::O_RDWR {
            return None;
        }
        let mut stat: libc::stat = unsafe { std::mem::zeroed() };
        if unsafe { libc::fstat(fd, &mut stat) } != 0
            || stat.st_mode & libc::S_IFMT != libc::S_IFREG
            || u64::try_from(stat.st_size).ok()? < end
        {
            return None;
        }
        let page = u64::try_from(unsafe { libc::sysconf(libc::_SC_PAGESIZE) }).ok()?;
        if !page.is_power_of_two() {
            return None;
        }
        let start = window.offset & !(page - 1);
        let length = usize::try_from(end - start).ok()?;
        let offset = libc::off_t::try_from(start).ok()?;
        // SAFETY: a fresh mapping at a kernel-chosen address. The seal keeps
        // the file at least `end` bytes long for the mapping's lifetime, so
        // no access inside it can fault past the end of the file.
        let base = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                length,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED,
                fd,
                offset,
            )
        };
        if base == libc::MAP_FAILED {
            return None;
        }
        Some(Self {
            base: NonNull::new(base.cast())?,
            length,
            start,
            window,
        })
    }

    /// The bytes at file offset `offset`, `length` long, inside the window.
    ///
    /// # Safety
    ///
    /// No other live reference may cover the same bytes. The client process
    /// may write them concurrently; callers must treat the contents as
    /// untrusted.
    #[allow(clippy::mut_from_ref)]
    unsafe fn bytes_mut(&self, offset: u64, length: usize) -> Result<&mut [u8]> {
        let end = offset
            .checked_add(length as u64)
            .ok_or(Error::InvalidRange)?;
        if offset < self.window.offset || end > self.window.end() {
            return Err(Error::InvalidRange);
        }
        let at = usize::try_from(offset - self.start).map_err(|_| Error::InvalidRange)?;
        debug_assert!(at + length <= self.length);
        // SAFETY: inside the mapping, checked above.
        Ok(unsafe { std::slice::from_raw_parts_mut(self.base.as_ptr().add(at), length) })
    }
}

impl Drop for HostMapping {
    fn drop(&mut self) {
        // SAFETY: unmaps exactly the mapping made in `new`; no slices outlive
        // the jobs that hold this mapping.
        unsafe { libc::munmap(self.base.as_ptr().cast(), self.length) };
    }
}

/// Fault `bytes` in writable in one call, so the copy does not take a page
/// fault per 4 KiB. A kernel without `MADV_POPULATE_WRITE` faults lazily.
fn populate(bytes: &mut [u8]) -> Result<()> {
    if bytes.is_empty() {
        return Ok(());
    }
    // SAFETY: sysconf has no side effects.
    let page = unsafe { libc::sysconf(libc::_SC_PAGESIZE) } as usize;
    let address = bytes.as_mut_ptr() as usize;
    let start = address & !(page - 1);
    let length = address + bytes.len() - start;
    // SAFETY: the range lies within a live mapping; advice does not change
    // its contents.
    if unsafe { libc::madvise(start as *mut libc::c_void, length, MADV_POPULATE_WRITE) } == 0 {
        return Ok(());
    }
    match std::io::Error::last_os_error().raw_os_error() {
        Some(libc::EINVAL) => Ok(()),
        Some(libc::EINTR | libc::EAGAIN) => Ok(()),
        _ => Err(Error::Internal),
    }
}

/// One sealed page for a crypto worker to decrypt into a host mapping.
pub struct DecryptInto {
    pub(crate) page: Box<SealedPage>,

    /// The plaintext bytes of the page to write.
    pub(crate) range: std::ops::Range<u64>,

    pub(crate) mapping: Arc<HostMapping>,

    /// File offset of `range.start`.
    pub(crate) destination: u64,
}

impl DecryptInto {
    /// Plaintext bytes this job writes.
    pub(crate) fn len(&self) -> u64 {
        self.range.end.saturating_sub(self.range.start)
    }

    /// The page number, for diagnostics.
    pub(crate) fn page_number(&self) -> u64 {
        self.page.page().number.0
    }

    /// Authenticate the page and write `range` of its plaintext to the
    /// mapping. Runs on a crypto worker. A failed authentication zeroes the
    /// written bytes and returns [`Error::CorruptRecord`].
    pub(crate) fn run(&self) -> Result<()> {
        let SealedSource::Host(copy) = &self.page.source else {
            return Err(Error::Internal);
        };
        let length = self.page.envelope.plaintext_length as usize;
        let bytes = copy.copy.ciphertext.bytes();
        if length.checked_add(racer_crypto::TAG_LEN) != Some(bytes.len()) {
            return Err(Error::CorruptRecord);
        }
        let (body, tag) = bytes.split_at(length);
        let start = usize::try_from(self.range.start).map_err(|_| Error::InvalidRange)?;
        let end = usize::try_from(self.range.end).map_err(|_| Error::InvalidRange)?;
        if start >= end || end > length {
            return Err(Error::InvalidRange);
        }
        // SAFETY: the sink delivers one page at a time and each job covers
        // its own destination bytes.
        let destination = unsafe { self.mapping.bytes_mut(self.destination, end - start)? };
        populate(destination)?;
        let key = &self.page.key;
        if start == 0 && end == length {
            // Decrypt from the ciphertext copy straight into the mapping in
            // one pass. A failed tag check zeroes the destination.
            return key
                .open_into(&self.page.aad, body, tag, destination)
                .map_err(|_| Error::CorruptRecord);
        }
        // A partial page: authenticate the whole page into wiped scratch
        // memory and copy out only the requested bytes.
        let mut scratch = zeroize::Zeroizing::new(vec![0u8; length]);
        key.open_into(&self.page.aad, body, tag, &mut scratch)
            .map_err(|_| Error::CorruptRecord)?;
        destination.copy_from_slice(&scratch[start..end]);
        Ok(())
    }
}

#[cfg(test)]
mod tests;
