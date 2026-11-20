//! Coordinator failover (WP8).
//!
//! The role of a worker for a written object is not journaled: after a
//! restart every journaled session loads as a coordinator, and a manifest
//! replica must take over when the coordinator is lost. Roles are therefore
//! decided again whenever the membership version changes (and every
//! [`ROLE_RECHECK`] otherwise), with one deterministic rule:
//!
//! > A worker coordinates a written version exactly when no member ranked
//! > ahead of it in the page-zero placement (among the first
//! > `RANKED_DEPTH`) knows that version.
//!
//! "Knows" is a `status` answer that names the same Racer ETag. So the
//! highest-ranked live holder of the manifest coordinates, and every other
//! holder defers to it. A worker that promotes itself restores the manifest
//! copies and resumes an upload that was pending; a worker that demotes
//! itself sends its manifest (holders, upload outcome, and upload
//! credentials) to the member it found, which merges it.
//!
//! Two coordinators can overlap briefly (a partition, or a restarted
//! coordinator that missed the takeover). Their uploads are idempotent at
//! the origin: Put Block of the same block ID writes the same bytes, and
//! Put Block List uses `If-None-Match: *`, so at most one block list
//! commits. The loser sees a conflict, asks its peers, and adopts the
//! winner's commit instead of failing (`upload::spawn_commit`); a winner's
//! manifest push after the commit also converges the others
//! (`SessionEvent::UploadedElsewhere`).

use super::RANKED_DEPTH;
use super::coordinator::object_event;
use super::coordinator::ranked;
use super::coordinator::replicate_manifest;
use super::coordinator::send_manifest;
use super::directory::Role;
use super::send;
use crate::error::Result;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::peer::protocol::Operation as PeerOperation;
use crate::read::Coordinator;
use crate::runtime::RequestScope;
use crate::topology::Membership;
use racer_core::write::Phase;
use racer_core::write::SessionEvent;
use racer_core::write::UploadState;
use racer_core::write::WriteAction;
use racer_core::write::WriteReply;
use racer_wire::control::NodeId;
use std::sync::Arc;
use std::time::Duration;

/// Longest a role decision stands without a placement change.
pub const ROLE_RECHECK: Duration = Duration::from_secs(30);

/// What a member says about one written version.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Knowledge {
    /// It tracks this version, in this upload state.
    Known(UploadState),
    /// It answered and does not track this version.
    Unknown,
    /// It did not answer.
    Silent,
    /// It holds a tombstone: a client deleted this version.
    Deleted,
}

/// Ask `node` for the status of `version`.
pub(crate) async fn probe(
    local: &Coordinator,
    membership: &Arc<Membership>,
    node: &NodeId,
    version: &ObjectVersion,
    scope: &RequestScope,
) -> Knowledge {
    // Naming the version lets the member answer that it was deleted.
    let operation = PeerOperation::WriteSession {
        object: version.object.clone(),
        etag: Some(version.etag.clone()),
        action: Box::new(WriteAction::Status { wait_millis: 0 }),
    };
    let context = super::bare_context(&version.object);
    match send(local, membership, node, operation, &context, scope).await {
        Ok(WriteReply::Status { state, etag }) if etag.as_bytes() == version.etag.as_bytes() => {
            Knowledge::Known(state)
        }
        Ok(WriteReply::Deleted) => Knowledge::Deleted,
        Ok(_) => Knowledge::Unknown,
        Err(_) => Knowledge::Silent,
    }
}

/// The pool members of `object`'s cache among the first [`RANKED_DEPTH`] of
/// its page-zero placement, in rank order.
pub(crate) async fn page_zero_members(
    local: &Coordinator,
    membership: &Arc<Membership>,
    object: &ObjectId,
    scope: &RequestScope,
) -> Result<Vec<NodeId>> {
    Ok(ranked(local, membership, object, 0, scope)
        .await?
        .into_iter()
        .filter(|node| membership.serves(&object.cache, node))
        .take(RANKED_DEPTH)
        .collect())
}

/// Decide the role of every tracked, committed object whose decision is due.
pub(crate) async fn check_roles(
    local: &Coordinator,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<()> {
    let placement = membership.version.0;
    let now = scope.clock().now();
    let wall = super::now_millis(local);
    let directory = local.writes.directory().clone();
    for object in directory.objects() {
        let due = directory.with(&object, |e| {
            let e = e?;
            // Open sessions only exist on the coordinator that opened them;
            // expired ones are about to be dropped.
            if e.session.phase() != Phase::Committed || wall >= e.session.spec().hold_until_millis {
                return None;
            }
            // An unconfirmed (restored) session is checked every turn.
            let due = !e.confirmed
                || e.role_checked
                    .is_none_or(|(at, when)| at != placement || now >= when + ROLE_RECHECK);
            due.then(|| {
                (
                    e.role,
                    e.version.clone(),
                    e.confirmed,
                    e.session.spec().hold_until_millis,
                    e.session.spec().replicas,
                )
            })
        });
        let Some((role, version, confirmed, hold_until_millis, replicas)) = due else {
            continue;
        };
        let deleted = || async {
            super::coordinator::record_tombstone(local, &version, hold_until_millis, replicas).await
        };
        if !confirmed {
            match confirm(local, membership, &version, replicas, scope).await? {
                Confirmation::Deleted => {
                    deleted().await?;
                    continue;
                }
                Confirmation::Pending => continue,
                Confirmation::Live => {
                    directory.with(&object, |e| {
                        if let Some(e) = e.filter(|e| e.version == version) {
                            e.confirmed = true;
                        }
                    });
                    let _ = super::coordinator::publish(local, &object);
                }
            }
        }
        let member = page_zero_members(local, membership, &object, scope)
            .await?
            .contains(&local.local_node());
        let decided = decide(local, membership, &version, scope).await?;
        directory.with(&object, |e| {
            if let Some(e) = e.filter(|e| e.version == version) {
                e.role_checked = Some((placement, now));
            }
        });
        let decided = match decided {
            Decision::Deleted => {
                deleted().await?;
                continue;
            }
            Decision::Lead => None,
            Decision::Defer(leader) => Some(leader),
        };
        match (role, decided) {
            // Only a page-zero member may take over: deletes and readers
            // reach those members, not a replica the placement moved away.
            // It first asks the members ranked behind it for a tombstone
            // (those ahead answered `decide`): a replica that missed a
            // delete must not take the deleted version over.
            (Role::Replica, None) if member => {
                if tombstoned_behind(local, membership, &version, scope).await? {
                    deleted().await?;
                    continue;
                }
                promote(local, &object, &version, membership, scope).await;
            }
            // A member knows it: this displaced manifest copy is not
            // needed. Its written pages are: they stay pinned (in memory and
            // across a restart) until the coordinator releases them.
            (Role::Replica, Some(_)) if !member => {
                if local.writes.directory().remove_version(&version).is_some() {
                    local
                        .fill
                        .dependencies
                        .writer
                        .index()
                        .invalidate_current(&object);
                    if let Ok(record) = super::coordinator::forget_record(&version) {
                        let _ = super::coordinator::journal(local, record).await;
                    }
                }
            }
            (Role::Coordinator, Some(leader)) => {
                demote(local, &object, &version, &leader, membership, scope).await;
            }
            _ => {}
        }
    }
    Ok(())
}

/// The outcome of a role decision.
enum Decision {
    /// This worker should coordinate the version.
    Lead,
    /// The first member ranked ahead of this worker that knows the version.
    Defer(NodeId),
    /// A member ranked ahead holds a tombstone for the version.
    Deleted,
}

/// Whether this worker should coordinate `version`.
async fn decide(
    local: &Coordinator,
    membership: &Arc<Membership>,
    version: &ObjectVersion,
    scope: &RequestScope,
) -> Result<Decision> {
    let me = local.local_node();
    let ranked = page_zero_members(local, membership, &version.object, scope).await?;
    for node in ranked.into_iter().take_while(|node| *node != me) {
        match probe(local, membership, &node, version, scope).await {
            Knowledge::Known(_) => return Ok(Decision::Defer(node)),
            Knowledge::Deleted => return Ok(Decision::Deleted),
            Knowledge::Unknown | Knowledge::Silent => {}
        }
    }
    Ok(Decision::Lead)
}

/// Whether a page-zero member ranked behind this worker holds a tombstone
/// for `version`.
async fn tombstoned_behind(
    local: &Coordinator,
    membership: &Arc<Membership>,
    version: &ObjectVersion,
    scope: &RequestScope,
) -> Result<bool> {
    let me = local.local_node();
    let ranked = page_zero_members(local, membership, &version.object, scope).await?;
    for node in ranked.into_iter().skip_while(|node| *node != me).skip(1) {
        if probe(local, membership, &node, version, scope).await == Knowledge::Deleted {
            return Ok(true);
        }
    }
    Ok(false)
}

/// What a restored session's tombstone check found.
#[derive(Debug, Eq, PartialEq)]
pub(crate) enum Confirmation {
    /// No member that answered holds a tombstone, and too few stayed silent
    /// to hide one: the version is live.
    Live,
    /// A member holds a tombstone: the version was deleted.
    Deleted,
    /// Too many members stayed silent; ask again next turn.
    Pending,
}

/// Ask every page-zero member (other than this worker) whether it holds a
/// tombstone for `version`. An acknowledged delete left one on at least
/// `min(replicas, members)` of them, so the version is live once fewer
/// than that many stayed silent and none answered `deleted`
/// ([`confirmation`]).
async fn confirm(
    local: &Coordinator,
    membership: &Arc<Membership>,
    version: &ObjectVersion,
    replicas: u8,
    scope: &RequestScope,
) -> Result<Confirmation> {
    let me = local.local_node();
    let members = page_zero_members(local, membership, &version.object, scope).await?;
    let others: Vec<NodeId> = members.iter().filter(|n| **n != me).cloned().collect();
    let answers = futures::future::join_all(
        others
            .iter()
            .map(|node| probe(local, membership, node, version, scope)),
    )
    .await;
    Ok(confirmation(&answers, replicas, members.len()))
}

/// [`confirm`]'s rule over the other members' `answers`, for a version of
/// `replicas` replicas among `members` page-zero members (this worker
/// included when it is one).
pub(crate) fn confirmation(answers: &[Knowledge], replicas: u8, members: usize) -> Confirmation {
    if answers.contains(&Knowledge::Deleted) {
        return Confirmation::Deleted;
    }
    let quorum = usize::from(replicas.max(1)).min(members.max(1));
    let silent = answers.iter().filter(|a| **a == Knowledge::Silent).count();
    if silent < quorum {
        Confirmation::Live
    } else {
        Confirmation::Pending
    }
}

/// Take over `version`: restore the manifest copies and resume a pending
/// upload when the credentials are here.
async fn promote(
    local: &Coordinator,
    object: &ObjectId,
    version: &ObjectVersion,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) {
    let promoted = local.writes.directory().with(object, |e| {
        let Some(e) = e.filter(|e| e.version == *version && e.role == Role::Replica && e.confirmed)
        else {
            return false;
        };
        e.role = Role::Coordinator;
        true
    });
    if !promoted {
        return;
    }
    replicate_manifest(local, object, membership, scope).await;
    let _ = resume(local, object, membership, scope).await;
}

/// Restart a pending upload of an object this worker coordinates, when it
/// has the upload credentials.
pub(crate) async fn resume(
    local: &Coordinator,
    object: &ObjectId,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<()> {
    let pending = local.writes.directory().with(object, |e| {
        e.is_some_and(|e| {
            e.role == Role::Coordinator
                && e.confirmed
                && e.session.phase() == Phase::Committed
                && e.session.state() == UploadState::Pending
                && e.context.is_some()
        })
    });
    if !pending {
        return Ok(());
    }
    if let Some(effects) = object_event(local, object, SessionEvent::UploadResumed) {
        super::coordinator::apply(local, object, effects, membership, scope).await?;
    }
    Ok(())
}

/// Defer to `leader` for `version`: become a replica and send it this
/// worker's manifest to merge.
async fn demote(
    local: &Coordinator,
    object: &ObjectId,
    version: &ObjectVersion,
    leader: &NodeId,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) {
    let demoted = local.writes.directory().with(object, |e| {
        let Some(e) = e.filter(|e| e.version == *version && e.role == Role::Coordinator) else {
            return false;
        };
        e.role = Role::Replica;
        true
    });
    if demoted {
        send_manifest(
            local,
            object,
            std::slice::from_ref(leader),
            membership,
            scope,
        )
        .await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_restored_session_is_live_only_when_no_hidden_tombstone_can_exist() {
        use Knowledge::*;
        let known = Known(UploadState::Never);
        // Any tombstone wins, whatever else answered.
        assert_eq!(
            confirmation(&[known.clone(), Deleted, Silent], 2, 4),
            Confirmation::Deleted
        );
        // replicas=2 over 4 members: an acknowledged delete left the
        // tombstone on two others, so one silent member cannot hide it.
        assert_eq!(
            confirmation(&[known.clone(), Unknown, Silent], 2, 4),
            Confirmation::Live
        );
        assert_eq!(
            confirmation(&[Unknown, Silent, Silent], 2, 4),
            Confirmation::Pending
        );
        // replicas=1: the only tombstone may be on the silent member.
        assert_eq!(
            confirmation(&[Unknown, Silent], 1, 3),
            Confirmation::Pending
        );
        assert_eq!(confirmation(&[Unknown, Unknown], 1, 3), Confirmation::Live);
        // Fewer members than replicas: a delete needed every member, this
        // worker included, so it was never acknowledged without it.
        assert_eq!(confirmation(&[Silent], 3, 2), Confirmation::Live);
        // Alone: nobody to ask.
        assert_eq!(confirmation(&[], 2, 1), Confirmation::Live);
    }
}
