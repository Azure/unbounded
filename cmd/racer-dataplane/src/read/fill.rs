//! Local disk -> ranked peer -> authorized origin acquisition and publication.
//!
//! Reserve progress memory/dirty capacity before download. Decrypt once per fill;
//! encrypt origin data once. Publish only verified whole pages, with original
//! ciphertext queued asynchronously on candidates. Disk failure may discard dirty
//! bytes. Origin 412 does not prove old copies absent from other permitted caches.
use super::{candidates::CandidatePolicy, flight::Flights};
use crate::{
    error::{Operation, deferred},
    memory::{
        cache::MemoryCache,
        pool::{BufferPool, CiphertextPage},
    },
    model::{context::OriginContext, identity::PageId, metadata::ObjectMetadata},
    origin::client::Origin,
    peer::requester::PeerClient,
    runtime::{admission::Admission, deadline::RequestScope},
    security::{aead::PageCrypto, credentials::CredentialCrypto},
    store::{reader::StoreReader, writer::StoreWriter},
    topology::membership::MembershipLease,
};
use std::{rc::Rc, sync::Arc};

pub use crate::memory::page::PageResult;
pub struct FillDependencies {
    pub memory: Rc<MemoryCache>,
    pub buffers: Rc<BufferPool>,
    pub disk: Rc<StoreReader>,
    pub writer: Rc<StoreWriter>,
    pub peers: Rc<dyn PeerClient>,
    pub origin: Rc<dyn Origin>,
    pub candidates: Rc<CandidatePolicy>,
    pub flights: Rc<Flights>,
    pub crypto: Rc<PageCrypto>,
    pub credentials: Rc<CredentialCrypto>,
    pub admission: Rc<Admission>,
    /// Publish immutable descriptors to the page-zero owner over bounded commands.
    /// Page workers keep their own page-attached descriptor even if that catalog evicts it.
    pub metadata_owner: Arc<super::dispatch::WorkerDirectory>,
}
pub struct Fill {
    dependencies: FillDependencies,
}
impl Fill {
    pub fn new(dependencies: FillDependencies) -> Self {
        Self { dependencies }
    }
    /// Local shard's memory/pending/disk descriptors, without starting acquisition.
    /// Used by WorkerDirectory's bounded retained-metadata lookup on a pinned miss.
    pub fn retained_metadata<'a>(
        &'a self,
        _version: &'a crate::model::identity::ObjectVersion,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Option<crate::model::metadata::VersionMetadata>> {
        deferred("fill.retained_metadata")
    }
    pub fn acquire<'a>(
        &'a self,
        _page: PageId,
        _membership: MembershipLease,
        _context: &'a OriginContext,
        _scope: &'a RequestScope,
    ) -> Operation<'a, PageResult> {
        deferred("fill.acquire")
    }
    /// Strictly local completed/pending copy or join of existing work; never starts
    /// another acquisition or contacts origin. Ciphertext preserves its nonce/tag.
    pub fn copy_only<'a>(
        &'a self,
        _page: &'a PageId,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Option<(ObjectMetadata, CiphertextPage)>> {
        deferred("fill.copy_only")
    }
}
#[cfg(test)]
mod tests { /* One decrypt, bad disk miss, pending peer copy, write discard, 412 copies. */
}
