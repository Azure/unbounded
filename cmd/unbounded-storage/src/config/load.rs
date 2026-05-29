// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Read and validate the daemon's TOML configuration.

use std::collections::HashSet;
use std::fmt;
use std::fs;
use std::io;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};

use super::schema::{Config, PeerTransport};

#[derive(Debug)]
pub enum ConfigError {
    Io(io::Error),
    Toml(toml::de::Error),
    DuplicatePeer(u64),
    DuplicateDiskPath(PathBuf),
    InvalidTcpAddr { peer_id: u64, addr: String },
    InvalidRdmaHex { peer_id: u64 },
    EmptyDiskPath,
    MissingLocalNodeId,
    LocalNodeIdCollidesWithPeer(u64),
    ZeroFingersPerNode,
}

impl fmt::Display for ConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ConfigError::Io(e) => write!(f, "io error reading config: {e}"),
            ConfigError::Toml(e) => write!(f, "toml parse error: {e}"),
            ConfigError::DuplicatePeer(id) => write!(f, "duplicate peer id: {id}"),
            ConfigError::DuplicateDiskPath(p) => {
                write!(f, "duplicate disk path: {}", p.display())
            }
            ConfigError::InvalidTcpAddr { peer_id, addr } => {
                write!(f, "peer {peer_id}: invalid tcp socket address {addr:?}")
            }
            ConfigError::InvalidRdmaHex { peer_id } => {
                write!(f, "peer {peer_id}: invalid rdma hex address")
            }
            ConfigError::EmptyDiskPath => write!(f, "disk path must not be empty"),
            ConfigError::MissingLocalNodeId => write!(
                f,
                "p2p.local_node_id must be set when [[peers]] are configured: a multi-node \
                 deployment requires a stable local node id to avoid silent NodeId(0) collisions"
            ),
            ConfigError::LocalNodeIdCollidesWithPeer(id) => write!(
                f,
                "p2p.local_node_id {id} collides with a peer id: the local node and a peer \
                 cannot share a node id, or the p2p finger table will silently drop that peer"
            ),
            ConfigError::ZeroFingersPerNode => write!(
                f,
                "p2p.fingers_per_node must be greater than zero: the p2p subsystem needs at \
                 least one finger per node"
            ),
        }
    }
}

impl std::error::Error for ConfigError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            ConfigError::Io(e) => Some(e),
            ConfigError::Toml(e) => Some(e),
            _ => None,
        }
    }
}

impl From<io::Error> for ConfigError {
    fn from(e: io::Error) -> Self {
        ConfigError::Io(e)
    }
}

impl From<toml::de::Error> for ConfigError {
    fn from(e: toml::de::Error) -> Self {
        ConfigError::Toml(e)
    }
}

impl Config {
    pub fn load(path: impl AsRef<Path>) -> Result<Self, ConfigError> {
        load(path.as_ref())
    }
}

pub fn load(path: &Path) -> Result<Config, ConfigError> {
    let raw = fs::read_to_string(path)?;
    let cfg: Config = toml::from_str(&raw)?;
    validate(&cfg)?;
    Ok(cfg)
}

fn validate(cfg: &Config) -> Result<(), ConfigError> {
    if cfg.p2p.fingers_per_node == 0 {
        return Err(ConfigError::ZeroFingersPerNode);
    }
    if !cfg.peers.is_empty() && cfg.p2p.local_node_id.is_none() {
        return Err(ConfigError::MissingLocalNodeId);
    }

    let mut seen_peers: HashSet<u64> = HashSet::new();
    for p in &cfg.peers {
        if !seen_peers.insert(p.id) {
            return Err(ConfigError::DuplicatePeer(p.id));
        }
        if cfg.p2p.local_node_id == Some(p.id) {
            return Err(ConfigError::LocalNodeIdCollidesWithPeer(p.id));
        }
        match p.transport {
            PeerTransport::Tcp => {
                if p.address.parse::<SocketAddr>().is_err() {
                    return Err(ConfigError::InvalidTcpAddr {
                        peer_id: p.id,
                        addr: p.address.clone(),
                    });
                }
            }
            PeerTransport::Rdma => {
                if !is_valid_even_hex(&p.address) {
                    return Err(ConfigError::InvalidRdmaHex { peer_id: p.id });
                }
            }
        }
    }

    let mut seen_paths: HashSet<&PathBuf> = HashSet::new();
    for d in &cfg.disks {
        if d.path.as_os_str().is_empty() {
            return Err(ConfigError::EmptyDiskPath);
        }
        if !seen_paths.insert(&d.path) {
            return Err(ConfigError::DuplicateDiskPath(d.path.clone()));
        }
    }
    Ok(())
}

fn is_valid_even_hex(s: &str) -> bool {
    !s.is_empty() && s.len() % 2 == 0 && s.bytes().all(|b| b.is_ascii_hexdigit())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use tempfile::NamedTempFile;

    fn write_cfg(contents: &str) -> NamedTempFile {
        let mut f = NamedTempFile::new().unwrap();
        f.write_all(contents.as_bytes()).unwrap();
        f.flush().unwrap();
        f
    }

    #[test]
    fn loads_minimal_config() {
        let f = write_cfg("");
        let cfg = load(f.path()).unwrap();
        assert!(cfg.peers.is_empty());
    }

    #[test]
    fn loads_full_happy_path() {
        let s = r#"
[fabric]
listen_addr = "0.0.0.0:1234"

[storage]
bytes_per_shard = "64M"
backing_kind = "heap"

[p2p]
local_node_id = 99

[[peers]]
id = 1
transport = "tcp"
address = "10.0.0.1:9000"

[[peers]]
id = 2
transport = "rdma"
address = "deadbeef"
hca_numa = 0

[[disks]]
path = "/dev/nvme0n1"
kind = "nvme"
numa = 0

[[disks]]
path = "/dev/nvme1n1"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).unwrap();
        assert_eq!(cfg.peers.len(), 2);
        assert_eq!(cfg.disks.len(), 2);
        assert_eq!(cfg.storage.bytes_per_shard.bytes(), 64 * 1024 * 1024);
    }

    #[test]
    fn rejects_duplicate_peer_ids() {
        let s = r#"
[p2p]
local_node_id = 99

[[peers]]
id = 1
transport = "tcp"
address = "10.0.0.1:9000"

[[peers]]
id = 1
transport = "tcp"
address = "10.0.0.2:9000"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::DuplicatePeer(1)) => {}
            other => panic!("expected DuplicatePeer(1), got {other:?}"),
        }
    }

    #[test]
    fn rejects_duplicate_disk_paths() {
        let s = r#"
[[disks]]
path = "/dev/nvme0n1"

[[disks]]
path = "/dev/nvme0n1"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::DuplicateDiskPath(_)) => {}
            other => panic!("expected DuplicateDiskPath, got {other:?}"),
        }
    }

    #[test]
    fn rejects_invalid_tcp_addr() {
        let s = r#"
[p2p]
local_node_id = 1

[[peers]]
id = 7
transport = "tcp"
address = "not-an-addr"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::InvalidTcpAddr { peer_id: 7, .. }) => {}
            other => panic!("expected InvalidTcpAddr, got {other:?}"),
        }
    }

    #[test]
    fn rejects_hostname_for_tcp() {
        let s = r#"
[p2p]
local_node_id = 1

[[peers]]
id = 8
transport = "tcp"
address = "example.com:9000"
"#;
        let f = write_cfg(s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::InvalidTcpAddr { peer_id: 8, .. })
        ));
    }

    #[test]
    fn rejects_invalid_rdma_hex() {
        for bad in ["xyzz", "abc", "", "deadbeefg0"] {
            let s = format!(
                r#"
[p2p]
local_node_id = 1

[[peers]]
id = 3
transport = "rdma"
address = "{bad}"
"#
            );
            let f = write_cfg(&s);
            assert!(
                matches!(load(f.path()), Err(ConfigError::InvalidRdmaHex { peer_id: 3 })),
                "expected InvalidRdmaHex for {bad:?}"
            );
        }
    }

    #[test]
    fn rejects_empty_disk_path() {
        let s = r#"
[[disks]]
path = ""
"#;
        let f = write_cfg(s);
        assert!(matches!(load(f.path()), Err(ConfigError::EmptyDiskPath)));
    }

    #[test]
    fn io_error_when_missing() {
        let path = Path::new("/definitely/not/a/real/path/for/unbounded-storage.toml");
        match Config::load(path) {
            Err(ConfigError::Io(_)) => {}
            other => panic!("expected Io error, got {other:?}"),
        }
    }

    #[test]
    fn toml_error_on_bad_syntax() {
        let f = write_cfg("this is = not = valid = toml");
        assert!(matches!(load(f.path()), Err(ConfigError::Toml(_))));
    }

    #[test]
    fn rejects_peers_without_local_node_id() {
        let s = r#"
[[peers]]
id = 1
transport = "tcp"
address = "10.0.0.1:9000"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::MissingLocalNodeId) => {}
            other => panic!("expected MissingLocalNodeId, got {other:?}"),
        }
    }

    #[test]
    fn accepts_peers_with_local_node_id() {
        let s = r#"
[p2p]
local_node_id = 7

[[peers]]
id = 1
transport = "tcp"
address = "10.0.0.1:9000"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.p2p.local_node_id, Some(7));
        assert_eq!(cfg.peers.len(), 1);
    }

    #[test]
    fn rejects_local_node_id_colliding_with_peer() {
        let s = r#"
[p2p]
local_node_id = 1

[[peers]]
id = 1
transport = "tcp"
address = "10.0.0.1:9000"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::LocalNodeIdCollidesWithPeer(1)) => {}
            other => panic!("expected LocalNodeIdCollidesWithPeer(1), got {other:?}"),
        }
    }

    #[test]
    fn accepts_local_node_id_distinct_from_peers() {
        let s = r#"
[p2p]
local_node_id = 99

[[peers]]
id = 1
transport = "tcp"
address = "10.0.0.1:9000"

[[peers]]
id = 2
transport = "tcp"
address = "10.0.0.2:9000"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.p2p.local_node_id, Some(99));
        assert_eq!(cfg.peers.len(), 2);
    }

    #[test]
    fn rejects_zero_fingers_per_node() {
        let s = r#"
[p2p]
fingers_per_node = 0
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::ZeroFingersPerNode) => {}
            other => panic!("expected ZeroFingersPerNode, got {other:?}"),
        }
    }
}
