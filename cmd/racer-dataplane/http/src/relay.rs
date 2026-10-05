//! Bounded synchronous transfer of already validated, fixed-length HTTP bodies.
//!
//! [`Relay`] combines the HTTP context's accounting and connections with a small
//! [`RelayPipe`] adapter, implemented for [`PipeLease`]. It does not select an
//! executor, telemetry, authentication, quota class, or application protocol.
//!
//! Each [`Relay::step`] performs at most 32 nonblocking actions. Read-ahead and
//! socket-to-pipe-to-socket splice preserve the accepted prefix. Unsupported splice
//! falls back to one lazy 64 KiB admitted HTTP buffer, draining any pipe suffix
//! before copied receives resume. Excess read-ahead is restored so finalization
//! still rejects pipelining. EINTR consumes a turn; EAGAIN returns readiness. EOF
//! and drain errors are terminal, unlike [`crate::delivery`]'s drain retry policy.
//!
//! The caller checks its scope, holds the complete engine in `Rc<RefCell<_>>`, and
//! passes that owner to the runtime's `readiness_with_lease`. A readiness descriptor
//! alone does not retain connections, pipe, or buffer accounting. Cancellation or
//! abandonment must fence the wait before those owners can be released. The engine
//! submits no asynchronous I/O itself: deadline clipping, retry, cooperative
//! yielding, exchange finish/poison ordering, and reservation release are caller
//! policy. The `test-util` feature adds deterministic fallback injection; production
//! unsupported errors trigger fallback without that feature.
use crate::{
    connection::{ConnectionLease, Context, HttpIo, OwnedBuffer, Result},
    splice_unsupported,
};
use flow_control::{
    Policy,
    pipe::{MAX_PIPE_BYTES, PipeLease},
};
use std::{io, ops::Range, rc::Rc};
use uring_runtime::reactor::{Descriptor, IoBuffer};

/// Synchronous, nonblocking pipe operations. Successful counts must not exceed
/// the requested bytes or buffered bytes; buffered() tracks the exact suffix.
/// Implementations own the pipe and any admission for its entire lifetime.
pub trait RelayPipe: 'static {
    /// Return the exact suffix still buffered in the pipe.
    fn buffered(&self) -> usize;
    /// Copy buffered bytes out without blocking.
    fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize>;
    /// Splice up to `count` source bytes into the pipe without blocking.
    fn receive(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize>;
    /// Splice the buffered suffix into the destination without blocking.
    fn send(&mut self, socket: &Descriptor) -> io::Result<usize>;
}
impl<P: Policy> RelayPipe for PipeLease<P> {
    /// Report the production pipe's buffered suffix.
    fn buffered(&self) -> usize {
        self.buffered()
    }
    /// Drain admitted pipe bytes into owned fallback storage.
    fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        self.try_read(bytes)
    }
    /// Receive source bytes into the admitted kernel pipe.
    fn receive(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize> {
        self.try_splice_from(socket, count)
    }
    /// Splice the pipe's buffered suffix into the destination socket.
    fn send(&mut self, socket: &Descriptor) -> io::Result<usize> {
        self.try_splice_connection(socket)
    }
}

/// A readiness descriptor is not the owner of the transfer. Any submitted wait
/// must retain the complete Relay as its runtime lease through the final fence.
pub enum Step {
    /// Both body cursors are exhausted; exchange finalization remains caller-owned.
    Complete,
    /// The bounded turn ended and the caller should yield cooperatively.
    Yield,
    /// Wait for readiness while retaining the complete relay owner.
    Readiness {
        /// Socket to poll, not a substitute for the relay's resource ownership.
        socket: Rc<Descriptor>,
        /// Poll interest selected by the direction that blocked.
        interest: i16,
    },
}

/// Exact opaque HTTP body transit. Does not finish exchanges or select deadlines.
pub struct Relay<C: Context, P: RelayPipe> {
    /// Source exchange and its receive cursor.
    source: ConnectionLease<C>,
    /// Destination exchange and its accepted send cursor.
    destination: ConnectionLease<C>,
    /// Admitted pipe retaining any staged suffix.
    pipe: P,
    /// Lazily admitted scratch for read-ahead and copying fallback.
    fallback: Option<OwnedBuffer<C>>,
    /// Unsent bytes in fallback storage.
    pending: Range<usize>,
    /// Whether subsequent transfers bypass kernel splice.
    copied: bool,
    /// Optional remaining-byte threshold for deterministic fallback tests.
    #[cfg(any(test, feature = "test-util"))]
    fallback_at: Option<usize>,
}
impl<C: Context, P: RelayPipe> Relay<C, P> {
    /// Require equal known body lengths. The caller admits an empty pipe and
    /// validates any application envelope before constructing a nonempty relay.
    pub fn new(
        source: ConnectionLease<C>,
        destination: ConnectionLease<C>,
        pipe: P,
    ) -> Result<C, Self> {
        if source.receive_remaining().is_none()
            || source.receive_remaining() != destination.send_remaining()
        {
            return Err(crate::Error::Malformed.into());
        }
        Ok(Self {
            source,
            destination,
            pipe,
            fallback: None,
            pending: 0..0,
            copied: false,
            #[cfg(any(test, feature = "test-util"))]
            fallback_at: None,
        })
    }

    /// Inject copying immediately or once the remaining body reaches a threshold.
    #[cfg(any(test, feature = "test-util"))]
    pub fn force_fallback(&mut self, copied: bool, at_remaining: Option<usize>) {
        self.copied = copied;
        self.fallback_at = at_remaining;
    }

    /// Run at most 32 nonblocking actions, retaining pending pipe/copy suffixes.
    /// The caller checks its scope before each step and before finalization.
    pub fn step(&mut self, io: &HttpIo<C>) -> Result<C, Step> {
        if self.destination.socket().peer_read_closed() {
            return Err(uring_runtime::Error::Io.into());
        }
        let mut wait = Step::Yield;
        for _ in 0..32 {
            let remaining = self
                .source
                .receive_remaining()
                .ok_or(crate::Error::Malformed)? as usize;
            if self.pending.is_empty() && self.pipe.buffered() == 0 && remaining == 0 {
                break;
            }
            let writing = !self.pending.is_empty() || self.pipe.buffered() != 0;
            let result = if !self.pending.is_empty() {
                self.destination
                    .socket()
                    .try_send(&self.fallback.as_ref().unwrap().bytes()?[self.pending.clone()])
            } else if self.pipe.buffered() != 0 {
                #[cfg(any(test, feature = "test-util"))]
                if self
                    .fallback_at
                    .is_some_and(|threshold| remaining <= threshold)
                    && !self.copied
                {
                    self.copied = true;
                }
                if self.copied {
                    if self.fallback.is_none() {
                        self.fallback = Some(io.buffer(MAX_PIPE_BYTES)?);
                    }
                    let n = self
                        .pipe
                        .drain(self.fallback.as_mut().unwrap().bytes_mut()?)
                        .map_err(|_| uring_runtime::Error::Io)?;
                    self.pending = 0..n;
                    continue;
                }
                self.pipe.send(&self.destination.socket())
            } else if let Some((ahead, range)) = self.source.take_read_ahead() {
                let count = remaining.min(range.len()).min(MAX_PIPE_BYTES);
                if self.fallback.is_none() {
                    self.fallback = Some(io.buffer(MAX_PIPE_BYTES)?);
                }
                self.fallback.as_mut().unwrap().bytes_mut()?[..count]
                    .copy_from_slice(&ahead.bytes()?[range.start..range.start + count]);
                if range.len() > count {
                    self.source
                        .restore_read_ahead(ahead, range.start + count..range.end)?;
                }
                self.pending = 0..count;
                self.source.consume_received(count)?;
                continue;
            } else if self.copied {
                if self.fallback.is_none() {
                    self.fallback = Some(io.buffer(MAX_PIPE_BYTES)?);
                }
                self.source.socket().try_recv(
                    &mut self.fallback.as_mut().unwrap().bytes_mut()?
                        [..remaining.min(MAX_PIPE_BYTES)],
                )
            } else {
                self.pipe.receive(&self.source.socket(), remaining)
            };
            match result {
                Ok(0) => return Err(uring_runtime::Error::Io.into()),
                Ok(n) => {
                    if writing {
                        if !self.pending.is_empty() {
                            self.pending.start += n;
                        }
                        self.destination.consume_sent(n)?;
                    } else {
                        self.source.consume_received(n)?;
                        if self.copied {
                            self.pending = 0..n;
                        }
                    }
                }
                Err(error) if splice_unsupported(&error) => self.copied = true,
                Err(error) if error.kind() == io::ErrorKind::Interrupted => (),
                Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                    wait = Step::Readiness {
                        socket: if writing {
                            self.destination.socket()
                        } else {
                            self.source.socket()
                        },
                        interest: if writing { libc::POLLOUT } else { libc::POLLIN },
                    };
                    break;
                }
                Err(_) => return Err(uring_runtime::Error::Io.into()),
            }
        }
        if self.source.receive_remaining() == Some(0)
            && self.destination.send_remaining() == Some(0)
        {
            Ok(Step::Complete)
        } else {
            Ok(wait)
        }
    }

    /// Finalize both connections while pipe and scratch owners remain retained.
    pub fn connections_mut(&mut self) -> (&mut ConnectionLease<C>, &mut ConnectionLease<C>) {
        (&mut self.source, &mut self.destination)
    }

    /// Return the destination without marking it reusable. Drop the source before
    /// pipe/scratch owners, after the caller's completion and poison policy.
    pub fn into_destination(self) -> ConnectionLease<C> {
        self.destination
    }
}
/// Relay framing, fallback, fairness, and readiness-ownership regressions.
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{Codec, Error, connection::Endpoint};
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

    /// Keep HTTP and runtime failures distinct in assertions.
    #[derive(Clone, Copy, Debug, PartialEq)]
    enum Failure {
        Http(Error),
        Runtime(uring_runtime::Error),
    }
    impl From<Error> for Failure {
        /// Preserve an HTTP failure without reclassification.
        fn from(e: Error) -> Self {
            Self::Http(e)
        }
    }
    impl From<uring_runtime::Error> for Failure {
        /// Preserve a runtime failure without reclassification.
        fn from(e: uring_runtime::Error) -> Self {
            Self::Runtime(e)
        }
    }
    /// Always-live scope for explicitly driven readiness tests.
    #[derive(Clone)]
    struct TestScope;
    impl Scope for TestScope {
        type Error = Failure;
        /// Keep fixture operations live while the driver runs.
        fn check(&self) -> std::result::Result<(), Failure> {
            Ok(())
        }
    }
    /// Counted admission returned to its ledger on drop.
    struct Charge(Rc<Cell<usize>>, usize);
    impl Drop for Charge {
        /// Release the fixture's retained byte or slot charge.
        fn drop(&mut self) {
            self.0.set(self.0.get() - self.1);
        }
    }
    /// Single endpoint key for local socket fixtures.
    #[derive(Clone, PartialEq, Eq, PartialOrd, Ord)]
    struct Key;
    impl Endpoint<Failure> for Key {
        /// Supply a loopback address without resolving names.
        fn address(&self) -> std::result::Result<SocketAddress, Failure> {
            Ok(SocketAddress::Inet("127.0.0.1:9".parse().unwrap()))
        }
    }
    /// Observable byte and slot admission with injectable allocation rejection.
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
        /// Reserve counted bytes unless overload has been injected.
        fn charge(&self, n: usize) -> Result<Self, Charge> {
            if self.reject.get() {
                return Err(uring_runtime::Error::Overloaded.into());
            }
            self.bytes.set(self.bytes.get() + n);
            Ok(Charge(self.bytes.clone(), n))
        }
        /// Reserve one counted connection slot.
        fn outbound_slot(&self) -> Result<Self, Charge> {
            self.slots.set(self.slots.get() + 1);
            Ok(Charge(self.slots.clone(), 1))
        }
        /// Keep fixture admission open.
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
        /// Start with an empty pipe and no injected failures.
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
        /// Report the scripted pipe's exact staged suffix.
        fn buffered(&self) -> usize {
            self.bytes.len()
        }
        /// Drain staged bytes unless a terminal drain error has been injected.
        fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
            if self.drain_error {
                return Err(io::ErrorKind::Interrupted.into());
            }
            let n = bytes.len().min(self.bytes.len());
            bytes[..n].copy_from_slice(&self.bytes[..n]);
            self.bytes.drain(..n);
            Ok(n)
        }
        /// Receive at most three bytes and count attempts, including interruptions.
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
        /// Send one byte unless unsupported splice has been injected.
        fn send(&mut self, socket: &Descriptor) -> io::Result<usize> {
            if self.unsupported {
                return Err(io::Error::from_raw_os_error(libc::EOPNOTSUPP));
            }
            let n = socket.try_send(&self.bytes[..1])?;
            self.bytes.drain(..n);
            Ok(n)
        }
    }
    /// Small HTTP I/O fixture with independently observable admission ledgers.
    struct Fixture {
        hooks: Rc<Hooks>,
        io: HttpIo<Hooks>,
    }
    impl Fixture {
        /// Create a small reactor and fixed HTTP storage limits.
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
        /// Reserve a socket pair and install the requested synthetic framing.
        fn connection(&self, receive: u64, send: u64) -> (ConnectionLease<Hooks>, UnixStream) {
            let (fd, peer) = UnixStream::pair().unwrap();
            peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
            let mut c =
                ConnectionLease::from_reserved(fd.into(), self.hooks.outbound_slot().unwrap(), ())
                    .unwrap();
            c.set_framing(Some(receive), Some(send), false);
            (c, peer)
        }
        /// Construct a relay with matching framing and both peer endpoints.
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

    /// Short splice sends and buffered fallback preserve exact wire bytes and framing.
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

    /// Excess read-ahead survives forwarding and prevents connection reuse.
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

    /// Repeated interruption consumes a bounded turn; backpressure selects read readiness.
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

    /// Allocation, EOF, and drain failures stay terminal and release all admission.
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

    /// Relays require matching known framing, but empty bodies need no pipe actions.
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

    /// Abandoned readiness retains both connections, pipe, and scratch until its fence.
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
}
