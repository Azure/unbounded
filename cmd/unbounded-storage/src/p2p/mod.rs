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

mod finger_router;
mod fingers;
mod handler;
mod ring;
mod routing_handle;
mod transport;
mod types;

#[cfg(test)]
mod tests;

pub use finger_router::{ChainFingerRouter, FingerRouter};
pub use fingers::{FingerTable, FingerTableConfig};
pub use handler::{RecursiveHandler, RecursiveHandlerError, RecursiveHandlerStream};
pub use ring::{node_id_from_name, node_to_ring, splitmix64, stripe_to_ring};
pub use routing_handle::{RouteTableHandle, RouteTableSnapshot, RoutingHandle, RoutingSnapshot};
pub use transport::{RoutedStream, RoutedTransport};
pub use types::{NodeId, P2pReq, PeerEntry, RingId, TopologyTags};
