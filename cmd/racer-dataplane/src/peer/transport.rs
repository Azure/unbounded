//! Transport-neutral ciphertext lifecycle, selecting HTTP or authenticated RDMA.
use super::protocol::{PeerResponse, SecurityCodec, SignedRequest, SignedResponse, WireCodec};
use crate::telemetry::failures::{BodyProgress, Detail, Failure, Stage, timestamp};
use crate::{
    error::{Error, Operation, Result},
    http::{io::HttpIo, pool::HttpPool},
    memory::pool::CiphertextPage,
    model::ResourceClass,
    rdma::RdmaTransfer,
    runtime::deadline::RequestScope,
    runtime::{
        admission::{Admission, Reservation},
        reactor::IoBuffer,
    },
    topology::rails::TransportPlan,
};
use std::rc::Rc;

/// Stable, quota-owned transport staging. Never contains plaintext page data.
pub(crate) struct WireBuffer {
    bytes: Box<[u8]>,
    _reservation: Reservation,
}
impl WireBuffer {
    pub(crate) fn new(admission: &Admission, length: usize) -> Result<Self> {
        if length > crate::model::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        let reservation = admission.reserve(None, ResourceClass::Ciphertext, length)?;
        Ok(Self {
            bytes: reservation.buffer(length)?.into_boxed_slice(),
            _reservation: reservation,
        })
    }
    pub(crate) fn reserved(reservation: Reservation, length: usize) -> Result<Self> {
        reservation.validate(ResourceClass::Ciphertext, length)?;
        if length > crate::model::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        Ok(Self {
            bytes: reservation.buffer(length)?.into_boxed_slice(),
            _reservation: reservation,
        })
    }
    pub(crate) fn into_parts(self) -> (Vec<u8>, Reservation) {
        (self.bytes.into_vec(), self._reservation)
    }
}
impl crate::runtime::reactor::sealed::Sealed for WireBuffer {}
impl IoBuffer for WireBuffer {
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.bytes)
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.bytes)
    }
}

type ReclaimCiphertext = dyn Fn(&crate::model::CacheId, usize);

/// Generic Io has lost errno and may mean local ENOBUFS/ENOMEM. It is not link
/// evidence. Only an observed orderly EOF is classified here; connect preserves
/// its errno separately in HttpPool.
fn observe_read(
    result: Result<usize>,
    permit: &Option<std::sync::Arc<super::adaptive::Permit>>,
    failure: &Rc<std::cell::Cell<bool>>,
    scope: &RequestScope,
) -> Result<usize> {
    if matches!(result, Ok(0)) && scope.check().is_ok() {
        failure.set(true);
        if let Some(permit) = permit {
            permit.observe(super::adaptive::Outcome::PeerFailure);
        }
    }
    result
}

#[cfg(test)]
#[test]
fn adaptive_socket_attribution_ignores_local_pressure_and_expired_scope() {
    let scope = RequestScope::new(
        crate::model::RequestId([91; 16]),
        crate::runtime::environment::now() + std::time::Duration::from_secs(10),
    )
    .unwrap();
    let owner =
        super::adaptive::AdaptivePeers::new(Default::default(), Default::default()).unwrap();
    let node = crate::model::NodeId("peer".into());
    let permit = Some(owner.acquire(&node).unwrap());
    let failed = Rc::new(std::cell::Cell::new(false));
    for error in [
        Error::Overloaded,
        Error::Unavailable,
        Error::DeadlineExceeded,
        Error::Cancelled,
        Error::InvalidRequest,
        Error::Io,
    ] {
        assert_eq!(
            observe_read(Err(error), &permit, &failed, &scope),
            Err(error)
        );
        assert!(!failed.get());
        assert!(owner.available(&node));
    }
    assert_eq!(observe_read(Ok(0), &permit, &failed, &scope), Ok(0));
    assert!(failed.get());
    assert!(!owner.available(&node));
    failed.set(false);
    scope.cancel().unwrap();
    assert_eq!(observe_read(Ok(0), &permit, &failed, &scope), Ok(0));
    assert!(!failed.get());
}

/// Internal transport result: native delivery stays materialized; HTTP relay
/// delivery owns an unfinished connection and its exact opaque body framing.
pub enum RelayResponse {
    Complete(SignedResponse),
    Http {
        authentication: crate::security::forwarding::ForwardedHead,
        connection: Box<crate::http::pool::ConnectionLease>,
        length: usize,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Capabilities {
    pub rdma: bool,
    pub scoped_grants: bool,
}

pub struct Transfers {
    reclaim: Option<Rc<ReclaimCiphertext>>,
    signatures: Rc<crate::security::signing::Signatures>,
    #[cfg(test)]
    pub(super) native_completions: std::cell::Cell<usize>,
    #[cfg(test)]
    pub(super) native_completed: std::cell::Cell<usize>,
    #[cfg(test)]
    pub(super) native_fallbacks: std::cell::Cell<usize>,
    pub(super) http: Rc<HttpPool>,
    pub(super) io: Rc<HttpIo>,
    pub(super) rdma: Option<Rc<RdmaTransfer>>,
    pub(super) wire: (Rc<Admission>, Rc<SecurityCodec>),
    pub(super) native: Option<(
        Rc<crate::security::signing::Signatures>,
        Rc<crate::rdma::session::Sessions>,
    )>,
}
impl Transfers {
    #[cfg(test)]
    pub(crate) fn transport_io(&self) -> &Rc<HttpIo> {
        &self.io
    }
    /// Authentication and charged decoding are mandatory, even for HTTP-only peers.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{http::{io::HttpIo, pool::HttpPool}, peer::transport::Transfers};
    /// use std::rc::Rc;
    /// fn unsigned(pool: Rc<HttpPool>, io: Rc<HttpIo>) {
    ///     let _ = Transfers::new(pool, io, None);
    /// }
    /// ```
    pub fn new(
        http: Rc<HttpPool>,
        io: Rc<HttpIo>,
        rdma: Option<Rc<RdmaTransfer>>,
        admission: Rc<Admission>,
        codec: Rc<SecurityCodec>,
        signatures: Rc<crate::security::signing::Signatures>,
    ) -> Self {
        Self {
            reclaim: None,
            signatures,
            #[cfg(test)]
            native_completions: std::cell::Cell::new(0),
            #[cfg(test)]
            native_completed: std::cell::Cell::new(0),
            #[cfg(test)]
            native_fallbacks: std::cell::Cell::new(0),
            http,
            io,
            rdma,
            wire: (admission, codec),
            native: None,
        }
    }
    pub fn with_native(mut self, sessions: Rc<crate::rdma::session::Sessions>) -> Self {
        self.native = Some((self.signatures.clone(), sessions));
        self
    }
    pub(crate) fn with_reclamation(
        mut self,
        reclaim: impl Fn(&crate::model::CacheId, usize) + 'static,
    ) -> Self {
        self.reclaim = Some(Rc::new(reclaim));
        self
    }
    /// Route selection alone never authorizes RDMA. A matching, live authenticated
    /// single-use session and transfer-scoped grant are both required.
    pub fn select(
        &self,
        proposed: TransportPlan,
        capabilities: Capabilities,
        session: Option<&crate::rdma::session::SessionLease>,
    ) -> TransportPlan {
        match proposed {
            TransportPlan::Rdma { rail }
                if capabilities.rdma
                    && capabilities.scoped_grants
                    && self.rdma.as_ref().is_some_and(|rdma| rdma.ready(rail))
                    && session.is_some_and(|s| s.rail() == rail && s.ready()) =>
            {
                TransportPlan::Rdma { rail }
            }
            _ => TransportPlan::Http,
        }
    }

    pub fn send_scoped<'a>(
        &'a self,
        destination: &'a crate::model::NodeId,
        session: &'a crate::rdma::session::SessionLease,
        page: CiphertextPage,
        descriptor: crate::rdma::permission::AuthenticatedDescriptor,
        scope: &'a RequestScope,
    ) -> Operation<'a, crate::rdma::SendCompletion> {
        Box::pin(async move {
            scope.check()?;
            if session.peer() != destination {
                return Err(Error::Unauthorized);
            }
            self.rdma
                .as_ref()
                .ok_or(Error::Unavailable)?
                .send_to(session, page, descriptor, scope)
                .await
        })
    }

    pub fn prepare_receive<'a>(
        &'a self,
        source: &'a crate::model::NodeId,
        session: &'a crate::rdma::session::SessionLease,
        envelope: &'a crate::model::PageEnvelope,
        transfer: crate::model::TransferId,
        scope: &'a RequestScope,
    ) -> Operation<'a, crate::rdma::permission::Grant> {
        Box::pin(async move {
            if session.peer() != source {
                return Err(Error::Unauthorized);
            }
            self.rdma
                .as_ref()
                .ok_or(Error::Unavailable)?
                .prepare_receive(session, envelope, transfer, scope)
                .await
        })
    }

    pub fn finish_receive<'a>(
        &'a self,
        session: &'a crate::rdma::session::SessionLease,
        grant: crate::rdma::permission::Grant,
        completion: &'a crate::security::signing::VerifiedHead,
        envelope: crate::model::PageEnvelope,
        scope: &'a RequestScope,
    ) -> Operation<'a, CiphertextPage> {
        Box::pin(async move {
            let (admission, _) = &self.wire;
            self.rdma
                .as_ref()
                .ok_or(Error::Unavailable)?
                .finish_receive(session, grant, completion, envelope, admission, scope)
                .await
        })
    }
    /// The signed envelope and HTTP ciphertext share one exclusive pooled socket.
    /// A failed/abandoned exchange is never marked reusable.
    pub fn exchange<'a>(
        &'a self,
        endpoint: crate::http::pool::Endpoint,
        request: SignedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        self.exchange_planned(endpoint, request, TransportPlan::Http, scope)
    }
    pub fn exchange_planned<'a>(
        &'a self,
        endpoint: crate::http::pool::Endpoint,
        request: SignedRequest,
        plan: TransportPlan,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            match self
                .exchange_inner(
                    endpoint,
                    request,
                    plan,
                    None,
                    None,
                    Rc::new(std::cell::Cell::new(false)),
                    scope,
                )
                .await?
            {
                RelayResponse::Complete(response) => Ok(response),
                RelayResponse::Http { .. } => Err(Error::Internal),
            }
        })
    }
    pub(crate) fn exchange_inner<'a>(
        &'a self,
        endpoint: crate::http::pool::Endpoint,
        request: SignedRequest,
        plan: TransportPlan,
        relay: Option<Rc<Reservation>>,
        peer_admission: Option<std::sync::Arc<super::adaptive::Permit>>,
        failure: Rc<std::cell::Cell<bool>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, RelayResponse> {
        Box::pin(async move {
            scope.check()?;
            let (admission, codec) = &self.wire;
            let head_size = std::iter::once(request.authentication.original.as_ref())
                .chain(request.authentication.hops.iter())
                .try_fold(0usize, |total, signed| {
                    signed.head.headers.iter().try_fold(total, |n, h| {
                        n.checked_add(h.value.len() + h.name.len())
                            .ok_or(Error::InvalidRequest)
                    })
                })?;
            let _head_reservation = admission.reserve(
                None,
                ResourceClass::RequestContext,
                head_size
                    .checked_mul(3)
                    .ok_or(Error::InvalidRequest)?
                    .max(1),
            )?;
            let mut head = WireCodec::encode(&request.authentication, false, 0)?;
            let signatures = self.signatures.clone();
            let peer = crate::security::signing::receiver(
                &request
                    .authentication
                    .hops
                    .last()
                    .unwrap_or(&request.authentication.original)
                    .head,
            )?;
            let observer = admission.observer();
            let mut connection = observer.result(
                Stage::PeerCheckout,
                scope,
                self.http
                    .checkout_peer(
                        &endpoint,
                        relay.clone(),
                        peer_admission.clone(),
                        Some(failure.clone()),
                        scope,
                    )
                    .await,
            )?;
            connection.peer_admission = peer_admission.clone();
            connection.relay_reservation = relay.clone();
            // Connection and handshake get separate bounded idle allowances.
            // No header byte can renew the request/head allowance.
            scope.candidate_progress()?;
            let connection = {
                let _permit = connection
                    .session
                    .is_none()
                    .then(|| admission.reserve(None, ResourceClass::ControlProgress, 1))
                    .transpose()?;
                observer.result(
                    Stage::PeerHandshake,
                    scope,
                    crate::security::connection::connect(
                        &self.io, connection, signatures, &peer, scope,
                    )
                    .await,
                )?
            };
            let native = self.accept_native(&request, plan, scope)?;
            scope.candidate_progress()?;
            if let Some((_, accept, _)) = &native {
                super::native::attach(&mut head, accept)?;
            }
            let sent = observer.result(
                Stage::PeerHead,
                scope,
                self.io.send_head(connection, head, scope).await,
            )?;
            let mut received = observer.result(
                Stage::PeerHead,
                scope,
                self.io.receive_head(sent.connection, scope).await,
            )?;
            let control = super::native::detach(&mut received.value)?;
            let relay_context = if relay.is_some() {
                let size = received.value.headers.iter().try_fold(0usize, |n, h| {
                    n.checked_add(h.value.len() + h.name.len())
                        .ok_or(Error::InvalidRequest)
                })?;
                Some(admission.reserve(
                    None,
                    ResourceClass::RequestContext,
                    size.checked_mul(3).ok_or(Error::InvalidRequest)?.max(1),
                )?)
            } else {
                None
            };
            let (authentication, length) = WireCodec::decode(received.value, true)?;
            if let Some(control) = control {
                let (binding, accept, peer) = native.ok_or(Error::Unauthorized)?;
                if length != 0 {
                    return Err(Error::InvalidRequest);
                }
                return self
                    .receive_native(
                        received.connection,
                        authentication,
                        binding,
                        accept,
                        peer,
                        control,
                        scope,
                    )
                    .await
                    .map(RelayResponse::Complete);
            }
            if relay.is_some() {
                received.connection.relay_context = relay_context;
                return Ok(RelayResponse::Http {
                    authentication,
                    connection: Box::new(received.connection),
                    length,
                });
            }
            let mut connection = received.connection;
            let (body, staging_reservation) = if length == 0 {
                (Vec::new(), None)
            } else {
                let cache = &request.request.origin.object.cache;
                let mut reservation =
                    admission.reserve(Some(cache), ResourceClass::Ciphertext, length);
                if matches!(reservation, Err(Error::Overloaded))
                    && let Some(reclaim) = &self.reclaim
                {
                    reclaim(cache, length);
                    reservation = admission.reserve(Some(cache), ResourceClass::Ciphertext, length);
                }
                let mut buffer = WireBuffer::reserved(
                    observer.result(Stage::PeerReceiveAdmission, scope, reservation)?,
                    length,
                )?;
                let mut offset = 0;
                let mut first = None;
                let mut last = None;
                let mut reads = 0u32;
                // Capture numeric identity once; failed I/O consumes the lease.
                // Do not retain the descriptor or extend socket/quota ownership.
                let tuple = connection.socket().tcp_tuple();
                let record = |error,
                              offset,
                              first: Option<std::time::Instant>,
                              last: Option<std::time::Instant>,
                              reads| {
                    let mut remote = [b'?'; 36];
                    if crate::security::certificates::canonical_uuid(&peer.0) {
                        remote.copy_from_slice(peer.0.as_bytes());
                    }
                    observer.record(
                        Failure::new(Stage::PeerReceiveBody, error)
                            .request(scope)
                            .attempt(request.request.route.attempt)
                            .detail(Detail::Body(BodyProgress {
                                received: offset as u32,
                                expected: length as u32,
                                reads,
                                first: first.map(timestamp).unwrap_or_default(),
                                last: last.map(timestamp).unwrap_or_default(),
                                now: timestamp(crate::runtime::environment::now()),
                                original: scope
                                    .body_deadlines
                                    .map(|d| timestamp(d.0))
                                    .unwrap_or_default(),
                                share: scope
                                    .body_deadlines
                                    .map(|d| timestamp(d.1))
                                    .unwrap_or_default(),
                                signed: timestamp(request.request.route.deadline.0),
                                remote,
                                tuple,
                            })),
                    );
                };
                while offset < length {
                    let completion = match self
                        .io
                        .read_body_range(connection, buffer, offset..length, scope)
                        .await
                    {
                        Ok(completion) => completion,
                        Err(error) => {
                            record(error, offset, first, last, reads);
                            return Err(error);
                        }
                    };
                    let bytes =
                        observe_read(Ok(completion.bytes), &peer_admission, &failure, scope)?;
                    if bytes == 0 || bytes > length - offset {
                        record(Error::Io, offset, first, last, reads);
                        return Err(Error::Io);
                    }
                    offset += completion.bytes;
                    let now = crate::runtime::environment::now();
                    first.get_or_insert(now);
                    last = Some(now);
                    reads = reads.saturating_add(1);
                    if let Err(error) = scope.candidate_body_progress(offset, length) {
                        record(error, offset, first, last, reads);
                        return Err(error);
                    }
                    connection = completion.lease;
                    buffer = completion.buffer;
                }
                let (bytes, reservation) = buffer.into_parts();
                (bytes, Some(reservation))
            };
            scope.check()?;
            let response = observer.result(
                Stage::PeerDecode,
                scope,
                codec.response_reserved(authentication, body, staging_reservation, scope),
            )?;
            // Logical decoding must account for every body byte before pooling.
            match &response.response {
                PeerResponse::Bootstrap {
                    page_zero: Some(ciphertext),
                    ..
                } if ciphertext.bytes().len() == length => {}
                PeerResponse::Bootstrap {
                    page_zero: Some(_), ..
                } => return Err(Error::InvalidRequest),
                PeerResponse::Page { ciphertext, .. }
                | PeerResponse::Selected { ciphertext, .. }
                    if ciphertext.bytes().len() == length => {}
                PeerResponse::Page { .. } | PeerResponse::Selected { .. } => {
                    return Err(Error::InvalidRequest);
                }
                _ if length == 0 => {}
                _ => return Err(Error::InvalidRequest),
            }
            connection.finish_exchange()?;
            Ok(RelayResponse::Complete(response))
        })
    }
    pub fn send<'a>(
        &'a self,
        _destination: &'a crate::model::NodeId,
        _transfer: crate::model::TransferId,
        _plan: TransportPlan,
        _page: CiphertextPage,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            _scope.check()?;
            // HTTP pages are carried by exchange/respond with their signed head.
            // An unbound standalone transfer cannot safely report success.
            Err(Error::InvalidRequest)
        })
    }
    pub fn receive<'a>(
        &'a self,
        _source: &'a crate::model::NodeId,
        _transfer: crate::model::TransferId,
        _plan: TransportPlan,
        _envelope: crate::model::PageEnvelope,
        _scope: &'a RequestScope,
    ) -> Operation<'a, CiphertextPage> {
        Box::pin(async move {
            _scope.check()?;
            Err(Error::InvalidRequest)
        })
    }
}
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wire_checkout_reuses_zeroed_payload_without_moving_or_releasing_its_charge() {
        use crate::model::CacheId;
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
        let first = CacheId("first".into());
        let second = CacheId("second".into());
        let length = 1 << 20;
        for reserved in [false, true] {
            let mut old = admission
                .reserve(Some(&first), ResourceClass::Plaintext, length)
                .unwrap();
            let mut bytes = old.buffer(length).unwrap();
            bytes.fill(0xa7);
            bytes.truncate(1);
            let pointer = bytes.as_ptr();
            old.recycle(bytes);
            drop(old);
            let mut buffer = if reserved {
                WireBuffer::reserved(
                    admission
                        .reserve(Some(&second), ResourceClass::Ciphertext, length)
                        .unwrap(),
                    length,
                )
            } else {
                WireBuffer::new(&admission, length)
            }
            .unwrap();
            assert_eq!(buffer.bytes().unwrap().as_ptr(), pointer);
            assert_eq!(buffer.bytes().unwrap().len(), length);
            assert!(buffer.bytes().unwrap().iter().all(|byte| *byte == 0));
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), length);
            assert_eq!(admission.retained_buffer_bytes(), 0);
            buffer.bytes_mut().unwrap()[..3].copy_from_slice(b"abc");
            let (bytes, mut reservation) = buffer.into_parts();
            assert_eq!(bytes.as_ptr(), pointer);
            assert_eq!(&bytes[..3], b"abc");
            assert_eq!(reservation.cache(), reserved.then_some(&second));
            assert_eq!(reservation.amount(), length);
            assert_eq!(admission.used(ResourceClass::Ciphertext), length);
            reservation.recycle(bytes);
            drop(reservation);
            admission.reclaim_buffers();
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }

    #[test]
    fn wire_checkout_validates_bounds_class_and_admission_before_reuse() {
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
        let length = 1 << 20;
        let mut old = admission
            .reserve(None, ResourceClass::Ciphertext, length)
            .unwrap();
        old.recycle(old.buffer(length).unwrap());
        drop(old);
        for (class, amount, requested) in [
            (ResourceClass::Plaintext, length, length),
            (ResourceClass::Ciphertext, length - 1, length),
            (ResourceClass::Ciphertext, (16 << 20) + 17, (16 << 20) + 17),
        ] {
            let reservation = admission.reserve(None, class, amount).unwrap();
            assert!(WireBuffer::reserved(reservation, requested).is_err());
            assert_eq!(admission.retained_buffer_bytes(), length);
            assert_eq!(admission.used(ResourceClass::Ciphertext), length);
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        }
        assert!(matches!(
            WireBuffer::new(&admission, (16 << 20) + 17),
            Err(Error::InvalidRequest)
        ));
        assert!(matches!(
            WireBuffer::new(&admission, 0),
            Err(Error::InvalidConfiguration)
        ));
        // A pool miss still exposes only initialized bytes.
        let fresh = WireBuffer::new(&admission, 3).unwrap();
        assert_eq!(fresh.bytes().unwrap(), &[0; 3]);
        drop(fresh);
        admission.stop();
        assert!(matches!(
            WireBuffer::new(&admission, length),
            Err(Error::Unavailable)
        ));
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    #[ignore = "opt-in same-workload wire buffer checkout benchmark"]
    fn wire_checkout_benchmark() {
        use std::{hint::black_box, time::Instant};
        const ITERATIONS: usize = 128;
        for length in [1 << 20, 16 << 20, (16 << 20) + 16] {
            for reserved in [false, true] {
                let admission = Admission::new(crate::test_support::cluster::config(false).limits);
                for sample in 0..6 {
                    let start = Instant::now();
                    for _ in 0..ITERATIONS {
                        let mut buffer = if reserved {
                            WireBuffer::reserved(
                                admission
                                    .reserve(None, ResourceClass::Ciphertext, length)
                                    .unwrap(),
                                length,
                            )
                        } else {
                            WireBuffer::new(&admission, length)
                        }
                        .unwrap();
                        buffer.bytes_mut().unwrap().fill(black_box(0xa7));
                        black_box(buffer.bytes().unwrap());
                        let (bytes, mut reservation) = buffer.into_parts();
                        reservation.recycle(bytes);
                        drop(reservation);
                    }
                    let elapsed = start.elapsed();
                    if sample != 0 {
                        println!(
                            "wire_checkout length={length} reserved={reserved} sample={sample} iterations={ITERATIONS} ns_per_op={:.0}",
                            elapsed.as_nanos() as f64 / ITERATIONS as f64,
                        );
                    }
                }
                admission.reclaim_buffers();
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            }
        }
    }
}
