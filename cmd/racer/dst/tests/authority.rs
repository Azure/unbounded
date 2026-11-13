//! Full fidelity control plane: the invariants catch what they claim to.
use racer_dst::{
    Choices,
    authority::{self, ApiFaults},
    choices::Source,
    determinism::run_twice,
    engine::SECONDS,
    scenario::FaultMix,
    scenarios,
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

/// A crash of the primary and seat holder is followed by a restart that
/// loses its old seat. The restarted Pod must reclaim a seat and then a
/// primary within [`authority::PRIMARY_BOUND`] of the last fault; an
/// established cluster's seat lottery must not back off for minutes.
#[test]
fn crash_restart_recovers_a_primary_within_the_bound() {
    let s = scenarios::find("authority-crash-restart").expect("catalog scenario");
    let out = run_twice(&s, &Source::Seed(151)).unwrap_or_else(|d| panic!("{d}"));
    assert_eq!(out.failure, None);
}
