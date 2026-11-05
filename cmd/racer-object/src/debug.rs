// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Optional diagnostics listener. It serves only `/healthz` (profile with
//! `perf` instead).

use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::watch;

const READ_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_HEAD: usize = 8192;

/// Bind a TCP listener: a host may be a name or IP,
/// and an empty host means every interface.
pub async fn bind(address: &str) -> std::io::Result<TcpListener> {
    if let Some(port) = address.strip_prefix(':') {
        // Prefer a dual-stack wildcard; fall back to IPv4 only.
        if let Ok(l) = TcpListener::bind(format!("[::]:{port}")).await {
            return Ok(l);
        }
        return TcpListener::bind(format!("0.0.0.0:{port}")).await;
    }
    TcpListener::bind(address).await
}

/// Serve until `shutdown` turns true.
pub async fn serve(listener: TcpListener, mut shutdown: watch::Receiver<bool>) {
    loop {
        tokio::select! {
            biased;
            _ = crate::stopped(&mut shutdown) => return,
            accepted = listener.accept() => {
                if let Ok((stream, _)) = accepted {
                    tokio::spawn(handle(stream));
                } else {
                    tokio::time::sleep(Duration::from_millis(5)).await;
                }
            }
        }
    }
}

async fn handle(mut stream: TcpStream) {
    let mut buf = Vec::with_capacity(1024);
    let read = tokio::time::timeout(READ_TIMEOUT, async {
        loop {
            if buf.len() >= MAX_HEAD {
                return false;
            }
            buf.reserve(1024);
            match stream.read_buf(&mut buf).await {
                Ok(0) | Err(_) => return false,
                Ok(_) if buf.windows(4).any(|w| w == b"\r\n\r\n") => return true,
                Ok(_) => {}
            }
        }
    })
    .await;
    if read != Ok(true) {
        return;
    }
    let response = response(&buf);
    let _ = tokio::time::timeout(READ_TIMEOUT, stream.write_all(response.as_bytes())).await;
    let _ = stream.shutdown().await;
}

/// The response for one request head; connections close after it.
pub fn response(head: &[u8]) -> &'static str {
    let mut headers = [httparse::EMPTY_HEADER; 64];
    let mut req = httparse::Request::new(&mut headers);
    let (method, path) = match req.parse(head) {
        Ok(httparse::Status::Complete(_)) => (req.method.unwrap_or(""), req.path.unwrap_or("")),
        _ => return "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
    };
    let path = path.split('?').next().unwrap_or("");
    match (method, path) {
        ("GET" | "HEAD", "/healthz") => {
            "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
        }
        (_, "/healthz") => {
            "HTTP/1.1 405 Method Not Allowed\r\nAllow: GET, HEAD\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
        }
        _ => {
            "HTTP/1.1 404 Not Found\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 19\r\nConnection: close\r\n\r\n404 page not found\n"
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn routes() {
        assert!(response(b"GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n").starts_with("HTTP/1.1 200"));
        assert!(response(b"HEAD /healthz HTTP/1.1\r\n\r\n").starts_with("HTTP/1.1 200"));
        assert!(response(b"POST /healthz HTTP/1.1\r\n\r\n").starts_with("HTTP/1.1 405"));
        assert!(response(b"GET /debug/pprof/ HTTP/1.1\r\n\r\n").starts_with("HTTP/1.1 404"));
        assert!(response(b"bad\r\n\r\n").starts_with("HTTP/1.1 400"));
    }
}
