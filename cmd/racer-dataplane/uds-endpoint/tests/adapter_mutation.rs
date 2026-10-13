use std::fs;
use std::os::unix::fs::{MetadataExt, PermissionsExt, symlink};
use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::sync::atomic::{AtomicU64, Ordering};
use uds_endpoint::publication::{Endpoint, Rename};
use uds_endpoint::{BoundSocket, EndpointOwner, Layout, file_path, open_directory};

const CANONICAL: &str = "endpoint.sock";
const TEMPORARY: &str = "pending-next";
const WITNESS: &str = "owned-pending-next";
const LOCK: &str = "endpoint.lock";

struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let root = Self(PathBuf::from(env!("CARGO_TARGET_TMPDIR")).join(format!(
            "adapter-mutation-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        )));
        fs::create_dir_all(&root.0).unwrap();
        fs::set_permissions(&root.0, fs::Permissions::from_mode(0o700)).unwrap();
        root
    }

    fn path(&self, name: &str) -> PathBuf {
        self.0.join(name)
    }

    fn socket(&self) -> BoundSocket {
        let directory = open_directory(&self.0, 0o700).unwrap();
        let layout = Layout::new(LOCK, CANONICAL, "owned-", |name| {
            name.starts_with("pending-")
        })
        .unwrap();
        let owner = Rc::new(EndpointOwner::acquire(&directory, layout).unwrap());
        BoundSocket::bind(owner, TEMPORARY).unwrap()
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

fn identity(path: &Path) -> (u64, u64) {
    let metadata = fs::symlink_metadata(path).unwrap();
    (metadata.dev(), metadata.ino())
}

#[test]
fn direct_rename_preserves_lock_witnesses_and_foreign_sources() {
    for mode in [Rename::NoReplace, Rename::Exchange] {
        for from in [LOCK, WITNESS, "unrelated", "pending-foreign"] {
            let root = Root::new();
            let socket = root.socket();
            fs::write(root.path("unrelated"), b"unrelated").unwrap();
            fs::write(root.path("pending-foreign"), b"foreign").unwrap();
            if mode == Rename::Exchange {
                fs::write(root.path(CANONICAL), b"destination").unwrap();
            }
            let source = identity(&root.path(from));
            assert!(socket.rename(from, CANONICAL, mode).is_err());
            assert_eq!(identity(&root.path(from)), source);
            if mode == Rename::Exchange {
                assert_eq!(fs::read(root.path(CANONICAL)).unwrap(), b"destination");
            } else {
                assert!(!root.path(CANONICAL).exists());
            }
            assert_eq!(socket.basename(), TEMPORARY);
            assert!(socket.owns(TEMPORARY));
            assert!(socket.owns(WITNESS));
            socket.set_mode(0o600).unwrap();
        }
    }
}

#[test]
fn direct_rename_rejects_non_socket_namespaces_and_malformed_targets() {
    for mode in [Rename::NoReplace, Rename::Exchange] {
        for to in [
            LOCK,
            WITNESS,
            "owned-pending-other",
            "unrelated",
            "missing",
            "../escape",
            "pending-../escape",
            "pending-\0bad",
            TEMPORARY,
        ] {
            let root = Root::new();
            let socket = root.socket();
            fs::write(root.path("unrelated"), b"foreign").unwrap();
            let lock = identity(&root.path(LOCK));
            assert!(socket.rename(TEMPORARY, to, mode).is_err(), "{to:?}");
            assert!(socket.owns(TEMPORARY));
            assert!(socket.owns(WITNESS));
            assert_eq!(identity(&root.path(LOCK)), lock);
            assert_eq!(fs::read(root.path("unrelated")).unwrap(), b"foreign");
            assert!(!root.path("owned-pending-other").exists());
            assert!(!root.path("missing").exists());
            socket.set_mode(0o600).unwrap();
        }
    }
}

#[test]
fn basename_adoption_requires_socket_identity_without_following_symlinks() {
    let root = Root::new();
    let socket = root.socket();
    fs::write(root.path("pending-foreign"), b"foreign").unwrap();
    let directory = open_directory(&root.0, 0o700).unwrap();
    let foreign_socket =
        std::os::unix::net::UnixListener::bind(file_path(&directory).join("pending-socket"))
            .unwrap();
    symlink(TEMPORARY, root.path("pending-link")).unwrap();
    for name in [
        "",
        ".",
        "..",
        "../escape",
        "pending-\0bad",
        "pending-missing",
        "pending-foreign",
        "pending-socket",
        "pending-link",
        LOCK,
        WITNESS,
        CANONICAL,
        &"p".repeat(256),
    ] {
        socket.set_basename(name.into());
        assert_eq!(socket.basename(), TEMPORARY, "{name:?}");
    }
    drop(socket);
    assert!(!root.path(TEMPORARY).exists());
    assert!(!root.path(WITNESS).exists());
    assert!(root.path(LOCK).exists());
    assert_eq!(fs::read(root.path("pending-foreign")).unwrap(), b"foreign");
    assert!(root.path("pending-socket").exists());
    assert!(fs::symlink_metadata(root.path("pending-link")).is_ok());
    drop(foreign_socket);
}

#[test]
fn direct_owned_rename_and_adoption_work_in_both_directions() {
    let root = Root::new();
    let socket = root.socket();
    for (from, to) in [(TEMPORARY, CANONICAL), (CANONICAL, "pending-restored")] {
        socket.rename(from, to, Rename::NoReplace).unwrap();
        assert_eq!(socket.basename(), from);
        socket.set_basename(to.into());
        assert_eq!(socket.basename(), to);
        assert!(socket.owns(to));
        assert!(socket.owns(WITNESS));
    }
    drop(socket);
    assert!(!root.path("pending-restored").exists());
    assert!(!root.path(WITNESS).exists());
}
