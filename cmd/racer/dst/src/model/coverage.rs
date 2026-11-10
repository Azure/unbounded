//! Coverage check: every core effect and serve outcome variant must be
//! exercised by the CI Model scenario set.
//!
//! [`super::effect_name`] and the serve outcome names match exhaustively on the core types, so a
//! new core variant does not compile until it has a name; [`VARIANTS`] must then list it: a run
//! that meets a name missing from [`VARIANTS`] fails with a hard failure.

use std::collections::BTreeSet;

/// Every coverage name: effect variants, completion results and serve outcomes.
pub const VARIANTS: &[&str] = &[
    "Acquisition::Rank",
    "Acquisition::Hints",
    "Acquisition::Hedge",
    "Acquisition::Refresh",
    "Acquisition::Send",
    "Acquisition::Validate",
    "Acquisition::Batch",
    "Acquisition::Sleep",
    "Acquisition::Origin",
    "Acquisition::Publish",
    "Window::Probe",
    "Window::Fetch",
    "Window::Select",
    "Window::Hold",
    "Window::SpillFill",
    "Window::Release",
    "Window::Discard",
    "Deliver::End",
    "Deliver::Wait",
    "Deliver::Page",
    "Deliver::Fail",
    "Worker::Fetch",
    "Worker::Fetched",
    "Completed::Ok",
    "Completed::Err",
    "Metadata",
    "MetadataReady::Ok",
    "MetadataReady::Err",
    "Silence",
    "Served::Defer",
    "Served::Reject",
    "Served::Miss",
    "Served::Shed",
    "Served::CopyOnly",
    "Served::Acquire",
];

/// Variants the core never emits by construction, with the reason. They are exempt from the
/// coverage check; the interpreter counts one as a hard failure if it ever appears.
pub const UNREACHABLE: &[(&str, &str)] = &[
    (
        "Window::Fetch",
        "WorkerCore::route_window turns every window Fetch into a flight join or a Worker::Fetch",
    ),
    (
        "Deliver::Wait",
        "WorkerCore::deliver returns on Next::Wait without emitting it",
    ),
];

/// Variants the Model cannot drive yet, with the reason. Each is a known gap listed in
/// `dst/MODEL.md`. A variant here that the CI set does
/// exercise fails the check, so the list cannot go stale.
pub const NOT_YET: &[(&str, &str)] = &[
    (
        "Served::Reject",
        "serving rejects mismatched objects or operations; Model probes always match",
    ),
    (
        "Served::Defer",
        "successor deferral needs successor hints from orphan notices, which are not modeled",
    ),
    (
        "Acquisition::Hedge",
        "hedged reads need FillConfig::hedge and hedge budgets the Model does not grant",
    ),
];

/// The coverage names of `required` that `seen` lacks, and the names of [`NOT_YET`] that `seen`
/// covers (stale exemptions).
pub fn check(seen: &BTreeSet<String>) -> (Vec<&'static str>, Vec<&'static str>) {
    let exempt = |name: &str| {
        UNREACHABLE.iter().any(|(n, _)| *n == name) || NOT_YET.iter().any(|(n, _)| *n == name)
    };
    let missing = VARIANTS
        .iter()
        .copied()
        .filter(|name| !exempt(name) && !seen.contains(*name))
        .collect();
    let stale = NOT_YET
        .iter()
        .map(|(name, _)| *name)
        .filter(|name| seen.contains(*name))
        .collect();
    (missing, stale)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exemptions_name_listed_variants() {
        for (name, _) in UNREACHABLE.iter().chain(NOT_YET) {
            assert!(VARIANTS.contains(name), "{name}");
        }
    }

    #[test]
    fn check_reports_missing_and_stale_names() {
        let mut seen: BTreeSet<String> = VARIANTS.iter().map(|s| s.to_string()).collect();
        let (missing, stale) = check(&seen);
        assert!(missing.is_empty());
        assert_eq!(stale.len(), NOT_YET.len());
        seen.clear();
        let (missing, stale) = check(&seen);
        assert_eq!(
            missing.len(),
            VARIANTS.len() - UNREACHABLE.len() - NOT_YET.len()
        );
        assert!(stale.is_empty());
    }
}
