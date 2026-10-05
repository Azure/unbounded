use super::*;
use crate::{Generation, SegmentId, Segments};

#[path = "../../tests/support/mod.rs"]
mod support;
use support::CountingCharge;

#[cfg(feature = "simulation")]
mod simulated {
    use super::*;
    use support::simulated::{TestError, TestScope, drive, poll};
    use uring_runtime::reactor::simulation::{Fault, Simulation};

    fn setup<C: Charge>() -> (Slab<C>, Segments) {
        let slab = Slab::new(PathBuf::from("/virtual/slab.dat"), 8192, 4096, 512);
        let segments = Segments::new(4096);
        let _ = slab.open_configured(&segments).unwrap();
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
        let _ = conflicting.open_now().unwrap();
        drop(conflicting);
        let wrong_size = Slab::<()>::new(PathBuf::from("/virtual/slab.dat"), 16384, 4096, 512);
        assert_eq!(wrong_size.open_now(), Err(Error::InvalidConfiguration));
        sim.inject("open", Fault::Errno(libc::EOPNOTSUPP));
        assert_eq!(wrong_size.open_now(), Err(Error::Unsupported));
        sim.inject("open", Fault::Errno(libc::ENOSPC));
        assert_eq!(
            wrong_size.open_now(),
            Err(Error::SystemIo {
                operation: "open",
                errno: Some(libc::ENOSPC),
            })
        );
    }
    #[test]
    fn replacement_hooks_are_fallible_and_refuse_live_writes() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let replacement = || {
            sim.open(
                None,
                Path::new("/replacement"),
                libc::O_CREAT | libc::O_RDWR,
            )
            .unwrap()
        };
        let unopened = Slab::<()>::new(PathBuf::from("/unopened"), 8192, 4096, 512);
        assert_eq!(
            unopened.replace_descriptor_for_test(replacement()),
            Err(Error::Unavailable)
        );
        let (slab, segments) = setup::<()>();
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        let (lease, extent) = segments.append(4096).unwrap();
        sim.inject("write", Fault::HoldCompletion(8));
        let mut write = slab.write(
            &reactor,
            extent,
            slab.allocate(4096, ()).unwrap(),
            lease,
            &TestScope,
        );
        assert!(poll(&mut write).is_pending());
        assert_eq!(
            slab.replace_descriptor_for_test(replacement()),
            Err(Error::Busy)
        );
        drop(write);
        drive(&reactor, slab.fence_writes()).unwrap();
        slab.replace_descriptor_for_test(replacement()).unwrap();
    }
}

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
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn capability_skip(reason: &str) {
    assert_ne!(
        std::env::var("PAGE_ALLOC_REQUIRE_REAL_IO").as_deref(),
        Ok("1"),
        "PAGE_ALLOC_REQUIRE_REAL_IO=1 forbids capability skips: {reason}"
    );
    eprintln!("SKIP real slab test: {reason}");
}

/// Only an explicit unsupported-filesystem result may skip file coverage.
/// Permission, configuration, and all other system errors are test failures.
fn real_alignment(result: Result<Alignment>) -> Option<Alignment> {
    match result {
        Ok(alignment) => Some(alignment),
        Err(Error::Unsupported) => {
            capability_skip("filesystem does not support direct I/O geometry");
            None
        }
        Err(error) => panic!("unexpected real slab startup failure: {error}"),
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
    let Some(alignment) = real_alignment(slabs.open_now()) else {
        return;
    };
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

#[test]
fn fence_waiters_sleep_update_wakers_and_unregister_on_cancellation() {
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };
    use std::task::Wake;
    #[derive(Default)]
    struct WakeCount(AtomicUsize);
    impl Wake for WakeCount {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
        fn wake_by_ref(self: &Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }
    let slab = Slab::<()>::new(PathBuf::new(), 4096, 4096, 512);
    slab.writes.count.set(2);
    let first_write = WriteFence(slab.writes.clone());
    let last_write = WriteFence(slab.writes.clone());
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
    // New writes between wake and poll must not strand an existing waiter.
    slab.writes.count.set(1);
    let next = WriteFence(slab.writes.clone());
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
    // Missing components are created privately without following symlinks.
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
    // A retained lease still authorizes its captured range after eviction starts.
    if length < 4096 {
        drop(table.append(4096 - length).unwrap());
    }
    table.begin_evict(SegmentId(0)).unwrap();
    assert!(slab.submission(extent, &buffer, &lease).is_ok());
    assert_eq!(table.recycle(SegmentId(0)), Err(Error::Busy));
    drop(lease);
    table.recycle(SegmentId(0)).unwrap();
}

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

#[path = "real_tests.rs"]
mod real;
