// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Typed, validated TOML configuration for the unbounded-storage daemon.

pub mod apply;
pub mod control;
pub mod diff;
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
pub use load::ConfigError;
pub use reconcile::{
    ApplyReport, BackendReconcileReport, BackendReconcileTarget, FrontendReconcileReport,
    FrontendReconcileTarget, PeerReconcileTarget, ReconcileReport, SpecReconcileReport,
    apply_peers_startup, reconcile_backends, reconcile_frontends, reconcile_peers,
};
pub use schema::{
    BackendKind, BackendSpec, Config, DiskKind, DiskSpec, FabricAddress, FabricCfg, FrontendKind,
    FrontendSpec, MemoryCfg, P2pCfg, PeerSpec, RoutingPlan, StartupCfg, TopologyCfg,
};
pub use watch::{ConfigUpdate, ConfigWatcher, WatchError};
