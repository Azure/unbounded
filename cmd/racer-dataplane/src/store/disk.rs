//! Direct-I/O geometry, owned aligned buffers, and worker-local sparse slab access.
use super::catalog::SegmentLease;
use crate::runtime::reactor::Descriptor;
use crate::{
    error::{Error, Operation, Result},
    model::{CacheId, PAGE_BYTES, ResourceClass, WorkerId},
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
        reactor::{IoBuffer, Reactor},
    },
};
use std::{
    alloc::{Layout, alloc_zeroed, dealloc},
    cell::{Cell, RefCell},
    fs::{File, OpenOptions},
    os::{fd::AsRawFd, unix::fs::OpenOptionsExt},
    path::PathBuf,
    ptr::NonNull,
    rc::{Rc, Weak},
};
use zeroize::Zeroize;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DirectAlignment {
    memory: usize,
    offset: u64,
    length: usize,
}
pub struct AlignedBuffer {
    allocation: NonNull<u8>,
    layout: Layout,
    length: usize,
    reservation: Option<Reservation>,
    retained: Vec<std::rc::Rc<Reservation>>,
    pool: Weak<RefCell<Option<AlignedBuffer>>>,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DirectExtent {
    offset: u64,
    length: usize,
}
impl DirectExtent {
    pub fn checked(offset: u64, length: usize) -> Result<Self> {
        if length == 0 || offset.checked_add(length as u64).is_none() {
            return Err(Error::CorruptRecord);
        }
        Ok(Self { offset, length })
    }
    pub fn offset(self) -> u64 {
        self.offset
    }
    pub fn length(self) -> usize {
        self.length
    }
}
impl DirectAlignment {
    pub fn validate(memory: usize, offset: u64, length: usize) -> Result<Self> {
        if !memory.is_power_of_two() || offset == 0 || length == 0 || memory > isize::MAX as usize {
            return Err(Error::DirectIoUnsupported);
        }
        Ok(Self {
            memory,
            offset,
            length,
        })
    }
    pub fn memory(self) -> usize {
        self.memory
    }
    pub fn offset(self) -> u64 {
        self.offset
    }
    pub fn length(self) -> usize {
        self.length
    }
    pub fn extent(&self, offset: u64, logical_length: usize) -> Result<DirectExtent> {
        if !offset.is_multiple_of(self.offset) || logical_length == 0 {
            return Err(Error::InvalidConfiguration);
        }
        let offset_unit = usize::try_from(self.offset).map_err(|_| Error::InvalidConfiguration)?;
        let mut a = offset_unit;
        let mut b = self.length;
        while b != 0 {
            let r = a % b;
            a = b;
            b = r;
        }
        let unit = (offset_unit / a)
            .checked_mul(self.length)
            .ok_or(Error::InvalidConfiguration)?;
        let length = logical_length
            .checked_add(unit - 1)
            .and_then(|v| (v / unit).checked_mul(unit))
            .ok_or(Error::InvalidConfiguration)?;
        DirectExtent::checked(offset, length)
    }
    pub fn allocate(&self, length: usize, reservation: Reservation) -> Result<AlignedBuffer> {
        if length == 0
            || !length.is_multiple_of(self.length)
            || reservation.amount() < length
            || !matches!(reservation.class(), crate::model::ResourceClass::Ciphertext)
        {
            return Err(Error::InvalidConfiguration);
        }
        let layout = Layout::from_size_align(length, self.memory)
            .map_err(|_| Error::InvalidConfiguration)?;
        // SAFETY: valid nonzero layout; ownership is unique and Drop uses this layout.
        let allocation = NonNull::new(unsafe { alloc_zeroed(layout) }).ok_or(Error::Overloaded)?;
        Ok(AlignedBuffer {
            allocation,
            layout,
            length,
            reservation: Some(reservation),
            retained: Vec::new(),
            pool: Weak::new(),
        })
    }
    pub fn check(&self, extent: DirectExtent, buffer: &AlignedBuffer) -> Result<()> {
        if !(buffer.allocation.as_ptr() as usize).is_multiple_of(self.memory)
            || !extent.offset.is_multiple_of(self.offset)
            || !extent.length.is_multiple_of(self.length)
            || buffer.length != extent.length
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}
impl AlignedBuffer {
    pub(crate) fn pooled(mut self, pool: &Rc<RefCell<Option<Self>>>) -> Self {
        self.pool = Rc::downgrade(pool);
        self
    }
    pub(crate) fn rebind(&mut self, reservation: Reservation) -> Result<()> {
        reservation.validate(crate::model::ResourceClass::Ciphertext, self.length)?;
        self.reservation = Some(reservation);
        Ok(())
    }
    /// Additional accounting whose lifetime must include submitted kernel access.
    pub(crate) fn retain_charge(&mut self, charge: std::rc::Rc<Reservation>) {
        self.retained.push(charge);
    }
    pub fn len(&self) -> usize {
        self.length
    }
    pub fn is_empty(&self) -> bool {
        self.length == 0
    }
}
impl Drop for AlignedBuffer {
    fn drop(&mut self) {
        if self.reservation.is_none() {
            return;
        }
        self.bytes_mut().expect("owned aligned buffer").zeroize();
        self.retained.clear();
        if let Some(pool) = self.pool.upgrade() {
            if let Ok(mut idle) = pool.try_borrow_mut() {
                if idle.is_none() {
                    *idle = Some(Self {
                        allocation: self.allocation,
                        layout: self.layout,
                        length: self.length,
                        reservation: self.reservation.take(),
                        retained: Vec::new(),
                        pool: Weak::new(),
                    });
                    return;
                }
            }
        }
        // SAFETY: allocation is exclusively owned, with its original layout.
        unsafe { dealloc(self.allocation.as_ptr(), self.layout) };
    }
}
// SAFETY: explicitly allocated aligned backing remains owned and stable.
unsafe impl IoBuffer for AlignedBuffer {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        // SAFETY: initialized allocation remains live for this borrow.
        Ok(unsafe { std::slice::from_raw_parts(self.allocation.as_ptr(), self.length) })
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        // SAFETY: exclusive borrow prevents mutable aliases.
        Ok(unsafe { std::slice::from_raw_parts_mut(self.allocation.as_ptr(), self.length) })
    }
}

#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct SlabId(pub u64);
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SlabLocation {
    pub slab: SlabId,
    pub extent: DirectExtent,
}
struct OpenSlab {
    file: Rc<Descriptor>,
    alignment: DirectAlignment,
}
pub struct Slabs {
    worker: WorkerId,
    directory: PathBuf,
    reactor: Rc<Reactor>,
    slab_bytes: u64,
    segment_bytes: u64,
    opened: RefCell<Option<OpenSlab>>,
    admission: Rc<Admission>,
    writes: Rc<Cell<usize>>,
    idle_buffer: Rc<RefCell<Option<AlignedBuffer>>>,
}
impl Slabs {
    pub fn new(
        worker: WorkerId,
        directory: PathBuf,
        reactor: Rc<Reactor>,
        admission: Rc<Admission>,
        slab_bytes: u64,
        segment_bytes: u64,
    ) -> Self {
        Self {
            worker,
            directory,
            reactor,
            slab_bytes,
            segment_bytes,
            opened: RefCell::new(None),
            admission,
            writes: Rc::new(Cell::new(0)),
            idle_buffer: Rc::new(RefCell::new(None)),
        }
    }
    pub(super) fn validate_admission(&self, admission: &Rc<Admission>) -> Result<()> {
        if Rc::ptr_eq(&self.admission, admission) {
            Ok(())
        } else {
            Err(Error::InvalidConfiguration)
        }
    }
    pub fn owns_reservation(&self, reservation: &Reservation) -> bool {
        self.admission.owns(reservation)
    }
    pub fn reserve(
        &self,
        cache: Option<&CacheId>,
        class: ResourceClass,
        amount: usize,
    ) -> Result<Reservation> {
        let result = self.admission.reserve(cache, class, amount);
        if matches!(result, Err(Error::Overloaded)) {
            self.idle_buffer.borrow_mut().take();
            return self.admission.reserve(cache, class, amount);
        }
        result
    }
    /// Existing accepted fills may complete after request admission closes.
    /// Completion admission still enforces the configured byte quota.
    pub(crate) fn reserve_staging(&self, length: usize, cache: &CacheId) -> Result<Reservation> {
        let result =
            self.admission
                .reserve_completion(Some(cache), ResourceClass::Ciphertext, length);
        if matches!(result, Err(Error::Overloaded)) {
            self.idle_buffer.borrow_mut().take();
            return self.admission.reserve_completion(
                Some(cache),
                ResourceClass::Ciphertext,
                length,
            );
        }
        result
    }
    pub fn writes_in_flight(&self) -> usize {
        self.writes.get()
    }
    pub fn retained_staging_bytes(&self) -> usize {
        self.idle_buffer
            .borrow()
            .as_ref()
            .map_or(0, |buffer| buffer.len())
    }
    #[cfg(test)]
    pub(super) fn replace_file_for_test(&self, file: File) {
        self.opened.borrow_mut().as_mut().unwrap().file = Rc::new(file.into());
    }
    pub fn fence_writes(&self) -> Operation<'_, ()> {
        Box::pin(std::future::poll_fn(|cx| {
            if self.writes.get() == 0 {
                std::task::Poll::Ready(Ok(()))
            } else {
                cx.waker().wake_by_ref();
                std::task::Poll::Pending
            }
        }))
    }
    pub fn slab_bytes(&self) -> u64 {
        self.slab_bytes
    }
    pub fn segment_bytes(&self) -> u64 {
        self.segment_bytes
    }
    pub fn alignment(&self) -> Result<DirectAlignment> {
        self.opened
            .borrow()
            .as_ref()
            .map(|o| o.alignment)
            .ok_or(Error::Unavailable)
    }
    pub fn open(&self) -> Operation<'_, DirectAlignment> {
        Box::pin(async move { self.open_now() })
    }
    pub fn open_now(&self) -> Result<DirectAlignment> {
        if let Ok(a) = self.alignment() {
            return Ok(a);
        }
        if self.segment_bytes == 0
            || self.slab_bytes == 0
            || !self.slab_bytes.is_multiple_of(self.segment_bytes)
            || self.slab_bytes > i64::MAX as u64
        {
            return Err(Error::InvalidConfiguration);
        }
        let path = self
            .directory
            .join(format!("worker-{}-slab-0.dat", self.worker.0));
        #[cfg(test)]
        if let Some(sim) = crate::runtime::reactor::simulation::Simulation::current() {
            sim.create_dir_all(&self.directory).map_err(|_| Error::Io)?;
            let file = sim
                .open(None, &path, libc::O_CREAT | libc::O_RDWR | libc::O_DIRECT)
                .map_err(|_| Error::Io)?;
            let Some(handle) = file.as_sim() else {
                unreachable!()
            };
            handle.lock().map_err(|_| Error::Unavailable)?;
            let stat = handle.stat().map_err(|_| Error::Io)?;
            let a = DirectAlignment::validate(
                stat.stx_dio_mem_align as usize,
                stat.stx_dio_offset_align as u64,
                stat.stx_dio_offset_align as usize,
            )?;
            self.validate_layout(a, stat.stx_size)?;
            if stat.stx_size == 0 {
                handle.set_len(self.slab_bytes).map_err(|_| Error::Io)?;
            }
            *self.opened.borrow_mut() = Some(OpenSlab {
                file: Rc::new(file),
                alignment: a,
            });
            return Ok(a);
        }
        std::fs::create_dir_all(&self.directory).map_err(|_| Error::Io)?;
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .custom_flags(libc::O_DIRECT | libc::O_CLOEXEC | libc::O_NOFOLLOW)
            .open(path)
            .map_err(direct_error)?;
        // Prevent accidental concurrent owners of this worker's slab.
        if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
            return Err(Error::Unavailable);
        }
        let a = discover(&file)?;
        let size = file.metadata().map_err(|_| Error::Io)?.len();
        self.validate_layout(a, size)?;
        if size == 0 {
            file.set_len(self.slab_bytes).map_err(|_| Error::Io)?;
        }
        *self.opened.borrow_mut() = Some(OpenSlab {
            file: Rc::new(file.into()),
            alignment: a,
        });
        Ok(a)
    }
    fn validate_layout(&self, a: DirectAlignment, size: u64) -> Result<()> {
        let largest = a
            .extent(
                0,
                PAGE_BYTES as usize + super::format::MAX_HEADER_BYTES + 16,
            )?
            .length() as u64;
        if largest > self.segment_bytes
            || !self.segment_bytes.is_multiple_of(a.offset())
            || !self.segment_bytes.is_multiple_of(a.length() as u64)
        {
            return Err(Error::InvalidConfiguration);
        }
        if size != 0 && size != self.slab_bytes {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
    fn submission(
        &self,
        location: SlabLocation,
        buffer: &AlignedBuffer,
        lease: &SegmentLease,
    ) -> Result<Rc<Descriptor>> {
        let opened = self.opened.borrow();
        let slab = opened.as_ref().ok_or(Error::Unavailable)?;
        slab.alignment.check(location.extent, buffer)?;
        let start = lease
            .id()
            .0
            .checked_mul(self.segment_bytes)
            .ok_or(Error::CorruptRecord)?;
        let end = location
            .extent
            .offset()
            .checked_add(location.extent.length() as u64)
            .ok_or(Error::CorruptRecord)?;
        if lease.worker() != self.worker
            || location.slab != SlabId(0)
            || location.extent.offset() < start
            || end
                > start
                    .checked_add(self.segment_bytes)
                    .ok_or(Error::CorruptRecord)?
            || end > self.slab_bytes
        {
            return Err(Error::CorruptRecord);
        }
        Ok(slab.file.clone())
    }
    pub fn read<'a>(
        &'a self,
        location: SlabLocation,
        buffer: AlignedBuffer,
        lease: SegmentLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, AlignedBuffer> {
        Box::pin(async move {
            let fd = self.submission(location, &buffer, &lease)?;
            let completion = self
                .reactor
                .read_at(fd, location.extent.offset(), buffer, lease, scope)
                .await?;
            if completion.bytes != location.extent.length() {
                return Err(Error::Io);
            }
            Ok(completion.buffer)
        })
    }
    pub fn write<'a>(
        &'a self,
        location: SlabLocation,
        buffer: AlignedBuffer,
        lease: SegmentLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, AlignedBuffer> {
        Box::pin(async move {
            let fd = self.submission(location, &buffer, &lease)?;
            self.writes
                .set(self.writes.get().checked_add(1).ok_or(Error::Overloaded)?);
            let fence = WriteFence(self.writes.clone());
            let completion = self
                .reactor
                .write_at(fd, location.extent.offset(), buffer, (lease, fence), scope)
                .await?;
            if completion.bytes != location.extent.length() {
                return Err(Error::Io);
            }
            Ok(completion.buffer)
        })
    }
    pub fn allocate(&self, length: usize, cache: Option<&CacheId>) -> Result<AlignedBuffer> {
        self.allocate_reserved(
            length,
            self.reserve(cache, ResourceClass::Ciphertext, length)?,
        )
    }
    pub(crate) fn allocate_reserved(
        &self,
        length: usize,
        reservation: Reservation,
    ) -> Result<AlignedBuffer> {
        if !self.owns_reservation(&reservation) {
            return Err(Error::InvalidConfiguration);
        }
        let idle = self.idle_buffer.borrow_mut().take();
        if let Some(mut buffer) = idle {
            if buffer.len() == length {
                buffer.rebind(reservation)?;
                return Ok(buffer.pooled(&self.idle_buffer));
            }
        }
        Ok(self
            .alignment()?
            .allocate(length, reservation)?
            .pooled(&self.idle_buffer))
    }
    pub(crate) fn reclaim_buffer(&self) -> usize {
        self.idle_buffer
            .borrow_mut()
            .take()
            .map_or(0, |buffer| buffer.len())
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
        Some(libc::EINVAL | libc::EOPNOTSUPP | libc::ENOSYS) => Error::DirectIoUnsupported,
        _ => Error::Io,
    }
}
fn discover(file: &File) -> Result<DirectAlignment> {
    // SAFETY: valid FD, empty C path with AT_EMPTY_PATH, initialized output storage.
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
    {
        return Err(Error::DirectIoUnsupported);
    }
    if stat.stx_mask & libc::STATX_DIOALIGN == 0 {
        return Err(Error::DirectIoUnsupported);
    }
    DirectAlignment::validate(
        stat.stx_dio_mem_align as usize,
        stat.stx_dio_offset_align as u64,
        stat.stx_dio_offset_align as usize,
    )
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn aligned_pool_reuses_only_fenced_zeroed_admitted_storage() {
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
        let cache = CacheId("pool".into());
        let pool = Rc::new(RefCell::new(None));
        let alignment = DirectAlignment::validate(512, 512, 512).unwrap();
        let charge = || {
            admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, 512)
                .unwrap()
        };
        let mut buffer = alignment.allocate(512, charge()).unwrap().pooled(&pool);
        let pointer = buffer.bytes().unwrap().as_ptr();
        buffer.bytes_mut().unwrap().fill(42);
        drop(buffer);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 512);
        let mut reused = pool.borrow_mut().take().unwrap();
        assert_eq!(reused.bytes().unwrap().as_ptr(), pointer);
        assert!(reused.bytes().unwrap().iter().all(|b| *b == 0));
        reused.rebind(charge()).unwrap();
        assert_eq!(admission.used(ResourceClass::Ciphertext), 512);
        drop(reused.pooled(&pool));
        drop(pool);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }
    #[test]
    fn geometry_rounds_without_assuming_page_size() {
        let a = DirectAlignment::validate(512, 512, 1024).unwrap();
        assert_eq!(a.extent(512, 1025).unwrap().length(), 2048);
        assert!(a.extent(1, 1).is_err());
        assert!(a.extent(0, usize::MAX).is_err());
        assert!(DirectAlignment::validate(3, 512, 512).is_err());
        assert!(DirectExtent::checked(u64::MAX, 1).is_err());
    }
    #[test]
    fn real_file_is_direct_aligned_and_sparse_without_reactor() {
        let directory = crate::store::tests::Directory::new();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let slabs = || {
            Slabs::new(
                WorkerId(0),
                directory.0.clone(),
                Rc::new(Reactor::new(admission.clone())),
                admission.clone(),
                64 * 1024 * 1024,
                32 * 1024 * 1024,
            )
        };
        let conflicting = slabs();
        let slabs = slabs();
        assert!(!directory.0.join("worker-0-slab-0.dat").exists());
        let alignment = slabs.open_now().unwrap();
        let opened = slabs.opened.borrow();
        let fd = opened.as_ref().unwrap().file.as_raw_fd();
        // SAFETY: descriptor is open and owned above; calls synchronously borrow buffers.
        assert_ne!(
            unsafe { libc::fcntl(fd, libc::F_GETFL) } & libc::O_DIRECT,
            0
        );
        let extent = alignment.extent(0, 31).unwrap();
        let mut buffer = slabs.allocate(extent.length(), None).unwrap();
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
        slabs.reclaim_buffer();
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        assert_eq!(conflicting.open_now(), Err(Error::Unavailable));
    }
}
