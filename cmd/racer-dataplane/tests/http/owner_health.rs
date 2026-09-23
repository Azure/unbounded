// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::{handlers::Attempt, http_auth::failure_tests::route, outcome::PeerFailure};
use std::cell::Cell;

fn fixture() -> (Owners, Rc<Cell<Instant>>) {
    let clock = Rc::new(Cell::new(crate::environment::now()));
    let read_clock = clock.clone();
    let mut owners = Owners::default();
    owners.0.insert(
        ([0; 32], Key::Slot(3)),
        OwnerHealth {
            breaker: crate::breaker::CircuitBreaker::with_clock(COOLDOWN, move || read_clock.get()),
            observed: Rc::new(RefCell::new(None)),
        },
    );
    (owners, clock)
}

fn expire(owners: &Owners, clock: &Cell<Instant>, key: Key) {
    owners.0[&([0; 32], key)]
        .observed
        .borrow_mut()
        .as_mut()
        .unwrap()
        .at = crate::environment::now() - 2 * COOLDOWN;
    clock.set(clock.get() + 2 * COOLDOWN);
}

#[test]
fn indirect_owner_recovery_does_not_rearm_on_relay_cache_hit() {
    let (mut owners, clock) = fixture();
    let mut attempt = Some(Attempt {
        route: route(false),
        owner: Some(owners.acquire_final([0; 32], 3, None).unwrap()),
    });
    crate::http_auth::failure::reported(
        PeerFailure {
            identity: [0; 32],
            candidate: 3,
            reason: crate::outcome::PeerReason::OwnerUnavailable,
            evidence: None,
        },
        &mut attempt,
    )
    .unwrap_err();
    assert!(owners.evidence([0; 32], 3).is_some());
    assert!(owners.blocked([0; 32], 3));

    // Advance both clocks without sleeping: observation age and breaker time.
    expire(&owners, &clock, Key::Slot(3));
    assert!(owners.evidence([0; 32], 3).is_none());
    let mut recovery = Attempt {
        route: route(false),
        owner: Some(owners.acquire_final([0; 32], 3, None).unwrap()),
    };
    recovery.owner_reachable();
    assert!(
        recovery.owner.is_some(),
        "relay hit must not claim owner reachability"
    );
    drop(recovery);
    assert!(
        !owners.blocked([0; 32], 3),
        "expired indirect evidence must not rearm suppression"
    );
    assert!(owners.acquire_final([0; 32], 3, None).is_ok());
}

#[test]
fn indirect_expiry_fences_old_completions_and_allows_overlapping_recovery() {
    let (mut owners, clock) = fixture();
    let stale_success = owners.acquire_final([0; 32], 3, None).unwrap();
    let stale_failure = owners.acquire_final([0; 32], 3, None).unwrap();
    owners
        .acquire_final([0; 32], 3, None)
        .unwrap()
        .failure(clock.get());
    assert!(owners.acquire_final([0; 32], 3, None).is_err());
    expire(&owners, &clock, Key::Slot(3));

    let recovery = owners.acquire_final([0; 32], 3, None).unwrap();
    let other = owners.acquire_final([0; 32], 3, None).unwrap();
    assert!(!Rc::ptr_eq(&stale_success.observed, &recovery.observed));
    stale_failure.failure(crate::environment::now());
    assert!(owners.evidence([0; 32], 3).is_none());
    assert!(!owners.blocked([0; 32], 3));

    let renewed = crate::environment::now();
    recovery.failure(renewed);
    stale_success.success();
    drop(other);
    assert_eq!(owners.evidence([0; 32], 3), Some(renewed));
    assert!(owners.blocked([0; 32], 3));
    assert!(owners.acquire_final([0; 32], 3, None).is_err());
    assert!(owners.acquire_final([1; 32], 3, None).is_ok());
    assert!(owners.acquire_final([0; 32], 4, None).is_ok());
}

#[test]
fn indirect_expiry_detaches_a_live_old_probe() {
    for success in [false, true] {
        let (mut owners, clock) = fixture();
        owners
            .acquire_final([0; 32], 3, None)
            .unwrap()
            .failure(clock.get());
        // Hold a current-generation probe across observation expiry. Separate
        // clock control lets the test exercise this boundary without sleeping.
        clock.set(clock.get() + 2 * COOLDOWN);
        let old_probe = owners.acquire_final([0; 32], 3, None).unwrap();
        assert!(old_probe.permit.current());
        expire(&owners, &clock, Key::Slot(3));
        assert!(!owners.blocked([0; 32], 3));
        let recovered = owners.acquire_final([0; 32], 3, None).unwrap();
        let renewed = crate::environment::now();
        if success {
            recovered.failure(renewed);
            old_probe.success();
            assert_eq!(owners.evidence([0; 32], 3), Some(renewed));
            assert!(owners.blocked([0; 32], 3));
        } else {
            old_probe.failure(renewed);
            drop(recovered);
            assert!(owners.evidence([0; 32], 3).is_none());
            assert!(!owners.blocked([0; 32], 3));
        }
    }
}

#[test]
fn final_hop_report_retains_single_probe_and_requires_final_success() {
    let (mut owners, clock) = fixture();
    owners
        .acquire_final([0; 32], 3, Some("owner"))
        .unwrap()
        .failure(clock.get());
    assert!(owners.physical_evidence([0; 32], "owner").is_none());
    expire(&owners, &clock, Key::Slot(3));
    let mut relay = Attempt {
        route: route(false),
        owner: Some(owners.acquire_final([0; 32], 3, None).unwrap()),
    };
    assert!(owners.acquire_final([0; 32], 3, None).is_err());
    relay.owner_reachable();
    assert!(relay.owner.is_some());
    drop(relay);
    assert!(owners.blocked([0; 32], 3));
    clock.set(clock.get() + 2 * COOLDOWN);

    let mut final_attempt = Attempt {
        route: route(true),
        owner: Some(owners.acquire_final([0; 32], 3, Some("owner")).unwrap()),
    };
    final_attempt.owner_reachable();
    assert!(final_attempt.owner.is_none());
    assert!(!owners.blocked([0; 32], 3));
    assert!(
        owners.0[&([0; 32], Key::Slot(3))]
            .observed
            .borrow()
            .is_none()
    );
    assert!(owners.acquire_final([0; 32], 3, Some("owner")).is_ok());
}

#[test]
fn physical_failure_keeps_cross_slot_probe_and_generation_fencing() {
    let (mut owners, clock) = fixture();
    // Share the deterministic clock with the physical scope too.
    let read_clock = clock.clone();
    owners.0.insert(
        ([0; 32], Key::Physical("owner".into())),
        OwnerHealth {
            breaker: crate::breaker::CircuitBreaker::with_clock(COOLDOWN, move || read_clock.get()),
            observed: Rc::new(RefCell::new(None)),
        },
    );
    let stale = owners.acquire_final([0; 32], 3, Some("owner")).unwrap();
    owners
        .acquire_final([0; 32], 3, Some("owner"))
        .unwrap()
        .transport_failure(clock.get());
    assert!(owners.physical_evidence([0; 32], "owner").is_some());
    assert!(owners.acquire_final([0; 32], 4, Some("owner")).is_err());
    expire(&owners, &clock, Key::Slot(3));
    expire(&owners, &clock, Key::Physical("owner".into()));
    let probe = owners.acquire_final([0; 32], 3, Some("owner")).unwrap();
    assert!(owners.acquire_final([0; 32], 4, Some("owner")).is_err());
    probe.success();
    stale.transport_failure(crate::environment::now());
    assert!(owners.physical_evidence([0; 32], "owner").is_none());
    assert!(!owners.blocked([0; 32], 3));
    assert!(owners.acquire_final([0; 32], 4, Some("owner")).is_ok());
}
