//! Peer responses generic over their page payload.
//!
//! The dataplane instantiates [`PeerResponse`] with real ciphertext pages and
//! transfer grants; the simulator uses [`PhantomPage`] and `()`, which carry
//! only the identity and length decisions need.

use super::candidates::{OperationKind, ProbeOutcome, ResponseKind, classify};
use super::error::{Error, Result};
use racer_object_wire::model::{ObjectMetadata, PageId};
use std::time::SystemTime;

/// What read decisions need from a page payload.
pub trait Payload {
    /// Ciphertext bytes carried.
    fn len(&self) -> usize;

    /// Whether the payload carries no bytes.
    fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Page identity the payload claims.
    fn page_id(&self) -> &PageId;
}

/// Data-free payload for simulation: identity plus length.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PhantomPage {
    pub page: PageId,
    pub len: usize,
}

impl Payload for PhantomPage {
    fn len(&self) -> usize {
        self.len
    }

    fn page_id(&self) -> &PageId {
        &self.page
    }
}

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

    /// Payload bytes carried by the response.
    pub fn payload_len(&self) -> usize
    where
        P: Payload,
    {
        match self {
            Self::Selected { ciphertext, .. } | Self::Page { ciphertext, .. } => ciphertext.len(),
            Self::Bootstrap {
                page_zero: Some(page),
                ..
            } => page.len(),
            _ => 0,
        }
    }
}

/// Classify with payload identity checks only (no content authentication),
/// for payloads without bytes. Identity mismatches are corrupt records.
pub fn classify_identity<P: Payload, G>(
    response: &PeerResponse<P, G>,
    operation: OperationKind,
    expected: Option<&PageId>,
    acquire: bool,
) -> Result<Option<ProbeOutcome>> {
    classify(response.kind(), operation, acquire, || {
        let claimed = match response {
            PeerResponse::Selected { ciphertext, .. } | PeerResponse::Page { ciphertext, .. } => {
                Some(ciphertext.page_id())
            }
            _ => None,
        };
        match (claimed, expected) {
            (Some(claimed), Some(expected)) if claimed != expected => Err(Error::CorruptRecord),
            _ => Ok(()),
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use racer_control_wire::CacheId;
    use racer_object_wire::model::{
        CacheKey, ExpiresAt, ObjectId, ObjectVersion, PageNumber, StrongEtag,
    };

    fn page(number: u64) -> PageId {
        PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("c".into()),
                    key: CacheKey([1; 32]),
                },
                etag: StrongEtag::test_value("v"),
            },
            number: PageNumber(number),
        }
    }

    fn metadata() -> ObjectMetadata {
        ObjectMetadata {
            content_type: None,
            version: page(0).version,
            length: 10,
            expires_at: ExpiresAt::test_time(SystemTime::UNIX_EPOCH),
        }
    }

    type Phantom = PeerResponse<PhantomPage, ()>;

    #[test]
    fn phantom_responses_classify_by_kind_and_identity() {
        let ok: Phantom = PeerResponse::Page {
            metadata: metadata(),
            ciphertext: PhantomPage {
                page: page(1),
                len: 7,
            },
        };
        assert_eq!(ok.kind(), ResponseKind::Page);
        assert_eq!(ok.payload_len(), 7);
        assert_eq!(
            classify_identity(&ok, OperationKind::Page, Some(&page(1)), true),
            Ok(None)
        );
        assert_eq!(
            classify_identity(&ok, OperationKind::Page, Some(&page(2)), true),
            Err(Error::CorruptRecord)
        );
        assert_eq!(
            classify_identity(&ok, OperationKind::Metadata { fresh: false }, None, true),
            Err(Error::CorruptRecord)
        );
        let miss: Phantom = PeerResponse::Miss;
        assert_eq!(miss.payload_len(), 0);
        assert_eq!(
            classify_identity(&miss, OperationKind::Page, None, true),
            Ok(Some(ProbeOutcome::CopyMiss))
        );
        let held: Phantom = PeerResponse::Held {
            until: SystemTime::UNIX_EPOCH,
        };
        assert_eq!(held.kind(), ResponseKind::Held);
        let boot: Phantom = PeerResponse::Bootstrap {
            metadata: metadata(),
            page_zero: Some(PhantomPage {
                page: page(0),
                len: 10,
            }),
        };
        assert_eq!(boot.payload_len(), 10);
        assert!(!boot.kind().has_payload() || boot.kind() == ResponseKind::Bootstrap);
    }
}
