//! Isolated test crate using the real topology and boundary modules. This allows
//! component verification while unrelated worktree components are being edited.
#![allow(dead_code)]

const MAX_FIELD_BYTES: usize = 8192;

#[path = "../error.rs"]
mod error;
#[path = "../model/identity.rs"]
pub mod identity;
mod model {
    pub use crate::identity;
}
#[path = "../runtime/deadline.rs"]
pub mod deadline;
mod runtime {
    pub use crate::deadline;
}
mod fixtures;
pub mod graph;
mod hash;
pub mod health;
pub mod membership;
pub mod paths;
pub mod placement;
pub mod rails;
mod topology {
    pub(crate) use crate::{fixtures, health, membership, placement, rails};
}
