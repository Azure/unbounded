//! Immutable placement, bidirectional routing, and end-to-end rail selection.
pub mod graph;
pub mod health;
pub mod membership;
pub mod paths;
pub mod placement;
pub mod rails;

mod equal_cost;
mod hash;

/// Algorithm changes require a new version and new interoperability vectors.
/// Latest supported contract; production defaults to v2 until coordinated opt-in.
pub const ALGORITHM_VERSION: u32 = 3;

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum RoutingAlgorithm {
    #[default]
    V2,
    V3,
}

#[cfg(test)]
mod fixtures;
