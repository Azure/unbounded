// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! HTTP/1.1 response wire format: client-side [`ResponseHead::parse`] of
//! a response head and server-side [`serialize_response_head`] of an
//! outbound response head.
//!
//! Parsing borrows from a caller-owned buffer that the caller fills
//! incrementally: `Ok(None)` means the terminating `\r\n\r\n` has not
//! arrived yet (read more), `Ok(Some(head))` is a complete head, and
//! `Err` is a malformed status/header line. Serialization renders the
//! status line plus headers and the blank-line terminator (no body).

use super::headers::{self, Header, MAX_HEADERS, ParseError};

/// Canonical typed status code, re-exported from the [`http`] crate.
pub use ::http::StatusCode;

/// A parsed HTTP/1.1 response head: the typed status, the byte offset
/// where the body begins (just past the `\r\n\r\n`), and the borrowed
/// header views.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ResponseHead<'a> {
    pub status: StatusCode,
    /// Byte offset just past the terminating `\r\n\r\n`, i.e. where the
    /// body begins in the source buffer.
    pub header_end: usize,
    pub headers: Vec<Header<'a>>,
}

impl<'a> ResponseHead<'a> {
    /// Parse a response header block. Returns `Ok(None)` when the
    /// terminating `\r\n\r\n` has not arrived yet (the caller should
    /// receive more bytes), `Ok(Some(head))` once the head is complete,
    /// and `Err` on a malformed status or header line.
    pub fn parse(buf: &'a [u8]) -> Result<Option<Self>, ParseError> {
        let mut header_storage = [httparse::EMPTY_HEADER; MAX_HEADERS];
        let mut resp = httparse::Response::new(&mut header_storage);

        match resp.parse(buf) {
            Ok(httparse::Status::Complete(n)) => {
                let code = resp.code.ok_or(ParseError::BadStatusLine)?;
                let status = StatusCode::from_u16(code).map_err(|_| ParseError::BadStatusLine)?;
                let headers = headers::collect_headers(resp.headers)?;
                Ok(Some(ResponseHead {
                    status,
                    header_end: n,
                    headers,
                }))
            }
            Ok(httparse::Status::Partial) => Ok(None),
            Err(e) => Err(headers::map_httparse_error(e)),
        }
    }

    /// Look up a header by case-insensitive name, returning its trimmed
    /// value (first match if repeated).
    pub fn header(&self, name: &str) -> Option<&'a str> {
        headers::header(&self.headers, name)
    }

    /// The advertised `Content-Length`, if present and parseable.
    pub fn content_length(&self) -> Option<u64> {
        headers::content_length(&self.headers)
    }

    /// The start offset from a `Content-Range: bytes <start>-<end>/<total>`
    /// header, if present and parseable.
    pub fn content_range_start(&self) -> Option<u64> {
        headers::content_range_start(&self.headers)
    }
}

/// Serialize an outbound response head: `"HTTP/1.1 {code} {reason}\r\n"`
/// (reason from [`StatusCode::canonical_reason`]), the header lines,
/// then the blank-line terminator. No body is emitted.
///
/// Header names are rendered in the [`http`] crate's canonical
/// (lowercase) form; HTTP header names are case-insensitive on the wire.
/// Header iteration order follows the response's [`http::HeaderMap`].
pub fn serialize_response_head(resp: &::http::Response<()>) -> Vec<u8> {
    let status = resp.status();
    let code = status.as_u16();
    let reason = status.canonical_reason().unwrap_or("");
    let mut out = format!("HTTP/1.1 {code} {reason}\r\n").into_bytes();
    for (name, value) in resp.headers() {
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
    use ::http::header::{CONNECTION, CONTENT_LENGTH};

    #[test]
    fn parse_response_head_200() {
        let raw = b"HTTP/1.1 200 OK\r\nContent-Length: 1024\r\nContent-Type: x\r\n\r\nBODY";
        let head = ResponseHead::parse(raw).unwrap().expect("complete head");
        assert_eq!(head.status, StatusCode::OK);
        assert_eq!(head.content_length(), Some(1024));
        assert_eq!(&raw[head.header_end..], b"BODY");
    }

    #[test]
    fn parse_response_head_206_with_content_range() {
        let raw = b"HTTP/1.1 206 Partial Content\r\n\
                    Content-Range: bytes 0-4095/100000\r\n\
                    Content-Length: 4096\r\n\r\n";
        let head = ResponseHead::parse(raw).unwrap().expect("complete head");
        assert_eq!(head.status, StatusCode::PARTIAL_CONTENT);
        assert_eq!(head.content_length(), Some(4096));
        assert_eq!(head.content_range_start(), Some(0));
        assert_eq!(head.header_end, raw.len());
    }

    #[test]
    fn parse_content_range_start_extracts_offset() {
        let raw = b"HTTP/1.1 206 Partial Content\r\n\
                    Content-Range: bytes 8192-12287/100000\r\n\
                    Content-Length: 4096\r\n\r\n";
        let head = ResponseHead::parse(raw).unwrap().unwrap();
        assert_eq!(head.content_range_start(), Some(8192));
    }

    #[test]
    fn parse_content_range_start_case_insensitive_and_absent() {
        let lower = b"HTTP/1.1 206 Partial Content\r\n\
                      content-range: bytes 5-9/10\r\n\r\n";
        assert_eq!(
            ResponseHead::parse(lower)
                .unwrap()
                .unwrap()
                .content_range_start(),
            Some(5)
        );
        // No Content-Range header at all (e.g. a 200): None.
        let none = b"HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n";
        assert_eq!(
            ResponseHead::parse(none)
                .unwrap()
                .unwrap()
                .content_range_start(),
            None
        );
    }

    #[test]
    fn parse_response_head_404_is_status_404() {
        // A 404 parses successfully; the non-2xx decision is the
        // caller's, not the parser's.
        let raw = b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n";
        let head = ResponseHead::parse(raw).unwrap().expect("complete head");
        assert_eq!(head.status, StatusCode::NOT_FOUND);
        assert_eq!(head.content_length(), Some(0));
    }

    #[test]
    fn parse_response_head_incomplete_returns_none() {
        // No terminating CRLFCRLF yet: needs more bytes.
        let raw = b"HTTP/1.1 200 OK\r\nContent-Length: 10\r\n";
        assert_eq!(ResponseHead::parse(raw).unwrap(), None);
        // Even a bare status line with no headers and no terminator.
        assert_eq!(ResponseHead::parse(b"HTTP/1.1 200 OK\r\n").unwrap(), None);
        assert_eq!(ResponseHead::parse(b"").unwrap(), None);
    }

    #[test]
    fn parse_response_head_missing_length_is_none_field() {
        let raw = b"HTTP/1.1 200 OK\r\nContent-Type: x\r\n\r\n";
        let head = ResponseHead::parse(raw).unwrap().unwrap();
        assert_eq!(head.status, StatusCode::OK);
        assert_eq!(head.content_length(), None);
    }

    #[test]
    fn content_length_is_case_insensitive() {
        let raw = b"HTTP/1.1 200 OK\r\ncontent-length: 7\r\n\r\n";
        let head = ResponseHead::parse(raw).unwrap().unwrap();
        assert_eq!(head.content_length(), Some(7));
    }

    #[test]
    fn parse_response_head_rejects_malformed_status() {
        // httparse rejects a non-HTTP status line outright.
        assert!(ResponseHead::parse(b"NOTHTTP 200 OK\r\n\r\n").is_err());
        assert!(ResponseHead::parse(b"HTTP/1.1\r\n\r\n").is_err());
        assert!(ResponseHead::parse(b"HTTP/1.1 twohundred OK\r\n\r\n").is_err());
    }

    /// The serialized head must carry the correct status line, every
    /// header, and the blank-line terminator. Header order follows
    /// `HeaderMap` iteration and is not asserted; each expected line is
    /// checked independently.
    #[test]
    fn serialize_response_head_emits_status_line_headers_and_terminator() {
        let resp = ::http::Response::builder()
            .status(StatusCode::OK)
            .header(CONTENT_LENGTH, "4096")
            .header(CONNECTION, "close")
            .body(())
            .unwrap();
        let bytes = serialize_response_head(&resp);
        let s = std::str::from_utf8(&bytes).unwrap();
        assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
        assert!(s.contains("content-length: 4096\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn serialize_response_head_renders_canonical_reason() {
        let resp = ::http::Response::builder()
            .status(StatusCode::RANGE_NOT_SATISFIABLE)
            .body(())
            .unwrap();
        let bytes = serialize_response_head(&resp);
        let s = std::str::from_utf8(&bytes).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 416 Range Not Satisfiable\r\n"),
            "got: {s}"
        );
    }
}
