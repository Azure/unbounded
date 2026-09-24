// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
pub(crate) mod failure_tests {
    use super::failure::*;
    use crate::outcome::{
        AttemptFailure, AttemptRoute, PeerFailure, PeerReason, attempt_evidence, failure_reason,
        owner_failure, peer_failure,
    };
    use crate::{cache, http_client as client};
    use cache::{http_metadata::headers, peer_wire::hex};
    use std::{io, sync::Arc};

    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/security/outcome.rs"
    ));

    #[test]
    fn http_metric_classification_preserves_shared_and_remote_evidence() {
        use crate::metrics::{HttpErrorReason as R, HttpPressure as P};
        use crate::outcome::{Cause, PeerEvidence, Phase, Transport};
        for (reason, expected) in [
            (PeerReason::OwnerUnavailable, R::OwnerUnavailable),
            (PeerReason::Busy, R::Busy),
            (PeerReason::Unavailable, R::Unavailable),
            (PeerReason::Protocol, R::Protocol),
            (PeerReason::Service, R::Service),
            (PeerReason::Deadline, R::Deadline),
            (PeerReason::Cancelled, R::Cancelled),
            (PeerReason::NotFound, R::NotFound),
            (PeerReason::Gone, R::Gone),
            (PeerReason::Precondition, R::Precondition),
        ] {
            let error = cache::Error::Shared(Arc::new(
                io::Error::other(PeerFailure {
                    response: Default::default(),
                    identity: [3; 32],
                    candidate: 7,
                    reason,
                    evidence: None,
                })
                .into(),
            ));
            assert_eq!(metric_failure(&error).reason, expected);
            assert_eq!(metric_failure(&error).pressure, None);
        }
        for (cause, pressure) in [
            (Cause::LocalPressure, Some(P::LocalPressure)),
            (Cause::BreakerRejected, Some(P::BreakerRejected)),
            (Cause::ServiceTimeout, None),
        ] {
            let evidence = PeerEvidence {
                endpoint: "127.0.0.1:9".parse().unwrap(),
                transport: Transport::Http,
                phase: Phase::LocalAdmission,
                cause,
                initiated: false,
            };
            let error = io::Error::other(PeerFailure {
                response: Default::default(),
                identity: [0; 32],
                candidate: 0,
                reason: PeerReason::Busy,
                evidence: Some(evidence),
            })
            .into();
            assert_eq!(metric_failure(&error).pressure, pressure);
        }
        let admission =
            cache::Error::Shared(Arc::new(cache::busy("arbitrary diagnostic, never a label")));
        assert_eq!(metric_failure(&admission).reason, R::Busy);
        assert_eq!(metric_failure(&admission).pressure, Some(P::Admission));
        assert_eq!(
            metric_failure(&io::Error::from(io::ErrorKind::WouldBlock).into()).pressure,
            Some(P::WouldBlock)
        );
        assert_eq!(
            metric_failure(&cache::Error::Unavailable).reason,
            R::Unavailable
        );
    }

    // Independent expected outcomes shared by framing and owner-probe corpora.
    pub(crate) const SEMANTICS: [(PeerReason, u16, bool); 13] = [
        (PeerReason::OwnerUnavailable, 503, false),
        (PeerReason::Busy, 503, false),
        (PeerReason::Unavailable, 503, false),
        (PeerReason::Protocol, 502, false),
        (PeerReason::Service, 502, false),
        (PeerReason::Deadline, 504, false),
        (PeerReason::Cancelled, 502, false),
        (PeerReason::NotFound, 404, true),
        (PeerReason::Gone, 410, true),
        (PeerReason::Precondition, 412, true),
        (PeerReason::Unauthorized, 401, true),
        (PeerReason::Forbidden, 403, true),
        (PeerReason::MetadataChanged, 412, true),
    ];
    pub(crate) fn route(final_hop: bool) -> AttemptRoute {
        AttemptRoute {
            cursor: crate::routing::Cursor::decode(&[0; crate::routing::Cursor::LEN]).unwrap(),
            candidate: 3,
            endpoint: "127.0.0.1:1".parse().unwrap(),
            final_hop,
            context: "a".repeat(96),
        }
    }

    #[test]
    fn local_pressure_classification_survives_adapter_and_fanout_wrappers() {
        use crate::outcome::{Cause, Failure, Phase, Transport};
        let route = route(true);
        for (cause, kind, reason, status) in [
            (
                Cause::LocalPressure,
                io::ErrorKind::TimedOut,
                PeerReason::Busy,
                503,
            ),
            (
                Cause::BreakerRejected,
                io::ErrorKind::WouldBlock,
                PeerReason::Busy,
                503,
            ),
            (
                Cause::CallerDeadline,
                io::ErrorKind::TimedOut,
                PeerReason::Deadline,
                504,
            ),
            (
                Cause::ServiceTimeout,
                io::ErrorKind::TimedOut,
                PeerReason::Deadline,
                504,
            ),
            (
                Cause::Cancelled,
                io::ErrorKind::TimedOut,
                PeerReason::Cancelled,
                502,
            ),
            (
                Cause::Connection,
                io::ErrorKind::ConnectionRefused,
                PeerReason::Service,
                502,
            ),
            (
                Cause::Other,
                io::ErrorKind::WouldBlock,
                PeerReason::Service,
                502,
            ),
            (
                Cause::Protocol,
                io::ErrorKind::TimedOut,
                PeerReason::Protocol,
                502,
            ),
        ] {
            for routed in [false, true] {
                for wrapper in 0..4 {
                    let evidence = Failure {
                        endpoint: route.endpoint.into(),
                        transport: Transport::Http,
                        phase: Phase::LocalAdmission,
                        cause,
                        initiated: false,
                        kind,
                        message: "classification regression".into(),
                    };
                    let expected = crate::outcome::PeerEvidence::from_failure(&evidence).unwrap();
                    let error = if routed {
                        io::Error::other(AttemptFailure {
                            route: route.clone(),
                            evidence: Some(evidence),
                            reported: false,
                        })
                    } else {
                        io::Error::new(kind, evidence)
                    };
                    let error = cache::Error::from(error);
                    let error = match wrapper {
                        0 => error,
                        1 => cache::Error::Shared(Arc::new(error)),
                        2 => cache::Error::Shared(Arc::new(error)).into_io().into(),
                        _ => io::Error::new(
                            io::ErrorKind::TimedOut,
                            cache::Error::Shared(Arc::new(error)).into_io(),
                        )
                        .into(),
                    };
                    assert_eq!(error_status(&error), status, "{error:?}");
                    let report = peer_failure(&error, route.cursor.identity, 3);
                    assert_eq!(report.reason, reason, "{error:?}");
                    assert_eq!(report.evidence, Some(expected));
                    assert_eq!(owner_failure(&error), None);
                    assert!(!error.attempt_failure().is_some_and(|f| f.owner_evidence()));
                    if matches!(
                        cause,
                        Cause::LocalPressure
                            | Cause::BreakerRejected
                            | Cause::CallerDeadline
                            | Cause::Cancelled
                    ) {
                        let breaker =
                            crate::breaker::CircuitBreaker::new(std::time::Duration::from_secs(1));
                        client::Origin::error(breaker.try_acquire().unwrap(), &error, false);
                        assert!(breaker.available());
                    }
                }
            }
        }
        let busy = cache::Error::from(io::Error::from(io::ErrorKind::WouldBlock));
        assert_eq!(error_status(&busy), 503);
        assert_eq!(peer_failure(&busy, [0; 32], 3).reason, PeerReason::Busy);
        assert!(attempt_evidence(&busy).is_none());
        assert_eq!(owner_failure(&busy), None);
        for error in [
            cache::Error::Timeout,
            io::Error::from(io::ErrorKind::TimedOut).into(),
        ] {
            assert_eq!(error_status(&error), 504);
            assert_eq!(
                peer_failure(&error, [0; 32], 3).reason,
                PeerReason::Deadline
            );
        }
    }
    #[test]
    fn typed_reports_require_exact_bounded_request_context() {
        let route = route(false);
        let valid = format!(
            "X-Racer-Owner-Unavailable: 3\r\nX-Racer-Attempt: {}\r\n",
            route.context
        );
        assert!(headers(&valid, |h| validate_peer_report(h, Some(0), 503, &route)).is_err());
        let error = headers(&valid, |h| cache::http_metadata::response_status(503, h)).unwrap();
        assert_eq!(owner_failure(&error), None);
        assert!(!error.attempt_failure().is_some_and(|f| f.owner_evidence()));
        for wire in [
            valid.replace("Unavailable: 3", "Unavailable: 4"),
            valid.replace(&route.context, &"b".repeat(96)),
            valid.replace("Unavailable: 3", "Unavailable: nope"),
            format!("{valid}X-Racer-Owner-Unavailable: 3\r\n"),
            format!("{valid}X-Racer-Attempt: {}\r\n", route.context),
            "X-Racer-Owner-Unavailable: 3\r\n".into(),
            format!("{valid}Content-Encoding: gzip\r\n"),
        ] {
            assert!(headers(&wire, |h| validate_peer_report(h, Some(0), 503, &route)).is_err());
        }
        for length in [None, Some(1), Some(u64::MAX)] {
            assert!(headers(&valid, |h| validate_peer_report(h, length, 503, &route)).is_err());
        }
        for (reason, status, _) in SEMANTICS {
            let failure = PeerFailure {
                response: Default::default(),
                identity: route.cursor.identity,
                candidate: 3,
                reason,
                evidence: None,
            };
            assert_eq!(error_status(&io::Error::other(failure).into()), status);
            let encoded = hex(&failure.encode());
            let wire = format!(
                "X-Racer-Failure: {encoded}\r\nX-Racer-Attempt: {}\r\n",
                route.context
            );
            assert_eq!(
                headers(&wire, |h| validate_peer_report(h, Some(0), status, &route)).unwrap(),
                failure
            );
            for malformed in [
                format!("{wire}X-Racer-Failure: {encoded}\r\n"),
                format!("{wire}Content-Encoding: gzip\r\n"),
                wire.replace(&route.context, &"b".repeat(96)),
                wire.replace(&encoded, "00"),
            ] {
                assert!(
                    headers(&malformed, |h| validate_peer_report(
                        h,
                        Some(0),
                        status,
                        &route
                    ))
                    .is_err()
                );
            }
            assert!(headers(&wire, |h| validate_peer_report(h, Some(0), 200, &route)).is_err());
            assert!(headers(&wire, |h| validate_peer_report(h, Some(1), status, &route)).is_err());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tls::{ExpectedPeer, TlsProgress, TlsSession, tests::Authority};
    use std::{
        net::{TcpListener, TcpStream},
        time::{Duration, Instant},
    };

    fn policy() -> Policy {
        Policy {
            members: Arc::new(
                [[2; 32], [0xab; 32]]
                    .into_iter()
                    .map(|node| (node, ("pod-123".into(), String::new())))
                    .collect(),
            ),
            universe: [8; 32],
            node: [1; 32],
        }
    }

    fn identity(universe: u8, node: u8) -> PeerIdentity {
        PeerIdentity::new(
            &format!("{universe:02x}").repeat(32),
            &format!("{node:02x}").repeat(32),
            "pod-123",
        )
        .unwrap()
    }

    #[test]
    fn membership_requires_current_peer_in_same_universe() {
        let mut policy = policy();
        policy.authorize(&identity(8, 2)).unwrap();
        policy.authorize(&identity(8, 0xab)).unwrap();
        for peer in [identity(9, 2), identity(8, 3), identity(8, 1)] {
            assert_eq!(
                policy.authorize(&peer).unwrap_err().kind(),
                io::ErrorKind::PermissionDenied
            );
        }
        Arc::make_mut(&mut policy.members).insert(policy.node, ("pod-123".into(), String::new()));
        assert!(policy.authorize(&identity(8, 1)).is_err());
        Arc::make_mut(&mut policy.members).remove(&[2; 32]);
        assert!(policy.authorize(&identity(8, 2)).is_err());
        Arc::make_mut(&mut policy.members).clear();
        assert!(policy.authorize(&identity(8, 0xab)).is_err());
    }

    #[test]
    fn membership_rejects_noncanonical_identity_fields() {
        let policy = policy();
        let mut replaced = identity(8, 2);
        replaced.pod_uid = "replaced-pod".into();
        assert_eq!(
            policy.authorize(&replaced).unwrap_err().kind(),
            io::ErrorKind::PermissionDenied
        );
        for malformed in [
            String::new(),
            "ab".repeat(31),
            "ab".repeat(33),
            "AB".repeat(32),
            "ag".repeat(32),
            "\u{00e9}".repeat(32),
            format!("{} ", "a".repeat(63)),
        ] {
            let mut peer = identity(8, 0xab);
            peer.node = malformed.clone();
            assert_eq!(
                policy.authorize(&peer).unwrap_err().kind(),
                io::ErrorKind::PermissionDenied
            );
            peer = identity(8, 0xab);
            peer.universe = malformed;
            assert_eq!(
                policy.authorize(&peer).unwrap_err().kind(),
                io::ErrorKind::PermissionDenied
            );
        }
    }

    #[test]
    fn real_tls_identity_is_rechecked_after_membership_removal() {
        let ca = Authority::new();
        let local = identity(8, 1);
        let remote = identity(8, 2);
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let client_socket = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (server_socket, _) = listener.accept().unwrap();
        client_socket.set_nonblocking(true).unwrap();
        server_socket.set_nonblocking(true).unwrap();
        let mut client = TlsSession::client(
            &ca.context(&remote, false),
            client_socket.into(),
            ExpectedPeer::Identity(local.clone()),
        )
        .unwrap();
        let mut server = TlsSession::server(
            &ca.context(&local, false),
            server_socket.into(),
            ExpectedPeer::Universe(local.universe.clone()),
        )
        .unwrap();
        assert!(server.peer_identity().is_none());
        let deadline = Instant::now() + Duration::from_secs(3);
        loop {
            assert!(Instant::now() < deadline, "TLS handshake timed out");
            let client_done = matches!(client.handshake().unwrap(), TlsProgress::Complete(()));
            let server_done = matches!(server.handshake().unwrap(), TlsProgress::Complete(()));
            if client_done && server_done {
                break;
            }
            std::thread::sleep(Duration::from_millis(1));
        }
        assert_eq!(client.peer_identity(), Some(&local));
        assert_eq!(server.peer_identity(), Some(&remote));
        let mut policy = policy();
        policy.authorize(server.peer_identity().unwrap()).unwrap();
        Arc::make_mut(&mut policy.members).remove(&[2; 32]);
        assert_eq!(
            policy
                .authorize(server.peer_identity().unwrap())
                .unwrap_err()
                .kind(),
            io::ErrorKind::PermissionDenied
        );
    }
}

mod attribution {
    #[test]
    fn topology_owner_failure_agrees_on_ring_successor_but_relay_failure_does_not() {
        let Some(mut c) = crate::runtime::tests::Cluster::new() else {
            return;
        };
        c.remove(7);
        for client in [0, 2] {
            let target = c.target(7, &format!("failed-owner-client-{client}"));
            assert_eq!(c.get(client, &target), (200, b"abc".to_vec()));
        }
        let hits = c.hits.lock().unwrap().clone();
        assert_eq!(hits.len(), 4);
        let metadata: Vec<_> = hits
            .iter()
            .filter(|(_, request)| request.starts_with("HEAD "))
            .collect();
        assert_eq!(metadata.len(), 2);
        assert!(
            metadata.iter().all(|(node, _)| *node == 0),
            "both metadata requests must choose successor 0"
        );
        c.remove(1);
        let target = c.target(7, "failed-relay");
        assert_eq!(c.get(4, &target).0, 502);
        assert_eq!(
            c.hits.lock().unwrap().len(),
            4,
            "relay failure cannot authorize a backend"
        );
    }
}
