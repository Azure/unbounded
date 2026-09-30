//! Scoped admission regression tests replacing the former node barrier tests.
use super::*;

#[test]
fn removal_visibility_changes_without_worker_or_checkpoint_barriers() {
    let config = crate::test_support::cluster::config(false);
    let node = Arc::new(NodeState::default());
    let (worker, _, _) = integration_tests::local_worker(&config, &node, 0);
    let cache = integration_tests::definition();
    let availability = crate::control::availability::Availability::new(
        node.publications.clone(),
        worker.keys.clone(),
    );
    worker
        .snapshots
        .publish(integration_tests::publication(
            &config,
            1,
            vec![cache.clone()],
        ))
        .unwrap();
    assert!(availability.cache(&cache.id));
    worker
        .snapshots
        .publish(integration_tests::publication(&config, 2, vec![]))
        .unwrap();
    assert!(!availability.cache(&cache.id));
    worker
        .snapshots
        .publish(integration_tests::publication(
            &config,
            3,
            vec![cache.clone()],
        ))
        .unwrap();
    assert!(availability.cache(&cache.id));
}

#[test]
fn non_listener_worker_installs_nonempty_cache_set_without_binding_paths() {
    let config = crate::test_support::cluster::config(false);
    let node = Arc::new(NodeState::default());
    let (mut worker, _, _) = integration_tests::local_worker(&config, &node, 1);
    let definition = integration_tests::definition();
    worker
        .snapshots
        .publish(integration_tests::publication(
            &config,
            1,
            vec![definition.clone()],
        ))
        .unwrap();
    worker
        .refresh_snapshot(&scope(Duration::from_secs(1)).unwrap())
        .unwrap();
    assert_eq!(worker.caches, vec![definition]);
    assert!(worker.peer_task.is_none());
    assert!(worker.diagnostic_task.is_none());
    assert!(worker.prepared_listeners.borrow().is_none());
}

#[test]
fn snapshot_refresh_retries_cancelled_publication_and_applies_skipped_removal() {
    use crate::control::wire;

    let config = crate::test_support::cluster::config(false);
    let node = Arc::new(NodeState::default());
    let (mut worker, _, _) = integration_tests::local_worker(&config, &node, 1);
    let original = integration_tests::definition();
    let current_scope = scope(Duration::from_secs(1)).unwrap();
    assert_eq!(
        worker.refresh_snapshot(&current_scope),
        Err(Error::Unavailable)
    );
    worker
        .snapshots
        .publish(integration_tests::publication(
            &config,
            1,
            vec![original.clone()],
        ))
        .unwrap();
    worker.refresh_snapshot(&current_scope).unwrap();
    assert_eq!(worker.caches, vec![original.clone()]);

    let (_, _, roots) = crate::security::identity::tests::issued();
    worker
        .keys
        .install(wire::KeyringBundle {
            schema_version: wire::SCHEMA_VERSION,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(1),
            peer_trust_roots: roots,
            cache_keys: vec![wire::CacheEncryptionKey {
                key: wire::CacheKeyRef {
                    cache: original.id.clone(),
                    id: crate::model::KeyId([7; 16]),
                    purpose: wire::CacheKeyPurpose::Page,
                },
                state: wire::CacheKeyState::Active,
                material: [19; 32],
            }],
        })
        .unwrap();
    let page = integration_tests::page(&worker);
    let page_id = page.plaintext.page().clone();
    let retained_plaintext = Arc::downgrade(&page.plaintext.inner);
    let retained_ciphertext = Arc::downgrade(&page.ciphertext.inner);
    let metadata = page.metadata.immutable();
    let index = worker.store.writer.index().clone();
    index.publish_version(metadata.clone()).unwrap();
    worker.memory.publish(page).unwrap();
    assert!(worker.memory.get(&page_id).unwrap().is_some());
    assert_eq!(retained_plaintext.strong_count(), 1);
    assert_eq!(retained_ciphertext.strong_count(), 1);
    assert_eq!(index.snapshot_metadata(), vec![metadata.clone()]);

    // The worker need not observe the intervening empty cache set to retire the
    // old UID. A cancelled refresh must not mark the replacement as installed.
    worker
        .snapshots
        .publish(integration_tests::publication(&config, 2, vec![]))
        .unwrap();
    let mut replacement = original.clone();
    replacement.id = crate::model::CacheId("55555555-5555-4555-8555-555555555555".into());
    worker
        .snapshots
        .publish(integration_tests::publication(
            &config,
            3,
            vec![replacement.clone()],
        ))
        .unwrap();
    // Publication hides the old UID immediately, but these owners still need
    // worker-local cleanup. Do not let a lookup lazily evict the memory entry.
    assert_eq!(retained_plaintext.strong_count(), 1);
    assert_eq!(retained_ciphertext.strong_count(), 1);
    assert_eq!(index.snapshot_metadata(), vec![metadata]);
    let cancelled = scope(Duration::from_secs(1)).unwrap();
    cancelled.cancel().unwrap();
    assert_eq!(worker.refresh_snapshot(&cancelled), Err(Error::Cancelled));
    assert_eq!(worker.caches, vec![original]);
    assert_eq!(
        worker.snapshot_sequence,
        Some(crate::control::wire::PublicationSequence(1))
    );
    // Removal precedes the scope check, even though installation must retry.
    assert!(retained_plaintext.upgrade().is_none());
    assert!(retained_ciphertext.upgrade().is_none());
    assert!(index.snapshot_metadata().is_empty());

    worker.refresh_snapshot(&current_scope).unwrap();
    assert_eq!(worker.caches, vec![replacement]);
    assert_eq!(
        worker.snapshot_sequence,
        Some(crate::control::wire::PublicationSequence(3))
    );
    // An unchanged publication is a no-op, even when the supplied scope is cancelled.
    worker.refresh_snapshot(&cancelled).unwrap();
    assert!(worker.memory.get(&page_id).unwrap().is_none());
    assert!(index.snapshot_metadata().is_empty());
    assert!(worker.peer_task.is_none());
    assert!(worker.prepared_listeners.borrow().is_none());
}
