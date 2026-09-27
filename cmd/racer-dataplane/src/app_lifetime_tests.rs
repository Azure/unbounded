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
    futures::executor::block_on(worker.refresh_snapshot(&scope(Duration::from_secs(1)).unwrap()))
        .unwrap();
    assert_eq!(worker.caches, vec![definition]);
    assert!(worker.peer_task.is_none());
    assert!(worker.diagnostic_task.is_none());
    assert!(worker.prepared_listeners.borrow().is_none());
}
