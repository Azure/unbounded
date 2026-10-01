//! Validate and atomically publish complete immutable state; retain last good state.
use super::wire::{Publication, PublicationSequence};
use crate::{
    error::{Error, Result},
    model::{CacheId, ClusterId, KeyId, MembershipVersion},
    runtime::collections::HashSet,
    security::identity::{KeyPurpose, Keyring},
    topology::membership::{Membership, MembershipLease},
};
use sha2::{Digest, Sha256};
use std::{
    path::PathBuf,
    rc::Rc,
    sync::{Arc, Mutex, Weak},
};
pub struct Snapshot {
    pub cluster: ClusterId,
    pub sequence: PublicationSequence,
    pub membership: MembershipLease,
    pub caches: Vec<CacheDefinition>,
}
pub type SnapshotLease = Arc<Snapshot>;
pub struct PreparedPublication {
    pub(super) snapshot: SnapshotLease,
    content_hash: [u8; 32],
    membership_hash: [u8; 32],
}
impl PreparedPublication {
    pub fn content_hash(&self) -> String {
        self.content_hash
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect()
    }
}
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
    grace: Vec<(std::time::Instant, MembershipLease)>,
}
#[allow(non_upper_case_globals)]
pub const PublishedState: PublishedState = PublishedState {
    state: Mutex::new(State {
        current: None,
        memberships: Vec::new(),
        content_hash: [0; 32],
        membership_hash: [0; 32],
        grace: Vec::new(),
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
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        let now = crate::runtime::environment::now();
        state.grace.retain(|(until, _)| *until > now);
        state
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
                grace: Vec::new(),
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
    /// Read-only coherent accepted identity; never describes a prepared update.
    pub(crate) fn accepted_identity(&self) -> Result<(u64, u64, [u8; 32])> {
        let state = self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?;
        let current = state.current.as_ref().ok_or(Error::Unavailable)?;
        Ok((
            current.sequence.0,
            current.membership.version.0,
            state.membership_hash,
        ))
    }

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
    pub fn content_hash(&self, sequence: PublicationSequence) -> Result<String> {
        let state = self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?;
        if state
            .current
            .as_ref()
            .is_none_or(|s| s.sequence != sequence)
        {
            return Err(Error::Replay);
        }
        Ok(state
            .content_hash
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect())
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
        transition: Option<Box<dyn CacheTransition>>,
    ) -> Result<SnapshotLease> {
        let prepared = self.prepare(publication)?;
        self.publish_prepared(&prepared, transition)
    }
    pub fn prepare(&self, publication: Publication) -> Result<PreparedPublication> {
        if publication.cluster != self.cluster {
            return Err(Error::Unauthorized);
        }
        let publication = super::wire::validate_publication(&publication)?;
        let (content, membership) = super::wire::canonical_content(&publication)?;
        let content_hash: [u8; 32] = Sha256::digest(content).into();
        let membership_hash: [u8; 32] = Sha256::digest(membership).into();
        let current = self.current().ok();
        let mut validated =
            Membership::validate(publication.membership_version, publication.members)?;
        if let Some(current) = &current {
            validated = validated.with_predecessor(&current.membership);
        }
        let mut next = Snapshot {
            cluster: publication.cluster,
            sequence: publication.sequence,
            membership: Arc::new(validated),
            caches: publication.caches,
        };
        if let Some(current) = current {
            if current.membership.version == next.membership.version {
                next.membership = current.membership.clone();
            }
        }
        let prepared = PreparedPublication {
            snapshot: Arc::new(next),
            content_hash,
            membership_hash,
        };
        self.check_prepared(&prepared)?;
        Ok(prepared)
    }
    fn check_prepared(&self, prepared: &PreparedPublication) -> Result<()> {
        let state = self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?;
        Self::check_state(&state, prepared)
    }
    fn check_state(state: &State, prepared: &PreparedPublication) -> Result<()> {
        let next = &prepared.snapshot;
        if let Some(old) = &state.current {
            if next.sequence < old.sequence || next.membership.version.0 < old.membership.version.0
            {
                return Err(Error::Replay);
            }
            if next.sequence == old.sequence
                && (prepared.content_hash != state.content_hash
                    || next.membership.version != old.membership.version)
            {
                return Err(Error::Replay);
            }
            if next.membership.version == old.membership.version
                && prepared.membership_hash != state.membership_hash
            {
                return Err(Error::IncompatibleMembership);
            }
        }
        Ok(())
    }
    pub fn publish_prepared(
        &self,
        prepared: &PreparedPublication,
        transition: Option<Box<dyn CacheTransition>>,
    ) -> Result<SnapshotLease> {
        let mut state = self
            .published
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?;
        Self::check_state(&state, prepared)?;
        let publication = &prepared.snapshot;
        if let Some(old) = state
            .current
            .as_ref()
            .filter(|old| old.sequence == publication.sequence)
        {
            return Ok(old.clone());
        }
        let now = crate::runtime::environment::now();
        state.grace.retain(|(until, _)| *until > now);
        // Grace-only owners are disposable under the configured generation bound.
        // Externally pinned operations still block replacement rather than revoke.
        while state.memberships.len() > self.retained_limit && !state.grace.is_empty() {
            let Some(index) = state
                .grace
                .iter()
                .position(|(_, m)| Arc::strong_count(m) == 1)
            else {
                break;
            };
            state.grace.remove(index);
            state.memberships.retain(|(_, m)| m.strong_count() != 0);
        }
        state.memberships.retain(|(_, m)| m.strong_count() != 0);
        let same_membership = state
            .current
            .as_ref()
            .filter(|s| s.membership.version == publication.membership.version);
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
            if state.memberships.len() - usize::from(replaceable) > self.retained_limit {
                return Err(Error::Overloaded);
            }
            publication.membership.clone()
        };
        // Preparation shares the current membership for cache-only updates. A
        // concurrently replaced base must be prepared again, never cloned here.
        if !Arc::ptr_eq(&membership, &publication.membership) {
            return Err(Error::Replay);
        }
        let next = publication.clone();
        if let Some(transition) = transition {
            transition.commit();
        }
        if self.retained_limit >= 2 {
            if let Some(old) = &state.current {
                if old.membership.version != next.membership.version {
                    let old = old.membership.clone();
                    state
                        .grace
                        .push((now + std::time::Duration::from_secs(30), old));
                    while state.grace.len() > self.retained_limit
                        || state
                            .grace
                            .iter()
                            .map(|(_, m)| m.retained_bytes())
                            .sum::<usize>()
                            > 128 * 1024 * 1024
                    {
                        state.grace.remove(0);
                    }
                }
            }
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
        state.content_hash = prepared.content_hash;
        state.membership_hash = prepared.membership_hash;
        Ok(next)
    }
}
/// Cache definitions. Removal closes new admission while
/// accepted socket, key, and I/O owners drain independently.
///
/// Socket paths are fixed: /run/racer/<cache name>/client/socket and
/// /run/racer/<cache name>/origin/socket. Separate endpoint directories let pods
/// mount only the endpoint authorized by a future admission controller. The
/// dataplane owns the client listener; the application adapter owns the origin
/// listener. Never unlink an adapter-owned origin socket during cache removal.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CacheDefinition {
    pub id: CacheId,
    /// ClusterCache name, distinct from its UID and one safe path component.
    pub name: String,
    /// Must equal /run/racer/<name>/client/socket, not an arbitrary supplied path.
    pub client_socket: PathBuf,
    /// Must equal /run/racer/<name>/origin/socket, not an arbitrary supplied path.
    pub origin_socket: PathBuf,
}
/// A sole owner stages all listener/resource changes before publication. Dropping
/// an uncommitted transition must undo preparation. Commit cannot fail; removal
/// stops admission and arranges drain/fences before releasing old resources.
pub trait CacheTransition {
    fn commit(self: Box<Self>);
}
pub fn canonical_socket_paths(name: &str) -> Result<(PathBuf, PathBuf)> {
    if name.is_empty()
        || name.len() > 253
        || !name.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && label.bytes().enumerate().all(|(i, b)| {
                    b.is_ascii_lowercase()
                        || b.is_ascii_digit()
                        || b == b'-' && i != 0 && i + 1 != label.len()
                })
        })
    {
        return Err(Error::InvalidRequest);
    }
    let client = format!("/run/racer/{name}/client/socket");
    let origin = format!("/run/racer/{name}/origin/socket");
    if client.len() > 107 || origin.len() > 107 {
        return Err(Error::InvalidRequest);
    }
    Ok((client.into(), origin.into()))
}
pub fn validate_definitions(definitions: &[CacheDefinition]) -> Result<()> {
    let mut ids = HashSet::default();
    let mut names = HashSet::default();
    for d in definitions {
        if !super::wire::valid_uuid(&d.id.0) || !ids.insert(&d.id) || !names.insert(&d.name) {
            return Err(Error::InvalidRequest);
        }
        let (client, origin) = canonical_socket_paths(&d.name)?;
        // Path equality normalizes separators; the wire requires exact strings.
        if d.client_socket.as_os_str() != client.as_os_str()
            || d.origin_socket.as_os_str() != origin.as_os_str()
        {
            return Err(Error::InvalidRequest);
        }
    }
    Ok(())
}

/// Current positive admission set, shared by cache lookups and late publications.
/// No removal history is needed: an absent UID/key is a miss. Reintroducing a UID
/// denotes the same immutable namespace; a different namespace requires a new UID.
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

#[cfg(test)]
pub(crate) fn for_caches(keys: Rc<Keyring>, caches: Vec<CacheId>) -> Rc<Availability> {
    use super::wire::*;
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
                    let (client_socket, origin_socket) = canonical_socket_paths(&name).unwrap();
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

#[cfg(test)]
mod cache_tests {
    use super::*;
    #[test]
    fn replacement_definitions_and_socket_paths_are_validated() {
        let mut defs =
            crate::control::wire::decode_publication(include_bytes!("testdata/publication.json"))
                .unwrap()
                .caches;
        validate_definitions(&defs).unwrap();
        defs[0].id.0 = "66666666-6666-4666-8666-666666666666".into();
        validate_definitions(&defs).unwrap();
        let mut bad = defs.clone();
        bad[0].name = "..".into();
        assert!(validate_definitions(&bad).is_err());
        validate_definitions(&defs).unwrap();
        defs[0].name = "renamed".into();
        (defs[0].client_socket, defs[0].origin_socket) = canonical_socket_paths("renamed").unwrap();
        validate_definitions(&defs).unwrap();
        for name in ["", ".", "..", "a/b", "A", "-a", "a-", "a..b"] {
            assert!(canonical_socket_paths(name).is_err());
        }
        let maximum = format!("{}.{}", "a".repeat(63), "b".repeat(18));
        assert_eq!(
            canonical_socket_paths(&maximum)
                .unwrap()
                .0
                .as_os_str()
                .len(),
            107
        );
        assert!(canonical_socket_paths(&(maximum + "b")).is_err());
    }
}
#[cfg(test)]
mod publication_tests {
    use super::*;
    fn publication(sequence: u64) -> Publication {
        let mut p =
            crate::control::wire::decode_publication(include_bytes!("testdata/publication.json"))
                .unwrap();
        p.sequence.0 = sequence;
        p.membership_version.0 = 1;
        // Lifecycle cases use ASCII fabric IDs; wire parity has separate coverage.
        for member in &mut p.members {
            for rail in &mut member.rails {
                rail.fabric = "fabric-a".into();
            }
        }
        p
    }
    fn store(retained: usize) -> SnapshotStore {
        SnapshotStore::new(
            publication(1).cluster,
            Arc::new(PublishedState::default()),
            retained,
        )
    }
    fn membership(sequence: u64, version: u64) -> Publication {
        let mut next = publication(sequence);
        next.membership_version.0 = version;
        next
    }
    #[test]
    fn default_two_old_generations_resolve_without_local_request_pins() {
        let store = store(2);
        for version in 1..20 {
            store.publish(membership(version, version)).unwrap();
            for old in version.saturating_sub(2).max(1)..=version {
                assert_eq!(
                    store
                        .published
                        .membership(MembershipVersion(old))
                        .unwrap()
                        .version
                        .0,
                    old
                );
            }
            assert!(store.published.state.lock().unwrap().memberships.len() <= 3);
        }
    }
    #[test]
    fn cache_only_history_uses_one_slot_and_weak_registry_stays_bounded() {
        let store = store(0);
        let first = store.publish(publication(1)).unwrap();
        let mut history = vec![first.clone()];
        for sequence in 2..40 {
            let mut next = publication(sequence);
            next.caches.clear();
            let snapshot = store.publish(next).unwrap();
            assert!(Arc::ptr_eq(&first.membership, &snapshot.membership));
            history.push(snapshot);
            assert_eq!(store.published.state.lock().unwrap().memberships.len(), 1);
        }
        let next = membership(40, 2);
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
            store.publish(membership(version + 40, version)).unwrap();
            assert_eq!(store.published.state.lock().unwrap().memberships.len(), 1);
            assert!(matches!(
                store.published.membership(MembershipVersion(version - 1)),
                Err(Error::IncompatibleMembership)
            ));
        }
    }
    #[test]
    fn delayed_thread_lease_blocks_admission_until_release() {
        let store = store(1);
        let published = store.published.clone();
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
        let current = store.publish(membership(2, 2)).unwrap();
        assert!(matches!(
            store.publish(membership(3, 3)),
            Err(Error::Overloaded)
        ));
        release_tx.send(()).unwrap();
        worker.join().unwrap();
        store.publish(membership(3, 3)).unwrap();
        assert_eq!(current.membership.version, MembershipVersion(2));
    }
    #[test]
    fn atomic_replay_rollback_and_leased_history() {
        let store = store(1);
        let first = store.publish(publication(1)).unwrap();
        assert!(Arc::ptr_eq(&first, &store.publish(publication(1)).unwrap()));
        let mut changed = publication(1);
        changed.caches[0].id = CacheId("66666666-6666-4666-8666-666666666666".into());
        assert!(matches!(store.publish(changed), Err(Error::Replay)));
        let second = store.publish(membership(2, 2)).unwrap();
        assert!(matches!(store.publish(publication(1)), Err(Error::Replay)));
        assert!(matches!(
            store.publish(membership(3, 3)),
            Err(Error::Overloaded)
        ));
        assert_eq!(store.cursor().unwrap(), Some(PublicationSequence(2)));
        drop(first);
        assert!(store.publish(membership(3, 3)).is_ok());
        drop(second);
        let mut changed = membership(4, 3);
        changed.members[0].peer_endpoint = "192.0.2.7:7443".into();
        assert!(matches!(
            store.publish(changed.clone()),
            Err(Error::IncompatibleMembership)
        ));
        changed.membership_version.0 += 1;
        assert!(store.publish(changed).is_ok());
    }
    #[test]
    fn site_changes_require_new_membership_and_preserve_leased_history() {
        let store = store(3);
        let old = store.publish(publication(1)).unwrap();
        let mut next = publication(2);
        next.members[0].site = "site1".into();
        assert!(matches!(
            store.publish(next.clone()),
            Err(Error::IncompatibleMembership)
        ));
        next.membership_version.0 = 2;
        let current = store.publish(next).unwrap();
        assert!(old.membership.members()[0].site.is_empty());
        assert_eq!(current.membership.members()[0].site, "site1");
        assert_eq!(
            old.membership.placement_identity(),
            current.membership.placement_identity()
        );
        assert!(
            store
                .publish(membership(3, 3))
                .unwrap()
                .membership
                .members()[0]
                .site
                .is_empty()
        );
    }
    #[test]
    fn staged_resources_commit_only_on_accepted_replacement() {
        struct Transition(Rc<std::cell::Cell<usize>>);
        impl CacheTransition for Transition {
            fn commit(self: Box<Self>) {
                self.0.set(self.0.get() + 1);
            }
        }
        let committed = Rc::new(std::cell::Cell::new(0));
        let store = store(0);
        let publish = |p| store.publish_staged(p, Some(Box::new(Transition(committed.clone()))));
        let first = publish(publication(1)).unwrap();
        assert_eq!(committed.get(), 1);
        assert!(matches!(publish(membership(2, 2)), Err(Error::Overloaded)));
        assert_eq!(committed.get(), 1);
        drop(first);
        publish(membership(2, 2)).unwrap();
        assert_eq!(committed.get(), 2);
    }
}
