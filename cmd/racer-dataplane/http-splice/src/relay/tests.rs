use super::*;
use http1::{Codec, Error, connection::Endpoint};
use std::{
    cell::{Cell, RefCell},
    io::{Read, Write},
    os::unix::net::UnixStream,
    task::{Poll, Waker},
    time::{Duration, Instant},
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
struct Charge(Rc<Cell<usize>>, usize);
impl Drop for Charge {
    fn drop(&mut self) {
        self.0.set(self.0.get() - self.1);
    }
}
#[derive(Clone, PartialEq, Eq, PartialOrd, Ord)]
struct Key;
impl Endpoint<Failure> for Key {
    fn address(&self) -> std::result::Result<SocketAddress, Failure> {
        Ok(SocketAddress::Inet("127.0.0.1:9".parse().unwrap()))
    }
}
#[derive(Default)]
struct Hooks {
    bytes: Rc<Cell<usize>>,
    slots: Rc<Cell<usize>>,
    reject: Cell<bool>,
}
impl Context for Hooks {
    type Error = Failure;
    type Scope = TestScope;
    type Budget = ();
    type Reactor = Rc<Reactor<TestScope, ()>>;
    type Charge = Charge;
    type Slot = Charge;
    type Opaque = ();
    type State = ();
    type Endpoint = Key;
    fn charge(&self, n: usize) -> Result<Self, Charge> {
        if self.reject.get() {
            return Err(uring_runtime::Error::Overloaded.into());
        }
        self.bytes.set(self.bytes.get() + n);
        Ok(Charge(self.bytes.clone(), n))
    }
    fn outbound_slot(&self) -> Result<Self, Charge> {
        self.slots.set(self.slots.get() + 1);
        Ok(Charge(self.slots.clone(), 1))
    }
    fn stopped(&self) -> bool {
        false
    }
}

/// Bounded scripted pipe tests the adapter boundary without application types.
/// A successful receive buffers at most three bytes, and sends at most one.
struct Pipe {
    bytes: Vec<u8>,
    calls: Rc<Cell<usize>>,
    interrupted: bool,
    unsupported: bool,
    drain_error: bool,
    _owner: Rc<()>,
}
impl Default for Pipe {
    fn default() -> Self {
        Self {
            bytes: Vec::new(),
            calls: Rc::default(),
            interrupted: false,
            unsupported: false,
            drain_error: false,
            _owner: Rc::new(()),
        }
    }
}
impl RelayPipe for Pipe {
    fn buffered(&self) -> usize {
        self.bytes.len()
    }
    fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        if self.drain_error {
            return Err(io::ErrorKind::Interrupted.into());
        }
        let n = bytes.len().min(self.bytes.len());
        bytes[..n].copy_from_slice(&self.bytes[..n]);
        self.bytes.drain(..n);
        Ok(n)
    }
    fn receive(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize> {
        self.calls.set(self.calls.get() + 1);
        if self.interrupted {
            return Err(io::ErrorKind::Interrupted.into());
        }
        let mut bytes = [0; 3];
        let n = socket.try_recv(&mut bytes[..count.min(3)])?;
        self.bytes.extend_from_slice(&bytes[..n]);
        Ok(n)
    }
    fn send(&mut self, socket: &Descriptor) -> io::Result<usize> {
        if self.unsupported {
            return Err(io::Error::from_raw_os_error(libc::EOPNOTSUPP));
        }
        let n = socket.try_send(&self.bytes[..1])?;
        self.bytes.drain(..n);
        Ok(n)
    }
}
struct Fixture {
    hooks: Rc<Hooks>,
    io: HttpIo<Hooks>,
}
impl Fixture {
    fn new() -> Self {
        let hooks = Rc::new(Hooks::default());
        let io = HttpIo::new(
            Rc::new(Rc::new(Reactor::new(16, ()))),
            Codec::new(128),
            hooks.clone(),
            64,
            64,
        );
        Self { hooks, io }
    }
    fn connection(&self, receive: u64, send: u64) -> (ConnectionLease<Hooks>, UnixStream) {
        let (fd, peer) = UnixStream::pair().unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
        let mut c =
            ConnectionLease::from_reserved(fd.into(), self.hooks.outbound_slot().unwrap(), ())
                .unwrap();
        c.set_framing(Some(receive), Some(send), false);
        (c, peer)
    }
    fn relay(&self, length: u64, pipe: Pipe) -> (Relay<Hooks, Pipe>, UnixStream, UnixStream) {
        let (source, writer) = self.connection(length, 0);
        let (destination, reader) = self.connection(0, length);
        (
            Relay::new(source, destination, pipe).unwrap(),
            writer,
            reader,
        )
    }
}

#[test]
fn partial_pipe_sends_and_buffered_fallback_preserve_exact_cursor() {
    for fallback in [false, true] {
        let f = Fixture::new();
        let pipe = Pipe {
            unsupported: fallback,
            ..Pipe::default()
        };
        let (mut relay, mut writer, mut reader) = f.relay(9, pipe);
        writer.write_all(b"abcdefghi").unwrap();
        assert!(matches!(relay.step(&f.io).unwrap(), Step::Complete));
        let mut bytes = [0; 9];
        reader.read_exact(&mut bytes).unwrap();
        assert_eq!(&bytes, b"abcdefghi");
        assert_eq!(relay.source.receive_remaining(), Some(0));
        assert_eq!(relay.destination.send_remaining(), Some(0));
        assert_eq!(relay.copied, fallback);
        let (source, destination) = relay.connections_mut();
        assert!(!source.is_reusable() && !destination.is_reusable());
        source.finish_exchange().unwrap();
        destination.finish_exchange().unwrap();
    }
}

#[test]
fn ahead_excess_is_retained_and_reuse_rejected() {
    let f = Fixture::new();
    let (mut relay, _writer, mut reader) = f.relay(3, Pipe::default());
    let ahead = OwnedBuffer::copy_from(f.hooks.as_ref(), b"abcEXCESS").unwrap();
    relay.source.restore_read_ahead(ahead, 0..9).unwrap();
    assert!(matches!(relay.step(&f.io).unwrap(), Step::Complete));
    let mut bytes = [0; 3];
    reader.read_exact(&mut bytes).unwrap();
    assert_eq!(&bytes, b"abc");
    assert!(relay.source.closing());
    assert_eq!(
        relay.source.finish_exchange(),
        Err(Failure::Http(Error::Malformed))
    );
    let (ahead, range) = relay.source.take_read_ahead().unwrap();
    assert_eq!(&ahead.bytes().unwrap()[range], b"EXCESS");
}

#[test]
fn interrupted_turn_is_bounded_and_eagain_returns_receive_readiness() {
    let f = Fixture::new();
    let pipe = Pipe {
        interrupted: true,
        ..Pipe::default()
    };
    let calls = pipe.calls.clone();
    let (mut relay, _writer, _reader) = f.relay(3, pipe);
    assert!(matches!(relay.step(&f.io).unwrap(), Step::Yield));
    assert_eq!(calls.get(), 32);
    relay.pipe.interrupted = false;
    assert!(matches!(
        relay.step(&f.io).unwrap(),
        Step::Readiness {
            interest: libc::POLLIN,
            ..
        }
    ));
    assert_eq!(relay.source.receive_remaining(), Some(3));
}

#[test]
fn allocation_failure_eof_and_pipe_drain_errors_stay_terminal() {
    for failure in ["allocation", "eof", "drain"] {
        let f = Fixture::new();
        let (mut relay, mut writer, _reader) = f.relay(3, Pipe::default());
        if failure == "eof" {
            drop(writer);
        } else {
            writer.write_all(b"abc").unwrap();
            relay.pipe.unsupported = true;
            relay.pipe.drain_error = failure == "drain";
            f.hooks.reject.set(failure == "allocation");
        }
        let expected = if failure == "allocation" {
            uring_runtime::Error::Overloaded
        } else {
            uring_runtime::Error::Io
        };
        assert!(matches!(relay.step(&f.io), Err(Failure::Runtime(e)) if e == expected));
        assert!(!relay.source.is_reusable() && !relay.destination.is_reusable());
        drop(relay);
        f.io.reclaim_buffer();
        assert_eq!(f.hooks.slots.get(), 0);
        assert_eq!(f.hooks.bytes.get(), 0);
    }
}

#[test]
fn rejects_mismatched_or_missing_framing_and_accepts_empty_body() {
    let f = Fixture::new();
    let (source, _writer) = f.connection(3, 0);
    let (destination, _reader) = f.connection(0, 2);
    assert!(matches!(
        Relay::new(source, destination, Pipe::default()),
        Err(Failure::Http(Error::Malformed))
    ));
    let (mut source, _writer) = f.connection(0, 0);
    source.set_framing(None, Some(0), false);
    let (destination, _reader) = f.connection(0, 0);
    assert!(matches!(
        Relay::new(source, destination, Pipe::default()),
        Err(Failure::Http(Error::Malformed))
    ));
    let (mut relay, _writer, _reader) = f.relay(0, Pipe::default());
    assert!(matches!(relay.step(&f.io).unwrap(), Step::Complete));
    assert_eq!(relay.pipe.calls.get(), 0);
}

#[test]
fn readiness_abandonment_fences_both_slots_pipe_and_scratch() {
    let f = Fixture::new();
    f.io.reactor().init().unwrap();
    let pipe = Pipe::default();
    let weak = Rc::downgrade(&pipe._owner);
    let (mut relay, _writer, _reader) = f.relay(3, pipe);
    relay.force_fallback(true, None);
    let Step::Readiness { socket, interest } = relay.step(&f.io).unwrap() else {
        panic!("must wait")
    };
    let owner = Rc::new(RefCell::new(relay));
    let mut wait =
        f.io.reactor()
            .readiness_with_lease(socket, interest as u32, owner.clone(), &TestScope);
    let mut cx = std::task::Context::from_waker(Waker::noop());
    assert!(wait.as_mut().poll(&mut cx).is_pending());
    drop(wait);
    drop(owner);
    assert_eq!(f.hooks.slots.get(), 2);
    assert_eq!(f.hooks.bytes.get(), MAX_PIPE_BYTES);
    assert!(weak.upgrade().is_some());
    let mut drain = f.io.reactor().drain();
    let until = Instant::now() + Duration::from_secs(2);
    loop {
        if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
            result.unwrap();
            break;
        }
        assert!(Instant::now() < until, "drain watchdog");
        f.io.reactor().poll_budgeted(32).unwrap();
    }
    f.io.reclaim_buffer();
    assert_eq!(f.hooks.slots.get(), 0);
    assert_eq!(f.hooks.bytes.get(), 0);
    assert!(weak.upgrade().is_none());
}
