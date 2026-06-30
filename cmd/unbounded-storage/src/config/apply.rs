// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Pure-function adapters from config types to the daemon's internal
//! types. Kept separate so the schema crate never has to depend on the
//! daemon's runtime types and vice versa.

use std::net::SocketAddr;

use crate::fabric::{ConnectionSpec, FabricAddress, PeerId};
use crate::p2p::node_id_from_name;

use super::schema::{PeerSpec, peer_spec};

pub fn peer_spec_to_connection(p: &PeerSpec) -> ConnectionSpec {
    let node_id = node_id_from_name(&p.name);
    ConnectionSpec {
        peer: PeerId(node_id.0),
        address: peer_address(p),
        hca_numa: None,
        tags: p.tags.clone(),
    }
}

fn peer_address(p: &PeerSpec) -> FabricAddress {
    match p.config.as_ref() {
        Some(peer_spec::Config::Tcp(cfg)) => FabricAddress::socket(cfg.addr.clone()),
        Some(peer_spec::Config::Rdma(cfg)) => rdma_address(&cfg.addr),
        None => FabricAddress::native(""),
    }
}

fn rdma_address(addr: &str) -> FabricAddress {
    if addr.parse::<SocketAddr>().is_ok() {
        FabricAddress::socket(addr.to_string())
    } else {
        FabricAddress::native(addr.to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use crate::config::schema::{RdmaPeerConfig, TcpPeerConfig};

    #[test]
    fn tcp_peer_spec_maps_directly() {
        let p = PeerSpec {
            name: "node-a".to_string(),
            tags: vec!["us-west".to_string(), "rack7".to_string()],
            config: Some(peer_spec::Config::Tcp(TcpPeerConfig {
                addr: "10.0.0.1:9000".into(),
            })),
        };
        let c = peer_spec_to_connection(&p);
        assert_eq!(c.peer, PeerId(node_id_from_name("node-a").0));
        assert_eq!(c.address, FabricAddress::socket("10.0.0.1:9000"));
        assert_eq!(c.hca_numa, None);
        assert_eq!(c.tags, p.tags);
    }

    #[test]
    fn rdma_peer_spec_maps_directly() {
        let p = PeerSpec {
            name: "node-a".to_string(),
            tags: vec!["us-west".to_string(), "rack7".to_string()],
            config: Some(peer_spec::Config::Rdma(RdmaPeerConfig {
                addr: "hex:01020304".into(),
                addrs: Vec::new(),
            })),
        };
        let c = peer_spec_to_connection(&p);
        assert_eq!(c.peer, PeerId(node_id_from_name("node-a").0));
        assert_eq!(c.address, FabricAddress::native("hex:01020304"));
        assert_eq!(c.hca_numa, None);
        assert_eq!(c.tags, p.tags);
    }

    #[test]
    fn rdma_socket_peer_spec_maps_to_socket_address() {
        let p = PeerSpec {
            name: "node-a".to_string(),
            tags: vec!["us-west".to_string(), "rack7".to_string()],
            config: Some(peer_spec::Config::Rdma(RdmaPeerConfig {
                addr: "10.0.0.1:9000".into(),
                addrs: Vec::new(),
            })),
        };
        let c = peer_spec_to_connection(&p);
        assert_eq!(c.peer, PeerId(node_id_from_name("node-a").0));
        assert_eq!(c.address, FabricAddress::socket("10.0.0.1:9000"));
        assert_eq!(c.hca_numa, None);
        assert_eq!(c.tags, p.tags);
    }
}
