//! Peer responses generic over their page payload.
//!
//! The dataplane instantiates [`PeerResponse`] with real ciphertext pages and
//! transfer grants.

use super::candidates::ResponseKind;
use racer_object_wire::model::ObjectMetadata;
use std::time::SystemTime;

/// Unsigned local result. Transport must sign it against the admitted request.
pub enum PeerResponse<P, G> {
    Selected {
        metadata: ObjectMetadata,
        ciphertext: P,
        grant: G,
    },
    Bootstrap {
        metadata: ObjectMetadata,
        page_zero: Option<P>,
    },
    Page {
        metadata: ObjectMetadata,
        ciphertext: P,
    },
    Metadata(ObjectMetadata),
    Miss,
    /// Authoritative origin absence, only for fresh metadata Acquire.
    NotFound,
    VersionUnavailable,
    Unavailable,
    Overloaded,
    OriginRejected,
    OriginForbidden,
    /// Authenticated peer no longer retains the requested routing epoch.
    StaleMembership,
    /// Warm holds accepted until this wall-clock time (millisecond precision).
    Held {
        until: SystemTime,
    },
    /// The requested warm page is published on the owner's disk under its hold.
    Warmed,
    /// The warm lease is gone. Unused capacity is freed after the last lease.
    Released,
}

impl<P, G> PeerResponse<P, G> {
    /// Payload-free shape used by racer-core decisions.
    pub fn kind(&self) -> ResponseKind {
        match self {
            Self::Selected { .. } => ResponseKind::Selected,
            Self::Bootstrap { .. } => ResponseKind::Bootstrap,
            Self::Page { .. } => ResponseKind::Page,
            Self::Metadata(_) => ResponseKind::Metadata,
            Self::Miss => ResponseKind::Miss,
            Self::NotFound => ResponseKind::NotFound,
            Self::VersionUnavailable => ResponseKind::VersionUnavailable,
            Self::Unavailable => ResponseKind::Unavailable,
            Self::Overloaded => ResponseKind::Overloaded,
            Self::OriginRejected => ResponseKind::OriginRejected,
            Self::OriginForbidden => ResponseKind::OriginForbidden,
            Self::StaleMembership => ResponseKind::StaleMembership,
            Self::Held { .. } => ResponseKind::Held,
            Self::Warmed => ResponseKind::Warmed,
            Self::Released => ResponseKind::Released,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kind_matches_variant() {
        let miss: PeerResponse<(), ()> = PeerResponse::Miss;
        assert_eq!(miss.kind(), ResponseKind::Miss);
        let held: PeerResponse<(), ()> = PeerResponse::Held {
            until: SystemTime::UNIX_EPOCH,
        };
        assert_eq!(held.kind(), ResponseKind::Held);
    }
}
