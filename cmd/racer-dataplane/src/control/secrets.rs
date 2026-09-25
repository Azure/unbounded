//! Watch projected-directory replacement and stage complete coherent key bundles.
use super::{
    files,
    wire::{self, BundleGeneration, KeyringBundle},
};
use crate::{
    error::{Error, Operation, Result},
    runtime::deadline::RequestScope,
    security::keyring::Keyring,
};
use sha2::{Digest, Sha256};
use std::{cell::RefCell, path::PathBuf, rc::Rc};
pub struct SecretWatcher {
    directory: PathBuf,
    keys: RefCell<Rc<Keyring>>,
    accepted: RefCell<Option<(BundleGeneration, [u8; 32], Vec<Vec<u8>>)>>,
    reactor: RefCell<Option<Rc<crate::runtime::reactor::Reactor>>>,
}
impl SecretWatcher {
    pub fn new(directory: PathBuf, keys: Rc<Keyring>) -> Self {
        Self {
            directory,
            keys: RefCell::new(keys),
            accepted: RefCell::new(None),
            reactor: RefCell::new(None),
        }
    }
    /// Load common bundle.json from one coherent projected generation; malformed
    /// reloads retain the last valid bundle. Local signing identity is independent.
    /// Never retire ciphertext keys ahead of lease barriers.
    /// Generation is local diagnostics only, never a controller acknowledgment.
    pub fn reload<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, BundleGeneration> {
        Box::pin(async move {
            scope.check()?;
            self.reload_async(scope)
                .await
                .map(|(generation, _)| generation)
        })
    }
    pub fn read_bundle(&self) -> Result<KeyringBundle> {
        if self.reactor.borrow().is_some() {
            return Err(Error::InvalidConfiguration);
        }
        let bytes = zeroize::Zeroizing::new(files::projected_file(
            &self.directory,
            "bundle.json",
            wire::MAX_BUNDLE_BYTES,
        )?);
        wire::decode_bundle(&bytes)
    }
    pub fn attach_reactor(&self, reactor: Rc<crate::runtime::reactor::Reactor>) {
        *self.reactor.borrow_mut() = Some(reactor);
    }
    pub async fn read_bundle_async(&self, scope: &RequestScope) -> Result<KeyringBundle> {
        let r = self
            .reactor
            .borrow()
            .clone()
            .ok_or(Error::InvalidConfiguration)?;
        let bytes = super::async_files::projected_file(
            &r,
            &self.directory,
            "bundle.json",
            wire::MAX_BUNDLE_BYTES,
            scope,
        )
        .await?;
        wire::decode_bundle(&bytes)
    }
    pub async fn reload_async(
        &self,
        scope: &RequestScope,
    ) -> Result<(BundleGeneration, Vec<Vec<u8>>)> {
        let bundle = self.read_bundle_async(scope).await?;
        scope.check()?;
        self.install(bundle)
    }
    pub fn bind_keyring(&self, keys: Rc<Keyring>) {
        *self.keys.borrow_mut() = keys;
        *self.accepted.borrow_mut() = None;
    }
    pub fn reload_now(&self) -> Result<(BundleGeneration, Vec<Vec<u8>>)> {
        self.install(self.read_bundle()?)
    }
    pub(crate) fn install(
        &self,
        mut bundle: KeyringBundle,
    ) -> Result<(BundleGeneration, Vec<Vec<u8>>)> {
        bundle.peer_trust_roots.sort();
        bundle.cache_keys.sort_by(|a, b| {
            (&a.key.cache.0, a.key.purpose as u8, a.key.id.0).cmp(&(
                &b.key.cache.0,
                b.key.purpose as u8,
                b.key.id.0,
            ))
        });
        let encoded = zeroize::Zeroizing::new(wire::encode_bundle(&bundle)?);
        let hash: [u8; 32] = Sha256::digest(&*encoded).into();
        if let Some((generation, old, roots)) = self.accepted.borrow().as_ref() {
            if bundle.generation < *generation || bundle.generation == *generation && hash != *old {
                return Err(Error::Replay);
            }
            if bundle.generation == *generation {
                return Ok((*generation, roots.clone()));
            }
        }
        let roots = bundle.peer_trust_roots.clone();
        let generation = self.keys.borrow().install(bundle)?;
        *self.accepted.borrow_mut() = Some((generation, hash, roots.clone()));
        Ok((generation, roots))
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::control::testing;
    use std::os::unix::fs::symlink;
    #[test]
    fn coherent_projection_rejects_partial_and_escaping_links() {
        let d = testing::Directory::new();
        std::fs::create_dir(d.0.join("epoch-a")).unwrap();
        std::fs::write(
            d.0.join("epoch-a/bundle.json"),
            include_bytes!("testdata/bundle.json"),
        )
        .unwrap();
        symlink("epoch-a", d.0.join("..data")).unwrap();
        // Never use the per-file link, even if it points to attacker-selected bytes.
        symlink("/dev/null", d.0.join("bundle.json")).unwrap();
        let bytes = files::projected_file(&d.0, "bundle.json", wire::MAX_BUNDLE_BYTES).unwrap();
        assert!(wire::decode_bundle(&bytes).is_ok());
        std::fs::create_dir(d.0.join("epoch-b")).unwrap();
        symlink("epoch-b", d.0.join("..next")).unwrap();
        std::fs::rename(d.0.join("..next"), d.0.join("..data")).unwrap();
        assert!(files::projected_file(&d.0, "bundle.json", wire::MAX_BUNDLE_BYTES).is_err());
        std::fs::write(d.0.join("epoch-b/bundle.json"), b"{\"generation\":null}").unwrap();
        assert!(
            wire::decode_bundle(
                &files::projected_file(&d.0, "bundle.json", wire::MAX_BUNDLE_BYTES).unwrap()
            )
            .is_err()
        );
        std::fs::remove_file(d.0.join("..data")).unwrap();
        symlink("../", d.0.join("..data")).unwrap();
        assert!(files::projected_file(&d.0, "bundle.json", wire::MAX_BUNDLE_BYTES).is_err());
    }
}
