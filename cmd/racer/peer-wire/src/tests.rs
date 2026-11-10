//! Mechanism fixtures and byte/authentication regression tests.

use super::*;
use racer_crypto::identity::{
    KeyEpochs,
    test_util::{ca, issue},
};
use racer_wire::control::{BundleGeneration, ClusterId, KeyringBundle};

/// Deterministic canonical fixture identity.
fn wall_clock(ctx: &racer_io::WorkerCtx) -> racer_crypto::identity::WallClock {
    let clock = ctx.clock.clone();
    racer_crypto::identity::WallClock::new(move || clock.wall_now())
}

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
/// Signing identities whose wall clocks and signatures read `ctx`.
pub(crate) fn network(count: usize, ctx: &racer_io::WorkerCtx) -> Vec<Rc<Signatures>> {
    let (ca, key) = ca();
    let roots = vec![ca.der().to_vec()];
    let cluster = ClusterId(node(99).0);
    (0..count)
        .map(|i| {
            let node = node(i);
            let (pending, chain) = issue(&ca, &key, &cluster, &node, |_| {});
            let identity = pending
                .accept(
                    cluster.clone(),
                    node.clone(),
                    chain,
                    &roots,
                    &wall_clock(ctx),
                )
                .unwrap();
            let keys = Rc::new(Keyring::new(
                cluster.clone(),
                node,
                Arc::new(KeyEpochs::with_clock(wall_clock(ctx))),
            ));
            keys.install(KeyringBundle {
                schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
                cluster: cluster.clone(),
                generation: BundleGeneration(1),
                peer_trust_roots: roots.clone(),
                cache_keys: vec![],
            })
            .unwrap();
            keys.install_signing_identity(Arc::new(identity)).unwrap();
            let certificates = Rc::new(Certificates::new(cluster.clone(), keys.clone()));
            Rc::new(Signatures::new(
                keys,
                certificates,
                Rc::new(NoMac),
                ctx.clone(),
            ))
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
    push(&mut head, "host", "");
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
fn auth_field_recognizes_reserved_names_in_any_ascii_case() {
    for name in [
        "signature-input",
        "signature",
        "racer-profile",
        "racer-cluster",
        "racer-signer",
        "racer-receiver",
        "racer-certificates",
        "racer-session",
        "racer-direction",
        "racer-sequence",
        "racer-timestamp",
        "racer-mac-key",
        "racer-request-mac",
    ] {
        let mixed: String = name
            .chars()
            .enumerate()
            .map(|(i, c)| {
                if i % 2 == 0 {
                    c.to_ascii_uppercase()
                } else {
                    c
                }
            })
            .collect();
        for spelling in [name.to_owned(), name.to_ascii_uppercase(), mixed] {
            assert!(is_auth_field(&spelling), "{spelling}");
            for unknown in [
                format!("x-{spelling}"),
                format!("{spelling}-extra"),
                format!(" {spelling}"),
                format!("{spelling} "),
            ] {
                assert!(!is_auth_field(&unknown), "{unknown:?}");
            }
        }
    }
}

#[test]
fn auth_field_leaves_other_names_unrecognized() {
    for name in [
        "",
        "authorization",
        "content-length",
        "host",
        "racer-kind",
        "racer-metadata",
        "racer-route-deadline",
        "racer-unknown",
        "ſignature",
    ] {
        assert!(!is_auth_field(name), "{name:?}");
        assert!(!is_auth_field(&name.to_ascii_uppercase()), "{name:?}");
    }
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
            "control",
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
                        "control" => ("x-fixture-00", vec![0x7f]),
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
fn signature_base_preserves_obs_text_and_rejects_controls() {
    for response in [false, true] {
        for byte in 0..=u8::MAX {
            let mut head = canonical_fixture(response, 1, 1);
            head.headers
                .iter_mut()
                .find(|h| h.name == "x-fixture-00")
                .unwrap()
                .value = vec![b'a', byte, b'z'];
            let base = signature_base(&head);
            if byte.is_ascii_control() {
                assert!(base.is_err(), "response={response} byte={byte}");
            } else {
                let expected = [b"\"x-fixture-00\": a".as_slice(), &[byte], b"z\n"].concat();
                assert!(
                    base.unwrap().windows(expected.len()).any(|v| v == expected),
                    "response={response} byte={byte}"
                );
            }
        }
    }
}

#[test]
fn object_context_signature_base_vector_and_proof_preserve_opaque_bytes() {
    use racer_wire::object::{CacheKey, client::RequestParser, origin};

    let metadata = b"a,\\b\x80";
    let authorization = b"Bearer \xff";
    let mut head = origin::request(
        &CacheKey([0xab; 32]),
        "GET",
        Some(metadata),
        Some(authorization),
    )
    .unwrap();
    let encoded = Codec::new(MAX_HEAD).encode_head(&head).unwrap();
    let (mut decoded, consumed) = Codec::new(MAX_HEAD).decode_head(&encoded).unwrap().unwrap();
    assert_eq!(consumed, encoded.len());
    decoded.start = StartLine::Request {
        method: "HEAD".into(),
        target: format!("/v1/objects/{}", CacheKey([0xab; 32]).to_hex()),
    };
    let parsed = RequestParser::new(MAX_HEAD).parse(&decoded).unwrap();
    assert_eq!(parsed.metadata, Some(metadata.as_slice()));
    assert_eq!(parsed.authorization, Some(authorization.as_slice()));

    push(&mut head, "racer-timestamp", "1700000000123");
    push(&mut head, "racer-signer", node(0).0);
    let params = "(\"@method\" \"@request-target\" \"authorization\" \"content-length\" \"host\" \"racer-metadata\" \"racer-signer\" \"racer-timestamp\");created=1700000000;keyid=\"00000000-1111-4111-8111-111111111111\";alg=\"ed25519\";tag=\"racer-peer-v2\"";
    push(&mut head, "signature-input", format!("racer={params}"));
    let mut expected = b"\"@method\": GET\n\"@request-target\": /v1/objects/abababababababababababababababababababababababababababababababab\n\"authorization\": Bearer \xff\n\"content-length\": 0\n\"host\": racer\n\"racer-metadata\": a,\\b\x80\n\"racer-signer\": 00000000-1111-4111-8111-111111111111\n\"racer-timestamp\": 1700000000123\n\"@signature-params\": ".to_vec();
    expected.extend_from_slice(params.as_bytes());
    assert_eq!(signature_base(&head).unwrap(), expected);

    head.headers.retain(|h| !is_auth_field(&h.name));
    push(&mut head, "racer-receiver", node(1).0);
    let n = network(2, &racer_io::WorkerCtx::host());
    let signed = n[0].sign(head).unwrap();
    let digest = signed_digest(&signed).unwrap();
    let encoded = encode_signed(&signed).unwrap();
    let mut decoded = decode_signed(&encoded).unwrap();
    assert!(
        n[1].verify_proof(decode_signed(&encoded).unwrap()).is_err(),
        "unresolved chain"
    );
    resolve(&mut decoded, &test_util::AnyChain).unwrap();
    assert_eq!(signed_digest(&decoded).unwrap(), digest);
    let verified = n[1].verify_proof(decoded).unwrap();
    assert_eq!(
        verified.signed().head.unique("racer-metadata").unwrap(),
        Some(metadata.as_slice())
    );
    assert_eq!(
        verified.signed().head.unique("authorization").unwrap(),
        Some(authorization.as_slice())
    );
    for name in ["racer-metadata", "authorization"] {
        let mut tampered = decode_signed(&encoded).unwrap();
        resolve(&mut tampered, &test_util::AnyChain).unwrap();
        let value = &mut tampered
            .head
            .headers
            .iter_mut()
            .find(|h| h.name.eq_ignore_ascii_case(name))
            .unwrap()
            .value;
        *value.last_mut().unwrap() ^= 1;
        assert_ne!(signed_digest(&tampered).unwrap(), digest);
        assert!(n[1].verify_proof(tampered).is_err());
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
    push(&mut request, "host", "");
    push(&mut request, "racer-timestamp", "1700000000123");
    push(&mut request, "racer-signer", node(0).0);
    push(&mut request, "content-length", "0");
    let params = "(\"@method\" \"@request-target\" \"content-length\" \"host\" \"racer-signer\" \"racer-timestamp\");created=1700000000;keyid=\"00000000-1111-4111-8111-111111111111\";alg=\"ed25519\";tag=\"racer-peer-v2\"";
    push(&mut request, "signature-input", format!("racer={params}"));
    assert_eq!(signature_base(&request).unwrap(), format!("\"@method\": POST\n\"@request-target\": /racer/peer/v1?attempt=1\n\"content-length\": 0\n\"host\": \n\"racer-signer\": 00000000-1111-4111-8111-111111111111\n\"racer-timestamp\": 1700000000123\n\"@signature-params\": {params}").as_bytes());
    request.start = StartLine::Response { status: 200 };
    request
        .headers
        .retain(|h| h.name != "signature-input" && h.name != "host");
    let params = "(\"@status\" \"content-length\" \"racer-signer\" \"racer-timestamp\");created=1700000000;keyid=\"00000000-1111-4111-8111-111111111111\";alg=\"ed25519\";tag=\"racer-peer-v2\"";
    push(&mut request, "signature-input", format!("racer={params}"));
    assert_eq!(signature_base(&request).unwrap(), format!("\"@status\": 200\n\"content-length\": 0\n\"racer-signer\": 00000000-1111-4111-8111-111111111111\n\"racer-timestamp\": 1700000000123\n\"@signature-params\": {params}").as_bytes());
}

#[test]
fn ed25519_rfc9421_base_tamper_replay_and_receiver_challenge() {
    let n = network(3, &racer_io::WorkerCtx::host());
    let original = n[0].sign(head()).unwrap();
    let clone = || {
        let mut copy = decode_signed(&encode_signed(&original).unwrap()).unwrap();
        resolve(&mut copy, &test_util::AnyChain).unwrap();
        copy
    };
    let base = String::from_utf8(signature_base(&original.head).unwrap()).unwrap();
    assert!(base.starts_with(
        "\"@method\": POST\n\"@request-target\": /racer/peer/v1\n\"content-length\": 0\n"
    ));
    assert!(base.ends_with(";alg=\"ed25519\";tag=\"racer-peer-v2\""));
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
    let clock = racer_io::SimClock::new_at(87, std::time::Instant::now(), SystemTime::now());
    let sim_ctx = clock.ctx(0);
    let n = network(2, &sim_ctx);
    let mut head = head();
    head.headers.retain(|h| h.name != "racer-kind");
    push(&mut head, "racer-kind", "request");
    let deadline = millis(sim_ctx.clock.wall_now()).unwrap() + 120_000;
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
    let n = network(2, &racer_io::WorkerCtx::host());
    let signed = n[0].sign(head()).unwrap();
    let encoded = encode_signed(&signed).unwrap();
    let auth = ForwardedHead {
        original: Arc::new(signed),
        hops: vec![],
    };
    for length in [0, MAX_BODY] {
        let (decoded, actual) = decode_envelope(
            encode_envelope(&auth, true, length).unwrap(),
            true,
            &test_util::AnyChain,
        )
        .unwrap();
        assert_eq!(actual, length);
        assert_eq!(encode_signed(&decoded.original).unwrap(), encoded);
    }
    assert!(encode_envelope(&auth, true, MAX_BODY + 1).is_err());
    assert!(encode_envelope(&auth, false, 1).is_err());
    for (name, value) in [
        ("content-length", "00"),
        ("racer-peer-version", "1"),
        ("racer-hop-1", "AAAA"),
    ] {
        let mut bad = encode_envelope(&auth, false, 0).unwrap();
        bad.headers.retain(|h| h.name != name);
        push(&mut bad, name, value);
        assert!(decode_envelope(bad, false, &test_util::AnyChain).is_err());
    }
    let mut duplicate = encode_envelope(&auth, false, 0).unwrap();
    push(&mut duplicate, "Content-Length", 0);
    assert!(decode_envelope(duplicate, false, &test_util::AnyChain).is_err());
    let mut trailing = STANDARD.decode(&encoded).unwrap();
    trailing.extend_from_slice(b"extra");
    assert!(decode_signed(STANDARD.encode(trailing).as_bytes()).is_err());
    for value in [b"YQ".as_slice(), b"YR=="] {
        assert_eq!(decode_binary(value), Err(Error::Unauthorized));
    }
    assert_eq!(binary(&[0, 1, 255]), "AAH/");
    let duplicate_nodes = b"\x00\x00\x00\x2400000001-1111-4111-8111-111111111111".repeat(2);
    assert_eq!(
        decode_nodes(binary(&duplicate_nodes).as_bytes()),
        Err(Error::Unauthorized)
    );
}

#[test]
fn unique_node_lists_roundtrip_preserving_order_and_framing() {
    for length in [0, 1, 3, MAX_HOPS + 1] {
        let input: Vec<_> = (0..length).rev().map(node).collect();
        let encoded = nodes(&input).unwrap();
        let mut expected = Vec::new();
        for node in &input {
            expected.extend_from_slice(&36u32.to_be_bytes());
            expected.extend_from_slice(node.0.as_bytes());
        }
        assert_eq!(encoded, binary(&expected));
        assert_eq!(decode_nodes(encoded.as_bytes()).unwrap(), input);
    }
}

#[test]
fn node_list_decoding_rejects_truncated_and_empty_entries() {
    let encoded = STANDARD
        .decode(nodes(&[node(0), node(1)]).unwrap())
        .unwrap();
    for end in 1..encoded.len() {
        if end == 40 {
            continue;
        }
        assert_eq!(
            decode_nodes(binary(&encoded[..end]).as_bytes()),
            Err(Error::Unauthorized),
            "{end}"
        );
    }
    assert_eq!(
        decode_nodes(binary(&[0; 4]).as_bytes()),
        Err(Error::Unauthorized)
    );
    let mut oversize = 257u32.to_be_bytes().to_vec();
    oversize.extend_from_slice(&[b'a'; 257]);
    assert_eq!(
        decode_nodes(binary(&oversize).as_bytes()),
        Err(Error::Unauthorized)
    );
}

#[test]
fn node_list_encoding_rejects_duplicates_invalid_ids_and_excess_hops() {
    let mut maximum_duplicate: Vec<_> = (0..=MAX_HOPS).map(node).collect();
    maximum_duplicate[MAX_HOPS] = node(0);
    for input in [
        vec![node(1), node(1)],
        vec![node(1), node(2), node(1)],
        maximum_duplicate,
        vec![NodeId("not-a-uuid".into())],
    ] {
        assert_eq!(nodes(&input), Err(Error::InvalidRequest));
    }
    let too_many: Vec<_> = (0..MAX_HOPS + 2).map(node).collect();
    assert_eq!(nodes(&too_many), Err(Error::HopBudgetExhausted));
}

#[test]
fn control_chain_rejects_phase_previous_identity_and_extension_substitution() {
    use crate::control::{Binding, Phase};
    let n = network(3, &racer_io::WorkerCtx::host());
    let binding = Binding {
        request: [1; 32],
        response: [2; 32],
        transfer: [3; 16],
        membership: 1,
        deadline: encode_deadline(
            &racer_io::Clock::host(),
            racer_io::Clock::host().now() + Duration::from_secs(30),
        )
        .unwrap(),
        rail: racer_wire::control::RailId(1),
    };
    let grant = || {
        vec![Header::new(
            "racer-rdma-grant",
            binary(&[7; 38]).into_bytes(),
        )]
    };
    assert!(
        binding
            .sign(&n[0], n[1].node(), Phase::Accept, &[0; 32], 0, vec![])
            .is_err(),
        "accept names a grant"
    );
    let accept = binding
        .sign(&n[0], n[1].node(), Phase::Accept, &[0; 32], 0, grant())
        .unwrap();
    assert_eq!(Binding::parse_accept(&accept).unwrap(), binding);
    let previous = signed_digest(&accept).unwrap();
    binding
        .verify(&n[1], n[0].node(), accept, &[Phase::Accept], &[0; 32], 0)
        .unwrap();
    for fault in 0..4 {
        let signed = binding
            .sign(&n[0], n[1].node(), Phase::Finish, &previous, 0, vec![])
            .unwrap();
        let allowed = if fault == 0 {
            Phase::Complete
        } else {
            Phase::Finish
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
    assert!(
        binding
            .head(
                &racer_io::Clock::host(),
                Phase::Accept,
                &previous,
                0,
                vec![]
            )
            .is_err()
    );
    assert!(
        binding
            .head(
                &racer_io::Clock::host(),
                Phase::Finish,
                &previous,
                MAX_BODY,
                vec![]
            )
            .is_ok()
    );
    assert!(
        binding
            .head(
                &racer_io::Clock::host(),
                Phase::Finish,
                &previous,
                MAX_BODY + 1,
                vec![]
            )
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
fn request_builders_supply_host_for_strict_http_codec() {
    use crate::control::{Binding, Phase};
    let n = network(2, &racer_io::WorkerCtx::host());
    let original = n[0].sign(head()).unwrap();
    let hop = forwarding::hop_head("request-hop", &original, &original).unwrap();
    assert_eq!(hop.unique("host").unwrap(), Some(b"".as_slice()));
    Codec::new(MAX_HEAD).encode_head(&hop).unwrap();

    let auth = ForwardedHead {
        original: Arc::new(original),
        hops: vec![],
    };
    let envelope = encode_envelope(&auth, false, 0).unwrap();
    assert_eq!(envelope.unique("host").unwrap(), Some(b"".as_slice()));
    decode_envelope(envelope, false, &test_util::AnyChain).unwrap();
    let binding = Binding {
        request: [1; 32],
        response: [2; 32],
        transfer: [3; 16],
        membership: 1,
        deadline: 1,
        rail: racer_wire::control::RailId(1),
    };
    let grant = vec![Header::new(
        "racer-rdma-grant",
        binary(&[7; 38]).into_bytes(),
    )];
    for (phase, extensions) in [(Phase::Accept, grant), (Phase::Finish, vec![])] {
        let head = binding
            .head(&racer_io::Clock::host(), phase, &[0; 32], 0, extensions)
            .unwrap();
        assert_eq!(
            head.unique("host").unwrap(),
            (!phase.response()).then_some(b"".as_slice())
        );
        Codec::new(MAX_HEAD).encode_head(&head).unwrap();
    }

    let mut duplicate = head();
    push(&mut duplicate, "Host", "");
    assert!(n[0].sign(duplicate).is_err());
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

    let n = network(2, &racer_io::WorkerCtx::host());
    let policy = Rc::new(Reject(std::cell::Cell::new(0)));
    let verifier = Signatures::new(
        n[1].keys.clone(),
        n[1].certificates.clone(),
        policy.clone(),
        racer_io::WorkerCtx::host(),
    );
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

#[test]
fn rdma_link_request_and_response_roundtrip_and_reject_substitution() {
    use crate::link::{BINDING_FIELD, KIND, MAX_SETUP, RdmaLink, SETUP_FIELD, TARGET};
    let n = network(3, &racer_io::WorkerCtx::host());
    let request = RdmaLink {
        setup: vec![7; 74],
        binding: None,
    };
    let response = RdmaLink {
        setup: vec![9; 74],
        binding: Some([5; 32]),
    };
    let head = request.head().unwrap();
    assert!(matches!(&head.start, StartLine::Request { method, target }
        if method == "POST" && target == TARGET));
    assert_eq!(field(&head, "racer-kind").unwrap(), KIND);
    assert_eq!(field(&head, "content-length").unwrap(), "0");
    assert!(head.unique(BINDING_FIELD).unwrap().is_none());
    assert!(matches!(
        response.head().unwrap().start,
        StartLine::Response { status: 200 }
    ));

    // Success: both directions, with and without a known signer.
    let signed = request.sign(&n[0], n[1].node()).unwrap();
    let (verified, parsed) = RdmaLink::verify(
        &n[1],
        None,
        request.sign(&n[0], n[1].node()).unwrap(),
        false,
    )
    .unwrap();
    assert_eq!(parsed, request);
    assert_eq!(verified.peer().node(), n[0].node());
    let input = String::from_utf8(
        signed
            .head
            .unique("signature-input")
            .unwrap()
            .unwrap()
            .to_vec(),
    )
    .unwrap();
    assert!(input.contains(&format!("\"{SETUP_FIELD}\"")));
    let signed = response.sign(&n[1], n[0].node()).unwrap();
    let (_, parsed) = RdmaLink::verify(
        &n[0],
        Some(n[1].node()),
        response.sign(&n[1], n[0].node()).unwrap(),
        true,
    )
    .unwrap();
    assert_eq!(parsed, response);
    let input = String::from_utf8(
        signed
            .head
            .unique("signature-input")
            .unwrap()
            .unwrap()
            .to_vec(),
    )
    .unwrap();
    assert!(input.contains(&format!("\"{BINDING_FIELD}\"")));

    // Failure: wrong direction, signer, receiver, or tampered setup.
    assert!(
        RdmaLink::verify(
            &n[0],
            None,
            response.sign(&n[1], n[0].node()).unwrap(),
            false
        )
        .is_err()
    );
    assert!(
        RdmaLink::verify(
            &n[0],
            Some(n[2].node()),
            response.sign(&n[1], n[0].node()).unwrap(),
            true
        )
        .is_err()
    );
    assert!(
        RdmaLink::verify(
            &n[2],
            None,
            response.sign(&n[1], n[0].node()).unwrap(),
            true
        )
        .is_err()
    );
    let mut tampered = response.sign(&n[1], n[0].node()).unwrap();
    for h in &mut tampered.head.headers {
        if h.name == SETUP_FIELD {
            h.value = binary(&[8; 74]).into_bytes();
        }
    }
    assert!(RdmaLink::verify(&n[0], None, tampered, true).is_err());

    // Edge: setup bounds, malformed binding, wrong kind, extra fields.
    for setup in [vec![], vec![1; MAX_SETUP + 1]] {
        assert!(
            RdmaLink {
                setup,
                binding: None
            }
            .head()
            .is_err()
        );
    }
    assert!(
        RdmaLink {
            setup: vec![1; MAX_SETUP],
            binding: None
        }
        .head()
        .is_ok()
    );
    let mut bad = response.head().unwrap();
    for h in &mut bad.headers {
        if h.name == BINDING_FIELD {
            h.value = binary(&[5; 31]).into_bytes();
        }
    }
    assert!(RdmaLink::parse(&bad).is_err());
    let mut bad = response.head().unwrap();
    bad.headers.retain(|h| h.name != BINDING_FIELD);
    assert!(RdmaLink::parse(&bad).is_err());
    let mut bad = request.head().unwrap();
    for h in &mut bad.headers {
        if h.name == "racer-kind" {
            h.value = b"request".to_vec();
        }
    }
    assert!(RdmaLink::parse(&bad).is_err());
    let mut bad = request.head().unwrap();
    for h in &mut bad.headers {
        if h.name == SETUP_FIELD {
            h.value = b"not base64!".to_vec();
        }
    }
    assert!(RdmaLink::parse(&bad).is_err());
    let mut extra = request.head().unwrap();
    push(&mut extra, "racer-extra", "1");
    push(&mut extra, "racer-receiver", &n[1].node().0);
    let signed = n[0].sign(extra).unwrap();
    assert!(RdmaLink::verify(&n[1], None, signed, false).is_err());
}

#[test]
fn route_state_accepts_eight_links_and_rejects_nine() {
    use crate::forwarding::RouteState;
    let route = |links: u64, visited: usize| {
        let mut h = head();
        push(&mut h, "racer-route-membership", 1);
        push_binary(&mut h, "racer-route-request", &[1; 16]);
        push_binary(&mut h, "racer-route-attempt", &[2; 16]);
        push(&mut h, "racer-route-destination", node(20).0);
        let nodes_list: Vec<_> = (0..visited).map(node).collect();
        push(&mut h, "racer-route-visited", nodes(&nodes_list).unwrap());
        push(&mut h, "racer-route-links", links);
        push(&mut h, "racer-route-attempts", 1);
        push(&mut h, "racer-route-deadline", 100);
        RouteState::from_head(&h)
    };
    // Eight links: the origin alone in `visited` with eight to spend, or eight
    // nodes already visited with the final link left.
    assert!(route(8, 1).is_ok());
    assert!(route(1, 8).is_ok());
    // Nine links, however split, and links beyond the per-route cap.
    for (links, visited) in [(9, 1), (8, 2), (1, 9), (2, 8)] {
        assert_eq!(
            route(links, visited).err(),
            Some(Error::HopBudgetExhausted),
            "{links} links, {visited} visited"
        );
    }
}

#[test]
fn overloaded_head_is_exact_and_bodiless() {
    let head = overloaded_head();
    assert!(is_overloaded(&head));
    // The encoded answer carries nothing but the status and an empty body.
    let bytes = Codec::new(MAX_HEAD).encode_head(&head).unwrap();
    assert_eq!(
        bytes,
        b"HTTP/1.1 429 \r\ncontent-length: 0\r\n\r\n".to_vec()
    );
    let mut other = overloaded_head();
    other.start = StartLine::Response { status: 503 };
    assert!(!is_overloaded(&other));
    let mut body = overloaded_head();
    body.headers[0].value = b"1".to_vec();
    assert!(!is_overloaded(&body));
    let mut extra = overloaded_head();
    extra.headers.push(Header {
        name: "racer-peer-version".into(),
        value: VERSION.as_bytes().to_vec(),
    });
    assert!(!is_overloaded(&extra));
    // A signed envelope is never mistaken for it, and it never decodes as one.
    assert!(decode_envelope(overloaded_head(), true, &test_util::AnyChain).is_err());
}

/// Previous allocation-heavy implementation, kept as a byte-for-byte oracle.
mod legacy {
    use super::*;

    fn components(head: &MessageHead) -> Result<Vec<String>> {
        Codec::new(MAX_ENVELOPE_HEAD).encode_head(head)?;
        let mut names = BTreeSet::new();
        for h in &head.headers {
            let name = h.name.to_ascii_lowercase();
            if !names.insert(name)
                || h.value.first().is_some_and(|b| b.is_ascii_whitespace())
                || h.value.last().is_some_and(|b| b.is_ascii_whitespace())
                || h.value.iter().any(u8::is_ascii_control)
            {
                return Err(Error::Unauthorized);
            }
        }
        names.remove("signature");
        names.remove("signature-input");
        let mut components = match head.start {
            StartLine::Request { .. } => vec!["@method".into(), "@request-target".into()],
            StartLine::Response { .. } => vec!["@status".into()],
        };
        components.extend(names);
        Ok(components)
    }

    pub(super) fn signature_input(head: &MessageHead) -> Result<String> {
        let components = components(head)?
            .iter()
            .map(|s| format!("\"{s}\""))
            .collect::<Vec<_>>()
            .join(" ");
        let timestamp = number(head, "racer-timestamp")? / 1000;
        let keyid = field(head, "racer-signer")?;
        uuid(&keyid)?;
        Ok(format!(
            "({components});created={timestamp};keyid=\"{keyid}\";alg=\"ed25519\";tag=\"{}\"",
            PROFILE
        ))
    }

    pub(super) fn signature_base(head: &MessageHead) -> Result<Vec<u8>> {
        let components = components(head)?;
        let input = signature_input(head)?;
        if field(head, "signature-input")? != format!("racer={input}") {
            return Err(Error::Unauthorized);
        }
        let mut base = Vec::new();
        for name in components {
            base.extend_from_slice(format!("\"{name}\": ").as_bytes());
            match (name.as_str(), &head.start) {
                ("@method", StartLine::Request { method, .. }) => {
                    base.extend_from_slice(method.as_bytes())
                }
                ("@request-target", StartLine::Request { target, .. }) => {
                    base.extend_from_slice(target.as_bytes())
                }
                ("@status", StartLine::Response { status }) => {
                    base.extend_from_slice(status.to_string().as_bytes())
                }
                _ => base.extend_from_slice(head.unique(&name)?.ok_or(Error::Unauthorized)?),
            }
            base.push(b'\n');
        }
        base.extend_from_slice(format!("\"@signature-params\": {input}").as_bytes());
        Ok(base)
    }

    pub(super) fn mac_base(head: &MessageHead) -> Result<Vec<u8>> {
        fn append(out: &mut Vec<u8>, value: &[u8]) -> Result<()> {
            let length = u32::try_from(value.len()).map_err(|_| Error::InvalidRequest)?;
            out.extend_from_slice(&length.to_be_bytes());
            out.extend_from_slice(value);
            Ok(())
        }
        let mut out = b"racer/request-mac/message/v1\0".to_vec();
        let start = match &head.start {
            StartLine::Request { method, target } => format!("{method} {target}"),
            _ => return Err(Error::Unauthorized),
        };
        append(&mut out, start.as_bytes())?;
        for name in components(head)? {
            if name.starts_with('@') || name == "racer-request-mac" {
                continue;
            }
            append(&mut out, name.as_bytes())?;
            append(&mut out, head.unique(&name)?.ok_or(Error::Unauthorized)?)?;
        }
        Ok(out)
    }
}

fn copy(head: &MessageHead) -> MessageHead {
    MessageHead {
        start: match &head.start {
            StartLine::Request { method, target } => StartLine::Request {
                method: method.clone(),
                target: target.clone(),
            },
            StartLine::Response { status } => StartLine::Response { status: *status },
        },
        headers: head
            .headers
            .iter()
            .map(|h| Header {
                name: h.name.clone(),
                value: h.value.clone(),
            })
            .collect(),
    }
}

#[test]
fn signature_and_mac_bases_match_legacy_implementation_bytes() {
    for response in [false, true] {
        for (fields, value_bytes) in [(0, 0), (1, 1), (8, 16), (40, 3)] {
            let mut head = canonical_fixture(response, fields, value_bytes);
            push(&mut head, "Racer-Request-Mac", "racer=:abc:");
            head.headers.push(Header {
                name: "X-Obs".into(),
                value: vec![b'a', 0x80, b'z'],
            });
            // Refresh signature-input to cover the added fields.
            head.headers
                .retain(|h| !h.name.eq_ignore_ascii_case("signature-input"));
            let input = signature_input(&head).unwrap();
            assert_eq!(input, legacy::signature_input(&head).unwrap());
            push(&mut head, "signature-input", format!("racer={input}"));
            for mixed in [false, true] {
                let mut head = copy(&head);
                if mixed {
                    head.headers.reverse();
                    for h in &mut head.headers {
                        h.name = h.name.to_ascii_uppercase();
                    }
                }
                assert_eq!(
                    signature_base(&head).unwrap(),
                    legacy::signature_base(&head).unwrap()
                );
                assert_eq!(mac_base(&head).is_ok(), legacy::mac_base(&head).is_ok());
                if !response {
                    assert_eq!(mac_base(&head).unwrap(), legacy::mac_base(&head).unwrap());
                }
            }
            let mut dup = copy(&head);
            push(&mut dup, "x-obs", "again");
            assert!(signature_base(&dup).is_err() && legacy::signature_base(&dup).is_err());
            assert!(mac_base(&dup).is_err() && legacy::mac_base(&dup).is_err());
        }
    }
}

#[test]
fn hop_field_table_matches_hop_names() {
    for (i, name) in HOP_FIELDS.iter().enumerate() {
        assert_eq!(*name, format!("racer-hop-{i}"));
    }
}
