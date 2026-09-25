//! Environment and deployment configuration, validated before startup.
//!
//! Default to at most eight total userspace threads (four I/O/crypto worker pairs).
//! Shares and rail alignment come
//! exclusively from accepted controller membership, derived from Node annotations.

use crate::{
    error::{Result, pending},
    model::{
        identity::{ClusterId, NodeId},
        limits::Limits,
    },
};
use std::{path::PathBuf, time::Duration};

pub struct Config {
    pub cluster: ClusterId,
    pub node: NodeId,
    /// Total thread cap, minimum two; odd caps round down to complete worker pairs.
    /// Control and diagnostics run on I/O threads within this budget.
    pub max_threads: usize,
    pub enable_rdma: bool,
    pub control_endpoint: String,
    pub peer_listen: std::net::SocketAddr,
    pub diagnostics_listen: std::net::SocketAddr,
    pub trust_bundle: PathBuf,
    pub service_account_token: PathBuf,
    pub secret_directory: PathBuf,
    /// Node-private persistent keys, separate from projected Secrets and slabs.
    pub identity_directory: PathBuf,
    pub slab_directory: PathBuf,
    pub slab_bytes: u64,
    pub segment_bytes: u64,
    pub free_segment_reserve: usize,
    pub limits: Limits,
    pub request_timeout: Duration,
    pub reader_stall_timeout: Duration,
    pub shutdown_timeout: Duration,
}

impl Config {
    pub fn from_env() -> Result<Self> {
        pending("config.from_env")
    }

    /// Check arithmetic and progress reserves; filesystem alignment is additionally
    /// discovered and checked by store::slab at open, not guessed from this config.
    pub fn validate(&self) -> Result<()> {
        pending("config.validate")
    }
}

#[cfg(test)]
mod tests {
    // Cover invalid budgets, cluster/node identities, cpusets, and overflowing geometry.
}
