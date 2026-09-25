//! Semantic identities and canonical-encoding boundaries.
//!
//! Keys are exactly 32 bytes. Strong ETags are opaque version identifiers, not
//! content hashes. Placement excludes ETag; page cache and flight identity include it.

use crate::error::{Result, pending};

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct ClusterId(pub String);
/// Kubernetes ClusterCache UID, not its reusable resource name.
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct CacheId(pub String);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct CacheKey(pub [u8; 32]);
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
/// Kubernetes Node UID, not its reusable resource name.
pub struct NodeId(pub String);
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct StrongEtag(String);

impl StrongEtag {
    #[cfg(test)]
    pub(crate) fn test_value(value: &str) -> Self {
        Self(value.to_owned())
    }
    /// Reject weak, wildcard, malformed, and ambiguous HTTP entity tags.
    pub fn parse(_value: &[u8]) -> Result<Self> {
        pending("identity.strong_etag")
    }
}

#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct PageNumber(pub u64);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct MembershipVersion(pub u64);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct RequestId(pub [u8; 16]);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct AttemptId(pub [u8; 16]);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct TransferId(pub [u8; 16]);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct WorkerId(pub u16);

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct ObjectId {
    pub cache: CacheId,
    pub key: CacheKey,
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct ObjectVersion {
    pub object: ObjectId,
    pub etag: StrongEtag,
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct PageId {
    pub version: ObjectVersion,
    pub number: PageNumber,
}

#[cfg(test)]
mod tests {
    // Implement canonical encoding vectors and malformed strong-ETag cases here.
}
