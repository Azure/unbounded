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
            use futures::{FutureExt, StreamExt, stream::FuturesUnordered};
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
                let accept = reactor.accept(fd.clone(), scope).fuse();
                futures::pin_mut!(accept);
                let accepted = if active.is_empty() {
                    accept.await?
                } else {
                    futures::select_biased! {
                        _ = active.next().fuse() => continue,
                        accepted = accept => accepted?,
                    }
                };
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
            use super::{transfer::WireBuffer, wire::WireCodec};
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
            let body = match &response.response {
                PeerResponse::Page { ciphertext, .. } => ciphertext.bytes(),
                _ => &[],
            };
            let head = WireCodec::encode(&response.authentication, true, body.len())?;
            let sent = self.io.send_head(connection, head, &request_scope).await?;
            let mut connection = sent.connection;
            if !body.is_empty() {
                let mut buffer = WireBuffer::new(&self.admission, body.len())?;
                buffer.bytes_mut()?.copy_from_slice(body);
                let sent = self
                    .io
                    .write_body(connection, buffer, &request_scope)
                    .await?;
                if sent.bytes != body.len() {
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
