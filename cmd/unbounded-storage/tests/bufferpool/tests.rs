// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use proptest::prelude::*;

use crate::bufferpool::workload::{
    ClientOutcome, ClientSpec, RunReport, Workload, run_workload, workload_strategy,
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
        key_count: 1,
        clients: vec![
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 128,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 128,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 128,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 128,
                cancel_after: None,
            },
        ],
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
        key_count: 2,
        clients: vec![
            ClientSpec {
                key_idx: 0,
                offset: 640,
                len: 1,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 55,
                offset: 2048,
                len: 1,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 14,
                offset: 256,
                len: 1,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 1664,
                len: 1,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 103,
                offset: 640,
                len: 1,
                cancel_after: None,
            },
        ],
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
        key_count: 2,
        clients: vec![
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 256,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 512,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 1,
                offset: 0,
                len: 1024,
                cancel_after: None,
            },
        ],
    };
    let report: RunReport = run_workload(0xC0FFEE, w).expect("smoke run");
    assert_eq!(report.free_pages_at_end, 4);
    assert_eq!(report.inflight_entries_at_end, 0);
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
        key_count: 2,
        clients: vec![
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 1024,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 1,
                offset: 64,
                len: 512,
                cancel_after: None,
            },
            ClientSpec {
                key_idx: 0,
                offset: 100,
                len: 200,
                cancel_after: None,
            },
        ],
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
        key_count: 2,
        clients: vec![
            // Full read.
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 512,
                cancel_after: None,
            },
            // Cancels before first page.
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 512,
                cancel_after: Some(0),
            },
            // Cancels after the first page.
            ClientSpec {
                key_idx: 1,
                offset: 0,
                len: 512,
                cancel_after: Some(1),
            },
            // Cancels after two pages on the same key as the full
            // reader so the slot's `stream_refcount` interleaves
            // increments and decrements.
            ClientSpec {
                key_idx: 0,
                offset: 0,
                len: 512,
                cancel_after: Some(2),
            },
        ],
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
