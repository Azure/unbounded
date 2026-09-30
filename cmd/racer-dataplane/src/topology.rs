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
/// Latest supported contract; topology changes require coordinated rollout.
pub const ALGORITHM_VERSION: u32 = 5;

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum RoutingAlgorithm {
    V2,
    V3,
    V4,
    #[default]
    V5,
}

impl RoutingAlgorithm {
    pub const fn radix(self) -> usize {
        match self {
            Self::V2 | Self::V3 | Self::V4 => 18,
            Self::V5 => 32,
        }
    }

    pub const fn max_degree(self) -> usize {
        2 * self.radix()
    }
}

/// Shared capacity bound for every supported topology, including first-hop masks.
pub const MAX_DEGREE: usize = RoutingAlgorithm::V5.max_degree();
const _: () = assert!(MAX_DEGREE <= u64::BITS as usize);

#[cfg(test)]
mod fixtures;
