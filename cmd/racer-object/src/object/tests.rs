// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

/// Golden values match the keys computed by the in-tree `internal/racerobject`
/// library, so both produce the same Racer keys.
const GOLDEN: &[(&str, &str, &str, &str, &str)] = &[
    (
        "bucket",
        "key",
        "",
        r#"{"schema":1,"bucket":"bucket","key":"key"}"#,
        "cb89dfbcde99d6d99a2e75545d3a21cc35db85bcf3ffa2a336412ab5a0320bc3",
    ),
    (
        "bucket",
        "a/../b%2Fc +\u{96ea}",
        "v+/=",
        "{\"schema\":1,\"bucket\":\"bucket\",\"key\":\"a/../b%2Fc +\u{96ea}\",\"versionId\":\"v+/=\"}",
        "ad43dd1e70a3fe621e2e56aa14bf1281831d356cdb78cbcf9699fc8c46ed8d8b",
    ),
    (
        "Legacy_Bucket-9.example",
        "key",
        "v\x00\n\u{96ea}",
        "{\"schema\":1,\"bucket\":\"Legacy_Bucket-9.example\",\"key\":\"key\",\"versionId\":\"v\\u0000\\n\u{96ea}\"}",
        "5d484d5af661f9ca26f33c7b711c0f17ea82610339011b27816a61a2311e37ee",
    ),
    (
        "b",
        "<>&\"\\\x08\x0c\n\r\t\x01\x1f\u{2028}\u{2029}~",
        "",
        "{\"schema\":1,\"bucket\":\"b\",\"key\":\"\\u003c\\u003e\\u0026\\\"\\\\\\b\\f\\n\\r\\t\\u0001\\u001f\\u2028\\u2029~\"}",
        "1dbb4f01646f43169be73c14f3803e78234dd166fd35eb9fd819e9459b6bb739",
    ),
    (
        "b",
        "line\nbreak",
        "null",
        "{\"schema\":1,\"bucket\":\"b\",\"key\":\"line\\nbreak\",\"versionId\":\"null\"}",
        "509b375fc0e364d59524952fa4932130d55de3504b97a5306ce417c4b1562b14",
    ),
    (
        "b",
        "\u{96ea}/\u{2603} \u{e9}",
        "v",
        "{\"schema\":1,\"bucket\":\"b\",\"key\":\"\u{96ea}/\u{2603} \u{e9}\",\"versionId\":\"v\"}",
        "160147cded6f67068952e25a91591f5f04ced6a80f4ecab8e7549efcfc0d9911",
    ),
];

#[test]
fn canonical_golden() {
    for (bucket, key, version, json, hash) in GOLDEN {
        let id = identity(bucket.as_bytes(), key.as_bytes(), version.as_bytes())
            .unwrap_or_else(|| panic!("rejected {key:?}"));
        assert_eq!(std::str::from_utf8(&id.metadata).unwrap(), *json);
        assert_eq!(id.key_str(), *hash);
    }
    let nulls = "\0".repeat(300);
    let id = identity(b"b", nulls.as_bytes(), b"").unwrap();
    assert_eq!(
        id.key_str(),
        "050416725502b834b502b1725e7f5caa15389b942a17b54761fc4185a54ab93e"
    );
    assert_eq!(id.metadata.len(), 34 + 300 * 6);
}

#[test]
fn identity_distinct_and_omits_empty_version() {
    let id = identity(b"bucket", "a/../b%2Fc +\u{96ea}".as_bytes(), b"v+/=").unwrap();
    for (b, k, v) in [
        ("other", "a/../b%2Fc +\u{96ea}", "v+/="),
        ("bucket", "b%2Fc +\u{96ea}", "v+/="),
        ("bucket", "a/../b/c +\u{96ea}", "v+/="),
        ("bucket", "a/../b%2Fc +\u{96ea}", ""),
    ] {
        let other = identity(b.as_bytes(), k.as_bytes(), v.as_bytes()).unwrap();
        assert_ne!(other.key, id.key);
    }
    let plain = identity(b"bucket", b"key", b"").unwrap();
    assert!(
        !String::from_utf8(plain.metadata)
            .unwrap()
            .contains("versionId")
    );
}

#[test]
fn identity_validation() {
    let long_key = "k".repeat(1025);
    let long_version = "v".repeat(1025);
    let long_bucket = "b".repeat(256);
    let bad: &[(&[u8], &[u8], &[u8])] = &[
        (b"", b"key", b""),
        (b"with space", b"key", b""),
        (b"bucket\n", b"key", b""),
        (b"\xff", b"key", b""),
        (b"https://evil", b"key", b""),
        (b"../bucket", b"key", b""),
        (b"bucket", b"", b""),
        (b"bucket", long_key.as_bytes(), b""),
        (b"bucket", b"\xff", b""),
        (b"bucket", b"key", b"\xff"),
        (b"bucket", b"key", long_version.as_bytes()),
        ("bucket\u{a0}".as_bytes(), b"key", b""),
        (b"bucket\x00", b"key", b""),
        (b".", b"key", b""),
        (b"..", b"key", b""),
        (long_bucket.as_bytes(), b"key", b""),
        ("buck\u{e9}t".as_bytes(), b"key", b""),
        // DEL survives JSON but Racer rejects it as metadata.
        (b"bucket", b"a\x7fb", b""),
    ];
    for (b, k, v) in bad {
        assert!(identity(b, k, v).is_none(), "{b:?} {k:?} {v:?}");
    }
    let max_key = "k".repeat(1024);
    for key in [
        "/",
        ".",
        "../",
        "a//b",
        "a\x00b",
        "line\nbreak",
        max_key.as_str(),
    ] {
        assert!(
            identity(b"bucket", key.as_bytes(), b"null").is_some(),
            "{key:?}"
        );
    }
    let (b, k, v) = ("b".repeat(255), "k".repeat(1024), "v".repeat(1024));
    assert!(identity(b.as_bytes(), k.as_bytes(), v.as_bytes()).is_some());
    // Both fit the object limits, but escaping exceeds the 8 KiB field cap.
    let nulls = "\0".repeat(1024);
    assert!(identity(b"bucket", nulls.as_bytes(), nulls.as_bytes()).is_none());
}

fn split(target: &str) -> Option<(Vec<u8>, Vec<u8>)> {
    let t = split_target(target.as_bytes())?;
    let (mut b, mut k) = (Vec::new(), Vec::new());
    bucket_key(t.path, &mut b, &mut k).then_some((b, k))
}

#[test]
fn paths() {
    let ok = |target: &str, bucket: &str, key: &[u8]| {
        let (b, k) = split(target).unwrap_or_else(|| panic!("{target}"));
        assert_eq!(b, bucket.as_bytes(), "{target}");
        assert_eq!(k, key, "{target}");
    };
    ok("/bucket/key", "bucket", b"key");
    ok("/bucket/a%2Fb", "bucket", b"a/b");
    ok("/bucket/a//b/../c", "bucket", b"a//b/../c");
    ok("/bucket/%2Fleading", "bucket", b"/leading");
    ok("/bucket/a+b%20c", "bucket", b"a+b c");
    ok("/bucket/%E9%9B%AA", "bucket", "\u{96ea}".as_bytes());
    ok("/bucket/trailing/", "bucket", b"trailing/");
    ok("/bucket/a%3Fb%23c", "bucket", b"a?b#c");
    ok("/bucket/a%25zz", "bucket", b"a%zz");
    ok("http://host:80/bucket/key?x", "bucket", b"key");
    // An invalid raw encoding (here a raw '|') re-encodes the decoded
    // path, so an escaped slash becomes a separator.
    ok("/a%2Fb/c|d", "a", b"b/c|d");
    for bad in ["/", "/bucket", "/bucket/", "/bucket%2Fkey", "//key", "*"] {
        assert!(split(bad).is_none(), "{bad}");
    }
    for invalid in ["bucket/key", "/bucket/%zz", "/bucket/%f", "/a\x01b/c"] {
        assert!(split_target(invalid.as_bytes()).is_none(), "{invalid}");
    }
}

#[test]
fn s3_queries() {
    let v = |q: &str, head: bool| s3_version(q.as_bytes(), head);
    assert_eq!(v("", false), Ok(Vec::new()));
    assert_eq!(v("versionId=version%2B1", false), Ok(b"version+1".to_vec()));
    assert_eq!(v("versionId=a+b", false), Ok(b"a b".to_vec()));
    assert_eq!(v("x-id=GetObject&versionId=v", false), Ok(b"v".to_vec()));
    assert_eq!(v("x-id=HeadObject", true), Ok(Vec::new()));
    for (q, want) in [
        ("list-type=2", Failure::NotImplemented),
        ("acl", Failure::BadRequest),
        ("partNumber=1", Failure::NotImplemented),
        ("response-content-type=text/plain", Failure::NotImplemented),
        ("X-Amz-Signature=secret", Failure::NotImplemented),
        ("versionId=", Failure::BadRequest),
        ("versionId=1&versionId=2", Failure::BadRequest),
        ("versionId=%zz", Failure::BadRequest),
        ("versionId=a;b", Failure::BadRequest),
        ("versionId=a%0D%0AX-Injected%3Ayes", Failure::BadRequest),
        ("versionId=a%00", Failure::BadRequest),
        ("versionId=a%7F", Failure::BadRequest),
        ("x-id=ListObjects", Failure::NotImplemented),
        ("x-id=HeadObject", Failure::NotImplemented),
        // 501: the unsupported name is reported before the
        // control-character check.
        ("foo=1&versionId=%00", Failure::NotImplemented),
    ] {
        assert_eq!(v(q, false), Err(want), "{q}");
    }
    assert_eq!(v("x-id=GetObject", true), Err(Failure::NotImplemented));
    // Empty segments are skipped.
    assert_eq!(v("&versionId=v&", false), Ok(b"v".to_vec()));
}

#[test]
fn azure_queries() {
    let v = |q: &str| azure_version(q.as_bytes());
    assert_eq!(v("versionid=v%2B%2F%25&timeout=30"), Ok(b"v+/%".to_vec()));
    assert_eq!(v("timeout=001"), Ok(Vec::new()));
    for (q, want) in [
        ("versionid=", Failure::BadRequest),
        ("versionid=a&versionid=b", Failure::BadRequest),
        ("versionid=%zz", Failure::BadRequest),
        ("versionid=a;b", Failure::BadRequest),
        ("versionid=%00", Failure::BadRequest),
        ("versionid=%7F", Failure::BadRequest),
        ("versionid=%FF", Failure::BadRequest),
        ("timeout=0", Failure::BadRequest),
        ("timeout=-1", Failure::BadRequest),
        ("timeout=1.5", Failure::BadRequest),
        ("timeout=%2B1", Failure::BadRequest),
        ("timeout=", Failure::BadRequest),
        ("timeout=1&timeout=2", Failure::BadRequest),
        ("timeout=18446744073709551616", Failure::BadRequest),
        ("snapshot=x", Failure::NotImplemented),
        ("sig=secret", Failure::NotImplemented),
        ("versionId=x", Failure::NotImplemented),
        ("unknown=", Failure::BadRequest),
        ("unknown=a&unknown=b", Failure::BadRequest),
        ("comp=metadata&timeout=bad", Failure::BadRequest),
        ("=x", Failure::BadRequest),
    ] {
        assert_eq!(v(q), Err(want), "{q}");
    }
}

#[test]
fn headers() {
    for name in [
        "x-amz-expected-bucket-owner",
        "X-Amz-Server-Side-Encryption",
        "X-AMZ-CHECKSUM-MODE",
        "x-amz-meta-custom",
        "If-Modified-Since",
        "if-unmodified-since",
        "IF-RANGE",
    ] {
        assert!(!supported_header(Api::S3, name.as_bytes()), "{name}");
    }
    for name in [
        "Authorization",
        "X-Amz-Date",
        "x-amz-content-sha256",
        "X-Amz-Security-Token",
        "X-Amz-Region-Set",
        "X-Amz-S3session-Token",
        "X-Amz-User-Agent",
        "Amz-Sdk-Invocation-Id",
        "x-ms-lease-id",
    ] {
        assert!(supported_header(Api::S3, name.as_bytes()), "{name}");
    }
    for name in [
        "x-ms-lease-id",
        "x-ms-encryption-key",
        "X-Ms-If-Tags",
        "x-ms-unknown",
        "If-Range",
    ] {
        assert!(!supported_header(Api::Azure, name.as_bytes()), "{name}");
    }
    for name in [
        "x-ms-range",
        "X-Ms-Version",
        "x-ms-date",
        "x-ms-client-request-id",
        "x-ms-return-client-request-id",
        "x-ms-useragent",
        "x-amz-anything",
        "User-Agent",
    ] {
        assert!(supported_header(Api::Azure, name.as_bytes()), "{name}");
    }
}

#[test]
fn conditions() {
    let tag = br#""a,b\c""#;
    let cond = |v: &str| parse_condition(&[v.as_bytes()]);
    let m = |v: &str| cond(v).map(|c| c.matches(tag, false));
    let n = |v: &str| cond(v).map(|c| c.matches(tag, true));
    assert_eq!(m(r#""a,b\c""#), Some(true));
    assert_eq!(m(r#""other", "a,b\c""#), Some(true));
    assert_eq!(m("*"), Some(true));
    assert_eq!(m(r#""other""#), Some(false));
    assert_eq!(m(r#"W/"a,b\c""#), Some(false));
    assert_eq!(n(r#"W/"a,b\c""#), Some(true));
    assert_eq!(n(r#""other", W/"a,b\c""#), Some(true));
    assert_eq!(m(r#""""#), Some(false));
    assert_eq!(m(r#", "a,b\c",,"#), Some(true));
    for bad in [
        "bare",
        r#""x"#,
        r#""a,b\c" suffix"#,
        r#"*, "a,b\c""#,
        r#""a b""#,
        ",,",
        "",
    ] {
        assert_eq!(cond(bad), None, "{bad:?}");
    }
    // Multiple header lines form one list.
    let c = parse_condition(&[b"\"other\"", tag]).unwrap();
    assert!(c.matches(tag, false));
    assert_eq!(parse_condition(&[]), Some(Condition::default()));
}

fn range(v: &str, size: u64) -> Result<(u64, u64), u16> {
    let spec = parse_range(&[v.as_bytes()]).map_err(Failure::status)?;
    let s = spec.resolve(size).map_err(Failure::status)?;
    assert!(s.partial);
    Ok((s.offset, s.length))
}

#[test]
fn ranges() {
    for (v, want) in [
        ("bytes=2-5", (2, 4)),
        ("bytes=7-", (7, 3)),
        ("bytes=-3", (7, 3)),
        ("bytes=8-999", (8, 2)),
        ("bytes=-999", (0, 10)),
        ("bytes=0-0", (0, 1)),
        (" bytes=2-5\t", (2, 4)),
        ("bytes=8-9223372036854775808", (8, 2)),
        ("bytes=-18446744073709551615", (0, 10)),
    ] {
        assert_eq!(range(v, 10), Ok(want), "{v}");
    }
    for (v, want) in [
        ("bytes=4-", 416),
        ("bytes=-0", 416),
        ("bytes=999-1000", 416),
        ("bytes=2-1", 400),
        ("bytes=0-1,2-3", 400),
        ("items=0-1", 400),
        ("bytes=", 400),
        ("bytes=-", 400),
        ("bytes=+1-2", 400),
        ("bytes=0-+1", 400),
        ("bytes=0-1-2", 400),
        ("bytes= 0-1", 400),
        ("bytes=18446744073709551616-", 400),
        ("bytes=9223372036854775808-", 416),
        ("bytes=18446744073709551615-", 416),
        ("bytes=-bad", 400),
        ("bytes=-18446744073709551616", 400),
        ("", 400),
    ] {
        assert_eq!(range(v, 4), Err(want), "{v}");
    }
    assert_eq!(
        parse_range(&[b"bytes=0-1", b"bytes=2-3"]),
        Err(Failure::BadRequest)
    );
    assert_eq!(
        RangeSpec::Suffix(1).resolve(0),
        Err(Failure::RangeNotSatisfiable(Some(0)))
    );
    assert_eq!(
        RangeSpec::From(0).resolve(0),
        Err(Failure::RangeNotSatisfiable(Some(0)))
    );
    let full = RangeSpec::Full.resolve(0).unwrap();
    assert!(!full.partial && full.length == 0);
}

#[test]
fn range_int64_bounds() {
    let size = i64::MAX as u64;
    for (v, want) in [
        ("bytes=0-18446744073709551615", Ok((0, size))),
        ("bytes=9223372036854775806-", Ok((size - 1, 1))),
        (
            "bytes=9223372036854775806-18446744073709551615",
            Ok((size - 1, 1)),
        ),
        ("bytes=-18446744073709551615", Ok((0, size))),
        ("bytes=-1", Ok((size - 1, 1))),
        ("bytes=9223372036854775807-", Err(416)),
        ("bytes=18446744073709551615-", Err(416)),
        ("bytes=18446744073709551616-", Err(400)),
    ] {
        assert_eq!(range(v, size), want, "{v}");
    }
}

#[test]
fn racer_range_headers() {
    let header = |r: RangeSpec| {
        let mut out = Vec::new();
        r.racer_header(&mut out)
            .then(|| String::from_utf8(out).unwrap())
    };
    assert_eq!(header(RangeSpec::Full), None);
    assert_eq!(header(RangeSpec::Suffix(3)).as_deref(), Some("bytes=-3"));
    assert_eq!(header(RangeSpec::From(7)).as_deref(), Some("bytes=7-"));
    assert_eq!(
        header(RangeSpec::Closed(0, 18)).as_deref(),
        Some("bytes=0-18")
    );
}

#[test]
fn failure_codes() {
    for (f, status, s3, azure) in [
        (Failure::BadRequest, 400, "InvalidArgument", "InvalidInput"),
        (
            Failure::Forbidden,
            403,
            "AccessDenied",
            "AuthorizationFailure",
        ),
        (Failure::NotFound, 404, "NoSuchKey", "BlobNotFound"),
        (
            Failure::MethodNotAllowed,
            405,
            "MethodNotAllowed",
            "UnsupportedHttpVerb",
        ),
        (
            Failure::PreconditionFailed,
            412,
            "PreconditionFailed",
            "ConditionNotMet",
        ),
        (
            Failure::RangeNotSatisfiable(None),
            416,
            "InvalidRange",
            "InvalidRange",
        ),
        (
            Failure::HeaderTooLarge,
            431,
            "RequestHeaderSectionTooLarge",
            "InvalidInput",
        ),
        (Failure::Internal, 500, "InternalError", "InternalError"),
        (
            Failure::NotImplemented,
            501,
            "NotImplemented",
            "NotImplemented",
        ),
        (Failure::BadGateway, 502, "BadGateway", "BadGateway"),
        (
            Failure::Unavailable,
            503,
            "ServiceUnavailable",
            "ServerBusy",
        ),
    ] {
        assert_eq!(f.status(), status);
        assert_eq!(f.code(Api::S3), s3);
        assert_eq!(f.code(Api::Azure), azure);
        assert!(!status_text(status).is_empty());
    }
}

#[test]
fn many_distinct_query_names_are_grouped_linearly() {
    let start = std::time::Instant::now();
    let names: Vec<String> = (0..8000).map(|i| format!("p{i}=1")).collect();
    let query = names.join("&");
    assert!(query.len() <= 64 * 1024);
    assert_eq!(
        s3_version(query.as_bytes(), false),
        Err(Failure::NotImplemented)
    );
    assert_eq!(
        azure_version(query.as_bytes()),
        Err(Failure::NotImplemented)
    );
    let bad = format!("{query}&p0=2");
    assert_eq!(s3_version(bad.as_bytes(), false), Err(Failure::BadRequest));
    let grouped_pairs = parse_query(query.as_bytes()).unwrap();
    let groups = grouped(&grouped_pairs);
    assert_eq!(groups.len(), 8000);
    assert_eq!(groups[0].0, b"p0");
    assert_eq!(groups[7999].0, b"p7999");
    assert!(start.elapsed() < std::time::Duration::from_secs(1));
}
