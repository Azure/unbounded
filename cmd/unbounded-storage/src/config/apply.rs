// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Pure-function adapters from config types to the daemon's internal
//! types. Kept separate so the schema crate never has to depend on the
//! daemon's runtime types and vice versa.

use crate::fabric::{ConnectionSpec, FabricAddress, PeerId};
use crate::p2p::node_id_from_name;

use super::schema::PeerSpec;

pub fn peer_spec_to_connection(p: &PeerSpec) -> ConnectionSpec {
    let node_id = node_id_from_name(&p.name);
    ConnectionSpec {
        peer: PeerId(node_id.0),
        address: FabricAddress::socket(p.discovery_addr.clone()),
        hca_numa: None,
        tags: p.tags.clone(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn peer_spec_maps_discovery_address() {
        let p = PeerSpec {
            name: "node-a".to_string(),
            tags: vec!["us-west".to_string(), "rack7".to_string()],
            discovery_addr: "10.0.0.1:9101".into(),
        };
        let c = peer_spec_to_connection(&p);
        assert_eq!(c.peer, PeerId(node_id_from_name("node-a").0));
        assert_eq!(c.address, FabricAddress::socket("10.0.0.1:9101"));
        assert_eq!(c.hca_numa, None);
        assert_eq!(c.tags, p.tags);
    }
}
