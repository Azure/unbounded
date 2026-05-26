// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Typed, validated TOML configuration for the unbounded-storage daemon.

pub mod schema;
mod load;

pub use load::ConfigError;
pub use schema::{
    BackingKindCfg, Config, DiskKind, DiskSpec, FabricCfg, PeerSpec, PeerTransport, StorageCfg,
    TopologyCfg,
};
