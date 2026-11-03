//! Mesh v2 measurements: peers per node, route lengths, RDMA edges and churn.
//!
//! These work on any [`topology::Membership`] and run the production route code
//! ([`topology::Paths`]), so the same numbers come out of a full node run
//! ([`crate::node::NodeWorld`] reports them under `mesh`) and of the fast
//! topology-only scenarios here (`--scenario mesh`, `--scenario mesh-selector`),
//! which build a membership of up to 100k members without simulating traffic.
//! See `designs/racer-mesh-v2.md`, section "Simulator".

use std::collections::HashMap;
use std::future::Future;
use std::num::NonZeroU32;
use std::task::{Context, Poll, Waker};

use serde_json::{Value, json};
use topology::{Member, Membership, PathQuery, Paths, PoolSpec, RDMA_PEERS};

use racer_core::time::SplitMix64;

use crate::engine::{Role, Streams};
use crate::report::Histogram;

/// Links a normal (failure free) route may take inside one pool.
pub const NORMAL_LINKS: usize = 2;

/// Route search limit for surveys: the mesh fallback is 3 links.
const SURVEY_LINKS: u8 = 3;

/// Peers per node over a membership's union graph.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Degrees {
    pub min: usize,
    pub mean: f64,
    pub max: usize,
    pub members: usize,
}

impl Degrees {
    pub fn to_json(&self) -> Value {
        json!({
            "members": self.members,
            "min": self.min,
            "mean": (self.mean * 10.0).round() / 10.0,
            "max": self.max,
        })
    }
}

/// Peers per node of every member of `members` (all members when `None`).
pub fn degrees<M: Member>(membership: &Membership<M>, members: Option<&[usize]>) -> Degrees {
    let all: Vec<usize>;
    let positions = match members {
        Some(p) => p,
        None => {
            all = (0..membership.members().len()).collect();
            &all
        }
    };
    if positions.is_empty() {
        return Degrees::default();
    }
    let (mut min, mut max, mut sum) = (usize::MAX, 0, 0usize);
    for &p in positions {
        let d = membership.degree(p);
        min = min.min(d);
        max = max.max(d);
        sum += d;
    }
    Degrees {
        min,
        mean: sum as f64 / positions.len() as f64,
        max,
        members: positions.len(),
    }
}

/// A member ID as a number: IDs of up to 8 bytes exactly, longer ones hashed (FNV-1a).
fn id_key(id: &[u8]) -> u64 {
    if id.len() <= 8 {
        let mut bytes = [0u8; 8];
        bytes[8 - id.len()..].copy_from_slice(id);
        return u64::from_be_bytes(bytes) ^ ((id.len() as u64) << 60);
    }
    id.iter().fold(0xCBF2_9CE4_8422_2325, |h, b| {
        (h ^ u64::from(*b)).wrapping_mul(0x0000_0100_0000_01B3)
    })
}

/// Neighbor IDs of every member, sorted, keyed by member ID.
fn neighbor_ids<M: Member>(membership: &Membership<M>) -> HashMap<u64, Vec<u64>> {
    let key = |p: usize| id_key(membership.members()[p].id());
    (0..membership.members().len())
        .map(|p| {
            let mut ids: Vec<u64> = membership.neighbors(p).into_iter().map(key).collect();
            ids.sort_unstable();
            (key(p), ids)
        })
        .collect()
}

/// Edges added plus edges removed from `old` to `new`, matching members by ID. An edge to a member
/// that joined or left counts too.
pub fn edges_changed<M: Member>(old: &Membership<M>, new: &Membership<M>) -> u64 {
    let (before, after) = (neighbor_ids(old), neighbor_ids(new));
    let empty = Vec::new();
    let mut ends = 0u64;
    for id in before
        .keys()
        .chain(after.keys().filter(|k| !before.contains_key(*k)))
    {
        let a = before.get(id).unwrap_or(&empty);
        let b = after.get(id).unwrap_or(&empty);
        ends += sorted_difference(a, b) + sorted_difference(b, a);
    }
    // Every changed edge has two ends.
    ends / 2
}

/// Items of sorted `a` missing from sorted `b`.
fn sorted_difference<T: Ord>(a: &[T], b: &[T]) -> u64 {
    let (mut i, mut j, mut n) = (0, 0, 0);
    while i < a.len() {
        match b.get(j).map(|y| a[i].cmp(y)) {
            Some(std::cmp::Ordering::Equal) => {
                i += 1;
                j += 1;
            }
            Some(std::cmp::Ordering::Greater) => j += 1,
            _ => {
                n += 1;
                i += 1;
            }
        }
    }
    n
}

/// Poll a future to completion with a no-op waker. Mesh route searches never wait on I/O.
pub fn block_on<F: Future>(future: F) -> F::Output {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(Waker::noop());
    loop {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
    }
}

/// RDMA eligibility from a Site per member position: same Site on both ends. The simulator has one
/// rail per node, so a shared Site means a shared rail.
pub struct Rdma<'a> {
    pub site: &'a dyn Fn(usize) -> u32,
    peers: HashMap<usize, Vec<usize>>,
}

impl<'a> Rdma<'a> {
    pub fn new(site: &'a dyn Fn(usize) -> u32) -> Self {
        Self {
            site,
            peers: HashMap::new(),
        }
    }

    fn peers<M: Member>(&mut self, membership: &Membership<M>, p: usize) -> &[usize] {
        let site = self.site;
        self.peers
            .entry(p)
            .or_insert_with(|| membership.rdma_peers(p, RDMA_PEERS, |a, b| site(a) == site(b)))
    }

    /// Whether the edge `a - b` uses RDMA (production `Membership::is_rdma_edge`, cached).
    pub fn is_rdma<M: Member>(&mut self, membership: &Membership<M>, a: usize, b: usize) -> bool {
        (self.site)(a) == (self.site)(b)
            && membership.is_neighbor(a, b)
            && self.peers(membership, a).contains(&b)
            && self.peers(membership, b).contains(&a)
    }
}

/// Route lengths over sampled reader and owner pairs, split by transport.
#[derive(Debug, Default)]
pub struct Survey {
    pub routes: u64,
    pub links: Histogram,
    pub rdma_links: u64,
    pub tcp_links: u64,
    /// Routes longer than [`NORMAL_LINKS`].
    pub long: u64,
    /// Routes that passed through a member outside the pool.
    pub outside: u64,
    pub unreachable: u64,
}

impl Survey {
    pub fn to_json(&self) -> Value {
        let links = self.rdma_links + self.tcp_links;
        json!({
            "routes": self.routes,
            "links": self.links.to_json(),
            "links_rdma": self.rdma_links,
            "links_tcp": self.tcp_links,
            "rdma_fraction": if links == 0 { 0.0 } else { self.rdma_links as f64 / links as f64 },
            "longer_than_normal": self.long,
            "outside_pool": self.outside,
            "unreachable": self.unreachable,
        })
    }
}

/// Route `samples` random pairs of `pool` members (all members when `None`) with the production
/// route search and no failures, and measure them.
pub fn survey<M: Member>(
    membership: &Membership<M>,
    pool: Option<&[usize]>,
    samples: u64,
    rng: &mut SplitMix64,
    rdma: &mut Rdma<'_>,
) -> Survey {
    let all: Vec<usize>;
    let members = match pool {
        Some(p) => p,
        None => {
            all = (0..membership.members().len()).collect();
            &all
        }
    };
    let mut out = Survey::default();
    if members.len() < 2 {
        return out;
    }
    let paths = Paths::new(1 << 12);
    let pick = |rng: &mut SplitMix64| members[(rng.next_u64() % members.len() as u64) as usize];
    for i in 0..samples {
        let from = pick(rng);
        let mut to = pick(rng);
        while to == from {
            to = pick(rng);
        }
        let seed = i.to_be_bytes();
        let query = PathQuery {
            from,
            to,
            links: SURVEY_LINKS,
            visited: &[],
            blocked: &[],
            seed: &seed,
        };
        let Ok(route) = block_on(paths.route(membership, query)) else {
            out.unreachable += 1;
            continue;
        };
        out.routes += 1;
        let links = route.len() - 1;
        out.links.record(links as u64);
        out.long += u64::from(links > NORMAL_LINKS);
        if route.iter().any(|p| members.binary_search(p).is_err()) {
            out.outside += 1;
        }
        for hop in route.windows(2) {
            if rdma.is_rdma(membership, hop[0], hop[1]) {
                out.rdma_links += 1;
            } else {
                out.tcp_links += 1;
            }
        }
    }
    out
}

/// A member for topology-only scenarios: an ID and a Site.
#[derive(Clone, Debug)]
pub struct MeshMember {
    id: [u8; 4],
    site: Vec<u8>,
}

impl MeshMember {
    pub fn new(node: u32, site: u32) -> Self {
        Self {
            id: node.to_be_bytes(),
            site: format!("site-{site}").into_bytes(),
        }
    }
}

impl Member for MeshMember {
    const DOMAIN: &'static str = "racer-sim";

    fn id(&self) -> &[u8] {
        &self.id
    }

    fn weight(&self) -> NonZeroU32 {
        NonZeroU32::MIN
    }

    fn site(&self) -> &[u8] {
        &self.site
    }
}

/// A membership change in a topology-only scenario.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Change {
    None,
    /// A new node with this ID joins.
    Join(u32),
    /// This node leaves.
    Leave(u32),
}

/// Inputs of a topology-only mesh scenario.
#[derive(Clone, Debug)]
pub struct MeshScenario {
    /// Cluster size.
    pub nodes: u32,
    /// Sites; node `n` is in Site `n % sites`.
    pub sites: u32,
    /// A selector pool of this many members (every `nodes / selector`-th node) besides the default
    /// pool, or 0 for the default pool only.
    pub selector: u32,
    /// Sampled routes per pool.
    pub samples: u64,
    /// Joins and leaves measured for churn, each from the full cluster. A leave from a power of
    /// four (and a join to one) changes the column count, so most edges move.
    pub churn: u32,
    pub seed: u64,
}

impl MeshScenario {
    pub fn new(nodes: u32, seed: u64) -> Self {
        Self {
            nodes,
            sites: 1,
            selector: 0,
            samples: 4096,
            churn: 8,
            seed,
        }
    }

    fn members(&self, change: Change) -> Vec<MeshMember> {
        let joined = match change {
            Change::Join(n) => Some(n),
            _ => None,
        };
        (0..self.nodes)
            .chain(joined)
            .filter(|n| change != Change::Leave(*n))
            .map(|n| MeshMember::new(n, n % self.sites.max(1)))
            .collect()
    }

    /// Node IDs of the selector pool.
    pub fn selector_nodes(&self) -> Vec<u32> {
        if self.selector == 0 {
            return Vec::new();
        }
        let stride = (self.nodes / self.selector).max(1);
        (0..self.selector)
            .map(|i| i * stride)
            .filter(|n| *n < self.nodes)
            .collect()
    }

    fn pools(&self, change: Change) -> Vec<PoolSpec> {
        let mut pools = vec![PoolSpec::default_pool()];
        if self.selector > 0 {
            pools.push(PoolSpec {
                id: (*b"sel-sim").into(),
                rank: 1 << 32,
                members: Some(
                    self.selector_nodes()
                        .into_iter()
                        .filter(|n| change != Change::Leave(*n))
                        .map(|n| n.to_be_bytes().to_vec().into_boxed_slice())
                        .collect(),
                ),
            });
        }
        pools
    }

    /// The membership of every node, after `change`.
    pub fn membership(&self, change: Change) -> Membership<MeshMember> {
        Membership::with_pools(self.members(change), self.pools(change)).expect("valid pools")
    }

    /// Run: degrees, sampled routes per pool, and edges changed by single joins and leaves.
    pub fn run(&self) -> Value {
        let streams = Streams::new(self.seed);
        let membership = self.membership(Change::None);
        let sites = self.sites.max(1);
        let site = |p: usize| p as u32 % sites;
        let mut rdma = Rdma::new(&site);
        let mut pools = Vec::new();
        for (i, pool) in membership.pools().iter().enumerate() {
            let members = pool.members();
            let mut rng = streams.stream(Role::Mesh, i as u64);
            let survey = survey(
                &membership,
                Some(members),
                self.samples,
                &mut rng,
                &mut rdma,
            );
            let (columns, size) = pool.target();
            pools.push(json!({
                "id": String::from_utf8_lossy(pool.id()),
                "members": members.len(),
                "columns": pool.columns(),
                "target_columns": columns,
                "target_column_size": size,
                "degree": degrees(&membership, Some(members)).to_json(),
                "pool_degree": pool_degrees(pool, members).to_json(),
                "routes": survey.to_json(),
            }));
        }
        let (mut joins, mut leaves) = (Histogram::default(), Histogram::default());
        let mut rng = streams.stream(Role::Mesh, u64::MAX);
        for i in 0..self.churn {
            // A leave from `nodes` and a join to `nodes + 1`; a join and the matching leave change
            // the same edges.
            let node = (rng.next_u64() % u64::from(self.nodes)) as u32;
            let left = self.membership(Change::Leave(node));
            leaves.record(edges_changed(&membership, &left));
            let joined = self.membership(Change::Join(self.nodes + i));
            joins.record(edges_changed(&membership, &joined));
        }
        json!({
            "scenario": if self.selector > 0 { "mesh-selector" } else { "mesh" },
            "nodes": self.nodes,
            "sites": sites,
            "seed": self.seed,
            "degree": degrees(&membership, None).to_json(),
            "pools": pools,
            "edges_changed_per_join": joins.to_json(),
            "edges_changed_per_leave": leaves.to_json(),
            "identity": hex(&membership.topology_identity()),
        })
    }
}

/// Peers per node inside one pool's own mesh.
pub fn pool_degrees(pool: &topology::Pool, members: &[usize]) -> Degrees {
    if members.is_empty() {
        return Degrees::default();
    }
    let (mut min, mut max, mut sum) = (usize::MAX, 0, 0usize);
    for &p in members {
        let d = pool.neighbors(p).len();
        min = min.min(d);
        max = max.max(d);
        sum += d;
    }
    Degrees {
        min,
        mean: sum as f64 / members.len() as f64,
        max,
        members: members.len(),
    }
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sorted_difference_counts_missing_items() {
        assert_eq!(sorted_difference(&[1, 2, 3, 5], &[2, 4, 5]), 2);
        assert_eq!(sorted_difference::<u8>(&[], &[1]), 0);
        assert_eq!(sorted_difference(&[1], &[]), 1);
    }

    #[test]
    fn full_mesh_degrees_and_routes() {
        let scenario = MeshScenario {
            samples: 256,
            churn: 2,
            ..MeshScenario::new(64, 1)
        };
        let report = scenario.run();
        assert_eq!(report["degree"]["min"], 63, "{report}");
        assert_eq!(report["degree"]["max"], 63, "{report}");
        assert_eq!(report["pools"][0]["routes"]["links"]["max"], 1, "{report}");
        // A leave in a full mesh removes exactly the member's own edges.
        assert_eq!(report["edges_changed_per_leave"]["max"], 63, "{report}");
        // A join to 65 members splits the full mesh into columns.
        assert!(
            report["edges_changed_per_join"]["max"].as_u64() > Some(63),
            "{report}"
        );
    }

    #[test]
    fn identical_inputs_give_identical_meshes() {
        let scenario = MeshScenario::new(300, 1);
        let (a, b) = (
            scenario.membership(Change::None),
            scenario.membership(Change::None),
        );
        assert_eq!(edges_changed(&a, &b), 0);
        assert_eq!(a.topology_identity(), b.topology_identity());
    }

    #[test]
    fn rdma_needs_a_shared_site() {
        let scenario = MeshScenario {
            sites: 2,
            ..MeshScenario::new(40, 1)
        };
        let membership = scenario.membership(Change::None);
        let site = |p: usize| p as u32 % 2;
        let mut rdma = Rdma::new(&site);
        assert!(rdma.is_rdma(&membership, 0, 2));
        assert!(!rdma.is_rdma(&membership, 0, 1));
    }
}
