use super::*;

#[test]
fn replacement_removes_before_add_and_invalid_update_is_atomic() {
    let registry = CacheRegistry::default();
    let mut defs =
        crate::control::wire::decode_publication(include_bytes!("../testdata/publication.json"))
            .unwrap()
            .caches;
    assert!(matches!(
        registry.reconcile(&defs).unwrap().as_slice(),
        [CacheEvent::Add(_)]
    ));
    assert!(registry.reconcile(&defs).unwrap().is_empty());
    let old = defs[0].id.clone();
    defs[0].id.0 = "66666666-6666-4666-8666-666666666666".into();
    let events = registry.reconcile(&defs).unwrap();
    assert!(matches!(&events[0],CacheEvent::Remove(id) if *id == old));
    assert!(matches!(&events[1], CacheEvent::Add(_)));
    let mut bad = defs.clone();
    bad[0].name = "..".into();
    assert!(registry.reconcile(&bad).is_err());
    assert!(registry.reconcile(&defs).unwrap().is_empty());
    defs[0].name = "renamed".into();
    (defs[0].client_socket, defs[0].origin_socket) = canonical_socket_paths("renamed").unwrap();
    assert!(matches!(
        registry.reconcile(&defs).unwrap().as_slice(),
        [CacheEvent::Update(_)]
    ));
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
