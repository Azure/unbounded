//! Versioned metadata. TTL controls new unpinned admission, never page eviction.

use super::identity::ObjectVersion;

/// Absolute wall-clock deadline. Wire epoch/precision must be standardized.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ExpiresAt(pub std::time::SystemTime);

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ObjectMetadata {
    pub version: ObjectVersion,
    pub length: u64,
    pub expires_at: ExpiresAt,
}

/// Distinguishes freshness admission from an explicit immutable-version pin.
#[derive(Clone, Debug)]
pub enum MetadataSelector {
    Fresh,
    Pinned(super::identity::StrongEtag),
}

#[cfg(test)]
mod tests {
    // Cover zero TTL, clock discontinuity, empty values, and old-version lengths.
}
