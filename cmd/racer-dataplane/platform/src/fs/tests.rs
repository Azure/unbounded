//! Filesystem tests: pure policy, buffers, and host behavior through io_uring.

use super::*;
use crate::reactor::tests::{TestReactor, drive, kernel_reactor, poll, scope};
use operations::{Durability, Replacement, ReplacementError};
use std::path::Path;

/// Keep cursor mutations bounded and release quota on allocation failure or drop.
#[test]
fn shared_allocation_preserves_cursor_and_releases_charge_on_failure() {
    let charge = Rc::new(());
    let weak = Rc::downgrade(&charge);
    let mut buffer = Buffer::allocate(4, Box::new(charge)).unwrap();
    assert_eq!(buffer.bytes().unwrap(), &[0; 4]);
    buffer.bytes_mut().unwrap().copy_from_slice(b"data");
    buffer.advance(2).unwrap();
    assert_eq!(buffer.prefix(2).unwrap(), b"ta");
    assert_eq!(buffer.advance(3), Err(Error::Io));
    assert_eq!(buffer.remaining(), 2);
    assert!(weak.upgrade().is_some());
    drop(buffer);
    assert!(weak.upgrade().is_none());

    let charge = Rc::new(());
    let weak = Rc::downgrade(&charge);
    assert!(matches!(
        Buffer::allocate(usize::MAX, Box::new(charge)),
        Err(Error::Overloaded)
    ));
    assert!(weak.upgrade().is_none());
    assert_eq!(Buffer::allocate(0, Box::new(())).unwrap().remaining(), 0);
}

fn runtime(errno: i32) -> Failure<Error> {
    Failure::Runtime(uring::Error::from_errno(errno))
}

/// Accept only the selected operation's idempotent errno, never scope failures.
#[test]
fn namespace_policy_is_bound_to_the_operation() {
    for (change, exists, absent) in [
        (
            NamespaceChange::EnsureDirectory,
            Ok(()),
            Err(Error::NotFound),
        ),
        (
            NamespaceChange::Unlink,
            Err(Error::AlreadyExists),
            Err(Error::NotFound),
        ),
        (
            NamespaceChange::RemoveIfPresent,
            Err(Error::AlreadyExists),
            Ok(()),
        ),
    ] {
        assert_eq!(change.complete::<Error>(Ok(())), Ok(()));
        assert_eq!(change.complete(Err(runtime(libc::EEXIST))), exists);
        assert_eq!(change.complete(Err(runtime(libc::ENOENT))), absent);
        assert_eq!(
            change.complete::<Error>(Err(runtime(libc::EIO))),
            Err(Error::Os(libc::EIO))
        );
        for error in [
            Error::Canceled,
            Error::DeadlineExceeded,
            Error::Overloaded,
            Error::AlreadyExists,
            Error::NotFound,
        ] {
            assert_eq!(
                change.complete::<Error>(Err(Failure::Scope(error))),
                Err(error)
            );
        }
        // Library cancellation carries no errno and is never accepted.
        assert_eq!(
            change.complete::<Error>(Err(Failure::Runtime(uring::Error::new(
                uring::ErrorKind::Canceled
            )))),
            Err(Error::Canceled)
        );
    }
}

/// Keep secure flags and Linux pathname bounds independent of service limits.
#[test]
fn flags_and_linux_path_bounds_are_not_application_limits() {
    assert_eq!(
        open_flags(libc::O_PATH),
        Ok((libc::O_PATH | libc::O_CLOEXEC, 0))
    );
    assert_eq!(
        open_flags(libc::O_PATH | libc::O_NONBLOCK),
        Err(Error::InvalidInput)
    );
    assert_eq!(
        open_flags(libc::O_PATH | libc::O_CREAT),
        Err(Error::InvalidInput)
    );
    assert_eq!(
        open_flags(libc::O_TMPFILE | libc::O_RDONLY),
        Err(Error::InvalidInput)
    );
    assert_eq!(
        open_flags(libc::O_TMPFILE | libc::O_WRONLY).unwrap().1,
        0o600
    );
    assert_eq!(open_flags(libc::O_CREAT | libc::O_WRONLY).unwrap().1, 0o600);
    assert_eq!(open_flags(libc::O_RDONLY).unwrap().1, 0);
    assert!(secure::path_name(std::ffi::OsStr::new(&"x".repeat(4095)), usize::MAX).is_ok());
    assert_eq!(
        secure::path_name(std::ffi::OsStr::new(&"x".repeat(4096)), usize::MAX),
        Err(Error::InvalidInput)
    );
}

/// A unique host directory, removed on drop.
struct TempDir(PathBuf);

impl TempDir {
    fn new(name: &str) -> Self {
        let path =
            std::env::temp_dir().join(format!("racer-platform-fs-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&path);
        Self(path)
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn mode(path: &Path) -> u32 {
    use std::os::unix::fs::PermissionsExt;
    std::fs::symlink_metadata(path)
        .unwrap()
        .permissions()
        .mode()
        & 0o777
}

/// Traversal creates private directories, pins them, and rejects escapes.
#[test]
fn host_directory_traversal_and_private_checks() {
    let Some(r) = kernel_reactor(8) else {
        return;
    };
    let temp = TempDir::new("traversal");
    let request = scope();
    let nested = temp.0.join("private/child");
    let dir = drive(
        &r,
        secure::directory(&r, &nested, true, true, &request).boxed(),
    )
    .unwrap();
    assert_eq!(mode(&nested), 0o700);
    let stat = drive(&r, r.file_stat(dir.clone(), &request)).unwrap();
    assert_eq!(stat.stx_mode as u32 & libc::S_IFMT, libc::S_IFDIR);
    for (path, error) in [
        (temp.0.join("private/../child"), Error::InvalidConfiguration),
        (temp.0.join("absent"), Error::NotFound),
    ] {
        assert_eq!(
            drive(&r, r.file_directory(&path, false, 4096, &request)).err(),
            Some(error),
            "{path:?}"
        );
    }
    std::os::unix::fs::symlink(&nested, temp.0.join("link")).unwrap();
    assert_eq!(
        drive(
            &r,
            r.file_directory(&temp.0.join("link"), false, 4096, &request)
        )
        .err(),
        Some(Error::Os(libc::ELOOP))
    );
    request.cancel();
    assert_eq!(
        poll(&mut r.file_directory(Path::new("/"), false, 4096, &request)).map(|r| r.err()),
        Poll::Ready(Some(Error::Canceled))
    );
}

/// Atomic writes publish complete private files; reads enforce limits.
#[test]
fn host_atomic_write_read_and_remove() {
    let Some(r) = kernel_reactor(8) else {
        return;
    };
    let temp = TempDir::new("atomic");
    let request = scope();
    let dir = drive(
        &r,
        secure::directory(&r, &temp.0, true, true, &request).boxed(),
    )
    .unwrap();
    drive(
        &r,
        secure::atomic_write(&r, &dir, "config", b"payload", &request).boxed(),
    )
    .unwrap();
    assert_eq!(std::fs::read(temp.0.join("config")).unwrap(), b"payload");
    assert_eq!(mode(&temp.0.join("config")), 0o600);
    assert!(!temp.0.join(".config.stage").exists());
    let read = drive(
        &r,
        secure::read_at(&r, &dir, "config", 64, true, &request).boxed(),
    )
    .unwrap();
    assert_eq!(&*read, b"payload");
    assert_eq!(
        drive(
            &r,
            secure::read_at(&r, &dir, "config", 3, false, &request).boxed()
        )
        .err(),
        Some(Error::InvalidInput)
    );
    let read = drive(
        &r,
        secure::read_path(&r, &temp.0.join("config"), 64, &request).boxed(),
    )
    .unwrap();
    assert_eq!(read.as_ref(), b"payload");
    assert_eq!(
        drive(
            &r,
            secure::read_at(&r, &dir, "../config", 64, false, &request).boxed()
        )
        .err(),
        Some(Error::InvalidInput)
    );
    drive(&r, secure::remove(&r, &dir, "config", &request).boxed()).unwrap();
    drive(&r, secure::remove(&r, &dir, "config", &request).boxed()).unwrap();
    assert!(!temp.0.join("config").exists());
    assert_eq!(
        drive(
            &r,
            r.file_unlink(dir, CString::new("config").unwrap(), &request)
        )
        .err(),
        Some(Error::NotFound)
    );
}

/// Bounded reads cover empty, exact and oversized files across chunks.
#[test]
fn host_bounded_reads() {
    let Some(r) = kernel_reactor(8) else {
        return;
    };
    let temp = TempDir::new("bounded");
    std::fs::create_dir_all(&temp.0).unwrap();
    let request = scope();
    for (len, limit, expected) in [
        (0, 4, Ok(0)),
        (4, 4, Ok(4)),
        (5, 4, Err(Error::Overloaded)),
        (100, 200, Ok(100)),
    ] {
        let path = temp.0.join(format!("f{len}"));
        std::fs::write(&path, vec![7u8; len]).unwrap();
        let fd = drive(
            &r,
            r.file_open(
                None,
                CString::new(path.as_os_str().as_bytes()).unwrap(),
                libc::O_RDONLY,
                0,
                &request,
            ),
        )
        .unwrap();
        let result = drive(
            &r,
            r.file_read_bounded(fd, limit, NonZeroUsize::new(3).unwrap(), &request),
        )
        .map(|out| out.len());
        assert_eq!(result, expected, "{len} {limit}");
    }
}

/// Chunked replacement verifies the stage and publishes with explicit durability.
#[test]
fn host_replacement_chunked_and_publication_errors() {
    let Some(r) = kernel_reactor(8) else {
        return;
    };
    let temp = TempDir::new("replace");
    let request = scope();
    let dir = drive(
        &r,
        secure::directory(&r, &temp.0, true, false, &request).boxed(),
    )
    .unwrap();
    let replacement = |r: &TestReactor, durability| {
        let staged = drive(
            r,
            r.file_stage(dir.clone(), "stage".as_ref(), 4096, &request),
        )
        .unwrap();
        Replacement {
            directory: dir.clone(),
            staged,
            temporary: CString::new("stage").unwrap(),
            target: CString::new("target").unwrap(),
            durability,
        }
    };
    let bytes = vec![9u8; 10_000];
    drive(
        &r,
        r.file_replace_chunked(
            replacement(&r, Durability::FileAndDirectory),
            &bytes,
            NonZeroUsize::new(4096).unwrap(),
            &request,
        ),
    )
    .unwrap();
    assert_eq!(std::fs::read(temp.0.join("target")).unwrap(), bytes);
    drive(
        &r,
        r.file_replace(
            replacement(&r, Durability::Publish),
            r.file_bytes(b"").unwrap(),
            &request,
        ),
    )
    .unwrap();
    assert_eq!(std::fs::read(temp.0.join("target")).unwrap(), b"");
    // A nonempty stage is rejected before rename.
    let used = replacement(&r, Durability::Publish);
    std::fs::write(temp.0.join("stage"), b"x").unwrap();
    assert_eq!(
        drive(
            &r,
            r.file_replace(used, r.file_bytes(b"new").unwrap(), &request)
        )
        .err(),
        Some(ReplacementError::BeforeRename(Error::InvalidInput))
    );
    let mut same = replacement(&r, Durability::Publish);
    same.target = same.temporary.clone();
    assert_eq!(
        drive(
            &r,
            r.file_replace(same, r.file_bytes(b"new").unwrap(), &request)
        )
        .err()
        .map(|error| error.cause()),
        Some(Error::InvalidInput)
    );
}

/// Synchronous publication skips occupied stages and cleans failures.
#[test]
fn host_publish_new_skips_collisions() {
    let temp = TempDir::new("publish");
    let target = temp.0.join("target");
    operations::publish_new(&temp.0, &target, b"one", [temp.0.join("a")]).unwrap();
    assert_eq!(mode(&temp.0), 0o700);
    std::fs::write(temp.0.join("stale"), b"keep").unwrap();
    assert!(operations::publish_new(&temp.0, &target, b"two", [temp.0.join("stale")]).is_err());
    operations::publish_new(
        &temp.0,
        &target,
        b"two",
        [temp.0.join("stale"), temp.0.join("b")],
    )
    .unwrap();
    assert_eq!(std::fs::read(&target).unwrap(), b"two");
    assert_eq!(std::fs::read(temp.0.join("stale")).unwrap(), b"keep");
    assert!(operations::publish_new(&temp.0, &target, b"x", [temp.0.join("../outside")]).is_err());
}

/// Two-slot checkpoints alternate and fall back to an older valid slot.
#[test]
fn host_checkpoint_slots_alternate_and_fall_back() {
    use checkpoint::Slots;
    const SLOTS: Slots<8> = Slots {
        names: ["a", "b"],
        minimum_bytes: 9,
        maximum_bytes: 64,
    };
    fn image(sequence: u64) -> Vec<u8> {
        let mut bytes = sequence.to_le_bytes().to_vec();
        bytes.push(42);
        bytes
    }
    fn hint(bytes: &[u8]) -> std::io::Result<u64> {
        Ok(u64::from_le_bytes(bytes[..8].try_into().unwrap()))
    }
    fn decode(bytes: &[u8], budget: usize) -> std::io::Result<u64> {
        if bytes != image(hint(bytes)?) || budget < 16 {
            return Err(std::io::Error::other("invalid test image"));
        }
        hint(bytes)
    }
    let temp = TempDir::new("checkpoint");
    assert!(
        SLOTS
            .candidates(&temp.0, 64, hint, decode)
            .unwrap()
            .next()
            .is_none()
    );
    std::fs::create_dir_all(&temp.0).unwrap();
    std::fs::write(temp.0.join("a"), image(1)).unwrap();
    std::fs::write(temp.0.join("b"), b"torn").unwrap();
    let newest = SLOTS.candidates(&temp.0, 64, hint, decode).unwrap().next();
    let (slot, sequence) = SLOTS.next_publication(newest).unwrap();
    assert_eq!((slot, sequence), (1, 2));
    SLOTS.publish(&temp.0, slot, &image(sequence)).unwrap();
    assert_eq!(
        SLOTS
            .candidates(&temp.0, 64, hint, decode)
            .unwrap()
            .collect::<Vec<_>>(),
        vec![(1, 2), (0, 1)]
    );
    assert_eq!(SLOTS.next_publication(Some((1, u64::MAX))), None);
    assert!(SLOTS.publish(&temp.0, 2, &image(1)).is_err());
    let Some(r) = kernel_reactor(8) else {
        return;
    };
    let request = scope();
    drive(
        &r,
        Box::pin(SLOTS.publish_async(
            &r,
            &temp.0,
            0,
            &image(3),
            NonZeroUsize::new(4).unwrap(),
            &request,
        )),
    )
    .unwrap();
    assert_eq!(
        SLOTS.candidates(&temp.0, 64, hint, decode).unwrap().next(),
        Some((0, 3))
    );
}

/// Abandoned namespace changes keep their descriptor and charge until fenced.
#[test]
fn host_abandoned_operations_are_fenced_by_request() {
    let Some(r) = kernel_reactor(8) else {
        return;
    };
    let temp = TempDir::new("fence");
    let request = scope();
    let dir = drive(
        &r,
        secure::directory(&r, &temp.0, true, false, &request).boxed(),
    )
    .unwrap();
    let weak = Rc::downgrade(&dir);
    let mut op = r.file_sync(dir, &request);
    let _ = poll(&mut op);
    drop(op);
    drive(&r, r.fence_matching(|_| true)).unwrap();
    assert_eq!(r.in_flight(), 0);
    assert!(weak.upgrade().is_none());
}

trait Boxed<'a, T>: std::future::Future<Output = T> + Sized + 'a {
    fn boxed(self) -> std::pin::Pin<Box<dyn std::future::Future<Output = T> + 'a>> {
        Box::pin(self)
    }
}

impl<'a, T, F: std::future::Future<Output = T> + 'a> Boxed<'a, T> for F {}

use std::task::Poll;

/// Test-only mapping so the shared test scope can use the secure helpers.
impl From<secure::AccessError> for Error {
    fn from(error: secure::AccessError) -> Self {
        match error {
            secure::AccessError::MissingMetadata => Self::InvalidInput,
            secure::AccessError::PermissionDenied => Self::Os(libc::EACCES),
        }
    }
}
