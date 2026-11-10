//! `DELETE /v1/objects/{key}` on the client socket: drop one cached version.
//!
//! The request names the version with a required strong `If-Match` tag. It has
//! no body, credentials, or origin context. A processed request is answered
//! `204 No Content` and the connection closes, whether or not anything was
//! cached. Errors reuse the object error heads.

use crate::http::{Header, MessageHead, StartLine, is_token, trim_ows};
use crate::object::model::{CacheKey, MAX_FIELD_BYTES, StrongEtag};
use crate::object::{Error, Result};

/// Object path prefix shared with reads.
const OBJECTS_PATH: &str = "/v1/objects/";

/// A validated invalidation request.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct InvalidateRequest {
    pub key: CacheKey,

    pub etag: StrongEtag,
}

/// Whether a request head selects the invalidation route: `DELETE` on an
/// object path. Other methods keep the read parser and its errors.
pub fn is_invalidate(head: &MessageHead) -> bool {
    matches!(
        &head.start,
        StartLine::Request { method, target }
            if method == "DELETE" && target.starts_with(OBJECTS_PATH)
    )
}

/// Parse a `DELETE /v1/objects/{key}` head. Raw HTTP framing must be checked
/// before calling this. `header_limit` caps the decoded head.
pub fn parse(head: &MessageHead, header_limit: usize) -> Result<InvalidateRequest> {
    let StartLine::Request { method, target } = &head.start else {
        return Err(Error::InvalidRequest);
    };
    if method != "DELETE" {
        return Err(Error::MethodNotAllowed);
    }
    let key_text = target
        .strip_prefix(OBJECTS_PATH)
        .ok_or(Error::InvalidRequest)?;
    let mut decoded = method.len().saturating_add(target.len());
    let mut host = None;
    let mut etag = None;
    for header in &head.headers {
        decoded = decoded
            .saturating_add(header.name.len())
            .saturating_add(header.value.len());
        if decoded > header_limit {
            return Err(Error::HeaderTooLarge);
        }
        if header.name.is_empty()
            || !header.name.bytes().all(is_token)
            || header
                .value
                .iter()
                .any(|&b| b == 0x7f || (b < 0x20 && b != b'\t'))
        {
            return Err(Error::InvalidRequest);
        }
        let value = header.value.as_slice();
        match header.name.to_ascii_lowercase().as_str() {
            "host" => {
                if host.replace(trim_ows(value) == b"racer").is_some() {
                    return Err(Error::InvalidRequest);
                }
            }
            "if-match" => {
                if value.len() > MAX_FIELD_BYTES {
                    return Err(Error::HeaderTooLarge);
                }
                if etag.replace(StrongEtag::parse(value)?).is_some() {
                    return Err(Error::InvalidRequest);
                }
            }
            "content-length" if value == b"0" => {}
            // No body, ranges, credentials, or other preconditions.
            "content-length"
            | "transfer-encoding"
            | "content-encoding"
            | "trailer"
            | "upgrade"
            | "expect"
            | "range"
            | "if-none-match"
            | "if-modified-since"
            | "if-unmodified-since"
            | "if-range"
            | "authorization"
            | "racer-metadata" => return Err(Error::InvalidRequest),
            _ => {}
        }
    }
    if host != Some(true) {
        return Err(Error::InvalidRequest);
    }
    let key = CacheKey::parse_hex(key_text.as_bytes())?;
    let etag = etag.ok_or(Error::InvalidRequest)?;
    Ok(InvalidateRequest { key, etag })
}

/// The success head: `204 No Content`, then the connection closes.
pub fn success_head() -> MessageHead {
    MessageHead {
        start: StartLine::Response { status: 204 },
        headers: vec![Header::new("Connection", "close")],
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const KEY: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    fn head(method: &str, target: &str, headers: &[(&str, &str)]) -> MessageHead {
        MessageHead {
            start: StartLine::Request {
                method: method.into(),
                target: target.into(),
            },
            headers: headers.iter().map(|(n, v)| Header::new(*n, *v)).collect(),
        }
    }

    fn target() -> String {
        format!("{OBJECTS_PATH}{KEY}")
    }

    #[test]
    fn parses_key_and_etag() {
        let h = head(
            "DELETE",
            &target(),
            &[
                ("Host", "racer"),
                ("If-Match", "\"v1\""),
                ("Content-Length", "0"),
            ],
        );
        assert!(is_invalidate(&h));
        let request = parse(&h, 4096).unwrap();
        assert_eq!(request.key.to_hex(), KEY);
        assert_eq!(request.etag.as_str(), "\"v1\"");
    }

    #[test]
    fn selects_only_delete_on_objects() {
        assert!(!is_invalidate(&head("HEAD", &target(), &[])));
        assert!(!is_invalidate(&head("DELETE", "/v1/prefetches/a", &[])));
    }

    #[test]
    fn rejects_invalid_requests() {
        let ok = [("Host", "racer"), ("If-Match", "\"v1\"")];
        let cases: Vec<(MessageHead, Error)> = vec![
            (
                head("DELETE", &target(), &[("Host", "racer")]),
                Error::InvalidRequest,
            ),
            (
                head("DELETE", &target(), &[("If-Match", "\"v1\"")]),
                Error::InvalidRequest,
            ),
            (
                head(
                    "DELETE",
                    &target(),
                    &[("Host", "racer"), ("If-Match", "W/\"v1\"")],
                ),
                Error::InvalidRequest,
            ),
            (
                head("DELETE", &target(), &[("Host", "racer"), ("If-Match", "*")]),
                Error::InvalidRequest,
            ),
            (
                head(
                    "DELETE",
                    &target(),
                    &[
                        ("Host", "racer"),
                        ("If-Match", "\"a\""),
                        ("If-Match", "\"a\""),
                    ],
                ),
                Error::InvalidRequest,
            ),
            (
                head("DELETE", &format!("{OBJECTS_PATH}{}", &KEY[1..]), &ok),
                Error::InvalidRequest,
            ),
            (
                head(
                    "DELETE",
                    &format!("{OBJECTS_PATH}{}", KEY.to_uppercase()),
                    &ok,
                ),
                Error::InvalidRequest,
            ),
            (
                head("DELETE", &format!("{}/host-read", target()), &ok),
                Error::InvalidRequest,
            ),
            (
                head(
                    "DELETE",
                    &target(),
                    &[
                        ("Host", "racer"),
                        ("If-Match", "\"v1\""),
                        ("Content-Length", "1"),
                    ],
                ),
                Error::InvalidRequest,
            ),
            (
                head(
                    "DELETE",
                    &target(),
                    &[
                        ("Host", "racer"),
                        ("If-Match", "\"v1\""),
                        ("Authorization", "x"),
                    ],
                ),
                Error::InvalidRequest,
            ),
            (head("POST", &target(), &ok), Error::MethodNotAllowed),
        ];
        for (h, want) in cases {
            assert_eq!(parse(&h, 4096).err(), Some(want));
        }
        assert_eq!(
            parse(&head("DELETE", &target(), &ok), 10).err(),
            Some(Error::HeaderTooLarge)
        );
    }

    #[test]
    fn success_is_no_content_and_closes() {
        let h = success_head();
        assert!(matches!(h.start, StartLine::Response { status: 204 }));
        assert_eq!(h.unique("connection").unwrap(), Some(&b"close"[..]));
        assert_eq!(h.unique("content-length").unwrap(), None);
    }
}
