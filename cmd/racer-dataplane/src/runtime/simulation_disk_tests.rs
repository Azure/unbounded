use super::*;

#[test]
fn partition_stalls_established_streams_and_connects_then_heals_without_loss() {
    let (sim, _environment, r, scope) = setup();
    let a = SocketAddress::Inet("127.0.0.1:101".parse().unwrap());
    let b = SocketAddress::Inet("127.0.0.1:102".parse().unwrap());
    let listener = sim.listen(b.clone()).unwrap();
    let _node = sim.enter_endpoint(a.clone());
    let client = Rc::new(sim.connect(b.clone()).unwrap());
    let Descriptor::Sim(listener) = listener else {
        unreachable!()
    };
    let server = Rc::new(listener.accept().unwrap());
    drive(
        &r,
        r.send(client.clone(), r.file_bytes(b"before").unwrap(), (), &scope),
    )
    .unwrap();
    sim.partition(a.clone(), b.clone());
    let read = drive(
        &r,
        r.recv(server.clone(), r.file_buffer(32).unwrap(), (), &scope),
    )
    .unwrap();
    assert_eq!(read.buffer.prefix(read.bytes).unwrap(), b"before");
    drop(read);
    let mut send = r.send(client.clone(), r.file_bytes(b"after").unwrap(), (), &scope);
    let mut reverse = r.send(
        server.clone(),
        r.file_bytes(b"reverse").unwrap(),
        (),
        &scope,
    );
    let fresh = Rc::new(Descriptor::socket(libc::AF_INET).unwrap());
    let mut connect = r.connect(fresh.clone(), b.clone(), &scope);
    let mut ready = r.readiness(client.clone(), libc::POLLOUT as u32, &scope);
    for _ in 0..5 {
        assert!(poll(&mut send).is_pending());
        assert!(poll(&mut reverse).is_pending());
        assert!(poll(&mut connect).is_pending());
        assert!(poll(&mut ready).is_pending());
        r.poll_budgeted(8).unwrap();
    }
    let (x, y) = sim.socket_pair();
    x.try_send(b"ok").unwrap();
    let mut bytes = [0; 8];
    assert_eq!(y.try_recv(&mut bytes).unwrap(), 2);
    sim.heal(b.clone(), a.clone());
    assert_eq!(drive(&r, send).unwrap().bytes, 5);
    assert_eq!(drive(&r, reverse).unwrap().bytes, 7);
    drive(&r, connect).unwrap();
    drive(&r, ready).unwrap();
    assert_eq!(server.try_recv(&mut bytes).unwrap(), 5);
    assert_eq!(&bytes[..5], b"after");
    assert_eq!(client.try_recv(&mut bytes).unwrap(), 7);
    assert_eq!(&bytes[..7], b"reverse");
    sim.partition(a, b);
    let weak = Rc::downgrade(&client);
    let mut pending = r.send(client, r.file_bytes(b"blocked").unwrap(), (), &scope);
    assert!(poll(&mut pending).is_pending());
    drop(pending);
    r.poll_budgeted(1).unwrap();
    assert!(weak.upgrade().is_some());
    drive(&r, r.drain()).unwrap();
    assert!(weak.upgrade().is_none());
}
use crate::{
    error::{Error, Operation, Result},
    model::identity::RequestId,
    runtime::{admission::Admission, deadline::RequestScope, reactor::Reactor},
};
use std::{
    task::{Context, Poll},
    time::{Duration, Instant},
};

fn setup() -> (Simulation, Environment, Reactor, RequestScope) {
    let sim = Simulation::new();
    let environment = sim.enter();
    let r = Reactor::new(Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let scope = RequestScope::new(
        RequestId([33; 16]),
        Instant::now() + Duration::from_secs(30),
    )
    .unwrap();
    (sim, environment, r, scope)
}
fn poll<T>(op: &mut Operation<'_, T>) -> Poll<Result<T>> {
    op.as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}
fn drive<T>(r: &Reactor, mut op: Operation<'_, T>) -> Result<T> {
    for _ in 0..100 {
        if let Poll::Ready(result) = poll(&mut op) {
            return result;
        }
        r.poll_budgeted(16)?;
    }
    panic!("disk operation stalled")
}
fn open(sim: &Simulation, path: &str) -> Rc<Descriptor> {
    Rc::new(sim.open(None, Path::new(path), libc::O_RDWR).unwrap())
}
fn read(sim: &Simulation, path: &str) -> Vec<u8> {
    sim.disk()
        .read(Path::new(path), 0, 64, DiskState::Volatile)
        .unwrap()
}

#[test]
fn file_sync_and_namespace_sync_are_independent() {
    for (file_sync, dir_sync) in [(false, false), (true, false), (false, true), (true, true)] {
        let (sim, _environment, r, scope) = setup();
        sim.create_dir_all(Path::new("/disk")).unwrap();
        sim.disk().sync_all().unwrap();
        sim.write_file(Path::new("/disk/new"), b"contents").unwrap();
        if file_sync {
            drive(&r, r.file_sync(open(&sim, "/disk/new"), &scope)).unwrap();
        }
        if dir_sync {
            let dir = Rc::new(
                sim.open(None, Path::new("/disk"), libc::O_RDONLY | libc::O_DIRECTORY)
                    .unwrap(),
            );
            drive(&r, r.file_sync(dir, &scope)).unwrap();
        }
        sim.disk().crash().unwrap();
        if !dir_sync {
            assert_eq!(
                sim.read_file(Path::new("/disk/new"))
                    .unwrap_err()
                    .raw_os_error(),
                Some(libc::ENOENT)
            );
        } else {
            assert_eq!(
                read(&sim, "/disk/new"),
                if file_sync {
                    b"contents".as_slice()
                } else {
                    b""
                }
            );
        }
    }
}

#[test]
fn replacement_unlink_and_directory_ancestors_require_namespace_fences() {
    let (sim, _environment, _, _) = setup();
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
fn failed_sync_and_crash_during_pending_write_preserve_fences_and_other_disks() {
    let (sim, _environment, r, scope) = setup();
    for path in ["/a/file", "/b/file"] {
        sim.write_file(Path::new(path), b"old").unwrap();
    }
    sim.disk().sync_all().unwrap();
    let a = open(&sim, "/a/file");
    let b = open(&sim, "/b/file");
    drive(
        &r,
        r.write_at(a.clone(), 0, r.file_bytes(b"bad").unwrap(), (), &scope),
    )
    .unwrap();
    sim.inject("fsync", Fault::Errno(libc::EIO));
    assert_eq!(drive(&r, r.file_sync(a.clone(), &scope)), Err(Error::Io));
    sim.inject("write", Fault::Delay(3));
    let weak = Rc::downgrade(&a);
    let lease = r
        .admission
        .reserve(None, crate::model::limits::ResourceClass::Connection, 1)
        .unwrap();
    let mut write = r.write_at(a, 0, r.file_bytes(b"late").unwrap(), lease, &scope);
    assert!(poll(&mut write).is_pending());
    sim.disk().crash_under(Path::new("/a")).unwrap();
    assert!(weak.upgrade().is_some());
    assert_eq!(r.in_flight(), 1);
    assert_eq!(read(&sim, "/a/file"), b"old");
    drive(
        &r,
        r.write_at(b, 0, r.file_bytes(b"new").unwrap(), (), &scope),
    )
    .unwrap();
    assert!(matches!(drive(&r, write), Err(Error::Io)));
    assert!(weak.upgrade().is_none());
    assert_eq!(read(&sim, "/a/file"), b"old");
    assert_eq!(read(&sim, "/b/file"), b"new");
    assert_eq!(
        r.admission
            .used(crate::model::limits::ResourceClass::Connection),
        0
    );
    // A path-only open queued before crash cannot recreate lost names afterwards.
    sim.inject("open", Fault::Delay(2));
    let mut op = r.file_open(
        None,
        CString::new("/a/late").unwrap(),
        libc::O_CREAT | libc::O_RDWR,
        0,
        &scope,
    );
    assert!(poll(&mut op).is_pending());
    sim.disk().crash_under(Path::new("/a")).unwrap();
    assert!(matches!(drive(&r, op), Err(Error::Io)));
    assert!(sim.metadata(Path::new("/a/late")).is_err());
}

#[test]
fn sparse_corruption_and_truncate_do_not_mutate_durable_shared_pages() {
    let (sim, _environment, r, scope) = setup();
    sim.write_file(Path::new("/file"), b"abcdef").unwrap();
    sim.disk().sync_all().unwrap();
    let fd = open(&sim, "/file");
    let Descriptor::Sim(h) = &*fd else {
        unreachable!()
    };
    h.set_len(2).unwrap();
    h.set_len(6).unwrap();
    assert_eq!(read(&sim, "/file"), b"ab\0\0\0\0");
    assert_eq!(
        sim.disk()
            .read(Path::new("/file"), 0, 8, DiskState::Durable)
            .unwrap(),
        b"abcdef"
    );
    sim.disk()
        .corrupt(Path::new("/file"), 1, b"X", DiskState::Volatile)
        .unwrap();
    sim.disk()
        .corrupt(Path::new("/file"), 4, b"Y", DiskState::Durable)
        .unwrap();
    sim.disk().crash().unwrap();
    assert_eq!(read(&sim, "/file"), b"abcdYf");
    let fd = open(&sim, "/file");
    drive(
        &r,
        r.write_at(
            fd.clone(),
            1 << 40,
            r.file_bytes(b"sparse").unwrap(),
            (),
            &scope,
        ),
    )
    .unwrap();
    drive(&r, r.file_sync(fd, &scope)).unwrap();
    assert_eq!(
        sim.disk()
            .read(Path::new("/file"), (1 << 40) - 2, 10, DiskState::Durable)
            .unwrap(),
        b"\0\0sparse"
    );
    assert!(
        sim.disk()
            .corrupt(Path::new("/file"), u64::MAX, b"bad", DiskState::Both)
            .is_err()
    );
    sim.disk().crash().unwrap();
    assert_eq!(
        sim.disk()
            .read(Path::new("/file"), 1 << 40, 6, DiskState::Volatile)
            .unwrap(),
        b"sparse"
    );
}

#[test]
fn crash_after_sync_issue_before_cqe_and_abandoned_sync_keep_real_fences() {
    let (sim, _environment, r, scope) = setup();
    sim.write_file(Path::new("/file"), b"old").unwrap();
    sim.disk().sync_all().unwrap();
    let fd = open(&sim, "/file");
    drive(
        &r,
        r.write_at(fd.clone(), 0, r.file_bytes(b"new").unwrap(), (), &scope),
    )
    .unwrap();
    let mut sync = r.file_sync(fd.clone(), &scope);
    assert!(poll(&mut sync).is_pending());
    // Submit fsync, but leave its successful original CQE unconsumed.
    r.poll_budgeted(1).unwrap();
    assert_eq!(r.in_flight(), 1);
    sim.disk().crash().unwrap();
    assert_eq!(read(&sim, "/file"), b"new");
    assert!(matches!(drive(&r, sync), Ok(())));
    assert!(matches!(drive(&r, r.file_stat(fd, &scope)), Err(Error::Io)));
    let fd = open(&sim, "/file");
    drive(
        &r,
        r.write_at(fd.clone(), 0, r.file_bytes(b"bad").unwrap(), (), &scope),
    )
    .unwrap();
    let weak = Rc::downgrade(&fd);
    let mut sync = r.file_sync(fd, &scope);
    assert!(poll(&mut sync).is_pending());
    drop(sync);
    r.poll_budgeted(1).unwrap();
    assert!(weak.upgrade().is_some());
    r.poll_budgeted(1).unwrap();
    assert!(weak.upgrade().is_some());
    r.poll_budgeted(1).unwrap();
    assert!(weak.upgrade().is_none());
    sim.disk().crash().unwrap();
    assert_eq!(read(&sim, "/file"), b"new");
}

#[test]
fn renamed_directory_fsync_uses_inode_and_durable_corruption_targets_exact_generation() {
    let (sim, _environment, _, _) = setup();
    sim.write_file(Path::new("/old/file"), b"before").unwrap();
    sim.disk().sync_all().unwrap();
    let directory = sim
        .open(None, Path::new("/old"), libc::O_RDONLY | libc::O_DIRECTORY)
        .unwrap();
    sim.rename(Path::new("/old"), Path::new("/new"), 0).unwrap();
    sim.write_file(Path::new("/new/extra"), b"extra").unwrap();
    sim.disk().sync(Path::new("/new/extra")).unwrap();
    let Descriptor::Sim(directory) = directory else {
        unreachable!()
    };
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
