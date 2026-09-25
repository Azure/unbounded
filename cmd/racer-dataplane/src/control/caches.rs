//! Cache definitions and lifecycle events. Removal drains sockets, keys, and I/O.
use crate::{
    error::{Result, pending},
    model::identity::CacheId,
};
use std::path::PathBuf;
#[derive(Clone, Debug)]
pub struct CacheDefinition {
    pub id: CacheId,
    pub client_socket: PathBuf,
    pub origin_socket: PathBuf,
    pub socket_mode: u32,
}
pub enum CacheEvent {
    Add(CacheDefinition),
    Update(CacheDefinition),
    Remove(CacheId),
}
pub struct CacheRegistry;
impl CacheRegistry {
    pub fn reconcile(&self, _definitions: &[CacheDefinition]) -> Result<Vec<CacheEvent>> {
        pending("caches.reconcile")
    }
}
#[cfg(test)]
mod tests { /* Duplicate paths, safe owned-path removal, updates, and drain ordering. */
}
