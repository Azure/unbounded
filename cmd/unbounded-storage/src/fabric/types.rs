// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Public-facing fabric value types. Kept thin so later phases can
//! attach transport metadata without touching the bufferpool surface.

use crate::bufferpool::PageRef;

/// Opaque peer identifier minted by the fabric/connection layer.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct PeerId(pub u64);

pub type ConnectionId = PeerId;

#[derive(Clone, Debug)]
pub struct ConnectionSpec {
    pub peer: ConnectionId,
    pub wire_addr: String,
    pub hca_numa: Option<u16>,
    /// Topology labels for the peer, propagated from
    /// `PeerSpec.labels`. Consumed by the p2p FingerTable's
    /// topology-distance heuristic when peers are added to the local
    /// routing table; ignored by the fabric itself.
    pub labels: Vec<String>,
}

/// Newtype around a buffer-pool page so the fabric layer can attach
/// transport-specific metadata in later phases without churning the
/// bufferpool types.
#[derive(Copy, Clone, Debug)]
pub struct Page(pub PageRef);

impl From<PageRef> for Page {
    fn from(p: PageRef) -> Self {
        Page(p)
    }
}

impl From<Page> for PageRef {
    fn from(p: Page) -> Self {
        p.0
    }
}
