// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Read and validate the daemon's TOML configuration.

use std::collections::HashSet;
use std::fmt;
use std::fs;
use std::io;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};

use super::schema::{Config, DiskKind, PeerTransport};

#[derive(Debug)]
pub enum ConfigError {
    Io(io::Error),
    Toml(toml::de::Error),
    DuplicatePeer(u64),
    DuplicateDiskPath(PathBuf),
    MissingFileDiskSize(PathBuf),
    ZeroFileDiskSize(PathBuf),
    FileDiskSizeNotPageMultiple {
        path: PathBuf,
        size: usize,
        page_size: usize,
    },
    SizeOnlyForFileDisk(PathBuf),
    InvalidTcpAddr {
        peer_id: u64,
        addr: String,
    },
    InvalidRdmaHex {
        peer_id: u64,
    },
    EmptyDiskPath,
    MissingLocalNodeId,
    LocalNodeIdCollidesWithPeer(u64),
    ZeroFingersPerNode,
    DuplicateBackendId(String),
    DuplicateFrontendId(String),
    EmptyBackendId,
    EmptyFrontendId,
    EmptyBackendEndpoint(String),
    EmptyFrontendBind(String),
    ZeroStripeSize(String),
    StripeSizeNotPowerOfTwo {
        backend_id: String,
        stripe_size_bytes: u64,
    },
    ZeroHttpConcurrency(String),
    InvalidFrontendBind {
        frontend_id: String,
        bind: String,
    },
    DuplicateFrontendBind {
        frontend_id: String,
        bind: String,
    },
    DanglingFrontendBackend {
        frontend_id: String,
        backend_id: String,
    },
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
            ConfigError::MissingFileDiskSize(p) => {
                write!(f, "disk {}: kind = \"file\" requires a `size`", p.display())
            }
            ConfigError::ZeroFileDiskSize(p) => {
                write!(
                    f,
                    "disk {}: file size must be greater than zero",
                    p.display()
                )
            }
            ConfigError::FileDiskSizeNotPageMultiple {
                path,
                size,
                page_size,
            } => write!(
                f,
                "disk {}: file size {size} must be a positive multiple of the page size {page_size}",
                path.display()
            ),
            ConfigError::SizeOnlyForFileDisk(p) => write!(
                f,
                "disk {}: `size` is only valid for kind = \"file\"",
                p.display()
            ),
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
            ConfigError::DuplicateBackendId(id) => write!(f, "duplicate backend id: {id:?}"),
            ConfigError::DuplicateFrontendId(id) => write!(f, "duplicate frontend id: {id:?}"),
            ConfigError::EmptyBackendId => write!(f, "backend id must not be empty"),
            ConfigError::EmptyFrontendId => write!(f, "frontend id must not be empty"),
            ConfigError::EmptyBackendEndpoint(id) => {
                write!(f, "backend {id:?}: endpoint must not be empty")
            }
            ConfigError::EmptyFrontendBind(id) => {
                write!(f, "frontend {id:?}: bind must not be empty")
            }
            ConfigError::ZeroStripeSize(id) => write!(
                f,
                "backend {id:?}: stripe_size_bytes must be greater than zero: a zero stripe size \
                 is a divide-by-zero in StripeKey derivation"
            ),
            ConfigError::StripeSizeNotPowerOfTwo {
                backend_id,
                stripe_size_bytes,
            } => write!(
                f,
                "backend {backend_id:?}: stripe_size_bytes {stripe_size_bytes} must be a power of \
                 two for deterministic StripeKey derivation"
            ),
            ConfigError::ZeroHttpConcurrency(id) => write!(
                f,
                "backend {id:?}: http_concurrency must be greater than zero: a zero limit would \
                 stall all origin fetches"
            ),
            ConfigError::InvalidFrontendBind { frontend_id, bind } => {
                write!(
                    f,
                    "frontend {frontend_id:?}: invalid bind socket address {bind:?}"
                )
            }
            ConfigError::DuplicateFrontendBind { frontend_id, bind } => {
                write!(
                    f,
                    "frontend {frontend_id:?}: duplicate bind address {bind:?}"
                )
            }
            ConfigError::DanglingFrontendBackend {
                frontend_id,
                backend_id,
            } => write!(
                f,
                "frontend {frontend_id:?} references backend {backend_id:?} which is not defined \
                 in any [[backends]] entry"
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
        match d.kind {
            DiskKind::File => {
                let page_size = d.page_size_bytes.unwrap_or(4096);
                let size = match d.size {
                    Some(s) => s.bytes(),
                    None => return Err(ConfigError::MissingFileDiskSize(d.path.clone())),
                };
                if size == 0 {
                    return Err(ConfigError::ZeroFileDiskSize(d.path.clone()));
                }
                if page_size == 0 || size % page_size != 0 {
                    return Err(ConfigError::FileDiskSizeNotPageMultiple {
                        path: d.path.clone(),
                        size,
                        page_size,
                    });
                }
            }
            _ => {
                if d.size.is_some() {
                    return Err(ConfigError::SizeOnlyForFileDisk(d.path.clone()));
                }
            }
        }
    }

    let mut seen_backends: HashSet<&str> = HashSet::new();
    for b in &cfg.backends {
        if b.id.is_empty() {
            return Err(ConfigError::EmptyBackendId);
        }
        if !seen_backends.insert(b.id.as_str()) {
            return Err(ConfigError::DuplicateBackendId(b.id.clone()));
        }
        if b.endpoint.is_empty() {
            return Err(ConfigError::EmptyBackendEndpoint(b.id.clone()));
        }
        if b.stripe_size_bytes == 0 {
            return Err(ConfigError::ZeroStripeSize(b.id.clone()));
        }
        if !b.stripe_size_bytes.is_power_of_two() {
            return Err(ConfigError::StripeSizeNotPowerOfTwo {
                backend_id: b.id.clone(),
                stripe_size_bytes: b.stripe_size_bytes,
            });
        }
        if b.http_concurrency == 0 {
            return Err(ConfigError::ZeroHttpConcurrency(b.id.clone()));
        }
    }

    let mut seen_frontends: HashSet<&str> = HashSet::new();
    let mut seen_binds: HashSet<&str> = HashSet::new();
    for fr in &cfg.frontends {
        if fr.id.is_empty() {
            return Err(ConfigError::EmptyFrontendId);
        }
        if !seen_frontends.insert(fr.id.as_str()) {
            return Err(ConfigError::DuplicateFrontendId(fr.id.clone()));
        }
        if fr.bind.is_empty() {
            return Err(ConfigError::EmptyFrontendBind(fr.id.clone()));
        }
        if fr.bind.parse::<SocketAddr>().is_err() {
            return Err(ConfigError::InvalidFrontendBind {
                frontend_id: fr.id.clone(),
                bind: fr.bind.clone(),
            });
        }
        if !seen_binds.insert(fr.bind.as_str()) {
            return Err(ConfigError::DuplicateFrontendBind {
                frontend_id: fr.id.clone(),
                bind: fr.bind.clone(),
            });
        }
        if !seen_backends.contains(fr.backend.as_str()) {
            return Err(ConfigError::DanglingFrontendBackend {
                frontend_id: fr.id.clone(),
                backend_id: fr.backend.clone(),
            });
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
                matches!(
                    load(f.path()),
                    Err(ConfigError::InvalidRdmaHex { peer_id: 3 })
                ),
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
    fn loads_file_disk_with_size() {
        let s = r#"
[[disks]]
path = "/tmp/unbounded-file-disk"
kind = "file"
size = "16M"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.disks[0].kind, DiskKind::File);
        assert!(cfg.disks[0].size.is_some());
        assert_eq!(cfg.disks[0].size.unwrap().bytes(), 16 * 1024 * 1024);
    }

    #[test]
    fn rejects_file_disk_without_size() {
        let s = r#"
[[disks]]
path = "/tmp/unbounded-file-disk"
kind = "file"
"#;
        let f = write_cfg(s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::MissingFileDiskSize(_))
        ));
    }

    #[test]
    fn rejects_file_disk_with_zero_size() {
        let s = r#"
[[disks]]
path = "/tmp/unbounded-file-disk"
kind = "file"
size = 0
"#;
        let f = write_cfg(s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::ZeroFileDiskSize(_))
        ));
    }

    #[test]
    fn rejects_file_disk_size_not_page_multiple() {
        let s = r#"
[[disks]]
path = "/tmp/unbounded-file-disk"
kind = "file"
size = 5000
"#;
        let f = write_cfg(s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::FileDiskSizeNotPageMultiple { .. })
        ));
    }

    #[test]
    fn rejects_size_on_non_file_disk() {
        let s = r#"
[[disks]]
path = "/dev/nvme0n1"
kind = "nvme"
size = "16M"
"#;
        let f = write_cfg(s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::SizeOnlyForFileDisk(_))
        ));
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

    #[test]
    fn loads_backends_and_frontends_happy_path() {
        let s = r#"
[[backends]]
id = "primary-http"
kind = "http"
endpoint = "https://origin.example.com"

[[frontends]]
id = "workload-http"
kind = "http"
bind = "0.0.0.0:9000"
backend = "primary-http"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.backends.len(), 1);
        assert_eq!(cfg.frontends.len(), 1);
    }

    #[test]
    fn rejects_duplicate_backend_ids() {
        let s = r#"
[[backends]]
id = "dup"
kind = "http"
endpoint = "https://e"

[[backends]]
id = "dup"
kind = "http"
endpoint = "https://e2"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::DuplicateBackendId(id)) if id == "dup" => {}
            other => panic!("expected DuplicateBackendId(dup), got {other:?}"),
        }
    }

    #[test]
    fn rejects_duplicate_frontend_ids() {
        let s = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://e"

[[frontends]]
id = "dup"
kind = "http"
bind = "0.0.0.0:9000"
backend = "b"

[[frontends]]
id = "dup"
kind = "http"
bind = "0.0.0.0:9001"
backend = "b"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::DuplicateFrontendId(id)) if id == "dup" => {}
            other => panic!("expected DuplicateFrontendId(dup), got {other:?}"),
        }
    }

    #[test]
    fn rejects_dangling_frontend_backend_reference() {
        let s = r#"
[[backends]]
id = "real"
kind = "http"
endpoint = "https://e"

[[frontends]]
id = "f"
kind = "http"
bind = "0.0.0.0:9000"
backend = "ghost"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::DanglingFrontendBackend {
                frontend_id,
                backend_id,
            }) if frontend_id == "f" && backend_id == "ghost" => {}
            other => panic!("expected DanglingFrontendBackend, got {other:?}"),
        }
    }

    #[test]
    fn rejects_empty_backend_endpoint() {
        let no_endpoint = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = ""
"#;
        let f = write_cfg(no_endpoint);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::EmptyBackendEndpoint(_))
        ));
    }

    #[test]
    fn rejects_zero_stripe_size() {
        let s = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://e"
stripe_size_bytes = 0
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::ZeroStripeSize(id)) if id == "b" => {}
            other => panic!("expected ZeroStripeSize(b), got {other:?}"),
        }
    }

    #[test]
    fn rejects_non_power_of_two_stripe_size() {
        let s = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://e"
stripe_size_bytes = 3000000
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::StripeSizeNotPowerOfTwo { backend_id, .. }) if backend_id == "b" => {}
            other => panic!("expected StripeSizeNotPowerOfTwo, got {other:?}"),
        }
    }

    #[test]
    fn accepts_power_of_two_stripe_size() {
        let s = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://e"
stripe_size_bytes = 8388608
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.backends[0].stripe_size_bytes, 8 * 1024 * 1024);
    }

    #[test]
    fn rejects_zero_http_concurrency() {
        let s = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://e"
http_concurrency = 0
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::ZeroHttpConcurrency(id)) if id == "b" => {}
            other => panic!("expected ZeroHttpConcurrency(b), got {other:?}"),
        }
    }

    #[test]
    fn rejects_invalid_frontend_bind() {
        let s = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://e"

[[frontends]]
id = "f"
kind = "http"
bind = "not-an-addr"
backend = "b"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::InvalidFrontendBind { frontend_id, .. }) if frontend_id == "f" => {}
            other => panic!("expected InvalidFrontendBind, got {other:?}"),
        }
    }

    #[test]
    fn rejects_hostname_for_frontend_bind() {
        let s = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://e"

[[frontends]]
id = "f"
kind = "http"
bind = "example.com:9000"
backend = "b"
"#;
        let f = write_cfg(s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::InvalidFrontendBind { .. })
        ));
    }

    #[test]
    fn rejects_duplicate_frontend_bind() {
        let s = r#"
[[backends]]
id = "b"
kind = "http"
endpoint = "https://e"

[[frontends]]
id = "f1"
kind = "http"
bind = "0.0.0.0:9000"
backend = "b"

[[frontends]]
id = "f2"
kind = "http"
bind = "0.0.0.0:9000"
backend = "b"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::DuplicateFrontendBind { frontend_id, bind })
                if frontend_id == "f2" && bind == "0.0.0.0:9000" => {}
            other => panic!("expected DuplicateFrontendBind, got {other:?}"),
        }
    }
}
