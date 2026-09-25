//! Ephemeral credential AEAD, separate from immutable page encryption and storage.
//!
//! Domain-separate keys/AAD from pages. Bind cache/key, request/attempt, and opaque
//! metadata. Relays keep this envelope opaque; eligible origin fetchers may open it.
//! No credential fingerprint or credential value becomes a cache/singleflight key.
//!
//! Compile-only retry/fanout contract: one request owner lends its raw context to
//! each seal. The two independently owned envelopes can coexist and leave that
//! borrow's lifetime. No secret context clone or successful crypto is fabricated.
//! Nonce generation, canonical AAD, admission, and AEAD remain fail-closed work.
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
use super::keyring::Keyring;
use crate::{
    error::{Result, pending},
    model::{
        context::{OriginContext, PeerOriginContext},
        identity::{AttemptId, RequestId},
    },
    runtime::{admission::Admission, deadline::RequestScope},
};
use std::rc::Rc;
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
    /// AEAD, entropy, accounting, and secret zeroization are not implemented here.
    pub fn seal(
        &self,
        _context: &OriginContext,
        _attempt: AttemptId,
        _scope: &RequestScope,
    ) -> Result<PeerOriginContext> {
        pending("credentials.seal")
    }
    pub fn open(
        &self,
        _context: PeerOriginContext,
        _request: RequestId,
        _attempt: AttemptId,
    ) -> Result<OriginContext> {
        pending("credentials.open")
    }
}
#[cfg(test)]
mod tests {
    use super::*;

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

    // Runtime vectors must cover fresh nonces on reseal, wrong object/request/
    // attempt/domain, metadata substitution (including absent vs empty), exact
    // header round trips, quota saturation, and cancellation when AEAD is added.
}
