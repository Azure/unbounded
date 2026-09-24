// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local HTTP/RDMA adapters; cache owns validated publication. Call
//! [`Handler::poll_background`] each application turn, including while HTTP is idle.
//! Backend HEAD and GET address the exact object target with identity encoding.
//! Origin and HTTP peer representations require a canonical checksum ETag.
//! Metadata requires Content-Length; Cache-Control/Age govern freshness.
//! Pages use aligned EOF-clipped Range, identity encoding and strong If-Match;
//! 206 requires matching Content-Range, 200 requires a full object page.
//! Peers carry bounded RD01/RR01/RB01 descriptors via HTTP or authenticated RDMA.
//! Only the selected owner accesses backend. RDMA failure retries same-hop HTTP
//! within the original candidate budget. Health/reuse wait for CRC validation.

use crate::http_auth::failure::{error_status, metric_failure, reported, validate_peer_report};
#[cfg(test)]
use crate::outcome::semantic_failure;
use crate::outcome::{
    AttemptFailure, AttemptRoute, OwnerUnavailable, PeerFailure, PeerReason, peer_failure,
};
use crate::{
    buffers::{BUFFER_SIZE, Destination},
    cache::{
        self, Cache, ExchangeProgress, Fault, Metadata, MetadataFault, Received, Upstream,
        UpstreamRequest, UpstreamResult,
        http_metadata::{checksum, identity_encoding, metadata_facts, page_facts, text},
        peer_wire::{
            MAX_CANDIDATE, MAX_DESCRIPTOR, budget_descriptor, descriptor, hex, routed_descriptor,
            unhex,
        },
    },
    http::{Headers, Progress},
    http_client as client, http_server as http, rdma,
    uring::{Ring, Work},
};
#[cfg(test)]
use cache::http_metadata::status;
use client::Endpoint;
use client::Origin as HttpOrigin;
use client::owner_health::{OwnerPermit, Owners};
use std::{
    cell::{RefCell, RefMut},
    collections::{BTreeMap, VecDeque},
    io,
    net::SocketAddr,
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant},
};

const WINDOW: usize = 2;
const COOLDOWN: Duration = Duration::from_secs(1);
const TIMEOUT: Duration = Duration::from_secs(30);
const RETURN_SLACK: Duration = Duration::from_millis(500);

mod attempt;
mod request;
mod response;
mod upstream;
use upstream::*;

fn metric_kind(request: &UpstreamRequest) -> crate::metrics::Kind {
    match request {
        UpstreamRequest::BackendMetadata(_) | UpstreamRequest::PeerMetadata(_) => {
            crate::metrics::Kind::Metadata
        }
        UpstreamRequest::BackendPage(_) | UpstreamRequest::PeerPage(_) => {
            crate::metrics::Kind::Page
        }
    }
}

use attempt::candidate_end;

pub(crate) fn remote_deadline(bytes: &[u8], local: Instant) -> io::Result<Instant> {
    let (_, remaining) = budget_descriptor(bytes)?;
    Ok(local.min(crate::environment::now() + remaining))
}

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
fn runnable() -> Work {
    Work {
        runnable: true,
        deadline: None,
    }
}
/// Opaque, affine owner-health completion authority for one pinned HTTP route.
pub struct Attempt {
    pub(crate) route: AttemptRoute,
    pub(crate) owner: Option<OwnerPermit>,
}
impl Attempt {
    pub(crate) fn owner_reachable(&mut self) {
        // A relay may answer from cache without contacting the owner. The affine
        // permit also fences candidate/routing identity and breaker generation.
        if self.route.final_hop
            && let Some(owner) = self.owner.take()
        {
            owner.success();
        }
    }
}
/// Origin transport plus a cache identity derived from its P2PCache UID.
#[derive(Clone)]
pub struct Backend {
    endpoint: Endpoint,
    namespace: cache::Namespace,
}
impl Backend {
    pub fn unix(path: &str, cache_id: &str) -> io::Result<Self> {
        Ok(Self {
            endpoint: Endpoint::unix(path)?,
            namespace: cache::Namespace::new(cache_id).map_err(cache::Error::into_io)?,
        })
    }
    pub fn new(address: &str, identity: &str) -> io::Result<Self> {
        let endpoint = Endpoint::parse(address)?;
        let namespace = cache::Namespace::new(identity).map_err(cache::Error::into_io)?;
        Ok(Self {
            endpoint,
            namespace,
        })
    }
    pub fn namespace(&self) -> cache::Namespace {
        self.namespace
    }
    /// Numeric HTTP destination.
    pub fn address(&self) -> SocketAddr {
        self.endpoint.address.tcp().expect("numeric backend")
    }
    /// Canonical numeric authority for the Host header.
    pub fn host(&self) -> &str {
        &self.endpoint.host
    }
}

/// Trusted HTTP peer with an optional authenticated, worker-local RDMA session.
/// Live configuration supplies only direct topology mappings.
pub struct Peer {
    http: HttpOrigin,
    rdma: Option<Rc<rdma::Connection>>,
    breaker: crate::breaker::CircuitBreaker,
    // Actual immediate transport failure, never inferred from breaker rejection.
    repair_evidence: Option<Instant>,
}
impl Peer {
    fn record_repair_failure(&mut self, permit: &crate::breaker::Permit) {
        if permit.current() {
            self.repair_evidence = Some(crate::environment::now());
        }
    }

    fn clear_repair_failure(&mut self, permit: &crate::breaker::Permit) {
        if permit.current() {
            self.repair_evidence = None;
        }
    }

    pub fn from_endpoint(endpoint: Endpoint) -> Self {
        Self {
            http: HttpOrigin::peer(endpoint),
            rdma: None,
            breaker: crate::breaker::CircuitBreaker::new(COOLDOWN),
            repair_evidence: None,
        }
    }
    pub fn new(http_address: &str, rdma: Option<rdma::Connection>) -> io::Result<Self> {
        Ok(Self {
            http: HttpOrigin::peer(Endpoint::parse(http_address)?),
            rdma: rdma.map(Rc::new),
            breaker: crate::breaker::CircuitBreaker::new(COOLDOWN),
            repair_evidence: None,
        })
    }
}

/// Sender placement hint. Namespace and chain validation precede cache faults.
pub(crate) fn routing_identity(headers: Headers<'_>) -> io::Result<Option<[u8; 32]>> {
    let Some(wire) = text(headers, "x-racer-fault")? else {
        return Ok(None);
    };
    let bytes = unhex(wire)?;
    let (cursor, _) = routed_descriptor(&bytes).map_err(cache::Error::into_io)?;
    Ok(cursor.map(|c| c.identity))
}

/// Request-local upstream context. Endpoint pools, negotiations and owner health
/// are shared across a volume; the selected route belongs to this request.
pub struct Provider {
    namespace: cache::Namespace,
    chain: Rc<RefCell<Chain>>,
    // Frozen for this local resolution: retries cannot enter a downstream rank
    // while an earlier child or canceled transport still holds resources.
    receive_rank: Option<usize>,
    repaired_candidate: bool,
    // Absolute consumer cap, distinct from the private candidate/service cap.
    caller_deadline: Option<Instant>,
    flight: u64,
    reply_route: Option<([u8; 32], u32)>,
    volume: Option<String>,
    metrics: crate::metrics::Local,
    authentication: Option<crate::http_auth::Policy>,
    max_attempts: u32,
    backend: Rc<RefCell<HttpOrigin>>,
    peer: Option<Rc<RefCell<Peer>>>,
    peers: Rc<RefCell<BTreeMap<String, Rc<RefCell<Peer>>>>>,
    selected: Option<String>,
    routing: Option<Arc<crate::routing::Routing>>,
    active: Option<Rc<RefCell<RouteState>>>,
    owners: Rc<RefCell<Owners>>,
    negotiations: Rc<RefCell<BTreeMap<String, String>>>,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Chain {
    hops: u8,
    work: u8,
}
impl Default for Chain {
    fn default() -> Self {
        Self {
            hops: cache::peer_wire::MAX_HOPS,
            work: cache::peer_wire::MAX_WORK,
        }
    }
}
impl Chain {
    fn forward(&mut self) -> io::Result<(u8, u8)> {
        if self.hops == 0 || self.work == 0 {
            return Err(io::Error::other("request chain exhausted"));
        }
        self.hops -= 1;
        // Split, never copy, per-resolution work between child and local recovery.
        // Unused remote work cannot be reclaimed without an authenticated return
        // protocol: failure/timeout does not prove the child stopped executing.
        let child = (self.work - 1) / 2;
        self.work -= child + 1;
        Ok((self.hops, child))
    }
}
#[derive(Clone)]
struct RouteState {
    cursor: crate::routing::Cursor,
    origin: bool,
    // Retain the last valid cursor for cache lookup/scoping, but grant no upstream.
    exhausted: bool,
}
/// Inline transport state. None of these states can publish a destination.
/// ```compile_fail
/// use racer_dataplane::{cache::Upstream, handlers::{Provider, Exchange}, uring::Ring};
/// fn reuse(provider: &mut Provider, exchange: Exchange, ring: &mut Ring) {
///     let _ = provider.poll(exchange, ring);
///     let _ = provider.poll(exchange, ring);
/// }
/// ```
#[allow(clippy::large_enum_variant)]
pub enum Exchange {
    Head(HeadPhase),
    Get(GetPhase),
    Grant(GrantPhase),
    Read(ReadPhase),
    /// The cache must reacquire both capabilities before resuming this HTTP retry.
    RecoverHttp(HttpRecovery),
    ValidateHttp(HttpValidation),
    ValidateRdma(RdmaValidation),
}
/// In-flight origin metadata request and its health authority.
pub struct HeadPhase {
    exchange: client::HeadExchange,
    permit: crate::breaker::Permit,
}
/// In-flight HTTP receive; publication remains owned by the cache.
pub struct GetPhase {
    exchange: HttpGet,
    request: UpstreamRequest,
    permit: crate::breaker::Permit,
    attempt: Option<Attempt>,
}
/// Same-hop fallback retaining only the request and owner-health authority.
pub struct HttpRecovery {
    request: UpstreamRequest,
    attempt: Option<Attempt>,
}
/// HTTP completion awaiting semantic/CRC validation before connection reuse.
pub struct HttpValidation {
    connection: Option<client::Connection>,
    permit: Option<crate::breaker::Permit>,
    attempt: Option<Attempt>,
}
/// RDMA completion retaining a same-hop HTTP fallback until validation.
pub struct RdmaValidation {
    request: UpstreamRequest,
    permit: Option<crate::breaker::Permit>,
    connection: Rc<rdma::Connection>,
    attempt: Option<Attempt>,
}
/// Pending RDMA grant, retaining unpublished destination ownership.
pub struct GrantPhase {
    attempt: Option<Attempt>,
    permit: crate::breaker::Permit,
    connection: Rc<rdma::Connection>,
    ticket: rdma::Ticket<rdma::Grant>,
    destination: Destination,
    request: UpstreamRequest,
    deadline: Instant,
}
/// Pending RDMA read, whose driver owns the unpublished destination.
pub struct ReadPhase {
    attempt: Option<Attempt>,
    checksum: Option<u64>,
    permit: crate::breaker::Permit,
    connection: Rc<rdma::Connection>,
    ticket: rdma::Ticket<rdma::DestinationRead>,
    request: UpstreamRequest,
    deadline: Instant,
}
impl Provider {
    fn forward_available(&self) -> cache::Result<()> {
        let chain = self.chain.borrow();
        if chain.hops == 0 || chain.work == 0 {
            return Err(cache::Error::Unavailable);
        }
        Ok(())
    }
    fn failure(&self, error: &cache::Error) -> PeerFailure {
        let local = self
            .active
            .as_ref()
            .map(|r| {
                let r = r.borrow();
                (
                    r.cursor.identity,
                    self.routing.as_ref().unwrap().destination(&r.cursor),
                )
            })
            .unwrap_or(([0; 32], 0));
        let reply = self.reply_route.unwrap_or(local);
        let mut failure = peer_failure(error, reply.0, reply.1);
        failure.identity = reply.0;
        failure.candidate = reply.1;
        if reply != local && failure.reason == PeerReason::OwnerUnavailable {
            // A different placement cannot attest to the sender's owner.
            failure.reason = PeerReason::Unavailable;
            failure.evidence = None;
        }
        failure
    }
    fn new(backend: Backend) -> Self {
        Self {
            namespace: backend.namespace,
            chain: Rc::new(RefCell::new(Chain::default())),
            receive_rank: None,
            repaired_candidate: false,
            caller_deadline: None,
            flight: 0,
            reply_route: None,
            volume: None,
            metrics: crate::metrics::Local::default(),
            authentication: None,
            max_attempts: 3,
            backend: Rc::new(RefCell::new(HttpOrigin::new(backend.endpoint))),
            peer: None,
            peers: Rc::new(RefCell::new(BTreeMap::new())),
            selected: None,
            routing: None,
            active: None,
            owners: Rc::new(RefCell::new(Owners::default())),
            negotiations: Rc::new(RefCell::new(BTreeMap::new())),
        }
    }
    fn reported(
        &mut self,
        failure: PeerFailure,
        attempt: &mut Option<Attempt>,
    ) -> cache::Result<()> {
        reported(failure, attempt)
    }
    #[cfg(test)]
    fn budget_wire(&self, request: &UpstreamRequest, end: Instant) -> io::Result<Vec<u8>> {
        let (wire, spent) = self.prepare_budget_wire(request, end)?;
        *self.chain.borrow_mut() = spent;
        Ok(wire)
    }
    /// Build an offer without spending it. The caller commits `spent` only after
    /// admission, before the first operation that could submit the offer.
    fn prepare_budget_wire(
        &self,
        request: &UpstreamRequest,
        end: Instant,
    ) -> io::Result<(Vec<u8>, Chain)> {
        // Benchmark fidelity: keep bench/fixture.rs framing aligned when changing
        // routed descriptors, budget accounting, or peer request headers.
        if cache::peer_wire::request_len(request, self.active.is_some(), true)?
            + cache::peer_wire::CHAIN_LEN
            > MAX_DESCRIPTOR
        {
            return Err(invalid("fault descriptor too large"));
        }
        let bytes = self.wire(request)?;
        if crate::environment::now() >= end {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "peer budget exhausted",
            ));
        }
        let remaining = end
            .saturating_duration_since(crate::environment::now())
            .saturating_sub(RETURN_SLACK);
        let bytes = cache::peer_wire::with_budget(bytes, remaining)?;
        let mut spent = *self.chain.borrow();
        let (hops, work) = spent.forward()?;
        let candidate = self.active.as_ref().map_or(0, |s| {
            self.routing
                .as_ref()
                .unwrap()
                .destination(&s.borrow().cursor)
        });
        Ok((
            cache::peer_wire::with_chain(bytes, *self.namespace.digest(), hops, work, candidate)?,
            spent,
        ))
    }
    fn wire(&self, request: &UpstreamRequest) -> io::Result<Vec<u8>> {
        let mut bytes = descriptor(request)?;
        if let Some(state) = &self.active {
            let (_, next) = self
                .routing
                .as_ref()
                .unwrap()
                .next(&state.borrow().cursor)?
                .ok_or_else(|| invalid("no next hop"))?;
            let mut routed = crate::routing::Cursor::MAGIC.to_vec();
            routed.extend(next.encode());
            routed.append(&mut bytes);
            bytes = routed;
        }
        if bytes.len() > MAX_DESCRIPTOR {
            return Err(invalid("fault descriptor too large"));
        }
        Ok(bytes)
    }
    fn rdma_failed(
        &mut self,
        request: UpstreamRequest,
        permit: crate::breaker::Permit,
        attempt: Option<Attempt>,
        connection: &rdma::Connection,
    ) -> ExchangeProgress<Exchange> {
        if connection.needs_http_recovery() {
            drop(permit);
        } else {
            permit.failure();
        }
        ExchangeProgress::RetryPeer {
            exchange: Exchange::RecoverHttp(HttpRecovery { request, attempt }),
        }
    }
}
impl Upstream for Provider {
    type Exchange = Exchange;
    fn start_metadata(
        &mut self,
        request: UpstreamRequest,
        deadline: Instant,
        _ring: &mut Ring,
    ) -> cache::Result<Exchange> {
        if self.active.as_ref().is_some_and(|s| s.borrow().exhausted) {
            return Err(cache::Error::Unavailable);
        }
        if matches!(request, UpstreamRequest::PeerMetadata(_)) {
            return self.http_peer_attempt(request, None, deadline, None);
        }
        let UpstreamRequest::BackendMetadata(meta) = request else {
            return Err(invalid("invalid metadata request").into());
        };
        if let (Some(routing), Some(state)) = (&self.routing, &self.active) {
            if routing.next(&state.borrow().cursor)?.is_some() {
                return Err(cache::Error::Unavailable);
            }
        }
        let (connection, permit) = self.backend.borrow_mut().connection()?;
        let service_end = deadline
            .checked_sub(RETURN_SLACK)
            .unwrap_or_else(crate::environment::now)
            .max(crate::environment::now());
        let exchange = connection
            .head(
                client::Request::new(meta.target(), &[("Accept-Encoding", "identity")])?
                    .with_authorization(meta.authorization()),
                service_end,
            )?
            .service_deadline(self.private_service_deadline(service_end, deadline))
            .retry_idle_backend(&self.metrics);
        self.metrics.upstream(
            crate::metrics::Upstream::BackendHttp,
            crate::metrics::Kind::Metadata,
        );
        Ok(Exchange::Head(HeadPhase { exchange, permit }))
    }
    fn proven_failure(&self, error: &cache::Error) -> bool {
        error.attempt_failure().is_some_and(|f| f.owner_evidence())
    }
    fn candidate_deadline(&mut self, caller: Instant) -> Instant {
        self.caller_deadline = Some(self.caller_deadline.map_or(caller, |old| old.min(caller)));
        let caller = self.caller_deadline.unwrap();
        let now = crate::environment::now();
        let Some(state) = &self.active else {
            return caller;
        };
        let state = state.borrow();
        if !state.origin {
            return caller;
        }
        let total = self
            .max_attempts
            .min(self.routing.as_ref().unwrap().candidate_count());
        let remaining = total.saturating_sub(state.cursor.attempt).max(1);
        candidate_end(now, caller, remaining)
    }
    fn network_scope(&self, value: [u8; 32]) -> Option<crate::buffers::NetworkFlightKey> {
        let routing = self.routing.as_ref()?;
        let state = self.active.as_ref()?.borrow();
        Some(crate::buffers::NetworkFlightKey {
            value,
            routing: state.cursor.identity,
            version: crate::routing::Cursor::WIRE_VERSION,
            destination: routing.destination(&state.cursor),
            dependency: crate::buffers::NetworkDependency::Independent(self.flight),
        })
    }
    fn peer_validated(&mut self, retry: &mut Exchange, valid: bool) {
        if let Exchange::ValidateRdma(RdmaValidation {
            permit,
            connection,
            attempt,
            ..
        }) = retry
        {
            if valid && let Some(a) = attempt.as_mut() {
                a.owner_reachable();
            }
            if let Some(permit) = permit.take() {
                if valid {
                    permit.success();
                } else {
                    permit.failure();
                    let _ = connection.disconnect();
                }
            }
        }
        if let Exchange::ValidateHttp(HttpValidation {
            connection,
            permit,
            attempt,
        }) = retry
        {
            if valid && let Some(attempt) = attempt.as_mut() {
                attempt.owner_reachable();
            }
            if valid {
                if let Some(peer) = self.peer.as_mut() {
                    let mut peer = peer.borrow_mut();
                    if let Some(permit) = permit.as_ref() {
                        peer.clear_repair_failure(permit);
                    }
                    peer.http.recycle(connection.take());
                }
            } else {
                connection.take();
            }
            if let Some(permit) = permit.take() {
                if valid {
                    permit.success();
                } else {
                    permit.failure();
                }
            }
        }
    }
    fn has_peer(&self) -> bool {
        self.peer.is_some()
    }
    fn receive_reserve(&mut self, capacity: usize) -> cache::Result<usize> {
        // The wire hop allowance decreases at every forwarding hop.
        // Only a fresh local payload may shorten it to fit this pool.
        // Received providers already have a frozen rank from peer_provider.
        let rank = *self.receive_rank.get_or_insert_with(|| {
            let mut chain = self.chain.borrow_mut();
            chain.hops = usize::from(chain.hops).min(capacity.saturating_sub(1)) as u8;
            usize::from(chain.hops)
        });
        if !self.has_peer() {
            return Ok(0);
        }
        self.forward_available()?;
        if rank >= capacity {
            return Err(cache::busy(
                "peer receive rank exceeds local buffer capacity",
            ));
        }
        Ok(rank)
    }
    fn peer_failed(&mut self, error: cache::Error) -> cache::Result<bool> {
        self.repaired_candidate = false;
        self.forward_available()?;
        let Some(state) = self.active.clone() else {
            return Ok(false);
        };
        let routing = self.routing.as_ref().unwrap();
        let mut state = state.borrow_mut();
        if state.exhausted {
            return Err(cache::Error::Unavailable);
        }
        let destination = routing.destination(&state.cursor);
        let failure = error.attempt_failure();
        // A typed, initiated immediate HTTP failure can repair an intermediate.
        // RDMA first recovers over the same peer's HTTP transport. Remote reports,
        // pressure, cancellations and content failures are never local link evidence.
        if let Some(failure) = failure
            && !failure.reported
            && !failure.route.final_hop
            && routing.compatible(&failure.route.cursor, &state.cursor)
            && failure.route.candidate == destination
            && failure.evidence.as_ref().is_some_and(|e| {
                e.transport == crate::outcome::Transport::Http
                    && e.endpoint.tcp() == Some(failure.route.endpoint)
                    && e.owner_evidence()
            })
            && let Ok(repaired) = routing.repair(&state.cursor)
        {
            state.cursor = repaired;
            self.repaired_candidate = true;
            drop(state);
            self.select_peer();
            return Ok(true);
        }
        let transport_failure = failure.is_some_and(|f| {
            f.owner_evidence()
                && routing.compatible(&f.route.cursor, &state.cursor)
                && f.route.candidate == destination
        });

        if !transport_failure {
            return Err(error);
        }
        if !state.origin {
            return Err(OwnerUnavailable(destination).into());
        }
        loop {
            if state.cursor.attempt + 1 >= routing.candidate_count().min(self.max_attempts) {
                return Err(cache::Error::Unavailable);
            }
            routing.advance_candidate(&mut state.cursor)?;
            if !self
                .owners
                .borrow()
                .blocked(state.cursor.identity, routing.destination(&state.cursor))
            {
                break;
            }
        }
        state.cursor.position = 0;

        drop(state);
        self.select_peer();
        Ok(true)
    }
    fn repaired_candidate(&self) -> bool {
        self.repaired_candidate
    }
    fn start(
        &mut self,
        request: UpstreamRequest,
        destination: Destination,
        deadline: Instant,
        _ring: &mut Ring,
    ) -> cache::Result<Exchange> {
        if matches!(
            request,
            UpstreamRequest::BackendMetadata(_) | UpstreamRequest::PeerMetadata(_)
        ) {
            return Err(invalid("metadata requires a storage-free exchange").into());
        }
        if self.active.as_ref().is_some_and(|s| s.borrow().exhausted) {
            return Err(cache::Error::Unavailable);
        }
        if matches!(
            request,
            UpstreamRequest::PeerMetadata(_) | UpstreamRequest::PeerPage(_)
        ) {
            // The candidate's downstream budget also serves same-peer HTTP
            // recovery. RDMA's shorter speculative grant/read cap is local;
            // it must not turn a healthy slow relay into final-owner evidence.
            self.forward_available()?;
            let mut attempt = None;
            let (key, len) = match &request {
                UpstreamRequest::PeerMetadata(m) => (m.key(), m.len()),
                UpstreamRequest::PeerPage(p) => (*p.key(), p.len()),
                _ => unreachable!(),
            };
            let peer = self.peer.as_ref().ok_or(cache::Error::Unavailable)?.clone();
            let rdma_wire = if peer.borrow().rdma.is_some() {
                let (wire, spent) =
                    self.prepare_budget_wire(&request, self.service_end(deadline))?;
                let Some(wire) =
                    crate::authorization::rdma_envelope(&wire, request.authorization())
                else {
                    return self.http_peer_attempt(request, Some(destination), deadline, None);
                };
                attempt = self.attempt(hex(blake3::hash(&wire).as_bytes()))?;
                Some((wire, spent))
            } else {
                None
            };
            let peer = peer.borrow_mut();
            if peer.http.breaker.active() + peer.breaker.active() >= peer.http.limit {
                return Err(cache::busy("direct peer exchange limit"));
            }
            // Faults can originate in an inbound RDMA task, without any HTTP
            // request on this relay. Notify runtime of the actual direct hop.
            if peer.rdma.is_none()
                && let Some(id) = &self.selected
            {
                let target = match &request {
                    UpstreamRequest::PeerMetadata(m) => m.target(),
                    UpstreamRequest::PeerPage(p) => p.target(),
                    _ => unreachable!(),
                };
                self.negotiations
                    .borrow_mut()
                    .entry(id.clone())
                    .or_insert_with(|| target.to_owned());
            }
            if let Some(connection) = &peer.rdma
                && let Ok(permit) = peer.breaker.try_acquire()
            {
                let (wire, spent) = rdma_wire.as_ref().unwrap();
                match connection.request_with_metadata(key, len, || {
                    // RDMA has reserved a request slot. Anything after this
                    // point may submit; even WouldBlock must not refund it.
                    *self.chain.borrow_mut() = *spent;
                    Ok(wire.as_slice())
                }) {
                    Ok(ticket) => {
                        self.metrics
                            .upstream(crate::metrics::Upstream::PeerRdma, metric_kind(&request));

                        return Ok(Exchange::Grant(GrantPhase {
                            attempt: attempt.take(),
                            permit,
                            connection: connection.clone(),
                            ticket,
                            destination,
                            request,
                            deadline: (crate::environment::now() + COOLDOWN).min(deadline),
                        }));
                    }
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                        drop(permit);
                        return Err(cache::busy("RDMA request capacity"));
                    }
                    Err(_) if connection.needs_http_recovery() => drop(permit),
                    Err(_) => permit.failure(),
                }
            }
            drop(peer);
            return self.http_peer_attempt(request, Some(destination), deadline, attempt);
        }
        if let (Some(routing), Some(state)) = (&self.routing, &self.active) {
            if routing.next(&state.borrow().cursor)?.is_some() {
                return Err(cache::Error::Unavailable);
            }
        }
        let (connection, permit) = self.backend.borrow_mut().connection()?;
        let kind = metric_kind(&request);
        let mut permit = Some(permit);
        // Reserve return slack inside the existing consumer budget.
        let service_end = deadline
            .checked_sub(RETURN_SLACK)
            .unwrap_or_else(crate::environment::now)
            .max(crate::environment::now());
        let result = (|| match &request {
            UpstreamRequest::BackendPage(page) => {
                let range = format!("bytes={}-{}", page.range().start(), page.range().end());
                let mut headers = vec![("Range", range.as_str()), ("Accept-Encoding", "identity")];
                let tag = page.checksum().etag();
                headers.push(("If-Match", tag.as_str()));
                Ok(Exchange::Get(GetPhase {
                    exchange: HttpGet::Payload(
                        connection
                            .get(
                                client::Request::new(page.target(), &headers)?
                                    .with_authorization(page.authorization()),
                                destination,
                                service_end,
                            )?
                            .service_deadline(self.private_service_deadline(service_end, deadline))
                            .retry_idle_backend(&self.metrics),
                    ),
                    request,
                    permit: permit.take().unwrap(),
                    attempt: None,
                }))
            }
            _ => unreachable!(),
        })();
        if let Err(error) = &result {
            HttpOrigin::error(permit.take().unwrap(), error, false);
        } else {
            self.metrics
                .upstream(crate::metrics::Upstream::BackendHttp, kind);
        }
        result
    }
    fn resume_peer(
        &mut self,
        exchange: Exchange,
        destination: Destination,
        deadline: Instant,
        _ring: &mut Ring,
    ) -> cache::Result<Exchange> {
        let (request, permit, connection, attempt) = match exchange {
            Exchange::RecoverHttp(HttpRecovery { request, attempt }) => {
                (request, None, None, attempt)
            }
            Exchange::ValidateRdma(RdmaValidation {
                request,
                permit,
                connection,
                attempt,
            }) => (request, permit, Some(connection), attempt),
            _ => return Err(invalid("invalid peer retry").into()),
        };
        // Also covers a successful READ rejected by cache semantic validation.
        if let Some(permit) = permit {
            permit.failure();
            if let Some(connection) = connection {
                let _ = connection.disconnect();
            }
        }
        self.http_peer_attempt(request, Some(destination), deadline, attempt)
    }
    fn poll(
        &mut self,
        exchange: Exchange,
        ring: &mut Ring,
    ) -> cache::Result<ExchangeProgress<Exchange>> {
        let pending = |exchange, work| Ok(ExchangeProgress::Pending { exchange, work });
        match exchange {
            Exchange::Head(phase) => self.poll_head(phase, ring),
            Exchange::Get(phase) => self.poll_get(phase, ring),
            Exchange::Grant(GrantPhase {
                mut attempt,
                permit,
                connection,
                mut ticket,
                destination,
                request,
                deadline,
            }) => match connection.take_reply(&mut ticket) {
                Ok(None) if crate::environment::now() >= deadline => {
                    drop((ticket, destination));
                    Ok(self.rdma_failed(request, permit, attempt, &connection))
                }
                Ok(None) => pending(
                    Exchange::Grant(GrantPhase {
                        attempt,
                        permit,
                        connection,
                        ticket,
                        destination,
                        request,
                        deadline,
                    }),
                    Work {
                        runnable: false,
                        deadline: Some(deadline),
                    },
                ),
                Ok(Some(reply @ (rdma::GrantReply::Failure(_) | rdma::GrantReply::Retry(_)))) => {
                    let (failure, retry) = match reply {
                        rdma::GrantReply::Failure(failure) => (failure, false),
                        rdma::GrantReply::Retry(failure) => (failure, true),
                        _ => unreachable!(),
                    };
                    let valid = attempt.as_ref().map_or(
                        failure.identity == [0; 32]
                            && failure.candidate == 0
                            && failure.reason != PeerReason::OwnerUnavailable,
                        |a| {
                            failure.identity == a.route.cursor.identity
                                && failure.candidate == a.route.candidate
                        },
                    );
                    if !valid {
                        let _ = connection.disconnect();
                        return Ok(self.rdma_failed(request, permit, attempt, &connection));
                    }
                    if retry {
                        // The authenticated, descriptor-bound rotation response is
                        // not owner failure. Cache recovery retains the original
                        // candidate deadline and reacquires private storage.
                        drop((permit, ticket, destination));
                        if let Some(peer) = &mut self.peer {
                            peer.borrow_mut().http.retire_idle();
                        }
                        return Ok(ExchangeProgress::RetryPeer {
                            exchange: Exchange::RecoverHttp(HttpRecovery { request, attempt }),
                        });
                    }
                    permit.success();
                    self.reported(failure, &mut attempt)?;
                    unreachable!();
                }
                Ok(Some(rdma::GrantReply::Grant(grant))) => {
                    if crate::environment::now() >= deadline {
                        drop((grant, destination));
                        return Ok(self.rdma_failed(request, permit, attempt, &connection));
                    }
                    let checksum = grant.checksum();
                    match connection.read(grant, destination) {
                        Ok(ticket) => pending(
                            Exchange::Read(ReadPhase {
                                attempt,
                                checksum,
                                permit,
                                connection,
                                ticket,
                                request,
                                deadline,
                            }),
                            runnable(),
                        ),
                        Err(rejected) => {
                            if rejected.error.kind() == io::ErrorKind::WouldBlock {
                                drop((rejected, permit));
                                return Err(cache::busy("RDMA READ capacity"));
                            }
                            drop(rejected);
                            Ok(self.rdma_failed(request, permit, attempt, &connection))
                        }
                    }
                }
                Err(_) => {
                    drop((ticket, destination));
                    Ok(self.rdma_failed(request, permit, attempt, &connection))
                }
            },
            Exchange::Read(ReadPhase {
                attempt,
                checksum,
                permit,
                connection,
                mut ticket,
                request,
                deadline,
            }) => {
                if crate::environment::now() >= deadline {
                    drop(ticket);
                    return Ok(self.rdma_failed(request, permit, attempt, &connection));
                }
                // Benchmark fidelity: rdma-bench stops here, before checksum
                // admission. Review its timing boundary if completion moves.
                match connection.take_read_unpublished(&mut ticket) {
                    Ok(None) => pending(
                        Exchange::Read(ReadPhase {
                            attempt,
                            checksum,
                            permit,
                            connection,
                            ticket,
                            request,
                            deadline,
                        }),
                        Work {
                            runnable: false,
                            deadline: Some(deadline),
                        },
                    ),
                    Ok(Some((destination, len))) => {
                        let received = Received {
                            destination,
                            len,
                            checksum,
                        };
                        let result = match &request {
                            UpstreamRequest::PeerMetadata(_) => {
                                return Err(
                                    invalid("metadata cannot use RDMA payload storage").into()
                                );
                            }
                            UpstreamRequest::PeerPage(_) => UpstreamResult::PeerPage(received),
                            _ => return Err(invalid("invalid RDMA result kind").into()),
                        };
                        Ok(ExchangeProgress::ReadyPeer {
                            result,
                            retry: Exchange::ValidateRdma(RdmaValidation {
                                request,
                                permit: Some(permit),
                                connection,
                                attempt,
                            }),
                        })
                    }
                    Err(_) => {
                        drop(ticket);
                        Ok(self.rdma_failed(request, permit, attempt, &connection))
                    }
                }
            }
            Exchange::RecoverHttp(..) | Exchange::ValidateHttp(..) | Exchange::ValidateRdma(..) => {
                Err(invalid("peer retry requires a fresh destination").into())
            }
        }
    }
}

/// HTTP application for one volume on one worker.
pub struct Handler {
    maintenance: bool,
    draining: bool,
    rdma_task_limit: usize,
    crypto: Option<Rc<RefCell<crate::crypto::Worker>>>,
    cache: Rc<RefCell<Cache>>,
    namespace: cache::Namespace,
    upstream: Provider,
    incoming: VecDeque<RdmaTask>,
    connections: Vec<Rc<rdma::Connection>>,
    peer_cursor: usize,
}
impl Handler {
    fn peer_provider(&self, bytes: &[u8]) -> cache::Result<Provider> {
        let (cursor, descriptor) = routed_descriptor(bytes)?;
        let chain = cache::peer_wire::chain(bytes)?;
        if chain.is_none() || cursor.is_none() {
            return Err(invalid("missing bounded request chain or cursor").into());
        }
        if chain.is_some_and(|(namespace, _, _, _)| namespace != *self.namespace.digest()) {
            return Err(invalid("foreign cache namespace").into());
        }
        let reply = cursor
            .as_ref()
            .zip(chain)
            .map(|(c, (_, _, _, candidate))| (c.identity, candidate));
        let mut provider = self.upstream.routed(self.upstream.route_state(
            cursor,
            &descriptor.key(self.namespace)?,
            true,
        )?);
        provider.reply_route = reply;
        if let (Some(routing), Some(state), Some((_, _, _, candidate))) =
            (&provider.routing, &provider.active, chain)
            && routing.destination(&state.borrow().cursor) != candidate
        {
            return Err(invalid("product chain candidate mismatch").into());
        }
        if let Some((_, hops, work, _)) = chain {
            provider.chain = Rc::new(RefCell::new(Chain { hops, work }));
            provider.receive_rank = Some(usize::from(hops));
        }
        Ok(provider)
    }
    pub fn set_authentication(&mut self, policy: crate::http_auth::Policy) {
        self.upstream.authentication = Some(policy);
    }
    pub(crate) fn set_peer_tls(
        &mut self,
        volume: &str,
        provider: Arc<crate::control::credentials::Provider>,
        identities: &BTreeMap<String, crate::tls::PeerIdentity>,
    ) {
        self.upstream.volume = Some(volume.to_owned());
        if let (Some(id), Some(peer)) = (&self.upstream.selected, &mut self.upstream.peer)
            && let Some(identity) = identities.get(id)
        {
            peer.borrow_mut()
                .http
                .set_tls(provider.clone(), identity.clone());
        }
        for (id, peer) in self.upstream.peers.borrow().iter() {
            if let Some(identity) = identities.get(id) {
                peer.borrow_mut()
                    .http
                    .set_tls(provider.clone(), identity.clone());
            }
        }
    }
    pub(crate) fn reject(&mut self, request: http::Request, status: u16) -> Task {
        let mut task = <Self as http::Handler>::start(self, request);
        task.fault = None;
        task.failure = Some(status);
        task
    }
}
impl Provider {
    fn route_state(
        &self,
        cursor: Option<crate::routing::Cursor>,
        key: &[u8; 32],
        peer: bool,
    ) -> io::Result<Option<Rc<RefCell<RouteState>>>> {
        let Some(routing) = &self.routing else {
            return if cursor.is_some() {
                Err(invalid("unexpected topology"))
            } else {
                Ok(None)
            };
        };
        let origin = !peer;
        let mut cursor = match cursor {
            Some(cursor) if peer => routing.receive(cursor, key)?,
            None if !peer => routing.start_key(key),
            _ => return Err(invalid("missing topology cursor")),
        };
        let mut exhausted = false;
        if origin {
            while self
                .owners
                .borrow()
                .blocked(cursor.identity, routing.destination(&cursor))
            {
                if cursor.attempt + 1 >= routing.candidate_count().min(self.max_attempts) {
                    exhausted = true;
                    break;
                }
                routing.advance_candidate(&mut cursor)?;
            }
        }
        routing.validate(&cursor, key)?;

        Ok(Some(Rc::new(RefCell::new(RouteState {
            cursor,
            origin,
            exhausted,
        }))))
    }
}
impl Handler {
    pub fn set_crypto(&mut self, crypto: Option<Rc<RefCell<crate::crypto::Worker>>>) {
        self.crypto = crypto;
    }
    pub fn new(cache: Cache, backend: Backend) -> Self {
        let namespace = backend.namespace();
        Self::shared(Rc::new(RefCell::new(cache)), backend, namespace)
    }
    pub fn shared(
        cache: Rc<RefCell<Cache>>,
        backend: Backend,
        namespace: cache::Namespace,
    ) -> Self {
        let metrics = cache.borrow().metrics().clone();
        Self {
            rdma_task_limit: 64,
            maintenance: false,
            draining: false,
            cache,
            crypto: None,
            namespace,
            upstream: Provider {
                metrics,
                namespace,
                ..Provider::new(backend)
            },
            incoming: VecDeque::new(),
            connections: Vec::new(),
            peer_cursor: 0,
        }
    }
    /// Direct access for cache-specific configuration. Worker maintenance should
    /// call `poll_background`, which also services inbound RDMA.
    pub fn cache_mut(&mut self) -> RefMut<'_, Cache> {
        self.cache.borrow_mut()
    }
    #[cfg(test)]
    pub(crate) fn test_authentication(&mut self, node: u8, peers: &[u8], selected: Option<u8>) {
        let (trust, _) = crate::control::tests::fixture();
        self.set_authentication(crate::http_auth::Policy {
            members: Arc::new(
                peers
                    .iter()
                    .map(|peer| ([*peer; 32], ("metadata-test-pod".into(), String::new())))
                    .collect(),
            ),
            universe: trust.universe,
            node: [node; 32],
        });
        self.upstream.selected = selected.map(|peer| hex(&[peer; 32]));
    }

    pub fn set_peer(&mut self, peer: Peer) {
        if let Some(connection) = &peer.rdma {
            self.add_shared_connection(connection.clone());
        }
        self.upstream.peer = Some(Rc::new(RefCell::new(peer)));
    }
    pub fn set_routing(
        &mut self,
        routing: Arc<crate::routing::Routing>,
        peers: BTreeMap<String, Peer>,
    ) {
        self.upstream.routing = Some(routing);
        self.upstream.peers = Rc::new(RefCell::new(
            peers
                .into_iter()
                .map(|(id, peer)| (id, Rc::new(RefCell::new(peer))))
                .collect(),
        ));
    }
    pub(crate) fn peer_metrics(&self, volume: &str, out: &mut Vec<crate::metrics::PeerState>) {
        for (id, peer) in self.upstream.peers.borrow().iter().chain(
            self.upstream
                .selected
                .as_ref()
                .zip(self.upstream.peer.as_ref()),
        ) {
            let peer = peer.borrow();
            out.push(crate::metrics::PeerState {
                volume: volume.to_owned(),
                peer: id.clone(),
                http: peer.http.breaker.status(),
                rdma: peer.breaker.status(),
                prefer_rdma: peer.rdma.as_ref().is_some_and(|c| c.is_healthy())
                    && peer.breaker.available(),
            });
        }
    }
    pub fn set_attempt_policy(&mut self, max_attempts: u32) -> io::Result<()> {
        if !(1..=8).contains(&max_attempts) {
            return Err(invalid("max attempts must be in 1..=8"));
        }
        self.upstream.max_attempts = max_attempts;
        Ok(())
    }
    /// Per-generation inbound RDMA work and per-direct-peer exchanges across
    /// HTTP/RDMA (also the backend HTTP limit). Call after configuring peers.
    /// Cache limits separately bound aggregate work across retained generations.
    pub fn set_resource_limits(
        &mut self,
        rdma_tasks: usize,
        http_per_endpoint: usize,
    ) -> io::Result<()> {
        if rdma_tasks == 0 || http_per_endpoint == 0 {
            return Err(invalid("zero resource limit"));
        }
        self.rdma_task_limit = rdma_tasks;
        self.upstream.backend.borrow_mut().limit = http_per_endpoint;
        for peer in self
            .upstream
            .peers
            .borrow()
            .values()
            .chain(self.upstream.peer.iter())
        {
            peer.borrow_mut().http.limit = http_per_endpoint;
        }
        Ok(())
    }
    pub fn set_routed_connection(&mut self, id: &str, connection: Rc<rdma::Connection>) {
        self.add_shared_connection(connection.clone());
        let peer = if self.upstream.selected.as_deref() == Some(id) {
            self.upstream.peer.clone()
        } else {
            self.upstream.peers.borrow().get(id).cloned()
        };
        if let Some(peer) = peer {
            peer.borrow_mut().rdma = Some(connection);
        }
    }
    pub(crate) fn take_negotiations(&mut self) -> BTreeMap<String, String> {
        std::mem::take(&mut *self.upstream.negotiations.borrow_mut())
    }
    /// Upgrade/downgrade transport without resetting the HTTP endpoint pool or
    /// either endpoint breaker. Negotiation retry policy belongs to runtime.
    pub fn set_peer_connection(&mut self, connection: Option<Rc<rdma::Connection>>) {
        if let Some(connection) = &connection {
            self.add_shared_connection(connection.clone());
        }
        if let Some(peer) = &mut self.upstream.peer {
            peer.borrow_mut().rdma = connection;
        }
    }
    pub fn add_shared_connection(&mut self, connection: Rc<rdma::Connection>) {
        if !self.connections.iter().any(|c| Rc::ptr_eq(c, &connection)) {
            self.connections.push(connection);
        }
    }
    /// Remove a retired session and its pending inbound work. Does not shut
    /// down the shared cache used by other volume generations.
    pub fn remove_connection(&mut self, connection: &Rc<rdma::Connection>) {
        for peer in self.upstream.peers.borrow().values() {
            let mut peer = peer.borrow_mut();
            if peer
                .rdma
                .as_ref()
                .is_some_and(|c| Rc::ptr_eq(c, connection))
            {
                peer.rdma = None;
            }
        }
        self.connections.retain(|c| !Rc::ptr_eq(c, connection));
        self.incoming
            .retain(|t| !Rc::ptr_eq(&t.connection, connection));
        if let Some(peer) = &mut self.upstream.peer
            && peer
                .borrow()
                .rdma
                .as_ref()
                .is_some_and(|c| Rc::ptr_eq(c, connection))
        {
            peer.borrow_mut().rdma = None;
        }
    }
    pub fn add_connection(&mut self, connection: rdma::Connection) {
        self.connections.push(Rc::new(connection));
    }
    pub(crate) fn begin_drain(&mut self) {
        self.draining = true;
    }
    pub(crate) fn maintenance(&mut self, enabled: bool) {
        self.maintenance = enabled;
    }

    /// Bounded round-robin disk and inbound RDMA service, even with no HTTP tasks.
    pub fn poll_background(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
        for peer in self
            .upstream
            .peers
            .borrow()
            .values()
            .chain(self.upstream.peer.iter())
        {
            peer.borrow_mut().http.maintain();
        }
        let mut cache = self.cache.borrow_mut();
        let mut work = cache.poll(ring, budget).map_err(cache::Error::into_io)?;
        if budget == 0 {
            return Ok(work);
        }
        if !self.draining && !self.connections.is_empty() {
            let connection = self.connections[self.peer_cursor % self.connections.len()].clone();
            self.peer_cursor = (self.peer_cursor + 1) % self.connections.len();
            if let Ok(Some(request)) = connection.next_request() {
                self.upstream
                    .metrics
                    .request(crate::metrics::Traffic::PeerRdma);
                if let Ok((wire, authorization)) =
                    crate::authorization::rdma_decode(&request.metadata)
                    && let Ok((_, descriptor)) = routed_descriptor(wire)
                    && let descriptor = descriptor.with_expected(request.value, request.len)
                    && let Ok(upstream) = self.peer_provider(wire)
                    && let Ok(deadline) = remote_deadline(wire, crate::environment::now() + TIMEOUT)
                {
                    let admitted = if self.maintenance {
                        Err(cache::busy("storage maintenance"))
                    } else if self.incoming.len() >= self.rdma_task_limit {
                        Err(cache::busy("inbound RDMA task limit"))
                    } else {
                        cache.peer_fault_in(
                            &cache::Context::new(self.namespace)
                                .with_crypto(self.crypto.clone())
                                .with_authorization(authorization),
                            descriptor,
                            deadline,
                        )
                    };
                    match admitted {
                        Ok(fault) => {
                            self.incoming.push_back(RdmaTask {
                                connection,
                                request,
                                fault,
                                upstream,
                            });
                        }
                        Err(error) => {
                            let _ = connection.respond_error(request, upstream.failure(&error));
                        }
                    }
                } else {
                    let _ = connection.respond_error(
                        request,
                        PeerFailure {
                            response: Default::default(),
                            identity: [0; 32],
                            candidate: 0,
                            reason: PeerReason::Protocol,
                            evidence: None,
                        },
                    );
                }
                work.runnable = true;
            }
            if self.connections.len() > 1 {
                work.merge(Work {
                    runnable: false,
                    deadline: Some(crate::environment::now() + Duration::from_millis(1)),
                });
            }
        }
        for _ in 0..budget.min(self.incoming.len()) {
            let RdmaTask {
                connection,
                request,
                fault,
                mut upstream,
            } = self.incoming.pop_front().unwrap();
            if !connection.request_live(&request) {
                work.runnable = true;
                continue;
            }
            match cache.poll_fault(fault, ring, &mut upstream) {
                Ok(cache::Progress::Ready(buffer)) => {
                    let _ = connection.respond(request, buffer);
                    work.runnable = true;
                }
                Ok(cache::Progress::Pending { fault, work: w }) => {
                    work.merge(w);
                    self.incoming.push_back(RdmaTask {
                        connection,
                        request,
                        fault,
                        upstream,
                    });
                }
                Err(error) => {
                    let _ = connection.respond_error(request, upstream.failure(&error));
                    work.runnable = true;
                }
            }
        }
        if !self.incoming.is_empty() {
            work.merge(Work {
                runnable: false,
                deadline: self.incoming.iter().map(|t| t.fault.deadline()).min(),
            });
            if self.incoming.len() > budget {
                work.merge(Work {
                    runnable: false,
                    deadline: Some(crate::environment::now() + Duration::from_millis(1)),
                });
            }
        }
        Ok(work)
    }
    pub fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()> {
        self.incoming.clear();
        self.cache
            .borrow_mut()
            .shutdown(ring)
            .map_err(cache::Error::into_io)
    }
}
struct RdmaTask {
    upstream: Provider,
    connection: Rc<rdma::Connection>,
    request: rdma::Request,
    fault: Fault<Provider>,
}

#[allow(clippy::large_enum_variant)]
enum Response {
    Request(http::Request),
    Head(http::SendingHeadHeaders),
    Headers(http::SendingGetHeaders),
    Writer(http::BodyWriter),
    Body(http::SendingBody),
    Done,
}
// Keep cache fault state inline; routed pages pin an independent retry cursor.
#[allow(clippy::large_enum_variant)]
enum PageLoad {
    Loading(Fault<Provider>, Provider),
    Ready(cache::CachedValue),
    Taken,
}
enum Initial {
    Rejected(cache::Error),
    Metadata(MetadataFault<Provider>),
    Peer(Fault<Provider>),
}
struct PendingHead {
    status: u16,
    len: u64,
    headers: Vec<(String, Vec<u8>)>,
}
/// Opaque affine HTTP task.
/// ```compile_fail
/// use racer_dataplane::handlers::Task;
/// fn duplicate(task: Task) { let _copy = task.clone(); }
/// ```
#[must_use]
pub struct Task {
    // Includes streaming gaps with resolved metadata but no current Fault.
    // Rejected maintenance requests never acquire a cache-use guard.
    _cache_use: Option<Rc<()>>,
    upstream: Provider,
    response: Response,
    metadata: Option<Metadata>,
    fault: Option<Initial>,
    pages: VecDeque<(u64, PageLoad)>,
    position: u64,
    end: u64,
    next: u64,
    peer: bool,
    distributed: bool,
    deadline: Instant,
    response_deadline: http::Deadline,
    failure: Option<u16>,
    head: Option<PendingHead>,
    metric_peer: bool,
    error_metric: Option<crate::metrics::HttpFailure>,
    headers_sent: bool,
}
impl Task {
    fn respond(&mut self, status: u16, len: u64, headers: &[(&str, &[u8])]) -> io::Result<()> {
        let Response::Request(request) = std::mem::replace(&mut self.response, Response::Done)
        else {
            return Err(invalid("response already started"));
        };
        let head = http::ResponseHead::new(status, Some(len), headers)?;
        self.response = match request {
            http::Request::Get(request) => Response::Headers(request.respond(head)?),
            http::Request::Head(request) => Response::Head(request.respond(head)?),
        };
        if status >= 400 && self.error_metric.is_none() {
            self.error_metric = Some(crate::metrics::HttpFailure {
                reason: crate::metrics::HttpErrorReason::status(status),
                pressure: None,
            });
        }
        Ok(())
    }
    fn sent_headers(&mut self, metrics: &crate::metrics::Local) {
        self.headers_sent = true;
        if let Some(failure) = self.error_metric.take() {
            metrics.http_failure(self.metric_peer, false, failure);
        }
    }
    fn prefetch(&mut self, cache: &mut Cache, ring: &mut Ring) -> cache::Result<Work> {
        if self.peer
            || matches!(
                self.response,
                Response::Head(_) | Response::Request(http::Request::Head(_))
            )
        {
            return Ok(Work::default());
        }
        let mut work = Work::default();
        if self.pages.len() < WINDOW && self.next < self.end {
            let leading_owns_buffer = self.pages.iter().all(|(_, page)| match page {
                PageLoad::Ready(_) => true,
                PageLoad::Loading(fault, _) => fault.can_prefetch(),
                PageLoad::Taken => false,
            });
            if leading_owns_buffer {
                // One finite resolution budget per newly admitted page. Aggregate
                // work is bounded by the metadata's page count and stream deadline;
                // existing faults/candidates never have their caps renewed.
                let fault = cache.page(
                    self.metadata.as_ref().unwrap(),
                    self.next,
                    self.response_deadline
                        .get()
                        .min(crate::environment::now() + TIMEOUT),
                )?;
                let offset = self.next;
                self.next += fault.len() as u64;
                let upstream = self.upstream.page_provider(fault.key())?;
                self.pages
                    .push_back((offset, PageLoad::Loading(fault, upstream)));
                work.runnable = true;
            }
        }
        // At most two faults per turn; keep reading while SEND_ZC waits.
        for (_, page) in &mut self.pages {
            if matches!(page, PageLoad::Loading(..)) {
                let PageLoad::Loading(fault, mut upstream) =
                    std::mem::replace(page, PageLoad::Taken)
                else {
                    unreachable!()
                };
                match cache.poll_value(fault, ring, &mut upstream)? {
                    cache::Progress::Pending { fault, work: w } => {
                        *page = PageLoad::Loading(fault, upstream);
                        work.merge(w);
                    }
                    cache::Progress::Ready(buffer) => {
                        *page = PageLoad::Ready(buffer);
                        work.runnable = true;
                    }
                }
            }
        }
        Ok(work)
    }
}
impl http::Handler for Handler {
    type Task = Task;
    fn start(&mut self, request: http::Request) -> Task {
        self.start_request(request)
    }
    fn poll(
        &mut self,
        task: &mut Task,
        ring: &mut Ring,
        budget: usize,
    ) -> io::Result<Progress<http::Completed>> {
        match self.poll_http(task, ring, budget) {
            Err(error) => {
                if std::mem::take(&mut task.headers_sent) {
                    let error = cache::Error::from(error);
                    self.upstream.metrics.http_failure(
                        task.metric_peer,
                        true,
                        metric_failure(&error),
                    );
                    return Err(error.into_io());
                }
                Err(error)
            }
            Ok(Progress::Ready(done)) => {
                task.headers_sent = false;
                Ok(Progress::Ready(done))
            }
            pending => pending,
        }
    }
}
impl Handler {
    fn poll_http(
        &mut self,
        task: &mut Task,
        ring: &mut Ring,
        budget: usize,
    ) -> io::Result<Progress<http::Completed>> {
        let mut cache = self.cache.borrow_mut();
        if budget == 0 {
            return Ok(Progress::Pending(runnable()));
        }
        if let Some(status) = task.failure.take() {
            task.respond(status, 0, &[])?;
            return Ok(Progress::Pending(runnable()));
        }
        if let Some(fault) = task.fault.take() {
            let result: cache::Result<()> = match fault {
                Initial::Rejected(error) => Err(error),
                Initial::Metadata(fault) => {
                    match cache.poll_metadata(fault, ring, &mut task.upstream) {
                        Ok(cache::Progress::Ready(meta)) => task.prepare(meta).map_err(Into::into),
                        Ok(cache::Progress::Pending { fault, work }) => {
                            task.fault = Some(Initial::Metadata(fault));
                            return Ok(Progress::Pending(work));
                        }
                        Err(error) => Err(error),
                    }
                }
                Initial::Peer(fault) => {
                    let identity = fault.representation_checksum();
                    let content_type = fault.content_type();
                    match cache.poll_value(fault, ring, &mut task.upstream) {
                        Ok(cache::Progress::Ready(buffer)) => {
                            task.end = buffer.len() as u64;
                            let checksum = format!(
                                "{:016x}",
                                buffer
                                    .checksum()
                                    .ok_or_else(|| invalid("missing retained checksum"))?
                            );
                            let identity = match identity {
                                Some(identity) => identity,
                                None => match &buffer {
                                    cache::CachedValue::Metadata(record) => record.checksum,
                                    _ => {
                                        return Err(invalid(
                                            "metadata must resolve to a typed record",
                                        ));
                                    }
                                },
                            };
                            let etag = identity.etag();
                            task.pages.push_back((0, PageLoad::Ready(buffer)));
                            let mut headers = vec![
                                ("X-Racer-Crc64", checksum.as_bytes()),
                                ("ETag", etag.as_str().as_bytes()),
                            ];
                            let content_type = match task.pages.back() {
                                Some((
                                    _,
                                    PageLoad::Ready(cache::CachedValue::Metadata(record)),
                                )) => record.content_type,
                                _ => content_type,
                            };
                            if let Some(value) = content_type.as_bytes() {
                                headers.push(("Content-Type", value));
                            }
                            task.respond(200, task.end, &headers).map_err(Into::into)
                        }
                        Ok(cache::Progress::Pending { fault, work }) => {
                            task.fault = Some(Initial::Peer(fault));
                            return Ok(Progress::Pending(work));
                        }
                        Err(error) => Err(error),
                    }
                }
            };
            if let Err(error) = result {
                task.head = None;
                task.pages.clear();
                task.position = 0;
                task.end = 0;
                task.next = 0;
                let failure = if task.peer {
                    task.upstream
                        .active
                        .as_ref()
                        .map(|_| hex(&task.upstream.failure(&error).encode()))
                } else {
                    None
                };
                let context = if task.peer {
                    if let Response::Request(request) = &task.response {
                        text(request.headers(), "x-racer-attempt")?
                            .filter(|v| v.len() == 96 && v.bytes().all(|b| b.is_ascii_hexdigit()))
                            .map(str::to_owned)
                    } else {
                        None
                    }
                } else {
                    None
                };
                let mut headers = Vec::new();
                let response_metadata = error
                    .evidence()
                    .semantic
                    .map(|f| f.response)
                    .unwrap_or(error.evidence().response);
                if let Some(value) = response_metadata.challenge.as_bytes() {
                    headers.push(("WWW-Authenticate", value));
                }
                if let Some(value) = response_metadata.retry_after.as_bytes() {
                    headers.push(("Retry-After", value));
                }
                if let (Some(failure), Some(context)) = (&failure, &context) {
                    headers.push(("X-Racer-Failure", failure.as_bytes()));
                    headers.push(("X-Racer-Attempt", context.as_bytes()));
                }
                let status = if let Some(failure) = &failure {
                    let reported = io::Error::other(PeerFailure::decode(&unhex(failure)?)?).into();
                    task.error_metric = Some(metric_failure(&reported));
                    error_status(&reported)
                } else {
                    task.error_metric = Some(metric_failure(&error));
                    error_status(&error)
                };
                // Local admission detail is not carried by the peer wire reason.
                if let Some(metric) = &mut task.error_metric {
                    metric.pressure = metric_failure(&error).pressure;
                }
                task.respond(status, 0, &headers)?;
            }
            return Ok(Progress::Pending(runnable()));
        }
        let page_work = match task.prefetch(&mut cache, ring) {
            Ok(work) => work,
            Err(error) if matches!(task.response, Response::Request(_)) => {
                task.pages.clear();
                task.head = None;
                task.end = 0;
                task.next = 0;
                task.error_metric = Some(metric_failure(&error));
                let response_metadata = error
                    .evidence()
                    .semantic
                    .map(|f| f.response)
                    .unwrap_or(error.evidence().response);
                let mut headers = Vec::new();
                if let Some(value) = response_metadata.challenge.as_bytes() {
                    headers.push(("WWW-Authenticate", value));
                }
                if let Some(value) = response_metadata.retry_after.as_bytes() {
                    headers.push(("Retry-After", value));
                }
                task.respond(error_status(&error), 0, &headers)?;
                return Ok(Progress::Pending(runnable()));
            }
            Err(error) => return Err(error.into_io()),
        };
        task.poll_response(ring, page_work)
    }
}

#[cfg(test)]
#[path = "../tests/http/handlers.rs"]
mod tests;
