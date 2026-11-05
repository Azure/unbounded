//! Full fidelity control plane: the invariants catch what they claim to.
use racer_dst::{
    Choices,
    authority::{self, ApiFaults},
    engine::SECONDS,
    scenario::FaultMix,
};

/// With every API write rejected nobody can take the primary Lease, so the
/// bounded liveness invariant fires once the bound passed.
#[test]
fn no_primary_is_a_liveness_failure() {
    let s = authority::scenario(
        "no-primary",
        2,
        FaultMix::default(),
        ApiFaults {
            conflict_ppm: 1_000_000,
            watch_drop_ppm: 0,
        },
        authority::PRIMARY_BOUND + 20 * SECONDS,
    );
    let out = s.run(Choices::seeded(1));
    let failure = out.failure.expect("must fail");
    assert_eq!(failure.name, "eventual-primary", "{failure}");
    assert!(failure.at >= authority::PRIMARY_BOUND);
}

/// Two authorities elect one primary within the bound and the run is
/// deterministic (the run passes the bound, so no primary would fail it).
#[test]
fn two_authorities_elect_deterministically() {
    let s = authority::scenario(
        "two",
        2,
        FaultMix::default(),
        ApiFaults::default(),
        authority::PRIMARY_BOUND + 10 * SECONDS,
    );
    let a = s.run(Choices::seeded(7));
    let b = s.run(Choices::seeded(7));
    assert_eq!(a.failure, None);
    assert_eq!(a, b);
}
