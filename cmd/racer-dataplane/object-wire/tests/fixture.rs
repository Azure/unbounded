#![cfg(feature = "test-util")]

use http1::MessageHead;
use racer_control_wire::CacheId;
use racer_object_wire::origin;
use racer_object_wire::test_util::{AdapterOrigin, RequestKind};
use racer_object_wire::*;
use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::time::Duration;

fn metadata(length: u64, etag: &str) -> ObjectMetadata {
    ObjectMetadata {
        content_type: None,
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value(etag),
        },
        length,
        expires_at: ExpiresAt::from_unix_millis(1234).unwrap(),
    }
}

fn exchange(fixture: &AdapterOrigin, method: &str, first: u64) -> (MessageHead, Vec<u8>) {
    exchange_with_pin(fixture, method, first, None)
}

fn exchange_with_pin(
    fixture: &AdapterOrigin,
    method: &str,
    first: u64,
    if_match: Option<&str>,
) -> (MessageHead, Vec<u8>) {
    let mut request = format!(
        "{method} /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n",
        CacheKey([0; 32]).to_hex()
    );
    if method == "GET" {
        request.push_str(&format!(
            "Range: bytes={first}-{}\r\n",
            first + PAGE_BYTES - 1
        ));
    }
    if let Some(etag) = if_match {
        request.push_str(&format!("If-Match: {etag}\r\n"));
    }
    request.push_str("\r\n");
    exchange_raw(fixture, request.as_bytes())
}

fn exchange_raw(fixture: &AdapterOrigin, request: &[u8]) -> (MessageHead, Vec<u8>) {
    let mut stream = UnixStream::connect(fixture.root.join("cache/origin/socket")).unwrap();
    stream
        .set_read_timeout(Some(Duration::from_secs(2)))
        .unwrap();
    stream
        .set_write_timeout(Some(Duration::from_secs(2)))
        .unwrap();
    stream.write_all(request).unwrap();
    let mut response = Vec::new();
    stream.read_to_end(&mut response).unwrap();
    let (head, used) = http1::Codec::<()>::new(32768)
        .decode_head(&response)
        .unwrap()
        .unwrap();
    (head, response[used..].to_vec())
}

#[test]
fn fixture_rejects_invalid_cache_names_before_filesystem_mutations() {
    const CHILD: &str = "RACER_OBJECT_WIRE_INVALID_CACHE_NAME_CHILD";
    if std::env::var_os(CHILD).is_none() {
        let status = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "fixture_rejects_invalid_cache_names_before_filesystem_mutations",
            ])
            .env(CHILD, "1")
            .status()
            .unwrap();
        assert!(status.success());
        return;
    }

    let target = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("target");
    let prefix = format!("adapter-origin-{}-", std::process::id());
    for name in [
        ".",
        "..",
        "",
        "/",
        "/cache",
        "cache/child",
        "./cache",
        "../cache",
        "cache/..",
        "cache/.",
        "cache/",
        "cache//",
        "cache\0name",
    ] {
        let panic = std::panic::catch_unwind(|| drop(AdapterOrigin::new(name, metadata(3, "v1"))))
            .expect_err("invalid cache name must panic");
        let message = panic
            .downcast_ref::<&str>()
            .copied()
            .or_else(|| panic.downcast_ref::<String>().map(String::as_str));
        assert_eq!(
            message,
            Some("cache name must be a single normal path component"),
            "cache name {name:?}"
        );
        match std::fs::read_dir(&target) {
            Ok(entries) => {
                for entry in entries {
                    assert!(
                        !entry
                            .unwrap()
                            .file_name()
                            .to_string_lossy()
                            .starts_with(&prefix),
                        "cache name {name:?} created a fixture directory"
                    );
                }
            }
            Err(error) => assert_eq!(error.kind(), std::io::ErrorKind::NotFound),
        }
    }
}

#[test]
fn fixture_accepts_normal_cache_names_and_cleans_up() {
    for name in ["cache", "cache-name_1.2", ".cache", "..cache"] {
        let fixture = AdapterOrigin::new(name, metadata(3, "v1"));
        let directory = fixture.root.canonicalize().unwrap();
        let socket = directory.join(name).join("origin/socket");
        assert!(socket.exists());
        UnixStream::connect(fixture.root.join(name).join("origin/socket")).unwrap();
        drop(fixture);
        assert!(!directory.exists());
    }
}

#[test]
fn fixture_default_body_matches_range_headers() {
    let fixture = AdapterOrigin::new("cache", metadata(3, "v1"));
    for (first, expected_body, expected_range) in [
        (0, b"abc".as_slice(), "bytes 0-2/3"),
        (PAGE_BYTES, b"".as_slice(), "bytes */3"),
    ] {
        let (head, body) = exchange_with_pin(&fixture, "GET", first, Some("\"v1\""));
        assert_eq!(body, expected_body, "offset {first}");
        assert_eq!(
            head.unique("Content-Length").unwrap(),
            Some(body.len().to_string().as_bytes()),
            "offset {first}"
        );
        assert_eq!(
            head.unique("Content-Range").unwrap(),
            Some(expected_range.as_bytes()),
            "offset {first}"
        );
        assert!(matches!(
            head.start,
            http1::StartLine::Response { status, .. }
                if status == if first == PAGE_BYTES { 416 } else { 206 }
        ));
    }
}

#[test]
fn fixture_errors_include_required_origin_headers() {
    for length in [0, 3, MAX_WIRE_INTEGER] {
        let fixture = AdapterOrigin::new("cache", metadata(length, "v1"));
        for (method, kind) in [
            ("HEAD", RequestKind::Head),
            ("GET", RequestKind::InitialGet),
        ] {
            for (status, expected) in [
                (405, Error::MethodNotAllowed),
                (416, Error::UnsatisfiableRangeWithLength(length)),
                (503, Error::Unavailable),
            ] {
                fixture.reject_next(kind, status);
                let (head, body) = exchange(&fixture, method, 0);
                assert!(body.is_empty());
                assert_eq!(origin::validate_response(&head, false), Err(expected));
                let mut malformed = head;
                if status == 405 || status == 416 {
                    let required = if status == 405 {
                        "Allow"
                    } else {
                        "Content-Range"
                    };
                    malformed
                        .headers
                        .retain(|h| !h.name.eq_ignore_ascii_case(required));
                    assert_eq!(
                        origin::validate_response(&malformed, false),
                        Err(Error::BadGateway)
                    );
                }
            }
        }
        fixture.reject_page(0, 416);
        let (head, body) = exchange(&fixture, "GET", 0);
        assert!(body.is_empty());
        assert_eq!(
            origin::validate_response(&head, false),
            Err(Error::UnsatisfiableRangeWithLength(length))
        );
    }

    let fixture = AdapterOrigin::new("cache", metadata(3, "v1"));
    let (head, body) = exchange_with_pin(&fixture, "GET", PAGE_BYTES, Some("\"v1\""));
    assert!(body.is_empty());
    assert_eq!(
        origin::validate_response(&head, false),
        Err(Error::UnsatisfiableRangeWithLength(3))
    );
}

#[test]
fn fixture_version_changes_discard_stale_body_but_refreshes_preserve_it() {
    let mut current = metadata(3, "v1");
    let fixture = AdapterOrigin::new("cache", current.clone());
    fixture.set_body(b"old".to_vec());
    current.expires_at = ExpiresAt::from_unix_millis(5678).unwrap();
    fixture.set_version(current.clone());
    let (head, body) = exchange(&fixture, "GET", 0);
    assert_eq!(body, b"old");
    assert_eq!(
        origin::validate_bootstrap(&head, &current.version.object),
        Ok((current.clone(), 3))
    );

    for (length, etag) in [(3, "v2"), (8, "v2"), (1, "v3"), (0, "v4")] {
        current = metadata(length, etag);
        fixture.set_version(current.clone());
        let (head, body) = exchange(&fixture, "GET", 0);
        let expected = if length == 3 {
            b"abc".to_vec()
        } else {
            vec![0; length as usize]
        };
        assert_eq!(body, expected);
        assert_eq!(
            origin::validate_bootstrap(&head, &current.version.object),
            Ok((current.clone(), length))
        );
        fixture.set_body(vec![b'x'; length as usize]);
        assert_eq!(exchange(&fixture, "GET", 0).1, vec![b'x'; length as usize]);
    }
}

#[test]
fn fixture_empty_explicit_body_does_not_slice_past_its_end() {
    let current = metadata(0, "empty");
    let fixture = AdapterOrigin::new("cache", current.clone());
    fixture.set_body(vec![]);
    let (head, body) = exchange_with_pin(&fixture, "GET", PAGE_BYTES, Some("\"empty\""));
    assert!(body.is_empty());
    assert_eq!(
        origin::validate_response(&head, true),
        Err(Error::UnsatisfiableRangeWithLength(0))
    );
}

#[test]
fn fixture_empty_pinned_get_rejects_ranges_after_checking_version() {
    let fixture = AdapterOrigin::new("cache", metadata(0, "empty"));
    fixture.set_body(vec![]);
    for first in [0, PAGE_BYTES] {
        for (pin, status, expected_range) in [
            ("\"empty\"", 416, Some(b"bytes */0".as_slice())),
            ("\"other\"", 412, None),
        ] {
            let (head, body) = exchange_with_pin(&fixture, "GET", first, Some(pin));
            assert!(body.is_empty());
            assert!(matches!(
                head.start,
                http1::StartLine::Response { status: actual, .. } if actual == status
            ));
            assert_eq!(
                head.unique("Content-Length").unwrap(),
                Some(b"0".as_slice())
            );
            assert_eq!(head.unique("Content-Range").unwrap(), expected_range);
            assert_eq!(
                origin::validate_response(&head, true),
                Err(if status == 416 {
                    Error::UnsatisfiableRangeWithLength(0)
                } else {
                    Error::VersionUnavailable
                })
            );
        }
    }
    assert_eq!(fixture.count(RequestKind::PinnedGet), 4);
}

#[test]
fn fixture_empty_bootstrap_and_head_remain_successful() {
    let current = metadata(0, "empty");
    let fixture = AdapterOrigin::new("cache", current.clone());
    for (method, pin) in [("GET", None), ("HEAD", None), ("HEAD", Some("\"empty\""))] {
        let (head, body) = exchange_with_pin(&fixture, method, 0, pin);
        assert!(body.is_empty());
        assert!(matches!(
            head.start,
            http1::StartLine::Response { status: 200, .. }
        ));
        assert_eq!(
            head.unique("Content-Length").unwrap(),
            Some(b"0".as_slice())
        );
        assert_eq!(head.unique("Content-Range").unwrap(), None);
        if method == "GET" {
            assert_eq!(
                origin::validate_bootstrap(&head, &current.version.object),
                Ok((current.clone(), 0))
            );
        } else {
            assert_eq!(
                origin::validate_metadata(
                    &head,
                    &current.version.object,
                    pin.map(|_| &current.version.etag),
                ),
                Ok(current.clone())
            );
        }
    }
}

fn raw_request(method: &str, fields: &str) -> String {
    format!(
        "{method} /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n{fields}\r\n",
        CacheKey([0; 32]).to_hex()
    )
}

#[test]
fn fixture_rejects_bad_envelopes_before_recording_or_consuming_scripts() {
    let fixture = AdapterOrigin::new("cache", metadata(3, "v1"));
    for kind in [
        RequestKind::Head,
        RequestKind::InitialGet,
        RequestKind::PinnedGet,
    ] {
        fixture.block(kind);
        fixture.reject_next(kind, 503);
    }
    let valid = raw_request("GET", "Range: bytes=0-16777215\r\n");
    let target = format!("/v1/objects/{}", CacheKey([0; 32]).to_hex());
    let mut invalid = vec![
        valid.replace(&target, "/wrong"),
        valid.replace(
            &target,
            &format!("/v1/objects/{}", CacheKey([1; 32]).to_hex()),
        ),
        valid.replace(&target, &format!("/v1/objects/{}", "A".repeat(64))),
        valid.replace(&target, &format!("{target}?query")),
        valid.replace(&target, &format!("http://racer{target}")),
        valid.replace(&target, &format!("/v1/objects/%30{}", "0".repeat(63))),
        valid.replace("Host: racer\r\n", ""),
        valid.replace("Host: racer", "Host: other"),
        valid.replace("Host: racer", "Host: racer:80"),
        valid.replace("Host: racer", "Host: RACER"),
        valid.replace("Host: racer", "Host: racer\r\nhOsT: racer"),
        valid.replace("Content-Length: 0", "Content-Length: 1"),
        valid.replace("Content-Length: 0", "Content-Length: 00"),
        valid.replace("Content-Length: 0", "Content-Length:  0"),
        valid.replace("Content-Length: 0", "Content-Length: 0 "),
        valid.replace(
            "Content-Length: 0",
            "Content-Length: 0\r\ncontent-length: 0",
        ),
        valid.replace("HTTP/1.1", "HTTP/1.0"),
        valid.replace("Host: racer", "Host : racer"),
        "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n".into(),
    ];
    for field in [
        "Transfer-Encoding: chunked",
        "Content-Encoding: identity",
        "Trailer: X",
        "Upgrade: h2c",
        "Connection: Upgrade",
        "Expect: 100-continue",
        "If-None-Match: *",
        "If-Modified-Since: date",
        "If-Unmodified-Since: date",
        "If-Range: \"v1\"",
        "Content-Range: bytes */3",
        "ETag: \"v1\"",
        "Racer-Expires-At: 0",
        "Racer-Content-Type: text/plain",
        "Range: bytes=0-16777215",
        "If-Match: \"v1\"\r\nif-match: \"v1\"",
        "Content-Type: text/plain\r\ncontent-type: text/plain",
        "Authorization: value\r\nauthorization: value",
        "Racer-Metadata: value\r\nracer-metadata: value",
        "Authorization:",
        "Authorization:  padded",
        "Authorization: padded ",
        "Authorization: tab\tvalue",
        "Racer-Metadata: padded ",
    ] {
        invalid.push(raw_request(
            "GET",
            &format!("Range: bytes=0-16777215\r\n{field}\r\n"),
        ));
    }
    for method in ["POST", "PUT", "get", "head", "OPTIONS"] {
        let (head, body) = exchange_raw(&fixture, raw_request(method, "").as_bytes());
        assert!(body.is_empty());
        assert_eq!(
            origin::validate_response(&head, false),
            Err(Error::MethodNotAllowed)
        );
    }
    for request in invalid {
        let (head, body) = exchange_raw(&fixture, request.as_bytes());
        assert!(body.is_empty());
        assert_eq!(
            origin::validate_response(&head, false),
            Err(Error::InvalidRequest),
            "{request:?}"
        );
        assert!(fixture.calls().is_empty());
    }
    for (method, pin, kind) in [
        ("HEAD", None, RequestKind::Head),
        ("GET", None, RequestKind::InitialGet),
        ("GET", Some("\"v1\""), RequestKind::PinnedGet),
    ] {
        assert_eq!(fixture.completed(kind), 0);
        fixture.release(kind);
        let (head, body) = exchange_with_pin(&fixture, method, 0, pin);
        assert!(body.is_empty());
        assert_eq!(
            origin::validate_response(&head, pin.is_some()),
            Err(Error::Unavailable)
        );
        assert_eq!(fixture.count(kind), 1);
        assert_eq!(fixture.completed(kind), 1);
    }
}

#[test]
fn fixture_rejects_malformed_ranges_and_pins_before_recording() {
    let fixture = AdapterOrigin::new("cache", metadata(3, "v1"));
    for pin in ["", "If-Match: \"v1\"\r\n"] {
        for range in [
            "",
            "bytes=-",
            "bytes=0-",
            "bytes=-3",
            "bytes=1-0",
            "bytes=0-garbage",
            "bytes=0-2-3",
            "bytes=0-2,4-5",
            "bytes=00-2",
            "bytes=0-02",
            "bytes=+0-2",
            "bytes=0- 2",
            "bytes=0-2 ",
            "Bytes=0-2",
            "bytes=0-9223372036854775808",
            "bytes=1-16777215",
            "bytes=2-16777215",
            "bytes=3-16777215",
            "bytes=0-16777216",
            "bytes=0-1",
            "bytes=0-3",
        ] {
            let request = raw_request("GET", &format!("Range: {range}\r\n{pin}"));
            let (head, body) = exchange_raw(&fixture, request.as_bytes());
            assert!(body.is_empty());
            assert_eq!(
                origin::validate_response(&head, false),
                Err(Error::InvalidRequest),
                "{request:?}"
            );
        }
        for (method, range) in [("HEAD", "Range: bytes=0-16777215\r\n"), ("GET", "")] {
            let (head, body) = exchange_raw(
                &fixture,
                raw_request(method, &format!("{range}{pin}")).as_bytes(),
            );
            assert!(body.is_empty());
            assert_eq!(
                origin::validate_response(&head, false),
                Err(Error::InvalidRequest)
            );
        }
    }
    for range in ["bytes=0-2", "bytes=16777216-33554431"] {
        let (head, _) = exchange_raw(
            &fixture,
            raw_request("GET", &format!("Range: {range}\r\n")).as_bytes(),
        );
        assert_eq!(
            origin::validate_response(&head, false),
            Err(Error::InvalidRequest)
        );
    }
    for pin in ["", "*", "W/\"v1\"", "\"v1\", \"v2\"", "unquoted"] {
        for method in ["HEAD", "GET"] {
            let range = if method == "GET" {
                "Range: bytes=0-16777215\r\n"
            } else {
                ""
            };
            let (head, _) = exchange_raw(
                &fixture,
                raw_request(method, &format!("{range}If-Match: {pin}\r\n")).as_bytes(),
            );
            assert_eq!(
                origin::validate_response(&head, false),
                Err(Error::InvalidRequest)
            );
        }
    }
    assert!(fixture.calls().is_empty());
    for kind in [
        RequestKind::Head,
        RequestKind::InitialGet,
        RequestKind::PinnedGet,
    ] {
        assert_eq!(fixture.completed(kind), 0);
    }
}

#[test]
fn fixture_accepts_complete_requests_and_short_final_pages() {
    let fixture = AdapterOrigin::new("cache", metadata(PAGE_BYTES + 3, "v1"));
    for last in [PAGE_BYTES + 2, 2 * PAGE_BYTES - 1] {
        let request = raw_request(
            "GET",
            &format!("Range: bytes={PAGE_BYTES}-{last}\r\nIf-Match: \"v1\"\r\n"),
        );
        let (head, body) = exchange_raw(&fixture, request.as_bytes());
        assert_eq!(body, [1; 3]);
        assert_eq!(origin::validate_response(&head, true), Ok((206, 3)));
        assert_eq!(
            head.unique("Content-Range").unwrap(),
            Some(format!("bytes {PAGE_BYTES}-{}/{}", PAGE_BYTES + 2, PAGE_BYTES + 3).as_bytes())
        );
    }
    let calls = fixture.calls();
    assert_eq!(calls.len(), 2);
    for call in calls {
        assert_eq!(call.kind, RequestKind::PinnedGet);
        assert_eq!(call.page, 1);
        assert_eq!(call.if_match.as_deref(), Some(b"\"v1\"".as_slice()));
    }
    let mut current = metadata(3, "a,b\\c");
    current.version.object.key = CacheKey([0xab; 32]);
    fixture.set_version(current.clone());
    for (method, range, pin, kind) in [
        ("HEAD", "", "", RequestKind::Head),
        ("HEAD", "", "If-Match: \"a,b\\c\"\r\n", RequestKind::Head),
        (
            "GET",
            "Range: bytes=0-16777215\r\n",
            "",
            RequestKind::InitialGet,
        ),
        (
            "GET",
            "Range: bytes=0-2\r\n",
            "If-Match: \"a,b\\c\"\r\n",
            RequestKind::PinnedGet,
        ),
    ] {
        let request = raw_request(
            method,
            &format!("{range}{pin}X-Unknown: allowed\r\nX-Unknown: duplicate\r\n"),
        )
        .replace(
            &CacheKey([0; 32]).to_hex(),
            &current.version.object.key.to_hex(),
        )
        .replace("Host: racer", "hOsT: \tracer \t")
        .replace("Content-Length: 0\r\n", "");
        let mut request = request.into_bytes();
        request.truncate(request.len() - 2);
        request
            .extend_from_slice(b"Authorization: Bearer \xff\r\nRacer-Metadata: a,\\b\x80\r\n\r\n");
        let (head, body) = exchange_raw(&fixture, &request);
        let expected_length = if method == "HEAD" { 0 } else { 3 };
        assert_eq!(body.len(), expected_length);
        assert_eq!(
            origin::validate_response(&head, !pin.is_empty()),
            Ok((if method == "HEAD" { 200 } else { 206 }, 3))
        );
        let calls = fixture.calls();
        let call = calls.last().unwrap();
        assert_eq!(call.kind, kind);
        assert_eq!(call.page, 0);
        assert_eq!(
            call.if_match.as_deref(),
            (!pin.is_empty()).then_some(current.version.etag.as_bytes())
        );
    }
}

#[test]
fn fixture_bounds_request_heads_fields_and_highest_page() {
    let fixture = AdapterOrigin::new("cache", metadata(3, "v1"));
    for length in [8192, 8193] {
        for field in ["Authorization", "Racer-Metadata"] {
            let request = raw_request("HEAD", &format!("{field}: {}\r\n", "x".repeat(length)));
            let (head, body) = exchange_raw(&fixture, request.as_bytes());
            assert!(body.is_empty());
            assert_eq!(
                origin::validate_response(&head, false),
                if length == 8192 {
                    Ok((200, 3))
                } else {
                    Err(Error::HeaderTooLarge)
                }
            );
        }
    }
    let request = raw_request("HEAD", "X-Padding: \r\n");
    for length in [32768, 32769] {
        let request = request.replace(
            "X-Padding: ",
            &format!("X-Padding: {}", "x".repeat(length - request.len())),
        );
        assert_eq!(request.len(), length);
        let (head, body) = exchange_raw(&fixture, request.as_bytes());
        assert!(body.is_empty());
        assert_eq!(
            origin::validate_response(&head, false),
            if length == 32768 {
                Ok((200, 3))
            } else {
                Err(Error::HeaderTooLarge)
            }
        );
    }
    assert_eq!(fixture.count(RequestKind::Head), 3);
    let first = MAX_WIRE_INTEGER / PAGE_BYTES * PAGE_BYTES;
    let request = raw_request(
        "GET",
        &format!("Range: bytes={first}-{MAX_WIRE_INTEGER}\r\nIf-Match: \"v1\"\r\n"),
    );
    let (head, body) = exchange_raw(&fixture, request.as_bytes());
    assert!(body.is_empty());
    assert_eq!(
        origin::validate_response(&head, true),
        Err(Error::UnsatisfiableRangeWithLength(3))
    );
    assert_eq!(fixture.calls().last().unwrap().page, first / PAGE_BYTES);
}
