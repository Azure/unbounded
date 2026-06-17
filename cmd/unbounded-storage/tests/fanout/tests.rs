// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Property tests for the cross-shard fan-out path. Each invariant is
//! a small `assert_*` helper returning `Result<(), TestCaseError>`, so
//! shrinking output stays legible. A single `proptest!` block drives
//! `run_workload` once per case and dispatches to every invariant.

use proptest::prelude::*;

use crate::fanout::workload::{FetchOutcome, RunReport, run_workload, workload_strategy};

proptest! {
    #![proptest_config(ProptestConfig {
        // Keep CI runtime modest; bump locally (or via
        // `PROPTEST_CASES`) for soak runs.
        cases: 256,
        ..ProptestConfig::default()
    })]

    #[test]
    fn fanout_invariants(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("run completed without deadlock or budget exhaustion");
        assert_bytes_match(&report)?;
        assert_pagelocs_cover(&report)?;
        assert_no_pin_leak(&report)?;
        assert_faults_only_with_injection(&report)?;
        assert_shutdown_send_errors(&report)?;
        assert_busy_only_under_pressure(&report)?;
    }
}

/// Invariant: zero-copy byte correctness across the round-trip.
///
/// For every successful fetch, the bytes read from the owner backing
/// at the returned `PageLoc`s - after the `hold` delay, while the owner
/// still holds the pages pinned - must equal the oracle slice. This is
/// the core guarantee: the owner's pin keeps the source pages valid and
/// unmodified from reply until release, so the coordinator's `SEND_ZC`
/// (modeled here as a direct memory read) observes correct bytes.
fn assert_bytes_match(report: &RunReport) -> Result<(), TestCaseError> {
    for (i, o) in report.outcomes.iter().enumerate() {
        if let FetchOutcome::Ok { got, expected, .. } = o {
            prop_assert_eq!(got, expected, "fetch {} bytes mismatch", i);
        }
    }
    Ok(())
}

/// Invariant: the reply's `PageLoc`s exactly cover the requested range
/// and stay within the owner backing.
///
/// The concatenated `PageLoc` lengths must equal the requested byte
/// length, each loc must be non-empty (every requested fetch has
/// `len >= 1`), and `page_byte_offset + len` must fit inside the pool's
/// backing. A loc pointing outside the backing would mean the
/// coordinator reads out of bounds during `SEND_ZC`.
fn assert_pagelocs_cover(report: &RunReport) -> Result<(), TestCaseError> {
    let backing_bytes = (report.total_pool_pages * report.page_size) as u64;
    for (i, o) in report.outcomes.iter().enumerate() {
        if let FetchOutcome::Ok { page_locs, len, .. } = o {
            let covered: u64 = page_locs.iter().map(|(_, l)| *l as u64).sum();
            prop_assert_eq!(
                covered,
                *len,
                "fetch {} pagelocs cover {} bytes, expected {}",
                i,
                covered,
                len,
            );
            for (off, l) in page_locs {
                prop_assert!(*l > 0, "fetch {} has an empty PageLoc", i);
                prop_assert!(
                    off + *l as u64 <= backing_bytes,
                    "fetch {} PageLoc {}+{} exceeds backing {}",
                    i,
                    off,
                    l,
                    backing_bytes,
                );
            }
        }
    }
    Ok(())
}

/// Invariant: no pin or stripe-fetch leak at quiescence.
///
/// After every client has released its pins and the service has
/// drained, all pool pages must be back on the free list and the
/// inflight stripe-fetch map must be empty. This catches leaks in the
/// owner read path, the `Release` handling, and the fetch-error path
/// (which drops its partial guards without inserting a pin). Must hold
/// under fault injection too.
fn assert_no_pin_leak(report: &RunReport) -> Result<(), TestCaseError> {
    prop_assert_eq!(
        report.free_pages_at_end,
        report.total_pool_pages,
        "pages leaked: free={} expected {}",
        report.free_pages_at_end,
        report.total_pool_pages,
    );
    prop_assert_eq!(
        report.inflight_entries_at_end,
        0,
        "inflight stripe-fetches not drained: {} entries",
        report.inflight_entries_at_end,
    );
    Ok(())
}

/// Invariant: fetch errors only occur under fault injection.
///
/// With `io_fault_rate == 0` the owner read path never surfaces an error
/// to the coordinator, so every fetch must resolve `Ok`. This now also
/// guards the page-pressure path: under `tight_pool` the owner returns
/// `Error::Busy` when it cannot pin, but coordinators retry until they
/// succeed, so `Busy` must never leak out as a `FetchOutcome::Err`. A
/// happy-path run producing a `FetchOutcome::Err` is itself a bug.
fn assert_faults_only_with_injection(report: &RunReport) -> Result<(), TestCaseError> {
    if report.io_fault_rate == 0 {
        for (i, o) in report.outcomes.iter().enumerate() {
            if let FetchOutcome::Err(e) = o {
                prop_assert!(false, "fetch {} errored ({}) with io_fault_rate=0", i, e);
            }
        }
    }
    Ok(())
}

/// Invariant: a fetch after service shutdown errors rather than parks.
///
/// When the probe ran, the service task had already returned and
/// dropped the receiver, so the surviving channel clone's `fetch` must
/// resolve with an error (the framework would otherwise hang, which the
/// probe's bounded spin would have turned into a panic).
fn assert_shutdown_send_errors(report: &RunReport) -> Result<(), TestCaseError> {
    if let Some(errored) = report.post_shutdown_send_errored {
        prop_assert!(errored, "post-shutdown fetch unexpectedly succeeded");
    }
    Ok(())
}

/// Invariant: `Error::Busy` back-pressure only arises under page
/// pressure.
///
/// The owner read path returns `Busy` (and coordinators retry) only when
/// the pool cannot pin a fetch's head page non-blockingly. The generous
/// pool sizing holds every distinct stripe at once, so it must never
/// produce a retry; any `busy_retries` there would mean the fail-fast
/// path fired when there was capacity. A positive count is expected only
/// under `tight_pool`.
fn assert_busy_only_under_pressure(report: &RunReport) -> Result<(), TestCaseError> {
    if !report.tight_pool {
        prop_assert_eq!(
            report.busy_retries,
            0,
            "{} Busy retries on a generously sized pool",
            report.busy_retries,
        );
    }
    Ok(())
}
