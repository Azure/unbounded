// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

    /// Invariant: every `ReadHit` returned over the channel carries
    /// bytes the workload previously wrote for that `(key, offset)`.
    /// This is the round-trip correctness property: a write shipped to
    /// the storage core, committed by the engine, and later read back
    /// across the channel must reproduce the original bytes. Catches
    /// silent corruption, mis-routing, and cross-page bleed in the
    /// `PageChannel` -> `PageService` -> engine handoff.
    #[test]
    fn invariant_hit_bytes_were_written(seed in any::<u64>(), w in workload_strategy()) {
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
        let faults_enabled = w.io_fault_rate > 0 || w.read_corrupt_rate > 0;
        let report = run_workload(seed, w).expect("run completed");
        for (i, o) in report.outcomes.iter().enumerate() {
            match o {
                Outcome::ReadHit { key, offset, bytes } => {
                    prop_assert!(
                        oracle.allows_read(*key, *offset, bytes),
                        "op {} returned a hit with bytes not previously written for ({:?}, {})",
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
    /// wrote must miss. Dual of the byte-correctness check; catches a
    /// false-positive hit reported back through the channel.
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

    /// Invariant: multi-disk routing fans device writes out across
    /// more than one disk. With `num_disks >= 2` and a non-trivial
    /// number of device writes, a healthy `disk_for` hash (which the
    /// clients use to pick a channel) must land writes on at least two
    /// disks. A regression that collapsed the hash to a constant, or a
    /// channel-fan-out bug that funneled every op to one storage core,
    /// would route everything to a single disk.
    ///
    /// Scoped tightly with early returns (not `prop_assume!`) so
    /// single-disk and tiny workloads do not count against proptest's
    /// global reject budget.
    #[test]
    fn invariant_multidisk_routing_diverse(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("run completed");
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
            report.device_writes, report.num_disks_used, report.device_writes_per_disk,
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
