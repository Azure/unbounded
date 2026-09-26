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

/// Stable, quota-owned transport staging. Never contains plaintext page data.
pub(crate) struct WireBuffer {
    bytes: Box<[u8]>,
    _reservation: Reservation,
}
impl WireBuffer {
    pub(crate) fn new(admission: &Admission, length: usize) -> Result<Self> {
        if length > crate::model::range::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        let reservation = admission.reserve(None, ResourceClass::Ciphertext, length)?;
        Ok(Self {
            bytes: vec![0; length].into_boxed_slice(),
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

pub struct Transfers {
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
            let (body, _staging_reservation) = if length == 0 {
                (Vec::new(), None)
            } else {
                let mut buffer = WireBuffer::new(admission, length)?;
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
            let response = codec.response(authentication, body, scope)?;
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
mod tests { /* HTTP fallback, exact ciphertext preservation, bounded transfer credits. */
}
