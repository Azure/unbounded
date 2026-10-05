use std::fs::{self, File, OpenOptions};
use std::os::fd::AsRawFd;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt, symlink};
use std::os::unix::net::UnixListener;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use uds_endpoint::*;

const LAYOUT: Layout = Layout {
    lock: "endpoint.lock",
    canonical: "endpoint.sock",
    witness_prefix: "owned-",
    temporary_name: |name| name.starts_with("pending-") && name.len() == 9,
};

struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let root = Self(
            PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!(
                    "ownership-{}-{}",
                    std::process::id(),
                    NEXT.fetch_add(1, Ordering::Relaxed)
                )),
        );
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
        fs::remove_dir_all(&self.0).unwrap();
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
    let owner = EndpointOwner::acquire(&directory, LAYOUT, 0o600).unwrap();
    let before = fs::metadata(root.0.join(LAYOUT.lock)).unwrap();
    assert!(EndpointOwner::acquire(&directory, LAYOUT, 0o600).is_err());
    owner.validate(&directory).unwrap();
    let other = Root::new();
    assert!(owner.validate(&other.directory()).is_err());
    drop(owner);
    let owner = EndpointOwner::acquire(&directory, LAYOUT, 0o600).unwrap();
    assert!(same_inode(
        &before,
        &fs::metadata(root.0.join(LAYOUT.lock)).unwrap()
    ));
    fs::rename(root.0.join(LAYOUT.lock), root.0.join("old-lock")).unwrap();
    fs::write(root.0.join(LAYOUT.lock), b"").unwrap();
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
        let lock = root.0.join(LAYOUT.lock);
        match kind {
            "symlink" => symlink(&target, &lock).unwrap(),
            "hardlink" => fs::hard_link(&target, &lock).unwrap(),
            "directory" => fs::create_dir(&lock).unwrap(),
            "fifo" => {
                let name = std::ffi::CString::new(lock.as_os_str().as_encoded_bytes()).unwrap();
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
            EndpointOwner::acquire(&directory, LAYOUT, 0o600).is_err(),
            "{kind}"
        );
        assert_eq!(fs::read(target).unwrap(), b"preserve");
    }
}

fn stale(listener: UnixListener) {
    // Prevent a concurrently spawned child from retaining a live listening alias.
    assert_eq!(
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
            fs::hard_link(&witness, anchor.join(LAYOUT.canonical)).unwrap();
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
                fs::remove_file(anchor.join(LAYOUT.canonical)).unwrap();
                stale(UnixListener::bind(anchor.join(LAYOUT.canonical)).unwrap());
            }
            "bad-witness" => fs::write(anchor.join("owned-invalid"), b"preserve").unwrap(),
            "bad-temporary" => fs::write(anchor.join("pending-b"), b"preserve").unwrap(),
            _ => {}
        }
        let result = EndpointOwner::acquire(&directory, LAYOUT, 0o600);
        if matches!(kind, "recover" | "unpublished") {
            let owner = result.unwrap();
            assert_eq!(fs::read_dir(&anchor).unwrap().count(), 1);
            owner.validate(&directory).unwrap();
        } else {
            assert!(result.is_err(), "{kind}");
            assert!(witness.exists());
            assert!(anchor.join(LAYOUT.canonical).exists());
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
    let owner = Rc::new(EndpointOwner::acquire(&directory, LAYOUT, 0o600).unwrap());
    let bound = BoundSocket::bind(
        directory.try_clone().unwrap(),
        owner,
        "pending-a".into(),
        "owned-pending-a".into(),
    )
    .unwrap();
    assert!(
        bound
            .accept()
            .is_err_and(|error| error.kind() == std::io::ErrorKind::WouldBlock)
    );
    let (device, inode) = bound.identity();
    let pinned = pin_socket(&directory, "pending-a", device, inode).unwrap();
    fs::set_permissions(file_path(&pinned), fs::Permissions::from_mode(0o666)).unwrap();
    fs::rename(anchor.join("pending-a"), anchor.join(LAYOUT.canonical)).unwrap();
    *bound.basename().borrow_mut() = LAYOUT.canonical.into();
    let client = UnixStream::connect(anchor.join(LAYOUT.canonical)).unwrap();
    let _accepted = bound.accept().unwrap();
    assert!(EndpointOwner::acquire(&directory, LAYOUT, 0o600).is_err());
    drop(bound);
    assert_eq!(fs::read_dir(&anchor).unwrap().count(), 1);
    EndpointOwner::acquire(&directory, LAYOUT, 0o600).unwrap();
    drop(client);
}

#[test]
fn failed_bind_and_replaced_basename_preserve_foreign_paths() {
    use std::rc::Rc;
    for stage in ["hardlink", "after-bind"] {
        let root = Root::new();
        let directory = root.directory();
        let anchor = file_path(&directory);
        let owner = Rc::new(EndpointOwner::acquire(&directory, LAYOUT, 0o600).unwrap());
        if stage == "hardlink" {
            fs::write(anchor.join("pending-a"), b"foreign").unwrap();
        }
        let result = BoundSocket::bind(
            directory.try_clone().unwrap(),
            owner,
            "pending-a".into(),
            "owned-pending-a".into(),
        );
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
    let owner = Rc::new(EndpointOwner::acquire(&directory, LAYOUT, 0o600).unwrap());
    let bound = BoundSocket::bind(
        directory,
        owner,
        "pending-a".into(),
        "owned-pending-a".into(),
    )
    .unwrap();
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
