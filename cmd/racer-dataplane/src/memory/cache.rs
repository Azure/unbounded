//! Worker-local idle verified pages and original ciphertext; independent of disk clock.
use super::{
    page::{CiphertextCopy, PageResult},
    pool::BufferPool,
};
use crate::{
    error::{Result, pending},
    model::identity::PageId,
};
use std::rc::Rc;
pub struct MemoryCache {
    pool: Rc<BufferPool>,
}
impl MemoryCache {
    pub fn new(pool: Rc<BufferPool>) -> Self {
        Self { pool }
    }
    pub fn get(&self, _page: &PageId) -> Result<Option<PageResult>> {
        pending("memory.get")
    }
    pub fn ciphertext(&self, _page: &PageId) -> Result<Option<CiphertextCopy>> {
        pending("memory.ciphertext")
    }
    /// Validate matching identities and full-page bounds before retaining the bundle.
    pub fn publish(&self, _page: PageResult) -> Result<()> {
        pending("memory.publish")
    }
    pub fn metadata(
        &self,
        _version: &crate::model::identity::ObjectVersion,
    ) -> Result<Option<crate::model::metadata::VersionMetadata>> {
        pending("memory.metadata")
    }
    pub fn evict_idle(&self, _bytes: usize) -> Result<usize> {
        pending("memory.evict_idle")
    }
}
#[cfg(test)]
mod tests { /* Busy lease protection, independent eviction, exact ciphertext reuse. */
}
