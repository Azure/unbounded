//! Validate and atomically publish complete immutable state; retain last good state.
use super::{
    caches::CacheDefinition,
    wire::{Publication, PublicationSequence},
};
use crate::{
    error::{Error, Result},
    model::identity::ClusterId,
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
    old: Vec<(Weak<Snapshot>, Weak<Membership>)>,
    content_hash: [u8; 32],
    membership_hash: [u8; 32],
}
#[allow(non_upper_case_globals)]
pub const PublishedState: PublishedState = PublishedState {
    state: Mutex::new(State {
        current: None,
        old: Vec::new(),
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
        state
            .old
            .retain(|(s, m)| s.strong_count() != 0 || m.strong_count() != 0);
        let retain_current = state
            .current
            .as_ref()
            .is_some_and(|s| Arc::strong_count(s) > 1 || Arc::strong_count(&s.membership) > 1);
        if state.old.len() + usize::from(retain_current) > self.retained_limit {
            return Err(Error::Overloaded);
        }
        let next = Arc::new(Snapshot {
            cluster: publication.cluster,
            sequence: publication.sequence,
            membership: Arc::new(Membership::validate(
                publication.membership_version,
                publication.members,
            )?),
            caches: publication.caches,
        });
        if retain_current {
            let old = state.current.as_ref().unwrap();
            let weak = (Arc::downgrade(old), Arc::downgrade(&old.membership));
            state.old.push(weak);
        }
        if let Some(transition) = transition {
            transition.commit();
        }
        state.current = Some(next.clone());
        state.content_hash = content_hash;
        state.membership_hash = membership_hash;
        Ok(next)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
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
        changed.caches[0].socket_mode = 0o600;
        assert!(matches!(store.publish(changed), Err(Error::Replay)));
        let second = store.publish(publication(2)).unwrap();
        assert!(matches!(store.publish(publication(1)), Err(Error::Replay)));
        assert!(matches!(
            store.publish(publication(3)),
            Err(Error::Overloaded)
        ));
        assert_eq!(store.cursor().unwrap(), Some(PublicationSequence(2)));
        drop(first);
        assert!(store.publish(publication(3)).is_ok());
        drop(second);
        let mut changed = publication(4);
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
        assert!(matches!(
            store.publish_staged(
                publication(2),
                Some(Box::new(Transition(committed.clone())))
            ),
            Err(Error::Overloaded)
        ));
        assert_eq!(committed.get(), 1);
        drop(first);
        store
            .publish_staged(
                publication(2),
                Some(Box::new(Transition(committed.clone()))),
            )
            .unwrap();
        assert_eq!(committed.get(), 2);
    }
}
