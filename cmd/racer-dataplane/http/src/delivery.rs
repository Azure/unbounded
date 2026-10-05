//! Immutable-body delivery, distinct from opaque relay's readiness-driven profile.
//!
//! [`Owner`] retains immutable backing, the socket-accepted cursor, an independent
//! staging pipe, and stable owning send views. [`DeliveryPipe`] has a production
//! adapter for [`PipeLease`]; the HTTP context admits scratch storage and its
//! reactor submits owned sends. No executor or application policy is selected here.
//!
//! [`send`] stages into an empty pipe and splices its buffered suffix. Unsupported
//! splice resumes copying at the accepted cursor, not the staged cursor. On
//! backpressure, a pipe drain feeds one admitted buffer into a send retaining the
//! complete `(owner, connection)` tuple. Later sends reconstruct unsent bytes from
//! immutable views. The full owner remains retained through the completion fence.
//!
//! Interrupted operations yield and retry, including pipe drains, unlike
//! [`crate::relay`]. Partial or empty drains are terminal. Accepted progress resets
//! the stall clock; turns yield after 256 KiB or 32 successful calls, with 64 KiB
//! maximum chunks. [`Observer`] selects checked scopes and observes direct bytes,
//! drained pipes, and before/after-send boundaries without releasing owned resources.
//!
//! Callers validate identity, framing, and the socket, call `begin_io` before the
//! first poll, and consume HTTP framing after success. Authorization, deadline
//! arithmetic, telemetry, reservation release, and exchange finalization stay with
//! the caller. This engine neither authorizes bytes nor finishes exchanges.
use crate::{
    connection::{ConnectionLease, Context, OwnedBuffer, Result},
    splice_unsupported,
};
use flow_control::{Policy, pipe::PipeLease};
use std::{io, task::Poll, time::Instant};
use uring_runtime::reactor::{Descriptor, IoBuffer, SendBuffer};

/// Maximum immutable slice considered by one delivery operation.
pub const CHUNK_BYTES: usize = 64 * 1024;
/// Accepted bytes allowed before yielding a cooperative turn.
const TURN_BYTES: usize = 256 * 1024;
/// Successful calls allowed before yielding a cooperative turn.
const TURN_CALLS: usize = 32;

/// Nonblocking staging pipe. Writes/drains expose EINTR and EAGAIN; successful
/// counts must be bounded by the supplied slice or requested count.
pub trait DeliveryPipe: 'static {
    /// Return the exact staged suffix still held by the pipe.
    fn buffered(&self) -> usize;
    /// Stage bytes without blocking and return the accepted count.
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize>;
    /// Copy staged bytes out without blocking.
    fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize>;
    /// Splice up to `count` staged bytes into the socket without blocking.
    fn send(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize>;
}
impl<P: Policy> DeliveryPipe for PipeLease<P> {
    /// Report the production pipe's buffered suffix.
    fn buffered(&self) -> usize {
        self.buffered()
    }
    /// Stage bytes in the admitted kernel pipe.
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.try_write(bytes)
    }
    /// Drain the admitted kernel pipe into owned scratch storage.
    fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        self.try_read(bytes)
    }
    /// Splice staged bytes into the destination socket.
    fn send(&mut self, socket: &Descriptor, count: usize) -> io::Result<usize> {
        self.try_splice_descriptor(socket, count)
    }
}

/// The complete admitted reader: immutable bytes, a staging pipe, and the exact
/// socket-accepted cursor. Application identity/range validation precedes use.
pub trait Owner<C: Context>: 'static {
    /// Independently owned staging pipe.
    type Pipe: DeliveryPipe;
    /// Immutable view that retains its backing through runtime completion.
    type View: SendBuffer<Error = C::Error>;
    /// Return bytes not yet accepted by the socket.
    fn remaining(&self) -> usize;
    /// Advance only by a checked positive socket-accepted count.
    fn advance(&mut self, count: usize);
    /// Return immutable bytes at the accepted cursor and the independent pipe.
    /// Bytes must cover min(remaining, CHUNK_BYTES).
    fn parts(&mut self) -> (&[u8], &mut Self::Pipe);
    /// Stable owning view of `count` bytes at the accepted cursor. Must retain
    /// the full backing owner through its runtime completion fence.
    fn view(&self, count: usize) -> Result<C, Self::View>;
}

/// Caller policy and observation at the same boundaries as the send mechanism.
/// Callbacks may reject a send but must not release any completion-owned resource.
pub trait Observer<C: Context> {
    /// Select and check the next send scope, using last socket progress.
    fn scope(&self, stalled_at: Instant) -> Result<C, C::Scope>;
    /// Observe bytes accepted by a direct or runtime-owned send.
    fn direct_bytes(&self, _count: usize) {}
    /// Observe a complete pipe drain before submitting an owned send.
    fn pipe_drained(&self) {}
    /// Observe the next owned send and total unaccepted bytes.
    fn before_send(&self, _count: usize, _remaining: usize) {}
    /// Check a completed owned send before advancing the accepted cursor.
    fn after_send(&self, _count: usize, _accepted: usize) -> Result<C, ()> {
        Ok(())
    }
}

/// Retain either drained pipe bytes or an immutable backing view during a send.
enum Buffer<C: Context, V: SendBuffer<Error = C::Error>> {
    /// Admitted scratch populated by a complete pipe drain.
    Pipe(OwnedBuffer<C>),
    /// Owning view used after switching to copying.
    View(V),
}
// SAFETY: each variant retains stable immutable backing through completion.
unsafe impl<C: Context, V: SendBuffer<Error = C::Error>> SendBuffer for Buffer<C, V> {
    type Error = C::Error;
    /// Borrow stable bytes from the selected completion-owned buffer.
    fn send_bytes(&self) -> Result<C, &[u8]> {
        match self {
            Self::Pipe(buffer) => buffer.send_bytes(),
            Self::View(buffer) => buffer.send_bytes(),
        }
    }
}

/// Deliver the owner's remaining slice without finishing or consuming HTTP
/// framing. The caller validates framing/socket first and consumes framing only
/// after success. Every async send owns the complete (reader, connection) tuple.
/// The caller must invalidate connection reuse with begin_io before handing it
/// to the future, including abandonment before the first poll.
pub async fn send<C, O, H>(
    context: &C,
    reactor: &C::Reactor,
    mut owner: O,
    mut connection: ConnectionLease<C>,
    observer: &H,
) -> Result<C, (O, ConnectionLease<C>)>
where
    C: Context,
    O: Owner<C>,
    H: Observer<C>,
{
    let mut stalled_at = uring_runtime::environment::now();
    let mut copying = false;
    let mut budget = 0;
    let mut calls = 0;
    while owner.remaining() != 0 {
        let send_scope = observer.scope(stalled_at)?;
        let count = owner.remaining().min(CHUNK_BYTES);
        let result = (|| -> io::Result<usize> {
            let (bytes, pipe) = owner.parts();
            let bytes = &bytes[..count];
            if copying {
                connection.socket().try_send(bytes)
            } else if pipe.buffered() == 0 && pipe.write(bytes)? == 0 {
                Err(io::ErrorKind::WriteZero.into())
            } else {
                pipe.send(&connection.socket(), count)
            }
        })();
        let sent = match result {
            Ok(sent) => {
                if copying {
                    observer.direct_bytes(sent);
                }
                sent
            }
            Err(error) if !copying && splice_unsupported(&error) => {
                copying = true;
                continue;
            }
            Err(error) if error.kind() == io::ErrorKind::Interrupted => {
                yield_once().await;
                continue;
            }
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                let buffer = if copying {
                    Buffer::View(owner.view(owner.remaining().min(CHUNK_BYTES))?)
                } else {
                    let (_, pipe) = owner.parts();
                    let count = pipe.buffered();
                    let mut buffer = OwnedBuffer::new(context, count)?;
                    match pipe.drain(buffer.bytes_mut()?) {
                        Ok(read) if read == count && count != 0 => {}
                        Err(error) if error.kind() == io::ErrorKind::Interrupted => {
                            yield_once().await;
                            continue;
                        }
                        _ => return Err(uring_runtime::Error::Io.into()),
                    }
                    observer.pipe_drained();
                    Buffer::Pipe(buffer)
                };
                let count = buffer.send_bytes()?.len();
                observer.before_send(count, owner.remaining());
                let completion = reactor
                    .send(
                        connection.socket(),
                        buffer,
                        (owner, connection),
                        &send_scope,
                    )
                    .await?;
                (owner, connection) = completion.lease;
                observer.after_send(count, completion.bytes)?;
                if completion.bytes > count {
                    return Err(uring_runtime::Error::Io.into());
                }
                // Reconstruct any unsent suffix from immutable backing, not the
                // now-empty pipe. Subsequent backpressure owns immutable views.
                copying = true;
                observer.direct_bytes(completion.bytes);
                completion.bytes
            }
            Err(_) => return Err(uring_runtime::Error::Io.into()),
        };
        if sent == 0 || sent > owner.remaining() {
            return Err(uring_runtime::Error::Io.into());
        }
        owner.advance(sent);
        stalled_at = uring_runtime::environment::now();
        budget += sent;
        calls += 1;
        if (budget >= TURN_BYTES || calls >= TURN_CALLS) && owner.remaining() != 0 {
            yield_once().await;
            budget = 0;
            calls = 0;
        }
    }
    Ok((owner, connection))
}

/// Give the caller's executor one cooperative scheduling opportunity.
async fn yield_once() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if std::mem::replace(&mut yielded, true) {
            Poll::Ready(())
        } else {
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    })
    .await
}

/// Delivery cursor, callback, fairness, and completion-ownership regressions.
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{Error, connection::Endpoint};
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
    /// Always-live scope bounded by the test driver's watchdog.
    #[derive(Clone)]
    struct TestScope;
    impl Scope for TestScope {
        type Error = Failure;
        /// Keep fixture operations live while the driver runs.
        fn check(&self) -> std::result::Result<(), Failure> {
            Ok(())
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
    /// Permissive context with reference-counted connection admission.
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
        /// Admit scratch without a byte quota.
        fn charge(&self, _: usize) -> Result<Self, ()> {
            Ok(())
        }
        /// Reserve a slot whose lifetime can be observed through a weak reference.
        fn outbound_slot(&self) -> Result<Self, Rc<()>> {
            Ok(Rc::new(()))
        }
        /// Keep fixture admission open.
        fn stopped(&self) -> bool {
            false
        }
    }
    /// Script short sends, unsupported splice, backpressure, and drain failures.
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
        /// Report the scripted pipe's exact staged suffix.
        fn buffered(&self) -> usize {
            self.bytes.len()
        }
        /// Stage bytes after any one-shot injected interruption.
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            if std::mem::take(&mut self.interrupt_write) {
                return Err(io::ErrorKind::Interrupted.into());
            }
            self.bytes.extend_from_slice(bytes);
            Ok(bytes.len())
        }
        /// Drain staged bytes with optional interruption or short-count injection.
        fn drain(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
            if std::mem::take(&mut self.interrupt_drain) {
                return Err(io::ErrorKind::Interrupted.into());
            }
            let n = bytes.len().min(self.bytes.len()) - usize::from(self.short_drain);
            bytes[..n].copy_from_slice(&self.bytes[..n]);
            self.bytes.drain(..n);
            Ok(n)
        }
        /// Send at most two bytes unless fallback or backpressure is injected.
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
    /// Immutable body owner with independently staged and accepted cursors.
    struct Reader {
        bytes: Rc<Vec<u8>>,
        pipe: Pipe,
        sent: usize,
    }
    /// Owning immutable byte range retained through a runtime send.
    struct View {
        bytes: Rc<Vec<u8>>,
        range: std::ops::Range<usize>,
    }
    // SAFETY: the immutable Rc backing is retained and cannot be mutated through this view.
    unsafe impl SendBuffer for View {
        type Error = Failure;
        /// Borrow the immutable send range while retaining its backing owner.
        fn send_bytes(&self) -> Result<Caller, &[u8]> {
            Ok(&self.bytes[self.range.clone()])
        }
    }
    impl Owner<Caller> for Reader {
        type Pipe = Pipe;
        type View = View;
        /// Count bytes not yet accepted by the socket.
        fn remaining(&self) -> usize {
            self.bytes.len() - self.sent
        }
        /// Record socket acceptance independently of pipe staging.
        fn advance(&mut self, n: usize) {
            self.sent += n;
        }
        /// Borrow the unaccepted body and independent staging pipe.
        fn parts(&mut self) -> (&[u8], &mut Pipe) {
            (&self.bytes[self.sent..], &mut self.pipe)
        }
        /// Retain a stable range beginning at the accepted cursor.
        fn view(&self, count: usize) -> Result<Caller, View> {
            Ok(View {
                bytes: self.bytes.clone(),
                range: self.sent..self.sent + count,
            })
        }
    }
    /// Record callback order and inject scope or completion rejection.
    #[derive(Default)]
    struct Observe {
        events: RefCell<Vec<(&'static str, usize)>>,
        reject: Cell<bool>,
        scope_error: Cell<bool>,
        times: RefCell<Vec<Instant>>,
    }
    impl Observer<Caller> for Observe {
        /// Record the progress clock and optionally reject the next send scope.
        fn scope(&self, stalled_at: Instant) -> Result<Caller, TestScope> {
            self.times.borrow_mut().push(stalled_at);
            if self.scope_error.get() {
                return Err(uring_runtime::Error::DeadlineExceeded.into());
            }
            Ok(TestScope)
        }
        /// Record bytes accepted outside the splice path.
        fn direct_bytes(&self, count: usize) {
            self.events.borrow_mut().push(("direct", count));
        }
        /// Record a completed pipe drain.
        fn pipe_drained(&self) {
            self.events.borrow_mut().push(("drain", 0));
        }
        /// Record the owned send before its submission.
        fn before_send(&self, count: usize, _: usize) {
            self.events.borrow_mut().push(("before", count));
        }
        /// Record completion and optionally reject it before cursor advancement.
        fn after_send(&self, _: usize, count: usize) -> Result<Caller, ()> {
            self.events.borrow_mut().push(("after", count));
            if self.reject.get() {
                return Err(Error::Malformed.into());
            }
            Ok(())
        }
    }
    /// Reserve a socket pair with bounded peer reads and observable slot lifetime.
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
    /// Poll a future and its reactor under the delivery watchdog.
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

    /// Fallback must reconstruct the unaccepted body rather than replay staging.
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

    /// An interrupted drain yields without submission or resetting the stall clock.
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

    /// Terminal drain and policy failures release admission without recording progress.
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

    /// Interrupted staging yields, while an exhausted reader does not consult policy.
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

    /// Abandoning backpressured I/O retains backing and admission until its fence.
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

    /// Writable short sends yield at the call budget without submitting readiness.
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
}
