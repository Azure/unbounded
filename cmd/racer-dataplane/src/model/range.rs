//! Overflow-checked single-range normalization and whole-page slice planning.

use super::identity::PageNumber;
use crate::error::{Result, pending};

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
    pub fn resolve(self, _object_length: u64) -> Result<ResolvedRange> {
        pending("range.resolve")
    }
}

impl ResolvedRange {
    /// Produce the next slice, not an allocation proportional to object length.
    pub fn slice_at(&self, _page: PageNumber) -> Result<Option<PageSlice>> {
        pending("range.slice_at")
    }
}

#[cfg(test)]
mod tests {
    // Cover suffix/open/closed ranges, overflow, empty objects, and final-page math.
}
