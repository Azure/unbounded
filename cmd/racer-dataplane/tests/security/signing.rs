// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
pub(crate) mod bundle_tests {
    use super::*;
    pub(crate) fn bundle(generation: u64, active: u8, trusted: &[u8], peer: bool) -> Vec<u8> {
        let hex = |bytes: &[u8]| bytes.iter().map(|b| format!("{b:02x}")).collect::<String>();
        let public = |seed| {
            hex(SigningKey::from_bytes(&[seed; 32])
                .verifying_key()
                .as_bytes())
        };
        let mut value = serde_json::json!({"version":1,"generation":generation,"active":public(active),"public":trusted.iter().map(|s|public(*s)).collect::<Vec<_>>()});
        if peer {
            value["seed"] = hex(&[active; 32]).into();
        }
        serde_json::to_vec(&value).unwrap()
    }

    #[test]
    fn projected_rotation_is_coherent_live_and_monotonic() {
        let root = std::env::temp_dir().join(format!("racer-bundle-{}", std::process::id()));
        std::fs::create_dir_all(root.join("old")).unwrap();
        std::fs::create_dir_all(root.join("new")).unwrap();
        std::fs::write(root.join("old/bundle.json"), bundle(1, 1, &[1], true)).unwrap();
        std::fs::write(root.join("new/bundle.json"), bundle(2, 2, &[1, 2], true)).unwrap();
        std::os::unix::fs::symlink("old", root.join("..data")).unwrap();
        let mut live = Keys::bundle(&root, true).unwrap().live();
        let retained_generation = live.clone();
        let pinned = live.pinned();
        let swap = |target| {
            std::os::unix::fs::symlink(target, root.join("..next")).unwrap();
            std::fs::rename(root.join("..next"), root.join("..data")).unwrap();
        };
        swap("new");
        assert!(live.reload_bundle(&root, true).unwrap());
        assert_eq!(retained_generation.generation(), 2);
        assert_ne!(
            pinned.signing_id().unwrap(),
            retained_generation.signing_id().unwrap()
        );
        let signature = pinned.sign(b"test", &[b"body"]).unwrap();
        retained_generation
            .verify(b"test", &[b"body"], &signature)
            .unwrap();
        swap("old");
        assert!(live.reload_bundle(&root, true).is_err());
        assert_eq!(live.generation(), 2);
        swap("new");
        std::fs::write(root.join("new/bundle.json"), bundle(2, 3, &[2, 3], true)).unwrap();
        assert!(live.reload_bundle(&root, true).is_err());
        std::fs::write(root.join("new/bundle.json"), bundle(3, 3, &[2, 3], true)).unwrap();
        live.reload_bundle(&root, true).unwrap();
        assert!(
            retained_generation
                .verify(b"test", &[b"body"], &signature)
                .is_err()
        );
        assert!(Keys::bundle(&root, false).is_err());
        std::fs::write(root.join("new/bundle.json"), b"invalid").unwrap();
        assert!(live.reload_bundle(&root, true).is_err());
        assert_eq!(live.generation(), 3);
        std::fs::remove_dir_all(root).unwrap();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::control::{
        proto,
        tests::{fixture, prepare_snapshot},
    };
    #[test]
    fn rdma_identity_limits_bound_negotiation_headers() {
        use crate::{crypto, http_client as http, peer_identity::*};
        use std::time::{Duration, Instant};
        for value in [
            "".to_owned(),
            "0x".to_owned() + &"ab".repeat(31),
            "ab".repeat(31),
            "é".repeat(32),
            "a ".repeat(32),
        ] {
            assert!(value.parse::<NodeId>().is_err());
        }
        assert!(NodeId::from_bytes(&[0; 31]).is_err());
        assert!(NodeId::from_bytes(&[0; 33]).is_err());
        let node = "AB".repeat(32).parse::<NodeId>().unwrap();
        assert_eq!(node.to_string(), "ab".repeat(32));
        assert_eq!(node.bytes(), [0xab; 32]);
        for value in [
            "",
            " rack",
            "rack ",
            "rack\t1",
            "rack\n1",
            "rack\01",
            "râck",
            "rack\u{7f}",
        ] {
            assert!(FabricId::new(value).is_err());
        }
        assert!(FabricId::new(&"f".repeat(MAX_FABRIC_LEN)).is_ok());
        assert!(FabricId::new(&"f".repeat(MAX_FABRIC_LEN + 1)).is_err());
        for (index, count) in [(0, 0), (1, 1), (256, 256), (0, 257), (u32::MAX, u32::MAX)] {
            assert!(RailId::new(index, count).is_err());
        }
        let rail = RailId::new(255, MAX_RAILS).unwrap();
        assert_eq!((rail.index(), rail.count()), (255, 256));
        assert!(RailId::new(0, 1).is_ok());
        let mut offer = vec![0u8; MAX_OFFER_LEN];
        offer[..4].copy_from_slice(&2u32.to_be_bytes());
        offer[4..20].fill(1);
        offer[52..56].copy_from_slice(&1u32.to_be_bytes());
        offer[62] = 1;
        offer[64] = 1;
        offer[70..74].copy_from_slice(&MAX_RAILS.to_be_bytes());
        offer[74..76].copy_from_slice(&(MAX_FABRIC_LEN as u16).to_be_bytes());
        offer[76..].fill(b'f');
        let offer = crate::rdma::Offer::decode(&offer).unwrap();
        assert_eq!(offer.encode().len(), MAX_OFFER_LEN);
        let (trust, snapshot) = crate::control::tests::rdma_fixture();
        let prepared = prepare_snapshot(&trust, snapshot);
        let (_, hello) = crypto::auth::Initiator::start(
            prepared.crypto_snapshot().clone(),
            crypto::auth::PeerContext::new(trust.node, node.bytes()).unwrap(),
            Some(&offer),
            Duration::from_secs(1),
        )
        .unwrap();
        assert_eq!(hello.encode().len() + 64, MAX_HANDSHAKE_LEN);
        let hex: String = hello.encode().iter().map(|b| format!("{b:02x}")).collect();
        let host = "h".repeat(MAX_AUTHORITY_LEN);
        let headers = [("X-Racer-Auth", hex.as_str())];
        let request = http::Request::new("/", &headers).unwrap();
        let connection = http::Connection::new("127.0.0.1:80".parse().unwrap(), &host).unwrap();
        assert!(
            connection
                .head(request, Instant::now() + Duration::from_secs(1))
                .is_ok()
        );
        assert!(2 * MAX_HANDSHAKE_LEN + MAX_AUTHORITY_LEN + 1024 < crate::http::SCRATCH_SIZE);
    }
    #[test]
    fn signed_configuration_binds_exact_plaintext_contents() {
        use prost::Message;
        let (trust, snapshot) = fixture();
        let bytes = snapshot.encode_to_vec();
        let signed = proto::SignedSnapshot {
            signature: trust
                .keys
                .sign(b"racer/config/v2", &[&bytes])
                .unwrap()
                .to_vec(),
            snapshot: bytes,
        };
        let wrap = |e| proto::Configuration {
            contents: Some(proto::configuration::Contents::Signed(e)),
        };
        assert_eq!(
            trust.prepare_http(wrap(signed.clone())).unwrap().config,
            snapshot
        );
        for mutate in [
            |e: &mut proto::SignedSnapshot| e.snapshot[0] ^= 1,
            |e: &mut proto::SignedSnapshot| e.signature[0] ^= 1,
            |e: &mut proto::SignedSnapshot| e.signature[64] ^= 1,
            |e: &mut proto::SignedSnapshot| e.signature.clear(),
        ] {
            let mut e = signed.clone();
            mutate(&mut e);
            assert!(trust.prepare_http(wrap(e)).is_err());
        }
    }

    #[test]
    fn remote_configuration_requires_a_signature_even_without_verifiers() {
        let (mut trust, snapshot) = fixture();
        trust.keys = Keys::default();
        let envelope = proto::Configuration {
            contents: Some(proto::configuration::Contents::Snapshot(snapshot)),
        };
        assert!(trust.prepare_http(envelope.clone()).is_err());
        assert!(trust.prepare(envelope).is_ok());
    }

    #[test]
    fn scoped_endpoints_authorization_and_epoch_isolation() {
        use crate::control::Updates;
        let (trust, mut config) = crate::control::tests::rdma_fixture();
        let peer = config.peers[0].id.clone();
        let scope = |port| proto::VolumePeerEndpoints {
            peers: vec![proto::VolumePeerEndpoint {
                peer: peer.clone(),
                http_address: format!("127.0.0.1:{port}"),
            }],
        };
        config.volumes[0].peer_endpoints = Some(scope(9001));
        let mut second = config.volumes[0].clone();
        second.id = "second".into();
        second.listen = "127.0.0.1:9002".into();
        second.peer_endpoints = Some(scope(9003));
        config.volumes.push(second);
        let prepared = prepare_snapshot(&trust, config.clone());
        let first = &config.volumes[0].id;
        assert_eq!(
            prepared
                .eligible_peer_for_volume(first, &peer)
                .unwrap()
                .endpoint()
                .address()
                .tcp()
                .unwrap()
                .port(),
            9001
        );
        assert_eq!(
            prepared
                .eligible_peer_for_volume("second", &peer)
                .unwrap()
                .endpoint()
                .address()
                .tcp()
                .unwrap()
                .port(),
            9003
        );
        let updates = Updates::default();
        updates.publish(prepared).unwrap();
        config.revision += 1;
        config.volumes[1].peer_endpoints = Some(scope(9004));
        assert!(
            updates
                .publish(prepare_snapshot(&trust, config.clone()))
                .is_err()
        );
        config.volumes[1].topology.as_mut().unwrap().epoch += 1;
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        config.volumes[1].peers.clear();
        config.volumes[1].topology = None;
        let p = prepare_snapshot(&trust, config.clone());
        assert!(
            p.eligible_node_for_volume("second", peer.parse().unwrap())
                .is_some()
        );
        config.volumes[1].peer_endpoints = Some(proto::VolumePeerEndpoints::default());
        let p = prepare_snapshot(&trust, config.clone());
        assert!(
            p.eligible_node_for_volume("second", peer.parse().unwrap())
                .is_none()
        );
        config.volumes[0].peer_endpoints = Some(proto::VolumePeerEndpoints::default());
        assert!(
            trust
                .prepare(proto::Configuration {
                    contents: Some(proto::configuration::Contents::Snapshot(config))
                })
                .is_err()
        );
    }
    #[test]
    fn projected_verifier_swap_is_atomic_and_never_unsigned() {
        let path = std::env::temp_dir().join(format!("racer-keys-{}", std::process::id()));
        std::fs::create_dir_all(path.join("old")).unwrap();
        std::fs::create_dir_all(path.join("new")).unwrap();
        let a = SigningKey::from_bytes(&[3; 32]);
        std::fs::write(
            path.join("old/bundle.json"),
            bundle_tests::bundle(1, 3, &[3], false),
        )
        .unwrap();
        std::fs::write(
            path.join("new/bundle.json"),
            bundle_tests::bundle(2, 4, &[4], false),
        )
        .unwrap();
        std::os::unix::fs::symlink("old", path.join("..data")).unwrap();
        let old = Keys::bundle(&path, false).unwrap();
        std::os::unix::fs::symlink("new", path.join("..next")).unwrap();
        std::fs::rename(path.join("..next"), path.join("..data")).unwrap();
        let next = Keys::bundle(&path, false).unwrap();
        assert_ne!(old.digest(), next.digest());
        assert!(old.trusts(&key_id(&a.verifying_key())));
        assert!(!next.trusts(&key_id(&a.verifying_key())));
        std::fs::write(path.join("new/bundle.json"), [0; 31]).unwrap();
        assert!(Keys::bundle(&path, false).is_err());
        assert!(next.requires_verification());
        std::fs::remove_file(path.join("new/bundle.json")).unwrap();
        assert!(Keys::bundle(&path, false).is_err());
        std::fs::remove_dir_all(path).unwrap();
    }
    #[test]
    fn distinct_keys_domains_and_exact_bytes() {
        let a = SigningKey::from_bytes(&[1; 32]);
        let b = SigningKey::from_bytes(&[2; 32]);
        let alice = Keys::new(Some(a.to_bytes()), vec![b.verifying_key().to_bytes()]).unwrap();
        let bob = Keys::new(Some(b.to_bytes()), vec![a.verifying_key().to_bytes()]).unwrap();
        let signature = alice.sign(b"request", &[b"ab", b"c"]).unwrap();
        bob.verify(b"request", &[b"ab", b"c"], &signature).unwrap();
        assert!(bob.verify(b"response", &[b"ab", b"c"], &signature).is_err());
        assert!(bob.verify(b"request", &[b"a", b"bc"], &signature).is_err());
        assert!(
            alice
                .verify(b"request", &[b"ab", b"c"], &signature)
                .is_err()
        );
        for i in 0..signature.len() {
            let mut bad = signature;
            bad[i] ^= 1;
            assert!(bob.verify(b"request", &[b"ab", b"c"], &bad).is_err());
        }
    }
}

#[cfg(test)]
mod loader_tests {
    //! Real filesystem and isolated startup environment coverage for peer trust.
    use super::*;
    use std::{
        path::PathBuf,
        process::Command,
        time::{Duration, Instant},
    };

    fn public(seed: u8) -> [u8; 32] {
        SigningKey::from_bytes(&[seed; 32])
            .verifying_key()
            .to_bytes()
    }
    fn swap(root: &Path, version: &str) {
        std::os::unix::fs::symlink(version, root.join("..next")).unwrap();
        std::fs::rename(root.join("..next"), root.join("..data")).unwrap();
    }
    fn projected(root: &Path) {
        for (version, seed) in [("..old", 1), ("..new", 2)] {
            std::fs::create_dir_all(root.join(version)).unwrap();
            std::fs::write(
                root.join(version).join("bundle.json"),
                bundle_tests::bundle(seed as u64, seed, &[seed], true),
            )
            .unwrap();
        }
        swap(root, "..old");
        std::os::unix::fs::symlink("..data/bundle.json", root.join("bundle.json")).unwrap();
    }

    #[test]
    fn startup_peer_trust_and_config_interactions() {
        const CHILD: &str = "RACER_A28_CASE";
        if let Some(case) = std::env::var_os(CHILD) {
            let root = PathBuf::from(std::env::var_os("RACER_A28_ROOT").unwrap());
            let peer = root.join("peer");
            match case.to_str().unwrap() {
                "plain" | "root-link" | "projected" => {
                    let keys = Keys::from_env().unwrap();
                    assert!(keys.can_authenticate());
                    assert!(keys.trusts(&key_id(&VerifyingKey::from_bytes(&public(1)).unwrap())));
                    let signature = keys.sign(b"peer", &[b"body"]).unwrap();
                    keys.verify(b"peer", &[b"body"], &signature).unwrap();
                }
                "mixed-links" => {
                    // Visible key links deliberately disagree with ..data. Only the
                    // pinned version is authoritative; stale root links are not read.
                    swap(&peer, "..new");
                    std::fs::remove_file(peer.join("bundle.json")).unwrap();
                    std::os::unix::fs::symlink("..old/bundle.json", peer.join("bundle.json"))
                        .unwrap();
                    let keys = Keys::from_env().unwrap();
                    assert_eq!(
                        keys.digest(),
                        Keys::new(None, vec![public(2)]).unwrap().digest()
                    );
                }
                _ => assert!(
                    Keys::from_env().is_err(),
                    "invalid startup trust accepted: {case:?}"
                ),
            }
            return;
        }

        // Environment changes are confined to bounded child processes, never the
        // parallel test runner or a live subscriber's process environment.
        for case in [
            "plain",
            "root-link",
            "projected",
            "mixed-links",
            "empty",
            "invalid",
            "verify-only",
            "dangling-data",
            "missing",
            "unset",
        ] {
            let root =
                std::env::temp_dir().join(format!("racer-a28-{}-{case}", std::process::id()));
            std::fs::create_dir(&root).unwrap();
            let peer = root.join("peer");
            projected(&peer);
            let version = peer.join("..old");
            let key = version.join("bundle.json");
            let mut selected_peer = peer.clone();
            match case {
                "plain" => selected_peer = version.clone(),
                "root-link" => {
                    selected_peer = root.join("link");
                    std::os::unix::fs::symlink(&peer, &selected_peer).unwrap();
                }
                "empty" => std::fs::remove_file(&key).unwrap(),
                "invalid" => std::fs::write(&key, b"invalid").unwrap(),
                "verify-only" => {
                    std::fs::write(&key, bundle_tests::bundle(1, 1, &[1], false)).unwrap()
                }
                "dangling-data" => swap(&peer, "..missing"),
                "missing" => selected_peer = root.join("missing"),
                _ => {}
            }
            let mut child = Command::new(std::env::current_exe().unwrap());
            child
                .args([
                    "signing::loader_tests::startup_peer_trust_and_config_interactions",
                    "--exact",
                    "--nocapture",
                    "--test-threads=2",
                ])
                .env(CHILD, case)
                .env("RACER_A28_ROOT", &root)
                .env("RACER_UNIVERSE", "03".repeat(32))
                .env("RACER_NODE", "04".repeat(32))
                .env("RACER_PEER_KEYS_DIR", &selected_peer);
            if case == "unset" {
                child.env_remove("RACER_PEER_KEYS_DIR");
            }
            let mut child = child.stdout(std::process::Stdio::piped()).spawn().unwrap();
            let end = Instant::now() + Duration::from_secs(20);
            let status = loop {
                if let Some(status) = child.try_wait().unwrap() {
                    break status;
                }
                if Instant::now() >= end {
                    child.kill().unwrap();
                    child.wait().unwrap();
                    panic!("startup child deadline: {case}");
                }
                std::thread::sleep(Duration::from_millis(10));
            };
            std::fs::remove_dir_all(&root).unwrap();
            assert!(status.success(), "startup case: {case}");
            let output = child.wait_with_output().unwrap();
            assert!(String::from_utf8_lossy(&output.stdout).contains("1 passed; 0 failed"));
        }
    }
}
