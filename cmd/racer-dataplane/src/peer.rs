//! Correlated logical requests with monotonic budgets, attempts, and cancellation.
pub mod forwarding;
pub mod protocol;
pub mod server;
pub mod subscriptions;
pub mod transport;
use self::forwarding::Forwarding;
use self::forwarding::VerifiedRequest;
use self::forwarding::VerifiedResponse;
use self::protocol::PeerRequest;
use self::protocol::SignedRequest;
use self::protocol::SignedResponse;
use self::transport::Transfers;
use crate::admission::AdmissionPolicy;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use racer_control_wire::MembershipVersion;
use racer_control_wire::NodeId;
use crate::admission::ResourceClass;
use crate::runtime::RequestScope;
use crate::telemetry::Event;
use crate::telemetry::Gauge;
use crate::telemetry::Metrics;
use crate::telemetry::Observer;
use crate::telemetry::Stage;

use crate::rdma;
use crate::rdma::TransportPlan;
use crate::topology::Paths;
use std::collections::BTreeMap;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::time::Duration;
use std::time::Instant;
use uring_runtime::environment::now;

const STAGES: [(Event, Event); 4] = [
    (Event::PeerPageCheckoutCount, Event::PeerPageCheckoutNs),
    (Event::PeerPageAuthCount, Event::PeerPageAuthNs),
    (Event::PeerPageHeadCount, Event::PeerPageHeadNs),
    (Event::PeerPageBodyCount, Event::PeerPageBodyNs),
];

/// Publish all HTTP Page stages only after verification; censor other outcomes.
pub(crate) struct PageTiming<'a> {
    metrics: &'a Metrics,
    active: bool,
    start: Option<Instant>,
    durations: [u64; 4],
    completed: u8,
}
impl<'a> PageTiming<'a> {
    fn new(metrics: &'a Metrics) -> Self {
        Self {
            metrics,
            active: false,
            start: None,
            durations: [0; 4],
            completed: 0,
        }
    }
    fn enable(&mut self, request: &PeerRequest, plan: TransportPlan, opaque: bool) {
        self.active = !opaque
            && matches!(plan, TransportPlan::Http)
            && request.route.visited.len() == 1
            && matches!(request.operation, protocol::Operation::Page { .. });
        self.begin();
    }
    fn begin(&mut self) {
        if self.active {
            self.start = Some(now());
        }
    }
    fn end(&mut self, stage: usize) {
        if let Some(start) = self.start.take() {
            self.durations[stage] = now()
                .saturating_duration_since(start)
                .as_nanos()
                .min(u64::MAX as u128) as u64;
            self.completed |= 1 << stage;
        }
    }
    fn success(&mut self, response: &VerifiedResponse) {
        if self.active
            && self.completed == 15
            && matches!(response.response(), protocol::PeerResponse::Page { .. })
        {
            // Concurrent scrapes are not atomic snapshots.
            for ((count, sum), duration) in STAGES.into_iter().zip(self.durations) {
                let _ = self.metrics.record(sum, duration);
                let _ = self.metrics.record(count, 1);
            }
            self.active = false;
        }
    }
}
impl Drop for PageTiming<'_> {
    fn drop(&mut self) {
        if self.active {
            let _ = self.metrics.record(Event::PeerPageCensored, 1);
        }
    }
}
/// Node-wide outbound admission, independent of worker count and byte quotas.
#[derive(Clone, Copy)]
pub struct Config {
    pub total: usize,
    pub per_peer: usize,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            total: 256,
            per_peer: 32,
        }
    }
}
impl Config {
    pub fn validate(self) -> Result<()> {
        if self.total == 0 || self.total > 65536 || self.per_peer == 0 || self.per_peer > self.total
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}
const CAPACITY: usize = 256;
const BACKOFF: Duration = Duration::from_millis(250);
const RECOVERY: Duration = Duration::from_secs(1);
pub(crate) struct AdaptivePeers {
    config: Config,
    state: Mutex<State>,
    metrics: Metrics,
}
struct State {
    active: usize,
    limit: usize,
    updated: Instant,
    peers: BTreeMap<NodeId, Peer>,
}
struct Peer {
    active: usize,
    limit: usize,
    generation: u64,
    retry: Option<Instant>,
    probe: bool,
    updated: Instant,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Outcome {
    Verified,
    PeerFailure,
    LocalPressure,
    Neutral,
}
pub(crate) struct Permit {
    owner: Arc<AdaptivePeers>,
    node: NodeId,
    generation: u64,
    probe: bool,
}
impl AdaptivePeers {
    pub(crate) fn hedge_available(&self, node: &NodeId) -> bool {
        self.state.lock().is_ok_and(|state| {
            state.limit == self.config.total
                && state.active + 1 < state.limit
                && state.peers.get(node).is_none_or(|p| {
                    p.retry.is_none()
                        && !p.probe
                        && p.limit == self.config.per_peer
                        && p.active < p.limit
                })
        })
    }
    pub(crate) fn new(config: Config, metrics: Metrics) -> Result<Arc<Self>> {
        config.validate()?;
        metrics.set_gauge(Gauge::PeerAdmissionLimit, config.total as u64);
        Ok(Arc::new(Self {
            config,
            metrics,
            state: Mutex::new(State {
                active: 0,
                limit: config.total,
                updated: now(),
                peers: BTreeMap::new(),
            }),
        }))
    }
    pub(crate) fn available(&self, node: &NodeId) -> bool {
        let now = now();
        self.state.lock().is_ok_and(|state| {
            state
                .peers
                .get(node)
                .is_none_or(|p| !p.probe && p.retry.is_none_or(|at| now >= at))
        })
    }
    pub(crate) fn acquire(self: &Arc<Self>, node: &NodeId) -> Result<Arc<Permit>> {
        let now = now();
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        if state.active >= state.limit {
            self.metrics.record(Event::PeerAdmissionRejected, 1);
            return Err(Error::Overloaded);
        }
        if !state.peers.contains_key(node) && state.peers.len() == CAPACITY {
            // Never discard live work or a circuit still in backoff. Retired entries
            // eventually age out without depending on any worker's retained lease.
            let retired = state
                .peers
                .iter()
                .find(|(_, p)| {
                    p.active == 0
                        && !p.probe
                        && p.retry.is_none_or(|retry| now >= retry)
                        && now.saturating_duration_since(p.updated) >= Duration::from_secs(60)
                })
                .map(|(n, _)| n.clone());
            if let Some(retired) = retired {
                state.peers.remove(&retired);
            } else {
                self.metrics.record(Event::PeerAdmissionRejected, 1);
                return Err(Error::Overloaded);
            }
        }
        let peer = state.peers.entry(node.clone()).or_insert(Peer {
            active: 0,
            limit: self.config.per_peer,
            generation: 0,
            retry: None,
            probe: false,
            updated: now,
        });
        if peer.probe || peer.retry.is_some_and(|at| now < at) {
            self.metrics.record(Event::PeerCircuitRejected, 1);
            return Err(Error::Unavailable);
        }
        if peer.active >= peer.limit {
            self.metrics.record(Event::PeerAdmissionRejected, 1);
            return Err(Error::Overloaded);
        }
        let probe = peer.retry.is_some();
        peer.probe = probe;
        peer.active += 1;
        let generation = peer.generation;
        state.active += 1;
        self.metrics
            .set_gauge(Gauge::PeerExchanges, state.active as u64);
        self.metrics.record(Event::PeerAdmissionAccepted, 1);
        if probe {
            self.metrics.record(Event::PeerProbe, 1);
        }
        Ok(Arc::new(Permit {
            owner: self.clone(),
            node: node.clone(),
            generation,
            probe,
        }))
    }
}
impl Permit {
    pub(crate) fn observe(&self, outcome: Outcome) {
        let now = now();
        let Ok(mut state) = self.owner.state.lock() else {
            return;
        };
        // Local quota pressure adapts only the node-wide admission limit.
        if outcome == Outcome::LocalPressure {
            let _ = self.owner.metrics.record(Event::PeerLocalPressure, 1);
            if now.saturating_duration_since(state.updated) >= BACKOFF {
                state.limit = (state.limit / 2).max(1);
                state.updated = now;
                self.owner
                    .metrics
                    .set_gauge(Gauge::PeerAdmissionLimit, state.limit as u64);
            }
            return;
        }
        if outcome == Outcome::Verified && now.saturating_duration_since(state.updated) >= RECOVERY
        {
            state.limit = (state.limit + 1).min(self.owner.config.total);
            state.updated = now;
            self.owner
                .metrics
                .set_gauge(Gauge::PeerAdmissionLimit, state.limit as u64);
        }
        let peer = state
            .peers
            .get_mut(&self.node)
            .expect("live permit retains peer");
        let event = match outcome {
            Outcome::Verified => Event::PeerVerified,
            Outcome::PeerFailure => Event::PeerLinkFailure,
            Outcome::LocalPressure => Event::PeerLocalPressure,
            Outcome::Neutral => return,
        };
        let _ = self.owner.metrics.record(event, 1);
        if peer.generation != self.generation {
            return;
        }
        match outcome {
            Outcome::PeerFailure => {
                peer.limit = (peer.limit / 2).max(1);
                peer.generation = peer.generation.saturating_add(1);
                peer.retry = Some(now + BACKOFF);
                peer.updated = now;
            }
            Outcome::Verified => {
                // Only the exclusive probe recovers an open circuit; old successes cannot.
                if peer.retry.is_some() && !self.probe {
                    return;
                }
                peer.retry = None;
                if now.saturating_duration_since(peer.updated) >= RECOVERY {
                    peer.limit = (peer.limit + 1).min(self.owner.config.per_peer);
                    peer.updated = now;
                }
            }
            Outcome::LocalPressure | Outcome::Neutral => {}
        }
    }
}
impl Drop for Permit {
    fn drop(&mut self) {
        let Ok(mut state) = self.owner.state.lock() else {
            return;
        };
        let peer = state
            .peers
            .get_mut(&self.node)
            .expect("live permit retains peer");
        peer.active -= 1;
        if self.probe {
            peer.probe = false;
            if peer.retry.is_some() {
                let now = now();
                peer.retry = Some(now + BACKOFF);
                peer.updated = now;
            }
        }
        state.active -= 1;
        self.owner
            .metrics
            .set_gauge(Gauge::PeerExchanges, state.active as u64);
    }
}

/// Worker-local identity and a handle to the sole node-wide incoming registry.
/// Outbound operations route directly from their retained membership lease.
pub struct PeerNetwork {
    pub local: NodeId,
    published: Arc<crate::control::PublishedState>,
}

impl PeerNetwork {
    pub fn new(local: NodeId, published: Arc<crate::control::PublishedState>) -> Result<Self> {
        if local.0.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self { local, published })
    }

    pub fn membership(
        &self,
        version: MembershipVersion,
    ) -> Result<std::sync::Arc<crate::topology::Membership>> {
        self.published.membership(version)
    }

    pub fn endpoint(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
        node: &NodeId,
    ) -> Result<crate::http::Endpoint> {
        if !membership.neighbors(&self.local)?.contains(node) {
            return Err(Error::InvalidRequest);
        }
        let member = membership.member(node)?;
        Ok(crate::http::Endpoint::Peer(member.peer_endpoint.clone()))
    }
}

/// Narrow a caller's scope to the signed route without creating a new cancellation
/// domain or extending the original deadline.
pub(crate) fn request_scope(
    request: &protocol::PeerRequest,
    scope: &RequestScope,
) -> Result<RequestScope> {
    scope.check()?;
    if request.route.request != scope.request
        || request.origin.request != scope.request
        || request.route.attempt != request.origin.attempt
    {
        return Err(Error::InvalidRequest);
    }
    let mut narrowed = scope.clone();
    narrowed.deadline.0 = narrowed
        .deadline
        .0
        .min(request.route.deadline.0)
        .min(request.origin.scope().deadline.0);
    narrowed.check()?;
    Ok(narrowed)
}

pub(crate) fn check_membership(
    request: &protocol::PeerRequest,
    membership: &std::sync::Arc<crate::topology::Membership>,
) -> Result<()> {
    if request.route.membership != membership.version {
        return Err(Error::IncompatibleMembership);
    }
    Ok(())
}

pub(crate) fn search_budget(
    route: &crate::topology::RouteBudget,
    local: &NodeId,
) -> Result<crate::topology::RouteBudget> {
    let mut budget = route.clone();
    if budget.visited.last() == Some(local) {
        budget.visited.pop();
    } else {
        budget.remaining_links = budget
            .remaining_links
            .checked_sub(1)
            .ok_or(Error::HopBudgetExhausted)?;
    }
    Ok(budget)
}
/// Opaque bounded transit with recorded reverse-path responses, never a page cache.
/// No decryption service is injected. Reverse-link failure terminates the attempt;
/// responses are not independently rerouted. Preserve encrypted credentials.
pub struct Relay {
    paths: Rc<Paths>,
    forwarding: Rc<Forwarding>,
    transport: Rc<Requester>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    network: Rc<PeerNetwork>,
}

impl Relay {
    pub fn new(
        paths: Rc<Paths>,
        forwarding: Rc<Forwarding>,
        transport: Rc<Requester>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        network: Rc<PeerNetwork>,
    ) -> Self {
        Self {
            paths,
            forwarding,
            transport,
            admission,
            network,
        }
    }

    /// Retain ingress binding and reverse path, append a signed request hop, and
    /// exchange complete envelopes. Verify the downstream response against that
    /// binding before appending a reverse hop. Never re-sign the original response.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{peer::{Relay, protocol::SignedRequest},
    ///     runtime::RequestScope, topology::std::sync::Arc<crate::topology::Membership>};
    /// fn unverified(relay: &Relay, request: SignedRequest,
    ///     membership: std::sync::Arc<crate::topology::Membership>, scope: &RequestScope) {
    ///     relay.forward(request, membership, scope);
    /// }
    /// ```
    pub fn forward<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            match self.forward_inner(request, membership, None, scope).await? {
                transport::RelayResponse::Complete(response) => Ok(response),
                _ => Err(Error::Internal),
            }
        })
    }

    pub(crate) fn forward_inner<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, transport::RelayResponse> {
        Box::pin(async move {
            let scope = request_scope(request.request(), scope)?;
            check_membership(request.request(), &membership)?;
            let network = &self.network;
            let budget = &request.request().route;
            if budget.destination == network.local || budget.visited.contains(&network.local) {
                return Err(Error::InvalidRequest);
            }
            let reservation = match relay.as_ref() {
                Some(reservation) => reservation.clone(),
                None => Rc::new(self.admission.reserve(None, ResourceClass::Relay, 1)?),
            };
            let search_budget = search_budget(budget, &network.local)?;
            let route = self
                .paths
                .shortest_async(membership.clone(), &network.local, &search_budget, &scope)
                .await?;
            let next = route.nodes.get(1).ok_or(Error::Unavailable)?;
            let previous = request
                .forwarders()
                .last()
                .unwrap_or(request.origin())
                .node()
                .clone();
            let binding = request.binding().clone();
            let mut visited = budget.visited.clone();
            visited.push(network.local.clone());
            let remaining_links = budget
                .remaining_links
                .checked_sub(1)
                .ok_or(Error::HopBudgetExhausted)?;
            let outbound_budget = crate::topology::RouteBudget {
                membership: budget.membership,
                request: budget.request,
                attempt: budget.attempt,
                destination: budget.destination.clone(),
                visited,
                remaining_links,
                remaining_attempts: budget.remaining_attempts,
                deadline: budget.deadline,
            };
            let outbound = self
                .forwarding
                .append_request(request, next, outbound_budget)?;
            let response = if relay.is_some() {
                self.transport
                    .exchange_relay(outbound, membership, reservation, &scope)
                    .await?
            } else {
                transport::RelayResponse::Complete(
                    self.transport
                        .exchange(outbound, membership, &scope)
                        .await?,
                )
            };
            scope.check()?;
            match response {
                transport::RelayResponse::Complete(response) => {
                    let response = self.forwarding.verify_response(response, &binding)?;
                    self.forwarding
                        .append_response(response, &previous)
                        .map(transport::RelayResponse::Complete)
                }
                transport::RelayResponse::Http {
                    authentication,
                    connection,
                    length,
                } => {
                    let authentication = self.forwarding.forward_opaque(
                        authentication,
                        length,
                        &binding,
                        &previous,
                    )?;
                    Ok(transport::RelayResponse::Http {
                        authentication,
                        connection,
                        length,
                    })
                }
            }
        })
    }
}

/// Concrete candidate requester. Scripted outcomes exist only in unit tests and
/// the subscription interoperability fixture, so cancellation and an adversarial
/// late-success completion fence remain distinct.
pub enum Requester {
    Network {
        #[cfg(test)]
        outbound_requests: std::cell::Cell<usize>,
        metrics: crate::telemetry::Metrics,
        observer: Observer,
        health: Rc<crate::topology::LinkHealth>,
        paths: Rc<Paths>,
        forwarding: Rc<Forwarding>,
        transfers: Rc<Transfers>,
        network: Rc<PeerNetwork>,
    },
    #[cfg(any(test, feature = "subscription-interop"))]
    Scripted {
        available: Box<
            dyn Fn(&std::sync::Arc<crate::topology::Membership>, &racer_control_wire::NodeId) -> bool,
        >,
        request: Box<
            dyn Fn(
                PeerRequest,
                std::sync::Arc<crate::topology::Membership>,
                RequestScope,
                bool,
            ) -> Operation<'static, VerifiedResponse>,
        >,
    },
}
impl Requester {
    #[cfg(any(test, feature = "subscription-interop"))]
    pub(crate) fn scripted<T: 'static>(
        state: Rc<T>,
        available: fn(
            &T,
            &std::sync::Arc<crate::topology::Membership>,
            &racer_control_wire::NodeId,
        ) -> bool,
        request: for<'a> fn(
            &'a T,
            PeerRequest,
            std::sync::Arc<crate::topology::Membership>,
            &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse>,
        direct: for<'a> fn(
            &'a T,
            PeerRequest,
            std::sync::Arc<crate::topology::Membership>,
            &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse>,
    ) -> Rc<Self> {
        let capability = state.clone();
        Rc::new(Self::Scripted {
            available: Box::new(move |m, n| available(&capability, m, n)),
            request: Box::new(move |r, m, scope, is_direct| {
                let state = state.clone();
                Box::pin(async move {
                    if is_direct {
                        direct(&state, r, m, &scope).await
                    } else {
                        request(&state, r, m, &scope).await
                    }
                })
            }),
        })
    }
    pub fn new(
        paths: Rc<Paths>,
        forwarding: Rc<Forwarding>,
        transfers: Rc<Transfers>,
        network: Rc<PeerNetwork>,
    ) -> Self {
        Self::Network {
            #[cfg(test)]
            outbound_requests: std::cell::Cell::new(0),
            metrics: crate::telemetry::Metrics::default(),
            observer: Observer::default(),
            health: paths.link_health(),
            paths,
            forwarding,
            transfers,
            network,
        }
    }
    pub(crate) fn with_observer(mut self, observer: Observer) -> Self {
        match &mut self {
            Self::Network {
                observer: target, ..
            } => *target = observer,
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted { .. } => {}
        }
        self
    }
    pub(crate) fn with_metrics(mut self, metrics: crate::telemetry::Metrics) -> Self {
        match &mut self {
            Self::Network {
                metrics: target, ..
            } => *target = metrics,
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted { .. } => {}
        }
        self
    }
    #[cfg(test)]
    pub(crate) fn admission(&self) -> &std::sync::Arc<AdaptivePeers> {
        match self {
            Self::Network { paths, .. } => paths.peer_admission.as_ref().unwrap(),
            _ => panic!("scripted requester has no network admission"),
        }
    }
    pub fn direct_hedge_available(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
        destination: &racer_control_wire::NodeId,
    ) -> bool {
        match self {
            Self::Network {
                network,
                health,
                paths,
                ..
            } => {
                network.endpoint(membership, destination).is_ok()
                    && health.available(destination).unwrap_or(false)
                    && paths
                        .peer_admission
                        .as_ref()
                        .is_some_and(|a| a.hedge_available(destination))
            }
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted { available, .. } => available(membership, destination),
        }
    }
    pub fn request_direct<'a>(
        &'a self,
        request: PeerRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        match self {
            Self::Network { .. } => self.request_direct_network(request, membership, scope),
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted {
                request: script, ..
            } => script(request, membership, scope.clone(), true),
        }
    }
    pub fn request<'a>(
        &'a self,
        request: PeerRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        match self {
            Self::Network { .. } => self.request_network(request, membership, scope),
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted {
                request: script, ..
            } => script(request, membership, scope.clone(), false),
        }
    }
}
/// Owned signed-envelope exchange shared by requesters and opaque relays.
/// Receiving a signed response does not authenticate it: callers must verify it
/// against their retained request binding before use or reverse forwarding.
/// Requesters handle both complete and opaque I/O.
///
/// ```no_run
/// use racer_dataplane::{error::Result, peer::{Requester,
///     Relay, server::PeerServer,
///     protocol::{PeerRequest, SignedRequest, SignedResponse}, forwarding::VerifiedResponse},
///     runtime::RequestScope, peer::forwarding::Forwarding,
///     topology::std::sync::Arc<crate::topology::Membership>};
/// async fn interfaces(
///     client: &Requester, transport: &Requester,
///     server: &PeerServer, relay: &Relay, auth: &Forwarding,
///     local: PeerRequest, wire: SignedRequest, outbound: SignedRequest,
///     inbound: SignedRequest, membership: std::sync::Arc<crate::topology::Membership>, scope: &RequestScope,
/// ) -> Result<()> {
///     let verified: VerifiedResponse = client.request(local, membership.clone(), scope).await?;
///     let _wire_response: SignedResponse = verified.into_signed();
///     let admitted = auth.verify_request(wire)?;
///     let reply = relay.forward(admitted, membership.clone(), scope).await?;
///     let _retained_chain = reply.authentication;
///     // Both network boundaries preserve envelopes in each direction.
///     let _exchange: SignedResponse = transport.exchange(outbound, membership, scope).await?;
///     let _dispatch: SignedResponse = server.dispatch(inbound, scope).await?;
///     Ok(())
/// }
/// ```
impl Requester {
    fn request_direct_network<'a>(
        &'a self,
        request: PeerRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        let (forwarding, metrics) = match self {
            Self::Network {
                forwarding,
                metrics,
                ..
            } => (forwarding, metrics),
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted { .. } => {
                unreachable!("scripted requests are dispatched before network I/O")
            }
        };
        Box::pin(async move {
            if !matches!(request.operation, protocol::Operation::Page { .. }) {
                return Err(Error::InvalidRequest);
            }
            let scope = request_scope(&request, scope)?;
            let next = request.route.destination.clone();
            if !self.direct_hedge_available(&membership, &next) {
                return Err(Error::Overloaded);
            }
            let (signed, binding) = forwarding.sign_request_to(request, &next)?;
            let mut timing = PageTiming::new(metrics);
            let response = self
                .exchange_inner_mode(signed, membership, None, &scope, true, Some(&mut timing))
                .await?;
            let transport::RelayResponse::Complete(response) = response else {
                return Err(Error::Internal);
            };
            scope.check()?;
            let response = forwarding.verify_response(response, &binding)?;
            timing.success(&response);
            Ok(response)
        })
    }
    /// Sign a fresh attempt, exchange the full envelope, then verify the response
    /// using the binding retained from signing. Logical callers retain the proof.
    fn request_network<'a>(
        &'a self,
        request: PeerRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        let (network, paths, observer, forwarding, metrics) = match self {
            Self::Network {
                network,
                paths,
                observer,
                forwarding,
                metrics,
                ..
            } => (network, paths, observer, forwarding, metrics),
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted { .. } => {
                unreachable!("scripted requests are dispatched before network I/O")
            }
        };
        Box::pin(async move {
            let scope = request_scope(&request, scope)?;
            check_membership(&request, &membership)?;
            let search_budget = search_budget(&request.route, &network.local)?;
            let route = observer.result(
                Stage::PeerRoute,
                &scope,
                paths
                    .shortest_async(membership.clone(), &network.local, &search_budget, &scope)
                    .await,
            )?;
            let next = route.nodes.get(1).ok_or(Error::Unavailable)?;
            let (signed, binding) = forwarding.sign_request_to(request, next)?;
            let mut timing = PageTiming::new(metrics);
            let response = self
                .exchange_inner_mode(signed, membership, None, &scope, false, Some(&mut timing))
                .await?;
            let transport::RelayResponse::Complete(response) = response else {
                return Err(Error::Internal);
            };
            scope.check()?;
            let response = observer.result(
                Stage::PeerVerify,
                &scope,
                forwarding.verify_response(response, &binding),
            )?;
            timing.success(&response);
            Ok(response)
        })
    }
}
impl Requester {
    #[cfg(test)]
    fn outbound_requests(&self) -> usize {
        match self {
            Self::Network {
                outbound_requests, ..
            } => outbound_requests.get(),
            Self::Scripted { .. } => panic!("scripted requester has no network attempts"),
        }
    }
    pub fn exchange_relay<'a>(
        &'a self,
        request: SignedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        reservation: Rc<flow_control::Charge<AdmissionPolicy>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, transport::RelayResponse> {
        self.exchange_inner(request, membership, Some(reservation), scope)
    }
    /// Use this exact lease for the signed route; never resolve its version again.
    pub fn exchange<'a>(
        &'a self,
        request: SignedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            match self
                .exchange_inner(request, membership, None, scope)
                .await?
            {
                transport::RelayResponse::Complete(response) => Ok(response),
                _ => Err(Error::Internal),
            }
        })
    }
}
impl Requester {
    fn exchange_inner<'a>(
        &'a self,
        request: SignedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, transport::RelayResponse> {
        self.exchange_inner_mode(request, membership, relay, scope, false, None)
    }
    fn exchange_inner_mode<'a>(
        &'a self,
        request: SignedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        scope: &'a RequestScope,
        direct_http: bool,
        timing: Option<&'a mut PageTiming<'_>>,
    ) -> Operation<'a, transport::RelayResponse> {
        let (network, paths, health, forwarding, transfers) = match self {
            Self::Network {
                network,
                paths,
                health,
                forwarding,
                transfers,
                ..
            } => (network, paths, health, forwarding, transfers),
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted { .. } => panic!("scripted candidate cannot exchange opaque envelopes"),
        };
        Box::pin(async move {
            #[cfg(test)]
            if let Self::Network {
                outbound_requests, ..
            } = self
            {
                outbound_requests.set(outbound_requests.get() + 1);
            }
            let scope = request_scope(&request.request, scope)?;
            check_membership(&request.request, &membership)?;
            let budget = &request.request.route;
            let signed_head = request
                .authentication
                .hops
                .last()
                .unwrap_or(&request.authentication.original);
            let next = crate::peer::protocol::receiver(&signed_head.head)?;
            // A signature selects the next receiver. Never reroute this envelope
            // independently after signing, even if link health changes.
            let endpoint = network.endpoint(&membership, &next)?;
            // The hint chooses a candidate rail, never the provider's page order.
            // The sender validates the actual selected page against that rail and
            // falls back to HTTP before exporting a window when they disagree.
            let rail_hint = match &request.request.operation {
                protocol::Operation::Page { page, .. } => Some(page.clone()),
                protocol::Operation::Subscribe { subscription, .. } => subscription
                    .demand
                    .intervals()
                    .first()
                    .map(|interval| crate::model::PageId {
                        version: subscription.version.clone(),
                        number: crate::model::PageNumber(interval.start),
                    }),
                _ => None,
            };
            let plan = if direct_http {
                if next != budget.destination {
                    return Err(Error::InvalidRequest);
                }
                crate::rdma::TransportPlan::Http
            } else if let Some(page) = rail_hint {
                let search = search_budget(budget, &network.local)?;
                let route = paths
                    .shortest_async(membership.clone(), &network.local, &search, &scope)
                    .await?;
                if route.nodes.get(1) != Some(&next) {
                    return Err(Error::Unavailable);
                }
                rdma::select_hop(&route, &page, &network.local, &next)?
            } else {
                crate::rdma::TransportPlan::Http
            };
            let _probe = health.acquire(&next)?;
            let permit = paths
                .peer_admission
                .as_ref()
                .map(|a| a.acquire(&next))
                .transpose()?;
            let binding = forwarding.outbound_binding(&request)?;
            let socket_failure = Rc::new(std::cell::Cell::new(false));
            let response = transfers
                .exchange_timed(
                    endpoint,
                    request,
                    plan,
                    Some(membership.clone()),
                    relay,
                    permit.clone(),
                    socket_failure.clone(),
                    timing,
                    &scope,
                )
                .await;
            // Receiving a signed envelope is not proof. Recover only after the
            // complete reverse chain and original request binding are verified.
            let response = response.and_then(|response| self.verify_exchange(response, &binding));
            use crate::topology::LinkOutcome;
            let outcome = match &response {
                Ok(transport::RelayResponse::Complete(_)) => Some(LinkOutcome::Success),
                Err(_) if permit.is_none() && socket_failure.get() => Some(LinkOutcome::Refused),
                // Unavailable/deadline/protocol errors can arise locally or at a
                // downstream node. Do not blame an immediate peer without evidence.
                _ => None,
            };
            if let Some(outcome) = outcome {
                health.observe(&next, outcome)?;
            }
            if let Some(permit) = permit {
                permit.observe(match &response {
                    // A downstream overload is not attributable to the immediate
                    // peer. It also must not increase its admission or recover a probe.
                    Ok(transport::RelayResponse::Complete(response))
                        if matches!(response.response, protocol::PeerResponse::Overloaded) =>
                    {
                        Outcome::Neutral
                    }
                    Ok(transport::RelayResponse::Complete(_)) => Outcome::Verified,
                    Err(Error::Overloaded) => Outcome::LocalPressure,
                    _ => Outcome::Neutral,
                });
            }
            drop(membership);
            response
        })
    }

    fn verify_exchange(
        &self,
        response: transport::RelayResponse,
        binding: &crate::peer::forwarding::RequestBinding,
    ) -> Result<transport::RelayResponse> {
        let forwarding = match self {
            Self::Network { forwarding, .. } => forwarding,
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted { .. } => {
                unreachable!("scripted candidate cannot exchange opaque envelopes")
            }
        };
        match response {
            transport::RelayResponse::Complete(response) => forwarding
                .verify_response(response, binding)
                .map(|verified| transport::RelayResponse::Complete(verified.into_signed())),
            transport::RelayResponse::Http {
                authentication,
                mut connection,
                length,
            } => {
                forwarding.verify_opaque(&authentication, length, binding)?;
                // All Racer outcomes use HTTP 200. Inspect the authenticated
                // outcome, exactly as the materialized path does, not HTTP status.
                connection.state_mut().peer_response_verified =
                    crate::peer::protocol::field(&authentication.original.head, "racer-outcome")?
                        != "overloaded";
                Ok(transport::RelayResponse::Http {
                    authentication,
                    connection,
                    length,
                })
            }
        }
    }
}

#[cfg(test)]
pub(crate) mod tests;
