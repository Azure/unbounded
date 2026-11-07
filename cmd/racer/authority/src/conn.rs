//! Strict HTTP/1.1 framing over [`TlsStream`], using the pure `http1` codec.
//!
//! Heads are decoded by [`http1::Codec`]. Servers accept only
//! `Content-Length` request bodies (the request codec rejects transfer
//! coding). Clients accept `Content-Length`, chunked, and close-delimited
//! response bodies through [`http1::small::Decoder`]. Every read has a caller
//! limit.

use crate::tls::TlsStream;
use http1::{
    Codec, StartLine,
    small::{self, Decoder, Framing, Status},
};
use std::io;

/// Most trailer bytes accepted after a final chunk.
const MAX_TRAILER_BYTES: usize = 64 * 1024;

/// Most bytes in one chunk-size line.
const MAX_CHUNK_LINE: usize = 1024;

fn invalid<E>(_: E) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, "http")
}

/// Buffered HTTP connection.
pub struct Conn {
    pub tls: TlsStream,
    buf: Vec<u8>,
}

/// Parsed request or response head.
#[derive(Debug, Default)]
pub struct Head {
    pub method: String,
    pub target: String,
    pub status: u16,
    pub headers: Vec<(String, String)>,
    /// Response body framing; `None` for requests.
    framing: Option<Framing>,
}

impl Head {
    /// The only value of a header, case-insensitive. Repeats yield `Err`.
    pub fn one(&self, name: &str) -> Result<Option<&str>, ()> {
        let mut it = self
            .headers
            .iter()
            .filter(|(k, _)| k.eq_ignore_ascii_case(name));
        let first = it.next().map(|(_, v)| v.as_str());
        if it.next().is_some() {
            Err(())
        } else {
            Ok(first)
        }
    }

    /// Number of header occurrences.
    pub fn count(&self, name: &str) -> usize {
        self.headers
            .iter()
            .filter(|(k, _)| k.eq_ignore_ascii_case(name))
            .count()
    }

    fn content_length(&self) -> io::Result<Option<usize>> {
        match self.one("content-length").map_err(invalid)? {
            None => Ok(None),
            Some(v) if !v.is_empty() && v.bytes().all(|b| b.is_ascii_digit()) => {
                v.parse().map(Some).map_err(invalid)
            }
            Some(_) => Err(invalid(())),
        }
    }

    /// Body decoder for this response head, capped at `limit` undrained bytes.
    fn decoder(&self, limit: usize) -> io::Result<Decoder> {
        let framing = self.framing.ok_or_else(|| invalid(()))?;
        Decoder::new(
            framing,
            small::Limits {
                body: limit,
                line: MAX_CHUNK_LINE,
                trailers: MAX_TRAILER_BYTES,
            },
        )
        .map_err(invalid)
    }
}

/// HTTP framing errors that map to a status.
#[derive(Debug)]
pub enum HttpError {
    TooLarge,
    Invalid,
    Io,
}

impl From<io::Error> for HttpError {
    fn from(_: io::Error) -> Self {
        Self::Io
    }
}

impl Conn {
    pub fn new(tls: TlsStream) -> Self {
        Self {
            tls,
            buf: Vec::new(),
        }
    }

    async fn fill(&mut self) -> io::Result<usize> {
        let mut chunk = [0u8; 16 * 1024];
        let n = self.tls.read(&mut chunk).await?;
        self.buf.extend_from_slice(&chunk[..n]);
        Ok(n)
    }

    /// Wait until at least one request byte is buffered. `Ok(false)` means the
    /// peer closed cleanly first. Releases an idle read buffer, so idle
    /// keep-alive connections hold no plaintext buffer.
    pub async fn wait_for_data(&mut self) -> io::Result<bool> {
        if !self.buf.is_empty() {
            return Ok(true);
        }
        if self.buf.capacity() > 0 {
            self.buf = Vec::new();
        }
        Ok(self.fill().await? != 0)
    }

    /// Read one head. `Ok(None)` means the peer closed before any byte.
    pub async fn read_head(
        &mut self,
        request: bool,
        limit: usize,
    ) -> Result<Option<Head>, HttpError> {
        let codec = Codec::<()>::new(limit).trim_ows();
        let codec = if request {
            codec
        } else {
            codec.transfer_coding()
        };
        loop {
            match codec.decode_head(&self.buf) {
                Ok(Some((message, n))) => {
                    self.buf.drain(..n);
                    return convert(message, request).map(Some);
                }
                Ok(None) => {}
                Err(http1::Error::HeadTooLarge) => return Err(HttpError::TooLarge),
                Err(http1::Error::Malformed) => return Err(HttpError::Invalid),
            }
            if self.fill().await? == 0 {
                return if self.buf.is_empty() {
                    Ok(None)
                } else {
                    Err(HttpError::Io)
                };
            }
        }
    }

    async fn exact(&mut self, n: usize) -> io::Result<Vec<u8>> {
        while self.buf.len() < n {
            if self.fill().await? == 0 {
                return Err(io::ErrorKind::UnexpectedEof.into());
            }
        }
        Ok(self.buf.drain(..n).collect())
    }

    /// Read a request body. Requests must not use transfer coding.
    pub async fn read_request_body(
        &mut self,
        head: &Head,
        limit: usize,
    ) -> Result<Vec<u8>, HttpError> {
        if head.count("transfer-encoding") != 0 {
            return Err(HttpError::Invalid);
        }
        let n = head
            .content_length()
            .map_err(|_| HttpError::Invalid)?
            .unwrap_or(0);
        if n > limit {
            return Err(HttpError::TooLarge);
        }
        Ok(self.exact(n).await?)
    }

    /// Feed buffered bytes, reading more when none are buffered. Returns
    /// whether the body is complete; read-ahead stays buffered.
    async fn feed(&mut self, decoder: &mut Decoder) -> io::Result<bool> {
        if decoder.is_done() {
            return Ok(true);
        }
        if self.buf.is_empty() && self.fill().await? == 0 {
            decoder.finish().map_err(invalid)?;
            return Ok(true);
        }
        match decoder.feed(&self.buf).map_err(invalid)? {
            Status::Done(n) => {
                self.buf.drain(..n);
                Ok(true)
            }
            Status::Need => {
                self.buf.clear();
                Ok(false)
            }
        }
    }

    /// Read a full response body by its framing.
    pub async fn read_response_body(&mut self, head: &Head, limit: usize) -> io::Result<Vec<u8>> {
        let mut decoder = head.decoder(limit)?;
        while !self.feed(&mut decoder).await? {}
        Ok(decoder.into_body())
    }

    /// Start streaming a response body, such as a watch.
    pub fn body_stream(&self, head: &Head, window: usize) -> io::Result<BodyStream> {
        Ok(BodyStream {
            decoder: head.decoder(window)?,
            done: false,
        })
    }

    /// Read the next decoded part of a streamed body. Empty means the end.
    pub async fn read_part(&mut self, stream: &mut BodyStream) -> io::Result<Vec<u8>> {
        while !stream.done {
            stream.done = self.feed(&mut stream.decoder).await?;
            let part = stream.decoder.take_body();
            if !part.is_empty() {
                return Ok(part);
            }
        }
        Ok(Vec::new())
    }
}

/// A response body read in parts; each undrained part is capped.
pub struct BodyStream {
    decoder: Decoder,
    done: bool,
}

fn convert(message: http1::MessageHead, request: bool) -> Result<Head, HttpError> {
    let mut head = Head::default();
    match &message.start {
        StartLine::Request { method, target } if request => {
            head.method.clone_from(method);
            head.target.clone_from(target);
        }
        StartLine::Response { status } if !request => {
            head.status = *status;
            head.framing = Some(if matches!(status, 204 | 304) {
                Framing::Length(0)
            } else {
                Framing::from_response(&message).map_err(|_| HttpError::Invalid)?
            });
        }
        _ => return Err(HttpError::Invalid),
    }
    for h in &message.headers {
        let v = std::str::from_utf8(&h.value).map_err(|_| HttpError::Invalid)?;
        head.headers.push((h.name.clone(), v.to_owned()));
    }
    Ok(head)
}

/// Serialize a response head with `Content-Length`.
pub fn response_head(status: u16, headers: &[(&str, &str)], length: usize, close: bool) -> Vec<u8> {
    let reason = match status {
        200 => "OK",
        204 => "No Content",
        400 => "Bad Request",
        401 => "Unauthorized",
        403 => "Forbidden",
        404 => "Not Found",
        405 => "Method Not Allowed",
        409 => "Conflict",
        413 => "Payload Too Large",
        415 => "Unsupported Media Type",
        426 => "Upgrade Required",
        429 => "Too Many Requests",
        _ => "Service Unavailable",
    };
    let mut out = format!("HTTP/1.1 {status} {reason}\r\n");
    for (k, v) in headers {
        out.push_str(&format!("{k}: {v}\r\n"));
    }
    if status != 204 {
        out.push_str(&format!("Content-Length: {length}\r\n"));
    }
    if close {
        out.push_str("Connection: close\r\n");
    }
    out.push_str("\r\n");
    out.into_bytes()
}

/// Serialize a request.
pub fn request(
    method: &str,
    host: &str,
    target: &str,
    headers: &[(&str, &str)],
    body: &[u8],
) -> Vec<u8> {
    let mut out = format!("{method} {target} HTTP/1.1\r\nHost: {host}\r\n");
    for (k, v) in headers {
        out.push_str(&format!("{k}: {v}\r\n"));
    }
    if !body.is_empty() || matches!(method, "POST" | "PUT" | "PATCH") {
        out.push_str(&format!("Content-Length: {}\r\n", body.len()));
    }
    out.push_str("\r\n");
    let mut out = out.into_bytes();
    out.extend_from_slice(body);
    out
}

/// Split a target into path and raw query.
pub fn split_target(target: &str) -> (&str, Option<&str>) {
    match target.split_once('?') {
        Some((p, q)) => (p, Some(q)),
        None => (target, None),
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::rt::{Executor, Listener};
    use crate::{rt::Tcp, tls::TlsStream};
    use rustls::pki_types::ServerName;
    use std::sync::Arc;

    /// Self-signed server config and matching client config for tests.
    pub fn tls_pair() -> (Arc<rustls::ServerConfig>, Arc<rustls::ClientConfig>) {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
        let cert = rcgen::CertificateParams::new(vec!["localhost".into()])
            .unwrap()
            .self_signed(&key)
            .unwrap();
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let server = rustls::ServerConfig::builder_with_provider(provider.clone())
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_no_client_auth()
            .with_single_cert(
                vec![cert.der().clone()],
                rustls::pki_types::PrivateKeyDer::Pkcs8(key.serialize_der().into()),
            )
            .unwrap();
        let mut roots = rustls::RootCertStore::empty();
        roots.add(cert.der().clone()).unwrap();
        let client = rustls::ClientConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_root_certificates(roots)
            .with_no_client_auth();
        (Arc::new(server), Arc::new(client))
    }

    #[test]
    fn request_response_roundtrip_with_chunked_body() {
        let (server, client) = tls_pair();
        let mut ex = Executor::new(8).unwrap();
        ex.block_on(async move {
            let listener = Listener::bind("127.0.0.1:0".parse().unwrap()).unwrap();
            let address = listener.local_addr().unwrap();
            let serve = async {
                let (tcp, _) = listener.accept().await.unwrap();
                let mut c = Conn::new(TlsStream::server(tcp, server).await.unwrap());
                let head = c.read_head(true, 4096).await.unwrap().unwrap();
                assert_eq!(head.method, "POST");
                assert_eq!(head.target, "/x?a=1");
                let body = c.read_request_body(&head, 100).await.unwrap();
                assert_eq!(body, b"hello");
                c.tls
                    .write_all(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n")
                    .await
                    .unwrap();
                let head = c.read_head(true, 4096).await.unwrap().unwrap();
                assert!(matches!(
                    c.read_request_body(&head, 2).await,
                    Err(HttpError::TooLarge)
                ));
            };
            let call = async {
                let tcp = Tcp::connect(address).await.unwrap();
                let tls = TlsStream::client(tcp, client, ServerName::try_from("localhost").unwrap())
                    .await
                    .unwrap();
                let mut c = Conn::new(tls);
                c.tls.write_all(&request("POST", "h", "/x?a=1", &[], b"hello")).await.unwrap();
                let head = c.read_head(false, 4096).await.unwrap().unwrap();
                assert_eq!(head.status, 200);
                assert_eq!(c.read_response_body(&head, 100).await.unwrap(), b"abcde");
                c.tls.write_all(&request("POST", "h", "/", &[], b"toolong")).await.unwrap();
            };
            futures::join!(serve, call);
        });
    }

    /// The same TLS and framing code runs on the simulated driver with a
    /// virtual clock: a roundtrip completes and a timer fires without any
    /// wall-clock wait.
    #[test]
    fn roundtrip_and_timeout_on_the_simulated_driver() {
        let (server, client) = tls_pair();
        let clock = racer_io::ctx::VirtualClock::new();
        let reactor = racer_io::Reactor::new(
            racer_io::Driver::Sim(racer_io::sim::SimOs::seeded(7)),
            racer_io::ring::Config::default(),
            racer_io::WorkerCtx::simulated(&clock, 7, 0),
        )
        .unwrap();
        let mut ex = crate::rt::Executor::with_reactor(reactor, 8);
        let wall = std::time::Instant::now();
        let started = clock.now();
        ex.block_on(async move {
            let address = "10.0.0.1:443".parse().unwrap();
            let listener = crate::rt::Listener::bind(address).unwrap();
            let serve = async {
                let (tcp, _) = listener.accept().await.unwrap();
                let mut c = Conn::new(TlsStream::server(tcp, server).await.unwrap());
                let head = c.read_head(true, 4096).await.unwrap().unwrap();
                assert_eq!(head.target, "/sim");
                c.tls
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
                    .await
                    .unwrap();
            };
            let call = async {
                let tcp = Tcp::connect(address).await.unwrap();
                let tls =
                    TlsStream::client(tcp, client, ServerName::try_from("localhost").unwrap())
                        .await
                        .unwrap();
                let mut c = Conn::new(tls);
                c.tls
                    .write_all(&request("GET", "h", "/sim", &[], b""))
                    .await
                    .unwrap();
                let head = c.read_head(false, 4096).await.unwrap().unwrap();
                assert_eq!(c.read_response_body(&head, 100).await.unwrap(), b"ok");
            };
            futures::join!(serve, call);
            assert_eq!(
                crate::rt::timeout(
                    std::time::Duration::from_secs(3600),
                    crate::rt::sleep(std::time::Duration::from_secs(7200))
                )
                .await,
                Err(crate::Error::Timeout)
            );
        });
        assert!(clock.now() - started >= std::time::Duration::from_secs(3600));
        assert!(wall.elapsed() < std::time::Duration::from_secs(60));
    }

    #[test]
    fn malformed_and_oversized_chunks_fail() {
        let cases: [&[u8]; 6] = [
            b"+3\r\nabc\r\n0\r\n\r\n",
            b"-1\r\n",
            b"\r\nabc\r\n0\r\n\r\n",
            b"zz\r\n",
            b"65\r\n",
            b"3\r\nabcXX0\r\n\r\n",
        ];
        for case in cases {
            let (server, client) = tls_pair();
            let mut ex = Executor::new(8).unwrap();
            ex.block_on(async move {
                let listener = Listener::bind("127.0.0.1:0".parse().unwrap()).unwrap();
                let address = listener.local_addr().unwrap();
                let serve = async {
                    let (tcp, _) = listener.accept().await.unwrap();
                    let mut c = Conn::new(TlsStream::server(tcp, server).await.unwrap());
                    let mut out = b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n".to_vec();
                    out.extend_from_slice(case);
                    c.tls.write_all(&out).await.unwrap();
                    c.tls.close().await;
                };
                let call = async {
                    let tcp = Tcp::connect(address).await.unwrap();
                    let tls =
                        TlsStream::client(tcp, client, ServerName::try_from("localhost").unwrap())
                            .await
                            .unwrap();
                    let mut c = Conn::new(tls);
                    let head = c.read_head(false, 4096).await.unwrap().unwrap();
                    // Limit 100 bytes: 0x65 = 101 is oversized.
                    assert!(
                        c.read_response_body(&head, 100).await.is_err(),
                        "{}",
                        String::from_utf8_lossy(case)
                    );
                };
                futures::join!(serve, call);
            });
        }
    }

    #[test]
    fn head_limits_and_duplicates() {
        let head = Head {
            headers: vec![("A".into(), "1".into()), ("a".into(), "2".into())],
            ..Head::default()
        };
        assert!(head.one("a").is_err());
        assert_eq!(head.count("A"), 2);
        assert_eq!(split_target("/p?q"), ("/p", Some("q")));
        let r = response_head(204, &[], 0, true);
        assert!(!String::from_utf8(r).unwrap().contains("Content-Length"));
    }
}
