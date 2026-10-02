//! Ed25519 signing and strict verification without entropy or identity policy.

use crate::Error;
use ed25519_dalek::{
    Signature, Signer,
    pkcs8::{DecodePrivateKey, EncodePrivateKey},
};
use zeroize::Zeroizing;

/// Secret signing key. Intentionally does not implement Debug or Clone.
/// The upstream key zeroizes its secret on drop; callers own their seed's lifetime.
pub struct SigningKey(ed25519_dalek::SigningKey);

impl SigningKey {
    /// Import a caller-supplied secret seed. No randomness is generated here.
    pub fn from_seed(seed: &[u8; 32]) -> Self {
        Self(ed25519_dalek::SigningKey::from_bytes(seed))
    }

    pub fn from_pkcs8_der(der: &[u8]) -> Result<Self, Error> {
        ed25519_dalek::SigningKey::from_pkcs8_der(der)
            .map(Self)
            .map_err(|_| Error(()))
    }

    /// Explicit secret export. Both the temporary DER document and returned
    /// bytes are zeroized on drop.
    pub fn to_pkcs8_der(&self) -> Result<Zeroizing<Vec<u8>>, Error> {
        let document = self.0.to_pkcs8_der().map_err(|_| Error(()))?;
        Ok(Zeroizing::new(document.as_bytes().to_vec()))
    }

    pub fn verifying_key(&self) -> VerifyingKey {
        VerifyingKey(self.0.verifying_key())
    }

    pub fn sign(&self, msg: &[u8]) -> [u8; 64] {
        self.0.sign(msg).to_bytes()
    }
}

/// Public verification key. Parsing does not establish trust or identity.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct VerifyingKey(ed25519_dalek::VerifyingKey);

impl VerifyingKey {
    pub fn from_bytes(bytes: &[u8; 32]) -> Result<Self, Error> {
        ed25519_dalek::VerifyingKey::from_bytes(bytes)
            .map(Self)
            .map_err(|_| Error(()))
    }

    pub fn as_bytes(&self) -> &[u8; 32] {
        self.0.as_bytes()
    }

    /// Reject malformed signatures and use upstream strict verification,
    /// including its small-order point and scalar malleability checks.
    pub fn verify_strict(&self, msg: &[u8], sig: &[u8]) -> Result<(), Error> {
        let signature = Signature::from_slice(sig).map_err(|_| Error(()))?;
        self.0.verify_strict(msg, &signature).map_err(|_| Error(()))
    }
}
