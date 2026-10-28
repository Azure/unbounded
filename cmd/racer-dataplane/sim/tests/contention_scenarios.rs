//! Pressure scenarios ported from the retired `src/contention.rs` model.
//!
//! The old model ran payload-free abstractions of queues and page ownership.
//! These tests keep the same intent but run the real node logic (placement,
//! walks, flights, fill plans, admission, caches) inside `racer-sim`.
//! See "Scenarios" in `designs/racer-core-simulation.md`.

use racer_sim::config::Config;
use racer_sim::engine::{MILLIS, SECONDS};
use racer_sim::faults::Fault;
use racer_sim::node::NodeWorld;
use racer_sim::resources::OriginErrors;
use racer_sim::workload::{Churn, Storm};
use racer_sim::{Options, run as run_cli};
use serde_json::Value;

fn run_world(config: Config, seed: u64) -> (NodeWorld, Value) {
    let mut world = NodeWorld::new(config, seed);
    let end = world.config.duration_ns;
    world.run(end);
    let report = world.report("test", seed);
    assert_eq!(
        report["invariants"]["hard_failures"], 0,
        "{}",
        report["invariants"]
    );
    (world, report)
}

fn run(config: Config, seed: u64) -> Value {
    run_world(config, seed).1
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

fn quiet3(nodes: u32) -> Config {
    Config {
        duration_ns: 3 * SECONDS,
        ..quiet(nodes)
    }
}

fn pages(config: &Config) -> u64 {
    u64::from(config.pages_per_object)
}

/// Old: `cold_nodes_contend_for_one_global_origin_queue`. Each node reads a distinct cold object,
/// so every byte comes from the origin. The origin's bandwidth is one shared server: slowing it
/// slows the fleet, and the last read cannot finish before the origin has sent every byte once.
#[test]
fn cold_nodes_contend_for_one_global_origin() {
    let nodes = 8;
    let build = |origin_bps: u64| {
        let mut config = quiet(nodes);
        config.pages_per_object = 2;
        config.origin_bps = origin_bps;
        config.duration_ns = 20 * SECONDS;
        config.storms = (0..u64::from(nodes))
            .map(|object| Storm::new(0, object, 1, 1))
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

/// Old: `warm_peer_fanout_is_limited_by_source_node_network`. One node warms an object, then every
/// other node reads it at once. The origin is not touched again beyond the per-owner bound, and
/// copies are served by peers whose NICs serialize the fanout: with a slow NIC, many destinations
/// take far longer than one destination, although every destination has its own NIC.
#[test]
fn warm_peer_fanout_is_limited_by_source_node_network() {
    let nodes = 17;
    let build = |readers: u32| {
        let mut config = quiet(nodes);
        config.pages_per_object = 1;
        config.nic_bps = 64 << 20;
        config.duration_ns = 30 * SECONDS;
        config.storms = vec![
            Storm::new(0, 0, 1, 1),
            Storm::new(2 * SECONDS, 0, readers, 1),
        ];
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
    // Every peer byte crosses some sender NIC, and there are at most `nodes` senders: this is the
    // aggregate floor of the fanout.
    let floor =
        counter(&fanout, "peer_bytes") * SECONDS / (u64::from(nodes) * fanout_config.nic_bps);
    let fanout_max = max_ns(&fanout, "read_peer_ns");
    assert!(fanout_max >= floor, "{fanout_max} < {floor}");
    // Destinations do not each get their own copy at single-reader speed: the few sources serialize
    // the copies on their NICs.
    let one_max = max_ns(&one, "read_peer_ns").max(1);
    assert!(
        fanout_max >= 2 * one_max,
        "fanout bypassed the source NIC: fanout {fanout_max} vs one reader {one_max}"
    );
}

/// Old: `workers_share_node_network_bandwidth`. NIC service is per node, not per worker: adding
/// workers must not multiply node bandwidth for a fanout.
#[test]
fn workers_share_node_network_bandwidth() {
    let build = |workers: u8| {
        let mut config = quiet(9);
        config.workers = workers;
        config.pages_per_object = 4;
        config.nic_bps = 64 << 20;
        config.duration_ns = 30 * SECONDS;
        config.storms = vec![Storm::new(0, 0, 1, 1), Storm::new(2 * SECONDS, 0, 9, 1)];
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
/// `canceling_one_joined_reader_preserves_the_surviving_read` (join part). Concurrent cold readers
/// of one object join flights instead of fetching again, every read is delivered, and origin
/// traffic stays per owner.
#[test]
fn concurrent_cold_readers_join_flights() {
    let mut config = quiet(64);
    config.storms = vec![Storm::new(0, 0, 64, MILLIS)];
    let report = run(config.clone(), 2);
    assert_eq!(counter(&report, "reads_completed"), 64 * pages(&config));
    assert!(counter(&report, "flight_joins") > 0, "{report}");
    assert!(
        counter(&report, "origin_requests") <= pages(&config) * topology::REPLICAS as u64,
        "{report}"
    );
    // Joined readers share one acquisition per flight; the origin sees a small fraction of the
    // reads.
    assert!(
        counter(&report, "origin_requests") < counter(&report, "reads_completed") / 8,
        "{report}"
    );
}

/// Old: `slow_disk_sheds_persistence_without_losing_reader_progress`. A slow disk holds dirty
/// ciphertext charges longer, so the persistence policy sheds optional writes (`NoHeadroom`) while
/// every read still completes.
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
    // Reads still in flight when the run ends differ slightly between the two runs; none fail and
    // progress is essentially unchanged.
    assert_eq!(counter(&slow, "reads_failed"), 0, "{slow}");
    assert!(
        counter(&slow, "reads_completed") * 100 >= counter(&fast, "reads_completed") * 95,
        "persistence pressure lost reads"
    );
}

/// Old: `partition_retries_recover_after_heal` and
/// `retry_exhaustion_is_bounded_and_does_not_poison_later_requests`. A site partition during a
/// storm may fail reads, but a storm after the heal completes in full.
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
    config.storms = vec![
        Storm::new(0, 0, 32, MILLIS),
        Storm::new(4 * SECONDS, 1, 32, MILLIS),
    ];
    let report = run(config.clone(), 6);
    let completed = counter(&report, "reads_completed");
    assert!(completed >= 32 * pages(&config), "{report}");
}

/// Old: `two_thousand_nodes_keep_per_worker_bounds_under_cold_start` (small version). Every node
/// cold-starts on one object; every read terminates, each page reaches the origin at most once per
/// owner, and admission invariants (per-worker quotas) hold. The old model asserted that every read
/// succeeds. With the real logic, before round 3 the flight table cap (`NodeConfig::flights`)
/// rejected joins under this storm and the rejected walks ended in `HopBudgetExhausted`: 851 of
/// 6,400 reads at 400 nodes and 7,122 of 32,000 at 2,000 nodes (seed 8). A local hop budget
/// shortfall is now walk evidence and route searches queue, so no read fails that way. Reads may
/// fail only with admission `Overloaded` or a recoverable `Unavailable` after the client's retries,
/// at most 1 in 100. With rank-0 delegation (round 3 origin) one read of 6,400 at 400 nodes (seed
/// 8) ends `Unavailable`: rejected flight joins end walks `Exhausted` and that read ran out of
/// retries.
#[test]
fn cold_start_completes_with_bounded_origin_traffic() {
    cold_start(400);
}

/// Full-size version of [`cold_start_completes_with_bounded_origin_traffic`] (a few seconds under
/// the workspace test profile).
#[test]
fn two_thousand_nodes_cold_start() {
    cold_start(2000);
}

fn cold_start(nodes: u32) {
    let mut config = quiet(nodes);
    // Long enough for every read to end (read timeout is 10 s).
    config.duration_ns = 30 * SECONDS;
    config.storms = vec![Storm::new(0, 0, nodes, 10 * MILLIS)];
    let report = run(config.clone(), 8);
    let total = u64::from(nodes) * pages(&config);
    let completed = counter(&report, "reads_completed");
    let failed = counter(&report, "reads_failed");
    assert_eq!(completed + failed, total, "{report}");
    // `Overloaded` is an admission rejection (quota full), the expected shed under pressure.
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

/// Old: `generated_seed_matrix_replays_identical_reports_and_drains`. A seed matrix over the fault
/// scenario (partition, crash, slow node, origin errors): each seed replays bit-identically,
/// invariants hold, faults have an effect, and distinct seeds produce distinct traces.
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

/// (data plane, round 4 capacity).
#[test]
fn overloaded_hot_page_owners_do_not_stream_to_abandoned_requesters() {
    let nodes = 96;
    let mut config = quiet(nodes);
    config.node.fanout = false;
    config.pages_per_object = 1;
    config.nic_bps = 4 << 20;
    config.duration_ns = 40 * SECONDS;
    config.storms = vec![
        Storm::new(0, 0, 1, 1),
        Storm::new(5 * SECONDS, 0, nodes, MILLIS),
    ];
    let report = run(config.clone(), 5);
    let completed = counter(&report, "reads_completed");
    let failed = counter(&report, "reads_failed");
    assert_eq!(completed + failed, u64::from(nodes) + 1, "{report}");
    assert!(failed > 0, "the storm must overload the owners: {report}");
    // No page body arrives after its requester gave up.
    assert_eq!(counter(&report, "late_page_replies"), 0, "{report}");
    assert!(counter(&report, "replies_abandoned") > 0, "{report}");
    // Each storm read ends within the read timeout (10 s) plus retries' short backoff; over that
    // span the owners' NICs can send at most this many pages.
    let page = config.page_bytes;
    let span_s = 10;
    let bound = topology::REPLICAS as u64 * config.nic_bps * span_s / page;
    let peer = counter(&report, "fills_from_peer");
    assert!(
        peer * 2 >= bound,
        "owners delivered {peer} pages, under half of the NIC bound {bound}: {report}"
    );
}

/// (round 4 fanout). One warm page read by 512 nodes within a millisecond over 32 MiB/s NICs: in a
/// 2 s attempt the page's 3 owners can send about 192 copies, so without fan-out readers queue on
/// the owners until their attempts time out and some reads fail. With fan-out, owners already
/// streaming `hot_page_bodies` bodies of the page shed further readers with `Overloaded`, and a
/// shed reader asks one of the page's helpers, which the owners always serve. Every read completes,
/// readers get copies from helpers, and the page still reaches the origin once.
#[test]
fn hot_page_readers_fan_out_through_helpers() {
    let nodes = 512;
    let mut config = quiet(nodes);
    config.pages_per_object = 1;
    config.nic_bps = 32 << 20;
    config.duration_ns = 40 * SECONDS;
    config.storms = vec![
        Storm::new(0, 0, 1, 1),
        Storm::new(5 * SECONDS, 0, nodes, MILLIS),
    ];
    let on = run(config.clone(), 5);
    config.node.fanout = false;
    // Without fan-out, owners whose flight is full shed readers too; since a shed reader's retry
    // visits the other owners first, which alone completes every read of this storm (513 of 513).
    config.node.shed_busy = false;
    let off = run(config, 5);
    let reads = u64::from(nodes) + 1;
    assert_eq!(counter(&on, "reads_completed"), reads, "{on}");
    assert_eq!(counter(&on, "reads_failed"), 0, "{on}");
    assert!(counter(&on, "hot_page_sheds") > 0, "{on}");
    assert!(counter(&on, "helper_copies") > 0, "{on}");
    assert_eq!(counter(&on, "origin_requests"), 1, "{on}");
    assert!(counter(&off, "reads_failed") > 0, "{off}");
    assert_eq!(counter(&off, "helper_probes"), 0, "{off}");
    assert!(
        max_ns(&on, "read_ns") < max_ns(&off, "read_ns"),
        "fan-out must shorten the slowest read: {} vs {}",
        max_ns(&on, "read_ns"),
        max_ns(&off, "read_ns")
    );
}

/// (round 4, overload): with the origin far slower than the demand, rank 0's request for a page
/// queues for seconds.
#[test]
fn owners_above_rank_zero_do_not_overlap_a_slow_origin_fetch() {
    let nodes = 300;
    let mut config = quiet(nodes);
    config.pages_per_object = 64;
    // 64 MiB of cold pages at 8 MiB/s: rank 0's requests queue for up to 8 s, past the 2 s attempt
    // timeout of the owners delegating to it.
    config.origin_bps = 8 << 20;
    config.duration_ns = 60 * SECONDS;
    // Few enough readers, and room for every waiter, that rank 0's flight never refuses a waiter
    // (an `Overloaded` rank 0 is still transient evidence).
    config.node.waiters_per_flight = 128;
    config.storms = vec![Storm::new(0, 0, 48, 100 * MILLIS)];
    let report = run(config, 3);
    assert!(counter(&report, "origin_left_to_rank0") > 0, "{report}");
    assert_eq!(
        counter(&report, "origin_repeats_concurrent"),
        0,
        "{}",
        report["origin_per_page"]
    );
    assert_eq!(
        report["origin_per_page"]["max_requests_per_page"], 1,
        "{}",
        report["origin_per_page"]
    );
    // Rank 0 is busy, not hung: the owners whose probes timed out ask it whether it answers, and it
    // does.
    assert!(counter(&report, "silence_pings") > 0, "{report}");
    assert_eq!(counter(&report, "silence_marks"), 0, "{report}");
    assert_eq!(counter(&report, "silent_destination_fails"), 0, "{report}");
    // bound: a ping follows a probe timeout (at most one each), two unanswered exchanges over a
    // last link, or a silent peer's recheck; never anything else.
    let pings = counter(&report, "silence_pings");
    let bound = counter(&report, "probe_timeouts")
        + counter(&report, "silence_timeouts") / u64::from(racer_core::liveness::LINK_TIMEOUTS)
        + counter(&report, "silence_rechecks");
    assert!(pings <= bound, "{pings} pings, bound {bound}: {report}");
}

/// (round 4, relay): a relay forwarding a body still queued at a busy owner reserved its own NIC at
/// the body's future send time, so its other sends queued behind the gap, backlogs spread from
/// relay to relay and relays held their `Relay` reservations for hundreds of milliseconds (5k
/// `zipf`: 339k relay rejections, 4,373 failed reads).
#[test]
fn relays_hold_their_reservation_only_while_the_body_streams() {
    let mut config = Config {
        nodes: 300,
        objects: 1024,
        zipf_s: 1.0,
        ..Config::default()
    };
    config.node.relay_transfers = 16;
    let report = run(config, 1);
    let stream = &report["metrics"]["histograms"]["relay_hold_stream_ns"];
    assert!(stream["count"].as_u64().unwrap_or(0) > 1_000, "{stream}");
    let p99 = stream["p99"].as_u64().unwrap();
    assert!(
        p99 < 30 * MILLIS,
        "relayed bodies queue behind idle gaps: {stream}"
    );
    assert_eq!(counter(&report, "reads_failed"), 0, "{report}");
}

/// Runs ten hung peers (hung until `until_ns`) under two storms 30 s apart, and returns the reads
/// that failed on members whose own peer server does not hang, before and after 29 s.
fn hang(watch: bool, silence: bool, until_ns: u64) -> (u64, u64, Value) {
    let failed = |m: &racer_sim::report::Metrics| {
        m.get("reads_failed") - m.get("reads_failed_on_hung_nodes")
    };
    let mut config = quiet(200);
    config.duration_ns = 45 * SECONDS;
    config.pages_per_object = 64;
    config.node.rank0_watch = watch;
    config.node.silence = silence;
    config.faults = (0..10)
        .map(|node| Fault::Hang {
            node: node * 20,
            from_ns: 0,
            until_ns,
        })
        .collect();
    config.storms = vec![
        Storm::new(0, 0, 100, 100 * MILLIS),
        Storm::new(30 * SECONDS, 0, 100, 100 * MILLIS),
    ];
    let mut world = NodeWorld::new(config, 5);
    world.run(29 * SECONDS);
    let early = failed(&world.metrics);
    let end = world.config.duration_ns;
    world.run(end);
    let late = failed(&world.metrics) - early;
    let report = world.report("contention", 5);
    assert_eq!(
        report["invariants"]["hard_failures"], 0,
        "{}",
        report["invariants"]
    );
    assert!(counter(&report, "messages_lost_to_hang") > 0, "{report}");
    (early, late, report)
}

/// A rank 0 whose peer server accepts requests and never answers used to block its pages for as
/// long as it stayed a member (`rank0_watch`), and each owner and reader learned on its own that it
/// hangs (`silence`: a ping finds it and the verdict is shared through the sender's links). A
/// recovered peer answers its recovery ping.
#[test]
fn hung_peers_are_found_and_recovered_peers_are_used_again() {
    let forever = 45 * SECONDS;
    let (early, late, report) = hang(true, false, forever);
    assert!(
        counter(&report, "rank0_watch_overdue_walks") > 0,
        "{report}"
    );
    assert!(
        late <= 1,
        "second storm still fails {late} (first: {early}): {report}"
    );
    assert!(early >= 50, "the watch alone: {early} early: {report}");
    assert_eq!(counter(&report, "silence_pings"), 0, "{report}");
    let (old_early, old_late, _) = hang(false, false, forever);
    assert!(
        old_late >= 100 && old_late > 50 * late,
        "without the watch: {old_early} early, {old_late} late"
    );
    for watch in [true, false] {
        let (early, late, report) = hang(watch, true, forever);
        assert!(
            early <= 2 && late <= 1,
            "watch {watch}: {early} early, {late} late: {report}"
        );
        assert!(counter(&report, "silence_marks") > 0, "{report}");
        assert!(counter(&report, "silent_destination_fails") > 0, "{report}");
    }
    let (_, late, report) = hang(true, true, 10 * SECONDS);
    assert!(counter(&report, "silence_marks") > 0, "{report}");
    assert!(counter(&report, "silence_recoveries") > 0, "{report}");
    assert!(counter(&report, "silence_pings_direct") > 0, "{report}");
    assert_eq!(late, 0, "{report}");
}

/// (round 4, busy): an owner that sheds a reader (a signed `Overloaded`, here a hot page's owner)
/// is busy, not hung.
#[test]
fn readers_do_not_return_first_to_an_owner_that_shed_them() {
    let nodes = 512;
    let mut config = quiet(nodes);
    config.pages_per_object = 1;
    config.nic_bps = 32 << 20;
    config.duration_ns = 40 * SECONDS;
    config.storms = vec![
        Storm::new(0, 0, 1, 1),
        Storm::new(5 * SECONDS, 0, nodes, MILLIS),
        Storm::new(7 * SECONDS, 0, nodes, MILLIS),
    ];
    let on = run(config.clone(), 5);
    config.node.shed_busy = false;
    let off = run(config, 5);
    assert!(counter(&off, "shed_repeats") > 0, "{off}");
    assert!(
        counter(&on, "shed_repeats") * 2 <= counter(&off, "shed_repeats"),
        "{} vs {}",
        counter(&on, "shed_repeats"),
        counter(&off, "shed_repeats")
    );
    assert!(counter(&on, "reads_failed") <= counter(&off, "reads_failed"));
    assert_eq!(counter(&on, "origin_requests"), 1, "{on}");
    // Busy, not hung: nobody is marked silent for a shed.
    assert_eq!(counter(&on, "silence_marks"), 0, "{on}");
}

/// One reader fetches an object, then every node reads it.
#[test]
fn single_reader_then_storm_reuses_copies() {
    let mut config = quiet3(64);
    config.storms = vec![
        Storm::new(0, 3, 1, 1),
        Storm::new(SECONDS, 3, 64, 100 * MILLIS),
    ];
    let (world, report) = run_world(config, 7);
    let pages = u64::from(world.config.pages_per_object);
    assert_eq!(counter(&report, "reads_completed"), pages * 65);
    let origin = counter(&report, "origin_requests");
    let per_page: Vec<u32> = (0..pages)
        .map(|p| world.origin_requests(3 * pages + p))
        .collect();
    // Before owners delegated to rank 0 this was 33.
    assert_eq!(origin, pages, "{per_page:?}");
    assert!(counter(&report, "peer_bytes") > 0);
    assert!(counter(&report, "flight_joins") + counter(&report, "peer_memory_hits") > 0);
    assert!(
        report["metrics"]["histograms"]["hops"]["count"]
            .as_u64()
            .unwrap()
            > 0
    );
}

/// Cold-start storm: origin traffic per page is bounded by the replica count.
#[test]
fn cold_storm_bounds_origin_per_page() {
    let mut config = quiet3(300);
    config.duration_ns = 4 * SECONDS;
    config.storms = vec![Storm::new(0, 0, 300, 10 * MILLIS)];
    let (_, report) = run_world(config, 3);
    let max = report["origin_per_page"]["max_requests_per_page"]
        .as_u64()
        .unwrap();
    assert!(max <= topology::REPLICAS as u64, "{max}");
    assert_eq!(report["origin_per_page"]["pages"], 16);
    assert!(counter(&report, "flight_joins") > 0);
}

fn churny(nodes: u32, retained: u64) -> Config {
    let mut config = Config {
        nodes,
        duration_ns: 2 * SECONDS,
        objects: 32,
        ..Config::default()
    };
    config.node.retained_versions = retained;
    config.churn = Churn::random(
        nodes,
        nodes / 8,
        config.duration_ns,
        SECONDS / 2,
        &mut racer_core::time::SplitMix64::new(5),
    );
    config
}

/// 412 continues with the remaining walk; 403 and 5xx fail the fill.
#[test]
fn origin_errors_follow_the_fill_plan() {
    let mut config = quiet3(32);
    config.request_rate = 20.0;
    config.origin_errors = OriginErrors {
        p412: 0.2,
        p403: 0.05,
        p5xx: 0.1,
    };
    let (_, report) = run_world(config, 4);
    assert!(counter(&report, "remaining_walks") > 0);
    assert!(counter(&report, "origin_403") > 0);
    assert!(counter(&report, "origin_5xx") > 0);
    assert!(counter(&report, "fills_failed") > 0);
    assert!(counter(&report, "error_origin_forbidden") > 0);
}

/// A 403 fails only the leader that received it; a waiter on the same flight is re-elected and
/// fills with its own budget (matching production `Flights::fail(OriginForbidden)`).
#[test]
fn origin_forbidden_reelects_a_waiter() {
    let mut config = quiet3(64);
    config.storms = vec![Storm::new(0, 1, 64, MILLIS)];
    config.origin_errors = OriginErrors {
        p412: 0.0,
        p403: 0.5,
        p5xx: 0.0,
    };
    let (_, report) = run_world(config, 11);
    assert!(counter(&report, "origin_403") > 0);
    assert!(counter(&report, "flight_reelected") > 0);
    assert_eq!(counter(&report, "flight_unexpected_lead"), 0);
    assert!(counter(&report, "error_origin_forbidden") > 0);
}

/// Tight quotas reject work per resource class.
#[test]
fn tight_quotas_reject_by_class() {
    let mut config = quiet3(100);
    config.node.relay_transfers = 1;
    config.node.flights = 1;
    config.storms = vec![Storm::new(0, 1, 100, MILLIS)];
    let (_, report) = run_world(config, 9);
    // The flight table's own entry cap rejects before the Flight quota, as in production
    // `Flights::join_prepared`; relays hit the Relay quota.
    let r = &report["rejections"];
    assert!(r["relay"].as_u64().unwrap() > 0, "{r}");
    assert!(counter(&report, "flight_join_rejected") > 0);
}

#[test]
fn same_seed_replays_and_seeds_differ() {
    let a = run_world(churny(60, 2), 11).1;
    let b = run_world(churny(60, 2), 11).1;
    assert_eq!(a["trace_hash"], b["trace_hash"]);
    assert_eq!(a["metrics"], b["metrics"]);
    let c = run_world(churny(60, 2), 12).1;
    assert_ne!(a["trace_hash"], c["trace_hash"]);
}

#[test]
fn memory_accounting_is_per_component() {
    let (_, report) = run_world(churny(100, 2), 1);
    let components = report["memory"]["components"].as_object().unwrap();
    for name in [
        "placement",
        "paths",
        "memory_lru",
        "disk_index",
        "flights",
        "quotas",
        "placement_memo",
        "route_memo",
    ] {
        assert!(components.contains_key(name), "{name}");
    }
    assert!(components["placement"].as_u64().unwrap() > 0);
    let per_node = report["memory"]["accounted_bytes_per_node"]
        .as_u64()
        .unwrap();
    assert!(per_node < 640 << 10, "{per_node}");
}

fn client_read(
    node: u32,
    page: u64,
    at: racer_sim::engine::Nanos,
) -> racer_sim::node::world::Event {
    racer_sim::node::world::Event::Read(racer_sim::node::world::ClientRead {
        node,
        incarnation: 0,
        page,
        start: at,
        attempt: 0,
        range: 0,
        slot: 0,
    })
}

/// Retention observes every client read, including memory hits: a non-owner persists a page on its
/// second read (second sight), not at publication.
#[test]
fn retention_observes_client_reads_including_memory_hits() {
    let config = quiet3(64);
    let mut world = NodeWorld::new(config, 5);
    let pages = u64::from(world.config.pages_per_object);
    let reader = 9;
    let foreign: Vec<u64> = (3 * pages..4 * pages)
        .filter(|p| !world.owners(*p).contains(&reader))
        .collect();
    assert!(!foreign.is_empty());
    for p in &foreign {
        world.engine.at(0, client_read(reader, *p, 0));
    }
    world.run(SECONDS);
    let after_first = world.metrics.get("retention_observations");
    assert_eq!(after_first, foreign.len() as u64);
    for p in &foreign {
        assert!(
            !world.on_disk(reader, *p),
            "page {p} persisted on first sight"
        );
    }
    let hits = world.metrics.get("memory_hits");
    for p in &foreign {
        world.engine.at(SECONDS, client_read(reader, *p, SECONDS));
    }
    world.run(2 * SECONDS);
    assert_eq!(
        world.metrics.get("memory_hits") - hits,
        foreign.len() as u64
    );
    assert_eq!(
        world.metrics.get("retention_observations"),
        2 * foreign.len() as u64
    );
    for p in &foreign {
        assert!(
            world.on_disk(reader, *p),
            "page {p} not persisted on second sight"
        );
    }
    assert_eq!(world.invariants.to_json()["hard_failures"], 0);
}

/// Rankings and route searches yield between CPU slices and hold their placement pin or search slot
/// meanwhile, so production's concurrency limits reject concurrent work with `Overloaded` without a
/// search queue. With the production queue, searches beyond the slot limit wait and are woken when
/// a slot frees, so far fewer are rejected, and none at the default limits.
#[test]
fn yielding_searches_hit_production_concurrency_limits() {
    let mut config = quiet3(600);
    config.duration_ns = 2 * SECONDS;
    config.node.placement_entries = 2;
    config.node.active_searches = 1;
    config.storms = vec![Storm::new(0, 2, 600, MILLIS)];
    let mut rejecting = config.clone();
    rejecting.node.search_queue = 0;
    let (_, before) = run_world(rejecting.clone(), 3);
    assert!(counter(&before, "ranking_overloaded") > 0, "{before}");
    let rejected = counter(&before, "route_search_overloaded");
    assert!(rejected > 0, "{before}");
    // 600 members take three 256-member chunks per cold ranking.
    assert!(counter(&before, "ranking_polls") > counter(&before, "fills"));
    rejecting.node.placement_entries = 256;
    rejecting.node.active_searches = 8;
    let (_, report) = run_world(rejecting, 3);
    assert_eq!(counter(&report, "ranking_overloaded"), 0, "{report}");
    let (_, after) = run_world(config.clone(), 3);
    assert!(counter(&after, "route_search_queued") > 0, "{after}");
    assert!(counter(&after, "route_search_queue_wakes") > 0, "{after}");
    assert!(
        counter(&after, "route_search_overloaded") * 4 < rejected,
        "{after}"
    );
    config.node.active_searches = 8;
    let (_, report) = run_world(config, 3);
    assert_eq!(counter(&report, "route_search_overloaded"), 0, "{report}");
}

/// Runs the hedge scenario: every ranked owner of object 4 warms its pages, then each rank 0 owner
/// becomes 255 times slower and non-owner readers (never slow) read every page.
fn hedge_scenario(slots: u32) -> (NodeWorld, Value) {
    use racer_sim::faults::Fault;
    let mut config = quiet3(64);
    config.node.hedge_slots = slots;
    config.node.hedge_delay_ns = 5 * MILLIS;
    config.node.hedge_bytes =
        u64::from(slots.max(1)) * racer_sim::node::world::DUPLICATE_BYTES as u64;
    config.duration_ns = 8 * SECONDS;
    let probe = NodeWorld::new(config.clone(), 21);
    let pages = u64::from(probe.config.pages_per_object);
    let range: Vec<u64> = (4 * pages..5 * pages).collect();
    let owners: Vec<Vec<u32>> = range.iter().map(|p| probe.owners(*p)).collect();
    let slow: std::collections::BTreeSet<u32> = owners.iter().map(|o| o[0]).collect();
    for node in &slow {
        config.faults.push(Fault::Slow {
            node: *node,
            factor: 255,
            from_ns: SECONDS,
            until_ns: 3 * SECONDS,
        });
    }
    let mut world = NodeWorld::new(config, 21);
    for (p, o) in range.iter().zip(&owners) {
        for node in o {
            world.engine.at(0, client_read(*node, *p, 0));
        }
        let readers = (0..64u32)
            .filter(|n| !o.contains(n) && !slow.contains(n))
            .take(4);
        for (i, node) in readers.enumerate() {
            let at = SECONDS + 100 * MILLIS + i as u64 * MILLIS;
            world.engine.at(at, client_read(node, *p, at));
        }
    }
    world.run(8 * SECONDS);
    let report = world.report("hedge", 21);
    assert_eq!(
        report["invariants"]["hard_failures"], 0,
        "{}",
        report["invariants"]
    );
    (world, report)
}

/// Production default (no slots) never races; with slots, a slow rank 0 owner loses to the
/// copy-only duplicate at the second owner: lower peer read latency, duplicate bytes counted per
/// started hedge, and the split budgets never grow (checked by `budget_never_grows`).
#[test]
fn hedging_beats_a_slow_primary() {
    let (_, off) = hedge_scenario(0);
    let (_, on) = hedge_scenario(4);
    assert_eq!(counter(&off, "hedge_races"), 0);
    assert!(counter(&off, "hedge_eligible") > 0, "{off}");
    assert!(counter(&on, "hedge_races") > 0, "{on}");
    let started = counter(&on, "hedge_started");
    assert!(started > 0, "{on}");
    // The duplicate at the fast second owner wins most races.
    assert!(2 * counter(&on, "hedge_won") > started, "{on}");
    assert!(counter(&on, "hedge_losers_canceled") > 0, "{on}");
    assert_eq!(
        counter(&on, "hedge_duplicate_bytes"),
        started * racer_sim::node::world::DUPLICATE_BYTES as u64
    );
    assert_eq!(
        counter(&on, "reads_completed"),
        counter(&off, "reads_completed"),
        "on {}\noff {}",
        on["metrics"]["counters"],
        off["metrics"]["counters"]
    );
    let mean = |r: &Value| {
        r["metrics"]["histograms"]["read_ns"]["mean"]
            .as_u64()
            .unwrap()
    };
    assert!(
        mean(&on) < mean(&off),
        "on {} off {}",
        mean(&on),
        mean(&off)
    );
}

/// A duplicate publish of a resident page does not refresh its recency, as in production
/// `MemoryCache::publish`: inserting 0, 1, 0, 2 into a two-page worker cache evicts 0, the oldest
/// insert.
#[test]
fn duplicate_memory_publish_does_not_touch() {
    let mut config = quiet3(4);
    config.memory_pages = 2 * u32::from(config.workers);
    let mut world = NodeWorld::new(config, 1);
    for page in [0, 1, 0, 2] {
        world.memory_insert(0, 0, page);
    }
    assert!(!world.in_memory(0, 0, 0));
    assert!(world.in_memory(0, 0, 1));
    assert!(world.in_memory(0, 0, 2));
}

/// Ownership moves when nodes join: an owner that adopted the new membership asks the page's
/// previous owners copy-only before origin, so a page read before the join is not fetched again
/// after it.
#[test]
fn joined_owners_copy_from_previous_owners() {
    let mut config = quiet3(64);
    config.duration_ns = 4 * SECONDS;
    let mut churn = Vec::new();
    for node in 48..64 {
        churn.push(Churn {
            at_ns: MILLIS,
            node,
            join: false,
        });
        churn.push(Churn {
            at_ns: SECONDS + SECONDS / 2,
            node,
            join: true,
        });
    }
    config.churn = churn;
    config.storms = vec![
        Storm::new(SECONDS, 3, 1, 1),
        Storm::new(3 * SECONDS, 3, 48, 100 * MILLIS),
    ];
    let (world, report) = run_world(config, 7);
    let pages = u64::from(world.config.pages_per_object);
    let per_page: Vec<u32> = (0..pages)
        .map(|p| world.origin_requests(3 * pages + p))
        .collect();
    // Without handoff probes this was 19 (three pages fetched twice).
    assert!(counter(&report, "handoff_probes") > 0, "{report}");
    assert_eq!(counter(&report, "origin_requests"), pages, "{per_page:?}");
    assert_eq!(all_repeats(&report), 0, "{}", report["origin_per_page"]);
}

fn repeats(report: &Value, kind: &str) -> u64 {
    report["origin_per_page"]["repeats"][kind].as_u64().unwrap()
}

/// Repeated origin requests over every class (`NodeWorld::classify_repeat`).
fn all_repeats(report: &Value) -> u64 {
    [
        "after_owner_move",
        "after_failure",
        "concurrent",
        "copy_lost",
        "copy_held",
    ]
    .iter()
    .map(|kind| repeats(report, kind))
    .sum()
}

/// `copy_held` repeats with cause `cause` (`origin_repeats_held_*`).
fn held(report: &Value, cause: &str) -> u64 {
    report["origin_per_page"]["repeats"]["held"][cause]
        .as_u64()
        .unwrap()
}

/// With origin slower than the read deadline, leaders expire while their fills wait on origin, and
/// production cancels an expired fill's origin exchange at the leader's deadline.
#[test]
fn expired_fills_cancel_their_origin_request() {
    let mut config = quiet3(64);
    // Without handover every expired leader's fill drains or is canceled.
    config.node.flight_handover = false;
    config.origin_bps = 1 << 20;
    config.duration_ns = 12 * SECONDS;
    config.storms = (0..4)
        .map(|i| Storm::new(i * 3 * SECONDS, 1, 64, 100 * MILLIS))
        .collect();
    let (_, report) = run_world(config, 5);
    assert!(counter(&report, "flight_stale_finishes") > 0, "{report}");
    assert!(counter(&report, "origin_canceled") > 0, "{report}");
    assert_eq!(counter(&report, "flight_unexpected_lead"), 0);
}

/// With origin slower than the read deadline and readers arriving throughout, an expired leader's
/// fill continues for a later waiter instead of being canceled and restarted.
#[test]
fn expired_leaders_hand_their_fill_to_later_waiters() {
    let report = |handover: bool| {
        let mut config = quiet3(64);
        config.origin_bps = 1 << 20;
        config.duration_ns = 16 * SECONDS;
        config.node.flight_handover = handover;
        config.storms = (0..12)
            .map(|i| Storm::new(i * SECONDS, 1, 16, 500 * MILLIS))
            .collect();
        run_world(config, 7).1
    };
    let (before, after) = (report(false), report(true));
    assert_eq!(counter(&before, "flight_handovers"), 0);
    assert!(counter(&after, "flight_handovers") > 0, "{after}");
    assert!(
        counter(&after, "origin_canceled") < counter(&before, "origin_canceled"),
        "before {} after {}",
        before["metrics"]["counters"],
        after["metrics"]["counters"]
    );
    assert!(
        counter(&after, "origin_requests") < counter(&before, "origin_requests"),
        "before {} after {}",
        before["metrics"]["counters"],
        after["metrics"]["counters"]
    );
    let max = |r: &Value| {
        r["origin_per_page"]["max_requests_per_page"]
            .as_u64()
            .unwrap()
    };
    assert!(
        max(&after) <= max(&before),
        "{} {}",
        max(&before),
        max(&after)
    );
    assert_eq!(counter(&after, "flight_unexpected_lead"), 0);
}

/// Churn runs: nodes adopt publications with lag, lagging receivers answer stale membership,
/// requesters ahead of their destination skip it, owners ask lagging predecessors under the
/// previous membership, and origin repeats are classified and bounded.
#[test]
fn churn_adoption_staleness_and_repeats() {
    // (nodes, retained versions, seed)
    for (nodes, retained, seed) in [
        (120, 2, 1),
        (120, 0, 2),
        (120, 2, 2),
        (300, 2, 1),
        (300, 2, 2),
    ] {
        let (world, report) = run_world(churny(nodes, retained), seed);
        let at = format!("{nodes} {retained} {seed}");
        let c = |name| counter(&report, name);
        if (nodes, seed) == (120, 1) {
            assert!(c("membership_publications") > 1);
            assert!(c("membership_adoptions") > 120);
            assert!(c("placement_maintenance_steps") > 0);
            assert!(c("stale_membership_replies") > 0, "{report}");
            assert!(world.versions.latest().number > 2);
            assert!(
                report["membership"]["placement_memo_entries"]
                    .as_u64()
                    .unwrap()
                    > 0
            );
        }
        if nodes == 120 && matches!((retained, seed), (0, 2) | (2, 1)) {
            assert!(c("stale_destination_skips") > 0, "{at}: {report}");
            assert_eq!(c("error_incompatible_membership"), 0, "{at}: {report}");
            assert_eq!(c("error_hop_budget_exhausted"), 0, "{at}: {report}");
        }
        if retained == 0 {
            // Without retained versions receivers that moved ahead answer stale membership and
            // peers cannot resolve the previous membership (production: no grace retention).
            assert!(c("walk_membership_refreshes") > 0, "{report}");
            assert_eq!(c("previous_membership_probes"), 0);
            continue;
        }
        if (nodes, seed) != (300, 2) {
            assert!(c("previous_membership_probes") > 0, "{at}: {report}");
            assert!(
                c("owner_stale_destination_skips") <= 4,
                "{at}: {}",
                report["metrics"]["counters"]
            );
            assert!(
                repeats(&report, "copy_held") <= 8,
                "{at}: {}",
                report["origin_per_page"]
            );
        }
        if (nodes, seed) != (120, 2) {
            let origin = c("origin_requests");
            let pages = report["origin_per_page"]["pages"].as_u64().unwrap();
            let held = repeats(&report, "copy_held");
            assert_eq!(
                all_repeats(&report),
                origin - pages,
                "{}",
                report["origin_per_page"]
            );
            let causes: u64 = [
                "raced",
                "unreachable",
                "busy",
                "unfunded",
                "missed",
                "unasked",
            ]
            .iter()
            .map(|cause| self::held(&report, cause))
            .sum();
            assert_eq!(causes, held, "{}", report["origin_per_page"]);
            assert!(held * 50 <= origin, "{at}: {}", report["origin_per_page"]);
        }
    }
}

/// With a membership lag as long as the batch period, readers often adopt a version before the
/// page's owners. Each `config.node` stale-handling switch, turned off, brings back the failures it
/// fixes.
#[test]
fn readers_handle_owners_that_are_behind() {
    let with = |f: fn(&mut racer_sim::config::NodeConfig)| {
        let mut config = churny(120, 2);
        config.request_rate = 40.0;
        config.node.membership_lag_ns = config.node.membership_batch_ns;
        f(&mut config.node);
        run_world(config, 3).1
    };
    let on = with(|_| {});
    let exhausted = |r: &Value| counter(r, "walk_exhausted_stale");
    let failed = |r: &Value| counter(r, "reads_failed");
    // A reader that owned the page under the previous membership may not walk it, and waits for
    // a destination that is behind like a reader that joined in the current membership.
    assert!(counter(&on, "stale_fallback_denied") > 0, "{on}");
    assert_eq!(
        counter(&on, "stale_waits_denied"),
        counter(&on, "stale_fallback_denied"),
        "{on}"
    );
    assert_eq!(exhausted(&on), 0, "{on}");
    // A node that rejoined in version N is not a member of N - 1, so it cannot fall back: it waits.
    let off = with(|n| n.stale_wait = false);
    assert_eq!(counter(&off, "stale_waits"), 0);
    assert!(counter(&on, "stale_waits") > 0, "{on}");
    assert!(counter(&off, "stale_fallback_not_member") > 0, "{off}");
    assert!(
        exhausted(&off) > 0 && exhausted(&on) < exhausted(&off),
        "{off}"
    );
    assert!(failed(&on) <= failed(&off));
    // A verified `StaleMembership` returns the links past its answerer.
    let off = with(|n| n.stale_link_refund = false);
    assert!(counter(&on, "stale_links_refunded") > 0, "{on}");
    assert_eq!(counter(&off, "stale_links_refunded"), 0, "{off}");
    assert!(counter(&on, "walk_exhausted") <= counter(&off, "walk_exhausted"));
    assert!(failed(&on) <= failed(&off));
    // Stale owners send non-owners to the previous membership. Measured without waits, and first
    // without the refund, which lets a walk reach the other owners even without the fallback.
    let fallback = |fallback: bool, refund: bool| match (fallback, refund) {
        (false, false) => {
            with(|n| (n.stale_wait, n.stale_fallback, n.stale_link_refund) = (false, false, false))
        }
        (true, false) => with(|n| (n.stale_wait, n.stale_link_refund) = (false, false)),
        (false, true) => with(|n| (n.stale_wait, n.stale_fallback) = (false, false)),
        (true, true) => with(|n| n.stale_wait = false),
    };
    for refund in [false, true] {
        let (off, on) = (fallback(false, refund), fallback(true, refund));
        assert_eq!(counter(&off, "stale_fallbacks"), 0);
        assert!(counter(&on, "stale_fallbacks") > 0, "{on}");
        assert!(exhausted(&off) > 0, "{off}");
        assert!(
            exhausted(&on) * 10 <= exhausted(&off),
            "{} vs {}",
            exhausted(&on),
            exhausted(&off)
        );
        if refund {
            assert!(failed(&on) <= failed(&off));
        } else {
            assert!(failed(&on) * 4 <= failed(&off));
        }
    }
}
