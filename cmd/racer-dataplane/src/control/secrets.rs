//! Watch projected-directory replacement and stage complete coherent key bundles.
use super::{enrollment::Enrollment, wire::BundleGeneration};
use crate::{
    error::{Operation, deferred},
    runtime::deadline::RequestScope,
    security::keyring::Keyring,
};
use std::{path::PathBuf, rc::Rc};
pub struct SecretWatcher {
    directory: PathBuf,
    keys: Rc<Keyring>,
    enrollment: Rc<Enrollment>,
}
impl SecretWatcher {
    pub fn new(directory: PathBuf, keys: Rc<Keyring>, enrollment: Rc<Enrollment>) -> Self {
        Self {
            directory,
            keys,
            enrollment,
        }
    }
    /// Match mounted certificates to local private keys; malformed reloads retain
    /// the last valid bundle. Never retire ciphertext keys ahead of lease barriers.
    /// Load bundle.json from one projected generation and match all signing keys.
    /// Generation is local diagnostics only, never a controller acknowledgment.
    pub fn reload<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, BundleGeneration> {
        deferred("secrets.reload")
    }
}
#[cfg(test)]
mod tests { /* Partial projections, symlink replacement, invalid bundle rollback. */
}
