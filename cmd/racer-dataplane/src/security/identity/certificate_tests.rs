use super::tests::{CLUSTER, NODE, issued, issued_with};
use super::*;

#[test]
fn cache_requires_exact_chain_current_trust_time_and_fresh_signature() {
    let clock = crate::runtime::environment::SimulationClock::new(93);
    let _environment = clock.environment(0).enter();
    let (pending, chain, roots) = issued();
    let keys = Rc::new(Keyring::new(
        ClusterId(CLUSTER.into()),
        NodeId(NODE.into()),
        Arc::new(KeyEpochs::default()),
    ));
    let bundle = |generation, roots| crate::control::wire::KeyringBundle {
        schema_version: 1,
        cluster: ClusterId(CLUSTER.into()),
        generation: crate::control::wire::BundleGeneration(generation),
        peer_trust_roots: roots,
        cache_keys: vec![],
    };
    keys.install(bundle(1, roots.clone())).unwrap();
    let identity = pending
        .accept(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            chain.clone(),
            &roots,
        )
        .unwrap();
    let certs = Certificates::new(ClusterId(CLUSTER.into()), keys.clone());
    let node = NodeId(NODE.into());
    let signature = identity.sign(b"message").unwrap();
    certs
        .verify_signed(&chain, &node, b"message", &signature)
        .unwrap();
    certs
        .verify_signed(&chain, &node, b"message", &signature)
        .unwrap();
    assert!(
        certs
            .verify_signed(&chain, &node, b"tampered", &signature)
            .is_err()
    );
    let mut changed = chain.clone();
    changed[0].push(0);
    assert!(certs.verify(&changed, &node).is_err());
    // Exercise expiration through the clock, not the cache implementation.
    let original = crate::runtime::environment::wall_now();
    let (_, until) = validity(chain.iter().chain(roots.iter())).unwrap();
    clock.set_wall_time(
        std::time::SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(until + 1),
    );
    assert!(certs.verify(&chain, &node).is_err());
    clock.set_wall_time(original);
    certs.verify(&chain, &node).unwrap();
    let (_, _, replacement) = issued();
    keys.install(bundle(2, replacement)).unwrap();
    assert!(certs.verify(&chain, &node).is_err());
}
#[test]
fn validates_chain_node_and_strict_signature() {
    let (pending, chain, roots) = issued();
    let cluster = ClusterId(CLUSTER.into());
    let node = NodeId(NODE.into());
    let key = verify_chain(&roots, &chain, &cluster, &node).unwrap();
    let identity = pending
        .accept(cluster.clone(), node.clone(), chain.clone(), &roots)
        .unwrap();
    let signature = Signature::from_slice(&identity.sign(b"message").unwrap()).unwrap();
    assert!(key.verify_strict(b"message", &signature).is_ok());
    assert!(key.verify_strict(b"changed", &signature).is_err());
    assert!(verify_chain(&roots, &chain, &cluster, &NodeId("other".into())).is_err());
    assert!(verify_chain(&roots, &chain, &ClusterId("other".into()), &node).is_err());
    let (_, _, foreign_roots) = issued();
    assert!(verify_chain(&foreign_roots, &chain, &cluster, &node).is_err());
    let mut bad = chain.clone();
    bad[0].push(0);
    assert!(verify_chain(&roots, &bad, &cluster, &node).is_err());
    assert!(verify_chain(&roots, &vec![chain[0].clone(); 9], &cluster, &node).is_err());
}
#[test]
fn rejects_missing_usage_ca_expiration_and_ambiguous_identity() {
    for case in 0..8 {
        let (_, chain, roots) = issued_with(|params| match case {
            0 => params.key_usages.clear(),
            1 => params.key_usages = vec![rcgen::KeyUsagePurpose::KeyEncipherment],
            2 => params.extended_key_usages.clear(),
            3 => params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth],
            4 => params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained),
            5 => {
                params.not_before = rcgen::date_time_ymd(2000, 1, 1);
                params.not_after = rcgen::date_time_ymd(2001, 1, 1);
            }
            6 => params
                .subject_alt_names
                .push(params.subject_alt_names[0].clone()),
            _ => {
                params.subject_alt_names = vec![rcgen::SanType::URI(
                    format!("spiffe://{CLUSTER}/node/{NODE}/extra")
                        .try_into()
                        .unwrap(),
                )]
            }
        });
        assert!(
            verify_chain(
                &roots,
                &chain,
                &ClusterId(CLUSTER.into()),
                &NodeId(NODE.into())
            )
            .is_err(),
            "case {case}"
        );
    }
}
#[test]
fn canonical_identity_rejects_aliases() {
    assert!(canonical_uuid(CLUSTER));
    for value in [
        "abc",
        "AAAAAAAA-1111-4111-8111-111111111111",
        "11111111111141118111111111111111",
        "11111111-1111-4111-8111-11111111111/",
    ] {
        assert!(!canonical_uuid(value));
    }
}
