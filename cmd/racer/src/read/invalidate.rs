//! Drop one cached version: locally, then best effort on the page owners.
//!
//! A client invalidation removes the version's metadata and pages from this
//! node, then sends a signed `prefetch invalidate` peer operation to the
//! [`REPLICAS`] ranked owners of each page. Page zero's owners also hold the
//! version's metadata, so they drop it too. Receivers drop local state only
//! and never send the operation on, so it cannot amplify.
//!
//! Bounds: at most [`MAX_PAGES`] pages are named per invalidation, so a larger
//! object drops its metadata and its first pages only. At most
//! [`MAX_CONCURRENT`] invalidations run on a node at once; more are refused
//! `Overloaded`. Peer failures are counted and never fail the client.

use super::Coordinator;
use crate::error::{Error, Result};
use crate::model::{ObjectVersion, PageId, PageNumber};
use crate::peer::protocol::{Operation as PeerOperation, PeerResponse, PrefetchAction};
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use crate::telemetry::Event;
use futures::StreamExt;
use racer_core::prefetch::{MAX_OPERATION_PAGES, PageResult};
use racer_wire::control::NodeId;
use std::collections::BTreeMap;
use std::sync::atomic::Ordering;
use std::time::Duration;

/// Most pages one invalidation names. Later pages of a larger object stay
/// until evicted or refetched under another version.
pub const MAX_PAGES: u64 = 1024;

/// Most invalidations running at once on one node.
pub const MAX_CONCURRENT: usize = 8;

/// Owners ranked for each page, the same count reads place copies on.
pub const REPLICAS: usize = ::topology::REPLICAS;

/// Longest a client waits for peers before it is answered.
const PROPAGATION_TIMEOUT: Duration = Duration::from_secs(5);

/// Peer operations sent at once by one invalidation.
const PEER_CONCURRENCY: usize = 8;

/// What one client invalidation did.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Outcome {
    /// Pages named, starting at page zero.
    pub pages: u64,
    /// Whether the object had more pages than [`MAX_PAGES`].
    pub truncated: bool,
    /// Peer operations sent.
    pub sent: usize,
    /// Peer operations that failed or were refused.
    pub failed: usize,
}

/// Releases one concurrent invalidation slot.
struct Permit<'a>(&'a std::sync::atomic::AtomicUsize);

impl Drop for Permit<'_> {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

/// The pages to name for an object of `length` bytes cut at `page_size`, and
/// whether that list was capped. Unknown length names page zero only.
pub(crate) fn pages_for(length: Option<u64>, page_size: u64) -> (Vec<u64>, bool) {
    let count = match length {
        Some(length) if page_size > 0 => length.div_ceil(page_size).max(1),
        _ => 1,
    };
    ((0..count.min(MAX_PAGES)).collect(), count > MAX_PAGES)
}

impl Coordinator {
    /// Client entry: drop `version` on this node, then on its page owners.
    /// Succeeds once local state is dropped, whatever peers answer.
    pub(crate) async fn invalidate(
        &self,
        version: ObjectVersion,
        scope: &RequestScope,
    ) -> Result<Outcome> {
        let metrics = &self.fill.metrics;
        let directory = self.prefetch_directory().clone();
        let counter = &directory.invalidations;
        if counter
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |n| {
                (n < MAX_CONCURRENT).then_some(n + 1)
            })
            .is_err()
        {
            metrics.record(Event::InvalidateRejected, 1);
            return Err(Error::Overloaded);
        }
        let _permit = Permit(counter);
        metrics.record(Event::InvalidateRequest, 1);
        // Learn the length before local state, which holds it, is dropped.
        let length = directory
            .retained_metadata(&version, scope)
            .await
            .ok()
            .flatten()
            .map(|metadata| metadata.length);
        let page_size = self.page_size().map(|size| size.get()).unwrap_or(0);
        let (pages, truncated) = pages_for(length, page_size);
        if truncated {
            metrics.record(Event::InvalidateTruncated, 1);
        }
        directory
            .invalidate_version(&version, &pages, scope)
            .await?;
        let mut outcome = Outcome {
            pages: pages.len() as u64,
            truncated,
            ..Outcome::default()
        };
        let deadline = scope
            .deadline
            .0
            .min(scope.clock().now() + PROPAGATION_TIMEOUT);
        if let Ok(child) = scope.child(scope.request, deadline) {
            let (sent, failed) = self.propagate(&version, &pages, &child).await;
            outcome.sent = sent;
            outcome.failed = failed;
        } else {
            outcome.failed = 1;
        }
        metrics.record(Event::InvalidatePeerSent, outcome.sent as u64);
        metrics.record(Event::InvalidatePeerFailed, outcome.failed as u64);
        Ok(outcome)
    }

    /// Send `prefetch invalidate` to every other ranked owner of `pages`.
    /// Returns operations sent and operations that failed.
    async fn propagate(
        &self,
        version: &ObjectVersion,
        pages: &[u64],
        scope: &RequestScope,
    ) -> (usize, usize) {
        let membership = match self.cache_membership(&version.object.cache) {
            Ok(membership) => membership,
            Err(_) => return (0, 1),
        };
        let me = self.local_node();
        let mut groups: BTreeMap<NodeId, Vec<u64>> = BTreeMap::new();
        let mut failed = 0;
        for &page in pages {
            let ranked = self
                .fill
                .dependencies
                .candidates
                .candidates_scoped(membership.clone(), &version.object, PageNumber(page), scope)
                .await;
            match ranked {
                Ok(candidates) => {
                    for owner in candidates.ordered.into_iter().take(REPLICAS) {
                        if owner != me {
                            groups.entry(owner).or_default().push(page);
                        }
                    }
                }
                Err(_) => failed += 1,
            }
        }
        let context = OriginContext {
            object: version.object.clone(),
            metadata: None,
            authorization: None,
        };
        let operations: Vec<(NodeId, Vec<u64>)> = groups
            .into_iter()
            .flat_map(|(owner, pages)| {
                pages
                    .chunks(MAX_OPERATION_PAGES)
                    .map(|chunk| (owner.clone(), chunk.to_vec()))
                    .collect::<Vec<_>>()
            })
            .collect();
        let sent = operations.len();
        let membership = &membership;
        let context = &context;
        let results = futures::stream::iter(operations.into_iter().map(|(owner, pages)| {
            let operation = PeerOperation::Prefetch {
                version: version.clone(),
                action: PrefetchAction::Invalidate { pages },
            };
            async move {
                matches!(
                    self.peer_prefetch(membership, &owner, context, &operation, scope)
                        .await,
                    Ok(PeerResponse::Prefetch(_))
                )
            }
        }))
        .buffer_unordered(PEER_CONCURRENCY)
        .collect::<Vec<bool>>()
        .await;
        failed += results.iter().filter(|ok| !**ok).count();
        (sent, failed)
    }

    /// Peer entry: drop `version`'s metadata and `pages` on this node only.
    pub(crate) async fn serve_invalidate(
        &self,
        version: &ObjectVersion,
        pages: &[u64],
        scope: &RequestScope,
    ) -> Vec<PageResult> {
        self.fill.metrics.record(Event::InvalidatePeerServed, 1);
        let result = match self
            .prefetch_directory()
            .invalidate_version(version, pages, scope)
            .await
        {
            Ok(()) => PageResult::Admitted,
            Err(_) => PageResult::Failed(racer_core::prefetch::FailCode::Unavailable),
        };
        vec![result; pages.len()]
    }

    /// Drop `version` on this worker: its metadata when `metadata`, and each
    /// listed page from memory, the write queue, and the disk index.
    pub(crate) fn invalidate_worker(&self, version: &ObjectVersion, pages: &[u64], metadata: bool) {
        let dependencies = &self.fill.dependencies;
        let index = dependencies.writer.index();
        if metadata {
            self.metadata.catalog().invalidate_version(version);
            index.invalidate_version(version);
        }
        for &number in pages {
            let page = PageId {
                version: version.clone(),
                number: PageNumber(number),
            };
            dependencies.memory.invalidate_page(&page);
            dependencies.writer.discard_queued(&page);
            index.invalidate_page(&page);
            self.fill.gpu_copies.forget(&page);
            dependencies.candidates.forget_holders(&page);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pages_cover_the_object_up_to_the_cap() {
        assert_eq!(pages_for(None, 16), (vec![0], false));
        assert_eq!(pages_for(Some(0), 16), (vec![0], false));
        assert_eq!(pages_for(Some(16), 16), (vec![0], false));
        assert_eq!(pages_for(Some(17), 16), (vec![0, 1], false));
        assert_eq!(pages_for(Some(5), 0), (vec![0], false));
        let (pages, truncated) = pages_for(Some(16 * (MAX_PAGES + 1)), 16);
        assert!(truncated);
        assert_eq!(pages.len() as u64, MAX_PAGES);
        assert_eq!(pages.last(), Some(&(MAX_PAGES - 1)));
    }
}
