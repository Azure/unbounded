//! Public runtime contracts across host placement, filesystem I/O, and admission.
//! Private completion-state permutations and pointer-provenance tests stay in source.

#[cfg(target_os = "linux")]
mod filesystem {
    //! Host filesystem publication, ownership, permissions, and cancellation fences.
    use std::{
        cell::Cell,
        ffi::CString,
        fs,
        ops::Deref,
        os::{
            fd::AsRawFd,
            unix::{
                ffi::OsStrExt,
                fs::{PermissionsExt, symlink},
            },
        },
        path::{Path, PathBuf},
        rc::Rc,
        sync::atomic::{AtomicUsize, Ordering},
        task::{Context, Poll},
        time::{Duration, Instant},
    };
    use uring_runtime::{
        Budget, Error, Operation, Result, Scope,
        reactor::{
            IoBuffer, Reactor as Core,
            filesystem::{
                operations::{Durability, Replacement, ReplacementError, publish_new},
                secure,
            },
        },
    };

    /// Caller-selected Linux pathname ceiling; not a private implementation import.
    const PATH_BYTES: usize = 4095;

    /// Public scope implementation with shared cancellation and a host deadline.
    #[derive(Clone)]
    struct RequestScope {
        deadline: Instant,

        canceled: Rc<Cell<bool>>,
    }

    impl RequestScope {
        /// Construct an independently cancelable request with the supplied deadline.
        fn new(deadline: Instant) -> Self {
            Self {
                deadline,
                canceled: Rc::new(Cell::new(false)),
            }
        }

        /// Request cancellation without directly releasing any submitted resource.
        fn cancel(&self) {
            self.canceled.set(true);
        }
    }

    impl Scope for RequestScope {
        /// Report cancellation and deadline failures using runtime errors.
        type Error = Error;

        /// Reject canceled requests and requests whose deadline has passed.
        fn check(&self) -> Result<()> {
            if self.canceled.get() {
                Err(Error::Cancelled)
            } else if Instant::now() >= self.deadline {
                Err(Error::DeadlineExceeded)
            } else {
                Ok(())
            }
        }
    }

    /// Count retained budget independently through the public accounting callback.
    struct CountingBudget(Rc<Cell<usize>>);

    /// Return the exact accounted amount when its owner is dropped.
    struct Charge {
        used: Rc<Cell<usize>>,

        amount: usize,
    }

    impl Drop for Charge {
        /// Release this charge from the shared budget counter.
        fn drop(&mut self) {
            self.used.set(self.used.get() - self.amount);
        }
    }

    impl Budget for CountingBudget {
        /// Retain the accounted amount until the charge is dropped.
        type Charge = Charge;

        /// Account for the requested amount and return its release guard.
        fn charge(&self, amount: usize) -> Result<Charge> {
            self.0.set(self.0.get() + amount);
            Ok(Charge {
                used: self.0.clone(),
                amount,
            })
        }
    }

    /// Public reactor plus externally observed budget usage, without private access.
    struct Reactor {
        core: Core<RequestScope, CountingBudget>,

        used: Rc<Cell<usize>>,
    }

    impl Deref for Reactor {
        /// Expose the wrapped public reactor for filesystem operations.
        type Target = Core<RequestScope, CountingBudget>;

        /// Borrow the wrapped reactor without changing its accounting.
        fn deref(&self) -> &Self::Target {
            &self.core
        }
    }

    impl Reactor {
        /// Fence current operations without closing admission for subsequent checks.
        fn file_fence(&self) -> Operation<'_, ()> {
            self.fence_matching(|_| true)
        }
    }

    /// Own a unique project-local directory and clean it after the test.
    struct Directory(PathBuf);

    impl Directory {
        /// Allocate a collision-free directory, also safe for isolated child processes.
        fn new() -> Self {
            static NEXT: AtomicUsize = AtomicUsize::new(0);
            let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!(
                    "filesystem-review-{}-{}",
                    std::process::id(),
                    NEXT.fetch_add(1, Ordering::Relaxed)
                ));
            fs::create_dir_all(&path).unwrap();
            Self(path)
        }
    }

    impl Drop for Directory {
        /// Remove the test directory and all of its contents.
        fn drop(&mut self) {
            fs::remove_dir_all(&self.0).unwrap();
        }
    }

    /// Encode a host pathname without lossy Unicode conversion.
    fn name(path: &Path) -> CString {
        CString::new(path.as_os_str().as_bytes()).unwrap()
    }

    /// Give each operation group a fresh five-second host deadline.
    fn scope() -> RequestScope {
        RequestScope::new(Instant::now() + Duration::from_secs(5))
    }

    /// Poll delivery without implicitly driving kernel progress.
    fn poll<T, E>(future: &mut Operation<'_, T, E>) -> Poll<Result<T, E>> {
        future
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }

    /// Explicitly drive bounded completion turns until the public operation resolves.
    fn drive<T, E>(reactor: &Reactor, mut future: Operation<'_, T, E>) -> Result<T, E> {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Poll::Ready(result) = poll(&mut future) {
                return result;
            }
            assert!(Instant::now() < deadline, "reactor failed to make progress");
            reactor.poll_budgeted(8).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }

    /// Skip only unsupported or policy-denied io_uring, preserving the required-kernel gate.
    fn kernel_reactor(capacity: usize) -> Option<Reactor> {
        match io_uring::IoUring::new(2) {
            Ok(ring) => drop(ring),
            Err(error)
                if matches!(
                    error.raw_os_error(),
                    Some(libc::ENOSYS | libc::EPERM | libc::EACCES)
                ) =>
            {
                assert_ne!(
                    std::env::var("RUNTIME_REQUIRE_IO_URING").as_deref(),
                    Ok("1"),
                    "RUNTIME_REQUIRE_IO_URING=1 but io_uring is unavailable: {error}"
                );
                eprintln!("io_uring kernel test unavailable: {error}");
                return None;
            }
            Err(error) => panic!("unexpected io_uring setup failure: {error}"),
        }
        let used = Rc::new(Cell::new(0));
        let reactor = Reactor {
            core: Core::new(capacity, CountingBudget(used.clone())),
            used,
        };
        reactor.init().unwrap();
        Some(reactor)
    }

    /// Preserve the distinction between runtime failures and access-policy rejection.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    enum SecureError {
        Runtime(Error),
        Access(secure::AccessError),
    }

    impl From<Error> for SecureError {
        /// Preserve the runtime failure without collapsing cancellation or I/O errors.
        fn from(error: Error) -> Self {
            Self::Runtime(error)
        }
    }

    impl From<secure::AccessError> for SecureError {
        /// Retain access-policy attribution independently of filesystem failures.
        fn from(error: secure::AccessError) -> Self {
            Self::Access(error)
        }
    }

    /// Adapt the existing public request policy to the secure helper error boundary.
    #[derive(Clone)]
    struct SecureScope(RequestScope);

    impl Scope for SecureScope {
        /// Both failure classes remain observable by the helper caller.
        type Error = SecureError;

        /// Reuse cancellation and deadline checks without changing their precedence.
        fn check(&self) -> Result<(), SecureError> {
            self.0.check().map_err(Into::into)
        }
    }

    /// Drive composed helper futures explicitly with a bounded host deadline.
    fn drive_secure<T, E>(
        reactor: &Core<SecureScope, ()>,
        future: impl std::future::Future<Output = Result<T, E>>,
    ) -> Result<T, E> {
        let mut future = Box::pin(future);
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Poll::Ready(result) = future
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            {
                return result;
            }
            assert!(
                Instant::now() < deadline,
                "secure helper failed to make progress"
            );
            reactor.poll_budgeted(8).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }

    /// Private helpers follow retained descriptors across ambient simulation changes.
    #[cfg(feature = "simulation")]
    #[test]
    fn secure_helpers_use_retained_backend_owner_after_environment_switch() {
        use uring_runtime::reactor::simulation::Simulation;

        let Some(_kernel) = kernel_reactor(4) else {
            return;
        };
        let root = Directory::new();
        let path = root.0.join("private");
        let request = SecureScope(scope());
        let host = Core::new(16, ());
        let host_dir =
            drive_secure(&host, secure::directory(&host, &path, true, true, &request)).unwrap();
        drive_secure(
            &host,
            secure::atomic_write(&host, &host_dir, "value", b"host", &request),
        )
        .unwrap();
        let sim = Simulation::new();
        let environment = sim.enter();
        let simulated = Core::new(16, ());
        let sim_dir = drive_secure(
            &simulated,
            secure::directory(&simulated, &path, true, true, &request),
        )
        .unwrap();
        drive_secure(
            &simulated,
            secure::atomic_write(&simulated, &sim_dir, "value", b"sim", &request),
        )
        .unwrap();

        // The host reactor and descriptors stay host-backed under this simulation guard.
        let reopened = drive_secure(
            &host,
            secure::directory(&host, &path, false, true, &request),
        )
        .unwrap();
        assert_eq!(
            drive_secure(
                &host,
                secure::read_at(&host, &reopened, "value", 4, true, &request)
            )
            .unwrap()
            .as_ref(),
            b"host"
        );
        let stat = drive_secure(&host, host.file_stat(host_dir.clone(), &request)).unwrap();
        assert_eq!(secure::check_private(&stat, false), Ok(()));
        let mut foreign = stat;
        foreign.stx_uid = stat.stx_uid.wrapping_add(1);
        assert_eq!(
            secure::check_private(&foreign, false),
            Err(secure::AccessError::PermissionDenied)
        );
        drop(environment);

        // Simulated owner zero must not be compared with the non-root host UID.
        let reopened = drive_secure(
            &simulated,
            secure::directory(&simulated, &path, false, true, &request),
        )
        .unwrap();
        assert_eq!(
            drive_secure(
                &simulated,
                secure::read_at(&simulated, &reopened, "value", 3, true, &request)
            )
            .unwrap()
            .as_ref(),
            b"sim"
        );
        sim.chmod(&path.join("value"), 0o644).unwrap();
        assert!(matches!(
            drive_secure(
                &simulated,
                secure::read_at(&simulated, &sim_dir, "value", 3, true, &request)
            ),
            Err(SecureError::Access(secure::AccessError::PermissionDenied))
        ));
        fs::set_permissions(path.join("value"), fs::Permissions::from_mode(0o644)).unwrap();
        let _environment = sim.enter();
        assert!(matches!(
            drive_secure(
                &host,
                secure::read_at(&host, &host_dir, "value", 4, true, &request)
            ),
            Err(SecureError::Access(secure::AccessError::PermissionDenied))
        ));
    }

    /// Exercise secure composition against the host and optional deterministic backend.
    #[test]
    fn secure_helpers_compose_private_publication_reads_and_removal() {
        let Some(_kernel) = kernel_reactor(4) else {
            return;
        };
        for simulated in [false, true] {
            if simulated && !cfg!(feature = "simulation") {
                continue;
            }
            #[cfg(feature = "simulation")]
            let sim = uring_runtime::reactor::simulation::Simulation::new();
            #[cfg(feature = "simulation")]
            let _environment = simulated.then(|| sim.enter());
            let root = Directory::new();
            let path = root.0.join("private");
            let request = SecureScope(scope());
            let r = Core::new(16, ());
            r.init().unwrap();
            let dir = drive_secure(&r, secure::directory(&r, &path, true, true, &request)).unwrap();
            drive_secure(
                &r,
                secure::atomic_write(&r, &dir, "value", b"secret", &request),
            )
            .unwrap();
            let output =
                drive_secure(&r, secure::read_at(&r, &dir, "value", 6, true, &request)).unwrap();
            assert_eq!(output.as_ref(), b"secret");
            assert!(matches!(
                drive_secure(&r, secure::read_at(&r, &dir, "value", 5, true, &request)),
                Err(SecureError::Runtime(Error::InvalidInput))
            ));
            assert!(matches!(
                drive_secure(
                    &r,
                    secure::read_at(&r, &dir, "value", 1024 * 1024 + 1, true, &request)
                ),
                Err(SecureError::Runtime(Error::Overloaded))
            ));
            assert!(matches!(
                drive_secure(&r, secure::read_at(&r, &dir, "../value", 6, true, &request)),
                Err(SecureError::Runtime(Error::InvalidInput))
            ));
            assert!(matches!(
                drive_secure(&r, secure::read_file(&r, dir.clone(), 6, false, &request)),
                Err(SecureError::Runtime(Error::InvalidInput))
            ));
            assert!(matches!(
                drive_secure(
                    &r,
                    secure::atomic_write(&r, &dir, "../value", b"bad", &request)
                ),
                Err(ReplacementError::BeforeRename(SecureError::Runtime(
                    Error::InvalidInput
                )))
            ));
            #[cfg(feature = "simulation")]
            if simulated {
                sim.symlink(Path::new("value"), &path.join("link")).unwrap();
                sim.chmod(&path.join("value"), 0o644).unwrap();
            }
            if !simulated {
                symlink("value", path.join("link")).unwrap();
                fs::set_permissions(path.join("value"), fs::Permissions::from_mode(0o644)).unwrap();
            }
            assert!(matches!(
                drive_secure(&r, secure::read_at(&r, &dir, "value", 6, true, &request)),
                Err(SecureError::Access(secure::AccessError::PermissionDenied))
            ));
            assert!(
                drive_secure(&r, secure::read_at(&r, &dir, "link", 6, false, &request)).is_err()
            );
            assert_eq!(
                drive_secure(&r, secure::read_path(&r, &path.join("link"), 6, &request))
                    .unwrap()
                    .as_ref(),
                b"secret"
            );
            #[cfg(feature = "simulation")]
            if simulated {
                sim.inject(
                    "rename",
                    uring_runtime::reactor::simulation::Fault::Errno(libc::EIO),
                )
                .unwrap();
                assert!(matches!(
                    drive_secure(
                        &r,
                        secure::atomic_write(&r, &dir, "value", b"next", &request)
                    ),
                    Err(ReplacementError::RenameUncertain(SecureError::Runtime(
                        Error::Os(libc::EIO)
                    )))
                ));
                assert_eq!(sim.read_file(&path.join("value")).unwrap(), b"secret");
            }
            drive_secure(&r, secure::atomic_write(&r, &dir, "value", b"", &request)).unwrap();
            assert!(
                drive_secure(&r, secure::read_at(&r, &dir, "value", 0, true, &request))
                    .unwrap()
                    .is_empty()
            );
            for _ in 0..2 {
                drive_secure(&r, secure::remove(&r, &dir, "value", &request)).unwrap();
            }
            assert!(matches!(
                drive_secure(&r, secure::read_at(&r, &dir, "value", 6, true, &request)),
                Err(SecureError::Runtime(Error::NotFound))
            ));
            assert_eq!(r.in_flight(), 0);
        }
    }

    /// Compare failed creation and anonymous-file semantics without namespace changes.
    #[cfg(feature = "simulation")]
    #[test]
    fn creation_flags_preserve_namespace_and_tmpfile_is_explicitly_unsupported() {
        use uring_runtime::reactor::simulation::Simulation;
        let Some(host) = kernel_reactor(16) else {
            return;
        };
        let root = Directory::new();
        let existing = root.0.join("existing");
        let missing = root.0.join("missing");
        fs::write(&existing, b"keep").unwrap();
        let request = scope();
        for path in [&missing, &existing, &root.0] {
            assert!(matches!(
                drive(
                    &host,
                    host.file_open(
                        None,
                        name(path),
                        libc::O_CREAT | libc::O_DIRECTORY | libc::O_RDWR,
                        0,
                        &request
                    )
                ),
                Err(Error::Os(libc::EINVAL))
            ));
        }
        assert!(!missing.exists());
        assert_eq!(fs::read(&existing).unwrap(), b"keep");
        let named = drive(
            &host,
            host.file_open(None, name(&existing), libc::O_RDONLY, 0, &request),
        )
        .unwrap();
        let stat = drive(&host, host.file_stat(named, &request)).unwrap();
        assert_eq!(stat.stx_mode as u32 & libc::S_IFMT, libc::S_IFREG);
        assert_eq!(stat.stx_nlink, 1);
        match drive(
            &host,
            host.file_open(
                None,
                name(&root.0),
                libc::O_TMPFILE | libc::O_RDWR,
                0,
                &request,
            ),
        ) {
            Ok(anonymous) => {
                let stat = drive(&host, host.file_stat(anonymous, &request)).unwrap();
                assert_eq!(stat.stx_mode as u32 & libc::S_IFMT, libc::S_IFREG);
                assert_eq!(stat.stx_nlink, 0);
            }
            Err(Error::Os(libc::EOPNOTSUPP)) => (),
            other => panic!("unexpected host O_TMPFILE result: {other:?}"),
        }
        assert_eq!(fs::read_dir(&root.0).unwrap().count(), 1);

        let sim = Simulation::new();
        sim.write_file(&existing, b"keep").unwrap();
        let directory_before = sim.metadata(&root.0).unwrap();
        let file_before = sim.metadata(&existing).unwrap();
        let _simulation = sim.enter();
        let used = Rc::new(Cell::new(0));
        let simulated = Reactor {
            core: Core::new(16, CountingBudget(used.clone())),
            used,
        };
        simulated.init().unwrap();
        for path in [&missing, &existing, &root.0] {
            assert!(matches!(
                drive(
                    &simulated,
                    simulated.file_open(
                        None,
                        name(path),
                        libc::O_CREAT | libc::O_DIRECTORY | libc::O_RDWR,
                        0,
                        &request
                    )
                ),
                Err(Error::Os(libc::EINVAL))
            ));
        }
        for flags in [
            libc::O_TMPFILE | libc::O_RDWR,
            libc::O_TMPFILE | libc::O_WRONLY,
        ] {
            assert!(matches!(
                drive(
                    &simulated,
                    simulated.file_open(None, name(&root.0), flags, 0, &request)
                ),
                Err(Error::Os(libc::EOPNOTSUPP))
            ));
        }
        assert_eq!(
            sim.metadata(&missing).unwrap_err().raw_os_error(),
            Some(libc::ENOENT)
        );
        assert_eq!(sim.metadata(&root.0).unwrap(), directory_before);
        assert_eq!(sim.metadata(&existing).unwrap(), file_before);
        assert_eq!(sim.read_file(&existing).unwrap(), b"keep");
        let named = drive(
            &simulated,
            simulated.file_open(None, name(&existing), libc::O_RDONLY, 0, &request),
        )
        .unwrap();
        let stat = drive(&simulated, simulated.file_stat(named, &request)).unwrap();
        assert_eq!(stat.stx_mode as u32 & libc::S_IFMT, libc::S_IFREG);
        assert_eq!(stat.stx_nlink, 1);
    }

    /// Check buffer progress, bounds, quota release, and empty reads.
    #[test]
    fn partial_completion_preserves_remaining_bytes_and_quota() {
        let Some(r) = kernel_reactor(16) else {
            return;
        };
        let baseline = r.used.get();
        let mut b = r.file_bytes(b"abcdef").unwrap();
        assert!(r.used.get() >= baseline + 6);
        b.advance(2).unwrap();
        assert_eq!(b.bytes().unwrap(), b"cdef");
        assert_eq!(b.prefix(2).unwrap(), b"cd");
        assert_eq!(b.prefix(5), Err(Error::Io));
        assert_eq!(b.advance(0), Err(Error::Io));
        assert_eq!(b.advance(5), Err(Error::Io));
        b.advance(4).unwrap();
        assert_eq!(b.remaining(), 0);
        drop(b);
        assert_eq!(r.used.get(), baseline);
        let scope = scope();
        let fd = drive(
            &r,
            r.file_open(
                None,
                CString::new("/dev/null").unwrap(),
                libc::O_RDONLY,
                0,
                &scope,
            ),
        )
        .unwrap();
        let completion =
            drive(&r, r.read_at(fd, 0, r.file_buffer(17).unwrap(), (), &scope)).unwrap();
        assert_eq!(completion.bytes, 0);
        assert_eq!(completion.buffer.remaining(), 17);
    }

    /// Verify abandoned operations retain resources until kernel completion is fenced.
    #[test]
    fn abandoned_open_and_stat_remain_owned_until_real_cqe_fences() {
        let Some(r) = kernel_reactor(16) else {
            return;
        };
        let scope = scope();
        let baseline = r.used.get();
        let mut open = r.file_open(
            None,
            CString::new("/").unwrap(),
            libc::O_RDONLY | libc::O_DIRECTORY,
            0,
            &scope,
        );
        for _ in 0..20 {
            assert!(poll(&mut open).is_pending());
        }
        assert_eq!(r.in_flight(), 1);
        drop(open);
        assert!(r.used.get() > baseline);
        drive(&r, r.file_fence()).unwrap();
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.used.get(), baseline);
        let fd = drive(
            &r,
            r.file_open(
                None,
                CString::new("/").unwrap(),
                libc::O_RDONLY | libc::O_DIRECTORY,
                0,
                &scope,
            ),
        )
        .unwrap();
        let weak = Rc::downgrade(&fd);
        let mut stat = r.file_stat(fd, &scope);
        assert!(poll(&mut stat).is_pending());
        scope.cancel();
        drop(stat);
        assert!(weak.upgrade().is_some());
        drive(&r, r.file_fence()).unwrap();
        assert!(weak.upgrade().is_none());
        assert_eq!(r.used.get(), baseline);
    }

    /// Check partial reads, abandoned opens, and canceled metadata requests on Linux.
    #[test]
    fn real_partial_read_and_abandoned_open_fence_own_resources() {
        let Some(r) = kernel_reactor(16) else {
            return;
        };
        let scope = RequestScope::new(Instant::now() + Duration::from_secs(10));
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!("control-fs-{}", std::process::id()));
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(&path, b"abc").unwrap();
        let filename = || CString::new(path.as_os_str().as_bytes()).unwrap();
        let baseline = r.used.get();
        let mut abandoned = r.file_open(None, filename(), libc::O_RDONLY, 0, &scope);
        assert!(poll(&mut abandoned).is_pending());
        for _ in 0..8 {
            assert!(poll(&mut abandoned).is_pending());
        }
        assert_eq!(r.in_flight(), 1);
        assert!(r.used.get() > baseline);
        drop(abandoned);
        assert!(r.used.get() > baseline);
        drive(&r, r.file_fence()).unwrap();
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.used.get(), baseline);
        let fd = drive(&r, r.file_open(None, filename(), libc::O_RDONLY, 0, &scope)).unwrap();
        let read = drive(
            &r,
            r.read_at(fd.clone(), 0, r.file_buffer(64).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read.bytes, 3);
        assert_eq!(read.buffer.prefix(3).unwrap(), b"abc");
        drop(read);
        let mut buffer = r.file_bytes(b"abcdef").unwrap();
        buffer.advance(2).unwrap();
        assert_eq!(buffer.bytes().unwrap(), b"cdef");
        assert_eq!(buffer.advance(0), Err(Error::Io));
        assert_eq!(buffer.advance(5), Err(Error::Io));
        drop(buffer);
        let canceled = RequestScope::new(scope.deadline);
        let mut stat = r.file_stat(fd.clone(), &canceled);
        assert!(poll(&mut stat).is_pending());
        canceled.cancel();
        assert!(matches!(drive(&r, stat), Err(Error::Cancelled)));
        drop(fd);
        fs::remove_file(path).unwrap();
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.used.get(), baseline);
    }

    /// Verify publication contains staging paths and preserves permissions and symlinks.
    #[test]
    fn host_publication_containment_permissions_and_symlink_policy() {
        let root = Directory::new();
        let directory = root.0.join("private");
        let target = directory.join("target");
        let outside = root.0.join("outside");
        for candidate in [
            outside.clone(),
            directory.join("child/stage"),
            directory.join("../stage"),
            target.clone(),
        ] {
            assert_eq!(
                publish_new(&directory, &target, b"secret", [candidate])
                    .unwrap_err()
                    .raw_os_error(),
                Some(libc::EINVAL)
            );
            assert!(!directory.exists());
        }
        assert!(publish_new(&directory, &outside, b"secret", [directory.join("stage")]).is_err());
        assert!(!directory.exists());
        publish_new(&directory, &target, b"secret", [directory.join("stage")]).unwrap();
        assert_eq!(fs::read(&target).unwrap(), b"secret");
        assert_eq!(
            fs::metadata(&directory).unwrap().permissions().mode() & 0o077,
            0
        );
        assert_eq!(
            fs::metadata(&target).unwrap().permissions().mode() & 0o177,
            0
        );
        fs::write(&outside, b"untouched").unwrap();
        let stage_link = directory.join("link-stage");
        symlink(&outside, &stage_link).unwrap();
        publish_new(
            &directory,
            &target,
            b"next",
            [stage_link.clone(), directory.join("next")],
        )
        .unwrap();
        assert!(
            fs::symlink_metadata(stage_link)
                .unwrap()
                .file_type()
                .is_symlink()
        );
        assert_eq!(fs::read(&outside).unwrap(), b"untouched");
        let link = root.0.join("link");
        symlink(&directory, &link).unwrap();
        assert!(publish_new(&link, &link.join("target"), b"bad", [link.join("stage")]).is_err());
        assert_eq!(fs::read(&target).unwrap(), b"next");
    }

    /// Verify publication permissions under a restrictive umask in an isolated child.
    #[test]
    fn host_publication_honors_umask_in_isolated_process() {
        const CHILD: &str = "URING_FILESYSTEM_UMASK_CHILD";
        let Some(acknowledgment) = std::env::var_os(CHILD) else {
            let root = Directory::new();
            let acknowledgment = root.0.join("child-assertions-passed");
            let result = std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "filesystem::host_publication_honors_umask_in_isolated_process",
                    "--nocapture",
                ])
                .env(CHILD, &acknowledgment)
                .status()
                .unwrap();
            assert!(result.success());
            // A successful zero-test child must not masquerade as executed coverage.
            assert_eq!(fs::read(&acknowledgment).unwrap(), b"assertions passed");
            return;
        };
        // Isolated single-test child avoids a process-wide umask race.
        let previous = unsafe { libc::umask(0o077) };
        let root = Directory::new();
        let directory = root.0.join("private");
        publish_new(
            &directory,
            &directory.join("target"),
            b"secret",
            [directory.join("stage")],
        )
        .unwrap();
        assert_eq!(
            fs::metadata(&directory).unwrap().permissions().mode() & 0o777,
            0o700
        );
        assert_eq!(
            fs::metadata(directory.join("target"))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
        unsafe {
            libc::umask(previous);
        }
        fs::write(acknowledgment, b"assertions passed").unwrap();
    }

    /// Check secure path access and replacement against real Linux filesystem behavior.
    #[test]
    fn real_secure_traversal_magiclinks_hardlinks_cloexec_and_append_normalization() {
        let Some(r) = kernel_reactor(16) else {
            return;
        };
        let root = Directory::new();
        let request = scope();
        for flags in [libc::O_RDONLY, libc::O_WRONLY | libc::O_CREAT] {
            assert!(matches!(
                drive(
                    &r,
                    r.file_open(
                        None,
                        CString::new("x".repeat(4096)).unwrap(),
                        flags,
                        0,
                        &request
                    )
                ),
                Err(Error::InvalidInput)
            ));
            assert_eq!(r.in_flight(), 0);
        }
        let directory = drive(&r, r.file_directory(&root.0, false, PATH_BYTES, &request)).unwrap();
        let stat = drive(&r, r.file_stat(directory.clone(), &request)).unwrap();
        assert_eq!(stat.stx_mode as u32 & libc::S_IFMT, libc::S_IFDIR);
        let target = root.0.join("target");
        fs::write(&target, b"old").unwrap();
        symlink("target", root.0.join("link")).unwrap();
        assert!(
            drive(
                &r,
                r.file_open(
                    Some(directory.clone()),
                    CString::new("link").unwrap(),
                    libc::O_RDONLY,
                    secure::BENEATH | secure::NO_SYMLINKS,
                    &request
                )
            )
            .is_err()
        );
        let data = drive(
            &r,
            r.file_open(None, name(&target), libc::O_RDONLY, 0, &request),
        )
        .unwrap();
        assert_ne!(
            unsafe { libc::fcntl(data.as_raw_fd(), libc::F_GETFD) } & libc::FD_CLOEXEC,
            0
        );
        let magic = CString::new(format!("/proc/self/fd/{}", data.as_raw_fd())).unwrap();
        assert!(
            drive(
                &r,
                r.file_open(None, magic, libc::O_RDONLY, secure::NO_MAGICLINKS, &request)
            )
            .is_err()
        );
        let path_only = drive(
            &r,
            r.file_open(None, name(&target), libc::O_PATH, 0, &request),
        )
        .unwrap();
        assert_ne!(
            unsafe { libc::fcntl(path_only.as_raw_fd(), libc::F_GETFL) } & libc::O_PATH,
            0
        );
        assert_ne!(
            unsafe { libc::fcntl(path_only.as_raw_fd(), libc::F_GETFD) } & libc::FD_CLOEXEC,
            0
        );
        fs::hard_link(&target, root.0.join("alias")).unwrap();
        let stat = drive(&r, r.file_stat(data, &request)).unwrap();
        assert_eq!(stat.stx_nlink, 2);
        assert_eq!(
            secure::check_access(
                &stat,
                secure::AccessRequirements {
                    owner: stat.stx_uid,
                    forbidden_mode: 0,
                    links: Some(1)
                }
            ),
            Err(secure::AccessError::PermissionDenied)
        );
        let stage = drive(
            &r,
            r.file_open(
                Some(directory.clone()),
                CString::new("stage").unwrap(),
                libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL | libc::O_APPEND,
                secure::BENEATH | secure::NO_SYMLINKS,
                &request,
            ),
        )
        .unwrap();
        let make = || Replacement {
            directory: directory.clone(),
            staged: stage.clone(),
            temporary: CString::new("stage").unwrap(),
            target: CString::new("target").unwrap(),
            durability: Durability::FileAndDirectory,
        };
        fs::hard_link(root.0.join("stage"), root.0.join("stage-alias")).unwrap();
        assert_eq!(
            drive(
                &r,
                r.file_replace(make(), r.file_bytes(b"bad").unwrap(), &request)
            ),
            Err(ReplacementError::BeforeRename(Error::InvalidInput))
        );
        fs::remove_file(root.0.join("stage-alias")).unwrap();
        drive(
            &r,
            r.file_replace_chunked(
                make(),
                b"replacement",
                std::num::NonZeroUsize::new(2).unwrap(),
                &request,
            ),
        )
        .unwrap();
        assert_eq!(fs::read(&target).unwrap(), b"replacement");
        assert_eq!(fs::read(root.0.join("alias")).unwrap(), b"old");
        let invalid = root.0.join("must-not-exist/../child");
        assert!(drive(&r, r.file_directory(&invalid, true, PATH_BYTES, &request)).is_err());
        assert!(!root.0.join("must-not-exist").exists());
    }
}

#[cfg(target_os = "linux")]
mod affinity {
    //! Thread-local affinity and discovered topology checked against Linux.
    use std::{collections::BTreeSet, fs, path::Path, sync::mpsc, thread, time::Duration};
    use uring_runtime::{
        Error,
        group::affinity::{EffectiveTopology, current_cpus, pin_cpu, set_cpus},
    };

    /// Decode procfs independently of the runtime's syscall reader.
    fn proc_cpus() -> BTreeSet<usize> {
        let status = fs::read_to_string("/proc/thread-self/status").unwrap();
        let mask = status
            .lines()
            .find_map(|line| line.strip_prefix("Cpus_allowed:"))
            .unwrap();
        mask.trim()
            .split(',')
            .rev()
            .enumerate()
            .flat_map(|(word, hex)| {
                let bits = u32::from_str_radix(hex, 16).unwrap();
                (0..32)
                    .filter(move |bit| bits & (1 << bit) != 0)
                    .map(move |bit| word * 32 + bit)
            })
            .collect()
    }

    /// Verify affinity changes agree with Linux and leave another thread unchanged.
    #[test]
    fn affinity_round_trips_through_linux_without_affecting_another_thread() {
        let original = proc_cpus();
        assert!(!original.is_empty());
        assert_eq!(current_cpus().unwrap(), original);
        let (pinned_tx, pinned_rx) = mpsc::channel();
        let (resume_tx, resume_rx) = mpsc::channel();
        let worker = thread::spawn(move || {
            let allowed = proc_cpus();
            let cpu = *allowed.first().unwrap();
            pin_cpu(cpu).unwrap();
            assert_eq!(proc_cpus(), BTreeSet::from([cpu]));
            assert_eq!(current_cpus().unwrap(), proc_cpus());
            // SAFETY: sched_getcpu has no pointer arguments or preconditions.
            assert_eq!(unsafe { libc::sched_getcpu() }, cpu as i32);
            pinned_tx.send(()).unwrap();
            resume_rx.recv_timeout(Duration::from_secs(5)).unwrap();
            if allowed.len() > 1 {
                let pair = BTreeSet::from([cpu, *allowed.last().unwrap()]);
                set_cpus(&pair).unwrap();
                assert_eq!(proc_cpus(), pair);
                assert_eq!(current_cpus().unwrap(), pair);
                assert_eq!(
                    EffectiveTopology::discover()
                        .unwrap()
                        .cpus
                        .iter()
                        .map(|c| c.cpu)
                        .collect::<BTreeSet<_>>(),
                    pair
                );
            } else {
                eprintln!("single allowed CPU: multi-CPU affinity check not applicable");
            }
            set_cpus(&allowed).unwrap();
            assert_eq!(proc_cpus(), allowed);
            assert_eq!(current_cpus().unwrap(), allowed);
        });
        pinned_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        assert_eq!(proc_cpus(), original);
        assert_eq!(current_cpus().unwrap(), original);
        resume_tx.send(()).unwrap();
        worker.join().unwrap();
    }

    /// Verify a rejected CPU selection leaves the thread's affinity unchanged.
    #[test]
    fn kernel_rejected_affinity_preserves_the_mask() {
        thread::spawn(|| {
            let allowed = proc_cpus();
            let possible = fs::read_to_string("/sys/devices/system/cpu/possible").unwrap();
            let absent = possible
                .trim()
                .split([',', '-'])
                .map(|n| n.parse::<usize>().unwrap())
                .max()
                .unwrap()
                + 1;
            assert!(
                absent <= 1_048_575,
                "host leaves no representable absent CPU"
            );
            assert_eq!(pin_cpu(absent), Err(Error::Io));
            assert_eq!(proc_cpus(), allowed);
            assert_eq!(current_cpus().unwrap(), allowed);
        })
        .join()
        .unwrap();
    }

    /// Compare discovered CPU and network device topology with host sysfs data.
    #[test]
    fn discovered_topology_matches_host_sysfs() {
        let allowed = proc_cpus();
        let topology = EffectiveTopology::discover().unwrap();
        assert_eq!(
            topology.cpus.iter().map(|c| c.cpu).collect::<BTreeSet<_>>(),
            allowed
        );
        for cpu in &topology.cpus {
            let path = Path::new("/sys/devices/system/cpu").join(format!("cpu{}", cpu.cpu));
            for (file, actual) in [("physical_package_id", cpu.package), ("core_id", cpu.core)] {
                assert_eq!(
                    actual,
                    fs::read_to_string(path.join("topology").join(file))
                        .unwrap()
                        .trim()
                        .parse::<usize>()
                        .unwrap()
                );
            }
            let nodes: BTreeSet<usize> = fs::read_dir(&path)
                .unwrap()
                .map(|e| e.unwrap().file_name())
                .filter_map(|name| name.to_str()?.strip_prefix("node")?.parse().ok())
                .collect();
            assert_eq!(cpu.numa_node, nodes.first().copied());
        }
        let mut expected = Vec::new();
        for directory in ["/sys/class/net", "/sys/class/infiniband"] {
            if !Path::new(directory).exists() {
                continue;
            }
            for entry in fs::read_dir(directory).unwrap() {
                let entry = entry.unwrap();
                let node = match fs::read_to_string(entry.path().join("device/numa_node")) {
                    Ok(value) => usize::try_from(value.trim().parse::<i64>().unwrap()).ok(),
                    Err(e) if e.kind() == std::io::ErrorKind::NotFound => None,
                    Err(e) => panic!("{}: {e}", entry.path().display()),
                };
                expected.push((entry.file_name().to_string_lossy().into_owned(), node));
            }
        }
        let mut actual: Vec<_> = topology
            .nics
            .into_iter()
            .map(|nic| (nic.device, nic.numa_node))
            .collect();
        expected.sort();
        actual.sort();
        assert_eq!(actual, expected);
        assert_eq!(proc_cpus(), allowed);
        assert_eq!(current_cpus().unwrap(), allowed);
    }
}

#[cfg(feature = "simulation")]
mod reserved_capacity {
    //! Prepaid capacity and unread reply ownership through public operations.
    use std::{
        rc::Rc,
        task::{Context, Poll},
    };
    use uring_runtime::{
        Error, Operation, Result, Scope,
        reactor::{Reactor, simulation::Simulation},
    };

    /// Keep cancellation and deadlines out of admission-only scenarios.
    #[derive(Clone)]
    struct OpenScope;

    impl Scope for OpenScope {
        /// Use the runtime error type for the always-open scope.
        type Error = Error;

        /// Allow every request without cancellation or deadline checks.
        fn check(&self) -> Result<()> {
            Ok(())
        }
    }

    /// Poll once without driving kernel execution or consuming unrelated replies.
    fn poll<T>(operation: &mut Operation<'_, T>) -> Poll<Result<T>> {
        operation
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }

    /// Complete accepted operations without consuming their waiting replies.
    fn complete(reactor: &Reactor<OpenScope, ()>) {
        for _ in 0..16 {
            if reactor.in_flight() == 0 {
                return;
            }
            reactor.poll_budgeted(1).unwrap();
        }
        panic!("simulated operation did not complete in 16 driver turns");
    }

    /// Verify reservations belong to one reactor and unread failures retain capacity.
    #[test]
    fn reserved_capacity_is_reactor_local_and_failed_replies_release_it_only_on_consumption() {
        let simulation = Simulation::new();
        let _os = simulation.enter();
        let owner = Reactor::new(1, ());
        let foreign = Reactor::new(1, ());
        let scope = OpenScope;
        let charge = Rc::new(());
        let retained_charge = Rc::downgrade(&charge);
        let capacity = owner.reserve_submissions(1, charge).unwrap();
        let (socket, peer) = simulation.socket_pair();
        let socket = Rc::new(socket);
        let mut wrong_owner = foreign.send_reserved(
            socket.clone(),
            foreign.file_bytes(b"wrong").unwrap(),
            capacity.clone(),
            &scope,
        );
        assert!(matches!(
            poll(&mut wrong_owner),
            Poll::Ready(Err(Error::InvalidConfiguration))
        ));
        drop(wrong_owner);
        assert_eq!(foreign.in_flight(), 0);
        simulation
            .inject(
                "send",
                uring_runtime::reactor::simulation::Fault::Errno(libc::EPIPE),
            )
            .unwrap();
        let mut failed = owner.send_reserved(
            socket.clone(),
            owner.file_bytes(b"failed").unwrap(),
            capacity.clone(),
            &scope,
        );
        assert!(poll(&mut failed).is_pending());
        complete(&owner);
        let mut excess = owner.send_reserved(
            socket.clone(),
            owner.file_bytes(b"excess").unwrap(),
            capacity.clone(),
            &scope,
        );
        assert!(matches!(
            poll(&mut excess),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop(excess);
        assert!(matches!(
            poll(&mut failed),
            Poll::Ready(Err(Error::Os(libc::EPIPE)))
        ));
        drop(failed);
        let mut send = owner.send_reserved(
            socket.clone(),
            owner.file_bytes(b"ok").unwrap(),
            capacity.clone(),
            &scope,
        );
        assert!(poll(&mut send).is_pending());
        complete(&owner);
        let mut bytes = [0; 8];
        assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
        assert_eq!(&bytes[..2], b"ok", "rejected sends must not publish bytes");
        drop(capacity);
        assert!(retained_charge.upgrade().is_some());
        drop(send);
        assert!(retained_charge.upgrade().is_none());
        let mut ordinary = owner.send(
            socket.clone(),
            owner.file_bytes(b"next").unwrap(),
            (),
            &scope,
        );
        assert!(poll(&mut ordinary).is_pending());
        complete(&owner);
        assert!(
            matches!(poll(&mut ordinary), Poll::Ready(Ok(completion)) if completion.bytes == 4)
        );
        drop(ordinary);
        assert_eq!(peer.try_recv(&mut bytes).unwrap(), 4);
        assert_eq!(&bytes[..4], b"next");
        assert_eq!(owner.in_flight(), 0);
        drop((socket, peer));
        assert_eq!(simulation.live_handles(), 0);
    }

    /// Verify completed receives and accepts hold reserved slots until replies are read.
    #[test]
    fn reserved_receive_and_accept_keep_slots_until_reply_consumption() {
        use uring_runtime::reactor::{SocketAddress, simulation::Fault};
        let simulation = Simulation::new();
        let _os = simulation.enter();
        let reactor = Reactor::new(1, ());
        let capacity = reactor.reserve_submissions(1, ()).unwrap();
        let scope = OpenScope;
        let (socket, peer) = simulation.socket_pair();
        let socket = Rc::new(socket);
        simulation.inject("recv", Fault::HoldCompletion(4)).unwrap();
        let mut recv = reactor.recv_reserved(
            socket.clone(),
            reactor.file_bytes(&[0; 8]).unwrap(),
            capacity.clone(),
            &scope,
        );
        assert!(poll(&mut recv).is_pending());
        peer.try_send(b"ok").unwrap();
        reactor.poll_budgeted(1).unwrap();
        assert_eq!(reactor.in_flight(), 1);
        complete(&reactor);
        let mut blocked = reactor.recv_reserved(
            socket.clone(),
            reactor.file_bytes(&[0]).unwrap(),
            capacity.clone(),
            &scope,
        );
        assert!(matches!(
            poll(&mut blocked),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert!(matches!(poll(&mut recv), Poll::Ready(Ok(result)) if result.bytes == 2));
        drop((recv, blocked));
        let address = SocketAddress::Inet("127.0.0.1:18080".parse().unwrap());
        let listener = Rc::new(simulation.listen(address.clone()).unwrap());
        let mut accept = reactor.accept_reserved(listener.clone(), Some(capacity.clone()), &scope);
        assert!(poll(&mut accept).is_pending());
        let client = simulation.connect(address).unwrap();
        complete(&reactor);
        let mut blocked = reactor.accept_reserved(listener, Some(capacity), &scope);
        assert!(matches!(
            poll(&mut blocked),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert!(matches!(poll(&mut accept), Poll::Ready(Ok(_))));
        drop((accept, blocked, client, socket, peer));
        assert_eq!(simulation.live_handles(), 0);
    }

    /// Verify simulated sends use shared buffer access without requesting mutable access.
    #[test]
    fn simulated_writes_use_immutable_accessor_like_production() {
        use uring_runtime::reactor::IoBuffer;

        /// Stable send storage that rejects all mutable access.
        struct ReadOnly(Vec<u8>);

        // SAFETY: private stable Vec; mutation accessor fails without exposing aliases.
        unsafe impl IoBuffer for ReadOnly {
            /// Report attempts to mutate the read-only buffer as runtime errors.
            type Error = Error;

            /// Borrow the initialized bytes for sending.
            fn bytes(&self) -> Result<&[u8]> {
                Ok(&self.0)
            }

            /// Reject mutable access so the test detects an incorrect send accessor.
            fn bytes_mut(&mut self) -> Result<&mut [u8]> {
                Err(Error::InvalidInput)
            }
        }
        let simulation = Simulation::new();
        let _os = simulation.enter();
        let reactor = Reactor::new(1, ());
        let capacity = reactor.reserve_submissions(1, ()).unwrap();
        let scope = OpenScope;
        let (socket, peer) = simulation.socket_pair();
        let mut send =
            reactor.send_reserved(Rc::new(socket), ReadOnly(b"ok".to_vec()), capacity, &scope);
        assert!(poll(&mut send).is_pending());
        complete(&reactor);
        assert!(matches!(poll(&mut send), Poll::Ready(Ok(result)) if result.bytes == 2));
        let mut bytes = [0; 2];
        assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
        assert_eq!(&bytes, b"ok");
    }

    /// Verify reserved failures and unread accepts neither block ordinary sends nor leak handles.
    #[test]
    fn reserved_failures_and_unread_accepts_do_not_block_other_operations_or_leak_handles() {
        use uring_runtime::reactor::{SocketAddress, simulation::Fault};
        let simulation = Simulation::new();
        let _os = simulation.enter();
        let reactor = Reactor::new(3, ());
        let scope = OpenScope;
        let charge = Rc::new(());
        let weak = Rc::downgrade(&charge);
        let capacity = reactor.reserve_submissions(2, charge).unwrap();
        let address = SocketAddress::Unix("/reserved-errors".into());
        let listener = Rc::new(simulation.listen(address.clone()).unwrap());
        let (socket, peer) = simulation.socket_pair();
        let socket = Rc::new(socket);
        let baseline = simulation.live_handles();
        for accept_error in [false, true] {
            simulation
                .inject("recv", Fault::Errno(libc::ECONNRESET))
                .unwrap();
            if accept_error {
                simulation
                    .inject("accept", Fault::Errno(libc::EMFILE))
                    .unwrap();
            }
            let mut recv = reactor.recv_reserved(
                socket.clone(),
                reactor.file_buffer(4).unwrap(),
                capacity.clone(),
                &scope,
            );
            let mut accept =
                reactor.accept_reserved(listener.clone(), Some(capacity.clone()), &scope);
            assert!(poll(&mut recv).is_pending());
            assert!(poll(&mut accept).is_pending());
            let client = (!accept_error).then(|| simulation.connect(address.clone()).unwrap());
            complete(&reactor);
            let mut send = reactor.send(
                socket.clone(),
                reactor.file_bytes(b"ok").unwrap(),
                (),
                &scope,
            );
            assert!(poll(&mut send).is_pending());
            complete(&reactor);
            assert!(matches!(poll(&mut send), Poll::Ready(Ok(done)) if done.bytes == 2));
            let mut bytes = [0; 2];
            assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
            assert_eq!(&bytes, b"ok");
            let mut blocked = reactor.recv_reserved(
                socket.clone(),
                reactor.file_buffer(1).unwrap(),
                capacity.clone(),
                &scope,
            );
            assert!(matches!(
                poll(&mut blocked),
                Poll::Ready(Err(Error::Overloaded))
            ));
            assert_eq!(
                simulation.live_handles(),
                baseline + if accept_error { 0 } else { 2 }
            );
            assert!(matches!(
                poll(&mut recv),
                Poll::Ready(Err(Error::Os(libc::ECONNRESET)))
            ));
            if accept_error {
                assert!(matches!(
                    poll(&mut accept),
                    Poll::Ready(Err(Error::Os(libc::EMFILE)))
                ));
            }
            drop((recv, accept, send, blocked, client));
            assert_eq!(simulation.live_handles(), baseline);
        }
        drop((capacity, listener, socket, peer));
        assert!(weak.upgrade().is_none());
        assert_eq!(simulation.live_handles(), 0);
    }

    /// Verify rejected submissions release their reserved slot and owned buffer exactly once.
    #[test]
    fn reserved_sq_rejection_releases_exact_partition_and_buffer_owner() {
        use std::cell::Cell;
        use uring_runtime::reactor::IoBuffer;

        /// Count releases of stable submission storage independently of the reactor.
        struct Owned(Vec<u8>, Rc<Cell<usize>>);

        // SAFETY: private non-resizing Vec retains initialized backing across moves.
        unsafe impl IoBuffer for Owned {
            /// Use runtime errors for buffer access results.
            type Error = Error;

            /// Borrow the initialized bytes without transferring ownership.
            fn bytes(&self) -> Result<&[u8]> {
                Ok(&self.0)
            }

            /// Borrow the initialized bytes for an operation that needs mutable storage.
            fn bytes_mut(&mut self) -> Result<&mut [u8]> {
                Ok(&mut self.0)
            }
        }

        impl Drop for Owned {
            /// Record the release of this owned submission buffer.
            fn drop(&mut self) {
                self.1.set(self.1.get() + 1);
            }
        }
        let simulation = Simulation::new();
        let _os = simulation.enter();
        let reactor = Reactor::new(2, ());
        let scope = OpenScope;
        let charge = Rc::new(());
        let charge_weak = Rc::downgrade(&charge);
        let capacity = reactor.reserve_submissions(1, charge).unwrap();
        let (socket, peer) = simulation.socket_pair();
        let socket = Rc::new(socket);
        let drops = Rc::new(Cell::new(0));
        let mut ordinary =
            reactor.recv(socket.clone(), reactor.file_buffer(4).unwrap(), (), &scope);
        assert!(poll(&mut ordinary).is_pending());
        simulation.reject_submissions(1).unwrap();
        let mut rejected = reactor.send_reserved(
            socket.clone(),
            Owned(b"bad".to_vec(), drops.clone()),
            capacity.clone(),
            &scope,
        );
        assert!(matches!(
            poll(&mut rejected),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert_eq!(drops.get(), 1);
        assert_eq!(reactor.in_flight(), 1);
        let mut excess = reactor.send(
            socket.clone(),
            reactor.file_bytes(b"no").unwrap(),
            (),
            &scope,
        );
        assert!(matches!(
            poll(&mut excess),
            Poll::Ready(Err(Error::Overloaded))
        ));
        let mut reserved = reactor.send_reserved(
            socket.clone(),
            Owned(b"ok".to_vec(), drops.clone()),
            capacity.clone(),
            &scope,
        );
        assert!(
            poll(&mut reserved).is_pending(),
            "same reserved slot must be reusable"
        );
        peer.try_send(b"in").unwrap();
        complete(&reactor);
        assert!(matches!(poll(&mut ordinary), Poll::Ready(Ok(done)) if done.bytes == 2));
        assert!(matches!(poll(&mut reserved), Poll::Ready(Ok(done)) if done.bytes == 2));
        assert_eq!(drops.get(), 2);
        let mut bytes = [0; 8];
        assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
        assert_eq!(&bytes[..2], b"ok");
        drop((ordinary, excess, rejected, reserved, capacity, socket, peer));
        assert!(charge_weak.upgrade().is_none());
        assert_eq!(simulation.live_handles(), 0);
    }
}
