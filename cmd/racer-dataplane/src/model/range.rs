//! Overflow-checked single-range normalization and whole-page slice planning.

use super::{MAX_WIRE_INTEGER, identity::PageNumber, parse_decimal};
use crate::error::{Error, Result};

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

#[cfg(test)]
mod tests {
    use super::*;

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
}
