//! Independent queue bounds, cancellation, and scheduled-owner edge cases.
use super::*;

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
    assert_eq!(report.completed, completed, "{report:?}");
    assert_eq!(report.failed, failed, "{report:?}");
    assert_eq!(report.canceled, canceled, "{report:?}");
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

    // A fresh run exercises the real dispatcher and terminal accounting.
    let report = Simulator::new(config.clone()).run(vec![request(); count]);
    assert_outcomes(&report, count, 0, 0);
    assert_eq!(report.peak_requests, count);
    assert_eq!(report.peak_worker[ResourceClass::Pipe as usize], 1);
    assert_eq!(report.rejections[ResourceClass::Pipe as usize], count - 1);
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
        sim.release(active);
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
