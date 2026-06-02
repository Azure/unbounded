// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! S3 response serialization: object response heads (200 / 206 / HEAD)
//! and the S3-shaped XML error bodies (`NoSuchKey`, `InvalidRange`,
//! `MethodNotAllowed`, `InternalError`) plus the `?location` reply.
//!
//! Every function returns a fully-formed byte buffer ready to hand to
//! the serving engine: an object *head* ends at the blank
//! `\r\n\r\n` line (the engine streams the body after it), while a
//! self-contained reply (errors, `?location`) carries its XML body
//! appended after the head. All responses set `Connection: close`,
//! matching the one-request-per-connection serving model.

use std::borrow::Cow;
use std::fmt::Write as _;

use ::http::header::{
    ACCEPT_RANGES, CONNECTION, CONTENT_LENGTH, CONTENT_RANGE, CONTENT_TYPE, ETAG, LAST_MODIFIED,
};
use ::http::response::Builder;
use ::http::{Response, StatusCode};

use super::catalog::ObjectMeta;
use crate::frontend::range::ResolvedRange;
use crate::http::serialize_response_head;

/// Build the response head for serving a whole object (`200 OK`).
///
/// Returns head bytes only (terminated by the blank line); the serving
/// engine streams the body stripes after it. `Content-Length` is the
/// object's full size.
pub(crate) fn full_head(meta: &ObjectMeta) -> Vec<u8> {
    finish_head(object_builder(StatusCode::OK, meta), meta.size, None)
}

/// Build the `206 Partial Content` head for a resolved byte range.
///
/// `END` in `Content-Range` is inclusive (`resolved.end - 1`).
pub(crate) fn partial_head(meta: &ObjectMeta, resolved: ResolvedRange) -> Vec<u8> {
    let content_range = format!(
        "bytes {}-{}/{}",
        resolved.start,
        resolved.end - 1,
        meta.size
    );
    finish_head(
        object_builder(StatusCode::PARTIAL_CONTENT, meta),
        resolved.len(),
        Some(content_range),
    )
}

/// `404 NoSuchKey` for a `(bucket, key)` not present in the catalog.
pub(crate) fn not_found(bucket: &str, key: &str) -> Vec<u8> {
    let xml = s3_error(
        "NoSuchKey",
        &format!("The specified key does not exist: {bucket}/{key}"),
    );
    // S3 advertises Accept-Ranges on missing-key responses so callers
    // know ranges are supported by the resource type even though this
    // specific key is unknown.
    finish_body(
        Response::builder()
            .status(StatusCode::NOT_FOUND)
            .header(CONTENT_TYPE, "application/xml")
            .header(ACCEPT_RANGES, "bytes"),
        xml.into_bytes(),
    )
}

/// `416 Range Not Satisfiable` (`InvalidRange`).
///
/// Includes `ETag`, `Last-Modified`, and `Accept-Ranges: bytes` per
/// RFC 9110 so `If-Range` clients can identify the current
/// representation and retry. Content-Type is `application/xml` for the
/// error body, overriding the object's own MIME type.
pub(crate) fn invalid_range(meta: &ObjectMeta) -> Vec<u8> {
    let msg = format!(
        "The specified range is not valid for an object of size {}.",
        meta.size
    );
    let xml = s3_error("InvalidRange", &msg);
    finish_body(
        Response::builder()
            .status(StatusCode::RANGE_NOT_SATISFIABLE)
            .header(CONTENT_TYPE, "application/xml")
            .header(ETAG, meta.etag.clone())
            .header(LAST_MODIFIED, meta.last_modified.clone())
            .header(ACCEPT_RANGES, "bytes")
            .header(CONTENT_RANGE, format!("bytes */{}", meta.size)),
        xml.into_bytes(),
    )
}

/// `405 MethodNotAllowed`.
pub(crate) fn method_not_allowed() -> Vec<u8> {
    let xml = s3_error(
        "MethodNotAllowed",
        "The specified method is not allowed against this resource.",
    );
    finish_body(
        Response::builder()
            .status(StatusCode::METHOD_NOT_ALLOWED)
            .header(CONTENT_TYPE, "application/xml"),
        xml.into_bytes(),
    )
}

/// `400 BadRequest`, used when the request head could not be parsed.
pub(crate) fn bad_request() -> Vec<u8> {
    let xml = s3_error("BadRequest", "The request could not be parsed.");
    finish_body(
        Response::builder()
            .status(StatusCode::BAD_REQUEST)
            .header(CONTENT_TYPE, "application/xml"),
        xml.into_bytes(),
    )
}

/// `200 OK` for `GET /<bucket>/?location`.
///
/// Returns an empty `LocationConstraint`, which AWS S3 treats as the
/// default region (`us-east-1`) and `s3cmd` accepts without further
/// region negotiation. Intentionally permissive: it does not check
/// whether `<bucket>` exists in the catalog, matching AWS behavior
/// where `?location` is answerable for any bucket name.
pub(crate) fn bucket_location() -> Vec<u8> {
    let body = b"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
        <LocationConstraint xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\"></LocationConstraint>"
        .to_vec();
    finish_body(
        Response::builder()
            .status(StatusCode::OK)
            .header(CONTENT_TYPE, "application/xml"),
        body,
    )
}

/// Common builder for the object-bearing responses (200 / 206 / HEAD):
/// the object's MIME type, ETag, Last-Modified, and `Accept-Ranges`.
fn object_builder(status: StatusCode, meta: &ObjectMeta) -> Builder {
    Response::builder()
        .status(status)
        .header(CONTENT_TYPE, meta.content_type.clone())
        .header(ETAG, meta.etag.clone())
        .header(LAST_MODIFIED, meta.last_modified.clone())
        .header(ACCEPT_RANGES, "bytes")
}

/// Finish an object head: set `Content-Length` to the served byte
/// count (not a body length, since the engine streams the body
/// separately), an optional `Content-Range`, `Connection: close`, and
/// serialize. No body is appended.
fn finish_head(builder: Builder, content_length: u64, content_range: Option<String>) -> Vec<u8> {
    let mut builder = builder
        .header(CONTENT_LENGTH, content_length.to_string())
        .header(CONNECTION, "close");
    if let Some(cr) = content_range {
        builder = builder.header(CONTENT_RANGE, cr);
    }
    let resp = builder.body(()).expect("valid object response head");
    serialize_response_head(&resp)
}

/// Finish a self-contained response: set `Content-Length` to the body
/// length, `Connection: close`, serialize the head, and append `body`.
fn finish_body(builder: Builder, body: Vec<u8>) -> Vec<u8> {
    let resp = builder
        .header(CONTENT_LENGTH, body.len().to_string())
        .header(CONNECTION, "close")
        .body(())
        .expect("valid self-contained response head");
    let mut out = serialize_response_head(&resp);
    out.extend_from_slice(&body);
    out
}

/// Build an S3-shaped XML error body. `code` and `message` are
/// XML-escaped so callers can pass raw strings (including bucket/key
/// names that may contain `&`, `<`, `>`, quotes, etc.) without
/// producing malformed XML.
fn s3_error(code: &str, message: &str) -> String {
    let code = xml_escape(code);
    let message = xml_escape(message);
    let mut xml = String::with_capacity(96 + code.len() + message.len());
    write!(
        xml,
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
         <Error><Code>{code}</Code><Message>{message}</Message></Error>",
    )
    .expect("string write infallible");
    xml
}

/// XML-escape `&`, `<`, `>`, `"`, and `'` for use inside element text.
/// Also strips XML-1.0-illegal C0 control characters (`\x00`-`\x08`,
/// `\x0B`, `\x0C`, `\x0E`-`\x1F`); XML has no escape sequence for them
/// so silent removal is the only option that yields well-formed XML,
/// matching AWS SDK behavior. `\t`, `\n`, and `\r` are preserved.
///
/// Returns `Cow::Borrowed` when no escaping is needed so the common
/// path allocates nothing.
fn xml_escape(s: &str) -> Cow<'_, str> {
    if !s.bytes().any(needs_escape) {
        return Cow::Borrowed(s);
    }
    let mut out = String::with_capacity(s.len() + 16);
    for c in s.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&apos;"),
            // Strip XML-illegal C0 controls. `\t`, `\n`, `\r` are the
            // only sub-space characters XML 1.0 permits, so they pass.
            c if is_illegal_xml_char(c) => {}
            other => out.push(other),
        }
    }
    Cow::Owned(out)
}

/// Whether `b` should trigger a copy in [`xml_escape`]: a metacharacter
/// that needs entity-escaping, or an XML-illegal C0 control byte.
fn needs_escape(b: u8) -> bool {
    matches!(b, b'&' | b'<' | b'>' | b'"' | b'\'')
        || (b < 0x20 && !matches!(b, b'\t' | b'\n' | b'\r'))
}

/// Whether `c` is a char XML 1.0 does not allow in document text.
/// Only the C0 controls are reachable for our UTF-8 inputs.
fn is_illegal_xml_char(c: char) -> bool {
    (c as u32) < 0x20 && !matches!(c, '\t' | '\n' | '\r')
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::StripeKey;

    fn dummy_meta() -> ObjectMeta {
        ObjectMeta {
            stripe: StripeKey([0u8; 32]),
            size: 1000,
            etag: "\"0000000000000000\"".into(),
            content_type: "text/plain".into(),
            last_modified: "Thu, 01 Jan 1970 00:00:00 GMT".into(),
        }
    }

    fn as_str(v: &[u8]) -> &str {
        std::str::from_utf8(v).unwrap()
    }

    #[test]
    fn full_head_exact_bytes() {
        let head = full_head(&dummy_meta());
        let s = as_str(&head);
        assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
        assert!(s.contains("content-type: text/plain\r\n"), "got: {s}");
        assert!(s.contains("etag: \"0000000000000000\"\r\n"), "got: {s}");
        assert!(
            s.contains("last-modified: Thu, 01 Jan 1970 00:00:00 GMT\r\n"),
            "got: {s}"
        );
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("content-length: 1000\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn partial_head_has_content_range_inclusive_end() {
        let head = partial_head(&dummy_meta(), ResolvedRange { start: 10, end: 30 });
        let s = as_str(&head);
        assert!(
            s.starts_with("HTTP/1.1 206 Partial Content\r\n"),
            "got: {s}"
        );
        assert!(
            s.contains("content-range: bytes 10-29/1000\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-length: 20\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn not_found_is_well_formed_and_escapes() {
        let resp = not_found("b", "weird<key&name>");
        let s = as_str(&resp);
        assert!(s.starts_with("HTTP/1.1 404 Not Found\r\n"), "got: {s}");
        assert!(s.contains("content-type: application/xml\r\n"), "got: {s}");
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("<Code>NoSuchKey</Code>"), "got: {s}");
        assert!(
            s.contains("weird&lt;key&amp;name&gt;"),
            "missing escaped form: {s}"
        );
        assert!(
            !s.contains("weird<key&name>"),
            "raw unescaped substring leaked: {s}"
        );
        assert_xml_body_well_formed(s);
    }

    #[test]
    fn invalid_range_has_content_range_star() {
        let resp = invalid_range(&dummy_meta());
        let s = as_str(&resp);
        assert!(
            s.starts_with("HTTP/1.1 416 Range Not Satisfiable\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes */1000\r\n"), "got: {s}");
        assert!(s.contains("content-type: application/xml\r\n"), "got: {s}");
        assert!(s.contains("<Code>InvalidRange</Code>"), "got: {s}");
    }

    #[test]
    fn method_not_allowed_xml() {
        let resp = method_not_allowed();
        let s = as_str(&resp);
        assert!(
            s.starts_with("HTTP/1.1 405 Method Not Allowed\r\n"),
            "got: {s}"
        );
        assert!(s.contains("<Code>MethodNotAllowed</Code>"), "got: {s}");
    }

    #[test]
    fn bad_request_xml() {
        let resp = bad_request();
        let s = as_str(&resp);
        assert!(s.starts_with("HTTP/1.1 400 Bad Request\r\n"), "got: {s}");
        assert!(s.contains("<Code>BadRequest</Code>"), "got: {s}");
    }

    #[test]
    fn bucket_location_is_empty_constraint() {
        let resp = bucket_location();
        let s = as_str(&resp);
        assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
        assert!(s.contains("<LocationConstraint"), "got: {s}");
        assert!(s.contains("</LocationConstraint>"), "got: {s}");
    }

    #[test]
    fn content_length_matches_body_for_errors() {
        // The serialized Content-Length must equal the appended XML
        // body length so a strict client does not hang waiting for
        // missing bytes.
        let resp = method_not_allowed();
        let s = as_str(&resp);
        let split = s.find("\r\n\r\n").unwrap() + 4;
        let body_len = resp.len() - split;
        let want = format!("content-length: {body_len}\r\n");
        assert!(s.contains(&want), "expected {want:?} in: {s}");
    }

    #[test]
    fn xml_escape_borrows_when_clean() {
        assert!(matches!(xml_escape("plain text 1.0"), Cow::Borrowed(_)));
    }

    #[test]
    fn xml_escape_replaces_all_specials() {
        assert_eq!(
            xml_escape("a<b>c&d\"e'f").as_ref(),
            "a&lt;b&gt;c&amp;d&quot;e&apos;f"
        );
    }

    #[test]
    fn xml_escape_strips_illegal_c0_controls() {
        assert_eq!(xml_escape("a\x01b\x02c").as_ref(), "abc");
        assert_eq!(xml_escape("\x00\x01\x1f").as_ref(), "");
        // The three legal whitespace controls survive without copying.
        assert!(matches!(xml_escape("a\tb\nc\rd"), Cow::Borrowed(_)));
        assert_eq!(xml_escape("a\x01<b>").as_ref(), "a&lt;b&gt;");
    }

    #[test]
    fn not_found_with_control_chars_is_well_formed() {
        let resp = not_found("b", "key\x01with\x02ctrl");
        let s = as_str(&resp);
        assert!(s.contains("keywithctrl"), "body: {s}");
        assert_xml_body_well_formed(s);
    }

    /// Parse the response body (after the blank line) with quick-xml as
    /// a stricter well-formedness check than substring matching.
    fn assert_xml_body_well_formed(resp: &str) {
        let body = &resp[resp.find("\r\n\r\n").unwrap() + 4..];
        let mut reader = quick_xml::Reader::from_str(body);
        loop {
            match reader.read_event() {
                Ok(quick_xml::events::Event::Eof) => break,
                Ok(_) => continue,
                Err(e) => panic!("malformed XML: {e}: {body}"),
            }
        }
    }
}
