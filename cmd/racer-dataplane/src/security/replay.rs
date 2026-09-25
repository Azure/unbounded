//! Bounded atomic check-and-record by signer/epoch; saturation rejects new work.
use crate::{
    error::{Result, pending},
    model::identity::NodeId,
};
/// Node-wide partitioned admission state; replaying on another reactor must fail.
pub struct ReplayState;
pub struct ReplayWindow {
    state: std::sync::Arc<ReplayState>,
    capacity: usize,
}
#[derive(Clone, Copy)]
pub struct ReplayNonce(pub [u8; 24]);
pub struct Freshness {
    pub nonce: ReplayNonce,
    pub timestamp: std::time::SystemTime,
    pub session_challenge: [u8; 32],
}
impl ReplayWindow {
    pub fn new(state: std::sync::Arc<ReplayState>, capacity: usize) -> Self {
        Self { state, capacity }
    }
    /// Standardize skew and restart challenge binding before accepting wire input.
    pub fn admit(&self, _signer: &NodeId, _freshness: &Freshness) -> Result<()> {
        pending("replay.admit")
    }
}
#[cfg(test)]
mod tests { /* Concurrent duplicates, saturation without nonce eviction, restart. */
}
