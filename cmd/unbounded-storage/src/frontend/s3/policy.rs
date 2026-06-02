// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! The S3 [`ServePolicy`]: map an S3 GET/HEAD onto a catalog lookup and
//! a single-stripe body plan.
//!
//! Unlike the HTTP policy, S3 resolves object identity and length
//! locally from the pre-loaded [`YamlCatalog`] rather than from a live
//! origin `HEAD`, so [`ServePolicy::respond`] never touches the socket
//! ring: the whole decision is pure and lives in [`S3Policy::plan`].
//! In v0 each object is exactly one stripe, so the body plan is at most
//! one [`BodyStripe`] read from `meta.stripe`.

use std::sync::Arc;

use super::catalog::{ObjectMeta, YamlCatalog};
use super::response;
use super::routing::{self, Route};
use crate::frontend::conn::{Action, BodyStripe, ServePolicy, split_query};
use crate::frontend::range::{ByteRange, RangeError, ResolvedRange, full_object};
use crate::http::{HttpRequest, Method};
use crate::ring::NetHandle;
use crate::storage::StripeReq;

/// Per-shard S3 serving policy. Holds the shared, immutable catalog;
/// cloned cheaply per shard behind an `Arc`. `respond` takes `&self`
/// and needs no interior mutability because the catalog is read-only.
pub struct S3Policy {
    catalog: Arc<YamlCatalog>,
}

impl S3Policy {
    /// Build a policy over a loaded catalog.
    pub fn new(catalog: Arc<YamlCatalog>) -> Self {
        Self { catalog }
    }

    /// Pure request -> [`Action`] decision. Split out from
    /// [`ServePolicy::respond`] so it can be unit-tested without a
    /// socket ring (S3 never uses the ring to resolve metadata).
    fn plan(&self, req: &HttpRequest<'_>) -> Action {
        // Only GET and HEAD are served; everything else is 405 across
        // all paths, matching the standalone frontend's fallback.
        let is_head = match req.method {
            Method::GET => false,
            Method::HEAD => true,
            _ => return Action::Respond(response::method_not_allowed()),
        };

        let (path, query) = split_query(req.target);
        match routing::route(path) {
            Route::NotAllowed => Action::Respond(response::method_not_allowed()),
            Route::BucketRoot => {
                if routing::has_location(query) {
                    Action::Respond(response::bucket_location())
                } else {
                    Action::Respond(response::method_not_allowed())
                }
            }
            Route::Object { bucket, key } => {
                self.serve_object(bucket, key, req.header("range"), is_head)
            }
        }
    }

    /// Resolve an object GET/HEAD: catalog lookup, range resolution,
    /// then either a head-only [`Action::Respond`] (HEAD) or a
    /// [`Action::Stream`] whose body is the (single) stripe slice.
    fn serve_object(
        &self,
        bucket: &str,
        key: &str,
        raw_range: Option<&str>,
        is_head: bool,
    ) -> Action {
        let meta = match self.catalog.get(bucket, key) {
            Some(m) => m,
            None => return Action::Respond(response::not_found(bucket, key)),
        };

        let (head, resolved) = match resolve_range(raw_range, meta.size) {
            RangeOutcome::Unsatisfiable => return Action::Respond(response::invalid_range(meta)),
            RangeOutcome::Full => (response::full_head(meta), full_object(meta.size)),
            RangeOutcome::Partial(r) => (response::partial_head(meta, r), r),
        };

        if is_head {
            return Action::Respond(head);
        }
        Action::Stream {
            head,
            body: body_plan(meta, resolved),
        }
    }
}

impl ServePolicy for S3Policy {
    async fn respond(&self, _handle: &NetHandle, req: &HttpRequest<'_>) -> Action {
        self.plan(req)
    }

    fn malformed_request(&self) -> Vec<u8> {
        response::bad_request()
    }
}

/// The body plan for a resolved span. In v0 an object is a single
/// stripe, so this is at most one [`BodyStripe`] read out of
/// `meta.stripe` at the resolved intra-stripe window. An empty span
/// (a zero-length object, or `bytes=-0`-style empty resolve) yields no
/// stripes and the engine streams nothing after the head.
fn body_plan(meta: &ObjectMeta, resolved: ResolvedRange) -> Vec<BodyStripe> {
    if resolved.is_empty() {
        return Vec::new();
    }
    vec![BodyStripe {
        req: StripeReq::new(meta.stripe),
        intra_offset: resolved.start,
        intra_len: resolved.len(),
    }]
}

/// Outcome of resolving an S3 `Range` header against an object size.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum RangeOutcome {
    /// Serve the whole object (`200`). Covers no header and an
    /// ignored-malformed header.
    Full,
    /// Serve a resolved sub-range (`206`).
    Partial(ResolvedRange),
    /// The range cannot be satisfied (`416`).
    Unsatisfiable,
}

/// Map a raw `Range` header value onto an [`RangeOutcome`], folding the
/// shared [`ByteRange`] parser into S3's semantics:
///
/// - no header -> `Full` (200).
/// - syntactically malformed / bad number -> ignored, `Full` (200),
///   per RFC 9110 §14.1.1.
/// - an inverted closed range (`bytes=10-5`) -> `Unsatisfiable` (416),
///   matching the standalone S3 frontend.
/// - otherwise resolve against `size`: a satisfiable range is
///   `Partial` (206); a range wholly past EOF is `Unsatisfiable` (416).
fn resolve_range(raw: Option<&str>, size: u64) -> RangeOutcome {
    let raw = match raw {
        Some(r) => r,
        None => return RangeOutcome::Full,
    };
    match ByteRange::parse(raw) {
        Err(RangeError::Inverted) => RangeOutcome::Unsatisfiable,
        Err(_) => RangeOutcome::Full,
        Ok(br) => match br.resolve(size) {
            Ok(resolved) => RangeOutcome::Partial(resolved),
            Err(RangeError::Unsatisfiable { .. }) => RangeOutcome::Unsatisfiable,
            Err(_) => RangeOutcome::Full,
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const CATALOG: &str = r#"
objects:
  - bucket: demo
    key: helloworld.txt
    stripe: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    size: 7
    content_type: text/plain
  - bucket: demo
    key: empty.bin
    stripe: 0000000000000000000000000000000000000000000000000000000000000000
    size: 0
"#;

    fn policy() -> S3Policy {
        S3Policy::new(Arc::new(YamlCatalog::from_yaml(CATALOG).unwrap()))
    }

    /// Parse a raw request line + minimal headers into an owned buffer
    /// the borrowed `HttpRequest` reads from. Returns the buffer so the
    /// caller keeps it alive across the borrow.
    fn raw(method: &str, target: &str, extra: &str) -> Vec<u8> {
        format!("{method} {target} HTTP/1.1\r\nHost: x\r\n{extra}\r\n").into_bytes()
    }

    fn respond_str(act: &Action) -> &str {
        match act {
            Action::Respond(bytes) => std::str::from_utf8(bytes).unwrap(),
            _ => panic!("expected Action::Respond, got Stream/Close"),
        }
    }

    #[test]
    fn get_full_object_streams_single_stripe() {
        let buf = raw("GET", "/demo/helloworld.txt", "");
        let req = HttpRequest::parse(&buf).unwrap();
        match policy().plan(&req) {
            Action::Stream { head, body } => {
                let s = std::str::from_utf8(&head).unwrap();
                assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
                assert!(s.contains("content-length: 7\r\n"), "got: {s}");
                assert_eq!(body.len(), 1);
                assert_eq!(body[0].intra_offset, 0);
                assert_eq!(body[0].intra_len, 7);
            }
            other => panic!("expected Stream, got {:?}", variant(&other)),
        }
    }

    #[test]
    fn head_object_is_head_only_respond() {
        let buf = raw("HEAD", "/demo/helloworld.txt", "");
        let req = HttpRequest::parse(&buf).unwrap();
        let act = policy().plan(&req);
        let s = respond_str(&act);
        assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
        assert!(s.contains("content-length: 7\r\n"), "got: {s}");
        // No body after the blank line.
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn get_with_range_streams_partial() {
        let buf = raw("GET", "/demo/helloworld.txt", "Range: bytes=2-4\r\n");
        let req = HttpRequest::parse(&buf).unwrap();
        match policy().plan(&req) {
            Action::Stream { head, body } => {
                let s = std::str::from_utf8(&head).unwrap();
                assert!(
                    s.starts_with("HTTP/1.1 206 Partial Content\r\n"),
                    "got: {s}"
                );
                assert!(s.contains("content-range: bytes 2-4/7\r\n"), "got: {s}");
                assert_eq!(body.len(), 1);
                assert_eq!(body[0].intra_offset, 2);
                assert_eq!(body[0].intra_len, 3);
            }
            other => panic!("expected Stream, got {:?}", variant(&other)),
        }
    }

    #[test]
    fn unknown_key_is_404() {
        let buf = raw("GET", "/demo/missing.txt", "");
        let req = HttpRequest::parse(&buf).unwrap();
        let act = policy().plan(&req);
        let s = respond_str(&act);
        assert!(s.starts_with("HTTP/1.1 404 Not Found\r\n"), "got: {s}");
        assert!(s.contains("<Code>NoSuchKey</Code>"), "got: {s}");
    }

    #[test]
    fn unknown_bucket_is_404() {
        let buf = raw("GET", "/nope/helloworld.txt", "");
        let req = HttpRequest::parse(&buf).unwrap();
        let act = policy().plan(&req);
        let s = respond_str(&act);
        assert!(s.starts_with("HTTP/1.1 404 Not Found\r\n"), "got: {s}");
    }

    #[test]
    fn put_is_405() {
        let buf = raw("PUT", "/demo/helloworld.txt", "");
        let req = HttpRequest::parse(&buf).unwrap();
        let s_owned = respond_str(&policy().plan(&req)).to_string();
        assert!(
            s_owned.starts_with("HTTP/1.1 405 Method Not Allowed\r\n"),
            "got: {s_owned}"
        );
    }

    #[test]
    fn bucket_location_query() {
        let buf = raw("GET", "/demo/?location", "");
        let req = HttpRequest::parse(&buf).unwrap();
        let act = policy().plan(&req);
        let s = respond_str(&act);
        assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
        assert!(s.contains("<LocationConstraint"), "got: {s}");
    }

    #[test]
    fn bucket_root_without_location_is_405() {
        let buf = raw("GET", "/demo/", "");
        let req = HttpRequest::parse(&buf).unwrap();
        let act = policy().plan(&req);
        let s = respond_str(&act);
        assert!(
            s.starts_with("HTTP/1.1 405 Method Not Allowed\r\n"),
            "got: {s}"
        );
    }

    #[test]
    fn zero_size_object_streams_empty_body() {
        let buf = raw("GET", "/demo/empty.bin", "");
        let req = HttpRequest::parse(&buf).unwrap();
        match policy().plan(&req) {
            Action::Stream { head, body } => {
                let s = std::str::from_utf8(&head).unwrap();
                assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
                assert!(s.contains("content-length: 0\r\n"), "got: {s}");
                assert!(body.is_empty(), "expected no stripes for empty object");
            }
            other => panic!("expected Stream, got {:?}", variant(&other)),
        }
    }

    #[test]
    fn inverted_range_is_416() {
        let buf = raw("GET", "/demo/helloworld.txt", "Range: bytes=4-2\r\n");
        let req = HttpRequest::parse(&buf).unwrap();
        let act = policy().plan(&req);
        let s = respond_str(&act);
        assert!(
            s.starts_with("HTTP/1.1 416 Range Not Satisfiable\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes */7\r\n"), "got: {s}");
    }

    #[test]
    fn range_past_eof_is_416() {
        let buf = raw("GET", "/demo/helloworld.txt", "Range: bytes=100-200\r\n");
        let req = HttpRequest::parse(&buf).unwrap();
        let act = policy().plan(&req);
        let s = respond_str(&act);
        assert!(
            s.starts_with("HTTP/1.1 416 Range Not Satisfiable\r\n"),
            "got: {s}"
        );
    }

    #[test]
    fn malformed_range_is_ignored_full_200() {
        let buf = raw("GET", "/demo/helloworld.txt", "Range: items=0-1\r\n");
        let req = HttpRequest::parse(&buf).unwrap();
        match policy().plan(&req) {
            Action::Stream { head, body } => {
                let s = std::str::from_utf8(&head).unwrap();
                assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
                assert_eq!(body.len(), 1);
                assert_eq!(body[0].intra_len, 7);
            }
            other => panic!("expected Stream, got {:?}", variant(&other)),
        }
    }

    // --- resolve_range unit coverage ---------------------------------

    #[test]
    fn resolve_range_no_header_is_full() {
        assert_eq!(resolve_range(None, 100), RangeOutcome::Full);
    }

    #[test]
    fn resolve_range_malformed_is_full() {
        assert_eq!(resolve_range(Some("not-a-range"), 100), RangeOutcome::Full);
        assert_eq!(
            resolve_range(Some("bytes=abc-def"), 100),
            RangeOutcome::Full
        );
    }

    #[test]
    fn resolve_range_inverted_is_unsatisfiable() {
        assert_eq!(
            resolve_range(Some("bytes=10-5"), 100),
            RangeOutcome::Unsatisfiable
        );
    }

    #[test]
    fn resolve_range_partial_and_eof() {
        assert_eq!(
            resolve_range(Some("bytes=10-19"), 100),
            RangeOutcome::Partial(ResolvedRange { start: 10, end: 20 })
        );
        assert_eq!(
            resolve_range(Some("bytes=100-200"), 100),
            RangeOutcome::Unsatisfiable
        );
    }

    fn variant(act: &Action) -> &'static str {
        match act {
            Action::Respond(_) => "Respond",
            Action::Stream { .. } => "Stream",
            Action::Close => "Close",
        }
    }
}
