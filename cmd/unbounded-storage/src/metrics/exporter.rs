// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! A tiny blocking HTTP server that exposes the process-global metric
//! registry on `GET /metrics`.
//!
//! This deliberately does *not* use the io_uring data-plane machinery
//! (`crate::http`, `crate::ring`). Scraping is a low-frequency control
//! operation that must not contend for the pinned shard cores or the
//! fixed io_uring buffers. A single plain `std::net` thread, parked off
//! the shard cores, keeps the exporter fully isolated from the data
//! path and sidesteps the `SO_REUSEPORT` per-shard listener model used
//! by the frontends (which would otherwise land each scrape on a random
//! shard).
//!
//! The server speaks just enough HTTP/1.1 to answer a Prometheus
//! scraper: it reads the request line, ignores the rest of the head,
//! and replies `Connection: close`. Routes:
//!   * `GET /metrics` -> the text exposition format
//!   * `GET /` or `GET /health` -> `200 OK`
//!   * `GET /inventory/block` -> block-device discovery annotation value
//!   * anything else -> `404`
//!   * non-GET -> `405`

use std::io::{Read, Write};
use std::net::{SocketAddr, TcpListener, TcpStream};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, RwLock};
use std::thread::JoinHandle;
use std::time::Duration;

use crate::config::ConfigVersionStatus;

/// How long a worker waits for a slow client before giving up, and how
/// often the accept loop wakes to re-check the shutdown flag.
const IO_TIMEOUT: Duration = Duration::from_secs(5);
const ACCEPT_POLL: Duration = Duration::from_millis(250);

/// Largest request head the exporter will read before responding. A
/// scrape request is tiny; anything larger is rejected to bound memory.
const MAX_HEAD_BYTES: usize = 8 * 1024;

/// Reasons the exporter could not be started.
#[derive(Debug)]
pub enum ExporterError {
    /// The configured address could not be parsed.
    BadBind(String),
    /// Binding or listening on the address failed.
    Bind(std::io::Error),
}

impl std::fmt::Display for ExporterError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ExporterError::BadBind(b) => write!(f, "invalid metrics addr: {b}"),
            ExporterError::Bind(e) => write!(f, "failed to bind metrics listener: {e}"),
        }
    }
}

impl std::error::Error for ExporterError {}

#[derive(Clone, Default)]
pub struct DeviceInventoryStatus {
    block: Arc<RwLock<Option<Vec<u8>>>>,
}

impl DeviceInventoryStatus {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn set_block(&self, body: Vec<u8>) {
        *self.block.write().expect("block inventory lock poisoned") = Some(body);
    }

    fn block_body(&self) -> Option<Vec<u8>> {
        self.block
            .read()
            .expect("block inventory lock poisoned")
            .clone()
    }
}

/// Start the metrics exporter on `bind`, scraping the global registry
/// and reporting `versions`. Returns a join handle for the listener
/// thread; the thread exits once `shutdown` is set and the next accept
/// poll fires. `init` must have been called first.
pub fn spawn(
    bind: &str,
    versions: ConfigVersionStatus,
    device_inventory: DeviceInventoryStatus,
    shutdown: &'static AtomicBool,
) -> Result<JoinHandle<()>, ExporterError> {
    let addr: SocketAddr = bind
        .parse()
        .map_err(|_| ExporterError::BadBind(bind.to_string()))?;
    let listener = TcpListener::bind(addr).map_err(ExporterError::Bind)?;
    // Non-blocking accept so the loop can periodically observe the
    // shutdown flag instead of parking forever in `accept`.
    listener
        .set_nonblocking(true)
        .map_err(ExporterError::Bind)?;

    let handle = std::thread::Builder::new()
        .name("metrics-exporter".to_string())
        .spawn(move || run(listener, versions, device_inventory, shutdown))
        .map_err(ExporterError::Bind)?;
    Ok(handle)
}

fn run(
    listener: TcpListener,
    versions: ConfigVersionStatus,
    device_inventory: DeviceInventoryStatus,
    shutdown: &'static AtomicBool,
) {
    for stream in accept_loop(&listener, shutdown) {
        if let Ok(stream) = stream {
            // One scrape at a time is plenty; handle inline so a slow
            // client cannot spawn unbounded threads.
            handle_conn(stream, &versions, &device_inventory);
        }
    }
}

/// Iterator-like accept loop that polls for connections while watching
/// the shutdown flag. Yields each accepted stream until shutdown.
fn accept_loop<'a>(
    listener: &'a TcpListener,
    shutdown: &'a AtomicBool,
) -> impl Iterator<Item = std::io::Result<TcpStream>> + 'a {
    std::iter::from_fn(move || {
        loop {
            if shutdown.load(Ordering::Relaxed) {
                return None;
            }
            match listener.accept() {
                Ok((stream, _)) => return Some(Ok(stream)),
                Err(ref e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    std::thread::sleep(ACCEPT_POLL);
                    continue;
                }
                Err(e) => return Some(Err(e)),
            }
        }
    })
}

fn handle_conn(
    mut stream: TcpStream,
    versions: &ConfigVersionStatus,
    device_inventory: &DeviceInventoryStatus,
) {
    let _ = stream.set_read_timeout(Some(IO_TIMEOUT));
    let _ = stream.set_write_timeout(Some(IO_TIMEOUT));
    stream.set_nonblocking(false).ok();

    let request_line = match read_request_head(&mut stream) {
        Some(line) => line,
        None => return,
    };

    let mut parts = request_line.split_whitespace();
    let method = parts.next().unwrap_or("");
    let target = parts.next().unwrap_or("");
    let path = target.split('?').next().unwrap_or(target);

    if method != "GET" {
        let _ = write_response(
            &mut stream,
            405,
            "text/plain; charset=utf-8",
            b"method not allowed\n",
        );
        return;
    }

    match path {
        "/metrics" => {
            let body = super::render(versions);
            let _ = write_response(&mut stream, 200, super::TEXT_CONTENT_TYPE, &body);
        }
        "/inventory/block" => match device_inventory.block_body() {
            Some(body) => {
                let _ = write_response(&mut stream, 200, "text/plain; charset=utf-8", &body);
            }
            None => {
                let _ = write_response(
                    &mut stream,
                    503,
                    "text/plain; charset=utf-8",
                    b"block inventory not ready\n",
                );
            }
        },
        "/" | "/health" | "/healthz" => {
            let _ = write_response(&mut stream, 200, "text/plain; charset=utf-8", b"ok\n");
        }
        _ => {
            let _ = write_response(
                &mut stream,
                404,
                "text/plain; charset=utf-8",
                b"not found\n",
            );
        }
    }
}

/// Read the full request head (until the terminating blank line),
/// bounded by [`MAX_HEAD_BYTES`], and return the request line (the first
/// line) without its terminator. Draining the entire head matters: if we
/// close the socket with the client's request bytes still unread, Linux
/// emits a RST instead of a FIN and the client's read fails with
/// ECONNRESET. Returns `None` on EOF/error/oversize.
fn read_request_head(stream: &mut TcpStream) -> Option<String> {
    let mut buf = Vec::with_capacity(256);
    let mut byte = [0u8; 1];
    loop {
        match stream.read(&mut byte) {
            Ok(0) => break,
            Ok(_) => {
                buf.push(byte[0]);
                if buf.len() > MAX_HEAD_BYTES {
                    return None;
                }
                if buf.ends_with(b"\r\n\r\n") || buf.ends_with(b"\n\n") {
                    break;
                }
            }
            Err(_) => return None,
        }
    }
    let text = String::from_utf8(buf).ok()?;
    let line = text.lines().next().unwrap_or("").to_string();
    Some(line)
}

fn reason(status: u16) -> &'static str {
    match status {
        200 => "OK",
        404 => "Not Found",
        405 => "Method Not Allowed",
        503 => "Service Unavailable",
        _ => "OK",
    }
}

fn write_response(
    stream: &mut TcpStream,
    status: u16,
    content_type: &str,
    body: &[u8],
) -> std::io::Result<()> {
    let head = format!(
        "HTTP/1.1 {} {}\r\nContent-Type: {}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        status,
        reason(status),
        content_type,
        body.len()
    );
    stream.write_all(head.as_bytes())?;
    stream.write_all(body)?;
    stream.flush()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn get(addr: SocketAddr, path: &str) -> (String, Vec<u8>) {
        let mut s = TcpStream::connect(addr).unwrap();
        s.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        s.write_all(format!("GET {path} HTTP/1.1\r\nHost: x\r\n\r\n").as_bytes())
            .unwrap();
        let mut resp = Vec::new();
        s.read_to_end(&mut resp).unwrap();
        let split = resp
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .map(|p| p + 4)
            .unwrap_or(resp.len());
        let head = String::from_utf8_lossy(&resp[..split]).to_string();
        (head, resp[split..].to_vec())
    }

    #[test]
    fn exporter_responds_over_tcp() {
        super::super::init();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        listener.set_nonblocking(true).unwrap();
        let shutdown: &'static AtomicBool = Box::leak(Box::new(AtomicBool::new(false)));
        let versions = ConfigVersionStatus::new(9);
        let inventory = DeviceInventoryStatus::new();
        inventory.set_block(b"/dev/sdb?name=sdb&size_bytes=4096".to_vec());
        let inventory_for_thread = inventory.clone();
        let t = std::thread::spawn(move || run(listener, versions, inventory_for_thread, shutdown));

        let (head, body) = get(addr, "/metrics");
        assert!(head.contains("200 OK"), "head: {head}");
        assert!(head.contains(super::super::TEXT_CONTENT_TYPE));
        assert!(String::from_utf8_lossy(&body).contains("unbounded_storage_build_info"));

        let (head, _) = get(addr, "/health");
        assert!(head.contains("200 OK"));

        let (head, body) = get(addr, "/inventory/block");
        assert!(head.contains("200 OK"));
        assert!(head.contains("text/plain; charset=utf-8"));
        assert_eq!(body, b"/dev/sdb?name=sdb&size_bytes=4096");

        let (head, _) = get(addr, "/nope");
        assert!(head.contains("404 Not Found"));

        shutdown.store(true, Ordering::Relaxed);
        // Nudge the accept loop so it observes shutdown without waiting.
        let _ = TcpStream::connect(addr);
        t.join().unwrap();
    }

    #[test]
    fn device_inventory_reports_not_ready_until_set() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        listener.set_nonblocking(true).unwrap();
        let shutdown: &'static AtomicBool = Box::leak(Box::new(AtomicBool::new(false)));
        let versions = ConfigVersionStatus::new(9);
        let t = std::thread::spawn(move || {
            run(listener, versions, DeviceInventoryStatus::new(), shutdown)
        });

        let (head, body) = get(addr, "/inventory/block");
        assert!(head.contains("503 Service Unavailable"), "head: {head}");
        assert_eq!(body, b"block inventory not ready\n");

        shutdown.store(true, Ordering::Relaxed);
        let _ = TcpStream::connect(addr);
        t.join().unwrap();
    }
}
