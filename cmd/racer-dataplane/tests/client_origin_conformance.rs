//! Independent client v2 and origin v1 wire checks.
//! These exercise production HTTP/parser/writer components over real Unix sockets;
//! they do not substitute for an Application/Coordinator end-to-end deployment.
use http1::Header;
use http1::MessageHead;
use http1::StartLine;
use racer_dataplane::client::ClientRequest;
use racer_dataplane::client::ReadKind;
use racer_dataplane::client::RequestParser;
use racer_dataplane::client::Responses;
use racer_dataplane::error::Error;
use racer_dataplane::error::Result;
use racer_dataplane::http::Codec;
use racer_dataplane::http::ConnectionLease;
use racer_dataplane::http::HttpIo;
use racer_dataplane::http::Delivery;
use racer_dataplane::http::new_pipe_pool;
use racer_dataplane::model::ByteRange;
use racer_dataplane::model::CacheId;
use racer_dataplane::model::CacheKey;
use racer_dataplane::model::ExpiresAt;
use racer_dataplane::config::Limits;
use racer_dataplane::model::ObjectId;
use racer_dataplane::model::ObjectMetadata;
use racer_dataplane::model::ObjectVersion;
use racer_dataplane::model::PageId;
use racer_dataplane::model::PageNumber;
use racer_dataplane::model::RequestId;
use racer_dataplane::model::StrongEtag;
use racer_dataplane::origin::validate_bootstrap;
use racer_dataplane::origin::validate_metadata;
use racer_dataplane::origin::validate_page;
use racer_dataplane::read::ReadResponse;
use racer_dataplane::admission::AdmissionPolicy;
use racer_dataplane::runtime::RequestScope;
use racer_dataplane::runtime::Reactor;
use std::future::Future;
use std::io::Read;
use std::io::Write;
use std::num::NonZeroUsize;
use std::os::unix::net::UnixStream;
use std::rc::Rc;
use std::task::Context;
use std::task::Poll;
use std::thread;
use std::time::Duration;
use std::time::Instant;
use std::time::UNIX_EPOCH;
use uring_runtime::IoBuffer;

const LIMIT: usize = 32768;
const P: u64 = 16777216;
const MAX: u64 = 9223372036854775807;

struct Rig {
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    io: Rc<HttpIo>,
}
impl Rig {
    fn new() -> Self {
        let n = NonZeroUsize::new(32).unwrap();
        let bytes = NonZeroUsize::new(4 * P as usize).unwrap();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(Limits {
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
            header_bytes: NonZeroUsize::new(LIMIT).unwrap(),
            cached_rankings: n,
            cached_paths: n,
            retained_snapshots: n,
            metadata_entries: n,
            relay_transfers: n,
        })));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = Rc::new(racer_dataplane::http::new_io(
            reactor.clone(),
            Codec::new(LIMIT),
            admission.clone(),
            MAX,
        ));
        Self {
            admission,
            reactor,
            io,
        }
    }
    fn lease(&self, socket: UnixStream) -> ConnectionLease {
        racer_dataplane::http::from_accepted(socket.into(), &self.admission).unwrap()
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
                Rc::new(new_pipe_pool(self.admission.clone())),
                self.reactor.clone(),
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
    // Client request fixtures use only the current v2 target.
    let version = "v2";
    let mut raw = format!(
        "{method} /{version}/objects/{} HTTP/1.1\r\nHost: racer\r\n",
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
    reject(request("POST", b""), Error::InvalidRequest);
    reject(request("GET", b""), Error::MethodNotAllowed);
    let canonical_v2 = canonical.replacen("v1", "v2", 1);
    reject(
        format!("GET {canonical_v2} HTTP/1.1\r\nHost: racer\r\n\r\n").into_bytes(),
        Error::MethodNotAllowed,
    );
    reject(
        format!("POST {canonical} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n\r\n")
            .into_bytes(),
        Error::InvalidRequest,
    );
    for target in [
        format!("{canonical_v2}?"),
        format!("{canonical_v2}#x"),
        format!("{canonical_v2}/"),
        canonical_v2.replace("ab", "AB"),
        canonical_v2.replace("ab", "%61b"),
        format!("http://racer{canonical_v2}"),
        canonical_v2.replace("objects/", "objects//"),
    ] {
        reject(
            format!("POST {target} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n\r\n")
                .into_bytes(),
            Error::InvalidRequest,
        );
    }
    let parsed = raw_request(request("POST", b"Content-Length: 0\r\n")).unwrap();
    assert_eq!(
        parsed.kind,
        ReadKind::Subscription {
            pin: None,
            range: None,
            page_credits: 2,
            byte_credits: 2 * P,
            ordered: false
        }
    );
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
        reject(
            request(
                "POST",
                format!("Content-Length: 0\r\n{field}\r\n").as_bytes(),
            ),
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
        ("Racer-Page-Credits", "1"),
        ("Racer-Byte-Credits", "16777216"),
        ("Racer-Ordered", "1"),
    ] {
        let fields = format!(
            "{name}: {value}\r\n{}: {value}\r\n",
            name.to_ascii_lowercase()
        );
        reject(request("HEAD", fields.as_bytes()), Error::InvalidRequest);
        reject(
            request("POST", format!("Content-Length: 0\r\n{fields}").as_bytes()),
            Error::InvalidRequest,
        );
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
        } else {
            parsed.origin.metadata.as_ref().unwrap().as_header()
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
                "POST",
                format!("Content-Length: 0\r\nIf-Match: \"v\"\r\nRange: {range}\r\n").as_bytes()
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
                "POST",
                format!("Content-Length: 0\r\nIf-Match: \"v\"\r\nRange: {range}\r\n").as_bytes()
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
        let metadata = validate_metadata(&head, &object()).unwrap();
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
            .and_then(|head| validate_metadata(&head, &object()));
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
        assert_eq!(validate_metadata(&head, &object()), Err(Error::BadGateway));
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
            validate_metadata(&head, &object()),
            Err(Error::BadGateway),
            "expiry whitespace normalized"
        );
    }
}

#[test]
fn raw_uds_origin_bootstrap_and_aligned_final_page() {
    let empty = raw_response(head_response("Content-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n"), "GET").unwrap();
    assert_eq!(validate_bootstrap(&empty, &object()).unwrap().1, 0);
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
        assert_eq!(validate_page(&head, &page, length).is_ok(), valid);
        if valid {
            assert_eq!(
                validate_page(&head, &page, length - 1),
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
        let expected = validate_metadata(&base, &object());
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
            raw_response(raw, "HEAD").and_then(|h| validate_metadata(&h, &object())),
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
        assert_eq!(validate_metadata(&head, &object()), Err(expected));
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
                .and_then(|h| validate_metadata(&h, &object()))
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
        (Error::MethodNotAllowed, 405, "allow: head, post\r\n"),
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
        (
            ReadKind::Subscription {
                pin: None,
                range: None,
                page_credits: 1,
                byte_credits: P,
                ordered: true,
            },
            0,
        ),
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
        let pinned = kind.pin().is_some();
        let reader = thread::spawn(move || {
            peer.write_all(&request(
                if is_head { "HEAD" } else { "POST" },
                if pinned {
                    b"If-Match: \"v\"\r\n"
                } else if is_head {
                    b""
                } else {
                    b"Content-Length: 0\r\nRacer-Page-Credits: 1\r\nRacer-Byte-Credits: 16777216\r\nRacer-Ordered: 1\r\n"
                },
            ))
            .unwrap();
            receive_all(peer)
        });
        rig.drive(async {
            let scope = scope();
            let received = rig.io.receive_head(rig.lease(local), &scope).await.unwrap();
            let parsed = RequestParser::new(LIMIT)
                .parse(&object().cache, received.value)
                .unwrap();
            assert_eq!(parsed.kind, kind);
            let response = ReadResponse {
                metadata: ObjectMetadata {
                    content_type: None,
                    version: ObjectVersion {
                        object: object(),
                        etag: StrongEtag::parse(b"\"v\"").unwrap(),
                    },
                    length,
                    expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
                },
                range: None,
                body: None,
            };
            let responses = rig.responses();
            responses.validate(&kind, &response).unwrap();
            if is_head {
                drop(
                    responses
                        .send(received.connection, response, &scope)
                        .await
                        .unwrap(),
                );
            } else {
                drop(
                    responses
                        .send_subscription_unobserved(
                            received.connection,
                            response,
                            &scope,
                            Duration::from_secs(5),
                        )
                        .await
                        .unwrap(),
                );
            }
        });
        let raw = reader.join().unwrap();
        let head_end = raw.windows(4).position(|part| part == b"\r\n\r\n").unwrap() + 4;
        let text = String::from_utf8(raw[..head_end].to_vec())
            .unwrap()
            .to_ascii_lowercase();
        assert!(text.starts_with("http/1.1 200 "));
        assert!(text.contains(&format!(
            "content-length: {}\r\n",
            if is_head { length } else { 21 }
        )));
        assert!(!text.contains("content-range:"));
        if is_head {
            assert_eq!(raw.len(), head_end);
        } else {
            assert!(text.contains("racer-object-length: 0\r\n"));
            assert!(text.contains("racer-range-start: 0\r\n"));
            assert!(text.contains("racer-range-end: 0\r\n"));
            assert!(text.contains("connection: close\r\n"));
            let mut complete = [0; 21];
            complete[0] = 2;
            assert_eq!(&raw[head_end..], &complete);
        }
    }
}

// Keep this module path: the racer-sdk-conformance Make target selects it exactly.
mod sdk {
    use super::*;
    use racer_dataplane::model::ResourceClass;
    use std::fs;
    use std::os::fd::AsRawFd;
    use std::os::unix::net::UnixListener;
    use std::path::PathBuf;
    use std::process::Child;
    use std::process::Command;
    use std::process::Stdio;

    struct Process(Child);
    impl Drop for Process {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }

    // Explicitly opt in: builds and runs the actual Go SDK, which may live in another
    // worktree. The Rust client-facing peer scripts data through production HTTP;
    // the origin-facing peer invokes the built production SDK server.
    #[test]
    #[ignore = "set RACER_SDK_ROOT to the Go repository and run --ignored sdk_client"]
    fn sdk_client_to_rust_http_and_request_parser_over_uds() {
        let root =
            PathBuf::from(std::env::var_os("RACER_SDK_ROOT").expect("RACER_SDK_ROOT required"));
        assert!(root.join("pkg/racersdk/client.go").is_file());
        let fixture = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("tests/conformance/sdk_fixture_test.go.txt");
        let output = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!("conformance-sdk-{}", std::process::id()));
        fs::create_dir(&output).unwrap();
        struct Cleanup(PathBuf);
        impl Drop for Cleanup {
            fn drop(&mut self) {
                let _ = fs::remove_dir_all(&self.0);
            }
        }
        let _cleanup = Cleanup(output.clone());
        let overlay = output.join("overlay.json");
        fs::write(&overlay, serde_json::to_vec(&serde_json::json!({"Replace": {root.join("pkg/racersdk/rust_conformance_fixture_test.go").to_str().unwrap(): fixture}})).unwrap()).unwrap();
        let binary = output.join("sdk.test");
        let build = Command::new("timeout")
            .current_dir(&root)
            .args([
                "--signal=TERM",
                "--kill-after=10s",
                "300s",
                "go",
                "test",
                "-timeout=5m",
                "-overlay",
            ])
            .arg(&overlay)
            .args(["-c", "-o"])
            .arg(&binary)
            .arg("./pkg/racersdk")
            .output()
            .unwrap();
        assert!(
            build.status.success(),
            "SDK build: {}",
            String::from_utf8_lossy(&build.stderr)
        );
        // A proc-fd alias avoids sun_path overflow without creating anything in /run.
        let directory = fs::File::open(&output).unwrap();
        let socket = PathBuf::from(format!(
            "/proc/{}/fd/{}/socket",
            std::process::id(),
            directory.as_raw_fd()
        ));
        for size in [0, 3, P, 2 * P, 3 * P + 13] {
            let listener = UnixListener::bind(&socket).unwrap();
            listener.set_nonblocking(true).unwrap();
            let mut child = Process(
                Command::new(&binary)
                    .arg("-test.run=^TestRustWireClientFixture$")
                    .arg("-test.timeout=30s")
                    .env("RACER_CONFORMANCE_SOCKET", &socket)
                    .env("RACER_CONFORMANCE_SIZE", size.to_string())
                    .spawn()
                    .unwrap(),
            );
            let deadline = Instant::now() + Duration::from_secs(10);
            let accepted = loop {
                match listener.accept() {
                    Ok((socket, _)) => break socket,
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        assert!(
                            child.0.try_wait().unwrap().is_none(),
                            "SDK exited before connecting"
                        );
                        assert!(Instant::now() < deadline, "SDK did not connect");
                        thread::sleep(Duration::from_millis(1));
                    }
                    Err(error) => panic!("accept: {error}"),
                }
            };
            let rig = Rig::new();
            rig.reactor.init().unwrap();
            let context_baseline = rig.admission.used(ResourceClass::RequestContext);
            let mut releases = accepted.try_clone().unwrap();
            rig.drive(async {
                let connection = rig.lease(accepted);
                let scope = scope();
                let received = rig.io.receive_head(connection, &scope).await.unwrap();
                let parsed = RequestParser::new(LIMIT)
                    .parse(&object().cache, received.value)
                    .unwrap();
                assert_eq!(parsed.origin.object.key, CacheKey([0; 32]));
                assert_eq!(
                    parsed.origin.metadata.as_ref().unwrap().as_header(),
                    b"opaque,  bytes\xff"
                );
                assert_eq!(
                    parsed
                        .origin
                        .authorization
                        .as_ref()
                        .unwrap()
                        .expose_for_origin(),
                    b"fixture credential\x80"
                );
                assert_eq!(
                    parsed.kind,
                    ReadKind::Subscription {
                        pin: None,
                        range: None,
                        page_credits: 1,
                        byte_credits: P,
                        ordered: true,
                    }
                );
                drop(parsed);
                send_subscription(
                    &rig,
                    received.connection,
                    &mut releases,
                    size,
                    0,
                    size,
                    "application/vnd.oci.image.manifest.v1+json",
                )
                .await;
            });
            assert_eq!(rig.admission.used(ResourceClass::Connection), 0);
            // The HTTP owner retains one admitted idle staging buffer for reuse.
            // Dropping it cannot release any buffer still owned by the reactor.
            drop(rig.io);
            assert_eq!(
                rig.admission.used(ResourceClass::RequestContext),
                context_baseline,
                "SDK exchanges retained request buffers"
            );
            let until = Instant::now() + Duration::from_secs(10);
            loop {
                if let Some(status) = child.0.try_wait().unwrap() {
                    assert!(status.success(), "SDK fixture failed");
                    break;
                }
                if Instant::now() >= until {
                    child.0.kill().unwrap();
                    child.0.wait().unwrap();
                    panic!("SDK fixture stalled");
                }
                thread::sleep(Duration::from_millis(1));
            }
            sdk_range(&binary, &socket, &listener, size);
            drop(listener);
            fs::remove_file(output.join("socket")).unwrap();
        }
        // ServeOrigin intentionally rejects proc-fd symlink ancestors. Use a short,
        // real project-local directory for its private path seam instead.
        let origin_directory = root.join("tmp").join(format!("s{}", std::process::id()));
        fs::create_dir(&origin_directory).unwrap();
        let _origin_cleanup = Cleanup(origin_directory.clone());
        sdk_origin(&binary, &origin_directory.join("s"));
    }

    fn sdk_range(
        binary: &std::path::Path,
        socket: &std::path::Path,
        listener: &UnixListener,
        size: u64,
    ) {
        let mut child = Process(
            Command::new(binary)
                .args(["-test.run=^TestRustWireRangeFixture$", "-test.timeout=30s"])
                .env("RACER_CONFORMANCE_SOCKET", socket)
                .env("RACER_CONFORMANCE_SIZE", size.to_string())
                .spawn()
                .unwrap(),
        );
        let accept = || {
            let deadline = Instant::now() + Duration::from_secs(10);
            loop {
                match listener.accept() {
                    Ok((stream, _)) => break stream,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        assert!(Instant::now() < deadline, "range fixture did not connect");
                        thread::sleep(Duration::from_millis(1));
                    }
                    Err(e) => panic!("range accept: {e}"),
                }
            }
        };
        let accepted = accept();
        let rig = Rig::new();
        rig.reactor.init().unwrap();
        rig.drive(async {
            let connection = rig.lease(accepted);
            let scope = scope();
            let received = rig.io.receive_head(connection, &scope).await.unwrap();
            let request = RequestParser::new(LIMIT)
                .parse(&object().cache, received.value)
                .unwrap();
            assert_eq!(request.kind, ReadKind::Head);
            let mut connection = rig
                .io
                .send_head(
                    received.connection,
                    MessageHead {
                        start: StartLine::Response { status: 200 },
                        headers: vec![
                            Header {
                                name: "Content-Length".into(),
                                value: size.to_string().into_bytes(),
                            },
                            Header {
                                name: "ETag".into(),
                                value: b"\"v\"".to_vec(),
                            },
                            Header {
                                name: "Racer-Expires-At".into(),
                                value: b"0".to_vec(),
                            },
                            Header {
                                name: "Racer-Content-Type".into(),
                                value: b"text/plain".to_vec(),
                            },
                        ],
                    },
                    &scope,
                )
                .await
                .unwrap()
                .connection;
            connection.finish_exchange().unwrap();
        });
        {
            let first = size.saturating_sub(1).min(P - 3);
            let length = (size - first).min(P + 9);
            let accepted = accept();
            let mut releases = accepted.try_clone().unwrap();
            rig.drive(async {
                let scope = scope();
                let received = rig
                    .io
                    .receive_head(rig.lease(accepted), &scope)
                    .await
                    .unwrap();
                let request = RequestParser::new(LIMIT)
                    .parse(&object().cache, received.value)
                    .unwrap();
                assert_eq!(
                    request.kind,
                    ReadKind::Subscription {
                        pin: Some(StrongEtag::parse(b"\"v\"").unwrap()),
                        range: if length == 0 {
                            None
                        } else {
                            Some(ByteRange::Closed {
                                first,
                                last: first + length - 1,
                            })
                        },
                        page_credits: 1,
                        byte_credits: P,
                        ordered: true,
                    }
                );
                send_subscription(
                    &rig,
                    received.connection,
                    &mut releases,
                    size,
                    first,
                    first + length,
                    "text/plain",
                )
                .await;
            });
        }
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            if let Some(status) = child.0.try_wait().unwrap() {
                assert!(status.success(), "range fixture failed");
                break;
            }
            assert!(Instant::now() < deadline, "range fixture stalled");
            thread::sleep(Duration::from_millis(1));
        }
        assert_eq!(rig.admission.used(ResourceClass::Connection), 0);
    }

    // A scripted peer exercises the real Rust HTTP writer and production Go SDK.
    // One credit forces exact releases before the next page, including clipped pages.
    async fn send_subscription(
        rig: &Rig,
        connection: racer_dataplane::http::ConnectionLease,
        releases: &mut UnixStream,
        size: u64,
        first: u64,
        end: u64,
        content_type: &str,
    ) {
        let scope = scope();
        let pages = if first == end {
            0
        } else {
            (end - 1) / P - first / P + 1
        };
        let fields = [
            (
                "Content-Length",
                (end - first + 21 * (pages + 1)).to_string(),
            ),
            ("Content-Type", "application/octet-stream".into()),
            ("Connection", "close".into()),
            ("ETag", "\"v\"".into()),
            ("Racer-Expires-At", "0".into()),
            ("Racer-Object-Length", size.to_string()),
            ("Racer-Range-Start", first.to_string()),
            ("Racer-Range-End", end.to_string()),
            ("Racer-Content-Type", content_type.into()),
        ];
        let head = MessageHead {
            start: StartLine::Response { status: 200 },
            headers: fields
                .into_iter()
                .map(|(name, value)| Header {
                    name: name.into(),
                    value: value.into_bytes(),
                })
                .collect(),
        };
        let mut connection = rig
            .io
            .send_head(connection, head, &scope)
            .await
            .unwrap()
            .connection;
        let mut offset = first;
        while offset < end {
            let number = offset / P;
            let page_end = end.min((number + 1) * P);
            let length = (page_end - offset) as u32;
            let mut frame = rig.io.buffer(21).unwrap();
            frame
                .bytes_mut()
                .unwrap()
                .copy_from_slice(&subscription_frame(1, number, offset, length));
            connection = rig
                .io
                .write_body(connection, frame, &scope)
                .await
                .unwrap()
                .lease;
            while offset < page_end {
                let n = (page_end - offset).min(32768) as usize;
                let mut buffer = rig.io.buffer(n).unwrap();
                for (i, b) in buffer.bytes_mut().unwrap().iter_mut().enumerate() {
                    *b = ((offset + i as u64) % 251) as u8;
                }
                connection = rig
                    .io
                    .write_body(connection, buffer, &scope)
                    .await
                    .unwrap()
                    .lease;
                offset += n as u64;
            }
            if offset < end {
                let deadline = Instant::now() + Duration::from_secs(5);
                let mut release = [0; 12];
                let mut used = 0;
                while used < release.len() {
                    match releases.read(&mut release[used..]) {
                        Ok(0) => panic!("SDK closed before releasing page"),
                        Ok(n) => used += n,
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                            assert!(Instant::now() < deadline, "SDK did not release page");
                            thread::sleep(Duration::from_millis(1));
                        }
                        Err(e) => panic!("SDK release: {e}"),
                    }
                }
                assert_eq!(&release[..8], &number.to_be_bytes());
                assert_eq!(&release[8..], &length.to_be_bytes());
            }
        }
        let mut frame = rig.io.buffer(21).unwrap();
        frame
            .bytes_mut()
            .unwrap()
            .copy_from_slice(&subscription_frame(2, pages, end - first, 0));
        connection = rig
            .io
            .write_body(connection, frame, &scope)
            .await
            .unwrap()
            .lease;
        connection.finish_exchange().unwrap();
        // Close both descriptors, including the release observer, at completion.
        releases.shutdown(std::net::Shutdown::Both).unwrap();
    }

    fn subscription_frame(kind: u8, number: u64, offset: u64, length: u32) -> [u8; 21] {
        let mut frame = [0; 21];
        frame[0] = kind;
        frame[1..9].copy_from_slice(&number.to_be_bytes());
        frame[9..17].copy_from_slice(&offset.to_be_bytes());
        frame[17..].copy_from_slice(&length.to_be_bytes());
        frame
    }

    fn sdk_origin(binary: &std::path::Path, socket: &std::path::Path) {
        let mut child = Process(
            Command::new(binary)
                .args(["-test.run=^TestRustWireOriginFixture$", "-test.timeout=40s"])
                .env("RACER_CONFORMANCE_SOCKET", socket)
                .stdin(Stdio::piped())
                .spawn()
                .unwrap(),
        );
        let until = Instant::now() + Duration::from_secs(10);
        while !socket.exists() {
            assert!(
                child.0.try_wait().unwrap().is_none(),
                "origin fixture exited before bind"
            );
            assert!(Instant::now() < until, "SDK origin did not bind");
            thread::sleep(Duration::from_millis(1));
        }
        for (key, method, fields, status, body) in [
            (0, "HEAD", "If-Match: \"v\"\r\n", 200, Some(b"".as_slice())),
            (
                0,
                "GET",
                "Range: bytes=0-16777215\r\n",
                206,
                Some(b"abc".as_slice()),
            ),
            (
                1,
                "GET",
                "Range: bytes=0-16777215\r\n",
                200,
                Some(b"".as_slice()),
            ),
            (
                0,
                "GET",
                "If-Match: \"v\"\r\nRange: bytes=0-2\r\n",
                206,
                Some(b"abc".as_slice()),
            ),
            (
                0,
                "GET",
                "If-Match: \"v\"\r\nRange: bytes=0-1\r\n",
                400,
                Some(b"".as_slice()),
            ),
            (
                0,
                "GET",
                "If-Match: \"v\"\r\nRange: bytes=0-\r\n",
                400,
                Some(b"".as_slice()),
            ),
            (
                0,
                "GET",
                "If-Match: \"v\"\r\nRange: bytes=-1\r\n",
                400,
                Some(b"".as_slice()),
            ),
            (
                0,
                "GET",
                "If-Match: \"v\"\r\nRange: bytes=1-2\r\n",
                400,
                Some(b"".as_slice()),
            ),
            (
                0,
                "GET",
                "If-Match: \"v\"\r\nRange: bytes=0-16777216\r\n",
                400,
                Some(b"".as_slice()),
            ),
            (
                0,
                "GET",
                "If-Match: \"v\"\r\nRange: bytes=16777216-33554431\r\n",
                416,
                Some(b"".as_slice()),
            ),
            (
                0,
                "HEAD",
                "If-Match: \"wrong\"\r\n",
                502,
                Some(b"".as_slice()),
            ),
            (0, "POST", "", 405, Some(b"".as_slice())),
            (4, "HEAD", "", 404, Some(b"".as_slice())),
            (4, "HEAD", "If-Match: \"v\"\r\n", 412, Some(b"".as_slice())),
            (5, "HEAD", "If-Match: \"v\"\r\n", 401, Some(b"".as_slice())),
            (6, "HEAD", "If-Match: \"v\"\r\n", 403, Some(b"".as_slice())),
            (7, "HEAD", "If-Match: \"v\"\r\n", 503, Some(b"".as_slice())),
            (2, "GET", "Range: bytes=0-16777215\r\n", 206, None),
            (3, "GET", "Range: bytes=0-16777215\r\n", 206, None),
        ] {
            let mut stream = UnixStream::connect(socket).unwrap();
            stream
                .set_write_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            let mut raw = format!("{method} /v1/objects/{key:02x}{} HTTP/1.1\r\nHost: racer\r\nConnection: close\r\n{fields}", "00".repeat(31)).into_bytes();
            raw.extend_from_slice(
                b"Racer-Metadata: opaque,  bytes\xff\r\nAuthorization: fixture credential\x80\r\n\r\n",
            );
            stream.write_all(&raw).unwrap();
            let response = receive_all(stream);
            let (head, used) = Codec::new(LIMIT).decode_head(&response).unwrap().unwrap();
            assert!(
                matches!(head.start, StartLine::Response { status: actual } if actual == status),
                "wrong SDK status for key {key}, {method}, {fields}"
            );
            if let Some(body) = body {
                assert_eq!(&response[used..], body);
            } else {
                assert!(
                    response.len() - used < 3,
                    "late callback failure completed the frame"
                );
                assert!(
                    !response[used..].windows(5).any(|w| w == b"HTTP/"),
                    "second status after success"
                );
            }
            if status >= 400 {
                assert_eq!(
                    head.unique("Content-Length").unwrap(),
                    Some(b"0".as_slice())
                );
                assert!(head.unique("ETag").unwrap().is_none());
                assert!(head.unique("Racer-Expires-At").unwrap().is_none());
                assert!(head.unique("Racer-Content-Type").unwrap().is_none());
                if status == 416 {
                    assert_eq!(
                        head.unique("Content-Range").unwrap(),
                        Some(b"bytes */3".as_slice())
                    );
                }
                if status == 405 {
                    assert_eq!(head.unique("Allow").unwrap(), Some(b"HEAD, GET".as_slice()));
                }
            } else if method == "HEAD" {
                assert_eq!(
                    head.unique("Racer-Content-Type").unwrap(),
                    Some(b"text/plain".as_slice())
                );
                assert_eq!(validate_metadata(&head, &object()).unwrap().length, 3);
            } else if !fields.contains("If-Match") {
                assert_eq!(
                    validate_bootstrap(&head, &object()).unwrap().1,
                    if key == 1 { 0 } else { 3 }
                );
            } else {
                let page = PageId {
                    version: ObjectVersion {
                        object: object(),
                        etag: StrongEtag::parse(b"\"v\"").unwrap(),
                    },
                    number: PageNumber(0),
                };
                assert_eq!(validate_page(&head, &page, 3).unwrap().length, 3);
            }
        }
        child.0.stdin.take().unwrap().write_all(b"x").unwrap();
        let until = Instant::now() + Duration::from_secs(5);
        loop {
            if let Some(status) = child.0.try_wait().unwrap() {
                assert!(status.success());
                break;
            }
            assert!(Instant::now() < until, "SDK origin did not stop");
            thread::sleep(Duration::from_millis(1));
        }
    }
}

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
            let (head, _) = Codec::new(LIMIT)
                .decode_head(&request("GET", b"Range: bytes=0-16777215\r\n"))
                .unwrap()
                .unwrap();
            let sent = rig.io.send_head(rig.lease(local), head, &scope).await?;
            let received = rig.io.receive_head(sent.connection, &scope).await?;
            assert_eq!(validate_bootstrap(&received.value, &object())?.1, 9);
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
                    content_type: None,
                    version: ObjectVersion {
                        object: object(),
                        etag: StrongEtag::parse(b"\"v\"").unwrap(),
                    },
                    length: 17,
                    expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
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

// First-page and late-acquisition failure wire assertions live in
// src/client/listener/tests/acquisition.rs, driven by a real Coordinator and UDS origin.

#[test]
fn cache_names_validate_dns_labels_and_complete_socket_path() {
    use racer_control_wire::canonical_socket_paths;
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
        let result = raw_response(raw, "HEAD").and_then(|head| validate_metadata(&head, &object()));
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
