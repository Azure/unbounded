//! Authenticate, replay-check, authorize, and admit before local dispatch or relay.
use super::{
    relay::Relay,
    wire::{PeerResponse, SignedRequest, SignedResponse, VerifiedRequest},
};
use crate::{
    error::{Error, Operation},
    model::limits::ResourceClass,
    runtime::{admission::Admission, deadline::RequestScope},
    security::forwarding::Forwarding,
};
use std::{
    rc::Rc,
    time::{Duration, Instant},
};
/// Implemented by the existing read coordinator, never a second acquisition graph.
/// Ingress must be verified; the local result is unsigned until the server signs
/// it with a retained clone of the request binding.
///
/// ```compile_fail
/// use racer_dataplane::{peer::{server::LocalPageService, wire::PeerRequest},
///     runtime::deadline::RequestScope};
/// fn unverified(service: &dyn LocalPageService, request: PeerRequest, scope: &RequestScope) {
///     service.serve_peer(request, scope);
/// }
/// ```
pub trait LocalPageService {
    fn serve_peer<'a>(
        &'a self,
        request: VerifiedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse>;
}
pub struct PeerServer {
    io: Rc<crate::http::io::HttpIo>,
    forwarding: Rc<Forwarding>,
    admission: Rc<Admission>,
    local: Rc<dyn LocalPageService>,
    relay: Rc<Relay>,
    network: Option<Rc<super::PeerNetwork>>,
    wire: Option<Rc<dyn super::wire::LogicalCodec>>,
    handshake: Option<Rc<super::handshake::Handshake>>,
    reactor: Option<Rc<crate::runtime::reactor::Reactor>>,
    transfers: Option<Rc<super::transfer::Transfers>>,
    request_timeout: Duration,
}
impl PeerServer {
    /// Accept bounded neighbor HTTP connections on the owning reactor. The shared
    /// codec frames input before signatures are verified and operations decoded.
    pub fn listen<'a>(
        &'a self,
        address: std::net::SocketAddr,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            use futures::{StreamExt, stream::FuturesUnordered};
            scope.check()?;
            let reactor = self.reactor.as_ref().ok_or(Error::InvalidConfiguration)?;
            let listener = std::net::TcpListener::bind(address).map_err(|_| Error::Io)?;
            listener.set_nonblocking(true).map_err(|_| Error::Io)?;
            let fd = Rc::new(std::os::fd::OwnedFd::from(listener));
            let mut active = FuturesUnordered::new();
            let maximum = self.admission.limits().client_connections.get();
            loop {
                scope.check()?;
                if active.len() >= maximum {
                    let _ = active.next().await;
                    continue;
                }
                let accepted =
                    next_accepted(reactor.accept(fd.clone(), scope), &mut active, scope).await?;
                let connection = match crate::http::pool::ConnectionLease::from_accepted(
                    accepted,
                    &self.admission,
                ) {
                    Ok(connection) => connection,
                    Err(Error::Overloaded) => continue,
                    Err(error) => return Err(error),
                };
                active.push(Box::pin(async move {
                    let mut connection = connection;
                    loop {
                        connection = self.serve_connection(connection, scope).await?;
                        if !connection.is_reusable() {
                            return Ok::<(), Error>(());
                        }
                    }
                }));
            }
        })
    }
    pub fn new(
        io: Rc<crate::http::io::HttpIo>,
        forwarding: Rc<Forwarding>,
        admission: Rc<Admission>,
        local: Rc<dyn LocalPageService>,
        relay: Rc<Relay>,
    ) -> Self {
        Self {
            io,
            forwarding,
            admission,
            local,
            relay,
            network: None,
            wire: None,
            handshake: None,
            reactor: None,
            transfers: None,
            request_timeout: Duration::from_secs(30),
        }
    }
    /// Bound the entire incoming head, including idle time between exchanges.
    pub fn with_request_timeout(mut self, timeout: Duration) -> Self {
        self.request_timeout = timeout;
        self
    }
    pub fn with_transfers(mut self, transfers: Rc<super::transfer::Transfers>) -> Self {
        self.transfers = Some(transfers);
        self
    }
    pub fn with_reactor(mut self, reactor: Rc<crate::runtime::reactor::Reactor>) -> Self {
        self.reactor = Some(reactor);
        self
    }
    pub fn with_network(mut self, network: Rc<super::PeerNetwork>) -> Self {
        self.network = Some(network);
        self
    }
    pub fn with_wire(mut self, wire: Rc<dyn super::wire::LogicalCodec>) -> Self {
        self.wire = Some(wire);
        self
    }
    pub fn with_handshake(mut self, handshake: Rc<super::handshake::Handshake>) -> Self {
        self.handshake = Some(handshake);
        self
    }

    /// Serve one complete exchange. The listener may reuse the returned connection
    /// only when the HTTP layer confirms both bodies were fully consumed.
    pub fn serve_connection<'a>(
        &'a self,
        connection: crate::http::pool::ConnectionLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, crate::http::pool::ConnectionLease> {
        Box::pin(async move {
            use super::{transfer::SendPage, wire::WireCodec};
            use crate::runtime::reactor::IoBuffer;
            scope.check()?;
            let codec = self.wire.as_ref().ok_or(Error::InvalidConfiguration)?;
            // One fixed budget per exchange, never renewed by partial headers.
            // Clone the listener cancellation, but keep this cap out of dispatch
            // and response I/O, which use the signed request deadline below.
            let header_scope = header_scope(scope, self.request_timeout, Instant::now())?;
            let mut received = self.io.receive_head(connection, &header_scope).await?;
            let head_bytes = received.value.headers.iter().try_fold(0usize, |n, h| {
                n.checked_add(h.name.len())
                    .and_then(|n| n.checked_add(h.value.len()))
                    .ok_or(Error::InvalidRequest)
            })?;
            let _head_reservation = self.admission.reserve(
                None,
                ResourceClass::RequestContext,
                head_bytes
                    .checked_mul(3)
                    .ok_or(Error::InvalidRequest)?
                    .max(1),
            )?;
            if matches!(&received.value.start, crate::http::codec::StartLine::Request { method, target } if method == "POST" && target == "/racer/peer/v1/challenge")
            {
                use crate::{
                    http::codec::{MessageHead, StartLine},
                    security::protocol as p,
                };
                if received.value.content_length()? != Some(0) {
                    return Err(Error::InvalidRequest);
                }
                let value = p::field(&received.value, "racer-probe")?;
                if value.len() > 512 {
                    return Err(Error::InvalidRequest);
                }
                let _permit = self
                    .admission
                    .reserve(None, ResourceClass::ControlProgress, 1)?;
                let probe = p::decode_binary(value.as_bytes())?;
                let bytes = self
                    .handshake
                    .as_ref()
                    .ok_or(Error::InvalidConfiguration)?
                    .respond_probe(&probe)?;
                let mut head = MessageHead {
                    start: StartLine::Response { status: 200 },
                    headers: Vec::new(),
                };
                p::push(&mut head, "content-length", bytes.len());
                let mut buffer = self.io.buffer(bytes.len())?;
                buffer.bytes_mut()?.copy_from_slice(&bytes);
                let sent = self.io.send_head(received.connection, head, scope).await?;
                let sent = self.io.write_body(sent.connection, buffer, scope).await?;
                let mut connection = sent.lease;
                connection.finish_exchange()?;
                return Ok(connection);
            }
            let native_control = super::native::detach(&mut received.value)?;
            let (authentication, length) = WireCodec::decode(received.value, false)?;
            if length != 0 {
                return Err(Error::InvalidRequest);
            }
            if crate::security::protocol::field(&authentication.original.head, "racer-kind")?
                == "handshake"
            {
                if !authentication.hops.is_empty() {
                    return Err(Error::InvalidRequest);
                }
                let signed = std::sync::Arc::try_unwrap(authentication.original)
                    .map_err(|_| Error::InvalidRequest)?;
                let response = self
                    .handshake
                    .as_ref()
                    .ok_or(Error::InvalidConfiguration)?
                    .respond(signed)?;
                let envelope = crate::security::forwarding::ForwardedHead {
                    original: std::sync::Arc::new(response),
                    hops: Vec::new(),
                };
                let sent = self
                    .io
                    .send_head(
                        received.connection,
                        WireCodec::encode(&envelope, true, 0)?,
                        scope,
                    )
                    .await?;
                let mut connection = sent.connection;
                connection.finish_exchange()?;
                return Ok(connection);
            }
            let request = codec.request(authentication, scope)?;
            // Listener scopes bound connection lifetime, while the signed request
            // supplies correlation and can only shorten the deadline.
            let mut request_scope = scope.clone();
            request_scope.request = request.request.route.request;
            request_scope.deadline.0 = request_scope
                .deadline
                .0
                .min(request.request.route.deadline.0);
            let admitted = match (native_control, &self.transfers) {
                (Some(control), Some(transfers)) => {
                    transfers.admit_native(&request, control, &request_scope)?
                }
                _ => None,
            };
            let response = self.dispatch(request, &request_scope).await?;
            let mut connection = received.connection;
            if let (Some(admitted), Some(transfers)) = (admitted, &self.transfers) {
                let (returned, sent) = transfers
                    .send_native(
                        connection,
                        &response,
                        admitted,
                        self.network.as_ref().ok_or(Error::InvalidConfiguration)?,
                        &request_scope,
                    )
                    .await?;
                connection = returned;
                if sent {
                    connection.finish_exchange()?;
                    return Ok(connection);
                }
            }
            let body = match response.response {
                PeerResponse::Page { ciphertext, .. } => Some(SendPage(ciphertext)),
                _ => None,
            };
            let length = body.as_ref().map_or(0, |body| body.0.bytes().len());
            let head = WireCodec::encode(&response.authentication, true, length)?;
            let sent = self.io.send_head(connection, head, &request_scope).await?;
            let mut connection = sent.connection;
            if let Some(buffer) = body {
                let sent = self
                    .io
                    .write_body(connection, buffer, &request_scope)
                    .await?;
                if sent.bytes != length {
                    return Err(Error::Io);
                }
                connection = sent.lease;
            }
            connection.finish_exchange()?;
            Ok(connection)
        })
    }
    /// Verify the complete ingress envelope before service or relay. Sign a local
    /// result against its binding; return a relayed signed result without replacing
    /// the responder's original signature or discarding forwarding headers.
    pub fn dispatch<'a>(
        &'a self,
        request: SignedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            scope.check()?;
            let request = self.forwarding.verify_request(request)?;
            let scope = super::request_scope(request.request(), scope)?;
            let network = self.network.as_ref().ok_or(Error::InvalidConfiguration)?;
            let _membership = network.membership(request.request().route.membership)?;
            let previous = request
                .forwarders()
                .last()
                .unwrap_or(request.origin())
                .node();
            network.endpoint(request.request().route.membership, previous)?;
            if request.request().route.destination != network.local {
                let binding = request.binding().clone();
                return match self.relay.forward(request, &scope).await {
                    Ok(response) => Ok(response),
                    Err(Error::Overloaded) => self
                        .forwarding
                        .sign_response(&binding, PeerResponse::Overloaded),
                    Err(
                        Error::Unavailable
                        | Error::Io
                        | Error::HopBudgetExhausted
                        | Error::IncompatibleMembership,
                    ) => {
                        scope.check()?;
                        self.forwarding
                            .sign_response(&binding, PeerResponse::Unavailable)
                    }
                    Err(error) => Err(error),
                };
            }
            let binding = request.binding().clone();
            let reservation = self.admission.reserve(None, ResourceClass::Waiter, 1);
            let _reservation = match reservation {
                Ok(reservation) => reservation,
                Err(Error::Overloaded) => {
                    return self
                        .forwarding
                        .sign_response(&binding, PeerResponse::Overloaded);
                }
                Err(error) => return Err(error),
            };
            let response = match self.local.serve_peer(request, &scope).await {
                Ok(response) => response,
                Err(Error::VersionUnavailable) => PeerResponse::VersionUnavailable,
                Err(Error::Overloaded) => PeerResponse::Overloaded,
                Err(Error::Unauthorized) => PeerResponse::OriginRejected,
                Err(Error::OriginForbidden) => PeerResponse::OriginForbidden,
                Err(Error::Unavailable | Error::Io | Error::MissingKey | Error::CorruptRecord) => {
                    PeerResponse::Unavailable
                }
                Err(error) => return Err(error),
            };
            scope.check()?;
            self.forwarding.sign_response(&binding, response)
        })
    }
}
/// Drain completed connections without abandoning the outstanding accept, which
/// may already own a successful result that has not been consumed yet.
async fn next_accepted<A, C>(
    accept: A,
    active: &mut futures::stream::FuturesUnordered<C>,
    scope: &RequestScope,
) -> crate::error::Result<std::os::fd::OwnedFd>
where
    A: std::future::Future<Output = crate::error::Result<std::os::fd::OwnedFd>>,
    C: std::future::Future,
{
    use futures::{FutureExt, StreamExt};
    let accept = accept.fuse();
    futures::pin_mut!(accept);
    loop {
        scope.check()?;
        if active.is_empty() {
            return accept.await;
        }
        futures::select_biased! {
            _ = active.next().fuse() => continue,
            accepted = accept => return accepted,
        }
    }
}
fn header_scope(
    scope: &RequestScope,
    timeout: Duration,
    now: Instant,
) -> crate::error::Result<RequestScope> {
    let mut header = scope.clone();
    header.deadline.0 = scope.deadline.0.min(
        now.checked_add(timeout)
            .ok_or(Error::InvalidConfiguration)?,
    );
    Ok(header)
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{model::identity::RequestId, test_support::clock::Clock};
    use futures::{channel::oneshot, stream::FuturesUnordered};
    use std::{
        cell::Cell,
        future::Future,
        io::{Read, Write},
        os::fd::OwnedFd,
        os::unix::net::UnixStream,
        pin::Pin,
        task::{Context, Poll},
    };

    #[derive(Default)]
    struct AcceptCounts {
        created: Cell<usize>,
        dropped: Cell<usize>,
        consumed: Cell<usize>,
        polled: Cell<usize>,
    }
    struct CountedAccept<F> {
        future: Pin<Box<F>>,
        counts: Rc<AcceptCounts>,
    }
    impl<F> CountedAccept<F> {
        fn new(future: F, counts: &Rc<AcceptCounts>) -> Self {
            counts.created.set(counts.created.get() + 1);
            Self {
                future: Box::pin(future),
                counts: counts.clone(),
            }
        }
    }
    impl<F: Future<Output = crate::error::Result<OwnedFd>>> Future for CountedAccept<F> {
        type Output = F::Output;
        fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
            self.counts.polled.set(self.counts.polled.get() + 1);
            let result = self.future.as_mut().poll(cx);
            if matches!(result, Poll::Ready(Ok(_))) {
                self.counts.consumed.set(self.counts.consumed.get() + 1);
            }
            result
        }
    }
    impl<F> Drop for CountedAccept<F> {
        fn drop(&mut self) {
            self.counts.dropped.set(self.counts.dropped.get() + 1);
        }
    }
    fn poll<F: Future>(future: Pin<&mut F>) -> Poll<F::Output> {
        future.poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }
    fn listener_scope() -> RequestScope {
        RequestScope::new(RequestId([6; 16]), Instant::now() + Duration::from_secs(60)).unwrap()
    }
    fn assert_counts(counts: &AcceptCounts, dropped: usize, consumed: usize) {
        assert_eq!(counts.created.get(), 1);
        assert_eq!(counts.dropped.get(), dropped);
        assert_eq!(counts.consumed.get(), consumed);
    }
    fn drive<T>(reactor: &crate::runtime::reactor::Reactor, future: impl Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let watchdog = Instant::now() + Duration::from_secs(10);
        loop {
            if let Poll::Ready(result) = poll(future.as_mut()) {
                return result;
            }
            assert!(Instant::now() < watchdog, "accept did not finish");
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }

    #[test]
    fn accept_survives_pending_and_ready_connection_completions() {
        for ready in [false, true] {
            for (completions, remaining) in [(0, 0), (1, 0), (4, 0), (4, 1)] {
                let scope = listener_scope();
                let counts = Rc::new(AcceptCounts::default());
                let (send_accept, receive_accept) = oneshot::channel();
                let accept = CountedAccept::new(async { receive_accept.await.unwrap() }, &counts);
                let mut active = FuturesUnordered::new();
                let finished = Rc::new(Cell::new(0));
                let mut send_completions = Vec::new();
                for _ in 0..completions + remaining {
                    let (send, receive) = oneshot::channel::<()>();
                    send_completions.push(send);
                    let finished = finished.clone();
                    active.push(async move {
                        receive.await.unwrap();
                        finished.set(finished.get() + 1);
                        Err::<(), _>(Error::DeadlineExceeded)
                    });
                }
                let _pending_completion = if remaining == 1 {
                    send_completions.pop()
                } else {
                    None
                };
                let mut work = Box::pin(next_accepted(accept, &mut active, &scope));
                assert!(poll(work.as_mut()).is_pending());
                assert_counts(&counts, 0, 0);
                let (socket, mut peer) = UnixStream::pair().unwrap();
                let mut send_accept = Some(send_accept);
                let mut socket = Some(socket);
                if ready {
                    // A successful, unconsumed accept and every completion become
                    // ready before the next poll of the production helper.
                    send_accept
                        .take()
                        .unwrap()
                        .send(Ok(socket.take().unwrap().into()))
                        .unwrap();
                    for send in send_completions.drain(..) {
                        send.send(()).unwrap();
                    }
                } else {
                    // Separate polls prove repeated completions keep the same
                    // pending accept, including the transition to empty active.
                    for (i, send) in send_completions.drain(..).enumerate() {
                        send.send(()).unwrap();
                        assert!(poll(work.as_mut()).is_pending());
                        assert_eq!(finished.get(), i + 1);
                        assert_counts(&counts, 0, 0);
                    }
                    send_accept
                        .take()
                        .unwrap()
                        .send(Ok(socket.take().unwrap().into()))
                        .unwrap();
                }
                let Poll::Ready(Ok(fd)) = poll(work.as_mut()) else {
                    panic!("retained accept must return its socket");
                };
                assert_eq!(finished.get(), completions);
                drop(work);
                assert_eq!(active.len(), remaining);
                assert_counts(&counts, 1, 1);
                // The exact accepted endpoint remains usable until its one
                // returned owner is dropped, then the peer observes EOF.
                let mut accepted = UnixStream::from(fd);
                peer.write_all(b"x").unwrap();
                let mut byte = [0];
                accepted.read_exact(&mut byte).unwrap();
                assert_eq!(&byte, b"x");
                drop(accepted);
                assert_eq!(peer.read(&mut byte).unwrap(), 0);
            }
        }
    }

    #[test]
    fn accept_scope_cancellation_and_drop_dispose_unconsumed_result_once() {
        for ready in [false, true] {
            for cancel in [false, true] {
                let scope = listener_scope();
                let counts = Rc::new(AcceptCounts::default());
                let (send_accept, receive_accept) = oneshot::channel();
                let accept = CountedAccept::new(async { receive_accept.await.unwrap() }, &counts);
                let (send_completion, receive_completion) = oneshot::channel::<()>();
                let mut active = FuturesUnordered::new();
                active.push(receive_completion);
                let mut work = Box::pin(next_accepted(accept, &mut active, &scope));
                assert!(poll(work.as_mut()).is_pending());
                let (socket, mut peer) = UnixStream::pair().unwrap();
                if ready {
                    send_accept.send(Ok(socket.into())).unwrap();
                } else {
                    drop(socket);
                }
                if cancel {
                    scope.cancel().unwrap();
                    send_completion.send(()).unwrap();
                    assert!(matches!(
                        poll(work.as_mut()),
                        Poll::Ready(Err(Error::Cancelled))
                    ));
                }
                drop(work);
                assert_counts(&counts, 1, 0);
                assert_eq!(peer.read(&mut [0]).unwrap(), 0);
            }
        }
    }

    #[test]
    fn accept_error_propagates_after_draining_completions() {
        let scope = listener_scope();
        let counts = Rc::new(AcceptCounts::default());
        let accept = CountedAccept::new(std::future::ready(Err(Error::Io)), &counts);
        let mut active = FuturesUnordered::new();
        active.push(std::future::ready(Err::<(), _>(Error::DeadlineExceeded)));
        let result = futures::executor::block_on(next_accepted(accept, &mut active, &scope));
        assert!(matches!(result, Err(Error::Io)));
        assert!(active.is_empty());
        assert_counts(&counts, 1, 0);
        assert_eq!(counts.polled.get(), 1);
    }

    #[test]
    fn real_accept_retains_socket_and_fences_cancellation_and_abandonment() {
        use crate::runtime::reactor::Reactor;
        use std::net::{TcpListener, TcpStream};

        for ready in [false, true] {
            for end in ["consume", "cancel", "drop"] {
                let admission = Rc::new(Admission::new(
                    crate::test_support::cluster::config(false).limits,
                ));
                let reactor = Reactor::new(admission.clone());
                reactor.init().unwrap();
                let baseline = admission.used(ResourceClass::RequestContext);
                let scope = listener_scope();
                let listener = TcpListener::bind("127.0.0.1:0").unwrap();
                listener.set_nonblocking(true).unwrap();
                let address = listener.local_addr().unwrap();
                let fd = Rc::new(OwnedFd::from(listener));
                let weak = Rc::downgrade(&fd);
                let counts = Rc::new(AcceptCounts::default());
                let accept = CountedAccept::new(reactor.accept(fd, &scope), &counts);
                let (send, receive) = oneshot::channel::<()>();
                let mut active = FuturesUnordered::new();
                active.push(receive);
                let mut work = Box::pin(next_accepted(accept, &mut active, &scope));
                assert!(poll(work.as_mut()).is_pending());
                reactor.poll_budgeted(128).unwrap();
                let mut peer = None;
                if ready {
                    peer = Some(TcpStream::connect(address).unwrap());
                    // Reap the real successful accept without polling its owner.
                    drive(
                        &reactor,
                        futures::future::poll_fn(|_| {
                            if reactor.in_flight() == 0 {
                                Poll::Ready(())
                            } else {
                                Poll::Pending
                            }
                        }),
                    );
                }
                if end == "cancel" {
                    scope.cancel().unwrap();
                }
                send.send(()).unwrap();
                if end == "consume" {
                    if !ready {
                        assert!(poll(work.as_mut()).is_pending());
                        assert_counts(&counts, 0, 0);
                        assert_eq!(reactor.in_flight(), 1);
                        peer = Some(TcpStream::connect(address).unwrap());
                    }
                    let accepted = drive(&reactor, work.as_mut()).unwrap();
                    let mut accepted = TcpStream::from(accepted);
                    accepted.write_all(b"x").unwrap();
                    let mut byte = [0];
                    peer.as_mut().unwrap().read_exact(&mut byte).unwrap();
                    assert_eq!(&byte, b"x");
                    drop(accepted);
                } else if end == "cancel" {
                    assert!(matches!(
                        poll(work.as_mut()),
                        Poll::Ready(Err(Error::Cancelled))
                    ));
                }
                drop(work);
                assert_counts(&counts, 1, usize::from(end == "consume"));
                if !ready && end != "consume" {
                    assert_eq!(reactor.in_flight(), 1);
                    assert!(weak.upgrade().is_some());
                    assert!(admission.used(ResourceClass::RequestContext) > baseline);
                }
                drive(&reactor, reactor.drain()).unwrap();
                assert_eq!(reactor.in_flight(), 0);
                assert!(weak.upgrade().is_none());
                assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
                if let Some(mut peer) = peer {
                    peer.set_read_timeout(Some(Duration::from_secs(10)))
                        .unwrap();
                    assert_eq!(peer.read(&mut [0]).unwrap(), 0);
                }
            }
        }
    }

    #[test]
    fn header_budget_is_fixed_per_exchange_and_preserves_listener_cancellation() {
        let clock = Clock::default();
        let timeout = Duration::from_secs(10);
        let listener =
            RequestScope::new(RequestId([9; 16]), clock.now() + Duration::from_secs(60)).unwrap();
        let first = header_scope(&listener, timeout, clock.now()).unwrap();
        assert_eq!(first.request, listener.request);
        assert_eq!(first.deadline.0, clock.now() + timeout);
        clock.advance(timeout).unwrap();
        assert_eq!(
            clock.check_deadline(first.deadline),
            Err(Error::DeadlineExceeded)
        );
        let next = header_scope(&listener, timeout, clock.now()).unwrap();
        assert_eq!(next.deadline.0, clock.now() + timeout);
        assert_eq!(clock.check_deadline(next.deadline), Ok(()));
        assert_eq!(clock.check_deadline(listener.deadline), Ok(()));
        listener.cancel().unwrap();
        assert_eq!(first.check(), Err(Error::Cancelled));
        assert_eq!(next.check(), Err(Error::Cancelled));
    }

    #[test]
    fn header_budget_never_extends_listener_deadline_and_rejects_overflow() {
        let clock = Clock::default();
        let listener =
            RequestScope::new(RequestId([8; 16]), clock.now() + Duration::from_secs(1)).unwrap();
        let header = header_scope(&listener, Duration::from_secs(30), clock.now()).unwrap();
        assert_eq!(header.deadline.0, listener.deadline.0);
        clock.advance(Duration::from_secs(1)).unwrap();
        assert_eq!(
            clock.check_deadline(header.deadline),
            Err(Error::DeadlineExceeded)
        );
        assert!(matches!(
            header_scope(&listener, Duration::MAX, clock.now()),
            Err(Error::InvalidConfiguration)
        ));
    }
}
