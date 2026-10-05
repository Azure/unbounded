use super::test_support::drive;
use super::*;
use crate::reactor::tests::{kernel_reactor, scope};
use std::{
    fs,
    os::unix::fs::{PermissionsExt, symlink},
    path::Path,
    sync::atomic::{AtomicUsize, Ordering},
};

struct Directory(PathBuf);
impl Directory {
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
    fn drop(&mut self) {
        fs::remove_dir_all(&self.0).unwrap();
    }
}
fn name(path: &Path) -> CString {
    CString::new(path.as_os_str().as_bytes()).unwrap()
}

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

#[test]
fn host_publication_honors_umask_in_isolated_process() {
    const CHILD: &str = "URING_FILESYSTEM_UMASK_CHILD";
    if std::env::var_os(CHILD).is_none() {
        let result = std::process::Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "reactor::filesystem::kernel_tests::host_publication_honors_umask_in_isolated_process", "--nocapture"])
            .env(CHILD, "1").status().unwrap();
        assert!(result.success());
        return;
    }
    // Isolated single-test child, so no process-wide umask race with other tests.
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
}

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
