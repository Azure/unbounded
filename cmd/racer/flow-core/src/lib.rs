//! Pure flow-control mechanisms: keyed coalescing, flight lifecycle state, adaptive admission,
//! endpoint circuits, hedges, FIFO bookkeeping, progress prediction, and subscription fanout.
//!
//! `racer-core` re-exports this crate as `racer_core::flow`; `flow-control` re-exports it and adds
//! the IO-bound parts (charged buffers, quotas over `page-alloc`, kernel pipes).
//!
//! The purity fence (`hack/scripts/racer-purity-graph.py`, `core/tests/purity.rs`) forbids pure
//! crates such as `racer-core` from depending on IO crates such as `flow-control`, so these shared
//! mechanisms cannot live in `flow-control`. They could instead live in `racer-core` with
//! `flow-control` depending on `racer-core`; this separate pure crate keeps `flow-control` from
//! pulling in all of `racer-core` for them.
//!
//! Callers pass time in; the crate never reads clocks, spawns threads, or reads OS randomness.

#![cfg_attr(test, allow(clippy::disallowed_methods))]

/// Adaptive admission and completion-owned handoffs.
pub mod admission;

/// Keyed cohorts and completion-owned flight tracking.
pub mod coalesce;

/// Deadline-aware FIFO admission bookkeeping.
pub mod fifo;

/// Progress prediction and bounded synchronous reclamation.
pub mod progress;

/// Compact interval selection and bounded subscription fanout.
pub mod subscriptions;

pub use admission::{
    Adaptive, Admitted, Circuits, Config as AdaptiveConfig, Event as AdaptiveEvent, Handoff,
    HandoffAdmission, HedgePermit, Hedges, Observer as AdaptiveObserver, Offer,
    Outcome as AdaptiveOutcome, Permit as AdaptivePermit, Probe,
};

/// Admission and pipe-creation failures, independent of application error policy.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// The requested geometry or state transition is invalid.
    InvalidInput,

    /// A resource or keyed fairness limit is exhausted.
    Overloaded,

    /// Admission has stopped or its shared state is unavailable.
    Unavailable,

    /// A kernel pipe operation failed during creation.
    Io,
}

impl std::fmt::Display for Error {
    /// Describe the failure without application resource names.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::InvalidInput => "invalid flow-control input",
            Self::Overloaded => "flow-control quota exhausted",
            Self::Unavailable => "flow control unavailable",
            Self::Io => "flow-control I/O failed",
        })
    }
}

impl std::error::Error for Error {}

/// A flow-control operation's result.
pub type Result<T> = std::result::Result<T, Error>;

/// A dense, stable index in `0..COUNT`, unique to each resource class.
pub trait Class: Copy + Send + Sync + 'static {
    /// Number of distinct resource classes.
    const COUNT: usize;

    /// Return this class's unique index, strictly below `COUNT`.
    fn index(self) -> usize;

    /// Default to secure erasure unless the class contains only public bytes.
    fn requires_wipe(self) -> bool {
        true
    }
}
