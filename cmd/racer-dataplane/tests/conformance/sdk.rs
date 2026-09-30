use super::*;
use racer_dataplane::model::ResourceClass;
use std::{
    fs,
    os::{fd::AsRawFd, unix::net::UnixListener},
    path::PathBuf,
    process::{Child, Command, Stdio},
};

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
    let root = PathBuf::from(std::env::var_os("RACER_SDK_ROOT").expect("RACER_SDK_ROOT required"));
    assert!(root.join("pkg/racersdk/client.go").is_file());
    let fixture =
        PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/conformance/sdk_fixture_test.go.txt");
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
        let mut connection = rig.lease(accepted);
        for pinned in [false] {
            let scope = scope();
            let received = rig.io.receive_head(connection, &scope).await.unwrap();
            let request = RequestParser::new(LIMIT)
                .parse(&object().cache, received.value)
                .unwrap();
            assert_eq!(
                request.kind,
                if pinned {
                    ReadKind::HeadPinned {
                        etag: StrongEtag::parse(b"\"v\"").unwrap(),
                    }
                } else {
                    ReadKind::Head
                }
            );
            connection = rig
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
        }
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
    connection: racer_dataplane::http::pool::ConnectionLease,
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
        let (head, used) = Codec::new(LIMIT, MAX)
            .decode_head(&response)
            .unwrap()
            .unwrap();
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
            assert_eq!(metadata::validate(&head, &object()).unwrap().length, 3);
        } else if !fields.contains("If-Match") {
            assert_eq!(
                metadata::validate_bootstrap(&head, &object()).unwrap().1,
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
            assert_eq!(page::validate(&head, &page, 3).unwrap().length, 3);
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
