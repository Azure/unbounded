//! Validate and atomically publish complete immutable state; retain last good state.
use super::{caches::CacheDefinition, wire::Publication};
use crate::{
    error::{Result, pending},
    topology::membership::MembershipLease,
};
use std::sync::Arc;
pub struct Snapshot {
    pub sequence: u64,
    pub membership: MembershipLease,
    pub caches: Vec<CacheDefinition>,
}
pub type SnapshotLease = Arc<Snapshot>;
/// One node-wide publication cell. Its implementation publishes immutable leases
/// atomically; worker handles never become independent authorities for membership.
pub struct PublishedState;
pub struct SnapshotStore {
    published: Arc<PublishedState>,
    retained_limit: usize,
}
impl SnapshotStore {
    pub fn new(published: Arc<PublishedState>, retained_limit: usize) -> Self {
        Self {
            published,
            retained_limit,
        }
    }
    pub fn current(&self) -> Result<SnapshotLease> {
        pending("snapshot.current")
    }
    pub fn publish(&self, _publication: Publication) -> Result<SnapshotLease> {
        pending("snapshot.publish")
    }
}
#[cfg(test)]
mod tests { /* Atomicity, stale versions, compatibility, and bounded leased history. */
}
