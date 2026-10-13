use super::*;
use publication::{Endpoint, Replacement};
use std::ffi::OsStr;
use std::os::unix::fs::{PermissionsExt, symlink};
use std::sync::atomic::{AtomicU64, Ordering};

struct Root(PathBuf);

fn fixture_base(explicit: Option<&OsStr>, target: Option<&OsStr>, cwd: &Path) -> PathBuf {
    let base = if let Some(path) = explicit.filter(|path| !path.is_empty()) {
        PathBuf::from(path)
    } else if let Some(path) = target.filter(|path| !path.is_empty()) {
        PathBuf::from(path).join("tmp")
    } else {
        Path::new(env!("CARGO_MANIFEST_DIR"))
            .parent()
            .unwrap()
            .join("target/tmp")
    };
    cwd.join(base)
}

pub(super) fn fixture_root(prefix: &str) -> PathBuf {
    static NEXT: AtomicU64 = AtomicU64::new(0);
    fixture_base(
        std::env::var_os("UDS_TEST_TMPDIR").as_deref(),
        std::env::var_os("CARGO_TARGET_DIR").as_deref(),
        &std::env::current_dir().unwrap(),
    )
    .join(format!(
        "{prefix}-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ))
}

#[test]
fn fixture_root_selection_honors_overrides_without_changing_environment() {
    let cwd = Path::new("/workspace/crate");
    let fallback = Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .unwrap()
        .join("target/tmp");
    for (explicit, target, expected) in [
        (None, None, fallback.clone()),
        (Some(""), Some(""), fallback),
        (
            None,
            Some("/workspace/build"),
            PathBuf::from("/workspace/build/tmp"),
        ),
        (None, Some("build"), cwd.join("build/tmp")),
        (Some("fixtures"), Some("build"), cwd.join("fixtures")),
        (
            Some("/workspace/fixtures"),
            Some("build"),
            PathBuf::from("/workspace/fixtures"),
        ),
        (Some(""), Some("build"), cwd.join("build/tmp")),
    ] {
        assert_eq!(
            fixture_base(explicit.map(OsStr::new), target.map(OsStr::new), cwd),
            expected
        );
    }
}

impl Root {
    fn new() -> Self {
        let path = fixture_root("core");
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
fn stale_shutdown_disables_inherited_listener_alias() {
    let root = Root::new();
    let directory = root.directory();
    let witness = file_path(&directory).join("witness-stage-a");
    let listener = UnixListener::bind(&witness).unwrap();
    let inherited = listener.try_clone().unwrap();
    stale(listener);
    assert_eq!(
        UnixStream::connect(&witness).unwrap_err().raw_os_error(),
        Some(libc::ECONNREFUSED)
    );
    EndpointOwner::acquire(&directory, layout()).unwrap();
    assert!(!witness.exists());
    drop(inherited);
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
            stale(listener);
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
            assert_eq!(fs::metadata(witness).unwrap().mode() & 0o7777, 0);
            assert_eq!(
                fs::metadata(root.0.join("socket")).unwrap().mode() & 0o7777,
                0
            );
        } else {
            result.unwrap();
            assert!(!witness.exists());
            assert!(!root.0.join("socket").exists());
        }
        drop(listener);
    }
}

#[test]
fn recovery_restores_mode_on_probe_error() {
    let root = Root::new();
    let directory = root.directory();
    let witness = root.0.join("witness-stage-a");
    let _socket =
        std::os::unix::net::UnixDatagram::bind(file_path(&directory).join("witness-stage-a"))
            .unwrap();
    fs::hard_link(&witness, root.0.join("socket")).unwrap();
    fs::set_permissions(&witness, fs::Permissions::from_mode(0o000)).unwrap();

    let failure = EndpointOwner::acquire(&directory, layout()).unwrap_err();
    assert_eq!(failure.raw_os_error(), Some(libc::EPROTOTYPE));
    for name in ["witness-stage-a", "socket"] {
        assert_eq!(fs::metadata(root.0.join(name)).unwrap().mode() & 0o7777, 0);
    }
}

#[test]
fn recovery_restores_mixed_witnesses_before_any_cleanup() {
    for live_index in 0..2 {
        let root = Root::new();
        let directory = root.directory();
        let names = ["stage-a", "stage-b"];
        let modes = [0o040, 0o1400];
        let mut live = None;
        for (index, name) in names.iter().enumerate() {
            let witness = format!("witness-{name}");
            let listener = UnixListener::bind(file_path(&directory).join(&witness)).unwrap();
            fs::hard_link(root.0.join(&witness), root.0.join(name)).unwrap();
            fs::set_permissions(
                root.0.join(&witness),
                fs::Permissions::from_mode(modes[index]),
            )
            .unwrap();
            if index == live_index {
                fs::hard_link(root.0.join(&witness), root.0.join("socket")).unwrap();
                live = Some(listener);
            } else {
                stale(listener);
            }
        }

        let failure = EndpointOwner::acquire(&directory, layout()).unwrap_err();
        assert_eq!(
            failure.get_ref().unwrap().downcast_ref::<Error>(),
            Some(&Error::LiveSocket)
        );
        for (name, mode) in names.iter().zip(modes) {
            for entry in [name.to_string(), format!("witness-{name}")] {
                assert_eq!(
                    fs::metadata(root.0.join(entry)).unwrap().mode() & 0o7777,
                    mode
                );
            }
        }
        assert_eq!(
            fs::metadata(root.0.join("socket")).unwrap().mode() & 0o7777,
            modes[live_index]
        );

        stale(live.unwrap());
        EndpointOwner::acquire(&directory, layout()).unwrap();
        assert_eq!(fs::read_dir(&root.0).unwrap().count(), 1);
        assert!(root.0.join("lock").exists());
    }
}

#[test]
fn drop_requires_safe_directory_permissions_before_cleanup() {
    for mode in [0o700, 0o755, 0o770, 0o707, 0o777] {
        for foreign in [false, true] {
            let root = Root::new();
            let directory = root.directory();
            let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
            let bound = BoundSocket::bind(owner.clone(), "stage-a").unwrap();
            let witness = root.0.join("witness-stage-a");
            let basename = root.0.join("stage-a");
            let identity = fs::metadata(&witness).unwrap();
            if foreign {
                fs::remove_file(&basename).unwrap();
                fs::write(&basename, b"foreign").unwrap();
            }
            directory
                .set_permissions(fs::Permissions::from_mode(mode))
                .unwrap();
            drop(bound);
            let unsafe_directory = mode & 0o022 != 0;
            assert_eq!(witness.exists(), unsafe_directory, "mode {mode:o}");
            assert_eq!(
                basename.exists(),
                unsafe_directory || foreign,
                "mode {mode:o}"
            );
            if unsafe_directory {
                assert!(same_inode(&fs::metadata(&witness).unwrap(), &identity));
            }
            if foreign {
                assert_eq!(fs::read(&basename).unwrap(), b"foreign");
            }
            directory
                .set_permissions(fs::Permissions::from_mode(0o700))
                .unwrap();
            drop(owner);
            if foreign {
                assert!(EndpointOwner::acquire(&directory, layout()).is_err());
                assert_eq!(fs::read(&basename).unwrap(), b"foreign");
                fs::remove_file(&basename).unwrap();
            }
            EndpointOwner::acquire(&directory, layout()).unwrap();
            assert!(!witness.exists());
            assert!(!basename.exists());
        }
    }
}

#[test]
fn drop_preserves_names_when_owner_lock_validation_fails() {
    for failure in ["mode", "replacement", "metadata"] {
        let root = Root::new();
        let directory = root.directory();
        let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
        let bound = BoundSocket::bind(owner.clone(), "stage-a").unwrap();
        let lock = root.0.join("lock");
        match failure {
            "mode" => fs::set_permissions(&lock, fs::Permissions::from_mode(0o666)).unwrap(),
            "replacement" => {
                fs::rename(&lock, root.0.join("old-lock")).unwrap();
                fs::write(&lock, b"foreign").unwrap();
                fs::set_permissions(&lock, fs::Permissions::from_mode(0o600)).unwrap();
            }
            _ => directory::FAIL_METADATA.with(|value| *value.borrow_mut() = Some("lock".into())),
        }
        assert!(owner.validate(&directory).is_err());
        drop(bound);
        directory::FAIL_METADATA.with(|value| *value.borrow_mut() = None);
        for name in ["stage-a", "witness-stage-a"] {
            assert!(root.0.join(name).exists(), "{failure}: {name}");
        }
        if failure == "replacement" {
            assert_eq!(fs::read(&lock).unwrap(), b"foreign");
            fs::remove_file(&lock).unwrap();
            fs::rename(root.0.join("old-lock"), &lock).unwrap();
        }
        fs::set_permissions(&lock, fs::Permissions::from_mode(0o600)).unwrap();
        drop(owner);
        EndpointOwner::acquire(&directory, layout()).unwrap();
        assert!(!root.0.join("stage-a").exists());
        assert!(!root.0.join("witness-stage-a").exists());
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
fn checked_publication_probe_preserves_stat_error() {
    let root = Root::new();
    let directory = root.directory();
    let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
    let bound = Rc::new(BoundSocket::bind(owner, "stage-a").unwrap());
    directory::FAIL_METADATA.with(|failure| *failure.borrow_mut() = Some("stage-a".into()));
    let e =
        Replacement::prepare(bound.clone(), None, "stage-a".into(), "socket".into()).unwrap_err();
    directory::FAIL_METADATA.with(|failure| *failure.borrow_mut() = None);
    assert_eq!(e.raw_os_error(), Some(libc::EACCES));
    assert!(bound.owns("stage-a"));
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
