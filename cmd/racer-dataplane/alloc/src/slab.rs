//! Locked sparse storage and completion-owned direct I/O.
//!
//! Startup is blocking and belongs outside the latency-sensitive worker path.
//! Capacity bounds logical addresses, not reserved physical space: writes can
//! still fail with ENOSPC. Recycling never truncates or erases disk bytes.
//! Parent directories must be trusted against hostile rename and unlink, even
//! though descriptor-relative traversal rejects symlinks and parent components.
use crate::segments::TableIdentity;
use crate::{
    AlignedBuffer, Alignment, Charge, Error, Extent, Result, SegmentGeometry, SegmentLease,
    Segments,
};
use std::{
    cell::{Cell, RefCell},
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
    task::{Context, Poll, Waker},
};
use uring_runtime::{
    Budget, Operation, Scope,
    reactor::{Reactor, descriptor::Descriptor},
};

/// One sparse direct-I/O cache file, not a durable storage transaction.
/// `open_configured` binds submissions to one segment table; read/write require
/// a successful binding, not merely an open file.
#[repr(align(64))]
pub struct Slab<C: Charge> {
    path: PathBuf,

    capacity_bytes: u64,

    segment_bytes: u64,

    max_record_bytes: usize,

    opened: RefCell<Option<OpenSlab>>,

    writes: Rc<WriteState>,

    idle_buffer: Rc<RefCell<Option<AlignedBuffer<C>>>>,
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
            path,
            capacity_bytes,
            segment_bytes,
            max_record_bytes,
            opened: RefCell::new(None),
            writes: Rc::new(WriteState::default()),
            idle_buffer: Rc::new(RefCell::new(None)),
        }
    }

    /// Logical file capacity, not a reservation of physical disk blocks.
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

    /// Accounted bytes in the single idle buffer slot.
    pub fn idle_bytes(&self) -> usize {
        self.idle_buffer.borrow().as_ref().map_or(0, |b| b.len())
    }

    /// Release only idle memory, returning its size without touching live I/O.
    pub fn reclaim_idle(&self) -> usize {
        let idle = self.idle_buffer.borrow_mut().take();
        idle.map_or(0, |b| b.len())
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

    /// Discovered direct-I/O requirements, or Unavailable before startup.
    pub fn alignment(&self) -> Result<Alignment> {
        self.opened
            .borrow()
            .as_ref()
            .map(|o| o.geometry.alignment())
            .ok_or(Error::Unavailable)
    }

    /// Physical slab geometry, including the full segment capacity. A bound
    /// table may deliberately expose fewer segments than this physical count.
    pub fn geometry(&self) -> Result<SegmentGeometry> {
        self.opened
            .borrow()
            .as_ref()
            .map(|o| o.geometry)
            .ok_or(Error::Unavailable)
    }

    /// Configure an empty table, or validate an already configured partial table,
    /// and permanently bind this slab to that table's identity.
    #[cfg(any(test, feature = "simulation"))]
    pub fn configure_segments(&self, segments: &Segments) -> Result<()> {
        self.bind(segments)
    }

    /// Attach the table identity to this file only after all dimensions agree.
    fn bind(&self, segments: &Segments) -> Result<()> {
        let mut opened = self.opened.borrow_mut();
        let slab = opened.as_mut().ok_or(Error::Unavailable)?;
        let geometry = slab.geometry;
        let identity = segments.table_identity();
        if let Some(bound) = slab.table.as_ref()
            && !bound.matches(&identity)
        {
            return Err(Error::InvalidConfiguration);
        }
        if segments.segment_bytes() != geometry.segment_bytes() {
            return Err(Error::InvalidConfiguration);
        }
        if !segments.is_configured() {
            let count = usize::try_from(geometry.segment_count())
                .map_err(|_| Error::InvalidConfiguration)?;
            segments.configure_table(geometry.slab_bytes(), count, geometry.alignment())?;
        }
        let configured = segments.geometry().ok_or(Error::InvalidConfiguration)?;
        if configured.slab_bytes() != geometry.slab_bytes()
            || configured.segment_bytes() != geometry.segment_bytes()
            || configured.alignment() != geometry.alignment()
            || configured.segment_count() > geometry.segment_count()
        {
            return Err(Error::InvalidConfiguration);
        }
        slab.table = Some(identity);
        Ok(())
    }

    /// Blocking startup helper. Do not invoke on a latency-sensitive worker.
    pub fn open_configured(&self, segments: &Segments) -> Result<Alignment> {
        let alignment = self.open_file()?;
        self.bind(segments)?;
        Ok(alignment)
    }

    /// Blocking startup I/O for geometry probing and buffer allocation. Read/write
    /// return `Unavailable` until `configure_segments` succeeds.
    /// Parent directories must be trusted against
    /// rename/unlink by other users. Linux traversal rejects symlinks and `..`;
    /// newly created directories are private. Existing files must be owned by the
    /// effective user, regular, singly linked, and mode 0600.
    #[cfg(any(test, feature = "simulation"))]
    pub fn open_now(&self) -> Result<Alignment> {
        self.open_file()
    }

    /// Open and validate the physical file without granting submission authority.
    fn open_file(&self) -> Result<Alignment> {
        if let Ok(a) = self.alignment() {
            return Ok(a);
        }
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
            if let Some(parent) = self.path.parent().filter(|p| !p.as_os_str().is_empty()) {
                sim.create_dir_all(parent)
                    .map_err(|e| system_error("mkdir", e))?;
            }
            let file = sim
                .open(
                    None,
                    &self.path,
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
            let geometry = self.validate_layout(a, stat.stx_size)?;
            if stat.stx_size == 0 {
                handle
                    .set_len(self.capacity_bytes)
                    .map_err(|e| system_error("ftruncate", e))?;
            }
            self.publish(file, geometry);
            return Ok(a);
        }
        let file = open_private_file(&self.path)?;
        let metadata = file.metadata().map_err(|e| system_error("fstat", e))?;
        // SAFETY: geteuid has no arguments or borrowed memory.
        validate_file(
            metadata.mode(),
            metadata.uid(),
            unsafe { libc::geteuid() },
            metadata.nlink(),
        )?;
        // SAFETY: flock synchronously borrows this live descriptor.
        if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
            return Err(lock_error(std::io::Error::last_os_error()));
        }
        // Refresh size under the lock: a previous lock holder may have resized
        // the file between the first fstat and acquiring our lock.
        let size = file.metadata().map_err(|e| system_error("fstat", e))?.len();
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
        let geometry = self.validate_layout(a, size)?;
        if size == 0 {
            file.set_len(self.capacity_bytes)
                .map_err(|e| system_error("ftruncate", e))?;
        }
        self.publish(file.into(), geometry);
        Ok(a)
    }

    /// Publish geometry with its owning file, initially without I/O authority.
    fn publish(&self, file: Descriptor, geometry: SegmentGeometry) {
        *self.opened.borrow_mut() = Some(OpenSlab {
            file: Rc::new(file),
            geometry,
            table: None,
        });
    }

    /// Check physical dimensions and padded record size without changing the file.
    fn validate_layout(&self, a: Alignment, size: u64) -> Result<SegmentGeometry> {
        if a.extent(0, self.max_record_bytes)?.length() as u64 > self.segment_bytes
            || !self.segment_bytes.is_multiple_of(a.offset())
            || !self.segment_bytes.is_multiple_of(a.length() as u64)
            || (size != 0 && size != self.capacity_bytes)
        {
            return Err(Error::InvalidConfiguration);
        }
        SegmentGeometry::new(
            self.capacity_bytes,
            self.segment_bytes,
            self.capacity_bytes / self.segment_bytes,
            a,
        )
        .map_err(|_| Error::InvalidConfiguration)
    }

    /// Borrow-check a request before moving its resources into a submission.
    fn submission(
        &self,
        extent: Extent,
        buffer: &AlignedBuffer<C>,
        lease: &SegmentLease,
    ) -> Result<Rc<Descriptor>> {
        let opened = self.opened.borrow();
        let slab = opened.as_ref().ok_or(Error::Unavailable)?;
        let table = slab.table.as_ref().ok_or(Error::Unavailable)?;
        slab.geometry.alignment().check(extent, buffer)?;
        if !table.matches(&lease.table_identity()) {
            return Err(Error::Stale);
        }
        let geometry = lease.geometry();
        if geometry.slab_bytes() != slab.geometry.slab_bytes()
            || geometry.segment_bytes() != slab.geometry.segment_bytes()
            || geometry.alignment() != slab.geometry.alignment()
        {
            return Err(Error::Corrupt);
        }
        lease.validate_extent(&extent)?;
        let start = lease
            .id()
            .0
            .checked_mul(self.segment_bytes)
            .ok_or(Error::Corrupt)?;
        let end = extent
            .offset()
            .checked_add(extent.length() as u64)
            .ok_or(Error::Corrupt)?;
        if extent.offset() < start
            || end
                > start
                    .checked_add(self.segment_bytes)
                    .ok_or(Error::Corrupt)?
            || end > self.capacity_bytes
        {
            return Err(Error::Corrupt);
        }
        Ok(slab.file.clone())
    }

    /// Consume a checked request so its descriptor, buffer, and lease travel together.
    fn prepare(
        &self,
        extent: Extent,
        buffer: AlignedBuffer<C>,
        lease: SegmentLease,
    ) -> Result<Submission<C>> {
        let file = self.submission(extent, &buffer, &lease)?;
        Ok(Submission {
            file,
            extent,
            buffer,
            lease,
        })
    }

    /// Read exactly one checked extent, retaining its buffer and lease until completion.
    /// Dropping the waiting future does not release kernel-owned resources.
    pub fn read<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        extent: Extent,
        buffer: AlignedBuffer<C>,
        lease: SegmentLease,
        scope: &'a S,
    ) -> Operation<'a, AlignedBuffer<C>, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            self.prepare(extent, buffer, lease)?
                .read(reactor, scope)
                .await
        })
    }

    /// Write exactly one checked extent with completion-owned accounting and lease.
    /// Failed writes do not roll back the space reserved by append.
    pub fn write<'a, S: Scope, B: Budget>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        extent: Extent,
        buffer: AlignedBuffer<C>,
        lease: SegmentLease,
        scope: &'a S,
    ) -> Operation<'a, AlignedBuffer<C>, S::Error>
    where
        S::Error: From<Error>,
    {
        Box::pin(async move {
            let submission = self.prepare(extent, buffer, lease)?;
            let fence = self.writes.acquire()?;
            submission.write(reactor, scope, fence).await
        })
    }

    /// Reuse one exact-size idle buffer. A size mismatch releases the old idle
    /// buffer and its charge, even if the replacement allocation fails.
    pub fn allocate(&self, length: usize, charge: C) -> Result<AlignedBuffer<C>> {
        let alignment = self.alignment()?;
        if !charge.covers(length) {
            return Err(Error::InvalidConfiguration);
        }
        let idle = self.idle_buffer.borrow_mut().take();
        if let Some(mut buffer) = idle
            && buffer.len() == length
        {
            buffer.rebind(charge)?;
            return Ok(buffer.pooled(&self.idle_buffer));
        }
        Ok(alignment
            .allocate(length, charge)?
            .pooled(&self.idle_buffer))
    }

    /// Replace the open file for fault-injection tests; geometry remains unchanged.
    #[cfg(feature = "simulation")]
    #[doc(hidden)]
    pub fn replace_file_for_test(&self, file: File) -> Result<()> {
        self.replace_descriptor_for_test(file.into())
    }

    /// Also accepts a virtual descriptor from the simulation backend.
    #[cfg(feature = "simulation")]
    #[doc(hidden)]
    pub fn replace_descriptor_for_test(&self, file: Descriptor) -> Result<()> {
        if self.writes_in_flight() != 0 {
            return Err(Error::Busy);
        }
        self.opened
            .borrow_mut()
            .as_mut()
            .ok_or(Error::Unavailable)?
            .file = Rc::new(file);
        Ok(())
    }
}

/// A file and its geometry own their binding; a closed slab cannot retain authority.
struct OpenSlab {
    file: Rc<Descriptor>,

    geometry: SegmentGeometry,

    table: Option<TableIdentity>,
}

/// A validated transfer owns every resource needed to keep kernel access safe.
/// Only Slab::prepare constructs this capability, and submission consumes it.
struct Submission<C: Charge> {
    file: Rc<Descriptor>,

    extent: Extent,

    buffer: AlignedBuffer<C>,

    lease: SegmentLease,
}

impl<C: Charge> Submission<C> {
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
        let waker = cx.waker().clone();
        if self.state.count.get() == 0 {
            self.unregister();
            return Poll::Ready(Ok(()));
        }
        if let Some(registration) = self.registration.clone() {
            let old = registration.replace(waker);
            drop(old);
            if self.state.count.get() == 0 {
                self.unregister();
                return Poll::Ready(Ok(()));
            }
            let mut waiters = self.state.waiters.borrow_mut();
            if !waiters.iter().any(|w| Rc::ptr_eq(w, &registration)) {
                waiters.push(registration);
            }
        } else {
            let registration = Rc::new(RefCell::new(waker));
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
    use crate::{Generation, SegmentId};

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

    /// Pooling preserves the primary charge and zeroes bytes before reuse.
    #[test]
    fn aligned_pool_reuses_only_fenced_zeroed_admitted_storage() {
        let used = Rc::new(Cell::new(0));
        let pool = Rc::new(RefCell::new(None));
        let alignment = Alignment::new(512, 512, 512).unwrap();
        let charge = || CountingCharge::new(&used, 512);
        let mut buffer = alignment.allocate(512, charge()).unwrap().pooled(&pool);
        let pointer = buffer.bytes().unwrap().as_ptr();
        buffer.bytes_mut().unwrap().fill(42);
        let extra = Rc::new(charge());
        let weak = Rc::downgrade(&extra);
        buffer.retain(extra);
        assert_eq!(used.get(), 1024);
        drop(buffer);
        assert!(weak.upgrade().is_none());
        assert_eq!(used.get(), 512);
        let mut reused = pool.borrow_mut().take().unwrap();
        assert_eq!(reused.bytes().unwrap().as_ptr(), pointer);
        assert!(reused.bytes().unwrap().iter().all(|b| *b == 0));
        reused.rebind(charge()).unwrap();
        assert_eq!(used.get(), 512);
        drop(reused.pooled(&pool));
        drop(pool);
        assert_eq!(used.get(), 0);
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

    /// Failed admission and borrowed return slots cannot leak accounted bytes.
    #[test]
    fn invalid_charge_is_released_and_pool_borrow_does_not_panic() {
        let used = Rc::new(Cell::new(0));
        let a = Alignment::new(512, 512, 512).unwrap();
        assert!(matches!(
            a.allocate(512, CountingCharge::new(&used, 511)),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(used.get(), 0);
        let pool = Rc::new(RefCell::new(None));
        let b = a
            .allocate(512, CountingCharge::new(&used, 512))
            .unwrap()
            .pooled(&pool);
        let borrow = pool.borrow_mut();
        drop(b);
        assert_eq!(used.get(), 0);
        drop(borrow);
        assert!(pool.borrow().is_none());
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
        let fd = opened.as_ref().unwrap().file.as_raw_fd();
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
        assert_eq!(
            unsafe { libc::pwrite(fd, buffer.bytes().unwrap().as_ptr().cast(), 31, 1) },
            -1
        );
        drop(buffer);
        assert_eq!(slabs.idle_bytes(), extent.length());
        assert_eq!(slabs.reclaim_idle(), extent.length());
        assert_eq!(slabs.idle_bytes(), 0);
        assert_eq!(conflicting.open_now(), Err(Error::Unavailable));
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
    fn submission_checks_alignment_segment_and_capacity() {
        let directory = Directory::new();
        let slab = Slab::<()>::new(directory.0.join("data"), 8192, 4096, 512);
        let segments = Segments::new(4096);
        let Some(a) = real_alignment(slab.open_configured(&segments)) else {
            return;
        };
        let (lease, extent) = segments.append(4096).unwrap();
        let buffer = slab.allocate(4096, ()).unwrap();
        assert!(slab.submission(extent, &buffer, &lease).is_ok());
        assert!(matches!(
            slab.submission(Extent::new(4096, 4096).unwrap(), &buffer, &lease),
            Err(Error::Corrupt)
        ));
        assert!(matches!(
            slab.submission(Extent::new(1, 4096).unwrap(), &buffer, &lease),
            Err(Error::InvalidConfiguration)
        ));
        let outside_table = Segments::new(4096);
        outside_table.configure(12288, 3, a).unwrap();
        drop(outside_table.append(4096).unwrap());
        drop(outside_table.append(4096).unwrap());
        let (outside, extent) = outside_table.append(4096).unwrap();
        assert!(matches!(
            slab.submission(extent, &buffer, &outside),
            Err(Error::Stale)
        ));
        assert_eq!(
            outside_table.validate(SegmentId(2), Generation(1), &extent),
            Ok(())
        );
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

    /// Validation consumes rejected resources and successful preparation retains its lease.
    #[test]
    fn prepared_submission_owns_resources_and_failed_binding_is_recoverable() {
        let directory = Directory::new();
        let slab = Slab::<CountingCharge>::new(directory.0.join("prepared"), 8192, 4096, 512);
        let Some(alignment) = real_alignment(slab.open_now()) else {
            return;
        };
        let table = Segments::new(4096);
        table.configure(8192, 2, alignment).unwrap();
        let (lease, extent) = table.append(4096).unwrap();
        let used = Rc::new(Cell::new(0));
        let buffer = slab
            .allocate(4096, CountingCharge::new(&used, 4096))
            .unwrap();
        assert!(matches!(
            slab.prepare(extent, buffer, lease),
            Err(Error::Unavailable)
        ));
        assert_eq!(slab.reclaim_idle(), 4096);
        assert_eq!(used.get(), 0);
        assert_eq!(slab.writes_in_flight(), 0);
        slab.configure_segments(&table).unwrap();
        let lease = table.lease(SegmentId(0), Generation(1)).unwrap();
        let buffer = slab
            .allocate(4096, CountingCharge::new(&used, 4096))
            .unwrap();
        let submission = slab.prepare(extent, buffer, lease).unwrap();
        table.begin_evict(SegmentId(0)).unwrap();
        assert_eq!(table.recycle(SegmentId(0)), Err(Error::Busy));
        drop(submission);
        table.recycle(SegmentId(0)).unwrap();
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

    /// Binding is permanent and validates dimensions as well as captured used extents.
    #[test]
    fn binding_validates_geometry_identity_and_used_extents() {
        let directory = Directory::new();
        let slab = Slab::<()>::new(directory.0.join("data"), 8192, 4096, 512);
        let table = Segments::new(4096);
        let Some(a) = real_alignment(slab.open_configured(&table)) else {
            return;
        };
        assert_eq!(table.geometry(), Some(slab.geometry().unwrap()));
        slab.configure_segments(&table).unwrap();
        let other = Segments::new(4096);
        other.configure(8192, 2, a).unwrap();
        assert_eq!(
            slab.configure_segments(&other),
            Err(Error::InvalidConfiguration)
        );
        let length = a.extent(0, 512).unwrap().length();
        let (foreign, extent) = other.append(length).unwrap();
        let buffer = slab.allocate(length, ()).unwrap();
        assert!(matches!(
            slab.submission(extent, &buffer, &foreign),
            Err(Error::Stale)
        ));
        let (lease, extent) = table.append(length).unwrap();
        assert!(slab.submission(extent, &buffer, &lease).is_ok());
        assert!(matches!(
            slab.submission(Extent::new(length as u64, length).unwrap(), &buffer, &lease),
            Err(Error::Corrupt)
        ));
        let wrong = Slab::<()>::new(directory.0.join("wrong"), 8192, 4096, 512);
        let _ = wrong.open_now().unwrap();
        assert_eq!(
            wrong.configure_segments(&Segments::new(8192)),
            Err(Error::InvalidConfiguration)
        );
        let wrong_capacity = Segments::new(4096);
        wrong_capacity.configure(4096, 1, a).unwrap();
        assert_eq!(
            wrong.configure_segments(&wrong_capacity),
            Err(Error::InvalidConfiguration)
        );
        let wrong_alignment = Segments::new(4096);
        let incompatible = Alignment::new(a.memory() * 2, a.offset(), a.length()).unwrap();
        wrong_alignment.configure(8192, 2, incompatible).unwrap();
        assert_eq!(
            wrong.configure_segments(&wrong_alignment),
            Err(Error::InvalidConfiguration)
        );
        let partial = Segments::new(4096);
        partial.configure(8192, 1, a).unwrap();
        wrong.configure_segments(&partial).unwrap();
        assert_eq!(partial.count(), 1);
        if length < 4096 {
            drop(table.append(4096 - length).unwrap());
        }
        table.begin_evict(SegmentId(0)).unwrap();
        assert!(slab.submission(extent, &buffer, &lease).is_ok());
        assert_eq!(table.recycle(SegmentId(0)), Err(Error::Busy));
        drop(lease);
        table.recycle(SegmentId(0)).unwrap();
    }

    /// Size mismatch releases idle accounting even when new allocation is invalid.
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
        assert_eq!(used.get(), 0);
        assert_eq!(slab.idle_bytes(), 0);
    }
}
