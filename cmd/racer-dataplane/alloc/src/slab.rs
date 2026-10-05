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
    reactor::{Descriptor, Reactor},
};

struct OpenSlab {
    file: Rc<Descriptor>,
    geometry: SegmentGeometry,
}

/// One sparse direct-I/O cache file, not a durable storage transaction.
/// `open_configured` binds submissions to one segment table; read/write require
/// a successful binding, not merely an open file.
pub struct Slab<C: Charge> {
    path: PathBuf,
    capacity_bytes: u64,
    segment_bytes: u64,
    max_record_bytes: usize,
    opened: RefCell<Option<OpenSlab>>,
    writes: Rc<WriteState>,
    table: RefCell<Option<Rc<()>>>,
    idle_buffer: Rc<RefCell<Option<AlignedBuffer<C>>>>,
}
impl<C: Charge> Slab<C> {
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
            table: RefCell::new(None),
            idle_buffer: Rc::new(RefCell::new(None)),
        }
    }
    pub fn capacity_bytes(&self) -> u64 {
        self.capacity_bytes
    }
    pub fn segment_bytes(&self) -> u64 {
        self.segment_bytes
    }
    pub fn writes_in_flight(&self) -> usize {
        self.writes.count.get()
    }
    pub fn idle_bytes(&self) -> usize {
        self.idle_buffer.borrow().as_ref().map_or(0, |b| b.len())
    }
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
    pub fn configure_segments(&self, segments: &Segments) -> Result<()> {
        let geometry = self.geometry()?;
        let identity = segments.table_identity();
        if let Some(bound) = self.table.borrow().as_ref()
            && !Rc::ptr_eq(bound, &identity)
        {
            return Err(Error::InvalidConfiguration);
        }
        if segments.segment_bytes() != geometry.segment_bytes() {
            return Err(Error::InvalidConfiguration);
        }
        if !segments.is_configured() {
            let count = usize::try_from(geometry.segment_count())
                .map_err(|_| Error::InvalidConfiguration)?;
            segments.configure(geometry.slab_bytes(), count, geometry.alignment())?;
        }
        let configured = segments.geometry().ok_or(Error::InvalidConfiguration)?;
        if configured.slab_bytes() != geometry.slab_bytes()
            || configured.segment_bytes() != geometry.segment_bytes()
            || configured.alignment() != geometry.alignment()
            || configured.segment_count() > geometry.segment_count()
        {
            return Err(Error::InvalidConfiguration);
        }
        *self.table.borrow_mut() = Some(identity);
        Ok(())
    }
    /// Blocking startup helper. Do not invoke on a latency-sensitive worker.
    pub fn open_configured(&self, segments: &Segments) -> Result<Alignment> {
        let alignment = self.open_now()?;
        self.configure_segments(segments)?;
        Ok(alignment)
    }
    /// Blocking startup I/O for geometry probing and buffer allocation. Read/write
    /// return `Unavailable` until `configure_segments` succeeds.
    /// Parent directories must be trusted against
    /// rename/unlink by other users. Linux traversal rejects symlinks and `..`;
    /// newly created directories are private. Existing files must be owned by the
    /// effective user, regular, singly linked, and mode 0600.
    pub fn open_now(&self) -> Result<Alignment> {
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
    fn publish(&self, file: Descriptor, geometry: SegmentGeometry) {
        *self.opened.borrow_mut() = Some(OpenSlab {
            file: Rc::new(file),
            geometry,
        });
    }
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
    fn submission(
        &self,
        extent: Extent,
        buffer: &AlignedBuffer<C>,
        lease: &SegmentLease,
    ) -> Result<Rc<Descriptor>> {
        let opened = self.opened.borrow();
        let slab = opened.as_ref().ok_or(Error::Unavailable)?;
        let binding = self.table.borrow();
        let table = binding.as_ref().ok_or(Error::Unavailable)?;
        slab.geometry.alignment().check(extent, buffer)?;
        if !Rc::ptr_eq(table, &lease.table_identity()) {
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
            let fd = self.submission(extent, &buffer, &lease)?;
            let completion = reactor
                .read_at(fd, extent.offset(), buffer, lease, scope)
                .await?;
            if completion.bytes != extent.length() {
                return Err(Error::Io.into());
            }
            Ok(completion.buffer)
        })
    }
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
            let fd = self.submission(extent, &buffer, &lease)?;
            self.writes
                .count
                .set(self.writes.count.get().checked_add(1).ok_or(Error::Busy)?);
            let fence = WriteFence(self.writes.clone());
            let completion = reactor
                .write_at(fd, extent.offset(), buffer, (lease, fence), scope)
                .await?;
            if completion.bytes != extent.length() {
                return Err(Error::Io.into());
            }
            Ok(completion.buffer)
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
#[derive(Default)]
struct WriteState {
    count: Cell<usize>,
    waiters: RefCell<Vec<Rc<RefCell<Waker>>>>,
}
struct FenceWaiter {
    state: Rc<WriteState>,
    registration: Option<Rc<RefCell<Waker>>>,
}
impl Future for FenceWaiter {
    type Output = Result<()>;
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
    fn drop(&mut self) {
        self.unregister();
    }
}
struct WriteFence(Rc<WriteState>);
impl Drop for WriteFence {
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
fn system_error(operation: &'static str, error: std::io::Error) -> Error {
    Error::SystemIo {
        operation,
        errno: error.raw_os_error(),
    }
}
fn lock_error(error: std::io::Error) -> Error {
    if error.raw_os_error() == Some(libc::EWOULDBLOCK) {
        Error::Unavailable
    } else {
        system_error("flock", error)
    }
}
fn direct_error(operation: &'static str, error: std::io::Error) -> Error {
    match error.raw_os_error() {
        Some(libc::EINVAL | libc::EOPNOTSUPP | libc::ENOSYS) => Error::Unsupported,
        _ => system_error(operation, error),
    }
}
fn probe(file: &File) -> Result<Alignment> {
    probe_fd(file.as_raw_fd())
}
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

#[cfg(test)]
mod tests;
