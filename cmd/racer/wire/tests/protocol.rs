//! Direct protocol tests independent of application quotas, crypto, or lifecycle.

use racer_wire::control::CacheId;
use racer_wire::http::{Header, MessageHead, StartLine};
use racer_wire::object::client::{self, ReadKind, RequestParser};
use racer_wire::object::origin;
use racer_wire::object::*;

#[test]
fn head_ignores_cache_control_including_former_max_age_retention() {
    let parser = RequestParser::new(usize::MAX);
    for fields in [
        vec![],
        vec!["max-age=42"],
        vec!["max-age"],
        vec!["max-age=1", "max-age=2"],
        vec!["no-cache, extension=ignored"],
    ] {
        for method in ["HEAD", "POST"] {
            let mut head = request();
            if let StartLine::Request { method: m, .. } = &mut head.start {
                *m = method.into();
            }
            for field in &fields {
                head.headers.push(Header::new("Cache-Control", field));
            }
            let parsed = parser.parse(&head).unwrap();
            assert_eq!(parsed.kind.is_head(), method == "HEAD", "{fields:?}");
        }
    }
}

#[test]
fn head_success_never_reports_retention() {
    let head = client::success_head(&metadata(123), None).unwrap();
    assert!(head.unique("Racer-Retain-Until").unwrap().is_none());
    assert_eq!(
        head.unique("Content-Length").unwrap(),
        Some(b"123".as_slice())
    );
}

/// An object descriptor shared by response and fixture scenarios.
fn metadata(length: u64) -> ObjectMetadata {
    ObjectMetadata {
        content_type: Some(ContentType::parse(b"text/plain").unwrap()),
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([0xab; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        length,
        expires_at: ExpiresAt::from_unix_millis(1234).unwrap(),
    }
}

/// Construct a subscription head with literal context bytes.
fn request() -> MessageHead {
    MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: format!("/v1/objects/{}", CacheKey([0xab; 32]).to_hex()),
        },
        headers: vec![
            Header::new("Host", "racer"),
            Header::new("Content-Length", "0"),
            Header::new("Authorization", b"Bearer \xff"),
            Header::new("Racer-Metadata", b"a,\\b\x80"),
        ],
    }
}

/// Context stays borrowed and byte-exact across the client-to-origin boundary.
#[test]
fn borrowed_context_and_subscription_defaults_round_trip() {
    let head = request();
    let parsed = RequestParser::new(usize::MAX).parse(&head).unwrap();
    assert_eq!(parsed.key, CacheKey([0xab; 32]));
    assert_eq!(
        parsed.kind,
        ReadKind::Subscription {
            pin: None,
            range: None,
            page_credits: 2,
            byte_credits: 2 * PAGE_BYTES,
        }
    );
    assert_eq!(
        parsed.authorization.unwrap().as_ptr(),
        head.headers[2].value.as_ptr()
    );
    assert_eq!(
        parsed.metadata.unwrap().as_ptr(),
        head.headers[3].value.as_ptr()
    );
    let outbound =
        origin::request(&parsed.key, "GET", parsed.metadata, parsed.authorization).unwrap();
    assert_eq!(
        outbound.unique("Authorization").unwrap(),
        parsed.authorization
    );
    assert_eq!(outbound.unique("Racer-Metadata").unwrap(), parsed.metadata);
    assert!(
        matches!(outbound.start, StartLine::Request { method, target } if method == "GET" && target == format!("/v1/objects/{}", parsed.key.to_hex()))
    );
}

/// The removed `Racer-Ordered` header from older SDKs is accepted and ignored with any
/// value, including duplicates.
#[test]
fn racer_ordered_header_is_ignored_any_value() {
    let parser = RequestParser::new(usize::MAX);
    let expected = parser.parse(&request()).unwrap().kind;
    for values in [&["1"][..], &["0"], &["2"], &["yes"], &["1", "0"]] {
        let mut head = request();
        for value in values {
            head.headers.push(Header::new("Racer-Ordered", *value));
        }
        assert_eq!(parser.parse(&head).unwrap().kind, expected, "{values:?}");
    }
}

/// Duplicate fields, oversized context, malformed credits, and HEAD ranges fail closed.
#[test]
fn request_failures_preserve_protocol_taxonomy() {
    let parser = RequestParser::new(usize::MAX);
    assert_eq!(parser.header_limit(), client::MAX_HEAD_BYTES);
    for (name, value, error) in [
        ("host", "racer", Error::InvalidRequest),
        ("Racer-Page-Credits", "01", Error::InvalidRequest),
        ("Racer-Page-Credits", "65", Error::InvalidRequest),
        ("Racer-Byte-Credits", "1", Error::InvalidRequest),
        ("Range", "bytes=1-0", Error::InvalidRange),
    ] {
        let mut head = request();
        head.headers.push(Header::new(name, value));
        assert!(matches!(parser.parse(&head), Err(actual) if actual == error));
    }
    let mut head = request();
    head.headers[2].value = vec![b'a'; MAX_FIELD_BYTES + 1];
    assert!(matches!(parser.parse(&head), Err(Error::HeaderTooLarge)));
    head.headers[2].value = b" padded".to_vec();
    assert!(matches!(parser.parse(&head), Err(Error::InvalidRequest)));
    head.headers[2].value = b"Bearer a".to_vec();
    let StartLine::Request { method, .. } = &mut head.start else {
        unreachable!()
    };
    *method = "HEAD".into();
    head.headers.push(Header::new("Range", "bytes=0-0"));
    assert!(matches!(parser.parse(&head), Err(Error::InvalidRequest)));
    assert!(matches!(
        RequestParser::new(1).parse(&request()),
        Err(Error::HeaderTooLarge)
    ));
}

/// Response framing counts fixed frame headers and rejects inconsistent metadata.
#[test]
fn response_heads_and_frame_vectors_are_exact() {
    let metadata = metadata(PAGE_BYTES + 3);
    let head = client::success_head(&metadata, None).unwrap();
    assert_eq!(
        origin::validate_metadata(&head, &metadata.version.object),
        Ok(metadata.clone())
    );
    let range = ByteRange::From(PAGE_BYTES - 1)
        .resolve(metadata.length)
        .unwrap();
    let subscription = client::subscription_head(&metadata, Some(range)).unwrap();
    assert_eq!(
        subscription.unique("Content-Length").unwrap(),
        Some(b"67".as_slice())
    );
    assert_eq!(
        subscription.unique("Racer-Range-Start").unwrap(),
        Some(b"16777215".as_slice())
    );
    assert!(client::success_head(&metadata, Some(range)).is_err());
    assert!(client::subscription_head(&metadata, None).is_err());
    assert_eq!(
        client::subscription_head(&self::metadata(0), None)
            .unwrap()
            .unique("Content-Length")
            .unwrap(),
        Some(b"21".as_slice())
    );
    assert_eq!(
        client::frame(1, 2, 3, 4),
        [
            1, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 4
        ]
    );
    assert_eq!(
        client::error_head(405, None)
            .unwrap()
            .unique("Allow")
            .unwrap(),
        Some(b"HEAD, POST".as_slice())
    );
    assert_eq!(
        client::error_head(416, Some(0))
            .unwrap()
            .unique("Content-Range")
            .unwrap(),
        Some(b"bytes */0".as_slice())
    );
    assert!(matches!(
        client::error_head(416, None),
        Err(Error::Internal)
    ));
    assert!(matches!(
        client::error_head(416, Some(u64::MAX)),
        Err(Error::Internal)
    ));
}

/// Framing is validated before any status can serve as missing-object evidence.
#[test]
fn origin_error_framing_and_pinned_misses_keep_their_meanings() {
    for (status, expected) in [
        (400, Error::InvalidRequest),
        (401, Error::OriginRejected),
        (403, Error::OriginForbidden),
        (404, Error::NotFound),
        (412, Error::VersionUnavailable),
        (431, Error::HeaderTooLarge),
        (500, Error::Internal),
        (503, Error::Unavailable),
    ] {
        let mut head = MessageHead {
            start: StartLine::Response { status },
            headers: vec![Header::new("Content-Length", "0")],
        };
        assert_eq!(origin::validate_response(&head, false), Err(expected));
        if status == 404 {
            assert_eq!(
                origin::validate_response(&head, true),
                Err(Error::BadGateway)
            );
        }
        head.headers[0].value = b" 0".to_vec();
        assert_eq!(
            origin::validate_response(&head, false),
            Err(Error::BadGateway)
        );
    }
    let head = client::error_head(416, Some(42)).unwrap();
    assert_eq!(
        origin::validate_response(&head, false),
        Err(Error::UnsatisfiableRangeWithLength(42))
    );
}

/// The feature-gated fixture is usable without the application and cleans up sockets.
#[cfg(feature = "test-util")]
#[test]
fn portable_fixture_serves_pages_and_missing_pins() {
    use racer_wire::object::test_util::{AdapterOrigin, RequestKind};
    use std::io::{Read, Write};
    use std::os::unix::net::UnixStream;
    use std::time::Duration;

    let metadata = metadata(3);
    let fixture = AdapterOrigin::new("cache", metadata.clone());
    let path = fixture.root.join("cache/origin/socket");
    let exchange = |pinned: bool| {
        let mut stream = UnixStream::connect(&path).unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        stream
            .set_write_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        let pin = if pinned { "If-Match: \"v1\"\r\n" } else { "" };
        write!(stream, "GET /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRange: bytes=0-16777215\r\n{pin}\r\n", metadata.version.object.key.to_hex()).unwrap();
        let mut response = Vec::new();
        stream.read_to_end(&mut response).unwrap();
        let (head, used) = racer_wire::http::Codec::<()>::new(32768)
            .decode_head(&response)
            .unwrap()
            .unwrap();
        (head, response[used..].to_vec())
    };
    let (head, body) = exchange(false);
    assert_eq!(body, b"abc");
    assert_eq!(
        origin::validate_bootstrap(
            &head,
            &metadata.version.object,
            racer_wire::object::PageSize::DEFAULT
        ),
        Ok((metadata.clone(), 3))
    );
    fixture.set_missing(true);
    let (head, body) = exchange(true);
    assert!(body.is_empty());
    assert_eq!(
        origin::validate_response(&head, true),
        Err(Error::VersionUnavailable)
    );
    assert_eq!(fixture.count(RequestKind::InitialGet), 1);
    assert_eq!(fixture.completed(RequestKind::PinnedGet), 1);
    drop(fixture);
    assert!(!path.exists());
}

/// A local-only page request is a pinned page GET plus `Racer-Source: local`.
#[test]
fn local_page_request_is_a_pinned_get_with_the_local_source() {
    let metadata = metadata(PageSize::DEFAULT.get() + 3);
    let page = PageId {
        version: metadata.version.clone(),
        number: PageNumber(1),
    };
    let size = PageSize::DEFAULT;
    let head = origin::local_page_request(&page, size, Some(b"meta"), None).unwrap();
    let StartLine::Request { method, target } = &head.start else {
        panic!("request line");
    };
    assert_eq!(method, "GET");
    assert_eq!(
        target,
        &format!("/v1/objects/{}", page.version.object.key.to_hex())
    );
    let last = 2 * size.get() - 1;
    let range = format!("bytes={}-{last}", size.get());
    assert_eq!(head.unique("Range").unwrap(), Some(range.as_bytes()));
    assert_eq!(head.unique("If-Match").unwrap(), Some(b"\"v1\"".as_slice()));
    assert_eq!(
        head.unique(origin::SOURCE_HEADER).unwrap(),
        Some(origin::SOURCE_LOCAL)
    );
    assert_eq!(
        head.unique("Racer-Metadata").unwrap(),
        Some(b"meta".as_slice())
    );
    assert!(head.unique("Authorization").unwrap().is_none());
    // Opaque context keeps the ordinary request validation.
    assert_eq!(
        origin::local_page_request(&page, size, Some(b" bad"), None).map(|_| ()),
        Err(Error::InvalidRequest)
    );
}

/// A well-framed 404 or 412 is a local miss; other statuses keep their
/// meaning, and a success must match the pinned page exactly.
#[test]
fn local_page_misses_are_not_errors() {
    let metadata = metadata(3);
    let page = PageId {
        version: metadata.version.clone(),
        number: PageNumber(0),
    };
    let size = PageSize::DEFAULT;
    for status in [404, 412] {
        let head = MessageHead {
            start: StartLine::Response { status },
            headers: vec![Header::new("Content-Length", "0")],
        };
        assert_eq!(
            origin::validate_local_page_head(&head, &page, size),
            Ok(None)
        );
    }
    for (status, expected) in [
        (400, Error::InvalidRequest),
        (401, Error::OriginRejected),
        (500, Error::Internal),
        (503, Error::Unavailable),
    ] {
        let head = MessageHead {
            start: StartLine::Response { status },
            headers: vec![Header::new("Content-Length", "0")],
        };
        assert_eq!(
            origin::validate_local_page_head(&head, &page, size),
            Err(expected)
        );
    }
    // A malformed miss is never a miss.
    let head = MessageHead {
        start: StartLine::Response { status: 404 },
        headers: vec![Header::new("Content-Length", "1")],
    };
    assert_eq!(
        origin::validate_local_page_head(&head, &page, size),
        Err(Error::BadGateway)
    );
    let success = |range: &str, length: &str, etag: &str| MessageHead {
        start: StartLine::Response { status: 206 },
        headers: vec![
            Header::new("Content-Length", length),
            Header::new("Content-Range", range),
            Header::new("Content-Type", "application/octet-stream"),
            Header::new("ETag", etag),
            Header::new("Racer-Expires-At", "1234"),
            Header::new("Racer-Content-Type", "text/plain"),
        ],
    };
    assert_eq!(
        origin::validate_local_page_head(&success("bytes 0-2/3", "3", "\"v1\""), &page, size),
        Ok(Some((metadata.clone(), 3)))
    );
    // A short page or another version fails instead of being served.
    assert_eq!(
        origin::validate_local_page_head(&success("bytes 0-1/3", "2", "\"v1\""), &page, size),
        Err(Error::BadGateway)
    );
    assert_eq!(
        origin::validate_local_page_head(&success("bytes 0-2/3", "3", "\"v2\""), &page, size),
        Err(Error::BadGateway)
    );
}

/// The fixture serves local-only GETs only for pages it holds locally.
#[cfg(feature = "test-util")]
#[test]
fn portable_fixture_serves_local_copies_only() {
    use racer_wire::object::test_util::{AdapterOrigin, RequestKind};
    use std::io::{Read, Write};
    use std::os::unix::net::UnixStream;
    use std::time::Duration;

    let metadata = metadata(3);
    let page = PageId {
        version: metadata.version.clone(),
        number: PageNumber(0),
    };
    let fixture = AdapterOrigin::new("cache", metadata.clone());
    let path = fixture.root.join("cache/origin/socket");
    let exchange = || {
        let mut stream = UnixStream::connect(&path).unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        stream
            .set_write_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        write!(stream, "GET /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRange: bytes=0-16777215\r\nIf-Match: \"v1\"\r\nRacer-Source: local\r\n\r\n", metadata.version.object.key.to_hex()).unwrap();
        let mut response = Vec::new();
        stream.read_to_end(&mut response).unwrap();
        let (head, used) = racer_wire::http::Codec::<()>::new(32768)
            .decode_head(&response)
            .unwrap()
            .unwrap();
        (head, response[used..].to_vec())
    };
    let (head, body) = exchange();
    assert!(body.is_empty());
    assert_eq!(
        origin::validate_local_page_head(&head, &page, PageSize::DEFAULT),
        Ok(None)
    );
    fixture.hold_local(0);
    let (head, body) = exchange();
    assert_eq!(body, b"abc");
    assert_eq!(
        origin::validate_local_page_head(&head, &page, PageSize::DEFAULT),
        Ok(Some((metadata.clone(), 3)))
    );
    assert_eq!(fixture.count(RequestKind::LocalGet), 2);
    assert_eq!(fixture.count(RequestKind::PinnedGet), 0);
}
