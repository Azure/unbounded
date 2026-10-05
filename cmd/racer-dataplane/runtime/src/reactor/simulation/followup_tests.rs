use super::io_tests::{drive, poll, reactor, scope};
use super::*;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};

// Keep differential fixtures inside the worktree and remove them even on panic.
struct Directory(PathBuf);
impl Directory {
    fn new() -> Self {
        let root = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../target")
            .join(format!("sim-open-differential-{}", std::process::id()));
        std::fs::create_dir(&root).unwrap();
        Self(root)
    }
}
impl Drop for Directory {
    fn drop(&mut self) {
        std::fs::remove_dir_all(&self.0).unwrap();
    }
}

#[test]
fn linux_final_symlink_and_path_descriptor_differential() {
    let directory = Directory::new();
    std::fs::create_dir(directory.0.join("real")).unwrap();
    std::fs::write(directory.0.join("real/file"), b"data").unwrap();
    let sim = Simulation::new();
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
                                libc::pread(expected.as_raw_fd(), bytes.as_mut_ptr().cast(), 4, 0)
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
fn actual_reactor_path_descriptors_stat_but_reject_io() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let r = reactor();
    let scope = scope();
    sim.write_file(Path::new("/real/file"), b"data").unwrap();
    sim.symlink(Path::new("real"), Path::new("/parent"))
        .unwrap();
    sim.symlink(Path::new("file"), Path::new("/real/link"))
        .unwrap();
    for (name, flags, mode) in [
        ("/parent/file", libc::O_PATH, libc::S_IFREG),
        (
            "/parent/link",
            libc::O_PATH | libc::O_NOFOLLOW,
            libc::S_IFLNK,
        ),
    ] {
        let fd = drive(
            &r,
            r.file_open(None, CString::new(name).unwrap(), flags, 0, &scope),
        )
        .unwrap();
        assert_eq!(
            drive(&r, r.file_stat(fd.clone(), &scope)).unwrap().stx_mode as u32 & libc::S_IFMT,
            mode
        );
        assert!(matches!(
            drive(
                &r,
                r.read_at(fd.clone(), 0, r.file_buffer(4).unwrap(), (), &scope)
            ),
            Err(crate::Error::Os(libc::EBADF))
        ));
        assert!(matches!(
            drive(
                &r,
                r.write_at(fd.clone(), 0, r.file_bytes(b"bad").unwrap(), (), &scope)
            ),
            Err(crate::Error::Os(libc::EBADF))
        ));
        assert_eq!(
            drive(&r, r.file_sync(fd, &scope)),
            Err(crate::Error::Os(libc::EBADF))
        );
    }
    assert_eq!(r.in_flight(), 0);
}

fn linux_ready(fd: &impl AsRawFd, interest: i16) -> i32 {
    let mut pfd = libc::pollfd {
        fd: fd.as_raw_fd(),
        events: interest,
        revents: 0,
    };
    assert!(unsafe { libc::poll(&mut pfd, 1, 0) } >= 0);
    pfd.revents as i32
}

fn simulated_ready(fd: &Descriptor, interest: i16) -> i32 {
    match fd.as_sim().unwrap().ready(interest as u32) {
        Ok(flags) => flags,
        Err(error) => {
            assert_eq!(error.raw_os_error(), Some(libc::EAGAIN));
            0
        }
    }
}

#[test]
fn regular_file_readiness_matches_linux_poll_and_uring() {
    let Some(host) = super::super::tests::kernel_reactor(8) else {
        return;
    };
    let raw = unsafe { libc::memfd_create(c"sim-poll-differential".as_ptr(), libc::MFD_CLOEXEC) };
    assert!(raw >= 0);
    let file = unsafe { OwnedFd::from_raw_fd(raw) };
    let path = CString::new(format!("/proc/self/fd/{}", file.as_raw_fd())).unwrap();
    let sim = Simulation::new();
    sim.write_file(Path::new("/poll-file"), b"").unwrap();
    let _environment = sim.enter();
    let simulated_reactor = reactor();
    let request = scope();
    for flags in [libc::O_RDONLY, libc::O_WRONLY, libc::O_RDWR, libc::O_PATH] {
        let raw = unsafe { libc::open(path.as_ptr(), flags | libc::O_CLOEXEC) };
        assert!(raw >= 0);
        let real = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
        let simulated = Rc::new(sim.open(None, Path::new("/poll-file"), flags).unwrap());
        for interest in [libc::POLLIN, libc::POLLOUT, libc::POLLIN | libc::POLLOUT] {
            let host_result = drive(
                &host,
                host.readiness(real.clone(), interest as u32, &request),
            );
            let sim_result = drive(
                &simulated_reactor,
                simulated_reactor.readiness(simulated.clone(), interest as u32, &request),
            );
            assert_eq!(
                sim_result, host_result,
                "flags={flags:#x} interest={interest:#x}"
            );
            if flags == libc::O_PATH {
                assert_eq!(linux_ready(&*real, interest), libc::POLLNVAL as i32);
                assert_eq!(host_result, Err(crate::Error::Os(libc::EBADF)));
            } else {
                assert_eq!(host_result, Ok(linux_ready(&*real, interest) as u32));
                assert_eq!(simulated_ready(&simulated, interest), interest as i32);
            }
        }
    }
}

#[test]
fn linux_shutdown_matrix_and_outstanding_reactor_polls() {
    let all = libc::POLLIN | libc::POLLOUT | libc::POLLRDHUP;
    for full in [false, true] {
        for how in [libc::SHUT_RD, libc::SHUT_WR, libc::SHUT_RDWR] {
            for endpoint in 0..2 {
                for interest in [libc::POLLIN, libc::POLLOUT, libc::POLLRDHUP, all] {
                    let (a, b) = std::os::unix::net::UnixStream::pair().unwrap();
                    let real = [a, b];
                    for fd in &real {
                        fd.set_nonblocking(true).unwrap();
                    }
                    let sim = Simulation::new();
                    let _environment = sim.enter();
                    let r = reactor();
                    let scope = scope();
                    sim.set_stream_capacity(1).unwrap();
                    let (sa, sb) = sim.socket_pair();
                    let simulated = [Rc::new(sa), Rc::new(sb)];
                    if full {
                        for fd in &real {
                            let bytes = [0; 4096];
                            loop {
                                let count = unsafe {
                                    libc::send(
                                        fd.as_raw_fd(),
                                        bytes.as_ptr().cast(),
                                        bytes.len(),
                                        libc::MSG_NOSIGNAL,
                                    )
                                };
                                if count < 0 {
                                    assert_eq!(
                                        io::Error::last_os_error().raw_os_error(),
                                        Some(libc::EAGAIN)
                                    );
                                    break;
                                }
                                assert!(count > 0);
                            }
                        }
                        for fd in &simulated {
                            fd.try_send(b"x").unwrap();
                        }
                    }
                    let before = linux_ready(&real[endpoint], interest);
                    assert_eq!(simulated_ready(&simulated[endpoint], interest), before);
                    let mut pending =
                        r.readiness(simulated[endpoint].clone(), interest as u32, &scope);
                    assert!(poll(&mut pending).is_pending());
                    // Only execute pre-shutdown if this interest is not ready;
                    // otherwise submit it and let shutdown precede execution.
                    if before == 0 {
                        r.poll_budgeted(8).unwrap();
                        assert!(poll(&mut pending).is_pending());
                    }
                    assert_eq!(unsafe { libc::shutdown(real[1].as_raw_fd(), how) }, 0);
                    simulated[1].as_sim().unwrap().shutdown(how).unwrap();
                    let expected = linux_ready(&real[endpoint], interest);
                    // Preserve Linux's limitation: a full socket can remain
                    // non-writable despite a subsequent send returning EPIPE.
                    let terminal = if endpoint == 0 {
                        how != libc::SHUT_WR
                    } else {
                        how != libc::SHUT_RD
                    };
                    assert_eq!(
                        simulated_ready(&simulated[endpoint], interest),
                        expected,
                        "full={full} how={how} endpoint={endpoint} interest={interest:#x}"
                    );
                    if expected == 0 {
                        r.poll_budgeted(8).unwrap();
                        assert!(poll(&mut pending).is_pending());
                        drop(pending);
                        drive(&r, r.drain()).unwrap();
                    } else {
                        assert_eq!(drive(&r, pending).unwrap(), expected as u32);
                    }
                    if terminal {
                        assert_eq!(
                            unsafe {
                                libc::send(
                                    real[endpoint].as_raw_fd(),
                                    b"x".as_ptr().cast(),
                                    1,
                                    libc::MSG_NOSIGNAL,
                                )
                            },
                            -1
                        );
                        assert_eq!(io::Error::last_os_error().raw_os_error(), Some(libc::EPIPE));
                        assert_eq!(
                            simulated[endpoint]
                                .try_send(b"x")
                                .unwrap_err()
                                .raw_os_error(),
                            Some(libc::EPIPE)
                        );
                    }
                    assert_eq!(r.in_flight(), 0);
                }
            }
        }
    }
}

#[test]
fn actual_reactor_arbitrary_completion_permutations_keep_both_fences() {
    use super::super::tests::fixtures::ResourceClass;
    // Exhaust all 4! interleavings of two original/cancel pairs, not just
    // globally cancel-first versus globally original-first schedules.
    for a in 0..4 {
        for b in 0..4 {
            for c in 0..4 {
                for d in 0..4 {
                    let order = [a, b, c, d];
                    if order.iter().copied().collect::<BTreeSet<_>>().len() != 4 {
                        continue;
                    }
                    let sim = Simulation::new();
                    let _environment = sim.enter();
                    let r = reactor();
                    r.init().unwrap();
                    let scope = scope();
                    let baseline = r.admission.used(ResourceClass::RequestContext);
                    let mut weak_fds = Vec::new();
                    let mut weak_leases = Vec::new();
                    let mut peers = Vec::new();
                    for _ in 0..2 {
                        let (fd, peer) = sim.socket_pair();
                        peers.push(peer);
                        let fd = Rc::new(fd);
                        weak_fds.push(Rc::downgrade(&fd));
                        let lease = Rc::new(());
                        weak_leases.push(Rc::downgrade(&lease));
                        let mut recv = r.recv(fd, r.file_buffer(8).unwrap(), lease, &scope);
                        assert!(poll(&mut recv).is_pending());
                        drop(recv);
                    }
                    let ids: Vec<_> = r.state.borrow().entries.keys().map(|id| id.0).collect();
                    // Actual cancellation scan queues both completion pairs.
                    assert_eq!(r.poll_budgeted(2).unwrap(), 0);
                    let original;
                    {
                        let mut state = r.state.borrow_mut();
                        let driver = state.simulation.as_mut().unwrap();
                        original = driver
                            .completed
                            .borrow()
                            .iter()
                            .map(|(id, _)| *id)
                            .collect::<Vec<_>>();
                        assert_eq!(original.len(), 4);
                        let mut current = [0, 1, 2, 3];
                        for (index, wanted) in order.iter().enumerate() {
                            let other = current.iter().position(|id| id == wanted).unwrap();
                            driver.reorder_completions(index, other).unwrap();
                            current.swap(index, other);
                        }
                    }
                    let mut seen = BTreeSet::new();
                    for index in order {
                        assert_eq!(r.poll_budgeted(1).unwrap(), 1);
                        seen.insert(original[index]);
                        for (slot, id) in ids.iter().enumerate() {
                            let fenced = seen.contains(id) && seen.contains(&(id | CANCEL_BIT));
                            assert_eq!(
                                weak_fds[slot].upgrade().is_none(),
                                fenced,
                                "order={order:?}"
                            );
                            assert_eq!(
                                weak_leases[slot].upgrade().is_none(),
                                fenced,
                                "order={order:?}"
                            );
                        }
                    }
                    assert_eq!(r.in_flight(), 0);
                    assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
                    assert_eq!(sim.live_handles(), 2);
                    drop(peers);
                    assert_eq!(sim.live_handles(), 0);
                }
            }
        }
    }
}

#[test]
fn actual_reactor_injected_completion_retires_borrowed_pointer() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let r = reactor();
    let scope = scope();
    let (fd, peer) = sim.socket_pair();
    let fd = Rc::new(fd);
    let weak = Rc::downgrade(&fd);
    let mut recv = r.recv(fd, r.file_buffer(8).unwrap(), (), &scope);
    assert!(poll(&mut recv).is_pending());
    let id = r.state.borrow().entries.keys().next().unwrap().0;
    r.state
        .borrow_mut()
        .simulation
        .as_mut()
        .unwrap()
        .inject_completion(id, -libc::EIO)
        .unwrap();
    assert!(weak.upgrade().is_some());
    assert!(matches!(drive(&r, recv), Err(crate::Error::Os(libc::EIO))));
    assert!(weak.upgrade().is_none());
    // A subsequent submit must never execute the retired buffer pointer.
    r.poll_budgeted(8).unwrap();
    assert!(
        r.state
            .borrow()
            .simulation
            .as_ref()
            .unwrap()
            .pending
            .borrow()
            .is_empty()
    );
    drop(peer);
    assert_eq!(sim.live_handles(), 0);
}

#[test]
fn bounded_simulation_setters_validate_atomically() {
    let sim = Simulation::new();
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
    // max_chunk is a transfer cap, not an allocation, so usize::MAX is valid.
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

#[test]
fn unix_address_restrictions_match_shared_encoder_without_disk_side_effects() {
    use std::os::unix::ffi::OsStrExt;
    let sim = Simulation::new();
    let _environment = sim.enter();
    let r = reactor();
    let scope = scope();
    let fd = Rc::new(sim.socket(libc::AF_UNIX).unwrap());
    for bytes in [
        b"".as_slice(),
        b"\0abstract",
        b"/embedded\0nul",
        &[b'x'; 108],
    ] {
        let path = PathBuf::from(std::ffi::OsStr::from_bytes(bytes));
        let address = SocketAddress::Unix(path.clone());
        assert!(super::super::encode_address(address.clone()).is_err());
        assert_eq!(
            sim.listen(address.clone()).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(
            sim.connect(address.clone()).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(
            fd.as_sim()
                .unwrap()
                .connect(&address)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(
            drive(&r, r.connect(fd.clone(), address, &scope)),
            Err(crate::Error::InvalidInput)
        );
        assert!(!sim.0.borrow().paths.contains_key(&path));
        assert!(sim.0.borrow().listeners.is_empty());
        assert_eq!(sim.live_handles(), 1);
        assert_eq!(r.in_flight(), 0);
    }
}
