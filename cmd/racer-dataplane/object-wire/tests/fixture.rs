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
    let mut stream = UnixStream::connect(fixture.root.join("cache/origin/socket")).unwrap();
    stream
        .set_read_timeout(Some(Duration::from_secs(2)))
        .unwrap();
    stream
        .set_write_timeout(Some(Duration::from_secs(2)))
        .unwrap();
    write!(stream, "{method} /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRange: bytes={first}-\r\n\r\n", CacheKey([0; 32]).to_hex()).unwrap();
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
        (1, b"bc".as_slice(), "bytes 1-2/3"),
        (2, b"c".as_slice(), "bytes 2-2/3"),
        (3, b"".as_slice(), "bytes */3"),
    ] {
        let (head, body) = exchange(&fixture, "GET", first);
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
                if status == if first == 3 { 416 } else { 206 }
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
    let (head, body) = exchange(&fixture, "GET", PAGE_BYTES);
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
    let (head, body) = exchange(&fixture, "GET", PAGE_BYTES);
    assert!(body.is_empty());
    assert_eq!(
        origin::validate_bootstrap(&head, &current.version.object),
        Ok((current, 0))
    );
}
