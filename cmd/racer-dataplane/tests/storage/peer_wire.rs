// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[test]
fn maximum_descriptor_http_bounds() {
    use crate::http_client as client;
    // Full RB01 metadata/page frames, not just target+ETag, at the cap.
    for page in [false, true] {
        let (_, config) = crate::control::tests::fixture();
        let routing = crate::routing::Routing::new(&config.universe, &config.volumes[0]).unwrap();
        let target_len = if page { 3132 } else { 3438 };
        let target = format!("/{}", "x".repeat(target_len - 1));
        let cursor = routing.start(&target);
        let mut wire = b"RB01".to_vec();
        wire.extend(1000u32.to_le_bytes());
        wire.extend(cursor.algorithm.magic());
        wire.extend(cursor.encode());
        wire.extend(b"RD01");
        wire.push(u8::from(page));
        if page {
            wire.extend(0u64.to_le_bytes());
            wire.extend(3u64.to_le_bytes());
            wire.extend(blake3::hash(b"abc").as_bytes());
            wire.extend([0; 258]);
        }
        wire.extend(target.as_bytes());
        assert_eq!(wire.len(), MAX_DESCRIPTOR);
        assert!(routed_descriptor(&wire).is_ok());
        let headers: Vec<(String, Vec<u8>)> = vec![
            ("X-Racer-Fault".into(), hex(&wire).into_bytes()),
            ("X-Racer-Attempt".into(), vec![b'a'; 96]),
            ("X-Racer-Volume".into(), vec![b'v'; 253]),
        ];
        let text = headers
            .iter()
            .map(|(n, v)| format!("{n}: {}\r\n", std::str::from_utf8(v).unwrap()))
            .collect::<String>();
        // Largest DNS authority+port and IPv6 socket authority. Authentication
        // belongs to the TLS channel and does not inflate HTTP descriptors.
        for host in [
            "h".repeat(259),
            "[ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]:65535".into(),
        ] {
            assert!(
                "GET / HTTP/1.1\r\nHost: \r\n\r\n".len() + host.len() + text.len()
                    < crate::http::SCRATCH_SIZE
            );
            let fields = headers
                .iter()
                .map(|(n, v)| (n.as_str(), std::str::from_utf8(v).unwrap()))
                .collect::<Vec<_>>();
            // HEAD's request line is one byte larger than GET; the same
            // production serializer enforces the complete header bound.
            assert!(
                client::Connection::new("127.0.0.1:80".parse().unwrap(), &host)
                    .unwrap()
                    .head(
                        client::Request::new("/", &fields).unwrap(),
                        std::time::Instant::now() + Duration::from_secs(1)
                    )
                    .is_ok()
            );
        }
        wire.push(b'x');
        assert!(unhex(&hex(&wire)).is_err());
        assert!(routed_descriptor(&wire).is_err());
    }
}

#[test]
fn bounded_chain_rejects_truncation_nesting_and_oversized_hops() {
    let bytes = with_budget(b"RD01\0/object".to_vec(), Duration::from_secs(1)).unwrap();
    let mut wire = with_chain(bytes, [7; 32], 0, 0, 42).unwrap();
    assert_eq!(chain(&wire).unwrap(), Some(([7; 32], 0, 0, 42)));
    assert_eq!(routed_descriptor(&wire).unwrap().1.target(), "/object");
    for n in 4..CHAIN_LEN + 14 {
        assert!(chain(&wire[..n]).is_err());
    }
    wire[36] = MAX_HOPS + 1;
    assert!(routed_descriptor(&wire).is_err());
    wire[36] = MAX_HOPS;
    assert!(with_chain(wire.clone(), [7; 32], 1, 1, 42).is_err());
    wire.resize(MAX_DESCRIPTOR, b'x');
    assert!(routed_descriptor(&wire).is_ok());
    wire.push(b'x');
    assert!(routed_descriptor(&wire).is_err());
    assert!(client_fits(
        MAX_DESCRIPTOR - encoded_len(0, true, true, true) - CHAIN_LEN
    ));
    assert!(!client_fits(
        MAX_DESCRIPTOR - encoded_len(0, true, true, true) - CHAIN_LEN + 1
    ));
}

#[test]
fn exact_descriptor_boundaries() {
    use super::{MetadataRequest, Namespace, Object, Record};
    let (_, config) = crate::control::tests::fixture();
    let routing = crate::routing::Routing::new(&config.universe, &config.volumes[0]).unwrap();
    for routed in [false, true] {
        for budget in [false, true] {
            for page in [false, true] {
                let overhead = encoded_len(0, page, routed, budget);
                for delta in [-1isize, 0, 1] {
                    let target = format!(
                        "/{}",
                        "x".repeat(
                            (MAX_DESCRIPTOR - overhead)
                                .checked_add_signed(delta)
                                .unwrap()
                                - 1
                        )
                    );
                    let object =
                        Object::new(Namespace::new("origin").unwrap().digest(), &target).unwrap();
                    let request = if page {
                        let record = Record {
                            content_type: Default::default(),
                            len: 3,
                            expires: 0,
                            checksum: Checksum(*blake3::hash(b"abc").as_bytes()),
                        };
                        UpstreamRequest::PeerPage(
                            std::rc::Rc::new(record).page(&object, 0).unwrap(),
                        )
                    } else {
                        UpstreamRequest::PeerMetadata(MetadataRequest { object })
                    };
                    assert_eq!(
                        request_len(&request, routed, budget).unwrap(),
                        (MAX_DESCRIPTOR as isize + delta) as usize
                    );
                    let inner = descriptor(&request);
                    if !routed && !budget && delta == 1 {
                        assert!(inner.is_err());
                        continue;
                    }
                    let mut wire = Vec::new();
                    if budget {
                        wire.extend(b"RB01");
                        wire.extend(1000u32.to_le_bytes());
                    }
                    if routed {
                        let c = routing.start(&target);
                        wire.extend(c.algorithm.magic());
                        wire.extend(c.encode());
                    }
                    wire.extend(inner.unwrap());
                    assert_eq!(wire.len(), request_len(&request, routed, budget).unwrap());
                    assert_eq!(routed_descriptor(&wire).is_ok(), delta <= 0 && budget);
                    if delta <= 0 && budget {
                        let (_, decoded) = routed_descriptor(&wire).unwrap();
                        assert_eq!(decoded.target(), target);
                        assert_eq!(decoded.page.is_some(), page);
                    }
                }
            }
        }
    }
    assert_eq!(encoded_len(0, false, true, true), 62);
    assert_eq!(encoded_len(0, true, true, true), 368);
    assert!(!client_fits(usize::MAX));
}

#[test]
fn authorization_and_content_type_do_not_change_content_identity_or_placement() {
    use super::{Object, Record};
    let mut a = Object::new(&[3; 32], "/same").unwrap();
    let mut b = a.clone();
    a.authorization = crate::authorization::Authorization::new("Bearer a").unwrap();
    b.authorization = crate::authorization::Authorization::new("Bearer b").unwrap();
    assert_eq!(a.metadata_key().0, b.metadata_key().0);
    let record = std::rc::Rc::new(Record {
        checksum: Checksum([7; 32]),
        len: 3,
        expires: 1,
        content_type: crate::metadata::ContentType::new(b"application/json").unwrap(),
    });
    let pa = record.page(&a, 0).unwrap();
    let pb = record.page(&b, 0).unwrap();
    assert_eq!(pa.key(), pb.key());
    let wire = descriptor(&UpstreamRequest::PeerPage(pa)).unwrap();
    assert_eq!(wire, descriptor(&UpstreamRequest::PeerPage(pb)).unwrap());
    assert!(!wire.windows(6).any(|b| b == b"Bearer"));
    let decoded = decode_descriptor(&wire).unwrap();
    let super::Spec::Page(page) = decoded.spec(super::Namespace([3; 32])).unwrap() else {
        panic!("page")
    };
    assert_eq!(page.content_type(), record.content_type);
    assert_eq!(page.authorization().as_str(), None);
    assert_eq!(page.key(), record.page(&a, 0).unwrap().key());
}

#[test]
fn peer_wire_bounds_and_untrusted_facts() {
    use crate::handlers::{Backend, Provider};
    let backend = Backend::new("127.0.0.1:1", "test-origin").unwrap();
    let mut cache = super::adapter_fixture::cache(backend.namespace(), 1);
    let deadline = || std::time::Instant::now() + std::time::Duration::from_secs(10);
    let wire = b"RD01\0//%2f?x=1&x=2";
    assert_eq!(unhex(&hex(wire)).unwrap(), wire);
    let fault = cache
        .peer_fault::<Provider>(decode_descriptor(wire).unwrap(), deadline())
        .unwrap();
    assert_eq!(fault.target(), "//%2f?x=1&x=2");
    assert_eq!(fault.len(), super::METADATA_SIZE);
    assert!(
        cache
            .peer_fault::<Provider>(
                decode_descriptor(wire)
                    .unwrap()
                    .with_expected([0; 32], fault.len()),
                deadline()
            )
            .is_err()
    );
    let mut largest = b"RD01\0/".to_vec();
    largest.resize(MAX_DESCRIPTOR, b'x');
    assert!(decode_descriptor(&largest).is_ok());
    largest.push(b'x');
    assert!(decode_descriptor(&largest).is_err());
    assert!(unhex(&"00".repeat(MAX_DESCRIPTOR + 1)).is_err());
    for bytes in [
        b"".as_slice(),
        b"RF01",
        b"RF01\0",
        b"RF02\0/",
        b"RF01\x02/",
        b"RF01\x01/",
        b"RF01\0/legacy",
        b"RD01\x01/",
    ] {
        assert!(decode_descriptor(bytes).is_err());
    }
    for wire in ["0", "gg", "zz"] {
        assert!(unhex(wire).is_err());
    }
    // Structurally valid page facts still require cache bounds/version validation.
    let mut page = b"RD01\x01".to_vec();
    page.extend_from_slice(&1u64.to_le_bytes());
    page.extend_from_slice(&3u64.to_le_bytes());
    page.extend_from_slice(&[42; 32]);
    page.extend([0; 258]);
    page.extend_from_slice(b"/unaligned");
    assert!(
        cache
            .peer_fault::<Provider>(decode_descriptor(&page).unwrap(), deadline())
            .is_err()
    );
    page[5..13].copy_from_slice(&0u64.to_le_bytes());
    assert!(
        cache
            .peer_fault::<Provider>(decode_descriptor(&page).unwrap(), deadline())
            .is_ok()
    );
    assert!(decode_descriptor(&page[..311]).is_err());
}

#[test]
fn algorithm_versioned_metadata_and_page_descriptors_are_exact_and_bounded() {
    use super::http_metadata::headers;
    use crate::handlers::{remote_deadline, routing_identity};
    let (_, mut config) = crate::control::tests::fixture();
    let target = "/%2f?x=1&x=2";
    let mut page = b"RD01\x01".to_vec();
    page.extend(0u64.to_le_bytes());
    page.extend(3u64.to_le_bytes());
    page.extend([42; 32]);
    page.extend([0; 258]);
    page.extend(target.as_bytes());
    let mut meta = b"RD01\0".to_vec();
    meta.extend(target.as_bytes());
    for algorithm in [1] {
        config.volumes[0]
            .topology
            .as_mut()
            .unwrap()
            .routing_algorithm = Some(algorithm);
        let routing = crate::routing::Routing::new(&config.universe, &config.volumes[0]).unwrap();
        let cursor = routing.start(target);
        for inner in [&meta, &page] {
            let namespace = crate::cache::Namespace::new("transport-agreement").unwrap();
            let expected = decode_descriptor(inner).unwrap().key(namespace).unwrap();
            let cursor = routing.start_key(&expected);
            let mut wire = cursor.algorithm.magic().to_vec();
            wire.extend(cursor.encode());
            wire.extend(inner);
            assert!(routed_descriptor(&wire).is_err(), "budget is mandatory");
            let mut bounded = b"RB01".to_vec();
            bounded.extend(1500u32.to_le_bytes());
            bounded.extend(&wire);
            let (decoded, descriptor) = routed_descriptor(&bounded).unwrap();
            assert_eq!(decoded.unwrap().encode(), cursor.encode());
            assert_eq!(descriptor.target(), target);
            // HTTP hex framing and RDMA's raw descriptor must select the same
            // key and owner, including its independently advertised value facts.
            let http = unhex(&hex(&bounded)).unwrap();
            let (http_cursor, http_descriptor) = routed_descriptor(&http).unwrap();
            assert_eq!(http_descriptor.key(namespace).unwrap(), expected);
            assert_eq!(descriptor.key(namespace).unwrap(), expected);
            routing.validate(&http_cursor.unwrap(), &expected).unwrap();
            assert!(descriptor.with_expected([0; 32], 3).key(namespace).is_err());
            assert_eq!(
                budget_descriptor(&bounded).unwrap().1,
                Some(Duration::from_millis(1500))
            );
            // Context binds the transmitted budget as well as cursor/value.
            let digest = blake3::hash(&bounded);
            bounded[4..8].copy_from_slice(&1499u32.to_le_bytes());
            assert_ne!(digest, blake3::hash(&bounded));
            bounded[4..8].copy_from_slice(&1500u32.to_le_bytes());
            headers(&format!("X-Racer-Fault: {}\r\n", hex(&bounded)), |h| {
                assert_eq!(routing_identity(h).unwrap(), Some(routing.identity));
            });
            let now = crate::environment::now();
            assert_eq!(remote_deadline(&bounded, now).unwrap(), now);
            bounded[4..8].copy_from_slice(&u32::MAX.to_le_bytes());
            assert_eq!(budget_descriptor(&bounded).unwrap().1, Some(MAX_CANDIDATE));
            bounded[4..8].fill(0);
            assert!(routed_descriptor(&bounded).is_err());
            bounded[4..8].copy_from_slice(&1u32.to_le_bytes());
            bounded[8..12].copy_from_slice(b"RB01");
            assert!(routed_descriptor(&bounded).is_err());
            for n in 0..4 + crate::routing::Cursor::LEN + 6 {
                assert!(routed_descriptor(&wire[..n.min(wire.len())]).is_err());
            }
            wire[3] = b'9';
            assert!(routed_descriptor(&wire).is_err());
        }
        let mut largest = b"RB01".to_vec();
        largest.extend(1000u32.to_le_bytes());
        largest.extend(cursor.algorithm.magic());
        largest.extend(cursor.encode());
        largest.extend(&meta);
        largest.resize(MAX_DESCRIPTOR, b'x');
        assert!(routed_descriptor(&largest).is_ok());
        largest.push(b'x');
        assert!(routed_descriptor(&largest).is_err());
    }
}
