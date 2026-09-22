// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

fn command(version: u64, bytes: u64) -> proto::ControlCommand {
    proto::ControlCommand {
        pod_uid: "pod".into(),
        storage_policy: Some(proto::StoragePolicy {
            identity: vec![7; 32],
            version,
            desired_bytes: bytes,
        }),
        ..Default::default()
    }
}

#[test]
fn coalescing_replay_and_thread_safe_reports() {
    let updates = std::sync::Arc::new(Updates::default());
    updates.receive_storage_policy(&command(1, 1 << 30));
    let first = updates.desired_storage().unwrap();
    assert_eq!(
        updates.storage_policy_status().result,
        Some(StorageResult::Pending)
    );
    let worker = updates.clone();
    let request = first.clone();
    assert!(
        std::thread::spawn(move || worker.report_storage(
            &request,
            StorageResult::Applied,
            1 << 30
        ))
        .join()
        .unwrap()
    );
    updates.receive_storage_policy(&command(1, 1 << 30));
    assert_eq!(
        updates.storage_policy_status().result,
        Some(StorageResult::Applied)
    );
    updates.receive_storage_policy(&command(2, 2 << 30));
    updates.receive_storage_policy(&command(3, 3 << 30));
    let latest = updates.desired_storage().unwrap();
    assert_eq!(latest.version, 3);
    assert_eq!(updates.storage_policy_status().applied_bytes, 1 << 30);
    assert!(!updates.report_storage(&first, StorageResult::Applied, 1 << 30));
    assert!(!updates.report_storage(&latest, StorageResult::Applied, 2 << 30));
    assert!(updates.report_storage(&latest, StorageResult::Failed("disk full".into()), 1 << 30));
    updates.receive_storage_policy(&command(3, 3 << 30));
    assert_eq!(
        updates.storage_policy_status().result,
        Some(StorageResult::Failed("disk full".into()))
    );
    assert!(updates.report_storage(&latest, StorageResult::Applied, 3 << 30));
    assert!(
        updates
            .storage_headers()
            .contains(&("X-Racer-Storage-State", "applied".into()))
    );
    // A new process has no applied acknowledgment even with the same policy.
    let restarted = Updates::default();
    restarted.receive_storage_policy(&command(3, 3 << 30));
    assert_eq!(restarted.storage_policy_status().applied_bytes, 0);
    assert_eq!(
        restarted.storage_policy_status().result,
        Some(StorageResult::Pending)
    );
}

#[test]
fn invalid_and_stale_policy_preserve_last_good() {
    let updates = Updates::default();
    updates.receive_storage_policy(&command(4, 1 << 30));
    let good = updates.desired_storage().unwrap();
    for bad in [
        command(0, 1 << 30),
        command(3, 1 << 30),
        command(4, 2 << 30),
        command(5, 1),
        command(5, MAX_BYTES + ALIGNMENT),
        command(5, MIN_BYTES + 1),
    ] {
        updates.receive_storage_policy(&bad);
        assert_eq!(updates.desired_storage(), Some(good.clone()));
        assert!(updates.storage_policy_status().validation_error.is_some());
    }
    for identity in [vec![], vec![8; 32]] {
        let mut bad = command(5, 2 << 30);
        bad.storage_policy.as_mut().unwrap().identity = identity;
        updates.receive_storage_policy(&bad);
        assert_eq!(updates.desired_storage(), Some(good.clone()));
    }
    let mut bad = command(5, 2 << 30);
    bad.pod_uid.clear();
    updates.receive_storage_policy(&bad);
    assert_eq!(updates.desired_storage(), Some(good));
    assert!(updates.latest(0).is_none());
    assert_eq!(updates.applied_epoch(), 0);
    updates.receive_storage_policy(&command(5, 2 << 30));
    assert!(updates.storage_policy_status().validation_error.is_none());
}
