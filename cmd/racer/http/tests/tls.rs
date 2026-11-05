//! Pooled HTTPS exchanges through kernel TLS and the userspace relay.
use flow_control::{Charge, Policy, Quotas, Rejection};
use http1::connection::{self, Context as HttpContext, Endpoint as EndpointPolicy, PoolConfig};
use http1::tls::{TlsClient, TlsMode, rustls};
use http1::{Codec, Header, MessageHead, StartLine};
use racer_io::cancel::Cancellation;
use racer_io::ring::IoBuffer;
use racer_io::{Scope, ring::SocketAddress};
use std::{
    future::Future,
    io::{Read, Write},
    net::{SocketAddr, TcpListener},
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Poll, Waker},
    time::{Duration, Instant},
};

const PAGE: usize = 16 << 20;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Error {
    Http(http1::Error),
    Runtime(racer_io::Error),
    Quota(flow_control::Error),
}
impl From<http1::Error> for Error {
    fn from(e: http1::Error) -> Self {
        Self::Http(e)
    }
}
impl From<racer_io::Error> for Error {
    fn from(e: racer_io::Error) -> Self {
        Self::Runtime(e)
    }
}
impl From<flow_control::Error> for Error {
    fn from(e: flow_control::Error) -> Self {
        Self::Quota(e)
    }
}
type Result<T> = std::result::Result<T, Error>;

#[derive(Clone, Copy)]
enum Resource {
    Context,
    Connection,
}
impl flow_control::Class for Resource {
    const COUNT: usize = 2;

    fn index(self) -> usize {
        self as usize
    }
}
struct Limits(usize);
impl Policy for Limits {
    type Class = Resource;

    type Key = ();

    fn limit(&self, class: Resource) -> usize {
        match class {
            Resource::Context => self.0,
            Resource::Connection => 64,
        }
    }

    fn max_keys(&self) -> usize {
        1
    }

    fn wakes(_: Resource) -> bool {
        true
    }

    fn covers(_: Resource) -> bool {
        false
    }

    fn rejected(&self, _: Rejection<Resource>) {}
}
#[derive(Clone)]
struct RequestScope {
    deadline: Instant,

    cancellation: Cancellation,
}
impl Scope for RequestScope {
    type Error = Error;

    fn check(&self) -> Result<()> {
        if self.cancellation.is_cancelled() {
            return Err(racer_io::Error::Cancelled.into());
        }
        if Instant::now() >= self.deadline {
            return Err(racer_io::Error::DeadlineExceeded.into());
        }
        Ok(())
    }

    fn cancel_token(&self) -> Option<&racer_io::CancelToken> {
        Some(self.cancellation.token())
    }
}
fn scope() -> RequestScope {
    RequestScope {
        deadline: Instant::now() + Duration::from_secs(60),
        cancellation: Cancellation::new().unwrap(),
    }
}

/// Address plus optional TLS identity; ordered by address and mode only.
#[derive(Clone)]
struct Endpoint {
    address: SocketAddr,

    tls: Option<TlsClient>,
}
impl PartialEq for Endpoint {
    fn eq(&self, other: &Self) -> bool {
        self.cmp(other).is_eq()
    }
}
impl Eq for Endpoint {}
impl PartialOrd for Endpoint {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(other))
    }
}
impl Ord for Endpoint {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        (self.address, self.tls.as_ref().map(TlsClient::mode))
            .cmp(&(other.address, other.tls.as_ref().map(TlsClient::mode)))
    }
}
impl EndpointPolicy<Error> for Endpoint {
    fn address(&self) -> Result<SocketAddress> {
        Ok(SocketAddress::Inet(self.address))
    }

    fn tls(&self) -> Option<&TlsClient> {
        self.tls.as_ref()
    }
}
struct Caller(std::rc::Rc<Quotas<Limits>>);
impl HttpContext for Caller {
    type Error = Error;

    type Scope = RequestScope;

    type Reactor = std::rc::Rc<Reactor>;

    type Charge = Charge<Limits>;

    type Slot = Charge<Limits>;

    type Opaque = ();

    type State = ();

    type Endpoint = Endpoint;

    fn charge(&self, bytes: usize) -> Result<Self::Charge> {
        Ok(self.0.reserve(None, Resource::Context, bytes)?)
    }

    fn outbound_slot(&self) -> Result<Self::Slot> {
        Ok(self.0.reserve(None, Resource::Connection, 1)?)
    }

    fn stopped(&self) -> bool {
        self.0.is_stopped()
    }
}
type Reactor = racer_io::scoped::Reactor<RequestScope>;
type Pool = connection::HttpPool<Caller>;
type Io = connection::HttpIo<Caller>;

struct Fixture {
    reactor: std::rc::Rc<Reactor>,
    pool: Pool,
    io: Io,
    admission: std::rc::Rc<Quotas<Limits>>,
}
fn fixture() -> Fixture {
    fixture_with(1 << 30)
}
fn fixture_with(context: usize) -> Fixture {
    let admission = std::rc::Rc::new(Quotas::new(Limits(context)));
    let reactor = std::rc::Rc::new(Reactor::new(64, &racer_io::Env::host()));
    reactor.init().unwrap();
    let pool = Pool::new(
        std::rc::Rc::new(reactor.clone()),
        std::rc::Rc::new(Caller(admission.clone())),
        PoolConfig {
            per_endpoint: 4,
            secondary_cap: 4,
            max_endpoints: 8,
            waiter_cap: 8,
            idle_timeout: Duration::from_secs(30),
            tcp_nodelay: true,
        },
    );
    let io = Io::new(
        std::rc::Rc::new(reactor.clone()),
        Codec::new(16 * 1024),
        std::rc::Rc::new(Caller(admission.clone())),
        (PAGE * 2) as u64,
        1 << 20,
    );
    Fixture {
        reactor,
        pool,
        io,
        admission,
    }
}
fn drive<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let until = Instant::now() + Duration::from_secs(120);
    let mut cx = Context::from_waker(Waker::noop());
    loop {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        assert!(Instant::now() < until, "fixture watchdog");
        reactor.poll_budgeted(64).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

/// Deterministic body byte for an absolute object offset.
fn pattern(offset: usize) -> u8 {
    (offset.wrapping_mul(31) ^ (offset >> 13)) as u8
}

struct Identity {
    roots: rustls::RootCertStore,
    chain: Vec<rustls::pki_types::CertificateDer<'static>>,
    key: rustls::pki_types::PrivateKeyDer<'static>,
}
fn identity(names: &[&str]) -> Identity {
    let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
    let ca = ca_params.self_signed(&ca_key).unwrap();
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
    let params =
        rcgen::CertificateParams::new(names.iter().map(|n| n.to_string()).collect::<Vec<_>>())
            .unwrap();
    let cert = params.signed_by(&key, &ca, &ca_key).unwrap();
    let pem = ca.pem();
    Identity {
        roots: http1::tls::trust_store_from_pem(pem.as_bytes()).unwrap(),
        chain: vec![cert.der().clone()],
        key: rustls::pki_types::PrivateKeyDer::Pkcs8(key.serialize_der().into()),
    }
}

/// Serve ranged GETs over TLS (or plaintext) with keep-alive. Each request's
/// `Range: bytes=a-b` selects the body. Returns the accept count.
fn serve(
    listener: TcpListener,
    identity: Option<&Identity>,
    tls12: bool,
    connections: usize,
) -> (std::thread::JoinHandle<()>, Arc<AtomicUsize>) {
    let config = identity.map(|identity| {
        let versions: &[&rustls::SupportedProtocolVersion] = if tls12 {
            &[&rustls::version::TLS12]
        } else {
            &[&rustls::version::TLS13]
        };
        Arc::new(
            rustls::ServerConfig::builder_with_protocol_versions(versions)
                .with_no_client_auth()
                .with_single_cert(identity.chain.clone(), identity.key.clone_key())
                .unwrap(),
        )
    });
    let accepted = Arc::new(AtomicUsize::new(0));
    let counter = accepted.clone();
    let handle = std::thread::spawn(move || {
        let body: Vec<u8> = (0..PAGE * 2).map(pattern).collect();
        for _ in 0..connections {
            let Ok((tcp, _)) = listener.accept() else {
                break;
            };
            counter.fetch_add(1, Ordering::SeqCst);
            tcp.set_nodelay(true).unwrap();
            match &config {
                Some(config) => {
                    let session = rustls::ServerConnection::new(config.clone()).unwrap();
                    serve_connection(rustls::StreamOwned::new(session, tcp), &body);
                }
                None => serve_connection(tcp, &body),
            }
        }
    });
    (handle, accepted)
}
fn serve_connection(mut stream: impl Read + Write, body: &[u8]) {
    loop {
        let mut head = Vec::new();
        let mut byte = [0];
        while !head.ends_with(b"\r\n\r\n") {
            match stream.read(&mut byte) {
                Ok(1) => head.push(byte[0]),
                _ => return,
            }
        }
        let text = String::from_utf8(head).unwrap();
        let range = text
            .lines()
            .find_map(|l| l.strip_prefix("Range: bytes="))
            .map(|r| {
                let (a, b) = r.split_once('-').unwrap();
                a.parse::<usize>().unwrap()..b.parse::<usize>().unwrap() + 1
            })
            .unwrap_or(0..body.len());
        let is_head = text.starts_with("HEAD ");
        let response = format!(
            "HTTP/1.1 206 Partial Content\r\nContent-Length: {}\r\nContent-Range: bytes {}-{}/{}\r\nETag: \"0x1\"\r\nx-ms-blob-type: BlockBlob\r\n\r\n",
            range.len(),
            range.start,
            range.end - 1,
            body.len()
        );
        if stream.write_all(response.as_bytes()).is_err() {
            return;
        }
        if !is_head && stream.write_all(&body[range]).is_err() {
            return;
        }
        let _ = stream.flush();
    }
}

fn get(range: std::ops::Range<usize>, host: &str) -> MessageHead {
    MessageHead {
        start: StartLine::Request {
            method: "GET".into(),
            target: "/container/blob".into(),
        },
        headers: vec![
            Header::new("Host", host),
            Header::new("x-ms-version", "2021-08-06"),
            Header::new("Authorization", "Bearer token"),
            Header::new("Range", format!("bytes={}-{}", range.start, range.end - 1)),
        ],
    }
}

/// Run `count` ranged 16 MiB GETs on the pool, verifying each body.
fn fetch_pages(fixture: &Fixture, endpoint: &Endpoint, count: usize) -> Result<bool> {
    fetch(fixture, endpoint, count, true)
}
fn fetch(fixture: &Fixture, endpoint: &Endpoint, count: usize, verify: bool) -> Result<bool> {
    let scope = scope();
    let mut kernel = false;
    for index in 0..count {
        let offset = (index * 4096) % PAGE;
        let range = offset..offset + PAGE;
        let page = drive(&fixture.reactor, async {
            let connection = fixture.pool.checkout(endpoint, &scope).await?;
            kernel = connection.is_kernel_tls();
            let sent = fixture
                .io
                .send_head(connection, get(range.clone(), "localhost"), &scope)
                .await?;
            let head = fixture.io.receive_head(sent.connection, &scope).await?;
            assert!(matches!(
                head.value.start,
                StartLine::Response { status: 206 }
            ));
            assert_eq!(head.value.unique("etag")?, Some(&b"\"0x1\""[..]));
            assert_eq!(
                head.value.unique("x-ms-blob-type")?,
                Some(&b"BlockBlob"[..])
            );
            assert_eq!(head.value.content_length()?, Some(PAGE as u64));
            let buffer = fixture.io.buffer(PAGE)?;
            let read = fixture
                .io
                .read_body_exact(head.connection, buffer, &scope)
                .await?;
            let mut connection = read.lease;
            connection.finish_exchange()?;
            Ok::<_, Error>(read.buffer)
        })?;
        let bytes = page.bytes()?;
        assert!(
            !verify
                || bytes
                    .iter()
                    .enumerate()
                    .all(|(i, b)| *b == pattern(offset + i)),
            "page {index} content"
        );
    }
    Ok(kernel)
}

fn tls_endpoint(identity: &Identity, address: SocketAddr, mode: TlsMode) -> Endpoint {
    let config = http1::tls::client_config(identity.roots.clone());
    Endpoint {
        address,
        tls: Some(TlsClient::new(config, "localhost").unwrap().with_mode(mode)),
    }
}

fn kernel_tls_available() -> bool {
    if http1::tls::kernel_tls_loaded() {
        return true;
    }
    eprintln!("skipping: kernel tls module is not loaded");
    false
}

fn keep_alive_pages(mode: TlsMode, tls12: bool) {
    let identity = identity(&["localhost"]);
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let (server, accepted) = serve(listener, Some(&identity), tls12, 1);
    let fixture = fixture();
    let endpoint = tls_endpoint(&identity, address, mode);
    let kernel = fetch_pages(&fixture, &endpoint, 3).unwrap();
    assert_eq!(kernel, mode == TlsMode::Kernel);
    assert_eq!(accepted.load(Ordering::SeqCst), 1, "keep-alive reuse");
    fixture.pool.close();
    drop(fixture);
    server.join().unwrap();
}

#[test]
fn kernel_tls13_keep_alive_range_pages() {
    if kernel_tls_available() {
        keep_alive_pages(TlsMode::Kernel, false);
        let stats = http1::tls::stats();
        assert!(stats.kernel >= 1);
        // rustls servers send TLS 1.3 tickets after the handshake; kernel TLS
        // must surface and discard them rather than failing the receive.
        assert!(stats.discarded_tickets >= 1);
    }
}

#[test]
fn kernel_tls12_keep_alive_range_pages() {
    if kernel_tls_available() {
        keep_alive_pages(TlsMode::Kernel, true);
    }
}

#[test]
fn userspace_tls13_keep_alive_range_pages() {
    keep_alive_pages(TlsMode::Userspace, false);
    assert!(http1::tls::stats().userspace >= 1);
}

#[test]
fn userspace_tls12_keep_alive_range_pages() {
    keep_alive_pages(TlsMode::Userspace, true);
}

#[test]
fn plaintext_keep_alive_range_pages() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let (server, accepted) = serve(listener, None, false, 1);
    let fixture = fixture();
    let endpoint = Endpoint { address, tls: None };
    assert!(!fetch_pages(&fixture, &endpoint, 2).unwrap());
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    fixture.pool.close();
    drop(fixture);
    server.join().unwrap();
}

#[test]
fn untrusted_certificate_fails_handshake() {
    let identity = identity(&["localhost"]);
    let other = self::identity(&["localhost"]);
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let (server, _) = serve(listener, Some(&identity), false, 1);
    let fixture = fixture();
    for mode in [TlsMode::Userspace, TlsMode::Auto] {
        let endpoint = tls_endpoint(&other, address, mode);
        let scope = scope();
        let result = drive(&fixture.reactor, fixture.pool.checkout(&endpoint, &scope));
        assert!(matches!(
            result.err(),
            Some(Error::Runtime(racer_io::Error::Os(libc::EPROTO)))
        ));
        if mode == TlsMode::Userspace {
            break;
        }
    }
    drop(fixture);
    server.join().unwrap();
}

#[test]
fn wrong_server_name_fails_handshake() {
    let identity = identity(&["origin.example"]);
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let (server, _) = serve(listener, Some(&identity), false, 1);
    let fixture = fixture();
    let endpoint = tls_endpoint(&identity, address, TlsMode::Userspace);
    let scope = scope();
    let result = drive(&fixture.reactor, fixture.pool.checkout(&endpoint, &scope));
    assert!(result.is_err());
    drop(fixture);
    server.join().unwrap();
}

/// Auto mode reconnects with userspace TLS when the kernel rejects the
/// negotiated keys, and later connections to that endpoint skip kernel TLS.
#[cfg(feature = "test-util")]
#[test]
fn auto_falls_back_to_userspace_when_key_install_fails() {
    if !kernel_tls_available() {
        return;
    }
    let identity = identity(&["localhost"]);
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let (server, accepted) = serve(listener, Some(&identity), false, 3);
    let fixture = fixture();
    let endpoint = tls_endpoint(&identity, address, TlsMode::Auto);
    http1::tls::fail_next_key_installs(1);
    let kernel = fetch(&fixture, &endpoint, 1, true).unwrap();
    assert!(!kernel, "fallback connection uses userspace TLS");
    assert_eq!(accepted.load(Ordering::SeqCst), 2, "one fresh reconnect");
    // A new connection for the same endpoint goes straight to userspace.
    fixture.pool.close();
    drop(fixture);
    let fixture = self::fixture();
    http1::tls::fail_next_key_installs(1);
    assert!(!fetch(&fixture, &endpoint, 1, true).unwrap());
    assert_eq!(
        accepted.load(Ordering::SeqCst),
        3,
        "no reconnect once learned"
    );
    fixture.pool.close();
    drop(fixture);
    http1::tls::fail_next_key_installs(0);
    server.join().unwrap();
}

/// Kernel mode keeps failing hard when key installation fails.
#[cfg(feature = "test-util")]
#[test]
fn kernel_mode_fails_when_key_install_fails() {
    if !kernel_tls_available() {
        return;
    }
    let identity = identity(&["localhost"]);
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let (server, accepted) = serve(listener, Some(&identity), false, 1);
    let fixture = fixture();
    let endpoint = tls_endpoint(&identity, address, TlsMode::Kernel);
    http1::tls::fail_next_key_installs(1);
    let scope = scope();
    let result = drive(&fixture.reactor, fixture.pool.checkout(&endpoint, &scope));
    assert!(matches!(
        result.err(),
        Some(Error::Runtime(racer_io::Error::Unavailable))
    ));
    assert_eq!(accepted.load(Ordering::SeqCst), 1);
    http1::tls::fail_next_key_installs(0);
    drop(fixture);
    server.join().unwrap();
}

/// Hold an accepted socket without answering until the peer closes it.
/// Returns true when the client closed the socket (EOF or reset) and false if
/// it stayed open past the watchdog.
fn stall_until_closed(mut tcp: std::net::TcpStream) -> bool {
    tcp.set_read_timeout(Some(Duration::from_secs(30))).unwrap();
    let mut buffer = [0u8; 4096];
    loop {
        match tcp.read(&mut buffer) {
            Ok(0) => return true,
            Ok(_) => {}
            Err(e) if e.kind() == std::io::ErrorKind::ConnectionReset => return true,
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
            Err(_) => return false,
        }
    }
}

/// Poll `future` until `ready` holds, so it is parked mid-handshake.
fn drive_until(
    reactor: &Reactor,
    future: &mut std::pin::Pin<&mut impl Future>,
    ready: impl Fn() -> bool,
) {
    let until = Instant::now() + Duration::from_secs(30);
    let mut cx = Context::from_waker(Waker::noop());
    while !ready() {
        assert!(
            future.as_mut().poll(&mut cx).is_pending(),
            "checkout must stay pending against a stalled server"
        );
        assert!(Instant::now() < until, "fixture watchdog");
        reactor.poll_budgeted(64).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    // Let the client flush what it can (ClientHello) before abandonment.
    for _ in 0..10 {
        assert!(future.as_mut().poll(&mut cx).is_pending());
        reactor.poll_budgeted(64).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

/// After abandonment the slot, admission, and pool entry are released and
/// nothing unfinished was parked in the idle pool.
fn assert_released(fixture: &Fixture, endpoint: &Endpoint) {
    for (key, (active, idle)) in fixture.pool.snapshot().entries {
        if key == *endpoint {
            assert_eq!((active, idle), (0, 0), "no active or idle connection");
        }
    }
    assert_eq!(
        fixture.admission.used(Resource::Connection),
        0,
        "slot released"
    );
    assert_eq!(fixture.reactor.in_flight(), 0);
}

/// Dropping a checkout whose TLS handshake is stalled releases its slot and
/// closes the socket without pooling the unfinished connection.
#[test]
fn abandoned_tls_handshake_releases_connection() {
    let identity = identity(&["localhost"]);
    for mode in [TlsMode::Userspace, TlsMode::Auto] {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let accepted = Arc::new(AtomicUsize::new(0));
        let counter = accepted.clone();
        let server = std::thread::spawn(move || {
            let (tcp, _) = listener.accept().unwrap();
            counter.fetch_add(1, Ordering::SeqCst);
            stall_until_closed(tcp)
        });
        let fixture = fixture();
        let endpoint = tls_endpoint(&identity, address, mode);
        let scope = scope();
        {
            let checkout = fixture.pool.checkout(&endpoint, &scope);
            let mut checkout = std::pin::pin!(checkout);
            drive_until(&fixture.reactor, &mut checkout, || {
                accepted.load(Ordering::SeqCst) == 1
            });
        }
        drive(&fixture.reactor, fixture.reactor.drain()).unwrap();
        assert_released(&fixture, &endpoint);
        assert!(server.join().unwrap(), "client socket closed ({mode:?})");
        assert_eq!(fixture.admission.used(Resource::Context), 0);
    }
}

/// Cancelling a checkout while the userspace reconnect after a failed kernel
/// key install is stalled releases both sockets and the slot.
#[cfg(feature = "test-util")]
#[test]
fn cancelled_fallback_reconnect_releases_connection() {
    if !kernel_tls_available() {
        return;
    }
    let identity = identity(&["localhost"]);
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let config = Arc::new(
        rustls::ServerConfig::builder_with_protocol_versions(&[&rustls::version::TLS13])
            .with_no_client_auth()
            .with_single_cert(identity.chain.clone(), identity.key.clone_key())
            .unwrap(),
    );
    let accepted = Arc::new(AtomicUsize::new(0));
    let counter = accepted.clone();
    let server = std::thread::spawn(move || {
        // First connection: complete the handshake so the client reaches key
        // installation, then wait for it to drop the socket.
        let (mut tcp, _) = listener.accept().unwrap();
        counter.fetch_add(1, Ordering::SeqCst);
        let mut session = rustls::ServerConnection::new(config).unwrap();
        while session.is_handshaking() {
            session.complete_io(&mut tcp).unwrap();
        }
        while session.wants_write() {
            session.write_tls(&mut tcp).unwrap();
        }
        let first = stall_until_closed(tcp);
        // Second connection: the userspace reconnect, stalled.
        let (tcp, _) = listener.accept().unwrap();
        counter.fetch_add(1, Ordering::SeqCst);
        (first, stall_until_closed(tcp))
    });
    let fixture = fixture();
    let endpoint = tls_endpoint(&identity, address, TlsMode::Auto);
    let scope = scope();
    http1::tls::fail_next_key_installs(1);
    let result = {
        let checkout = fixture.pool.checkout(&endpoint, &scope);
        let mut checkout = std::pin::pin!(checkout);
        drive_until(&fixture.reactor, &mut checkout, || {
            accepted.load(Ordering::SeqCst) == 2
        });
        scope.cancellation.cancel().unwrap();
        drive(&fixture.reactor, checkout)
    };
    http1::tls::fail_next_key_installs(0);
    assert!(matches!(
        result.err(),
        Some(Error::Runtime(racer_io::Error::Cancelled))
    ));
    drive(&fixture.reactor, fixture.reactor.drain()).unwrap();
    assert_released(&fixture, &endpoint);
    assert_eq!(server.join().unwrap(), (true, true), "both sockets closed");
    assert_eq!(fixture.admission.used(Resource::Context), 0);
}

/// A budget that admits the handshake but not the relay buffers rejects the
/// userspace connect and leaks neither the charge nor the slot.
#[test]
fn userspace_relay_over_budget_is_rejected() {
    let identity = identity(&["localhost"]);
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let (server, accepted) = serve(listener, Some(&identity), false, 1);
    let fixture = fixture_with(http1::tls::RELAY_ALLOCATION - 1);
    let endpoint = tls_endpoint(&identity, address, TlsMode::Userspace);
    let scope = scope();
    let result = drive(&fixture.reactor, fixture.pool.checkout(&endpoint, &scope));
    assert!(matches!(result.err(), Some(Error::Quota(_))));
    assert_eq!(accepted.load(Ordering::SeqCst), 1, "handshake was admitted");
    drive(&fixture.reactor, fixture.reactor.drain()).unwrap();
    assert_released(&fixture, &endpoint);
    assert_eq!(fixture.admission.used(Resource::Context), 0);
    drop(fixture);
    server.join().unwrap();
}

/// The relay charge stays held while the connection idles in the pool and is
/// released once the pool closes it and the relay thread exits.
#[test]
fn userspace_relay_charge_spans_idle_connection() {
    let identity = identity(&["localhost"]);
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let (server, accepted) = serve(listener, Some(&identity), false, 1);
    let fixture = fixture();
    let endpoint = tls_endpoint(&identity, address, TlsMode::Userspace);
    assert!(!fetch(&fixture, &endpoint, 2, false).unwrap());
    assert_eq!(accepted.load(Ordering::SeqCst), 1, "keep-alive reuse");
    assert!(
        fixture
            .pool
            .snapshot()
            .entries
            .iter()
            .any(|(key, counts)| *key == endpoint && *counts == (0, 1))
    );
    assert!(fixture.admission.used(Resource::Context) >= http1::tls::RELAY_ALLOCATION);
    fixture.pool.close();
    // Only the I/O helper's cached head buffer may remain charged.
    assert!(fixture.admission.used(Resource::Context) < http1::tls::RELAY_ALLOCATION);
    drop(fixture);
    server.join().unwrap();
}

/// Cancelling an exchange on a userspace connection releases the relay charge
/// once the connection is dropped.
#[test]
fn cancelled_userspace_exchange_releases_relay_charge() {
    let identity = identity(&["localhost"]);
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let (server, _) = serve(listener, Some(&identity), false, 1);
    let fixture = fixture();
    let endpoint = tls_endpoint(&identity, address, TlsMode::Userspace);
    let scope = scope();
    let connection = drive(&fixture.reactor, fixture.pool.checkout(&endpoint, &scope)).unwrap();
    assert!(fixture.admission.used(Resource::Context) >= http1::tls::RELAY_ALLOCATION);
    // No request was sent, so the head receive stalls until cancelled.
    let result = {
        let receive = fixture.io.receive_head(connection, &scope);
        let mut receive = std::pin::pin!(receive);
        let mut cx = Context::from_waker(Waker::noop());
        for _ in 0..10 {
            assert!(receive.as_mut().poll(&mut cx).is_pending());
            fixture.reactor.poll_budgeted(64).unwrap();
            fixture.reactor.wait(Duration::from_millis(1)).unwrap();
        }
        scope.cancellation.cancel().unwrap();
        drive(&fixture.reactor, receive)
    };
    assert!(matches!(
        result.err(),
        Some(Error::Runtime(racer_io::Error::Cancelled))
    ));
    drive(&fixture.reactor, fixture.reactor.drain()).unwrap();
    assert_released(&fixture, &endpoint);
    // Only the I/O helper's cached head buffer may remain charged.
    assert!(fixture.admission.used(Resource::Context) < http1::tls::RELAY_ALLOCATION);
    drop(fixture);
    server.join().unwrap();
}
