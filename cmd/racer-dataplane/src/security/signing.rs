//! HTTP Message Signatures over canonical headers and method/target or status.
//!
//! Cover identities, range, lengths, TTL, membership, freshness, metadata, and
//! encrypted Authorization. Never hash/sign page bodies. Reject duplicate fields.
use super::{
    certificates::{Certificates, VerifiedPeer},
    keyring::Keyring,
    replay::ReplayWindow,
};
use crate::{
    error::{Result, pending},
    http::codec::MessageHead,
};
use std::rc::Rc;
pub struct Signatures {
    keys: Rc<Keyring>,
    certificates: Rc<Certificates>,
    replay: Rc<ReplayWindow>,
}
pub struct SignedHead {
    pub head: MessageHead,
    pub signature: Vec<u8>,
}
pub struct VerifiedHead {
    pub(crate) signed: SignedHead,
    pub(crate) peer: VerifiedPeer,
}
impl Signatures {
    pub fn new(
        keys: Rc<Keyring>,
        certificates: Rc<Certificates>,
        replay: Rc<ReplayWindow>,
    ) -> Self {
        Self {
            keys,
            certificates,
            replay,
        }
    }
    pub fn sign(&self, _head: MessageHead) -> Result<SignedHead> {
        pending("signing.sign")
    }
    pub fn verify(&self, _head: SignedHead) -> Result<VerifiedHead> {
        pending("signing.verify")
    }
}
#[cfg(test)]
mod tests { /* Canonical vectors, substitution, response binding, metadata-only errors. */
}
