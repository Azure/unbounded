// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! HTTP connection lifetime policy shared by frontends and backends.

use super::headers::{any_header_value_has_token, header_count, header_value_has_token};
use super::request::HttpRequest;

/// Whether a request's HTTP version and `Connection` headers ask for a
/// persistent connection.
pub fn request_wants_keep_alive(req: &HttpRequest<'_>) -> bool {
    if req.version_minor == 0 {
        any_header_value_has_token(&req.headers, "connection", "keep-alive")
    } else {
        !any_header_value_has_token(&req.headers, "connection", "close")
    }
}

/// Whether a request has no body bytes the frontend would need to drain
/// before parsing the next request on the same connection.
pub fn request_is_bodyless(req: &HttpRequest<'_>) -> bool {
    if header_count(&req.headers, "transfer-encoding") > 0 {
        return false;
    }

    match header_count(&req.headers, "content-length") {
        0 => true,
        1 => {
            req.header("content-length")
                .and_then(|value| value.trim().parse::<u64>().ok())
                == Some(0)
        }
        _ => false,
    }
}

/// Whether the frontend can safely keep a client connection alive after
/// serving this request.
pub fn request_allows_keep_alive(req: &HttpRequest<'_>) -> bool {
    request_wants_keep_alive(req) && request_is_bodyless(req)
}

/// Canonical `Connection` response header value for a keep-alive decision.
pub fn connection_header_value(keep_alive: bool) -> &'static str {
    if keep_alive { "keep-alive" } else { "close" }
}

/// Whether an origin response keeps the TCP connection reusable after
/// its body has been fully consumed.
pub fn response_keep_alive(version_minor: u8, connection: Option<&str>) -> bool {
    if version_minor == 0 {
        connection.is_some_and(|value| header_value_has_token(value, "keep-alive"))
    } else {
        !connection.is_some_and(|value| header_value_has_token(value, "close"))
    }
}

/// Whether EOF is the expected delimiter after the response body.
pub fn response_closes_after_body(version_minor: u8, connection: Option<&str>) -> bool {
    !response_keep_alive(version_minor, connection)
}
