// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! End-to-end tests over TCP against an in-process fake Racer. Cases are
//! ported from the Go racerobject and racer-object tests.

mod common;

use common::*;
use racer_object::object::Api;
use std::time::Duration;

const TAG: &str = r#""opaque,tag\\value""#;

async fn setup(api: Api, data: &str, tag: &str) -> (FakeRacer, Sidecar) {
    let racer = FakeRacer::start().await;
    racer.put("bucket", "key", "", Object::new(data, tag));
    let sidecar = Sidecar::start(&racer, api).await;
    (racer, sidecar)
}

#[tokio::test]
async fn s3_reads() {
    let racer = FakeRacer::start().await;
    racer.put("bucket", "key", "version+1", Object::new("0123456789", TAG));
    let s = Sidecar::start(&racer, Api::S3).await;
    for (name, method, range, body, content_range, status) in [
        ("full", "GET", "", "0123456789", "", 200),
        ("head", "HEAD", "", "", "", 200),
        ("bounded", "GET", "bytes=2-5", "2345", "bytes 2-5/10", 206),
        ("open", "GET", "bytes=7-", "789", "bytes 7-9/10", 206),
        ("suffix", "GET", "bytes=-3", "789", "bytes 7-9/10", 206),
        ("clamped", "GET", "bytes=8-999", "89", "bytes 8-9/10", 206),
        (
            "large suffix",
            "GET",
            "bytes=-999",
            "0123456789",
            "bytes 0-9/10",
            206,
        ),
        ("one byte", "GET", "bytes=0-0", "0", "bytes 0-0/10", 206),
        ("head range", "HEAD", "bytes=2-5", "", "bytes 2-5/10", 206),
        (
            "clamped past int64",
            "GET",
            "bytes=8-9223372036854775808",
            "89",
            "bytes 8-9/10",
            206,
        ),
        (
            "suffix past int64",
            "GET",
            "bytes=-18446744073709551615",
            "0123456789",
            "bytes 0-9/10",
            206,
        ),
    ] {
        let before = (racer.heads(), racer.posts());
        let mut headers = vec![("Authorization", "AWS4-HMAC-SHA256 incoming-secret")];
        if !range.is_empty() {
            headers.push(("Range", range));
        }
        let r = s
            .get(method, "/bucket/key?versionId=version%2B1", &headers)
            .await;
        assert_eq!((r.status, r.text().as_str()), (status, body), "{name}");
        assert_eq!(r.header("etag"), Some(TAG), "{name}");
        assert_eq!(
            r.header("content-range").unwrap_or(""),
            content_range,
            "{name}"
        );
        assert_eq!(r.header("content-type"), Some("text/plain"), "{name}");
        assert_eq!(r.header("accept-ranges"), Some("bytes"), "{name}");
        assert_eq!(r.header("x-amz-version-id"), Some("version+1"), "{name}");
        assert!(r.header("date").is_some());
        let length = if method == "HEAD" {
            if range.is_empty() { 10 } else { 4 }
        } else {
            body.len()
        };
        assert_eq!(
            r.header("content-length"),
            Some(length.to_string().as_str()),
            "{name}"
        );
        let after = (racer.heads(), racer.posts());
        if method == "HEAD" {
            assert_eq!(
                after,
                (before.0 + 1, before.1),
                "{name}: HEAD opened a stream"
            );
        } else {
            // Unconditional GETs skip Stat entirely.
            assert_eq!(after, (before.0, before.1 + 1), "{name}");
        }
    }
    // Credentials are never forwarded; the identity is canonical JSON.
    for seen in racer.seen() {
        assert_eq!(
            seen.metadata,
            r#"{"schema":1,"bucket":"bucket","key":"key","versionId":"version+1"}"#
        );
    }
}

#[tokio::test]
async fn s3_conditions() {
    let tag = r#""a,b\c""#;
    let (racer, s) = setup(Api::S3, "data", tag).await;
    let other_list = format!(r#""other", {tag}"#);
    let weak = format!("W/{tag}");
    let none_list = format!(r#""other", W/{tag}"#);
    let bad_list = format!("{tag} suffix");
    let wildcard_list = format!("*, {tag}");
    let empty_elements = format!(", {tag},,");
    for (name, m, n, status) in [
        ("match", tag, "", 200),
        ("match list", other_list.as_str(), "", 200),
        ("match wildcard", "*", "", 200),
        ("match absent", r#""other""#, "", 412),
        ("weak match fails", weak.as_str(), "", 412),
        ("none", "", tag, 304),
        ("none weak", "", weak.as_str(), 304),
        ("none list", "", none_list.as_str(), 304),
        ("none wildcard", "", "*", 304),
        ("none absent", "", r#""other""#, 200),
        ("match precedence", r#""other""#, "*", 412),
        ("both", "*", tag, 304),
        ("unquoted", "bare", "", 400),
        ("unterminated", r#""x"#, "", 400),
        ("bad list", bad_list.as_str(), "", 400),
        ("wildcard list", wildcard_list.as_str(), "", 400),
        ("space in tag", r#""a b""#, "", 400),
        ("empty list", ",,", "", 400),
        ("empty tag valid", r#""""#, "", 412),
        ("empty list elements", empty_elements.as_str(), "", 200),
    ] {
        for method in ["GET", "HEAD"] {
            let before = racer.posts();
            let mut headers = Vec::new();
            if !m.is_empty() {
                headers.push(("If-Match", m));
            }
            if !n.is_empty() {
                headers.push(("If-None-Match", n));
            }
            let r = s.get(method, "/bucket/key", &headers).await;
            assert_eq!(r.status, status, "{name} {method}: {}", r.text());
            if method == "HEAD" || status == 304 {
                assert!(r.body.is_empty(), "{name}");
            }
            if status == 304 {
                assert_eq!(r.header("etag"), Some(tag));
                assert_eq!(r.header("content-length"), None);
            }
            if status != 200 || method == "HEAD" {
                assert_eq!(racer.posts(), before, "{name}: condition opened a stream");
            }
            if status == 200 && method == "GET" {
                assert_eq!(r.text(), "data");
                // Conditional reads pin the Stat version.
                assert_eq!(racer.seen().last().unwrap().if_match.as_deref(), Some(tag));
            }
        }
    }
    let r = s
        .get(
            "GET",
            "/bucket/key",
            &[("If-Match", r#""other""#), ("If-Match", tag)],
        )
        .await;
    assert_eq!(r.status, 200, "multiple header lines are one list");
    let r = s.get("GET", "/bucket/key", &[("If-None-Match", "")]).await;
    assert_eq!(r.status, 400, "empty condition accepted");
}

fn assert_xml(r: &Response, method: &str, status: u16, code: &str) {
    assert_eq!(r.status, status, "{}", r.text());
    assert_eq!(r.header("content-type"), Some("application/xml"));
    let message = racer_object::object::status_text(status);
    let body = format!("<Error><Code>{code}</Code><Message>{message}</Message></Error>");
    assert_eq!(
        r.header("content-length"),
        Some(body.len().to_string().as_str())
    );
    if method == "HEAD" {
        assert!(r.body.is_empty());
    } else {
        assert_eq!(r.text(), body);
    }
}

#[tokio::test]
async fn s3_rejected_requests() {
    let (racer, s) = setup(Api::S3, "data", r#""v""#).await;
    let long = format!("/bucket/{}", "k".repeat(1025));
    for (method, target, headers, status) in [
        ("PUT", "/bucket/key", &[][..], 405),
        ("POST", "/bucket/key", &[], 405),
        ("DELETE", "/bucket/key", &[], 405),
        ("OPTIONS", "/bucket/key", &[], 405),
        ("get", "/bucket/key", &[], 405),
        ("GET", "/", &[], 400),
        ("GET", "/bucket", &[], 400),
        ("GET", "/bucket/", &[], 400),
        ("GET", "/bucket%2Fkey", &[], 400),
        ("GET", "/bucket/key?list-type=2", &[], 501),
        ("GET", "/bucket/key?acl", &[], 400),
        ("GET", "/bucket/key?partNumber=1", &[], 501),
        ("GET", "/bucket/key?X-Amz-Signature=secret", &[], 501),
        ("GET", "/bucket/key?versionId=", &[], 400),
        ("GET", "/bucket/key?versionId=1&versionId=2", &[], 400),
        ("GET", "/bucket/key?versionId=%zz", &[], 400),
        ("GET", "/bucket/key?versionId=a;b", &[], 400),
        (
            "GET",
            "/bucket/key?versionId=a%0D%0AX-Injected%3Ayes",
            &[],
            400,
        ),
        ("GET", "/bucket/key?versionId=%FF", &[], 400),
        ("GET", long.as_str(), &[], 400),
        ("GET", "/bucket/%FF", &[], 400),
        ("GET", "/bucket/a%7Fb", &[], 400),
        ("GET", "/bucket/key?x-id=ListObjects", &[], 501),
        ("HEAD", "/bucket/key?x-id=GetObject", &[], 501),
        (
            "GET",
            "/bucket/key",
            &[("If-Modified-Since", "Tue, 06 Oct 2026 00:00:00 GMT")],
            501,
        ),
        ("GET", "/bucket/key", &[("If-Unmodified-Since", "")], 501),
        ("HEAD", "/bucket/key", &[("If-Range", r#""v""#)], 501),
        (
            "GET",
            "/bucket/key",
            &[(
                "x-amz-server-side-encryption-customer-key",
                "private-semantic-value",
            )],
            501,
        ),
        ("HEAD", "/bucket/key", &[("X-AMZ-CHECKSUM-MODE", "")], 501),
    ] {
        let r = s.get(method, target, headers).await;
        let code = match status {
            400 => "InvalidArgument",
            405 => "MethodNotAllowed",
            _ => "NotImplemented",
        };
        assert_xml(&r, method, status, code);
        if status == 405 {
            assert_eq!(r.header("allow"), Some("GET, HEAD"));
        }
        assert!(!r.text().contains("secret") && !r.text().contains("bucket"));
    }
    assert_eq!(racer.calls(), 0, "invalid request reached Racer");
}

#[tokio::test]
async fn s3_auth_and_plumbing_headers() {
    let (_racer, s) = setup(Api::S3, "data", r#""v""#).await;
    let headers = [
        ("Authorization", "AWS4-HMAC-SHA256 ignored"),
        ("X-Amz-Date", "20261006T140000Z"),
        ("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD"),
        ("X-Amz-Security-Token", "ignored-session-token"),
        ("X-Amz-Region-Set", "us-east-1"),
        ("X-Amz-S3session-Token", "ignored-express-token"),
        ("X-Amz-User-Agent", "aws-sdk-test"),
        ("Amz-Sdk-Invocation-Id", "test-invocation"),
        ("Amz-Sdk-Request", "attempt=1; max=1"),
        ("User-Agent", "aws-sdk-test"),
        ("Accept-Encoding", "identity"),
    ];
    for method in ["GET", "HEAD"] {
        assert_eq!(s.get(method, "/bucket/key", &headers).await.status, 200);
    }
}

#[tokio::test]
async fn invalid_ranges() {
    let (racer, s) = setup(Api::S3, "data", r#""v""#).await;
    for (value, status) in [
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
        ("bytes=18446744073709551616-", 400),
        ("bytes=9223372036854775808-", 416),
        ("bytes=18446744073709551615-", 416),
        ("bytes=-bad", 400),
        ("", 400),
    ] {
        for method in ["GET", "HEAD"] {
            let r = s.get(method, "/bucket/key", &[("Range", value)]).await;
            let code = if status == 416 {
                "InvalidRange"
            } else {
                "InvalidArgument"
            };
            assert_xml(&r, method, status, code);
            if status == 416 {
                assert_eq!(r.header("content-range"), Some("bytes */4"), "{value}");
            }
        }
    }
    let r = s
        .get(
            "GET",
            "/bucket/key",
            &[("Range", "bytes=0-1"), ("Range", "bytes=2-3")],
        )
        .await;
    assert_eq!(r.status, 400);
    // Syntax errors still consult Racer first, as the Go sidecar's Stat did.
    let r = s
        .get("GET", "/bucket/missing", &[("Range", "bytes=2-1")])
        .await;
    assert_eq!(r.status, 404);
    assert!(racer.heads() > 0);
}

#[tokio::test]
async fn exact_identity() {
    for api in [Api::S3, Api::Azure] {
        let racer = FakeRacer::start().await;
        let s = Sidecar::start(&racer, api).await;
        let query = if api == Api::S3 {
            "versionId"
        } else {
            "versionid"
        };
        for (key, escaped) in [
            ("a//b/../c", "a//b/../c"),
            ("/leading", "%2Fleading"),
            ("./relative", "./relative"),
            ("a%2Fb", "a%252Fb"),
            ("a+b c", "a+b%20c"),
            ("\u{96ea}/\u{2603}", "%E9%9B%AA/%E2%98%83"),
            ("trailing/", "trailing/"),
            ("a?b#c", "a%3Fb%23c"),
        ] {
            racer.put("bucket", key, "version+/%", Object::new("body", r#""v""#));
            let r = s
                .get(
                    "GET",
                    &format!("/bucket/{escaped}?{query}=version%2B%2F%25"),
                    &[],
                )
                .await;
            assert_eq!((r.status, r.text().as_str()), (200, "body"), "{key}");
            let want = id("bucket", key, "version+/%");
            let seen = racer.seen();
            assert_eq!(seen.last().unwrap().key, want.key_str());
            assert_eq!(seen.last().unwrap().metadata.as_bytes(), want.metadata);
        }
    }
}

#[tokio::test]
async fn racer_failures() {
    for (status, want, s3, azure) in [
        (400, 400, "InvalidArgument", "InvalidInput"),
        (401, 403, "AccessDenied", "AuthorizationFailure"),
        (403, 403, "AccessDenied", "AuthorizationFailure"),
        (404, 404, "NoSuchKey", "BlobNotFound"),
        (412, 412, "PreconditionFailed", "ConditionNotMet"),
        (431, 400, "InvalidArgument", "InvalidInput"),
        (500, 502, "BadGateway", "BadGateway"),
        (502, 502, "BadGateway", "BadGateway"),
        (503, 503, "ServiceUnavailable", "ServerBusy"),
        (418, 502, "BadGateway", "BadGateway"),
    ] {
        for api in [Api::S3, Api::Azure] {
            let racer = FakeRacer::start().await;
            let mut o = Object::new("data", r#""v""#);
            o.head_status = Some(status);
            o.post_status = Some(status);
            racer.put("bucket", "key", "", o);
            let s = Sidecar::start(&racer, api).await;
            let code = if api == Api::S3 { s3 } else { azure };
            for method in ["GET", "HEAD"] {
                let r = s.get(method, "/bucket/key", &[]).await;
                assert_xml(&r, method, want, code);
                if api == Api::Azure {
                    assert_eq!(r.header("x-ms-error-code"), Some(code));
                    assert_eq!(r.header("x-ms-version"), Some("2026-02-06"));
                }
            }
        }
    }
}

#[tokio::test]
async fn racer_unavailable() {
    // No socket at all: dial fails, which is unavailable.
    let racer = FakeRacer::start().await;
    let mut options = options(Api::S3);
    options.request_timeout = Duration::from_secs(10);
    let s = Sidecar::start_with(&racer, options, false).await;
    std::fs::remove_file(&racer.path).unwrap();
    for method in ["GET", "HEAD"] {
        let r = s.get(method, "/bucket/key", &[]).await;
        assert_xml(&r, method, 503, "ServiceUnavailable");
    }
}

#[tokio::test]
async fn pinned_version_failure_and_changed_metadata() {
    for (mode, status, code) in [
        ("pin", 412, "PreconditionFailed"),
        ("size", 502, "BadGateway"),
        ("type", 502, "BadGateway"),
    ] {
        let racer = FakeRacer::start().await;
        let mut o = Object::new("data", r#""v""#);
        match mode {
            "pin" => o.post_status = Some(412),
            "size" => o.post_size_delta = 1,
            _ => o.post_content_type = Some("application/octet-stream".into()),
        }
        racer.put("bucket", "key", "", o);
        let s = Sidecar::start(&racer, Api::S3).await;
        // A condition forces Stat then a pinned subscription.
        let r = s.get("GET", "/bucket/key", &[("If-Match", "*")]).await;
        assert_xml(&r, "GET", status, code);
    }
}

#[tokio::test]
async fn empty_object() {
    let (_racer, s) = setup(Api::S3, "", r#""empty""#).await;
    for method in ["GET", "HEAD"] {
        let r = s.get(method, "/bucket/key", &[]).await;
        assert_eq!((r.status, r.body.len()), (200, 0));
        assert_eq!(r.header("content-length"), Some("0"));
        let r = s.get(method, "/bucket/key", &[("Range", "bytes=0-")]).await;
        assert_eq!(r.status, 416);
        assert_eq!(r.header("content-range"), Some("bytes */0"));
        let r = s
            .get(
                method,
                "/bucket/key",
                &[("Range", "bytes=0-"), ("If-Match", "*")],
            )
            .await;
        assert_eq!(r.status, 416);
        assert_eq!(r.header("content-range"), Some("bytes */0"));
    }
}

#[tokio::test]
async fn small_failures_become_errors() {
    for (truncate, corrupt, status) in [
        (Some(10), false, 503),
        (Some(0), false, 503),
        (None, true, 502),
    ] {
        for data in ["", "data"] {
            let racer = FakeRacer::start().await;
            let mut o = Object::new(data, r#""v""#);
            o.truncate_at = truncate;
            o.corrupt_frame = corrupt && !data.is_empty();
            if corrupt && data.is_empty() {
                o.truncate_at = Some(0);
            }
            racer.put("bucket", "key", "", o);
            let s = Sidecar::start(&racer, Api::S3).await;
            let r = s.get("GET", "/bucket/key", &[]).await;
            let want = if corrupt && data.is_empty() {
                503
            } else {
                status
            };
            assert_eq!(r.status, want, "{truncate:?} {corrupt} {data:?}");
            assert!(r.text().contains("<Error>"));
        }
    }
}

async fn large_case(
    size: usize,
    truncate: Option<usize>,
    corrupt: bool,
    range: Option<&str>,
) -> (Response, Vec<u8>) {
    let racer = FakeRacer::start().await;
    let data: Vec<u8> = (0..size).map(|i| (i * 7 % 251) as u8).collect();
    let mut o = Object::new(data.clone(), r#""big""#);
    o.truncate_at = truncate;
    o.corrupt_frame = corrupt;
    racer.put("bucket", "key", "", o);
    let s = Sidecar::start(&racer, Api::S3).await;
    let headers: Vec<(&str, &str)> = range.map(|r| vec![("Range", r)]).unwrap_or_default();
    let mut client = Client::connect(s.addr).await;
    let r = client.request("GET", "/bucket/key", &headers).await;
    (r, data)
}

#[tokio::test]
async fn large_bodies_splice() {
    // Multi-page (crosses the 16 MiB page boundary) and large single page.
    for size in [(PAGE as usize) * 2 + 12345, 3 << 20] {
        let (r, data) = large_case(size, None, false, None).await;
        assert_eq!(r.status, 200);
        assert!(!r.truncated);
        assert_eq!(r.body.len(), data.len());
        assert!(r.body == data, "body mismatch for {size}");
    }
    let (r, data) = large_case(PAGE as usize + 100, None, false, Some("bytes=1000-")).await;
    assert_eq!(r.status, 206);
    assert_eq!(
        r.header("content-range").unwrap(),
        format!("bytes 1000-{}/{}", data.len() - 1, data.len())
    );
    assert!(r.body == data[1000..]);
}

#[tokio::test]
async fn cross_page_small_range() {
    let first = PAGE as usize - 4;
    let range = format!("bytes={}-{}", first, first + 10);
    let (r, data) = large_case(PAGE as usize + 32, None, false, Some(&range)).await;
    assert_eq!(r.status, 206);
    assert_eq!(r.body, data[first..first + 11]);
}

#[tokio::test]
async fn large_late_failure_aborts() {
    for (truncate, corrupt) in [
        (Some(1 << 20), false),
        // Everything but the completion frame: the final byte is withheld.
        (Some((2 << 20) + 21), false),
        (Some((2 << 20) + 21 + 10), false),
        (None, true),
    ] {
        let (r, data) = large_case(2 << 20, truncate, corrupt, None).await;
        if corrupt {
            // A corrupt first frame fails before anything is committed.
            assert_eq!(r.status, 502);
            continue;
        }
        assert_eq!(r.status, 200, "{truncate:?}");
        assert!(r.truncated, "{truncate:?}: late failure completed");
        assert!(r.body.len() < data.len());
        assert!(!r.text().contains("<Error>"));
    }
}

#[tokio::test]
async fn keep_alive_and_pipelining() {
    let (racer, s) = setup(Api::S3, "0123456789", r#""v""#).await;
    let mut c = Client::connect(s.addr).await;
    for _ in 0..3 {
        assert_eq!(
            c.request("GET", "/bucket/key", &[]).await.text(),
            "0123456789"
        );
        assert_eq!(c.request("HEAD", "/bucket/key", &[]).await.status, 200);
    }
    // Pipelined: three requests in one write, responses in order.
    let mut batch = String::new();
    for range in ["bytes=0-0", "bytes=1-1", "bytes=2-2"] {
        batch.push_str(&format!(
            "GET /bucket/key HTTP/1.1\r\nHost: x\r\nRange: {range}\r\n\r\n"
        ));
    }
    batch.push_str("HEAD /bucket/key HTTP/1.1\r\nHost: x\r\n\r\n");
    tokio::io::AsyncWriteExt::write_all(&mut c.stream, batch.as_bytes())
        .await
        .unwrap();
    for want in ["0", "1", "2"] {
        assert_eq!(c.response(false).await.text(), want);
    }
    assert_eq!(c.response(true).await.header("content-length"), Some("10"));
    // Stat connections are pooled: far fewer dials than HEADs.
    let dials = racer
        .state
        .connections
        .load(std::sync::atomic::Ordering::Relaxed);
    assert!(dials < racer.heads() + racer.posts(), "dials {dials}");
}

#[tokio::test]
async fn connection_semantics() {
    let (_racer, s) = setup(Api::S3, "data", r#""v""#).await;
    // Connection: close.
    let mut c = Client::connect(s.addr).await;
    let r = c
        .request("GET", "/bucket/key", &[("Connection", "close")])
        .await;
    assert_eq!(r.header("connection"), Some("close"));
    assert!(c.closed().await);
    // A request body is answered, then the connection closes.
    let mut c = Client::connect(s.addr).await;
    let r = c
        .request("GET", "/bucket/key", &[("Content-Length", "4")])
        .await;
    assert_eq!(r.status, 200);
    assert_eq!(r.header("connection"), Some("close"));
    // HTTP/1.0: close by default, keep-alive when asked.
    let mut c = Client::connect(s.addr).await;
    tokio::io::AsyncWriteExt::write_all(&mut c.stream, b"GET /bucket/key HTTP/1.0\r\n\r\n")
        .await
        .unwrap();
    let r = c.response(false).await;
    assert_eq!((r.status, r.header("connection")), (200, Some("close")));
    assert!(c.closed().await);
    let mut c = Client::connect(s.addr).await;
    for _ in 0..2 {
        tokio::io::AsyncWriteExt::write_all(
            &mut c.stream,
            b"GET /bucket/key HTTP/1.0\r\nConnection: keep-alive\r\n\r\n",
        )
        .await
        .unwrap();
        let r = c.response(false).await;
        assert_eq!(
            (r.status, r.header("connection")),
            (200, Some("keep-alive"))
        );
    }
    // HTTP/1.1 without Host is a 400.
    let mut c = Client::connect(s.addr).await;
    tokio::io::AsyncWriteExt::write_all(&mut c.stream, b"GET /bucket/key HTTP/1.1\r\n\r\n")
        .await
        .unwrap();
    assert_eq!(c.response(false).await.status, 400);
    assert!(c.closed().await);
    // Garbage is a 400.
    let mut c = Client::connect(s.addr).await;
    tokio::io::AsyncWriteExt::write_all(&mut c.stream, b"\x01\x02 nonsense\r\n\r\n")
        .await
        .unwrap();
    assert_eq!(c.response(false).await.status, 400);
    // Invalid escapes in the target are rejected before method checks.
    let r = s.get("PUT", "/bucket/%zz", &[]).await;
    assert_eq!(r.status, 400);
    // Absolute-form targets work.
    let r = s.get("GET", "http://localhost/bucket/key", &[]).await;
    assert_eq!((r.status, r.text().as_str()), (200, "data"));
    // OPTIONS * is answered like net/http.
    let r = s.get("OPTIONS", "*", &[]).await;
    assert_eq!((r.status, r.header("content-length")), (200, Some("0")));
}

#[tokio::test]
async fn header_too_large() {
    for api in [Api::S3, Api::Azure] {
        let (_racer, s) = setup(api, "data", r#""v""#).await;
        let big = "x".repeat(40 << 10);
        let mut c = Client::connect(s.addr).await;
        let r = c.request("GET", "/bucket/key", &[("X-Big", &big)]).await;
        let code = if api == Api::S3 {
            "RequestHeaderSectionTooLarge"
        } else {
            "InvalidInput"
        };
        assert_xml(&r, "GET", 431, code);
        assert_eq!(r.header("connection"), Some("close"));
        // Just under Go's effective limit is fine.
        let ok = "x".repeat(32 << 10);
        let r = s.get("GET", "/bucket/key", &[("X-Ok", &ok)]).await;
        assert_eq!(r.status, 200);
    }
}

#[tokio::test]
async fn azure_reads() {
    let racer = FakeRacer::start().await;
    racer.put(
        "bucket",
        "blob",
        "v+/%",
        Object::new("0123456789", r#""v""#),
    );
    let s = Sidecar::start(&racer, Api::Azure).await;
    for (name, method, standard, azure, body, content_range, status, length) in [
        ("full", "GET", "", "", "0123456789", "", 200, 10),
        ("head", "HEAD", "", "", "", "", 200, 10),
        (
            "standard",
            "GET",
            "bytes=2-4",
            "",
            "234",
            "bytes 2-4/10",
            206,
            3,
        ),
        (
            "azure",
            "GET",
            "",
            "bytes=2-4",
            "234",
            "bytes 2-4/10",
            206,
            3,
        ),
        (
            "precedence",
            "GET",
            "bad",
            "bytes=7-",
            "789",
            "bytes 7-9/10",
            206,
            3,
        ),
        (
            "suffix",
            "GET",
            "",
            "bytes=-2",
            "89",
            "bytes 8-9/10",
            206,
            2,
        ),
        (
            "clamp",
            "GET",
            "",
            "bytes=8-99",
            "89",
            "bytes 8-9/10",
            206,
            2,
        ),
        (
            "head ignores ranges",
            "HEAD",
            "bad",
            "bytes=999-",
            "",
            "",
            200,
            10,
        ),
    ] {
        let mut headers = vec![("Authorization", "Bearer ignored-secret")];
        if !standard.is_empty() {
            headers.push(("Range", standard));
        }
        if !azure.is_empty() {
            headers.push(("x-ms-range", azure));
        }
        let before = racer.posts();
        let r = s
            .get(
                method,
                "/bucket/blob?versionid=v%2B%2F%25&timeout=30",
                &headers,
            )
            .await;
        assert_eq!((r.status, r.text().as_str()), (status, body), "{name}");
        for (n, want) in [
            ("etag", Some(r#""v""#)),
            ("content-length", Some(length.to_string().as_str())),
            ("content-type", Some("text/plain")),
            ("accept-ranges", Some("bytes")),
            (
                "content-range",
                (!content_range.is_empty()).then_some(content_range),
            ),
            ("x-ms-blob-type", Some("BlockBlob")),
            ("x-ms-version", Some("2026-02-06")),
            ("x-ms-version-id", Some("v+/%")),
            ("last-modified", None),
            ("x-amz-version-id", None),
        ] {
            assert_eq!(r.header(n), want, "{name} {n}");
        }
        if method == "HEAD" {
            assert_eq!(racer.posts(), before, "HEAD opened a stream");
        }
    }
}

#[tokio::test]
async fn azure_conditions_and_ranges() {
    let (_racer, s) = setup(Api::Azure, "data", r#""v""#).await;
    // Insert the blob under the Azure test name too.
    _racer.put("bucket", "blob", "", Object::new("data", r#""v""#));
    for (header, value, status, code) in [
        ("If-Match", r#""v""#, 200, ""),
        ("If-Match", r#""other", "v""#, 200, ""),
        ("If-Match", "*", 200, ""),
        ("If-Match", r#"W/"v""#, 412, "ConditionNotMet"),
        ("If-Match", r#""other""#, 412, "ConditionNotMet"),
        ("If-Match", "bare", 400, "InvalidInput"),
        ("If-None-Match", r#""v""#, 304, ""),
        ("If-None-Match", r#"W/"v""#, 304, ""),
        ("If-None-Match", "*", 304, ""),
        ("If-None-Match", r#""other""#, 200, ""),
        ("x-ms-range", "bytes=4-", 416, "InvalidRange"),
        ("x-ms-range", "bytes=-0", 416, "InvalidRange"),
        ("x-ms-range", "bytes=2-1", 400, "InvalidInput"),
        ("x-ms-range", "bytes=0-1,2-3", 400, "InvalidInput"),
        ("x-ms-range", "", 400, "InvalidInput"),
        (
            "x-ms-range",
            "bytes=18446744073709551616-",
            400,
            "InvalidInput",
        ),
    ] {
        for method in ["GET", "HEAD"] {
            let r = s.get(method, "/bucket/blob", &[(header, value)]).await;
            let (status, code) = if method == "HEAD" && header == "x-ms-range" {
                (200, "")
            } else {
                (status, code)
            };
            assert_eq!(r.status, status, "{method} {header} {value}");
            assert_eq!(r.header("x-ms-error-code").unwrap_or(""), code);
            assert_eq!(r.header("x-ms-version"), Some("2026-02-06"));
            if status == 304 {
                assert_eq!(r.header("etag"), Some(r#""v""#));
                assert_eq!(r.header("x-ms-blob-type"), Some("BlockBlob"));
                assert!(r.body.is_empty());
            }
            if status == 416 {
                assert_eq!(r.header("content-range"), Some("bytes */4"));
            }
        }
    }
    for headers in [
        &[("x-ms-range", "bytes=0-1"), ("x-ms-range", "bytes=2-3")][..],
        &[("x-ms-range", ""), ("Range", "bytes=0-1")],
    ] {
        assert_eq!(s.get("GET", "/bucket/blob", headers).await.status, 400);
    }
}

#[tokio::test]
async fn azure_rejected_requests() {
    let racer = FakeRacer::start().await;
    let s = Sidecar::start(&racer, Api::Azure).await;
    for (target, status) in [
        ("/", 400),
        ("/bucket", 400),
        ("/bucket/", 400),
        ("/bucket%2Fblob", 400),
        ("/bucket/%FF", 400),
        ("/bucket/a%7Fb", 400),
        ("/bucket/blob?versionid=", 400),
        ("/bucket/blob?versionid=a&versionid=b", 400),
        ("/bucket/blob?versionid=%zz", 400),
        ("/bucket/blob?versionid=a;b", 400),
        ("/bucket/blob?versionid=%00", 400),
        ("/bucket/blob?timeout=0", 400),
        ("/bucket/blob?timeout=1.5", 400),
        ("/bucket/blob?timeout=18446744073709551616", 400),
        ("/bucket/blob?snapshot=x", 501),
        ("/bucket/blob?sig=secret", 501),
        ("/bucket/blob?versionId=x", 501),
        ("/bucket/blob?unknown=", 400),
        ("/bucket/blob?comp=metadata&timeout=bad", 400),
    ] {
        for method in ["GET", "HEAD"] {
            let r = s.get(method, target, &[]).await;
            let code = if status == 400 {
                "InvalidInput"
            } else {
                "NotImplemented"
            };
            assert_xml(&r, method, status, code);
            assert_eq!(r.header("x-ms-error-code"), Some(code));
        }
    }
    for name in [
        "If-Modified-Since",
        "If-Unmodified-Since",
        "If-Range",
        "x-ms-lease-id",
        "x-ms-encryption-key",
        "x-ms-if-tags",
        "x-ms-range-get-content-md5",
        "x-ms-unknown",
    ] {
        for method in ["GET", "HEAD"] {
            let r = s.get(method, "/bucket/blob", &[(name, "")]).await;
            assert_xml(&r, method, 501, "NotImplemented");
        }
    }
    for method in ["PUT", "POST", "DELETE", "OPTIONS"] {
        let r = s.get(method, "/bucket/blob", &[]).await;
        assert_xml(&r, method, 405, "UnsupportedHttpVerb");
        assert_eq!(r.header("allow"), Some("GET, HEAD"));
        assert_eq!(r.header("x-ms-version"), Some("2026-02-06"));
    }
    assert_eq!(racer.calls(), 0);
}

#[tokio::test]
async fn azure_plumbing() {
    let racer = FakeRacer::start().await;
    racer.put("bucket", "blob", "", Object::new("data", r#""v""#));
    let s = Sidecar::start(&racer, Api::Azure).await;
    let headers = [
        ("Authorization", "ignored"),
        ("x-ms-version", "2026-02-06"),
        ("x-ms-date", "ignored"),
        (
            "x-ms-client-request-id",
            "8c5bfe95-d0ce-4d9a-b82a-2a45094c71ae",
        ),
        ("x-ms-return-client-request-id", "true"),
        ("x-ms-useragent", "ignored"),
        ("User-Agent", "azsdk-net-Storage.Blobs/12.25.1"),
    ];
    for method in ["GET", "HEAD"] {
        let r = s.get(method, "/bucket/blob?timeout=001", &headers).await;
        assert_eq!(r.status, 200);
        assert_eq!(r.header("content-length"), Some("4"));
        assert_eq!(r.header("x-ms-version-id"), None);
    }
}

#[tokio::test]
async fn default_content_type_and_versionless() {
    let racer = FakeRacer::start().await;
    let mut o = Object::new("data", r#""v""#);
    o.content_type = None;
    racer.put("bucket", "a%zz", "", o);
    let s = Sidecar::start(&racer, Api::S3).await;
    for (method, x_id) in [("GET", "GetObject"), ("HEAD", "HeadObject")] {
        let r = s
            .get(method, &format!("/bucket/a%25zz?x-id={x_id}"), &[])
            .await;
        assert_eq!(r.status, 200);
        assert_eq!(r.header("content-type"), Some("application/octet-stream"));
        assert_eq!(r.header("content-length"), Some("4"));
        assert_eq!(r.header("x-amz-version-id"), None);
    }
}

#[tokio::test]
async fn request_timeout() {
    let racer = FakeRacer::start().await;
    let mut o = Object::new("data", r#""v""#);
    o.stall_post = true;
    o.stall_head = true;
    racer.put("bucket", "key", "", o);
    let mut options = options(Api::S3);
    options.request_timeout = Duration::from_millis(300);
    let s = Sidecar::start_with(&racer, options, false).await;
    for method in ["GET", "HEAD"] {
        let started = std::time::Instant::now();
        let r = s.get(method, "/bucket/key", &[]).await;
        assert_xml(&r, method, 503, "ServiceUnavailable");
        assert!(started.elapsed() < Duration::from_secs(5));
    }
}

#[tokio::test]
async fn graceful_shutdown() {
    let racer = FakeRacer::start().await;
    racer.put("bucket", "key", "", Object::new("data", r#""v""#));
    let s = Sidecar::start_with(&racer, options(Api::S3), true).await;
    // Debug health.
    let mut d = Client::connect(s.debug.unwrap()).await;
    let r = d.request("GET", "/healthz", &[]).await;
    assert_eq!(r.status, 200);
    let r = Client::connect(s.debug.unwrap())
        .await
        .request("GET", "/debug/pprof/", &[])
        .await;
    assert_eq!(r.status, 404);
    // An idle keep-alive connection closes on shutdown and the server exits.
    let mut idle = Client::connect(s.addr).await;
    assert_eq!(idle.request("GET", "/bucket/key", &[]).await.status, 200);
    s.stop.send(true).unwrap();
    assert!(idle.closed().await);
    tokio::time::timeout(Duration::from_secs(10), s.done)
        .await
        .unwrap()
        .unwrap();
    assert!(tokio::net::TcpStream::connect(s.addr).await.is_err());
}

#[tokio::test]
async fn shutdown_finishes_in_flight() {
    let racer = FakeRacer::start().await;
    let data: Vec<u8> = vec![7u8; 8 << 20];
    racer.put("bucket", "key", "", Object::new(data.clone(), r#""v""#));
    let s = Sidecar::start(&racer, Api::S3).await;
    let mut c = Client::connect(s.addr).await;
    c.send("GET", "/bucket/key", &[]).await;
    // Wait until the request is in flight.
    while racer.posts() == 0 {
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    s.stop.send(true).unwrap();
    let r = c.response(false).await;
    assert_eq!(r.status, 200);
    assert_eq!(r.body.len(), data.len());
    tokio::time::timeout(Duration::from_secs(10), s.done)
        .await
        .unwrap()
        .unwrap();
}

#[tokio::test]
async fn connection_limit_queues_in_backlog() {
    let racer = FakeRacer::start().await;
    racer.put("bucket", "key", "", Object::new("data", r#""v""#));
    let mut options = options(Api::S3);
    options.max_connections = 1;
    let s = Sidecar::start_with(&racer, options, false).await;
    let mut first = Client::connect(s.addr).await;
    assert_eq!(first.request("GET", "/bucket/key", &[]).await.status, 200);
    // The second connection completes the TCP handshake (backlog) but is not
    // served until the first closes.
    let mut second = Client::connect(s.addr).await;
    second.send("GET", "/bucket/key", &[]).await;
    let waited = tokio::time::timeout(Duration::from_millis(300), second.try_response(false)).await;
    assert!(waited.is_err(), "second connection served past the limit");
    drop(first);
    assert_eq!(second.response(false).await.status, 200);
}

#[tokio::test]
async fn workers_share_listener_and_limits() {
    let racer = FakeRacer::start().await;
    let big: Vec<u8> = vec![9u8; 3 << 20];
    racer.put("bucket", "key", "", Object::new("data", r#""v""#));
    racer.put("bucket", "big", "", Object::new(big.clone(), r#""b""#));
    let mut options = options(Api::S3);
    options.workers = 4;
    options.max_connections = 6;
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let debug = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    debug.set_nonblocking(true).unwrap();
    let addr = listener.local_addr().unwrap();
    let debug_addr = debug.local_addr().unwrap();
    let (stop, shutdown) = tokio::sync::watch::channel(false);
    let path = racer.path.clone();
    let done = std::thread::spawn(move || {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        racer_object::run_workers(
            &runtime,
            listener,
            Some(debug),
            &options,
            path,
            shutdown,
            || Ok(()),
        )
    });
    let r = Client::connect(debug_addr)
        .await
        .request("GET", "/healthz", &[])
        .await;
    assert_eq!(r.status, 200);
    // Many concurrent keep-alive connections, served across workers.
    let mut tasks = Vec::new();
    for i in 0..16 {
        let big = big.clone();
        tasks.push(tokio::spawn(async move {
            let mut c = Client::connect(addr).await;
            for _ in 0..4 {
                let r = c.request("GET", "/bucket/key", &[]).await;
                assert_eq!((r.status, r.body.as_slice()), (200, &b"data"[..]));
                if i % 4 == 0 {
                    let r = c.request("GET", "/bucket/big", &[]).await;
                    assert_eq!(r.status, 200);
                    assert!(r.body == big);
                }
            }
        }));
    }
    for t in tasks {
        t.await.unwrap();
    }
    // The connection limit is global: with every slot held by an idle
    // connection, one more is not served.
    let mut held = Vec::new();
    for _ in 0..6 {
        let mut c = Client::connect(addr).await;
        assert_eq!(c.request("GET", "/bucket/key", &[]).await.status, 200);
        held.push(c);
    }
    let mut extra = Client::connect(addr).await;
    extra.send("GET", "/bucket/key", &[]).await;
    let waited = tokio::time::timeout(Duration::from_millis(300), extra.try_response(false)).await;
    assert!(waited.is_err(), "connection served past the global limit");
    held.pop();
    assert_eq!(extra.response(false).await.status, 200);
    // Shutdown stops every worker and closes idle connections.
    stop.send(true).unwrap();
    for mut c in held {
        assert!(c.closed().await);
    }
    assert!(extra.closed().await);
    let result = tokio::task::spawn_blocking(move || done.join().unwrap())
        .await
        .unwrap();
    assert_eq!(result, Ok(()));
    assert!(tokio::net::TcpStream::connect(addr).await.is_err());
}

#[tokio::test]
async fn handoff_to_stopped_worker_is_served_locally() {
    use racer_object::server::{Config, Peer, Source, serve};
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};
    let racer = FakeRacer::start().await;
    racer.put("bucket", "key", "", Object::new("data", r#""v""#));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    // A peer whose worker already stopped: its inbox is closed.
    let (inbox, closed) = tokio::sync::mpsc::unbounded_channel();
    drop(closed);
    let peer_active = Arc::new(AtomicUsize::new(0));
    let peers = vec![Peer {
        inbox,
        active: peer_active.clone(),
    }];
    let slots = Arc::new(tokio::sync::Semaphore::new(4));
    let (stop, shutdown) = tokio::sync::watch::channel(false);
    let done = tokio::spawn(serve(
        Source::Listener { listener, peers },
        racer_object::racer::Client::new(racer.path.clone(), 4),
        Config {
            api: Api::S3,
            request_timeout: Duration::from_secs(10),
            slots: slots.clone(),
            max_connections: 4,
        },
        Arc::new(AtomicUsize::new(0)),
        shutdown,
    ));
    // Ties rotate across workers, so some of these pick the stopped peer.
    for _ in 0..4 {
        let mut c = Client::connect(addr).await;
        assert_eq!(c.request("GET", "/bucket/key", &[]).await.status, 200);
    }
    assert_eq!(peer_active.load(Ordering::Relaxed), 0);
    stop.send(true).unwrap();
    tokio::time::timeout(Duration::from_secs(10), done)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(slots.available_permits(), 4);
}
