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
//! dataplane fault: [`HostMapping::new`] accepts only a memfd (a file on the
//! kernel's internal shmem mount) that carries `F_SEAL_SHRINK`, is at least as long as the window, and is
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
use std::sync::Mutex;
use std::sync::OnceLock;
use std::sync::atomic::AtomicU16;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;
use std::sync::mpsc;
use std::time::Duration;
use std::time::Instant;

/// `madvise` advice that faults pages in writable; Linux 5.14 and later.
const MADV_POPULATE_WRITE: libc::c_int = 23;

/// `statfs.f_type` of tmpfs, which holds memfds.
const TMPFS_MAGIC: i64 = 0x0102_1994;

/// How long a cached mapping that no request uses stays mapped. Holding a
/// mapping keeps the client's memfd alive after the client closes it, so keep
/// this short.
const CACHE_IDLE: Duration = Duration::from_secs(5);

/// Most mappings the cache holds.
const CACHE_MAX_ENTRIES: usize = 128;

/// Most bytes of client files the cache keeps mapped.
const CACHE_MAX_BYTES: u64 = 256 << 30;

/// How often the background thread evicts idle mappings.
const REAPER_PERIOD: Duration = Duration::from_secs(1);

/// One `MAP_SHARED` mapping of a whole client file, from offset 0. Unmapped
/// on drop. The mapping holds a reference to the file, so its inode cannot be
/// freed while mapped. Its inode number is not reused either: memfds share the
/// internal shmem mount, whose inode numbers come from a 64-bit counter that
/// does not wrap (Linux 5.9 and later), unlike a user tmpfs mount's.
pub(crate) struct Region {
    base: NonNull<u8>,

    length: usize,

    /// One bit per [`POPULATED_CHUNK`] of the mapping already faulted in
    /// writable, so a reused mapping skips `MADV_POPULATE_WRITE`. A hint
    /// only: pages the kernel later reclaims fault back in on access.
    populated: Box<[AtomicU64]>,

    /// One entry per [`POPULATED_CHUNK`]: 1 + the NUMA node its first page
    /// was found on, or 0 when not yet known. A hint, like `populated`.
    nodes: Box<[AtomicU16]>,
}

/// Granularity of [`Region::populated`].
const POPULATED_CHUNK: usize = 2 << 20;

// SAFETY: the mapping is plain shared memory with no thread affinity.
unsafe impl Send for Region {}
unsafe impl Sync for Region {}

impl Region {
    fn map(fd: libc::c_int, length: usize) -> Option<Self> {
        // SAFETY: a fresh mapping at a kernel-chosen address. The shrink seal
        // keeps the file at least `length` bytes long for the mapping's
        // lifetime, so no access inside it can fault past the end of the file.
        let base = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                length,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED,
                fd,
                0,
            )
        };
        if base == libc::MAP_FAILED {
            return None;
        }
        let chunks = length.div_ceil(POPULATED_CHUNK);
        Some(Self {
            base: NonNull::new(base.cast())?,
            length,
            populated: (0..chunks.div_ceil(64))
                .map(|_| AtomicU64::new(0))
                .collect(),
            nodes: (0..chunks).map(|_| AtomicU16::new(0)).collect(),
        })
    }

    /// NUMA node of the chunk holding byte `at`, from its first resident
    /// page. Cached per chunk once found; None while no page is resident
    /// there, so a later first touch decides it. Never faults a page in.
    fn node_at(&self, at: usize) -> Option<u16> {
        let chunk = at / POPULATED_CHUNK;
        let slot = self.nodes.get(chunk)?;
        match slot.load(Ordering::Relaxed) {
            0 => {}
            known => return Some(known - 1),
        }
        if at >= self.length {
            return None;
        }
        // SAFETY: `at` is inside the mapping; the address is only queried.
        let address = unsafe { self.base.as_ptr().add(at) };
        let node = crate::affinity::resident_numa_node(address)
            .and_then(|node| u16::try_from(node).ok())
            .filter(|node| *node < u16::MAX)?;
        slot.store(node + 1, Ordering::Relaxed);
        Some(node)
    }

    /// Chunks wholly inside `[at, at + length)`.
    fn whole_chunks(at: usize, length: usize) -> std::ops::Range<usize> {
        at.div_ceil(POPULATED_CHUNK)..(at + length) / POPULATED_CHUNK
    }

    /// Whether every chunk that `[at, at + length)` touches is populated.
    fn is_populated(&self, at: usize, length: usize) -> bool {
        let first = at / POPULATED_CHUNK;
        let last = (at + length).div_ceil(POPULATED_CHUNK);
        (first..last).all(|chunk| {
            self.populated[chunk / 64].load(Ordering::Relaxed) & (1 << (chunk % 64)) != 0
        })
    }

    /// Mark the chunks wholly inside `[at, at + length)` populated.
    fn mark_populated(&self, at: usize, length: usize) {
        for chunk in Self::whole_chunks(at, length) {
            self.populated[chunk / 64].fetch_or(1 << (chunk % 64), Ordering::Relaxed);
        }
    }
}

impl Drop for Region {
    fn drop(&mut self) {
        // SAFETY: unmaps exactly the mapping made in `map`; no slices outlive
        // the jobs that hold this region.
        unsafe { libc::munmap(self.base.as_ptr().cast(), self.length) };
    }
}

/// Identity of a client memfd: `st_dev` and `st_ino`, unique among live
/// memfds (see [`Region`]).
type FileKey = (u64, u64);

struct Entry {
    key: FileKey,

    region: Arc<Region>,

    last_used: Instant,
}

/// A bounded cache of host-read client mappings, keyed by file identity.
/// Repeated requests that present the same memfd reuse one mapping instead of
/// mapping and unmapping it per request; both take the process `mmap_lock`
/// for writing, and every unmap shoots down TLB entries on all CPUs.
///
/// A request reuses a mapping only after presenting a descriptor for the same
/// file and passing every check [`HostMapping::new`] makes, so the dataplane
/// writes into a mapping only on behalf of a request that presented that
/// file.
pub(crate) struct MappingCache {
    entries: Mutex<Vec<Entry>>,

    max_entries: usize,

    max_bytes: u64,

    idle: Duration,

    reuses: AtomicU64,

    created: AtomicU64,

    evicted: AtomicU64,

    cached: AtomicU64,

    cached_bytes: AtomicU64,
}

/// Counters and gauges of the host-read mapping cache.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct MappingCacheStats {
    pub(crate) reuses: u64,

    pub(crate) created: u64,

    pub(crate) evicted: u64,

    pub(crate) cached: u64,

    pub(crate) cached_bytes: u64,
}

impl MappingCache {
    pub(crate) fn new(max_entries: usize, max_bytes: u64, idle: Duration) -> Self {
        Self {
            entries: Mutex::new(Vec::new()),
            max_entries,
            max_bytes,
            idle,
            reuses: AtomicU64::new(0),
            created: AtomicU64::new(0),
            evicted: AtomicU64::new(0),
            cached: AtomicU64::new(0),
            cached_bytes: AtomicU64::new(0),
        }
    }

    /// The process-wide cache. The first call starts the thread that evicts
    /// idle mappings and unmaps released ones.
    pub(crate) fn global() -> &'static Self {
        static CACHE: OnceLock<MappingCache> = OnceLock::new();
        CACHE.get_or_init(|| {
            let cache = Self::new(CACHE_MAX_ENTRIES, CACHE_MAX_BYTES, CACHE_IDLE);
            reaper();
            cache
        })
    }

    pub(crate) fn stats(&self) -> MappingCacheStats {
        MappingCacheStats {
            reuses: self.reuses.load(Ordering::Relaxed),
            created: self.created.load(Ordering::Relaxed),
            evicted: self.evicted.load(Ordering::Relaxed),
            cached: self.cached.load(Ordering::Relaxed),
            cached_bytes: self.cached_bytes.load(Ordering::Relaxed),
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Vec<Entry>> {
        self.entries.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn publish(&self, entries: &[Entry]) {
        self.cached.store(entries.len() as u64, Ordering::Relaxed);
        let bytes = entries.iter().map(|e| e.region.length as u64).sum();
        self.cached_bytes.store(bytes, Ordering::Relaxed);
    }

    /// A mapping of the file behind `fd` that covers `[0, end)`. `size` is the
    /// file's current size, at least `end`. The caller has checked the file.
    fn get(&self, fd: libc::c_int, key: FileKey, end: u64, size: u64) -> Option<Arc<Region>> {
        let now = Instant::now();
        {
            let mut entries = self.lock();
            if let Some(entry) = entries
                .iter_mut()
                .find(|e| e.key == key && e.region.length as u64 >= end)
            {
                entry.last_used = now;
                self.reuses.fetch_add(1, Ordering::Relaxed);
                return Some(entry.region.clone());
            }
        }
        // Map outside the lock: mmap takes the process mmap_lock.
        let region = Arc::new(Region::map(fd, usize::try_from(size).ok()?)?);
        self.created.fetch_add(1, Ordering::Relaxed);
        let mut released = Vec::new();
        {
            let mut entries = self.lock();
            // A smaller mapping of the same file, made before it grew, is
            // replaced. Requests that hold it keep it alive.
            let mut index = 0;
            while index < entries.len() {
                if entries[index].key == key {
                    released.push(entries.swap_remove(index).region);
                } else {
                    index += 1;
                }
            }
            if region.length as u64 <= self.max_bytes && self.max_entries > 0 {
                entries.push(Entry {
                    key,
                    region: region.clone(),
                    last_used: now,
                });
                // Evict least recently used mappings until within the caps.
                // An evicted mapping still in use stays mapped until its last
                // request ends.
                while entries.len() > self.max_entries
                    || entries.iter().map(|e| e.region.length as u64).sum::<u64>() > self.max_bytes
                {
                    let Some((oldest, _)) = entries
                        .iter()
                        .enumerate()
                        .filter(|(_, e)| !Arc::ptr_eq(&e.region, &region))
                        .min_by_key(|(_, e)| e.last_used)
                    else {
                        break;
                    };
                    released.push(entries.swap_remove(oldest).region);
                    self.evicted.fetch_add(1, Ordering::Relaxed);
                }
            }
            self.publish(&entries);
        }
        released.into_iter().for_each(release);
        Some(region)
    }

    /// Drop mappings no request has used for the idle time and none holds.
    /// Returns them so the caller unmaps them; the reaper thread does.
    fn take_idle(&self, now: Instant) -> Vec<Arc<Region>> {
        let mut entries = self.lock();
        let mut idle = Vec::new();
        let mut index = 0;
        while index < entries.len() {
            let entry = &entries[index];
            if Arc::strong_count(&entry.region) == 1
                && now.saturating_duration_since(entry.last_used) >= self.idle
            {
                idle.push(entries.swap_remove(index).region);
            } else {
                index += 1;
            }
        }
        self.evicted.fetch_add(idle.len() as u64, Ordering::Relaxed);
        self.publish(&entries);
        idle
    }
}

/// Channel to the background thread that unmaps released regions, so munmap
/// and its TLB shootdown stay off the IO and crypto workers.
fn reaper() -> Option<&'static mpsc::Sender<Arc<Region>>> {
    static REAPER: OnceLock<Option<mpsc::Sender<Arc<Region>>>> = OnceLock::new();
    REAPER
        .get_or_init(|| {
            let (sender, receiver) = mpsc::channel::<Arc<Region>>();
            std::thread::Builder::new()
                .name("racer-hostmap".into())
                .spawn(move || {
                    loop {
                        match receiver.recv_timeout(REAPER_PERIOD) {
                            Ok(region) => drop(region),
                            Err(mpsc::RecvTimeoutError::Timeout) => {}
                            Err(mpsc::RecvTimeoutError::Disconnected) => return,
                        }
                        drop(MappingCache::global().take_idle(Instant::now()));
                    }
                })
                .ok()?;
            Some(sender)
        })
        .as_ref()
}

/// Hand a region reference to the reaper thread, or drop it here when there
/// is none.
fn release(region: Arc<Region>) {
    if let Some(sender) = reaper() {
        if let Err(mpsc::SendError(region)) = sender.send(region) {
            drop(region);
        }
    } else {
        drop(region);
    }
}

/// A request's view of a shared, writable mapping of a client file's
/// destination window. Crypto jobs hold it through an [`Arc`], so it outlives
/// a request that is dropped while a decrypt is still running. The region
/// under it is shared with other requests for the same file through
/// [`MappingCache`].
/// Whether `stat` describes a file on the kernel's internal shmem mount, which
/// holds every memfd. Files on a user tmpfs mount have another `st_dev`; their
/// inode numbers can wrap, so they could alias a cached mapping's key.
fn is_memfd(stat: &libc::stat) -> bool {
    static DEV: OnceLock<Option<u64>> = OnceLock::new();
    let dev = DEV.get_or_init(|| {
        // SAFETY: memfd_create takes a NUL-terminated name; the descriptor is
        // owned by the File, which closes it.
        let fd = unsafe { libc::memfd_create(c"racer-memfd-dev".as_ptr(), libc::MFD_CLOEXEC) };
        if fd < 0 {
            return None;
        }
        // SAFETY: fd is a fresh descriptor owned by nothing else.
        let probe = unsafe { <std::fs::File as std::os::fd::FromRawFd>::from_raw_fd(fd) };
        let mut stat: libc::stat = unsafe { std::mem::zeroed() };
        // SAFETY: fstat writes only into the zeroed buffer.
        if unsafe { libc::fstat(probe.as_raw_fd(), &mut stat) } != 0 {
            return None;
        }
        #[allow(clippy::unnecessary_cast)]
        Some(stat.st_dev as u64)
    });
    #[allow(clippy::unnecessary_cast)]
    let presented = stat.st_dev as u64;
    *dev == Some(presented)
}

pub(crate) struct HostMapping {
    /// Some until drop.
    region: Option<Arc<Region>>,

    /// The destination window, in file offsets.
    window: Destination,
}

impl HostMapping {
    /// Map `window` of `file` when it is safe to: a memfd sealed against
    /// shrinking, not sealed against writes, open read-write, and at least
    /// `window.end()` bytes long. None means use positioned writes instead.
    /// Reuses a cached mapping of the same file when one covers the window.
    pub(crate) fn new(file: &impl AsRawFd, window: Destination) -> Option<Self> {
        Self::new_in(MappingCache::global(), file, window)
    }

    pub(crate) fn new_in(
        cache: &MappingCache,
        file: &impl AsRawFd,
        window: Destination,
    ) -> Option<Self> {
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
        {
            return None;
        }
        if !is_memfd(&stat) {
            return None;
        }
        let size = u64::try_from(stat.st_size).ok()?;
        if size < end {
            return None;
        }
        #[allow(clippy::unnecessary_cast)]
        let key = (stat.st_dev as u64, stat.st_ino as u64);
        let region = cache.get(fd, key, end, size)?;
        Some(Self {
            region: Some(region),
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
        let region = self.region.as_ref().ok_or(Error::Internal)?;
        let at = usize::try_from(offset).map_err(|_| Error::InvalidRange)?;
        if at + length > region.length {
            return Err(Error::InvalidRange);
        }
        // SAFETY: inside the mapping, checked above.
        Ok(unsafe { std::slice::from_raw_parts_mut(region.base.as_ptr().add(at), length) })
    }

    /// NUMA node of the destination page at file offset `offset`, when a
    /// page is resident there. Decrypting on a helper of that node writes
    /// the plaintext to local memory. Cheap after the first query of each
    /// 2 MiB chunk of the cached mapping.
    pub(crate) fn node_at(&self, offset: u64) -> Option<u16> {
        if offset < self.window.offset || offset >= self.window.end() {
            return None;
        }
        let at = usize::try_from(offset).ok()?;
        self.region.as_ref()?.node_at(at)
    }

    /// Fault `bytes`, which start at file offset `offset`, in writable unless
    /// an earlier request already did for this mapping.
    fn populate(&self, offset: u64, bytes: &mut [u8]) -> Result<()> {
        let region = self.region.as_ref().ok_or(Error::Internal)?;
        let at = usize::try_from(offset).map_err(|_| Error::InvalidRange)?;
        if region.is_populated(at, bytes.len()) {
            return Ok(());
        }
        populate(bytes)?;
        region.mark_populated(at, bytes.len());
        Ok(())
    }
}

impl Drop for HostMapping {
    fn drop(&mut self) {
        // Usually the cache still holds the region; when this is the last
        // reference, the reaper thread unmaps it.
        if let Some(region) = self.region.take() {
            release(region);
        }
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

    /// The page's fused store once [`Self::run`] has filled and
    /// authenticated it.
    pub(crate) stored: Option<crate::memory::CiphertextBuffer>,

    /// NUMA node whose crypto helpers should run this job: the node of
    /// the destination pages, when they are resident. None lets the
    /// requester's helpers run it, and first touch puts the pages there.
    pub(crate) node: Option<u16>,

    /// Where a fused [`Self::run`] found its memory, relative to the crypto
    /// helper that ran it. None for other pages.
    pub(crate) numa: Option<DecryptNuma>,
}

/// Whether one buffer is on the NUMA node of the CPU that read or wrote it.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) enum Locality {
    /// The CPU's or the memory's node is not reported.
    #[default]
    Unknown,
    Local,
    Remote,
}

impl Locality {
    fn of(cpu: Option<usize>, bytes: &[u8]) -> Self {
        let memory = (!bytes.is_empty())
            .then(|| crate::affinity::page_numa_node(bytes))
            .flatten();
        match (cpu, memory) {
            (Some(cpu), Some(memory)) if cpu == memory => Self::Local,
            (Some(_), Some(_)) => Self::Remote,
            _ => Self::Unknown,
        }
    }
}

/// NUMA locality of the three buffers of one fused host-read decrypt. Each is
/// sampled at its first byte; a 16 MiB buffer may span nodes only under an
/// interleave policy.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct DecryptNuma {
    /// The registered RDMA receive buffer the page is read from.
    pub(crate) registered: Locality,

    /// The ciphertext store the page is copied into.
    pub(crate) store: Locality,

    /// The client's mapped destination the plaintext is written to.
    pub(crate) destination: Locality,
}

impl DecryptNuma {
    /// The events to count for this decrypt.
    pub(crate) fn events(self) -> [crate::telemetry::Event; 3] {
        use crate::telemetry::Event as E;
        let pick = |locality, [local, remote, unknown]: [E; 3]| match locality {
            Locality::Local => local,
            Locality::Remote => remote,
            Locality::Unknown => unknown,
        };
        [
            pick(
                self.registered,
                [
                    E::HostReadFusedNumaRegisteredLocal,
                    E::HostReadFusedNumaRegisteredRemote,
                    E::HostReadFusedNumaRegisteredUnknown,
                ],
            ),
            pick(
                self.store,
                [
                    E::HostReadFusedNumaStoreLocal,
                    E::HostReadFusedNumaStoreRemote,
                    E::HostReadFusedNumaStoreUnknown,
                ],
            ),
            pick(
                self.destination,
                [
                    E::HostReadFusedNumaDestinationLocal,
                    E::HostReadFusedNumaDestinationRemote,
                    E::HostReadFusedNumaDestinationUnknown,
                ],
            ),
        ]
    }
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
    ///
    /// When the page arrived in a registered RDMA buffer and the read path
    /// supplied aligned storage ([`SealedPage::fused_store`]), the page is
    /// copied into that storage and decrypted in one chunked pass, so the
    /// registered bytes are read from memory once. The filled storage is kept
    /// for [`Self::publish_store`] only when the page authenticates.
    pub(crate) fn run(&mut self) -> Result<()> {
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
        self.mapping.populate(self.destination, destination)?;
        let key = &self.page.key;
        if let Some(mut store) = self.page.fused_store.take()
            && store.len() == bytes.len()
        {
            // Whole and partial pages both decrypt into the chunk-sized,
            // cache-resident scratch and leave it with non-temporal stores,
            // so the mapping is never read for ownership. With many helpers
            // decrypting at once this moves about 1.7x the bytes of
            // decrypting straight into the mapping on a 2-socket EPYC 9575F
            // (`fused_open_bench`: 426 vs 249 GB/s at 128 threads), because
            // the node is memory-bandwidth bound, not AES bound.
            let output = racer_crypto::FusedOutput::Scratch {
                out: destination,
                start,
            };
            // A failure drops `store` unpublished, back to its pool.
            key.open_copy_into(
                &self.page.aad,
                bytes,
                store.as_mut_slice(),
                output,
                racer_crypto::FUSED_CHUNK,
                &mut page_alloc::stream::copy_nontemporal,
            )
            .map_err(|_| Error::CorruptRecord)?;
            // Three syscalls per page (up to 16 MiB): every buffer is
            // populated by now, so none faults.
            let cpu = crate::affinity::current_numa_node();
            self.numa = Some(DecryptNuma {
                registered: Locality::of(cpu, bytes),
                store: Locality::of(cpu, store.as_mut_slice()),
                destination: Locality::of(cpu, destination),
            });
            self.stored = Some(store);
            return Ok(());
        }
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

    /// Whether [`Self::run`] authenticated the page and filled its fused
    /// store, which [`Self::publish_store`] has not yet taken.
    pub(crate) fn fused(&self) -> bool {
        self.stored.is_some()
    }

    /// After a successful fused [`Self::run`], freeze the filled store into a
    /// ciphertext page and record it on the registered source, so the
    /// verdict's owner persists and caches that copy instead of copying the
    /// registered buffer again ([`crate::memory::CiphertextPage::retained_copy`]).
    /// Runs on the I/O worker. Does nothing for other pages.
    pub(crate) fn publish_store(&mut self) {
        let Some(store) = self.stored.take() else {
            return;
        };
        let SealedSource::Host(copy) = &self.page.source else {
            return;
        };
        let source = &copy.copy.ciphertext;
        let mut detached =
            crate::memory::CiphertextPage::from_aligned(store, source.envelope().clone());
        detached.provenance = source.provenance;
        source.record_copy_out(&detached);
    }
}

#[cfg(test)]
mod tests;
