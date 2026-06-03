// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Typed, validated TOML configuration for the unbounded-storage daemon.

pub mod apply;
mod load;
pub mod reconcile;
pub mod schema;
pub mod watch;

pub use apply::{backing_kind_from_cfg, peer_spec_to_connection, topology_cfg_to_plan_config};
pub use load::ConfigError;
pub use reconcile::{
    ApplyReport, BackendReconcileReport, BackendReconcileTarget, FrontendReconcileReport,
    FrontendReconcileTarget, PeerReconcileTarget, ReconcileReport, SpecReconcileReport,
    apply_peers_startup, reconcile_backends, reconcile_frontends, reconcile_peers,
};
pub use schema::{
    BackendKind, BackendSpec, BackingKindCfg, Config, DiskKind, DiskSpec, FabricCfg, FrontendKind,
    FrontendSpec, PeerSpec, PeerTransport, StorageCfg, TopologyCfg,
};
pub use watch::{ConfigUpdate, ConfigWatcher, WatchError};
