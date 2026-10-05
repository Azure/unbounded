use super::*;
use publication::{Endpoint, Publication, Replacement};
use std::os::unix::fs::{PermissionsExt, symlink};
use std::sync::atomic::{AtomicU64, Ordering};

struct Root(PathBuf);

impl Root {
    fn new() -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .parent()
            .unwrap()
            .join("target/tmp")
            .join(format!(
                "core-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        fs::create_dir_all(&path).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o700)).unwrap();
        Self(path)
    }

    fn directory(&self) -> File {
        open_directory(&self.0, 0o700).unwrap()
    }
}

impl Drop for Root {
    fn drop(&mut self) {
        if let Err(error) = fs::remove_dir_all(&self.0) {
            eprintln!("fixture cleanup failed for {}: {error}", self.0.display());
        }
    }
}

fn layout() -> Layout {
    Layout::new("lock", "socket", "witness-", |s| s.starts_with("stage-")).unwrap()
}

#[test]
fn component_boundaries_reject_before_creating_any_path() {
    let root = Root::new();
    let directory = root.directory();
    for name in [b"".as_slice(), b".", b"..", b"a/b", b"/absolute", b"a\0b"] {
        let error = child_directory(&directory, name, 0o700).unwrap_err();
        assert_eq!(error.kind(), ErrorKind::InvalidInput);
    }
    assert!(child_directory(&directory, &[b'x'; 256], 0o700).is_err());
    assert!(open_directory(Path::new("relative"), 0o700).is_err());
    assert!(open_directory(&root.0.join("new/../escape"), 0o700).is_err());
    assert!(!root.0.join("new").exists());
    assert_eq!(fs::read_dir(&root.0).unwrap().count(), 0);
    let long = [b'x'; 255];
    child_directory(&directory, &long, 0o700).unwrap();
}

#[test]
fn layout_checks_static_and_dynamic_overlap() {
    assert!(Layout::new("lock", "lock", "witness-", |_| false).is_err());
    assert!(Layout::new("witness-lock", "socket", "witness-", |_| false).is_err());
    assert!(Layout::new("lock", "socket", "witness-", |n| n == "socket").is_err());
    let layout = Layout::new("lock", "socket", "witness-", |n| n.ends_with("-bad")).unwrap();
    assert!(layout.witness("stage-bad").is_err());
    assert!(layout.witness("../stage-bad").is_err());
}

#[test]
fn owner_repairs_zero_mode_lock_without_replacing_inode() {
    let root = Root::new();
    let directory = root.directory();
    let path = root.0.join("lock");
    fs::write(&path, b"").unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o000)).unwrap();
    let before = fs::metadata(&path).unwrap();
    let owner = EndpointOwner::acquire(&directory, layout()).unwrap();
    let after = fs::metadata(&path).unwrap();
    assert!(same_inode(&before, &after));
    assert_eq!(after.mode() & 0o7777, 0o600);
    owner.validate(&directory).unwrap();
}

#[test]
fn recovery_repairs_unwritable_witness_before_distinguishing_live_and_dead() {
    for live in [false, true] {
        let root = Root::new();
        let directory = root.directory();
        let witness = root.0.join("witness-stage-a");
        let listener = UnixListener::bind(file_path(&directory).join("witness-stage-a")).unwrap();
        fs::hard_link(&witness, root.0.join("socket")).unwrap();
        fs::set_permissions(&witness, fs::Permissions::from_mode(0o000)).unwrap();
        let listener = if live {
            Some(listener)
        } else {
            drop(listener);
            None
        };
        let result = EndpointOwner::acquire(&directory, layout());
        if live {
            let e = result.unwrap_err();
            assert_eq!(
                e.get_ref().unwrap().downcast_ref::<Error>(),
                Some(&Error::LiveSocket)
            );
            assert!(witness.exists());
            assert!(root.0.join("socket").exists());
            assert_ne!(fs::metadata(witness).unwrap().mode() & 0o200, 0);
        } else {
            result.unwrap();
            assert!(!witness.exists());
            assert!(!root.0.join("socket").exists());
        }
        drop(listener);
    }
}

#[test]
fn drop_preserves_witness_on_basename_stat_failure() {
    let root = Root::new();
    let directory = root.directory();
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let bound = BoundSocket::bind(owner.clone(), "stage-a").unwrap();
    directory::FAIL_METADATA.with(|failure| *failure.borrow_mut() = Some("stage-a".into()));
    drop(bound);
    directory::FAIL_METADATA.with(|failure| *failure.borrow_mut() = None);
    assert!(root.0.join("stage-a").exists());
    assert!(root.0.join("witness-stage-a").exists());
    drop(owner);
    EndpointOwner::acquire(&directory, layout()).unwrap();
    assert!(!root.0.join("stage-a").exists());
    assert!(!root.0.join("witness-stage-a").exists());
}

#[test]
fn pinned_mode_change_never_follows_replaced_basename_or_witness() {
    let root = Root::new();
    let directory = root.directory();
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let bound = BoundSocket::bind(owner, "stage-a").unwrap();
    fs::write(root.0.join("foreign"), b"preserve").unwrap();
    fs::set_permissions(root.0.join("foreign"), fs::Permissions::from_mode(0o400)).unwrap();
    for name in ["stage-a", "witness-stage-a"] {
        fs::remove_file(root.0.join(name)).unwrap();
        symlink("foreign", root.0.join(name)).unwrap();
    }
    bound.set_mode(0o666).unwrap();
    assert_eq!(
        fs::metadata(root.0.join("foreign")).unwrap().mode() & 0o777,
        0o400
    );
    drop(bound);
    assert!(
        fs::symlink_metadata(root.0.join("stage-a"))
            .unwrap()
            .file_type()
            .is_symlink()
    );
}

#[test]
fn checked_publication_probe_preserves_stat_error_and_completed_is_typed() {
    let root = Root::new();
    let directory = root.directory();
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let bound = Rc::new(BoundSocket::bind(owner, "stage-a").unwrap());
    directory::FAIL_METADATA.with(|failure| *failure.borrow_mut() = Some("stage-a".into()));
    let e =
        Replacement::prepare(bound.clone(), None, "stage-a".into(), "socket".into()).unwrap_err();
    directory::FAIL_METADATA.with(|failure| *failure.borrow_mut() = None);
    assert_eq!(e.raw_os_error(), Some(libc::EACCES));
    let replacement =
        Replacement::prepare(bound.clone(), None, "stage-a".into(), "socket".into()).unwrap();
    let mut publication = Publication::default();
    publication.commit();
    let e = publication.publish(replacement).unwrap_err();
    assert_eq!(
        e.get_ref().unwrap().downcast_ref::<Error>(),
        Some(&Error::PublicationCompleted)
    );
    assert!(bound.owns("stage-a"));
}

#[test]
fn rollback_new_endpoint_retains_external_listener_and_restores_staging() {
    let root = Root::new();
    let directory = root.directory();
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let bound = Rc::new(BoundSocket::bind(owner, "stage-a").unwrap());
    let replacement =
        Replacement::prepare(bound.clone(), None, "stage-a".into(), "socket".into()).unwrap();
    let mut publication = Publication::default();
    publication.publish(replacement).unwrap();
    assert_eq!(bound.basename(), "socket");
    publication.rollback();
    assert_eq!(bound.basename(), "stage-a");
    assert!(bound.owns("stage-a"));
    assert!(bound.absent("socket"));
    let _client = UnixStream::connect(file_path(&directory).join("stage-a")).unwrap();
    bound.accept().unwrap();
}

#[test]
fn non_permission_remove_bits_do_not_trigger_fchmod() {
    let root = Root::new();
    let directory = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_PATH | libc::O_DIRECTORY)
        .open(&root.0)
        .unwrap();
    // An unnecessary fchmod would fail with EBADF on this O_PATH descriptor.
    restrict_directory(&directory, libc::S_IFDIR).unwrap();
}

#[cfg(feature = "test-util")]
#[test]
fn explicit_unlock_releases_cloned_open_file_description() {
    let root = Root::new();
    let directory = root.directory();
    let owner = EndpointOwner::acquire(&directory, layout()).unwrap();
    let inherited = owner.clone_lock_for_test().unwrap();
    assert!(EndpointOwner::acquire(&directory, layout()).is_err());
    drop(owner);
    let next = EndpointOwner::acquire(&directory, layout()).unwrap();
    assert!(same_inode(
        &inherited.metadata().unwrap(),
        &next.lock.metadata().unwrap()
    ));
    drop(inherited);
    assert!(EndpointOwner::acquire(&directory, layout()).is_err());
}

#[test]
fn stat_failure_does_not_remove_foreign_path_or_recovery_witness() {
    let root = Root::new();
    let directory = root.directory();
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let bound = BoundSocket::bind(owner.clone(), "stage-a").unwrap();
    fs::remove_file(root.0.join("stage-a")).unwrap();
    fs::write(root.0.join("stage-a"), b"foreign").unwrap();
    directory::FAIL_METADATA.with(|failure| *failure.borrow_mut() = Some("stage-a".into()));
    drop(bound);
    directory::FAIL_METADATA.with(|failure| *failure.borrow_mut() = None);
    assert_eq!(fs::read(root.0.join("stage-a")).unwrap(), b"foreign");
    assert!(root.0.join("witness-stage-a").exists());
    drop(owner);
    assert!(EndpointOwner::acquire(&directory, layout()).is_err());
    assert!(root.0.join("witness-stage-a").exists());
    assert_eq!(fs::read(root.0.join("stage-a")).unwrap(), b"foreign");
}
