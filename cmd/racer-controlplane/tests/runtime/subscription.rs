// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::model::{Inventory, Node, Pod};

#[tokio::test]
async fn two_universe_live_selection_preserves_and_restores_convergence_digests() {
    let state = Subscriptions::new(Arc::new(|| None), 1 << 20, Duration::from_secs(1));
    let mut live = BTreeMap::new();
    for universe in ["a", "b"] {
        let input = Inventory {
            universe: universe.into(),
            socket_root: "/run/racer".into(),
            caches: vec![],
            nodes: vec![Node {
                name: universe.into(),
                uid: universe.into(),
                universe: universe.into(),
                eligible: true,
                ready: true,
                fabric: String::new(),
                pods: vec![Pod {
                    uid: format!("pod-{universe}"),
                    namespace: "system".into(),
                    name: format!("pod-{universe}"),
                    ip: "10.0.0.1".into(),
                    available: true,
                    ready: true,
                    created_at: 0,
                }],
            }],
        };
        let mut generation = crate::topology::compile(&input, None).unwrap();
        generation.revision = 1;
        state.install(Arc::new(generation)).unwrap();
        let key = (identity("universe", universe), identity("node", universe));
        live.insert(key.clone(), state.selection(&key.0, &key.1).unwrap());
    }
    state.set_live_selections(live.clone());
    let a = (identity("universe", "a"), identity("node", "a"));
    let b = (identity("universe", "b"), identity("node", "b"));
    let (snapshot_a, _, _) = state.snapshot(&a.0, &a.1, "pod-a").await.unwrap();
    let (snapshot_b, _, _) = state.snapshot(&b.0, &b.1, "pod-b").await.unwrap();
    let expected_b = state.expected_digest(&b.0, &b.1).unwrap();
    let mut changed = live.clone();
    changed.remove(&a);
    state.set_live_selections(changed);
    assert!(state.expected_digest(&a.0, &a.1).is_none());
    assert_eq!(state.expected_digest(&b.0, &b.1), Some(expected_b.clone()));
    let (cached_b, _, _) = state.snapshot(&b.0, &b.1, "pod-b").await.unwrap();
    assert!(
        Arc::ptr_eq(&snapshot_b, &cached_b),
        "exercise payload cache hit"
    );
    assert_eq!(state.expected_digest(&b.0, &b.1), Some(expected_b));

    state.set_live_selections(live);
    assert!(state.expected_digest(&a.0, &a.1).is_none());
    let (cached_a, _, _) = state.snapshot(&a.0, &a.1, "pod-a").await.unwrap();
    assert!(Arc::ptr_eq(&snapshot_a, &cached_a));
    assert_eq!(
        state.expected_digest(&a.0, &a.1),
        Some((1, snapshot_a.digest.clone()))
    );
}
