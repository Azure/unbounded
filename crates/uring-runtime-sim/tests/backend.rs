//! Drive `SimBackend` directly through the `Backend` and `Host` traits.

use std::collections::BTreeMap;
use std::ffi::CString;
use std::net::Shutdown;
use std::time::Duration;

use uring_runtime::backend::{
    BackendConfig, CompletionEntry, KernelTimespec, Op, OpKind, OpenHow, PushError,
};
use uring_runtime::{Backend, Clock, Entropy, Host, SockAddr, SocketAddress};
use uring_runtime_sim::{
    DEFAULT_ENTROPY_DOMAIN, Event, EventKind, Fault, FaultTarget, SimBackend, SimClock, SimEntropy,
    SimFd, World,
};

fn backend(world: &World, clock: &SimClock) -> SimBackend {
    let mut backend = SimBackend::new(
        world,
        clock.clone(),
        SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, clock.seed(), 0),
    );
    backend.init(&BackendConfig::new(64)).unwrap();
    backend
}

fn setup() -> (World, SimClock, SimBackend) {
    let world = World::new();
    let clock = SimClock::new(1);
    let backend = backend(&world, &clock);
    (world, clock, backend)
}

/// Run turns until every listed `user_data` has completed; return results.
fn drive(backend: &mut SimBackend, wanted: &[u64]) -> BTreeMap<u64, i32> {
    let mut results = BTreeMap::new();
    for _ in 0..100 {
        backend.submit().unwrap();
        while let Some(entry) = backend.pop() {
            results.insert(entry.user_data, entry.result);
        }
        if wanted.iter().all(|ud| results.contains_key(ud)) {
            return results;
        }
    }
    panic!("simulation did not progress: {results:?}");
}

fn run(backend: &mut SimBackend, user_data: u64) -> i32 {
    drive(backend, &[user_data])[&user_data]
}

fn turns(backend: &mut SimBackend, count: usize) -> Vec<CompletionEntry> {
    let mut out = Vec::new();
    for _ in 0..count {
        backend.submit().unwrap();
        while let Some(entry) = backend.pop() {
            out.push(entry);
        }
    }
    out
}

fn read_op<'a>(fd: &'a SimFd, offset: u64, buf: &'a mut [u8]) -> Op<'a, SimFd> {
    Op::Read {
        fd,
        offset,
        buf: buf.as_mut_ptr(),
        len: buf.len() as u32,
    }
}

fn write_op<'a>(fd: &'a SimFd, offset: u64, buf: &'a [u8]) -> Op<'a, SimFd> {
    Op::Write {
        fd,
        offset,
        buf: buf.as_ptr(),
        len: buf.len() as u32,
    }
}

fn push(backend: &mut SimBackend, user_data: u64, op: &Op<'_, SimFd>) {
    // SAFETY: every test keeps the op's buffers and descriptors alive until
    // the completion for `user_data` has been popped.
    unsafe { backend.push(user_data, op) }.unwrap();
}

fn open_op<'a>(path: &'a CString, how: &'a OpenHow) -> Op<'a, SimFd> {
    Op::OpenAt2 {
        dir: None,
        path,
        how,
    }
}

fn how(flags: i32, mode: u64) -> OpenHow {
    OpenHow {
        flags: flags as u64,
        mode,
        resolve: 0,
    }
}

#[test]
fn read_write_files_with_positions_and_short_faults() {
    let (world, _, mut backend) = setup();
    world.write_file("/f", b"0123456789").unwrap();
    let fd = world.open("/f", libc::O_RDWR).unwrap();
    world
        .inject(FaultTarget::Op(OpKind::Read), Fault::Short(3))
        .unwrap();
    let mut buf = [0u8; 8];
    push(&mut backend, 1, &read_op(&fd, u64::MAX, &mut buf));
    assert_eq!(run(&mut backend, 1), 3);
    push(&mut backend, 2, &read_op(&fd, u64::MAX, &mut buf[3..]));
    assert_eq!(run(&mut backend, 2), 5);
    assert_eq!(&buf, b"01234567");
    push(&mut backend, 3, &write_op(&fd, 20, b"xy"));
    assert_eq!(run(&mut backend, 3), 2);
    assert_eq!(world.metadata("/f").unwrap().len, 22);
    // Synchronous faults are not consumed by asynchronous operations.
    world
        .inject(
            FaultTarget::Sync(uring_runtime_sim::SyncOp::Write),
            Fault::Errno(libc::EIO),
        )
        .unwrap();
    push(&mut backend, 4, &write_op(&fd, 0, b"z"));
    assert_eq!(run(&mut backend, 4), 1);
    assert_eq!(world.pending_faults(), 1);
}

#[test]
fn errno_delay_and_hold_faults() {
    let (world, _, mut backend) = setup();
    world.write_file("/f", b"abc").unwrap();
    let fd = world.open("/f", libc::O_RDONLY).unwrap();
    let mut buf = [0u8; 3];
    world
        .inject(FaultTarget::Op(OpKind::Read), Fault::Errno(libc::EIO))
        .unwrap();
    push(&mut backend, 1, &read_op(&fd, 0, &mut buf));
    assert_eq!(run(&mut backend, 1), -libc::EIO);

    world.inject(FaultTarget::Any, Fault::Delay(2)).unwrap();
    push(&mut backend, 2, &read_op(&fd, 0, &mut buf));
    assert!(turns(&mut backend, 2).is_empty());
    assert_eq!(turns(&mut backend, 1)[0].result, 3);

    // A held completion has executed (bytes are visible) but is not yet
    // reported, and can no longer be canceled.
    let (a, b) = world.socket_pair().unwrap();
    world
        .inject(FaultTarget::Op(OpKind::Send), Fault::HoldCompletion(2))
        .unwrap();
    let payload = *b"held";
    let send = Op::Send {
        fd: &a,
        buf: payload.as_ptr(),
        len: 4,
        flags: 0,
    };
    push(&mut backend, 3, &send);
    assert!(turns(&mut backend, 1).is_empty());
    let mut received = [0u8; 4];
    assert_eq!(b.recv(&mut received, false).unwrap(), 4);
    backend.cancel(4, 3).unwrap();
    assert_eq!(
        backend.pop(),
        Some(CompletionEntry {
            user_data: 4,
            result: -libc::ENOENT,
            flags: 0
        })
    );
    assert!(turns(&mut backend, 2).is_empty());
    assert_eq!(turns(&mut backend, 1)[0].result, 4);
    assert_eq!(backend.in_flight(), 0);
}

#[test]
fn cancel_orders_and_fences() {
    for cancel_first in [true, false] {
        let (world, _, mut backend) = setup();
        world.set_cancel_first(cancel_first);
        let (fd, _peer) = world.socket_pair().unwrap();
        let mut buf = [0u8; 8];
        let recv = Op::Recv {
            fd: &fd,
            buf: buf.as_mut_ptr(),
            len: 8,
            flags: 0,
        };
        push(&mut backend, 1, &recv);
        assert!(turns(&mut backend, 3).is_empty(), "recv waits for data");
        backend.cancel(2, 1).unwrap();
        let first = backend.pop().unwrap();
        let second = backend.pop().unwrap();
        let (cancel, target) = if cancel_first {
            (first, second)
        } else {
            (second, first)
        };
        assert_eq!((cancel.user_data, cancel.result), (2, 0));
        assert_eq!((target.user_data, target.result), (1, -libc::ECANCELED));
        backend.cancel(3, 99).unwrap();
        assert_eq!(backend.pop().unwrap().result, -libc::ENOENT);
        world
            .inject(FaultTarget::Op(OpKind::Cancel), Fault::Errno(libc::EINVAL))
            .unwrap();
        backend.cancel(4, 99).unwrap();
        assert_eq!(backend.pop().unwrap().result, -libc::EINVAL);
        assert!(
            world
                .trace()
                .iter()
                .any(|e| e.kind == EventKind::Cancel && e.result == 1)
        );
    }
}

#[test]
fn scheduling_hooks_reject_inject_and_reorder() {
    let (world, _, mut backend) = setup();
    let (fd, _peer) = world.socket_pair().unwrap();
    world.reject_submissions(1).unwrap();
    let poll = Op::PollAdd {
        fd: &fd,
        events: libc::POLLIN as u32,
    };
    // SAFETY: `poll` references only `fd`, alive for the whole test.
    assert_eq!(unsafe { backend.push(1, &poll) }, Err(PushError::Full));
    assert_eq!(backend.in_flight(), 0);
    push(&mut backend, 2, &poll);
    backend.inject_completion(2, -libc::EIO).unwrap();
    backend.inject_completion(3, -libc::ENOENT).unwrap();
    assert_eq!(backend.in_flight(), 0, "injection retires the operation");
    backend.reorder_completions(0, 1).unwrap();
    assert_eq!(
        backend
            .queued_completions()
            .iter()
            .map(|c| c.user_data)
            .collect::<Vec<_>>(),
        [3, 2]
    );
    assert_eq!(backend.pop().unwrap().user_data, 3);
    assert_eq!(backend.pop().unwrap().user_data, 2);
    assert!(turns(&mut backend, 2).is_empty());
    assert_eq!(
        backend
            .reorder_completions(0, 1)
            .unwrap_err()
            .raw_os_error(),
        Some(libc::EINVAL)
    );
    assert!(
        world
            .trace()
            .iter()
            .any(|e| e.kind == EventKind::Reject(OpKind::PollAdd))
    );
}

#[test]
fn capacity_limits_unsubmitted_pushes() {
    let world = World::new();
    let clock = SimClock::new(0);
    let mut backend = SimBackend::new(&world, clock, SimEntropy::new(b"x", 0, 0));
    assert!(backend.init(&BackendConfig::new(0)).is_err());
    backend.init(&BackendConfig::new(2)).unwrap();
    push(&mut backend, 1, &Op::Nop);
    push(&mut backend, 2, &Op::Nop);
    // SAFETY: `Nop` references no memory.
    assert_eq!(unsafe { backend.push(3, &Op::Nop) }, Err(PushError::Full));
    assert_eq!(backend.submit().unwrap(), 2);
    push(&mut backend, 3, &Op::Nop);
    assert_eq!(drive(&mut backend, &[1, 2, 3]).len(), 3);
    assert!(OpKind::ALL.iter().all(|kind| backend.supports(*kind)));
}

#[test]
fn timeouts_fire_on_the_sim_clock_and_auto_advance() {
    let (_, clock, mut backend) = setup();
    let mut spec = KernelTimespec::default();
    let deadline = clock.now() + Duration::from_secs(10);
    let timeout = Op::Timeout {
        deadline,
        timespec: &mut spec,
    };
    push(&mut backend, 1, &timeout);
    assert!(turns(&mut backend, 3).is_empty());
    backend.wait(Duration::from_secs(1)).unwrap();
    assert!(
        backend.pop().is_none(),
        "wait does not move time by default"
    );
    clock.advance(Duration::from_secs(10));
    assert_eq!(backend.pop().unwrap().result, -libc::ETIME);

    backend.set_auto_advance(true);
    assert!(backend.auto_advance());
    let start = clock.now();
    let timeout = Op::Timeout {
        deadline: start + Duration::from_secs(5),
        timespec: &mut spec,
    };
    push(&mut backend, 2, &timeout);
    backend.submit().unwrap();
    backend.wait(Duration::from_secs(2)).unwrap();
    assert!(backend.pop().is_none());
    assert_eq!(clock.now() - start, Duration::from_secs(2));
    backend.wait(Duration::from_secs(60)).unwrap();
    assert_eq!(backend.pop().unwrap().result, -libc::ETIME);
    assert_eq!(clock.now() - start, Duration::from_secs(5));
}

#[test]
fn accept_connect_and_adopt_through_host() {
    let world = World::new();
    let clock = SimClock::new(3);
    let mut server = backend(&world, &clock);
    let mut client = backend(&world, &clock);
    let address = SocketAddress::Inet("127.0.0.1:7000".parse().unwrap());
    server.set_endpoint(Some(address.clone()));
    let listener = server
        .socket(libc::AF_INET, libc::SOCK_STREAM | libc::SOCK_NONBLOCK, 0)
        .unwrap();
    server.bind(&listener, &address).unwrap();
    server.listen(&listener, 16).unwrap();
    assert_eq!(server.local_addr(&listener).unwrap(), address);

    let mut peer = SockAddr::empty();
    let accept = Op::Accept {
        fd: &listener,
        peer: &mut peer,
        flags: libc::SOCK_CLOEXEC,
    };
    push(&mut server, 1, &accept);
    assert!(turns(&mut server, 2).is_empty());

    let socket = client.socket(libc::AF_INET, libc::SOCK_STREAM, 0).unwrap();
    let encoded = SockAddr::encode(&address).unwrap();
    let connect = Op::Connect {
        fd: &socket,
        address: &encoded,
    };
    push(&mut client, 1, &connect);
    assert_eq!(run(&mut client, 1), 0);
    let raw = run(&mut server, 1);
    assert!(raw > 0);
    let accepted = server.adopt_fd(raw).unwrap();
    assert!(server.adopt_fd(raw).is_err());
    assert_eq!(peer.decode().unwrap(), client.local_addr(&socket).unwrap());
    assert_eq!(server.peer_addr(&accepted).unwrap(), peer.decode().unwrap());

    server.set_nodelay(&accepted, true).unwrap();
    assert_eq!(client.try_send(&socket, b"ping", 0).unwrap(), 4);
    let mut buf = [0u8; 8];
    assert_eq!(
        server
            .try_recv(&accepted, &mut buf, libc::MSG_PEEK)
            .unwrap(),
        4
    );
    assert_eq!(server.try_recv(&accepted, &mut buf, 0).unwrap(), 4);
    assert_eq!(
        server
            .poll_now(&accepted, libc::POLLIN | libc::POLLOUT)
            .unwrap(),
        libc::POLLOUT
    );
    client.shutdown(&socket, Shutdown::Write).unwrap();
    assert_eq!(server.try_recv(&accepted, &mut buf, 0).unwrap(), 0);

    // Descriptors from another world are rejected.
    let other = World::new();
    let (foreign, _) = other.socket_pair().unwrap();
    assert_eq!(
        server
            .try_send(&foreign, b"x", 0)
            .unwrap_err()
            .raw_os_error(),
        Some(libc::EXDEV)
    );
    let mut buf = [0u8; 1];
    push(&mut server, 2, &read_op(&foreign, 0, &mut buf));
    assert_eq!(run(&mut server, 2), -libc::EXDEV);
}

#[test]
fn partitions_stall_pending_operations_until_heal() {
    let world = World::new();
    let clock = SimClock::new(4);
    let a = SocketAddress::Inet("127.0.0.1:101".parse().unwrap());
    let b = SocketAddress::Inet("127.0.0.1:102".parse().unwrap());
    let mut node_a = backend(&world, &clock);
    node_a.set_endpoint(Some(a.clone()));
    let listener = world.listen(b.clone()).unwrap();
    let client = node_a.connect_to(b.clone()).unwrap();
    let server = listener.accept().unwrap();
    world.partition(&a, &b);
    let payload = *b"after";
    let send = Op::Send {
        fd: &client,
        buf: payload.as_ptr(),
        len: 5,
        flags: 0,
    };
    push(&mut node_a, 1, &send);
    let fresh = node_a.socket(libc::AF_INET, libc::SOCK_STREAM, 0).unwrap();
    let encoded = SockAddr::encode(&b).unwrap();
    let connect = Op::Connect {
        fd: &fresh,
        address: &encoded,
    };
    push(&mut node_a, 2, &connect);
    let dontwait = Op::Send {
        fd: &client,
        buf: payload.as_ptr(),
        len: 5,
        flags: libc::MSG_DONTWAIT,
    };
    push(&mut node_a, 3, &dontwait);
    let done = turns(&mut node_a, 5);
    assert_eq!(done.len(), 1);
    assert_eq!((done[0].user_data, done[0].result), (3, -libc::EAGAIN));
    world.heal(&b, &a);
    let results = drive(&mut node_a, &[1, 2]);
    assert_eq!(results[&1], 5);
    assert_eq!(results[&2], 0);
    let mut buf = [0u8; 8];
    assert_eq!(server.recv(&mut buf, false).unwrap(), 5);
}

#[test]
fn namespace_operations_and_open_adoption() {
    let (world, _, mut backend) = setup();
    let dir_path = CString::new("/data").unwrap();
    let mkdir = Op::MkDirAt {
        dir: None,
        path: &dir_path,
        mode: 0o750,
    };
    push(&mut backend, 1, &mkdir);
    assert_eq!(run(&mut backend, 1), 0);
    assert_eq!(world.metadata("/data").unwrap().mode, libc::S_IFDIR | 0o750);
    push(&mut backend, 2, &mkdir);
    assert_eq!(run(&mut backend, 2), -libc::EEXIST);

    let open_how = how(libc::O_RDONLY | libc::O_DIRECTORY, 0);
    push(&mut backend, 3, &open_op(&dir_path, &open_how));
    let raw = run(&mut backend, 3);
    let dir = backend.adopt_fd(raw).unwrap();
    let name = CString::new("a").unwrap();
    let create = how(libc::O_CREAT | libc::O_RDWR | libc::O_EXCL, 0o640);
    let open = Op::OpenAt2 {
        dir: Some(&dir),
        path: &name,
        how: &create,
    };
    push(&mut backend, 4, &open);
    let raw = run(&mut backend, 4);
    let file = backend.adopt_fd(raw).unwrap();
    push(&mut backend, 5, &open);
    assert_eq!(run(&mut backend, 5), -libc::EEXIST);
    let bad_mode = how(libc::O_RDONLY, 0o600);
    push(&mut backend, 6, &open_op(&dir_path, &bad_mode));
    assert_eq!(run(&mut backend, 6), -libc::EINVAL);

    // SAFETY: an all-zero statx is valid.
    let mut stat: libc::statx = unsafe { std::mem::zeroed() };
    let empty = CString::new("").unwrap();
    let statx = Op::Statx {
        dir: Some(&file),
        path: &empty,
        flags: libc::AT_EMPTY_PATH,
        mask: libc::STATX_BASIC_STATS,
        out: &mut stat,
    };
    push(&mut backend, 7, &statx);
    assert_eq!(run(&mut backend, 7), 0);
    assert_eq!(u32::from(stat.stx_mode), libc::S_IFREG | 0o640);

    let new_name = CString::new("b").unwrap();
    let rename = Op::RenameAt {
        old_dir: Some(&dir),
        old_path: &name,
        new_dir: Some(&dir),
        new_path: &new_name,
        flags: 0,
    };
    push(&mut backend, 8, &rename);
    assert_eq!(run(&mut backend, 8), 0);
    let fsync = Op::Fsync {
        fd: &file,
        datasync: true,
    };
    push(&mut backend, 9, &fsync);
    assert_eq!(run(&mut backend, 9), 0);
    let unlink = Op::UnlinkAt {
        dir: Some(&dir),
        path: &new_name,
        flags: 0,
    };
    push(&mut backend, 10, &unlink);
    assert_eq!(run(&mut backend, 10), 0);
    let rmdir = Op::UnlinkAt {
        dir: None,
        path: &dir_path,
        flags: libc::AT_REMOVEDIR,
    };
    push(&mut backend, 11, &rmdir);
    assert_eq!(run(&mut backend, 11), 0);
    drop((file, dir));
    assert_eq!(world.live_handles(), 0);

    // An unadopted result stays open until closed.
    world.write_file("/x", b"").unwrap();
    let x = CString::new("/x").unwrap();
    let read_only = how(libc::O_RDONLY, 0);
    push(&mut backend, 12, &open_op(&x, &read_only));
    let raw = run(&mut backend, 12);
    assert_eq!(world.live_handles(), 1);
    world.close_raw(raw).unwrap();
    assert_eq!(world.live_handles(), 0);
}

#[test]
fn poll_readiness_and_path_descriptors() {
    let (world, _, mut backend) = setup();
    let (read, write) = backend.pipe().unwrap();
    let poll = Op::PollAdd {
        fd: &read,
        events: libc::POLLIN as u32,
    };
    push(&mut backend, 1, &poll);
    assert!(turns(&mut backend, 2).is_empty());
    write.pipe_write(b"x").unwrap();
    assert_eq!(run(&mut backend, 1), libc::POLLIN as i32);

    world.write_file("/f", b"").unwrap();
    let path = world.open("/f", libc::O_PATH).unwrap();
    let poll = Op::PollAdd {
        fd: &path,
        events: libc::POLLIN as u32,
    };
    push(&mut backend, 2, &poll);
    assert_eq!(run(&mut backend, 2), -libc::EBADF);
    assert_eq!(
        backend.poll_now(&path, libc::POLLIN).unwrap(),
        libc::POLLNVAL
    );
    let recv = Op::Recv {
        fd: &read,
        buf: std::ptr::null_mut(),
        len: 0,
        flags: 0,
    };
    push(&mut backend, 3, &recv);
    assert_eq!(run(&mut backend, 3), -libc::ENOTSOCK);
}

#[test]
fn delayed_alias_open_is_invalidated_by_target_disk_only() {
    for crash_root in ["/storage", "/alias"] {
        let (world, _, mut backend) = setup();
        world.write_file("/storage/f", b"x").unwrap();
        world.create_dir_all("/alias").unwrap();
        world.symlink("/storage/f", "/alias/f").unwrap();
        world.disk().sync_all().unwrap();
        world
            .inject(FaultTarget::Op(OpKind::OpenAt2), Fault::Delay(2))
            .unwrap();
        let path = CString::new("/alias/f").unwrap();
        let read_only = how(libc::O_RDONLY, 0);
        push(&mut backend, 1, &open_op(&path, &read_only));
        assert!(turns(&mut backend, 1).is_empty());
        world.disk().crash_under(crash_root).unwrap();
        let result = run(&mut backend, 1);
        if crash_root == "/storage" {
            assert_eq!(result, -libc::EIO);
        } else {
            world.close_raw(result).unwrap();
        }
    }
}

#[test]
fn pending_operations_survive_unrelated_crashes() {
    let (world, _, mut backend) = setup();
    for path in ["/a/file", "/b/file", "/ab/file"] {
        world.write_file(path, b"old").unwrap();
    }
    world.disk().sync_all().unwrap();
    let fds: Vec<SimFd> = ["/a/file", "/b/file", "/ab/file"]
        .iter()
        .map(|path| world.open(path, libc::O_RDWR).unwrap())
        .collect();
    let paths: Vec<CString> = ["/a/late", "/b/late", "/ab/late"]
        .iter()
        .map(|path| CString::new(*path).unwrap())
        .collect();
    let create = how(libc::O_CREAT | libc::O_RDWR, 0o600);
    for (index, fd) in fds.iter().enumerate() {
        world
            .inject(FaultTarget::Op(OpKind::Write), Fault::Delay(2))
            .unwrap();
        push(&mut backend, index as u64, &write_op(fd, 0, b"new"));
        world
            .inject(FaultTarget::Op(OpKind::OpenAt2), Fault::Delay(2))
            .unwrap();
        push(
            &mut backend,
            10 + index as u64,
            &open_op(&paths[index], &create),
        );
    }
    assert!(turns(&mut backend, 1).is_empty());
    for _ in 0..if cfg!(miri) { 20 } else { 4100 } {
        world.disk().crash_under("/a").unwrap();
    }
    let results = drive(&mut backend, &[0, 1, 2, 10, 11, 12]);
    assert_eq!(results[&0], -libc::EIO);
    assert_eq!((results[&1], results[&2]), (3, 3));
    assert_eq!(results[&10], -libc::EIO);
    world.close_raw(results[&11]).unwrap();
    world.close_raw(results[&12]).unwrap();
    assert_eq!(world.read_file("/a/file").unwrap(), b"old");
    assert_eq!(world.read_file("/b/file").unwrap(), b"new");
    assert!(world.metadata("/a/late").is_err());
    assert_eq!(backend.in_flight(), 0);
}

#[test]
fn fsync_executed_before_crash_is_durable_even_if_unpopped() {
    let (world, _, mut backend) = setup();
    world.write_file("/file", b"old").unwrap();
    world.disk().sync_all().unwrap();
    let fd = world.open("/file", libc::O_RDWR).unwrap();
    fd.write_at(0, b"new").unwrap();
    let fsync = Op::Fsync {
        fd: &fd,
        datasync: false,
    };
    push(&mut backend, 1, &fsync);
    backend.submit().unwrap();
    world.disk().crash().unwrap();
    assert_eq!(backend.pop().unwrap().result, 0);
    assert_eq!(world.read_file("/file").unwrap(), b"new");
    let mut buf = [0u8; 3];
    push(&mut backend, 2, &read_op(&fd, 0, &mut buf));
    assert_eq!(
        run(&mut backend, 2),
        -libc::EBADF,
        "crash closed the description"
    );
}

#[test]
fn completions_retain_descriptors_until_popped() {
    let (world, _, mut backend) = setup();
    let (fd, peer) = world.socket_pair().unwrap();
    let mut buf = [0u8; 4];
    let recv = Op::Recv {
        fd: &fd,
        buf: buf.as_mut_ptr(),
        len: 4,
        flags: 0,
    };
    push(&mut backend, 1, &recv);
    drop(fd);
    assert_eq!(
        world.live_handles(),
        2,
        "in-flight op keeps the socket open"
    );
    backend.cancel(2, 1).unwrap();
    assert_eq!(world.live_handles(), 1);
    drop(peer);
    assert_eq!(world.live_handles(), 0);
    assert_eq!(turns(&mut backend, 1).len(), 2);
}

/// One scenario mixing every source of nondeterminism the simulator owns.
fn scenario(seed: u64) -> (Vec<Event>, Vec<u8>, Vec<i32>) {
    let world = World::new();
    let clock = SimClock::new(seed);
    let mut backend = backend(&world, &clock);
    let mut random = [0u8; 16];
    backend.entropy().fill(&mut random).unwrap();
    world.write_file("/file", b"abc").unwrap();
    world
        .inject(FaultTarget::Op(OpKind::OpenAt2), Fault::Delay(2))
        .unwrap();
    let path = CString::new("/file").unwrap();
    let read_only = how(libc::O_RDONLY, 0);
    push(&mut backend, 1, &open_op(&path, &read_only));
    let raw = run(&mut backend, 1);
    let fd = backend.adopt_fd(raw).unwrap();
    world
        .inject(FaultTarget::Op(OpKind::Read), Fault::Errno(libc::EIO))
        .unwrap();
    let mut buf = [0u8; 3];
    push(&mut backend, 2, &read_op(&fd, 0, &mut buf));
    let (a, b) = world.socket_pair().unwrap();
    let mut spec = KernelTimespec::default();
    let delay = Duration::from_millis(u64::from(random[0]));
    let timeout = Op::Timeout {
        deadline: clock.now() + delay,
        timespec: &mut spec,
    };
    push(&mut backend, 3, &timeout);
    let mut received = [0u8; 4];
    let recv = Op::Recv {
        fd: &b,
        buf: received.as_mut_ptr(),
        len: 4,
        flags: 0,
    };
    push(&mut backend, 4, &recv);
    a.send(&random[..4]).unwrap();
    backend.set_auto_advance(true);
    let mut results = Vec::new();
    for _ in 0..10 {
        backend.submit().unwrap();
        backend.wait(Duration::from_millis(100)).unwrap();
        while let Some(entry) = backend.pop() {
            results.push(entry.user_data as i32);
            results.push(entry.result);
        }
    }
    (world.trace(), random.to_vec(), results)
}

#[test]
fn replay_is_deterministic() {
    let first = scenario(42);
    assert_eq!(first, scenario(42));
    assert_ne!(first.1, scenario(43).1);
    assert!(first.2.contains(&-libc::ETIME));
    assert!(first.2.contains(&-libc::EIO));
}
