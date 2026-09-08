// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Native S3 `<Error>` XML serialization for the S3 frontend.
//!
//! S3 clients expect error responses to carry an XML body with a
//! well-known `<Code>` rather than a plain HTTP status line, so this
//! file owns the small, pure, cross-platform serializer the S3 serving
//! path uses for every error it emits. It knows nothing about sockets,
//! ranges, or stripe geometry: it maps an [`S3ErrorCode`] to its HTTP
//! status and `<Code>` string and renders the canonical error document.
//!
//! The set of codes is deliberately closed to exactly the errors the
//! frontend can produce; adding a new failure mode means adding a
//! variant here so its status and code string stay in one place.

/// The closed set of S3 error codes the S3 frontend can emit. Each
/// variant carries (implicitly, via its accessors) the HTTP status and
/// the S3 `<Code>` string a client matches on.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum S3ErrorCode {
    /// 404: the requested object key does not exist at the origin.
    NoSuchKey,
    /// 416: the requested byte range lies wholly outside the object.
    InvalidRange,
    /// 405: the HTTP method is not GET or HEAD.
    MethodNotAllowed,
    /// 400: the request line/headers were malformed or otherwise
    /// unusable.
    InvalidRequest,
    /// 500: an internal error reading the object metadata or body.
    InternalError,
}

impl S3ErrorCode {
    /// The HTTP status code this error maps to.
    pub fn http_status_u16(&self) -> u16 {
        match self {
            S3ErrorCode::NoSuchKey => 404,
            S3ErrorCode::InvalidRange => 416,
            S3ErrorCode::MethodNotAllowed => 405,
            S3ErrorCode::InvalidRequest => 400,
            S3ErrorCode::InternalError => 500,
        }
    }

    /// The S3 `<Code>` token clients switch on. ASCII-only, so it never
    /// needs XML escaping.
    pub fn code_str(&self) -> &'static str {
        match self {
            S3ErrorCode::NoSuchKey => "NoSuchKey",
            S3ErrorCode::InvalidRange => "InvalidRange",
            S3ErrorCode::MethodNotAllowed => "MethodNotAllowed",
            S3ErrorCode::InvalidRequest => "InvalidRequest",
            S3ErrorCode::InternalError => "InternalError",
        }
    }

    /// The human-readable `<Message>` text S3 pairs with this code.
    pub fn message(&self) -> &'static str {
        match self {
            S3ErrorCode::NoSuchKey => "The specified key does not exist.",
            S3ErrorCode::InvalidRange => "The requested range is not satisfiable",
            S3ErrorCode::MethodNotAllowed => {
                "The specified method is not allowed against this resource."
            }
            S3ErrorCode::InvalidRequest => "Invalid request.",
            S3ErrorCode::InternalError => "We encountered an internal error. Please try again.",
        }
    }
}

/// Render the canonical S3 `<Error>` document for `code`, naming the
/// `resource` (the request path/key) and a `request_id`.
///
/// The output is exactly:
///
/// ```text
/// <?xml version="1.0" encoding="UTF-8"?>
/// <Error><Code>..</Code><Message>..</Message><Resource>..</Resource><RequestId>..</RequestId></Error>
/// ```
///
/// with no trailing newline. The dynamic `message`, `resource`, and
/// `request_id` text is XML-escaped; the `<Code>` token is a fixed
/// ASCII string and is emitted verbatim.
pub fn error_xml(code: S3ErrorCode, resource: &str, request_id: &str) -> String {
    format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
<Error><Code>{}</Code><Message>{}</Message><Resource>{}</Resource><RequestId>{}</RequestId></Error>",
        code.code_str(),
        xml_escape(code.message()),
        xml_escape(resource),
        xml_escape(request_id),
    )
}

/// Escape the five XML metacharacters (`&`, `<`, `>`, `"`, `'`) in
/// `s` so arbitrary object keys can be embedded in a `<Resource>` or
/// `<Message>` element without breaking the document. `&` is replaced
/// first by construction (each replacement only introduces `&...;`
/// entities, never a bare metacharacter).
pub fn xml_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for ch in s.chars() {
        match ch {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&apos;"),
            other => out.push(other),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn status_and_code_strings() {
        assert_eq!(S3ErrorCode::NoSuchKey.http_status_u16(), 404);
        assert_eq!(S3ErrorCode::NoSuchKey.code_str(), "NoSuchKey");
        assert_eq!(S3ErrorCode::InvalidRange.http_status_u16(), 416);
        assert_eq!(S3ErrorCode::InvalidRange.code_str(), "InvalidRange");
        assert_eq!(S3ErrorCode::MethodNotAllowed.http_status_u16(), 405);
        assert_eq!(S3ErrorCode::MethodNotAllowed.code_str(), "MethodNotAllowed");
        assert_eq!(S3ErrorCode::InvalidRequest.http_status_u16(), 400);
        assert_eq!(S3ErrorCode::InvalidRequest.code_str(), "InvalidRequest");
        assert_eq!(S3ErrorCode::InternalError.http_status_u16(), 500);
        assert_eq!(S3ErrorCode::InternalError.code_str(), "InternalError");
    }

    #[test]
    fn no_such_key_exact_bytes() {
        let xml = error_xml(S3ErrorCode::NoSuchKey, "/bucket/obj", "req-1");
        assert_eq!(
            xml,
            "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message>\
<Resource>/bucket/obj</Resource><RequestId>req-1</RequestId></Error>"
        );
    }

    #[test]
    fn invalid_range_exact_bytes() {
        let xml = error_xml(S3ErrorCode::InvalidRange, "/k", "abc");
        assert_eq!(
            xml,
            "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
<Error><Code>InvalidRange</Code><Message>The requested range is not satisfiable</Message>\
<Resource>/k</Resource><RequestId>abc</RequestId></Error>"
        );
    }

    #[test]
    fn escapes_all_metacharacters_in_resource() {
        let xml = error_xml(S3ErrorCode::NoSuchKey, "/a&b<c>d\"e'f", "id");
        assert!(
            xml.contains("<Resource>/a&amp;b&lt;c&gt;d&quot;e&apos;f</Resource>"),
            "got: {xml}"
        );
        // No raw metacharacter leaks into the resource segment.
        let start = xml.find("<Resource>").unwrap() + "<Resource>".len();
        let end = xml.find("</Resource>").unwrap();
        let seg = &xml[start..end];
        assert!(!seg.contains('<'), "got: {seg}");
        assert!(!seg.contains('>'), "got: {seg}");
        assert!(!seg.contains('"'), "got: {seg}");
        assert!(!seg.contains('\''), "got: {seg}");
    }

    #[test]
    fn xml_escape_leaves_plain_text_untouched() {
        assert_eq!(xml_escape("/plain/key-123"), "/plain/key-123");
        assert_eq!(xml_escape(""), "");
    }
}
