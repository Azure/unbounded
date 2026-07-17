// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Storage-engine DST property tests.

use proptest::prelude::*;

use crate::storage::oracle::Oracle;
use crate::storage::workload::{
    ClientSpec, Op, Outcome, RunReport, Workload, run_workload, workload_strategy,
    workload_strategy_no_faults_multi_client,
};

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 128,
        ..ProptestConfig::default()
    })]

    #[test]
    fn storage_invariants(seed in any::<u64>(), w in workload_strategy()) {
        // Build the assertion oracle independently of the driver so the
        // correctness checks do not depend on harness internals.
        let oracle = build_oracle(&w);
        let report = run_workload(seed, w.clone())
            .expect("run completed without deadlock or budget exhaustion");

        assert_bounded_termination(&report)?;
        assert_hit_bytes_were_written(&w, &report, &oracle)?;
        assert_no_false_hit_under_corruption(&report, &oracle)?;
        assert_unwritten_keys_miss(&report, &oracle)?;
        assert_snapshot_accounting(&w, &report)?;
        assert_restart_preserves_correctness(&w, &report, &oracle)?;
        assert_multidisk_routing_diverse(&report)?;
        assert_resident_pages_within_capacity(&report)?;
        assert_pending_free_drains(&w, &report)?;
        assert_max_inflight_bounded(&w, &report)?;
        assert_mutator_drains_at_shutdown(&report)?;
    }

    /// Invariant: no committed write is silently lost when
    /// multiple clients write the same `(key, offset)`
    /// concurrently.
    ///
    /// The general suite already enforces "no wrong bytes on a hit"
    /// via [`assert_hit_bytes_were_written`] and the oracle.
    /// What this check adds is an end-of-run sanity probe: a
    /// regression that let the mutator drop one of two coalesced
    /// inserts (singleflight follower vs leader) without
    /// committing either would manifest as the engine reporting
    /// `admitted > 0` while every read missed and `btree_entries`
    /// stayed at zero. That precise shape is what we look for
    /// here. We don't directly cross-reference per-write commit
    /// receipts because the mutator does not surface per-request
    /// commit counters to the harness today; documenting the gap
    /// (see comment) keeps the intent explicit for the next
    /// reader.
    ///
    /// Scoped tightly to happy-path runs: under fault injection
    /// admissions can legitimately survive without a backing
    /// btree entry (the data-page write failed after admission
    /// bumped its counter), so this exact shape is only a bug
    /// in the fault-free regime.
    #[test]
    fn invariant_no_lost_writes_under_concurrent_writers(
        seed in any::<u64>(), w in workload_strategy_no_faults_multi_client(),
    ) {
        // Strategy already pins `io_fault_rate == 0`,
        // `read_corrupt_rate == 0`, and `clients.len() >= 2`, so we
        // don't need a `prop_assume!` gate here. Doing the filtering
        // in the strategy keeps soak runs (high `PROPTEST_CASES`)
        // from tripping `max_global_rejects`.
        debug_assert_eq!(w.io_fault_rate, 0);
        debug_assert_eq!(w.read_corrupt_rate, 0);
        debug_assert!(w.clients.len() >= 2);
        let report = run_workload(seed, w).expect("run completed");
        if report.admitted == 0 {
            return Ok(());
        }
        // At least one admitted write must have produced an
        // observable btree entry. (Coalesced followers don't
        // each bump `btree_entries`, so we cannot equate the
        // two counts; we only assert the non-zero floor.)
        prop_assert!(
            report.btree_entries >= 1,
            "{} admissions but zero btree entries: every write was lost between \
             admission and commit (admitted={}, evictions={}, write_io_errors={})",
            report.admitted, report.admitted, report.evictions, report.write_io_errors,
        );
    }
}

fn build_oracle(w: &Workload) -> Oracle {
    let oracle = Oracle::new();
    for c in &w.clients {
        for op in &c.ops {
            if let Op::Write {
                key_idx,
                off_idx,
                payload_seed,
            } = op
            {
                oracle.record_write(
                    w.key(*key_idx),
                    w.offset(*off_idx),
                    w.payload(*key_idx, *off_idx, *payload_seed),
                );
            }
        }
    }
    oracle
}

/// Invariant: bounded termination.
///
/// `run_workload` returning `Ok` implies the executor neither deadlocked nor
/// exhausted its step budget. Re-asserting it explicitly pins the contract.
fn assert_bounded_termination(report: &RunReport) -> Result<(), TestCaseError> {
    prop_assert!(report.steps > 0);
    Ok(())
}

/// Invariant: every `ReadHit` returns bytes that the workload previously wrote
/// for that `(key, offset)`. Catches silent data corruption, mis-routed reads,
/// and cross-page bleed.
fn assert_hit_bytes_were_written(
    w: &Workload,
    report: &RunReport,
    oracle: &Oracle,
) -> Result<(), TestCaseError> {
    let faults_enabled = w.io_fault_rate > 0 || w.read_corrupt_rate > 0;
    for (i, o) in report.outcomes.iter().enumerate() {
        match o {
            Outcome::ReadHit { key, offset, bytes } => {
                prop_assert!(
                    oracle.allows_read(*key, *offset, bytes),
                    "client op {} returned a hit with bytes not previously written for ({:?}, {})",
                    i,
                    key.0,
                    offset,
                );
            }
            Outcome::Err(e) => {
                prop_assert!(
                    faults_enabled,
                    "op {} produced an error under happy-path config: {}",
                    i,
                    e,
                );
            }
            _ => {}
        }
    }
    Ok(())
}

/// Invariant: read corruption never produces a false hit.
///
/// The sim device may flip the first byte of any successful read. The engine's
/// per-page checksum must convert a corrupted data-page read into a miss rather
/// than exposing silent data corruption. Whether `checksum_misses` increments
/// depends on which device read was corrupted and belongs in a targeted test.
fn assert_no_false_hit_under_corruption(
    report: &RunReport,
    oracle: &Oracle,
) -> Result<(), TestCaseError> {
    for (i, o) in report.outcomes.iter().enumerate() {
        if let Outcome::ReadHit { key, offset, bytes } = o {
            prop_assert!(
                oracle.allows_read(*key, *offset, bytes),
                "op {} returned a corrupted hit for ({:?}, {}); first byte {:#x}",
                i,
                key.0,
                offset,
                bytes.first().copied().unwrap_or(0),
            );
        }
    }
    Ok(())
}

/// Invariant: reads for pages the workload never wrote must miss.
fn assert_unwritten_keys_miss(report: &RunReport, oracle: &Oracle) -> Result<(), TestCaseError> {
    for (i, o) in report.outcomes.iter().enumerate() {
        if let Outcome::ReadHit { key, offset, .. } = o {
            prop_assert!(
                oracle.was_written(*key, *offset),
                "op {} returned a hit for never-written ({:?}, {})",
                i,
                key.0,
                offset,
            );
        }
    }
    Ok(())
}

/// Invariant: snapshot accounting is self-consistent.
///
/// Read and write counters cannot exceed their corresponding workload calls.
/// Without faults, resident and indexed page counts must agree. Under faults,
/// failed eviction batches can leave up to `EVICT_SWEEP_TARGET` indexed pages
/// outside the LRU per device error, while corrupted prior-LBA probes can orphan
/// one LRU entry per injected corruption. `pending_free_len` provides the same
/// defensive headroom as before for transient reclaim bookkeeping.
fn assert_snapshot_accounting(w: &Workload, report: &RunReport) -> Result<(), TestCaseError> {
    const EVICT_SWEEP_TARGET: i64 = 8;

    let mut writes = 0u64;
    let mut reads = 0u64;
    for c in &w.clients {
        for op in &c.ops {
            match op {
                Op::Write { .. } => writes += 1,
                Op::Read { .. } => reads += 1,
            }
        }
    }
    let faults_enabled = w.io_fault_rate > 0 || w.read_corrupt_rate > 0;
    prop_assert!(
        report.hits + report.misses <= reads,
        "hits ({}) + misses ({}) > read calls ({})",
        report.hits,
        report.misses,
        reads,
    );
    prop_assert!(
        report.admitted + report.rejected_by_filter <= writes,
        "admitted ({}) + rejected ({}) > write calls ({})",
        report.admitted,
        report.rejected_by_filter,
        writes,
    );

    let resident = report.resident_pages as i64;
    let btree = report.btree_entries as i64;
    let diff = (resident - btree).abs();
    let bound = EVICT_SWEEP_TARGET * report.device_io_errors as i64
        + report.device_corruptions_injected as i64
        + report.pending_free_len as i64;
    prop_assert!(
        diff <= bound,
        "|resident_pages ({}) - btree_entries ({})| = {} exceeds bound {} \
         (device_io_errors={}, device_corruptions_injected={}, pending_free_len={}); \
         LRU and index diverged beyond what failed evictions and corrupted btree \
         reads can explain",
        resident,
        btree,
        diff,
        bound,
        report.device_io_errors,
        report.device_corruptions_injected,
        report.pending_free_len,
    );

    if !faults_enabled {
        prop_assert_eq!(
            report.resident_pages,
            report.btree_entries,
            "resident_pages ({}) != btree_entries ({}); LRU and index disagree \
             under fault-free config",
            report.resident_pages,
            report.btree_entries,
        );
    }
    Ok(())
}

/// Invariant: a clean restart preserves correctness.
///
/// Fault-free replay after reopen must visit every grid cell without errors,
/// and every hit must contain bytes previously written for that page. Misses
/// remain legal because eviction and partial rebuild are best-effort.
fn assert_restart_preserves_correctness(
    w: &Workload,
    report: &RunReport,
    oracle: &Oracle,
) -> Result<(), TestCaseError> {
    if !w.restart_after {
        prop_assert!(!report.restart_performed);
        prop_assert_eq!(report.post_restart_hits, 0);
        prop_assert_eq!(report.post_restart_misses, 0);
        prop_assert_eq!(report.post_restart_errors, 0);
        return Ok(());
    }
    if !report.restart_performed {
        prop_assert_eq!(report.post_restart_hits, 0);
        prop_assert_eq!(report.post_restart_misses, 0);
        prop_assert_eq!(report.post_restart_errors, 0);
        return Ok(());
    }
    prop_assert_eq!(
        report.post_restart_errors,
        0,
        "restart replay produced {} error(s) with faults disabled",
        report.post_restart_errors,
    );
    let grid = w.key_count.max(1) as u64 * w.offset_count.max(1) as u64;
    prop_assert_eq!(
        report.post_restart_hits + report.post_restart_misses,
        grid,
        "post-restart replay should visit every grid cell exactly once: hits={} misses={} grid={}",
        report.post_restart_hits,
        report.post_restart_misses,
        grid,
    );
    for (i, o) in report.outcomes.iter().enumerate() {
        if let Outcome::ReadHit { key, offset, bytes } = o {
            prop_assert!(
                oracle.allows_read(*key, *offset, bytes),
                "op {} (incl. restart replay) returned a hit with bytes not previously written for ({:?}, {})",
                i,
                key.0,
                offset,
            );
        }
    }
    Ok(())
}

/// Invariant: non-trivial multi-disk workloads route writes diversely.
///
/// Scope this to at least two disks and eight device writes. Below that
/// threshold, a healthy hash can legitimately route every write to one disk.
fn assert_multidisk_routing_diverse(report: &RunReport) -> Result<(), TestCaseError> {
    if report.num_disks_used < 2 || report.device_writes < 8 {
        return Ok(());
    }
    let touched = report
        .device_writes_per_disk
        .iter()
        .filter(|n| **n > 0)
        .count();
    prop_assert!(
        touched >= 2,
        "all {} device writes routed to a single disk across {} disks: {:?}",
        report.device_writes,
        report.num_disks_used,
        report.device_writes_per_disk,
    );
    Ok(())
}

/// Invariant: per-disk residency never exceeds capacity.
///
/// This asserts the hard capacity rather than the SIEVE watermark so the test
/// does not pin a policy knob.
fn assert_resident_pages_within_capacity(report: &RunReport) -> Result<(), TestCaseError> {
    for (i, n) in report.resident_pages_per_disk.iter().enumerate() {
        prop_assert!(
            (*n as u64) <= report.device_pages,
            "disk {} resident_pages={} exceeds device_pages={}",
            i,
            n,
            report.device_pages,
        );
    }
    Ok(())
}

/// Invariant: deferred reclaim remains bounded without injected faults.
///
/// Entries surviving quiescence reflect a retire-then-no-more-writes tail and
/// stay bounded by workload writes. Fault injection is excluded because failed
/// writes and checksum retries can legitimately interrupt the drain handshake.
fn assert_pending_free_drains(w: &Workload, report: &RunReport) -> Result<(), TestCaseError> {
    if w.io_fault_rate > 0 || w.read_corrupt_rate > 0 {
        return Ok(());
    }
    let writes = w
        .clients
        .iter()
        .flat_map(|c| &c.ops)
        .filter(|op| matches!(op, Op::Write { .. }))
        .count();
    prop_assert!(
        report.pending_free_len <= writes,
        "pending_free queue len={} exceeds bound (writes={}): drain path may be stuck",
        report.pending_free_len,
        writes,
    );
    Ok(())
}

/// Invariant: per-disk peak inflight I/O stays within plausible bounds.
///
/// The `clients + 4` limit is intentionally generous. It shape-checks device
/// accounting and await boundaries without pinning an exact concurrency level.
fn assert_max_inflight_bounded(w: &Workload, report: &RunReport) -> Result<(), TestCaseError> {
    let client_count = w.clients.len() as u32;
    let bound = client_count + 4;
    for (i, n) in report.max_inflight_per_disk.iter().enumerate() {
        prop_assert!(
            *n <= bound,
            "disk {} max_inflight={} exceeds plausible bound {} (clients={})",
            i,
            n,
            bound,
            client_count,
        );
    }
    Ok(())
}

/// Invariant: every engine's mutator queue drains at shutdown.
///
/// The harness closes each queue after clients finish and joins the mutator.
/// A remaining request would indicate lost committed state or notifications.
fn assert_mutator_drains_at_shutdown(report: &RunReport) -> Result<(), TestCaseError> {
    for (i, n) in report.mutator_pending_per_disk.iter().enumerate() {
        prop_assert_eq!(
            *n,
            0,
            "engine {} exited with {} mutator request(s) still buffered after shutdown",
            i,
            n,
        );
    }
    Ok(())
}

/// Smoke scenario: hand-tuned workload that exercises the write -> read
/// roundtrip path under modest delay, with no faults.
#[test]
fn smoke() {
    let w = Workload {
        page_size: 4096,
        device_pages: 64,
        max_io_delay: 2,
        io_fault_rate: 0,
        read_corrupt_rate: 0,
        key_count: 2,
        offset_count: 2,
        clients: vec![
            ClientSpec {
                ops: vec![
                    Op::Write {
                        key_idx: 0,
                        off_idx: 0,
                        payload_seed: 1,
                    },
                    Op::Write {
                        key_idx: 0,
                        off_idx: 0,
                        payload_seed: 1,
                    },
                    Op::Read {
                        key_idx: 0,
                        off_idx: 0,
                    },
                ],
            },
            ClientSpec {
                ops: vec![
                    Op::Write {
                        key_idx: 1,
                        off_idx: 1,
                        payload_seed: 9,
                    },
                    Op::Write {
                        key_idx: 1,
                        off_idx: 1,
                        payload_seed: 9,
                    },
                    Op::Read {
                        key_idx: 1,
                        off_idx: 1,
                    },
                ],
            },
        ],
        restart_after: false,
        num_disks: 1,
    };
    let report: RunReport = run_workload(0xC0FFEE, w).expect("smoke run");
    // At least one of the two reads should hit; admission filter
    // requires the second write before anything lands.
    let hits: usize = report
        .outcomes
        .iter()
        .filter(|o| matches!(o, Outcome::ReadHit { .. }))
        .count();
    assert!(
        hits >= 1,
        "expected at least one hit after double-write: {:?}",
        report.outcomes
    );
}

/// Sweep-complement to [`assert_max_inflight_bounded`]: a
/// hand-crafted workload that the engine cannot serialize,
/// proving the `SimBlockDevice` inflight counter actually
/// observes overlap when overlap is geometrically forced.
///
/// Several clients each admit one distinct key and then issue many
/// reads for that key with a generous `max_io_delay`. Reads are not
/// singleflight-gated and (unlike writes) do not funnel through the
/// mutator, so the executor can interleave device reads from
/// independent client tasks freely; with per-op `yield_n(delay)` and
/// the executor's random task pick, at least one engine must see two
/// device ops in flight at once. If this test ever observes peak == 1,
/// `SimBlockDevice` has stopped yielding through `await` or
/// the engine has acquired a global I/O lock that defeats
/// the per-disk concurrency the rest of the harness assumes.
#[test]
fn smoke_concurrent_reads_overlap() {
    // Each client first writes one distinct key twice so admission
    // promotes it, then repeatedly reads that key. The per-client
    // prefix avoids relying on a separate primer client completing
    // before readers start; after the prefixes, reads cannot be
    // collapsed and are numerous enough to force overlap under the
    // deterministic executor.
    let mut clients = Vec::new();
    let key_count = 16u8;
    for k in 0..key_count {
        let mut ops = Vec::new();
        ops.push(Op::Write {
            key_idx: k,
            off_idx: 0,
            payload_seed: k,
        });
        ops.push(Op::Write {
            key_idx: k,
            off_idx: 0,
            payload_seed: k,
        });
        for _ in 0..16 {
            ops.push(Op::Read {
                key_idx: k,
                off_idx: 0,
            });
        }
        clients.push(ClientSpec { ops });
    }
    let w = Workload {
        page_size: 4096,
        device_pages: 256,
        max_io_delay: 32,
        io_fault_rate: 0,
        read_corrupt_rate: 0,
        key_count,
        offset_count: 1,
        clients,
        restart_after: false,
        num_disks: 1,
    };
    let report = run_workload(0xDEADBEEF, w).expect("smoke run");
    let peak = report
        .max_inflight_per_disk
        .iter()
        .copied()
        .max()
        .unwrap_or(0);
    assert!(
        peak >= 2,
        "engine never saw overlapping device ops despite 16 concurrent readers over 16 \
         admitted keys with max_io_delay=32: per-disk peaks={:?}, device_reads={}, \
         device_writes={}",
        report.max_inflight_per_disk,
        report.device_reads,
        report.device_writes,
    );
}
