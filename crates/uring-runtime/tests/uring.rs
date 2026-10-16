//! Integration tests against the real io_uring backend.
//!
//! When io_uring is unavailable these tests are skipped with a message,
//! unless `URING_RUNTIME_REQUIRE_KERNEL=1` is set, in which case they fail.

#![cfg(target_os = "linux")]

use std::ffi::CString;
use std::future::Future;
use std::net::{Ipv6Addr, SocketAddr, SocketAddrV6};
use std::os::fd::{FromRawFd, OwnedFd};
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::pin::pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::task::{Context, Poll, Wake, Waker};
use std::time::{Duration, Instant};

use futures::future::join;
use uring_runtime::backend::{OpKind, OpenHow};
use uring_runtime::reactor::leaked_operations;
use uring_runtime::{
    Backend, CancelToken, ErrorKind, Fd, Host, Reactor, ReactorConfig, Scope, SocketAddress,
    Unscoped, Uring,
};

const REQUIRE: &str = "URING_RUNTIME_REQUIRE_KERNEL";

/// Create a reactor, or explain why the test is skipped.
fn reactor_with(config: ReactorConfig) -> Option<Reactor<Uring>> {
    if !Uring::is_available() {
        if std::env::var(REQUIRE).is_ok_and(|value| value == "1") {
            panic!("io_uring is unavailable but {REQUIRE}=1");
        }
        eprintln!("skipping: io_uring is unavailable (set {REQUIRE}=1 to fail instead)");
        return None;
    }
    Some(Reactor::new(Uring::new(), config).expect("reactor"))
}

fn reactor() -> Option<Reactor<Uring>> {
    reactor_with(ReactorConfig::default())
}

struct Flag(AtomicBool);

impl Wake for Flag {
    fn wake(self: Arc<Self>) {
        self.0.store(true, Ordering::SeqCst);
    }
}

/// Drive `future` and the reactor until the future resolves.
fn block_on<F: Future>(reactor: &Reactor<Uring>, future: F) -> F::Output {
    let flag = Arc::new(Flag(AtomicBool::new(true)));
    let waker = Waker::from(flag.clone());
    let mut cx = Context::from_waker(&waker);
    let mut future = pin!(future);
    let started = Instant::now();
    loop {
        if flag.0.swap(false, Ordering::SeqCst)
            && let Poll::Ready(output) = future.as_mut().poll(&mut cx)
        {
            return output;
        }
        assert!(
            started.elapsed() < Duration::from_secs(10),
            "test timed out"
        );
        reactor.poll(64).expect("poll");
        if !flag.0.load(Ordering::SeqCst) {
            reactor.wait(Duration::from_millis(10)).expect("wait");
        }
    }
}

fn temp_dir(name: &str) -> PathBuf {
    let base = PathBuf::from(env!("CARGO_TARGET_TMPDIR"));
    let dir = base.join(format!("uring-{name}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

fn cstring(path: &Path) -> CString {
    CString::new(path.as_os_str().as_bytes()).unwrap()
}

fn listener(reactor: &Reactor<Uring>, address: SocketAddress) -> (Fd, SocketAddress) {
    let backend = reactor.backend();
    let fd = backend
        .socket(address.family(), libc::SOCK_STREAM, 0)
        .unwrap();
    backend.bind(&fd, &address).unwrap();
    backend.listen(&fd, 16).unwrap();
    let local = backend.local_addr(&fd).unwrap();
    (fd, local)
}

fn loopback() -> SocketAddress {
    SocketAddress::from(SocketAddr::from(([127, 0, 0, 1], 0)))
}

#[test]
fn ring_setup_and_probe() {
    let Some(reactor) = reactor() else { return };
    let backend = reactor.backend();
    for kind in [
        OpKind::Nop,
        OpKind::Read,
        OpKind::Write,
        OpKind::Recv,
        OpKind::Send,
        OpKind::PollAdd,
        OpKind::Accept,
        OpKind::Connect,
        OpKind::Timeout,
        OpKind::Cancel,
    ] {
        assert!(backend.supports(kind), "{kind:?}");
    }
    assert!(format!("{:?}", *backend).contains("sq_entries"));
    drop(backend);
    block_on(&reactor, reactor.nop(&Unscoped)).unwrap();
}

#[test]
fn tcp_accept_connect_send_recv() {
    let Some(reactor) = reactor() else { return };
    let (listener, address) = listener(&reactor, loopback());
    let client = reactor
        .backend()
        .socket(libc::AF_INET, libc::SOCK_STREAM, 0)
        .unwrap();
    let (accepted, connected) = block_on(
        &reactor,
        join(
            reactor.accept(&listener, &Unscoped),
            reactor.connect(&client, &address, &Unscoped),
        ),
    );
    connected.unwrap();
    let (server, peer) = accepted.unwrap();
    assert_eq!(
        peer.decode().unwrap(),
        reactor.backend().local_addr(&client).unwrap()
    );
    reactor.backend().set_nodelay(&client, true).unwrap();
    assert_eq!(
        reactor.backend().peer_addr(&client).unwrap(),
        address,
        "connected to the listener"
    );

    let sent = block_on(
        &reactor,
        reactor.send(&client, b"hello".as_slice(), &Unscoped),
    );
    assert_eq!(sent.result.unwrap(), 5);
    let received = block_on(
        &reactor,
        reactor.recv(&server, Vec::with_capacity(16), &Unscoped),
    );
    assert_eq!(received.result.unwrap(), 5);
    assert_eq!(received.buf, b"hello");

    reactor
        .backend()
        .shutdown(&client, std::net::Shutdown::Write)
        .unwrap();
    let eof = block_on(
        &reactor,
        reactor.recv(&server, Vec::with_capacity(4), &Unscoped),
    );
    assert_eq!(eof.result.unwrap(), 0, "end of stream");
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn unix_socket_keeps_its_address_alive() {
    let Some(reactor) = reactor() else { return };
    // sun_path holds 108 bytes, too few for paths under the target directory.
    let dir = std::env::temp_dir().join(format!("uring-unix-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let path = dir.join("socket");
    let (listener, _) = listener(&reactor, SocketAddress::Unix(path.clone()));
    let client = reactor
        .backend()
        .socket(libc::AF_UNIX, libc::SOCK_STREAM, 0)
        .unwrap();
    // The address is encoded into the operation; the caller's copy may go.
    let connect = {
        let address = SocketAddress::Unix(path.clone());
        reactor.connect(&client, &address, &Unscoped)
    };
    let (accepted, connected) = block_on(
        &reactor,
        join(reactor.accept(&listener, &Unscoped), connect),
    );
    connected.unwrap();
    let (server, _peer) = accepted.unwrap();
    let sent = block_on(&reactor, reactor.send(&server, vec![7; 3], &Unscoped));
    assert_eq!(sent.result.unwrap(), 3);
    let received = block_on(
        &reactor,
        reactor.recv(&client, Vec::with_capacity(3), &Unscoped),
    );
    assert_eq!(received.buf, [7, 7, 7]);
    std::fs::remove_dir_all(dir).unwrap();
}

#[test]
fn ipv6_connect_with_flowinfo() {
    let Some(reactor) = reactor() else { return };
    let backend = reactor.backend();
    let fd = backend
        .socket(libc::AF_INET6, libc::SOCK_STREAM, 0)
        .unwrap();
    let any = SocketAddress::from(SocketAddrV6::new(Ipv6Addr::LOCALHOST, 0, 0, 0));
    if let Err(error) = backend.bind(&fd, &any) {
        eprintln!("skipping: IPv6 loopback unavailable: {error}");
        return;
    }
    backend.listen(&fd, 4).unwrap();
    let port = backend.local_addr(&fd).unwrap().as_inet().unwrap().port();
    let client = backend
        .socket(libc::AF_INET6, libc::SOCK_STREAM, 0)
        .unwrap();
    drop(backend);
    let target = SocketAddress::from(SocketAddrV6::new(Ipv6Addr::LOCALHOST, port, 0x12345, 0));
    let (accepted, connected) = block_on(
        &reactor,
        join(
            reactor.accept(&fd, &Unscoped),
            reactor.connect(&client, &target, &Unscoped),
        ),
    );
    connected.unwrap();
    let (_, peer) = accepted.unwrap();
    assert!(peer.decode().unwrap().as_inet().unwrap().is_ipv6());
}

#[test]
fn connect_refused_reports_errno() {
    let Some(reactor) = reactor() else { return };
    // Find a free port, then close the listener.
    let address = {
        let (fd, address) = listener(&reactor, loopback());
        drop(fd);
        address
    };
    let client = reactor
        .backend()
        .socket(libc::AF_INET, libc::SOCK_STREAM, 0)
        .unwrap();
    let error = block_on(&reactor, reactor.connect(&client, &address, &Unscoped)).unwrap_err();
    assert_eq!(error.raw_os_error(), Some(libc::ECONNREFUSED));
}

fn memfd() -> Fd {
    // SAFETY: the name is a valid C string; the result is checked.
    let fd = unsafe { libc::memfd_create(c"uring-test".as_ptr(), libc::MFD_CLOEXEC) };
    assert!(fd >= 0);
    // SAFETY: `fd` is fresh and owned by nothing else.
    Fd::from(unsafe { OwnedFd::from_raw_fd(fd) })
}

#[test]
fn positioned_file_io() {
    let Some(reactor) = reactor() else { return };
    let file = memfd();
    let written = block_on(
        &reactor,
        reactor.write_at(&file, 5, b"abcdef".as_slice(), &Unscoped),
    );
    assert_eq!(written.result.unwrap(), 6);
    let read = block_on(
        &reactor,
        reactor.read_at(&file, 7, Vec::with_capacity(16), &Unscoped),
    );
    assert_eq!(read.result.unwrap(), 4, "short read at end of file");
    assert_eq!(read.buf, b"cdef");
    let read = block_on(
        &reactor,
        reactor.read_at(&file, 0, Vec::with_capacity(5), &Unscoped),
    );
    assert_eq!(read.buf, [0; 5], "the hole reads as zeros");
    let eof = block_on(
        &reactor,
        reactor.read_at(&file, 100, Vec::with_capacity(1), &Unscoped),
    );
    assert_eq!(eof.result.unwrap(), 0);
    block_on(&reactor, reactor.fsync(&file, true, &Unscoped)).unwrap();
    // IOSQE_ASYNC changes only where the kernel executes the operation.
    let read = block_on(
        &reactor,
        reactor
            .read_at(&file, 5, Vec::with_capacity(6), &Unscoped)
            .offload(),
    );
    assert_eq!(read.result.unwrap(), 6);
    assert_eq!(read.buf, b"abcdef");
    block_on(&reactor, reactor.fsync(&file, false, &Unscoped).offload()).unwrap();
}

#[test]
fn absolute_timeouts() {
    let Some(reactor) = reactor() else { return };
    let started = Instant::now();
    let deadline = started + Duration::from_millis(30);
    block_on(&reactor, reactor.sleep_until(deadline, &Unscoped)).unwrap();
    assert!(Instant::now() >= deadline - Duration::from_millis(1));
    let started = Instant::now();
    block_on(
        &reactor,
        reactor.sleep_until(started - Duration::from_secs(1), &Unscoped),
    )
    .unwrap();
    assert!(
        started.elapsed() < Duration::from_secs(1),
        "a past deadline fires at once"
    );
}

#[test]
fn readiness_on_a_pipe() {
    let Some(reactor) = reactor() else { return };
    let (read, write) = reactor.backend().pipe().unwrap();
    let ready = reactor.readiness(&read, libc::POLLIN as u32, &Unscoped);
    let writer = async {
        block_on_nothing().await;
        // SAFETY: writes one byte from a static buffer to an open pipe.
        let n = unsafe {
            libc::write(
                std::os::fd::AsRawFd::as_raw_fd(&write),
                b"x".as_ptr().cast(),
                1,
            )
        };
        assert_eq!(n, 1);
    };
    let (events, ()) = block_on(&reactor, join(ready, writer));
    assert_ne!(events.unwrap() & libc::POLLIN as u32, 0);
    assert_ne!(
        reactor.backend().poll_now(&read, libc::POLLIN).unwrap() & libc::POLLIN,
        0
    );
}

/// Yield once.
async fn block_on_nothing() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if yielded {
            Poll::Ready(())
        } else {
            yielded = true;
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    })
    .await;
}

#[test]
fn scope_cancellation_fences_a_pending_receive() {
    let Some(reactor) = reactor() else { return };
    let (listener, address) = listener(&reactor, loopback());
    let client = reactor
        .backend()
        .socket(libc::AF_INET, libc::SOCK_STREAM, 0)
        .unwrap();
    let (accepted, connected) = block_on(
        &reactor,
        join(
            reactor.accept(&listener, &Unscoped),
            reactor.connect(&client, &address, &Unscoped),
        ),
    );
    connected.unwrap();
    let (server, _) = accepted.unwrap();
    let token = CancelToken::new();
    let remote = token.clone();
    let canceler = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(20));
        remote.cancel();
    });
    let started = Instant::now();
    let outcome = block_on(
        &reactor,
        reactor.recv(&server, Vec::with_capacity(8), &token),
    );
    canceler.join().unwrap();
    assert!(outcome.result.unwrap_err().is_canceled());
    assert!(started.elapsed() < Duration::from_secs(5));
    assert_eq!(outcome.buf.capacity(), 8, "the buffer is returned");
    assert!(token.check().is_err());
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn dropped_operations_are_canceled_and_drained() {
    let Some(reactor) = reactor() else { return };
    let (read, _write) = reactor.backend().pipe().unwrap();
    {
        let mut ready = reactor.readiness(&read, libc::POLLIN as u32, &Unscoped);
        let mut cx = Context::from_waker(Waker::noop());
        assert!(std::pin::Pin::new(&mut ready).poll(&mut cx).is_pending());
        reactor.poll(0).unwrap();
    }
    assert_eq!(reactor.in_flight(), 1);
    block_on(&reactor, reactor.drain()).unwrap();
    assert_eq!(reactor.in_flight(), 0);
    let late = block_on(&reactor, reactor.nop(&Unscoped)).unwrap_err();
    assert_eq!(late.kind(), ErrorKind::Unavailable);
}

#[test]
fn fence_matching_cancels_one_request() {
    #[derive(Clone)]
    struct Request(u32);
    impl Scope for Request {
        type Error = uring_runtime::Error;
        fn check(&self) -> uring_runtime::Result<()> {
            Ok(())
        }
    }
    let Some(reactor) = reactor() else { return };
    let (read, write) = reactor.backend().pipe().unwrap();
    let mut first = reactor.readiness(&read, libc::POLLIN as u32, &Request(1));
    let mut second = reactor.readiness(&read, libc::POLLIN as u32, &Request(2));
    let mut cx = Context::from_waker(Waker::noop());
    assert!(std::pin::Pin::new(&mut first).poll(&mut cx).is_pending());
    assert!(std::pin::Pin::new(&mut second).poll(&mut cx).is_pending());
    block_on(
        &reactor,
        reactor.fence_matching(|request: &Request| request.0 == 1),
    )
    .unwrap();
    assert!(block_on(&reactor, &mut first).unwrap_err().is_canceled());
    assert_eq!(reactor.in_flight(), 1);
    // SAFETY: writes one byte from a static buffer to an open pipe.
    let n = unsafe {
        libc::write(
            std::os::fd::AsRawFd::as_raw_fd(&write),
            b"x".as_ptr().cast(),
            1,
        )
    };
    assert_eq!(n, 1);
    assert_ne!(block_on(&reactor, &mut second).unwrap(), 0);
}

#[test]
fn remote_and_local_wakes_interrupt_wait() {
    let Some(reactor) =
        reactor_with(ReactorConfig::default().with_max_wait(Duration::from_secs(5)))
    else {
        return;
    };
    // An idle wait blocks for its timeout.
    let started = Instant::now();
    reactor.wait(Duration::from_millis(30)).unwrap();
    assert!(started.elapsed() >= Duration::from_millis(25));

    let waker = reactor.remote_waker();
    let thread = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(50));
        waker.wake();
        waker.wake();
    });
    let started = Instant::now();
    let mut waits = 0;
    while started.elapsed() < Duration::from_millis(45) {
        reactor.wait(Duration::from_secs(5)).unwrap();
        waits += 1;
    }
    thread.join().unwrap();
    assert!(started.elapsed() < Duration::from_secs(4), "woken remotely");
    assert!(waits <= 3, "{waits} waits");

    // A same-thread wake makes the next wait return without blocking.
    reactor.remote_waker().wake();
    let started = Instant::now();
    reactor.wait(Duration::from_secs(5)).unwrap();
    assert!(started.elapsed() < Duration::from_secs(1));
}

#[test]
fn filesystem_operations() {
    let Some(reactor) = reactor() else { return };
    let backend_supports = reactor.backend().supports(OpKind::MkDirAt);
    let root = temp_dir("fs");
    let dir: Fd = OwnedFd::from(std::fs::File::open(&root).unwrap()).into();

    if backend_supports {
        block_on(
            &reactor,
            reactor.mkdir_at(Some(&dir), c"sub", 0o700, &Unscoped),
        )
        .unwrap();
        let error = block_on(
            &reactor,
            reactor.mkdir_at(Some(&dir), c"sub", 0o700, &Unscoped),
        )
        .unwrap_err();
        assert_eq!(error.kind(), ErrorKind::AlreadyExists);
    } else {
        eprintln!("mkdirat unsupported; creating the directory synchronously");
        std::fs::create_dir(root.join("sub")).unwrap();
    }

    let how = OpenHow {
        flags: (libc::O_CREAT | libc::O_RDWR | libc::O_CLOEXEC) as u64,
        mode: 0o600,
        resolve: 0,
    };
    let file = block_on(
        &reactor,
        reactor.open_at2(Some(&dir), c"sub/file", how, &Unscoped),
    )
    .unwrap();
    let written = block_on(
        &reactor,
        reactor.write_at(&file, 0, b"0123456789".as_slice(), &Unscoped),
    );
    assert_eq!(written.result.unwrap(), 10);
    block_on(&reactor, reactor.fsync(&file, false, &Unscoped)).unwrap();

    let stat = block_on(
        &reactor,
        reactor.statx(Some(&dir), c"sub/file", 0, libc::STATX_SIZE, &Unscoped),
    )
    .unwrap();
    assert_eq!(stat.stx_size, 10);
    let absolute = cstring(&root.join("sub/file"));
    let stat = block_on(
        &reactor,
        reactor.statx(None, &absolute, 0, libc::STATX_SIZE, &Unscoped),
    )
    .unwrap();
    assert_eq!(stat.stx_size, 10);

    block_on(
        &reactor,
        reactor.rename_at(
            Some(&dir),
            c"sub/file",
            Some(&dir),
            c"sub/renamed",
            0,
            &Unscoped,
        ),
    )
    .unwrap();
    assert!(root.join("sub/renamed").exists());
    let missing = block_on(
        &reactor,
        reactor.statx(Some(&dir), c"sub/file", 0, libc::STATX_SIZE, &Unscoped),
    )
    .unwrap_err();
    assert_eq!(missing.kind(), ErrorKind::NotFound);

    block_on(
        &reactor,
        reactor.unlink_at(Some(&dir), c"sub/renamed", 0, &Unscoped),
    )
    .unwrap();
    block_on(
        &reactor,
        reactor.unlink_at(Some(&dir), c"sub", libc::AT_REMOVEDIR, &Unscoped),
    )
    .unwrap();
    assert!(!root.join("sub").exists());
    std::fs::remove_dir_all(root).unwrap();
}

#[test]
fn shutdown_and_drop_fence_outstanding_work() {
    let Some(reactor) =
        reactor_with(ReactorConfig::default().with_drop_wait(Duration::from_secs(5)))
    else {
        return;
    };
    let (read, _write) = reactor.backend().pipe().unwrap();
    let mut cx = Context::from_waker(Waker::noop());
    let mut ready = reactor.readiness(&read, libc::POLLIN as u32, &Unscoped);
    assert!(std::pin::Pin::new(&mut ready).poll(&mut cx).is_pending());
    drop(ready);
    reactor.poll(0).unwrap();
    let leaked = leaked_operations();
    drop(reactor);
    assert_eq!(
        leaked_operations(),
        leaked,
        "drop_wait fenced the operation"
    );

    let Some(reactor) = reactor_with(ReactorConfig::default()) else {
        return;
    };
    let mut ready = reactor.readiness(&read, libc::POLLIN as u32, &Unscoped);
    assert!(std::pin::Pin::new(&mut ready).poll(&mut cx).is_pending());
    reactor.shutdown(Duration::from_secs(5)).unwrap();
    assert!(block_on(&reactor, &mut ready).unwrap_err().is_canceled());
}

#[test]
fn reservations_admit_beyond_the_ordinary_partition() {
    let Some(reactor) = reactor_with(
        ReactorConfig::default()
            .with_max_in_flight(1)
            .with_max_reserved(1),
    ) else {
        return;
    };
    let reservation = reactor.reserve(1).unwrap();
    let (read, write) = reactor.backend().pipe().unwrap();
    let mut cx = Context::from_waker(Waker::noop());
    let mut ordinary = reactor.readiness(&read, libc::POLLIN as u32, &Unscoped);
    assert!(std::pin::Pin::new(&mut ordinary).poll(&mut cx).is_pending());
    let rejected = block_on(&reactor, reactor.nop(&Unscoped)).unwrap_err();
    assert_eq!(rejected.kind(), ErrorKind::Overloaded);
    block_on(&reactor, reactor.nop(&Unscoped).reserved(&reservation)).unwrap();
    // SAFETY: writes one byte from a static buffer to an open pipe.
    let n = unsafe {
        libc::write(
            std::os::fd::AsRawFd::as_raw_fd(&write),
            b"x".as_ptr().cast(),
            1,
        )
    };
    assert_eq!(n, 1);
    block_on(&reactor, &mut ordinary).unwrap();
}
