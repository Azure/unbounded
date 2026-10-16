//! Differential tests: the simulator against the host Linux kernel.
//!
//! Each test performs the same operations on real kernel objects (memfd,
//! pipes, Unix sockets, a scratch directory under `CARGO_TARGET_TMPDIR`)
//! and on the simulator, and requires identical results and errnos.
#![cfg(target_os = "linux")]

use std::ffi::CString;
use std::io::{Read, Write};
use std::net::Shutdown;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use uring_runtime::backend::{BackendConfig, Op};
use uring_runtime::{Backend, Host};
use uring_runtime_sim::{DEFAULT_ENTROPY_DOMAIN, SimBackend, SimClock, SimEntropy, SimFd, World};

fn last_errno() -> i32 {
    std::io::Error::last_os_error().raw_os_error().unwrap()
}

fn code<T: std::fmt::Debug>(result: uring_runtime::Result<T>) -> i32 {
    result.unwrap_err().raw_os_error().unwrap()
}

/// A scratch directory under the target tmpdir, removed on drop.
struct Scratch(PathBuf);

impl Scratch {
    fn new() -> Self {
        static NEXT: AtomicUsize = AtomicUsize::new(0);
        let root = Path::new(env!("CARGO_TARGET_TMPDIR")).join(format!(
            "sim-differential-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir_all(&root).unwrap();
        Self(root)
    }
}

impl Drop for Scratch {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// `openat2` on the host, returning the descriptor or errno.
fn host_openat2(
    dir: i32,
    path: &Path,
    flags: i32,
    mode: u64,
    resolve: u64,
) -> Result<OwnedFd, i32> {
    #[repr(C)]
    struct How {
        flags: u64,
        mode: u64,
        resolve: u64,
    }
    let how = How {
        flags: flags as u64,
        mode,
        resolve,
    };
    let name = CString::new(path.as_os_str().as_encoded_bytes()).unwrap();
    // SAFETY: valid dirfd, NUL-terminated path and a correctly sized `open_how`.
    let raw = unsafe {
        libc::syscall(
            libc::SYS_openat2,
            dir,
            name.as_ptr(),
            &how,
            std::mem::size_of::<How>(),
        )
    };
    if raw < 0 {
        Err(last_errno())
    } else {
        // SAFETY: the kernel returned a new descriptor we now own.
        Ok(unsafe { OwnedFd::from_raw_fd(raw as i32) })
    }
}

fn host_kind(fd: &OwnedFd) -> u32 {
    // SAFETY: all-zero is a valid `stat`, and `fd` is open.
    let mut stat: libc::stat = unsafe { std::mem::zeroed() };
    // SAFETY: `stat` is a valid output buffer.
    assert_eq!(unsafe { libc::fstat(fd.as_raw_fd(), &mut stat) }, 0);
    stat.st_mode & libc::S_IFMT
}

fn sim_kind(fd: &SimFd) -> u32 {
    u32::from(fd.stat().unwrap().stx_mode) & libc::S_IFMT
}

fn host_ready(fd: &impl AsRawFd, events: i16) -> u32 {
    let mut pfd = libc::pollfd {
        fd: fd.as_raw_fd(),
        events,
        revents: 0,
    };
    // SAFETY: one valid pollfd.
    assert!(unsafe { libc::poll(&mut pfd, 1, 0) } >= 0);
    pfd.revents as u16 as u32
}

fn memfd(name: &std::ffi::CStr) -> OwnedFd {
    // SAFETY: valid name; the result is checked.
    let raw = unsafe { libc::memfd_create(name.as_ptr(), libc::MFD_CLOEXEC) };
    assert!(raw >= 0);
    // SAFETY: a new descriptor we own.
    unsafe { OwnedFd::from_raw_fd(raw) }
}

#[test]
fn scratch_directories_are_unique_and_removed() {
    let first = Scratch::new();
    let second = std::thread::spawn(Scratch::new).join().unwrap();
    assert_ne!(first.0, second.0);
    let removed = first.0.clone();
    std::fs::write(second.0.join("keep"), b"owned").unwrap();
    drop(first);
    assert!(!removed.exists());
    assert_eq!(std::fs::read(second.0.join("keep")).unwrap(), b"owned");
}

#[test]
fn file_writes_locks_and_pipe_eof() {
    let real = memfd(c"sim-differential");
    let world = World::new();
    let file = world.open("/f", libc::O_CREAT | libc::O_RDWR).unwrap();
    for (offset, bytes) in [(0, b"abc".as_slice()), (100, b""), (2, b"XY")] {
        // SAFETY: valid descriptor and buffer.
        let actual =
            unsafe { libc::pwrite(real.as_raw_fd(), bytes.as_ptr().cast(), bytes.len(), offset) };
        assert_eq!(
            file.write_at(offset as u64, bytes).unwrap(),
            actual as usize
        );
        // SAFETY: valid descriptor and output.
        let mut stat: libc::stat = unsafe { std::mem::zeroed() };
        // SAFETY: as above.
        assert_eq!(unsafe { libc::fstat(real.as_raw_fd(), &mut stat) }, 0);
        assert_eq!(file.stat().unwrap().stx_size, stat.st_size as u64);
    }
    assert_eq!(
        // SAFETY: valid descriptor.
        unsafe { libc::flock(real.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
        0
    );
    file.lock().unwrap();
    assert_eq!(
        // SAFETY: valid descriptor.
        unsafe { libc::fcntl(real.as_raw_fd(), libc::F_SETFL, libc::O_APPEND) },
        0
    );
    let append = world.open("/f", libc::O_RDWR | libc::O_APPEND).unwrap();
    assert_eq!(
        // SAFETY: valid descriptor and buffer.
        unsafe { libc::pwrite(real.as_raw_fd(), b"Z".as_ptr().cast(), 1, 0) },
        append.write_at(0, b"Z").unwrap() as isize
    );
    let mut actual = [0u8; 8];
    // SAFETY: valid descriptor and buffer.
    let count = unsafe { libc::pread(real.as_raw_fd(), actual.as_mut_ptr().cast(), 8, 0) };
    assert_eq!(&actual[..count as usize], world.read_file("/f").unwrap());
    // Re-locking an owned lock succeeds on both.
    assert_eq!(
        // SAFETY: valid descriptor.
        unsafe { libc::flock(real.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
        0
    );
    file.lock().unwrap();

    let mut fds = [-1; 2];
    assert_eq!(
        // SAFETY: `fds` has room for two descriptors.
        unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_NONBLOCK | libc::O_CLOEXEC) },
        0
    );
    // SAFETY: new descriptors we own.
    let (reader, writer) = unsafe { (OwnedFd::from_raw_fd(fds[0]), OwnedFd::from_raw_fd(fds[1])) };
    let (sim_reader, sim_writer) = world.pipe(4096).unwrap();
    let mut byte = [0u8];
    assert_eq!(
        // SAFETY: valid descriptor and buffer.
        unsafe { libc::read(reader.as_raw_fd(), byte.as_mut_ptr().cast(), 1) },
        -1
    );
    assert_eq!(code(sim_reader.pipe_read(&mut byte)), last_errno());
    drop((writer, sim_writer));
    assert_eq!(
        // SAFETY: valid descriptor and buffer.
        unsafe { libc::read(reader.as_raw_fd(), byte.as_mut_ptr().cast(), 1) },
        sim_reader.pipe_read(&mut byte).unwrap() as isize
    );
}

#[test]
fn openat2_resolve_policies() {
    let scratch = Scratch::new();
    let directory = std::fs::File::open(&scratch.0).unwrap();
    let world = World::new();
    world.mkdir("/root").unwrap();
    let dir = world.open("/root", libc::O_DIRECTORY).unwrap();
    for (name, policy) in [
        (".", 0x01),
        (".", 0x02),
        (".", 0x04),
        (".", 0x08),
        ("/", 0x08),
        ("..", 0x08),
        ("..", 0x10),
        ("/", 0x10),
        (".", 0x18),
        (".", 0x20),
        (".", 0x40),
    ] {
        let expected = host_openat2(
            directory.as_raw_fd(),
            Path::new(name),
            libc::O_DIRECTORY,
            0,
            policy,
        )
        .err();
        let actual = world
            .open_at(Some(&dir), name, libc::O_DIRECTORY, policy)
            .err()
            .and_then(|error| error.raw_os_error());
        assert_eq!(actual, expected, "{name} {policy:#x}");
    }
    // RESOLVE_CACHED cannot create or truncate.
    let expected = host_openat2(
        directory.as_raw_fd(),
        Path::new("new"),
        libc::O_CREAT | libc::O_RDWR,
        0o600,
        0x20,
    )
    .err();
    let actual = world
        .open_at(Some(&dir), "new", libc::O_CREAT | libc::O_RDWR, 0x20)
        .err()
        .and_then(|error| error.raw_os_error());
    assert_eq!(actual, expected);
}

#[test]
fn open_access_modes_suffixes_and_absolute_dirfds() {
    let scratch = Scratch::new();
    std::fs::create_dir(scratch.0.join("dir")).unwrap();
    std::fs::write(scratch.0.join("file"), b"data").unwrap();
    let world = World::new();
    world.mkdir("/dir").unwrap();
    world.write_file("/file", b"data").unwrap();
    for (target, link) in [
        ("file", "file-link"),
        ("dir", "dir-link"),
        ("file/", "file-slash"),
        ("file/.", "file-dot"),
        ("dir/", "dir-slash"),
        ("dir/.", "dir-dot"),
    ] {
        std::os::unix::fs::symlink(target, scratch.0.join(link)).unwrap();
        world.symlink(target, Path::new("/").join(link)).unwrap();
    }
    let real_dir = std::fs::File::open(&scratch.0).unwrap();
    let real_file = std::fs::File::open(scratch.0.join("file")).unwrap();
    let sim_dir = world.open("/", libc::O_DIRECTORY).unwrap();
    let sim_file = world.open("/file", libc::O_RDONLY).unwrap();
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
                            (scratch.0.join(name), Path::new("/").join(name))
                        }
                    } else {
                        (PathBuf::from(name), PathBuf::from(name))
                    };
                    std::fs::write(scratch.0.join("file"), b"data").unwrap();
                    world.write_file("/file", b"data").unwrap();
                    let host_dir = if regular_dirfd { &real_file } else { &real_dir };
                    let expected =
                        host_openat2(host_dir.as_raw_fd(), &host_path, flags, 0, resolve);
                    let sim_dir = if regular_dirfd { &sim_file } else { &sim_dir };
                    let actual = world
                        .open_at(Some(sim_dir), &sim_path, flags, resolve)
                        .map_err(|error| error.raw_os_error().unwrap());
                    let context = format!(
                        "{name} flags={flags:#x} resolve={resolve:#x} absolute={absolute} regular_dirfd={regular_dirfd}"
                    );
                    assert_eq!(actual.as_ref().err(), expected.as_ref().err(), "{context}");
                    if let (Ok(actual), Ok(expected)) = (&actual, &expected) {
                        assert_eq!(sim_kind(actual), host_kind(expected), "{context}");
                    }
                    assert_eq!(
                        world.read_file("/file").unwrap(),
                        std::fs::read(scratch.0.join("file")).unwrap(),
                        "{context}"
                    );
                }
            }
        }
    }
}

#[test]
fn final_symlinks_and_path_descriptors() {
    let scratch = Scratch::new();
    std::fs::create_dir(scratch.0.join("real")).unwrap();
    std::fs::write(scratch.0.join("real/file"), b"data").unwrap();
    let world = World::new();
    world.write_file("/real/file", b"data").unwrap();
    for (target, link) in [
        ("real", "parent"),
        ("file", "real/link"),
        ("missing", "real/dangling"),
        ("loop", "real/loop"),
    ] {
        std::os::unix::fs::symlink(target, scratch.0.join(link)).unwrap();
        world.symlink(target, Path::new("/").join(link)).unwrap();
    }
    let real_dir = std::fs::File::open(&scratch.0).unwrap();
    let sim_dir = world.open("/", libc::O_DIRECTORY).unwrap();
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
                let mode = if flags & libc::O_CREAT != 0 { 0o600 } else { 0 };
                let expected =
                    host_openat2(real_dir.as_raw_fd(), Path::new(path), flags, mode, resolve);
                let actual = world
                    .open_at(Some(&sim_dir), path, flags, resolve)
                    .map_err(|error| error.raw_os_error().unwrap());
                let context = format!("{path} flags={flags:#x} resolve={resolve:#x}");
                assert_eq!(actual.as_ref().err(), expected.as_ref().err(), "{context}");
                let (Ok(actual), Ok(expected)) = (actual, expected) else {
                    continue;
                };
                assert_eq!(sim_kind(&actual), host_kind(&expected), "{context}");
                if flags & libc::O_PATH != 0 {
                    let mut bytes = [0u8; 4];
                    // SAFETY: valid descriptor and buffer.
                    let read = unsafe {
                        libc::pread(expected.as_raw_fd(), bytes.as_mut_ptr().cast(), 4, 0)
                    };
                    assert_eq!(read, -1);
                    assert_eq!(code(actual.read_at(0, &mut bytes)), last_errno());
                    // SAFETY: valid descriptor.
                    assert_eq!(unsafe { libc::fsync(expected.as_raw_fd()) }, -1);
                    assert_eq!(code(actual.sync()), last_errno());
                    assert_eq!(host_ready(&expected, libc::POLLIN), libc::POLLNVAL as u32);
                }
            }
        }
    }
    assert_eq!(world.read_file("/real/file").unwrap(), b"data");
}

#[test]
fn unix_stream_half_close() {
    let (mut a, mut b) = UnixStream::pair().unwrap();
    let world = World::new();
    let (sa, sb) = world.socket_pair().unwrap();
    a.write_all(b"x").unwrap();
    sa.send(b"x").unwrap();
    a.shutdown(Shutdown::Write).unwrap();
    sa.shutdown(Shutdown::Write).unwrap();
    let events = libc::POLLIN | libc::POLLRDHUP | libc::POLLOUT;
    assert_eq!(sb.poll(events as u32).unwrap(), host_ready(&b, events));
    for _ in 0..2 {
        let mut actual = [0u8; 8];
        let mut expected = [0u8; 8];
        assert_eq!(
            sb.recv(&mut actual, false).unwrap(),
            b.read(&mut expected).unwrap()
        );
        assert_eq!(actual, expected);
    }
    b.write_all(b"y").unwrap();
    sb.send(b"y").unwrap();
    let (mut expected, mut actual) = ([0u8], [0u8]);
    assert_eq!(
        sa.recv(&mut actual, false).unwrap(),
        a.read(&mut expected).unwrap()
    );
    assert_eq!(actual, expected);
}

#[test]
fn regular_file_readiness() {
    let file = memfd(c"sim-poll-differential");
    let path = CString::new(format!("/proc/self/fd/{}", file.as_raw_fd())).unwrap();
    let world = World::new();
    world.write_file("/poll-file", b"").unwrap();
    let backend = SimBackend::new(
        &world,
        SimClock::new(0),
        SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 0, 0),
    );
    for flags in [libc::O_RDONLY, libc::O_WRONLY, libc::O_RDWR, libc::O_PATH] {
        // SAFETY: valid path; the result is checked and owned.
        let raw = unsafe { libc::open(path.as_ptr(), flags | libc::O_CLOEXEC) };
        assert!(raw >= 0);
        // SAFETY: a new descriptor we own.
        let real = unsafe { OwnedFd::from_raw_fd(raw) };
        let sim = world.open("/poll-file", flags).unwrap();
        for events in [libc::POLLIN, libc::POLLOUT, libc::POLLIN | libc::POLLOUT] {
            let expected = host_ready(&real, events);
            assert_eq!(
                backend.poll_now(&sim, events).unwrap() as u16 as u32,
                expected
            );
            if flags == libc::O_PATH {
                assert_eq!(expected, libc::POLLNVAL as u32);
                // io_uring reports EBADF for an O_PATH poll.
                assert_eq!(code(sim.poll(events as u32)), libc::EBADF);
            } else {
                assert_eq!(sim.poll(events as u32).unwrap(), expected);
            }
        }
    }
}

#[test]
fn shutdown_matrix_and_outstanding_polls() {
    let all = libc::POLLIN | libc::POLLOUT | libc::POLLRDHUP;
    for full in [false, true] {
        for how in [Shutdown::Read, Shutdown::Write, Shutdown::Both] {
            for endpoint in 0..2 {
                for events in [libc::POLLIN, libc::POLLOUT, libc::POLLRDHUP, all] {
                    let (a, b) = UnixStream::pair().unwrap();
                    let real = [a, b];
                    for fd in &real {
                        fd.set_nonblocking(true).unwrap();
                    }
                    let world = World::new();
                    let mut backend = SimBackend::new(
                        &world,
                        SimClock::new(0),
                        SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 0, 0),
                    );
                    backend.init(&BackendConfig::new(8)).unwrap();
                    world.set_stream_capacity(1).unwrap();
                    let (sa, sb) = world.socket_pair().unwrap();
                    let sim = [sa, sb];
                    if full {
                        for fd in &real {
                            let bytes = [0u8; 4096];
                            loop {
                                // SAFETY: valid descriptor and buffer.
                                let n = unsafe {
                                    libc::send(
                                        fd.as_raw_fd(),
                                        bytes.as_ptr().cast(),
                                        bytes.len(),
                                        libc::MSG_NOSIGNAL,
                                    )
                                };
                                if n < 0 {
                                    assert_eq!(last_errno(), libc::EAGAIN);
                                    break;
                                }
                            }
                        }
                        for fd in &sim {
                            fd.send(b"x").unwrap();
                        }
                    }
                    let before = host_ready(&real[endpoint], events);
                    assert_eq!(sim[endpoint].poll(events as u32).unwrap(), before);
                    let poll = Op::PollAdd {
                        fd: &sim[endpoint],
                        events: events as u32,
                    };
                    // SAFETY: `sim` outlives every completion popped below.
                    unsafe { backend.push(1, &poll) }.unwrap();
                    if before == 0 {
                        backend.submit().unwrap();
                        assert!(backend.pop().is_none());
                    }
                    let raw_how = match how {
                        Shutdown::Read => libc::SHUT_RD,
                        Shutdown::Write => libc::SHUT_WR,
                        Shutdown::Both => libc::SHUT_RDWR,
                    };
                    // SAFETY: valid socket.
                    assert_eq!(unsafe { libc::shutdown(real[1].as_raw_fd(), raw_how) }, 0);
                    sim[1].shutdown(how).unwrap();
                    let expected = host_ready(&real[endpoint], events);
                    let context =
                        format!("full={full} how={how:?} endpoint={endpoint} events={events:#x}");
                    assert_eq!(
                        sim[endpoint].poll(events as u32).unwrap(),
                        expected,
                        "{context}"
                    );
                    backend.submit().unwrap();
                    backend.wait(Duration::ZERO).unwrap();
                    match backend.pop() {
                        Some(entry) => assert_eq!(entry.result as u32, expected, "{context}"),
                        None => {
                            assert_eq!(expected, 0, "{context}");
                            backend.cancel(2, 1).unwrap();
                            assert_eq!(backend.pop().unwrap().result, 0);
                            assert_eq!(backend.pop().unwrap().result, -libc::ECANCELED);
                        }
                    }
                    let terminal = if endpoint == 0 {
                        how != Shutdown::Write
                    } else {
                        how != Shutdown::Read
                    };
                    if terminal {
                        // SAFETY: valid descriptor and buffer.
                        let n = unsafe {
                            libc::send(
                                real[endpoint].as_raw_fd(),
                                b"x".as_ptr().cast(),
                                1,
                                libc::MSG_NOSIGNAL,
                            )
                        };
                        assert_eq!(n, -1);
                        assert_eq!(last_errno(), libc::EPIPE);
                        assert_eq!(code(sim[endpoint].send(b"x")), libc::EPIPE);
                    }
                    assert_eq!(backend.in_flight(), 0);
                }
            }
        }
    }
}
