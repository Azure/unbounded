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

    // The worker need not observe the intervening empty cache set to retire the
    // old UID. A cancelled refresh must not mark the replacement as installed.
    worker
        .snapshots
        .publish(integration_tests::publication(&config, 2, vec![]))
        .unwrap();
    let mut replacement = original.clone();
    replacement.id = crate::model::identity::CacheId("55555555-5555-4555-8555-555555555555".into());
    worker
        .snapshots
        .publish(integration_tests::publication(
            &config,
            3,
            vec![replacement.clone()],
        ))
        .unwrap();
    let cancelled = scope(Duration::from_secs(1)).unwrap();
    cancelled.cancel().unwrap();
    assert_eq!(worker.refresh_snapshot(&cancelled), Err(Error::Cancelled));
    assert_eq!(worker.caches, vec![original]);
    assert_eq!(
        worker.snapshot_sequence,
        Some(crate::control::wire::PublicationSequence(1))
    );

    worker.refresh_snapshot(&current_scope).unwrap();
    assert_eq!(worker.caches, vec![replacement]);
    assert_eq!(
        worker.snapshot_sequence,
        Some(crate::control::wire::PublicationSequence(3))
    );
    // An unchanged publication is a no-op, even when the supplied scope is cancelled.
    worker.refresh_snapshot(&cancelled).unwrap();
    assert!(worker.peer_task.is_none());
    assert!(worker.prepared_listeners.borrow().is_none());
}
