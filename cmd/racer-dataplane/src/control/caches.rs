//! Cache definitions and lifecycle events. Removal drains sockets, keys, and I/O.
//!
//! Socket paths are fixed: /run/racer/<cache name>/client/socket and
//! /run/racer/<cache name>/origin/socket. Separate endpoint directories let pods
//! mount only the endpoint authorized by a future admission controller. The
//! dataplane owns the client listener; the application adapter owns the origin
//! listener. Never unlink an adapter-owned origin socket during cache removal.
use crate::{
    error::{Result, pending},
    model::identity::CacheId,
};
use std::path::PathBuf;
#[derive(Clone, Debug)]
pub struct CacheDefinition {
    pub id: CacheId,
    /// ClusterCache name, distinct from its UID and one safe path component.
    pub name: String,
    /// Must equal /run/racer/<name>/client/socket, not an arbitrary supplied path.
    pub client_socket: PathBuf,
    /// Must equal /run/racer/<name>/origin/socket, not an arbitrary supplied path.
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
    /// Reject unsafe/duplicate names, noncanonical paths, and socket paths exceeding
    /// the platform UDS limit. Validate before filesystem access; prevent symlink
    /// traversal outside the endpoint directories during socket lifecycle work.
    pub fn reconcile(&self, _definitions: &[CacheDefinition]) -> Result<Vec<CacheEvent>> {
        pending("caches.reconcile")
    }
}
#[cfg(test)]
mod tests { /* Duplicate paths, safe owned-path removal, updates, and drain ordering. */
}
