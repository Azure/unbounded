//! Slab use by the extent volume: offset-addressed I/O, volume binding, and sync.

use page_alloc::{Alignment, DevicePlacement, Error, Slab};
use std::{
    fs::File,
    future::Future,
    os::fd::FromRawFd,
    path::PathBuf,
    pin::Pin,
    sync::Arc,
    task::{Context, Poll, Waker},
};
use uring_runtime::{Scope, reactor::Reactor};

/// Allocator and runtime errors kept apart.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum TestError {
    Alloc(Error),

    Runtime(uring_runtime::Error),
}

impl From<Error> for TestError {
    /// Keep the allocator category.
    fn from(error: Error) -> Self {
        Self::Alloc(error)
    }
}

impl From<uring_runtime::Error> for TestError {
    /// Keep the runtime category.
    fn from(error: uring_runtime::Error) -> Self {
        Self::Runtime(error)
    }
}

/// Always-live scope.
#[derive(Clone)]
struct TestScope;

impl Scope for TestScope {
    type Error = TestError;

    /// Never cancel.
    fn check(&self) -> Result<(), TestError> {
        Ok(())
    }
}

/// Drive an operation with a bounded number of reactor turns or a deadline.
fn drive<T>(
    reactor: &Reactor<TestScope, ()>,
    mut operation: Pin<Box<dyn Future<Output = T> + '_>>,
) -> T {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
    loop {
        if let Poll::Ready(value) = operation
            .as_mut()
            .poll(&mut Context::from_waker(Waker::noop()))
        {
            return value;
        }
        reactor.poll_budgeted(64).unwrap();
        assert!(std::time::Instant::now() < deadline, "I/O did not complete");
    }
}

#[cfg(feature = "simulation")]
mod simulation {
    use super::*;
    use page_alloc::{Segments, volume::MAX_SLOTS};
    use std::rc::Rc;
    use uring_runtime::reactor::simulation::{Fault, Simulation};

    const SEGMENT: u64 = 8192;
    const PATH: &str = "/volume/slab";

    fn open(slots: u64) -> Slab<()> {
        let slab = Slab::new(PATH.into(), 4 * SEGMENT, SEGMENT, SEGMENT as usize);
        assert_eq!(slab.open_volume(slots).unwrap().offset(), 4096);
        slab
    }

    fn write(slab: &Slab<()>, reactor: &Reactor<TestScope, ()>, offset: u64, byte: u8) {
        let mut buffer = slab.allocate(4096, ()).unwrap();
        buffer.as_mut_slice().fill(byte);
        let _ = drive(
            reactor,
            slab.write_at(reactor, offset, buffer, (), &TestScope),
        )
        .unwrap();
    }

    fn read(slab: &Slab<()>, reactor: &Reactor<TestScope, ()>, offset: u64) -> Vec<u8> {
        let buffer = slab.allocate(4096, ()).unwrap();
        drive(
            reactor,
            slab.read_at(reactor, offset, buffer, (), &TestScope),
        )
        .unwrap()
        .as_slice()
        .to_vec()
    }

    /// Unsynced writes are lost on crash; synced writes survive.
    #[test]
    fn sync_makes_writes_durable() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        let offset = SEGMENT + 4096;

        let slab = open(4);
        // Make the file and its directory entry durable so only data is at stake.
        sim.disk().sync_all().unwrap();
        write(&slab, &reactor, offset, 7);
        assert!(read(&slab, &reactor, offset) == vec![7; 4096]);
        drop(slab);
        sim.disk().crash().unwrap();

        let slab = open(4);
        assert!(
            read(&slab, &reactor, offset) == vec![0; 4096],
            "unsynced write survived"
        );
        write(&slab, &reactor, offset, 9);
        drive(&reactor, slab.sync(&reactor, &TestScope)).unwrap();
        drop(slab);
        sim.disk().crash().unwrap();

        let slab = open(4);
        assert!(
            read(&slab, &reactor, offset) == vec![9; 4096],
            "synced write lost"
        );
    }

    /// A failed fsync reports the runtime error and keeps the data volatile.
    #[test]
    fn sync_failure_is_reported() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        let slab = open(4);
        sim.disk().sync_all().unwrap();
        write(&slab, &reactor, 0, 3);
        sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
        assert!(matches!(
            drive(&reactor, slab.sync(&reactor, &TestScope)),
            Err(TestError::Runtime(_))
        ));
        drop(slab);
        sim.disk().crash().unwrap();
        let slab = open(4);
        assert!(read(&slab, &reactor, 0) == vec![0; 4096]);
    }

    /// Volume binding and offset checks.
    #[test]
    fn volume_binding_and_range_checks() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let reactor = Reactor::<TestScope, ()>::new(16, ());
        let slab = Slab::<()>::new(PATH.into(), 4 * SEGMENT, SEGMENT, SEGMENT as usize);
        assert_eq!(
            drive(&reactor, slab.sync(&reactor, &TestScope)),
            Err(TestError::Alloc(Error::Unavailable))
        );
        for count in [0, 5, MAX_SLOTS + 1] {
            assert_eq!(slab.open_volume(count), Err(Error::InvalidConfiguration));
        }
        let buffer = slab.allocate(4096, ());
        assert!(buffer.is_ok());
        // Opened but not bound: offset I/O is unavailable.
        assert_eq!(
            drive(
                &reactor,
                slab.read_at(&reactor, 0, buffer.unwrap(), (), &TestScope)
            )
            .err(),
            Some(TestError::Alloc(Error::Unavailable))
        );
        assert!(slab.open_volume(3).is_ok());
        assert!(slab.open_volume(3).is_ok());
        assert_eq!(slab.open_volume(4), Err(Error::InvalidConfiguration));
        assert_eq!(
            slab.open_configured(&Segments::new(SEGMENT)),
            Err(Error::InvalidConfiguration)
        );
        for (offset, length, expected) in [
            (SEGMENT + 512, 4096, Error::InvalidConfiguration),
            (SEGMENT - 4096, 8192, Error::Corrupt),
            (3 * SEGMENT, 4096, Error::Corrupt),
            (u64::MAX - 4095, 4096, Error::Corrupt),
        ] {
            let buffer = slab.allocate(length, ()).unwrap();
            assert_eq!(
                drive(
                    &reactor,
                    slab.write_at(&reactor, offset, buffer, (), &TestScope)
                )
                .err(),
                Some(TestError::Alloc(expected)),
                "{offset} {length}"
            );
        }
        assert_eq!(slab.writes_in_flight(), 0);
        // A guard is held until completion.
        let guard = Rc::new(());
        let buffer = slab.allocate(SEGMENT as usize, ()).unwrap();
        let mut op = slab.write_at(&reactor, 2 * SEGMENT, buffer, guard.clone(), &TestScope);
        assert!(
            op.as_mut()
                .poll(&mut Context::from_waker(Waker::noop()))
                .is_pending()
        );
        assert_eq!(Rc::strong_count(&guard), 2);
        let _ = drive(&reactor, op).unwrap();
        assert_eq!(Rc::strong_count(&guard), 1);

        // A slab bound to a segment table cannot be bound as a volume.
        let other = Slab::<()>::new("/volume/other".into(), 4 * SEGMENT, SEGMENT, 4096);
        let _ = other.open_configured(&Segments::new(SEGMENT)).unwrap();
        assert_eq!(other.open_volume(4), Err(Error::InvalidConfiguration));
    }
}

/// Owned test directory under the crate's target directory.
struct Directory(PathBuf);

impl Directory {
    /// Create a unique directory.
    fn new() -> Self {
        static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let id = NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!("page-alloc-volume-{}-{id}", std::process::id()));
        std::fs::create_dir_all(&path).unwrap();
        Self(path)
    }
}

impl Drop for Directory {
    /// Remove the directory.
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// Report a capability skip, or fail when real I/O is required.
fn capability_skip(reason: &str) {
    assert_ne!(
        std::env::var("PAGE_ALLOC_REQUIRE_REAL_IO").as_deref(),
        Ok("1"),
        "PAGE_ALLOC_REQUIRE_REAL_IO=1 forbids capability skips: {reason}"
    );
    eprintln!("SKIP real slab test: {reason}");
}

/// Whether io_uring is usable on this host.
fn kernel_available() -> bool {
    let mut params = [0u64; 15];
    // SAFETY: initialized, aligned 120-byte UAPI output; success owns a new descriptor.
    let fd = unsafe { libc::syscall(libc::SYS_io_uring_setup, 2u32, params.as_mut_ptr()) };
    if fd >= 0 {
        drop(unsafe { File::from_raw_fd(fd as i32) });
        return true;
    }
    let error = std::io::Error::last_os_error();
    match error.raw_os_error() {
        Some(libc::ENOSYS | libc::EOPNOTSUPP | libc::EPERM | libc::EACCES) => {
            capability_skip(&format!("io_uring unavailable: {error}"));
            false
        }
        _ => panic!("unexpected io_uring setup failure: {error}"),
    }
}

/// Open a direct file of `bytes`, or skip when direct I/O is unsupported.
fn direct_file(path: &std::path::Path, bytes: u64) -> Option<(Arc<File>, Alignment)> {
    use std::os::unix::fs::OpenOptionsExt;
    let file = match std::fs::OpenOptions::new()
        .create_new(true)
        .read(true)
        .write(true)
        .custom_flags(libc::O_DIRECT)
        .open(path)
    {
        Ok(file) => file,
        Err(error) if error.raw_os_error() == Some(libc::EINVAL) => {
            capability_skip("filesystem does not support O_DIRECT");
            return None;
        }
        Err(error) => panic!("open: {error}"),
    };
    file.set_len(bytes).unwrap();
    // Probe alignment through a throwaway slab on the same filesystem.
    let probe = Slab::<()>::new(path.with_extension("probe"), 1 << 20, 1 << 20, 4096);
    match probe.open_volume(1) {
        Ok(alignment) => Some((Arc::new(file), alignment)),
        Err(Error::Unsupported) => {
            capability_skip("filesystem does not support direct I/O geometry");
            None
        }
        Err(error) => panic!("unexpected probe failure: {error}"),
    }
}

/// Real kernel: offset I/O on reordered device placements, then fsync.
#[test]
fn io_uring_device_volume_roundtrip_and_sync() {
    if !kernel_available() {
        return;
    }
    let directory = Directory::new();
    let segment = 1u64 << 20;
    let Some((file, alignment)) = direct_file(&directory.0.join("device"), 3 * segment) else {
        return;
    };
    let slab = Slab::<()>::from_devices(
        vec![
            DevicePlacement {
                file: file.clone(),
                offset: 2 * segment,
            },
            DevicePlacement {
                file: file.clone(),
                offset: 0,
            },
        ],
        segment,
        4096,
        alignment,
    )
    .unwrap();
    assert_eq!(slab.open_volume(2), Ok(alignment));
    let reactor = Reactor::<TestScope, ()>::new(16, ());
    reactor.init().unwrap();
    let length = alignment.extent(0, 4096).unwrap().length();
    let mut buffer = slab.allocate(length, ()).unwrap();
    buffer.as_mut_slice().fill(42);
    let _ = drive(
        &reactor,
        slab.write_at(&reactor, segment + 4096, buffer, (), &TestScope),
    )
    .unwrap();
    drive(&reactor, slab.sync(&reactor, &TestScope)).unwrap();
    let buffer = drive(
        &reactor,
        slab.read_at(
            &reactor,
            segment + 4096,
            slab.allocate(length, ()).unwrap(),
            (),
            &TestScope,
        ),
    )
    .unwrap();
    assert!(buffer.as_slice().iter().all(|&b| b == 42));
    // Logical segment 1 maps to physical offset 0.
    use std::os::unix::fs::FileExt;
    let plain = File::open(directory.0.join("device")).unwrap();
    let mut bytes = vec![0; length];
    plain.read_exact_at(&mut bytes, 4096).unwrap();
    assert!(bytes.iter().all(|&b| b == 42));
    assert_eq!(reactor.in_flight(), 0);
}
