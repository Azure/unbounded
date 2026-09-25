//! Worker-owned sparse slab, opened once with O_DIRECT and never scanned at startup.
use super::{
    direct::{AlignedBuffer, DirectAlignment, DirectExtent},
    segment::SegmentLease,
};
use crate::{
    error::{Error, Operation, Result},
    model::{
        identity::{CacheId, WorkerId},
        limits::ResourceClass,
        range::PAGE_BYTES,
    },
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
        reactor::Reactor,
    },
};
use std::{
    cell::{Cell, RefCell},
    fs::{File, OpenOptions},
    os::{
        fd::{AsRawFd, OwnedFd},
        unix::fs::OpenOptionsExt,
    },
    path::PathBuf,
    rc::Rc,
};
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct SlabId(pub u64);
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SlabLocation {
    pub slab: SlabId,
    pub extent: DirectExtent,
}
struct OpenSlab {
    file: Rc<OwnedFd>,
    alignment: DirectAlignment,
}
pub struct Slabs {
    worker: WorkerId,
    directory: PathBuf,
    reactor: Rc<Reactor>,
    slab_bytes: u64,
    segment_bytes: u64,
    opened: RefCell<Option<OpenSlab>>,
    admission: RefCell<Option<Rc<Admission>>>,
    writes: Rc<Cell<usize>>,
}
impl Slabs {
    pub fn new(
        worker: WorkerId,
        directory: PathBuf,
        reactor: Rc<Reactor>,
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
            admission: RefCell::new(None),
            writes: Rc::new(Cell::new(0)),
        }
    }
    pub fn set_admission(&self, admission: Rc<Admission>) {
        *self.admission.borrow_mut() = Some(admission);
    }
    pub fn owns_reservation(&self, reservation: &Reservation) -> bool {
        self.admission
            .borrow()
            .as_ref()
            .is_some_and(|admission| admission.owns(reservation))
    }
    pub fn reserve(
        &self,
        cache: Option<&CacheId>,
        class: ResourceClass,
        amount: usize,
    ) -> Result<Reservation> {
        self.admission
            .borrow()
            .as_ref()
            .ok_or(Error::Unavailable)?
            .reserve(cache, class, amount)
    }
    /// Existing accepted fills may complete after request admission closes.
    /// Completion admission still enforces the configured byte quota.
    pub(crate) fn reserve_staging(&self, length: usize, cache: &CacheId) -> Result<Reservation> {
        self.admission
            .borrow()
            .as_ref()
            .ok_or(Error::Unavailable)?
            .reserve_completion(Some(cache), ResourceClass::Ciphertext, length)
    }
    pub fn writes_in_flight(&self) -> usize {
        self.writes.get()
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
        std::fs::create_dir_all(&self.directory).map_err(|_| Error::Io)?;
        let path = self
            .directory
            .join(format!("worker-{}-slab-0.dat", self.worker.0));
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
        let size = file.metadata().map_err(|_| Error::Io)?.len();
        if size != 0 && size != self.slab_bytes {
            return Err(Error::InvalidConfiguration);
        }
        if size == 0 {
            file.set_len(self.slab_bytes).map_err(|_| Error::Io)?;
        }
        *self.opened.borrow_mut() = Some(OpenSlab {
            file: Rc::new(file.into()),
            alignment: a,
        });
        Ok(a)
    }
    fn submission(
        &self,
        location: SlabLocation,
        buffer: &AlignedBuffer,
        lease: &SegmentLease,
    ) -> Result<Rc<OwnedFd>> {
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
        self.alignment()?.allocate(
            length,
            self.reserve(cache, ResourceClass::Ciphertext, length)?,
        )
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
    use crate::runtime::reactor::IoBuffer;
    #[test]
    fn real_file_is_direct_aligned_and_sparse_without_reactor() {
        let directory = super::super::tests::Directory::new();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let slabs = Slabs::new(
            WorkerId(0),
            directory.0.clone(),
            Rc::new(Reactor::new(admission.clone())),
            64 * 1024 * 1024,
            32 * 1024 * 1024,
        );
        assert!(!directory.0.join("worker-0-slab-0.dat").exists());
        slabs.set_admission(admission.clone());
        let alignment = slabs.open_now().unwrap();
        let opened = slabs.opened.borrow();
        let fd = opened.as_ref().unwrap().file.as_raw_fd();
        // SAFETY: descriptor is open and owned above; these calls synchronously borrow buffers.
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
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        let conflicting = Slabs::new(
            WorkerId(0),
            directory.0.clone(),
            Rc::new(Reactor::new(admission)),
            64 * 1024 * 1024,
            32 * 1024 * 1024,
        );
        assert_eq!(conflicting.open_now(), Err(Error::Unavailable));
    }
}
