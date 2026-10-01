//! Raw wire failure assertions formerly in client_origin_conformance, now using
//! the same real coordinator/worker as the listener delivery scenarios.
use super::*;

#[test]
fn raw_uds_late_rust_acquisition_failure_never_appends_second_status() {
    use crate::model::PAGE_BYTES;
    let (fixture, pipes) = body_fixture_with_large_page(16, false, true);
    let metadata = ObjectMetadata {
        content_type: None,
        version: ObjectVersion {
            object: crate::model::ObjectId {
                cache: definition().id,
                key: crate::model::CacheKey([0; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        length: 3 * PAGE_BYTES + 13,
        expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
    };
    let worker = fixture.worker.as_ref().unwrap();
    worker.origin.set_version(metadata.clone());
    worker.origin.set_body(vec![b'x'; metadata.length as usize]);
    let mut socket = fixture.connect();
    socket.write_all(&request("POST", "If-Match: \"v1\"\r\nRange: bytes=0-\r\nRacer-Page-Credits: 1\r\nRacer-Byte-Credits: 16777216\r\nRacer-Ordered: 1\r\n")).unwrap();
    let mut raw = fixture.receive(&mut socket, false);
    let end = raw.windows(4).position(|part| part == b"\r\n\r\n").unwrap() + 4;
    let deadline = Instant::now() + Duration::from_secs(10);
    while raw.len() < end + PAGE_BYTES as usize + 21 {
        fixture.pump(64);
        let mut bytes = [0; 65536];
        match socket.read(&mut bytes) {
            Ok(0) => panic!("first page truncated"),
            Ok(n) => raw.extend_from_slice(&bytes[..n]),
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
            Err(error) => panic!("{error}"),
        }
        assert!(Instant::now() < deadline);
    }
    let mut release = [0; 12];
    release[8..].copy_from_slice(&(PAGE_BYTES as u32).to_be_bytes());
    socket.write_all(&release).unwrap();
    raw.extend(fixture.receive(&mut socket, true));
    let head = std::str::from_utf8(&raw[..end])
        .unwrap()
        .to_ascii_lowercase();
    assert!(head.starts_with("http/1.1 200 "));
    assert!(head.contains("content-length: 50331766\r\n"));
    assert!(head.contains("racer-object-length: 50331661\r\n"));
    assert!(head.contains("racer-range-start: 0\r\n"));
    assert!(head.contains("racer-range-end: 50331661\r\n"));
    assert!(!head.contains("content-range:"));
    assert_eq!(raw.len() - end, PAGE_BYTES as usize + 21);
    let mut frame = [0; 21];
    frame[0] = 1;
    frame[17..].copy_from_slice(&(PAGE_BYTES as u32).to_be_bytes());
    assert_eq!(&raw[end..end + 21], &frame);
    assert!(raw[end + 21..].iter().all(|byte| *byte == b'x'));
    assert_eq!(
        raw.windows(8).filter(|part| *part == b"HTTP/1.1").count(),
        1
    );
    assert_only_idle_pipes(&fixture, &pipes);
}

#[test]
fn coordinator_enforces_pinned_head_and_unsatisfiable_range_length() {
    // The assembled coordinator enforces the selected version for HEAD and
    // attaches the selected length to both empty and nonempty invalid ranges.
    for (length, fields, status) in [
        (4, "", "200"),
        (4, "If-Match: \"v1\"\r\n", "200"),
        (4, "If-Match: \"old\"\r\n", "412"),
        (4, "Range: bytes=4-\r\n", "416"),
        (0, "Range: bytes=0-\r\n", "416"),
    ] {
        let (fixture, _pipes) = body_fixture(16, false);
        let worker = fixture.worker.as_ref().unwrap();
        worker.origin.set_version(ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: crate::model::ObjectId {
                    cache: definition().id,
                    key: crate::model::CacheKey([0; 32]),
                },
                etag: StrongEtag::parse(b"\"v1\"").unwrap(),
            },
            length,
            expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
        });
        let method = if status == "416" { "POST" } else { "HEAD" };
        let mut socket = fixture.connect();
        socket
            .write_all(&request(method, &format!("{fields}Connection: close\r\n")))
            .unwrap();
        let response = String::from_utf8(fixture.receive(&mut socket, true)).unwrap();
        assert!(
            response.starts_with(&format!("HTTP/1.1 {status} ")),
            "{response}"
        );
        if status == "416" {
            assert!(
                response.contains(&format!("Content-Range: bytes */{length}\r\n")),
                "{response}"
            );
        }
    }
}

#[test]
fn raw_uds_first_rust_acquisition_failure_returns_complete_503() {
    let (fixture, pipes) = body_fixture(16, true);
    let mut socket = start_body(&fixture);
    let text = String::from_utf8(fixture.receive(&mut socket, true))
        .unwrap()
        .to_ascii_lowercase();
    assert!(text.starts_with("http/1.1 503 "));
    assert!(text.contains("content-length: 0\r\n"));
    assert!(!text.contains("content-range:"));
    assert_eq!(text.matches("http/1.1").count(), 1);
    assert_eq!(text.find("\r\n\r\n").unwrap() + 4, text.len());
    assert_only_idle_pipes(&fixture, &pipes);
}
