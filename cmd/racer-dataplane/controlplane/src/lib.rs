//! Bounded control feeds and atomic immutable publication without domain policy.
//!
//! I/O, credentials, and install hooks stay on their owner thread. Only owned
//! preparation closures and immutable shared generations cross thread boundaries.

pub mod feed;
pub mod published;
pub mod rollout;

pub use feed::{Client, Codec, Credentials, FailureClass, Feed, Host, Sync, Target};
pub use published::{Generation, Published, Retention};
pub use rollout::Rollout;

/// Mechanism failures that adapters map to their domain error vocabulary.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// An older version or incompatible same-version document was submitted.
    Replay,
    /// A retained part changed without changing its version or shared allocation.
    Conflict,
    /// A bounded resource or externally pinned generation prevents progress.
    Capacity,
    /// A worker or acknowledgment does not belong to the active proposal.
    Stale,
    /// A required resource or worker is not ready yet.
    Pending,
    /// Synchronization state was poisoned or background preparation panicked.
    Internal,
}
