use crate::{AlignedBuffer, Alignment, Charge, Error, Extent, Result, SegmentLease};
use std::{
    cell::{Cell, RefCell},
    fs::{File, OpenOptions},
    os::{fd::AsRawFd, unix::fs::OpenOptionsExt},
    path::PathBuf,
    rc::Rc,
};
use uring_runtime::{
    Budget, Operation, Scope,
    reactor::{Descriptor, Reactor},
};

struct OpenSlab {
    file: Rc<Descriptor>,
    alignment: Alignment,
}

/// One sparse direct-I/O file. The caller pairs it with its own `Segments`.
pub struct Slab<C: Charge> {
    path: PathBuf,
    capacity_bytes: u64,
    segment_bytes: u64,
    max_record_bytes: usize,
    opened: RefCell<Option<OpenSlab>>,
    writes: Rc<Cell<usize>>,
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
            writes: Rc::new(Cell::new(0)),
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
        self.writes.get()
    }
    pub fn idle_bytes(&self) -> usize {
        self.idle_buffer.borrow().as_ref().map_or(0, |b| b.len())
    }
    pub fn reclaim_idle(&self) -> usize {
        self.idle_buffer.borrow_mut().take().map_or(0, |b| b.len())
    }
    pub fn fence_writes(&self) -> Operation<'_, (), Error> {
        Box::pin(std::future::poll_fn(|cx| {
            if self.writes.get() == 0 {
                std::task::Poll::Ready(Ok(()))
            } else {
                cx.waker().wake_by_ref();
                std::task::Poll::Pending
            }
        }))
    }
    pub fn alignment(&self) -> Result<Alignment> {
        self.opened
            .borrow()
            .as_ref()
            .map(|o| o.alignment)
            .ok_or(Error::Unavailable)
    }
    pub fn open(&self) -> Operation<'_, Alignment, Error> {
        Box::pin(async move { self.open_now() })
    }
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
        let parent = self.path.parent().filter(|p| !p.as_os_str().is_empty());
        #[cfg(feature = "simulation")]
        if let Some(sim) = uring_runtime::reactor::simulation::Simulation::current() {
            if let Some(parent) = parent {
                sim.create_dir_all(parent).map_err(|_| Error::Io)?;
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
                .map_err(direct_error)?;
            let handle = file.as_sim().expect("simulation descriptor");
            handle.lock().map_err(|_| Error::Unavailable)?;
            let stat = handle.stat().map_err(|_| Error::Io)?;
            let a = Alignment::new(
                stat.stx_dio_mem_align as usize,
                stat.stx_dio_offset_align as u64,
                stat.stx_dio_offset_align as usize,
            )?;
            self.validate_layout(a, stat.stx_size)?;
            if stat.stx_size == 0 {
                handle.set_len(self.capacity_bytes).map_err(|_| Error::Io)?;
            }
            *self.opened.borrow_mut() = Some(OpenSlab {
                file: Rc::new(file),
                alignment: a,
            });
            return Ok(a);
        }
        if let Some(parent) = parent {
            std::fs::create_dir_all(parent).map_err(|_| Error::Io)?;
        }
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .custom_flags(libc::O_DIRECT | libc::O_CLOEXEC | libc::O_NOFOLLOW)
            .open(&self.path)
            .map_err(direct_error)?;
        // SAFETY: flock synchronously borrows this live descriptor.
        if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
            return Err(Error::Unavailable);
        }
        let a = probe(&file)?;
        let size = file.metadata().map_err(|_| Error::Io)?.len();
        self.validate_layout(a, size)?;
        if size == 0 {
            file.set_len(self.capacity_bytes).map_err(|_| Error::Io)?;
        }
        *self.opened.borrow_mut() = Some(OpenSlab {
            file: Rc::new(file.into()),
            alignment: a,
        });
        Ok(a)
    }
    fn validate_layout(&self, a: Alignment, size: u64) -> Result<()> {
        if a.extent(0, self.max_record_bytes)?.length() as u64 > self.segment_bytes
            || !self.segment_bytes.is_multiple_of(a.offset())
            || !self.segment_bytes.is_multiple_of(a.length() as u64)
            || (size != 0 && size != self.capacity_bytes)
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
    fn submission(
        &self,
        extent: Extent,
        buffer: &AlignedBuffer<C>,
        lease: &SegmentLease,
    ) -> Result<Rc<Descriptor>> {
        let opened = self.opened.borrow();
        let slab = opened.as_ref().ok_or(Error::Unavailable)?;
        slab.alignment.check(extent, buffer)?;
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
                .set(self.writes.get().checked_add(1).ok_or(Error::Busy)?);
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
    pub fn replace_file_for_test(&self, file: File) {
        self.replace_descriptor_for_test(file.into());
    }
    /// Also accepts a virtual descriptor from the simulation backend.
    #[cfg(feature = "simulation")]
    pub fn replace_descriptor_for_test(&self, file: Descriptor) {
        self.opened.borrow_mut().as_mut().expect("open slab").file = Rc::new(file);
    }
}
struct WriteFence(Rc<Cell<usize>>);
impl Drop for WriteFence {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}
fn direct_error(error: std::io::Error) -> Error {
    match error.raw_os_error() {
        Some(libc::EINVAL | libc::EOPNOTSUPP | libc::ENOSYS) => Error::Unsupported,
        _ => Error::Io,
    }
}
fn probe(file: &File) -> Result<Alignment> {
    // SAFETY: statx output is initialized, the FD is live, and AT_EMPTY_PATH accepts an empty path.
    let mut stat: libc::statx = unsafe { std::mem::zeroed() };
    if unsafe {
        libc::statx(
            file.as_raw_fd(),
            c"".as_ptr(),
            libc::AT_EMPTY_PATH,
            libc::STATX_DIOALIGN,
            &mut stat,
        )
    } != 0
        || stat.stx_mask & libc::STATX_DIOALIGN == 0
    {
        return Err(Error::Unsupported);
    }
    Alignment::new(
        stat.stx_dio_mem_align as usize,
        stat.stx_dio_offset_align as u64,
        stat.stx_dio_offset_align as usize,
    )
}

#[cfg(test)]
mod tests;
