//! Validate HEAD/bootstrap/pinned GET and preserve opaque adapter fields.
//!
//! Wire values follow pkg/racersdk: canonical lowercase keys, quoted strong pins,
//! signed-63-bit decimal ranges, and byte-preserving opaque context.
use crate::{
    error::{Error, Result},
    http::codec::{MessageHead, StartLine},
    model::{
        context::{Authorization, OpaqueMetadata, OriginContext},
        identity::{CacheId, CacheKey, ObjectId, StrongEtag},
        range::{ByteRange, PAGE_BYTES},
    },
};
use std::collections::HashSet;

pub const MAX_HEAD_BYTES: usize = 32 * 1024;
pub const MAX_FIELD_BYTES: usize = 8192;

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ReadKind {
    Head,
    HeadPinned { etag: StrongEtag },
    Bootstrap,
    Pinned { etag: StrongEtag, range: ByteRange },
}

impl ReadKind {
    pub fn is_head(&self) -> bool {
        matches!(self, Self::Head | Self::HeadPinned { .. })
    }

    pub fn pin(&self) -> Option<&StrongEtag> {
        match self {
            Self::HeadPinned { etag } | Self::Pinned { etag, .. } => Some(etag),
            _ => None,
        }
    }
}

pub struct ClientRequest {
    pub kind: ReadKind,
    pub origin: OriginContext,
}

/// Endpoint-only distinctions without putting raw request data in shared errors.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RequestError {
    Invalid(Error),
    HeaderLimit,
    MethodNotAllowed,
}

impl From<Error> for RequestError {
    fn from(error: Error) -> Self {
        Self::Invalid(error)
    }
}

#[derive(Clone)]
pub struct RequestParser {
    header_limit: usize,
}
impl RequestParser {
    pub fn new(header_limit: usize) -> Self {
        Self {
            header_limit: header_limit.min(MAX_HEAD_BYTES),
        }
    }
    /// Apply this cap to raw HTTP framing before calling the semantic parser.
    pub(crate) fn header_limit(&self) -> usize {
        self.header_limit
    }
    pub fn parse(&self, cache: &CacheId, head: MessageHead) -> Result<ClientRequest> {
        self.parse_detailed(cache, head)
            .map_err(|error| match error {
                RequestError::Invalid(error) => error,
                RequestError::HeaderLimit => Error::HeaderTooLarge,
                RequestError::MethodNotAllowed => Error::MethodNotAllowed,
            })
    }

    /// Codec must validate the raw head and its byte limit before calling this.
    /// In particular, context fields must have exactly one separator space and
    /// must not have their value trimmed by the codec. Decoded fields cannot
    /// reconstruct wire length: unknown fields need not have a separator SP.
    pub fn parse_detailed(
        &self,
        cache: &CacheId,
        head: MessageHead,
    ) -> std::result::Result<ClientRequest, RequestError> {
        let StartLine::Request { method, target } = head.start else {
            return Err(Error::InvalidRequest.into());
        };
        // Bound manually constructed input data as well, without pretending this
        // is a wire-length check. Framing owns start-line/colon/OWS/CRLF accounting.
        let mut decoded_bytes = method.len().saturating_add(target.len());
        let mut seen = HashSet::new();
        let mut host = None;
        let mut pin = None;
        let mut range = None;
        let mut metadata = None;
        let mut authorization = None;
        for header in head.headers {
            decoded_bytes = decoded_bytes
                .saturating_add(header.name.len())
                .saturating_add(header.value.len());
            if decoded_bytes > self.header_limit {
                return Err(RequestError::HeaderLimit);
            }
            if header.name.is_empty()
                || !header.name.bytes().all(header_token)
                || header
                    .value
                    .iter()
                    .any(|&b| b == 0x7f || (b < 0x20 && b != b'\t'))
            {
                return Err(Error::InvalidRequest.into());
            }
            let name = header.name.to_ascii_lowercase();
            let value = header.value.as_slice();
            if matches!(
                name.as_str(),
                "host"
                    | "content-length"
                    | "content-type"
                    | "content-range"
                    | "etag"
                    | "if-match"
                    | "range"
                    | "racer-expires-at"
                    | "racer-metadata"
                    | "authorization"
            ) && !seen.insert(name.clone())
            {
                return Err(Error::InvalidRequest.into());
            }
            match name.as_str() {
                "host" => host = Some(value == b"racer"),
                "content-length" if value == b"0" => {}
                "content-length"
                | "content-range"
                | "etag"
                | "racer-expires-at"
                | "transfer-encoding"
                | "content-encoding"
                | "trailer"
                | "upgrade"
                | "expect"
                | "if-none-match"
                | "if-modified-since"
                | "if-unmodified-since"
                | "if-range" => return Err(Error::InvalidRequest.into()),
                "connection"
                    if value
                        .split(|&b| b == b',')
                        .any(|token| trim_ows(token).eq_ignore_ascii_case(b"upgrade")) =>
                {
                    return Err(Error::InvalidRequest.into());
                }
                "if-match" => {
                    if value.len() > MAX_FIELD_BYTES {
                        return Err(RequestError::HeaderLimit);
                    }
                    pin = Some(StrongEtag::parse(value)?);
                }
                "range" => range = Some(ByteRange::parse(value)?),
                "racer-metadata" => {
                    validate_opaque(value)?;
                    metadata = Some(OpaqueMetadata::from_header(value)?);
                }
                "authorization" => {
                    validate_opaque(value)?;
                    authorization = Some(Authorization::from_header(value)?);
                }
                _ => {}
            }
        }
        if decoded_bytes > self.header_limit {
            return Err(RequestError::HeaderLimit);
        }
        if host != Some(true) {
            return Err(Error::InvalidRequest.into());
        }
        let key = parse_key(&target)?;
        let kind = match method.as_str() {
            "HEAD" if range.is_none() => match pin {
                Some(etag) => ReadKind::HeadPinned { etag },
                None => ReadKind::Head,
            },
            "HEAD" => return Err(Error::InvalidRequest.into()),
            "GET" => match (pin, range) {
                (Some(etag), Some(range)) => ReadKind::Pinned { etag, range },
                (None, Some(ByteRange::Closed { first: 0, last })) if last == PAGE_BYTES - 1 => {
                    ReadKind::Bootstrap
                }
                _ => return Err(Error::InvalidRequest.into()),
            },
            _ => return Err(RequestError::MethodNotAllowed),
        };
        Ok(ClientRequest {
            kind,
            origin: OriginContext {
                object: ObjectId {
                    cache: cache.clone(),
                    key,
                },
                metadata,
                authorization,
            },
        })
    }
}

fn parse_key(target: &str) -> Result<CacheKey> {
    CacheKey::parse_hex(
        target
            .strip_prefix("/v1/objects/")
            .ok_or(Error::InvalidRequest)?
            .as_bytes(),
    )
}

fn validate_opaque(value: &[u8]) -> std::result::Result<(), RequestError> {
    if value.len() > MAX_FIELD_BYTES {
        return Err(RequestError::HeaderLimit);
    }
    if value.is_empty()
        || value.first() == Some(&b' ')
        || value.last() == Some(&b' ')
        || value.iter().any(|&b| b < 0x20 || b == 0x7f)
    {
        return Err(Error::InvalidRequest.into());
    }
    Ok(())
}

fn header_token(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b)
}

fn trim_ows(mut value: &[u8]) -> &[u8] {
    while matches!(value.first(), Some(b' ' | b'\t')) {
        value = &value[1..];
    }
    while matches!(value.last(), Some(b' ' | b'\t')) {
        value = &value[..value.len() - 1];
    }
    value
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::http::codec::{Codec, Header};

    fn head(method: &str, fields: &[(&str, &[u8])]) -> MessageHead {
        MessageHead {
            start: StartLine::Request {
                method: method.into(),
                target: format!("/v1/objects/{}", "01".repeat(32)),
            },
            headers: std::iter::once(Header {
                name: "Host".into(),
                value: b"racer".to_vec(),
            })
            .chain(fields.iter().map(|(name, value)| Header {
                name: (*name).into(),
                value: value.to_vec(),
            }))
            .collect(),
        }
    }

    fn parse(head: MessageHead) -> std::result::Result<ClientRequest, RequestError> {
        RequestParser::new(MAX_HEAD_BYTES).parse_detailed(&CacheId("uid".into()), head)
    }

    #[test]
    fn sdk_methods_ranges_pins_and_opaque_bytes() {
        for (method, fields, expected) in [
            ("HEAD", vec![], ReadKind::Head),
            (
                "HEAD",
                vec![("If-Match", b"\"\"".as_slice())],
                ReadKind::HeadPinned {
                    etag: StrongEtag::parse(b"\"\"").unwrap(),
                },
            ),
            (
                "GET",
                vec![("Range", b"bytes=0-16777215".as_slice())],
                ReadKind::Bootstrap,
            ),
            (
                "GET",
                vec![
                    ("Range", b"bytes=-0".as_slice()),
                    ("If-Match", b"\"a,b\\c\"".as_slice()),
                ],
                ReadKind::Pinned {
                    etag: StrongEtag::parse(b"\"a,b\\c\"").unwrap(),
                    range: ByteRange::Suffix(0),
                },
            ),
            (
                "GET",
                vec![
                    ("Range", b"bytes=16777216-".as_slice()),
                    ("If-Match", b"\"v\"".as_slice()),
                ],
                ReadKind::Pinned {
                    etag: StrongEtag::parse(b"\"v\"").unwrap(),
                    range: ByteRange::From(PAGE_BYTES),
                },
            ),
        ] {
            let mut request = head(method, &fields);
            request.headers.push(Header {
                name: "Racer-Metadata".into(),
                value: b"opaque,\xff value".to_vec(),
            });
            request.headers.push(Header {
                name: "Authorization".into(),
                value: b"Scheme opaque \xfe".to_vec(),
            });
            let request = parse(request).unwrap();
            assert_eq!(request.kind, expected);
            assert_eq!(request.origin.object.key, CacheKey([1; 32]));
            assert_eq!(
                request.origin.metadata.unwrap().as_header().unwrap(),
                b"opaque,\xff value"
            );
            assert_eq!(
                request
                    .origin
                    .authorization
                    .unwrap()
                    .expose_for_origin()
                    .unwrap(),
                b"Scheme opaque \xfe"
            );
        }
    }

    #[test]
    fn rejects_noncanonical_targets_and_envelopes() {
        for target in [
            format!("/v1/objects/{}?", "0".repeat(64)),
            format!("/v1/objects/{}", "A".repeat(64)),
            format!("http://racer/v1/objects/{}", "0".repeat(64)),
            "/v1/objects/%30".into(),
        ] {
            let mut request = head("HEAD", &[]);
            request.start = StartLine::Request {
                method: "HEAD".into(),
                target,
            };
            assert!(parse(request).is_err());
        }
        for (name, value) in [
            ("Host", b"racer".as_slice()),
            ("Content-Length", b"00"),
            ("Expect", b"100-continue"),
            ("Transfer-Encoding", b"identity"),
            ("Content-Encoding", b"identity"),
            ("If-Range", b"\"v\""),
            ("Connection", b"keep-alive, Upgrade"),
            ("Range", b"bytes=0-0"),
        ] {
            assert!(parse(head("HEAD", &[(name, value)])).is_err());
        }
        assert!(matches!(
            parse(head("POST", &[])),
            Err(RequestError::MethodNotAllowed)
        ));
        let mut request = head("HEAD", &[]);
        request.headers.clear();
        assert!(parse(request).is_err());
    }

    #[test]
    fn rejects_ambiguous_pins_and_ranges() {
        for pin in [
            b"*".as_slice(),
            b"W/\"v\"",
            b"\"a\", \"b\"",
            b"",
            b"\"space value\"",
        ] {
            assert!(parse(head("HEAD", &[("If-Match", pin)])).is_err());
        }
        for range in [
            b"bytes=00-1".as_slice(),
            b"bytes=1-0",
            b"bytes=0-1,2-3",
            b"bytes=9223372036854775808-",
            b"bytes=-",
            b"bytes=+1-2",
            b"bytes=0--1",
        ] {
            assert!(parse(head("GET", &[("Range", range), ("If-Match", b"\"v\"")])).is_err());
        }
        for range in [b"bytes=0-1".as_slice(), b"bytes=0-", b"bytes=-1"] {
            assert!(parse(head("GET", &[("Range", range)])).is_err());
        }
        assert!(parse(head("GET", &[])).is_err());
    }

    #[test]
    fn context_duplicates_empty_and_limits() {
        for name in ["Authorization", "Racer-Metadata"] {
            for value in [
                b"".as_slice(),
                b" leading",
                b"trailing ",
                b"a\tb",
                b"a\x7fb",
                b"a\rb",
            ] {
                assert!(parse(head("HEAD", &[(name, value)])).is_err());
            }
            assert!(
                parse(head(
                    "HEAD",
                    &[(name, b"a"), (&name.to_ascii_lowercase(), b"a")]
                ))
                .is_err()
            );
            assert!(parse(head("HEAD", &[(name, &vec![b'a'; MAX_FIELD_BYTES])])).is_ok());
            assert!(matches!(
                parse(head("HEAD", &[(name, &vec![b'a'; MAX_FIELD_BYTES + 1])])),
                Err(RequestError::HeaderLimit)
            ));
        }
        let request = head("HEAD", &[("X", &vec![b'x'; MAX_HEAD_BYTES])]);
        assert!(matches!(parse(request), Err(RequestError::HeaderLimit)));
    }

    #[test]
    fn sdk_total_head_limit_includes_unknown_fields() {
        let prefix = format!(
            "HEAD /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nX-Padding: ",
            "0".repeat(64)
        );
        let mut raw = prefix.into_bytes();
        raw.resize(MAX_HEAD_BYTES - 4, b'x');
        raw.extend_from_slice(b"\r\n\r\n");
        let codec = Codec::new(MAX_HEAD_BYTES, 0);
        let (head, consumed) = codec.decode_head(&raw).unwrap().unwrap();
        assert_eq!(consumed, MAX_HEAD_BYTES);
        assert!(parse(head).is_ok());
        raw.insert(raw.len() - 4, b'x');
        assert!(codec.decode_head(&raw).is_err());
    }

    #[test]
    fn raw_head_limit_is_independent_of_unknown_field_whitespace() {
        let codec = Codec::new(MAX_HEAD_BYTES, 0);
        for separator in ["", " ", "\t", "  ", " \t"] {
            for trailing in ["", " ", "\t"] {
                for length in [MAX_HEAD_BYTES - 1, MAX_HEAD_BYTES, MAX_HEAD_BYTES + 1] {
                    let prefix = format!(
                        "HEAD /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nX-Empty:\r\nX-Padding:{separator}",
                        "0".repeat(64)
                    );
                    let mut raw = prefix.into_bytes();
                    raw.resize(length - trailing.len() - 4, b'x');
                    raw.extend_from_slice(trailing.as_bytes());
                    raw.extend_from_slice(b"\r\n\r\n");
                    if length > MAX_HEAD_BYTES {
                        assert!(matches!(
                            codec.decode_head(&raw),
                            Err(Error::HeaderTooLarge)
                        ));
                    } else {
                        let (head, consumed) = codec.decode_head(&raw).unwrap().unwrap();
                        assert_eq!(consumed, length);
                        assert!(
                            parse(head).is_ok(),
                            "separator={separator:?}, trailing={trailing:?}"
                        );
                    }
                }
            }
        }
    }

    #[test]
    fn sdk_raw_head_preserves_non_utf8_and_rejects_normalization() {
        let codec = Codec::new(MAX_HEAD_BYTES, 0);
        let mut raw = format!(
            "HEAD /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nRacer-Metadata: opaque,",
            "0".repeat(64)
        )
        .into_bytes();
        raw.extend_from_slice(b"\xff value\r\nAuthorization: Bearer secret\r\n\r\n");
        let (head, used) = codec.decode_head(&raw).unwrap().unwrap();
        assert_eq!(used, raw.len());
        let parsed = parse(head).unwrap();
        assert_eq!(
            parsed.origin.metadata.unwrap().as_header().unwrap(),
            b"opaque,\xff value"
        );
        for value in [
            "",
            "secret",
            "  secret",
            " secret ",
            "\tsecret",
            " secret\t",
        ] {
            let raw = format!(
                "HEAD /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nAuthorization:{value}\r\n\r\n",
                "0".repeat(64)
            );
            let rejected = match codec.decode_head(raw.as_bytes()) {
                Err(_) => true,
                Ok(Some((head, _))) => parse(head).is_err(),
                Ok(None) => false,
            };
            assert!(rejected, "opaque separator/OWS was normalized");
        }
    }

    #[test]
    fn raw_head_limit_does_not_assume_unknown_header_whitespace() {
        for separator in ["", " ", "\t", "  ", " \t"] {
            for trailing in ["", " ", "\t"] {
                for limit in [512, MAX_HEAD_BYTES] {
                    let parser = RequestParser::new(limit);
                    let codec = Codec::new(parser.header_limit(), 0);
                    let prefix = format!(
                        "HEAD /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nX:{separator}",
                        "0".repeat(64)
                    );
                    let suffix = format!("{trailing}\r\nY:\r\n\r\n");
                    for length in [limit - 1, limit, limit + 1] {
                        let raw = format!(
                            "{prefix}{}{suffix}",
                            "x".repeat(length - prefix.len() - suffix.len())
                        );
                        assert_eq!(raw.len(), length);
                        let decoded = codec.decode_head(raw.as_bytes());
                        if length > limit {
                            assert!(matches!(decoded, Err(Error::HeaderTooLarge)));
                        } else {
                            let (head, consumed) = decoded.unwrap().unwrap();
                            assert_eq!(consumed, length);
                            assert!(parser.parse(&CacheId("uid".into()), head).is_ok());
                        }
                    }
                }
            }
        }
    }
}
