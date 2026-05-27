// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::fmt::Write;

use axum::http::{header, HeaderMap, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use bytes::Bytes;
use futures::stream::BoxStream;
use futures::StreamExt;

use crate::catalog::ObjectMeta;
use crate::object::Error as ObjectError;

/// Default `Last-Modified` (IMF-fixdate) used as the
/// `HeaderValue::from_str` fallback. Mirrors the catalog-side default
/// so a malformed string never produces an unset header.
const EPOCH_IMF: &str = "Thu, 01 Jan 1970 00:00:00 GMT";

/// Common headers shared by 200, 206, and HEAD responses.
pub(crate) fn common_headers(meta: &ObjectMeta) -> HeaderMap {
    let mut h = HeaderMap::new();
    h.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_str(&meta.content_type)
            .unwrap_or_else(|_| HeaderValue::from_static("application/octet-stream")),
    );
    h.insert(
        header::ETAG,
        HeaderValue::from_str(&meta.etag)
            .unwrap_or_else(|_| HeaderValue::from_static("\"0000000000000000\"")),
    );
    h.insert(
        header::LAST_MODIFIED,
        HeaderValue::from_str(&meta.last_modified)
            .unwrap_or_else(|_| HeaderValue::from_static(EPOCH_IMF)),
    );
    h.insert(header::ACCEPT_RANGES, HeaderValue::from_static("bytes"));
    h
}

/// 200 OK for a full-object GET (no Range, or range = entire object).
pub fn full_response(
    meta: ObjectMeta,
    stream: BoxStream<'static, Result<Bytes, ObjectError>>,
) -> Response {
    let mut headers = common_headers(&meta);
    headers.insert(
        header::CONTENT_LENGTH,
        HeaderValue::from_str(&meta.size.to_string()).expect("valid u64"),
    );
    let body = axum::body::Body::from_stream(stream.map(|r| r.map_err(axum::Error::new)));
    (StatusCode::OK, headers, body).into_response()
}

/// 206 Partial Content for a ranged GET.
pub fn partial_response(
    meta: ObjectMeta,
    stream: BoxStream<'static, Result<Bytes, ObjectError>>,
    range: super::range::ByteRange,
) -> Response {
    // Satisfiable byte ranges always have `len >= 1`; an empty range
    // would produce a malformed `bytes N-N/size` Content-Range that
    // claims one byte instead of zero. Kept defensive against future
    // parser changes.
    debug_assert!(
        range.len > 0,
        "partial_response called with empty range {range:?}",
    );
    let mut headers = common_headers(&meta);
    headers.insert(
        header::CONTENT_LENGTH,
        HeaderValue::from_str(&range.len.to_string()).expect("valid u64"),
    );
    let content_range = format!(
        "bytes {}-{}/{}",
        range.offset,
        range.offset + range.len.saturating_sub(1),
        meta.size,
    );
    headers.insert(
        header::CONTENT_RANGE,
        HeaderValue::from_str(&content_range).expect("valid content-range"),
    );
    let body = axum::body::Body::from_stream(stream.map(|r| r.map_err(axum::Error::new)));
    (StatusCode::PARTIAL_CONTENT, headers, body).into_response()
}

/// 404 NoSuchKey.
pub fn not_found(bucket: &str, key: &str) -> Response {
    // `s3_error` escapes the message exactly once; pass the raw
    // bucket and key through so we don't end up with double-escaped
    // `&amp;lt;` in the body.
    let xml = s3_error(
        "NoSuchKey",
        &format!("The specified key does not exist: {bucket}/{key}"),
    );
    let mut headers = HeaderMap::new();
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/xml"),
    );
    // S3 advertises Accept-Ranges on missing-key responses so callers
    // know ranges are supported by the resource type even though this
    // specific key is unknown.
    headers.insert(header::ACCEPT_RANGES, HeaderValue::from_static("bytes"));
    (StatusCode::NOT_FOUND, headers, xml).into_response()
}

/// 416 InvalidRange.
///
/// Includes `ETag`, `Last-Modified`, and `Accept-Ranges: bytes` per
/// RFC 9110 §15.5.17 so `If-Range` clients can identify the current
/// representation and retry. Content-Type is overridden from the
/// object's default to `application/xml` for the error body.
pub fn invalid_range(meta: &ObjectMeta) -> Response {
    let msg = if meta.size == 0 {
        "The specified range is not valid for an object of size 0.".into()
    } else {
        format!(
            "The specified range is not valid for an object of size {}.",
            meta.size,
        )
    };
    let xml = s3_error("InvalidRange", &msg);
    let mut headers = common_headers(meta);
    // common_headers sets Content-Type to the object's MIME; override
    // for the XML error body.
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/xml"),
    );
    headers.insert(
        header::CONTENT_RANGE,
        HeaderValue::from_str(&format!("bytes */{}", meta.size)).expect("valid"),
    );
    (StatusCode::RANGE_NOT_SATISFIABLE, headers, xml).into_response()
}

/// 405 MethodNotAllowed.
pub fn method_not_allowed() -> Response {
    let xml = s3_error(
        "MethodNotAllowed",
        "The specified method is not allowed against this resource.",
    );
    let mut headers = HeaderMap::new();
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/xml"),
    );
    (StatusCode::METHOD_NOT_ALLOWED, headers, xml).into_response()
}

/// 200 OK for `GET /<bucket>/?location`.
///
/// Returns an empty `LocationConstraint`, which AWS S3 treats as the
/// default region (`us-east-1`) and `s3cmd` accepts without further
/// region negotiation. This handler is intentionally permissive: it
/// does not check whether `<bucket>` exists in the catalog, matching
/// AWS behavior where `?location` is answerable for any bucket name
/// the caller is authorized for.
pub fn bucket_location() -> Response {
    let body: Bytes = Bytes::from_static(
        b"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
          <LocationConstraint xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\"></LocationConstraint>",
    );
    let mut headers = HeaderMap::new();
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/xml"),
    );
    (StatusCode::OK, headers, body).into_response()
}

/// 500 InternalError.
pub fn internal_error(msg: &str) -> Response {
    let xml = s3_error("InternalError", msg);
    let mut headers = HeaderMap::new();
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/xml"),
    );
    (StatusCode::INTERNAL_SERVER_ERROR, headers, xml).into_response()
}

/// Build an S3-shaped XML error body. `code` and `message` are
/// XML-escaped so callers can pass raw strings (including bucket/key
/// names that may contain `&`, `<`, `>`, quotes, etc.) without
/// worrying about producing malformed XML.
fn s3_error(code: &str, message: &str) -> Bytes {
    let code = xml_escape(code);
    let message = xml_escape(message);
    let mut xml = String::with_capacity(96 + code.len() + message.len());
    write!(
        xml,
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
         <Error><Code>{}</Code><Message>{}</Message></Error>",
        code, message,
    )
    .expect("string write infallible");
    Bytes::from(xml)
}

/// XML-escape `&`, `<`, `>`, `"`, and `'` for use inside element text
/// or attribute values. Also strips XML-1.0-illegal C0 control
/// characters (`\x00`-`\x08`, `\x0B`, `\x0C`, `\x0E`-`\x1F`); XML has
/// no escape sequence for them so silent removal is the only option
/// that yields well-formed XML, and that matches AWS SDK behavior on
/// the same inputs. `\t`, `\n`, and `\r` are preserved.
///
/// Returns `Cow::Borrowed` when no escaping is needed so the common
/// path allocates nothing.
fn xml_escape(s: &str) -> std::borrow::Cow<'_, str> {
    if !s.bytes().any(needs_escape) {
        return std::borrow::Cow::Borrowed(s);
    }
    let mut out = String::with_capacity(s.len() + 16);
    for c in s.chars() {
        match c {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&apos;"),
            // Strip XML-illegal C0 controls. `\t` (0x09), `\n` (0x0A),
            // and `\r` (0x0D) are the only sub-space characters XML
            // 1.0 permits, so they pass through.
            c if is_illegal_xml_char(c) => {}
            other => out.push(other),
        }
    }
    std::borrow::Cow::Owned(out)
}

/// Returns true if `b` should trigger a copy in `xml_escape`: either
/// a metacharacter that needs entity-escaping, or an XML-illegal C0
/// control byte that needs stripping.
fn needs_escape(b: u8) -> bool {
    matches!(b, b'&' | b'<' | b'>' | b'"' | b'\'')
        || (b < 0x20 && !matches!(b, b'\t' | b'\n' | b'\r'))
}

/// Returns true for chars that XML 1.0 does not allow in document
/// text. Only the C0 controls are relevant for our inputs (UTF-8
/// bucket / key strings); the high-byte XML-illegal set (`\u{FFFE}`,
/// `\u{FFFF}`, surrogates) is not reachable because Rust `&str` is
/// already well-formed UTF-8.
fn is_illegal_xml_char(c: char) -> bool {
    (c as u32) < 0x20 && !matches!(c, '\t' | '\n' | '\r')
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::catalog::ObjectMeta;
    use bytes::Bytes;
    use futures::stream;
    use unbounded_storage::bufferpool::StripeKey;

    fn dummy_meta() -> ObjectMeta {
        ObjectMeta {
            stripe: StripeKey([0u8; 32]),
            size: 1000,
            etag: "\"0000000000000000\"".into(),
            content_type: "text/plain".into(),
            last_modified: EPOCH_IMF.into(),
        }
    }

    #[test]
    fn s3_error_xml_valid() {
        let xml = s3_error("NoSuchKey", "not found");
        let s = String::from_utf8(xml.to_vec()).unwrap();
        assert!(s.contains("<Code>NoSuchKey</Code>"));
        assert!(s.contains("<Message>not found</Message>"));
    }

    #[test]
    fn xml_escape_no_special_chars_borrows() {
        // No allocation when the input is XML-safe.
        let s = xml_escape("plain text 1.0");
        assert!(matches!(s, std::borrow::Cow::Borrowed(_)));
    }

    #[test]
    fn xml_escape_replaces_all_specials() {
        assert_eq!(
            xml_escape("a<b>c&d\"e'f").as_ref(),
            "a&lt;b&gt;c&amp;d&quot;e&apos;f",
        );
    }

    #[test]
    fn xml_escape_strips_illegal_c0_controls() {
        // \x00, \x01, \x02, ... must be silently dropped because XML 1.0
        // has no representation for them.
        assert_eq!(xml_escape("a\x01b\x02c").as_ref(), "abc");
        assert_eq!(xml_escape("\x00\x01\x1f").as_ref(), "");
        // The three legal whitespace controls survive.
        assert_eq!(xml_escape("a\tb\nc\rd").as_ref(), "a\tb\nc\rd");
        assert!(matches!(
            xml_escape("a\tb\nc\rd"),
            std::borrow::Cow::Borrowed(_),
        ));
        // Stripping interacts correctly with metachar escaping.
        assert_eq!(xml_escape("a\x01<b>").as_ref(), "a&lt;b&gt;");
    }

    #[test]
    fn not_found_with_control_chars_is_well_formed() {
        // A key with embedded control characters must still produce
        // an XML body that strict parsers accept.
        let resp = not_found("b", "key\x01with\x02ctrl");
        let body = futures::executor::block_on(async {
            axum::body::to_bytes(resp.into_body(), 4096).await.unwrap()
        });
        let s = std::str::from_utf8(&body).unwrap();
        // Control chars were stripped; the surrounding text remains.
        assert!(s.contains("keywithctrl"), "body: {s}");
        // quick-xml parses the body without error.
        let mut reader = quick_xml::Reader::from_str(s);
        loop {
            match reader.read_event() {
                Ok(quick_xml::events::Event::Eof) => break,
                Ok(_) => continue,
                Err(e) => panic!("malformed XML: {e}: {s}"),
            }
        }
    }

    #[tokio::test]
    async fn not_found_escapes_bucket_and_key() {
        // The body must contain the escaped form and must NOT contain
        // the raw substring; otherwise XML callers would see malformed
        // markup for keys containing `<` / `&` / `>`.
        let resp = not_found("b", "weird<key&name>");
        assert_eq!(resp.status(), StatusCode::NOT_FOUND);
        let body = axum::body::to_bytes(resp.into_body(), 4096).await.unwrap();
        let s = std::str::from_utf8(&body).unwrap();
        assert!(
            s.contains("weird&lt;key&amp;name&gt;"),
            "missing escaped form in body: {s}",
        );
        assert!(
            !s.contains("weird<key&name>"),
            "raw unescaped substring leaked into body: {s}",
        );
    }

    #[test]
    fn s3_error_with_specials_is_well_formed() {
        // A key with `<`, `>`, `&` must not break XML well-formedness.
        let xml = s3_error("NoSuchKey", "key: weird<key&name>");
        let s = String::from_utf8(xml.to_vec()).unwrap();
        assert!(s.contains("&lt;key&amp;name&gt;"));
        assert!(!s.contains("<key&name>"));

        // Parse the body with quick-xml as a stricter well-formedness
        // check than substring matching.
        let mut reader = quick_xml::Reader::from_str(&s);
        loop {
            match reader.read_event() {
                Ok(quick_xml::events::Event::Eof) => break,
                Ok(_) => continue,
                Err(e) => panic!("malformed XML: {e}: {s}"),
            }
        }
    }

    #[tokio::test]
    async fn full_response_has_correct_headers() {
        let meta = dummy_meta();
        let stream: BoxStream<'static, Result<Bytes, ObjectError>> =
            stream::once(async { Ok(Bytes::from_static(b"hello")) }).boxed();
        let resp = full_response(meta, stream);
        assert_eq!(resp.status(), StatusCode::OK);
        assert_eq!(
            resp.headers().get(header::CONTENT_LENGTH).unwrap(),
            "1000",
        );
        assert_eq!(
            resp.headers().get(header::CONTENT_TYPE).unwrap(),
            "text/plain",
        );
        assert!(resp.headers().get(header::ACCEPT_RANGES).is_some());
    }

    #[tokio::test]
    async fn partial_response_has_content_range() {
        let meta = dummy_meta();
        let stream: BoxStream<'static, Result<Bytes, ObjectError>> =
            stream::once(async { Ok(Bytes::from_static(b"hello")) }).boxed();
        let range = crate::server::range::ByteRange { offset: 10, len: 20 };
        let resp = partial_response(meta, stream, range);
        assert_eq!(resp.status(), StatusCode::PARTIAL_CONTENT);
        assert_eq!(
            resp.headers().get(header::CONTENT_RANGE).unwrap(),
            "bytes 10-29/1000",
        );
        assert_eq!(
            resp.headers().get(header::CONTENT_LENGTH).unwrap(),
            "20",
        );
    }
}
