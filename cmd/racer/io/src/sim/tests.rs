//! Reactor-independent contracts of the simulated OS: the syscall semantics it models.
#![allow(clippy::type_complexity)]
use super::*;
use std::ops::Deref;

/// A test owner of a simulated descriptor exposing its resource view. The
/// descriptor is never read; holding it keeps the resource open.
pub(super) struct Owned(#[allow(dead_code)] Descriptor, Handle);

impl Deref for Owned {
    type Target = Handle;
    fn deref(&self) -> &Handle {
        &self.1
    }
}

/// Old `into_sim` for tests: keep the descriptor alive with its view.
pub(super) trait IntoSim {
    fn into_sim(self) -> std::result::Result<Owned, Descriptor>;
}

impl IntoSim for Descriptor {
    fn into_sim(self) -> std::result::Result<Owned, Descriptor> {
        match self.as_sim() {
            Some(handle) => Ok(Owned(self, handle)),
            None => Err(self),
        }
    }
}

mod world {
    use super::*;
    use std::ffi::CString;

    /// A unique host directory under the project tmp directory, removed on drop.
    struct Directory(PathBuf);

    impl Directory {
        /// Creates a unique host fixture.
        fn new() -> Self {
            use std::sync::atomic::{AtomicUsize, Ordering};

            static NEXT: AtomicUsize = AtomicUsize::new(0);
            let root = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("../../../tmp")
                .join(format!(
                    "sim-open-differential-{}-{}",
                    std::process::id(),
                    NEXT.fetch_add(1, Ordering::Relaxed)
                ));
            std::fs::create_dir_all(&root).unwrap();
            Self(std::fs::canonicalize(root).unwrap())
        }
    }

    impl Drop for Directory {
        /// Remove the host fixture even when assertions unwind.
        fn drop(&mut self) {
            std::fs::remove_dir_all(&self.0).unwrap();
        }
    }
    use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};

    /// Requires a syscall failure and extracts its Linux error number for comparison.
    fn code<T>(result: io::Result<T>) -> i32 {
        result
            .err()
            .expect("expected error")
            .raw_os_error()
            .unwrap()
    }

    /// Consume synchronous transfer faults once without stealing driver-selected faults.
    #[test]
    fn transfer_fault_policy_preserves_matching_order_and_execution_ownership() {
        let sim = SimOs::new();
        sim.inject("write", Fault::Short(3)).unwrap();
        sim.inject("recv", Fault::Errno(libc::EINTR)).unwrap();
        sim.inject("*", Fault::Short(0)).unwrap();
        {
            let mut world = sim.0.borrow_mut();
            world.executing = true;
            assert_eq!(world.transfer_limit("recv").unwrap(), usize::MAX);
            assert_eq!(world.faults.len(), 3);
            world.executing = false;
            assert_eq!(code(world.transfer_limit("recv")), libc::EINTR);
            assert_eq!(world.transfer_limit("send").unwrap(), 0);
            assert_eq!(world.transfer_limit("write").unwrap(), 3);
            assert_eq!(world.transfer_limit("read").unwrap(), usize::MAX);
        }
        for fault in [Fault::Delay(2), Fault::HoldCompletion(2)] {
            sim.inject("read", fault).unwrap();
            assert_eq!(
                sim.0.borrow_mut().transfer_limit("read").unwrap(),
                usize::MAX
            );
            assert!(sim.0.borrow().faults.is_empty());
        }
        let faults: Vec<_> = sim
            .take_trace()
            .into_iter()
            .filter(|event| event.operation.starts_with("fault:"))
            .map(|event| (event.operation, event.result))
            .collect();
        assert_eq!(
            faults,
            [
                ("fault:recv".into(), -i64::from(libc::EINTR)),
                ("fault:send".into(), 0),
                ("fault:write".into(), 3),
                ("fault:read".into(), 2),
                ("fault:read".into(), 2),
            ]
        );
    }

    /// Reject foreign and host descriptors even when their numeric resource IDs collide.
    #[test]
    fn stream_mutation_checks_world_provenance_before_resource_lookup() {
        let sim = SimOs::new();
        let other = SimOs::new();
        let (local, peer) = sim.socket_pair();
        let (foreign, _foreign_peer) = other.socket_pair();
        assert_eq!(local.as_sim().unwrap().id(), foreign.as_sim().unwrap().id());
        let (host, _host_peer) = std::os::unix::net::UnixStream::pair().unwrap();
        let host = Descriptor::from(host);
        let a = SocketAddress::Inet("127.0.0.1:101".parse().unwrap());
        let b = SocketAddress::Inet("127.0.0.1:102".parse().unwrap());
        for invalid in [&foreign, &host] {
            assert_eq!(
                code(sim.label_stream(invalid, a.clone(), b.clone())),
                libc::EXDEV
            );
            assert_eq!(code(sim.disconnect(invalid)), libc::EXDEV);
        }
        sim.label_stream(&local, a.clone(), b.clone()).unwrap();
        sim.partition(a.clone(), b.clone());
        assert_eq!(code(local.try_send(b"x")), libc::EAGAIN);
        sim.heal(a, b);
        assert_eq!(local.try_send(b"x").unwrap(), 1);
        sim.disconnect(&local).unwrap();
        assert_eq!(peer.try_recv(&mut [0; 1]).unwrap(), 1);
        assert_eq!(peer.try_recv(&mut [0; 1]).unwrap(), 0);
        assert_eq!(code(local.try_send(b"x")), libc::EPIPE);
    }

    /// A blocked attempt is traced but is not a world change, so it does not
    /// make waiting operations of other reactors runnable again.
    #[test]
    fn blocked_attempts_are_traced_but_change_nothing() {
        let sim = SimOs::new();
        let (local, _peer) = sim.socket_pair();
        let a = SocketAddress::Inet("127.0.0.1:101".parse().unwrap());
        let b = SocketAddress::Inet("127.0.0.1:102".parse().unwrap());
        sim.label_stream(&local, a.clone(), b.clone()).unwrap();
        sim.partition(a, b);
        let (events, changes) = (sim.0.borrow().next_event, sim.0.borrow().changes);
        assert_eq!(code(local.try_send(b"x")), libc::EAGAIN);
        assert_eq!(sim.0.borrow().next_event, events + 1);
        assert_eq!(sim.0.borrow().changes, changes);
    }

    #[test]
    /// Exercise short sparse I/O, append, locks, bounds, and invalid fault inputs.
    fn short_files_zero_writes_append_locks_and_bounds() {
        let sim = SimOs::new();
        let file = sim
            .open(None, Path::new("/f"), libc::O_CREAT | libc::O_RDWR)
            .unwrap()
            .into_sim()
            .unwrap();
        sim.set_max_chunk(2).unwrap();
        assert_eq!(file.file_write(0, b"abcd").unwrap(), 2);
        assert_eq!(file.file_write(100, b"").unwrap(), 0);
        assert_eq!(file.stat().unwrap().stx_size, 2);
        assert_eq!(file.file_write(2, b"cd").unwrap(), 2);
        let mut bytes = [0xcc; 4];
        assert_eq!(file.file_read(0, &mut bytes).unwrap(), 2);
        assert_eq!(bytes, [b'a', b'b', 0xcc, 0xcc]);
        let append = sim
            .open(None, Path::new("/f"), libc::O_RDWR | libc::O_APPEND)
            .unwrap()
            .into_sim()
            .unwrap();
        append.file_write(0, b"ef").unwrap();
        assert_eq!(sim.read_file(Path::new("/f")).unwrap(), b"abcdef");
        file.lock().unwrap();
        file.lock().unwrap();
        assert_eq!(code(append.lock()), libc::EWOULDBLOCK);
        drop(file);
        append.lock().unwrap();
        assert_eq!(code(append.file_write(u64::MAX, b"x")), libc::EINVAL);
        assert_eq!(code(append.file_read(u64::MAX, &mut bytes)), libc::EINVAL);
        assert_eq!(code(append.set_len(u64::MAX)), libc::EINVAL);
        append.set_len(1 << 40).unwrap();
        assert_eq!(code(sim.read_file(Path::new("/f"))), libc::EFBIG);
        assert_eq!(
            code(
                sim.disk()
                    .read(Path::new("/f"), 0, usize::MAX, DiskState::Volatile)
            ),
            libc::EFBIG
        );
        assert_eq!(
            sim.disk()
                .read(Path::new("/f"), 1 << 39, 4, DiskState::Volatile)
                .unwrap(),
            [0; 4]
        );
        assert_eq!(code(sim.set_max_chunk(0)), libc::EINVAL);
        assert_eq!(code(sim.set_stream_capacity(0)), libc::EINVAL);
        assert_eq!(code(sim.set_stream_capacity(usize::MAX)), libc::EINVAL);
        assert_eq!(code(sim.inject("read", Fault::Errno(0))), libc::EINVAL);
    }

    #[test]
    /// Enforce supported resolver boundaries and nonrecursive mkdir semantics.
    fn resolver_policies_and_single_component_mkdir() {
        let sim = SimOs::new();
        sim.write_file(Path::new("/root/child/f"), b"x").unwrap();
        sim.symlink(Path::new("/child/f"), Path::new("/root/absolute"))
            .unwrap();
        sim.symlink(Path::new("child/f"), Path::new("/root/relative"))
            .unwrap();
        let dir = sim
            .open(None, Path::new("/root"), libc::O_DIRECTORY)
            .unwrap();
        for policy in [1, 0x20, 0x40, 0x18] {
            assert_eq!(
                code(sim.open_resolved(Some(&dir), Path::new("child/f"), 0, policy)),
                libc::EINVAL
            );
        }
        assert!(
            sim.open_resolved(Some(&dir), Path::new("relative"), 0, 0x02 | 0x08)
                .is_ok()
        );
        assert_eq!(
            code(sim.open_resolved(Some(&dir), Path::new("relative"), 0, 0x04)),
            libc::ELOOP
        );
        assert_eq!(
            code(sim.open_resolved(Some(&dir), Path::new("absolute"), 0, 0x08)),
            libc::EXDEV
        );
        assert_eq!(
            code(sim.open_resolved(Some(&dir), Path::new("../root/child/f"), 0, 0x08)),
            libc::EXDEV
        );
        assert!(
            sim.open_resolved(Some(&dir), Path::new("child/../child/f"), 0, 0x08)
                .is_ok()
        );
        for name in ["absolute", "/child/f", "../../child/f"] {
            assert!(
                sim.open_resolved(Some(&dir), Path::new(name), 0, 0x10)
                    .is_ok(),
                "{name}"
            );
        }
        assert_eq!(code(sim.mkdir(Path::new("/missing/child"))), libc::ENOENT);
        assert!(sim.metadata(Path::new("/missing")).is_err());
        sim.mkdir(Path::new("/one")).unwrap();
        assert_eq!(code(sim.mkdir(Path::new("/one"))), libc::EEXIST);
        assert_eq!(
            code(sim.mkdir(Path::new("/root/child/f/dir"))),
            libc::ENOTDIR
        );
    }

    #[test]
    /// Preserve socket families, half-close semantics, and renamed Unix listeners.
    fn socket_families_half_close_and_renamed_listener_descendants() {
        let sim = SimOs::new();
        let address = SocketAddress::Inet("127.0.0.1:8080".parse().unwrap());
        let listener = sim.listen(address.clone()).unwrap().into_sim().unwrap();
        let wrong = sim.socket(libc::AF_UNIX).unwrap().into_sim().unwrap();
        assert_eq!(code(wrong.connect(&address)), libc::EAFNOSUPPORT);
        let client = sim.connect(address).unwrap().into_sim().unwrap();
        let server = listener.accept().unwrap().into_sim().unwrap();
        client.send(b"hello").unwrap();
        client.shutdown(libc::SHUT_WR).unwrap();
        assert_eq!(code(client.send(b"x")), libc::EPIPE);
        assert_eq!(
            server.ready(libc::POLLRDHUP as u32).unwrap(),
            libc::POLLRDHUP as i32
        );
        let mut bytes = [0; 8];
        assert_eq!(server.recv(&mut bytes).unwrap(), 5);
        assert_eq!(server.recv(&mut bytes).unwrap(), 0);
        server.send(b"reply").unwrap();
        assert_eq!(client.recv(&mut bytes).unwrap(), 5);
        assert!(!server.idle_healthy());
        assert!(!server.peer_disconnected());
        assert!(server.peer_read_closed());
        assert_eq!(code(client.shutdown(123)), libc::EINVAL);
        sim.create_dir_all(Path::new("/old/child")).unwrap();
        let listener = sim
            .listen(SocketAddress::Unix("/old/child/socket".into()))
            .unwrap();
        sim.rename(Path::new("/old"), Path::new("/new"), 0).unwrap();
        assert!(
            sim.connect(SocketAddress::Unix("/old/child/socket".into()))
                .is_err()
        );
        assert!(
            sim.connect(SocketAddress::Unix("/new/child/socket".into()))
                .is_ok()
        );
        drop(listener);
    }

    #[test]
    /// Check pipe close errors, small-write atomicity, and empty splice behavior.
    fn pipe_close_atomicity_and_empty_splice() {
        let sim = SimOs::new();
        let (read, write) = sim.pipe(4096);
        let (read, write) = (read.into_sim().unwrap(), write.into_sim().unwrap());
        let (socket, _peer) = sim.socket_pair();
        let socket = socket.into_sim().unwrap();
        assert_eq!(code(read.splice(&socket, 1)), libc::EAGAIN);
        assert_eq!(read.splice(&socket, 0).unwrap(), 0);
        write.pipe_write(&[1; 4095]).unwrap();
        assert_eq!(code(write.pipe_write(&[2; 2])), libc::EAGAIN);
        assert_eq!(write.pipe_write(&[2; 4097]).unwrap(), 1);
        let mut bytes = [0; 4096];
        assert_eq!(read.pipe_read(&mut bytes).unwrap(), 4096);
        assert_eq!(bytes[4095], 2);
        drop(write);
        assert_eq!(read.pipe_read(&mut bytes).unwrap(), 0);
        assert_eq!(read.splice(&socket, 1).unwrap(), 0);
        let (read, write) = sim.pipe(4096);
        drop(read);
        let write = write.into_sim().unwrap();
        assert_eq!(code(write.pipe_write(b"x")), libc::EPIPE);
        assert_eq!(write.pipe_write(b"").unwrap(), 0);
    }

    #[test]
    /// Compare sparse writes, append, locks, and pipe EOF directly against Linux.
    fn linux_file_and_pipe_differential() {
        let raw = unsafe { libc::memfd_create(c"sim-differential".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(raw >= 0);
        let real = unsafe { OwnedFd::from_raw_fd(raw) };
        let sim = SimOs::new();
        let file = sim
            .open(None, Path::new("/f"), libc::O_CREAT | libc::O_RDWR)
            .unwrap()
            .into_sim()
            .unwrap();
        for (offset, bytes) in [(0, b"abc".as_slice()), (100, b""), (2, b"XY")] {
            let actual = unsafe {
                libc::pwrite(real.as_raw_fd(), bytes.as_ptr().cast(), bytes.len(), offset)
            };
            assert_eq!(
                file.file_write(offset as u64, bytes).unwrap(),
                actual as usize
            );
            let mut stat: libc::stat = unsafe { std::mem::zeroed() };
            assert_eq!(unsafe { libc::fstat(real.as_raw_fd(), &mut stat) }, 0);
            assert_eq!(file.stat().unwrap().stx_size, stat.st_size as u64);
        }
        assert_eq!(
            unsafe { libc::flock(real.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
            0
        );
        file.lock().unwrap();
        assert_eq!(
            unsafe { libc::fcntl(real.as_raw_fd(), libc::F_SETFL, libc::O_APPEND) },
            0
        );
        let append = sim
            .open(None, Path::new("/f"), libc::O_RDWR | libc::O_APPEND)
            .unwrap()
            .into_sim()
            .unwrap();
        assert_eq!(
            unsafe { libc::pwrite(real.as_raw_fd(), b"Z".as_ptr().cast(), 1, 0) },
            append.file_write(0, b"Z").unwrap() as isize
        );
        let mut actual = [0; 8];
        let count = unsafe {
            libc::pread(
                real.as_raw_fd(),
                actual.as_mut_ptr().cast(),
                actual.len(),
                0,
            )
        };
        assert_eq!(
            &actual[..count as usize],
            sim.read_file(Path::new("/f")).unwrap()
        );
        assert_eq!(
            unsafe { libc::flock(real.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
            0
        );
        file.lock().unwrap();
        let mut fds = [-1; 2];
        assert_eq!(
            unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_NONBLOCK | libc::O_CLOEXEC) },
            0
        );
        let reader = unsafe { OwnedFd::from_raw_fd(fds[0]) };
        let writer = unsafe { OwnedFd::from_raw_fd(fds[1]) };
        let (sr, sw) = sim.pipe(4096);
        let sr = sr.into_sim().unwrap();
        let mut byte = [0];
        assert_eq!(
            unsafe { libc::read(reader.as_raw_fd(), byte.as_mut_ptr().cast(), 1) },
            -1
        );
        assert_eq!(
            code(sr.pipe_read(&mut byte)),
            io::Error::last_os_error().raw_os_error().unwrap()
        );
        drop((writer, sw));
        assert_eq!(
            unsafe { libc::read(reader.as_raw_fd(), byte.as_mut_ptr().cast(), 1) },
            sr.pipe_read(&mut byte).unwrap() as isize
        );
    }

    #[test]
    /// Compare openat2 traversal policy errors against host syscalls.
    fn linux_openat2_policy_differential() {
        let directory = std::fs::File::open(".").unwrap();
        let sim = SimOs::new();
        sim.mkdir(Path::new("/root")).unwrap();
        let dir = sim
            .open(None, Path::new("/root"), libc::O_DIRECTORY)
            .unwrap();

        /// Linux openat2 argument layout used to compare path-resolution policies.
        #[repr(C)]
        struct How {
            flags: u64,

            mode: u64,

            resolve: u64,
        }
        for (name, policy) in [
            (".", 0x02),
            (".", 0x04),
            (".", 0x08),
            ("/", 0x08),
            ("..", 0x08),
            ("..", 0x10),
            ("/", 0x10),
            (".", 0x18),
            (".", 0x40),
        ] {
            let how = How {
                flags: libc::O_DIRECTORY as u64,
                mode: 0,
                resolve: policy,
            };
            let name_c = CString::new(name).unwrap();
            let result = unsafe {
                libc::syscall(
                    libc::SYS_openat2,
                    directory.as_raw_fd(),
                    name_c.as_ptr(),
                    &how,
                    std::mem::size_of::<How>(),
                )
            };
            let expected = if result < 0 {
                io::Error::last_os_error().raw_os_error()
            } else {
                drop(unsafe { OwnedFd::from_raw_fd(result as i32) });
                None
            };
            let actual = sim
                .open_resolved(Some(&dir), Path::new(name), libc::O_DIRECTORY, policy)
                .err()
                .and_then(|error| error.raw_os_error());
            assert_eq!(actual, expected, "{name} {policy:#x}");
        }
    }

    #[test]
    /// Keep crash watches bounded and reclaim unreachable durable images.
    fn repeated_crashes_compact_history_and_reclaim_orphan_images() {
        let sim = SimOs::new();
        let affected = sim.0.borrow_mut().disk.watch(vec!["/unused/file".into()]);
        let unrelated = sim.0.borrow_mut().disk.watch(vec!["/file".into()]);
        let network = sim.0.borrow_mut().disk.watch(vec![]);
        for _ in 0..4100 {
            sim.disk().crash_under(Path::new("/unused")).unwrap();
        }
        assert!(affected.crashed.get());
        assert!(!unrelated.crashed.get());
        assert!(!network.crashed.get());
        assert_eq!(code(sim.try_pipe(0)), libc::EINVAL);
        assert_eq!(code(sim.try_pipe(usize::MAX)), libc::EINVAL);
        sim.write_file(Path::new("/file"), b"x").unwrap();
        sim.disk().sync_all().unwrap();
        sim.unlink(Path::new("/file")).unwrap();
        sim.disk().sync(Path::new("/")).unwrap();
        sim.disk().crash().unwrap();
        assert!(sim.read_file(Path::new("/file")).is_err());
    }

    /// Parallel fixture instances own distinct paths and clean up independently.
    #[test]
    fn differential_directories_are_unique_and_cleanup_is_independent() {
        let first = Directory::new();
        let second = std::thread::spawn(Directory::new).join().unwrap();
        assert_ne!(first.0, second.0);
        let removed = first.0.clone();
        std::fs::write(second.0.join("keep"), b"owned").unwrap();
        drop(first);
        assert!(!removed.exists());
        assert_eq!(std::fs::read(second.0.join("keep")).unwrap(), b"owned");
        let removed = second.0.clone();
        drop(second);
        assert!(!removed.exists());
    }

    /// Compare access modes, directory suffixes, and ignored absolute-path dirfds.
    #[test]
    fn linux_open_access_suffix_and_absolute_dirfd_differential() {
        let directory = Directory::new();
        std::fs::create_dir(directory.0.join("dir")).unwrap();
        std::fs::write(directory.0.join("file"), b"data").unwrap();
        let sim = SimOs::new();
        sim.mkdir(Path::new("/dir")).unwrap();
        sim.write_file(Path::new("/file"), b"data").unwrap();
        for (target, link) in [
            ("file", "file-link"),
            ("dir", "dir-link"),
            ("file/", "file-slash"),
            ("file/.", "file-dot"),
            ("dir/", "dir-slash"),
            ("dir/.", "dir-dot"),
        ] {
            std::os::unix::fs::symlink(target, directory.0.join(link)).unwrap();
            sim.symlink(Path::new(target), &Path::new("/").join(link))
                .unwrap();
        }
        let real_dir = std::fs::File::open(&directory.0).unwrap();
        let real_file = std::fs::File::open(directory.0.join("file")).unwrap();
        let sim_dir = sim.open(None, Path::new("/"), libc::O_DIRECTORY).unwrap();
        let sim_file = sim.open(None, Path::new("/file"), libc::O_RDONLY).unwrap();

        /// Linux openat2 ABI used by the independent host comparison.
        #[repr(C)]
        struct How {
            flags: u64,

            mode: u64,

            resolve: u64,
        }
        for name in [
            "file",
            "file/",
            "file/.",
            "file/./",
            "dir",
            "dir/",
            "dir/.",
            "file-link/",
            "file-link/.",
            "dir-link/",
            "dir-link/.",
            "file-slash",
            "file-dot",
            "dir-slash",
            "dir-dot",
        ] {
            for flags in [
                libc::O_RDONLY,
                libc::O_WRONLY,
                libc::O_RDWR,
                libc::O_RDONLY | libc::O_TRUNC,
                libc::O_RDONLY | libc::O_NOFOLLOW,
                libc::O_PATH | libc::O_NOFOLLOW,
            ] {
                for resolve in [0, 0x08, 0x10] {
                    for (absolute, regular_dirfd) in [(false, false), (true, false), (true, true)] {
                        let (host_path, sim_path) = if absolute {
                            if resolve == 0x10 {
                                (Path::new("/").join(name), Path::new("/").join(name))
                            } else {
                                (directory.0.join(name), Path::new("/").join(name))
                            }
                        } else {
                            (PathBuf::from(name), PathBuf::from(name))
                        };
                        // Reset content so every successful truncation has an observable effect.
                        std::fs::write(directory.0.join("file"), b"data").unwrap();
                        sim.write_file(Path::new("/file"), b"data").unwrap();
                        let host_name =
                            CString::new(host_path.as_os_str().as_encoded_bytes()).unwrap();
                        let how = How {
                            flags: flags as u64,
                            mode: 0,
                            resolve,
                        };
                        let raw = unsafe {
                            libc::syscall(
                                libc::SYS_openat2,
                                if regular_dirfd {
                                    real_file.as_raw_fd()
                                } else {
                                    real_dir.as_raw_fd()
                                },
                                host_name.as_ptr(),
                                &how,
                                std::mem::size_of::<How>(),
                            )
                        };
                        let expected = if raw < 0 {
                            Err(io::Error::last_os_error().raw_os_error().unwrap())
                        } else {
                            Ok(unsafe { OwnedFd::from_raw_fd(raw as i32) })
                        };
                        let actual = sim
                            .open_resolved(
                                Some(if regular_dirfd { &sim_file } else { &sim_dir }),
                                &sim_path,
                                flags,
                                resolve,
                            )
                            .map_err(|e| e.raw_os_error().unwrap());
                        assert_eq!(
                            actual.as_ref().err(),
                            expected.as_ref().err(),
                            "{name} flags={flags:#x} resolve={resolve:#x} absolute={absolute} regular_dirfd={regular_dirfd}"
                        );
                        if let (Ok(actual), Ok(expected)) = (actual, expected) {
                            let mut stat: libc::stat = unsafe { std::mem::zeroed() };
                            assert_eq!(unsafe { libc::fstat(expected.as_raw_fd(), &mut stat) }, 0);
                            assert_eq!(
                                actual.as_sim().unwrap().stat().unwrap().stx_mode as u32
                                    & libc::S_IFMT,
                                stat.st_mode & libc::S_IFMT
                            );
                        }
                        assert_eq!(
                            sim.read_file(Path::new("/file")).unwrap(),
                            std::fs::read(directory.0.join("file")).unwrap()
                        );
                    }
                }
            }
        }
    }

    #[test]
    /// Compare final symlink and O_PATH behavior across supported open policies.
    fn linux_final_symlink_and_path_descriptor_differential() {
        let directory = Directory::new();
        std::fs::create_dir(directory.0.join("real")).unwrap();
        std::fs::write(directory.0.join("real/file"), b"data").unwrap();
        let sim = SimOs::new();
        sim.write_file(Path::new("/real/file"), b"data").unwrap();
        for (target, link) in [
            ("real", "parent"),
            ("file", "real/link"),
            ("missing", "real/dangling"),
            ("loop", "real/loop"),
        ] {
            std::os::unix::fs::symlink(target, directory.0.join(link)).unwrap();
            sim.symlink(Path::new(target), &Path::new("/").join(link))
                .unwrap();
        }
        let real_dir = std::fs::File::open(&directory.0).unwrap();
        let sim_dir = sim.open(None, Path::new("/"), libc::O_DIRECTORY).unwrap();

        /// Linux openat2 argument layout used to compare final-symlink handling.
        #[repr(C)]
        struct How {
            flags: u64,

            mode: u64,

            resolve: u64,
        }
        for path in [
            "real/file",
            "real/link",
            "real/dangling",
            "real/loop",
            "parent/link",
            "parent/dangling",
        ] {
            for flags in [
                libc::O_RDONLY,
                libc::O_RDONLY | libc::O_NOFOLLOW,
                libc::O_CREAT | libc::O_EXCL | libc::O_RDWR,
                libc::O_CREAT | libc::O_EXCL | libc::O_RDWR | libc::O_NOFOLLOW,
                libc::O_PATH,
                libc::O_PATH | libc::O_NOFOLLOW,
                libc::O_PATH | libc::O_NOFOLLOW | libc::O_DIRECTORY,
            ] {
                for resolve in [0, 0x04, 0x08] {
                    let how = How {
                        flags: flags as u64,
                        mode: if flags & libc::O_CREAT != 0 { 0o600 } else { 0 },
                        resolve,
                    };
                    let name = CString::new(path).unwrap();
                    let raw = unsafe {
                        libc::syscall(
                            libc::SYS_openat2,
                            real_dir.as_raw_fd(),
                            name.as_ptr(),
                            &how,
                            std::mem::size_of::<How>(),
                        )
                    };
                    let expected = if raw < 0 {
                        Err(io::Error::last_os_error().raw_os_error().unwrap())
                    } else {
                        Ok(unsafe { OwnedFd::from_raw_fd(raw as i32) })
                    };
                    let actual = sim
                        .open_resolved(Some(&sim_dir), Path::new(path), flags, resolve)
                        .map_err(|error| error.raw_os_error().unwrap());
                    assert_eq!(
                        actual.as_ref().err(),
                        expected.as_ref().err(),
                        "{path} flags={flags:#x} resolve={resolve:#x}"
                    );
                    if let (Ok(actual), Ok(expected)) = (actual, expected) {
                        let h = actual.as_sim().unwrap();
                        let mut stat: libc::stat = unsafe { std::mem::zeroed() };
                        assert_eq!(unsafe { libc::fstat(expected.as_raw_fd(), &mut stat) }, 0);
                        assert_eq!(
                            h.stat().unwrap().stx_mode as u32 & libc::S_IFMT,
                            stat.st_mode & libc::S_IFMT
                        );
                        if flags & libc::O_PATH != 0 {
                            let mut bytes = [0; 4];
                            assert_eq!(
                                unsafe {
                                    libc::pread(
                                        expected.as_raw_fd(),
                                        bytes.as_mut_ptr().cast(),
                                        4,
                                        0,
                                    )
                                },
                                -1
                            );
                            assert_eq!(
                                h.file_read(0, &mut bytes).unwrap_err().raw_os_error(),
                                io::Error::last_os_error().raw_os_error()
                            );
                            assert_eq!(unsafe { libc::fsync(expected.as_raw_fd()) }, -1);
                            assert_eq!(
                                h.sync().unwrap_err().raw_os_error(),
                                io::Error::last_os_error().raw_os_error()
                            );
                            assert_eq!(
                                h.file_write(0, b"bad").unwrap_err().raw_os_error(),
                                Some(libc::EBADF)
                            );
                            assert_eq!(h.set_len(0).unwrap_err().raw_os_error(), Some(libc::EBADF));
                            assert_eq!(h.lock().unwrap_err().raw_os_error(), Some(libc::EBADF));
                        }
                    }
                }
            }
        }
        assert_eq!(sim.read_file(Path::new("/real/file")).unwrap(), b"data");
        assert!(sim.metadata(Path::new("/real/missing")).is_err());
    }

    #[test]
    /// Reject invalid simulation bounds and fault plans without partial mutation.
    fn bounded_simulation_setters_validate_atomically() {
        let sim = SimOs::new();
        sim.set_stream_capacity(MAX_ALLOCATION).unwrap();
        for invalid in [0, MAX_ALLOCATION + 1, usize::MAX] {
            assert_eq!(
                sim.set_stream_capacity(invalid).unwrap_err().raw_os_error(),
                Some(libc::EINVAL)
            );
            assert_eq!(sim.0.borrow().stream_capacity, MAX_ALLOCATION);
            assert_eq!(
                sim.try_pipe(invalid).unwrap_err().raw_os_error(),
                Some(libc::EINVAL)
            );
            assert_eq!(sim.live_handles(), 0);
        }
        let (a, b) = sim.try_pipe(MAX_ALLOCATION).unwrap();
        drop((a, b));
        sim.set_max_chunk(usize::MAX).unwrap();
        assert_eq!(
            sim.set_max_chunk(0).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(sim.0.borrow().max_chunk, usize::MAX);
        sim.reject_submissions(16384).unwrap();
        assert_eq!(
            sim.reject_submissions(16385).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(sim.0.borrow().reject_submissions, 16384);
        sim.reject_submissions(0).unwrap();
        for invalid in [i32::MIN, -1, 0, 4096, i32::MAX] {
            assert_eq!(
                sim.inject("read", Fault::Errno(invalid))
                    .unwrap_err()
                    .raw_os_error(),
                Some(libc::EINVAL)
            );
            assert!(sim.0.borrow().faults.is_empty());
        }
        for _ in 0..16384 {
            sim.inject("read", Fault::Errno(4095)).unwrap();
        }
        assert_eq!(
            sim.inject("write", Fault::Short(1))
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ENOSPC)
        );
        assert_eq!(sim.0.borrow().faults.len(), 16384);
        assert!(matches!(
            sim.0.borrow_mut().fault("read"),
            Some(Fault::Errno(4095))
        ));
        sim.inject("write", Fault::Short(1)).unwrap();
        sim.set_cancel_first(false);
        assert!(!sim.0.borrow().cancel_first);
        sim.set_cancel_first(true);
        assert!(sim.0.borrow().cancel_first);
    }
}
mod disk {
    use super::*;

    /// Reads a small volatile prefix without allocating a sparse file's logical size.
    fn read(sim: &SimOs, path: &str) -> Vec<u8> {
        sim.disk()
            .read(Path::new(path), 0, 64, DiskState::Volatile)
            .unwrap()
    }

    #[test]
    /// Cover sparse page boundaries, holes, short progress, and snapshot isolation.
    fn sparse_page_copies_preserve_boundaries_holes_and_snapshots() {
        for offset in [0, 1, 4095, 4096, 4097] {
            for length in [0, 1, 4095, 4096, 4097, 8193] {
                let sim = SimOs::new();
                let file = sim
                    .open(None, Path::new("/file"), libc::O_CREAT | libc::O_RDWR)
                    .unwrap()
                    .into_sim()
                    .unwrap();
                // Leave a full sparse page before an unaligned multi-page write.
                let offset = 8192 + offset;
                let original: Vec<_> = (0..length).map(|i| (i % 251) as u8).collect();
                assert_eq!(file.file_write(offset, &original).unwrap(), length);
                let mut expected = vec![0; if length == 0 { 0 } else { offset as usize }];
                expected.extend_from_slice(&original);
                let (node, _) = file.node().unwrap();
                let snapshot = node.borrow().pages.clone();
                let replacement = vec![0xa5; length + 1];
                sim.inject("write", Fault::Short(length / 2)).unwrap();
                assert_eq!(file.file_write(offset, &replacement).unwrap(), length / 2);
                if length != 0 {
                    expected[offset as usize..offset as usize + length / 2].fill(0xa5);
                }
                for (index, byte) in original.iter().enumerate() {
                    let pos = offset as usize + index;
                    assert_eq!(snapshot[&(pos as u64 / 4096)][pos % 4096], *byte);
                }
                let mut output = vec![0xcc; expected.len() + 8];
                assert_eq!(file.file_read(0, &mut output).unwrap(), expected.len());
                assert_eq!(&output[..expected.len()], expected);
                assert_eq!(&output[expected.len()..], &[0xcc; 8]);
                let mut partial = vec![0xcc; length + 8];
                sim.inject("read", Fault::Short(length / 2)).unwrap();
                assert_eq!(file.file_read(offset, &mut partial).unwrap(), length / 2);
                if length != 0 {
                    assert_eq!(
                        &partial[..length / 2],
                        &expected[offset as usize..offset as usize + length / 2]
                    );
                }
                assert!(partial[length / 2..].iter().all(|b| *b == 0xcc));
                sim.inject("write", Fault::Errno(libc::EIO)).unwrap();
                assert_eq!(
                    file.file_write(offset, b"bad").unwrap_err().raw_os_error(),
                    Some(libc::EIO)
                );
                assert_eq!(sim.read_file(Path::new("/file")).unwrap(), expected);
            }
        }
    }

    #[test]
    /// Require namespace fences for replacements, unlink, and new directory ancestors.
    fn replacement_unlink_and_directory_ancestors_require_namespace_fences() {
        let sim = SimOs::new();
        sim.write_file(Path::new("/disk/current"), b"old").unwrap();
        sim.disk().sync_all().unwrap();
        sim.write_file(Path::new("/disk/stage"), b"new").unwrap();
        sim.disk().sync(Path::new("/disk/stage")).unwrap();
        sim.rename(Path::new("/disk/stage"), Path::new("/disk/current"), 0)
            .unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/disk/current"), b"old");
        sim.write_file(Path::new("/disk/stage"), b"new").unwrap();
        sim.disk().sync(Path::new("/disk/stage")).unwrap();
        sim.rename(Path::new("/disk/stage"), Path::new("/disk/current"), 0)
            .unwrap();
        sim.disk().sync(Path::new("/disk")).unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/disk/current"), b"new");
        sim.unlink(Path::new("/disk/current")).unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/disk/current"), b"new");
        sim.unlink(Path::new("/disk/current")).unwrap();
        sim.disk().sync(Path::new("/disk")).unwrap();
        sim.disk().crash().unwrap();
        assert!(sim.read_file(Path::new("/disk/current")).is_err());
        sim.write_file(Path::new("/disk/child/file"), b"hidden")
            .unwrap();
        sim.disk().sync(Path::new("/disk/child/file")).unwrap();
        sim.disk().sync(Path::new("/disk/child")).unwrap();
        sim.disk().crash().unwrap();
        assert!(sim.metadata(Path::new("/disk/child")).is_err());
    }

    #[test]
    /// Sync renamed directory inodes and reject corruption across replacement generations.
    fn renamed_directory_fsync_uses_inode_and_durable_corruption_targets_exact_generation() {
        let sim = SimOs::new();
        sim.write_file(Path::new("/old/file"), b"before").unwrap();
        sim.disk().sync_all().unwrap();
        let directory = sim
            .open(None, Path::new("/old"), libc::O_RDONLY | libc::O_DIRECTORY)
            .unwrap();
        sim.rename(Path::new("/old"), Path::new("/new"), 0).unwrap();
        sim.write_file(Path::new("/new/extra"), b"extra").unwrap();
        sim.disk().sync(Path::new("/new/extra")).unwrap();
        let directory = directory.into_sim().unwrap();
        directory.sync().unwrap();
        sim.disk().sync(Path::new("/")).unwrap();
        sim.disk().crash().unwrap();
        assert_eq!(read(&sim, "/new/file"), b"before");
        assert_eq!(read(&sim, "/new/extra"), b"extra");
        assert!(sim.metadata(Path::new("/old")).is_err());
        sim.write_file(Path::new("/stage"), b"replace").unwrap();
        sim.rename(Path::new("/stage"), Path::new("/new/file"), 0)
            .unwrap();
        assert_eq!(
            sim.disk()
                .corrupt(Path::new("/new/file"), 0, b"X", DiskState::Both)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ESTALE)
        );
        assert_eq!(read(&sim, "/new/file"), b"replace");
        assert_eq!(
            sim.disk()
                .read(Path::new("/new/file"), 0, 64, DiskState::Durable)
                .unwrap(),
            b"before"
        );
    }

    // Persistence faults mutate the integrated disk; production entries own I/O.
}
mod network {
    use super::*;
    #[test]
    fn relative_unix_listener_lookup_unlink_and_rebind() {
        let sim = SimOs::new();
        let relative = SocketAddress::Unix("sock".into());
        let absolute = SocketAddress::Unix("/sock".into());
        let listener = sim.listen(relative.clone()).unwrap();
        assert_eq!(
            sim.metadata(Path::new("sock")).unwrap(),
            sim.metadata(Path::new("/sock")).unwrap()
        );
        assert_eq!(
            sim.listen(absolute.clone()).unwrap_err().raw_os_error(),
            Some(libc::EADDRINUSE)
        );
        let client = sim.connect(absolute.clone()).unwrap();
        assert!(listener.as_sim().unwrap().accept().is_ok());
        drop(client);
        sim.unlink(Path::new("sock")).unwrap();
        assert_eq!(
            sim.connect(relative.clone()).unwrap_err().raw_os_error(),
            Some(libc::ECONNREFUSED)
        );
        let rebound = sim.listen(relative.clone()).unwrap();
        assert!(sim.connect(relative).is_ok());
        drop((listener, rebound));
        assert_eq!(
            sim.listen(absolute).unwrap_err().raw_os_error(),
            Some(libc::EADDRINUSE)
        );
    }

    #[test]
    fn pipe_read_readiness_separates_queued_bytes_from_writer_close() {
        for len in 0..=2 {
            let sim = SimOs::new();
            let (read, write) = sim.pipe(2);
            let read = read.into_sim().unwrap();
            assert_eq!(
                write.as_sim().unwrap().pipe_write(&b"ab"[..len]).unwrap(),
                len
            );
            if len == 0 {
                assert_eq!(
                    read.ready(libc::POLLIN as u32).unwrap_err().raw_os_error(),
                    Some(libc::EAGAIN)
                );
            } else {
                assert_eq!(
                    read.ready(libc::POLLIN as u32).unwrap(),
                    libc::POLLIN as i32
                );
            }
            drop(write);
            let data = if len == 0 { 0 } else { libc::POLLIN };
            assert_eq!(
                read.ready(libc::POLLIN as u32).unwrap(),
                (data | libc::POLLHUP) as i32
            );
            assert_eq!(read.ready(0).unwrap(), libc::POLLHUP as i32);
            let mut output = [0; 2];
            assert_eq!(read.pipe_read(&mut output).unwrap(), len);
            assert_eq!(&output[..len], &b"ab"[..len]);
            assert_eq!(
                read.ready(libc::POLLIN as u32).unwrap(),
                libc::POLLHUP as i32
            );
            assert_eq!(read.pipe_read(&mut output).unwrap(), 0);
        }
    }

    #[test]
    fn pipe_write_readiness_separates_capacity_from_reader_close() {
        for len in 0..=2 {
            let sim = SimOs::new();
            let (read, write) = sim.pipe(2);
            let write = write.into_sim().unwrap();
            assert_eq!(write.pipe_write(&b"ab"[..len]).unwrap(), len);
            if len == 2 {
                assert_eq!(
                    write
                        .ready(libc::POLLOUT as u32)
                        .unwrap_err()
                        .raw_os_error(),
                    Some(libc::EAGAIN)
                );
            } else {
                assert_eq!(
                    write.ready(libc::POLLOUT as u32).unwrap(),
                    libc::POLLOUT as i32
                );
            }
            drop(read);
            let capacity = if len < 2 { libc::POLLOUT } else { 0 };
            assert_eq!(
                write.ready(libc::POLLOUT as u32).unwrap(),
                (capacity | libc::POLLERR) as i32
            );
            assert_eq!(write.ready(0).unwrap(), libc::POLLERR as i32);
            assert_eq!(
                write.pipe_write(b"x").unwrap_err().raw_os_error(),
                Some(libc::EPIPE)
            );
        }
    }

    #[test]
    /// Preserve wrapped queue prefixes across short reads, splice failures, and EOF.
    fn wrapped_stream_and_pipe_copies_preserve_short_io_and_errors() {
        let sim = SimOs::new();
        let (writer, reader) = sim.socket_pair();
        let (writer, reader) = (writer.into_sim().unwrap(), reader.into_sim().unwrap());
        let (pipe_reader, pipe_writer) = sim.pipe(16);
        let (pipe_reader, pipe_writer) = (
            pipe_reader.into_sim().unwrap(),
            pipe_writer.into_sim().unwrap(),
        );
        // Install wrapped queues explicitly so this does not depend on allocator growth.
        let wrapped = || {
            let mut queue = VecDeque::with_capacity(16);
            queue.extend(0..16);
            queue.drain(..12);
            queue.extend(16..24);
            assert!(!queue.as_slices().1.is_empty());
            queue
        };
        {
            let mut world = sim.0.borrow_mut();
            let Resource::Socket { bytes, .. } = world.resources.get_mut(&reader.id).unwrap()
            else {
                unreachable!()
            };
            *bytes = wrapped();
            let Resource::Pipe { bytes, .. } = world.resources.get(&pipe_reader.id).unwrap() else {
                unreachable!()
            };
            *bytes.borrow_mut() = wrapped();
        }
        let mut output = [0xcc; 16];
        sim.inject("recv", Fault::Short(7)).unwrap();
        assert_eq!(reader.recv(&mut output).unwrap(), 7);
        assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
        assert_eq!(&output[7..], &[0xcc; 9]);
        assert_eq!(reader.recv(&mut output).unwrap(), 5);
        assert_eq!(&output[..5], &[19, 20, 21, 22, 23]);
        sim.inject("pipe_read", Fault::Short(7)).unwrap();
        assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 7);
        assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
        assert_eq!(pipe_writer.pipe_write(&[24, 25, 26, 27]).unwrap(), 4);
        sim.inject("send", Fault::Errno(libc::EPIPE)).unwrap();
        assert_eq!(
            pipe_reader.splice(&writer, 9).unwrap_err().raw_os_error(),
            Some(libc::EPIPE)
        );
        sim.inject("splice", Fault::Short(6)).unwrap();
        assert_eq!(pipe_reader.splice(&writer, 9).unwrap(), 6);
        assert_eq!(reader.recv(&mut output).unwrap(), 6);
        assert_eq!(&output[..6], &[19, 20, 21, 22, 23, 24]);
        assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 3);
        assert_eq!(&output[..3], &[25, 26, 27]);
        assert_eq!(
            pipe_reader.pipe_read(&mut output).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(
            reader.recv(&mut output).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        drop(writer);
        assert_eq!(reader.recv(&mut output).unwrap(), 0);
    }

    #[test]
    /// Keep source addresses and packet boundaries through a datagram round trip.
    fn datagrams_preserve_packet_boundaries_and_source_addresses() {
        let sim = SimOs::new();
        let server_address = "127.0.0.1:53".parse().unwrap();
        let server = sim.bind_datagram(server_address).unwrap();
        let client = sim.bind_datagram("127.0.0.1:0".parse().unwrap()).unwrap();
        let (server, client) = (server.into_sim().unwrap(), client.into_sim().unwrap());
        client.connect_datagram(server_address).unwrap();
        client.send_datagram(b"query").unwrap();
        let mut bytes = [0; 32];
        let (count, source) = server.recv_from(&mut bytes).unwrap();
        assert_eq!(&bytes[..count], b"query");
        server.send_to(b"reply", source).unwrap();
        let (count, source) = client.recv_from(&mut bytes).unwrap();
        assert_eq!(source, server_address);
        assert_eq!(&bytes[..count], b"reply");
    }

    #[test]
    /// Plain `send`/`recv` on a connected datagram socket move whole packets,
    /// as on Linux, so reactor stream operations can drive UDP.
    fn connected_datagrams_accept_send_and_recv() {
        let sim = SimOs::new();
        let server_address = "127.0.0.1:53".parse().unwrap();
        let server = sim.bind_datagram(server_address).unwrap();
        let client = sim.bind_datagram("127.0.0.1:0".parse().unwrap()).unwrap();
        let (server, client) = (server.into_sim().unwrap(), client.into_sim().unwrap());
        let mut bytes = [0; 4];
        assert_eq!(
            client.recv(&mut bytes).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(
            client.send(b"q").unwrap_err().raw_os_error(),
            Some(libc::ENOTCONN)
        );
        client.connect_datagram(server_address).unwrap();
        assert_eq!(client.send(b"query").unwrap(), 5);
        let (_, source) = server.recv_from(&mut [0; 8]).unwrap();
        server.send_to(b"long reply", source).unwrap();
        server.send_to(b"next", source).unwrap();
        assert_eq!(client.recv(&mut bytes).unwrap(), 4);
        assert_eq!(&bytes, b"long");
        assert_eq!(client.recv(&mut bytes).unwrap(), 4);
        assert_eq!(&bytes, b"next");
    }
}

mod driver {
    use super::*;
    use crate::ring::op::Op;

    /// Run turns until the driver reports nothing runnable, returning the
    /// completions in order. Panics if it never settles.
    fn drain(driver: &mut SimDriver) -> Vec<(u64, i32, u32)> {
        let now = std::time::Instant::now();
        for _ in 0..64 {
            if !driver.runnable(now) {
                break;
            }
            driver.run_turn(now);
        }
        assert!(!driver.runnable(now), "driver never settled");
        std::iter::from_fn(|| driver.next_cqe()).collect()
    }

    /// An operation whose submission delay counts down to zero must stay
    /// runnable until it executes; it used to stall with no result.
    #[test]
    fn delayed_operation_stays_runnable_until_it_executes() {
        let os = SimOs::new();
        os.inject("fsync", Fault::Delay(2)).unwrap();
        let mut driver = SimDriver::new(os, 8, crate::Clock::host());
        let op = Op::Fsync {
            fd: 9999,
            datasync: false,
        };
        driver.push(1, &op).unwrap();
        assert_eq!(drain(&mut driver), [(1, -libc::EBADF, 0)]);
    }

    /// Every seed completes every delayed operation.
    #[test]
    fn seeded_delays_never_stall() {
        for seed in 0..64 {
            let mut driver = SimDriver::new(SimOs::seeded(seed), 8, crate::Clock::host());
            for id in 1..=4 {
                let op = Op::Fsync {
                    fd: 9999,
                    datasync: false,
                };
                driver.push(id, &op).unwrap();
            }
            let mut done: Vec<u64> = drain(&mut driver).iter().map(|c| c.0).collect();
            done.sort_unstable();
            assert_eq!(done, [1, 2, 3, 4], "seed {seed}");
        }
    }
}

mod choices {
    use super::*;
    use crate::{ctx::ChoiceSource, ring::op::Op};

    /// Replays a fixed list of draws, then zeros.
    struct Script(VecDeque<u64>);

    impl ChoiceSource for Script {
        fn next_u64(&mut self) -> u64 {
            self.0.pop_front().unwrap_or(0)
        }
    }

    fn fsync() -> Op {
        Op::Fsync {
            fd: 9999,
            datasync: false,
        }
    }

    fn run(driver: &mut SimDriver) -> Vec<u64> {
        let now = std::time::Instant::now();
        for _ in 0..64 {
            driver.run_turn(now);
        }
        std::iter::from_fn(|| driver.next_cqe())
            .map(|c| c.0)
            .collect()
    }

    /// An all-zero source keeps submission order with no delays.
    #[test]
    fn zero_source_keeps_fifo_order() {
        let mut driver = SimDriver::new(
            SimOs::with_choices(Script(VecDeque::new())),
            8,
            crate::Clock::host(),
        );
        for id in 1..=4 {
            driver.push(id, &fsync()).unwrap();
        }
        assert_eq!(run(&mut driver), [1, 2, 3, 4]);
    }

    /// Delays and execution order come straight from the source.
    #[test]
    fn source_draws_delays_and_order() {
        // Push draws: delays 2, 0, 0. First turn shuffle (index 2 then 1):
        // back 2 swaps ids[2] with ids[0], giving [3, 2, 1].
        let script = Script([2, 0, 0, 2, 0].into());
        let mut driver = SimDriver::new(SimOs::with_choices(script), 8, crate::Clock::host());
        for id in 1..=3 {
            driver.push(id, &fsync()).unwrap();
        }
        assert_eq!(run(&mut driver), [3, 2, 1]);
    }
}
