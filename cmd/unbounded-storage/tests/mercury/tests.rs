// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Proptest entrypoint and invariant helpers for the Mercury bridge
//! DST area. One `proptest!` block per AGENTS.md convention; one
//! `#[test]` that calls `run_workload` once and dispatches to small
//! `assert_<invariant>` helpers returning `Result<(), TestCaseError>`.

use std::collections::{HashMap, HashSet};

use proptest::prelude::*;

use crate::mercury::oracle::{SubmissionObserved, pre_close_tags};
use crate::mercury::workload::{RunReport, run_workload, workload_strategy};

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 256,
        ..ProptestConfig::default()
    })]

    #[test]
    fn invariants(seed in any::<u64>(), w in workload_strategy()) {
        let report = run_workload(seed, w).expect("run completed");
        assert_no_completion_leak(&report)?;
        assert_no_slot_arc_leak(&report)?;
        assert_outcome_observed(&report)?;
        assert_cancellation_safe(&report)?;
        assert_max_inflight(&report)?;
        assert_submit_rejected_releases_slot(&report)?;
        assert_queue_no_drop_before_close(&report)?;
        assert_queue_drained_after_close(&report)?;
        assert_queue_post_close_pushes_dropped(&report)?;
        assert_queue_fifo_before_close(&report)?;
        assert_progress_history_complete(&report)?;
    }
}

/// No leak: registry empties out and observed peak never exceeds cap.
fn assert_no_completion_leak(r: &RunReport) -> Result<(), TestCaseError> {
    prop_assert_eq!(
        r.registry_live_at_end,
        0,
        "registry retained {} slots at end of run",
        r.registry_live_at_end,
    );
    Ok(())
}

/// Stronger leak check: every `Arc<CompletionSlot>` allocated during
/// the run is fully dropped by the time the executor returns. The
/// registry's `live_count` only sees the registry-side `Arc`; this
/// invariant proves the FFI-side reclaim path also drops its
/// reference, by upgrading mock-held `Weak` snapshots.
fn assert_no_slot_arc_leak(r: &RunReport) -> Result<(), TestCaseError> {
    prop_assert_eq!(
        r.slot_weak_upgrades_at_end,
        0,
        "{} CompletionSlot Arc(s) still upgradeable from Weak at end of run",
        r.slot_weak_upgrades_at_end,
    );
    Ok(())
}

/// For every submission that was not cancelled and whose alloc
/// succeeded, the client observes the exact outcome the mock chose.
/// This also covers the lost-wakeup liveness invariant: a stuck
/// `Pending` future would surface as `RunError::Deadlock` from
/// `run_workload`, and a misrouted `Cancelled` observation against a
/// non-cancelled client trips the `false` branch below.
fn assert_outcome_observed(r: &RunReport) -> Result<(), TestCaseError> {
    for (i, sub) in r.submissions.iter().enumerate() {
        if sub.cancelled {
            continue;
        }
        match r.observations[i] {
            SubmissionObserved::Resolved(o) => {
                prop_assert_eq!(
                    o,
                    sub.outcome,
                    "client {} observed {:?}, expected {:?}",
                    i,
                    o,
                    sub.outcome,
                );
            }
            SubmissionObserved::Cancelled => {
                prop_assert!(
                    false,
                    "client {} reported Cancelled but was not flagged cancelled",
                    i
                );
            }
            SubmissionObserved::AllocRejected | SubmissionObserved::SubmitRejected => {
                // Pre-submit failures are legitimate: no callback was
                // ever scheduled, so no outcome to compare against.
            }
        }
    }
    Ok(())
}

/// Cancelled clients report `Cancelled`; the run must still finish
/// (the `run_workload` Ok return covers liveness, this helper covers
/// labelling).
fn assert_cancellation_safe(r: &RunReport) -> Result<(), TestCaseError> {
    for (i, sub) in r.submissions.iter().enumerate() {
        if !sub.cancelled {
            continue;
        }
        let o = r.observations[i];
        prop_assert!(
            matches!(
                o,
                SubmissionObserved::Cancelled
                    | SubmissionObserved::AllocRejected
                    | SubmissionObserved::SubmitRejected
            ),
            "client {} flagged cancel but observed {:?}",
            i,
            o,
        );
    }
    Ok(())
}

/// `CompletionRegistry::alloc` honoured the configured cap.
fn assert_max_inflight(r: &RunReport) -> Result<(), TestCaseError> {
    prop_assert!(
        r.registry_peak_inflight <= r.registry_capacity,
        "peak {} exceeded cap {}",
        r.registry_peak_inflight,
        r.registry_capacity,
    );
    Ok(())
}

/// A synchronous `submit` failure must release the slot back to the
/// registry (no leak) and must not produce a callback. The end-of-run
/// `registry_live_at_end == 0` assertion plus the
/// `assert_progress_history_complete` tag-multiset check between them
/// catch double-counting; this helper names the invariant explicitly
/// and checks that `SubmitRejected` only appears against submissions
/// that the workload did not flag as cancelled (cancellation is
/// applied post-submit).
fn assert_submit_rejected_releases_slot(r: &RunReport) -> Result<(), TestCaseError> {
    for (i, sub) in r.submissions.iter().enumerate() {
        if r.observations[i] != SubmissionObserved::SubmitRejected {
            continue;
        }
        prop_assert!(
            !sub.cancelled,
            "client {} reported SubmitRejected but was flagged cancelled",
            i,
        );
    }
    Ok(())
}

/// Every job pushed *before* `close` is observed by exactly one
/// consumer.
fn assert_queue_no_drop_before_close(r: &RunReport) -> Result<(), TestCaseError> {
    let pre = pre_close_tags(&r.jobs);
    let observed: HashSet<u64> = r.jobs_observed.iter().copied().collect();
    // Multiplicity: tags are unique by construction; each pre-close
    // tag must appear exactly once across consumers.
    let mut counts: HashMap<u64, u32> = HashMap::new();
    for tag in &r.jobs_observed {
        *counts.entry(*tag).or_default() += 1;
    }
    for tag in &pre {
        prop_assert!(
            observed.contains(tag),
            "pre-close job {} not observed by any consumer",
            tag,
        );
        prop_assert_eq!(
            counts.get(tag).copied().unwrap_or(0),
            1,
            "pre-close job {} observed {} times (expected 1)",
            tag,
            counts.get(tag).copied().unwrap_or(0),
        );
    }
    Ok(())
}

/// Every consumer eventually observed `Ready(None)` after the queue
/// closed.
fn assert_queue_drained_after_close(r: &RunReport) -> Result<(), TestCaseError> {
    prop_assert_eq!(
        r.consumers_saw_none,
        r.queue_consumers,
        "{} of {} consumers exited via Ready(None)",
        r.consumers_saw_none,
        r.queue_consumers,
    );
    Ok(())
}

/// Jobs the producer pushed *after* calling `close` are silently
/// dropped (current production behaviour). Verify they are not
/// observed by any consumer.
fn assert_queue_post_close_pushes_dropped(r: &RunReport) -> Result<(), TestCaseError> {
    let post_close: HashSet<u64> = r
        .jobs
        .iter()
        .filter(|j| !j.before_close)
        .map(|j| j.tag)
        .collect();
    for tag in r.jobs_observed.iter() {
        prop_assert!(
            !post_close.contains(tag),
            "post-close job {} was observed",
            tag,
        );
    }
    Ok(())
}

/// With a single consumer, pre-close jobs must be observed in
/// producer (push) order. `ServerJobQueue` is a `Mutex<VecDeque>`
/// drained from the front, so for a single drainer the observation
/// order must equal the push order of pre-close tags. The producer
/// pushes tags in the order they appear in `r.jobs`, so the
/// pre-close subsequence of `r.jobs_observed` (filtering to tags
/// that are in the pre-close set) must equal the pre-close
/// subsequence of `r.jobs` in order.
fn assert_queue_fifo_before_close(r: &RunReport) -> Result<(), TestCaseError> {
    if r.queue_consumers != 1 {
        return Ok(());
    }
    let pre: HashSet<u64> = pre_close_tags(&r.jobs);
    let expected: Vec<u64> = r
        .jobs
        .iter()
        .filter(|j| j.before_close)
        .map(|j| j.tag)
        .collect();
    let observed: Vec<u64> = r
        .jobs_observed
        .iter()
        .copied()
        .filter(|t| pre.contains(t))
        .collect();
    prop_assert_eq!(
        &observed,
        &expected,
        "queue did not deliver pre-close jobs FIFO",
    );
    Ok(())
}

/// The progress task delivers a callback for every submission that
/// actually made it past `alloc`. The number of history entries
/// must equal the number of non-`AllocRejected` submissions, and
/// the tag multiset must match. Tags are unique per submission, so
/// this also implies each tag is delivered at most once. Catches a
/// class of bugs where the callback path silently drops a slot
/// (e.g. losing the FFI-side `Arc` reclaim) without producing a
/// `Deadlock`.
fn assert_progress_history_complete(r: &RunReport) -> Result<(), TestCaseError> {
    let submitted_tags: Vec<u64> = r
        .submissions
        .iter()
        .zip(r.observations.iter())
        .filter_map(|(s, o)| match o {
            SubmissionObserved::AllocRejected | SubmissionObserved::SubmitRejected => None,
            _ => Some(s.tag),
        })
        .collect();
    prop_assert_eq!(
        r.progress_history.len(),
        submitted_tags.len(),
        "progress_history.len() = {} vs submitted = {}",
        r.progress_history.len(),
        submitted_tags.len(),
    );
    let mut expected: HashMap<u64, u32> = HashMap::new();
    for t in &submitted_tags {
        *expected.entry(*t).or_default() += 1;
    }
    let mut got: HashMap<u64, u32> = HashMap::new();
    for (t, _) in &r.progress_history {
        *got.entry(*t).or_default() += 1;
    }
    prop_assert_eq!(
        expected,
        got,
        "progress_history tag multiset mismatch",
    );
    Ok(())
}
