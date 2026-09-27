//! Current positive admission set, shared by cache lookups and late publications.
//! No removal history is needed: an absent UID/key is a miss. Reintroducing a UID
//! denotes the same immutable namespace; a different namespace requires a new UID.
use crate::{
    control::snapshot::PublishedState,
    model::{envelope::KeyId, identity::CacheId},
    security::keyring::{KeyPurpose, Keyring},
};
use std::{rc::Rc, sync::Arc};

pub struct Availability {
    publications: Arc<PublishedState>,
    keys: Rc<Keyring>,
}
impl Availability {
    pub fn new(publications: Arc<PublishedState>, keys: Rc<Keyring>) -> Self {
        Self { publications, keys }
    }
    pub fn cache(&self, cache: &CacheId) -> bool {
        self.publications
            .current()
            .is_ok_and(|s| s.caches.iter().any(|c| &c.id == cache))
    }
    pub fn metadata(&self, cache: &CacheId) -> bool {
        self.cache(cache) && self.keys.active(cache, KeyPurpose::Page).is_ok()
    }
    pub fn page(&self, cache: &CacheId, key: KeyId) -> bool {
        self.cache(cache) && self.keys.lease(Some(cache), key, KeyPurpose::Page).is_ok()
    }
}
