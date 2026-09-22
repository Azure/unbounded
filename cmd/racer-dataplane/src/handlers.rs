// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local HTTP/RDMA adapters; cache owns validated publication. Call
//! [`Handler::poll_background`] each application turn, including while HTTP is idle.
//! Backend HEAD and GET address the exact object target with identity encoding.
//! Origin and HTTP peer representations require a canonical checksum ETag.
//! Metadata requires Content-Length; Cache-Control/Age govern freshness.
//! Pages use aligned EOF-clipped Range, identity encoding and strong If-Match;
//! 206 requires matching Content-Range, 200 requires a full object page.
//! Peers carry bounded RF05/RF03/RF04 descriptors via HTTP or authenticated RDMA.
//! See `cache::peer_wire` for encoding.
//! Only the selected owner accesses backend. RDMA failure retries same-hop HTTP
//! within the original candidate budget. Health/reuse wait for CRC validation.

use crate::http_auth::failure::*;
use crate::{
    buffers::{BUFFER_SIZE, Destination},
    cache::{
        self, Cache, ExchangeProgress, Fault, Metadata, MetadataFault, Received, Upstream,
        UpstreamRequest, UpstreamResult,
        http_metadata::{checksum, identity_encoding, metadata_facts, page_facts, status, text},
        peer_wire::{
            MAX_CANDIDATE, MAX_DESCRIPTOR, budget_descriptor, descriptor, hex, routed_descriptor,
            unhex,
        },
    },
    http::{Headers, Progress},
    http_client as client, http_server as http, rdma,
    uring::{Ring, Work},
};
use client::Endpoint;
use client::Origin as HttpOrigin;
use client::attempt::{PeerFailure, PeerReason};
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

mod response {
    //! Client representation preconditions and range selection, before payload faults.
    use super::*;
    use crate::http::trim;

    // RFC 9110 entity-tag lists are byte strings, not quoted-string values: commas
    // and backslashes inside a tag are literal, and obs-text need not be UTF-8.
    // Repeated field lines combine as a list. Empty list members are tolerated;
    // wildcard is only valid as the entire field value, never a list member.
    pub(super) fn matches(
        headers: Headers<'_>,
        name: &str,
        current: &[u8],
        strong: bool,
    ) -> io::Result<Option<bool>> {
        let mut present = false;
        let mut wildcard = false;
        let mut matched = false;
        let mut members = 0;
        for (_, value) in headers.iter().filter(|(n, _)| n.eq_ignore_ascii_case(name)) {
            if wildcard {
                return Err(invalid("wildcard in entity-tag list"));
            }
            let mut rest = trim(value);
            if rest == b"*" {
                if present {
                    return Err(invalid("wildcard in entity-tag list"));
                }
                wildcard = true;
            } else {
                while !rest.is_empty() {
                    if rest[0] == b',' {
                        rest = trim(&rest[1..]);
                        continue;
                    }
                    let weak = rest.starts_with(b"W/");
                    if weak {
                        rest = &rest[2..];
                    }
                    if rest.first() != Some(&b'"') {
                        return Err(invalid("invalid entity-tag"));
                    }
                    let end = rest[1..]
                        .iter()
                        .position(|b| *b == b'"')
                        .map(|n| n + 2)
                        .ok_or_else(|| invalid("unterminated entity-tag"))?;
                    let tag = &rest[..end];
                    if !tag[1..end - 1].iter().all(|b| *b >= 0x21 && *b != 0x7f) {
                        return Err(invalid("invalid entity-tag bytes"));
                    }
                    members += 1;
                    matched |= if strong {
                        !weak && tag == current
                    } else {
                        tag == current.strip_prefix(b"W/").unwrap_or(current)
                    };
                    rest = trim(&rest[end..]);
                    if !rest.is_empty() {
                        if rest[0] != b',' {
                            return Err(invalid("invalid entity-tag separator"));
                        }
                        rest = trim(&rest[1..]);
                    }
                }
            }
            present = true;
        }
        if present && !wildcard && members == 0 {
            return Err(invalid("empty entity-tag list"));
        }
        Ok(present.then_some(wildcard || matched))
    }

    impl Task {
        pub(super) fn prepare(&mut self, meta: Metadata) -> io::Result<()> {
            let Response::Request(request) = &self.response else {
                return Err(invalid("missing request"));
            };
            // Admit the representation before HEAD, preconditions, ranges, cache
            // hits or page faults can produce ownership-dependent client results.
            if self.distributed && !cache::peer_wire::client_fits(meta.target().len()) {
                return self.respond(422, 0, &[]);
            }
            // Validate both fields before evaluating them in protocol order. Syntax
            // errors are client 400s, not upstream failures. Metadata proves existence
            // for wildcard comparisons.
            let etag = meta.etag();
            let etag = etag.as_str();
            let conditions = matches(request.headers(), "if-match", etag.as_bytes(), true)
                .and_then(|m| {
                    matches(request.headers(), "if-none-match", etag.as_bytes(), false)
                        .map(|n| (m, n))
                });
            let mut headers = vec![("Accept-Ranges", b"bytes".as_slice())];
            headers.push(("ETag", etag.as_bytes()));
            match conditions {
                Err(_) => return self.respond(400, 0, &[]),
                Ok((Some(false), _)) => return self.respond(412, 0, &headers),
                // 304's Content-Length describes the full selected representation;
                // transport suppresses its body, and end stays zero (no prefetch).
                Ok((_, Some(true))) => return self.respond(304, meta.len(), &headers),
                _ => {}
            }
            let mut status = 200;
            let mut content_range = String::new();
            self.end = meta.len();
            if matches!(request, http::Request::Get(_)) {
                let if_range = text(request.headers(), "if-range")?;
                if if_range.is_none() || if_range == Some(etag) {
                    match http::resolve_range(request.headers(), meta.len()) {
                        http::RangeSelection::Full => {}
                        http::RangeSelection::Partial(range) => {
                            status = 206;
                            self.position = range.start();
                            self.end = range.end();
                            content_range = range.content_range().to_string();
                        }
                        http::RangeSelection::Unsatisfiable => {
                            status = 416;
                            self.end = 0;
                            content_range = format!("bytes */{}", meta.len());
                        }
                    }
                }
            }
            self.next = self.position / BUFFER_SIZE as u64 * BUFFER_SIZE as u64;
            if !content_range.is_empty() {
                headers.push(("Content-Range", content_range.as_bytes()));
            }
            self.head = Some(PendingHead {
                status,
                len: self.end - self.position,
                headers: headers
                    .into_iter()
                    .map(|(n, v)| (n.to_owned(), v.to_vec()))
                    .collect(),
            });
            self.metadata = Some(meta);
            Ok(())
        }
    }
}

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

fn candidate_end(now: Instant, caller: Instant, remaining: u32) -> Instant {
    now + (caller
        .saturating_duration_since(now)
        .saturating_sub(RETURN_SLACK)
        / remaining.max(1))
    .min(MAX_CANDIDATE)
}

pub(crate) fn remote_deadline(bytes: &[u8], local: Instant) -> io::Result<Instant> {
    let (_, remaining) = budget_descriptor(bytes)?;
    Ok(local.min(crate::environment::now() + remaining.unwrap_or(MAX_CANDIDATE)))
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
/// Numeric origin transport plus an explicit, stable cache identity.
#[derive(Clone)]
pub struct Backend {
    endpoint: Endpoint,
    namespace: cache::Namespace,
}
impl Backend {
    pub fn new(address: &str, identity: &str) -> io::Result<Self> {
        let endpoint = Endpoint::parse(address)?;
        let namespace = cache::Namespace::new(identity).map_err(io_error)?;
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
        self.endpoint.address
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
}
impl Peer {
    pub fn from_endpoint(endpoint: Endpoint) -> Self {
        Self {
            http: HttpOrigin::peer(endpoint),
            rdma: None,
            breaker: crate::breaker::CircuitBreaker::new(COOLDOWN),
        }
    }
    pub fn new(http_address: &str, rdma: Option<rdma::Connection>) -> io::Result<Self> {
        Ok(Self {
            http: HttpOrigin::peer(Endpoint::parse(http_address)?),
            rdma: rdma.map(Rc::new),
            breaker: crate::breaker::CircuitBreaker::new(COOLDOWN),
        })
    }
}

#[cfg(test)]
pub(crate) fn simulation_target(wire: &str) -> String {
    routed_descriptor(&unhex(wire).unwrap())
        .unwrap()
        .1
        .target()
        .to_owned()
}
/// Runtime uses this solely to select a retained immutable generation. Full
/// target/path validation happens before creating any cache fault.
pub(crate) fn routing_identity(headers: Headers<'_>) -> io::Result<Option<[u8; 32]>> {
    let Some(wire) = text(headers, "x-racer-fault")? else {
        return Ok(None);
    };
    let bytes = unhex(wire)?;
    let (cursor, _) = routed_descriptor(&bytes).map_err(io_error)?;
    Ok(cursor.map(|c| c.identity))
}

/// Concrete worker-local upstream provider used by the handler's affine tasks.
pub struct Provider {
    metrics: crate::metrics::Local,
    authentication: Option<crate::http_auth::Policy>,
    max_attempts: u32,
    backend: HttpOrigin,
    peer: Option<Peer>,
    peers: BTreeMap<String, Peer>,
    selected: Option<String>,
    routing: Option<Arc<crate::routing::Routing>>,
    active: Option<Rc<RefCell<RouteState>>>,
    owners: Owners,
    negotiations: BTreeMap<String, String>,
}
#[derive(Clone)]
struct RouteState {
    #[cfg(test)]
    target: String,
    cursor: crate::routing::Cursor,
    origin: bool,
    // Retain the last valid cursor for cache lookup/scoping, but grant no upstream.
    exhausted: bool,
}
#[allow(clippy::large_enum_variant)]
pub enum HttpGet {
    Payload(client::GetExchange<Destination>),
    Metadata(client::SmallExchange),
}
enum HttpResponse {
    Payload(client::GetResponse<Destination>),
    Metadata(client::SmallResponse),
}
impl HttpGet {
    fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<HttpResponse>> {
        Ok(match self {
            Self::Payload(e) => match e.poll(ring, budget)? {
                Progress::Pending(w) => Progress::Pending(w),
                Progress::Ready(r) => Progress::Ready(HttpResponse::Payload(r)),
            },
            Self::Metadata(e) => match e.poll(ring, budget)? {
                Progress::Pending(w) => Progress::Pending(w),
                Progress::Ready(r) => Progress::Ready(HttpResponse::Metadata(r)),
            },
        })
    }
}
impl HttpResponse {
    fn status(&self) -> u16 {
        match self {
            Self::Payload(r) => r.status(),
            Self::Metadata(r) => r.status(),
        }
    }
    fn content_length(&self) -> Option<u64> {
        match self {
            Self::Payload(r) => r.content_length(),
            Self::Metadata(r) => r.content_length(),
        }
    }
    fn headers(&self) -> client::Headers<'_> {
        match self {
            Self::Payload(r) => r.headers(),
            Self::Metadata(r) => r.headers(),
        }
    }
    fn body(&mut self) -> &[u8] {
        match self {
            Self::Payload(r) => r.body(),
            Self::Metadata(r) => r.body(),
        }
    }
    fn recycle(self) -> (Option<client::Connection>, Option<Destination>, usize) {
        match self {
            Self::Payload(r) => {
                let (c, d, n) = r.recycle();
                (c, Some(d), n)
            }
            Self::Metadata(r) => {
                let n = r.body().len();
                (r.recycle(), None, n)
            }
        }
    }
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
    Head(client::HeadExchange, crate::breaker::Permit),
    Get(
        HttpGet,
        UpstreamRequest,
        crate::breaker::Permit,
        Option<Attempt>,
        Option<crate::http_auth::Pending>,
    ),
    Grant {
        attempt: Option<Attempt>,
        permit: crate::breaker::Permit,
        connection: Rc<rdma::Connection>,
        ticket: rdma::Ticket<rdma::Grant>,
        destination: Destination,
        request: UpstreamRequest,
        deadline: Instant,
    },
    Read {
        attempt: Option<Attempt>,
        checksum: Option<u64>,
        permit: crate::breaker::Permit,
        connection: Rc<rdma::Connection>,
        ticket: rdma::Ticket<rdma::DestinationRead>,
        request: UpstreamRequest,
        deadline: Instant,
    },
    /// The cache must reacquire both capabilities before resuming this HTTP retry.
    RecoverHttp(UpstreamRequest, Option<Attempt>),
    ValidateHttp(
        Option<client::Connection>,
        Option<crate::breaker::Permit>,
        Option<Attempt>,
    ),
    ValidateRdma(
        UpstreamRequest,
        Option<crate::breaker::Permit>,
        Rc<rdma::Connection>,
        Option<Attempt>,
    ),
}
impl Provider {
    fn new(backend: Backend) -> Self {
        Self {
            metrics: crate::metrics::Local::default(),
            authentication: None,
            max_attempts: 3,
            backend: HttpOrigin::new(backend.endpoint),
            peer: None,
            peers: BTreeMap::new(),
            selected: None,
            routing: None,
            active: None,
            owners: Owners::default(),
            negotiations: BTreeMap::new(),
        }
    }
    fn attempt(&mut self, context: String) -> io::Result<Option<Attempt>> {
        let (Some(state), Some(routing), Some(peer)) = (&self.active, &self.routing, &self.peer)
        else {
            return Ok(None);
        };
        let cursor = state.borrow().cursor.clone();
        let candidate = routing.destination(&cursor);
        if self.owner_evidence(&cursor) {
            return Err(io::Error::other(AttemptFailure {
                route: AttemptRoute {
                    cursor: cursor.clone(),
                    candidate,
                    endpoint: peer.http.endpoint.address,
                    final_hop: routing.last_hop(&cursor),
                    context,
                },
                evidence: None,
                reported: true,
            }));
        }
        Ok(Some(Attempt {
            route: AttemptRoute {
                cursor: cursor.clone(),
                candidate,
                endpoint: peer.http.endpoint.address,
                final_hop: routing.last_hop(&cursor),
                context,
            },
            owner: Some(self.owners.acquire_final(
                cursor.identity,
                candidate,
                routing.final_peer(&cursor),
            )?),
        }))
    }
    fn owner_evidence(&self, cursor: &crate::routing::Cursor) -> bool {
        let Some(routing) = &self.routing else {
            return false;
        };
        self.owners
            .evidence(cursor.identity, routing.destination(cursor))
            .is_some()
            || routing.final_peer(cursor).is_some_and(|peer| {
                self.owners
                    .physical_evidence(cursor.identity, peer)
                    .is_some()
            })
    }
    fn reported(
        &mut self,
        failure: PeerFailure,
        attempt: &mut Option<Attempt>,
    ) -> cache::Result<()> {
        reported(failure, attempt)
    }
    fn service_end(&self, deadline: Instant) -> Instant {
        let now = crate::environment::now();
        let window = self.active.as_ref().map_or(COOLDOWN, |s| {
            Duration::from_secs(4 + 2 * u64::from(3 - s.borrow().cursor.position.min(3)))
        });
        (now + window).min(deadline.checked_sub(RETURN_SLACK).unwrap_or(now).max(now))
    }
    fn budget_wire(&self, request: &UpstreamRequest, end: Instant) -> io::Result<Vec<u8>> {
        if cache::peer_wire::request_len(request, self.active.is_some(), true)? > MAX_DESCRIPTOR {
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
            .saturating_sub(RETURN_SLACK)
            .min(MAX_CANDIDATE)
            .as_millis() as u32;
        if remaining == 0 {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "peer budget exhausted",
            ));
        }
        let mut out = b"RF04".to_vec();
        out.extend(remaining.to_le_bytes());
        out.extend(bytes);
        if out.len() > MAX_DESCRIPTOR {
            return Err(invalid("fault descriptor too large"));
        }
        Ok(out)
    }
    fn activate(&mut self, state: Option<Rc<RefCell<RouteState>>>) {
        self.active = state;
        let selected = self.active.as_ref().and_then(|s| {
            self.routing
                .as_ref()
                .unwrap()
                .next(&s.borrow().cursor)
                .ok()
                .flatten()
                .map(|n| n.0)
        });
        if self.routing.is_some() && selected != self.selected {
            if let (Some(id), Some(peer)) = (self.selected.take(), self.peer.take()) {
                self.peers.insert(id, peer);
            }
            self.peer = selected.as_ref().and_then(|id| self.peers.remove(id));
            self.selected = selected;
        }
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
            let mut routed = next.algorithm.magic().to_vec();
            routed.extend(next.encode());
            routed.append(&mut bytes);
            bytes = routed;
        }
        if bytes.len() > MAX_DESCRIPTOR {
            return Err(invalid("fault descriptor too large"));
        }
        Ok(bytes)
    }
    fn http_peer_attempt(
        &mut self,
        request: UpstreamRequest,
        destination: Option<Destination>,
        deadline: Instant,
        inherited: Option<Attempt>,
    ) -> cache::Result<Exchange> {
        let kind = metric_kind(&request);
        let service_end = self.service_end(deadline);
        let bytes = self.budget_wire(&request, service_end)?;
        let wire = hex(&bytes);
        let mut headers = vec![("X-Racer-Fault".to_owned(), wire.as_bytes().to_vec())];
        let mut nonce = [0; 16];
        crate::environment::random(&mut nonce).map_err(|e| io::Error::other(e.to_string()))?;
        let context = format!("{}{}", hex(blake3::hash(&bytes).as_bytes()), hex(&nonce));
        headers.push(("X-Racer-Attempt".to_owned(), context.as_bytes().to_vec()));
        let policy = self
            .authentication
            .as_ref()
            .ok_or_else(|| invalid("missing peer authentication policy"))?;
        let peer = self
            .selected
            .as_ref()
            .ok_or_else(|| invalid("missing peer identity"))?
            .parse::<crate::peer_identity::NodeId>()?
            .bytes();
        let authentication = Some(policy.request(peer, "GET", "/", &mut headers)?);
        let mut attempt = if let Some(mut inherited) = inherited {
            inherited.route.context = context.clone();
            Some(inherited)
        } else if let (Some(state), Some(routing), Some(peer)) =
            (&self.active, &self.routing, &self.peer)
        {
            let cursor = state.borrow().cursor.clone();
            let candidate = routing.destination(&cursor);
            Some(Attempt {
                route: AttemptRoute {
                    cursor: cursor.clone(),
                    candidate,
                    endpoint: peer.http.endpoint.address,
                    final_hop: routing.last_hop(&cursor),
                    context: context.clone(),
                },
                owner: None,
            })
        } else {
            None
        };
        // An RDMA producer can finish before its HTTP recovery waiter becomes
        // producer. Retain actual generation/owner evidence briefly, rather than
        // interpreting a transport breaker rejection as new owner evidence.
        if let Some(a) = &attempt
            && self.owner_evidence(&a.route.cursor)
        {
            return Err(io::Error::other(AttemptFailure {
                route: a.route.clone(),
                evidence: None,
                reported: true,
            })
            .into());
        }
        if let Some(a) = &mut attempt
            && a.owner.is_none()
        {
            a.owner = Some(
                self.owners.acquire_final(
                    a.route.cursor.identity,
                    a.route.candidate,
                    self.routing
                        .as_ref()
                        .and_then(|r| r.final_peer(&a.route.cursor)),
                )?,
            );
        }
        let peer = self.peer.as_mut().ok_or(cache::Error::Unavailable)?;
        if peer.http.breaker.active() + peer.breaker.active() >= peer.http.limit {
            return Err(cache::busy("direct peer exchange limit"));
        }
        #[cfg(test)]
        if let Some(w) = crate::simulation::current() {
            w.event(
                "http-attempt",
                &simulation_target(&wire),
                format!("endpoint={}", peer.http.endpoint.address),
            );
        }
        let connection = peer.http.connection();
        #[cfg(test)]
        if let Some(w) = crate::simulation::current() {
            w.event(
                if connection.is_ok() {
                    "transport-http"
                } else {
                    "http-rejected"
                },
                &simulation_target(&wire),
                format!("endpoint={}", peer.http.endpoint.address),
            );
        }
        let (connection, permit) = connection.map_err(|error| {
            if let Some(a) = &attempt {
                let evidence = error
                    .get_ref()
                    .and_then(|e| e.downcast_ref::<client::attempt::Failure>())
                    .cloned()
                    .unwrap_or_else(|| client::attempt::Failure {
                        endpoint: a.route.endpoint,
                        transport: client::attempt::Transport::Http,
                        phase: client::attempt::Phase::LocalAdmission,
                        cause: if error.kind() == io::ErrorKind::WouldBlock {
                            client::attempt::Cause::BreakerRejected
                        } else {
                            client::attempt::Cause::Other
                        },
                        initiated: false,
                        kind: error.kind(),
                        message: error.to_string(),
                    });
                cache::Error::Io(io::Error::other(AttemptFailure {
                    route: a.route.clone(),
                    evidence: Some(evidence),
                    reported: false,
                }))
            } else {
                error.into()
            }
        })?;
        let mut permit = Some(permit);
        let result = (|| {
            let fields = headers
                .iter()
                .map(|(n, v)| {
                    Ok((
                        n.as_str(),
                        std::str::from_utf8(v).map_err(io::Error::other)?,
                    ))
                })
                .collect::<io::Result<Vec<_>>>()?;
            let request_wire = client::Request::new("/", &fields)?;
            let get = match destination {
                Some(destination) => HttpGet::Payload(
                    connection
                        .get(request_wire, destination, service_end.min(deadline))?
                        .service_deadline(service_end < deadline)
                        .connect_cap(COOLDOWN),
                ),
                None => HttpGet::Metadata(
                    connection
                        .get_small(
                            request_wire,
                            cache::METADATA_SIZE,
                            service_end.min(deadline),
                        )?
                        .service_deadline(service_end < deadline)
                        .connect_cap(COOLDOWN),
                ),
            };
            Ok(Exchange::Get(
                get,
                request,
                permit.take().unwrap(),
                attempt,
                authentication,
            ))
        })();
        if let Err(error) = &result {
            HttpOrigin::error(permit.take().unwrap(), error, true);
        } else {
            self.metrics
                .upstream(crate::metrics::Upstream::PeerHttp, kind);
        }
        result
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
            exchange: Exchange::RecoverHttp(request, attempt),
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
        let (connection, permit) = self.backend.connection()?;
        let service_end = deadline
            .checked_sub(RETURN_SLACK)
            .unwrap_or_else(crate::environment::now)
            .max(crate::environment::now());
        let exchange = connection
            .head(
                client::Request::backend(meta.target(), &[("Accept-Encoding", "identity")])?,
                service_end,
            )?
            .service_deadline(service_end < deadline)
            .retry_idle_backend(&self.metrics);
        self.metrics.upstream(
            crate::metrics::Upstream::BackendHttp,
            crate::metrics::Kind::Metadata,
        );
        Ok(Exchange::Head(exchange, permit))
    }
    fn proven_failure(&self, error: &cache::Error) -> bool {
        error_detail::<AttemptFailure>(error).is_some_and(|f| f.owner_evidence())
    }
    fn candidate_deadline(&mut self, caller: Instant) -> Instant {
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
            .min(self.routing.as_ref().unwrap().geometry.slot_count());
        let remaining = total.saturating_sub(state.cursor.attempt).max(1);
        candidate_end(now, caller, remaining)
    }
    fn network_scope(&self, value: [u8; 32]) -> Option<crate::buffers::NetworkFlightKey> {
        let routing = self.routing.as_ref()?;
        let state = self.active.as_ref()?.borrow();
        Some(crate::buffers::NetworkFlightKey {
            value,
            routing: state.cursor.identity,
            version: 5,
            destination: routing.destination(&state.cursor),
            dependency: routing.dependency(&state.cursor).expect("validated route"),
        })
    }
    fn peer_validated(&mut self, retry: &mut Exchange, valid: bool) {
        if let Exchange::ValidateRdma(_, permit, connection, attempt) = retry {
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
        if let Exchange::ValidateHttp(connection, permit, attempt) = retry {
            if valid && let Some(attempt) = attempt.as_mut() {
                attempt.owner_reachable();
            }
            if valid {
                if let Some(peer) = self.peer.as_mut() {
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
    fn peer_failed(&mut self, error: cache::Error) -> cache::Result<bool> {
        let Some(state) = self.active.clone() else {
            return Ok(false);
        };
        let routing = self.routing.as_ref().unwrap();
        let mut state = state.borrow_mut();
        if state.exhausted {
            return Err(cache::Error::Unavailable);
        }
        let destination = routing.destination(&state.cursor);
        let failure = error_detail::<AttemptFailure>(&error);
        let transport_failure = failure.is_some_and(|f| {
            f.owner_evidence()
                && routing.compatible(&f.route.cursor, &state.cursor)
                && f.route.candidate == destination
        });
        #[cfg(test)]
        if let Some(w) = crate::simulation::current() {
            w.event("classify", &state.target, format!("candidate={destination} last_hop={} transport_failure={transport_failure} error={error:?}", routing.last_hop(&state.cursor)));
        }
        if !transport_failure {
            return Err(error);
        }
        if !state.origin {
            return Err(io::Error::other(OwnerUnavailable(destination)).into());
        }
        loop {
            state.cursor.attempt += 1;
            if state.cursor.attempt >= routing.geometry.slot_count().min(self.max_attempts) {
                return Err(cache::Error::Unavailable);
            }
            if !self
                .owners
                .blocked(state.cursor.identity, routing.destination(&state.cursor))
            {
                break;
            }
        }
        state.cursor.position = 0;
        #[cfg(test)]
        if let Some(w) = crate::simulation::current() {
            w.event(
                "candidate",
                &state.target,
                format!(
                    "owner={destination} next={} attempt={}",
                    routing.destination(&state.cursor),
                    state.cursor.attempt
                ),
            );
        }
        drop(state);
        self.activate(self.active.clone());
        Ok(true)
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
            let wire = self.budget_wire(&request, self.service_end(deadline))?;
            let mut attempt = self.attempt(hex(blake3::hash(&wire).as_bytes()))?;
            let (key, len) = match &request {
                UpstreamRequest::PeerMetadata(m) => (m.key(), m.len()),
                UpstreamRequest::PeerPage(p) => (*p.key(), p.len()),
                _ => unreachable!(),
            };
            let peer = self.peer.as_mut().ok_or(cache::Error::Unavailable)?;
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
                    .entry(id.clone())
                    .or_insert_with(|| target.to_owned());
            }
            if let Some(connection) = &peer.rdma
                && let Ok(permit) = peer.breaker.try_acquire()
            {
                match connection.request(key, len, &wire) {
                    Ok(ticket) => {
                        self.metrics
                            .upstream(crate::metrics::Upstream::PeerRdma, metric_kind(&request));
                        #[cfg(test)]
                        if let Some(w) = crate::simulation::current() {
                            let target = match &request {
                                UpstreamRequest::PeerMetadata(m) => m.target(),
                                UpstreamRequest::PeerPage(p) => p.target(),
                                _ => unreachable!(),
                            };
                            w.request(key, target);
                            w.event(
                                "transport-rdma",
                                target,
                                format!("endpoint={}", peer.http.endpoint.address),
                            );
                        }
                        return Ok(Exchange::Grant {
                            attempt: attempt.take(),
                            permit,
                            connection: connection.clone(),
                            ticket,
                            destination,
                            request,
                            deadline: (crate::environment::now() + COOLDOWN).min(deadline),
                        });
                    }
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                        drop(permit);
                        return Err(cache::busy("RDMA request capacity"));
                    }
                    Err(_) if connection.needs_http_recovery() => drop(permit),
                    Err(_) => permit.failure(),
                }
            }
            return self.http_peer_attempt(request, Some(destination), deadline, attempt);
        }
        if let (Some(routing), Some(state)) = (&self.routing, &self.active) {
            if routing.next(&state.borrow().cursor)?.is_some() {
                return Err(cache::Error::Unavailable);
            }
        }
        let (connection, permit) = self.backend.connection()?;
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
                Ok(Exchange::Get(
                    HttpGet::Payload(
                        connection
                            .get(
                                client::Request::backend(page.target(), &headers)?,
                                destination,
                                service_end,
                            )?
                            .service_deadline(service_end < deadline)
                            .retry_idle_backend(&self.metrics),
                    ),
                    request,
                    permit.take().unwrap(),
                    None,
                    None,
                ))
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
            Exchange::RecoverHttp(r, a) => (r, None, None, a),
            Exchange::ValidateRdma(r, p, c, a) => (r, p, Some(c), a),
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
            Exchange::Head(mut exchange, permit) => {
                let mut permit = Some(permit);
                let result = (|| match exchange.poll(ring, 1)? {
                    Progress::Pending(work) => {
                        pending(Exchange::Head(exchange, permit.take().unwrap()), work)
                    }
                    Progress::Ready(response) => {
                        let facts = metadata_facts(&response)?;
                        self.backend.recycle(response.recycle());
                        Ok(ExchangeProgress::Ready(UpstreamResult::Metadata(
                            crate::metadata::Metadata::from_backend(facts),
                        )))
                    }
                })();
                if let Err(error) = &result {
                    HttpOrigin::error(permit.take().unwrap(), error, false);
                }
                if let Some(permit) = permit {
                    permit.success();
                }
                result
            }
            Exchange::Get(mut exchange, request, permit, mut attempt, mut authentication) => {
                let mut permit = Some(permit);
                let peer = matches!(
                    request,
                    UpstreamRequest::PeerMetadata(_) | UpstreamRequest::PeerPage(_)
                );
                let result = (|| match exchange.poll(ring, 1).map_err(|error| {
                    if let Some(attempt) = &attempt {
                        let evidence = error
                            .get_ref()
                            .and_then(|e| e.downcast_ref::<client::attempt::Failure>())
                            .cloned();
                        cache::Error::Io(io::Error::other(AttemptFailure {
                            route: attempt.route.clone(),
                            evidence,
                            reported: false,
                        }))
                    } else {
                        error.into()
                    }
                })? {
                    Progress::Pending(work) => pending(
                        Exchange::Get(
                            exchange,
                            request,
                            permit.take().unwrap(),
                            attempt.take(),
                            authentication.take(),
                        ),
                        work,
                    ),
                    Progress::Ready(mut response) => {
                        if peer {
                            if let Some(authentication) = &authentication {
                                authentication.verify(
                                    &self.authentication.as_ref().unwrap().keys,
                                    response.status(),
                                    response
                                        .content_length()
                                        .ok_or_else(|| invalid("missing peer length"))?,
                                    response.headers(),
                                )?;
                            } else {
                                return Err(invalid("missing peer authentication context").into());
                            }
                        }
                        if peer && response.status() != 200 {
                            if text(response.headers(), "x-racer-failure")?.is_some() {
                                let a = attempt
                                    .as_ref()
                                    .ok_or_else(|| invalid("unrouted peer failure"))?;
                                let failure = validate_peer_report(
                                    response.headers(),
                                    response.content_length(),
                                    response.status(),
                                    &a.route,
                                )?;
                                let (connection, _, len) = response.recycle();
                                if len != 0 {
                                    return Err(invalid("peer failure body").into());
                                }
                                self.peer.as_mut().unwrap().http.recycle(connection);
                                permit.take().unwrap().success();
                                self.reported(failure, &mut attempt)?;
                                unreachable!();
                            }
                            if response.status() == 503 {
                                if text(response.headers(), "x-racer-owner-unavailable")?.is_some()
                                {
                                    let a = attempt
                                        .as_ref()
                                        .ok_or_else(|| invalid("unrouted owner report"))?;
                                    validate_owner_report(
                                        response.headers(),
                                        response.content_length(),
                                        &a.route,
                                    )?;
                                    let (connection, _, len) = response.recycle();
                                    if len != 0 {
                                        return Err(invalid("owner report body").into());
                                    }
                                    self.peer.as_mut().unwrap().http.recycle(connection);
                                    permit.take().unwrap().success();
                                    return Err(io::Error::other(AttemptFailure {
                                        route: a.route.clone(),
                                        evidence: None,
                                        reported: true,
                                    })
                                    .into());
                                }
                            }
                            return Err(status(response.status()));
                        }
                        let facts = if peer {
                            let expected = match &request {
                                UpstreamRequest::PeerPage(page) => page.checksum(),
                                UpstreamRequest::PeerMetadata(_) => {
                                    crate::metadata::Metadata::from_bytes(response.body())?.checksum
                                }
                                _ => unreachable!(),
                            };
                            cache::http_metadata::peer_checksum(response.headers(), expected)?;
                            None
                        } else {
                            Some(page_facts(response.status(), response.headers())?)
                        };
                        identity_encoding(response.headers())?;
                        let checksum = if peer {
                            checksum(response.headers())?
                        } else {
                            None
                        };
                        let metadata = if matches!(request, UpstreamRequest::PeerMetadata(_)) {
                            let bytes = response.body();
                            if checksum != Some(crate::allocator::crc64(bytes)) {
                                return Err(invalid("peer checksum mismatch").into());
                            }
                            Some(crate::metadata::Metadata::from_bytes(bytes)?)
                        } else {
                            None
                        };
                        let (connection, destination, len) = response.recycle();
                        let validation = if peer {
                            Some(Exchange::ValidateHttp(
                                connection,
                                permit.take(),
                                attempt.take(),
                            ))
                        } else {
                            self.backend.recycle(connection);
                            None
                        };
                        let result = match request {
                            UpstreamRequest::PeerMetadata(_) => {
                                UpstreamResult::Metadata(metadata.unwrap())
                            }
                            UpstreamRequest::PeerPage(_) => UpstreamResult::PeerPage(Received {
                                destination: destination.unwrap(),
                                len,
                                checksum,
                            }),
                            UpstreamRequest::BackendPage(_) => UpstreamResult::BackendPage {
                                received: Received {
                                    destination: destination.unwrap(),
                                    len,
                                    checksum,
                                },
                                facts: facts.unwrap(),
                            },
                            _ => return Err(invalid("invalid GET result kind").into()),
                        };
                        Ok(match validation {
                            Some(retry) => ExchangeProgress::ReadyPeer { result, retry },
                            None => ExchangeProgress::Ready(result),
                        })
                    }
                })();
                if let Err(error) = &result {
                    if let Some(a) = attempt.as_mut() {
                        if let Some(failure) = error_detail::<AttemptFailure>(error)
                            && failure.owner_evidence()
                        {
                            if let Some(owner) = a.owner.take() {
                                if failure.reported {
                                    owner.failure(crate::environment::now());
                                } else {
                                    owner.transport_failure(crate::environment::now());
                                }
                            }
                        }
                    }
                    if let Some(permit) = permit.take() {
                        HttpOrigin::error(permit, error, peer);
                    }
                }
                if let Some(permit) = permit {
                    permit.success();
                }
                result
            }
            Exchange::Grant {
                mut attempt,
                permit,
                connection,
                mut ticket,
                destination,
                request,
                deadline,
            } => match connection.take_reply(&mut ticket) {
                Ok(None) if crate::environment::now() >= deadline => {
                    drop((ticket, destination));
                    Ok(self.rdma_failed(request, permit, attempt, &connection))
                }
                Ok(None) => pending(
                    Exchange::Grant {
                        attempt,
                        permit,
                        connection,
                        ticket,
                        destination,
                        request,
                        deadline,
                    },
                    Work {
                        runnable: false,
                        deadline: Some(deadline),
                    },
                ),
                Ok(Some(rdma::GrantReply::Failure(failure))) => {
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
                            Exchange::Read {
                                attempt,
                                checksum,
                                permit,
                                connection,
                                ticket,
                                request,
                                deadline,
                            },
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
            Exchange::Read {
                attempt,
                checksum,
                permit,
                connection,
                mut ticket,
                request,
                deadline,
            } => {
                if crate::environment::now() >= deadline {
                    drop(ticket);
                    return Ok(self.rdma_failed(request, permit, attempt, &connection));
                }
                match connection.take_read_unpublished(&mut ticket) {
                    Ok(None) => pending(
                        Exchange::Read {
                            attempt,
                            checksum,
                            permit,
                            connection,
                            ticket,
                            request,
                            deadline,
                        },
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
                            retry: Exchange::ValidateRdma(
                                request,
                                Some(permit),
                                connection,
                                attempt,
                            ),
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
    pub fn set_authentication(&mut self, policy: crate::http_auth::Policy) {
        self.upstream.authentication = Some(policy);
    }
    pub(crate) fn reject(&mut self, request: http::Request, status: u16) -> Task {
        let mut task = <Self as http::Handler>::start(self, request);
        task.fault = None;
        task.failure = Some(status);
        task
    }
    fn route_state(
        &self,
        cursor: Option<crate::routing::Cursor>,
        target: &str,
        peer: bool,
    ) -> io::Result<Option<Rc<RefCell<RouteState>>>> {
        let Some(routing) = &self.upstream.routing else {
            return if cursor.is_some() {
                Err(invalid("unexpected topology"))
            } else {
                Ok(None)
            };
        };
        let origin = !peer;
        let mut cursor = match cursor {
            Some(cursor) if peer => cursor,
            None if !peer => routing.start(target),
            _ => return Err(invalid("missing topology cursor")),
        };
        let mut exhausted = false;
        if origin {
            while self
                .upstream
                .owners
                .blocked(cursor.identity, routing.destination(&cursor))
            {
                if cursor.attempt + 1
                    >= routing
                        .geometry
                        .slot_count()
                        .min(self.upstream.max_attempts)
                {
                    exhausted = true;
                    break;
                }
                cursor.attempt += 1;
            }
        }
        routing.validate(&cursor, target)?;
        #[cfg(test)]
        if let Some(w) = crate::simulation::current() {
            w.event(
                "route",
                target,
                format!(
                    "source={} owner={} attempt={} position={} origin={origin}",
                    cursor.source, cursor.owner, cursor.attempt, cursor.position
                ),
            );
        }
        Ok(Some(Rc::new(RefCell::new(RouteState {
            #[cfg(test)]
            target: target.into(),
            cursor,
            origin,
            exhausted,
        }))))
    }
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
            draining: false,
            cache,
            crypto: None,
            namespace,
            upstream: Provider {
                metrics,
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
            keys: trust.keys,
            universe: trust.universe,
            node: [node; 32],
            peers: peers.iter().map(|peer| [*peer; 32]).collect(),
        });
        self.upstream.selected = selected.map(|peer| hex(&[peer; 32]));
    }
    #[cfg(test)]
    pub(crate) fn test_peer_breakers(
        &self,
    ) -> (
        crate::breaker::CircuitBreaker,
        crate::breaker::CircuitBreaker,
    ) {
        let peer = self
            .upstream
            .peer
            .as_ref()
            .or_else(|| self.upstream.peers.values().next())
            .unwrap();
        (peer.breaker.clone(), peer.http.breaker.clone())
    }
    #[cfg(test)]
    pub(crate) fn test_suppress_owner(&mut self, slot: u32) {
        let identity = self.upstream.routing.as_ref().unwrap().identity;
        self.upstream
            .owners
            .acquire(identity, slot)
            .unwrap()
            .failure(crate::environment::now());
    }
    #[cfg(test)]
    pub(crate) fn test_has_owner_evidence(&self) -> bool {
        self.upstream.owners.has_evidence()
    }
    pub fn set_peer(&mut self, peer: Peer) {
        if let Some(connection) = &peer.rdma {
            self.add_shared_connection(connection.clone());
        }
        self.upstream.peer = Some(peer);
    }
    pub fn set_routing(
        &mut self,
        routing: Arc<crate::routing::Routing>,
        peers: BTreeMap<String, Peer>,
    ) {
        self.upstream.routing = Some(routing);
        self.upstream.peers = peers;
    }
    pub(crate) fn peer_metrics(&self, volume: &str, out: &mut Vec<crate::metrics::PeerState>) {
        for (id, peer) in self.upstream.peers.iter().chain(
            self.upstream
                .selected
                .as_ref()
                .zip(self.upstream.peer.as_ref()),
        ) {
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
        self.upstream.backend.limit = http_per_endpoint;
        for peer in self
            .upstream
            .peers
            .values_mut()
            .chain(self.upstream.peer.iter_mut())
        {
            peer.http.limit = http_per_endpoint;
        }
        Ok(())
    }
    pub fn set_routed_connection(&mut self, id: &str, connection: Rc<rdma::Connection>) {
        self.add_shared_connection(connection.clone());
        let peer = if self.upstream.selected.as_deref() == Some(id) {
            self.upstream.peer.as_mut()
        } else {
            self.upstream.peers.get_mut(id)
        };
        if let Some(peer) = peer {
            peer.rdma = Some(connection);
        }
    }
    pub(crate) fn take_negotiations(&mut self) -> BTreeMap<String, String> {
        std::mem::take(&mut self.upstream.negotiations)
    }
    /// Upgrade/downgrade transport without resetting the HTTP endpoint pool or
    /// either endpoint breaker. Negotiation retry policy belongs to runtime.
    pub fn set_peer_connection(&mut self, connection: Option<Rc<rdma::Connection>>) {
        if let Some(connection) = &connection {
            self.add_shared_connection(connection.clone());
        }
        if let Some(peer) = &mut self.upstream.peer {
            peer.rdma = connection;
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
        for peer in self.upstream.peers.values_mut() {
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
                .rdma
                .as_ref()
                .is_some_and(|c| Rc::ptr_eq(c, connection))
        {
            peer.rdma = None;
        }
    }
    pub fn add_connection(&mut self, connection: rdma::Connection) {
        self.connections.push(Rc::new(connection));
    }
    pub(crate) fn begin_drain(&mut self) {
        self.draining = true;
    }
    /// Bounded round-robin disk and inbound RDMA service, even with no HTTP tasks.
    pub fn poll_background(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
        let mut cache = self.cache.borrow_mut();
        cache.set_namespace(self.namespace);
        cache.set_crypto(self.crypto.clone());
        let mut work = cache.poll(ring, budget).map_err(io_error)?;
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
                if let Ok((cursor, descriptor)) = routed_descriptor(&request.metadata)
                    && let Ok(route) = self.route_state(cursor, descriptor.target(), true)
                    && let Ok(deadline) =
                        remote_deadline(&request.metadata, crate::environment::now() + TIMEOUT)
                {
                    let admitted = if self.incoming.len() >= self.rdma_task_limit {
                        Err(cache::busy("inbound RDMA task limit"))
                    } else {
                        cache.peer_fault(
                            descriptor.with_expected(request.value, request.len),
                            deadline,
                        )
                    };
                    match admitted {
                        Ok(fault) => {
                            self.incoming.push_back(RdmaTask {
                                connection,
                                request,
                                fault,
                                route,
                            });
                        }
                        Err(error) => {
                            let (identity, candidate) = route.as_ref().map_or(([0; 32], 0), |r| {
                                let r = r.borrow();
                                (
                                    r.cursor.identity,
                                    self.upstream
                                        .routing
                                        .as_ref()
                                        .unwrap()
                                        .destination(&r.cursor),
                                )
                            });
                            let _ = connection
                                .respond_error(request, peer_failure(&error, identity, candidate));
                        }
                    }
                } else {
                    let _ = connection.respond_error(
                        request,
                        PeerFailure {
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
                route,
            } = self.incoming.pop_front().unwrap();
            if !connection.request_live(&request) {
                work.runnable = true;
                continue;
            }
            self.upstream.activate(route.clone());
            match cache.poll_fault(fault, ring, &mut self.upstream) {
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
                        route,
                    });
                }
                Err(error) => {
                    let (identity, candidate) = route.as_ref().map_or(([0; 32], 0), |r| {
                        let r = r.borrow();
                        (
                            r.cursor.identity,
                            self.upstream
                                .routing
                                .as_ref()
                                .unwrap()
                                .destination(&r.cursor),
                        )
                    });
                    #[cfg(test)]
                    if let Some(world) = crate::simulation::current() {
                        eprintln!(
                            "DST rdma-fault-error tick={} node={:?} key={} candidate={candidate} error={error:?}",
                            world.tick(),
                            world.process().node,
                            blake3::Hash::from(request.value)
                        );
                    }
                    let _ = connection
                        .respond_error(request, peer_failure(&error, identity, candidate));
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
        self.cache.borrow_mut().shutdown(ring).map_err(io_error)
    }
}
struct RdmaTask {
    route: Option<Rc<RefCell<RouteState>>>,
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
    Loading(Fault<Provider>, Option<Rc<RefCell<RouteState>>>),
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
    authentication: Option<(crate::http_auth::Incoming, crate::signing::Keys)>,
    route: Option<Rc<RefCell<RouteState>>>,
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
        let signature = self
            .authentication
            .as_ref()
            .map(|(incoming, keys)| incoming.response(keys, status, len, headers))
            .transpose()?;
        let mut headers = headers.to_vec();
        if let Some(signature) = &signature {
            headers.push(("X-Racer-Signature", signature.as_bytes()));
        }
        let head = http::ResponseHead::new(status, Some(len), &headers)?;
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
    fn prefetch(
        &mut self,
        cache: &mut Cache,
        upstream: &mut Provider,
        ring: &mut Ring,
    ) -> cache::Result<Work> {
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
                // Snapshot the current stream budget only for a newly admitted
                // page. Existing faults/candidates never have their caps renewed.
                let fault = cache.page(
                    self.metadata.as_ref().unwrap(),
                    self.next,
                    self.response_deadline.get(),
                )?;
                let offset = self.next;
                self.next += fault.len() as u64;
                let route = self
                    .route
                    .as_ref()
                    .map(|r| Rc::new(RefCell::new(r.borrow().clone())));
                self.pages
                    .push_back((offset, PageLoad::Loading(fault, route)));
                work.runnable = true;
            }
        }
        // At most two faults per turn; keep reading while SEND_ZC waits.
        for (_, page) in &mut self.pages {
            if matches!(page, PageLoad::Loading(..)) {
                let PageLoad::Loading(fault, route) = std::mem::replace(page, PageLoad::Taken)
                else {
                    unreachable!()
                };
                upstream.activate(route.clone());
                match cache.poll_value(fault, ring, upstream)? {
                    cache::Progress::Pending { fault, work: w } => {
                        *page = PageLoad::Loading(fault, route);
                        work.merge(w);
                    }
                    cache::Progress::Ready(buffer) => {
                        if let (Some(parent), Some(route)) = (&self.route, &route) {
                            if route.borrow().cursor.attempt > parent.borrow().cursor.attempt {
                                *parent.borrow_mut() = route.borrow().clone();
                            }
                        }
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
    fn start(&mut self, mut request: http::Request) -> Task {
        let traffic = if request.headers().get("x-racer-fault").is_some() {
            crate::metrics::Traffic::PeerHttp
        } else {
            crate::metrics::Traffic::ClientHttp
        };
        self.upstream.metrics.request(traffic);
        request.set_metric_traffic(traffic);
        let shared_cache = self.cache.clone();
        let mut cache = shared_cache.borrow_mut();
        cache.set_namespace(self.namespace);
        cache.set_crypto(self.crypto.clone());
        let deadline = request.deadline();
        let response_deadline = request.response_deadline();
        let mut task = Task {
            authentication: None,
            route: None,
            response: Response::Request(request),
            metadata: None,
            fault: None,
            pages: VecDeque::new(),
            position: 0,
            end: 0,
            next: 0,
            peer: false,
            distributed: self
                .upstream
                .routing
                .as_ref()
                .map_or(self.upstream.peer.is_some(), |r| {
                    r.local.len() < r.geometry.slot_count() as usize
                }),
            deadline,
            response_deadline,
            failure: None,
            head: None,
            metric_peer: matches!(traffic, crate::metrics::Traffic::PeerHttp),
            error_metric: None,
            headers_sent: false,
        };
        let Response::Request(request) = &task.response else {
            unreachable!()
        };
        let parsed: cache::Result<Initial> = (|| {
            if let Some(wire) = text(request.headers(), "x-racer-fault")? {
                if !matches!(request, http::Request::Get(_)) {
                    return Err(invalid("peer faults require GET").into());
                }
                task.peer = true;
                let policy = self
                    .upstream
                    .authentication
                    .as_ref()
                    .ok_or_else(|| invalid("missing peer authentication policy"))?;
                let incoming = policy.receive("GET", request.target(), request.headers())?;
                task.authentication = Some((incoming, policy.keys.clone()));
                let bytes = unhex(wire)?;
                let (cursor, descriptor) = routed_descriptor(&bytes)?;
                task.deadline = remote_deadline(&bytes, deadline)?;
                task.route = self.route_state(cursor, descriptor.target(), true)?;
                if let Some((incoming, _)) = &task.authentication {
                    if let Err(error) = incoming.accept_once() {
                        if error.kind() == io::ErrorKind::WouldBlock {
                            return Ok(Initial::Rejected(error.into()));
                        }
                        return Err(error.into());
                    }
                }
                cache
                    .peer_fault(descriptor, task.deadline)
                    .map(|fault| Initial::Peer(fault))
            } else {
                if task.distributed && !cache::peer_wire::client_fits(request.target().len()) {
                    task.failure = Some(414);
                    return Err(invalid("target exceeds distributed page wire limit").into());
                }
                task.route = self.route_state(None, request.target(), false)?;
                cache
                    .metadata(request.target(), deadline)
                    .map(Initial::Metadata)
            }
        })();
        match parsed {
            Ok(fault) => task.fault = Some(fault),
            Err(error @ cache::Error::Admission(_)) => task.fault = Some(Initial::Rejected(error)),
            Err(cache::Error::Io(e)) if e.kind() == io::ErrorKind::WouldBlock => {
                task.error_metric = Some(metric_failure(&cache::Error::Io(e)));
                task.failure = Some(503)
            }
            Err(_) => task.failure = Some(task.failure.unwrap_or(400)),
        }
        if task.peer
            && let Response::Request(request) = &mut task.response
        {
            request.cap_deadline(task.deadline + RETURN_SLACK);
        }
        task
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
                    let error = cache::Error::Io(error);
                    self.upstream.metrics.http_failure(
                        task.metric_peer,
                        true,
                        metric_failure(&error),
                    );
                    return Err(io_error(error));
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
        self.upstream.activate(task.route.clone());
        let mut cache = self.cache.borrow_mut();
        cache.set_crypto(self.crypto.clone());
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
                    match cache.poll_metadata(fault, ring, &mut self.upstream) {
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
                    match cache.poll_value(fault, ring, &mut self.upstream) {
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
                            let headers = vec![
                                ("X-Racer-Crc64", checksum.as_bytes()),
                                ("ETag", etag.as_str().as_bytes()),
                            ];
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
                #[cfg(test)]
                if let Some(world) = crate::simulation::current() {
                    eprintln!(
                        "DST request-error tick={} node={:?} target={:?} peer={} error={error:?}",
                        world.tick(),
                        world.process().node,
                        task.route.as_ref().map(|r| r.borrow().target.clone()),
                        task.peer
                    );
                }
                task.head = None;
                task.pages.clear();
                task.position = 0;
                task.end = 0;
                task.next = 0;
                let owner = owner_failure(&error).map(|s| s.to_string());
                let failure = if task.peer {
                    task.route.as_ref().map(|route| {
                        let route = route.borrow();
                        hex(&peer_failure(
                            &error,
                            route.cursor.identity,
                            self.upstream
                                .routing
                                .as_ref()
                                .unwrap()
                                .destination(&route.cursor),
                        )
                        .encode())
                    })
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
                let mut headers = owner
                    .as_ref()
                    .map(|s| {
                        let mut headers = vec![("X-Racer-Owner-Unavailable", s.as_bytes())];
                        if let Some(context) = &context {
                            headers.push(("X-Racer-Attempt", context.as_bytes()));
                        }
                        headers
                    })
                    .unwrap_or_default();
                if let (Some(failure), Some(context)) = (&failure, &context) {
                    headers.push(("X-Racer-Failure", failure.as_bytes()));
                    if owner.is_none() {
                        headers.push(("X-Racer-Attempt", context.as_bytes()));
                    }
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
        let mut page_work = match task.prefetch(&mut cache, &mut self.upstream, ring) {
            Ok(work) => work,
            Err(error) if matches!(task.response, Response::Request(_)) => {
                #[cfg(test)]
                if let Some(world) = crate::simulation::current() {
                    eprintln!(
                        "DST prefetch-error-before-clear tick={} node={:?} target={:?} position={} end={} next={} pages={:?} error={error:?}",
                        world.tick(),
                        world.process().node,
                        task.route.as_ref().map(|r| r.borrow().target.clone()),
                        task.position,
                        task.end,
                        task.next,
                        task.pages
                            .iter()
                            .map(|(offset, page)| (
                                *offset,
                                match page {
                                    PageLoad::Loading(f, _) => Some(blake3::Hash::from(*f.key())),
                                    _ => None,
                                }
                            ))
                            .collect::<Vec<_>>()
                    );
                }
                task.pages.clear();
                task.head = None;
                task.end = 0;
                task.next = 0;
                task.error_metric = Some(metric_failure(&error));
                task.respond(error_status(&error), 0, &[])?;
                return Ok(Progress::Pending(runnable()));
            }
            Err(error) => return Err(io_error(error)),
        };
        if task.head.is_some() {
            if task.end != 0
                && !matches!(task.response, Response::Request(http::Request::Head(_)))
                && !matches!(task.pages.front(), Some((_, PageLoad::Ready(_))))
            {
                return Ok(Progress::Pending(page_work));
            }
            let PendingHead {
                status,
                len,
                headers,
            } = task.head.take().unwrap();
            let headers: Vec<_> = headers
                .iter()
                .map(|(n, v)| (n.as_str(), v.as_slice()))
                .collect();
            task.respond(status, len, &headers)?;
        }
        let state = std::mem::replace(&mut task.response, Response::Done);
        let progress = match state {
            Response::Head(mut headers) => {
                let result = headers.poll(ring, 1)?;
                if matches!(result, Progress::Pending(_)) {
                    task.response = Response::Head(headers);
                } else {
                    task.sent_headers(&self.upstream.metrics);
                }
                return Ok(result);
            }
            Response::Headers(mut headers) => match headers.poll(ring, 1)? {
                Progress::Pending(work) => {
                    task.response = Response::Headers(headers);
                    page_work.merge(work);
                    return Ok(Progress::Pending(page_work));
                }
                Progress::Ready(progress) => {
                    task.sent_headers(&self.upstream.metrics);
                    progress
                }
            },
            Response::Body(mut body) => match body.poll(ring, 1)? {
                Progress::Pending(work) => {
                    task.response = Response::Body(body);
                    page_work.merge(work);
                    return Ok(Progress::Pending(page_work));
                }
                Progress::Ready(progress) => progress,
            },
            Response::Writer(writer) => {
                if matches!(task.pages.front(), Some((_, PageLoad::Ready(_)))) {
                    let (offset, PageLoad::Ready(buffer)) = task.pages.pop_front().unwrap() else {
                        unreachable!()
                    };
                    let start = (task.position - offset) as usize;
                    let len = (buffer.len() - start)
                        .min((task.end - task.position).min(usize::MAX as u64) as usize);
                    task.position += len as u64;
                    let chunk =
                        http::BodyChunk::value(buffer, start..start + len).map_err(|e| e.error)?;
                    task.response = Response::Body(writer.send(chunk).map_err(|e| e.error)?);
                    return Ok(Progress::Pending(runnable()));
                }
                task.response = Response::Writer(writer);
                return Ok(Progress::Pending(page_work));
            }
            _ => return Err(invalid("invalid HTTP task state")),
        };
        match progress {
            http::BodyProgress::More(writer) => {
                task.response = Response::Writer(writer);
                Ok(Progress::Pending(runnable()))
            }
            http::BodyProgress::Done(done) => Ok(Progress::Ready(done)),
        }
    }
}

#[cfg(test)]
#[path = "../tests/http/handlers.rs"]
mod tests;
