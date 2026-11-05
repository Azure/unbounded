//! The one retry/terminal table over the closed [`Error`] enum.
//!
//! Every consumer derives its view from [`disposition`]: the candidate walk
//! ([`super::walk::transport_outcome`]), the peer response an owner reports
//! ([`super::peer_error`]), and the prefetch owner failure code ([`fail_code`]). Each row is an
//! explicit match arm, so a new variant cannot fall through to a default.

use super::{Error, ProbeOutcome, ResponseKind};
use crate::prefetch::FailCode;

/// What a requester may do after an attempt failed with an error.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Retry {
    /// The failure is specific to the candidate (unreachable, unusable copy); try the next one.
    NextCandidate,
    /// The candidate is healthy but busy; it may be retried after backing off.
    SameCandidateLater,
    /// The failure is about the request or object itself; no candidate can do better.
    Never,
}

/// How one error is treated by every consumer.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Disposition {
    /// Requester retry rule.
    pub retry: Retry,
    /// Candidate-walk outcome; `None` exactly when `retry` is [`Retry::Never`].
    pub probe: Option<ProbeOutcome>,
    /// Peer response an owner reports; `None` means the error is internal and propagates.
    pub peer: Option<ResponseKind>,
    /// Prefetch owner failure code.
    pub fail: FailCode,
}

const fn row(
    retry: Retry,
    probe: Option<ProbeOutcome>,
    peer: Option<ResponseKind>,
    fail: FailCode,
) -> Disposition {
    Disposition {
        retry,
        probe,
        peer,
        fail,
    }
}

/// The table. A candidate-local failure never yields a terminal prefetch code (`OriginError` is
/// reserved for dataplane origin failures without a core counterpart). `HopBudgetExhausted` is local (the owner is unreachable within budget);
/// `Unauthorized` is a peer response that failed verification, which says nothing about other
/// owners. Dataplane errors without a core counterpart are mapped to `Internal` first.
pub const fn disposition(error: Error) -> Disposition {
    use FailCode as F;
    use ProbeOutcome as P;
    use ResponseKind as K;
    use Retry::{Never, NextCandidate, SameCandidateLater};
    match error {
        Error::Unavailable => row(
            NextCandidate,
            Some(P::Unreachable),
            Some(K::Unavailable),
            F::Unavailable,
        ),
        Error::HopBudgetExhausted => row(
            NextCandidate,
            Some(P::Unreachable),
            Some(K::Unavailable),
            F::Unavailable,
        ),
        Error::Io | Error::Os(_) => row(NextCandidate, Some(P::Unreachable), None, F::Unavailable),
        Error::Overloaded => row(
            SameCandidateLater,
            Some(P::Overloaded),
            Some(K::Overloaded),
            F::Overloaded,
        ),
        // Busy like `Overloaded`, but the destination proved it is fetching the page; the walk
        // tells the two apart only on a signed peer response (`ResponseKind::FlightFull`).
        Error::FlightFull => row(
            SameCandidateLater,
            Some(P::Overloaded),
            Some(K::FlightFull),
            F::Overloaded,
        ),
        Error::Unauthorized
        | Error::InvalidRequest
        | Error::HeaderTooLarge
        | Error::MissingKey
        | Error::CorruptRecord
        | Error::Internal => row(NextCandidate, Some(P::UnusableCopy), None, F::Unavailable),
        Error::DeadlineExceeded => row(Never, None, Some(K::Unavailable), F::Unavailable),
        Error::Cancelled | Error::IncompatibleMembership | Error::InvalidConfiguration => {
            row(Never, None, None, F::Unavailable)
        }
        Error::OriginRejected => row(Never, None, Some(K::OriginRejected), F::Forbidden),
        Error::OriginForbidden => row(Never, None, Some(K::OriginForbidden), F::Forbidden),
        Error::NotFound => row(Never, None, Some(K::NotFound), F::NotFound),
        Error::VersionUnavailable => {
            row(Never, None, Some(K::VersionUnavailable), F::VersionChanged)
        }
    }
}

/// Requester retry rule for `error`.
pub const fn retry(error: Error) -> Retry {
    disposition(error).retry
}

/// Prefetch owner failure code for `error`. Prefetch maps the code to a unit outcome, so whole-
/// operation classification and per-page codes agree by construction.
pub const fn fail_code(error: Error) -> FailCode {
    disposition(error).fail
}

/// The error a non-payload peer response reports, so requesters classify responses through the
/// same table. Payload responses and `Prefetch` carry no error. `Miss` and `StaleMembership` are
/// candidate-local and read as `Unavailable`.
pub const fn response_error(kind: ResponseKind) -> Option<Error> {
    Some(match kind {
        ResponseKind::Selected
        | ResponseKind::Bootstrap
        | ResponseKind::Page
        | ResponseKind::Metadata
        | ResponseKind::Prefetch => return None,
        ResponseKind::Miss | ResponseKind::StaleMembership | ResponseKind::Unavailable => {
            Error::Unavailable
        }
        ResponseKind::NotFound => Error::NotFound,
        ResponseKind::VersionUnavailable => Error::VersionUnavailable,
        ResponseKind::Overloaded => Error::Overloaded,
        ResponseKind::FlightFull => Error::FlightFull,
        ResponseKind::OriginRejected => Error::OriginRejected,
        ResponseKind::OriginForbidden => Error::OriginForbidden,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    const ALL: [Error; 20] = [
        Error::InvalidConfiguration,
        Error::InvalidRequest,
        Error::HeaderTooLarge,
        Error::NotFound,
        Error::Internal,
        Error::VersionUnavailable,
        Error::Unavailable,
        Error::Overloaded,
        Error::FlightFull,
        Error::DeadlineExceeded,
        Error::Cancelled,
        Error::Unauthorized,
        Error::OriginRejected,
        Error::OriginForbidden,
        Error::IncompatibleMembership,
        Error::HopBudgetExhausted,
        Error::CorruptRecord,
        Error::MissingKey,
        Error::Io,
        Error::Os(5),
    ];

    /// Keep `ALL` complete: adding a variant fails to compile here.
    #[test]
    fn all_is_exhaustive() {
        for error in ALL {
            match error {
                Error::InvalidConfiguration
                | Error::InvalidRequest
                | Error::HeaderTooLarge
                | Error::NotFound
                | Error::Internal
                | Error::VersionUnavailable
                | Error::Unavailable
                | Error::Overloaded
                | Error::FlightFull
                | Error::DeadlineExceeded
                | Error::Cancelled
                | Error::Unauthorized
                | Error::OriginRejected
                | Error::OriginForbidden
                | Error::IncompatibleMembership
                | Error::HopBudgetExhausted
                | Error::CorruptRecord
                | Error::MissingKey
                | Error::Io
                | Error::Os(_) => {}
            }
        }
    }

    #[test]
    fn every_variant_row() {
        use FailCode as F;
        use ProbeOutcome as P;
        use ResponseKind as K;
        use Retry::*;
        let expected = |error| match error {
            Error::InvalidConfiguration => (Never, None, None, F::Unavailable),
            Error::InvalidRequest => (NextCandidate, Some(P::UnusableCopy), None, F::Unavailable),
            Error::HeaderTooLarge => (NextCandidate, Some(P::UnusableCopy), None, F::Unavailable),
            Error::NotFound => (Never, None, Some(K::NotFound), F::NotFound),
            Error::Internal => (NextCandidate, Some(P::UnusableCopy), None, F::Unavailable),
            Error::VersionUnavailable => {
                (Never, None, Some(K::VersionUnavailable), F::VersionChanged)
            }
            Error::Unavailable => (
                NextCandidate,
                Some(P::Unreachable),
                Some(K::Unavailable),
                F::Unavailable,
            ),
            Error::Overloaded => (
                SameCandidateLater,
                Some(P::Overloaded),
                Some(K::Overloaded),
                F::Overloaded,
            ),
            Error::FlightFull => (
                SameCandidateLater,
                Some(P::Overloaded),
                Some(K::FlightFull),
                F::Overloaded,
            ),
            Error::DeadlineExceeded => (Never, None, Some(K::Unavailable), F::Unavailable),
            Error::Cancelled => (Never, None, None, F::Unavailable),
            Error::Unauthorized => (NextCandidate, Some(P::UnusableCopy), None, F::Unavailable),
            Error::OriginRejected => (Never, None, Some(K::OriginRejected), F::Forbidden),
            Error::OriginForbidden => (Never, None, Some(K::OriginForbidden), F::Forbidden),
            Error::IncompatibleMembership => (Never, None, None, F::Unavailable),
            Error::HopBudgetExhausted => (
                NextCandidate,
                Some(P::Unreachable),
                Some(K::Unavailable),
                F::Unavailable,
            ),
            Error::CorruptRecord => (NextCandidate, Some(P::UnusableCopy), None, F::Unavailable),
            Error::MissingKey => (NextCandidate, Some(P::UnusableCopy), None, F::Unavailable),
            Error::Io | Error::Os(_) => (NextCandidate, Some(P::Unreachable), None, F::Unavailable),
        };
        for error in ALL {
            let (retry, probe, peer, fail) = expected(error);
            assert_eq!(
                disposition(error),
                Disposition {
                    retry,
                    probe,
                    peer,
                    fail
                },
                "{error:?}"
            );
            assert_eq!(super::retry(error), retry);
            assert_eq!(super::fail_code(error), fail);
        }
    }

    #[test]
    fn consumers_derive_from_the_table() {
        for error in ALL {
            let row = disposition(error);
            assert_eq!(super::super::walk::transport_outcome(error), row.probe);
            assert_eq!(super::super::peer_error(error), row.peer);
            // The walk stops exactly on terminal errors.
            assert_eq!(row.probe.is_none(), row.retry == Retry::Never, "{error:?}");
        }
    }

    #[test]
    fn terminal_codes_only_for_never() {
        for error in ALL {
            let row = disposition(error);
            let terminal = matches!(
                row.fail,
                FailCode::VersionChanged
                    | FailCode::NotFound
                    | FailCode::Forbidden
                    | FailCode::OriginError
            );
            if terminal {
                assert_eq!(row.retry, Retry::Never, "{error:?}");
            }
        }
    }

    #[test]
    fn responses_round_trip_through_the_table() {
        for kind in [
            ResponseKind::NotFound,
            ResponseKind::VersionUnavailable,
            ResponseKind::Unavailable,
            ResponseKind::Overloaded,
            ResponseKind::FlightFull,
            ResponseKind::OriginRejected,
            ResponseKind::OriginForbidden,
        ] {
            let error = response_error(kind).unwrap();
            assert_eq!(disposition(error).peer, Some(kind));
        }
        for kind in [ResponseKind::Miss, ResponseKind::StaleMembership] {
            assert_eq!(response_error(kind), Some(Error::Unavailable));
        }
        for kind in [
            ResponseKind::Selected,
            ResponseKind::Bootstrap,
            ResponseKind::Page,
            ResponseKind::Metadata,
            ResponseKind::Prefetch,
        ] {
            assert_eq!(response_error(kind), None);
        }
    }
}
