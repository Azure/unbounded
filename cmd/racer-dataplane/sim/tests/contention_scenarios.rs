//! Pressure scenarios ported from the retired `src/contention.rs` model.
//!
//! The old model ran payload-free abstractions of queues and page ownership.
//! These tests keep the same intent but run the real node logic (placement,
//! walks, flights, fill plans, admission, caches) inside `racer-sim`. See
//! "Round 2: contention model retired" in `designs/racer-core-simulation.md`
//! for the mapping from each old test to its replacement.

use racer_sim::config::Config;
use racer_sim::engine::{MILLIS, SECONDS};
use racer_sim::faults::Fault;
use racer_sim::node::NodeWorld;
use racer_sim::workload::Storm;
use racer_sim::{Options, run as run_cli};
use serde_json::Value;

fn run(config: Config, seed: u64) -> Value {
    let mut world = NodeWorld::new(config, seed);
    let end = world.config.duration_ns;
    world.run(end);
    let report = world.report("contention", seed);
    assert_eq!(
        report["invariants"]["hard_failures"], 0,
        "{}",
        report["invariants"]
    );
    report
}

fn counter(report: &Value, name: &str) -> u64 {
    report["metrics"]["counters"][name].as_u64().unwrap_or(0)
}

fn max_ns(report: &Value, histogram: &str) -> u64 {
    report["metrics"]["histograms"][histogram]["max"]
        .as_u64()
        .unwrap_or(0)
}

/// No background traffic; only the storms a test schedules.
fn quiet(nodes: u32) -> Config {
    Config {
        nodes,
        request_rate: 0.0,
        duration_ns: 4 * SECONDS,
        ..Config::default()
    }
}

fn storm(at_ns: u64, object: u64, nodes: u32, window_ns: u64) -> Storm {
    Storm {
        at_ns,
        object,
        nodes,
        window_ns,
    }
}

fn pages(config: &Config) -> u64 {
    u64::from(config.pages_per_object)
}

/// Old: `cold_nodes_contend_for_one_global_origin_queue`. Each node reads a
/// distinct cold object, so every byte comes from the origin. The origin's
/// bandwidth is one shared server: slowing it slows the fleet, and the last
/// read cannot finish before the origin has sent every byte once.
#[test]
fn cold_nodes_contend_for_one_global_origin() {
    let nodes = 8;
    let build = |origin_bps: u64| {
        let mut config = quiet(nodes);
        config.pages_per_object = 2;
        config.origin_bps = origin_bps;
        config.duration_ns = 20 * SECONDS;
        config.storms = (0..u64::from(nodes))
            .map(|object| storm(0, object, 1, 1))
            .collect();
        config
    };
    let fast_config = build(64 << 30);
    let slow_config = build(64 << 20);
    let requested = u64::from(nodes) * pages(&slow_config) * slow_config.page_bytes;
    let fast = run(fast_config.clone(), 1);
    let slow = run(slow_config.clone(), 1);
    for (config, report) in [(&fast_config, &fast), (&slow_config, &slow)] {
        assert_eq!(
            counter(report, "reads_completed"),
            u64::from(nodes) * pages(config),
            "{report}"
        );
    }
    assert!(counter(&slow, "origin_bytes") >= requested, "{slow}");
    assert!(max_ns(&slow, "read_ns") > max_ns(&fast, "read_ns"));
    let origin_ns = requested * SECONDS / slow_config.origin_bps;
    assert!(
        max_ns(&slow, "read_ns") >= origin_ns,
        "origin bandwidth was multiplied by node count: {} < {origin_ns}",
        max_ns(&slow, "read_ns")
    );
}

/// Old: `warm_peer_fanout_is_limited_by_source_node_network`. One node warms
/// an object, then every other node reads it at once. The origin is not
/// touched again beyond the per-owner bound (Known issue 4), and copies are
/// served by peers whose NICs serialize the fanout: with a slow NIC, many
/// destinations take far longer than one destination, although every
/// destination has its own NIC.
#[test]
fn warm_peer_fanout_is_limited_by_source_node_network() {
    let nodes = 17;
    let build = |readers: u32| {
        let mut config = quiet(nodes);
        config.pages_per_object = 1;
        config.nic_bps = 64 << 20;
        config.duration_ns = 30 * SECONDS;
        config.storms = vec![storm(0, 0, 1, 1), storm(2 * SECONDS, 0, readers, 1)];
        config
    };
    let one = run(build(1), 3);
    let fanout_config = build(nodes);
    let fanout = run(fanout_config.clone(), 3);
    let page = fanout_config.page_bytes;
    assert_eq!(counter(&fanout, "reads_completed"), u64::from(nodes) + 1);
    assert!(
        counter(&fanout, "origin_requests") <= topology::REPLICAS as u64,
        "{fanout}"
    );
    assert!(
        counter(&fanout, "peer_bytes") >= u64::from(nodes - topology::REPLICAS as u32) * page,
        "fanout must fetch copies from peers: {fanout}"
    );
    // Every peer byte crosses some sender NIC, and there are at most
    // `nodes` senders: this is the aggregate floor of the fanout.
    let floor =
        counter(&fanout, "peer_bytes") * SECONDS / (u64::from(nodes) * fanout_config.nic_bps);
    let fanout_max = max_ns(&fanout, "read_peer_ns");
    assert!(fanout_max >= floor, "{fanout_max} < {floor}");
    // Destinations do not each get their own copy at single-reader speed:
    // the few sources serialize the copies on their NICs.
    let one_max = max_ns(&one, "read_peer_ns").max(1);
    assert!(
        fanout_max >= 2 * one_max,
        "fanout bypassed the source NIC: fanout {fanout_max} vs one reader {one_max}"
    );
}

/// Old: `workers_share_node_network_bandwidth`. NIC service is per node, not
/// per worker: adding workers must not multiply node bandwidth for a fanout.
#[test]
fn workers_share_node_network_bandwidth() {
    let build = |workers: u8| {
        let mut config = quiet(9);
        config.workers = workers;
        config.pages_per_object = 4;
        config.nic_bps = 64 << 20;
        config.duration_ns = 30 * SECONDS;
        config.storms = vec![storm(0, 0, 1, 1), storm(2 * SECONDS, 0, 9, 1)];
        config
    };
    let one = run(build(1), 5);
    let four = run(build(4), 5);
    let (one_max, four_max) = (max_ns(&one, "read_ns"), max_ns(&four, "read_ns"));
    assert!(
        four_max * 2 >= one_max,
        "workers multiplied node bandwidth: {four_max} vs {one_max}"
    );
}

/// Old: `overlapping_ranges_join_cold_fills_and_deliver_each_read` and
/// `canceling_one_joined_reader_preserves_the_surviving_read` (join part).
/// Concurrent cold readers of one object join flights instead of fetching
/// again, every read is delivered, and origin traffic stays per owner.
#[test]
fn concurrent_cold_readers_join_flights() {
    let mut config = quiet(64);
    config.storms = vec![storm(0, 0, 64, MILLIS)];
    let report = run(config.clone(), 2);
    assert_eq!(counter(&report, "reads_completed"), 64 * pages(&config));
    assert!(counter(&report, "flight_joins") > 0, "{report}");
    assert!(
        counter(&report, "origin_requests") <= pages(&config) * topology::REPLICAS as u64,
        "{report}"
    );
    // Joined readers share one acquisition per flight; the origin sees a
    // small fraction of the reads.
    assert!(
        counter(&report, "origin_requests") < counter(&report, "reads_completed") / 8,
        "{report}"
    );
}

/// Old: `slow_disk_sheds_persistence_without_losing_reader_progress`. A slow
/// disk holds dirty ciphertext charges longer, so the persistence policy sheds
/// optional writes (`NoHeadroom`) while every read still completes.
#[test]
fn slow_disk_sheds_persistence_without_losing_reader_progress() {
    let build = |disk_write_bps: u64| {
        let mut config = quiet(16);
        config.disk_write_bps = disk_write_bps;
        // Room for a few dirty pages only, so write duration decides headroom.
        config.node.dirty_bytes = 4 * config.page_bytes;
        config.request_rate = 40.0;
        config.objects = 4096;
        config.duration_ns = 2 * SECONDS;
        config
    };
    let fast = run(build(8 << 30), 4);
    let slow = run(build(1 << 20), 4);
    assert!(counter(&slow, "disk_writes") > 0, "{slow}");
    assert!(
        counter(&slow, "persist_no_headroom") > counter(&fast, "persist_no_headroom"),
        "fast={} slow={}",
        fast["metrics"]["counters"],
        slow["metrics"]["counters"]
    );
    assert!(counter(&slow, "disk_writes") < counter(&fast, "disk_writes"));
    // Reads still in flight when the run ends differ slightly between the
    // two runs; none fail and progress is essentially unchanged.
    assert_eq!(counter(&slow, "reads_failed"), 0, "{slow}");
    assert!(
        counter(&slow, "reads_completed") * 100 >= counter(&fast, "reads_completed") * 95,
        "persistence pressure lost reads"
    );
}

/// Old: `partition_retries_recover_after_heal` and
/// `retry_exhaustion_is_bounded_and_does_not_poison_later_requests`. A site
/// partition during a storm may fail reads, but a storm after the heal
/// completes in full.
#[test]
fn reads_recover_after_partition_heals() {
    let mut config = quiet(32);
    config.duration_ns = 8 * SECONDS;
    config.faults = vec![Fault::Partition {
        from_ns: 0,
        until_ns: 2 * SECONDS,
        site_a: 0,
        site_b: 1,
    }];
    config.storms = vec![storm(0, 0, 32, MILLIS), storm(4 * SECONDS, 1, 32, MILLIS)];
    let report = run(config.clone(), 6);
    let completed = counter(&report, "reads_completed");
    assert!(completed >= 32 * pages(&config), "{report}");
}

/// Old: `two_thousand_nodes_keep_per_worker_bounds_under_cold_start` (small
/// version). Every node cold-starts on one object; every read terminates, each
/// page reaches the origin at most once per owner, and admission invariants
/// (per-worker quotas) hold.
///
/// The old model asserted that every read succeeds. With the real logic,
/// before round 3 the flight table cap (`NodeConfig::flights`) rejected joins
/// under this storm and the rejected walks ended in `HopBudgetExhausted`
/// (Known issue 5): 851 of 6,400 reads at 400 nodes and 7,122 of 32,000 at
/// 2,000 nodes (seed 8). A local hop budget shortfall is now walk evidence and
/// route searches queue (Known issue 10), so no read fails that way. Reads
/// may fail only with admission `Overloaded` or a recoverable `Unavailable`
/// after the client's retries, at most 1 in 100. With rank-0 delegation
/// (round 3 origin) one read of 6,400 at 400 nodes (seed 8) ends
/// `Unavailable`: rejected flight joins end walks `Exhausted` and that read
/// ran out of retries.
#[test]
fn cold_start_completes_with_bounded_origin_traffic() {
    cold_start(400);
}

/// Full-size version of [`cold_start_completes_with_bounded_origin_traffic`]
/// (a few seconds under the workspace test profile).
#[test]
fn two_thousand_nodes_cold_start() {
    cold_start(2000);
}

fn cold_start(nodes: u32) {
    let mut config = quiet(nodes);
    // Long enough for every read to end (read timeout is 10 s).
    config.duration_ns = 30 * SECONDS;
    config.storms = vec![storm(0, 0, nodes, 10 * MILLIS)];
    let report = run(config.clone(), 8);
    let total = u64::from(nodes) * pages(&config);
    let completed = counter(&report, "reads_completed");
    let failed = counter(&report, "reads_failed");
    assert_eq!(completed + failed, total, "{report}");
    // `Overloaded` is an admission rejection (quota full), the expected
    // shed under pressure.
    assert_eq!(
        counter(&report, "error_hop_budget_exhausted"),
        0,
        "{report}"
    );
    assert_eq!(
        failed,
        counter(&report, "error_overloaded") + counter(&report, "error_unavailable"),
        "only admission or exhausted retries may fail reads: {report}"
    );
    assert!(failed * 100 <= total, "{report}");
    let max = report["origin_per_page"]["max_requests_per_page"]
        .as_u64()
        .unwrap();
    assert!((1..=topology::REPLICAS as u64).contains(&max), "{max}");
    assert!(counter(&report, "disk_writes") > 0, "{report}");
    let per_node = report["memory"]["accounted_bytes_per_node"]
        .as_u64()
        .unwrap();
    assert!(per_node < 640 << 10, "{per_node}");
}

/// Old: `generated_seed_matrix_replays_identical_reports_and_drains`. A seed
/// matrix over the fault scenario (partition, crash, slow node, origin
/// errors): each seed replays bit-identically, invariants hold, faults have an
/// effect, and distinct seeds produce distinct traces.
#[test]
fn seed_matrix_replays_identical_reports() {
    let mut traces = Vec::new();
    for seed in [1, 7, 42, 0xdead_beef] {
        let options = Options {
            scenario: "faults".into(),
            nodes: Some(40),
            seed,
            duration_ns: Some(SECONDS),
            ..Options::default()
        };
        let strip = |mut v: Value| {
            let o = v.as_object_mut().unwrap();
            o.remove("wall_ms");
            o.remove("profile_wall_ms");
            o["memory"]
                .as_object_mut()
                .unwrap()
                .remove("peak_rss_bytes");
            v
        };
        let report = strip(run_cli(&options).unwrap());
        let replay = strip(run_cli(&options).unwrap());
        assert_eq!(report, replay, "replay diverged for seed={seed}");
        assert_eq!(report["invariants"]["hard_failures"], 0, "seed={seed}");
        let c = &report["metrics"]["counters"];
        assert!(c["reads_completed"].as_u64().unwrap() > 0, "seed={seed}");
        assert_eq!(c["crashes"], 1, "seed={seed}");
        assert!(c["fills"].as_u64().unwrap() > 0, "seed={seed}");
        traces.push(report["trace_hash"].clone());
    }
    assert!(
        traces.windows(2).all(|pair| pair[0] != pair[1]),
        "distinct seeds produced identical traces"
    );
}

/// Known issue 11 (data plane, round 4 capacity). A warm hot page is served
/// only by its `REPLICAS` owners, so their NICs bound the storm's peer
/// throughput. Before round 4 a sender streamed every queued page body even
/// after the requester's attempt timed out: the owners' NIC queues grew
/// without bound, almost every reply arrived late (`late_replies`), and
/// useful throughput collapsed far below the NIC bound. Now a body stops at
/// the requester's attempt deadline, so no reply arrives late and the owners
/// deliver at least half of what their NICs allow during the storm.
#[test]
fn overloaded_hot_page_owners_do_not_stream_to_abandoned_requesters() {
    let nodes = 96;
    let mut config = quiet(nodes);
    config.pages_per_object = 1;
    config.nic_bps = 4 << 20;
    config.duration_ns = 40 * SECONDS;
    config.storms = vec![storm(0, 0, 1, 1), storm(5 * SECONDS, 0, nodes, MILLIS)];
    let report = run(config.clone(), 5);
    let completed = counter(&report, "reads_completed");
    let failed = counter(&report, "reads_failed");
    assert_eq!(completed + failed, u64::from(nodes) + 1, "{report}");
    assert!(failed > 0, "the storm must overload the owners: {report}");
    assert_eq!(counter(&report, "late_replies"), 0, "{report}");
    assert!(counter(&report, "replies_abandoned") > 0, "{report}");
    // Each storm read ends within the read timeout (10 s) plus retries'
    // short backoff; over that span the owners' NICs can send at most this
    // many pages. Relayed bytes also cross relay NICs, so this is an upper
    // bound on what the owners alone can deliver.
    let page = config.page_bytes;
    let span_s = 10;
    let bound = topology::REPLICAS as u64 * config.nic_bps * span_s / page;
    let peer = counter(&report, "fills_from_peer");
    assert!(
        peer * 2 >= bound,
        "owners delivered {peer} pages, under half of the NIC bound {bound}: {report}"
    );
}
