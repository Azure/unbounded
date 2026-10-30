//! TLS streams and strict HTTP/1.1 framing over the control reactor.
//!
//! Servers accept only `Content-Length` request bodies. Clients accept
//! `Content-Length`, chunked, and close-delimited response bodies. Every read
//! has a caller limit.

use crate::exec::{self, Interest, Tcp};
use rustls::pki_types::ServerName;
use std::{
    io::{self, Read, Write},
    sync::Arc,
};

/// TLS over a nonblocking TCP stream.
pub struct TlsStream {
    tcp: Tcp,
    conn: rustls::Connection,
}

fn invalid<E>(_: E) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, "tls")
}

impl TlsStream {
    /// Connect TLS as a client and finish the handshake.
    pub async fn client(
        tcp: Tcp,
        config: Arc<rustls::ClientConfig>,
        name: ServerName<'static>,
    ) -> io::Result<Self> {
        let conn = rustls::ClientConnection::new(config, name).map_err(invalid)?;
        let mut s = Self {
            tcp,
            conn: conn.into(),
        };
        s.handshake().await?;
        Ok(s)
    }

    /// Accept TLS as a server and finish the handshake.
    pub async fn server(tcp: Tcp, config: Arc<rustls::ServerConfig>) -> io::Result<Self> {
        let conn = rustls::ServerConnection::new(config).map_err(invalid)?;
        let mut s = Self {
            tcp,
            conn: conn.into(),
        };
        s.handshake().await?;
        Ok(s)
    }

    async fn handshake(&mut self) -> io::Result<()> {
        while self.conn.is_handshaking() {
            self.flush().await?;
            if self.conn.is_handshaking() && self.conn.wants_read() && self.read_tls().await? == 0 {
                return Err(io::ErrorKind::UnexpectedEof.into());
            }
        }
        self.flush().await
    }

    /// DER chain sent by the peer, if any.
    pub fn peer_certificates(&self) -> Vec<Vec<u8>> {
        self.conn
            .peer_certificates()
            .map(|c| c.iter().map(|c| c.to_vec()).collect())
            .unwrap_or_default()
    }

    async fn read_tls(&mut self) -> io::Result<usize> {
        loop {
            match self.conn.read_tls(self.tcp.std()) {
                Ok(n) => {
                    if let Err(e) = self.conn.process_new_packets() {
                        let _ = self.flush().await;
                        return Err(invalid(e));
                    }
                    return Ok(n);
                }
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                    exec::ready(self.tcp.fd(), Interest::Read).await
                }
                Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                Err(e) => return Err(e),
            }
        }
    }

    /// Write all pending TLS records.
    pub async fn flush(&mut self) -> io::Result<()> {
        while self.conn.wants_write() {
            match self.conn.write_tls(self.tcp.std()) {
                Ok(_) => {}
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                    exec::ready(self.tcp.fd(), Interest::Write).await
                }
                Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                Err(e) => return Err(e),
            }
        }
        Ok(())
    }

    /// Read plaintext. Returns 0 at a clean end of stream.
    pub async fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        loop {
            match self.conn.reader().read(buf) {
                Ok(n) => return Ok(n),
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {}
                Err(e) if e.kind() == io::ErrorKind::UnexpectedEof => return Ok(0),
                Err(e) => return Err(e),
            }
            self.flush().await?;
            if self.read_tls().await? == 0 {
                return Ok(0);
            }
        }
    }

    /// Write and flush all plaintext.
    pub async fn write_all(&mut self, mut data: &[u8]) -> io::Result<()> {
        while !data.is_empty() {
            let n = self
                .conn
                .writer()
                .write(&data[..data.len().min(64 * 1024)])?;
            data = &data[n..];
            self.flush().await?;
        }
        self.flush().await
    }

    /// Send close_notify. Errors are ignored.
    pub async fn close(&mut self) {
        self.conn.send_close_notify();
        let _ = self.flush().await;
    }
}

/// Most trailer lines accepted after a final chunk.
const MAX_TRAILERS: usize = 64;

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
        loop {
            let mut headers = [httparse::EMPTY_HEADER; 96];
            let parsed = if request {
                let mut r = httparse::Request::new(&mut headers);
                match r.parse(&self.buf) {
                    Ok(httparse::Status::Complete(n)) => Some((
                        n,
                        Head {
                            method: r.method.unwrap_or_default().into(),
                            target: r.path.unwrap_or_default().into(),
                            status: 0,
                            headers: collect(r.headers)?,
                        },
                        r.version,
                    )),
                    Ok(httparse::Status::Partial) => None,
                    Err(_) => return Err(HttpError::Invalid),
                }
            } else {
                let mut r = httparse::Response::new(&mut headers);
                match r.parse(&self.buf) {
                    Ok(httparse::Status::Complete(n)) => Some((
                        n,
                        Head {
                            status: r.code.unwrap_or_default(),
                            headers: collect(r.headers)?,
                            ..Head::default()
                        },
                        r.version,
                    )),
                    Ok(httparse::Status::Partial) => None,
                    Err(_) => return Err(HttpError::Invalid),
                }
            };
            if let Some((n, head, version)) = parsed {
                if n > limit {
                    return Err(HttpError::TooLarge);
                }
                if version != Some(1) {
                    return Err(HttpError::Invalid);
                }
                self.buf.drain(..n);
                return Ok(Some(head));
            }
            if self.buf.len() > limit {
                return Err(HttpError::TooLarge);
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

    async fn line(&mut self, limit: usize) -> io::Result<Vec<u8>> {
        loop {
            if let Some(i) = self.buf.windows(2).position(|w| w == b"\r\n") {
                let line = self.buf.drain(..i + 2).take(i).collect();
                return Ok(line);
            }
            if self.buf.len() > limit {
                return Err(invalid(()));
            }
            if self.fill().await? == 0 {
                return Err(io::ErrorKind::UnexpectedEof.into());
            }
        }
    }

    /// Read the next chunk of a chunked body. Empty means the end.
    pub async fn read_chunk(&mut self, limit: usize) -> io::Result<Vec<u8>> {
        let line = self.line(1024).await?;
        let size = line.split(|b| *b == b';').next().unwrap_or_default();
        let size = std::str::from_utf8(size)
            .map_err(invalid)?
            .trim_end_matches([' ', '\t']);
        // Strict hex: `from_str_radix` would also accept a sign.
        if size.is_empty() || size.len() > 15 || !size.bytes().all(|b| b.is_ascii_hexdigit()) {
            return Err(invalid(()));
        }
        let n = usize::from_str_radix(size, 16).map_err(invalid)?;
        if n > limit {
            return Err(invalid(()));
        }
        if n == 0 {
            // Trailers are ignored up to the final empty line.
            for _ in 0..MAX_TRAILERS {
                if self.line(8 * 1024).await?.is_empty() {
                    return Ok(Vec::new());
                }
            }
            return Err(invalid(()));
        }
        let data = self.exact(n).await?;
        if !self.exact(2).await?.eq(b"\r\n") {
            return Err(invalid(()));
        }
        Ok(data)
    }

    /// Read a full response body by its framing.
    pub async fn read_response_body(&mut self, head: &Head, limit: usize) -> io::Result<Vec<u8>> {
        if is_chunked(head)? {
            let mut body = Vec::new();
            loop {
                let chunk = self.read_chunk(limit).await?;
                if chunk.is_empty() {
                    return Ok(body);
                }
                if body.len() + chunk.len() > limit {
                    return Err(invalid(()));
                }
                body.extend(chunk);
            }
        }
        if let Some(n) = head.content_length()? {
            if n > limit {
                return Err(invalid(()));
            }
            return self.exact(n).await;
        }
        if matches!(head.status, 204 | 304) {
            return Ok(Vec::new());
        }
        loop {
            if self.buf.len() > limit {
                return Err(invalid(()));
            }
            if self.fill().await? == 0 {
                return Ok(std::mem::take(&mut self.buf));
            }
        }
    }
}

/// Whether the response uses chunked transfer coding.
pub fn is_chunked(head: &Head) -> io::Result<bool> {
    match head.count("transfer-encoding") {
        0 => Ok(false),
        1 if head
            .one("transfer-encoding")
            .ok()
            .flatten()
            .is_some_and(|v| v.eq_ignore_ascii_case("chunked")) =>
        {
            Ok(true)
        }
        _ => Err(invalid(())),
    }
}

fn collect(headers: &[httparse::Header<'_>]) -> Result<Vec<(String, String)>, HttpError> {
    headers
        .iter()
        .map(|h| {
            let v = std::str::from_utf8(h.value).map_err(|_| HttpError::Invalid)?;
            Ok((h.name.to_owned(), v.to_owned()))
        })
        .collect()
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
    use crate::exec::{Executor, Listener};

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
