use super::*;
use crate::read::walk::Features;
use crate::read::{OperationKind, ResponseKind};
use std::time::Duration;

type Acq = PageAcquisition<&'static str, u64, &'static str>;
type Fx = Effect<&'static str, u64, &'static str>;
type In = Input<&'static str, u64, &'static str>;

struct Harness {
    acq: Acq,
    budget: AcquisitionBudget,
    ids: EffectIds,
    now: Instant,
}

impl Harness {
    fn new(hedge: bool) -> Self {
        Self::with(hedge, Features::PRODUCTION, 8, 16)
    }

    fn with(hedge: bool, features: Features, attempts: u32, links: u8) -> Self {
        let now = Instant::now();
        let config = FillConfig {
            operation: OperationKind::Page,
            hedge,
            walk_only: false,
            scope: now + Duration::from_secs(60),
            features,
        };
        Self {
            acq: PageAcquisition::new(config, "initial", now),
            budget: AcquisitionBudget::new(now + Duration::from_secs(30), attempts, links),
            ids: EffectIds::new(),
            now,
        }
    }

    fn feed(&mut self, input: In) -> Vec<Fx> {
        let mut out = Vec::new();
        self.acq
            .on(input, &mut self.budget, self.now, &mut self.ids, &mut out)
            .expect("accepted");
        out
    }

    fn id(&self) -> EffectId {
        self.acq.pending().expect("an outstanding effect")
    }

    /// Effects without reports.
    fn act(&mut self, input: In) -> Vec<Fx> {
        self.feed(input)
            .into_iter()
            .filter(|effect| !matches!(effect, Effect::Report(_)))
            .collect()
    }

    fn ranked(&mut self, version: u64, rank: Option<usize>, ranking: &'static str) -> Vec<Fx> {
        let id = self.id();
        self.act(Input::Ranked {
            id,
            result: Ok(RankAnswer {
                ranked: owners(version, rank),
                ranking: Some(ranking),
                recent: None,
            }),
        })
    }

    fn reply(&mut self, reply: Reply, answer: Option<&'static str>) -> Vec<Fx> {
        let id = self.id();
        self.act(Input::Replied { id, reply, answer })
    }

    fn done(&self) -> Done {
        self.acq.done().expect("done")
    }
}

fn owners(version: u64, rank: Option<usize>) -> Ranked {
    Ranked {
        members: 3,
        ..Ranked::new(version, &[10, 11, 12], rank)
    }
}

fn page() -> Reply {
    Reply::Answer {
        kind: ResponseKind::Page,
        signed: true,
    }
}

fn kind(effect: &Fx) -> &'static str {
    match effect {
        Effect::Io(Io::Rank { .. }) => "Rank",
        Effect::Io(Io::Hints { .. }) => "Hints",
        Effect::Io(Io::Hedge { .. }) => "Hedge",
        Effect::Io(Io::Refresh { .. }) => "Refresh",
        Effect::Io(Io::Send { .. }) => "Send",
        Effect::Io(Io::Validate { .. }) => "Validate",
        Effect::Io(Io::Batch { .. }) => "Batch",
        Effect::Io(Io::Sleep { .. }) => "Sleep",
        Effect::Io(Io::Origin { .. }) => "Origin",
        Effect::Io(Io::Publish { .. }) => "Publish",
        Effect::Report(_) => "Report",
        Effect::Done(_) => "Done",
    }
}

fn kinds(effects: &[Fx]) -> Vec<&'static str> {
    effects.iter().map(kind).collect()
}

fn start(h: &mut Harness) -> Vec<Fx> {
    h.act(Input::Start { local_hit: false })
}

/// An owner at `rank` walks to its hints.
fn owner_to_hints(h: &mut Harness, rank: usize) {
    start(h);
    assert_eq!(kinds(&h.ranked(5, Some(rank), "r5")), ["Rank"]);
    let id = h.id();
    let effects = h.act(Input::Ranked {
        id,
        result: Err(Error::Unavailable),
    });
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Hints { version: 5, .. })]
    ));
}

#[test]
fn split_separates_io_from_what_the_owner_consumes() {
    let mut h = Harness::new(true);
    let done = h.act(Input::Start { local_hit: true }).remove(0);
    assert!(matches!(done.split(), Err(Terminal::Done(_))));
    let mut h = Harness::new(true);
    let first = start(&mut h).remove(0);
    let io = first.clone().split().expect("a walk starts with I/O");
    assert_eq!(kind(&first), "Rank");
    assert!(matches!(io, Io::Rank { .. }));
}

#[test]
fn local_hit_is_done_without_effects() {
    let mut h = Harness::new(true);
    let effects = h.act(Input::Start { local_hit: true });
    assert!(matches!(
        &effects[..],
        [Effect::Done(Done {
            result: Ok(Source::Local),
            phase: Phase::Walk,
            ..
        })]
    ));
}

#[test]
fn first_ranking_verifies_the_callers_ranking() {
    let mut h = Harness::new(false);
    let effects = start(&mut h);
    assert!(matches!(
        &effects[..],
        [Effect::Io(Io::Rank {
            ranking: Ranking::Current,
            basis: RankBasis::Verify("initial"),
            ..
        })]
    ));
}

#[test]
fn hedge_won_publishes_the_held_copy() {
    let mut h = Harness::new(true);
    start(&mut h);
    let effects = h.ranked(5, None, "r5");
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Hedge {
            eligible: true,
            first: 0,
            second: 1,
            ..
        })]
    ));
    let id = h.id();
    let effects = h.act(Input::Hedged {
        id,
        end: HedgeEnd::Won,
        copy: true,
    });
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Publish {
            source: Source::Hedge,
            ..
        })]
    ));
    let id = h.id();
    h.act(Input::Published { id, result: Ok(()) });
    assert_eq!(h.done().result, Ok(Source::Hedge));
}

#[test]
fn a_win_without_a_held_copy_is_internal() {
    let mut h = Harness::new(true);
    start(&mut h);
    h.ranked(5, None, "r5");
    let id = h.id();
    let effects = h.act(Input::Hedged {
        id,
        end: HedgeEnd::Won,
        copy: false,
    });
    assert_eq!(kinds(&effects), ["Done"]);
    assert_eq!(h.done().result, Err(Error::Internal));
}

#[test]
fn reader_probe_validates_and_publishes_a_peer_copy() {
    let mut h = Harness::new(false);
    start(&mut h);
    let effects = h.ranked(5, None, "r5");
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Send {
            dest: Some(10),
            version: 5,
            ..
        })]
    ));
    assert_eq!(h.acq.current(), Some(&"r5"));
    let effects = h.reply(page(), Some("bytes"));
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Validate {
            answer: "bytes",
            ..
        })]
    ));
    let id = h.id();
    let effects = h.act(Input::Validated {
        id,
        result: Ok(None),
    });
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Publish {
            source: Source::Peer,
            ..
        })]
    ));
    let id = h.id();
    h.act(Input::Published { id, result: Ok(()) });
    assert_eq!(h.done().result, Ok(Source::Peer));
}

#[test]
fn an_answer_without_its_payload_fails_validation() {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, None, "r5");
    let effects = h.reply(page(), None);
    // The missing payload is an internal validation error: terminal for the read.
    assert_eq!(kinds(&effects), ["Done"]);
    assert_eq!(h.done().result, Err(Error::Internal));
}

#[test]
fn evidence_reports_the_probed_destination() {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, None, "r5");
    let id = h.id();
    let effects = h.feed(Input::Replied {
        id,
        reply: Reply::Failed {
            error: Error::Io,
            sent: true,
            scope_live: true,
        },
        answer: None,
    });
    let report = effects.iter().find_map(|effect| match effect {
        Effect::Report(Report::Evidence {
            dest,
            version,
            evidence,
            ..
        }) => Some((*dest, *version, evidence.outcome)),
        _ => None,
    });
    assert_eq!(
        report,
        Some((
            Some(10),
            5,
            Some(Ok(super::super::walk::Outcome::Transport(
                ProbeOutcome::Unreachable
            )))
        ))
    );
    assert!(matches!(
        effects.last(),
        Some(Effect::Io(Io::Send { dest: Some(11), .. }))
    ));
}

#[test]
fn an_unsent_failure_still_moves_on() {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, None, "r5");
    let effects = h.reply(
        Reply::Failed {
            error: Error::Overloaded,
            sent: false,
            scope_live: true,
        },
        None,
    );
    assert_eq!(kinds(&effects), ["Send"]);
}

#[test]
fn owner_rank0_fetches_from_origin_with_authority() {
    let mut h = Harness::new(false);
    owner_to_hints(&mut h, 0);
    let id = h.id();
    let effects = h.act(Input::Hints {
        id,
        hints: vec![],
        rank0: Rank0State::Unknown,
    });
    let authority = Authority {
        version: 5,
        owners: List::from_slice(&[10, 11, 12]),
        rank: 0,
    };
    assert!(
        matches!(&effects[..], [Effect::Io(Io::Origin { authority: a, .. })] if *a == authority)
    );
    assert_eq!(h.acq.phase(), Phase::Origin);
    let id = h.id();
    let effects = h.feed(Input::Origin { id, result: Ok(()) });
    assert!(matches!(
        &effects[..],
        [
            Effect::Report(Report::OriginFetched { .. }),
            Effect::Io(Io::Publish {
                source: Source::Origin,
                ..
            })
        ]
    ));
    let id = h.id();
    h.act(Input::Published { id, result: Ok(()) });
    assert_eq!(h.done().result, Ok(Source::Origin));
    assert_eq!(h.done().phase, Phase::Origin);
}

#[test]
fn origin_failure_is_reported_and_terminal() {
    let mut h = Harness::new(false);
    owner_to_hints(&mut h, 0);
    let id = h.id();
    h.act(Input::Hints {
        id,
        hints: vec![],
        rank0: Rank0State::Unknown,
    });
    let id = h.id();
    let effects = h.feed(Input::Origin {
        id,
        result: Err(Error::OriginForbidden),
    });
    assert!(matches!(
        effects[0],
        Effect::Report(Report::OriginFailed {
            error: Error::OriginForbidden,
            ..
        })
    ));
    assert_eq!(h.done().result, Err(Error::OriginForbidden));
}

#[test]
fn origin_version_miss_starts_the_remaining_walk_without_a_failure_report() {
    let mut h = Harness::new(false);
    owner_to_hints(&mut h, 0);
    let id = h.id();
    h.act(Input::Hints {
        id,
        hints: vec![],
        rank0: Rank0State::Unknown,
    });
    let id = h.id();
    let effects = h.feed(Input::Origin {
        id,
        result: Err(Error::VersionUnavailable),
    });
    assert!(!effects.iter().any(|e| matches!(e, Effect::Report(_))));
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Send { dest: Some(11), .. })]
    ));
    assert_eq!(h.acq.phase(), Phase::Remaining);
    let effects = h.reply(
        Reply::Answer {
            kind: ResponseKind::Miss,
            signed: true,
        },
        None,
    );
    assert_eq!(kinds(&effects), ["Send"]);
}

#[test]
fn origin_without_budget_fails_before_fetching() {
    let mut h = Harness::new(false);
    owner_to_hints(&mut h, 0);
    let id = h.id();
    // The read's deadline passes while the hints are computed.
    h.now += Duration::from_secs(31);
    let effects = h.act(Input::Hints {
        id,
        hints: vec![],
        rank0: Rank0State::Unknown,
    });
    assert_eq!(kinds(&effects), ["Done"]);
    // The authority ranking itself refuses a read past its deadline.
    assert_eq!(h.done().result, Err(Error::DeadlineExceeded));
    assert_eq!(h.done().phase, Phase::Walk);
}

#[test]
fn origin_without_attempts_fails_before_fetching() {
    let mut h = Harness::with(false, Features::PRODUCTION, 0, 16);
    owner_to_hints(&mut h, 0);
    let id = h.id();
    let effects = h.act(Input::Hints {
        id,
        hints: vec![],
        rank0: Rank0State::Unknown,
    });
    assert_eq!(kinds(&effects), ["Done"]);
    assert_eq!(h.done().result, Err(Error::Unavailable));
    assert_eq!(h.done().phase, Phase::Origin);
}

#[test]
fn a_walk_only_origin_leaves_the_attempt_to_its_caller() {
    let mut h = Harness::with(false, Features::PRODUCTION, 0, 16);
    h.acq = PageAcquisition::new(
        FillConfig {
            operation: OperationKind::Page,
            hedge: false,
            walk_only: true,
            scope: h.now + Duration::from_secs(60),
            features: Features::PRODUCTION,
        },
        "initial",
        h.now,
    );
    owner_to_hints(&mut h, 0);
    let id = h.id();
    let effects = h.act(Input::Hints {
        id,
        hints: vec![],
        rank0: Rank0State::Unknown,
    });
    assert_eq!(kinds(&effects), ["Origin"]);
    assert_eq!(h.budget.remaining_attempts(), 0);
}

#[test]
fn done_carries_the_final_notes() {
    let mut h = Harness::new(false);
    exhaust(&mut h);
    assert_ne!(h.done().notes & Notes::EXHAUSTED, 0);
}

#[test]
fn successors_become_a_granted_batch() {
    let mut h = Harness::new(false);
    owner_to_hints(&mut h, 0);
    let id = h.id();
    let effects = h.act(Input::Hints {
        id,
        hints: vec![12, 99],
        rank0: Rank0State::Unknown,
    });
    assert!(
        matches!(&effects[..], [Effect::Io(Io::Batch { targets, version: 5, .. })] if targets == &[12])
    );
    let id = h.id();
    let effects = h.act(Input::Batched {
        id,
        result: Ok(true),
    });
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Publish {
            source: Source::Peer,
            ..
        })]
    ));
}

#[test]
fn a_failed_batch_aborts() {
    let mut h = Harness::new(false);
    owner_to_hints(&mut h, 0);
    let id = h.id();
    h.act(Input::Hints {
        id,
        hints: vec![12],
        rank0: Rank0State::Unknown,
    });
    let id = h.id();
    h.act(Input::Batched {
        id,
        result: Err(Error::Canceled),
    });
    assert_eq!(h.done().result, Err(Error::Canceled));
}

fn stale_answer() -> Reply {
    Reply::Answer {
        kind: ResponseKind::StaleMembership,
        signed: true,
    }
}

#[test]
fn refresh_ranks_the_adopted_membership() {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, None, "r5");
    let effects = h.reply(stale_answer(), None);
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Refresh { version: 5, .. })]
    ));
    let id = h.id();
    let effects = h.act(Input::Membership {
        id,
        latest: Ok(Some((6, 600))),
        recent_previous: None,
    });
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Rank {
            ranking: Ranking::Current,
            basis: RankBasis::Latest(600),
            ..
        })]
    ));
    let effects = h.ranked(6, None, "r6");
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Send { version: 6, .. })]
    ));
    assert_eq!(h.acq.current(), Some(&"r6"));
}

#[test]
fn a_failed_refresh_aborts() {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, None, "r5");
    h.reply(stale_answer(), None);
    let id = h.id();
    h.act(Input::Membership {
        id,
        latest: Err(Error::Io),
        recent_previous: None,
    });
    assert_eq!(h.done().result, Err(Error::Io));
}

#[test]
fn fallback_walks_the_previous_membership_and_swaps_rankings() {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, None, "r5");
    h.reply(stale_answer(), None);
    let id = h.id();
    let effects = h.act(Input::Membership {
        id,
        latest: Ok(Some((5, 500))),
        recent_previous: Some(400),
    });
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Rank {
            ranking: Ranking::Fallback,
            basis: RankBasis::Fallback(400),
            ..
        })]
    ));
    let effects = h.ranked(4, None, "r4");
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Send { version: 4, .. })]
    ));
    assert_eq!(h.acq.current(), Some(&"r4"));
}

fn sleeping() -> Harness {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, None, "r5");
    h.reply(stale_answer(), None);
    let id = h.id();
    let effects = h.act(Input::Membership {
        id,
        latest: Ok(Some((5, 500))),
        recent_previous: None,
    });
    let until = h.now + Duration::from_secs(4);
    assert!(matches!(effects[..], [Effect::Io(Io::Sleep { until: u, .. })] if u == until));
    h
}

#[test]
fn a_stale_wait_sleeps_and_a_failed_sleep_is_terminal() {
    let mut h = sleeping();
    let id = h.id();
    h.act(Input::Timer {
        id,
        result: Err(Error::Canceled),
    });
    let failed = h.done();
    assert_eq!(failed.result, Err(Error::Canceled));
    // A failed sleep ends through the walk exactly like a cancel does.
    let mut canceled = sleeping();
    canceled.act(Input::Cancel(Error::Canceled));
    assert_eq!(failed, canceled.done());
}

#[test]
fn an_elapsed_wait_probes_again() {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, None, "r5");
    h.reply(stale_answer(), None);
    let id = h.id();
    h.act(Input::Membership {
        id,
        latest: Ok(Some((5, 500))),
        recent_previous: None,
    });
    let id = h.id();
    let effects = h.act(Input::Timer { id, result: Ok(()) });
    assert!(matches!(
        effects[..],
        [Effect::Io(Io::Send { dest: Some(10), .. })]
    ));
}

fn exhaust(h: &mut Harness) {
    start(h);
    h.ranked(5, None, "r5");
    for _ in 0..3 {
        h.reply(
            Reply::Failed {
                error: Error::Io,
                sent: true,
                scope_live: true,
            },
            None,
        );
    }
}

#[test]
fn an_exhausted_walk_is_unavailable() {
    let mut h = Harness::new(false);
    exhaust(&mut h);
    let done = h.done();
    assert_eq!(done.result, Err(Error::Unavailable));
    assert!(done.exhausted);
    assert_eq!(done.phase, Phase::Walk);
}

#[test]
fn an_exhausted_walk_past_its_deadline_reports_the_deadline() {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, None, "r5");
    let failed = Reply::Failed {
        error: Error::Io,
        sent: true,
        scope_live: true,
    };
    h.reply(failed, None);
    h.reply(failed, None);
    h.now += Duration::from_secs(31);
    h.reply(failed, None);
    assert_eq!(h.done().result, Err(Error::DeadlineExceeded));
}

#[test]
fn cancel_ends_an_outstanding_send_and_late_replies_are_stale() {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, None, "r5");
    let id = h.id();
    let effects = h.act(Input::Cancel(Error::Canceled));
    assert_eq!(kinds(&effects), ["Done"]);
    assert_eq!(h.done().result, Err(Error::Canceled));
    let mut out = Vec::new();
    let late = h.acq.on(
        Input::Replied {
            id,
            reply: page(),
            answer: Some("late"),
        },
        &mut h.budget,
        h.now,
        &mut h.ids,
        &mut out,
    );
    assert_eq!(late, Err(Stale(id)));
    assert!(out.is_empty());
}

#[test]
fn mismatched_ids_and_kinds_are_refused_without_a_state_change() {
    let mut h = Harness::new(false);
    start(&mut h);
    let id = h.id();
    let mut out = Vec::new();
    let wrong_id = EffectId(id.0 + 100);
    assert_eq!(
        h.acq.on(
            Input::Timer {
                id: wrong_id,
                result: Ok(())
            },
            &mut h.budget,
            h.now,
            &mut h.ids,
            &mut out
        ),
        Err(Stale(wrong_id))
    );
    // The right id with the wrong kind of answer.
    assert_eq!(
        h.acq.on(
            Input::Timer { id, result: Ok(()) },
            &mut h.budget,
            h.now,
            &mut h.ids,
            &mut out
        ),
        Err(Stale(id))
    );
    assert!(out.is_empty());
    assert_eq!(h.acq.pending(), Some(id));
    // A second start is refused too.
    assert!(
        h.acq
            .on(
                Input::Start { local_hit: true },
                &mut h.budget,
                h.now,
                &mut h.ids,
                &mut out
            )
            .is_err()
    );
    assert!(out.is_empty());
}

#[test]
fn empty_or_missing_rankings_are_incompatible_or_internal() {
    let mut h = Harness::new(false);
    start(&mut h);
    let id = h.id();
    h.act(Input::Ranked {
        id,
        result: Ok(RankAnswer {
            ranked: Ranked::new(5, &[], None),
            ranking: Some("empty"),
            recent: None,
        }),
    });
    assert_eq!(h.done().result, Err(Error::IncompatibleMembership));

    let mut h = Harness::new(false);
    start(&mut h);
    let id = h.id();
    h.act(Input::Ranked {
        id,
        result: Ok(RankAnswer {
            ranked: owners(5, None),
            ranking: None,
            recent: None,
        }),
    });
    assert_eq!(h.done().result, Err(Error::Internal));
}

#[test]
fn previous_ranking_keeps_the_recent_membership() {
    let mut h = Harness::new(false);
    start(&mut h);
    h.ranked(5, Some(0), "r5");
    let id = h.id();
    h.act(Input::Ranked {
        id,
        result: Ok(RankAnswer {
            ranked: Ranked {
                recent_member: true,
                ..Ranked::new(4, &[10, 11, 12], None)
            },
            ranking: None,
            recent: Some(400),
        }),
    });
    assert_eq!(h.acq.previous(), Some(&400));
}

/// Seeded random answers never panic, never emit more than one answered effect per input, and
/// always end in `Done` within a bounded number of steps.
#[test]
fn seeded_random_answers_always_terminate() {
    let mut seed = 0x9e37_79b9_7f4a_7c15u64;
    let mut rand = move || {
        seed ^= seed << 13;
        seed ^= seed >> 7;
        seed ^= seed << 17;
        seed
    };
    for _ in 0..500 {
        let mut h = Harness::new(rand() % 2 == 0);
        let mut effects = start(&mut h);
        for _ in 0..200 {
            let Some(effect) = effects.pop() else { break };
            assert!(effects.is_empty(), "one effect at a time");
            let pick = rand();
            let input: In = match effect {
                Effect::Done(_) => break,
                Effect::Report(_) => unreachable!("filtered"),
                Effect::Io(Io::Rank { id, ranking, .. }) => {
                    let rank = match pick % 4 {
                        0 => None,
                        r => Some(r as usize - 1),
                    };
                    let version = 3 + pick % 4;
                    Input::Ranked {
                        id,
                        result: match (ranking, pick % 7) {
                            (_, 0) => Err(Error::Unavailable),
                            (Ranking::Previous, _) => Ok(RankAnswer {
                                ranked: Ranked::new(version, &[10, 11, 12], None),
                                ranking: None,
                                recent: Some(version),
                            }),
                            _ => Ok(RankAnswer {
                                ranked: owners(version, rank),
                                ranking: Some("r"),
                                recent: None,
                            }),
                        },
                    }
                }
                Effect::Io(Io::Hints { id, .. }) => Input::Hints {
                    id,
                    hints: if pick % 2 == 0 { vec![] } else { vec![12] },
                    rank0: Rank0State::Unknown,
                },
                Effect::Io(Io::Hedge { id, .. }) => Input::Hedged {
                    id,
                    end: if pick % 2 == 0 {
                        HedgeEnd::Won
                    } else {
                        HedgeEnd::NotStarted
                    },
                    copy: pick % 2 == 0,
                },
                Effect::Io(Io::Refresh { id, .. }) => Input::Membership {
                    id,
                    latest: Ok(Some((4 + pick % 3, 1))),
                    recent_previous: (pick % 2 == 0).then_some(2),
                },
                Effect::Io(Io::Send { id, .. }) => match pick % 6 {
                    0 => Input::NoHelper { id },
                    1 => Input::Replied {
                        id,
                        reply: page(),
                        answer: Some("a"),
                    },
                    2 => Input::Replied {
                        id,
                        reply: stale_answer(),
                        answer: None,
                    },
                    3 => Input::Replied {
                        id,
                        reply: Reply::TimedOut,
                        answer: None,
                    },
                    4 => Input::Replied {
                        id,
                        reply: Reply::Answer {
                            kind: ResponseKind::Miss,
                            signed: true,
                        },
                        answer: Some("m"),
                    },
                    _ => Input::Replied {
                        id,
                        reply: Reply::Failed {
                            error: Error::Io,
                            sent: pick % 4 != 0,
                            scope_live: true,
                        },
                        answer: None,
                    },
                },
                Effect::Io(Io::Validate { id, .. }) => Input::Validated {
                    id,
                    result: if pick % 3 == 0 {
                        Ok(Some(ProbeOutcome::UnusableCopy))
                    } else {
                        Ok(None)
                    },
                },
                Effect::Io(Io::Batch { id, .. }) => Input::Batched {
                    id,
                    result: Ok(pick % 2 == 0),
                },
                Effect::Io(Io::Sleep { id, .. }) => Input::Timer { id, result: Ok(()) },
                Effect::Io(Io::Origin { id, .. }) => Input::Origin {
                    id,
                    result: match pick % 3 {
                        0 => Ok(()),
                        1 => Err(Error::VersionUnavailable),
                        _ => Err(Error::Io),
                    },
                },
                Effect::Io(Io::Publish { id, .. }) => Input::Published { id, result: Ok(()) },
            };
            effects = h.act(input);
        }
        let done = h.acq.done().expect("terminated");
        if let Ok(source) = done.result {
            assert_ne!(source, Source::Local);
        }
    }
}
