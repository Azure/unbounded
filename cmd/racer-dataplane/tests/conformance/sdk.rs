use super::*;
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
    let build = Command::new("go")
        .current_dir(&root)
        .args(["test", "-overlay"])
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
    for size in [0, 3, 3 * P + 13] {
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
        rig.drive(async {
            let mut connection = rig.lease(accepted);
            for (index, (first, length)) in [(0, size.min(P)), (P, size.saturating_sub(P))]
                .into_iter()
                .enumerate()
            {
                if index == 1 && length == 0 {
                    break;
                }
                let scope = scope();
                let received = rig.io.receive_head(connection, &scope).await.unwrap();
                let parsed = RequestParser::new(LIMIT)
                    .parse(&object().cache, received.value)
                    .unwrap();
                assert_eq!(parsed.origin.object.key, CacheKey([0; 32]));
                assert_eq!(
                    parsed
                        .origin
                        .metadata
                        .as_ref()
                        .unwrap()
                        .as_header()
                        .unwrap(),
                    b"opaque,  bytes\xff"
                );
                assert_eq!(
                    parsed
                        .origin
                        .authorization
                        .as_ref()
                        .unwrap()
                        .expose_for_origin()
                        .unwrap(),
                    b"fixture credential\x80"
                );
                if index == 0 {
                    assert_eq!(parsed.kind, ReadKind::Bootstrap);
                } else {
                    assert_eq!(
                        parsed.kind,
                        ReadKind::Pinned {
                            etag: StrongEtag::parse(b"\"v\"").unwrap(),
                            range: ByteRange::Closed {
                                first: P,
                                last: size - 1
                            }
                        }
                    );
                }
                drop(parsed);
                // This is a scripted HTTP peer, not a substitute Coordinator. It
                // checks SDK's full-remainder request and real Rust framing with
                // bounded chunks. Page acquisition is a separate acceptance gate.
                let mut fields = vec![
                    ("Content-Length", length.to_string()),
                    ("Content-Type", "application/octet-stream".into()),
                    ("ETag", "\"v\"".into()),
                    ("Racer-Expires-At", index.to_string()),
                ];
                if length != 0 {
                    fields.push((
                        "Content-Range",
                        format!("bytes {first}-{}/{size}", first + length - 1),
                    ));
                }
                let head = MessageHead {
                    start: StartLine::Response {
                        status: if length == 0 { 200 } else { 206 },
                    },
                    headers: fields
                        .into_iter()
                        .map(|(name, value)| Header {
                            name: name.into(),
                            value: value.into_bytes(),
                        })
                        .collect(),
                };
                connection = rig
                    .io
                    .send_head(received.connection, head, &scope)
                    .await
                    .unwrap()
                    .connection;
                let mut offset = first;
                while offset < first + length {
                    let n = (first + length - offset).min(32768) as usize;
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
                connection.finish_exchange().unwrap();
            }
        });
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
        drop(listener);
        fs::remove_file(output.join("socket")).unwrap();
    }
    // ServeOrigin intentionally rejects proc-fd symlink ancestors. Use a short,
    // real project-local directory for its private path seam instead.
    let origin_directory = root
        .join("tmp")
        .join(format!("racer-conformance-{}", std::process::id()));
    fs::create_dir(&origin_directory).unwrap();
    let _origin_cleanup = Cleanup(origin_directory.clone());
    sdk_origin(&binary, &origin_directory.join("socket"));
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
