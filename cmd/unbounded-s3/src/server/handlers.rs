// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::sync::Arc;

use axum::extract::{Path, Query, State};
use axum::http::{header, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use futures::stream::{self, StreamExt};
use serde::Deserialize;

use super::range::{parse_range_header, ByteRange, RangeError};
use super::response;
use crate::catalog::{Catalog, ObjectMeta};
use crate::object::ObjectSource;

#[derive(Clone)]
pub(crate) struct AppState {
    pub catalog: Arc<dyn Catalog>,
    pub source: Arc<dyn ObjectSource>,
}

/// Parse the inbound `Range` header against an object's size.
///
/// Returns `Ok((range, satisfied))` where:
/// - `range` is the byte interval to serve. When the header is
///   absent or malformed it is the whole object (`offset = 0,
///   len = meta.size`).
/// - `satisfied` is `true` iff a `Range` header was present *and*
///   parsed successfully. The caller uses this flag to decide
///   between `206` and `200`.
///
/// Returns `Err(Response)` already shaped as a `416` for the
/// unsatisfiable case so the caller can `return` it directly.
///
/// RFC 9110 §14.1.1 says recipients MUST ignore a malformed `Range`
/// and respond as though it were absent. We do that here: malformed
/// syntax (`RangeError::Invalid`) collapses to the no-header path
/// and the response is `200`, not `416`.
fn resolve_range(
    raw_range: Option<&str>,
    meta: &ObjectMeta,
) -> Result<(ByteRange, bool), Response> {
    match parse_range_header(raw_range, meta.size) {
        Ok(r) => Ok((r, raw_range.is_some())),
        Err(RangeError::Unsatisfiable) => Err(response::invalid_range(meta)),
        Err(RangeError::Invalid) => Ok((
            ByteRange { offset: 0, len: meta.size },
            false,
        )),
    }
}

pub(crate) async fn get_object(
    State(state): State<AppState>,
    Path((bucket, key)): Path<(String, String)>,
    headers: axum::http::HeaderMap,
) -> Response {
    // Cheap request log so operators and the smoke test can see the
    // GET reach the daemon without pulling in a full tower-http
    // tracing layer. `?bucket` / `?key` route through `Debug`, which
    // escapes control characters so a key containing e.g. `%0A`
    // can't inject a forged log line. `range` is included so an
    // operator triaging an unexpected 416 has the request's Range
    // header in the log.
    let raw_range = headers.get("range").and_then(|v| v.to_str().ok());
    tracing::info!(
        method = "GET",
        ?bucket,
        ?key,
        range = ?raw_range,
        "object request",
    );

    let meta = match state.catalog.lookup(&bucket, &key) {
        Some(m) => m,
        None => return response::not_found(&bucket, &key),
    };

    let (range, range_satisfied) = match resolve_range(raw_range, &meta) {
        Ok(rr) => rr,
        Err(resp) => return resp,
    };

    // A zero-byte object with no satisfied `Range` header reaches
    // here with `range = {offset: 0, len: 0}` and is served as a
    // regular empty body. Anything with an explicit satisfiable
    // `Range` against a zero-byte object was already rejected by
    // `resolve_range` as Unsatisfiable.
    if meta.size == 0 {
        return handle_empty(meta);
    }

    // Peek the first chunk before committing the response status. If
    // the storage path fails on its first item (e.g. NullBlockStore
    // miss with no P2P fallback) we can still return a proper S3
    // `InternalError` body instead of a `200 OK` with a half-written
    // body that the client has to interpret as a connection drop.
    // Errors after the first chunk are unrecoverable in HTTP/1 and
    // still surface as body-stream errors.
    //
    // Detailed error text is logged server-side; the body returns a
    // generic message so internal stripe IDs and backend state don't
    // leak to anonymous clients.
    let mut stream = state.source.read_range(&meta, range.offset, range.len);
    let first = match stream.next().await {
        None => {
            tracing::error!(
                "storage source returned empty stream for non-empty object",
            );
            return response::internal_error(
                "We encountered an internal error. Please try again.",
            );
        }
        Some(Err(e)) => {
            tracing::error!(error = %e, "storage error before any body bytes; returning 500");
            return response::internal_error(
                "We encountered an internal error. Please try again.",
            );
        }
        Some(Ok(chunk)) => chunk,
    };
    let body = stream::once(async move { Ok(first) }).chain(stream).boxed();

    if range_satisfied {
        response::partial_response(meta, body, range)
    } else {
        response::full_response(meta, body)
    }
}

fn handle_empty(meta: ObjectMeta) -> Response {
    let mut h = response::common_headers(&meta);
    h.insert(header::CONTENT_LENGTH, HeaderValue::from_static("0"));
    (StatusCode::OK, h, axum::body::Body::empty()).into_response()
}

pub(crate) async fn head_object(
    State(state): State<AppState>,
    Path((bucket, key)): Path<(String, String)>,
    headers: axum::http::HeaderMap,
) -> Response {
    // Mirror the GET log line so operators tailing the daemon log
    // can see HEAD requests reach the daemon too. Same `?bucket` /
    // `?key` Debug formatting protects against log injection via
    // control chars in the key. `range` lets operators debug
    // HEAD-with-Range probes (boto3, awscli) that get unexpected
    // 416s.
    let raw_range = headers.get("range").and_then(|v| v.to_str().ok());
    tracing::info!(
        method = "HEAD",
        ?bucket,
        ?key,
        range = ?raw_range,
        "object request",
    );

    let meta = match state.catalog.lookup(&bucket, &key) {
        Some(m) => m,
        None => return response::not_found(&bucket, &key),
    };

    // HEAD honors `Range` the same way GET does. AWS S3 reports the
    // ranged status / Content-Length / Content-Range on HEAD too, and
    // some clients (boto3, awscli) use HEAD with `Range` to probe for
    // valid offsets without downloading any data. The body is always
    // empty on HEAD; we just don't open the storage stream.
    let (range, range_satisfied) = match resolve_range(raw_range, &meta) {
        Ok(rr) => rr,
        Err(resp) => return resp,
    };

    let mut h = response::common_headers(&meta);
    h.insert(
        header::CONTENT_LENGTH,
        HeaderValue::from_str(&range.len.to_string()).expect("valid u64"),
    );

    let status = if range_satisfied {
        // Same invariant as `partial_response`: a satisfied range
        // always has `len >= 1`. See response::partial_response.
        debug_assert!(
            range.len > 0,
            "head_object partial branch reached with empty range {range:?}",
        );
        let content_range = format!(
            "bytes {}-{}/{}",
            range.offset,
            range.offset + range.len.saturating_sub(1),
            meta.size,
        );
        h.insert(
            header::CONTENT_RANGE,
            HeaderValue::from_str(&content_range).expect("valid content-range"),
        );
        StatusCode::PARTIAL_CONTENT
    } else {
        StatusCode::OK
    };

    (status, h).into_response()
}

/// Query parameters accepted on the bucket root (`GET /<bucket>/`).
///
/// `s3cmd` issues `GET /<bucket>/?location` before every operation
/// to discover the bucket's region; without this, every `s3cmd get`
/// burns its retry budget on the location lookup and never sends the
/// actual object GET.
#[derive(Deserialize, Default)]
pub(crate) struct BucketQuery {
    /// Presence is what matters; the value is ignored. `s3cmd` sends
    /// `?location` with no value.
    #[serde(default)]
    location: Option<String>,
}

/// Handles `GET /<bucket>` and `GET /<bucket>/`.
///
/// Today the only recognized sub-resource is `?location`, which
/// returns an empty `LocationConstraint` (i.e. `us-east-1`, which is
/// what `s3cmd` treats as default). Any other request falls through
/// to `405 MethodNotAllowed`; this matches the fallback used for
/// unsupported object-level methods.
pub(crate) async fn bucket_root(
    Path(_bucket): Path<String>,
    Query(q): Query<BucketQuery>,
) -> Response {
    if q.location.is_some() {
        return response::bucket_location();
    }
    response::method_not_allowed()
}
