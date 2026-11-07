//! `PageAcquisition`: one page read's acquisition as a sans-IO machine.
//!
//! It wraps the candidate walk [`Fill`] with the state the dataplane kept beside it (the rankings
//! the walk refers to, the memberships a refresh or fallback adopts, the answer awaiting
//! validation, the outstanding probe's destination, the phase and the origin authority) and the
//! outer hedge, origin and publish loop. The caller performs [`Effect`]s and answers each with an
//! [`Input`] carrying the same [`EffectId`]; at most one effect is outstanding at a time.
//!
//! Payloads stay with the interpreter: `R` is a ranking (the dataplane's candidate list), `M` a
//! membership handle and `A` an authenticated peer answer. The acquired page itself is never held
//! here; the machine only tracks whether the interpreter holds one.

use super::effect::{EffectId, EffectIds, Stale};
use super::walk::{
    Action, Event, Evidence, Fill, FillConfig, HedgeEnd, List, MAX_HANDOFF, Notes, Probe,
    Rank0State, Ranked, Ranking, RemainingEnd, Reply,
};
use super::{AcquisitionBudget, Error, ProbeOutcome, Source};
use std::time::Instant;

/// Which part of the acquisition runs.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    /// The candidate walk (and the hedge before it).
    Walk,
    /// This owner fetches from origin.
    Origin,
    /// The copy-only walk after an origin version miss.
    Remaining,
}

/// The ranking an origin fetch is authorized by.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Authority {
    pub version: u64,
    pub owners: List<u32, MAX_HANDOFF>,
    pub rank: usize,
}

/// What the interpreter ranks for [`Effect::Rank`].
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum RankBasis<R, M> {
    /// The caller's own ranking: recompute it and fail with `IncompatibleMembership` when it
    /// differs (public rankings are not authority).
    Verify(R),
    /// The membership a refresh adopted.
    Latest(M),
    /// Membership `version` (version - 1 of the walked one), as positions in the walked one.
    /// Return the recent membership in [`RankAnswer::recent`] when this node is a member of it.
    Previous { version: u64 },
    /// The previous membership a stale fallback walks.
    Fallback(M),
}

/// The interpreter's answer to [`Effect::Rank`].
#[derive(Clone, Debug)]
pub struct RankAnswer<R, M> {
    pub ranked: Ranked,
    /// The ranking (`Verify`, `Latest`, `Fallback`).
    pub ranking: Option<R>,
    /// `Previous`: version - 1 while recent and listing this node.
    pub recent: Option<M>,
}

/// An effect of a running acquisition that the interpreter performs: every [`Effect`] except the
/// [`Effect::Report`]s and [`Effect::Done`] the owner consumes. Holding an `Io` proves the effect
/// is neither, so code downstream of the owner cannot meet them.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Io<R, M, A> {
    /// Rank the page; answer [`Input::Ranked`].
    Rank {
        id: EffectId,
        ranking: Ranking,
        basis: RankBasis<R, M>,
    },
    /// Report successor hints and rank 0's state under `version`; answer [`Input::Hints`].
    Hints { id: EffectId, version: u64 },
    /// Race a hedge if enabled, eligible and admitted; answer [`Input::Hedged`].
    Hedge {
        id: EffectId,
        eligible: bool,
        first: usize,
        second: usize,
    },
    /// Report the newest adopted membership and version - 1 of `version` when recent and listing
    /// this node; answer [`Input::Membership`].
    Refresh { id: EffectId, version: u64 },
    /// Send `probe` to member position `dest` of `version` (`None`: the overflow helper) on
    /// [`PageAcquisition::request_budget`]; answer [`Input::Replied`] or [`Input::NoHelper`].
    Send {
        id: EffectId,
        probe: Probe,
        dest: Option<u32>,
        version: u64,
    },
    /// Validate the payload of the answer; answer [`Input::Validated`]. A usable copy is kept by
    /// the interpreter.
    Validate { id: EffectId, answer: A },
    /// Send copy-only probes to `targets`, each on its own granted budget; answer
    /// [`Input::Batched`].
    Batch {
        id: EffectId,
        version: u64,
        targets: Vec<u32>,
        remaining_opportunities: u32,
    },
    /// Wake at `until`; answer [`Input::Timer`].
    Sleep { id: EffectId, until: Instant },
    /// Fetch the page from origin under `authority`; answer [`Input::Origin`].
    Origin { id: EffectId, authority: Authority },
    /// Publish the held page; answer [`Input::Published`].
    Publish { id: EffectId, source: Source },
}

impl<R, M, A> Effect<R, M, A> {
    /// Split an effect into what the interpreter performs and what the owner consumes.
    pub fn split(self) -> Result<Io<R, M, A>, Terminal> {
        Ok(match self {
            Effect::Rank { id, ranking, basis } => Io::Rank { id, ranking, basis },
            Effect::Hints { id, version } => Io::Hints { id, version },
            Effect::Hedge {
                id,
                eligible,
                first,
                second,
            } => Io::Hedge {
                id,
                eligible,
                first,
                second,
            },
            Effect::Refresh { id, version } => Io::Refresh { id, version },
            Effect::Send {
                id,
                probe,
                dest,
                version,
            } => Io::Send {
                id,
                probe,
                dest,
                version,
            },
            Effect::Validate { id, answer } => Io::Validate { id, answer },
            Effect::Batch {
                id,
                version,
                targets,
                remaining_opportunities,
            } => Io::Batch {
                id,
                version,
                targets,
                remaining_opportunities,
            },
            Effect::Sleep { id, until } => Io::Sleep { id, until },
            Effect::Origin { id, authority } => Io::Origin { id, authority },
            Effect::Publish { id, source } => Io::Publish { id, source },
            Effect::Report(report) => return Err(Terminal::Report(report)),
            Effect::Done(done) => return Err(Terminal::Done(done)),
        })
    }
}

/// The effects of an acquisition its owner consumes instead of performing.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Terminal {
    Report(Report),
    Done(Done),
}

/// One external effect.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Effect<R, M, A> {
    /// Rank the page; answer [`Input::Ranked`].
    Rank {
        id: EffectId,
        ranking: Ranking,
        basis: RankBasis<R, M>,
    },
    /// Report successor hints and rank 0's state under `version`; answer [`Input::Hints`].
    Hints { id: EffectId, version: u64 },
    /// Race a hedge if enabled, eligible and admitted; answer [`Input::Hedged`].
    Hedge {
        id: EffectId,
        eligible: bool,
        first: usize,
        second: usize,
    },
    /// Report the newest adopted membership and version - 1 of `version` when recent and listing
    /// this node; answer [`Input::Membership`].
    Refresh { id: EffectId, version: u64 },
    /// Send `probe` to member position `dest` of `version` (`None`: the overflow helper) on
    /// [`PageAcquisition::request_budget`]; answer [`Input::Replied`] or [`Input::NoHelper`].
    Send {
        id: EffectId,
        probe: Probe,
        dest: Option<u32>,
        version: u64,
    },
    /// Validate the payload of the answer; answer [`Input::Validated`]. A usable copy is kept by
    /// the interpreter.
    Validate { id: EffectId, answer: A },
    /// Send copy-only probes to `targets`, each on its own granted budget; answer
    /// [`Input::Batched`].
    Batch {
        id: EffectId,
        version: u64,
        targets: Vec<u32>,
        remaining_opportunities: u32,
    },
    /// Wake at `until`; answer [`Input::Timer`].
    Sleep { id: EffectId, until: Instant },
    /// Fetch the page from origin under `authority`; answer [`Input::Origin`].
    Origin { id: EffectId, authority: Authority },
    /// Publish the held page; answer [`Input::Published`].
    Publish { id: EffectId, source: Source },
    /// Observations for counters and shared health state; no answer.
    Report(Report),
    /// The acquisition ended. The interpreter's held page is the result on `Ok`.
    Done(Done),
}

/// What an [`Effect::Report`] tells the interpreter.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Report {
    /// What one walk step showed about the probe it answered, sent to `dest` of `version` at
    /// `sent` (`dest: None` for the helper). The remaining-copy walk reports none.
    Evidence {
        evidence: Evidence,
        dest: Option<u32>,
        version: u64,
        sent: Instant,
    },
    /// An origin fetch under `authority` succeeded: the interpreter notes orphaned predecessors.
    OriginFetched { authority: Authority },
    /// An origin fetch failed for a reason other than a version miss.
    OriginFailed { authority: Authority, error: Error },
}

/// How the acquisition ended.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Done {
    pub result: Result<Source, Error>,
    pub phase: Phase,
    /// The walk ran out of candidates (`Notes::EXHAUSTED`).
    pub exhausted: bool,
    /// The last walk could not fund a probe.
    pub funding: bool,
    pub remaining: Option<RemainingEnd>,
    /// The final step's evidence notes ([`Notes`]).
    pub notes: u16,
}

/// One completion or external event.
#[derive(Clone, Debug)]
pub enum Input<R, M, A> {
    /// The local tiers were checked; the interpreter holds the page when `local_hit`.
    Start { local_hit: bool },
    Ranked {
        id: EffectId,
        result: Result<RankAnswer<R, M>, Error>,
    },
    Hints {
        id: EffectId,
        hints: Vec<u32>,
        rank0: Rank0State,
    },
    /// `copy`: the interpreter holds the hedge's validated page.
    Hedged {
        id: EffectId,
        end: HedgeEnd,
        copy: bool,
    },
    Membership {
        id: EffectId,
        latest: Result<Option<(u64, M)>, Error>,
        recent_previous: Option<M>,
    },
    /// The probe's reply. `answer` must be present for [`Reply::Answer`]; `Reply::Failed::sent`
    /// is false only when the request never left this node.
    Replied {
        id: EffectId,
        reply: Reply,
        answer: Option<A>,
    },
    /// Every candidate helper is excluded.
    NoHelper { id: EffectId },
    /// `Ok(None)`: the interpreter now holds a usable copy.
    Validated {
        id: EffectId,
        result: Result<Option<ProbeOutcome>, Error>,
    },
    /// `Ok(true)`: the interpreter now holds a validated copy.
    Batched {
        id: EffectId,
        result: Result<bool, Error>,
    },
    Timer {
        id: EffectId,
        result: Result<(), Error>,
    },
    /// `Ok`: the interpreter now holds the origin page.
    Origin {
        id: EffectId,
        result: Result<(), Error>,
    },
    Published {
        id: EffectId,
        result: Result<(), Error>,
    },
    /// The caller's scope ended.
    Cancel(Error),
}

impl<R, M, A> Input<R, M, A> {
    fn id(&self) -> Option<EffectId> {
        match self {
            Input::Start { .. } | Input::Cancel(_) => None,
            Input::Ranked { id, .. }
            | Input::Hints { id, .. }
            | Input::Hedged { id, .. }
            | Input::Membership { id, .. }
            | Input::Replied { id, .. }
            | Input::NoHelper { id }
            | Input::Validated { id, .. }
            | Input::Batched { id, .. }
            | Input::Timer { id, .. }
            | Input::Origin { id, .. }
            | Input::Published { id, .. } => Some(*id),
        }
    }
}

/// A ranking with the facts the walk took from it.
#[derive(Clone, Debug)]
struct Held<R> {
    version: u64,
    owners: List<u32, MAX_HANDOFF>,
    rank: Option<usize>,
    value: R,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Pending {
    Rank(Ranking),
    Hints,
    Hedge,
    Refresh,
    Send,
    Validate,
    Batch,
    Sleep,
    Origin,
    Publish,
}

/// Whether `input` is the kind of answer `kind` waits for.
fn answers<R, M, A>(kind: Pending, input: &Input<R, M, A>) -> bool {
    matches!(
        (kind, input),
        (Pending::Rank(_), Input::Ranked { .. })
            | (Pending::Hints, Input::Hints { .. })
            | (Pending::Hedge, Input::Hedged { .. })
            | (Pending::Refresh, Input::Membership { .. })
            | (
                Pending::Send,
                Input::Replied { .. } | Input::NoHelper { .. }
            )
            | (Pending::Validate, Input::Validated { .. })
            | (Pending::Batch, Input::Batched { .. })
            | (Pending::Sleep, Input::Timer { .. })
            | (Pending::Origin, Input::Origin { .. })
            | (Pending::Publish, Input::Published { .. })
    )
}

/// The acquisition machine. See the module docs.
pub struct PageAcquisition<R, M, A> {
    fill: Fill,
    scope: Instant,
    initial: Option<R>,
    current: Option<Held<R>>,
    fallback: Option<Held<R>>,
    latest: Option<M>,
    previous: Option<M>,
    stale_previous: Option<M>,
    answer: Option<A>,
    dest: Option<u32>,
    sent: Instant,
    phase: Phase,
    authority: Option<Authority>,
    /// The interpreter holds an acquired page.
    copy: bool,
    walk_only: bool,
    pending: Option<(EffectId, Pending)>,
    started: bool,
    done: Option<Done>,
}

impl<R: Clone, M, A> PageAcquisition<R, M, A> {
    /// An acquisition starting from the caller's ranking `initial`.
    pub fn new(config: FillConfig, initial: R, now: Instant) -> Self {
        Self {
            fill: Fill::new(config),
            scope: config.scope,
            initial: Some(initial),
            current: None,
            fallback: None,
            latest: None,
            previous: None,
            stale_previous: None,
            answer: None,
            dest: None,
            sent: now,
            phase: Phase::Walk,
            authority: None,
            copy: false,
            walk_only: config.walk_only,
            pending: None,
            started: false,
            done: None,
        }
    }

    pub fn phase(&self) -> Phase {
        self.phase
    }

    /// The walked ranking.
    pub fn current(&self) -> Option<&R> {
        self.current.as_ref().map(|held| &held.value)
    }

    /// Version - 1 while recent and listing this node, after a `Previous` ranking.
    pub fn previous(&self) -> Option<&M> {
        self.previous.as_ref()
    }

    pub fn authority(&self) -> Option<&Authority> {
        self.authority.as_ref()
    }

    /// The effect the machine waits for.
    pub fn pending(&self) -> Option<EffectId> {
        self.pending.map(|(id, _)| id)
    }

    pub fn done(&self) -> Option<Done> {
        self.done
    }

    /// The walk state, read-only (diagnostics and handoff target kinds).
    pub fn fill(&self) -> &Fill {
        &self.fill
    }

    /// The budget the outstanding [`Effect::Send`] spends.
    pub fn request_budget<'a>(
        &'a mut self,
        budget: &'a mut AcquisitionBudget,
    ) -> &'a mut AcquisitionBudget {
        self.fill.request_budget(budget)
    }

    /// Feed one input; effects are appended to `out`. An input for an effect the machine is not
    /// waiting for is refused without a state change.
    pub fn on(
        &mut self,
        input: Input<R, M, A>,
        budget: &mut AcquisitionBudget,
        now: Instant,
        ids: &mut EffectIds,
        out: &mut Vec<Effect<R, M, A>>,
    ) -> Result<(), Stale> {
        if self.done.is_some() {
            return Err(Stale(input.id().unwrap_or_default()));
        }
        let kind = match (input.id(), self.pending) {
            (Some(id), Some((pending, kind))) if id == pending => Some(kind),
            (Some(id), _) => return Err(Stale(id)),
            (None, _) => None,
        };
        if let Some(kind) = kind
            && !answers(kind, &input)
        {
            return Err(Stale(input.id().unwrap_or_default()));
        }
        if matches!(input, Input::Start { .. }) {
            if self.started {
                return Err(Stale(EffectId::default()));
            }
            self.started = true;
        }
        self.pending = None;
        let hints;
        let event = match input {
            Input::Start { local_hit } => Event::Start { local_hit },
            Input::Cancel(error) => {
                self.pending = None;
                Event::Abort(error)
            }
            Input::Ranked { result, .. } => {
                let Some(Pending::Rank(ranking)) = kind else {
                    unreachable!("checked by answers()");
                };
                Event::Ranked(self.ranked(ranking, result))
            }
            Input::Hints {
                hints: h, rank0, ..
            } => {
                hints = h;
                Event::Hints {
                    hints: &hints,
                    rank0,
                }
            }
            Input::Hedged { end, copy, .. } => {
                self.copy = copy;
                Event::Hedge(end)
            }
            Input::Membership {
                latest,
                recent_previous,
                ..
            } => match latest {
                Err(error) => Event::Abort(error),
                Ok(latest) => {
                    let version = latest.as_ref().map(|(v, _)| *v);
                    self.latest = latest.map(|(_, m)| m);
                    let previous_recent_member = recent_previous.is_some();
                    self.stale_previous = recent_previous;
                    Event::Membership {
                        latest: version,
                        previous_recent_member,
                    }
                }
            },
            Input::Replied { reply, answer, .. } => {
                self.answer = answer;
                Event::Reply(reply)
            }
            Input::NoHelper { .. } => Event::NoHelper,
            Input::Validated { result, .. } => {
                if matches!(result, Ok(None)) {
                    self.copy = true;
                }
                Event::Validated(result)
            }
            Input::Batched { result, .. } => match result {
                Ok(copy) => {
                    self.copy |= copy;
                    Event::BatchDone { copy }
                }
                Err(error) => Event::Abort(error),
            },
            Input::Timer { result, .. } => match result {
                Ok(()) => Event::Timer,
                Err(error) => {
                    self.finish(Err(error), Evidence::default(), budget, now, out);
                    return Ok(());
                }
            },
            Input::Origin { result, .. } => {
                let authority = self.authority.expect("origin authority");
                match result {
                    Ok(()) => {
                        self.copy = true;
                        out.push(Effect::Report(Report::OriginFetched { authority }));
                    }
                    Err(error) if error != Error::VersionUnavailable => {
                        out.push(Effect::Report(Report::OriginFailed { authority, error }));
                    }
                    Err(_) => {}
                }
                Event::Origin(result)
            }
            Input::Published { result, .. } => Event::Published(result),
        };
        self.advance(event, budget, now, ids, out);
        Ok(())
    }

    fn ranked(
        &mut self,
        ranking: Ranking,
        result: Result<RankAnswer<R, M>, Error>,
    ) -> Result<Ranked, Error> {
        let answer = result?;
        let ranked = answer.ranked;
        match ranking {
            Ranking::Current | Ranking::Fallback => {
                if ranked.owners.is_empty() {
                    return Err(Error::IncompatibleMembership);
                }
                let value = answer.ranking.ok_or(Error::Internal)?;
                let held = Held {
                    version: ranked.version,
                    owners: ranked.owners,
                    rank: ranked.rank,
                    value,
                };
                if ranking == Ranking::Current {
                    self.current = Some(held);
                    self.previous = None;
                } else {
                    self.fallback = Some(held);
                }
            }
            Ranking::Previous => self.previous = answer.recent,
            Ranking::Authority => {}
        }
        Ok(ranked)
    }

    fn advance(
        &mut self,
        mut event: Event<'_>,
        budget: &mut AcquisitionBudget,
        now: Instant,
        ids: &mut EffectIds,
        out: &mut Vec<Effect<R, M, A>>,
    ) {
        loop {
            let (action, evidence) = self.fill.on(event, budget, now);
            // The walk took the previous membership's ranking.
            if self
                .fallback
                .as_ref()
                .is_some_and(|held| held.version == self.fill.version)
            {
                self.current = self.fallback.take();
            }
            if evidence.notes & Notes::REMAINING != 0 {
                self.phase = Phase::Remaining;
            }
            if self.phase != Phase::Remaining
                && evidence != Evidence::default()
                && let Some(held) = &self.current
            {
                out.push(Effect::Report(Report::Evidence {
                    evidence,
                    dest: self.dest,
                    version: held.version,
                    sent: self.sent,
                }));
            }
            let next = match action {
                Action::Rank { ranking, version } => match self.rank_basis(ranking, version) {
                    Ok(Some(basis)) => {
                        let id = self.expect(ids, Pending::Rank(ranking));
                        out.push(Effect::Rank { id, ranking, basis });
                        return;
                    }
                    // The authority ranking needs no IO, but not past the read's deadline.
                    Ok(None) if now >= budget.deadline() => {
                        Event::Ranked(Err(Error::DeadlineExceeded))
                    }
                    Ok(None) => Event::Ranked(Ok(self.authority_ranking())),
                    Err(error) => Event::Ranked(Err(error)),
                },
                Action::Hints => {
                    let version = self.fill.version;
                    let id = self.expect(ids, Pending::Hints);
                    out.push(Effect::Hints { id, version });
                    return;
                }
                Action::Hedge {
                    eligible,
                    first,
                    second,
                } => {
                    let id = self.expect(ids, Pending::Hedge);
                    out.push(Effect::Hedge {
                        id,
                        eligible,
                        first,
                        second,
                    });
                    return;
                }
                Action::Refresh => {
                    let version = self.walked_version();
                    let id = self.expect(ids, Pending::Refresh);
                    out.push(Effect::Refresh { id, version });
                    return;
                }
                Action::Probe(probe) => {
                    let dest = self.fill.dest(&probe);
                    self.dest = dest;
                    self.sent = now;
                    let version = self.fill.version;
                    let id = self.expect(ids, Pending::Send);
                    out.push(Effect::Send {
                        id,
                        probe,
                        dest,
                        version,
                    });
                    return;
                }
                Action::Validate => match self.answer.take() {
                    Some(answer) => {
                        let id = self.expect(ids, Pending::Validate);
                        out.push(Effect::Validate { id, answer });
                        return;
                    }
                    // An answer reported without its payload cannot be used.
                    None => Event::Validated(Err(Error::Internal)),
                },
                Action::Batch {
                    first,
                    count,
                    remaining_opportunities,
                } => {
                    let targets = self.fill.handoff[first..first + count].to_vec();
                    let version = self.fill.version;
                    let id = self.expect(ids, Pending::Batch);
                    out.push(Effect::Batch {
                        id,
                        version,
                        targets,
                        remaining_opportunities,
                    });
                    return;
                }
                Action::Wait(wait) => {
                    let id = self.expect(ids, Pending::Sleep);
                    out.push(Effect::Sleep {
                        id,
                        until: now + wait,
                    });
                    return;
                }
                Action::Origin { rank } => {
                    let held = self.current.as_ref().expect("a ranked walk");
                    let authority = Authority {
                        version: held.version,
                        owners: held.owners,
                        rank,
                    };
                    self.phase = Phase::Origin;
                    self.authority = Some(authority);
                    // A walk-only acquisition hands the authority to its caller, who funds the
                    // origin attempt.
                    if !self.walk_only
                        && let Err(error) = budget.begin_attempt(now, self.scope)
                    {
                        self.finish(Err(error), evidence, budget, now, out);
                        return;
                    }
                    let id = self.expect(ids, Pending::Origin);
                    out.push(Effect::Origin { id, authority });
                    return;
                }
                Action::Publish(source) => {
                    if !self.copy {
                        Event::Published(Err(Error::Internal))
                    } else {
                        let id = self.expect(ids, Pending::Publish);
                        out.push(Effect::Publish { id, source });
                        return;
                    }
                }
                Action::Done(result) => {
                    let result = match result {
                        Ok(Source::Local) => Ok(Source::Local),
                        Ok(_) if !self.copy => Err(Error::Internal),
                        result => result,
                    };
                    self.finish(result, evidence, budget, now, out);
                    return;
                }
                // One effect is outstanding at a time, so the walk never idles here.
                Action::Idle => Event::Abort(Error::Internal),
            };
            event = next;
        }
    }

    fn walked_version(&self) -> u64 {
        self.current
            .as_ref()
            .map_or(self.fill.version, |h| h.version)
    }

    fn expect(&mut self, ids: &mut EffectIds, kind: Pending) -> EffectId {
        let id = ids.allocate();
        self.pending = Some((id, kind));
        id
    }

    /// What to rank for `ranking`; `Ok(None)` when the machine answers itself.
    fn rank_basis(
        &mut self,
        ranking: Ranking,
        version: Option<u64>,
    ) -> Result<Option<RankBasis<R, M>>, Error> {
        match ranking {
            Ranking::Current => match (version, self.initial.take()) {
                (None, Some(initial)) => Ok(Some(RankBasis::Verify(initial))),
                _ => self
                    .latest
                    .take()
                    .map(|m| Some(RankBasis::Latest(m)))
                    .ok_or(Error::Unavailable),
            },
            Ranking::Previous => Ok(Some(RankBasis::Previous {
                version: version.ok_or(Error::Internal)?,
            })),
            Ranking::Fallback => self
                .stale_previous
                .take()
                .map(|m| Some(RankBasis::Fallback(m)))
                .ok_or(Error::Unavailable),
            Ranking::Authority => Ok(None),
        }
    }

    /// The walked ranking again: only a ranked owner fetches.
    fn authority_ranking(&self) -> Ranked {
        match &self.current {
            Some(held) => Ranked::new(held.version, &held.owners, held.rank),
            None => Ranked::new(0, &[], None),
        }
    }

    fn finish(
        &mut self,
        result: Result<Source, Error>,
        evidence: Evidence,
        budget: &AcquisitionBudget,
        now: Instant,
        out: &mut Vec<Effect<R, M, A>>,
    ) {
        let exhausted = evidence.notes & Notes::EXHAUSTED != 0;
        let result = match result {
            // An exhausted walk or a walk-phase version miss after the deadline reports the
            // deadline, not the walk's own error.
            Err(_)
                if (evidence.notes & (Notes::EXHAUSTED | Notes::EXHAUSTED_FUNDING) != 0
                    || (self.phase == Phase::Walk && result == Err(Error::VersionUnavailable)))
                    && now >= budget.deadline() =>
            {
                Err(Error::DeadlineExceeded)
            }
            result => result,
        };
        let done = Done {
            result,
            phase: self.phase,
            exhausted,
            funding: self.fill.funding,
            remaining: self.fill.remaining_end,
            notes: evidence.notes,
        };
        self.pending = None;
        self.done = Some(done);
        out.push(Effect::Done(done));
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests;
