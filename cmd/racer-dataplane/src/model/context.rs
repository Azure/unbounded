//! Request-scoped adapter context, deliberately absent from cache identities.
//!
//! Carry the exact key and opaque Racer-Metadata value to origin. Authorization
//! is an opaque upstream credential, not Racer authorization. It travels encrypted
//! across peers and as a normal header on the local origin Unix socket. Never put
//! either raw or encrypted credentials in page/metadata caches, checkpoints, logs,
//! metrics, or durable retry queues. Relays preserve encrypted credentials unopened.

use super::{
    MAX_FIELD_BYTES,
    envelope::{KeyId, Nonce},
    identity::{AttemptId, ObjectId, RequestId},
};
use crate::{
    error::{Error, Result},
    runtime::{admission::Reservation, deadline::RequestScope},
};
use std::fmt;
use zeroize::Zeroizing;

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
    pub fn expose_for_origin(&self) -> Result<&[u8]> {
        Ok(self.bytes.as_slice())
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

    pub fn as_header(&self) -> Result<&[u8]> {
        Ok(self.bytes.as_slice())
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
/// use racer_dataplane::model::context::OriginContext;
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
    use super::*;
    use crate::model::identity::{CacheId, CacheKey};

    #[test]
    fn opaque_context_round_trips_non_utf8_without_normalization() {
        let bytes = b"opaque,  credential\\\"\xff";
        let authorization = Authorization::from_header(bytes).unwrap();
        let metadata = OpaqueMetadata::from_header(bytes).unwrap();
        assert_eq!(authorization.expose_for_origin().unwrap(), bytes);
        assert_eq!(metadata.as_header().unwrap(), bytes);
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
