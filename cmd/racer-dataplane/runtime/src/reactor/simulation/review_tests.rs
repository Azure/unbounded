use super::io_tests::{drive, poll, reactor, scope};
use super::*;

fn code<T>(result: io::Result<T>) -> i32 {
    result
        .err()
        .expect("expected error")
        .raw_os_error()
        .unwrap()
}

#[test]
fn short_files_zero_writes_append_locks_and_bounds() {
    let sim = Simulation::new();
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
fn resolver_policies_and_single_component_mkdir() {
    let sim = Simulation::new();
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
fn delayed_alias_open_is_invalidated_by_target_disk_not_alias_disk() {
    for crash_root in ["/storage", "/alias"] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let scope = scope();
        sim.write_file(Path::new("/storage/f"), b"x").unwrap();
        sim.create_dir_all(Path::new("/alias")).unwrap();
        sim.symlink(Path::new("/storage/f"), Path::new("/alias/f"))
            .unwrap();
        sim.disk().sync_all().unwrap();
        sim.inject("open", Fault::Delay(2)).unwrap();
        let mut open = r.file_open(
            None,
            CString::new("/alias/f").unwrap(),
            libc::O_RDONLY,
            0,
            &scope,
        );
        assert!(poll(&mut open).is_pending());
        sim.disk().crash_under(Path::new(crash_root)).unwrap();
        let result = drive(&r, open);
        if crash_root == "/storage" {
            assert!(matches!(result, Err(crate::Error::Os(libc::EIO))));
        } else {
            assert!(result.is_ok());
        }
    }
}

#[test]
fn socket_families_half_close_and_renamed_listener_descendants() {
    let sim = Simulation::new();
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
fn pipe_close_atomicity_and_empty_splice() {
    let sim = Simulation::new();
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
fn scheduling_hooks_reject_sq_and_reorder_arbitrary_completions() {
    let sim = Simulation::new();
    let mut driver = Driver::new(sim.clone());
    let (fd, _peer) = sim.socket_pair();
    let fd = Rc::new(fd);
    sim.reject_submissions(1).unwrap();
    assert!(
        driver
            .push(
                1,
                Op::Poll {
                    fd: fd.clone(),
                    interest: libc::POLLIN as u32
                }
            )
            .is_err()
    );
    assert!(driver.pending.borrow().is_empty());
    driver
        .push(
            2,
            Op::Poll {
                fd,
                interest: libc::POLLIN as u32,
            },
        )
        .unwrap();
    driver.inject_completion(2, -libc::EIO).unwrap();
    driver
        .inject_completion(2 | CANCEL_BIT, -libc::ENOENT)
        .unwrap();
    driver.reorder_completions(0, 1).unwrap();
    assert_eq!(driver.pop().unwrap().0, 2 | CANCEL_BIT);
    assert_eq!(driver.pop().unwrap().0, 2);
    driver.submit();
    assert!(driver.pop().is_none());
    assert_eq!(code(driver.reorder_completions(0, 1)), libc::EINVAL);
    for _ in 0..20000 {
        sim.0.borrow_mut().record("bounded", 0, 0);
    }
    assert!(sim.trace().len() <= 16384);
}

#[test]
fn linux_file_and_pipe_differential() {
    use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
    let raw = unsafe { libc::memfd_create(c"sim-differential".as_ptr(), libc::MFD_CLOEXEC) };
    assert!(raw >= 0);
    let real = unsafe { OwnedFd::from_raw_fd(raw) };
    let sim = Simulation::new();
    let file = sim
        .open(None, Path::new("/f"), libc::O_CREAT | libc::O_RDWR)
        .unwrap()
        .into_sim()
        .unwrap();
    for (offset, bytes) in [(0, b"abc".as_slice()), (100, b""), (2, b"XY")] {
        let actual =
            unsafe { libc::pwrite(real.as_raw_fd(), bytes.as_ptr().cast(), bytes.len(), offset) };
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
fn linux_openat2_policy_differential() {
    use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
    let directory = std::fs::File::open(".").unwrap();
    let sim = Simulation::new();
    sim.mkdir(Path::new("/root")).unwrap();
    let dir = sim
        .open(None, Path::new("/root"), libc::O_DIRECTORY)
        .unwrap();
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
fn linux_socket_half_close_differential() {
    use std::io::{Read, Write};
    use std::os::fd::AsRawFd;
    let (mut a, mut b) = std::os::unix::net::UnixStream::pair().unwrap();
    let sim = Simulation::new();
    let (sa, sb) = sim.socket_pair();
    let (sa, sb) = (sa.into_sim().unwrap(), sb.into_sim().unwrap());
    a.write_all(b"x").unwrap();
    sa.send(b"x").unwrap();
    a.shutdown(std::net::Shutdown::Write).unwrap();
    sa.shutdown(libc::SHUT_WR).unwrap();
    let mut pfd = libc::pollfd {
        fd: b.as_raw_fd(),
        events: libc::POLLIN | libc::POLLRDHUP | libc::POLLOUT,
        revents: 0,
    };
    assert_eq!(unsafe { libc::poll(&mut pfd, 1, 0) }, 1);
    assert_eq!(sb.ready(pfd.events as u32).unwrap(), pfd.revents as i32);
    for _ in 0..2 {
        let mut actual = [0; 8];
        let mut expected = [0; 8];
        assert_eq!(
            sb.recv(&mut actual).unwrap(),
            b.read(&mut expected).unwrap()
        );
        assert_eq!(actual, expected);
    }
    b.write_all(b"y").unwrap();
    sb.send(b"y").unwrap();
    let mut expected = [0];
    let mut actual = [0];
    assert_eq!(
        sa.recv(&mut actual).unwrap(),
        a.read(&mut expected).unwrap()
    );
    assert_eq!(actual, expected);
}

#[test]
fn repeated_crashes_compact_history_and_reclaim_orphan_images() {
    let sim = Simulation::new();
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

#[test]
fn pending_reactor_operations_survive_unrelated_crashes_past_old_history_limit() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let r = reactor();
    let scope = scope();
    for path in ["/a/file", "/b/file", "/ab/file"] {
        sim.write_file(Path::new(path), b"old").unwrap();
    }
    sim.disk().sync_all().unwrap();
    let mut writes = Vec::new();
    let mut opens = Vec::new();
    for root in ["/a", "/b", "/ab"] {
        let fd = Rc::new(
            sim.open(None, &Path::new(root).join("file"), libc::O_RDWR)
                .unwrap(),
        );
        sim.inject("write", Fault::Delay(2)).unwrap();
        let mut write = r.write_at(fd, 0, r.file_bytes(b"new").unwrap(), (), &scope);
        assert!(poll(&mut write).is_pending());
        writes.push(write);
        sim.inject("open", Fault::Delay(2)).unwrap();
        let mut open = r.file_open(
            None,
            CString::new(format!("{root}/late")).unwrap(),
            libc::O_CREAT | libc::O_RDWR,
            0,
            &scope,
        );
        assert!(poll(&mut open).is_pending());
        opens.push(open);
    }
    for _ in 0..4100 {
        sim.disk().crash_under(Path::new("/a")).unwrap();
    }
    for (index, write) in writes.into_iter().enumerate() {
        let result = drive(&r, write);
        if index == 0 {
            assert!(matches!(result, Err(crate::Error::Os(libc::EIO))));
        } else {
            assert_eq!(result.unwrap().bytes, 3);
        }
    }
    for (index, open) in opens.into_iter().enumerate() {
        let result = drive(&r, open);
        if index == 0 {
            assert!(matches!(result, Err(crate::Error::Os(libc::EIO))));
        } else {
            assert!(result.is_ok());
        }
    }
    assert_eq!(sim.read_file(Path::new("/a/file")).unwrap(), b"old");
    assert_eq!(sim.read_file(Path::new("/b/file")).unwrap(), b"new");
    assert_eq!(sim.read_file(Path::new("/ab/file")).unwrap(), b"new");
    assert!(sim.metadata(Path::new("/a/late")).is_err());
    assert_eq!(r.in_flight(), 0);
}
