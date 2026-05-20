// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Storage-engine DST property tests.

use proptest::prelude::*;

use crate::storage::oracle::Oracle;
use crate::storage::workload::{
    ClientSpec, Op, Outcome, RunReport, Workload, run_workload, workload_strategy,
};

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 128,
        ..ProptestConfig::default()
    })]

    /// Invariant: bounded termination.
    /// `run_workload` returning `Ok` implies the executor neither
    /// deadlocked nor exhausted its step budget. Re-asserting it
    /// explicitly pins the contract.
    #[test]
    fn invariant_bounded_termination(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("run completed without deadlock or budget exhaustion");
        prop_assert!(report.steps > 0);
    }

    /// Invariant: every `ReadHit` returns bytes that the workload
    /// previously wrote for that `(key, offset)`. Catches silent
    /// data corruption, mis-routed reads, and cross-page bleed.
    #[test]
    fn invariant_hit_bytes_were_written(seed in any::<u64>(), w in workload_strategy()) {
        // Rebuild the oracle alongside the run so the assertion is
        // independent of the harness internals.
        let oracle = Oracle::new();
        for c in &w.clients {
            for op in &c.ops {
                if let Op::Write { key_idx, off_idx, payload_seed } = op {
                    oracle.record_write(
                        w.key(*key_idx),
                        w.offset(*off_idx),
                        w.payload(*key_idx, *off_idx, *payload_seed),
                    );
                }
            }
        }
        let faults_enabled = w.io_fault_rate > 0;
        let report = run_workload(seed, w).expect("run completed");
        for (i, o) in report.outcomes.iter().enumerate() {
            match o {
                Outcome::ReadHit { key, offset, bytes } => {
                    prop_assert!(
                        oracle.allows_read(*key, *offset, bytes),
                        "client op {} returned a hit with bytes not previously written for ({:?}, {})",
                        i, key.0, offset,
                    );
                }
                Outcome::Err(e) => {
                    prop_assert!(
                        faults_enabled,
                        "op {} produced an error under happy-path config: {}", i, e,
                    );
                }
                _ => {}
            }
        }
    }

    /// Invariant: a read for a `(key, offset)` the workload never
    /// wrote must miss. This is the dual of the byte-correctness
    /// invariant and catches false-positive hit reports.
    #[test]
    fn invariant_unwritten_keys_miss(seed in any::<u64>(), w in workload_strategy()) {
        let oracle = Oracle::new();
        for c in &w.clients {
            for op in &c.ops {
                if let Op::Write { key_idx, off_idx, payload_seed } = op {
                    oracle.record_write(
                        w.key(*key_idx),
                        w.offset(*off_idx),
                        w.payload(*key_idx, *off_idx, *payload_seed),
                    );
                }
            }
        }
        let report = run_workload(seed, w).expect("run completed");
        for (i, o) in report.outcomes.iter().enumerate() {
            if let Outcome::ReadHit { key, offset, .. } = o {
                prop_assert!(
                    oracle.was_written(*key, *offset),
                    "op {} returned a hit for never-written ({:?}, {})",
                    i, key.0, offset,
                );
            }
        }
    }

    /// Invariant: snapshot accounting is self-consistent.
    /// - `hits + misses` cannot exceed the number of `read_page`
    ///   calls in the workload.
    /// - `admitted + rejected_by_filter` cannot exceed the number
    ///   of `write_page` calls in the workload (singleflight
    ///   followers and write-I/O errors contribute the slack).
    /// - `resident_pages == btree_entries` at quiescence: the
    ///   engine's LRU and on-disk index must agree on how many
    ///   pages it is caching. Without the replace-LBA reclaim in
    ///   `write_page`, repeated overwrites of the same
    ///   `(key, offset)` orphan cache entries and break this
    ///   equality. Only asserted when faults are disabled because
    ///   a failed eviction batch under injected faults can leave
    ///   the two counters transiently inconsistent.
    #[test]
    fn invariant_snapshot_accounting(seed in any::<u64>(), w in workload_strategy()) {
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
        let faults_enabled = w.io_fault_rate > 0;
        let report = run_workload(seed, w).expect("run completed");
        prop_assert!(
            report.hits + report.misses <= reads,
            "hits ({}) + misses ({}) > read calls ({})",
            report.hits, report.misses, reads,
        );
        prop_assert!(
            report.admitted + report.rejected_by_filter <= writes,
            "admitted ({}) + rejected ({}) > write calls ({})",
            report.admitted, report.rejected_by_filter, writes,
        );
        if !faults_enabled {
            prop_assert_eq!(
                report.resident_pages, report.btree_entries,
                "resident_pages ({}) != btree_entries ({}); LRU and index disagree",
                report.resident_pages, report.btree_entries,
            );
        }
    }
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
