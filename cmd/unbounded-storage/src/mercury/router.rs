// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use crate::bufferpool::PeerId;
use crate::mercury::error::Result;

/// Maps a typed request to a peer. The transport calls this exactly
/// once per `bulk_get` before forwarding.
pub trait PeerRouter<R> {
    fn route(&self, req: &R) -> Result<PeerId>;
}

/// Constant router: every request goes to the same peer. Convenient
/// for unit tests and for simple two-node topologies.
pub struct StaticPeer(pub PeerId);

impl<R> PeerRouter<R> for StaticPeer {
    fn route(&self, _req: &R) -> Result<PeerId> {
        Ok(self.0)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn static_peer_returns_configured_peer_for_any_request() {
        let r = StaticPeer(PeerId(7));
        assert_eq!(PeerRouter::<()>::route(&r, &()).unwrap(), PeerId(7));
        assert_eq!(PeerRouter::<u32>::route(&r, &42).unwrap(), PeerId(7));
        // Confirm it really is constant across distinct request values.
        assert_eq!(PeerRouter::<u32>::route(&r, &0).unwrap(), PeerId(7));
    }
}
