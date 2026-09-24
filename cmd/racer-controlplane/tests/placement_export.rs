// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Test-only wire fixtures compiled from inventory by the production compiler.
use prost::Message;
use racer_controlplane::{
    model::*,
    topology::{Topology, compile},
};

#[test]
fn export_dataplane_placement() {
    let Some(dir) = std::env::var_os("RACER_PLACEMENT_EXPORT") else {
        return;
    };
    let dir = std::path::PathBuf::from(dir);
    let mut files = Vec::new();
    for count in [2, 3, 7] {
        let input = Inventory {
            universe: "placement-conformance".into(),
            socket_root: SOCKET_ROOT.into(),
            nodes: (0..count)
                .map(|i| Node {
                    name: format!("node-{i:06}"),
                    uid: format!("uid-{i}"),
                    universe: "placement-conformance".into(),
                    eligible: true,
                    ready: true,
                    fabric: "rack".into(),
                    pods: vec![Pod {
                        uid: format!("pod-{i}"),
                        namespace: "racer".into(),
                        name: format!("racer-{i}"),
                        ip: if i % 2 == 0 {
                            format!("10.0.0.{}", i + 1)
                        } else {
                            format!("fd00::{:x}", i + 1)
                        },
                        available: true,
                        ready: true,
                        created_at: 1,
                    }],
                })
                .collect(),
            caches: vec![Cache {
                name: "cache-a".into(),
                uid: "cache-uid".into(),
                resource_generation: 1,
                cache_generation: 3,
                max_candidate_attempts: 3,
            }],
        };
        let mut generation = compile(&input, None).unwrap();
        generation.revision = 1;
        let persisted = generation.canonical_bytes();
        let restored = serde_json::from_slice(&persisted).unwrap();
        assert_eq!(compile(&input, Some(&restored)).unwrap(), generation);
        // Canonical product-v1 snapshots from the production compiler.
        let product_topology = Topology::new(&generation).unwrap();
        for (i, member) in generation.nodes.values().enumerate() {
            let snapshot = product_topology.snapshot(&member.id).unwrap();
            std::fs::write(
                dir.join(format!("product-n{count}-{i}.pb")),
                snapshot.encode_to_vec(),
            )
            .unwrap();
        }
        // Default production geometry plus cube/noncube boundaries exercise the
        // same snapshot serializer with deterministic universe/Node identities.
        for slots in [1, 8, 17, 64, SLOT_COUNT] {
            let mut g = generation.clone();
            g.volumes[0].slots = slots;
            g.volumes[0].owners.truncate(slots as usize);
            let product = g.product.as_mut().unwrap();
            product
                .candidates
                .truncate(slots as usize * product.candidate_width as usize);
            let top = Topology::new(&g).unwrap();
            let prefix = format!("p{slots}-n{count}-fresh");
            let ids: std::collections::BTreeMap<_, _> = g
                .nodes
                .iter()
                .map(|(name, member)| (name, &member.id))
                .collect();
            std::fs::write(
                dir.join(format!("{prefix}-ids.json")),
                serde_json::to_vec(&ids).unwrap(),
            )
            .unwrap();
            std::fs::write(
                dir.join(format!("{prefix}-owners.json")),
                serde_json::to_vec(&g.volumes[0].owners).unwrap(),
            )
            .unwrap();
            for (i, member) in g.nodes.values().enumerate() {
                let snapshot = top.snapshot(&member.id).unwrap();
                let file = format!("{prefix}-{i}.pb");
                std::fs::write(dir.join(&file), snapshot.encode_to_vec()).unwrap();
                files.push(file);
                if count == 2 && slots == SLOT_COUNT && i == 0 {
                    std::fs::write(dir.join("default.pb"), snapshot.encode_to_vec()).unwrap();
                }
            }
        }
    }
    std::fs::write(dir.join("files.json"), serde_json::to_vec(&files).unwrap()).unwrap();
}
