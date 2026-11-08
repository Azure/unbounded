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
use racer_io::ring::IoBuffer;
#[cfg(test)]
use racer_wire::control::CacheId;
#[cfg(test)]
use racer_wire::control::MembershipVersion;
use racer_wire::control::NodeId;
use racer_wire::control::RailId;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;

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
        clock: &racer_io::Clock,
        phase: Phase,
        previous: &[u8; 32],
        length: usize,
        extensions: Vec<Header>,
    ) -> Result<MessageHead> {
        self.wire()
            .head(clock, phase, previous, length, extensions)
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
        rng: &racer_io::Rng,
    ) -> Result<Self> {
        let mut transfer = [0; 16];
        rng.fill(&mut transfer).map_err(|_| Error::Unavailable)?;
        Ok(Self {
            request: envelope_digest(auth)?,
            response: [0; 32],
            transfer: TransferId(transfer),
            membership,
            deadline: p::encode_deadline(scope.clock(), scope.deadline)?,
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
/// Remove the control proof and resolve its chain from the carrying session.
pub(super) fn detach(
    head: &mut MessageHead,
    chains: &dyn peer_wire::Chains,
) -> Result<Option<SignedHead>> {
    let value = head.unique(HEADER)?.map(|v| v.to_vec());
    head.headers
        .retain(|h| !h.name.eq_ignore_ascii_case(HEADER));
    value
        .map(|v| {
            let mut signed = super::protocol::decode_signed(&v)?;
            peer_wire::resolve(&mut signed, chains)?;
            Ok(signed)
        })
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
fn unframe(
    head: MessageHead,
    response: bool,
    chains: &dyn peer_wire::Chains,
) -> Result<SignedHead> {
    let (auth, len) = decode_envelope(head, response, chains)?;
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
        Error::Unavailable | Error::Io | Error::Os(_) | Error::Overloaded | Error::FlightFull
    )
}
/// Longest a peer body already on the wire waits for this worker's ciphertext budget
/// ([`Transfers::reserve_ciphertext_waiting`]).
pub(crate) const CIPHERTEXT_WAIT: Duration = Duration::from_millis(250);

fn native_scope(scope: &RequestScope) -> RequestScope {
    native_scope_within(scope, Duration::from_secs(5))
}
/// `scope`, ending no later than `limit` from now.
fn native_scope_within(scope: &RequestScope, limit: Duration) -> RequestScope {
    let mut bounded = scope.clone();
    bounded.deadline.0 = bounded.deadline.0.min(scope.clock().now() + limit);
    bounded
}
/// `scope` bounded by the RDMA page timeout of `links`, for one page's grant
/// or write: past it the page goes over HTTP and the breaker counts a soft
/// failure.
fn page_scope(links: &crate::rdma::Links, scope: &RequestScope) -> RequestScope {
    native_scope_within(scope, links.page_timeout())
}
/// Feed the breaker a failed RDMA grant, write, or arrival whose page goes
/// over HTTP instead. A page timeout is soft, running out of grant slots,
/// flight room, or ciphertext budget is load, and any other error means the
/// link is broken.
fn trip_native(links: &crate::rdma::Links, peer: &NodeId, rail: RailId, error: Error, send: bool) {
    links.breaker_trip(peer, rail, native_reason(links, error, send));
}
/// The breaker reason for a failed RDMA grant (`send` false) or write or
/// arrival (`send` true), counting a page timeout.
fn native_reason(
    links: &crate::rdma::Links,
    error: Error,
    send: bool,
) -> crate::rdma::breaker::TripReason {
    use crate::rdma::breaker::TripReason;
    match error {
        Error::DeadlineExceeded => {
            links.record_page_timeout();
            TripReason::Timeout
        }
        Error::Overloaded | Error::FlightFull => TripReason::Load,
        _ if send => TripReason::Send,
        _ => TripReason::Grant,
    }
}
/// NUMA-`local` rails other than the chosen `rail` that a receiver may grant a
/// page on when `rail` has no free slot, in order, keeping only those
/// `eligible` (a ready link that both ends publish).
fn spare_rails(rail: RailId, local: &[RailId], eligible: impl Fn(RailId) -> bool) -> Vec<RailId> {
    let mut rails = Vec::new();
    for &r in local {
        if r != rail && !rails.contains(&r) && eligible(r) {
            rails.push(r);
        }
    }
    rails
}
/// What a receiver does when its grant for a hop fails with an error.
#[derive(Debug, PartialEq, Eq)]
enum GrantFailure {
    /// Fail the page request `Overloaded` so its range window retries it.
    Backpressure,
    /// Carry the body over HTTP instead.
    Fallback,
    /// Fail the request with the error.
    Fail,
}
/// Classify a failed grant. A page whose grant found every slot of every
/// eligible rail held (`Overloaded`) on a live request pushes back: grants are
/// held until the sender's page lands, so exhaustion means a slow sender, and
/// HTTP would read the same disk at a much higher receive cost. Other
/// recoverable failures (a link that broke, a lost device) and non-page bodies
/// fall back to HTTP as before.
fn grant_failure(error: Error, page: bool, scope: &RequestScope) -> GrantFailure {
    if error == Error::Overloaded && page && scope.check().is_ok() {
        GrantFailure::Backpressure
    } else if native_failure(error, scope) {
        GrantFailure::Fallback
    } else {
        GrantFailure::Fail
    }
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
            // Outgoing responses were signed here or verified once by the exchange
            // that returned them; only unverified ingress needs signature checks.
            let signer = if sending {
                crate::peer::protocol::node_field(&signed.head, "racer-signer")?
            } else {
                self.signatures.verify_historical(signed)?.node().clone()
            };
            if signer != path[index]
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
        let serving = scope.clock().now();
        let peer = accept.peer.node();
        let (PeerResponse::Page { ciphertext, .. } | PeerResponse::Selected { ciphertext, .. }) =
            &response.response
        else {
            links.release(peer, &grant);
            links.record_send_declined(true);
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
                .min(crate::peer::protocol::decode_deadline(scope.clock(), binding.deadline)?.0);
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
        // The receiver chose the rail; it must be one both ends publish.
        if !crate::rdma::admits_rail(plan, membership, self.signatures.node(), peer, binding.rail) {
            links.release(peer, &grant);
            links.record_send_declined(false);
            return Ok((connection, false));
        }
        binding.response = envelope_digest(&response.authentication)?;
        let previous = signed_digest(&accept.signed)?;
        // An open breaker sends the body over HTTP at once instead of
        // writing over a link that recently failed.
        if links.breaker_admit(peer, binding.rail) == crate::rdma::breaker::Admit::Http {
            links.release(peer, &grant);
            links.record_breaker_http();
            return self
                .send_fallback(connection, response, &binding, peer, previous, scope)
                .await;
        }
        // `Links::send` writes the page in place, or stages a copy without ODP.
        // The write gets the page timeout, not the whole request: a write the
        // link cannot finish by then goes over HTTP in this same exchange. The
        // receiver's grant stays held until our release (or the link break)
        // reaches it, so a late write cannot land in a reused buffer.
        let sending = scope.clock().now();
        let sent = links
            .send(peer, &grant, ciphertext, &page_scope(links, scope))
            .await;
        let sent = match sent {
            Ok(sent) => {
                links.record_stage(Event::RdmaSendCount, Event::RdmaSendNs, sending);
                links.breaker_success(peer, binding.rail);
                sent
            }
            Err(error) => {
                links.release(peer, &grant);
                if !native_failure(error, scope) {
                    return Err(error);
                }
                trip_native(links, peer, binding.rail, error, true);
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
    /// Feed the breaker the outcome of a page the sender reported written:
    /// success closes a half-open breaker; a failed arrival while the request
    /// is alive trips it.
    fn native_outcome<T>(
        &self,
        peer: &NodeId,
        rail: RailId,
        outcome: &Result<T>,
        scope: &RequestScope,
    ) {
        let Some(links) = &self.links else {
            return;
        };
        match outcome {
            Ok(_) => links.breaker_success(peer, rail),
            Err(error) if native_failure(*error, scope) => {
                trip_native(links, peer, rail, *error, true);
            }
            Err(_) => {}
        }
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
        membership: &crate::topology::Membership,
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
        // Land the page on a rail whose NIC shares the NUMA node of the
        // thread that reads it when one is ready, so the NIC writes and the
        // reader copies node-local memory. That is this worker, or for a host
        // read the requesting worker (`reader_numa`), whose crypto helpers
        // decrypt the registered buffer. A GPU landing keeps the hashed rail,
        // whose landing buffers were placed for that GPU.
        let rail = if landing.is_none() {
            let local: Vec<_> = links
                .rails_on(&peer, scope.reader_numa.map(usize::from).or(links.numa()))
                .into_iter()
                .filter(|&r| {
                    crate::rdma::admits_rail(plan, membership, self.signatures.node(), &peer, r)
                })
                .collect();
            let page = super::rail_hint(&request.request.operation);
            crate::rdma::receiver_rail(page.as_ref(), rail, &local)
        } else {
            rail
        };
        if links.ready(&peer, rail).is_none() {
            // A link being torn down failed; the next one is probed before
            // pages trust it again.
            if matches!(links.state(&peer, rail), rdma_verbs::LinkState::Breaking) {
                links.breaker_trip(&peer, rail, crate::rdma::breaker::TripReason::LinkDown);
            }
            links.record_fallback_no_link();
            return Ok(None);
        }
        if links.breaker_admit(&peer, rail) == crate::rdma::breaker::Admit::Http {
            links.record_http_fallback();
            links.record_breaker_http();
            return Ok(None);
        }
        let mut binding = Binding::request(
            &request.authentication,
            request.request.route.membership.0,
            scope,
            rail,
            &self.io.reactor().ctx().rng,
        )?;
        let landed = match landing {
            Some(ordinal)
                if matches!(
                    request.request.operation,
                    crate::peer::protocol::Operation::Page { .. }
                ) && links.has_landing(rail, ordinal) =>
            {
                match links
                    .grant_landing(
                        &peer,
                        rail,
                        binding.transfer,
                        ordinal,
                        &page_scope(links, scope),
                    )
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
                // Take a free slot on the chosen rail, or else on another NUMA-local rail,
                // then park on the chosen rail until a slot frees (oldest request first).
                // Waiting costs nothing until a release wakes it; other NUMA nodes' rails
                // would land the page in remote memory, and when one rail is out of
                // buffers the rest usually are too.
                let spares = if landing.is_none() {
                    spare_rails(rail, &links.local_rails(&peer), |r| {
                        links.ready(&peer, r).is_some()
                            && !links.breaker_open(&peer, r)
                            && crate::rdma::admits_rail(
                                plan,
                                membership,
                                self.signatures.node(),
                                &peer,
                                r,
                            )
                    })
                } else {
                    Vec::new()
                };
                let native = page_scope(links, scope);
                let transfer = binding.transfer;
                let mut granted = Err(Error::Overloaded);
                if !spares.is_empty() {
                    for r in std::iter::once(rail).chain(spares) {
                        granted = links.grant(&peer, r, transfer, &native).await;
                        if !matches!(granted, Err(Error::Overloaded)) {
                            break;
                        }
                    }
                }
                if matches!(granted, Err(Error::Overloaded)) {
                    let until = crate::rdma::links::grant_wait_until(
                        native.clock().now(),
                        native.deadline.0,
                    );
                    let parked = links
                        .grant_parked(&peer, rail, transfer, until, &native)
                        .await;
                    let waited = match &parked {
                        Ok((_, waited)) => *waited,
                        Err(error) => *error == Error::Overloaded,
                    };
                    links.record_grant_wait(waited, parked.is_ok());
                    granted = parked.map(|(pending, _)| pending);
                }
                granted
            }
        };
        let page = matches!(
            request.request.operation,
            crate::peer::protocol::Operation::Page { .. }
        );
        let pending = match granted {
            Ok(pending) => pending,
            Err(error) => match grant_failure(error, page, scope) {
                GrantFailure::Backpressure => {
                    links.record_grant_backpressure();
                    return Err(Error::Overloaded);
                }
                GrantFailure::Fallback => {
                    links.record_fallback_grant(error);
                    trip_native(links, &peer, rail, error, false);
                    return Ok(None);
                }
                GrantFailure::Fail => return Err(error),
            },
        };
        binding.rail = pending.rail();
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
                > crate::peer::protocol::encode_deadline(
                    &signatures.ctx().clock,
                    request.request.route.deadline,
                )?
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
        cache: Option<&racer_wire::control::CacheId>,
        length: usize,
        scope: &RequestScope,
    ) -> Result<(ConnectionLease, WireBuffer)> {
        let (admission, codec) = &self.wire;
        // Page bodies land in aligned storage charged to their cache.
        let buffer = match cache {
            Some(cache) if length != 0 => {
                let buffers = codec.buffers();
                let reservation = self
                    .reserve_ciphertext_waiting(cache, buffers.aligned_capacity(length)?, scope)
                    .await?;
                WireBuffer::aligned(buffers, reservation, length)?
            }
            _ => WireBuffer::unkeyed(codec.buffers(), admission, length)?,
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
        granted: std::time::Instant,
        scope: &RequestScope,
        watch: &CloseWatch,
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
        let plan = self.response_plan(
            &connection,
            &authentication,
            membership,
            &envelope.page,
            verified.peer.node(),
            false,
        )?;
        if !crate::rdma::admits_rail(
            plan,
            membership,
            self.signatures.node(),
            verified.peer.node(),
            binding.rail,
        ) {
            return Err(Error::Unauthorized);
        }
        if phase == Phase::Finish {
            if let Some(links) = &self.links {
                links.record_http_fallback();
                // An answer this slow most likely means the sender's write
                // ran out of its page timeout: a soft failure, which trips
                // only in a streak with no RDMA success in between.
                if scope.clock().now().saturating_duration_since(granted) >= links.page_timeout() {
                    links.breaker_trip(
                        verified.peer.node(),
                        binding.rail,
                        crate::rdma::breaker::TripReason::SlowFallback,
                    );
                }
            }
            // The owner keeps the tag until the sender's release arrives.
            drop(pending);
            return self
                .receive_fallback(connection, authentication, length, scope, watch)
                .await
                .map(RelayResponse::Complete);
        }
        // A signed `Complete` ends the HTTP exchange: its head carries no body, and the
        // page arrives over the link. Finish it now so a failure while the page lands
        // (ciphertext budget, a lost arrival, a canceled read) returns the socket to
        // the pool instead of closing it. The lease is held to the end so its peer and
        // receive permits keep covering the page until it is materialized.
        let mut connection = connection;
        connection.finish_exchange()?;
        watch.settle(&connection);
        if pending.is_landing() {
            let landed = pending
                .finish_landed(&verified, &envelope, &native_scope(scope))
                .await;
            self.native_outcome(verified.peer.node(), binding.rail, &landed, scope);
            let landed = landed?;
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
            drop(connection);
            return Ok(RelayResponse::Landed(Box::new(LandedResponse {
                authentication,
                metadata,
                envelope,
                landed,
            })));
        }
        let (admission, _) = &self.wire;
        // The sender already wrote the page, so wait briefly for ciphertext budget
        // rather than discard a delivered page and send the walk around again.
        let completed = pending.completion_length(&verified, &envelope)?;
        let reservation = self
            .reserve_ciphertext_waiting(
                &envelope.page.version.object.cache,
                completed as usize,
                scope,
            )
            .await?;
        // The arrival stays in registered memory; retaining paths copy it out.
        let page = pending
            .finish_reserved(
                &verified,
                envelope,
                reservation,
                admission,
                &native_scope(scope),
            )
            .await;
        self.native_outcome(verified.peer.node(), binding.rail, &page, scope);
        let page = page?;
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
        drop(connection);
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
        watch: &CloseWatch,
    ) -> Result<SignedResponse> {
        scope.check()?;
        let cache = super::protocol::page_descriptor(&authentication.original.head)
            .ok()
            .map(|(metadata, _)| metadata.version.object.cache);
        let (mut connection, buffer) = self
            .read_ciphertext(connection, cache.as_ref(), length, scope)
            .await?;
        let (_, codec) = &self.wire;
        // Unkeyed staging is recharged to the page's cache by the decoder.
        let response = codec.response_body(authentication, buffer.into_body(), scope)?;
        validate_body_length(&response.response, length)?;
        connection.finish_exchange()?;
        watch.settle(&connection);
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

/// Whether the responder refuses a link setup from `peer` on `rail`: an
/// error when the pair fails [`link_allowed`] (no membership, Site, or rail),
/// and `403` when the edge is not an RDMA edge of the membership because at
/// least one end did not take it among its `RDMA_PEERS` best. The initiator
/// backs off on `403` and pages use HTTP.
pub(crate) fn link_refusal(
    membership: Option<&crate::topology::Membership>,
    local: &NodeId,
    peer: &NodeId,
    rail: RailId,
) -> Result<Option<u16>> {
    let Some(membership) = membership.filter(|m| link_allowed(m, local, peer, rail)) else {
        return Err(Error::Unauthorized);
    };
    Ok((!crate::rdma::is_rdma_edge(membership, local, peer)).then_some(403))
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
    let declined = std::cell::Cell::new(false);
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
        match received.value.start {
            StartLine::Response { status: 200 } => {}
            // 409: our offer lost the tie-break; the peer's offer replaces it.
            // 429 (rate limit or overload) and 503 (no RDMA there yet): benign,
            // retry after the minimum backoff without escalating.
            StartLine::Response {
                status: 409 | 429 | 503,
            } => {
                declined.set(true);
                return Err(Error::Unavailable);
            }
            // 403 (not an RDMA edge in the responder's membership) and anything
            // else: try again after the escalating backoff.
            _ => return Err(Error::Unavailable),
        }
        let mut connection = received.connection;
        let (verified, _) = peer_wire::link::RdmaLink::verify(
            signatures.wire(),
            Some(peer),
            unframe(
                received.value,
                true,
                crate::peer::protocol::chains(&connection)?,
            )?,
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
    let outcome = match &result {
        Ok(_) => {
            attempt.finish();
            crate::rdma::SetupOutcome::Success
        }
        Err(_) if declined.get() => {
            attempt.decline();
            crate::rdma::SetupOutcome::Declined
        }
        Err(_) => {
            links.breaker_trip(peer, rail, crate::rdma::breaker::TripReason::Setup);
            crate::rdma::SetupOutcome::Failed
        }
    };
    links.record_setup(outcome);
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
    let ctx = io.reactor().ctx();
    let mut request = [0; 16];
    ctx.rng.fill(&mut request).map_err(|_| Error::Unavailable)?;
    let scope = RequestScope::with_clock(
        crate::model::RequestId(request),
        ctx.now() + crate::rdma::SETUP_DEADLINE,
        ctx.clock.clone(),
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
    /// offers get `403` (the edge is not an RDMA edge), `409` (our concurrent
    /// offer wins), `429` (rate limit or
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
            Some(_)
                if let Some(status) =
                    link_refusal(membership.ok().as_deref(), local, &peer, rail)? =>
            {
                link_status(status)
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
///
/// Bodies land in aligned storage from the receiving worker's pool so the
/// decoded page can be persisted without a copy.
pub(crate) enum WireBuffer {
    /// A zero-length body.
    Empty,
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
    /// Staging for a body whose cache is not known yet, charged unkeyed to
    /// `admission`, which must own `buffers`. The decoder swaps the charge
    /// for one keyed to the page's cache without copying.
    pub(crate) fn unkeyed(
        buffers: &crate::memory::BufferPool,
        admission: &flow_control::Quotas<AdmissionPolicy>,
        length: usize,
    ) -> Result<Self> {
        if length == 0 {
            return Ok(Self::Empty);
        }
        if length > crate::model::PAGE_BYTES as usize + 16 {
            return Err(Error::InvalidRequest);
        }
        let reservation = admission.reserve(
            None,
            ResourceClass::Ciphertext,
            buffers.aligned_capacity(length)?,
        )?;
        Self::aligned(buffers, reservation, length)
    }
    /// Hand the completed receive to the response decoder with its charge.
    pub(crate) fn into_body(self) -> super::protocol::ResponseBody {
        match self {
            Self::Empty => super::protocol::ResponseBody::Empty,
            Self::Aligned(buffer) => super::protocol::ResponseBody::Aligned(buffer),
        }
    }
}
// SAFETY: the aligned variant owns stable, exclusive backing and its complete
// reservation until dropped; the empty variant exposes no memory.
unsafe impl IoBuffer for WireBuffer {
    fn bytes(&self) -> racer_io::Result<&[u8]> {
        match self {
            Self::Empty => Ok(&[]),
            Self::Aligned(buffer) => buffer.bytes(),
        }
    }
    fn bytes_mut(&mut self) -> racer_io::Result<&mut [u8]> {
        match self {
            Self::Empty => Ok(&mut []),
            Self::Aligned(buffer) => buffer.bytes_mut(),
        }
    }
}

/// Frees idle memory for a cache's ciphertext reservation of the given size.
pub(crate) type Reclaim<'a> = dyn Fn(&racer_wire::control::CacheId, usize) + 'a;
type ReclaimCiphertext = Reclaim<'static>;

/// Reserve keyed peer-receive ciphertext, reclaiming idle cache memory once
/// before failing. Idle cache entries keep their Ciphertext charge, so without
/// this a full cache share would reject every in-flight peer receive.
pub(crate) fn reserve_ciphertext(
    admission: &flow_control::Quotas<AdmissionPolicy>,
    reclaim: Option<&Reclaim<'_>>,
    cache: &racer_wire::control::CacheId,
    capacity: usize,
) -> Result<flow_control::Charge<AdmissionPolicy>> {
    let reserve = || {
        admission
            .reserve(Some(cache), ResourceClass::Ciphertext, capacity)
            .map_err(Error::from)
    };
    crate::admission::report_final(admission, || match (reserve(), reclaim) {
        (Err(Error::Overloaded), Some(reclaim)) => {
            reclaim(cache, capacity);
            reserve()
        }
        (result, _) => result,
    })
}

/// A connect that failed for want of a local resource says nothing about the peer.
///
/// `EADDRNOTAVAIL` is the kernel out of ephemeral ports for this destination (its closed
/// connections still sit in `TIME_WAIT`), and the rest are descriptor or buffer exhaustion on
/// this node. Each is this node being busy: the walk retries the same owner later
/// (`Overloaded`) instead of treating it as unreachable and asking origin for every page until
/// the ports free up.
pub(crate) fn local_connect_overload(error: Error) -> Error {
    match error {
        Error::Os(
            libc::EADDRNOTAVAIL
            | libc::EADDRINUSE
            | libc::EMFILE
            | libc::ENFILE
            | libc::ENOBUFS
            | libc::ENOMEM
            | libc::EAGAIN,
        ) => Error::Overloaded,
        error => error,
    }
}

/// Counts a requester's peer connection that closes instead of returning to the pool.
///
/// Every close leaves this node's ephemeral port in `TIME_WAIT` toward the peer for
/// the kernel's 60 s, so a steady close rate runs the node out of ports for that peer
/// (`racer_peer_connect_local_exhausted_total`). The exchange holds the watch while its
/// lease would close if dropped: from checkout until the lease is clean again (after
/// the handshake, or a finished exchange) or handed to a relay. A watch dropped while
/// held counts one close labeled by the exchange's error, or `abandoned` when the
/// exchange future itself was dropped mid-flight (a lost race or a canceled read).
#[derive(Default)]
pub(super) struct CloseWatch {
    metrics: Option<crate::telemetry::Metrics>,
    held: std::cell::Cell<bool>,
    error: std::cell::Cell<Option<Error>>,
}

impl CloseWatch {
    fn new(metrics: Option<crate::telemetry::Metrics>) -> Self {
        Self {
            metrics,
            held: std::cell::Cell::new(false),
            error: std::cell::Cell::new(None),
        }
    }

    /// The exchange now owns a lease that closes if dropped.
    fn hold(&self) {
        self.held.set(true);
    }

    /// The exchange's lease returns to the pool if dropped, or is no longer its own.
    fn release(&self) {
        self.held.set(false);
    }

    /// Settle a lease after a successful `finish_exchange`: it pools unless the peer
    /// asked to close it (`Connection: close`), which counts as `peer_close`.
    fn settle(&self, connection: &ConnectionLease) {
        if !connection.is_reusable() && self.held.get() {
            self.record(Event::PeerConnectionClosePeerClose);
        }
        self.release();
    }

    /// Record the error that ends the exchange.
    fn fail(&self, error: Error) {
        self.error.set(Some(error));
    }

    fn record(&self, event: Event) {
        if let Some(metrics) = &self.metrics {
            metrics.record(event, 1);
        }
    }

    /// The close reason for a watch still held when the exchange ends.
    fn reason(error: Option<Error>) -> Event {
        match error {
            None => Event::PeerConnectionCloseAbandoned,
            Some(Error::Canceled) => Event::PeerConnectionCloseCanceled,
            Some(Error::DeadlineExceeded) => Event::PeerConnectionCloseDeadline,
            Some(Error::Overloaded) => Event::PeerConnectionCloseOverloaded,
            Some(_) => Event::PeerConnectionCloseError,
        }
    }
}

impl Drop for CloseWatch {
    fn drop(&mut self) {
        if self.held.get() {
            self.record(Self::reason(self.error.get()));
        }
    }
}

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
///
/// The type parameters carry the proof state: the transport returns signed,
/// unverified owners, and the requester converts them exactly once into
/// [`VerifiedExchange`] so no later stage verifies the same envelope again.
#[allow(clippy::large_enum_variant)] // Keep completed delivery inline without another allocation.
pub enum RelayResponse<C = SignedResponse, A = crate::peer::forwarding::ForwardedHead> {
    Complete(C),

    /// A page in a GPU landing buffer. Only a requester that asked for
    /// landing gets this, and only that requester's sealed read uses it.
    /// The transport verifies it against the request binding before returning.
    Landed(Box<LandedResponse>),

    Http {
        authentication: A,

        connection: Box<crate::http::ConnectionLease>,

        length: usize,

        /// Links to charge one overflow page once the opaque body is
        /// relayed in full, set only when this hop is an overflow edge.
        overflow: Option<Rc<crate::rdma::Links>>,
    },
}

/// An exchange whose response proofs were verified once against the request binding.
pub type VerifiedExchange = RelayResponse<
    crate::peer::forwarding::VerifiedResponse,
    crate::peer::forwarding::VerifiedOpaque,
>;

/// Whether the hop from `local` to `peer` carries `operation` over HTTP only
/// because the edge is an overflow edge (see [`crate::rdma::is_overflow`]).
pub(crate) fn overflow_hop(
    plan: TransportPlan,
    membership: Option<&crate::topology::Membership>,
    operation: &super::protocol::Operation,
    local: &racer_wire::control::NodeId,
    peer: &racer_wire::control::NodeId,
) -> bool {
    plan == TransportPlan::Http
        && membership
            .zip(super::rail_hint(operation))
            .is_some_and(|(membership, page)| {
                crate::rdma::is_overflow(membership, &page, local, peer)
            })
}

/// Whether a validated HTTP response of `length` body bytes delivered a page.
/// Misses, signed overloads, and other bodiless outcomes deliver none.
pub(crate) fn delivers_page_body<P, G>(
    response: &racer_core::read::PeerResponse<P, G>,
    length: usize,
) -> bool {
    use racer_core::read::PeerResponse;
    length != 0
        && matches!(
            response,
            PeerResponse::Page { .. }
                | PeerResponse::Selected { .. }
                | PeerResponse::Bootstrap {
                    page_zero: Some(_),
                    ..
                }
        )
}

/// Count one page body delivered over HTTP on an overflow edge.
pub(crate) fn record_overflow(overflow: Option<&crate::rdma::Links>, delivered: bool) {
    if delivered && let Some(links) = overflow {
        links.record_fallback_overflow();
    }
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
    metrics: Option<crate::telemetry::Metrics>,
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
            metrics: None,
        }
    }
    /// Count this worker's local peer-transport refusals.
    pub(crate) fn with_metrics(mut self, metrics: crate::telemetry::Metrics) -> Self {
        self.metrics = Some(metrics);
        self
    }
    fn record(&self, event: Event, amount: u64) {
        if let Some(metrics) = &self.metrics {
            metrics.record(event, amount);
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
    /// Reserve a keyed peer-receive body, reclaiming idle cache memory first.
    pub(crate) fn reserve_ciphertext(
        &self,
        cache: &racer_wire::control::CacheId,
        capacity: usize,
    ) -> Result<flow_control::Charge<AdmissionPolicy>> {
        reserve_ciphertext(&self.wire.0, self.reclaim.as_deref(), cache, capacity)
    }
    /// Reserve a keyed peer-receive body whose bytes are already on the wire, waiting briefly
    /// for this worker's ciphertext budget instead of failing at once.
    ///
    /// The peer has answered and its body is in flight, so a refusal here closes a healthy
    /// connection mid-body: the walk retries on a new one, and every refusal parks an ephemeral
    /// port in `TIME_WAIT`. Pages in decode release ciphertext within milliseconds, so a short
    /// wait ([`CIPHERTEXT_WAIT`], at most a quarter of what is left of the request) usually
    /// keeps the connection. A wait that runs out still fails `Overloaded`.
    pub(crate) async fn reserve_ciphertext_waiting(
        &self,
        cache: &racer_wire::control::CacheId,
        capacity: usize,
        scope: &RequestScope,
    ) -> Result<flow_control::Charge<AdmissionPolicy>> {
        let admission = &self.wire.0;
        // Every poll retries, so only the attempt that gives up is recorded.
        let last = std::cell::Cell::new(None);
        let attempt = || {
            let (result, rejection) =
                admission.retrying(|| self.reserve_ciphertext(cache, capacity));
            last.set(rejection.or(last.get()));
            result
        };
        let first = attempt();
        if !matches!(first, Err(Error::Overloaded)) {
            return first;
        }
        let _waiting = self.wire.0.policy().holders().waiting();
        let reactor = self.io.reactor();
        let clock = || scope.clock().now();
        let now = clock();
        let until = now + CIPHERTEXT_WAIT.min(scope.deadline.0.saturating_duration_since(now) / 4);
        let (reserved, _) = crate::rdma::links::retry_overloaded(
            || std::future::ready(attempt()),
            |wake| reactor.sleep_until(wake, scope),
            clock,
            until,
        )
        .await;
        if let (Err(Error::Overloaded), Some(rejection)) = (&reserved, last.get()) {
            admission.report(rejection);
        }
        self.record(Event::PeerReceiveCiphertextWaits, 1);
        if reserved.is_ok() {
            self.record(Event::PeerReceiveCiphertextWaitGranted, 1);
        }
        reserved
    }
    pub(crate) fn with_reclamation(
        mut self,
        reclaim: impl Fn(&racer_wire::control::CacheId, usize) + 'static,
    ) -> Self {
        self.reclaim = Some(Rc::new(reclaim));
        self
    }
    /// The signed envelope and HTTP ciphertext share one exclusive pooled socket.
    /// An exchange that fails or is abandoned after its head is sent and before its
    /// response completes is never marked reusable; one that fails before the head is
    /// sent, or after a bodiless answer completes, returns the socket to the pool.
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
            let watch = CloseWatch::new(self.metrics.clone());
            let result = async {
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
                let mut connection = observer
                    .result(
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
                    )
                    .map_err(|error| {
                        let error = local_connect_overload(error);
                        if error == Error::Overloaded {
                            self.record(Event::PeerConnectLocalExhausted, 1);
                        }
                        error
                    })?;
                watch.hold();
                connection.state_mut().exchange.admission = peer_admission.clone();
                if let Some(timing) = timing.as_deref_mut() {
                    timing.end(0);
                }
                connection.state_mut().relay.reservation = relay.clone();
                // Connection and handshake get separate bounded idle allowances.
                // No header byte can renew the request/head allowance.
                scope.candidate_progress()?;
                let mut connection = {
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
                        crate::peer::protocol::connect(
                            &self.io, connection, signatures, &peer, scope,
                        )
                        .await,
                    )?
                };
                if let Some(timing) = timing.as_deref_mut() {
                    timing.end(1);
                }
                // Nothing of this exchange is on the wire yet. A grant wait that fails
                // or is canceled before the head is sent returns the socket to the pool
                // instead of closing it; `send_head` revokes this.
                if connection.release_idle() {
                    watch.release();
                }
                // Native response verification requires an authenticated membership lease.
                let granting = scope.clock().now();
                let native = if let Some(membership) = &membership {
                    self.accept_native(&request, plan, membership, landing, scope)
                        .await?
                } else {
                    None
                };
                if native.is_some() {
                    self.record_rdma_stage(Event::RdmaGrantCount, Event::RdmaGrantNs, granting);
                }
                // Overflow counts only page bodies this hop actually receives.
                let overflow = self.links.clone().filter(|_| {
                    overflow_hop(
                        plan,
                        membership.as_deref(),
                        &request.request.operation,
                        self.signatures.node(),
                        &peer,
                    )
                });
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
                let heading = scope.clock().now();
                watch.hold();
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
                if peer_wire::is_overloaded(&received.value) {
                    // The next holder shed the request before verifying it
                    // (`racer_core::route::unverified_failure`): a relay signs its own `Overloaded`,
                    // a requester's walk asks its helper. The answer is complete (no body) and the
                    // holder keeps the connection open (`PeerServer` finishes the exchange), so pool it:
                    // closing it here would park an ephemeral port in `TIME_WAIT` for every shed, and
                    // a hot holder's sheds would run this node out of ports for that holder.
                    let mut connection = received.connection;
                    if connection.finish_exchange().is_ok() {
                        watch.settle(&connection);
                    }
                    drop(connection);
                    return Err(Error::Overloaded);
                }
                let control = detach(
                    &mut received.value,
                    crate::peer::protocol::chains(&received.connection)?,
                )?;
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
                let (authentication, length) = decode_envelope(
                    received.value,
                    true,
                    crate::peer::protocol::chains(&received.connection)?,
                )?;
                if let Some(control) = control {
                    let (binding, accept, peer, pending) = native.ok_or(Error::Unauthorized)?;
                    let arriving = scope.clock().now();
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
                            granting,
                            scope,
                            &watch,
                        )
                        .await;
                    if received.is_ok() {
                        self.record_rdma_stage(
                            Event::RdmaArrivalCount,
                            Event::RdmaArrivalNs,
                            arriving,
                        );
                    }
                    return received;
                }
                if relay.is_some() {
                    received.connection.state_mut().relay.context = relay_context;
                    // The relay owns the connection and finishes or closes it.
                    watch.release();
                    return Ok(RelayResponse::Http {
                        authentication,
                        connection: Box::new(received.connection),
                        length,
                        overflow,
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
                watch.settle(&connection);
                record_overflow(
                    overflow.as_deref(),
                    delivers_page_body(&response.response, length),
                );
                Ok(RelayResponse::Complete(response))
            }
            .await;
            if let Err(error) = &result {
                watch.fail(*error);
            }
            result
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
            super::protocol::ResponseBody::Empty
        } else {
            let cache = &request.request.origin.object.cache;
            let buffers = codec.buffers();
            // Charge the whole pooled size class of the aligned body.
            let capacity = buffers.aligned_capacity(length)?;
            let reservation = self
                .reserve_ciphertext_waiting(cache, capacity, scope)
                .await;
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
                            first: first
                                .map(|at| timestamp(scope.clock(), at))
                                .unwrap_or_default(),
                            last: last
                                .map(|at| timestamp(scope.clock(), at))
                                .unwrap_or_default(),
                            now: timestamp(scope.clock(), scope.clock().now()),
                            original: scope
                                .body_deadlines
                                .map(|d| timestamp(scope.clock(), d.0))
                                .unwrap_or_default(),
                            share: scope
                                .body_deadlines
                                .map(|d| timestamp(scope.clock(), d.1))
                                .unwrap_or_default(),
                            signed: timestamp(scope.clock(), request.request.route.deadline.0),
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
                let now = scope.clock().now();
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
                    .min(scope.clock().now() + Duration::from_millis(10));
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

    /// A held watch counts one close by the exchange's error, or `abandoned` when the
    /// exchange future was dropped; a released watch counts nothing.
    #[test]
    fn close_watch_counts_only_held_connections_by_reason() {
        use crate::telemetry::{Event, Metrics};
        let cases = [
            (None, Event::PeerConnectionCloseAbandoned),
            (Some(Error::Canceled), Event::PeerConnectionCloseCanceled),
            (
                Some(Error::DeadlineExceeded),
                Event::PeerConnectionCloseDeadline,
            ),
            (
                Some(Error::Overloaded),
                Event::PeerConnectionCloseOverloaded,
            ),
            (Some(Error::Io), Event::PeerConnectionCloseError),
            (Some(Error::Unauthorized), Event::PeerConnectionCloseError),
        ];
        for (error, event) in cases {
            let metrics = Metrics::default();
            let watch = CloseWatch::new(Some(metrics.clone()));
            watch.hold();
            if let Some(error) = error {
                watch.fail(error);
            }
            drop(watch);
            assert_eq!(metrics.count(event), 1, "{error:?}");

            // Released (pooled or handed off) before the error: no close.
            let metrics = Metrics::default();
            let watch = CloseWatch::new(Some(metrics.clone()));
            watch.hold();
            watch.release();
            if let Some(error) = error {
                watch.fail(error);
            }
            drop(watch);
            assert_eq!(metrics.count(event), 0, "{error:?}");
        }
        // Never held (checkout failed): nothing to close.
        let metrics = Metrics::default();
        let watch = CloseWatch::new(Some(metrics.clone()));
        watch.fail(Error::Overloaded);
        drop(watch);
        assert_eq!(metrics.count(Event::PeerConnectionCloseOverloaded), 0);
    }

    /// Settling a finished lease pools it, unless the peer asked to close it.
    #[test]
    fn close_watch_settle_counts_a_peer_requested_close() {
        use crate::telemetry::{Event, Metrics};
        let env = racer_io::Env::host();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(true).limits,
        )));
        let _reactor = Rc::new(crate::runtime::Reactor::new(admission.clone(), &env));
        for close in [false, true] {
            let metrics = Metrics::default();
            let watch = CloseWatch::new(Some(metrics.clone()));
            let (a, _b) = std::os::unix::net::UnixStream::pair().unwrap();
            let mut lease = crate::http::from_accepted(a.into(), &admission).unwrap();
            lease.set_framing(Some(0), Some(0), false);
            if close {
                lease.poison();
            }
            lease.finish_exchange().unwrap();
            watch.hold();
            watch.settle(&lease);
            assert!(!watch.held.get());
            drop(watch);
            assert_eq!(
                metrics.count(Event::PeerConnectionClosePeerClose),
                u64::from(close)
            );
            assert_eq!(metrics.count(Event::PeerConnectionCloseAbandoned), 0);
        }
    }

    /// Overflow counts a page only when its body arrives over an overflow
    /// edge, once per hop, and never for bodiless outcomes or other edges.
    #[test]
    fn overflow_counts_only_delivered_page_bodies() {
        use crate::rdma::tests::rails_tests::{mesh_with_budget, page};
        use crate::telemetry::Event;
        use racer_core::read::PeerResponse;
        type Response = PeerResponse<(), ()>;

        let membership = mesh_with_budget(40, 16);
        let nodes: Vec<_> = membership
            .members()
            .iter()
            .map(|m| m.node.clone())
            .collect();
        let operation = super::super::protocol::Operation::Page {
            page: page(0),
            mode: super::super::protocol::FetchMode::CopyOnly,
        };
        let hop = |a, b| {
            overflow_hop(
                TransportPlan::Http,
                Some(&membership),
                &operation,
                &nodes[a],
                &nodes[b],
            )
        };
        let edge = |a, b| crate::rdma::is_rdma_edge(&membership, &nodes[a], &nodes[b]);
        let pick = |want_rdma: bool| {
            (1..nodes.len())
                .find(|&b| membership.is_neighbor(&nodes[0], &nodes[b]) && edge(0, b) == want_rdma)
                .expect("edge kind present")
        };
        let (overflow, rdma) = (pick(false), pick(true));
        assert!(hop(0, overflow));
        assert!(!hop(0, rdma));
        // A planned RDMA hop, no membership, or a bodiless operation is never overflow.
        assert!(!overflow_hop(
            TransportPlan::Rdma { rail: RailId(7) },
            Some(&membership),
            &operation,
            &nodes[0],
            &nodes[overflow],
        ));
        assert!(!overflow_hop(
            TransportPlan::Http,
            None,
            &operation,
            &nodes[0],
            &nodes[overflow],
        ));
        let offer = super::super::protocol::Operation::Page {
            page: page(0),
            mode: super::super::protocol::FetchMode::Offer,
        };
        assert!(!overflow_hop(
            TransportPlan::Http,
            Some(&membership),
            &offer,
            &nodes[0],
            &nodes[overflow],
        ));

        let metadata = || crate::model::ObjectMetadata {
            content_type: None,
            version: page(0).version,
            length: 8,
            expires_at: crate::model::ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        };
        let body = Response::Page {
            metadata: metadata(),
            ciphertext: (),
        };
        assert!(delivers_page_body(&body, 8));
        assert!(delivers_page_body(
            &Response::Selected {
                metadata: metadata(),
                ciphertext: (),
                grant: (),
            },
            8
        ));
        for bodiless in [Response::Miss, Response::Overloaded, Response::NotFound] {
            assert!(!delivers_page_body(&bodiless, 0));
        }

        let w = crate::rdma::links::tests::world();

        let (w, metrics, _) = crate::rdma::links::tests::send_in_place::metered(w);
        let links = Some(&*w.a.links);
        let count = || metrics.count(Event::RdmaFallbackOverflow);
        // Overflow edge, delivered page: once.
        record_overflow(
            links.filter(|_| hop(0, overflow)),
            delivers_page_body(&body, 8),
        );
        assert_eq!(count(), 1);
        // Overflow edge, copy miss or signed overload: none.
        record_overflow(
            links.filter(|_| hop(0, overflow)),
            delivers_page_body(&Response::Miss, 0),
        );
        record_overflow(
            links.filter(|_| hop(0, overflow)),
            delivers_page_body(&Response::Overloaded, 0),
        );
        // Overflow edge, failed exchange or failed relay body: none.
        record_overflow(links.filter(|_| hop(0, overflow)), false);
        assert_eq!(count(), 1);
        // A relay hop on an overflow edge counts its relayed body once.
        let relayed: Result<()> = Ok(());
        record_overflow(
            links.filter(|_| hop(0, overflow)),
            relayed.is_ok() && 8 != 0,
        );
        assert_eq!(count(), 2);
        // A non-overflow edge never counts, body or not.
        record_overflow(links.filter(|_| hop(0, rdma)), true);
        assert_eq!(count(), 2);
        assert_eq!(metrics.count(Event::RdmaHttpFallback), 0);
    }
    #[test]
    fn link_refusal_admits_only_rdma_edges() {
        let rail = RailId(7);
        let small = crate::rdma::tests::rails_tests::mesh(8);
        let a = &small.members()[0].node;
        let b = &small.members()[1].node;
        assert_eq!(link_refusal(Some(&small), a, b, rail), Ok(None));
        assert_eq!(link_refusal(None, a, b, rail), Err(Error::Unauthorized));
        assert_eq!(
            link_refusal(Some(&small), a, b, RailId(99)),
            Err(Error::Unauthorized)
        );
        // Over the peer budget, an eligible edge one end did not take gets 403.
        let large = crate::rdma::tests::rails_tests::mesh_with_budget(40, 16);
        let nodes: Vec<_> = large.members().iter().map(|m| m.node.clone()).collect();
        let (mut refused, mut admitted) = (0, 0);
        for b in &nodes[1..] {
            match link_refusal(Some(&large), &nodes[0], b, rail).unwrap() {
                None => {
                    assert!(crate::rdma::is_rdma_edge(&large, &nodes[0], b));
                    admitted += 1;
                }
                Some(status) => {
                    assert_eq!(status, 403);
                    assert!(!crate::rdma::is_rdma_edge(&large, &nodes[0], b));
                    refused += 1;
                }
            }
        }
        assert!(admitted > 0 && refused > 0);
    }
    #[test]
    fn adaptive_socket_attribution_ignores_local_pressure_and_expired_scope() {
        let scope = RequestScope::host(
            crate::model::RequestId([91; 16]),
            racer_io::Clock::host().now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        let owner = crate::peer::AdaptivePeers::new(
            Default::default(),
            Default::default(),
            racer_io::Clock::host(),
        )
        .unwrap();
        let node = racer_wire::control::NodeId("peer".into());
        let permit = Some(owner.acquire(&node).unwrap());
        let failed = Rc::new(std::cell::Cell::new(false));
        for error in [
            Error::Overloaded,
            Error::Unavailable,
            Error::DeadlineExceeded,
            Error::Canceled,
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
        use racer_wire::control::KeyId;
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
            use racer_wire::control::RailId;
            use racer_wire::control::RailMapping;
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
            /// Responder membership override; `sites` builds it otherwise.
            membership: Option<std::sync::Arc<crate::topology::Membership>>,
        }

        fn link_setup(
            env: &racer_io::Env,
            case: LinkSetupCase,
        ) -> (
            crate::rdma::links::tests::World,
            Result<()>,
            Result<()>,
            NodeId,
            NodeId,
        ) {
            use crate::rdma::links::tests::world_for;
            let signers = crate::peer::tests::signers(env);
            let membership = case
                .membership
                .clone()
                .unwrap_or_else(|| test_membership(&signers, &[0, 2], &case.sites));
            let responder = 2 - case.initiator;
            let mut w = world_for(
                [
                    signers[case.initiator].node().clone(),
                    signers[responder].node().clone(),
                ],
                false,
            );
            let env = w.env.clone();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone(), &env));
            let initiator = transfers(signers[case.initiator].clone(), &admission, &reactor)
                .with_links(w.a.links.clone());
            let mut server = transfers(signers[responder].clone(), &admission, &reactor);
            if case.responder_links {
                server = server.with_links(w.b.links.clone());
            }
            let scope =
                RequestScope::host(RequestId([5; 16]), Instant::now() + Duration::from_secs(10))
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
                    let (auth, length) = decode_envelope(
                        received.value,
                        false,
                        crate::peer::protocol::chains(&received.connection)?,
                    )?;
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
            let env = racer_io::Env::host();
            for initiator in [0, 2] {
                let (w, client, serve, local, peer) = link_setup(
                    &env,
                    LinkSetupCase {
                        initiator,
                        responder_links: true,
                        responder_opens: false,
                        sites: ["site1", "site1"],
                        membership: None,
                    },
                );
                client.unwrap();
                serve.unwrap();
                let (_, ours) = w.a.links.ready(&peer, RailId(7)).expect("initiator ready");
                let (_, theirs) = w.b.links.ready(&local, RailId(7)).expect("responder ready");
                assert_eq!(ours, theirs);
            }
        }

        #[test]
        fn signed_link_setup_tie_break_rejects_the_higher_offer() {
            let env = racer_io::Env::host();
            let signers = crate::peer::tests::signers(&env);
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
            let (mut w, client, serve, local, peer) = link_setup(
                &env,
                LinkSetupCase {
                    initiator,
                    responder_links: true,
                    responder_opens: true,
                    sites: ["site1", "site1"],
                    membership: None,
                },
            );
            assert_eq!(client, Err(Error::Unavailable));
            serve.unwrap();
            assert!(w.a.links.ready(&peer, RailId(7)).is_none());
            assert!(matches!(
                w.a.links.state(&peer, RailId(7)),
                rdma_verbs::LinkState::Backoff(_) | rdma_verbs::LinkState::Breaking
            ));
            // A lost tie-break is benign: declined, not abandoned.
            w.a.owner.poll_budgeted(8).unwrap();
            assert_eq!(w.a.links.link_breaks(rdma_verbs::BreakReason::Declined), 1);
            assert_eq!(w.a.links.link_breaks(rdma_verbs::BreakReason::Abandoned), 0);
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
            let env = racer_io::Env::host();
            let (mut w, client, serve, _, peer) = link_setup(
                &env,
                LinkSetupCase {
                    initiator: 0,
                    responder_links: false,
                    responder_opens: false,
                    sites: ["site1", "site1"],
                    membership: None,
                },
            );
            assert_eq!(client, Err(Error::Unavailable));
            serve.unwrap();
            assert!(w.a.links.ready(&peer, RailId(7)).is_none());
            // 503 (no RDMA there) is benign: declined, not abandoned.
            w.a.owner.poll_budgeted(8).unwrap();
            assert_eq!(w.a.links.link_breaks(rdma_verbs::BreakReason::Declined), 1);
            assert_eq!(w.a.links.link_breaks(rdma_verbs::BreakReason::Abandoned), 0);
            for sites in [["site1", "site2"], ["", ""]] {
                let (w, client, serve, local, peer) = link_setup(
                    &env,
                    LinkSetupCase {
                        initiator: 0,
                        responder_links: true,
                        responder_opens: false,
                        sites,
                        membership: None,
                    },
                );
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

        /// Members 0 and 2 of the test signers in one Site and rail, plus
        /// `fillers` more, with each node taking at most `budget` RDMA peers.
        fn budget_membership(
            signers: &[Rc<Signatures>],
            fillers: usize,
            budget: usize,
        ) -> std::sync::Arc<crate::topology::Membership> {
            let base = test_membership(signers, &[0, 2], &["site1", "site1"]);
            let template = base.members()[0].clone();
            let mut members = base.members().to_vec();
            for i in 0..fillers {
                let mut member = template.clone();
                member.node = NodeId(format!("filler-{i}"));
                member.peer_endpoint = format!("127.0.0.1:{}", 10_000 + i);
                members.push(member);
            }
            let mut membership =
                crate::topology::Membership::validate(MembershipVersion(1), members).unwrap();
            membership.rdma_edges = crate::rdma::RdmaEdges::with_budget(budget);
            std::sync::Arc::new(membership)
        }

        #[test]
        fn signed_link_setup_refuses_a_qualifying_non_rdma_edge() {
            let env = racer_io::Env::host();
            let signers = crate::peer::tests::signers(&env);
            let (a, b) = (signers[0].node(), signers[2].node());
            let refused = (8..64)
                .map(|fillers| budget_membership(&signers, fillers, 1))
                .find(|m| !crate::rdma::is_rdma_edge(m, a, b))
                .expect("some mesh leaves the edge out");
            // The same Site and rail under the default budget is an RDMA edge.
            let admitted = budget_membership(&signers, 8, crate::topology::RDMA_PEERS);
            assert!(crate::rdma::is_rdma_edge(&admitted, a, b));
            assert_eq!(link_refusal(Some(&refused), a, b, RailId(7)), Ok(Some(403)));
            let (mut w, client, serve, local, peer) = link_setup(
                &env,
                LinkSetupCase {
                    initiator: 0,
                    responder_links: true,
                    responder_opens: false,
                    sites: ["site1", "site1"],
                    membership: Some(refused),
                },
            );
            assert_eq!(client, Err(Error::Unavailable));
            serve.unwrap();
            assert!(w.a.links.ready(&peer, RailId(7)).is_none());
            assert_eq!(
                w.b.links.state(&local, RailId(7)),
                rdma_verbs::LinkState::Idle,
                "refused offers never reach the owner"
            );
            // 403 is not benign: the attempt is abandoned and backs off.
            w.a.owner.poll_budgeted(8).unwrap();
            assert_eq!(w.a.links.link_breaks(rdma_verbs::BreakReason::Abandoned), 1);
            assert_eq!(w.a.links.link_breaks(rdma_verbs::BreakReason::Declined), 0);
            let (w, client, serve, local, peer) = link_setup(
                &env,
                LinkSetupCase {
                    initiator: 0,
                    responder_links: true,
                    responder_opens: false,
                    sites: ["site1", "site1"],
                    membership: Some(admitted),
                },
            );
            client.unwrap();
            serve.unwrap();
            let (_, ours) = w.a.links.ready(&peer, RailId(7)).expect("initiator ready");
            let (_, theirs) = w.b.links.ready(&local, RailId(7)).expect("responder ready");
            assert_eq!(ours, theirs);
        }

        #[test]
        fn native_subdeadline_preserves_parent_deadline_and_cancellation() {
            let scope =
                RequestScope::host(RequestId([1; 16]), Instant::now() + Duration::from_secs(30))
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
            assert_eq!(native.check(), Err(Error::Canceled));
            assert!(!native_failure(Error::DeadlineExceeded, &scope));
            assert!(!native_failure(Error::Os(libc::ECONNRESET), &scope));
        }
        #[test]
        fn grant_exhaustion_on_a_page_pushes_back_instead_of_falling_back() {
            let scope =
                RequestScope::host(RequestId([1; 16]), Instant::now() + Duration::from_secs(30))
                    .unwrap();
            assert_eq!(
                grant_failure(Error::Overloaded, true, &scope),
                GrantFailure::Backpressure
            );
            // Non-page bodies still need HTTP when no slot frees.
            assert_eq!(
                grant_failure(Error::Overloaded, false, &scope),
                GrantFailure::Fallback
            );
            // A link that broke or a lost device still falls back.
            for error in [Error::Unavailable, Error::Io, Error::Os(libc::ECONNRESET)] {
                assert_eq!(grant_failure(error, true, &scope), GrantFailure::Fallback);
            }
            assert_eq!(
                grant_failure(Error::Unauthorized, true, &scope),
                GrantFailure::Fail
            );
            scope.cancel().unwrap();
            assert_eq!(
                grant_failure(Error::Overloaded, true, &scope),
                GrantFailure::Fail
            );
        }

        #[test]
        fn spare_rails_are_the_other_eligible_local_rails() {
            let r = RailId;
            assert_eq!(
                spare_rails(r(2), &[r(1), r(2), r(3), r(1), r(6)], |x| x != r(6)),
                vec![r(1), r(3)]
            );
            // Ineligible local rails are skipped, and other NUMA nodes' rails
            // are never spares.
            assert!(spare_rails(r(4), &[r(1)], |x| x != r(1)).is_empty());
            assert!(spare_rails(r(4), &[], |_| true).is_empty());
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
            /// The sender's breaker for the receiver is tripped first, with
            /// this cooldown.
            sender_breaker: Option<Duration>,
            /// The receiver grants a slot of a simulated GPU's landing buffer.
            landing: bool,
            /// The receiver's ciphertext share for the page's cache is held in full
            /// until the end of the exchange.
            ciphertext_full: bool,
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
            /// Sends the sender declined because its plan disagreed with the grant.
            declined: u64,
            /// The receiver's exchange left its connection closing instead of reusable.
            closed: bool,
            /// Receiver ciphertext budget waits.
            ciphertext_waits: u64,
            /// The sender's breaker for the receiver is open after the exchange.
            sender_open: bool,
            /// Sender breaker trips because a write failed.
            sender_trips: u64,
            /// Sender pages sent over HTTP because its breaker was open.
            sender_breaker_http: u64,
            /// Sender breakers closed by a probe.
            sender_closes: u64,
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
                            rails: vec![racer_wire::control::RailMapping {
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

        fn page_roundtrip(env: &racer_io::Env, case: PageCase) -> PageOutcome {
            use crate::rdma::links::tests::link_up;
            use crate::rdma::links::tests::world_for;
            let mut signers = crate::peer::tests::signers(env);
            if case.reverse {
                signers.swap(0, 2);
            }
            let w = world_for(
                [signers[0].node().clone(), signers[2].node().clone()],
                false,
            );
            // `w.b` is always the sending node.
            let (mut w, _, sender_metrics) = crate::rdma::links::tests::send_in_place::metered(w);
            let env = w.env.clone();
            link_up(&env, &mut w);
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(true).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone(), &env));
            let receiver =
                transfers(signers[0].clone(), &admission, &reactor).with_links(w.a.links.clone());
            let sender =
                transfers(signers[2].clone(), &admission, &reactor).with_links(w.b.links.clone());
            let scope =
                RequestScope::host(RequestId([7; 16]), Instant::now() + Duration::from_secs(30))
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
                page_size: racer_wire::object::PageSize::DEFAULT,
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
                deadline: p::encode_deadline(scope.clock(), scope.deadline).unwrap(),
                rail: RailId(7),
            };
            let sender_membership = page_membership(&signers, case.sender_site);
            let receiver_membership = page_membership(&signers, case.receiver_site);
            let gpu = case.landing.then(|| {
                let helper = crate::gpu::testing::helper(&[None]);
                let bytes = (crate::rdma::BUFFER_LEN as u64)
                    .next_multiple_of(crate::config::GPU_STAGING_UNIT);
                crate::rdma::links::tests::attach_landing(&env, &mut w, &helper, bytes);
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
            if let Some(cooldown) = case.sender_breaker {
                let links = sender.links.as_ref().unwrap();
                links.configure_breaker(crate::rdma::breaker::BreakerConfig {
                    base: cooldown,
                    max: cooldown,
                    ..Default::default()
                });
                links.breaker_trip(
                    signers[0].node(),
                    RailId(7),
                    crate::rdma::breaker::TripReason::Setup,
                );
                std::thread::sleep(cooldown.min(Duration::from_millis(50)));
            }
            if case.sender_down {
                let links = sender.links.as_ref().unwrap();
                let (epoch, _) = links.ready(signers[0].node(), RailId(7)).unwrap();
                links.abandon(signers[0].node(), RailId(7), epoch);
                crate::rdma::links::tests::pump(&mut w.a.owner, &mut w.b.owner);
            }
            let released = std::cell::Cell::new(false);
            let receiver_metrics = crate::telemetry::Metrics::default();
            let receiver = receiver.with_metrics(receiver_metrics.clone());
            let full = std::cell::RefCell::new(Vec::new());
            if case.ciphertext_full {
                for shift in (0..48).rev() {
                    while let Ok(charge) =
                        admission.reserve(Some(&cache), ResourceClass::Ciphertext, 1 << shift)
                    {
                        full.borrow_mut().push(charge);
                    }
                }
            }
            let watch = CloseWatch::new(Some(receiver_metrics.clone()));
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
                    watch.hold();
                    let sent = receiver.io.send_head(a, initial, &scope).await?;
                    let mut received = receiver.io.receive_head(sent.connection, &scope).await?;
                    let control = detach(
                        &mut received.value,
                        crate::peer::protocol::chains(&received.connection)?,
                    )?;
                    let (auth, length) = decode_envelope(
                        received.value,
                        true,
                        crate::peer::protocol::chains(&received.connection)?,
                    )?;
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
                            scope.clock().now(),
                            &scope,
                            &watch,
                        )
                        .await
                };
                let send = async {
                    let b = p::accept(&sender.io, b, signers[2].clone(), &scope).await?;
                    let mut received = sender.io.receive_head(b, &scope).await?;
                    let control = detach(
                        &mut received.value,
                        crate::peer::protocol::chains(&received.connection)?,
                    )?
                    .ok_or(Error::Unauthorized)?;
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
            drop(full);
            if let Err(error) = &result {
                watch.fail(*error);
            }
            let closed = watch.held.get();
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
                declined: sender_metrics.count(crate::telemetry::Event::RdmaSendDeclinedPlan),
                closed,
                ciphertext_waits: receiver_metrics
                    .count(crate::telemetry::Event::PeerReceiveCiphertextWaits),
                sender_open: sender
                    .links
                    .as_ref()
                    .unwrap()
                    .breaker_open(signers[0].node(), RailId(7)),
                sender_trips: sender_metrics.count(crate::telemetry::Event::RdmaBreakerTripSend),
                sender_breaker_http: sender_metrics.count(crate::telemetry::Event::RdmaBreakerHttp),
                sender_closes: sender_metrics.count(crate::telemetry::Event::RdmaBreakerClose),
            }
        }

        fn page_bytes(result: &Result<Vec<u8>>) -> Vec<u8> {
            result.as_ref().expect("page").clone()
        }

        /// A page the sender already wrote over the link, arriving while the receiver's
        /// ciphertext budget is full, waits for budget and then fails `Overloaded`, but
        /// its HTTP exchange is complete (the `Complete` head carries no body), so the
        /// connection returns to the pool rather than closing into `TIME_WAIT`.
        #[test]
        fn native_complete_without_ciphertext_budget_keeps_the_connection() {
            let env = racer_io::Env::host();
            let outcome = page_roundtrip(
                &env,
                PageCase {
                    ciphertext_full: true,
                    ..PageCase::default()
                },
            );
            assert!(matches!(outcome.result, Err(Error::Overloaded)));
            assert_eq!(outcome.sent, Ok(true));
            assert_eq!(outcome.ciphertext_waits, 1);
            assert!(
                !outcome.closed,
                "the finished exchange pools its connection"
            );
            assert_eq!(outcome.free.0, outcome.free.1, "the grant is released");
        }

        #[test]
        fn simulated_link_grant_in_request_and_complete_both_directions() {
            let env = racer_io::Env::host();
            for reverse in [false, true] {
                for relayed in [false, true] {
                    let outcome = page_roundtrip(
                        &env,
                        PageCase {
                            reverse,
                            relayed,
                            ..PageCase::default()
                        },
                    );
                    assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
                    assert_eq!(outcome.sent, Ok(true));
                    assert_eq!(outcome.declined, 0);
                    assert!(!outcome.closed, "a completed exchange pools");
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
            let env = racer_io::Env::host();
            for reverse in [false, true] {
                let outcome = page_roundtrip(
                    &env,
                    PageCase {
                        reverse,
                        landing: true,
                        ..PageCase::default()
                    },
                );
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
            let env = racer_io::Env::host();
            let outcome = page_roundtrip(
                &env,
                PageCase {
                    landing: true,
                    sender_down: true,
                    ..PageCase::default()
                },
            );
            assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
            assert!(!outcome.landed);
            assert_eq!(
                (outcome.completed, outcome.fallbacks, outcome.completions),
                (0, 1, 0)
            );
        }

        #[test]
        fn simulated_sender_rejects_cross_site_and_releases_the_grant() {
            let env = racer_io::Env::host();
            for site in ["site2", ""] {
                for (reverse, relayed) in [(false, false), (true, true)] {
                    let outcome = page_roundtrip(
                        &env,
                        PageCase {
                            reverse,
                            relayed,
                            sender_site: Some(site),
                            ..PageCase::default()
                        },
                    );
                    assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
                    assert_eq!(outcome.sent, Ok(false));
                    assert_eq!(outcome.declined, 1, "the declined send is counted");
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
            let env = racer_io::Env::host();
            let outcome = page_roundtrip(
                &env,
                PageCase {
                    sender_down: true,
                    ..PageCase::default()
                },
            );
            assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
            assert_eq!(outcome.sent, Ok(true));
            assert_eq!(
                (outcome.completed, outcome.fallbacks, outcome.completions),
                (0, 1, 0)
            );
        }

        /// A write that fails on a broken link goes over HTTP in the same
        /// exchange (no page retry), and trips the sender's breaker.
        #[test]
        fn failed_write_trips_the_breaker_and_finishes_over_http() {
            let env = racer_io::Env::host();
            let outcome = page_roundtrip(
                &env,
                PageCase {
                    sender_down: true,
                    ..PageCase::default()
                },
            );
            assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
            assert_eq!((outcome.completed, outcome.fallbacks), (0, 1));
            assert!(outcome.sender_open);
            assert_eq!(outcome.sender_trips, 1);
        }

        /// While the sender's breaker is open it answers a grant over HTTP at
        /// once, without writing, and releases the receiver's grant.
        #[test]
        fn open_breaker_sends_over_http_without_writing() {
            let env = racer_io::Env::host();
            let outcome = page_roundtrip(
                &env,
                PageCase {
                    sender_breaker: Some(Duration::from_secs(60)),
                    ..PageCase::default()
                },
            );
            assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
            assert_eq!(outcome.sent, Ok(true));
            assert_eq!((outcome.completed, outcome.fallbacks), (0, 1));
            assert_eq!(outcome.sender_breaker_http, 1);
            assert!(outcome.sender_open);
            assert_eq!(outcome.free, (Some(2), Some(2)), "the grant is released");
        }

        /// After the cooldown one probe page goes over RDMA, and its success
        /// closes the breaker.
        #[test]
        fn probe_after_cooldown_writes_and_closes_the_breaker() {
            let env = racer_io::Env::host();
            let outcome = page_roundtrip(
                &env,
                PageCase {
                    sender_breaker: Some(Duration::from_millis(5)),
                    ..PageCase::default()
                },
            );
            assert_eq!(page_bytes(&outcome.result), vec![0x5a; 144]);
            assert_eq!((outcome.completed, outcome.fallbacks), (1, 0));
            assert_eq!(outcome.sender_breaker_http, 0);
            assert_eq!(outcome.sender_closes, 1);
            assert!(!outcome.sender_open);
        }

        /// The receiver plans HTTP for a peer whose breaker is open, without
        /// taking a grant, and grants again once a probe succeeds.
        #[test]
        fn open_breaker_makes_the_receiver_plan_http_at_once() {
            use crate::rdma::links::tests::link_up;
            use crate::rdma::links::tests::world_for;
            let env = racer_io::Env::host();
            let signers = crate::peer::tests::signers(&env);
            let mut w = world_for(
                [signers[0].node().clone(), signers[2].node().clone()],
                false,
            );
            let env = w.env.clone();
            link_up(&env, &mut w);
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(true).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone(), &env));
            let receiver =
                transfers(signers[0].clone(), &admission, &reactor).with_links(w.a.links.clone());
            let membership = page_membership(&signers, None);
            let (signed, _) = crate::peer::forwarding::Forwarding::new(signers[0].clone())
                .sign_request(crate::peer::tests::request(&env, &admission, 0))
                .unwrap();
            let scope = signed.request.origin.scope().clone();
            let peer = signers[2].node().clone();
            let links = w.a.links.clone();
            let plan = TransportPlan::Rdma { rail: RailId(7) };
            let free = || links.buffers(RailId(7), rdma_verbs::BufferState::Free);
            let accept = |w: &mut crate::rdma::links::tests::World| {
                crate::rdma::links::tests::drive(&mut w.a.owner, &mut w.b.owner, async {
                    receiver
                        .accept_native(&signed, plan, &membership, None, &scope)
                        .await
                        .unwrap()
                        .map(|(_, _, _, pending)| pending)
                })
            };
            // Closed: the receiver grants.
            let granted = accept(&mut w);
            assert!(granted.is_some());
            drop(granted);
            crate::rdma::links::tests::pump(&mut w.a.owner, &mut w.b.owner);
            links.configure_breaker(crate::rdma::breaker::BreakerConfig {
                base: Duration::from_millis(20),
                max: Duration::from_millis(20),
                ..Default::default()
            });
            links.breaker_trip(&peer, RailId(7), crate::rdma::breaker::TripReason::Send);
            // Open: HTTP at once, no grant taken.
            let before = free();
            assert!(accept(&mut w).is_none());
            assert_eq!(free(), before);
            std::thread::sleep(Duration::from_millis(25));
            // Half-open: one probe grant, then HTTP until it reports.
            let probe = accept(&mut w);
            assert!(probe.is_some());
            assert!(accept(&mut w).is_none());
            drop(probe);
            links.breaker_success(&peer, RailId(7));
            // Closed again. (Both test buffers are now held by grants no
            // sender released, so check the verdict rather than grant.)
            assert_eq!(
                links.breaker_admit(&peer, RailId(7)),
                crate::rdma::breaker::Admit::Rdma
            );
        }

        /// A receiver whose owner is backed up (its command queue to the
        /// owner is full) answers over HTTP at once instead of failing the
        /// page `Overloaded` into a retry loop that never reaches RDMA.
        #[test]
        fn backed_up_owner_makes_the_receiver_fall_back_to_http() {
            use crate::rdma::links::tests::link_up;
            use crate::rdma::links::tests::world_for;
            let env = racer_io::Env::host();
            let signers = crate::peer::tests::signers(&env);
            let w = world_for(
                [signers[0].node().clone(), signers[2].node().clone()],
                false,
            );
            let (mut w, metrics, _) = crate::rdma::links::tests::send_in_place::metered(w);
            let env = w.env.clone();
            link_up(&env, &mut w);
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(true).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone(), &env));
            let receiver =
                transfers(signers[0].clone(), &admission, &reactor).with_links(w.a.links.clone());
            let membership = page_membership(&signers, None);
            let (signed, _) = crate::peer::forwarding::Forwarding::new(signers[0].clone())
                .sign_request(crate::peer::tests::request(&env, &admission, 0))
                .unwrap();
            let scope = signed.request.origin.scope().clone();
            let peer = signers[2].node().clone();
            let links = w.a.links.clone();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            // Queue grants the owner never drains.
            let mut queued = Vec::new();
            for n in 0..rdma_verbs::COMMAND_QUEUE {
                let mut grant =
                    Box::pin(links.grant(&peer, RailId(7), TransferId([n as u8; 16]), &scope));
                assert!(grant.as_mut().poll(&mut cx).is_pending());
                queued.push(grant);
            }
            let plan = TransportPlan::Rdma { rail: RailId(7) };
            let mut accept =
                Box::pin(receiver.accept_native(&signed, plan, &membership, None, &scope));
            let Poll::Ready(accepted) = accept.as_mut().poll(&mut cx) else {
                panic!("a backed-up owner must not make the receiver wait");
            };
            assert!(accepted.unwrap().is_none(), "the receiver plans HTTP");
            assert_eq!(
                metrics.count(crate::telemetry::Event::RdmaGrantOwnerQueueFull),
                1
            );
            assert_eq!(
                metrics.count(crate::telemetry::Event::RdmaGrantBackpressure),
                0
            );
            assert_eq!(metrics.count(crate::telemetry::Event::RdmaHttpFallback), 1);
            drop(accept);
            drop(queued);
            crate::rdma::links::tests::pump(&mut w.a.owner, &mut w.b.owner);
        }

        #[test]
        fn simulated_receiver_rejects_complete_from_cross_site_hop() {
            let env = racer_io::Env::host();
            for site in ["site2", ""] {
                let outcome = page_roundtrip(
                    &env,
                    PageCase {
                        receiver_site: Some(site),
                        ..PageCase::default()
                    },
                );
                assert!(matches!(outcome.result, Err(Error::Unauthorized)));
                assert_eq!(outcome.sent, Ok(true));
                assert_eq!(outcome.completions, 0);
                // The dropped arrival still returns its buffer.
                assert_eq!(outcome.free, (Some(2), Some(2)));
            }
        }

        /// The native receive reserves its landing charge through the
        /// transfer's reclaim hook: a full share fails `Overloaded` unless the
        /// hook frees idle bytes, and then the receive proceeds to its arrival.
        #[test]
        fn native_finish_reclaims_before_overloaded() {
            use crate::rdma::links::tests::drive;
            use crate::rdma::links::tests::link_up;
            use crate::rdma::links::tests::page;
            use crate::rdma::links::tests::world_for;
            use base64::Engine;
            let env = racer_io::Env::host();
            let signers = crate::peer::tests::signers(&env);
            let (a, b) = (signers[0].node().clone(), signers[2].node().clone());
            let mut w = world_for([a.clone(), b.clone()], false);
            let env = w.env.clone();
            let scope =
                RequestScope::host(RequestId([8; 16]), Instant::now() + Duration::from_secs(30))
                    .unwrap();
            link_up(&env, &mut w);
            let binding = Binding {
                request: [4; 32],
                response: [5; 32],
                transfer: TransferId([6; 16]),
                membership: 1,
                deadline: p::encode_deadline(scope.clock(), scope.deadline).unwrap(),
                rail: RailId(7),
            };
            let admission = w.a.admission.clone();
            let envelope = page(&admission, 35, 1).envelope().clone();
            let cache = envelope.page.version.object.cache.clone();
            // Stand-in for idle cache entries: hold the cache's whole share.
            let held = std::cell::RefCell::new(Vec::new());
            for shift in (0..48).rev() {
                while let Ok(charge) =
                    admission.reserve(Some(&cache), ResourceClass::Ciphertext, 1 << shift)
                {
                    held.borrow_mut().push(charge);
                }
            }
            let used = admission.used(ResourceClass::Ciphertext);
            let calls = std::cell::Cell::new(0);
            let busy = |_: &racer_wire::control::CacheId, _: usize| calls.set(calls.get() + 1);
            let idle = |owner: &racer_wire::control::CacheId, _: usize| {
                assert_eq!(owner, &cache);
                held.borrow_mut().clear();
            };
            let hooks: [(Option<&Reclaim<'_>>, bool); 3] =
                [(None, false), (Some(&busy), false), (Some(&idle), true)];
            for (reclaim, reclaimed) in hooks {
                let pending = drive(
                    &mut w.a.owner,
                    &mut w.b.owner,
                    w.a.links.grant(&b, RailId(7), binding.transfer, &scope),
                )
                .unwrap();
                let value = crate::rdma::links::completion_value(
                    pending.link(),
                    pending.tag(),
                    35,
                    binding.transfer,
                );
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
                if !reclaimed {
                    let result = drive(
                        &mut w.a.owner,
                        &mut w.b.owner,
                        pending.finish(&verified, envelope.clone(), &admission, reclaim, &scope),
                    );
                    assert!(matches!(result, Err(Error::Overloaded)));
                    assert_eq!(admission.used(ResourceClass::Ciphertext), used);
                    w.b.links.release(&a, &grant);
                } else {
                    // Admission passes; the lost arrival then fails `Io`.
                    let (epoch, _) = w.a.links.ready(&b, RailId(7)).unwrap();
                    w.a.links.abandon(&b, RailId(7), epoch);
                    let result = drive(
                        &mut w.a.owner,
                        &mut w.b.owner,
                        pending.finish(&verified, envelope.clone(), &admission, reclaim, &scope),
                    );
                    assert!(matches!(result, Err(Error::Io)));
                }
            }
            assert_eq!(calls.get(), 1);
            assert!(held.borrow().is_empty());
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }

        #[test]
        fn completion_must_name_the_grant_and_a_lost_arrival_is_io() {
            use crate::rdma::links::tests::drive;
            use crate::rdma::links::tests::link_up;
            use crate::rdma::links::tests::page;
            use crate::rdma::links::tests::world_for;
            use base64::Engine;
            let env = racer_io::Env::host();
            let signers = crate::peer::tests::signers(&env);
            let (a, b) = (signers[0].node().clone(), signers[2].node().clone());
            let mut w = world_for([a.clone(), b.clone()], false);
            let env = w.env.clone();
            let scope =
                RequestScope::host(RequestId([8; 16]), Instant::now() + Duration::from_secs(30))
                    .unwrap();
            // Before setup, the receiver cannot grant and the page uses HTTP.
            let early = drive(
                &mut w.a.owner,
                &mut w.b.owner,
                w.a.links.grant(&b, RailId(7), TransferId([6; 16]), &scope),
            );
            assert!(matches!(early, Err(Error::Unavailable)));
            assert!(native_failure(Error::Unavailable, &scope));
            link_up(&env, &mut w);
            let binding = Binding {
                request: [4; 32],
                response: [5; 32],
                transfer: TransferId([6; 16]),
                membership: 1,
                deadline: p::encode_deadline(scope.clock(), scope.deadline).unwrap(),
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
                        pending.finish(&verified, envelope.clone(), &admission, None, &scope),
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
                        pending.finish(&verified, envelope.clone(), &admission, None, &scope),
                    );
                    assert!(matches!(result, Err(Error::Io)));
                }
            }
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }
    /// Unkeyed receives land in the worker's pool, charged to the whole
    /// pooled size class, and later receives reuse the idle buffer.
    #[test]
    fn wire_unkeyed_receive_uses_pooled_storage_and_validates_bounds() {
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let buffers = crate::memory::BufferPool::new(admission.clone());
        let length = 1 << 20;
        let capacity = buffers.aligned_capacity(length).unwrap();
        let mut first = WireBuffer::unkeyed(&buffers, &admission, length).unwrap();
        assert_eq!(first.bytes().unwrap().len(), length);
        assert_eq!(admission.used(ResourceClass::Ciphertext), capacity);
        first.bytes_mut().unwrap()[..3].copy_from_slice(b"abc");
        let pointer = first.bytes().unwrap().as_ptr();
        drop(first);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        let second = WireBuffer::unkeyed(&buffers, &admission, length).unwrap();
        assert_eq!(second.bytes().unwrap().as_ptr(), pointer, "pool reuse");
        let crate::peer::protocol::ResponseBody::Aligned(body) = second.into_body() else {
            panic!("aligned body");
        };
        assert_eq!(body.charge().key(), None);
        assert_eq!(body.charge().amount(), capacity);
        drop(body);
        let empty = WireBuffer::unkeyed(&buffers, &admission, 0).unwrap();
        assert!(empty.bytes().unwrap().is_empty());
        assert!(matches!(
            empty.into_body(),
            crate::peer::protocol::ResponseBody::Empty
        ));
        assert!(matches!(
            WireBuffer::unkeyed(&buffers, &admission, (16 << 20) + 17),
            Err(Error::InvalidRequest)
        ));
        buffers.reclaim_idle_buffers();
        admission.stop();
        assert!(matches!(
            WireBuffer::unkeyed(&buffers, &admission, length),
            Err(Error::Unavailable)
        ));
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }
    /// Keyed TCP and HTTP peer bodies reserve through the production reclaim
    /// hook wired by `with_reclamation`, and fail `Overloaded` without it.
    #[test]
    fn transfers_keyed_receive_reclaims_before_overloaded() {
        let env = racer_io::Env::host();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(crate::runtime::Reactor::new(admission.clone(), &env));
        let signers = crate::peer::tests::signers(&env);
        let transfers = || {
            let io = Rc::new(crate::http::new_io(
                reactor.clone(),
                crate::http::Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
            ));
            Transfers::new(
                Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 1)),
                io,
                admission.clone(),
                Rc::new(crate::peer::protocol::SecurityCodec::new(
                    admission.clone(),
                    crate::memory::BufferPool::new(admission.clone()),
                )),
                signers[0].clone(),
            )
        };
        let cache = racer_wire::control::CacheId("11111111-4444-4444-8444-444444444444".into());
        let held = Rc::new(std::cell::RefCell::new(Vec::new()));
        for shift in (0..48).rev() {
            while let Ok(charge) =
                admission.reserve(Some(&cache), ResourceClass::Ciphertext, 1 << shift)
            {
                held.borrow_mut().push(charge);
            }
        }
        let used = admission.used(ResourceClass::Ciphertext);
        assert_eq!(
            transfers().reserve_ciphertext(&cache, 64).err(),
            Some(Error::Overloaded)
        );
        let requests = Rc::new(std::cell::Cell::new(None));
        let reclaiming = transfers().with_reclamation({
            let (held, requests) = (held.clone(), requests.clone());
            move |owner, bytes| {
                requests.set(Some((owner.clone(), bytes)));
                held.borrow_mut().clear();
            }
        });
        assert_eq!(admission.used(ResourceClass::Ciphertext), used);
        let charge = reclaiming.reserve_ciphertext(&cache, 64).unwrap();
        assert_eq!(charge.amount(), 64);
        assert_eq!(requests.take(), Some((cache, 64)));
        drop(charge);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }
    /// A body already on the wire waits briefly for ciphertext budget: it gets the budget once
    /// decode frees some, fails `Overloaded` when none frees within the bound, and passes other
    /// results through without waiting.
    #[test]
    fn transfers_keyed_receive_waits_for_ciphertext_budget() {
        use std::task::{Context, Poll};
        let env = racer_io::Env::host();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(crate::runtime::Reactor::new(admission.clone(), &env));
        let signers = crate::peer::tests::signers(&env);
        let metrics = crate::telemetry::Metrics::for_workers(1).unwrap().remove(0);
        let failures = crate::telemetry::Failures::default();
        let recorded = || {
            let mut out = String::new();
            failures.write(&mut out).unwrap();
            out
        };
        let transfers = |metrics: crate::telemetry::Metrics| {
            let io = Rc::new(crate::http::new_io(
                reactor.clone(),
                crate::http::Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
            ));
            Transfers::new(
                Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 1)),
                io,
                admission.clone(),
                Rc::new(crate::peer::protocol::SecurityCodec::new(
                    admission.clone(),
                    crate::memory::BufferPool::new(admission.clone()),
                )),
                signers[0].clone(),
            )
            .with_metrics(metrics)
        };
        fn drive<T>(
            reactor: &crate::runtime::Reactor,
            future: impl std::future::Future<Output = T>,
        ) -> T {
            let mut future = std::pin::pin!(future);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let watchdog = std::time::Instant::now() + Duration::from_secs(10);
            loop {
                if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                    return result;
                }
                assert!(std::time::Instant::now() < watchdog, "wait did not end");
                reactor.poll_budgeted(128).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
        }
        let scope = |seconds| {
            RequestScope::host(
                crate::model::RequestId([5; 16]),
                std::time::Instant::now() + Duration::from_secs(seconds),
            )
            .unwrap()
        };
        let cache = racer_wire::control::CacheId("11111111-4444-4444-8444-444444444444".into());
        // Free budget: granted at once, no wait counted.
        let free = transfers(metrics.clone());
        let charge = {
            let scope = scope(30);
            drive(
                &reactor,
                free.reserve_ciphertext_waiting(&cache, 64, &scope),
            )
            .unwrap()
        };
        assert_eq!(charge.amount(), 64);
        drop(charge);
        assert_eq!(metrics.count(Event::PeerReceiveCiphertextWaits), 0);
        let held = Rc::new(std::cell::RefCell::new(Vec::new()));
        for shift in (0..48).rev() {
            while let Ok(charge) =
                admission.reserve(Some(&cache), ResourceClass::Ciphertext, 1 << shift)
            {
                held.borrow_mut().push(charge);
            }
        }
        // Observe from here so the fixture's own filling is not counted.
        admission
            .policy()
            .set_observer(failures.observer(crate::model::WorkerId(0)));
        // Nothing frees: the wait ends `Overloaded` within its bound.
        let started = std::time::Instant::now();
        {
            let scope = scope(30);
            assert_eq!(
                drive(
                    &reactor,
                    free.reserve_ciphertext_waiting(&cache, 64, &scope)
                )
                .err(),
                Some(Error::Overloaded)
            );
        }
        assert!(started.elapsed() < CIPHERTEXT_WAIT * 4);
        // The wait retried many times but records one Admission failure, with
        // the worker's holder breakdown, when it gives up.
        let out = recorded();
        assert!(out.contains("total=1 "), "{out}");
        assert!(out.contains("stage=Admission"), "{out}");
        assert!(out.contains("holders: Some("), "{out}");
        assert!(out.contains("receive_waiting: 1 "), "{out}");
        assert_eq!(metrics.count(Event::PeerReceiveCiphertextWaits), 1);
        assert_eq!(metrics.count(Event::PeerReceiveCiphertextWaitGranted), 0);
        // Decode frees budget during the wait: the third reclaim (the first poll after the
        // initial attempt and its reclaim) releases it and the body is admitted.
        let calls = Rc::new(std::cell::Cell::new(0));
        let freeing = transfers(metrics.clone()).with_reclamation({
            let (held, calls) = (held.clone(), calls.clone());
            move |_, _| {
                calls.set(calls.get() + 1);
                if calls.get() == 3 {
                    held.borrow_mut().clear();
                }
            }
        });
        let charge = {
            let scope = scope(30);
            drive(
                &reactor,
                freeing.reserve_ciphertext_waiting(&cache, 64, &scope),
            )
            .unwrap()
        };
        assert_eq!(charge.amount(), 64);
        assert_eq!(calls.get(), 3);
        // A wait that is granted records no failure.
        assert!(recorded().contains("total=1 "));
        assert_eq!(metrics.count(Event::PeerReceiveCiphertextWaits), 2);
        assert_eq!(metrics.count(Event::PeerReceiveCiphertextWaitGranted), 1);
        drop(charge);
        // A stopped quota is not load: it fails at once without waiting.
        admission.stop();
        {
            let scope = scope(30);
            assert_eq!(
                drive(
                    &reactor,
                    free.reserve_ciphertext_waiting(&cache, 64, &scope)
                )
                .err(),
                Some(Error::Unavailable)
            );
        }
        assert_eq!(metrics.count(Event::PeerReceiveCiphertextWaits), 2);
    }
    /// Connect failures for want of a local resource are load on this node, not evidence that
    /// the peer is down; refusals, resets, and lost errnos keep their meaning.
    #[test]
    fn local_connect_exhaustion_is_overloaded_not_unreachable() {
        for errno in [
            libc::EADDRNOTAVAIL,
            libc::EADDRINUSE,
            libc::EMFILE,
            libc::ENFILE,
            libc::ENOBUFS,
            libc::ENOMEM,
            libc::EAGAIN,
        ] {
            assert_eq!(
                local_connect_overload(Error::Os(errno)),
                Error::Overloaded,
                "{errno}"
            );
        }
        for error in [
            Error::Os(libc::ECONNREFUSED),
            Error::Os(libc::ECONNRESET),
            Error::Os(libc::EHOSTUNREACH),
            Error::Os(libc::ETIMEDOUT),
            Error::Io,
            Error::Unavailable,
            Error::Overloaded,
            Error::Canceled,
        ] {
            assert_eq!(local_connect_overload(error), error);
        }
        // The walk treats the result as busy (retry the owner) rather than unreachable.
        assert_eq!(
            transport_outcome_of(local_connect_overload(Error::Os(libc::EADDRNOTAVAIL))),
            Some(racer_core::read::ProbeOutcome::Overloaded)
        );
        assert_eq!(
            transport_outcome_of(Error::Os(libc::ECONNREFUSED)),
            Some(racer_core::read::ProbeOutcome::Unreachable)
        );
    }
    fn transport_outcome_of(error: Error) -> Option<racer_core::read::ProbeOutcome> {
        racer_core::read::walk::transport_outcome(racer_core::read::Error::try_from(error).unwrap())
    }
    mod native_control_tests {
        use super::*;
        fn scope() -> RequestScope {
            RequestScope::host(
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
                deadline: p::encode_deadline(scope.clock(), scope.deadline).unwrap(),
                rail: RailId(7),
            }
        }
        fn copy(control: &SignedHead) -> SignedHead {
            let mut copy = p::decode_signed(&p::encode_signed(control).unwrap()).unwrap();
            peer_wire::resolve(&mut copy, &peer_wire::test_util::AnyChain).unwrap();
            copy
        }
        #[test]
        fn session_admitted_setup_grant_completion_still_require_exact_transfer_and_phase() {
            use crate::peer::protocol::tests::sessions::pair;
            use crate::peer::protocol::tests::sessions::signer;
            let env = racer_io::Env::host();
            let (mut sender, mut receiver) = pair(&env);
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
                let control =
                    unframe(admitted, phase.response(), &peer_wire::test_util::AnyChain).unwrap();
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
            let env = racer_io::Env::host();
            let nodes = crate::peer::tests::signers(&env);
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
            let mut head = original
                .head(&env.ctx.clock, Phase::Finish, &[9; 32], 0, vec![])
                .unwrap();
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
            let env = racer_io::Env::host();
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
                assert!(
                    binding
                        .head(&env.ctx.clock, phase, &[0; 32], length, extensions)
                        .is_err()
                );
            }
            assert!(
                binding
                    .head(&env.ctx.clock, Phase::Finish, &[0; 32], 17, vec![])
                    .is_ok()
            );
        }
    }
}
