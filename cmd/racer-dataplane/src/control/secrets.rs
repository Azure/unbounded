//! Install complete network key bundles without filesystem access.
use super::wire::{self, BundleGeneration, KeyringBundle};
use crate::{
    error::{Error, Result},
    security::identity::Keyring,
};
use sha2::{Digest, Sha256};
use std::{cell::RefCell, rc::Rc};
pub struct BundleInstaller {
    keys: RefCell<Rc<Keyring>>,
    accepted: RefCell<Option<(BundleGeneration, [u8; 32], Vec<Vec<u8>>)>>,
}
impl BundleInstaller {
    pub fn new(keys: Rc<Keyring>) -> Self {
        Self {
            keys: RefCell::new(keys),
            accepted: RefCell::new(None),
        }
    }
    pub fn generation(&self) -> Option<BundleGeneration> {
        self.accepted
            .borrow()
            .as_ref()
            .map(|(generation, _, _)| *generation)
    }
    pub fn bind_keyring(&self, keys: Rc<Keyring>) {
        *self.keys.borrow_mut() = keys;
        *self.accepted.borrow_mut() = None;
    }
    pub fn install(&self, mut bundle: KeyringBundle) -> Result<(BundleGeneration, Vec<Vec<u8>>)> {
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
    use crate::control::{async_files, testing};
    use std::os::unix::fs::symlink;
    #[test]
    fn bundle_installation_is_idempotent_and_rejects_rollback() {
        use crate::security::identity::{KeyEpochs, KeyPurpose};
        use std::sync::Arc;

        let publication =
            wire::decode_publication(include_bytes!("testdata/publication.json")).unwrap();
        let keys = Rc::new(Keyring::new(
            publication.cluster,
            publication.members[0].node.clone(),
            Arc::new(KeyEpochs::default()),
        ));
        let installer = BundleInstaller::new(keys.clone());
        let (ca, _) = testing::ca();
        let mut bundle = wire::decode_bundle(include_bytes!("testdata/bundle.json")).unwrap();
        bundle.generation = BundleGeneration(2);
        bundle.peer_trust_roots = vec![ca.der().to_vec()];
        // The shared fixture repeats material across purposes. Keep only page keys,
        // since production security correctly rejects cross-purpose key reuse.
        bundle.cache_keys.truncate(2);
        let cache = bundle.cache_keys[0].key.cache.clone();
        for _ in 0..2 {
            assert_eq!(
                installer.install(bundle.clone()).unwrap().0,
                BundleGeneration(2)
            );
            assert!(keys.active(&cache, KeyPurpose::Page).is_ok());
        }
        assert!(wire::decode_bundle(b"{}").is_err());
        bundle.generation = BundleGeneration(0);
        assert!(installer.install(bundle).is_err());
        assert_eq!(installer.generation(), Some(BundleGeneration(2)));
        assert!(keys.active(&cache, KeyPurpose::Page).is_ok());
    }

    #[test]
    fn coherent_projection_rejects_partial_and_escaping_links() {
        let Some(reactor) = testing::reactor() else {
            return;
        };
        let d = testing::Directory::new();
        let scope = testing::scope();
        let read = || {
            testing::drive(
                &reactor,
                Box::pin(async_files::projected_file(
                    &reactor,
                    &d.0,
                    "bundle.json",
                    wire::MAX_BUNDLE_BYTES,
                    &scope,
                )),
            )
        };
        std::fs::create_dir(d.0.join("epoch-a")).unwrap();
        std::fs::write(
            d.0.join("epoch-a/bundle.json"),
            include_bytes!("testdata/bundle.json"),
        )
        .unwrap();
        symlink("epoch-a", d.0.join("..data")).unwrap();
        // Never use the per-file link, even if it points to attacker-selected bytes.
        symlink("/dev/null", d.0.join("bundle.json")).unwrap();
        let bytes = read().unwrap();
        assert!(wire::decode_bundle(&bytes).is_ok());
        std::fs::create_dir(d.0.join("epoch-b")).unwrap();
        symlink("epoch-b", d.0.join("..next")).unwrap();
        std::fs::rename(d.0.join("..next"), d.0.join("..data")).unwrap();
        assert!(read().is_err());
        std::fs::write(d.0.join("epoch-b/bundle.json"), b"{\"generation\":null}").unwrap();
        assert!(wire::decode_bundle(&read().unwrap()).is_err());
        std::fs::remove_file(d.0.join("..data")).unwrap();
        symlink("../", d.0.join("..data")).unwrap();
        assert!(read().is_err());
    }
}
