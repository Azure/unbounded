use std::fs::{self, File, OpenOptions};
use std::os::fd::AsRawFd;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt, symlink};
use std::os::unix::net::UnixListener;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use uds_endpoint::*;

fn layout() -> Layout {
    Layout::new("endpoint.lock", "endpoint.sock", "owned-", |name| {
        name.starts_with("pending-") && name.len() == 9
    })
    .unwrap()
}

struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let root = Self(PathBuf::from(env!("CARGO_TARGET_TMPDIR")).join(format!(
            "ownership-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        )));
        fs::create_dir_all(&root.0).unwrap();
        fs::set_permissions(&root.0, fs::Permissions::from_mode(0o700)).unwrap();
        root
    }
    fn directory(&self) -> File {
        open_directory(&self.0, 0o755).unwrap()
    }
}
impl Drop for Root {
    fn drop(&mut self) {
        if let Err(error) = fs::remove_dir_all(&self.0) {
            if std::thread::panicking() {
                eprintln!("fixture cleanup failed: {error}");
            } else {
                panic!("fixture cleanup failed: {error}");
            }
        }
    }
}

#[test]
fn walk_rejects_symlink_components_and_invalid_paths() {
    let root = Root::new();
    let directory = root.directory();
    let child = child_directory(&directory, b"child", 0o700).unwrap();
    assert_eq!(child.metadata().unwrap().mode() & 0o777, 0o700);
    symlink("child", root.0.join("link")).unwrap();
    assert!(open_directory(&root.0.join("link/nested"), 0o755).is_err());
    assert!(!root.0.join("child/nested").exists());
    assert!(child_directory(&directory, b"link", 0o755).is_err());
    assert!(child_directory(&directory, b"bad\0name", 0o755).is_err());
    assert!(open_directory(&root.0.join("../other"), 0o755).is_err());
    fs::write(root.0.join("file"), b"preserve").unwrap();
    assert!(child_directory(&directory, b"file", 0o755).is_err());
}

#[test]
fn hardening_is_pinned_idempotent_and_never_broadens_permissions() {
    for mode in [0o777, 0o775, 0o750, 0o700, 0o2750, 0o1777] {
        let root = Root::new();
        let directory = root.directory();
        let child = child_directory(&directory, b"child", 0o700).unwrap();
        child
            .set_permissions(fs::Permissions::from_mode(mode))
            .unwrap();
        let before = child.metadata().unwrap();
        fs::rename(root.0.join("child"), root.0.join("moved")).unwrap();
        symlink("target", root.0.join("child")).unwrap();
        fs::create_dir(root.0.join("target")).unwrap();
        fs::set_permissions(root.0.join("target"), fs::Permissions::from_mode(0o777)).unwrap();
        restrict_directory(&child, 0o022).unwrap();
        let after = child.metadata().unwrap();
        assert!(same_inode(&before, &after));
        assert_eq!((before.uid(), before.gid()), (after.uid(), after.gid()));
        assert_eq!(after.mode() & 0o7777, mode & !0o022);
        restrict_directory(&child, 0o022).unwrap();
        let again = child.metadata().unwrap();
        assert_eq!(
            (after.ctime(), after.ctime_nsec()),
            (again.ctime(), again.ctime_nsec())
        );
        assert_eq!(
            fs::metadata(root.0.join("target")).unwrap().mode() & 0o777,
            0o777
        );
        assert_eq!(directory.metadata().unwrap().mode() & 0o777, 0o700);
    }
}

#[test]
fn hardening_rejects_non_directory_and_fchmod_failure() {
    let root = Root::new();
    let path = root.0.join("file");
    fs::write(&path, b"preserve").unwrap();
    assert!(restrict_directory(&File::open(&path).unwrap(), 0o022).is_err());
    fs::set_permissions(&root.0, fs::Permissions::from_mode(0o777)).unwrap();
    let directory = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_PATH | libc::O_DIRECTORY)
        .open(&root.0)
        .unwrap();
    assert!(restrict_directory(&directory, 0o022).is_err());
    assert_eq!(directory.metadata().unwrap().mode() & 0o777, 0o777);
}

#[test]
fn lock_is_exclusive_persistent_and_validates_path_identity() {
    let root = Root::new();
    let directory = root.directory();
    let owner = EndpointOwner::acquire(&directory, layout()).unwrap();
    let before = fs::metadata(root.0.join(layout().lock())).unwrap();
    assert!(EndpointOwner::acquire(&directory, layout()).is_err());
    owner.validate(&directory).unwrap();
    let other = Root::new();
    assert!(owner.validate(&other.directory()).is_err());
    drop(owner);
    let owner = EndpointOwner::acquire(&directory, layout()).unwrap();
    assert!(same_inode(
        &before,
        &fs::metadata(root.0.join(layout().lock())).unwrap()
    ));
    fs::rename(root.0.join(layout().lock()), root.0.join("old-lock")).unwrap();
    fs::write(root.0.join(layout().lock()), b"").unwrap();
    assert!(owner.validate(&directory).is_err());
}

#[test]
fn unsafe_locks_and_writable_directories_are_rejected_without_cleanup() {
    for kind in [
        "symlink",
        "hardlink",
        "directory",
        "fifo",
        "permissions",
        "writable",
    ] {
        let root = Root::new();
        let directory = root.directory();
        let target = root.0.join("target");
        fs::write(&target, b"preserve").unwrap();
        fs::set_permissions(&target, fs::Permissions::from_mode(0o600)).unwrap();
        let lock = root.0.join(layout().lock());
        match kind {
            "symlink" => symlink(&target, &lock).unwrap(),
            "hardlink" => fs::hard_link(&target, &lock).unwrap(),
            "directory" => fs::create_dir(&lock).unwrap(),
            "fifo" => {
                let name = std::ffi::CString::new(lock.as_os_str().as_encoded_bytes()).unwrap();
                // SAFETY: name is a live, NUL-terminated pathname.
                assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
            }
            "permissions" => {
                fs::write(&lock, b"").unwrap();
                fs::set_permissions(&lock, fs::Permissions::from_mode(0o666)).unwrap();
            }
            _ => directory
                .set_permissions(fs::Permissions::from_mode(0o777))
                .unwrap(),
        }
        assert!(
            EndpointOwner::acquire(&directory, layout()).is_err(),
            "{kind}"
        );
        assert_eq!(fs::read(target).unwrap(), b"preserve");
    }
}

fn stale(listener: UnixListener) {
    // Prevent a concurrently spawned child from retaining a live listening alias.
    assert_eq!(
        // SAFETY: listener retains a valid socket descriptor throughout shutdown.
        unsafe { libc::shutdown(listener.as_raw_fd(), libc::SHUT_RDWR) },
        0
    );
    drop(listener);
}

#[test]
fn recovery_requires_all_witnesses_and_preserves_live_or_foreign_paths() {
    for kind in [
        "live",
        "foreign",
        "bad-witness",
        "bad-temporary",
        "recover",
        "unpublished",
    ] {
        let root = Root::new();
        let directory = root.directory();
        let anchor = file_path(&directory);
        let witness = anchor.join("owned-pending-a");
        let listener = UnixListener::bind(&witness).unwrap();
        if kind != "unpublished" {
            fs::hard_link(&witness, anchor.join(layout().canonical())).unwrap();
            fs::hard_link(&witness, anchor.join("pending-a")).unwrap();
        }
        let live = if kind == "live" {
            Some(listener)
        } else {
            stale(listener);
            None
        };
        match kind {
            "foreign" => {
                fs::remove_file(anchor.join(layout().canonical())).unwrap();
                stale(UnixListener::bind(anchor.join(layout().canonical())).unwrap());
            }
            "bad-witness" => fs::write(anchor.join("owned-invalid"), b"preserve").unwrap(),
            "bad-temporary" => fs::write(anchor.join("pending-b"), b"preserve").unwrap(),
            _ => {}
        }
        let result = EndpointOwner::acquire(&directory, layout());
        if matches!(kind, "recover" | "unpublished") {
            let owner = result.unwrap();
            assert_eq!(fs::read_dir(&anchor).unwrap().count(), 1);
            owner.validate(&directory).unwrap();
        } else {
            assert!(result.is_err(), "{kind}");
            assert!(witness.exists());
            assert!(anchor.join(layout().canonical()).exists());
            assert!(anchor.join("pending-a").exists());
        }
        drop(live);
    }
}

#[test]
fn socket_pin_rejects_replacement_symlinks_and_wrong_inodes() {
    let root = Root::new();
    let directory = root.directory();
    let anchor = file_path(&directory);
    let listener = UnixListener::bind(anchor.join("socket")).unwrap();
    let metadata = fs::symlink_metadata(anchor.join("socket")).unwrap();
    let pinned = pin_socket(&directory, "socket", metadata.dev(), metadata.ino()).unwrap();
    assert!(pin_socket(&directory, "socket", metadata.dev(), metadata.ino() + 1).is_err());
    fs::rename(anchor.join("socket"), anchor.join("moved")).unwrap();
    symlink("moved", anchor.join("socket")).unwrap();
    assert!(pin_socket(&directory, "socket", metadata.dev(), metadata.ino()).is_err());
    fs::set_permissions(file_path(&pinned), fs::Permissions::from_mode(0o666)).unwrap();
    assert_eq!(
        fs::metadata(anchor.join("moved")).unwrap().mode() & 0o777,
        0o666
    );
    drop(listener);
}

#[test]
fn bound_socket_accepts_and_cleans_up_published_names_before_unlock() {
    use std::os::unix::net::UnixStream;
    use std::rc::Rc;
    let root = Root::new();
    let directory = root.directory();
    let anchor = file_path(&directory);
    use uds_endpoint::publication::{Publication, Replacement};
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let bound = Rc::new(BoundSocket::bind(owner, "pending-a").unwrap());
    assert!(
        bound
            .accept()
            .is_err_and(|error| error.kind() == std::io::ErrorKind::WouldBlock)
    );
    let (device, inode) = bound.identity();
    let pinned = pin_socket(&directory, "pending-a", device, inode).unwrap();
    bound.set_mode(0o666).unwrap();
    assert_eq!(pinned.metadata().unwrap().mode() & 0o777, 0o666);
    let mut publication = Publication::default();
    publication
        .publish(
            Replacement::prepare(
                bound.clone(),
                None,
                "pending-a".into(),
                layout().canonical().into(),
            )
            .unwrap(),
        )
        .unwrap();
    publication.commit();
    drop(publication);
    let client = UnixStream::connect(anchor.join(layout().canonical())).unwrap();
    let _accepted = bound.accept().unwrap();
    assert!(EndpointOwner::acquire(&directory, layout()).is_err());
    drop(bound);
    assert_eq!(fs::read_dir(&anchor).unwrap().count(), 1);
    EndpointOwner::acquire(&directory, layout()).unwrap();
    drop(client);
}

#[test]
fn failed_bind_and_replaced_basename_preserve_foreign_paths() {
    use std::rc::Rc;
    for stage in ["hardlink", "after-bind"] {
        let root = Root::new();
        let directory = root.directory();
        let anchor = file_path(&directory);
        let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
        if stage == "hardlink" {
            fs::write(anchor.join("pending-a"), b"foreign").unwrap();
        }
        let result = BoundSocket::bind(owner, "pending-a");
        if stage == "hardlink" {
            assert!(result.is_err());
        } else {
            let bound = result.unwrap();
            fs::remove_file(anchor.join("pending-a")).unwrap();
            fs::write(anchor.join("pending-a"), b"foreign").unwrap();
            drop(bound);
        }
        assert_eq!(fs::read(anchor.join("pending-a")).unwrap(), b"foreign");
        assert!(!anchor.join("owned-pending-a").exists());
    }
}

#[test]
fn bound_socket_cleanup_stays_on_pinned_directory_after_path_swap() {
    use std::rc::Rc;
    let root = Root::new();
    let directory = child_directory(&root.directory(), b"child", 0o700).unwrap();
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let bound = BoundSocket::bind(owner, "pending-a").unwrap();
    fs::rename(root.0.join("child"), root.0.join("moved")).unwrap();
    fs::create_dir(root.0.join("child")).unwrap();
    fs::write(root.0.join("child/pending-a"), b"foreign").unwrap();
    drop(bound);
    assert_eq!(fs::read_dir(root.0.join("moved")).unwrap().count(), 1);
    assert_eq!(
        fs::read(root.0.join("child/pending-a")).unwrap(),
        b"foreign"
    );
}

fn semantic(error: &std::io::Error) -> Option<&Error> {
    error.get_ref()?.downcast_ref()
}

#[test]
fn public_validation_rejects_bad_names_without_creating_paths() {
    use std::path::Path;
    use std::rc::Rc;
    let root = Root::new();
    let directory = root.directory();
    for name in [
        b"".as_slice(),
        b".",
        b"..",
        b"a/b",
        b"/absolute",
        b"a\0b",
        &[b'x'; 256],
    ] {
        let error = child_directory(&directory, name, 0o700).unwrap_err();
        assert_eq!(error.kind(), std::io::ErrorKind::InvalidInput);
        assert_eq!(semantic(&error), Some(&Error::InvalidName));
    }
    for path in [
        Path::new("relative"),
        Path::new(""),
        &root.0.join("new/./child"),
        &root.0.join("new/../child"),
    ] {
        assert_eq!(
            semantic(&open_directory(path, 0o700).unwrap_err()),
            Some(&Error::InvalidName)
        );
    }
    assert_eq!(fs::read_dir(&root.0).unwrap().count(), 0);
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    for name in [
        "../pending-a",
        "/pending-a",
        "pending-a/child",
        "pending-\0",
        "",
        ".",
        "..",
    ] {
        assert_eq!(
            semantic(&BoundSocket::bind(owner.clone(), name).unwrap_err()),
            Some(&Error::InvalidName)
        );
    }
    assert_eq!(fs::read_dir(&root.0).unwrap().count(), 1);
    child_directory(&directory, &[b'x'; 255], 0o700).unwrap();
}

#[test]
fn layouts_reject_static_overlap_and_callable_witness_overlap() {
    for result in [
        Layout::new("same", "same", "owned-", |_| false),
        Layout::new("owned-lock", "socket", "owned-", |_| false),
        Layout::new("lock", "owned-socket", "owned-", |_| false),
        Layout::new("lock", "socket", "owned-", |n| n == "lock"),
        Layout::new("lock", "socket", "owned-", |n| n == "socket"),
        Layout::new("lock", "socket", "owned-", |n| n == "owned-"),
    ] {
        assert_eq!(semantic(&result.unwrap_err()), Some(&Error::InvalidLayout));
    }
    for result in [
        Layout::new("../lock", "socket", "owned-", |_| false),
        Layout::new("lock", "", "owned-", |_| false),
        Layout::new("lock", "socket", "bad/", |_| false),
    ] {
        assert_eq!(semantic(&result.unwrap_err()), Some(&Error::InvalidName));
    }
    let root = Root::new();
    let directory = root.directory();
    // Construction cannot prove an arbitrary function disjoint for all names.
    let dynamic = Layout::new("lock", "socket", "owned-", |n| n.ends_with("-a")).unwrap();
    let owner = std::rc::Rc::new(EndpointOwner::acquire(&directory, dynamic).unwrap());
    assert_eq!(
        semantic(&BoundSocket::bind(owner.clone(), "pending-a").unwrap_err()),
        Some(&Error::InvalidLayout)
    );
    assert_eq!(fs::read_dir(&root.0).unwrap().count(), 1);
    drop(owner);
    stale(UnixListener::bind(file_path(&directory).join("owned-pending-a")).unwrap());
    assert_eq!(
        semantic(&EndpointOwner::acquire(&directory, dynamic).unwrap_err()),
        Some(&Error::InvalidLayout)
    );
    assert!(root.0.join("owned-pending-a").exists());
}

#[test]
fn lock_modes_are_fixed_private_and_directory_must_be_a_directory() {
    // Independent policy cases, not a copy of the implementation's bit mask.
    for (mode, accepted) in [
        (0o000, true),
        (0o200, true),
        (0o400, true),
        (0o600, true),
        (0o100, false),
        (0o700, false),
        (0o640, false),
        (0o606, false),
        (0o1600, false),
        (0o2600, false),
        (0o4600, false),
    ] {
        let root = Root::new();
        let directory = root.directory();
        let path = root.0.join(layout().lock());
        fs::write(&path, b"preserve").unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(mode)).unwrap();
        let before = fs::metadata(&path).unwrap();
        let result = EndpointOwner::acquire(&directory, layout());
        if accepted {
            result.unwrap().validate(&directory).unwrap();
            assert_eq!(fs::metadata(&path).unwrap().mode() & 0o7777, 0o600);
        } else {
            assert_eq!(
                semantic(&result.unwrap_err()),
                Some(&Error::UnsafeLock),
                "{mode:o}"
            );
            assert_eq!(fs::metadata(&path).unwrap().mode() & 0o7777, mode);
        }
        assert!(same_inode(&before, &fs::metadata(&path).unwrap()));
    }
    let root = Root::new();
    let directory = root.directory();
    let owner = EndpointOwner::acquire(&directory, layout()).unwrap();
    fs::set_permissions(
        root.0.join(layout().lock()),
        fs::Permissions::from_mode(0o700),
    )
    .unwrap();
    assert_eq!(
        semantic(&owner.validate(&directory).unwrap_err()),
        Some(&Error::UnsafeLock)
    );
    let root = Root::new();
    let path = root.0.join("file");
    fs::write(&path, b"preserve").unwrap();
    assert_eq!(
        semantic(&EndpointOwner::acquire(&File::open(&path).unwrap(), layout()).unwrap_err()),
        Some(&Error::UnsafeDirectory)
    );
    assert_eq!(fs::read(path).unwrap(), b"preserve");
}

#[test]
fn non_socket_witnesses_and_pins_never_become_ownership_evidence() {
    for is_symlink in [false, true] {
        let root = Root::new();
        let directory = root.directory();
        let target = root.0.join("foreign");
        fs::write(&target, b"preserve").unwrap();
        let witness = root.0.join("owned-pending-a");
        if is_symlink {
            symlink(&target, &witness).unwrap();
        } else {
            fs::hard_link(&target, &witness).unwrap();
        }
        let metadata = fs::symlink_metadata(&witness).unwrap();
        assert_eq!(
            semantic(
                &pin_socket(
                    &directory,
                    "owned-pending-a",
                    metadata.dev(),
                    metadata.ino()
                )
                .unwrap_err()
            ),
            Some(&Error::OwnershipChanged)
        );
        assert_eq!(
            semantic(&EndpointOwner::acquire(&directory, layout()).unwrap_err()),
            Some(&Error::UnrecognizedSocket)
        );
        assert!(same_inode(
            &metadata,
            &fs::symlink_metadata(&witness).unwrap()
        ));
        assert_eq!(fs::read(&target).unwrap(), b"preserve");
    }
}

#[test]
fn bind_uses_only_owner_directory_and_rejects_invalid_socket_modes() {
    let root = Root::new();
    let other = Root::new();
    let directory = root.directory();
    let owner = std::rc::Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    // The public bind signature has no directory or witness override.
    let bound = BoundSocket::bind(owner.clone(), "pending-a").unwrap();
    assert!(root.0.join("pending-a").exists());
    assert_eq!(fs::read_dir(&other.0).unwrap().count(), 0);
    for mode in [0o1000, 0o2600, 0o4600, libc::S_IFSOCK | 0o600, u32::MAX] {
        assert_eq!(
            semantic(&bound.set_mode(mode).unwrap_err()),
            Some(&Error::InvalidMode)
        );
        assert_eq!(
            fs::metadata(root.0.join("pending-a")).unwrap().mode() & 0o7777,
            0o600
        );
    }
    for mode in [0, 0o200, 0o600, 0o666, 0o777] {
        bound.set_mode(mode).unwrap();
        assert_eq!(
            fs::metadata(root.0.join("pending-a")).unwrap().mode() & 0o7777,
            mode
        );
    }
    directory
        .set_permissions(fs::Permissions::from_mode(0o777))
        .unwrap();
    assert_eq!(
        semantic(&bound.set_mode(0o600).unwrap_err()),
        Some(&Error::UnsafeDirectory)
    );
    directory
        .set_permissions(fs::Permissions::from_mode(0o700))
        .unwrap();
}

#[test]
fn long_components_and_unix_socket_paths_fail_without_staging_leaks() {
    let root = Root::new();
    let directory = root.directory();
    let long_layout =
        Layout::new("lock", "socket", "witness-", |n| n.starts_with("stage-")).unwrap();
    let owner = std::rc::Rc::new(EndpointOwner::acquire(&directory, long_layout).unwrap());
    // A legal filesystem component can still exceed sockaddr_un.sun_path.
    let too_long_for_socket = format!("stage-{}", "x".repeat(110));
    assert!(BoundSocket::bind(owner.clone(), &too_long_for_socket).is_err());
    let too_long_for_witness = format!("stage-{}", "x".repeat(249));
    assert_eq!(
        semantic(&BoundSocket::bind(owner, &too_long_for_witness).unwrap_err()),
        Some(&Error::InvalidName)
    );
    assert_eq!(fs::read_dir(&root.0).unwrap().count(), 1);
    let deep = root.0.join("x".repeat(200)).join("child");
    let deep_directory = open_directory(&deep, 0o700).unwrap();
    let owner = std::rc::Rc::new(EndpointOwner::acquire(&deep_directory, layout()).unwrap());
    // The pinned /proc path avoids the absolute directory path's socket limit.
    BoundSocket::bind(owner, "pending-a").unwrap();
}

#[test]
#[ignore = "invoked by crash_recovery_subprocess_states with a private fixture"]
fn crash_state_driver() {
    use std::rc::Rc;
    use uds_endpoint::publication::{Publication, Replacement};
    let root = PathBuf::from(std::env::var_os("UDS_CRASH_ROOT").expect("subprocess root"));
    let state = std::env::var("UDS_CRASH_STATE").unwrap();
    let directory = open_directory(&root, 0o700).unwrap();
    // SAFETY: this driver runs alone in a dedicated subprocess. No parallel test
    // can observe this process-wide umask, and exit below avoids restoration races.
    unsafe { libc::umask(0o777) };
    if state == "witness-only" {
        let _listener = UnixListener::bind(file_path(&directory).join("owned-pending-a")).unwrap();
        assert_eq!(
            fs::metadata(root.join("owned-pending-a")).unwrap().mode() & 0o777,
            0
        );
        std::process::exit(73);
    }
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let old = Rc::new(BoundSocket::bind(owner.clone(), "pending-a").unwrap());
    assert_eq!(
        fs::metadata(root.join(layout().lock())).unwrap().mode() & 0o777,
        0o600
    );
    assert_eq!(
        fs::metadata(root.join("pending-a")).unwrap().mode() & 0o777,
        0o600
    );
    if state == "prepared" {
        old.set_mode(0).unwrap();
        std::process::exit(73);
    }
    let mut initial = Publication::default();
    initial
        .publish(
            Replacement::prepare(
                old.clone(),
                None,
                "pending-a".into(),
                layout().canonical().into(),
            )
            .unwrap(),
        )
        .unwrap();
    initial.commit();
    drop(initial);
    if state == "committed" {
        std::process::exit(73);
    }
    assert_eq!(state, "exchanged-uncommitted");
    let next = Rc::new(BoundSocket::bind(owner, "pending-b").unwrap());
    let mut publication = Publication::default();
    publication
        .publish(
            Replacement::prepare(
                next,
                Some(old),
                "pending-b".into(),
                layout().canonical().into(),
            )
            .unwrap(),
        )
        .unwrap();
    // exit bypasses Rust destructors, preserving the post-exchange journal state.
    std::process::exit(73);
}

#[test]
fn crash_recovery_subprocess_states() {
    for state in [
        "witness-only",
        "prepared",
        "committed",
        "exchanged-uncommitted",
    ] {
        let root = Root::new();
        let status = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "crash_state_driver",
                "--ignored",
                "--test-threads=1",
            ])
            .env("UDS_CRASH_ROOT", &root.0)
            .env("UDS_CRASH_STATE", state)
            .status()
            .unwrap();
        assert_eq!(status.code(), Some(73), "{state}");
        let witness = fs::metadata(root.0.join("owned-pending-a")).unwrap();
        match state {
            "prepared" => {
                assert!(same_inode(
                    &witness,
                    &fs::metadata(root.0.join("pending-a")).unwrap()
                ));
                assert!(!root.0.join(layout().canonical()).exists());
                assert_eq!(witness.mode() & 0o777, 0);
            }
            "committed" => assert!(same_inode(
                &witness,
                &fs::metadata(root.0.join(layout().canonical())).unwrap()
            )),
            "exchanged-uncommitted" => {
                assert!(same_inode(
                    &witness,
                    &fs::metadata(root.0.join("pending-b")).unwrap()
                ));
                assert!(same_inode(
                    &fs::metadata(root.0.join("owned-pending-b")).unwrap(),
                    &fs::metadata(root.0.join(layout().canonical())).unwrap()
                ));
            }
            "witness-only" => assert_eq!(witness.mode() & 0o777, 0),
            _ => unreachable!(),
        }
        let directory = root.directory();
        let owner = EndpointOwner::acquire(&directory, layout()).unwrap();
        owner.validate(&directory).unwrap();
        assert_eq!(fs::read_dir(&root.0).unwrap().count(), 1, "{state}");
        assert!(root.0.join(layout().lock()).exists());
    }
}

#[test]
#[ignore = "invoked by recovery_uses_bounded_descriptors with a private fixture"]
fn descriptor_limit_driver() {
    let root = PathBuf::from(std::env::var_os("UDS_RLIMIT_ROOT").expect("subprocess root"));
    let directory = open_directory(&root, 0o700).unwrap();
    let layout = Layout::new("lock", "canonical", "witness-", |name| {
        name.starts_with("stage-")
    })
    .unwrap();
    let anchor = file_path(&directory);
    // More witness inodes than the entire descriptor budget, all initially
    // unwritable. Setup itself opens at most one listener at a time.
    for index in 0..128 {
        let witness = anchor.join(format!("witness-stage-{index}"));
        stale(UnixListener::bind(&witness).unwrap());
        fs::set_permissions(&witness, fs::Permissions::from_mode(0o000)).unwrap();
        fs::hard_link(&witness, anchor.join(format!("stage-{index}"))).unwrap();
    }
    let mut limit = libc::rlimit {
        rlim_cur: 0,
        rlim_max: 0,
    };
    assert_eq!(
        // SAFETY: limit is valid writable storage. This runs alone in a subprocess.
        unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut limit) },
        0
    );
    limit.rlim_cur = limit.rlim_cur.min(64);
    // SAFETY: lowering only this isolated process's soft descriptor limit; the
    // hard limit stays unchanged. Exiting the subprocess discards the change.
    assert_eq!(unsafe { libc::setrlimit(libc::RLIMIT_NOFILE, &limit) }, 0);

    // Whole-namespace validation still precedes ALL permission repairs/removals,
    // including when a foreign canonical entry is found after the witness scan.
    fs::write(anchor.join("canonical"), b"foreign").unwrap();
    assert_eq!(
        semantic(&EndpointOwner::acquire(&directory, layout).unwrap_err()),
        Some(&Error::UnrecognizedSocket)
    );
    for index in 0..128 {
        assert_eq!(
            fs::metadata(anchor.join(format!("witness-stage-{index}")))
                .unwrap()
                .mode()
                & 0o777,
            0
        );
        assert!(anchor.join(format!("stage-{index}")).exists());
    }
    assert_eq!(fs::read(anchor.join("canonical")).unwrap(), b"foreign");
    fs::remove_file(anchor.join("canonical")).unwrap();
    fs::hard_link(anchor.join("witness-stage-0"), anchor.join("canonical")).unwrap();
    let owner = EndpointOwner::acquire(&directory, layout).unwrap();
    owner.validate(&directory).unwrap();
    assert_eq!(fs::read_dir(&root).unwrap().count(), 1);
    assert!(root.join("lock").exists());
}

#[test]
fn recovery_uses_bounded_descriptors() {
    let root = Root::new();
    let status = std::process::Command::new("timeout")
        .args(["--signal=TERM", "--kill-after=10s", "300s"])
        .arg(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "descriptor_limit_driver",
            "--ignored",
            "--test-threads=1",
        ])
        .env("UDS_RLIMIT_ROOT", &root.0)
        .status()
        .unwrap();
    assert!(
        status.success(),
        "descriptor-limit subprocess failed: {status}"
    );
}
