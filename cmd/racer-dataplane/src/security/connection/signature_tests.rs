use super::p as protocol;
use super::*;
use crate::model::ClusterId;
use std::time::SystemTime;

pub(crate) fn node(n: usize) -> NodeId {
    NodeId(format!("{n:08x}-1111-4111-8111-111111111111"))
}
pub(crate) fn network(count: usize) -> Vec<Rc<Signatures>> {
    super::super::test_support::identities(
        ClusterId(node(99).0),
        &(0..count).map(node).collect::<Vec<_>>(),
        mac_test_keys,
    )
    .into_iter()
    .map(|identity| identity.signatures)
    .collect()
}
pub(crate) fn mac_test_keys() -> Vec<crate::control::wire::CacheEncryptionKey> {
    use crate::control::wire::*;
    [node(88).0, super::super::identity::tests::CACHE.into()]
        .into_iter()
        .enumerate()
        .map(|(i, cache)| CacheEncryptionKey {
            key: CacheKeyRef {
                cache: crate::model::CacheId(cache),
                id: crate::model::KeyId::from_generation(1, 100 + i as u32).unwrap(),
                purpose: CacheKeyPurpose::OriginCredentials,
            },
            state: CacheKeyState::Active,
            material: [100 + i as u8; 32],
        })
        .collect()
}
pub(crate) fn mac_test_key(cache: &str) -> Vec<crate::control::wire::CacheEncryptionKey> {
    let mut keys = mac_test_keys();
    keys.truncate(1);
    keys[0].key.cache = crate::model::CacheId(cache.into());
    keys
}
pub(crate) fn clone_head(head: &SignedHead) -> SignedHead {
    let codec = Codec::new(protocol::MAX_HEAD, u64::MAX);
    let encoded = codec.encode_head(&head.head).unwrap();
    SignedHead {
        head: codec.decode_head(&encoded).unwrap().unwrap().0,
        signature: head.signature.clone(),
    }
}
fn head(receiver: usize) -> MessageHead {
    let mut head = MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: "/racer/peer/v1".into(),
        },
        headers: Vec::new(),
    };
    push(&mut head, "racer-receiver", node(receiver).0);
    push(&mut head, "content-length", 0);
    push(&mut head, "racer-kind", "test");
    head
}
fn canonical_fixture(response: bool, fields: usize, value_bytes: usize) -> MessageHead {
    let mut head = head(1);
    if response {
        head.start = StartLine::Response { status: 200 };
    }
    push(&mut head, "racer-timestamp", "1700000000123");
    push(&mut head, "racer-signer", node(0).0);
    for i in (0..fields).rev() {
        push(
            &mut head,
            &format!("x-fixture-{i:02}"),
            "a".repeat(value_bytes),
        );
    }
    let input = signature_input(&head).unwrap();
    push(&mut head, "signature-input", format!("racer={input}"));
    push(&mut head, "signature", "racer=:fixture:");
    head
}
#[test]
fn signature_base_preserves_order_case_and_rejects_invalid_components() {
    for response in [false, true] {
        let mut head = canonical_fixture(response, 8, 16);
        let expected = signature_base(&head).unwrap();
        assert_ne!(expected.last(), Some(&b'\n'));
        head.headers.reverse();
        for h in &mut head.headers {
            h.name = h.name.to_ascii_uppercase();
        }
        assert_eq!(signature_base(&head).unwrap(), expected);
        for fault in [
            "duplicate",
            "duplicate-input",
            "duplicate-signature",
            "leading",
            "trailing",
            "non-ascii",
            "newline",
            "bad-name",
            "timestamp",
            "signer",
            "missing-input",
            "coverage",
            "oversized",
        ] {
            let mut head = canonical_fixture(response, 8, 16);
            match fault {
                "duplicate" => push(&mut head, "X-Fixture-00", "duplicate"),
                "duplicate-input" => push(&mut head, "Signature-Input", "duplicate"),
                "duplicate-signature" => push(&mut head, "Signature", "duplicate"),
                "missing-input" => head.headers.retain(|h| h.name != "signature-input"),
                _ => {
                    let (name, value) = match fault {
                        "leading" => ("x-fixture-00", b" leading".to_vec()),
                        "trailing" => ("x-fixture-00", b"trailing\t".to_vec()),
                        "non-ascii" => ("x-fixture-00", vec![0xff]),
                        "newline" => ("x-fixture-00", b"x\r\ny".to_vec()),
                        "timestamp" => ("racer-timestamp", b"01700000000123".to_vec()),
                        "signer" => ("racer-signer", b"not-a-uuid".to_vec()),
                        "coverage" => ("signature-input", b"racer=()".to_vec()),
                        "oversized" => (
                            "x-fixture-00",
                            vec![b'a'; crate::peer::protocol::MAX_ENVELOPE_HEAD],
                        ),
                        "bad-name" => ("x-fixture-00", b"valid".to_vec()),
                        _ => unreachable!(),
                    };
                    let field = head.headers.iter_mut().find(|h| h.name == name).unwrap();
                    field.value = value;
                    if fault == "bad-name" {
                        field.name = "invalid name".into();
                    }
                }
            }
            assert!(
                signature_base(&head).is_err(),
                "response={response} fault={fault}"
            );
        }
    }
}
#[test]
#[ignore = "release-only canonical signature-base construction benchmark, no cryptography"]
fn signature_base_benchmark() {
    use std::{hint::black_box, time::Instant};
    assert!(!cfg!(debug_assertions), "run with --release");
    for (label, fields, value_bytes) in
        [("small", 0, 0), ("fields", 24, 64), ("envelope", 24, 4096)]
    {
        for response in [false, true] {
            let head = canonical_fixture(response, fields, value_bytes);
            let expected = signature_base(&head).unwrap();
            const ITERATIONS: usize = 2000;
            for sample in 0..6 {
                let start = Instant::now();
                for _ in 0..ITERATIONS {
                    black_box(signature_base(black_box(&head)).unwrap());
                }
                let elapsed = start.elapsed();
                assert_eq!(signature_base(&head).unwrap(), expected);
                if sample != 0 {
                    println!(
                        "signature_base case={label} response={response} fields={} base_bytes={} sample={sample} iterations={ITERATIONS} ns_per_op={:.0}",
                        head.headers.len(),
                        expected.len(),
                        elapsed.as_nanos() as f64 / ITERATIONS as f64
                    );
                }
            }
        }
    }
}
#[test]
fn request_mac_rotates_and_rejects_missing_retired_or_mutated_tags() {
    use crate::control::wire::*;
    let network = network(2);
    let make = || {
        let mut request = head(1);
        request
            .headers
            .iter_mut()
            .find(|h| h.name == "racer-kind")
            .unwrap()
            .value = b"request".to_vec();
        push(&mut request, "racer-cache", node(88).0);
        request
    };
    let old = network[0].sign(make()).unwrap();
    assert!(network[1].verify_proof(clone_head(&old)).is_ok());
    let mut tampered = clone_head(&old);
    tampered
        .head
        .headers
        .iter_mut()
        .find(|h| h.name == "racer-request-mac")
        .unwrap()
        .value[0] ^= 1;
    assert!(network[1].verify_proof(tampered).is_err());
    let mut missing = clone_head(&old);
    missing
        .head
        .headers
        .retain(|h| h.name != "racer-request-mac");
    assert!(network[1].verify_proof(missing).is_err());
    for signer in &network {
        let mut keys = mac_test_keys();
        for key in &mut keys {
            key.state = CacheKeyState::Retiring;
        }
        let mut active = mac_test_keys();
        for key in &mut active {
            key.key.id.0[4..12].copy_from_slice(&2u64.to_be_bytes());
            key.material[0] ^= 1;
        }
        keys.extend(active);
        signer
            .keys
            .install(KeyringBundle {
                schema_version: SCHEMA_VERSION,
                cluster: signer.keys.cluster().clone(),
                generation: BundleGeneration(2),
                peer_trust_roots: (*signer.keys.peer_trust_roots().unwrap()).clone(),
                cache_keys: keys,
            })
            .unwrap();
    }
    assert!(
        network[1].verify_proof(clone_head(&old)).is_err(),
        "retiring epoch closes new admission"
    );
    let current = network[0].sign(make()).unwrap();
    assert!(network[1].verify_proof(current).is_ok());
}
#[test]
fn rfc9421_exact_request_and_response_signature_base_vectors() {
    let mut request = MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: "/racer/peer/v1?attempt=1".into(),
        },
        headers: Vec::new(),
    };
    // Deliberately unsorted input: canonical components are sorted by name.
    push(&mut request, "racer-timestamp", "1700000000123");
    push(&mut request, "racer-signer", node(0).0);
    push(&mut request, "content-length", "0");
    let params = "(\"@method\" \"@request-target\" \"content-length\" \"racer-signer\" \"racer-timestamp\");created=1700000000;keyid=\"00000000-1111-4111-8111-111111111111\";alg=\"ed25519\";tag=\"racer-peer-v5\"";
    push(&mut request, "signature-input", format!("racer={params}"));
    assert_eq!(signature_base(&request).unwrap(), format!(
        "\"@method\": POST\n\"@request-target\": /racer/peer/v1?attempt=1\n\"content-length\": 0\n\"racer-signer\": 00000000-1111-4111-8111-111111111111\n\"racer-timestamp\": 1700000000123\n\"@signature-params\": {params}"
    ).as_bytes());
    request.start = StartLine::Response { status: 200 };
    request.headers.retain(|h| h.name != "signature-input");
    let params = "(\"@status\" \"content-length\" \"racer-signer\" \"racer-timestamp\");created=1700000000;keyid=\"00000000-1111-4111-8111-111111111111\";alg=\"ed25519\";tag=\"racer-peer-v5\"";
    push(&mut request, "signature-input", format!("racer={params}"));
    assert_eq!(signature_base(&request).unwrap(), format!(
        "\"@status\": 200\n\"content-length\": 0\n\"racer-signer\": 00000000-1111-4111-8111-111111111111\n\"racer-timestamp\": 1700000000123\n\"@signature-params\": {params}"
    ).as_bytes());
}
#[test]
fn ed25519_rfc9421_base_tamper_replay_and_receiver_challenge() {
    let n = network(3);
    let original = n[0].sign(head(1)).unwrap();
    let base = String::from_utf8(signature_base(&original.head).unwrap()).unwrap();
    assert!(base.starts_with(
        "\"@method\": POST\n\"@request-target\": /racer/peer/v1\n\"content-length\": 0\n"
    ));
    assert!(base.ends_with(";alg=\"ed25519\";tag=\"racer-peer-v5\""));
    for h in &original.head.headers {
        let mut tamper = clone_head(&original);
        tamper
            .head
            .headers
            .iter_mut()
            .find(|v| v.name == h.name)
            .unwrap()
            .value
            .push(b'x');
        assert!(
            n[1].verify_historical(&tamper).is_err(),
            "accepted {} mutation",
            h.name
        );
    }
    let mut target = clone_head(&original);
    target.head.start = StartLine::Request {
        method: "GET".into(),
        target: "/racer/peer/v1".into(),
    };
    assert!(n[1].verify_historical(&target).is_err());
    assert!(n[2].verify_proof(clone_head(&original)).is_err());
    n[1].verify_proof(clone_head(&original)).unwrap();
    // Historical proofs are reusable; only fresh connection heads admit work.
    n[1].verify_proof(clone_head(&original)).unwrap();
    n[2].verify_historical(&original).unwrap();
    super::tests::replay_and_binding_checks();
}
#[test]
pub(crate) fn malformed_fields_unknown_algorithm_and_historical_expiry() {
    let n = network(2);
    let original = n[0].sign(head(1)).unwrap();
    let mut duplicate = clone_head(&original);
    push(&mut duplicate.head, "Racer-Kind", "test");
    assert!(n[1].verify_historical(&duplicate).is_err());
    let mut algorithm = clone_head(&original);
    for h in &mut algorithm.head.headers {
        if h.name == "signature-input" {
            h.value = String::from_utf8(h.value.clone())
                .unwrap()
                .replace("ed25519", "rsa-pss-sha512")
                .into_bytes();
        }
    }
    assert!(n[1].verify_historical(&algorithm).is_err());
    for time in [
        SystemTime::now() - Duration::from_secs(61),
        SystemTime::now() + Duration::from_secs(6),
    ] {
        let mut stale = clone_head(&original);
        stale.head.headers.retain(|h| {
            h.name != "signature" && h.name != "signature-input" && h.name != "racer-timestamp"
        });
        push(
            &mut stale.head,
            "racer-timestamp",
            protocol::millis(time).unwrap(),
        );
        let input = signature_input(&stale.head).unwrap();
        push(&mut stale.head, "signature-input", format!("racer={input}"));
        stale.signature = n[0]
            .keys
            .signing_identity()
            .unwrap()
            .sign(&signature_base(&stale.head).unwrap())
            .unwrap();
        push(
            &mut stale.head,
            "signature",
            format!("racer=:{}:", protocol::binary(&stale.signature)),
        );
        assert!(matches!(n[1].verify_historical(&stale), Err(Error::Replay)));
    }
}
