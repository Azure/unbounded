// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

fn command(version: u64, bytes: u64) -> proto::DesiredState {
    proto::DesiredState {
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

#[test]
fn storage_status_serialization_is_separate_bounded_and_process_local() {
    let updates = Updates::default();
    updates.observe_storage(1 << 30, 2);
    let mut policy = command(4, 2 << 30);
    policy.incarnation = vec![9; 32];
    updates.receive_storage_policy(&policy);
    let request = updates.desired_storage().unwrap();
    assert!(updates.report_storage(
        &request,
        StorageResult::Failed("disk\nfull λ".repeat(200)),
        1 << 30
    ));
    let json = updates.status();
    let storage = &json["storage"];
    assert_eq!(json["lastError"], serde_json::Value::Null);
    assert_eq!(storage["phase"], "failed");
    assert_eq!(storage["policyVersion"], 4);
    assert_eq!(storage["policyIdentity"], "07".repeat(32));
    assert_eq!(storage["effectiveBytes"], 2u64 << 30);
    assert_eq!(storage["appliedBytes"], 1u64 << 30);
    assert_eq!(storage["shards"], 2);
    assert_eq!(storage["selectedPodUID"], "pod");
    assert_eq!(storage["boot"], "09".repeat(32));
    assert_eq!(storage["controlFresh"], true);
    assert_eq!(storage["error"].as_str().unwrap().chars().count(), 1024);
    let headers = updates.storage_headers();
    let error = &headers
        .iter()
        .find(|(key, _)| *key == "X-Racer-Storage-Error")
        .unwrap()
        .1;
    assert_eq!(error.len(), 2048);
    assert!(error.bytes().all(|b| b.is_ascii_hexdigit()));
    updates.receive_storage_policy(&command(3, 2 << 30));
    let storage = updates.storage_policy_status().json();
    assert_eq!(storage["phase"], "failed");
    assert_eq!(storage["validationError"], "stale storage policy version");
    assert_eq!(storage["appliedBytes"], 1u64 << 30);
    updates.receive_storage_policy(&policy);
    assert!(updates.report_storage(&request, StorageResult::Applied, 2 << 30));
    assert_eq!(updates.storage_policy_status().applied_version, 4);
    // A superseded commit can change actual geometry without acknowledging the
    // current policy; do not attribute different bytes to the previous version.
    updates.observe_storage(3 << 30, 3);
    assert_eq!(updates.storage_policy_status().applied_version, 0);
    updates.storage.lock().unwrap().status.last_received =
        Some(std::time::Instant::now() - std::time::Duration::from_secs(76));
    assert_eq!(
        updates.storage_policy_status().json()["controlFresh"],
        false
    );
    let restarted = Updates::default();
    restarted.observe_storage(2 << 30, 2);
    restarted.receive_storage_policy(&policy);
    let status = restarted.storage_policy_status().json();
    assert_eq!(status["phase"], "pending");
    assert_eq!(status["appliedVersion"], 0);
    assert_eq!(status["appliedBytes"], 2u64 << 30);
}
