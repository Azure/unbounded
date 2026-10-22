//! Direct-read (`host-read`, `gpu-read`) request parsing and response heads.

use http1::{Header, MessageHead, StartLine};
use racer_control_wire::CacheId;
use racer_object_wire::client::{self, Destination, DirectDelivery, ReadKind, RequestParser};
use racer_object_wire::*;

fn key() -> String {
    CacheKey([0xab; 32]).to_hex()
}

/// A direct request with valid destination fields plus `extra`.
fn direct(method: &str, suffix: &str, extra: &[(&str, &str)]) -> MessageHead {
    let mut headers = vec![
        Header::new("Host", "racer"),
        Header::new("Content-Length", "0"),
    ];
    headers.extend(extra.iter().map(|(name, value)| Header::new(*name, *value)));
    MessageHead {
        start: StartLine::Request {
            method: method.into(),
            target: format!("/v1/objects/{}/{suffix}", key()),
        },
        headers,
    }
}

fn parse(head: &MessageHead) -> Result<ReadKind> {
    RequestParser::new(usize::MAX)
        .parse(head)
        .map(|request| request.kind)
}

const DEST: [(&str, &str); 2] = [
    ("Racer-Destination-Offset", "4096"),
    ("Racer-Destination-Length", "100"),
];

#[test]
fn host_read_parses_range_pin_and_destination() {
    let head = direct(
        "POST",
        "host-read",
        &[
            DEST[0],
            DEST[1],
            ("Range", "bytes=1-3"),
            ("If-Match", "\"v1\""),
            // Credits are ignored, even when they would be invalid on a Get.
            ("Racer-Page-Credits", "0"),
            ("Racer-Byte-Credits", "x"),
            ("Racer-Allocation-Size", "junk"),
        ],
    );
    let request = RequestParser::new(usize::MAX).parse(&head).unwrap();
    assert_eq!(request.key, CacheKey([0xab; 32]));
    let kind = request.kind;
    assert_eq!(
        kind,
        ReadKind::HostRead {
            pin: Some(StrongEtag::parse(b"\"v1\"").unwrap()),
            range: Some(ByteRange::closed(1, 3).unwrap()),
            destination: Destination {
                offset: 4096,
                length: 100
            },
        }
    );
    assert_eq!(kind.direct(), Some(DirectDelivery::Host));
    assert_eq!(kind.destination().unwrap().end(), 4196);
    assert_eq!(kind.range(), Some(ByteRange::closed(1, 3).unwrap()));
    assert!(kind.pin().is_some());
    assert!(!kind.is_head());
}

#[test]
fn gpu_read_requires_allocation_that_holds_the_window() {
    let parsed = parse(&direct(
        "POST",
        "gpu-read",
        &[DEST[0], DEST[1], ("Racer-Allocation-Size", "4196")],
    ))
    .unwrap();
    assert_eq!(
        parsed,
        ReadKind::GpuRead {
            pin: None,
            range: None,
            destination: Destination {
                offset: 4096,
                length: 100
            },
            allocation_size: 4196,
        }
    );
    assert_eq!(parsed.direct(), Some(DirectDelivery::Gpu));
    for extra in [
        &[DEST[0], DEST[1]][..],
        &[DEST[0], DEST[1], ("Racer-Allocation-Size", "4195")],
        &[DEST[0], DEST[1], ("Racer-Allocation-Size", "-1")],
        &[
            DEST[0],
            DEST[1],
            ("Racer-Allocation-Size", "9999"),
            ("Racer-Allocation-Size", "9999"),
        ],
    ] {
        assert_eq!(
            parse(&direct("POST", "gpu-read", extra)),
            Err(Error::InvalidRequest),
            "{extra:?}"
        );
    }
}

#[test]
fn destination_fields_are_required_canonical_unique_and_bounded() {
    let max = i64::MAX.to_string();
    for suffix in ["host-read", "gpu-read"] {
        let size = ("Racer-Allocation-Size", max.as_str());
        for extra in [
            &[size][..],
            &[size, DEST[0]],
            &[size, DEST[1]],
            &[size, DEST[0], ("Racer-Destination-Length", "0")],
            &[size, DEST[0], ("Racer-Destination-Length", "01")],
            &[size, DEST[1], ("Racer-Destination-Offset", " 1")],
            &[
                size,
                DEST[1],
                ("Racer-Destination-Offset", "9223372036854775808"),
            ],
            &[size, DEST[0], DEST[1], DEST[1]],
            &[size, DEST[0], DEST[0], DEST[1]],
            // offset + length must stay inside a wire integer.
            &[
                size,
                ("Racer-Destination-Offset", max.as_str()),
                ("Racer-Destination-Length", "1"),
            ],
        ] {
            assert_eq!(
                parse(&direct("POST", suffix, extra)),
                Err(Error::InvalidRequest),
                "{suffix} {extra:?}"
            );
        }
        let ok = parse(&direct(
            "POST",
            suffix,
            &[
                size,
                ("Racer-Destination-Offset", "0"),
                ("Racer-Destination-Length", max.as_str()),
            ],
        ))
        .unwrap();
        assert_eq!(ok.destination().unwrap().end(), i64::MAX as u64);
    }
}

#[test]
fn direct_paths_require_post_with_zero_length_and_known_suffix() {
    for method in ["GET", "HEAD", "PUT"] {
        assert_eq!(
            parse(&direct(method, "host-read", &DEST)),
            Err(Error::MethodNotAllowed)
        );
    }
    let mut head = direct("POST", "host-read", &DEST);
    head.headers.retain(|h| h.name != "Content-Length");
    assert_eq!(parse(&head), Err(Error::InvalidRequest));
    for suffix in ["cpu-read", "host-read/", "", "HOST-READ"] {
        assert_eq!(
            parse(&direct("POST", suffix, &DEST)),
            Err(Error::InvalidRequest),
            "{suffix}"
        );
    }
    // The key is still validated on direct paths.
    let mut head = direct("POST", "host-read", &DEST);
    head.start = StartLine::Request {
        method: "POST".into(),
        target: format!("/v1/objects/{}/host-read", "A".repeat(64)),
    };
    assert_eq!(parse(&head), Err(Error::InvalidRequest));
}

#[test]
fn plain_requests_ignore_destination_fields_as_before() {
    let mut head = direct("POST", "", &[]);
    head.start = StartLine::Request {
        method: "POST".into(),
        target: format!("/v1/objects/{}", key()),
    };
    let plain = parse(&head).unwrap();
    head.headers.extend([
        Header::new("Racer-Destination-Offset", "x"),
        Header::new("Racer-Destination-Offset", "y"),
        Header::new("Racer-Allocation-Size", "z"),
    ]);
    assert_eq!(parse(&head).unwrap(), plain);
    assert!(matches!(plain, ReadKind::Subscription { .. }));
    assert_eq!(plain.direct(), None);
    assert_eq!(plain.destination(), None);
}

fn metadata(length: u64) -> ObjectMetadata {
    ObjectMetadata {
        content_type: None,
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

#[test]
fn direct_head_counts_only_frames_and_names_delivery() {
    let length = 2 * PAGE_BYTES + 5;
    for (range, delivery, pages) in [
        (Some((0, length)), DirectDelivery::Host, 3),
        (
            Some((PAGE_BYTES - 1, PAGE_BYTES + 1)),
            DirectDelivery::Gpu,
            2,
        ),
        (Some((3, 4)), DirectDelivery::Host, 1),
    ] {
        let range = range.map(|(start, end)| {
            ByteRange::closed(start, end - 1)
                .unwrap()
                .resolve(length)
                .unwrap()
        });
        let head = client::direct_head(&metadata(length), range, delivery).unwrap();
        let subscription = client::subscription_head(&metadata(length), range).unwrap();
        assert!(matches!(head.start, StartLine::Response { status: 200 }));
        assert_eq!(
            head.unique("Content-Length").unwrap(),
            Some((21 * (pages + 1)).to_string().as_bytes())
        );
        assert_eq!(
            head.unique("Racer-Delivery").unwrap(),
            Some(delivery.as_str().as_bytes())
        );
        assert_eq!(
            head.unique("Connection").unwrap(),
            Some(b"close".as_slice())
        );
        // Every other field matches the Get head.
        for field in &subscription.headers {
            if !field.name.eq_ignore_ascii_case("Content-Length") {
                assert_eq!(
                    head.unique(&field.name).unwrap(),
                    Some(field.value.as_slice())
                );
            }
        }
        assert_eq!(head.headers.len(), subscription.headers.len() + 1);
    }
    let empty = client::direct_head(&metadata(0), None, DirectDelivery::Host).unwrap();
    assert_eq!(
        empty.unique("Content-Length").unwrap(),
        Some(b"21".as_slice())
    );
    // A maximum-length object fits because frames carry no payload.
    let range = ByteRange::From(0).resolve(MAX_WIRE_INTEGER).unwrap();
    assert!(
        client::direct_head(
            &metadata(MAX_WIRE_INTEGER),
            Some(range),
            DirectDelivery::Host
        )
        .is_ok()
    );
    assert!(client::subscription_head(&metadata(MAX_WIRE_INTEGER), Some(range)).is_err());
}
