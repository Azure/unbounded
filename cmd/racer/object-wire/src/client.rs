//! Client request semantics and response heads, independent of I/O and admission.

use crate::model::{
    ByteRange, CacheKey, MAX_FIELD_BYTES, MAX_WIRE_INTEGER, ObjectMetadata, PAGE_BYTES,
    ResolvedRange, StrongEtag, parse_decimal,
};
use crate::{Error, Result};
use http1::{Header, MessageHead, StartLine, is_token, trim_ows};
use std::collections::HashSet;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

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
    },
    /// Write the plaintext into a client file instead of the socket.
    HostRead {
        pin: Option<StrongEtag>,

        range: Option<ByteRange>,

        destination: Destination,
    },
    /// Write the plaintext into a client GPU allocation.
    GpuRead {
        pin: Option<StrongEtag>,

        range: Option<ByteRange>,

        destination: Destination,

        /// The size the client passed to `cuMemCreate`.
        allocation_size: u64,

        /// The UUID of the GPU that holds the allocation, from
        /// `Racer-Gpu-Uuid`. Absent from older clients.
        gpu_uuid: Option<[u8; 16]>,
    },
}

/// The client window `[offset, offset + length)` that a direct read may write.
/// The parser guarantees `length > 0` and that the end fits in a wire integer.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Destination {
    pub offset: u64,

    pub length: u64,
}

impl Destination {
    /// Exclusive end of the window.
    pub fn end(&self) -> u64 {
        self.offset + self.length
    }
}

/// Where a direct read puts the plaintext, sent as `Racer-Delivery`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DirectDelivery {
    Host,
    Gpu,
}

impl DirectDelivery {
    /// The `Racer-Delivery` header value.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Host => "host",
            Self::Gpu => "gpu",
        }
    }
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
            Self::Subscription { pin, .. }
            | Self::HostRead { pin, .. }
            | Self::GpuRead { pin, .. } => pin.as_ref(),
            _ => None,
        }
    }

    /// The requested range of a body read, if any.
    pub fn range(&self) -> Option<ByteRange> {
        match self {
            Self::Subscription { range, .. }
            | Self::HostRead { range, .. }
            | Self::GpuRead { range, .. } => *range,
            _ => None,
        }
    }

    /// Whether this request writes into a client descriptor, not the socket.
    /// Exactly these requests must carry one descriptor.
    pub fn direct(&self) -> Option<DirectDelivery> {
        match self {
            Self::HostRead { .. } => Some(DirectDelivery::Host),
            Self::GpuRead { .. } => Some(DirectDelivery::Gpu),
            _ => None,
        }
    }

    /// The destination window of a direct read.
    pub fn destination(&self) -> Option<Destination> {
        match self {
            Self::HostRead { destination, .. } | Self::GpuRead { destination, .. } => {
                Some(*destination)
            }
            _ => None,
        }
    }
}

/// Validated request borrowing opaque fields; credential ownership stays with the app.
/// Deliberately neither Clone nor Debug, to avoid duplicating or logging secrets.
pub struct ClientRequest<'a> {
    pub kind: ReadKind,

    /// Requested HEAD retention. Zero max-age and subscriptions do not warm.
    pub retain: Option<Duration>,

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
        let path = target
            .strip_prefix("/v1/objects/")
            .ok_or(Error::InvalidRequest)?;
        // Only the exact suffixes select a direct read; anything else keeps the
        // plain object path and its existing key validation.
        let (key_text, direct) = match path.split_once('/') {
            Some((key, "host-read")) => (key, Some(DirectDelivery::Host)),
            Some((key, "gpu-read")) => (key, Some(DirectDelivery::Gpu)),
            _ => (path, None),
        };
        let mut destination_offset = None;
        let mut destination_length = None;
        let mut allocation_size = None;
        let mut gpu_uuid = None;
        let mut seen = HashSet::new();
        let mut host = None;
        let mut pin = None;
        let mut range = None;
        let mut metadata = None;
        let mut authorization = None;
        let mut content_length = false;
        let mut page_credits = 2;
        let mut byte_credits = 2 * PAGE_BYTES;
        let mut max_age = None;
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
            ) && !seen.insert(name.clone())
            {
                return Err(Error::InvalidRequest);
            }
            match name.as_str() {
                "cache-control" if method == "HEAD" => parse_max_age(value, &mut max_age)?,
                "host" => host = Some(trim_ows(value) == b"racer"),
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
                // Direct reads are paced by the dataplane and ignore credits.
                "racer-page-credits" | "racer-byte-credits" if direct.is_some() => {}
                "racer-page-credits" => page_credits = decimal(value, 1, 64)? as usize,
                "racer-byte-credits" => byte_credits = decimal(value, PAGE_BYTES, 64 * PAGE_BYTES)?,
                "racer-destination-offset" if direct.is_some() => {
                    once(&mut destination_offset, parse_decimal(value)?)?;
                }
                "racer-destination-length" if direct.is_some() => {
                    once(&mut destination_length, parse_decimal(value)?)?;
                }
                "racer-allocation-size" if direct == Some(DirectDelivery::Gpu) => {
                    once(&mut allocation_size, parse_decimal(value)?)?;
                }
                "racer-gpu-uuid" if direct == Some(DirectDelivery::Gpu) => {
                    if gpu_uuid.is_some() {
                        return Err(Error::InvalidRequest);
                    }
                    gpu_uuid = Some(parse_gpu_uuid(trim_ows(value))?);
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
        let key = CacheKey::parse_hex(key_text.as_bytes())?;
        let kind = match (method.as_str(), direct) {
            ("POST", Some(_)) if !content_length => return Err(Error::InvalidRequest),
            ("POST", Some(delivery)) => {
                let destination = Destination {
                    offset: destination_offset.ok_or(Error::InvalidRequest)?,
                    length: destination_length
                        .filter(|&length| length != 0)
                        .ok_or(Error::InvalidRequest)?,
                };
                destination
                    .offset
                    .checked_add(destination.length)
                    .filter(|&end| end <= MAX_WIRE_INTEGER)
                    .ok_or(Error::InvalidRequest)?;
                match delivery {
                    DirectDelivery::Host => ReadKind::HostRead {
                        pin,
                        range,
                        destination,
                    },
                    DirectDelivery::Gpu => {
                        let allocation_size = allocation_size
                            .filter(|&size| destination.end() <= size)
                            .ok_or(Error::InvalidRequest)?;
                        ReadKind::GpuRead {
                            pin,
                            range,
                            destination,
                            allocation_size,
                            gpu_uuid,
                        }
                    }
                }
            }
            (_, Some(_)) => return Err(Error::MethodNotAllowed),
            (method, None) => match method {
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
                },
                "POST" => return Err(Error::InvalidRequest),
                _ => return Err(Error::MethodNotAllowed),
            },
        };
        Ok(ClientRequest {
            kind,
            retain: max_age
                .filter(|&seconds| seconds != 0)
                .map(Duration::from_secs),
            key,
            metadata,
            authorization,
        })
    }
}

/// Read delta-seconds, including quoted digits, without splitting quoted commas.
fn parse_max_age(value: &[u8], max_age: &mut Option<u64>) -> Result<()> {
    let mut quoted = false;
    let mut escaped = false;
    let mut start = 0;
    for (i, &byte) in value.iter().chain(std::iter::once(&b',')).enumerate() {
        if escaped {
            escaped = false;
        } else if quoted && byte == b'\\' {
            escaped = true;
        } else if byte == b'"' {
            quoted = !quoted;
        } else if byte == b',' && !quoted {
            let directive = trim_ows(&value[start..i]);
            start = i + 1;
            let end = directive
                .iter()
                .position(|&b| !is_token(b))
                .unwrap_or(directive.len());
            if !directive[..end].eq_ignore_ascii_case(b"max-age") {
                continue;
            }
            if max_age.is_some() {
                return Err(Error::InvalidRequest);
            }
            let argument = trim_ows(&directive[end..])
                .strip_prefix(b"=")
                .ok_or(Error::InvalidRequest)?;
            let mut digits = trim_ows(argument);
            if digits.starts_with(b"\"") {
                digits = digits
                    .strip_prefix(b"\"")
                    .and_then(|s| s.strip_suffix(b"\""))
                    .ok_or(Error::InvalidRequest)?;
            }
            if digits.is_empty() || !digits.iter().all(u8::is_ascii_digit) {
                return Err(Error::InvalidRequest);
            }
            *max_age = Some(
                digits
                    .iter()
                    .try_fold(0u64, |n, digit| {
                        n.checked_mul(10)?.checked_add(u64::from(digit - b'0'))
                    })
                    .ok_or(Error::InvalidRequest)?,
            );
        }
    }
    if quoted || escaped {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

/// Store a destination field, rejecting a repeated one.
/// Parse a GPU UUID as `nvidia-smi` prints it, `GPU-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`.
/// The `GPU-` prefix is optional and hex digits may be either case.
pub fn parse_gpu_uuid(value: &[u8]) -> Result<[u8; 16]> {
    let value = match value.get(..4) {
        Some(prefix) if prefix.eq_ignore_ascii_case(b"GPU-") => &value[4..],
        _ => value,
    };
    if value.len() != 36 {
        return Err(Error::InvalidRequest);
    }
    let mut uuid = [0; 16];
    let mut digits = 0;
    for (i, &b) in value.iter().enumerate() {
        if matches!(i, 8 | 13 | 18 | 23) {
            if b != b'-' {
                return Err(Error::InvalidRequest);
            }
            continue;
        }
        let nibble = (b as char).to_digit(16).ok_or(Error::InvalidRequest)? as u8;
        uuid[digits / 2] |= if digits % 2 == 0 { nibble << 4 } else { nibble };
        digits += 1;
    }
    Ok(uuid)
}

fn once(slot: &mut Option<u64>, value: u64) -> Result<()> {
    if slot.replace(value).is_some() {
        return Err(Error::InvalidRequest);
    }
    Ok(())
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
    body_head(metadata, range, true)
}

/// Shared Get-style head. Without payload, page frames carry only their 21 bytes.
fn body_head(
    metadata: &ObjectMetadata,
    range: Option<ResolvedRange>,
    payload: bool,
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
        .and_then(|n| n.checked_add(if payload { end - start } else { 0 }))
        .filter(|&n| n <= MAX_WIRE_INTEGER)
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

/// Encode a direct-read success head. Page frames carry no payload, so the
/// body is `21 * (pages + 1)` bytes. An empty object has no pages.
pub fn direct_head(
    metadata: &ObjectMetadata,
    range: Option<ResolvedRange>,
    delivery: DirectDelivery,
) -> Result<MessageHead> {
    let mut head = body_head(metadata, range, false)?;
    let close = head
        .headers
        .iter()
        .position(|h| h.name.eq_ignore_ascii_case("Connection"))
        .unwrap_or(head.headers.len());
    head.headers
        .insert(close, header("Racer-Delivery", delivery.as_str()));
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
    success_head_retained(metadata, range, None)
}

/// Add an accepted retention deadline, rounded down to Unix milliseconds.
pub fn success_head_retained(
    metadata: &ObjectMetadata,
    range: Option<ResolvedRange>,
    retain_until: Option<SystemTime>,
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
    if let Some(until) = retain_until {
        let millis = until
            .duration_since(UNIX_EPOCH)
            .map_err(|_| Error::BadGateway)?
            .as_millis();
        if millis > i64::MAX as u128 {
            return Err(Error::BadGateway);
        }
        headers.push(header("Racer-Retain-Until", millis.to_string()));
    }
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
