// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-submission and per-job expectations the test driver tracks
//! alongside the system under test. Kept separate from `mocks.rs`
//! so the invariant helpers in `tests.rs` read against an explicit
//! model instead of inspecting mock internals directly.

use std::collections::HashSet;

use crate::mercury::mocks::{Outcome, SlotTag};

/// Per-submission expectation: which outcome the progress task will
/// deliver, and whether the client cancelled before observing it.
#[derive(Clone, Copy, Debug)]
pub struct SubmissionExpect {
    pub tag: SlotTag,
    pub outcome: Outcome,
    pub cancelled: bool,
}

/// Per-submission observation reported by the client task.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SubmissionObserved {
    /// The future resolved with the recorded outcome.
    Resolved(Outcome),
    /// The client dropped the future before observing the callback.
    Cancelled,
    /// Allocation failed (registry was at capacity).
    AllocRejected,
    /// `submit` failed synchronously after a successful alloc.
    /// The client released the slot and no callback was scheduled.
    SubmitRejected,
}

/// Expected and observed records for every server-job that the
/// producer planned to push.
#[derive(Clone, Copy, Debug)]
pub struct JobExpect {
    pub tag: SlotTag,
    /// Whether the producer pushed this job before calling `close`.
    /// Jobs pushed after close should be silently dropped by the
    /// queue (current production behaviour).
    pub before_close: bool,
}

/// Helper: build the set of tags pushed before close, for the
/// `assert_queue_no_drop_before_close` invariant.
pub fn pre_close_tags(jobs: &[JobExpect]) -> HashSet<SlotTag> {
    jobs.iter()
        .filter(|j| j.before_close)
        .map(|j| j.tag)
        .collect()
}
