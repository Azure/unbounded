//! Watch projected-directory replacement and stage complete coherent key bundles.
use super::enrollment::LocalSigningIdentity;
use crate::{
    error::{Operation, deferred},
    security::keyring::Keyring,
};
use std::{path::PathBuf, rc::Rc};
pub struct SecretWatcher {
    directory: PathBuf,
    keys: Rc<Keyring>,
}
impl SecretWatcher {
    pub fn new(directory: PathBuf, keys: Rc<Keyring>) -> Self {
        Self { directory, keys }
    }
    /// Match mounted certificates to local private keys; malformed reloads retain
    /// the last valid bundle. Never retire ciphertext keys ahead of lease barriers.
    pub fn reload<'a>(&'a self, _identity: &'a LocalSigningIdentity) -> Operation<'a, ()> {
        deferred("secrets.reload")
    }
}
#[cfg(test)]
mod tests { /* Partial projections, symlink replacement, invalid bundle rollback. */
}
