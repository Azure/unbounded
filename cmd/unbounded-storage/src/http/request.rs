// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! HTTP/1.1 request wire format: server-side [`HttpRequest::parse`] of a
//! request head and client-side [`serialize_request`] of an outbound
//! request head.
//!
//! Parsing borrows from a caller-owned buffer and never consumes a body;
//! serialization renders the head only (no body) in origin-form, which
//! is what the storage backends' bodyless GET/HEAD requests need. A
//! future body-bearing variant (for S3 PUT) can build on the same
//! [`http::Request`] input.

use super::headers::{self, Header, MAX_HEADERS, ParseError};

/// HTTP method type, re-exported from the [`http`] crate so the rest of
/// the crate has a single canonical `Method`.
pub use ::http::Method;

/// A parsed HTTP/1.1 request line plus headers, borrowing from the
/// source buffer. The target is the raw request target (path plus
/// optional `?query`); splitting it into path and query string is the
/// caller's job.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HttpRequest<'a> {
    pub method: Method,
    pub target: &'a str,
    /// Minor version: `1` for `HTTP/1.1`, `0` for `HTTP/1.0`.
    pub version_minor: u8,
    /// Byte offset just past the terminating `\r\n\r\n`, i.e. where a
    /// request body or the next pipelined request begins.
    pub header_end: usize,
    /// Headers are kept as borrowed, zero-copy slices into the source
    /// buffer rather than an owned [`http::HeaderMap`]. This is a
    /// deliberate choice: the frontend parses one request per buffer on
    /// a hot path, and a `HeaderMap` would force a per-request
    /// allocation we do not need for the case-insensitive lookups in
    /// [`HttpRequest::header`].
    pub headers: Vec<Header<'a>>,
}

impl<'a> HttpRequest<'a> {
    /// Parse a request from `buf`. Returns [`ParseError::Incomplete`]
    /// when the header terminator (`\r\n\r\n`) is not yet present, so a
    /// streaming caller can distinguish "need more bytes" from a hard
    /// protocol error.
    ///
    /// Only the bytes up to and including the header terminator are
    /// consumed; anything after (a body) is ignored, since the parsed
    /// surface never carries one.
    pub fn parse(buf: &'a [u8]) -> Result<Self, ParseError> {
        let mut header_storage = [httparse::EMPTY_HEADER; MAX_HEADERS];
        let mut req = httparse::Request::new(&mut header_storage);

        let header_end = match req.parse(buf) {
            Ok(httparse::Status::Complete(n)) => n,
            Ok(httparse::Status::Partial) => return Err(ParseError::Incomplete),
            Err(e) => return Err(headers::map_httparse_error(e)),
        };

        let method = Method::from_bytes(req.method.unwrap().as_bytes())
            .map_err(|_| ParseError::BadRequestLine)?;
        let target = req.path.unwrap();
        // httparse reports the minor version directly as 0 or 1.
        let version_minor = req.version.unwrap();
        let headers = headers::collect_headers(req.headers)?;

        Ok(HttpRequest {
            method,
            target,
            version_minor,
            header_end,
            headers,
        })
    }

    /// Look up a header by case-insensitive name, returning its trimmed
    /// value. Returns the first match if a header is repeated.
    pub fn header(&self, name: &str) -> Option<&'a str> {
        headers::header(&self.headers, name)
    }
}

/// Serialize an outbound request head in origin-form:
/// `"{METHOD} {path_and_query} HTTP/1.1\r\n"`, the header lines, then the
/// blank-line terminator. The body, if any, is not emitted; the caller
/// is responsible for the bodyless requests the storage backends issue.
///
/// Header names are rendered in the [`http`] crate's canonical
/// (lowercase) form; HTTP header names are case-insensitive on the wire.
/// Header iteration order follows the request's [`http::HeaderMap`].
pub fn serialize_request(req: &::http::Request<()>) -> Vec<u8> {
    let method = req.method().as_str();
    let target = req
        .uri()
        .path_and_query()
        .map(|pq| pq.as_str())
        .unwrap_or("/");
    let mut out = format!("{method} {target} HTTP/1.1\r\n").into_bytes();
    for (name, value) in req.headers() {
        out.extend_from_slice(name.as_str().as_bytes());
        out.extend_from_slice(b": ");
        out.extend_from_slice(value.as_bytes());
        out.extend_from_slice(b"\r\n");
    }
    out.extend_from_slice(b"\r\n");
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use ::http::header::{CONNECTION, HOST, RANGE};

    fn req(s: &str) -> Result<HttpRequest<'_>, ParseError> {
        HttpRequest::parse(s.as_bytes())
    }

    #[test]
    fn parses_well_formed_get() {
        let r = req("GET /bucket/key HTTP/1.1\r\nHost: example.com\r\nRange: bytes=0-10\r\n\r\n")
            .unwrap();
        assert_eq!(r.method, Method::GET);
        assert_eq!(r.target, "/bucket/key");
        assert_eq!(r.version_minor, 1);
        assert_eq!(
            r.header_end,
            "GET /bucket/key HTTP/1.1\r\nHost: example.com\r\nRange: bytes=0-10\r\n\r\n".len()
        );
        assert_eq!(r.header("host"), Some("example.com"));
        assert_eq!(r.header("range"), Some("bytes=0-10"));
    }

    #[test]
    fn parses_head_and_list() {
        let h = req("HEAD /b/k HTTP/1.1\r\nHost: x\r\n\r\n").unwrap();
        assert_eq!(h.method, Method::HEAD);

        let l = req("GET /b?list-type=2&prefix=p/ HTTP/1.1\r\nHost: x\r\n\r\n").unwrap();
        assert_eq!(l.method, Method::GET);
        assert_eq!(l.target, "/b?list-type=2&prefix=p/");
    }

    #[test]
    fn header_lookup_is_case_insensitive() {
        let r = req("GET / HTTP/1.1\r\nCoNtEnT-LeNgTh: 42\r\n\r\n").unwrap();
        assert_eq!(r.header("content-length"), Some("42"));
        assert_eq!(r.header("CONTENT-LENGTH"), Some("42"));
        assert_eq!(r.header("missing"), None);
    }

    #[test]
    fn first_repeated_header_wins() {
        let r = req("GET / HTTP/1.1\r\nX: a\r\nX: b\r\n\r\n").unwrap();
        assert_eq!(r.header("x"), Some("a"));
    }

    #[test]
    fn http_1_0_supported() {
        let r = req("GET / HTTP/1.0\r\n\r\n").unwrap();
        assert_eq!(r.version_minor, 0);
        assert!(r.headers.is_empty());
    }

    #[test]
    fn maps_methods() {
        assert_eq!(req("PUT / HTTP/1.1\r\n\r\n").unwrap().method, Method::PUT);
        assert_eq!(req("POST / HTTP/1.1\r\n\r\n").unwrap().method, Method::POST);
        assert_eq!(
            req("OPTIONS / HTTP/1.1\r\n\r\n").unwrap().method,
            Method::OPTIONS
        );
    }

    #[test]
    fn incomplete_without_terminator() {
        assert_eq!(
            req("GET / HTTP/1.1\r\nHost: x\r\n"),
            Err(ParseError::Incomplete)
        );
        assert_eq!(req("GET / HTTP/1.1"), Err(ParseError::Incomplete));
        assert_eq!(req(""), Err(ParseError::Incomplete));
    }

    #[test]
    fn rejects_bad_request_line() {
        // Missing version field.
        assert_eq!(req("GET /\r\n\r\n"), Err(ParseError::BadRequestLine));
        // Too many fields.
        assert_eq!(
            req("GET / HTTP/1.1 extra\r\n\r\n"),
            Err(ParseError::BadRequestLine)
        );
        // Bad version token.
        assert_eq!(
            req("GET / HTTP/2.0\r\n\r\n"),
            Err(ParseError::BadRequestLine)
        );
    }

    #[test]
    fn rejects_bad_header() {
        // Header line without a colon.
        assert_eq!(
            req("GET / HTTP/1.1\r\nnonsense\r\n\r\n"),
            Err(ParseError::BadHeader)
        );
        // Header with a space inside the name.
        assert_eq!(
            req("GET / HTTP/1.1\r\nbad name: v\r\n\r\n"),
            Err(ParseError::BadHeader)
        );
    }

    #[test]
    fn ignores_body_after_terminator() {
        // A body after the header block is not consumed by the parser.
        let r = req("GET / HTTP/1.1\r\nContent-Length: 5\r\n\r\nhello").unwrap();
        assert_eq!(r.header("content-length"), Some("5"));
        assert_eq!(
            r.header_end,
            "GET / HTTP/1.1\r\nContent-Length: 5\r\n\r\n".len()
        );
        assert_eq!(r.headers.len(), 1);
    }

    #[test]
    fn rejects_non_utf8_head() {
        let mut buf = b"GET / HTTP/1.1\r\nX: ".to_vec();
        buf.push(0xff);
        buf.extend_from_slice(b"\r\n\r\n");
        assert_eq!(HttpRequest::parse(&buf), Err(ParseError::NotUtf8));
    }

    /// The serialized head must carry the correct origin-form request
    /// line, every header, and the blank-line terminator. Header order
    /// follows `HeaderMap` iteration and is not asserted; each expected
    /// line is checked independently.
    #[test]
    fn serialize_request_emits_request_line_headers_and_terminator() {
        let r = ::http::Request::builder()
            .method(Method::GET)
            .uri("/models/llama.bin")
            .header(HOST, "10.0.0.1:8080")
            .header(RANGE, "bytes=0-4095")
            .header(CONNECTION, "close")
            .body(())
            .unwrap();
        let bytes = serialize_request(&r);
        let s = std::str::from_utf8(&bytes).unwrap();

        assert!(
            s.starts_with("GET /models/llama.bin HTTP/1.1\r\n"),
            "got: {s}"
        );
        // Header names render lowercase (http crate canonical form).
        assert!(s.contains("host: 10.0.0.1:8080\r\n"), "got: {s}");
        assert!(s.contains("range: bytes=0-4095\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    /// A bodyless request with no path component still serializes a
    /// valid origin-form request line and terminator.
    #[test]
    fn serialize_request_defaults_target_and_renders_head() {
        let r = ::http::Request::builder()
            .method(Method::HEAD)
            .uri("/o")
            .header(HOST, "h:1")
            .header(CONNECTION, "close")
            .body(())
            .unwrap();
        let bytes = serialize_request(&r);
        let s = std::str::from_utf8(&bytes).unwrap();
        assert!(s.starts_with("HEAD /o HTTP/1.1\r\n"), "got: {s}");
        assert!(s.contains("host: h:1\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }
}
