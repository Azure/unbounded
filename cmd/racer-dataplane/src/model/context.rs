//! Request-scoped adapter context, deliberately absent from cache identities.
//!
//! Carry the exact key and opaque Racer-Metadata value to origin. Authorization
//! is an opaque upstream credential, not Racer authorization. It travels encrypted
//! across peers and as a normal header on the local origin Unix socket. Never put
//! either raw or encrypted credentials in page/metadata caches, checkpoints, logs,
//! metrics, or durable retry queues. Relays preserve encrypted credentials unopened.

use super::{
    envelope::{KeyId, Nonce},
    identity::{AttemptId, ObjectId, RequestId},
};
use crate::{
    error::{Result, pending},
    runtime::{admission::Reservation, deadline::RequestScope},
};

pub const METADATA_HEADER: &str = "Racer-Metadata";

/// Sensitive bytes, intentionally neither Debug nor Clone. Implement memory
/// zeroization with a vetted secret container before accepting real credentials.
pub struct Authorization {
    bytes: Vec<u8>,
}

impl Authorization {
    /// Validate HTTP field syntax/size without interpreting the credential scheme.
    pub fn from_header(_bytes: &[u8]) -> Result<Self> {
        pending("context.authorization")
    }

    /// Expose only for encryption or a local adapter write, never for diagnostics.
    pub fn expose_for_origin(&self) -> Result<&[u8]> {
        pending("context.expose_for_origin")
    }
}

/// Preserve one bounded field value; reject ambiguous duplicates and CR/LF.
pub struct OpaqueMetadata {
    bytes: Vec<u8>,
}

impl OpaqueMetadata {
    pub fn from_header(_bytes: &[u8]) -> Result<Self> {
        pending("context.metadata")
    }

    pub fn as_header(&self) -> Result<&[u8]> {
        pending("context.metadata_header")
    }
}

/// One request owns the raw context and lends it to origin writes and sealing.
/// Retrying/fanning out must not require duplicating secrets:
/// ```compile_fail
/// use racer_dataplane::model::context::OriginContext;
/// fn duplicate(origin: OriginContext) { let _copy = origin.clone(); }
/// ```
pub struct OriginContext {
    pub object: ObjectId,
    pub metadata: Option<OpaqueMetadata>,
    pub authorization: Option<Authorization>,
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

/// Owned per-attempt envelope, with no borrow of the raw request context. Relays
/// preserve it unopened. Local quota and scope are never serialized on the wire.
/// Neither envelope nor quota can be cloned to bypass per-attempt admission:
/// ```compile_fail
/// use racer_dataplane::model::context::PeerOriginContext;
/// fn duplicate(envelope: PeerOriginContext) { let _copy = envelope.clone(); }
/// ```
/// Wire decoding must also admit its allocations before constructing this owner;
/// callers cannot construct an uncharged envelope using only its wire fields:
/// ```compile_fail
/// use racer_dataplane::model::{context::PeerOriginContext,
///     identity::{ObjectId, RequestId, AttemptId}};
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
    pub(crate) reservation: Reservation,
    pub(crate) scope: RequestScope,
}

impl PeerOriginContext {
    /// Original deadline/shared cancellation, never reset by retry or fanout.
    pub fn scope(&self) -> &RequestScope {
        &self.scope
    }
}

#[cfg(test)]
mod tests {
    // Verify exact opaque forwarding, duplicate rejection, redaction, and bounds.
    // Credentials must never appear in cache, disk, or diagnostic representations.
}
