// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Lifecycle DST property tests.

use proptest::prelude::*;

use crate::lifecycle::workload::{
    ApplyKind, RunReport, expected_apply_counts, run_workload, workload_strategy,
};

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 128,
        ..ProptestConfig::default()
    })]

    /// Invariant: the combined lifecycle workload terminates under the
    /// deterministic scheduler. This covers active shard loops, config apply
    /// acks, disk-channel swaps, storage-core services, and serving clients.
    #[test]
    fn invariant_bounded_lifecycle_termination(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("lifecycle run completed");
        prop_assert!(report.steps > 0);
    }

    /// Invariant: every config version that requires a shard broadcast is
    /// applied exactly once by every shard, and no shard serves before the
    /// phase-B peer publish gate opens.
    #[test]
    fn invariant_config_applies_reach_every_live_shard(seed in any::<u64>(), w in workload_strategy()) {
        let expected = expected_apply_counts(&w);
        let report = run_workload(seed, w).expect("lifecycle run completed");
        prop_assert_eq!(report.phase_a_ready, report.shard_count);
        prop_assert_eq!(report.phase_b_ready, report.shard_count);
        prop_assert_eq!(report.broadcasts, expected.broadcasts);
        for (idx, applied) in report.shard_apply_counts.iter().enumerate() {
            prop_assert_eq!(
                *applied,
                expected.broadcasts,
                "shard {} applied {} broadcast(s), expected {}",
                idx,
                applied,
                expected.broadcasts,
            );
        }
        prop_assert_eq!(
            report.serve_before_phase_b, 0,
            "serving future ran before phase-B peer publication",
        );
        prop_assert_eq!(
            report.serve_before_initial_disk_publication,
            0,
            "serving future ran before initial disk publication",
        );
    }

    /// Invariant: disk config changes publish coherent directory generations
    /// while clients are using snapshots. Reapplying unchanged config must not
    /// bump the directory; every disk swap must bump it exactly once.
    #[test]
    fn invariant_disk_swaps_publish_expected_generations(seed in any::<u64>(), w in workload_strategy()) {
        let expected = expected_apply_counts(&w);
        let report = run_workload(seed, w).expect("lifecycle run completed");
        prop_assert_eq!(report.disk_applies, expected.disk_applies);
        prop_assert_eq!(
            report.directory_generation,
            (1 + expected.disk_applies) as u64,
            "directory generation should be initial publish plus one per disk swap",
        );
        prop_assert!(
            report.max_snapshot_generation <= report.directory_generation,
            "client observed future directory generation {} > final {}",
            report.max_snapshot_generation,
            report.directory_generation,
        );
    }

    /// Invariant: serving clients complete through hot reload. Without disk
    /// swaps, no channel should fail; with swaps, failures are allowed only when
    /// an in-flight op races a removed disk service.
    #[test]
    fn invariant_serving_completes_across_hot_reload(seed in any::<u64>(), w in workload_strategy()) {
        let has_disk_swap = w.applies.iter().any(|a| matches!(a.kind, ApplyKind::DiskSwap { .. }));
        let report = run_workload(seed, w).expect("lifecycle run completed");
        prop_assert_eq!(report.clients_finished, report.client_count);
        prop_assert_eq!(
            report.completed_ops,
            report.expected_ops,
            "some client operation did not record an outcome",
        );
        if !has_disk_swap {
            prop_assert_eq!(
                report.channel_errors,
                0,
                "channel errors without a disk swap indicate a serving-path regression",
            );
        }
    }
}

#[test]
fn smoke_config_disk_and_shard_lifecycle() {
    let w = crate::lifecycle::workload::Workload {
        shard_count: 2,
        initial_disks: 1,
        device_pages: 64,
        max_io_delay: 2,
        key_count: 2,
        offset_count: 2,
        clients: vec![crate::lifecycle::workload::ClientSpec {
            ops: vec![
                crate::lifecycle::workload::Op::Write {
                    key_idx: 0,
                    off_idx: 0,
                    payload_seed: 1,
                },
                crate::lifecycle::workload::Op::Write {
                    key_idx: 0,
                    off_idx: 0,
                    payload_seed: 1,
                },
                crate::lifecycle::workload::Op::Read {
                    key_idx: 0,
                    off_idx: 0,
                },
            ],
        }],
        applies: vec![
            crate::lifecycle::workload::ApplySpec {
                delay: 1,
                kind: ApplyKind::Peers { count: 2 },
            },
            crate::lifecycle::workload::ApplySpec {
                delay: 1,
                kind: ApplyKind::DiskSwap { count: 2 },
            },
            crate::lifecycle::workload::ApplySpec {
                delay: 1,
                kind: ApplyKind::Frontends { count: 1 },
            },
        ],
    };
    let report: RunReport = run_workload(0x51A7E, w).expect("smoke run");
    assert_eq!(report.phase_b_ready, 2);
    assert_eq!(report.broadcasts, 3);
    assert_eq!(report.disk_applies, 1);
    assert_eq!(report.shard_apply_counts, vec![3, 3]);
    assert_eq!(report.directory_generation, 2);
    assert_eq!(report.clients_finished, 1);
}
