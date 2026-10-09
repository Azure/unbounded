//! Client request semantics and response heads, independent of I/O and admission.

use crate::model::{
    ByteRange, CacheKey, MAX_FIELD_BYTES, ObjectMetadata, PAGE_BYTES, ResolvedRange, StrongEtag,
};
use crate::{Error, Result};
use http1::{Header, MessageHead, StartLine, is_token, trim_ows};
use std::collections::HashSet;
use std::time::UNIX_EPOCH;

/// Maximum client request head size, including raw HTTP framing.
pub const MAX_HEAD_BYTES: usize = 32 * 1024;

/// A metadata read or a credit-bounded object subscription.
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
    /// Whether this request returns metadata without a subscription body.
    pub fn is_head(&self) -> bool {
        matches!(self, Self::Head | Self::HeadPinned { .. })
    }

    /// The explicitly requested immutable version, if any.
    pub fn pin(&self) -> Option<&StrongEtag> {
        match self {
            Self::HeadPinned { etag } => Some(etag),
            Self::Subscription { pin, .. } => pin.as_ref(),
            _ => None,
        }
    }
}

/// Validated request borrowing opaque fields; credential ownership stays with the app.
/// Deliberately neither Clone nor Debug, to avoid duplicating or logging secrets.
pub struct ClientRequest<'a> {
    pub kind: ReadKind,

    pub key: CacheKey,

    pub metadata: Option<&'a [u8]>,

    pub authorization: Option<&'a [u8]>,
}

/// Bounded semantic parser. Raw HTTP framing must be checked before calling it.
#[derive(Clone)]
pub struct RequestParser {
    header_limit: usize,
}

impl RequestParser {
    /// Cap decoded input as well as the separately enforced raw head limit.
    pub fn new(header_limit: usize) -> Self {
        Self {
            header_limit: header_limit.min(MAX_HEAD_BYTES),
        }
    }

    /// Cap to apply to raw framing before semantic parsing.
    pub fn header_limit(&self) -> usize {
        self.header_limit
    }

    /// Preserve opaque bytes and require canonical SDK keys, ranges, and credits.
    pub fn parse<'a>(&self, head: &'a MessageHead) -> Result<ClientRequest<'a>> {
        let StartLine::Request { method, target } = &head.start else {
            return Err(Error::InvalidRequest);
        };
        let mut decoded_bytes = method.len().saturating_add(target.len());
        let mut seen = HashSet::new();
        let mut host = None;
        let mut pin = None;
        let mut range = None;
        let mut metadata = None;
        let mut authorization = None;
        let mut content_length = false;
        let mut page_credits = 2;
        let mut byte_credits = 2 * PAGE_BYTES;
        let mut ordered = false;
        for header in &head.headers {
            decoded_bytes = decoded_bytes
                .saturating_add(header.name.len())
                .saturating_add(header.value.len());
            if decoded_bytes > self.header_limit {
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
                return Err(Error::InvalidRequest);
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
                | "if-range" => return Err(Error::InvalidRequest),
                "connection"
                    if value
                        .split(|&b| b == b',')
                        .any(|token| trim_ows(token).eq_ignore_ascii_case(b"upgrade")) =>
                {
                    return Err(Error::InvalidRequest);
                }
                "if-match" => {
                    if value.len() > MAX_FIELD_BYTES {
                        return Err(Error::HeaderTooLarge);
                    }
                    pin = Some(StrongEtag::parse(value)?);
                }
                "range" => range = Some(ByteRange::parse(value)?),
                "racer-page-credits" => page_credits = decimal(value, 1, 64)? as usize,
                "racer-byte-credits" => byte_credits = decimal(value, PAGE_BYTES, 64 * PAGE_BYTES)?,
                "racer-ordered" => {
                    ordered = match value {
                        b"0" => false,
                        b"1" => true,
                        _ => return Err(Error::InvalidRequest),
                    }
                }
                "racer-metadata" => {
                    validate_opaque(value)?;
                    metadata = Some(value);
                }
                "authorization" => {
                    validate_opaque(value)?;
                    authorization = Some(value);
                }
                _ => {}
            }
        }
        if decoded_bytes > self.header_limit {
            return Err(Error::HeaderTooLarge);
        }
        if host != Some(true) {
            return Err(Error::InvalidRequest);
        }
        let key = CacheKey::parse_hex(
            target
                .strip_prefix("/v2/objects/")
                .ok_or(Error::InvalidRequest)?
                .as_bytes(),
        )?;
        let kind = match method.as_str() {
            "HEAD" if range.is_none() => match pin {
                Some(etag) => ReadKind::HeadPinned { etag },
                None => ReadKind::Head,
            },
            "HEAD" => return Err(Error::InvalidRequest),
            "POST" if content_length => ReadKind::Subscription {
                pin,
                range,
                page_credits,
                byte_credits,
                ordered,
            },
            "POST" => return Err(Error::InvalidRequest),
            _ => return Err(Error::MethodNotAllowed),
        };
        Ok(ClientRequest {
            kind,
            key,
            metadata,
            authorization,
        })
    }
}

/// Parse a nonzero canonical credit value inside its protocol bounds.
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

/// Validate a client context field without normalizing or owning sensitive bytes.
fn validate_opaque(value: &[u8]) -> Result<()> {
    if value.len() > MAX_FIELD_BYTES {
        return Err(Error::HeaderTooLarge);
    }
    if value.is_empty()
        || value.first() == Some(&b' ')
        || value.last() == Some(&b' ')
        || value.iter().any(|&b| b < 0x20 || b == 0x7f)
    {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

/// Encode subscription framing lengths before any response bytes are sent.
pub fn subscription_head(
    metadata: &ObjectMetadata,
    range: Option<ResolvedRange>,
) -> Result<MessageHead> {
    let mut head = success_head(metadata, None)?;
    head.headers
        .retain(|h| !h.name.eq_ignore_ascii_case("Content-Length"));
    let (start, end, pages) = match range {
        Some(range) if range.end() <= metadata.length => (
            range.start(),
            range.end(),
            range.last_page().0 - range.first_page().0 + 1,
        ),
        None if metadata.length == 0 => (0, 0, 0),
        _ => return Err(Error::BadGateway),
    };
    let length = pages
        .checked_add(1)
        .and_then(|n| n.checked_mul(21))
        .and_then(|n| n.checked_add(end - start))
        .ok_or(Error::BadGateway)?;
    head.start = StartLine::Response { status: 200 };
    head.headers.extend([
        header("Racer-Object-Length", metadata.length.to_string()),
        header("Racer-Range-Start", start.to_string()),
        header("Racer-Range-End", end.to_string()),
        header("Content-Length", length.to_string()),
        header("Connection", "close"),
    ]);
    Ok(head)
}

/// Encode the fixed-width page or completion frame without managing page leases.
pub fn frame(kind: u8, number: u64, offset: u64, length: u32) -> [u8; 21] {
    let mut bytes = [0; 21];
    bytes[0] = kind;
    bytes[1..9].copy_from_slice(&number.to_be_bytes());
    bytes[9..17].copy_from_slice(&offset.to_be_bytes());
    bytes[17..].copy_from_slice(&length.to_be_bytes());
    bytes
}

/// Encode a HEAD success after validating immutable metadata and expiry precision.
pub fn success_head(
    metadata: &ObjectMetadata,
    range: Option<ResolvedRange>,
) -> Result<MessageHead> {
    let expiry = metadata
        .expires_at
        .as_system_time()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| Error::BadGateway)?;
    if metadata.length > i64::MAX as u64
        || expiry.as_millis() > i64::MAX as u128
        || expiry.subsec_nanos() % 1_000_000 != 0
    {
        return Err(Error::BadGateway);
    }
    let mut headers = vec![
        header("ETag", metadata.version.etag.as_bytes()),
        header("Racer-Expires-At", expiry.as_millis().to_string()),
    ];
    if let Some(content_type) = &metadata.content_type {
        headers.push(header("Racer-Content-Type", content_type.as_bytes()));
    }
    if range.is_some() {
        return Err(Error::BadGateway);
    }
    headers.push(header("Content-Length", metadata.length.to_string()));
    headers.push(header("Content-Type", "application/octet-stream"));
    Ok(MessageHead {
        start: StartLine::Response { status: 200 },
        headers,
    })
}

/// Encode a zero-body error selected by application policy, with required metadata.
pub fn error_head(status: u16, unsatisfied_length: Option<u64>) -> Result<MessageHead> {
    let mut headers = vec![header("Content-Length", "0")];
    if status == 405 {
        headers.push(header("Allow", "HEAD, POST"));
    }
    if status == 416 {
        let length = unsatisfied_length
            .filter(|length| *length <= i64::MAX as u64)
            .ok_or(Error::Internal)?;
        headers.push(header("Content-Range", format!("bytes */{length}")));
    }
    Ok(MessageHead {
        start: StartLine::Response { status },
        headers,
    })
}

/// Build one HTTP field without normalizing its bytes.
fn header(name: &str, value: impl AsRef<[u8]>) -> Header {
    Header::new(name, value)
}
