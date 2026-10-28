//! Sans-IO page fill plan: local tiers, then the candidate walk, then origin,
//! then publication.
//!
//! Production `Fill::acquire_once` (`src/read/fill.rs`) and the simulator both
//! drive [`FillPlan`]; production also calls [`leader_source`],
//! [`acquisition_failure`], and [`ciphertext_publish_tolerated`].

use super::candidates::{ProbeOutcome, origin_miss_error};
use super::error::Error;
use super::walk::RemainingEnd;

/// Which local source an elected leader tries before network acquisition.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum LeaderSource {
    /// Ciphertext retained by the flight from an earlier partial publication.
    Retained,
    /// Join the shared local plaintext decrypt job.
    SharedLocal,
    /// Look up cached ciphertext (memory or disk-backed).
    Cached,
}

/// Order of local sources for an elected leader. `retained` is the flight's
/// partial ciphertext; `local_job` is an existing shared plaintext job; and
/// `unverified` is cached ciphertext in memory.
pub fn leader_source(
    retained: bool,
    plaintext: bool,
    local_job: bool,
    unverified: bool,
) -> LeaderSource {
    if retained {
        LeaderSource::Retained
    } else if plaintext && (local_job || unverified) {
        LeaderSource::SharedLocal
    } else {
        LeaderSource::Cached
    }
}

/// How a failed leader settles its flight.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FailureKind {
    OriginRejected,
    OriginForbidden,
    Terminal(Error),
}

/// Map a leader's acquisition error to its flight failure.
pub fn acquisition_failure(error: Error) -> FailureKind {
    match error {
        Error::OriginRejected => FailureKind::OriginRejected,
        Error::OriginForbidden => FailureKind::OriginForbidden,
        error => FailureKind::Terminal(error),
    }
}

/// Errors from publishing an acquired ciphertext copy to memory that do not
/// fail the acquisition (the copy is still returned to waiters).
pub fn ciphertext_publish_tolerated(error: Error) -> bool {
    matches!(
        error,
        Error::Overloaded | Error::MissingKey | Error::Unavailable
    )
}

/// Where a filled page came from.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Source {
    Local,
    Hedge,
    Peer,
    Origin,
}

/// Next action the driver must perform.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum FillStep {
    /// Probe local memory and disk copies.
    LocalCopy,
    /// Optionally race a speculative hedge (plaintext page fills only).
    Hedge,
    /// Run the candidate walk.
    Walk,
    /// This owner contacts origin.
    Origin,
    /// After an origin version miss, probe later owners copy-only.
    RemainingCopy,
    /// Publish the page obtained from `Source`.
    Publish(Source),
    /// Finished.
    Done(Source),
    Failed(Error),
}

/// How the walk ended, as reported to the plan.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WalkResult {
    Copy,
    Origin { evidence: Vec<ProbeOutcome> },
    Failed(Error),
}

/// Sans-IO fill state machine for one elected acquisition.
#[derive(Clone, Debug)]
pub struct FillPlan {
    step: FillStep,
    plaintext: bool,
    evidence: Vec<ProbeOutcome>,
}

impl FillPlan {
    /// Start with the local tiers. `plaintext` selects hedging and whether a
    /// peer copy is decrypted before publication.
    pub fn new(plaintext: bool) -> Self {
        Self {
            step: FillStep::LocalCopy,
            plaintext,
            evidence: Vec::new(),
        }
    }

    pub fn step(&self) -> &FillStep {
        &self.step
    }

    /// Report the local tier result.
    pub fn local(&mut self, hit: bool) {
        self.expect(FillStep::LocalCopy);
        self.step = if hit {
            FillStep::Done(Source::Local)
        } else if self.plaintext {
            FillStep::Hedge
        } else {
            FillStep::Walk
        };
    }

    /// Report whether a hedge produced a page (`false` when not eligible).
    pub fn hedge(&mut self, won: bool) {
        self.expect(FillStep::Hedge);
        self.step = if won {
            FillStep::Publish(Source::Hedge)
        } else {
            FillStep::Walk
        };
    }

    /// Report how the candidate walk ended.
    pub fn walk(&mut self, result: WalkResult) {
        self.expect(FillStep::Walk);
        self.step = match result {
            WalkResult::Copy => FillStep::Publish(Source::Peer),
            WalkResult::Origin { evidence } => {
                self.evidence = evidence;
                FillStep::Origin
            }
            WalkResult::Failed(error) => FillStep::Failed(error),
        };
    }

    /// Report the origin result.
    pub fn origin(&mut self, result: Result<(), Error>) {
        self.expect(FillStep::Origin);
        self.step = match result {
            Ok(()) => FillStep::Publish(Source::Origin),
            Err(Error::VersionUnavailable) => FillStep::RemainingCopy,
            Err(error) => FillStep::Failed(error),
        };
    }

    /// Report how the remaining-copy walk ended.
    pub fn remaining(&mut self, result: Result<RemainingEnd, Error>) {
        self.expect(FillStep::RemainingCopy);
        self.step = match result {
            Ok(RemainingEnd::Copy) => FillStep::Publish(Source::Peer),
            Ok(RemainingEnd::None) => FillStep::Failed(origin_miss_error(&self.evidence)),
            Ok(RemainingEnd::Unavailable) => FillStep::Failed(Error::Unavailable),
            Err(error) => FillStep::Failed(error),
        };
    }

    /// Report publication.
    pub fn published(&mut self, result: Result<(), Error>) {
        let FillStep::Publish(source) = self.step else {
            panic!("not publishing: {:?}", self.step);
        };
        self.step = match result {
            Ok(()) => FillStep::Done(source),
            Err(error) => FillStep::Failed(error),
        };
    }

    fn expect(&self, step: FillStep) {
        assert_eq!(self.step, step, "fill plan out of order");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn local_hit_finishes() {
        let mut plan = FillPlan::new(true);
        plan.local(true);
        assert_eq!(plan.step(), &FillStep::Done(Source::Local));
    }

    #[test]
    fn plaintext_hedges_before_walk_ciphertext_does_not() {
        let mut plan = FillPlan::new(true);
        plan.local(false);
        assert_eq!(plan.step(), &FillStep::Hedge);
        plan.hedge(true);
        assert_eq!(plan.step(), &FillStep::Publish(Source::Hedge));
        plan.published(Ok(()));
        assert_eq!(plan.step(), &FillStep::Done(Source::Hedge));
        let mut plan = FillPlan::new(false);
        plan.local(false);
        assert_eq!(plan.step(), &FillStep::Walk);
    }

    #[test]
    fn origin_then_publish() {
        let mut plan = FillPlan::new(false);
        plan.local(false);
        plan.walk(WalkResult::Origin { evidence: vec![] });
        assert_eq!(plan.step(), &FillStep::Origin);
        plan.origin(Ok(()));
        plan.published(Err(Error::Overloaded));
        assert_eq!(plan.step(), &FillStep::Failed(Error::Overloaded));
    }

    #[test]
    fn version_miss_probes_remaining_copies_then_classifies() {
        for (end, step) in [
            (Ok(RemainingEnd::Copy), FillStep::Publish(Source::Peer)),
            (Ok(RemainingEnd::None), FillStep::Failed(Error::Unavailable)),
            (
                Ok(RemainingEnd::Unavailable),
                FillStep::Failed(Error::Unavailable),
            ),
            (Err(Error::Cancelled), FillStep::Failed(Error::Cancelled)),
        ] {
            let mut plan = FillPlan::new(false);
            plan.local(false);
            plan.walk(WalkResult::Origin {
                evidence: vec![ProbeOutcome::Unreachable],
            });
            plan.origin(Err(Error::VersionUnavailable));
            assert_eq!(plan.step(), &FillStep::RemainingCopy);
            plan.remaining(end);
            assert_eq!(plan.step(), &step);
        }
        let mut plan = FillPlan::new(false);
        plan.local(false);
        plan.walk(WalkResult::Origin {
            evidence: vec![ProbeOutcome::CopyMiss],
        });
        plan.origin(Err(Error::VersionUnavailable));
        plan.remaining(Ok(RemainingEnd::None));
        assert_eq!(plan.step(), &FillStep::Failed(Error::VersionUnavailable));
    }

    #[test]
    fn walk_and_origin_failures() {
        let mut plan = FillPlan::new(false);
        plan.local(false);
        plan.walk(WalkResult::Failed(Error::Unavailable));
        assert_eq!(plan.step(), &FillStep::Failed(Error::Unavailable));
        let mut plan = FillPlan::new(false);
        plan.local(false);
        plan.walk(WalkResult::Origin { evidence: vec![] });
        plan.origin(Err(Error::OriginForbidden));
        assert_eq!(plan.step(), &FillStep::Failed(Error::OriginForbidden));
    }

    #[test]
    #[should_panic(expected = "out of order")]
    fn out_of_order_report_panics() {
        FillPlan::new(false).origin(Ok(()));
    }

    #[test]
    fn leader_sources_and_failures() {
        assert_eq!(
            leader_source(true, true, true, true),
            LeaderSource::Retained
        );
        assert_eq!(
            leader_source(false, true, true, false),
            LeaderSource::SharedLocal
        );
        assert_eq!(
            leader_source(false, true, false, true),
            LeaderSource::SharedLocal
        );
        assert_eq!(
            leader_source(false, false, true, true),
            LeaderSource::Cached
        );
        assert_eq!(
            leader_source(false, true, false, false),
            LeaderSource::Cached
        );
        assert_eq!(
            acquisition_failure(Error::OriginRejected),
            FailureKind::OriginRejected
        );
        assert_eq!(
            acquisition_failure(Error::OriginForbidden),
            FailureKind::OriginForbidden
        );
        assert_eq!(
            acquisition_failure(Error::Unavailable),
            FailureKind::Terminal(Error::Unavailable)
        );
        assert!(ciphertext_publish_tolerated(Error::MissingKey));
        assert!(!ciphertext_publish_tolerated(Error::CorruptRecord));
    }
}
