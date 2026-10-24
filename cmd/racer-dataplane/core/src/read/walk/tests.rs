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
        handoff: 0,
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
fn owner_asks_rank_zero_to_acquire_then_probes_copy_only_then_gets_origin() {
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
            (0, Mode::Acquire, 3, RouteMode::FundedRoute),
            (1, Mode::CopyOnly, 2, RouteMode::SharedRoute)
        ]
    );
    // The Acquire to rank 0 carries exactly the exchange and one origin
    // credit over a normal route.
    assert_eq!((probes[0].1, probes[0].2), (2, 4));
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
fn owner_funding_covers_delegation_and_origin() {
    // Rank 0: the exchange and its origin attempt.
    assert_eq!(owner_funding(0), 2);
    assert_eq!(owner_funding_min(0), 2);
    // Without delegation: one copy-only probe per predecessor instead.
    assert_eq!(owner_funding_min(1), 3);
    assert_eq!(owner_funding_min(2), 4);
    // Rank r > 0: the exchange, an Acquire to rank 0 (two credits), r - 1
    // copy-only probes, and its own origin attempt.
    assert_eq!(owner_funding(1), 4);
    assert_eq!(owner_funding(2), 5);
}

#[test]
fn owner_delegates_only_with_credit_left_for_its_own_origin() {
    // Rank 1 holding two credits cannot fund the Acquire to rank 0 (two) and
    // keep one for its own origin attempt, so it probes copy-only as before.
    let mut walk = CandidateWalk::new(config(Some(1), 3, VisitOrder::canonical(3)));
    let mut b = budget(2, 16);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(probes.len(), 1);
    assert_eq!(probes[0].0.mode, Mode::CopyOnly);
    assert!(matches!(end, End::Origin { rank: 1, .. }));
    // With three it delegates.
    let mut walk = CandidateWalk::new(config(Some(1), 3, VisitOrder::canonical(3)));
    let mut b = budget(3, 16);
    let (probes, _) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(probes[0].0.mode, Mode::Acquire);
    // Rank 2 needs four: the Acquire, rank 1's probe, and its own origin.
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(3, 16);
    let (probes, _) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert!(probes.iter().all(|(p, ..)| p.mode == Mode::CopyOnly));
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(4, 16);
    let (probes, _) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(probes[0].0.mode, Mode::Acquire);
    assert_eq!(probes[1].0.mode, Mode::CopyOnly);
}

#[test]
fn non_owner_funds_delegation_from_slack_only() {
    // Default page budget, visit order [1, 2, 0]: rank 1 gets `owner_funding`
    // because rank 2's minimum (4) still fits; rank 2 then gets its minimum.
    let order = visit_order(ReplicaSelection::Spread, 3, |r| r != 0, |r| 3 - r as u64);
    let mut walk = CandidateWalk::new(config(None, 3, order));
    let mut b = budget(8, 16);
    let mut seen = Vec::new();
    loop {
        match walk.next(&mut b).unwrap() {
            Step::Probe(p) => {
                let request = walk.request_budget(&mut b);
                seen.push((p.canonical, request.remaining_attempts()));
                // Spend the child entirely, as a forwarded funded route does.
                let spent = request.remaining_attempts();
                drop(request.partition(spent, 0).unwrap());
                assert_eq!(
                    walk.complete(&mut b, Outcome::Probe(ProbeOutcome::CopyMiss))
                        .unwrap(),
                    None
                );
            }
            Step::End(_) => break,
        }
    }
    assert_eq!(order.ranks().collect::<Vec<_>>(), [1, 2, 0]);
    assert_eq!(
        seen[..2],
        [(1, owner_funding(1)), (2, owner_funding_min(2))]
    );
}

#[test]
fn delegated_version_miss_ends_without_origin() {
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(8, 16);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::VersionUnavailable)
    });
    assert_eq!(probes.len(), 1);
    assert_eq!(end, End::VersionUnavailable);
    // A copy-only predecessor's version miss is only evidence.
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(8, 16);
    let (probes, end) = drive(&mut walk, &mut b, |p, _| {
        Outcome::Probe(if p.canonical == 0 {
            ProbeOutcome::CopyMiss
        } else {
            ProbeOutcome::VersionUnavailable
        })
    });
    assert_eq!(probes.len(), 2);
    assert!(matches!(end, End::Origin { rank: 2, .. }));
    // Metadata and other non-page walks never delegate.
    let mut cfg = config(Some(1), 3, VisitOrder::canonical(3));
    cfg.page_operation = false;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(8, 16);
    let (probes, _) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(probes[0].0.mode, Mode::CopyOnly);
}

#[test]
fn owner_asks_handoff_targets_copy_only_after_predecessors() {
    let mut cfg = config(Some(1), 3, VisitOrder::canonical(3));
    cfg.handoff = 2;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(8, 32);
    let (probes, end) = drive(&mut walk, &mut b, |p, _| {
        Outcome::Probe(if p.canonical == 0 {
            ProbeOutcome::Overloaded
        } else {
            ProbeOutcome::CopyMiss
        })
    });
    assert_eq!(
        probes
            .iter()
            .map(|(p, ..)| (p.canonical, p.mode, p.remaining_opportunities))
            .collect::<Vec<_>>(),
        [
            (0, Mode::Acquire, 4),
            (3, Mode::CopyOnly, 3),
            (4, Mode::CopyOnly, 2)
        ]
    );
    // Handoff outcomes are not predecessor evidence.
    assert_eq!(
        end,
        End::Origin {
            rank: 1,
            evidence: vec![ProbeOutcome::Overloaded]
        }
    );
    // A handoff target's copy wins.
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 3;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(8, 32);
    let (probes, end) = drive(&mut walk, &mut b, |p, _| {
        if p.canonical == 4 {
            Outcome::Copy
        } else {
            Outcome::Probe(ProbeOutcome::CopyMiss)
        }
    });
    assert_eq!(probes.len(), 2);
    assert_eq!(end, End::Copy);
    // Non-page walks and non-owners ignore handoff targets.
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 3;
    cfg.page_operation = false;
    let mut walk = CandidateWalk::new(cfg);
    let (probes, _) = drive(&mut walk, &mut budget(8, 32), |_, _| unreachable!());
    assert!(probes.is_empty());
}

#[test]
fn handoff_probes_never_spend_the_origin_credit() {
    // Rank 0 with three credits asks two handoff targets (each exchange
    // spends one) and keeps one credit for origin.
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 3;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(3, 32);
    let mut probes = 0;
    let end = loop {
        match walk.next(&mut b).unwrap() {
            Step::Probe(_) => {
                probes += 1;
                drop(walk.request_budget(&mut b).partition(1, 0).unwrap());
                assert_eq!(
                    walk.complete(&mut b, Outcome::Probe(ProbeOutcome::CopyMiss))
                        .unwrap(),
                    None
                );
            }
            Step::End(end) => break end,
        }
    };
    assert_eq!(probes, 2);
    assert_eq!(b.remaining_attempts(), 1);
    assert!(matches!(end, End::Origin { rank: 0, .. }));
    // One credit: straight to origin.
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 3;
    let mut walk = CandidateWalk::new(cfg);
    let (probes, _) = drive(&mut walk, &mut budget(1, 32), |_, _| unreachable!());
    assert!(probes.is_empty());
    // No links left for a route: straight to origin.
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 1;
    let mut walk = CandidateWalk::new(cfg);
    let (probes, _) = drive(&mut walk, &mut budget(8, 0), |_, _| unreachable!());
    assert!(probes.is_empty());
}

#[test]
fn handoff_targets_skip_self_predecessors_and_unchanged_sets() {
    // Unchanged owner set (any order): nothing to ask.
    assert!(handoff_targets(&[1, 2, 3], &[2, 1, 3], 0).is_empty());
    // A join put 9 at rank 0: the old owners, in old rank order.
    assert_eq!(handoff_targets(&[1, 2, 3], &[9, 1, 2], 0), [1, 2, 3]);
    // At rank 1 (node 1), its predecessor 9 is probed anyway; itself skipped.
    assert_eq!(handoff_targets(&[1, 2, 3], &[9, 1, 2], 1), [2, 3]);
    // A removal: the previous owner 1 is gone from the current set.
    assert_eq!(handoff_targets(&[1, 2, 3], &[2, 3, 4], 2), [1]);
    // Fewer owners before.
    assert_eq!(handoff_targets(&[1], &[1, 2], 1), Vec::<i32>::new());
    assert_eq!(handoff_targets::<i32>(&[], &[1], 0), Vec::<i32>::new());
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
    // gets a child with links/remaining capped at four, and one attempt (two
    // for the Acquire to rank 0: the exchange and rank 0's origin credit).
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(4, 7);
    b.note_route_failure();
    let (probes, _) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(probes[0].0.mode, Mode::Acquire);
    assert_eq!((probes[0].1, probes[0].2), (2, 3));
    // The last predecessor may use the whole remaining pool.
    assert_eq!((probes[1].1, probes[1].2), (4, 7));
}

#[test]
fn non_owner_funds_out_of_order_owner_and_reports_version_unavailable() {
    // Visit rank 1 first: it is funded with `owner_funding(1)` attempts and 8 links.
    let order = visit_order(ReplicaSelection::Spread, 3, |r| r == 1, |_| 0);
    assert_eq!(order.get(0), Some(1));
    let mut walk = CandidateWalk::new(config(None, 3, order));
    let mut b = budget(32, 96);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::VersionUnavailable)
    });
    assert_eq!(probes.len(), 3);
    assert_eq!(probes[0].0.route, RouteMode::FundedRoute);
    assert_eq!((probes[0].1, probes[0].2), (owner_funding(1), 8));
    assert_eq!(owner_funding(1), 4);
    // With fewer credits it still funds the minimum, so reach is unchanged.
    let mut walk = CandidateWalk::new(config(None, 3, order));
    let mut b = budget(3, 96);
    let (probes, _) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::VersionUnavailable)
    });
    assert_eq!(probes[0].0.route, RouteMode::FundedRoute);
    assert_eq!((probes[0].1, probes[0].2), (owner_funding_min(1), 8));
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
    assert_eq!(stale_action(true, 1, Some(2)), Ok(StaleAction::Skip));
    assert_eq!(stale_action(false, 2, Some(2)), Ok(StaleAction::Skip));
    assert_eq!(stale_action(false, 3, Some(2)), Ok(StaleAction::Skip));
    assert_eq!(stale_action(false, 2, None), Err(Error::Unavailable));
    assert_eq!(stale_action(false, 2, Some(3)), Ok(StaleAction::Refresh));
}

/// Known issue 3: a requester ahead of its destination skips the stale owner
/// and reaches the next one instead of failing the read.
#[test]
fn stale_destination_moves_to_the_next_owner() {
    let mut walk = CandidateWalk::new(config(None, 3, VisitOrder::canonical(3)));
    let mut b = budget(32, 96);
    let (probes, end) = drive(&mut walk, &mut b, |p, _| {
        if p.canonical == 0 {
            Outcome::StaleDestination
        } else {
            Outcome::Copy
        }
    });
    assert_eq!(probes.len(), 2);
    assert_eq!(end, End::Copy);
    // A stale answer is not a route failure: the next probe keeps the normal
    // per-attempt route ceiling.
    assert_eq!(b.route_links(), 4);
}

#[test]
fn stale_destinations_everywhere_end_exhausted_or_at_origin() {
    let mut walk = CandidateWalk::new(config(None, 2, VisitOrder::canonical(2)));
    let mut b = budget(32, 96);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| Outcome::StaleDestination);
    assert_eq!(probes.len(), 2);
    assert_eq!(end, End::Exhausted { funding: false });
    // An owner keeps its origin authority, with transient evidence for the
    // stale predecessor.
    let mut walk = CandidateWalk::new(config(Some(1), 2, VisitOrder::canonical(2)));
    let (_, end) = drive(&mut walk, &mut b, |_, _| Outcome::StaleDestination);
    assert_eq!(
        end,
        End::Origin {
            rank: 1,
            evidence: vec![ProbeOutcome::Unreachable]
        }
    );
}

#[test]
fn remaining_walk_treats_a_stale_destination_as_unavailable() {
    let mut walk = RemainingWalk::new(0, 2);
    let mut b = budget(4, 8);
    walk.next_probe().unwrap();
    assert_eq!(walk.complete(&mut b, Outcome::StaleDestination), None);
    assert_eq!(walk.next_probe(), Err(RemainingEnd::Unavailable));
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
    assert_eq!(b.remaining_attempts(), 32 - owner_funding(2));
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
    assert_eq!(transport_outcome(Error::DeadlineExceeded), None);
    // Known issue 5: a local hop budget shortfall is evidence, not terminal.
    assert_eq!(
        transport_outcome(Error::HopBudgetExhausted),
        Some(ProbeOutcome::Unreachable)
    );
}

/// Known issue 5: once the links are spent, each later probe fails locally in
/// `plan_attempt` and is recorded as unreachable; the walk ends `Exhausted`
/// (recoverable) instead of abandoning with `HopBudgetExhausted`.
#[test]
fn spent_links_end_the_walk_exhausted() {
    let mut walk = CandidateWalk::new(config(None, 3, VisitOrder::canonical(3)));
    let mut b = budget(32, 16);
    b.note_route_failure();
    let now = Instant::now();
    let mut ends = Vec::new();
    let end = loop {
        match walk.next(&mut b).unwrap() {
            Step::End(end) => break end,
            Step::Probe(probe) => {
                let inputs = AttemptInputs {
                    now,
                    caller_deadline: now + Duration::from_secs(10),
                    remaining_opportunities: probe.remaining_opportunities,
                    route: probe.route,
                    mode: probe.mode,
                    attempt_timeout: Duration::from_secs(1),
                    failure_links: 8,
                };
                let outcome = match plan_attempt(walk.request_budget(&mut b), inputs) {
                    // The route was spent on an overloaded relay.
                    Ok(_) => ProbeOutcome::Overloaded,
                    Err(error) => {
                        ends.push(error);
                        transport_outcome(error).expect("recoverable")
                    }
                };
                walk.complete(&mut b, Outcome::Transport(outcome)).unwrap();
            }
        }
    };
    assert!(ends.contains(&Error::HopBudgetExhausted), "{ends:?}");
    assert_eq!(end, End::Exhausted { funding: false });
    assert_eq!(b.remaining_links(), 0);
}
