//! Mechanism fixtures and byte/authentication regression tests.

use super::*;
use racer_control_wire::{BundleGeneration, ClusterId, KeyringBundle, SCHEMA_VERSION};
use racer_crypto::identity::{
    KeyEpochs,
    test_util::{ca, issue},
};

/// Deterministic canonical fixture identity.
pub(crate) fn node(n: usize) -> NodeId {
    NodeId(format!("{n:08x}-1111-4111-8111-111111111111"))
}

struct NoMac;
impl RequestMac for NoMac {
    fn sign(&self, _: &Keyring, _: &mut MessageHead) -> Result<()> {
        Ok(())
    }

    fn verify(&self, _: &Keyring, _: &MessageHead) -> Result<()> {
        Ok(())
    }
}

/// Independent certificate network without application credential policy.
pub(crate) fn network(count: usize) -> Vec<Rc<Signatures>> {
    let (ca, key) = ca();
    let roots = vec![ca.der().to_vec()];
    let cluster = ClusterId(node(99).0);
    (0..count)
        .map(|i| {
            let node = node(i);
            let (pending, chain) = issue(&ca, &key, &cluster, &node, |_| {});
            let identity = pending
                .accept(cluster.clone(), node.clone(), chain, &roots)
                .unwrap();
            let keys = Rc::new(Keyring::new(
                cluster.clone(),
                node,
                Arc::new(KeyEpochs::default()),
            ));
            keys.install(KeyringBundle {
                schema_version: SCHEMA_VERSION,
                cluster: cluster.clone(),
                generation: BundleGeneration(1),
                peer_trust_roots: roots.clone(),
                cache_keys: vec![],
            })
            .unwrap();
            keys.install_signing_identity(Arc::new(identity)).unwrap();
            let certificates = Rc::new(Certificates::new(cluster.clone(), keys.clone()));
            Rc::new(Signatures::new(keys, certificates, Rc::new(NoMac)))
        })
        .collect()
}

fn head() -> MessageHead {
    let mut head = MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: "/racer/peer/v1".into(),
        },
        headers: vec![],
    };
    push(&mut head, "racer-receiver", node(1).0);
    push(&mut head, "content-length", 0);
    push(&mut head, "racer-kind", "test");
    head
}

fn canonical_fixture(response: bool, fields: usize, value_bytes: usize) -> MessageHead {
    let mut head = head();
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
                        "oversized" => ("x-fixture-00", vec![b'a'; MAX_ENVELOPE_HEAD]),
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
fn rfc9421_exact_request_and_response_signature_base_vectors() {
    let mut request = MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: "/racer/peer/v1?attempt=1".into(),
        },
        headers: vec![],
    };
    push(&mut request, "racer-timestamp", "1700000000123");
    push(&mut request, "racer-signer", node(0).0);
    push(&mut request, "content-length", "0");
    let params = "(\"@method\" \"@request-target\" \"content-length\" \"racer-signer\" \"racer-timestamp\");created=1700000000;keyid=\"00000000-1111-4111-8111-111111111111\";alg=\"ed25519\";tag=\"racer-peer-v5\"";
    push(&mut request, "signature-input", format!("racer={params}"));
    assert_eq!(signature_base(&request).unwrap(), format!("\"@method\": POST\n\"@request-target\": /racer/peer/v1?attempt=1\n\"content-length\": 0\n\"racer-signer\": 00000000-1111-4111-8111-111111111111\n\"racer-timestamp\": 1700000000123\n\"@signature-params\": {params}").as_bytes());
    request.start = StartLine::Response { status: 200 };
    request.headers.retain(|h| h.name != "signature-input");
    let params = "(\"@status\" \"content-length\" \"racer-signer\" \"racer-timestamp\");created=1700000000;keyid=\"00000000-1111-4111-8111-111111111111\";alg=\"ed25519\";tag=\"racer-peer-v5\"";
    push(&mut request, "signature-input", format!("racer={params}"));
    assert_eq!(signature_base(&request).unwrap(), format!("\"@status\": 200\n\"content-length\": 0\n\"racer-signer\": 00000000-1111-4111-8111-111111111111\n\"racer-timestamp\": 1700000000123\n\"@signature-params\": {params}").as_bytes());
}

#[test]
fn ed25519_rfc9421_base_tamper_replay_and_receiver_challenge() {
    let n = network(3);
    let original = n[0].sign(head()).unwrap();
    let clone = || decode_signed(&encode_signed(&original).unwrap()).unwrap();
    let base = String::from_utf8(signature_base(&original.head).unwrap()).unwrap();
    assert!(base.starts_with(
        "\"@method\": POST\n\"@request-target\": /racer/peer/v1\n\"content-length\": 0\n"
    ));
    assert!(base.ends_with(";alg=\"ed25519\";tag=\"racer-peer-v5\""));
    for h in &original.head.headers {
        let mut tamper = clone();
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
    let mut target = clone();
    target.head.start = StartLine::Request {
        method: "GET".into(),
        target: "/racer/peer/v1".into(),
    };
    assert!(n[1].verify_historical(&target).is_err());
    assert!(n[2].verify_proof(clone()).is_err());
    n[1].verify_proof(clone()).unwrap();
    n[1].verify_proof(clone()).unwrap();
    n[2].verify_historical(&original).unwrap();
}

#[test]
fn timestamp_window_and_retained_deadline_revalidate_authority() {
    let clock = uring_runtime::environment::SimulationClock::new_at(
        87,
        std::time::Instant::now(),
        SystemTime::now(),
    );
    let _guard = clock.environment(0).enter();
    let n = network(2);
    let mut head = head();
    head.headers.retain(|h| h.name != "racer-kind");
    push(&mut head, "racer-kind", "request");
    let deadline = millis(uring_runtime::environment::wall_now()).unwrap() + 120_000;
    push(&mut head, "racer-route-deadline", deadline);
    let signed = n[0].sign(head).unwrap();
    clock.advance(Duration::from_secs(60));
    assert!(matches!(
        n[1].verify_historical(&signed),
        Err(Error::Replay)
    ));
    n[1].verify_retained_request(&signed, deadline).unwrap();
    assert!(matches!(
        n[1].verify_retained_request(&signed, deadline + 1),
        Err(Error::Unauthorized)
    ));
    clock.advance(Duration::from_secs(60));
    assert!(matches!(
        n[1].verify_retained_request(&signed, deadline),
        Err(Error::DeadlineExceeded)
    ));
}

#[test]
fn envelope_limits_and_canonical_fields_preserve_exact_bytes() {
    let n = network(2);
    let signed = n[0].sign(head()).unwrap();
    let encoded = encode_signed(&signed).unwrap();
    let auth = ForwardedHead {
        original: Arc::new(signed),
        hops: vec![],
    };
    for length in [0, MAX_BODY] {
        let (decoded, actual) =
            decode_envelope(encode_envelope(&auth, true, length).unwrap(), true).unwrap();
        assert_eq!(actual, length);
        assert_eq!(encode_signed(&decoded.original).unwrap(), encoded);
    }
    assert!(encode_envelope(&auth, true, MAX_BODY + 1).is_err());
    assert!(encode_envelope(&auth, false, 1).is_err());
    for (name, value) in [
        ("content-length", "00"),
        ("racer-peer-version", "4"),
        ("racer-hop-1", "AAAA"),
    ] {
        let mut bad = encode_envelope(&auth, false, 0).unwrap();
        bad.headers.retain(|h| h.name != name);
        push(&mut bad, name, value);
        assert!(decode_envelope(bad, false).is_err());
    }
    let mut duplicate = encode_envelope(&auth, false, 0).unwrap();
    push(&mut duplicate, "Content-Length", 0);
    assert!(decode_envelope(duplicate, false).is_err());
    let mut trailing = STANDARD.decode(&encoded).unwrap();
    trailing.extend_from_slice(b"extra");
    assert!(decode_signed(STANDARD.encode(trailing).as_bytes()).is_err());
    for value in [b"YQ".as_slice(), b"YR=="] {
        assert_eq!(decode_binary(value), Err(Error::Unauthorized));
    }
    assert_eq!(binary(&[0, 1, 255]), "AAH/");
    assert!(decode_nodes(nodes(&[node(1), node(1)]).unwrap().as_bytes()).is_err());
}

#[test]
fn control_chain_rejects_phase_previous_identity_and_extension_substitution() {
    use crate::control::{Binding, Phase};
    let n = network(3);
    let binding = Binding {
        request: [1; 32],
        response: [2; 32],
        transfer: [3; 16],
        membership: 1,
        deadline: encode_deadline(Deadline(
            uring_runtime::environment::now() + Duration::from_secs(30),
        ))
        .unwrap(),
        rail: racer_control_wire::RailId(1),
    };
    let accept = binding
        .sign(&n[0], n[1].node(), Phase::Accept, &[0; 32], 0, vec![])
        .unwrap();
    assert_eq!(Binding::parse_accept(&accept).unwrap(), binding);
    let previous = signed_digest(&accept).unwrap();
    binding
        .verify(&n[1], n[0].node(), accept, &[Phase::Accept], &[0; 32], 0)
        .unwrap();
    for fault in 0..4 {
        let signed = binding
            .sign(&n[0], n[1].node(), Phase::Fallback, &previous, 0, vec![])
            .unwrap();
        let allowed = if fault == 0 {
            Phase::Done
        } else {
            Phase::Fallback
        };
        let from = if fault == 1 { n[2].node() } else { n[0].node() };
        let previous = if fault == 2 { [0; 32] } else { previous };
        let length = usize::from(fault == 3);
        assert!(
            binding
                .verify(&n[1], from, signed, &[allowed], &previous, length)
                .is_err()
        );
    }
    assert!(binding.head(Phase::Offer, &previous, 0, vec![]).is_err());
    assert!(
        binding
            .head(Phase::Finish, &previous, MAX_BODY, vec![])
            .is_ok()
    );
    assert!(
        binding
            .head(Phase::Finish, &previous, MAX_BODY + 1, vec![])
            .is_err()
    );
}

#[test]
fn route_transition_consumes_one_link_without_refilling_authority() {
    use crate::forwarding::RouteState;
    let route = |links, attempts, deadline, visited: &[NodeId]| {
        let mut h = head();
        push(&mut h, "racer-route-membership", 1);
        push_binary(&mut h, "racer-route-request", &[1; 16]);
        push_binary(&mut h, "racer-route-attempt", &[2; 16]);
        push(&mut h, "racer-route-destination", node(3).0);
        push(&mut h, "racer-route-visited", nodes(visited).unwrap());
        push(&mut h, "racer-route-links", links);
        push(&mut h, "racer-route-attempts", attempts);
        push(&mut h, "racer-route-deadline", deadline);
        RouteState::from_head(&h).unwrap()
    };
    let old = route(4, 7, 100, &[node(0)]);
    old.transition(&route(3, 4, 99, &[node(0), node(1)]), &node(1))
        .unwrap();
    for bad in [
        route(4, 4, 99, &[node(0), node(1)]),
        route(3, 8, 99, &[node(0), node(1)]),
        route(3, 4, 101, &[node(0), node(1)]),
        route(3, 4, 99, &[node(0), node(2)]),
    ] {
        assert_eq!(
            old.transition(&bad, &node(1)),
            Err(Error::HopBudgetExhausted)
        );
    }
}

#[test]
fn credential_policy_runs_after_profile_checks_before_signature_verification() {
    struct Reject(std::cell::Cell<usize>);

    impl RequestMac for Reject {
        fn sign(&self, _: &Keyring, _: &mut MessageHead) -> Result<()> {
            Err(Error::Unavailable)
        }

        fn verify(&self, _: &Keyring, _: &MessageHead) -> Result<()> {
            self.0.set(self.0.get() + 1);
            Err(Error::Unavailable)
        }
    }

    let n = network(2);
    let policy = Rc::new(Reject(std::cell::Cell::new(0)));
    let verifier = Signatures::new(n[1].keys.clone(), n[1].certificates.clone(), policy.clone());
    let mut signed = n[0].sign(head()).unwrap();
    signed.signature[0] ^= 1;
    assert!(matches!(
        verifier.verify_historical(&signed),
        Err(Error::Unavailable)
    ));
    assert_eq!(policy.0.get(), 1);
    signed
        .head
        .headers
        .iter_mut()
        .find(|h| h.name == "racer-profile")
        .unwrap()
        .value = b"other-profile".to_vec();
    assert!(matches!(
        verifier.verify_historical(&signed),
        Err(Error::Unauthorized)
    ));
    assert_eq!(policy.0.get(), 1);
    assert!(matches!(verifier.sign(head()), Err(Error::Unavailable)));
}
