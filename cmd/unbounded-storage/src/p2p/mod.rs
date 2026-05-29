// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Peer-to-peer stripe DHT.
//!
//! Each node deterministically computes a k-arc finger table from
//! the cluster's sorted node list and routes requests recursively
//! via Chord closest-preceding-finger lookup. This layer is
//! concerned purely with routing topology; what a node does with a
//! response (caching, admission, eviction) is the storage layer's
//! decision and lives entirely under [`crate::storage`].

mod fingers;
mod ring;
mod types;

#[cfg(test)]
mod tests;

pub use fingers::{FingerTable, FingerTableConfig};
pub(crate) use ring::{
    WILDCARD_LABEL, node_to_ring, rendezvous_hash, ring_distance, topology_distance,
};
pub use ring::{splitmix64, stripe_to_ring};
pub use types::{NodeId, P2pReq, PeerEntry, RingId, TopologyLabels};
