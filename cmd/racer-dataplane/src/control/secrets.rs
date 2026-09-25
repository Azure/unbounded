//! Watch projected-directory replacement and stage complete coherent key bundles.
use super::wire::BundleGeneration;
use crate::{
    error::{Operation, deferred},
    runtime::deadline::RequestScope,
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
    /// Load common bundle.json from one coherent projected generation; malformed
    /// reloads retain the last valid bundle. Local signing identity is independent.
    /// Never retire ciphertext keys ahead of lease barriers.
    /// Generation is local diagnostics only, never a controller acknowledgment.
    pub fn reload<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, BundleGeneration> {
        deferred("secrets.reload")
    }
}
#[cfg(test)]
mod tests { /* Partial projections, symlink replacement, invalid bundle rollback. */
}
