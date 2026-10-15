use http1::{MessageHead, StartLine};
use peer_wire::{
    Error, MAX_HOPS, RequestMac, Result, Signatures, SignedHead, binary, decode_nodes, field,
    forwarding::response_hop_head, push, signed_digest,
};
use racer_control_wire::{BundleGeneration, ClusterId, KeyringBundle, NodeId, SCHEMA_VERSION};
use racer_crypto::identity::{
    Certificates, KeyEpochs, Keyring,
    test_util::{ca, issue},
};
use std::{rc::Rc, sync::Arc};

struct NoMac;

impl RequestMac for NoMac {
    fn sign(&self, _: &Keyring, _: &mut MessageHead) -> Result<()> {
        Ok(())
    }

    fn verify(&self, _: &Keyring, _: &MessageHead) -> Result<()> {
        Ok(())
    }
}

fn node(index: usize) -> NodeId {
    NodeId(format!("{index:08x}-1111-4111-8111-111111111111"))
}

fn signatures() -> Signatures {
    let (ca, key) = ca();
    let roots = vec![ca.der().to_vec()];
    let cluster = ClusterId(node(99).0);
    let node = node(1);
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
        peer_trust_roots: roots,
        cache_keys: vec![],
    })
    .unwrap();
    keys.install_signing_identity(Arc::new(identity)).unwrap();
    let certificates = Rc::new(Certificates::new(cluster, keys.clone()));
    Signatures::new(keys, certificates, Rc::new(NoMac))
}

fn signed(signatures: &Signatures, kind: &str) -> SignedHead {
    let mut head = MessageHead {
        start: if kind == "response" {
            StartLine::Response { status: 200 }
        } else {
            StartLine::Request {
                method: "POST".into(),
                target: "/racer/peer/v1/hop".into(),
            }
        },
        headers: vec![],
    };
    if kind != "response" {
        push(&mut head, "host", "");
    }
    push(&mut head, "content-length", 0);
    push(&mut head, "racer-kind", kind);
    push(&mut head, "racer-receiver", node(2).0);
    signatures.sign(head).unwrap()
}

fn proofs() -> [SignedHead; 3] {
    let signatures = signatures();
    ["response", "response-hop", "request"].map(|kind| signed(&signatures, kind))
}

#[test]
fn response_hop_binds_distinct_original_previous_and_request_digests() {
    let [original, previous, request] = proofs();
    let path = [node(3), node(1), node(2)];
    let head = response_hop_head(&original, &previous, &request, &path, 1).unwrap();
    assert!(matches!(
        &head.start,
        StartLine::Request { method, target }
            if method == "POST" && target == "/racer/peer/v1/hop"
    ));
    let digests = [&original, &previous, &request].map(|proof| signed_digest(proof).unwrap());
    assert_ne!(digests[0], digests[1]);
    assert_ne!(digests[0], digests[2]);
    assert_ne!(digests[1], digests[2]);
    let expected = [
        ("host", String::new()),
        ("content-length", "0".into()),
        ("racer-kind", "response-hop".into()),
        ("racer-original", binary(&digests[0])),
        ("racer-previous", binary(&digests[1])),
        ("racer-request-binding", binary(&digests[2])),
        ("racer-reverse-index", "1".into()),
    ];
    assert_eq!(head.headers.len(), expected.len() + 1);
    for (name, value) in expected {
        assert_eq!(field(&head, name).unwrap(), value, "{name}");
    }
    assert_eq!(
        decode_nodes(field(&head, "racer-response-path").unwrap().as_bytes()).unwrap(),
        path
    );
}

#[test]
fn response_hop_request_binding_tracks_head_and_detached_signature() {
    let [original, previous, mut request] = proofs();
    let path = [node(1), node(2)];
    let initial = response_hop_head(&original, &previous, &request, &path, 0).unwrap();
    let mut last_binding = field(&initial, "racer-request-binding").unwrap();
    for change_signature in [false, true] {
        if change_signature {
            request.signature[0] ^= 1;
        } else {
            request.head.start = StartLine::Request {
                method: "POST".into(),
                target: "/racer/peer/v1/changed".into(),
            };
        }
        let changed = response_hop_head(&original, &previous, &request, &path, 0).unwrap();
        let binding = field(&changed, "racer-request-binding").unwrap();
        assert_eq!(binding, binary(&signed_digest(&request).unwrap()));
        assert_ne!(binding, last_binding);
        for name in [
            "racer-original",
            "racer-previous",
            "racer-response-path",
            "racer-reverse-index",
        ] {
            assert_eq!(field(&changed, name), field(&initial, name), "{name}");
        }
        last_binding = binding;
    }
}

#[test]
fn response_hop_preserves_forward_path_order() {
    let [original, previous, request] = proofs();
    let path = [node(3), node(1), node(2)];
    let reversed = [node(2), node(1), node(3)];
    let mut encodings = Vec::new();
    for path in [path, reversed] {
        let head = response_hop_head(&original, &previous, &request, &path, 1).unwrap();
        let encoded = field(&head, "racer-response-path").unwrap();
        let mut expected = Vec::new();
        for node in &path {
            expected.extend_from_slice(&36u32.to_be_bytes());
            expected.extend_from_slice(node.0.as_bytes());
        }
        assert_eq!(encoded, binary(&expected));
        assert_eq!(decode_nodes(encoded.as_bytes()).unwrap(), path);
        encodings.push(encoded);
    }
    assert_ne!(encodings[0], encodings[1]);
}

#[test]
fn response_hop_serializes_path_and_index_boundaries() {
    let [original, previous, request] = proofs();
    for length in 0..=MAX_HOPS + 1 {
        let path: Vec<_> = (0..length).map(node).collect();
        for index in [0, length.saturating_sub(1), length, usize::MAX] {
            let head = response_hop_head(&original, &previous, &request, &path, index).unwrap();
            assert_eq!(
                field(&head, "racer-reverse-index").unwrap(),
                index.to_string(),
                "length={length} index={index}"
            );
            assert_eq!(
                decode_nodes(field(&head, "racer-response-path").unwrap().as_bytes()).unwrap(),
                path,
                "length={length} index={index}"
            );
        }
    }
}

#[test]
fn response_hop_rejects_invalid_paths() {
    let [original, previous, request] = proofs();
    let cases = [
        (
            "adjacent duplicate",
            vec![node(1), node(1)],
            Error::InvalidRequest,
        ),
        (
            "nonadjacent duplicate",
            vec![node(1), node(2), node(1)],
            Error::InvalidRequest,
        ),
        (
            "empty node",
            vec![NodeId(String::new())],
            Error::InvalidRequest,
        ),
        (
            "malformed node",
            vec![node(1), NodeId("not-a-uuid".into())],
            Error::InvalidRequest,
        ),
        (
            "uppercase node",
            vec![NodeId(node(10).0.to_ascii_uppercase())],
            Error::InvalidRequest,
        ),
        (
            "overlong path",
            (0..MAX_HOPS + 2).map(node).collect(),
            Error::HopBudgetExhausted,
        ),
    ];
    for (case, path, error) in cases {
        assert_eq!(
            response_hop_head(&original, &previous, &request, &path, 0).err(),
            Some(error),
            "{case}"
        );
    }
}

#[test]
fn response_hop_propagates_digest_errors_from_each_proof() {
    for position in 0..3 {
        let mut proofs = proofs();
        proofs[position]
            .head
            .headers
            .retain(|header| header.name != "signature-input");
        let [original, previous, request] = proofs;
        assert_eq!(
            response_hop_head(&original, &previous, &request, &[node(1), node(2)], 0).err(),
            Some(Error::Unauthorized),
            "proof position={position}"
        );
    }
}
