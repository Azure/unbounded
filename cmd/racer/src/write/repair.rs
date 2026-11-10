//! Repair: restore `replicas` durable copies of a written, not yet uploaded
//! object's pages after a holder is lost (WP8).
//!
//! The coordinator watches its membership on every maintenance turn. Holders
//! that left the cache's pool are removed from the holder table in one
//! journaled `NodesLost` event; a page with no surviving holder fails the
//! object (upload state `failed`). For every page left with fewer than
//! `replicas` holders, a background task asks a new store target to pull the
//! page from a surviving holder with the same `write-store` operation the
//! writer uses: the target verifies the envelope and the AEAD tag of the
//! copy before it makes it durable (`store_target::store_page`). New targets
//! follow the writer's rule: the first reachable ranked owners of the page
//! that do not already hold it. Restored copies are merged into the holder
//! table with one journaled `HoldersMerged` event per task, and the manifest
//! is sent to the manifest replicas again.

use super::OPERATION_PAGES;
use super::RANKED_DEPTH;
use super::coordinator::background_event;
use super::coordinator::background_scope;
use super::coordinator::object_event;
use super::coordinator::ranked;
use super::coordinator::replicate_manifest;
use super::directory::Role;
use super::send;
use crate::error::Error;
use crate::error::Result;
use crate::model::ObjectId;
use crate::peer::protocol::Operation as PeerOperation;
use crate::peer::protocol::WriteStore;
use crate::read::Coordinator;
use crate::runtime::RequestScope;
use crate::topology::Membership;
use racer_core::write::Holders;
use racer_core::write::NodeIdBytes;
use racer_core::write::Phase;
use racer_core::write::SessionEvent;
use racer_core::write::UploadState;
use racer_core::write::WriteReply;
use racer_wire::control::NodeId;
use std::collections::BTreeMap;
use std::sync::Arc;

/// Holders of `object`'s session that are no longer members of its cache's
/// pool under `membership`.
pub(crate) fn lost_holders(
    local: &Coordinator,
    object: &ObjectId,
    membership: &Membership,
) -> Vec<NodeIdBytes> {
    local.writes.directory().with(object, |e| {
        let Some(e) = e else {
            return Vec::new();
        };
        e.session
            .holders()
            .nodes()
            .into_iter()
            .filter(|node| {
                String::from_utf8(node.clone())
                    .map_or(true, |id| !membership.serves(&object.cache, &NodeId(id)))
            })
            .collect()
    })
}

/// One repair turn for an object this worker coordinates: drop lost
/// holders, then start restoring under-replicated pages.
pub(crate) async fn maintain_object(
    local: &Coordinator,
    object: &ObjectId,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<()> {
    let eligible = local.writes.directory().with(object, |e| {
        e.is_some_and(|e| {
            e.role == Role::Coordinator
                && e.session.phase() == Phase::Committed
                && !matches!(
                    e.session.state(),
                    UploadState::Committed | UploadState::Failed
                )
        })
    });
    if !eligible {
        return Ok(());
    }
    let lost = lost_holders(local, object, membership);
    if !lost.is_empty() {
        if let Some(effects) = object_event(local, object, SessionEvent::NodesLost(lost)) {
            // `apply` starts the repair for `Restore` effects.
            super::coordinator::apply(local, object, effects, membership, scope).await?;
        }
        // The holder table changed: tell the manifest replicas.
        replicate_manifest(local, object, membership, scope).await;
    }
    spawn(local, object, membership);
    Ok(())
}

/// Start restoring `object`'s under-replicated pages in the background,
/// unless a repair already runs or nothing needs one.
pub(crate) fn spawn(local: &Coordinator, object: &ObjectId, membership: &Arc<Membership>) {
    let start = local.writes.directory().with(object, |e| {
        let Some(e) = e else {
            return false;
        };
        if e.repairing || e.role != Role::Coordinator || e.session.under_replicated().is_empty() {
            return false;
        }
        e.repairing = true;
        true
    });
    if !start {
        return;
    }
    let done = |local: &Coordinator, object: &ObjectId| {
        local.writes.directory().with(object, |e| {
            if let Some(e) = e {
                e.repairing = false;
            }
        });
    };
    let Ok(worker) = local.prefetch_directory().local_coordinator() else {
        done(local, object);
        return;
    };
    let Ok(permit) = worker.drivers().reserve() else {
        done(local, object);
        return;
    };
    let object = object.clone();
    let membership = membership.clone();
    permit.submit_detached(async move {
        let result = match background_scope(&worker) {
            Ok(scope) => restore(&worker, &object, &membership, &scope).await,
            Err(error) => Err(error),
        };
        done(&worker, &object);
        result
    });
}

/// Restore every under-replicated page of `object` once; pages no target
/// could store are retried on a later maintenance turn.
async fn restore(
    local: &Coordinator,
    object: &ObjectId,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<()> {
    let Some((version, session, spec, holders, pages)) =
        local.writes.directory().with(object, |e| {
            let e = e?;
            Some((
                e.version.clone(),
                e.session.identity().session,
                e.session.spec().clone(),
                e.session.holders().clone(),
                e.session.under_replicated(),
            ))
        })
    else {
        return Ok(());
    };
    if pages.is_empty() {
        return Ok(());
    }
    let template = WriteStore {
        session,
        pages: Vec::new(),
        length: spec.length,
        page_size: crate::model::PageSize::from_wire(spec.page_size)
            .map_err(|_| Error::Internal)?,
        content_type: spec
            .content_type
            .as_deref()
            .and_then(|c| crate::model::ContentType::parse(c.as_bytes()).ok()),
        source: local.local_node(),
        hold_until_millis: spec.hold_until_millis,
    };
    let health = local.fill.dependencies.candidates.link_health();
    let me = local.local_node();
    // Per page: the survivor to copy from and the ranked targets to try.
    let mut plans = Vec::with_capacity(pages.len());
    for (page, source) in pages {
        let Ok(source) = String::from_utf8(source).map(NodeId) else {
            continue;
        };
        let held = holders.page(page);
        let want = usize::from(spec.replicas).saturating_sub(held.len());
        let targets: Vec<NodeId> = ranked(local, membership, object, page, scope)
            .await?
            .into_iter()
            .filter(|node| {
                membership.serves(&object.cache, node)
                    && (*node == me || !health.silent(node))
                    && !held.contains(&node.0.clone().into_bytes())
            })
            .take(RANKED_DEPTH)
            .collect();
        plans.push(Plan {
            page,
            source,
            targets,
            next: 0,
            want,
        });
    }
    let mut restored = Holders(vec![Vec::new(); holders.pages() as usize]);
    let mut copies = 0u64;
    // Rounds: each round sends every page still short of copies to its next
    // untried target, batched per (target, source).
    for _ in 0..RANKED_DEPTH {
        let mut batches: BTreeMap<(NodeId, NodeId), Vec<u64>> = BTreeMap::new();
        for plan in plans.iter_mut().filter(|p| p.want > 0) {
            if let Some(target) = plan.targets.get(plan.next) {
                batches
                    .entry((target.clone(), plan.source.clone()))
                    .or_default()
                    .push(plan.page);
                plan.next += 1;
            }
        }
        if batches.is_empty() {
            break;
        }
        for ((target, source), batch) in batches {
            for chunk in batch.chunks(OPERATION_PAGES) {
                let operation = PeerOperation::WriteStore {
                    version: version.clone(),
                    store: Box::new(WriteStore {
                        pages: chunk.to_vec(),
                        source: source.clone(),
                        ..template.clone()
                    }),
                };
                let context = super::bare_context(object);
                let stored =
                    match send(local, membership, &target, operation, &context, scope).await {
                        Ok(WriteReply::Stored { pages }) => pages,
                        _ => Vec::new(),
                    };
                for page in stored {
                    if let Some(plan) = plans.iter_mut().find(|p| p.page == page)
                        && plan.want > 0
                    {
                        plan.want -= 1;
                        restored.0[page as usize].push(target.0.clone().into_bytes());
                        copies += 1;
                    }
                }
            }
        }
    }
    if copies == 0 {
        return Ok(());
    }
    background_event(
        local,
        object,
        session,
        SessionEvent::HoldersMerged(restored),
        membership,
        scope,
    )
    .await?;
    let metrics = local.fill.metrics.writes();
    for _ in 0..copies {
        metrics.repair(&object.cache.0);
    }
    replicate_manifest(local, object, membership, scope).await;
    Ok(())
}

/// Restore plan of one page.
struct Plan {
    page: u64,
    source: NodeId,
    targets: Vec<NodeId>,
    /// Index into `targets` of the next one to try.
    next: usize,
    /// Copies still missing.
    want: usize,
}
