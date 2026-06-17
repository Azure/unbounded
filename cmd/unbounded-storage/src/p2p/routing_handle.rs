// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Live-reloadable routing surface shared across a shard's p2p
//! consumers.
//!
//! The finger table and the `node -> peer` map are derived together
//! from the active neighborhood projected by the config graph. Three independent
//! consumers read them on the hot path:
//!
//! * [`crate::p2p::FingerRouter`] (the first-hop lookup wrapped by
//!   every [`crate::fabric::FabricTransport`]),
//! * [`crate::p2p::RoutedTransport`] (its local-ownership pre-check),
//! * [`crate::p2p::RecursiveHandler`] (its server-side classify and
//!   forward path).
//!
//! When peers change, all three must observe the new surface
//! atomically and without a restart. [`RoutingHandle`] is the seam:
//! a cheaply-clonable handle over a single [`ArcSwap`] of a
//! [`RoutingSnapshot`]. Every consumer holds a clone of the SAME
//! handle, so one [`RoutingHandle::store`] publishes a new snapshot to
//! all of them at once. The swap is wait-free for readers, which
//! matters because the [`RecursiveHandler`] is read concurrently by
//! the fabric RPC worker threads while the shard thread performs the
//! store.
//!
//! [`RecursiveHandler`]: crate::p2p::RecursiveHandler

use std::collections::HashMap;
use std::sync::Arc;

use arc_swap::ArcSwap;

use crate::fabric::PeerId;
use crate::p2p::{FingerTable, NodeId};

/// Immutable view of the routing surface. Published as a unit so a
/// reader can never observe a new finger table paired with a stale
/// `node -> peer` map (or vice versa).
///
/// Cloning is cheap: both fields are `Arc`s, so a clone is two atomic
/// refcount bumps and shares the underlying tables.
#[derive(Clone)]
pub struct RoutingSnapshot {
    pub fingers: Arc<FingerTable>,
    pub node_to_peer: Arc<HashMap<NodeId, PeerId>>,
}

#[derive(Clone, Default)]
pub struct RouteTableSnapshot {
    pub routes: HashMap<String, RoutingSnapshot>,
}

/// Shared, live-reloadable handle to a [`RoutingSnapshot`].
///
/// Cloning is cheap (an `Arc` bump) and every clone observes stores
/// made through any other clone. This is the single seam through
/// which a shard republishes routing after a peer-set change.
#[derive(Clone)]
pub struct RoutingHandle(Arc<ArcSwap<RoutingSnapshot>>);

#[derive(Clone)]
pub struct RouteTableHandle(Arc<ArcSwap<RouteTableSnapshot>>);

impl RoutingHandle {
    /// Build a handle seeded with the initial routing surface.
    pub fn new(fingers: Arc<FingerTable>, node_to_peer: Arc<HashMap<NodeId, PeerId>>) -> Self {
        Self(Arc::new(ArcSwap::from_pointee(RoutingSnapshot {
            fingers,
            node_to_peer,
        })))
    }

    /// Atomically publish a new routing surface to every clone of this
    /// handle. Wait-free for concurrent readers.
    pub fn store(&self, fingers: Arc<FingerTable>, node_to_peer: Arc<HashMap<NodeId, PeerId>>) {
        self.0.store(Arc::new(RoutingSnapshot {
            fingers,
            node_to_peer,
        }));
    }

    /// Load the current snapshot. The two maps are guaranteed mutually
    /// consistent (published together by [`Self::store`]).
    pub fn snapshot(&self) -> Arc<RoutingSnapshot> {
        self.0.load_full()
    }
}

impl RouteTableHandle {
    pub const LEGACY_ROUTE_ID: &'static str = "";

    pub fn new(routes: HashMap<String, RoutingSnapshot>) -> Self {
        Self(Arc::new(ArcSwap::from_pointee(RouteTableSnapshot {
            routes,
        })))
    }

    pub fn from_snapshot(snapshot: RouteTableSnapshot) -> Self {
        Self(Arc::new(ArcSwap::from_pointee(snapshot)))
    }

    pub fn empty() -> Self {
        Self::new(HashMap::new())
    }

    pub fn store(&self, routes: HashMap<String, RoutingSnapshot>) {
        self.0.store(Arc::new(RouteTableSnapshot { routes }));
    }

    pub fn store_snapshot(&self, snapshot: RouteTableSnapshot) {
        self.0.store(Arc::new(snapshot));
    }

    pub fn snapshot(&self) -> Arc<RouteTableSnapshot> {
        self.0.load_full()
    }

    pub fn route(&self, neighborhood_id: &str) -> Option<RoutingSnapshot> {
        self.snapshot().routes.get(neighborhood_id).cloned()
    }

    pub fn route_for_req<R: crate::bufferpool::Req + ?Sized>(
        &self,
        req: &R,
    ) -> Option<RoutingSnapshot> {
        let route_id = req
            .neighborhood_id()
            .map_or(Self::LEGACY_ROUTE_ID, String::as_str);
        self.route(route_id)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::p2p::{FingerTableConfig, PeerEntry, TopologyLabels, node_to_ring};

    fn peer(node: u64) -> PeerEntry {
        PeerEntry {
            node: NodeId(node),
            ring: node_to_ring(NodeId(node)),
            labels: TopologyLabels(vec!["r".to_string()]),
        }
    }

    fn table(local: u64, peers: &[u64]) -> Arc<FingerTable> {
        let entries: Vec<PeerEntry> = peers.iter().map(|&n| peer(n)).collect();
        Arc::new(FingerTable::build(
            peer(local),
            &entries,
            FingerTableConfig { k: 8 },
        ))
    }

    fn map(nodes: &[u64]) -> Arc<HashMap<NodeId, PeerId>> {
        Arc::new(nodes.iter().map(|&n| (NodeId(n), PeerId(n))).collect())
    }

    #[test]
    fn store_is_observed_by_existing_clones() {
        // A clone taken BEFORE the store must observe the new snapshot:
        // every clone shares one ArcSwap, so this is the property the
        // shard relies on to fan a reload out to consumers it already
        // handed clones to.
        let handle = RoutingHandle::new(table(1, &[]), map(&[1]));
        let consumer = handle.clone();
        assert!(consumer.snapshot().node_to_peer.contains_key(&NodeId(1)));
        assert!(!consumer.snapshot().node_to_peer.contains_key(&NodeId(2)));

        handle.store(table(1, &[2]), map(&[1, 2]));

        let snap = consumer.snapshot();
        assert!(snap.node_to_peer.contains_key(&NodeId(2)));
        assert_eq!(snap.node_to_peer.get(&NodeId(2)), Some(&PeerId(2)));
    }
}
