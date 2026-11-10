//! Sans-IO write-back caching types: write specs, the coordinator session state
//! machine, the write journal codec, holder tables, and upload planning.
//!
//! See `designs/racer-writes.md`. Nothing here performs IO, reads clocks, or
//! draws randomness; callers pass `now_millis` in.

pub mod holders;
pub mod journal;
pub mod ranks;
pub mod session;
pub mod upload;

pub use holders::{HolderError, Holders, NodeIdBytes};
pub use journal::{JournalError, JournalRecord};
pub use ranks::{HolderRanks, MAX_RANK_DEPTH};
pub use racer_wire::object::write::{
    Durability, MAX_REPLICAS, MIN_REPLICAS, UploadMode, UploadState,
};
pub use session::{Phase, Rejection, Session, SessionEffect, SessionEvent, WriteIdentity};
pub use upload::{UploadPlan, block_id, block_list_xml, block_list_xml_from_ids};

/// Azure Blob allows at most this many committed blocks per blob, and writes
/// use one block per page.
pub const MAX_PAGES: u64 = 50_000;

/// Writer-chosen random session identifier.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash, PartialOrd, Ord)]
pub struct SessionId(pub [u8; 16]);

/// Parameters fixed when a write session opens.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WriteSpec {
    /// Object length in bytes, nonzero.
    pub length: u64,

    /// Page size in bytes, a nonzero power of two.
    pub page_size: u64,

    pub content_type: Option<String>,

    /// Copies required on every page before commit, `1..=MAX_REPLICAS`.
    pub replicas: u8,

    pub durability: Durability,

    pub upload: UploadMode,

    pub hold_until_millis: u64,
}

/// Why a [`WriteSpec`] is not acceptable.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SpecError {
    EmptyObject,
    PageSize,
    TooManyPages,
    Replicas,
    /// `Durability::Replicas(n)` with `n != replicas`.
    DurabilityMismatch,
    /// `Durability::Origin` with an upload mode other than `Auto`.
    OriginWithoutUpload,
}

impl WriteSpec {
    /// Number of pages, `ceil(length / page_size)`. Zero when the page size is zero.
    pub fn pages(&self) -> u64 {
        if self.page_size == 0 {
            return 0;
        }
        self.length.div_ceil(self.page_size)
    }

    /// Check every invariant the session and codecs rely on.
    pub fn validate(&self) -> Result<(), SpecError> {
        if self.length == 0 {
            return Err(SpecError::EmptyObject);
        }
        if !self.page_size.is_power_of_two() {
            return Err(SpecError::PageSize);
        }
        if self.pages() > MAX_PAGES {
            return Err(SpecError::TooManyPages);
        }
        if !(MIN_REPLICAS..=MAX_REPLICAS).contains(&self.replicas) {
            return Err(SpecError::Replicas);
        }
        match self.durability {
            Durability::Replicas(n) if n != self.replicas => Err(SpecError::DurabilityMismatch),
            Durability::Origin if self.upload != UploadMode::Auto => {
                Err(SpecError::OriginWithoutUpload)
            }
            _ => Ok(()),
        }
    }
}

/// Peer write-session action, decoded from `racer-write-action`.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WriteAction {
    Open {
        session: SessionId,
        spec: WriteSpec,
    },
    Renew {
        session: SessionId,
    },
    Commit {
        session: SessionId,
        holders: Holders,
    },
    /// `commit` with a rank-relative holder table, for objects whose explicit
    /// table does not fit a peer head. The coordinator resolves it against
    /// the request's membership.
    CommitRanked {
        session: SessionId,
        ranks: HolderRanks,
    },
    Abort {
        session: SessionId,
    },
    Status {
        wait_millis: u32,
    },
    Delete,
    Persist,
    Replicate {
        manifest: Manifest,
    },
    /// `replicate` with a rank-relative holder table; `manifest.holders` is
    /// empty and is resolved by the receiver.
    ReplicateRanked {
        manifest: Manifest,
        ranks: HolderRanks,
    },
}

/// Maximum `racer-write-wait-millis`.
pub const MAX_STATUS_WAIT_MILLIS: u32 = 30_000;

/// The coordinator's durable record of a committed write.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Manifest {
    pub session: SessionId,

    pub etag: String,

    pub spec: WriteSpec,

    pub holders: Holders,

    pub state: UploadState,

    pub blob_etag: Option<String>,
}

/// Reply to a write peer operation.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WriteReply {
    Opened { lease_until_millis: u64 },
    Stored { pages: Vec<u64> },
    Committed { state: UploadState },
    Released,
    Uploaded { pages: Vec<u64> },
    Status { state: UploadState, etag: String },
    Exists,
    Full,
    LeaseExpired,
    Unknown,
    Failed,
}

impl WriteReply {
    /// The `racer-write-result` token.
    pub fn result(&self) -> &'static str {
        match self {
            Self::Opened { .. } => "opened",
            Self::Stored { .. } => "stored",
            Self::Committed { .. } => "committed",
            Self::Released => "released",
            Self::Uploaded { .. } => "uploaded",
            Self::Status { .. } => "status",
            Self::Exists => "exists",
            Self::Full => "full",
            Self::LeaseExpired => "lease-expired",
            Self::Unknown => "unknown",
            Self::Failed => "failed",
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    pub(crate) fn spec() -> WriteSpec {
        WriteSpec {
            length: 10,
            page_size: 4,
            content_type: Some("application/x-test".into()),
            replicas: 2,
            durability: Durability::Replicas(2),
            upload: UploadMode::Auto,
            hold_until_millis: 1_000_000,
        }
    }

    #[test]
    fn spec_pages_and_validation() {
        let s = spec();
        assert_eq!(s.pages(), 3);
        assert_eq!(s.validate(), Ok(()));
        let exact = WriteSpec {
            length: 8,
            ..spec()
        };
        assert_eq!(exact.pages(), 2);
        let cases = [
            (
                WriteSpec {
                    length: 0,
                    ..spec()
                },
                SpecError::EmptyObject,
            ),
            (
                WriteSpec {
                    page_size: 0,
                    ..spec()
                },
                SpecError::PageSize,
            ),
            (
                WriteSpec {
                    page_size: 3,
                    ..spec()
                },
                SpecError::PageSize,
            ),
            (
                WriteSpec {
                    length: MAX_PAGES * 4 + 1,
                    ..spec()
                },
                SpecError::TooManyPages,
            ),
            (
                WriteSpec {
                    replicas: 0,
                    ..spec()
                },
                SpecError::Replicas,
            ),
            (
                WriteSpec {
                    replicas: 4,
                    ..spec()
                },
                SpecError::Replicas,
            ),
            (
                WriteSpec {
                    durability: Durability::Replicas(3),
                    ..spec()
                },
                SpecError::DurabilityMismatch,
            ),
            (
                WriteSpec {
                    durability: Durability::Origin,
                    upload: UploadMode::Manual,
                    ..spec()
                },
                SpecError::OriginWithoutUpload,
            ),
        ];
        for (s, error) in cases {
            assert_eq!(s.validate(), Err(error), "{s:?}");
        }
        let boundary = WriteSpec {
            length: MAX_PAGES * 4,
            ..spec()
        };
        assert_eq!(boundary.validate(), Ok(()));
        let origin = WriteSpec {
            durability: Durability::Origin,
            ..spec()
        };
        assert_eq!(origin.validate(), Ok(()));
    }

    #[test]
    fn reply_tokens() {
        assert_eq!(WriteReply::LeaseExpired.result(), "lease-expired");
        assert_eq!(WriteReply::Stored { pages: vec![] }.result(), "stored");
    }
}
