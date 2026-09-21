// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[test]
fn physical_evidence_is_transport_only_and_generation_fenced() {
    let world = crate::simulation::World::new(92);
    let _scope = world.enter();
    let mut owners = Owners::default();
    let identity = [1; 32];
    // A semantic owner-unavailable report is scoped to its logical slot.
    owners
        .acquire_final(identity, 1, Some("A"))
        .unwrap()
        .failure(world.now());
    assert!(owners.evidence(identity, 1).is_some());
    assert!(owners.physical_evidence(identity, "A").is_none());
    let old_success = owners.acquire_final(identity, 2, Some("A")).unwrap();
    let old_failure = owners.acquire_final(identity, 3, Some("A")).unwrap();
    owners
        .acquire_final(identity, 4, Some("A"))
        .unwrap()
        .transport_failure(world.now());
    let observed = owners.physical_evidence(identity, "A").unwrap();
    old_success.success();
    assert_eq!(owners.physical_evidence(identity, "A"), Some(observed));
    assert!(owners.acquire_final(identity, 5, Some("A")).is_err());
    assert!(owners.acquire_final(identity, 5, Some("B")).is_ok());
    assert!(owners.acquire_final([2; 32], 5, Some("A")).is_ok());
    assert!(owners.acquire_final(identity, 5, None).is_ok());
    assert!(owners.physical_evidence(identity, "B").is_none());
    assert!(owners.physical_evidence([2; 32], "A").is_none());
    world.advance(COOLDOWN);
    assert!(owners.physical_evidence(identity, "A").is_none());
    let probe = owners.acquire_final(identity, 5, Some("A")).unwrap();
    old_failure.transport_failure(world.now());
    assert!(owners.physical_evidence(identity, "A").is_none());
    assert!(owners.acquire_final(identity, 6, Some("A")).is_err());
    probe.success();
    assert!(!owners.physical_needs_probe(identity, "A"));
    owners
        .acquire_final(identity, 6, Some("A"))
        .unwrap()
        .transport_failure(world.now());
    world.advance(COOLDOWN);
    drop(owners.acquire_final(identity, 7, Some("A")).unwrap());
    assert!(owners.physical_needs_probe(identity, "A"));
    assert!(
        owners.physical_evidence(identity, "A").is_none(),
        "cancel cannot renew evidence"
    );
}
#[test]
fn owner_cooldowns_fence_stale_completion_and_bound_live_entries() {
    let world = crate::simulation::World::new(91);
    let _scope = world.enter();
    let mut owners = Owners::default();
    let identity = [1; 32];
    let old_success = owners.acquire(identity, 3).unwrap();
    let old_failure = owners.acquire(identity, 3).unwrap();
    owners
        .acquire(identity, 3)
        .unwrap()
        .failure(crate::environment::now());
    old_success.success();
    assert!(owners.blocked(identity, 3));
    let observed = owners.evidence(identity, 3).unwrap();
    assert!(!owners.blocked([2; 32], 3));
    world.advance(COOLDOWN);
    let probe = owners.acquire(identity, 3).unwrap();
    assert!(owners.acquire(identity, 3).is_err());
    old_failure.failure(crate::environment::now());
    assert_eq!(
        *owners.0[&(identity, Key::Slot(3))].observed.borrow(),
        Some(observed)
    );
    probe.success();
    assert!(!owners.blocked(identity, 3));
    assert!(owners.evidence(identity, 3).is_none());
    owners
        .acquire(identity, 3)
        .unwrap()
        .failure(crate::environment::now());
    world.advance(COOLDOWN);
    drop(owners.acquire(identity, 3).unwrap()); // cancellation/cache hit is not recovery
    assert!(owners.blocked(identity, 3));
    assert!(
        owners.evidence(identity, 3).is_none(),
        "cancelled probe must not refresh owner evidence"
    );
    let permits: Vec<_> = (10..4105)
        .map(|slot| owners.acquire(identity, slot).unwrap())
        .collect();
    assert!(owners.acquire(identity, 9999).is_err());
    assert_eq!(owners.0.len(), 4096);
    drop(permits);
    assert!(owners.acquire(identity, 9999).is_ok());
    assert_eq!(owners.0.len(), 4096);
}
