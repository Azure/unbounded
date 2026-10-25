//! Probe sequences for owners, non-owners, stale membership, version misses,
//! and budget exhaustion.

// The attribute below also marks this file as test-only for tests/purity.rs.
#[cfg(test)]
use super::*;
use crate::read::candidates::{ReplicaSelection, visit_order};
use crate::read::fanout::{
    HELPER_ATTEMPTS, HELPER_FLOOR_ATTEMPTS, HELPER_FLOOR_LINKS, HELPER_LINKS, HELPER_MIN_ATTEMPTS,
};

fn config(rank: Option<usize>, owners: usize, order: VisitOrder) -> WalkConfig {
    WalkConfig {
        rank,
        owners,
        order,
        page_operation: true,
        continuation: Continuation::default(),
        handoff: 0,
        previous: 0,
        helper: false,
        lineage: [0; MAX_HANDOFF],
        granted: 0,
        rank0: Rank0State::Unknown,
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
    // credit over a normal route (remote credits stay spent, so no more).
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
    while let Step::Probe(p) = walk.next(&mut b).unwrap() {
        let request = walk.request_budget(&mut b);
        seen.push((p.canonical, request.remaining_attempts()));
        // Spend the child entirely, as a forwarded funded route does.
        let spent = request.remaining_attempts();
        let _ = request.partition(spent, 0).unwrap();
        assert_eq!(
            walk.complete(&mut b, Outcome::Probe(ProbeOutcome::CopyMiss))
                .unwrap(),
            None
        );
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
    // A stale handoff target is only a miss.
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 1;
    let mut walk = CandidateWalk::new(cfg);
    let (probes, end) = drive(&mut walk, &mut budget(8, 32), |_, _| {
        Outcome::StaleMembership
    });
    assert_eq!(probes.len(), 1);
    assert!(matches!(end, End::Origin { rank: 0, .. }));
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
                let _ = walk.request_budget(&mut b).partition(1, 0).unwrap();
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
    // Two predecessors, seven inherited links after a failure route. The
    // Acquire to rank 0 (two attempts: the exchange and rank 0's origin
    // credit) comes first: it gets a full failure route and leaves one link
    // for rank 1's copy-only probe, which matters only if rank 0 fails
    // (Known issue 13: an equal split left it too few links to reach rank 0).
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(4, 7);
    b.note_route_failure();
    let (probes, _) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(probes[0].0.mode, Mode::Acquire);
    assert_eq!((probes[0].1, probes[0].2), (2, 6));
    // Without a delegation (too few attempts), the copy-only probe of rank
    // 0 comes first the same way: it decides whether this owner leaves the
    // fetch to rank 0 (a route failure gives origin authority; an equal
    // split left rank 2 two links and made it fetch pages rank 0 was
    // fetching, Known issue 40).
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(3, 7);
    b.note_route_failure();
    let (probes, _) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(probes[0].0.mode, Mode::CopyOnly);
    assert_eq!((probes[0].1, probes[0].2), (1, 6));
    // The last predecessor may use the whole remaining pool.
    assert_eq!((probes[1].1, probes[1].2), (3, 7));
    // Without a failure route the first route is capped at four links, and
    // the later predecessor keeps the rest.
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(3, 4);
    let (probes, _) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!((probes[0].0.canonical, probes[0].2), (0, 3));
    assert_eq!(probes[1].0.canonical, 1);
    assert!(probes[1].2 >= 1);
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

/// Merge of round 3 route and origin: `pending_handoff` tells callers a
/// stale answer comes from a handoff target (a miss, no refresh), and a
/// `StaleDestination` from one is also only a miss, never walk evidence.
#[test]
fn pending_handoff_marks_handoff_probes_and_stale_destination_is_a_miss() {
    let mut cfg = config(Some(1), 3, VisitOrder::canonical(3));
    cfg.handoff = 1;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(8, 32);
    assert!(!walk.pending_handoff());
    let mut seen = Vec::new();
    let end = loop {
        match walk.next(&mut b).unwrap() {
            Step::Probe(p) => {
                seen.push((p.canonical, walk.pending_handoff()));
                let outcome = if walk.pending_handoff() {
                    Outcome::StaleDestination
                } else {
                    Outcome::Probe(ProbeOutcome::CopyMiss)
                };
                if let Some(end) = walk.complete(&mut b, outcome).unwrap() {
                    break end;
                }
                assert!(!walk.pending_handoff());
            }
            Step::End(end) => break end,
        }
    };
    assert_eq!(seen, [(0, false), (3, true)]);
    assert_eq!(
        end,
        End::Origin {
            rank: 1,
            evidence: vec![ProbeOutcome::CopyMiss]
        }
    );
}

/// Known issue 13: handoff probes are opportunistic, so they must not
/// reserve credits that make an owner skip the delegation to rank 0 and fetch
/// the page itself.
#[test]
fn handoff_targets_do_not_block_delegation() {
    for (rank, attempts) in [(1, 3), (2, 4)] {
        for handoff in 0..=MAX_HANDOFF {
            let mut cfg = config(Some(rank), 3, VisitOrder::canonical(3));
            cfg.handoff = handoff;
            let mut walk = CandidateWalk::new(cfg);
            let mut b = budget(attempts, 16);
            let Step::Probe(probe) = walk.next(&mut b).unwrap() else {
                panic!("expected a probe");
            };
            assert_eq!(
                (probe.canonical, probe.mode),
                (0, Mode::Acquire),
                "rank {rank} handoff {handoff}"
            );
            // The owner keeps one credit per later predecessor and one for
            // its own origin attempt.
            assert_eq!(b.remaining_attempts(), rank as u32);
        }
    }
}

/// Known issue 13: rank 0 reached by an owner's delegation asks previous
/// owners only with credits the delegation carries. An owner that sees an
/// ownership change forwards its spare credits (up to [`MAX_HANDOFF`]); an
/// owner that sees none forwards exactly the exchange and the origin credit.
#[test]
fn delegation_forwards_spare_credits_for_rank_zero_handoff() {
    for (handoff, attempts, child) in [(0, 8, 2), (1, 3, 2), (1, 4, 3), (2, 6, 5), (3, 16, 5)] {
        let mut cfg = config(Some(1), 3, VisitOrder::canonical(3));
        cfg.handoff = handoff;
        let mut walk = CandidateWalk::new(cfg);
        let mut b = budget(attempts, 16);
        let Step::Probe(probe) = walk.next(&mut b).unwrap() else {
            panic!("expected a probe");
        };
        assert_eq!(probe.mode, Mode::Acquire);
        assert_eq!(probe.route, RouteMode::FundedRoute);
        assert_eq!(
            walk.request_budget(&mut b).remaining_attempts(),
            child,
            "handoff {handoff} attempts {attempts}"
        );
        // The owner keeps its origin credit, never less.
        assert!(b.remaining_attempts() >= 1);
        assert_eq!(b.remaining_attempts() + child, attempts);
    }
}

/// Known issue 13: the delegation route comes first. Rank 2 with five
/// inherited links (a failure route) gives the Acquire four and keeps one
/// for rank 1's copy-only probe instead of splitting them evenly (two each,
/// too few to reach rank 0 in most topologies).
#[test]
fn delegation_route_takes_priority_over_later_predecessors() {
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let mut b = budget(6, 5);
    b.note_route_failure();
    let Step::Probe(probe) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    assert_eq!(probe.mode, Mode::Acquire);
    assert_eq!(walk.request_budget(&mut b).remaining_links(), 4);
    assert_eq!(b.remaining_links(), 1);
}

/// Handoff targets are asked in previous rank order, each with whatever
/// links the earlier ones left, so the first (most likely holder) gets a
/// usable route rather than an even share of a small pool.
#[test]
fn handoff_probes_take_remaining_links_in_order() {
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 3;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(8, 3);
    let Step::Probe(probe) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    assert_eq!(probe.canonical, 3);
    assert_eq!(walk.request_budget(&mut b).remaining_links(), 3);
}

#[test]
fn same_rank_previous_marks_unchanged_predecessors() {
    // Unchanged owners: every predecessor of rank 2.
    assert_eq!(same_rank_previous(&[1, 2, 3], &[1, 2, 3], 2), 0b11);
    // Rank 0 has no predecessors.
    assert_eq!(same_rank_previous(&[1, 2, 3], &[1, 2, 3], 0), 0);
    // A join at rank 0 shifts everyone: nothing held the same rank.
    assert_eq!(same_rank_previous(&[1, 2, 3], &[9, 1, 2], 2), 0);
    // A removal at rank 1 keeps rank 0.
    assert_eq!(same_rank_previous(&[1, 2, 3], &[1, 3, 4], 2), 0b01);
    // Fewer owners before.
    assert_eq!(same_rank_previous(&[1], &[1, 2, 3], 2), 0b01);
    assert_eq!(same_rank_previous::<i32>(&[], &[1, 2], 1), 0);
}

/// Known issue 12: predecessors that held the same rank under the previous
/// membership are asked under it, so one that has not adopted the current
/// version yet serves instead of answering `StaleMembership`.
#[test]
fn predecessors_with_unchanged_rank_are_asked_under_the_previous_membership() {
    let mut cfg = config(Some(2), 3, VisitOrder::canonical(3));
    cfg.previous = 0b01;
    cfg.handoff = 1;
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
            .map(|(p, ..)| (p.canonical, p.mode, p.previous))
            .collect::<Vec<_>>(),
        [
            (0, Mode::Acquire, true),
            (1, Mode::CopyOnly, false),
            // Handoff targets are never asked under the previous membership.
            (3, Mode::CopyOnly, false)
        ]
    );
    assert!(matches!(end, End::Origin { rank: 2, .. }));
    // A copy from the predecessor asked under the previous membership wins.
    let mut cfg = config(Some(1), 3, VisitOrder::canonical(3));
    cfg.previous = 0b01;
    let mut walk = CandidateWalk::new(cfg);
    let (probes, end) = drive(&mut walk, &mut budget(8, 32), |_, _| Outcome::Copy);
    assert!(probes[0].0.previous);
    assert_eq!(end, End::Copy);
    // Non-owners and non-page walks ignore the bits.
    let mut cfg = config(None, 3, VisitOrder::canonical(3));
    cfg.previous = 0b111;
    let mut walk = CandidateWalk::new(cfg);
    let (probes, _) = drive(&mut walk, &mut budget(8, 32), |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert!(probes.iter().all(|(p, ..)| !p.previous));
    let mut cfg = config(Some(1), 3, VisitOrder::canonical(3));
    cfg.previous = 0b01;
    cfg.page_operation = false;
    let mut walk = CandidateWalk::new(cfg);
    let (probes, _) = drive(&mut walk, &mut budget(8, 32), |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert!(!probes[0].0.previous);
}

/// A `StaleMembership` (or `StaleDestination`) answer to a probe sent under
/// the previous membership means the destination no longer resolves it: the
/// walk records transient evidence and moves on; it never refreshes.
#[test]
fn stale_answer_under_the_previous_membership_is_transient_evidence() {
    for stale in [Outcome::StaleMembership, Outcome::StaleDestination] {
        let mut cfg = config(Some(2), 3, VisitOrder::canonical(3));
        cfg.previous = 0b11;
        let mut walk = CandidateWalk::new(cfg);
        let mut b = budget(8, 32);
        let mut seen = Vec::new();
        let (probes, end) = drive(&mut walk, &mut b, |p, _| {
            seen.push(p.previous);
            if p.canonical == 0 {
                stale
            } else {
                Outcome::Probe(ProbeOutcome::CopyMiss)
            }
        });
        assert_eq!(probes.len(), 2);
        assert_eq!(seen, [true, true]);
        assert_eq!(
            end,
            End::Origin {
                rank: 2,
                evidence: vec![ProbeOutcome::Unreachable, ProbeOutcome::CopyMiss]
            }
        );
        assert_eq!((b.remaining_attempts(), b.remaining_links()), (8, 32));
    }
}
fn helper_config(order: VisitOrder) -> WalkConfig {
    WalkConfig {
        helper: true,
        ..config(None, 3, order)
    }
}

fn shapes(probes: &[(Probe, u32, u8)]) -> Vec<(usize, Mode, RouteMode)> {
    probes
        .iter()
        .map(|(p, ..)| (p.canonical, p.mode, p.route))
        .collect()
}

#[test]
fn overloaded_owner_sends_the_reader_to_its_helper_once() {
    let mut walk = CandidateWalk::new(helper_config(VisitOrder::canonical(3)));
    let mut b = budget(8, 16);
    let (probes, end) = drive(&mut walk, &mut b, |p, _| {
        if p.canonical == 3 {
            Outcome::Copy
        } else {
            Outcome::Shed
        }
    });
    assert_eq!(end, End::Copy);
    assert_eq!(
        shapes(&probes),
        vec![
            (0, Mode::Acquire, RouteMode::SharedRoute),
            (3, Mode::Acquire, RouteMode::FundedRoute),
        ]
    );
    // The helper probe is funded from what the owner probe left, capped.
    let (_, attempts, links) = probes[1];
    assert!(
        (HELPER_FLOOR_ATTEMPTS..=HELPER_ATTEMPTS).contains(&attempts),
        "{attempts}"
    );
    assert!(
        (HELPER_FLOOR_LINKS..=HELPER_LINKS).contains(&links),
        "{links}"
    );
}

#[test]
fn helper_miss_falls_back_to_remaining_owners_and_never_repeats() {
    let mut walk = CandidateWalk::new(helper_config(VisitOrder::canonical(3)));
    let mut b = budget(32, 64);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| Outcome::Shed);
    let canon: Vec<_> = probes.iter().map(|(p, ..)| p.canonical).collect();
    assert_eq!(canon, vec![0, 3, 1, 2]);
    assert!(matches!(end, End::Exhausted { .. }), "{end:?}");
}

#[test]
fn only_a_destination_shed_asks_the_helper() {
    // Relay overload and timeouts say the route is congested, not the page.
    for outcome in [
        Outcome::Probe(ProbeOutcome::Overloaded),
        Outcome::Transport(ProbeOutcome::Overloaded),
        Outcome::Transport(ProbeOutcome::Unreachable),
        Outcome::Probe(ProbeOutcome::CopyMiss),
    ] {
        let mut walk = CandidateWalk::new(helper_config(VisitOrder::canonical(3)));
        let mut b = budget(32, 64);
        let (probes, _) = drive(&mut walk, &mut b, |_, _| outcome);
        assert!(probes.iter().all(|(p, ..)| p.canonical < 3), "{outcome:?}");
    }
}

#[test]
fn owner_probes_are_planned_exactly_as_without_a_helper() {
    // Without overflow the helper changes nothing, for every visit order.
    for first in 0..3 {
        let order = visit_order(ReplicaSelection::Spread, 3, |r| r == first, |_| 0);
        let run = |helper: bool| {
            let mut walk = CandidateWalk::new(WalkConfig {
                helper,
                ..config(None, 3, order)
            });
            let mut b = budget(8, 16);
            let (probes, end) = drive(&mut walk, &mut b, |_, _| {
                Outcome::Probe(ProbeOutcome::CopyMiss)
            });
            (probes, end, b.remaining_attempts(), b.remaining_links())
        };
        assert_eq!(run(true), run(false), "first {first}");
    }
}

#[test]
fn helper_never_chains_and_owners_never_use_helpers() {
    // A helper inherits at most HELPER_ATTEMPTS - 1: too few to be eligible.
    const { assert!(HELPER_ATTEMPTS < HELPER_MIN_ATTEMPTS) };
    let mut walk = CandidateWalk::new(helper_config(VisitOrder::canonical(3)));
    let mut b = budget(HELPER_ATTEMPTS, 8);
    let (probes, _) = drive(&mut walk, &mut b, |_, _| Outcome::Shed);
    assert!(probes.iter().all(|(p, ..)| p.canonical < 3));
    // Owners ignore the helper flag.
    let mut walk = CandidateWalk::new(WalkConfig {
        helper: true,
        ..config(Some(2), 3, VisitOrder::canonical(3))
    });
    let mut b = budget(8, 16);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| Outcome::Shed);
    assert!(probes.iter().all(|(p, ..)| p.canonical < 2));
    assert!(matches!(end, End::Origin { rank: 2, .. }));
}

#[test]
fn helper_is_skipped_when_too_little_is_left() {
    // Rank 2 first is funded with `owner_funding(2)` = 5 of 6 and spends it
    // all (forwarded credits never come back), leaving 1 < the floor.
    let order = visit_order(ReplicaSelection::Spread, 3, |r| r == 2, |_| 0);
    let mut walk = CandidateWalk::new(helper_config(order));
    let mut b = budget(HELPER_MIN_ATTEMPTS + 1, 16);
    let mut canon = Vec::new();
    while let Step::Probe(probe) = walk.next(&mut b).unwrap() {
        canon.push(probe.canonical);
        // The exchange forwards every credit of its budget.
        let _ = walk.request_budget(&mut b).transfer();
        let outcome = Outcome::Shed;
        if walk.complete(&mut b, outcome).unwrap().is_some() {
            break;
        }
    }
    assert_eq!(canon[0], 2);
    assert!(canon.iter().all(|c| *c < 3), "{canon:?}");
}

#[test]
fn small_budget_non_owner_visits_rank_zero_first() {
    // A helper's inherited budget cannot fund rank 2 out of order at all; it
    // asks rank 0 (the origin authority) first instead of ending the walk on
    // funding before any probe.
    let order = visit_order(ReplicaSelection::Spread, 3, |r| r == 2, |_| 0);
    assert_eq!(order.get(0), Some(2));
    let mut walk = CandidateWalk::new(config(None, 3, order));
    let mut b = budget(HELPER_ATTEMPTS - 1, 7);
    let (probes, end) = drive(&mut walk, &mut b, |p, _| {
        if p.canonical == 0 {
            Outcome::Copy
        } else {
            Outcome::Probe(ProbeOutcome::CopyMiss)
        }
    });
    assert_eq!(
        shapes(&probes)[0],
        (0, Mode::Acquire, RouteMode::SharedRoute)
    );
    assert_eq!(end, End::Copy);
}

#[test]
fn hedged_primary_overload_does_not_ask_the_helper() {
    // A hedge reports only `ProbeOutcome::Overloaded`, without who signed it,
    // so it never counts as a destination shed.
    let mut cfg = helper_config(VisitOrder::canonical(3));
    cfg.continuation.primary_consumed = true;
    cfg.continuation.primary_outcome = Some(ProbeOutcome::Overloaded);
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(8, 16);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| Outcome::Copy);
    assert_eq!(probes[0].0.canonical, 1);
    assert_eq!(end, End::Copy);
}

#[test]
fn destination_shed_is_evidence_but_not_a_route_failure() {
    for (outcome, failed) in [
        (Outcome::Shed, false),
        (Outcome::Transport(ProbeOutcome::Overloaded), true),
    ] {
        let mut walk = CandidateWalk::new(config(None, 3, VisitOrder::canonical(3)));
        let mut b = budget(32, 64);
        let Step::Probe(_) = walk.next(&mut b).unwrap() else {
            panic!("probe")
        };
        assert_eq!(walk.complete(&mut b, outcome).unwrap(), None);
        assert_eq!(walk.evidence(), &[ProbeOutcome::Overloaded]);
        assert_eq!(b.route_links(), if failed { 8 } else { 4 }, "{outcome:?}");
    }
}

/// Known issue 31: an owner whose probe of rank 0 (a delegated `Acquire`, or
/// copy-only when it cannot fund one) is sent but unanswered within the
/// attempt does not fetch origin: rank 0 is most likely still fetching, and
/// this owner's request would queue behind it at the same origin.
#[test]
fn owner_leaves_the_fetch_to_rank_zero_after_a_timeout() {
    // Delegated Acquire (rank 1, enough credits).
    let mut walk = CandidateWalk::new(config(Some(1), 3, VisitOrder::canonical(3)));
    let mut b = budget(8, 32);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| Outcome::TimedOut);
    assert_eq!(probes.len(), 1);
    assert_eq!(probes[0].0.mode, Mode::Acquire);
    assert_eq!(end, End::Exhausted { funding: false });
    assert!(walk.rank0_unanswered());
    // Copy-only probe of rank 0 (too few credits to delegate): it joined
    // rank 0's running acquisition, so the same holds.
    let mut walk = CandidateWalk::new(config(Some(1), 3, VisitOrder::canonical(3)));
    let (probes, end) = drive(&mut walk, &mut budget(2, 16), |_, _| Outcome::TimedOut);
    assert_eq!(probes[0].0.mode, Mode::CopyOnly);
    assert_eq!(end, End::Exhausted { funding: false });
    // Rank 2: rank 0 timed out, rank 1 still asked copy-only; its copy wins.
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let (probes, end) = drive(&mut walk, &mut budget(8, 32), |p, _| {
        if p.canonical == 0 {
            Outcome::TimedOut
        } else {
            Outcome::Copy
        }
    });
    assert_eq!(probes.len(), 2);
    assert_eq!(end, End::Copy);
    // Rank 2: rank 0 timed out and rank 1 missed: still no origin.
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let (_, end) = drive(&mut walk, &mut budget(8, 32), |p, _| {
        if p.canonical == 0 {
            Outcome::TimedOut
        } else {
            Outcome::Probe(ProbeOutcome::CopyMiss)
        }
    });
    assert_eq!(end, End::Exhausted { funding: false });
}

/// The rule covers only an owner's probe of rank 0. A transport failure or a
/// miss from rank 0 still gives an owner origin authority, a timeout of a
/// later predecessor or a handoff target does not block it, and for a
/// non-owner a timeout is the same evidence as `Transport(Unreachable)`.
#[test]
fn only_an_owner_probe_of_rank_zero_timing_out_withholds_origin() {
    for outcome in [
        Outcome::Transport(ProbeOutcome::Unreachable),
        Outcome::Probe(ProbeOutcome::Overloaded),
        Outcome::Probe(ProbeOutcome::CopyMiss),
        Outcome::Shed,
    ] {
        let mut walk = CandidateWalk::new(config(Some(1), 3, VisitOrder::canonical(3)));
        let (_, end) = drive(&mut walk, &mut budget(8, 32), |_, _| outcome);
        assert!(matches!(end, End::Origin { rank: 1, .. }), "{outcome:?}");
        assert!(!walk.rank0_unanswered());
    }
    // Rank 2: rank 1 timing out (copy-only) does not withhold origin.
    let mut walk = CandidateWalk::new(config(Some(2), 3, VisitOrder::canonical(3)));
    let (_, end) = drive(&mut walk, &mut budget(8, 32), |p, _| {
        if p.canonical == 0 {
            Outcome::Probe(ProbeOutcome::CopyMiss)
        } else {
            Outcome::TimedOut
        }
    });
    assert_eq!(
        end,
        End::Origin {
            rank: 2,
            evidence: vec![ProbeOutcome::CopyMiss, ProbeOutcome::Unreachable]
        }
    );
    // Non-page walks (metadata) keep the timeout as transient evidence.
    let mut cfg = config(Some(1), 3, VisitOrder::canonical(3));
    cfg.page_operation = false;
    let mut walk = CandidateWalk::new(cfg);
    let (_, end) = drive(&mut walk, &mut budget(8, 32), |_, _| Outcome::TimedOut);
    assert!(matches!(end, End::Origin { rank: 1, .. }));
    // A handoff target timing out is only a miss.
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 1;
    let mut walk = CandidateWalk::new(cfg);
    let (probes, end) = drive(&mut walk, &mut budget(8, 32), |_, _| Outcome::TimedOut);
    assert_eq!(probes.len(), 1);
    assert!(matches!(end, End::Origin { rank: 0, .. }));
    // Non-owner: identical to an unreachable owner, including the route
    // failure it notes.
    let run = |outcome: Outcome| {
        let mut walk = CandidateWalk::new(config(None, 2, VisitOrder::canonical(2)));
        let mut b = budget(32, 96);
        let (probes, end) = drive(&mut walk, &mut b, |_, _| outcome);
        (probes.len(), end, b.route_links(), walk.rank0_unanswered())
    };
    assert_eq!(
        run(Outcome::TimedOut),
        run(Outcome::Transport(ProbeOutcome::Unreachable))
    );
    // The remaining walk treats a timeout as unavailable.
    let mut remaining = RemainingWalk::new(0, 2);
    let mut b = budget(8, 32);
    remaining.next_probe().unwrap();
    assert_eq!(remaining.complete(&mut b, Outcome::TimedOut), None);
    assert_eq!(remaining.next_probe(), Err(RemainingEnd::Unavailable));
}

/// Known issue 39: the rank 0 watch bounds the timeout rule. Once rank 0
/// stayed silent for a request timeout (`Overdue`), a timeout gives
/// authority; `Fetching` keeps the rule and changes nothing else (a route
/// failure, `Overloaded`, shed or miss from rank 0 still gives authority,
/// Known issue 40). Probes are planned the same in every state.
#[test]
fn rank_zero_watch_bounds_the_timeout_rule() {
    let with = |rank: usize, state: Rank0State| WalkConfig {
        rank0: state,
        ..config(Some(rank), 3, VisitOrder::canonical(3))
    };
    let mut walk = CandidateWalk::new(with(1, Rank0State::Fetching));
    let (_, end) = drive(&mut walk, &mut budget(8, 32), |_, _| Outcome::TimedOut);
    assert_eq!(end, End::Exhausted { funding: false });
    assert!(walk.rank0_unanswered());
    for outcome in [
        Outcome::Transport(ProbeOutcome::Unreachable),
        Outcome::Transport(ProbeOutcome::Overloaded),
        Outcome::Probe(ProbeOutcome::Overloaded),
        Outcome::Shed,
        Outcome::Probe(ProbeOutcome::CopyMiss),
        Outcome::Probe(ProbeOutcome::Unreachable),
    ] {
        let mut walk = CandidateWalk::new(with(1, Rank0State::Fetching));
        let (_, end) = drive(&mut walk, &mut budget(8, 32), |_, _| outcome);
        assert!(matches!(end, End::Origin { rank: 1, .. }), "{outcome:?}");
    }
    // Overdue: a timeout is a transport failure again, at rank 1 and 2.
    let mut walk = CandidateWalk::new(with(1, Rank0State::Overdue));
    let (_, end) = drive(&mut walk, &mut budget(8, 32), |_, _| Outcome::TimedOut);
    assert!(matches!(end, End::Origin { rank: 1, .. }));
    assert!(!walk.rank0_unanswered());
    let mut walk = CandidateWalk::new(with(2, Rank0State::Overdue));
    let (_, end) = drive(&mut walk, &mut budget(8, 32), |p, _| {
        if p.canonical == 0 {
            Outcome::TimedOut
        } else {
            Outcome::Probe(ProbeOutcome::CopyMiss)
        }
    });
    assert!(matches!(end, End::Origin { rank: 2, .. }));
    // Non-owners and rank 0 ignore the state; probes and budgets match.
    for state in [Rank0State::Fetching, Rank0State::Overdue] {
        let run = |cfg: WalkConfig| {
            let mut walk = CandidateWalk::new(cfg);
            let mut b = budget(8, 32);
            let (probes, end) = drive(&mut walk, &mut b, |_, _| Outcome::TimedOut);
            (
                shapes(&probes),
                end,
                b.remaining_attempts(),
                b.remaining_links(),
            )
        };
        for rank in [None, Some(0)] {
            let base = config(rank, 3, VisitOrder::canonical(3));
            assert_eq!(
                run(WalkConfig {
                    rank0: state,
                    ..base
                }),
                run(base)
            );
        }
        let probes = |cfg: WalkConfig| {
            let mut walk = CandidateWalk::new(cfg);
            let mut b = budget(8, 32);
            let (probes, _) = drive(&mut walk, &mut b, |_, _| Outcome::TimedOut);
            (shapes(&probes), b.remaining_attempts(), b.remaining_links())
        };
        assert_eq!(probes(with(1, state)), probes(with(1, Rank0State::Unknown)));
    }
}

/// Known issue 25: later owners that arrived before an owner are its lineage
/// targets; equal or unknown arrivals never are.
#[test]
fn lineage_targets_are_later_owners_that_arrived_earlier() {
    let owners = ["a", "b", "c"];
    // `a` joined in generation 5 above `b` (0) and `c` (3).
    let since = [Some(5), Some(0), Some(3)];
    assert_eq!(lineage_targets(&owners, &since, 0), vec!["b", "c"]);
    // `b` is older than `c`: `c` never ranked above it.
    assert!(lineage_targets(&owners, &since, 1).is_empty());
    assert!(lineage_targets(&owners, &since, 2).is_empty());
    assert_eq!(lineage_counts(&since), [2, 0, 0]);
    // Members that arrived together (a fresh chain) pay nothing.
    let together = [Some(0); 3];
    assert!(lineage_targets(&owners, &together, 0).is_empty());
    assert_eq!(lineage_counts(&together), [0; 3]);
    // Unknown arrivals (not ranked in this chain) never qualify.
    assert!(lineage_targets(&owners, &[None, Some(0), Some(0)], 0).is_empty());
    assert_eq!(
        lineage_targets(&owners, &[Some(2), None, Some(1)], 0),
        vec!["c"]
    );
    assert_eq!(lineage_counts(&[Some(2), None, Some(1)]), [1, 0, 0]);
    // Out of range or short rankings.
    assert!(lineage_targets(&owners, &since, 3).is_empty());
    assert_eq!(lineage_targets(&owners[..2], &since[..2], 0), vec!["b"]);
    assert_eq!(lineage_counts(&since[..1]), [0; 3]);
    assert_eq!(lineage_counts(&[]), [0; 3]);
}

#[test]
fn merge_handoff_keeps_lineage_targets_first_without_duplicates() {
    assert_eq!(
        merge_handoff(vec!["x"], vec!["b", "x", "c"]),
        vec!["b", "x", "c"]
    );
    assert_eq!(
        merge_handoff(vec!["x", "y", "z"], vec!["b"]),
        vec!["b", "x", "y"]
    );
    assert_eq!(merge_handoff(Vec::<&str>::new(), vec!["b"]), vec!["b"]);
    assert_eq!(merge_handoff(vec!["x", "y"], Vec::new()), vec!["x", "y"]);
    assert_eq!(
        merge_handoff(vec!["x", "y"], vec!["b", "c"]),
        vec!["b", "c", "x"]
    );
    assert!(merge_handoff(Vec::<&str>::new(), Vec::new()).is_empty());
}

/// Known issue 25: a delegation to rank 0 forwards spare credits for rank 0's
/// lineage targets even when this owner has no handoff targets of its own;
/// never the owner's own origin credit.
#[test]
fn delegation_forwards_spare_credits_for_rank_zero_lineage() {
    for (lineage, attempts, child) in [(0, 8, 2), (1, 3, 2), (1, 4, 3), (2, 8, 4), (2, 4, 3)] {
        let mut cfg = config(Some(1), 3, VisitOrder::canonical(3));
        cfg.lineage = [lineage, 0, 0];
        let mut walk = CandidateWalk::new(cfg);
        let mut b = budget(attempts, 16);
        let Step::Probe(probe) = walk.next(&mut b).unwrap() else {
            panic!("expected a probe");
        };
        assert_eq!(probe.mode, Mode::Acquire);
        assert_eq!(
            walk.request_budget(&mut b).remaining_attempts(),
            child,
            "lineage {lineage} attempts {attempts}"
        );
        assert!(b.remaining_attempts() >= 1);
        assert_eq!(b.remaining_attempts() + child, attempts);
    }
}

/// Known issue 25: a non-owner's out-of-order Acquire carries one more
/// credit per lineage target of the destination and of rank 0, only from
/// slack.
#[test]
fn non_owner_funds_destination_lineage_from_slack_only() {
    let order = visit_order(ReplicaSelection::Spread, 3, |r| r != 0, |r| 3 - r as u64);
    assert_eq!(order.ranks().collect::<Vec<_>>(), [1, 2, 0]);
    let first = |attempts: u32, lineage: [u8; 3]| {
        let mut cfg = config(None, 3, order);
        cfg.lineage = lineage;
        let mut walk = CandidateWalk::new(cfg);
        let mut b = budget(attempts, 16);
        let Step::Probe(p) = walk.next(&mut b).unwrap() else {
            panic!("expected a probe");
        };
        (
            p.canonical,
            walk.request_budget(&mut b).remaining_attempts(),
        )
    };
    // Plenty of slack: rank 1 gets its funding plus its one target.
    assert_eq!(first(32, [0, 1, 0]), (1, owner_funding(1) + 1));
    // And for rank 0's, which rank 1 forwards when it delegates.
    assert_eq!(first(32, [2, 0, 1]), (1, owner_funding(1) + 2));
    assert_eq!(first(32, [1, 1, 0]), (1, owner_funding(1) + 2));
    // Not for another later rank's targets.
    assert_eq!(first(32, [0, 0, 1]), (1, owner_funding(1)));
    // Without slack for the extra credit, funding is as before: the later
    // visits of ranks 2 and 0 keep `owner_funding_min` each (4 + 2).
    assert_eq!(
        owner_funding(1) + 1 + owner_funding_min(2) + owner_funding_min(0),
        11
    );
    assert_eq!(first(11, [0, 1, 0]), (1, owner_funding(1) + 1));
    assert_eq!(first(10, [0, 1, 0]), (1, owner_funding(1)));
    assert_eq!(first(8, [0, 1, 0]), (1, owner_funding(1)));
    assert_eq!(first(8, [0, 0, 0]), (1, owner_funding(1)));
}

/// Known issue 26: a reader waiting for a destination that is behind asks
/// the same destination again, with the credits its verified
/// `StaleMembership` returned, and then walks on as before.
#[test]
fn retry_pending_plans_the_same_destination_again() {
    let order = visit_order(ReplicaSelection::Spread, 3, |r| r != 0, |r| 3 - r as u64);
    let mut walk = CandidateWalk::new(config(None, 3, order));
    let mut b = budget(8, 16);
    let Step::Probe(first) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    assert_eq!(first.canonical, 1);
    let request = walk.request_budget(&mut b);
    let plan = |request: &mut AcquisitionBudget, mode| {
        plan_attempt(
            request,
            AttemptInputs {
                now: Instant::now(),
                caller_deadline: Instant::now() + Duration::from_secs(30),
                attempt_timeout: Duration::from_secs(2),
                remaining_opportunities: 3,
                mode,
                route: RouteMode::FundedRoute,
                failure_links: 8,
            },
        )
        .unwrap()
    };
    let sent = plan(request, first.mode).remote_attempts;
    assert_eq!(sent, owner_funding(1) - 1);
    // The destination was behind: its credits come back, the exchange not.
    request.refund_remote(sent).unwrap();
    walk.retry_pending(&mut b).unwrap();
    assert_eq!(b.remaining_attempts(), 7);
    let Step::Probe(again) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    assert_eq!(
        (again.index, again.canonical),
        (first.index, first.canonical)
    );
    assert_eq!(
        walk.request_budget(&mut b).remaining_attempts(),
        owner_funding_min(1)
    );
    // Then the walk goes on as usual.
    walk.complete(&mut b, Outcome::StaleDestination).unwrap();
    let Step::Probe(next) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    assert_eq!(next.canonical, 2);
}

#[test]
fn delegated_rank_zero_asks_successors_on_their_own_grant() {
    // Rank 0 reached by a delegation: one origin credit, one link. The
    // successor probe is funded by its grant; the previous-membership target
    // after it is not (it would spend the origin credit).
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 2;
    cfg.granted = 1;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(1, 1);
    let mut granted = Vec::new();
    let (probes, end) = drive(&mut walk, &mut b, |_, request| {
        granted.push((request.remaining_attempts(), request.remaining_links()));
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(
        probes
            .iter()
            .map(|(p, ..)| (p.canonical, p.mode, p.route))
            .collect::<Vec<_>>(),
        [(3, Mode::CopyOnly, RouteMode::FundedRoute)]
    );
    assert_eq!(granted, [(SUCCESSOR_GRANT_ATTEMPTS, SUCCESSOR_GRANT_LINKS)]);
    assert_eq!(
        end,
        End::Origin {
            rank: 0,
            evidence: Vec::new()
        }
    );
    // The walk's own credits are untouched: the origin attempt remains.
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (1, 1));

    // A successor's copy ends the walk.
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 1;
    cfg.granted = 1;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(1, 1);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| Outcome::Copy);
    assert_eq!(probes.len(), 1);
    assert_eq!(end, End::Copy);
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (1, 1));
}

#[test]
fn successor_grant_never_grows_the_walk_budget() {
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 2;
    cfg.granted = 2;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(1, 10);
    // An unreachable successor is a miss and does not mark this walk's
    // route as failed (the grant's route is not this walk's).
    let (probes, end) = drive(&mut walk, &mut b, |p, _| {
        if p.canonical == 3 {
            Outcome::Transport(ProbeOutcome::Unreachable)
        } else {
            Outcome::StaleMembership
        }
    });
    assert_eq!(probes.len(), 2);
    assert!(probes.iter().all(|(p, ..)| p.mode == Mode::CopyOnly));
    assert!(matches!(end, End::Origin { rank: 0, .. }));
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (1, 10));
    assert_eq!(b.route_links(), 4);

    // Abandoning a pending successor probe drops its grant.
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 1;
    cfg.granted = 1;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(2, 3);
    let Step::Probe(probe) = walk.next(&mut b).unwrap() else {
        panic!("expected a successor probe");
    };
    assert!(walk.pending_successor());
    assert!(walk.pending_handoff());
    assert_eq!(probe.canonical, 3);
    walk.abandon(&mut b).unwrap();
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (2, 3));
}

#[test]
fn owner_above_rank_zero_asks_successors_after_its_delegation() {
    let mut cfg = config(Some(1), 3, VisitOrder::canonical(3));
    cfg.handoff = 1;
    cfg.granted = 1;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(3, 8);
    let (probes, end) = drive(&mut walk, &mut b, |p, _| {
        Outcome::Probe(if p.canonical == 0 {
            ProbeOutcome::Unreachable
        } else {
            ProbeOutcome::CopyMiss
        })
    });
    assert_eq!(
        probes
            .iter()
            .map(|(p, ..)| (p.canonical, p.mode))
            .collect::<Vec<_>>(),
        [(0, Mode::Acquire), (3, Mode::CopyOnly)]
    );
    assert_eq!(
        end,
        End::Origin {
            rank: 1,
            evidence: vec![ProbeOutcome::Unreachable]
        }
    );
    // Without successors nothing changes for a non-owner.
    let mut cfg = config(None, 3, VisitOrder::canonical(3));
    cfg.granted = 1;
    let mut walk = CandidateWalk::new(cfg);
    let (probes, _) = drive(&mut walk, &mut budget(32, 96), |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert!(probes.iter().all(|(p, ..)| p.canonical < 3));
}

/// Wave 4 merge: rank 0 asks a successor (own grant) and then a lineage
/// target before origin. A `StaleMembership` refund of the successor's
/// remote credits lands in the grant, which is dropped, so the walk's budget
/// never grows; and neither probe timing out is a rank 0 timeout, so rank 0
/// keeps origin authority (Known issues 21, 25, 26, 31).
#[test]
fn successor_and_lineage_probes_neither_grow_the_budget_nor_withhold_origin() {
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 2;
    cfg.granted = 1;
    cfg.lineage = [1, 0, 0];
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(4, 32);
    let start = (b.remaining_attempts(), b.remaining_links());
    let mut probes = Vec::new();
    let end = loop {
        match walk.next(&mut b).unwrap() {
            Step::Probe(probe) => {
                probes.push(probe.canonical);
                let successor = walk.pending_successor();
                let request = walk.request_budget(&mut b);
                let outcome = if successor {
                    let sent = request.send_remote(1).unwrap();
                    request.refund_remote(sent).unwrap();
                    Outcome::StaleDestination
                } else {
                    Outcome::TimedOut
                };
                if let Some(end) = walk.complete(&mut b, outcome).unwrap() {
                    break end;
                }
                let now = (b.remaining_attempts(), b.remaining_links());
                assert!(now.0 <= start.0 && now.1 <= start.1, "{now:?} > {start:?}");
            }
            Step::End(end) => break end,
        }
    };
    assert_eq!(probes, [3, 4]);
    assert!(matches!(end, End::Origin { rank: 0, .. }), "{end:?}");
    assert!(!walk.rank0_unanswered());
    assert!(b.remaining_attempts() <= start.0 && b.remaining_links() <= start.1);
}

#[test]
fn demoted_targets_are_members_that_were_owners_when_they_arrived() {
    // Owners arrived in generations 5, 6, 7; the members ranked fourth and
    // fifth arrived in 0 and 1, before any owner: each was an owner then.
    let since = [Some(5), Some(6), Some(7)];
    let demoted = ["d3", "d4"];
    assert_eq!(
        demoted_targets(&since, &demoted, &[Some(0), Some(1)], 0),
        ["d3", "d4"]
    );
    // Rank 2 is the newest owner: same targets.
    assert_eq!(
        demoted_targets(&since, &demoted, &[Some(0), Some(1)], 2),
        ["d3", "d4"]
    );
    // An owner that arrived before a demoted member never displaced it.
    assert_eq!(
        demoted_targets(&since, &demoted, &[Some(5), Some(1)], 0),
        ["d4"]
    );
    // Three members ranked above d4 were present when it arrived (owners 1
    // and 2 at generation 2, d3 at 0): d4 never was an owner.
    let since = [Some(9), Some(2), Some(2)];
    assert_eq!(
        demoted_targets(&since, &demoted, &[Some(0), Some(3)], 0),
        ["d3"]
    );
    // Unknown arrivals never add a target and count as present above.
    assert!(
        demoted_targets(&[None, Some(1), Some(1)], &demoted, &[Some(0), Some(0)], 0).is_empty()
    );
    assert_eq!(
        demoted_targets(&[Some(9), None, None], &demoted, &[Some(0), None], 0),
        ["d3"]
    );
    assert_eq!(
        demoted_targets(&[Some(9), None, None], &demoted, &[Some(0), Some(0)], 0),
        ["d3"],
        "d4 had three members above it"
    );
    // A cluster whose members arrived together pays nothing.
    assert!(demoted_targets(&[Some(0); 3], &demoted, &[Some(0); 2], 0).is_empty());
    // No demoted members or an unknown own arrival: nothing.
    assert!(demoted_targets::<&str>(&[Some(3); 3], &[], &[], 0).is_empty());
    assert!(demoted_targets(&[None, Some(3), Some(3)], &demoted, &[Some(0); 2], 0).is_empty());
}

#[test]
fn granted_prefix_counts_leading_successor_and_lineage_targets() {
    assert_eq!(granted_prefix(&["s", "l", "p"], &["s", "l"]), 2);
    assert_eq!(granted_prefix(&["p", "l"], &["l"]), 0);
    assert_eq!(granted_prefix::<&str>(&[], &["l"]), 0);
    let handoff = merge_handoff(vec!["p1", "p2"], vec!["l1", "d3"]);
    assert_eq!(handoff, ["l1", "d3", "p1"]);
    assert_eq!(granted_prefix(&handoff, &["l1", "d3"]), 2);
}

#[test]
fn delegated_rank_zero_asks_granted_lineage_targets_and_keeps_origin_credit() {
    // Known issue 35: rank 0 reached by a delegation has one credit. Its two
    // lineage targets are granted; the version - 1 target after them is
    // funded from the walk and stops before the origin credit.
    let mut cfg = config(Some(0), 3, VisitOrder::canonical(3));
    cfg.handoff = 3;
    cfg.granted = 2;
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(1, 1);
    let (probes, end) = drive(&mut walk, &mut b, |_, _| {
        Outcome::Probe(ProbeOutcome::CopyMiss)
    });
    assert_eq!(
        probes.iter().map(|(p, ..)| p.canonical).collect::<Vec<_>>(),
        [3, 4]
    );
    assert!(matches!(end, End::Origin { rank: 0, .. }));
    assert_eq!((b.remaining_attempts(), b.remaining_links()), (1, 1));
    // The second target holds the page: copy, no origin.
    let mut walk = CandidateWalk::new(cfg);
    let mut b = budget(1, 1);
    let (probes, end) = drive(&mut walk, &mut b, |p, _| {
        if p.canonical == 4 {
            Outcome::Copy
        } else {
            Outcome::Probe(ProbeOutcome::CopyMiss)
        }
    });
    assert_eq!(probes.len(), 2);
    assert_eq!(end, End::Copy);
}

/// Known issue 36: a further ask of a destination that is behind must leave
/// attempts for every later visit's minimum and a full route share of links
/// for each.
#[test]
fn retry_affordable_keeps_later_visits_funded() {
    let order = visit_order(ReplicaSelection::Spread, 3, |r| r != 0, |r| 3 - r as u64);
    for (attempts, links, affordable) in [
        (8, 16, false),
        (16, 32, true),
        // Attempts enough, links not.
        (16, 8, false),
        // Links enough, attempts not.
        (6, 32, false),
    ] {
        let mut walk = CandidateWalk::new(config(None, 3, order));
        let mut b = budget(attempts, links);
        let Step::Probe(_) = walk.next(&mut b).unwrap() else {
            panic!("expected a probe");
        };
        assert_eq!(
            walk.retry_affordable(&b),
            affordable,
            "{attempts} attempts, {links} links"
        );
    }
    // The last visit needs nothing after it.
    let mut walk = CandidateWalk::new(config(None, 3, order));
    let mut b = budget(8, 16);
    for _ in 0..2 {
        let Step::Probe(_) = walk.next(&mut b).unwrap() else {
            panic!("expected a probe");
        };
        walk.complete(&mut b, Outcome::StaleDestination).unwrap();
    }
    let Step::Probe(last) = walk.next(&mut b).unwrap() else {
        panic!("expected a probe");
    };
    assert_eq!(last.index, 2);
    assert!(
        walk.retry_affordable(&b),
        "{} attempts",
        b.remaining_attempts()
    );
}

/// Wave 5 interplay of the lineage grants (Known issues 34 and 35) with the
/// rank 0 timeout rule (Known issues 31, 39 and 40): an owner above rank 0
/// whose granted targets time out keeps origin authority, since only its
/// probe of rank 0 decides it; a timeout of rank 0 still withholds it after
/// granted probes; and the grants never draw on the walk's budget.
#[test]
fn granted_probes_leave_the_rank_zero_timeout_rule_alone() {
    let with = |state: Rank0State| WalkConfig {
        handoff: 2,
        granted: 2,
        rank0: state,
        ..config(Some(1), 3, VisitOrder::canonical(3))
    };
    for state in [Rank0State::Unknown, Rank0State::Fetching] {
        // Granted targets time out, rank 0 answers a miss: origin.
        let mut walk = CandidateWalk::new(with(state));
        let mut b = budget(8, 32);
        let (probes, end) = drive(&mut walk, &mut b, |p, _| {
            if p.canonical == 0 {
                Outcome::Probe(ProbeOutcome::CopyMiss)
            } else {
                Outcome::TimedOut
            }
        });
        assert!(probes.iter().any(|(p, ..)| p.canonical == 0), "{state:?}");
        assert!(
            probes.iter().filter(|(p, ..)| p.canonical >= 3).count() == 2,
            "{state:?}: both granted targets asked"
        );
        assert!(matches!(end, End::Origin { rank: 1, .. }), "{state:?}");
        assert!(!walk.rank0_unanswered(), "{state:?}");
        assert!(b.remaining_attempts() <= 8 && b.remaining_links() <= 32);
        // Rank 0 times out after the granted targets missed: withheld.
        let mut walk = CandidateWalk::new(with(state));
        let (_, end) = drive(&mut walk, &mut budget(8, 32), |p, _| {
            if p.canonical == 0 {
                Outcome::TimedOut
            } else {
                Outcome::Probe(ProbeOutcome::CopyMiss)
            }
        });
        assert_eq!(end, End::Exhausted { funding: false }, "{state:?}");
        assert!(walk.rank0_unanswered(), "{state:?}");
    }
}
