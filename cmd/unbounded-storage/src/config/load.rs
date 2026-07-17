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

use super::graph::{RuntimeGraph, project_runtime, route_snapshot, validate_binding_graph};
use super::schema::{Config, backend_spec, disk_spec, frontend_spec, peer_spec};
use crate::p2p::RouteTableSnapshot;

/// One finalized configuration and every immutable runtime view derived from it.
pub struct LoadedConfig {
    config: Config,
    runtime: RuntimeGraph,
    routes: RouteTableSnapshot,
}

impl fmt::Debug for LoadedConfig {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("LoadedConfig")
            .field("version", &self.config.version)
            .field("backend_count", &self.config.backends.len())
            .field("frontend_count", &self.config.frontends.len())
            .field("cache_count", &self.config.caches.len())
            .field("disk_count", &self.config.disks.len())
            .field("peer_count", &self.config.peers.len())
            .field("route_cache_ids", &self.routes.cache_ids)
            .field("has_fingers", &self.routes.fingers.is_some())
            .finish()
    }
}

impl LoadedConfig {
    pub fn load(path: impl AsRef<Path>) -> Result<Self, ConfigError> {
        load(path.as_ref())
    }

    pub fn from_config(mut config: Config) -> Result<Self, ConfigError> {
        config.apply_defaults();
        validate(&config)?;
        let runtime = project_runtime(&config);
        let routes = route_snapshot(&runtime);
        Ok(Self {
            config,
            runtime,
            routes,
        })
    }

    pub fn config(&self) -> &Config {
        &self.config
    }

    pub fn runtime(&self) -> &RuntimeGraph {
        &self.runtime
    }

    pub fn routes(&self) -> &RouteTableSnapshot {
        &self.routes
    }

    pub fn into_config(self) -> Config {
        self.config
    }
}

#[derive(Debug)]
pub enum ConfigError {
    Io(io::Error),
    Toml {
        span: Option<std::ops::Range<usize>>,
    },
    Protobuf(prost::DecodeError),
    DuplicateDiskPath(String),
    MissingFileDiskSize(String),
    ZeroFileDiskSize(String),
    FileDiskSizeNotPageMultiple {
        path: String,
        size: u64,
        page_size: u64,
    },
    InvalidTcpAddr {
        peer_name: String,
        addr: String,
    },
    InvalidTlsTcpBindAddr(String),
    ZeroTlsTcpBindPort(String),
    EmptyTlsTcpCertPath(&'static str),
    ZeroTlsTcpLanes,
    ZeroTlsTcpRingDepth,
    TlsTcpPeerTransportMismatch(String),
    TlsTcpServerNameMismatch {
        peer_name: String,
        server_name: String,
    },
    InvalidNativePeerAddr {
        peer_name: String,
        addr: String,
    },
    EmptyDiskPath,
    MissingSelfPeer,
    SelfPeerNotFound(String),
    EmptyPeerName,
    DuplicatePeerName(String),
    RoutingPlanUnknownPeer {
        name: String,
        role: &'static str,
    },
    RoutingPlanSelfReference {
        name: String,
        role: &'static str,
    },
    RoutingPlanDuplicateFinger(String),
    DuplicateTopologyPrefixWeight(u32),
    InvalidTopologyPrefixWeight {
        tag_index: u32,
        weight: f64,
    },
    DuplicateBackendName(String),
    DuplicateFrontendName(String),
    DuplicateCacheName(String),
    EmptyBackendName,
    EmptyFrontendName,
    EmptyCacheName,
    MissingBackendConfig(String),
    MissingFrontendConfig(String),
    EmptyBackendUrl(String),
    InvalidBackendUrl {
        backend_name: String,
        reason: String,
    },
    ConflictingTlsConfig(String),
    IncompleteTlsClientAuth(String),
    EmptyTlsValue {
        backend_name: String,
        field: &'static str,
    },
    PlaintextTlsConfig(String),
    MissingS3AuthField {
        backend_name: String,
        field: &'static str,
    },
    S3SessionTokenWithoutCredentials(String),
    EmptyS3AuthValue {
        backend_name: String,
        field: &'static str,
    },
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
    ZeroFrontendMaxRequestsPerConnection(String),
    MissingPeerConfig(String),
    MissingDiskConfig,
    InvalidMetricsAddr {
        addr: String,
    },
    InvalidBindingGraph(String),
}

impl fmt::Display for ConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ConfigError::Io(e) => write!(f, "io error reading config: {e}"),
            ConfigError::Toml { span: Some(span) } => {
                write!(f, "toml parse error at bytes {}..{}", span.start, span.end)
            }
            ConfigError::Toml { span: None } => write!(f, "toml parse error"),
            ConfigError::Protobuf(e) => write!(f, "protobuf decode error: {e}"),
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
            ConfigError::InvalidTcpAddr { peer_name, addr } => {
                write!(f, "peer {peer_name:?}: invalid tcp socket address {addr:?}")
            }
            ConfigError::InvalidTlsTcpBindAddr(addr) => {
                write!(
                    f,
                    "tls_tcp bind addr {addr:?} is not a valid socket address"
                )
            }
            ConfigError::ZeroTlsTcpBindPort(addr) => {
                write!(f, "tls_tcp bind addr {addr:?} must use a nonzero port")
            }
            ConfigError::EmptyTlsTcpCertPath(field) => {
                write!(f, "tls_tcp bind {field} must not be empty")
            }
            ConfigError::ZeroTlsTcpLanes => {
                write!(f, "tls_tcp bind lanes must be greater than zero")
            }
            ConfigError::ZeroTlsTcpRingDepth => {
                write!(f, "tls_tcp bind ring_depth must be greater than zero")
            }
            ConfigError::TlsTcpPeerTransportMismatch(peer_name) => write!(
                f,
                "peer {peer_name:?}: config must use tls_tcp when the fabric bind uses tls_tcp"
            ),
            ConfigError::TlsTcpServerNameMismatch {
                peer_name,
                server_name,
            } => write!(
                f,
                "peer {peer_name:?}: tls_tcp server_name {server_name:?} must match the peer name"
            ),
            ConfigError::InvalidNativePeerAddr { peer_name, addr } => {
                write!(
                    f,
                    "peer {peer_name:?}: invalid rdma fabric address {addr:?}"
                )
            }
            ConfigError::EmptyDiskPath => write!(f, "disk path must not be empty"),
            ConfigError::MissingSelfPeer => write!(
                f,
                "self must be set when peers or routing_plan are configured: a multi-node \
                 deployment requires a stable local peer name"
            ),
            ConfigError::SelfPeerNotFound(name) => write!(
                f,
                "self peer {name:?} is not present in peers: the local process must be listed \
                 in the peer roster"
            ),
            ConfigError::EmptyPeerName => write!(f, "peer name must not be empty"),
            ConfigError::DuplicatePeerName(name) => write!(f, "duplicate peer name: {name:?}"),
            ConfigError::RoutingPlanUnknownPeer { name, role } => write!(
                f,
                "routing_plan {role} {name:?} does not reference any peer name: every \
                 routing-plan neighbor must have a matching peer so a fabric connection exists"
            ),
            ConfigError::RoutingPlanSelfReference { name, role } => write!(
                f,
                "routing_plan {role} {name:?} equals self: a node cannot list \
                 itself as a routing neighbor"
            ),
            ConfigError::RoutingPlanDuplicateFinger(name) => {
                write!(f, "routing_plan.fingers contains duplicate peer {name:?}")
            }
            ConfigError::DuplicateTopologyPrefixWeight(tag_index) => write!(
                f,
                "topology_weighting.prefix_weights contains duplicate tag_index {tag_index}"
            ),
            ConfigError::InvalidTopologyPrefixWeight { tag_index, weight } => write!(
                f,
                "topology_weighting.prefix_weights tag_index {tag_index} has invalid weight {weight}: weight must be finite and have absolute value <= 1000000.0"
            ),
            ConfigError::DuplicateBackendName(name) => {
                write!(f, "duplicate backend name: {name:?}")
            }
            ConfigError::DuplicateFrontendName(name) => {
                write!(f, "duplicate frontend name: {name:?}")
            }
            ConfigError::DuplicateCacheName(name) => write!(f, "duplicate cache name: {name:?}"),
            ConfigError::EmptyBackendName => write!(f, "backend name must not be empty"),
            ConfigError::EmptyFrontendName => write!(f, "frontend name must not be empty"),
            ConfigError::EmptyCacheName => write!(f, "cache name must not be empty"),
            ConfigError::MissingBackendConfig(id) => {
                write!(f, "backend {id:?}: config must set one backend type")
            }
            ConfigError::MissingFrontendConfig(id) => {
                write!(f, "frontend {id:?}: config must set one frontend type")
            }
            ConfigError::EmptyBackendUrl(name) => {
                write!(f, "backend {name:?}: url must not be empty")
            }
            ConfigError::InvalidBackendUrl {
                backend_name,
                reason,
            } => write!(f, "backend {backend_name:?}: invalid url: {reason}"),
            ConfigError::ConflictingTlsConfig(name) => write!(
                f,
                "backend {name:?}: ca_cert and insecure_skip_verify are mutually exclusive"
            ),
            ConfigError::IncompleteTlsClientAuth(name) => write!(
                f,
                "backend {name:?}: client_cert and client_key must be set together"
            ),
            ConfigError::EmptyTlsValue {
                backend_name,
                field,
            } => write!(
                f,
                "backend {backend_name:?}: TLS field {field} must not be empty or whitespace"
            ),
            ConfigError::PlaintextTlsConfig(name) => {
                write!(f, "backend {name:?}: TLS settings require an https url")
            }
            ConfigError::MissingS3AuthField {
                backend_name,
                field,
            } => write!(
                f,
                "backend {backend_name:?}: S3 static authentication requires {field}"
            ),
            ConfigError::S3SessionTokenWithoutCredentials(name) => write!(
                f,
                "backend {name:?}: S3 session_token requires region, access_key_id, and secret_access_key"
            ),
            ConfigError::EmptyS3AuthValue {
                backend_name,
                field,
            } => write!(
                f,
                "backend {backend_name:?}: S3 authentication field {field} must not be empty or whitespace"
            ),
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
            ConfigError::ZeroFrontendMaxRequestsPerConnection(frontend_name) => write!(
                f,
                "frontend {frontend_name:?}: max_requests_per_connection must be greater than zero"
            ),
            ConfigError::MissingPeerConfig(peer_name) => {
                write!(f, "peer {peer_name:?}: config must set one peer transport")
            }
            ConfigError::MissingDiskConfig => write!(f, "disk config must set one disk type"),
            ConfigError::InvalidMetricsAddr { addr } => {
                write!(f, "metrics addr {addr:?} is not a valid socket address")
            }
            ConfigError::InvalidBindingGraph(msg) => write!(f, "invalid binding graph: {msg}"),
        }
    }
}

impl std::error::Error for ConfigError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            ConfigError::Io(e) => Some(e),
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
        ConfigError::Toml { span: e.span() }
    }
}

impl From<prost::DecodeError> for ConfigError {
    fn from(e: prost::DecodeError) -> Self {
        ConfigError::Protobuf(e)
    }
}

impl Config {
    pub fn load(path: impl AsRef<Path>) -> Result<Self, ConfigError> {
        LoadedConfig::load(path).map(LoadedConfig::into_config)
    }
}

/// Loads a config from `path`, decoding raw binary protobuf for a
/// `.binpb` extension and strict TOML for anything else. Both encodings
/// share the same defaulting and validation finalization.
pub fn load(path: &Path) -> Result<LoadedConfig, ConfigError> {
    let cfg = if has_binpb_extension(path) {
        Config::decode(fs::read(path)?.as_slice())?
    } else {
        toml::from_str(&fs::read_to_string(path)?)?
    };
    LoadedConfig::from_config(cfg)
}

fn has_binpb_extension(path: &Path) -> bool {
    path.extension().and_then(|e| e.to_str()) == Some("binpb")
}

fn validate(cfg: &Config) -> Result<(), ConfigError> {
    validate_tls_tcp_fabric(cfg)?;

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
                validate_backend_url(
                    &b.name,
                    &cfg.url,
                    &cfg.ca_cert,
                    cfg.insecure_skip_verify,
                    &cfg.client_cert,
                    &cfg.client_key,
                )?;
                cfg.stripe_size_bytes.unwrap_or(0)
            }
            Some(backend_spec::Config::S3(cfg)) => {
                validate_backend_url(
                    &b.name,
                    &cfg.url,
                    &cfg.ca_cert,
                    cfg.insecure_skip_verify,
                    &cfg.client_cert,
                    &cfg.client_key,
                )?;
                validate_s3_auth(&b.name, cfg)?;
                cfg.stripe_size_bytes.unwrap_or(0)
            }
            Some(backend_spec::Config::Azure(cfg)) => {
                validate_backend_url(
                    &b.name,
                    &cfg.url,
                    &cfg.ca_cert,
                    cfg.insecure_skip_verify,
                    &cfg.client_cert,
                    &cfg.client_key,
                )?;
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
    for cache in &cfg.caches {
        if cache.name.is_empty() {
            return Err(ConfigError::EmptyCacheName);
        }
        if !seen_caches.insert(cache.name.as_str()) {
            return Err(ConfigError::DuplicateCacheName(cache.name.clone()));
        }
    }

    let mut seen_disk_paths: HashSet<&str> = HashSet::new();
    validate_disks(&cfg.disks)?;
    for disk in &cfg.disks {
        let path = validated_disk_path(disk)?;
        if !seen_disk_paths.insert(path) {
            return Err(ConfigError::DuplicateDiskPath(path.to_string()));
        }
    }

    validate_mesh(cfg)?;

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
                if cfg.max_requests_per_connection == Some(0) {
                    return Err(ConfigError::ZeroFrontendMaxRequestsPerConnection(
                        fr.name.clone(),
                    ));
                }
            }
            Some(frontend_spec::Config::S3(cfg)) => {
                validate_frontend_addr(&fr.name, &cfg.addr, &mut seen_addrs)?;
            }
            Some(frontend_spec::Config::Loadgen(_)) => {}
            None => return Err(ConfigError::MissingFrontendConfig(fr.name.clone())),
        }
    }

    validate_binding_graph(cfg).map_err(ConfigError::InvalidBindingGraph)?;

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

fn validate_backend_url(
    backend_name: &str,
    url: &str,
    ca_cert: &Option<String>,
    insecure_skip_verify: bool,
    client_cert: &Option<String>,
    client_key: &Option<String>,
) -> Result<(), ConfigError> {
    if url.is_empty() {
        return Err(ConfigError::EmptyBackendUrl(backend_name.to_string()));
    }
    let parsed =
        crate::backend::url::parse_endpoint(url).map_err(|e| ConfigError::InvalidBackendUrl {
            backend_name: backend_name.to_string(),
            reason: e.to_string(),
        })?;
    for (field, value) in [
        ("ca_cert", ca_cert),
        ("client_cert", client_cert),
        ("client_key", client_key),
    ] {
        if value.as_deref().is_some_and(|value| value.trim().is_empty()) {
            return Err(ConfigError::EmptyTlsValue {
                backend_name: backend_name.to_string(),
                field,
            });
        }
    }
    if ca_cert.is_some() && insecure_skip_verify {
        return Err(ConfigError::ConflictingTlsConfig(backend_name.to_string()));
    }
    if client_cert.is_some() != client_key.is_some() {
        return Err(ConfigError::IncompleteTlsClientAuth(
            backend_name.to_string(),
        ));
    }
    if !parsed.scheme.is_tls()
        && (ca_cert.is_some()
            || insecure_skip_verify
            || client_cert.is_some()
            || client_key.is_some())
    {
        return Err(ConfigError::PlaintextTlsConfig(backend_name.to_string()));
    }

    Ok(())
}

fn validate_s3_auth(
    backend_name: &str,
    cfg: &super::schema::S3BackendConfig,
) -> Result<(), ConfigError> {
    let credentials_absent = cfg.region.is_none()
        && cfg.access_key_id.is_none()
        && cfg.secret_access_key.is_none();
    if credentials_absent && cfg.session_token.is_none() {
        return Ok(());
    }
    if credentials_absent {
        return Err(ConfigError::S3SessionTokenWithoutCredentials(
            backend_name.to_string(),
        ));
    }

    for (field, value) in [
        ("region", &cfg.region),
        ("access_key_id", &cfg.access_key_id),
        ("secret_access_key", &cfg.secret_access_key),
    ] {
        let Some(value) = value else {
            return Err(ConfigError::MissingS3AuthField {
                backend_name: backend_name.to_string(),
                field,
            });
        };
        if value.trim().is_empty() {
            return Err(ConfigError::EmptyS3AuthValue {
                backend_name: backend_name.to_string(),
                field,
            });
        }
    }
    if cfg
        .session_token
        .as_deref()
        .is_some_and(|token| token.trim().is_empty())
    {
        return Err(ConfigError::EmptyS3AuthValue {
            backend_name: backend_name.to_string(),
            field: "session_token",
        });
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

fn validate_mesh(cfg: &Config) -> Result<(), ConfigError> {
    if (!cfg.peers.is_empty() || cfg.routing_plan.is_some()) && cfg.self_.is_empty() {
        return Err(ConfigError::MissingSelfPeer);
    }

    let mut peer_names: HashSet<&str> = HashSet::new();
    let mut self_seen = cfg.self_.is_empty();
    for p in &cfg.peers {
        if p.name.is_empty() {
            return Err(ConfigError::EmptyPeerName);
        }
        if !peer_names.insert(p.name.as_str()) {
            return Err(ConfigError::DuplicatePeerName(p.name.clone()));
        }
        if p.name == cfg.self_ {
            self_seen = true;
        }
        match p.config.as_ref() {
            Some(peer_spec::Config::Tcp(cfg)) => {
                if cfg.addr.parse::<SocketAddr>().is_err() {
                    return Err(ConfigError::InvalidTcpAddr {
                        peer_name: p.name.clone(),
                        addr: cfg.addr.clone(),
                    });
                }
            }
            Some(peer_spec::Config::Rdma(cfg)) => {
                if !is_valid_rdma_peer_address(&cfg.addr) {
                    return Err(ConfigError::InvalidNativePeerAddr {
                        peer_name: p.name.clone(),
                        addr: cfg.addr.clone(),
                    });
                }
                for addr in &cfg.addrs {
                    if !is_valid_rdma_peer_address(addr) {
                        return Err(ConfigError::InvalidNativePeerAddr {
                            peer_name: p.name.clone(),
                            addr: addr.clone(),
                        });
                    }
                }
            }
            Some(peer_spec::Config::TlsTcp(peer_cfg)) => {
                validate_tls_tcp_peer_addr(&p.name, &peer_cfg.addr)?;
                if matches!(
                    cfg.startup().fabric().binds,
                    Some(super::schema::fabric_cfg::Binds::TlsTcp(_))
                ) && peer_cfg.server_name != p.name
                {
                    return Err(ConfigError::TlsTcpServerNameMismatch {
                        peer_name: p.name.clone(),
                        server_name: peer_cfg.server_name.clone(),
                    });
                }
            }
            None => return Err(ConfigError::MissingPeerConfig(p.name.clone())),
        }

        if matches!(
            cfg.startup().fabric().binds,
            Some(super::schema::fabric_cfg::Binds::TlsTcp(_))
        ) && !matches!(p.config, Some(peer_spec::Config::TlsTcp(_)))
        {
            return Err(ConfigError::TlsTcpPeerTransportMismatch(p.name.clone()));
        }
    }
    if !self_seen {
        return Err(ConfigError::SelfPeerNotFound(cfg.self_.clone()));
    }

    if let Some(plan) = &cfg.routing_plan {
        let mut seen_fingers: HashSet<&str> = HashSet::new();
        for name in &plan.fingers {
            if !seen_fingers.insert(name.as_str()) {
                return Err(ConfigError::RoutingPlanDuplicateFinger(name.clone()));
            }
        }
        for (name, role) in plan
            .fingers
            .iter()
            .map(|name| (name.as_str(), "finger"))
            .chain(plan.successor.as_deref().map(|name| (name, "successor")))
            .chain(
                plan.predecessor
                    .as_deref()
                    .map(|name| (name, "predecessor")),
            )
        {
            if name == cfg.self_ {
                return Err(ConfigError::RoutingPlanSelfReference {
                    name: name.to_string(),
                    role,
                });
            }
            if !peer_names.contains(name) {
                return Err(ConfigError::RoutingPlanUnknownPeer {
                    name: name.to_string(),
                    role,
                });
            }
        }
    }

    if let Some(weighting) = &cfg.topology_weighting {
        let mut seen_weights = HashSet::new();
        for weight in &weighting.prefix_weights {
            if !seen_weights.insert(weight.tag_index) {
                return Err(ConfigError::DuplicateTopologyPrefixWeight(weight.tag_index));
            }
            if !weight.weight.is_finite() || weight.weight.abs() > 1_000_000.0 {
                return Err(ConfigError::InvalidTopologyPrefixWeight {
                    tag_index: weight.tag_index,
                    weight: weight.weight,
                });
            }
        }
    }

    Ok(())
}

fn validate_tls_tcp_peer_addr(peer_name: &str, addr: &str) -> Result<(), ConfigError> {
    let parsed = addr
        .parse::<SocketAddr>()
        .map_err(|_| ConfigError::InvalidTcpAddr {
            peer_name: peer_name.to_string(),
            addr: addr.to_string(),
        })?;
    if parsed.port() == 0 {
        return Err(ConfigError::InvalidTcpAddr {
            peer_name: peer_name.to_string(),
            addr: addr.to_string(),
        });
    }

    Ok(())
}

fn validate_tls_tcp_fabric(cfg: &Config) -> Result<(), ConfigError> {
    let Some(super::schema::fabric_cfg::Binds::TlsTcp(tls_tcp)) =
        cfg.startup().fabric().binds.as_ref()
    else {
        return Ok(());
    };

    let addr = tls_tcp
        .addr
        .parse::<SocketAddr>()
        .map_err(|_| ConfigError::InvalidTlsTcpBindAddr(tls_tcp.addr.clone()))?;
    if addr.port() == 0 {
        return Err(ConfigError::ZeroTlsTcpBindPort(tls_tcp.addr.clone()));
    }
    for (field, path) in [
        ("ca_cert_path", tls_tcp.ca_cert_path.as_str()),
        ("cert_path", tls_tcp.cert_path.as_str()),
        ("key_path", tls_tcp.key_path.as_str()),
    ] {
        if path.is_empty() {
            return Err(ConfigError::EmptyTlsTcpCertPath(field));
        }
    }
    if tls_tcp.lanes == Some(0) {
        return Err(ConfigError::ZeroTlsTcpLanes);
    }
    if tls_tcp.ring_depth == Some(0) {
        return Err(ConfigError::ZeroTlsTcpRingDepth);
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
            None => return Err(ConfigError::MissingDiskConfig),
        }
    }

    Ok(())
}

fn validated_disk_path(disk: &super::schema::DiskSpec) -> Result<&str, ConfigError> {
    let Some(path) = disk.path() else {
        return Err(ConfigError::MissingDiskConfig);
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

fn is_valid_rdma_peer_address(addr: &str) -> bool {
    is_valid_native_address(addr) || addr.parse::<SocketAddr>().is_ok()
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

    fn mesh_toml() -> &'static str {
        r#"
self = "node-a"

[[backends]]
name = "b"

[backends.config.fake]

[[peers]]
name = "node-a"

[peers.config.tcp]
addr = "10.0.0.99:9000"
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

    fn s3_toml(auth: &str) -> String {
        format!(
            r#"
[[backends]]
name = "s3"

[backends.config.s3]
url = "https://s3.example.com"
{auth}
"#
        )
    }

    #[test]
    fn loads_minimal_config() {
        let f = write_cfg("");
        let cfg = load(f.path()).unwrap();
        let cfg = cfg.config();
        assert!(cfg.peers.is_empty());
        assert!(cfg.caches.is_empty());
    }

    #[test]
    fn loads_full_happy_path() {
        let s = r#"
self = "node-a"

[[backends]]
name = "b"

[backends.config.fake]

[[peers]]
name = "node-a"

[peers.config.tcp]
addr = "10.0.0.1:9000"

[[peers]]
name = "node-b"

[peers.config.rdma]
addr = "hex:deadbeef"

[[caches]]
name = "c"
source = "b"

[[disks]]
[disks.config.block]
numa = 0
path = "/dev/nvme0n1"

[[disks]]
[disks.config.block]
path = "/dev/nvme1n1"

[[frontends]]
name = "f"
source = "c"

[frontends.config.http]
addr = "0.0.0.0:9000"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).unwrap();
        let raw = cfg.config();
        assert_eq!(raw.peers.len(), 2);
        assert_eq!(raw.disks.len(), 2);
        assert_eq!(raw.self_, "node-a");
        assert!(!cfg.runtime().frontends["f"].bypass_cache);
        assert_eq!(cfg.runtime().frontends["f"].backend_id, "b");
        assert!(cfg.routes().cache_ids.contains("c"));
        assert!(cfg.routes().fingers.is_some());
    }

    #[test]
    fn rejects_duplicate_peer_names() {
        let s = r#"
self = "node-a"

[[backends]]
name = "b"

[backends.config.fake]

[[peers]]
name = "node-a"

[peers.config.tcp]
addr = "10.0.0.1:9000"

[[peers]]
name = "node-a"

[peers.config.tcp]
addr = "10.0.0.2:9000"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::DuplicatePeerName(name)) if name == "node-a" => {}
            other => panic!("expected duplicate peer error, got {other:?}"),
        }
    }

    #[test]
    fn rejects_duplicate_disk_paths() {
        let s = format!(
            r#"{}
[[disks]]
[disks.config.block]
path = "/dev/nvme0n1"

[[disks]]
[disks.config.block]
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
    fn rejects_invalid_tcp_addr() {
        let s = format!(
            r#"{}
[[peers]]
name = "node-b"

[peers.config.tcp]
addr = "not-an-addr"
"#,
            mesh_toml()
        );
        let f = write_cfg(&s);
        match load(f.path()) {
            Err(ConfigError::InvalidTcpAddr { peer_name, .. }) if peer_name == "node-b" => {}
            other => panic!("expected InvalidTcpAddr, got {other:?}"),
        }
    }

    #[test]
    fn rejects_duplicate_topology_prefix_weights() {
        let s = format!(
            r#"{}
[topology_weighting]

[[topology_weighting.prefix_weights]]
tag_index = 1
weight = 0.25

[[topology_weighting.prefix_weights]]
tag_index = 1
weight = -0.5
"#,
            mesh_toml()
        );
        let f = write_cfg(&s);
        match load(f.path()) {
            Err(ConfigError::DuplicateTopologyPrefixWeight(1)) => {}
            other => panic!("expected DuplicateTopologyPrefixWeight, got {other:?}"),
        }
    }

    #[test]
    fn rejects_non_finite_topology_prefix_weight() {
        let s = format!(
            r#"{}
[topology_weighting]

[[topology_weighting.prefix_weights]]
tag_index = 0
weight = inf
"#,
            mesh_toml()
        );
        let f = write_cfg(&s);
        match load(f.path()) {
            Err(ConfigError::InvalidTopologyPrefixWeight { tag_index: 0, .. }) => {}
            other => panic!("expected InvalidTopologyPrefixWeight, got {other:?}"),
        }
    }

    #[test]
    fn rejects_excessive_topology_prefix_weight() {
        let s = format!(
            r#"{}
[topology_weighting]

[[topology_weighting.prefix_weights]]
tag_index = 0
weight = 1000000.1
"#,
            mesh_toml()
        );
        let f = write_cfg(&s);
        match load(f.path()) {
            Err(ConfigError::InvalidTopologyPrefixWeight { tag_index: 0, .. }) => {}
            other => panic!("expected InvalidTopologyPrefixWeight, got {other:?}"),
        }
    }

    #[test]
    fn rejects_hostname_for_tcp() {
        let s = format!(
            r#"{}
[[peers]]
name = "node-b"

[peers.config.tcp]
addr = "example.com:9000"
"#,
            mesh_toml()
        );
        let f = write_cfg(&s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::InvalidTcpAddr { peer_name, .. }) if peer_name == "node-b"
        ));
    }

    #[test]
    fn accepts_native_peer_addr() {
        let s = format!(
            r#"{}
[[peers]]
name = "node-rdma"

[peers.config.rdma]
addr = "hex:01020304"
"#,
            mesh_toml()
        );
        let f = write_cfg(&s);
        let cfg = load(f.path()).expect("load should succeed");
        let peer = cfg
            .config()
            .peers
            .iter()
            .find(|peer| peer.name == "node-rdma")
            .unwrap();
        match peer.config.as_ref().unwrap() {
            peer_spec::Config::Rdma(cfg) => assert_eq!(cfg.addr, "hex:01020304"),
            other => panic!("expected rdma config, got {other:?}"),
        }
    }

    #[test]
    fn accepts_rdma_socket_peer_addr() {
        let s = format!(
            r#"{}
[[peers]]
name = "node-rdma"

[peers.config.rdma]
addr = "10.0.0.2:5000"
"#,
            mesh_toml()
        );
        let f = write_cfg(&s);
        let cfg = load(f.path()).expect("load should succeed");
        let peer = cfg
            .config()
            .peers
            .iter()
            .find(|peer| peer.name == "node-rdma")
            .unwrap();
        match peer.config.as_ref().unwrap() {
            peer_spec::Config::Rdma(cfg) => assert_eq!(cfg.addr, "10.0.0.2:5000"),
            other => panic!("expected rdma config, got {other:?}"),
        }
    }

    #[test]
    fn accepts_rdma_peer_addr_list() {
        let s = format!(
            r#"{}
[[peers]]
name = "node-rdma"

[peers.config.rdma]
addr = "hex:01020304"
addrs = ["hex:01020304", "10.0.0.2:5000"]
"#,
            mesh_toml()
        );
        let f = write_cfg(&s);
        let cfg = load(f.path()).expect("load should succeed");
        let peer = cfg
            .config()
            .peers
            .iter()
            .find(|peer| peer.name == "node-rdma")
            .unwrap();
        match peer.config.as_ref().unwrap() {
            peer_spec::Config::Rdma(cfg) => {
                assert_eq!(cfg.addr, "hex:01020304");
                assert_eq!(cfg.addrs, ["hex:01020304", "10.0.0.2:5000"]);
            }
            other => panic!("expected rdma config, got {other:?}"),
        }
    }

    #[test]
    fn rejects_invalid_native_peer_addr() {
        for bad in [
            "gid:bad",
            "hex:",
            "hex:abc",
            "hex:deadbeefg0",
            "example.com:9000",
        ] {
            let s = format!(
                r#"{}
[[peers]]
name = "node-rdma"

[peers.config.rdma]
addr = "{bad}"
"#,
                mesh_toml()
            );
            let f = write_cfg(&s);
            assert!(
                matches!(
                    load(f.path()),
                    Err(ConfigError::InvalidNativePeerAddr { peer_name, .. }) if peer_name == "node-rdma"
                ),
                "expected InvalidNativePeerAddr for {bad:?}"
            );
        }
    }

    #[test]
    fn rejects_invalid_rdma_peer_addr_list_entry() {
        let s = format!(
            r#"{}
[[peers]]
name = "node-rdma"

[peers.config.rdma]
addr = "hex:01020304"
addrs = ["hex:01020304", "example.com:5000"]
"#,
            mesh_toml()
        );
        let f = write_cfg(&s);

        assert!(matches!(
            load(f.path()),
            Err(ConfigError::InvalidNativePeerAddr { peer_name, addr })
                if peer_name == "node-rdma" && addr == "example.com:5000"
        ));
    }

    #[test]
    fn rejects_empty_disk_path() {
        let s = format!(
            r#"{}
[[disks]]
[disks.config.block]
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
[[disks]]
[disks.config.file]
path = "/tmp/unbounded-file-disk"
size = 16777216
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.config().disks[0].kind_name(), "file");
        assert_eq!(cfg.config().disks[0].file_size(), Some(16 * 1024 * 1024));
    }

    #[test]
    fn rejects_file_disk_without_size() {
        let s = format!(
            r#"{}
[[disks]]
[disks.config.file]
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
[[disks]]
[disks.config.file]
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
[[disks]]
[disks.config.file]
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
[[disks]]
size = 16777216

[disks.config.block]
path = "/dev/nvme0n1"
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::Toml { .. })
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
        const SECRET: &str = "TOML_SECRET_SENTINEL";
        let f = write_cfg(&format!("secret_access_key = {SECRET:?} trailing"));
        let error = load(f.path()).unwrap_err();
        assert!(matches!(error, ConfigError::Toml { .. }));
        assert!(!error.to_string().contains(SECRET));
    }

    #[test]
    fn rejects_peers_without_self() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.fake]

[[peers]]
name = "node-a"

[peers.config.tcp]
addr = "10.0.0.1:9000"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::MissingSelfPeer) => {}
            other => panic!("expected MissingSelfPeer, got {other:?}"),
        }
    }

    #[test]
    fn accepts_peers_with_self() {
        let s = r#"
self = "node-a"

[[backends]]
name = "b"

[backends.config.fake]

[[peers]]
name = "node-a"

[peers.config.tcp]
addr = "10.0.0.1:9000"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.config().self_, "node-a");
        assert_eq!(cfg.config().peers.len(), 1);
    }

    #[test]
    fn rejects_self_not_in_peer_roster() {
        let s = r#"
self = "node-a"

[[backends]]
name = "b"

[backends.config.fake]

[[peers]]
name = "node-b"

[peers.config.tcp]
addr = "10.0.0.1:9000"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::SelfPeerNotFound(name)) if name == "node-a" => {}
            other => panic!("expected SelfPeerNotFound(node-a), got {other:?}"),
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
        assert_eq!(cfg.config().backends.len(), 1);
        assert_eq!(cfg.config().frontends.len(), 1);
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
        assert_eq!(cfg.config().backends[0].name, "synthetic");
        match cfg.config().backends[0]
            .config
            .as_ref()
            .expect("backend config set")
        {
            backend_spec::Config::Fake(fake) => {
                assert_eq!(fake.object_size_bytes, Some(1024 * 1024));
            }
            other => panic!("expected fake backend config, got {other:?}"),
        }
    }

    #[test]
    fn rejects_backend_url_without_scheme() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "origin.example.com:443"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::InvalidBackendUrl { backend_name, .. }) if backend_name == "b" => {}
            other => panic!("expected InvalidBackendUrl, got {other:?}"),
        }
    }

    #[test]
    fn invalid_backend_url_error_does_not_expose_url() {
        const SECRET: &str = "URL_SECRET_SENTINEL";
        let s = format!(
            r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://user:{SECRET}@origin.example.com"
"#
        );
        let f = write_cfg(&s);
        let error = load(f.path()).unwrap_err();
        assert!(matches!(error, ConfigError::InvalidBackendUrl { .. }));
        assert!(!error.to_string().contains(SECRET));
    }

    #[test]
    fn loaded_config_debug_does_not_expose_credentials() {
        const SECRET: &str = "CONFIG_SECRET_SENTINEL";
        let s = s3_toml(&format!(
            "region = \"us-east-1\"\naccess_key_id = \"access\"\nsecret_access_key = {SECRET:?}"
        ));
        let f = write_cfg(&s);
        let loaded = load(f.path()).unwrap();
        let debug = format!("{loaded:?}");
        assert!(!debug.contains(SECRET));
        assert!(debug.contains("backend_count"));
    }

    #[test]
    fn rejects_conflicting_tls_config() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"
ca_cert = "CA PEM"
insecure_skip_verify = true
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::ConflictingTlsConfig(name)) if name == "b" => {}
            other => panic!("expected ConflictingTlsConfig, got {other:?}"),
        }
    }

    #[test]
    fn rejects_whitespace_tls_inline_values() {
        for (field, value) in [
            ("ca_cert", "   "),
            ("client_cert", "\n\t"),
            ("client_key", "  \n"),
        ] {
            let s = format!(
                r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"
{field} = '''{value}'''
"#
            );
            let f = write_cfg(&s);
            assert!(
                matches!(
                    load(f.path()),
                    Err(ConfigError::EmptyTlsValue { backend_name, field: actual })
                        if backend_name == "b" && actual == field
                ),
                "expected EmptyTlsValue for {field}"
            );
        }
    }

    #[test]
    fn rejects_tls_config_on_plaintext_url() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "http://e"
insecure_skip_verify = true
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::PlaintextTlsConfig(name)) if name == "b" => {}
            other => panic!("expected PlaintextTlsConfig, got {other:?}"),
        }
    }

    #[test]
    fn rejects_client_cert_without_client_key() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"
client_cert = "CLIENT CERT PEM"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::IncompleteTlsClientAuth(name)) if name == "b" => {}
            other => panic!("expected IncompleteTlsClientAuth, got {other:?}"),
        }
    }

    #[test]
    fn rejects_client_key_without_client_cert() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"
client_key = "CLIENT KEY PEM"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::IncompleteTlsClientAuth(name)) if name == "b" => {}
            other => panic!("expected IncompleteTlsClientAuth, got {other:?}"),
        }
    }

    #[test]
    fn rejects_client_auth_on_plaintext_url() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "http://e"
client_cert = "CLIENT CERT PEM"
client_key = "CLIENT KEY PEM"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::PlaintextTlsConfig(name)) if name == "b" => {}
            other => panic!("expected PlaintextTlsConfig, got {other:?}"),
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
        assert_eq!(
            cfg.config().backends[0].stripe_size_bytes(),
            8 * 1024 * 1024
        );
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
    fn rejects_zero_frontend_request_cap() {
        let s = r#"
[[backends]]
name = "b"

[backends.config.http]
url = "https://e"

[[frontends]]
name = "f"
source = "b"

[frontends.config.http]
addr = "127.0.0.1:9000"
max_requests_per_connection = 0
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::ZeroFrontendMaxRequestsPerConnection(frontend_name))
                if frontend_name == "f" => {}
            other => panic!("expected ZeroFrontendMaxRequestsPerConnection, got {other:?}"),
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
        assert_eq!(cfg.config().startup().metrics().addr, "0.0.0.0:9100");
    }

    #[test]
    fn empty_metrics_addr_is_allowed() {
        let f = write_cfg("");
        let cfg = load(f.path()).expect("absent metrics section loads");
        assert_eq!(cfg.config().startup().metrics().addr, "");
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
        assert_eq!(cfg.config().frontends[0].kind_name(), "loadgen");
        assert_eq!(cfg.config().frontends[0].addr(), None);
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
        assert_eq!(cfg.config().frontends.len(), 3);
    }

    #[test]
    fn accepts_s3_backend() {
        let s = r#"
[[backends]]
name = "s3"

[backends.config.s3]
url = "https://s3.example.com:443"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.config().backends[0].kind_name(), "s3");
        assert_eq!(
            cfg.config().backends[0].url(),
            Some("https://s3.example.com:443")
        );
    }

    #[test]
    fn accepts_s3_static_auth_with_session_token() {
        let s = s3_toml(
            r#"region = "us-east-1"
access_key_id = "access"
secret_access_key = "secret"
session_token = "token""#,
        );
        let f = write_cfg(&s);
        let cfg = load(f.path()).expect("authenticated S3 config should load");
        let Some(backend_spec::Config::S3(s3)) = cfg.config().backends[0].config.as_ref() else {
            panic!("expected s3 backend config");
        };
        assert_eq!(s3.region.as_deref(), Some("us-east-1"));
        assert_eq!(s3.access_key_id.as_deref(), Some("access"));
        assert_eq!(s3.secret_access_key.as_deref(), Some("secret"));
        assert_eq!(s3.session_token.as_deref(), Some("token"));
    }

    #[test]
    fn accepts_anonymous_s3_auth() {
        let s = s3_toml("");
        let f = write_cfg(&s);
        let cfg = load(f.path()).expect("anonymous S3 config should load");
        let Some(backend_spec::Config::S3(s3)) = cfg.config().backends[0].config.as_ref() else {
            panic!("expected s3 backend config");
        };
        assert_eq!(s3.region, None);
        assert_eq!(s3.access_key_id, None);
        assert_eq!(s3.secret_access_key, None);
        assert_eq!(s3.session_token, None);
    }

    #[test]
    fn rejects_s3_static_auth_missing_each_required_field() {
        for (missing, auth) in [
            (
                "region",
                r#"access_key_id = "access"
secret_access_key = "secret""#,
            ),
            (
                "access_key_id",
                r#"region = "us-east-1"
secret_access_key = "secret""#,
            ),
            (
                "secret_access_key",
                r#"region = "us-east-1"
access_key_id = "access""#,
            ),
        ] {
            let s = s3_toml(auth);
            let f = write_cfg(&s);
            assert!(
                matches!(
                    load(f.path()),
                    Err(ConfigError::MissingS3AuthField { backend_name, field })
                        if backend_name == "s3" && field == missing
                ),
                "expected missing S3 auth field {missing}"
            );
        }
    }

    #[test]
    fn rejects_s3_session_token_without_credentials() {
        let s = s3_toml(r#"session_token = "token""#);
        let f = write_cfg(&s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::S3SessionTokenWithoutCredentials(name)) if name == "s3"
        ));
    }

    #[test]
    fn rejects_whitespace_s3_auth_values() {
        for field in [
            "region",
            "access_key_id",
            "secret_access_key",
            "session_token",
        ] {
            let mut values = [
                ("region", "us-east-1"),
                ("access_key_id", "access"),
                ("secret_access_key", "secret"),
                ("session_token", "token"),
            ];
            values.iter_mut().find(|(name, _)| *name == field).unwrap().1 = " \t ";
            let auth = values
                .iter()
                .map(|(name, value)| format!("{name} = {value:?}"))
                .collect::<Vec<_>>()
                .join("\n");
            let s = s3_toml(&auth);
            let f = write_cfg(&s);
            assert!(
                matches!(
                    load(f.path()),
                    Err(ConfigError::EmptyS3AuthValue { backend_name, field: actual })
                        if backend_name == "s3" && actual == field
                ),
                "expected whitespace S3 auth field {field}"
            );
        }
    }

    #[test]
    fn accepts_azure_backend() {
        let s = r#"
[[backends]]
name = "azure"

[backends.config.azure]
url = "https://acct.blob.core.windows.net:443"
"#;
        let f = write_cfg(s);
        let cfg = load(f.path()).expect("load should succeed");
        assert_eq!(cfg.config().backends[0].kind_name(), "azure");
    }

    #[test]
    fn rejects_unknown_key() {
        // A typo in a key now fails loudly at parse time instead of being
        // silently dropped (deny_unknown_fields on the TOML path).
        let s = r#"
fingers_per_nod = 128
"#;
        let f = write_cfg(s);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::Toml { .. })
        ));
    }

    #[test]
    fn rejects_missing_peer_config() {
        let s = r#"
self = "node-a"

[[backends]]
name = "b"

[backends.config.fake]

[[peers]]
name = "node-a"
"#;
        let f = write_cfg(s);
        match load(f.path()) {
            Err(ConfigError::MissingPeerConfig(name)) if name == "node-a" => {}
            other => panic!("expected MissingPeerConfig, got {other:?}"),
        }
    }

    #[test]
    fn rejects_missing_disk_config() {
        let s = format!(
            r#"{}
[[disks]]
"#,
            cache_toml()
        );
        let f = write_cfg(&s);
        match load(f.path()) {
            Err(ConfigError::MissingDiskConfig) => {}
            other => panic!("expected MissingDiskConfig, got {other:?}"),
        }
    }

    #[test]
    fn loads_binpb_config() {
        // A `.binpb` file is decoded from the protobuf wire format that
        // the TOML loader's `Config` round-trips to.
        let toml = r#"
self = "node-a"

[[backends]]
name = "b"

[backends.config.fake]

[[peers]]
name = "node-a"

[peers.config.tcp]
addr = "10.0.0.1:9000"

[[caches]]
name = "c"
source = "b"

[[disks]]
[disks.config.block]
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
        assert_eq!(loaded.config().peers.len(), 1);
        assert_eq!(loaded.config().peers[0].name, "node-a");
        assert_eq!(loaded.config().disks.len(), 1);
        assert_eq!(loaded.config().self_, "node-a");
    }

    #[test]
    fn binpb_applies_defaults() {
        // The binpb path shares the TOML path's defaulting finalization.
        let f = write_binpb(&encode_config(&Config::default()));
        let loaded = load(f.path()).unwrap();
        assert!(loaded.config().peers.is_empty());
        assert_eq!(
            loaded.config().startup().fabric().default_listen_addr(),
            Some("0.0.0.0:0")
        );
    }

    #[test]
    fn binpb_runs_validation() {
        // Validation runs regardless of encoding: duplicate unscoped peer
        // endpoints are rejected even when they arrive over the protobuf wire
        // path.
        let toml = r#"
self = "node-a"

[[backends]]
name = "b"

[backends.config.fake]

[[peers]]
name = "node-a"

[peers.config.tcp]
addr = "10.0.0.1:9000"

[[peers]]
name = "node-b"

[peers.config.tcp]
addr = "10.0.0.2:9000"
"#;
        let mut cfg: Config = toml::from_str(toml).unwrap();
        cfg.peers[1].name = "node-a".to_string();
        let f = write_binpb(&encode_config(&cfg));
        match load(f.path()) {
            Err(ConfigError::DuplicatePeerName(name)) if name == "node-a" => {}
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
        let s = cfg.config().startup();
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
        assert!(cfg.config().startup().memory().no_hugepages);
        assert_eq!(
            cfg.config().startup().memory().memory_total_bytes,
            Some(64 * 1024 * 1024)
        );
        assert_eq!(
            cfg.config().startup().fabric().default_listen_addr(),
            Some("10.0.0.1:7000")
        );
        assert!(cfg.config().startup().topology().disable_rdma);
        // Unset siblings still default.
        assert_eq!(cfg.config().startup().fabric().progress_threads, Some(2));
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
            loaded.config().startup().fabric().default_listen_addr(),
            Some("10.0.0.2:8000")
        );
        assert_eq!(loaded.config().startup().fabric().max_inflight, Some(4096));
        assert!(loaded.config().startup().topology().disable_rdma);
        assert_eq!(
            loaded.config().startup().memory().memory_total_bytes,
            Some(128 * 1024 * 1024)
        );
    }

    fn tls_tcp_toml(bind_overrides: &str, peer_overrides: &str) -> String {
        format!(
            r#"
self = "node-a"

[[peers]]
name = "node-a"

[peers.config.tls_tcp]
addr = "127.0.0.1:9443"
{peer_overrides}

[startup.fabric.binds.tls_tcp]
addr = "0.0.0.0:9443"
ca_cert_path = "/etc/unbounded-storage/ca.pem"
cert_path = "/etc/unbounded-storage/node.pem"
key_path = "/etc/unbounded-storage/node-key.pem"
{bind_overrides}
"#
        )
    }

    #[test]
    fn loads_valid_tls_tcp_config() {
        let f = write_cfg(&tls_tcp_toml(
            "lanes = 8\nrequest_timeout_ms = 15000\nsocket_buffer_bytes = 8388608\nring_depth = 2048",
            "server_name = \"node-a\"",
        ));
        let cfg = load(f.path()).expect("valid tls_tcp config should load");

        let binds = match cfg.startup().fabric().binds.as_ref() {
            Some(super::super::schema::fabric_cfg::Binds::TlsTcp(binds)) => binds,
            other => panic!("expected tls_tcp binds, got {other:?}"),
        };
        assert_eq!(binds.lanes, Some(8));
        assert_eq!(binds.request_timeout_ms, Some(15_000));
        assert_eq!(binds.socket_buffer_bytes, Some(8 * 1024 * 1024));
        assert_eq!(binds.ring_depth, Some(2048));
    }

    #[test]
    fn tls_tcp_config_round_trips_through_binpb() {
        let mut cfg: Config = toml::from_str(&tls_tcp_toml("", "")).unwrap();
        cfg.apply_defaults();
        let f = write_binpb(&encode_config(&cfg));
        let loaded = load(f.path()).expect("tls_tcp protobuf config should load");

        assert_eq!(loaded.peers[0].transport_name(), "tls_tcp");
        let peer = match loaded.peers[0].config.as_ref() {
            Some(peer_spec::Config::TlsTcp(peer)) => peer,
            other => panic!("expected tls_tcp peer, got {other:?}"),
        };
        assert_eq!(peer.addr, "127.0.0.1:9443");
        let binds = match loaded.startup().fabric().binds.as_ref() {
            Some(super::super::schema::fabric_cfg::Binds::TlsTcp(binds)) => binds,
            other => panic!("expected tls_tcp binds, got {other:?}"),
        };
        assert_eq!(binds.lanes, Some(4));
        assert_eq!(binds.request_timeout_ms, Some(30_000));
        assert_eq!(binds.socket_buffer_bytes, Some(16 * 1024 * 1024));
        assert_eq!(binds.ring_depth, Some(4096));
    }

    #[test]
    fn rejects_invalid_tls_tcp_addresses_and_zero_bind_port() {
        for (from, to, expected) in [
            (
                "addr = \"0.0.0.0:9443\"",
                "addr = \"example.com:9443\"",
                "tls_tcp bind addr",
            ),
            (
                "addr = \"0.0.0.0:9443\"",
                "addr = \"0.0.0.0:0\"",
                "nonzero port",
            ),
            (
                "addr = \"127.0.0.1:9443\"",
                "addr = \"example.com:9443\"",
                "invalid tcp socket",
            ),
        ] {
            let config = tls_tcp_toml("", "").replace(from, to);
            let f = write_cfg(&config);
            let error = load(f.path()).expect_err("invalid tls_tcp address must fail");
            assert!(error.to_string().contains(expected), "{error}");
        }
    }

    #[test]
    fn rejects_zero_tls_tcp_peer_ports() {
        let config = tls_tcp_toml("", "")
            .replace("addr = \"127.0.0.1:9443\"", "addr = \"127.0.0.1:0\"");
        let f = write_cfg(&config);

        assert!(matches!(
            load(f.path()),
            Err(ConfigError::InvalidTcpAddr { peer_name, addr })
                if peer_name == "node-a" && addr == "127.0.0.1:0"
        ));
    }

    #[test]
    fn rejects_empty_tls_tcp_certificate_paths() {
        for (field, line) in [
            (
                "ca_cert_path",
                "ca_cert_path = \"/etc/unbounded-storage/ca.pem\"",
            ),
            (
                "cert_path",
                "cert_path = \"/etc/unbounded-storage/node.pem\"",
            ),
            (
                "key_path",
                "key_path = \"/etc/unbounded-storage/node-key.pem\"",
            ),
        ] {
            let config = tls_tcp_toml("", "").replace(line, &format!("{field} = \"\""));
            let f = write_cfg(&config);
            assert!(
                matches!(load(f.path()), Err(ConfigError::EmptyTlsTcpCertPath(found)) if found == field),
                "expected empty {field} to fail"
            );
        }
    }

    #[test]
    fn rejects_zero_tls_tcp_lanes_and_ring_depth() {
        for (overrides, expected) in [
            ("lanes = 0", ConfigError::ZeroTlsTcpLanes),
            ("ring_depth = 0", ConfigError::ZeroTlsTcpRingDepth),
        ] {
            let f = write_cfg(&tls_tcp_toml(overrides, ""));
            let error = load(f.path()).expect_err("zero tls_tcp sizing must fail");
            assert_eq!(
                std::mem::discriminant(&error),
                std::mem::discriminant(&expected)
            );
        }
    }

    #[test]
    fn rejects_tls_tcp_peer_transport_and_server_name_mismatches() {
        let plaintext_peer = tls_tcp_toml("", "").replace(
            "[peers.config.tls_tcp]\naddr = \"127.0.0.1:9443\"",
            "[peers.config.tcp]\naddr = \"127.0.0.1:9443\"",
        );
        let f = write_cfg(&plaintext_peer);
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::TlsTcpPeerTransportMismatch(name)) if name == "node-a"
        ));

        let f = write_cfg(&tls_tcp_toml("", "server_name = \"node-b\""));
        assert!(matches!(
            load(f.path()),
            Err(ConfigError::TlsTcpServerNameMismatch { peer_name, server_name })
                if peer_name == "node-a" && server_name == "node-b"
        ));
    }

    #[test]
    fn legacy_tcp_and_rdma_mesh_still_loads() {
        let f = write_cfg(
            r#"
self = "node-tcp"

[[peers]]
name = "node-tcp"
[peers.config.tcp]
addr = "127.0.0.1:9000"

[[peers]]
name = "node-rdma"
[peers.config.rdma]
addr = "hex:01020304"
"#,
        );
        let cfg = load(f.path()).expect("legacy peer transports should still load");
        assert_eq!(cfg.peers[0].transport_name(), "tcp");
        assert_eq!(cfg.peers[1].transport_name(), "rdma");
    }
}
