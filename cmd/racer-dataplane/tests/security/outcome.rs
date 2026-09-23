// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[test]
fn typed_evidence_preserves_semantic_precedence_and_candidate_scope() {
    use crate::outcome::{Cause, Evidence, Failure, PeerEvidence, Phase, Transport};
    let direct = Failure {
        endpoint: "127.0.0.1:9"
            .parse::<std::net::SocketAddr>()
            .unwrap()
            .into(),
        transport: Transport::Http,
        phase: Phase::Connect,
        cause: Cause::Connection,
        initiated: true,
        kind: io::ErrorKind::ConnectionRefused,
        message: "connection refused".into(),
    };
    let mut facts = Evidence {
        attempt: Some(&direct),
        fallback: Some(PeerReason::Busy),
        ..Evidence::default()
    };
    assert_eq!(facts.reason(), PeerReason::Service);
    assert!(!facts.neutral_for_health());
    facts.owner = Some(3);
    assert_eq!(facts.reason(), PeerReason::OwnerUnavailable);
    assert_eq!(
        facts.peer_failure([1; 32], 3).reason,
        PeerReason::OwnerUnavailable
    );
    assert_eq!(facts.peer_failure([1; 32], 4).reason, PeerReason::Service);
    let remote = PeerFailure {
        identity: [2; 32],
        candidate: 5,
        reason: PeerReason::Busy,
        evidence: Some(PeerEvidence {
            transport: Transport::Rdma,
            phase: Phase::Grant,
            cause: Cause::LocalPressure,
            ..PeerEvidence::from_failure(&direct).unwrap()
        }),
    };
    facts.semantic = Some(remote);
    assert_eq!(facts.peer_failure([1; 32], 3), remote);
    assert_eq!(facts.reason(), PeerReason::Busy);
    assert_eq!(facts.cause(), Some(Cause::LocalPressure));
    assert!(facts.neutral_for_health());

    let unix = Failure {
        endpoint: crate::socket::Address::unix("/cache").unwrap(),
        ..direct
    };
    assert!(!unix.owner_evidence());
    assert!(PeerEvidence::from_failure(&unix).is_none());
}

#[test]
fn peer_failure_codec_keeps_fixed_wire_layout_and_rejects_reserved_bytes() {
    use crate::outcome::{Cause, PeerEvidence, Phase, Transport};
    let failure = PeerFailure {
        identity: [0xab; 32],
        candidate: 0x01020304,
        reason: PeerReason::Deadline,
        evidence: Some(PeerEvidence {
            endpoint: "127.0.0.1:258".parse().unwrap(),
            transport: Transport::Rdma,
            phase: Phase::Grant,
            cause: Cause::ServiceTimeout,
            initiated: true,
        }),
    };
    let wire = failure.encode();
    assert_eq!(&wire[..32], &[0xab; 32]);
    assert_eq!(&wire[32..42], &[1, 2, 3, 4, 6, 4, 127, 0, 0, 1]);
    assert_eq!(&wire[42..54], &[0; 12]);
    assert_eq!(&wire[54..], &[1, 2, 2, 6, 2, 1, 0]);
    assert_eq!(PeerFailure::decode(&wire).unwrap(), failure);
    for (index, value) in [
        (36, 0),
        (37, 5),
        (42, 1),
        (56, 3),
        (57, 8),
        (58, 9),
        (59, 2),
        (60, 1),
    ] {
        let mut bad = wire;
        bad[index] = value;
        assert!(PeerFailure::decode(&bad).is_err(), "byte {index}");
    }
    for length in 0..PeerFailure::LEN {
        assert!(PeerFailure::decode(&wire[..length]).is_err());
    }
    assert!(PeerFailure::decode(&[0; PeerFailure::LEN + 1]).is_err());
    let bare = PeerFailure {
        evidence: None,
        ..failure
    };
    assert_eq!(PeerFailure::decode(&bare.encode()).unwrap(), bare);
    for index in 38..PeerFailure::LEN {
        let mut bad = bare.encode();
        bad[index] = 1;
        assert!(PeerFailure::decode(&bad).is_err());
    }
    let ipv6 = PeerFailure {
        evidence: Some(PeerEvidence {
            endpoint: "[2001:db8::1]:443".parse().unwrap(),
            ..failure.evidence.unwrap()
        }),
        ..failure
    };
    assert_eq!(PeerFailure::decode(&ipv6.encode()).unwrap(), ipv6);
}
