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
    let narrow = run(&narrow_config, &requests);
    let wide = run(&wide_config, &requests);

    assert_success(&narrow_config, &requests, &narrow);
    assert_success(&wide_config, &requests, &wide);
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
    let fast = run(&fast_config, &requests);
    let slow = run(&slow_config, &requests);

    assert_success(&fast_config, &requests, &fast);
    assert_success(&slow_config, &requests, &slow);
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
    let fast = run(&fast_config, &requests);
    let slow = run(&slow_config, &requests);

    assert_success(&fast_config, &requests, &fast);
    assert_success(&slow_config, &requests, &slow);
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
    let fast = run(&fast_config, &requests);
    let slow = run(&slow_config, &requests);

    assert_success(&fast_config, &requests, &fast);
    assert_success(&slow_config, &requests, &slow);
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
    assert!(report.peer_bytes > 0, "warm peers were unused: {report:?}");
    assert!(
        report.origin_bytes < requested_bytes(&requests),
        "fanout fetched every copy from origin: {report:?}"
    );
    // Use charged peer bytes rather than an exact fanout count: subsequent
    // destinations may themselves become sources as their fills complete.
    let one_page_ticks = PAGE_BYTES.div_ceil(config.network_bytes_per_tick);
    assert!(
        report.latency_ticks.iter().copied().max().unwrap() > one_page_ticks * 2,
        "fanout never queued behind source traffic: {report:?}"
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
