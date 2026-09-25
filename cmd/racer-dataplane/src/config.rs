//! Environment and deployment configuration, validated before startup.
//!
//! Default to four positive placement shares, at most eight allowed CPU threads,
//! and aligned rails. Do not advertise a local weight outside accepted membership.

use crate::{
    error::{Result, pending},
    model::{identity::NodeId, limits::Limits},
};
use std::{num::NonZeroU32, path::PathBuf, time::Duration};

pub struct Config {
    pub node: NodeId,
    pub shares: NonZeroU32,
    pub max_threads: usize,
    pub aligned_rails: bool,
    pub enable_rdma: bool,
    pub control_endpoint: String,
    pub peer_listen: std::net::SocketAddr,
    pub diagnostics_listen: std::net::SocketAddr,
    pub trust_bundle: PathBuf,
    pub service_account_token: PathBuf,
    pub secret_directory: PathBuf,
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
    // Cover invalid budgets, positive shares, cpusets, and overflowing geometry.
}
