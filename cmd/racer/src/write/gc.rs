//! Garbage collection of written pages and sessions nobody needs (WP8).
//!
//! Runs on every worker's maintenance turn, over the written pages this
//! worker holds:
//!
//! - Orphans: a written version that no page-zero member knows (the
//!   coordinator lost the session: a lost abort, a coordinator that lost its
//!   journal, a release that never arrived) is dropped once it stayed
//!   unknown for `RACER_WRITE_ORPHAN_SECS`. Members are asked with `status`
//!   at most every [`check_interval`]; a version is unknown only when a
//!   member answered and none knows it, so an unreachable coordinator never
//!   counts as a vote.
//! - Removed caches: written pages and sessions of a cache that is no
//!   longer published are dropped once the cache stayed absent for
//!   [`REMOVED_CACHE_GRACE`]. A cache ID is never reused for another
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
use crate::runtime::RequestScope;
use crate::topology::Membership;
use racer_wire::control::CacheId;
use std::collections::BTreeMap;
use std::collections::HashMap;
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

/// Drop written pages and sessions of caches unpublished for
/// [`REMOVED_CACHE_GRACE`]. Removes them from `by_version`.
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
    let gone: Vec<CacheId> = {
        let mut gc = local.writes.gc.borrow_mut();
        gc.absent.retain(|cache, _| caches.contains(cache));
        caches
            .into_iter()
            .filter(|cache| {
                if local.availability.cache(cache) {
                    gc.absent.remove(cache);
                    return false;
                }
                let since = *gc.absent.entry(cache.clone()).or_insert(now);
                now.saturating_duration_since(since) >= REMOVED_CACHE_GRACE
            })
            .collect()
    };
    if gone.is_empty() {
        return Ok(());
    }
    let writer = &local.fill.dependencies.writer;
    let versions: Vec<ObjectVersion> = by_version
        .keys()
        .filter(|v| gone.contains(&v.object.cache))
        .cloned()
        .collect();
    for version in versions {
        for page in by_version.remove(&version).unwrap_or_default() {
            writer.drop_written(&page).await?;
        }
    }
    for object in directory.objects() {
        if !gone.contains(&object.cache) {
            continue;
        }
        if let Some(entry) = directory.remove(&object) {
            writer.index().invalidate_current(&object);
            if let Ok(record) = super::coordinator::delete_record(&entry.version) {
                super::coordinator::journal(local, record).await?;
            }
        }
    }
    let mut gc = local.writes.gc.borrow_mut();
    for cache in gone {
        gc.absent.remove(&cache);
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
    let mut answered = false;
    for node in page_zero_members(local, membership, &version.object, scope).await? {
        match probe(local, membership, &node, version, scope).await {
            Knowledge::Known(state) => return Ok(Knowledge::Known(state)),
            Knowledge::Unknown => answered = true,
            Knowledge::Silent => {}
        }
    }
    Ok(if answered {
        Knowledge::Unknown
    } else {
        Knowledge::Silent
    })
}
