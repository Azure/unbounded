// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[test]
fn typed_failures_survive_fanout_and_nested_io_boundaries_without_reclassification() {
    use crate::outcome::{Cause, Failure, Phase, Transport};
    use std::sync::atomic::{AtomicUsize, Ordering};
    #[derive(Debug)]
    struct Foreign {
        reads: Arc<AtomicUsize>,
        source: cache::Error,
    }
    impl std::fmt::Display for Foreign {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            f.write_str("foreign adapter")
        }
    }
    impl std::error::Error for Foreign {
        fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
            self.reads.fetch_add(1, Ordering::Relaxed);
            Some(&self.source)
        }
    }
    let route = route(true);
    let direct = Failure {
        endpoint: route.endpoint.into(),
        transport: Transport::Http,
        phase: Phase::Headers,
        cause: Cause::ServiceTimeout,
        initiated: true,
        kind: io::ErrorKind::TimedOut,
        message: "service deadline".into(),
    };
    let typed: cache::Error = AttemptFailure {
        route: route.clone(),
        evidence: Some(direct),
        reported: false,
    }
    .into();
    assert!(matches!(typed, cache::Error::Outcome(_)));
    assert!(typed.attempt_failure().unwrap().owner_evidence());
    let shared = cache::Error::Shared(Arc::new(typed));
    let reads = Arc::new(AtomicUsize::new(0));
    let error: cache::Error = io::Error::new(
        io::ErrorKind::WouldBlock,
        io::Error::new(
            io::ErrorKind::TimedOut,
            Foreign {
                reads: reads.clone(),
                source: shared,
            },
        ),
    )
    .into();
    let captured_reads = reads.load(Ordering::Relaxed);
    assert_eq!(captured_reads, 1, "inspect the foreign chain once on entry");
    for _ in 0..4 {
        assert_eq!(failure_reason(&error), PeerReason::Deadline);
        assert_eq!(error_status(&error), 504);
        assert!(error.attempt_failure().unwrap().owner_evidence());
        assert!(!error.evidence().neutral_for_health());
        assert_eq!(
            peer_failure(&error, route.cursor.identity, 3)
                .evidence
                .unwrap()
                .phase,
            Phase::Headers
        );
        metric_failure(&error);
    }
    assert_eq!(reads.load(Ordering::Relaxed), captured_reads);
    let reentered: cache::Error = io::Error::from(error).into();
    assert!(reentered.attempt_failure().unwrap().owner_evidence());
    assert_eq!(failure_reason(&reentered), PeerReason::Deadline);
}

#[test]
fn typed_and_boundary_statuses_keep_origin_health_semantics() {
    for code in [400, 404, 410, 412, 429, 500, 503] {
        for peer in [false, true] {
            let error = cache::http_metadata::status(code);
            let breaker = crate::breaker::CircuitBreaker::new(std::time::Duration::from_secs(1));
            client::Origin::error(breaker.try_acquire().unwrap(), &error, peer);
            assert_eq!(
                breaker.available(),
                !peer && code < 500,
                "{code} peer={peer}"
            );
        }
    }
    for error in [cache::Error::Timeout, cache::busy("admission")] {
        let shared = cache::Error::Shared(Arc::new(error));
        assert!(shared.evidence().neutral_for_health());
        let nested: cache::Error = io::Error::new(io::ErrorKind::ConnectionReset, shared).into();
        assert!(nested.evidence().neutral_for_health());
    }
    // Shared origin statuses historically do not prove a healthy direct exchange.
    let shared = cache::Error::Shared(Arc::new(cache::Error::NotFound));
    assert!(!shared.healthy_http_status());
    assert_eq!(failure_reason(&shared), PeerReason::NotFound);
}

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
        response: Default::default(),
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
        response: Default::default(),
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
    assert_eq!(&wire[54..61], &[1, 2, 2, 6, 2, 1, 0]);
    assert!(wire[61..].iter().all(|b| *b == 0));
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
