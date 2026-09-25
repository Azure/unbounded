//! Fixed-label metrics. Never label by object keys, ETags, or request headers.
use crate::error::{Result, pending};
pub struct Metrics;
pub enum Event {
    MemoryHit,
    DiskHit,
    PeerHit,
    OriginFill,
    DirtyDiscard,
    Overload,
    CorruptMiss,
}
impl Metrics {
    pub fn record(&self, _event: Event, _amount: u64) -> Result<()> {
        pending("metrics.record")
    }
}
#[cfg(test)]
mod tests { /* Assert cardinality bounds and lease gauges returning to baseline. */
}
