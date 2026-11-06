//! Mesh v2 scenarios (`designs/racer-mesh-v2.md`, "Simulator").
//!
//! Most run the topology-only [`MeshScenario`]: the production mesh and route search on a
//! membership of any size, without traffic. The same runs are available from the command line:
//! `make racer-sim ARGS='--scenario mesh --nodes 20000 --config sites1.json'` (and
//! `mesh-selector`), where `sites1.json` is `{"sites": 1}`. The hot page storm with mesh v2
//! helpers is `mesh_hot_page_readers_fan_out_through_column_helpers` in
//! `contention_scenarios.rs`.

use racer_sim::config::Config;
use racer_sim::engine::{MILLIS, SECONDS};
use racer_sim::mesh::{Change, MeshScenario, churn, edges_changed};
use racer_sim::node::NodeWorld;
use racer_sim::workload::{Churn, Storm};
use serde_json::Value;
use topology::{Membership, target_columns};

/// One Site, so every pool is laid out like the spec table.
fn scenario(nodes: u32, samples: u64, churn: u32) -> MeshScenario {
    MeshScenario {
        samples,
        churn,
        ..MeshScenario::new(nodes, 1)
    }
}

fn u(v: &Value) -> u64 {
    v.as_u64().unwrap_or_else(|| panic!("not a number: {v}"))
}

/// Peers per node from the spec: about `T - 1` column edges and `C - 1` delegate edges each way.
fn expected_degree(k: usize) -> f64 {
    let (columns, size) = target_columns(k);
    (size - 1 + 2 * (columns - 1)) as f64
}

/// Every sampled normal route stays within 2 links, and the mean peers per node is near the spec.
fn assert_default_pool(nodes: u32, samples: u64, table: Option<f64>) -> Value {
    let report = scenario(nodes, samples, 0).run();
    let pool = &report["pools"][0];
    let routes = &pool["routes"];
    assert_eq!(u(&routes["routes"]), samples, "{routes}");
    assert_eq!(u(&routes["unreachable"]), 0, "{routes}");
    assert_eq!(u(&routes["longer_than_normal"]), 0, "{routes}");
    assert!(u(&routes["links"]["max"]) <= 2, "{routes}");
    let mean = report["degree"]["mean"].as_f64().unwrap();
    let expected = table.unwrap_or_else(|| expected_degree(nodes as usize));
    assert!(
        (mean - expected).abs() <= expected * 0.03,
        "{nodes} nodes: {mean} peers per node, spec {expected}: {}",
        report["degree"]
    );
    // No member is far from the mean: column sizes stay within about T / 2 and 3T / 2.
    assert!(
        u(&report["degree"]["max"]) as f64 <= expected * 1.7,
        "{}",
        report["degree"]
    );
    report
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
        ..scenario(20_000, 2048, 0)
    }
    .run();
    let pool = &report["pools"][1];
    assert_eq!(u(&pool["members"]), 256, "{pool}");
    let routes = &pool["routes"];
    assert_eq!(u(&routes["routes"]), 2048, "{routes}");
    assert_eq!(u(&routes["outside_pool"]), 0, "{routes}");
    assert_eq!(u(&routes["longer_than_normal"]), 0, "{routes}");
    assert!(u(&routes["links"]["max"]) <= 2, "{routes}");
    let mean = pool["pool_degree"]["mean"].as_f64().unwrap();
    assert!((mean - 45.0).abs() <= 45.0 * 0.03, "{pool}");
    assert!(routes["rdma_fraction"].as_f64().unwrap() > 0.95, "{routes}");
}

/// A join or leave changes O(sqrt(k)) edges: the member's own edges (about `T + 2C`) plus the
/// delegate edges whose ring arc it takes or gives back.
#[test]
fn churn_changes_few_edges() {
    for nodes in [512u32, 4000] {
        let report = scenario(nodes, 0, 6).run();
        let bound = 6.0 * f64::from(nodes).sqrt();
        for kind in ["edges_changed_per_join", "edges_changed_per_leave"] {
            let max = u(&report[kind]["max"]) as f64;
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
    let report = scenario(20_000, 0, 4).run();
    let bound = 6.0 * 20_000f64.sqrt();
    for kind in ["edges_changed_per_join", "edges_changed_per_leave"] {
        assert!(u(&report[kind]["max"]) as f64 <= bound, "{kind}: {report}");
    }
}

/// Crossing a power of four changes the column count, so most edges move. This is rare (pool sizes
/// 256, 1024, 4096, ...), and the spec accepts it.
#[test]
fn crossing_a_power_of_four_rebuilds_the_mesh() {
    let s = scenario(256, 0, 0);
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
        ..scenario(3000, 0, 0)
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

/// A full node run with churn reports edges changed per membership change, and peers per node,
/// links by transport, and fan-in and egress peaks.
#[test]
fn node_runs_report_mesh_metrics() {
    let nodes = 300;
    let mut config = Config {
        nodes,
        request_rate: 0.0,
        duration_ns: 4 * SECONDS,
        ..Config::default()
    };
    config.pages_per_object = 4;
    config.churn = vec![
        Churn {
            at_ns: SECONDS,
            node: 5,
            join: false,
        },
        Churn {
            at_ns: 2 * SECONDS,
            node: 5,
            join: true,
        },
    ];
    config.storms = vec![Storm::new(100 * MILLIS, 0, nodes, 10 * MILLIS)];
    let mut world = NodeWorld::new(config, 3);
    let end = world.config.duration_ns;
    world.run(end);
    let report = world.report("mesh", 3);
    let mesh = &report["mesh"];
    assert_eq!(
        report["invariants"]["hard_failures"], 0,
        "{}",
        report["invariants"]
    );
    let per_event = &mesh["edges_changed_per_event"];
    assert_eq!(u(&per_event["count"]), 2, "{mesh}");
    assert!(
        u(&per_event["max"]) as f64 <= 6.0 * f64::from(nodes).sqrt(),
        "{mesh}"
    );
    assert!(u(&mesh["degree"]["min"]) > 0, "{mesh}");
    assert!(
        u(&mesh["owner_peak_fanin"]) >= u(&mesh["page_peak_fanin"]),
        "{mesh}"
    );
    assert!(u(&mesh["page_peak_fanin"]) > 0, "{mesh}");
    assert!(
        u(&mesh["owner_peak_egress_bytes_per_s"]) >= world.config.page_bytes,
        "{mesh}"
    );
    let links = u(&mesh["links_rdma"]) + u(&mesh["links_tcp"]);
    let hops = &report["metrics"]["histograms"]["hops"];
    assert!(links >= u(&hops["count"]), "{mesh}");
    assert!(u(&mesh["hops_rdma_only"]["max"]).max(u(&mesh["hops_with_tcp"]["max"])) <= 2);
}
