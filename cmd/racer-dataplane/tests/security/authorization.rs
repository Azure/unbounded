// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0
use super::*;

#[test]
fn exact_limit_opaque_credentials_and_keyed_isolation() {
    let value = "x".repeat(MAX_AUTHORIZATION);
    let auth = Authorization::new(&value).unwrap();
    assert_eq!(auth.as_str(), Some(value.as_str()));
    assert!(Authorization::new(&(value + "x")).is_err());
    for bad in [
        "",
        " leading",
        "trailing ",
        "Bearer\tvalue",
        "a\r\nb",
        "a\0b",
        "a\u{7f}",
        "é",
    ] {
        assert!(Authorization::new(bad).is_err());
    }
    let a = Authorization::new("Bearer credential-a").unwrap();
    let b = Authorization::new("Bearer credential-b").unwrap();
    assert_eq!(a.fingerprint(), a.clone().fingerprint());
    assert_ne!(a.fingerprint(), b.fingerprint());
    assert_ne!(a.fingerprint(), Authorization::default().fingerprint());
    assert_ne!(binding(b"descriptor", &a), binding(b"descriptor", &b));
    assert_ne!(binding(b"descriptor", &a), binding(b"changed", &a));
    let expected = a.fingerprint();
    assert_eq!(
        std::thread::spawn(move || a.fingerprint()).join().unwrap(),
        expected
    );
}

#[test]
fn rdma_exact_envelope_threshold_and_multihop_forwarding() {
    let descriptor = b"RF04\xe8\x03\0\0RF08\0/object";
    let max = crate::rdma::MAX_METADATA - 8 - descriptor.len();
    for n in [0, 1, max - 1, max, max + 1, MAX_AUTHORIZATION] {
        let auth = if n == 0 {
            Authorization::default()
        } else {
            Authorization::new(&"x".repeat(n)).unwrap()
        };
        let envelope = rdma_envelope(descriptor, &auth);
        assert_eq!(envelope.is_some(), n <= max);
        if let Some(mut envelope) = envelope {
            assert_eq!(envelope.len(), 8 + descriptor.len() + n);
            for _ in 0..4 {
                let (wire, forwarded) = rdma_decode(&envelope).unwrap();
                assert_eq!(wire, descriptor);
                assert_eq!(forwarded.as_str(), auth.as_str());
                envelope = rdma_envelope(wire, &forwarded).unwrap();
            }
            envelope.push(0);
            assert!(rdma_decode(&envelope).is_err());
        }
    }
    assert!(rdma_decode(descriptor).is_err());
}

#[test]
fn auth_failures_roundtrip_through_multiple_hops_without_owner_evidence() {
    use crate::{
        cache::http_metadata::{headers, response_status},
        outcome::*,
    };
    for code in [401, 403] {
        let challenge = format!("Bearer realm=\"{}\"", "x".repeat(1009));
        assert_eq!(challenge.len(), 1024);
        let mut error = headers(
            &format!("WWW-Authenticate: {challenge}\r\nRetry-After: 7\r\n"),
            |h| response_status(code, h),
        )
        .unwrap();
        for _ in 0..4 {
            assert_eq!(crate::http_auth::failure::error_status(&error), code);
            assert!(error.evidence().neutral_for_health());
            assert!(error.attempt_failure().is_none());
            let report = peer_failure(&error, [7; 32], 3);
            assert_eq!(
                report.response.challenge.as_bytes(),
                Some(challenge.as_bytes())
            );
            assert_eq!(
                report.response.retry_after.as_bytes(),
                Some(b"7".as_slice())
            );
            assert!(report.reason.establishes_owner_reachability());
            let bytes = report.encode();
            assert!(32 + bytes.len() <= crate::rdma::MAX_METADATA);
            error = PeerFailure::decode(&bytes).unwrap().into();
        }
        assert!(
            headers(&format!("WWW-Authenticate: {challenge}x\r\n"), |h| {
                response_status(code, h)
            })
            .is_err()
        );
        assert!(
            headers("WWW-Authenticate: a\r\nwww-authenticate: b\r\n", |h| {
                response_status(code, h)
            })
            .is_err()
        );
    }
}

#[test]
fn retry_metadata_survives_non_auth_failure_and_http_binding_validation() {
    use crate::{
        cache::http_metadata::{headers, response_status},
        outcome::*,
    };
    let error = headers("Retry-After: 120\r\n", |h| response_status(503, h)).unwrap();
    let report = peer_failure(&error, [1; 32], 2);
    assert_eq!(
        report.response.retry_after.as_bytes(),
        Some(b"120".as_slice())
    );
    let report = PeerFailure::decode(&report.encode()).unwrap();
    let route = AttemptRoute {
        cursor: crate::routing::Cursor {
            algorithm: crate::routing::Algorithm::Canonical,
            identity: [1; 32],
            source: 0,
            owner: 2,
            attempt: 0,
            position: 1,
        },
        candidate: 2,
        endpoint: "127.0.0.1:80".parse().unwrap(),
        final_hop: true,
        context: "a".repeat(96),
    };
    let wire = format!(
        "X-Racer-Failure: {}\r\nX-Racer-Attempt: {}\r\nRetry-After: 120\r\n",
        crate::cache::peer_wire::hex(&report.encode()),
        route.context
    );
    assert_eq!(
        headers(&wire, |h| crate::http_auth::failure::validate_peer_report(
            h,
            Some(0),
            502,
            &route
        ))
        .unwrap(),
        report
    );
    assert!(
        headers(&wire.replace("120", "121"), |h| {
            crate::http_auth::failure::validate_peer_report(h, Some(0), 502, &route)
        })
        .is_err()
    );
}
