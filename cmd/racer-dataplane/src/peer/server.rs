//! Authenticate, replay-check, authorize, and admit before local dispatch or relay.
use super::Relay;
use super::protocol::PeerResponse;
use super::protocol::SignedRequest;
use super::protocol::SignedResponse;
use super::protocol::VerifiedRequest;
use crate::error::Error;
use crate::error::Operation;
use crate::model::ResourceClass;

use crate::admission::AdmissionPolicy;
use crate::runtime::RequestScope;
use crate::peer::forwarding::Forwarding;
use std::rc::Rc;
use std::time::Duration;
use std::time::Instant;
/// Implemented by the existing read coordinator, never a second acquisition graph.
/// Ingress must be verified; the local result is unsigned until the server signs
/// it with a retained clone of the request binding.
///
/// ```compile_fail
/// use racer_dataplane::{peer::{server::LocalPageService, protocol::PeerRequest},
///     runtime::RequestScope, topology::Membership};
/// fn unverified(service: &dyn LocalPageService, request: PeerRequest,
///     membership: std::sync::Arc<Membership>, scope: &RequestScope) {
///     service.serve_peer(request, membership, scope);
/// }
/// ```
pub trait LocalPageService {
    fn serve_peer<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse>;
}
/// Listener ownership is explicit: production distributes accepted descriptors,
/// while local and simulated listeners serve them on their own reactor.
pub(crate) enum AcceptMode {
    Distributed(std::sync::Arc<crate::admission::Ingress>),
    Local,
}

pub(crate) struct Settings {
    pub tcp_nodelay: bool,
    pub accept: AcceptMode,
    pub request_timeout: Duration,
    pub opaque_relay: bool,
}

pub struct PeerServer {
    tcp_nodelay: bool,
    metrics: crate::telemetry::Metrics,
    send_crc: Option<(
        crate::telemetry::Pair,
        crate::telemetry::Samples,
        Rc<crate::security::aead::PageCrypto>,
    )>,
    subscriptions: std::sync::Arc<super::subscriptions::Subscriptions>,
    placement: crate::topology::Placement,
    opaque_relay: bool,
    pipes: Rc<flow_control::pipe::PipePool<AdmissionPolicy>>,
    accept: AcceptMode,
    io: Rc<crate::http::HttpIo>,
    forwarding: Rc<Forwarding>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    local: Rc<dyn LocalPageService>,
    relay: Rc<Relay>,
    wire: Rc<super::protocol::SecurityCodec>,
    signatures: Rc<crate::peer::protocol::Signatures>,
    transfers: Rc<super::transport::Transfers>,
    request_timeout: Duration,
}
impl PeerServer {
    pub(crate) fn configure_accepted(
        &self,
        fd: uring_runtime::Descriptor,
    ) -> crate::error::Result<uring_runtime::Descriptor> {
        fd.enable_tcp_nodelay(self.tcp_nodelay)?;
        Ok(fd)
    }
    pub(crate) fn with_metrics(mut self, metrics: crate::telemetry::Metrics) -> Self {
        self.metrics = metrics;
        self
    }
    pub(crate) fn with_send_crc(
        mut self,
        pair: Option<crate::telemetry::Pair>,
        samples: crate::telemetry::Samples,
        crypto: Rc<crate::security::aead::PageCrypto>,
    ) -> Self {
        self.send_crc = pair.map(|pair| (pair, samples, crypto));
        self
    }
    fn begin_send_sample(
        &self,
        connection: &crate::http::ConnectionLease,
        response: &SignedResponse,
        scope: &RequestScope,
    ) -> Option<crate::telemetry::Ticket> {
        let (pair, samples, crypto) = self.send_crc.as_ref()?;
        let ciphertext = match &response.response {
            PeerResponse::Page { ciphertext, .. }
            | PeerResponse::Selected { ciphertext, .. }
            | PeerResponse::Bootstrap {
                page_zero: Some(ciphertext),
                ..
            } => ciphertext,
            _ => return None,
        };
        ciphertext.provenance?;
        let receiver = connection.state().session.as_ref()?.peer();
        let (ticket, sample) = samples.begin(pair, self.signatures.node(), receiver)?;
        let permit = match uring_runtime::drivers::reserve().map_err(Error::from) {
            Ok(p) => p,
            Err(error) => {
                sample.finish(Some(error));
                return Some(ticket);
            }
        };
        let deadline = scope
            .deadline
            .0
            .min(uring_runtime::environment::now() + Duration::from_secs(2));
        let diagnostic_scope = match RequestScope::new(scope.request, deadline) {
            Ok(s) => s,
            Err(error) => {
                sample.finish(Some(error));
                return Some(ticket);
            }
        };
        let crypto = crypto.clone();
        let ciphertext = ciphertext.clone();
        permit.submit_detached(Box::pin(async move {
            crypto
                .sample_send(ciphertext, &diagnostic_scope, sample)
                .await;
            Ok::<_, Error>(())
        }));
        Some(ticket)
    }
    #[cfg(test)]
    pub(crate) fn for_test(
        io: Rc<crate::http::HttpIo>,
        forwarding: Rc<Forwarding>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        local: Rc<dyn LocalPageService>,
        relay: Rc<Relay>,
        wire: Rc<super::protocol::SecurityCodec>,
        signatures: Rc<crate::peer::protocol::Signatures>,
    ) -> Self {
        let pipes = Rc::new(crate::http::new_pipe_pool(admission.clone()));
        let transfers = Rc::new(super::transport::Transfers::new(
            Rc::new(crate::http::new_pool(
                io.reactor().clone(),
                admission.clone(),
                1,
            )),
            io.clone(),
            None,
            admission.clone(),
            wire.clone(),
            signatures.clone(),
        ));
        Self::new(
            io,
            forwarding,
            admission,
            local,
            relay,
            wire,
            signatures,
            std::sync::Arc::new(
                super::subscriptions::Subscriptions::new(Default::default()).unwrap(),
            ),
            pipes,
            transfers,
            Settings {
                tcp_nodelay: false,
                accept: AcceptMode::Local,
                request_timeout: Duration::from_secs(30),
                opaque_relay: false,
            },
        )
    }
    #[cfg(test)]
    pub(crate) fn subscription_owner(
        &self,
    ) -> &std::sync::Arc<super::subscriptions::Subscriptions> {
        &self.subscriptions
    }
    #[cfg(test)]
    pub(crate) fn transport_io(&self) -> &Rc<crate::http::HttpIo> {
        &self.io
    }
    /// Accept bounded neighbor HTTP connections on the owning reactor. The shared
    /// codec frames input before signatures are verified and operations decoded.
    pub fn listen<'a>(
        &'a self,
        address: std::net::SocketAddr,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            use futures::StreamExt;
            use futures::stream::FuturesUnordered;
            scope.check()?;
            let reactor = self.io.reactor();
            let fd = Rc::new(uring_runtime::Descriptor::tcp_listener(address)?);
            let mut active = FuturesUnordered::new();
            let maximum = self
                .admission
                .limit(crate::model::ResourceClass::IngressConnection);
            loop {
                scope.check()?;
                if let AcceptMode::Distributed(ingress) = &self.accept {
                    uring_runtime::retry_listener(scope, || {
                        reactor.readiness_with_lease(fd.clone(), libc::POLLIN as u32, (), scope)
                    })
                    .await?;
                    let cancellation = scope.cancellation.subscribe()?;
                    let offer = std::future::poll_fn(|cx| {
                        cancellation.register(cx.waker());
                        if let Err(error) = scope.check() {
                            return std::task::Poll::Ready(Err(error));
                        }
                        match ingress.reserve(cx.waker()) {
                            Ok(offer) => std::task::Poll::Ready(Ok(offer)),
                            Err(Error::Overloaded) => std::task::Poll::Pending,
                            Err(error) => std::task::Poll::Ready(Err(error)),
                        }
                    })
                    .await?;
                    let accepted = match fd.try_accept() {
                        Ok(fd) => fd,
                        Err(error)
                            if matches!(
                                error.kind(),
                                std::io::ErrorKind::WouldBlock | std::io::ErrorKind::Interrupted
                            ) =>
                        {
                            continue;
                        }
                        Err(_) => return Err(Error::Io),
                    };
                    offer.deliver(
                        self.configure_accepted(accepted)?
                            .into_host()
                            .map_err(|_| Error::InvalidConfiguration)?,
                        crate::admission::Kind::Peer,
                    )?;
                    continue;
                }
                if active.len() >= maximum {
                    let _ = active.next().await;
                    continue;
                }
                let accepted = next_accepted(
                    uring_runtime::retry_listener(scope, || reactor.accept(fd.clone(), scope)),
                    &mut active,
                    scope,
                )
                .await?;
                let connection = match crate::http::from_accepted(
                    self.configure_accepted(accepted)?,
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
    pub(crate) fn new(
        io: Rc<crate::http::HttpIo>,
        forwarding: Rc<Forwarding>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        local: Rc<dyn LocalPageService>,
        relay: Rc<Relay>,
        wire: Rc<super::protocol::SecurityCodec>,
        signatures: Rc<crate::peer::protocol::Signatures>,
        subscriptions: std::sync::Arc<super::subscriptions::Subscriptions>,
        pipes: Rc<flow_control::pipe::PipePool<AdmissionPolicy>>,
        transfers: Rc<super::transport::Transfers>,
        settings: Settings,
    ) -> Self {
        Self {
            subscriptions,
            metrics: crate::telemetry::Metrics::default(),
            tcp_nodelay: settings.tcp_nodelay,
            send_crc: None,
            placement: crate::topology::Placement::new(64),
            opaque_relay: settings.opaque_relay,
            pipes,
            accept: settings.accept,
            io,
            forwarding,
            admission,
            local,
            relay,
            wire,
            signatures,
            transfers,
            request_timeout: settings.request_timeout,
        }
    }
    /// Bound incoming heads and the complete connection handshake, including writes.
    /// Authenticated page dispatch and transfer retain their signed request deadline.
    #[cfg(test)]
    pub fn with_request_timeout(mut self, timeout: Duration) -> Self {
        self.request_timeout = timeout;
        self
    }
    #[cfg(test)]
    pub fn with_transfers(mut self, transfers: Rc<super::transport::Transfers>) -> Self {
        self.transfers = transfers;
        self
    }

    /// Serve one complete exchange. The listener may reuse the returned connection
    /// only when the HTTP layer confirms both bodies were fully consumed.
    pub fn serve_connection<'a>(
        &'a self,
        connection: crate::http::ConnectionLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, crate::http::ConnectionLease> {
        Box::pin(async move {
            use super::protocol::decode_envelope;
            use super::protocol::encode_envelope;
            scope.check()?;
            let codec = &self.wire;
            // One fixed budget for handshake reads, verification, signing, writes,
            // and the incoming application head. Partial progress never renews it.
            // Only application dispatch/response I/O use the signed deadline below.
            let header_scope = header_scope(
                scope,
                self.request_timeout,
                uring_runtime::environment::now(),
            )?;
            let connection = self
                .authenticate_connection(connection, &header_scope)
                .await?;
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
            let native_control = super::transport::detach(&mut received.value)?;
            let (authentication, length) = decode_envelope(received.value, false)?;
            if length != 0 {
                return Err(Error::InvalidRequest);
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
            let admitted = match native_control {
                Some(control) => self
                    .transfers
                    .admit_native(&request, control, &request_scope)?,
                _ => None,
            };
            let request = self.forwarding.verify_request(request)?;
            let membership = self
                .relay
                .network
                .membership(request.request().route.membership);
            let response = match &membership {
                Ok(membership)
                    if admitted.is_none()
                        && self.opaque_relay()
                        && request.request().route.destination != self.relay.network.local =>
                {
                    let network = &self.relay.network;
                    let previous = request
                        .forwarders()
                        .last()
                        .unwrap_or(request.origin())
                        .node();
                    network.endpoint(membership, previous)?;
                    let binding = request.binding().clone();
                    let result = match self
                        .admission
                        .reserve(None, ResourceClass::Relay, 1)
                        .map_err(Error::from)
                    {
                        Ok(reservation) => {
                            let reservation = Rc::new(reservation);
                            received.connection.state_mut().relay_reservation =
                                Some(reservation.clone());
                            // Cancel only this downstream exchange on upstream FIN.
                            // Fence its I/O and disconnect watch before streaming.
                            let exchange =
                                RequestScope::new(request_scope.request, request_scope.deadline.0)?;
                            disconnect_fenced_exchange(
                                &self.io,
                                &received.connection,
                                &request_scope,
                                &exchange,
                                self.relay.forward_inner(
                                    request,
                                    membership.clone(),
                                    Some(reservation),
                                    &exchange,
                                ),
                            )
                            .await
                        }
                        Err(error) => Err(error),
                    };
                    // Pipe pressure is a signed overload before any success head
                    // is sent. No queue holds a downstream body waiting for quota.
                    let result = result.and_then(|response| {
                        if matches!(&response, super::transport::RelayResponse::Http { length, .. } if *length != 0) {
                            let mut pipe = self.pipes.acquire()?;
                            pipe.prepare_transit();
                            received.connection.state_mut().relay_pipe = Some(pipe);
                        }
                        Ok(response)
                    });
                    let result = self.admission.policy().observer().result(
                        crate::telemetry::Stage::PeerRelay,
                        &request_scope,
                        result,
                    );
                    match result {
                        Ok(super::transport::RelayResponse::Http {
                            authentication,
                            connection,
                            length,
                        }) => {
                            let head = encode_envelope(&authentication, true, length)?;
                            received.connection.state_mut().relay_peer = Some(connection);
                            let mut connection = self
                                .io
                                .send_head(received.connection, head, &request_scope)
                                .await?
                                .connection;
                            let downstream = *connection
                                .state_mut()
                                .relay_peer
                                .take()
                                .ok_or(Error::Internal)?;
                            let pipe = connection.state_mut().relay_pipe.take();
                            // Once the success head is sent, any body failure closes
                            // both dirty connections. Never append an error envelope.
                            let mut observation = self.metrics.opaque_relay_body(length);
                            let result = crate::http::relay_body(
                                &self.io,
                                downstream,
                                connection,
                                pipe,
                                &request_scope,
                            )
                            .await;
                            if result.is_ok() {
                                if let Some(observation) = &mut observation {
                                    observation.complete();
                                }
                            }
                            return result;
                        }
                        Ok(super::transport::RelayResponse::Complete(response)) => response,
                        Err(Error::Overloaded) => self
                            .forwarding
                            .sign_response(&binding, PeerResponse::Overloaded)?,
                        Err(
                            Error::Unavailable
                            | Error::Io
                            | Error::HopBudgetExhausted
                            | Error::IncompatibleMembership,
                        ) => {
                            request_scope.check()?;
                            self.forwarding
                                .sign_response(&binding, PeerResponse::Unavailable)?
                        }
                        Err(error) => return Err(error),
                    }
                }
                Ok(membership) => {
                    if admitted.is_none() {
                        // A listener scope is shared by unrelated connections. Only
                        // this HTTP exchange may be canceled, including its local
                        // destination waiter, never independent shared-flight work.
                        let exchange =
                            RequestScope::new(request_scope.request, request_scope.deadline.0)?;
                        disconnect_fenced_exchange(
                            &self.io,
                            &received.connection,
                            &request_scope,
                            &exchange,
                            self.dispatch_verified(request, membership.clone(), &exchange),
                        )
                        .await?
                    } else {
                        self.dispatch_verified(request, membership.clone(), &request_scope)
                            .await?
                    }
                }
                Err(Error::IncompatibleMembership) => self
                    .forwarding
                    .sign_response(request.binding(), PeerResponse::StaleMembership)?,
                Err(error) => return Err(*error),
            };
            let mut connection = received.connection;
            if let (Some(admitted), Ok(membership)) = (admitted, &membership) {
                let (returned, sent) = self
                    .transfers
                    .send_native(connection, &response, admitted, &membership, &request_scope)
                    .await?;
                connection = returned;
                if sent {
                    connection.finish_exchange()?;
                    return Ok(connection);
                }
            }
            let connection = self
                .send_http_response(connection, &response, &request_scope)
                .await?;
            drop(membership);
            Ok(connection)
        })
    }

    async fn authenticate_connection(
        &self,
        mut connection: crate::http::ConnectionLease,
        scope: &RequestScope,
    ) -> crate::error::Result<crate::http::ConnectionLease> {
        if connection.state().session.is_some() {
            return Ok(connection);
        }
        connection.state_mut().control_reservation = Some(self.admission.reserve(
            None,
            ResourceClass::ControlProgress,
            1,
        )?);
        let mut connection = crate::peer::protocol::accept(
            &self.io,
            connection,
            self.signatures.clone(),
            scope,
        )
        .await?;
        // Successful accept has fenced every control operation. On error or
        // abandonment the reactor-owned connection retains this charge.
        connection.state_mut().control_reservation.take();
        Ok(connection)
    }

    pub(crate) async fn send_http_response(
        &self,
        connection: crate::http::ConnectionLease,
        response: &SignedResponse,
        scope: &RequestScope,
    ) -> crate::error::Result<crate::http::ConnectionLease> {
        let ticket = self.begin_send_sample(&connection, response, scope);
        let result = self
            .send_http_response_inner(connection, response, scope)
            .await;
        if let Some(ticket) = &ticket {
            ticket.finish(result.is_ok());
        }
        result
    }
    async fn send_http_response_inner(
        &self,
        connection: crate::http::ConnectionLease,
        response: &SignedResponse,
        scope: &RequestScope,
    ) -> crate::error::Result<crate::http::ConnectionLease> {
        let body = match &response.response {
            PeerResponse::Page { ciphertext, .. } | PeerResponse::Selected { ciphertext, .. } => {
                ciphertext.bytes()
            }
            PeerResponse::Bootstrap {
                page_zero: Some(ciphertext),
                ..
            } => ciphertext.bytes(),
            _ => &[],
        };
        let head = super::protocol::encode_envelope(&response.authentication, true, body.len())?;
        let sent = self.io.send_head(connection, head, scope).await?;
        let mut connection = sent.connection;
        if !body.is_empty() {
            let ciphertext = match &response.response {
                PeerResponse::Page { ciphertext, .. }
                | PeerResponse::Selected { ciphertext, .. }
                | PeerResponse::Bootstrap {
                    page_zero: Some(ciphertext),
                    ..
                } => ciphertext,
                _ => return Err(Error::InvalidRequest),
            };
            let sent = self
                .io
                .write_body(connection, ciphertext.clone(), scope)
                .await?;
            if sent.bytes != body.len() {
                return Err(Error::Io);
            }
            connection = sent.lease;
        }
        connection.finish_exchange()?;
        connection.state_mut().relay_reservation = None;
        Ok(connection)
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
            let membership = self
                .relay
                .network
                .membership(request.request().route.membership);
            let membership = match membership {
                Ok(membership) => membership,
                Err(Error::IncompatibleMembership) => {
                    return self
                        .forwarding
                        .sign_response(request.binding(), PeerResponse::StaleMembership);
                }
                Err(error) => return Err(error),
            };
            self.dispatch_verified(request, membership, scope).await
        })
    }

    fn dispatch_verified<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            let scope = super::request_scope(request.request(), scope)?;
            let network = &self.relay.network;
            let previous = request
                .forwarders()
                .last()
                .unwrap_or(request.origin())
                .node();
            network.endpoint(&membership, previous)?;
            if request.request().route.destination != network.local {
                let binding = request.binding().clone();
                let result = self.relay.forward(request, membership, &scope).await;
                let result = self.admission.policy().observer().result(
                    crate::telemetry::Stage::PeerRelay,
                    &scope,
                    result,
                );
                return match result {
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
            let reservation = self
                .admission
                .reserve(None, ResourceClass::Waiter, 1)
                .map_err(Error::from);
            let _reservation = match reservation {
                Ok(reservation) => reservation,
                Err(Error::Overloaded) => {
                    return self
                        .forwarding
                        .sign_response(&binding, PeerResponse::Overloaded);
                }
                Err(error) => return Err(error),
            };
            let result = self.serve_selected(request, membership, &scope).await;
            let result = self.admission.policy().observer().result(
                crate::telemetry::Stage::PeerLocal,
                &scope,
                result,
            );
            let response = match result {
                Ok(response) => response,
                Err(Error::NotFound) => PeerResponse::NotFound,
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
    fn serve_selected<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        Box::pin(async move {
            use super::subscriptions::Selection;
            use crate::peer::protocol::encode_deadline;
            use crate::peer::protocol::millis;
            let super::protocol::Operation::Subscribe { subscription, mode } =
                &request.request().operation
            else {
                return self.local.serve_peer(request, membership, scope).await;
            };
            membership.member(request.origin().node())?;
            let local = &self.relay.network.local;
            let selection = if matches!(mode, super::protocol::FetchMode::CopyOnly) {
                self.subscriptions.schedule(
                    subscription.clone(),
                    membership.version,
                    request.origin().node().clone(),
                    encode_deadline(scope.deadline)?,
                    millis(uring_runtime::environment::wall_now())?,
                )?
            } else {
                self.subscriptions
                    .schedule_scoped(
                        subscription.clone(),
                        membership.clone(),
                        request.origin().node().clone(),
                        local,
                        &self.placement,
                        scope,
                    )
                    .await?
            };
            let (mut work, mut waiter) = match selection {
                Selection::Leader { work, waiter } => (Some(work), waiter),
                Selection::Follower(waiter) => (None, waiter),
            };
            let mut request = Some(request);
            let cancellation = scope.cancellation.subscribe()?;
            loop {
                if let Some(owner) = work.take() {
                    let selected = request
                        .take()
                        .ok_or(Error::StaleFlight)?
                        .select_page(owner.page().clone())?;
                    match self
                        .local
                        .serve_peer(selected, membership.clone(), scope)
                        .await
                    {
                        Ok(PeerResponse::Page {
                            metadata,
                            ciphertext,
                        }) => {
                            owner.complete(
                                crate::memory::page::CiphertextCopy {
                                    metadata,
                                    ciphertext,
                                },
                                millis(uring_runtime::environment::wall_now())?,
                            )?;
                        }
                        Ok(other) => {
                            owner.fail(match other {
                                PeerResponse::OriginRejected => Error::OriginRejected,
                                PeerResponse::OriginForbidden => Error::OriginForbidden,
                                _ => Error::Unavailable,
                            });
                            return Ok(other);
                        }
                        Err(error) => {
                            owner.fail(error);
                            return Err(error);
                        }
                    }
                }
                let completion = std::future::poll_fn(|cx| {
                    cancellation.register(cx.waker());
                    if let Err(error) = scope.check() {
                        return std::task::Poll::Ready(Err(error));
                    }
                    let now = match millis(uring_runtime::environment::wall_now()) {
                        Ok(now) => now,
                        Err(error) => return std::task::Poll::Ready(Err(error)),
                    };
                    if let Some(promoted) = waiter.take_work(now) {
                        work = Some(promoted);
                        return std::task::Poll::Ready(Ok(None));
                    }
                    waiter.poll_result(cx, now).map(|result| result.map(Some))
                })
                .await?;
                if let Some(completion) = completion {
                    return Ok(PeerResponse::Selected {
                        metadata: completion.metadata,
                        ciphertext: completion.ciphertext,
                        grant: completion.grant,
                    });
                }
            }
        })
    }
    /// Enable experimental opaque HTTP transit without changing endpoint/native paths.
    #[cfg(test)]
    pub fn with_opaque_relay(mut self, enabled: bool) -> Self {
        self.opaque_relay = enabled;
        self
    }
    pub(crate) fn opaque_relay(&self) -> bool {
        self.opaque_relay
    }
}
/// Drain completed connections without abandoning the outstanding accept, which
/// may already own a successful result that has not been consumed yet.
async fn next_accepted<A, C>(
    accept: A,
    active: &mut futures::stream::FuturesUnordered<C>,
    scope: &RequestScope,
) -> crate::error::Result<uring_runtime::Descriptor>
where
    A: std::future::Future<Output = crate::error::Result<uring_runtime::Descriptor>>,
    C: std::future::Future,
{
    use futures::FutureExt;
    use futures::StreamExt;
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
/// Cancel an abandoned HTTP exchange without dropping work before its completion fences.
async fn disconnect_fenced_exchange<T>(
    io: &crate::http::HttpIo,
    connection: &crate::http::ConnectionLease,
    parent: &RequestScope,
    exchange: &RequestScope,
    work: impl std::future::Future<Output = crate::error::Result<T>>,
) -> crate::error::Result<T> {
    use std::task::Poll;
    let parent_wake = parent.cancellation.subscribe()?;
    let watch_scope = RequestScope::new(exchange.request, exchange.deadline.0)?;
    let socket = connection.socket();
    let events = (libc::POLLRDHUP | libc::POLLHUP | libc::POLLERR) as u32;
    let mut watch = Some(io.reactor().readiness_with_lease(
        socket.clone(),
        events,
        connection.slot().cloned(),
        &watch_scope,
    ));
    let mut work = std::pin::pin!(work);
    let mut failure = None;
    let result = std::future::poll_fn(|cx| {
        parent_wake.register(cx.waker());
        if let Err(error) = parent.check() {
            failure.get_or_insert(error);
        }
        if let Some(Poll::Ready(result)) = watch.as_mut().map(|watch| watch.as_mut().poll(cx)) {
            watch = None;
            match result {
                Ok(ready) if ready & events != 0 => {
                    failure.get_or_insert(Error::Cancelled);
                }
                Err(error) => {
                    failure.get_or_insert(error);
                }
                _ => (),
            }
        }
        if socket.peer_read_closed() {
            failure.get_or_insert(Error::Cancelled);
        }
        if failure.is_some() {
            let _ = exchange.cancel();
        }
        work.as_mut().poll(cx)
    })
    .await;
    // Fence the watch before sending a response or reusing the HTTP connection.
    let _ = watch_scope.cancel();
    if let Some(watch) = watch {
        let _ = watch.await;
    }
    match failure {
        Some(error) => Err(error),
        None => result,
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
    use crate::model::RequestId;
    use crate::test_support::clock::Clock;
    use futures::channel::oneshot;
    use futures::stream::FuturesUnordered;
    use std::cell::Cell;
    use std::future::Future;
    use std::io::Read;
    use std::io::Write;
    use std::os::unix::net::UnixStream;
    use std::pin::Pin;
    use std::task::Context;
    use std::task::Poll;

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
    impl<F: Future<Output = crate::error::Result<uring_runtime::Descriptor>>> Future
        for CountedAccept<F>
    {
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
    fn poll<F: Future + ?Sized>(future: Pin<&mut F>) -> Poll<F::Output> {
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
    use crate::http::tests::drive;

    #[test]
    fn materialized_transit_fin_and_parent_cancel_fence_head_and_body() {
        use crate::http::Codec;
        use crate::runtime::Reactor;
        use std::net::Shutdown;
        use std::net::TcpListener;
        use std::net::TcpStream;

        for body in [false, true] {
            for cancel_parent in [false, true] {
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let reactor = Rc::new(Reactor::new(admission.clone()));
                reactor.init().unwrap();
                let io =
                    crate::http::new_io(reactor.clone(), Codec::new(4096), admission.clone(), 4096);
                let parent = listener_scope();
                let exchange = RequestScope::new(
                    RequestId([7; 16]),
                    parent.deadline.0 - Duration::from_secs(10),
                )
                .unwrap();
                let listener = TcpListener::bind("127.0.0.1:0").unwrap();
                let upstream = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
                let (socket, _) = listener.accept().unwrap();
                let connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
                let (socket, mut downstream) = UnixStream::pair().unwrap();
                let downstream_connection =
                    crate::http::from_accepted(socket.into(), &admission).unwrap();
                if body {
                    downstream
                        .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nx")
                        .unwrap();
                }
                let entered_body = Cell::new(false);
                let completed = Cell::new(false);
                let work = async {
                    let _relay = admission.reserve(None, ResourceClass::Relay, 1)?;
                    let result = async {
                        let received = io.receive_head(downstream_connection, &exchange).await?;
                        entered_body.set(true);
                        let mut connection = received.connection;
                        let mut buffer = io.buffer(4)?;
                        while connection.remaining_body() != Some(0) {
                            let read = io.read_body(connection, buffer, &exchange).await?;
                            connection = read.lease;
                            buffer = read.buffer;
                        }
                        Ok(crate::peer::tests::signed_fixture_response())
                    }
                    .await;
                    completed.set(true);
                    result
                };
                let mut work = Box::pin(disconnect_fenced_exchange(
                    &io,
                    &connection,
                    &parent,
                    &exchange,
                    work,
                ));
                drive(
                    &reactor,
                    std::future::poll_fn(|_| {
                        assert!(
                            poll(work.as_mut()).is_pending(),
                            "body={body} parent={cancel_parent}"
                        );
                        if (!body || entered_body.get()) && reactor.in_flight() >= 2 {
                            Poll::Ready(())
                        } else {
                            Poll::Pending
                        }
                    }),
                );
                assert_eq!(admission.used(ResourceClass::Relay), 1);
                let start = Instant::now();
                if cancel_parent {
                    parent.cancel().unwrap();
                } else {
                    // Real TCP FIN, not Unix full-close/POLLHUP. Keep the read side open.
                    upstream.shutdown(Shutdown::Write).unwrap();
                }
                assert!(matches!(
                    drive(&reactor, work.as_mut()),
                    Err(Error::Cancelled)
                ));
                assert!(start.elapsed() < Duration::from_secs(2));
                assert!(
                    completed.get(),
                    "downstream future must run through cleanup"
                );
                assert_eq!(reactor.in_flight(), 0, "both IO and watch must be fenced");
                assert_eq!(admission.used(ResourceClass::Relay), 0);
                assert_eq!(parent.cancellation.is_cancelled(), cancel_parent);
                drop(work);
                downstream
                    .set_read_timeout(Some(Duration::from_secs(1)))
                    .unwrap();
                assert_eq!(downstream.read(&mut [0]).unwrap(), 0);
                if !cancel_parent {
                    // A different connection on the same listener still makes progress.
                    let (socket, mut peer) = UnixStream::pair().unwrap();
                    let unrelated = crate::http::from_accepted(socket.into(), &admission).unwrap();
                    peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
                        .unwrap();
                    drive(&reactor, io.receive_head(unrelated, &parent)).unwrap();
                }
            }
        }
    }

    #[test]
    fn materialized_transit_success_fences_watch_before_keepalive() {
        use crate::http::Codec;
        use crate::runtime::Reactor;
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let io = crate::http::new_io(reactor.clone(), Codec::new(4096), admission.clone(), 4096);
        let parent = listener_scope();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let mut connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
        for _ in 0..2 {
            peer.write_all(b"GET / HTTP/1.1\r\nContent-Length: 0\r\n\r\n")
                .unwrap();
            let received = drive(&reactor, io.receive_head(connection, &parent)).unwrap();
            connection = received.connection;
            let exchange = RequestScope::new(RequestId([7; 16]), parent.deadline.0).unwrap();
            let mut pending_once = true;
            let mut signed = Some(crate::peer::tests::signed_fixture_response());
            let signature = signed
                .as_ref()
                .unwrap()
                .authentication
                .original
                .signature
                .clone();
            let work = std::future::poll_fn(|cx| {
                if pending_once {
                    pending_once = false;
                    cx.waker().wake_by_ref();
                    Poll::Pending
                } else {
                    Poll::Ready(Ok(signed.take().unwrap()))
                }
            });
            let response = drive(
                &reactor,
                disconnect_fenced_exchange(&io, &connection, &parent, &exchange, work),
            )
            .unwrap();
            assert!(matches!(response.response, PeerResponse::Miss));
            assert_eq!(response.authentication.original.signature, signature);
            assert_eq!(reactor.in_flight(), 0);
            assert_eq!(exchange.check(), Ok(()));
            let head = Codec::new(4096)
                .decode_head(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
                .unwrap()
                .unwrap()
                .0;
            connection = drive(&reactor, io.send_head(connection, head, &parent))
                .unwrap()
                .connection;
            connection.finish_exchange().unwrap();
            assert!(connection.is_reusable());
            peer.set_read_timeout(Some(Duration::from_secs(1))).unwrap();
            let mut response = [0; 128];
            assert!(peer.read(&mut response).unwrap() > 0);
        }
        assert_eq!(parent.check(), Ok(()));
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
                let mut accepted = UnixStream::from(fd.into_host().unwrap());
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
        use crate::runtime::Reactor;
        use std::net::TcpListener;
        use std::net::TcpStream;

        for ready in [false, true] {
            for end in ["consume", "cancel", "drop"] {
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let reactor = Reactor::new(admission.clone());
                reactor.init().unwrap();
                let baseline = admission.used(ResourceClass::RequestContext);
                let scope = listener_scope();
                let listener = TcpListener::bind("127.0.0.1:0").unwrap();
                listener.set_nonblocking(true).unwrap();
                let address = listener.local_addr().unwrap();
                let fd = Rc::new(uring_runtime::Descriptor::from(listener));
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
                    let mut accepted = TcpStream::from(accepted.into_host().unwrap());
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

    #[test]
    fn backpressured_handshake_responses_keep_fixed_deadline_and_fenced_admission() {
        use crate::http::Codec;
        use crate::http::ConnectionLease;
        use crate::memory::BufferPool;
        use crate::peer::protocol::SecurityCodec;
        use crate::runtime::Reactor;
        use crate::peer::protocol::tests::finish;
        use crate::peer::protocol::tests::hello;
        use crate::topology::LinkHealth;
        use crate::topology::Paths;

        struct Never;
        impl LocalPageService for Never {
            fn serve_peer<'a>(
                &'a self,
                _: VerifiedRequest,
                _: std::sync::Arc<crate::topology::Membership>,
                _: &'a RequestScope,
            ) -> Operation<'a, PeerResponse> {
                Box::pin(async { panic!("handshake must not dispatch") })
            }
        }
        fn fill(socket: &mut UnixStream) -> usize {
            socket.set_nonblocking(true).unwrap();
            let mut total = 0;
            loop {
                match socket.write(&[0x55; 4096]) {
                    Ok(n) => {
                        assert!(n > 0);
                        total += n;
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => break,
                    Err(error) => panic!("fill: {error}"),
                }
            }
            assert!(total > 0);
            total
        }
        fn response(
            reactor: &Reactor,
            work: &mut Operation<'_, ConnectionLease>,
            peer: &mut UnixStream,
            codec: &Codec,
        ) -> http1::MessageHead {
            let mut bytes = Vec::new();
            drive(
                reactor,
                futures::future::poll_fn(|_| {
                    assert!(poll(work.as_mut()).is_pending());
                    let mut buffer = [0; 8192];
                    match peer.read(&mut buffer) {
                        Ok(n) => {
                            assert!(n > 0);
                            bytes.extend_from_slice(&buffer[..n]);
                        }
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                        Err(error) => panic!("response: {error}"),
                    }
                    match codec.decode_head(&bytes).unwrap() {
                        Some((head, end)) => {
                            assert_eq!(end, bytes.len());
                            Poll::Ready(head)
                        }
                        None => Poll::Pending,
                    }
                }),
            )
        }

        for phase in ["challenge", "ready"] {
            for end in ["expiry", "handshake-cap", "cancel", "drop", "resume"] {
                // Only the clock is virtual: socket buffers and CQEs are real.
                let clock =
                    SimulationClock::new_at(83, Instant::now(), std::time::SystemTime::now());
                let environment = clock.environment(0);
                let _guard = environment.enter();
                let signers = crate::security::test_support::network(2);
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.client_connections = std::num::NonZeroUsize::new(1).unwrap();
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
                let reactor = Rc::new(Reactor::new(admission.clone()));
                reactor.init().unwrap();
                let baseline = admission.used(ResourceClass::RequestContext);
                let io = Rc::new(crate::http::new_io(
                    reactor.clone(),
                    Codec::new(super::super::protocol::MAX_ENVELOPE_HEAD),
                    admission.clone(),
                    0,
                ));
                let forwarding = Rc::new(Forwarding::new(signers[1].clone()));
                let outbound = crate::peer::tests::NoOutbound::new(
                    signers[1].clone(),
                    Rc::new(
                        super::super::PeerNetwork::new(
                            signers[1].node().clone(),
                            std::sync::Arc::new(Default::default()),
                        )
                        .unwrap(),
                    ),
                );
                let relay = Rc::new(Relay::new(
                    Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
                    forwarding.clone(),
                    outbound.requester.clone(),
                    admission.clone(),
                    Rc::new(
                        super::super::PeerNetwork::new(
                            signers[1].node().clone(),
                            std::sync::Arc::new(Default::default()),
                        )
                        .unwrap(),
                    ),
                ));
                let server = PeerServer::for_test(
                    io,
                    forwarding,
                    admission.clone(),
                    Rc::new(Never),
                    relay,
                    Rc::new(SecurityCodec::new(
                        admission.clone(),
                        BufferPool::new(admission.clone()),
                    )),
                    signers[1].clone(),
                )
                .with_request_timeout(Duration::from_secs(
                    if end == "handshake-cap" { 60 } else { 4 },
                ));
                let scope = RequestScope::new(
                    RequestId([5; 16]),
                    uring_runtime::environment::now() + Duration::from_secs(365 * 24 * 3600),
                )
                .unwrap();
                let (socket, mut peer) = UnixStream::pair().unwrap();
                let mut writer = socket.try_clone().unwrap();
                peer.set_nonblocking(true).unwrap();
                let connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
                let mut work = server.serve_connection(connection, &scope);
                assert!(poll(work.as_mut()).is_pending());
                let codec = Codec::new(65536);
                let hello = codec
                    .encode_head(&hello(&signers[0], signers[1].node()))
                    .unwrap();
                let request = if phase == "ready" {
                    peer.write_all(&hello).unwrap();
                    let challenge = response(&reactor, &mut work, &mut peer, &codec);
                    // Let the server enter its finish read before sending it.
                    reactor.poll_budgeted(128).unwrap();
                    assert!(poll(work.as_mut()).is_pending());
                    codec
                        .encode_head(&finish(&signers[0], signers[1].node(), challenge))
                        .unwrap()
                } else {
                    // Consume a partial hello before advancing time. Completing
                    // its head must not restart the response-write budget.
                    peer.write_all(&hello[..10]).unwrap();
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
                    assert!(poll(work.as_mut()).is_pending());
                    hello[10..].to_vec()
                };
                clock.advance(Duration::from_secs(3));
                let filled = fill(&mut writer);
                drop(writer);
                peer.write_all(&request).unwrap();
                // Reap only the incoming head, then submit the response write.
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
                assert!(poll(work.as_mut()).is_pending());
                reactor.poll_budgeted(128).unwrap();
                assert!(poll(work.as_mut()).is_pending());
                assert_eq!(reactor.in_flight(), 1);
                assert_eq!(admission.used(ResourceClass::Connection), 1);
                assert_eq!(admission.used(ResourceClass::ControlProgress), 1);
                assert!(admission.used(ResourceClass::RequestContext) > baseline);
                assert!(matches!(
                    admission.reserve(None, ResourceClass::Connection, 1),
                    Err(flow_control::Error::Overloaded)
                ));

                if end == "resume" {
                    let mut filler = vec![0; filled];
                    peer.read_exact(&mut filler).unwrap();
                    assert!(filler.iter().all(|b| *b == 0x55));
                    let mut reply = response(&reactor, &mut work, &mut peer, &codec);
                    if phase == "challenge" {
                        let finish = finish(&signers[0], signers[1].node(), reply);
                        peer.write_all(&codec.encode_head(&finish).unwrap())
                            .unwrap();
                        reply = response(&reactor, &mut work, &mut peer, &codec);
                    }
                    assert_eq!(
                        reply.unique("racer-handshake").unwrap(),
                        Some(b"ready".as_slice())
                    );
                    // The server has completed control I/O and now awaits the
                    // application head under the same initial four-second cap.
                    drive(
                        &reactor,
                        futures::future::poll_fn(|_| {
                            assert!(poll(work.as_mut()).is_pending());
                            if admission.used(ResourceClass::ControlProgress) == 0 {
                                Poll::Ready(())
                            } else {
                                Poll::Pending
                            }
                        }),
                    );
                    scope.cancel().unwrap();
                } else if matches!(end, "expiry" | "handshake-cap") {
                    clock.advance(Duration::from_secs(if end == "handshake-cap" {
                        2
                    } else {
                        1
                    }));
                    assert_eq!(scope.check(), Ok(()));
                } else if end == "cancel" {
                    scope.cancel().unwrap();
                }
                if matches!(end, "expiry" | "handshake-cap" | "cancel") {
                    // Deadline/cancellation alone cannot release kernel owners.
                    assert_eq!(reactor.in_flight(), 1);
                    assert_eq!(admission.used(ResourceClass::Connection), 1);
                    assert_eq!(admission.used(ResourceClass::ControlProgress), 1);
                    assert!(admission.used(ResourceClass::RequestContext) > baseline);
                }
                if end != "drop" {
                    let result = drive(&reactor, work.as_mut());
                    let expected = if matches!(end, "expiry" | "handshake-cap") {
                        Error::DeadlineExceeded
                    } else {
                        Error::Cancelled
                    };
                    assert!(
                        matches!(result, Err(error) if error == expected),
                        "{phase}/{end}"
                    );
                }
                drop(work);
                if end == "drop" {
                    // No CQE has been driven since abandonment: all three charges
                    // must remain, even though the waiting future is gone.
                    assert_eq!(reactor.in_flight(), 1);
                    assert_eq!(admission.used(ResourceClass::Connection), 1);
                    assert_eq!(admission.used(ResourceClass::ControlProgress), 1);
                    assert!(admission.used(ResourceClass::RequestContext) > baseline);
                }
                drive(&reactor, reactor.drain()).unwrap();
                assert_eq!(reactor.in_flight(), 0);
                assert_eq!(admission.used(ResourceClass::Connection), 0);
                assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
                assert_eq!(
                    admission.used(ResourceClass::RequestContext),
                    baseline + server.io.retained_buffer_bytes()
                );
                let (next, _peer) = UnixStream::pair().unwrap();
                let admitted = crate::http::from_accepted(next.into(), &admission).unwrap();
                drop(admitted);
            }
        }
    }
}
#[cfg(test)]
use uring_runtime::environment::SimulationClock;
