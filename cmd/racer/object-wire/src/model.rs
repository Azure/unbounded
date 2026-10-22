//! Shared semantic values. This layer imports neither I/O nor read policy.

use crate::Error;
use crate::Result;
use racer_control_wire::CacheId;
use std::time::Duration;
use std::time::SystemTime;
use std::time::UNIX_EPOCH;

// Wire field bounds and canonical decimal encoding.

/// Client/origin v1 maximum size of one field value, in bytes.
pub const MAX_FIELD_BYTES: usize = 8192;
/// Client/origin v1 lengths, offsets, and Unix milliseconds are nonnegative i64s.
pub const MAX_WIRE_INTEGER: u64 = i64::MAX as u64;

/// Canonical decimal: no sign, whitespace, leading zeros, or values above i64::MAX.
pub fn parse_decimal(value: &[u8]) -> Result<u64> {
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

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
/// Canonical 256-bit object key, independent of an immutable version.
pub struct CacheKey(pub [u8; 32]);

impl CacheKey {
    /// Decode exactly 64 lowercase hexadecimal bytes without normalization.
    pub fn parse_hex(value: &[u8]) -> Result<Self> {
        wire_codec::decode_hex(value)
            .map(Self)
            .map_err(|_| Error::InvalidRequest)
    }

    /// Encode the key in the canonical lowercase SDK representation.
    pub fn to_hex(&self) -> String {
        wire_codec::encode_hex(&self.0)
    }
}

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
/// Quoted ASCII version identifier, preserving literal punctuation.
pub struct StrongEtag(String);

impl StrongEtag {
    /// Construct a strong tag fixture, adding quotes when omitted.
    #[cfg(any(test, feature = "test-util"))]
    pub fn test_value(value: &str) -> Self {
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

    /// Borrow the exact quoted tag.
    pub fn as_str(&self) -> &str {
        &self.0
    }

    /// Borrow the exact quoted wire bytes.
    pub fn as_bytes(&self) -> &[u8] {
        self.0.as_bytes()
    }
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
/// Zero-based page index within an immutable version.
pub struct PageNumber(pub u64);

#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
/// End-to-end request correlation identifier.
pub struct RequestId(pub [u8; 16]);

#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
/// One acquisition attempt within a request.
pub struct AttemptId(pub [u8; 16]);

#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
/// One page transfer correlation identifier.
pub struct TransferId(pub [u8; 16]);

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
/// Local worker identifier carried with object operations.
pub struct WorkerId(pub u16);

/// Object identity without an immutable version pin.
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct ObjectId {
    pub cache: CacheId,

    pub key: CacheKey,
}

/// Exact version of one object.
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct ObjectVersion {
    pub object: ObjectId,

    pub etag: StrongEtag,
}

/// Exact page of one immutable object version.
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct PageId {
    pub version: ObjectVersion,

    pub number: PageNumber,
}

// Range
// Overflow-checked single-range normalization and whole-page slice planning.

/// Fixed logical page size; only the final page may be shorter.
pub const PAGE_BYTES: u64 = 16 * 1024 * 1024;

/// One canonical byte-range request, resolved against a version length later.
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

/// Requested portion of a single logical page.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PageSlice {
    pub page: PageNumber,

    pub offset: u32,

    pub length: u32,
}

impl ByteRange {
    /// Parse exactly one canonical SDK byte range, without trimming or merging.
    pub fn parse(value: &[u8]) -> Result<Self> {
        let range = http1::range::ByteRange::parse_with(value, parse_decimal)
            .map_err(|_| Error::InvalidRange)?;
        match range {
            http1::range::ByteRange::Closed { first, last } => Self::closed(first, last),
            http1::range::ByteRange::From(first) => Self::from(first),
            http1::range::ByteRange::Suffix(length) => Self::suffix(length),
        }
    }

    /// Construct an inclusive closed range after checking integer bounds.
    pub fn closed(first: u64, last: u64) -> Result<Self> {
        let range = Self::Closed { first, last };
        range.validate()?;
        Ok(range)
    }

    /// Construct a range from an offset to the eventual object end.
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

    /// Resolve and clamp a range to a nonempty immutable object.
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

    /// Encode a validated range using canonical SDK syntax.
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
    /// Inclusive start offset.
    pub fn start(&self) -> u64 {
        self.start
    }

    /// Exclusive byte offset.
    pub fn end(&self) -> u64 {
        self.end
    }

    /// Number of bytes requested.
    pub fn len(&self) -> u64 {
        self.end - self.start
    }

    /// Resolved ranges always contain at least one byte.
    pub fn is_empty(&self) -> bool {
        false
    }

    /// First page containing requested bytes.
    pub fn first_page(&self) -> PageNumber {
        PageNumber(self.start / PAGE_BYTES)
    }

    /// Last page containing requested bytes.
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

/// Object MIME metadata field, separate from the transport content type.
pub const CONTENT_TYPE_HEADER: &str = "Racer-Content-Type";
/// Maximum object MIME metadata size.
pub const MAX_CONTENT_TYPE_BYTES: usize = 256;

/// Bounded, validated MIME metadata preserving its exact spelling.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ContentType(http1::ContentType);

impl ContentType {
    /// Validate syntax and the object-protocol field limit.
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        if bytes.len() > MAX_CONTENT_TYPE_BYTES {
            return Err(Error::InvalidRequest);
        }
        http1::ContentType::parse(bytes)
            .map(Self)
            .map_err(Into::into)
    }

    /// Borrow the validated MIME string.
    pub fn as_str(&self) -> &str {
        self.0.as_str()
    }

    /// Borrow the validated MIME wire bytes.
    pub fn as_bytes(&self) -> &[u8] {
        self.0.as_bytes()
    }
}

// Metadata
// Versioned metadata. TTL controls new unpinned admission, never page eviction.

/// Absolute Unix-millisecond deadline in 0..=i64::MAX, never a stream deadline.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ExpiresAt(std::time::SystemTime);

impl ExpiresAt {
    /// Construct a millisecond fixture from a test clock.
    #[cfg(any(test, feature = "test-util"))]
    pub fn test_time(time: SystemTime) -> Self {
        Self::from_unix_millis(
            time.duration_since(UNIX_EPOCH)
                .unwrap()
                .as_millis()
                .try_into()
                .unwrap(),
        )
        .unwrap()
    }

    /// Validate an exact, nonnegative millisecond timestamp.
    pub fn from_system_time(time: SystemTime) -> Result<Self> {
        let value = Self(time);
        value.to_unix_millis()?;
        Ok(value)
    }

    /// Return the absolute wall-clock timestamp.
    pub fn as_system_time(self) -> SystemTime {
        self.0
    }

    /// Construct from the protocol's nonnegative signed-64-bit domain.
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

    /// Parse canonical Unix milliseconds without whitespace or rounding.
    pub fn parse(value: &[u8]) -> Result<Self> {
        Self::from_unix_millis(parse_decimal(value)?)
    }

    /// Encode exact Unix milliseconds.
    pub fn to_header(self) -> Result<String> {
        Ok(self.to_unix_millis()?.to_string())
    }
}

/// Version metadata plus its current freshness deadline.
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
    /// Validate public numeric fields before admitting metadata.
    pub fn validate(&self) -> Result<()> {
        if self.length > MAX_WIRE_INTEGER {
            return Err(Error::InvalidRequest);
        }
        self.expires_at.to_unix_millis()?;
        Ok(())
    }

    /// Drop the freshness claim while retaining the immutable version descriptor.
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
    pub fn validate_page(&self, envelope: &impl PageDescriptor) -> Result<()> {
        envelope.validate_structure()?;
        if self.page_length(envelope.page())? != envelope.plaintext_length() {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }

    /// Resolve a page's exact length, rejecting another version or an absent page.
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

/// A structurally checked page supplied by the application's storage or crypto layer.
/// This view does not authenticate bytes or prescribe an encryption format.
pub trait PageDescriptor {
    /// Check the application's page representation before comparing metadata.
    fn validate_structure(&self) -> Result<()>;

    /// Return the complete version and page identity.
    fn page(&self) -> &PageId;

    /// Return the decoded page length, excluding transport or encryption overhead.
    fn plaintext_length(&self) -> u32;
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
        // The HTTP codec owns the MIME grammar matrix; this wrapper maps its error.
        assert_eq!(
            ContentType::parse(b"text/plain;x=a;X=b"),
            Err(Error::InvalidRequest)
        );
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
}
