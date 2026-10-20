//! Sparse file or device storage and completion-owned direct I/O.
//!
//! Startup is blocking and belongs outside the latency-sensitive worker path.
//! Capacity bounds logical addresses, not reserved physical space: writes can
//! still fail with ENOSPC. Recycling never truncates or erases disk bytes.
//! Parent directories must be trusted against hostile rename and unlink, even
//! though descriptor-relative traversal rejects symlinks and parent components.
use crate::{AlignedBuffer, Alignment, Charge, Error, Extent, Result};
use std::{
    cell::{Cell, RefCell},
    collections::HashMap,
    ffi::CString,
    fs::File,
    future::Future,
    os::{
        fd::{AsRawFd, FromRawFd},
        unix::{ffi::OsStrExt, fs::MetadataExt},
    },
    path::{Component, Path, PathBuf},
    pin::Pin,
    rc::Rc,
    sync::Arc,
    task::{Context, Poll, Waker},
};
use uring_runtime::{
    Budget, Operation, Scope,
    reactor::{Reactor, descriptor::Descriptor},
};

/// One logical segment's range on a caller-opened device.
/// Share one Arc per device within a worker to avoid per-segment descriptors.
#[derive(Clone, Debug)]
pub struct DevicePlacement {
    /// Read/write O_DIRECT file, opened exclusively by the caller for real devices.
    pub file: Arc<File>,

    /// Physical start of this segment, in bytes.
    pub offset: u64,
}

/// Startup resources, before worker-local descriptor creation.
enum Backing {
    File(PathBuf),
    Devices {
        placements: RefCell<Vec<DevicePlacement>>,
        alignment: Alignment,
    },
}

/// Direct-I/O cache storage, not a durable storage transaction.
/// [`Slab::open_volume`] binds it for extent volume use; offset I/O requires
/// that binding, not merely an open file.
#[repr(align(64))]
pub struct Slab<C: Charge> {
    backing: Backing,

    capacity_bytes: u64,

    segment_bytes: u64,

    max_record_bytes: usize,

    opened: RefCell<Option<OpenSlab>>,

    writes: Rc<WriteState>,

    idle_buffer: Rc<RefCell<crate::BufferPool<C>>>,

    resize_mismatched: bool,

    // Per distinct descriptor: completed a write since its last sync.
    dirty: RefCell<Vec<bool>>,
}

/// Fsync descriptors in parallel; the first error wins after all finish.
async fn sync_all<S: Scope, B: Budget>(
    reactor: &Reactor<S, B>,
    files: Vec<Rc<Descriptor>>,
    scope: &S,
) -> std::result::Result<(), S::Error> {
    let mut running: Vec<Option<Operation<'_, (), S::Error>>> = files
        .into_iter()
        .map(|file| Some(reactor.file_sync(file, scope)))
        .collect();
    let mut error = None;
    std::future::poll_fn(|cx| {
        let mut pending = false;
        for slot in &mut running {
            if let Some(op) = slot {
                match op.as_mut().poll(cx) {
                    Poll::Ready(result) => {
                        if let Err(e) = result {
                            error.get_or_insert(e);
                        }
                        *slot = None;
                    }
                    Poll::Pending => pending = true,
                }
            }
        }
        if pending {
            Poll::Pending
        } else {
            Poll::Ready(())
        }
    })
    .await;
    error.map_or(Ok(()), Err)
}

impl<C: Charge> Slab<C> {
    /// Describe a worker's file; validation and blocking I/O happen at startup.
    pub fn new(
        path: PathBuf,
        capacity_bytes: u64,
        segment_bytes: u64,
        max_record_bytes: usize,
    ) -> Self {
        Self {
            backing: Backing::File(path),
            capacity_bytes,
            segment_bytes,
            max_record_bytes,
            opened: RefCell::new(None),
            writes: Rc::new(WriteState::default()),
            idle_buffer: Rc::new(RefCell::new(crate::BufferPool::default())),
            resize_mismatched: false,
            dirty: RefCell::new(Vec::new()),
        }
    }

    /// Accept an existing nonempty file of another size (for example one left
    /// by an older layout) and resize it to the configured capacity, discarding
    /// its contents. The resize happens only after the file is opened through
    /// the validated path and the exclusive lock is held, so a running owner's
    /// file is never changed.
    pub fn resize_mismatched(mut self) -> Self {
        self.resize_mismatched = true;
        self
    }

    /// Describe device ranges in logical segment order, without creating, sizing,
    /// or locking files. Bounds use BLKGETSIZE64 for block devices and file length
    /// for regular direct files. The caller must supply suitable alignment and keep
    /// exclusive ownership, file flags, and capacity unchanged while in use.
    /// Overlap checks only compare the same inode or device identity in this slab.
    /// Whole-disk, partition, and device-mapper aliases are not detected. The caller
    /// must guarantee disjoint physical storage across aliases and different slabs.
    /// Files stay owned until `open_volume` creates worker-local descriptors.
    pub fn from_devices(
        placements: Vec<DevicePlacement>,
        segment_bytes: u64,
        max_record_bytes: usize,
        alignment: Alignment,
    ) -> Result<Self> {
        Self::from_devices_with_capacity(
            placements,
            segment_bytes,
            max_record_bytes,
            alignment,
            |file, metadata| {
                if metadata.mode() & libc::S_IFMT == libc::S_IFBLK {
                    block_device_capacity(file.as_raw_fd())
                } else {
                    Ok(metadata.len())
                }
            },
        )
    }

    /// Keep capacity probing injectable for tests without block-device access.
    fn from_devices_with_capacity(
        placements: Vec<DevicePlacement>,
        segment_bytes: u64,
        max_record_bytes: usize,
        alignment: Alignment,
        mut capacity: impl FnMut(&File, &std::fs::Metadata) -> Result<u64>,
    ) -> Result<Self> {
        let capacity_bytes = (placements.len() as u64)
            .checked_mul(segment_bytes)
            .filter(|&size| size != 0 && size <= i64::MAX as u64)
            .ok_or(Error::InvalidConfiguration)?;
        let mut slab = Self::new(
            PathBuf::new(),
            capacity_bytes,
            segment_bytes,
            max_record_bytes,
        );
        slab.validate_layout(alignment, capacity_bytes)?;
        let mut files = HashMap::new();
        let mut ranges = Vec::with_capacity(placements.len());
        for placement in &placements {
            let end = placement
                .offset
                .checked_add(segment_bytes)
                .filter(|&end| end <= i64::MAX as u64)
                .ok_or(Error::InvalidConfiguration)?;
            if !placement.offset.is_multiple_of(alignment.offset()) {
                return Err(Error::InvalidConfiguration);
            }
            let key = Arc::as_ptr(&placement.file);
            let (metadata, capacity_bytes) = match files.entry(key) {
                std::collections::hash_map::Entry::Occupied(entry) => entry.into_mut(),
                std::collections::hash_map::Entry::Vacant(entry) => {
                    // SAFETY: F_GETFL only inspects the live caller-owned descriptor.
                    let flags = unsafe { libc::fcntl(placement.file.as_raw_fd(), libc::F_GETFL) };
                    if flags < 0 {
                        return Err(system_error("fcntl-getfl", std::io::Error::last_os_error()));
                    }
                    if flags & libc::O_DIRECT == 0
                        || flags & libc::O_ACCMODE != libc::O_RDWR
                        || flags & libc::O_APPEND != 0
                    {
                        return Err(Error::InvalidConfiguration);
                    }
                    let metadata = placement
                        .file
                        .metadata()
                        .map_err(|e| system_error("fstat", e))?;
                    if !matches!(
                        metadata.mode() & libc::S_IFMT,
                        libc::S_IFREG | libc::S_IFBLK
                    ) {
                        return Err(Error::InvalidConfiguration);
                    }
                    let capacity_bytes = capacity(&placement.file, &metadata)?;
                    entry.insert((metadata, capacity_bytes))
                }
            };
            if end > *capacity_bytes {
                return Err(Error::InvalidConfiguration);
            }
            let identity = if metadata.mode() & libc::S_IFMT == libc::S_IFBLK {
                (libc::S_IFBLK, metadata.rdev(), 0)
            } else {
                (libc::S_IFREG, metadata.dev(), metadata.ino())
            };
            ranges.push((identity, placement.offset, end));
        }
        ranges.sort_unstable();
        if ranges
            .windows(2)
            .any(|pair| pair[0].0 == pair[1].0 && pair[0].2 > pair[1].1)
        {
            return Err(Error::InvalidConfiguration);
        }
        slab.backing = Backing::Devices {
            placements: RefCell::new(placements),
            alignment,
        };
        Ok(slab)
    }

    /// Logical capacity, not a reservation of physical disk blocks.
    pub fn capacity_bytes(&self) -> u64 {
        self.capacity_bytes
    }

    /// Size of each physical segment.
    pub fn segment_bytes(&self) -> u64 {
        self.segment_bytes
    }

    /// Accepted writes whose completion guards have not yet been released.
    pub fn writes_in_flight(&self) -> usize {
        self.writes.count.get()
    }

    /// Accounted backing capacity in the bounded idle free lists.
    pub fn idle_bytes(&self) -> usize {
        self.idle_buffer.borrow().bytes()
    }

    /// Release only idle memory, returning its size without touching live I/O.
    pub fn reclaim_idle(&self) -> usize {
        let idle = std::mem::take(&mut self.idle_buffer.borrow_mut().entries);
        idle.iter().map(|b| b.allocation().layout.size()).sum()
    }

    /// Wait for accepted writes to release their runtime completion fences.
    /// This does not call fsync/fdatasync and does NOT promise crash durability.
    /// Drive the reactor concurrently; stop new writes first if quiescence is needed.
    pub fn fence_writes(&self) -> Operation<'_, (), Error> {
        Box::pin(FenceWaiter {
            state: self.writes.clone(),
            registration: None,
        })
    }

    /// Discovered or caller-supplied direct-I/O requirements, or Unavailable before startup.
    pub fn alignment(&self) -> Result<Alignment> {
        self.opened
            .borrow()
            .as_ref()
            .map(|o| o.alignment)
            .ok_or(Error::Unavailable)
    }

    /// Blocking startup I/O for geometry probing and buffer allocation. Offset
    /// I/O returns `Unavailable` until [`Slab::open_volume`] succeeds.
    /// Parent directories must be trusted against
    /// rename/unlink by other users. Linux traversal rejects symlinks and `..`;
    /// newly created directories are private. Existing files must be owned by the
    /// effective user, regular, singly linked, and mode 0600.
    #[cfg(any(test, feature = "simulation"))]
    pub fn open_now(&self) -> Result<Alignment> {
        self.open_file()
    }

    /// Prepare backing descriptors without granting submission authority.
    fn open_file(&self) -> Result<Alignment> {
        self.open_file_with_lock_handoff(|_| {})
    }

    /// Open backing storage with a hook for testing a prior lock holder's changes.
    fn open_file_with_lock_handoff(&self, before_lock: impl FnOnce(&File)) -> Result<Alignment> {
        if let Ok(a) = self.alignment() {
            return Ok(a);
        }
        let path = match &self.backing {
            Backing::File(path) => path,
            Backing::Devices {
                placements,
                alignment,
            } => {
                let mut files = HashMap::new();
                let mut distinct: Vec<Rc<Descriptor>> = Vec::new();
                let mut opened = Vec::with_capacity(placements.borrow().len());
                for placement in placements.borrow().iter() {
                    let index = match files.entry(Arc::as_ptr(&placement.file)) {
                        std::collections::hash_map::Entry::Occupied(entry) => *entry.get(),
                        std::collections::hash_map::Entry::Vacant(entry) => {
                            let file = placement
                                .file
                                .try_clone()
                                .map_err(|e| system_error("dup", e))?;
                            distinct.push(Rc::new(Descriptor::from(file)));
                            *entry.insert(distinct.len() - 1)
                        }
                    };
                    opened.push(OpenPlacement {
                        file: distinct[index].clone(),
                        index,
                        offset: placement.offset,
                    });
                }
                let segment_count = opened.len() as u64;
                *self.dirty.borrow_mut() = vec![false; distinct.len()];
                *self.opened.borrow_mut() = Some(OpenSlab {
                    files: distinct,
                    backing: OpenBacking::Devices(opened),
                    alignment: *alignment,
                    segment_count,
                    volume: None,
                });
                *placements.borrow_mut() = Vec::new();
                return Ok(*alignment);
            }
        };
        if self.segment_bytes == 0
            || self.capacity_bytes == 0
            || self.max_record_bytes == 0
            || !self.capacity_bytes.is_multiple_of(self.segment_bytes)
            || self.capacity_bytes > i64::MAX as u64
        {
            return Err(Error::InvalidConfiguration);
        }
        #[cfg(feature = "simulation")]
        if let Some(sim) = uring_runtime::reactor::simulation::Simulation::current() {
            if let Some(parent) = path.parent().filter(|p| !p.as_os_str().is_empty()) {
                sim.create_dir_all(parent)
                    .map_err(|e| system_error("mkdir", e))?;
            }
            let file = sim
                .open(
                    None,
                    path,
                    libc::O_CREAT
                        | libc::O_RDWR
                        | libc::O_DIRECT
                        | libc::O_CLOEXEC
                        | libc::O_NOFOLLOW,
                )
                .map_err(|e| direct_error("open", e))?;
            let handle = file.as_sim().expect("simulation descriptor");
            handle.lock().map_err(lock_error)?;
            let stat = handle.stat().map_err(|e| system_error("statx", e))?;
            // The virtual filesystem has a fixed root owner, independent of host uid.
            validate_file(stat.stx_mode as u32, stat.stx_uid, 0, stat.stx_nlink as u64)?;
            let a = alignment_from_stat(&stat)?;
            let size = self.accepted_size(path, stat.stx_size);
            let segment_count = self.validate_layout(a, size)?;
            if size == 0 {
                handle
                    .set_len(self.capacity_bytes)
                    .map_err(|e| system_error("ftruncate", e))?;
                // A new file must survive a crash: sync its length and every
                // directory entry leading to it.
                handle.sync().map_err(|e| system_error("fsync", e))?;
                for directory in parent_directories(path) {
                    sim.disk()
                        .sync(&directory)
                        .map_err(|e| system_error("fsync-directory", e))?;
                }
            }
            self.publish(file, a, segment_count);
            return Ok(a);
        }
        let file = open_private_file(path)?;
        let metadata = file.metadata().map_err(|e| system_error("fstat", e))?;
        // SAFETY: geteuid has no arguments or borrowed memory.
        validate_file(
            metadata.mode(),
            metadata.uid(),
            unsafe { libc::geteuid() },
            metadata.nlink(),
        )?;
        before_lock(&file);
        // SAFETY: flock synchronously borrows this live descriptor.
        if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
            return Err(lock_error(std::io::Error::last_os_error()));
        }
        // Recheck metadata under the lock: a prior holder may have changed it.
        let metadata = file.metadata().map_err(|e| system_error("fstat", e))?;
        // SAFETY: geteuid has no arguments or borrowed memory.
        validate_file(
            metadata.mode(),
            metadata.uid(),
            unsafe { libc::geteuid() },
            metadata.nlink(),
        )?;
        let size = metadata.len();
        // Delay O_DIRECT until after rejecting nonregular files (including FIFOs).
        // SAFETY: fcntl synchronously borrows this live descriptor.
        let flags = unsafe { libc::fcntl(file.as_raw_fd(), libc::F_GETFL) };
        if flags < 0 {
            return Err(system_error("fcntl-getfl", std::io::Error::last_os_error()));
        }
        if unsafe {
            libc::fcntl(
                file.as_raw_fd(),
                libc::F_SETFL,
                (flags | libc::O_DIRECT) & !libc::O_NONBLOCK,
            )
        } < 0
        {
            return Err(direct_error(
                "fcntl-direct",
                std::io::Error::last_os_error(),
            ));
        }
        let a = probe(&file)?;
        let size = self.accepted_size(path, size);
        let segment_count = self.validate_layout(a, size)?;
        if size == 0 {
            file.set_len(self.capacity_bytes)
                .map_err(|e| system_error("ftruncate", e))?;
            // A new file must survive a crash: sync its length and every
            // directory entry leading to it.
            file.sync_all().map_err(|e| system_error("fsync", e))?;
            for directory in parent_directories(path) {
                File::open(&directory)
                    .and_then(|d| d.sync_all())
                    .map_err(|e| system_error("fsync-directory", e))?;
            }
        }
        self.publish(file.into(), a, segment_count);
        Ok(a)
    }

    /// Size to validate for a locked file: a mismatched nonempty file accepted
    /// for resizing is treated as new, so it is sized and synced like one.
    fn accepted_size(&self, path: &Path, size: u64) -> u64 {
        if !self.resize_mismatched || size == 0 || size == self.capacity_bytes {
            return size;
        }
        eprintln!(
            "page-alloc: resizing slab {} from {size} to {} bytes; its contents are discarded",
            path.display(),
            self.capacity_bytes
        );
        0
    }

    /// Publish geometry with its owning file, initially without I/O authority.
    fn publish(&self, file: Descriptor, alignment: Alignment, segment_count: u64) {
        let file = Rc::new(file);
        *self.dirty.borrow_mut() = vec![false];
        *self.opened.borrow_mut() = Some(OpenSlab {
            files: vec![file.clone()],
            backing: OpenBacking::File(file),
            alignment,
            segment_count,
            volume: None,
        });
    }

    /// Check physical dimensions and padded record size without changing the
    /// file, returning the physical segment count. Segment boundaries must be
    /// multiples of both direct-I/O units.
    fn validate_layout(&self, a: Alignment, size: u64) -> Result<u64> {
        if self.segment_bytes == 0
            || self.capacity_bytes == 0
            || !self.capacity_bytes.is_multiple_of(self.segment_bytes)
            || a.extent(0, self.max_record_bytes)?.length() as u64 > self.segment_bytes
            || !self.segment_bytes.is_multiple_of(a.offset())
            || !self.segment_bytes.is_multiple_of(a.length() as u64)
            || (size != 0 && size != self.capacity_bytes)
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(self.capacity_bytes / self.segment_bytes)
    }

    /// Blocking startup helper that binds the slab for extent volume use.
    /// Slots are segments; `slot_count` may be at most `volume::MAX_SLOTS` and
    /// at most the physical segment count. The volume uses `volume::SLOT_BYTES`
    /// segments. Read and write go through [`Slab::read_at`] and
    /// [`Slab::write_at`]. Rebinding with a different slot count is rejected.
    pub fn open_volume(&self, slot_count: u64) -> Result<Alignment> {
        if slot_count == 0 || slot_count > crate::volume::MAX_SLOTS {
            return Err(Error::InvalidConfiguration);
        }
        let alignment = self.open_file()?;
        let mut opened = self.opened.borrow_mut();
        let slab = opened.as_mut().ok_or(Error::Unavailable)?;
        if slab.volume.is_some_and(|count| count != slot_count) || slot_count > slab.segment_count {
            return Err(Error::InvalidConfiguration);
        }
        slab.volume = Some(slot_count);
        Ok(alignment)
    }

    /// Map an absolute slab offset to a backing descriptor and physical extent.
    fn volume_submission(
        &self,
        offset: u64,
        buffer: &AlignedBuffer<C>,
    ) -> Result<(Rc<Descriptor>, Extent, usize)> {
        let opened = self.opened.borrow();
        let slab = opened.as_ref().ok_or(Error::Unavailable)?;
        let count = slab.volume.ok_or(Error::Unavailable)?;
        let extent = Extent::new(offset, buffer.len())?;
        slab.alignment.check(extent, buffer)?;
        let segment = offset / self.segment_bytes;
        let start = segment * self.segment_bytes;
        let end = offset
            .checked_add(buffer.len() as u64)
            .ok_or(Error::Corrupt)?;
        if segment >= count || end > start + self.segment_bytes {
            return Err(Error::Corrupt);
        }
        match &slab.backing {
            OpenBacking::File(file) => Ok((file.clone(), extent, 0)),
            OpenBacking::Devices(placements) => {
                let position = usize::try_from(segment).map_err(|_| Error::Corrupt)?;
                let placement = placements.get(position).ok_or(Error::Corrupt)?;
                let physical = Extent::new(
                    placement
                        .offset
                        .checked_add(offset - start)
                        .ok_or(Error::Corrupt)?,
                    buffer.len(),
                )?;
                slab.alignment.check(physical, buffer)?;
                Ok((placement.file.clone(), physical, placement.index))
            }
        }
    }

    /// Check a volume request and bundle it with its guard.
    fn prepare_at<G: 'static>(
        &self,
        offset: u64,
        buffer: AlignedBuffer<C>,
        guard: G,
    ) -> Result<Submission<C, G>> {
        let (file, extent, index) = self.volume_submission(offset, &buffer)?;
        Ok(Submission {
            index,
            file,
            extent,
            buffer,
            lease: guard,
        })
    }

    /// Read `buffer.len()` bytes at an absolute slab offset of a volume-bound
    /// slab. `guard` (for example a slot lease) is held until completion. The
    /// transfer must be aligned and stay inside one segment, or it fails with
    /// `InvalidConfiguration` (alignment) or `Corrupt` (range). Returns
    /// `Unavailable` unless [`Slab::open_volume`] succeeded.
    pub fn read_at<'a, S: Scope, B: Budget, G: 'static>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        offset: u64,
        buffer: AlignedBuffer<C>,
        guard: G,
        scope: &'a S,
    ) -> Operation<'a, AlignedBuffer<C>, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            self.prepare_at(offset, buffer, guard)?
                .read(reactor, scope)
                .await
        })
    }

    /// Write `buffer.len()` bytes at an absolute slab offset of a volume-bound
    /// slab, with the same checks as [`Slab::read_at`]. The write counts toward
    /// [`Slab::fence_writes`]. It is not durable until [`Slab::sync`].
    pub fn write_at<'a, S: Scope, B: Budget, G: 'static>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        offset: u64,
        buffer: AlignedBuffer<C>,
        guard: G,
        scope: &'a S,
    ) -> Operation<'a, AlignedBuffer<C>, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let submission = self.prepare_at(offset, buffer, guard)?;
            let index = submission.index;
            let fence = self.writes.acquire()?;
            let buffer = submission.write(reactor, scope, fence).await?;
            if let Some(dirty) = self.dirty.borrow_mut().get_mut(index) {
                *dirty = true;
            }
            Ok(buffer)
        })
    }

    /// Fsync every distinct backing descriptor, making completed writes
    /// durable. Writes still in flight are not covered; wait for them first.
    /// Returns `Unavailable` before startup.
    pub fn sync<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let files = self.descriptors()?;
            let count = files.len();
            let result = sync_all(reactor, files, scope).await;
            if result.is_ok() {
                let mut dirty = self.dirty.borrow_mut();
                dirty.iter_mut().take(count).for_each(|d| *d = false);
            }
            result
        })
    }

    /// Fsync, in parallel, only the descriptors that completed a write since
    /// their last successful sync. Writes still in flight are not covered.
    /// A failed sync marks its descriptors dirty again.
    pub fn sync_dirty<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let files = self.descriptors()?;
            let taken: Vec<usize> = {
                let mut dirty = self.dirty.borrow_mut();
                let taken = (0..dirty.len()).filter(|&i| dirty[i]).collect();
                dirty.iter_mut().for_each(|d| *d = false);
                taken
            };
            let chosen = taken.iter().map(|&i| files[i].clone()).collect();
            let result = sync_all(reactor, chosen, scope).await;
            if result.is_err() {
                let mut dirty = self.dirty.borrow_mut();
                for i in taken {
                    dirty[i] = true;
                }
            }
            result
        })
    }

    /// Fsync only the descriptor that backs `offset` of a volume-bound slab.
    pub fn sync_at<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        offset: u64,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let file = {
                let opened = self.opened.borrow();
                let slab = opened.as_ref().ok_or(Error::Unavailable)?;
                match &slab.backing {
                    OpenBacking::File(file) => file.clone(),
                    OpenBacking::Devices(placements) => {
                        let segment = usize::try_from(offset / self.segment_bytes)
                            .map_err(|_| Error::Corrupt)?;
                        placements.get(segment).ok_or(Error::Corrupt)?.file.clone()
                    }
                }
            };
            reactor.file_sync(file, scope).await
        })
    }

    /// Number of distinct backing descriptors, cached at open.
    pub fn descriptor_count(&self) -> usize {
        self.opened
            .borrow()
            .as_ref()
            .map_or(0, |slab| slab.files.len())
    }

    /// Distinct backing descriptors, in first-use order, cached at open.
    fn descriptors(&self) -> Result<Vec<Rc<Descriptor>>> {
        let opened = self.opened.borrow();
        let slab = opened.as_ref().ok_or(Error::Unavailable)?;
        Ok(slab.files.clone())
    }

    /// Reuse charged capacity in the same size class without discarding mismatches.
    pub fn allocate(&self, length: usize, charge: C) -> Result<AlignedBuffer<C>> {
        let alignment = self.alignment()?;
        if length == 0
            || length > Alignment::MAX_TRANSFER_LENGTH
            || !length.is_multiple_of(alignment.length())
            || !charge.covers(length)
        {
            return Err(Error::InvalidConfiguration);
        }
        let idle = self.idle_buffer.borrow_mut().take_for(length, &charge);
        if let Some(mut buffer) = idle {
            buffer.rebind(charge)?;
            buffer.length = length;
            return Ok(buffer.pooled(&self.idle_buffer));
        }
        let capacity = crate::buffer_capacity(length)
            .filter(|&n| {
                length >= crate::HUGE_PAGE_BYTES
                    && n <= Alignment::MAX_TRANSFER_LENGTH
                    && n.is_multiple_of(alignment.length())
                    && charge.covers(n)
            })
            .unwrap_or(length);
        let mut buffer = alignment.allocate(capacity, charge)?;
        buffer.length = length;
        Ok(buffer.pooled(&self.idle_buffer))
    }

    /// Replace file-mode backing for fault tests; device mode is rejected.
    #[cfg(feature = "simulation")]
    #[doc(hidden)]
    pub fn replace_file_for_test(&self, file: File) -> Result<()> {
        self.replace_descriptor_for_test(file.into())
    }

    /// Accept a virtual descriptor for file-mode fault tests; geometry stays unchanged.
    #[cfg(feature = "simulation")]
    #[doc(hidden)]
    pub fn replace_descriptor_for_test(&self, file: Descriptor) -> Result<()> {
        if self.writes_in_flight() != 0 {
            return Err(Error::Busy);
        }
        let mut opened = self.opened.borrow_mut();
        let slab = opened.as_mut().ok_or(Error::Unavailable)?;
        let OpenBacking::File(current) = &mut slab.backing else {
            return Err(Error::InvalidConfiguration);
        };
        let file = Rc::new(file);
        *current = file.clone();
        slab.files = vec![file];
        Ok(())
    }
}

/// Backing and geometry own their binding; a closed slab cannot retain authority.
struct OpenSlab {
    backing: OpenBacking,

    // Distinct descriptors in first-use order, computed once at open.
    files: Vec<Rc<Descriptor>>,

    alignment: Alignment,

    // Physical segment count, possibly more than the bound slot count.
    segment_count: u64,

    // Slot count once bound for volume use.
    volume: Option<u64>,
}

/// Runtime owners shared by all submissions to a backing file.
enum OpenBacking {
    File(Rc<Descriptor>),
    Devices(Vec<OpenPlacement>),
}

/// One logical segment's worker-local destination.
struct OpenPlacement {
    file: Rc<Descriptor>,
    // Position of `file` in `OpenSlab::files`.
    index: usize,
    offset: u64,
}

/// A validated transfer owns every resource needed to keep kernel access safe.
/// Only Slab::prepare_at constructs this capability, and submission consumes it.
struct Submission<C: Charge, G: 'static> {
    // Position of `file` in `OpenSlab::files`.
    index: usize,

    file: Rc<Descriptor>,

    extent: Extent,

    buffer: AlignedBuffer<C>,

    lease: G,
}

impl<C: Charge, G: 'static> Submission<C, G> {
    /// Move the complete read capability into the reactor's completion ownership.
    async fn read<S: Scope, B: Budget>(
        self,
        reactor: &Reactor<S, B>,
        scope: &S,
    ) -> std::result::Result<AlignedBuffer<C>, S::Error>
    where
        S::Error: From<Error>,
    {
        let completion = reactor
            .read_at(
                self.file,
                self.extent.offset(),
                self.buffer,
                self.lease,
                scope,
            )
            .await?;
        Self::complete(self.extent, completion.bytes, completion.buffer)
    }

    /// Move the write capability and its counter guard into completion ownership.
    async fn write<S: Scope, B: Budget>(
        self,
        reactor: &Reactor<S, B>,
        scope: &S,
        fence: WriteFence,
    ) -> std::result::Result<AlignedBuffer<C>, S::Error>
    where
        S::Error: From<Error>,
    {
        let completion = reactor
            .write_at(
                self.file,
                self.extent.offset(),
                self.buffer,
                (self.lease, fence),
                scope,
            )
            .await?;
        Self::complete(self.extent, completion.bytes, completion.buffer)
    }

    /// Return storage only after an exact-length completion; short I/O drops it.
    fn complete<E: From<Error>>(
        extent: Extent,
        bytes: usize,
        buffer: AlignedBuffer<C>,
    ) -> std::result::Result<AlignedBuffer<C>, E> {
        if bytes != extent.length() {
            return Err(Error::Io.into());
        }
        Ok(buffer)
    }
}

/// Worker-local write counters and fence registrations, independent of slab lifetime.
#[derive(Default)]
struct WriteState {
    count: Cell<usize>,

    waiters: RefCell<Vec<Rc<RefCell<Waker>>>>,
}

impl WriteState {
    /// Increment before constructing the sole guard responsible for decrementing.
    fn acquire(self: &Rc<Self>) -> Result<WriteFence> {
        self.count
            .set(self.count.get().checked_add(1).ok_or(Error::Busy)?);
        Ok(WriteFence(self.clone()))
    }
}

/// Cancel-safe registration waiting for all currently accepted writes to finish.
struct FenceWaiter {
    state: Rc<WriteState>,

    registration: Option<Rc<RefCell<Waker>>>,
}

impl Future for FenceWaiter {
    type Output = Result<()>;

    /// Refresh the task's registration without busy-waking the worker.
    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        if self.state.count.get() == 0 {
            self.unregister();
            return Poll::Ready(Ok(()));
        }
        if let Some(registration) = &self.registration {
            registration.borrow_mut().clone_from(cx.waker());
            // Final completion drained registrations; a new write can precede
            // our next poll, in which case this waiter must register again.
            let mut waiters = self.state.waiters.borrow_mut();
            if !waiters.iter().any(|w| Rc::ptr_eq(w, registration)) {
                waiters.push(registration.clone());
            }
        } else {
            let registration = Rc::new(RefCell::new(cx.waker().clone()));
            self.state.waiters.borrow_mut().push(registration.clone());
            self.registration = Some(registration);
        }
        Poll::Pending
    }
}

impl FenceWaiter {
    /// Remove only this waiter's registration, including after cancellation.
    fn unregister(&mut self) {
        if let Some(registration) = self.registration.take() {
            self.state
                .waiters
                .borrow_mut()
                .retain(|w| !Rc::ptr_eq(w, &registration));
        }
    }
}

impl Drop for FenceWaiter {
    /// A canceled wait must not retain its task's waker.
    fn drop(&mut self) {
        self.unregister();
    }
}

/// Unique write-count decrement authority retained by the reactor completion.
struct WriteFence(Rc<WriteState>);

impl Drop for WriteFence {
    /// Release the count and wake sleepers outside all registration borrows.
    fn drop(&mut self) {
        let remaining = self.0.count.get() - 1;
        self.0.count.set(remaining);
        if remaining == 0 {
            let waiters = std::mem::take(&mut *self.0.waiters.borrow_mut());
            for waiter in waiters {
                let waker = waiter.borrow().clone();
                waker.wake();
            }
        }
    }
}

/// Read the block device's byte capacity without changing its contents.
fn block_device_capacity(fd: i32) -> Result<u64> {
    let mut capacity = 0u64;
    // Linux fs.h encodes BLKGETSIZE64 with size_t, but the output is always u64.
    let request = libc::_IOR::<libc::size_t>(0x12, 114);
    // SAFETY: BLKGETSIZE64 writes one u64 to this live output pointer. Invalid
    // descriptors are rejected by the kernel without accessing device storage.
    if unsafe { libc::ioctl(fd, request, &mut capacity) } != 0 {
        return Err(system_error(
            "ioctl-blkgetsize64",
            std::io::Error::last_os_error(),
        ));
    }
    Ok(capacity)
}

/// Preserve synchronous operating-system diagnostic context.
fn system_error(operation: &'static str, error: std::io::Error) -> Error {
    Error::SystemIo {
        operation,
        errno: error.raw_os_error(),
    }
}

/// Distinguish an already locked file from a failed locking syscall.
fn lock_error(error: std::io::Error) -> Error {
    if error.raw_os_error() == Some(libc::EWOULDBLOCK) {
        Error::Unavailable
    } else {
        system_error("flock", error)
    }
}

/// Classify explicit direct-I/O capability denials without hiding other failures.
fn direct_error(operation: &'static str, error: std::io::Error) -> Error {
    match error.raw_os_error() {
        Some(libc::EINVAL | libc::EOPNOTSUPP | libc::ENOSYS) => Error::Unsupported,
        _ => system_error(operation, error),
    }
}

/// Discover direct-I/O requirements for an owned file descriptor.
fn probe(file: &File) -> Result<Alignment> {
    probe_fd(file.as_raw_fd())
}

/// Ask Linux for descriptor-specific alignment without assuming a page size.
fn probe_fd(fd: i32) -> Result<Alignment> {
    // SAFETY: initialized statx output and valid empty C path. Invalid FDs are
    // rejected by the kernel without accessing user memory through the FD.
    let mut stat: libc::statx = unsafe { std::mem::zeroed() };
    if unsafe {
        libc::statx(
            fd,
            c"".as_ptr(),
            libc::AT_EMPTY_PATH,
            libc::STATX_DIOALIGN,
            &mut stat,
        )
    } != 0
    {
        let error = std::io::Error::last_os_error();
        return Err(match error.raw_os_error() {
            Some(libc::ENOSYS | libc::EOPNOTSUPP) => Error::Unsupported,
            _ => system_error("statx", error),
        });
    }
    alignment_from_stat(&stat)
}

/// Reject missing alignment capability or invalid values returned by statx.
fn alignment_from_stat(stat: &libc::statx) -> Result<Alignment> {
    if stat.stx_mask & libc::STATX_DIOALIGN == 0 {
        return Err(Error::Unsupported);
    }
    Alignment::new(
        stat.stx_dio_mem_align as usize,
        stat.stx_dio_offset_align as u64,
        stat.stx_dio_offset_align as usize,
    )
}

/// Require a private regular file with one link and the expected owner.
fn validate_file(mode: u32, owner: u32, expected_owner: u32, links: u64) -> Result<()> {
    if mode & libc::S_IFMT != libc::S_IFREG
        || mode & 0o7777 != 0o600
        || owner != expected_owner
        || links != 1
    {
        return Err(Error::InvalidConfiguration);
    }
    Ok(())
}

/// Resolve each directory relative to its already-open predecessor. Unlike
/// create_dir_all plus open, this never follows an intermediate symlink.
/// Directories from the parent of `path` up to the root (or `.`), whose
/// entries must be synced after creating `path`.
fn parent_directories(path: &Path) -> Vec<PathBuf> {
    path.ancestors()
        .skip(1)
        .map(|p| {
            if p.as_os_str().is_empty() {
                PathBuf::from(".")
            } else {
                p.to_path_buf()
            }
        })
        .collect()
}

fn open_private_file(path: &Path) -> Result<File> {
    let name = path.file_name().ok_or(Error::InvalidConfiguration)?;
    if path.components().any(|c| matches!(c, Component::ParentDir)) {
        return Err(Error::InvalidConfiguration);
    }
    let anchor = if path.is_absolute() { c"/" } else { c"." };
    // SAFETY: valid C path, no borrowed memory retained by open.
    let fd = unsafe {
        libc::open(
            anchor.as_ptr(),
            libc::O_PATH | libc::O_DIRECTORY | libc::O_CLOEXEC,
        )
    };
    if fd < 0 {
        return Err(system_error("open-parent", std::io::Error::last_os_error()));
    }
    // SAFETY: open returned a new, uniquely owned descriptor.
    let mut parent = unsafe { File::from_raw_fd(fd) };
    for component in path.parent().unwrap_or(Path::new("")).components() {
        let Component::Normal(component) = component else {
            continue;
        };
        let component =
            CString::new(component.as_bytes()).map_err(|_| Error::InvalidConfiguration)?;
        // SAFETY: parent FD and C component remain valid throughout each syscall.
        let flags = libc::O_PATH | libc::O_DIRECTORY | libc::O_CLOEXEC | libc::O_NOFOLLOW;
        let mut next = unsafe { libc::openat(parent.as_raw_fd(), component.as_ptr(), flags) };
        if next < 0 && std::io::Error::last_os_error().raw_os_error() == Some(libc::ENOENT) {
            if unsafe { libc::mkdirat(parent.as_raw_fd(), component.as_ptr(), 0o700) } != 0
                && std::io::Error::last_os_error().raw_os_error() != Some(libc::EEXIST)
            {
                return Err(system_error("mkdirat", std::io::Error::last_os_error()));
            }
            next = unsafe { libc::openat(parent.as_raw_fd(), component.as_ptr(), flags) };
        }
        if next < 0 {
            return Err(system_error("open-parent", std::io::Error::last_os_error()));
        }
        // SAFETY: successful openat returned a uniquely owned descriptor.
        parent = unsafe { File::from_raw_fd(next) };
    }
    let name = CString::new(name.as_bytes()).map_err(|_| Error::InvalidConfiguration)?;
    // O_NONBLOCK prevents a malicious FIFO from hanging startup before fstat.
    // SAFETY: live directory FD and NUL-terminated name; mode supplied for O_CREAT.
    let fd = unsafe {
        libc::openat(
            parent.as_raw_fd(),
            name.as_ptr(),
            libc::O_RDWR | libc::O_CREAT | libc::O_CLOEXEC | libc::O_NOFOLLOW | libc::O_NONBLOCK,
            0o600,
        )
    };
    if fd < 0 {
        return Err(system_error("open", std::io::Error::last_os_error()));
    }
    // SAFETY: successful openat returned a uniquely owned descriptor.
    Ok(unsafe { File::from_raw_fd(fd) })
}

/// Private storage invariants and synchronous Linux file validation.
#[cfg(test)]
mod tests {
    use super::*;

    /// Counts retained bytes independently of buffer pooling.
    struct CountingCharge {
        used: Rc<Cell<usize>>,

        bytes: usize,
    }

    impl CountingCharge {
        /// Admit a fixed number of bytes.
        fn new(used: &Rc<Cell<usize>>, bytes: usize) -> Self {
            used.set(used.get() + bytes);
            Self {
                used: used.clone(),
                bytes,
            }
        }
    }

    impl Charge for CountingCharge {
        /// Cover only bytes actually admitted by this guard.
        fn covers(&self, bytes: usize) -> bool {
            self.bytes >= bytes
        }
    }

    impl Drop for CountingCharge {
        /// Release accounting exactly once on destruction.
        fn drop(&mut self) {
            self.used.set(self.used.get() - self.bytes);
        }
    }

    /// Owns and cleans up an isolated directory inside the project build tree.
    struct Directory(PathBuf);

    impl Directory {
        /// Create a per-process, per-test directory without touching host temp paths.
        fn new() -> Self {
            static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
            let id = NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("page-alloc-test-{}-{id}", std::process::id()));
            std::fs::create_dir_all(&path).unwrap();
            Self(path)
        }
    }

    impl Drop for Directory {
        /// Remove only this test's owned directory.
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    /// Skip only explicit unsupported-filesystem results unless real I/O is required.
    fn real_alignment(result: Result<Alignment>) -> Option<Alignment> {
        match result {
            Ok(alignment) => Some(alignment),
            Err(Error::Unsupported) => {
                assert_ne!(
                    std::env::var("PAGE_ALLOC_REQUIRE_REAL_IO").as_deref(),
                    Ok("1"),
                    "PAGE_ALLOC_REQUIRE_REAL_IO=1 forbids capability skips: filesystem does not support direct I/O geometry"
                );
                eprintln!("SKIP real slab test: filesystem does not support direct I/O geometry");
                None
            }
            Err(error) => panic!("unexpected real slab startup failure: {error}"),
        }
    }

    /// Geometry obeys the discovered units, not assumed memory-page dimensions.
    #[test]
    fn geometry_rounds_without_assuming_page_size() {
        let a = Alignment::new(512, 512, 1024).unwrap();
        assert_eq!(a.extent(512, 1025).unwrap().length(), 2048);
        assert!(a.extent(1, 1).is_err());
        assert!(a.extent(0, usize::MAX).is_err());
        assert!(Alignment::new(3, 512, 512).is_err());
        assert!(Extent::new(u64::MAX, 1).is_err());
        assert!(Extent::new(0, 0).is_err());
        assert!(Alignment::new(512, 0, 512).is_err());
        assert!(Alignment::new(512, 512, 0).is_err());
        let a = Alignment::new(512, 768, 512).unwrap();
        assert_eq!(a.extent(0, 513).unwrap().length(), 1536);
        let b = a.allocate(512, ()).unwrap();
        assert!(!b.is_empty());
        assert_eq!(
            a.check(Extent::new(1, 512).unwrap(), &b),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(
            a.check(Extent::new(0, 1024).unwrap(), &b),
            Err(Error::InvalidConfiguration)
        );
    }

    /// Open direct files without using the slab's private-file startup path.
    fn device_file(path: &Path) -> Arc<File> {
        use std::os::unix::fs::OpenOptionsExt;
        let file = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .custom_flags(libc::O_DIRECT)
            .open(path)
            .unwrap();
        file.set_len(16384).unwrap();
        Arc::new(file)
    }

    /// Queried capacities bound every placement, including cached-file placements.
    #[test]
    fn device_capacity_bounds_are_checked_before_startup() {
        let directory = Directory::new();
        let file = device_file(&directory.0.join("capacity"));
        let a = Alignment::new(512, 512, 512).unwrap();
        for (capacity, offsets, valid) in [
            (0, vec![0], false),
            (4095, vec![0], false),
            (4096, vec![0], true),
            (8191, vec![4096], false),
            (8192, vec![4096], true),
            (8192, vec![8192], false),
            (8192, vec![0, 4096], true),
            (8191, vec![0, 4096], false),
            (u64::MAX, vec![0, 4096], true),
        ] {
            let mut queries = 0;
            let result = Slab::<()>::from_devices_with_capacity(
                offsets
                    .into_iter()
                    .map(|offset| DevicePlacement {
                        file: file.clone(),
                        offset,
                    })
                    .collect(),
                4096,
                512,
                a,
                |_, _| {
                    queries += 1;
                    Ok(capacity)
                },
            );
            if valid {
                assert!(result.is_ok(), "capacity {capacity}: {:?}", result.err());
            } else {
                assert!(matches!(result, Err(Error::InvalidConfiguration)));
            }
            assert_eq!(queries, 1);
            assert_eq!(file.metadata().unwrap().len(), 16384);
        }
    }

    /// Failed capacity queries preserve syscall context rather than allowing a slab.
    #[test]
    fn device_capacity_errors_keep_errno() {
        assert_eq!(
            block_device_capacity(-1),
            Err(Error::SystemIo {
                operation: "ioctl-blkgetsize64",
                errno: Some(libc::EBADF),
            })
        );
        let directory = Directory::new();
        let file = device_file(&directory.0.join("capacity-error"));
        assert_eq!(
            block_device_capacity(file.as_raw_fd()),
            Err(Error::SystemIo {
                operation: "ioctl-blkgetsize64",
                errno: Some(libc::ENOTTY),
            })
        );
        for errno in [libc::EIO, libc::EACCES, libc::ENOTTY] {
            let error = system_error(
                "ioctl-blkgetsize64",
                std::io::Error::from_raw_os_error(errno),
            );
            let result = Slab::<()>::from_devices_with_capacity(
                vec![DevicePlacement {
                    file: file.clone(),
                    offset: 0,
                }],
                4096,
                512,
                Alignment::new(512, 512, 512).unwrap(),
                |_, _| Err(error),
            );
            assert!(matches!(result, Err(actual) if actual == error));
        }
    }

    /// Device layouts reject same-identity overlaps, misalignment, overflow, and invalid flags.
    #[test]
    fn device_layout_validation_is_read_only() {
        use std::os::unix::fs::OpenOptionsExt;
        let directory = Directory::new();
        let file = device_file(&directory.0.join("device"));
        let Some(host_alignment) = real_alignment(probe(&file)) else {
            return;
        };
        for a in [
            host_alignment,
            Alignment::new(4096, 4096, 4096).unwrap(),
            Alignment::new(8192, 8192, 8192).unwrap(),
            Alignment::new(4096, 768, 512).unwrap(),
        ] {
            let unit = a.extent(0, 1).unwrap().length();
            let segment_bytes = 2 * unit as u64;
            let file_bytes = 4 * segment_bytes;
            file.set_len(file_bytes).unwrap();
            let build = |offsets: &[u64], segment, record| {
                Slab::<()>::from_devices(
                    offsets
                        .iter()
                        .map(|&offset| DevicePlacement {
                            file: file.clone(),
                            offset,
                        })
                        .collect(),
                    segment,
                    record,
                    a,
                )
            };
            for (offsets, segment, record) in [
                (vec![], segment_bytes, unit),
                (vec![0], 0, unit),
                (vec![0], segment_bytes, 0),
                (vec![0], segment_bytes + 1, unit),
                (vec![0], segment_bytes, segment_bytes as usize + 1),
                (vec![0, segment_bytes], u64::MAX, unit),
                (vec![0], i64::MAX as u64 + 1, unit),
                (vec![1], segment_bytes, unit),
                (vec![file_bytes], segment_bytes, unit),
                (vec![u64::MAX], segment_bytes, unit),
                (
                    vec![i64::MAX as u64 - segment_bytes + 1],
                    segment_bytes,
                    unit,
                ),
            ] {
                assert!(matches!(
                    build(&offsets, segment, record),
                    Err(Error::InvalidConfiguration)
                ));
            }
            let duplicate = Arc::new(file.try_clone().unwrap());
            assert!(!Arc::ptr_eq(&file, &duplicate));
            for second in [file.clone(), duplicate] {
                let placement = |offset| DevicePlacement {
                    file: second.clone(),
                    offset,
                };
                for offset in [0, unit as u64, segment_bytes] {
                    assert!(
                        Slab::<()>::from_devices(vec![placement(offset)], segment_bytes, unit, a)
                            .is_ok()
                    );
                }
                assert!(
                    Slab::<()>::from_devices(
                        vec![
                            DevicePlacement {
                                file: file.clone(),
                                offset: 0
                            },
                            placement(segment_bytes)
                        ],
                        segment_bytes,
                        unit,
                        a
                    )
                    .is_ok()
                );
                for offset in [0, unit as u64] {
                    assert!(matches!(
                        Slab::<()>::from_devices(
                            vec![
                                DevicePlacement {
                                    file: file.clone(),
                                    offset: 0
                                },
                                placement(offset),
                            ],
                            segment_bytes,
                            unit,
                            a
                        ),
                        Err(Error::InvalidConfiguration)
                    ));
                }
            }
            for flags in [0, libc::O_DIRECT | libc::O_APPEND] {
                let invalid = std::fs::OpenOptions::new()
                    .read(true)
                    .write(true)
                    .custom_flags(flags)
                    .open(directory.0.join("device"))
                    .unwrap();
                assert!(matches!(
                    Slab::<()>::from_devices(
                        vec![DevicePlacement {
                            file: Arc::new(invalid),
                            offset: 0,
                        }],
                        segment_bytes,
                        unit,
                        a
                    ),
                    Err(Error::InvalidConfiguration)
                ));
            }
            let readonly = std::fs::OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_DIRECT)
                .open(directory.0.join("device"))
                .unwrap();
            assert!(matches!(
                Slab::<()>::from_devices(
                    vec![DevicePlacement {
                        file: Arc::new(readonly),
                        offset: 0,
                    }],
                    segment_bytes,
                    unit,
                    a
                ),
                Err(Error::InvalidConfiguration)
            ));
            let slab = build(&[2 * segment_bytes, segment_bytes], segment_bytes, unit).unwrap();
            assert_eq!(slab.capacity_bytes(), 2 * segment_bytes);
            assert_eq!(slab.open_volume(3), Err(Error::InvalidConfiguration));
            assert_eq!(slab.open_volume(2), Ok(a));
            assert_eq!(slab.open_volume(2), Ok(a));
            assert_eq!(file.metadata().unwrap().len(), file_bytes);
            let independent = File::open(directory.0.join("device")).unwrap();
            // SAFETY: this live independent descriptor tests that startup took no flock.
            assert_eq!(
                unsafe { libc::flock(independent.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
                0
            );
            let opened = slab.opened.borrow();
            let OpenBacking::Devices(placements) = &opened.as_ref().unwrap().backing else {
                unreachable!()
            };
            assert!(Rc::ptr_eq(&placements[0].file, &placements[1].file));
            let Backing::Devices { placements, .. } = &slab.backing else {
                unreachable!()
            };
            assert!(placements.borrow().is_empty());
        }
    }

    /// Translation maps logical offsets to placements and rechecks physical bounds.
    #[test]
    fn device_volume_submission_checks_logical_and_physical_extents() {
        let directory = Directory::new();
        let file = device_file(&directory.0.join("device"));
        let Some(a) = real_alignment(probe(&file)) else {
            return;
        };
        let slab = Slab::<()>::from_devices(
            vec![
                DevicePlacement {
                    file: file.clone(),
                    offset: 8192,
                },
                DevicePlacement { file, offset: 0 },
            ],
            4096,
            4096,
            a,
        )
        .unwrap();
        let _ = slab.open_volume(2).unwrap();
        let buffer = slab.allocate(4096, ()).unwrap();
        let (_, physical, _) = slab.volume_submission(4096, &buffer).unwrap();
        assert_eq!(physical, Extent::new(0, 4096).unwrap());
        let (_, physical, _) = slab.volume_submission(0, &buffer).unwrap();
        assert_eq!(physical, Extent::new(8192, 4096).unwrap());
        for (offset, expected) in [
            (8192, Error::Corrupt),
            (6144, Error::Corrupt),
            (4097, Error::InvalidConfiguration),
        ] {
            assert!(
                matches!(slab.volume_submission(offset, &buffer), Err(error) if error == expected),
                "{offset}"
            );
        }
        for (placement, expected) in [
            (1, Error::InvalidConfiguration),
            (u64::MAX - 511, Error::Corrupt),
        ] {
            let mut opened = slab.opened.borrow_mut();
            let OpenBacking::Devices(placements) = &mut opened.as_mut().unwrap().backing else {
                unreachable!()
            };
            placements[1].offset = placement;
            drop(opened);
            assert!(
                matches!(slab.volume_submission(4096, &buffer), Err(error) if error == expected)
            );
        }
    }

    /// Device submissions keep completion error handling and release their fences.
    #[cfg(feature = "simulation")]
    #[test]
    fn device_io_faults_release_completion_resources() {
        use uring_runtime::reactor::simulation::{Fault, Simulation};

        #[derive(Clone, Copy, Debug, PartialEq)]
        enum TestError {
            Alloc(Error),
            Runtime(uring_runtime::Error),
        }
        impl From<Error> for TestError {
            fn from(error: Error) -> Self {
                Self::Alloc(error)
            }
        }
        impl From<uring_runtime::Error> for TestError {
            fn from(error: uring_runtime::Error) -> Self {
                Self::Runtime(error)
            }
        }
        #[derive(Clone)]
        struct TestScope;
        impl Scope for TestScope {
            type Error = TestError;
            fn check(&self) -> std::result::Result<(), TestError> {
                Ok(())
            }
        }
        let directory = Directory::new();
        let file = device_file(&directory.0.join("device"));
        let Some(a) = real_alignment(probe(&file)) else {
            return;
        };
        let slab =
            Slab::<()>::from_devices(vec![DevicePlacement { file, offset: 4096 }], 4096, 4096, a)
                .unwrap();
        let _ = slab.open_volume(1).unwrap();
        let sim = Simulation::new();
        let _environment = sim.enter();
        let descriptor = sim
            .open(
                None,
                Path::new("/device-faults"),
                libc::O_CREAT | libc::O_RDWR,
            )
            .unwrap();
        descriptor.as_sim().unwrap().set_len(8192).unwrap();
        let mut opened = slab.opened.borrow_mut();
        let OpenBacking::Devices(placements) = &mut opened.as_mut().unwrap().backing else {
            unreachable!()
        };
        placements[0].file = Rc::new(descriptor);
        drop(opened);
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        for write in [false, true] {
            for (fault, expected) in [
                (Fault::Short(512), TestError::Alloc(Error::Io)),
                (
                    Fault::Errno(libc::EIO),
                    TestError::Runtime(uring_runtime::Error::Os(libc::EIO)),
                ),
            ] {
                sim.inject(if write { "write" } else { "read" }, fault)
                    .unwrap();
                let guard = Rc::new(());
                let buffer = slab.allocate(4096, ()).unwrap();
                let mut operation = if write {
                    slab.write_at(&reactor, 0, buffer, guard.clone(), &TestScope)
                } else {
                    slab.read_at(&reactor, 0, buffer, guard.clone(), &TestScope)
                };
                let mut completed = false;
                for _ in 0..100 {
                    if let Poll::Ready(result) = operation
                        .as_mut()
                        .poll(&mut Context::from_waker(Waker::noop()))
                    {
                        assert_eq!(result.unwrap_err(), expected);
                        completed = true;
                        break;
                    }
                    reactor.poll_budgeted(64).unwrap();
                }
                assert!(completed);
                drop(operation);
                assert_eq!(slab.writes_in_flight(), 0);
                assert_eq!(reactor.in_flight(), 0);
                assert_eq!(Rc::strong_count(&guard), 1);
            }
        }
    }

    /// The live descriptor is direct, sparse, exclusively locked, and aligned.
    #[test]
    fn real_file_is_direct_aligned_and_sparse_without_reactor() {
        let directory = Directory::new();
        let path = directory.0.join("caller-chosen.dat");
        let make = || Slab::<()>::new(path.clone(), 64 * 1024 * 1024, 32 * 1024 * 1024, 1024);
        let conflicting = make();
        let slabs = make();
        assert!(!path.exists());
        let Some(alignment) = real_alignment(slabs.open_now()) else {
            return;
        };
        let stat = std::fs::metadata(&path).unwrap();
        assert_eq!(stat.len(), slabs.capacity_bytes());
        assert!(stat.blocks() * 512 < stat.len());
        let opened = slabs.opened.borrow();
        let OpenBacking::File(file) = &opened.as_ref().unwrap().backing else {
            unreachable!()
        };
        let fd = file.as_raw_fd();
        // SAFETY: descriptor and buffers remain live throughout these synchronous calls.
        assert_ne!(
            unsafe { libc::fcntl(fd, libc::F_GETFL) } & libc::O_DIRECT,
            0
        );
        let extent = alignment.extent(0, 31).unwrap();
        let mut buffer = slabs.allocate(extent.length(), ()).unwrap();
        buffer.bytes_mut().unwrap()[..31].fill(42);
        assert_eq!(
            unsafe { libc::pwrite(fd, buffer.bytes().unwrap().as_ptr().cast(), buffer.len(), 0) },
            buffer.len() as isize
        );
        buffer.bytes_mut().unwrap().fill(0);
        assert_eq!(
            unsafe {
                libc::pread(
                    fd,
                    buffer.bytes_mut().unwrap().as_mut_ptr().cast(),
                    extent.length(),
                    0,
                )
            },
            extent.length() as isize
        );
        assert_eq!(&buffer.bytes().unwrap()[..31], &[42; 31]);
        assert!(buffer.bytes().unwrap()[31..].iter().all(|b| *b == 0));
        // statx(2) allows misaligned direct I/O to either fail with EINVAL or fall back to
        // buffered I/O, so only a failure's errno is part of the contract.
        // SAFETY: the descriptor and buffer remain live throughout this synchronous call.
        let misaligned =
            unsafe { libc::pwrite(fd, buffer.bytes().unwrap().as_ptr().cast(), 31, 1) };
        if misaligned < 0 {
            assert_eq!(
                std::io::Error::last_os_error().raw_os_error(),
                Some(libc::EINVAL)
            );
        } else {
            assert_eq!(misaligned, 31);
        }
        drop(buffer);
        assert_eq!(slabs.idle_bytes(), extent.length());
        assert_eq!(slabs.reclaim_idle(), extent.length());
        assert_eq!(slabs.idle_bytes(), 0);
        assert_eq!(conflicting.open_now(), Err(Error::Unavailable));
    }

    /// Invalid arithmetic inputs fail without creating a directory or a file.
    #[test]
    fn open_volume_rejects_invalid_geometry_before_open() {
        let directory = Directory::new();
        for (capacity, segment, record) in [
            (0, 0, 512),
            (4096, 0, 512),
            (0, 4096, 512),
            (4097, 4096, 512),
            (4096, 8192, 512),
            (4096, 4096, 0),
            (u64::MAX, 1, 512),
            (u64::MAX, u64::MAX, 512),
        ] {
            let path = directory.0.join("missing/data");
            let slab = Slab::<()>::new(path.clone(), capacity, segment, record);
            assert_eq!(slab.open_volume(1), Err(Error::InvalidConfiguration));
            assert_eq!(slab.alignment(), Err(Error::Unavailable));
            assert!(!path.exists());
            assert!(!path.parent().unwrap().exists());
        }
    }

    /// Invalid startup dimensions never truncate a preexisting nonempty file.
    #[test]
    fn open_rejects_bad_layout_and_existing_size_without_truncating() {
        use std::os::unix::fs::PermissionsExt;
        let directory = Directory::new();
        let probe = Slab::<()>::new(directory.0.join("capability-probe"), 4096, 4096, 512);
        if real_alignment(probe.open_now()).is_none() {
            return;
        }
        let path = directory.0.join("data");
        std::fs::write(&path, [42; 7]).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        let slab = Slab::<()>::new(path.clone(), 4096, 4096, 512);
        assert_eq!(slab.open_now(), Err(Error::InvalidConfiguration));
        assert_eq!(std::fs::read(&path).unwrap(), [42; 7]);
        for (capacity, segment, record) in [
            (0, 4096, 512),
            (4096, 0, 512),
            (4096, 4096, 0),
            (4097, 4096, 512),
            (4096, 4096, 4097),
        ] {
            assert_eq!(
                Slab::<()>::new(directory.0.join("invalid"), capacity, segment, record).open_now(),
                Err(Error::InvalidConfiguration)
            );
        }
        assert_eq!(slab.alignment(), Err(Error::Unavailable));
        assert!(matches!(slab.allocate(512, ()), Err(Error::Unavailable)));
    }

    /// Private validation rejects wrong ranges before any runtime admission.
    #[test]
    fn volume_submission_checks_alignment_segment_and_capacity() {
        let directory = Directory::new();
        let slab = Slab::<()>::new(directory.0.join("data"), 8192, 4096, 512);
        if real_alignment(slab.open_volume(2)).is_none() {
            return;
        }
        let buffer = slab.allocate(4096, ()).unwrap();
        assert!(slab.volume_submission(0, &buffer).is_ok());
        assert!(slab.volume_submission(4096, &buffer).is_ok());
        assert!(matches!(
            slab.volume_submission(8192, &buffer),
            Err(Error::Corrupt)
        ));
        assert!(matches!(
            slab.volume_submission(1, &buffer),
            Err(Error::InvalidConfiguration)
        ));
    }

    /// Waiters sleep, replace wakers, unregister on cancellation, and register again.
    #[test]
    fn fence_waiters_sleep_update_wakers_and_unregister_on_cancellation() {
        use std::sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        };
        use std::task::Wake;
        /// Counts wake notifications without scheduling actual work.
        #[derive(Default)]
        struct WakeCount(AtomicUsize);

        impl Wake for WakeCount {
            /// Count an owned notification.
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }

            /// Count a borrowed notification.
            fn wake_by_ref(self: &Arc<Self>) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        let slab = Slab::<()>::new(PathBuf::new(), 4096, 4096, 512);
        let first_write = slab.writes.acquire().unwrap();
        let last_write = slab.writes.acquire().unwrap();
        let old = Arc::new(WakeCount::default());
        let current = Arc::new(WakeCount::default());
        let other = Arc::new(WakeCount::default());
        let cancelled = Arc::new(WakeCount::default());
        let poll = |op: &mut Operation<'_, (), Error>, wakes: &Arc<WakeCount>| {
            op.as_mut()
                .poll(&mut Context::from_waker(&Waker::from(wakes.clone())))
        };
        let mut one = slab.fence_writes();
        let mut two = slab.fence_writes();
        let mut abandoned = slab.fence_writes();
        assert!(poll(&mut one, &old).is_pending());
        assert!(poll(&mut one, &current).is_pending());
        assert!(poll(&mut two, &other).is_pending());
        assert!(poll(&mut abandoned, &cancelled).is_pending());
        assert_eq!(slab.writes.waiters.borrow().len(), 3);
        drop(abandoned);
        assert_eq!(slab.writes.waiters.borrow().len(), 2);
        drop(first_write);
        for count in [&old, &current, &other, &cancelled] {
            assert_eq!(count.0.load(Ordering::Relaxed), 0);
        }
        drop(last_write);
        assert_eq!(old.0.load(Ordering::Relaxed), 0);
        assert_eq!(cancelled.0.load(Ordering::Relaxed), 0);
        assert_eq!(current.0.load(Ordering::Relaxed), 1);
        assert_eq!(other.0.load(Ordering::Relaxed), 1);
        assert!(slab.writes.waiters.borrow().is_empty());
        let next = slab.writes.acquire().unwrap();
        assert!(poll(&mut one, &current).is_pending());
        assert_eq!(slab.writes.waiters.borrow().len(), 1);
        drop(next);
        assert_eq!(current.0.load(Ordering::Relaxed), 2);
        assert_eq!(poll(&mut one, &current), Poll::Ready(Ok(())));
        assert_eq!(poll(&mut two, &other), Poll::Ready(Ok(())));
        assert_eq!(
            poll(&mut slab.fence_writes(), &current),
            Poll::Ready(Ok(()))
        );
    }

    /// Overflow cannot create a decrement guard or alter the saturated count.
    #[test]
    fn write_guard_overflow_is_atomic() {
        let state = Rc::new(WriteState::default());
        state.count.set(usize::MAX);
        assert!(matches!(state.acquire(), Err(Error::Busy)));
        assert_eq!(state.count.get(), usize::MAX);
        state.count.set(0);
        let guard = state.acquire().unwrap();
        assert_eq!(state.count.get(), 1);
        drop(guard);
        assert_eq!(state.count.get(), 0);
    }

    /// Validation consumes rejected resources and successful preparation retains its guard.
    #[test]
    fn prepared_submission_owns_resources_and_failed_binding_is_recoverable() {
        let directory = Directory::new();
        let slab = Slab::<CountingCharge>::new(directory.0.join("prepared"), 8192, 4096, 512);
        if real_alignment(slab.open_now()).is_none() {
            return;
        }
        let used = Rc::new(Cell::new(0));
        let guard = Rc::new(());
        let buffer = slab
            .allocate(4096, CountingCharge::new(&used, 4096))
            .unwrap();
        assert!(matches!(
            slab.prepare_at(0, buffer, guard.clone()),
            Err(Error::Unavailable)
        ));
        assert_eq!(Rc::strong_count(&guard), 1);
        assert_eq!(slab.reclaim_idle(), 4096);
        assert_eq!(used.get(), 0);
        assert_eq!(slab.writes_in_flight(), 0);
        let _ = slab.open_volume(2).unwrap();
        let buffer = slab
            .allocate(4096, CountingCharge::new(&used, 4096))
            .unwrap();
        let submission = slab.prepare_at(0, buffer, guard.clone()).unwrap();
        assert_eq!(Rc::strong_count(&guard), 2);
        drop(submission);
        assert_eq!(Rc::strong_count(&guard), 1);
        assert_eq!(used.get(), 4096);
        slab.reclaim_idle();
        assert_eq!(used.get(), 0);
    }

    /// System classification preserves errno and operation without hiding failures.
    #[test]
    fn system_errors_keep_operation_and_errno() {
        for errno in [
            libc::EACCES,
            libc::EPERM,
            libc::ENOSPC,
            libc::EIO,
            libc::ELOOP,
        ] {
            assert_eq!(
                direct_error("open", std::io::Error::from_raw_os_error(errno)),
                Error::SystemIo {
                    operation: "open",
                    errno: Some(errno)
                }
            );
            assert_eq!(
                lock_error(std::io::Error::from_raw_os_error(errno)),
                Error::SystemIo {
                    operation: "flock",
                    errno: Some(errno)
                }
            );
        }
        assert_eq!(
            lock_error(std::io::Error::from_raw_os_error(libc::EWOULDBLOCK)),
            Error::Unavailable
        );
        for errno in [libc::EINVAL, libc::EOPNOTSUPP, libc::ENOSYS] {
            assert_eq!(
                direct_error("fcntl-direct", std::io::Error::from_raw_os_error(errno)),
                Error::Unsupported
            );
        }
        assert_eq!(
            system_error("test", std::io::Error::other("synthetic")),
            Error::SystemIo {
                operation: "test",
                errno: None
            }
        );
        assert_eq!(
            probe_fd(-1),
            Err(Error::SystemIo {
                operation: "statx",
                errno: Some(libc::EBADF)
            })
        );
        // SAFETY: zero is a valid initialized statx output representation.
        let mut stat: libc::statx = unsafe { std::mem::zeroed() };
        assert_eq!(alignment_from_stat(&stat), Err(Error::Unsupported));
        stat.stx_mask = libc::STATX_DIOALIGN;
        stat.stx_dio_mem_align = 512;
        stat.stx_dio_offset_align = 512;
        assert_eq!(alignment_from_stat(&stat), Alignment::new(512, 512, 512));
    }

    /// File validation requires regular type, effective owner, private mode, and one link.
    #[test]
    fn private_file_validation_checks_type_owner_permissions_and_links() {
        assert_eq!(validate_file(libc::S_IFREG | 0o600, 17, 17, 1), Ok(()));
        for (mode, owner, links) in [
            (libc::S_IFREG | 0o644, 17, 1),
            (libc::S_IFREG | 0o600, 18, 1),
            (libc::S_IFREG | 0o600, 17, 2),
            (libc::S_IFREG | 0o4600, 17, 1),
            (libc::S_IFDIR | 0o600, 17, 1),
            (libc::S_IFIFO | 0o600, 17, 1),
        ] {
            assert_eq!(
                validate_file(mode, owner, 17, links),
                Err(Error::InvalidConfiguration)
            );
        }
    }

    /// A prior lock holder cannot leave unsafe metadata for startup to accept.
    #[test]
    fn lock_handoff_revalidates_private_file_metadata() {
        use std::os::unix::fs::PermissionsExt;

        for change in ["permissions", "special-bits", "hard-link", "unlink"] {
            let directory = Directory::new();
            let path = directory.0.join("data");
            let prior = open_private_file(&path).unwrap();
            let observer = File::open(&path).unwrap();
            // SAFETY: flock borrows a live descriptor owned by this test.
            assert_eq!(
                unsafe { libc::flock(prior.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
                0
            );
            let slab = Slab::<()>::new(path.clone(), 8192, 4096, 512);
            let mut called = false;
            let result = slab.open_file_with_lock_handoff(|_| {
                called = true;
                match change {
                    "permissions" => prior
                        .set_permissions(std::fs::Permissions::from_mode(0o644))
                        .unwrap(),
                    "special-bits" => prior
                        .set_permissions(std::fs::Permissions::from_mode(0o4600))
                        .unwrap(),
                    "hard-link" => std::fs::hard_link(&path, directory.0.join("alias")).unwrap(),
                    "unlink" => std::fs::remove_file(&path).unwrap(),
                    _ => unreachable!(),
                }
                drop(prior);
            });
            assert!(called);
            assert_eq!(result, Err(Error::InvalidConfiguration), "{change}");
            assert!(slab.opened.borrow().is_none());
            assert_eq!(observer.metadata().unwrap().len(), 0);
            // SAFETY: the independent live descriptor checks lock cleanup on failure.
            assert_eq!(
                unsafe { libc::flock(observer.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
                0
            );
        }
    }

    /// Valid handoffs use the current size and never truncate mismatched files.
    #[test]
    fn lock_handoff_uses_refreshed_size() {
        for size_in_segments in [0, 2, 1] {
            let directory = Directory::new();
            let path = directory.0.join("data");
            let prior = open_private_file(&path).unwrap();
            let Some(alignment) = real_alignment(probe(&prior)) else {
                continue;
            };
            let segment_bytes = alignment.extent(0, 512).unwrap().length() as u64;
            let capacity = segment_bytes.checked_mul(2).unwrap();
            let size = segment_bytes * size_in_segments;
            // SAFETY: flock borrows a live descriptor owned by this test.
            assert_eq!(
                unsafe { libc::flock(prior.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
                0
            );
            let slab = Slab::<()>::new(path.clone(), capacity, segment_bytes, 512);
            let result = slab.open_file_with_lock_handoff(|_| {
                prior.set_len(size).unwrap();
                drop(prior);
            });
            if result == Err(Error::Unsupported) {
                assert!(real_alignment(result).is_none());
                continue;
            }
            if size_in_segments == 1 {
                assert_eq!(result, Err(Error::InvalidConfiguration));
                assert!(slab.opened.borrow().is_none());
                assert_eq!(std::fs::metadata(&path).unwrap().len(), size);
            } else {
                assert_eq!(result, Ok(alignment));
                assert_eq!(slab.alignment(), Ok(alignment));
                assert_eq!(std::fs::metadata(&path).unwrap().len(), capacity);
                assert_eq!(
                    Slab::<()>::new(path, capacity, segment_bytes, 512).open_now(),
                    Err(Error::Unavailable)
                );
            }
        }
    }

    /// Geometry for resize tests, or None when the filesystem lacks direct I/O.
    fn resize_geometry(path: &Path) -> Option<(u64, u64)> {
        let probe_file = open_private_file(path).unwrap();
        let alignment = real_alignment(probe(&probe_file))?;
        let segment_bytes = alignment.extent(0, 512).unwrap().length() as u64;
        Some((segment_bytes, segment_bytes * 2))
    }

    /// A mismatched file is resized only through the validated, locked open,
    /// and the lock stays held afterward.
    #[test]
    fn resize_mismatched_resizes_under_lock() {
        let directory = Directory::new();
        let path = directory.0.join("data");
        let Some((segment_bytes, capacity)) = resize_geometry(&path) else {
            return;
        };
        for size in [segment_bytes, capacity * 3 + 1] {
            std::fs::OpenOptions::new()
                .write(true)
                .open(&path)
                .unwrap()
                .set_len(size)
                .unwrap();
            assert_eq!(
                Slab::<()>::new(path.clone(), capacity, segment_bytes, 512).open_now(),
                Err(Error::InvalidConfiguration)
            );
            assert_eq!(std::fs::metadata(&path).unwrap().len(), size);
            let slab =
                Slab::<()>::new(path.clone(), capacity, segment_bytes, 512).resize_mismatched();
            assert!(slab.open_now().is_ok());
            assert_eq!(std::fs::metadata(&path).unwrap().len(), capacity);
            let observer = File::open(&path).unwrap();
            // SAFETY: flock borrows a live descriptor owned by this test.
            assert_ne!(
                unsafe { libc::flock(observer.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
                0
            );
            drop(slab);
        }
    }

    /// A second opener cannot resize a file another owner holds locked.
    #[test]
    fn resize_mismatched_leaves_locked_file_unchanged() {
        let directory = Directory::new();
        let path = directory.0.join("data");
        let Some((segment_bytes, capacity)) = resize_geometry(&path) else {
            return;
        };
        let owner = Slab::<()>::new(path.clone(), capacity * 2, segment_bytes, 512);
        assert!(owner.open_now().is_ok());
        let second =
            Slab::<()>::new(path.clone(), capacity, segment_bytes, 512).resize_mismatched();
        assert_eq!(second.open_now(), Err(Error::Unavailable));
        assert!(second.opened.borrow().is_none());
        assert_eq!(std::fs::metadata(&path).unwrap().len(), capacity * 2);
    }

    /// A symlinked slab path is rejected and its target is never resized.
    #[test]
    fn resize_mismatched_rejects_symlink_without_touching_target() {
        let directory = Directory::new();
        let target = directory.0.join("target");
        let Some((segment_bytes, capacity)) = resize_geometry(&target) else {
            return;
        };
        std::fs::OpenOptions::new()
            .write(true)
            .open(&target)
            .unwrap()
            .set_len(capacity * 3)
            .unwrap();
        let link = directory.0.join("link");
        std::os::unix::fs::symlink(&target, &link).unwrap();
        let slab = Slab::<()>::new(link, capacity, segment_bytes, 512).resize_mismatched();
        assert!(slab.open_now().is_err());
        assert!(slab.opened.borrow().is_none());
        assert_eq!(std::fs::metadata(&target).unwrap().len(), capacity * 3);
    }

    /// Descriptor traversal rejects symlinks, FIFOs, and parent-directory escapes.
    #[test]
    fn descriptor_relative_open_rejects_symlinks_and_nonregular_files() {
        use std::os::unix::fs::{PermissionsExt, symlink};
        let directory = Directory::new();
        let make = |path| Slab::<()>::new(path, 8192, 4096, 512);
        let target = directory.0.join("target");
        std::fs::create_dir(&target).unwrap();
        let alias = directory.0.join("alias");
        symlink(&target, &alias).unwrap();
        assert!(matches!(
            make(alias.join("data")).open_now(),
            Err(Error::SystemIo {
                operation: "open-parent",
                errno: Some(libc::ENOTDIR | libc::ELOOP)
            })
        ));
        assert!(!target.join("data").exists());
        let data = target.join("data");
        std::fs::write(&data, [42; 7]).unwrap();
        let link = directory.0.join("link");
        symlink(&data, &link).unwrap();
        assert_eq!(
            make(link).open_now(),
            Err(Error::SystemIo {
                operation: "open",
                errno: Some(libc::ELOOP)
            })
        );
        assert_eq!(std::fs::read(&data).unwrap(), [42; 7]);
        std::fs::set_permissions(&data, std::fs::Permissions::from_mode(0o666)).unwrap();
        assert_eq!(
            make(data.clone()).open_now(),
            Err(Error::InvalidConfiguration)
        );
        assert!(make(target.clone()).open_now().is_err());
        let fifo = directory.0.join("fifo");
        let fifo_c = CString::new(fifo.as_os_str().as_bytes()).unwrap();
        // SAFETY: valid C pathname and POSIX permission mode.
        assert_eq!(unsafe { libc::mkfifo(fifo_c.as_ptr(), 0o600) }, 0);
        assert_eq!(make(fifo).open_now(), Err(Error::InvalidConfiguration));
        assert_eq!(
            make(target.join("..").join("escape")).open_now(),
            Err(Error::InvalidConfiguration)
        );
        let new_path = directory.0.join("new/nested/data");
        let opened = open_private_file(&new_path).unwrap();
        assert_eq!(opened.metadata().unwrap().mode() & 0o777, 0o600);
        assert_eq!(
            std::fs::metadata(new_path.parent().unwrap())
                .unwrap()
                .mode()
                & 0o777,
            0o700
        );
    }

    /// Invalid replacement preserves idle accounting until explicit reclamation.
    #[test]
    fn idle_size_mismatch_releases_retained_charge_even_on_invalid_replacement() {
        let directory = Directory::new();
        let slab = Slab::<CountingCharge>::new(directory.0.join("data"), 8192, 4096, 512);
        let Some(a) = real_alignment(slab.open_now()) else {
            return;
        };
        let length = a.extent(0, 512).unwrap().length();
        let used = Rc::new(Cell::new(0));
        drop(
            slab.allocate(length, CountingCharge::new(&used, length))
                .unwrap(),
        );
        assert_eq!(slab.idle_bytes(), length);
        assert_eq!(used.get(), length);
        assert!(matches!(
            slab.allocate(0, CountingCharge::new(&used, 0)),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(used.get(), length);
        assert_eq!(slab.idle_bytes(), length);
        assert_eq!(slab.reclaim_idle(), length);
        assert_eq!(used.get(), 0);
        assert_eq!(slab.idle_bytes(), 0);
    }

    #[test]
    fn slab_reuses_large_capacity_across_exact_io_lengths() {
        let directory = Directory::new();
        let slab =
            Slab::<CountingCharge>::new(directory.0.join("data"), 16 << 20, 8 << 20, 4 << 20);
        let Some(a) = real_alignment(slab.open_now()) else {
            return;
        };
        let length = a.extent(0, (2 << 20) + 1).unwrap().length();
        let next = a.extent(0, length + 1).unwrap().length();
        let capacity = crate::buffer_capacity(length).unwrap();
        let used = Rc::new(Cell::new(0));
        let mut first = slab
            .allocate(length, CountingCharge::new(&used, capacity))
            .unwrap();
        let pointer = first.as_slice().as_ptr();
        first.as_mut_slice().fill(0xa5);
        drop(first);
        assert_eq!(slab.idle_bytes(), capacity);
        let mismatch = slab
            .allocate(4 << 20, CountingCharge::new(&used, 4 << 20))
            .unwrap();
        assert_eq!(slab.idle_bytes(), capacity);
        let second = slab
            .allocate(next, CountingCharge::new(&used, capacity))
            .unwrap();
        assert_eq!(second.len(), next);
        assert_eq!(second.as_slice().as_ptr(), pointer);
        assert!(second.as_slice().iter().all(|&b| b == 0));
        a.check(Extent::new(0, next).unwrap(), &second).unwrap();
        drop((second, mismatch));
        assert_eq!(slab.reclaim_idle(), capacity + (4 << 20));
        assert_eq!(used.get(), 0);
    }
}
