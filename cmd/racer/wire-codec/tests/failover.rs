//! Discovered-seat failover after a seat accepts TLS but answers badly.
//!
//! Connect failures are covered in `transport.rs`. These tests cover a live
//! seat that returns an error status, a stale 200 seen by the caller, and the
//! rule that no request bytes reach a seat whose certificate does not match.

use std::{
    io::{Read, Write},
    rc::Rc,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU16, AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};
use wire_codec::rest::{DiscoveredEndpoint, Method, Transport};

#[path = "support/mod.rs"]
/// Loopback I/O, identities, and scopes shared with the other suites.
#[allow(dead_code)]
mod testing;

use testing::{FixtureIo, TestIdentity, TestTransport as _};

/// One seat: a TLS server with a settable status that records each request head.
struct Seat {
    port: u16,

    certificate: Vec<u8>,

    status: Arc<AtomicU16>,

    requests: Arc<AtomicUsize>,

    heads: Arc<Mutex<Vec<String>>>,

    stop: Arc<AtomicBool>,

    thread: Option<std::thread::JoinHandle<()>>,
}

impl Seat {
    /// Serve `name` in each body. A `wrong_name` certificate fails hostname checks.
    fn new(name: &'static str, status: u16) -> Self {
        let mut params = rcgen::CertificateParams::new(vec!["127.0.0.1".into()]).unwrap();
        params.is_ca = rcgen::IsCa::ExplicitNoCa;
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
        let cert = params.self_signed(&key).unwrap();
        let config = Arc::new(
            rustls::ServerConfig::builder_with_provider(Arc::new(
                rustls::crypto::ring::default_provider(),
            ))
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_client_cert_verifier(Arc::new(AnyClient))
            .with_single_cert(
                vec![cert.der().clone()],
                rustls::pki_types::PrivatePkcs8KeyDer::from(key.serialize_der()).into(),
            )
            .unwrap(),
        );
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let port = listener.local_addr().unwrap().port();
        let status = Arc::new(AtomicU16::new(status));
        let requests = Arc::new(AtomicUsize::new(0));
        let heads = Arc::new(Mutex::new(Vec::new()));
        let stop = Arc::new(AtomicBool::new(false));
        let (s, r, h, x) = (
            status.clone(),
            requests.clone(),
            heads.clone(),
            stop.clone(),
        );
        let thread = std::thread::spawn(move || {
            while !x.load(Ordering::SeqCst) {
                let socket = match listener.accept() {
                    Ok((socket, _)) => socket,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        std::thread::sleep(Duration::from_millis(1));
                        continue;
                    }
                    Err(e) => panic!("seat accept: {e}"),
                };
                socket.set_nonblocking(false).unwrap();
                socket
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                socket
                    .set_write_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                let tls = rustls::ServerConnection::new(config.clone()).unwrap();
                let mut stream = rustls::StreamOwned::new(tls, socket);
                'connection: while !x.load(Ordering::SeqCst) {
                    let deadline = Instant::now() + Duration::from_secs(5);
                    let mut head = Vec::new();
                    while !head.ends_with(b"\r\n\r\n") {
                        let mut byte = [0];
                        if Instant::now() >= deadline
                            || head.len() >= 16384
                            || stream.read_exact(&mut byte).is_err()
                        {
                            break 'connection;
                        }
                        head.push(byte[0]);
                    }
                    r.fetch_add(1, Ordering::SeqCst);
                    h.lock()
                        .unwrap()
                        .push(String::from_utf8_lossy(&head).into_owned());
                    let status = s.load(Ordering::SeqCst);
                    let body = if status == 200 {
                        format!("{{\"seat\":\"{name}\"}}")
                    } else {
                        "{\"code\":\"Unavailable\"}".to_owned()
                    };
                    let reason = if status == 200 { "OK" } else { "Error" };
                    let response = format!(
                        "HTTP/1.1 {status} {reason}\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
                        body.len()
                    );
                    if stream.write_all(response.as_bytes()).is_err() || stream.flush().is_err() {
                        break;
                    }
                }
            }
        });
        Self {
            port,
            certificate: cert.der().to_vec(),
            status,
            requests,
            heads,
            stop,
            thread: Some(thread),
        }
    }

    fn endpoint(&self, pin: &[u8]) -> DiscoveredEndpoint {
        DiscoveredEndpoint {
            url: format!("https://127.0.0.1:{}", self.port),
            certificate: pin.to_vec(),
            generation: "g".into(),
        }
    }

    fn pinned(&self) -> DiscoveredEndpoint {
        self.endpoint(&self.certificate)
    }

    fn requests(&self) -> usize {
        self.requests.load(Ordering::SeqCst)
    }
}

impl Drop for Seat {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        let _ = self.thread.take().unwrap().join();
    }
}

/// Accept any client certificate; identity policy is not under test here.
#[derive(Debug)]
struct AnyClient;

impl rustls::server::danger::ClientCertVerifier for AnyClient {
    fn offer_client_auth(&self) -> bool {
        true
    }

    fn client_auth_mandatory(&self) -> bool {
        false
    }

    fn root_hint_subjects(&self) -> &[rustls::DistinguishedName] {
        &[]
    }

    fn verify_client_cert(
        &self,
        _: &rustls::pki_types::CertificateDer<'_>,
        _: &[rustls::pki_types::CertificateDer<'_>],
        _: rustls::pki_types::UnixTime,
    ) -> Result<rustls::server::danger::ClientCertVerified, rustls::Error> {
        Ok(rustls::server::danger::ClientCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        _: &[u8],
        _: &rustls::pki_types::CertificateDer<'_>,
        _: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        Ok(rustls::client::danger::HandshakeSignatureValid::assertion())
    }

    fn verify_tls13_signature(
        &self,
        _: &[u8],
        _: &rustls::pki_types::CertificateDer<'_>,
        _: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        Ok(rustls::client::danger::HandshakeSignatureValid::assertion())
    }

    fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
        rustls::crypto::ring::default_provider()
            .signature_verification_algorithms
            .supported_schemes()
    }
}

fn transport(endpoints: Vec<DiscoveredEndpoint>) -> Transport<FixtureIo> {
    let transport = Transport::new(testing::config(
        "https://static.invalid".into(),
        "/nonexistent/trust.pem".into(),
    ));
    transport.attach_io(Rc::new(FixtureIo));
    transport.set_endpoints(Arc::new(std::sync::RwLock::new(endpoints)));
    transport
}

/// Send one GET and return its status and body.
fn get(
    transport: &Transport<FixtureIo>,
    identity: Option<&TestIdentity>,
    bearer: Option<&str>,
) -> testing::Result<(u16, Vec<u8>)> {
    let scope = testing::scope();
    futures::executor::block_on(async {
        let connection = match identity {
            Some(identity) => transport.authenticated(identity, &scope).await?,
            None => transport.bootstrap(&scope).await?,
        };
        let response = connection
            .request(
                testing::request(Method::Get, "/watch", bearer, 1024),
                &scope,
            )
            .await?;
        Ok((response.status, response.body.clone()))
    })
}

/// A live seat that answers 503 is left after one response, and the healthy
/// seat stays preferred, for both pooled identity and bearer connections.
#[test]
fn live_seat_error_status_fails_over_and_stays_away() {
    let (ca, ca_key) = testing::ca();
    let identity = TestIdentity::new(&ca, &ca_key);
    for identity in [None, Some(&identity)] {
        let bad = Seat::new("bad", 503);
        let good = Seat::new("good", 200);
        let transport = transport(vec![bad.pinned(), good.pinned()]);
        let mut previous = 0;
        for _ in 0..12 {
            let (status, body) = get(&transport, identity, None).unwrap();
            if previous == 503 {
                assert_eq!(status, 200, "a failed seat must not be retried first");
            }
            if status == 200 {
                assert_eq!(body, b"{\"seat\":\"good\"}");
            }
            previous = status;
        }
        assert!(bad.requests() <= 1, "bad seat hit {} times", bad.requests());
        assert!(good.requests() >= 11);
    }
}

/// 401 and 403 from a live seat also fail over, as a stale leader may reject
/// a node certificate issued by its successor.
#[test]
fn live_seat_auth_rejection_fails_over() {
    for status in [401, 403] {
        let bad = Seat::new("bad", status);
        let good = Seat::new("good", 200);
        let transport = transport(vec![bad.pinned(), good.pinned()]);
        for _ in 0..6 {
            let _ = get(&transport, None, None).unwrap();
        }
        assert!(bad.requests() <= 1);
        assert_eq!(get(&transport, None, None).unwrap().0, 200);
    }
}

/// A seat that becomes bad after being preferred is left on its next error,
/// and an explicit demote after a stale 200 moves the next request away.
#[test]
fn preferred_seat_is_demoted_by_status_and_by_caller() {
    let (ca, ca_key) = testing::ca();
    let identity = TestIdentity::new(&ca, &ca_key);
    let a = Seat::new("a", 200);
    let b = Seat::new("b", 200);
    let transport = transport(vec![a.pinned(), b.pinned()]);
    let first = get(&transport, Some(&identity), None).unwrap().1;
    for _ in 0..4 {
        assert_eq!(get(&transport, Some(&identity), None).unwrap().1, first);
    }
    let (current, other) = if first == b"{\"seat\":\"a\"}" {
        (&a, &b)
    } else {
        (&b, &a)
    };

    // Stale 200: the caller sees an old cursor and demotes the seat.
    transport.demote();
    let before = other.requests();
    let moved = get(&transport, Some(&identity), None).unwrap().1;
    assert_ne!(moved, first);
    assert_eq!(other.requests(), before + 1);

    // The new seat turns into a stale follower answering 503.
    other.status.store(503, Ordering::SeqCst);
    assert_eq!(get(&transport, Some(&identity), None).unwrap().0, 503);
    let (status, body) = get(&transport, Some(&identity), None).unwrap();
    assert_eq!((status, body), (200, first));
    assert!(current.requests() >= 6);
}

/// A bearer token reaches only a seat whose certificate matches its pin.
#[test]
fn bearer_is_sent_only_after_pin_validation() {
    let impostor = Seat::new("impostor", 200);
    let good = Seat::new("good", 200);
    let transport = transport(vec![impostor.endpoint(&good.certificate), good.pinned()]);
    for _ in 0..6 {
        assert_eq!(get(&transport, None, Some("secret-token")).unwrap().0, 200);
    }
    assert_eq!(impostor.requests(), 0);
    assert!(impostor.heads.lock().unwrap().is_empty());
    let heads = good.heads.lock().unwrap();
    assert_eq!(heads.len(), 6);
    assert!(
        heads
            .iter()
            .all(|h| h.contains("Authorization: Bearer secret-token"))
    );
}

/// Demote without discovered endpoints only drops the idle connection.
#[test]
fn demote_without_discovery_is_harmless() {
    let transport = Transport::<FixtureIo>::new(testing::config(
        "https://static.invalid".into(),
        "/nonexistent/trust.pem".into(),
    ));
    transport.demote();
    transport.demote();
}
