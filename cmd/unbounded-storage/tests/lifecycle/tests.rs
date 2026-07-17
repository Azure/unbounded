// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

    #[test]
    fn lifecycle_invariants(seed in any::<u64>(), w in workload_strategy()) {
        let expected = expected_apply_counts(&w);
        let has_disk_swap = w.applies.iter().any(|a| matches!(a.kind, ApplyKind::DiskSwap { .. }));
        let report = run_workload(seed, w).expect("lifecycle run completed");

        assert_bounded_termination(&report)?;
        assert_config_applies(&report, expected.broadcasts)?;
        assert_disk_generations(&report, expected.disk_applies)?;
        assert_serving_completion(&report, has_disk_swap)?;
    }
}

/// The combined lifecycle workload terminates under the deterministic
/// scheduler with active shards, config applies, disk swaps, and clients.
fn assert_bounded_termination(report: &RunReport) -> Result<(), TestCaseError> {
    prop_assert!(report.steps > 0);
    Ok(())
}

/// Every broadcast reaches each shard exactly once, and serving starts only
/// after the phase-B peer publication gate opens.
fn assert_config_applies(
    report: &RunReport,
    expected_broadcasts: usize,
) -> Result<(), TestCaseError> {
    prop_assert_eq!(report.phase_a_ready, report.shard_count);
    prop_assert_eq!(report.phase_b_ready, report.shard_count);
    prop_assert_eq!(report.broadcasts, expected_broadcasts);
    for (idx, applied) in report.shard_apply_counts.iter().enumerate() {
        prop_assert_eq!(
            *applied,
            expected_broadcasts,
            "shard {} applied {} broadcast(s), expected {}",
            idx,
            applied,
            expected_broadcasts,
        );
    }
    prop_assert_eq!(
        report.serve_before_phase_b,
        0,
        "serving future ran before phase-B peer publication",
    );
    prop_assert_eq!(
        report.serve_before_initial_disk_publication,
        0,
        "serving future ran before initial disk publication",
    );
    Ok(())
}

/// Disk changes publish one coherent generation each, and clients never
/// observe a generation newer than the final directory.
fn assert_disk_generations(
    report: &RunReport,
    expected_disk_applies: usize,
) -> Result<(), TestCaseError> {
    prop_assert_eq!(report.disk_applies, expected_disk_applies);
    prop_assert_eq!(
        report.directory_generation,
        (1 + expected_disk_applies) as u64,
        "directory generation should be initial publish plus one per disk swap",
    );
    prop_assert!(
        report.max_snapshot_generation <= report.directory_generation,
        "client observed future directory generation {} > final {}",
        report.max_snapshot_generation,
        report.directory_generation,
    );
    Ok(())
}

/// Clients finish through hot reload; without a disk swap, channel failures
/// cannot be explained by a race with a removed disk service.
fn assert_serving_completion(report: &RunReport, has_disk_swap: bool) -> Result<(), TestCaseError> {
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
    Ok(())
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
