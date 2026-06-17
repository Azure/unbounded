// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Pure-function adapters from config types to the daemon's internal
//! types. Kept separate so the schema crate never has to depend on the
//! daemon's runtime types and vice versa.

use crate::fabric::ConnectionSpec;
use crate::fabric::PeerId;

use super::schema::PeerSpec;

pub fn peer_spec_to_connection(p: &PeerSpec) -> ConnectionSpec {
    ConnectionSpec {
        peer: PeerId(p.id),
        wire_addr: p.address.clone(),
        hca_numa: p.hca_numa.map(|n| n as u16),
        labels: p.labels.clone(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn peer_spec_maps_directly() {
        let p = PeerSpec {
            id: 42,
            address: "10.0.0.1:9000".into(),
            hca_numa: Some(1),
            labels: vec!["us-west".to_string(), "rack7".to_string()],
        };
        let c = peer_spec_to_connection(&p);
        assert_eq!(c.peer, PeerId(42));
        assert_eq!(c.wire_addr, "10.0.0.1:9000");
        assert_eq!(c.hca_numa, Some(1));
        assert_eq!(c.labels, p.labels);
    }
}
