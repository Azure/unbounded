//! Header codec for the write peer operations (`write-session`,
//! `write-store`, `write-release`, `write-upload`) and their
//! [`WriteReply`] responses. See `designs/racer-writes.md`, "Peer operations
//! (WP4)". Like every peer request, write requests carry no page bytes: a
//! store target pulls ciphertext from the writer named by
//! [`WriteStore::source`] with a directed copy-only `page` request.

use super::bytes;
use super::field;
use super::number;
use super::push;
use super::push_binary;
use crate::error::Error;
use crate::error::Result;
use crate::model::ContentType;
use crate::model::PageSize;
use http1::MessageHead;
use racer_core::write::Durability;
use racer_core::write::HolderRanks;
use racer_core::write::Holders;
use racer_core::write::SessionId;
use racer_core::write::UploadMode;
use racer_core::write::UploadState;
use racer_core::write::WriteAction;
use racer_core::write::WriteReply;
use racer_core::write::WriteSpec;
use racer_wire::control::NodeId;

pub const WRITE_ACTION: &str = "racer-write-action";
pub const WRITE_SESSION: &str = "racer-write-session";
pub const WRITE_PAGES: &str = "racer-write-pages";
pub const WRITE_SOURCE: &str = "racer-write-source";
pub const WRITE_HOLD_UNTIL: &str = "racer-write-hold-until";
pub const WRITE_REPLICAS: &str = "racer-write-replicas";
pub const WRITE_DURABILITY: &str = "racer-write-durability";
pub const WRITE_UPLOAD: &str = "racer-write-upload";
pub const WRITE_HOLDERS: &str = "racer-write-holders";
pub const WRITE_HOLDER_RANKS: &str = "racer-write-holder-ranks";
pub const WRITE_KEEP: &str = "racer-write-keep";
pub const WRITE_WAIT_MILLIS: &str = "racer-write-wait-millis";
pub const WRITE_MANIFEST: &str = "racer-write-manifest";
pub const WRITE_RESULT: &str = "racer-write-result";
pub const WRITE_LEASE_UNTIL: &str = "racer-write-lease-until";
pub const WRITE_UPLOAD_STATE: &str = "racer-write-upload-state";
pub const WRITE_STORED: &str = "racer-write-stored";
pub const WRITE_ETAG: &str = "racer-write-etag";
pub const OBJECT_LENGTH: &str = "racer-object-length";
pub const PAGE_SIZE: &str = "racer-page-size";
pub const CONTENT_TYPE: &str = "racer-content-type";

/// `write-store`: tells a store target to pull `pages` of a session from
/// `source` (the writer, or a surviving holder during repair) and make them
/// durable until `hold_until_millis`. `length`, `page_size`, and
/// `content_type` are the version's immutable metadata, so the target can
/// validate the pulled envelopes.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct WriteStore {
    pub session: SessionId,
    /// Ascending, unique, `1..=MAX_OPERATION_PAGES` entries, inside the object.
    pub pages: Vec<u64>,
    pub length: u64,
    pub page_size: PageSize,
    pub content_type: Option<ContentType>,
    pub source: NodeId,
    pub hold_until_millis: u64,
}

/// The `racer-write-action` token.
pub fn action_name(action: &WriteAction) -> &'static str {
    match action {
        WriteAction::Open { .. } => "open",
        WriteAction::Renew { .. } => "renew",
        WriteAction::Commit { .. } | WriteAction::CommitRanked { .. } => "commit",
        WriteAction::Abort { .. } => "abort",
        WriteAction::Status { .. } => "status",
        WriteAction::Delete => "delete",
        WriteAction::Persist => "persist",
        WriteAction::Replicate { .. } | WriteAction::ReplicateRanked { .. } => "replicate",
    }
}

/// Whether `action` must name the exact version (`racer-etag`). `status`,
/// `delete`, and `persist` address the object and may omit it, since a
/// client `DELETE` or `persist` does not know the Racer version.
pub fn action_requires_etag(action: &WriteAction) -> bool {
    !matches!(
        action,
        WriteAction::Status { .. } | WriteAction::Delete | WriteAction::Persist
    )
}

/// Every page, cut at `page_size`, lies inside a nonempty object of `length`.
pub fn pages_in_object(pages: &[u64], length: u64, page_size: u64) -> bool {
    pages.last().is_some_and(|last| {
        last.checked_mul(page_size)
            .is_some_and(|start| start < length)
    })
}

fn encode_pages(head: &mut MessageHead, name: &str, pages: &[u64]) -> Result<()> {
    let encoded = racer_core::prefetch::encode_pages(pages).ok_or(Error::InvalidRequest)?;
    push_binary(head, name, &encoded);
    Ok(())
}

fn decode_pages(head: &MessageHead, name: &str) -> Result<Vec<u64>> {
    racer_core::prefetch::decode_pages(&bytes(head, name)?).ok_or(Error::InvalidRequest)
}

fn encode_session(head: &mut MessageHead, session: &SessionId) {
    push_binary(head, WRITE_SESSION, &session.0);
}

fn decode_session(head: &MessageHead) -> Result<SessionId> {
    Ok(SessionId(super::array(head, WRITE_SESSION)?))
}

fn page_size(head: &MessageHead) -> Result<PageSize> {
    PageSize::from_wire(number(head, PAGE_SIZE)?).map_err(|_| Error::InvalidRequest)
}

fn content_type(head: &MessageHead) -> Result<Option<ContentType>> {
    head.unique(CONTENT_TYPE)?
        .map(ContentType::parse)
        .transpose()
        .map_err(Into::into)
}

/// Spec checks shared by the encoder and decoder: the core invariants plus a
/// page size the object protocol accepts and a valid content type.
fn check_spec(spec: &WriteSpec) -> Result<()> {
    spec.validate().map_err(|_| Error::InvalidRequest)?;
    PageSize::from_wire(spec.page_size).map_err(|_| Error::InvalidRequest)?;
    if let Some(content_type) = &spec.content_type {
        ContentType::parse(content_type.as_bytes())?;
    }
    Ok(())
}

/// Encode the `write-session` action fields after `racer-operation`.
pub(super) fn encode_action(head: &mut MessageHead, action: &WriteAction) -> Result<()> {
    push(head, WRITE_ACTION, action_name(action));
    match action {
        WriteAction::Open { session, spec } => {
            check_spec(spec)?;
            encode_session(head, session);
            push(head, OBJECT_LENGTH, spec.length);
            push(head, PAGE_SIZE, spec.page_size);
            if let Some(content_type) = &spec.content_type {
                push(head, CONTENT_TYPE, content_type);
            }
            push(head, WRITE_REPLICAS, spec.replicas);
            push(head, WRITE_DURABILITY, spec.durability.to_header());
            push(head, WRITE_UPLOAD, spec.upload.as_str());
            push(head, WRITE_HOLD_UNTIL, spec.hold_until_millis);
        }
        WriteAction::Renew { session } | WriteAction::Abort { session } => {
            encode_session(head, session);
        }
        WriteAction::Commit { session, holders } => {
            encode_session(head, session);
            push_binary(head, WRITE_HOLDERS, &encode_holders(holders)?);
        }
        WriteAction::CommitRanked { session, ranks } => {
            encode_session(head, session);
            push_binary(head, WRITE_HOLDER_RANKS, &encode_ranks(ranks)?);
        }
        WriteAction::Status { wait_millis } => {
            if *wait_millis > racer_core::write::MAX_STATUS_WAIT_MILLIS {
                return Err(Error::InvalidRequest);
            }
            // Omitted when zero, so the encoding is canonical.
            if *wait_millis != 0 {
                push(head, WRITE_WAIT_MILLIS, wait_millis);
            }
        }
        WriteAction::Delete | WriteAction::Persist => {}
        WriteAction::Replicate { manifest } => {
            check_spec(&manifest.spec)?;
            let mut encoded = Vec::new();
            racer_core::write::journal::encode_manifest(manifest, &mut encoded)
                .map_err(|_| Error::InvalidRequest)?;
            push_binary(head, WRITE_MANIFEST, &encoded);
        }
        WriteAction::ReplicateRanked { manifest, ranks } => {
            check_spec(&manifest.spec)?;
            if !manifest.holders.0.is_empty() || ranks.pages() != manifest.spec.pages() {
                return Err(Error::InvalidRequest);
            }
            let mut encoded = Vec::new();
            racer_core::write::journal::encode_manifest_head(manifest, &mut encoded)
                .map_err(|_| Error::InvalidRequest)?;
            push_binary(head, WRITE_MANIFEST, &encoded);
            push_binary(head, WRITE_HOLDER_RANKS, &encode_ranks(ranks)?);
        }
    }
    Ok(())
}

/// A rank-relative table names at least one page and at most `MAX_PAGES`.
fn encode_ranks(ranks: &HolderRanks) -> Result<Vec<u8>> {
    if ranks.pages() == 0 || ranks.pages() > racer_core::write::MAX_PAGES {
        return Err(Error::InvalidRequest);
    }
    Ok(ranks.to_bytes())
}

fn decode_ranks(head: &MessageHead) -> Result<HolderRanks> {
    let ranks =
        HolderRanks::decode(&bytes(head, WRITE_HOLDER_RANKS)?).map_err(|_| Error::InvalidRequest)?;
    encode_ranks(&ranks)?;
    Ok(ranks)
}

/// A commit names at least one page and at least one holder per page. The
/// page count and replica floor are checked against the session by its owner.
fn encode_holders(holders: &Holders) -> Result<Vec<u8>> {
    if holders.0.is_empty()
        || holders.pages() > racer_core::write::MAX_PAGES
        || holders.0.iter().any(Vec::is_empty)
    {
        return Err(Error::InvalidRequest);
    }
    holders.to_bytes().map_err(|_| Error::InvalidRequest)
}

/// Decode the `write-session` action fields.
pub(super) fn decode_action(head: &MessageHead) -> Result<WriteAction> {
    Ok(match field(head, WRITE_ACTION)?.as_str() {
        "open" => {
            let spec = WriteSpec {
                length: number(head, OBJECT_LENGTH)?,
                page_size: page_size(head)?.get(),
                content_type: content_type(head)?.map(|c| c.as_str().to_owned()),
                replicas: u8::try_from(number(head, WRITE_REPLICAS)?)
                    .map_err(|_| Error::InvalidRequest)?,
                durability: Durability::parse(field(head, WRITE_DURABILITY)?.as_bytes())
                    .ok_or(Error::InvalidRequest)?,
                upload: UploadMode::parse(field(head, WRITE_UPLOAD)?.as_bytes())
                    .ok_or(Error::InvalidRequest)?,
                hold_until_millis: number(head, WRITE_HOLD_UNTIL)?,
            };
            check_spec(&spec)?;
            WriteAction::Open {
                session: decode_session(head)?,
                spec,
            }
        }
        "renew" => WriteAction::Renew {
            session: decode_session(head)?,
        },
        "abort" => WriteAction::Abort {
            session: decode_session(head)?,
        },
        "commit" if head.unique(WRITE_HOLDER_RANKS)?.is_some() => {
            if head.unique(WRITE_HOLDERS)?.is_some() {
                return Err(Error::InvalidRequest);
            }
            WriteAction::CommitRanked {
                session: decode_session(head)?,
                ranks: decode_ranks(head)?,
            }
        }
        "commit" => {
            let holders =
                Holders::decode(&bytes(head, WRITE_HOLDERS)?).map_err(|_| Error::InvalidRequest)?;
            encode_holders(&holders)?;
            WriteAction::Commit {
                session: decode_session(head)?,
                holders,
            }
        }
        "status" => {
            let wait_millis = match head.unique(WRITE_WAIT_MILLIS)? {
                None => 0,
                Some(_) => u32::try_from(number(head, WRITE_WAIT_MILLIS)?)
                    .ok()
                    .filter(|w| (1..=racer_core::write::MAX_STATUS_WAIT_MILLIS).contains(w))
                    .ok_or(Error::InvalidRequest)?,
            };
            WriteAction::Status { wait_millis }
        }
        "delete" => WriteAction::Delete,
        "persist" => WriteAction::Persist,
        "replicate" if head.unique(WRITE_HOLDER_RANKS)?.is_some() => {
            let manifest =
                racer_core::write::journal::decode_manifest_head(&bytes(head, WRITE_MANIFEST)?)
                    .map_err(|_| Error::InvalidRequest)?;
            check_spec(&manifest.spec)?;
            let ranks = decode_ranks(head)?;
            if !manifest.holders.0.is_empty() || ranks.pages() != manifest.spec.pages() {
                return Err(Error::InvalidRequest);
            }
            WriteAction::ReplicateRanked { manifest, ranks }
        }
        "replicate" => {
            let manifest =
                racer_core::write::journal::decode_manifest(&bytes(head, WRITE_MANIFEST)?)
                    .map_err(|_| Error::InvalidRequest)?;
            check_spec(&manifest.spec)?;
            WriteAction::Replicate { manifest }
        }
        _ => return Err(Error::InvalidRequest),
    })
}

/// Encode the `write-store` fields after `racer-operation`.
pub(super) fn encode_store(head: &mut MessageHead, store: &WriteStore) -> Result<()> {
    if !pages_in_object(&store.pages, store.length, store.page_size.get()) {
        return Err(Error::InvalidRequest);
    }
    encode_pages(head, WRITE_PAGES, &store.pages)?;
    push(head, OBJECT_LENGTH, store.length);
    push(head, PAGE_SIZE, store.page_size.get());
    if let Some(content_type) = &store.content_type {
        push(head, CONTENT_TYPE, content_type.as_str());
    }
    super::uuid(&store.source.0)?;
    push(head, WRITE_SOURCE, &store.source.0);
    encode_session(head, &store.session);
    push(head, WRITE_HOLD_UNTIL, store.hold_until_millis);
    Ok(())
}

/// Decode the `write-store` fields.
pub(super) fn decode_store(head: &MessageHead) -> Result<WriteStore> {
    let store = WriteStore {
        pages: decode_pages(head, WRITE_PAGES)?,
        length: number(head, OBJECT_LENGTH)?,
        page_size: page_size(head)?,
        content_type: content_type(head)?,
        source: super::node_field(head, WRITE_SOURCE)?,
        session: decode_session(head)?,
        hold_until_millis: number(head, WRITE_HOLD_UNTIL)?,
    };
    if !pages_in_object(&store.pages, store.length, store.page_size.get()) {
        return Err(Error::InvalidRequest);
    }
    Ok(store)
}

/// Encode the `write-release` and `write-upload` fields.
pub(super) fn encode_session_pages(
    head: &mut MessageHead,
    session: &SessionId,
    pages: &[u64],
) -> Result<()> {
    encode_pages(head, WRITE_PAGES, pages)?;
    encode_session(head, session);
    Ok(())
}

/// Decode the `write-release` and `write-upload` fields.
pub(super) fn decode_session_pages(head: &MessageHead) -> Result<(SessionId, Vec<u64>)> {
    Ok((decode_session(head)?, decode_pages(head, WRITE_PAGES)?))
}

fn check_etag(etag: &str) -> Result<()> {
    crate::model::StrongEtag::parse(etag.as_bytes())?;
    Ok(())
}

/// Encode a [`WriteReply`] after the response prefix.
pub(super) fn encode_reply(head: &mut MessageHead, reply: &WriteReply) -> Result<()> {
    push(head, WRITE_RESULT, reply.result());
    match reply {
        WriteReply::Opened { lease_until_millis } => {
            push(head, WRITE_LEASE_UNTIL, lease_until_millis);
        }
        WriteReply::Stored { pages } | WriteReply::Uploaded { pages } => {
            encode_pages(head, WRITE_STORED, pages)?;
        }
        WriteReply::Committed { state } => push(head, WRITE_UPLOAD_STATE, state.as_str()),
        WriteReply::Status { state, etag } => {
            check_etag(etag)?;
            push(head, WRITE_UPLOAD_STATE, state.as_str());
            push(head, WRITE_ETAG, etag);
        }
        WriteReply::Released
        | WriteReply::Exists
        | WriteReply::Full
        | WriteReply::LeaseExpired
        | WriteReply::Unknown
        | WriteReply::Failed => {}
    }
    Ok(())
}

fn upload_state(head: &MessageHead) -> Result<UploadState> {
    UploadState::parse(field(head, WRITE_UPLOAD_STATE)?.as_bytes()).ok_or(Error::InvalidRequest)
}

/// Decode a [`WriteReply`] from a `write` outcome head.
pub(super) fn decode_reply(head: &MessageHead) -> Result<WriteReply> {
    Ok(match field(head, WRITE_RESULT)?.as_str() {
        "opened" => WriteReply::Opened {
            lease_until_millis: number(head, WRITE_LEASE_UNTIL)?,
        },
        "stored" => WriteReply::Stored {
            pages: decode_pages(head, WRITE_STORED)?,
        },
        "uploaded" => WriteReply::Uploaded {
            pages: decode_pages(head, WRITE_STORED)?,
        },
        "committed" => WriteReply::Committed {
            state: upload_state(head)?,
        },
        "status" => {
            let etag = field(head, WRITE_ETAG)?;
            check_etag(&etag)?;
            WriteReply::Status {
                state: upload_state(head)?,
                etag,
            }
        }
        "released" => WriteReply::Released,
        "exists" => WriteReply::Exists,
        "full" => WriteReply::Full,
        "lease-expired" => WriteReply::LeaseExpired,
        "unknown" => WriteReply::Unknown,
        "failed" => WriteReply::Failed,
        _ => return Err(Error::InvalidRequest),
    })
}
