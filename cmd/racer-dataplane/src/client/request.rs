//! Validate v2 subscriptions and HEAD metadata while preserving opaque fields.
//!
//! Wire values follow pkg/racersdk: canonical lowercase keys, quoted strong pins,
//! signed-63-bit decimal ranges, and byte-preserving opaque context.
use crate::runtime::collections::HashSet;
use crate::{
    error::{Error, Result},
    http::codec::{MessageHead, StartLine},
    model::{
        Authorization, ByteRange, CacheId, CacheKey, ObjectId, OpaqueMetadata, OriginContext,
        PAGE_BYTES, StrongEtag,
    },
};

pub const MAX_HEAD_BYTES: usize = 32 * 1024;
pub const MAX_FIELD_BYTES: usize = 8192;

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ReadKind {
    Head,
    HeadPinned {
        etag: StrongEtag,
    },
    Subscription {
        pin: Option<StrongEtag>,
        range: Option<ByteRange>,
        page_credits: usize,
        byte_credits: u64,
        ordered: bool,
    },
}

impl ReadKind {
    pub fn is_head(&self) -> bool {
        matches!(self, Self::Head | Self::HeadPinned { .. })
    }

    pub fn pin(&self) -> Option<&StrongEtag> {
        match self {
            Self::HeadPinned { etag } => Some(etag),
            Self::Subscription { pin, .. } => pin.as_ref(),
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
        let mut seen = HashSet::default();
        let mut host = None;
        let mut pin = None;
        let mut range = None;
        let mut metadata = None;
        let mut authorization = None;
        let mut content_length = false;
        let mut page_credits = 2;
        let mut byte_credits = 2 * PAGE_BYTES;
        let mut ordered = false;
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
                    | "racer-content-type"
                    | "racer-metadata"
                    | "authorization"
                    | "racer-page-credits"
                    | "racer-byte-credits"
                    | "racer-ordered"
            ) && !seen.insert(name.clone())
            {
                return Err(Error::InvalidRequest.into());
            }
            match name.as_str() {
                "host" => host = Some(value == b"racer"),
                "content-length" if value == b"0" => content_length = true,
                "content-length"
                | "content-range"
                | "etag"
                | "racer-expires-at"
                | "racer-content-type"
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
                "racer-page-credits" => page_credits = decimal(value, 1, 64)? as usize,
                "racer-byte-credits" => {
                    byte_credits = decimal(value, PAGE_BYTES, 64 * PAGE_BYTES)?;
                }
                "racer-ordered" => {
                    ordered = match value {
                        b"0" => false,
                        b"1" => true,
                        _ => return Err(Error::InvalidRequest.into()),
                    }
                }
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
        // HEAD remains available for internal metadata users on the old endpoint.
        let key = if method == "HEAD" {
            parse_key(&target, "/v1/objects/").or_else(|_| parse_key(&target, "/v2/objects/"))?
        } else {
            parse_key(&target, "/v2/objects/")?
        };
        let kind = match method.as_str() {
            "HEAD" if range.is_none() => match pin {
                Some(etag) => ReadKind::HeadPinned { etag },
                None => ReadKind::Head,
            },
            "HEAD" => return Err(Error::InvalidRequest.into()),
            "POST" if content_length => ReadKind::Subscription {
                pin,
                range,
                page_credits,
                byte_credits,
                ordered,
            },
            "POST" => return Err(Error::InvalidRequest.into()),
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

fn parse_key(target: &str, prefix: &str) -> Result<CacheKey> {
    CacheKey::parse_hex(
        target
            .strip_prefix(prefix)
            .ok_or(Error::InvalidRequest)?
            .as_bytes(),
    )
}

fn decimal(value: &[u8], minimum: u64, maximum: u64) -> Result<u64> {
    if value.is_empty() || value[0] == b'0' || !value.iter().all(u8::is_ascii_digit) {
        return Err(Error::InvalidRequest);
    }
    let number = value
        .iter()
        .try_fold(0u64, |n, digit| {
            n.checked_mul(10)?.checked_add(u64::from(digit - b'0'))
        })
        .ok_or(Error::InvalidRequest)?;
    if !(minimum..=maximum).contains(&number) {
        return Err(Error::InvalidRequest);
    }
    Ok(number)
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
                target: format!("/v2/objects/{}", "01".repeat(32)),
            },
            headers: std::iter::once(Header {
                name: "Host".into(),
                value: b"racer".to_vec(),
            })
            .chain(fields.iter().map(|(name, value)| Header {
                name: (*name).into(),
                value: value.to_vec(),
            }))
            .chain((method == "POST").then(|| Header {
                name: "Content-Length".into(),
                value: b"0".to_vec(),
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
                "POST",
                vec![("Range", b"bytes=0-16777215".as_slice())],
                ReadKind::Subscription {
                    pin: None,
                    range: Some(ByteRange::Closed {
                        first: 0,
                        last: PAGE_BYTES - 1,
                    }),
                    page_credits: 2,
                    byte_credits: 2 * PAGE_BYTES,
                    ordered: false,
                },
            ),
            (
                "POST",
                vec![
                    ("Range", b"bytes=-0".as_slice()),
                    ("If-Match", b"\"a,b\\c\"".as_slice()),
                ],
                ReadKind::Subscription {
                    pin: Some(StrongEtag::parse(b"\"a,b\\c\"").unwrap()),
                    range: Some(ByteRange::Suffix(0)),
                    page_credits: 2,
                    byte_credits: 2 * PAGE_BYTES,
                    ordered: false,
                },
            ),
            (
                "POST",
                vec![
                    ("Range", b"bytes=16777216-".as_slice()),
                    ("If-Match", b"\"v\"".as_slice()),
                ],
                ReadKind::Subscription {
                    pin: Some(StrongEtag::parse(b"\"v\"").unwrap()),
                    range: Some(ByteRange::From(PAGE_BYTES)),
                    page_credits: 2,
                    byte_credits: 2 * PAGE_BYTES,
                    ordered: false,
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
                request.origin.metadata.unwrap().as_header(),
                b"opaque,\xff value"
            );
            assert_eq!(
                request.origin.authorization.unwrap().expose_for_origin(),
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
            parse(head("GET", &[])),
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
            assert!(parse(head("POST", &[("Range", range), ("If-Match", b"\"v\"")])).is_err());
        }
        for range in [b"bytes=0-1".as_slice(), b"bytes=0-", b"bytes=-1"] {
            assert!(parse(head("POST", &[("Range", range)])).is_ok());
        }
        assert!(parse(head("GET", &[])).is_err());
    }

    #[test]
    fn subscription_credit_limits_defaults_and_required_zero_body() {
        assert_eq!(
            parse(head("POST", &[])).unwrap().kind,
            ReadKind::Subscription {
                pin: None,
                range: None,
                page_credits: 2,
                byte_credits: 2 * PAGE_BYTES,
                ordered: false,
            }
        );
        for name in ["Racer-Page-Credits", "Racer-Byte-Credits"] {
            for value in [
                b"".as_slice(),
                b"0",
                b"00",
                b"01",
                b"+1",
                b"-1",
                b" 1",
                b"1 ",
                b"1.0",
                b"18446744073709551616",
            ] {
                assert!(
                    parse(head("POST", &[(name, value)])).is_err(),
                    "{name} {value:?}"
                );
            }
            assert!(parse(head("POST", &[(name, b"2"), (name, b"2")])).is_err());
        }
        for pages in [1, 64] {
            for bytes in [PAGE_BYTES, 64 * PAGE_BYTES] {
                for ordered in ["0", "1"] {
                    let parsed = parse(head(
                        "POST",
                        &[
                            ("Racer-Page-Credits", pages.to_string().as_bytes()),
                            ("Racer-Byte-Credits", bytes.to_string().as_bytes()),
                            ("Racer-Ordered", ordered.as_bytes()),
                        ],
                    ))
                    .unwrap();
                    assert!(
                        matches!(parsed.kind, ReadKind::Subscription { page_credits, byte_credits, ordered: actual, .. }
                        if page_credits == pages && byte_credits == bytes && actual == (ordered == "1"))
                    );
                }
            }
        }
        for (name, value) in [
            ("Racer-Page-Credits", "65".into()),
            ("Racer-Byte-Credits", (PAGE_BYTES - 1).to_string()),
            ("Racer-Byte-Credits", (64 * PAGE_BYTES + 1).to_string()),
            ("Racer-Ordered", "2".into()),
            ("Racer-Ordered", "01".into()),
        ] {
            assert!(parse(head("POST", &[(name, value.as_bytes())])).is_err());
        }
        assert!(
            parse(head(
                "POST",
                &[("Racer-Ordered", b"0"), ("racer-ordered", b"1")]
            ))
            .is_err()
        );
        let mut missing = head("POST", &[]);
        missing.headers.retain(|h| h.name != "Content-Length");
        assert!(parse(missing).is_err());
        for value in [b"0".as_slice(), b"00", b"1"] {
            assert!(parse(head("POST", &[("Content-Length", value)])).is_err());
        }
        let mut legacy = head("POST", &[]);
        if let StartLine::Request { target, .. } = &mut legacy.start {
            *target = target.replace("/v2/", "/v1/");
        }
        assert!(parse(legacy).is_err());
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
            parsed.origin.metadata.unwrap().as_header(),
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
