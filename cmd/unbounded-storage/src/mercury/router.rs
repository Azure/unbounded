// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Peer-routing trait and static implementation.
//!
//! The transport layer needs to translate an outbound request into a
//! concrete `PeerId` before it can look up a wire address and issue
//! the RPC. `PeerRouter` is that mapping; `StaticPeer` is the trivial
//! impl that always returns the same peer and is the routing layer
//! used during bring-up, in single-peer deployments, and in tests.
//!
//! The trait is generic over the request type `R` so the same routing
//! abstraction can be reused by any transport-level request: a
//! `bufferpool::Req` for `bulk_get`, a control-plane RPC, or a test
//! placeholder. The router itself is the only piece of the transport
//! that needs to know how to interpret request shape into peer
//! identity, so callers depend on `PeerRouter<R>` rather than wiring
//! peer selection ad-hoc into each call site.
//!
//! Richer routers (consistent-hashed, replica-aware, leader-tracking,
//! etc.) live in their own files when they appear; this file is
//! intentionally small.

use super::peer::PeerId;

/// Maps an outbound request to the peer that should serve it.
///
/// Implementations must be `Send + Sync` so a single router instance
/// can be shared across the executor's worker tasks. `route` is
/// expected to be cheap; if a real implementation needs to do
/// non-trivial work (e.g. read a routing table behind a lock) it
/// should still return synchronously - the transport calls `route`
/// on the hot path of every outbound request.
pub trait PeerRouter<R>: Send + Sync {
    /// Returns `Some(peer)` if the router knows where to send `req`.
    /// Returning `None` is a routing failure that the caller surfaces
    /// as an error.
    fn route(&self, req: &R) -> Option<PeerId>;
}

/// Routes every request to the same peer. Useful for bring-up,
/// single-peer deployments, and tests.
///
/// `StaticPeer` is `Copy` so it can be embedded directly in transport
/// configs and cloned freely; there is no state to share.
#[derive(Debug, Clone, Copy)]
pub struct StaticPeer(pub PeerId);

impl<R> PeerRouter<R> for StaticPeer {
    fn route(&self, _req: &R) -> Option<PeerId> {
        Some(self.0)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn static_peer_returns_configured_peer() {
        let r = StaticPeer(PeerId(7));
        struct Fake;
        let p: Option<PeerId> = <StaticPeer as PeerRouter<Fake>>::route(&r, &Fake);
        assert_eq!(p, Some(PeerId(7)));
    }

    #[test]
    fn distinct_static_peers_route_distinctly() {
        struct Fake;
        let a = StaticPeer(PeerId(1));
        let b = StaticPeer(PeerId(2));
        let pa: Option<PeerId> = <StaticPeer as PeerRouter<Fake>>::route(&a, &Fake);
        let pb: Option<PeerId> = <StaticPeer as PeerRouter<Fake>>::route(&b, &Fake);
        assert_eq!(pa, Some(PeerId(1)));
        assert_eq!(pb, Some(PeerId(2)));
        assert_ne!(pa, pb);
    }
}
