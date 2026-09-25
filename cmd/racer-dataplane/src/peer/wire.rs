//! Logical peer operations, distinct from HTTP encoding and routing policy.
//!
//! Wire requests carry encrypted origin context; responses and cacheable records
//! never contain credentials. CopyOnly must never trigger origin access.
use crate::security::forwarding::ForwardedHead;
use crate::{
    memory::pool::CiphertextPage,
    model::{
        context::PeerOriginContext,
        identity::{ObjectId, PageId},
        metadata::{MetadataSelector, ObjectMetadata},
    },
    topology::paths::RouteBudget,
};

pub use crate::security::forwarding::{RequestBinding, VerifiedRequest, VerifiedResponse};

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
/// Locally constructed operation, not evidence of authenticated ingress.
pub struct PeerRequest {
    pub operation: Operation,
    pub origin: PeerOriginContext,
    pub route: RouteBudget,
}
/// Unsigned local result. Transport must sign it against the admitted request.
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

/// Owned, unverified wire input/output. The original and all forwarding signatures
/// travel with the operation, including opaque encrypted origin credentials.
/// Verification must check that the operation and effective route agree with the
/// original signed fields and the complete forwarding chain before admitting work.
pub struct SignedRequest {
    pub authentication: ForwardedHead,
    pub request: PeerRequest,
}

/// Owned, unverified wire response, including signed misses and errors. A relay
/// preserves both the original head and ciphertext; it never decrypts the body.
/// Verification checks logical response fields against the signed head and binds
/// them to the outstanding request. Page bodies are neither signed nor hashed.
pub struct SignedResponse {
    pub authentication: ForwardedHead,
    pub response: PeerResponse,
}
#[cfg(test)]
mod tests { /* Operation bounds, copy-only encoding, signed misses, request binding. */
}
