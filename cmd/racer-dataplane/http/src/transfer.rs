//! Body transfer engines with independent ownership and progress contracts.
//!
//! Delivery retains immutable backing through owned sends; relay retains both
//! connections through caller-owned readiness waits. They share pipe adapters,
//! not cancellation, drain, scheduling, or exchange-finalization policy.

pub mod delivery {
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

    use crate::connection::{ConnectionLease, Context, OwnedBuffer, Result};
    use flow_control::{PipeLease, Policy, splice_unsupported};
    use std::{io, task::Poll, time::Instant};
    use uring_runtime::reactor::{IoBuffer, SendBuffer, descriptor::Descriptor};

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
        /// Bytes must cover min(remaining, 64 KiB).
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

    /// Maximum immutable slice considered by one delivery operation.
    const CHUNK_BYTES: usize = 64 * 1024;

    /// Accepted bytes allowed before yielding a cooperative turn.
    const TURN_BYTES: usize = 256 * 1024;

    /// Successful calls allowed before yielding a cooperative turn.
    const TURN_CALLS: usize = 32;

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
        use super::super::fixtures::{Failure, Key, TestScope};
        use super::*;
        use crate::Error;
        use std::{
            cell::{Cell, RefCell},
            future::Future,
            io::Read,
            os::unix::net::UnixStream,
            rc::Rc,
            task::Waker,
            time::Duration,
        };
        use uring_runtime::reactor::Reactor;

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
}

pub mod relay {
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
    //! [`Relay::run`] retains the complete engine through readiness fences and yields
    //! between bounded turns. Callers supply deadline clipping and timeout policy.
    //! [`finish`] handles paired exchange completion and poisoning; application
    //! reservation release remains with the caller. The `test-util` feature adds deterministic fallback injection; production
    //! unsupported errors trigger fallback without that feature.

    use crate::connection::{ConnectionLease, Context, HttpIo, OwnedBuffer, Result};
    use flow_control::{MAX_PIPE_BYTES, PipeLease, Policy, splice_unsupported};
    use std::{io, ops::Range, rc::Rc};
    use uring_runtime::reactor::{IoBuffer, descriptor::Descriptor};

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

    impl<C: Context, P: RelayPipe> Relay<C, P> {
        /// Drive bounded turns, retaining all resources through every readiness CQE.
        /// `tick` narrows the caller scope; `retry` identifies an ignorable tick timeout.
        pub async fn run(
            self,
            io: &HttpIo<C>,
            scope: &C::Scope,
            tick: impl Fn() -> C::Scope,
            retry: impl Fn(C::Error) -> bool,
        ) -> Result<C, Self> {
            use std::{cell::RefCell, task::Poll};
            use uring_runtime::Scope;
            let state = Rc::new(RefCell::new(self));
            loop {
                scope.check()?;
                let step = state.borrow_mut().step(io)?;
                match step {
                    Step::Complete => break,
                    Step::Readiness { socket, interest } => {
                        match io
                            .reactor()
                            .readiness_with_lease(socket, interest as u32, state.clone(), &tick())
                            .await
                        {
                            Ok(_) => (),
                            Err(error) if retry(error) => (),
                            Err(error) => return Err(error),
                        }
                    }
                    Step::Yield => {
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
                }
            }
            scope.check()?;
            Ok(Rc::try_unwrap(state)
                .map_err(|_| uring_runtime::Error::InvalidInput)?
                .into_inner())
        }

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
            // A requester may finish writing while still reading its response.
            // Reject only full disconnect here; sends report other write failures.
            if self.destination.socket().peer_disconnected() {
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
                        let buffer = fallback_buffer(&mut self.fallback, io)?;
                        let n = self
                            .pipe
                            .drain(buffer.bytes_mut()?)
                            .map_err(|_| uring_runtime::Error::Io)?;
                        self.pending = 0..n;
                        continue;
                    }
                    self.pipe.send(&self.destination.socket())
                } else if let Some((ahead, range)) = self.source.take_read_ahead() {
                    let count = remaining.min(range.len()).min(MAX_PIPE_BYTES);
                    fallback_buffer(&mut self.fallback, io)?.bytes_mut()?[..count]
                        .copy_from_slice(&ahead.bytes()?[range.start..range.start + count]);
                    if range.len() > count {
                        self.source
                            .restore_read_ahead(ahead, range.start + count..range.end)?;
                    }
                    self.pending = 0..count;
                    self.source.consume_received(count)?;
                    continue;
                } else if self.copied {
                    let buffer = fallback_buffer(&mut self.fallback, io)?;
                    self.source
                        .socket()
                        .try_recv(&mut buffer.bytes_mut()?[..remaining.min(MAX_PIPE_BYTES)])
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

    /// Finish both HTTP exchanges, poisoning both connections on either failure.
    pub fn finish<C: Context>(
        source: &mut ConnectionLease<C>,
        destination: &mut ConnectionLease<C>,
    ) -> Result<C, ()> {
        if let Err(error) = source
            .finish_exchange()
            .and_then(|()| destination.finish_exchange())
        {
            source.poison();
            destination.poison();
            return Err(error);
        }
        Ok(())
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

    /// Admit scratch only when the selected drain, read-ahead, or copy path needs it.
    fn fallback_buffer<'a, C: Context>(
        fallback: &'a mut Option<OwnedBuffer<C>>,
        io: &HttpIo<C>,
    ) -> Result<C, &'a mut OwnedBuffer<C>> {
        if fallback.is_none() {
            *fallback = Some(io.buffer(MAX_PIPE_BYTES)?);
        }
        Ok(fallback.as_mut().unwrap())
    }

    /// Relay framing, fallback, fairness, and readiness-ownership regressions.
    #[cfg(test)]
    mod tests {
        use super::super::fixtures::{Failure, Key, TestScope};
        use super::*;
        use crate::{Codec, Error};
        use std::{
            cell::{Cell, RefCell},
            io::{Read, Write},
            os::unix::net::UnixStream,
            task::{Poll, Waker},
            time::{Duration, Instant},
        };
        use uring_runtime::reactor::Reactor;

        /// Counted admission returned to its ledger on drop.
        struct Charge(Rc<Cell<usize>>, usize);

        impl Drop for Charge {
            /// Release the fixture's retained byte or slot charge.
            fn drop(&mut self) {
                self.0.set(self.0.get() - self.1);
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
                let mut c = ConnectionLease::from_reserved(
                    fd.into(),
                    self.hooks.outbound_slot().unwrap(),
                    (),
                )
                .unwrap();
                c.set_framing(Some(receive), Some(send), false);
                (c, peer)
            }

            /// Construct a relay with matching framing and both peer endpoints.
            fn relay(
                &self,
                length: u64,
                pipe: Pipe,
            ) -> (Relay<Hooks, Pipe>, UnixStream, UnixStream) {
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

        /// A write-half-closed requester still receives the complete response body.
        #[test]
        fn relay_destination_half_close_preserves_response_and_full_close_fails() {
            for copied in [false, true] {
                let f = Fixture::new();
                let (mut relay, mut writer, mut reader) = f.relay(9, Pipe::default());
                relay.force_fallback(copied, None);
                reader.shutdown(std::net::Shutdown::Write).unwrap();
                assert!(relay.destination.socket().peer_read_closed());
                assert!(!relay.destination.socket().peer_disconnected());
                writer.write_all(b"abcdefghi").unwrap();
                assert!(matches!(relay.step(&f.io).unwrap(), Step::Complete));
                let mut bytes = [0; 9];
                reader.read_exact(&mut bytes).unwrap();
                assert_eq!(&bytes, b"abcdefghi");
                assert_eq!(relay.source.receive_remaining(), Some(0));
                assert_eq!(relay.destination.send_remaining(), Some(0));
                drop(relay);
                f.io.reclaim_buffer();
                assert_eq!(f.hooks.slots.get(), 0);
                assert_eq!(f.hooks.bytes.get(), 0);

                let (mut relay, mut writer, reader) = f.relay(3, Pipe::default());
                relay.force_fallback(copied, None);
                writer.write_all(b"abc").unwrap();
                drop(reader);
                assert!(matches!(
                    relay.step(&f.io),
                    Err(Failure::Runtime(uring_runtime::Error::Io))
                ));
                assert_eq!(relay.destination.send_remaining(), Some(3));
            }
        }

        /// The asynchronous runner retains readiness owners and finishes both cursors.
        #[test]
        fn runner_waits_then_finishes_and_abandonment_retains_pipe_until_fence() {
            for abandon in [false, true] {
                let f = Fixture::new();
                f.io.reactor().init().unwrap();
                let pipe = Pipe::default();
                let weak = Rc::downgrade(&pipe._owner);
                let (relay, mut writer, mut reader) = f.relay(3, pipe);
                let mut run = Box::pin(relay.run(&f.io, &TestScope, || TestScope, |_| false));
                let mut cx = std::task::Context::from_waker(Waker::noop());
                assert!(run.as_mut().poll(&mut cx).is_pending());
                if abandon {
                    drop(run);
                    assert!(weak.upgrade().is_some());
                    assert_eq!(f.hooks.slots.get(), 2);
                } else {
                    writer.write_all(b"abc").unwrap();
                    let until = Instant::now() + Duration::from_secs(2);
                    let mut relay = loop {
                        f.io.reactor().poll_budgeted(32).unwrap();
                        if let Poll::Ready(result) = run.as_mut().poll(&mut cx) {
                            break result.unwrap();
                        }
                        assert!(Instant::now() < until, "relay watchdog");
                    };
                    let mut bytes = [0; 3];
                    reader.read_exact(&mut bytes).unwrap();
                    assert_eq!(&bytes, b"abc");
                    let (source, destination) = relay.connections_mut();
                    finish(source, destination).unwrap();
                    assert!(source.is_reusable() && destination.is_reusable());
                    drop((run, relay));
                }
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
                assert!(weak.upgrade().is_none());
                assert_eq!(f.hooks.slots.get(), 0);
            }
        }

        /// A failed source finish poisons a destination that otherwise could be reused.
        #[test]
        fn paired_finish_poisons_both_on_unfinished_body() {
            let f = Fixture::new();
            let (mut relay, _writer, _reader) = f.relay(1, Pipe::default());
            let (source, destination) = relay.connections_mut();
            assert!(finish(source, destination).is_err());
            assert!(source.closing() && destination.closing());
            assert!(!source.is_reusable() && !destination.is_reusable());
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

        /// Scratch stays lazy on splice waits and is reused across read-ahead and copying.
        #[test]
        fn fallback_admission_stays_lazy_and_reuses_read_ahead_storage() {
            let f = Fixture::new();
            let (mut relay, mut writer, mut reader) = f.relay(6, Pipe::default());
            f.hooks.reject.set(true);
            assert!(matches!(relay.step(&f.io).unwrap(), Step::Readiness { .. }));
            assert!(relay.fallback.is_none());
            assert_eq!(f.hooks.bytes.get(), 0);

            f.hooks.reject.set(false);
            let ahead = OwnedBuffer::copy_from(f.hooks.as_ref(), b"abc").unwrap();
            relay.source.restore_read_ahead(ahead, 0..3).unwrap();
            assert!(matches!(relay.step(&f.io).unwrap(), Step::Readiness { .. }));
            assert_eq!(f.hooks.bytes.get(), MAX_PIPE_BYTES);
            let mut prefix = [0; 3];
            reader.read_exact(&mut prefix).unwrap();
            assert_eq!(&prefix, b"abc");

            // Copy fallback must reuse read-ahead scratch even when admission closes.
            f.hooks.reject.set(true);
            relay.force_fallback(true, None);
            writer.write_all(b"def").unwrap();
            assert!(matches!(relay.step(&f.io).unwrap(), Step::Complete));
            let mut suffix = [0; 3];
            reader.read_exact(&mut suffix).unwrap();
            assert_eq!(&suffix, b"def");
            assert_eq!(f.hooks.bytes.get(), MAX_PIPE_BYTES);
            drop(relay);
            f.io.reclaim_buffer();
            assert_eq!(f.hooks.bytes.get(), 0);

            // Read-ahead still attempts admission at its original branch boundary.
            let (mut relay, _writer, _reader) = f.relay(3, Pipe::default());
            f.hooks.reject.set(false);
            let ahead = OwnedBuffer::copy_from(f.hooks.as_ref(), b"abc").unwrap();
            relay.source.restore_read_ahead(ahead, 0..3).unwrap();
            f.hooks.reject.set(true);
            assert!(matches!(
                relay.step(&f.io),
                Err(Failure::Runtime(uring_runtime::Error::Overloaded))
            ));
            assert!(relay.fallback.is_none());
            assert_eq!(relay.source.receive_remaining(), Some(3));
            assert_eq!(relay.destination.send_remaining(), Some(3));
            drop(relay);
            assert_eq!(f.hooks.bytes.get(), 0);
            assert_eq!(f.hooks.slots.get(), 0);
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
            let mut wait = f.io.reactor().readiness_with_lease(
                socket,
                interest as u32,
                owner.clone(),
                &TestScope,
            );
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
}

/// Identical transport primitives shared by the two independent engine fixtures.
#[cfg(test)]
mod fixtures {
    use crate::{Error, connection::Endpoint};
    use uring_runtime::{Scope, reactor::SocketAddress};

    /// Keep HTTP and runtime failures distinct in assertions.
    #[derive(Clone, Copy, Debug, PartialEq)]
    pub(super) enum Failure {
        /// An HTTP framing or syntax failure.
        Http(Error),

        /// A runtime transport or admission failure.
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

    /// Always-live scope bounded by each engine fixture's watchdog.
    #[derive(Clone)]
    pub(super) struct TestScope;

    impl Scope for TestScope {
        type Error = Failure;

        /// Keep fixture operations live while the driver runs.
        fn check(&self) -> std::result::Result<(), Failure> {
            Ok(())
        }
    }

    /// Single endpoint key for local socket fixtures.
    #[derive(Clone, PartialEq, Eq, PartialOrd, Ord)]
    pub(super) struct Key;

    impl Endpoint<Failure> for Key {
        /// Supply a loopback address without resolving names.
        fn address(&self) -> std::result::Result<SocketAddress, Failure> {
            Ok(SocketAddress::Inet("127.0.0.1:9".parse().unwrap()))
        }
    }
}
