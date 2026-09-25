//! Immutable placement, bidirectional routing, and end-to-end rail selection.
pub mod graph;
pub mod health;
pub mod membership;
pub mod paths;
pub mod placement;
pub mod rails;

mod hash;

/// Algorithm changes require a new version and new interoperability vectors.
pub const ALGORITHM_VERSION: u32 = 1;

#[cfg(test)]
mod fixtures;
