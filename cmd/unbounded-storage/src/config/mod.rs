// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Typed, validated TOML configuration for the unbounded-storage daemon.

pub mod apply;
pub mod control;
pub mod diff;
pub mod graph;
mod load;
pub mod reconcile;
pub mod schema;
pub mod watch;

pub use apply::peer_spec_to_connection;
pub use control::{
    ApplyError, ApplyOutcome, ApplyTier, ConfigApplyTarget, ConfigController,
    ConfigVersionSnapshot, ConfigVersionStatus, ShardAck, ShardApply, ShardCommand,
    ShardControlGroup,
};
pub use diff::ConfigDiff;
pub use graph::{
    ResolvedFrontendBinding, RuntimeCache, RuntimeGraph, RuntimeMesh, RuntimePeer,
    frontend_backend_map, runtime_disks, runtime_peers, runtime_projection, validate_binding_graph,
};
pub use load::{ConfigError, LoadedConfig};
pub use reconcile::{
    ApplyReport, BackendReconcileReport, BackendReconcileTarget, FrontendReconcileReport,
    FrontendReconcileTarget, PeerReconcileTarget, ReconcileReport, SpecReconcileReport,
    apply_peers_startup, reconcile_backends, reconcile_frontends, reconcile_peers,
};
pub use schema::{
    AutoRdmaFabricBinds, AzureBackendConfig, BackendSpec, BlockDiskConfig, CacheSpec, Config,
    DiskDiscoveryCfg, DiskSpec, FabricCfg, FakeBackendConfig, FileDiskConfig, FrontendSpec,
    HttpBackendConfig, HttpFrontendConfig, LoadgenFrontendConfig, MemoryCfg, PeerSpec,
    RdmaPeerConfig, RoutingPlan, S3BackendConfig, S3FrontendConfig, StartupCfg, TcpFabricBinds,
    TcpPeerConfig, TopologyCfg, TopologyPrefixWeight, TopologyWeighting, backend_spec, disk_spec,
    fabric_cfg, frontend_spec, peer_spec,
};
pub use watch::{ConfigUpdate, ConfigWatcher, WatchError};
