//! [`MetadataCohort`]: sans-IO metadata refresh cohorts on a [`FlightTable`].
//!
//! Mirrors the dataplane refresh cohort (`src/read/metadata.rs`, `resolve_inner` and `refresh`):
//!
//! - **Purpose**: callers coalesce only with the same object and [`RefreshPurpose`], so a
//!   metadata-only (HEAD) caller never joins or elects a body-fetching bootstrap leader, and a pin
//!   never shares a leader with a current observation.
//! - **Election**: the first eligible waiter leads, with at most `generations_per_flight`
//!   attempts per cohort; a leader needs a nonzero attempt budget.
//! - **Settlement** ([`refresh_settlement`]): a credential-specific origin rejection or a failure
//!   of the leader's own budget fails only the leader and lets another waiter retry; any other
//!   failure, or a success, finishes the cohort for every waiter.
//! - **Epoch check**: a leader records the clock epoch when it starts. A current or bootstrap
//!   observation is published as current freshness only if no clock discontinuity was observed
//!   since; otherwise only the immutable version is published.

use super::flight::{FlightLimits, FlightTable, Joined, Reserve, Settled, Ticket, Token, Wait};
use super::{
    Error, FailureKind, MAX_REFRESH_ATTEMPTS, MAX_WAITERS, RefreshPurpose, Result, Settlement,
    refresh_settlement,
};
use crate::flow::coalesce::flight::state::Published;
use std::collections::hash_map::RandomState;
use std::hash::{BuildHasher, Hash};
use std::time::Instant;

/// Credential-free cohort identity: object plus purpose.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct CohortKey<Obj> {
    pub object: Obj,
    pub purpose: RefreshPurpose,
}

/// A leader's capability and the clock epoch it started in.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Started {
    pub ticket: Ticket,
    pub epoch: u64,
}

/// What a polled cohort waiter should do next.
#[derive(Debug, Eq, PartialEq)]
pub enum Step<M> {
    /// Refresh with the caller's own budget, then call [`MetadataCohort::settle`].
    Lead(Started),
    Ready(M),
    Failed(Error),
    Pending,
}

/// Result of a cohort join.
#[derive(Debug, Eq, PartialEq)]
pub enum Join<M> {
    /// The cohort already settled to this observation.
    Ready(M),
    /// Registered as waiter `id`; poll it.
    Waiter(u64),
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

/// Cohort bounds matching the dataplane: `MAX_WAITERS` per cohort and `MAX_REFRESH_ATTEMPTS`
/// elections per cohort.
pub fn cohort_limits(entries: usize) -> FlightLimits {
    FlightLimits {
        entries,
        waiters_per_flight: MAX_WAITERS,
        operations_per_flight: 1,
        generations_per_flight: MAX_REFRESH_ATTEMPTS as u64,
    }
}

/// Worker-local metadata refresh cohorts. `M` is the shared observation and `R` each waiter's and
/// cohort's admission charge.
pub struct MetadataCohort<Obj, M, R = (), S = RandomState, T = Token> {
    table: FlightTable<CohortKey<Obj>, M, M, M, R, S, T>,
    epoch: u64,
}

impl<Obj, M, R, S, T> MetadataCohort<Obj, M, R, S, T>
where
    Obj: Eq + Hash + Clone,
    M: Clone,
    S: BuildHasher,
    T: Clone + PartialEq,
{
    pub fn new(table: FlightTable<CohortKey<Obj>, M, M, M, R, S, T>) -> Self {
        Self { table, epoch: 0 }
    }

    /// A table with [`cohort_limits`].
    pub fn with_limits(entries: usize, now: Instant, hasher: S) -> Self {
        Self::new(FlightTable::new(
            cohort_limits(entries),
            complete,
            now,
            hasher,
        ))
    }

    pub fn table(&self) -> &FlightTable<CohortKey<Obj>, M, M, M, R, S, T> {
        &self.table
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

    /// Join `key`'s cohort, or observe its settled result.
    #[allow(clippy::too_many_arguments)] // Mirrors the flight table join.
    pub fn join(
        &mut self,
        key: &CohortKey<Obj>,
        now: Instant,
        deadline: Instant,
        attempts: u32,
        reserve: impl FnMut(Reserve) -> Result<R>,
        wakes: &mut Vec<T>,
    ) -> Result<Join<M>> {
        Ok(
            match self
                .table
                .join(key, now, deadline, attempts, true, reserve, wakes)?
            {
                Joined::Settled(Settled::Complete(m) | Settled::Partial(m)) => Join::Ready(m),
                Joined::Waiter(id) => Join::Waiter(id),
                Joined::Miss => return Err(Error::Internal),
            },
        )
    }

    /// Poll waiter `id`. A leader needs a nonzero attempt budget.
    pub fn poll(
        &mut self,
        key: &CohortKey<Obj>,
        id: u64,
        now: Instant,
        attempts: u32,
        token: T,
        wakes: &mut Vec<T>,
    ) -> Result<Step<M>> {
        Ok(
            match self.table.poll(key, id, now, attempts, token, wakes)? {
                Wait::Lead => Step::Lead(Started {
                    ticket: self.table.ticket(key, id).ok_or(Error::Internal)?,
                    epoch: self.epoch,
                }),
                Wait::Ready(Settled::Complete(m) | Settled::Partial(m)) => Step::Ready(m),
                Wait::Failed(error) => Step::Failed(error),
                Wait::Pending => Step::Pending,
            },
        )
    }

    /// Settle the leader's attempt. `remaining` is the leader's attempt budget after it.
    /// `Internal` for a stale leader.
    pub fn settle(
        &mut self,
        key: &CohortKey<Obj>,
        started: Started,
        result: Result<M>,
        remaining: u32,
        wakes: &mut Vec<T>,
    ) -> Result<Outcome> {
        self.table.validate(key, &started.ticket)?;
        let leader = started.ticket.waiter;
        match result {
            Ok(metadata) => {
                self.table.finish(key, leader, Ok(metadata), wakes)?;
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
            Err(error) => match (refresh_settlement(error, remaining), error) {
                (Settlement::Retry, Error::OriginRejected) => {
                    self.table
                        .finish(key, leader, Err(FailureKind::OriginRejected), wakes)?;
                    Ok(Outcome::Retry)
                }
                (Settlement::Retry, Error::OriginForbidden) => {
                    self.table
                        .finish(key, leader, Err(FailureKind::OriginForbidden), wakes)?;
                    Ok(Outcome::Retry)
                }
                (Settlement::Retry, _) => {
                    // The leader's own budget failed: revoke it so another waiter leads.
                    self.table.cancel(key, leader, wakes);
                    Ok(Outcome::Retry)
                }
                (Settlement::Finish, error) => {
                    self.table
                        .finish(key, leader, Err(FailureKind::Terminal(error)), wakes)?;
                    Ok(Outcome::Finished(None))
                }
            },
        }
    }

    /// Detach waiter `id` (completion or caller drop), removing a quiescent cohort.
    pub fn detach(&mut self, key: &CohortKey<Obj>, id: u64, wakes: &mut Vec<T>) {
        self.table.detach(key, id, wakes);
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;
    use racer_object_wire::model::StrongEtag;
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
        fn join(&mut self, k: &CohortKey<u32>, attempts: u32) -> Result<Join<u64>> {
            let deadline = self.at + Duration::from_secs(10);
            self.c
                .join(k, self.at, deadline, attempts, |_| Ok(()), &mut self.wakes)
        }
        fn waiter(&mut self, k: &CohortKey<u32>) -> u64 {
            match self.join(k, 2) {
                Ok(Join::Waiter(id)) => id,
                other => panic!("expected waiter, got {other:?}"),
            }
        }
        fn poll(&mut self, k: &CohortKey<u32>, id: u64) -> Step<u64> {
            self.c.poll(k, id, self.at, 2, id, &mut self.wakes).unwrap()
        }
        fn lead(&mut self, k: &CohortKey<u32>, id: u64) -> Started {
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
        assert_eq!(f.join(&k, 2), Ok(Join::Ready(5)));
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
        let ids: Vec<u64> = (0..=MAX_REFRESH_ATTEMPTS as u64)
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
}
