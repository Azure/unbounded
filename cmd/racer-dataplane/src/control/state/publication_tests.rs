use super::*;

#[test]
fn default_two_old_generations_resolve_without_local_request_pins() {
    let published = Arc::new(PublishedState::default());
    let store = SnapshotStore::new(publication(1).cluster, published.clone(), 2);
    for version in 1..20 {
        let mut next = publication(version);
        next.membership_version.0 = version;
        store.publish(next).unwrap();
        for old in version.saturating_sub(2).max(1)..=version {
            assert_eq!(
                published
                    .membership(MembershipVersion(old))
                    .unwrap()
                    .version
                    .0,
                old
            );
        }
        assert!(published.state.lock().unwrap().memberships.len() <= 3);
    }
}

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
        crate::control::wire::decode_publication(include_bytes!("../testdata/publication.json"))
            .unwrap();
    p.sequence.0 = sequence;
    p.membership_version.0 = 1;
    // Lifecycle tests do not depend on the topology Unicode-fabric mismatch.
    // Wire fixture parity is tested in codec.rs.
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
    changed.caches[0].id = crate::model::CacheId("66666666-6666-4666-8666-666666666666".into());
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
fn site_changes_require_new_membership_and_preserve_leased_history() {
    let store = SnapshotStore::new(publication(1).cluster, Arc::new(PublishedState), 3);
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
    let mut removed = publication(3);
    removed.membership_version.0 = 3;
    assert!(
        store.publish(removed).unwrap().membership.members()[0]
            .site
            .is_empty()
    );
}

#[test]
fn staged_resources_commit_only_on_accepted_replacement() {
    struct Transition(std::rc::Rc<std::cell::Cell<usize>>);
    impl CacheTransition for Transition {
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
