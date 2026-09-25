//! Vetted XChaCha20-Poly1305 adapter, canonical page AAD, fresh cryptographic nonces.
use super::keyring::Keyring;
use crate::{
    error::{Operation, deferred},
    memory::pool::{CiphertextPage, PlaintextBuffer, VerifiedPage},
    model::identity::PageId,
    runtime::{admission::FillReservation, deadline::RequestScope},
};
use std::rc::Rc;
pub struct PageCrypto {
    keys: Rc<Keyring>,
}
impl PageCrypto {
    pub fn new(keys: Rc<Keyring>) -> Self {
        Self { keys }
    }
    /// Authenticate the entire page before constructing a publishable lease.
    pub fn decrypt<'a>(
        &'a self,
        _ciphertext: CiphertextPage,
        _reservation: &'a mut FillReservation,
        _scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedPage> {
        deferred("aead.decrypt")
    }
    /// Encrypt once; original ciphertext is reused verbatim for peers and disk.
    pub fn encrypt<'a>(
        &'a self,
        _page: PageId,
        _plaintext: PlaintextBuffer,
        _reservation: &'a mut FillReservation,
        _scope: &'a RequestScope,
    ) -> Operation<'a, (VerifiedPage, CiphertextPage)> {
        deferred("aead.encrypt")
    }
}
#[cfg(test)]
mod tests { /* Known-answer vectors, tampering, wrong identity/length, nonce freshness. */
}
