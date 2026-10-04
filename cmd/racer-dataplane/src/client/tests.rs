//! Client socket scenarios: ownership, publication, HTTP delivery, and retirement.
use super::*;
use super::listener::*;
use super::response::Responses;
use racer_control_wire::CacheDefinition;
use crate::admission::AdmissionPolicy;
use crate::runtime::Cancellation;
use crate::runtime::RequestScope;
use crate::test_support::ReadWorker;
use crate::test_support::origin::RequestKind;
use std::time::Instant;
use std::cell::Cell;
use std::cell::RefCell;
use std::ffi::CString;
use std::fs;
use std::fs::File;
use std::fs::OpenOptions;
use std::os::fd::AsRawFd;
use std::os::unix::fs::MetadataExt;
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::UnixListener;
use std::path::Path;
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
mod acquisition {
    use super::*;

    #[test]
    fn raw_uds_late_rust_acquisition_failure_never_appends_second_status() {
        use crate::model::PAGE_BYTES;
        let (fixture, pipes) = body_fixture_with_large_page(16, false, true);
        let metadata = body_metadata(3 * PAGE_BYTES + 13);
        let worker = fixture.worker.as_ref().unwrap();
        worker.origin.set_version(metadata.clone());
        worker.origin.set_body(vec![b'x'; metadata.length as usize]);
        let mut socket = fixture.connect();
        socket.write_all(&request("POST", "If-Match: \"v1\"\r\nRange: bytes=0-\r\nRacer-Page-Credits: 1\r\nRacer-Byte-Credits: 16777216\r\nRacer-Ordered: 1\r\n")).unwrap();
        let mut raw = fixture.receive(&mut socket, false);
        let end = raw.windows(4).position(|part| part == b"\r\n\r\n").unwrap() + 4;
        let deadline = Instant::now() + Duration::from_secs(10);
        while raw.len() < end + PAGE_BYTES as usize + 21 {
            fixture.pump(64);
            let mut bytes = [0; 65536];
            match socket.read(&mut bytes) {
                Ok(0) => panic!("first page truncated"),
                Ok(n) => raw.extend_from_slice(&bytes[..n]),
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(error) => panic!("{error}"),
            }
            assert!(Instant::now() < deadline);
        }
        let mut release = [0; 12];
        release[8..].copy_from_slice(&(PAGE_BYTES as u32).to_be_bytes());
        socket.write_all(&release).unwrap();
        raw.extend(fixture.receive(&mut socket, true));
        let head = std::str::from_utf8(&raw[..end])
            .unwrap()
            .to_ascii_lowercase();
        assert!(head.starts_with("http/1.1 200 "));
        assert!(head.contains("content-length: 50331766\r\n"));
        assert!(head.contains("racer-object-length: 50331661\r\n"));
        assert!(head.contains("racer-range-start: 0\r\n"));
        assert!(head.contains("racer-range-end: 50331661\r\n"));
        assert!(!head.contains("content-range:"));
        assert_eq!(raw.len() - end, PAGE_BYTES as usize + 21);
        let mut frame = [0; 21];
        frame[0] = 1;
        frame[17..].copy_from_slice(&(PAGE_BYTES as u32).to_be_bytes());
        assert_eq!(&raw[end..end + 21], &frame);
        assert!(raw[end + 21..].iter().all(|byte| *byte == b'x'));
        assert_eq!(
            raw.windows(8).filter(|part| *part == b"HTTP/1.1").count(),
            1
        );
        assert_only_idle_pipes(&fixture, &pipes);
    }

    #[test]
    fn coordinator_enforces_pinned_head_and_unsatisfiable_range_length() {
        for (length, fields, status) in [
            (4, "", "200"),
            (4, "If-Match: \"v1\"\r\n", "200"),
            (4, "If-Match: \"old\"\r\n", "412"),
            (4, "Range: bytes=4-\r\n", "416"),
            (0, "Range: bytes=0-\r\n", "416"),
        ] {
            let (fixture, _pipes) = body_fixture(16, false);
            fixture
                .worker
                .as_ref()
                .unwrap()
                .origin
                .set_version(body_metadata(length));
            let method = if status == "416" { "POST" } else { "HEAD" };
            let mut socket = fixture.connect();
            socket
                .write_all(&request(method, &format!("{fields}Connection: close\r\n")))
                .unwrap();
            let response = String::from_utf8(fixture.receive(&mut socket, true)).unwrap();
            assert!(
                response.starts_with(&format!("HTTP/1.1 {status} ")),
                "{response}"
            );
            if status == "416" {
                assert!(
                    response.contains(&format!("Content-Range: bytes */{length}\r\n")),
                    "{response}"
                );
            }
        }
    }

    #[test]
    fn raw_uds_first_rust_acquisition_failure_returns_complete_503() {
        let (fixture, pipes) = body_fixture(16, true);
        let mut socket = start_body(&fixture);
        let text = String::from_utf8(fixture.receive(&mut socket, true))
            .unwrap()
            .to_ascii_lowercase();
        assert!(text.starts_with("http/1.1 503 "));
        assert!(text.contains("content-length: 0\r\n"));
        assert!(!text.contains("content-range:"));
        assert_eq!(text.matches("http/1.1").count(), 1);
        assert_eq!(text.find("\r\n\r\n").unwrap() + 4, text.len());
        assert_only_idle_pipes(&fixture, &pipes);
    }
}

fn body_metadata(length: u64) -> ObjectMetadata {
    ObjectMetadata {
        content_type: None,
        version: ObjectVersion {
            object: crate::model::ObjectId {
                cache: definition().id,
                key: crate::model::CacheKey([0; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        length,
        expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
    }
}
mod directory {
    use super::*;
    fn assert_identity(before: &fs::Metadata, after: &fs::Metadata, mode: u32) {
        assert!(same_inode(before, after));
        assert_eq!((after.uid(), after.gid()), (before.uid(), before.gid()));
        assert_eq!(after.mode() & 0o7777, mode);
    }
    #[test]
    fn startup_hardens_existing_client_directory_without_changing_ancestors() {
        for mode in [0o777, 0o775, 0o757, 0o770, 0o722, 0o2775, 0o1777] {
            let fixture = Fixture::new();
            let path = fixture.socket().parent().unwrap().to_owned();
            fs::create_dir_all(&path).unwrap();
            let cache = path.parent().unwrap();
            let origin = cache.join("origin");
            fs::create_dir(&origin).unwrap();
            for ancestor in [&fixture.root.0, cache, &origin] {
                fs::set_permissions(ancestor, fs::Permissions::from_mode(0o777)).unwrap();
            }
            fs::set_permissions(&path, fs::Permissions::from_mode(mode)).unwrap();
            let before = fs::metadata(&path).unwrap();
            fixture.reconcile(&[definition()]).unwrap();
            assert_identity(&before, &fs::metadata(&path).unwrap(), mode & !0o022);
            for ancestor in [&fixture.root.0, cache, &origin] {
                assert_eq!(fs::metadata(ancestor).unwrap().mode() & 0o7777, 0o777);
            }
            assert_eq!(
                fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
                0o666
            );
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
    }
    #[test]
    fn secure_client_directory_permissions_are_unchanged() {
        for mode in [0o755, 0o750, 0o700, 0o500, 0o2750, 0o1700] {
            let root = Root::new();
            let directory = open_directory(&root.0).unwrap();
            directory
                .set_permissions(fs::Permissions::from_mode(mode))
                .unwrap();
            let before = directory.metadata().unwrap();
            prepare_client_directory(&directory).unwrap();
            prepare_client_directory(&directory).unwrap();
            let after = directory.metadata().unwrap();
            assert_identity(&before, &after, mode);
            assert_eq!(
                (after.ctime(), after.ctime_nsec()),
                (before.ctime(), before.ctime_nsec())
            );
            directory
                .set_permissions(fs::Permissions::from_mode(0o700))
                .unwrap();
        }
    }
    #[test]
    fn client_directory_preparation_pins_inode_across_path_replacement() {
        for symlink in [false, true] {
            let root = Root::new();
            let parent = open_directory(&root.0).unwrap();
            let directory = child_directory(&parent, b"client").unwrap();
            directory
                .set_permissions(fs::Permissions::from_mode(0o777))
                .unwrap();
            let path = root.0.join("client");
            let moved = root.0.join("moved");
            let target = root.0.join("target");
            fs::create_dir(&target).unwrap();
            fs::set_permissions(&target, fs::Permissions::from_mode(0o777)).unwrap();
            fs::rename(&path, &moved).unwrap();
            if symlink {
                std::os::unix::fs::symlink(&target, &path).unwrap();
                assert!(child_directory(&parent, b"client").is_err());
            } else {
                fs::create_dir(&path).unwrap();
                fs::set_permissions(&path, fs::Permissions::from_mode(0o777)).unwrap();
            }
            prepare_client_directory(&directory).unwrap();
            assert_eq!(fs::metadata(&moved).unwrap().mode() & 0o7777, 0o755);
            assert_eq!(fs::metadata(&path).unwrap().mode() & 0o7777, 0o777);
            assert_eq!(fs::metadata(&target).unwrap().mode() & 0o7777, 0o777);
        }
    }
    #[test]
    fn client_directory_preparation_rejects_non_directories_and_chmod_failure() {
        let root = Root::new();
        let path = root.0.join("file");
        fs::write(&path, b"preserve").unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o666)).unwrap();
        assert_eq!(
            prepare_client_directory(&File::open(&path).unwrap()),
            Err(Error::Io)
        );
        assert_eq!(fs::metadata(&path).unwrap().mode() & 0o7777, 0o666);
        assert_eq!(fs::read(&path).unwrap(), b"preserve");
        fs::set_permissions(&root.0, fs::Permissions::from_mode(0o777)).unwrap();
        // O_PATH supports fstat but not fchmod, deterministically exercising failure.
        let directory = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_PATH | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
            .open(&root.0)
            .unwrap();
        assert_eq!(prepare_client_directory(&directory), Err(Error::Io));
        assert_eq!(directory.metadata().unwrap().mode() & 0o7777, 0o777);
        let link = root.0.join("link");
        std::os::unix::fs::symlink(&root.0, &link).unwrap();
        let link = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC)
            .open(link)
            .unwrap();
        assert_eq!(prepare_client_directory(&link), Err(Error::Io));
        assert_eq!(directory.metadata().unwrap().mode() & 0o7777, 0o777);
    }
    #[test]
    fn startup_rejects_foreign_owned_directory_without_chmod() {
        if unsafe { libc::geteuid() } != 0 {
            return;
        }
        let fixture = Fixture::new();
        let path = fixture.socket().parent().unwrap().to_owned();
        fs::create_dir_all(&path).unwrap();
        let directory = File::open(&path).unwrap();
        assert_eq!(unsafe { libc::fchown(directory.as_raw_fd(), 1, !0) }, 0);
        directory
            .set_permissions(fs::Permissions::from_mode(0o777))
            .unwrap();
        assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io));
        assert_eq!(directory.metadata().unwrap().uid(), 1);
        assert_eq!(directory.metadata().unwrap().mode() & 0o7777, 0o777);
        assert_eq!(fs::read_dir(&path).unwrap().count(), 0);
    }
}
mod recovery {
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
            Some(
                futures::executor::block_on(fixture.listeners.prepare(&[changed], &scope()))
                    .unwrap(),
            )
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
        use std::process::Command;
        use std::process::Stdio;
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
                        "client::tests::recovery::crash_owner_child",
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
}

#[test]
fn local_and_distributed_installs_drain_responses_and_retire_idle_generations() {
    use crate::model::WorkerId;
    use crate::admission::Ingress;
    use crate::test_support::WakeCounter;
    use std::task::Waker;

    for distributed in [false, true] {
        for busy in [false, true] {
            let mut acceptor = Fixture::new();
            let mut worker = Fixture::new();
            let ingress = Arc::new(Ingress::new(&[WorkerId(1)]));
            if distributed {
                ingress
                    .install(WorkerId(1), &worker.listeners.admission)
                    .unwrap();
                acceptor.listeners = acceptor.listeners.with_ingress(ingress.clone());
            }
            let receiver = if distributed {
                &mut worker
            } else {
                &mut acceptor
            };
            receiver
                .worker
                .as_ref()
                .unwrap()
                .origin
                .block(RequestKind::Head);
            acceptor.reconcile(&[definition()]).unwrap();
            let mut socket = acceptor.connect();
            let wakes = Arc::new(WakeCounter::default());
            let waker = Waker::from(wakes.clone());
            let mut cx = Context::from_waker(&waker);
            if distributed {
                // Register the receiving worker before the acceptor delivers a socket.
                assert!(ingress.pop_batch::<1>(WorkerId(1), &waker, 1).unwrap()[0].is_none());
            }
            assert_eq!(acceptor.listeners.poll_budgeted(&mut cx, 1).unwrap(), 1);
            assert_eq!(wakes.count(), 1, "new work must wake its owning worker");
            let receiver = if distributed { &worker } else { &acceptor };
            if distributed {
                assert_eq!(acceptor.listeners.active_connections(), 0);
                assert_eq!(receiver.listeners.active_connections(), 0);
                receiver.install_handoff(&ingress);
            }
            assert_eq!(
                receiver.listeners.active_connections_for(&definition().id),
                1
            );
            if busy {
                socket.write_all(&request("HEAD", "")).unwrap();
            }
            for _ in 0..16 {
                receiver.pump(16);
            }
            assert_eq!(
                receiver.listeners.read_scopes.borrow().len(),
                usize::from(busy)
            );
            if busy {
                receiver.wait_for_origin(RequestKind::Head, 1);
            }
            acceptor.reconcile(&[]).unwrap();
            // Reusing a UID creates a fresh generation, not a revival of the old one.
            acceptor.reconcile(&[definition()]).unwrap();
            for _ in 0..16 {
                receiver.pump(16);
            }
            if busy {
                assert!(receiver.listeners.read_scopes.borrow()[0].check().is_ok());
                assert_eq!(receiver.listeners.active_connections(), 1);
                receiver
                    .worker
                    .as_ref()
                    .unwrap()
                    .origin
                    .release(RequestKind::Head);
            }
            let response = receiver.receive(&mut socket, true);
            if busy {
                assert!(response.starts_with(b"HTTP/1.1 200"));
            } else {
                assert!(response.is_empty());
            }
            assert_eq!(receiver.completed_heads(), usize::from(busy));
            assert_no_body_leases(receiver);
        }
    }
}

#[test]
fn queued_handoffs_reject_retired_generations_and_stopped_receivers() {
    use crate::model::ResourceClass;
    use crate::model::WorkerId;
    use crate::admission::Ingress;

    for stop_receiver in [false, true] {
        let mut acceptor = Fixture::new();
        let worker = Fixture::new();
        let ingress = Arc::new(Ingress::new(&[WorkerId(1)]));
        ingress
            .install(WorkerId(1), &worker.listeners.admission)
            .unwrap();
        acceptor.listeners = acceptor.listeners.with_ingress(ingress.clone());
        acceptor.reconcile(&[definition()]).unwrap();
        let mut socket = acceptor.connect();
        acceptor.pump(1);
        assert_eq!(
            worker
                .listeners
                .admission
                .used(ResourceClass::IngressConnection),
            1
        );
        assert_eq!(worker.listeners.active_connections(), 0);
        if stop_receiver {
            worker.listeners.stop_admission();
        } else {
            acceptor.reconcile(&[]).unwrap();
            acceptor.reconcile(&[definition()]).unwrap();
        }
        worker.install_handoff(&ingress);
        assert_eq!(worker.listeners.active_connections(), 0);
        assert_eq!(
            worker
                .listeners
                .admission
                .used(ResourceClass::IngressConnection),
            0
        );
        // A concurrent fork can briefly retain the closed socket until exec.
        // Wait for EOF through the same bounded path as other real UDS tests.
        assert!(worker.receive(&mut socket, true).is_empty());
        assert_eq!(worker.completed_heads(), 0);
        assert_no_body_leases(&worker);
    }
}

#[test]
fn simulated_listener_preparation_rollback_and_real_http_exchange() {
    use uring_runtime::SocketAddress;
    use uring_runtime::simulation::Simulation;
    let sim = Simulation::new();
    let _environment = sim.enter();
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits())));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(crate::http::new_io(
        reactor.clone(),
        Codec::new(32768),
        admission.clone(),
        i64::MAX as u64,
    ));
    let delivery = Rc::new(Delivery::new(
        Rc::new(new_pipe_pool(admission.clone())),
        reactor.clone(),
        Duration::from_secs(2),
    ));
    let worker = ReadWorker::new(
        definition(),
        body_metadata(17),
        admission.clone(),
        reactor.clone(),
        delivery.clone(),
        1,
    );
    worker
        .coordinator
        .metadata
        .publish_version(body_metadata(17).immutable())
        .unwrap();
    let listeners = ClientListeners::new(
        worker.coordinator.clone(),
        RequestParser::new(32768),
        Rc::new(Responses::new(io.clone(), delivery)),
        io,
        admission,
    );
    let definition = definition();
    futures::executor::block_on(listeners.reconcile(std::slice::from_ref(&definition), &scope()))
        .unwrap();
    let path = PathBuf::from("/run/racer/example/client/socket");
    let inode = sim.metadata(&path).unwrap().0;
    assert_eq!(sim.metadata(&path).unwrap().1 & 0o777, 0o666);
    let mut changed = definition.clone();
    changed.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let prepared = futures::executor::block_on(listeners.prepare(&[changed], &scope())).unwrap();
    assert_ne!(sim.metadata(&path).unwrap().0, inode);
    drop(prepared);
    assert_eq!(sim.metadata(&path).unwrap().0, inode);
    assert_eq!(sim.metadata(&path).unwrap().1 & 0o777, 0o666);
    let client = sim.connect(SocketAddress::Unix(path)).unwrap();
    let client = client.into_sim().unwrap();
    client
        .send(&request("HEAD", "If-Match: \"v1\"\r\n"))
        .unwrap();
    let mut response = Vec::new();
    let mut bytes = [0; 4096];
    for _ in 0..1000 {
        let _queue = worker.drivers.enter();
        listeners
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                16,
            )
            .unwrap();
        reactor.poll_budgeted(16).unwrap();
        worker.poll(&mut Context::from_waker(futures::task::noop_waker_ref()));
        match client.recv(&mut bytes) {
            Ok(n) => response.extend_from_slice(&bytes[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
            Err(e) => panic!("{e}"),
        }
        if response.windows(4).any(|w| w == b"\r\n\r\n") {
            break;
        }
    }
    assert!(response.starts_with(b"HTTP/1.1 200"), "{response:?}");
    assert_eq!(listeners.read_scopes.borrow().len(), 1);
    drop(client);
    listeners.stop_admission();
    drop(listeners);
    drop(worker);
    drop(reactor);
    assert_eq!(sim.live_handles(), 0);
}
use crate::http::Codec;
use crate::http::Delivery;
use crate::http::new_pipe_pool;
use crate::model::ExpiresAt;
use crate::config::Limits;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::StrongEtag;
use crate::read::ReadResponse;
use crate::runtime::Reactor;
use std::io::Read;
use std::io::Write;
use std::num::NonZeroUsize;
use std::os::unix::net::UnixStream;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;
use std::time::UNIX_EPOCH;

static NEXT_ROOT: AtomicUsize = AtomicUsize::new(0);
struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        // Test files stay inside the shared worktree, even on unprivileged runs.
        let path = Path::new(env!("CARGO_MANIFEST_DIR")).join(format!(
            ".client-test-{}-{}",
            std::process::id(),
            NEXT_ROOT.fetch_add(1, Ordering::Relaxed)
        ));
        fs::create_dir(&path).unwrap();
        Self(path)
    }
}
impl Drop for Root {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}
fn limits() -> Limits {
    let n = NonZeroUsize::new(64).unwrap();
    let bytes = NonZeroUsize::new(64 * 1024 * 1024).unwrap();
    Limits {
        plaintext_bytes: bytes,
        ciphertext_bytes: bytes,
        dirty_bytes: bytes,
        registered_bytes: bytes,
        request_context_bytes: bytes,
        flights: n,
        waiters_per_flight: n,
        queue_entries: n,
        connections_per_neighbor: n,
        client_connections: n,
        pipes: n,
        range_window_pages: n,
        header_bytes: NonZeroUsize::new(32768).unwrap(),
        cached_rankings: n,
        cached_paths: n,
        retained_snapshots: n,
        metadata_entries: n,
        relay_transfers: n,
    }
}
fn definition() -> CacheDefinition {
    CacheDefinition {
        id: CacheId(crate::security::test_support::CACHE.into()),
        name: "example".into(),
        client_socket: "/run/racer/example/client/socket".into(),
        origin_socket: "/run/racer/example/origin/socket".into(),
    }
}
fn scope() -> RequestScope {
    new_scope(Duration::from_secs(5), Cancellation::new().unwrap()).unwrap()
}
struct Fixture {
    root: Root,
    listeners: ClientListeners,
    reactor: Rc<Reactor>,
    worker: Option<crate::test_support::ReadWorker>,
}
impl Fixture {
    fn install_handoff(&self, ingress: &crate::admission::Ingress) {
        use crate::model::WorkerId;
        use crate::admission::Kind;
        let [accepted] = ingress
            .pop_batch::<1>(WorkerId(1), futures::task::noop_waker_ref(), 1)
            .unwrap();
        let accepted = accepted.expect("accepted socket queued for receiving worker");
        let Kind::Client(cache, retired) = accepted.kind else {
            panic!("client endpoint delivered a peer socket");
        };
        let connection =
            crate::http::from_reserved(accepted.fd.into(), accepted.reservation).unwrap();
        self.listeners
            .install_connection(connection, cache, retired)
            .unwrap();
    }

    fn new() -> Self {
        Self::with_limits(limits())
    }
    fn with_limits(limits: Limits) -> Self {
        let root = Root::new();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = Rc::new(crate::http::new_io(
            reactor.clone(),
            Codec::new(32768),
            admission.clone(),
            i64::MAX as u64,
        ));
        let delivery = Rc::new(Delivery::new(
            Rc::new(new_pipe_pool(admission.clone())),
            reactor.clone(),
            Duration::from_secs(2),
        ));
        let mut metadata = body_metadata(17);
        metadata.expires_at =
            ExpiresAt::from_system_time(UNIX_EPOCH + Duration::from_millis(1234)).unwrap();
        let worker = ReadWorker::new(
            definition(),
            metadata,
            admission.clone(),
            reactor.clone(),
            delivery.clone(),
            1,
        );
        let responses = Rc::new(Responses::new(io.clone(), delivery));
        let mut listeners = ClientListeners::new(
            worker.coordinator.clone(),
            RequestParser::new(32768),
            responses,
            io,
            admission,
        );
        listeners.root = root.0.clone();
        Self {
            root,
            listeners,
            reactor,
            worker: Some(worker),
        }
    }
    fn reconcile(&self, caches: &[CacheDefinition]) -> Result<()> {
        futures::executor::block_on(self.listeners.reconcile(caches, &scope()))
    }
    fn completed_heads(&self) -> usize {
        self.worker
            .as_ref()
            .unwrap()
            .origin
            .completed(RequestKind::Head)
    }
    fn wait_for_origin(&self, kind: RequestKind, count: usize) {
        let deadline = Instant::now() + Duration::from_secs(5);
        while self.worker.as_ref().unwrap().origin.count(kind) < count {
            self.pump(16);
            assert!(
                Instant::now() < deadline,
                "gated origin request was not submitted"
            );
            std::thread::yield_now();
        }
    }
    // Exercise the completed-result defense directly, never fake Coordinator reads.
    fn result_output(
        &self,
        kind: ReadKind,
        result: Result<ReadResponse>,
        canceled: bool,
    ) -> Vec<u8> {
        let (server, mut client) = UnixStream::pair().unwrap();
        client.set_nonblocking(true).unwrap();
        let admission = self.listeners.admission.clone();
        let connection = crate::http::from_accepted(server.into(), &admission).unwrap();
        let responses = self.listeners.responses.clone();
        let metrics = self.listeners.metrics.clone();
        let scope = scope();
        if canceled {
            scope.cancel().unwrap();
        }
        self.listeners.active.borrow_mut().push_back(Active {
            runnable: uring_runtime::drivers::Runnable::new(),
            deadline: Rc::new(Cell::new(scope.deadline.0)),
            expired: None,
            cache: definition().id,
            retired: Arc::new(std::sync::atomic::AtomicBool::default()),
            idle: Rc::new(Cell::new(false)),
            cancellation: scope.cancellation.clone(),
            operation: Box::pin(async move {
                let mut observation = metrics.request()?;
                handle_read_result(
                    connection,
                    &kind,
                    &body_metadata(0).version.object,
                    result,
                    &responses,
                    &admission,
                    &scope,
                    &mut observation,
                    Duration::from_secs(5),
                )
                .await?;
                Ok(())
            }),
        });
        self.receive(&mut client, true)
    }
    fn socket(&self) -> PathBuf {
        self.root.0.join("example/client/socket")
    }
    fn assert_metrics(&self, requests: u64, errors: u64, active: u64) {
        use crate::telemetry::Event;
        use crate::telemetry::Gauge;
        assert_eq!(self.listeners.metrics.count(Event::Request), requests);
        assert_eq!(self.listeners.metrics.count(Event::RequestError), errors);
        assert_eq!(self.listeners.metrics.gauge(Gauge::ActiveRequests), active);
    }
    fn connect(&self) -> UnixStream {
        // /proc keeps sun_path short even in deeply nested CI worktrees.
        let directory = File::open(self.socket().parent().unwrap()).unwrap();
        let stream = UnixStream::connect(file_path(&directory).join("socket")).unwrap();
        stream.set_nonblocking(true).unwrap();
        stream
    }
    fn pump(&self, budget: usize) {
        let _queue = self.worker.as_ref().map(|worker| worker.drivers.enter());
        self.listeners
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                budget,
            )
            .unwrap();
        if let Some(worker) = &self.worker {
            worker.poll(&mut Context::from_waker(futures::task::noop_waker_ref()));
        }
        self.reactor.poll_budgeted(64).unwrap();
    }
    fn receive(&self, socket: &mut UnixStream, eof: bool) -> Vec<u8> {
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut output = Vec::new();
        loop {
            self.pump(16);
            let mut bytes = [0; 8192];
            match socket.read(&mut bytes) {
                Ok(0) => return output,
                Ok(n) => output.extend_from_slice(&bytes[..n]),
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                Err(error) => panic!("client read failed: {error}"),
            }
            if !eof && output.windows(4).any(|part| part == b"\r\n\r\n") {
                return output;
            }
            assert!(
                Instant::now() < deadline,
                "client exchange did not complete"
            );
        }
    }
}
fn request(method: &str, fields: &str) -> Vec<u8> {
    let body = if method == "POST" {
        "Content-Length: 0\r\n"
    } else {
        ""
    };
    format!(
        "{method} /v2/objects/{} HTTP/1.1\r\nHost: racer\r\n{body}{fields}\r\n",
        "0".repeat(64)
    )
    .into_bytes()
}

#[test]
fn client_listener_readiness_recovers_from_queue_pressure() {
    let mut limits = limits();
    limits.queue_entries = NonZeroUsize::new(8).unwrap();
    let fixture = Fixture::with_limits(limits);
    fixture.reconcile(&[definition()]).unwrap();
    let scope = scope();
    let (reader, _writer) = UnixStream::pair().unwrap();
    let reader = Rc::new(uring_runtime::Descriptor::from(reader));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut pressure = Vec::new();
    for _ in 0..8 {
        let mut wait = fixture
            .reactor
            .readiness(reader.clone(), libc::POLLIN as u32, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        pressure.push(wait);
    }
    for _ in 0..32 {
        fixture.listeners.poll_budgeted(&mut cx, 16).unwrap();
        assert_eq!(fixture.reactor.in_flight(), 8);
    }
    drop(pressure);
    let mut client = fixture.connect();
    client
        .write_all(&request("HEAD", "Connection: close\r\n"))
        .unwrap();
    assert!(
        fixture
            .receive(&mut client, true)
            .starts_with(b"HTTP/1.1 200")
    );
}

fn sleep_until(deadline: Instant) {
    std::thread::sleep(deadline.saturating_duration_since(Instant::now()));
}

#[test]
fn configured_timeout_bounds_idle_partial_headers_and_keepalive() {
    for mode in ["idle", "partial", "keepalive"] {
        let mut fixture = Fixture::new();
        assert_eq!(fixture.listeners.request_timeout(), Duration::from_secs(30));
        let timeout = Duration::from_millis(200);
        fixture.listeners = fixture.listeners.with_request_timeout(timeout);
        fixture.reconcile(&[definition()]).unwrap();
        let mut socket = fixture.connect();
        if mode == "keepalive" {
            socket.write_all(&request("HEAD", "")).unwrap();
            assert!(
                fixture
                    .receive(&mut socket, false)
                    .starts_with(b"HTTP/1.1 200 ")
            );
        }
        for _ in 0..16 {
            fixture.pump(16);
        }
        assert_eq!(fixture.listeners.active_connections(), 1);
        assert_no_head(&mut socket);
        // Make progress inside the header budget, then cross its original
        // deadline before a renewed budget could expire.
        let started = Instant::now();
        if mode == "partial" {
            sleep_until(started + timeout / 2);
            socket.write_all(b"HEAD /v2/objects/").unwrap();
            for _ in 0..16 {
                fixture.pump(16);
            }
            assert_no_head(&mut socket);
        }
        sleep_until(started + timeout + Duration::from_millis(20));
        for _ in 0..64 {
            fixture.pump(16);
        }
        assert_eq!(fixture.listeners.active_connections(), 0, "{mode}");
        assert_eq!(socket.read(&mut [0; 1]).unwrap(), 0, "{mode}");
        assert_eq!(fixture.completed_heads(), usize::from(mode == "keepalive"));
        assert_no_body_leases(&fixture);
    }
}

#[test]
fn configured_timeout_starts_fresh_operations_after_headers_and_on_reuse() {
    let mut fixture = Fixture::new();
    let timeout = Duration::from_millis(500);
    fixture.listeners = fixture.listeners.with_request_timeout(timeout);
    fixture
        .worker
        .as_ref()
        .unwrap()
        .origin
        .block(RequestKind::Head);
    fixture.reconcile(&[definition()]).unwrap();
    let mut socket = fixture.connect();
    let head = request("HEAD", "");
    socket.write_all(&head[..head.len() - 2]).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    assert!(fixture.listeners.read_scopes.borrow().is_empty());
    std::thread::sleep(Duration::from_millis(100));
    let before = Instant::now();
    socket.write_all(b"\r\n").unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    let first = fixture.listeners.read_scopes.borrow()[0].clone();
    fixture.wait_for_origin(RequestKind::Head, 1);
    assert!(first.deadline.0 >= before + timeout);
    assert!(first.deadline.0 <= Instant::now() + timeout);
    fixture
        .worker
        .as_ref()
        .unwrap()
        .origin
        .release(RequestKind::Head);
    assert!(
        fixture
            .receive(&mut socket, false)
            .starts_with(b"HTTP/1.1 200 ")
    );
    fixture
        .worker
        .as_ref()
        .unwrap()
        .origin
        .block(RequestKind::Head);
    socket.write_all(&head).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    let second = fixture.listeners.read_scopes.borrow()[1].clone();
    fixture.wait_for_origin(RequestKind::Head, 2);
    assert_ne!(first.request, second.request);
    assert!(second.deadline.0 > first.deadline.0);
    assert_no_head(&mut socket);
    sleep_until(second.deadline.0 + Duration::from_millis(20));
    let output = fixture.receive(&mut socket, true);
    assert!(output.starts_with(b"HTTP/1.1 503 "), "{output:?}");
    assert!(output.ends_with(b"\r\n\r\n"));
    assert_eq!(fixture.completed_heads(), 1);
    assert_no_body_leases(&fixture);
}

#[test]
fn accepted_client_wakes_before_first_poll_and_blocked_clients_are_fair() {
    use crate::test_support::WakeCounter;
    use std::sync::Arc;
    use std::task::Waker;
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let _socket = fixture.connect();
    let count = Arc::new(WakeCounter::default());
    let waker = Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 0).unwrap(), 0);
    assert_eq!(count.count(), 0);
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 1).unwrap(), 1);
    assert_eq!(fixture.listeners.active_connections(), 1);
    assert_eq!(
        count.count(),
        1,
        "accepted task has no I/O registration yet"
    );
    // Replace the not-yet-polled socket operation with deterministic futures
    // at the actual listener driver seam, avoiding kernel timing dependencies.
    fixture.listeners.active.borrow_mut().clear();
    fixture.listeners.accepting.set(false);
    let order = Rc::new(RefCell::new(Vec::new()));
    let mut senders = Vec::new();
    for id in 0..3 {
        let (send, mut receive) = futures::channel::oneshot::channel::<()>();
        senders.push(send);
        let order = order.clone();
        fixture.listeners.active.borrow_mut().push_back(Active {
            runnable: uring_runtime::drivers::Runnable::new(),
            deadline: Rc::new(Cell::new(Instant::now() + Duration::from_secs(5))),
            expired: None,
            cache: definition().id,
            retired: Arc::new(std::sync::atomic::AtomicBool::default()),
            idle: Rc::new(Cell::new(false)),
            cancellation: Cancellation::new().unwrap(),
            operation: Box::pin(std::future::poll_fn(move |cx| {
                order.borrow_mut().push(id);
                std::pin::Pin::new(&mut receive)
                    .poll(cx)
                    .map(|r| r.map_err(|_| Error::Unavailable))
            })),
        });
    }
    for _ in 0..6 {
        assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 1).unwrap(), 1);
    }
    assert_eq!(&*order.borrow(), &[0, 1, 2]);
    assert_eq!(count.count(), 1, "blocked clients do not self-wake");
    for active in fixture.listeners.active.borrow().iter() {
        std::task::Wake::wake_by_ref(&active.runnable);
    }
    for _ in 0..3 {
        fixture.listeners.poll_budgeted(&mut cx, 1).unwrap();
    }
    assert_eq!(&*order.borrow(), &[0, 1, 2, 0, 1, 2]);
    assert_eq!(
        count.count(),
        4,
        "only the three explicit notifications wake the worker"
    );
    std::thread::spawn(move || {
        for send in senders {
            send.send(()).unwrap();
        }
    })
    .join()
    .unwrap();
    assert_eq!(
        count.count(),
        7,
        "listener forwards the real completion waker"
    );
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 2).unwrap(), 2);
    assert_eq!(fixture.listeners.active_connections(), 1);
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 2).unwrap(), 1);
    assert_eq!(fixture.listeners.active_connections(), 0);
    let mut yielded = false;
    fixture.listeners.active.borrow_mut().push_back(Active {
        runnable: uring_runtime::drivers::Runnable::new(),
        deadline: Rc::new(Cell::new(Instant::now() + Duration::from_secs(5))),
        expired: None,
        cache: definition().id,
        retired: Arc::new(std::sync::atomic::AtomicBool::default()),
        idle: Rc::new(Cell::new(false)),
        cancellation: Cancellation::new().unwrap(),
        operation: Box::pin(std::future::poll_fn(move |cx| {
            if yielded {
                Poll::Ready(Ok(()))
            } else {
                yielded = true;
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        })),
    });
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 1).unwrap(), 1);
    assert_eq!(
        count.count(),
        8,
        "cooperative client continuation reaches driver"
    );
    assert_eq!(fixture.listeners.poll_budgeted(&mut cx, 1).unwrap(), 1);
    assert_eq!(fixture.listeners.active_connections(), 0);
}

fn install_body_worker(
    fixture: &mut Fixture,
    delivery: Rc<Delivery>,
    large: bool,
    fail_first: bool,
) {
    use crate::model::CacheKey;
    use crate::model::ObjectId;
    use crate::model::PAGE_BYTES;
    use crate::test_support::ReadWorker;
    let length = if large { PAGE_BYTES + 1 } else { 5 };
    let worker = ReadWorker::new(
        definition(),
        ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: definition().id,
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::parse(b"\"v1\"").unwrap(),
            },
            length,
            expires_at: ExpiresAt::from_system_time(UNIX_EPOCH + Duration::from_millis(1234))
                .unwrap(),
        },
        fixture.listeners.admission.clone(),
        fixture.reactor.clone(),
        delivery,
        1,
    );
    worker.origin.set_body(if large {
        vec![b'x'; length as usize]
    } else {
        b"hello".to_vec()
    });
    if fail_first || large {
        worker
            .origin
            .reject_page(if fail_first { 0 } else { 1 }, 503);
    }
    fixture.listeners.reads = worker.coordinator.clone();
    fixture.worker = Some(worker);
}

#[test]
fn actual_uds_nonempty_range_and_late_failure_truncates() {
    use crate::model::WorkerId;
    for (late_failure, fields, expected_range, expected_length, expected_body) in [
        (
            false,
            "Range: bytes=0-16777215\r\n",
            "bytes 0-4/5",
            5,
            b"hello".as_slice(),
        ),
        (
            false,
            "If-Match: \"v1\"\r\nRange: bytes=1-3\r\n",
            "bytes 1-3/5",
            3,
            b"ell".as_slice(),
        ),
        (
            false,
            "If-Match: \"v1\"\r\nRange: bytes=2-\r\n",
            "bytes 2-4/5",
            3,
            b"llo".as_slice(),
        ),
        (
            false,
            "If-Match: \"v1\"\r\nRange: bytes=-2\r\n",
            "bytes 3-4/5",
            2,
            b"lo".as_slice(),
        ),
        (
            true,
            "If-Match: \"v1\"\r\nRange: bytes=16777214-16777216\r\n",
            "bytes 16777214-16777216/16777217",
            3,
            b"xx".as_slice(),
        ),
    ] {
        let mut fixture = Fixture::new();
        let admission = fixture.listeners.admission.clone();
        let failures = crate::telemetry::Failures::default();
        let observer = failures.observer(WorkerId(0));
        let delivery = Rc::new(Delivery::new(
            Rc::new(new_pipe_pool(admission.clone())),
            fixture.reactor.clone(),
            Duration::from_secs(2),
        ));
        fixture.listeners.responses = Rc::new(
            Responses::new(fixture.listeners.io.clone(), delivery.clone())
                .with_observer(observer.clone()),
        );
        install_body_worker(&mut fixture, delivery, late_failure, false);
        fixture.reconcile(&[definition()]).unwrap();
        let mut socket = fixture.connect();
        socket
            .write_all(&request("POST", &format!("{fields}Connection: close\r\n")))
            .unwrap();
        let output = fixture.receive(&mut socket, true);
        let end = output
            .windows(4)
            .position(|part| part == b"\r\n\r\n")
            .unwrap()
            + 4;
        let head = std::str::from_utf8(&output[..end])
            .unwrap()
            .to_ascii_lowercase();
        assert!(head.starts_with("http/1.1 200"), "{head}");
        assert!(head.contains("content-type: application/octet-stream\r\n"));
        let overhead = if late_failure { 63 } else { 42 };
        assert!(head.contains(&format!(
            "content-length: {}\r\n",
            expected_length + overhead
        )));
        let start: u64 = expected_range
            .strip_prefix("bytes ")
            .unwrap()
            .split('-')
            .next()
            .unwrap()
            .parse()
            .unwrap();
        assert!(head.contains(&format!("racer-range-start: {start}\r\n")));
        assert_eq!(output[end], 1);
        assert_eq!(
            u64::from_be_bytes(output[end + 9..end + 17].try_into().unwrap()),
            start
        );
        assert_eq!(
            &output[end + 21..end + 21 + expected_body.len()],
            expected_body
        );
        if late_failure {
            assert_eq!(output.len(), end + 21 + expected_body.len());
        } else {
            assert_eq!(output.len(), end + 42 + expected_body.len());
            assert_eq!(output[end + 21 + expected_body.len()], 2);
        }
        let mut diagnostics = String::new();
        failures.write(&mut diagnostics).unwrap();
        if late_failure {
            assert!(
                diagnostics.contains("stage=NextSlice error=Unavailable"),
                "{diagnostics}"
            );
            assert!(
                diagnostics.contains("sent: 2, expected: 3"),
                "{diagnostics}"
            );
        } else {
            assert!(diagnostics.starts_with("total=0 "), "{diagnostics}");
        }
    }
}

#[test]
fn subscription_retains_delivered_page_until_release_and_rejects_invalid_releases() {
    use crate::model::ResourceClass;
    for release in [Some((0u64, 2u32)), Some((0, 1)), Some((1, 2)), None] {
        let (fixture, pipes) = body_fixture_with_large_page(4, false, true);
        let mut socket = fixture.connect();
        socket
            .write_all(&request(
                "POST",
                "Range: bytes=16777214-16777216\r\nRacer-Page-Credits: 1\r\n",
            ))
            .unwrap();
        let mut output = fixture.receive(&mut socket, false);
        let end = output.windows(4).position(|b| b == b"\r\n\r\n").unwrap() + 4;
        let deadline = Instant::now() + Duration::from_secs(5);
        while output.len() < end + 23 {
            fixture.pump(16);
            let mut bytes = [0; 4096];
            match socket.read(&mut bytes) {
                Ok(0) => panic!("subscription closed before release"),
                Ok(n) => output.extend_from_slice(&bytes[..n]),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(e) => panic!("{e}"),
            }
            assert!(Instant::now() < deadline);
        }
        assert_eq!(&output[end + 21..], b"xx");
        for _ in 0..32 {
            fixture.pump(16);
        }
        assert_eq!(fixture.listeners.active_connections(), 1);
        assert_eq!(
            fixture.listeners.admission.used(ResourceClass::Plaintext),
            crate::model::PAGE_BYTES as usize
        );
        let Some(release) = release else {
            drop(socket);
            for _ in 0..64 {
                fixture.pump(16);
            }
            assert_only_idle_pipes(&fixture, &pipes);
            continue;
        };
        let mut bytes = [0; 12];
        bytes[..8].copy_from_slice(&release.0.to_be_bytes());
        bytes[8..].copy_from_slice(&release.1.to_be_bytes());
        // Exercise fragmented release reception and retention of its prefix.
        socket.write_all(&bytes[..5]).unwrap();
        for _ in 0..16 {
            fixture.pump(16);
        }
        assert_eq!(fixture.listeners.active_connections(), 1);
        socket.write_all(&bytes[5..]).unwrap();
        assert!(fixture.receive(&mut socket, true).is_empty());
        // A valid release admits the unavailable next page; invalid releases
        // close immediately. Neither path can emit Complete or leak the lease.
        assert_only_idle_pipes(&fixture, &pipes);
    }
}

fn body_fixture(
    queue: usize,
    fail_first: bool,
) -> (Fixture, Rc<flow_control::pipe::PipePool<AdmissionPolicy>>) {
    body_fixture_with_large_page(queue, fail_first, false)
}

fn body_fixture_with_large_page(
    queue: usize,
    fail_first: bool,
    large_page: bool,
) -> (Fixture, Rc<flow_control::pipe::PipePool<AdmissionPolicy>>) {
    let mut limits = limits();
    limits.pipes = NonZeroUsize::new(1).unwrap();
    limits.queue_entries = NonZeroUsize::new(queue).unwrap();
    let mut fixture = Fixture::with_limits(limits);
    let admission = fixture.listeners.admission.clone();
    let pipes = Rc::new(new_pipe_pool(admission.clone()));
    let delivery = Rc::new(Delivery::new(
        pipes.clone(),
        fixture.reactor.clone(),
        Duration::from_secs(2),
    ));
    fixture.listeners.responses = Rc::new(Responses::new(
        fixture.listeners.io.clone(),
        delivery.clone(),
    ));
    install_body_worker(&mut fixture, delivery, large_page, fail_first);
    fixture.reconcile(&[definition()]).unwrap();
    (fixture, pipes)
}

fn start_body(fixture: &Fixture) -> UnixStream {
    let mut socket = fixture.connect();
    socket
        .write_all(&request(
            "POST",
            "If-Match: \"v1\"\r\nRange: bytes=0-4\r\nConnection: close\r\n",
        ))
        .unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    socket
}

fn assert_no_head(socket: &mut UnixStream) {
    assert_eq!(
        socket.read(&mut [0; 1]).unwrap_err().kind(),
        std::io::ErrorKind::WouldBlock
    );
}

fn assert_no_body_leases(fixture: &Fixture) {
    use crate::model::ResourceClass;
    assert_eq!(fixture.listeners.active_connections(), 0);
    for class in [
        ResourceClass::Pipe,
        ResourceClass::Plaintext,
        ResourceClass::Ciphertext,
        ResourceClass::Connection,
    ] {
        assert_eq!(fixture.listeners.admission.used(class), 0, "{class:?}");
    }
}

fn assert_only_idle_pipes(
    fixture: &Fixture,
    pipes: &flow_control::pipe::PipePool<AdmissionPolicy>,
) {
    fixture.listeners.admission.reclaim_buffers();
    use crate::model::ResourceClass;
    assert_eq!(fixture.listeners.active_connections(), 0);
    assert_eq!(
        fixture.listeners.admission.used(ResourceClass::Pipe),
        pipes.idle_count()
    );
    for class in [
        ResourceClass::Plaintext,
        ResourceClass::Ciphertext,
        ResourceClass::Connection,
    ] {
        assert_eq!(fixture.listeners.admission.used(class), 0, "{class:?}");
    }
}

#[test]
fn configured_timeout_is_not_renewed_by_response_or_stream_progress() {
    let (mut fixture, _pipes) = body_fixture_with_large_page(16, false, true);
    let timeout = Duration::from_millis(800);
    fixture.listeners = fixture.listeners.with_request_timeout(timeout);
    fixture
        .worker
        .as_ref()
        .unwrap()
        .origin
        .block(RequestKind::Head);
    let mut socket = fixture.connect();
    socket
        .write_all(&request(
            "POST",
            "If-Match: \"v1\"\r\nRange: bytes=0-16777215\r\nConnection: close\r\n",
        ))
        .unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    let scope = fixture.listeners.read_scopes.borrow()[0].clone();
    // Consume part of the total budget before committing response headers.
    fixture.wait_for_origin(RequestKind::Head, 1);
    sleep_until(scope.deadline.0 - timeout / 2);
    fixture
        .worker
        .as_ref()
        .unwrap()
        .origin
        .release(RequestKind::Head);
    let mut output = fixture.receive(&mut socket, false);
    assert!(output.starts_with(b"HTTP/1.1 200 "));
    let head_end = output
        .windows(4)
        .position(|part| part == b"\r\n\r\n")
        .unwrap()
        + 4;
    let mut bytes = [0; 8192];
    // Keep making observable body progress without draining the whole page.
    for _ in 0..4 {
        loop {
            fixture.pump(16);
            match socket.read(&mut bytes) {
                Ok(n) if n != 0 => {
                    output.extend_from_slice(&bytes[..n]);
                    break;
                }
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                    assert!(Instant::now() < scope.deadline.0);
                }
                result => panic!("expected streaming progress: {result:?}"),
            }
        }
    }
    assert!(output.len() > head_end);
    assert_eq!(fixture.listeners.active_connections(), 1);
    sleep_until(scope.deadline.0 + Duration::from_millis(20));
    for _ in 0..64 {
        fixture.pump(16);
    }
    // The initial budget is not renewed: it is no longer the body lifetime.
    // Delivery remains live under its independent write-stall bound.
    assert_eq!(fixture.listeners.active_connections(), 1);
    output.extend(fixture.receive(&mut socket, true));
    assert_eq!(
        output.len() - head_end,
        crate::model::PAGE_BYTES as usize + 42
    );
    assert!(
        output[head_end + 21..output.len() - 21]
            .iter()
            .all(|byte| *byte == b'x')
    );
    assert_only_idle_pipes(&fixture, &_pipes);
}

#[test]
fn client_disconnect_cancels_pending_metadata_before_acquisition_deadline() {
    let fixture = Fixture::new();
    fixture
        .worker
        .as_ref()
        .unwrap()
        .origin
        .block(RequestKind::Head);
    fixture.reconcile(&[definition()]).unwrap();
    let mut socket = fixture.connect();
    socket.write_all(&request("HEAD", "")).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    let scope = fixture.listeners.read_scopes.borrow()[0].clone();
    assert_eq!(scope.check(), Ok(()));
    fixture.wait_for_origin(RequestKind::Head, 1);
    drop(socket);
    for _ in 0..64 {
        fixture.pump(16);
    }
    assert!(scope.cancellation.is_cancelled());
    assert!(Instant::now() < scope.deadline.0);
    assert_no_body_leases(&fixture);
}

#[test]
fn actual_uds_pipe_waiters_progress_within_budget_and_overflow_before_206() {
    // Duplex completion frames and the next response head may be in flight
    // together, in addition to listener readiness. Keep reactor headroom
    // while independently exercising the bounded FIFO pipe queue.
    let (mut fixture, pipes) = body_fixture(4, false);
    let failures = crate::telemetry::Failures::default();
    fixture.listeners.responses = Rc::new(
        Responses::new(
            fixture.listeners.io.clone(),
            Rc::new(Delivery::new(
                pipes.clone(),
                fixture.reactor.clone(),
                Duration::from_secs(2),
            )),
        )
        .with_observer(failures.observer(crate::model::WorkerId(0))),
    );
    let held = pipes.acquire().unwrap();
    let mut first = start_body(&fixture);
    let mut second = start_body(&fixture);
    let mut third = start_body(&fixture);
    let mut fourth = start_body(&fixture);
    assert_no_head(&mut first);
    assert_no_head(&mut second);
    assert_no_head(&mut third);
    assert_no_head(&mut fourth);
    let mut overflow = start_body(&fixture);
    let output = fixture.receive(&mut overflow, true);
    assert!(output.starts_with(b"HTTP/1.1 503 "), "{output:?}");
    assert!(output.ends_with(b"\r\n\r\n"));
    drop(held);
    for socket in [&mut first, &mut second, &mut third, &mut fourth] {
        let output = fixture.receive(socket, true);
        let mut diagnostics = String::new();
        failures.write(&mut diagnostics).unwrap();
        assert!(
            output.starts_with(b"HTTP/1.1 200 "),
            "{output:?} {diagnostics}"
        );
        assert_eq!(&output[output.len() - 26..output.len() - 21], b"hello");
    }
    assert_only_idle_pipes(&fixture, &pipes);
}

#[test]
fn actual_uds_first_page_failure_is_complete_503() {
    let (mut fixture, _pipes) = body_fixture(16, true);
    let failures = crate::telemetry::Failures::default();
    let delivery = Rc::new(Delivery::new(
        _pipes.clone(),
        fixture.reactor.clone(),
        Duration::from_secs(2),
    ));
    fixture.listeners.responses = Rc::new(
        Responses::new(fixture.listeners.io.clone(), delivery)
            .with_observer(failures.observer(crate::model::WorkerId(0))),
    );
    // The adapter rejects the first page before success headers are committed.
    let mut socket = start_body(&fixture);
    let output = fixture.receive(&mut socket, true);
    assert!(output.starts_with(b"HTTP/1.1 503 "), "{output:?}");
    assert!(output.ends_with(b"\r\n\r\n"));
    assert_only_idle_pipes(&fixture, &_pipes);
    fixture.assert_metrics(1, 1, 0);
    let mut diagnostics = String::new();
    failures.write(&mut diagnostics).unwrap();
    assert!(
        diagnostics.contains("stage=FirstSlice error=Unavailable"),
        "{diagnostics}"
    );
    assert!(!diagnostics.contains("stage=NextSlice"));
}

#[test]
fn actual_uds_waiting_deadline_and_cache_shutdown_release_all_leases() {
    for cancel in [false, true] {
        let (mut fixture, pipes) = body_fixture(16, false);
        fixture.listeners = fixture
            .listeners
            .with_request_timeout(Duration::from_millis(200));
        let held = pipes.acquire().unwrap();
        let mut socket = start_body(&fixture);
        assert_no_head(&mut socket);
        fixture.assert_metrics(1, 0, 1);
        if cancel {
            fixture.listeners.cancel_cache(&definition().id).unwrap();
        } else {
            std::thread::sleep(Duration::from_millis(220));
        }
        let output = fixture.receive(&mut socket, true);
        assert!(output.starts_with(b"HTTP/1.1 503 "), "{output:?}");
        assert!(output.ends_with(b"\r\n\r\n"));
        drop(held);
        futures::executor::block_on(fixture.listeners.drain(&scope())).unwrap();
        assert_only_idle_pipes(&fixture, &pipes);
        fixture.assert_metrics(1, 1, 0);
    }
}

#[test]
fn actual_uds_empty_bootstrap_and_canceled_success() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let mut metadata = body_metadata(0);
    metadata.version.etag = StrongEtag::parse(b"\"\"").unwrap();
    fixture
        .worker
        .as_ref()
        .unwrap()
        .origin
        .set_version(metadata.clone());
    for canceled in [false, true] {
        let output = if canceled {
            fixture.result_output(
                ReadKind::Subscription {
                    pin: None,
                    range: None,
                    page_credits: 2,
                    byte_credits: 2 * PAGE_BYTES,
                    ordered: false,
                },
                Ok(ReadResponse {
                    metadata: metadata.clone(),
                    range: None,
                    body: None,
                }),
                true,
            )
        } else {
            let mut socket = fixture.connect();
            socket
                .write_all(&request("POST", "Connection: close\r\n"))
                .unwrap();
            fixture.receive(&mut socket, true)
        };
        let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
        let status = if canceled { 503 } else { 200 };
        assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
        assert!(text.contains(if canceled {
            "content-length: 0\r\n"
        } else {
            "content-length: 21\r\n"
        }));
        assert!(!text.contains("content-range:"));
        if canceled {
            assert!(text.ends_with("\r\n\r\n"));
        } else {
            assert_eq!(
                &output[output.len() - 21..],
                &[
                    2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
                ]
            );
        }
        if !canceled {
            assert!(text.contains("etag: \"\"\r\n"));
            assert!(text.contains("racer-expires-at: 0\r\n"));
            assert!(text.contains("content-type: application/octet-stream\r\n"));
        }
        fixture.assert_metrics(if canceled { 2 } else { 1 }, u64::from(canceled), 0);
    }
}

#[test]
fn actual_uds_head_keepalive_removal_and_accept_fairness() {
    let fixture = Fixture::new();
    assert!(!fixture.socket().exists());
    fixture.reconcile(&[definition()]).unwrap();
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    let mut idle = fixture.connect();
    fixture.pump(1);
    let mut socket = fixture.connect();
    socket
        .write_all(&request("HEAD", "If-Match: \"v1\"\r\n"))
        .unwrap();
    // A waiting idle connection must not monopolize a single-unit poll budget.
    for _ in 0..8 {
        fixture.pump(1);
    }
    assert_eq!(fixture.listeners.active_connections(), 2);
    let first = fixture.receive(&mut socket, false);
    let text = std::str::from_utf8(&first).unwrap().to_ascii_lowercase();
    assert!(text.starts_with("http/1.1 200"));
    assert!(text.contains("content-length: 17\r\n"));
    assert!(text.contains("etag: \"v1\"\r\n"));
    assert!(text.contains("racer-expires-at: 1234\r\n"));
    assert!(text.ends_with("\r\n\r\n"));
    socket.write_all(&request("HEAD", "")).unwrap();
    fixture.receive(&mut socket, false);
    assert_eq!(fixture.completed_heads(), 2);
    fixture.reconcile(&[]).unwrap();
    assert!(!fixture.socket().exists());
    assert!(fixture.receive(&mut socket, true).is_empty());
    assert!(fixture.receive(&mut idle, true).is_empty());
    assert_eq!(fixture.completed_heads(), 2);
}

#[test]
fn actual_uds_errors_and_idle_drain() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    for (method, fields, status, extra) in [
        ("GET", "", "405", "allow: head, post\r\n"),
        (
            "POST",
            "Range: bytes=2-1\r\n",
            "400",
            "content-length: 0\r\n",
        ),
        (
            "HEAD",
            "Authorization: \r\n",
            "400",
            "content-length: 0\r\n",
        ),
    ] {
        let mut socket = fixture.connect();
        socket.write_all(&request(method, fields)).unwrap();
        let output = fixture.receive(&mut socket, true);
        let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
        assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
        assert!(text.contains(extra), "{text}");
        assert!(text.ends_with("\r\n\r\n"));
    }
    assert_eq!(fixture.completed_heads(), 0);
    let mut idle = fixture.connect();
    fixture.pump(16);
    fixture.pump(16);
    fixture.listeners.stop_admission();
    assert!(fixture.receive(&mut idle, true).is_empty());
    futures::executor::block_on(fixture.listeners.drain(&scope())).unwrap();
    assert_eq!(fixture.listeners.active_connections(), 0);
}

#[test]
fn actual_uds_rejected_raw_heads_receive_sdk_errors() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let mut oversized = request("HEAD", &format!("X-Padding: {}\r\n", "x".repeat(32768)));
    // Hit the cap without sending beyond it: a full unterminated head is 431.
    oversized.truncate(32768);
    for (raw, status) in [
        (request("HEAD", "Authorization:secret\r\n"), 400),
        (request("HEAD", "Authorization:  secret\r\n"), 400),
        (
            request("HEAD", "Content-Length: 0\r\nContent-Length: 0\r\n"),
            400,
        ),
        (request("HEAD", "Transfer-Encoding: chunked\r\n"), 400),
        (oversized, 431),
        (
            request("HEAD", &format!("Racer-Metadata: {}\r\n", "x".repeat(8193))),
            431,
        ),
    ] {
        let mut socket = fixture.connect();
        socket.write_all(&raw).unwrap();
        let output = fixture.receive(&mut socket, true);
        let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
        assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
        assert!(text.contains("content-length: 0\r\n"));
        assert!(text.ends_with("\r\n\r\n"));
    }
    assert_eq!(fixture.completed_heads(), 0);
}

#[test]
fn actual_uds_configured_head_cap_counts_only_wire_bytes() {
    for limit in [512, super::MAX_HEAD_BYTES] {
        let mut fixture = Fixture::new();
        fixture.listeners.parser = RequestParser::new(limit);
        fixture.reconcile(&[definition()]).unwrap();
        for separator in ["", " ", "\t", "  "] {
            let fields = format!(
                "Connection: close\r\nX:{separator}{}\r\n",
                "x".repeat(
                    limit
                        - request("HEAD", &format!("Connection: close\r\nX:{separator}\r\n")).len()
                )
            );
            let raw = request("HEAD", &fields);
            assert_eq!(raw.len(), limit);
            let mut socket = fixture.connect();
            socket.write_all(&raw).unwrap();
            let output = fixture.receive(&mut socket, true);
            assert!(output.starts_with(b"HTTP/1.1 200 "));
        }
        let admitted = fixture.completed_heads();
        // Exhaust the raw cap on an unterminated head. Decoded value bytes
        // alone would fit; only framing can reject this before dispatch.
        let mut raw = request("HEAD", &format!("X:{}\r\n", "x".repeat(limit)));
        raw.truncate(limit);
        let mut socket = fixture.connect();
        socket.write_all(&raw).unwrap();
        let output = fixture.receive(&mut socket, true);
        assert!(output.starts_with(b"HTTP/1.1 431 "));
        assert_eq!(fixture.completed_heads(), admitted);
    }
}

#[test]
fn actual_uds_configured_head_limit_counts_received_bytes() {
    let mut fixture = Fixture::new();
    let limit = 512;
    fixture.listeners.parser = RequestParser::new(limit);
    fixture.reconcile(&[definition()]).unwrap();
    for separator in ["", " ", "\t", " \t"] {
        let prefix = request("HEAD", &format!("X:{separator}"));
        // Replace the request helper's final CRLF with field data and the
        // complete terminator. Unknown-field whitespace stays on the wire.
        let mut exact = prefix[..prefix.len() - 2].to_vec();
        exact.resize(limit - 4, b'x');
        exact.extend_from_slice(b"\r\n\r\n");
        let mut socket = fixture.connect();
        socket.write_all(&exact).unwrap();
        let reply = fixture.receive(&mut socket, false);
        assert!(reply.starts_with(b"HTTP/1.1 200 "));
        drop(socket);

        // This is the first limit bytes of a limit+1-byte head: the final
        // LF lies beyond the configured cap, so framing must reject it.
        exact.insert(exact.len() - 4, b'x');
        exact.truncate(limit);
        let mut socket = fixture.connect();
        socket.write_all(&exact).unwrap();
        let reply = fixture.receive(&mut socket, true);
        assert!(reply.starts_with(b"HTTP/1.1 431 "));
    }
    assert_eq!(fixture.completed_heads(), 4);
}

#[test]
fn actual_uds_read_failures_and_immutable_result_validation() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    for (error, fields, status, range) in [
        (Error::NotFound, "", 404, None),
        (Error::NotFound, "If-Match: \"v1\"\r\n", 412, None),
        (
            Error::UnsatisfiableRangeWithLength(17),
            "",
            416,
            Some("content-range: bytes */17\r\n"),
        ),
        (Error::Cancelled, "", 503, None),
        (Error::OriginRejected, "", 401, None),
        (Error::OriginForbidden, "", 403, None),
        (Error::Overloaded, "", 503, None),
    ] {
        let kind = if fields.is_empty() {
            ReadKind::Head
        } else {
            ReadKind::HeadPinned {
                etag: StrongEtag::parse(b"\"v1\"").unwrap(),
            }
        };
        let output = fixture.result_output(kind, Err(error), error == Error::Cancelled);
        let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
        assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
        assert!(text.contains("content-length: 0\r\n"));
        assert!(!text.contains("etag:") && !text.contains("racer-expires-at:"));
        if let Some(range) = range {
            assert!(text.contains(range));
        }
        assert!(text.ends_with("\r\n\r\n"));
    }
    fixture.assert_metrics(7, 7, 0);
    assert_eq!(
        fixture
            .listeners
            .metrics
            .count(crate::telemetry::Event::Overload),
        1
    );
    for wrong_object in [true, false] {
        let mut metadata = body_metadata(0);
        metadata.version.etag = StrongEtag::parse(b"\"other\"").unwrap();
        let kind = if wrong_object {
            metadata.version.object.key.0[0] ^= 1;
            ReadKind::Head
        } else {
            ReadKind::HeadPinned {
                etag: StrongEtag::parse(b"\"v1\"").unwrap(),
            }
        };
        let output = fixture.result_output(
            kind,
            Ok(ReadResponse {
                metadata,
                range: None,
                body: None,
            }),
            false,
        );
        assert!(output.starts_with(b"HTTP/1.1 502"));
    }
}

#[test]
fn real_coordinator_maps_adapter_metadata_failures() {
    for (status, pin, expected) in [
        (404, false, 404),
        (404, true, 502),
        (412, true, 412),
        (401, false, 401),
        (403, false, 403),
        (503, false, 503),
    ] {
        let fixture = Fixture::new();
        fixture
            .worker
            .as_ref()
            .unwrap()
            .origin
            .reject_next(RequestKind::Head, status);
        fixture.reconcile(&[definition()]).unwrap();
        let mut socket = fixture.connect();
        socket
            .write_all(&request(
                "HEAD",
                if pin { "If-Match: \"v1\"\r\n" } else { "" },
            ))
            .unwrap();
        let output = fixture.receive(&mut socket, true);
        assert!(
            output.starts_with(format!("HTTP/1.1 {expected} ").as_bytes()),
            "{output:?}"
        );
        assert_eq!(fixture.completed_heads(), 1);
        fixture.assert_metrics(1, 1, 0);
        assert_no_body_leases(&fixture);
    }
}

#[test]
fn prepared_transition_rolls_back_bind_chmod_and_drop() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let inode = fs::metadata(fixture.socket()).unwrap().ino();
    let mut changed = definition();
    changed.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let mut added = definition();
    added.id = CacheId("00000000-0000-4000-8000-000000000002".into());
    added.name = "blocked".into();
    added.client_socket = "/run/racer/blocked/client/socket".into();
    added.origin_socket = "/run/racer/blocked/origin/socket".into();
    fs::write(fixture.root.0.join("blocked"), b"foreign").unwrap();
    assert!(
        futures::executor::block_on(
            fixture
                .listeners
                .prepare(&[changed.clone(), added], &scope())
        )
        .is_err()
    );
    assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    FAIL_CHMOD.with(|fail| fail.set(true));
    assert!(
        futures::executor::block_on(fixture.listeners.prepare(&[changed.clone()], &scope()))
            .is_err()
    );
    assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    let prepared =
        futures::executor::block_on(fixture.listeners.prepare(&[changed.clone()], &scope()))
            .unwrap();
    assert_ne!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    assert!(matches!(
        futures::executor::block_on(fixture.listeners.prepare(&[], &scope())),
        Err(Error::Overloaded)
    ));
    drop(prepared);
    assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::read_dir(fixture.socket().parent().unwrap())
            .unwrap()
            .count(),
        3
    );
    let mut socket = fixture.connect();
    socket.write_all(&request("HEAD", "")).unwrap();
    assert!(
        fixture
            .receive(&mut socket, false)
            .starts_with(b"HTTP/1.1 200")
    );
    let prepared =
        futures::executor::block_on(fixture.listeners.prepare(&[changed], &scope())).unwrap();
    prepared.commit();
    fixture.pump(16);
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    assert!(fixture.receive(&mut socket, true).is_empty());
}

#[test]
fn abandoned_prepare_future_removes_temporary_socket() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let inode = fs::metadata(fixture.socket()).unwrap().ino();
    let mut changed = definition();
    changed.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let definitions = [changed];
    let scope = scope();
    let mut future = fixture.listeners.prepare(&definitions, &scope);
    let waker = futures::task::noop_waker();
    assert!(
        future
            .as_mut()
            .poll(&mut Context::from_waker(&waker))
            .is_pending()
    );
    assert_eq!(
        fs::read_dir(fixture.socket().parent().unwrap())
            .unwrap()
            .count(),
        5
    );
    drop(future);
    assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::read_dir(fixture.socket().parent().unwrap())
            .unwrap()
            .count(),
        3
    );
    assert!(!fixture.listeners.preparing.get());
}

#[test]
fn prepared_rename_failure_restores_already_exchanged_paths() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let inode = fs::metadata(fixture.socket()).unwrap().ino();
    let mut changed = definition();
    changed.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let mut added = definition();
    added.id = CacheId("00000000-0000-4000-8000-000000000002".into());
    added.name = "added".into();
    added.client_socket = "/run/racer/added/client/socket".into();
    added.origin_socket = "/run/racer/added/origin/socket".into();
    FAIL_RENAME_AFTER.with(|count| count.set(Some(1)));
    assert!(
        futures::executor::block_on(fixture.listeners.prepare(&[changed, added], &scope()))
            .is_err()
    );
    assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    assert_eq!(
        fs::read_dir(fixture.socket().parent().unwrap())
            .unwrap()
            .count(),
        3
    );
    assert_eq!(
        fs::read_dir(fixture.root.0.join("added/client"))
            .unwrap()
            .count(),
        1
    );
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
fn prepared_uid_reuse_and_foreign_replacement_are_inode_safe() {
    let fixture = Fixture::new();
    fixture.reconcile(&[definition()]).unwrap();
    let origin = fixture.root.0.join("example/origin");
    fs::create_dir(&origin).unwrap();
    fs::write(origin.join("socket"), b"origin").unwrap();
    let mut replacement = definition();
    replacement.id = CacheId("00000000-0000-4000-8000-000000000002".into());
    let prepared =
        futures::executor::block_on(fixture.listeners.prepare(&[replacement.clone()], &scope()))
            .unwrap();
    prepared.commit();
    fixture.pump(16);
    let inode = fs::metadata(fixture.socket()).unwrap().ino();
    assert!(
        fixture
            .listeners
            .listeners
            .borrow()
            .contains_key(&replacement.id)
    );
    replacement.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    let prepared =
        futures::executor::block_on(fixture.listeners.prepare(&[replacement], &scope())).unwrap();
    fs::remove_file(fixture.socket()).unwrap();
    fs::write(fixture.socket(), b"foreign").unwrap();
    drop(prepared);
    assert_eq!(fs::read(fixture.socket()).unwrap(), b"foreign");
    assert_eq!(fs::read(origin.join("socket")).unwrap(), b"origin");
    assert!(
        fs::read_dir(fixture.socket().parent().unwrap())
            .unwrap()
            .any(|entry| entry.unwrap().metadata().unwrap().ino() == inode)
    );
}

#[test]
fn removal_commit_drains_active_response_and_reused_uid_does_not_revive_old_keepalive() {
    let fixture = Fixture::new();
    fixture
        .worker
        .as_ref()
        .unwrap()
        .origin
        .block(RequestKind::Head);
    fixture.reconcile(&[definition()]).unwrap();
    // Model the descriptor reference inherited by a concurrent fork before exec.
    // Releasing the endpoint must not wait for that unrelated reference to close.
    let inherited_lock = fixture.listeners.listeners.borrow()[&definition().id]
        .owner
        .as_ref()
        .unwrap()
        .lock
        .try_clone()
        .unwrap();
    let mut socket = fixture.connect();
    socket.write_all(&request("HEAD", "")).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    assert_eq!(fixture.listeners.read_scopes.borrow().len(), 1);
    let operation_scope = fixture.listeners.read_scopes.borrow()[0].clone();
    fixture.wait_for_origin(RequestKind::Head, 1);
    fixture.reconcile(&[]).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    assert!(operation_scope.check().is_ok());
    assert_eq!(fixture.listeners.active_connections(), 1);
    fixture.reconcile(&[definition()]).unwrap();
    drop(inherited_lock);
    fixture
        .worker
        .as_ref()
        .unwrap()
        .origin
        .release(RequestKind::Head);
    let response = fixture.receive(&mut socket, true);
    assert!(response.starts_with(b"HTTP/1.1 200"));
    assert_eq!(fixture.listeners.active_connections(), 0);
    let mut new = fixture.connect();
    new.write_all(&request("HEAD", "Connection: close\r\n"))
        .unwrap();
    assert!(fixture.receive(&mut new, true).starts_with(b"HTTP/1.1 200"));
    assert_eq!(fixture.completed_heads(), 2);
}

#[test]
fn per_cache_cancellation_drains_active_read_and_keeps_other_cache() {
    let fixture = Fixture::new();
    fixture
        .worker
        .as_ref()
        .unwrap()
        .origin
        .block(RequestKind::Head);
    let mut other = definition();
    other.id = CacheId("00000000-0000-4000-8000-000000000002".into());
    other.name = "other".into();
    other.client_socket = "/run/racer/other/client/socket".into();
    other.origin_socket = "/run/racer/other/origin/socket".into();
    fixture.reconcile(&[definition(), other.clone()]).unwrap();
    let mut socket = fixture.connect();
    socket.write_all(&request("HEAD", "")).unwrap();
    for _ in 0..16 {
        fixture.pump(16);
    }
    assert_eq!(fixture.listeners.read_scopes.borrow().len(), 1);
    assert_eq!(
        fixture.listeners.active_connections_for(&definition().id),
        1
    );
    fixture.wait_for_origin(RequestKind::Head, 1);
    fixture.listeners.cancel_cache(&definition().id).unwrap();
    assert!(
        fixture
            .receive(&mut socket, true)
            .starts_with(b"HTTP/1.1 503")
    );
    let deadline = Instant::now() + Duration::from_secs(5);
    while fixture.listeners.active_connections_for(&definition().id) != 0 {
        fixture.pump(64);
        assert!(Instant::now() < deadline, "canceled cache did not drain");
    }
    assert_eq!(
        fixture.listeners.active_connections_for(&definition().id),
        0
    );
    assert!(fixture.listeners.listeners.borrow().contains_key(&other.id));
    assert!(fixture.root.0.join("other/client/socket").exists());
}

#[test]
fn socket_lifecycle_rejects_symlinks_and_preserves_unowned_paths() {
    let fixture = Fixture::new();
    let origin = fixture.root.0.join("example/origin");
    fs::create_dir_all(&origin).unwrap();
    fs::write(origin.join("socket"), b"adapter owned").unwrap();
    fixture.reconcile(&[definition()]).unwrap();
    let mut updated = definition();
    updated.id = CacheId("00000000-0000-4000-8000-000000000003".into());
    fixture.reconcile(&[updated]).unwrap();
    assert_eq!(
        fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
        0o666
    );
    // Replacing the pathname does not give us ownership of its replacement.
    fs::remove_file(fixture.socket()).unwrap();
    fs::write(fixture.socket(), b"replacement").unwrap();
    fixture.reconcile(&[]).unwrap();
    assert_eq!(fs::read(fixture.socket()).unwrap(), b"replacement");
    assert_eq!(fs::read(origin.join("socket")).unwrap(), b"adapter owned");
    assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io));
    fs::remove_file(fixture.socket()).unwrap();
    // The persistent lock deliberately survives endpoint removal. Move the
    // whole directory aside to exercise replacement by a symlink.
    fs::rename(
        fixture.socket().parent().unwrap(),
        fixture.root.0.join("old-client"),
    )
    .unwrap();
    std::os::unix::fs::symlink(&origin, fixture.socket().parent().unwrap()).unwrap();
    assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io));
    assert_eq!(fs::read(origin.join("socket")).unwrap(), b"adapter owned");
    let mut invalid = definition();
    invalid.name = "../escape".into();
    assert_eq!(fixture.reconcile(&[invalid]), Err(Error::InvalidRequest));
}
