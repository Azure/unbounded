// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Pure-function adapters from config types to the daemon's internal
//! types. Kept separate so the schema crate never has to depend on the
//! daemon's runtime types and vice versa.

use crate::fabric::ConnectionSpec;
use crate::fabric::PeerId;
use crate::memory::BackingKind;
use crate::topology;

use super::schema::{BackingKindCfg, PeerSpec, TopologyCfg};

pub fn peer_spec_to_connection(p: &PeerSpec) -> ConnectionSpec {
    ConnectionSpec {
        peer: PeerId(p.id),
        wire_addr: p.address.clone(),
        hca_numa: p.hca_numa,
        labels: p.labels.clone(),
    }
}

pub fn topology_cfg_to_plan_config(t: &TopologyCfg) -> topology::PlanConfig {
    let defaults = topology::PlanConfig::default();
    topology::PlanConfig {
        rdma_progress_per_hca: t.rdma_progress_per_hca,
        rdma_handlers_per_hca: t.rdma_handlers_per_hca,
        // The production daemon no longer schedules NVMe progress
        // threads via the topology plan: per-disk supervision lives
        // in `storage::disks::DiskRegistry`. The plan still emits
        // NVMe workers because the bench binary consumes them, so
        // we keep the default `nvme_threads_per_drive` here and do
        // not expose it as a knob in the daemon config.
        nvme_threads_per_drive: defaults.nvme_threads_per_drive,
        // Network shards are not exposed as a daemon config knob yet;
        // keep the default (0) so the production plan is unchanged.
        network_shards_per_nic: defaults.network_shards_per_nic,
        use_smt_siblings: t.use_smt_siblings,
        respect_isolated: t.respect_isolated,
        exclude_node_cpu0: t.exclude_node_cpu0,
        require_node_type_ca: defaults.require_node_type_ca,
        require_active_port: t.require_active_port,
        tcp_fallback_threads: t.tcp_fallback_threads,
        disable_rdma: t.disable_rdma,
    }
}

pub fn backing_kind_from_cfg(k: BackingKindCfg) -> BackingKind {
    match k {
        BackingKindCfg::Hugepage2Mb => BackingKind::Hugepage2Mb,
        BackingKindCfg::Heap => BackingKind::Heap,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use crate::config::schema::PeerTransport;

    #[test]
    fn peer_spec_maps_directly() {
        let p = PeerSpec {
            id: 42,
            transport: PeerTransport::Tcp,
            address: "10.0.0.1:9000".into(),
            hca_numa: Some(1),
            labels: vec!["us-west".to_string(), "rack7".to_string()],
        };
        let c = peer_spec_to_connection(&p);
        assert_eq!(c.peer, PeerId(42));
        assert_eq!(c.wire_addr, "10.0.0.1:9000");
        assert_eq!(c.hca_numa, Some(1));
        assert_eq!(c.labels, p.labels);
    }

    #[test]
    fn topology_cfg_maps_field_by_field() {
        let mut t = TopologyCfg::default();
        t.rdma_progress_per_hca = 3;
        t.rdma_handlers_per_hca = 7;
        t.use_smt_siblings = true;
        t.respect_isolated = false;
        t.exclude_node_cpu0 = false;
        t.require_active_port = false;
        t.tcp_fallback_threads = 4;

        let p = topology_cfg_to_plan_config(&t);
        let defaults = topology::PlanConfig::default();
        assert_eq!(p.rdma_progress_per_hca, 3);
        assert_eq!(p.rdma_handlers_per_hca, 7);
        // `nvme_threads_per_drive` is no longer config-driven; it
        // stays at the `PlanConfig` default so the bench binary
        // (the only remaining consumer of NVMe placements) keeps
        // working unchanged.
        assert_eq!(p.nvme_threads_per_drive, defaults.nvme_threads_per_drive);
        assert!(p.use_smt_siblings);
        assert!(!p.respect_isolated);
        assert!(!p.exclude_node_cpu0);
        assert!(!p.require_active_port);
        assert_eq!(p.tcp_fallback_threads, 4);
        assert_eq!(p.require_node_type_ca, defaults.require_node_type_ca);
    }

    #[test]
    fn backing_kind_maps() {
        assert!(matches!(
            backing_kind_from_cfg(BackingKindCfg::Hugepage2Mb),
            BackingKind::Hugepage2Mb
        ));
        assert!(matches!(
            backing_kind_from_cfg(BackingKindCfg::Heap),
            BackingKind::Heap
        ));
    }
}
