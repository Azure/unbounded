//! Transport-neutral ciphertext lifecycle, selecting HTTP or authenticated RDMA.
use super::protocol::{PeerResponse, SecurityCodec, SignedRequest, SignedResponse, WireCodec};
use crate::telemetry::failures::{BodyProgress, Detail, Failure, Stage, timestamp};
use crate::{
    error::{Error, Operation, Result},
    http::{
        connection::HttpIo,
        connection::{ConnectionLease, HttpPool},
    },
    model::{NodeId, ResourceClass, TransferId},
    rdma::RdmaTransfer,
    rdma::{
        AuthenticatedDescriptor, COMPLETION_HEADER, DESCRIPTOR_HEADER, SETUP_BINDING_HEADER,
        SETUP_HEADER, SetupParameters,
    },
    runtime::deadline::RequestScope,
    runtime::{
        admission::{Admission, Reservation},
        reactor::IoBuffer,
    },
    security::{
        forwarding::ForwardedHead,
        signing::{Signatures, SignedHead, VerifiedHead, signed_digest},
    },
    topology::rails::{RailId, TransportPlan},
};
use crate::{
    http::{Header, MessageHead, StartLine},
    security::protocol as p,
};
use std::{rc::Rc, sync::Arc, time::Duration};

#[cfg(test)]
mod native_control_tests;
#[cfg(test)]
mod native_exchange_tests;

const HEADER: &str = "racer-payload-control";
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Binding {
    pub request: [u8; 32],
    pub response: [u8; 32],
    pub transfer: TransferId,
    pub membership: u64,
    pub deadline: u64,
    pub rail: RailId,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Phase {
    Accept,
    Offer,
    Setup,
    Ready,
    Grant,
    Complete,
    Failed,
    Fallback,
    Done,
    Finish,
}
impl Phase {
    fn name(self) -> &'static str {
        match self {
            Self::Accept => "accept",
            Self::Offer => "offer",
            Self::Setup => "setup",
            Self::Ready => "ready",
            Self::Grant => "grant",
            Self::Complete => "complete",
            Self::Failed => "failed",
            Self::Fallback => "fallback",
            Self::Done => "done",
            Self::Finish => "finish",
        }
    }
    fn response(self) -> bool {
        matches!(
            self,
            Self::Offer | Self::Ready | Self::Complete | Self::Failed | Self::Finish
        )
    }
    fn fields(self) -> &'static [&'static str] {
        match self {
            Self::Offer => &["racer-rdma-setup"],
            Self::Setup | Self::Ready => &["racer-rdma-setup", "racer-rdma-setup-binding"],
            Self::Grant => &["racer-rdma-descriptor"],
            Self::Complete => &["racer-rdma-completion"],
            _ => &[],
        }
    }
}
impl Binding {
    pub fn request(
        auth: &ForwardedHead,
        membership: u64,
        scope: &RequestScope,
        rail: RailId,
    ) -> Result<Self> {
        let mut transfer = [0; 16];
        crate::runtime::environment::fill_random(&mut transfer).map_err(|_| Error::Unavailable)?;
        Ok(Self {
            request: envelope_digest(auth)?,
            response: [0; 32],
            transfer: TransferId(transfer),
            membership,
            deadline: p::encode_deadline(scope.deadline)?,
            rail,
        })
    }
    fn head(
        &self,
        phase: Phase,
        previous: &[u8; 32],
        length: usize,
        extensions: Vec<Header>,
    ) -> Result<MessageHead> {
        if self.membership == 0
            || self.transfer.0 == [0; 16]
            || extensions.len() != phase.fields().len()
            || extensions
                .iter()
                .zip(phase.fields())
                .any(|(h, n)| h.name != *n)
        {
            return Err(Error::InvalidRequest);
        }
        for h in &extensions {
            if h.value.len() > 256 {
                return Err(Error::InvalidRequest);
            }
            p::decode_binary(&h.value)?;
        }
        if length > crate::model::PAGE_BYTES as usize + 16
            || (length != 0 && phase != Phase::Finish)
        {
            return Err(Error::InvalidRequest);
        }
        let mut h = MessageHead {
            start: if phase.response() {
                StartLine::Response { status: 200 }
            } else {
                StartLine::Request {
                    method: "POST".into(),
                    target: "/racer/peer/v1/payload".into(),
                }
            },
            headers: vec![],
        };
        p::push(&mut h, "content-length", length);
        p::push(&mut h, "racer-kind", format!("payload-v1-{}", phase.name()));
        for (n, b) in [
            ("racer-payload-request", self.request.as_slice()),
            ("racer-payload-response", self.response.as_slice()),
            ("racer-payload-transfer", self.transfer.0.as_slice()),
            ("racer-payload-previous", previous.as_slice()),
        ] {
            p::push_binary(&mut h, n, b);
        }
        p::push(&mut h, "racer-payload-membership", self.membership);
        p::push(&mut h, "racer-payload-deadline", self.deadline);
        p::push(&mut h, "racer-payload-rail", self.rail.0);
        h.headers.extend(extensions);
        Ok(h)
    }
    pub fn sign(
        &self,
        signatures: &Signatures,
        to: &NodeId,
        phase: Phase,
        previous: &[u8; 32],
        length: usize,
        extensions: Vec<Header>,
    ) -> Result<SignedHead> {
        let mut h = self.head(phase, previous, length, extensions)?;
        p::push(&mut h, "racer-receiver", &to.0);
        signatures.sign(h)
    }
    pub fn verify(
        &self,
        signatures: &Signatures,
        from: &NodeId,
        signed: SignedHead,
        allowed: &[Phase],
        previous: &[u8; 32],
        length: usize,
        scope: &RequestScope,
    ) -> Result<(VerifiedHead, Phase)> {
        scope.check()?;
        if p::decode_deadline(self.deadline)?.0 <= crate::runtime::environment::now() {
            return Err(Error::DeadlineExceeded);
        }
        let kind = p::field(&signed.head, "racer-kind")?;
        let phase = *allowed
            .iter()
            .find(|phase| kind == format!("payload-v1-{}", phase.name()))
            .ok_or(Error::Unauthorized)?;
        let extensions = phase
            .fields()
            .iter()
            .map(|name| {
                Ok(Header {
                    name: (*name).into(),
                    value: signed
                        .head
                        .unique(name)?
                        .ok_or(Error::Unauthorized)?
                        .to_vec(),
                })
            })
            .collect::<Result<Vec<_>>>()?;
        p::agrees(
            &signed.head,
            &self.head(phase, previous, length, extensions)?,
            false,
        )?;
        if crate::security::signing::node_field(&signed.head, "racer-signer")? != *from {
            return Err(Error::Unauthorized);
        }
        Ok((signatures.verify_proof(signed)?, phase))
    }
    pub fn parse_accept(signed: &SignedHead) -> Result<Self> {
        fn a<const N: usize>(h: &MessageHead, n: &str) -> Result<[u8; N]> {
            p::decode_binary(p::field(h, n)?.as_bytes())?
                .try_into()
                .map_err(|_| Error::InvalidRequest)
        }
        let h = &signed.head;
        Ok(Self {
            request: a(h, "racer-payload-request")?,
            response: a(h, "racer-payload-response")?,
            transfer: TransferId(a(h, "racer-payload-transfer")?),
            membership: p::number(h, "racer-payload-membership")?,
            deadline: p::number(h, "racer-payload-deadline")?,
            rail: RailId(
                p::number(h, "racer-payload-rail")?
                    .try_into()
                    .map_err(|_| Error::InvalidRequest)?,
            ),
        })
    }
}
/// Bind all original/hop signatures using security's canonical digest, never bodies.
fn envelope_digest(auth: &ForwardedHead) -> Result<[u8; 32]> {
    use sha2::{Digest, Sha256};
    let mut hash = Sha256::new();
    hash.update(b"racer-peer-v1/payload-envelope\0");
    hash.update((auth.hops.len() as u64).to_be_bytes());
    hash.update(signed_digest(&auth.original)?);
    for h in &auth.hops {
        hash.update(signed_digest(h)?);
    }
    Ok(hash.finalize().into())
}
fn attach(head: &mut MessageHead, signed: &SignedHead) -> Result<()> {
    head.headers.push(Header {
        name: HEADER.into(),
        value: super::protocol::encode_signed(signed)?,
    });
    Ok(())
}
pub(super) fn detach(head: &mut MessageHead) -> Result<Option<SignedHead>> {
    let value = head.unique(HEADER)?.map(|v| v.to_vec());
    head.headers
        .retain(|h| !h.name.eq_ignore_ascii_case(HEADER));
    value
        .map(|v| super::protocol::decode_signed(&v))
        .transpose()
}
fn frame(signed: SignedHead) -> Result<MessageHead> {
    let response = matches!(signed.head.start, StartLine::Response { .. });
    WireCodec::encode(
        &ForwardedHead {
            original: Arc::new(signed),
            hops: vec![],
        },
        response,
        0,
    )
}
fn unframe(head: MessageHead, response: bool) -> Result<SignedHead> {
    let (auth, len) = WireCodec::decode(head, response)?;
    if len != 0 || !auth.hops.is_empty() {
        return Err(Error::InvalidRequest);
    }
    Arc::try_unwrap(auth.original).map_err(|_| Error::InvalidRequest)
}
fn extension(name: &str, value: Vec<u8>) -> Header {
    Header {
        name: name.into(),
        value,
    }
}

fn recoverable(error: Error) -> bool {
    matches!(error, Error::Unavailable | Error::Io | Error::Overloaded)
}
fn native_scope(scope: &RequestScope) -> RequestScope {
    let mut bounded = scope.clone();
    bounded.deadline.0 = bounded
        .deadline
        .0
        .min(crate::runtime::environment::now() + Duration::from_secs(5));
    bounded
}
fn native_failure(error: Error, scope: &RequestScope) -> bool {
    scope.check().is_ok() && (recoverable(error) || error == Error::DeadlineExceeded)
}
async fn fence(session: &crate::rdma::SessionLease, scope: &RequestScope) -> Result<()> {
    let cancellation = scope.cancellation.subscribe()?;
    futures::future::poll_fn(|cx| {
        cancellation.register(cx.waker());
        session.abort()?;
        scope.check()?;
        session.qp.poll_stopped(cx)
    })
    .await
}

impl Transfers {
    pub(super) async fn send_native(
        &self,
        mut connection: ConnectionLease,
        response: &SignedResponse,
        admitted: (Binding, VerifiedHead),
        membership: &crate::topology::membership::MembershipLease,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, bool)> {
        let (signatures, sessions) = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let Some(rdma) = &self.rdma else {
            return Ok((connection, false));
        };
        let (PeerResponse::Page { ciphertext, .. } | PeerResponse::Selected { ciphertext, .. }) =
            &response.response
        else {
            return Ok((connection, false));
        };
        let (mut binding, accept) = admitted;
        if binding.membership != membership.version.0 {
            return Err(Error::IncompatibleMembership);
        }
        let mut bounded_scope = scope.clone();
        bounded_scope.deadline.0 = bounded_scope
            .deadline
            .0
            .min(crate::security::protocol::decode_deadline(binding.deadline)?.0);
        let scope = &bounded_scope;
        scope.check()?;
        let peer = accept.peer.node();
        let path = crate::security::protocol::decode_nodes(
            crate::security::protocol::field(
                &response.authentication.original.head,
                "racer-response-path",
            )?
            .as_bytes(),
        )?;
        let route = crate::topology::paths::Route {
            membership: membership.clone(),
            nodes: path,
        };
        if crate::topology::rails::select(&route, &ciphertext.envelope().page)?
            != (TransportPlan::Rdma { rail: binding.rail })
            || !rdma.ready(binding.rail)
        {
            return Ok((connection, false));
        }
        let prepared = match sessions.prepare(&accept.peer, binding.rail, scope).await {
            Ok(p) => p,
            Err(e) if recoverable(e) => return Ok((connection, false)),
            Err(e) => return Err(e),
        };
        binding.response = envelope_digest(&response.authentication)?;
        let local_setup = prepared.setup().header_value();
        let offer = binding.sign(
            signatures,
            peer,
            Phase::Offer,
            &signed_digest(&accept.signed)?,
            0,
            vec![extension(SETUP_HEADER, local_setup.clone())],
        )?;
        let mut previous = signed_digest(&offer)?;
        let mut head = WireCodec::encode(&response.authentication, true, 0)?;
        attach(&mut head, &offer)?;
        connection = self.io.send_head(connection, head, scope).await?.connection;
        connection.next_round()?;
        let (conn, setup) = self.read_control(connection, false, scope).await?;
        connection = conn;
        let (setup, phase) = binding.verify(
            signatures,
            peer,
            setup,
            &[Phase::Setup, Phase::Fallback],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&setup.signed)?;
        if phase == Phase::Fallback {
            drop(prepared);
            return self
                .send_fallback(connection, response, &binding, peer, previous, scope)
                .await;
        }
        let remote = SetupParameters::from_verified(&setup, binding.rail)?;
        let session = match prepared.finish(&setup, scope).await {
            Ok(session) => session,
            Err(error) if recoverable(error) => {
                return self
                    .failed_then_fallback(connection, response, &binding, peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        let ready = binding.sign(
            signatures,
            peer,
            Phase::Ready,
            &previous,
            0,
            vec![
                extension(SETUP_HEADER, local_setup),
                extension(SETUP_BINDING_HEADER, remote.binding_header_value()),
            ],
        )?;
        previous = signed_digest(&ready)?;
        connection = self.write_control(connection, ready, scope).await?;
        connection.next_round()?;
        let (conn, grant) = self.read_control(connection, false, scope).await?;
        connection = conn;
        let (grant, phase) = binding.verify(
            signatures,
            peer,
            grant,
            &[Phase::Grant, Phase::Fallback],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&grant.signed)?;
        if phase == Phase::Fallback {
            fence(&session, scope).await?;
            return self
                .send_fallback(connection, response, &binding, peer, previous, scope)
                .await;
        }
        let descriptor =
            AuthenticatedDescriptor::from_verified(&grant, &session, binding.transfer)?;
        let native_deadline = native_scope(scope);
        let complete = match rdma
            .send_to(&session, ciphertext.clone(), descriptor, &native_deadline)
            .await
        {
            Ok(complete) => complete,
            Err(error) if native_failure(error, scope) => {
                fence(&session, scope).await?;
                return self
                    .failed_then_fallback(connection, response, &binding, peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        let complete = binding.sign(
            signatures,
            peer,
            Phase::Complete,
            &previous,
            0,
            vec![extension(COMPLETION_HEADER, complete.header_value())],
        )?;
        previous = signed_digest(&complete)?;
        connection = self.write_control(connection, complete, scope).await?;
        connection.next_round()?;
        let (conn, done) = self.read_control(connection, false, scope).await?;
        connection = conn;
        let (done, phase) = binding.verify(
            signatures,
            peer,
            done,
            &[Phase::Done, Phase::Fallback],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&done.signed)?;
        if phase == Phase::Fallback {
            fence(&session, scope).await?;
            return self
                .send_fallback(connection, response, &binding, peer, previous, scope)
                .await;
        }
        let finish = binding.sign(signatures, peer, Phase::Finish, &previous, 0, vec![])?;
        connection = self.write_control(connection, finish, scope).await?;
        #[cfg(test)]
        self.native_completed.set(self.native_completed.get() + 1);
        Ok((connection, true))
    }
    async fn failed_then_fallback(
        &self,
        mut connection: ConnectionLease,
        response: &SignedResponse,
        binding: &Binding,
        peer: &NodeId,
        previous: [u8; 32],
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, bool)> {
        let (signatures, _) = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let failed = binding.sign(signatures, peer, Phase::Failed, &previous, 0, vec![])?;
        let previous = signed_digest(&failed)?;
        connection = self.write_control(connection, failed, scope).await?;
        connection.next_round()?;
        let (connection, fallback) = self.read_control(connection, false, scope).await?;
        let (fallback, _) = binding.verify(
            signatures,
            peer,
            fallback,
            &[Phase::Fallback],
            &previous,
            0,
            scope,
        )?;
        self.send_fallback(
            connection,
            response,
            binding,
            peer,
            signed_digest(&fallback.signed)?,
            scope,
        )
        .await
    }
    async fn send_fallback(
        &self,
        connection: ConnectionLease,
        response: &SignedResponse,
        binding: &Binding,
        peer: &NodeId,
        previous: [u8; 32],
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, bool)> {
        #[cfg(test)]
        self.native_fallbacks.set(self.native_fallbacks.get() + 1);
        scope.check()?;
        let (signatures, _) = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let (PeerResponse::Page { ciphertext, .. } | PeerResponse::Selected { ciphertext, .. }) =
            &response.response
        else {
            return Err(Error::InvalidRequest);
        };
        let finish = binding.sign(
            signatures,
            peer,
            Phase::Finish,
            &previous,
            ciphertext.bytes().len(),
            vec![],
        )?;
        let mut head = WireCodec::encode(&response.authentication, true, ciphertext.bytes().len())?;
        attach(&mut head, &finish)?;
        let connection = self.io.send_head(connection, head, scope).await?.connection;
        let written = self
            .io
            .write_body(connection, ciphertext.clone(), scope)
            .await?;
        Ok((written.lease, true))
    }
    pub(super) fn accept_native(
        &self,
        request: &SignedRequest,
        plan: TransportPlan,
        scope: &RequestScope,
    ) -> Result<Option<(Binding, SignedHead, NodeId)>> {
        let TransportPlan::Rdma { rail } = plan else {
            return Ok(None);
        };
        let Some((signatures, sessions)) = &self.native else {
            return Ok(None);
        };
        if !sessions.ready(rail) || !self.rdma.as_ref().is_some_and(|rdma| rdma.ready(rail)) {
            return Ok(None);
        }
        let peer = crate::security::signing::receiver(
            &request
                .authentication
                .hops
                .last()
                .unwrap_or(&request.authentication.original)
                .head,
        )?;
        let binding = Binding::request(
            &request.authentication,
            request.request.route.membership.0,
            scope,
            rail,
        )?;
        let accept = binding.sign(signatures, &peer, Phase::Accept, &[0; 32], 0, vec![])?;
        Ok(Some((binding, accept, peer)))
    }
    pub(super) fn admit_native(
        &self,
        request: &SignedRequest,
        control: SignedHead,
        scope: &RequestScope,
    ) -> Result<Option<(Binding, VerifiedHead)>> {
        let Some((signatures, _)) = &self.native else {
            return Ok(None);
        };
        let binding = Binding::parse_accept(&control)?;
        if binding.request != envelope_digest(&request.authentication)?
            || binding.response != [0; 32]
            || binding.membership != request.request.route.membership.0
            || binding.deadline
                > crate::security::protocol::encode_deadline(request.request.route.deadline)?
        {
            return Err(Error::Unauthorized);
        }
        let peer = crate::security::signing::node_field(
            &request
                .authentication
                .hops
                .last()
                .unwrap_or(&request.authentication.original)
                .head,
            "racer-signer",
        )?;
        let (verified, _) = binding.verify(
            signatures,
            &peer,
            control,
            &[Phase::Accept],
            &[0; 32],
            0,
            scope,
        )?;
        Ok(Some((binding, verified)))
    }
    async fn write_control(
        &self,
        connection: ConnectionLease,
        signed: SignedHead,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        Ok(self
            .io
            .send_head(connection, frame(signed)?, scope)
            .await?
            .connection)
    }
    async fn read_control(
        &self,
        connection: ConnectionLease,
        response: bool,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, SignedHead)> {
        let received = self.io.receive_head(connection, scope).await?;
        Ok((received.connection, unframe(received.value, response)?))
    }
    async fn read_ciphertext(
        &self,
        mut connection: ConnectionLease,
        length: usize,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, WireBuffer)> {
        let (admission, _) = &self.wire;
        let mut buffer = WireBuffer::new(admission, length)?;
        let mut offset = 0;
        while offset < length {
            let read = self
                .io
                .read_body_range(connection, buffer, offset..length, scope)
                .await?;
            if read.bytes == 0 || read.bytes > length - offset {
                return Err(Error::Io);
            }
            offset += read.bytes;
            connection = read.lease;
            buffer = read.buffer;
        }
        Ok((connection, buffer))
    }
    /// A completed control round always pairs one request and one response before
    /// resetting HTTP framing. No pipelining or detached state map is required.
    pub(super) async fn receive_native(
        &self,
        mut connection: ConnectionLease,
        authentication: ForwardedHead,
        mut binding: Binding,
        accept: SignedHead,
        peer: NodeId,
        offer: SignedHead,
        scope: &RequestScope,
    ) -> Result<SignedResponse> {
        let (signatures, sessions) = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let rdma = self.rdma.as_ref().ok_or(Error::Unavailable)?;
        let (admission, _) = &self.wire;
        binding.response = envelope_digest(&authentication)?;
        let (offer, _) = binding.verify(
            signatures,
            &peer,
            offer,
            &[Phase::Offer],
            &signed_digest(&accept)?,
            0,
            scope,
        )?;
        let (metadata, envelope) = super::protocol::page_descriptor(&authentication.original.head)?;
        let remote = SetupParameters::from_verified(&offer, binding.rail)?;
        let mut previous = signed_digest(&offer.signed)?;
        connection.next_round()?;
        let prepared = match sessions
            .prepare_admitted(
                &offer.peer,
                binding.rail,
                connection.peer_admission.clone(),
                scope,
            )
            .await
        {
            Ok(prepared) => prepared,
            Err(error) if recoverable(error) => {
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        let local_setup = prepared.setup().header_value();
        let setup = binding.sign(
            signatures,
            &peer,
            Phase::Setup,
            &previous,
            0,
            vec![
                extension(SETUP_HEADER, local_setup),
                extension(SETUP_BINDING_HEADER, remote.binding_header_value()),
            ],
        )?;
        previous = signed_digest(&setup)?;
        connection = self.write_control(connection, setup, scope).await?;
        let (conn, ready) = self.read_control(connection, true, scope).await?;
        connection = conn;
        let (ready, phase) = binding.verify(
            signatures,
            &peer,
            ready,
            &[Phase::Ready, Phase::Failed],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&ready.signed)?;
        connection.next_round()?;
        if phase == Phase::Failed {
            drop(prepared);
            return self
                .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                .await;
        }
        if SetupParameters::from_verified(&ready, binding.rail)?.encoded != remote.encoded {
            return Err(Error::Unauthorized);
        }
        let session = match prepared.finish(&ready, scope).await {
            Ok(session) => session,
            Err(error) if recoverable(error) => {
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        let native_deadline = native_scope(scope);
        if let Err(error) = session.wait_ready(&native_deadline).await {
            fence(&session, scope).await?;
            if native_failure(error, scope) {
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            return Err(error);
        }
        let grant = match rdma
            .prepare_receive(&session, &envelope, binding.transfer, scope)
            .await
        {
            Ok(grant) => grant,
            Err(error) if recoverable(error) => {
                fence(&session, scope).await?;
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        if let Err(error) = grant.wait_bound(&native_deadline).await {
            fence(&session, scope).await?;
            drop(grant);
            if native_failure(error, scope) {
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            return Err(error);
        }
        let request = binding.sign(
            signatures,
            &peer,
            Phase::Grant,
            &previous,
            0,
            vec![extension(DESCRIPTOR_HEADER, grant.header_value()?)],
        )?;
        previous = signed_digest(&request)?;
        connection = self.write_control(connection, request, scope).await?;
        let (conn, completed) = self.read_control(connection, true, scope).await?;
        connection = conn;
        let (completed, phase) = binding.verify(
            signatures,
            &peer,
            completed,
            &[Phase::Complete, Phase::Failed],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&completed.signed)?;
        connection.next_round()?;
        if phase == Phase::Failed {
            fence(&session, scope).await?;
            drop(grant);
            return self
                .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                .await;
        }
        let native_deadline = native_scope(scope);
        let page = match rdma
            .finish_receive(
                &session,
                grant,
                &completed,
                envelope,
                admission,
                &native_deadline,
            )
            .await
        {
            Ok(page) => page,
            Err(error) if native_failure(error, scope) => {
                fence(&session, scope).await?;
                return self
                    .receive_fallback(connection, authentication, &binding, &peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        #[cfg(test)]
        self.native_completions
            .set(self.native_completions.get() + 1);
        let done = binding.sign(signatures, &peer, Phase::Done, &previous, 0, vec![])?;
        previous = signed_digest(&done)?;
        connection = self.write_control(connection, done, scope).await?;
        let (mut conn, finish) = self.read_control(connection, true, scope).await?;
        binding.verify(
            signatures,
            &peer,
            finish,
            &[Phase::Finish],
            &previous,
            0,
            scope,
        )?;
        // The original envelope is verified by the requester/relay's outstanding
        // binding after this transport returns. No plaintext is published here.
        let response =
            if crate::security::protocol::field(&authentication.original.head, "racer-outcome")?
                == "selected"
            {
                PeerResponse::Selected {
                    metadata,
                    ciphertext: page,
                    grant: super::protocol::grant(&authentication.original.head)?,
                }
            } else {
                PeerResponse::Page {
                    metadata,
                    ciphertext: page,
                }
            };
        let original = &authentication.original.head;
        let request_digest = crate::security::protocol::decode_binary(
            crate::security::protocol::field(original, "racer-request-binding")?.as_bytes(),
        )?
        .try_into()
        .map_err(|_| Error::InvalidRequest)?;
        let path = crate::security::protocol::decode_nodes(
            crate::security::protocol::field(original, "racer-response-path")?.as_bytes(),
        )?;
        crate::security::protocol::agrees(
            original,
            &crate::security::protocol::response_head(&response, &request_digest, &path)?,
            false,
        )?;
        conn.finish_exchange()?;
        Ok(SignedResponse {
            authentication,
            response,
        })
    }
    async fn receive_fallback(
        &self,
        connection: ConnectionLease,
        authentication: ForwardedHead,
        binding: &Binding,
        peer: &NodeId,
        previous: [u8; 32],
        scope: &RequestScope,
    ) -> Result<SignedResponse> {
        scope.check()?;
        let (signatures, _) = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let fallback = binding.sign(signatures, peer, Phase::Fallback, &previous, 0, vec![])?;
        let previous = signed_digest(&fallback)?;
        let connection = self.write_control(connection, fallback, scope).await?;
        let mut received = self.io.receive_head(connection, scope).await?;
        let control = detach(&mut received.value)?.ok_or(Error::Unauthorized)?;
        let (returned, length) = WireCodec::decode(received.value, true)?;
        if envelope_digest(&returned)? != binding.response {
            return Err(Error::Unauthorized);
        }
        binding.verify(
            signatures,
            peer,
            control,
            &[Phase::Finish],
            &previous,
            length,
            scope,
        )?;
        let (mut connection, buffer) = self
            .read_ciphertext(received.connection, length, scope)
            .await?;
        let (bytes, _reservation) = buffer.into_parts();
        let (_, codec) = &self.wire;
        let response = codec.response(authentication, bytes, scope)?;
        connection.finish_exchange()?;
        Ok(response)
    }
}

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
        connection: Box<crate::http::connection::ConnectionLease>,
        length: usize,
    },
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
        Rc<crate::rdma::Sessions>,
    )>,
}
impl Transfers {
    /// Authentication and charged decoding are mandatory, even for HTTP-only peers.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{http::connection::{HttpIo, HttpPool}, peer::transport::Transfers};
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
    pub fn with_native(mut self, sessions: Rc<crate::rdma::Sessions>) -> Self {
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
    /// The signed envelope and HTTP ciphertext share one exclusive pooled socket.
    /// A failed/abandoned exchange is never marked reusable.
    pub fn exchange<'a>(
        &'a self,
        endpoint: crate::http::connection::Endpoint,
        request: SignedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        self.exchange_planned(endpoint, request, TransportPlan::Http, scope)
    }
    pub fn exchange_planned<'a>(
        &'a self,
        endpoint: crate::http::connection::Endpoint,
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
        endpoint: crate::http::connection::Endpoint,
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
                attach(&mut head, accept)?;
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
            let control = detach(&mut received.value)?;
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
