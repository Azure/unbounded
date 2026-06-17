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
    CompatRuntimeProjection, ResolvedFrontendBinding, RuntimeCache, RuntimeGraph,
    RuntimeNeighborhood, RuntimeP2p, RuntimePeer, compat_runtime_projection, frontend_backend_map,
    runtime_disks, runtime_p2p_or_default, runtime_peers, runtime_projection, scoped_peer_id,
    validate_binding_graph,
};
pub use load::ConfigError;
pub use reconcile::{
    ApplyReport, BackendReconcileReport, BackendReconcileTarget, FrontendReconcileReport,
    FrontendReconcileTarget, PeerReconcileTarget, ReconcileReport, SpecReconcileReport,
    apply_peers_startup, reconcile_backends, reconcile_frontends, reconcile_peers,
};
pub use schema::{
    AzureBackendConfig, BackendSpec, BlockDiskConfig, CacheSpec, Config, DiskSpec, FabricCfg,
    FakeBackendConfig, FileDiskConfig, FrontendSpec, HttpBackendConfig, HttpFrontendConfig,
    LoadgenFrontendConfig, MemoryCfg, NeighborhoodSpec, PeerSpec, RdmaPeerConfig, RoutingPlan,
    S3BackendConfig, S3FrontendConfig, StartupCfg, TcpPeerConfig, TopologyCfg, backend_spec,
    disk_spec, frontend_spec, peer_spec,
};
pub use watch::{ConfigUpdate, ConfigWatcher, WatchError};
