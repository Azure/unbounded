// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::collections::HashMap;
use std::sync::Arc;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use bytes::Bytes;
use tower::ServiceExt;

use crate::catalog::{Catalog, YamlCatalog};
use crate::object::memory_source::MemoryObjectSource;
use crate::server::router::build_router;
use unbounded_storage::bufferpool::StripeKey;

fn test_catalog_yaml() -> &'static str {
    r#"
objects:
  - bucket: test
    key: hello.bin
    stripe: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    size: 5
    content_type: text/plain
    last_modified: "2026-01-15T12:00:00Z"
  - bucket: test
    key: empty.bin
    stripe: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    size: 0
"#
}

#[tokio::test]
async fn get_full_object() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let meta = catalog.lookup("test", "hello.bin").unwrap();
    assert_eq!(meta.size, 5);

    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/hello.bin")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(
        resp.headers().get("content-type").unwrap(),
        "text/plain",
    );
    assert_eq!(resp.headers().get("content-length").unwrap(), "5");

    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert_eq!(&body[..], b"hello");
}

#[tokio::test]
async fn get_range() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/hello.bin")
                .header("range", "bytes=1-3")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::PARTIAL_CONTENT);
    let content_range = resp
        .headers()
        .get("content-range")
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned();
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert_eq!(&body[..], b"ell");
    assert_eq!(content_range, "bytes 1-3/5");
}

#[tokio::test]
async fn head_returns_metadata() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::head("/test/hello.bin")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(resp.headers().get("content-length").unwrap(), "5");
    // No body for HEAD
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert!(body.is_empty());
}

#[tokio::test]
async fn head_with_satisfiable_range_returns_206() {
    // S3 HEAD honors `Range`: it must mirror GET's status,
    // Content-Length, and Content-Range without sending a body.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::head("/test/hello.bin")
                .header("range", "bytes=1-3")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::PARTIAL_CONTENT);
    assert_eq!(resp.headers().get("content-length").unwrap(), "3");
    assert_eq!(
        resp.headers().get("content-range").unwrap(),
        "bytes 1-3/5",
    );
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert!(body.is_empty(), "HEAD body must be empty");
}

#[tokio::test]
async fn head_with_unsatisfiable_range_returns_416() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::head("/test/hello.bin")
                .header("range", "bytes=100-")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::RANGE_NOT_SATISFIABLE);
    assert_eq!(
        resp.headers().get("content-range").unwrap(),
        "bytes */5",
    );
}

#[tokio::test]
async fn nonexistent_key_returns_404() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let src = MemoryObjectSource::new(HashMap::new());
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/nope.bin")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn storage_error_on_first_chunk_returns_500() {
    // Catalog advertises `hello.bin` at stripe `aa..` with size 5, but
    // the memory source has no buffer for that stripe. The source
    // therefore yields `Err(...)` on first poll. The GET handler must
    // peek that error before committing the status and return a
    // proper S3 `InternalError` body, not a `200 OK` with a stalled
    // body stream.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let src = MemoryObjectSource::new(HashMap::new());
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/hello.bin")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::INTERNAL_SERVER_ERROR);
    assert_eq!(
        resp.headers().get("content-type").unwrap(),
        "application/xml",
    );
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    let s = std::str::from_utf8(&body).unwrap();
    assert!(s.contains("<Code>InternalError</Code>"), "body: {s}");
    // Internal error text must not leak through to the response body:
    // the underlying message contains the stripe hex and backend
    // details we keep server-side.
    assert!(
        !s.contains("stripe aa"),
        "internal stripe hex leaked into error body: {s}",
    );
    assert!(
        !s.contains("not found in memory source"),
        "internal source-specific text leaked into error body: {s}",
    );
    assert!(
        s.contains("internal error"),
        "expected generic message in body: {s}",
    );
}

#[tokio::test]
async fn get_with_full_coverage_range_returns_206() {
    // RFC 9110 §15.3.7: a request carrying a satisfiable `Range`
    // gets 206 Partial Content, even when the requested range
    // happens to cover the whole representation.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/hello.bin")
                .header("range", "bytes=0-4")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::PARTIAL_CONTENT);
    assert_eq!(resp.headers().get("content-length").unwrap(), "5");
    assert_eq!(
        resp.headers().get("content-range").unwrap(),
        "bytes 0-4/5",
    );
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert_eq!(&body[..], b"hello");
}

#[tokio::test]
async fn get_with_clipped_range_returns_206() {
    // A request that asks for more bytes than the object has is
    // clipped to EOF and still returns 206 (per RFC 9110) because
    // the client explicitly sent `Range`.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/hello.bin")
                .header("range", "bytes=0-999")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::PARTIAL_CONTENT);
    assert_eq!(resp.headers().get("content-length").unwrap(), "5");
    assert_eq!(
        resp.headers().get("content-range").unwrap(),
        "bytes 0-4/5",
    );
}

#[tokio::test]
async fn head_with_clipped_range_returns_206() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::head("/test/hello.bin")
                .header("range", "bytes=0-999")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::PARTIAL_CONTENT);
    assert_eq!(resp.headers().get("content-length").unwrap(), "5");
    assert_eq!(
        resp.headers().get("content-range").unwrap(),
        "bytes 0-4/5",
    );
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert!(body.is_empty(), "HEAD body must be empty");
}

#[tokio::test]
async fn get_with_malformed_range_is_ignored() {
    // RFC 9110 §14.1.1: recipients MUST ignore a malformed `Range`
    // and respond as though it were absent. Multi-range
    // (`bytes=0-1,5-6`) is parsed as Invalid in our parser and must
    // therefore drop through to the no-Range path: 200 OK, full
    // body, no `Content-Range`.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    for bad in ["bytes=0-1,5-6", "range=0-", "bytes=-"] {
        let resp = app
            .clone()
            .oneshot(
                Request::get("/test/hello.bin")
                    .header("range", bad)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK, "range {bad}");
        assert_eq!(resp.headers().get("content-length").unwrap(), "5");
        assert!(
            resp.headers().get("content-range").is_none(),
            "ignored malformed range {bad} should not produce Content-Range",
        );
        let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
        assert_eq!(&body[..], b"hello", "range {bad}");
    }
}

#[tokio::test]
async fn head_with_malformed_range_is_ignored() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::head("/test/hello.bin")
                .header("range", "bytes=0-1,5-6")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(resp.headers().get("content-length").unwrap(), "5");
    assert!(resp.headers().get("content-range").is_none());
}

#[tokio::test]
async fn range_past_end_is_416() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/hello.bin")
                .header("range", "bytes=100-")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::RANGE_NOT_SATISFIABLE);
}

#[tokio::test]
async fn empty_object_returns_200() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_b = StripeKey([0xbb; 32]);
    let src = MemoryObjectSource::single(stripe_b, Bytes::new());
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/empty.bin")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(resp.headers().get("content-length").unwrap(), "0");
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert!(body.is_empty());
}

#[tokio::test]
async fn empty_object_with_explicit_range_is_416() {
    // A `Range:` header against a zero-byte object must produce 416,
    // not a successful 200 - the response would otherwise contradict
    // the requested range.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_b = StripeKey([0xbb; 32]);
    let src = MemoryObjectSource::single(stripe_b, Bytes::new());
    let app = build_router(catalog, Arc::new(src));

    for range in ["bytes=0-", "bytes=0-0", "bytes=-1"] {
        let resp = app
            .clone()
            .oneshot(
                Request::get("/test/empty.bin")
                    .header("range", range)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(
            resp.status(),
            StatusCode::RANGE_NOT_SATISFIABLE,
            "range {range} should be unsatisfiable on an empty object",
        );
        assert_eq!(
            resp.headers().get("content-range").unwrap(),
            "bytes */0",
        );
    }
}

#[tokio::test]
async fn empty_object_head_returns_200() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_b = StripeKey([0xbb; 32]);
    let src = MemoryObjectSource::single(stripe_b, Bytes::new());
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::head("/test/empty.bin")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(resp.headers().get("content-length").unwrap(), "0");
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert!(body.is_empty());
}

#[tokio::test]
async fn method_not_allowed_for_post() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let src = MemoryObjectSource::new(HashMap::new());
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::post("/test/hello.bin")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::METHOD_NOT_ALLOWED);
}

#[tokio::test]
async fn bucket_location_returns_xml() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let src = MemoryObjectSource::new(HashMap::new());
    let app = build_router(catalog, Arc::new(src));

    for path in ["/test/?location", "/test?location"] {
        let resp = app
            .clone()
            .oneshot(Request::get(path).body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK, "path {path}");
        assert_eq!(
            resp.headers().get("content-type").unwrap(),
            "application/xml",
        );
        let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
        let s = std::str::from_utf8(&body).unwrap();
        assert!(s.contains("<LocationConstraint"), "body: {s}");
    }
}

#[tokio::test]
async fn bucket_location_permissive_for_unknown_bucket() {
    // ?location is answerable even when the bucket isn't in the
    // catalog, matching AWS behavior.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let src = MemoryObjectSource::new(HashMap::new());
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/no-such-bucket/?location")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
}

#[tokio::test]
async fn bucket_root_without_location_is_405() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let src = MemoryObjectSource::new(HashMap::new());
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(Request::get("/test/").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::METHOD_NOT_ALLOWED);
}

#[tokio::test]
async fn bucket_location_head_returns_200() {
    // S3 clients that HEAD-probe a bucket (e.g. to verify
    // existence/region without downloading the body) must get the
    // same status and content-type as GET; hyper strips the body.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let src = MemoryObjectSource::new(HashMap::new());
    let app = build_router(catalog, Arc::new(src));

    for path in ["/test/?location", "/test?location"] {
        let resp = app
            .clone()
            .oneshot(Request::head(path).body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK, "path {path}");
        assert_eq!(
            resp.headers().get("content-type").unwrap(),
            "application/xml",
        );
        let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
        assert!(body.is_empty(), "HEAD body should be empty");
    }
}

#[tokio::test]
async fn range_past_end_returns_416_with_etag_and_last_modified() {
    // RFC 9110 §15.5.17: 416 responses SHOULD identify the current
    // representation via ETag / Last-Modified so If-Range clients
    // can retry. AWS S3 includes both; we should too.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/hello.bin")
                .header("range", "bytes=100-")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::RANGE_NOT_SATISFIABLE);
    assert_eq!(
        resp.headers().get("content-range").unwrap(),
        "bytes */5",
    );
    assert_eq!(
        resp.headers().get("content-type").unwrap(),
        "application/xml",
    );
    // Pin exact values, not just presence. The test catalog yields a
    // deterministic ETag from the first 16 hex chars of the stripe
    // (`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`)
    // and converts the RFC 3339 `last_modified` to IMF-fixdate at
    // load time (`2026-01-15T12:00:00Z` → `Thu, 15 Jan 2026 12:00:00 GMT`).
    // Asserting values catches a regression where `common_headers`
    // falls through to the `"\"0000000000000000\""` placeholder via
    // `unwrap_or_else`.
    assert_eq!(
        resp.headers().get("etag").unwrap(),
        "\"aaaaaaaaaaaaaaaa\"",
    );
    assert_eq!(
        resp.headers().get("last-modified").unwrap(),
        "Thu, 15 Jan 2026 12:00:00 GMT",
    );
    assert_eq!(
        resp.headers().get("accept-ranges").unwrap(),
        "bytes",
        "missing Accept-Ranges",
    );
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    let s = std::str::from_utf8(&body).unwrap();
    assert!(s.contains("<Code>InvalidRange</Code>"), "body: {s}");
}

#[tokio::test]
async fn not_found_includes_accept_ranges() {
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let src = MemoryObjectSource::new(HashMap::new());
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/nope.bin")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
    assert_eq!(
        resp.headers().get("accept-ranges").unwrap(),
        "bytes",
    );
}

#[tokio::test]
async fn head_for_nonexistent_key_returns_404() {
    // The HEAD-404 path is independent of GET-404 (the round-3 HEAD
    // refactor split the handlers) and was previously uncovered.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let src = MemoryObjectSource::new(HashMap::new());
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::head("/test/nope.bin")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
    // HEAD body must be empty; axum strips it from the not_found XML.
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert!(body.is_empty(), "HEAD body must be empty");
}

#[tokio::test]
async fn get_with_open_ended_full_range_returns_206() {
    // `bytes=0-` exercises the open-ended branch in
    // parse_range_header. Coverage is the same as bytes=0-{size-1}
    // (full object), so the 206-on-Range invariant must hold.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/hello.bin")
                .header("range", "bytes=0-")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::PARTIAL_CONTENT);
    assert_eq!(resp.headers().get("content-length").unwrap(), "5");
    assert_eq!(
        resp.headers().get("content-range").unwrap(),
        "bytes 0-4/5",
    );
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert_eq!(&body[..], b"hello");
}

#[tokio::test]
async fn get_with_suffix_covering_full_object_returns_206() {
    // `bytes=-5` against a 5-byte object exercises the suffix branch
    // in parse_range_header and also covers the whole object, so the
    // 206-on-Range invariant must still hold.
    let catalog = Arc::new(YamlCatalog::from_str(test_catalog_yaml()).unwrap());
    let stripe_a = StripeKey([0xaa; 32]);
    let src = MemoryObjectSource::single(stripe_a, Bytes::from_static(b"hello"));
    let app = build_router(catalog, Arc::new(src));

    let resp = app
        .oneshot(
            Request::get("/test/hello.bin")
                .header("range", "bytes=-5")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::PARTIAL_CONTENT);
    assert_eq!(resp.headers().get("content-length").unwrap(), "5");
    assert_eq!(
        resp.headers().get("content-range").unwrap(),
        "bytes 0-4/5",
    );
    let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
    assert_eq!(&body[..], b"hello");
}
