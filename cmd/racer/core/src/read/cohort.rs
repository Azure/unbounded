//! [`MetadataCohort`]: sans-IO metadata refresh cohorts on a [`FlightTable`].
//!
//! Mirrors the dataplane refresh cohort (`src/read/metadata.rs`, `resolve_inner` and `refresh`):
//!
//! - **Purpose**: callers coalesce only with the same object and [`RefreshPurpose`], so a
//!   metadata-only (HEAD) caller never joins or elects a body-fetching bootstrap leader, and a pin
//!   never shares a leader with a current observation.
//! - **Admission**: a settled cohort (a published result, a terminal failure, or exhausted
//!   elections) closes admission at once: a later caller starts a new cohort for the same key and
//!   never observes an earlier cohort's result. Readers already attached still observe it. At most
//!   `cohorts` keys admit at a time, and at most `cohorts * MAX_WAITERS` waiters are attached
//!   across open and settled cohorts.
//! - **Election**: the first eligible waiter leads, with at most `generations_per_flight`
//!   attempts per cohort; a leader needs a nonzero attempt budget.
//! - **Settlement** ([`refresh_settlement`]): a credential-specific origin rejection or a failure
//!   of the leader's own budget fails only the leader and lets another waiter retry; any other
//!   failure, or a success, finishes the cohort for every waiter.
//! - **Epoch check**: a leader records the clock epoch when it starts. A current or bootstrap
//!   observation is published as current freshness only if no clock discontinuity was observed
//!   since; otherwise only the immutable version is published.
//!
//! `M` is the observation every waiter shares. A driver whose failures do not fit [`Error`] may
//! carry them in `M` (for example `Result<Observation, DriverError>`) and settle with
//! [`MetadataCohort::finish`] and [`MetadataCohort::retry`] instead of [`MetadataCohort::settle`].

use super::flight::{FlightLimits, FlightTable, Joined, Reserve, Settled, Ticket, Token, Wait};
use super::{
    Error, FailureKind, MAX_REFRESH_ATTEMPTS, MAX_WAITERS, RefreshPurpose, Result, Settlement,
    refresh_settlement,
};
use crate::flow::coalesce::flight::state::Published;
use std::collections::HashMap;
use std::collections::hash_map::RandomState;
use std::hash::{BuildHasher, Hash};
use std::time::Instant;

/// Credential-free cohort identity: object plus purpose.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct CohortKey<Obj> {
    pub object: Obj,
    pub purpose: RefreshPurpose,
}

/// One attached waiter: its cohort (one admission of a key) and its waiter id.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct Member {
    pub cohort: u64,
    pub waiter: u64,
}

/// A leader's capability and the clock epoch it started in.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Started {
    pub cohort: u64,
    pub ticket: Ticket,
    pub epoch: u64,
}

/// What a polled cohort waiter should do next.
#[derive(Debug, Eq, PartialEq)]
pub enum Step<M> {
    /// Refresh with the caller's own budget, then settle the cohort.
    Lead(Started),
    Ready(M),
    Failed(Error),
    Pending,
}

/// Where a successful observation is published.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Publish {
    /// Publish current freshness (and the version).
    Current,
    /// Publish only the immutable version.
    Version,
}

/// How a leader's attempt settled.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Outcome {
    /// Every waiter observes the result; successes say where to publish.
    Finished(Option<Publish>),
    /// Only the leader failed; another waiter may lead.
    Retry,
}

fn complete<M>(metadata: M) -> Published<M, M> {
    Published::Complete(metadata)
}

/// Per-cohort bounds matching the dataplane: `MAX_WAITERS` per cohort and `MAX_REFRESH_ATTEMPTS`
/// elections per cohort. `entries` bounds every live cohort, open or settled.
pub fn cohort_limits(entries: usize) -> FlightLimits {
    FlightLimits {
        entries,
        waiters_per_flight: MAX_WAITERS,
        operations_per_flight: 1,
        generations_per_flight: MAX_REFRESH_ATTEMPTS as u64,
    }
}

type Key<Obj> = (CohortKey<Obj>, u64);

/// Worker-local metadata refresh cohorts. `M` is the shared observation and `R` each waiter's and
/// cohort's admission charge.
pub struct MetadataCohort<Obj, M, R = (), S = RandomState, T = Token> {
    table: FlightTable<Key<Obj>, M, M, M, R, S, T>,
    /// The cohort still admitting callers for each key.
    open: HashMap<CohortKey<Obj>, u64, S>,
    /// Maximum keys admitting at once.
    cohorts: usize,
    /// Waiters attached across open and settled cohorts.
    attached: usize,
    next_cohort: u64,
    epoch: u64,
}

impl<Obj, M, R, S, T> MetadataCohort<Obj, M, R, S, T>
where
    Obj: Eq + Hash + Clone,
    M: Clone,
    S: BuildHasher + Clone,
    T: Clone + PartialEq,
{
    /// At most `cohorts` keys admit at a time and at most `cohorts * MAX_WAITERS` waiters are
    /// attached.
    pub fn with_limits(cohorts: usize, now: Instant, hasher: S) -> Self {
        let entries = cohorts.saturating_mul(MAX_WAITERS);
        Self {
            table: FlightTable::new(cohort_limits(entries), complete, now, hasher.clone()),
            open: HashMap::with_hasher(hasher),
            cohorts,
            attached: 0,
            next_cohort: 0,
            epoch: 0,
        }
    }

    /// See [`FlightTable::retain_retired`].
    pub fn retain_retired(mut self) -> Self {
        self.table = self.table.retain_retired();
        self
    }

    /// See [`FlightTable::take_retired`].
    pub fn take_retired(&mut self) -> Vec<T> {
        self.table.take_retired()
    }

    pub fn table(&self) -> &FlightTable<Key<Obj>, M, M, M, R, S, T> {
        &self.table
    }

    /// Keys still admitting callers.
    pub fn open(&self) -> usize {
        self.open.len()
    }

    /// Waiters attached across open and settled cohorts.
    pub fn attached(&self) -> usize {
        self.attached
    }

    /// Waiters still attached to `member`'s cohort.
    pub fn waiters(&self, key: &CohortKey<Obj>, member: Member) -> usize {
        self.table.waiters(&(key.clone(), member.cohort))
    }

    /// Current clock epoch.
    pub fn epoch(&self) -> u64 {
        self.epoch
    }

    /// A clock discontinuity was observed: observations started before it may not publish
    /// current freshness.
    pub fn clock_jump(&mut self) {
        self.epoch += 1;
    }

    /// Close admission of `cohort` once it settled or was removed.
    fn sync(&mut self, key: &CohortKey<Obj>, cohort: u64) {
        if self.open.get(key) == Some(&cohort) {
            let k = (key.clone(), cohort);
            if !self.table.contains(&k) || self.table.settled(&k) {
                self.open.remove(key);
            }
        }
    }

    /// Join `key`'s open cohort, starting one if none admits. `Overloaded` past the cohort or
    /// waiter bounds; `FlightFull` when the open cohort holds `MAX_WAITERS` waiters.
    #[allow(clippy::too_many_arguments)] // Mirrors the flight table join.
    pub fn join(
        &mut self,
        key: &CohortKey<Obj>,
        now: Instant,
        deadline: Instant,
        attempts: u32,
        reserve: impl FnMut(Reserve) -> Result<R>,
        wakes: &mut Vec<T>,
    ) -> Result<Member> {
        if self.attached >= self.cohorts.saturating_mul(MAX_WAITERS) {
            return Err(Error::Overloaded);
        }
        if let Some(&cohort) = self.open.get(key) {
            self.table.refresh(&(key.clone(), cohort), now, wakes);
            self.sync(key, cohort);
        }
        let cohort = match self.open.get(key) {
            Some(&cohort) => cohort,
            None => {
                if self.open.len() >= self.cohorts {
                    return Err(Error::Overloaded);
                }
                let cohort = self.next_cohort;
                self.next_cohort += 1;
                self.open.insert(key.clone(), cohort);
                cohort
            }
        };
        let joined = self.table.join(
            &(key.clone(), cohort),
            now,
            deadline,
            attempts,
            true,
            reserve,
            wakes,
        );
        self.sync(key, cohort);
        match joined? {
            Joined::Waiter(waiter) => {
                self.attached += 1;
                Ok(Member { cohort, waiter })
            }
            // An open cohort has not settled.
            Joined::Settled(_) | Joined::Miss => Err(Error::Internal),
        }
    }

    /// Poll `member`. A leader needs a nonzero attempt budget.
    pub fn poll(
        &mut self,
        key: &CohortKey<Obj>,
        member: Member,
        now: Instant,
        attempts: u32,
        token: T,
        wakes: &mut Vec<T>,
    ) -> Result<Step<M>> {
        let k = (key.clone(), member.cohort);
        let polled = self
            .table
            .poll(&k, member.waiter, now, attempts, token, wakes);
        self.sync(key, member.cohort);
        Ok(match polled? {
            Wait::Lead => Step::Lead(Started {
                cohort: member.cohort,
                ticket: self
                    .table
                    .ticket(&k, member.waiter)
                    .ok_or(Error::Internal)?,
                epoch: self.epoch,
            }),
            Wait::Ready(Settled::Complete(m) | Settled::Partial(m)) => Step::Ready(m),
            Wait::Failed(error) => Step::Failed(error),
            Wait::Pending => Step::Pending,
        })
    }

    /// Finish the cohort for every waiter with `metadata`, closing admission. `Internal` for a
    /// stale leader.
    pub fn finish(
        &mut self,
        key: &CohortKey<Obj>,
        started: Started,
        metadata: M,
        wakes: &mut Vec<T>,
    ) -> Result<()> {
        self.settle_with(key, started, Ok(metadata), wakes)
    }

    /// Fail only the leader so another waiter may lead. A credential-specific rejection
    /// (`OriginRejected`, `OriginForbidden`) fails the leader with it; any other error revokes it.
    /// `Internal` for a stale leader.
    pub fn retry(
        &mut self,
        key: &CohortKey<Obj>,
        started: Started,
        error: Error,
        wakes: &mut Vec<T>,
    ) -> Result<()> {
        let k = (key.clone(), started.cohort);
        self.table.validate(&k, &started.ticket)?;
        let leader = started.ticket.waiter;
        match error {
            Error::OriginRejected => {
                self.table
                    .finish(&k, leader, Err(FailureKind::OriginRejected), wakes)?
            }
            Error::OriginForbidden => {
                self.table
                    .finish(&k, leader, Err(FailureKind::OriginForbidden), wakes)?
            }
            _ => self.table.cancel(&k, leader, wakes),
        }
        self.sync(key, started.cohort);
        Ok(())
    }

    fn settle_with(
        &mut self,
        key: &CohortKey<Obj>,
        started: Started,
        result: std::result::Result<M, FailureKind>,
        wakes: &mut Vec<T>,
    ) -> Result<()> {
        let k = (key.clone(), started.cohort);
        self.table.validate(&k, &started.ticket)?;
        let finished = self.table.finish(&k, started.ticket.waiter, result, wakes);
        self.sync(key, started.cohort);
        finished
    }

    /// Settle the leader's attempt by [`refresh_settlement`]. `remaining` is the leader's attempt
    /// budget after it. `Internal` for a stale leader.
    pub fn settle(
        &mut self,
        key: &CohortKey<Obj>,
        started: Started,
        result: Result<M>,
        remaining: u32,
        wakes: &mut Vec<T>,
    ) -> Result<Outcome> {
        match result {
            Ok(metadata) => {
                self.finish(key, started, metadata, wakes)?;
                let current = matches!(
                    key.purpose,
                    RefreshPurpose::Current | RefreshPurpose::Bootstrap
                ) && started.epoch == self.epoch;
                Ok(Outcome::Finished(Some(if current {
                    Publish::Current
                } else {
                    Publish::Version
                })))
            }
            Err(error) => match refresh_settlement(error, remaining) {
                Settlement::Retry => {
                    self.retry(key, started, error, wakes)?;
                    Ok(Outcome::Retry)
                }
                Settlement::Finish => {
                    self.settle_with(key, started, Err(FailureKind::Terminal(error)), wakes)?;
                    Ok(Outcome::Finished(None))
                }
            },
        }
    }

    /// Detach `member` (completion or caller drop), removing a quiescent cohort.
    pub fn detach(&mut self, key: &CohortKey<Obj>, member: Member, wakes: &mut Vec<T>) {
        let k = (key.clone(), member.cohort);
        if self.table.attached(&k, member.waiter) {
            self.attached -= 1;
        }
        self.table.detach(&k, member.waiter, wakes);
        self.sync(key, member.cohort);
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;
    use racer_wire::object::model::StrongEtag;
    use std::time::Duration;

    type Cohorts = MetadataCohort<u32, u64>;

    struct Fx {
        c: Cohorts,
        at: Instant,
        wakes: Vec<Token>,
    }

    fn fx() -> Fx {
        let at = Instant::now();
        Fx {
            c: MetadataCohort::with_limits(4, at, RandomState::new()),
            at,
            wakes: Vec::new(),
        }
    }

    fn key(purpose: RefreshPurpose) -> CohortKey<u32> {
        CohortKey { object: 1, purpose }
    }

    impl Fx {
        fn join(&mut self, k: &CohortKey<u32>, attempts: u32) -> Result<Member> {
            let deadline = self.at + Duration::from_secs(10);
            self.c
                .join(k, self.at, deadline, attempts, |_| Ok(()), &mut self.wakes)
        }
        fn waiter(&mut self, k: &CohortKey<u32>) -> Member {
            self.join(k, 2).expect("waiter")
        }
        fn poll(&mut self, k: &CohortKey<u32>, id: Member) -> Step<u64> {
            self.c
                .poll(k, id, self.at, 2, id.waiter, &mut self.wakes)
                .unwrap()
        }
        fn lead(&mut self, k: &CohortKey<u32>, id: Member) -> Started {
            match self.poll(k, id) {
                Step::Lead(started) => started,
                other => panic!("expected lead, got {other:?}"),
            }
        }
    }

    #[test]
    fn purposes_never_share_a_leader() {
        let mut f = fx();
        let current = key(RefreshPurpose::Current);
        let bootstrap = key(RefreshPurpose::Bootstrap);
        let pinned = key(RefreshPurpose::Pinned(StrongEtag::test_value("v")));
        let ids = [f.waiter(&current), f.waiter(&bootstrap), f.waiter(&pinned)];
        // Each purpose elects its own leader.
        f.lead(&current, ids[0]);
        f.lead(&bootstrap, ids[1]);
        f.lead(&pinned, ids[2]);
        assert_eq!(f.c.table().len(), 3);
        assert_eq!(f.c.open(), 3);
    }

    #[test]
    fn success_finishes_cohort_and_checks_epoch() {
        let mut f = fx();
        let k = key(RefreshPurpose::Current);
        let (a, b) = (f.waiter(&k), f.waiter(&k));
        let started = f.lead(&k, a);
        assert_eq!(f.poll(&k, b), Step::Pending);
        assert_eq!(
            f.c.settle(&k, started, Ok(5), 1, &mut f.wakes),
            Ok(Outcome::Finished(Some(Publish::Current)))
        );
        assert_eq!(f.poll(&k, b), Step::Ready(5));
        // Completion closed admission: a late caller starts a new cohort and leads it.
        let late = f.waiter(&k);
        assert_ne!(late.cohort, a.cohort);
        f.lead(&k, late);
        assert_eq!(f.poll(&k, b), Step::Ready(5));
        // A stale settle after completion is rejected.
        assert_eq!(
            f.c.settle(&k, started, Ok(6), 1, &mut f.wakes),
            Err(Error::Internal)
        );

        // A clock jump during the refresh demotes publication to the version only.
        let k = CohortKey {
            object: 2,
            purpose: RefreshPurpose::Bootstrap,
        };
        let c = f.waiter(&k);
        let started = f.lead(&k, c);
        f.c.clock_jump();
        assert_eq!(
            f.c.settle(&k, started, Ok(7), 1, &mut f.wakes),
            Ok(Outcome::Finished(Some(Publish::Version)))
        );

        // A pin always publishes only the version.
        let k = key(RefreshPurpose::Pinned(StrongEtag::test_value("v")));
        let d = f.waiter(&k);
        let started = f.lead(&k, d);
        assert_eq!(
            f.c.settle(&k, started, Ok(8), 1, &mut f.wakes),
            Ok(Outcome::Finished(Some(Publish::Version)))
        );
    }

    #[test]
    fn rejection_and_leader_budget_failures_reelect() {
        for error in [
            Error::OriginRejected,
            Error::OriginForbidden,
            Error::DeadlineExceeded,
            Error::Cancelled,
            Error::HopBudgetExhausted,
        ] {
            let mut f = fx();
            let k = key(RefreshPurpose::Current);
            let (a, b) = (f.waiter(&k), f.waiter(&k));
            let started = f.lead(&k, a);
            assert_eq!(
                f.c.settle(&k, started, Err(error), 1, &mut f.wakes),
                Ok(Outcome::Retry),
                "{error:?}"
            );
            assert!(matches!(f.poll(&k, a), Step::Failed(_)), "{error:?}");
            let next = f.lead(&k, b);
            assert_ne!(next.ticket, started.ticket);
        }
        // Unavailable retries only when the leader's own attempts are spent.
        let mut f = fx();
        let k = key(RefreshPurpose::Current);
        let (a, b) = (f.waiter(&k), f.waiter(&k));
        let started = f.lead(&k, a);
        assert_eq!(
            f.c.settle(&k, started, Err(Error::Unavailable), 0, &mut f.wakes),
            Ok(Outcome::Retry)
        );
        f.lead(&k, b);
    }

    #[test]
    fn other_failures_finish_every_waiter() {
        let mut f = fx();
        let k = key(RefreshPurpose::Current);
        let (a, b) = (f.waiter(&k), f.waiter(&k));
        let started = f.lead(&k, a);
        assert_eq!(
            f.c.settle(&k, started, Err(Error::Unavailable), 1, &mut f.wakes),
            Ok(Outcome::Finished(None))
        );
        assert_eq!(f.poll(&k, b), Step::Failed(Error::Unavailable));
        f.c.detach(&k, a, &mut f.wakes);
        f.c.detach(&k, b, &mut f.wakes);
        assert!(f.c.table().is_empty());
    }

    #[test]
    fn elections_are_capped_per_cohort() {
        let mut f = fx();
        let k = key(RefreshPurpose::Current);
        let ids: Vec<Member> = (0..=MAX_REFRESH_ATTEMPTS as u64)
            .map(|_| f.waiter(&k))
            .collect();
        for &id in &ids[..MAX_REFRESH_ATTEMPTS] {
            let started = f.lead(&k, id);
            f.c.settle(&k, started, Err(Error::OriginRejected), 1, &mut f.wakes)
                .unwrap();
        }
        let last = *ids.last().unwrap();
        assert_eq!(f.poll(&k, last), Step::Failed(Error::Unavailable));
    }

    #[test]
    fn zero_budget_caller_cannot_join_to_lead() {
        let mut f = fx();
        let k = key(RefreshPurpose::Current);
        assert_eq!(f.join(&k, 0), Err(Error::Unavailable));
    }

    #[test]
    fn failure_and_exhaustion_close_admission() {
        let mut f = fx();
        let k = key(RefreshPurpose::Current);
        let a = f.waiter(&k);
        let started = f.lead(&k, a);
        f.c.settle(&k, started, Err(Error::Unavailable), 1, &mut f.wakes)
            .unwrap();
        assert_eq!(f.c.open(), 0);
        let b = f.waiter(&k);
        assert_ne!(b.cohort, a.cohort);
        assert!(matches!(f.poll(&k, b), Step::Lead(_)));
        // The settled cohort still answers its attached reader.
        assert_eq!(f.poll(&k, a), Step::Failed(Error::Unavailable));
    }

    #[test]
    fn opaque_payloads_finish_and_retry() {
        let at = Instant::now();
        let mut c: MetadataCohort<u32, std::result::Result<u64, &'static str>> =
            MetadataCohort::with_limits(4, at, RandomState::new());
        let mut wakes = Vec::new();
        let k = key(RefreshPurpose::Current);
        let deadline = at + Duration::from_secs(10);
        let mut join = |c: &mut MetadataCohort<_, _>| {
            c.join(&k, at, deadline, 2, |_| Ok(()), &mut wakes).unwrap()
        };
        let (a, b) = (join(&mut c), join(&mut c));
        let mut wakes = Vec::new();
        let Ok(Step::Lead(started)) = c.poll(&k, a, at, 2, 0, &mut wakes) else {
            panic!("lead");
        };
        assert_eq!(c.poll(&k, b, at, 2, 1, &mut wakes), Ok(Step::Pending));
        c.retry(&k, started, Error::OriginRejected, &mut wakes)
            .unwrap();
        assert_eq!(
            c.poll(&k, a, at, 2, 0, &mut wakes),
            Ok(Step::Failed(Error::OriginRejected))
        );
        let Ok(Step::Lead(started)) = c.poll(&k, b, at, 2, 1, &mut wakes) else {
            panic!("re-elect");
        };
        c.finish(&k, started, Err("not found"), &mut wakes).unwrap();
        assert_eq!(
            c.poll(&k, b, at, 2, 1, &mut wakes),
            Ok(Step::Ready(Err("not found")))
        );
        assert_eq!(c.waiters(&k, b), 2);
        assert_eq!(c.attached(), 2);
        c.detach(&k, a, &mut wakes);
        c.detach(&k, b, &mut wakes);
        c.detach(&k, b, &mut wakes);
        assert_eq!(c.attached(), 0);
        assert!(c.table().is_empty());
    }

    #[test]
    fn cohort_and_waiter_bounds() {
        let at = Instant::now();
        let mut c: Cohorts = MetadataCohort::with_limits(1, at, RandomState::new());
        let mut wakes = Vec::new();
        let deadline = at + Duration::from_secs(10);
        let k = key(RefreshPurpose::Current);
        let other = key(RefreshPurpose::Bootstrap);
        let mut members = Vec::new();
        for _ in 0..MAX_WAITERS {
            members.push(c.join(&k, at, deadline, 1, |_| Ok(()), &mut wakes).unwrap());
        }
        assert_eq!(
            c.join(&other, at, deadline, 1, |_| Ok(()), &mut wakes),
            Err(Error::Overloaded)
        );
        assert_eq!(
            c.join(&k, at, deadline, 1, |_| Ok(()), &mut wakes),
            Err(Error::Overloaded)
        );
        c.detach(&k, members.pop().unwrap(), &mut wakes);
        assert_eq!(
            c.join(&other, at, deadline, 1, |_| Ok(()), &mut wakes),
            Err(Error::Overloaded)
        );
        assert!(c.join(&k, at, deadline, 1, |_| Ok(()), &mut wakes).is_ok());
    }

    #[test]
    fn retained_targets_are_handed_back() {
        let at = Instant::now();
        let deadline = at + Duration::from_secs(10);
        let mut c: Cohorts =
            MetadataCohort::with_limits(1, at, RandomState::new()).retain_retired();
        let mut wakes = Vec::new();
        let k = key(RefreshPurpose::Current);
        let a = c.join(&k, at, deadline, 1, |_| Ok(()), &mut wakes).unwrap();
        let b = c.join(&k, at, deadline, 1, |_| Ok(()), &mut wakes).unwrap();
        assert!(matches!(
            c.poll(&k, a, at, 1, 10, &mut wakes),
            Ok(Step::Lead(_))
        ));
        assert_eq!(c.poll(&k, b, at, 1, 11, &mut wakes), Ok(Step::Pending));
        assert_eq!(c.poll(&k, b, at, 1, 12, &mut wakes), Ok(Step::Pending));
        assert_eq!(c.take_retired(), vec![11]);
        c.detach(&k, b, &mut wakes);
        assert_eq!(c.take_retired(), vec![12]);
        assert!(c.take_retired().is_empty());
    }
}
