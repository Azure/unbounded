//! Explicit execution ownership. No library may introduce an unbudgeted thread pool.

pub mod admission;
pub mod affinity;
pub mod channel;
pub mod deadline;
pub mod reactor;
pub mod worker;
