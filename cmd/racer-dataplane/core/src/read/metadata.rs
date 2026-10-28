//! Metadata refresh cohort bounds, purposes, and settlement.
//!
//! Concurrent metadata resolutions for one object and purpose share one
//! refresh through `flow_control::coalesce::Table` in production
//! `MetadataService`. Elections are bounded by [`MAX_REFRESH_ATTEMPTS`].
//! [`refresh_settlement`] is the shared rule for a leader's outcome.

use super::error::Error;
use racer_object_wire::model::{MetadataSelector, StrongEtag};

/// Waiters per refresh cohort.
pub const MAX_WAITERS: usize = 64;

/// Leader elections per cohort before broadcasting exhaustion.
pub const MAX_REFRESH_ATTEMPTS: usize = 8;

/// The work a refresh cohort may perform. A pin never bootstraps a current body.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub enum RefreshPurpose {
    /// Observe the current version without requesting its body (including HEAD).
    Current,
    /// Resolve immutable metadata for exactly this version, even after expiry.
    Pinned(StrongEtag),
    /// Observe the current version and allow acquisition of page zero.
    Bootstrap,
}

impl From<MetadataSelector> for RefreshPurpose {
    /// Metadata-only callers cannot opt into a body-fetching cohort.
    fn from(selector: MetadataSelector) -> Self {
        match selector {
            MetadataSelector::Fresh => Self::Current,
            MetadataSelector::Pinned(etag) => Self::Pinned(etag),
        }
    }
}

impl RefreshPurpose {
    /// Select metadata without changing whether the cohort may fetch a body.
    pub fn selector(&self) -> MetadataSelector {
        match self {
            Self::Current | Self::Bootstrap => MetadataSelector::Fresh,
            Self::Pinned(etag) => MetadataSelector::Pinned(etag.clone()),
        }
    }

    /// Whether this purpose may acquire page zero (a bootstrap operation).
    pub fn bootstrap(&self) -> bool {
        matches!(self, Self::Bootstrap)
    }
}

/// How a refresh leader settles its cohort.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Settlement {
    /// Broadcast the result to every waiter.
    Finish,
    /// Release leadership so another waiter may try with its own credentials
    /// or budget.
    Retry,
}

/// Settle a failed refresh. Credential-specific origin rejections and
/// failures of the leader's own budget let another waiter retry; other
/// failures finish the cohort.
pub fn refresh_settlement(error: Error, remaining_attempts: u32) -> Settlement {
    let rejected = matches!(error, Error::OriginRejected | Error::OriginForbidden);
    let caller_budget = matches!(
        error,
        Error::Cancelled | Error::DeadlineExceeded | Error::HopBudgetExhausted
    ) || (error == Error::Unavailable && remaining_attempts == 0);
    if rejected || caller_budget {
        Settlement::Retry
    } else {
        Settlement::Finish
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn settlement_rules() {
        for error in [
            Error::OriginRejected,
            Error::OriginForbidden,
            Error::Cancelled,
            Error::DeadlineExceeded,
            Error::HopBudgetExhausted,
        ] {
            assert_eq!(refresh_settlement(error, 3), Settlement::Retry, "{error:?}");
        }
        assert_eq!(refresh_settlement(Error::Unavailable, 0), Settlement::Retry);
        assert_eq!(
            refresh_settlement(Error::Unavailable, 1),
            Settlement::Finish
        );
        assert_eq!(refresh_settlement(Error::NotFound, 0), Settlement::Finish);
    }

    #[test]
    fn purposes() {
        let etag = StrongEtag::test_value("v");
        assert_eq!(
            RefreshPurpose::from(MetadataSelector::Pinned(etag.clone())),
            RefreshPurpose::Pinned(etag.clone())
        );
        assert!(matches!(
            RefreshPurpose::Bootstrap.selector(),
            MetadataSelector::Fresh
        ));
        assert!(matches!(
            RefreshPurpose::Pinned(etag).selector(),
            MetadataSelector::Pinned(_)
        ));
        assert!(RefreshPurpose::Bootstrap.bootstrap());
        assert!(!RefreshPurpose::Current.bootstrap());
    }
}
