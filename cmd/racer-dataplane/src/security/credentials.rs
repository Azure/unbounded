//! Ephemeral credential AEAD, separate from immutable page encryption and storage.
//!
//! Domain-separate keys/AAD from pages. Bind cache/key, request/attempt, and opaque
//! metadata. Relays keep this envelope opaque; eligible origin fetchers may open it.
//! No credential fingerprint or credential value becomes a cache/singleflight key.
use super::keyring::Keyring;
use crate::{
    error::{Result, pending},
    model::{
        context::{OriginContext, PeerOriginContext},
        identity::{AttemptId, RequestId},
    },
};
use std::rc::Rc;
pub struct CredentialCrypto {
    keys: Rc<Keyring>,
}
impl CredentialCrypto {
    pub fn new(keys: Rc<Keyring>) -> Self {
        Self { keys }
    }
    pub fn seal(
        &self,
        _context: OriginContext,
        _request: RequestId,
        _attempt: AttemptId,
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
mod tests { /* Round trips, wrong object/request, metadata substitution, no persistence. */
}
