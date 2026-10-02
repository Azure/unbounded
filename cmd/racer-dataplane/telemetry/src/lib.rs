//! Fixed metrics and bounded diagnostic primitives without application policy.

pub mod metrics;
pub mod ring;

pub use metrics::{Lease, Metric, Metrics};
pub use ring::Ring;
