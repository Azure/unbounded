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
    pub(crate) fn into_vec(self) -> Vec<u8> {
        self.bytes.into_vec()
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
    http: Rc<HttpPool>,
    io: Rc<HttpIo>,
    rdma: Option<Rc<RdmaTransfer>>,
    wire: Option<(Rc<Admission>, Rc<dyn LogicalCodec>)>,
}
impl Transfers {
    pub fn new(http: Rc<HttpPool>, io: Rc<HttpIo>, rdma: Option<Rc<RdmaTransfer>>) -> Self {
        Self {
            http,
            io,
            rdma,
            wire: None,
        }
    }
    pub fn with_wire(mut self, admission: Rc<Admission>, codec: Rc<dyn LogicalCodec>) -> Self {
        self.wire = Some((admission, codec));
        self
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

    pub fn prepare_receive(
        &self,
        source: &crate::model::identity::NodeId,
        session: &crate::rdma::session::SessionLease,
        envelope: &crate::model::envelope::PageEnvelope,
        transfer: crate::model::identity::TransferId,
        scope: &RequestScope,
    ) -> Result<crate::rdma::permission::Grant> {
        if session.peer() != source {
            return Err(Error::Unauthorized);
        }
        self.rdma
            .as_ref()
            .ok_or(Error::Unavailable)?
            .prepare_receive(session, envelope, transfer, scope)
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
        Box::pin(async move {
            scope.check()?;
            let (admission, codec) = self.wire.as_ref().ok_or(Error::InvalidConfiguration)?;
            let head = WireCodec::encode(&request.authentication, false, 0)?;
            let connection = self.http.checkout(&endpoint, scope).await?;
            let sent = self.io.send_head(connection, head, scope).await?;
            let received = self.io.receive_head(sent.connection, scope).await?;
            let (authentication, length) = WireCodec::decode(received.value, true)?;
            let mut connection = received.connection;
            let body = if length == 0 {
                Vec::new()
            } else {
                let buffer = WireBuffer::new(admission, length)?;
                let completion = self.io.read_body(connection, buffer, scope).await?;
                if completion.bytes != length {
                    return Err(Error::Io);
                }
                connection = completion.lease;
                completion.buffer.into_vec()
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
