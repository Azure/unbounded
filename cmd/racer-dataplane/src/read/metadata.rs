//! Versioned metadata cache and page-zero-worker refresh singleflight.
//!
//! Keep old lengths separately from the current-version pointer. TTL gates fresh
//! admission; explicit pins may use expired metadata. A zero-TTL refresh admits its
//! waiters once. Clock discontinuities invalidate uncertain freshness. Cache entries
//! contain no origin context, Authorization, or opaque adapter metadata header.
use super::candidates::CandidatePolicy;
use crate::{
    error::{Operation, deferred},
    model::{
        context::OriginContext,
        metadata::{MetadataSelector, ObjectMetadata},
    },
    origin::client::Origin,
    peer::requester::PeerClient,
    runtime::deadline::RequestScope,
    security::credentials::CredentialCrypto,
    store::index::Index,
    topology::membership::MembershipLease,
};
use std::{rc::Rc, sync::Arc};
/// An empty bootstrap is metadata-only: it never constructs an encrypted page.
pub enum BootstrapResult {
    Empty(ObjectMetadata),
    Page(super::fill::PageResult),
}
pub struct MetadataDependencies {
    pub index: Rc<Index>,
    pub fill: Rc<super::fill::Fill>,
    pub owners: Arc<super::dispatch::WorkerDirectory>,
}
pub struct MetadataService {
    candidates: Rc<CandidatePolicy>,
    origin: Rc<dyn Origin>,
    peers: Rc<dyn PeerClient>,
    credentials: Rc<CredentialCrypto>,
    capacity: usize,
    storage: MetadataDependencies,
}
impl MetadataService {
    pub fn new(
        candidates: Rc<CandidatePolicy>,
        origin: Rc<dyn Origin>,
        peers: Rc<dyn PeerClient>,
        credentials: Rc<CredentialCrypto>,
        capacity: usize,
        storage: MetadataDependencies,
    ) -> Self {
        Self {
            candidates,
            origin,
            peers,
            credentials,
            capacity,
            storage,
        }
    }
    /// Missing pinned metadata may require a conditional page-zero probe; never
    /// substitute current-version length to resolve a suffix or final-page range.
    pub fn resolve<'a>(
        &'a self,
        _selector: MetadataSelector,
        _membership: MembershipLease,
        _context: &'a OriginContext,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ObjectMetadata> {
        deferred("metadata.resolve")
    }
    pub fn copy_only<'a>(
        &'a self,
        _selector: MetadataSelector,
        _context: &'a OriginContext,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Option<ObjectMetadata>> {
        deferred("metadata.copy_only")
    }
    /// Runs on the page-zero owner. Resolve metadata, then acquire page zero via
    /// the same Fill as pinned reads. Require identical version AND total length;
    /// bounded version-change retries happen before any response headers escape.
    /// Missing pinned descriptors may use owners.retained_metadata before probing
    /// peers/origin; fresh admission must still revalidate an absent/expired pointer.
    pub fn bootstrap<'a>(
        &'a self,
        _selector: MetadataSelector,
        _membership: MembershipLease,
        _context: &'a OriginContext,
        _scope: &'a RequestScope,
    ) -> Operation<'a, BootstrapResult> {
        deferred("metadata.bootstrap")
    }
}
#[cfg(test)]
mod tests { /* Refresh coalescing, zero TTL, old pin, clock jumps, no credential cache. */
}
