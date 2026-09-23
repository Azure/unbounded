// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Uses the production mTLS Subscriber with the owning module's TLS fixture.
#[test]
fn independent_mtls_storage_subscription() {
    fn encode(_trust: &Trust, command: &proto::DesiredState) -> Vec<u8> {
        command.encode_to_vec()
    }
    let server = Server::new();
    let (subscriber, updates, trust, config) = server.start();
    let (mut socket, request, _) = server.next();
    assert!(request.contains("X-Racer-Storage-Policy: 1\r\n"));
    let mut command =
        proto::DesiredState::decode(signed(&trust, config.clone()).as_slice()).unwrap();
    // Invalid storage leaves valid topology fully usable.
    command.storage_policy = Some(proto::StoragePolicy {
        identity: vec![5; 32],
        version: 1,
        desired_bytes: 1,
    });
    reply(&mut socket, &encode(&trust, &command), "\"initial\"");
    let (mut socket, _, _) = server.next();
    assert_eq!(updates.latest(0).unwrap().config.revision, 1);
    assert!(updates.desired_storage().is_none());
    assert!(updates.storage_policy_status().validation_error.is_some());
    // An unchanged topology delivers policy without republishing topology.
    command.storage_policy.as_mut().unwrap().desired_bytes = 1 << 30;
    reply(&mut socket, &encode(&trust, &command), "\"policy\"");
    let (_pending_socket, request, _) = server.next();
    let first = updates.desired_storage().unwrap();
    assert_eq!(first.version, 1);
    assert!(request.contains("X-Racer-Storage-State: pending\r\n"));
    assert!(updates.latest(1).is_none());
    assert!(updates.report_storage(&first, StorageResult::Applied, 1 << 30));
    // The local report cancels the request whose headers still said pending.
    let (mut socket, _, _) = server.next();
    // A stale topology revision cannot discard a newer independently ordered policy.
    command.revision = 0;
    command.storage_policy.as_mut().unwrap().version = 2;
    command.storage_policy.as_mut().unwrap().desired_bytes = 2 << 30;
    reply(&mut socket, &encode(&trust, &command), "\"independent\"");
    let (mut socket, _, _) = server.next();
    assert_eq!(updates.desired_storage().unwrap().version, 2);
    assert_eq!(updates.storage_policy_status().applied_bytes, 1 << 30);
    assert_eq!(updates.latest(0).unwrap().config.revision, 1);
    // Every identity binding and malformed frame is checked before the storage mailbox.
    for field in 0..6 {
        let mut bad = command.clone();
        bad.revision = 1;
        bad.storage_policy.as_mut().unwrap().version = 3;
        match field {
            0 => bad.universe[0] ^= 1,
            1 => bad.node[0] ^= 1,
            2 => bad.incarnation[0] ^= 1,
            3 => bad.pod_uid = "other-pod".into(),
            4 => bad.profile = 2,
            _ => (),
        }
        let mut body = encode(&trust, &bad);
        if field == 5 {
            body = vec![0xff];
        }
        reply(&mut socket, &body, "\"bad\"");
        (socket, _, _) = server.next();
        assert_eq!(updates.desired_storage().unwrap().version, 2);
        // Reset transport backoff with a valid topology heartbeat between cases.
        let mut good = command.clone();
        good.revision = 1;
        reply(&mut socket, &encode(&trust, &good), "\"good\"");
        (socket, _, _) = server.next();
    }
    drop(subscriber);
}
