//! Independent v1 wire checks. Expectations come from CLIENT_ORIGIN_API.md.
//! These exercise production HTTP/parser/writer components over real Unix sockets;
//! they do not substitute for an Application/Coordinator end-to-end deployment.
use racer_dataplane::{
    client::{
        request::{ClientRequest, ReadKind, RequestParser},
        response::Responses,
    },
    error::{Error, Result},
    http::{
        codec::{Codec, Header, MessageHead, StartLine},
        io::HttpIo,
        pool::ConnectionLease,
    },
    memory::{delivery::Delivery, pipe::PipePool},
    model::{
        identity::{
            CacheId, CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, RequestId, StrongEtag,
        },
        limits::Limits,
        metadata::{ExpiresAt, ObjectMetadata},
        range::ByteRange,
    },
    origin::{metadata, page},
    read::serve::ReadResponse,
    runtime::{
        admission::Admission,
        deadline::RequestScope,
        reactor::{IoBuffer, Reactor},
    },
};
use std::{
    future::Future,
    io::{Read, Write},
    num::NonZeroUsize,
    os::unix::net::UnixStream,
    rc::Rc,
    task::{Context, Poll},
    thread,
    time::{Duration, Instant, UNIX_EPOCH},
};

const LIMIT: usize = 32768;
const P: u64 = 16777216;
const MAX: u64 = 9223372036854775807;

struct Rig {
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
    io: Rc<HttpIo>,
}
impl Rig {
    fn new() -> Self {
        let n = NonZeroUsize::new(32).unwrap();
        let bytes = NonZeroUsize::new(4 * P as usize).unwrap();
        let admission = Rc::new(Admission::new(Limits {
            plaintext_bytes: bytes,
            ciphertext_bytes: bytes,
            dirty_bytes: bytes,
            registered_bytes: bytes,
            request_context_bytes: bytes,
            flights: n,
            waiters_per_flight: n,
            queue_entries: n,
            connections_per_neighbor: n,
            client_connections: n,
            pipes: n,
            range_window_pages: n,
            replay_entries: n,
            header_bytes: NonZeroUsize::new(LIMIT).unwrap(),
            route_search_work: n,
            cached_rankings: n,
            cached_paths: n,
            retained_snapshots: n,
            metadata_entries: n,
            relay_transfers: n,
        }));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            Codec::new(LIMIT, MAX),
            admission.clone(),
        ));
        Self {
            admission,
            reactor,
            io,
        }
    }
    fn lease(&self, socket: UnixStream) -> ConnectionLease {
        ConnectionLease::from_accepted(socket.into(), &self.admission).unwrap()
    }
    fn drive<T>(&self, future: impl Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let until = Instant::now() + Duration::from_secs(10);
        loop {
            if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                return result;
            }
            assert!(Instant::now() < until, "wire operation stalled");
            self.reactor.poll_budgeted(128).unwrap();
            self.reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
    fn responses(&self) -> Responses {
        Responses::new(
            self.io.clone(),
            Rc::new(Delivery::new(
                Rc::new(PipePool::new(self.admission.clone(), self.reactor.clone())),
                Duration::from_secs(5),
            )),
        )
    }
}
fn scope() -> RequestScope {
    RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(8)).unwrap()
}
fn object() -> ObjectId {
    ObjectId {
        cache: CacheId("conformance".into()),
        key: CacheKey([0xab; 32]),
    }
}
fn request(method: &str, fields: &[u8]) -> Vec<u8> {
    let mut raw = format!(
        "{method} /v1/objects/{} HTTP/1.1\r\nHost: racer\r\n",
        "ab".repeat(32)
    )
    .into_bytes();
    raw.extend_from_slice(fields);
    raw.extend_from_slice(b"\r\n");
    raw
}
fn raw_request(raw: Vec<u8>) -> Result<ClientRequest> {
    let rig = Rig::new();
    let (local, mut peer) = UnixStream::pair().unwrap();
    let writer = thread::spawn(move || {
        let _ = peer.write_all(&raw);
    });
    let result = rig.drive(async {
        let head = rig.io.receive_head(rig.lease(local), &scope()).await?;
        RequestParser::new(LIMIT).parse(&object().cache, head.value)
    });
    writer.join().unwrap();
    result
}
fn reject(raw: Vec<u8>, expected: Error) {
    assert!(
        matches!(raw_request(raw), Err(error) if error == expected),
        "wrong request rejection: {expected:?}"
    );
}

#[test]
fn raw_uds_exact_targets_methods_and_bodyless_framing() {
    let canonical = format!("/v1/objects/{}", "ab".repeat(32));
    for target in [
        format!("{canonical}?"),
        format!("{canonical}#x"),
        format!("{canonical}/"),
        canonical.replace("ab", "AB"),
        canonical.replace("ab", "%61b"),
        format!("http://racer{canonical}"),
        canonical.replace("objects/", "objects//"),
    ] {
        reject(
            format!("HEAD {target} HTTP/1.1\r\nHost: racer\r\n\r\n").into_bytes(),
            Error::InvalidRequest,
        );
    }
    reject(request("POST", b""), Error::MethodNotAllowed);
    reject(request("GET", b""), Error::InvalidRequest);
    reject(
        request("HEAD", b"Range: bytes=0-1\r\n"),
        Error::InvalidRequest,
    );
    for field in [
        "Content-Length: 1",
        "Content-Length: 00",
        "Content-Length: 0, 0",
        "Transfer-Encoding: identity",
        "Content-Encoding: identity",
        "Expect: 100-continue",
        "Trailer: X",
        "Upgrade: h2c",
        "If-None-Match: *",
        "If-Modified-Since: x",
        "If-Unmodified-Since: x",
        "If-Range: x",
        "X: first\r\n folded",
    ] {
        reject(
            request("HEAD", format!("{field}\r\n").as_bytes()),
            Error::InvalidRequest,
        );
    }
    assert!(raw_request(request("HEAD", b"Content-Length: 0\r\n")).is_ok());
    reject(
        b"HEAD / HTTP/1.0\r\nHost: racer\r\n\r\n".to_vec(),
        Error::InvalidRequest,
    );
}

#[test]
fn raw_uds_all_singletons_reject_identical_case_insensitive_repeats() {
    for (name, value) in [
        ("Host", "racer"),
        ("Content-Length", "0"),
        ("Content-Type", "application/octet-stream"),
        ("Content-Range", "bytes 0-1/2"),
        ("ETag", "\"v\""),
        ("If-Match", "\"v\""),
        ("Range", "bytes=0-16777215"),
        ("Racer-Expires-At", "0"),
        ("Racer-Metadata", "opaque"),
        ("Authorization", "opaque"),
    ] {
        let fields = format!(
            "{name}: {value}\r\n{}: {value}\r\n",
            name.to_ascii_lowercase()
        );
        reject(request("HEAD", fields.as_bytes()), Error::InvalidRequest);
    }
}

#[test]
fn raw_uds_opaque_values_preserve_every_allowed_byte_and_exact_separator() {
    let mut value = b"literal,  spaces\\".to_vec();
    value.extend(0x80..=0xff);
    for name in ["Authorization", "Racer-Metadata"] {
        let mut fields = format!("{name}: ").into_bytes();
        fields.extend_from_slice(&value);
        fields.extend_from_slice(b"\r\n");
        let parsed = raw_request(request("HEAD", &fields)).unwrap();
        let actual = if name == "Authorization" {
            parsed
                .origin
                .authorization
                .as_ref()
                .unwrap()
                .expose_for_origin()
                .unwrap()
        } else {
            parsed
                .origin
                .metadata
                .as_ref()
                .unwrap()
                .as_header()
                .unwrap()
        };
        assert_eq!(actual, value);
        for invalid in [
            b"".as_slice(),
            b"x",
            b"  x",
            b"\tx",
            b" x ",
            b" x\t",
            b" x\0",
            b" x\x7f",
            b" a\tb",
        ] {
            let mut fields = format!("{name}:").into_bytes();
            fields.extend_from_slice(invalid);
            fields.extend_from_slice(b"\r\n");
            reject(request("HEAD", &fields), Error::InvalidRequest);
        }
        for length in [1, 8192, 8193] {
            let fields = format!("{name}: {}\r\n", "x".repeat(length));
            let result = raw_request(request("HEAD", fields.as_bytes()));
            if length <= 8192 {
                assert!(result.is_ok());
            } else {
                assert!(matches!(result, Err(Error::HeaderTooLarge)));
            }
        }
    }
}

#[test]
fn raw_uds_head_limit_counts_start_line_and_final_crlf_exactly() {
    for length in [LIMIT - 1, LIMIT, LIMIT + 1] {
        let base = request("HEAD", b"X: \r\n").len();
        let raw = request(
            "HEAD",
            format!("X: {}\r\n", "x".repeat(length - base)).as_bytes(),
        );
        assert_eq!(raw.len(), length);
        let result = raw_request(raw);
        if length <= LIMIT {
            assert!(result.is_ok(), "valid {length}-byte head rejected");
        } else {
            assert!(matches!(result, Err(Error::HeaderTooLarge)));
        }
    }
}

#[test]
fn raw_uds_head_limit_does_not_invent_separator_bytes_for_unknown_fields() {
    // Unknown fields need not use the opaque fields' mandatory separator SP.
    let base = request("HEAD", b"X:\r\n").len();
    let raw = request(
        "HEAD",
        format!("X:{}\r\n", "x".repeat(LIMIT - base)).as_bytes(),
    );
    assert_eq!(raw.len(), LIMIT);
    assert!(
        raw_request(raw).is_ok(),
        "32 KiB raw head was counted after reserialization"
    );
}

#[test]
fn raw_uds_pinned_head_tags_and_maxint64_ranges() {
    for tag in ["\"\"", "\"a,b\\c\""] {
        let parsed =
            raw_request(request("HEAD", format!("If-Match: {tag}\r\n").as_bytes())).unwrap();
        assert!(
            matches!(parsed.kind, ReadKind::HeadPinned { etag } if etag.as_bytes() == tag.as_bytes())
        );
    }
    for tag in ["*", "W/\"v\"", "\"a\", \"b\"", "\"a b\"", "\"a\"b\""] {
        reject(
            request("HEAD", format!("If-Match: {tag}\r\n").as_bytes()),
            Error::InvalidRequest,
        );
    }
    for range in [
        "bytes=9223372036854775807-",
        "bytes=-0",
        "bytes=0-9223372036854775807",
    ] {
        assert!(
            raw_request(request(
                "GET",
                format!("If-Match: \"v\"\r\nRange: {range}\r\n").as_bytes()
            ))
            .is_ok()
        );
    }
    for range in [
        "bytes=01-2",
        "bytes=+1-2",
        "bytes=1-0",
        "bytes=0-1,2-3",
        "bytes=0 -1",
        "bytes=9223372036854775808-",
        "bytes=-",
    ] {
        assert!(
            raw_request(request(
                "GET",
                format!("If-Match: \"v\"\r\nRange: {range}\r\n").as_bytes()
            ))
            .is_err()
        );
    }
    for (range, size, first, end) in [
        (ByteRange::From(3), 9, 3, 9),
        (ByteRange::Suffix(99), 9, 0, 9),
        (
            ByteRange::Closed {
                first: 3,
                last: MAX,
            },
            9,
            3,
            9,
        ),
    ] {
        let resolved = range.resolve(size).unwrap();
        assert_eq!((resolved.start(), resolved.end()), (first, end));
    }
    for (range, size) in [
        (ByteRange::Suffix(0), 9),
        (ByteRange::From(9), 9),
        (ByteRange::From(0), 0),
    ] {
        assert!(matches!(
            range.resolve(size),
            Err(Error::UnsatisfiableRange)
        ));
    }
}

fn raw_response(raw: Vec<u8>, method: &str) -> Result<MessageHead> {
    let rig = Rig::new();
    let (local, mut peer) = UnixStream::pair().unwrap();
    peer.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
    let writer = thread::spawn(move || {
        let mut head = Vec::new();
        while !head.ends_with(b"\r\n\r\n") {
            let mut b = [0];
            peer.read_exact(&mut b).unwrap();
            head.push(b[0]);
        }
        let _ = peer.write_all(&raw);
    });
    let result = rig.drive(async {
        let scope = scope();
        let head = MessageHead {
            start: StartLine::Request {
                method: method.into(),
                target: format!("/v1/objects/{}", "ab".repeat(32)),
            },
            headers: vec![Header {
                name: "Host".into(),
                value: b"racer".to_vec(),
            }],
        };
        let sent = rig.io.send_head(rig.lease(local), head, &scope).await?;
        Ok(rig.io.receive_head(sent.connection, &scope).await?.value)
    });
    writer.join().unwrap();
    result
}
fn head_response(fields: &str) -> Vec<u8> {
    format!("HTTP/1.1 200 OK\r\n{fields}\r\n").into_bytes()
}

#[test]
fn raw_uds_origin_metadata_canonical_numbers_and_strong_etags() {
    for number in ["0", "9223372036854775807"] {
        let head = raw_response(
            head_response(&format!(
                "Content-Length: {number}\r\nETag: \"\"\r\nRacer-Expires-At: {number}\r\n"
            )),
            "HEAD",
        )
        .unwrap();
        let metadata = metadata::validate(&head, &object()).unwrap();
        assert_eq!(metadata.length.to_string(), number);
        assert_eq!(
            metadata.expires_at.to_unix_millis().unwrap().to_string(),
            number
        );
    }
    for value in ["00", "+1", "-1", "9223372036854775808", "1.0"] {
        for field in ["Content-Length", "Racer-Expires-At"] {
            let (length, expiry) = if field == "Content-Length" {
                (value, "0")
            } else {
                ("0", value)
            };
            let result = raw_response(
                head_response(&format!(
                    "Content-Length: {length}\r\nETag: \"v\"\r\nRacer-Expires-At: {expiry}\r\n"
                )),
                "HEAD",
            )
            .and_then(|head| metadata::validate(&head, &object()));
            assert!(result.is_err(), "accepted {field}: {value}");
        }
    }
    for tag in ["W/\"v\"", "*", "\"a\",\"b\"", "\"a b\""] {
        let head = raw_response(
            head_response(&format!(
                "Content-Length: 0\r\nETag: {tag}\r\nRacer-Expires-At: 0\r\n"
            )),
            "HEAD",
        )
        .unwrap();
        assert_eq!(metadata::validate(&head, &object()), Err(Error::BadGateway));
    }
}

#[test]
fn raw_uds_origin_expiry_rejects_noncanonical_whitespace() {
    for expiry in [" 0", "0 ", "\t0", "0\t"] {
        let head = raw_response(
            head_response(&format!(
                "Content-Length: 0\r\nETag: \"v\"\r\nRacer-Expires-At: {expiry}\r\n"
            )),
            "HEAD",
        )
        .unwrap();
        assert_eq!(
            metadata::validate(&head, &object()),
            Err(Error::BadGateway),
            "expiry whitespace normalized"
        );
    }
}

#[test]
fn raw_uds_origin_bootstrap_and_aligned_final_page() {
    let empty = raw_response(head_response("Content-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n"), "GET").unwrap();
    assert_eq!(
        metadata::validate_bootstrap(&empty, &object()).unwrap().1,
        0
    );
    let page = PageId {
        version: ObjectVersion {
            object: object(),
            etag: StrongEtag::parse(b"\"v\"").unwrap(),
        },
        number: PageNumber(1),
    };
    for (status, range, tag, length, valid) in [
        (206, "bytes 16777216-16777218/16777219", "\"v\"", 3, true),
        (200, "bytes 16777216-16777218/16777219", "\"v\"", 3, false),
        (206, "bytes 16777217-16777218/16777219", "\"v\"", 2, false),
        (206, "bytes 16777216-16777217/16777219", "\"v\"", 2, false),
        (206, "bytes 16777216-16777218/16777219", "\"new\"", 3, false),
    ] {
        let raw = format!("HTTP/1.1 {status} Result\r\nContent-Length: {length}\r\nContent-Type: application/octet-stream\r\nContent-Range: {range}\r\nETag: {tag}\r\nRacer-Expires-At: 0\r\n\r\n").into_bytes();
        let head = raw_response(raw, "GET").unwrap();
        assert_eq!(page::validate(&head, &page, length).is_ok(), valid);
        if valid {
            assert_eq!(
                page::validate(&head, &page, length - 1),
                Err(Error::BadGateway)
            );
        }
    }
}

#[test]
fn raw_uds_origin_response_singletons_statuses_and_error_framing() {
    for (name, value) in [
        ("Host", "racer"),
        ("Content-Length", "0"),
        ("Content-Type", "application/octet-stream"),
        ("Content-Range", "bytes */0"),
        ("ETag", "\"v\""),
        ("If-Match", "\"v\""),
        ("Range", "bytes=0-1"),
        ("Racer-Expires-At", "0"),
        ("Racer-Metadata", "x"),
        ("Authorization", "x"),
    ] {
        // Start with a valid 503; no already-invalid ETag/expiry/content-range may
        // make the duplicate assertion pass for an unrelated reason.
        let status = if name == "ETag" || name == "Racer-Expires-At" {
            200
        } else if name == "Content-Range" {
            416
        } else {
            503
        };
        let mut fields = String::new();
        if name != "Content-Length" {
            fields.push_str("Content-Length: 0\r\n");
        }
        if status == 200 {
            if name != "ETag" {
                fields.push_str("ETag: \"v\"\r\n");
            }
            if name != "Racer-Expires-At" {
                fields.push_str("Racer-Expires-At: 0\r\n");
            }
        }
        fields.push_str(&format!("{name}: {value}\r\n"));
        let base = raw_response(
            format!("HTTP/1.1 {status} Result\r\n{fields}\r\n").into_bytes(),
            "HEAD",
        )
        .unwrap();
        let expected = metadata::validate(&base, &object());
        assert!(
            expected.is_ok()
                || matches!(
                    expected,
                    Err(Error::Unavailable | Error::UnsatisfiableRangeWithLength(0))
                )
        );
        fields.push_str(&format!("{}: {value}\r\n", name.to_ascii_lowercase()));
        let raw = format!("HTTP/1.1 {status} Result\r\n{fields}\r\n").into_bytes();
        assert!(matches!(
            raw_response(raw, "HEAD").and_then(|h| metadata::validate(&h, &object())),
            Err(Error::InvalidRequest | Error::BadGateway)
        ));
    }
    for (status, extra, expected) in [
        (400, "", Error::InvalidRequest),
        (401, "", Error::OriginRejected),
        (403, "", Error::OriginForbidden),
        (404, "", Error::NotFound),
        (405, "Allow: HEAD, GET\r\n", Error::MethodNotAllowed),
        (412, "", Error::VersionUnavailable),
        (
            416,
            "Content-Range: bytes */17\r\n",
            Error::UnsatisfiableRangeWithLength(17),
        ),
        (431, "", Error::HeaderTooLarge),
        (500, "", Error::Internal),
        (502, "", Error::BadGateway),
        (503, "", Error::Unavailable),
        (302, "", Error::BadGateway),
        (418, "", Error::BadGateway),
    ] {
        let raw =
            format!("HTTP/1.1 {status} Result\r\nContent-Length: 0\r\n{extra}\r\n").into_bytes();
        let head = raw_response(raw, "HEAD").unwrap();
        assert_eq!(metadata::validate(&head, &object()), Err(expected));
    }
    for fields in [
        "Content-Length: 1\r\n",
        "Content-Length: 0\r\nETag: \"v\"\r\n",
        "Content-Length: 0\r\nRacer-Expires-At: 0\r\n",
        "Content-Length: 0\r\nContent-Encoding: identity\r\n",
        "Content-Length: 0\r\nTransfer-Encoding: identity\r\n",
    ] {
        let raw = format!("HTTP/1.1 503 Result\r\n{fields}\r\n").into_bytes();
        assert!(
            raw_response(raw, "HEAD")
                .and_then(|h| metadata::validate(&h, &object()))
                .is_err()
        );
    }
}

fn receive_all(mut peer: UnixStream) -> Vec<u8> {
    peer.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
    let mut bytes = Vec::new();
    // Closing without draining a rejected request can report reset after the
    // complete empty response. Preserve those bytes for the framing assertions.
    if let Err(error) = peer.read_to_end(&mut bytes) {
        assert_eq!(error.kind(), std::io::ErrorKind::ConnectionReset);
    }
    bytes
}
#[test]
fn raw_uds_client_error_writer_is_empty_with_required_416_and_405_fields() {
    for (error, status, extra) in [
        (Error::InvalidRequest, 400, ""),
        (Error::OriginRejected, 401, ""),
        (Error::OriginForbidden, 403, ""),
        (Error::NotFound, 404, ""),
        (Error::MethodNotAllowed, 405, "allow: head, get\r\n"),
        (Error::VersionUnavailable, 412, ""),
        (
            Error::UnsatisfiableRangeWithLength(17),
            416,
            "content-range: bytes */17\r\n",
        ),
        (Error::HeaderTooLarge, 431, ""),
        (Error::Internal, 500, ""),
        (Error::BadGateway, 502, ""),
        (Error::Unavailable, 503, ""),
        (Error::DeadlineExceeded, 503, ""),
    ] {
        let rig = Rig::new();
        let (local, peer) = UnixStream::pair().unwrap();
        let reader = thread::spawn(move || receive_all(peer));
        drop(
            rig.drive(
                rig.responses()
                    .send_error(rig.lease(local), error, &scope()),
            )
            .unwrap(),
        );
        let raw = reader.join().unwrap();
        let text = String::from_utf8(raw).unwrap().to_ascii_lowercase();
        assert!(text.starts_with(&format!("http/1.1 {status} ")));
        assert_eq!(text.matches("content-length:").count(), 1);
        assert!(text.contains("content-length: 0\r\n"));
        assert!(text.contains(extra));
        assert!(!text.contains("etag:") && !text.contains("racer-expires-at:"));
        assert_eq!(text.find("\r\n\r\n").unwrap() + 4, text.len());
    }
}

#[test]
fn raw_uds_client_empty_bootstrap_and_head_success_writer() {
    for (kind, length) in [
        (ReadKind::Bootstrap, 0),
        (ReadKind::Head, MAX),
        (
            ReadKind::HeadPinned {
                etag: StrongEtag::parse(b"\"v\"").unwrap(),
            },
            17,
        ),
    ] {
        let rig = Rig::new();
        let (local, mut peer) = UnixStream::pair().unwrap();
        let is_head = kind.is_head();
        let reader = thread::spawn(move || {
            peer.write_all(&request(
                if is_head { "HEAD" } else { "GET" },
                if is_head {
                    b""
                } else {
                    b"Range: bytes=0-16777215\r\n"
                },
            ))
            .unwrap();
            receive_all(peer)
        });
        rig.drive(async {
            let scope = scope();
            let received = rig.io.receive_head(rig.lease(local), &scope).await.unwrap();
            let response = ReadResponse {
                metadata: ObjectMetadata {
                    version: ObjectVersion {
                        object: object(),
                        etag: StrongEtag::parse(b"\"v\"").unwrap(),
                    },
                    length,
                    expires_at: ExpiresAt(UNIX_EPOCH),
                },
                range: None,
                body: None,
            };
            let responses = rig.responses();
            responses.validate(&kind, &response).unwrap();
            drop(
                responses
                    .send(received.connection, response, &scope)
                    .await
                    .unwrap(),
            );
        });
        let raw = reader.join().unwrap();
        let text = String::from_utf8(raw).unwrap().to_ascii_lowercase();
        assert!(text.starts_with("http/1.1 200 "));
        assert!(text.contains(&format!("content-length: {length}\r\n")));
        assert!(!text.contains("content-range:"));
        assert_eq!(text.find("\r\n\r\n").unwrap() + 4, text.len());
    }
}

#[path = "conformance/sdk.rs"]
mod sdk;

#[test]
fn raw_uds_fixed_body_is_not_scanned_for_heads_and_short_eof_is_failure() {
    for truncated in [false, true] {
        let rig = Rig::new();
        let (local, mut peer) = UnixStream::pair().unwrap();
        let writer = thread::spawn(move || {
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let mut b = [0];
                peer.read_exact(&mut b).unwrap();
                request.push(b[0]);
            }
            peer.write_all(b"HTTP/1.1 206 Result\r\nContent-Length: 9\r\nContent-Type: application/octet-stream\r\nContent-Range: bytes 0-8/9\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n\r\n\r\n\r\nHTTP").unwrap();
            if !truncated {
                peer.write_all(b"/").unwrap();
            }
        });
        let result: Result<Vec<u8>> = rig.drive(async {
            let scope = scope();
            let (head, _) = Codec::new(LIMIT, MAX)
                .decode_head(&request("GET", b"Range: bytes=0-16777215\r\n"))
                .unwrap()
                .unwrap();
            let sent = rig.io.send_head(rig.lease(local), head, &scope).await?;
            let received = rig.io.receive_head(sent.connection, &scope).await?;
            assert_eq!(
                metadata::validate_bootstrap(&received.value, &object())?.1,
                9
            );
            let mut connection = received.connection;
            let mut body = Vec::new();
            while body.len() < 9 {
                let received = rig
                    .io
                    .read_body(connection, rig.io.buffer(3)?, &scope)
                    .await?;
                body.extend_from_slice(&received.buffer.bytes()?[..received.bytes]);
                connection = received.lease;
            }
            connection.finish_exchange()?;
            Ok(body)
        });
        writer.join().unwrap();
        if truncated {
            assert!(result.is_err(), "short frame treated as clean EOF");
        } else {
            assert_eq!(result.unwrap(), b"\r\n\r\nHTTP/");
        }
    }
}

#[test]
fn raw_uds_sequential_requests_do_not_inherit_opaque_context() {
    let rig = Rig::new();
    let (local, mut peer) = UnixStream::pair().unwrap();
    peer.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
    let remote = thread::spawn(move || {
        for fields in [
            b"Authorization: fixture secret\r\nRacer-Metadata: opaque\r\n".as_slice(),
            b"",
        ] {
            peer.write_all(&request("HEAD", fields)).unwrap();
            let mut raw = Vec::new();
            while !raw.ends_with(b"\r\n\r\n") {
                let mut b = [0];
                peer.read_exact(&mut b).unwrap();
                raw.push(b[0]);
            }
            assert!(!raw.windows(14).any(|w| w == b"fixture secret"));
        }
    });
    rig.drive(async {
        let mut connection = rig.lease(local);
        for present in [true, false] {
            let scope = scope();
            let received = rig.io.receive_head(connection, &scope).await.unwrap();
            let parsed = RequestParser::new(LIMIT)
                .parse(&object().cache, received.value)
                .unwrap();
            assert_eq!(parsed.origin.authorization.is_some(), present);
            assert_eq!(parsed.origin.metadata.is_some(), present);
            drop(parsed);
            let response = ReadResponse {
                metadata: ObjectMetadata {
                    version: ObjectVersion {
                        object: object(),
                        etag: StrongEtag::parse(b"\"v\"").unwrap(),
                    },
                    length: 17,
                    expires_at: ExpiresAt(UNIX_EPOCH),
                },
                range: None,
                body: None,
            };
            connection = rig
                .responses()
                .send(received.connection, response, &scope)
                .await
                .unwrap();
            assert!(connection.is_reusable());
        }
    });
    remote.join().unwrap();
}

#[test]
fn raw_uds_late_rust_acquisition_failure_never_appends_second_status() {
    use racer_dataplane::{
        model::identity::{MembershipVersion, WorkerId},
        read::{dispatch::WorkerDirectory, range_stream::RangeStreams},
        runtime::worker::WorkerMap,
        topology::membership::Membership,
    };
    use std::sync::Arc;
    let rig = Rig::new();
    let (local, mut peer) = UnixStream::pair().unwrap();
    let reader = thread::spawn(move || {
        peer.write_all(&request("GET", b"If-Match: \"v\"\r\nRange: bytes=0-\r\n"))
            .unwrap();
        receive_all(peer)
    });
    rig.drive(async {
        let scope = scope();
        let received = rig.io.receive_head(rig.lease(local), &scope).await.unwrap();
        let parsed = RequestParser::new(LIMIT)
            .parse(&object().cache, received.value)
            .unwrap();
        let metadata = ObjectMetadata {
            version: ObjectVersion {
                object: object(),
                etag: StrongEtag::parse(b"\"v\"").unwrap(),
            },
            length: 3 * P + 13,
            expires_at: ExpiresAt(UNIX_EPOCH),
        };
        let range = ByteRange::From(0).resolve(metadata.length).unwrap();
        // Deliberately absent owner produces a real acquisition error after the
        // success head. No mock response writer or manufactured second status.
        let directory = Arc::new(
            WorkerDirectory::new(
                Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                vec![WorkerId(0)],
                4,
            )
            .unwrap(),
        );
        let delivery = Rc::new(Delivery::new(
            Rc::new(PipePool::new(rig.admission.clone(), rig.reactor.clone())),
            Duration::from_secs(5),
        ));
        let streams = RangeStreams::from_directory(directory, delivery, 1);
        let stream = streams
            .open(
                metadata.clone(),
                range,
                parsed.origin,
                Arc::new(Membership::validate(MembershipVersion(1), vec![]).unwrap()),
                scope.clone(),
            )
            .unwrap();
        assert_eq!(stream.buffered_pages(), 0);
        let response = ReadResponse {
            metadata,
            range: Some(range),
            body: Some(stream),
        };
        let responses = rig.responses();
        responses.validate(&parsed.kind, &response).unwrap();
        assert!(
            responses
                .send(received.connection, response, &scope)
                .await
                .is_err()
        );
    });
    let raw = reader.join().unwrap();
    let text = String::from_utf8(raw).unwrap().to_ascii_lowercase();
    assert!(text.starts_with("http/1.1 206 "));
    assert!(text.contains("content-length: 50331661\r\n"));
    assert_eq!(text.matches("http/1.1").count(), 1);
    assert_eq!(text.find("\r\n\r\n").unwrap() + 4, text.len());
}

#[test]
fn cache_names_validate_dns_labels_and_complete_socket_path() {
    use racer_dataplane::control::caches::canonical_socket_paths;
    for name in ["a", "a-b.c9", "0"] {
        assert!(canonical_socket_paths(name).is_ok());
    }
    for name in ["", ".a", "a.", "a..b", "-a", "a-", "A", "a_b", "../a"] {
        assert!(canonical_socket_paths(name).is_err());
    }
    assert!(canonical_socket_paths(&"a".repeat(64)).is_err());
    // Fill Linux's limit using the complete canonical path, not just its name.
    let fixed = "/run/racer//origin/socket".len();
    let name = format!("{}.{}", "a".repeat(63), "b".repeat(107 - fixed - 64));
    let (_, origin) = canonical_socket_paths(&name).unwrap();
    assert_eq!(origin.as_os_str().len(), 107);
    assert!(canonical_socket_paths(&format!("{name}b")).is_err());
}

#[test]
fn raw_uds_parser_errors_retain_a_writable_lease_for_empty_errors() {
    for (raw, status) in [
        (
            request("HEAD", b"Content-Length: 0\r\ncontent-length: 0\r\n"),
            400,
        ),
        (request("HEAD", b"Transfer-Encoding: identity\r\n"), 400),
        (request("HEAD", b"Authorization:  surplus\r\n"), 400),
        (
            request("HEAD", format!("X: {}\r\n", "x".repeat(LIMIT)).as_bytes()),
            431,
        ),
    ] {
        let rig = Rig::new();
        let (local, mut peer) = UnixStream::pair().unwrap();
        let reader = thread::spawn(move || {
            let _ = peer.write_all(&raw);
            receive_all(peer)
        });
        rig.drive(async {
            let scope = scope();
            let outcome = rig
                .io
                .receive_request_head_limited(rig.lease(local), &scope, LIMIT)
                .await
                .unwrap();
            let error = match outcome.value {
                Err(error) => error,
                Ok(_) => panic!("malformed head admitted"),
            };
            assert!(!outcome.connection.is_reusable());
            drop(
                rig.responses()
                    .send_error(outcome.connection, error, &scope)
                    .await
                    .unwrap(),
            );
        });
        let raw = reader.join().unwrap();
        let text = String::from_utf8(raw).unwrap().to_ascii_lowercase();
        assert!(text.starts_with(&format!("http/1.1 {status} ")));
        assert!(text.contains("content-length: 0\r\n"));
        assert_eq!(text.find("\r\n\r\n").unwrap() + 4, text.len());
    }
}

#[test]
fn raw_uds_response_limit_and_informational_rejection() {
    for length in [LIMIT, LIMIT + 1] {
        let fields = "Content-Length: 0\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\nX: ";
        let base = head_response(&format!("{fields}\r\n")).len();
        let raw = head_response(&format!("{fields}{}\r\n", "x".repeat(length - base)));
        assert_eq!(raw.len(), length);
        let result =
            raw_response(raw, "HEAD").and_then(|head| metadata::validate(&head, &object()));
        assert_eq!(result.is_ok(), length == LIMIT);
    }
    for status in [100, 101, 103] {
        assert!(
            raw_response(
                format!("HTTP/1.1 {status} Info\r\nContent-Length: 0\r\n\r\n").into_bytes(),
                "HEAD"
            )
            .is_err()
        );
    }
}
