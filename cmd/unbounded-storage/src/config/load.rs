// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Read and validate the daemon's configuration.

use std::collections::HashSet;
use std::fmt;
use std::fs;
use std::io;
use std::net::SocketAddr;
use std::path::Path;

use super::schema::{BackendKind, Config, DiskKind, FrontendKind};

#[derive(Debug)]
pub enum ConfigError {
    Io(io::Error),
    Toml(toml::de::Error),
    DuplicatePeer(u64),
    DuplicateDiskPath(String),
    MissingFileDiskSize(String),
    ZeroFileDiskSize(String),
    FileDiskSizeNotPageMultiple {
        path: String,
        size: u64,
        page_size: u64,
    },
    SizeOnlyForFileDisk(String),
    InvalidPeerAddr {
        peer_id: u64,
        addr: String,
    },
    EmptyDiskPath,
    MissingLocalNodeId,
    LocalNodeIdCollidesWithPeer(u64),
    RoutingPlanUnknownPeer {
        id: u64,
        role: &'static str,
    },
    RoutingPlanSelfReference {
        id: u64,
        role: &'static str,
    },
    RoutingPlanDuplicateFinger(u64),
    DuplicateBackendId(String),
    DuplicateFrontendId(String),
    EmptyBackendId,
    EmptyFrontendId,
    EmptyBackendEndpoint(String),
    EmptyFrontendBind(String),
    StripeSizeNotPowerOfTwo {
        backend_id: String,
        stripe_size_bytes: u64,
    },
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
    InvalidDiskKind {
        path: String,
        value: i32,
    },
    InvalidBackendKind {
        backend_id: String,
        value: i32,
    },
    InvalidFrontendKind {
        frontend_id: String,
        value: i32,
    },
    InvalidMetricsBind {
        bind: String,
    },
}

impl fmt::Display for ConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ConfigError::Io(e) => write!(f, "io error reading config: {e}"),
            ConfigError::Toml(e) => write!(f, "toml parse error: {e}"),
            ConfigError::DuplicatePeer(id) => write!(f, "duplicate peer id: {id}"),
            ConfigError::DuplicateDiskPath(p) => {
                write!(f, "duplicate disk path: {p}")
            }
            ConfigError::MissingFileDiskSize(p) => {
                write!(f, "disk {p}: kind = \"file\" requires a `size`")
            }
            ConfigError::ZeroFileDiskSize(p) => {
                write!(f, "disk {p}: file size must be greater than zero")
            }
            ConfigError::FileDiskSizeNotPageMultiple {
                path,
                size,
                page_size,
            } => write!(
                f,
                "disk {path}: file size {size} must be a positive multiple of the page size {page_size}"
            ),
            ConfigError::SizeOnlyForFileDisk(p) => {
                write!(f, "disk {p}: `size` is only valid for kind = \"file\"")
            }
            ConfigError::InvalidPeerAddr { peer_id, addr } => {
                write!(f, "peer {peer_id}: invalid socket address {addr:?}")
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
            ConfigError::RoutingPlanUnknownPeer { id, role } => write!(
                f,
                "p2p.routing_plan {role} {id} does not reference any [[peers]] id: every \
                 routing-plan neighbor must have a matching peer so a fabric connection exists"
            ),
            ConfigError::RoutingPlanSelfReference { id, role } => write!(
                f,
                "p2p.routing_plan {role} {id} equals p2p.local_node_id: a node cannot list \
                 itself as a routing neighbor"
            ),
            ConfigError::RoutingPlanDuplicateFinger(id) => {
                write!(f, "p2p.routing_plan.fingers contains duplicate id {id}")
            }
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
            ConfigError::StripeSizeNotPowerOfTwo {
                backend_id,
                stripe_size_bytes,
            } => write!(
                f,
                "backend {backend_id:?}: stripe_size_bytes {stripe_size_bytes} must be a power of \
                 two for deterministic StripeKey derivation"
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
            ConfigError::InvalidDiskKind { path, value } => write!(
                f,
                "disk {path}: kind {value} is not a valid value (0 = nvme, 1 = block, 2 = file)"
            ),
            ConfigError::InvalidBackendKind { backend_id, value } => write!(
                f,
                "backend {backend_id:?}: kind {value} is not a valid value (0 = http, 1 = s3, 2 = azure)"
            ),
            ConfigError::InvalidFrontendKind { frontend_id, value } => write!(
                f,
                "frontend {frontend_id:?}: kind {value} is not a valid value (0 = http, 1 = s3)"
            ),
            ConfigError::InvalidMetricsBind { bind } => {
                write!(f, "metrics bind {bind:?} is not a valid socket address")
            }
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

/// Loads a TOML config from `path` and applies defaulting and validation.
pub fn load(path: &Path) -> Result<Config, ConfigError> {
    let mut cfg: Config = toml::from_str(&fs::read_to_string(path)?)?;
    cfg.apply_defaults();
    validate(&cfg)?;
    Ok(cfg)
}

fn validate(cfg: &Config) -> Result<(), ConfigError> {
    let p2p = cfg.p2p();
    if !cfg.peers.is_empty() && p2p.local_node_id.is_none() {
        return Err(ConfigError::MissingLocalNodeId);
    }

    let mut seen_peers: HashSet<u64> = HashSet::new();
    for p in &cfg.peers {
        if !seen_peers.insert(p.id) {
            return Err(ConfigError::DuplicatePeer(p.id));
        }
        if p2p.local_node_id == Some(p.id) {
            return Err(ConfigError::LocalNodeIdCollidesWithPeer(p.id));
        }
        if p.address.parse::<SocketAddr>().is_err() {
            return Err(ConfigError::InvalidPeerAddr {
                peer_id: p.id,
                addr: p.address.clone(),
            });
        }
    }

    // Disjoint-discovery routing plan: every referenced id must name a
    // configured peer (so a fabric connection exists), must not be the
    // local node, and fingers must be free of duplicates. `seen_peers`
    // now holds every peer id.
    if let Some(plan) = &p2p.routing_plan {
        let mut seen_fingers: HashSet<u64> = HashSet::new();
        for &id in &plan.fingers {
            if !seen_fingers.insert(id) {
                return Err(ConfigError::RoutingPlanDuplicateFinger(id));
            }
        }
        for (id, role) in plan
            .fingers
            .iter()
            .map(|id| (*id, "finger"))
            .chain(plan.successor.map(|id| (id, "successor")))
            .chain(plan.predecessor.map(|id| (id, "predecessor")))
        {
            if p2p.local_node_id == Some(id) {
                return Err(ConfigError::RoutingPlanSelfReference { id, role });
            }
            if !seen_peers.contains(&id) {
                return Err(ConfigError::RoutingPlanUnknownPeer { id, role });
            }
        }
    }

    let mut seen_paths: HashSet<&str> = HashSet::new();
    for d in &cfg.disks {
        if d.path.is_empty() {
            return Err(ConfigError::EmptyDiskPath);
        }
        if !seen_paths.insert(d.path.as_str()) {
            return Err(ConfigError::DuplicateDiskPath(d.path.clone()));
        }
        if DiskKind::try_from(d.kind).is_err() {
            return Err(ConfigError::InvalidDiskKind {
                path: d.path.clone(),
                value: d.kind,
            });
        }
        match d.kind() {
            DiskKind::File => {
                let page_size = d.page_size_bytes.unwrap_or(4096);
                let size = match d.size {
                    Some(s) => s,
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
        if BackendKind::try_from(b.kind).is_err() {
            return Err(ConfigError::InvalidBackendKind {
                backend_id: b.id.clone(),
                value: b.kind,
            });
        }
        if b.endpoint.is_empty() {
            return Err(ConfigError::EmptyBackendEndpoint(b.id.clone()));
        }
        if !b.stripe_size_bytes.is_power_of_two() {
            return Err(ConfigError::StripeSizeNotPowerOfTwo {
                backend_id: b.id.clone(),
                stripe_size_bytes: b.stripe_size_bytes,
            });
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
        if FrontendKind::try_from(fr.kind).is_err() {
            return Err(ConfigError::InvalidFrontendKind {
                frontend_id: fr.id.clone(),
                value: fr.kind,
            });
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

    // The metrics exporter bind is optional; when set it must parse as a
    // socket address (an empty value disables the exporter).
    let metrics_bind = &cfg.startup().metrics().bind;
    if !metrics_bind.is_empty() && metrics_bind.parse::<SocketAddr>().is_err() {
        return Err(ConfigError::InvalidMetricsBind {
            bind: metrics_bind.clone(),
        });
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::super::schema::BackendKind;
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
[p2p]
local_node_id = 99

[[peers]]
id = 1
address = "10.0.0.1:9000"

[[peers]]
id = 2
address = "10.0.0.2:9000"
hca_numa = 0

[[disks]]
path = "/dev/nvme0n1"
kind = 0
numa = 0

[[disks]]
path = "/dev/nvme1n1"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).unwrap();
        assert_eq!(cfg.peers.len(), 2);
        assert_eq!(cfg.disks.len(), 2);
        assert_eq!(cfg.p2p().local_node_id, Some(99));
    }

    #[test]
    fn rejects_duplicate_peer_ids() {
        let s = r#"
[p2p]
local_node_id = 99

[[peers]]
id = 1
address = "10.0.0.1:9000"

[[peers]]
id = 1
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
    fn rejects_invalid_peer_addr() {
        let s = r#"
[p2p]
local_node_id = 1

[[peers]]
id = 7
address = "not-an-addr"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::InvalidPeerAddr { peer_id: 7, .. }) => {}
            other => panic!("expected InvalidPeerAddr, got {other:?}"),
        }
    }

    #[test]
    fn rejects_hostname_for_peer_addr() {
        let s = r#"
[p2p]
local_node_id = 1

[[peers]]
id = 8
address = "example.com:9000"
"#;
        let f = write_cfg(s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::InvalidPeerAddr { peer_id: 8, .. })
        ));
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
kind = 2
size = 16777216
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.disks[0].kind(), DiskKind::File);
        assert!(cfg.disks[0].size.is_some());
        assert_eq!(cfg.disks[0].size.unwrap(), 16 * 1024 * 1024);
    }

    #[test]
    fn rejects_file_disk_without_size() {
        let s = r#"
[[disks]]
path = "/tmp/unbounded-file-disk"
kind = 2
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
kind = 2
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
kind = 2
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
kind = 0
size = 16777216
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
address = "10.0.0.1:9000"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.p2p().local_node_id, Some(7));
        assert_eq!(cfg.peers.len(), 1);
    }

    #[test]
    fn rejects_local_node_id_colliding_with_peer() {
        let s = r#"
[p2p]
local_node_id = 1

[[peers]]
id = 1
address = "10.0.0.1:9000"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::LocalNodeIdCollidesWithPeer(1)) => {}
            other => panic!("expected LocalNodeIdCollidesWithPeer(1), got {other:?}"),
        }
    }

    #[test]
    fn loads_backends_and_frontends_happy_path() {
        let s = r#"
[[backends]]
id = "primary-http"
kind = 0
endpoint = "https://origin.example.com"

[[frontends]]
id = "workload-http"
kind = 0
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
kind = 0
endpoint = "https://e"

[[backends]]
id = "dup"
kind = 0
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
kind = 0
endpoint = "https://e"

[[frontends]]
id = "dup"
kind = 0
bind = "0.0.0.0:9000"
backend = "b"

[[frontends]]
id = "dup"
kind = 0
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
kind = 0
endpoint = "https://e"

[[frontends]]
id = "f"
kind = 0
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
kind = 0
endpoint = ""
"#;
        let f = write_cfg(no_endpoint);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::EmptyBackendEndpoint(_))
        ));
    }

    #[test]
    fn rejects_non_power_of_two_stripe_size() {
        let s = r#"
[[backends]]
id = "b"
kind = 0
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
kind = 0
endpoint = "https://e"
stripe_size_bytes = 8388608
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.backends[0].stripe_size_bytes, 8 * 1024 * 1024);
    }

    #[test]
    fn rejects_invalid_frontend_bind() {
        let s = r#"
[[backends]]
id = "b"
kind = 0
endpoint = "https://e"

[[frontends]]
id = "f"
kind = 0
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
kind = 0
endpoint = "https://e"

[[frontends]]
id = "f"
kind = 0
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
    fn accepts_valid_metrics_bind() {
        let f = write_cfg("[startup.metrics]\nbind = \"0.0.0.0:9100\"\n");
        let cfg = load(f.path()).expect("valid metrics bind loads");
        assert_eq!(cfg.startup().metrics().bind, "0.0.0.0:9100");
    }

    #[test]
    fn empty_metrics_bind_is_allowed() {
        let f = write_cfg("");
        let cfg = load(f.path()).expect("absent metrics section loads");
        assert_eq!(cfg.startup().metrics().bind, "");
    }

    #[test]
    fn rejects_invalid_metrics_bind() {
        let f = write_cfg("[startup.metrics]\nbind = \"not-an-addr\"\n");
        match load(f.path()) {
            Err(ConfigError::InvalidMetricsBind { bind }) if bind == "not-an-addr" => {}
            other => panic!("expected InvalidMetricsBind, got {other:?}"),
        }
    }

    #[test]
    fn rejects_duplicate_frontend_bind() {
        let s = r#"
[[backends]]
id = "b"
kind = 0
endpoint = "https://e"

[[frontends]]
id = "f1"
kind = 0
bind = "0.0.0.0:9000"
backend = "b"

[[frontends]]
id = "f2"
kind = 0
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

    #[test]
    fn accepts_s3_backend() {
        let s = r#"
[[backends]]
id = "s3"
kind = 1
endpoint = "s3.example.com:443"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.backends[0].kind(), BackendKind::S3);
        assert!(cfg.backends[0].bucket.is_none());
    }

    #[test]
    fn accepts_azure_backend() {
        let s = r#"
[[backends]]
id = "azure"
kind = 2
endpoint = "acct.blob.core.windows.net:443"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.backends[0].kind(), BackendKind::Azure);
    }

    #[test]
    fn rejects_unknown_key() {
        // A typo in a key now fails loudly at parse time instead of being
        // silently dropped (deny_unknown_fields on the TOML path).
        let s = r#"
[p2p]
fingers_per_nod = 128
"#;
        let f = write_cfg(s);
        assert!(matches!(load(f.path()), Err(ConfigError::Toml(_))));
    }

    fn rejects_out_of_range_disk_kind() {
        let s = r#"
[[disks]]
path = "/dev/nvme0n1"
kind = 3
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::InvalidDiskKind { path, value: 3 }) if path == "/dev/nvme0n1" => {}
            other => panic!("expected InvalidDiskKind, got {other:?}"),
        }
    }

    #[test]
    fn rejects_out_of_range_backend_kind() {
        let s = r#"
[[backends]]
id = "b"
kind = 3
endpoint = "https://e"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::InvalidBackendKind {
                backend_id,
                value: 3,
            }) if backend_id == "b" => {}
            other => panic!("expected InvalidBackendKind, got {other:?}"),
        }
    }

    #[test]
    fn rejects_out_of_range_frontend_kind() {
        let s = r#"
[[backends]]
id = "b"
kind = 0
endpoint = "https://e"

[[frontends]]
id = "f"
kind = 2
bind = "0.0.0.0:9000"
backend = "b"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::InvalidFrontendKind {
                frontend_id,
                value: 2,
            }) if frontend_id == "f" => {}
            other => panic!("expected InvalidFrontendKind, got {other:?}"),
        }
    }

    fn loads_startup_defaults_via_toml() {
        // An omitted [startup] section is populated entirely from the
        // documented defaults during load.
        let f = write_cfg("");
        let cfg = load(f.path()).unwrap();
        let s = cfg.startup();
        assert_eq!(s.memory().memory_total_bytes, 128 * 1024 * 1024);
        assert_eq!(s.fabric().listen_addr, "0.0.0.0:0");
        assert_eq!(s.fabric().max_inflight, 1024);
        assert_eq!(s.topology().nic_workers, 4);
    }

    #[test]
    fn loads_startup_section_via_toml() {
        let s = r#"
[startup.memory]
no_hugepages = true
memory_total_bytes = 67108864

[startup.fabric]
listen_addr = "10.0.0.1:7000"

[startup.topology]
disable_rdma = true
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).unwrap();
        assert!(cfg.startup().memory().no_hugepages);
        assert_eq!(cfg.startup().memory().memory_total_bytes, 64 * 1024 * 1024);
        assert_eq!(cfg.startup().fabric().listen_addr, "10.0.0.1:7000");
        assert!(cfg.startup().topology().disable_rdma);
        // Unset siblings still default.
        assert_eq!(cfg.startup().fabric().progress_threads, 2);
    }
}
