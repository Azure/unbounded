//! Ephemeral credential AEAD, separate from immutable page encryption and storage.
//!
//! Domain-separate keys/AAD from pages. Bind cache/key, request/attempt, and opaque
//! metadata. Relays keep this envelope opaque; eligible origin fetchers may open it.
//! No credential fingerprint or credential value becomes a cache/singleflight key.
//!
//! Compile-only retry/fanout contract: one request owner lends its raw context to
//! each seal. The two independently owned envelopes can coexist and leave that
//! borrow's lifetime. No secret context clone or successful crypto is fabricated.
//! Nonces are generated internally and temporary secrets are zeroized.
//!
//! ```no_run
//! use racer_dataplane::{
//!     error::Result,
//!     model::{context::{OriginContext, PeerOriginContext}, identity::AttemptId},
//!     runtime::deadline::RequestScope,
//!     security::credentials::CredentialCrypto,
//! };
//! fn fanout(
//!     crypto: &CredentialCrypto, origin: &OriginContext, scope: &RequestScope,
//!     first: AttemptId, second: AttemptId,
//! ) -> Result<(PeerOriginContext, PeerOriginContext)> {
//!     let a = crypto.seal(origin, first, scope)?;
//!     let b = crypto.seal(origin, second, scope)?;
//!     Ok((a, b))
//! }
//! fn retry_after_failure(
//!     crypto: &CredentialCrypto, origin: &OriginContext, scope: &RequestScope,
//!     first: AttemptId, retry: AttemptId,
//! ) -> Result<PeerOriginContext> {
//!     match crypto.seal(origin, first, scope) {
//!         Ok(envelope) => Ok(envelope),
//!         Err(_) => crypto.seal(origin, retry, scope),
//!     }
//! }
//! ```
use super::{
    aead::{field, fresh_nonce},
    keyring::{KeyPurpose, Keyring},
};
use crate::{
    error::{Error, Result},
    model::{
        context::{
            Authorization, EncryptedAuthorization, OpaqueMetadata, OriginContext, PeerOriginContext,
        },
        envelope::KeyId,
        identity::{AttemptId, ObjectId, RequestId},
        limits::ResourceClass,
    },
    runtime::{admission::Admission, deadline::RequestScope},
};
use chacha20poly1305::{KeyInit, XChaCha20Poly1305, XNonce, aead::AeadInPlace};
use std::{ops::Deref, rc::Rc};
use zeroize::Zeroizing;
/// Decrypted context remains charged for the complete local origin operation.
pub struct ChargedOriginContext {
    context: OriginContext,
    _reservation: crate::runtime::admission::Reservation,
}
impl Deref for ChargedOriginContext {
    type Target = OriginContext;
    fn deref(&self) -> &OriginContext {
        &self.context
    }
}

fn aad(
    key: KeyId,
    object: &ObjectId,
    request: RequestId,
    attempt: AttemptId,
    metadata: Option<&[u8]>,
) -> Result<Zeroizing<Vec<u8>>> {
    // Allocate once so reallocations cannot leave metadata in an unwiped buffer.
    let capacity = 128usize
        .checked_add(object.cache.0.len())
        .and_then(|n| n.checked_add(metadata.map_or(0, <[u8]>::len)))
        .ok_or(Error::InvalidRequest)?;
    let mut out = Zeroizing::new(Vec::with_capacity(capacity));
    out.extend_from_slice(b"racer/credentials/aead/v1\0");
    out.extend_from_slice(&key.0);
    field(&mut out, object.cache.0.as_bytes())?;
    out.extend_from_slice(&object.key.0);
    out.extend_from_slice(&request.0);
    out.extend_from_slice(&attempt.0);
    out.push(u8::from(metadata.is_some()));
    if let Some(metadata) = metadata {
        field(&mut out, metadata)?;
    }
    Ok(out)
}
fn bounds(object: &ObjectId, metadata: Option<&[u8]>, credential_length: usize) -> Result<usize> {
    if !super::certificates::canonical_uuid(&object.cache.0)
        || metadata.is_some_and(|m| m.len() > crate::model::MAX_FIELD_BYTES)
        || credential_length > crate::model::MAX_FIELD_BYTES + 16
    {
        return Err(Error::InvalidRequest);
    }
    512usize
        .checked_add(object.cache.0.len())
        .and_then(|n| n.checked_add(metadata.map_or(0, <[u8]>::len)))
        .and_then(|n| n.checked_add(credential_length))
        .ok_or(Error::InvalidRequest)
}
pub struct CredentialCrypto {
    keys: Rc<Keyring>,
    admission: Rc<Admission>,
}
impl CredentialCrypto {
    pub fn new(keys: Rc<Keyring>, admission: Rc<Admission>) -> Self {
        Self { keys, admission }
    }
    /// Borrow only for this bounded synchronous call; never retain, clone, enqueue,
    /// or cache the raw context. Each retry/fanout uses a new attempt ID and calls
    /// seal again, even when the credential bytes are identical. The output owns
    /// its object/metadata copies and encrypted bytes, independent of this borrow.
    ///
    /// Before allocation, check the original scope and all field/encoded lengths
    /// (including overflow, tag, and AAD scratch), then reserve RequestContext bytes
    /// against context.object.cache. Retain output quota in PeerOriginContext;
    /// temporary quota ends only when sealing stops accessing its scratch. Reject
    /// saturation rather than allocating uncharged bytes. Copy opaque metadata
    /// exactly, preserving absent versus present-empty; never normalize headers.
    ///
    /// Select KeyPurpose::OriginCredentials, never a page key. For every encryption
    /// generate a fresh CSPRNG XChaCha20 nonce internally, independent of attempt IDs,
    /// page nonces, and HTTP replay nonces. Canonical, versioned credential-domain
    /// AAD binds key ID, object (cache UID + exact key), scope.request, attempt, and
    /// opaque metadata presence/length/bytes. Encrypt the exact Authorization bytes.
    /// A missing Authorization still requires an owned, charged peer context and
    /// signed metadata; it must not bypass scope checks or admission.
    ///
    /// Recheck cancellation/deadline before returning; errors release local work
    /// without consuming origin. No detached work can outlive this borrow. The
    /// envelope retains the original scope for transport, which must retain quota
    /// through any submitted I/O completion even if its waiting future is dropped.
    /// Output and temporary scratch have independent admission charges.
    pub fn seal(
        &self,
        context: &OriginContext,
        attempt: AttemptId,
        scope: &RequestScope,
    ) -> Result<PeerOriginContext> {
        scope.check()?;
        let metadata = context
            .metadata
            .as_ref()
            .map(OpaqueMetadata::as_header)
            .transpose()?;
        let raw = context
            .authorization
            .as_ref()
            .map(Authorization::expose_for_origin)
            .transpose()?;
        let size = bounds(&context.object, metadata, raw.map_or(0, <[u8]>::len) + 16)?;
        let reservation = self.admission.reserve(
            Some(&context.object.cache),
            ResourceClass::RequestContext,
            size,
        )?;
        let _scratch = self.admission.reserve(
            Some(&context.object.cache),
            ResourceClass::RequestContext,
            size,
        )?;
        let authorization = if let Some(raw) = raw {
            let key = self
                .keys
                .active(&context.object.cache, KeyPurpose::OriginCredentials)?;
            let nonce = fresh_nonce()?;
            let aad = aad(key.id(), &context.object, scope.request, attempt, metadata)?;
            let cipher =
                XChaCha20Poly1305::new(key.material(KeyPurpose::OriginCredentials)?.into());
            let mut bytes = Zeroizing::new(Vec::with_capacity(raw.len() + 16));
            bytes.extend_from_slice(raw);
            cipher
                .encrypt_in_place(XNonce::from_slice(&nonce.0), &aad, &mut *bytes)
                .map_err(|_| Error::Unauthorized)?;
            Some(EncryptedAuthorization {
                key_id: key.id(),
                nonce,
                ciphertext: std::mem::take(&mut *bytes),
            })
        } else {
            None
        };
        let metadata = metadata.map(OpaqueMetadata::from_header).transpose()?;
        scope.check()?;
        Ok(PeerOriginContext {
            object: context.object.clone(),
            request: scope.request,
            attempt,
            metadata,
            authorization,
            reservation,
            scope: scope.clone(),
        })
    }
    pub fn open(
        &self,
        context: PeerOriginContext,
        request: RequestId,
        attempt: AttemptId,
    ) -> Result<ChargedOriginContext> {
        self.open_charged(context, request, attempt)
    }
    pub fn open_charged(
        &self,
        context: PeerOriginContext,
        request: RequestId,
        attempt: AttemptId,
    ) -> Result<ChargedOriginContext> {
        context.scope.check()?;
        if context.request != request
            || context.attempt != attempt
            || context.scope.request != request
        {
            return Err(Error::Unauthorized);
        }
        let metadata = context
            .metadata
            .as_ref()
            .map(OpaqueMetadata::as_header)
            .transpose()?;
        let size = bounds(
            &context.object,
            metadata,
            context
                .authorization
                .as_ref()
                .map_or(0, |a| a.ciphertext.len()),
        )?;
        context
            .reservation
            .validate(ResourceClass::RequestContext, size)?;
        if context.reservation.cache() != Some(&context.object.cache) {
            return Err(Error::Unauthorized);
        }
        let output = self.admission.reserve(
            Some(&context.object.cache),
            ResourceClass::RequestContext,
            size,
        )?;
        let _scratch = self.admission.reserve(
            Some(&context.object.cache),
            ResourceClass::RequestContext,
            size.checked_mul(2).ok_or(Error::InvalidRequest)?,
        )?;
        let authorization = if let Some(encrypted) = &context.authorization {
            if encrypted.ciphertext.len() < 16 {
                return Err(Error::Unauthorized);
            }
            let key = self.keys.lease(
                Some(&context.object.cache),
                encrypted.key_id,
                KeyPurpose::OriginCredentials,
            )?;
            let aad = aad(key.id(), &context.object, request, attempt, metadata)?;
            let mut bytes = Zeroizing::new(encrypted.ciphertext.clone());
            XChaCha20Poly1305::new(key.material(KeyPurpose::OriginCredentials)?.into())
                .decrypt_in_place(XNonce::from_slice(&encrypted.nonce.0), &aad, &mut *bytes)
                .map_err(|_| Error::Unauthorized)?;
            Some(Authorization::from_header(&bytes)?)
        } else {
            None
        };
        context.scope.check()?;
        Ok(ChargedOriginContext {
            context: OriginContext {
                object: context.object,
                metadata: context.metadata,
                authorization,
            },
            _reservation: output,
        })
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::identity::{CacheId, CacheKey};
    fn scope() -> RequestScope {
        RequestScope::new(
            RequestId([1; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap()
    }
    fn origin() -> OriginContext {
        OriginContext {
            object: ObjectId {
                cache: CacheId(super::super::identity::tests::CACHE.into()),
                key: CacheKey([3; 32]),
            },
            metadata: Some(OpaqueMetadata::from_header(b"opaque,  \xff").unwrap()),
            authorization: Some(Authorization::from_header(b"Bearer exact  \xfe").unwrap()),
        }
    }
    #[test]
    fn exact_roundtrip_nonce_freshness_admission_and_cancellation() {
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let crypto = CredentialCrypto::new(
            Rc::new(super::super::keyring::tests::keys()),
            admission.clone(),
        );
        let original = origin();
        let scope = scope();
        let attempt = AttemptId([4; 16]);
        let first = crypto.seal(&original, attempt, &scope).unwrap();
        let second = crypto.seal(&original, attempt, &scope).unwrap();
        assert_ne!(
            first.authorization.as_ref().unwrap().nonce,
            second.authorization.as_ref().unwrap().nonce
        );
        drop(second);
        let clear = crypto.open(first, scope.request, attempt).unwrap();
        assert_eq!(
            clear
                .authorization
                .as_ref()
                .unwrap()
                .expose_for_origin()
                .unwrap(),
            b"Bearer exact  \xfe"
        );
        assert_eq!(
            clear.metadata.as_ref().unwrap().as_header().unwrap(),
            b"opaque,  \xff"
        );
        assert!(admission.used(ResourceClass::RequestContext) > 0);
        drop(clear);
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        scope.cancel().unwrap();
        assert!(matches!(
            crypto.seal(&original, attempt, &scope),
            Err(Error::Cancelled)
        ));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.request_context_bytes = std::num::NonZeroUsize::new(1).unwrap();
        let crypto = CredentialCrypto::new(
            Rc::new(super::super::keyring::tests::keys()),
            Rc::new(Admission::new(limits)),
        );
        assert!(matches!(
            crypto.seal(&original, attempt, &self::scope()),
            Err(Error::Overloaded)
        ));
    }
    #[test]
    fn absent_authorization_is_charged_and_expired_context_is_rejected() {
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let crypto = CredentialCrypto::new(
            Rc::new(super::super::keyring::tests::keys()),
            admission.clone(),
        );
        let mut original = origin();
        original.authorization = None;
        let scope = scope();
        let attempt = AttemptId([4; 16]);
        let sealed = crypto.seal(&original, attempt, &scope).unwrap();
        assert!(sealed.authorization.is_none());
        assert!(admission.used(ResourceClass::RequestContext) > 0);
        let clear = crypto.open(sealed, scope.request, attempt).unwrap();
        assert!(clear.authorization.is_none());
        assert_eq!(
            clear.metadata.as_ref().unwrap().as_header().unwrap(),
            b"opaque,  \xff"
        );
        drop(clear);
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        let sealed = crypto.seal(&original, attempt, &scope).unwrap();
        scope.cancel().unwrap();
        assert!(matches!(
            crypto.open(sealed, scope.request, attempt),
            Err(Error::Cancelled)
        ));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    }
    #[test]
    fn rejects_substitution_and_distinguishes_absent_from_empty() {
        let crypto = CredentialCrypto::new(
            Rc::new(super::super::keyring::tests::keys()),
            Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            )),
        );
        let original = origin();
        let scope = scope();
        let attempt = AttemptId([4; 16]);
        for index in 0..6 {
            let mut sealed = crypto.seal(&original, attempt, &scope).unwrap();
            match index {
                0 => sealed.object.key.0[0] ^= 1,
                1 => sealed.metadata = None,
                2 => sealed.authorization.as_mut().unwrap().ciphertext[0] ^= 1,
                3 => sealed.authorization.as_mut().unwrap().nonce.0[0] ^= 1,
                4 => sealed.request.0[0] ^= 1,
                _ => sealed.attempt.0[0] ^= 1,
            }
            assert!(crypto.open_charged(sealed, scope.request, attempt).is_err());
        }
        assert_ne!(
            *aad(
                KeyId([1; 16]),
                &original.object,
                scope.request,
                attempt,
                None
            )
            .unwrap(),
            *aad(
                KeyId([1; 16]),
                &original.object,
                scope.request,
                attempt,
                Some(b"")
            )
            .unwrap()
        );
    }

    #[test]
    fn seal_borrows_inputs_and_returns_an_independent_owner() {
        // Independent input lifetimes; the result cannot borrow any of them.
        let _: for<'crypto, 'origin, 'scope> fn(
            &'crypto CredentialCrypto,
            &'origin OriginContext,
            AttemptId,
            &'scope RequestScope,
        ) -> Result<PeerOriginContext> = CredentialCrypto::seal;
        fn owned<T: Send + 'static>() {}
        owned::<PeerOriginContext>();
    }
}
