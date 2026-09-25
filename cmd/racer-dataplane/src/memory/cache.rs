//! Worker-local idle verified pages and original ciphertext; independent of disk clock.
use super::pool::{BufferPool, CiphertextPage, VerifiedPage};
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
    pub fn get(&self, _page: &PageId) -> Result<Option<VerifiedPage>> {
        pending("memory.get")
    }
    pub fn ciphertext(&self, _page: &PageId) -> Result<Option<CiphertextPage>> {
        pending("memory.ciphertext")
    }
    pub fn publish(&self, _plain: VerifiedPage, _cipher: CiphertextPage) -> Result<()> {
        pending("memory.publish")
    }
    pub fn evict_idle(&self, _bytes: usize) -> Result<usize> {
        pending("memory.evict_idle")
    }
}
#[cfg(test)]
mod tests { /* Busy lease protection, independent eviction, exact ciphertext reuse. */
}
