use crate::{
    error::{Error, Operation, Result},
    model::{AttemptId, MembershipVersion, NodeId, ObjectId, PageNumber, RequestId},
    runtime::deadline::{Deadline, RequestScope},
};
pub use ::topology::MAX_DEGREE;
use racer_control_wire::{RailMapping, valid_site};
use sha2::{Digest, Sha256};
use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet},
    future::Future,
    net::SocketAddr,
    num::NonZeroU32,
    rc::Rc,
    sync::Arc,
    task::Poll,
    time::{Duration, Instant},
};

// Immutable placement, authenticated routing, and worker-local endpoint circuits.

#[cfg(test)]
pub(crate) const RADIX: usize = 32;

/// Shared capacity bound for every supported topology, including first-hop masks.
const _: () = assert!(MAX_DEGREE <= u64::BITS as usize);

// Worker-local link circuits, distinct from process readiness and placement.
pub struct LinkHealth {
    capacity: usize,
    states: RefCell<BTreeMap<NodeId, Circuit>>,
    probes: RefCell<BTreeSet<NodeId>>,
}
pub struct LinkProbe<'a> {
    health: &'a LinkHealth,
    node: Option<NodeId>,
}
impl Drop for LinkProbe<'_> {
    fn drop(&mut self) {
        if let Some(node) = &self.node {
            self.health.probes.borrow_mut().remove(node);
        }
    }
}
#[allow(non_upper_case_globals, clippy::declare_interior_mutable_const)]
pub const LinkHealth: LinkHealth = LinkHealth::new(MAX_DEGREE);
struct Circuit {
    failures: u32,
    retry_at: Instant,
    probe_until: Option<Instant>,
}
#[derive(Clone, Copy, Debug)]
pub enum LinkOutcome {
    Success,
    Timeout,
    Refused,
    ProtocolFailure,
}
impl LinkHealth {
    pub fn acquire(&self, node: &NodeId) -> Result<LinkProbe<'_>> {
        if !self.try_acquire(node)? {
            return Err(Error::Unavailable);
        }
        let probe = self.states.borrow().contains_key(node);
        if probe {
            if self.probes.borrow().len() >= self.capacity {
                return Err(Error::Overloaded);
            }
            self.probes.borrow_mut().insert(node.clone());
        }
        Ok(LinkProbe {
            health: self,
            node: probe.then(|| node.clone()),
        })
    }
    /// Application misses and credential rejection do not open transport circuits.
    pub async fn run<T>(
        &self,
        endpoint: &NodeId,
        operation: impl std::future::Future<Output = Result<T>>,
    ) -> Result<T> {
        let _probe = self.acquire(endpoint)?;
        let result = operation.await;
        let outcome = match &result {
            Err(Error::Io | Error::Unavailable) => Some(LinkOutcome::Refused),
            Err(Error::DeadlineExceeded) => Some(LinkOutcome::Timeout),
            Err(Error::BadGateway | Error::CorruptRecord) => Some(LinkOutcome::ProtocolFailure),
            Err(Error::Cancelled | Error::Overloaded) => None,
            _ => Some(LinkOutcome::Success),
        };
        if let Some(outcome) = outcome {
            let _ = self.observe(endpoint, outcome);
        }
        result
    }
    pub const fn new(capacity: usize) -> Self {
        Self {
            capacity,
            states: RefCell::new(BTreeMap::new()),
            probes: RefCell::new(BTreeSet::new()),
        }
    }
    pub fn observe(&self, neighbor: &NodeId, outcome: LinkOutcome) -> Result<()> {
        self.observe_at(neighbor, outcome, uring_runtime::environment::now())
    }
    pub fn observe_at(&self, neighbor: &NodeId, outcome: LinkOutcome, now: Instant) -> Result<()> {
        let mut states = self.states.borrow_mut();
        if matches!(outcome, LinkOutcome::Success) {
            states.remove(neighbor);
            return Ok(());
        }
        if !states.contains_key(neighbor) && states.len() >= self.capacity {
            return Err(Error::Overloaded);
        }
        let state = states.entry(neighbor.clone()).or_insert(Circuit {
            failures: 0,
            retry_at: now,
            probe_until: None,
        });
        state.failures = state.failures.saturating_add(1);
        state.retry_at = now + backoff(neighbor, state.failures);
        state.probe_until = None;
        Ok(())
    }
    /// Routing hint only; actual sends acquire an exclusive half-open probe.
    pub fn available(&self, neighbor: &NodeId) -> Result<bool> {
        self.available_at(neighbor, uring_runtime::environment::now())
    }
    pub fn available_at(&self, neighbor: &NodeId, now: Instant) -> Result<bool> {
        if self.probes.borrow().contains(neighbor) {
            return Ok(false);
        }
        Ok(self
            .states
            .borrow()
            .get(neighbor)
            .is_none_or(|s| now >= s.retry_at && s.probe_until.is_none_or(|until| now >= until)))
    }
    pub fn try_acquire(&self, neighbor: &NodeId) -> Result<bool> {
        self.try_acquire_at(neighbor, uring_runtime::environment::now())
    }
    pub fn try_acquire_at(&self, neighbor: &NodeId, now: Instant) -> Result<bool> {
        if self.probes.borrow().contains(neighbor) {
            return Ok(false);
        }
        let mut states = self.states.borrow_mut();
        let Some(state) = states.get_mut(neighbor) else {
            return Ok(true);
        };
        if now < state.retry_at || state.probe_until.is_some_and(|until| now < until) {
            return Ok(false);
        }
        // A dropped or hung probe releases eligibility only after this timeout.
        state.probe_until = Some(now + Duration::from_secs(1));
        Ok(true)
    }
    pub fn retain_neighbors(&self, neighbors: &[NodeId]) {
        self.states
            .borrow_mut()
            .retain(|node, _| neighbors.contains(node));
    }
    pub fn tracked_links(&self) -> usize {
        self.states.borrow().len()
    }
}
fn backoff(node: &NodeId, failures: u32) -> Duration {
    let base = (100u64 << failures.saturating_sub(1).min(8)).min(20_000);
    let mut digest = hash_domain(b"racer/link-backoff/v1\0");
    hash_bytes(&mut digest, node.0.as_bytes());
    digest.update(failures.to_be_bytes());
    let digest = hash_finish(digest);
    let jitter = u64::from(u16::from_be_bytes([digest[0], digest[1]])) % (base / 4 + 1);
    Duration::from_millis(base + jitter)
}

pub(crate) fn hash_domain(name: &[u8]) -> Sha256 {
    let mut hash = Sha256::new();
    hash.update(name);
    hash
}
pub(crate) fn hash_bytes(hash: &mut Sha256, value: &[u8]) {
    hash.update((value.len() as u32).to_be_bytes());
    hash.update(value);
}
pub(crate) fn hash_object(hash: &mut Sha256, object: &ObjectId, page: PageNumber) {
    hash_bytes(hash, object.cache.0.as_bytes());
    hash.update(object.key.0);
    hash.update(page.0.to_be_bytes());
}
pub(crate) fn hash_finish(hash: Sha256) -> [u8; 32] {
    hash.finalize().into()
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
    // Counts algorithm weight reads, including scoring. Placement/selection tests
    // sample only ranking polls, excluding publication and weighted path queries.
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
    pub fn validate(version: MembershipVersion, mut members: Vec<Member>) -> Result<Self> {
        if version.0 == 0 || members.len() > MAX_MEMBERS {
            return Err(Error::InvalidConfiguration);
        }
        for member in &mut members {
            if !valid_identity(&member.node.0) || !valid_site(&member.site) {
                return Err(Error::InvalidConfiguration);
            }
            let endpoint: SocketAddr = member
                .peer_endpoint
                .parse()
                .map_err(|_| Error::InvalidConfiguration)?;
            // Match Go netip.ParseAddrPort: publication validity is independent
            // of local reachability, but zones are not portable topology inputs.
            if endpoint.port() == 0 || member.peer_endpoint.contains('%') {
                return Err(Error::InvalidConfiguration);
            }
            member.rails.sort_unstable_by(|a, b| {
                (a.rail, &a.device, a.port).cmp(&(b.rail, &b.device, b.port))
            });
            let mut physical = std::collections::BTreeSet::new();
            if member.rails.len() > 64
                || member.rails.iter().any(|mapping| {
                    !valid_fabric(&mapping.device)
                        || mapping.port == 0
                        || !physical.insert((&mapping.device, mapping.port))
                        || mapping
                            .numa_node
                            .is_some_and(|numa| u32::try_from(numa).is_err())
                })
            {
                return Err(Error::InvalidConfiguration);
            }
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
            + members.capacity() * std::mem::size_of::<Member>()
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
            inner: ::topology::Membership::new(members).map_err(|_| Error::InvalidConfiguration)?,
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
        self.retained_bytes + 64 * std::mem::size_of::<(Option<usize>, Option<usize>)>()
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
    pub fn member(&self, node: &NodeId) -> Result<&Member> {
        Ok(&self.members()[self.position(node)?])
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

fn valid_fabric(value: &str) -> bool {
    // Go wire.validRail accepts any nonempty UTF-8 string except NUL/CR/LF.
    // String guarantees UTF-8 here. The bounded publication codec owns the
    // aggregate byte limit; the wire contract has no per-fabric length limit.
    !value.is_empty() && !value.contains(['\0', '\r', '\n'])
}

// Canonical slot encoding, placement, authenticated routing, and cancellation.
// Graph/search/weighted selection live in the runtime-independent topology crate.

pub const SLOT_COUNT: u32 = 1 << 20;
/// Conservative allocation charge: ranking, four scores, Rc/RefCell, BTree
/// entry and FIFO key, including container slack and allocator overhead.
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
    key.extend_from_slice(&(object.cache.0.len() as u32).to_be_bytes());
    key.extend_from_slice(object.cache.0.as_bytes());
    key.extend_from_slice(&object.key.0);
    key.extend_from_slice(&page.0.to_be_bytes());
    key
}

/// Fixed slot independent of object version and membership. Metadata passes page 0.
pub fn slot(object: &ObjectId, page: PageNumber) -> u32 {
    let mut digest = hash_domain(b"racer/slot/v1\0");
    hash_object(&mut digest, object, page);
    let digest = hash_finish(digest);
    u32::from_be_bytes(digest[..4].try_into().unwrap()) >> 12
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
    ) -> Result<Candidates> {
        let ranked = self
            .inner
            .rank(&membership.inner, &encoded_key(object, page))
            .map_err(placement_error)?;
        Ok(candidates(membership, ranked))
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
        scope: Option<&'a crate::runtime::deadline::RequestScope>,
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
    pub fn maintain(&self, membership: &std::sync::Arc<crate::topology::Membership>) -> Result<()> {
        self.inner
            .maintain(&membership.inner)
            .map_err(placement_error)
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
    pub membership: crate::model::MembershipVersion,
    pub request: RequestId,
    pub attempt: AttemptId,
    pub destination: NodeId,
    pub visited: Vec<NodeId>,
    pub remaining_links: u8,
    /// Acquisition credits transferred from the caller, never replenished by a hop.
    pub remaining_attempts: u32,
    pub deadline: Deadline,
}
pub const NORMAL_LINKS: u8 = 4;
pub const FAILURE_LINKS: u8 = 8;
impl RouteBudget {
    /// `visited` contains prior senders, excluding the current recipient.
    pub fn forwarded(&self, from: &NodeId, next: &NodeId) -> Result<Self> {
        self.validate_at(from, uring_runtime::environment::now())?;
        if self.remaining_links == 0 {
            return Err(Error::HopBudgetExhausted);
        }
        if next == from || self.visited.contains(next) {
            return Err(Error::InvalidRequest);
        }
        let mut forwarded = self.clone();
        forwarded.visited.push(from.clone());
        forwarded.remaining_links -= 1;
        Ok(forwarded)
    }
    /// Authentication verifies this monotonic relationship between signed hops.
    pub fn validate_forwarded(&self, forwarded: &Self, from: &NodeId, next: &NodeId) -> Result<()> {
        let expected = self.forwarded(from, next)?;
        if forwarded.membership != expected.membership
            || forwarded.request != expected.request
            || forwarded.attempt != expected.attempt
            || forwarded.destination != expected.destination
            || forwarded.visited != expected.visited
            || forwarded.remaining_links != expected.remaining_links
            || forwarded.remaining_attempts > expected.remaining_attempts
            || forwarded.deadline.0 > expected.deadline.0
        {
            return Err(Error::InvalidRequest);
        }
        forwarded.validate_at(next, uring_runtime::environment::now())
    }
    fn validate_at(&self, from: &NodeId, now: Instant) -> Result<()> {
        if now >= self.deadline.0 {
            return Err(Error::DeadlineExceeded);
        }
        if self.visited.len() > usize::from(FAILURE_LINKS)
            || self.visited.len() + usize::from(self.remaining_links) > usize::from(FAILURE_LINKS)
        {
            return Err(Error::HopBudgetExhausted);
        }
        if self.visited.contains(from)
            || self.visited.contains(&self.destination)
            || self
                .visited
                .iter()
                .enumerate()
                .any(|(i, node)| self.visited[..i].contains(node))
        {
            return Err(Error::InvalidRequest);
        }
        Ok(())
    }
}

pub struct Paths {
    pub(crate) peer_admission: Option<Arc<crate::peer::adaptive::AdaptivePeers>>,
    health: Rc<LinkHealth>,
    inner: ::topology::Paths,
}
impl Paths {
    pub(crate) fn with_peer_admission(
        mut self,
        admission: Arc<crate::peer::adaptive::AdaptivePeers>,
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
            let blocked = self.blocked(&membership, source)?;
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
            if self.blocked(&membership, source)? != blocked {
                return Err(Error::Unavailable);
            }
            let nodes = nodes?
                .into_iter()
                .map(|index| membership.members()[index].node.clone())
                .collect();
            Ok(Route { membership, nodes })
        })
    }

    fn blocked(&self, membership: &Membership, source: usize) -> Result<Vec<usize>> {
        let positions = membership.inner.neighbors(source);
        let neighbors: Vec<_> = positions
            .iter()
            .map(|&i| membership.members()[i].node.clone())
            .collect();
        self.health.retain_neighbors(&neighbors);
        let mut blocked = Vec::new();
        for (position, node) in positions.into_iter().zip(&neighbors) {
            if !self.health.available(node)?
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

    pub(crate) mod fixtures {
        use super::*;
        use crate::model::*;
        use crate::topology::Member;
        use crate::topology::Membership;
        use std::{num::NonZeroU32, sync::Arc};

        // Independent Python hashlib + outgoing-edge BFS vectors. N=1500, source=0,
        // destination=1499, request=[1;16], shares=1 at multiples of 3 and 4 elsewhere.
        // Attempts are big-endian u128; V5 intentionally retains the V4 hash domain.
        pub(crate) const V5_NEXT_HOPS: [(u128, usize); 4] =
            [(0, 1312), (1, 937), (2, 703), (127, 937)];
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
                .unwrap()
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
                placement
                    .rank(members, &object(), PageNumber(0))
                    .unwrap()
                    .ordered
            );
            health.retain_neighbors(&[]);
            assert_eq!(health.tracked_links(), 0);
        }
        #[test]
        fn default_tracks_all_64_neighbors_without_eviction() {
            let health = LinkHealth;
            let now = Instant::now();
            for i in 0..64 {
                health
                    .observe_at(&NodeId(format!("peer-{i}")), LinkOutcome::Timeout, now)
                    .unwrap();
            }
            assert_eq!(health.tracked_links(), 64);
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
                    2,
                    "500304ace38caf7cc36f8f97ae12bdfc89b92f7eb64c8bb060d1daf49f76314c",
                ),
                (
                    b"racer/rail/v2\0",
                    0,
                    "9ac860a9df3ce1ac648cde8350aeec1d1efc604eeae5cceb70d67dd55f4443d1",
                ),
            ] {
                let mut hash = hash_domain(name);
                if kind == 1 {
                    hash.update(887651u32.to_be_bytes());
                    hash_bytes(&mut hash, b"node-000000");
                } else {
                    hash_object(&mut hash, &fixtures::object(), PageNumber(0));
                    if kind == 2 {
                        hash_bytes(&mut hash, b"\"v1\"");
                    }
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
                "/../../internal/racer/wire/testdata/publication.json"
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
        use crate::model::MembershipVersion;
        use crate::topology::Membership;
        use crate::topology::scored_members;
        use crate::topology::tests::fixtures::member;
        use crate::topology::tests::fixtures::membership;
        use crate::topology::tests::fixtures::object;
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
                let ranking = placement
                    .rank(members.clone(), &object(), PageNumber(page))
                    .unwrap();
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
                    .unwrap()
                    .ordered;
                let extended = placement
                    .rank(added.clone(), &object(), PageNumber(page))
                    .unwrap()
                    .ordered;
                let survivors: Vec<_> = extended
                    .iter()
                    .filter(|node| **node != member(12, 1).node)
                    .cloned()
                    .collect();
                assert_eq!(&original[..survivors.len()], survivors);
                let reduced = placement
                    .rank(removed.clone(), &object(), PageNumber(page))
                    .unwrap()
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
                        .unwrap()
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
                        .unwrap()
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
                        .unwrap()
                        .ordered,
                    placement
                        .rank(changed.clone(), &object(), PageNumber(page))
                        .unwrap()
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
                    placement
                        .rank(old.clone(), &object(), PageNumber(page))
                        .unwrap();
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
                            .unwrap()
                            .ordered,
                        oracle
                            .rank(next.clone(), &object(), PageNumber(page))
                            .unwrap()
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
            let expected = placement
                .rank(old.clone(), &object(), PageNumber(0))
                .unwrap();
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
            placement.rank(current, &object(), PageNumber(0)).unwrap();
            assert_eq!(scored_members(), before);
        }

        #[test]
        fn scoped_cold_rank_cancels_between_bounded_quanta() {
            use crate::model::RequestId;
            use crate::runtime::deadline::RequestScope;
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
            assert_eq!(scored_members() - before, 256);
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
            assert_eq!(scored_members() - before, 512);
            assert_eq!(
                placement
                    .rank(members.clone(), &object(), PageNumber(1))
                    .unwrap_err(),
                Error::Overloaded
            );
            drop(first);
            let ranked = futures::executor::block_on(second).unwrap();
            assert_eq!(
                ranked.ordered,
                placement
                    .rank(members.clone(), &object(), PageNumber(0))
                    .unwrap()
                    .ordered
            );
            placement
                .rank(members.clone(), &object(), PageNumber(1))
                .unwrap();
            let before = scored_members();
            placement
                .rank(members.clone(), &object(), PageNumber(1))
                .unwrap();
            assert_eq!(scored_members(), before);
            assert_eq!(Arc::strong_count(&members), 2); // caller and returned Candidates only
            let uncached = Placement::new(0);
            uncached
                .rank(members.clone(), &object(), PageNumber(0))
                .unwrap();
            let before = scored_members();
            uncached.rank(members, &object(), PageNumber(0)).unwrap();
            assert_eq!(scored_members() - before, 100_000);
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
        #[test]
        fn independent_hash_vectors_cache_reselection_and_snapshot_weights() {
            let mut input = membership(1500).members().to_vec();
            for (i, member) in input.iter_mut().enumerate() {
                member.shares = std::num::NonZeroU32::new(if i % 3 == 0 { 1 } else { 4 }).unwrap();
            }
            let members =
                Arc::new(Membership::validate(crate::model::MembershipVersion(1), input).unwrap());
            let cached = Paths::new(Rc::new(LinkHealth), 1);
            let cold = Paths::new(Rc::new(LinkHealth), 0);
            for (attempt, expected) in crate::topology::tests::fixtures::V5_NEXT_HOPS {
                let mut request = budget(&members, 1499, NORMAL_LINKS);
                request.attempt = AttemptId(attempt.to_be_bytes());
                let from = &members.members()[0].node;
                let actual = cached.shortest(members.clone(), from, &request).unwrap();
                assert_eq!(actual.nodes[1], members.members()[expected].node);
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
            changed[1312].shares = std::num::NonZeroU32::new(u32::MAX).unwrap();
            let changed = Arc::new(
                Membership::validate(crate::model::MembershipVersion(2), changed).unwrap(),
            );
            let route = cached
                .shortest(
                    changed.clone(),
                    &changed.members()[0].node,
                    &budget(&changed, 1499, 4),
                )
                .unwrap();
            assert_eq!(route.nodes[1], changed.members()[1312].node);
            // Private eviction assertions moved to topology::paths tests.
        }
        #[test]
        fn health_eviction_budget_deadline_and_cancellation() {
            let members = membership(1500);
            let source = &members.members()[0].node;
            let health = Rc::new(LinkHealth);
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
            let cold = Paths::new(Rc::new(LinkHealth), 0);
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
        #[test]
        fn forwarded_budget_is_monotonic_and_rejects_cycles() {
            let members = membership(100);
            let request = budget(&members, 99, 4);
            let from = &members.members()[0].node;
            let next = &members.members()[1].node;
            let forwarded = request.forwarded(from, next).unwrap();
            request.validate_forwarded(&forwarded, from, next).unwrap();
            assert_eq!(forwarded.remaining_links, 3);
            assert_eq!(forwarded.remaining_attempts, request.remaining_attempts);
            assert!(forwarded.forwarded(next, from).is_err());
            let mut invalid = forwarded.clone();
            invalid.remaining_attempts += 1;
            assert!(request.validate_forwarded(&invalid, from, next).is_err());
            invalid = forwarded;
            invalid.deadline.0 += Duration::from_secs(1);
            assert!(request.validate_forwarded(&invalid, from, next).is_err());
        }
        #[test]
        fn rejects_unknown_and_empty_membership() {
            let empty = membership(0);
            assert!(empty.neighbors(&NodeId("unknown".into())).is_err());
            let one = membership(1);
            assert!(one.neighbors(&one.members()[0].node).unwrap().is_empty());
        }

        #[test]
        fn yielded_search_rechecks_health_and_retains_only_current_neighbors() {
            let members = membership(100_000);
            let source = &members.members()[0].node;
            let health = Rc::new(LinkHealth);
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
            use crate::peer::adaptive::AdaptivePeers;
            use crate::peer::adaptive::Config;
            use crate::peer::adaptive::Outcome;
            let clock = uring_runtime::environment::SimulationClock::new(110);
            let _env = clock.environment(0).enter();
            let members = membership(100_000);
            let source = &members.members()[0].node;
            let admission = AdaptivePeers::new(Config::default(), Default::default()).unwrap();
            let paths = Paths::new(Rc::new(LinkHealth), 1).with_peer_admission(admission.clone());
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
                let paths = Paths::new(Rc::new(LinkHealth), 1);
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
                fresh.membership = crate::model::MembershipVersion(2);
                assert_eq!(
                    paths.shortest(members.clone(), source, &fresh).unwrap_err(),
                    Error::IncompatibleMembership
                );
            }
        }
    }
}
