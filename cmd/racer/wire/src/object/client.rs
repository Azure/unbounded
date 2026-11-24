//! Client request semantics and response heads, independent of I/O and admission.

use crate::http::{Header, MessageHead, StartLine, is_token, trim_ows};
use crate::object::model::{
    ByteRange, CacheKey, ContentType, MAX_FIELD_BYTES, MAX_WIRE_INTEGER, ObjectMetadata,
    PAGE_BYTES, PageSize, ResolvedRange, StrongEtag, parse_decimal,
};
use crate::object::{Error, Result};
use std::collections::HashSet;
use std::time::UNIX_EPOCH;

/// Maximum client request head size, including raw HTTP framing.
pub const MAX_HEAD_BYTES: usize = 32 * 1024;

/// Maximum fields accepted in a client-socket response head.
pub const MAX_RESPONSE_HEADERS: usize = 64;

/// Fixed-width subscription frame header size.
pub const FRAME_SIZE: usize = 21;

/// Fixed-width page credit size: page number and length.
pub const CREDIT_SIZE: usize = 12;

/// Page payload frame kind.
pub const PAGE_FRAME: u8 = 1;

/// Completion frame kind.
pub const COMPLETE_FRAME: u8 = 2;

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
                // The parser does not know the cluster page size: accept credits
                // down to the smallest page any cluster may publish. The stream
                // then requires at least one page of the cluster's size.
                "racer-byte-credits" => {
                    byte_credits = decimal(value, PageSize::TEST_MIN.get(), 64 * PAGE_BYTES)?
                }
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
            key,
            metadata,
            authorization,
        })
    }
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
pub fn frame(kind: u8, number: u64, offset: u64, length: u32) -> [u8; FRAME_SIZE] {
    let mut bytes = [0; FRAME_SIZE];
    bytes[0] = kind;
    bytes[1..9].copy_from_slice(&number.to_be_bytes());
    bytes[9..17].copy_from_slice(&offset.to_be_bytes());
    bytes[17..].copy_from_slice(&length.to_be_bytes());
    bytes
}

/// One decoded page or completion frame header.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Frame {
    /// [`PAGE_FRAME`] or [`COMPLETE_FRAME`].
    pub kind: u8,

    /// Zero-based page number, or the delivered page count on completion.
    pub number: u64,

    /// Absolute object offset, or the delivered byte count on completion.
    pub offset: u64,

    /// Payload bytes following a page header; zero on completion.
    pub length: u32,
}

impl Frame {
    /// Decode one fixed-width frame header.
    pub fn decode(bytes: &[u8; FRAME_SIZE]) -> Self {
        Self {
            kind: bytes[0],
            number: u64::from_be_bytes(bytes[1..9].try_into().unwrap_or_default()),
            offset: u64::from_be_bytes(bytes[9..17].try_into().unwrap_or_default()),
            length: u32::from_be_bytes(bytes[17..21].try_into().unwrap_or_default()),
        }
    }

    /// Encode one fixed-width frame header.
    pub fn encode(&self) -> [u8; FRAME_SIZE] {
        frame(self.kind, self.number, self.offset, self.length)
    }
}

/// Encode the credit returned for a consumed page.
pub fn credit_bytes(frame: &Frame) -> [u8; CREDIT_SIZE] {
    let mut bytes = [0; CREDIT_SIZE];
    bytes[..8].copy_from_slice(&frame.number.to_be_bytes());
    bytes[8..].copy_from_slice(&frame.length.to_be_bytes());
    bytes
}

/// Number of pages covering `[first, end)` at the protocol page size.
pub fn page_count(first: u64, end: u64) -> u64 {
    if end <= first {
        0
    } else {
        (end - 1) / PAGE_BYTES - first / PAGE_BYTES + 1
    }
}

/// Frame validation state for one subscription range.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Sequence {
    first: u64,
    end: u64,
    /// Pages covering the selected range.
    pub pages: u64,
    delivered: u64,
    bytes: u64,
}

impl Sequence {
    /// Expect the pages covering `[first, end)`.
    pub fn new(first: u64, end: u64) -> Self {
        Self {
            first,
            end,
            pages: page_count(first, end),
            delivered: 0,
            bytes: 0,
        }
    }

    /// Validate the next frame header. Page frames advance the sequence.
    pub fn accept(&mut self, frame: &Frame) -> Result<()> {
        let bad = Error::BadGateway;
        if frame.kind == COMPLETE_FRAME {
            if frame.number != self.pages
                || self.delivered != self.pages
                || frame.offset != self.end - self.first
                || self.bytes != frame.offset
                || frame.length != 0
            {
                return Err(bad);
            }
            return Ok(());
        }
        if frame.kind != PAGE_FRAME
            || frame.length == 0
            || frame.offset < self.first
            || frame.offset >= self.end
            || frame.number != frame.offset / PAGE_BYTES
            || self.delivered == self.pages
        {
            return Err(bad);
        }
        let start = self.first.max(frame.number * PAGE_BYTES);
        let end = self.end.min((frame.number + 1) * PAGE_BYTES);
        if frame.offset != start
            || u64::from(frame.length) != end - start
            || frame.number != self.first / PAGE_BYTES + self.delivered
        {
            return Err(bad);
        }
        self.delivered += 1;
        self.bytes += u64::from(frame.length);
        Ok(())
    }

    /// Whether every page was delivered.
    pub fn last_page(&self) -> bool {
        self.delivered == self.pages
    }
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

/// Validated object metadata from a client-socket response head.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ResponseMetadata<'a> {
    /// Object length in bytes.
    pub size: u64,

    /// Exact quoted strong entity tag.
    pub etag: &'a [u8],

    /// Exact object MIME metadata, if the response carried it.
    pub content_type: Option<&'a [u8]>,
}

/// A validated subscription response head.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SubscriptionResponse<'a> {
    /// Object metadata from the response head.
    pub metadata: ResponseMetadata<'a>,

    /// Inclusive first selected offset.
    pub first: u64,

    /// Exclusive selected end offset.
    pub end: u64,

    /// Pages covering the selection.
    pub pages: u64,
}

/// Fields of a client-socket response head after raw validation.
///
/// The borrowed values point into the exact head bytes supplied to
/// [`ResponseHead::parse`]. Body bytes that arrived with the head must not be
/// included.
#[derive(Default)]
pub struct ResponseHead<'a> {
    status: u16,
    content_length: Option<&'a [u8]>,
    content_type: Option<&'a [u8]>,
    content_range: Option<&'a [u8]>,
    etag: Option<&'a [u8]>,
    expires: Option<&'a [u8]>,
    racer_content_type: Option<&'a [u8]>,
    object_length: Option<&'a [u8]>,
    range_start: Option<&'a [u8]>,
    range_end: Option<&'a [u8]>,
    allow: Option<&'a [u8]>,
    close: bool,
    forbidden: bool,
}

impl<'a> ResponseHead<'a> {
    /// Parse exactly one response head, including its terminator.
    pub fn parse(raw: &'a [u8]) -> Result<Self> {
        let bad = Error::BadGateway;
        let mut headers = [httparse::EMPTY_HEADER; MAX_RESPONSE_HEADERS];
        let mut response = httparse::Response::new(&mut headers);
        match response.parse(raw) {
            Ok(httparse::Status::Complete(length)) if length == raw.len() => {}
            _ => return Err(bad),
        }
        if response.version != Some(1) {
            return Err(bad);
        }
        let mut head = Self {
            status: response.code.ok_or(bad)?,
            ..Self::default()
        };
        for field in response.headers.iter() {
            let value = field.value;
            if value
                .iter()
                .any(|&byte| (byte < 0x20 && byte != b'\t') || byte == 0x7f)
            {
                return Err(bad);
            }
            let name = field.name;
            let slot = if name.eq_ignore_ascii_case("content-length") {
                &mut head.content_length
            } else if name.eq_ignore_ascii_case("content-type") {
                &mut head.content_type
            } else if name.eq_ignore_ascii_case("content-range") {
                &mut head.content_range
            } else if name.eq_ignore_ascii_case("etag") {
                &mut head.etag
            } else if name.eq_ignore_ascii_case("racer-expires-at") {
                &mut head.expires
            } else if name.eq_ignore_ascii_case("racer-content-type") {
                &mut head.racer_content_type
            } else if name.eq_ignore_ascii_case("racer-object-length") {
                &mut head.object_length
            } else if name.eq_ignore_ascii_case("racer-range-start") {
                &mut head.range_start
            } else if name.eq_ignore_ascii_case("racer-range-end") {
                &mut head.range_end
            } else if name.eq_ignore_ascii_case("allow") {
                if head.allow.is_none() {
                    head.allow = Some(value);
                }
                continue;
            } else if name.eq_ignore_ascii_case("connection") {
                for token in value.split(|&byte| byte == b',') {
                    let token = token.trim_ascii();
                    head.close |= token.eq_ignore_ascii_case(b"close");
                    head.forbidden |= token.eq_ignore_ascii_case(b"upgrade");
                }
                continue;
            } else {
                head.forbidden |= [
                    "transfer-encoding",
                    "content-encoding",
                    "trailer",
                    "upgrade",
                    "expect",
                    "if-none-match",
                    "if-modified-since",
                    "if-unmodified-since",
                    "if-range",
                ]
                .iter()
                .any(|forbidden| name.eq_ignore_ascii_case(forbidden));
                continue;
            };
            // Singleton fields must not repeat.
            if slot.replace(value).is_some() {
                return Err(bad);
            }
        }
        if head.forbidden {
            return Err(bad);
        }
        Ok(head)
    }

    /// Whether the response asked for connection closure.
    pub fn closes_connection(&self) -> bool {
        self.close
    }

    /// Validate a metadata response.
    pub fn stat(&self) -> Result<ResponseMetadata<'a>> {
        let bad = Error::BadGateway;
        let length = self.content_length.and_then(response_decimal).ok_or(bad)?;
        if self.status != 200 && self.status != 206 {
            let failure = status_error(self.status);
            let known = matches!(
                self.status,
                400 | 401 | 403 | 404 | 405 | 412 | 416 | 431 | 500 | 502 | 503
            );
            if !known
                || length != 0
                || self.etag.is_some()
                || self.expires.is_some()
                || self.racer_content_type.is_some()
            {
                return Err(bad);
            }
            if self.status == 416 {
                let value = self.content_range.ok_or(bad)?;
                value
                    .strip_prefix(b"bytes */")
                    .and_then(response_decimal)
                    .ok_or(bad)?;
            } else if self.content_range.is_some() {
                return Err(bad);
            }
            if self.status == 405 && self.allow != Some(b"HEAD, POST") {
                return Err(bad);
            }
            return Err(failure);
        }
        if self.status != 200 || self.content_range.is_some() {
            return Err(bad);
        }
        let mut metadata = self.metadata()?;
        metadata.size = length;
        Ok(metadata)
    }

    /// Validate a subscription response head and return its selected range.
    pub fn subscription(&self, pin: Option<&[u8]>) -> Result<SubscriptionResponse<'a>> {
        let bad = Error::BadGateway;
        let length = self.content_length.and_then(response_decimal).ok_or(bad)?;
        if self.status != 200 {
            if length != 0 {
                return Err(bad);
            }
            let content_range = self.content_range.filter(|value| !value.is_empty());
            if self.status == 416 {
                let size = content_range
                    .and_then(|value| value.strip_prefix(b"bytes */"))
                    .and_then(response_decimal)
                    .ok_or(bad)?;
                return Err(Error::UnsatisfiableRangeWithLength(size));
            } else if content_range.is_some() {
                return Err(bad);
            }
            return Err(status_error(self.status));
        }
        if !self.close
            || self.content_range.is_some_and(|value| !value.is_empty())
            || self.content_type != Some(b"application/octet-stream")
        {
            return Err(bad);
        }
        let size = self.object_length.and_then(response_decimal).ok_or(bad)?;
        let first = self.range_start.and_then(response_decimal).ok_or(bad)?;
        let end = self.range_end.and_then(response_decimal).ok_or(bad)?;
        let mut metadata = self.metadata()?;
        metadata.size = size;
        if first > end || end > size || pin.is_some_and(|pin| pin != metadata.etag) {
            return Err(bad);
        }
        let pages = page_count(first, end);
        let framing = (pages + 1) * FRAME_SIZE as u64;
        if length != (end - first).checked_add(framing).ok_or(bad)? {
            return Err(bad);
        }
        Ok(SubscriptionResponse {
            metadata,
            first,
            end,
            pages,
        })
    }

    /// Validate immutable metadata shared by stat and subscription responses.
    fn metadata(&self) -> Result<ResponseMetadata<'a>> {
        let bad = Error::BadGateway;
        let etag = self
            .etag
            .filter(|etag| StrongEtag::validate(etag).is_ok())
            .ok_or(bad)?;
        self.expires.and_then(response_decimal).ok_or(bad)?;
        let content_type = match self.racer_content_type {
            Some(value) if value.is_empty() || ContentType::validate(value).is_err() => {
                return Err(bad);
            }
            other => other,
        };
        Ok(ResponseMetadata {
            size: 0,
            etag,
            content_type,
        })
    }
}

/// Canonical wire decimal, reported as a response protocol failure by callers.
fn response_decimal(value: &[u8]) -> Option<u64> {
    parse_decimal(value).ok()
}

/// Map a Racer status to its client-protocol failure.
fn status_error(status: u16) -> Error {
    match status {
        400 => Error::InvalidRequest,
        401 => Error::OriginRejected,
        403 => Error::OriginForbidden,
        404 => Error::NotFound,
        405 => Error::MethodNotAllowed,
        412 => Error::VersionUnavailable,
        416 => Error::UnsatisfiableRange,
        431 => Error::HeaderTooLarge,
        503 => Error::Unavailable,
        _ => Error::BadGateway,
    }
}

/// Build one HTTP field without normalizing its bytes.
fn header(name: &str, value: impl AsRef<[u8]>) -> Header {
    Header::new(name, value)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn head(raw: &str) -> Result<ResponseMetadata<'_>> {
        ResponseHead::parse(raw.as_bytes())?.stat()
    }

    #[test]
    fn stat_heads() {
        let ok = head("HTTP/1.1 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nRacer-Content-Type: text/plain\r\nContent-Length: 10\r\n\r\n").unwrap();
        assert_eq!(ok.size, 10);
        assert_eq!(ok.etag, b"\"v\"");
        assert_eq!(ok.content_type, Some(&b"text/plain"[..]));
        for (status, want) in [
            (400, Error::InvalidRequest),
            (401, Error::OriginRejected),
            (403, Error::OriginForbidden),
            (404, Error::NotFound),
            (412, Error::VersionUnavailable),
            (431, Error::HeaderTooLarge),
            (500, Error::BadGateway),
            (502, Error::BadGateway),
            (503, Error::Unavailable),
            (418, Error::BadGateway),
        ] {
            let raw = format!("HTTP/1.1 {status} X\r\nContent-Length: 0\r\n\r\n");
            let got = head(&raw);
            assert_eq!(got, Err(want), "{status}");
        }
        assert_eq!(
            head("HTTP/1.1 405 X\r\nContent-Length: 0\r\nAllow: HEAD, POST\r\n\r\n"),
            Err(Error::MethodNotAllowed)
        );
        for bad in [
            "HTTP/1.1 405 X\r\nContent-Length: 0\r\n\r\n",
            "HTTP/1.1 404 X\r\nContent-Length: 1\r\n\r\n",
            "HTTP/1.1 404 X\r\nContent-Length: 0\r\nETag: \"v\"\r\n\r\n",
            "HTTP/1.1 416 X\r\nContent-Length: 0\r\n\r\n",
            "HTTP/1.1 200 OK\r\nETag: v\r\nRacer-Expires-At: 1\r\nContent-Length: 10\r\n\r\n",
            "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nContent-Length: 10\r\n\r\n",
            "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Length: 10\r\n\r\n",
            "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Length: 10\r\nTransfer-Encoding: chunked\r\n\r\n",
            "HTTP/1.0 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Length: 10\r\n\r\n",
            "HTTP/1.1 206 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Length: 10\r\n\r\n",
        ] {
            assert_eq!(head(bad), Err(Error::BadGateway), "{bad}");
        }
    }

    fn subscription<'a>(
        raw: &'a str,
        pin: Option<&[u8]>,
    ) -> Result<(ResponseMetadata<'a>, u64, u64)> {
        let response = ResponseHead::parse(raw.as_bytes())?.subscription(pin)?;
        Ok((response.metadata, response.first, response.end))
    }

    #[test]
    fn subscription_heads() {
        let ok = "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\nContent-Type: application/octet-stream\r\nRacer-Object-Length: 10\r\nRacer-Range-Start: 2\r\nRacer-Range-End: 5\r\nContent-Length: 45\r\nConnection: close\r\n\r\n";
        let (metadata, first, end) = subscription(ok, None).unwrap();
        assert_eq!((metadata.size, first, end), (10, 2, 5));
        assert_eq!(subscription(ok, Some(b"\"w\"")), Err(Error::BadGateway));
        assert_eq!(
            subscription(
                &ok.replace("Content-Length: 45", "Content-Length: 44"),
                None
            ),
            Err(Error::BadGateway)
        );
        assert_eq!(
            subscription(&ok.replace("Connection: close\r\n", ""), None),
            Err(Error::BadGateway)
        );
        assert_eq!(
            subscription(
                "HTTP/1.1 416 X\r\nContent-Length: 0\r\nContent-Range: bytes */7\r\nConnection: close\r\n\r\n",
                None
            ),
            Err(Error::UnsatisfiableRangeWithLength(7))
        );
        assert_eq!(
            subscription("HTTP/1.1 416 X\r\nContent-Length: 0\r\n\r\n", None),
            Err(Error::BadGateway)
        );
        assert_eq!(
            subscription("HTTP/1.1 412 X\r\nContent-Length: 0\r\n\r\n", None),
            Err(Error::VersionUnavailable)
        );
        for (from, to) in [
            ("Racer-Range-Start: 2", "Racer-Range-Start: 6"),
            ("Racer-Range-End: 5", "Racer-Range-End: 11"),
            ("Racer-Range-Start: 2", "Racer-Range-Start: 02"),
            ("Racer-Object-Length: 10\r\n", ""),
            ("Racer-Range-End: 5\r\n", ""),
            ("application/octet-stream", "text/plain"),
            ("ETag: \"v\"", "ETag: v"),
            ("Racer-Expires-At: 1\r\n", ""),
            (
                "Connection: close",
                "Connection: close\r\nContent-Range: bytes 2-4/10",
            ),
            (
                "Connection: close",
                "Connection: close\r\nContent-Encoding: gzip",
            ),
        ] {
            assert_eq!(
                subscription(&ok.replace(from, to), None),
                Err(Error::BadGateway),
                "{to}"
            );
        }
        let with_content_type =
            ok.replace("Connection", "Racer-Content-Type: text/plain\r\nConnection");
        let (metadata, ..) = subscription(&with_content_type, Some(b"\"v\"")).unwrap();
        assert_eq!(metadata.content_type, Some(&b"text/plain"[..]));
        assert_eq!(
            subscription("HTTP/1.1 404 X\r\nContent-Length: 0\r\n\r\n", None),
            Err(Error::NotFound)
        );
        assert_eq!(
            subscription("HTTP/1.1 404 X\r\nContent-Length: 3\r\n\r\n", None),
            Err(Error::BadGateway)
        );
    }

    #[test]
    fn pages() {
        assert_eq!(page_count(0, 0), 0);
        assert_eq!(page_count(0, 1), 1);
        assert_eq!(page_count(0, PAGE_BYTES), 1);
        assert_eq!(page_count(PAGE_BYTES - 4, PAGE_BYTES + 7), 2);
        let frame = Frame {
            kind: PAGE_FRAME,
            number: 3,
            offset: 3 * PAGE_BYTES,
            length: 9,
        };
        assert_eq!(Frame::decode(&frame.encode()), frame);
    }

    #[test]
    fn credits() {
        let frame = Frame {
            kind: PAGE_FRAME,
            number: 0x0102,
            offset: 0,
            length: 0x0a0b0c0d,
        };
        assert_eq!(
            credit_bytes(&frame),
            [0, 0, 0, 0, 0, 0, 1, 2, 0x0a, 0x0b, 0x0c, 0x0d]
        );
    }

    fn page(number: u64, offset: u64, length: u32) -> Frame {
        Frame {
            kind: PAGE_FRAME,
            number,
            offset,
            length,
        }
    }

    fn complete(pages: u64, bytes: u64) -> Frame {
        Frame {
            kind: COMPLETE_FRAME,
            number: pages,
            offset: bytes,
            length: 0,
        }
    }

    #[test]
    fn sequence_accepts_valid_streams() {
        // Empty selection: completion only.
        let mut sequence = Sequence::new(0, 0);
        assert!(sequence.last_page());
        assert_eq!(sequence.accept(&complete(0, 0)), Ok(()));
        // A small range inside one page.
        let mut sequence = Sequence::new(2, 5);
        assert_eq!(sequence.accept(&page(0, 2, 3)), Ok(()));
        assert!(sequence.last_page());
        assert_eq!(sequence.accept(&complete(1, 3)), Ok(()));
        // A small range crossing a page boundary.
        let mut sequence = Sequence::new(PAGE_BYTES - 2, PAGE_BYTES + 3);
        assert_eq!(sequence.accept(&page(0, PAGE_BYTES - 2, 2)), Ok(()));
        assert!(!sequence.last_page());
        assert_eq!(sequence.accept(&page(1, PAGE_BYTES, 3)), Ok(()));
        assert_eq!(sequence.accept(&complete(2, 5)), Ok(()));
        // Full pages followed by a short last page.
        let end = 2 * PAGE_BYTES + 1;
        let mut sequence = Sequence::new(0, end);
        assert_eq!(sequence.accept(&page(0, 0, PAGE_BYTES as u32)), Ok(()));
        assert_eq!(
            sequence.accept(&page(1, PAGE_BYTES, PAGE_BYTES as u32)),
            Ok(())
        );
        assert_eq!(sequence.accept(&page(2, 2 * PAGE_BYTES, 1)), Ok(()));
        assert_eq!(sequence.accept(&complete(3, end)), Ok(()));
    }

    #[test]
    fn sequence_rejects_invalid_frames() {
        let bad = Err(Error::BadGateway);
        let fresh = || Sequence::new(PAGE_BYTES - 2, PAGE_BYTES + 3);
        for frame in [
            // Unknown kind.
            Frame {
                kind: 3,
                ..page(0, PAGE_BYTES - 2, 2)
            },
            // Empty page.
            page(0, PAGE_BYTES - 2, 0),
            // Wrong offset, length, or number.
            page(0, PAGE_BYTES - 3, 3),
            page(0, PAGE_BYTES - 2, 1),
            page(1, PAGE_BYTES - 2, 2),
            // Out of order: the second page first.
            page(1, PAGE_BYTES, 3),
            // Outside the selection.
            page(1, PAGE_BYTES + 3, 1),
            // Completion before any page (truncated stream).
            complete(2, 5),
            complete(0, 0),
        ] {
            assert_eq!(fresh().accept(&frame), bad, "{frame:?}");
        }
        // Completion after a partial delivery.
        let mut sequence = fresh();
        assert_eq!(sequence.accept(&page(0, PAGE_BYTES - 2, 2)), Ok(()));
        assert_eq!(sequence.accept(&complete(2, 5)), bad);
        assert_eq!(sequence.accept(&complete(1, 2)), bad);
        // A repeated page.
        assert_eq!(sequence.accept(&page(0, PAGE_BYTES - 2, 2)), bad);
        // Extra pages after the last, and a mismatched completion.
        assert_eq!(sequence.accept(&page(1, PAGE_BYTES, 3)), Ok(()));
        assert_eq!(sequence.accept(&page(1, PAGE_BYTES, 3)), bad);
        assert_eq!(sequence.accept(&complete(2, 4)), bad);
        assert_eq!(sequence.accept(&complete(3, 5)), bad);
        assert_eq!(
            sequence.accept(&Frame {
                length: 1,
                ..complete(2, 5)
            }),
            bad
        );
        assert_eq!(sequence.accept(&complete(2, 5)), Ok(()));
    }
}
