use super::*;
use crate::test_support::enrollment::{drive, reactor, scope};
use std::cell::Cell;
use std::io::{Read, Write};
use std::net::TcpListener;
use std::sync::atomic::{AtomicUsize, Ordering};

/// Maps every name to fixed addresses and counts queries.
pub(crate) struct MapResolver {
    pub addresses: Vec<SocketAddr>,
    pub queries: Cell<usize>,
}
impl MapResolver {
    pub fn new(addresses: Vec<SocketAddr>) -> Self {
        Self {
            addresses,
            queries: Cell::new(0),
        }
    }
}
impl Resolver for MapResolver {
    fn resolve<'a>(
        &'a self,
        _host: &'a str,
        _port: u16,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Vec<SocketAddr>> {
        self.queries.set(self.queries.get() + 1);
        let addresses = self.addresses.clone();
        Box::pin(async move {
            if addresses.is_empty() {
                Err(Error::Unavailable)
            } else {
                Ok(addresses)
            }
        })
    }
}

/// A CA-signed ECDSA server identity for `names` and a client trust store.
pub(crate) struct TestIdentity {
    pub roots: rustls::RootCertStore,
    pub server: Arc<rustls::ServerConfig>,
}
pub(crate) fn test_identity(names: &[&str]) -> TestIdentity {
    let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
    let ca = ca_params.self_signed(&ca_key).unwrap();
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
    let params =
        rcgen::CertificateParams::new(names.iter().map(|n| n.to_string()).collect::<Vec<_>>())
            .unwrap();
    let cert = params.signed_by(&key, &ca, &ca_key).unwrap();
    let server = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_no_client_auth()
    .with_single_cert(
        vec![cert.der().clone()],
        rustls::pki_types::PrivateKeyDer::Pkcs8(key.serialize_der().into()),
    )
    .unwrap();
    TestIdentity {
        roots: http1::tls::trust_store_from_pem(ca.pem().as_bytes()).unwrap(),
        server: Arc::new(server),
    }
}

/// Read one request head and its Content-Length body.
pub(crate) fn read_request(stream: &mut impl Read) -> Option<(String, Vec<u8>)> {
    let mut head = Vec::new();
    let mut byte = [0];
    while !head.ends_with(b"\r\n\r\n") {
        match stream.read(&mut byte) {
            Ok(1) => head.push(byte[0]),
            _ => return None,
        }
    }
    let head = String::from_utf8(head).unwrap();
    let length = head
        .lines()
        .find_map(|l| {
            l.to_ascii_lowercase()
                .strip_prefix("content-length:")
                .map(|v| v.trim().parse::<usize>().unwrap())
        })
        .unwrap_or(0);
    let mut body = vec![0; length];
    stream.read_exact(&mut body).ok()?;
    Some((head, body))
}

/// Serve `connections` connections on a thread; `handler` sees each request and
/// returns raw response bytes. Requests on one connection are served in order.
pub(crate) fn serve(
    tls: Option<Arc<rustls::ServerConfig>>,
    connections: usize,
    handler: impl Fn(&str, &[u8]) -> Vec<u8> + Send + 'static,
) -> (SocketAddr, Arc<AtomicUsize>, std::thread::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let accepted = Arc::new(AtomicUsize::new(0));
    let counter = accepted.clone();
    let handle = std::thread::spawn(move || {
        for _ in 0..connections {
            let Ok((tcp, _)) = listener.accept() else {
                return;
            };
            counter.fetch_add(1, Ordering::SeqCst);
            let mut stream: Box<dyn ReadWrite> = match &tls {
                Some(config) => Box::new(rustls::StreamOwned::new(
                    rustls::ServerConnection::new(config.clone()).unwrap(),
                    tcp,
                )),
                None => Box::new(tcp),
            };
            while let Some((head, body)) = read_request(&mut stream) {
                let response = handler(&head, &body);
                if stream.write_all(&response).is_err() || stream.flush().is_err() {
                    break;
                }
                if head.to_ascii_lowercase().contains("connection: close") {
                    break;
                }
            }
        }
    });
    (address, accepted, handle)
}
pub(crate) trait ReadWrite: Read + Write {}
impl<T: Read + Write> ReadWrite for T {}

#[test]
fn small_response_framings() {
    let complete = b"HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-A: b \r\n\r\nhello";
    let parsed = parse_small_response(complete, false, 64).unwrap().unwrap();
    assert_eq!(parsed.status, 200);
    assert_eq!(&parsed.body[..], b"hello");
    assert_eq!(parsed.header("x-a"), Some("b"));
    assert!(
        parse_small_response(&complete[..complete.len() - 1], false, 64)
            .unwrap()
            .is_none()
    );
    assert!(parse_small_response(&complete[..complete.len() - 1], true, 64).is_err());
    assert!(parse_small_response(complete, false, 4).is_err());

    let chunked = b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3;x=y\r\nabc\r\n2\r\nde\r\n0\r\nTrailer: t\r\n\r\n";
    for split in 0..chunked.len() {
        let partial = parse_small_response(&chunked[..split], false, 64).unwrap();
        assert!(partial.is_none(), "split {split}");
    }
    let parsed = parse_small_response(chunked, false, 64).unwrap().unwrap();
    assert_eq!(&parsed.body[..], b"abcde");
    assert!(parse_small_response(chunked, false, 4).is_err());
    assert!(
        parse_small_response(
            b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabcX\r\n0\r\n\r\n",
            false,
            64
        )
        .is_err()
    );
    assert!(
        parse_small_response(
            b"HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip\r\n\r\n",
            true,
            64
        )
        .is_err()
    );

    let close = b"HTTP/1.1 400 Bad\r\n\r\n{}";
    assert!(parse_small_response(close, false, 64).unwrap().is_none());
    let parsed = parse_small_response(close, true, 64).unwrap().unwrap();
    assert_eq!(parsed.status, 400);
    assert_eq!(&parsed.body[..], b"{}");
    assert!(parse_small_response(b"HTTP/1.1 100 Continue\r\n\r\n", false, 64).is_err());
}

#[test]
fn endpoint_identity_authority_and_validation() {
    let a = RemoteEndpoint::http("Account.Blob.Core.Windows.Net", 80).unwrap();
    assert_eq!(a.host(), "account.blob.core.windows.net");
    assert_eq!(a.authority(), "account.blob.core.windows.net");
    assert!(a.needs_resolve(racer_io::Clock::host().now()));
    let v6 = RemoteEndpoint::http("[::1]", 10000).unwrap();
    assert_eq!(v6.authority(), "[::1]:10000");
    assert!(!v6.needs_resolve(racer_io::Clock::host().now()));
    assert_eq!(v6.address().unwrap(), "[::1]:10000".parse().unwrap());
    let imds = RemoteEndpoint::http("169.254.169.254", 80).unwrap();
    assert_eq!(imds.authority(), "169.254.169.254");
    let roots = test_identity(&["localhost"]).roots;
    let https = RemoteEndpoint::https("localhost", 8443, http1::tls::client_config(roots)).unwrap();
    assert!(https.is_https());
    assert_eq!(https.authority(), "localhost:8443");
    assert_ne!(https, RemoteEndpoint::http("localhost", 8443).unwrap());
    assert_eq!(
        https.clone(),
        RemoteEndpoint::https(
            "localhost",
            8443,
            http1::tls::client_config(test_identity(&["x"]).roots)
        )
        .unwrap()
    );
    for bad in ["", "a b", "a/b", "a\r\n"] {
        assert!(RemoteEndpoint::http(bad, 80).is_err(), "{bad:?}");
    }
    assert!(RemoteEndpoint::http("a", 0).is_err());
}

#[test]
fn resolution_caches_rotates_and_keeps_stale_addresses() {
    let Some(reactor) = reactor() else { return };
    let endpoint = RemoteEndpoint::http("origin.example", 80).unwrap();
    assert_eq!(endpoint.address().err(), Some(Error::Unavailable));
    let resolver = MapResolver::new(vec![
        "10.0.0.1:80".parse().unwrap(),
        "10.0.0.2:80".parse().unwrap(),
    ]);
    let scope = scope();
    drive(&reactor, Box::pin(endpoint.resolve(&resolver, &scope))).unwrap();
    drive(&reactor, Box::pin(endpoint.resolve(&resolver, &scope))).unwrap();
    assert_eq!(resolver.queries.get(), 1);
    let first = endpoint.address().unwrap();
    let second = endpoint.address().unwrap();
    assert_ne!(first, second);
    assert_eq!(endpoint.address().unwrap(), first);
    endpoint.expire_addresses();
    let failing = MapResolver::new(Vec::new());
    drive(&reactor, Box::pin(endpoint.resolve(&failing, &scope))).unwrap();
    assert_eq!(failing.queries.get(), 1);
    assert!(endpoint.address().is_ok());
    let fresh = RemoteEndpoint::http("other.example", 80).unwrap();
    assert_eq!(
        drive(&reactor, Box::pin(fresh.resolve(&failing, &scope))).err(),
        Some(Error::Unavailable)
    );
}

#[test]
fn re_resolution_keeps_rotating_instead_of_pinning_the_first_address() {
    let Some(reactor) = reactor() else { return };
    let endpoint = RemoteEndpoint::http("origin.example", 80).unwrap();
    let dead: SocketAddr = "10.0.0.1:80".parse().unwrap();
    let live: SocketAddr = "10.0.0.2:80".parse().unwrap();
    let resolver = MapResolver::new(vec![dead, live]);
    let scope = scope();
    assert_eq!(endpoint.address_count(), 0);
    for _ in 0..4 {
        drive(&reactor, Box::pin(endpoint.resolve(&resolver, &scope))).unwrap();
        assert_eq!(endpoint.address_count(), 2);
        assert_eq!(endpoint.address().unwrap(), dead);
        // A failed connect expires the cache; the re-resolved set must not
        // send the next attempt back to the dead first address.
        endpoint.expire_addresses();
        drive(&reactor, Box::pin(endpoint.resolve(&resolver, &scope))).unwrap();
        assert_eq!(endpoint.address().unwrap(), live);
    }
    assert_eq!(RemoteEndpoint::http("[::1]", 1).unwrap().address_count(), 1);
}

fn json_response(body: &str, chunked: bool) -> Vec<u8> {
    if chunked {
        let (a, b) = body.split_at(body.len() / 2);
        format!(
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n{:x}\r\n{a}\r\n{:x}\r\n{b}\r\n0\r\n\r\n",
            a.len(),
            b.len()
        )
        .into_bytes()
    } else {
        format!(
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
            body.len()
        )
        .into_bytes()
    }
}

#[test]
fn small_exchange_over_http_and_https_by_name() {
    let Some(reactor) = reactor() else { return };
    let identity = test_identity(&["token.example"]);
    for (tls, chunked) in [(false, false), (false, true), (true, false), (true, true)] {
        let (address, _, server) = serve(
            tls.then(|| identity.server.clone()),
            1,
            move |head, body| {
                assert!(head.starts_with("POST /path?q=1 HTTP/1.1\r\n"));
                assert!(head.contains("Host: token.example:"));
                assert!(head.contains("Metadata: true\r\n"));
                assert_eq!(body, b"a=b");
                json_response("{\"ok\":true}", chunked)
            },
        );
        let endpoint = if tls {
            RemoteEndpoint::https(
                "token.example",
                address.port(),
                http1::tls::client_config(identity.roots.clone()),
            )
        } else {
            RemoteEndpoint::http("token.example", address.port())
        }
        .unwrap();
        let resolver = MapResolver::new(vec![address]);
        let scope = scope();
        let response = drive(
            &reactor,
            Box::pin(exchange_small(
                &reactor,
                &resolver,
                &endpoint,
                &SmallRequest {
                    method: "POST",
                    target: "/path?q=1",
                    headers: &[("Metadata", "true")],
                    body: b"a=b",
                },
                MAX_SMALL_BODY,
                &scope,
            )),
        )
        .unwrap();
        assert_eq!(response.status, 200);
        assert_eq!(&response.body[..], b"{\"ok\":true}");
        server.join().unwrap();
    }
}

/// Pooled ranged GETs to a DNS-named endpoint, plain and HTTPS (automatic
/// record-layer placement), reading a 16 MiB body in bounded chunks.
#[test]
fn pooled_remote_endpoint_range_reads() {
    let Some(reactor) = reactor() else { return };
    const BODY: usize = 16 << 20;
    let identity = test_identity(&["account.blob.example"]);
    for tls in [false, true] {
        let (address, accepted, server) = serve(
            tls.then(|| identity.server.clone()),
            1,
            move |head, _| {
                assert!(head.contains("x-ms-version: 2021-08-06\r\n"));
                assert!(head.contains("Range: bytes=0-16777215\r\n"));
                let mut response = format!(
                    "HTTP/1.1 206 Partial Content\r\nContent-Length: {BODY}\r\nContent-Range: bytes 0-{}/{}\r\nETag: \"0x8D\"\r\nx-ms-blob-type: BlockBlob\r\n\r\n",
                    BODY - 1,
                    BODY * 2
                )
                .into_bytes();
                response.extend((0..BODY).map(|i| (i % 251) as u8));
                response
            },
        );
        let endpoint = crate::http::Endpoint::Remote(
            if tls {
                RemoteEndpoint::https(
                    "account.blob.example",
                    address.port(),
                    http1::tls::client_config(identity.roots.clone()),
                )
            } else {
                RemoteEndpoint::http("account.blob.example", address.port())
            }
            .unwrap(),
        );
        let crate::http::Endpoint::Remote(remote) = &endpoint else {
            unreachable!()
        };
        let resolver = MapResolver::new(vec![address]);
        let admission = reactor.admission().clone();
        let pool = crate::http::new_pool(reactor.clone(), admission.clone(), 2);
        let io = crate::http::client_io(reactor.clone(), admission);
        for _ in 0..2 {
            let scope = scope();
            drive(&reactor, Box::pin(remote.resolve(&resolver, &scope))).unwrap();
            let checked = drive(
                &reactor,
                Box::pin(async {
                    let connection = pool.checkout(&endpoint, &scope).await?;
                    let head = http1::MessageHead {
                        start: http1::StartLine::Request {
                            method: "GET".into(),
                            target: "/container/blob".into(),
                        },
                        headers: vec![
                            http1::Header::new("Host", remote.authority()),
                            http1::Header::new("x-ms-version", "2021-08-06"),
                            http1::Header::new("Range", format!("bytes=0-{}", BODY - 1)),
                        ],
                    };
                    let sent = io.send_head(connection, head, &scope).await?;
                    let response = io.receive_head(sent.connection, &scope).await?;
                    assert!(matches!(
                        response.value.start,
                        http1::StartLine::Response { status: 206 }
                    ));
                    assert_eq!(
                        response.value.unique("content-range")?,
                        Some(format!("bytes 0-{}/{}", BODY - 1, BODY * 2).as_bytes())
                    );
                    assert_eq!(response.value.unique("etag")?, Some(&b"\"0x8D\""[..]));
                    let mut connection = response.connection;
                    let mut offset = 0;
                    while offset < BODY {
                        let buffer = io.buffer(256 * 1024)?;
                        let read = io.read_body_exact(connection, buffer, &scope).await?;
                        use racer_io::ring::IoBuffer as _;
                        let bytes = read.buffer.bytes()?;
                        assert!(
                            bytes
                                .iter()
                                .enumerate()
                                .all(|(i, b)| *b == ((offset + i) % 251) as u8)
                        );
                        offset += bytes.len();
                        connection = read.lease;
                    }
                    connection
                        .finish_exchange()
                        .map_err(|_| Error::BadGateway)?;
                    Ok(offset)
                }),
            )
            .unwrap();
            assert_eq!(checked, BODY);
        }
        assert_eq!(accepted.load(Ordering::SeqCst), 1, "keep-alive reuse");
        assert_eq!(resolver.queries.get(), 1, "cached resolution");
        pool.close();
        drop(pool);
        server.join().unwrap();
    }
}

#[test]
fn overlapping_exchanges_each_try_every_address() {
    let Some(reactor) = reactor() else { return };
    let endpoint = RemoteEndpoint::http("origin.example", 80).unwrap();
    let refused: SocketAddr = "10.0.0.1:80".parse().unwrap();
    let healthy: SocketAddr = "10.0.0.2:80".parse().unwrap();
    let resolver = MapResolver::new(vec![refused, healthy]);
    drive(&reactor, Box::pin(endpoint.resolve(&resolver, &scope()))).unwrap();
    let a = endpoint.address_sequence();
    let b = endpoint.address_sequence();
    assert!(a == endpoint && b == endpoint);
    // A connects first and is refused, B connects in between, then A retries:
    // A must move on to the healthy address rather than follow the shared
    // cursor back to the refused one.
    assert_eq!(a.address().unwrap(), refused);
    assert_eq!(b.address().unwrap(), healthy);
    assert_eq!(a.address().unwrap(), healthy);
    // Starting addresses still rotate across exchanges.
    assert_eq!(endpoint.address().unwrap(), refused);
    let c = endpoint.address_sequence();
    assert_eq!(c.address().unwrap(), healthy);
    assert_eq!(c.address().unwrap(), refused);
    // A sequence that never connects (idle reuse) leaves the rotation alone.
    let _unused = endpoint.address_sequence();
    assert_eq!(endpoint.address().unwrap(), refused);
}
