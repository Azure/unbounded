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
/// the requester's attempt deadline, so no page body arrives late and the owners
/// deliver at least half of what their NICs allow during the storm.
///
/// Runs with overflow fan-out off, so only the owners serve the page; see
/// `hot_page_readers_fan_out_through_helpers` for the same storm with it on.
#[test]
fn overloaded_hot_page_owners_do_not_stream_to_abandoned_requesters() {
    let nodes = 96;
    let mut config = quiet(nodes);
    config.node.fanout = false;
    config.pages_per_object = 1;
    config.nic_bps = 4 << 20;
    config.duration_ns = 40 * SECONDS;
    config.storms = vec![storm(0, 0, 1, 1), storm(5 * SECONDS, 0, nodes, MILLIS)];
    let report = run(config.clone(), 5);
    let completed = counter(&report, "reads_completed");
    let failed = counter(&report, "reads_failed");
    assert_eq!(completed + failed, u64::from(nodes) + 1, "{report}");
    assert!(failed > 0, "the storm must overload the owners: {report}");
    // No page body arrives after its requester gave up. Short answers may:
    // an owner above rank 0 whose delegation timed out now fails its
    // waiters with `Unavailable` instead of fetching (Known issue 31), and
    // some of those answers reach readers whose own attempt already ended.
    assert_eq!(counter(&report, "late_page_replies"), 0, "{report}");
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

/// Known issue 16 (round 4 fanout). One warm page read by 512 nodes within
/// a millisecond over 32 MiB/s NICs: in a 2 s attempt the page's 3 owners can
/// send about 192 copies, so without fan-out readers queue on the owners
/// until their attempts time out and some reads fail. With fan-out, owners
/// already streaming `hot_page_bodies` bodies of the page shed further
/// readers with `Overloaded`, and a shed reader asks one of the page's
/// helpers, which the owners always serve. Every read completes, readers get
/// copies from helpers, and the page still reaches the origin once.
#[test]
fn hot_page_readers_fan_out_through_helpers() {
    let nodes = 512;
    let mut config = quiet(nodes);
    config.pages_per_object = 1;
    config.nic_bps = 32 << 20;
    config.duration_ns = 40 * SECONDS;
    config.storms = vec![storm(0, 0, 1, 1), storm(5 * SECONDS, 0, nodes, MILLIS)];
    let on = run(config.clone(), 5);
    config.node.fanout = false;
    // Without fan-out, owners whose flight is full shed readers too; since
    // Known issue 54 a shed reader's retry visits the other owners first,
    // which alone completes every read of this storm (513 of 513). Measure
    // fan-out against the owners-only baseline it was built for.
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

/// Known issue 31 (round 4, overload): with the origin far slower than the
/// demand, rank 0's request for a page queues for seconds. Owners above rank
/// 0 whose `Acquire` to rank 0 (or copy-only probe, which joins rank 0's
/// running acquisition) timed out used to fetch the page themselves, behind
/// rank 0's request at the same origin. They now leave the fetch to rank 0,
/// so no origin request overlaps another node's request for the same page.
#[test]
fn owners_above_rank_zero_do_not_overlap_a_slow_origin_fetch() {
    let nodes = 300;
    let mut config = quiet(nodes);
    config.pages_per_object = 64;
    // 64 MiB of cold pages at 8 MiB/s: rank 0's requests queue for up to
    // 8 s, past the 2 s attempt timeout of the owners delegating to it.
    config.origin_bps = 8 << 20;
    config.duration_ns = 60 * SECONDS;
    // Few enough readers, and room for every waiter, that rank 0's flight
    // never refuses a waiter (an `Overloaded` rank 0 is still transient
    // evidence, Known issue 40). Since owners route their copy-only probe of
    // rank 0 first, more of them reach it and join its flight: one of them
    // found the default cap of 64 full.
    config.node.waiters_per_flight = 128;
    config.storms = vec![storm(0, 0, 48, 100 * MILLIS)];
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
    // Rank 0 is busy, not hung: the owners whose probes timed out ask it
    // whether it answers, and it does (Known issues 41 and 43).
    assert!(counter(&report, "silence_pings") > 0, "{report}");
    assert_eq!(counter(&report, "silence_marks"), 0, "{report}");
    assert_eq!(counter(&report, "silent_destination_fails"), 0, "{report}");
    // Known issue 53 bound: a ping follows a probe timeout (at most one
    // each), two unanswered exchanges over a last link, or a silent peer's
    // recheck; never anything else.
    let pings = counter(&report, "silence_pings");
    let bound = counter(&report, "probe_timeouts")
        + counter(&report, "silence_timeouts")
            / u64::from(racer_core::peer::silence::LINK_TIMEOUTS)
        + counter(&report, "silence_rechecks");
    assert!(pings <= bound, "{pings} pings, bound {bound}: {report}");
}

/// Known issue 18 (round 4, relay): a relay forwarding a body still queued at
/// a busy owner reserved its own NIC at the body's future send time, so its
/// other sends queued behind the gap, backlogs spread from relay to relay and
/// relays held their `Relay` reservations for hundreds of milliseconds (5k
/// `zipf`: 339k relay rejections, 4,373 failed reads). With transmit modeled
/// as a work-conserving backlog, a relayed 1 MiB body streams in about its
/// links' latency plus wire time: at most two links of (12.5 ms one way
/// + 0.84 ms at 10 Gbit/s) here. Before the fix this run's p99 was 42 ms.
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

/// Known issue 39 (round 4, hang): an owner above rank 0 leaves the fetch to
/// a rank 0 whose probe timed out (Known issue 31), so a rank 0 whose peer
/// server accepts requests and never answers used to block its pages for as
/// long as it stayed a member. Ten of 200 members hang for the whole run; a
/// storm reads a cold object at 0 s and again at 30 s. The owners above a
/// hung rank 0 now fetch once it stayed silent for half a request timeout
/// (5 s) after their first probe timed out (`Rank0Watch`), so the second storm
/// reads almost everything; without the watch it fails again on the same
/// pages. The one read the second storm
/// still fails visits a hung rank 2 first and then reaches rank 1 through a
/// hung relay, twice (Known issue 41: a reader does not learn that a peer
/// hangs).
#[test]
fn a_hung_rank_zero_blocks_its_pages_for_at_most_one_request_timeout() {
    // Failed reads of members whose own peer server does not hang (a hung
    // reader loses the answers to its own requests).
    let failed = |m: &racer_sim::report::Metrics| {
        m.get("reads_failed") - m.get("reads_failed_on_hung_nodes")
    };
    let run_hang = |watch: bool, silence: bool| {
        let mut config = quiet(200);
        config.duration_ns = 45 * SECONDS;
        config.pages_per_object = 64;
        config.node.rank0_watch = watch;
        config.node.silence = silence;
        config.faults = (0..10)
            .map(|node| Fault::Hang {
                node: node * 20,
                from_ns: 0,
                until_ns: config.duration_ns,
            })
            .collect();
        config.storms = vec![
            storm(0, 0, 100, 100 * MILLIS),
            storm(30 * SECONDS, 0, 100, 100 * MILLIS),
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
    };
    // The watch alone (silence off, Known issues 41 and 43 are measured in
    // `a_hung_peer_is_found_by_a_ping_and_shared_through_its_links`).
    let (early, late, report) = run_hang(true, false);
    assert!(
        counter(&report, "rank0_watch_overdue_walks") > 0,
        "{report}"
    );
    assert!(
        late <= 1,
        "second storm still fails {late} (first: {early}): {report}"
    );
    let (old_early, old_late, _) = run_hang(false, false);
    assert!(
        old_late >= 100 && old_late > 50 * late,
        "without the watch: {old_early} early, {old_late} late"
    );
}

/// Known issues 41 and 43 (round 4, hang2): each owner above a hung rank 0
/// learned on its own, after half a request timeout, that rank 0 hangs, and
/// readers never learned that a peer hangs, so the first storm on pages whose
/// rank 0 hangs failed almost every read. A sender whose exchange timed out
/// now pings the destination (mode `ping`, answered at once): the owner
/// pings its rank 0, and the sender of the last link pings its neighbor. A
/// silent peer fails requests for it at once at every node that holds it
/// silent, so owners take authority and readers move on without their own
/// wait. Same configuration as
/// `a_hung_rank_zero_blocks_its_pages_for_at_most_one_request_timeout`: both
/// storms now read every page, with or without the rank 0 watch, and the
/// watch alone still fails the first storm.
#[test]
fn a_hung_peer_is_found_by_a_ping_and_shared_through_its_links() {
    let failed = |m: &racer_sim::report::Metrics| {
        m.get("reads_failed") - m.get("reads_failed_on_hung_nodes")
    };
    let run_hang = |watch: bool, silence: bool| {
        let mut config = quiet(200);
        config.duration_ns = 45 * SECONDS;
        config.pages_per_object = 64;
        config.node.rank0_watch = watch;
        config.node.silence = silence;
        config.faults = (0..10)
            .map(|node| Fault::Hang {
                node: node * 20,
                from_ns: 0,
                until_ns: config.duration_ns,
            })
            .collect();
        config.storms = vec![
            storm(0, 0, 100, 100 * MILLIS),
            storm(30 * SECONDS, 0, 100, 100 * MILLIS),
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
        (early, late, report)
    };
    for watch in [true, false] {
        let (early, late, report) = run_hang(watch, true);
        assert!(
            early <= 2 && late <= 1,
            "watch {watch}: {early} early, {late} late: {report}"
        );
        assert!(counter(&report, "silence_marks") > 0, "{report}");
        assert!(counter(&report, "silent_destination_fails") > 0, "{report}");
    }
    let (early, _, report) = run_hang(true, false);
    assert!(early >= 50, "the watch alone: {early} early: {report}");
    assert_eq!(counter(&report, "silence_pings"), 0, "{report}");
}

/// Known issue 43: a recovery ping tests the link to the peer its sender
/// holds silent. Silent neighbors are routed around, but an origin's own ping
/// is not, so a direct ping does not detour or find no route. Peers hang for
/// the first 10 s only; their senders learn that they recovered from a ping
/// and the second storm reads every page.
#[test]
fn a_recovered_peer_answers_its_recovery_ping() {
    let mut config = quiet(200);
    config.duration_ns = 45 * SECONDS;
    config.pages_per_object = 64;
    config.faults = (0..10)
        .map(|node| Fault::Hang {
            node: node * 20,
            from_ns: 0,
            until_ns: 10 * SECONDS,
        })
        .collect();
    config.storms = vec![
        storm(0, 0, 100, 100 * MILLIS),
        storm(30 * SECONDS, 0, 100, 100 * MILLIS),
    ];
    let mut world = NodeWorld::new(config, 5);
    world.run(29 * SECONDS);
    let early = world.metrics.get("reads_failed");
    let end = world.config.duration_ns;
    world.run(end);
    let late = world.metrics.get("reads_failed") - early;
    let report = world.report("contention", 5);
    assert_eq!(report["invariants"]["hard_failures"], 0, "{report}");
    assert!(counter(&report, "silence_marks") > 0, "{report}");
    assert!(counter(&report, "silence_recoveries") > 0, "{report}");
    assert!(counter(&report, "silence_pings_direct") > 0, "{report}");
    assert_eq!(late, 0, "{report}");
}

/// Known issue 54 (round 4, busy): an owner that sheds a reader (a signed
/// `Overloaded`, here a hot page's owner) is busy, not hung. The reader's
/// later walks visit it after the page's other owners for half a request
/// timeout, so a retried read does not repeat the walk that was shed
/// (`Spread` is deterministic per requester and page). In the 5k `zipf` run
/// at 16 MiB pages every failed read was such a repeat: shed by rank 0, its
/// helper unable to reach rank 0, four times.
#[test]
fn readers_do_not_return_first_to_an_owner_that_shed_them() {
    let nodes = 512;
    let mut config = quiet(nodes);
    config.pages_per_object = 1;
    config.nic_bps = 32 << 20;
    config.duration_ns = 40 * SECONDS;
    config.storms = vec![
        storm(0, 0, 1, 1),
        storm(5 * SECONDS, 0, nodes, MILLIS),
        storm(7 * SECONDS, 0, nodes, MILLIS),
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
