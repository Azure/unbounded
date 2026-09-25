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
use crate::error::{Result, pending};

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

pub struct OriginContext {
    pub object: ObjectId,
    pub metadata: Option<OpaqueMetadata>,
    pub authorization: Option<Authorization>,
}

/// Separate AEAD domain from page encryption. Bind to request/attempt/object and
/// metadata using canonical AAD; retries reseal rather than change bound fields.
/// Any eligible origin-fetching node can open this cache-scoped credential envelope.
pub struct EncryptedAuthorization {
    pub key_id: KeyId,
    pub nonce: Nonce,
    pub ciphertext: Vec<u8>,
}

pub struct PeerOriginContext {
    pub object: ObjectId,
    pub request: RequestId,
    pub attempt: AttemptId,
    pub metadata: Option<OpaqueMetadata>,
    pub authorization: Option<EncryptedAuthorization>,
}

#[cfg(test)]
mod tests {
    // Verify exact opaque forwarding, duplicate rejection, redaction, and bounds.
    // Credentials must never appear in cache, disk, or diagnostic representations.
}
