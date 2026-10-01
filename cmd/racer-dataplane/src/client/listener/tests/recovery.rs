//! Crash recovery and rejection of foreign or replaced endpoint ownership.
use super::*;

#[test]
fn crash_owner_child() {
    let Ok(root) = std::env::var("RACER_SOCKET_CRASH_ROOT") else {
        return;
    };
    let mut fixture = Fixture::new();
    fs::remove_dir(&fixture.root.0).unwrap();
    fixture.root.0 = root.into();
    fixture.listeners.root = fixture.root.0.clone();
    fixture.reconcile(&[definition()]).unwrap();
    let mut changed = definition();
    changed.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let prepared = if std::env::var("RACER_SOCKET_CRASH_STAGE").unwrap() == "prepared" {
        Some(futures::executor::block_on(fixture.listeners.prepare(&[changed], &scope())).unwrap())
    } else {
        None
    };
    fs::write(fixture.root.0.join("ready"), b"ready").unwrap();
    loop {
        std::hint::black_box(&prepared);
        std::thread::sleep(Duration::from_secs(1));
    }
}

#[test]
fn sigkill_restart_recovers_committed_and_prepared_endpoints() {
    use std::process::{Command, Stdio};
    struct Child(std::process::Child);
    impl Drop for Child {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }
    for stage in ["committed", "prepared"] {
        let fixture = Fixture::new();
        let mut child = Child(
            Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "client::listener::tests::recovery::crash_owner_child",
                    "--nocapture",
                ])
                .env("RACER_SOCKET_CRASH_ROOT", &fixture.root.0)
                .env("RACER_SOCKET_CRASH_STAGE", stage)
                .stdout(Stdio::null())
                .spawn()
                .unwrap(),
        );
        let deadline = Instant::now() + Duration::from_secs(10);
        while !fixture.root.0.join("ready").exists() {
            assert!(
                child.0.try_wait().unwrap().is_none(),
                "child exited before readiness"
            );
            assert!(Instant::now() < deadline, "child failed to bind");
            std::thread::sleep(Duration::from_millis(10));
        }
        let original = fs::metadata(fixture.socket()).unwrap();
        let lock_path = fixture
            .socket()
            .parent()
            .unwrap()
            .join(".racer-client.lock");
        let lock = fs::metadata(&lock_path).unwrap().ino();
        assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io));
        assert_eq!(
            fs::metadata(fixture.socket()).unwrap().ino(),
            original.ino()
        );
        child.0.kill().unwrap();
        child.0.wait().unwrap();
        assert!(
            fixture.socket().exists(),
            "SIGKILL must leave canonical socket"
        );
        fixture.reconcile(&[definition()]).unwrap();
        assert_eq!(fs::metadata(&lock_path).unwrap().ino(), lock);
        let mut socket = fixture.connect();
        socket
            .write_all(&request("HEAD", "Connection: close\r\n"))
            .unwrap();
        assert!(
            fixture
                .receive(&mut socket, true)
                .starts_with(b"HTTP/1.1 200")
        );
        assert_eq!(
            fs::read_dir(fixture.socket().parent().unwrap())
                .unwrap()
                .count(),
            3
        );
        fixture.reconcile(&[]).unwrap();
        assert!(!fixture.socket().exists());
        assert_eq!(fs::metadata(&lock_path).unwrap().ino(), lock);
        fixture.reconcile(&[definition()]).unwrap();
    }
}

#[test]
fn endpoint_ownership_rejects_unsafe_locks_and_foreign_sockets() {
    for kind in [
        "symlink",
        "hardlink",
        "directory",
        "fifo",
        "permissions",
        "writable-directory",
        "foreign-live",
        "foreign-stale",
    ] {
        let fixture = Fixture::new();
        let directory = fixture.socket().parent().unwrap().to_owned();
        fs::create_dir_all(&directory).unwrap();
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o755)).unwrap();
        let lock = directory.join(".racer-client.lock");
        let target = fixture.root.0.join("target");
        fs::write(&target, b"preserve").unwrap();
        fs::set_permissions(&target, fs::Permissions::from_mode(0o600)).unwrap();
        let mut foreign = None;
        match kind {
            "symlink" => std::os::unix::fs::symlink(&target, &lock).unwrap(),
            "hardlink" => fs::hard_link(&target, &lock).unwrap(),
            "directory" => fs::create_dir(&lock).unwrap(),
            "fifo" => {
                let name = CString::new(lock.as_os_str().as_encoded_bytes()).unwrap();
                assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
            }
            "permissions" => {
                fs::write(&lock, b"preserve").unwrap();
                fs::set_permissions(&lock, fs::Permissions::from_mode(0o666)).unwrap();
            }
            "writable-directory" => {
                fs::set_permissions(&directory, fs::Permissions::from_mode(0o777)).unwrap()
            }
            _ => {
                let dir = File::open(&directory).unwrap();
                foreign = Some(UnixListener::bind(file_path(&dir).join("socket")).unwrap());
                if kind == "foreign-stale" {
                    foreign.take();
                }
            }
        }
        let before = fs::symlink_metadata(fixture.socket()).ok().map(|m| m.ino());
        if kind == "writable-directory" {
            // Startup prepares permissions first, but acquire itself stays strict.
            let dir = File::open(&directory).unwrap();
            assert!(EndpointOwner::acquire(&dir).is_err());
            assert_eq!(dir.metadata().unwrap().mode() & 0o7777, 0o777);
            assert!(!lock.exists());
        } else {
            for mode in [0o755, 0o777] {
                fs::set_permissions(&directory, fs::Permissions::from_mode(mode)).unwrap();
                assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io), "{kind}");
                assert_eq!(fs::metadata(&directory).unwrap().mode() & 0o7777, 0o755);
            }
        }
        assert_eq!(
            fs::symlink_metadata(fixture.socket()).ok().map(|m| m.ino()),
            before
        );
        assert_eq!(fs::read(&target).unwrap(), b"preserve");
        drop(foreign);
    }
}

#[test]
fn lock_replacement_and_live_witness_without_lock_are_refused() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let directory = File::open(fixture.socket().parent().unwrap()).unwrap();
    let lock = file_path(&directory).join(".racer-client.lock");
    fs::rename(&lock, file_path(&directory).join("old-lock")).unwrap();
    assert!(EndpointOwner::acquire(&directory).is_err());
    let mut changed = definition();
    changed.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    assert_eq!(fixture.reconcile(&[changed]), Err(Error::Io));
    let mut socket = fixture.connect();
    socket
        .write_all(&request("HEAD", "Connection: close\r\n"))
        .unwrap();
    assert!(
        fixture
            .receive(&mut socket, true)
            .starts_with(b"HTTP/1.1 200")
    );
}

#[test]
fn recovery_preserves_stale_foreign_inode_and_recovers_unpublished_witness() {
    let fixture = Fixture::new();
    let directory = fixture.root.0.join("example/client");
    fs::create_dir_all(&directory).unwrap();
    fs::set_permissions(&directory, fs::Permissions::from_mode(0o755)).unwrap();
    let directory = File::open(directory).unwrap();
    let witness =
        file_path(&directory).join(".racer-owned-.racer-00000000000000000000000000000000");
    let stale = UnixListener::bind(&witness).unwrap();
    // Parallel crash-owner tests spawn children. A concurrent fork can hold
    // this CLOEXEC descriptor until exec, so dropping our alias alone does
    // not guarantee a refused connection yet. Shut down the shared listener
    // before dropping it to establish the stale-witness precondition.
    assert_eq!(
        unsafe { libc::shutdown(stale.as_raw_fd(), libc::SHUT_RDWR) },
        0
    );
    drop(stale);
    let socket = file_path(&directory).join("socket");
    drop(UnixListener::bind(&socket).unwrap());
    let inode = fs::metadata(&socket).unwrap().ino();
    assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io));
    assert_eq!(fs::metadata(&socket).unwrap().ino(), inode);
    assert!(witness.exists());
    fs::remove_file(socket).unwrap();
    fixture.reconcile(&[definition()]).unwrap();
    assert!(!witness.exists());
    let mut socket = fixture.connect();
    socket
        .write_all(&request("HEAD", "Connection: close\r\n"))
        .unwrap();
    assert!(
        fixture
            .receive(&mut socket, true)
            .starts_with(b"HTTP/1.1 200")
    );
}
