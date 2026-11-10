//! Node-local prefetch tickets on the client socket (`/v1/prefetches`).
//!
//! A client submits a ticket of object ranges. The node normalizes them to
//! `(version, page)` units, schedules missing pages to their rank-0 owners,
//! and the owners fill them through the normal flight table and persist them
//! with normal eviction. Nothing is pinned. See `designs/racer-prefetch.md`.

use crate::error::{Error, Result};

pub mod api;
pub mod metrics;
pub mod registry;
pub mod service;
#[cfg(test)]
mod tests;

pub use racer_core::prefetch::Priority;

/// Status refs listed per page of `?refs=`.
pub const REFS_PAGE: usize = 256;

/// `RACER_PREFETCH_*` settings.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Config {
    pub max_body_bytes: u64,
    pub max_refs: u64,
    pub max_tickets: u64,
    pub max_ttl_secs: u64,
    pub ticket_retention_secs: u64,
    pub max_wait_secs: u64,
    pub default_pending_bytes: u64,
    pub max_pending_bytes: u64,
    pub node_pending_bytes: u64,
    pub concurrent_fills: u64,
    pub origin_bytes_per_sec: u64,
    pub max_attempts: u64,
}

const MIB16: u64 = 16 * 1024 * 1024;

impl Default for Config {
    fn default() -> Self {
        Self {
            max_body_bytes: 4 * 1024 * 1024,
            max_refs: 16384,
            max_tickets: 1024,
            max_ttl_secs: 3600,
            ticket_retention_secs: 300,
            max_wait_secs: 60,
            default_pending_bytes: 1 << 30,
            max_pending_bytes: 16 << 30,
            node_pending_bytes: 64 << 30,
            concurrent_fills: 16,
            origin_bytes_per_sec: 0,
            max_attempts: 5,
        }
    }
}

impl Config {
    pub fn validate(self) -> Result<()> {
        let ok = (4096..=64 * 1024 * 1024).contains(&self.max_body_bytes)
            && (1..=1_048_576).contains(&self.max_refs)
            && (1..=65536).contains(&self.max_tickets)
            && (1..=86400).contains(&self.max_ttl_secs)
            && self.ticket_retention_secs <= 86400
            && (1..=600).contains(&self.max_wait_secs)
            && (MIB16..=self.max_pending_bytes).contains(&self.default_pending_bytes)
            && (MIB16..=1 << 50).contains(&self.max_pending_bytes)
            && (MIB16..=1 << 52).contains(&self.node_pending_bytes)
            && (1..=1024).contains(&self.concurrent_fills)
            && self.origin_bytes_per_sec <= 1 << 40
            && (1..=32).contains(&self.max_attempts);
        if ok {
            Ok(())
        } else {
            Err(Error::InvalidConfiguration)
        }
    }

    /// Bounds the client-socket parser enforces before reading a body.
    pub fn limits(&self) -> racer_wire::object::prefetch::Limits {
        racer_wire::object::prefetch::Limits {
            max_body_bytes: self.max_body_bytes,
            max_wait: std::time::Duration::from_secs(self.max_wait_secs),
        }
    }
}
