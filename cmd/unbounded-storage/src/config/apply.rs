// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Pure-function adapters from config types to the daemon's internal
//! types. Kept separate so the schema crate never has to depend on the
//! daemon's runtime types and vice versa.

use crate::backing::BackingKind;
use crate::bufferpool::PeerId;
use crate::fabric::ConnectionSpec;
use crate::topology;

use super::schema::{BackingKindCfg, DiskSpec, PeerSpec, TopologyCfg};

pub fn peer_spec_to_connection(p: &PeerSpec) -> ConnectionSpec {
    ConnectionSpec {
        peer: PeerId(p.id),
        wire_addr: p.address.clone(),
        hca_numa: p.hca_numa,
    }
}

pub fn disk_specs_to_nvmes(disks: &[DiskSpec]) -> Vec<topology::Nvme> {
    disks
        .iter()
        .map(|d| {
            let dev_name = match d.path.file_name() {
                Some(n) => n.to_string_lossy().into_owned(),
                None => d.path.to_string_lossy().into_owned(),
            };
            topology::Nvme {
                dev_name,
                pci_bdf: None,
                numa: d.numa,
            }
        })
        .collect()
}

pub fn topology_cfg_to_plan_config(t: &TopologyCfg) -> topology::PlanConfig {
    topology::PlanConfig {
        rdma_progress_per_hca: t.rdma_progress_per_hca,
        rdma_handlers_per_hca: t.rdma_handlers_per_hca,
        nvme_threads_per_drive: t.nvme_threads_per_drive,
        use_smt_siblings: t.use_smt_siblings,
        respect_isolated: t.respect_isolated,
        exclude_node_cpu0: t.exclude_node_cpu0,
        require_node_type_ca: topology::PlanConfig::default().require_node_type_ca,
        require_active_port: t.require_active_port,
        tcp_fallback_threads: t.tcp_fallback_threads,
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
    use std::path::PathBuf;

    use crate::config::schema::PeerTransport;

    #[test]
    fn peer_spec_maps_directly() {
        let p = PeerSpec {
            id: 42,
            transport: PeerTransport::Tcp,
            address: "10.0.0.1:9000".into(),
            hca_numa: Some(1),
        };
        let c = peer_spec_to_connection(&p);
        assert_eq!(c.peer, PeerId(42));
        assert_eq!(c.wire_addr, "10.0.0.1:9000");
        assert_eq!(c.hca_numa, Some(1));
    }

    #[test]
    fn disk_specs_derive_dev_name_from_file_name() {
        let disks = vec![
            DiskSpec {
                path: PathBuf::from("/dev/nvme0n1"),
                kind: super::super::schema::DiskKind::Nvme,
                numa: Some(0),
                queue_depth: None,
            },
            DiskSpec {
                path: PathBuf::from("nvme9n1"),
                kind: super::super::schema::DiskKind::Nvme,
                numa: None,
                queue_depth: None,
            },
        ];
        let nvmes = disk_specs_to_nvmes(&disks);
        assert_eq!(nvmes.len(), 2);
        assert_eq!(nvmes[0].dev_name, "nvme0n1");
        assert_eq!(nvmes[0].pci_bdf, None);
        assert_eq!(nvmes[0].numa, Some(0));
        assert_eq!(nvmes[1].dev_name, "nvme9n1");
        assert_eq!(nvmes[1].numa, None);
    }

    #[test]
    fn topology_cfg_maps_field_by_field() {
        let mut t = TopologyCfg::default();
        t.rdma_progress_per_hca = 3;
        t.rdma_handlers_per_hca = 7;
        t.nvme_threads_per_drive = 5;
        t.use_smt_siblings = true;
        t.respect_isolated = false;
        t.exclude_node_cpu0 = false;
        t.require_active_port = false;
        t.tcp_fallback_threads = 4;

        let p = topology_cfg_to_plan_config(&t);
        assert_eq!(p.rdma_progress_per_hca, 3);
        assert_eq!(p.rdma_handlers_per_hca, 7);
        assert_eq!(p.nvme_threads_per_drive, 5);
        assert!(p.use_smt_siblings);
        assert!(!p.respect_isolated);
        assert!(!p.exclude_node_cpu0);
        assert!(!p.require_active_port);
        assert_eq!(p.tcp_fallback_threads, 4);
        assert_eq!(
            p.require_node_type_ca,
            topology::PlanConfig::default().require_node_type_ca
        );
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
