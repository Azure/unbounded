//! Coordinator: the write session of each object, on the worker that owns
//! its page zero, plus write-operation dispatch and periodic maintenance.

use super::OPERATION_PAGES;
use super::RANKED_DEPTH;
use super::directory::Entry;
use super::directory::Role;
use super::now_millis;
use super::send;
use crate::error::Error;
use crate::error::Result;
use crate::model::MetadataSelector;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::StrongEtag;
use crate::model::VersionMetadata;
use crate::peer::protocol::Operation as PeerOperation;
use crate::read::Coordinator;
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use crate::topology::Membership;
use racer_core::write::HolderRanks;
use racer_core::write::Holders;
use racer_core::write::JournalRecord;
use racer_core::write::Manifest;
use racer_core::write::NodeIdBytes;
use racer_core::write::Phase;
use racer_core::write::Rejection;
use racer_core::write::Session;
use racer_core::write::SessionEffect;
use racer_core::write::SessionEvent;
use racer_core::write::SessionId;
use racer_core::write::UploadState;
use racer_core::write::WriteAction;
use racer_core::write::WriteIdentity;
use racer_core::write::WriteReply;
use racer_core::write::WriteSpec;
use racer_wire::control::NodeId;
use std::sync::Arc;
use std::time::Duration;

/// Longest a background coordinator task (release, upload, commit) runs.
const BACKGROUND_TIMEOUT: Duration = Duration::from_secs(600);

/// Poll interval of a `status` long poll.
const STATUS_POLL: Duration = Duration::from_millis(50);

/// Holder pages dropped at hold expiry per maintenance turn.
const EXPIRY_BATCH: usize = 64;

/// Largest explicit holder table sent in a peer head; larger tables use the
/// rank-relative encoding (`racer_core::write::HolderRanks`).
pub const MAX_EXPLICIT_HOLDER_BYTES: usize = 12 * 1024;

/// Largest rank-relative holder table sent in a peer head.
pub const MAX_RANKED_HOLDER_BYTES: usize = 16 * 1024;

/// Serve one write peer operation on this, the object's metadata worker.
/// `requester` is the sending node (this node for in-process calls).
pub fn serve<'a>(
    local: &'a Coordinator,
    requester: &'a NodeId,
    operation: PeerOperation,
    context: &'a OriginContext,
    membership: Arc<Membership>,
    scope: &'a RequestScope,
) -> crate::error::Operation<'a, WriteReply> {
    Box::pin(serve_inner(
        local, requester, operation, context, membership, scope,
    ))
}

async fn serve_inner(
    local: &Coordinator,
    requester: &NodeId,
    operation: PeerOperation,
    context: &OriginContext,
    membership: Arc<Membership>,
    scope: &RequestScope,
) -> Result<WriteReply> {
    scope.check()?;
    let _ = requester;
    load(local);
    match operation {
        PeerOperation::WriteSession {
            object,
            etag,
            action,
        } => session(local, object, etag, *action, context, membership, scope).await,
        PeerOperation::WriteStore { version, store } => {
            super::store_target::serve_store(local, version, *store, membership, scope).await
        }
        PeerOperation::WriteRelease {
            version,
            session,
            pages,
            keep,
        } => super::store_target::serve_release(local, version, session, pages, keep, scope).await,
        PeerOperation::WriteUpload {
            version,
            session,
            pages,
        } => super::upload::serve_upload(local, version, session, pages, context, scope).await,
        _ => Err(Error::InvalidRequest),
    }
}

/// Whether `object`'s cache accepts writes on this node.
pub(crate) fn writes_enabled(local: &Coordinator, object: &ObjectId) -> bool {
    local.availability.writes(&object.cache).is_some()
        && !local.availability.passthrough(&object.cache)
}

fn etag_text(etag: &StrongEtag) -> Result<String> {
    String::from_utf8(etag.as_bytes().to_vec()).map_err(|_| Error::InvalidRequest)
}

#[allow(clippy::too_many_arguments)]
async fn session(
    local: &Coordinator,
    object: ObjectId,
    etag: Option<StrongEtag>,
    action: WriteAction,
    context: &OriginContext,
    membership: Arc<Membership>,
    scope: &RequestScope,
) -> Result<WriteReply> {
    let directory = local.writes.directory().clone();
    let version = etag.map(|etag| ObjectVersion {
        object: object.clone(),
        etag,
    });
    match action {
        WriteAction::Open { session, spec } => {
            let version = version.ok_or(Error::InvalidRequest)?;
            open(local, version, session, spec, context, membership, scope).await
        }
        WriteAction::Renew { session } => {
            let version = version.ok_or(Error::InvalidRequest)?;
            let Some(effects) = event(local, &version, session, SessionEvent::Renewed) else {
                return Ok(WriteReply::Unknown);
            };
            let rejection = apply(local, &object, effects, &membership, scope).await?;
            settle(local, &object);
            Ok(match rejection {
                Some(Rejection::LeaseExpired) => WriteReply::LeaseExpired,
                Some(_) => WriteReply::Unknown,
                None => directory
                    .with(&object, |e| e.and_then(|e| e.session.lease_until_millis()))
                    .map_or(WriteReply::Unknown, |lease_until_millis| {
                        WriteReply::Opened { lease_until_millis }
                    }),
            })
        }
        WriteAction::Commit { session, holders } => {
            let version = version.ok_or(Error::InvalidRequest)?;
            commit(local, &version, session, holders, &membership, scope).await
        }
        WriteAction::CommitRanked { session, ranks } => {
            let version = version.ok_or(Error::InvalidRequest)?;
            let Some(pages) = directory.with(&object, |e| {
                e.filter(|e| e.version == version)
                    .map(|e| e.session.spec().pages())
            }) else {
                return Ok(WriteReply::Unknown);
            };
            if ranks.pages() != pages {
                return Ok(WriteReply::Failed);
            }
            let holders = match resolve_ranks(local, &membership, &object, &ranks, scope).await {
                Ok(holders) => holders,
                Err(Error::InvalidRequest) => return Ok(WriteReply::Failed),
                Err(error) => return Err(error),
            };
            commit(local, &version, session, holders, &membership, scope).await
        }
        WriteAction::Abort { session } => {
            let version = version.ok_or(Error::InvalidRequest)?;
            let Some(effects) = event(local, &version, session, SessionEvent::Aborted) else {
                return Ok(WriteReply::Unknown);
            };
            apply(local, &object, effects, &membership, scope).await?;
            settle(local, &object);
            Ok(WriteReply::Released)
        }
        WriteAction::Status { wait_millis } => {
            status(local, &object, version.as_ref(), wait_millis, scope).await
        }
        WriteAction::Delete => delete(local, &object, version.as_ref(), &membership, scope).await,
        WriteAction::Persist => persist(local, &object, context, &membership, scope).await,
        WriteAction::Replicate { manifest } => {
            let version = version.ok_or(Error::InvalidRequest)?;
            replicate(local, version, manifest, context, &membership, scope).await
        }
        WriteAction::ReplicateRanked {
            mut manifest,
            ranks,
        } => {
            let version = version.ok_or(Error::InvalidRequest)?;
            manifest.holders = match resolve_ranks(local, &membership, &object, &ranks, scope).await
            {
                Ok(holders) => holders,
                Err(Error::InvalidRequest) => return Ok(WriteReply::Failed),
                Err(error) => return Err(error),
            };
            replicate(local, version, manifest, context, &membership, scope).await
        }
    }
}

/// Apply `event` to the session of `version` if it is the live session
/// `session`; `None` when there is no such session.
fn event(
    local: &Coordinator,
    version: &ObjectVersion,
    session: SessionId,
    event: SessionEvent,
) -> Option<Vec<SessionEffect>> {
    let now = now_millis(local);
    local.writes.directory().with(&version.object, |entry| {
        let entry = entry?;
        if entry.version != *version || entry.session.identity().session != session {
            return None;
        }
        Some(entry.session.on_event(event, now))
    })
}

/// Apply `event` to whatever session `object` has.
pub(crate) fn object_event(
    local: &Coordinator,
    object: &ObjectId,
    event: SessionEvent,
) -> Option<Vec<SessionEffect>> {
    let now = now_millis(local);
    local
        .writes
        .directory()
        .with(object, |entry| Some(entry?.session.on_event(event, now)))
}

/// Drop a terminal session from the directory.
fn settle(local: &Coordinator, object: &ObjectId) {
    let directory = local.writes.directory();
    if directory.with(object, |e| e.is_some_and(|e| e.session.is_terminal())) {
        directory.remove(object);
    }
}

#[allow(clippy::too_many_arguments)]
async fn open(
    local: &Coordinator,
    version: ObjectVersion,
    session: SessionId,
    spec: WriteSpec,
    context: &OriginContext,
    membership: Arc<Membership>,
    scope: &RequestScope,
) -> Result<WriteReply> {
    let object = version.object.clone();
    if !writes_enabled(local, &object) || spec.page_size != local.page_size()?.get() {
        return Ok(WriteReply::Failed);
    }
    let directory = local.writes.directory().clone();
    // A retried open of the same session is idempotent; any other live
    // write of the object is a create-once conflict.
    if let Some(reply) = directory.with(&object, |entry| {
        entry.map(|entry| {
            match (
                entry.version == version && entry.session.identity().session == session,
                entry.session.lease_until_millis(),
            ) {
                (true, Some(lease_until_millis)) => WriteReply::Opened { lease_until_millis },
                _ => WriteReply::Exists,
            }
        })
    }) {
        return Ok(reply);
    }
    let settings = local.writes.settings();
    let lease = u64::try_from(settings.lease.as_millis()).unwrap_or(u64::MAX);
    let identity = WriteIdentity {
        cache: object.cache.0.clone(),
        key: object.key.0,
        etag: etag_text(&version.etag)?,
        session,
    };
    let opened = Session::open(identity, spec, now_millis(local), lease);
    let record = opened.open_record();
    let lease_until_millis = opened.lease_until_millis().unwrap_or(0);
    // Claim the object before awaiting anything, so a concurrent open of the
    // same key sees it.
    if !directory.insert(
        object.clone(),
        Entry::new(
            Role::Coordinator,
            version.clone(),
            opened,
            Some(super::duplicate_context(context)?),
        ),
    ) {
        return Ok(WriteReply::Exists);
    }
    // Create-once against the origin only matters when the object can reach
    // it. A `never` write without `Racer-Metadata` cannot (the client check
    // requires metadata for every other mode), so only the cache-level
    // create-once above applies.
    if super::native_origin(local, &object.cache) && context.metadata.is_some() {
        match origin_has(local, context, membership, scope).await {
            Ok(false) => {}
            Ok(true) => {
                directory.remove_version(&version);
                return Ok(WriteReply::Exists);
            }
            Err(_) => {
                directory.remove_version(&version);
                return Ok(WriteReply::Failed);
            }
        }
    }
    if let Err(error) = journal(local, record).await {
        directory.remove_version(&version);
        return Err(error);
    }
    Ok(WriteReply::Opened { lease_until_millis })
}

/// Whether the origin already has `context.object` (create-once).
async fn origin_has(
    local: &Coordinator,
    context: &OriginContext,
    membership: Arc<Membership>,
    scope: &RequestScope,
) -> Result<bool> {
    let authority = local.fill.dependencies.candidates.write_authority(
        membership,
        &context.object,
        PageNumber(0),
    )?;
    match super::origin(local)
        .metadata(&authority, context, MetadataSelector::Fresh, scope)
        .await
    {
        Ok(_) => Ok(true),
        Err(Error::NotFound) => Ok(false),
        Err(error) => Err(error),
    }
}

async fn commit(
    local: &Coordinator,
    version: &ObjectVersion,
    session: SessionId,
    holders: Holders,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<WriteReply> {
    let object = &version.object;
    let Some(effects) = event(local, version, session, SessionEvent::Committed(holders)) else {
        return Ok(WriteReply::Unknown);
    };
    match apply(local, object, effects, membership, scope).await? {
        Some(Rejection::LeaseExpired) => {
            settle(local, object);
            return Ok(WriteReply::LeaseExpired);
        }
        Some(Rejection::InvalidHolders) => return Ok(WriteReply::Failed),
        Some(_) => return Ok(WriteReply::Unknown),
        None => {}
    }
    replicate_manifest(local, object, membership, scope).await;
    let state = local
        .writes
        .directory()
        .with(object, |e| e.map(|e| e.session.state()))
        .ok_or(Error::Unavailable)?;
    Ok(WriteReply::Committed { state })
}

/// Send the manifest, with the upload credentials when this worker has
/// them, to the next `replicas - 1` members of the page-zero ranking. Best
/// effort: the coordinator's journal is already durable.
pub(crate) async fn replicate_manifest(
    local: &Coordinator,
    object: &ObjectId,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) {
    let me = local.local_node();
    let Ok(ranked) = ranked(local, membership, object, 0, scope).await else {
        return;
    };
    let Some(replicas) = local.writes.directory().with(object, |e| {
        e.map(|e| usize::from(e.session.spec().replicas.saturating_sub(1)))
    }) else {
        return;
    };
    let members: Vec<NodeId> = ranked
        .into_iter()
        .filter(|node| membership.serves(&object.cache, node))
        .collect();
    let mut replicas: Vec<NodeId> = members
        .iter()
        .filter(|node| **node != me)
        .take(replicas)
        .cloned()
        .collect();
    // Page zero's rank-0 member always learns the manifest, even with
    // `replicas=1`: after a node joins ahead of this coordinator it is the
    // member readers ask first, and failover hands it the coordinator role.
    if let Some(first) = members.first()
        && *first != me
        && !replicas.contains(first)
    {
        replicas.push(first.clone());
    }
    send_manifest(local, object, &replicas, membership, scope).await;
}

/// Send this worker's manifest of `object` (and its upload credentials,
/// when it has them) to `nodes`.
pub(crate) async fn send_manifest(
    local: &Coordinator,
    object: &ObjectId,
    nodes: &[NodeId],
    membership: &Arc<Membership>,
    scope: &RequestScope,
) {
    if nodes.is_empty() {
        return;
    }
    let Some((version, manifest, credentials)) = local.writes.directory().with(object, |e| {
        e.map(|e| {
            (
                e.version.clone(),
                e.session.manifest(),
                e.context
                    .as_ref()
                    .and_then(|c| super::duplicate_context(c).ok()),
            )
        })
    }) else {
        return;
    };
    let Some(action) = manifest_action(local, membership, object, manifest, scope).await else {
        return;
    };
    let context = credentials.unwrap_or_else(|| super::bare_context(object));
    let context = &context;
    let sends = nodes.iter().map(|node| {
        let operation = PeerOperation::WriteSession {
            object: object.clone(),
            etag: Some(version.etag.clone()),
            action: Box::new(action.clone()),
        };
        send(local, membership, node, operation, context, scope)
    });
    let _ = futures::future::join_all(sends).await;
}

/// `replicate`, with an explicit holder table when it is small and a
/// rank-relative one otherwise; `None` when neither fits a peer head.
async fn manifest_action(
    local: &Coordinator,
    membership: &Arc<Membership>,
    object: &ObjectId,
    manifest: Manifest,
    scope: &RequestScope,
) -> Option<WriteAction> {
    let explicit = manifest.holders.to_bytes().ok()?.len();
    if explicit <= MAX_EXPLICIT_HOLDER_BYTES {
        return Some(WriteAction::Replicate { manifest });
    }
    let ranks = rank_holders(local, membership, object, &manifest.holders, scope)
        .await
        .ok()??;
    if ranks.to_bytes().len() > MAX_RANKED_HOLDER_BYTES {
        return None;
    }
    Some(WriteAction::ReplicateRanked {
        manifest: Manifest {
            holders: Holders::default(),
            ..manifest
        },
        ranks,
    })
}

/// The first [`RANKED_DEPTH`] members of `page`'s placement.
pub(crate) async fn ranked(
    local: &Coordinator,
    membership: &Arc<Membership>,
    object: &ObjectId,
    page: u64,
    scope: &RequestScope,
) -> Result<Vec<NodeId>> {
    let mut ordered = local
        .fill
        .dependencies
        .candidates
        .candidates_scoped(membership.clone(), object, PageNumber(page), scope)
        .await?
        .ordered;
    ordered.truncate(RANKED_DEPTH);
    Ok(ordered)
}

fn node_bytes(nodes: &[NodeId]) -> Vec<NodeIdBytes> {
    nodes.iter().map(|n| n.0.clone().into_bytes()).collect()
}

/// Rank-relative masks of `holders`; `None` if a holder is outside its
/// page's ranking.
pub(crate) async fn rank_holders(
    local: &Coordinator,
    membership: &Arc<Membership>,
    object: &ObjectId,
    holders: &Holders,
    scope: &RequestScope,
) -> Result<Option<HolderRanks>> {
    let mut ranks = HolderRanks::new(RANKED_DEPTH as u8).map_err(|_| Error::Internal)?;
    for (page, nodes) in holders.0.iter().enumerate() {
        let ordered = node_bytes(&ranked(local, membership, object, page as u64, scope).await?);
        let Some(page_ranks) =
            HolderRanks::from_holders(RANKED_DEPTH as u8, &Holders(vec![nodes.clone()]), |_| {
                &ordered
            })
            .map_err(|_| Error::InvalidRequest)?
        else {
            return Ok(None);
        };
        for mask in page_ranks.masks() {
            ranks.push(mask).map_err(|_| Error::InvalidRequest)?;
        }
    }
    Ok(Some(ranks))
}

/// Resolve a rank-relative table under `membership`.
async fn resolve_ranks(
    local: &Coordinator,
    membership: &Arc<Membership>,
    object: &ObjectId,
    ranks: &HolderRanks,
    scope: &RequestScope,
) -> Result<Holders> {
    let mut holders = Vec::with_capacity(ranks.pages() as usize);
    for (page, mask) in ranks.masks().enumerate() {
        let ordered = node_bytes(&ranked(local, membership, object, page as u64, scope).await?);
        holders.push(HolderRanks::resolve_page(mask, &ordered).map_err(|_| Error::InvalidRequest)?);
    }
    Ok(Holders(holders))
}

/// Upload state of `object`'s session. When `version` names the Blob ETag
/// the session's upload committed as (the ETag a fresh `HEAD` sees once the
/// origin has the object), the reply is `Committed` under that ETag, so a
/// client polling a written key keeps seeing `committed` after the switch.
async fn status(
    local: &Coordinator,
    object: &ObjectId,
    version: Option<&ObjectVersion>,
    wait_millis: u32,
    scope: &RequestScope,
) -> Result<WriteReply> {
    if let Some(version) = version.filter(|v| !super::is_written_etag(&v.etag)) {
        let committed = local.writes.directory().with(object, |entry| {
            entry.is_some_and(|e| {
                e.session.state() == UploadState::Committed
                    && e.session
                        .blob_etag()
                        .is_some_and(|blob| blob.as_bytes() == version.etag.as_bytes())
            })
        });
        return Ok(if committed {
            WriteReply::Status {
                state: UploadState::Committed,
                etag: etag_text(&version.etag)?,
            }
        } else {
            WriteReply::Unknown
        });
    }
    let clock = scope.clock().clone();
    let until = clock.now() + Duration::from_millis(u64::from(wait_millis));
    loop {
        let reply = local.writes.directory().with(object, |entry| {
            entry.and_then(|e| {
                Some((
                    e.session.state(),
                    etag_text(&e.version.etag).ok()?,
                    e.session.phase(),
                ))
            })
        });
        let Some((state, etag, phase)) = reply else {
            return Ok(WriteReply::Unknown);
        };
        let settled = !matches!(phase, Phase::Committed)
            || !matches!(state, UploadState::Pending | UploadState::Uploading);
        let now = clock.now();
        if wait_millis == 0 || settled || now >= until {
            return Ok(WriteReply::Status { state, etag });
        }
        let Some(reactor) = local.writes.reactor() else {
            return Ok(WriteReply::Status { state, etag });
        };
        reactor
            .sleep_until((now + STATUS_POLL).min(until), scope)
            .await?;
    }
}

async fn delete(
    local: &Coordinator,
    object: &ObjectId,
    version: Option<&ObjectVersion>,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<WriteReply> {
    let directory = local.writes.directory().clone();
    let found = directory.with(object, |e| {
        e.filter(|e| version.is_none_or(|v| *v == e.version))
            .map(|e| (e.role, e.version.clone()))
    });
    let Some((role, version)) = found else {
        return Ok(WriteReply::Unknown);
    };
    if role == Role::Replica {
        // The coordinator released the holders; drop the replica.
        directory.remove_version(&version);
        local
            .fill
            .dependencies
            .writer
            .index()
            .invalidate_current(object);
        journal(local, delete_record(&version)?).await?;
        return Ok(WriteReply::Released);
    }
    let Some(effects) = object_event(local, object, SessionEvent::DeleteRequested) else {
        return Ok(WriteReply::Unknown);
    };
    // Tell manifest replicas before the effects consume the session.
    let replicas = replica_nodes(local, membership, object, scope).await;
    match apply(local, object, effects, membership, scope).await? {
        Some(Rejection::Uploaded) => {
            return Ok(WriteReply::Status {
                state: UploadState::Committed,
                etag: etag_text(&version.etag)?,
            });
        }
        Some(_) => return Ok(WriteReply::Unknown),
        None => {}
    }
    settle(local, object);
    let context = super::bare_context(object);
    for node in replicas {
        let operation = PeerOperation::WriteSession {
            object: object.clone(),
            etag: Some(version.etag.clone()),
            action: Box::new(WriteAction::Delete),
        };
        let _ = send(local, membership, &node, operation, &context, scope).await;
    }
    Ok(WriteReply::Released)
}

/// Every member that may hold a manifest of `object`, other than this node:
/// the serving page-zero members among the first `RANKED_DEPTH`. Besides the
/// `replicas - 1` manifest replicas, a member that joined ahead of this
/// coordinator got the manifest (`replicate_manifest`), and a coordinator
/// that this one took over from may still act as one until its next role
/// check; a delete must reach them all, or they keep serving the version.
async fn replica_nodes(
    local: &Coordinator,
    membership: &Arc<Membership>,
    object: &ObjectId,
    scope: &RequestScope,
) -> Vec<NodeId> {
    let me = local.local_node();
    super::failover::page_zero_members(local, membership, object, scope)
        .await
        .map(|members| members.into_iter().filter(|node| *node != me).collect())
        .unwrap_or_default()
}

/// The record a displaced manifest replica journals when it drops the
/// session: unlike [`delete_record`], replay keeps the version's written
/// pages pinned, as they still are in memory.
pub(crate) fn forget_record(version: &ObjectVersion) -> Result<JournalRecord> {
    Ok(JournalRecord::Forget {
        cache: version.object.cache.0.clone(),
        key: version.object.key.0,
        etag: etag_text(&version.etag)?,
    })
}

pub(crate) fn delete_record(version: &ObjectVersion) -> Result<JournalRecord> {
    Ok(JournalRecord::Delete {
        cache: version.object.cache.0.clone(),
        key: version.object.key.0,
        etag: etag_text(&version.etag)?,
    })
}

async fn persist(
    local: &Coordinator,
    object: &ObjectId,
    context: &OriginContext,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<WriteReply> {
    let directory = local.writes.directory().clone();
    // A persist carries the client's origin context; keep it for the upload
    // (it is the only copy after a coordinator restart).
    let found = directory.with(object, |entry| {
        let entry = entry.filter(|e| e.role == Role::Coordinator)?;
        let resume = entry.session.state() == UploadState::Pending && entry.context.is_none();
        if entry.context.is_none() {
            entry.context = super::duplicate_context(context).ok();
        }
        Some((resume, entry.version.clone()))
    });
    let Some((resume, version)) = found else {
        return Ok(WriteReply::Unknown);
    };
    let event = if resume {
        SessionEvent::UploadResumed
    } else {
        SessionEvent::PersistRequested
    };
    let Some(effects) = object_event(local, object, event) else {
        return Ok(WriteReply::Unknown);
    };
    let rejection = apply(local, object, effects, membership, scope).await?;
    let state = directory
        .with(object, |e| e.map(|e| e.session.state()))
        .unwrap_or(UploadState::Failed);
    Ok(match rejection {
        None => WriteReply::Committed { state },
        Some(_) => WriteReply::Status {
            state,
            etag: etag_text(&version.etag)?,
        },
    })
}

async fn replicate(
    local: &Coordinator,
    version: ObjectVersion,
    manifest: Manifest,
    context: &OriginContext,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<WriteReply> {
    let object = version.object.clone();
    if !writes_enabled(local, &object)
        || manifest.etag != etag_text(&version.etag)?
        || now_millis(local) >= manifest.spec.hold_until_millis
    {
        return Ok(WriteReply::Failed);
    }
    let directory = local.writes.directory().clone();
    // Never displace a session this worker coordinates.
    if directory.with(&object, |e| {
        e.is_some_and(|e| e.role == Role::Coordinator && e.version != version)
    }) {
        return Ok(WriteReply::Exists);
    }
    // The writer's origin credentials travel with the manifest, so a replica
    // that takes over can finish the upload (WP8).
    let credentials = (context.authorization.is_some() || context.metadata.is_some())
        .then(|| super::duplicate_context(context).ok())
        .flatten();
    let held = directory.with(&object, |e| {
        e.filter(|e| e.version == version).map(|e| e.role)
    });
    if held == Some(Role::Coordinator) {
        // Another coordinator of this version (a failover overlap): merge
        // its holders and its upload outcome into ours.
        return merge(local, &object, manifest, credentials, membership, scope).await;
    }
    let state = manifest.state;
    let lease = u64::try_from(local.writes.settings().lease.as_millis()).unwrap_or(u64::MAX);
    let mut session = Session::from_manifest(
        object.cache.0.clone(),
        object.key.0,
        manifest.clone(),
        lease,
    );
    let mut context = credentials;
    if held == Some(Role::Replica) {
        // The sender's holder table is authoritative; keep an upload this
        // replica already saw commit, and its credentials.
        let now = now_millis(local);
        let previous = directory.with(&object, |e| {
            e.map(|e| {
                (
                    e.session.state(),
                    e.session.blob_etag().map(str::to_owned),
                    e.context
                        .as_ref()
                        .and_then(|c| super::duplicate_context(c).ok()),
                )
            })
        });
        if let Some((previous_state, blob_etag, previous_context)) = previous {
            if previous_state == UploadState::Committed {
                session.on_event(SessionEvent::UploadedElsewhere { blob_etag }, now);
            }
            if context.is_none() {
                context = previous_context;
            }
        }
    }
    journal(
        local,
        JournalRecord::Commit {
            cache: object.cache.0.clone(),
            key: object.key.0,
            manifest: session.manifest(),
        },
    )
    .await?;
    directory.replace(
        object.clone(),
        Entry::new(Role::Replica, version, session, context),
    );
    publish(local, &object)?;
    Ok(WriteReply::Committed { state })
}

/// Merge a manifest from another coordinator of the same version into this
/// coordinator's session: union the holders, and adopt its upload if it
/// committed the block list.
async fn merge(
    local: &Coordinator,
    object: &ObjectId,
    manifest: Manifest,
    credentials: Option<OriginContext>,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<WriteReply> {
    let directory = local.writes.directory();
    directory.with(object, |e| {
        if let Some(e) = e
            && e.context.is_none()
        {
            e.context = credentials;
        }
    });
    let mut events = vec![SessionEvent::HoldersMerged(manifest.holders)];
    if manifest.state == UploadState::Committed {
        events.push(SessionEvent::UploadedElsewhere {
            blob_etag: manifest.blob_etag,
        });
    }
    for event in events {
        if let Some(effects) = object_event(local, object, event) {
            apply(local, object, effects, membership, scope).await?;
        }
    }
    let state = directory
        .with(object, |e| e.map(|e| e.session.state()))
        .ok_or(Error::Unavailable)?;
    Ok(WriteReply::Committed { state })
}

/// Append `record` to this worker's write journal and wait until durable.
pub(crate) async fn journal(local: &Coordinator, record: JournalRecord) -> Result<()> {
    let journal = local
        .fill
        .dependencies
        .writer
        .write_journal()
        .ok_or(Error::Unavailable)?;
    let clock = local.fill.dependencies.candidates.ctx().clock.clone();
    let start = clock.now();
    journal.append(record, None).await?;
    local
        .fill
        .metrics
        .writes()
        .fsync(clock.now().saturating_duration_since(start));
    Ok(())
}

/// Publish the version descriptor and a short-lived current pointer.
fn publish(local: &Coordinator, object: &ObjectId) -> Result<()> {
    let now = super::wall_now(local);
    let Some((metadata, visible)) = local
        .writes
        .directory()
        .with(object, |e| e.map(|e| (e.metadata(now), e.visible(now))))
    else {
        return Ok(());
    };
    let Some(metadata) = metadata else {
        return Ok(());
    };
    let index = local.fill.dependencies.writer.index();
    if visible {
        index.publish_current(metadata)
    } else {
        index.publish_version(metadata.immutable())
    }
}

/// Apply session effects in order. Journal and catalog effects run before
/// this returns; releases, uploads, and the block-list commit run in the
/// background on this worker. Returns the event's rejection, if any.
pub(crate) async fn apply(
    local: &Coordinator,
    object: &ObjectId,
    effects: Vec<SessionEffect>,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<Option<Rejection>> {
    let _ = scope;
    let mut rejection = None;
    for effect in effects {
        match effect {
            SessionEffect::Journal(record) => journal(local, record).await?,
            SessionEffect::Publish => publish(local, object)?,
            SessionEffect::Unpublish => local
                .fill
                .dependencies
                .writer
                .index()
                .invalidate_current(object),
            SessionEffect::Notify(UploadState::Committed) => {
                // The origin is now authoritative for fresh reads.
                local
                    .fill
                    .dependencies
                    .writer
                    .index()
                    .invalidate_current(object);
                // Manifest replicas (and a coordinator this one overlapped
                // with after a failover) learn the upload committed.
                spawn_replicate(local, object, membership);
            }
            SessionEffect::Notify(_) => {}
            SessionEffect::Release { nodes } => {
                spawn_release(local, object, membership, Some(nodes));
            }
            SessionEffect::ReleaseTargets => spawn_release(local, object, membership, None),
            SessionEffect::Upload { node, pages } => {
                super::upload::spawn_upload(local, object, membership, node, pages);
            }
            SessionEffect::CommitBlockList => {
                super::upload::spawn_commit(local, object, membership);
            }
            SessionEffect::Restore { .. } => {
                // One repair task restores every under-replicated page.
                super::repair::spawn(local, object, membership);
            }
            SessionEffect::Reject(why) => rejection = Some(why),
        }
    }
    Ok(rejection)
}

/// Send `object`'s manifest to its manifest replicas in the background.
fn spawn_replicate(local: &Coordinator, object: &ObjectId, membership: &Arc<Membership>) {
    let Ok(local) = local.prefetch_directory().local_coordinator() else {
        return;
    };
    let Ok(permit) = local.drivers().reserve() else {
        return;
    };
    let object = object.clone();
    let membership = membership.clone();
    permit.submit_detached(async move {
        let scope = background_scope(&local)?;
        replicate_manifest(&local, &object, &membership, &scope).await;
        Ok::<(), Error>(())
    });
}

/// A scope for detached coordinator work.
pub(crate) fn background_scope(local: &Coordinator) -> Result<RequestScope> {
    let ctx = local.fill.dependencies.candidates.ctx();
    let mut id = [0; 16];
    ctx.rng.fill(&mut id).map_err(|_| Error::Unavailable)?;
    RequestScope::with_clock(
        crate::model::RequestId(id),
        ctx.now() + BACKGROUND_TIMEOUT,
        ctx.clock.clone(),
    )
}

/// Release the pages of `object`'s version on `nodes` (or, before commit,
/// on every ranked target of every page) in the background. Pages stay
/// readable (`keep`) only once the object uploaded.
fn spawn_release(
    local: &Coordinator,
    object: &ObjectId,
    membership: &Arc<Membership>,
    nodes: Option<Vec<NodeIdBytes>>,
) {
    let Some((version, session, holders, pages, keep)) =
        local.writes.directory().with(object, |e| {
            e.map(|e| {
                (
                    e.version.clone(),
                    e.session.identity().session,
                    e.session.holders().clone(),
                    e.session.spec().pages(),
                    e.session.state() == UploadState::Committed,
                )
            })
        })
    else {
        return;
    };
    let Ok(local) = local.prefetch_directory().local_coordinator() else {
        return;
    };
    let Ok(permit) = local.drivers().reserve() else {
        return;
    };
    let membership = membership.clone();
    let object = object.clone();
    permit.submit_detached(async move {
        let scope = background_scope(&local)?;
        // Pages per node.
        let mut plan: std::collections::BTreeMap<NodeId, Vec<u64>> = Default::default();
        match nodes {
            Some(nodes) => {
                for node in nodes {
                    let id = String::from_utf8(node.clone()).map_err(|_| Error::Internal)?;
                    let entry = plan.entry(NodeId(id)).or_default();
                    for page in 0..holders.pages() {
                        if holders.page(page).contains(&node) {
                            entry.push(page);
                        }
                    }
                }
            }
            None => {
                for page in 0..pages {
                    for node in ranked(&local, &membership, &object, page, &scope).await? {
                        plan.entry(node).or_default().push(page);
                    }
                }
            }
        }
        let context = super::bare_context(&object);
        for (node, pages) in plan {
            for chunk in pages.chunks(OPERATION_PAGES) {
                let operation = PeerOperation::WriteRelease {
                    version: version.clone(),
                    session,
                    pages: chunk.to_vec(),
                    keep,
                };
                let _ = send(&local, &membership, &node, operation, &context, &scope).await;
            }
        }
        Ok::<(), Error>(())
    });
}

/// Feed events to `object`'s session from background work and apply the
/// effects.
pub(crate) async fn background_event(
    local: &Coordinator,
    object: &ObjectId,
    session: SessionId,
    event: SessionEvent,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<()> {
    let Some(effects) = local.writes.directory().with(object, |e| {
        let e = e?;
        (e.session.identity().session == session)
            .then(|| e.session.on_event(event, now_millis(local)))
    }) else {
        return Ok(());
    };
    apply(local, object, effects, membership, scope).await?;
    settle(local, object);
    Ok(())
}

/// Upload state of a version, for `HEAD`. Asks the version's coordinator.
/// A Racer ETag reports its session's state; any other ETag reports
/// `Committed` only if it is the Blob ETag a still-known session committed
/// as. `None` otherwise or on any failure.
pub(crate) async fn upload_state(
    local: &Coordinator,
    version: &ObjectVersion,
    membership: Arc<Membership>,
    scope: &RequestScope,
) -> Option<UploadState> {
    if !writes_enabled(local, &version.object) {
        return None;
    }
    // A Racer ETag asks for the session's state; any other ETag asks
    // whether it is the session's committed Blob ETag.
    let etag = (!super::is_written_etag(&version.etag)).then(|| version.etag.clone());
    let coordinator = ranked(local, &membership, &version.object, 0, scope)
        .await
        .ok()?
        .into_iter()
        .next()?;
    let operation = PeerOperation::WriteSession {
        object: version.object.clone(),
        etag,
        action: Box::new(WriteAction::Status { wait_millis: 0 }),
    };
    let context = super::bare_context(&version.object);
    match send(local, &membership, &coordinator, operation, &context, scope).await {
        Ok(WriteReply::Status { state, etag }) if etag.as_bytes() == version.etag.as_bytes() => {
            Some(state)
        }
        _ => None,
    }
}

/// Load journaled sessions into the directory once the journal is bound.
/// Every reloaded session acts as its object's coordinator; a reloaded
/// manifest replica therefore also releases holders at expiry, which is
/// idempotent.
pub(crate) fn load(local: &Coordinator) {
    if local.writes.loaded() {
        return;
    }
    let Some(journal) = local.fill.dependencies.writer.write_journal() else {
        return;
    };
    local.writes.set_loaded();
    let now = now_millis(local);
    let lease = u64::try_from(local.writes.settings().lease.as_millis()).unwrap_or(u64::MAX);
    let directory = local.writes.directory().clone();
    for (key, session) in journal.state().sessions {
        let object = ObjectId {
            cache: racer_wire::control::CacheId(key.cache.clone()),
            key: crate::model::CacheKey(key.key),
        };
        let Ok(etag) = StrongEtag::parse(key.etag.as_bytes()) else {
            continue;
        };
        let version = ObjectVersion {
            object: object.clone(),
            etag,
        };
        let session = match session {
            crate::store::journal::JournalSession::Committed(manifest) => {
                Session::from_manifest(key.cache, key.key, manifest, lease)
            }
            crate::store::journal::JournalSession::Open {
                session,
                spec,
                lease_until_millis,
            } => Session::open(
                WriteIdentity {
                    cache: key.cache,
                    key: key.key,
                    etag: key.etag,
                    session,
                },
                spec,
                lease_until_millis.saturating_sub(lease).min(now),
                lease,
            ),
        };
        directory.replace(
            object,
            Entry::new(Role::Coordinator, version, session, None),
        );
    }
}

/// One maintenance turn: load the journal, expire leases and holds, drop
/// expired holder pages, rekey, collect garbage, decide coordinator roles,
/// repair lost copies, resume pending uploads, keep visible versions'
/// descriptors published, and sample metrics.
pub(crate) async fn maintain(local: &Coordinator) -> Result<()> {
    load(local);
    let scope = background_scope(local)?;
    let now = now_millis(local);
    let wall = super::wall_now(local);
    let writer = &local.fill.dependencies.writer;
    // Holder side: pages whose hold ended and that no coordinator extended.
    if writer.write_journal().is_some() {
        for page in writer.expired_written(wall).into_iter().take(EXPIRY_BATCH) {
            let _ = writer.drop_written(&page).await;
        }
    }
    let membership = local.current_membership();
    let directory = local.writes.directory().clone();
    // WP8: garbage, keys, roles, and repair. Each step is best effort; a
    // failure leaves the rest of the turn to run.
    let _ = super::rekey::run(local, &scope).await;
    if let Some(membership) = &membership {
        let _ = super::gc::collect(local, membership, &scope).await;
        let _ = super::failover::check_roles(local, membership, &scope).await;
        for object in directory.objects() {
            let _ = super::repair::maintain_object(local, &object, membership, &scope).await;
            let _ = super::failover::resume(local, &object, membership, &scope).await;
        }
    }
    for object in directory.objects() {
        let due = directory.with(&object, |e| {
            let e = e?;
            let hold = e.session.spec().hold_until_millis;
            Some(match (e.role, e.session.phase()) {
                (_, Phase::Open { .. }) => Some(SessionEvent::LeaseTick),
                (Role::Coordinator, Phase::Committed) if now >= hold => {
                    Some(SessionEvent::HoldExpired)
                }
                (Role::Replica, Phase::Committed) if now >= hold => None,
                _ => return None,
            })
        });
        match due {
            None => {}
            Some(None) => {
                // An expired replica: forget it.
                if let Some(entry) = directory.remove(&object) {
                    writer.index().invalidate_current(&object);
                    if let Ok(record) = delete_record(&entry.version) {
                        let _ = journal(local, record).await;
                    }
                }
            }
            Some(Some(event)) => {
                let Some(membership) = membership.clone() else {
                    continue;
                };
                let uploaded = directory.with(&object, |e| {
                    e.is_some_and(|e| e.session.state() == UploadState::Committed)
                });
                if let Some(effects) = object_event(local, &object, event) {
                    let _ = apply(local, &object, effects, &membership, &scope).await;
                }
                // An uploaded object needs no hold; forget it at expiry.
                if uploaded
                    && directory.with(&object, |e| {
                        e.is_some_and(|e| now >= e.session.spec().hold_until_millis)
                    })
                    && let Some(entry) = directory.remove(&object)
                    && let Ok(record) = delete_record(&entry.version)
                {
                    let _ = journal(local, record).await;
                }
                settle(local, &object);
            }
        }
    }
    // Keep the descriptors of visible versions in the catalog (a restart or
    // metadata eviction drops them); readers revalidate through refresh.
    let index = writer.index();
    for object in directory.objects() {
        let missing = directory.with(&object, |e| {
            e.filter(|e| e.visible(wall))
                .map(|e| e.version.clone())
                .filter(|v| index.version(v).ok().flatten().is_none())
        });
        if let Some(version) = missing {
            let spec = directory.with(&object, |e| e.map(|e| e.session.spec().clone()));
            if let Some(spec) = spec {
                let _ = index.publish_version(VersionMetadata {
                    content_type: spec
                        .content_type
                        .as_deref()
                        .and_then(|c| crate::model::ContentType::parse(c.as_bytes()).ok()),
                    version,
                    length: spec.length,
                });
            }
        }
    }
    sample_metrics(local);
    let _ = scope;
    Ok(())
}

fn sample_metrics(local: &Coordinator) {
    let metrics = local.fill.metrics.writes();
    let worker = local.fill.dependencies.writer.index().worker().0;
    let writer = &local.fill.dependencies.writer;
    let mut caches: Vec<String> = writer
        .written_pages()
        .into_iter()
        .map(|(page, _)| page.version.object.cache.0)
        .collect();
    caches.sort();
    caches.dedup();
    let dirty: Vec<(String, u64)> = caches
        .into_iter()
        .map(|cache| {
            let bytes = writer.dirty_bytes(&racer_wire::control::CacheId(cache.clone()));
            (cache, bytes)
        })
        .collect();
    metrics.sample_dirty(worker, &dirty);
    metrics.sample_states(worker, &local.writes.directory().upload_states());
}

/// A page id of `version`.
pub(crate) fn page_id(version: &ObjectVersion, page: u64) -> PageId {
    PageId {
        version: version.clone(),
        number: PageNumber(page),
    }
}
