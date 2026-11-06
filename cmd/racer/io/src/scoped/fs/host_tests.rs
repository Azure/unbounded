//! Host filesystem publication, ownership, permissions, and cancellation fences.
use crate::{
    Error, Operation, Result, Scope,
    scoped::{
        IoBuffer, Reactor as Core,
        fs::{
            operations::{Durability, Replacement, ReplacementError},
            prelude::*,
            secure,
        },
    },
    sim::SimOs,
};
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

/// Public reactor with a test fence, without private access.
struct Reactor {
    core: Core<RequestScope>,
}

impl Deref for Reactor {
    /// Expose the wrapped public reactor for filesystem operations.
    type Target = Core<RequestScope>;

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
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .parent()
            .unwrap()
            .join("target")
            .join(format!(
                "filesystem-test-{}-{}",
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
    let reactor = Reactor {
        core: Core::new(capacity, &crate::Env::host()),
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
    reactor: &Core<SecureScope>,
    future: impl std::future::Future<Output = Result<T, E>>,
) -> Result<T, E> {
    let mut future = Box::pin(future);
    // Only a stuck-test bound: a contended disk can park one sync for seconds.
    let deadline = Instant::now() + Duration::from_secs(30);
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

/// Host and simulated reactors keep their own backend for retained descriptors.

#[test]
fn secure_helpers_use_retained_backend_owner_after_environment_switch() {
    let Some(_kernel) = kernel_reactor(4) else {
        return;
    };
    let root = Directory::new();
    let path = root.0.join("private");
    let request = SecureScope(scope());
    let host = Core::new(16, &crate::Env::host());
    let host_dir =
        drive_secure(&host, secure::directory(&host, &path, true, true, &request)).unwrap();
    drive_secure(
        &host,
        secure::atomic_write(&host, &host_dir, "value", b"host", &request),
    )
    .unwrap();
    let sim = SimOs::new();
    let simulated = Core::new(16, &crate::Env::sim(&sim, crate::WorkerCtx::host()));
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
        if simulated && !cfg!(test) {
            continue;
        }
        let sim = SimOs::new();
        let env = if simulated {
            crate::Env::sim(&sim, crate::WorkerCtx::host())
        } else {
            crate::Env::host()
        };
        let root = Directory::new();
        let path = root.0.join("private");
        let request = SecureScope(scope());
        let r = Core::new(16, &env);
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
        assert!(drive_secure(&r, secure::read_at(&r, &dir, "link", 6, false, &request)).is_err());
        assert_eq!(
            drive_secure(&r, secure::read_path(&r, &path.join("link"), 6, &request))
                .unwrap()
                .as_ref(),
            b"secret"
        );
        if simulated {
            sim.inject("rename", crate::sim::Fault::Errno(libc::EIO))
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

#[test]
fn creation_flags_preserve_namespace_and_tmpfile_is_explicitly_unsupported() {
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

    let sim = SimOs::new();
    sim.write_file(&existing, b"keep").unwrap();
    let directory_before = sim.metadata(&root.0).unwrap();
    let file_before = sim.metadata(&existing).unwrap();
    let simulated = Reactor {
        core: Core::new(16, &crate::Env::sim(&sim, crate::WorkerCtx::host())),
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
    let mut b = r.file_bytes(b"abcdef").unwrap();
    b.advance(2).unwrap();
    assert_eq!(b.bytes().unwrap(), b"cdef");
    assert_eq!(b.prefix(2).unwrap(), b"cd");
    assert_eq!(b.prefix(5), Err(Error::Io));
    assert_eq!(b.advance(0), Err(Error::Io));
    assert_eq!(b.advance(5), Err(Error::Io));
    b.advance(4).unwrap();
    assert_eq!(b.remaining(), 0);
    drop(b);
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
    let completion = drive(&r, r.read_at(fd, 0, r.file_buffer(17).unwrap(), (), &scope)).unwrap();
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
    drive(&r, r.file_fence()).unwrap();
    assert_eq!(r.in_flight(), 0);
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
    let mut abandoned = r.file_open(None, filename(), libc::O_RDONLY, 0, &scope);
    assert!(poll(&mut abandoned).is_pending());
    for _ in 0..8 {
        assert!(poll(&mut abandoned).is_pending());
    }
    assert_eq!(r.in_flight(), 1);
    drop(abandoned);
    drive(&r, r.file_fence()).unwrap();
    assert_eq!(r.in_flight(), 0);
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
