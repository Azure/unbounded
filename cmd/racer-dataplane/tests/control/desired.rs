// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[test]
fn independent_workers_skip_failed_revisions_and_finish_granted_commit() {
    let (trust, mut config) = fixture();
    let updates = Updates::default();
    for _ in 0..2 {
        updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
    }
    let offer = |revision, updates: &Updates, config: &mut proto::Snapshot| {
        config.revision = revision;
        updates
            .apply_desired(prepare_snapshot(&trust, config.clone()))
            .unwrap();
    };
    offer(1, &updates, &mut config);
    updates.staged(1, 0, true);
    updates.staged(1, 1, true);
    assert_eq!(updates.decision(1), Decision::Activate);
    updates.activated(1, 0);
    // A later desired revision cannot revoke the other worker's commit grant.
    offer(4, &updates, &mut config);
    offer(9, &updates, &mut config);
    assert_eq!(updates.latest(0).unwrap().config.revision, 1);
    updates.activated(1, 1);
    assert_eq!(updates.active().unwrap().config.revision, 1);
    assert_eq!(updates.latest(1).unwrap().config.revision, 9);
    updates.staged(9, 0, true);
    updates.staged(9, 1, false);
    assert_eq!(updates.decision(9), Decision::Waiting);
    assert_eq!(updates.active().unwrap().config.revision, 1);
    // Failed preparation is superseded without waiting for another node or drain.
    offer(15, &updates, &mut config);
    assert_eq!(updates.decision(9), Decision::Discard);
    updates.staged(15, 0, true);
    updates.staged(15, 1, true);
    updates.activated(15, 1);
    updates.activated(15, 0);
    assert_eq!(updates.active().unwrap().config.revision, 15);
    assert_eq!(updates.status()["localState"], "applied");
}

#[test]
fn desired_digest_and_revision_mismatches_preserve_receipt_and_last_good() {
    for revision_mismatch in [false, true] {
        let server = Server::new();
        let (subscriber, updates, trust, mut config) = server.start();
        let (mut socket, _, _) = server.next();
        reply(&mut socket, &signed(&trust, config.clone()), "\"one\"");
        let (mut socket, _, _) = server.next();
        config.revision = 2;
        let mut command = proto::DesiredState::decode(signed(&trust, config).as_slice()).unwrap();
        if revision_mismatch {
            command.revision = 3;
        } else {
            command.snapshot_digest[0] ^= 1;
        }
        reply(&mut socket, &command.encode_to_vec(), "\"bad\"");
        let (_socket, request, _) = server.next();
        assert!(request.contains("X-Racer-Cursor: cursor-2\r\n"));
        assert!(request.contains("X-Racer-Applied-Revision: 1\r\n"));
        assert!(request.contains(&format!(
            "X-Racer-Rejected-Revision: {}\r\n",
            command.revision
        )));
        assert_eq!(updates.active().unwrap().config.revision, 1);
        assert!(
            updates.status()["lastError"]
                .as_str()
                .unwrap()
                .contains(if revision_mismatch {
                    "revision mismatch"
                } else {
                    "digest mismatch"
                })
        );
        drop(subscriber);
    }
}

#[test]
fn desired_cursor_survives_rejection_and_local_ack_interrupts_poll() {
    let server = Server::new();
    let (subscriber, updates, trust, mut config) = server.start();
    let (mut socket, request, _) = server.next();
    assert!(request.starts_with("GET /v1/config HTTP/1.1\r\n"));
    assert!(request.contains("X-Racer-Applied-Revision: 0\r\n"));
    assert!(!request.contains("X-Racer-Phase:"));
    reply(&mut socket, &signed(&trust, config.clone()), "\"one\"");
    let (mut socket, request, _) = server.next();
    assert!(request.contains("X-Racer-Cursor: cursor-1\r\n"));
    assert!(request.contains("X-Racer-Applied-Revision: 1\r\n"));
    config.revision = 4;
    config.volumes[0].max_candidate_attempts = Some(0);
    reply(&mut socket, &signed(&trust, config.clone()), "\"bad\"");
    let (_socket, request, _) = server.next();
    assert!(request.contains("X-Racer-Cursor: cursor-4\r\n"));
    assert!(request.contains("X-Racer-Applied-Revision: 1\r\n"));
    assert!(request.contains("X-Racer-Rejected-Revision: 4\r\n"));
    assert_eq!(updates.active().unwrap().config.revision, 1);
    // Report changes interrupt a normal held request, with no error backoff.
    updates.test_storage_policy(1, 1 << 30);
    let (_socket, request, _) = server.next();
    assert!(request.contains("X-Racer-Storage-State: pending\r\n"));
    let start = Instant::now();
    let storage = updates.desired_storage().unwrap();
    assert!(updates.report_storage(&storage, StorageResult::Applied, 1 << 30));
    let (_socket, request, _) = server.next();
    assert!(start.elapsed() < Duration::from_millis(600));
    assert!(request.contains("X-Racer-Storage-State: applied\r\n"));
    assert!(request.contains("X-Racer-Cursor: cursor-4\r\n"));
    drop(subscriber);
}

#[test]
fn operational_freshness_is_independent_of_storage_application() {
    let updates = Updates::default();
    updates.test_storage_policy(1, 1 << 30);
    let mut status = updates.storage_policy_status();
    status.last_received = Some(Instant::now() - Duration::from_secs(74));
    assert_eq!(status.json()["controlFresh"], true);
    status.last_received = Some(Instant::now() - Duration::from_secs(75));
    assert_eq!(status.json()["controlFresh"], false);
    updates.control_observed();
    assert_eq!(updates.storage_policy_status().json()["controlFresh"], true);
    assert_eq!(updates.storage_policy_status().phase(), "pending");
}

#[test]
fn successful_204_refreshes_observation_without_advancing_received_or_applied_state() {
    let server = Server::new();
    let (subscriber, updates, trust, config) = server.start();
    let (mut socket, _, _) = server.next();
    reply(&mut socket, &signed(&trust, config), "\"one\"");
    let (mut socket, _, _) = server.next();
    let before = updates.storage_policy_status().last_received.unwrap();
    socket
        .write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        .unwrap();
    let (_socket, request, _) = server.next();
    assert!(request.contains("X-Racer-Cursor: cursor-1\r\n"));
    assert!(request.contains("X-Racer-Applied-Revision: 1\r\n"));
    assert_eq!(updates.storage_policy_status().json()["controlFresh"], true);
    assert!(updates.storage_policy_status().last_received.unwrap() > before);
    drop(subscriber);
}

#[test]
fn applied_ack_and_health_change_cancel_normal_long_poll() {
    let server = Server::new();
    let (subscriber, updates, trust, config) = server.start();
    updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
    let life = Arc::new(crate::lifecycle::Lifecycle::new(Default::default()));
    life.configure_workers(1);
    updates.set_lifecycle(life.clone());
    let (mut socket, _, _) = server.next();
    reply(&mut socket, &signed(&trust, config.clone()), "\"one\"");
    // Setting lifecycle may have canceled the initial request before its reply.
    let (mut socket, mut request, _) = server.next();
    if !request.contains("X-Racer-Cursor: cursor-1\r\n") {
        reply(&mut socket, &signed(&trust, config), "\"one\"");
        (socket, request, _) = server.next();
    }
    assert!(request.contains("X-Racer-Applied-Revision: 0\r\n"));
    let start = Instant::now();
    updates.staged(1, 0, true);
    updates.activated(1, 0);
    let (_next, request, _) = server.next();
    assert!(start.elapsed() < Duration::from_millis(600));
    assert!(request.contains("X-Racer-Applied-Revision: 1\r\n"));
    life.progress(0);
    let (_healthy, request, _) = server.next();
    assert!(request.contains("X-Racer-Worker-Healthy: 1\r\n"));
    life.begin_shutdown();
    let (_draining, request, _) = server.next();
    assert!(request.contains("X-Racer-Worker-Healthy: 0\r\n"));
    drop(socket);
    drop(subscriber);
}
