//! Probe sequences for owners, non-owners, stale membership, version misses,
//! and budget exhaustion.

// The attribute below also marks this file as test-only for tests/purity.rs.
#[cfg(test)]
use super::*;
use crate::read::candidates::{ReplicaSelection, visit_order};

fn config(rank: Option<usize>, owners: usize, order: VisitOrder) -> WalkConfig {
    WalkConfig {
        rank,
        owners,
        order,
        page_operation: true,
        continuation: Continuation::default(),
    }
}

fn budget(attempts: u32, links: u8) -> AcquisitionBudget {
    AcquisitionBudget::new(Instant::now() + Duration::from_secs(60), attempts, links)
}

/// Drive a walk with scripted outcomes; return probes and the end.
fn drive(
    walk: &mut CandidateWalk,
    budget: &mut AcquisitionBudget,
    mut outcome: impl FnMut(&Probe, &AcquisitionBudget) -> Outcome,
) -> (Vec<(Probe, u32, u8)>, End) {
    let mut probes = Vec::new();
    loop {
        match walk.next(budget).unwrap() {
            Step::Probe(probe) => {
                let request = walk.request_budget(budget);
                probes.push((
                    probe,
                    request.remaining_attempts(),
                    request.remaining_links(),
                ));
                let result = outcome(&probe, request);
                if let Some(end) = walk.complete(budget, result).unwrap() {
                    return (probes, end);
                }
            }
            Step::End(end) => return (probes, end),
        }
    }
}

#[test]
fn owner_probes_predecessors_copy_only_then_gets_origin() {
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(32, 96);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(
        probes
            .iter()
            .map(|(p, ..)| (p.canonical, p.mode, p.remaining_opportunities, p.route))
            .collect::<Vec<_>>(),
        [
            (0, Mode::CopyOnly, 3, RouteMode::SharedRoute),
            (1, Mode::CopyOnly, 2, RouteMode::SharedRoute)
        ]
    );
    assert_eq!(
        end,
        End::Origin {
            rank: 2,
            evidence: vec![ProbeOutcome::CopyMiss, ProbeOutcome::CopyMiss]
        }
    );
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (32, 96));
}

#[test]
fn rank_zero_owner_gets_origin_without_probes() {
    let mut walk = CandidateWalk::new(config(Some(0), 3, VisitOrder::canonical(3)));
    let mut b = budget(1, 1);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| unreachable!());
    assert!(probes.is_empty());
    assert_eq!(
        end,
        End::Origin {
            rank: 0,
            evidence: vec![]
        }
    );
}

#[test]
fn owner_with_tight_links_partitions_predecessor_routes() {
    // Two predecessors, eight inherited links after a failure route: each probe
    // gets a one-attempt child with links/remaining capped at four.
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(4, 7);
    b.note_route_failure();
    let (probes, _) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!((probes[0].1, probes[0].2), (1, 3));
    // The last predecessor may use the whole remaining pool.
    assert_eq!((probes[1].1, probes[1].2), (4, 7));
}

#[test]
fn non_owner_funds_out_of_order_owner_and_reports_version_unavailable() {
    // Visit rank 1 first: it is funded with rank + 2 attempts and 8 links.
    let order = visit_order(ReplicaSelection::Spread, 3, |r| r == 1, |_| 0);
    assert_eq!(order.get(0), Some(1));
    let mut walk = CandidateWalk::new(config(None, 3, order));
    let mut b = budget(32, 96);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::VersionUnavailable)
    });
    assert_eq!(probes.len(), 3);
    assert_eq!(probes[0].0.route, RouteMode::FundedRoute);
    assert_eq!((probes[0].1, probes[0].2), (3, 8));
    assert_eq!(probes[1].0.route, RouteMode::SharedRoute);
    assert!(probes.iter().all(|(p, ..)| p.mode == Mode::Acquire));
    assert_eq!(end, End::VersionUnavailable);
}

#[test]
fn non_owner_transient_evidence_exhausts_and_notes_route_failure() {
    let mut walk = CandidateWalk::new(config(None, 2, VisitOrder::canonical(2)));
    let mut b = budget(32, 96);
    assert_eq!(b.route_links(), 4);
    let (probes, end) = drive(&mut walk, &mut b, |p, _| {
        if p.index == 0 {
            Outcome::Transport(ProbeOutcome::Unreachable)
        } else {
            Outcome::Probe(ProbeOutcome::VersionUnavailable)
        }
    });
    assert_eq!(probes.len(), 2);
    assert_eq!(end, End::Exhausted { funding: false });
    assert_eq!(b.route_links(), 8);
}

#[test]
fn last_attempt_page_probe_becomes_copy_only() {
    let mut walk = CandidateWalk::new(config(None, 1, VisitOrder::canonical(1)));
    let mut b = budget(1, 8);
    let Step::Probe(probe) = walk.next(&mut b).unwrap() else {
        panic!()
    };
    assert_eq!(probe.mode, Mode::CopyOnly);
    assert_eq!(
        walk.complete(&mut b, Outcome::Copy).unwrap(),
        Some(End::Copy)
    );
}

#[test]
fn unfundable_bounded_route_stops_with_funding_site() {
    let mut cfg = config(None, 3, VisitOrder::canonical(3));
    cfg.continuation.bounded_routes = true;
    let mut walk = CandidateWalk::new(cfg);
    // rank 0 needs 2 attempts + 8 links; rank 1 needs 3 attempts.
    let mut b = budget(2, 16);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(probes.len(), 1);
    assert_eq!(probes[0].0.route, RouteMode::FundedRoute);
    assert_eq!(end, End::Exhausted { funding: true });
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (2, 16));
}

#[test]
fn stale_membership_ends_and_returns_child_credits() {
    let order = visit_order(ReplicaSelection::Spread, 3, |r| r == 2, |_| 0);
    let mut walk = CandidateWalk::new(config(None, 3, order));
    let mut b = budget(32, 96);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| Outcome::StaleMembership);
    assert_eq!(probes.len(), 1);
    assert_eq!(end, End::StaleMembership);
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (32, 96));
    assert_eq!(
        refresh_allowed(true, 1, Some(2)),
        Err(Error::IncompatibleMembership)
    );
    assert_eq!(
        refresh_allowed(false, 2, Some(2)),
        Err(Error::IncompatibleMembership)
    );
    assert_eq!(refresh_allowed(false, 2, None), Err(Error::Unavailable));
    assert_eq!(refresh_allowed(false, 2, Some(3)), Ok(()));
}

#[test]
fn hedge_continuation_skips_primary_and_carries_its_outcome() {
    let mut cfg = config(None, 2, VisitOrder::canonical(2));
    cfg.continuation = Continuation {
        primary_consumed: true,
        primary_outcome: Some(ProbeOutcome::VersionUnavailable),
        bounded_routes: true,
    };
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(32, 96);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::VersionUnavailable)
    });
    assert_eq!(probes.len(), 1);
    assert_eq!(probes[0].0.canonical, 1);
    assert_eq!(end, End::VersionUnavailable);
    let mut cfg = config(None, 2, VisitOrder::canonical(2));
    cfg.continuation.primary_consumed = true;
    let mut walk = CandidateWalk::new(cfg);
    let (_, end) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::VersionUnavailable)
    });
    assert_eq!(end, End::Exhausted { funding: false });
}

#[test]
fn abandon_returns_child_credits() {
    let order = visit_order(ReplicaSelection::Spread, 3, |r| r == 2, |_| 0);
    let mut walk = CandidateWalk::new(config(None, 3, order));
    let mut b = budget(32, 96);
    assert!(matches!(walk.next(&mut b).unwrap(), Step::Probe(_)));
    assert_eq!(b.remaining_attempts(), 28);
    walk.abandon(&mut b).unwrap();
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (32, 96));
}

#[test]
fn remaining_walk_probes_later_owners_copy_only() {
    let mut walk = RemainingWalk::new(0, 3);
    let mut b = budget(8, 16);
    let p = walk.next_probe().unwrap();
    assert_eq!(
        (p.canonical, p.mode, p.remaining_opportunities),
        (1, Mode::CopyOnly, 2)
    );
    assert_eq!(
        walk.complete(&mut b, Outcome::Transport(ProbeOutcome::Overloaded)),
        None
    );
    assert_eq!(b.route_links(), 8);
    let p = walk.next_probe().unwrap();
    assert_eq!(p.canonical, 2);
    assert_eq!(
        walk.complete(&mut b, Outcome::Probe(ProbeOutcome::CopyMiss)),
        None
    );
    assert_eq!(walk.next_probe(), Err(RemainingEnd::Unavailable));
    let mut walk = RemainingWalk::new(1, 3);
    walk.next_probe().unwrap();
    walk.complete(&mut b, Outcome::Probe(ProbeOutcome::VersionUnavailable));
    assert_eq!(walk.next_probe(), Err(RemainingEnd::None));
    let mut walk = RemainingWalk::new(0, 2);
    walk.next_probe().unwrap();
    assert_eq!(
        walk.complete(&mut b, Outcome::Copy),
        Some(RemainingEnd::Copy)
    );
    assert_eq!(
        RemainingWalk::new(2, 3).next_probe(),
        Err(RemainingEnd::None)
    );
}

#[test]
fn attempt_plan_shares_deadline_and_reserves_credits() {
    let now = Instant::now();
    let deadline = now + Duration::from_secs(12);
    let mut b = AcquisitionBudget::new(deadline, 9, 16);
    let inputs = AttemptInputs {
        now,
        caller_deadline: deadline,
        remaining_opportunities: 3,
        route: RouteMode::SharedRoute,
        mode: Mode::Acquire,
        attempt_timeout: Duration::from_secs(60),
        failure_links: 8,
    };
    let plan = plan_attempt(&mut b, inputs).unwrap();
    assert_eq!(plan.links, 4);
    assert_eq!(plan.overall, deadline);
    assert_eq!(plan.share, now + Duration::from_secs(4));
    assert_eq!(plan.attempt_end, deadline);
    // 8 attempts after the sender's, ceil(8 / 3) = 3 forwarded.
    assert_eq!(plan.remote_attempts, 3);
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (5, 12));
    // Reserve min(share, (overall - share) / 2) = 4s for fallback.
    assert_eq!(plan.complete_by, deadline - Duration::from_secs(4));
    assert_eq!(plan.observation, Duration::from_secs(4));
    let hedge = plan_attempt(
        &mut b,
        AttemptInputs {
            route: RouteMode::DirectHedge,
            mode: Mode::CopyOnly,
            remaining_opportunities: 1,
            ..inputs
        },
    )
    .unwrap();
    assert_eq!(hedge.attempt_end, now + Duration::from_secs(4));
    assert_eq!(hedge.remote_attempts, 0);
    assert_eq!(hedge.complete_by, hedge.attempt_end);
    let funded = plan_attempt(
        &mut b,
        AttemptInputs {
            route: RouteMode::FundedRoute,
            ..inputs
        },
    )
    .unwrap();
    assert_eq!(funded.remote_attempts, 3);
    assert_eq!(b.remaining_attempts(), 0);
    let mut empty = AcquisitionBudget::new(deadline, 1, 0);
    assert_eq!(
        plan_attempt(&mut empty, inputs),
        Err(Error::HopBudgetExhausted)
    );
    let mut late = AcquisitionBudget::new(now, 1, 8);
    assert_eq!(
        plan_attempt(&mut late, inputs),
        Err(Error::DeadlineExceeded)
    );
}

#[test]
fn hedge_rules() {
    let facts = HedgeFacts {
        page_operation: true,
        is_candidate: false,
        owners: 3,
        first: 0,
        second: 1,
        distinct: true,
        remaining_attempts: 6,
        remaining_links: 10,
    };
    assert!(hedge_eligible(facts));
    assert!(!hedge_eligible(HedgeFacts {
        remaining_attempts: 5,
        ..facts
    }));
    assert!(!hedge_eligible(HedgeFacts {
        remaining_links: 9,
        ..facts
    }));
    assert!(!hedge_eligible(HedgeFacts { first: 1, ..facts }));
    assert!(!hedge_eligible(HedgeFacts {
        is_candidate: true,
        ..facts
    }));
    assert!(!hedge_eligible(HedgeFacts { owners: 1, ..facts }));
    assert!(!hedge_eligible(HedgeFacts {
        distinct: false,
        ..facts
    }));
    assert!(!hedge_eligible(HedgeFacts {
        page_operation: false,
        ..facts
    }));
    let mut b = budget(6, 10);
    let (secondary, primary) = hedge_budgets(&mut b).unwrap();
    assert_eq!(
        (secondary.remaining_attempts(), secondary.remaining_links()),
        (1, 1)
    );
    assert_eq!(
        (primary.remaining_attempts(), primary.remaining_links()),
        (2, 1)
    );
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (3, 8));
    assert!(hedge_fallback(Error::CorruptRecord));
    assert!(!hedge_fallback(Error::Unauthorized));
    assert!(!hedge_fallback(Error::Cancelled));
    assert_eq!(
        transport_outcome(Error::Os(1)),
        Some(ProbeOutcome::Unreachable)
    );
    assert_eq!(
        transport_outcome(Error::Overloaded),
        Some(ProbeOutcome::Overloaded)
    );
    assert_eq!(transport_outcome(Error::Unauthorized), None);
}
