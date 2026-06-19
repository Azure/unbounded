// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Read and validate the daemon's configuration.
//!
//! Two on-disk encodings are supported, selected by the file
//! extension:
//!
//! - `.binpb` is decoded as a raw binary protobuf wire message
//!   (`prost::Message::decode`), keeping protobuf's forward-compatible
//!   unknown-field semantics.
//! - any other extension (notably `.toml`) is parsed as strict TOML,
//!   where unknown keys are rejected.
//!
//! Both paths feed the same [`Config::apply_defaults`] and
//! [`validate`] finalization, so the encoding only affects how bytes
//! become a `Config`, not what counts as a valid one.

use std::collections::HashSet;
use std::fmt;
use std::fs;
use std::io;
use std::net::SocketAddr;
use std::path::Path;

use prost::Message;

use super::graph::{runtime_projection, validate_binding_graph};
use super::schema::{Config, backend_spec, disk_spec, frontend_spec, peer_spec};

#[derive(Debug)]
pub enum ConfigError {
    Io(io::Error),
    Toml(toml::de::Error),
    Protobuf(prost::DecodeError),
    DuplicatePeer(u64),
    DuplicateDiskPath(String),
    MissingFileDiskSize(String),
    ZeroFileDiskSize(String),
    FileDiskSizeNotPageMultiple {
        path: String,
        size: u64,
        page_size: u64,
    },
    InvalidTcpAddr {
        peer_id: u64,
        addr: String,
    },
    InvalidNativePeerAddr {
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
    DuplicateBackendName(String),
    DuplicateFrontendName(String),
    DuplicateCacheName(String),
    DuplicateNeighborhoodName(String),
    EmptyBackendName,
    EmptyFrontendName,
    EmptyCacheName,
    EmptyNeighborhoodName,
    MissingBackendConfig(String),
    MissingFrontendConfig(String),
    EmptyBackendUrl(String),
    EmptyFrontendAddr(String),
    StripeSizeNotPowerOfTwo {
        backend_name: String,
        stripe_size_bytes: u64,
    },
    InvalidFrontendAddr {
        frontend_name: String,
        addr: String,
    },
    DuplicateFrontendAddr {
        frontend_name: String,
        addr: String,
    },
    MissingPeerConfig(u64),
    MissingDiskConfig(String),
    InvalidMetricsAddr {
        addr: String,
    },
    InvalidBindingGraph(String),
    UnsupportedRuntimeProjection(String),
}

impl fmt::Display for ConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ConfigError::Io(e) => write!(f, "io error reading config: {e}"),
            ConfigError::Toml(e) => write!(f, "toml parse error: {e}"),
            ConfigError::Protobuf(e) => write!(f, "protobuf decode error: {e}"),
            ConfigError::DuplicatePeer(id) => write!(f, "duplicate peer id: {id}"),
            ConfigError::DuplicateDiskPath(p) => {
                write!(f, "duplicate disk path: {p}")
            }
            ConfigError::MissingFileDiskSize(p) => {
                write!(f, "disk {p}: file config requires a `size`")
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
            ConfigError::InvalidTcpAddr { peer_id, addr } => {
                write!(f, "peer {peer_id}: invalid tcp socket address {addr:?}")
            }
            ConfigError::InvalidNativePeerAddr { peer_id, addr } => {
                write!(f, "peer {peer_id}: invalid native fabric address {addr:?}")
            }
            ConfigError::EmptyDiskPath => write!(f, "disk path must not be empty"),
            ConfigError::MissingLocalNodeId => write!(
                f,
                "neighborhood local_node_id must be set when peers are configured: a multi-node \
                 deployment requires a stable local node id to avoid silent NodeId(0) collisions"
            ),
            ConfigError::LocalNodeIdCollidesWithPeer(id) => write!(
                f,
                "neighborhood local_node_id {id} collides with a peer id: the local node and a peer \
                 cannot share a node id, or the p2p finger table will silently drop that peer"
            ),
            ConfigError::RoutingPlanUnknownPeer { id, role } => write!(
                f,
                "neighborhood routing_plan {role} {id} does not reference any peer id: every \
                 routing-plan neighbor must have a matching peer so a fabric connection exists"
            ),
            ConfigError::RoutingPlanSelfReference { id, role } => write!(
                f,
                "neighborhood routing_plan {role} {id} equals local_node_id: a node cannot list \
                 itself as a routing neighbor"
            ),
            ConfigError::RoutingPlanDuplicateFinger(id) => {
                write!(
                    f,
                    "neighborhood routing_plan.fingers contains duplicate id {id}"
                )
            }
            ConfigError::DuplicateBackendName(name) => {
                write!(f, "duplicate backend name: {name:?}")
            }
            ConfigError::DuplicateFrontendName(name) => {
                write!(f, "duplicate frontend name: {name:?}")
            }
            ConfigError::DuplicateCacheName(name) => write!(f, "duplicate cache name: {name:?}"),
            ConfigError::DuplicateNeighborhoodName(name) => {
                write!(f, "duplicate neighborhood name: {name:?}")
            }
            ConfigError::EmptyBackendName => write!(f, "backend name must not be empty"),
            ConfigError::EmptyFrontendName => write!(f, "frontend name must not be empty"),
            ConfigError::EmptyCacheName => write!(f, "cache name must not be empty"),
            ConfigError::EmptyNeighborhoodName => write!(f, "neighborhood name must not be empty"),
            ConfigError::MissingBackendConfig(id) => {
                write!(f, "backend {id:?}: config must set one backend type")
            }
            ConfigError::MissingFrontendConfig(id) => {
                write!(f, "frontend {id:?}: config must set one frontend type")
            }
            ConfigError::EmptyBackendUrl(name) => {
                write!(f, "backend {name:?}: url must not be empty")
            }
            ConfigError::EmptyFrontendAddr(id) => {
                write!(f, "frontend {id:?}: addr must not be empty")
            }
            ConfigError::StripeSizeNotPowerOfTwo {
                backend_name,
                stripe_size_bytes,
            } => write!(
                f,
                "backend {backend_name:?}: stripe_size_bytes {stripe_size_bytes} must be a power of \
                 two for deterministic StripeKey derivation"
            ),
            ConfigError::InvalidFrontendAddr {
                frontend_name,
                addr,
            } => {
                write!(
                    f,
                    "frontend {frontend_name:?}: invalid addr socket address {addr:?}"
                )
            }
            ConfigError::DuplicateFrontendAddr {
                frontend_name,
                addr,
            } => {
                write!(
                    f,
                    "frontend {frontend_name:?}: duplicate addr address {addr:?}"
                )
            }
            ConfigError::MissingPeerConfig(peer_id) => {
                write!(f, "peer {peer_id}: config must set one peer transport")
            }
            ConfigError::MissingDiskConfig(path) => {
                write!(f, "disk {path}: config must set one disk type")
            }
            ConfigError::InvalidMetricsAddr { addr } => {
                write!(f, "metrics addr {addr:?} is not a valid socket address")
            }
            ConfigError::InvalidBindingGraph(msg) => write!(f, "invalid binding graph: {msg}"),
            ConfigError::UnsupportedRuntimeProjection(msg) => {
                write!(f, "unsupported runtime projection: {msg}")
            }
        }
    }
}

impl std::error::Error for ConfigError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            ConfigError::Io(e) => Some(e),
            ConfigError::Toml(e) => Some(e),
            ConfigError::Protobuf(e) => Some(e),
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

impl From<prost::DecodeError> for ConfigError {
    fn from(e: prost::DecodeError) -> Self {
        ConfigError::Protobuf(e)
    }
}

impl Config {
    pub fn load(path: impl AsRef<Path>) -> Result<Self, ConfigError> {
        load(path.as_ref())
    }
}

/// Loads a config from `path`, decoding raw binary protobuf for a
/// `.binpb` extension and strict TOML for anything else. Both encodings
/// share the same defaulting and validation finalization.
pub fn load(path: &Path) -> Result<Config, ConfigError> {
    let mut cfg = if has_binpb_extension(path) {
        Config::decode(fs::read(path)?.as_slice())?
    } else {
        toml::from_str(&fs::read_to_string(path)?)?
    };
    cfg.apply_defaults();
    validate(&cfg)?;
    Ok(cfg)
}

fn has_binpb_extension(path: &Path) -> bool {
    path.extension().and_then(|e| e.to_str()) == Some("binpb")
}

fn validate(cfg: &Config) -> Result<(), ConfigError> {
    let mut seen_backends: HashSet<&str> = HashSet::new();
    for b in &cfg.backends {
        if b.name.is_empty() {
            return Err(ConfigError::EmptyBackendName);
        }
        if !seen_backends.insert(b.name.as_str()) {
            return Err(ConfigError::DuplicateBackendName(b.name.clone()));
        }
        let stripe_size_bytes = match b.config.as_ref() {
            Some(backend_spec::Config::Http(cfg)) => {
                validate_backend_url(&b.name, &cfg.url)?;
                cfg.stripe_size_bytes.unwrap_or(0)
            }
            Some(backend_spec::Config::S3(cfg)) => {
                validate_backend_url(&b.name, &cfg.url)?;
                cfg.stripe_size_bytes.unwrap_or(0)
            }
            Some(backend_spec::Config::Azure(cfg)) => {
                validate_backend_url(&b.name, &cfg.url)?;
                cfg.stripe_size_bytes.unwrap_or(0)
            }
            Some(backend_spec::Config::Fake(cfg)) => cfg.stripe_size_bytes.unwrap_or(0),
            None => return Err(ConfigError::MissingBackendConfig(b.name.clone())),
        };
        if !stripe_size_bytes.is_power_of_two() {
            return Err(ConfigError::StripeSizeNotPowerOfTwo {
                backend_name: b.name.clone(),
                stripe_size_bytes,
            });
        }
    }

    let mut seen_caches: HashSet<&str> = HashSet::new();
    let mut seen_disk_paths: HashSet<&str> = HashSet::new();
    for cache in &cfg.caches {
        if cache.name.is_empty() {
            return Err(ConfigError::EmptyCacheName);
        }
        if !seen_caches.insert(cache.name.as_str()) {
            return Err(ConfigError::DuplicateCacheName(cache.name.clone()));
        }
        validate_disks(&cache.disks)?;
        for disk in &cache.disks {
            let path = validated_disk_path(disk)?;
            if !seen_disk_paths.insert(path) {
                return Err(ConfigError::DuplicateDiskPath(path.to_string()));
            }
        }
    }

    let mut seen_neighborhoods: HashSet<&str> = HashSet::new();
    for neighborhood in &cfg.neighborhoods {
        if neighborhood.name.is_empty() {
            return Err(ConfigError::EmptyNeighborhoodName);
        }
        if !seen_neighborhoods.insert(neighborhood.name.as_str()) {
            return Err(ConfigError::DuplicateNeighborhoodName(
                neighborhood.name.clone(),
            ));
        }
        validate_neighborhood(neighborhood)?;
    }

    let mut seen_frontends: HashSet<&str> = HashSet::new();
    let mut seen_addrs: HashSet<&str> = HashSet::new();
    for fr in &cfg.frontends {
        if fr.name.is_empty() {
            return Err(ConfigError::EmptyFrontendName);
        }
        if !seen_frontends.insert(fr.name.as_str()) {
            return Err(ConfigError::DuplicateFrontendName(fr.name.clone()));
        }
        match fr.config.as_ref() {
            Some(frontend_spec::Config::Http(cfg)) => {
                validate_frontend_addr(&fr.name, &cfg.addr, &mut seen_addrs)?;
            }
            Some(frontend_spec::Config::S3(cfg)) => {
                validate_frontend_addr(&fr.name, &cfg.addr, &mut seen_addrs)?;
            }
            Some(frontend_spec::Config::Loadgen(_)) => {}
            None => return Err(ConfigError::MissingFrontendConfig(fr.name.clone())),
        }
    }

    validate_binding_graph(cfg).map_err(ConfigError::InvalidBindingGraph)?;
    runtime_projection(cfg).map_err(ConfigError::UnsupportedRuntimeProjection)?;

    // The metrics exporter addr is optional; when set it must parse as a
    // socket address (an empty value disables the exporter).
    let metrics_addr = &cfg.startup().metrics().addr;
    if !metrics_addr.is_empty() && metrics_addr.parse::<SocketAddr>().is_err() {
        return Err(ConfigError::InvalidMetricsAddr {
            addr: metrics_addr.clone(),
        });
    }

    Ok(())
}

fn validate_backend_url(backend_name: &str, url: &str) -> Result<(), ConfigError> {
    if url.is_empty() {
        return Err(ConfigError::EmptyBackendUrl(backend_name.to_string()));
    }

    Ok(())
}

fn validate_frontend_addr<'a>(
    frontend_name: &str,
    addr: &'a str,
    seen_addrs: &mut HashSet<&'a str>,
) -> Result<(), ConfigError> {
    if addr.is_empty() {
        return Err(ConfigError::EmptyFrontendAddr(frontend_name.to_string()));
    }
    if addr.parse::<SocketAddr>().is_err() {
        return Err(ConfigError::InvalidFrontendAddr {
            frontend_name: frontend_name.to_string(),
            addr: addr.to_string(),
        });
    }
    if !seen_addrs.insert(addr) {
        return Err(ConfigError::DuplicateFrontendAddr {
            frontend_name: frontend_name.to_string(),
            addr: addr.to_string(),
        });
    }

    Ok(())
}

fn validate_neighborhood(
    neighborhood: &super::schema::NeighborhoodSpec,
) -> Result<(), ConfigError> {
    if !neighborhood.peers.is_empty() && neighborhood.local_node_id.is_none() {
        return Err(ConfigError::MissingLocalNodeId);
    }

    let mut peer_ids: HashSet<u64> = HashSet::new();
    for p in &neighborhood.peers {
        peer_ids.insert(p.id);
        if neighborhood.local_node_id == Some(p.id) {
            return Err(ConfigError::LocalNodeIdCollidesWithPeer(p.id));
        }
        match p.config.as_ref() {
            Some(peer_spec::Config::Tcp(cfg)) => {
                if cfg.addr.parse::<SocketAddr>().is_err() {
                    return Err(ConfigError::InvalidTcpAddr {
                        peer_id: p.id,
                        addr: cfg.addr.clone(),
                    });
                }
            }
            Some(peer_spec::Config::Rdma(cfg)) => {
                if !is_valid_native_address(&cfg.addr) {
                    return Err(ConfigError::InvalidNativePeerAddr {
                        peer_id: p.id,
                        addr: cfg.addr.clone(),
                    });
                }
            }
            None => return Err(ConfigError::MissingPeerConfig(p.id)),
        }
    }

    if let Some(plan) = &neighborhood.routing_plan {
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
            if neighborhood.local_node_id == Some(id) {
                return Err(ConfigError::RoutingPlanSelfReference { id, role });
            }
            if !peer_ids.contains(&id) {
                return Err(ConfigError::RoutingPlanUnknownPeer { id, role });
            }
        }
    }

    Ok(())
}

fn validate_disks(disks: &[super::schema::DiskSpec]) -> Result<(), ConfigError> {
    let mut seen_paths: HashSet<&str> = HashSet::new();
    for d in disks {
        match d.config.as_ref() {
            Some(disk_spec::Config::File(cfg)) => {
                let path = validated_disk_path(d)?;
                if !seen_paths.insert(path) {
                    return Err(ConfigError::DuplicateDiskPath(path.to_string()));
                }
                let page_size = d.page_size_bytes.unwrap_or(4096);
                let size = match cfg.size {
                    Some(s) => s,
                    None => return Err(ConfigError::MissingFileDiskSize(path.to_string())),
                };
                if size == 0 {
                    return Err(ConfigError::ZeroFileDiskSize(path.to_string()));
                }
                if page_size == 0 || size % page_size != 0 {
                    return Err(ConfigError::FileDiskSizeNotPageMultiple {
                        path: path.to_string(),
                        size,
                        page_size,
                    });
                }
            }
            Some(disk_spec::Config::Block(_)) => {
                let path = validated_disk_path(d)?;
                if !seen_paths.insert(path) {
                    return Err(ConfigError::DuplicateDiskPath(path.to_string()));
                }
            }
            None => return Err(ConfigError::MissingDiskConfig(String::new())),
        }
    }

    Ok(())
}

fn validated_disk_path(disk: &super::schema::DiskSpec) -> Result<&str, ConfigError> {
    let Some(path) = disk.path() else {
        return Err(ConfigError::MissingDiskConfig(String::new()));
    };
    if path.is_empty() {
        return Err(ConfigError::EmptyDiskPath);
    }
    Ok(path)
}

fn is_valid_native_address(addr: &str) -> bool {
    let Some(hex) = addr.strip_prefix("hex:") else {
        return false;
    };
    is_valid_even_hex(hex)
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

    fn write_binpb(bytes: &[u8]) -> NamedTempFile {
        let mut f = tempfile::Builder::new()
            .suffix(".binpb")
            .tempfile()
            .unwrap();
        f.write_all(bytes).unwrap();
        f.flush().unwrap();
        f
    }

    fn encode_config(cfg: &Config) -> Vec<u8> {
        cfg.encode_to_vec()
    }

    fn backend_toml() -> &'static str {
        r#"
[[backends]]
name = "b"

[backends.config.fake]
"#
    }

    fn neighborhood_toml() -> &'static str {
        r#"
[[backends]]
name = "b"

[backends.config.fake]

[[neighborhoods]]
name = "n"
source = "b"
local_node_id = 99
"#
    }

    fn cache_toml() -> &'static str {
        r#"
[[backends]]
name = "b"

[backends.config.fake]

[[caches]]
name = "c"
source = "b"
"#
    }

    #[test]
    fn loads_minimal_config() {
        let f = write_cfg("");
        let cfg = load(f.path()).unwrap();
        assert!(cfg.neighborhoods.is_empty());
        assert!(cfg.caches.is_empty());
    }

    #[test]
    fn loads_full_happy_path() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.fake]

[[neighborhoods]]
name = "n"
source = "b"
local_node_id = 99

[[neighborhoods.peers]]
id = 1

[neighborhoods.peers.config.tcp]
addr = "10.0.0.1:9000"

[[neighborhoods.peers]]
id = 2

[neighborhoods.peers.config.rdma]
addr = "hex:deadbeef"

[[caches]]
name = "c"
source = "n"

[[caches.disks]]
[caches.disks.config.block]
numa = 0
path = "/dev/nvme0n1"

[[caches.disks]]
[caches.disks.config.block]
path = "/dev/nvme1n1"

[[frontends]]
name = "f"
source = "c"

[frontends.config.http]
addr = "0.0.0.0:9000"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).unwrap();
        assert_eq!(cfg.neighborhoods[0].peers.len(), 2);
        assert_eq!(cfg.caches[0].disks.len(), 2);
        assert_eq!(cfg.neighborhoods[0].local_node_id, Some(99));
        let projection = runtime_projection(&cfg).unwrap();
        assert!(!projection.frontends["f"].bypass_cache);
        assert_eq!(projection.frontends["f"].backend_id, "b");
    }

    #[test]
    fn rejects_duplicate_peer_ids() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.fake]

[[neighborhoods]]
name = "n"
source = "b"
local_node_id = 99

[[neighborhoods.peers]]
id = 1

[neighborhoods.peers.config.tcp]
addr = "10.0.0.1:9000"

[[neighborhoods.peers]]
id = 1

[neighborhoods.peers.config.tcp]
addr = "10.0.0.2:9000"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::InvalidBindingGraph(err)) => {
                assert!(err.contains("peer 1 is duplicated"), "{err}");
            }
            other => panic!("expected duplicate peer error, got {other:?}"),
        }
    }

    #[test]
    fn rejects_duplicate_disk_paths() {
        let s = format!(
            r#"{}
[[caches.disks]]
[caches.disks.config.block]
path = "/dev/nvme0n1"

[[caches.disks]]
[caches.disks.config.block]
path = "/dev/nvme0n1"
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        match load(f.path()) {
            Err(ConfigError::DuplicateDiskPath(_)) => {}
            other => panic!("expected DuplicateDiskPath, got {other:?}"),
        }
    }

    #[test]
    fn rejects_duplicate_disk_paths_across_caches() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.fake]

[[caches]]
name = "c1"
source = "b"

[[caches.disks]]
[caches.disks.config.block]
path = "/dev/nvme0n1"

[[caches]]
name = "c2"
source = "b"

[[caches.disks]]
[caches.disks.config.block]
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
        let s = format!(
            r#"{}
[[neighborhoods.peers]]
id = 7

[neighborhoods.peers.config.tcp]
addr = "not-an-addr"
"#,
            neighborhood_toml()
        );
        let f = write_cfg(&s);
        match load(f.path()) {
            Err(ConfigError::InvalidTcpAddr { peer_id: 7, .. }) => {}
            other => panic!("expected InvalidTcpAddr, got {other:?}"),
        }
    }

    #[test]
    fn rejects_hostname_for_tcp() {
        let s = format!(
            r#"{}
[[neighborhoods.peers]]
id = 8

[neighborhoods.peers.config.tcp]
addr = "example.com:9000"
"#,
            neighborhood_toml()
        );
        let f = write_cfg(&s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::InvalidTcpAddr { peer_id: 8, .. })
        ));
    }

    #[test]
    fn accepts_native_peer_addr() {
        let s = format!(
            r#"{}
[[neighborhoods.peers]]
id = 9

[neighborhoods.peers.config.rdma]
addr = "hex:01020304"
"#,
            neighborhood_toml()
        );
        let f = write_cfg(&s);
        let cfg = load(f.path()).expect("load should succeed");
        match cfg.neighborhoods[0].peers[0].config.as_ref().unwrap() {
            peer_spec::Config::Rdma(cfg) => assert_eq!(cfg.addr, "hex:01020304"),
            other => panic!("expected rdma config, got {other:?}"),
        }
    }

    #[test]
    fn rejects_invalid_native_peer_addr() {
        for bad in ["gid:bad", "hex:", "hex:abc", "hex:deadbeefg0"] {
            let s = format!(
                r#"{}
[[neighborhoods.peers]]
id = 9

[neighborhoods.peers.config.rdma]
addr = "{bad}"
"#,
                neighborhood_toml()
            );
            let f = write_cfg(&s);
            assert!(
                matches!(
                    load(f.path()),
                    Err(ConfigError::InvalidNativePeerAddr { peer_id: 9, .. })
                ),
                "expected InvalidNativePeerAddr for {bad:?}"
            );
        }
    }

    #[test]
    fn rejects_empty_disk_path() {
        let s = format!(
            r#"{}
[[caches.disks]]
[caches.disks.config.block]
path = ""
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        assert!(matches!(load(f.path()), Err(ConfigError::EmptyDiskPath)));
    }

    #[test]
    fn loads_file_disk_with_size() {
        let s = format!(
            r#"{}
[[caches.disks]]
[caches.disks.config.file]
path = "/tmp/unbounded-file-disk"
size = 16777216
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.caches[0].disks[0].kind_name(), "file");
        assert_eq!(cfg.caches[0].disks[0].file_size(), Some(16 * 1024 * 1024));
    }

    #[test]
    fn rejects_file_disk_without_size() {
        let s = format!(
            r#"{}
[[caches.disks]]
[caches.disks.config.file]
path = "/tmp/unbounded-file-disk"
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::MissingFileDiskSize(_))
        ));
    }

    #[test]
    fn rejects_file_disk_with_zero_size() {
        let s = format!(
            r#"{}
[[caches.disks]]
[caches.disks.config.file]
path = "/tmp/unbounded-file-disk"
size = 0
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::ZeroFileDiskSize(_))
        ));
    }

    #[test]
    fn rejects_file_disk_size_not_page_multiple() {
        let s = format!(
            r#"{}
[[caches.disks]]
[caches.disks.config.file]
path = "/tmp/unbounded-file-disk"
size = 5000
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::FileDiskSizeNotPageMultiple { .. })
        ));
    }

    #[test]
    fn rejects_size_key_on_non_file_disk() {
        let s = format!(
            r#"{}
[[caches.disks]]
size = 16777216

[caches.disks.config.block]
path = "/dev/nvme0n1"
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        assert!(matches!(load(f.path()), Err(ConfigError::Toml(_))));
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
[[backends]]
name = "b"

[backends.config.fake]

[[neighborhoods]]
name = "n"
source = "b"

[[neighborhoods.peers]]
id = 1

[neighborhoods.peers.config.tcp]
addr = "10.0.0.1:9000"
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
[[backends]]
name = "b"

[backends.config.fake]

[[neighborhoods]]
name = "n"
source = "b"
local_node_id = 7

[[neighborhoods.peers]]
id = 1

[neighborhoods.peers.config.tcp]
addr = "10.0.0.1:9000"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.neighborhoods[0].local_node_id, Some(7));
        assert_eq!(cfg.neighborhoods[0].peers.len(), 1);
    }

    #[test]
    fn rejects_local_node_id_colliding_with_peer() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.fake]

[[neighborhoods]]
name = "n"
source = "b"
local_node_id = 1

[[neighborhoods.peers]]
id = 1

[neighborhoods.peers.config.tcp]
addr = "10.0.0.1:9000"
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
name = "primary-http"

[backends.config.http]
url = "https://origin.example.com"

[[frontends]]
name = "workload-http"
source = "primary-http"

[frontends.config.http]
addr = "0.0.0.0:9000"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.backends.len(), 1);
        assert_eq!(cfg.frontends.len(), 1);
    }

    #[test]
    fn rejects_duplicate_backend_names() {
        let s = r#"
[[backends]]
name = "dup"

[backends.config.http]
url = "https://e"

[[backends]]
name = "dup"

[backends.config.http]
url = "https://e2"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::DuplicateBackendName(name)) if name == "dup" => {}
            other => panic!("expected DuplicateBackendName(dup), got {other:?}"),
        }
    }

    #[test]
    fn rejects_duplicate_frontend_names() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"

[[frontends]]
name = "dup"
source = "b"

[frontends.config.http]
addr = "0.0.0.0:9000"

[[frontends]]
name = "dup"
source = "b"

[frontends.config.http]
addr = "0.0.0.0:9001"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::DuplicateFrontendName(name)) if name == "dup" => {}
            other => panic!("expected DuplicateFrontendName(dup), got {other:?}"),
        }
    }

    #[test]
    fn rejects_dangling_frontend_binding_reference() {
        let s = r#"
[[backends]]
name = "real"

[backends.config.http]
url = "https://e"

[[frontends]]
name = "f"
source = "ghost"

[frontends.config.http]
addr = "0.0.0.0:9000"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::InvalidBindingGraph(msg)) if msg.contains("ghost") => {}
            other => panic!("expected InvalidBindingGraph, got {other:?}"),
        }
    }

    #[test]
    fn rejects_empty_backend_url() {
        let no_url = r#"
[[backends]]
name = "b"

[backends.config.http]
url = ""
"#;
        let f = write_cfg(no_url);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::EmptyBackendUrl(_))
        ));
    }

    #[test]
    fn accepts_fake_backend_without_url() {
        // The fake backend dials no origin, so the otherwise-required
        // url may be omitted; its object size defaults in.
        let s = r#"
[[backends]]
name = "synthetic"

[backends.config.fake]
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("fake backend without url should load");
        assert_eq!(cfg.backends[0].name, "synthetic");
        match cfg.backends[0].config.as_ref().expect("backend config set") {
            backend_spec::Config::Fake(fake) => {
                assert_eq!(fake.object_size_bytes, Some(1024 * 1024));
            }
            other => panic!("expected fake backend config, got {other:?}"),
        }
    }

    #[test]
    fn rejects_non_power_of_two_stripe_size() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"
stripe_size_bytes = 3000000
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::StripeSizeNotPowerOfTwo { backend_name, .. })
                if backend_name == "b" => {}
            other => panic!("expected StripeSizeNotPowerOfTwo, got {other:?}"),
        }
    }

    #[test]
    fn accepts_power_of_two_stripe_size() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"
stripe_size_bytes = 8388608
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.backends[0].stripe_size_bytes(), 8 * 1024 * 1024);
    }

    #[test]
    fn rejects_invalid_frontend_addr() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"

[[frontends]]
name = "f"
source = "b"

[frontends.config.http]
addr = "not-an-addr"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::InvalidFrontendAddr { frontend_name, .. }) if frontend_name == "f" => {
            }
            other => panic!("expected InvalidFrontendAddr, got {other:?}"),
        }
    }

    #[test]
    fn rejects_hostname_for_frontend_addr() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"

[[frontends]]
name = "f"
source = "b"

[frontends.config.http]
addr = "example.com:9000"
"#;
        let f = write_cfg(s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::InvalidFrontendAddr { .. })
        ));
    }

    #[test]
    fn accepts_valid_metrics_addr() {
        let f = write_cfg("[startup.metrics]\naddr = \"0.0.0.0:9100\"\n");
        let cfg = load(f.path()).expect("valid metrics addr loads");
        assert_eq!(cfg.startup().metrics().addr, "0.0.0.0:9100");
    }

    #[test]
    fn empty_metrics_addr_is_allowed() {
        let f = write_cfg("");
        let cfg = load(f.path()).expect("absent metrics section loads");
        assert_eq!(cfg.startup().metrics().addr, "");
    }

    #[test]
    fn rejects_invalid_metrics_addr() {
        let f = write_cfg("[startup.metrics]\naddr = \"not-an-addr\"\n");
        match load(f.path()) {
            Err(ConfigError::InvalidMetricsAddr { addr }) if addr == "not-an-addr" => {}
            other => panic!("expected InvalidMetricsAddr, got {other:?}"),
        }
    }

    #[test]
    fn rejects_duplicate_frontend_addr() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"

[[frontends]]
name = "f1"
source = "b"

[frontends.config.http]
addr = "0.0.0.0:9000"

[[frontends]]
name = "f2"
source = "b"

[frontends.config.http]
addr = "0.0.0.0:9000"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::DuplicateFrontendAddr {
                frontend_name,
                addr,
            }) if frontend_name == "f2" && addr == "0.0.0.0:9000" => {}
            other => panic!("expected DuplicateFrontendAddr, got {other:?}"),
        }
    }

    #[test]
    fn accepts_loadgen_frontend_without_addr() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.fake]

[[frontends]]
name = "lg"
source = "b"

[frontends.config.loadgen]
workers = 4
read_bytes = 65536
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("loadgen frontend without addr should load");
        assert_eq!(cfg.frontends[0].kind_name(), "loadgen");
        assert_eq!(cfg.frontends[0].addr(), None);
    }

    #[test]
    fn loadgen_empty_addr_does_not_collide_with_socket_frontends() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.fake]

[[frontends]]
name = "lg1"
source = "b"

[frontends.config.loadgen]

[[frontends]]
name = "lg2"
source = "b"

[frontends.config.loadgen]

[[frontends]]
name = "http"
source = "b"

[frontends.config.http]
addr = "0.0.0.0:9000"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("empty loadgen addrs should not collide");
        assert_eq!(cfg.frontends.len(), 3);
    }

    #[test]
    fn accepts_s3_backend() {
        let s = r#"
[[backends]]
name = "s3"

[backends.config.s3]
url = "s3.example.com:443"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.backends[0].kind_name(), "s3");
        assert_eq!(cfg.backends[0].url(), Some("s3.example.com:443"));
    }

    #[test]
    fn accepts_azure_backend() {
        let s = r#"
[[backends]]
name = "azure"

[backends.config.azure]
url = "acct.blob.core.windows.net:443"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.backends[0].kind_name(), "azure");
    }

    #[test]
    fn rejects_unknown_key() {
        // A typo in a key now fails loudly at parse time instead of being
        // silently dropped (deny_unknown_fields on the TOML path).
        let s = r#"
[[neighborhoods]]
name = "n"
fingers_per_nod = 128
"#;
        let f = write_cfg(s);
        assert!(matches!(load(f.path()), Err(ConfigError::Toml(_))));
    }

    #[test]
    fn rejects_missing_peer_config() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.fake]

[[neighborhoods]]
name = "n"
source = "b"
local_node_id = 99

[[neighborhoods.peers]]
id = 1
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::MissingPeerConfig(1)) => {}
            other => panic!("expected MissingPeerConfig, got {other:?}"),
        }
    }

    #[test]
    fn rejects_missing_disk_config() {
        let s = format!(
            r#"{}
[[caches.disks]]
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        match load(f.path()) {
            Err(ConfigError::MissingDiskConfig(path)) if path.is_empty() => {}
            other => panic!("expected MissingDiskConfig, got {other:?}"),
        }
    }

    #[test]
    fn loads_binpb_config() {
        // A `.binpb` file is decoded from the protobuf wire format that
        // the TOML loader's `Config` round-trips to.
        let toml = r#"
[[backends]]
name = "b"

[backends.config.fake]

[[neighborhoods]]
name = "n"
source = "b"
local_node_id = 99

[[neighborhoods.peers]]
id = 1

[neighborhoods.peers.config.tcp]
addr = "10.0.0.1:9000"

[[caches]]
name = "c"
source = "n"

[[caches.disks]]
[caches.disks.config.block]
path = "/dev/nvme0n1"

[[frontends]]
name = "f"
source = "c"

[frontends.config.http]
addr = "0.0.0.0:9000"
"#;
        let cfg: Config = toml::from_str(toml).unwrap();
        let f = write_binpb(&encode_config(&cfg));
        let loaded = load(f.path()).unwrap();
        assert_eq!(loaded.neighborhoods[0].peers.len(), 1);
        assert_eq!(loaded.neighborhoods[0].peers[0].id, 1);
        assert_eq!(loaded.caches[0].disks.len(), 1);
        assert_eq!(loaded.neighborhoods[0].local_node_id, Some(99));
    }

    #[test]
    fn binpb_applies_defaults() {
        // The binpb path shares the TOML path's defaulting finalization.
        let f = write_binpb(&encode_config(&Config::default()));
        let loaded = load(f.path()).unwrap();
        assert!(loaded.neighborhoods.is_empty());
        assert_eq!(
            loaded.startup().fabric().default_listen_addr(),
            Some("0.0.0.0:0")
        );
    }

    #[test]
    fn binpb_runs_validation() {
        // Validation runs regardless of encoding: duplicate unscoped peer
        // endpoints are rejected even when they arrive over the protobuf wire
        // path.
        let toml = r#"
[[backends]]
name = "b"

[backends.config.fake]

[[neighborhoods]]
name = "n"
source = "b"
local_node_id = 99

[[neighborhoods.peers]]
id = 1

[neighborhoods.peers.config.tcp]
addr = "10.0.0.1:9000"

[[neighborhoods.peers]]
id = 2

[neighborhoods.peers.config.tcp]
addr = "10.0.0.2:9000"
"#;
        let mut cfg: Config = toml::from_str(toml).unwrap();
        cfg.neighborhoods[0].peers[1].id = 1;
        let f = write_binpb(&encode_config(&cfg));
        match load(f.path()) {
            Err(ConfigError::InvalidBindingGraph(err)) => {
                assert!(err.contains("peer 1 is duplicated"), "{err}");
            }
            other => panic!("expected duplicate peer error, got {other:?}"),
        }
    }

    #[test]
    fn rejects_invalid_binpb_bytes() {
        // A field tag with a truncated varint payload is not a valid
        // protobuf message and surfaces as a decode error.
        let f = write_binpb(&[0x08]);
        match load(f.path()) {
            Err(ConfigError::Protobuf(_)) => {}
            other => panic!("expected Protobuf decode error, got {other:?}"),
        }
    }

    #[test]
    fn loads_startup_defaults_via_toml() {
        // An omitted [startup] section is populated entirely from the
        // documented defaults during load.
        let f = write_cfg("");
        let cfg = load(f.path()).unwrap();
        let s = cfg.startup();
        assert_eq!(s.memory().memory_total_bytes, Some(128 * 1024 * 1024));
        assert_eq!(s.fabric().default_listen_addr(), Some("0.0.0.0:0"));
        assert_eq!(s.fabric().max_inflight, Some(1024));
        assert_eq!(s.topology().nic_workers, Some(4));
    }

    #[test]
    fn loads_startup_section_via_toml() {
        let s = r#"
[startup.memory]
no_hugepages = true
memory_total_bytes = 67108864

[startup.fabric]

[startup.fabric.binds.tcp]
addr = "10.0.0.1:7000"

[startup.topology]
disable_rdma = true
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).unwrap();
        assert!(cfg.startup().memory().no_hugepages);
        assert_eq!(
            cfg.startup().memory().memory_total_bytes,
            Some(64 * 1024 * 1024)
        );
        assert_eq!(
            cfg.startup().fabric().default_listen_addr(),
            Some("10.0.0.1:7000")
        );
        assert!(cfg.startup().topology().disable_rdma);
        // Unset siblings still default.
        assert_eq!(cfg.startup().fabric().progress_threads, Some(2));
    }

    #[test]
    fn startup_round_trips_through_binpb() {
        // The startup section survives the protobuf wire encoding and is
        // re-defaulted on decode, identically to the TOML path.
        let toml = r#"
[startup.fabric]
max_inflight = 4096

[startup.fabric.binds.tcp]
addr = "10.0.0.2:8000"

[startup.topology]
disable_rdma = true
"#;
        let cfg: Config = toml::from_str(toml).unwrap();
        let f = write_binpb(&encode_config(&cfg));
        let loaded = load(f.path()).unwrap();
        assert_eq!(
            loaded.startup().fabric().default_listen_addr(),
            Some("10.0.0.2:8000")
        );
        assert_eq!(loaded.startup().fabric().max_inflight, Some(4096));
        assert!(loaded.startup().topology().disable_rdma);
        assert_eq!(
            loaded.startup().memory().memory_total_bytes,
            Some(128 * 1024 * 1024)
        );
    }
}
