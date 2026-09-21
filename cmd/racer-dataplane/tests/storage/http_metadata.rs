pub(crate) fn headers<T>(wire: &str, f: impl FnOnce(Headers<'_>) -> T) -> T {
    let bytes = wire.as_bytes();
    let mut fields = Vec::new();
    let mut start = 0;
    for line in wire.split_inclusive("\r\n") {
        let end = start + line.len() - 2;
        fields.push(crate::http::field(bytes, start, end).unwrap());
        start = end + 2;
    }
    f(Headers {
        bytes,
        headers: &fields,
    })
}

#[test]
fn representation_contract_rejects_noncanonical_or_missing_checksums() {
    let tag = Checksum(*blake3::hash(b"abc").as_bytes()).etag();
    let tag = tag.as_str();
    for value in [
        String::new(),
        "\"opaque\"".into(),
        format!("W/{tag}"),
        tag.to_uppercase(),
        tag[1..65].into(),
        format!("{tag}, {tag}"),
        format!("\"{}\"", "a".repeat(63)),
        format!("\"{}\"", "a".repeat(65)),
    ] {
        let wire = format!("ETag: {value}\r\n");
        assert!(headers(&wire, representation_checksum).is_err(), "{value}");
        assert!(headers(&wire, |h| page_facts(200, h)).is_err());
        assert!(
            headers(&format!("{wire}Content-Range: bytes 0-2/3\r\n"), |h| {
                page_facts(206, h)
            })
            .is_err()
        );
    }
    for wire in [String::new(), format!("ETag: {tag}\r\nETag: {tag}\r\n")] {
        assert!(headers(&wire, representation_checksum).is_err());
        assert!(headers(&wire, |h| page_facts(200, h)).is_err());
    }
    let wire = format!("ETag: {tag}\r\nContent-Range: bytes 0-2/3\r\n");
    let identity = Checksum::from_etag(tag).unwrap();
    assert!(headers(&wire, |h| peer_checksum(h, identity)).is_ok());
    assert!(headers(&wire, |h| peer_checksum(h, Checksum([0; 32]))).is_err());
    assert!(headers("", |h| peer_checksum(h, identity)).is_err());
    assert!(
        headers(&format!("ETag: W/{tag}\r\n"), |h| peer_checksum(
            h, identity
        ))
        .is_err()
    );
    assert_eq!(
        headers(&wire, representation_checksum).unwrap(),
        headers(&wire, |h| page_facts(206, h)).unwrap().checksum
    );
}

#[test]
fn singleton_policy_range_and_checksum_parsing() {
    let p = headers("Cache-Control: max-age=100, public\r\nCache-Control: s-maxage=\"40\", ext=\"a,b\"\r\nAge: 3\r\n", policy).unwrap();
    assert_eq!(p.max_age, Some(100));
    assert_eq!(p.shared_max_age, Some(40));
    assert_eq!(p.effective_ttl(), 37);
    for directive in [
        "no-store",
        "no-cache=\"ETag, X-Other\"",
        "private=\"X-Foo\"",
    ] {
        assert!(
            headers(
                &format!("Cache-Control: max-age=99, {directive}\r\n"),
                policy
            )
            .unwrap()
            .disabled
        );
    }
    for wire in [
        "Cache-Control: max-age=1, MAX-AGE=1\r\n",
        "Cache-Control: max-age=1\r\nCache-Control: max-age=2\r\n",
        "Cache-Control: max-age=+1\r\n",
        "Cache-Control: max-age=\"1\r\n",
        "Cache-Control: max-age=1\"\r\n",
        "Cache-Control: s-maxage=-1\r\n",
        "Age: 1\r\nAge: 1\r\n",
        "Age: +1\r\n",
    ] {
        assert!(headers(wire, policy).is_err(), "{wire}");
    }
    assert_eq!(
        headers("Age: 99\r\nCache-Control: max-age=10\r\n", policy)
            .unwrap()
            .effective_ttl(),
        0
    );
    assert!(
        headers("ETag: \"a\"\r\neTAG: \"a\"\r\n", |h| text(h, "etag")
            .map(|_| ()))
        .is_err()
    );
    let range = content_range("bytes 4194304-4194306/4194307").unwrap();
    assert_eq!(
        (range.start(), range.end(), range.total()),
        (4194304, 4194306, 4194307)
    );
    for range in [
        "bytes */3",
        "bytes 0-3/3",
        "bytes 2-1/3",
        "bytes +0-1/3",
        "bytes 0-1/*",
        "items 0-1/3",
    ] {
        assert!(content_range(range).is_err(), "{range}");
    }
    assert!(headers("Content-Range: bytes 0-1/3\r\n", |h| page_facts(200, h)).is_err());
    assert!(headers("ETag: \"a\"\r\n", |h| page_facts(206, h)).is_err());
    for wire in ["", "ETag: W/\"a\"\r\n", "ETag: \"a\"\r\n"] {
        assert!(headers(wire, |h| page_facts(200, h)).is_err());
    }
    let identity = Checksum(*blake3::hash(b"abc").as_bytes());
    let wire = format!("ETag: {}\r\n", identity.etag().as_str());
    assert_eq!(
        headers(&wire, |h| page_facts(200, h)).unwrap().checksum,
        identity
    );
    assert!(headers("ETag: a\r\n", |h| page_facts(200, h)).is_err());
    assert_eq!(
        headers("X-Racer-CRC64: fF0123456789abcd\r\n", checksum).unwrap(),
        Some(0xff0123456789abcd)
    );
    for value in ["+1", "0x10", "00000000000000000", "g"] {
        assert!(headers(&format!("X-Racer-CRC64: {value}\r\n"), checksum).is_err());
    }
    assert!(headers("Content-Encoding: gzip\r\n", identity_encoding).is_err());
    assert!(
        headers(
            "Content-Encoding: identity\r\nContent-Encoding: identity\r\n",
            identity_encoding
        )
        .is_err()
    );
}
