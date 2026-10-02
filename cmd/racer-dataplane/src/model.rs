//! Shared semantic values. This layer imports neither I/O nor read policy.

use crate::{
    error::{Error, Result},
    runtime::{admission::AdmissionPolicy, deadline::RequestScope},
};
use std::{
    fmt,
    num::NonZeroUsize,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use zeroize::Zeroizing;

// Wire field bounds and canonical decimal encoding.

/// Client/origin v1 maximum size of one field value, in bytes.
pub const MAX_FIELD_BYTES: usize = 8192;
/// Client/origin v1 lengths, offsets, and Unix milliseconds are nonnegative i64s.
pub const MAX_WIRE_INTEGER: u64 = i64::MAX as u64;

/// Canonical decimal: no sign, whitespace, leading zeros, or values above i64::MAX.
pub(crate) fn parse_decimal(value: &[u8]) -> Result<u64> {
    if value.is_empty() || value.len() > 19 || (value.len() > 1 && value[0] == b'0') {
        return Err(Error::InvalidRequest);
    }
    let mut result = 0u64;
    for &byte in value {
        if !byte.is_ascii_digit() {
            return Err(Error::InvalidRequest);
        }
        result = result
            .checked_mul(10)
            .and_then(|n| n.checked_add(u64::from(byte - b'0')))
            .filter(|&n| n <= MAX_WIRE_INTEGER)
            .ok_or(Error::InvalidRequest)?;
    }
    Ok(result)
}

// Identity
// Semantic identities and canonical-encoding boundaries.
//
// Keys are exactly 32 bytes. Strong ETags are opaque version identifiers, not
// content hashes. Placement excludes ETag; page cache and flight identity include it.

pub use racer_control_wire::{CacheId, ClusterId, MembershipVersion, NodeId};
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct CacheKey(pub [u8; 32]);

impl CacheKey {
    /// Decode exactly 64 lowercase hexadecimal bytes without normalization.
    pub fn parse_hex(value: &[u8]) -> Result<Self> {
        if value.len() != 64 {
            return Err(Error::InvalidRequest);
        }
        let nibble = |byte| match byte {
            b'0'..=b'9' => Ok(byte - b'0'),
            b'a'..=b'f' => Ok(byte - b'a' + 10),
            _ => Err(Error::InvalidRequest),
        };
        let mut key = [0; 32];
        for (output, pair) in key.iter_mut().zip(value.chunks_exact(2)) {
            *output = nibble(pair[0])? << 4 | nibble(pair[1])?;
        }
        Ok(Self(key))
    }

    pub fn to_hex(&self) -> String {
        const HEX: &[u8; 16] = b"0123456789abcdef";
        let mut value = String::with_capacity(64);
        for byte in self.0 {
            value.push(char::from(HEX[usize::from(byte >> 4)]));
            value.push(char::from(HEX[usize::from(byte & 15)]));
        }
        value
    }
}

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct StrongEtag(String);

impl StrongEtag {
    #[cfg(test)]
    pub(crate) fn test_value(value: &str) -> Self {
        if value.starts_with('"') {
            Self::parse(value.as_bytes()).unwrap()
        } else {
            Self::parse(format!("\"{value}\"").as_bytes()).unwrap()
        }
    }
    /// Parse the SDK's quoted ASCII strong-tag grammar, preserving exact bytes.
    /// Commas and backslashes inside quotes are literal, not lists or escapes.
    pub fn parse(value: &[u8]) -> Result<Self> {
        if !(2..=MAX_FIELD_BYTES).contains(&value.len())
            || value.first() != Some(&b'"')
            || value.last() != Some(&b'"')
            || !value[1..value.len() - 1]
                .iter()
                .all(|&byte| byte == 0x21 || (0x23..=0x7e).contains(&byte))
        {
            return Err(Error::InvalidRequest);
        }
        let value = std::str::from_utf8(value).map_err(|_| Error::InvalidRequest)?;
        Ok(Self(value.to_owned()))
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }

    pub fn as_bytes(&self) -> &[u8] {
        self.0.as_bytes()
    }
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct PageNumber(pub u64);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct RequestId(pub [u8; 16]);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct AttemptId(pub [u8; 16]);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct TransferId(pub [u8; 16]);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct WorkerId(pub u16);

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct ObjectId {
    pub cache: CacheId,
    pub key: CacheKey,
}

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct ObjectVersion {
    pub object: ObjectId,
    pub etag: StrongEtag,
}

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct PageId {
    pub version: ObjectVersion,
    pub number: PageNumber,
}

// Range
// Overflow-checked single-range normalization and whole-page slice planning.

pub const PAGE_BYTES: u64 = 16 * 1024 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ByteRange {
    Closed { first: u64, last: u64 },
    From(u64),
    Suffix(u64),
}

/// Inclusive start and exclusive end, validated against one version's length.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ResolvedRange {
    start: u64,
    end: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PageSlice {
    pub page: PageNumber,
    pub offset: u32,
    pub length: u32,
}

impl ByteRange {
    /// Parse exactly one canonical SDK byte range, without trimming or merging.
    pub fn parse(value: &[u8]) -> Result<Self> {
        let bounds = value.strip_prefix(b"bytes=").ok_or(Error::InvalidRange)?;
        let separator = bounds
            .iter()
            .position(|&b| b == b'-')
            .ok_or(Error::InvalidRange)?;
        let (first, rest) = bounds.split_at(separator);
        let last = &rest[1..];
        let decimal = |bytes| parse_decimal(bytes).map_err(|_| Error::InvalidRange);
        match (first.is_empty(), last.is_empty()) {
            (true, true) => Err(Error::InvalidRange),
            (true, false) => Self::suffix(decimal(last)?),
            (false, true) => Self::from(decimal(first)?),
            (false, false) => Self::closed(decimal(first)?, decimal(last)?),
        }
    }

    pub fn closed(first: u64, last: u64) -> Result<Self> {
        let range = Self::Closed { first, last };
        range.validate()?;
        Ok(range)
    }

    pub fn from(first: u64) -> Result<Self> {
        let range = Self::From(first);
        range.validate()?;
        Ok(range)
    }

    /// A zero suffix is syntactically valid, but never satisfiable.
    pub fn suffix(length: u64) -> Result<Self> {
        let range = Self::Suffix(length);
        range.validate()?;
        Ok(range)
    }

    /// Public enum variants can bypass the constructors; validate at every boundary.
    pub fn validate(self) -> Result<()> {
        match self {
            Self::Closed { first, last } if first <= last && last <= MAX_WIRE_INTEGER => Ok(()),
            Self::From(first) if first <= MAX_WIRE_INTEGER => Ok(()),
            Self::Suffix(length) if length <= MAX_WIRE_INTEGER => Ok(()),
            _ => Err(Error::InvalidRange),
        }
    }

    pub fn resolve(self, object_length: u64) -> Result<ResolvedRange> {
        self.validate()?;
        if object_length > MAX_WIRE_INTEGER {
            return Err(Error::InvalidRange);
        }
        if object_length == 0 {
            return Err(Error::UnsatisfiableRange);
        }
        let (start, end) = match self {
            Self::Closed { first, last } => (first, (last + 1).min(object_length)),
            Self::From(first) => (first, object_length),
            Self::Suffix(0) => return Err(Error::UnsatisfiableRange),
            Self::Suffix(length) => (object_length.saturating_sub(length), object_length),
        };
        if start >= object_length {
            return Err(Error::UnsatisfiableRange);
        }
        Ok(ResolvedRange { start, end })
    }

    pub fn to_header(self) -> Result<String> {
        self.validate()?;
        Ok(match self {
            Self::Closed { first, last } => format!("bytes={first}-{last}"),
            Self::From(first) => format!("bytes={first}-"),
            Self::Suffix(length) => format!("bytes=-{length}"),
        })
    }
}

impl ResolvedRange {
    pub fn start(&self) -> u64 {
        self.start
    }

    /// Exclusive byte offset.
    pub fn end(&self) -> u64 {
        self.end
    }

    pub fn len(&self) -> u64 {
        self.end - self.start
    }

    /// Resolved ranges always contain at least one byte.
    pub fn is_empty(&self) -> bool {
        false
    }

    pub fn first_page(&self) -> PageNumber {
        PageNumber(self.start / PAGE_BYTES)
    }

    pub fn last_page(&self) -> PageNumber {
        PageNumber((self.end - 1) / PAGE_BYTES)
    }

    /// Produce the next slice, not an allocation proportional to object length.
    pub fn slice_at(&self, page: PageNumber) -> Result<Option<PageSlice>> {
        let page_start = page.0.checked_mul(PAGE_BYTES).ok_or(Error::InvalidRange)?;
        if page_start >= self.end {
            return Ok(None);
        }
        // page_start is now below the validated signed-63-bit range end.
        let start = self.start.max(page_start);
        let end = self.end.min(page_start + PAGE_BYTES);
        if start >= end {
            return Ok(None);
        }
        Ok(Some(PageSlice {
            page,
            offset: (start - page_start) as u32,
            length: (end - start) as u32,
        }))
    }
}

// Content type
// Bounded ASCII MIME metadata, separate from HTTP transport framing.

pub const CONTENT_TYPE_HEADER: &str = "Racer-Content-Type";
pub const MAX_CONTENT_TYPE_BYTES: usize = 256;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ContentType(String);

impl ContentType {
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        if bytes.is_empty()
            || bytes.len() > MAX_CONTENT_TYPE_BYTES
            || bytes.iter().any(|b| !(0x20..=0x7e).contains(b))
            || bytes.first() == Some(&b' ')
            || bytes.last() == Some(&b' ')
        {
            return Err(Error::InvalidRequest);
        }
        let mut rest = bytes;
        token(&mut rest)?;
        consume(&mut rest, b'/')?;
        token(&mut rest)?;
        let mut parameters: Vec<&[u8]> = Vec::new();
        while !rest.is_empty() {
            spaces(&mut rest);
            consume(&mut rest, b';')?;
            spaces(&mut rest);
            let name = token(&mut rest)?;
            if parameters.iter().any(|old| old.eq_ignore_ascii_case(name)) {
                return Err(Error::InvalidRequest);
            }
            parameters.push(name);
            spaces(&mut rest);
            consume(&mut rest, b'=')?;
            spaces(&mut rest);
            if rest.first() == Some(&b'"') {
                rest = &rest[1..];
                loop {
                    let byte = *rest.first().ok_or(Error::InvalidRequest)?;
                    rest = &rest[1..];
                    match byte {
                        b'"' => break,
                        b'\\' => {
                            rest = rest.get(1..).ok_or(Error::InvalidRequest)?;
                        }
                        _ => {}
                    }
                }
            } else {
                token(&mut rest)?;
            }
        }
        Ok(Self(
            String::from_utf8(bytes.to_vec()).map_err(|_| Error::InvalidRequest)?,
        ))
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
    pub fn as_bytes(&self) -> &[u8] {
        self.0.as_bytes()
    }
}
fn spaces(rest: &mut &[u8]) {
    while rest.first() == Some(&b' ') {
        *rest = &rest[1..];
    }
}
fn consume(rest: &mut &[u8], byte: u8) -> Result<()> {
    if rest.first() != Some(&byte) {
        return Err(Error::InvalidRequest);
    }
    *rest = &rest[1..];
    Ok(())
}
fn token<'a>(rest: &mut &'a [u8]) -> Result<&'a [u8]> {
    let length = rest
        .iter()
        .take_while(|b| b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(b))
        .count();
    if length == 0 {
        return Err(Error::InvalidRequest);
    }
    let value = &rest[..length];
    *rest = &rest[length..];
    Ok(value)
}

// Metadata
// Versioned metadata. TTL controls new unpinned admission, never page eviction.

/// Absolute Unix-millisecond deadline in 0..=i64::MAX, never a stream deadline.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ExpiresAt(std::time::SystemTime);

impl ExpiresAt {
    #[cfg(test)]
    pub(crate) fn test_time(time: SystemTime) -> Self {
        Self::from_unix_millis(
            time.duration_since(UNIX_EPOCH)
                .unwrap()
                .as_millis()
                .try_into()
                .unwrap(),
        )
        .unwrap()
    }

    pub fn from_system_time(time: SystemTime) -> Result<Self> {
        let value = Self(time);
        value.to_unix_millis()?;
        Ok(value)
    }

    pub fn as_system_time(self) -> SystemTime {
        self.0
    }

    pub fn from_unix_millis(milliseconds: u64) -> Result<Self> {
        if milliseconds > MAX_WIRE_INTEGER {
            return Err(Error::InvalidRequest);
        }
        UNIX_EPOCH
            .checked_add(Duration::from_millis(milliseconds))
            .map(Self)
            .ok_or(Error::InvalidRequest)
    }

    /// Reject pre-epoch, overflowing, or sub-millisecond times without rounding.
    pub fn to_unix_millis(self) -> Result<u64> {
        let duration = self
            .0
            .duration_since(UNIX_EPOCH)
            .map_err(|_| Error::InvalidRequest)?;
        if duration.subsec_nanos() % 1_000_000 != 0
            || duration.as_millis() > u128::from(MAX_WIRE_INTEGER)
        {
            return Err(Error::InvalidRequest);
        }
        Ok(duration.as_millis() as u64)
    }

    pub fn parse(value: &[u8]) -> Result<Self> {
        Self::from_unix_millis(parse_decimal(value)?)
    }

    pub fn to_header(self) -> Result<String> {
        Ok(self.to_unix_millis()?.to_string())
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ObjectMetadata {
    pub content_type: Option<ContentType>,
    pub version: ObjectVersion,
    pub length: u64,
    pub expires_at: ExpiresAt,
}

/// Immutable descriptor keyed by the complete object version, never by object alone.
/// Retained by page copies independently of the bounded page-zero metadata catalog.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct VersionMetadata {
    pub content_type: Option<ContentType>,
    pub version: ObjectVersion,
    pub length: u64,
}

/// Page-zero owner's volatile current-version pointer. Page hits and pinned probes
/// cannot publish this value; only a fresh metadata revalidation may do so.
/// It is deliberately absent from checkpoints and invalidated on clock uncertainty.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CurrentVersion {
    pub version: ObjectVersion,
    pub expires_at: ExpiresAt,
}

impl ObjectMetadata {
    pub fn validate(&self) -> Result<()> {
        if self.length > MAX_WIRE_INTEGER {
            return Err(Error::InvalidRequest);
        }
        self.expires_at.to_unix_millis()?;
        Ok(())
    }

    pub fn immutable(&self) -> VersionMetadata {
        VersionMetadata {
            content_type: self.content_type.clone(),
            version: self.version.clone(),
            length: self.length,
        }
    }
}

impl VersionMetadata {
    /// MIME metadata is immutable, including its absence.
    pub fn compatible(&self, other: &Self) -> bool {
        self.version == other.version
            && self.length == other.length
            && self.content_type == other.content_type
    }
    /// A recovered or retained immutable descriptor can answer a pin, but carries
    /// no reusable freshness claim. Never infer total length from a page's bytes.
    pub fn for_pin(&self) -> ObjectMetadata {
        ObjectMetadata {
            content_type: self.content_type.clone(),
            version: self.version.clone(),
            length: self.length,
            expires_at: ExpiresAt(UNIX_EPOCH),
        }
    }

    /// Structural consistency only, not authentication of origin/peer/disk input.
    pub fn validate_page(&self, envelope: &PageEnvelope) -> Result<()> {
        envelope.validate()?;
        if self.page_length(&envelope.page)? != envelope.plaintext_length {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }

    pub fn page_length(&self, page: &PageId) -> Result<u32> {
        let start = page
            .number
            .0
            .checked_mul(PAGE_BYTES)
            .ok_or(Error::CorruptRecord)?;
        if self.length > MAX_WIRE_INTEGER || page.version != self.version || start >= self.length {
            return Err(Error::CorruptRecord);
        }
        Ok((self.length - start).min(PAGE_BYTES) as u32)
    }
}

impl CurrentVersion {
    /// Join the freshness pointer to exactly its immutable descriptor. Expiry is
    /// inclusive; a zero-TTL refresh can admit its own waiters, not a cache hit.
    pub fn resolve(
        &self,
        descriptor: &VersionMetadata,
        now: SystemTime,
    ) -> Result<Option<ObjectMetadata>> {
        if self.version != descriptor.version {
            return Err(Error::VersionUnavailable);
        }
        if descriptor.length > MAX_WIRE_INTEGER {
            return Err(Error::CorruptRecord);
        }
        self.expires_at.to_unix_millis()?;
        Ok((now < self.expires_at.0).then(|| ObjectMetadata {
            content_type: descriptor.content_type.clone(),
            version: descriptor.version.clone(),
            length: descriptor.length,
            expires_at: self.expires_at,
        }))
    }
}

/// Distinguishes freshness admission from an explicit immutable-version pin.
#[derive(Clone, Debug)]
pub enum MetadataSelector {
    Fresh,
    Pinned(StrongEtag),
}

// Envelope
// Immutable page encryption descriptors shared by memory, peers, and storage.
//
// Disk padding is outside the authenticated ciphertext length. HTTP signatures
// authenticate this descriptor; XChaCha20-Poly1305 authenticates page bytes.

pub const AEAD_TAG_BYTES: u32 = 16;

#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct KeyId(pub [u8; 16]);
impl KeyId {
    /// Controller-issued epoch namespace. Zero and opaque IDs are invalid.
    pub(crate) fn generation(self) -> Option<u64> {
        (self.0[..4] == *b"RKG1")
            .then(|| u64::from_be_bytes(self.0[4..12].try_into().expect("generation bytes")))
            .filter(|generation| *generation != 0)
    }

    /// Construct an epoch-bound ID with a controller-selected uniqueness suffix.
    pub fn from_generation(generation: u64, suffix: u32) -> Result<Self> {
        if generation == 0 {
            return Err(Error::InvalidConfiguration);
        }
        let mut bytes = [0; 16];
        bytes[..4].copy_from_slice(b"RKG1");
        bytes[4..12].copy_from_slice(&generation.to_be_bytes());
        bytes[12..].copy_from_slice(&suffix.to_be_bytes());
        Ok(Self(bytes))
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Nonce(pub [u8; 24]);

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PageEnvelope {
    pub page: PageId,
    pub key_id: KeyId,
    pub nonce: Nonce,
    pub plaintext_length: u32,
    /// Includes the AEAD tag, excludes disk alignment padding and record headers.
    pub ciphertext_length: u32,
}

impl PageEnvelope {
    /// Structural bounds only. Identity and final-page length require the version
    /// descriptor; authenticity requires successful AEAD verification.
    pub fn validate(&self) -> Result<()> {
        if self.plaintext_length == 0
            || u64::from(self.plaintext_length) > PAGE_BYTES
            || self.plaintext_length.checked_add(AEAD_TAG_BYTES) != Some(self.ciphertext_length)
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }
}

// Context
// Request-scoped adapter context, deliberately absent from cache identities.
//
// Carry the exact key and opaque Racer-Metadata value to origin. Authorization
// is an opaque upstream credential, not Racer authorization. It travels encrypted
// across peers and as a normal header on the local origin Unix socket. Never put
// either raw or encrypted credentials in page/metadata caches, checkpoints, logs,
// metrics, or durable retry queues. Relays preserve encrypted credentials unopened.

pub const METADATA_HEADER: &str = "Racer-Metadata";

/// Sensitive bytes with redacted diagnostics and zeroization on drop. Not Clone.
pub struct Authorization {
    bytes: Zeroizing<Vec<u8>>,
}

impl Authorization {
    /// Validate HTTP field syntax/size without interpreting the credential scheme.
    pub fn from_header(bytes: &[u8]) -> Result<Self> {
        validate_opaque(bytes)?;
        Ok(Self {
            bytes: Zeroizing::new(bytes.to_vec()),
        })
    }

    /// Expose only for encryption or a local adapter write, never for diagnostics.
    pub fn expose_for_origin(&self) -> &[u8] {
        self.bytes.as_slice()
    }
}

/// Preserve one bounded field value. The HTTP parser must reject duplicate fields.
pub struct OpaqueMetadata {
    bytes: Zeroizing<Vec<u8>>,
}

impl OpaqueMetadata {
    pub fn from_header(bytes: &[u8]) -> Result<Self> {
        validate_opaque(bytes)?;
        Ok(Self {
            bytes: Zeroizing::new(bytes.to_vec()),
        })
    }

    pub fn as_header(&self) -> &[u8] {
        self.bytes.as_slice()
    }
}

fn validate_opaque(bytes: &[u8]) -> Result<()> {
    if bytes.is_empty()
        || bytes.len() > MAX_FIELD_BYTES
        || bytes.first() == Some(&b' ')
        || bytes.last() == Some(&b' ')
        || bytes.iter().any(|&byte| byte < 0x20 || byte == 0x7f)
    {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

impl fmt::Debug for Authorization {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("Authorization([redacted])")
    }
}

impl fmt::Debug for OpaqueMetadata {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("OpaqueMetadata([redacted])")
    }
}

/// One request owns the raw context and lends it to origin writes and sealing.
/// Retrying/fanning out must not require duplicating secrets:
/// ```compile_fail
/// use racer_dataplane::model::OriginContext;
/// fn duplicate(origin: OriginContext) { let _copy = origin.clone(); }
/// ```
pub struct OriginContext {
    pub object: ObjectId,
    pub metadata: Option<OpaqueMetadata>,
    pub authorization: Option<Authorization>,
}

impl fmt::Debug for OriginContext {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("OriginContext([redacted])")
    }
}

/// Separate AEAD domain from page encryption. Bind to request/attempt/object and
/// metadata using canonical AAD; retries reseal with a fresh cryptographic nonce
/// rather than change bound fields or reuse this ciphertext for a new attempt.
/// Any eligible origin-fetching node can open this cache-scoped credential envelope.
pub struct EncryptedAuthorization {
    pub key_id: KeyId,
    pub nonce: Nonce,
    pub ciphertext: Vec<u8>,
}

impl fmt::Debug for EncryptedAuthorization {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("EncryptedAuthorization([redacted])")
    }
}

/// Owned per-attempt envelope, with no borrow of the raw request context. Relays
/// preserve it unopened. Local quota and scope are never serialized on the wire.
/// Neither envelope nor quota can be cloned to bypass per-attempt admission:
/// ```compile_fail
/// use racer_dataplane::model::PeerOriginContext;
/// fn duplicate(envelope: PeerOriginContext) { let _copy = envelope.clone(); }
/// ```
/// Wire decoding must also admit its allocations before constructing this owner;
/// callers cannot construct an uncharged envelope using only its wire fields:
/// ```compile_fail
/// use racer_dataplane::model::{PeerOriginContext, ObjectId, RequestId, AttemptId};
/// fn uncharged(object: ObjectId, request: RequestId, attempt: AttemptId) {
///     let _envelope = PeerOriginContext {
///         object, request, attempt, metadata: None, authorization: None,
///     };
/// }
/// ```
pub struct PeerOriginContext {
    pub object: ObjectId,
    pub request: RequestId,
    pub attempt: AttemptId,
    pub metadata: Option<OpaqueMetadata>,
    pub authorization: Option<EncryptedAuthorization>,
    /// Charge all owned field allocations, including ciphertext/tag and metadata.
    /// Transport retains this owner until all I/O using those fields is fenced.
    pub(crate) reservation: flow_control::Charge<AdmissionPolicy>,
    pub(crate) scope: RequestScope,
}

impl PeerOriginContext {
    /// Original deadline/shared cancellation, never reset by retry or fanout.
    pub fn scope(&self) -> &RequestScope {
        &self.scope
    }
}

// Limits
// Independent bounded-resource dimensions. Validation precedes resource creation.

#[derive(Clone, Debug)]
pub struct Limits {
    pub plaintext_bytes: NonZeroUsize,
    pub ciphertext_bytes: NonZeroUsize,
    pub dirty_bytes: NonZeroUsize,
    pub registered_bytes: NonZeroUsize,
    pub request_context_bytes: NonZeroUsize,
    pub flights: NonZeroUsize,
    pub waiters_per_flight: NonZeroUsize,
    pub queue_entries: NonZeroUsize,
    pub connections_per_neighbor: NonZeroUsize,
    pub client_connections: NonZeroUsize,
    pub pipes: NonZeroUsize,
    pub range_window_pages: NonZeroUsize,
    pub header_bytes: NonZeroUsize,
    pub cached_rankings: NonZeroUsize,
    pub cached_paths: NonZeroUsize,
    /// Old live membership generations in addition to current; cache-only
    /// publications reuse a generation and do not consume another slot.
    pub retained_snapshots: NonZeroUsize,
    pub metadata_entries: NonZeroUsize,
    pub relay_transfers: NonZeroUsize,
}

#[derive(Clone, Copy, Debug)]
pub enum ResourceClass {
    Plaintext,
    Ciphertext,
    DirtyCiphertext,
    Registered,
    RequestContext,
    Flight,
    Waiter,
    Connection,
    Pipe,
    ControlProgress,
    Relay,
    IngressConnection,
    OutboundConnection,
    ControlConnection,
}

impl flow_control::Class for ResourceClass {
    const COUNT: usize = 14;
    fn index(self) -> usize {
        self as usize
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Identity boundaries.
    #[test]
    fn cache_key_canonical_hex_vectors() {
        for (bytes, wire) in [
            (
                [0; 32],
                "0000000000000000000000000000000000000000000000000000000000000000",
            ),
            (
                [0xff; 32],
                "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
            ),
            (
                std::array::from_fn(|i| i as u8),
                "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
            ),
        ] {
            assert_eq!(CacheKey::parse_hex(wire.as_bytes()), Ok(CacheKey(bytes)));
            assert_eq!(CacheKey(bytes).to_hex(), wire);
        }
        // Cover all byte values, including every high and low nibble.
        for start in (0..256).step_by(32) {
            let key = CacheKey(std::array::from_fn(|i| (start + i) as u8));
            assert_eq!(CacheKey::parse_hex(key.to_hex().as_bytes()), Ok(key));
        }
        for byte in 0..=u8::MAX {
            let key = CacheKey([byte; 32]);
            assert_eq!(CacheKey::parse_hex(key.to_hex().as_bytes()), Ok(key));
        }
    }

    #[test]
    fn cache_key_rejects_noncanonical_hex() {
        for length in [0, 1, 63, 65, 128] {
            assert_eq!(
                CacheKey::parse_hex(&vec![b'0'; length]),
                Err(Error::InvalidRequest)
            );
        }
        for invalid in [
            b'A', b'F', b'G', b'g', b'/', b':', b'%', b' ', b'\t', b'\n', 0, 0x80, 0xff,
        ] {
            assert_eq!(
                CacheKey::parse_hex(&[invalid; 64]),
                Err(Error::InvalidRequest)
            );
            for position in [0, 1, 62, 63] {
                let mut value = [b'0'; 64];
                value[position] = invalid;
                assert_eq!(CacheKey::parse_hex(&value), Err(Error::InvalidRequest));
            }
        }
        assert_eq!(
            CacheKey::parse_hex(format!("0x{}", "0".repeat(62)).as_bytes()),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn strong_tags_preserve_quotes_and_literal_punctuation() {
        for value in [br#""""#.as_slice(), br#""a,b""#, br#""a\b""#, br#""!#~""#] {
            let tag = StrongEtag::parse(value).unwrap();
            assert_eq!(tag.as_bytes(), value);
            assert_eq!(tag.as_str().as_bytes(), value);
        }
        let limit = format!("\"{}\"", "x".repeat(MAX_FIELD_BYTES - 2));
        assert_eq!(StrongEtag::parse(limit.as_bytes()).unwrap().as_str(), limit);
    }

    #[test]
    fn rejects_weak_lists_whitespace_controls_and_non_ascii() {
        for value in [
            b"".as_slice(),
            b"*",
            br#"W/"v""#,
            br#""a", "b""#,
            br#""a"b""#,
            br#""a b""#,
            b"\"\t\"",
            b"\"\x80\"",
            b"\"a\n\"",
            b" \"v\"",
            b"\"v\" ",
            b"\"",
            b"v",
            b"\"\x7f\"",
        ] {
            assert_eq!(StrongEtag::parse(value), Err(Error::InvalidRequest));
        }
        let oversized = format!("\"{}\"", "x".repeat(MAX_FIELD_BYTES - 1));
        assert_eq!(
            StrongEtag::parse(oversized.as_bytes()),
            Err(Error::InvalidRequest)
        );
    }

    // Range boundaries.
    #[test]
    fn sdk_range_vectors_resolve_and_round_trip() {
        for (wire, size, expected) in [
            ("bytes=0-0", 1, Ok((0, 1))),
            ("bytes=0-99", 2, Ok((0, 2))),
            ("bytes=1-", 2, Ok((1, 2))),
            ("bytes=-99", 2, Ok((0, 2))),
            ("bytes=-1", 2, Ok((1, 2))),
            ("bytes=-0", 2, Err(Error::UnsatisfiableRange)),
            ("bytes=0-0", 0, Err(Error::UnsatisfiableRange)),
            ("bytes=2-", 2, Err(Error::UnsatisfiableRange)),
            (
                "bytes=0-9223372036854775807",
                MAX_WIRE_INTEGER,
                Ok((0, MAX_WIRE_INTEGER)),
            ),
            (
                "bytes=9223372036854775807-",
                MAX_WIRE_INTEGER,
                Err(Error::UnsatisfiableRange),
            ),
            (
                "bytes=-9223372036854775807",
                MAX_WIRE_INTEGER,
                Ok((0, MAX_WIRE_INTEGER)),
            ),
            ("bytes=0-0", MAX_WIRE_INTEGER + 1, Err(Error::InvalidRange)),
        ] {
            let range = ByteRange::parse(wire.as_bytes()).unwrap();
            assert_eq!(range.to_header().unwrap(), wire);
            assert_eq!(
                range.resolve(size).map(|r| (r.start(), r.end())),
                expected,
                "{wire}"
            );
        }
    }

    #[test]
    fn malformed_ranges_and_direct_invalid_variants_are_rejected() {
        for wire in [
            "",
            "bytes=-",
            "bytes=1-0",
            "bytes=00-1",
            "bytes=0-01",
            "bytes=+1-",
            "bytes=0-1,2-3",
            "bytes=0- 1",
            "bytes=0-9223372036854775808",
            "Bytes=0-1",
            "bytes=1--2",
            "bytes=0-1 ",
        ] {
            assert_eq!(
                ByteRange::parse(wire.as_bytes()),
                Err(Error::InvalidRange),
                "{wire}"
            );
        }
        for range in [
            ByteRange::Closed { first: 2, last: 1 },
            ByteRange::From(u64::MAX),
            ByteRange::Suffix(u64::MAX),
            ByteRange::Closed {
                first: 0,
                last: u64::MAX,
            },
        ] {
            assert_eq!(range.resolve(10), Err(Error::InvalidRange));
            assert_eq!(range.to_header(), Err(Error::InvalidRange));
        }
    }

    #[test]
    fn page_slices_cover_only_requested_bytes_and_short_final_page() {
        let range = ByteRange::From(PAGE_BYTES - 2)
            .resolve(2 * PAGE_BYTES + 3)
            .unwrap();
        assert_eq!(range.first_page(), PageNumber(0));
        assert_eq!(range.last_page(), PageNumber(2));
        assert_eq!(range.len(), PAGE_BYTES + 5);
        assert!(!range.is_empty());
        for (page, offset, length) in [
            (0, PAGE_BYTES as u32 - 2, 2),
            (1, 0, PAGE_BYTES as u32),
            (2, 0, 3),
        ] {
            assert_eq!(
                range.slice_at(PageNumber(page)),
                Ok(Some(PageSlice {
                    page: PageNumber(page),
                    offset,
                    length
                }))
            );
        }
        assert_eq!(range.slice_at(PageNumber(3)), Ok(None));
        assert_eq!(
            range.slice_at(PageNumber(u64::MAX)),
            Err(Error::InvalidRange)
        );
        let range = ByteRange::closed(PAGE_BYTES + 1, PAGE_BYTES + 1)
            .unwrap()
            .resolve(PAGE_BYTES + 2)
            .unwrap();
        assert_eq!(range.slice_at(PageNumber(0)), Ok(None));
        assert_eq!(range.slice_at(PageNumber(1)).unwrap().unwrap().length, 1);
    }

    #[test]
    fn maximum_length_page_plan_is_constant_space_and_does_not_overflow() {
        let range = ByteRange::From(0).resolve(MAX_WIRE_INTEGER).unwrap();
        let last = range.slice_at(range.last_page()).unwrap().unwrap();
        assert_eq!(last.offset, 0);
        assert_eq!(u64::from(last.length), MAX_WIRE_INTEGER % PAGE_BYTES);
        assert_eq!(
            range.slice_at(PageNumber(range.last_page().0 + 1)),
            Ok(None)
        );
    }

    // Content type boundaries.
    #[test]
    fn bounded_mime_values_preserve_bytes_and_reject_malformed_metadata() {
        for value in [
            "application/vnd.oci.image.manifest.v1+json",
            "text/plain; charset=utf-8",
            "text/plain; x=\"a;b\\\"c\"",
        ] {
            assert_eq!(
                ContentType::parse(value.as_bytes()).unwrap().as_str(),
                value
            );
        }
        for value in [
            "",
            "text",
            "text/",
            "/plain",
            " text/plain",
            "text/plain ",
            "text/plain, text/html",
            "text/plain;",
            "text/plain;x",
            "text/plain;x=",
            "text/plain;x=\"",
            "text/plain;x=a;X=b",
            "text/plain\r\nx:y",
            "text/\tplain",
            "text/pläin",
        ] {
            assert!(ContentType::parse(value.as_bytes()).is_err(), "{value:?}");
        }
        assert!(ContentType::parse(format!("a/{}", "b".repeat(254)).as_bytes()).is_ok());
        assert!(ContentType::parse(format!("a/{}", "b".repeat(255)).as_bytes()).is_err());
    }

    // Metadata boundaries.
    #[test]
    fn expiry_round_trips_sdk_epoch_milliseconds_including_signed63_maximum() {
        for milliseconds in [0, 1, 999, 1000, 123_456_789, MAX_WIRE_INTEGER] {
            let value = milliseconds.to_string();
            let expiry = ExpiresAt::parse(value.as_bytes()).unwrap();
            assert_eq!(expiry.to_unix_millis(), Ok(milliseconds));
            assert_eq!(expiry.to_header(), Ok(value));
        }
    }

    #[test]
    fn expiry_rejects_noncanonical_precision_and_overflow() {
        for value in [
            "",
            "-1",
            "+1",
            "01",
            " 1",
            "1 ",
            "1.0",
            "9223372036854775808",
            "18446744073709551616",
        ] {
            assert_eq!(
                ExpiresAt::parse(value.as_bytes()),
                Err(Error::InvalidRequest)
            );
        }
        assert_eq!(
            ExpiresAt::from_unix_millis(MAX_WIRE_INTEGER + 1),
            Err(Error::InvalidRequest)
        );
        for time in [
            UNIX_EPOCH - Duration::from_millis(1),
            UNIX_EPOCH + Duration::from_nanos(1),
            UNIX_EPOCH + Duration::from_millis(MAX_WIRE_INTEGER + 1),
        ] {
            assert_eq!(
                ExpiresAt::from_system_time(time),
                Err(Error::InvalidRequest)
            );
        }
    }

    #[test]
    fn metadata_validation_rejects_invalid_public_numeric_fields() {
        let mut metadata = descriptor("v1", MAX_WIRE_INTEGER).for_pin();
        assert_eq!(metadata.validate(), Ok(()));
        metadata.length += 1;
        assert_eq!(metadata.validate(), Err(Error::InvalidRequest));
        metadata.length = 0;
        metadata.expires_at = ExpiresAt(UNIX_EPOCH + Duration::from_nanos(1));
        assert_eq!(metadata.validate(), Err(Error::InvalidRequest));
    }

    #[test]
    fn current_version_rejects_invalid_descriptors_before_fresh_admission() {
        let descriptor = descriptor("v1", MAX_WIRE_INTEGER + 1);
        let mut current = CurrentVersion {
            version: descriptor.version.clone(),
            expires_at: ExpiresAt(UNIX_EPOCH + Duration::from_secs(1)),
        };
        assert_eq!(
            current.resolve(&descriptor, UNIX_EPOCH),
            Err(Error::CorruptRecord)
        );
        let valid = VersionMetadata {
            length: 1,
            ..descriptor
        };
        current.expires_at = ExpiresAt(UNIX_EPOCH + Duration::from_nanos(1));
        assert_eq!(
            current.resolve(&valid, UNIX_EPOCH),
            Err(Error::InvalidRequest)
        );
    }

    pub(crate) fn descriptor(etag: &str, length: u64) -> VersionMetadata {
        VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length,
        }
    }

    #[test]
    fn freshness_never_substitutes_another_versions_length() {
        let old = descriptor("old", 7);
        let new = descriptor("new", 500);
        let now = UNIX_EPOCH + Duration::from_secs(100);
        let current = CurrentVersion {
            version: new.version.clone(),
            expires_at: ExpiresAt(now + Duration::from_secs(1)),
        };
        assert_eq!(current.resolve(&old, now), Err(Error::VersionUnavailable));
        assert_eq!(current.resolve(&new, now).unwrap().unwrap().length, 500);
        assert_eq!(old.for_pin().length, 7);
        assert_eq!(old.for_pin().expires_at, ExpiresAt(UNIX_EPOCH));
    }

    #[test]
    fn expired_and_zero_ttl_pointers_cannot_admit_fresh_reads() {
        let descriptor = descriptor("v1", 0);
        let deadline = UNIX_EPOCH + Duration::from_secs(100);
        let current = CurrentVersion {
            version: descriptor.version.clone(),
            expires_at: ExpiresAt(deadline),
        };
        for now in [deadline, deadline + Duration::from_secs(1)] {
            assert_eq!(current.resolve(&descriptor, now).unwrap(), None);
        }
        assert_eq!(descriptor.for_pin().length, 0);
    }

    #[test]
    fn page_bounds_use_total_version_length_and_reject_empty_overflow_and_mismatch() {
        let descriptor = descriptor("v1", PAGE_BYTES + 3);
        let mut envelope = PageEnvelope {
            page: PageId {
                version: descriptor.version.clone(),
                number: PageNumber(1),
            },
            key_id: KeyId([0; 16]),
            nonce: Nonce([0; 24]),
            plaintext_length: 3,
            ciphertext_length: 19,
        };
        assert_eq!(descriptor.validate_page(&envelope), Ok(()));
        envelope.ciphertext_length = 18;
        assert_eq!(
            descriptor.validate_page(&envelope),
            Err(Error::CorruptRecord)
        );
        envelope.ciphertext_length = 19;
        envelope.plaintext_length = 4;
        assert_eq!(
            descriptor.validate_page(&envelope),
            Err(Error::CorruptRecord)
        );
        envelope.plaintext_length = 3;
        for number in [2, u64::MAX] {
            envelope.page.number = PageNumber(number);
            assert_eq!(
                descriptor.validate_page(&envelope),
                Err(Error::CorruptRecord)
            );
        }
        envelope.page.number = PageNumber(0);
        let empty = VersionMetadata {
            length: 0,
            ..descriptor.clone()
        };
        assert_eq!(empty.validate_page(&envelope), Err(Error::CorruptRecord));
        envelope.page.version.etag = StrongEtag::test_value("other");
        assert_eq!(
            descriptor.validate_page(&envelope),
            Err(Error::CorruptRecord)
        );
    }

    // Envelope boundaries.
    #[test]
    fn envelope_enforces_nonempty_bounded_plaintext_and_exact_tag_length() {
        let mut envelope = PageEnvelope {
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId("cache".into()),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::parse(b"\"v1\"").unwrap(),
                },
                number: PageNumber(0),
            },
            key_id: KeyId([0; 16]),
            nonce: Nonce([0; 24]),
            plaintext_length: 1,
            ciphertext_length: 17,
        };
        for length in [1, PAGE_BYTES as u32] {
            envelope.plaintext_length = length;
            envelope.ciphertext_length = length + AEAD_TAG_BYTES;
            assert_eq!(envelope.validate(), Ok(()));
            envelope.ciphertext_length += 1;
            assert_eq!(envelope.validate(), Err(Error::CorruptRecord));
        }
        for (plaintext, ciphertext) in [
            (0, 16),
            (PAGE_BYTES as u32 + 1, PAGE_BYTES as u32 + 17),
            (u32::MAX, 15),
        ] {
            envelope.plaintext_length = plaintext;
            envelope.ciphertext_length = ciphertext;
            assert_eq!(envelope.validate(), Err(Error::CorruptRecord));
        }
    }

    // Context boundaries.
    #[test]
    fn opaque_context_round_trips_non_utf8_without_normalization() {
        let bytes = b"opaque,  credential\\\"\xff";
        let authorization = Authorization::from_header(bytes).unwrap();
        let metadata = OpaqueMetadata::from_header(bytes).unwrap();
        assert_eq!(authorization.expose_for_origin(), bytes);
        assert_eq!(metadata.as_header(), bytes);
        let context = OriginContext {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([0; 32]),
            },
            authorization: Some(authorization),
            metadata: Some(metadata),
        };
        assert_eq!(format!("{context:?}"), "OriginContext([redacted])");
        assert_eq!(
            format!("{:#?}", context.authorization.unwrap()),
            "Authorization([redacted])"
        );
        assert_eq!(
            format!("{:#?}", context.metadata.unwrap()),
            "OpaqueMetadata([redacted])"
        );
    }

    #[test]
    fn context_rejects_present_empty_controls_padding_and_oversize() {
        for bytes in [
            b"".as_slice(),
            b" leading",
            b"trailing ",
            b"a\tb",
            b"a\rb",
            b"a\nb",
            b"a\0b",
            b"a\x7fb",
        ] {
            assert!(matches!(
                Authorization::from_header(bytes),
                Err(Error::InvalidRequest)
            ));
            assert!(matches!(
                OpaqueMetadata::from_header(bytes),
                Err(Error::InvalidRequest)
            ));
        }
        for length in [MAX_FIELD_BYTES, MAX_FIELD_BYTES + 1] {
            let bytes = vec![b'x'; length];
            assert_eq!(
                Authorization::from_header(&bytes).is_ok(),
                length == MAX_FIELD_BYTES
            );
            assert_eq!(
                OpaqueMetadata::from_header(&bytes).is_ok(),
                length == MAX_FIELD_BYTES
            );
        }
    }

    #[test]
    fn sensitive_storage_uses_zeroizing_owners() {
        // Keep the storage contract checked without reading freed memory.
        fn zeroizing(_: &Zeroizing<Vec<u8>>) {}
        zeroizing(&Authorization::from_header(b"secret").unwrap().bytes);
        zeroizing(&OpaqueMetadata::from_header(b"private").unwrap().bytes);
    }
}
