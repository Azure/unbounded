//! Garbage collection of written pages and sessions nobody needs (WP8).
//!
//! Runs on every worker's maintenance turn, over the written pages this
//! worker holds:
//!
//! - Orphans: a written version that no page-zero member knows (the
//!   coordinator lost the session: a lost abort, a coordinator that lost its
//!   journal, a release that never arrived) is dropped once it stayed
//!   unknown for `RACER_WRITE_ORPHAN_SECS`. Members are asked with `status`
//!   at most every [`check_interval`]: page-zero members first, then, when
//!   none knows it, every other member of the cache's pool (the coordinator
//!   may have been displaced from page zero since the write). A version is
//!   unknown only when every member answered and none knows it, so an
//!   unreachable coordinator or replica blocks the vote.
//! - Missed releases: a version its coordinator reports uploaded is
//!   unpinned (kept readable) at the next check, in case the coordinator's
//!   one-shot background release never reached this holder.
//! - Removed caches: written pages and sessions of a cache that is no
//!   longer published are dropped once the cache stayed absent for
//!   [`REMOVED_CACHE_GRACE`] and their hold ended or the object has nothing
//!   left to upload, and otherwise once it stayed absent for
//!   `RACER_WRITE_ORPHAN_SECS`. A cache ID is never reused for another
//!   namespace (`control::Availability`), so a cache that comes back finds
//!   no stale pins.

use super::directory::Role;
use super::failover::Knowledge;
use super::failover::page_zero_members;
use super::failover::probe;
use crate::error::Result;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::read::Coordinator;
use crate::runtime::HashMap;
use crate::runtime::RequestScope;
use crate::topology::Membership;
use racer_wire::control::CacheId;
use racer_wire::object::write::UploadState;
use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;
use std::time::Instant;

/// How long a cache must stay unpublished before its written pages and
/// sessions are dropped. Covers a catalog republish that briefly omits it.
pub const REMOVED_CACHE_GRACE: Duration = Duration::from_secs(30);

/// Versions probed per maintenance turn.
const PROBES_PER_TURN: usize = 16;

/// Per-worker garbage collection state.
#[derive(Default)]
pub struct GcState {
    /// Per written version: when it was first found unknown, and when to
    /// ask again.
    versions: HashMap<ObjectVersion, Probe>,
    /// When each cache with written state was first seen unpublished.
    absent: HashMap<CacheId, Instant>,
}

#[derive(Clone, Copy)]
struct Probe {
    unknown_since: Option<Instant>,
    next: Instant,
}

/// How often an orphan candidate is asked about: a quarter of `orphan_after`,
/// between one second and five minutes.
pub fn check_interval(orphan_after: Duration) -> Duration {
    (orphan_after / 4).clamp(Duration::from_secs(1), Duration::from_secs(300))
}

/// One garbage collection turn.
pub(crate) async fn collect(
    local: &Coordinator,
    membership: &Arc<Membership>,
    scope: &RequestScope,
) -> Result<()> {
    let now = scope.clock().now();
    let writer = &local.fill.dependencies.writer;
    if writer.write_journal().is_none() {
        return Ok(());
    }
    let mut by_version: BTreeMap<ObjectVersion, Vec<PageId>> = BTreeMap::new();
    for (page, _) in writer.written_pages() {
        by_version
            .entry(page.version.clone())
            .or_default()
            .push(page);
    }
    removed_caches(local, &mut by_version, now).await?;
    orphans(local, membership, by_version, now, scope).await
}

/// Whether written state of a cache unpublished for `absent` may go. Only
/// pinned written pages and committed sessions reach here, and a pinned page
/// is one whose object has not uploaded yet, so absence alone is weak
/// evidence: a catalog republish that briefly omits the cache must not lose
/// acknowledged data. State goes after [`REMOVED_CACHE_GRACE`] once its hold
/// ended (nothing would keep it anyway) or it has nothing left to upload,
/// and otherwise only after `RACER_WRITE_ORPHAN_SECS`.
fn removable(absent: Duration, orphan_after: Duration, settled: bool) -> bool {
    absent >= REMOVED_CACHE_GRACE && (settled || absent >= orphan_after)
}

/// Drop written pages and sessions of caches unpublished long enough (see
/// [`removable`]). Removes dropped versions from `by_version`.
async fn removed_caches(
    local: &Coordinator,
    by_version: &mut BTreeMap<ObjectVersion, Vec<PageId>>,
    now: Instant,
) -> Result<()> {
    let directory = local.writes.directory().clone();
    let mut caches: Vec<CacheId> = by_version
        .keys()
        .map(|v| v.object.cache.clone())
        .chain(directory.objects().into_iter().map(|o| o.cache))
        .collect();
    caches.sort();
    caches.dedup();
    let absent: HashMap<CacheId, Duration> = {
        let mut gc = local.writes.gc.borrow_mut();
        gc.absent.retain(|cache, _| caches.contains(cache));
        caches
            .into_iter()
            .filter_map(|cache| {
                if local.availability.cache(&cache) {
                    gc.absent.remove(&cache);
                    return None;
                }
                let since = *gc.absent.entry(cache.clone()).or_insert(now);
                let absent = now.saturating_duration_since(since);
                (absent >= REMOVED_CACHE_GRACE).then_some((cache, absent))
            })
            .collect()
    };
    if absent.is_empty() {
        return Ok(());
    }
    let orphan_after = local.writes.settings().orphan_after;
    let wall = super::now_millis(local);
    let writer = &local.fill.dependencies.writer;
    let versions: Vec<ObjectVersion> = by_version
        .iter()
        .filter(|(v, pages)| {
            absent.get(&v.object.cache).is_some_and(|&absent| {
                let held = pages.iter().any(|page| {
                    writer.written(page).is_some_and(|entry| {
                        crate::store::writes::unix_millis(entry.hold_until) > wall
                    })
                });
                removable(absent, orphan_after, !held)
            })
        })
        .map(|(v, _)| v.clone())
        .collect();
    for version in versions {
        for page in by_version.remove(&version).unwrap_or_default() {
            writer.drop_written(&page).await?;
        }
    }
    for object in directory.objects() {
        let Some(&cache_absent) = absent.get(&object.cache) else {
            continue;
        };
        let settled = directory.with(&object, |e| {
            e.is_none_or(|e| {
                wall >= e.session.spec().hold_until_millis
                    || matches!(
                        e.session.state(),
                        UploadState::Committed | UploadState::Failed
                    )
            })
        });
        if !removable(cache_absent, orphan_after, settled) {
            continue;
        }
        if let Some(entry) = directory.remove(&object) {
            writer.index().invalidate_current(&object);
            if let Ok(record) = super::coordinator::delete_record(&entry.version) {
                super::coordinator::journal(local, record).await?;
            }
        }
    }
    Ok(())
}

/// Ask about due written versions; drop those unknown for `orphan_after`.
async fn orphans(
    local: &Coordinator,
    membership: &Arc<Membership>,
    by_version: BTreeMap<ObjectVersion, Vec<PageId>>,
    now: Instant,
    scope: &RequestScope,
) -> Result<()> {
    let orphan_after = local.writes.settings().orphan_after;
    let interval = check_interval(orphan_after);
    let due: Vec<ObjectVersion> = {
        let mut gc = local.writes.gc.borrow_mut();
        gc.versions.retain(|v, _| by_version.contains_key(v));
        by_version
            .keys()
            .filter(|v| {
                let probe = gc.versions.entry((*v).clone()).or_insert(Probe {
                    unknown_since: None,
                    next: now,
                });
                probe.next <= now
            })
            .take(PROBES_PER_TURN)
            .cloned()
            .collect()
    };
    let writer = &local.fill.dependencies.writer;
    for version in due {
        let knowledge = known(local, membership, &version, scope).await?;
        if matches!(
            knowledge,
            Knowledge::Known(racer_wire::object::write::UploadState::Committed)
        ) {
            // Uploaded: the coordinator's release (sent once, in the
            // background) missed this holder. Unpin; the pages stay
            // readable as ordinary cache pages.
            for page in by_version.get(&version).into_iter().flatten() {
                writer.release_written(page).await?;
            }
            local.writes.gc.borrow_mut().versions.remove(&version);
            continue;
        }
        let expired = {
            let mut gc = local.writes.gc.borrow_mut();
            let Some(probe) = gc.versions.get_mut(&version) else {
                continue;
            };
            probe.next = now + interval;
            match knowledge {
                Knowledge::Known(_) => probe.unknown_since = None,
                Knowledge::Unknown => {
                    probe.unknown_since.get_or_insert(now);
                }
                Knowledge::Silent => {}
            }
            probe
                .unknown_since
                .is_some_and(|since| now.saturating_duration_since(since) >= orphan_after)
        };
        if !expired {
            continue;
        }
        for page in by_version.get(&version).into_iter().flatten() {
            writer.drop_written(page).await?;
        }
        local.writes.gc.borrow_mut().versions.remove(&version);
    }
    Ok(())
}

/// Whether any page-zero member of `version`'s object knows it. This worker's
/// own directory counts first, without a message.
async fn known(
    local: &Coordinator,
    membership: &Arc<Membership>,
    version: &ObjectVersion,
    scope: &RequestScope,
) -> Result<Knowledge> {
    let local_entry = local.writes.directory().with(&version.object, |e| {
        e.filter(|e| e.version == *version)
            .map(|e| (e.role, e.session.state()))
    });
    if let Some((Role::Coordinator | Role::Replica, state)) = local_entry {
        return Ok(Knowledge::Known(state));
    }
    // Every member must answer: a silent one may be the coordinator or a
    // manifest replica that knows the version (a partition or a stale link
    // must never turn a live write into an orphan).
    let mut silent = false;
    let page_zero = page_zero_members(local, membership, &version.object, scope).await?;
    for node in &page_zero {
        match probe(local, membership, node, version, scope).await {
            Knowledge::Known(state) => return Ok(Knowledge::Known(state)),
            Knowledge::Unknown => {}
            Knowledge::Silent => silent = true,
        }
    }
    if silent {
        return Ok(Knowledge::Silent);
    }
    // Page zero does not know it. The placement may have moved since the
    // write (members joined ahead of the coordinator, or the coordinator
    // and its replicas were displaced before re-replicating), so the
    // coordinator can be any pool member: ask the rest of the pool before
    // calling the version unknown. This runs only for orphan candidates,
    // at most once per check interval.
    let me = local.local_node();
    let rest: Vec<_> = membership
        .members()
        .iter()
        .map(|m| m.node.clone())
        .filter(|node| {
            *node != me
                && !page_zero.contains(node)
                && membership.serves(&version.object.cache, node)
        })
        .collect();
    for node in rest {
        match probe(local, membership, &node, version, scope).await {
            Knowledge::Known(state) => return Ok(Knowledge::Known(state)),
            Knowledge::Unknown => {}
            Knowledge::Silent => silent = true,
        }
    }
    Ok(if silent {
        Knowledge::Silent
    } else {
        Knowledge::Unknown
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn removed_cache_state_waits_for_its_hold_or_the_orphan_age() {
        let orphan = Duration::from_secs(3600);
        let short = REMOVED_CACHE_GRACE - Duration::from_secs(1);
        // Never inside the grace, even when settled.
        assert!(!removable(short, orphan, true));
        // Settled (hold over, or nothing to upload): after the grace.
        assert!(removable(REMOVED_CACHE_GRACE, orphan, true));
        // Unsettled: only after the orphan age.
        assert!(!removable(REMOVED_CACHE_GRACE, orphan, false));
        assert!(!removable(orphan - Duration::from_secs(1), orphan, false));
        assert!(removable(orphan, orphan, false));
        // An orphan age shorter than the grace never shortens the grace.
        assert!(!removable(short, Duration::from_secs(1), false));
    }
}
