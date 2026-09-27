//! Validate and atomically publish complete immutable state; retain last good state.
use super::{
    caches::CacheDefinition,
    wire::{Publication, PublicationSequence},
};
use crate::{
    error::{Error, Result},
    model::identity::{ClusterId, MembershipVersion},
    topology::membership::{Membership, MembershipLease},
};
use sha2::{Digest, Sha256};
use std::sync::{Arc, Mutex, Weak};
pub struct Snapshot {
    pub cluster: ClusterId,
    pub sequence: PublicationSequence,
    pub membership: MembershipLease,
    pub caches: Vec<CacheDefinition>,
}
pub type SnapshotLease = Arc<Snapshot>;
/// One node-wide publication cell. Its implementation publishes immutable leases
/// atomically; worker handles never become independent authorities for membership.
pub struct PublishedState {
    state: Mutex<State>,
}
struct State {
    current: Option<SnapshotLease>,
    memberships: Vec<(MembershipVersion, Weak<Membership>)>,
    content_hash: [u8; 32],
    membership_hash: [u8; 32],
}
#[allow(non_upper_case_globals)]
pub const PublishedState: PublishedState = PublishedState {
    state: Mutex::new(State {
        current: None,
        memberships: Vec::new(),
        content_hash: [0; 32],
        membership_hash: [0; 32],
    }),
};
impl Default for PublishedState {
    fn default() -> Self {
        PublishedState
    }
}
impl PublishedState {
    /// Incoming wire versions resolve here once, then travel as operation leases.
    /// Weak entries never prolong a generation's lifetime and publication prunes
    /// dead entries before admission, bounding the registry as well as live state.
    pub fn membership(&self, version: MembershipVersion) -> Result<MembershipLease> {
        self.state
            .lock()
            .map_err(|_| Error::Unavailable)?
            .memberships
            .iter()
            .find(|(v, _)| *v == version)
            .and_then(|(_, membership)| membership.upgrade())
            .ok_or(Error::IncompatibleMembership)
    }

    #[cfg(test)]
    pub(crate) fn for_membership(membership: MembershipLease) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::new(State {
                memberships: vec![(membership.version, Arc::downgrade(&membership))],
                current: Some(Arc::new(Snapshot {
                    cluster: ClusterId("test".into()),
                    sequence: PublicationSequence(1),
                    membership,
                    caches: vec![],
                })),
                content_hash: [0; 32],
                membership_hash: [0; 32],
            }),
        })
    }

    pub fn current(&self) -> Result<SnapshotLease> {
        self.state
            .lock()
            .map_err(|_| Error::Unavailable)?
            .current
            .clone()
            .ok_or(Error::Unavailable)
    }
}
pub struct SnapshotStore {
    cluster: ClusterId,
    published: Arc<PublishedState>,
    /// Maximum old live membership generations, in addition to the current one.
    retained_limit: usize,
}
impl SnapshotStore {
    pub fn new(cluster: ClusterId, published: Arc<PublishedState>, retained_limit: usize) -> Self {
        Self {
            cluster,
            published,
            retained_limit,
        }
    }
    /// Cursor advances only after complete validation and atomic acceptance.
    pub fn cursor(&self) -> Result<Option<PublicationSequence>> {
        Ok(self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?
            .current
            .as_ref()
            .map(|s| s.sequence))
    }
    pub fn current(&self) -> Result<SnapshotLease> {
        self.published.current()
    }
    /// Reject cluster mismatch, rollback, conflicting replay, or invalid members.
    /// Skipped sequences are legal; disconnected nodes retain the last good state.
    pub fn publish(&self, publication: Publication) -> Result<SnapshotLease> {
        self.publish_staged(publication, None)
    }
    /// Commit prepared cache resources after every fallible validation, before the
    /// new immutable publication becomes visible to any reader of the shared cell.
    pub fn publish_staged(
        &self,
        publication: Publication,
        transition: Option<Box<dyn super::caches::CacheTransition>>,
    ) -> Result<SnapshotLease> {
        self.validate_or_publish(publication, transition, true)
    }
    /// Validate the entire downloaded publication before local resource staging.
    /// Capacity can change while leases drain, so installation rechecks it.
    pub fn validate(&self, publication: Publication) -> Result<()> {
        self.validate_or_publish(publication, None, false)
            .map(|_| ())
    }
    fn validate_or_publish(
        &self,
        publication: Publication,
        transition: Option<Box<dyn super::caches::CacheTransition>>,
        install: bool,
    ) -> Result<SnapshotLease> {
        if publication.cluster != self.cluster {
            return Err(Error::Unauthorized);
        }
        // The codec is also the validator for direct, in-process publications.
        let publication =
            super::wire::decode_publication(&super::wire::encode_publication(&publication)?)?;
        let (content, membership) = super::wire::canonical_content(&publication)?;
        let content_hash: [u8; 32] = Sha256::digest(content).into();
        let membership_hash: [u8; 32] = Sha256::digest(membership).into();
        let mut state = self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?;
        if let Some(old) = &state.current {
            if publication.sequence < old.sequence
                || publication.membership_version.0 < old.membership.version.0
            {
                return Err(Error::Replay);
            }
            if publication.sequence == old.sequence {
                return if content_hash == state.content_hash
                    && publication.membership_version == old.membership.version
                {
                    Ok(old.clone())
                } else {
                    Err(Error::Replay)
                };
            }
            if publication.membership_version == old.membership.version
                && membership_hash != state.membership_hash
            {
                return Err(Error::IncompatibleMembership);
            }
        }
        state.memberships.retain(|(_, m)| m.strong_count() != 0);
        let same_membership = state
            .current
            .as_ref()
            .filter(|s| s.membership.version == publication.membership_version);
        let membership = if let Some(current) = same_membership {
            current.membership.clone()
        } else {
            // Only the current publication's sole structural lease can disappear
            // on replacement. External snapshot and membership leases both count.
            // Resolution and admission share this lock, so an incoming request
            // cannot acquire that lease between this check and replacement.
            let replaceable = state.current.as_ref().is_some_and(|s| {
                Arc::strong_count(s) == 1 && Arc::strong_count(&s.membership) == 1
            });
            if install && state.memberships.len() - usize::from(replaceable) > self.retained_limit {
                return Err(Error::Overloaded);
            }
            Arc::new(Membership::validate(
                publication.membership_version,
                publication.members,
            )?)
        };
        let next = Arc::new(Snapshot {
            cluster: publication.cluster,
            sequence: publication.sequence,
            membership,
            caches: publication.caches,
        });
        if !install {
            return Ok(next);
        }
        if let Some(transition) = transition {
            transition.commit();
        }
        state.current = Some(next.clone());
        state.memberships.retain(|(_, m)| m.strong_count() != 0);
        if !state
            .memberships
            .iter()
            .any(|(v, _)| *v == next.membership.version)
        {
            state
                .memberships
                .push((next.membership.version, Arc::downgrade(&next.membership)));
        }
        state.content_hash = content_hash;
        state.membership_hash = membership_hash;
        Ok(next)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn cache_only_history_uses_one_slot_and_weak_registry_stays_bounded() {
        let published = Arc::new(PublishedState::default());
        let store = SnapshotStore::new(publication(1).cluster, published.clone(), 0);
        let first = store.publish(publication(1)).unwrap();
        let mut history = vec![first.clone()];
        for sequence in 2..40 {
            let mut next = publication(sequence);
            next.caches.clear();
            let snapshot = store.publish(next).unwrap();
            assert!(Arc::ptr_eq(&first.membership, &snapshot.membership));
            history.push(snapshot);
            assert_eq!(published.state.lock().unwrap().memberships.len(), 1);
        }
        let mut next = publication(40);
        next.membership_version.0 = 2;
        assert!(matches!(
            store.publish(next.clone()),
            Err(Error::Overloaded)
        ));
        let weak = Arc::downgrade(&first.membership);
        drop(first);
        drop(history);
        store.publish(next).unwrap();
        assert!(weak.upgrade().is_none());
        for version in 3..100 {
            let mut next = publication(version + 40);
            next.membership_version.0 = version;
            store.publish(next).unwrap();
            assert_eq!(published.state.lock().unwrap().memberships.len(), 1);
            assert!(matches!(
                published.membership(MembershipVersion(version - 1)),
                Err(Error::IncompatibleMembership)
            ));
        }
    }

    #[test]
    fn delayed_thread_lease_blocks_admission_until_release() {
        let published = Arc::new(PublishedState::default());
        let store = SnapshotStore::new(publication(1).cluster, published.clone(), 1);
        store.publish(publication(1)).unwrap();
        let (ready_tx, ready_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        let worker = std::thread::spawn(move || {
            let snapshot = published.current().unwrap();
            ready_tx.send(()).unwrap();
            release_rx.recv().unwrap();
            let incoming = published.membership(MembershipVersion(1)).unwrap();
            assert!(Arc::ptr_eq(&snapshot.membership, &incoming));
        });
        ready_rx.recv().unwrap();
        let mut next = publication(2);
        next.membership_version.0 = 2;
        let current = store.publish(next.clone()).unwrap();
        next.sequence.0 = 3;
        next.membership_version.0 = 3;
        assert!(matches!(
            store.publish(next.clone()),
            Err(Error::Overloaded)
        ));
        release_tx.send(()).unwrap();
        worker.join().unwrap();
        store.publish(next).unwrap();
        assert_eq!(current.membership.version, MembershipVersion(2));
    }

    fn publication(sequence: u64) -> Publication {
        let mut p =
            crate::control::wire::decode_publication(include_bytes!("testdata/publication.json"))
                .unwrap();
        p.sequence.0 = sequence;
        p.membership_version.0 = 1;
        // Lifecycle tests do not depend on the separately documented topology
        // Unicode-fabric mismatch. Wire fixture parity is tested in codec.rs.
        for member in &mut p.members {
            for rail in &mut member.rails {
                rail.fabric = "fabric-a".into();
            }
        }
        p
    }
    #[test]
    fn atomic_replay_rollback_and_leased_history() {
        let store = SnapshotStore::new(publication(1).cluster, Arc::new(PublishedState), 1);
        let first = store.publish(publication(1)).unwrap();
        assert!(Arc::ptr_eq(&first, &store.publish(publication(1)).unwrap()));
        let mut changed = publication(1);
        changed.caches[0].id =
            crate::model::identity::CacheId("66666666-6666-4666-8666-666666666666".into());
        assert!(matches!(store.publish(changed), Err(Error::Replay)));
        let mut next = publication(2);
        next.membership_version.0 = 2;
        let second = store.publish(next.clone()).unwrap();
        assert!(matches!(store.publish(publication(1)), Err(Error::Replay)));
        next.sequence.0 = 3;
        next.membership_version.0 = 3;
        assert!(matches!(
            store.publish(next.clone()),
            Err(Error::Overloaded)
        ));
        assert_eq!(store.cursor().unwrap(), Some(PublicationSequence(2)));
        drop(first);
        assert!(store.publish(next).is_ok());
        drop(second);
        let mut changed = publication(4);
        changed.membership_version.0 = 3;
        changed.members[0].peer_endpoint = "192.0.2.7:7443".into();
        assert!(matches!(
            store.publish(changed.clone()),
            Err(Error::IncompatibleMembership)
        ));
        changed.membership_version.0 += 1;
        assert!(store.publish(changed).is_ok());
    }
    #[test]
    fn staged_resources_commit_only_on_accepted_replacement() {
        struct Transition(std::rc::Rc<std::cell::Cell<usize>>);
        impl super::super::caches::CacheTransition for Transition {
            fn commit(self: Box<Self>) {
                self.0.set(self.0.get() + 1);
            }
        }
        let committed = std::rc::Rc::new(std::cell::Cell::new(0));
        let store = SnapshotStore::new(publication(1).cluster, Arc::new(PublishedState), 0);
        let first = store
            .publish_staged(
                publication(1),
                Some(Box::new(Transition(committed.clone()))),
            )
            .unwrap();
        assert_eq!(committed.get(), 1);
        let mut next = publication(2);
        next.membership_version.0 = 2;
        assert!(matches!(
            store.publish_staged(next.clone(), Some(Box::new(Transition(committed.clone())))),
            Err(Error::Overloaded)
        ));
        assert_eq!(committed.get(), 1);
        drop(first);
        store
            .publish_staged(next, Some(Box::new(Transition(committed.clone()))))
            .unwrap();
        assert_eq!(committed.get(), 2);
    }
}
