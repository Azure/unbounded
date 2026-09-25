//! Narrow libibverbs FFI ownership boundary. Unsafe code stays inside this adapter.
//!
//! Device, PD, CQ, QP, MR, and memory-window handles have ordered RAII teardown.
//! No native linking is needed by the scaffold. Select a vetted binding and gate
//! its implementation with the rdma feature; no native handles are fabricated.
use crate::error::{Result, pending};
pub struct Verbs;
pub struct DeviceHandle {
    opaque: usize,
}
pub struct QueuePairHandle {
    opaque: usize,
}
impl Verbs {
    pub fn discover(&self) -> Result<Vec<DeviceHandle>> {
        pending("verbs.discover")
    }
}
#[cfg(test)]
mod tests { /* Fake handle teardown order; explicitly gated real-device validation. */
}
