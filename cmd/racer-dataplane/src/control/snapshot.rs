//! Validate and atomically publish complete immutable state; retain last good state.
use super::{
    caches::CacheDefinition,
    wire::{Publication, PublicationSequence},
};
use crate::{
    error::{Result, pending},
    model::identity::ClusterId,
    topology::membership::MembershipLease,
};
use std::sync::Arc;
pub struct Snapshot {
    pub cluster: ClusterId,
    pub sequence: PublicationSequence,
    pub membership: MembershipLease,
    pub caches: Vec<CacheDefinition>,
}
pub type SnapshotLease = Arc<Snapshot>;
/// One node-wide publication cell. Its implementation publishes immutable leases
/// atomically; worker handles never become independent authorities for membership.
pub struct PublishedState;
pub struct SnapshotStore {
    cluster: ClusterId,
    published: Arc<PublishedState>,
    retained_limit: usize,
}
impl SnapshotStore {
    pub fn new(cluster: ClusterId, published: Arc<PublishedState>, retained_limit: usize) -> Self {
        Self {
            cluster,
            published,
            retained_limit,
        }
    }
    /// Cursor advances only after complete validation and atomic acceptance.
    pub fn cursor(&self) -> Result<Option<PublicationSequence>> {
        pending("snapshot.cursor")
    }
    pub fn current(&self) -> Result<SnapshotLease> {
        pending("snapshot.current")
    }
    /// Reject cluster mismatch, rollback, conflicting replay, or invalid members.
    /// Skipped sequences are legal; disconnected nodes retain the last good state.
    pub fn publish(&self, _publication: Publication) -> Result<SnapshotLease> {
        pending("snapshot.publish")
    }
}
#[cfg(test)]
mod tests { /* Atomicity, stale versions, compatibility, and bounded leased history. */
}
