// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Pure-function adapters from config types to the daemon's internal
//! types. Kept separate so the schema crate never has to depend on the
//! daemon's runtime types and vice versa.

use crate::fabric::{ConnectionSpec, FabricAddress, PeerId};

use super::schema::{PeerSpec, peer_spec};

pub fn peer_spec_to_connection(p: &PeerSpec) -> ConnectionSpec {
    ConnectionSpec {
        peer: PeerId(p.id),
        address: peer_address(p),
        hca_numa: p.hca_numa().map(|n| n as u16),
        tags: p.tags.clone(),
    }
}

fn peer_address(p: &PeerSpec) -> FabricAddress {
    match p.config.as_ref() {
        Some(peer_spec::Config::Tcp(cfg)) => FabricAddress::socket(cfg.addr.clone()),
        Some(peer_spec::Config::Rdma(cfg)) => FabricAddress::native(cfg.addr.clone()),
        None => FabricAddress::native(""),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use crate::config::schema::{RdmaPeerConfig, TcpPeerConfig};

    #[test]
    fn tcp_peer_spec_maps_directly() {
        let p = PeerSpec {
            id: 42,
            tags: vec!["us-west".to_string(), "rack7".to_string()],
            config: Some(peer_spec::Config::Tcp(TcpPeerConfig {
                addr: "10.0.0.1:9000".into(),
            })),
        };
        let c = peer_spec_to_connection(&p);
        assert_eq!(c.peer, PeerId(42));
        assert_eq!(c.address, FabricAddress::socket("10.0.0.1:9000"));
        assert_eq!(c.hca_numa, None);
        assert_eq!(c.tags, p.tags);
    }

    #[test]
    fn rdma_peer_spec_maps_directly() {
        let p = PeerSpec {
            id: 42,
            tags: vec!["us-west".to_string(), "rack7".to_string()],
            config: Some(peer_spec::Config::Rdma(RdmaPeerConfig {
                addr: "hex:01020304".into(),
                hca_numa: Some(1),
            })),
        };
        let c = peer_spec_to_connection(&p);
        assert_eq!(c.peer, PeerId(42));
        assert_eq!(c.address, FabricAddress::native("hex:01020304"));
        assert_eq!(c.hca_numa, Some(1));
        assert_eq!(c.tags, p.tags);
    }
}
