use super::*;
use crate::control::CacheId;
use crate::http::Codec;
use crate::object::model::{ExpiresAt, ObjectId, ObjectVersion};
use crate::object::{Error, client};
use std::time::{Duration, UNIX_EPOCH};

const KEY: &str = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff";
const PAGE: PageSize = PageSize::MAX;

fn head(method: &str, target: &str, fields: &[(&str, &str)]) -> MessageHead {
    MessageHead {
        start: StartLine::Request {
            method: method.into(),
            target: target.into(),
        },
        headers: fields.iter().map(|(n, v)| Header::new(*n, v)).collect(),
    }
}

fn object(suffix: &str) -> String {
    format!("/v1/objects/{KEY}{suffix}")
}

fn put(fields: &[(&str, &str)]) -> MessageHead {
    let mut all = vec![
        ("Host", "racer"),
        ("Content-Length", "100"),
        ("If-None-Match", "*"),
    ];
    for field in fields {
        all.retain(|(n, _)| !n.eq_ignore_ascii_case(field.0));
    }
    all.extend_from_slice(fields);
    all.retain(|(_, v)| *v != "<absent>");
    head("PUT", &object(""), &all)
}

fn host_write(fields: &[(&str, &str)]) -> MessageHead {
    let mut all = vec![
        ("Host", "racer"),
        ("Content-Length", "0"),
        ("If-None-Match", "*"),
        ("Racer-Source-Offset", "4096"),
        ("Racer-Source-Length", "8192"),
    ];
    for field in fields {
        all.retain(|(n, _)| !n.eq_ignore_ascii_case(field.0));
    }
    all.extend_from_slice(fields);
    all.retain(|(_, v)| *v != "<absent>");
    head("POST", &object("/host-write"), &all)
}

fn parse(head: &MessageHead) -> WriteResult<WriteRequest<'_>> {
    parse_write(head, MAX_HEAD_BYTES, PAGE)
}

fn err(head: &MessageHead) -> WriteError {
    match parse(head) {
        Ok(_) => panic!("accepted"),
        Err(e) => e,
    }
}

const INVALID: WriteError = WriteError::Object(Error::InvalidRequest);

#[test]
fn routes() {
    assert!(is_write(&put(&[])));
    assert!(is_write(&head("DELETE", &object(""), &[])));
    assert!(is_write(&head("POST", &object("/host-write"), &[])));
    assert!(is_write(&head("POST", &object("/persist"), &[])));
    for (method, target) in [
        ("POST", object("")),
        ("HEAD", object("")),
        ("GET", object("")),
        ("PUT", object("/host-write")),
        ("DELETE", object("/persist")),
        ("POST", object("/host-read")),
        ("POST", object("/persist/x")),
        ("PUT", "/v1/objects/abc".to_string()),
        ("PUT", format!("/v1/objects/{}", "g".repeat(64))),
        ("PUT", format!("/v2/objects/{KEY}")),
        ("put", object("")),
    ] {
        assert!(!is_write(&head(method, &target, &[])), "{method} {target}");
    }
    let response = MessageHead {
        start: StartLine::Response { status: 200 },
        headers: vec![],
    };
    assert!(!is_write(&response));
    assert_eq!(
        err(&head("GET", &object(""), &[("Host", "racer")])),
        WriteError::Object(Error::MethodNotAllowed)
    );
}

#[test]
fn put_defaults() {
    let h = put(&[]);
    let r = parse(&h).unwrap();
    assert_eq!(
        r.kind,
        WriteKind::Put {
            content_length: 100
        }
    );
    assert_eq!(r.key.to_hex(), KEY);
    assert!(r.metadata.is_none() && r.authorization.is_none());
    assert!(r.content_type.is_none() && r.durability.is_none());
    assert!(r.upload.is_none() && r.hold_seconds.is_none());
    assert!(r.kind.creates());
    assert_eq!(r.kind.object_length(), Some(100));
}

#[test]
fn put_all_fields() {
    let h = put(&[
        ("Racer-Metadata", r#"{"schema":1,"bucket":"b","key":"k"}"#),
        ("Authorization", "Bearer x"),
        ("Racer-Content-Type", "application/json"),
        ("Racer-Durability", "origin"),
        ("Racer-Upload", "auto"),
        ("Racer-Hold-Seconds", "2592000"),
    ]);
    let r = parse(&h).unwrap();
    assert_eq!(
        r.metadata,
        Some(&br#"{"schema":1,"bucket":"b","key":"k"}"#[..])
    );
    assert_eq!(r.authorization, Some(&b"Bearer x"[..]));
    assert_eq!(r.content_type.unwrap().as_str(), "application/json");
    assert_eq!(r.durability, Some(Durability::Origin));
    assert_eq!(r.upload, Some(UploadMode::Auto));
    assert_eq!(r.hold_seconds, Some(MAX_HOLD_SECONDS));
}

#[test]
fn content_length_rules() {
    assert_eq!(
        err(&put(&[("Content-Length", "<absent>")])),
        WriteError::LengthRequired
    );
    assert_eq!(err(&put(&[("Content-Length", "0")])), INVALID);
    assert_eq!(err(&put(&[("Content-Length", "01")])), INVALID);
    assert_eq!(err(&put(&[("Content-Length", "-1")])), INVALID);
    let max = max_object_bytes(PAGE).to_string();
    let over = (max_object_bytes(PAGE) + 1).to_string();
    assert_eq!(max_object_bytes(PAGE), 50_000 * 16 * 1024 * 1024);
    let h = put(&[("Content-Length", &max)]);
    assert!(parse(&h).is_ok());
    assert_eq!(
        err(&put(&[("Content-Length", &over)])),
        WriteError::PayloadTooLarge
    );
    // The limit follows the page size argument.
    let small = PageSize::new(64 * 1024).unwrap();
    let h = put(&[("Content-Length", &(50_000u64 * 64 * 1024 + 1).to_string())]);
    assert_eq!(
        parse_write(&h, MAX_HEAD_BYTES, small).err(),
        Some(WriteError::PayloadTooLarge)
    );
}

#[test]
fn if_none_match_rules() {
    for value in ["<absent>", "\"x\"", "*, \"x\"", "W/\"x\""] {
        assert_eq!(
            err(&put(&[("If-None-Match", value)])),
            WriteError::PreconditionRequired,
            "{value}"
        );
    }
    assert!(parse(&put(&[("If-None-Match", " * ")])).is_ok());
    assert_eq!(
        err(&host_write(&[("If-None-Match", "<absent>")])),
        WriteError::PreconditionRequired
    );
    assert_eq!(
        err(&head(
            "DELETE",
            &object(""),
            &[("Host", "racer"), ("If-None-Match", "*")]
        )),
        INVALID
    );
}

#[test]
fn value_rules() {
    for (name, value) in [
        ("Racer-Durability", "replicas=0"),
        ("Racer-Durability", "replicas=4"),
        ("Racer-Durability", "Origin"),
        ("Racer-Upload", "always"),
        ("Racer-Hold-Seconds", "0"),
        ("Racer-Hold-Seconds", "2592001"),
        ("Racer-Hold-Seconds", "01"),
        ("Racer-Content-Type", "not a type"),
        ("Racer-Metadata", " x"),
        ("Authorization", "x "),
        ("Expect", "100-continue"),
        ("Transfer-Encoding", "chunked"),
        ("If-Match", "\"x\""),
        ("Range", "bytes=0-1"),
        ("Connection", "upgrade"),
        ("Racer-Source-Offset", "0"),
    ] {
        assert_eq!(err(&put(&[(name, value)])), INVALID, "{name}: {value}");
    }
    for (durability, upload) in [("origin", "never"), ("origin", "manual")] {
        assert_eq!(
            err(&put(&[
                ("Racer-Durability", durability),
                ("Racer-Upload", upload)
            ])),
            INVALID
        );
    }
    for d in ["replicas=1", "replicas=2", "replicas=3"] {
        let h = put(&[("Racer-Durability", d), ("Racer-Upload", "never")]);
        assert!(parse(&h).is_ok());
    }
    let h = put(&[("Racer-Hold-Seconds", "1")]);
    assert_eq!(parse(&h).unwrap().hold_seconds, Some(1));
}

#[test]
fn duplicates_host_and_limits() {
    for name in [
        "Host",
        "Content-Length",
        "If-None-Match",
        "Racer-Metadata",
        "Authorization",
        "Racer-Durability",
        "Racer-Upload",
        "Racer-Hold-Seconds",
        "Racer-Content-Type",
    ] {
        let mut h = put(&[]);
        let value = match name {
            "Host" => "racer",
            "Content-Length" => "100",
            "If-None-Match" => "*",
            "Racer-Durability" => "origin",
            "Racer-Upload" => "auto",
            "Racer-Hold-Seconds" => "5",
            "Racer-Content-Type" => "text/plain",
            _ => "x",
        };
        h.headers.push(Header::new(name, value));
        h.headers
            .push(Header::new(name.to_ascii_lowercase(), value));
        assert_eq!(err(&h), INVALID, "{name}");
    }
    assert_eq!(err(&put(&[("Host", "<absent>")])), INVALID);
    assert_eq!(err(&put(&[("Host", "other")])), INVALID);
    let h = put(&[("X-Pad", &"a".repeat(200))]);
    assert_eq!(
        parse_write(&h, 128, PAGE).err(),
        Some(WriteError::Object(Error::HeaderTooLarge))
    );
    let h = put(&[("Racer-Metadata", &"m".repeat(MAX_FIELD_BYTES + 1))]);
    assert_eq!(err(&h), WriteError::Object(Error::HeaderTooLarge));
    let mut h = put(&[]);
    h.headers.push(Header::new("Bad Name", "x"));
    assert_eq!(err(&h), INVALID);
    let h = put(&[("X-Ctl", "a\u{1}b")]);
    assert_eq!(err(&h), INVALID);
    // Unknown fields are ignored, as on reads.
    assert!(parse(&put(&[("User-Agent", "t")])).is_ok());
}

#[test]
fn host_write_rules() {
    let h = host_write(&[]);
    let r = parse(&h).unwrap();
    assert_eq!(
        r.kind,
        WriteKind::HostWrite {
            source: Destination {
                offset: 4096,
                length: 8192
            }
        }
    );
    assert_eq!(r.kind.object_length(), Some(8192));
    let h = host_write(&[("Content-Length", "<absent>")]);
    assert!(parse(&h).is_ok());
    for fields in [
        &[("Content-Length", "1")][..],
        &[("Racer-Source-Offset", "<absent>")],
        &[("Racer-Source-Length", "<absent>")],
        &[("Racer-Source-Length", "0")],
        &[("Racer-Source-Offset", "9223372036854775807")],
    ] {
        assert_eq!(err(&host_write(fields)), INVALID, "{fields:?}");
    }
    let over = (max_object_bytes(PAGE) + 1).to_string();
    assert_eq!(
        err(&host_write(&[("Racer-Source-Length", &over)])),
        WriteError::PayloadTooLarge
    );
}

#[test]
fn delete_and_persist() {
    for (method, suffix, kind) in [
        ("DELETE", "", WriteKind::Delete),
        ("POST", "/persist", WriteKind::Persist),
    ] {
        let target = object(suffix);
        let h = head(method, &target, &[("Host", "racer")]);
        let r = parse(&h).unwrap();
        assert_eq!(r.kind, kind);
        assert!(!r.kind.creates() && r.kind.object_length().is_none());
        let h = head(
            method,
            &target,
            &[
                ("Host", "racer"),
                ("Content-Length", "0"),
                ("Authorization", "Bearer t"),
                ("Racer-Metadata", "m"),
            ],
        );
        assert!(parse(&h).is_ok());
        for field in [
            ("Content-Length", "5"),
            ("Racer-Durability", "origin"),
            ("Racer-Upload", "auto"),
            ("Racer-Hold-Seconds", "5"),
            ("Racer-Content-Type", "text/plain"),
            ("Racer-Source-Length", "5"),
        ] {
            let h = head(method, &target, &[("Host", "racer"), field]);
            assert_eq!(err(&h), INVALID, "{method} {field:?}");
        }
    }
}

#[test]
fn uppercase_key_is_accepted_only_if_the_key_parser_accepts_it() {
    let target = format!("/v1/objects/{}", KEY.to_ascii_uppercase());
    let h = head("DELETE", &target, &[("Host", "racer")]);
    assert!(is_write(&h));
    assert_eq!(
        parse(&h).is_ok(),
        CacheKey::parse_hex(KEY.to_ascii_uppercase().as_bytes()).is_ok()
    );
}

fn encode(head: &MessageHead) -> String {
    String::from_utf8(Codec::<()>::new(4096).encode_head(head).unwrap()).unwrap()
}

#[test]
fn golden_heads() {
    let etag = StrongEtag::test_value("racer-0123456789abcdef0123456789abcdef");
    assert_eq!(
        encode(&write_created_head(&etag, UploadState::Pending)),
        "HTTP/1.1 201 \r\nETag: \"racer-0123456789abcdef0123456789abcdef\"\r\nRacer-Upload-State: pending\r\nContent-Length: 0\r\n\r\n"
    );
    assert_eq!(
        encode(&write_error_head(
            503,
            Some(error_token::DIRTY_CAP),
            Some(1)
        )),
        "HTTP/1.1 503 \r\nContent-Length: 0\r\nRacer-Write-Error: dirty-cap\r\nRetry-After: 1\r\n\r\n"
    );
    assert_eq!(
        encode(&write_error_head(405, None, None)),
        "HTTP/1.1 405 \r\nContent-Length: 0\r\nAllow: HEAD, POST, PUT, DELETE\r\n\r\n"
    );
    assert_eq!(
        encode(&write_error_head(428, None, None)),
        "HTTP/1.1 428 \r\nContent-Length: 0\r\n\r\n"
    );
    assert_eq!(
        encode(&write_no_content_head(Some(UploadState::Uploading))),
        "HTTP/1.1 204 \r\nRacer-Upload-State: uploading\r\n\r\n"
    );
    assert_eq!(
        encode(&write_no_content_head(None)),
        "HTTP/1.1 204 \r\n\r\n"
    );
    assert_eq!(
        encode(&client::error_head_with_allow(405, None, true).unwrap()),
        encode(&write_error_head(405, None, None))
    );
    assert_eq!(
        encode(&client::error_head(405, None).unwrap()),
        "HTTP/1.1 405 \r\nContent-Length: 0\r\nAllow: HEAD, POST\r\n\r\n"
    );
}

#[test]
fn head_with_upload_state() {
    let metadata = ObjectMetadata {
        content_type: None,
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("c".into()),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value("racer-1"),
        },
        length: 5,
        expires_at: ExpiresAt::test_time(UNIX_EPOCH + Duration::from_millis(1234)),
    };
    let with = success_head_with_upload(&metadata, Some(UploadState::Manual)).unwrap();
    let names: Vec<_> = with.headers.iter().map(|h| h.name.as_str()).collect();
    assert_eq!(
        names,
        [
            "ETag",
            "Racer-Expires-At",
            "Racer-Upload-State",
            "Content-Length",
            "Content-Type"
        ]
    );
    let without = success_head_with_upload(&metadata, None).unwrap();
    assert_eq!(
        encode(&without),
        encode(&client::success_head(&metadata, None).unwrap())
    );
}

#[test]
fn error_statuses() {
    assert_eq!(WriteError::LengthRequired.status(), Some(411));
    assert_eq!(WriteError::PayloadTooLarge.status(), Some(413));
    assert_eq!(WriteError::PreconditionRequired.status(), Some(428));
    assert_eq!(INVALID.status(), None);
}
