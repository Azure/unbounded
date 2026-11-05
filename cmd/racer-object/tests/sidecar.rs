// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! End-to-end tests over TCP that need no Racer: requests rejected before
//! Racer is contacted, connection handling, worker plumbing, and shutdown.
//! The Racer socket path usually never exists, so anything that reaches
//! Racer is a 503; a few tests bind a stalling listener there instead. Reads through a real Racer are covered by the e2e/racer suite.

use racer_object::cli::Options;
use racer_object::object::Api;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::watch;

/// A per-test directory under the cargo target tmpdir.
struct TempDir(PathBuf);

impl TempDir {
    fn new() -> Self {
        static NEXT: AtomicUsize = AtomicUsize::new(0);
        let dir = PathBuf::from(env!("CARGO_TARGET_TMPDIR")).join(format!(
            "ro-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir_all(&dir).unwrap();
        Self(dir)
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// A Racer socket path where nothing listens.
fn missing_socket() -> PathBuf {
    PathBuf::from(env!("CARGO_TARGET_TMPDIR")).join("racer-object-no-such-socket")
}

fn options(api: Api) -> Options {
    Options {
        cache: "test".into(),
        api,
        listen: "127.0.0.1:0".parse().unwrap(),
        debug_listen: None,
        request_timeout: Duration::from_secs(10),
        max_connections: 16,
        workers: 1,
    }
}

/// A running sidecar.
struct Sidecar {
    addr: SocketAddr,
    debug: Option<SocketAddr>,
    stop: watch::Sender<bool>,
    done: tokio::task::JoinHandle<()>,
}

impl Sidecar {
    async fn start(api: Api) -> Self {
        Self::start_with(options(api), missing_socket(), false).await
    }

    async fn start_with(options: Options, path: PathBuf, debug: bool) -> Self {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let debug_listener = if debug {
            Some(tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap())
        } else {
            None
        };
        let debug = debug_listener.as_ref().map(|l| l.local_addr().unwrap());
        let (stop, shutdown) = watch::channel(false);
        let done = tokio::spawn(async move {
            racer_object::run_with(listener, debug_listener, &options, path, shutdown).await;
        });
        Self {
            addr,
            debug,
            stop,
            done,
        }
    }

    async fn get(&self, method: &str, target: &str, headers: &[(&str, &str)]) -> Response {
        Client::connect(self.addr)
            .await
            .request(method, target, headers)
            .await
    }
}

/// A parsed response. Header names are lowercased.
#[derive(Debug, Default)]
struct Response {
    status: u16,
    headers: Vec<(String, String)>,
    body: Vec<u8>,
    /// The body ended early (connection closed or reset).
    truncated: bool,
}

impl Response {
    fn header(&self, name: &str) -> Option<&str> {
        let name = name.to_ascii_lowercase();
        self.headers
            .iter()
            .find(|(n, _)| *n == name)
            .map(|(_, v)| v.as_str())
    }

    fn text(&self) -> String {
        String::from_utf8_lossy(&self.body).to_string()
    }
}

struct Client {
    stream: TcpStream,
    buf: Vec<u8>,
}

impl Client {
    async fn connect(addr: SocketAddr) -> Self {
        Self {
            stream: TcpStream::connect(addr).await.unwrap(),
            buf: Vec::new(),
        }
    }

    async fn send(&mut self, method: &str, target: &str, headers: &[(&str, &str)]) {
        let mut req = format!("{method} {target} HTTP/1.1\r\nHost: localhost\r\n");
        for (n, v) in headers {
            req.push_str(&format!("{n}: {v}\r\n"));
        }
        req.push_str("\r\n");
        self.stream.write_all(req.as_bytes()).await.unwrap();
    }

    async fn request(&mut self, method: &str, target: &str, headers: &[(&str, &str)]) -> Response {
        self.send(method, target, headers).await;
        self.response(method == "HEAD").await
    }

    /// Read one response; `None` if the connection closed first.
    async fn try_response(&mut self, head: bool) -> Option<Response> {
        let len = loop {
            if let Some(i) = self.buf.windows(4).position(|w| w == b"\r\n\r\n") {
                break i + 4;
            }
            let mut chunk = vec![0u8; 65536];
            let n = tokio::time::timeout(Duration::from_secs(30), self.stream.read(&mut chunk))
                .await
                .expect("response timed out")
                .unwrap_or(0);
            if n == 0 {
                return None;
            }
            self.buf.extend_from_slice(&chunk[..n]);
        };
        let head_bytes: Vec<u8> = self.buf.drain(..len).collect();
        let text = String::from_utf8(head_bytes).unwrap();
        let mut lines = text.split("\r\n");
        let status_line = lines.next().unwrap();
        let status = status_line[9..12].parse().unwrap();
        let headers: Vec<(String, String)> = lines
            .filter(|l| !l.is_empty())
            .map(|l| {
                let (n, v) = l.split_once(": ").unwrap();
                (n.to_ascii_lowercase(), v.to_string())
            })
            .collect();
        let mut response = Response {
            status,
            headers,
            ..Response::default()
        };
        let length: usize = if head || status == 304 {
            0
        } else {
            response
                .header("content-length")
                .map_or(0, |v| v.parse().unwrap())
        };
        while self.buf.len() < length {
            let mut chunk = vec![0u8; 1 << 20];
            // A reset counts as a closed connection.
            let n = tokio::time::timeout(Duration::from_secs(30), self.stream.read(&mut chunk))
                .await
                .expect("body timed out")
                .unwrap_or_default();
            if n == 0 {
                response.truncated = true;
                break;
            }
            self.buf.extend_from_slice(&chunk[..n]);
        }
        let take = length.min(self.buf.len());
        response.body = self.buf.drain(..take).collect();
        Some(response)
    }

    async fn response(&mut self, head: bool) -> Response {
        self.try_response(head).await.expect("connection closed")
    }

    /// Whether the server closed the connection (EOF or reset).
    async fn closed(&mut self) -> bool {
        let mut b = [0u8; 1];
        matches!(
            tokio::time::timeout(Duration::from_secs(5), self.stream.read(&mut b)).await,
            Ok(Ok(0)) | Ok(Err(_))
        )
    }
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
    let s = Sidecar::start(Api::S3).await;
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
}

#[tokio::test]
async fn azure_rejected_requests() {
    let s = Sidecar::start(Api::Azure).await;
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
}

#[tokio::test]
async fn racer_unavailable() {
    for api in [Api::S3, Api::Azure] {
        let s = Sidecar::start(api).await;
        let code = if api == Api::S3 {
            "ServiceUnavailable"
        } else {
            "ServerBusy"
        };
        for method in ["GET", "HEAD"] {
            for headers in [
                &[][..],
                &[("If-None-Match", r#""v""#)],
                &[("Range", "bytes=0-1")],
                // Range syntax errors still consult Racer first.
                &[("Range", "bytes=2-1")],
            ] {
                let r = s.get(method, "/bucket/key", headers).await;
                assert_xml(&r, method, 503, code);
            }
        }
    }
}

#[tokio::test]
async fn request_timeout() {
    // Racer accepts the connection but never answers.
    let dir = TempDir::new();
    let path = dir.0.join("socket");
    let _listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
    let mut options = options(Api::S3);
    options.request_timeout = Duration::from_millis(300);
    let s = Sidecar::start_with(options, path, false).await;
    for method in ["GET", "HEAD"] {
        let started = std::time::Instant::now();
        let r = s.get(method, "/bucket/key", &[]).await;
        assert_xml(&r, method, 503, "ServiceUnavailable");
        assert!(started.elapsed() < Duration::from_secs(5));
    }
}

#[tokio::test]
async fn client_reset_releases_slot() {
    // Racer never answers; a client that resets must free its slot.
    let dir = TempDir::new();
    let path = dir.0.join("socket");
    let _listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
    let mut options = options(Api::S3);
    options.request_timeout = Duration::from_secs(30);
    options.max_connections = 1;
    let s = Sidecar::start_with(options, path, false).await;
    let mut c = Client::connect(s.addr).await;
    c.send("GET", "/bucket/key", &[]).await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    c.stream.set_zero_linger().unwrap();
    drop(c);
    let started = std::time::Instant::now();
    let r = tokio::time::timeout(Duration::from_secs(5), s.get("PUT", "/bucket/key", &[]))
        .await
        .expect("slot was not released");
    assert_eq!(r.status, 405);
    assert!(started.elapsed() < Duration::from_secs(5));
}

#[tokio::test]
async fn client_reset_mid_body_releases_slot() {
    // Racer commits a 1 MiB body, sends one partial page, and stalls. A
    // client that resets mid-body must free its slot without waiting out
    // the Racer body timeout or the request deadline.
    let dir = TempDir::new();
    let path = dir.0.join("socket");
    let listener = tokio::net::UnixListener::bind(&path).unwrap();
    let racer = tokio::spawn(async move {
        let (mut conn, _) = listener.accept().await.unwrap();
        let mut req = Vec::new();
        while !req.windows(4).any(|w| w == b"\r\n\r\n") {
            let mut chunk = [0u8; 4096];
            let n = conn.read(&mut chunk).await.unwrap();
            assert!(n > 0);
            req.extend_from_slice(&chunk[..n]);
        }
        const SIZE: u64 = 1 << 20;
        let mut out = format!(
            "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\n\
             Content-Type: application/octet-stream\r\nRacer-Object-Length: {SIZE}\r\n\
             Racer-Range-Start: 0\r\nRacer-Range-End: {SIZE}\r\n\
             Content-Length: {}\r\nConnection: close\r\n\r\n",
            SIZE + 2 * 21
        )
        .into_bytes();
        // Page frame: kind 1, page 0, offset 0, length SIZE (big-endian).
        out.push(1);
        out.extend_from_slice(&0u64.to_be_bytes());
        out.extend_from_slice(&0u64.to_be_bytes());
        out.extend_from_slice(&(SIZE as u32).to_be_bytes());
        out.extend_from_slice(&[b'x'; 1000]);
        conn.write_all(&out).await.unwrap();
        // Stall; the sidecar dropping the subscription closes this socket.
        let mut rest = [0u8; 64];
        while conn.read(&mut rest).await.unwrap_or(0) > 0 {}
    });
    let mut options = options(Api::S3);
    options.request_timeout = Duration::from_secs(30);
    options.max_connections = 1;
    let s = Sidecar::start_with(options, path, false).await;
    let mut c = Client::connect(s.addr).await;
    c.send("GET", "/bucket/key", &[]).await;
    // Wait for the head and some payload so the body phase is underway.
    let mut got = Vec::new();
    while !got.windows(4).any(|w| w == b"\r\n\r\n") || got.ends_with(b"\r\n\r\n") {
        let mut chunk = [0u8; 4096];
        let n = tokio::time::timeout(Duration::from_secs(5), c.stream.read(&mut chunk))
            .await
            .expect("no response head")
            .unwrap();
        assert!(n > 0);
        got.extend_from_slice(&chunk[..n]);
    }
    assert!(
        got.starts_with(b"HTTP/1.1 200"),
        "{}",
        String::from_utf8_lossy(&got)
    );
    c.stream.set_zero_linger().unwrap();
    drop(c);
    let started = std::time::Instant::now();
    let r = tokio::time::timeout(Duration::from_secs(5), s.get("PUT", "/bucket/key", &[]))
        .await
        .expect("slot was not released");
    assert_eq!(r.status, 405);
    assert!(started.elapsed() < Duration::from_secs(5));
    // The Racer subscription was dropped too.
    tokio::time::timeout(Duration::from_secs(5), racer)
        .await
        .expect("Racer subscription not dropped")
        .unwrap();
}

#[tokio::test]
async fn client_reset_before_first_frame_releases_slot() {
    // Racer answers a 1 MiB subscription head and stalls before any frame.
    // The sidecar has not committed a response yet; a client reset must
    // still free its slot and drop the Racer subscription.
    let dir = TempDir::new();
    let path = dir.0.join("socket");
    let listener = tokio::net::UnixListener::bind(&path).unwrap();
    let (headed_tx, headed_rx) = tokio::sync::oneshot::channel();
    let racer = tokio::spawn(async move {
        let (mut conn, _) = listener.accept().await.unwrap();
        let mut req = Vec::new();
        while !req.windows(4).any(|w| w == b"\r\n\r\n") {
            let mut chunk = [0u8; 4096];
            let n = conn.read(&mut chunk).await.unwrap();
            assert!(n > 0);
            req.extend_from_slice(&chunk[..n]);
        }
        const SIZE: u64 = 1 << 20;
        let out = format!(
            "HTTP/1.1 200 OK\r\nETag: \"v\"\r\nRacer-Expires-At: 1\r\n\
             Content-Type: application/octet-stream\r\nRacer-Object-Length: {SIZE}\r\n\
             Racer-Range-Start: 0\r\nRacer-Range-End: {SIZE}\r\n\
             Content-Length: {}\r\nConnection: close\r\n\r\n",
            SIZE + 2 * 21
        );
        conn.write_all(out.as_bytes()).await.unwrap();
        let _ = headed_tx.send(());
        // Stall; the sidecar dropping the subscription closes this socket.
        let mut rest = [0u8; 64];
        while conn.read(&mut rest).await.unwrap_or(0) > 0 {}
    });
    let mut options = options(Api::S3);
    options.request_timeout = Duration::from_secs(30);
    options.max_connections = 1;
    let s = Sidecar::start_with(options, path, false).await;
    let mut c = Client::connect(s.addr).await;
    c.send("GET", "/bucket/key", &[]).await;
    tokio::time::timeout(Duration::from_secs(5), headed_rx)
        .await
        .expect("Racer never answered")
        .unwrap();
    // Let the sidecar parse the head and block on the first frame.
    tokio::time::sleep(Duration::from_millis(100)).await;
    c.stream.set_zero_linger().unwrap();
    drop(c);
    let r = tokio::time::timeout(Duration::from_secs(5), s.get("PUT", "/bucket/key", &[]))
        .await
        .expect("slot was not released");
    assert_eq!(r.status, 405);
    tokio::time::timeout(Duration::from_secs(5), racer)
        .await
        .expect("Racer subscription not dropped")
        .unwrap();
}

#[tokio::test]
async fn half_close_still_answered() {
    let dir = TempDir::new();
    let path = dir.0.join("socket");
    let _listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
    let mut options = options(Api::S3);
    options.request_timeout = Duration::from_millis(300);
    let s = Sidecar::start_with(options, path, false).await;
    for method in ["GET", "HEAD"] {
        let mut c = Client::connect(s.addr).await;
        c.send(method, "/bucket/key", &[]).await;
        c.stream.shutdown().await.unwrap();
        let r = c.response(method == "HEAD").await;
        assert_xml(&r, method, 503, "ServiceUnavailable");
        assert!(c.closed().await);
    }
}

#[tokio::test]
async fn keep_alive_and_pipelining() {
    let s = Sidecar::start(Api::S3).await;
    let mut c = Client::connect(s.addr).await;
    for _ in 0..3 {
        assert_eq!(c.request("GET", "/bucket/key", &[]).await.status, 503);
        assert_eq!(c.request("PUT", "/bucket/key", &[]).await.status, 405);
    }
    // Pipelined: several requests in one write, responses in order.
    let batch = "GET /bucket/key?partNumber=1 HTTP/1.1\r\nHost: x\r\n\r\n\
                 PUT /bucket/key HTTP/1.1\r\nHost: x\r\n\r\n\
                 GET /bucket HTTP/1.1\r\nHost: x\r\n\r\n\
                 HEAD /bucket/key HTTP/1.1\r\nHost: x\r\n\r\n";
    c.stream.write_all(batch.as_bytes()).await.unwrap();
    for want in [501, 405, 400] {
        assert_eq!(c.response(false).await.status, want);
    }
    let r = c.response(true).await;
    assert_eq!(r.status, 503);
    assert!(r.body.is_empty());
}

#[tokio::test]
async fn connection_semantics() {
    let s = Sidecar::start(Api::S3).await;
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
        .request("PUT", "/bucket/key", &[("Content-Length", "4")])
        .await;
    assert_eq!(r.status, 405);
    assert_eq!(r.header("connection"), Some("close"));
    assert!(c.closed().await);
    // HTTP/1.0: close by default, keep-alive when asked.
    let mut c = Client::connect(s.addr).await;
    c.stream
        .write_all(b"GET /bucket/key HTTP/1.0\r\n\r\n")
        .await
        .unwrap();
    let r = c.response(false).await;
    assert_eq!((r.status, r.header("connection")), (503, Some("close")));
    assert!(c.closed().await);
    let mut c = Client::connect(s.addr).await;
    for _ in 0..2 {
        c.stream
            .write_all(b"GET /bucket/key HTTP/1.0\r\nConnection: keep-alive\r\n\r\n")
            .await
            .unwrap();
        let r = c.response(false).await;
        assert_eq!(
            (r.status, r.header("connection")),
            (503, Some("keep-alive"))
        );
    }
    // HTTP/1.1 without Host is a 400.
    let mut c = Client::connect(s.addr).await;
    c.stream
        .write_all(b"GET /bucket/key HTTP/1.1\r\n\r\n")
        .await
        .unwrap();
    assert_eq!(c.response(false).await.status, 400);
    assert!(c.closed().await);
    // Garbage is a 400.
    let mut c = Client::connect(s.addr).await;
    c.stream
        .write_all(b"\x01\x02 nonsense\r\n\r\n")
        .await
        .unwrap();
    assert_eq!(c.response(false).await.status, 400);
    // Invalid escapes in the target are rejected before method checks.
    assert_eq!(s.get("PUT", "/bucket/%zz", &[]).await.status, 400);
    // Absolute-form targets reach Racer.
    let r = s.get("GET", "http://localhost/bucket/key", &[]).await;
    assert_eq!(r.status, 503);
    let r = s.get("GET", "http://localhost/bucket", &[]).await;
    assert_eq!(r.status, 400);
    // OPTIONS * gets an empty 200.
    let r = s.get("OPTIONS", "*", &[]).await;
    assert_eq!((r.status, r.header("content-length")), (200, Some("0")));
}

#[tokio::test]
async fn header_too_large() {
    for api in [Api::S3, Api::Azure] {
        let s = Sidecar::start(api).await;
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
        // Just under the limit is accepted and reaches Racer.
        let ok = "x".repeat(32 << 10);
        let r = s.get("GET", "/bucket/key", &[("X-Ok", &ok)]).await;
        assert_eq!(r.status, 503);
    }
}

#[tokio::test]
async fn graceful_shutdown() {
    let s = Sidecar::start_with(options(Api::S3), missing_socket(), true).await;
    // Debug health.
    let r = Client::connect(s.debug.unwrap())
        .await
        .request("GET", "/healthz", &[])
        .await;
    assert_eq!(r.status, 200);
    let r = Client::connect(s.debug.unwrap())
        .await
        .request("GET", "/debug/pprof/", &[])
        .await;
    assert_eq!(r.status, 404);
    // An idle keep-alive connection closes on shutdown and the server exits.
    let mut idle = Client::connect(s.addr).await;
    assert_eq!(idle.request("PUT", "/bucket/key", &[]).await.status, 405);
    s.stop.send(true).unwrap();
    assert!(idle.closed().await);
    tokio::time::timeout(Duration::from_secs(10), s.done)
        .await
        .unwrap()
        .unwrap();
    assert!(TcpStream::connect(s.addr).await.is_err());
}

#[tokio::test]
async fn connection_limit_queues_in_backlog() {
    let mut options = options(Api::S3);
    options.max_connections = 1;
    let s = Sidecar::start_with(options, missing_socket(), false).await;
    let mut first = Client::connect(s.addr).await;
    assert_eq!(first.request("PUT", "/bucket/key", &[]).await.status, 405);
    // The second connection completes the TCP handshake (backlog) but is not
    // served until the first closes.
    let mut second = Client::connect(s.addr).await;
    second.send("PUT", "/bucket/key", &[]).await;
    let waited = tokio::time::timeout(Duration::from_millis(300), second.try_response(false)).await;
    assert!(waited.is_err(), "second connection served past the limit");
    drop(first);
    assert_eq!(second.response(false).await.status, 405);
}

#[tokio::test]
async fn stalled_reader_releases_slot() {
    // A client pipelines many requests and then a malformed one, and never
    // reads. Once the send buffers fill, the response writes (including the
    // final 400 sent outside any request deadline) must time out and close
    // the connection so the only slot is released.
    let mut options = options(Api::S3);
    options.max_connections = 1;
    options.request_timeout = Duration::from_millis(300);
    let s = Sidecar::start_with(options, missing_socket(), false).await;
    let socket = tokio::net::TcpSocket::new_v4().unwrap();
    socket.set_recv_buffer_size(4096).unwrap();
    let stalled = socket.connect(s.addr).await.unwrap();
    let (_read, mut write) = stalled.into_split();
    let writer = tokio::spawn(async move {
        let one = "PUT /bucket/key HTTP/1.1\r\nHost: x\r\n\r\n".repeat(1000);
        for _ in 0..50 {
            if write.write_all(one.as_bytes()).await.is_err() {
                return;
            }
        }
        let _ = write.write_all(b"BOGUS\r\n\r\n").await;
        // Keep the write half open.
        std::future::pending::<()>().await;
    });
    let mut next = Client::connect(s.addr).await;
    next.send("PUT", "/bucket/key", &[]).await;
    let served = tokio::time::timeout(Duration::from_secs(10), next.response(false))
        .await
        .expect("slot not released by stalled reader");
    assert_eq!(served.status, 405);
    writer.abort();
}

#[tokio::test]
async fn workers_share_listener_and_limits() {
    let mut options = options(Api::S3);
    options.workers = 4;
    options.max_connections = 6;
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let debug = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    debug.set_nonblocking(true).unwrap();
    let addr = listener.local_addr().unwrap();
    let debug_addr = debug.local_addr().unwrap();
    let (stop, shutdown) = watch::channel(false);
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
            missing_socket(),
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
    for _ in 0..16 {
        tasks.push(tokio::spawn(async move {
            let mut c = Client::connect(addr).await;
            for _ in 0..4 {
                assert_eq!(c.request("GET", "/bucket/key", &[]).await.status, 503);
                assert_eq!(c.request("PUT", "/bucket/key", &[]).await.status, 405);
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
        assert_eq!(c.request("PUT", "/bucket/key", &[]).await.status, 405);
        held.push(c);
    }
    let mut extra = Client::connect(addr).await;
    extra.send("PUT", "/bucket/key", &[]).await;
    let waited = tokio::time::timeout(Duration::from_millis(300), extra.try_response(false)).await;
    assert!(waited.is_err(), "connection served past the global limit");
    held.pop();
    assert_eq!(extra.response(false).await.status, 405);
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
    assert!(TcpStream::connect(addr).await.is_err());
}

#[tokio::test]
async fn handoff_to_stopped_worker_is_served_locally() {
    use racer_object::server::{Config, Peer, Source, serve};
    use std::sync::Arc;
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
    let (stop, shutdown) = watch::channel(false);
    let done = tokio::spawn(serve(
        Source::Listener { listener, peers },
        racer_object::racer::Client::new(missing_socket(), 4),
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
        assert_eq!(c.request("PUT", "/bucket/key", &[]).await.status, 405);
    }
    assert_eq!(peer_active.load(Ordering::Relaxed), 0);
    stop.send(true).unwrap();
    tokio::time::timeout(Duration::from_secs(10), done)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(slots.available_permits(), 4);
}
