// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Public-facing fabric value types. Kept thin so later phases can
//! attach transport metadata without touching the bufferpool surface.

use std::fmt;

use crate::bufferpool::PageRef;

/// Opaque peer identifier minted by the fabric/connection layer.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct PeerId(pub u64);

pub type ConnectionId = PeerId;

/// Peer endpoint address understood by libfabric's connection manager.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum FabricAddress {
    /// Numeric IP socket address for RDMA-CM/TCP, for example
    /// `10.0.0.1:9000`.
    Socket(String),
    /// Provider-native address bytes encoded as `hex:<fi_getname-bytes>`.
    Native(String),
}

impl FabricAddress {
    pub fn socket(addr: impl Into<String>) -> Self {
        Self::Socket(addr.into())
    }

    pub fn native(addr: impl Into<String>) -> Self {
        Self::Native(addr.into())
    }
}

impl fmt::Display for FabricAddress {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            FabricAddress::Socket(addr) => write!(f, "{addr}"),
            FabricAddress::Native(addr) => write!(f, "native:{addr}"),
        }
    }
}

#[derive(Clone, Debug)]
pub struct ConnectionSpec {
    pub peer: ConnectionId,
    pub address: FabricAddress,
    pub hca_numa: Option<u16>,
    /// Topology tags for the peer, propagated from
    /// `PeerSpec.tags`. Consumed by the p2p FingerTable's
    /// topology-distance heuristic when peers are added to the local
    /// routing table; ignored by the fabric itself.
    pub tags: Vec<String>,
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
