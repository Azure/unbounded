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
use crate::rdma::AuthenticatedGrant;
use crate::rdma::COMPLETION_HEADER;
use crate::rdma::GRANT_HEADER;
use crate::rdma::PendingReceive;
use crate::rdma::SETUP_BINDING_HEADER;
use crate::rdma::SETUP_HEADER;
use crate::rdma::TransportPlan;
use crate::runtime::RequestScope;
use crate::telemetry::BodyProgress;
use crate::telemetry::Detail;
use crate::telemetry::Event;
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
use std::time::Duration;
use uring_runtime::environment::now;
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
pub(crate) use peer_wire::control::Phase;
impl Binding {
    #[cfg(test)]
    fn head(
        &self,
        phase: Phase,
        previous: &[u8; 32],
        length: usize,
        extensions: Vec<Header>,
    ) -> Result<MessageHead> {
        self.wire()
            .head(phase, previous, length, extensions)
            .map_err(Into::into)
    }

    fn wire(&self) -> peer_wire::control::Binding {
        peer_wire::control::Binding {
            request: self.request,
            response: self.response,
            transfer: self.transfer.0,
            membership: self.membership,
            deadline: self.deadline,
            rail: self.rail,
        }
    }
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
    pub fn sign(
        &self,
        signatures: &Signatures,
        to: &NodeId,
        phase: Phase,
        previous: &[u8; 32],
        length: usize,
        extensions: Vec<Header>,
    ) -> Result<SignedHead> {
        self.wire()
            .sign(signatures.wire(), to, phase, previous, length, extensions)
            .map_err(Into::into)
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
        let (verified, phase) =
            self.wire()
                .verify(signatures.wire(), from, signed, allowed, previous, length)?;
        let (signed, peer) = verified.into_parts();
        Ok((VerifiedHead { signed, peer }, phase))
    }
    pub fn parse_accept(signed: &SignedHead) -> Result<Self> {
        let binding = peer_wire::control::Binding::parse_accept(signed)?;
        Ok(Self {
            request: binding.request,
            response: binding.response,
            transfer: TransferId(binding.transfer),
            membership: binding.membership,
            deadline: binding.deadline,
            rail: binding.rail,
        })
    }
}
/// Bind all original/hop signatures using security's canonical digest, never bodies.
fn envelope_digest(auth: &ForwardedHead) -> Result<[u8; 32]> {
    peer_wire::control::envelope_digest(auth).map_err(Into::into)
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

    /// Record an RDMA transfer stage when native links exist.
    fn record_rdma_stage(&self, count: Event, sum: Event, start: std::time::Instant) {
        if let Some(links) = &self.links {
            links.record_stage(count, sum, start);
        }
    }

    /// Sender side of one page: write into the receiver's signed grant with
    /// immediate, then send one signed response head, `Complete`, or release
    /// the grant and fall back to the HTTP body (`designs/racer-rdma-sessions.md`,
    /// "Per-page protocol"). Every granted tag gets exactly one write or release
    /// unless the link breaks, which fences everything.
    pub(super) async fn send_native(
        &self,
        connection: ConnectionLease,
        response: &SignedResponse,
        admitted: (Binding, VerifiedHead, AuthenticatedGrant),
        membership: &std::sync::Arc<crate::topology::Membership>,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, bool)> {
        let (mut binding, accept, grant) = admitted;
        let Some(links) = &self.links else {
            return Ok((connection, false));
        };
        let serving = now();
        let peer = accept.peer.node();
        let (PeerResponse::Page { ciphertext, .. } | PeerResponse::Selected { ciphertext, .. }) =
            &response.response
        else {
            links.release(peer, &grant);
            return Ok((connection, false));
        };
        let checked = (|| {
            if binding.membership != membership.version.0 {
                return Err(Error::IncompatibleMembership);
            }
            let mut bounded = scope.clone();
            bounded.deadline.0 = bounded
                .deadline
                .0
                .min(crate::peer::protocol::decode_deadline(binding.deadline)?.0);
            bounded.check()?;
            let plan = self.response_plan(
                &connection,
                &response.authentication,
                membership,
                &ciphertext.envelope().page,
                peer,
                true,
            )?;
            Ok((bounded, plan))
        })();
        let (bounded, plan) = match checked {
            Ok(checked) => checked,
            Err(error) => {
                links.release(peer, &grant);
                return Err(error);
            }
        };
        let scope = &bounded;
        if plan != (TransportPlan::Rdma { rail: binding.rail }) {
            links.release(peer, &grant);
            return Ok((connection, false));
        }
        binding.response = envelope_digest(&response.authentication)?;
        let previous = signed_digest(&accept.signed)?;
        // `Links::send` writes the page in place, or stages a copy without ODP.
        let sending = now();
        let sent = links
            .send(peer, &grant, ciphertext, &native_scope(scope))
            .await;
        let sent = match sent {
            Ok(sent) => {
                links.record_stage(Event::RdmaSendCount, Event::RdmaSendNs, sending);
                sent
            }
            Err(error) => {
                links.release(peer, &grant);
                if !native_failure(error, scope) {
                    return Err(error);
                }
                return self
                    .send_fallback(connection, response, &binding, peer, previous, scope)
                    .await;
            }
        };
        let complete = binding.sign(
            &self.signatures,
            peer,
            Phase::Complete,
            &previous,
            0,
            vec![extension(COMPLETION_HEADER, sent.header_value())],
        )?;
        let mut head = encode_envelope(&response.authentication, true, 0)?;
        attach(&mut head, &complete)?;
        let connection = self.io.send_head(connection, head, scope).await?.connection;
        links.record_stage(Event::RdmaServeCount, Event::RdmaServeNs, serving);
        #[cfg(test)]
        self.native_completed.set(self.native_completed.get() + 1);
        Ok((connection, true))
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
    /// Whether a rail of this node has a landing buffer on GPU `ordinal`.
    pub(crate) fn landing_ready(&self, ordinal: i32) -> bool {
        self.links.as_ref().is_some_and(|links| {
            links
                .active()
                .iter()
                .any(|rail| links.has_landing(rail.rail, ordinal))
        })
    }
    /// Receiver: when the hop plan is RDMA and the link is ready, grant a page
    /// window in-band and sign it into the request's `Accept` control. Any
    /// recoverable failure means a plain HTTP request.
    ///
    /// With `landing`, the GPU of a `gpu-read` that asked for this fixed page,
    /// the grant is over a slot of that GPU's landing buffer when this rail
    /// has one and a slot is free. Otherwise it is a host grant as usual.
    pub(super) async fn accept_native(
        &self,
        request: &SignedRequest,
        plan: TransportPlan,
        landing: Option<i32>,
        scope: &RequestScope,
    ) -> Result<Option<(Binding, SignedHead, NodeId, PendingReceive)>> {
        let TransportPlan::Rdma { rail } = plan else {
            return Ok(None);
        };
        let Some(links) = &self.links else {
            return Ok(None);
        };
        let peer = crate::peer::protocol::receiver(
            &request
                .authentication
                .hops
                .last()
                .unwrap_or(&request.authentication.original)
                .head,
        )?;
        if links.ready(&peer, rail).is_none() {
            links.record_http_fallback();
            return Ok(None);
        }
        let binding = Binding::request(
            &request.authentication,
            request.request.route.membership.0,
            scope,
            rail,
        )?;
        let landed = match landing {
            Some(ordinal)
                if matches!(
                    request.request.operation,
                    crate::peer::protocol::Operation::Page { .. }
                ) && links.has_landing(rail, ordinal) =>
            {
                match links
                    .grant_landing(&peer, rail, binding.transfer, ordinal, &native_scope(scope))
                    .await
                {
                    Ok(pending) => Some(pending),
                    // No free landing slot: grant host memory instead.
                    Err(error) if native_failure(error, scope) => None,
                    Err(error) => return Err(error),
                }
            }
            _ => None,
        };
        let granted = match landed {
            Some(pending) => Ok(pending),
            None => {
                links
                    .grant(&peer, rail, binding.transfer, &native_scope(scope))
                    .await
            }
        };
        let pending = match granted {
            Ok(pending) => pending,
            Err(error) if native_failure(error, scope) => {
                links.record_http_fallback();
                return Ok(None);
            }
            Err(error) => return Err(error),
        };
        let accept = binding.sign(
            &self.signatures,
            &peer,
            Phase::Accept,
            &[0; 32],
            0,
            vec![extension(GRANT_HEADER, pending.header_value())],
        )?;
        Ok(Some((binding, accept, peer, pending)))
    }
    /// Sender: verify the requester's signed `Accept` and the grant it names.
    pub(super) fn admit_native(
        &self,
        request: &SignedRequest,
        control: SignedHead,
        scope: &RequestScope,
    ) -> Result<Option<(Binding, VerifiedHead, AuthenticatedGrant)>> {
        if self.links.is_none() {
            return Ok(None);
        }
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
        let grant = AuthenticatedGrant::from_verified(&verified, binding.rail, binding.transfer)?;
        Ok(Some((binding, verified, grant)))
    }
    async fn read_ciphertext(
        &self,
        connection: ConnectionLease,
        cache: Option<&racer_control_wire::CacheId>,
        length: usize,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, WireBuffer)> {
        let (admission, codec) = &self.wire;
        // Page bodies land in aligned storage charged to their cache.
        let buffer = match cache {
            Some(cache) if length != 0 => {
                let buffers = codec.buffers();
                let reservation = admission.reserve(
                    Some(cache),
                    ResourceClass::Ciphertext,
                    buffers.aligned_capacity(length)?,
                )?;
                WireBuffer::aligned(buffers, reservation, length)?
            }
            _ => WireBuffer::new(admission, length)?,
        };
        let read = self.io.read_body_exact(connection, buffer, scope).await?;
        Ok((read.lease, read.buffer))
    }
    /// Receiver: read the sender's one signed outcome. `Complete` means the
    /// page is in the granted window; anything after that is `Io`, because
    /// the HTTP body was never sent. `Finish` carries the HTTP body. A landing
    /// grant ([`Self::accept_native`] with a GPU) whose page the sender wrote
    /// comes back as [`RelayResponse::Landed`]; any other page as
    /// [`RelayResponse::Complete`].
    #[allow(clippy::too_many_arguments)] // Preserve explicit authenticated transfer inputs.
    pub(super) async fn receive_native(
        &self,
        connection: ConnectionLease,
        authentication: ForwardedHead,
        mut binding: Binding,
        accept: SignedHead,
        peer: NodeId,
        control: SignedHead,
        pending: PendingReceive,
        membership: &std::sync::Arc<crate::topology::Membership>,
        length: usize,
        scope: &RequestScope,
    ) -> Result<RelayResponse> {
        binding.response = envelope_digest(&authentication)?;
        let (verified, phase) = binding.verify(
            &self.signatures,
            &peer,
            control,
            &[Phase::Complete, Phase::Finish],
            &signed_digest(&accept)?,
            length,
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
            verified.peer.node(),
            false,
        )? != (TransportPlan::Rdma { rail: binding.rail })
        {
            return Err(Error::Unauthorized);
        }
        if phase == Phase::Finish {
            if let Some(links) = &self.links {
                links.record_http_fallback();
            }
            // The owner keeps the tag until the sender's release arrives.
            drop(pending);
            return self
                .receive_fallback(connection, authentication, length, scope)
                .await
                .map(RelayResponse::Complete);
        }
        if pending.is_landing() {
            let landed = pending
                .finish_landed(&verified, &envelope, &native_scope(scope))
                .await?;
            #[cfg(test)]
            self.native_completions
                .set(self.native_completions.get() + 1);
            // Only a fixed page request asks for landing. The signed head must
            // describe exactly this page; the requester verifies the chain
            // against its binding, and the GPU checks the tag.
            let original = &authentication.original.head;
            if crate::peer::protocol::field(original, "racer-outcome")? != "page" {
                return Err(Error::Unauthorized);
            }
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
                &crate::peer::protocol::landed_response_head(
                    &metadata,
                    &envelope,
                    &request_digest,
                    &path,
                )?,
                false,
            )?;
            let mut connection = connection;
            connection.finish_exchange()?;
            return Ok(RelayResponse::Landed(Box::new(LandedResponse {
                authentication,
                metadata,
                envelope,
                landed,
            })));
        }
        let (admission, _) = &self.wire;
        // The arrival stays in registered memory; retaining paths copy it out.
        let page = pending
            .finish(&verified, envelope, admission, &native_scope(scope))
            .await?;
        #[cfg(test)]
        self.native_completions
            .set(self.native_completions.get() + 1);
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
        let mut connection = connection;
        connection.finish_exchange()?;
        Ok(RelayResponse::Complete(SignedResponse {
            authentication,
            response,
        }))
    }
    async fn receive_fallback(
        &self,
        connection: ConnectionLease,
        authentication: ForwardedHead,
        length: usize,
        scope: &RequestScope,
    ) -> Result<SignedResponse> {
        scope.check()?;
        let cache = super::protocol::page_descriptor(&authentication.original.head)
            .ok()
            .map(|(metadata, _)| metadata.version.object.cache);
        let (mut connection, buffer) = self
            .read_ciphertext(connection, cache.as_ref(), length, scope)
            .await?;
        let (_, codec) = &self.wire;
        let response = match buffer.into_body() {
            // Unkeyed staging is recharged to the page's cache by the decoder.
            super::protocol::ResponseBody::Owned { bytes, .. } => {
                codec.response(authentication, bytes, scope)?
            }
            body => codec.response_body(authentication, body, scope)?,
        };
        validate_body_length(&response.response, length)?;
        connection.finish_exchange()?;
        Ok(response)
    }
}

/// Status for a link setup the responder declines without touching the owner.
fn link_status(status: u16) -> MessageHead {
    MessageHead {
        start: StartLine::Response { status },
        headers: vec![extension("content-length", b"0".to_vec())],
    }
}

/// Whether `local` and `peer` may hold a link on `rail`: both are members of
/// the same nonempty Site and both publish the rail of the rail domain (the
/// `select_hop` membership rules).
pub(crate) fn link_allowed(
    membership: &crate::topology::Membership,
    local: &NodeId,
    peer: &NodeId,
    rail: RailId,
) -> bool {
    let (Ok(ours), Ok(theirs)) = (membership.member(local), membership.member(peer)) else {
        return false;
    };
    !ours.site.is_empty()
        && ours.site == theirs.site
        && membership.rail_domain().contains(&rail)
        && ours.rails.iter().any(|m| m.rail == rail)
        && theirs.rails.iter().any(|m| m.rail == rail)
}

/// Initiator side of one link setup on an authenticated connection: open the
/// link, send the signed `RdmaLink` request, verify the signed response, and
/// connect. Any failure breaks the attempt, which backs the link off.
pub(crate) async fn link_exchange(
    io: &HttpIo,
    signatures: &Signatures,
    links: &crate::rdma::Links,
    connection: ConnectionLease,
    peer: &NodeId,
    rail: RailId,
    scope: &RequestScope,
) -> Result<ConnectionLease> {
    let (epoch, ours) = links.open(peer, rail, scope).await?;
    // Abandons the attempt on error or if this future is dropped mid-exchange.
    let attempt = crate::rdma::SetupAttempt::new(links, peer, rail, epoch);
    let result = async {
        let signed = peer_wire::link::RdmaLink {
            setup: ours.clone(),
            binding: None,
        }
        .sign(signatures.wire(), peer)?;
        let connection = io
            .send_head(connection, frame(signed)?, scope)
            .await?
            .connection;
        let received = io.receive_head(connection, scope).await?;
        if !matches!(received.value.start, StartLine::Response { status: 200 }) {
            // 409: our offer lost the tie-break; the peer's offer replaces it.
            // 429 and 503: try again after backoff.
            return Err(Error::Unavailable);
        }
        let mut connection = received.connection;
        let (verified, _) = peer_wire::link::RdmaLink::verify(
            signatures.wire(),
            Some(peer),
            unframe(received.value, true)?,
            true,
        )?;
        let (signed, verified_peer) = verified.into_parts();
        let head = VerifiedHead {
            signed,
            peer: verified_peer,
        };
        let theirs = crate::rdma::signed_value(&head, SETUP_HEADER, peer_wire::link::MAX_SETUP)?;
        let binding = crate::rdma::signed_value(&head, SETUP_BINDING_HEADER, 32)?;
        connection.finish_exchange()?;
        links
            .connect(peer, rail, epoch, &ours, &theirs, &binding, scope)
            .await?;
        Ok(connection)
    }
    .await;
    if result.is_ok() {
        attempt.finish();
    }
    result
}

/// Background link setup over a new pooled connection to `endpoint`.
pub(crate) async fn connect_link(
    http: Rc<HttpPool>,
    io: Rc<HttpIo>,
    signatures: Rc<Signatures>,
    links: Rc<crate::rdma::Links>,
    endpoint: crate::http::Endpoint,
    peer: NodeId,
    rail: RailId,
) -> Result<()> {
    let mut request = [0; 16];
    uring_runtime::environment::fill_random(&mut request).map_err(|_| Error::Unavailable)?;
    let scope = RequestScope::new(
        crate::model::RequestId(request),
        uring_runtime::environment::now() + crate::rdma::SETUP_DEADLINE,
    )?;
    let connection = http
        .checkout_with_state(&endpoint, Default::default(), &scope)
        .await?;
    let connection =
        crate::peer::protocol::connect(&io, connection, signatures.clone(), &peer, &scope).await?;
    link_exchange(&io, &signatures, &links, connection, &peer, rail, &scope).await?;
    Ok(())
}

impl Transfers {
    /// Start a background link setup when a page to `peer` on `rail` would use
    /// RDMA but the link is not ready. The page itself uses HTTP.
    pub(crate) fn ensure_link(
        &self,
        endpoint: &crate::http::Endpoint,
        peer: &NodeId,
        rail: RailId,
    ) {
        let Some(links) = &self.links else {
            return;
        };
        if !links.wants(peer, rail) {
            return;
        }
        let task = connect_link(
            self.http.clone(),
            self.io.clone(),
            self.signatures.clone(),
            links.clone(),
            endpoint.clone(),
            peer.clone(),
            rail,
        );
        links.spawn(Box::pin(async move {
            // Failures back the link off; pages keep using HTTP meanwhile.
            let _ = task.await;
            Ok(())
        }));
    }

    /// Responder side of an `RdmaLink` setup: verify the signed offer, check
    /// membership, rate limit and tie-break, accept, and answer signed. Declined
    /// offers get `409` (our concurrent offer wins), `429` (rate limit or
    /// overload), or `503` (no RDMA here) and never reach the owner.
    pub(crate) async fn serve_link(
        &self,
        connection: ConnectionLease,
        original: Arc<SignedHead>,
        membership: Result<Arc<crate::topology::Membership>>,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        let signed = Arc::try_unwrap(original).map_err(|_| Error::InvalidRequest)?;
        let (verified, _) =
            peer_wire::link::RdmaLink::verify(self.signatures.wire(), None, signed, false)?;
        let (signed, verified_peer) = verified.into_parts();
        let head = VerifiedHead {
            signed,
            peer: verified_peer,
        };
        let peer = head.peer.node().clone();
        if connection.state().session.as_ref().map(|s| s.peer()) != Some(&peer) {
            return Err(Error::Unauthorized);
        }
        let theirs = crate::rdma::signed_value(&head, SETUP_HEADER, peer_wire::link::MAX_SETUP)?;
        let rail = crate::rdma::link_setup::rail_of(&theirs)?;
        let local = self.signatures.node();
        let reply = match &self.links {
            None => link_status(503),
            Some(_) if !membership.is_ok_and(|m| link_allowed(&m, local, &peer, rail)) => {
                return Err(Error::Unauthorized);
            }
            Some(links) => match links.admit_setup(&peer, rail) {
                crate::rdma::SetupAdmission::Conflict => link_status(409),
                crate::rdma::SetupAdmission::Busy => link_status(429),
                crate::rdma::SetupAdmission::Accept => {
                    match links.accept(&peer, rail, &theirs, scope).await {
                        Ok(ours) => frame(
                            peer_wire::link::RdmaLink {
                                setup: ours,
                                binding: Some(crate::rdma::link_setup::binding(&theirs)),
                            }
                            .sign(self.signatures.wire(), &peer)?,
                        )?,
                        Err(Error::Overloaded) => link_status(429),
                        Err(error) if recoverable(error) => link_status(503),
                        Err(error) => return Err(error),
                    }
                }
            },
        };
        let mut connection = self
            .io
            .send_head(connection, reply, scope)
            .await?
            .connection;
        connection.finish_exchange()?;
        Ok(connection)
    }
}

/// Stable, quota-owned transport staging. Never contains plaintext page data.
/// Construction normalizes capacity before pointer extraction; Vec backing then
/// stays fixed while ownership moves through reactor completion closures.
///
/// Page bodies use [`WireBuffer::Aligned`] storage from the receiving worker's
/// pool so the decoded page can be persisted without a copy.
pub(crate) enum WireBuffer {
    Owned(flow_control::ChargedBuffer<AdmissionPolicy>),
    Aligned(crate::memory::CiphertextBuffer),
}
impl WireBuffer {
    /// Aligned page-body staging charged to a Ciphertext `reservation` that
    /// covers [`crate::memory::BufferPool::aligned_capacity`] of `length`.
    pub(crate) fn aligned(
        buffers: &crate::memory::BufferPool,
        reservation: flow_control::Charge<AdmissionPolicy>,
        length: usize,
    ) -> Result<Self> {
        if length > crate::model::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        buffers
            .aligned_ciphertext(reservation, length)
            .map(Self::Aligned)
    }
    pub(crate) fn new(
        admission: &flow_control::Quotas<AdmissionPolicy>,
        length: usize,
    ) -> Result<Self> {
        if length > crate::model::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        let reservation = admission.reserve(None, ResourceClass::Ciphertext, length)?;
        Ok(Self::Owned(flow_control::ChargedBuffer::unpooled(
            reservation,
            length,
        )?))
    }
    #[cfg(test)]
    pub(crate) fn reserved(
        reservation: flow_control::Charge<AdmissionPolicy>,
        length: usize,
    ) -> Result<Self> {
        reservation.validate(ResourceClass::Ciphertext, length)?;
        if length > crate::model::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        Ok(Self::Owned(flow_control::ChargedBuffer::unpooled(
            reservation,
            length,
        )?))
    }
    /// Hand the completed receive to the response decoder with its charge.
    pub(crate) fn into_body(self) -> super::protocol::ResponseBody {
        match self {
            Self::Owned(buffer) => {
                let (bytes, reservation) = buffer.into_parts();
                super::protocol::ResponseBody::Owned {
                    bytes: bytes.into_vec(),
                    reservation: Some(reservation),
                }
            }
            Self::Aligned(buffer) => super::protocol::ResponseBody::Aligned(buffer),
        }
    }
    /// Split vector staging; aligned staging has no `Vec` form.
    #[cfg(test)]
    pub(crate) fn into_parts(self) -> (Vec<u8>, flow_control::Charge<AdmissionPolicy>) {
        match self {
            Self::Owned(buffer) => {
                let (bytes, reservation) = buffer.into_parts();
                (bytes.into_vec(), reservation)
            }
            Self::Aligned(_) => panic!("aligned wire buffer"),
        }
    }
}
// SAFETY: both variants own stable, exclusive backing and their complete
// reservation until dropped.
unsafe impl IoBuffer for WireBuffer {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        match self {
            Self::Owned(buffer) => Ok(buffer.bytes()),
            Self::Aligned(buffer) => buffer.bytes(),
        }
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        match self {
            Self::Owned(buffer) => Ok(buffer.bytes_mut()),
            Self::Aligned(buffer) => buffer.bytes_mut(),
        }
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

/// A peer page whose ciphertext a NIC wrote into a GPU landing buffer, with
/// its signed response envelope. The requester verifies the signatures
/// against its outstanding binding, as for any response, and the GPU checks
/// the tag. There are no host bytes: this never reaches memory, disk, or
/// another reader.
pub struct LandedResponse {
    pub authentication: crate::peer::forwarding::ForwardedHead,
    pub metadata: crate::model::ObjectMetadata,
    pub envelope: crate::model::PageEnvelope,
    pub landed: crate::gpu::LandedCiphertext,
}

/// Internal transport result: native delivery stays materialized; HTTP relay
/// delivery owns an unfinished connection and its exact opaque body framing.
#[allow(clippy::large_enum_variant)] // Keep completed delivery inline without another allocation.
pub enum RelayResponse {
    Complete(SignedResponse),

    /// A page in a GPU landing buffer. Only a requester that asked for
    /// landing gets this, and only that requester's sealed read uses it.
    Landed(Box<LandedResponse>),

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
    pub(super) wire: (Rc<flow_control::Quotas<AdmissionPolicy>>, Rc<SecurityCodec>),
    pub(super) links: Option<Rc<crate::rdma::Links>>,
}
impl Transfers {
    /// Authentication and charged decoding are mandatory, even for HTTP-only peers.
    ///
    /// ```compile_fail
    /// use racer::{http::{HttpIo, HttpPool}, peer::transport::Transfers};
    /// use std::rc::Rc;
    /// fn unsigned(pool: Rc<HttpPool>, io: Rc<HttpIo>) {
    ///     let _ = Transfers::new(pool, io, None);
    /// }
    /// ```
    pub fn new(
        http: Rc<HttpPool>,
        io: Rc<HttpIo>,
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
            wire: (admission, codec),
            links: None,
        }
    }
    /// Use persistent RDMA links for pages and link setup.
    pub fn with_links(mut self, links: Rc<crate::rdma::Links>) -> Self {
        self.links = Some(links);
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
        // Metadata and the bodyless page modes (offer, ping) never receive a
        // body, so they bypass the body gate. A ping queued behind local
        // congestion would time out and count as silence from a healthy peer.
        use super::protocol::{FetchMode, Operation};
        if matches!(
            request.operation,
            Operation::Metadata { .. }
                | Operation::Page {
                    mode: FetchMode::Ping | FetchMode::Offer,
                    ..
                }
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
        landing: Option<i32>,
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
                            relay: crate::http::RelayResources::admitted(relay.clone()),
                            exchange: crate::http::PeerExchange::new(
                                peer_admission.clone(),
                                receive_permit,
                                Some(failure.clone()),
                            ),
                            ..Default::default()
                        },
                        scope,
                    )
                    .await,
            )?;
            connection.state_mut().exchange.admission = peer_admission.clone();
            if let Some(timing) = timing.as_deref_mut() {
                timing.end(0);
            }
            connection.state_mut().relay.reservation = relay.clone();
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
            let granting = now();
            let native = if membership.is_some() {
                self.accept_native(&request, plan, landing, scope).await?
            } else {
                None
            };
            if native.is_some() {
                self.record_rdma_stage(Event::RdmaGrantCount, Event::RdmaGrantNs, granting);
            }
            if let TransportPlan::Rdma { rail } = plan
                && membership.is_some()
                && self
                    .links
                    .as_ref()
                    .is_some_and(|links| links.ready(&peer, rail).is_none())
            {
                self.ensure_link(&endpoint, &peer, rail);
            }
            scope.candidate_progress()?;
            if let Some((_, accept, _, _)) = &native {
                attach(&mut head, accept)?;
            }
            if let Some(timing) = timing.as_deref_mut() {
                timing.begin();
            }
            let heading = now();
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
            if native.is_some() {
                self.record_rdma_stage(Event::RdmaHeadCount, Event::RdmaHeadNs, heading);
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
                let (binding, accept, peer, pending) = native.ok_or(Error::Unauthorized)?;
                let arriving = now();
                let received = self
                    .receive_native(
                        received.connection,
                        authentication,
                        binding,
                        accept,
                        peer,
                        control,
                        pending,
                        membership.as_ref().ok_or(Error::IncompatibleMembership)?,
                        length,
                        scope,
                    )
                    .await;
                if received.is_ok() {
                    self.record_rdma_stage(Event::RdmaArrivalCount, Event::RdmaArrivalNs, arriving);
                }
                return received;
            }
            if relay.is_some() {
                received.connection.state_mut().relay.context = relay_context;
                return Ok(RelayResponse::Http {
                    authentication,
                    connection: Box::new(received.connection),
                    length,
                });
            }
            if let Some(timing) = timing.as_deref_mut() {
                timing.begin();
            }
            let (mut connection, body) = self
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
                codec.response_body(authentication, body, scope),
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
    ) -> Result<(ConnectionLease, super::protocol::ResponseBody)> {
        let (admission, codec) = &self.wire;
        let observer = admission.policy().observer();
        let body = if length == 0 {
            super::protocol::ResponseBody::Owned {
                bytes: Vec::new(),
                reservation: None,
            }
        } else {
            let cache = &request.request.origin.object.cache;
            let buffers = codec.buffers();
            // Charge the whole pooled size class of the aligned body.
            let capacity = buffers.aligned_capacity(length)?;
            let mut reservation = admission
                .reserve(Some(cache), ResourceClass::Ciphertext, capacity)
                .map_err(Error::from);
            if matches!(reservation, Err(Error::Overloaded))
                && let Some(reclaim) = &self.reclaim
            {
                reclaim(cache, capacity);
                reservation = admission
                    .reserve(Some(cache), ResourceClass::Ciphertext, capacity)
                    .map_err(Error::from);
            }
            let mut buffer = WireBuffer::aligned(
                buffers,
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
            buffer.into_body()
        };
        Ok((connection, body))
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
        http1::relay::finish(&mut source, &mut destination)?;
        destination.state_mut().relay.reservation = None;
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
    let mut state = state
        .run(
            io,
            scope,
            || {
                let mut tick = scope.clone();
                tick.deadline.0 = tick
                    .deadline
                    .0
                    .min(uring_runtime::environment::now() + Duration::from_millis(10));
                tick
            },
            |error| error == Error::DeadlineExceeded,
        )
        .await?;
    let (source, destination) = state.connections_mut();
    http1::relay::finish(source, destination)?;
    destination.state_mut().relay.reservation = None;
    Ok(state.into_destination())
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
            let io = Rc::new(crate::http::new_io(
                reactor.clone(),
                Codec::new(p::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
            ));
            Transfers::new(
                Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 2)),
                io,
                admission.clone(),
                Rc::new(p::SecurityCodec::new(
                    admission.clone(),
                    BufferPool::new(admission.clone()),
                )),
                signatures.clone(),
            )
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
        /// Outcome of one `RdmaLink` setup over a real socket pair.
        struct LinkSetupCase {
            /// Initiator signer index (0 or 2); the other is the responder.
            initiator: usize,
            /// Responder has RDMA links.
            responder_links: bool,
            /// Responder starts its own setup first (simultaneous offers).
            responder_opens: bool,
            sites: [&'static str; 2],
        }

        fn link_setup(
            case: LinkSetupCase,
        ) -> (
            crate::rdma::links::tests::World,
            Result<()>,
            Result<()>,
            NodeId,
            NodeId,
        ) {
            use crate::rdma::links::tests::world_for;
            let signers = crate::peer::tests::signers();
            let membership = test_membership(&signers, &[0, 2], &case.sites);
            let responder = 2 - case.initiator;
            let mut w = world_for(
                [
                    signers[case.initiator].node().clone(),
                    signers[responder].node().clone(),
                ],
                false,
            );
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let initiator = transfers(signers[case.initiator].clone(), &admission, &reactor)
                .with_links(w.a.links.clone());
            let mut server = transfers(signers[responder].clone(), &admission, &reactor);
            if case.responder_links {
                server = server.with_links(w.b.links.clone());
            }
            let scope =
                RequestScope::new(RequestId([5; 16]), Instant::now() + Duration::from_secs(10))
                    .unwrap();
            let (peer, local) = (
                signers[responder].node().clone(),
                signers[case.initiator].node().clone(),
            );
            if case.responder_opens {
                let mut open = std::pin::pin!(w.b.links.open(&local, RailId(7), &scope));
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                loop {
                    if let Poll::Ready(result) = open.as_mut().poll(&mut cx) {
                        result.unwrap();
                        break;
                    }
                    w.a.owner.poll_budgeted(8).unwrap();
                    w.b.owner.poll_budgeted(8).unwrap();
                }
            }
            let (a, b) = UnixStream::pair().unwrap();
            let a = crate::http::from_accepted(a.into(), &admission).unwrap();
            let b = crate::http::from_accepted(b.into(), &admission).unwrap();
            let (client_result, serve_result) = {
                let client = async {
                    let a = p::connect(
                        &initiator.io,
                        a,
                        signers[case.initiator].clone(),
                        &peer,
                        &scope,
                    )
                    .await?;
                    link_exchange(
                        &initiator.io,
                        &signers[case.initiator],
                        initiator.links.as_ref().unwrap(),
                        a,
                        &peer,
                        RailId(7),
                        &scope,
                    )
                    .await
                    .map(drop)
                };
                let serve = async {
                    let b = p::accept(&server.io, b, signers[responder].clone(), &scope).await?;
                    let received = server.io.receive_head(b, &scope).await?;
                    let (auth, length) = decode_envelope(received.value, false)?;
                    assert_eq!(length, 0);
                    server
                        .serve_link(
                            received.connection,
                            auth.original,
                            Ok(membership.clone()),
                            &scope,
                        )
                        .await
                        .map(drop)
                };
                let mut client = std::pin::pin!(client);
                let mut serve = std::pin::pin!(serve);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                let (mut client_result, mut serve_result) = (None, None);
                for _ in 0..10_000 {
                    if client_result.is_none()
                        && let Poll::Ready(result) = client.as_mut().poll(&mut cx)
                    {
                        client_result = Some(result);
                    }
                    if serve_result.is_none()
                        && let Poll::Ready(result) = serve.as_mut().poll(&mut cx)
                    {
                        serve_result = Some(result);
                    }
                    if client_result.is_some() && serve_result.is_some() {
                        break;
                    }
                    reactor.poll_budgeted(128).unwrap();
                    reactor.wait(Duration::from_millis(1)).unwrap();
                    w.a.owner.poll_budgeted(16).unwrap();
                    w.b.owner.poll_budgeted(16).unwrap();
                }
                (client_result, serve_result)
            };
            for _ in 0..8 {
                w.a.owner.poll_budgeted(16).unwrap();
                w.b.owner.poll_budgeted(16).unwrap();
            }
            (
                w,
                client_result.expect("initiator finished"),
                serve_result.expect("responder finished"),
                local,
                peer,
            )
        }

        #[test]
        fn signed_link_setup_connects_both_sides() {
            for initiator in [0, 2] {
                let (w, client, serve, local, peer) = link_setup(LinkSetupCase {
                    initiator,
                    responder_links: true,
                    responder_opens: false,
                    sites: ["site1", "site1"],
                });
                client.unwrap();
                serve.unwrap();
                let (_, ours) = w.a.links.ready(&peer, RailId(7)).expect("initiator ready");
                let (_, theirs) = w.b.links.ready(&local, RailId(7)).expect("responder ready");
                assert_eq!(ours, theirs);
            }
        }

        #[test]
        fn signed_link_setup_tie_break_rejects_the_higher_offer() {
            let signers = crate::peer::tests::signers();
            // The initiator has the higher NodeId and the responder is also
            // initiating: the responder answers 409 and keeps its own offer.
            let initiator =
                if crate::rdma::link_setup::tie_break(signers[0].node(), signers[2].node())
                    == crate::rdma::link_setup::TieBreak::AcceptPeer
                {
                    0
                } else {
                    2
                };
            let (w, client, serve, local, peer) = link_setup(LinkSetupCase {
                initiator,
                responder_links: true,
                responder_opens: true,
                sites: ["site1", "site1"],
            });
            assert_eq!(client, Err(Error::Unavailable));
            serve.unwrap();
            assert!(w.a.links.ready(&peer, RailId(7)).is_none());
            assert!(matches!(
                w.a.links.state(&peer, RailId(7)),
                rdma_verbs::LinkState::Backoff(_) | rdma_verbs::LinkState::Breaking
            ));
            assert!(matches!(
                w.b.links.state(&local, RailId(7)),
                rdma_verbs::LinkState::Connecting(_)
            ));
            // The lower node's offer is accepted by the same rule.
            assert_eq!(
                w.a.links.admit_setup(&peer, RailId(7)),
                crate::rdma::SetupAdmission::Accept
            );
        }

        #[test]
        fn signed_link_setup_declines_without_rdma_or_across_sites() {
            let (w, client, serve, _, peer) = link_setup(LinkSetupCase {
                initiator: 0,
                responder_links: false,
                responder_opens: false,
                sites: ["site1", "site1"],
            });
            assert_eq!(client, Err(Error::Unavailable));
            serve.unwrap();
            assert!(w.a.links.ready(&peer, RailId(7)).is_none());
            for sites in [["site1", "site2"], ["", ""]] {
                let (w, client, serve, local, peer) = link_setup(LinkSetupCase {
                    initiator: 0,
                    responder_links: true,
                    responder_opens: false,
                    sites,
                });
                assert!(client.is_err());
                assert_eq!(serve, Err(Error::Unauthorized));
                assert!(w.a.links.ready(&peer, RailId(7)).is_none());
                assert_eq!(
                    w.b.links.state(&local, RailId(7)),
                    rdma_verbs::LinkState::Idle,
                    "rejected offers never reach the owner"
                );
            }
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
        /// One page over a ready link, driven as the peer server and requester do.
        #[derive(Clone, Copy, Default)]
        struct PageCase {
            reverse: bool,
            relayed: bool,
            /// The sender's membership puts the sender in this Site.
            sender_site: Option<&'static str>,
            /// The receiver's membership puts the sender in this Site.
            receiver_site: Option<&'static str>,
            /// The sender's link breaks before it writes.
            sender_down: bool,
            /// The receiver grants a slot of a simulated GPU's landing buffer.
            landing: bool,
        }

        struct PageOutcome {
            /// Received page bytes; the response is dropped before `free`.
            result: Result<Vec<u8>>,
            /// The received ciphertext was still in registered memory.
            registered: bool,
            /// The page landed in the GPU landing buffer.
            landed: bool,
            /// Free buffers while the response was still held.
            held: (Option<usize>, Option<usize>),
            sent: Result<bool>,
            completed: usize,
            fallbacks: usize,
            completions: usize,
            released: bool,
            free: (Option<usize>, Option<usize>),
        }

        fn page_membership(
            signers: &[Rc<Signatures>],
            sender_site: Option<&str>,
        ) -> Arc<crate::topology::Membership> {
            use crate::topology::Member;
            use crate::topology::Membership;
            Arc::new(
                Membership::validate(
                    MembershipVersion(1),
                    [0, 1, 2]
                        .into_iter()
                        .map(|i| Member {
                            node: signers[i].node().clone(),
                            shares: std::num::NonZeroU32::new(1).unwrap(),
                            peer_endpoint: format!("127.0.0.1:{}", 9000 + i),
                            rails: vec![racer_control_wire::RailMapping {
                                rail: RailId(7),
                                device: "test-provider".into(),
                                port: 1,
                                gid: None,
                                numa_node: None,
                            }],
                            site: match i {
                                1 => "site2",
                                2 => sender_site.unwrap_or("site1"),
                                _ => "site1",
                            }
                            .into(),
                        })
                        .collect(),
                )
                .unwrap(),
            )
        }

        fn page_roundtrip(case: PageCase) -> PageOutcome {
            use crate::rdma::links::tests::link_up;
            use crate::rdma::links::tests::world_for;
            let mut signers = crate::peer::tests::signers();
            if case.reverse {
                signers.swap(0, 2);
            }
            let mut w = world_for(
                [signers[0].node().clone(), signers[2].node().clone()],
                false,
            );
            link_up(&mut w);
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(true).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let receiver =
                transfers(signers[0].clone(), &admission, &reactor).with_links(w.a.links.clone());
            let sender =
                transfers(signers[2].clone(), &admission, &reactor).with_links(w.b.links.clone());
            let scope =
                RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(30))
                    .unwrap();
            let cache = CacheId("cccccccc-1111-4111-8111-111111111111".into());
            let version = ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: CacheKey([9; 32]),
                },
                etag: StrongEtag::parse(b"\"provider\"").unwrap(),
            };
            let envelope = PageEnvelope {
                cipher: crate::model::PageCipher::Xaes256Gcm,
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
            let path = if case.relayed {
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
            let original = Arc::new(
                signers[if case.relayed { 1 } else { 2 }]
                    .sign(head)
                    .unwrap(),
            );
            let mut hops = vec![];
            if case.relayed {
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
            let sender_membership = page_membership(&signers, case.sender_site);
            let receiver_membership = page_membership(&signers, case.receiver_site);
            let gpu = case.landing.then(|| {
                let helper = crate::gpu::testing::helper(&[None]);
                let bytes = (crate::rdma::BUFFER_LEN as u64)
                    .next_multiple_of(crate::config::GPU_STAGING_UNIT);
                crate::rdma::links::tests::attach_landing(&mut w, &helper, bytes);
                assert!(w.a.links.has_landing(RailId(7), 0));
                helper
            });
            // The receiver grants in-band and names the grant in its signed Accept.
            let links = receiver.links.as_ref().unwrap();
            let pending = crate::rdma::links::tests::drive(&mut w.a.owner, &mut w.b.owner, async {
                if case.landing {
                    links
                        .grant_landing(signers[2].node(), RailId(7), binding.transfer, 0, &scope)
                        .await
                } else {
                    links
                        .grant(signers[2].node(), RailId(7), binding.transfer, &scope)
                        .await
                }
            })
            .unwrap();
            let accept = binding
                .sign(
                    &signers[0],
                    signers[2].node(),
                    Phase::Accept,
                    &[0; 32],
                    0,
                    vec![extension(GRANT_HEADER, pending.header_value())],
                )
                .unwrap();
            let accept_wire = p::encode_signed(&accept).unwrap();
            if case.sender_down {
                let links = sender.links.as_ref().unwrap();
                let (epoch, _) = links.ready(signers[0].node(), RailId(7)).unwrap();
                links.abandon(signers[0].node(), RailId(7), epoch);
                crate::rdma::links::tests::pump(&mut w.a.owner, &mut w.b.owner);
            }
            let released = std::cell::Cell::new(false);
            let (a, b) = UnixStream::pair().unwrap();
            let a = crate::http::from_accepted(a.into(), &admission).unwrap();
            let b = crate::http::from_accepted(b.into(), &admission).unwrap();
            let (result, sent) = {
                let receive = async {
                    let a = p::connect(
                        &receiver.io,
                        a,
                        signers[0].clone(),
                        signers[2].node(),
                        &scope,
                    )
                    .await?;
                    let mut initial = MessageHead {
                        start: StartLine::Request {
                            method: "POST".into(),
                            target: "/test".into(),
                        },
                        headers: vec![extension("content-length", b"0".to_vec())],
                    };
                    attach(&mut initial, &p::decode_signed(&accept_wire)?)?;
                    let sent = receiver.io.send_head(a, initial, &scope).await?;
                    let mut received = receiver.io.receive_head(sent.connection, &scope).await?;
                    let control = detach(&mut received.value)?;
                    let (auth, length) = decode_envelope(received.value, true)?;
                    let Some(control) = control else {
                        // Plain HTTP: the sender released the grant.
                        let arrival = std::future::poll_fn(|cx| pending.poll_arrival(cx)).await;
                        released.set(matches!(arrival, Ok(rdma_verbs::Arrival::Released)));
                        let (mut connection, body) = receiver
                            .read_ciphertext(received.connection, None, length, &scope)
                            .await?;
                        connection.finish_exchange()?;
                        return receiver
                            .wire
                            .1
                            .response(auth, body.bytes()?.to_vec(), &scope)
                            .map(RelayResponse::Complete);
                    };
                    receiver
                        .receive_native(
                            received.connection,
                            auth,
                            binding.clone(),
                            p::decode_signed(&accept_wire)?,
                            signers[2].node().clone(),
                            control,
                            pending,
                            &receiver_membership,
                            length,
                            &scope,
                        )
                        .await
                };
                let send = async {
                    let b = p::accept(&sender.io, b, signers[2].clone(), &scope).await?;
                    let mut received = sender.io.receive_head(b, &scope).await?;
                    let control = detach(&mut received.value)?.ok_or(Error::Unauthorized)?;
                    let (verified, _) = binding.verify(
                        &signers[2],
                        signers[0].node(),
                        control,
                        &[Phase::Accept],
                        &[0; 32],
                        0,
                        &scope,
                    )?;
                    let grant = AuthenticatedGrant::from_verified(
                        &verified,
                        binding.rail,
                        binding.transfer,
                    )?;
                    let (mut conn, sent) = sender
                        .send_native(
                            received.connection,
                            &response,
                            (binding.clone(), verified, grant),
                            &sender_membership,
                            &scope,
                        )
                        .await?;
                    if !sent {
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
                    }
                    assert_eq!(conn.send_remaining(), Some(0));
                    conn.finish_exchange()?;
                    Ok::<bool, Error>(sent)
                };
                let mut exchange = std::pin::pin!(async { futures::join!(receive, send) });
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                loop {
                    if let Poll::Ready(result) =
                        std::future::Future::poll(exchange.as_mut(), &mut cx)
                    {
                        break result;
                    }
                    w.a.owner.poll_budgeted(16).unwrap();
                    w.b.owner.poll_budgeted(16).unwrap();
                    reactor.poll_budgeted(128).unwrap();
                    reactor.wait(Duration::from_millis(1)).unwrap();
                }
            };
            crate::rdma::links::tests::pump(&mut w.a.owner, &mut w.b.owner);
            let free = |w: &crate::rdma::links::tests::World| {
                (
                    w.a.links.buffers(RailId(7), rdma_verbs::BufferState::Free),
                    w.b.links.buffers(RailId(7), rdma_verbs::BufferState::Free),
                )
            };
            let held = free(&w);
            let registered = matches!(
                &result,
                Ok(RelayResponse::Complete(SignedResponse {
                    response: PeerResponse::Page { ciphertext, .. },
                    ..
                })) if ciphertext.is_registered()
            );
            let landed = matches!(&result, Ok(RelayResponse::Landed(_)));
            let result = result.map(|response| match response {
                RelayResponse::Complete(SignedResponse {
                    response: PeerResponse::Page { ciphertext, .. },
                    ..
                }) => ciphertext.bytes().to_vec(),
                RelayResponse::Landed(landed) => {
                    // Read what the NIC wrote, through the landing dma-buf.
                    use std::os::unix::fs::FileExt;
                    assert_eq!(landed.envelope.ciphertext_length, 144);
                    let buffer = w.a.links.node().landing(RailId(7), 0).unwrap();
                    let file = std::fs::File::from(
                        rdma_verbs::DmabufMemory::fd(&*buffer)
                            .try_clone_to_owned()
                            .unwrap(),
                    );
                    let mut bytes = vec![0; landed.landed.len() as usize];
                    file.read_exact_at(&mut bytes, landed.landed.offset())
                        .unwrap();
                    bytes
                }
                _ => panic!("expected page"),
            });
            drop(gpu);
            crate::rdma::links::tests::pump(&mut w.a.owner, &mut w.b.owner);
            PageOutcome {
                result,
                registered,
                landed,
                held,
                sent,
                completed: sender.native_completed.get(),
                fallbacks: sender.native_fallbacks.get(),
                completions: receiver.native_completions.get(),
                released: released.get(),
                free: free(&w),
            }
        }

        fn page_bytes(result: &Result<Vec<u8>>) -> Vec<u8> {
            result.as_ref().expect("page").clone()
        }

        #[test]
        fn simulated_link_grant_in_request_and_complete_both_directions() {
            for reverse in [false, true] {
                for relayed in [false, true] {
                    let outcome = page_roundtrip(PageCase {
                        reverse,
                        relayed,
                        ..PageCase::default()
                    });
                    assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
                    assert_eq!(outcome.sent, Ok(true));
                    assert_eq!(
                        (outcome.completed, outcome.fallbacks, outcome.completions),
                        (1, 0, 1),
                        "HTTP fallback cannot pass a native success test"
                    );
                    // The response held the receiver's buffer without a copy;
                    // dropping it returned the buffer.
                    assert!(outcome.registered);
                    // `w.a` is always the receiving node.
                    assert_eq!(outcome.held, (Some(1), Some(2)));
                    assert_eq!(outcome.free, (Some(2), Some(2)));
                }
            }
        }

        #[test]
        /// A landing grant over a ready link: the sender writes into the
        /// receiver's GPU landing buffer, the receiver checks the signed head
        /// for exactly this page and returns it as landed, and no host buffer
        /// is used on the receiver.
        fn simulated_landing_grant_lands_the_page_in_gpu_memory() {
            for reverse in [false, true] {
                let outcome = page_roundtrip(PageCase {
                    reverse,
                    landing: true,
                    ..PageCase::default()
                });
                assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
                assert!(outcome.landed && !outcome.registered);
                assert_eq!(outcome.sent, Ok(true));
                assert_eq!(
                    (outcome.completed, outcome.fallbacks, outcome.completions),
                    (1, 0, 1)
                );
                // No host receive buffer was used.
                assert_eq!(outcome.held, (Some(2), Some(2)));
                assert_eq!(outcome.free, (Some(2), Some(2)));
            }
        }

        #[test]
        /// The sender may still answer a landing grant over HTTP (here its
        /// link is down): the page arrives in host memory as usual.
        fn simulated_landing_grant_falls_back_to_http() {
            let outcome = page_roundtrip(PageCase {
                landing: true,
                sender_down: true,
                ..PageCase::default()
            });
            assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
            assert!(!outcome.landed);
            assert_eq!(
                (outcome.completed, outcome.fallbacks, outcome.completions),
                (0, 1, 0)
            );
        }

        #[test]
        fn simulated_sender_rejects_cross_site_and_releases_the_grant() {
            for site in ["site2", ""] {
                for (reverse, relayed) in [(false, false), (true, true)] {
                    let outcome = page_roundtrip(PageCase {
                        reverse,
                        relayed,
                        sender_site: Some(site),
                        ..PageCase::default()
                    });
                    assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
                    assert_eq!(outcome.sent, Ok(false));
                    assert!(outcome.released, "the receiver's grant is released");
                    assert_eq!(
                        (outcome.completed, outcome.fallbacks, outcome.completions),
                        (0, 0, 0)
                    );
                    assert_eq!(outcome.free, (Some(2), Some(2)));
                }
            }
        }

        #[test]
        fn simulated_sender_link_down_falls_back_to_signed_http_body() {
            let outcome = page_roundtrip(PageCase {
                sender_down: true,
                ..PageCase::default()
            });
            assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
            assert_eq!(outcome.sent, Ok(true));
            assert_eq!(
                (outcome.completed, outcome.fallbacks, outcome.completions),
                (0, 1, 0)
            );
        }

        #[test]
        fn simulated_receiver_rejects_complete_from_cross_site_hop() {
            for site in ["site2", ""] {
                let outcome = page_roundtrip(PageCase {
                    receiver_site: Some(site),
                    ..PageCase::default()
                });
                assert!(matches!(outcome.result, Err(Error::Unauthorized)));
                assert_eq!(outcome.sent, Ok(true));
                assert_eq!(outcome.completions, 0);
                // The dropped arrival still returns its buffer.
                assert_eq!(outcome.free, (Some(2), Some(2)));
            }
        }

        #[test]
        fn completion_must_name_the_grant_and_a_lost_arrival_is_io() {
            use crate::rdma::links::tests::drive;
            use crate::rdma::links::tests::link_up;
            use crate::rdma::links::tests::page;
            use crate::rdma::links::tests::world_for;
            use base64::Engine;
            let signers = crate::peer::tests::signers();
            let (a, b) = (signers[0].node().clone(), signers[2].node().clone());
            let mut w = world_for([a.clone(), b.clone()], false);
            let scope =
                RequestScope::new(RequestId([8; 16]), Instant::now() + Duration::from_secs(30))
                    .unwrap();
            // Before setup, the receiver cannot grant and the page uses HTTP.
            let early = drive(
                &mut w.a.owner,
                &mut w.b.owner,
                w.a.links.grant(&b, RailId(7), TransferId([6; 16]), &scope),
            );
            assert!(matches!(early, Err(Error::Unavailable)));
            assert!(native_failure(Error::Unavailable, &scope));
            link_up(&mut w);
            let binding = Binding {
                request: [4; 32],
                response: [5; 32],
                transfer: TransferId([6; 16]),
                membership: 1,
                deadline: p::encode_deadline(scope.deadline).unwrap(),
                rail: RailId(7),
            };
            let admission = w.a.admission.clone();
            let envelope = page(&admission, 35, 1).envelope().clone();
            for mutation in 0..5 {
                let pending = drive(
                    &mut w.a.owner,
                    &mut w.b.owner,
                    w.a.links.grant(&b, RailId(7), binding.transfer, &scope),
                )
                .unwrap();
                let mut transfer = binding.transfer;
                let (mut link, mut tag, mut length) = (pending.link(), pending.tag(), 35);
                match mutation {
                    0 => link ^= 1,
                    1 => tag ^= 1,
                    2 => length += 1,
                    3 => transfer.0[0] ^= 1,
                    _ => {}
                }
                let value = crate::rdma::links::completion_value(link, tag, length, transfer);
                let complete = binding
                    .sign(
                        &signers[2],
                        &a,
                        Phase::Complete,
                        &[9; 32],
                        0,
                        vec![extension(
                            COMPLETION_HEADER,
                            base64::engine::general_purpose::STANDARD
                                .encode(value)
                                .into_bytes(),
                        )],
                    )
                    .unwrap();
                let (verified, _) = binding
                    .verify(
                        &signers[0],
                        &b,
                        complete,
                        &[Phase::Complete],
                        &[9; 32],
                        0,
                        &scope,
                    )
                    .unwrap();
                let grant = crate::rdma::AuthenticatedGrant {
                    link: pending.link(),
                    tag: pending.tag(),
                    length: crate::rdma::MAX_CIPHERTEXT as u32,
                    rail: RailId(7),
                    transfer: binding.transfer,
                };
                if mutation < 4 {
                    let result = drive(
                        &mut w.a.owner,
                        &mut w.b.owner,
                        pending.finish(&verified, envelope.clone(), &admission, &scope),
                    );
                    assert!(matches!(result, Err(Error::Unauthorized)), "{mutation}");
                    w.b.links.release(&a, &grant);
                } else {
                    // The link breaks after the sender's completion: the page is
                    // lost and the request fails with `Io`, never a fallback.
                    let (epoch, _) = w.a.links.ready(&b, RailId(7)).unwrap();
                    w.a.links.abandon(&b, RailId(7), epoch);
                    let result = drive(
                        &mut w.a.owner,
                        &mut w.b.owner,
                        pending.finish(&verified, envelope.clone(), &admission, &scope),
                    );
                    assert!(matches!(result, Err(Error::Io)));
                }
            }
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
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
    /// Pre-reserved staging retains surplus credit and permits an empty view.
    #[test]
    fn wire_reserved_surplus_empty_and_abandoned_backing_keep_accounting() {
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        for length in [0, 3, 1 << 20] {
            let amount = 2 << 20;
            let buffer = WireBuffer::reserved(
                admission
                    .reserve(None, ResourceClass::Ciphertext, amount)
                    .unwrap(),
                length,
            )
            .unwrap();
            assert_eq!(buffer.bytes().unwrap().len(), length);
            assert_eq!(admission.used(ResourceClass::Ciphertext), amount);
            drop(buffer);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            assert_eq!(admission.retained_buffer_bytes(), 0);
        }
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
            for phase in [Phase::Accept, Phase::Complete, Phase::Finish] {
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
                            &[if phase == Phase::Finish {
                                Phase::Complete
                            } else {
                                Phase::Finish
                            }],
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
                        Phase::Finish,
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
                    Phase::Complete
                } else {
                    Phase::Finish
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
            let mut head = original.head(Phase::Finish, &[9; 32], 0, vec![]).unwrap();
            p::push(&mut head, "racer-receiver", &nodes[1].node().0);
            p::push(&mut head, "racer-extra", 1);
            assert!(
                original
                    .verify(
                        &nodes[1],
                        nodes[0].node(),
                        nodes[0].sign(head).unwrap(),
                        &[Phase::Finish],
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
                        &[Phase::Finish],
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
                (Phase::Accept, 0, vec![]),
                (
                    Phase::Complete,
                    0,
                    vec![extension("racer-rdma-completion", b"bad".to_vec())],
                ),
                (
                    Phase::Complete,
                    17,
                    vec![extension(
                        "racer-rdma-complete",
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
