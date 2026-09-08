// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Cross-core `PageChannel` DST property tests.
//!
//! Each invariant drives a randomized workload end to end over real
//! [`PageChannel`]s and asserts a single property of the
//! shard-to-storage-core page path. A couple of hand-built smoke tests
//! pin the round-trip and shutdown scenarios deterministically.
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

    /// Invariant: bounded termination. `run_workload` returning `Ok`
    /// proves the executor neither deadlocked nor exhausted its step
    /// budget while driving the full channel + storage-core path.
    #[test]
    fn invariant_bounded_termination(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w)
            .expect("run completed without deadlock or budget exhaustion");
        prop_assert!(report.steps > 0);
    }

    /// Invariant: counters are self-consistent. `hits + misses` cannot
    /// exceed the number of `read_page` calls the workload issued, and
    /// the per-disk device-write counters must sum to the aggregate.
    #[test]
    fn invariant_counters_consistent(seed in any::<u64>(), w in workload_strategy()) {
        let mut reads = 0u64;
        for c in &w.clients {
            for op in &c.ops {
                if matches!(op, Op::Read { .. }) {
                    reads += 1;
                }
            }
        }
        let report = run_workload(seed, w).expect("run completed");
        prop_assert!(
            report.hits + report.misses <= reads,
            "hits ({}) + misses ({}) > read calls ({})",
            report.hits, report.misses, reads,
        );
        let per_disk_sum: u64 = report.device_writes_per_disk.iter().copied().sum();
        prop_assert_eq!(
            per_disk_sum, report.device_writes,
            "device_writes ({}) disagrees with per-disk sum ({})",
            report.device_writes, per_disk_sum,
        );
    }

    /// Invariant: the service-shutdown path fails sends cleanly. When
    /// the workload requested the probe, every storage-core task has
    /// exited by the time `run_workload` issues one more `write_page`
    /// on a surviving channel clone; that send must resolve with an
    /// error (the receiver is gone / the service marked itself dead)
    /// rather than parking forever on a reply slot.
    #[test]
    fn invariant_shutdown_send_fails(seed in any::<u64>(), w in workload_strategy()) {
        let probe_requested = w.probe_shutdown;
        let report = run_workload(seed, w).expect("run completed");
        match report.post_shutdown_send_errored {
            Some(errored) => prop_assert!(
                errored,
                "post-shutdown send unexpectedly succeeded; the service did not fail it",
            ),
            None => prop_assert!(
                !probe_requested || report.outcomes.iter().any(|o| matches!(o, Outcome::Err(_))),
                "probe requested but no channel existed, yet bootstrap reported no error",
            ),
        }
    }

    /// Invariant: every reported hit returns bytes that were actually
    /// submitted by some workload write for the same `(key, offset)`.
    /// Fault injection may turn operations into `Err`, but it must never
    /// permit a successful hit with corrupted or mis-routed bytes.
    #[test]
    fn invariant_hit_bytes_were_written(seed in any::<u64>(), w in workload_strategy()) {
        let oracle = oracle_for_workload(&w);
        let report = run_workload(seed, w).expect("run completed");
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
    }

    /// Invariant: a key/offset the workload never wrote must not be
    /// reported as a cache hit. Such reads may miss, or error under an
    /// injected fault/shutdown race, but a hit would mean fabricated data.
    #[test]
    fn invariant_unwritten_keys_miss(seed in any::<u64>(), w in workload_strategy()) {
        let oracle = oracle_for_workload(&w);
        let report = run_workload(seed, w).expect("run completed");
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
    }
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
