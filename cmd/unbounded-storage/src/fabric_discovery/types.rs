// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::collections::HashSet;
use std::io;
use std::net::SocketAddr;

use serde::{Deserialize, Serialize};

use super::MAX_RESPONSE_BYTES;

pub const MANIFEST_VERSION: u32 = 1;

/// Identity and typed fabric listeners published by one daemon process.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Manifest {
    pub version: u32,
    pub peer_id: u64,
    pub process_incarnation: u64,
    pub listeners: Vec<Listener>,
}

impl Manifest {
    pub fn new(peer_id: u64, process_incarnation: u64, listeners: Vec<Listener>) -> Self {
        Self {
            version: MANIFEST_VERSION,
            peer_id,
            process_incarnation,
            listeners,
        }
    }

    pub(crate) fn validate(&self) -> io::Result<()> {
        if self.version != MANIFEST_VERSION {
            return Err(invalid_data(format!(
                "unsupported fabric discovery manifest version {}",
                self.version
            )));
        }
        if self.peer_id == 0 {
            return Err(invalid_data(
                "fabric discovery peer identity must be non-zero",
            ));
        }
        if self.process_incarnation == 0 {
            return Err(invalid_data(
                "fabric discovery process incarnation must be non-zero",
            ));
        }
        if self.listeners.len() > 1024 {
            return Err(invalid_data(
                "fabric discovery manifest contains too many listeners",
            ));
        }
        if self.listeners.is_empty() {
            return Err(invalid_data(
                "fabric discovery manifest contains no listeners",
            ));
        }

        let mut ids = HashSet::with_capacity(self.listeners.len());
        for listener in &self.listeners {
            let address_valid = match listener.transport {
                Transport::Tcp => listener
                    .address
                    .parse::<SocketAddr>()
                    .is_ok_and(|address| address.port() != 0),
                Transport::Rdma => {
                    listener
                        .address
                        .parse::<SocketAddr>()
                        .is_ok_and(|address| address.port() != 0)
                        || valid_native_address(&listener.address)
                }
            };
            if listener.id.is_empty()
                || listener.id.len() > 256
                || listener.address.is_empty()
                || listener.address.len() > 4096
                || !address_valid
                || !ids.insert(listener.id.as_str())
            {
                return Err(invalid_data(
                    "fabric discovery manifest contains an invalid listener",
                ));
            }
        }
        Ok(())
    }

    pub(crate) fn to_json(&self) -> io::Result<Vec<u8>> {
        self.validate()?;
        let mut manifest = self.clone();
        manifest
            .listeners
            .sort_unstable_by(|left, right| left.id.cmp(&right.id));
        let body = serde_json::to_vec(&manifest)
            .map_err(|error| invalid_data(format!("serialize discovery manifest: {error}")))?;
        if body.len() > MAX_RESPONSE_BYTES {
            return Err(invalid_data("fabric discovery response exceeds 256 KiB"));
        }
        Ok(body)
    }

    pub(crate) fn from_json(body: &[u8]) -> io::Result<Self> {
        let mut manifest: Self = serde_json::from_slice(body)
            .map_err(|error| invalid_data(format!("invalid discovery manifest JSON: {error}")))?;
        manifest.validate()?;
        manifest
            .listeners
            .sort_unstable_by(|left, right| left.id.cmp(&right.id));
        Ok(manifest)
    }
}

fn valid_native_address(address: &str) -> bool {
    let Some(hex) = address.strip_prefix("hex:") else {
        return false;
    };
    !hex.is_empty() && hex.len() % 2 == 0 && hex.bytes().all(|byte| byte.is_ascii_hexdigit())
}

/// One stable, typed fabric listener advertised by the process.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Listener {
    pub id: String,
    pub transport: Transport,
    pub address: String,
}

/// Data transport accepted by a discovery listener.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Transport {
    Tcp,
    Rdma,
}

fn invalid_data(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.into())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn manifest() -> Manifest {
        Manifest::new(
            42,
            7,
            vec![Listener {
                id: "fabric-0".to_string(),
                transport: Transport::Rdma,
                address: "hex:0102".to_string(),
            }],
        )
    }

    #[test]
    fn json_round_trip_is_typed_and_versioned() {
        let manifest = manifest();
        let body = manifest.to_json().unwrap();
        assert_eq!(Manifest::from_json(&body).unwrap(), manifest);
        assert_eq!(
            String::from_utf8(body).unwrap(),
            r#"{"version":1,"peer_id":42,"process_incarnation":7,"listeners":[{"id":"fabric-0","transport":"rdma","address":"hex:0102"}]}"#
        );
    }

    #[test]
    fn codec_orders_listeners_by_stable_id() {
        let mut manifest = manifest();
        manifest.listeners.insert(
            0,
            Listener {
                id: "fabric-2".to_string(),
                transport: Transport::Tcp,
                address: "127.0.0.1:9002".to_string(),
            },
        );
        manifest.listeners.insert(
            0,
            Listener {
                id: "fabric-1".to_string(),
                transport: Transport::Tcp,
                address: "127.0.0.1:9001".to_string(),
            },
        );

        let body = manifest.to_json().unwrap();
        let mut differently_ordered = manifest.clone();
        differently_ordered.listeners.reverse();
        assert_eq!(differently_ordered.to_json().unwrap(), body);

        let decoded = Manifest::from_json(&body).unwrap();
        let ids: Vec<&str> = decoded
            .listeners
            .iter()
            .map(|listener| listener.id.as_str())
            .collect();
        assert_eq!(ids, ["fabric-0", "fabric-1", "fabric-2"]);
        assert_eq!(manifest.listeners[0].id, "fabric-1");

        let unsorted_json = br#"{"version":1,"peer_id":1,"process_incarnation":1,"listeners":[{"id":"z","transport":"tcp","address":"127.0.0.1:1"},{"id":"a","transport":"rdma","address":"hex:01"}]}"#;
        let decoded = Manifest::from_json(unsorted_json).unwrap();
        assert_eq!(decoded.listeners[0].id, "a");
        assert_eq!(decoded.listeners[1].id, "z");
    }

    #[test]
    fn parser_rejects_unknown_fields_versions_and_transport() {
        assert!(
            Manifest::from_json(
                br#"{"version":2,"peer_id":1,"process_incarnation":1,"listeners":[]}"#
            )
            .is_err()
        );
        assert!(
            Manifest::from_json(
                br#"{"version":1,"peer_id":1,"process_incarnation":1,"listeners":[],"extra":true}"#
            )
            .is_err()
        );
        assert!(Manifest::from_json(br#"{"version":1,"peer_id":1,"process_incarnation":1,"listeners":[{"id":"x","transport":"udp","address":"a"}]}"#).is_err());
    }

    #[test]
    fn validation_rejects_zero_peer_and_incarnation() {
        let mut zero_peer = manifest();
        zero_peer.peer_id = 0;
        assert!(zero_peer.validate().is_err());
        assert!(zero_peer.to_json().is_err());

        let mut zero_incarnation = manifest();
        zero_incarnation.process_incarnation = 0;
        assert!(zero_incarnation.validate().is_err());
        assert!(zero_incarnation.to_json().is_err());
    }

    #[test]
    fn validation_rejects_invalid_or_duplicate_listeners() {
        let mut duplicate_listener = manifest();
        duplicate_listener
            .listeners
            .push(duplicate_listener.listeners[0].clone());
        assert!(duplicate_listener.validate().is_err());
        assert!(duplicate_listener.to_json().is_err());

        let duplicate_json = br#"{"version":1,"peer_id":1,"process_incarnation":1,"listeners":[{"id":"same","transport":"tcp","address":"a"},{"id":"same","transport":"rdma","address":"b"}]}"#;
        assert!(Manifest::from_json(duplicate_json).is_err());

        for listener in [
            Listener {
                id: String::new(),
                transport: Transport::Tcp,
                address: "address".to_string(),
            },
            Listener {
                id: "x".repeat(257),
                transport: Transport::Tcp,
                address: "address".to_string(),
            },
            Listener {
                id: "fabric-0".to_string(),
                transport: Transport::Tcp,
                address: String::new(),
            },
            Listener {
                id: "fabric-0".to_string(),
                transport: Transport::Tcp,
                address: "x".repeat(4097),
            },
        ] {
            let invalid = Manifest::new(1, 1, vec![listener]);
            assert!(invalid.validate().is_err());
            assert!(invalid.to_json().is_err());
        }
    }
}
