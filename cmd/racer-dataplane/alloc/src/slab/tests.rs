use super::*;
use crate::{Generation, SegmentId, Segments};

#[cfg(feature = "simulation")]
mod simulated {
    use super::*;
    use std::{
        future::Future,
        pin::Pin,
        task::{Context, Poll, Waker},
    };
    use uring_runtime::reactor::simulation::{Fault, Simulation};

    #[derive(Clone, Copy, Debug, PartialEq, Eq)]
    enum TestError {
        Alloc(Error),
        Runtime(uring_runtime::Error),
    }
    impl From<Error> for TestError {
        fn from(e: Error) -> Self {
            Self::Alloc(e)
        }
    }
    impl From<uring_runtime::Error> for TestError {
        fn from(e: uring_runtime::Error) -> Self {
            Self::Runtime(e)
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
    fn poll<T>(future: &mut Pin<Box<dyn Future<Output = T> + '_>>) -> Poll<T> {
        future
            .as_mut()
            .poll(&mut Context::from_waker(Waker::noop()))
    }
    fn drive<T>(
        reactor: &Reactor<TestScope, ()>,
        mut op: Pin<Box<dyn Future<Output = T> + '_>>,
    ) -> T {
        for _ in 0..100 {
            if let Poll::Ready(value) = poll(&mut op) {
                return value;
            }
            reactor.poll_budgeted(64).unwrap();
        }
        panic!("simulation did not complete in 100 turns");
    }
    fn setup<C: Charge>() -> (Slab<C>, Segments) {
        let slab = Slab::new(PathBuf::from("/virtual/slab.dat"), 8192, 4096, 512);
        let alignment = slab.open_now().unwrap();
        let segments = Segments::new(4096);
        segments.configure(8192, 2, alignment).unwrap();
        (slab, segments)
    }
    #[test]
    fn roundtrip_short_io_and_runtime_errors() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (slab, segments) = setup::<()>();
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        let (lease, extent) = segments.append(4096).unwrap();
        let mut buffer = slab.allocate(extent.length(), ()).unwrap();
        buffer.as_mut_slice().fill(42);
        let buffer = drive(
            &reactor,
            slab.write(&reactor, extent, buffer, lease, &TestScope),
        )
        .unwrap();
        assert_eq!(slab.writes_in_flight(), 0);
        drop(buffer);
        let read = || {
            slab.read(
                &reactor,
                extent,
                slab.allocate(extent.length(), ()).unwrap(),
                segments.lease(SegmentId(0), Generation(1)).unwrap(),
                &TestScope,
            )
        };
        let buffer = drive(&reactor, read()).unwrap();
        assert!(buffer.as_slice().iter().all(|b| *b == 42));
        drop(buffer);
        sim.inject("read", Fault::Short(512));
        assert!(matches!(
            drive(&reactor, read()),
            Err(TestError::Alloc(Error::Io))
        ));
        sim.inject("write", Fault::Short(512));
        let op = slab.write(
            &reactor,
            extent,
            slab.allocate(extent.length(), ()).unwrap(),
            segments.lease(SegmentId(0), Generation(1)).unwrap(),
            &TestScope,
        );
        assert!(matches!(
            drive(&reactor, op),
            Err(TestError::Alloc(Error::Io))
        ));
        assert_eq!(slab.writes_in_flight(), 0);
        sim.inject("read", Fault::Errno(libc::EIO));
        assert!(matches!(
            drive(&reactor, read()),
            Err(TestError::Runtime(uring_runtime::Error::Io))
        ));
        assert_eq!(reactor.in_flight(), 0);
        segments.begin_evict(SegmentId(0)).unwrap();
        segments.recycle(SegmentId(0)).unwrap();
    }
    #[test]
    fn abandoned_write_retains_lease_charges_and_counter_until_completion() {
        for cancel_first in [false, true] {
            let sim = Simulation::new();
            sim.set_cancel_first(cancel_first);
            let _environment = sim.enter();
            let (slab, segments) = setup::<CountingCharge>();
            let reactor = Reactor::<TestScope, ()>::new(16, ());
            let used = Rc::new(Cell::new(0));
            let (lease, extent) = segments.append(4096).unwrap();
            let mut buffer = slab
                .allocate(4096, CountingCharge::new(&used, 4096))
                .unwrap();
            buffer.as_mut_slice().fill(42);
            let extra = Rc::new(CountingCharge::new(&used, 512));
            let weak = Rc::downgrade(&extra);
            buffer.retain(extra);
            sim.inject("write", Fault::HoldCompletion(8));
            let mut op = slab.write(&reactor, extent, buffer, lease, &TestScope);
            assert!(poll(&mut op).is_pending());
            assert_eq!(slab.writes_in_flight(), 1);
            reactor.poll_budgeted(1).unwrap();
            drop(op);
            assert_eq!(slab.writes_in_flight(), 1);
            assert_eq!(slab.idle_bytes(), 0);
            assert_eq!(slab.reclaim_idle(), 0);
            assert!(weak.upgrade().is_some());
            assert_eq!(used.get(), 4608);
            segments.begin_evict(SegmentId(0)).unwrap();
            assert_eq!(segments.recycle(SegmentId(0)), Err(Error::Busy));
            let mut fence = slab.fence_writes();
            assert!(poll(&mut fence).is_pending());
            drive(&reactor, fence).unwrap();
            assert_eq!(slab.writes_in_flight(), 0);
            assert_eq!(reactor.in_flight(), 0);
            assert!(weak.upgrade().is_none());
            assert_eq!(used.get(), 4096);
            assert_eq!(slab.idle_bytes(), 4096);
            let reused = slab
                .allocate(4096, CountingCharge::new(&used, 4096))
                .unwrap();
            assert!(reused.as_slice().iter().all(|b| *b == 0));
            drop(reused);
            assert_eq!(slab.reclaim_idle(), 4096);
            assert_eq!(used.get(), 0);
            segments.recycle(SegmentId(0)).unwrap();
        }
    }
    #[test]
    fn abandoned_read_and_unpolled_write_release_at_the_correct_fence() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (slab, segments) = setup::<()>();
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        let (lease, extent) = segments.append(4096).unwrap();
        let op = slab.write(
            &reactor,
            extent,
            slab.allocate(4096, ()).unwrap(),
            lease,
            &TestScope,
        );
        drop(op);
        assert_eq!(slab.writes_in_flight(), 0);
        assert_eq!(reactor.in_flight(), 0);
        sim.inject("read", Fault::HoldCompletion(8));
        let mut op = slab.read(
            &reactor,
            extent,
            slab.allocate(4096, ()).unwrap(),
            segments.lease(SegmentId(0), Generation(1)).unwrap(),
            &TestScope,
        );
        assert!(poll(&mut op).is_pending());
        reactor.poll_budgeted(1).unwrap();
        drop(op);
        segments.begin_evict(SegmentId(0)).unwrap();
        assert_eq!(segments.recycle(SegmentId(0)), Err(Error::Busy));
        drive(&reactor, reactor.drain()).unwrap();
        segments.recycle(SegmentId(0)).unwrap();
    }
    #[test]
    fn simulation_open_lock_size_and_faults() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (slab, _) = setup::<()>();
        let conflicting = Slab::<()>::new(PathBuf::from("/virtual/slab.dat"), 8192, 4096, 512);
        assert_eq!(conflicting.open_now(), Err(Error::Unavailable));
        assert_eq!(slab.open_now(), slab.alignment());
        drop(slab);
        conflicting.open_now().unwrap();
        drop(conflicting);
        let wrong_size = Slab::<()>::new(PathBuf::from("/virtual/slab.dat"), 16384, 4096, 512);
        assert_eq!(wrong_size.open_now(), Err(Error::InvalidConfiguration));
        sim.inject("open", Fault::Errno(libc::EOPNOTSUPP));
        assert_eq!(wrong_size.open_now(), Err(Error::Unsupported));
    }
}

struct CountingCharge {
    used: Rc<Cell<usize>>,
    bytes: usize,
}
impl CountingCharge {
    fn new(used: &Rc<Cell<usize>>, bytes: usize) -> Self {
        used.set(used.get() + bytes);
        Self {
            used: used.clone(),
            bytes,
        }
    }
}
impl Charge for CountingCharge {
    fn covers(&self, bytes: usize) -> bool {
        self.bytes >= bytes
    }
}
impl Drop for CountingCharge {
    fn drop(&mut self) {
        self.used.set(self.used.get() - self.bytes);
    }
}

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

struct Directory(PathBuf);
impl Directory {
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
    fn drop(&mut self) {
        std::fs::remove_dir_all(&self.0).unwrap();
    }
}

#[test]
fn real_file_is_direct_aligned_and_sparse_without_reactor() {
    use std::os::unix::fs::MetadataExt;
    let directory = Directory::new();
    let path = directory.0.join("caller-chosen.dat");
    let make = || Slab::<()>::new(path.clone(), 64 * 1024 * 1024, 32 * 1024 * 1024, 1024);
    let conflicting = make();
    let slabs = make();
    assert!(!path.exists());
    let alignment = slabs.open_now().unwrap();
    let stat = std::fs::metadata(&path).unwrap();
    assert_eq!(stat.len(), slabs.capacity_bytes());
    assert!(stat.blocks() * 512 < stat.len());
    let opened = slabs.opened.borrow();
    let fd = opened.as_ref().unwrap().file.as_raw_fd();
    // SAFETY: descriptor and borrowed buffers remain live during each syscall.
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

#[test]
fn open_rejects_bad_layout_and_existing_size_without_truncating() {
    let directory = Directory::new();
    let path = directory.0.join("data");
    std::fs::write(&path, [42; 7]).unwrap();
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

#[test]
fn submission_checks_alignment_segment_and_capacity() {
    let directory = Directory::new();
    let slab = Slab::<()>::new(directory.0.join("data"), 8192, 4096, 512);
    let a = slab.open_now().unwrap();
    let segments = Segments::new(4096);
    segments.configure(12288, 3, a).unwrap();
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
    drop(segments.append(4096).unwrap());
    let (outside, extent) = segments.append(4096).unwrap();
    assert!(matches!(
        slab.submission(extent, &buffer, &outside),
        Err(Error::Corrupt)
    ));
    assert_eq!(
        segments.validate(SegmentId(2), Generation(1), &extent),
        Ok(())
    );
}
