//! Local link circuits, bounded probes, and backoff. Never edits placement.
use crate::{
    error::{Result, pending},
    model::identity::NodeId,
};
pub struct LinkHealth;
#[derive(Clone, Copy, Debug)]
pub enum LinkOutcome {
    Success,
    Timeout,
    Refused,
    ProtocolFailure,
}
impl LinkHealth {
    pub fn observe(&self, _neighbor: &NodeId, _outcome: LinkOutcome) -> Result<()> {
        pending("health.observe_link")
    }
    pub fn available(&self, _neighbor: &NodeId) -> Result<bool> {
        pending("health.link_available")
    }
}
#[cfg(test)]
mod tests { /* Circuit transitions, bounded probes, jitter, and placement invariance. */
}
