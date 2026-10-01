//! Current positive admission set, shared by cache lookups and late publications.
//! No removal history is needed: an absent UID/key is a miss. Reintroducing a UID
//! denotes the same immutable namespace; a different namespace requires a new UID.
use crate::{
    control::snapshot::PublishedState,
    model::{CacheId, KeyId},
    security::identity::{KeyPurpose, Keyring},
};
use std::{rc::Rc, sync::Arc};

pub struct Availability {
    publications: Arc<PublishedState>,
    keys: Rc<Keyring>,
    #[cfg(test)]
    permissive: bool,
}
impl Availability {
    pub fn new(publications: Arc<PublishedState>, keys: Rc<Keyring>) -> Self {
        Self {
            publications,
            keys,
            #[cfg(test)]
            permissive: false,
        }
    }
    /// Isolated state-machine tests explicitly opt out of control-plane admission.
    #[cfg(test)]
    pub(crate) fn permissive_for_tests() -> Rc<Self> {
        Rc::new(Self {
            publications: Arc::new(PublishedState::default()),
            keys: Rc::new(crate::security::identity::keyring_tests::keys()),
            permissive: true,
        })
    }
    pub fn cache(&self, cache: &CacheId) -> bool {
        #[cfg(test)]
        if self.permissive {
            return true;
        }
        self.publications
            .current()
            .is_ok_and(|s| s.caches.iter().any(|c| &c.id == cache))
    }
    pub fn metadata(&self, cache: &CacheId) -> bool {
        #[cfg(test)]
        if self.permissive {
            return true;
        }
        self.cache(cache) && self.keys.active(cache, KeyPurpose::Page).is_ok()
    }
    pub fn page(&self, cache: &CacheId, key: KeyId) -> bool {
        #[cfg(test)]
        if self.permissive {
            return true;
        }
        self.cache(cache) && self.keys.lease(Some(cache), key, KeyPurpose::Page).is_ok()
    }
}

#[cfg(test)]
pub(crate) fn for_caches(keys: Rc<Keyring>, caches: Vec<CacheId>) -> Rc<Availability> {
    use super::{caches::CacheDefinition, snapshot::SnapshotStore, wire::*};
    let publications = Arc::new(PublishedState::default());
    SnapshotStore::new(keys.cluster().clone(), publications.clone(), 1)
        .publish(Publication {
            schema_version: SCHEMA_VERSION,
            cluster: keys.cluster().clone(),
            sequence: PublicationSequence(1),
            membership_version: crate::model::MembershipVersion(1),
            members: vec![crate::topology::membership::Member {
                node: keys.node().clone(),
                shares: std::num::NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:7443".into(),
                rails: vec![],
                alignment_enabled: false,
                site: String::new(),
            }],
            caches: caches
                .into_iter()
                .enumerate()
                .map(|(i, id)| {
                    let name = format!("rotation-{i}");
                    let (client_socket, origin_socket) =
                        super::caches::canonical_socket_paths(&name).unwrap();
                    CacheDefinition {
                        id,
                        name,
                        client_socket,
                        origin_socket,
                    }
                })
                .collect(),
        })
        .unwrap();
    Rc::new(Availability::new(publications, keys))
}
