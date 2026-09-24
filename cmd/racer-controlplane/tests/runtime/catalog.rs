// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::model::{Member, ProductPlacement, Volume};
use crate::security::{Authority, CaState, StateImage};
use serde_json::{Value, json};

fn generation(revision: u64) -> Generation {
    let mut generation = Generation::empty("catalog-test");
    generation.revision = revision;
    for n in 0..3 {
        generation.nodes.insert(
            format!("node-{n}"),
            Member {
                id: format!("{n:064x}"),
                ip: Some(format!("10.0.0.{}", n + 1).parse().unwrap()),
                pod_uid: format!("pod-{revision}-{n}"),
                pod_namespace: "system".into(),
                pod_name: format!("dataplane-{n}"),
                fabric: "rack".into(),
            },
        );
    }
    generation.product = Some(ProductPlacement {
        left_factor: 1,
        right_factor: 3,
        members: generation.nodes.values().map(|m| m.id.clone()).collect(),
        roles: if revision == 1 {
            vec![0, 1, 2]
        } else {
            vec![2, 0, 1]
        },
        candidate_width: 1,
        candidates: vec![0],
    });
    generation.volumes.push(Volume {
        id: format!("volume-{revision}"),
        name: "cache".into(),
        resource_generation: 1,
        cache_socket: "/dev/racer/cache/cache".into(),
        origin_socket: "/dev/racer/cache/origin".into(),
        slots: 1,
        cache_generation: 1,
        routing_algorithm: crate::model::PRODUCT_ROUTING_ALGORITHM,
        max_candidate_attempts: 1,
        owners: vec!["node-0".into()],
    });
    generation
}

fn security(fence: &str) -> SecurityContext {
    let ca = Authority::generate(unix_now(), 86400, 60).unwrap();
    let metadata = serde_json::to_vec(&json!({
        "version": 1, "namespace": "system", "fence": fence, "generation": 1,
        "active": ca.digest, "phase": "stable", "authorities": [ca],
        "rotation_nonce": "", "published_at": null, "overlap_delay": 60,
        "retirement_skew": 60
    }))
    .unwrap();
    SecurityContext {
        fence: fence.into(),
        state: Arc::new(CaState::from_image(&StateImage { metadata }).unwrap()),
    }
}

fn fixture() -> (
    Arc<Subscriptions>,
    Arc<RwLock<Option<SecurityContext>>>,
    String,
) {
    let context = Arc::new(RwLock::new(Some(security("term"))));
    let source = context.clone();
    let subscriptions = Subscriptions::new(
        Arc::new(move || source.read().unwrap().clone()),
        1 << 20,
        Duration::from_secs(1),
    );
    subscriptions.set_fence(Some("term".into()));
    subscriptions.authority_until(Instant::now() + Duration::from_secs(60));
    subscriptions.install(Arc::new(generation(1))).unwrap();
    (subscriptions, context, identity("universe", "catalog-test"))
}

fn verify(body: &[u8], revision: u64) -> Value {
    let response: Value = serde_json::from_slice(body).unwrap();
    assert_eq!(response.as_object().unwrap().len(), 2);
    let catalog = &response["catalog"];
    assert_eq!(catalog["revision"], revision);
    assert_eq!(catalog["schema"], 1);
    assert_eq!(catalog["routingAlgorithm"], 1);
    assert_eq!(catalog["leftFactor"], 1);
    assert_eq!(catalog["rightFactor"], 3);
    assert_eq!(catalog["members"].as_array().unwrap().len(), 3);
    assert_eq!(
        catalog["roles"],
        if revision == 1 {
            json!([0, 1, 2])
        } else {
            json!([2, 0, 1])
        }
    );
    assert_eq!(
        catalog["selectedPodUIDs"],
        json!(
            (0..3)
                .map(|n| format!("pod-{revision}-{n}"))
                .collect::<Vec<_>>()
        )
    );
    assert_eq!(catalog["volumes"], json!([format!("volume-{revision}")]));
    let fields: Vec<_> = catalog
        .as_object()
        .unwrap()
        .keys()
        .map(String::as_str)
        .collect();
    assert_eq!(
        fields,
        [
            "leftFactor",
            "members",
            "revision",
            "rightFactor",
            "roles",
            "routingAlgorithm",
            "schema",
            "selectedPodUIDs",
            "universe",
            "volumes"
        ]
    );
    // Verify the actual wire bytes, not serde_json::Value's sorted key order.
    let start = body
        .windows(11)
        .position(|w| w == b"\"catalog\":{")
        .unwrap()
        + 10;
    assert_eq!(
        response["digest"],
        hex::encode(Sha256::digest(&body[start..body.len() - 1]))
    );
    response
}

#[tokio::test]
async fn product_catalog_is_coherent_deterministic_and_observation_free() {
    let (state, _, universe) = fixture();
    let node = format!("{:064x}", 0);
    let cached = state.snapshot(&universe, &node, "pod-1-0").await.unwrap().0;
    let mut headers = HeaderMap::new();
    headers.insert("x-racer-applied-revision", "1".parse().unwrap());
    headers.insert("x-racer-worker-healthy", "1".parse().unwrap());
    let report = Observation::observe(None, "pod-1-0", "boot", &headers, Instant::now());
    state
        .reports
        .lock()
        .unwrap()
        .insert((universe.clone(), node.clone()), report.clone());
    let publication = state.universes.read().unwrap()[&universe].clone();
    let watch = publication.changes.subscribe();
    let cache_before = state.cache_usage();
    let digest_before = state.expected_digest(&universe, &node);
    let waiters = state.waiter_count();
    let body = state.product_catalog(&universe).unwrap();
    verify(&body, 1);
    assert_eq!(state.product_catalog(&universe).unwrap(), body);
    assert_eq!(state.cache_usage(), cache_before);
    assert_eq!(state.expected_digest(&universe, &node), digest_before);
    assert_eq!(state.waiter_count(), waiters);
    assert!(!watch.has_changed().unwrap());
    assert_eq!(state.response_count(), 0);
    assert_eq!(state.builders.available_permits(), 4);
    let reports = state.reports.lock().unwrap();
    assert_eq!(reports.len(), 1);
    assert_eq!(
        format!("{:?}", reports[&(universe.clone(), node.clone())]),
        format!("{report:?}")
    );
    drop(reports);
    assert!(Arc::ptr_eq(
        &cached,
        &state.snapshot(&universe, &node, "pod-1-0").await.unwrap().0
    ));

    state.install(Arc::new(generation(2))).unwrap();
    // A pinned publication remains internally coherent after replacement.
    assert_eq!(encode_catalog(&universe, &publication).unwrap(), body);
    let next = state.product_catalog(&universe).unwrap();
    let next_value = verify(&next, 2);
    assert_ne!(next_value["digest"], verify(&body, 1)["digest"]);
}

#[test]
fn product_catalog_fails_closed_without_current_authority() {
    let (state, context, universe) = fixture();
    assert_eq!(
        state.product_catalog("invalid"),
        Err(StatusCode::BAD_REQUEST)
    );
    assert_eq!(
        state.product_catalog(&"f".repeat(64)),
        Err(StatusCode::NOT_FOUND)
    );
    let active = context.write().unwrap().take().unwrap();
    assert_eq!(
        state.product_catalog(&universe),
        Err(StatusCode::SERVICE_UNAVAILABLE)
    );
    *context.write().unwrap() = Some(active.clone());
    state.set_fence(Some("different".into()));
    assert_eq!(
        state.product_catalog(&universe),
        Err(StatusCode::SERVICE_UNAVAILABLE)
    );
    state.set_fence(Some("term".into()));
    context.write().unwrap().as_mut().unwrap().fence = "different".into();
    state.set_fence(Some("different".into()));
    assert_eq!(
        state.product_catalog(&universe),
        Err(StatusCode::SERVICE_UNAVAILABLE)
    );
    *context.write().unwrap() = Some(active);
    state.set_fence(Some("term".into()));
    state.authority_until(Instant::now());
    assert_eq!(
        state.product_catalog(&universe),
        Err(StatusCode::SERVICE_UNAVAILABLE)
    );
    *state.authority_deadline.write().unwrap() = None;
    assert_eq!(
        state.product_catalog(&universe),
        Err(StatusCode::SERVICE_UNAVAILABLE)
    );
}

#[test]
fn product_catalog_rechecks_authority_and_publication_after_encoding() {
    use std::sync::atomic::{AtomicUsize, Ordering};
    for replace in [false, true] {
        let active = security("term");
        let calls = Arc::new(AtomicUsize::new(0));
        let current = Arc::new(Mutex::new(std::sync::Weak::<Subscriptions>::new()));
        let target = current.clone();
        let count = calls.clone();
        let state = Subscriptions::new(
            Arc::new(move || {
                if count.fetch_add(1, Ordering::SeqCst) == 1 {
                    if !replace {
                        return None;
                    }
                    target
                        .lock()
                        .unwrap()
                        .upgrade()
                        .unwrap()
                        .install(Arc::new(generation(2)))
                        .unwrap();
                }
                Some(active.clone())
            }),
            1 << 20,
            Duration::from_secs(1),
        );
        *current.lock().unwrap() = Arc::downgrade(&state);
        state.set_fence(Some("term".into()));
        state.authority_until(Instant::now() + Duration::from_secs(60));
        state.install(Arc::new(generation(1))).unwrap();
        assert_eq!(
            state.product_catalog(&identity("universe", "catalog-test")),
            Err(StatusCode::SERVICE_UNAVAILABLE)
        );
    }
}
