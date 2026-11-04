// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Test support: an in-process fake Racer client socket and a tiny HTTP/1.1
//! client that exposes raw responses.

#![allow(dead_code)]

use racer_object::cli::Options;
use racer_object::object::{Api, Identity, identity};
use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpStream, UnixListener, UnixStream};
use tokio::sync::watch;

pub const PAGE: u64 = 16 * 1024 * 1024;

/// One stored object version plus fault injection for its reads.
#[derive(Clone, Default)]
pub struct Object {
    pub data: Arc<Vec<u8>>,
    pub etag: String,
    pub content_type: Option<String>,
    /// Error status for HEAD (Stat).
    pub head_status: Option<u16>,
    /// Error status for the subscription.
    pub post_status: Option<u16>,
    /// Close the subscription after this many body bytes (frames included).
    pub truncate_at: Option<usize>,
    /// Send a page frame with an invalid kind.
    pub corrupt_frame: bool,
    /// Report a different size or content type on the subscription.
    pub post_size_delta: u64,
    pub post_content_type: Option<String>,
    /// Never answer the subscription.
    pub stall_post: bool,
    /// Never answer HEAD.
    pub stall_head: bool,
}

impl Object {
    pub fn new(data: impl Into<Vec<u8>>, etag: &str) -> Self {
        Self {
            data: Arc::new(data.into()),
            etag: etag.into(),
            content_type: Some("text/plain".into()),
            ..Self::default()
        }
    }
}

/// What the fake saw.
#[derive(Clone, Debug, Default)]
pub struct Seen {
    pub method: String,
    pub key: String,
    pub metadata: String,
    pub range: Option<String>,
    pub if_match: Option<String>,
}

#[derive(Default)]
pub struct State {
    pub objects: Mutex<HashMap<String, Object>>,
    pub heads: AtomicUsize,
    pub posts: AtomicUsize,
    pub connections: AtomicUsize,
    pub seen: Mutex<Vec<Seen>>,
    pub credits: AtomicUsize,
}

pub struct FakeRacer {
    pub path: PathBuf,
    pub state: Arc<State>,
    _dir: TempDir,
}

/// A directory removed on drop, under cargo's per-target temp directory.
pub struct TempDir(pub PathBuf);

impl TempDir {
    pub fn new() -> Self {
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

pub fn id(bucket: &str, key: &str, version: &str) -> Identity {
    identity(bucket.as_bytes(), key.as_bytes(), version.as_bytes()).unwrap()
}

impl FakeRacer {
    pub async fn start() -> Self {
        let dir = TempDir::new();
        let path = dir.0.join("s");
        let listener = UnixListener::bind(&path).unwrap();
        let state = Arc::new(State::default());
        let s = state.clone();
        tokio::spawn(async move {
            while let Ok((stream, _)) = listener.accept().await {
                s.connections.fetch_add(1, Ordering::Relaxed);
                tokio::spawn(serve(stream, s.clone()));
            }
        });
        Self {
            path,
            state,
            _dir: dir,
        }
    }

    pub fn put(&self, bucket: &str, key: &str, version: &str, object: Object) {
        let id = id(bucket, key, version);
        self.state
            .objects
            .lock()
            .unwrap()
            .insert(id.key_str().to_string(), object);
    }

    pub fn heads(&self) -> usize {
        self.state.heads.load(Ordering::Relaxed)
    }

    pub fn posts(&self) -> usize {
        self.state.posts.load(Ordering::Relaxed)
    }

    pub fn calls(&self) -> usize {
        self.heads() + self.posts()
    }

    pub fn seen(&self) -> Vec<Seen> {
        self.state.seen.lock().unwrap().clone()
    }
}

async fn read_head(stream: &mut UnixStream, buf: &mut Vec<u8>) -> Option<usize> {
    loop {
        if let Some(i) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
            return Some(i + 4);
        }
        let mut chunk = [0u8; 4096];
        let n = stream.read(&mut chunk).await.ok()?;
        if n == 0 {
            return None;
        }
        buf.extend_from_slice(&chunk[..n]);
    }
}

fn frame(kind: u8, number: u64, offset: u64, length: u32) -> [u8; 21] {
    let mut b = [0u8; 21];
    b[0] = kind;
    b[1..9].copy_from_slice(&number.to_be_bytes());
    b[9..17].copy_from_slice(&offset.to_be_bytes());
    b[17..].copy_from_slice(&length.to_be_bytes());
    b
}

/// Racer's range resolution (object-wire `ByteRange::resolve`).
fn resolve(range: Option<&str>, size: u64) -> Result<(u64, u64), u16> {
    let Some(range) = range else {
        return Ok((0, size));
    };
    let spec = range.strip_prefix("bytes=").ok_or(400u16)?;
    let (a, b) = spec.split_once('-').ok_or(400u16)?;
    let num = |s: &str| s.parse::<u64>().map_err(|_| 400u16);
    if size == 0 {
        return Err(416);
    }
    let (start, end) = match (a.is_empty(), b.is_empty()) {
        (true, false) => {
            let n = num(b)?;
            if n == 0 {
                return Err(416);
            }
            (size.saturating_sub(n), size)
        }
        (false, true) => (num(a)?, size),
        (false, false) => {
            let (first, last) = (num(a)?, num(b)?);
            if first > last {
                return Err(400);
            }
            (first, (last + 1).min(size))
        }
        (true, true) => return Err(400),
    };
    if start >= size {
        return Err(416);
    }
    Ok((start, end))
}

async fn serve(mut stream: UnixStream, state: Arc<State>) {
    let mut buf = Vec::new();
    loop {
        let Some(len) = read_head(&mut stream, &mut buf).await else {
            return;
        };
        let head: Vec<u8> = buf.drain(..len).collect();
        let text = String::from_utf8_lossy(&head).to_string();
        let mut lines = text.split("\r\n");
        let request_line = lines.next().unwrap_or_default().to_string();
        let mut parts = request_line.split(' ');
        let method = parts.next().unwrap_or_default().to_string();
        let path = parts.next().unwrap_or_default().to_string();
        let mut headers = HashMap::new();
        for line in lines.filter(|l| !l.is_empty()) {
            if let Some((n, v)) = line.split_once(": ") {
                headers.insert(n.to_ascii_lowercase(), v.to_string());
            }
        }
        let key = path
            .strip_prefix("/v1/objects/")
            .unwrap_or_default()
            .to_string();
        state.seen.lock().unwrap().push(Seen {
            method: method.clone(),
            key: key.clone(),
            metadata: headers.get("racer-metadata").cloned().unwrap_or_default(),
            range: headers.get("range").cloned(),
            if_match: headers.get("if-match").cloned(),
        });
        let object = state.objects.lock().unwrap().get(&key).cloned();
        if method == "HEAD" {
            state.heads.fetch_add(1, Ordering::Relaxed);
            let response = match &object {
                None => "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n".to_string(),
                Some(o) if o.stall_head => {
                    tokio::time::sleep(Duration::from_secs(3600)).await;
                    return;
                }
                Some(o) if o.head_status.is_some() => format!(
                    "HTTP/1.1 {} X\r\nContent-Length: 0\r\n{}\r\n",
                    o.head_status.unwrap(),
                    if o.head_status == Some(405) {
                        "Allow: HEAD, POST\r\n"
                    } else {
                        ""
                    }
                ),
                Some(o) => format!(
                    "HTTP/1.1 200 OK\r\nETag: {}\r\nRacer-Expires-At: 1700000000000\r\n{}Content-Length: {}\r\nContent-Type: application/octet-stream\r\n\r\n",
                    o.etag,
                    o.content_type
                        .as_ref()
                        .map(|t| format!("Racer-Content-Type: {t}\r\n"))
                        .unwrap_or_default(),
                    o.data.len()
                ),
            };
            if stream.write_all(response.as_bytes()).await.is_err() {
                return;
            }
            continue;
        }
        state.posts.fetch_add(1, Ordering::Relaxed);
        let Some(o) = object else {
            let _ = stream
                .write_all(
                    b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                )
                .await;
            return;
        };
        if o.stall_post {
            tokio::time::sleep(Duration::from_secs(3600)).await;
            return;
        }
        let size = o.data.len() as u64 + o.post_size_delta;
        let error = |status: u16| {
            let extra = if status == 416 {
                format!("Content-Range: bytes */{size}\r\n")
            } else {
                String::new()
            };
            format!("HTTP/1.1 {status} X\r\nContent-Length: 0\r\n{extra}Connection: close\r\n\r\n")
        };
        if let Some(status) = o.post_status {
            let _ = stream.write_all(error(status).as_bytes()).await;
            return;
        }
        if headers.get("if-match").is_some_and(|p| *p != o.etag) {
            let _ = stream.write_all(error(412).as_bytes()).await;
            return;
        }
        let (first, end) = match resolve(headers.get("range").map(String::as_str), size) {
            Ok(r) => r,
            Err(status) => {
                let _ = stream.write_all(error(status).as_bytes()).await;
                return;
            }
        };
        let pages = if end <= first {
            0
        } else {
            (end - 1) / PAGE - first / PAGE + 1
        };
        let content_type = o.post_content_type.clone().or(o.content_type.clone());
        let head = format!(
            "HTTP/1.1 200 OK\r\nETag: {}\r\nRacer-Expires-At: 1700000000000\r\n{}Content-Type: application/octet-stream\r\nRacer-Object-Length: {size}\r\nRacer-Range-Start: {first}\r\nRacer-Range-End: {end}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            o.etag,
            content_type
                .map(|t| format!("Racer-Content-Type: {t}\r\n"))
                .unwrap_or_default(),
            (end - first) + 21 * (pages + 1)
        );
        // Build the body lazily per page and honor credits.
        let (mut reader, mut writer) = stream.into_split();
        let credits = Arc::new(tokio::sync::Semaphore::new(2));
        let c = credits.clone();
        let st = state.clone();
        tokio::spawn(async move {
            let mut b = [0u8; 12];
            while reader.read_exact(&mut b).await.is_ok() {
                st.credits.fetch_add(1, Ordering::Relaxed);
                c.add_permits(1);
            }
        });
        if writer.write_all(head.as_bytes()).await.is_err() {
            return;
        }
        let mut sent = 0usize;
        let limit = o.truncate_at.unwrap_or(usize::MAX);
        let mut write = async |bytes: &[u8]| -> bool {
            let room = limit.saturating_sub(sent);
            let n = bytes.len().min(room);
            if writer.write_all(&bytes[..n]).await.is_err() {
                return false;
            }
            sent += n;
            n == bytes.len()
        };
        let mut offset = first;
        let mut number = first / PAGE;
        while offset < end {
            let page_end = end.min((number + 1) * PAGE);
            let len = (page_end - offset) as u32;
            let Ok(permit) = credits.acquire().await else {
                return;
            };
            permit.forget();
            let kind = if o.corrupt_frame { 9 } else { 1 };
            if !write(&frame(kind, number, offset, len)).await {
                return;
            }
            let data = o.data.as_slice();
            let lo = offset as usize;
            let hi = (page_end as usize).min(data.len());
            if lo < hi && !write(&data[lo..hi]).await {
                return;
            }
            if hi < page_end as usize {
                // Size delta: pad with zeros.
                let pad = vec![0u8; page_end as usize - hi.max(lo)];
                if !write(&pad).await {
                    return;
                }
            }
            offset = page_end;
            number += 1;
        }
        let _ = write(&frame(2, pages, end - first, 0)).await;
        // Let the client read completion before closing.
        tokio::time::sleep(Duration::from_millis(50)).await;
        return;
    }
}

/// A running sidecar.
pub struct Sidecar {
    pub addr: SocketAddr,
    pub debug: Option<SocketAddr>,
    pub stop: watch::Sender<bool>,
    pub done: tokio::task::JoinHandle<()>,
}

pub fn options(api: Api) -> Options {
    Options {
        cache: "test".into(),
        api,
        listen: "127.0.0.1:0".parse().unwrap(),
        debug_listen: None,
        request_timeout: Duration::from_secs(10),
        max_connections: 16,
    }
}

impl Sidecar {
    pub async fn start(racer: &FakeRacer, api: Api) -> Self {
        Self::start_with(racer, options(api), false).await
    }

    pub async fn start_with(racer: &FakeRacer, options: Options, debug: bool) -> Self {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let debug_listener = if debug {
            Some(tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap())
        } else {
            None
        };
        let debug = debug_listener.as_ref().map(|l| l.local_addr().unwrap());
        let (stop, shutdown) = watch::channel(false);
        let path = racer.path.clone();
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

    pub async fn get(&self, method: &str, target: &str, headers: &[(&str, &str)]) -> Response {
        let mut client = Client::connect(self.addr).await;
        client.request(method, target, headers).await
    }
}

/// A parsed response. Header names are lowercased.
#[derive(Debug, Default)]
pub struct Response {
    pub status: u16,
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
    /// The body ended early (connection closed or reset).
    pub truncated: bool,
}

impl Response {
    pub fn header(&self, name: &str) -> Option<&str> {
        let name = name.to_ascii_lowercase();
        self.headers
            .iter()
            .find(|(n, _)| *n == name)
            .map(|(_, v)| v.as_str())
    }

    pub fn text(&self) -> String {
        String::from_utf8_lossy(&self.body).to_string()
    }
}

pub struct Client {
    pub stream: TcpStream,
    buf: Vec<u8>,
}

impl Client {
    pub async fn connect(addr: SocketAddr) -> Self {
        Self {
            stream: TcpStream::connect(addr).await.unwrap(),
            buf: Vec::new(),
        }
    }

    pub async fn send(&mut self, method: &str, target: &str, headers: &[(&str, &str)]) {
        let mut req = format!("{method} {target} HTTP/1.1\r\nHost: localhost\r\n");
        for (n, v) in headers {
            req.push_str(&format!("{n}: {v}\r\n"));
        }
        req.push_str("\r\n");
        self.stream.write_all(req.as_bytes()).await.unwrap();
    }

    pub async fn request(
        &mut self,
        method: &str,
        target: &str,
        headers: &[(&str, &str)],
    ) -> Response {
        self.send(method, target, headers).await;
        self.response(method == "HEAD").await
    }

    /// Read one response; `None` if the connection closed first.
    pub async fn try_response(&mut self, head: bool) -> Option<Response> {
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

    pub async fn response(&mut self, head: bool) -> Response {
        self.try_response(head).await.expect("connection closed")
    }

    /// Whether the server closed the connection (EOF or reset).
    pub async fn closed(&mut self) -> bool {
        let mut b = [0u8; 1];
        matches!(
            tokio::time::timeout(Duration::from_secs(5), self.stream.read(&mut b)).await,
            Ok(Ok(0)) | Ok(Err(_))
        )
    }
}
