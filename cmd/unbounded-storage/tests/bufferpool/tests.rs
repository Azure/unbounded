// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use proptest::prelude::*;

use crate::bufferpool::workload::{
    ClientOutcome, ClientSpec, PipelineSpec, PlanSliceSpec, RunReport, Workload,
    pipelined_workload_strategy, run_workload, workload_strategy,
};

proptest! {
    #![proptest_config(ProptestConfig {
        // Keep CI runtime modest; bump locally (or via
        // `PROPTEST_CASES`) for soak runs.
        cases: 256,
        ..ProptestConfig::default()
    })]

    /// Invariant: byte correctness for successful clients.
    /// For every client that finishes via `ClientOutcome::Ok`,
    /// `PageGuard::as_slice()` bytes concatenate to the oracle
    /// slice for the client's `(key, offset, len)`. `FetchErr` is
    /// tolerated only under fault injection (`io_fault_rate > 0`);
    /// a happy-path run that produces a `FetchErr` is itself a bug.
    /// `ReadErr` is tolerated only when it's `StreamLimit` (the
    /// only `Pool::read` error the harness can produce); other
    /// `ReadErr` variants would indicate a regression.
    #[test]
    fn invariant_byte_correctness(seed in any::<u64>(), w in workload_strategy()) {
        let faults_enabled = w.io_fault_rate > 0;
        let report = run_workload(seed, w).expect("run completed");
        for (i, o) in report.outcomes.iter().enumerate() {
            match o {
                ClientOutcome::Ok { got, expected } => {
                    prop_assert_eq!(got, expected, "client {} bytes mismatch", i);
                }
                ClientOutcome::Cancelled { got, expected, .. } => {
                    prop_assert!(
                        got.len() <= expected.len(),
                        "client {} cancelled with got.len()={} > expected.len()={}",
                        i, got.len(), expected.len(),
                    );
                    prop_assert_eq!(
                        &got[..], &expected[..got.len()],
                        "client {} cancelled-prefix bytes mismatch", i,
                    );
                }
                ClientOutcome::ReadErr(e) => {
                    prop_assert!(
                        is_stream_limit(e),
                        "client {} unexpected read error: {}", i, e,
                    );
                }
                ClientOutcome::FetchErr(e) => {
                    prop_assert!(
                        faults_enabled,
                        "client {} got FetchErr ({}) with io_fault_rate=0",
                        i, e,
                    );
                }
            }
        }
    }

    /// Invariant: bounded termination.
    /// `run_workload` returning `Ok` already implies the executor
    /// neither deadlocked nor exhausted its step budget. The
    /// property re-asserts that explicitly so a future refactor
    /// can't silently flip the return type.
    #[test]
    fn invariant_bounded_termination(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("run completed without deadlock or budget exhaustion");
        // Sanity: every spawned client produced an outcome.
        prop_assert!(!report.outcomes.is_empty());
    }

    /// Invariant: page accounting at quiescence.
    /// Once every client has dropped its `ReadStream` and any tee
    /// has drained, all pages must be back on the free list and
    /// the inflight map must be empty. This catches leaks in the
    /// recycle paths (`release_guard`, `decrement_stream`,
    /// `TeeGuard::drop`, `LeaderGuard::drop`, and the
    /// `ParkOutcome::Error` cleanup path).
    ///
    /// Must hold under fault injection too: error paths are not
    /// allowed to leak pages or inflight entries.
    #[test]
    fn invariant_page_accounting_at_quiescence(seed in any::<u64>(), w in workload_strategy()) {
        let page_count = w.page_count;
        let report = run_workload(seed, w).expect("run completed");
        prop_assert_eq!(
            report.free_pages_at_end, page_count,
            "free_pages={} expected {}", report.free_pages_at_end, page_count,
        );
        prop_assert_eq!(
            report.inflight_entries_at_end, 0,
            "inflight not drained: {} entries", report.inflight_entries_at_end,
        );
        prop_assert_eq!(
            report.prefetch_inflight_at_end, 0,
            "prefetch budget not released: {} pages still reserved",
            report.prefetch_inflight_at_end,
        );
    }

    /// Invariant: single-flight coalescing per page.
    /// For any `(key, page_no)` the pool issued `bulk_get` to, at
    /// most one `bulk_get` may be in flight at a time. Sequential
    /// re-issues (slot recycled and later refetched - including
    /// after a transport-error tear-down) are allowed; concurrent
    /// ones violate single-flight.
    #[test]
    fn invariant_single_flight_per_page(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("run completed");
        for (k, n) in &report.bulk_get_max_inflight {
            prop_assert!(
                *n <= 1,
                "(key {:?}, page {}) had {} concurrent bulk_get calls; expected single-flight",
                k.0, k.1, n,
            );
        }
    }

    /// Invariant: error outcomes do not stall the pool.
    /// For workloads with fault injection, every client must
    /// terminate with either `Ok` or `FetchErr` (never hang), and
    /// the pool must still drain pages + inflight at quiescence.
    /// This is the "no leaks on error paths" property that the
    /// recycle-after-Error logic in `Action::Park` (and the
    /// leader's error branch) is responsible for.
    #[test]
    fn invariant_no_leak_under_faults(seed in any::<u64>(), w in workload_strategy()) {
        let page_count = w.page_count;
        let report = run_workload(seed, w).expect("run completed under faults");
        for (i, o) in report.outcomes.iter().enumerate() {
            match o {
                ClientOutcome::Ok { .. }
                | ClientOutcome::Cancelled { .. }
                | ClientOutcome::FetchErr(_) => {}
                ClientOutcome::ReadErr(e) => {
                    prop_assert!(
                        is_stream_limit(e),
                        "client {} unexpected ReadErr: {}", i, e,
                    );
                }
            }
        }
        prop_assert_eq!(
            report.free_pages_at_end, page_count,
            "free_pages={} expected {} (leak on error path?)",
            report.free_pages_at_end, page_count,
        );
        prop_assert_eq!(
            report.inflight_entries_at_end, 0,
            "inflight not drained under faults: {} entries (leak on error path?)",
            report.inflight_entries_at_end,
        );
    }

    /// Invariant: `max_concurrent_streams` enforcement.
    /// Every `ReadErr` outcome must be `Error::StreamLimit` (the
    /// only `Pool::read` error the harness can produce), and the
    /// pool must still drain pages + inflight at quiescence even
    /// when some `read()` calls were rejected. When the workload
    /// gives the pool enough headroom (limit >= clients), the
    /// reject path must not fire at all.
    #[test]
    fn invariant_stream_limit_bounds(seed in any::<u64>(), w in workload_strategy()) {
        let page_count = w.page_count;
        let limit = w.max_concurrent_streams;
        let n_clients = w.clients.len();
        let report = run_workload(seed, w).expect("run completed");
        let mut rejects = 0usize;
        for (i, o) in report.outcomes.iter().enumerate() {
            if let ClientOutcome::ReadErr(e) = o {
                prop_assert!(
                    is_stream_limit(e),
                    "client {} unexpected ReadErr: {}", i, e,
                );
                rejects += 1;
            }
        }
        if limit >= n_clients {
            prop_assert_eq!(
                rejects, 0,
                "limit={} >= clients={} but {} streams were rejected",
                limit, n_clients, rejects,
            );
        }
        prop_assert!(
            rejects <= n_clients,
            "more rejects ({}) than clients ({})", rejects, n_clients,
        );
        prop_assert_eq!(
            report.free_pages_at_end, page_count,
            "free_pages={} expected {} after StreamLimit rejects",
            report.free_pages_at_end, page_count,
        );
        prop_assert_eq!(
            report.inflight_entries_at_end, 0,
            "inflight not drained after StreamLimit rejects: {} entries",
            report.inflight_entries_at_end,
        );
    }

    /// Invariant: drain on mid-stream cancellation.
    /// A `ClientSpec.cancel_after = Some(k)` client drops its
    /// `ReadStream` after `k` successful `next_page` calls (or
    /// immediately when `k == 0`). The pool must drain to a clean
    /// state regardless: all pages back on the free list and no
    /// inflight entries left, even when cancellations interleave
    /// with faults, stream-limit rejects, and full reads. This
    /// explicitly exercises the partial-iteration paths in
    /// `decrement_stream` (slot in Idle/Loading at the time of
    /// drop) and `release_guard` (last guard for a tee-pending
    /// page).
    #[test]
    fn invariant_drain_on_cancellation(seed in any::<u64>(), w in workload_strategy()) {
        let page_count = w.page_count;
        let report = run_workload(seed, w).expect("run completed");
        // Bytes for cancelled clients must be a prefix of the oracle.
        for (i, o) in report.outcomes.iter().enumerate() {
            if let ClientOutcome::Cancelled { got, expected, .. } = o {
                prop_assert!(
                    got.len() <= expected.len(),
                    "client {} cancelled with got.len()={} > expected.len()={}",
                    i, got.len(), expected.len(),
                );
                prop_assert_eq!(
                    &got[..], &expected[..got.len()],
                    "client {} cancelled-prefix bytes mismatch", i,
                );
            }
        }
        prop_assert_eq!(
            report.free_pages_at_end, page_count,
            "free_pages={} expected {} (leak on cancellation drain?)",
            report.free_pages_at_end, page_count,
        );
        prop_assert_eq!(
            report.inflight_entries_at_end, 0,
            "inflight not drained after cancellations: {} entries",
            report.inflight_entries_at_end,
        );
    }
}

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 256,
        ..ProptestConfig::default()
    })]

    /// Invariant: pipelined byte correctness with in-order delivery.
    /// `read_pipelined` admits one stream per active plan slice and
    /// pipelines page fetches across stripe boundaries, yet must
    /// still deliver pages strictly in global plan order. For every
    /// pipeline that finishes via `Ok`, the concatenated
    /// `PageGuard` bytes must equal the oracle bytes for the plan's
    /// ordered `(key, offset, len)` slices. Cancelled pipelines must
    /// yield a prefix of the expected bytes (in-order truncation).
    /// `FetchErr` is tolerated only under fault injection; `ReadErr`
    /// only when it is `StreamLimit` (the pinned-high limit makes
    /// this effectively unreachable, but we tolerate it rather than
    /// assert a stronger property the harness does not guarantee).
    #[test]
    fn pipelined_invariant_byte_correctness(
        seed in any::<u64>(),
        w in pipelined_workload_strategy(),
    ) {
        let faults_enabled = w.io_fault_rate > 0;
        let report = run_workload(seed, w).expect("run completed");
        for (i, o) in report.outcomes.iter().enumerate() {
            match o {
                ClientOutcome::Ok { got, expected } => {
                    prop_assert_eq!(got, expected, "pipeline {} bytes mismatch", i);
                }
                ClientOutcome::Cancelled { got, expected, .. } => {
                    prop_assert!(
                        got.len() <= expected.len(),
                        "pipeline {} cancelled with got.len()={} > expected.len()={}",
                        i, got.len(), expected.len(),
                    );
                    prop_assert_eq!(
                        &got[..], &expected[..got.len()],
                        "pipeline {} cancelled-prefix bytes mismatch", i,
                    );
                }
                ClientOutcome::ReadErr(e) => {
                    prop_assert!(
                        is_stream_limit(e),
                        "pipeline {} unexpected read error: {}", i, e,
                    );
                }
                ClientOutcome::FetchErr(e) => {
                    prop_assert!(
                        faults_enabled,
                        "pipeline {} got FetchErr ({}) with io_fault_rate=0",
                        i, e,
                    );
                }
            }
        }
    }

    /// Invariant: pipelined page accounting at quiescence.
    /// The cross-stripe reader admits and releases one stream per
    /// slice as the global cursor advances (`release_passed_slices`)
    /// and shares the global prefetch budget. Once every pipeline
    /// has dropped its `PipelinedRead`, all pages must be back on
    /// the free list, the inflight map empty, and the prefetch
    /// budget fully released. Catches leaks in `release_guard`,
    /// `release_prefetch`, and the per-slice `decrement_stream`.
    /// Must hold under faults and mid-stream cancellation too.
    #[test]
    fn pipelined_invariant_page_accounting(
        seed in any::<u64>(),
        w in pipelined_workload_strategy(),
    ) {
        let page_count = w.page_count;
        let report = run_workload(seed, w).expect("run completed");
        prop_assert_eq!(
            report.free_pages_at_end, page_count,
            "free_pages={} expected {}", report.free_pages_at_end, page_count,
        );
        prop_assert_eq!(
            report.inflight_entries_at_end, 0,
            "inflight not drained: {} entries", report.inflight_entries_at_end,
        );
        prop_assert_eq!(
            report.prefetch_inflight_at_end, 0,
            "prefetch budget not released: {} pages still reserved",
            report.prefetch_inflight_at_end,
        );
    }

    /// Invariant: single-flight coalescing survives pipelining.
    /// Even though the pipelined reader may have many pages in
    /// flight across stripes (and the same stripe can appear in
    /// more than one plan slice), no `(key, page_no)` may have more
    /// than one concurrent `bulk_get`. Sequential re-issues remain
    /// allowed.
    #[test]
    fn pipelined_invariant_single_flight(
        seed in any::<u64>(),
        w in pipelined_workload_strategy(),
    ) {
        let report = run_workload(seed, w).expect("run completed");
        for (k, n) in &report.bulk_get_max_inflight {
            prop_assert!(
                *n <= 1,
                "(key {:?}, page {}) had {} concurrent bulk_get calls; expected single-flight",
                k.0, k.1, n,
            );
        }
    }

    /// Invariant: pipelined bounded termination.
    /// `run_workload` returning `Ok` already implies no deadlock or
    /// budget exhaustion; re-assert it explicitly and confirm every
    /// spawned pipeline produced an outcome. This is the deadlock
    /// guard for the cross-stripe driver under page pressure.
    #[test]
    fn pipelined_invariant_bounded_termination(
        seed in any::<u64>(),
        w in pipelined_workload_strategy(),
    ) {
        let report = run_workload(seed, w)
            .expect("run completed without deadlock or budget exhaustion");
        prop_assert!(!report.outcomes.is_empty());
    }
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
    assert_eq!(report.free_pages_at_end, 2);
    assert_eq!(report.inflight_entries_at_end, 0);
}
/// `Error::StreamLimit`'s Display string from
/// `src/bufferpool/types.rs`. We match on the message because the
/// harness flattens errors to `String` at the framework boundary.
fn is_stream_limit(msg: &str) -> bool {
    msg == "max_concurrent_streams reached"
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
    assert_eq!(report.free_pages_at_end, 2);
    assert_eq!(report.inflight_entries_at_end, 0);
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
    assert_eq!(report.free_pages_at_end, 4);
    assert_eq!(report.inflight_entries_at_end, 0);
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
    assert_eq!(report.free_pages_at_end, 4);
    assert_eq!(report.inflight_entries_at_end, 0);
    for o in &report.outcomes {
        match o {
            ClientOutcome::Ok { got, expected } => assert_eq!(got, expected),
            other => panic!("unexpected outcome: {other:?}"),
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
    assert_eq!(
        report.free_pages_at_end, 4,
        "all pages must return to the free list"
    );
    assert_eq!(
        report.inflight_entries_at_end, 0,
        "all inflight entries must drain"
    );

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
    assert_eq!(report.free_pages_at_end, 6);
    assert_eq!(report.inflight_entries_at_end, 0);
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
        assert_eq!(report.free_pages_at_end, 2, "leak at seed {seed}");
        assert_eq!(
            report.inflight_entries_at_end, 0,
            "inflight leak at seed {seed}"
        );
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
        assert_eq!(report.free_pages_at_end, 8, "leak at seed {seed}");
        assert_eq!(
            report.inflight_entries_at_end, 0,
            "inflight leak at seed {seed}"
        );
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
        assert_eq!(report.free_pages_at_end, 2, "leak at seed {seed}");
        assert_eq!(
            report.inflight_entries_at_end, 0,
            "inflight leak at seed {seed}"
        );
        assert_eq!(
            report.prefetch_inflight_at_end, 0,
            "prefetch budget leak at seed {seed}"
        );
    }
}
