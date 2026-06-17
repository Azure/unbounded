// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! `PeerRouter` backed by the p2p finger table.
//!
//! Maps a request's stripe to the owning peer's `PeerId` via the
//! Chord `next_hop` lookup, then the `node -> peer` map. This is the
//! router the client-side `FabricTransport` consults to pick the
//! first hop; the server-side `RecursiveHandler` reuses the same type
//! to forward to the next hop. Both share the same `FingerTable` and
//! node->peer map so ownership decisions stay consistent across the
//! transport, the local-ownership pre-check, and the forwarding path.

use std::collections::HashMap;
use std::sync::Arc;

use crate::bufferpool::Req;
use crate::fabric::{PeerId, PeerRouter};
use crate::p2p::{FingerTable, NodeId, RouteTableHandle, RoutingHandle, stripe_to_ring};

/// `PeerRouter` backed by a finger table. Maps a request's stripe to
/// the owning peer's `PeerId` via the Chord `next_hop` lookup, then
/// the `node -> peer` map. The routing surface lives behind a shared
/// [`RoutingHandle`] so a peer-set change republished through that
/// handle is observed here without rebuilding the router.
pub struct FingerRouter {
    routing: RoutingHandle,
}

pub struct ChainFingerRouter {
    routes: RouteTableHandle,
}

impl FingerRouter {
    /// Build a router over a freshly-seeded [`RoutingHandle`]. Retained
    /// for callers (and tests) that hold the two maps directly; the
    /// resulting router owns its own handle and is not live-reloadable
    /// from elsewhere.
    pub fn new(fingers: Arc<FingerTable>, node_to_peer: Arc<HashMap<NodeId, PeerId>>) -> Self {
        Self::from_handle(RoutingHandle::new(fingers, node_to_peer))
    }

    /// Build a router that shares an existing [`RoutingHandle`]. A
    /// `store` on any clone of `routing` is observed by this router's
    /// next `route` call, which is how a shard fans a peer-set reload
    /// out to every consumer at once.
    pub fn from_handle(routing: RoutingHandle) -> Self {
        Self { routing }
    }
}

impl ChainFingerRouter {
    pub fn new(routes: RouteTableHandle) -> Self {
        Self { routes }
    }
}

impl<R: Req> PeerRouter<R> for FingerRouter {
    fn route(&self, req: &R) -> Option<PeerId> {
        let target = stripe_to_ring(req.key());
        let snap = self.routing.snapshot();
        snap.fingers
            .next_hop(target)
            .and_then(|pe| snap.node_to_peer.get(&pe.node).copied())
    }
}

impl<R: Req> PeerRouter<R> for ChainFingerRouter {
    fn route(&self, req: &R) -> Option<PeerId> {
        let snap = self.routes.route_for_req(req)?;
        snap.fingers
            .next_hop(stripe_to_ring(req.key()))
            .and_then(|pe| snap.node_to_peer.get(&pe.node).copied())
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;
    use std::sync::Arc;

    use crate::bufferpool::{Req, StripeKey};
    use crate::fabric::{PeerId, PeerRouter};
    use crate::p2p::{
        FingerTable, FingerTableConfig, NodeId, PeerEntry, RingId, TopologyLabels, node_to_ring,
        stripe_to_ring,
    };

    use super::FingerRouter;

    struct TestReq(StripeKey);

    impl Req for TestReq {
        fn key(&self) -> StripeKey {
            self.0
        }
    }

    fn peer(node: u64) -> PeerEntry {
        PeerEntry {
            node: NodeId(node),
            ring: node_to_ring(NodeId(node)),
            labels: TopologyLabels(vec!["r".to_string()]),
        }
    }

    /// A `StripeKey` whose leading 8 bytes little-endian equal
    /// `ring`, so `stripe_to_ring(key) == RingId(ring)`.
    fn key_for_ring(ring: u64) -> StripeKey {
        let mut k = [0u8; 32];
        k[..8].copy_from_slice(&ring.to_le_bytes());
        StripeKey(k)
    }

    fn node_to_peer_map(nodes: &[u64]) -> Arc<HashMap<NodeId, PeerId>> {
        Arc::new(nodes.iter().map(|&n| (NodeId(n), PeerId(n))).collect())
    }

    #[test]
    fn key_for_ring_round_trips() {
        assert_eq!(stripe_to_ring(key_for_ring(12345)), RingId(12345));
    }

    #[test]
    fn local_owned_stripe_routes_to_backend_and_router_returns_none() {
        // Single-node table: local owns everything, so next_hop is
        // always None for any target. FingerRouter therefore yields
        // no peer (backend arm).
        let local = peer(1);
        let fingers = Arc::new(FingerTable::build(
            local.clone(),
            &[],
            FingerTableConfig { k: 8 },
        ));
        let node_to_peer = node_to_peer_map(&[1]);

        let router = FingerRouter::new(fingers.clone(), node_to_peer);
        // Pick a target that is not the local ring id so the owner
        // check still exercises the predecessor path (single node
        // owns everything regardless).
        let req = TestReq(key_for_ring(local.ring.0.wrapping_add(999)));
        assert!(fingers.next_hop(stripe_to_ring(req.key())).is_none());
        assert!(PeerRouter::<TestReq>::route(&router, &req).is_none());
    }

    #[test]
    fn peer_owned_stripe_routes_to_peer() {
        // Two-node ring. Choose a target that the other node owns
        // from local's perspective; next_hop is Some and the router
        // maps it to the peer's PeerId.
        let local = peer(1);
        let other = peer(2);
        let fingers = Arc::new(FingerTable::build(
            local.clone(),
            std::slice::from_ref(&other),
            FingerTableConfig { k: 8 },
        ));
        let node_to_peer = node_to_peer_map(&[1, 2]);
        let router = FingerRouter::new(fingers.clone(), node_to_peer);

        // Target == other's ring id: local forwards to other.
        let req = TestReq(key_for_ring(other.ring.0));
        let hop = fingers
            .next_hop(stripe_to_ring(req.key()))
            .expect("local should forward to a peer");
        assert_eq!(hop.node, other.node);
        assert_eq!(
            PeerRouter::<TestReq>::route(&router, &req),
            Some(PeerId(other.node.0)),
        );
    }
}
