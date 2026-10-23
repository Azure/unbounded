//! Sans-IO policy for serving an authenticated peer read request.
//!
//! Production `Coordinator::serve_peer` (src/read.rs) calls [`serve_action`]
//! to dispatch, [`super::budget::inherited_budget`] for acquisition credits,
//! and [`super::policy::peer_error`] to translate failures.

use super::candidates::OperationKind;
use super::error::Error;
use std::time::Instant;

/// Facts available after request authentication.
#[derive(Clone, Copy, Debug)]
pub struct ServeFacts {
    pub operation: OperationKind,
    /// The request's fetch mode is CopyOnly.
    pub copy_only: bool,
    /// The operation's object equals the signed origin context's object.
    pub object_matches: bool,
    /// The cache is configured locally and its metadata is available.
    pub cache_available: bool,
}

/// How to serve a peer request.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ServeAction {
    Reject(Error),
    /// Answer `Miss` without touching storage.
    Miss,
    /// Serve local copies only; never open credentials or acquire.
    CopyOnly,
    /// Open credentials and acquire with the inherited budget.
    Acquire,
}

/// Dispatch rule. Subscriptions never arrive here; warm actions always acquire.
pub fn serve_action(facts: ServeFacts) -> ServeAction {
    if facts.operation == OperationKind::Subscribe {
        return ServeAction::Reject(Error::InvalidRequest);
    }
    if !facts.object_matches {
        return ServeAction::Reject(Error::InvalidRequest);
    }
    if !facts.cache_available {
        return ServeAction::Miss;
    }
    if facts.copy_only && facts.operation != OperationKind::Warm {
        ServeAction::CopyOnly
    } else {
        ServeAction::Acquire
    }
}

/// Effective serve deadline: never later than the route's signed deadline.
pub fn serve_deadline(scope: Instant, route: Instant) -> Instant {
    scope.min(route)
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;
    use std::time::Duration;

    fn facts(operation: OperationKind, copy_only: bool) -> ServeFacts {
        ServeFacts {
            operation,
            copy_only,
            object_matches: true,
            cache_available: true,
        }
    }

    #[test]
    fn dispatch_table() {
        assert_eq!(
            serve_action(facts(OperationKind::Subscribe, false)),
            ServeAction::Reject(Error::InvalidRequest)
        );
        assert_eq!(
            serve_action(facts(OperationKind::Page, true)),
            ServeAction::CopyOnly
        );
        assert_eq!(
            serve_action(facts(OperationKind::Page, false)),
            ServeAction::Acquire
        );
        assert_eq!(
            serve_action(facts(OperationKind::Bootstrap, true)),
            ServeAction::CopyOnly
        );
        assert_eq!(
            serve_action(facts(OperationKind::Metadata { fresh: true }, true)),
            ServeAction::CopyOnly
        );
        assert_eq!(
            serve_action(facts(OperationKind::Warm, true)),
            ServeAction::Acquire
        );
        assert_eq!(
            serve_action(ServeFacts {
                object_matches: false,
                ..facts(OperationKind::Page, true)
            }),
            ServeAction::Reject(Error::InvalidRequest)
        );
        assert_eq!(
            serve_action(ServeFacts {
                cache_available: false,
                ..facts(OperationKind::Page, false)
            }),
            ServeAction::Miss
        );
    }

    #[test]
    fn deadline_never_extends() {
        let now = Instant::now();
        let later = now + Duration::from_secs(1);
        assert_eq!(serve_deadline(now, later), now);
        assert_eq!(serve_deadline(later, now), now);
    }
}
