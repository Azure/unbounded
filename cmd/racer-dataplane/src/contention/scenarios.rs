//! Scenario-level oracles for the metadata-only contention model.
use super::*;

fn small_config() -> Config {
    Config {
        nodes: 4,
        workers: 2,
        pages_per_worker: 16,
        dirty_pages: 4,
        pipes: 16,
        queue_entries: 64,
        max_attempts: 32,
        ..Config::default()
    }
}

fn requested_bytes(requests: &[Request]) -> u64 {
    requests.iter().map(|r| r.pages as u64 * PAGE_BYTES).sum()
}

fn assert_drained(config: &Config, requests: &[Request], report: &Report) {
    assert_eq!(report.submitted, requests.len(), "{report:?}");
    assert_eq!(
        report.completed + report.failed + report.canceled,
        report.submitted,
        "every request must have exactly one terminal outcome: {report:?}"
    );
    assert_eq!(report.final_used, [0; 11], "owners leaked: {report:?}");
    assert!(
        report.events < config.max_events,
        "event budget: {report:?}"
    );
    assert!(report.peak_events <= report.events, "{report:?}");
    assert!(report.peak_requests <= requests.len(), "{report:?}");
    assert!(
        report.delivered_bytes <= requested_bytes(requests),
        "delivery duplicated requested bytes: {report:?}"
    );
    // These are per-authority peaks, not fleet totals. Adding workers or nodes
    // must never multiply the amount a single admission authority can own.
    for (class, limit) in [
        (ResourceClass::Plaintext, config.pages_per_worker * PLAIN),
        (ResourceClass::Ciphertext, config.pages_per_worker * CIPHER),
        (ResourceClass::DirtyCiphertext, config.dirty_pages * CIPHER),
        (ResourceClass::Pipe, config.pipes),
    ] {
        assert!(
            report.peak_worker[class as usize] <= limit,
            "{class:?} exceeded per-worker limit {limit}: {report:?}"
        );
    }
}

fn assert_success(config: &Config, requests: &[Request], report: &Report) {
    assert_drained(config, requests, report);
    assert_eq!(report.completed, requests.len(), "{report:?}");
    assert_eq!(
        report.delivered_bytes,
        requested_bytes(requests),
        "{report:?}"
    );
    assert_eq!(report.latency_ticks.len(), requests.len(), "{report:?}");
}

fn compare_success(configs: [&Config; 2], requests: &[Request]) -> [Report; 2] {
    configs.map(|config| {
        let report = run(config, requests);
        assert_success(config, requests, &report);
        report
    })
}

fn run(config: &Config, requests: &[Request]) -> Report {
    let mut sim = Simulator::new(config.clone());
    sim.run(requests.to_vec())
}

fn total_latency(report: &Report) -> u64 {
    report.latency_ticks.iter().sum()
}

#[test]
fn overlapping_ranges_join_cold_fills_and_deliver_each_read() {
    let config = Config {
        window: 4,
        ..small_config()
    };
    let mut overlap = Request::new(0, 0);
    overlap.first_page = 2;
    let requests = vec![Request::new(0, 0), overlap, Request::new(0, 0)];
    let report = run(&config, &requests);

    assert_success(&config, &requests, &report);
    assert!(report.joined > 0, "cold readers did not join: {report:?}");
    // Twelve page deliveries cover only six different page identities. There
    // is enough room to retain the entire union without eviction pressure.
    assert!(report.fills >= 6 && report.fills < 12, "{report:?}");
    assert_eq!(report.evictions, 0, "{report:?}");
}

#[test]
fn identical_object_numbers_in_distinct_caches_do_not_singleflight() {
    let config = small_config();
    let mut first = Request::new(0, 0);
    first.pages = 2;
    let mut second = first.clone();
    second.cache = 1;
    let requests = vec![first, second];
    let report = run(&config, &requests);

    assert_success(&config, &requests, &report);
    assert_eq!(report.fills, 4, "cache identity was aliased: {report:?}");
    assert_eq!(report.joined, 0, "cache identity was aliased: {report:?}");
}

#[test]
fn slow_reader_keeps_a_bounded_window_and_eventually_makes_progress() {
    let config = Config {
        nodes: 1,
        workers: 1,
        pages_per_worker: 4,
        window: 2,
        ..small_config()
    };
    let mut request = Request::new(0, 0);
    request.pages = 12;
    let fast_requests = vec![request.clone()];
    let fast = run(&config, &fast_requests);
    request.reader_bytes_per_tick = Some(PAGE_BYTES / 256);
    let slow_requests = vec![request];
    let slow = run(&config, &slow_requests);

    assert_success(&config, &fast_requests, &fast);
    assert_success(&config, &slow_requests, &slow);
    assert!(total_latency(&slow) > total_latency(&fast), "{slow:?}");
    assert!(
        slow.evictions > 0,
        "range never reclaimed idle pages: {slow:?}"
    );
    assert!(
        slow.peak_worker[ResourceClass::Plaintext as usize]
            < requested_bytes(&slow_requests) as usize,
        "entire range was retained instead of windowed: {slow:?}"
    );
}

#[test]
fn larger_request_window_overlaps_fill_and_reader_service() {
    let narrow_config = Config {
        nodes: 1,
        workers: 1,
        pages_per_worker: 16,
        window: 1,
        reader_bytes_per_tick: PAGE_BYTES / 64,
        ..small_config()
    };
    let wide_config = Config {
        window: 4,
        ..narrow_config.clone()
    };
    let mut request = Request::new(0, 0);
    request.pages = 12;
    let requests = vec![request];
    let [narrow, wide] = compare_success([&narrow_config, &wide_config], &requests);
    assert_eq!(wide.fills, narrow.fills, "window changed page identity");
    assert!(
        total_latency(&wide) < total_latency(&narrow),
        "a larger window did not overlap service: narrow={narrow:?}, wide={wide:?}"
    );
}

#[test]
fn slow_disk_sheds_persistence_without_losing_reader_progress() {
    let fast_config = Config {
        nodes: 1,
        workers: 1,
        pages_per_worker: 32,
        dirty_pages: 1,
        window: 4,
        disk_bytes_per_tick: PAGE_BYTES * 4,
        ..small_config()
    };
    let slow_config = Config {
        disk_bytes_per_tick: PAGE_BYTES / 1024,
        ..fast_config.clone()
    };
    let mut request = Request::new(0, 0);
    request.pages = 24;
    let requests = vec![request];
    let [fast, slow] = compare_success([&fast_config, &slow_config], &requests);
    assert!(slow.disk_bytes > 0, "disk path was not exercised: {slow:?}");
    assert!(
        slow.persistence_skips > fast.persistence_skips,
        "slow dirty owners must increase optional persistence pressure: fast={fast:?}, slow={slow:?}"
    );
    assert!(
        slow.disk_bytes < fast.disk_bytes,
        "fast={fast:?}, slow={slow:?}"
    );
}

#[test]
fn cache_churn_reclaims_idle_bundles_and_allows_a_new_cache_to_progress() {
    let config = Config {
        nodes: 1,
        workers: 1,
        pages_per_worker: 8,
        window: 2,
        ..small_config()
    };
    let mut warm = Request::new(0, 0);
    warm.pages = 8;
    let mut churn = Request::new(0, 1);
    churn.at = 2048;
    churn.pages = 12;
    let mut newcomer = Request::new(0, 2);
    newcomer.at = churn.at;
    newcomer.cache = 1;
    newcomer.pages = 2;
    let mut revisit = Request::new(0, 0);
    revisit.at = 8192;
    let requests = vec![warm, churn, newcomer, revisit];
    let report = run(&config, &requests);

    // Success for every request includes the newcomer while the old cache is
    // still active, and the old cache's later read after fair-share reclamation.
    assert_success(&config, &requests, &report);
    assert!(
        report.evictions > 0,
        "idle bundles were never reclaimed: {report:?}"
    );
    assert!(report.fills > config.pages_per_worker, "{report:?}");
}

#[test]
fn workers_share_node_network_bandwidth() {
    let fast_config = Config {
        nodes: 1,
        workers: 4,
        window: 1,
        network_bytes_per_tick: PAGE_BYTES,
        origin_bytes_per_tick: PAGE_BYTES * 64,
        disk_bytes_per_tick: PAGE_BYTES * 64,
        reader_bytes_per_tick: PAGE_BYTES * 64,
        ..small_config()
    };
    let slow_config = Config {
        network_bytes_per_tick: PAGE_BYTES / 64,
        ..fast_config.clone()
    };
    let requests: Vec<_> = (0..4)
        .map(|object| {
            let mut request = Request::new(0, object);
            request.pages = 1;
            request
        })
        .collect();
    let [fast, slow] = compare_success([&fast_config, &slow_config], &requests);
    assert!(total_latency(&slow) > total_latency(&fast), "{slow:?}");
    // Each object maps to a different worker, but all four inbound pages must
    // cross the same node NIC. Independent worker NICs violate this floor.
    let nic_ticks = requested_bytes(&requests).div_ceil(slow_config.network_bytes_per_tick);
    assert!(
        slow.latency_ticks.iter().copied().max().unwrap() >= nic_ticks,
        "worker count multiplied node bandwidth: {slow:?}"
    );
}

#[test]
fn cold_nodes_contend_for_one_global_origin_queue() {
    let fast_config = Config {
        nodes: 8,
        workers: 1,
        network_bytes_per_tick: PAGE_BYTES * 64,
        disk_bytes_per_tick: PAGE_BYTES * 64,
        reader_bytes_per_tick: PAGE_BYTES * 64,
        origin_bytes_per_tick: PAGE_BYTES * 64,
        ..small_config()
    };
    let slow_config = Config {
        origin_bytes_per_tick: PAGE_BYTES / 64,
        ..fast_config.clone()
    };
    let requests: Vec<_> = (0..fast_config.nodes)
        .map(|node| {
            let mut request = Request::new(node, node as u64);
            request.pages = 1;
            request
        })
        .collect();
    let [fast, slow] = compare_success([&fast_config, &slow_config], &requests);
    assert!(slow.origin_bytes >= requested_bytes(&requests), "{slow:?}");
    assert!(total_latency(&slow) > total_latency(&fast), "{slow:?}");
    let origin_ticks = requested_bytes(&requests).div_ceil(slow_config.origin_bytes_per_tick);
    assert!(
        slow.latency_ticks.iter().copied().max().unwrap() >= origin_ticks,
        "origin bandwidth was multiplied by node count: {slow:?}"
    );
}

#[test]
fn warm_peer_fanout_is_limited_by_source_node_network() {
    let config = Config {
        nodes: 9,
        workers: 1,
        network_bytes_per_tick: PAGE_BYTES / 64,
        origin_bytes_per_tick: PAGE_BYTES * 64,
        disk_bytes_per_tick: PAGE_BYTES * 64,
        reader_bytes_per_tick: PAGE_BYTES * 64,
        window: 1,
        ..small_config()
    };
    let mut warm = Request::new(0, 0);
    warm.pages = 1;
    let mut requests = vec![warm];
    for node in 1..config.nodes {
        let mut request = Request::new(node, 0);
        request.at = 4096;
        request.pages = 1;
        requests.push(request);
    }
    let report = run(&config, &requests);

    assert_success(&config, &requests, &report);
    let destinations = (config.nodes - 1) as u64;
    assert_eq!(report.origin_bytes, CIPHER as u64, "{report:?}");
    assert_eq!(
        report.peer_bytes,
        destinations * CIPHER as u64,
        "fanout must fetch every copy from the warm peer: {report:?}"
    );
    // Node 0 is warm and idle before the simultaneous arrivals. Every destination
    // schedules its single fill before any fill completes, so all copies use
    // node 0 and must serialize on its NIC, despite having distinct destination NICs.
    let source_ticks = destinations * (CIPHER as u64).div_ceil(config.network_bytes_per_tick);
    // Eight ciphertext transfers require 520 ticks. Without source serialization,
    // each peer read takes only 65 + 64 + 2 * 2 = 133 ticks (fill, send, completions),
    // and even the initial origin-backed warmup takes only 134 ticks.
    assert!(
        report.latency_ticks.iter().copied().max().unwrap() >= source_ticks,
        "fanout bypassed the cumulative source NIC bound of {source_ticks} ticks: {report:?}"
    );
}

#[test]
fn partition_retries_recover_after_heal() {
    let config = small_config();
    let requests = vec![Request::new(0, 0), Request::new(1, 1)];
    let healthy = run(&config, &requests);
    let mut sim = Simulator::new(config.clone());
    sim.partition(0, 0, 256);
    let healed = sim.run(requests.clone());

    assert_success(&config, &requests, &healthy);
    assert_success(&config, &requests, &healed);
    assert!(
        healed.retries > healthy.retries,
        "partition had no effect: {healed:?}"
    );
    assert!(
        total_latency(&healed) > total_latency(&healthy),
        "{healed:?}"
    );
}

#[test]
fn retry_exhaustion_is_bounded_and_does_not_poison_later_requests() {
    let config = Config {
        max_attempts: 2,
        ..small_config()
    };
    let mut blocked = Request::new(0, 0);
    blocked.pages = 1;
    let mut recovery = Request::new(0, 1);
    recovery.at = 4096;
    let requests = vec![blocked, recovery];
    let mut sim = Simulator::new(config.clone());
    sim.partition(0, 0, 2048);
    let report = sim.run(requests.clone());

    assert_drained(&config, &requests, &report);
    assert_eq!(
        report.failed, 1,
        "partitioned request did not exhaust retries: {report:?}"
    );
    assert_eq!(
        report.completed, 1,
        "post-heal request could not progress: {report:?}"
    );
    assert_eq!(report.canceled, 0, "{report:?}");
    assert!(
        report.retries > 0 && report.retries <= config.max_attempts,
        "{report:?}"
    );
    assert_eq!(
        report.delivered_bytes,
        requests[1].pages as u64 * PAGE_BYTES,
        "{report:?}"
    );
}

#[test]
fn cancellation_and_deadline_drain_inflight_owners_before_recovery() {
    for cancel in [true, false] {
        let config = Config {
            nodes: 1,
            workers: 1,
            pages_per_worker: 2,
            dirty_pages: 1,
            window: 1,
            network_bytes_per_tick: PAGE_BYTES / 64,
            disk_bytes_per_tick: PAGE_BYTES / 256,
            completion_ticks: 128,
            ..small_config()
        };
        let mut interrupted = Request::new(0, 0);
        interrupted.pages = 1;
        if cancel {
            interrupted.cancel_after = Some(8);
        } else {
            interrupted.deadline = 8;
        }
        let interrupted_requests = vec![interrupted.clone()];
        let interrupted_report = run(&config, &interrupted_requests);
        assert_drained(&config, &interrupted_requests, &interrupted_report);
        assert_eq!(interrupted_report.completed, 0, "{interrupted_report:?}");
        assert_eq!(
            interrupted_report.canceled,
            usize::from(cancel),
            "{interrupted_report:?}"
        );
        assert_eq!(
            interrupted_report.failed,
            usize::from(!cancel),
            "{interrupted_report:?}"
        );
        assert!(
            interrupted_report.fills > 0,
            "no in-flight fill: {interrupted_report:?}"
        );
        assert!(
            interrupted_report.end_tick >= config.completion_ticks,
            "request termination prematurely dropped scheduled ownership: {interrupted_report:?}"
        );

        let mut recovery = Request::new(0, 1);
        recovery.at = 4096;
        let requests = vec![interrupted, recovery];
        let report = run(&config, &requests);
        assert_drained(&config, &requests, &report);
        assert_eq!(
            report.completed, 1,
            "recovery failed, cancel={cancel}: {report:?}"
        );
        assert_eq!(report.canceled, usize::from(cancel), "{report:?}");
        assert_eq!(report.failed, usize::from(!cancel), "{report:?}");
        assert_eq!(
            report.delivered_bytes,
            requests[1].pages as u64 * PAGE_BYTES,
            "{report:?}"
        );
    }
}

#[test]
fn canceling_one_joined_reader_preserves_the_surviving_read() {
    let config = Config {
        nodes: 1,
        workers: 1,
        pages_per_worker: 4,
        window: 2,
        ..small_config()
    };
    let mut canceled = Request::new(0, 0);
    canceled.cancel_after = Some(8);
    let requests = vec![canceled, Request::new(0, 0)];
    let report = run(&config, &requests);

    assert_drained(&config, &requests, &report);
    assert_eq!(report.canceled, 1, "{report:?}");
    assert_eq!(
        report.completed, 1,
        "surviving reader lost its fill: {report:?}"
    );
    assert_eq!(report.failed, 0, "{report:?}");
    assert!(report.joined > 0, "no shared flight to cancel: {report:?}");
    assert!(
        report.fills < 8,
        "shared cold pages were fetched twice: {report:?}"
    );
    assert_eq!(report.delivered_bytes, 4 * PAGE_BYTES, "{report:?}");
}

// A fixed integer generator avoids external RNG dependencies and host entropy.
// Its state is local to each workload, so test execution order cannot affect it.
fn next_random(state: &mut u64) -> u64 {
    *state = state.wrapping_add(0x9e37_79b9_7f4a_7c15);
    let mut value = *state;
    value = (value ^ (value >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
    value = (value ^ (value >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
    value ^ (value >> 31)
}

fn generated_requests(seed: u64, config: &Config) -> Vec<Request> {
    let mut state = seed;
    let mut requests = Vec::new();
    for index in 0..96 {
        let mut request = Request::new(
            (next_random(&mut state) % config.nodes as u64) as usize,
            next_random(&mut state) % 8,
        );
        request.at = (index / 16) * 512 + next_random(&mut state) % 64;
        request.cache = (next_random(&mut state) % 3) as usize;
        request.first_page = next_random(&mut state) % 4;
        request.pages = 1 + (next_random(&mut state) % 4) as usize;
        if next_random(&mut state).is_multiple_of(4) {
            request.reader_bytes_per_tick = Some(PAGE_BYTES / 128);
        }
        match next_random(&mut state) % 8 {
            0 => request.cancel_after = Some(8),
            1 => request.deadline = 16,
            _ => {}
        }
        requests.push(request);
    }
    // A quiet recovery wave probes every node and worker after generated
    // pressure, cancellations, deadlines, and partitions have all subsided.
    for node in 0..config.nodes {
        for worker in 0..config.workers {
            let mut request = Request::new(node, 1024 * config.workers as u64 + worker as u64);
            request.at = 65_536;
            request.pages = 2;
            requests.push(request);
        }
    }
    requests
}

fn run_generated(config: &Config, requests: &[Request]) -> Report {
    let mut sim = Simulator::new(config.clone());
    sim.partition(0, 0, 256);
    sim.partition(config.nodes - 1, 1024, 1536);
    sim.run(requests.to_vec())
}

#[test]
fn generated_seed_matrix_replays_identical_reports_and_drains() {
    let mut traces = Vec::new();
    for seed in [1, 7, 42, 0xdead_beef] {
        let config = Config {
            nodes: 8,
            workers: 1 + (seed as usize % 2),
            pages_per_worker: 8,
            dirty_pages: 2,
            max_attempts: 8,
            ..small_config()
        };
        let requests = generated_requests(seed, &config);
        let report = run_generated(&config, &requests);
        let replay_requests = generated_requests(seed, &config);
        let replay = run_generated(&config, &replay_requests);
        let recovery_count = config.nodes * config.workers;
        let pressure_requests = &requests[..requests.len() - recovery_count];
        let pressure = run_generated(&config, pressure_requests);

        assert_eq!(report, replay, "replay diverged for seed={seed}");
        assert_drained(&config, &requests, &report);
        assert_drained(&config, pressure_requests, &pressure);
        assert_eq!(
            report.completed,
            pressure.completed + recovery_count,
            "recovery wave failed, seed={seed}: {report:?}"
        );
        assert_eq!(
            report.failed, pressure.failed,
            "recovery added failures, seed={seed}"
        );
        assert_eq!(
            report.canceled, pressure.canceled,
            "recovery added cancellations, seed={seed}"
        );
        assert_eq!(
            report.delivered_bytes,
            pressure.delivered_bytes + recovery_count as u64 * 2 * PAGE_BYTES,
            "recovery delivery mismatch, seed={seed}"
        );
        assert!(
            report.fills > 0 && report.delivered_bytes > 0,
            "seed={seed}: {report:?}"
        );
        assert!(
            report.failed + report.canceled > 0,
            "faults had no effect, seed={seed}: {report:?}"
        );
        assert!(
            report.evictions > 0,
            "no cache pressure, seed={seed}: {report:?}"
        );
        traces.push(report.trace_hash);
    }
    assert!(
        traces.windows(2).any(|pair| pair[0] != pair[1]),
        "distinct seeds produced identical traces"
    );
}

#[test]
fn two_thousand_nodes_keep_per_worker_bounds_under_cold_start() {
    let config = Config {
        nodes: 2000,
        workers: 2,
        pages_per_worker: 8,
        max_attempts: 16,
        retry_ticks: 256,
        ..Config::default()
    };
    let mut requests = Vec::with_capacity(config.nodes * config.workers);
    for node in 0..config.nodes {
        for worker in 0..config.workers {
            // Distinct objects eliminate accidental fleet-wide warm hits while
            // object modulo workers drives both authorities on every node.
            let object = (node * config.workers + worker) as u64;
            let mut request = Request::new(node, object);
            request.at = (node % 16) as u64;
            requests.push(request);
        }
    }
    let report = run(&config, &requests);

    assert_success(&config, &requests, &report);
    assert!(
        report.peak_requests >= config.nodes,
        "no fleet overlap: {report:?}"
    );
    assert!(
        report.origin_bytes >= requested_bytes(&requests),
        "{report:?}"
    );
    assert!(
        report.disk_bytes > 0,
        "cold fills did not reach disk: {report:?}"
    );
    assert!(
        report.peak_logical_bytes > (config.pages_per_worker * PLAIN) as u64,
        "fleet pressure never exceeded one worker: {report:?}"
    );
}

// Model mechanics: queue bounds, cancellation, and scheduled-owner edge cases.
//
// These tests drive Simulator events and inspect its accounting, not production
// I/O or completion fences. Allocation-backed comparisons live in fidelity.rs;
// production queue behavior belongs to the memory/runtime and app scenarios.
// Keep event ordering and terminal accounting here even when fidelity tests
// compare the same resource owners: those comparisons do not run this scheduler.

fn global_capacity_config() -> Config {
    Config {
        nodes: 1,
        workers: 1,
        pages_per_worker: 16,
        dirty_pages: 16,
        pipes: 6,
        max_events: 1_000,
        ..Config::default()
    }
}

#[test]
fn two_live_caches_can_use_global_transport_capacity_until_exhaustion() {
    for class in [ResourceClass::Pipe, ResourceClass::Connection] {
        assert_global_capacity(class);
    }
}

fn assert_global_capacity(class: ResourceClass) {
    // Transport leases are node-wide; live caches must not impose fair-share caps.
    let mut sim = Simulator::new(global_capacity_config());
    let contexts = [0, 1].map(|cache| {
        sim.reserve(0, cache, ResourceClass::RequestContext, CONTEXT)
            .unwrap()
    });
    let limit = sim.workers[0].admission.limit(class);
    let mut leases = Vec::new();
    for slot in 0..limit {
        // Both caches own transport leases, but cache 0 exceeds half the
        // capacity while cache 1 remains live with ample context quota.
        let cache = usize::from(slot == 0);
        leases.push(sim.reserve(0, cache, class, 1).unwrap_or_else(|| {
            panic!("{class:?} falsely rejected at {slot}/{limit} global usage")
        }));
    }
    assert!(leases.iter().all(|lease| lease.key().is_none()));
    assert_eq!(sim.report.rejections[class as usize], 0);
    assert_eq!(sim.workers[0].admission.used(class), limit);
    for cache in [0, 1] {
        assert!(sim.reserve(0, cache, class, 1).is_none());
    }
    assert_eq!(sim.workers[0].admission.used(class), limit);
    drop(leases.pop());
    let reused = sim.reserve(0, 1, class, 1).unwrap();
    assert_eq!(sim.workers[0].admission.used(class), limit);
    drop((leases, reused, contexts));
    assert!(
        CLASSES
            .iter()
            .all(|class| sim.workers[0].admission.used(*class) == 0)
    );
}

#[test]
fn two_caches_with_spare_pipes_do_not_park_readers() {
    let mut sim = Simulator::new(global_capacity_config());
    let requests = (0..5)
        .map(|object| Request {
            cache: usize::from(object == 0),
            pages: 1,
            ..Request::new(0, object)
        })
        .collect();
    let report = sim.run(requests);
    assert_eq!(report.completed, 5, "{report:?}");
    assert_eq!((report.failed, report.canceled, report.retries), (0, 0, 0));
    assert_eq!(report.rejections, [0; 11], "{report:?}");
    assert_eq!(report.peak_worker[ResourceClass::Pipe as usize], 5);
    assert_eq!(report.peak_worker[ResourceClass::Connection as usize], 5);
    assert_eq!(report.delivered_bytes, 5 * PAGE_BYTES);
    assert_eq!(report.final_used, [0; 11]);
}

#[test]
fn cache_scoped_resources_still_enforce_fairness_with_global_room() {
    for (class, amount) in [
        (ResourceClass::Plaintext, PLAIN),
        (ResourceClass::Ciphertext, CIPHER),
        (ResourceClass::DirtyCiphertext, CIPHER),
        (ResourceClass::Registered, CIPHER),
        (ResourceClass::RequestContext, CONTEXT),
        (ResourceClass::Flight, 1),
        (ResourceClass::Waiter, 1),
    ] {
        let mut sim = Simulator::new(global_capacity_config());
        let limit = sim.workers[0].admission.limit(class);
        let first = sim.reserve(0, 0, class, limit / 2).unwrap();
        let second = sim.reserve(0, 1, class, amount).unwrap();
        assert_eq!(first.key(), Some(&CacheId("0".into())));
        assert_eq!(second.key(), Some(&CacheId("1".into())));
        assert!(sim.workers[0].admission.used(class) + amount <= limit);
        assert!(sim.reserve(0, 0, class, amount).is_none(), "{class:?}");
        assert_eq!(sim.report.rejections[class as usize], 1);
        drop((first, second));
        assert_eq!(sim.workers[0].admission.used(class), 0);
    }
}

fn config() -> Config {
    Config {
        nodes: 1,
        workers: 1,
        pages_per_worker: 2,
        dirty_pages: 1,
        pipes: 1,
        queue_entries: 3,
        window: 1,
        network_bytes_per_tick: PAGE_BYTES * 2,
        origin_bytes_per_tick: PAGE_BYTES * 2,
        disk_bytes_per_tick: PAGE_BYTES * 2,
        reader_bytes_per_tick: PAGE_BYTES / 64,
        ..Config::default()
    }
}

fn request() -> Request {
    let mut request = Request::new(0, 0);
    request.pages = 1;
    request
}

fn assert_outcomes(report: &Report, completed: usize, failed: usize, canceled: usize) {
    assert_eq!(
        report.submitted,
        completed + failed + canceled,
        "{report:?}"
    );
    assert_eq!(
        report.outcomes(),
        (completed, failed, canceled),
        "{report:?}"
    );
    assert_eq!(report.delivered_bytes, completed as u64 * PAGE_BYTES);
    assert_eq!(report.latency_ticks.len(), completed);
    assert_eq!(report.final_used, [0; 11]);
}

fn request_ownership(sim: &Simulator) -> [usize; 3] {
    [
        ResourceClass::RequestContext,
        ResourceClass::Connection,
        ResourceClass::Pipe,
    ]
    .map(|class| sim.workers[0].admission.used(class))
}

fn pump_next(sim: &mut Simulator, expected: usize) {
    let ((at, _), event) = sim.events.pop_first().unwrap();
    assert_eq!(at, sim.now);
    assert!(matches!(event, Event::Pump(id) if id == expected));
    sim.pump(expected);
}

#[test]
fn same_tick_newer_pump_cannot_bypass_a_notified_head() {
    for pipes in [1, 2] {
        let mut sim = Simulator::new(Config { pipes, ..config() });
        let held: Vec<_> = (0..pipes)
            .map(|_| sim.reserve(0, 0, ResourceClass::Pipe, 1).unwrap())
            .collect();
        for id in 1..=2 {
            sim.arrive(id, request());
            pump_next(&mut sim, id);
        }

        drop(held);
        // The newcomer already has a pump scheduled ahead of the release wake.
        sim.arrive(3, request());
        sim.wake_pipe(0);
        assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([1, 2]));
        assert!(sim.active[&1].queued);
        pump_next(&mut sim, 3);
        assert!(sim.active[&3].pipe.is_none());
        assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([1, 2, 3]));

        pump_next(&mut sim, 1);
        assert!(sim.active[&1].pipe.is_some());
        assert!(!sim.active[&1].queued);
        assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([2, 3]));
        if pipes == 1 {
            // Return the only pipe to let the second waiter make progress.
            drop(sim.active.get_mut(&1).unwrap().pipe.take());
            sim.wake_pipe(0);
        }
        // With two free pipes, acquiring the head must notify its successor.
        pump_next(&mut sim, 2);
        assert!(sim.active[&2].pipe.is_some());
        assert!(sim.active[&3].pipe.is_none());
        assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([3]));
    }
}

#[test]
fn cancellation_removes_every_queue_position_before_delayed_release() {
    for canceled in 1..=3 {
        let mut sim = Simulator::new(config());
        let held = sim.reserve(0, 0, ResourceClass::Pipe, 1).unwrap();
        for id in 1..=3 {
            sim.arrive(id, request());
            pump_next(&mut sim, id);
        }

        let active = sim.active.remove(&canceled).unwrap();
        sim.release(canceled, active);
        let survivors: VecDeque<_> = (1..=3).filter(|id| *id != canceled).collect();
        assert_eq!(sim.workers[0].pipe_queue, survivors);
        assert_eq!(request_ownership(&sim), [3 * CONTEXT, 3, 1]);
        assert!(
            sim.events
                .values()
                .all(|event| !matches!(event, Event::Pump(_)))
        );
        // Queue capacity is reusable even while canceled context ownership is
        // retained by the completion event. One spare context admits this request.
        sim.arrive(4, request());
        pump_next(&mut sim, 4);
        assert!(sim.active[&4].queued);
        assert_eq!(sim.workers[0].pipe_queue.back(), Some(&4));
        assert_eq!(sim.workers[0].pipe_queue.len(), 3);
        assert_eq!(sim.report.failed, 0);
        drop(held);
    }
}

#[test]
fn canceling_a_notified_head_wakes_its_successor_before_delayed_release() {
    let mut sim = Simulator::new(config());
    let held = sim.reserve(0, 0, ResourceClass::Pipe, 1).unwrap();
    for id in 1..=2 {
        sim.arrive(id, request());
        pump_next(&mut sim, id);
    }
    drop(held);
    sim.wake_pipe(0);
    let head = sim.active.remove(&1).unwrap();
    sim.release(1, head);
    assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([2]));
    pump_next(&mut sim, 1); // The canceled head's scheduled pump is harmless.
    pump_next(&mut sim, 2);
    assert!(sim.active[&2].pipe.is_some());
    assert!(sim.workers[0].pipe_queue.is_empty());
}

#[test]
fn one_pipe_drains_a_full_bounded_queue_sequentially() {
    let config = config();
    let count = config.pipes + config.queue_entries;
    let service = PAGE_BYTES.div_ceil(config.reader_bytes_per_tick) + config.completion_ticks;
    let mut sim = Simulator::new(config.clone());

    // Check the actual queue before running its completion events. Repeated
    // pumps must not insert duplicate waiters or acquire another pipe.
    for id in 0..count {
        sim.arrive(id, request());
        sim.pump(id);
    }
    assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([1, 2, 3]));
    assert_eq!(request_ownership(&sim), [count * CONTEXT, count, 1]);
    for id in 1..count {
        assert!(sim.active[&id].queued);
        assert!(sim.active[&id].pipe.is_none());
        sim.pump(id);
    }
    assert_eq!(sim.workers[0].pipe_queue, VecDeque::from([1, 2, 3]));

    // A fresh run exercises the model event dispatcher and terminal accounting.
    let report = Simulator::new(config.clone()).run(vec![request(); count]);
    assert_outcomes(&report, count, 0, 0);
    assert_eq!(report.peak_requests, count);
    assert_eq!(report.peak_worker[ResourceClass::Pipe as usize], 1);
    // Once the queue exists, newcomers wait without attempting raw admission.
    assert_eq!(report.rejections[ResourceClass::Pipe as usize], 1);
    assert_eq!(report.retries, 0, "pipe waiters should be event-driven");
    assert_eq!(report.fills, 1);
    assert_eq!(report.hits, count - 1);
    for pair in report.latency_ticks.windows(2) {
        assert_eq!(pair[1] - pair[0], service + config.completion_ticks);
    }
}

#[test]
fn canceling_the_only_queued_request_allows_later_recovery() {
    let config = Config {
        queue_entries: 1,
        ..config()
    };
    let mut canceled = request();
    canceled.cancel_after = Some(8);
    let mut recovery = request();
    recovery.at = 512;
    let report = Simulator::new(config).run(vec![request(), canceled, recovery]);

    assert_outcomes(&report, 2, 0, 1);
    assert_eq!(report.peak_requests, 2);
    assert_eq!(report.rejections[ResourceClass::Pipe as usize], 1);
    assert_eq!(report.fills, 1);
    assert_eq!(report.hits, 1, "canceled waiter must not begin a read");
    assert_eq!(report.retries, 0);
}

#[test]
fn canceling_a_queued_request_preserves_survivor_fifo_order() {
    let config = config();
    let mut fast = request();
    fast.reader_bytes_per_tick = Some(PAGE_BYTES);
    let survivors = vec![request(), fast.clone(), request()];
    let baseline = Simulator::new(config.clone()).run(survivors);
    let mut canceled = request();
    canceled.cancel_after = Some(8);
    let report = Simulator::new(config).run(vec![request(), canceled, fast, request()]);

    assert_outcomes(&baseline, 3, 0, 0);
    assert_outcomes(&report, 3, 0, 1);
    // All arrive at zero. The distinct reader rates make a FIFO inversion
    // visible in completion latencies, even though all survivors still finish.
    assert_eq!(
        report.latency_ticks, baseline.latency_ticks,
        "canceling a waiter that owns no pipe must not reorder surviving waiters"
    );
}

#[test]
fn canceled_queue_tail_frees_capacity_for_a_replacement_waiter() {
    let config = config();
    let mut canceled = request();
    canceled.cancel_after = Some(8);
    let mut replacement = request();
    // The canceled request's delayed release has completed, while the first
    // reader is still using the only pipe. One queue slot must be available.
    replacement.at = 8 + config.completion_ticks + 1;
    let report =
        Simulator::new(config).run(vec![request(), request(), request(), canceled, replacement]);

    assert_outcomes(&report, 4, 0, 1);
    assert_eq!(report.peak_requests, 4);
    assert_eq!(report.rejections[ResourceClass::RequestContext as usize], 0);
    assert_eq!(report.hits, 3);
    assert_eq!(report.retries, 0);
}

#[test]
fn request_context_bound_rejects_excess_arrivals_and_recovers() {
    for queue_entries in [1, 3] {
        let config = Config {
            queue_entries,
            ..config()
        };
        let capacity = config.pipes + config.queue_entries;
        let excess = 5;
        let mut sim = Simulator::new(config.clone());
        for id in 0..capacity {
            sim.arrive(id, request());
        }
        assert_eq!(request_ownership(&sim), [capacity * CONTEXT, capacity, 0]);
        let scheduled = sim.events.len();
        for id in capacity..capacity + excess {
            sim.arrive(id, request());
            assert!(!sim.active.contains_key(&id));
            assert_eq!(sim.active.len(), capacity);
            assert_eq!(request_ownership(&sim), [capacity * CONTEXT, capacity, 0]);
            assert_eq!(sim.events.len(), scheduled, "rejection scheduled work");
        }
        assert_eq!(sim.report.failed, excess);
        assert_eq!(
            sim.report.rejections[ResourceClass::RequestContext as usize],
            excess
        );

        let mut requests = vec![request(); capacity + excess];
        let mut recovery = request();
        recovery.at = 4096;
        requests.push(recovery);
        let report = Simulator::new(config).run(requests);
        assert_outcomes(&report, capacity + 1, excess, 0);
        assert_eq!(report.peak_requests, capacity);
        assert_eq!(
            report.peak_worker[ResourceClass::RequestContext as usize],
            capacity * CONTEXT
        );
        assert_eq!(
            report.peak_worker[ResourceClass::Connection as usize],
            capacity
        );
        assert_eq!(
            report.rejections[ResourceClass::RequestContext as usize],
            excess
        );
    }
}

#[test]
fn connection_rejection_rolls_back_the_new_request_context() {
    let mut sim = Simulator::new(config());
    let capacity = sim.config.pipes + sim.config.queue_entries;
    // Normally context admission hits its equally sized bound first. Hold one
    // independent connection to exercise the subsequent reservation failure.
    let connection = sim.reserve(0, 0, ResourceClass::Connection, 1).unwrap();
    for id in 0..capacity - 1 {
        sim.arrive(id, request());
    }
    let before = request_ownership(&sim);
    let scheduled = sim.events.len();
    sim.arrive(capacity - 1, request());
    assert_eq!(sim.report.failed, 1);
    assert_eq!(sim.report.rejections[ResourceClass::Connection as usize], 1);
    assert_eq!(
        sim.report.rejections[ResourceClass::RequestContext as usize],
        0
    );
    assert_eq!(request_ownership(&sim), before);
    assert_eq!(sim.active.len(), capacity - 1);
    assert_eq!(sim.events.len(), scheduled);

    drop(connection);
    sim.arrive(capacity, request());
    assert!(sim.active.contains_key(&capacity));
    assert_eq!(request_ownership(&sim), [capacity * CONTEXT, capacity, 0]);
}

#[test]
fn idle_nodes_add_no_polling_events_or_logical_ownership() {
    let config = config();
    let mut later = request();
    later.at = 1_000_000_000;
    let requests = vec![request(), request(), later];
    let baseline = Simulator::new(config.clone()).run(requests.clone());
    assert_outcomes(&baseline, 3, 0, 0);

    for nodes in [2, 2000] {
        let config = Config {
            nodes,
            ..config.clone()
        };
        let empty = Simulator::new(config.clone()).run(Vec::new());
        assert_eq!(empty, Report::default(), "idle fleet generated work");
        let report = Simulator::new(config).run(requests.clone());
        // Compare deterministic work, not wall-clock time. This also checks
        // that the long idle interval contributes no periodic polling events.
        assert_eq!(report, baseline, "idle nodes changed the active workload");
    }
    assert!(baseline.events < 64, "long idle interval generated polling");
}

#[test]
fn canceled_send_keeps_page_and_pipe_until_their_scheduled_completions() {
    for cancel_near_completion in [false, true] {
        let mut sim = Simulator::new(Config {
            pages_per_worker: 1,
            ..config()
        });
        let key = (0, 0, 0);
        let plain = Arc::new(sim.reserve(0, 0, ResourceClass::Plaintext, PLAIN).unwrap());
        let cipher = Arc::new(
            sim.reserve(0, 0, ResourceClass::Ciphertext, CIPHER)
                .unwrap(),
        );
        let page = Arc::downgrade(&plain);
        sim.workers[0].cache.insert(key, Bundle { plain, cipher });
        sim.workers[0].lru.push_back(key);
        sim.directory.entry(key).or_default().insert(0);
        sim.arrive(0, request());
        let ((at, _), event) = sim.events.pop_first().unwrap();
        assert_eq!(at, 0);
        assert!(matches!(event, Event::Pump(0)));
        sim.pump(0);
        let sent_at = sim.active[&0].sending_until.unwrap();
        assert_eq!(page.strong_count(), 2, "cache and send must own the page");
        assert!(!sim.workers[0].cache[&key].idle());
        assert_eq!(request_ownership(&sim), [CONTEXT, 1, 1]);

        sim.now = if cancel_near_completion {
            sent_at - 1
        } else {
            1
        };
        // Use the same ownership transition as Terminate, then inspect the
        // events before the dispatcher could hide an early release by draining.
        let active = sim.active.remove(&0).unwrap();
        sim.release(0, active);
        let release_at = sent_at.max(sim.now + sim.config.completion_ticks);
        assert!(sim.events.iter().any(|(&(at, _), event)| {
            at == release_at && matches!(event, Event::Release(active) if active.pipe.is_some())
        }));
        assert_eq!(request_ownership(&sim), [CONTEXT, 1, 1]);

        // Remove the cache owner so only the scheduled send can keep quota
        // alive. A Weak observes identity without extending its lifetime.
        sim.evict(0, key);
        assert_eq!(page.strong_count(), 1);
        assert_eq!(
            sim.workers[0].admission.used(ResourceClass::Plaintext),
            PLAIN
        );
        assert_eq!(sim.workers[0].admission.used(ResourceClass::Ciphertext), 0);
        sim.now = sent_at - 1;
        assert!(
            sim.reserve_page(0, 0, ResourceClass::Plaintext, PLAIN)
                .is_none()
        );
        assert!(sim.reserve(0, 0, ResourceClass::Pipe, 1).is_none());
        assert_eq!(page.strong_count(), 1);
        assert_eq!(request_ownership(&sim), [CONTEXT, 1, 1]);

        let ((at, _), event) = sim.events.pop_first().unwrap();
        assert_eq!(
            at, sent_at,
            "send was released before its scheduled completion"
        );
        sim.now = at;
        let Event::Sent(0, plain) = event else {
            panic!("canceled send must retain its scheduled completion event");
        };
        assert_eq!(Arc::as_ptr(&plain), page.as_ptr());
        assert_eq!(
            sim.workers[0].admission.used(ResourceClass::Plaintext),
            PLAIN
        );
        // The canceled Sent branch has no Active to advance; it drops this pin.
        drop(plain);
        assert_eq!(page.strong_count(), 0);
        assert_eq!(sim.workers[0].admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(request_ownership(&sim), [CONTEXT, 1, 1]);
        assert_eq!(sim.report.delivered_bytes, 0);

        let ((at, _), event) = sim.events.pop_first().unwrap();
        assert_eq!(at, release_at);
        sim.now = at;
        let Event::Release(active) = event else {
            panic!("pipe must remain owned by the scheduled request release");
        };
        assert!(active.pipe.is_some());
        assert_eq!(request_ownership(&sim), [CONTEXT, 1, 1]);
        drop(active);
        sim.wake_pipe(0);
        assert_eq!(request_ownership(&sim), [0; 3]);
        assert!(
            CLASSES
                .iter()
                .all(|class| sim.workers[0].admission.used(*class) == 0)
        );
        let replacement_page = sim
            .reserve_page(0, 0, ResourceClass::Plaintext, PLAIN)
            .unwrap();
        let replacement_pipe = sim.reserve(0, 0, ResourceClass::Pipe, 1).unwrap();
        drop((replacement_page, replacement_pipe));
        assert_eq!(sim.run(Vec::new()).final_used, [0; 11]);
    }
}
