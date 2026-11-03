//! Mesh layout tests through the public API (`designs/racer-mesh-v2.md`).

use std::{collections::BTreeSet, num::NonZeroU32};

use futures::executor::block_on;

use topology::{
    EdgeKind, Error, FULL_MESH_MAX, Member, Membership, PathQuery, Paths, PoolSpec, RDMA_PEERS,
};

/// Test member with a Site.
#[derive(Clone, Debug)]
struct Node {
    id: Vec<u8>,

    site: Vec<u8>,
}

impl Member for Node {
    const DOMAIN: &'static str = "mesh-tests";

    /// Return the ID.
    fn id(&self) -> &[u8] {
        &self.id
    }

    /// Every member has weight 1.
    fn weight(&self) -> NonZeroU32 {
        NonZeroU32::new(1).unwrap()
    }

    /// Return the Site.
    fn site(&self) -> &[u8] {
        &self.site
    }
}

/// Ways to spread members over Sites.
#[derive(Clone, Copy, Debug)]
enum Sites {
    /// Every member in Site `a`.
    One,

    /// Two equal large Sites.
    TwoLarge,

    /// Sites of 3 members each.
    ManyTiny,

    /// Every member in the empty Site.
    Empty,

    /// 70% in one Site, the rest in Sites of 2.
    Mixed,
}

/// The Site of member `i` of `k`.
fn site(sites: Sites, i: usize, k: usize) -> Vec<u8> {
    match sites {
        Sites::One => b"a".to_vec(),
        Sites::TwoLarge => if i.is_multiple_of(2) { b"x" } else { b"y" }.to_vec(),
        Sites::ManyTiny => format!("tiny-{}", i / 3).into_bytes(),
        Sites::Empty => Vec::new(),
        Sites::Mixed if i * 10 < k * 7 => b"big".to_vec(),
        Sites::Mixed => format!("small-{}", i / 2).into_bytes(),
    }
}

/// `k` members with IDs `node-i`.
fn nodes(k: usize, sites: Sites) -> Vec<Node> {
    (0..k)
        .map(|i| Node {
            id: format!("node-{i:06}").into_bytes(),
            site: site(sites, i, k),
        })
        .collect()
}

/// Every member's neighbor list.
fn adjacency(members: &Membership<Node>) -> Vec<Vec<usize>> {
    (0..members.members().len())
        .map(|p| members.neighbors(p))
        .collect()
}

/// All edges as sorted ID pairs, comparable across memberships.
fn edges(members: &Membership<Node>) -> BTreeSet<(Vec<u8>, Vec<u8>)> {
    let ids = |p: usize| members.members()[p].id.clone();
    let mut out = BTreeSet::new();
    for (a, list) in adjacency(members).into_iter().enumerate() {
        for b in list.into_iter().filter(|&b| a < b) {
            out.insert((ids(a), ids(b)));
        }
    }
    out
}

/// Members within 2 links of `source`, by BFS over `graph`.
fn within_two(graph: &[Vec<usize>], source: usize) -> usize {
    let mut seen = vec![false; graph.len()];
    seen[source] = true;
    for &a in &graph[source] {
        seen[a] = true;
        for &b in &graph[a] {
            seen[b] = true;
        }
    }
    seen.into_iter().filter(|&s| s).count()
}

/// Every pool has diameter at most 2, the graph is symmetric, and the
/// primary and alternate paths are disjoint 2-link paths.
#[test]
fn diameter_two_symmetric_and_disjoint_paths_for_every_site_mix() {
    let mixes = [
        Sites::One,
        Sites::TwoLarge,
        Sites::ManyTiny,
        Sites::Empty,
        Sites::Mixed,
    ];
    for k in [0, 1, 2, 3, 10, 64, 65, 100, 257, 1000, 4096] {
        for sites in mixes {
            let members = Membership::new(nodes(k, sites)).unwrap();
            let graph = adjacency(&members);
            let pool = &members.pools()[0];
            assert_eq!(pool.members().len(), k);
            let bound = members.max_degree();
            for (a, list) in graph.iter().enumerate() {
                assert!(list.windows(2).all(|w| w[0] < w[1]));
                assert!(!list.contains(&a));
                assert!(list.len() <= bound, "k={k} {sites:?}");
                for &b in list {
                    assert!(graph[b].binary_search(&a).is_ok(), "asymmetric");
                    assert!(members.is_neighbor(a, b));
                }
            }
            let step = if k > 1000 { 61 } else { 1 };
            for source in (0..k).step_by(step) {
                assert_eq!(within_two(&graph, source), k, "k={k} {sites:?}");
            }
            // Structural check on a sample of pairs.
            for a in (0..k).step_by(step) {
                for b in (0..k).step_by(7) {
                    if a == b {
                        continue;
                    }
                    if pool.same_column(a, b) {
                        assert_eq!(pool.edge(a, b), Some(EdgeKind::Column));
                        assert_eq!(pool.primary(a, b), None);
                        assert_eq!(pool.helper(a, b), None);
                        continue;
                    }
                    let primary = pool.primary(a, b).unwrap();
                    let alternate = pool.alternate(a, b).unwrap();
                    assert_eq!(pool.helper(a, b), Some(primary));
                    assert_ne!(primary, alternate);
                    assert_eq!(pool.column(primary), pool.column(b));
                    assert_eq!(pool.column(alternate), pool.column(a));
                    for (x, y) in [(a, primary), (primary, b), (a, alternate), (alternate, b)] {
                        assert!(x == y || members.is_neighbor(x, y));
                    }
                }
            }
        }
    }
}

/// Columns of a large Site have only that Site's members, and column sizes
/// stay near the target.
#[test]
fn large_site_columns_are_single_site_and_sized_near_target() {
    for sites in [Sites::TwoLarge, Sites::Mixed] {
        let members = Membership::new(nodes(4000, sites)).unwrap();
        let pool = &members.pools()[0];
        let (_, t) = pool.target();
        for column in 0..pool.columns() {
            let list = pool.column_members(column);
            assert!(
                list.len() * 2 >= t / 2 && list.len() * 2 <= 3 * t + t,
                "{}",
                list.len()
            );
            for &p in &list {
                assert_eq!(pool.column(p), Some(column));
            }
        }
        if let Sites::TwoLarge = sites {
            for column in 0..pool.columns() {
                let found: BTreeSet<_> = pool
                    .column_members(column)
                    .into_iter()
                    .map(|p| members.members()[p].site.clone())
                    .collect();
                assert_eq!(found.len(), 1);
            }
        }
    }
}

/// The mesh does not depend on input order or weights.
#[test]
fn deterministic_under_permutation() {
    for k in [10, 300, 2000] {
        let input = nodes(k, Sites::Mixed);
        let a = Membership::new(input.clone()).unwrap();
        let mut reversed = input.clone();
        reversed.reverse();
        let mut shuffled = input;
        shuffled.rotate_left(k / 3);
        for other in [reversed, shuffled] {
            let b = Membership::new(other).unwrap();
            assert_eq!(adjacency(&a), adjacency(&b));
            assert_eq!(a.topology_identity(), b.topology_identity());
        }
    }
}

/// Mean degree matches the spec table within 25%.
#[test]
fn degree_matches_spec_table() {
    for (k, expected) in [(64usize, 63.0f64), (256, 45.0), (1024, 93.0), (4096, 189.0)] {
        let members = Membership::new(nodes(k, Sites::One)).unwrap();
        let total: usize = (0..k).map(|p| members.degree(p)).sum();
        let mean = total as f64 / k as f64;
        assert!(
            (mean - expected).abs() <= expected * 0.25,
            "k={k} mean={mean}"
        );
        eprintln!("k={k} mean degree {mean}");
        let max = (0..k).map(|p| members.degree(p)).max().unwrap();
        assert!(max <= members.max_degree());
    }
}

/// Pools of up to 64 members are full meshes, whatever their Sites.
#[test]
fn full_mesh_up_to_sixty_four() {
    for k in [2, 17, FULL_MESH_MAX] {
        for sites in [Sites::ManyTiny, Sites::TwoLarge] {
            let members = Membership::new(nodes(k, sites)).unwrap();
            assert_eq!(members.pools()[0].columns(), 1);
            for p in 0..k {
                assert_eq!(members.degree(p), k - 1);
            }
        }
    }
}

/// Routes take at most 2 links with nothing blocked, in every pool.
#[test]
fn routes_never_exceed_two_links_when_nothing_is_blocked() {
    let members = Membership::new(nodes(1000, Sites::Mixed)).unwrap();
    let paths = Paths::new(0);
    for from in (0..1000).step_by(97) {
        for to in (0..1000).step_by(3) {
            let route = block_on(paths.route(
                &members,
                PathQuery {
                    from,
                    to,
                    links: 8,
                    visited: &[],
                    blocked: &[],
                    seed: b"seed",
                },
            ))
            .unwrap();
            assert!(route.len() <= 3);
            if from != to && !members.is_neighbor(from, to) {
                // The primary delegate is preferred.
                let pool = &members.pools()[0];
                assert_eq!(route[1], pool.primary(from, to).unwrap());
            }
        }
    }
}

/// Join or leave of one member changes O(sqrt(k)) edges, away from the
/// sizes where column counts change.
#[test]
fn single_join_or_leave_changes_few_edges() {
    let k = 4000;
    let base = Membership::new(nodes(k, Sites::One)).unwrap();
    let before = edges(&base);
    let bound = 5 * (k as f64).sqrt() as usize;
    let mut joined = nodes(k, Sites::One);
    joined.push(Node {
        id: b"node-joiner".to_vec(),
        site: b"a".to_vec(),
    });
    let joined = Membership::new(joined).unwrap();
    let changed = before.symmetric_difference(&edges(&joined)).count();
    eprintln!("join changed {changed} edges, bound {bound}");
    assert!(changed <= bound, "join changed {changed} edges");
    for leaver in [0usize, 1234, 3999] {
        let mut left = nodes(k, Sites::One);
        left.remove(leaver);
        let left = Membership::new(left).unwrap();
        let changed = before.symmetric_difference(&edges(&left)).count();
        eprintln!("leave changed {changed} edges");
        assert!(changed <= bound, "leave changed {changed} edges");
    }
}

/// Pools with the same members share the mesh; a subset pool with the same
/// column count keeps every column edge of the larger pool.
#[test]
fn overlapping_pools_share_edges() {
    let all = nodes(2000, Sites::One);
    let subset: Vec<Box<[u8]>> = all[..1200]
        .iter()
        .map(|n| n.id.clone().into_boxed_slice())
        .collect();
    let members = Membership::with_pools(
        all,
        vec![
            PoolSpec::default_pool(),
            PoolSpec {
                id: (*b"sel-a").into(),
                rank: 1 << 32,
                members: Some(subset.clone()),
            },
            PoolSpec {
                id: (*b"sel-b").into(),
                rank: 1 << 32,
                members: Some(subset),
            },
        ],
    )
    .unwrap();
    let pools = members.pools();
    assert_eq!(pools.len(), 3);
    let (default, a, b) = (&pools[0], &pools[1], &pools[2]);
    assert_eq!(a.id(), b"sel-a");
    assert_eq!(a.columns(), default.columns());
    for p in a.members().iter().copied().step_by(5) {
        assert_eq!(a.neighbors(p), b.neighbors(p));
        for q in a.neighbors(p) {
            if a.edge(p, q) == Some(EdgeKind::Column) {
                assert_eq!(default.edge(p, q), Some(EdgeKind::Column));
            }
        }
        let mut union = default.neighbors(p);
        union.extend(a.neighbors(p));
        union.sort_unstable();
        union.dedup();
        assert_eq!(members.neighbors(p), union);
    }
    // Members outside the subset have only default pool edges.
    assert_eq!(members.neighbors(1999), default.neighbors(1999));
    assert!(!a.contains(1999));
    assert_eq!(a.column(1999), None);
}

/// RDMA peers are within budget, eligible, symmetric, and prefer edges of
/// higher ranked pools.
#[test]
fn rdma_ranking_is_mutual_bounded_and_prefers_rank() {
    let all = nodes(3000, Sites::TwoLarge);
    let selected: Vec<Box<[u8]>> = all
        .iter()
        .step_by(3)
        .map(|n| n.id.clone().into_boxed_slice())
        .collect();
    let members = Membership::with_pools(
        all,
        vec![
            PoolSpec::default_pool(),
            PoolSpec {
                id: (*b"sel").into(),
                rank: 1 << 32,
                members: Some(selected),
            },
        ],
    )
    .unwrap();
    let same_site = |a: usize, b: usize| members.members()[a].site == members.members()[b].site;
    let selector = members.pool(b"sel").unwrap();
    for budget in [0, 8, 40, RDMA_PEERS] {
        for p in (0..3000).step_by(101) {
            let peers = members.rdma_peers(p, budget, same_site);
            assert!(peers.len() <= budget);
            let unique: BTreeSet<_> = peers.iter().collect();
            assert_eq!(unique.len(), peers.len());
            for &q in &peers {
                assert!(members.is_neighbor(p, q) && same_site(p, q));
                assert_eq!(
                    members.is_rdma_edge(p, q, budget, same_site),
                    members.rdma_peers(q, budget, same_site).contains(&p)
                );
                assert_eq!(
                    members.is_rdma_edge(p, q, budget, same_site),
                    members.is_rdma_edge(q, p, budget, same_site)
                );
            }
            if selector.contains(p) {
                // Selector edges come first.
                let eligible = selector
                    .neighbors(p)
                    .into_iter()
                    .filter(|&q| same_site(p, q))
                    .count();
                let taken = peers
                    .iter()
                    .filter(|&&q| selector.edge(p, q).is_some())
                    .count();
                assert_eq!(taken, eligible.min(budget));
            }
        }
    }
    let none = members.rdma_peers(0, RDMA_PEERS, |_, _| false);
    assert!(none.is_empty());
}

/// Bad pools are rejected; zero pools give no edges.
#[test]
fn invalid_pools_and_zero_pools() {
    let pool = |id: &[u8], members: Option<Vec<&[u8]>>| PoolSpec {
        id: id.into(),
        rank: 0,
        members: members.map(|m| m.into_iter().map(Into::into).collect()),
    };
    let input = || nodes(10, Sites::One);
    assert_eq!(
        Membership::with_pools(input(), vec![pool(b"p", Some(vec![b"missing"]))]).unwrap_err(),
        Error::InvalidPool
    );
    assert_eq!(
        Membership::with_pools(
            input(),
            vec![pool(b"p", Some(vec![b"node-000001", b"node-000001"]))]
        )
        .unwrap_err(),
        Error::InvalidPool
    );
    assert_eq!(
        Membership::with_pools(input(), vec![pool(b"p", None), pool(b"p", None)]).unwrap_err(),
        Error::InvalidPool
    );
    let empty_pool = Membership::with_pools(input(), vec![pool(b"p", Some(vec![]))]).unwrap();
    assert_eq!(empty_pool.pools()[0].columns(), 0);
    let none = Membership::with_pools(nodes(5000, Sites::One), vec![]).unwrap();
    assert!(none.pools().is_empty());
    assert!(none.neighbors(0).is_empty());
    assert!(!none.is_neighbor(0, 1));
    assert_eq!(none.max_degree(), 0);
    assert_eq!(
        block_on(Paths::new(0).route(
            &none,
            PathQuery {
                from: 0,
                to: 1,
                links: 3,
                visited: &[],
                blocked: &[],
                seed: &[],
            }
        )),
        Err(Error::Unreachable)
    );
}

/// Members that share no pool and are not linked are unreachable, even if
/// a 2-link path exists through the union graph.
#[test]
fn no_shared_pool_is_unreachable() {
    let all = nodes(300, Sites::One);
    let ids = |range: std::ops::Range<usize>| {
        Some(
            all[range]
                .iter()
                .map(|n| n.id.clone().into_boxed_slice())
                .collect(),
        )
    };
    let members = Membership::with_pools(
        all.clone(),
        vec![
            PoolSpec {
                id: (*b"left").into(),
                rank: 0,
                members: ids(0..160),
            },
            PoolSpec {
                id: (*b"right").into(),
                rank: 0,
                members: ids(140..300),
            },
        ],
    )
    .unwrap();
    let paths = Paths::new(0);
    let (from, to) = (0, 299);
    assert!(!members.is_neighbor(from, to));
    let query = PathQuery {
        from,
        to,
        links: 3,
        visited: &[],
        blocked: &[],
        seed: &[],
    };
    assert_eq!(
        block_on(paths.route(&members, query)),
        Err(Error::Unreachable)
    );
    // A member in both pools reaches either side in 2 links.
    let route = block_on(paths.route(&members, PathQuery { from: 150, ..query })).unwrap();
    assert!(route.len() <= 3);
}

/// Topology identity covers Sites and pools, not weights or input order.
#[test]
fn topology_identity_covers_sites_and_pools() {
    let base = Membership::new(nodes(100, Sites::One)).unwrap();
    let sites = Membership::new(nodes(100, Sites::TwoLarge)).unwrap();
    assert_ne!(base.topology_identity(), sites.topology_identity());
    assert_eq!(base.identity(), sites.identity());
    let ranked = Membership::with_pools(
        nodes(100, Sites::One),
        vec![PoolSpec {
            rank: 7,
            ..PoolSpec::default_pool()
        }],
    )
    .unwrap();
    assert_ne!(base.topology_identity(), ranked.topology_identity());
    assert_eq!(adjacency(&base), adjacency(&ranked));
}

/// Building a 100,000 member default pool is fast enough to do on each
/// publication. Run with `cargo test --release -- --ignored`.
#[test]
#[ignore = "timing check; run in release"]
fn hundred_thousand_member_build_time() {
    let input = nodes(100_000, Sites::Mixed);
    let start = std::time::Instant::now();
    let members = Membership::new(input).unwrap();
    let elapsed = start.elapsed();
    let bytes = members.retained_bytes();
    eprintln!("build {elapsed:?}, retained {} MiB", bytes >> 20);
    assert!(elapsed < std::time::Duration::from_secs(10));
    assert!(bytes < 64 << 20);
    let start = std::time::Instant::now();
    let degree = members.degree(12_345);
    eprintln!("degree {degree} in {:?}", start.elapsed());
    assert!(degree <= members.max_degree());
}
