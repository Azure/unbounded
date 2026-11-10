//! The authority shell machine: the IO-level decisions of one primary term.
//!
//! The shell owns the term's writer fencing ([`Writer`]) and Lease freshness
//! ([`Freshness`]). It takes events (times, Lease renewals, state reads, PUT
//! results) and returns [`Effect`]s (read the state Secret, plan, PUT, settle,
//! abort) for an interpreter (`primary.rs`) to perform. It never reads a
//! clock: every event carries the time it refers to.
//!
//! One commit runs as:
//!
//! 1. [`Shell::begin`] returns [`Effect::ReadState`].
//! 2. The interpreter reads the Secret (and, when [`Shell::needs_live_uid`],
//!    the primary Lease UID after it) and feeds [`Event::StateRead`]. A
//!    record behind the process high water fails closed
//!    ([`StateError::Rollback`] or [`StateError::Wiped`]); a newer term fences
//!    this writer for good. Otherwise the shell asks for [`Effect::Plan`].
//! 3. The interpreter plans the pass and feeds [`Event::Planned`]. An
//!    unchanged record settles without writing. A changed record must be
//!    monotonic over the one read (else [`StateError::Rollback`], fail closed)
//!    and the Lease must be fresh and the writer unfenced (else
//!    [`StateError::StaleTerm`]); then the shell asks for [`Effect::Put`].
//! 4. Right before the PUT the interpreter calls [`Shell::recheck`], which
//!    refuses a write once the Lease went stale while planning or encoding.
//! 5. The PUT result is fed as [`Event::PutDone`]. Success settles the new
//!    record. A conflict re-reads and re-plans, at most
//!    [`MAX_WRITE_ATTEMPTS`] reads per pass; other failures end the pass.
//!
//! [`webhook_trust`] orders webhook trust around endpoint changes: trust is
//! added before an endpoint that needs it, and removed only after the
//! endpoint left and a grace passed.

use super::state::{self, Freshness, StateError, StateRecord, Term, Writer, lease_authority};
use std::collections::BTreeMap;
use std::time::{Duration, Instant};

/// State reads (and so PUT attempts) per pass. A conflict re-reads the
/// Secret and re-plans until this many reads were made.
pub const MAX_WRITE_ATTEMPTS: u32 = 3;

/// Outcome of a state PUT.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PutResult {
    Ok,
    /// The resourceVersion moved: someone else wrote. Re-read.
    Conflict,
    /// Any other failure. The pass ends without a commit.
    Failed,
}

/// An event fed to the shell.
#[derive(Debug)]
pub enum Event<'a> {
    /// The state Secret read started at `started` (before the GET) returned
    /// `current`. `high_water` is the newest record this process accepted;
    /// `live_lease_uid` is the primary Lease UID read after the Secret, when
    /// [`Shell::needs_live_uid`] asked for it.
    StateRead {
        started: Instant,
        current: Result<Option<StateRecord>, StateError>,
        high_water: Option<&'a StateRecord>,
        live_lease_uid: Option<&'a str>,
    },
    /// The pass plan at `now`: `None` when nothing changed.
    Planned {
        now: Instant,
        next: Option<StateRecord>,
    },
    /// The PUT that started at `started` finished.
    PutDone { started: Instant, result: PutResult },
}

/// IO the interpreter performs next.
#[derive(Debug, Eq, PartialEq)]
pub enum Effect {
    /// GET the state Secret, then feed [`Event::StateRead`].
    ReadState,
    /// Plan the pass on [`Shell::current`], then feed [`Event::Planned`].
    Plan,
    /// PUT this record. Call [`Shell::recheck`] right before writing.
    Put(Box<StateRecord>),
    /// Install and publish `record` (the committed one, if any) confirmed at
    /// `confirmed`. `committed` is true when this pass wrote it.
    Settle {
        record: Option<Box<StateRecord>>,
        confirmed: Instant,
        committed: bool,
    },
    /// End the pass without a commit and without settling.
    Done,
    /// End the pass with an error. Fencing and durable-state errors mean the
    /// term must stop writing and serving (see [`StateError::fails_closed`]).
    Abort(StateError),
}

impl StateError {
    /// Whether this error ends the term and stops serving.
    pub fn fails_closed(self) -> bool {
        matches!(
            self,
            StateError::StaleTerm
                | StateError::LeaseReplaced
                | StateError::Wiped
                | StateError::Invalid
                | StateError::Rollback
                | StateError::ClusterMismatch
        )
    }
}

#[derive(Debug)]
enum Phase {
    Idle,
    Reading {
        attempts: u32,
    },
    Planning {
        attempts: u32,
        current: Option<StateRecord>,
        read_at: Instant,
    },
    Writing {
        attempts: u32,
        next: StateRecord,
    },
}

/// The shell state of one primary term.
#[derive(Debug)]
pub struct Shell {
    writer: Writer,
    lease: Freshness,
    phase: Phase,
}

impl Shell {
    /// A term claimed with a Lease write that started at `claimed`.
    pub fn new(term: Term, claimed: Instant) -> Self {
        Self {
            writer: Writer::new(term),
            lease: lease_authority(claimed),
            phase: Phase::Idle,
        }
    }

    pub fn term(&self) -> &Term {
        self.writer.term()
    }

    pub fn writer(&self) -> &Writer {
        &self.writer
    }

    pub fn lease(&self) -> Freshness {
        self.lease
    }

    pub fn may_write(&self) -> bool {
        self.writer.may_write()
    }

    /// A Lease renewal that started at `started` succeeded.
    pub fn renewed(&mut self, started: Instant) {
        self.lease = lease_authority(started);
    }

    /// When the next renewal is due after one that started at `started`.
    pub fn next_renewal(started: Instant) -> Instant {
        started + crate::limits::LEASE_RENEW
    }

    /// Whether this term may still act (serve, issue, write) at `now`.
    pub fn fresh(&self, now: Instant) -> bool {
        self.lease.fresh(now) && self.writer.may_write()
    }

    /// Whether the interpreter must read the live primary Lease UID after
    /// reading `current`: only when another Lease stamped it.
    pub fn needs_live_uid(&self, current: Option<&StateRecord>) -> bool {
        current.is_some_and(|c| c.term.lease_uid != self.writer.term().lease_uid)
    }

    /// The record of the last state read, while planning.
    pub fn current(&self) -> Option<&StateRecord> {
        match &self.phase {
            Phase::Planning { current, .. } => current.as_ref(),
            _ => None,
        }
    }

    /// Start a pass.
    pub fn begin(&mut self) -> Effect {
        self.phase = Phase::Reading { attempts: 0 };
        Effect::ReadState
    }

    /// Refuse a PUT that would start at `now` once the Lease went stale or
    /// the writer was fenced. Call right before the write.
    pub fn recheck(&self, now: Instant) -> Result<(), StateError> {
        if matches!(self.phase, Phase::Writing { .. }) && self.fresh(now) {
            Ok(())
        } else {
            Err(StateError::StaleTerm)
        }
    }

    /// Feed one event and get the next effect.
    pub fn on(&mut self, event: Event<'_>) -> Effect {
        let phase = std::mem::replace(&mut self.phase, Phase::Idle);
        match (phase, event) {
            (
                Phase::Reading { attempts },
                Event::StateRead {
                    started,
                    current,
                    high_water,
                    live_lease_uid,
                },
            ) => {
                let current = match current {
                    Ok(c) => c,
                    Err(e) => return Effect::Abort(e),
                };
                // Never build on state older than what this process accepted.
                if let Some(hw) = high_water {
                    match &current {
                        None => return Effect::Abort(StateError::Wiped),
                        Some(r) => {
                            if let Err(e) = state::check_monotonic(hw, r) {
                                return Effect::Abort(e);
                            }
                        }
                    }
                }
                if let Err(e) = self.writer.observe(current.as_ref(), live_lease_uid) {
                    return Effect::Abort(e);
                }
                self.phase = Phase::Planning {
                    attempts: attempts + 1,
                    current,
                    read_at: started,
                };
                Effect::Plan
            }
            (
                Phase::Planning {
                    attempts,
                    current,
                    read_at,
                },
                Event::Planned { now, next },
            ) => {
                let Some(next) = next else {
                    return Effect::Settle {
                        record: current.map(Box::new),
                        confirmed: read_at,
                        committed: false,
                    };
                };
                if let Some(c) = &current
                    && let Err(e) = state::check_monotonic(c, &next)
                {
                    return Effect::Abort(e);
                }
                if !self.fresh(now) {
                    return Effect::Abort(StateError::StaleTerm);
                }
                self.phase = Phase::Writing {
                    attempts,
                    next: next.clone(),
                };
                Effect::Put(Box::new(next))
            }
            (Phase::Writing { attempts, next }, Event::PutDone { started, result }) => match result
            {
                PutResult::Ok => Effect::Settle {
                    record: Some(Box::new(next)),
                    confirmed: started,
                    committed: true,
                },
                PutResult::Conflict if attempts < MAX_WRITE_ATTEMPTS => {
                    self.phase = Phase::Reading { attempts };
                    Effect::ReadState
                }
                PutResult::Conflict | PutResult::Failed => Effect::Done,
            },
            // An event that does not belong to the current phase is a bug in
            // the interpreter. Fail closed rather than guess.
            _ => Effect::Abort(StateError::Invalid),
        }
    }
}

/// Webhook trust changes around one endpoint update.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct TrustPlan {
    /// Trust to install before the endpoints change: the current trust plus
    /// every desired CA, in current order.
    pub before: Vec<Vec<u8>>,
    /// CAs the endpoints are about to use.
    pub desired: Vec<Vec<u8>>,
}

/// Plan webhook trust around an endpoint change. Pure: nothing is retired
/// until [`TrustPlan::retire_after`] is called once the endpoints moved.
pub fn webhook_trust(current: &[Vec<u8>], desired: &[Vec<u8>]) -> TrustPlan {
    let mut before = current.to_vec();
    for d in desired {
        if !before.contains(d) {
            before.push(d.clone());
        }
    }
    TrustPlan {
        before,
        desired: desired.to_vec(),
    }
}

impl TrustPlan {
    /// Commit the endpoint change at `now` and return the trust to keep
    /// after it: desired CAs and CAs still within their grace. Call only once
    /// the endpoints are known to match `desired`, so a CA's grace starts
    /// when it actually left the endpoints. `retired` maps CAs that left to
    /// when they left; at most `max_retired` are kept (the oldest go first).
    pub fn retire_after(
        &self,
        retired: &mut BTreeMap<Vec<u8>, Instant>,
        now: Instant,
        grace: Duration,
        max_retired: usize,
    ) -> Vec<Vec<u8>> {
        let (before, desired) = (&self.before, &self.desired);
        retired.retain(|ca, _| !desired.contains(ca) && before.contains(ca));
        for ca in before.iter().filter(|c| !desired.contains(c)) {
            retired.entry(ca.clone()).or_insert(now);
        }
        retired.retain(|_, at| now.saturating_duration_since(*at) < grace);
        while retired.len() > max_retired {
            let oldest = retired
                .iter()
                .min_by_key(|(_, at)| **at)
                .map(|(k, _)| k.clone());
            if let Some(k) = oldest {
                retired.remove(&k);
            }
        }
        // Keep current order so an unchanged set is not rewritten.
        before
            .iter()
            .filter(|c| desired.contains(c) || retired.contains_key(*c))
            .cloned()
            .collect()
    }
}

/// Seat replica acceptance: a record is accepted only when it does not go
/// behind the high water, and a copied image only when it matches the
/// accepted record's hashes (see [`super::publication::verify_image`]).
pub fn accept_record(
    high_water: Option<&StateRecord>,
    record: &StateRecord,
) -> Result<(), StateError> {
    super::seat::admit(high_water, record)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::limits;

    fn term(t: u64, holder: &str) -> Term {
        Term {
            lease_uid: "lease-1".into(),
            transitions: t,
            holder: holder.into(),
        }
    }

    fn record(seq: u64, term: Term) -> StateRecord {
        StateRecord {
            version: state::VERSION,
            cluster: "c".into(),
            term,
            writer_pod_uid: "p".into(),
            sequence: seq,
            membership_version: seq,
            content_hash: if seq == 0 {
                String::new()
            } else {
                format!("{seq:064x}")
            },
            membership_hash: if seq == 0 {
                String::new()
            } else {
                format!("{seq:064x}")
            },
            ..Default::default()
        }
    }

    fn read<'a>(
        started: Instant,
        current: Option<StateRecord>,
        high_water: Option<&'a StateRecord>,
    ) -> Event<'a> {
        Event::StateRead {
            started,
            current: Ok(current),
            high_water,
            live_lease_uid: None,
        }
    }

    /// Run one pass that writes `next` over `current`, returning the PUT.
    fn to_put(s: &mut Shell, t: Instant, current: StateRecord, next: StateRecord) -> Effect {
        assert_eq!(s.begin(), Effect::ReadState);
        assert_eq!(s.on(read(t, Some(current), None)), Effect::Plan);
        s.on(Event::Planned {
            now: t,
            next: Some(next),
        })
    }

    #[test]
    fn commit_settles_the_written_record() {
        let t = Instant::now();
        let me = term(1, "a/1");
        let mut s = Shell::new(me.clone(), t);
        let next = record(2, me.clone());
        assert_eq!(
            to_put(&mut s, t, record(1, me), next.clone()),
            Effect::Put(Box::new(next.clone()))
        );
        s.recheck(t).unwrap();
        let done = t + Duration::from_millis(5);
        assert_eq!(
            s.on(Event::PutDone {
                started: done,
                result: PutResult::Ok
            }),
            Effect::Settle {
                record: Some(Box::new(next)),
                confirmed: done,
                committed: true
            }
        );
    }

    #[test]
    fn unchanged_pass_settles_without_writing() {
        let t = Instant::now();
        let me = term(1, "a/1");
        let mut s = Shell::new(me.clone(), t);
        s.begin();
        assert_eq!(
            s.on(read(t, Some(record(1, me.clone())), None)),
            Effect::Plan
        );
        assert_eq!(s.current(), Some(&record(1, me.clone())));
        assert_eq!(
            s.on(Event::Planned { now: t, next: None }),
            Effect::Settle {
                record: Some(Box::new(record(1, me))),
                confirmed: t,
                committed: false
            }
        );
        // A recheck outside a write refuses.
        assert_eq!(s.recheck(t), Err(StateError::StaleTerm));
    }

    #[test]
    fn at_most_one_writer_per_term() {
        let t = Instant::now();
        let a = term(1, "a/1");
        let b = term(1, "b/2");
        let mut sa = Shell::new(a.clone(), t);
        let mut sb = Shell::new(b.clone(), t);
        // A writes first.
        let base = record(1, a.clone());
        assert!(matches!(
            to_put(&mut sa, t, base.clone(), record(2, a.clone())),
            Effect::Put(_)
        ));
        // B, same transitions but another holder, reads A's record: fenced.
        sb.begin();
        assert_eq!(
            sb.on(read(t, Some(record(2, a.clone())), None)),
            Effect::Abort(StateError::StaleTerm)
        );
        assert!(!sb.may_write());
        // Fenced for good, even if an old record shows up again.
        sb.begin();
        assert_eq!(
            sb.on(read(t, Some(base), None)),
            Effect::Abort(StateError::StaleTerm)
        );
        assert!(!sb.fresh(t));
    }

    #[test]
    fn freshness_recheck_blocks_a_stale_put() {
        let t = Instant::now();
        let me = term(1, "a/1");
        let mut s = Shell::new(me.clone(), t);
        assert!(matches!(
            to_put(&mut s, t, record(1, me.clone()), record(2, me.clone())),
            Effect::Put(_)
        ));
        let stale = t + limits::LEASE_FRESHNESS;
        assert_eq!(s.recheck(stale), Err(StateError::StaleTerm));
        // A renewal before the write restores freshness.
        s.renewed(stale);
        s.recheck(stale).unwrap();
        // Planning on a stale Lease never asks for a PUT.
        let mut late = Shell::new(me.clone(), t);
        late.begin();
        late.on(read(t, Some(record(1, me.clone())), None));
        assert_eq!(
            late.on(Event::Planned {
                now: stale,
                next: Some(record(2, me))
            }),
            Effect::Abort(StateError::StaleTerm)
        );
    }

    #[test]
    fn conflicts_reread_up_to_the_limit() {
        let t = Instant::now();
        let me = term(1, "a/1");
        let mut s = Shell::new(me.clone(), t);
        assert!(matches!(
            to_put(&mut s, t, record(1, me.clone()), record(2, me.clone())),
            Effect::Put(_)
        ));
        let conflict = Event::PutDone {
            started: t,
            result: PutResult::Conflict,
        };
        for _ in 1..MAX_WRITE_ATTEMPTS {
            assert_eq!(s.on(conflict_like(&conflict)), Effect::ReadState);
            assert_eq!(
                s.on(read(t, Some(record(1, me.clone())), None)),
                Effect::Plan
            );
            assert!(matches!(
                s.on(Event::Planned {
                    now: t,
                    next: Some(record(2, me.clone()))
                }),
                Effect::Put(_)
            ));
        }
        assert_eq!(s.on(conflict), Effect::Done);
        // Other failures end the pass at once.
        assert!(matches!(
            to_put(&mut s, t, record(1, me.clone()), record(2, me)),
            Effect::Put(_)
        ));
        assert_eq!(
            s.on(Event::PutDone {
                started: t,
                result: PutResult::Failed
            }),
            Effect::Done
        );
    }

    fn conflict_like(e: &Event<'_>) -> Event<'static> {
        match e {
            Event::PutDone { started, result } => Event::PutDone {
                started: *started,
                result: *result,
            },
            _ => unreachable!(),
        }
    }

    #[test]
    fn rollback_fails_closed() {
        let t = Instant::now();
        let me = term(1, "a/1");
        let hw = record(5, me.clone());
        let mut s = Shell::new(me.clone(), t);
        // A read behind the high water.
        s.begin();
        let effect = s.on(read(t, Some(record(4, me.clone())), Some(&hw)));
        assert_eq!(effect, Effect::Abort(StateError::Rollback));
        assert!(StateError::Rollback.fails_closed());
        // A wiped record after one was accepted.
        s.begin();
        assert_eq!(
            s.on(read(t, None, Some(&hw))),
            Effect::Abort(StateError::Wiped)
        );
        // Read errors abort as they are.
        s.begin();
        assert_eq!(
            s.on(Event::StateRead {
                started: t,
                current: Err(StateError::Invalid),
                high_water: None,
                live_lease_uid: None
            }),
            Effect::Abort(StateError::Invalid)
        );
        // Out-of-order events fail closed.
        assert_eq!(
            s.on(Event::Planned { now: t, next: None }),
            Effect::Abort(StateError::Invalid)
        );
    }

    #[test]
    fn counters_must_be_monotonic() {
        let t = Instant::now();
        let me = term(1, "a/1");
        let mut s = Shell::new(me.clone(), t);
        // Going backward.
        assert_eq!(
            to_put(&mut s, t, record(5, me.clone()), record(4, me.clone())),
            Effect::Abort(StateError::Rollback)
        );
        // Same sequence, other content.
        let mut same = record(5, me.clone());
        same.content_hash = "f".repeat(64);
        assert_eq!(
            to_put(&mut s, t, record(5, me.clone()), same),
            Effect::Abort(StateError::Rollback)
        );
        // Keyring generation going backward.
        let mut base = record(5, me.clone());
        base.keyring.generation = 3;
        assert_eq!(
            to_put(&mut s, t, base, record(6, me)),
            Effect::Abort(StateError::Rollback)
        );
    }

    #[test]
    fn live_uid_is_needed_only_for_foreign_records() {
        let t = Instant::now();
        let me = term(1, "a/1");
        let s = Shell::new(me.clone(), t);
        assert!(!s.needs_live_uid(None));
        assert!(!s.needs_live_uid(Some(&record(1, me.clone()))));
        let mut other = me;
        other.lease_uid = "lease-0".into();
        assert!(s.needs_live_uid(Some(&record(1, other))));
        assert_eq!(Shell::next_renewal(t), t + limits::LEASE_RENEW);
    }

    struct Planned {
        before: Vec<Vec<u8>>,
        after: Vec<Vec<u8>>,
    }

    fn plan(
        current: &[Vec<u8>],
        desired: &[Vec<u8>],
        retired: &mut BTreeMap<Vec<u8>, Instant>,
        now: Instant,
        grace: Duration,
        max: usize,
    ) -> Planned {
        let p = webhook_trust(current, desired);
        let after = p.retire_after(retired, now, grace, max);
        Planned {
            before: p.before,
            after,
        }
    }

    #[test]
    fn trust_is_added_before_endpoints_and_removed_after() {
        let t = Instant::now();
        let grace = Duration::from_secs(120);
        let (a, b) = (vec![1u8], vec![2u8]);
        let mut retired = BTreeMap::new();
        // B joins: trusted before its endpoint is published.
        let p = plan(
            std::slice::from_ref(&a),
            &[a.clone(), b.clone()],
            &mut retired,
            t,
            grace,
            4,
        );
        assert_eq!(p.before, vec![a.clone(), b.clone()]);
        assert_eq!(p.after, vec![a.clone(), b.clone()]);
        // A leaves: still trusted after the endpoint change, within grace.
        let p = plan(
            &p.after,
            std::slice::from_ref(&b),
            &mut retired,
            t,
            grace,
            4,
        );
        assert_eq!(p.before, vec![a.clone(), b.clone()]);
        assert_eq!(p.after, vec![a.clone(), b.clone()]);
        let p = plan(
            &p.after,
            std::slice::from_ref(&b),
            &mut retired,
            t + grace / 2,
            grace,
            4,
        );
        assert!(p.after.contains(&a));
        // Removed only after the grace.
        let p = plan(
            &p.after,
            std::slice::from_ref(&b),
            &mut retired,
            t + grace,
            grace,
            4,
        );
        assert_eq!(p.after, vec![b.clone()]);
        assert!(retired.is_empty());
        // Retired CAs are bounded, oldest first.
        let mut retired = BTreeMap::new();
        let all: Vec<_> = (0..4u8).map(|i| vec![i]).collect();
        for (i, ca) in all.iter().enumerate() {
            retired.insert(ca.clone(), t + Duration::from_secs(i as u64));
        }
        let p = plan(
            &all,
            &[],
            &mut retired,
            t + Duration::from_secs(5),
            grace,
            2,
        );
        assert_eq!(p.after, vec![vec![2u8], vec![3u8]]);
    }

    #[test]
    fn replica_acceptance_refuses_rollback() {
        let me = term(1, "a/1");
        let hw = record(5, me.clone());
        assert!(accept_record(Some(&hw), &record(6, me.clone())).is_ok());
        assert_eq!(
            accept_record(Some(&hw), &record(4, me.clone())),
            Err(StateError::Rollback)
        );
        assert!(accept_record(None, &record(1, me)).is_ok());
    }
}
