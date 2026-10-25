//! Correlated logical requests with monotonic budgets, attempts, and cancellation.
use self::forwarding::Forwarding;
use self::forwarding::VerifiedRequest;
use self::forwarding::VerifiedResponse;
use self::protocol::PeerRequest;
use self::protocol::SignedRequest;
use self::protocol::SignedResponse;
use self::transport::Transfers;
use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::rdma;
use crate::rdma::TransportPlan;
use crate::runtime::RequestScope;
use crate::telemetry::Event;
use crate::telemetry::Gauge;
use crate::telemetry::Metrics;
use crate::telemetry::Observer;
use crate::telemetry::Stage;
use crate::topology::Paths;
use racer_control_wire::MembershipVersion;
use racer_control_wire::NodeId;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;
use std::time::Instant;
use uring_runtime::environment::now;

pub mod forwarding;
pub mod protocol;
pub mod receive;
pub mod server;
pub mod subscriptions;
pub mod transport;

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
                self.metrics.record(sum, duration);
                self.metrics.record(count, 1);
            }
            self.active = false;
        }
    }
}
impl Drop for PageTiming<'_> {
    fn drop(&mut self) {
        if self.active {
            self.metrics.record(Event::PeerPageCensored, 1);
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
    inner: Arc<flow_control::Adaptive<NodeId, AdmissionObserver>>,
    shard: usize,
}
pub(crate) use flow_control::AdaptiveOutcome as Outcome;
pub(crate) type Permit = flow_control::AdaptivePermit<NodeId, AdmissionObserver>;
pub(crate) struct AdmissionObserver(Metrics);
impl flow_control::AdaptiveObserver for AdmissionObserver {
    fn event(&self, event: flow_control::AdaptiveEvent) {
        self.event_on(self.0.writer_shard(), event);
    }
    fn event_on(&self, shard: usize, event: flow_control::AdaptiveEvent) {
        use flow_control::AdaptiveEvent as AdmissionEvent;
        self.0.record_on(
            shard,
            match event {
                AdmissionEvent::Rejected => Event::PeerAdmissionRejected,
                AdmissionEvent::CircuitRejected => Event::PeerCircuitRejected,
                AdmissionEvent::Accepted => Event::PeerAdmissionAccepted,
                AdmissionEvent::Probe => Event::PeerProbe,
                AdmissionEvent::LocalPressure => Event::PeerLocalPressure,
                AdmissionEvent::Verified => Event::PeerVerified,
                AdmissionEvent::LinkFailure => Event::PeerLinkFailure,
            },
            1,
        );
    }
    fn active(&self, active: usize) {
        self.0.set_gauge(Gauge::PeerExchanges, active as u64);
    }
    fn limit(&self, limit: usize) {
        self.0.set_gauge(Gauge::PeerAdmissionLimit, limit as u64);
    }
}
impl AdaptivePeers {
    pub(crate) fn hedge_available(&self, node: &NodeId) -> bool {
        self.inner.hedge_available(node)
    }
    pub(crate) fn new(config: Config, metrics: Metrics) -> Result<Arc<Self>> {
        Self::with_clock(config, metrics, now)
    }
    /// As `new`, reading time from `clock` instead of the runtime clock. A
    /// simulator passes its virtual clock; `new` passes the runtime clock, so
    /// production pays nothing for the parameter.
    pub(crate) fn with_clock(
        config: Config,
        metrics: Metrics,
        clock: fn() -> std::time::Instant,
    ) -> Result<Arc<Self>> {
        config.validate()?;
        Ok(Arc::new(Self {
            shard: metrics.writer_shard(),
            inner: flow_control::Adaptive::new(
                flow_control::AdaptiveConfig {
                    total: config.total,
                    per_key: config.per_peer,
                    capacity: CAPACITY,
                    backoff: BACKOFF,
                    recovery: RECOVERY,
                    retire_after: Duration::from_secs(60),
                },
                AdmissionObserver(metrics),
                clock,
            )?,
        }))
    }
    pub(crate) fn available(&self, node: &NodeId) -> bool {
        self.inner.available(node)
    }
    pub(crate) fn for_worker(&self, metrics: &Metrics) -> Arc<Self> {
        Arc::new(Self {
            inner: self.inner.clone(),
            shard: metrics.writer_shard(),
        })
    }
    pub(crate) fn acquire(self: &Arc<Self>, node: &NodeId) -> Result<Arc<Permit>> {
        self.inner.acquire_on(node, self.shard).map_err(Into::into)
    }
}

/// Worker-local identity and a handle to the sole node-wide incoming registry.
/// Outbound operations route directly from their retained membership lease.
pub struct PeerNetwork {
    pub local: NodeId,
    published: Arc<controlplane::Published<crate::control::Snapshot>>,
}

impl PeerNetwork {
    pub fn new(
        local: NodeId,
        published: Arc<controlplane::Published<crate::control::Snapshot>>,
    ) -> Result<Self> {
        if local.0.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self { local, published })
    }

    pub fn membership(
        &self,
        version: MembershipVersion,
    ) -> Result<std::sync::Arc<crate::topology::Membership>> {
        self.published
            .resolve(version.0, uring_runtime::environment::now())?
            .ok_or(Error::IncompatibleMembership)
    }

    /// Membership of the newest accepted publication.
    pub fn current_membership(&self) -> Result<std::sync::Arc<crate::topology::Membership>> {
        Ok(self
            .published
            .current()?
            .ok_or(Error::IncompatibleMembership)?
            .membership
            .clone())
    }

    pub fn endpoint(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
        node: &NodeId,
    ) -> Result<crate::http::Endpoint> {
        membership.position(&self.local)?;
        if !membership.is_neighbor(&self.local, node) {
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
    let (keep, remaining_links) =
        racer_core::route::search_budget(&route.visited, route.remaining_links, local)?;
    let mut budget = route.clone();
    budget.visited.truncate(keep);
    budget.remaining_links = remaining_links;
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
    /// ```compile_fail,E0308
    /// use racer_dataplane::{peer::{Relay, protocol::SignedRequest},
    ///     runtime::RequestScope, topology::Membership};
    /// fn unverified(relay: &Relay, request: SignedRequest,
    ///     membership: std::sync::Arc<Membership>, scope: &RequestScope) {
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
            let remaining_links = racer_core::route::relay(
                &budget.visited,
                &budget.destination,
                budget.remaining_links,
                &network.local,
            );
            if remaining_links == Err(racer_core::route::RouteFault::Invalid) {
                return Err(Error::InvalidRequest);
            }
            let reservation = match relay.as_ref() {
                Some(reservation) => reservation.clone(),
                None => Rc::new(self.admission.reserve(None, ResourceClass::Relay, 1)?),
            };
            // A destination this node holds silent fails at once (Known
            // issues 41 and 43): signed `Unavailable`, as for no route.
            if self.paths.link_health().silent(&budget.destination) {
                return Err(Error::Unavailable);
            }
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
            let remaining_links = remaining_links?;
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
                    .exchange_inner(outbound, membership, Some(reservation), &scope)
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
                    let response = self
                        .forwarding
                        .verify_response_async(response, &binding, &scope)
                        .await?;
                    self.forwarding
                        .append_response(response, &previous)
                        .map(transport::RelayResponse::Complete)
                }
                // A relay never asks for landing.
                transport::RelayResponse::Landed(_) => Err(Error::Internal),
                transport::RelayResponse::Http {
                    authentication,
                    connection,
                    length,
                } => {
                    let authentication = self
                        .forwarding
                        .forward_opaque_async(authentication, length, &binding, &previous, &scope)
                        .await?;
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

/// What [`Requester::request_landing`] produced.
#[allow(clippy::large_enum_variant)] // Short-lived, returned once per request.
pub(crate) enum Delivered {
    /// A verified response, as from [`Requester::request`].
    Response(VerifiedResponse),

    /// A verified page in a GPU landing buffer.
    Landed(Box<transport::LandedResponse>),
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
        #[expect(
            clippy::type_complexity,
            reason = "Keep the concrete membership owner visible instead of restoring a lease alias"
        )]
        available: Box<
            dyn Fn(
                &std::sync::Arc<crate::topology::Membership>,
                &racer_control_wire::NodeId,
            ) -> bool,
        >,
        #[expect(
            clippy::type_complexity,
            reason = "The scripted callback mirrors the owned production request without a facade"
        )]
        request: Box<
            dyn Fn(
                PeerRequest,
                std::sync::Arc<crate::topology::Membership>,
                RequestScope,
                bool,
            ) -> Operation<'static, VerifiedResponse>,
        >,
        /// Test-only GPU landing: the landing GPU, and a script that returns
        /// a landed page for a request or None to answer it as a plain request.
        #[cfg(test)]
        landing: Option<(i32, ScriptedLanding)>,
        /// Silence state the candidate policy keeps for scripted peers.
        health: Rc<crate::topology::LinkHealth>,
    },
}

/// A scripted landed page for one request, or None.
#[cfg(test)]
pub(crate) type ScriptedLanding = Box<dyn Fn(&PeerRequest) -> Option<transport::LandedResponse>>;
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
            health: Rc::new(crate::topology::LinkHealth::default()),
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
            #[cfg(test)]
            landing: None,
        })
    }

    /// Test-only: let a scripted requester land pages on GPU `ordinal`.
    #[cfg(test)]
    pub(crate) fn with_scripted_landing(
        self: Rc<Self>,
        ordinal: i32,
        land: ScriptedLanding,
    ) -> Rc<Self> {
        let mut requester = Rc::try_unwrap(self)
            .ok()
            .expect("unshared scripted requester");
        if let Self::Scripted { landing, .. } = &mut requester {
            *landing = Some((ordinal, land));
        }
        Rc::new(requester)
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
///     topology::Membership};
/// async fn interfaces(
///     client: &Requester, transport: &Requester,
///     server: &PeerServer, relay: &Relay, auth: &Forwarding,
///     local: PeerRequest, wire: SignedRequest, outbound: SignedRequest,
///     inbound: SignedRequest, membership: std::sync::Arc<Membership>, scope: &RequestScope,
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
                .exchange_inner_mode(
                    signed,
                    membership,
                    None,
                    &scope,
                    true,
                    Some(&mut timing),
                    None,
                )
                .await?;
            let transport::RelayResponse::Complete(response) = response else {
                return Err(Error::Internal);
            };
            scope.check()?;
            let response = forwarding
                .verify_response_async(response, &binding, &scope)
                .await?;
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
        Box::pin(async move {
            match self
                .request_network_landing(request, membership, None, scope)
                .await?
            {
                Delivered::Response(response) => Ok(response),
                Delivered::Landed(_) => Err(Error::Internal),
            }
        })
    }

    /// Like [`Self::request`] for one fixed page that a `gpu-read` will
    /// decrypt on GPU `ordinal`. If the last hop is RDMA and this node has a
    /// free landing slot on that GPU's rail, the page lands in GPU memory and
    /// comes back as [`Delivered::Landed`]. Otherwise this is a plain request.
    pub(crate) fn request_landing<'a>(
        &'a self,
        request: PeerRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        ordinal: i32,
        scope: &'a RequestScope,
    ) -> Operation<'a, Delivered> {
        match self {
            Self::Network { .. } => {
                self.request_network_landing(request, membership, Some(ordinal), scope)
            }
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted {
                request: script,
                #[cfg(test)]
                landing,
                ..
            } => {
                #[cfg(test)]
                if let Some((own, land)) = landing
                    && *own == ordinal
                    && let Some(landed) = land(&request)
                {
                    return Box::pin(async move { Ok(Delivered::Landed(Box::new(landed))) });
                }
                let response = script(request, membership, scope.clone(), false);
                Box::pin(async move { response.await.map(Delivered::Response) })
            }
        }
    }

    /// Whether this node has a landing buffer for GPU `ordinal` on any rail.
    pub(crate) fn landing_ready(&self, ordinal: i32) -> bool {
        match self {
            Self::Network { transfers, .. } => transfers.landing_ready(ordinal),
            #[cfg(test)]
            Self::Scripted {
                landing: Some((own, _)),
                ..
            } => *own == ordinal,
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted { .. } => false,
        }
    }

    fn request_network_landing<'a>(
        &'a self,
        request: PeerRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        landing: Option<i32>,
        scope: &'a RequestScope,
    ) -> Operation<'a, Delivered> {
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
                .exchange_inner_mode(
                    signed,
                    membership,
                    None,
                    &scope,
                    false,
                    Some(&mut timing),
                    landing,
                )
                .await?;
            let response = match response {
                transport::RelayResponse::Complete(response) => response,
                // Verified by exchange_inner_mode against `binding`.
                transport::RelayResponse::Landed(landed) => {
                    scope.check()?;
                    return Ok(Delivered::Landed(landed));
                }
                transport::RelayResponse::Http { .. } => return Err(Error::Internal),
            };
            scope.check()?;
            let response = observer.result(
                Stage::PeerVerify,
                &scope,
                forwarding
                    .verify_response_async(response, &binding, &scope)
                    .await,
            )?;
            timing.success(&response);
            Ok(Delivered::Response(response))
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
    /// This worker's link health (silence included, Known issues 41 and
    /// 43).
    pub(crate) fn link_health(&self) -> Rc<crate::topology::LinkHealth> {
        match self {
            Self::Network { health, .. } => health.clone(),
            #[cfg(any(test, feature = "subscription-interop"))]
            Self::Scripted { health, .. } => health.clone(),
        }
    }
    fn exchange_inner<'a>(
        &'a self,
        request: SignedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, transport::RelayResponse> {
        self.exchange_inner_mode(request, membership, relay, scope, false, None, None)
    }
    #[allow(clippy::too_many_arguments)] // Keep transport, routing, and landing inputs explicit.
    fn exchange_inner_mode<'a>(
        &'a self,
        request: SignedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        scope: &'a RequestScope,
        direct_http: bool,
        timing: Option<&'a mut PageTiming<'_>>,
        landing: Option<i32>,
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
            // The last link: this sender learns whether the destination
            // answers (Known issues 41 and 43).
            let last_page = match &request.request.operation {
                protocol::Operation::Page { page, .. } if next == budget.destination => {
                    Some(page.clone())
                }
                _ => None,
            };
            let receive_permit = transfers.admit_receive(&request.request, &scope).await?;
            let _probe = health.acquire(&next)?;
            let sent = uring_runtime::environment::now();
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
                    receive_permit,
                    socket_failure.clone(),
                    timing,
                    landing,
                    &scope,
                )
                .await;
            // Receiving a signed envelope is not proof. Recover only after the
            // complete reverse chain and original request binding are verified.
            let response = match response {
                Ok(response) => self.verify_exchange(response, &binding, &scope).await,
                Err(error) => Err(error),
            };
            use racer_core::peer::hop;
            let exchange = match &response {
                Ok(transport::RelayResponse::Complete(response)) => hop::Exchange::Complete {
                    overloaded: matches!(response.response, protocol::PeerResponse::Overloaded),
                },
                // A landed page was verified by the transport, like a complete response.
                Ok(transport::RelayResponse::Landed(_)) => {
                    hop::Exchange::Complete { overloaded: false }
                }
                Ok(_) => hop::Exchange::Streaming,
                Err(error) => hop::Exchange::Failed {
                    fault: error.fault(),
                    socket_failure: socket_failure.get(),
                },
            };
            // Unavailable/deadline/protocol errors can arise locally or at a
            // downstream node. Do not blame an immediate peer without evidence.
            if let Some(outcome) = hop::exchange_link_outcome(exchange, permit.is_some()) {
                health.observe(&next, outcome)?;
            }
            if let Some(permit) = permit {
                // A downstream overload is not attributable to the immediate
                // peer. It also must not increase its admission or recover a probe.
                permit.observe(hop::exchange_admission_outcome(exchange));
            }
            if let Some(page) = last_page {
                match hop::silence_evidence(exchange) {
                    Some(true) => health.silence_answered(&next),
                    // `CandidatePolicy::poll_pings` sends the ping.
                    Some(false) => {
                        let _ = health.silence_timed_out(
                            &next,
                            page,
                            sent,
                            racer_core::peer::silence::LINK_TIMEOUTS,
                        );
                    }
                    None => {}
                }
            }
            drop(membership);
            response
        })
    }

    async fn verify_exchange(
        &self,
        response: transport::RelayResponse,
        binding: &crate::peer::forwarding::RequestBinding,
        scope: &RequestScope,
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
                .verify_response_async(response, binding, scope)
                .await
                .map(|verified| transport::RelayResponse::Complete(verified.into_signed())),
            transport::RelayResponse::Landed(landed) => {
                forwarding.verify_landed(
                    &landed.authentication,
                    &landed.metadata,
                    &landed.envelope,
                    binding,
                )?;
                Ok(transport::RelayResponse::Landed(landed))
            }
            transport::RelayResponse::Http {
                authentication,
                mut connection,
                length,
            } => {
                let authentication = forwarding
                    .verify_opaque_async(authentication, length, binding, scope)
                    .await?;
                // All Racer outcomes use HTTP 200. Inspect the authenticated
                // outcome, exactly as the materialized path does, not HTTP status.
                connection.state_mut().exchange.verified_response(
                    crate::peer::protocol::field(&authentication.original.head, "racer-outcome")?
                        != "overloaded",
                );
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
