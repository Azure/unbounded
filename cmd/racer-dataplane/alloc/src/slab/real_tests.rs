//! Real kernel smoke coverage, including capability-aware setup skips.
use super::*;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
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

fn kernel_available() -> bool {
    // Linux's io_uring_params is a fixed 120-byte input/output UAPI structure.
    // All zero input requests the baseline ring, without optional setup flags.
    let mut params = [0u64; 15];
    // SAFETY: initialized, aligned output with the exact UAPI size; successful
    // syscall returns a new uniquely owned descriptor.
    let fd = unsafe { libc::syscall(libc::SYS_io_uring_setup, 2u32, params.as_mut_ptr()) };
    if fd >= 0 {
        drop(unsafe { File::from_raw_fd(fd as i32) });
        return true;
    }
    let error = std::io::Error::last_os_error();
    match error.raw_os_error() {
        Some(libc::ENOSYS | libc::EOPNOTSUPP | libc::EPERM | libc::EACCES) => {
            capability_skip(&format!(
                "io_uring kernel capability/permission error: {error}"
            ));
            false
        }
        _ => panic!("unexpected io_uring setup failure: {error}"),
    }
}

fn drive<T>(
    reactor: &Reactor<TestScope, ()>,
    mut op: Operation<'_, T, TestError>,
) -> std::result::Result<T, TestError> {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
    loop {
        if let Poll::Ready(value) = op.as_mut().poll(&mut Context::from_waker(Waker::noop())) {
            return value;
        }
        reactor.poll_budgeted(64).unwrap();
        assert!(
            std::time::Instant::now() < deadline,
            "real slab I/O did not complete"
        );
        std::thread::yield_now();
    }
}

#[test]
fn io_uring_roundtrip_and_completion_fence() {
    if !kernel_available() {
        return;
    }
    let directory = Directory::new();
    let slab = Slab::<()>::new(directory.0.join("uring.dat"), 8192, 4096, 512);
    let segments = Segments::new(4096);
    if real_alignment(slab.open_configured(&segments)).is_none() {
        return;
    }
    let reactor = Reactor::<TestScope, ()>::new(16, ());
    // The independent setup probe already passed. Do not hide runtime init bugs.
    reactor.init().unwrap();
    let (lease, extent) = segments.append(4096).unwrap();
    let mut buffer = slab.allocate(extent.length(), ()).unwrap();
    buffer.as_mut_slice().fill(73);
    drop(
        drive(
            &reactor,
            slab.write(&reactor, extent, buffer, lease, &TestScope),
        )
        .unwrap(),
    );
    assert_eq!(slab.writes_in_flight(), 0);
    let mut fence = slab.fence_writes();
    assert_eq!(
        fence.as_mut().poll(&mut Context::from_waker(Waker::noop())),
        Poll::Ready(Ok(()))
    );
    let buffer = drive(
        &reactor,
        slab.read(
            &reactor,
            extent,
            slab.allocate(extent.length(), ()).unwrap(),
            segments.lease(SegmentId(0), Generation(1)).unwrap(),
            &TestScope,
        ),
    )
    .unwrap();
    assert!(buffer.as_slice().iter().all(|byte| *byte == 73));
    drop(buffer);
    assert_eq!(reactor.in_flight(), 0);
    segments.begin_evict(SegmentId(0)).unwrap();
    segments.recycle(SegmentId(0)).unwrap();
}

#[test]
fn unbound_and_failed_binding_reject_reads_and_writes_before_reactor_admission() {
    let directory = Directory::new();
    for failure in ["unbound", "wrong-geometry", "frozen-table"] {
        let slab = Slab::<()>::new(directory.0.join(failure), 8192, 4096, 512);
        let correct = Segments::new(4096);
        match failure {
            "unbound" => {
                if real_alignment(slab.open_now()).is_none() {
                    return;
                }
            }
            "wrong-geometry" => {
                let result = slab.open_configured(&Segments::new(8192));
                if result == Err(Error::Unsupported) {
                    let _ = real_alignment(result);
                    return;
                }
                assert_eq!(result, Err(Error::InvalidConfiguration));
            }
            "frozen-table" => {
                let frozen = correct.freeze().unwrap();
                let result = slab.open_configured(&correct);
                if result == Err(Error::Unsupported) {
                    let _ = real_alignment(result);
                    return;
                }
                assert_eq!(result, Err(Error::Busy));
                drop(frozen);
            }
            _ => unreachable!(),
        }
        let alignment = slab.alignment().unwrap();
        correct.configure(8192, 2, alignment).unwrap();
        let (lease, extent) = correct.append(4096).unwrap();
        let buffer = slab.allocate(4096, ()).unwrap();
        assert!(matches!(
            slab.submission(extent, &buffer, &lease),
            Err(Error::Unavailable)
        ));
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        let mut read = slab.read(&reactor, extent, buffer, lease, &TestScope);
        assert!(matches!(
            read.as_mut().poll(&mut Context::from_waker(Waker::noop())),
            Poll::Ready(Err(TestError::Alloc(Error::Unavailable)))
        ));
        drop(read);
        let mut write = slab.write(
            &reactor,
            extent,
            slab.allocate(4096, ()).unwrap(),
            correct.lease(SegmentId(0), Generation(1)).unwrap(),
            &TestScope,
        );
        assert!(matches!(
            write.as_mut().poll(&mut Context::from_waker(Waker::noop())),
            Poll::Ready(Err(TestError::Alloc(Error::Unavailable)))
        ));
        drop(write);
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(slab.writes_in_flight(), 0);
        // A failed startup attempt is recoverable only after explicit binding.
        slab.configure_segments(&correct).unwrap();
        assert!(
            slab.submission(
                extent,
                &slab.allocate(4096, ()).unwrap(),
                &correct.lease(SegmentId(0), Generation(1)).unwrap()
            )
            .is_ok()
        );
    }
}
