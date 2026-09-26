//! Transport-neutral ciphertext lifecycle, selecting HTTP or authenticated RDMA.
use super::wire::{LogicalCodec, PeerResponse, SignedRequest, SignedResponse, WireCodec};
use crate::{
    error::{Error, Operation, Result},
    http::{io::HttpIo, pool::HttpPool},
    memory::pool::CiphertextPage,
    model::limits::ResourceClass,
    rdma::transfer::RdmaTransfer,
    runtime::deadline::RequestScope,
    runtime::{
        admission::{Admission, Reservation},
        reactor::IoBuffer,
    },
    topology::rails::TransportPlan,
};
use std::rc::Rc;

/// Read-only send owner. The reactor retains the page and its existing charge
/// through partial sends and cancellation completion, without a staging copy.
pub(crate) struct SendPage(pub(crate) CiphertextPage);
impl crate::runtime::reactor::sealed::Sealed for SendPage {}
impl IoBuffer for SendPage {
    fn bytes(&self) -> Result<&[u8]> {
        Ok(self.0.bytes())
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Err(Error::InvalidRequest)
    }
}

/// Stable, quota-owned transport staging. Never contains plaintext page data.
pub(crate) struct WireBuffer {
    bytes: Box<[u8]>,
    _reservation: Reservation,
}
impl WireBuffer {
    pub(crate) fn new(admission: &Admission, length: usize) -> Result<Self> {
        Self::for_cache(admission, None, length)
    }
    fn for_cache(
        admission: &Admission,
        cache: Option<&crate::model::identity::CacheId>,
        length: usize,
    ) -> Result<Self> {
        if length > crate::model::range::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        let reservation = admission.reserve(cache, ResourceClass::Ciphertext, length)?;
        Ok(Self {
            bytes: vec![0; length].into_boxed_slice(),
            _reservation: reservation,
        })
    }
    pub(crate) fn into_parts(self) -> (Vec<u8>, Reservation) {
        (self.bytes.into_vec(), self._reservation)
    }
    fn reserved(
        admission: &Admission,
        cache: &crate::model::identity::CacheId,
        length: usize,
        output: &mut Option<Reservation>,
    ) -> Result<Self> {
        if length == 0 || length > crate::model::range::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        let reservation = output.as_ref().ok_or(Error::InvalidConfiguration)?;
        if !admission.owns(reservation) || reservation.cache() != Some(cache) {
            return Err(Error::InvalidConfiguration);
        }
        reservation.validate(ResourceClass::Ciphertext, length)?;
        Ok(Self {
            bytes: vec![0; length].into_boxed_slice(),
            _reservation: output.take().ok_or(Error::Internal)?,
        })
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

pub struct Transfers {
    receive_cache: Option<Rc<crate::memory::cache::MemoryCache>>,
    receive_writer: Option<Rc<crate::store::writer::StoreWriter>>,
    #[cfg(test)]
    pub(super) native_completions: std::cell::Cell<usize>,
    #[cfg(test)]
    pub(super) native_completed: std::cell::Cell<usize>,
    #[cfg(test)]
    pub(super) native_fallbacks: std::cell::Cell<usize>,
    pub(super) http: Rc<HttpPool>,
    pub(super) io: Rc<HttpIo>,
    pub(super) rdma: Option<Rc<RdmaTransfer>>,
    pub(super) wire: Option<(Rc<Admission>, Rc<dyn LogicalCodec>)>,
    pub(super) native: Option<(
        Rc<crate::security::signing::Signatures>,
        Rc<crate::rdma::session::Sessions>,
    )>,
}
impl Transfers {
    pub fn new(http: Rc<HttpPool>, io: Rc<HttpIo>, rdma: Option<Rc<RdmaTransfer>>) -> Self {
        Self {
            receive_cache: None,
            receive_writer: None,
            #[cfg(test)]
            native_completions: std::cell::Cell::new(0),
            #[cfg(test)]
            native_completed: std::cell::Cell::new(0),
            #[cfg(test)]
            native_fallbacks: std::cell::Cell::new(0),
            http,
            io,
            rdma,
            wire: None,
            native: None,
        }
    }
    pub fn with_native(
        mut self,
        signatures: Rc<crate::security::signing::Signatures>,
        sessions: Rc<crate::rdma::session::Sessions>,
    ) -> Self {
        self.native = Some((signatures, sessions));
        self
    }
    pub fn with_wire(mut self, admission: Rc<Admission>, codec: Rc<dyn LogicalCodec>) -> Self {
        self.wire = Some((admission, codec));
        self
    }
    /// Peer traffic shares the worker's byte quota with retained local fills.
    /// Reclaim disposable copies before rejecting an already arriving response.
    pub fn with_receive_reclamation(
        mut self,
        memory: Rc<crate::memory::cache::MemoryCache>,
        writer: Rc<crate::store::writer::StoreWriter>,
    ) -> Self {
        self.receive_cache = Some(memory);
        self.receive_writer = Some(writer);
        self
    }
    pub(super) fn receive_buffer(
        &self,
        admission: &Admission,
        cache: &crate::model::identity::CacheId,
        length: usize,
    ) -> Result<WireBuffer> {
        match WireBuffer::for_cache(admission, Some(cache), length) {
            Err(Error::Overloaded) => {
                if let Some(writer) = &self.receive_writer {
                    writer.discard_unsubmitted();
                }
                // Each pass removes an idle entry, so this is bounded by the
                // existing cache size. Retry the actual ciphertext dimension:
                // evict_idle reports plaintext plus ciphertext, including slack.
                loop {
                    match WireBuffer::for_cache(admission, Some(cache), length) {
                        Err(Error::Overloaded) => {
                            let Some(memory) = &self.receive_cache else {
                                return Err(Error::Overloaded);
                            };
                            if memory.evict_idle(1)? == 0 {
                                return Err(Error::Overloaded);
                            }
                        }
                        result => return result,
                    }
                }
            }
            result => result,
        }
    }
    pub fn exchange_probe<'a>(
        &'a self,
        endpoint: crate::http::pool::Endpoint,
        probe: Vec<u8>,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<u8>> {
        Box::pin(async move {
            use crate::http::codec::{MessageHead, StartLine};
            use crate::security::protocol as p;
            if probe.len() > 256 {
                return Err(Error::InvalidRequest);
            }
            let mut head = MessageHead {
                start: StartLine::Request {
                    method: "POST".into(),
                    target: "/racer/peer/v1/challenge".into(),
                },
                headers: Vec::new(),
            };
            p::push(&mut head, "content-length", 0);
            p::push_binary(&mut head, "racer-probe", &probe);
            let connection = self.http.checkout(&endpoint, scope).await?;
            let response = self.io.exchange_head(connection, head, scope).await?;
            if !matches!(response.value.start, StartLine::Response { status: 200 }) {
                return Err(Error::Unauthorized);
            }
            let body = self
                .io
                .collect_body(
                    response.connection,
                    crate::security::session::MAX_CHALLENGE_REPLY,
                    scope,
                )
                .await?;
            let bytes = body.buffer.bytes()?.to_vec();
            let mut connection = body.lease;
            connection.finish_exchange()?;
            Ok(bytes)
        })
    }
    /// Route selection alone never authorizes RDMA. A matching, live authenticated
    /// single-use session and transfer-scoped grant are both required.
    pub fn select(
        &self,
        proposed: TransportPlan,
        capabilities: super::handshake::Capabilities,
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
        destination: &'a crate::model::identity::NodeId,
        session: &'a crate::rdma::session::SessionLease,
        page: CiphertextPage,
        descriptor: crate::rdma::permission::AuthenticatedDescriptor,
        scope: &'a RequestScope,
    ) -> Operation<'a, crate::rdma::transfer::SendCompletion> {
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
        source: &'a crate::model::identity::NodeId,
        session: &'a crate::rdma::session::SessionLease,
        envelope: &'a crate::model::envelope::PageEnvelope,
        transfer: crate::model::identity::TransferId,
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
        envelope: crate::model::envelope::PageEnvelope,
        scope: &'a RequestScope,
    ) -> Operation<'a, CiphertextPage> {
        Box::pin(async move {
            let (admission, _) = self.wire.as_ref().ok_or(Error::InvalidConfiguration)?;
            self.rdma
                .as_ref()
                .ok_or(Error::Unavailable)?
                .finish_receive(session, grant, completion, envelope, admission, scope)
                .await
        })
    }
    pub fn exchange_head<'a>(
        &'a self,
        endpoint: crate::http::pool::Endpoint,
        head: crate::security::signing::SignedHead,
        scope: &'a RequestScope,
    ) -> Operation<'a, crate::security::signing::SignedHead> {
        Box::pin(async move {
            scope.check()?;
            let envelope = crate::security::forwarding::ForwardedHead {
                original: std::sync::Arc::new(head),
                hops: Vec::new(),
            };
            let head = WireCodec::encode(&envelope, false, 0)?;
            let connection = self.http.checkout(&endpoint, scope).await?;
            let sent = self.io.send_head(connection, head, scope).await?;
            let received = self.io.receive_head(sent.connection, scope).await?;
            let (response, length) = WireCodec::decode(received.value, true)?;
            if length != 0 || !response.hops.is_empty() {
                return Err(Error::InvalidRequest);
            }
            let mut connection = received.connection;
            connection.finish_exchange()?;
            std::sync::Arc::try_unwrap(response.original).map_err(|_| Error::InvalidRequest)
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
            self.exchange_reserved(endpoint, request, plan, scope, &mut None)
                .await
        })
    }
    /// HTTP receive consumes an admitted Fill output only for a nonempty body.
    /// Misses and failures before body admission leave it available for another
    /// candidate or origin. Once submitted, reactor completion owns its lifetime.
    pub(crate) fn exchange_reserved<'a>(
        &'a self,
        endpoint: crate::http::pool::Endpoint,
        request: SignedRequest,
        plan: TransportPlan,
        scope: &'a RequestScope,
        output: &'a mut Option<Reservation>,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            scope.check()?;
            let (admission, codec) = self.wire.as_ref().ok_or(Error::InvalidConfiguration)?;
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
            let native = self.accept_native(&request, plan, scope)?;
            if let Some((_, accept, _)) = &native {
                super::native::attach(&mut head, accept)?;
            }
            let connection = self.http.checkout(&endpoint, scope).await?;
            let sent = self.io.send_head(connection, head, scope).await?;
            let mut received = self.io.receive_head(sent.connection, scope).await?;
            let control = super::native::detach(&mut received.value)?;
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
                    .await;
            }
            let mut connection = received.connection;
            let (body, reservation) = if length == 0 {
                (Vec::new(), None)
            } else {
                let cache = &request.request.origin.object.cache;
                let mut buffer = if output.is_some() {
                    WireBuffer::reserved(admission, cache, length, output)?
                } else {
                    self.receive_buffer(admission, cache, length)?
                };
                let mut offset = 0;
                while offset < length {
                    let completion = self
                        .io
                        .read_body_range(connection, buffer, offset..length, scope)
                        .await?;
                    if completion.bytes == 0 || completion.bytes > length - offset {
                        return Err(Error::Io);
                    }
                    offset += completion.bytes;
                    connection = completion.lease;
                    buffer = completion.buffer;
                }
                let (bytes, reservation) = buffer.into_parts();
                (bytes, Some(reservation))
            };
            scope.check()?;
            // Decoding moves this allocation; transfer its charge too. Reserving
            // it again can reject an already received page under fleet pressure.
            let response = codec.response_reserved(authentication, body, reservation, scope)?;
            // Logical decoding must account for every body byte before pooling.
            match &response.response {
                PeerResponse::Page { ciphertext, .. } if ciphertext.bytes().len() == length => {}
                PeerResponse::Page { .. } => return Err(Error::InvalidRequest),
                _ if length == 0 => {}
                _ => return Err(Error::InvalidRequest),
            }
            connection.finish_exchange()?;
            Ok(response)
        })
    }
    pub fn send<'a>(
        &'a self,
        _destination: &'a crate::model::identity::NodeId,
        _transfer: crate::model::identity::TransferId,
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
        _source: &'a crate::model::identity::NodeId,
        _transfer: crate::model::identity::TransferId,
        _plan: TransportPlan,
        _envelope: crate::model::envelope::PageEnvelope,
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
    fn reserved_receive_validates_before_taking_the_output() {
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
        let other = Admission::new(crate::test_support::cluster::config(false).limits);
        let cache = crate::model::identity::CacheId("cache".into());
        let wrong = crate::model::identity::CacheId("wrong".into());
        for case in ["valid", "owner", "cache", "class", "short", "zero", "long"] {
            let owner = if case == "owner" { &other } else { &admission };
            let class = if case == "class" {
                ResourceClass::Plaintext
            } else {
                ResourceClass::Ciphertext
            };
            let mut output = Some(
                owner
                    .reserve(
                        Some(if case == "cache" { &wrong } else { &cache }),
                        class,
                        32,
                    )
                    .unwrap(),
            );
            let length = match case {
                "short" => 33,
                "zero" => 0,
                "long" => crate::model::range::PAGE_BYTES as usize + 17,
                _ => 32,
            };
            let result = WireBuffer::reserved(&admission, &cache, length, &mut output);
            if case == "valid" {
                assert!(output.is_none());
                assert_eq!(admission.used(class), 32);
                drop(result.unwrap());
            } else {
                assert!(result.is_err(), "{case}");
                assert!(output.is_some(), "{case}");
            }
            drop(output);
            assert_eq!(admission.used(class), 0, "{case}");
            assert_eq!(other.used(class), 0, "{case}");
        }
    }
    use crate::{
        memory::pool::BufferPool,
        model::{
            envelope::{KeyId, Nonce, PageEnvelope},
            identity::*,
        },
        runtime::reactor::Reactor,
    };
    use std::{
        os::unix::net::UnixStream,
        task::{Context, Poll},
        time::{Duration, Instant},
    };

    #[test]
    fn send_page_is_immutable_and_completion_owned() {
        for outcome in ["complete", "cancel", "drop", "deadline"] {
            let admission = Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            ));
            let reactor = Reactor::new(admission.clone());
            let length = 1024 * 1024 + 16;
            let cache = CacheId("cache".into());
            let page = BufferPool::new(admission.clone())
                .ciphertext(
                    admission
                        .reserve(Some(&cache), ResourceClass::Ciphertext, length)
                        .unwrap(),
                    PageEnvelope {
                        page: PageId {
                            version: ObjectVersion {
                                object: ObjectId {
                                    cache,
                                    key: CacheKey([1; 32]),
                                },
                                etag: StrongEtag::parse(b"\"v1\"").unwrap(),
                            },
                            number: PageNumber(0),
                        },
                        key_id: KeyId([1; 16]),
                        nonce: Nonce([2; 24]),
                        plaintext_length: (length - 16) as u32,
                        ciphertext_length: length as u32,
                    },
                    vec![91; length],
                )
                .unwrap();
            let mut buffer = SendPage(page.clone());
            assert_eq!(buffer.bytes().unwrap().as_ptr(), page.bytes().as_ptr());
            assert_eq!(buffer.bytes_mut(), Err(Error::InvalidRequest));
            drop(page);
            let (socket, mut peer) = UnixStream::pair().unwrap();
            socket.set_nonblocking(true).unwrap();
            peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
            let fd = Rc::new(std::os::fd::OwnedFd::from(socket));
            let scope =
                RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(2))
                    .unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut work = reactor.send(fd, buffer, (), &scope);
            assert!(work.as_mut().poll(&mut cx).is_pending());
            assert_eq!(admission.used(ResourceClass::Ciphertext), length);
            if outcome == "drop" {
                drop(work);
                assert_eq!(admission.used(ResourceClass::Ciphertext), length);
            } else {
                if outcome == "cancel" {
                    scope.cancel().unwrap();
                }
                if outcome == "deadline" {
                    std::thread::sleep(Duration::from_millis(2010));
                }
                let result = loop {
                    if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                        break result;
                    }
                    assert!(Instant::now() < scope.deadline.0 + Duration::from_secs(2));
                    reactor.poll_budgeted(128).unwrap();
                    reactor.wait(Duration::from_millis(1)).unwrap();
                };
                if outcome == "complete" {
                    use std::io::Read;
                    let completion = result.unwrap();
                    assert!(
                        completion.bytes > 0 && completion.bytes < length,
                        "partial socket send required"
                    );
                    let mut received = vec![0; completion.bytes];
                    peer.read_exact(&mut received).unwrap();
                    assert!(received.iter().all(|b| *b == 91));
                    assert_eq!(admission.used(ResourceClass::Ciphertext), length);
                    drop(completion);
                } else {
                    assert!(
                        matches!(result, Err(error) if error == if outcome == "cancel" { Error::Cancelled } else { Error::DeadlineExceeded })
                    );
                }
                drop(work);
            }
            let end = Instant::now() + Duration::from_secs(2);
            let mut drain = reactor.drain();
            while drain.as_mut().poll(&mut cx).is_pending() {
                assert!(Instant::now() < end);
                reactor.poll_budgeted(128).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
            assert_eq!(reactor.in_flight(), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0, "{outcome}");
        }
    }
}
