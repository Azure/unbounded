// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Cross-core `PageChannel` DST property tests.
//!
//! Each generated case drives one randomized workload end to end over real
//! [`PageChannel`]s, then checks every invariant against the resulting report.
//! A couple of hand-built smoke tests pin the round-trip and shutdown scenarios
//! deterministically.
//!
//! [`PageChannel`]: unbounded_storage::storage::PageChannel

use proptest::prelude::*;

use crate::page_channel::workload::{
    ClientSpec, Op, Outcome, RunReport, Workload, run_workload, workload_strategy,
};
use crate::storage::oracle::Oracle;

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 256,
        ..ProptestConfig::default()
    })]

    #[test]
    fn page_channel_invariants(seed in any::<u64>(), w in workload_strategy()) {
        let oracle = oracle_for_workload(&w);
        let report = run_workload(seed, w.clone())
            .expect("run completed without deadlock or budget exhaustion");
        assert_bounded_termination(&report)?;
        assert_counters_consistent(&w, &report)?;
        assert_shutdown_send_fails(&w, &report)?;
        assert_hit_bytes_were_written(&report, &oracle)?;
        assert_unwritten_keys_miss(&report, &oracle)?;
    }
}

/// Invariant: the executor terminates within its step budget while driving the
/// complete channel and storage-core path.
fn assert_bounded_termination(report: &RunReport) -> Result<(), TestCaseError> {
    prop_assert!(report.steps > 0);
    Ok(())
}

/// Invariant: aggregate counters agree with workload and per-disk accounting.
fn assert_counters_consistent(w: &Workload, report: &RunReport) -> Result<(), TestCaseError> {
    let reads = w
        .clients
        .iter()
        .flat_map(|c| &c.ops)
        .filter(|op| matches!(op, Op::Read { .. }))
        .count() as u64;
    prop_assert!(
        report.hits + report.misses <= reads,
        "hits ({}) + misses ({}) > read calls ({})",
        report.hits,
        report.misses,
        reads,
    );
    let per_disk_sum: u64 = report.device_writes_per_disk.iter().copied().sum();
    prop_assert_eq!(
        per_disk_sum,
        report.device_writes,
        "device_writes ({}) disagrees with per-disk sum ({})",
        report.device_writes,
        per_disk_sum,
    );
    Ok(())
}

/// Invariant: a send after service shutdown fails instead of parking forever on
/// a reply slot whose receiver has exited.
fn assert_shutdown_send_fails(w: &Workload, report: &RunReport) -> Result<(), TestCaseError> {
    match report.post_shutdown_send_errored {
        Some(errored) => prop_assert!(
            errored,
            "post-shutdown send unexpectedly succeeded; the service did not fail it",
        ),
        None => prop_assert!(
            !w.probe_shutdown || report.outcomes.iter().any(|o| matches!(o, Outcome::Err(_))),
            "probe requested but no channel existed, yet bootstrap reported no error",
        ),
    }
    Ok(())
}

/// Invariant: every reported hit contains bytes submitted by a workload write
/// for the same logical page.
fn assert_hit_bytes_were_written(report: &RunReport, oracle: &Oracle) -> Result<(), TestCaseError> {
    for (idx, outcome) in report.outcomes.iter().enumerate() {
        if let Outcome::ReadHit { key, offset, bytes } = outcome {
            prop_assert!(
                oracle.allows_read(*key, *offset, bytes),
                "read hit {} returned bytes never written for key {:?} offset {}",
                idx,
                key,
                offset,
            );
        }
    }
    Ok(())
}

/// Invariant: a logical page the workload never wrote cannot be reported as a
/// hit; misses and injected errors remain legal.
fn assert_unwritten_keys_miss(report: &RunReport, oracle: &Oracle) -> Result<(), TestCaseError> {
    for (idx, outcome) in report.outcomes.iter().enumerate() {
        if let Outcome::ReadHit { key, offset, .. } = outcome {
            prop_assert!(
                oracle.was_written(*key, *offset),
                "read hit {} for never-written key {:?} offset {}",
                idx,
                key,
                offset,
            );
        }
    }
    Ok(())
}

fn oracle_for_workload(w: &Workload) -> Oracle {
    let oracle = Oracle::new();
    for client in &w.clients {
        for op in &client.ops {
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

/// Smoke scenario: hand-tuned single-disk workload that exercises the
/// write -> read round-trip across the channel under modest delay with
/// no faults. The admission filter rejects the first write, so each key
/// is written twice before the read can hit.
#[test]
fn smoke_round_trip() {
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
        num_disks: 1,
        probe_shutdown: false,
    };
    let report: RunReport = run_workload(0xC0FFEE, w).expect("smoke run");
    let hits: usize = report
        .outcomes
        .iter()
        .filter(|o| matches!(o, Outcome::ReadHit { .. }))
        .count();
    assert!(
        hits >= 1,
        "expected at least one hit after double-write through the channel: {:?}",
        report.outcomes
    );
}

/// Smoke scenario: multi-disk round-trip with the shutdown probe on.
/// Several keys are double-written then read across four channels; at
/// least two disks must take a write, and the post-shutdown send must
/// fail once the storage cores have torn down.
#[test]
fn smoke_multidisk_and_shutdown() {
    let key_count = 6u8;
    let mut ops = Vec::new();
    for k in 0..key_count {
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
        ops.push(Op::Read {
            key_idx: k,
            off_idx: 0,
        });
    }
    let w = Workload {
        page_size: 4096,
        device_pages: 64,
        max_io_delay: 4,
        io_fault_rate: 0,
        read_corrupt_rate: 0,
        key_count,
        offset_count: 1,
        clients: vec![ClientSpec { ops: ops.clone() }, ClientSpec { ops }],
        num_disks: 4,
        probe_shutdown: true,
    };
    let report = run_workload(0xDEADBEEF, w).expect("smoke run");
    let touched = report
        .device_writes_per_disk
        .iter()
        .filter(|n| **n > 0)
        .count();
    assert!(
        touched >= 2,
        "expected writes to fan across at least two disks: {:?}",
        report.device_writes_per_disk,
    );
    assert_eq!(
        report.post_shutdown_send_errored,
        Some(true),
        "post-shutdown send should have failed after the storage cores exited",
    );
}
