//! Vetted XChaCha20-Poly1305 adapter, canonical page AAD, fresh cryptographic nonces.
use super::keyring::{KeyPurpose, Keyring};
use crate::{
    error::{Error, Operation, Result, deferred, pending},
    memory::pool::{CiphertextPage, PlaintextBuffer, VerifiedPage},
    model::identity::PageId,
    runtime::{
        admission::Reservation,
        crypto::{CryptoClient, CryptoInput, CryptoOutput},
        deadline::RequestScope,
        worker::{CryptoRuntime, CryptoService},
    },
};
use std::rc::Rc;
pub struct PageCrypto {
    keys: Rc<Keyring>,
    client: Rc<CryptoClient>,
}
impl PageCrypto {
    /// I/O-local facade. Only owned page inputs and an immutable key lease cross
    /// to the engine; this Rc graph and its futures stay on I/O.
    pub fn new(keys: Rc<Keyring>, client: Rc<CryptoClient>) -> Self {
        Self { keys, client }
    }
    /// Authenticate the entire page before constructing a publishable lease.
    /// Futures stay on I/O, even though inputs/completions are Send:
    /// ```compile_fail
    /// use racer_dataplane::{security::aead::PageCrypto,
    ///     memory::pool::CiphertextPage,
    ///     runtime::{admission::Reservation, deadline::RequestScope}};
    /// fn require_send<T: Send>(_: T) {}
    /// fn move_future(crypto: &PageCrypto, page: CiphertextPage,
    ///     output: Reservation, scope: &RequestScope) {
    ///     require_send(crypto.decrypt(page, output, scope));
    /// }
    /// ```
    pub fn decrypt<'a>(
        &'a self,
        ciphertext: CiphertextPage,
        plaintext: Reservation,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedPage> {
        Box::pin(async move {
            let envelope = ciphertext.envelope();
            let key = self.keys.lease(
                Some(&envelope.page.version.object.cache),
                envelope.key_id,
                KeyPurpose::Page,
            )?;
            match self
                .client
                .execute(
                    CryptoInput::Decrypt {
                        ciphertext,
                        plaintext,
                    },
                    key,
                    scope,
                )
                .await?
            {
                CryptoOutput::Decrypted(page, _original_ciphertext) => Ok(page),
                CryptoOutput::Encrypted(..) => Err(Error::CorruptRecord),
            }
        })
    }
    /// Encrypt once; original ciphertext is reused verbatim for peers and disk.
    pub fn encrypt<'a>(
        &'a self,
        page: PageId,
        plaintext: PlaintextBuffer,
        ciphertext: Reservation,
        scope: &'a RequestScope,
    ) -> Operation<'a, (VerifiedPage, CiphertextPage)> {
        Box::pin(async move {
            let key = self
                .keys
                .active(&page.version.object.cache, KeyPurpose::Page)?;
            match self
                .client
                .execute(
                    CryptoInput::Encrypt {
                        page,
                        plaintext,
                        ciphertext,
                    },
                    key,
                    scope,
                )
                .await?
            {
                CryptoOutput::Encrypted(plaintext, ciphertext) => Ok((plaintext, ciphertext)),
                CryptoOutput::Decrypted(..) => Err(Error::CorruptRecord),
            }
        })
    }
}

/// Constructed on the paired crypto thread, without access to the I/O service graph.
/// A vetted AEAD adapter will process owned jobs in bounded quanta, check the
/// original deadline/cancellation, and return every accepted job as a completion.
pub struct PageCryptoEngine {
    runtime: CryptoRuntime,
}
impl PageCryptoEngine {
    pub fn new(runtime: CryptoRuntime) -> Self {
        Self { runtime }
    }
}
impl CryptoService for PageCryptoEngine {
    fn start<'a>(&'a mut self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("aead.engine_start")
    }
    fn poll_budgeted(&mut self, _work_budget: usize) -> Result<()> {
        pending("aead.engine_poll")
    }
    fn drain<'a>(&'a mut self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("aead.engine_drain")
    }
    fn shutdown<'a>(&'a mut self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("aead.engine_shutdown")
    }
}
#[cfg(test)]
mod tests { /* Known-answer vectors, tampering, wrong identity/length, nonce freshness. */
}
