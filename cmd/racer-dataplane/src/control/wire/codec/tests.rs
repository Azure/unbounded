use super::*;

const ROOT: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../../internal/racer/wire/testdata/"
);
fn fixture(name: &str) -> Vec<u8> {
    let mut b = std::fs::read(format!("{ROOT}{name}")).unwrap();
    if b.last() == Some(&b'\n') {
        b.pop();
    }
    b
}
fn round_trip(name: &str, b: &[u8]) -> Result<Vec<u8>> {
    match name {
        "publication.json" => encode_publication(&decode_publication(b)?),
        "bootstrap-request.json" => encode_enrollment_request(&decode_enrollment_request(b)?),
        "bootstrap-response.json" => encode_enrollment_response(&decode_enrollment_response(b)?),
        "bundle.json" => encode_bundle(&decode_bundle(b)?),
        _ => panic!("unknown vector"),
    }
}

#[test]
fn shared_publication_vectors() {
    let p = decode_publication(fixture("publication.json").as_slice()).unwrap();
    let (content, membership) = canonical_content(&p).unwrap();
    assert_eq!(content, fixture("content.json"));
    assert_eq!(membership, fixture("membership.json"));
    let hashes: Value = serde_json::from_slice(&fixture("hashes.json")).unwrap();
    let (ph, mh) = content_hashes(&p).unwrap();
    assert_eq!(ph, hashes["content"]);
    assert_eq!(mh, hashes["membership"]);
    let b = encode_publication(&p).unwrap();
    assert_eq!(b, round_trip("publication.json", &b).unwrap());
}

#[test]
fn shared_bootstrap_and_bundle_vectors() {
    for name in [
        "bootstrap-request.json",
        "bootstrap-response.json",
        "bundle.json",
    ] {
        let b = fixture(name);
        assert_eq!(round_trip(name, &b).unwrap(), b, "{name}");
    }
}

#[test]
fn shared_rejection_vectors() {
    let cases: Value = serde_json::from_slice(&fixture("rejections.json")).unwrap();
    for case in cases.as_array().unwrap() {
        let name = case["name"].as_str().unwrap();
        let file = case["file"].as_str().unwrap();
        let input = String::from_utf8(fixture(file)).unwrap();
        let mutated = input.replacen(
            case["old"].as_str().unwrap(),
            case["new"].as_str().unwrap(),
            1,
        );
        assert_ne!(input, mutated, "mutation did not match: {name}");
        let expected = decode_error(format!(r#"{{"code":{}}}"#, case["code"]).as_bytes())
            .unwrap()
            .code;
        assert_eq!(
            round_trip(file, mutated.as_bytes()).err(),
            Some(expected),
            "{name}"
        );
    }
}

#[test]
fn hash_semantics() {
    let mut p = decode_publication(fixture("publication.json").as_slice()).unwrap();
    let hashes = content_hashes(&p).unwrap();
    p.sequence.0 = 0;
    p.membership_version.0 = 0;
    p.members.reverse();
    p.members[0].rails.reverse();
    assert_eq!(content_hashes(&p).unwrap(), hashes);
    assert_eq!(p.members[0].rails[0].rail.0, 65535, "caller input mutated");
    p.caches[0].socket_mode = 0o600;
    let changed = content_hashes(&p).unwrap();
    assert_ne!(changed.0, hashes.0);
    assert_eq!(changed.1, hashes.1);
    p.members[0].peer_endpoint = "[2001:db8::2]:7443".into();
    let endpoint = content_hashes(&p).unwrap();
    assert_ne!(endpoint.0, changed.0);
    assert_ne!(endpoint.1, changed.1);
}

#[test]
fn byte_bounds_and_malformed_documents() {
    for (name, max) in [
        ("publication.json", MAX_PUBLICATION_BYTES),
        ("bootstrap-request.json", MAX_ENROLLMENT_BYTES),
        ("bootstrap-response.json", MAX_ENROLLMENT_BYTES),
        ("bundle.json", MAX_BUNDLE_BYTES),
    ] {
        let b = fixture(name);
        let mut padded = b.clone();
        padded.resize(max, b' ');
        assert!(round_trip(name, &padded).is_ok(), "exact bound {name}");
        padded.push(b' ');
        assert_eq!(
            round_trip(name, &padded).err(),
            Some(ProtocolFailure::TooLarge)
        );
        for bad in [
            Vec::new(),
            b"null".to_vec(),
            b"[]".to_vec(),
            b[..b.len() - 1].to_vec(),
            [b.as_slice(), b"{}"].concat(),
            [b.as_slice(), &[0xff]].concat(),
            format!("{}{}", "[".repeat(1000), "]".repeat(1000)).into_bytes(),
        ] {
            assert_eq!(
                round_trip(name, &bad).err(),
                Some(ProtocolFailure::InvalidRequest),
                "{name}"
            );
        }
    }
    let mut source = std::io::repeat(0).take((MAX_ENROLLMENT_BYTES * 10) as u64);
    assert_eq!(
        decode_enrollment_request(&mut source).err(),
        Some(ProtocolFailure::TooLarge)
    );
    assert_eq!(source.limit(), (MAX_ENROLLMENT_BYTES * 9 - 1) as u64);
}

#[test]
fn path_member_and_enum_bounds() {
    for name in [
        ".",
        "..",
        "a/b",
        "a\\b",
        "a\0b",
        "A",
        "-a",
        "a-",
        "a..b",
        &"a".repeat(64),
    ] {
        assert!(paths(name).is_err());
    }
    let name = format!("{}.{}", "a".repeat(63), "b".repeat(18));
    assert_eq!(paths(&name).unwrap().0.len(), 107);
    assert!(paths(&(name + "b")).is_err());
    let mut p = decode_publication(fixture("publication.json").as_slice()).unwrap();
    p.members.resize(MAX_MEMBERS + 1, p.members[0].clone());
    assert_eq!(
        encode_publication(&p).err(),
        Some(ProtocolFailure::TooLarge)
    );
    for code in [
        "invalid_request",
        "unauthenticated",
        "forbidden",
        "conflict",
        "too_large",
        "unsupported_version",
        "overloaded",
        "unavailable",
    ] {
        let b = format!(r#"{{"code":"{code}"}}"#);
        assert_eq!(
            encode_error(&decode_error(b.as_bytes()).unwrap()).unwrap(),
            b.as_bytes()
        );
    }
    assert!(decode_error(br#"{"code":"future"}"#.as_slice()).is_err());
}

#[test]
fn maximum_membership() {
    let mut p = decode_publication(fixture("publication.json").as_slice()).unwrap();
    p.members = (0..MAX_MEMBERS)
        .map(|i| Member {
            node: NodeId(format!("{i:08x}-1111-4111-8111-111111111111")),
            shares: NonZeroU32::new(1).unwrap(),
            peer_endpoint: "192.0.2.1:1".into(),
            rails: vec![],
            alignment_enabled: false,
        })
        .collect();
    let b = encode_publication(&p).unwrap();
    assert_eq!(
        decode_publication(b.as_slice()).unwrap().members.len(),
        MAX_MEMBERS
    );
    let mut dto = PublicationDto::from_publication(&p).unwrap();
    let extra: MemberDto =
        serde_json::from_value(serde_json::to_value(&dto.members[0]).unwrap()).unwrap();
    dto.members.push(extra);
    let b = encode(&dto, MAX_PUBLICATION_BYTES).unwrap();
    assert_eq!(
        decode_publication(b.as_slice()).err(),
        Some(ProtocolFailure::TooLarge)
    );
}
