// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Structural diff between two validated [`Config`] snapshots.
//!
//! Every section is reconciled in place on the live shard layer; a
//! config change never tears the shard layer down. The diff records
//! which sections changed so the
//! [`crate::config::control::ConfigController`]'s
//! [`ConfigApplyTarget`](crate::config::ConfigApplyTarget) can skip
//! untouched work:
//!
//! * **Routing (`[p2p]`, `[[peers]]`).** Rebuilds the finger table,
//!   republishes it to every shard, and reconciles fabric connections.
//! * **`[[disks]]`.** Reconciled in place against the disk registry.
//! * **`[[backends]]` / `[[frontends]]`.** Broadcast to every shard,
//!   which reconciles its own origin-backend and frontend registries on
//!   its own thread (binding/closing listeners and rebuilding backends
//!   without a shard restart).
//!
//! Startup-fixed knobs (the `[startup]` section: fabric listen address
//! and thread counts, fabric max in-flight, backing allocation, CPU
//! topology) live in the config file but are deliberately excluded from
//! the dynamic apply path: they only take effect on process start, so
//! they never appear in a diff. A change to them requires a restart and
//! is surfaced separately via the startup config version
//! ([`crate::config::ConfigVersionStatus::startup`]) rather than through
//! [`ConfigDiff`].
//!
//! [`ConfigDiff`] records which sections changed so the
//! [`crate::config::control::ConfigController`] can do the minimal
//! sufficient work. All section types are prost-generated and derive
//! [`PartialEq`], so each comparison is a structural value comparison.

use crate::config::schema::Config;

/// Per-section change flags between an old and a new [`Config`].
///
/// Each flag is `true` iff that section differs structurally between the
/// two configs. Construct with [`ConfigDiff::between`].
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct ConfigDiff {
    /// `[p2p]` section changed (local node id/labels, fingers-per-node).
    /// Rebuilds the routing surface.
    pub p2p_changed: bool,
    /// `[[peers]]` changed. Reconciles fabric connections and rebuilds
    /// the routing surface.
    pub peers_changed: bool,
    /// `[[disks]]` changed. Reconciled in place against the disk
    /// registry.
    pub disks_changed: bool,
    /// `[[backends]]` changed. Broadcast to every shard, which rebuilds
    /// its origin-backend registry in place (no shard restart).
    pub backends_changed: bool,
    /// `[[frontends]]` changed. Broadcast to every shard, which binds or
    /// closes the affected listener in place (no shard restart).
    pub frontends_changed: bool,
}

impl ConfigDiff {
    /// Compute the per-section diff between `old` and `new`.
    ///
    /// Both configs are expected to be post-`apply_defaults` (as every
    /// config produced by [`crate::config::load`] is), so promoted
    /// defaults compare equal and do not register as spurious changes.
    pub fn between(old: &Config, new: &Config) -> Self {
        Self {
            p2p_changed: old.p2p != new.p2p,
            peers_changed: old.peers != new.peers,
            disks_changed: old.disks != new.disks,
            backends_changed: old.backends != new.backends,
            frontends_changed: old.frontends != new.frontends,
        }
    }

    /// Whether anything changed at all. A no-op apply (`!any()`) can be
    /// acknowledged immediately without touching the shards.
    pub fn any(&self) -> bool {
        self.p2p_changed
            || self.peers_changed
            || self.disks_changed
            || self.backends_changed
            || self.frontends_changed
    }

    /// Whether the routing surface must be rebuilt and republished. True
    /// iff the `[p2p]` section or the peer set changed.
    pub fn requires_routing_reload(&self) -> bool {
        self.p2p_changed || self.peers_changed
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::schema::{BackendSpec, DiskSpec, FabricAddress, FrontendSpec, PeerSpec};

    fn base() -> Config {
        let mut c: Config = toml::from_str("").unwrap();
        c.apply_defaults();
        c
    }

    #[test]
    fn identical_configs_have_no_diff() {
        let a = base();
        let b = base();
        let d = ConfigDiff::between(&a, &b);
        assert!(!d.any());
        assert!(!d.requires_routing_reload());
    }

    #[test]
    fn peer_change_is_routing_reload() {
        let a = base();
        let mut b = base();
        b.p2p.as_mut().unwrap().local_node_id = Some(1);
        b.peers.push(PeerSpec {
            id: 2,
            address: Some(FabricAddress {
                socket: "127.0.0.1:9000".to_string(),
                native: String::new(),
            }),
            labels: vec![],
            hca_numa: None,
        });
        let d = ConfigDiff::between(&a, &b);
        assert!(d.peers_changed);
        assert!(d.p2p_changed);
        assert!(d.requires_routing_reload());
    }

    #[test]
    fn p2p_only_change_is_routing_reload() {
        let a = base();
        let mut b = base();
        b.p2p.as_mut().unwrap().fingers_per_node = 64;
        let d = ConfigDiff::between(&a, &b);
        assert!(d.p2p_changed);
        assert!(d.requires_routing_reload());
    }

    #[test]
    fn disk_change_is_not_routing_reload() {
        let a = base();
        let mut b = base();
        b.disks.push(DiskSpec {
            path: "/dev/nvme0n1".to_string(),
            kind: 0,
            size: None,
            numa: None,
            queue_depth: None,
            page_size_bytes: None,
            bypass_admission: false,
            skip_recovery_scan_if_no_meta: false,
        });
        let d = ConfigDiff::between(&a, &b);
        assert!(d.disks_changed);
        assert!(d.any());
        assert!(!d.requires_routing_reload());
    }

    #[test]
    fn backend_change_is_detected_without_routing_reload() {
        let a = base();
        let mut b = base();
        b.backends.push(BackendSpec {
            id: "b".to_string(),
            kind: 0,
            endpoint: "https://example.com".to_string(),
            stripe_size_bytes: 4 * 1024 * 1024,
            http_concurrency: 64,
            bucket: None,
        });
        let d = ConfigDiff::between(&a, &b);
        assert!(d.backends_changed);
        assert!(d.any());
        assert!(!d.requires_routing_reload());
    }

    #[test]
    fn frontend_change_is_detected_without_routing_reload() {
        let a = base();
        let mut b = base();
        b.frontends.push(FrontendSpec {
            id: "f".to_string(),
            kind: 0,
            bind: "0.0.0.0:9000".to_string(),
            backend: "b".to_string(),
        });
        let d = ConfigDiff::between(&a, &b);
        assert!(d.frontends_changed);
        assert!(d.any());
        assert!(!d.requires_routing_reload());
    }
}
