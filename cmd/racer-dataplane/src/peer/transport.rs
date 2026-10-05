//! Transport-neutral ciphertext lifecycle, selecting HTTP or authenticated RDMA.

use super::protocol::PeerResponse;
use super::protocol::SecurityCodec;
use super::protocol::SignedRequest;
use super::protocol::SignedResponse;
use super::protocol::decode_envelope;
use super::protocol::encode_envelope;
use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::http::ConnectionLease;
use crate::http::HttpIo;
use crate::http::HttpPool;
use crate::model::TransferId;
use crate::peer::forwarding::ForwardedHead;
use crate::peer::protocol as p;
use crate::peer::protocol::Signatures;
use crate::peer::protocol::SignedHead;
use crate::peer::protocol::VerifiedHead;
use crate::peer::protocol::signed_digest;
use crate::rdma::AuthenticatedDescriptor;
use crate::rdma::COMPLETION_HEADER;
use crate::rdma::DESCRIPTOR_HEADER;
use crate::rdma::SETUP_BINDING_HEADER;
use crate::rdma::SETUP_HEADER;
use crate::rdma::Sessions;
use crate::rdma::SetupParameters;
use crate::rdma::TransportPlan;
use crate::runtime::RequestScope;
use crate::telemetry::BodyProgress;
use crate::telemetry::Detail;
use crate::telemetry::Failure;
use crate::telemetry::Stage;
use crate::telemetry::timestamp;
use http1::Header;
use http1::MessageHead;
use http1::StartLine;
#[cfg(test)]
use racer_control_wire::CacheId;
#[cfg(test)]
use racer_control_wire::MembershipVersion;
use racer_control_wire::NodeId;
use racer_control_wire::RailId;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Poll;
use std::time::Duration;
use uring_runtime::reactor::IoBuffer;

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
        uring_runtime::environment::fill_random(&mut transfer).map_err(|_| Error::Unavailable)?;
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
    /// Verify the peer's signed phase against this transfer and its prior digest.
    #[allow(clippy::too_many_arguments)] // Protocol evidence is checked together at this boundary.
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
        if p::decode_deadline(self.deadline)?.0 <= uring_runtime::environment::now() {
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
                Ok(Header::new(
                    *name,
                    signed.head.unique(name)?.ok_or(Error::Unauthorized)?,
                ))
            })
            .collect::<Result<Vec<_>>>()?;
        p::agrees(
            &signed.head,
            &self.head(phase, previous, length, extensions)?,
            false,
        )?;
        if crate::peer::protocol::node_field(&signed.head, "racer-signer")? != *from {
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
    use sha2::Digest;
    use sha2::Sha256;
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
    encode_envelope(
        &ForwardedHead {
            original: Arc::new(signed),
            hops: vec![],
        },
        response,
        0,
    )
}
fn unframe(head: MessageHead, response: bool) -> Result<SignedHead> {
    let (auth, len) = decode_envelope(head, response)?;
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
    matches!(
        error,
        Error::Unavailable | Error::Io | Error::Os(_) | Error::Overloaded
    )
}
fn native_scope(scope: &RequestScope) -> RequestScope {
    let mut bounded = scope.clone();
    bounded.deadline.0 = bounded
        .deadline
        .0
        .min(uring_runtime::environment::now() + Duration::from_secs(5));
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
        session.qp.poll_stopped(cx).map_err(Into::into)
    })
    .await
}

impl Transfers {
    /// Bind the upgrade to the actual authenticated reverse hop, not a planned
    /// route or a peer's claimed Site. Run before either end prepares a native QP.
    fn response_plan(
        &self,
        connection: &ConnectionLease,
        authentication: &ForwardedHead,
        membership: &std::sync::Arc<crate::topology::Membership>,
        page: &crate::model::PageId,
        peer: &NodeId,
        sending: bool,
    ) -> Result<TransportPlan> {
        if connection.state().session.as_ref().map(|s| s.peer()) != Some(peer) {
            return Err(Error::Unauthorized);
        }
        let path = crate::peer::protocol::decode_nodes(
            crate::peer::protocol::field(&authentication.original.head, "racer-response-path")?
                .as_bytes(),
        )?;
        let receiver_index = path
            .len()
            .checked_sub(authentication.hops.len() + 2)
            .ok_or(Error::Unauthorized)?;
        let local = self.signatures.node();
        let (sender, receiver) = if sending {
            (local, peer)
        } else {
            (peer, local)
        };
        if &path[receiver_index] != receiver || &path[receiver_index + 1] != sender {
            return Err(Error::Unauthorized);
        }
        for (i, signed) in std::iter::once(authentication.original.as_ref())
            .chain(authentication.hops.iter())
            .enumerate()
        {
            let index = path
                .len()
                .checked_sub(i + 1)
                .filter(|i| *i > 0)
                .ok_or(Error::Unauthorized)?;
            let verified = self.signatures.verify_historical(signed)?;
            if verified.node() != &path[index]
                || crate::peer::protocol::receiver(&signed.head)? != path[index - 1]
            {
                return Err(Error::Unauthorized);
            }
        }
        crate::rdma::select_hop(
            &crate::topology::Route {
                membership: membership.clone(),
                nodes: path,
            },
            page,
            local,
            peer,
        )
    }

    pub(super) async fn send_native(
        &self,
        mut connection: ConnectionLease,
        response: &SignedResponse,
        admitted: (Binding, VerifiedHead),
        membership: &std::sync::Arc<crate::topology::Membership>,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, bool)> {
        let sessions = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let signatures = &self.signatures;
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
            .min(crate::peer::protocol::decode_deadline(binding.deadline)?.0);
        let scope = &bounded_scope;
        scope.check()?;
        let peer = accept.peer.node();
        if self.response_plan(
            &connection,
            &response.authentication,
            membership,
            &ciphertext.envelope().page,
            peer,
            true,
        )? != (TransportPlan::Rdma { rail: binding.rail })
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
        let mut head = encode_envelope(&response.authentication, true, 0)?;
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
        self.send_native_page(
            connection, response, &binding, peer, &session, grant, previous, scope,
        )
        .await
    }

    /// The established session stays owned by the caller through completion or fencing.
    #[allow(clippy::too_many_arguments)] // Keep session and transfer ownership visible across awaits.
    async fn send_native_page(
        &self,
        mut connection: ConnectionLease,
        response: &SignedResponse,
        binding: &Binding,
        peer: &NodeId,
        session: &crate::rdma::SessionLease,
        grant: VerifiedHead,
        mut previous: [u8; 32],
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, bool)> {
        let signatures = &self.signatures;
        let rdma = self.rdma.as_ref().ok_or(Error::Unavailable)?;
        let (PeerResponse::Page { ciphertext, .. } | PeerResponse::Selected { ciphertext, .. }) =
            &response.response
        else {
            return Err(Error::InvalidRequest);
        };
        let descriptor = AuthenticatedDescriptor::from_verified(&grant, session, binding.transfer)?;
        let native_deadline = native_scope(scope);
        let complete = match rdma
            .send_to(session, ciphertext.clone(), descriptor, &native_deadline)
            .await
        {
            Ok(complete) => complete,
            Err(error) if native_failure(error, scope) => {
                fence(session, scope).await?;
                return self
                    .failed_then_fallback(connection, response, binding, peer, previous, scope)
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
            fence(session, scope).await?;
            return self
                .send_fallback(connection, response, binding, peer, previous, scope)
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
        let signatures = &self.signatures;
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
        let signatures = &self.signatures;
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
        let mut head = encode_envelope(&response.authentication, true, ciphertext.bytes().len())?;
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
        let Some(sessions) = &self.native else {
            return Ok(None);
        };
        let signatures = &self.signatures;
        if !sessions.ready(rail) || !self.rdma.as_ref().is_some_and(|rdma| rdma.ready(rail)) {
            return Ok(None);
        }
        let peer = crate::peer::protocol::receiver(
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
        let Some(_) = &self.native else {
            return Ok(None);
        };
        let signatures = &self.signatures;
        let binding = Binding::parse_accept(&control)?;
        if binding.request != envelope_digest(&request.authentication)?
            || binding.response != [0; 32]
            || binding.membership != request.request.route.membership.0
            || binding.deadline
                > crate::peer::protocol::encode_deadline(request.request.route.deadline)?
        {
            return Err(Error::Unauthorized);
        }
        let peer = crate::peer::protocol::node_field(
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
        connection: ConnectionLease,
        length: usize,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, WireBuffer)> {
        let (admission, _) = &self.wire;
        let buffer = WireBuffer::new(admission, length)?;
        let read = self.io.read_body_exact(connection, buffer, scope).await?;
        Ok((read.lease, read.buffer))
    }
    /// A completed control round always pairs one request and one response before
    /// resetting HTTP framing. No pipelining or detached state map is required.
    /// Receive a native transfer with authenticated metadata and fallback framing.
    #[allow(clippy::too_many_arguments)] // Preserve explicit authenticated transfer inputs.
    pub(super) async fn receive_native(
        &self,
        mut connection: ConnectionLease,
        authentication: ForwardedHead,
        mut binding: Binding,
        accept: SignedHead,
        peer: NodeId,
        offer: SignedHead,
        membership: &std::sync::Arc<crate::topology::Membership>,
        scope: &RequestScope,
    ) -> Result<SignedResponse> {
        let sessions = self.native.as_ref().ok_or(Error::InvalidConfiguration)?;
        let signatures = &self.signatures;
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
        if binding.membership != membership.version.0 {
            return Err(Error::IncompatibleMembership);
        }
        if self.response_plan(
            &connection,
            &authentication,
            membership,
            &envelope.page,
            offer.peer.node(),
            false,
        )? != (TransportPlan::Rdma { rail: binding.rail })
        {
            return Err(Error::Unauthorized);
        }
        let remote = SetupParameters::from_verified(&offer, binding.rail)?;
        let mut previous = signed_digest(&offer.signed)?;
        connection.next_round()?;
        let prepared = match sessions
            .prepare_admitted(
                &offer.peer,
                binding.rail,
                connection.state().peer_admission.clone(),
                connection.state().receive_permit.clone(),
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
        self.receive_native_page(
            connection,
            authentication,
            &binding,
            &peer,
            &session,
            metadata,
            envelope,
            previous,
            scope,
        )
        .await
    }

    /// Grant, completion, and final acknowledgment share the established session owner.
    /// Receive and fence one page using the caller's established session.
    #[allow(clippy::too_many_arguments)] // Keep session and transfer ownership visible across awaits.
    async fn receive_native_page(
        &self,
        mut connection: ConnectionLease,
        authentication: ForwardedHead,
        binding: &Binding,
        peer: &NodeId,
        session: &crate::rdma::SessionLease,
        metadata: crate::model::ObjectMetadata,
        envelope: crate::model::PageEnvelope,
        mut previous: [u8; 32],
        scope: &RequestScope,
    ) -> Result<SignedResponse> {
        let signatures = &self.signatures;
        let rdma = self.rdma.as_ref().ok_or(Error::Unavailable)?;
        let (admission, _) = &self.wire;
        let native_deadline = native_scope(scope);
        if let Err(error) = session.wait_ready(&native_deadline).await {
            fence(session, scope).await?;
            if native_failure(error, scope) {
                return self
                    .receive_fallback(connection, authentication, binding, peer, previous, scope)
                    .await;
            }
            return Err(error);
        }
        let grant = match rdma
            .prepare_receive(session, &envelope, binding.transfer, scope)
            .await
        {
            Ok(grant) => grant,
            Err(error) if recoverable(error) => {
                fence(session, scope).await?;
                return self
                    .receive_fallback(connection, authentication, binding, peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        if let Err(error) = grant.wait_bound(&native_deadline).await {
            fence(session, scope).await?;
            drop(grant);
            if native_failure(error, scope) {
                return self
                    .receive_fallback(connection, authentication, binding, peer, previous, scope)
                    .await;
            }
            return Err(error);
        }
        let request = binding.sign(
            signatures,
            peer,
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
            peer,
            completed,
            &[Phase::Complete, Phase::Failed],
            &previous,
            0,
            scope,
        )?;
        previous = signed_digest(&completed.signed)?;
        connection.next_round()?;
        if phase == Phase::Failed {
            fence(session, scope).await?;
            drop(grant);
            return self
                .receive_fallback(connection, authentication, binding, peer, previous, scope)
                .await;
        }
        let native_deadline = native_scope(scope);
        let page = match rdma
            .finish_receive(
                session,
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
                fence(session, scope).await?;
                return self
                    .receive_fallback(connection, authentication, binding, peer, previous, scope)
                    .await;
            }
            Err(error) => return Err(error),
        };
        #[cfg(test)]
        self.native_completions
            .set(self.native_completions.get() + 1);
        let done = binding.sign(signatures, peer, Phase::Done, &previous, 0, vec![])?;
        previous = signed_digest(&done)?;
        connection = self.write_control(connection, done, scope).await?;
        let (mut conn, finish) = self.read_control(connection, true, scope).await?;
        binding.verify(
            signatures,
            peer,
            finish,
            &[Phase::Finish],
            &previous,
            0,
            scope,
        )?;
        // The original envelope is verified by the requester/relay's outstanding
        // binding after this transport returns. No plaintext is published here.
        let response =
            if crate::peer::protocol::field(&authentication.original.head, "racer-outcome")?
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
        let request_digest = crate::peer::protocol::decode_binary(
            crate::peer::protocol::field(original, "racer-request-binding")?.as_bytes(),
        )?
        .try_into()
        .map_err(|_| Error::InvalidRequest)?;
        let path = crate::peer::protocol::decode_nodes(
            crate::peer::protocol::field(original, "racer-response-path")?.as_bytes(),
        )?;
        crate::peer::protocol::agrees(
            original,
            &crate::peer::protocol::response_head(&response, &request_digest, &path)?,
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
        let signatures = &self.signatures;
        let fallback = binding.sign(signatures, peer, Phase::Fallback, &previous, 0, vec![])?;
        let previous = signed_digest(&fallback)?;
        let connection = self.write_control(connection, fallback, scope).await?;
        let mut received = self.io.receive_head(connection, scope).await?;
        let control = detach(&mut received.value)?.ok_or(Error::Unauthorized)?;
        let (returned, length) = decode_envelope(received.value, true)?;
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
/// Construction normalizes capacity before pointer extraction; Vec backing then
/// stays fixed while ownership moves through reactor completion closures.
pub(crate) struct WireBuffer {
    bytes: Vec<u8>,
    _reservation: flow_control::Charge<AdmissionPolicy>,
}
impl WireBuffer {
    pub(crate) fn new(
        admission: &flow_control::Quotas<AdmissionPolicy>,
        length: usize,
    ) -> Result<Self> {
        if length > crate::model::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        let reservation = admission.reserve(None, ResourceClass::Ciphertext, length)?;
        Ok(Self {
            bytes: reservation.buffer(length)?.into_boxed_slice().into_vec(),
            _reservation: reservation,
        })
    }
    pub(crate) fn reserved(
        reservation: flow_control::Charge<AdmissionPolicy>,
        length: usize,
    ) -> Result<Self> {
        reservation.validate(ResourceClass::Ciphertext, length)?;
        if length > crate::model::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        Ok(Self {
            bytes: reservation.buffer(length)?.into_boxed_slice().into_vec(),
            _reservation: reservation,
        })
    }
    pub(crate) fn into_parts(self) -> (Vec<u8>, flow_control::Charge<AdmissionPolicy>) {
        (self.bytes, self._reservation)
    }
}
// SAFETY: private fixed Vec retains its reservation and is not aliased.
unsafe impl IoBuffer for WireBuffer {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.bytes)
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.bytes)
    }
}

type ReclaimCiphertext = dyn Fn(&racer_control_wire::CacheId, usize);

/// Generic Io has lost errno and may mean local ENOBUFS/ENOMEM. It is not link
/// evidence. Only an observed orderly EOF is classified here; connect preserves
/// its errno separately in HttpPool.
fn observe_read(
    result: Result<usize>,
    permit: &Option<std::sync::Arc<super::Permit>>,
    failure: &Rc<std::cell::Cell<bool>>,
    scope: &RequestScope,
) -> Result<usize> {
    if matches!(result, Ok(0)) && scope.check().is_ok() {
        failure.set(true);
        if let Some(permit) = permit {
            permit.observe(super::Outcome::PeerFailure);
        }
    }
    result
}

/// Internal transport result: native delivery stays materialized; HTTP relay
/// delivery owns an unfinished connection and its exact opaque body framing.
#[allow(clippy::large_enum_variant)] // Keep completed delivery inline without another allocation.
pub enum RelayResponse {
    Complete(SignedResponse),

    Http {
        authentication: crate::peer::forwarding::ForwardedHead,

        connection: Box<crate::http::ConnectionLease>,

        length: usize,
    },
}

pub struct Transfers {
    receive_gate: Option<Arc<super::receive::Gate>>,
    reclaim: Option<Rc<ReclaimCiphertext>>,
    signatures: Rc<crate::peer::protocol::Signatures>,
    #[cfg(test)]
    pub(super) native_completions: std::cell::Cell<usize>,
    #[cfg(test)]
    pub(super) native_completed: std::cell::Cell<usize>,
    #[cfg(test)]
    pub(super) native_fallbacks: std::cell::Cell<usize>,
    pub(super) http: Rc<HttpPool>,
    pub(super) io: Rc<HttpIo>,
    pub(super) rdma: Option<Rc<Sessions>>,
    pub(super) wire: (Rc<flow_control::Quotas<AdmissionPolicy>>, Rc<SecurityCodec>),
    pub(super) native: Option<Rc<crate::rdma::Sessions>>,
}
impl Transfers {
    /// Authentication and charged decoding are mandatory, even for HTTP-only peers.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{http::{HttpIo, HttpPool}, peer::transport::Transfers};
    /// use std::rc::Rc;
    /// fn unsigned(pool: Rc<HttpPool>, io: Rc<HttpIo>) {
    ///     let _ = Transfers::new(pool, io, None);
    /// }
    /// ```
    pub fn new(
        http: Rc<HttpPool>,
        io: Rc<HttpIo>,
        rdma: Option<Rc<Sessions>>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        codec: Rc<SecurityCodec>,
        signatures: Rc<crate::peer::protocol::Signatures>,
    ) -> Self {
        Self {
            reclaim: None,
            receive_gate: None,
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
        self.native = Some(sessions);
        self
    }
    pub(crate) fn with_receive_gate(mut self, gate: Arc<super::receive::Gate>) -> Self {
        self.receive_gate = Some(gate);
        self
    }
    pub(crate) async fn admit_receive(
        &self,
        request: &super::protocol::PeerRequest,
        scope: &RequestScope,
    ) -> Result<Option<Arc<super::receive::Permit>>> {
        if matches!(
            request.operation,
            super::protocol::Operation::Metadata { .. }
        ) {
            return Ok(None);
        }
        match &self.receive_gate {
            Some(gate) => gate.acquire(&self.wire.0, scope).await,
            None => Ok(None),
        }
    }
    pub(crate) fn with_reclamation(
        mut self,
        reclaim: impl Fn(&racer_control_wire::CacheId, usize) + 'static,
    ) -> Self {
        self.reclaim = Some(Rc::new(reclaim));
        self
    }
    /// The signed envelope and HTTP ciphertext share one exclusive pooled socket.
    /// A failed/abandoned exchange is never marked reusable.
    /// Exchange a signed request while recording the caller's transfer timing.
    #[allow(clippy::too_many_arguments)] // Keep transport, security, and timing inputs explicit.
    pub(super) fn exchange_timed<'a>(
        &'a self,
        endpoint: crate::http::Endpoint,
        request: SignedRequest,
        plan: TransportPlan,
        membership: Option<std::sync::Arc<crate::topology::Membership>>,
        relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        peer_admission: Option<std::sync::Arc<super::Permit>>,
        receive_permit: Option<Arc<super::receive::Permit>>,
        failure: Rc<std::cell::Cell<bool>>,
        mut timing: Option<&'a mut super::PageTiming<'_>>,
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
            let mut head = encode_envelope(&request.authentication, false, 0)?;
            let signatures = self.signatures.clone();
            let peer = crate::peer::protocol::receiver(
                &request
                    .authentication
                    .hops
                    .last()
                    .unwrap_or(&request.authentication.original)
                    .head,
            )?;
            let observer = admission.policy().observer();
            if let Some(timing) = timing.as_deref_mut() {
                timing.enable(&request.request, plan, relay.is_some());
            }
            let mut connection = observer.result(
                Stage::PeerCheckout,
                scope,
                self.http
                    .checkout_with_state(
                        &endpoint,
                        crate::http::State {
                            relay_reservation: relay.clone(),
                            peer_admission: peer_admission.clone(),
                            receive_permit,
                            connect_failure: Some(failure.clone()),
                            ..Default::default()
                        },
                        scope,
                    )
                    .await,
            )?;
            connection.state_mut().peer_admission = peer_admission.clone();
            if let Some(timing) = timing.as_deref_mut() {
                timing.end(0);
            }
            connection.state_mut().relay_reservation = relay.clone();
            // Connection and handshake get separate bounded idle allowances.
            // No header byte can renew the request/head allowance.
            scope.candidate_progress()?;
            let connection = {
                if let Some(timing) = timing.as_deref_mut() {
                    timing.begin();
                }
                let _permit = connection
                    .state()
                    .session
                    .is_none()
                    .then(|| admission.reserve(None, ResourceClass::ControlProgress, 1))
                    .transpose()?;
                observer.result(
                    Stage::PeerHandshake,
                    scope,
                    crate::peer::protocol::connect(&self.io, connection, signatures, &peer, scope)
                        .await,
                )?
            };
            if let Some(timing) = timing.as_deref_mut() {
                timing.end(1);
            }
            // Native response verification requires an authenticated membership lease.
            let native = if membership.is_some() {
                self.accept_native(&request, plan, scope)?
            } else {
                None
            };
            scope.candidate_progress()?;
            if let Some((_, accept, _)) = &native {
                attach(&mut head, accept)?;
            }
            if let Some(timing) = timing.as_deref_mut() {
                timing.begin();
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
            if let Some(timing) = timing.as_deref_mut() {
                timing.end(2);
            }
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
            let (authentication, length) = decode_envelope(received.value, true)?;
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
                        membership.as_ref().ok_or(Error::IncompatibleMembership)?,
                        scope,
                    )
                    .await
                    .map(RelayResponse::Complete);
            }
            if relay.is_some() {
                received.connection.state_mut().relay_context = relay_context;
                return Ok(RelayResponse::Http {
                    authentication,
                    connection: Box::new(received.connection),
                    length,
                });
            }
            if let Some(timing) = timing.as_deref_mut() {
                timing.begin();
            }
            let (mut connection, body, staging_reservation) = self
                .receive_http_body(
                    received.connection,
                    length,
                    &request,
                    &peer,
                    &peer_admission,
                    &failure,
                    scope,
                )
                .await?;
            scope.check()?;
            if let Some(timing) = timing.as_deref_mut() {
                timing.end(3);
            }
            let response = observer.result(
                Stage::PeerDecode,
                scope,
                codec.response_reserved(authentication, body, staging_reservation, scope),
            )?;
            validate_body_length(&response.response, length)?;
            connection.finish_exchange()?;
            Ok(RelayResponse::Complete(response))
        })
    }

    /// Receive an authenticated HTTP body with its admission and reclaim policy.
    #[allow(clippy::too_many_arguments)] // Preserve connection and resource ownership at the boundary.
    async fn receive_http_body(
        &self,
        mut connection: ConnectionLease,
        length: usize,
        request: &SignedRequest,
        peer: &NodeId,
        peer_admission: &Option<std::sync::Arc<super::Permit>>,
        failure: &Rc<std::cell::Cell<bool>>,
        scope: &RequestScope,
    ) -> Result<(
        ConnectionLease,
        Vec<u8>,
        Option<flow_control::Charge<AdmissionPolicy>>,
    )> {
        let (admission, _) = &self.wire;
        let observer = admission.policy().observer();
        let (body, staging_reservation) = if length == 0 {
            (Vec::new(), None)
        } else {
            let cache = &request.request.origin.object.cache;
            let mut reservation = admission
                .reserve(Some(cache), ResourceClass::Ciphertext, length)
                .map_err(Error::from);
            if matches!(reservation, Err(Error::Overloaded))
                && let Some(reclaim) = &self.reclaim
            {
                reclaim(cache, length);
                reservation = admission
                    .reserve(Some(cache), ResourceClass::Ciphertext, length)
                    .map_err(Error::from);
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
                if racer_crypto::identity::canonical_uuid(&peer.0) {
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
                            now: timestamp(uring_runtime::environment::now()),
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
                let bytes = observe_read(Ok(completion.bytes), peer_admission, failure, scope)?;
                if bytes == 0 || bytes > length - offset {
                    record(Error::Io, offset, first, last, reads);
                    return Err(Error::Io);
                }
                offset += completion.bytes;
                let now = uring_runtime::environment::now();
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
        Ok((connection, body, staging_reservation))
    }
}

/// Logical decoding must account for every body byte before pooling.
fn validate_body_length(response: &PeerResponse, length: usize) -> Result<()> {
    match response {
        PeerResponse::Bootstrap {
            page_zero: Some(ciphertext),
            ..
        } if ciphertext.bytes().len() == length => {}
        PeerResponse::Bootstrap {
            page_zero: Some(_), ..
        } => return Err(Error::InvalidRequest),
        PeerResponse::Page { ciphertext, .. } | PeerResponse::Selected { ciphertext, .. }
            if ciphertext.bytes().len() == length => {}
        PeerResponse::Page { .. } | PeerResponse::Selected { .. } => {
            return Err(Error::InvalidRequest);
        }
        _ if length == 0 => {}
        _ => return Err(Error::InvalidRequest),
    }
    Ok(())
}
/// Both connections and all relay resources follow the readiness completion fence.
type Transit =
    http1::relay::Relay<crate::http::HttpContext, flow_control::PipeLease<AdmissionPolicy>>;
pub(crate) async fn relay_body(
    io: &HttpIo,
    source: crate::http::ConnectionLease,
    destination: crate::http::ConnectionLease,
    pipe: Option<flow_control::PipeLease<AdmissionPolicy>>,
    scope: &RequestScope,
) -> Result<crate::http::ConnectionLease> {
    if source.receive_remaining() != destination.send_remaining()
        || source.receive_remaining().is_none()
    {
        return Err(Error::InvalidRequest);
    }
    if source.receive_remaining() == Some(0) {
        let mut source = source;
        let mut destination = destination;
        finish_relay(&mut source, &mut destination)?;
        destination.state_mut().relay_reservation = None;
        return Ok(destination);
    }
    #[cfg(test)]
    let copied = destination.state().relay_fallback;
    #[cfg(test)]
    let fallback_at = destination.state().relay_fallback_at;
    let state = Transit::new(source, destination, pipe.ok_or(Error::InvalidRequest)?)?;
    #[cfg(test)]
    let state = {
        let mut state = state;
        state.force_fallback(copied, fallback_at);
        state
    };
    let state = Rc::new(std::cell::RefCell::new(state));
    loop {
        scope.check()?;
        let step = state.borrow_mut().step(io)?;
        let wait = match step {
            http1::relay::Step::Complete => break,
            http1::relay::Step::Yield => None,
            http1::relay::Step::Readiness { socket, interest } => Some((socket, interest)),
        };
        wait_relay_progress(io, state.clone(), wait, scope).await?;
    }
    scope.check()?;
    let mut state = Rc::try_unwrap(state)
        .map_err(|_| Error::Internal)?
        .into_inner();
    let (source, destination) = state.connections_mut();
    finish_relay(source, destination)?;
    destination.state_mut().relay_reservation = None;
    Ok(state.into_destination())
}
async fn wait_relay_progress(
    io: &HttpIo,
    state: Rc<std::cell::RefCell<Transit>>,
    wait: Option<(Rc<uring_runtime::reactor::descriptor::Descriptor>, i16)>,
    scope: &RequestScope,
) -> Result<()> {
    if let Some((fd, interest)) = wait {
        let mut tick = scope.clone();
        tick.deadline.0 = tick
            .deadline
            .0
            .min(uring_runtime::environment::now() + Duration::from_millis(10));
        match io
            .reactor()
            .readiness_with_lease(fd, interest as u32, state.clone(), &tick)
            .await
        {
            Ok(_) | Err(Error::DeadlineExceeded) => (),
            Err(error) => return Err(error),
        }
    } else {
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
    Ok(())
}
fn finish_relay(
    source: &mut crate::http::ConnectionLease,
    destination: &mut crate::http::ConnectionLease,
) -> Result<()> {
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

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn adaptive_socket_attribution_ignores_local_pressure_and_expired_scope() {
        let scope = RequestScope::new(
            crate::model::RequestId([91; 16]),
            uring_runtime::environment::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        let owner =
            crate::peer::AdaptivePeers::new(Default::default(), Default::default()).unwrap();
        let node = racer_control_wire::NodeId("peer".into());
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
    mod native_exchange_tests {
        //! Signed native offer/fallback exchanges over real sockets and optional hardware.
        use super::*;
        use crate::admission::ResourceClass;
        use crate::http::Codec;
        use crate::memory::BufferPool;
        use crate::model::ExpiresAt;
        use crate::model::Nonce;
        use crate::model::ObjectMetadata;
        use crate::model::PageEnvelope;
        use crate::model::*;
        use crate::peer::protocol as p;
        use crate::peer::protocol::Signatures;
        use crate::rdma::Devices;
        use crate::rdma::Sessions;
        use crate::runtime::Reactor;
        use racer_control_wire::KeyId;
        use std::os::unix::net::UnixStream;
        use std::rc::Rc;
        use std::sync::Arc;
        use std::task::Context;
        use std::task::Poll;
        use std::time::Duration;
        use std::time::Instant;
        fn transfers(
            signatures: Rc<Signatures>,
            admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
            reactor: &Rc<Reactor>,
        ) -> Transfers {
            let devices = Rc::new(Devices::new());
            let sessions = Rc::new(Sessions::new(devices.clone(), 2));
            let rdma = sessions.clone();
            let io = Rc::new(crate::http::new_io(
                reactor.clone(),
                Codec::new(p::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
            ));
            Transfers::new(
                Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 2)),
                io,
                Some(rdma),
                admission.clone(),
                Rc::new(p::SecurityCodec::new(
                    admission.clone(),
                    BufferPool::new(admission.clone()),
                )),
                signatures.clone(),
            )
            .with_native(sessions)
        }
        fn test_membership(
            signers: &[Rc<Signatures>],
            nodes: &[usize],
            sites: &[&str],
        ) -> std::sync::Arc<crate::topology::Membership> {
            use crate::topology::Member;
            use crate::topology::Membership;
            use racer_control_wire::RailId;
            use racer_control_wire::RailMapping;
            Arc::new(
                Membership::validate(
                    MembershipVersion(1),
                    nodes
                        .iter()
                        .zip(sites)
                        .map(|(&i, site)| Member {
                            node: signers[i].node().clone(),
                            shares: std::num::NonZeroU32::new(1).unwrap(),
                            peer_endpoint: format!("127.0.0.1:{}", 9000 + i),
                            rails: vec![RailMapping {
                                rail: RailId(7),
                                device: "test-provider".into(),
                                port: 1,
                                gid: None,
                                numa_node: None,
                            }],
                            site: (*site).into(),
                        })
                        .collect(),
                )
                .unwrap(),
            )
        }
        #[test]
        fn real_socket_signed_offer_falls_back_when_local_provider_is_unavailable() {
            offer_fallback(false, None, false);
        }
        #[test]
        fn real_socket_sender_failure_requires_signed_fallback_before_ciphertext() {
            offer_fallback(true, None, false);
        }
        #[test]
        fn signed_offer_rejects_cross_site_and_missing_site_before_session_preparation() {
            for sites in [["site1", "site2"], ["", "site1"], ["site1", ""], ["", ""]] {
                offer_fallback(false, Some(sites), false);
            }
        }
        #[test]
        fn signed_offer_cannot_substitute_the_actual_local_identity_in_response_path() {
            offer_fallback(false, None, true);
        }
        #[test]
        fn native_subdeadline_preserves_parent_deadline_and_cancellation() {
            let scope =
                RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(30))
                    .unwrap();
            let native = native_scope(&scope);
            assert!(native.deadline.0 <= scope.deadline.0);
            assert!(native_failure(Error::DeadlineExceeded, &scope));
            assert!(native_failure(Error::Os(libc::ECONNRESET), &scope));
            assert!(!native_failure(
                Error::RenameUncertain(crate::error::PublicationCause::Io),
                &scope
            ));
            assert!(!native_failure(
                Error::PublishedNotDurable(crate::error::PublicationCause::Os(libc::EIO)),
                &scope
            ));
            assert!(!native_failure(Error::Unauthorized, &scope));
            scope.cancel().unwrap();
            assert_eq!(native.check(), Err(Error::Cancelled));
            assert!(!native_failure(Error::DeadlineExceeded, &scope));
            assert!(!native_failure(Error::Os(libc::ECONNRESET), &scope));
        }
        fn offer_fallback(
            sender_failure: bool,
            rejected_sites: Option<[&str; 2]>,
            wrong_path: bool,
        ) {
            let rejected = rejected_sites.is_some() || wrong_path;
            let signers = crate::peer::tests::signers();
            let membership = test_membership(
                &signers,
                &[0, 2],
                &rejected_sites.unwrap_or(["site1", "site1"]),
            );
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let receiver = transfers(signers[0].clone(), &admission, &reactor);
            let sender = transfers(signers[2].clone(), &admission, &reactor);
            let scope =
                RequestScope::new(RequestId([5; 16]), Instant::now() + Duration::from_secs(10))
                    .unwrap();
            let cache = CacheId("cccccccc-1111-4111-8111-111111111111".into());
            let version = ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: CacheKey([3; 32]),
                },
                etag: StrongEtag::parse(b"\"v1\"").unwrap(),
            };
            let envelope = PageEnvelope {
                page: PageId {
                    version: version.clone(),
                    number: PageNumber(0),
                },
                key_id: KeyId([1; 16]),
                nonce: Nonce([2; 24]),
                plaintext_length: 19,
                ciphertext_length: 35,
            };
            let page = BufferPool::new(admission.clone())
                .ciphertext(
                    admission
                        .reserve(Some(&cache), ResourceClass::Ciphertext, 35)
                        .unwrap(),
                    envelope,
                    vec![77; 35],
                )
                .unwrap();
            let response = PeerResponse::Page {
                metadata: ObjectMetadata {
                    content_type: None,
                    version,
                    length: 19,
                    expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
                },
                ciphertext: page,
            };
            let mut head = p::response_head(
                &response,
                &[4; 32],
                &[
                    signers[usize::from(wrong_path)].node().clone(),
                    signers[2].node().clone(),
                ],
            )
            .unwrap();
            p::push(&mut head, "racer-receiver", &signers[0].node().0);
            let authentication = ForwardedHead {
                original: Arc::new(signers[2].sign(head).unwrap()),
                hops: vec![],
            };
            let mut binding = Binding {
                request: [4; 32],
                response: [0; 32],
                transfer: TransferId([6; 16]),
                membership: 1,
                deadline: p::encode_deadline(scope.deadline).unwrap(),
                rail: RailId(7),
            };
            let accept = binding
                .sign(
                    &signers[0],
                    signers[2].node(),
                    Phase::Accept,
                    &[0; 32],
                    0,
                    vec![],
                )
                .unwrap();
            binding.response = envelope_digest(&authentication).unwrap();
            // A well-formed signed remote endpoint cannot manufacture a local provider.
            let mut setup = b"racer-rdma-setup-v1\0".to_vec();
            setup.extend_from_slice(&7u16.to_be_bytes());
            setup.extend_from_slice(&[1; 16]);
            setup.extend_from_slice(&[2; 16]);
            for value in [1u32, 2, 3] {
                setup.extend_from_slice(&value.to_be_bytes());
            }
            setup.extend_from_slice(&1u16.to_be_bytes());
            setup.extend_from_slice(&[1, 1]);
            let offer = binding
                .sign(
                    &signers[2],
                    signers[0].node(),
                    Phase::Offer,
                    &signed_digest(&accept).unwrap(),
                    0,
                    vec![extension(SETUP_HEADER, p::binary(&setup).into_bytes())],
                )
                .unwrap();
            let previous = signed_digest(&offer).unwrap();
            let mut offered = encode_envelope(&authentication, true, 0).unwrap();
            attach(&mut offered, &offer).unwrap();
            let response = SignedResponse {
                authentication,
                response,
            };
            let (a, b) = UnixStream::pair().unwrap();
            let a = crate::http::from_accepted(a.into(), &admission).unwrap();
            let b = crate::http::from_accepted(b.into(), &admission).unwrap();
            let receive = async {
                let a = p::connect(
                    &receiver.io,
                    a,
                    signers[0].clone(),
                    signers[2].node(),
                    &scope,
                )
                .await?;
                let initial = MessageHead {
                    start: StartLine::Request {
                        method: "POST".into(),
                        target: "/test".into(),
                    },
                    headers: vec![extension("content-length", b"0".to_vec())],
                };
                let sent = receiver.io.send_head(a, initial, &scope).await?;
                let mut received = receiver.io.receive_head(sent.connection, &scope).await?;
                let offer = detach(&mut received.value)?.unwrap();
                let (auth, _) = decode_envelope(received.value, true)?;
                let mut original = binding.clone();
                original.response = [0; 32];
                if sender_failure {
                    let (verified, _) = binding.verify(
                        &signers[0],
                        signers[2].node(),
                        offer,
                        &[Phase::Offer],
                        &signed_digest(&accept)?,
                        0,
                        &scope,
                    )?;
                    let mut connection = received.connection;
                    connection.finish_exchange()?;
                    let setup = binding.sign(
                        &signers[0],
                        signers[2].node(),
                        Phase::Setup,
                        &signed_digest(&verified.signed)?,
                        0,
                        vec![
                            extension(SETUP_HEADER, p::binary(&[1; 32]).into_bytes()),
                            extension(SETUP_BINDING_HEADER, p::binary(&[2; 32]).into_bytes()),
                        ],
                    )?;
                    let setup_digest = signed_digest(&setup)?;
                    connection = receiver.write_control(connection, setup, &scope).await?;
                    let failed = receiver.read_control(connection, true, &scope).await?;
                    let (mut connection, signed) = failed;
                    let (failed, _) = binding.verify(
                        &signers[0],
                        signers[2].node(),
                        signed,
                        &[Phase::Failed],
                        &setup_digest,
                        0,
                        &scope,
                    )?;
                    connection.finish_exchange()?;
                    return receiver
                        .receive_fallback(
                            connection,
                            auth,
                            &binding,
                            signers[2].node(),
                            signed_digest(&failed.signed)?,
                            &scope,
                        )
                        .await;
                }
                receiver
                    .receive_native(
                        received.connection,
                        auth,
                        original,
                        accept,
                        signers[2].node().clone(),
                        offer,
                        &membership,
                        &scope,
                    )
                    .await
            };
            let send = async {
                let b = p::accept(&sender.io, b, signers[2].clone(), &scope).await?;
                let received = sender.io.receive_head(b, &scope).await?;
                let sent = sender
                    .io
                    .send_head(received.connection, offered, &scope)
                    .await?;
                let mut connection = sent.connection;
                connection.finish_exchange()?;
                if rejected {
                    // Keep the connection alive until the receiver returns its rejection.
                    return std::future::pending::<Result<()>>().await;
                }
                if sender_failure {
                    let (connection, setup) =
                        sender.read_control(connection, false, &scope).await?;
                    let (setup, _) = binding.verify(
                        &signers[2],
                        signers[0].node(),
                        setup,
                        &[Phase::Setup],
                        &previous,
                        0,
                        &scope,
                    )?;
                    let (mut connection, _) = sender
                        .failed_then_fallback(
                            connection,
                            &response,
                            &binding,
                            signers[0].node(),
                            signed_digest(&setup.signed)?,
                            &scope,
                        )
                        .await?;
                    connection.finish_exchange()?;
                    return Ok(());
                }
                let (connection, fallback) = sender.read_control(connection, false, &scope).await?;
                let (verified, phase) = binding.verify(
                    &signers[2],
                    signers[0].node(),
                    fallback,
                    &[Phase::Fallback],
                    &previous,
                    0,
                    &scope,
                )?;
                assert_eq!(phase, Phase::Fallback);
                let (mut connection, sent) = sender
                    .send_fallback(
                        connection,
                        &response,
                        &binding,
                        signers[0].node(),
                        signed_digest(&verified.signed)?,
                        &scope,
                    )
                    .await?;
                assert!(sent);
                connection.finish_exchange()?;
                Ok::<(), Error>(())
            };
            let mut future = std::pin::pin!(async { futures::try_join!(receive, send) });
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let result = loop {
                if let Poll::Ready(result) = std::future::Future::poll(future.as_mut(), &mut cx) {
                    break result;
                }
                reactor.poll_budgeted(128).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            };
            if rejected {
                assert!(matches!(result, Err(Error::Unauthorized)));
                assert_eq!(receiver.native.as_ref().unwrap().prepare_attempts.get(), 0);
                return;
            }
            let (result, ()) = result.unwrap();
            assert_eq!(
                envelope_digest(&result.authentication).unwrap(),
                binding.response
            );
            match result.response {
                PeerResponse::Page { ciphertext, .. } => assert_eq!(ciphertext.bytes(), &[77; 35]),
                _ => panic!("expected retained ciphertext"),
            }
            assert_eq!(admission.used(ResourceClass::Registered), 0);
        }
        #[cfg(feature = "rdma")]
        #[test]
        #[ignore = "requires real ABI v2 adapter and RACER_RDMA_TEST_DEVICE/PORT/GID for an active type-2B port"]
        fn native_provider_signed_setup_grant_write_completion_roundtrip() {
            native_roundtrip(false, false, None, false);
        }
        #[test]
        fn simulated_native_mixed_site_hop_both_directions() {
            for reverse in [false, true] {
                for relayed in [false, true] {
                    native_roundtrip(true, reverse, None, relayed);
                }
            }
        }
        #[test]
        fn simulated_native_sender_rejects_cross_site_before_session_preparation() {
            for site in ["site2", ""] {
                native_roundtrip(true, false, Some(site), false);
                native_roundtrip(true, true, Some(site), true);
            }
        }
        fn native_roundtrip(
            simulated: bool,
            reverse: bool,
            rejected_site: Option<&str>,
            relayed: bool,
        ) {
            use crate::topology::Member;
            use crate::topology::Membership;
            use racer_control_wire::RailId;
            use racer_control_wire::RailMapping;
            use rdma_verbs::NativeService;
            use rdma_verbs::pair;
            let reject_sender = rejected_site.is_some();
            let device = if simulated {
                "sim-rnic".into()
            } else {
                std::env::var("RACER_RDMA_TEST_DEVICE").expect("select real device")
            };
            let port = if simulated {
                1
            } else {
                std::env::var("RACER_RDMA_TEST_PORT")
                    .expect("select port")
                    .parse()
                    .unwrap()
            };
            let text = if simulated {
                "11".repeat(16)
            } else {
                std::env::var("RACER_RDMA_TEST_GID").expect("32 lowercase hex GID digits")
            };
            assert_eq!(text.len(), 32);
            let mut gid = [0; 16];
            for (i, b) in gid.iter_mut().enumerate() {
                *b = u8::from_str_radix(&text[2 * i..2 * i + 2], 16).unwrap();
            }
            let fabric = rdma_verbs::simulation::Simulation::new()
                .with_devices(vec![rdma_verbs::simulation::Device::new(
                    device.clone(),
                    gid,
                )])
                .unwrap();
            let _fabric = simulated.then(|| fabric.enter());
            let mut signers = crate::peer::tests::signers();
            if reverse {
                signers.swap(0, 2);
            }
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(true).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let mappings = vec![RailMapping {
                rail: RailId(7),
                device,
                port,
                gid: Some(gid),
                numa_node: None,
            }];
            let scope =
                RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(30))
                    .unwrap();
            let make = |signatures: Rc<Signatures>| {
                let (io, port) = pair(2).unwrap();
                let devices = Rc::new(Devices::new());
                devices.attach(io).unwrap();
                let sessions = Rc::new(Sessions::new(devices.clone(), 2));
                let rdma = sessions.clone();
                let http = Rc::new(crate::http::new_io(
                    reactor.clone(),
                    Codec::new(p::MAX_ENVELOPE_HEAD),
                    admission.clone(),
                    crate::model::PAGE_BYTES + 16,
                ));
                let transfer = Transfers::new(
                    Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 2)),
                    http,
                    Some(rdma),
                    admission.clone(),
                    Rc::new(p::SecurityCodec::new(
                        admission.clone(),
                        BufferPool::new(admission.clone()),
                    )),
                    signatures.clone(),
                )
                .with_native(sessions);
                (devices, NativeService::new(port), transfer)
            };
            let (receive_devices, mut receive_engine, receiver) = make(signers[0].clone());
            let (send_devices, mut send_engine, sender) = make(signers[2].clone());
            let mut activate = std::pin::pin!(async {
                futures::try_join!(
                    receive_devices.activate(mappings.clone(), &admission, 4096, &scope),
                    send_devices.activate(mappings.clone(), &admission, 4096, &scope)
                )
            });
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            loop {
                if let Poll::Ready(result) = std::future::Future::poll(activate.as_mut(), &mut cx) {
                    result.expect("native provider activation must succeed");
                    break;
                }
                receive_engine.poll_budgeted(8).unwrap();
                send_engine.poll_budgeted(8).unwrap();
            }
            let cache = CacheId("cccccccc-1111-4111-8111-111111111111".into());
            let version = ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: CacheKey([9; 32]),
                },
                etag: StrongEtag::parse(b"\"provider\"").unwrap(),
            };
            let envelope = PageEnvelope {
                page: PageId {
                    version: version.clone(),
                    number: PageNumber(0),
                },
                key_id: KeyId([2; 16]),
                nonce: Nonce([3; 24]),
                plaintext_length: 128,
                ciphertext_length: 144,
            };
            let page = BufferPool::new(admission.clone())
                .ciphertext(
                    admission
                        .reserve(Some(&cache), ResourceClass::Ciphertext, 144)
                        .unwrap(),
                    envelope,
                    vec![0x5a; 144],
                )
                .unwrap();
            let response = PeerResponse::Page {
                metadata: ObjectMetadata {
                    content_type: None,
                    version,
                    length: 128,
                    expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
                },
                ciphertext: page,
            };
            let path = if relayed {
                vec![
                    signers[0].node().clone(),
                    signers[2].node().clone(),
                    signers[1].node().clone(),
                ]
            } else {
                vec![
                    signers[1].node().clone(),
                    signers[0].node().clone(),
                    signers[2].node().clone(),
                ]
            };
            let mut head = p::response_head(&response, &[4; 32], &path).unwrap();
            p::push(&mut head, "racer-receiver", &path[1].0);
            let original = Arc::new(signers[if relayed { 1 } else { 2 }].sign(head).unwrap());
            let mut hops = vec![];
            if relayed {
                let mut hop = MessageHead {
                    start: StartLine::Request {
                        method: "POST".into(),
                        target: "/racer/peer/v1/hop".into(),
                    },
                    headers: vec![],
                };
                p::push(&mut hop, "racer-kind", "response-hop");
                p::push(&mut hop, "content-length", 0);
                p::push_binary(
                    &mut hop,
                    "racer-original",
                    &signed_digest(&original).unwrap(),
                );
                p::push_binary(
                    &mut hop,
                    "racer-previous",
                    &signed_digest(&original).unwrap(),
                );
                p::push_binary(&mut hop, "racer-request-binding", &[4; 32]);
                p::push(&mut hop, "racer-response-path", p::nodes(&path).unwrap());
                p::push(&mut hop, "racer-reverse-index", 0);
                p::push(&mut hop, "racer-receiver", &signers[0].node().0);
                hops.push(signers[2].sign(hop).unwrap());
            }
            let response = SignedResponse {
                authentication: ForwardedHead { original, hops },
                response,
            };
            let binding = Binding {
                request: [4; 32],
                response: [0; 32],
                transfer: TransferId([6; 16]),
                membership: 1,
                deadline: p::encode_deadline(scope.deadline).unwrap(),
                rail: RailId(7),
            };
            let accept = binding
                .sign(
                    &signers[0],
                    signers[2].node(),
                    Phase::Accept,
                    &[0; 32],
                    0,
                    vec![],
                )
                .unwrap();
            let accept_wire = p::encode_signed(&accept).unwrap();
            let membership = Arc::new(
                Membership::validate(
                    MembershipVersion(1),
                    [0, 1, 2]
                        .into_iter()
                        .map(|i| Member {
                            node: signers[i].node().clone(),
                            shares: std::num::NonZeroU32::new(1).unwrap(),
                            peer_endpoint: format!("127.0.0.1:{}", 9000 + i),
                            rails: mappings.clone(),
                            site: if i == 1 {
                                "site2"
                            } else if i == 2 {
                                rejected_site.unwrap_or("site1")
                            } else {
                                "site1"
                            }
                            .into(),
                        })
                        .collect(),
                )
                .unwrap(),
            );
            let (a, b) = UnixStream::pair().unwrap();
            let a = crate::http::from_accepted(a.into(), &admission).unwrap();
            let b = crate::http::from_accepted(b.into(), &admission).unwrap();
            let receive = async {
                let a = p::connect(
                    &receiver.io,
                    a,
                    signers[0].clone(),
                    signers[2].node(),
                    &scope,
                )
                .await?;
                let initial = MessageHead {
                    start: StartLine::Request {
                        method: "POST".into(),
                        target: "/test".into(),
                    },
                    headers: vec![extension("content-length", b"0".to_vec())],
                };
                let sent = receiver.io.send_head(a, initial, &scope).await?;
                let mut offered = receiver.io.receive_head(sent.connection, &scope).await?;
                let control = detach(&mut offered.value)?;
                let (auth, length) = decode_envelope(offered.value, true)?;
                if reject_sender {
                    assert!(control.is_none());
                    let (_, body) = receiver
                        .read_ciphertext(offered.connection, length, &scope)
                        .await?;
                    return receiver
                        .wire
                        .1
                        .response(auth, body.bytes()?.to_vec(), &scope);
                }
                let control = control.ok_or(Error::Unavailable)?;
                assert_eq!(length, 0, "provider test requires native offer");
                receiver
                    .receive_native(
                        offered.connection,
                        auth,
                        binding.clone(),
                        accept,
                        signers[2].node().clone(),
                        control,
                        &membership,
                        &scope,
                    )
                    .await
            };
            let send = async {
                let b = p::accept(&sender.io, b, signers[2].clone(), &scope).await?;
                let received = sender.io.receive_head(b, &scope).await?;
                let (verified, _) = binding.verify(
                    &signers[2],
                    signers[0].node(),
                    p::decode_signed(&accept_wire)?,
                    &[Phase::Accept],
                    &[0; 32],
                    0,
                    &scope,
                )?;
                let (mut conn, sent) = sender
                    .send_native(
                        received.connection,
                        &response,
                        (binding.clone(), verified),
                        &membership,
                        &scope,
                    )
                    .await?;
                if reject_sender {
                    assert!(!sent);
                    assert_eq!(sender.native.as_ref().unwrap().prepare_attempts.get(), 0);
                    let head = encode_envelope(&response.authentication, true, 144)?;
                    conn = sender.io.send_head(conn, head, &scope).await?.connection;
                    let PeerResponse::Page { ciphertext, .. } = &response.response else {
                        unreachable!()
                    };
                    conn = sender
                        .io
                        .write_body(conn, ciphertext.clone(), &scope)
                        .await?
                        .lease;
                } else {
                    assert!(sent);
                }
                assert_eq!(conn.send_remaining(), Some(0));
                assert_eq!(conn.receive_remaining(), Some(0));
                conn.finish_exchange()?;
                Ok::<(), Error>(())
            };
            let mut exchange = std::pin::pin!(async { futures::try_join!(receive, send) });
            let (result, ()) = loop {
                if let Poll::Ready(result) = std::future::Future::poll(exchange.as_mut(), &mut cx) {
                    break result.expect("native provider roundtrip");
                }
                receive_engine.poll_budgeted(8).unwrap();
                send_engine.poll_budgeted(8).unwrap();
                reactor.poll_budgeted(128).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            };
            match result.response {
                PeerResponse::Page { ciphertext, .. } => {
                    assert_eq!(ciphertext.bytes(), &[0x5a; 144])
                }
                _ => panic!("expected page"),
            }
            assert_eq!(
                sender.native_completed.get(),
                usize::from(!reject_sender),
                "HTTP fallback cannot pass a native success test"
            );
            assert_eq!(sender.native_fallbacks.get(), 0);
            assert_eq!(
                receiver.native_completions.get(),
                usize::from(!reject_sender)
            );
        }
    }
    #[test]
    fn wire_checkout_reuses_zeroed_payload_without_moving_or_releasing_its_charge() {
        use racer_control_wire::CacheId;
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
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
            assert_eq!(reservation.key(), reserved.then_some(&second));
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
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
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
        use std::hint::black_box;
        use std::time::Instant;
        const ITERATIONS: usize = 128;
        for length in [1 << 20, 16 << 20, (16 << 20) + 16] {
            for reserved in [false, true] {
                let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                ));
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
    mod native_control_tests {
        use super::*;
        fn scope() -> RequestScope {
            RequestScope::new(
                crate::model::RequestId([4; 16]),
                std::time::Instant::now() + Duration::from_secs(30),
            )
            .unwrap()
        }
        fn binding(scope: &RequestScope) -> Binding {
            Binding {
                request: [1; 32],
                response: [2; 32],
                transfer: TransferId([3; 16]),
                membership: 1,
                deadline: p::encode_deadline(scope.deadline).unwrap(),
                rail: RailId(7),
            }
        }
        fn copy(control: &SignedHead) -> SignedHead {
            p::decode_signed(&p::encode_signed(control).unwrap()).unwrap()
        }
        #[test]
        fn session_admitted_setup_grant_completion_still_require_exact_transfer_and_phase() {
            use crate::peer::protocol::tests::sessions::pair;
            use crate::peer::protocol::tests::sessions::signer;
            let (mut sender, mut receiver) = pair();
            let a = signer(&sender);
            let b = signer(&receiver);
            let scope = scope();
            let original = binding(&scope);
            for phase in [
                Phase::Accept,
                Phase::Offer,
                Phase::Setup,
                Phase::Ready,
                Phase::Grant,
                Phase::Complete,
                Phase::Done,
                Phase::Finish,
            ] {
                let extensions = phase
                    .fields()
                    .iter()
                    .map(|name| extension(name, p::binary(&[1; 32]).into_bytes()))
                    .collect();
                let control = original
                    .sign(&a, b.node(), phase, &[9; 32], 0, extensions)
                    .unwrap();
                let signed = sender.sign(frame(control).unwrap()).unwrap();
                let admitted = receiver.admit(signed).unwrap();
                let control = unframe(admitted, phase.response()).unwrap();
                let mut wrong = original.clone();
                wrong.transfer.0[0] ^= 1;
                assert!(
                    wrong
                        .verify(&b, a.node(), copy(&control), &[phase], &[9; 32], 0, &scope)
                        .is_err()
                );
                assert!(
                    original
                        .verify(
                            &b,
                            a.node(),
                            copy(&control),
                            &[Phase::Fallback],
                            &[9; 32],
                            0,
                            &scope
                        )
                        .is_err()
                );
                original
                    .verify(&b, a.node(), control, &[phase], &[9; 32], 0, &scope)
                    .unwrap();
            }
        }
        #[test]
        fn exact_signed_controls_reject_every_binding_substitution_and_unknown_field() {
            let nodes = crate::peer::tests::signers();
            let scope = scope();
            let original = binding(&scope);
            let signed = || {
                original
                    .sign(
                        &nodes[0],
                        nodes[1].node(),
                        Phase::Fallback,
                        &[9; 32],
                        0,
                        vec![],
                    )
                    .unwrap()
            };
            for mutation in 0..9 {
                let mut expected = original.clone();
                match mutation {
                    0 => expected.request[0] ^= 1,
                    1 => expected.response[0] ^= 1,
                    2 => expected.transfer.0[0] ^= 1,
                    3 => expected.membership += 1,
                    4 => expected.deadline += 1,
                    5 => expected.rail.0 += 1,
                    _ => {}
                }
                let previous = if mutation == 6 { [8; 32] } else { [9; 32] };
                let phase = if mutation == 7 {
                    Phase::Done
                } else {
                    Phase::Fallback
                };
                let peer = if mutation == 8 {
                    nodes[2].node()
                } else {
                    nodes[0].node()
                };
                assert!(
                    expected
                        .verify(&nodes[1], peer, signed(), &[phase], &previous, 0, &scope)
                        .is_err()
                );
            }
            let mut head = original.head(Phase::Fallback, &[9; 32], 0, vec![]).unwrap();
            p::push(&mut head, "racer-receiver", &nodes[1].node().0);
            p::push(&mut head, "racer-extra", 1);
            assert!(
                original
                    .verify(
                        &nodes[1],
                        nodes[0].node(),
                        nodes[0].sign(head).unwrap(),
                        &[Phase::Fallback],
                        &[9; 32],
                        0,
                        &scope
                    )
                    .is_err()
            );
            let signed = signed();
            let copied = copy(&signed);
            for control in [signed, copied] {
                original
                    .verify(
                        &nodes[1],
                        nodes[0].node(),
                        control,
                        &[Phase::Fallback],
                        &[9; 32],
                        0,
                        &scope,
                    )
                    .unwrap();
            }
        }
        #[test]
        fn control_extension_and_fallback_length_schema_is_closed() {
            let binding = binding(&scope());
            for (phase, length, extensions) in [
                (Phase::Offer, 0, vec![]),
                (
                    Phase::Complete,
                    0,
                    vec![extension("racer-rdma-completion", b"bad".to_vec())],
                ),
                (
                    Phase::Grant,
                    17,
                    vec![extension(
                        "racer-rdma-descriptor",
                        p::binary(&[0; 32]).into_bytes(),
                    )],
                ),
                (Phase::Finish, usize::MAX, vec![]),
            ] {
                assert!(binding.head(phase, &[0; 32], length, extensions).is_err());
            }
            assert!(binding.head(Phase::Finish, &[0; 32], 17, vec![]).is_ok());
        }
    }
}
