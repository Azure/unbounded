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
        pool::{BufferPool, CiphertextPage, VerifiedPage},
    },
    model::{context::OriginContext, identity::PageId, metadata::ObjectMetadata},
    origin::client::Origin,
    peer::requester::PeerClient,
    runtime::{admission::Admission, deadline::RequestScope},
    security::{aead::PageCrypto, credentials::CredentialCrypto},
    store::{reader::StoreReader, writer::StoreWriter},
    topology::membership::MembershipLease,
};
use std::rc::Rc;

pub struct PageResult {
    pub metadata: ObjectMetadata,
    pub plaintext: VerifiedPage,
    pub ciphertext: CiphertextPage,
}
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
}
pub struct Fill {
    dependencies: FillDependencies,
}
impl Fill {
    pub fn new(dependencies: FillDependencies) -> Self {
        Self { dependencies }
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
