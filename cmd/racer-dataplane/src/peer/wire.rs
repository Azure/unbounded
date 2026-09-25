//! Logical peer operations, distinct from HTTP encoding and routing policy.
//!
//! Wire requests carry encrypted origin context; responses and cacheable records
//! never contain credentials. CopyOnly must never trigger origin access.
use crate::{
    memory::pool::CiphertextPage,
    model::{
        context::PeerOriginContext,
        identity::{ObjectId, PageId},
        metadata::{MetadataSelector, ObjectMetadata},
    },
    topology::paths::RouteBudget,
};

pub enum FetchMode {
    CopyOnly,
    Acquire,
}
pub enum Operation {
    Page {
        page: PageId,
        mode: FetchMode,
    },
    Metadata {
        object: ObjectId,
        selector: MetadataSelector,
        mode: FetchMode,
    },
}
pub struct PeerRequest {
    pub operation: Operation,
    pub origin: PeerOriginContext,
    pub route: RouteBudget,
}
pub enum PeerResponse {
    Page {
        metadata: ObjectMetadata,
        ciphertext: CiphertextPage,
    },
    Metadata(ObjectMetadata),
    Miss,
    VersionUnavailable,
    Unavailable,
    Overloaded,
    OriginRejected,
}
#[cfg(test)]
mod tests { /* Operation bounds, copy-only encoding, signed misses, request binding. */
}
