//! Direct protocol tests independent of application quotas, crypto, or lifecycle.

use http1::{Header, MessageHead, StartLine};
use racer_control_wire::CacheId;
use racer_object_wire::client::{self, ReadKind, RequestParser};
use racer_object_wire::origin;
use racer_object_wire::*;
use std::time::{Duration, UNIX_EPOCH};

#[test]
fn head_retention_accepts_http_lists_and_quoted_delta_seconds() {
    let parser = RequestParser::new(usize::MAX);
    for (fields, expected) in [
        (vec![], None),
        (vec!["no-cache, extension=ignored"], None),
        (vec!["max-age=0"], None),
        (vec!["MAX-age = \"00012\""], Some(12)),
        (vec!["private, max-age=42, immutable"], Some(42)),
        (vec!["extension=\"a, max-age=99\"", "max-age=7"], Some(7)),
        (vec!["max-age=18446744073709551615"], Some(u64::MAX)),
    ] {
        for pin in [false, true] {
            let mut head = request();
            if let StartLine::Request { method, .. } = &mut head.start {
                *method = "HEAD".into();
            }
            if pin {
                head.headers.push(Header::new("If-Match", "\"v1\""));
            }
            for field in &fields {
                head.headers.push(Header::new("Cache-Control", field));
            }
            let parsed = parser.parse(&head).unwrap();
            assert!(parsed.kind.is_head());
            assert_eq!(parsed.kind.pin().is_some(), pin);
            assert_eq!(parsed.retain, expected.map(Duration::from_secs));
        }
    }
}

#[test]
fn head_retention_rejects_bad_max_age_but_post_ignores_it() {
    let parser = RequestParser::new(usize::MAX);
    for fields in [
        vec!["max-age"],
        vec!["max-age="],
        vec!["max-age=-1"],
        vec!["max-age=+1"],
        vec!["max-age=1.0"],
        vec!["max-age=1 2"],
        vec!["max-age=18446744073709551616"],
        vec!["max-age=\"\""],
        vec!["max-age=\"1"],
        vec!["max-age=\"1\\2\""],
        vec!["max-age=1;"],
        vec!["max-age=0, MAX-AGE=0"],
        vec!["max-age=1", "max-age=2"],
    ] {
        let mut head = request();
        for field in &fields {
            head.headers.push(Header::new("Cache-Control", field));
        }
        assert_eq!(parser.parse(&head).unwrap().retain, None);
        if let StartLine::Request { method, .. } = &mut head.start {
            *method = "HEAD".into();
        }
        assert!(
            matches!(parser.parse(&head), Err(Error::InvalidRequest)),
            "{fields:?}"
        );
    }
}

#[test]
fn retained_head_deadline_is_optional_bounded_unix_milliseconds() {
    let m = metadata(123);
    for head in [
        client::success_head(&m, None).unwrap(),
        client::success_head_retained(&m, None, None).unwrap(),
    ] {
        assert!(head.unique("Racer-Retain-Until").unwrap().is_none());
        assert_eq!(
            head.unique("Content-Length").unwrap(),
            Some(b"123".as_slice())
        );
    }
    for millis in [0, 1234, i64::MAX as u64] {
        let deadline = UNIX_EPOCH + Duration::from_millis(millis);
        let head = client::success_head_retained(&m, None, Some(deadline)).unwrap();
        assert_eq!(
            head.unique("Racer-Retain-Until").unwrap(),
            Some(millis.to_string().as_bytes())
        );
        assert_eq!(
            head.unique("Content-Length").unwrap(),
            Some(b"123".as_slice())
        );
    }
    let fractional = UNIX_EPOCH + Duration::from_nanos(1_999_999);
    assert_eq!(
        client::success_head_retained(&m, None, Some(fractional))
            .unwrap()
            .unique("Racer-Retain-Until")
            .unwrap(),
        Some(b"1".as_slice())
    );
    for deadline in [
        UNIX_EPOCH - Duration::from_nanos(1),
        UNIX_EPOCH + Duration::from_millis(i64::MAX as u64 + 1),
    ] {
        assert!(matches!(
            client::success_head_retained(&m, None, Some(deadline)),
            Err(Error::BadGateway)
        ));
    }
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
            ordered: false
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
        ("Racer-Ordered", "2", Error::InvalidRequest),
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

/// Mapped delivery is a subscription-only opt-in with its own exact framing.
#[test]
fn mapped_delivery_requests_heads_and_frames_are_exact() {
    let parser = RequestParser::new(usize::MAX);
    assert!(!parser.parse(&request()).unwrap().mapped);
    for (value, mapped) in [("mapped", true), ("inline", false)] {
        let mut head = request();
        head.headers.push(Header::new("Racer-Delivery", value));
        assert_eq!(parser.parse(&head).unwrap().mapped, mapped);
    }
    for value in ["Mapped", "memfd", ""] {
        let mut head = request();
        head.headers.push(Header::new("Racer-Delivery", value));
        assert!(matches!(parser.parse(&head), Err(Error::InvalidRequest)));
    }
    let mut head = request();
    head.headers.push(Header::new("Racer-Delivery", "mapped"));
    head.headers.push(Header::new("racer-delivery", "mapped"));
    assert!(matches!(parser.parse(&head), Err(Error::InvalidRequest)));
    let mut head = request();
    let StartLine::Request { method, .. } = &mut head.start else {
        unreachable!()
    };
    *method = "HEAD".into();
    head.headers.push(Header::new("Racer-Delivery", "mapped"));
    assert!(!parser.parse(&head).unwrap().mapped);

    let metadata = metadata(2 * PAGE_BYTES + 3);
    let range = ByteRange::From(PAGE_BYTES - 1)
        .resolve(metadata.length)
        .unwrap();
    let mapped = client::mapped_subscription_head(&metadata, Some(range)).unwrap();
    assert_eq!(
        mapped.unique("Content-Length").unwrap(),
        Some(b"108".as_slice())
    );
    assert_eq!(
        mapped.unique(client::DELIVERY_HEADER).unwrap(),
        Some(client::DELIVERY_MAPPED.as_bytes())
    );
    let inline = client::subscription_head(&metadata, Some(range)).unwrap();
    assert_eq!(inline.unique(client::DELIVERY_HEADER).unwrap(), None);
    assert_eq!(
        client::mapped_subscription_head(&self::metadata(0), None)
            .unwrap()
            .unique("Content-Length")
            .unwrap(),
        Some(b"21".as_slice())
    );
    assert_eq!(
        client::mapped_frame(1, 2, 3, 0x0102),
        [
            3, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 1, 2
        ]
    );
    assert_eq!(client::MAPPED_FRAME_BYTES, 29);
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
    use racer_object_wire::test_util::{AdapterOrigin, RequestKind};
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
        let (head, used) = http1::Codec::<()>::new(32768)
            .decode_head(&response)
            .unwrap()
            .unwrap();
        (head, response[used..].to_vec())
    };
    let (head, body) = exchange(false);
    assert_eq!(body, b"abc");
    assert_eq!(
        origin::validate_bootstrap(&head, &metadata.version.object),
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
