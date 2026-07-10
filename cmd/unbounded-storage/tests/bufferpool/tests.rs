// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::rc::Rc;

use proptest::prelude::*;
use unbounded_storage::bufferpool::{BufferPool, Pool, PoolConfig, StripeKey};

use crate::bufferpool::mocks::{
    CallCounts, DstBlockStore, DstTransport, MockSimConfig, Stripes, TestReq,
};
use crate::bufferpool::workload::{
    ClientOutcome, ClientSpec, PipelineSpec, PlanSliceSpec, RunReport, Workload, heap_backing,
    pipelined_workload_strategy, run_workload, workload_strategy,
};
use crate::framework::executor::{Executor, yield_n};

proptest! {
    #![proptest_config(ProptestConfig {
        // Keep CI runtime modest; bump locally (or via
        // `PROPTEST_CASES`) for soak runs.
        cases: 256,
        ..ProptestConfig::default()
    })]

    #[test]
    fn bufferpool_invariants(seed in any::<u64>(), w in workload_strategy()) {
        let faults_enabled = w.io_fault_rate > 0;
        let page_count = w.page_count;
        let page_cache_enabled = w.page_cache_enabled;
        let limit = w.max_concurrent_streams;
        let n_clients = w.clients.len();
        let report = run_workload(seed, w)
            .expect("run completed without deadlock or budget exhaustion");
        assert_byte_correctness(&report, faults_enabled)?;
        assert_bounded_termination(&report)?;
        assert_page_accounting(&report, page_count, page_cache_enabled)?;
        assert_single_flight(&report)?;
        assert_no_leak_under_faults(&report, page_count)?;
        assert_stream_limit_bounds(&report, page_count, limit, n_clients)?;
        assert_drain_on_cancellation(&report, page_count)?;
    }
}

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 256,
        ..ProptestConfig::default()
    })]

    #[test]
    fn pipelined_bufferpool_invariants(
        seed in any::<u64>(),
        w in pipelined_workload_strategy(),
    ) {
        let faults_enabled = w.io_fault_rate > 0;
        let page_count = w.page_count;
        let page_cache_enabled = w.page_cache_enabled;
        let report = run_workload(seed, w)
            .expect("run completed without deadlock or budget exhaustion");
        assert_pipelined_byte_correctness(&report, faults_enabled)?;
        assert_pipelined_page_accounting(&report, page_count, page_cache_enabled)?;
        assert_pipelined_single_flight(&report)?;
        assert_pipelined_bounded_termination(&report)?;
    }
}

/// Invariant: byte correctness for successful clients and oracle-prefix
/// correctness for cancelled clients. Fetch errors require fault injection,
/// and read errors must be stream-limit rejections.
fn assert_byte_correctness(report: &RunReport, faults_enabled: bool) -> Result<(), TestCaseError> {
    for (i, o) in report.outcomes.iter().enumerate() {
        match o {
            ClientOutcome::Ok { got, expected } => {
                prop_assert_eq!(got, expected, "client {} bytes mismatch", i);
            }
            ClientOutcome::Cancelled { got, expected, .. } => {
                prop_assert!(
                    got.len() <= expected.len(),
                    "client {} cancelled with got.len()={} > expected.len()={}",
                    i,
                    got.len(),
                    expected.len(),
                );
                prop_assert_eq!(
                    &got[..],
                    &expected[..got.len()],
                    "client {} cancelled-prefix bytes mismatch",
                    i,
                );
            }
            ClientOutcome::ReadErr(e) => {
                prop_assert!(
                    is_stream_limit(e),
                    "client {} unexpected read error: {}",
                    i,
                    e,
                );
            }
            ClientOutcome::FetchErr(e) => {
                prop_assert!(
                    faults_enabled,
                    "client {} got FetchErr ({}) with io_fault_rate=0",
                    i,
                    e,
                );
            }
        }
    }
    Ok(())
}

/// Invariant: every spawned client terminates within the executor budget.
fn assert_bounded_termination(report: &RunReport) -> Result<(), TestCaseError> {
    prop_assert!(!report.outcomes.is_empty());
    Ok(())
}

/// Invariant: pages, inflight entries, cache policy, and prefetch reservations
/// are fully accounted for at quiescence, including under fault injection.
fn assert_page_accounting(
    report: &RunReport,
    page_count: usize,
    page_cache_enabled: bool,
) -> Result<(), TestCaseError> {
    assert_quiescent_accounting(report, page_count)?;
    if !page_cache_enabled {
        prop_assert_eq!(
            report.cached_pages_at_end,
            0,
            "disabled page cache retained {} pages",
            report.cached_pages_at_end,
        );
    }
    prop_assert_eq!(
        report.prefetch_inflight_at_end,
        0,
        "prefetch budget not released: {} pages still reserved",
        report.prefetch_inflight_at_end,
    );
    Ok(())
}

/// Invariant: at most one `bulk_get` is concurrently in flight per logical
/// page; sequential reissues after recycle or failure remain allowed.
fn assert_single_flight(report: &RunReport) -> Result<(), TestCaseError> {
    for (k, n) in &report.bulk_get_max_inflight {
        prop_assert!(
            *n <= 1,
            "(key {:?}, page {}) had {} concurrent bulk_get calls; expected single-flight",
            k.0,
            k.1,
            n,
        );
    }
    Ok(())
}

/// Invariant: error outcomes neither stall clients nor leak pages or inflight
/// entries; read errors remain limited to stream admission rejection.
fn assert_no_leak_under_faults(report: &RunReport, page_count: usize) -> Result<(), TestCaseError> {
    for (i, o) in report.outcomes.iter().enumerate() {
        match o {
            ClientOutcome::Ok { .. }
            | ClientOutcome::Cancelled { .. }
            | ClientOutcome::FetchErr(_) => {}
            ClientOutcome::ReadErr(e) => {
                prop_assert!(is_stream_limit(e), "client {} unexpected ReadErr: {}", i, e,);
            }
        }
    }
    assert_quiescent_accounting(report, page_count)
}

/// Invariant: `max_concurrent_streams` rejects only with `StreamLimit`, never
/// rejects when the limit covers all clients, and cannot reject extra clients.
fn assert_stream_limit_bounds(
    report: &RunReport,
    page_count: usize,
    limit: usize,
    n_clients: usize,
) -> Result<(), TestCaseError> {
    let mut rejects = 0usize;
    for (i, o) in report.outcomes.iter().enumerate() {
        if let ClientOutcome::ReadErr(e) = o {
            prop_assert!(is_stream_limit(e), "client {} unexpected ReadErr: {}", i, e,);
            rejects += 1;
        }
    }
    if limit >= n_clients {
        prop_assert_eq!(
            rejects,
            0,
            "limit={} >= clients={} but {} streams were rejected",
            limit,
            n_clients,
            rejects,
        );
    }
    prop_assert!(
        rejects <= n_clients,
        "more rejects ({}) than clients ({})",
        rejects,
        n_clients,
    );
    assert_quiescent_accounting(report, page_count)
}

/// Invariant: dropping a stream before EOF returns only an oracle prefix and
/// drains pages and inflight entries despite faults or admission rejection.
fn assert_drain_on_cancellation(
    report: &RunReport,
    page_count: usize,
) -> Result<(), TestCaseError> {
    for (i, o) in report.outcomes.iter().enumerate() {
        if let ClientOutcome::Cancelled { got, expected, .. } = o {
            prop_assert!(
                got.len() <= expected.len(),
                "client {} cancelled with got.len()={} > expected.len()={}",
                i,
                got.len(),
                expected.len(),
            );
            prop_assert_eq!(
                &got[..],
                &expected[..got.len()],
                "client {} cancelled-prefix bytes mismatch",
                i,
            );
        }
    }
    assert_quiescent_accounting(report, page_count)
}

/// Invariant: pipelined reads deliver plan bytes in global order; cancellation
/// yields an ordered prefix, and errors obey the standard fault constraints.
fn assert_pipelined_byte_correctness(
    report: &RunReport,
    faults_enabled: bool,
) -> Result<(), TestCaseError> {
    for (i, o) in report.outcomes.iter().enumerate() {
        match o {
            ClientOutcome::Ok { got, expected } => {
                prop_assert_eq!(got, expected, "pipeline {} bytes mismatch", i);
            }
            ClientOutcome::Cancelled { got, expected, .. } => {
                prop_assert!(
                    got.len() <= expected.len(),
                    "pipeline {} cancelled with got.len()={} > expected.len()={}",
                    i,
                    got.len(),
                    expected.len(),
                );
                prop_assert_eq!(
                    &got[..],
                    &expected[..got.len()],
                    "pipeline {} cancelled-prefix bytes mismatch",
                    i,
                );
            }
            ClientOutcome::ReadErr(e) => {
                prop_assert!(
                    is_stream_limit(e),
                    "pipeline {} unexpected read error: {}",
                    i,
                    e,
                );
            }
            ClientOutcome::FetchErr(e) => {
                prop_assert!(
                    faults_enabled,
                    "pipeline {} got FetchErr ({}) with io_fault_rate=0",
                    i,
                    e,
                );
            }
        }
    }
    Ok(())
}

/// Invariant: pipelined readers release pages, per-slice streams, inflight
/// entries, cache-disabled pages, and the shared prefetch budget at quiescence.
fn assert_pipelined_page_accounting(
    report: &RunReport,
    page_count: usize,
    page_cache_enabled: bool,
) -> Result<(), TestCaseError> {
    assert_quiescent_accounting(report, page_count)?;
    if !page_cache_enabled {
        prop_assert_eq!(
            report.cached_pages_at_end,
            0,
            "disabled page cache retained {} pages",
            report.cached_pages_at_end,
        );
    }
    prop_assert_eq!(
        report.prefetch_inflight_at_end,
        0,
        "prefetch budget not released: {} pages still reserved",
        report.prefetch_inflight_at_end,
    );
    Ok(())
}

/// Invariant: cross-stripe pipelining preserves per-page single-flight even
/// when a stripe appears in multiple plan slices.
fn assert_pipelined_single_flight(report: &RunReport) -> Result<(), TestCaseError> {
    assert_single_flight(report)
}

/// Invariant: every spawned pipeline terminates without deadlock or executor
/// budget exhaustion under cross-stripe page pressure.
fn assert_pipelined_bounded_termination(report: &RunReport) -> Result<(), TestCaseError> {
    prop_assert!(!report.outcomes.is_empty());
    Ok(())
}

/// `Error::StreamLimit`'s Display string from `src/bufferpool/types.rs`.
/// The harness flattens errors to `String` at the framework boundary.
fn is_stream_limit(msg: &str) -> bool {
    msg == "max_concurrent_streams reached"
}

fn assert_quiescent_accounting(report: &RunReport, page_count: usize) -> Result<(), TestCaseError> {
    prop_assert_eq!(
        report.free_pages_at_end + report.cached_pages_at_end,
        page_count,
        "free_pages={} cached_pages={} expected total {}",
        report.free_pages_at_end,
        report.cached_pages_at_end,
        page_count,
    );
    prop_assert_eq!(
        report.active_inflight_entries_at_end,
        0,
        "active inflight not drained: {} entries (raw inflight={})",
        report.active_inflight_entries_at_end,
        report.inflight_entries_at_end,
    );
    Ok(())
}

/// Scenario test: with `max_concurrent_streams=1` and four
/// concurrent clients on the same stripe, at least one `read()`
/// must be rejected with `StreamLimit` (the leader holds the slot
/// across at least one yield, during which the executor will
/// schedule a sibling that finds `stream_count == limit`).
/// Verifies the strategy's "low limit" branch actually triggers
/// the reject path - the proptest property only asserts an upper
/// bound, so without this we couldn't tell if `StreamLimit` had
/// quietly become unreachable.
#[test]
fn stream_limit_rejects_excess_concurrent_reads() {
    let w = Workload {
        page_size: 128,
        page_count: 2,
        // Non-zero so the leader pends on first poll, parking task
        // 1 before a sibling gets to run `read()`. The executor's
        // random pick still admits schedules where the leader
        // finishes before some siblings even attempt `read()`, so
        // we don't pin down the exact reject count.
        max_io_delay: 4,
        io_fault_rate: 0,
        cache_hit_rate: 0,
        page_cache_enabled: true,
        max_concurrent_streams: 1,
        max_inflight_pages: 4,
        key_count: 1,
        clients: vec![
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 128,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 128,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 128,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 128,
                cancel_after: None,
                window: None,
            },
        ],
        pipelines: Vec::new(),
    };
    let report = run_workload(0xBADBED, w).expect("scenario run");
    let oks = report
        .outcomes
        .iter()
        .filter(|o| matches!(o, ClientOutcome::Ok { .. }))
        .count();
    let rejects = report
        .outcomes
        .iter()
        .filter(|o| matches!(o, ClientOutcome::ReadErr(e) if is_stream_limit(e)))
        .count();
    assert_eq!(
        oks + rejects,
        4,
        "every client must Ok or StreamLimit: {:?}",
        report.outcomes
    );
    assert!(
        rejects >= 1,
        "expected at least one StreamLimit reject: {:?}",
        report.outcomes
    );
    assert!(oks >= 1, "expected at least one Ok: {:?}", report.outcomes);
    for o in &report.outcomes {
        if let ClientOutcome::Ok { got, expected } = o {
            assert_eq!(got, expected, "byte mismatch on successful client");
        }
    }
    assert_quiescent_accounting(&report, 2).expect("quiescent accounting");
}
/// Regression: a 5-client / 2-page / fault-injecting workload
/// shrunk from `invariant_single_flight_per_page` was deadlocking
/// (no task ready while at least one was still alive). The bug
/// lived in the FreeList wake path under leader-error: see the
/// "Deadlock on free-list waiters" fix in `pool.rs`.
#[test]
fn regression_freelist_deadlock_under_faults() {
    let w = Workload {
        page_size: 128,
        page_count: 2,
        max_io_delay: 1,
        io_fault_rate: 3,
        cache_hit_rate: 0,
        page_cache_enabled: true,
        max_concurrent_streams: 1024,
        max_inflight_pages: 4,
        key_count: 2,
        clients: vec![
            ClientSpec {
                key_idx: 0,
                offset: 640,
                len: 1,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 55,
                offset: 2048,
                len: 1,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 14,
                offset: 256,
                len: 1,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 1664,
                len: 1,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 103,
                offset: 640,
                len: 1,
                cancel_after: None,
                window: None,
            },
        ],
        pipelines: Vec::new(),
    };
    let report = run_workload(16283855356151283598u64, w).expect("must not deadlock");
    assert_quiescent_accounting(&report, 2).expect("quiescent accounting");
}

/// Smoke test that hits one fixed seed; lets `cargo test dst::smoke`
/// give a quick signal without paying the full proptest cost.
#[test]
fn smoke() {
    let w = Workload {
        page_size: 256,
        page_count: 4,
        max_io_delay: 2,
        io_fault_rate: 0,
        cache_hit_rate: 0,
        page_cache_enabled: true,
        max_concurrent_streams: 1024,
        max_inflight_pages: 4,
        key_count: 2,
        clients: vec![
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 256,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 512,
                cancel_after: None,
                window: Some(3),
            },
            ClientSpec {
                key_idx: 1,
                offset: 0,
                len: 1024,
                cancel_after: None,
                window: Some(2),
            },
        ],
        pipelines: Vec::new(),
    };
    let report: RunReport = run_workload(0xC0FFEE, w).expect("smoke run");
    assert_quiescent_accounting(&report, 4).expect("quiescent accounting");
    assert_eq!(report.prefetch_inflight_at_end, 0);
    for o in &report.outcomes {
        match o {
            ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
            other => panic!("unexpected outcome: {other:?}"),
        }
    }
}

/// Scenario test: with `cache_hit_rate=100`, `BlockStore::read_page`
/// always returns `Ok(true)` and the pool must never invoke
/// `Transport::bulk_get`. Verifies the fast-path branch in
/// `Pool::fetch_page` and that the `need_tee = !hit` short-circuit
/// keeps the tee from running.
#[test]
fn all_cache_hits_skip_bulk_get_and_tee() {
    let w = Workload {
        page_size: 256,
        page_count: 4,
        max_io_delay: 2,
        io_fault_rate: 0,
        cache_hit_rate: 100,
        page_cache_enabled: true,
        max_concurrent_streams: 1024,
        max_inflight_pages: 4,
        key_count: 2,
        clients: vec![
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 1024,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 1,
                offset: 64,
                len: 512,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 100,
                len: 200,
                cancel_after: None,
                window: None,
            },
        ],
        pipelines: Vec::new(),
    };
    let report = run_workload(0xFADE, w).expect("scenario run");
    assert_eq!(
        report.bulk_get_calls, 0,
        "bulk_get must not run on full cache hit"
    );
    assert!(
        report.bulk_get_max_inflight.is_empty(),
        "no (key, page) should have any recorded bulk_get",
    );
    assert_quiescent_accounting(&report, 4).expect("quiescent accounting");
    for o in &report.outcomes {
        match o {
            ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
            other => panic!("unexpected outcome: {other:?}"),
        }
    }
}

/// Scenario test: a live page-cache drain invalidates the policy
/// snapshot held by an already admitted stream. Pages fetched after the
/// drain must still be delivered correctly but must not be retained in
/// the RAM page cache. This pins the branch's `page_cache_drain_epoch`
/// path under deterministic scheduler interleavings instead of only the
/// synchronous module tests.
#[test]
fn drain_page_cache_during_active_stream_disables_retention() {
    const PAGE_SIZE: usize = 128;
    const PAGE_COUNT: usize = 4;

    for seed in [0u64, 1, 7, 42, 1234, 99999] {
        let backing = heap_backing(PAGE_SIZE, PAGE_COUNT);
        let counts = Rc::new(CallCounts::default());
        let mock_cfg = MockSimConfig::new();
        mock_cfg.max_io_delay.set(3);
        let stripes: Stripes = Rc::new(RefCell::new(HashMap::new()));
        let key = StripeKey([0xDA; 32]);
        let expected: Vec<u8> = (0..PAGE_SIZE * 3)
            .map(|i| ((i + 0x31) & 0xff) as u8)
            .collect();
        stripes.borrow_mut().insert(key, expected.clone());

        let transport = DstTransport::new(
            stripes.clone(),
            counts.clone(),
            mock_cfg.clone(),
            backing.base,
            backing.page_size,
        );
        let blockstore = DstBlockStore::new(counts.clone(), stripes, mock_cfg);
        blockstore.set_page_cache_enabled(true);
        let pool = Rc::new(
            Pool::new(
                PoolConfig {
                    max_concurrent_streams: 1024,
                    max_inflight_pages: 3,
                },
                backing,
                transport,
                blockstore,
            )
            .expect("pool construction must succeed"),
        );

        let mut exec = Executor::new(seed);
        let first_page_seen = Rc::new(Cell::new(false));
        let drain_done = Rc::new(Cell::new(false));
        let outcomes: Rc<RefCell<Vec<ClientOutcome>>> = Rc::new(RefCell::new(Vec::new()));

        let p = pool.clone();
        let first_page_seen_reader = first_page_seen.clone();
        let outcomes_reader = outcomes.clone();
        let expected_reader = expected.clone();
        exec.spawn(async move {
            let req = TestReq { key };
            let mut read = p
                .read_windowed(&req, 0, (PAGE_SIZE * 3) as u64, 3)
                .expect("windowed read");
            let mut got = Vec::new();
            let first = read.next_page().await.unwrap().unwrap();
            got.extend_from_slice(first.as_slice());
            first_page_seen_reader.set(true);
            yield_n(4).await;
            drop(first);
            while let Some(page) = read.next_page().await {
                got.extend_from_slice(page.unwrap().as_slice());
            }
            outcomes_reader.borrow_mut().push(ClientOutcome::Ok {
                got,
                expected: expected_reader,
            });
        });

        let p = pool.clone();
        let first_page_seen_drainer = first_page_seen.clone();
        let drain_done_task = drain_done.clone();
        exec.spawn(async move {
            while !first_page_seen_drainer.get() {
                yield_n(1).await;
            }
            p.drain_page_cache();
            drain_done_task.set(true);
        });

        exec.run(5_000)
            .expect("active drain scenario must complete");
        assert!(drain_done.get(), "seed {seed}: drain task ran");
        assert_eq!(
            pool.cached_pages(),
            0,
            "seed {seed}: drained stream must not retain RAM cache pages",
        );
        assert_eq!(
            pool.free_pages(),
            PAGE_COUNT,
            "seed {seed}: all pages returned to free list",
        );
        assert_eq!(
            pool.active_inflight_entries(),
            0,
            "seed {seed}: active entries must drain",
        );
        assert_eq!(
            counts.bulk_get.get(),
            3,
            "seed {seed}: every page fetched exactly once",
        );

        let outcomes = outcomes.borrow();
        assert_eq!(outcomes.len(), 1, "seed {seed}: reader finished");
        match &outcomes[0] {
            ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
            other => panic!("seed {seed}: unexpected outcome {other:?}"),
        }
    }
}

/// Regression: idle cached pages must be evictable when later demand
/// heads need backing pages. The first phase fills a two-page pool's
/// RAM cache. The second phase starts three distinct reads; two evict
/// cached pages for their heads and the third parks until one active
/// guard drops. This pins both cache-pressure eviction and the
/// waiter-priority path under the deterministic scheduler.
#[test]
fn cached_pages_do_not_deadlock_contended_heads() {
    const PAGE_SIZE: usize = 128;
    const PAGE_COUNT: usize = 2;

    for seed in [0u64, 1, 7, 42, 1234, 99999] {
        let backing = heap_backing(PAGE_SIZE, PAGE_COUNT);
        let counts = Rc::new(CallCounts::default());
        let mock_cfg = MockSimConfig::new();
        mock_cfg.max_io_delay.set(2);
        let stripes: Stripes = Rc::new(RefCell::new(HashMap::new()));
        let mut oracle = HashMap::new();
        for idx in 0u8..5 {
            let key = StripeKey([idx; 32]);
            let bytes = vec![idx.wrapping_add(0x40); PAGE_SIZE];
            stripes.borrow_mut().insert(key, bytes.clone());
            oracle.insert(key, bytes);
        }

        let transport = DstTransport::new(
            stripes.clone(),
            counts.clone(),
            mock_cfg.clone(),
            backing.base,
            backing.page_size,
        );
        let blockstore = DstBlockStore::new(counts.clone(), stripes, mock_cfg);
        let pool = Rc::new(
            Pool::new(
                PoolConfig {
                    max_concurrent_streams: 1024,
                    max_inflight_pages: 4,
                },
                backing,
                transport,
                blockstore,
            )
            .expect("pool construction must succeed"),
        );

        let mut exec = Executor::new(seed);
        let outcomes: Rc<RefCell<Vec<ClientOutcome>>> = Rc::new(RefCell::new(Vec::new()));

        for idx in 0u8..2 {
            let p = pool.clone();
            let outcomes = outcomes.clone();
            let expected = oracle[&StripeKey([idx; 32])].clone();
            exec.spawn(async move {
                let req = TestReq {
                    key: StripeKey([idx; 32]),
                };
                let mut stream = p.read(&req, 0, PAGE_SIZE as u64).await.unwrap();
                let got = stream
                    .next_page()
                    .await
                    .unwrap()
                    .unwrap()
                    .as_slice()
                    .to_vec();
                outcomes
                    .borrow_mut()
                    .push(ClientOutcome::Ok { got, expected });
            });
        }

        exec.run(1_000).expect("warmup phase must complete");
        assert_eq!(pool.cached_pages(), PAGE_COUNT, "seed {seed}: warmup cache");
        assert_eq!(pool.free_pages(), 0, "seed {seed}: all pages cached");
        assert_eq!(
            pool.active_inflight_entries(),
            0,
            "seed {seed}: warmup active state"
        );

        for idx in 2u8..5 {
            let p = pool.clone();
            let outcomes = outcomes.clone();
            let expected = oracle[&StripeKey([idx; 32])].clone();
            exec.spawn(async move {
                let req = TestReq {
                    key: StripeKey([idx; 32]),
                };
                let mut stream = p.read(&req, 0, PAGE_SIZE as u64).await.unwrap();
                let guard = stream.next_page().await.unwrap().unwrap();
                let got = guard.as_slice().to_vec();
                yield_n(4).await;
                drop(guard);
                outcomes
                    .borrow_mut()
                    .push(ClientOutcome::Ok { got, expected });
            });
        }

        exec.run(5_000).expect("pressure phase must not deadlock");
        assert_eq!(
            pool.free_pages() + pool.cached_pages(),
            PAGE_COUNT,
            "seed {seed}: pages must be free or cached",
        );
        assert_eq!(
            pool.active_inflight_entries(),
            0,
            "seed {seed}: active entries must drain",
        );
        assert_eq!(
            counts.bulk_get.get(),
            5,
            "seed {seed}: every distinct page fetched once"
        );

        let outcomes = outcomes.borrow();
        assert_eq!(outcomes.len(), 5, "seed {seed}: all readers finished");
        for outcome in outcomes.iter() {
            match outcome {
                ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
                other => panic!("seed {seed}: unexpected outcome {other:?}"),
            }
        }
    }
}

/// Regression: a warm RAM page cache should not permanently disable
/// speculative prefetch. Cached idle pages are evictable backing, so a
/// windowed read may use them as spare pages while still preserving the
/// per-stream head reserve.
#[test]
fn prefetch_evicts_idle_cached_pages() {
    const PAGE_SIZE: usize = 128;
    const PAGE_COUNT: usize = 3;

    for seed in [0u64, 5, 77, 9001] {
        let backing = heap_backing(PAGE_SIZE, PAGE_COUNT);
        let counts = Rc::new(CallCounts::default());
        let mock_cfg = MockSimConfig::new();
        mock_cfg.max_io_delay.set(4);
        let stripes: Stripes = Rc::new(RefCell::new(HashMap::new()));
        let mut oracle = HashMap::new();
        for idx in 0u8..4 {
            let key = StripeKey([idx; 32]);
            let bytes = vec![idx.wrapping_add(0x20); PAGE_SIZE * PAGE_COUNT];
            stripes.borrow_mut().insert(key, bytes.clone());
            oracle.insert(key, bytes);
        }

        let transport = DstTransport::new(
            stripes.clone(),
            counts.clone(),
            mock_cfg.clone(),
            backing.base,
            backing.page_size,
        );
        let blockstore = DstBlockStore::new(counts.clone(), stripes, mock_cfg);
        let pool = Rc::new(
            Pool::new(
                PoolConfig {
                    max_concurrent_streams: 1024,
                    max_inflight_pages: 2,
                },
                backing,
                transport,
                blockstore,
            )
            .expect("pool construction must succeed"),
        );

        let mut exec = Executor::new(seed);
        for idx in 0u8..3 {
            let p = pool.clone();
            exec.spawn(async move {
                let req = TestReq {
                    key: StripeKey([idx; 32]),
                };
                let mut stream = p.read(&req, 0, PAGE_SIZE as u64).await.unwrap();
                let _ = stream.next_page().await.unwrap().unwrap();
            });
        }
        exec.run(2_000).expect("warmup phase must complete");
        assert_eq!(pool.cached_pages(), PAGE_COUNT, "seed {seed}: warm cache");
        assert_eq!(pool.free_pages(), 0, "seed {seed}: all pages cached");

        let p = pool.clone();
        let outcomes: Rc<RefCell<Vec<ClientOutcome>>> = Rc::new(RefCell::new(Vec::new()));
        let outcomes_task = outcomes.clone();
        let expected = oracle[&StripeKey([3; 32])][..PAGE_SIZE * 2].to_vec();
        exec.spawn(async move {
            let req = TestReq {
                key: StripeKey([3; 32]),
            };
            let mut read = p
                .read_windowed(&req, 0, (PAGE_SIZE * 2) as u64, 2)
                .expect("windowed read");
            let mut got = Vec::new();
            while let Some(page) = read.next_page().await {
                got.extend_from_slice(page.unwrap().as_slice());
            }
            outcomes_task
                .borrow_mut()
                .push(ClientOutcome::Ok { got, expected });
        });

        exec.run(5_000).expect("windowed read must complete");
        assert_eq!(
            pool.free_pages() + pool.cached_pages(),
            PAGE_COUNT,
            "seed {seed}: pages must be free or cached",
        );
        assert_eq!(pool.prefetch_inflight(), 0, "seed {seed}: prefetch budget");
        assert_eq!(
            pool.active_inflight_entries(),
            0,
            "seed {seed}: active entries must drain",
        );
        assert_eq!(counts.bulk_get.get(), 5, "seed {seed}: every miss fetched");
        assert!(
            counts.bulk_get_max_total_inflight.get() >= 2,
            "seed {seed}: windowed read should overlap head and prefetch",
        );
        let outcomes = outcomes.borrow();
        assert_eq!(outcomes.len(), 1, "seed {seed}: windowed reader finished");
        match &outcomes[0] {
            ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
            other => panic!("seed {seed}: unexpected outcome {other:?}"),
        }
    }
}

/// Scenario test: a mix of clients where some drop their
/// `ReadStream` mid-iteration. Asserts that cancellation drains
/// cleanly even when the cancelling client never observes EOF.
/// Pins the drain path so a regression that, say, forgot to
/// release a leader's `consumer_holds` on early `ReadStream` drop
/// would fail this test before the proptest sweep.
#[test]
fn cancellation_drains_to_clean_state() {
    let w = Workload {
        page_size: 128,
        page_count: 4,
        max_io_delay: 2,
        io_fault_rate: 0,
        cache_hit_rate: 0,
        page_cache_enabled: true,
        max_concurrent_streams: 1024,
        max_inflight_pages: 4,
        key_count: 2,
        clients: vec![
            // Full read.
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 512,
                cancel_after: None,
                window: None,
            },
            // Cancels before first page.
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 512,
                cancel_after: Some(0),
                window: None,
            },
            // Cancels after the first page.
            ClientSpec {
                key_idx: 1,
                offset: 0,
                len: 512,
                cancel_after: Some(1),
                window: None,
            },
            // Cancels after two pages on the same key as the full
            // reader so the slot's `stream_refcount` interleaves
            // increments and decrements.
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 512,
                cancel_after: Some(2),
                window: None,
            },
        ],
        pipelines: Vec::new(),
    };
    let report = run_workload(0xD1A1ED, w).expect("scenario run");
    assert_quiescent_accounting(&report, 4).expect("quiescent accounting");

    let mut cancelled = 0;
    for o in &report.outcomes {
        match o {
            ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
            ClientOutcome::Cancelled { got, expected, .. } => {
                cancelled += 1;
                assert!(got.len() <= expected.len());
                assert_eq!(&got[..], &expected[..got.len()]);
            }
            other => panic!("unexpected outcome: {other:?}"),
        }
    }
    assert_eq!(cancelled, 3, "exactly three clients must have cancelled");
}

/// Scenario test: the windowed reader returns bytes in cursor order
/// for a multi-page read with a window deeper than one page, and
/// releases its prefetch budget by quiescence. Two clients share a
/// stripe so the windowed path also rides the single-flight slot
/// machinery. Pins the happy-path windowed read independent of the
/// proptest sweep.
#[test]
fn windowed_read_in_order_and_drains() {
    let w = Workload {
        page_size: 128,
        page_count: 6,
        max_io_delay: 3,
        io_fault_rate: 0,
        cache_hit_rate: 0,
        page_cache_enabled: true,
        max_concurrent_streams: 1024,
        max_inflight_pages: 4,
        key_count: 1,
        clients: vec![
            // Full stripe via a deep window: forces speculative
            // refill up to the budget cap.
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 768,
                cancel_after: None,
                window: Some(5),
            },
            // Same stripe, offset start, shallower window: shares
            // pages via single-flight while prefetching ahead.
            ClientSpec {
                key_idx: 0,
                offset: 256,
                len: 512,
                cancel_after: None,
                window: Some(2),
            },
        ],
        pipelines: Vec::new(),
    };
    let report = run_workload(0x5EED, w).expect("scenario run");
    for o in &report.outcomes {
        match o {
            ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
            other => panic!("unexpected outcome: {other:?}"),
        }
    }
    assert_quiescent_accounting(&report, 6).expect("quiescent accounting");
    assert_eq!(report.prefetch_inflight_at_end, 0);
}

/// Regression: windowed reader cancelled mid-stream under page
/// pressure used to strand a single-flight subscriber across a
/// `Loading -> Idle -> Loading` re-lead, because `ParkOnSlot` only
/// registered its waker on the first poll. The fix re-registers the
/// waker into the current `Loading` waker list on every poll. This
/// shrunk seed reproduced the lost wakeup. seed = 12581658376333696978.
#[test]
fn regression_windowed_cancel_under_pressure() {
    let w = Workload {
        page_size: 1024,
        page_count: 3,
        max_io_delay: 3,
        io_fault_rate: 0,
        cache_hit_rate: 10,
        page_cache_enabled: true,
        max_concurrent_streams: 1024,
        max_inflight_pages: 1,
        key_count: 2,
        clients: vec![
            ClientSpec {
                key_idx: 104,
                offset: 1024,
                len: 1025,
                cancel_after: Some(1),
                window: Some(2),
            },
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 1,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 19,
                offset: 0,
                len: 1,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 2,
                offset: 1325,
                len: 724,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 72,
                offset: 1544,
                len: 505,
                cancel_after: None,
                window: None,
            },
        ],
        pipelines: Vec::new(),
    };
    let report = run_workload(12581658376333696978, w).expect("must not deadlock");
    let _ = report;
}

/// Scenario test: windowed readers under free-list pressure must not
/// deadlock and must drain cleanly. `page_count` is smaller than the
/// aggregate window demand across clients, so speculative prefetch
/// competes with every stream's head for the few free pages. The
/// head-of-line guarantee (head launched and polled first, never
/// counting against the prefetch budget) is what keeps this from
/// deadlocking; a regression there surfaces as `RunError::Deadlock`
/// or budget exhaustion from `run_workload`. Mid-stream cancellation
/// is mixed in so the windowed `Drop` cleanup path is exercised too.
#[test]
fn windowed_under_free_list_pressure_drains() {
    let w = Workload {
        page_size: 64,
        page_count: 2,
        max_io_delay: 3,
        io_fault_rate: 0,
        cache_hit_rate: 0,
        page_cache_enabled: true,
        max_concurrent_streams: 1024,
        max_inflight_pages: 3,
        key_count: 2,
        clients: vec![
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 128,
                cancel_after: None,
                window: Some(4),
            },
            ClientSpec {
                key_idx: 1,
                offset: 0,
                len: 128,
                cancel_after: None,
                window: Some(4),
            },
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 128,
                cancel_after: Some(1),
                window: Some(3),
            },
            ClientSpec {
                key_idx: 1,
                offset: 64,
                len: 64,
                cancel_after: None,
                window: Some(2),
            },
        ],
        pipelines: Vec::new(),
    };
    // Sweep a handful of seeds so several interleavings of the
    // free-list races are covered by this one deterministic test.
    for seed in [0u64, 1, 7, 42, 1234, 99999] {
        let report = run_workload(seed, w.clone()).expect("must not deadlock");
        for o in &report.outcomes {
            match o {
                ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
                ClientOutcome::Cancelled { got, expected, .. } => {
                    assert!(got.len() <= expected.len());
                    assert_eq!(&got[..], &expected[..got.len()]);
                }
                other => panic!("unexpected outcome at seed {seed}: {other:?}"),
            }
        }
        assert_quiescent_accounting(&report, 2)
            .unwrap_or_else(|e| panic!("accounting leak at seed {seed}: {e}"));
        assert_eq!(
            report.prefetch_inflight_at_end, 0,
            "prefetch budget leak at seed {seed}"
        );
    }
}

/// Regression: speculative prefetch must never starve another
/// stream's in-order head of a backing page. Here four readers (three
/// windowed) contend for only three backing pages while speculation
/// runs ahead; without bounding the prefetch budget to leave one page
/// per active stream, each stream pinned a prefetched-ahead page in
/// its ready set while every head blocked on `free.alloc`, forming a
/// circular hold-and-wait. The fix caps the global prefetch budget at
/// `page_count - stream_count`. seed = 345758264357940050.
#[test]
fn regression_windowed_speculation_starves_head() {
    let w = Workload {
        page_size: 256,
        page_count: 3,
        max_io_delay: 2,
        io_fault_rate: 0,
        cache_hit_rate: 0,
        page_cache_enabled: true,
        max_concurrent_streams: 1024,
        max_inflight_pages: 5,
        key_count: 2,
        clients: vec![
            ClientSpec {
                key_idx: 71,
                offset: 1599,
                len: 1155,
                cancel_after: None,
                window: Some(3),
            },
            ClientSpec {
                key_idx: 32,
                offset: 768,
                len: 257,
                cancel_after: None,
                window: Some(2),
            },
            ClientSpec {
                key_idx: 48,
                offset: 256,
                len: 1,
                cancel_after: None,
                window: None,
            },
            ClientSpec {
                key_idx: 31,
                offset: 1849,
                len: 1110,
                cancel_after: None,
                window: Some(2),
            },
        ],
        pipelines: Vec::new(),
    };
    let report = run_workload(345758264357940050, w).expect("must not deadlock");
    let _ = report;
}

/// Scenario test: a single pipelined reader spanning several stripes
/// (distinct keys) delivers every page in global order and drains to
/// a clean state. With `max_inflight_pages` larger than one stripe's
/// worth of pages, the reader pipelines fetches across stripe
/// boundaries (proven separately by the module-integration overlap
/// test); here we pin the end-to-end correctness and accounting under
/// the deterministic executor across several seeds.
#[test]
fn pipelined_multi_stripe_in_order_and_drains() {
    let w = Workload {
        page_size: 64,
        page_count: 8,
        max_io_delay: 3,
        io_fault_rate: 0,
        cache_hit_rate: 0,
        page_cache_enabled: true,
        max_concurrent_streams: 1024,
        max_inflight_pages: 6,
        key_count: 3,
        clients: Vec::new(),
        pipelines: vec![PipelineSpec {
            cancel_after: None,
            slices: vec![
                PlanSliceSpec {
                    key_idx: 0,
                    offset: 0,
                    len: 128,
                },
                PlanSliceSpec {
                    key_idx: 1,
                    offset: 0,
                    len: 128,
                },
                PlanSliceSpec {
                    key_idx: 2,
                    offset: 32,
                    len: 96,
                },
            ],
        }],
    };
    for seed in [0u64, 1, 7, 42, 1234, 99999] {
        let report = run_workload(seed, w.clone()).expect("must not deadlock");
        for o in &report.outcomes {
            match o {
                ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
                other => panic!("unexpected outcome at seed {seed}: {other:?}"),
            }
        }
        assert_quiescent_accounting(&report, 8)
            .unwrap_or_else(|e| panic!("accounting leak at seed {seed}: {e}"));
        assert_eq!(
            report.prefetch_inflight_at_end, 0,
            "prefetch budget leak at seed {seed}"
        );
    }
}

/// Scenario test: pipelined readers under free-list pressure must not
/// deadlock and must drain cleanly. `page_count` is far smaller than
/// the aggregate page demand of the concurrent multi-stripe plans, so
/// each plan's in-order head competes with the others (and with its
/// own speculation) for the few free pages. The same head-of-line
/// guarantee that protects windowed reads protects the pipelined
/// reader: the global head is launched non-speculatively and polled
/// first, and `release_passed_slices` frees streams as the cursor
/// advances. A regression surfaces as `RunError::Deadlock` or budget
/// exhaustion. Mid-stream cancellation exercises the pipelined `Drop`
/// cleanup path.
#[test]
fn pipelined_under_free_list_pressure_drains() {
    let w = Workload {
        page_size: 64,
        page_count: 2,
        max_io_delay: 3,
        io_fault_rate: 0,
        cache_hit_rate: 0,
        page_cache_enabled: true,
        max_concurrent_streams: 1024,
        max_inflight_pages: 4,
        key_count: 3,
        clients: Vec::new(),
        pipelines: vec![
            PipelineSpec {
                cancel_after: None,
                slices: vec![
                    PlanSliceSpec {
                        key_idx: 0,
                        offset: 0,
                        len: 128,
                    },
                    PlanSliceSpec {
                        key_idx: 1,
                        offset: 0,
                        len: 128,
                    },
                ],
            },
            PipelineSpec {
                cancel_after: Some(1),
                slices: vec![
                    PlanSliceSpec {
                        key_idx: 2,
                        offset: 0,
                        len: 128,
                    },
                    PlanSliceSpec {
                        key_idx: 0,
                        offset: 64,
                        len: 64,
                    },
                ],
            },
            PipelineSpec {
                cancel_after: None,
                slices: vec![PlanSliceSpec {
                    key_idx: 1,
                    offset: 32,
                    len: 96,
                }],
            },
        ],
    };
    for seed in [0u64, 1, 7, 42, 1234, 99999] {
        let report = run_workload(seed, w.clone()).expect("must not deadlock");
        for o in &report.outcomes {
            match o {
                ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
                ClientOutcome::Cancelled { got, expected, .. } => {
                    assert!(got.len() <= expected.len());
                    assert_eq!(&got[..], &expected[..got.len()]);
                }
                other => panic!("unexpected outcome at seed {seed}: {other:?}"),
            }
        }
        assert_quiescent_accounting(&report, 2)
            .unwrap_or_else(|e| panic!("accounting leak at seed {seed}: {e}"));
        assert_eq!(
            report.prefetch_inflight_at_end, 0,
            "prefetch budget leak at seed {seed}"
        );
    }
}
