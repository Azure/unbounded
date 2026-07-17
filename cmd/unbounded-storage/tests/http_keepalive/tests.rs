// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! HTTP keep-alive DST property tests.

use proptest::prelude::*;

use crate::http_keepalive::workload::{
    BodySpec, ConnectionSpec, HttpVersion, RequestMethod, RequestSpec, RunReport, StopReason,
    Workload, expected_keep_alive_prefix, request_bytes, run_workload, workload_strategy,
};

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 256,
        ..ProptestConfig::default()
    })]

    #[test]
    fn http_keepalive_invariants(seed in any::<u64>(), w in workload_strategy()) {
        let expected = expected_keep_alive_prefix(&w);
        let max_requests_per_connection = w.max_requests_per_connection;
        let report = run_workload(seed, w).expect("run completed");

        assert_served_prefix(&report, &expected)?;
        assert_response_connections(&report, max_requests_per_connection)?;
    }
}

/// The model serves exactly the prefix permitted by the shared keep-alive
/// policy and stops parsing pipelined bytes after a non-reusable request.
fn assert_served_prefix(report: &RunReport, expected: &[bool]) -> Result<(), TestCaseError> {
    prop_assert_eq!(report.served.len(), expected.len());
    for (idx, (served, keep_alive)) in report.served.iter().zip(expected).enumerate() {
        prop_assert_eq!(served.request_index, idx);
        prop_assert_eq!(served.keep_alive, *keep_alive);
    }
    if expected.last().copied() == Some(false) {
        prop_assert_eq!(report.stopped_with, StopReason::ServerClosed);
    } else {
        prop_assert_eq!(report.stopped_with, StopReason::ClientClosed);
    }
    prop_assert!(report.steps > 0);
    Ok(())
}

/// Every serialized response carries the connection header selected by the
/// same body, version, and request-cap policy that controls reuse.
fn assert_response_connections(
    report: &RunReport,
    max_requests_per_connection: usize,
) -> Result<(), TestCaseError> {
    for served in &report.served {
        let expected = if served.keep_alive {
            "keep-alive"
        } else {
            "close"
        };
        prop_assert_eq!(served.response_connection.as_str(), expected);
        prop_assert_eq!(
            served.keep_alive,
            served.wants_keep_alive
                && served.bodyless
                && served.request_index.saturating_add(1) < max_requests_per_connection
        );
    }
    Ok(())
}

#[test]
fn smoke_pipelined_http11_requests_share_connection() {
    let w = Workload {
        requests: vec![
            RequestSpec {
                method: RequestMethod::Get,
                version: HttpVersion::Http11,
                connection: ConnectionSpec::None,
                body: BodySpec::None,
            },
            RequestSpec {
                method: RequestMethod::Head,
                version: HttpVersion::Http11,
                connection: ConnectionSpec::Close,
                body: BodySpec::None,
            },
        ],
        chunk_sizes: vec![u8::MAX],
        max_send_delay: 0,
        max_requests_per_connection: 1024,
    };
    let report = run_workload(0, w).expect("run completed");
    assert_eq!(report.served.len(), 2);
    assert!(report.served[0].keep_alive);
    assert!(!report.served[1].keep_alive);
    assert_eq!(report.stopped_with, StopReason::ServerClosed);
    assert!(report.leftover.is_empty());
}

#[test]
fn smoke_body_header_closes_before_pipelined_request() {
    let w = Workload {
        requests: vec![
            RequestSpec {
                method: RequestMethod::Get,
                version: HttpVersion::Http11,
                connection: ConnectionSpec::KeepAlive,
                body: BodySpec::ContentLengthNonZero,
            },
            RequestSpec {
                method: RequestMethod::Get,
                version: HttpVersion::Http11,
                connection: ConnectionSpec::None,
                body: BodySpec::None,
            },
        ],
        chunk_sizes: vec![u8::MAX],
        max_send_delay: 0,
        max_requests_per_connection: 1024,
    };
    let all_bytes = request_bytes(&w.requests);
    let first_len = w.requests[0].to_bytes().len();
    let report = run_workload(0, w).expect("run completed");
    assert_eq!(report.served.len(), 1);
    assert!(!report.served[0].keep_alive);
    assert_eq!(report.stopped_with, StopReason::ServerClosed);
    assert_eq!(report.leftover, all_bytes[first_len - 1..].to_vec());
}

#[test]
fn smoke_request_cap_closes_after_bound() {
    let w = Workload {
        requests: vec![
            RequestSpec {
                method: RequestMethod::Get,
                version: HttpVersion::Http11,
                connection: ConnectionSpec::None,
                body: BodySpec::None,
            },
            RequestSpec {
                method: RequestMethod::Head,
                version: HttpVersion::Http11,
                connection: ConnectionSpec::None,
                body: BodySpec::None,
            },
            RequestSpec {
                method: RequestMethod::Get,
                version: HttpVersion::Http11,
                connection: ConnectionSpec::None,
                body: BodySpec::None,
            },
        ],
        chunk_sizes: vec![u8::MAX],
        max_send_delay: 0,
        max_requests_per_connection: 2,
    };
    let all_bytes = request_bytes(&w.requests);
    let first_two_len = w.requests[0].to_bytes().len() + w.requests[1].to_bytes().len();
    let report = run_workload(0, w).expect("run completed");
    assert_eq!(report.served.len(), 2);
    assert!(report.served[0].keep_alive);
    assert!(!report.served[1].keep_alive);
    assert_eq!(report.served[1].response_connection, "close");
    assert_eq!(report.stopped_with, StopReason::ServerClosed);
    assert_eq!(report.leftover, all_bytes[first_two_len..].to_vec());
}

#[test]
fn regression_delayed_one_byte_chunks_do_not_starve_server() {
    let w = Workload {
        requests: vec![
            RequestSpec {
                method: RequestMethod::Get,
                version: HttpVersion::Http10,
                connection: ConnectionSpec::KeepAlive,
                body: BodySpec::ContentLengthZero,
            },
            RequestSpec {
                method: RequestMethod::Get,
                version: HttpVersion::Http10,
                connection: ConnectionSpec::KeepAlive,
                body: BodySpec::DuplicateContentLength,
            },
        ],
        chunk_sizes: vec![1],
        max_send_delay: 4,
        max_requests_per_connection: 2,
    };
    let report = run_workload(1_895_560_329_144_191_809, w).expect("run completed");
    assert_eq!(report.served.len(), 2);
    assert!(report.served[0].keep_alive);
    assert!(!report.served[1].keep_alive);
    assert_eq!(report.stopped_with, StopReason::ServerClosed);
    assert!(report.leftover.is_empty());
}
