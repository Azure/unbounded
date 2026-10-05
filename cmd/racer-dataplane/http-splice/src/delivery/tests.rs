use super::*;
use http1::{Error, connection::Endpoint};
use std::{
    cell::{Cell, RefCell},
    future::Future,
    io::Read,
    os::unix::net::UnixStream,
    rc::Rc,
    task::Waker,
    time::Duration,
};
use uring_runtime::{
    Scope,
    reactor::{Reactor, SocketAddress},
};

#[derive(Clone, Copy, Debug, PartialEq)]
enum Failure {
    Http(Error),
    Runtime(uring_runtime::Error),
}
impl From<Error> for Failure {
    fn from(e: Error) -> Self {
        Self::Http(e)
    }
}
impl From<uring_runtime::Error> for Failure {
    fn from(e: uring_runtime::Error) -> Self {
        Self::Runtime(e)
    }
}
#[derive(Clone)]
struct TestScope;
impl Scope for TestScope {
    type Error = Failure;
    fn check(&self) -> std::result::Result<(), Failure> {
        Ok(())
    }
}
#[derive(Clone, PartialEq, Eq, PartialOrd, Ord)]
struct Key;
impl Endpoint<Failure> for Key {
    fn address(&self) -> std::result::Result<SocketAddress, Failure> {
        Ok(SocketAddress::Inet("127.0.0.1:9".parse().unwrap()))
    }
}
struct Caller;
impl Context for Caller {
    type Error = Failure;
    type Scope = TestScope;
    type Budget = ();
    type Reactor = Rc<Reactor<TestScope, ()>>;
    type Charge = ();
    type Slot = Rc<()>;
    type Opaque = ();
    type State = ();
    type Endpoint = Key;
    fn charge(&self, _: usize) -> Result<Self, ()> {
        Ok(())
    }
    fn outbound_slot(&self) -> Result<Self, Rc<()>> {
        Ok(Rc::new(()))
    }
    fn stopped(&self) -> bool {
        false
    }
}
#[derive(Default)]
struct Pipe {
    bytes: Vec<u8>,
    calls: usize,
    unsupported_after: Option<usize>,
    blocked: bool,
    interrupt_write: bool,
    interrupt_drain: bool,
    short_drain: bool,
}
impl DeliveryPipe for Pipe {
    fn buffered(&self) -> usize {
        self.bytes.len()
    }
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if std::mem::take(&mut self.interrupt_write) {
            return Err(io::ErrorKind::Interrupted.into());
        }
        self.bytes.extend_from_slice(bytes);
        Ok(bytes.len())
    }
    fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        if std::mem::take(&mut self.interrupt_drain) {
            return Err(io::ErrorKind::Interrupted.into());
        }
        let n = bytes.len().min(self.bytes.len()) - usize::from(self.short_drain);
        bytes[..n].copy_from_slice(&self.bytes[..n]);
        self.bytes.drain(..n);
        Ok(n)
    }
    fn send(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize> {
        self.calls += 1;
        if self.unsupported_after.is_some_and(|n| self.calls > n) {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        if self.blocked {
            return Err(io::ErrorKind::WouldBlock.into());
        }
        let n = socket.try_send(&self.bytes[..count.min(self.bytes.len()).min(2)])?;
        self.bytes.drain(..n);
        Ok(n)
    }
}
struct Reader {
    bytes: Rc<Vec<u8>>,
    pipe: Pipe,
    sent: usize,
}
struct View {
    bytes: Rc<Vec<u8>>,
    range: std::ops::Range<usize>,
}
// SAFETY: the immutable Rc backing is retained and cannot be mutated through this view.
unsafe impl SendBuffer for View {
    type Error = Failure;
    fn send_bytes(&self) -> Result<Caller, &[u8]> {
        Ok(&self.bytes[self.range.clone()])
    }
}
impl Owner<Caller> for Reader {
    type Pipe = Pipe;
    type View = View;
    fn remaining(&self) -> usize {
        self.bytes.len() - self.sent
    }
    fn advance(&mut self, n: usize) {
        self.sent += n;
    }
    fn parts(&mut self) -> (&[u8], &mut Pipe) {
        (&self.bytes[self.sent..], &mut self.pipe)
    }
    fn view(&self, count: usize) -> Result<Caller, View> {
        Ok(View {
            bytes: self.bytes.clone(),
            range: self.sent..self.sent + count,
        })
    }
}
#[derive(Default)]
struct Observe {
    events: RefCell<Vec<(&'static str, usize)>>,
    reject: Cell<bool>,
    scope_error: Cell<bool>,
    times: RefCell<Vec<Instant>>,
}
impl Observer<Caller> for Observe {
    fn scope(&self, stalled_at: Instant) -> Result<Caller, TestScope> {
        self.times.borrow_mut().push(stalled_at);
        if self.scope_error.get() {
            return Err(uring_runtime::Error::DeadlineExceeded.into());
        }
        Ok(TestScope)
    }
    fn direct_bytes(&self, count: usize) {
        self.events.borrow_mut().push(("direct", count));
    }
    fn pipe_drained(&self) {
        self.events.borrow_mut().push(("drain", 0));
    }
    fn before_send(&self, count: usize, _: usize) {
        self.events.borrow_mut().push(("before", count));
    }
    fn after_send(&self, _: usize, count: usize) -> Result<Caller, ()> {
        self.events.borrow_mut().push(("after", count));
        if self.reject.get() {
            return Err(Error::Malformed.into());
        }
        Ok(())
    }
}
fn connection() -> (ConnectionLease<Caller>, UnixStream, std::rc::Weak<()>) {
    let (socket, peer) = UnixStream::pair().unwrap();
    peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
    let slot = Rc::new(());
    let weak = Rc::downgrade(&slot);
    (
        ConnectionLease::from_reserved(socket.into(), slot, ()).unwrap(),
        peer,
        weak,
    )
}
fn drive<T>(reactor: &Reactor<TestScope, ()>, future: impl Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = std::task::Context::from_waker(Waker::noop());
    let until = Instant::now() + Duration::from_secs(2);
    loop {
        if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
            return result;
        }
        assert!(Instant::now() < until, "delivery watchdog");
        reactor.poll_budgeted(32).unwrap();
    }
}

#[test]
fn partial_pipe_fallback_resumes_at_accepted_not_staged_cursor() {
    let reactor = Rc::new(Reactor::new(16, ()));
    let (connection, mut peer, _) = connection();
    let observer = Observe::default();
    let owner = Reader {
        bytes: Rc::new(b"0123456789".to_vec()),
        sent: 0,
        pipe: Pipe {
            unsupported_after: Some(1),
            ..Pipe::default()
        },
    };
    let (owner, connection) = drive(
        &reactor,
        send(&Caller, &reactor, owner, connection, &observer),
    )
    .unwrap();
    assert_eq!(owner.sent, 10);
    assert_eq!(owner.pipe.bytes, b"23456789");
    assert_eq!(*observer.events.borrow(), [("direct", 8)]);
    assert_eq!(
        connection.send_remaining(),
        None,
        "framing remains caller-owned"
    );
    drop(connection);
    let mut wire = Vec::new();
    peer.read_to_end(&mut wire).unwrap();
    assert_eq!(wire, b"0123456789");
}

#[test]
fn drain_interruption_yields_then_owned_send_observes_in_order() {
    let reactor = Rc::new(Reactor::new(16, ()));
    let (connection, mut peer, _) = connection();
    let observer = Observe::default();
    let owner = Reader {
        bytes: Rc::new(b"abcdef".to_vec()),
        sent: 0,
        pipe: Pipe {
            blocked: true,
            interrupt_drain: true,
            ..Pipe::default()
        },
    };
    let mut work = Box::pin(send(&Caller, &reactor, owner, connection, &observer));
    assert!(
        work.as_mut()
            .poll(&mut std::task::Context::from_waker(Waker::noop()))
            .is_pending()
    );
    assert!(observer.events.borrow().is_empty());
    assert_eq!(
        reactor.in_flight(),
        0,
        "interrupted drain yields without owned send"
    );
    let (owner, connection) = drive(&reactor, work).unwrap();
    assert_eq!(owner.sent, 6);
    assert_eq!(
        *observer.events.borrow(),
        [("drain", 0), ("before", 6), ("after", 6), ("direct", 6)]
    );
    assert_eq!(
        observer.times.borrow()[0],
        observer.times.borrow()[1],
        "no progress on EINTR"
    );
    drop(connection);
    let mut wire = Vec::new();
    peer.read_to_end(&mut wire).unwrap();
    assert_eq!(wire, b"abcdef");
}

#[test]
fn short_drain_scope_rejection_and_completion_rejection_do_not_advance() {
    for failure in ["drain", "scope", "completion"] {
        let reactor = Rc::new(Reactor::new(16, ()));
        let (connection, _peer, weak) = connection();
        let observer = Observe::default();
        observer.scope_error.set(failure == "scope");
        observer.reject.set(failure == "completion");
        let owner = Reader {
            bytes: Rc::new(b"abcdef".to_vec()),
            sent: 0,
            pipe: Pipe {
                blocked: true,
                short_drain: failure == "drain",
                ..Pipe::default()
            },
        };
        let result = drive(
            &reactor,
            send(&Caller, &reactor, owner, connection, &observer),
        );
        let expected = match failure {
            "drain" => Failure::Runtime(uring_runtime::Error::Io),
            "scope" => Failure::Runtime(uring_runtime::Error::DeadlineExceeded),
            _ => Failure::Http(Error::Malformed),
        };
        assert!(matches!(result, Err(e) if e == expected));
        assert!(weak.upgrade().is_none());
        assert!(
            !observer
                .events
                .borrow()
                .iter()
                .any(|(name, _)| *name == "direct")
        );
    }
}

#[test]
fn interrupted_write_yields_and_empty_delivery_does_not_consult_scope() {
    let reactor = Rc::new(Reactor::new(16, ()));
    let (connection, _peer, _) = connection();
    let observer = Observe::default();
    let owner = Reader {
        bytes: Rc::new(b"abc".to_vec()),
        sent: 0,
        pipe: Pipe {
            interrupt_write: true,
            ..Pipe::default()
        },
    };
    let mut work = Box::pin(send(&Caller, &reactor, owner, connection, &observer));
    assert!(
        work.as_mut()
            .poll(&mut std::task::Context::from_waker(Waker::noop()))
            .is_pending()
    );
    assert_eq!(reactor.in_flight(), 0);
    let (owner, connection) = drive(&reactor, work).unwrap();
    assert_eq!(owner.sent, 3);
    observer.scope_error.set(true);
    let calls = observer.times.borrow().len();
    assert!(
        drive(
            &reactor,
            send(&Caller, &reactor, owner, connection, &observer)
        )
        .is_ok()
    );
    assert_eq!(observer.times.borrow().len(), calls);
}

#[test]
fn abandoned_owned_send_keeps_backing_and_connection_until_fence() {
    use std::os::fd::AsRawFd;
    let reactor = Rc::new(Reactor::new(16, ()));
    reactor.init().unwrap();
    let (connection, _peer, slot) = connection();
    let size: libc::c_int = 4096;
    // SAFETY: live socket and correctly sized option storage.
    assert_eq!(
        unsafe {
            libc::setsockopt(
                connection.socket().as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_SNDBUF,
                (&size as *const libc::c_int).cast(),
                std::mem::size_of_val(&size) as _,
            )
        },
        0
    );
    let bytes = Rc::new(vec![91; 512 * 1024]);
    let weak = Rc::downgrade(&bytes);
    let observer = Observe::default();
    let owner = Reader {
        bytes,
        sent: 0,
        pipe: Pipe {
            unsupported_after: Some(0),
            ..Pipe::default()
        },
    };
    let mut work = Box::pin(send(&Caller, &reactor, owner, connection, &observer));
    assert!(
        work.as_mut()
            .poll(&mut std::task::Context::from_waker(Waker::noop()))
            .is_pending()
    );
    assert_eq!(weak.strong_count(), 2, "reader and immutable send view");
    assert_eq!(reactor.in_flight(), 1);
    drop(work);
    assert!(slot.upgrade().is_some());
    assert_eq!(weak.strong_count(), 2);
    drive(&reactor, reactor.drain()).unwrap();
    assert!(weak.upgrade().is_none());
    assert!(slot.upgrade().is_none());
    assert!(
        !observer
            .events
            .borrow()
            .iter()
            .any(|(name, _)| *name == "after")
    );
}

#[test]
fn writable_pipe_yields_at_call_budget_and_retains_accepted_cursor() {
    let reactor = Rc::new(Reactor::new(16, ()));
    let (connection, mut peer, _) = connection();
    let observer = Observe::default();
    let owner = Reader {
        bytes: Rc::new(vec![7; 100]),
        sent: 0,
        pipe: Pipe::default(),
    };
    let mut work = Box::pin(send(&Caller, &reactor, owner, connection, &observer));
    assert!(
        work.as_mut()
            .poll(&mut std::task::Context::from_waker(Waker::noop()))
            .is_pending()
    );
    assert_eq!(observer.times.borrow().len(), TURN_CALLS);
    assert_eq!(
        reactor.in_flight(),
        0,
        "fairness yield does not submit readiness"
    );
    let mut prefix = [0; 64];
    peer.read_exact(&mut prefix).unwrap();
    assert_eq!(prefix, [7; 64]);
    let (owner, connection) = drive(&reactor, work).unwrap();
    assert_eq!(owner.sent, 100);
    drop(connection);
    let mut suffix = Vec::new();
    peer.read_to_end(&mut suffix).unwrap();
    assert_eq!(suffix, vec![7; 36]);
}
