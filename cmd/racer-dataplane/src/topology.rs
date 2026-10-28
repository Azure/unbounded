use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::AttemptId;
use crate::model::ObjectId;
use crate::model::PageNumber;
use crate::model::RequestId;
use crate::runtime::RequestScope;
#[cfg(test)]
use racer_control_wire::CacheId;
use racer_control_wire::MembershipVersion;
use racer_control_wire::NodeId;
use racer_control_wire::RailMapping;
use sha2::Digest;
use sha2::Sha256;
use std::future::Future;
use std::num::NonZeroU32;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Poll;
use std::time::Duration;
use std::time::Instant;
use uring_runtime::environment::Deadline;

pub use ::topology::MAX_DEGREE;
pub use ::topology::{Maintenance, SLOT_COUNT};

// Application wire/hash schemas are independent of the topology implementation.
pub(crate) use ::topology::hash::{finish as hash_finish, named_domain as hash_domain};

/// Append an application field whose wire validation already bounds its length.
pub(crate) fn hash_bytes(hash: &mut Sha256, value: &[u8]) {
    ::topology::hash::try_bytes(hash, value).expect("validated hash field fits u32");
}

// Immutable placement, authenticated routing, and worker-local endpoint circuits.

/// Shared capacity bound for every supported topology, including first-hop masks.
const _: () = assert!(MAX_DEGREE <= u64::BITS as usize);

// Worker-local link circuits, distinct from process readiness and placement.
pub struct LinkHealth {
    inner: flow_control::Circuits<NodeId>,
    /// Destinations that accept requests and never answer them (Known
    /// issues 41 and 43, `racer_core::liveness`). A ping carries the
    /// page of the last unanswered exchange.
    silence: std::cell::RefCell<Silence<NodeId, crate::model::PageId>>,
}
type Silence<N, P> = racer_core::liveness::Silence<N, P>;
pub type LinkProbe<'a> = flow_control::Probe<'a, NodeId>;
impl Default for LinkHealth {
    /// Construct independent worker-local circuit state for all possible neighbors.
    fn default() -> Self {
        Self::new(MAX_DEGREE)
    }
}
pub use racer_core::route::LinkOutcome;
impl LinkHealth {
    pub fn acquire(&self, node: &NodeId) -> Result<LinkProbe<'_>> {
        self.acquire_at(node, uring_runtime::environment::now())
    }
    pub fn acquire_at(&self, node: &NodeId, now: Instant) -> Result<LinkProbe<'_>> {
        self.inner.acquire(node, now).map_err(Into::into)
    }
    /// Application misses and credential rejection do not open transport circuits.
    pub async fn run<T>(
        &self,
        endpoint: &NodeId,
        operation: impl std::future::Future<Output = Result<T>>,
    ) -> Result<T> {
        let _probe = self.acquire(endpoint)?;
        let result = operation.await;
        let outcome = racer_core::route::operation_link_outcome(
            result.as_ref().map(|_| ()).map_err(Error::fault),
        );
        if let Some(outcome) = outcome {
            let _ = self.observe(endpoint, outcome);
        }
        result
    }
    pub fn new(capacity: usize) -> Self {
        Self {
            inner: flow_control::Circuits::new(capacity, Duration::from_secs(1)),
            silence: std::cell::RefCell::new(Silence::new(
                racer_core::liveness::SILENCE_CAPACITY,
                racer_core::liveness::HANG_AFTER,
                SILENCE_PING_TIMEOUT,
            )),
        }
    }
    /// Silence timing (`racer_core::liveness::Silence::new`):
    /// `hang_after` is half the client request timeout and `ping_timeout`
    /// the peer attempt timeout.
    pub fn with_silence(self, hang_after: Duration, ping_timeout: Duration) -> Self {
        self.set_silence(hang_after, ping_timeout);
        self
    }
    /// [`Self::with_silence`] on a shared handle; forgets every peer.
    pub fn set_silence(&self, hang_after: Duration, ping_timeout: Duration) {
        *self.silence.borrow_mut() = Silence::new(
            racer_core::liveness::SILENCE_CAPACITY,
            hang_after,
            ping_timeout,
        );
    }
    /// An exchange for `page` sent to `peer` at `sent` ended unanswered; see
    /// `Silence::timed_out`. Returns whether a ping is wanted.
    pub fn silence_timed_out(
        &self,
        peer: &NodeId,
        page: crate::model::PageId,
        sent: Instant,
        needed: u32,
    ) -> bool {
        self.silence.borrow_mut().timed_out(
            peer.clone(),
            page,
            sent,
            uring_runtime::environment::now(),
            needed,
        )
    }
    /// `peer` answered an exchange: it is not hung.
    pub fn silence_answered(&self, peer: &NodeId) {
        self.silence
            .borrow_mut()
            .answered(peer, uring_runtime::environment::now());
    }
    /// `peer` itself shed an exchange for `page` (an `Overloaded` it
    /// signed): it is alive and busy; see `Silence::shed` (Known issue 54).
    pub fn silence_shed(&self, peer: &NodeId, page: crate::model::PageId) {
        self.silence
            .borrow_mut()
            .shed(peer.clone(), page, uring_runtime::environment::now());
    }
    /// At most `max` pings due now, each marked outstanding
    /// (`Silence::due_pings`).
    pub fn due_pings(&self, now: Instant, max: usize) -> Vec<(NodeId, crate::model::PageId)> {
        self.silence.borrow_mut().due_pings(now, max)
    }
    /// Outcome of a ping sent for `peer`: answered by it (`Some(true)`),
    /// left unanswered or refused by a relay that cannot reach it
    /// (`Some(false)`), or inconclusive (`None`). Returns whether `peer`
    /// became silent.
    pub fn ping_outcome(&self, peer: &NodeId, answered: Option<bool>, now: Instant) -> bool {
        let mut silence = self.silence.borrow_mut();
        match answered {
            Some(true) => {
                silence.ping_answered(peer, now);
                false
            }
            Some(false) => silence.ping_failed(peer, now),
            None => false,
        }
    }
    /// What this worker last learned about `peer`'s answers (readers'
    /// visit order, Known issue 41).
    pub fn answering(&self, peer: &NodeId) -> racer_core::read::Answering {
        use racer_core::read::Answering;
        let now = uring_runtime::environment::now();
        let silence = self.silence.borrow();
        if silence.silent(peer, now) {
            Answering::Silent
        } else if silence.unanswered(peer, now) {
            Answering::Unanswered
        } else {
            Answering::Answers
        }
    }
    /// Whether `peer` is silent: requests for it fail at once.
    pub fn silent(&self, peer: &NodeId) -> bool {
        self.silent_at(peer, uring_runtime::environment::now())
    }
    pub fn silent_at(&self, peer: &NodeId, now: Instant) -> bool {
        self.silence.borrow().silent(peer, now)
    }
    pub fn observe(&self, neighbor: &NodeId, outcome: LinkOutcome) -> Result<()> {
        self.observe_at(neighbor, outcome, uring_runtime::environment::now())
    }
    pub fn observe_at(&self, neighbor: &NodeId, outcome: LinkOutcome, now: Instant) -> Result<()> {
        if matches!(outcome, LinkOutcome::Success) {
            self.inner.success(neighbor);
            return Ok(());
        }
        self.inner
            .failure(neighbor, now, backoff)
            .map_err(Into::into)
    }
    /// Routing hint only; actual sends acquire an exclusive half-open probe.
    pub fn available(&self, neighbor: &NodeId) -> Result<bool> {
        self.available_at(neighbor, uring_runtime::environment::now())
    }
    pub fn available_at(&self, neighbor: &NodeId, now: Instant) -> Result<bool> {
        // A silent neighbor is routed around (Known issue 41).
        Ok(self.inner.available(neighbor, now) && !self.silent_at(neighbor, now))
    }
    /// Circuit state only: whether `neighbor` may carry a ping meant for
    /// itself even while this node holds it silent (Known issue 43).
    pub fn circuit_available_at(&self, neighbor: &NodeId, now: Instant) -> bool {
        self.inner.available(neighbor, now)
    }
    pub fn try_acquire(&self, neighbor: &NodeId) -> Result<bool> {
        self.try_acquire_at(neighbor, uring_runtime::environment::now())
    }
    pub fn try_acquire_at(&self, neighbor: &NodeId, now: Instant) -> Result<bool> {
        Ok(self.inner.try_acquire(neighbor, now))
    }
    pub fn retain_neighbors(&self, neighbors: &[NodeId]) {
        self.inner.retain(neighbors);
    }
    pub fn tracked_links(&self) -> usize {
        self.inner.len()
    }
}
/// Default ping time bound: the default peer attempt timeout.
pub const SILENCE_PING_TIMEOUT: Duration = Duration::from_secs(30);

fn backoff(node: &NodeId, failures: u32) -> Duration {
    let mut digest = hash_domain(b"racer/link-backoff/v1\0");
    hash_bytes(&mut digest, node.0.as_bytes());
    digest.update(failures.to_be_bytes());
    let digest = hash_finish(digest);
    racer_core::route::link_backoff(
        failures,
        u64::from(u16::from_be_bytes([digest[0], digest[1]])),
    )
}

pub(crate) fn hash_object(hash: &mut Sha256, object: &ObjectId, page: PageNumber) {
    hash_bytes(hash, object.cache.0.as_bytes());
    hash.update(object.key.0);
    hash.update(page.0.to_be_bytes());
}

// Stable sorted node identities and immutable leased membership versions.
//
// Readiness never changes ownership. Exclusions, weights, additions, and deletions
// do. Retain bounded old snapshots until their in-flight leases are released.
// Endpoint/NIC/Site changes also advance membership version, but placement
// depends only on node IDs and shares. All inputs are controller-accepted values.

pub const MAX_MEMBERS: usize = 100_000;

#[derive(Clone, Debug)]
pub struct Member {
    pub node: NodeId,
    pub shares: NonZeroU32,
    pub peer_endpoint: String,
    pub rails: Vec<RailMapping>,
    pub site: String,
}

#[cfg(test)]
thread_local! {
    // Accessors are read once at publication; ranking uses frozen crate weights.
    static WEIGHT_READS: std::cell::Cell<usize> = const { std::cell::Cell::new(0) };
}
#[cfg(test)]
pub(crate) fn scored_members() -> usize {
    WEIGHT_READS.get()
}

impl ::topology::Member for Member {
    const DOMAIN: &'static str = "racer";

    fn id(&self) -> &[u8] {
        self.node.0.as_bytes()
    }

    fn weight(&self) -> NonZeroU32 {
        #[cfg(test)]
        WEIGHT_READS.set(WEIGHT_READS.get() + 1);
        self.shares
    }
}
#[derive(Debug)]
pub struct Membership {
    pub version: MembershipVersion,
    pub(crate) inner: ::topology::Membership<Member>,
    retained_bytes: usize,
    rail_domain: Vec<racer_control_wire::RailId>,
}

impl From<racer_control_wire::Member> for Member {
    fn from(value: racer_control_wire::Member) -> Self {
        Self {
            node: value.node,
            shares: value.shares,
            peer_endpoint: value.peer_endpoint,
            rails: value.rails,
            site: value.site,
        }
    }
}
impl From<Member> for racer_control_wire::Member {
    fn from(value: Member) -> Self {
        Self {
            node: value.node,
            shares: value.shares,
            peer_endpoint: value.peer_endpoint,
            rails: value.rails,
            site: value.site,
        }
    }
}

impl Membership {
    pub fn validate(version: MembershipVersion, members: Vec<Member>) -> Result<Self> {
        Self::validate_with_predecessor(version, members, None)
    }
    /// Validate application metadata and reuse ID-stable predecessor adjacency.
    pub(crate) fn validate_with_predecessor(
        version: MembershipVersion,
        mut members: Vec<Member>,
        old: Option<&Self>,
    ) -> Result<Self> {
        if version.0 == 0 || members.len() > MAX_MEMBERS {
            return Err(Error::InvalidConfiguration);
        }
        for member in &mut members {
            if !valid_identity(&member.node.0) {
                return Err(Error::InvalidConfiguration);
            }
            racer_control_wire::validate_member_metadata(&member.peer_endpoint, &member.site)
                .and_then(|()| racer_control_wire::normalize_rails(&mut member.rails))
                .map_err(|_| Error::InvalidConfiguration)?;
        }
        // Compute once per publication, never from a request's selected route.
        // A partially equipped hop must fall back rather than rehash the page.
        let rail_domain: Vec<_> = members
            .iter()
            .flat_map(|m| m.rails.iter().map(|r| r.rail))
            .collect::<std::collections::BTreeSet<_>>()
            .into_iter()
            .collect();
        let retained_bytes = std::mem::size_of::<Self>()
            + rail_domain.capacity() * std::mem::size_of::<racer_control_wire::RailId>()
            + members
                .iter()
                .map(|m| {
                    m.node.0.capacity()
                        + m.peer_endpoint.capacity()
                        + m.site.capacity()
                        + m.rails.capacity() * std::mem::size_of::<RailMapping>()
                        + m.rails.iter().map(|r| r.device.capacity()).sum::<usize>()
                })
                .sum::<usize>();
        Ok(Self {
            version,
            inner: match old {
                Some(old) => ::topology::Membership::new_with_predecessor(members, &old.inner),
                None => ::topology::Membership::new(members),
            }
            .map_err(|_| Error::InvalidConfiguration)?,
            retained_bytes,
            rail_domain,
        })
    }
    /// Prepare bounded incremental ranking hints outside the publication lock.
    /// Larger changes use exact cooperative cold computation on demand.
    pub fn with_predecessor(mut self, old: &Membership) -> Self {
        self.inner = self.inner.with_predecessor(&old.inner);
        self
    }
    /// Local cache identity only. Routing still uses the authenticated version.
    #[cfg(test)]
    pub fn placement_identity(&self) -> [u8; 32] {
        self.inner.identity()
    }
    pub fn retained_bytes(&self) -> usize {
        self.retained_bytes + self.inner.retained_bytes()
    }
    pub fn members(&self) -> &[Member] {
        self.inner.members()
    }
    pub fn rail_domain(&self) -> &[racer_control_wire::RailId] {
        &self.rail_domain
    }
    pub fn position(&self, node: &NodeId) -> Result<usize> {
        self.inner
            .position(node.0.as_bytes())
            .ok_or(Error::IncompatibleMembership)
    }
    /// The local generation in which `node` arrived in this node's
    /// membership chain (`topology::Membership::since`); `None` for a
    /// non-member. Placement uses it for lineage handoff targets (Known
    /// issue 25).
    pub fn since(&self, node: &NodeId) -> Option<u32> {
        self.inner.since(self.inner.position(node.0.as_bytes())?)
    }
    pub fn member(&self, node: &NodeId) -> Result<&Member> {
        Ok(&self.members()[self.position(node)?])
    }
    /// Whether `a` and `b` share a direct overlay edge, without allocating.
    /// Unknown members and `a == b` are not neighbors.
    pub fn is_neighbor(&self, a: &NodeId, b: &NodeId) -> bool {
        match (
            self.inner.position(a.0.as_bytes()),
            self.inner.position(b.0.as_bytes()),
        ) {
            (Some(a), Some(b)) => self.inner.is_neighbor(a, b),
            _ => false,
        }
    }
    pub fn neighbors(&self, node: &NodeId) -> Result<Vec<NodeId>> {
        let position = self.position(node)?;
        Ok(self
            .inner
            .neighbors(position)
            .into_iter()
            .map(|index| self.members()[index].node.clone())
            .collect())
    }
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 256 && value.bytes().all(|byte| byte.is_ascii_graphic())
}

// Canonical slot encoding, placement, authenticated routing, and cancellation.
// Graph/search/weighted selection live in the runtime-independent topology crate.

/// Crate structural allocation estimate per cached ranking.
pub const RANKING_BYTES: usize = ::topology::Placement::ENTRY_BYTES;

pub struct Placement {
    inner: ::topology::Placement,
}
#[derive(Clone, Debug)]
pub struct Candidates {
    pub membership: std::sync::Arc<crate::topology::Membership>,
    pub ordered: Vec<NodeId>,
}

fn encoded_key(object: &ObjectId, page: PageNumber) -> Vec<u8> {
    let mut key = Vec::with_capacity(4 + object.cache.0.len() + 32 + 8);
    let length = u32::try_from(object.cache.0.len()).expect("validated cache ID fits u32");
    key.extend_from_slice(&length.to_be_bytes());
    key.extend_from_slice(object.cache.0.as_bytes());
    key.extend_from_slice(&object.key.0);
    key.extend_from_slice(&page.0.to_be_bytes());
    key
}

/// Fixed slot independent of object version and membership. Metadata passes page 0.
pub fn slot(object: &ObjectId, page: PageNumber) -> u32 {
    ::topology::slot::<Member>(&encoded_key(object, page))
}

fn candidates(
    membership: std::sync::Arc<crate::topology::Membership>,
    ranked: Vec<usize>,
) -> Candidates {
    let ordered = ranked
        .into_iter()
        .map(|i| membership.members()[i].node.clone())
        .collect();
    Candidates {
        membership,
        ordered,
    }
}

fn placement_error(error: ::topology::Error) -> Error {
    match error {
        ::topology::Error::Overloaded => Error::Overloaded,
        ::topology::Error::SamplingExhausted => Error::Internal,
        _ => Error::InvalidConfiguration,
    }
}

impl Placement {
    pub fn new(capacity: usize) -> Self {
        Self {
            inner: ::topology::Placement::new(capacity),
        }
    }

    pub fn with_memory_budget(bytes: usize) -> Self {
        Self::new(bytes / RANKING_BYTES)
    }

    pub fn rank(
        &self,
        membership: std::sync::Arc<crate::topology::Membership>,
        object: &ObjectId,
        page: PageNumber,
    ) -> Candidates {
        let ranked = self
            .inner
            .rank(&membership.inner, &encoded_key(object, page));
        candidates(membership, ranked)
    }

    /// Cache-only retention hint. Never starts or advances a cold ranking.
    pub fn cached(
        &self,
        membership: Arc<Membership>,
        object: &ObjectId,
        page: PageNumber,
    ) -> Option<Candidates> {
        self.inner
            .cached(&membership.inner, &encoded_key(object, page))
            .map(|ranked| candidates(membership, ranked))
    }

    /// Cache-only: members ranked just below the page's owners, best first,
    /// from a complete cached ranking (Known issue 34). Never ranks.
    pub fn demoted(
        &self,
        membership: &Membership,
        object: &ObjectId,
        page: PageNumber,
    ) -> Vec<NodeId> {
        self.inner
            .demoted(&membership.inner, &encoded_key(object, page))
            .unwrap_or_default()
            .into_iter()
            .map(|index| membership.members()[index].node.clone())
            .collect()
    }

    pub fn is_owner(
        &self,
        membership: &Membership,
        object: &ObjectId,
        page: PageNumber,
        node: &NodeId,
    ) -> bool {
        self.inner
            .is_owner(&membership.inner, &encoded_key(object, page), |index| {
                &membership.members()[index].node == node
            })
    }

    /// One bounded resident refresh turn: at most 256 scores and 64 CLOCK visits.
    pub fn refresh(
        &self,
        membership: Arc<Membership>,
        object: &ObjectId,
        page: PageNumber,
    ) -> Option<Candidates> {
        self.inner
            .refresh(&membership.inner, &encoded_key(object, page))
            .map(|ranked| candidates(membership, ranked))
    }

    /// Reactor-friendly cold ranking. Concurrent requests for a resident slot
    /// share progress; each poll hashes at most 256 members, with no held borrow
    /// across the yield. Dropped operations release their active-cache admission.
    #[cfg(test)]
    pub fn rank_async<'a>(
        &'a self,
        membership: std::sync::Arc<crate::topology::Membership>,
        object: &ObjectId,
        page: PageNumber,
    ) -> Operation<'a, Candidates> {
        self.rank_scoped(membership, object, page, None)
    }

    pub fn rank_scoped<'a>(
        &'a self,
        membership: std::sync::Arc<crate::topology::Membership>,
        object: &ObjectId,
        page: PageNumber,
        scope: Option<&'a crate::runtime::RequestScope>,
    ) -> Operation<'a, Candidates> {
        let key = encoded_key(object, page);
        Box::pin(async move {
            if let Some(scope) = scope {
                scope.check()?;
            }
            let cancellation = scope
                .map(|scope| scope.cancellation.subscribe())
                .transpose()?;
            let mut ranking = std::pin::pin!(self.inner.rank_async(&membership.inner, &key));
            let ranked = std::future::poll_fn(|cx| {
                if let Some(scope) = scope {
                    if let Some(cancellation) = &cancellation {
                        cancellation.register(cx.waker());
                    }
                    scope.check()?;
                }
                let result = ranking
                    .as_mut()
                    .poll(cx)
                    .map(|result| result.map_err(placement_error));
                if let Some(scope) = scope {
                    scope.check()?;
                }
                result
            })
            .await?;
            Ok(candidates(membership.clone(), ranked))
        })
    }

    /// Warm only already-demanded predecessor slots. Each turn hashes at most
    /// one cold quantum or visits one retained key, with no full-cache sweep.
    pub fn maintain(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
    ) -> Maintenance {
        self.inner.maintain(&membership.inner)
    }
}

#[derive(Clone, Debug)]
pub struct Route {
    pub membership: std::sync::Arc<crate::topology::Membership>,
    pub nodes: Vec<NodeId>,
}
/// Signed forwarding state. Retries preserve deadline and consumed link budget.
#[derive(Clone, Debug)]
pub struct RouteBudget {
    pub membership: racer_control_wire::MembershipVersion,
    pub request: RequestId,
    pub attempt: AttemptId,
    pub destination: NodeId,
    pub visited: Vec<NodeId>,
    pub remaining_links: u8,
    /// Acquisition credits transferred from the caller, never replenished by a hop.
    pub remaining_attempts: u32,
    pub deadline: Deadline,
}
impl From<racer_core::route::RouteFault> for Error {
    fn from(fault: racer_core::route::RouteFault) -> Self {
        use racer_core::route::RouteFault;
        match fault {
            RouteFault::DeadlineExceeded => Self::DeadlineExceeded,
            RouteFault::HopBudgetExhausted => Self::HopBudgetExhausted,
            RouteFault::Invalid => Self::InvalidRequest,
        }
    }
}
impl Error {
    /// The failure class the sans-IO peer decisions (`racer_core::route`) read.
    pub fn fault(&self) -> racer_core::route::Fault {
        use racer_core::route::Fault;
        match self {
            Self::Overloaded => Fault::Overloaded,
            Self::Unavailable => Fault::Unavailable,
            Self::Io => Fault::Io,
            Self::Os(_) => Fault::Os,
            Self::DeadlineExceeded => Fault::DeadlineExceeded,
            Self::BadGateway => Fault::BadGateway,
            Self::CorruptRecord => Fault::CorruptRecord,
            Self::Cancelled => Fault::Cancelled,
            Self::HopBudgetExhausted => Fault::HopBudgetExhausted,
            Self::IncompatibleMembership => Fault::IncompatibleMembership,
            Self::NotFound => Fault::NotFound,
            Self::VersionUnavailable => Fault::VersionUnavailable,
            Self::Unauthorized => Fault::Unauthorized,
            Self::OriginForbidden => Fault::OriginForbidden,
            Self::MissingKey => Fault::MissingKey,
            _ => Fault::Other,
        }
    }
}
pub const NORMAL_LINKS: u8 = 4;
pub const FAILURE_LINKS: u8 = 8;
impl RouteBudget {
    fn validate_at(&self, from: &NodeId, now: Instant) -> Result<()> {
        racer_core::route::validate(
            &self.visited,
            from,
            &self.destination,
            self.remaining_links,
            self.deadline.0,
            now,
        )
        .map_err(Into::into)
    }
}

pub struct Paths {
    pub(crate) peer_admission: Option<Arc<crate::peer::AdaptivePeers>>,
    health: Rc<LinkHealth>,
    inner: ::topology::Paths,
}
impl Paths {
    pub(crate) fn with_peer_admission(
        mut self,
        admission: Arc<crate::peer::AdaptivePeers>,
    ) -> Self {
        self.peer_admission = Some(admission);
        self
    }
    pub fn link_health(&self) -> Rc<LinkHealth> {
        self.health.clone()
    }
    pub fn new(health: Rc<LinkHealth>, capacity: usize) -> Self {
        Self {
            peer_admission: None,
            health,
            inner: ::topology::Paths::new(capacity),
        }
    }
    pub fn with_limits(
        health: Rc<LinkHealth>,
        cache_entries: usize,
        cache_bytes: usize,
        active_searches: usize,
    ) -> Self {
        Self {
            peer_admission: None,
            health,
            inner: ::topology::Paths::with_limits(cache_entries, cache_bytes, active_searches),
        }
    }
    #[cfg(test)]
    pub fn shortest(
        &self,
        membership: std::sync::Arc<crate::topology::Membership>,
        from: &NodeId,
        budget: &RouteBudget,
    ) -> Result<Route> {
        let scope = RequestScope::new(budget.request, budget.deadline.0)?;
        futures::executor::block_on(self.shortest_async(membership, from, budget, &scope))
    }

    /// Runtime checks surround every cooperative algorithm poll, including cache hits.
    pub fn shortest_async<'a>(
        &'a self,
        membership: std::sync::Arc<crate::topology::Membership>,
        from: &'a NodeId,
        budget: &'a RouteBudget,
        scope: &'a RequestScope,
    ) -> Operation<'a, Route> {
        self.shortest_probe_async(membership, from, budget, scope, false)
    }

    /// Like [`Self::shortest_async`]. With `ping`, the budget's destination
    /// is not routed around for silence: an origin's recovery ping must be
    /// able to reach the peer it holds silent (Known issue 43). Circuit
    /// health and peer admission still apply, and silent intermediate
    /// neighbors are still routed around.
    pub fn shortest_probe_async<'a>(
        &'a self,
        membership: std::sync::Arc<crate::topology::Membership>,
        from: &'a NodeId,
        budget: &'a RouteBudget,
        scope: &'a RequestScope,
        ping: bool,
    ) -> Operation<'a, Route> {
        Box::pin(async move {
            scope.check()?;
            budget.validate_at(from, uring_runtime::environment::now())?;
            if membership.version != budget.membership {
                return Err(Error::IncompatibleMembership);
            }
            let source = membership.position(from)?;
            let to = membership.position(&budget.destination)?;
            if source != to && budget.remaining_links == 0 {
                return Err(Error::HopBudgetExhausted);
            }
            let visited = budget
                .visited
                .iter()
                .map(|node| membership.position(node))
                .collect::<Result<Vec<_>>>()?;
            let exempt = ping.then_some(&budget.destination);
            let blocked = self.blocked(&membership, source, exempt)?;
            let mut seed = [0; 32];
            seed[..16].copy_from_slice(&budget.request.0);
            seed[16..].copy_from_slice(&budget.attempt.0);
            let nodes = {
                let query = ::topology::PathQuery {
                    from: source,
                    to,
                    links: budget.remaining_links,
                    visited: &visited,
                    blocked: &blocked,
                    seed: &seed,
                };
                let mut operation = std::pin::pin!(self.inner.route(&membership.inner, query));
                std::future::poll_fn(|cx| {
                    if let Err(error) = scope
                        .check()
                        .and_then(|()| budget.validate_at(from, uring_runtime::environment::now()))
                    {
                        return Poll::Ready(Err(error));
                    }
                    operation
                        .as_mut()
                        .poll(cx)
                        .map(|result| result.map_err(map_error))
                })
                .await
            };
            scope.check()?;
            budget.validate_at(from, uring_runtime::environment::now())?;
            // Health/admission may change while yielded. Retry under the same budget.
            if self.blocked(&membership, source, exempt)? != blocked {
                return Err(Error::Unavailable);
            }
            let nodes = nodes?
                .into_iter()
                .map(|index| membership.members()[index].node.clone())
                .collect();
            Ok(Route { membership, nodes })
        })
    }

    fn blocked(
        &self,
        membership: &Membership,
        source: usize,
        exempt: Option<&NodeId>,
    ) -> Result<Vec<usize>> {
        let now = uring_runtime::environment::now();
        let positions = membership.inner.neighbors(source);
        let neighbors: Vec<_> = positions
            .iter()
            .map(|&i| membership.members()[i].node.clone())
            .collect();
        self.health.retain_neighbors(&neighbors);
        let mut blocked = Vec::new();
        for (position, node) in positions.into_iter().zip(&neighbors) {
            let available = if exempt == Some(node) {
                self.health.circuit_available_at(node, now)
            } else {
                self.health.available_at(node, now)?
            };
            if !available
                || self
                    .peer_admission
                    .as_ref()
                    .is_some_and(|admission| !admission.available(node))
            {
                blocked.push(position);
            }
        }
        Ok(blocked)
    }
}
fn map_error(error: ::topology::Error) -> Error {
    match error {
        ::topology::Error::InvalidQuery => Error::InvalidRequest,
        ::topology::Error::Overloaded => Error::Overloaded,
        ::topology::Error::Unreachable => Error::Unavailable,
        _ => Error::Internal,
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    /// Known issues 41 and 43: a destination whose ping went unanswered is
    /// silent: unavailable for routing until it answers anything.
    #[test]
    fn a_silent_peer_is_routed_around_until_it_answers() {
        let clock = uring_runtime::environment::SimulationClock::new(1);
        let _env = clock.environment(0).enter();
        let health =
            LinkHealth::default().with_silence(Duration::from_secs(10), Duration::from_secs(2));
        let peer = NodeId("peer".into());
        let page = crate::model::PageId {
            version: crate::model::ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: crate::model::CacheKey([7; 32]),
                },
                etag: crate::model::StrongEtag::test_value("v"),
            },
            number: PageNumber(3),
        };
        let sent = uring_runtime::environment::now();
        clock.advance(Duration::from_secs(2));
        // One unanswered exchange over the last link asks nothing yet.
        assert!(!health.silence_timed_out(
            &peer,
            page.clone(),
            sent,
            racer_core::liveness::LINK_TIMEOUTS
        ));
        assert!(health.silence_timed_out(
            &peer,
            page.clone(),
            sent,
            racer_core::liveness::LINK_TIMEOUTS
        ));
        let now = uring_runtime::environment::now();
        assert_eq!(health.due_pings(now, 8), vec![(peer.clone(), page.clone())]);
        assert!(health.available(&peer).unwrap());
        // A relay's overload proves nothing; an unanswered ping does.
        assert!(!health.ping_outcome(&peer, None, now));
        assert!(!health.silent(&peer));
        assert!(health.ping_outcome(&peer, Some(false), now));
        assert!(health.silent(&peer));
        assert!(!health.available(&peer).unwrap());
        // Pinged again after the hold; an answer clears it.
        assert!(health.due_pings(now, 8).is_empty());
        clock.advance(Duration::from_secs(20));
        let later = uring_runtime::environment::now();
        assert_eq!(health.due_pings(later, 8).len(), 1);
        assert!(!health.ping_outcome(&peer, Some(true), later));
        assert!(!health.silent(&peer));
        assert!(health.available(&peer).unwrap());
    }

    pub(crate) mod fixtures {
        use super::*;
        use crate::model::*;
        use crate::topology::Member;
        use crate::topology::Membership;
        use std::num::NonZeroU32;
        use std::sync::Arc;

        // Fixed next-hop vectors for the 32-ring overlay.
        // N=1500, source=0, destination=1499, request=[1;16]; weights 1 at
        // multiples of 3 and 4 elsewhere; attempts encoded as big-endian u128.
        pub(crate) const NEXT_HOPS: [(u128, usize); 4] =
            [(0, 1301), (1, 1301), (2, 251), (127, 251)];

        pub(crate) fn member(index: usize, shares: u32) -> Member {
            Member {
                node: NodeId(format!("node-{index:06}")),
                shares: NonZeroU32::new(shares).unwrap(),
                peer_endpoint: "127.0.0.1:8080".into(),
                rails: vec![],
                site: String::new(),
            }
        }
        pub(crate) fn membership(count: usize) -> std::sync::Arc<crate::topology::Membership> {
            Arc::new(
                Membership::validate(
                    MembershipVersion(1),
                    (0..count).map(|i| member(i, 4)).collect(),
                )
                .unwrap(),
            )
        }
        pub(crate) fn object() -> ObjectId {
            ObjectId {
                cache: CacheId("cache-a".into()),
                key: CacheKey([0x42; 32]),
            }
        }
    }

    pub(crate) mod circuits_tests {
        use super::*;
        use crate::model::PageNumber;
        use crate::topology::Placement;
        use crate::topology::tests::fixtures::membership;
        use crate::topology::tests::fixtures::object;
        #[test]
        fn owned_half_open_probe_remains_exclusive_until_completion_or_drop() {
            let health = LinkHealth::new(1);
            let node = NodeId("peer".into());
            health
                .observe_at(
                    &node,
                    LinkOutcome::Timeout,
                    uring_runtime::environment::now() - Duration::from_secs(60),
                )
                .unwrap();
            let probe = health.acquire(&node).unwrap();
            assert!(
                !health
                    .try_acquire_at(
                        &node,
                        uring_runtime::environment::now() + Duration::from_secs(60)
                    )
                    .unwrap()
            );
            drop(probe);
            assert!(
                health
                    .try_acquire_at(
                        &node,
                        uring_runtime::environment::now() + Duration::from_secs(60)
                    )
                    .unwrap()
            );
        }
        #[test]
        fn endpoint_application_errors_do_not_open_circuits_and_probes_are_bounded() {
            let health = LinkHealth::new(2);
            let endpoint = NodeId("origin".into());
            for error in [
                Error::Unauthorized,
                Error::NotFound,
                Error::VersionUnavailable,
            ] {
                assert_eq!(
                    futures::executor::block_on(
                        health.run(&endpoint, async { Err::<(), _>(error) })
                    ),
                    Err(error)
                );
                assert_eq!(health.tracked_links(), 0);
            }
            assert_eq!(
                futures::executor::block_on(
                    health.run(&endpoint, async { Err::<(), _>(Error::Io) })
                ),
                Err(Error::Io)
            );
            assert_eq!(
                futures::executor::block_on(health.run(&endpoint, async {
                    panic!("open circuit attempted I/O");
                    #[allow(unreachable_code)]
                    Ok(())
                })),
                Err(Error::Unavailable)
            );
        }
        #[test]
        fn circuit_backoff_probes_success_and_isolation() {
            let first = LinkHealth::new(2);
            let second = LinkHealth::new(2);
            let node = NodeId("a".into());
            let now = Instant::now();
            assert!(first.try_acquire_at(&node, now).unwrap());
            first.observe_at(&node, LinkOutcome::Timeout, now).unwrap();
            assert!(!first.available_at(&node, now).unwrap());
            assert!(second.available_at(&node, now).unwrap());
            let retry = now + backoff(&node, 1);
            assert!(
                !first
                    .available_at(&node, retry - Duration::from_nanos(1))
                    .unwrap()
            );
            assert!(first.available_at(&node, retry).unwrap());
            assert!(first.try_acquire_at(&node, retry).unwrap());
            assert!(!first.try_acquire_at(&node, retry).unwrap());
            assert!(
                first
                    .try_acquire_at(&node, retry + Duration::from_secs(1))
                    .unwrap()
            );
            first
                .observe_at(&node, LinkOutcome::Refused, retry)
                .unwrap();
            assert!(backoff(&node, 2) > backoff(&node, 1));
            first
                .observe_at(&node, LinkOutcome::Success, retry)
                .unwrap();
            assert!(first.available_at(&node, retry).unwrap());
            assert_eq!(first.tracked_links(), 0);
            for failures in [1, 2, 10, u32::MAX] {
                assert!(backoff(&node, failures) <= Duration::from_secs(25));
            }
            assert_ne!(backoff(&node, 3), backoff(&NodeId("b".into()), 3));
        }
        #[test]
        fn capacity_and_placement_are_independent() {
            let health = LinkHealth::new(1);
            let members = membership(3);
            let placement = Placement::new(1);
            let before = placement
                .rank(members.clone(), &object(), PageNumber(0))
                .ordered;
            health
                .observe(&before[0], LinkOutcome::ProtocolFailure)
                .unwrap();
            assert_eq!(
                health.observe(&before[1], LinkOutcome::Timeout),
                Err(Error::Overloaded)
            );
            assert_eq!(
                before,
                placement.rank(members, &object(), PageNumber(0)).ordered
            );
            health.retain_neighbors(&[]);
            assert_eq!(health.tracked_links(), 0);
        }
        #[test]
        fn default_tracks_all_64_neighbors_without_eviction() {
            let health = LinkHealth::default();
            let independent = LinkHealth::default();
            let now = Instant::now();
            for i in 0..64 {
                health
                    .observe_at(&NodeId(format!("peer-{i}")), LinkOutcome::Timeout, now)
                    .unwrap();
            }
            assert_eq!(health.tracked_links(), 64);
            assert_eq!(independent.tracked_links(), 0);
            assert!(
                independent
                    .available_at(&NodeId("peer-0".into()), now)
                    .unwrap()
            );
            for i in 0..64 {
                assert!(
                    !health
                        .available_at(&NodeId(format!("peer-{i}")), now)
                        .unwrap()
                );
            }
            assert_eq!(
                health.observe_at(&NodeId("overflow".into()), LinkOutcome::Timeout, now),
                Err(Error::Overloaded)
            );
        }
    }

    pub(crate) mod hash_tests {
        use super::*;
        use crate::topology::tests::fixtures;
        #[test]
        fn full_domain_separated_digest_vectors() {
            for (name, kind, expected) in [
                (
                    b"racer/slot/v1\0".as_slice(),
                    0,
                    "d8b632a58acf4dc92ccc3abe711290968975c95221982118f58393fbdead4781",
                ),
                (
                    b"racer/hrw/v1\0",
                    1,
                    "e41095812e885f6f0ae7e3c1a93d8ec04999df01dbb5820765c7e972b2c07a9c",
                ),
                (
                    b"racer/rail/v1\0",
                    0,
                    "5f57aa16d39d63c738294a99c9f7311ad8169aeee1c75adeafe75c09e6eee2e5",
                ),
            ] {
                let mut hash = hash_domain(name);
                if kind == 1 {
                    hash.update(887651u32.to_be_bytes());
                    hash_bytes(&mut hash, b"node-000000");
                } else {
                    hash_object(&mut hash, &fixtures::object(), PageNumber(0));
                }
                let actual: String = hash_finish(hash)
                    .iter()
                    .map(|b| format!("{b:02x}"))
                    .collect();
                assert_eq!(actual, expected);
            }
        }
    }

    pub(crate) mod membership_tests {
        use super::*;
        use crate::topology::tests::fixtures::member;
        use racer_control_wire::RailId;
        use racer_control_wire::RailMapping;

        #[test]
        fn wire_member_adapter_preserves_publication_fields_at_validation() {
            let publication = racer_control_wire::decode_publication(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/control-wire/testdata/publication.json"
            )))
            .unwrap();
            let membership = Membership::validate(
                publication.membership_version,
                publication
                    .members
                    .iter()
                    .cloned()
                    .map(Member::from)
                    .collect(),
            )
            .unwrap();
            for wire in &publication.members {
                let local = membership.member(&wire.node).unwrap();
                assert_eq!(local.node, wire.node);
                assert_eq!(local.shares, wire.shares);
                assert_eq!(local.peer_endpoint, wire.peer_endpoint);
                assert_eq!(local.site, wire.site);
                let mut expected = wire.rails.clone();
                expected.sort_unstable_by(|a, b| {
                    (a.rail, &a.device, a.port).cmp(&(b.rail, &b.device, b.port))
                });
                assert_eq!(local.rails, expected);
            }
            let roundtrip = racer_control_wire::Publication {
                members: membership
                    .members()
                    .iter()
                    .cloned()
                    .map(Into::into)
                    .collect(),
                ..publication.clone()
            };
            assert_eq!(
                racer_control_wire::canonical_content(&roundtrip).unwrap(),
                racer_control_wire::canonical_content(&publication).unwrap()
            );
        }

        #[test]
        fn site_label_grammar_and_retained_bytes() {
            for site in ["", "A", "Site_1.west-2", &"a".repeat(63)] {
                let mut node = member(0, 1);
                node.site = site.into();
                assert!(Membership::validate(MembershipVersion(1), vec![node]).is_ok());
            }
            for site in ["-a", "a-", ".a", "a_", "a/b", "a b", "é", &"a".repeat(64)] {
                let mut node = member(0, 1);
                node.site = site.into();
                assert!(Membership::validate(MembershipVersion(1), vec![node]).is_err());
            }
            let original = Membership::validate(MembershipVersion(1), vec![member(0, 1)]).unwrap();
            let mut node = member(0, 1);
            node.site = "west".into();
            let changed = Membership::validate(MembershipVersion(2), vec![node]).unwrap();
            assert_eq!(changed.placement_identity(), original.placement_identity());
            assert_eq!(changed.retained_bytes(), original.retained_bytes() + 4);
        }

        #[test]
        fn validates_and_freezes_sorted_members() {
            let input = vec![member(2, 1), member(0, u32::MAX), member(1, 4)];
            let membership = Membership::validate(MembershipVersion(1), input).unwrap();
            assert_eq!(
                membership
                    .members()
                    .iter()
                    .map(|m| m.node.clone())
                    .collect::<Vec<_>>(),
                (0..3).map(|i| member(i, 1).node).collect::<Vec<_>>()
            );
            assert_eq!(
                membership.member(&member(0, 1).node).unwrap().shares.get(),
                u32::MAX
            );
            assert_eq!(
                membership.position(&NodeId("absent".into())),
                Err(Error::IncompatibleMembership)
            );
            assert!(Membership::validate(MembershipVersion(1), vec![]).is_ok());
        }

        #[test]
        fn retained_bytes_add_only_nested_application_allocations() {
            let mut node = member(0, 4);
            node.site = "west".into();
            node.rails.push(RailMapping {
                rail: RailId(1),
                device: "fabric".into(),
                port: 1,
                gid: None,
                numa_node: None,
            });
            let old = Membership::validate(MembershipVersion(1), vec![]).unwrap();
            let membership = Membership::validate(MembershipVersion(2), vec![node])
                .unwrap()
                .with_predecessor(&old);
            let node = &membership.members()[0];
            let nested = node.node.0.capacity()
                + node.site.capacity()
                + node.peer_endpoint.capacity()
                + node.rails.capacity() * std::mem::size_of::<RailMapping>()
                + node.rails[0].device.capacity();
            assert_eq!(
                membership.retained_bytes(),
                std::mem::size_of::<Membership>()
                    + membership.inner.retained_bytes()
                    + membership.rail_domain.capacity() * std::mem::size_of::<RailId>()
                    + nested
            );
        }

        #[test]
        fn rejects_bad_members_and_duplicate_rails() {
            assert!(Membership::validate(MembershipVersion(0), vec![]).is_err());
            assert!(Membership::validate(MembershipVersion(1), vec![member(0, 1); 2]).is_err());
            for endpoint in [
                "host:80",
                "127.0.0.1:0",
                "[::]:0",
                "[fe80::1%3]:80",
                "[fe80::1%eth0]:80",
                "garbage",
            ] {
                let mut node = member(0, 1);
                node.peer_endpoint = endpoint.into();
                assert!(Membership::validate(MembershipVersion(1), vec![node]).is_err());
            }
            let mut node = member(0, 1);
            node.rails = vec![
                RailMapping {
                    rail: RailId(0),
                    device: "a".into(),
                    port: 1,
                    gid: None,
                    numa_node: None
                };
                2
            ];
            assert!(Membership::validate(MembershipVersion(1), vec![node]).is_err());
            let mut node = member(0, 1);
            node.node.0.clear();
            assert!(Membership::validate(MembershipVersion(1), vec![node]).is_err());
        }

        #[test]
        fn fabric_matches_go_wire_utf8_and_control_contract() {
            // Includes the Go shared publication vector, whitespace/other controls
            // allowed by validRail, and names beyond the former local 256-byte cap.
            for fabric in [
                "β<&>\u{2028}".to_owned(),
                "网络 fabric 🚆".to_owned(),
                " \t\u{0001}\u{007f}".to_owned(),
                "é".repeat(257),
            ] {
                let mut node = member(0, 4);
                node.rails = vec![RailMapping {
                    rail: RailId(u16::MAX),
                    device: fabric.clone(),
                    port: 1,
                    gid: None,
                    numa_node: Some(u32::MAX as usize),
                }];
                let accepted = Membership::validate(MembershipVersion(1), vec![node]).unwrap();
                assert_eq!(
                    accepted.members()[0].rails[0].device.as_bytes(),
                    fabric.as_bytes()
                );
                assert_eq!(
                    accepted.members()[0].rails[0].numa_node,
                    Some(u32::MAX as usize)
                );
            }
            for fabric in ["", "\0", "\r", "\n", "β\0fabric", "β\rfabric", "β\nfabric"] {
                let mut node = member(0, 4);
                node.rails = vec![RailMapping {
                    rail: RailId(0),
                    device: fabric.into(),
                    port: 1,
                    gid: None,
                    numa_node: None,
                }];
                assert_eq!(
                    Membership::validate(MembershipVersion(1), vec![node]).unwrap_err(),
                    Error::InvalidConfiguration
                );
            }
        }

        #[test]
        fn endpoint_validation_matches_go_wire_without_reachability_policy() {
            for endpoint in [
                "0.0.0.0:80",
                "[::]:80",
                "224.0.0.1:80",
                "[ff02::1]:80",
                "[2001:db8::1]:7443",
                "192.0.2.1:65535",
            ] {
                let mut node = member(0, 4);
                node.peer_endpoint = endpoint.into();
                let accepted = Membership::validate(MembershipVersion(1), vec![node]).unwrap();
                assert_eq!(accepted.members()[0].peer_endpoint, endpoint);
            }
        }

        #[test]
        fn fabric_ascii_controls_match_wire_exactly() {
            for byte in 0u8..=127 {
                let mut node = member(0, 4);
                node.rails = vec![RailMapping {
                    rail: RailId(0),
                    device: format!("β{}fabric", char::from(byte)),
                    port: 1,
                    gid: None,
                    numa_node: None,
                }];
                assert_eq!(
                    Membership::validate(MembershipVersion(1), vec![node]).is_ok(),
                    !matches!(byte, 0 | b'\r' | b'\n'),
                    "ASCII byte {byte}",
                );
            }
        }

        #[test]
        fn numa_id_cannot_exceed_wire_u32() {
            if let Some(oversized) = (u32::MAX as usize).checked_add(1) {
                let mut node = member(0, 4);
                node.rails = vec![RailMapping {
                    rail: RailId(0),
                    device: "fabric".into(),
                    port: 1,
                    gid: None,
                    numa_node: Some(oversized),
                }];
                assert_eq!(
                    Membership::validate(MembershipVersion(1), vec![node]).unwrap_err(),
                    Error::InvalidConfiguration
                );
            }
        }
    }

    pub(crate) mod routing_tests {
        use super::*;
        use crate::topology::Membership;
        use crate::topology::scored_members;
        use crate::topology::tests::fixtures::member;
        use crate::topology::tests::fixtures::membership;
        use crate::topology::tests::fixtures::object;
        use racer_control_wire::MembershipVersion;
        use std::sync::Arc;

        #[test]
        fn golden_slot_and_weighted_ranking_vectors() {
            let members = Arc::new(
                Membership::validate(
                    MembershipVersion(1),
                    [1, 3, 6, 4]
                        .into_iter()
                        .enumerate()
                        .map(|(i, weight)| member(i, weight))
                        .collect(),
                )
                .unwrap(),
            );
            let placement = Placement::new(3);
            for (page, expected_slot, expected_order) in [
                (0, 887_651, [1, 3, 2]),
                (1, 348_931, [3, 2, 1]),
                (u64::MAX, 665_200, [2, 1, 3]),
            ] {
                assert_eq!(slot(&object(), PageNumber(page)), expected_slot);
                let ranking = placement.rank(members.clone(), &object(), PageNumber(page));
                assert_eq!(ranking.ordered, expected_order.map(|i| member(i, 1).node));
            }
        }

        #[test]
        fn addition_removal_preserve_survivor_order_and_old_snapshot_rankings() {
            let old = membership(12);
            let mut added = old.members().to_vec();
            added.push(member(12, 4));
            let added = Arc::new(Membership::validate(MembershipVersion(2), added).unwrap());
            let removed = Arc::new(
                Membership::validate(MembershipVersion(3), old.members()[1..].to_vec()).unwrap(),
            );
            let placement = Placement::new(6);
            for page in 0..100 {
                let original = placement
                    .rank(old.clone(), &object(), PageNumber(page))
                    .ordered;
                let extended = placement
                    .rank(added.clone(), &object(), PageNumber(page))
                    .ordered;
                let survivors: Vec<_> = extended
                    .iter()
                    .filter(|node| **node != member(12, 1).node)
                    .cloned()
                    .collect();
                assert_eq!(&original[..survivors.len()], survivors);
                let reduced = placement
                    .rank(removed.clone(), &object(), PageNumber(page))
                    .ordered;
                let survivors: Vec<_> = original
                    .iter()
                    .filter(|node| **node != member(0, 1).node)
                    .cloned()
                    .collect();
                assert_eq!(&reduced[..survivors.len()], survivors);
                assert_eq!(
                    placement
                        .rank(old.clone(), &object(), PageNumber(page))
                        .ordered,
                    original
                );
            }
        }

        #[test]
        fn small_clusters_order_invariance_and_nonplacement_changes() {
            let placement = Placement::new(10);
            for count in 0..=4 {
                assert_eq!(
                    placement
                        .rank(membership(count), &object(), PageNumber(0))
                        .ordered
                        .len(),
                    count.min(3)
                );
            }
            let original = membership(30);
            let mut changed = original.members().to_vec();
            changed.reverse();
            for node in &mut changed {
                node.peer_endpoint = "[::1]:9090".into();
                node.rails.clear();
            }
            let changed = Arc::new(Membership::validate(MembershipVersion(2), changed).unwrap());
            for page in 0..100 {
                assert_eq!(
                    placement
                        .rank(original.clone(), &object(), PageNumber(page))
                        .ordered,
                    placement
                        .rank(changed.clone(), &object(), PageNumber(page))
                        .ordered
                );
            }
        }

        #[test]
        fn incremental_demand_maintenance_matches_cold_oracle_under_churn() {
            let placement = Placement::with_memory_budget(512 * RANKING_BYTES);
            let oracle = Placement::new(0);
            let mut old = membership(80);
            for generation in 2..42 {
                for page in 0..40 {
                    placement.rank(old.clone(), &object(), PageNumber(page));
                }
                let mut members = old.members().to_vec();
                members.remove(generation as usize % members.len());
                members.push(member(100 + generation as usize, 4));
                members[generation as usize % 10].shares =
                    std::num::NonZeroU32::new(generation % 7 + 1).unwrap();
                let next = Arc::new(
                    Membership::validate(MembershipVersion(generation as u64), members)
                        .unwrap()
                        .with_predecessor(&old),
                );
                for page in 0..40 {
                    assert_eq!(
                        placement
                            .rank(next.clone(), &object(), PageNumber(page))
                            .ordered,
                        oracle
                            .rank(next.clone(), &object(), PageNumber(page))
                            .ordered,
                        "generation {generation} page {page}"
                    );
                }
                old = next;
            }
        }

        #[test]
        fn endpoint_epoch_reuses_completed_rank_and_returns_current_routing() {
            let placement = Placement::with_memory_budget(RANKING_BYTES);
            let old = membership(100_000);
            let expected = placement.rank(old.clone(), &object(), PageNumber(0));
            let mut nodes = old.members().to_vec();
            for node in &mut nodes {
                node.peer_endpoint = "[::1]:9090".into();
                node.rails.clear();
            }
            let current = Arc::new(Membership::validate(MembershipVersion(2), nodes).unwrap());
            assert_eq!(old.placement_identity(), current.placement_identity());
            let mut future = placement.rank_async(current.clone(), &object(), PageNumber(0));
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            let before = scored_members();
            let Poll::Ready(Ok(actual)) = future.as_mut().poll(&mut cx) else {
                panic!("endpoint-only change must not perform another cold ranking");
            };
            assert_eq!(actual.ordered, expected.ordered);
            assert!(Arc::ptr_eq(&actual.membership, &current));
            assert_eq!(scored_members(), before);
            drop(old);
            drop(expected);
            placement.rank(current, &object(), PageNumber(0));
            assert_eq!(scored_members(), before);
        }

        #[test]
        fn scoped_cold_rank_cancels_between_bounded_quanta() {
            use crate::model::RequestId;
            use crate::runtime::RequestScope;
            let placement = Placement::with_memory_budget(4 * RANKING_BYTES);
            let members = membership(100_000);
            let scope = RequestScope::new(
                RequestId([1; 16]),
                uring_runtime::environment::now() + std::time::Duration::from_secs(30),
            )
            .unwrap();
            let mut future = placement.rank_scoped(members, &object(), PageNumber(0), Some(&scope));
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            let before = scored_members();
            assert!(future.as_mut().poll(&mut cx).is_pending());
            scope.cancel().unwrap();
            assert!(matches!(
                future.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            assert_eq!(scored_members(), before, "ranking uses frozen weights");
        }

        #[test]
        fn cooperative_coalescing_bounded_cache_and_cancellation() {
            use std::task::Context;
            let placement = Placement::new(1);
            let members = membership(100_000);
            let mut first = placement.rank_async(members.clone(), &object(), PageNumber(0));
            let mut second = placement.rank_async(members.clone(), &object(), PageNumber(0));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let before = scored_members();
            assert!(first.as_mut().poll(&mut cx).is_pending());
            assert!(second.as_mut().poll(&mut cx).is_pending());
            assert_eq!(scored_members(), before, "ranking uses frozen weights");
            assert_eq!(
                futures::executor::block_on(placement.rank_async(
                    members.clone(),
                    &object(),
                    PageNumber(1)
                ))
                .unwrap_err(),
                Error::Overloaded
            );
            // Synchronous callers can complete uncached despite pinned entries.
            assert_eq!(
                placement
                    .rank(members.clone(), &object(), PageNumber(1))
                    .ordered,
                Placement::new(0)
                    .rank(members.clone(), &object(), PageNumber(1))
                    .ordered
            );
            drop(first);
            let ranked = futures::executor::block_on(second).unwrap();
            assert_eq!(
                ranked.ordered,
                placement
                    .rank(members.clone(), &object(), PageNumber(0))
                    .ordered
            );
            placement.rank(members.clone(), &object(), PageNumber(1));
            let before = scored_members();
            placement.rank(members.clone(), &object(), PageNumber(1));
            assert_eq!(scored_members(), before);
            assert_eq!(Arc::strong_count(&members), 2); // caller and returned Candidates only
            let uncached = Placement::new(0);
            uncached.rank(members.clone(), &object(), PageNumber(0));
            let before = scored_members();
            uncached.rank(members, &object(), PageNumber(0));
            assert_eq!(
                scored_members(),
                before,
                "uncached ranking uses frozen weights"
            );
        }
    }

    pub(crate) mod scenarios {
        use super::*;
        use crate::topology::LinkOutcome;
        use crate::topology::tests::fixtures::membership;
        use std::time::Duration;
        fn budget(
            members: &std::sync::Arc<crate::topology::Membership>,
            to: usize,
            links: u8,
        ) -> RouteBudget {
            RouteBudget {
                membership: members.version,
                request: RequestId([1; 16]),
                attempt: AttemptId([2; 16]),
                destination: members.members()[to].node.clone(),
                visited: vec![],
                remaining_links: links,
                remaining_attempts: 3,
                deadline: Deadline(Instant::now() + Duration::from_secs(60)),
            }
        }
        /// Hold `peer` silent: two unanswered last links, then a failed ping.
        fn silence(health: &LinkHealth, peer: &NodeId) {
            let page = crate::model::PageId {
                version: crate::model::ObjectVersion {
                    object: crate::topology::tests::fixtures::object(),
                    etag: crate::model::StrongEtag::test_value("v"),
                },
                number: PageNumber(0),
            };
            let sent = Instant::now() - Duration::from_secs(5);
            for _ in 0..2 {
                health.silence_timed_out(
                    peer,
                    page.clone(),
                    sent,
                    racer_core::liveness::LINK_TIMEOUTS,
                );
            }
            assert_eq!(health.due_pings(Instant::now(), 8).len(), 1);
            assert!(health.ping_outcome(peer, Some(false), Instant::now()));
            assert!(health.silent(peer));
        }
        fn probe(
            paths: &Paths,
            members: &std::sync::Arc<crate::topology::Membership>,
            source: &NodeId,
            request: &RouteBudget,
            ping: bool,
        ) -> Result<Route> {
            let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
            futures::executor::block_on(paths.shortest_probe_async(
                members.clone(),
                source,
                request,
                &scope,
                ping,
            ))
        }
        /// Known issue 43: an origin's recovery ping to a neighbor it holds
        /// silent goes over the direct link; other requests route around it,
        /// and an open circuit or a silent intermediate still blocks.
        #[test]
        fn a_ping_reaches_the_silent_neighbor_it_tests() {
            let members = membership(64);
            let source = &members.members()[0].node;
            let neighbors = members.neighbors(source).unwrap();
            let target = neighbors[0].clone();
            let to = members.position(&target).unwrap();
            let health = Rc::new(
                LinkHealth::default().with_silence(Duration::from_secs(1), Duration::from_secs(1)),
            );
            let paths = Paths::new(health.clone(), 0);
            let request = budget(&members, to, 1);
            // Healthy: both reach it directly.
            for ping in [false, true] {
                let route = probe(&paths, &members, source, &request, ping).unwrap();
                assert_eq!(route.nodes[1], target);
            }
            silence(&health, &target);
            // A one-link budget: only the direct link reaches it.
            assert_eq!(
                probe(&paths, &members, source, &request, false).unwrap_err(),
                Error::Unavailable
            );
            let route = probe(&paths, &members, source, &request, true).unwrap();
            assert_eq!(route.nodes, vec![source.clone(), target.clone()]);
            // A silent intermediate is still routed around for a ping to a
            // non-neighbor whose only short path crosses it.
            let far = members
                .members()
                .iter()
                .map(|m| m.node.clone())
                .find(|n| n != source && !neighbors.contains(n))
                .unwrap();
            let far_request = budget(&members, members.position(&far).unwrap(), 4);
            for ping in [false, true] {
                if let Ok(route) = probe(&paths, &members, source, &far_request, ping) {
                    assert_ne!(route.nodes[1], target);
                }
            }
            // An open circuit blocks even a ping.
            health
                .observe_at(
                    &target,
                    LinkOutcome::Timeout,
                    Instant::now() + Duration::from_secs(60),
                )
                .unwrap();
            assert_eq!(
                probe(&paths, &members, source, &request, true).unwrap_err(),
                Error::Unavailable
            );
        }
        #[test]
        fn path_search_limit_is_independent_of_cache_and_released_on_drop() {
            let members = membership(100_000);
            let source = &members.members()[0].node;
            let paths = Paths::with_limits(Rc::new(LinkHealth::default()), 0, 1, 1);
            let request = budget(&members, 80_003, NORMAL_LINKS);
            let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
            let mut first = paths.shortest_async(members.clone(), source, &request, &scope);
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            assert!(first.as_mut().poll(&mut cx).is_pending());
            let other = budget(&members, 80_004, NORMAL_LINKS);
            assert_eq!(
                paths.shortest(members.clone(), source, &other).unwrap_err(),
                Error::Overloaded
            );
            drop(first);
            assert!(paths.shortest(members.clone(), source, &other).is_ok());
            assert_eq!(
                map_error(::topology::Error::SamplingExhausted),
                Error::Internal
            );
            assert_eq!(
                placement_error(::topology::Error::SamplingExhausted),
                Error::Internal
            );
        }
        /// Known issue 10: production `Paths::new` queues searches beyond its 8
        /// slots instead of failing them with `Overloaded`; a queued search runs
        /// when a slot frees, and a cancelled one leaves the queue.
        #[test]
        fn searches_beyond_the_slot_limit_wait_for_a_slot() {
            let members = membership(100_000);
            let source = &members.members()[0].node;
            let paths = Paths::new(Rc::new(LinkHealth::default()), 0);
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            let budgets: Vec<_> = (0..10)
                .map(|i| budget(&members, 80_003 + i, NORMAL_LINKS))
                .collect();
            let scopes: Vec<_> = budgets
                .iter()
                .map(|b| RequestScope::new(b.request, b.deadline.0).unwrap())
                .collect();
            let mut running: Vec<_> = budgets[..8]
                .iter()
                .zip(&scopes)
                .map(|(b, scope)| paths.shortest_async(members.clone(), source, b, scope))
                .collect();
            for search in &mut running {
                assert!(search.as_mut().poll(&mut cx).is_pending());
            }
            let mut cancelled =
                paths.shortest_async(members.clone(), source, &budgets[8], &scopes[8]);
            assert!(cancelled.as_mut().poll(&mut cx).is_pending());
            let mut waiting =
                paths.shortest_async(members.clone(), source, &budgets[9], &scopes[9]);
            assert!(waiting.as_mut().poll(&mut cx).is_pending());
            drop(cancelled);
            drop(running.pop());
            let route = loop {
                if let std::task::Poll::Ready(route) = waiting.as_mut().poll(&mut cx) {
                    break route;
                }
            };
            assert!(route.is_ok());
        }
        #[test]
        fn independent_hash_vectors_cache_reselection_and_snapshot_weights() {
            let mut input = membership(1500).members().to_vec();
            for (i, member) in input.iter_mut().enumerate() {
                member.shares = std::num::NonZeroU32::new(if i % 3 == 0 { 1 } else { 4 }).unwrap();
            }
            let members = Arc::new(
                Membership::validate(racer_control_wire::MembershipVersion(1), input).unwrap(),
            );
            let cached = Paths::new(Rc::new(LinkHealth::default()), 1);
            let cold = Paths::new(Rc::new(LinkHealth::default()), 0);
            for (attempt, expected) in crate::topology::tests::fixtures::NEXT_HOPS {
                let mut request = budget(&members, 1499, NORMAL_LINKS);
                request.attempt = AttemptId(attempt.to_be_bytes());
                let from = &members.members()[0].node;
                let actual = cached.shortest(members.clone(), from, &request).unwrap();
                assert_eq!(actual.nodes[1], members.members()[expected].node);
                assert!(members.neighbors(from).unwrap().contains(&actual.nodes[1]));
                let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
                assert_eq!(
                    actual.nodes,
                    futures::executor::block_on(cold.shortest_async(
                        members.clone(),
                        from,
                        &request,
                        &scope
                    ))
                    .unwrap()
                    .nodes
                );
            }
            let mut changed = members.members().to_vec();
            let favored = cached
                .shortest(
                    members.clone(),
                    &members.members()[0].node,
                    &budget(&members, 1499, 4),
                )
                .unwrap()
                .nodes[1]
                .clone();
            let favored_index = members.position(&favored).unwrap();
            changed[favored_index].shares = std::num::NonZeroU32::new(u32::MAX).unwrap();
            let changed = Arc::new(
                Membership::validate(racer_control_wire::MembershipVersion(2), changed).unwrap(),
            );
            let route = cached
                .shortest(
                    changed.clone(),
                    &changed.members()[0].node,
                    &budget(&changed, 1499, 4),
                )
                .unwrap();
            assert_eq!(route.nodes[1], favored);
            // Private eviction assertions live in topology::paths tests.
        }
        #[test]
        fn health_eviction_budget_deadline_and_cancellation() {
            let members = membership(1500);
            let source = &members.members()[0].node;
            let health = Rc::new(LinkHealth::default());
            let paths = Paths::new(health.clone(), 1);
            let request = budget(&members, 1499, NORMAL_LINKS);
            let original = paths.shortest(members.clone(), source, &request).unwrap();
            let mut blocked = request.clone();
            blocked.visited.push(original.nodes[1].clone());
            let alternate = paths.shortest(members.clone(), source, &blocked).unwrap();
            assert_eq!(alternate.nodes.len(), original.nodes.len());
            assert!(!alternate.nodes.contains(&original.nodes[1]));
            health
                .observe_at(
                    &original.nodes[1],
                    LinkOutcome::Timeout,
                    Instant::now() + Duration::from_secs(60),
                )
                .unwrap();
            assert_ne!(
                paths
                    .shortest(members.clone(), source, &request)
                    .unwrap()
                    .nodes[1],
                original.nodes[1]
            );
            for neighbor in members.neighbors(source).unwrap() {
                health
                    .observe_at(
                        &neighbor,
                        LinkOutcome::Timeout,
                        Instant::now() + Duration::from_secs(60),
                    )
                    .unwrap();
            }
            for (case, expected) in [
                (0, Error::Unavailable),
                (1, Error::HopBudgetExhausted),
                (2, Error::InvalidRequest),
                (3, Error::DeadlineExceeded),
            ] {
                let mut blocked = request.clone();
                match case {
                    1 => blocked.remaining_links = 0,
                    2 => blocked.visited.push(source.clone()),
                    3 => blocked.deadline = Deadline(Instant::now()),
                    _ => {}
                }
                assert_eq!(
                    paths
                        .shortest(members.clone(), source, &blocked)
                        .unwrap_err(),
                    expected
                );
            }
            let cold = Paths::new(Rc::new(LinkHealth::default()), 0);
            assert_eq!(
                cold.shortest(members.clone(), source, &budget(&members, 0, 0))
                    .unwrap()
                    .nodes,
                vec![source.clone()]
            );
            let request = budget(&members, 1499, 4);
            let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
            let mut operation = cold.shortest_async(members.clone(), source, &request, &scope);
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            let _ = operation.as_mut().poll(&mut cx);
            drop(operation);
            // A fresh cold search succeeds after dropping the previous operation.
            assert!(cold.shortest(members.clone(), source, &request).is_ok());
        }
        /// Forwarding a search-form budget goes through the one rule in
        /// `racer_core::route`; the signed wire form is checked in peer-wire.
        #[test]
        fn forwarded_budget_is_monotonic_and_rejects_cycles() {
            let members = membership(100);
            let request = budget(&members, 99, 4);
            let from = &members.members()[0].node;
            let next = &members.members()[1].node;
            let now = uring_runtime::environment::now();
            let forward = |visited: &[NodeId], holder: &NodeId, to: &NodeId, links: u8| {
                racer_core::route::forward(
                    visited,
                    holder,
                    to,
                    &request.destination,
                    links,
                    request.deadline.0,
                    now,
                )
            };
            let links = forward(&request.visited, from, next, request.remaining_links).unwrap();
            assert_eq!(links, 3);
            let mut forwarded = request.clone();
            forwarded.visited.push(from.clone());
            forwarded.remaining_links = links;
            forwarded.validate_at(next, now).unwrap();
            assert_eq!(
                forward(&forwarded.visited, next, from, links),
                Err(racer_core::route::RouteFault::Invalid)
            );
            assert_eq!(
                forward(&forwarded.visited, next, next, links),
                Err(racer_core::route::RouteFault::Invalid)
            );
            assert_eq!(
                forward(&forwarded.visited, next, &members.members()[2].node, 0),
                Err(racer_core::route::RouteFault::HopBudgetExhausted)
            );
            // A holder that already appears in `visited` is a cycle.
            assert!(forwarded.validate_at(from, now).is_err());
            assert_eq!(
                racer_core::route::forward(
                    &request.visited,
                    from,
                    next,
                    &request.destination,
                    request.remaining_links,
                    request.deadline.0,
                    request.deadline.0,
                ),
                Err(racer_core::route::RouteFault::DeadlineExceeded)
            );
        }
        #[test]
        fn rejects_unknown_and_empty_membership() {
            let empty = membership(0);
            assert!(empty.neighbors(&NodeId("unknown".into())).is_err());
            let one = membership(1);
            assert!(one.neighbors(&one.members()[0].node).unwrap().is_empty());
        }

        #[test]
        fn is_neighbor_matches_allocated_neighbors() {
            let members = membership(100);
            let unknown = NodeId("unknown".into());
            for source in members.members() {
                let neighbors = members.neighbors(&source.node).unwrap();
                for other in members.members() {
                    assert_eq!(
                        members.is_neighbor(&source.node, &other.node),
                        neighbors.contains(&other.node)
                    );
                }
                assert!(!members.is_neighbor(&source.node, &source.node));
                assert!(!members.is_neighbor(&source.node, &unknown));
                assert!(!members.is_neighbor(&unknown, &source.node));
            }
        }

        #[test]
        fn yielded_search_rechecks_health_and_retains_only_current_neighbors() {
            let members = membership(100_000);
            let source = &members.members()[0].node;
            let health = Rc::new(LinkHealth::default());
            let paths = Paths::new(health.clone(), 1);
            let request = budget(&members, 80_003, 4);
            health
                .observe(&NodeId("retired".into()), LinkOutcome::Timeout)
                .unwrap();
            let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
            let mut operation = paths.shortest_async(members.clone(), source, &request, &scope);
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(health.tracked_links(), 0);
            let neighbor = members.neighbors(source).unwrap()[0].clone();
            health
                .observe_at(
                    &neighbor,
                    LinkOutcome::Timeout,
                    Instant::now() + Duration::from_secs(60),
                )
                .unwrap();
            assert_eq!(
                futures::executor::block_on(operation).unwrap_err(),
                Error::Unavailable
            );
            let retried = paths.shortest(members.clone(), source, &request).unwrap();
            assert_ne!(retried.nodes[1], neighbor);
        }

        #[test]
        fn yielded_search_rechecks_adaptive_peer_admission() {
            use crate::peer::AdaptivePeers;
            use crate::peer::Config;
            use crate::peer::Outcome;
            let clock = uring_runtime::environment::SimulationClock::new(110);
            let _env = clock.environment(0).enter();
            let members = membership(100_000);
            let source = &members.members()[0].node;
            let admission = AdaptivePeers::new(Config::default(), Default::default()).unwrap();
            let paths = Paths::new(Rc::new(LinkHealth::default()), 1)
                .with_peer_admission(admission.clone());
            let mut request = budget(&members, 80_003, 4);
            request.deadline =
                Deadline(uring_runtime::environment::now() + Duration::from_secs(60));
            let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
            let mut operation = paths.shortest_async(members.clone(), source, &request, &scope);
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            let neighbor = members.neighbors(source).unwrap()[0].clone();
            let permit = admission.acquire(&neighbor).unwrap();
            permit.observe(Outcome::PeerFailure);
            drop(permit);
            assert_eq!(
                futures::executor::block_on(operation).unwrap_err(),
                Error::Unavailable
            );
            assert_ne!(
                paths
                    .shortest(members.clone(), source, &request)
                    .unwrap()
                    .nodes[1],
                neighbor
            );
            assert_eq!(paths.link_health().tracked_links(), 0);
        }

        #[test]
        fn yielded_and_cached_searches_enforce_scope_budget_and_membership() {
            let clock = uring_runtime::environment::SimulationClock::new(109);
            let _env = clock.environment(0).enter();
            let members = membership(100_000);
            let source = &members.members()[0].node;
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            for case in 0..3 {
                let paths = Paths::new(Rc::new(LinkHealth::default()), 1);
                let mut request = budget(&members, 80_003, 4);
                let now = uring_runtime::environment::now();
                request.deadline =
                    Deadline(now + Duration::from_secs(if case == 1 { 1 } else { 60 }));
                let scope = RequestScope::new(
                    request.request,
                    now + Duration::from_secs(if case == 2 { 1 } else { 60 }),
                )
                .unwrap();
                let mut operation = paths.shortest_async(members.clone(), source, &request, &scope);
                assert!(operation.as_mut().poll(&mut cx).is_pending());
                if case == 0 {
                    scope.cancel().unwrap();
                } else {
                    clock.advance(Duration::from_secs(2));
                }
                let expected = if case == 0 {
                    Error::Cancelled
                } else {
                    Error::DeadlineExceeded
                };
                assert_eq!(
                    futures::executor::block_on(operation).unwrap_err(),
                    expected
                );
                // Failure drops the algorithm admission before a new request starts.
                let mut fresh = request.clone();
                fresh.deadline =
                    Deadline(uring_runtime::environment::now() + Duration::from_secs(60));
                paths.shortest(members.clone(), source, &fresh).unwrap();
                assert_eq!(
                    futures::executor::block_on(paths.shortest_async(
                        members.clone(),
                        source,
                        &request,
                        &scope
                    ))
                    .unwrap_err(),
                    expected
                );
                fresh.membership = racer_control_wire::MembershipVersion(2);
                assert_eq!(
                    paths.shortest(members.clone(), source, &fresh).unwrap_err(),
                    Error::IncompatibleMembership
                );
            }
        }
    }
}
