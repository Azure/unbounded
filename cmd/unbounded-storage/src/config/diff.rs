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
//! * **process mesh (`local_node_id`, `local_tags`, `fingers_per_node`,
//!   `routing_plan`).** Rebuilds the projected routing surface and republishes
//!   it to every shard.
//! * **`[[peers]]`.** Reconciles fabric connections and rebuilds the projected
//!   routing surface.
//! * **`[[caches]]`.** Cache names are route ids, so cache changes reload the
//!   projected routing surface and reconcile disks.
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
    /// `[[caches]]` changed. Reconciled in place against the disk registry;
    /// may also alter which cache route ids exist.
    pub caches_changed: bool,
    /// `[[disks]]` changed. Reconciled in place against the disk registry.
    pub disks_changed: bool,
    /// Process-wide routing knobs changed. Rebuilds the routing surface.
    pub routing_changed: bool,
    /// `[[peers]]` changed. Reconciles fabric connections and rebuilds the
    /// routing surface.
    pub peers_changed: bool,
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
            caches_changed: old.caches != new.caches,
            disks_changed: old.disks != new.disks,
            routing_changed: old.local_node_id != new.local_node_id
                || old.local_tags != new.local_tags
                || old.fingers_per_node != new.fingers_per_node
                || old.routing_plan != new.routing_plan,
            peers_changed: old.peers != new.peers,
            backends_changed: old.backends != new.backends,
            frontends_changed: old.frontends != new.frontends,
        }
    }

    /// Whether anything changed at all. A no-op apply (`!any()`) can be
    /// acknowledged immediately without touching the shards.
    pub fn any(&self) -> bool {
        self.caches_changed
            || self.disks_changed
            || self.routing_changed
            || self.peers_changed
            || self.backends_changed
            || self.frontends_changed
    }

    /// Whether the routing surface must be rebuilt and republished.
    /// Mesh and peer changes alter the projected route table directly.
    /// Cache diffs are intentionally conservative: disk-only changes do not
    /// affect routes, but cache names are route ids, so the whole cache
    /// section reloads the projected routing surface for now.
    pub fn requires_routing_reload(&self) -> bool {
        self.routing_changed || self.peers_changed || self.caches_changed
    }

    /// Whether fabric peer connections must be reconciled.
    pub fn requires_peer_reconcile(&self) -> bool {
        self.peers_changed
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::schema::{
        BackendSpec, BlockDiskConfig, CacheSpec, DiskSpec, FrontendSpec, HttpBackendConfig,
        HttpFrontendConfig, PeerSpec, RoutingPlan, TcpPeerConfig, backend_spec, disk_spec,
        frontend_spec, peer_spec,
    };

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
    fn peer_change_is_routing_reload_and_peer_reconcile() {
        let a = base();
        let mut b = base();
        b.peers.push(PeerSpec {
            id: 2,
            tags: vec![],
            config: Some(peer_spec::Config::Tcp(TcpPeerConfig {
                addr: "127.0.0.1:9000".to_string(),
            })),
        });
        let d = ConfigDiff::between(&a, &b);
        assert!(d.peers_changed);
        assert!(d.requires_routing_reload());
        assert!(d.requires_peer_reconcile());
    }

    #[test]
    fn routing_only_change_is_routing_reload() {
        let a = base();
        let mut b = base();
        b.fingers_per_node = Some(64);
        b.routing_plan = Some(RoutingPlan {
            fingers: vec![2],
            successor: Some(2),
            predecessor: None,
        });
        let d = ConfigDiff::between(&a, &b);
        assert!(d.routing_changed);
        assert!(d.requires_routing_reload());
        assert!(!d.requires_peer_reconcile());
    }

    #[test]
    fn cache_change_is_routing_reload() {
        let a = base();
        let mut b = base();
        b.caches.push(CacheSpec {
            name: "c".to_string(),
            source: "n".to_string(),
        });
        let d = ConfigDiff::between(&a, &b);
        assert!(d.caches_changed);
        assert!(d.any());
        assert!(d.requires_routing_reload());
        assert!(!d.requires_peer_reconcile());
    }

    #[test]
    fn disk_change_is_detected_without_routing_reload() {
        let a = base();
        let mut b = base();
        b.disks.push(DiskSpec {
            queue_depth: None,
            page_size_bytes: None,
            skip_recovery_scan: false,
            config: Some(disk_spec::Config::Block(BlockDiskConfig {
                numa: None,
                path: "/dev/nvme0n1".to_string(),
            })),
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
            name: "b".to_string(),
            config: Some(backend_spec::Config::Http(HttpBackendConfig {
                url: "https://example.com".to_string(),
                stripe_size_bytes: Some(4 * 1024 * 1024),
                http_concurrency: Some(64),
                ca_cert_path: None,
                insecure_skip_verify: false,
            })),
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
            name: "f".to_string(),
            source: "b".to_string(),
            config: Some(frontend_spec::Config::Http(HttpFrontendConfig {
                addr: "0.0.0.0:9000".to_string(),
                max_requests_per_connection: None,
            })),
        });
        let d = ConfigDiff::between(&a, &b);
        assert!(d.frontends_changed);
        assert!(d.any());
        assert!(!d.requires_routing_reload());
    }
}
