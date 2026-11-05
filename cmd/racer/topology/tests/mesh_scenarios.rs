//! Mesh v2 measurements at scale (`designs/racer-mesh-v2.md`): peers per node, route lengths,
//! RDMA links and edges changed by churn, measured on the production mesh and route search
//! ([`Paths`]) for memberships of up to 20k members without traffic.
//!
//! Moved from racer-sim (`sim/tests/mesh_scenarios.rs` and `sim/src/mesh.rs`) with the same
//! assertions. The hot page storm with mesh helpers is `model-hot-page-storm-512` in racer-dst.

use std::collections::HashMap;
use std::num::NonZeroU32;

use futures::executor::block_on;
use topology::{Member, Membership, PathQuery, Paths, Pool, PoolSpec, RDMA_PEERS, target_columns};

/// Links a normal (failure free) route may take inside one pool.
const NORMAL_LINKS: usize = 2;

/// Route search limit for surveys: the mesh fallback is 3 links.
const SURVEY_LINKS: u8 = 3;

/// SplitMix64, a small seeded generator for sampling pairs.
struct Rng(u64);

impl Rng {
    /// Next 64 random bits.
    fn next_u64(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9E37_79B9_7F4A_7C15);
        let mut z = self.0;
        z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
        z ^ (z >> 31)
    }
}

/// Minimum, mean and maximum of a set of counts.
#[derive(Clone, Copy, Debug, Default)]
struct Stats {
    count: u64,
    min: u64,
    mean: f64,
    max: u64,
}

impl Stats {
    /// Summarize `values`.
    fn of(values: impl IntoIterator<Item = u64>) -> Self {
        let (mut count, mut min, mut max, mut sum) = (0u64, u64::MAX, 0u64, 0u64);
        for v in values {
            count += 1;
            min = min.min(v);
            max = max.max(v);
            sum += v;
        }
        if count == 0 {
            return Self::default();
        }
        Self {
            count,
            min,
            mean: sum as f64 / count as f64,
            max,
        }
    }
}

/// A member: an ID and a Site.
#[derive(Clone, Debug)]
struct MeshMember {
    id: [u8; 4],

    site: Vec<u8>,
}

impl MeshMember {
    /// Node `node` in Site `site`.
    fn new(node: u32, site: u32) -> Self {
        Self {
            id: node.to_be_bytes(),
            site: format!("site-{site}").into_bytes(),
        }
    }
}

impl Member for MeshMember {
    const DOMAIN: &'static str = "racer-sim";

    /// Return the ID.
    fn id(&self) -> &[u8] {
        &self.id
    }

    /// Every member has weight 1.
    fn weight(&self) -> NonZeroU32 {
        NonZeroU32::MIN
    }

    /// Return the Site.
    fn site(&self) -> &[u8] {
        &self.site
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

/// Edges added plus edges removed from `old` to `new`, matching members by ID. An edge to a member
/// that joined or left counts too.
fn edges_changed<M: Member>(old: &Membership<M>, new: &Membership<M>) -> u64 {
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

/// Edges changed from `old` to `new` and members that joined or left.
fn churn<M: Member>(old: &Membership<M>, new: &Membership<M>) -> (u64, u64) {
    let ids = |m: &Membership<M>| {
        let mut ids: Vec<u64> = m.members().iter().map(|x| id_key(x.id())).collect();
        ids.sort_unstable();
        ids
    };
    let (a, b) = (ids(old), ids(new));
    let members = sorted_difference(&a, &b) + sorted_difference(&b, &a);
    (edges_changed(old, new), members)
}

/// RDMA eligibility: same Site on both ends (one rail per node), and production
/// `Membership::rdma_peers` on both ends, cached.
struct Rdma {
    sites: u32,

    peers: HashMap<usize, Vec<usize>>,
}

impl Rdma {
    /// Node `p` is in Site `p % sites`.
    fn new(sites: u32) -> Self {
        Self {
            sites,
            peers: HashMap::new(),
        }
    }

    /// Site of position `p`.
    fn site(&self, p: usize) -> u32 {
        p as u32 % self.sites
    }

    /// Whether the edge `a - b` uses RDMA.
    fn is_rdma<M: Member>(&mut self, membership: &Membership<M>, a: usize, b: usize) -> bool {
        if self.site(a) != self.site(b) || !membership.is_neighbor(a, b) {
            return false;
        }
        let sites = self.sites;
        let mut peers = |p: usize| {
            self.peers
                .entry(p)
                .or_insert_with(|| {
                    membership
                        .rdma_peers(p, RDMA_PEERS, |x, y| x as u32 % sites == y as u32 % sites)
                })
                .contains(&if p == a { b } else { a })
        };
        peers(a) && peers(b)
    }
}

/// Route lengths over sampled member pairs of one pool, split by transport.
#[derive(Debug, Default)]
struct Survey {
    routes: u64,

    links: Vec<u64>,

    rdma_links: u64,

    tcp_links: u64,

    /// Routes longer than [`NORMAL_LINKS`].
    long: u64,

    /// Routes that passed through a member outside the pool.
    outside: u64,

    unreachable: u64,
}

impl Survey {
    /// Longest sampled route in links.
    fn max_links(&self) -> u64 {
        self.links.iter().copied().max().unwrap_or(0)
    }

    /// Share of links that use RDMA.
    fn rdma_fraction(&self) -> f64 {
        let links = self.rdma_links + self.tcp_links;
        if links == 0 {
            0.0
        } else {
            self.rdma_links as f64 / links as f64
        }
    }
}

/// Route `samples` random pairs of `members` with the production route search and no failures.
fn survey<M: Member>(
    membership: &Membership<M>,
    members: &[usize],
    samples: u64,
    rng: &mut Rng,
    rdma: &mut Rdma,
) -> Survey {
    let mut out = Survey::default();
    if members.len() < 2 {
        return out;
    }
    let paths = Paths::new(1 << 12);
    let pick = |rng: &mut Rng| members[(rng.next_u64() % members.len() as u64) as usize];
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
        out.links.push(links as u64);
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

/// A membership change.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Change {
    None,

    /// A new node with this ID joins.
    Join(u32),

    /// This node leaves.
    Leave(u32),
}

/// One pool's measurements.
struct PoolReport {
    members: usize,

    degree: Stats,

    pool_degree: Stats,

    routes: Survey,
}

/// A scenario's measurements.
struct Report {
    degree: Stats,

    pools: Vec<PoolReport>,

    per_join: Stats,

    per_leave: Stats,
}

/// Inputs of a topology-only mesh scenario.
#[derive(Clone, Debug)]
struct MeshScenario {
    nodes: u32,

    /// Sites; node `n` is in Site `n % sites`.
    sites: u32,

    /// A selector pool of this many members (every `nodes / selector`-th node) besides the default
    /// pool, or 0 for the default pool only.
    selector: u32,

    /// Sampled routes per pool.
    samples: u64,

    /// Joins and leaves measured for churn, each from the full cluster.
    churn: u32,

    seed: u64,
}

impl MeshScenario {
    /// One Site, so every pool is laid out like the spec table.
    fn new(nodes: u32, samples: u64, churn: u32) -> Self {
        Self {
            nodes,
            sites: 1,
            selector: 0,
            samples,
            churn,
            seed: 1,
        }
    }

    /// Members after `change`, in node order.
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
    fn selector_nodes(&self) -> Vec<u32> {
        if self.selector == 0 {
            return Vec::new();
        }
        let stride = (self.nodes / self.selector).max(1);
        (0..self.selector)
            .map(|i| i * stride)
            .filter(|n| *n < self.nodes)
            .collect()
    }

    /// Pool specs after `change`.
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
    fn membership(&self, change: Change) -> Membership<MeshMember> {
        Membership::with_pools(self.members(change), self.pools(change)).expect("valid pools")
    }

    /// Degrees, sampled routes per pool, and edges changed by single joins and leaves.
    fn run(&self) -> Report {
        let membership = self.membership(Change::None);
        let mut rdma = Rdma::new(self.sites.max(1));
        let all = membership.members().len();
        let degree = Stats::of((0..all).map(|p| membership.degree(p) as u64));
        let mut pools = Vec::new();
        for (i, pool) in membership.pools().iter().enumerate() {
            let members = pool.members();
            let mut rng = Rng(self.seed ^ (i as u64).wrapping_mul(0x1000_0000_01B3));
            pools.push(PoolReport {
                members: members.len(),
                degree: Stats::of(members.iter().map(|&p| membership.degree(p) as u64)),
                pool_degree: pool_degrees(pool, members),
                routes: survey(&membership, members, self.samples, &mut rng, &mut rdma),
            });
        }
        let (mut joins, mut leaves) = (Vec::new(), Vec::new());
        let mut rng = Rng(!self.seed);
        for i in 0..self.churn {
            let node = (rng.next_u64() % u64::from(self.nodes)) as u32;
            leaves.push(edges_changed(
                &membership,
                &self.membership(Change::Leave(node)),
            ));
            joins.push(edges_changed(
                &membership,
                &self.membership(Change::Join(self.nodes + i)),
            ));
        }
        Report {
            degree,
            pools,
            per_join: Stats::of(joins),
            per_leave: Stats::of(leaves),
        }
    }
}

/// Peers per node inside one pool's own mesh.
fn pool_degrees(pool: &Pool, members: &[usize]) -> Stats {
    Stats::of(members.iter().map(|&p| pool.neighbors(p).len() as u64))
}

/// Peers per node from the spec: about `T - 1` column edges and `C - 1` delegate edges each way.
fn expected_degree(k: usize) -> f64 {
    let (columns, size) = target_columns(k);
    (size - 1 + 2 * (columns - 1)) as f64
}

/// Every sampled normal route stays within 2 links, and the mean peers per node is near the spec.
fn assert_default_pool(nodes: u32, samples: u64, table: Option<f64>) {
    let report = MeshScenario::new(nodes, samples, 0).run();
    let routes = &report.pools[0].routes;
    assert_eq!(routes.routes, samples, "{routes:?}");
    assert_eq!(routes.unreachable, 0);
    assert_eq!(routes.long, 0);
    assert!(routes.max_links() <= 2);
    let mean = report.degree.mean;
    let expected = table.unwrap_or_else(|| expected_degree(nodes as usize));
    assert!(
        (mean - expected).abs() <= expected * 0.03,
        "{nodes} nodes: {mean} peers per node, spec {expected}: {:?}",
        report.degree
    );
    // No member is far from the mean: column sizes stay within about T / 2 and 3T / 2.
    assert!(
        report.degree.max as f64 <= expected * 1.7,
        "{:?}",
        report.degree
    );
}

#[test]
fn sorted_difference_counts_missing_items() {
    assert_eq!(sorted_difference(&[1, 2, 3, 5], &[2, 4, 5]), 2);
    assert_eq!(sorted_difference::<u8>(&[], &[1]), 0);
    assert_eq!(sorted_difference(&[1], &[]), 1);
}

#[test]
fn full_mesh_degrees_and_routes() {
    let report = MeshScenario::new(64, 256, 2).run();
    assert_eq!(report.degree.min, 63);
    assert_eq!(report.degree.max, 63);
    assert_eq!(report.pools[0].routes.max_links(), 1);
    // A leave in a full mesh removes exactly the member's own edges.
    assert_eq!(report.per_leave.max, 63);
    // A join to 65 members splits the full mesh into columns.
    assert!(report.per_join.max > 63, "{:?}", report.per_join);
}

#[test]
fn identical_inputs_give_identical_meshes() {
    let scenario = MeshScenario::new(300, 0, 0);
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
        ..MeshScenario::new(40, 0, 0)
    };
    let membership = scenario.membership(Change::None);
    let mut rdma = Rdma::new(2);
    assert!(rdma.is_rdma(&membership, 0, 2));
    assert!(!rdma.is_rdma(&membership, 0, 1));
}

/// The spec's table of peers per node (one Site).
#[test]
fn degree_matches_the_spec_table() {
    for (nodes, table) in [(64, 63.0), (256, 45.0), (1024, 93.0)] {
        assert_default_pool(nodes, 1024, Some(table));
    }
}

#[test]
fn default_pool_of_512_routes_in_two_links() {
    assert_default_pool(512, 4096, None);
}

#[test]
fn default_pool_of_4k_routes_in_two_links() {
    assert_default_pool(4096, 4096, Some(189.0));
}

#[test]
fn default_pool_of_20k_routes_in_two_links() {
    assert_default_pool(20_000, 2048, Some(410.0));
}

/// A 256 member selector pool inside a 20k cluster: reads between its members stay within the
/// pool and take at most 2 links, and its peers per node inside the pool match a 256 member pool.
/// Selector pools rank first for RDMA, so with one Site nearly all its links are RDMA.
#[test]
fn selector_pool_in_a_20k_cluster_stays_in_the_pool() {
    let report = MeshScenario {
        selector: 256,
        ..MeshScenario::new(20_000, 2048, 0)
    }
    .run();
    let pool = &report.pools[1];
    assert_eq!(pool.members, 256);
    assert!(pool.degree.count == 256);
    let routes = &pool.routes;
    assert_eq!(routes.routes, 2048);
    assert_eq!(routes.outside, 0);
    assert_eq!(routes.long, 0);
    assert!(routes.max_links() <= 2);
    let mean = pool.pool_degree.mean;
    assert!((mean - 45.0).abs() <= 45.0 * 0.03, "{:?}", pool.pool_degree);
    assert!(routes.rdma_fraction() > 0.95, "{routes:?}");
}

/// A join or leave changes O(sqrt(k)) edges: the member's own edges (about `T + 2C`) plus the
/// delegate edges whose ring arc it takes or gives back.
#[test]
fn churn_changes_few_edges() {
    for nodes in [512u32, 4000] {
        let report = MeshScenario::new(nodes, 0, 6).run();
        let bound = 6.0 * f64::from(nodes).sqrt();
        for (kind, stats) in [("join", report.per_join), ("leave", report.per_leave)] {
            let max = stats.max as f64;
            assert!(max <= bound, "{nodes} nodes {kind}: {max} over {bound}");
            // At least the member's own edges change.
            assert!(
                max >= expected_degree(nodes as usize) * 0.5,
                "{nodes} nodes {kind}: {max}"
            );
        }
    }
}

/// The 20k form of [`churn_changes_few_edges`].
#[test]
fn churn_changes_few_edges_at_20k() {
    let report = MeshScenario::new(20_000, 0, 4).run();
    let bound = 6.0 * 20_000f64.sqrt();
    for stats in [report.per_join, report.per_leave] {
        assert!(stats.max as f64 <= bound, "{stats:?}");
    }
}

/// Crossing a power of four changes the column count, so most edges move. This is rare (pool sizes
/// 256, 1024, 4096, ...), and the spec accepts it.
#[test]
fn crossing_a_power_of_four_rebuilds_the_mesh() {
    let s = MeshScenario::new(256, 0, 0);
    let full = s.membership(Change::None);
    let left = s.membership(Change::Leave(7));
    let (edges, members) = churn(&full, &left);
    assert_eq!(members, 1);
    assert!(edges > 10 * 45, "{edges}");
}

/// A primary that takes over after a failover, and every node, computes the same mesh from the
/// same publication: member order, a predecessor chain, and pools listed in another order do not
/// matter.
#[test]
fn failover_rebuild_gives_the_same_mesh() {
    let s = MeshScenario {
        selector: 128,
        ..MeshScenario::new(3000, 0, 0)
    };
    let primary = s.membership(Change::None);
    // The new primary lists members in another order and pools the other way around.
    let mut members = s.members(Change::None);
    members.reverse();
    let mut pools = s.pools(Change::None);
    pools.reverse();
    let rebuilt = Membership::with_pools(members, pools).unwrap();
    assert_eq!(edges_changed(&primary, &rebuilt), 0);
    assert_eq!(primary.topology_identity(), rebuilt.topology_identity());
    // A node that applied the previous publication first reaches the same mesh.
    let before = s.membership(Change::Leave(11));
    let chained = Membership::with_pools_and_predecessor(
        s.members(Change::None),
        s.pools(Change::None),
        &before,
    )
    .unwrap();
    assert_eq!(edges_changed(&primary, &chained), 0);
    assert_eq!(primary.topology_identity(), chained.topology_identity());
}
