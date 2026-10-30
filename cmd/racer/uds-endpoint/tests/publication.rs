use std::cell::{Cell, RefCell};
use std::fs::{self, File};
use std::io;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::atomic::{AtomicU64, Ordering};
use uds_endpoint::publication::{Endpoint, Publication, Rename, Replacement};
use uds_endpoint::{BoundSocket, EndpointOwner, Layout, file_path, open_directory};

const CANONICAL: &str = "endpoint.sock";
fn layout() -> Layout {
    Layout::new("endpoint.lock", CANONICAL, "owned-", |name| {
        name.starts_with("pending-")
    })
    .unwrap()
}

struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let root = Self(PathBuf::from(env!("CARGO_TARGET_TMPDIR")).join(format!(
            "publication-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        )));
        fs::create_dir_all(&root.0).unwrap();
        fs::set_permissions(&root.0, fs::Permissions::from_mode(0o700)).unwrap();
        root
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

struct Fixture {
    directory: Rc<File>,
    owner: Rc<EndpointOwner>,
    _root: Root,
}
impl Fixture {
    fn new() -> Self {
        let root = Root::new();
        let directory = Rc::new(open_directory(&root.0, 0o700).unwrap());
        let owner = Rc::new(EndpointOwner::acquire(&directory, layout()).unwrap());
        Self {
            directory,
            owner,
            _root: root,
        }
    }
    fn path(&self, name: &str) -> PathBuf {
        file_path(&self.directory).join(name)
    }
    fn bind(&self, name: &str) -> Rc<BoundSocket> {
        Rc::new(BoundSocket::bind(self.owner.clone(), name).unwrap())
    }
    fn initial(&self) -> Rc<BoundSocket> {
        let socket = self.bind("pending-old");
        let replacement =
            Replacement::prepare(socket.clone(), None, "pending-old".into(), CANONICAL.into())
                .unwrap();
        let mut publication = Publication::default();
        publication.publish(replacement).unwrap();
        publication.commit();
        socket
    }
}

#[test]
fn prepare_does_not_publish_and_drop_restores_exchanged_inode() {
    let fixture = Fixture::new();
    let old = fixture.initial();
    let next = fixture.bind("pending-next");
    let replacement = Replacement::prepare(
        next.clone(),
        Some(old.clone()),
        "pending-next".into(),
        CANONICAL.into(),
    )
    .unwrap();
    assert!(old.owns(CANONICAL));
    assert!(next.owns("pending-next"));
    let mut publication = Publication::default();
    publication.publish(replacement).unwrap();
    assert!(next.owns(CANONICAL));
    assert!(old.owns("pending-next"));
    let _client = UnixStream::connect(fixture.path(CANONICAL)).unwrap();
    next.accept().unwrap();
    drop(publication);
    assert!(old.owns(CANONICAL));
    assert!(next.owns("pending-next"));
    assert_eq!(old.basename(), CANONICAL);
    assert_eq!(next.basename(), "pending-next");
    drop(next);
    assert!(!fixture.path("pending-next").exists());
    assert!(!fixture.path("owned-pending-next").exists());
    assert!(old.owns(CANONICAL));
}

#[test]
fn no_replace_handles_absent_path_and_refuses_racing_foreign_destination() {
    for race in [false, true] {
        let fixture = Fixture::new();
        let next = fixture.bind("pending-next");
        let replacement =
            Replacement::prepare(next.clone(), None, "pending-next".into(), CANONICAL.into())
                .unwrap();
        if race {
            fs::write(fixture.path(CANONICAL), b"foreign").unwrap();
        }
        let mut publication = Publication::default();
        let result = publication.publish(replacement);
        assert_eq!(result.is_err(), race);
        if race {
            assert_eq!(next.basename(), "pending-next");
        } else {
            assert!(next.owns(CANONICAL));
            assert_eq!(next.basename(), CANONICAL);
        }
        drop(next);
        drop(publication);
        assert!(!fixture.path("pending-next").exists());
        assert!(!fixture.path("owned-pending-next").exists());
        if race {
            assert_eq!(fs::read(fixture.path(CANONICAL)).unwrap(), b"foreign");
        } else {
            assert!(!fixture.path(CANONICAL).exists());
        }
    }
}

#[test]
fn prepare_rejects_wrong_directory_and_foreign_canonical() {
    let fixture = Fixture::new();
    let old = fixture.initial();
    let other = Fixture::new();
    let next = other.bind("pending-next");
    assert!(
        Replacement::prepare(
            next,
            Some(old.clone()),
            "pending-next".into(),
            CANONICAL.into()
        )
        .is_err()
    );
    assert!(old.owns(CANONICAL));
    let next = fixture.bind("pending-next");
    fs::remove_file(fixture.path(CANONICAL)).unwrap();
    fs::write(fixture.path(CANONICAL), b"foreign").unwrap();
    for previous in [None, Some(old)] {
        assert!(
            Replacement::prepare(
                next.clone(),
                previous,
                "pending-next".into(),
                CANONICAL.into()
            )
            .is_err()
        );
    }
    assert_eq!(fs::read(fixture.path(CANONICAL)).unwrap(), b"foreign");
}

#[test]
fn publish_rechecks_both_exchange_inodes() {
    for replaced in [CANONICAL, "pending-next"] {
        let fixture = Fixture::new();
        let old = fixture.initial();
        let next = fixture.bind("pending-next");
        let replacement = Replacement::prepare(
            next.clone(),
            Some(old.clone()),
            "pending-next".into(),
            CANONICAL.into(),
        )
        .unwrap();
        fs::remove_file(fixture.path(replaced)).unwrap();
        fs::write(fixture.path(replaced), b"foreign").unwrap();
        let mut publication = Publication::default();
        assert!(publication.publish(replacement).is_err());
        assert_eq!(next.basename(), "pending-next");
        assert_eq!(old.basename(), CANONICAL);
        assert_eq!(fs::read(fixture.path(replaced)).unwrap(), b"foreign");
    }
}

#[test]
fn rollback_restores_missing_canonical_but_never_swaps_foreign_inodes() {
    for replaced in ["missing", CANONICAL, "pending-next"] {
        let fixture = Fixture::new();
        let old = fixture.initial();
        let next = fixture.bind("pending-next");
        let replacement = Replacement::prepare(
            next.clone(),
            Some(old.clone()),
            "pending-next".into(),
            CANONICAL.into(),
        )
        .unwrap();
        let mut publication = Publication::default();
        publication.publish(replacement).unwrap();
        let path = fixture.path(if replaced == "missing" {
            CANONICAL
        } else {
            replaced
        });
        fs::remove_file(&path).unwrap();
        if replaced != "missing" {
            fs::write(&path, b"foreign").unwrap();
        }
        publication.rollback();
        publication.rollback();
        if replaced == "missing" {
            assert!(old.owns(CANONICAL));
            assert_eq!(old.basename(), CANONICAL);
        } else {
            assert_eq!(fs::read(&path).unwrap(), b"foreign");
            assert_eq!(old.basename(), "pending-next");
        }
        drop(publication);
        drop(next);
        if replaced != "missing" {
            assert_eq!(fs::read(&path).unwrap(), b"foreign");
        } else {
            assert!(old.owns(CANONICAL));
        }
    }
}

// Real filesystem adapter observes rename order and injects a single failure at
// the same boundary used by Racer's test-only adapter, without a runtime.
#[derive(Default)]
struct Calls {
    log: RefCell<Vec<(usize, Rename)>>,
    fail_after: Cell<Option<usize>>,
}
struct Observed {
    socket: Rc<BoundSocket>,
    id: usize,
    calls: Rc<Calls>,
}
impl Endpoint for Observed {
    type Error = io::Error;
    fn ownership_error() -> io::Error {
        io::Error::other("not owned")
    }
    fn owns(&self, name: &str) -> bool {
        self.socket.owns(name)
    }
    fn absent(&self, name: &str) -> bool {
        self.socket.absent(name)
    }
    fn same_directory(&self, other: &Self) -> io::Result<bool> {
        self.socket.same_directory(&other.socket)
    }
    fn set_basename(&self, name: String) {
        self.socket.set_basename(name);
    }
    fn rename(&self, from: &str, to: &str, mode: Rename) -> io::Result<()> {
        self.calls.log.borrow_mut().push((self.id, mode));
        match self.calls.fail_after.get() {
            Some(0) => {
                self.calls.fail_after.set(None);
                return Err(io::Error::other("injected rename failure"));
            }
            Some(count) => self.calls.fail_after.set(Some(count - 1)),
            None => {}
        }
        self.socket.rename(from, to, mode)
    }
}

#[test]
fn failed_later_publish_rolls_back_successes_in_reverse_order() {
    let fixtures = [Fixture::new(), Fixture::new(), Fixture::new()];
    let calls = Rc::new(Calls::default());
    let old: Vec<_> = fixtures.iter().map(Fixture::initial).collect();
    let next: Vec<_> = fixtures.iter().map(|f| f.bind("pending-next")).collect();
    let mut publication = Publication::default();
    calls.fail_after.set(Some(2));
    for id in 0..3 {
        let previous = Rc::new(Observed {
            socket: old[id].clone(),
            id,
            calls: calls.clone(),
        });
        let next = Rc::new(Observed {
            socket: next[id].clone(),
            id,
            calls: calls.clone(),
        });
        let replacement = Replacement::prepare(
            next,
            Some(previous),
            "pending-next".into(),
            CANONICAL.into(),
        )
        .unwrap();
        assert_eq!(publication.publish(replacement).is_err(), id == 2);
    }
    drop(publication);
    assert_eq!(
        &*calls.log.borrow(),
        &[
            (0, Rename::Exchange),
            (1, Rename::Exchange),
            (2, Rename::Exchange),
            (1, Rename::Exchange),
            (0, Rename::Exchange)
        ]
    );
    for id in 0..3 {
        assert!(old[id].owns(CANONICAL));
        assert!(next[id].owns("pending-next"));
    }
}

#[test]
fn failed_rollback_is_not_retried_and_keeps_cleanup_names_matching_inodes() {
    let fixture = Fixture::new();
    let old = fixture.initial();
    let next = fixture.bind("pending-next");
    let calls = Rc::new(Calls::default());
    let mut publication = Publication::default();
    let previous = Rc::new(Observed {
        socket: old.clone(),
        id: 0,
        calls: calls.clone(),
    });
    let observed = Rc::new(Observed {
        socket: next.clone(),
        id: 1,
        calls: calls.clone(),
    });
    publication
        .publish(
            Replacement::prepare(
                observed,
                Some(previous),
                "pending-next".into(),
                CANONICAL.into(),
            )
            .unwrap(),
        )
        .unwrap();
    calls.fail_after.set(Some(0));
    publication.rollback();
    publication.rollback();
    drop(publication);
    assert_eq!(calls.log.borrow().len(), 2);
    assert!(old.owns("pending-next"));
    assert!(next.owns(CANONICAL));
    assert_eq!(old.basename(), "pending-next");
    assert_eq!(next.basename(), CANONICAL);
}

#[test]
fn commit_does_not_rename_or_drop_owners_and_allows_deferred_cleanup() {
    let fixture = Fixture::new();
    let old = fixture.initial();
    let next = fixture.bind("pending-next");
    let calls = Rc::new(Calls::default());
    let previous = Rc::new(Observed {
        socket: old.clone(),
        id: 0,
        calls: calls.clone(),
    });
    let weak = Rc::downgrade(&previous);
    let observed = Rc::new(Observed {
        socket: next.clone(),
        id: 1,
        calls: calls.clone(),
    });
    let mut publication = Publication::default();
    publication
        .publish(
            Replacement::prepare(
                observed,
                Some(previous),
                "pending-next".into(),
                CANONICAL.into(),
            )
            .unwrap(),
        )
        .unwrap();
    drop(old);
    let deferred: Vec<_> = publication.previous().cloned().collect();
    publication.commit();
    publication.rollback();
    assert!(weak.upgrade().is_some());
    assert_eq!(calls.log.borrow().len(), 1);
    drop(publication);
    assert!(fixture.path("pending-next").exists());
    assert!(next.owns(CANONICAL));
    drop(deferred);
    assert!(weak.upgrade().is_none());
    assert!(!fixture.path("pending-next").exists());
    assert_eq!(
        fs::metadata(fixture.path(CANONICAL)).unwrap().ino(),
        next.identity().1
    );
}

#[test]
fn mixed_new_and_replacement_rollback_preserves_held_listeners() {
    let fixtures = [Fixture::new(), Fixture::new(), Fixture::new()];
    let old = fixtures[0].initial();
    let next: Vec<_> = fixtures.iter().map(|f| f.bind("pending-next")).collect();
    let calls = Rc::new(Calls::default());
    let previous = Rc::new(Observed {
        socket: old.clone(),
        id: 0,
        calls: calls.clone(),
    });
    let mut publication = Publication::default();
    calls.fail_after.set(Some(2));
    for (id, socket) in next.iter().enumerate() {
        let observed = Rc::new(Observed {
            socket: socket.clone(),
            id,
            calls: calls.clone(),
        });
        let replacement = Replacement::prepare(
            observed,
            (id == 0).then(|| previous.clone()),
            "pending-next".into(),
            CANONICAL.into(),
        )
        .unwrap();
        assert_eq!(publication.publish(replacement).is_err(), id == 2);
    }
    publication.rollback();
    publication.rollback();
    drop(publication);
    assert_eq!(
        &*calls.log.borrow(),
        &[
            (0, Rename::Exchange),
            (1, Rename::NoReplace),
            (2, Rename::NoReplace),
            (1, Rename::NoReplace),
            (0, Rename::Exchange)
        ]
    );
    assert!(old.owns(CANONICAL));
    for (id, socket) in next.iter().enumerate() {
        assert_eq!(socket.basename(), "pending-next");
        assert!(socket.owns("pending-next"));
        if id != 0 {
            assert!(!fixtures[id].path(CANONICAL).exists());
        }
        let _client = UnixStream::connect(fixtures[id].path("pending-next")).unwrap();
        socket.accept().unwrap();
    }
}

#[test]
fn completed_journals_reject_publish_after_commit_or_rollback() {
    for committed in [false, true] {
        let fixture = Fixture::new();
        let next = fixture.bind("pending-next");
        let replacement =
            Replacement::prepare(next.clone(), None, "pending-next".into(), CANONICAL.into())
                .unwrap();
        let mut publication = Publication::default();
        if committed {
            publication.commit();
        } else {
            publication.rollback();
        }
        let error = publication.publish(replacement).unwrap_err();
        assert_eq!(
            error
                .get_ref()
                .unwrap()
                .downcast_ref::<uds_endpoint::Error>(),
            Some(&uds_endpoint::Error::PublicationCompleted)
        );
        assert!(next.owns("pending-next"));
        assert_eq!(next.basename(), "pending-next");
        assert!(!fixture.path(CANONICAL).exists());
    }
}

#[test]
fn identical_names_same_rc_and_unconfigured_canonical_are_rejected() {
    let fixture = Fixture::new();
    let socket = fixture.bind("pending-next");
    assert!(
        Replacement::prepare(
            socket.clone(),
            None,
            "pending-next".into(),
            "pending-next".into()
        )
        .is_err()
    );
    // Hard links make both ownership probes true, but cannot make one Rc two owners.
    fs::hard_link(fixture.path("pending-next"), fixture.path(CANONICAL)).unwrap();
    assert!(
        Replacement::prepare(
            socket.clone(),
            Some(socket.clone()),
            "pending-next".into(),
            CANONICAL.into()
        )
        .is_err()
    );
    assert!(socket.owns("pending-next"));
    assert!(socket.owns(CANONICAL));
    fs::remove_file(fixture.path(CANONICAL)).unwrap();
    assert!(
        Replacement::prepare(
            socket.clone(),
            None,
            "pending-next".into(),
            "other.sock".into()
        )
        .is_err()
    );
    assert_eq!(socket.basename(), "pending-next");
    assert!(!fixture.path("other.sock").exists());
}

#[test]
fn guarded_new_endpoint_rollback_preserves_foreign_names_and_failed_rename() {
    #[derive(Clone, Copy, Debug)]
    enum Guard {
        ForeignCanonical,
        OccupiedTemporary,
        RenameFailure,
    }
    for guard in [
        Guard::ForeignCanonical,
        Guard::OccupiedTemporary,
        Guard::RenameFailure,
    ] {
        let fixture = Fixture::new();
        let next = fixture.bind("pending-next");
        let calls = Rc::new(Calls::default());
        let observed = Rc::new(Observed {
            socket: next.clone(),
            id: 0,
            calls: calls.clone(),
        });
        let mut publication = Publication::default();
        publication
            .publish(
                Replacement::prepare(observed, None, "pending-next".into(), CANONICAL.into())
                    .unwrap(),
            )
            .unwrap();
        match guard {
            Guard::ForeignCanonical => {
                fs::remove_file(fixture.path(CANONICAL)).unwrap();
                fs::write(fixture.path(CANONICAL), b"foreign").unwrap();
            }
            Guard::OccupiedTemporary => {
                fs::write(fixture.path("pending-next"), b"foreign").unwrap()
            }
            Guard::RenameFailure => calls.fail_after.set(Some(0)),
        }
        publication.rollback();
        publication.rollback();
        drop(publication);
        assert_eq!(next.basename(), CANONICAL, "{guard:?}");
        assert_eq!(
            calls.log.borrow().len(),
            if matches!(guard, Guard::RenameFailure) {
                2
            } else {
                1
            }
        );
        drop(next);
        match guard {
            Guard::ForeignCanonical => {
                assert_eq!(fs::read(fixture.path(CANONICAL)).unwrap(), b"foreign")
            }
            Guard::OccupiedTemporary => {
                assert_eq!(fs::read(fixture.path("pending-next")).unwrap(), b"foreign")
            }
            Guard::RenameFailure => assert!(!fixture.path(CANONICAL).exists()),
        }
        assert!(!fixture.path("owned-pending-next").exists());
    }
}

#[test]
fn absent_canonical_restore_failure_is_guarded_and_not_retried() {
    let fixture = Fixture::new();
    let old = fixture.initial();
    let next = fixture.bind("pending-next");
    let calls = Rc::new(Calls::default());
    let previous = Rc::new(Observed {
        socket: old.clone(),
        id: 0,
        calls: calls.clone(),
    });
    let observed = Rc::new(Observed {
        socket: next.clone(),
        id: 1,
        calls: calls.clone(),
    });
    let mut publication = Publication::default();
    publication
        .publish(
            Replacement::prepare(
                observed,
                Some(previous),
                "pending-next".into(),
                CANONICAL.into(),
            )
            .unwrap(),
        )
        .unwrap();
    fs::remove_file(fixture.path(CANONICAL)).unwrap();
    calls.fail_after.set(Some(0));
    publication.rollback();
    drop(publication);
    assert_eq!(
        &*calls.log.borrow(),
        &[(1, Rename::Exchange), (1, Rename::NoReplace)]
    );
    assert!(old.owns("pending-next"));
    assert_eq!(old.basename(), "pending-next");
    assert_eq!(next.basename(), CANONICAL);
    assert!(!fixture.path(CANONICAL).exists());
}

#[test]
fn exchange_does_not_migrate_connections_queued_on_previous_listener() {
    let fixture = Fixture::new();
    let old = fixture.initial();
    let _queued_before = UnixStream::connect(fixture.path(CANONICAL)).unwrap();
    let next = fixture.bind("pending-next");
    let mut publication = Publication::default();
    publication
        .publish(
            Replacement::prepare(
                next.clone(),
                Some(old.clone()),
                "pending-next".into(),
                CANONICAL.into(),
            )
            .unwrap(),
        )
        .unwrap();
    assert_eq!(next.accept().unwrap_err().kind(), io::ErrorKind::WouldBlock);
    old.accept().unwrap();
    let _queued_after = UnixStream::connect(fixture.path(CANONICAL)).unwrap();
    next.accept().unwrap();
    assert_eq!(old.accept().unwrap_err().kind(), io::ErrorKind::WouldBlock);
    publication.commit();
}
