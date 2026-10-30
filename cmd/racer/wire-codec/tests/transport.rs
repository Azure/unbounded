//! Socket-level transport contracts exercised through the public client API.
//!
//! This cohesive integration suite intentionally stays below 1000 lines. Its
//! loopback fixtures are shared with private library tests rather than duplicated
//! or exposed through the production API merely to meet a file-size target.

use std::{
    io::{Read, Write},
    rc::Rc,
    sync::Arc,
    time::Duration,
};
use wire_codec::rest::{
    DiscoveredEndpoint, DiscoveredEndpoints, Error as RestError, Method, Operation, Transport,
};

#[path = "support/mod.rs"]
/// Loopback I/O, identities, and join-on-drop servers shared with private tests.
mod testing;

use testing::{Error, RotationServer, TestTransport as _, WeeklyCertificates, rotation_request};
use testing::{FixtureIo, TestIdentity};

/// Concrete transport used by loopback fixtures.
type ControlTransport = Transport<FixtureIo>;

/// Full-publication path used by fixture requests.
const SNAPSHOT_PATH: &str = "/snapshot";

/// Enrollment path used by bootstrap fixture requests.
const BOOTSTRAP_PATH: &str = "/enroll";

/// Bootstrap and mutual authentication both deliver bounded chunked JSON.
#[test]
fn real_server_auth_and_mutual_tls_chunked_response() {
    tls_fixture(false);
}

/// Cross-signed serving chains accept exactly the retained trust anchors.
#[test]
fn weekly_cross_signed_chains_accept_retained_anchors_only() {
    let certs = WeeklyCertificates::new();
    let d = testing::Directory::new();
    let trust = d.0.join("trust.pem");
    let server = RotationServer::new(certs.servers[1].clone());
    let transport = server.transport(trust.clone());
    let (unrelated, _) = testing::ca();
    for generation in 1..4 {
        server.rotate(certs.servers[generation].clone());
        let first_retained = generation.saturating_sub(2);
        let overlap = certs.roots[first_retained..=generation]
            .iter()
            .rev()
            .map(|c| c.pem())
            .collect::<String>();
        for (label, bundle, accepted) in [
            ("old-only", certs.roots[generation - 1].pem(), true),
            ("current-only", certs.roots[generation].pem(), true),
            ("overlap", overlap, true),
            ("oldest-only", certs.roots[0].pem(), generation <= 2),
            (
                "new-cross-anchor",
                certs.crosses[generation - 1].pem(),
                true,
            ),
            ("unrelated", unrelated.pem(), false),
        ] {
            std::fs::write(&trust, bundle).unwrap();
            let result = rotation_request(&transport, None);
            if accepted {
                assert!(result.is_ok(), "week {generation}, {label}: {result:?}");
            } else {
                assert!(
                    matches!(result, Err(Error::Transport(RestError::Unauthorized))),
                    "week {generation}, {label}: {result:?}"
                );
            }
        }
    }
    // Week two: leaf -> CA2-by-CA1 -> CA1-by-CA0. A public
    // intermediate is also a valid anchor, without its self-signed root.
    server.rotate(certs.servers[2].clone());
    std::fs::write(&trust, certs.crosses[0].pem()).unwrap();
    assert!(rotation_request(&transport, None).is_ok());
    // Removing the compatibility bridges must break old-only trust, but
    // the exact same leaf still verifies directly under the current root.
    server.rotate(certs.leaf_only_servers[2].clone());
    std::fs::write(&trust, certs.roots[0].pem()).unwrap();
    assert!(matches!(
        rotation_request(&transport, None),
        Err(Error::Transport(RestError::Unauthorized))
    ));
    std::fs::write(&trust, certs.roots[2].pem()).unwrap();
    assert!(rotation_request(&transport, None).is_ok());
}

/// Trailing plaintext is rejected even when it remains in the TLS reader.
#[test]
fn rejects_trailing_tls_plaintext_before_connection_reuse() {
    tls_fixture(true);
}

/// Exercise bootstrap and mutual TLS, with optional plaintext past the frame.
fn tls_fixture(trailing: bool) {
    let d = testing::Directory::new();
    let (ca, ca_key) = testing::ca();
    let mut params =
        rcgen::CertificateParams::new(vec!["localhost".into(), "127.0.0.1".into()]).unwrap();
    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
    let server_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let cert = params.signed_by(&server_key, &ca, &ca_key).unwrap();
    let trust = d.0.join("trust.pem");
    std::fs::write(&trust, ca.pem()).unwrap();
    let identity = TestIdentity::new(&ca, &ca_key);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    let mut roots = rustls::RootCertStore::empty();
    roots.add(ca.der().clone()).unwrap();
    let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(roots),
        Arc::new(rustls::crypto::ring::default_provider()),
    )
    .allow_unauthenticated()
    .build()
    .unwrap();
    let config = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_client_cert_verifier(verifier)
    .with_single_cert(
        vec![cert.der().clone()],
        rustls::pki_types::PrivatePkcs8KeyDer::from(server_key.serialize_der()).into(),
    )
    .unwrap();
    let server = std::thread::spawn(move || {
        for mutual in [false, true] {
            let (socket, _) = listener.accept().unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            let tls = rustls::ServerConnection::new(Arc::new(config.clone())).unwrap();
            let mut stream = rustls::StreamOwned::new(tls, socket);
            let mut request = Vec::new();
            let mut b = [0; 1];
            while !request.ends_with(b"\r\n\r\n") {
                stream.read_exact(&mut b).unwrap();
                request.push(b[0]);
            }
            assert_eq!(
                stream.conn.protocol_version(),
                Some(rustls::ProtocolVersion::TLSv1_3)
            );
            assert_eq!(stream.conn.peer_certificates().is_some(), mutual);
            if !mutual {
                assert!(
                    String::from_utf8(request)
                        .unwrap()
                        .contains("Authorization: Bearer fixture.token")
                );
            }
            if trailing {
                // The framed response ends exactly at receive's 16 KiB
                // boundary, leaving the extra byte in rustls's reader.
                let head = b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 16310\r\n\r\n";
                let mut response = head.to_vec();
                response.resize(head.len() + 16310, b' ');
                response.push(b'x');
                stream.write_all(&response).unwrap();
            } else {
                stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n2\r\n{}\r\n0\r\n\r\n").unwrap();
            }
            stream.flush().unwrap();
        }
    });
    let transport =
        ControlTransport::new(testing::config(format!("https://127.0.0.1:{port}"), trust));
    transport.attach_io(Rc::new(FixtureIo));
    let scope = testing::scope();
    let run = |future: Operation<'_, wire_codec::rest::Response, Error>| {
        futures::executor::block_on(future)
    };
    let first = run(Box::pin(async {
        let connection = transport.bootstrap(&scope).await?;
        connection
            .request(
                testing::request(Method::Post, BOOTSTRAP_PATH, Some("fixture.token"), 65536),
                &scope,
            )
            .await
    }));
    if trailing {
        assert!(matches!(
            first,
            Err(Error::Transport(RestError::InvalidRequest))
        ));
    } else {
        assert_eq!(first.unwrap().body, b"{}");
    }
    let second = run(Box::pin(async {
        let connection = transport.authenticated(&identity, &scope).await?;
        connection
            .request(
                testing::request(Method::Get, SNAPSHOT_PATH, None, 65536),
                &scope,
            )
            .await
    }));
    if trailing {
        assert!(matches!(
            second,
            Err(Error::Transport(RestError::InvalidRequest))
        ));
    } else {
        let second = second.unwrap();
        assert_eq!(second.status, 200);
        assert_eq!(second.body, b"{}");
    }
    server.join().unwrap();
}

/// Self-signed serving certificate with the given IP SAN and validity window.
fn pinned_server(ip: &str, expired: bool) -> (Vec<u8>, Arc<rustls::ServerConfig>) {
    let mut params = rcgen::CertificateParams::new(vec![ip.into()]).unwrap();
    params.is_ca = rcgen::IsCa::ExplicitNoCa;
    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
    if expired {
        params.not_before = (std::time::SystemTime::now() - Duration::from_secs(7200)).into();
        params.not_after = (std::time::SystemTime::now() - Duration::from_secs(3600)).into();
    }
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
    let cert = params.self_signed(&key).unwrap();
    let config = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_no_client_auth()
    .with_single_cert(
        vec![cert.der().clone()],
        rustls::pki_types::PrivatePkcs8KeyDer::from(key.serialize_der()).into(),
    )
    .unwrap();
    (cert.der().to_vec(), Arc::new(config))
}

/// Discovered endpoint for a loopback port with an explicit pin and generation.
fn discovered(port: u16, certificate: &[u8], generation: &str) -> DiscoveredEndpoint {
    DiscoveredEndpoint {
        url: format!("https://127.0.0.1:{port}"),
        certificate: certificate.to_vec(),
        generation: generation.into(),
    }
}

/// Transport reading only the shared discovered set; the static trust file is absent.
fn discovered_transport(
    endpoints: Vec<DiscoveredEndpoint>,
) -> (ControlTransport, DiscoveredEndpoints) {
    let transport = ControlTransport::new(testing::config(
        "https://static.invalid".into(),
        "/nonexistent/trust.pem".into(),
    ));
    transport.attach_io(Rc::new(FixtureIo));
    let shared = Arc::new(std::sync::RwLock::new(endpoints));
    transport.set_endpoints(shared.clone());
    (transport, shared)
}

/// A port with no listener, used as a dead seat.
fn dead_port() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

/// Every ranking order reaches the one live seat; an empty set is unavailable.
#[test]
fn discovered_endpoints_fail_over_to_live_seat() {
    let (pin, config) = pinned_server("127.0.0.1", false);
    let server = RotationServer::new(config);
    let (dead_pin, _) = pinned_server("127.0.0.1", false);
    let (transport, shared) = discovered_transport(vec![
        discovered(dead_port(), &dead_pin, "a"),
        discovered(server.port(), &pin, "b"),
        discovered(dead_port(), &dead_pin, "c"),
    ]);
    for _ in 0..8 {
        rotation_request(&transport, None).unwrap();
    }
    shared.write().unwrap().clear();
    assert!(matches!(
        rotation_request(&transport, None),
        Err(Error::Transport(RestError::Unavailable))
    ));
}

/// Wrong pins, wrong names, and expired pins never authenticate the server.
#[test]
fn discovered_pin_requires_exact_valid_certificate() {
    let (pin, config) = pinned_server("127.0.0.1", false);
    let server = RotationServer::new(config);
    let (other, _) = pinned_server("127.0.0.1", false);
    let (wrong_name, wrong_name_config) = pinned_server("10.0.0.1", false);
    let (expired, expired_config) = pinned_server("127.0.0.1", true);
    let (transport, shared) = discovered_transport(vec![discovered(server.port(), &other, "a")]);
    assert!(matches!(
        rotation_request(&transport, None),
        Err(Error::Transport(RestError::Unavailable))
    ));
    for (certificate, config) in [(wrong_name, wrong_name_config), (expired, expired_config)] {
        server.rotate(config);
        *shared.write().unwrap() = vec![discovered(server.port(), &certificate, "a")];
        assert!(matches!(
            rotation_request(&transport, None),
            Err(Error::Transport(RestError::Unavailable))
        ));
    }
    // A mismatched seat is skipped in favor of a correctly pinned one.
    let (good_pin, good_config) = pinned_server("127.0.0.1", false);
    let good = RotationServer::new(good_config);
    *shared.write().unwrap() = vec![
        discovered(server.port(), &other, "a"),
        discovered(good.port(), &good_pin, "b"),
    ];
    for _ in 0..4 {
        rotation_request(&transport, None).unwrap();
    }
    drop(pin);
}

/// Pooled authenticated TLS is reused only while its endpoint generation is current.
#[test]
fn discovered_generation_change_fences_pooled_connection() {
    let (pin, config) = pinned_server("127.0.0.1", false);
    let server = RotationServer::new(config);
    let (ca, ca_key) = testing::ca();
    let identity = TestIdentity::new(&ca, &ca_key);
    let (transport, shared) = discovered_transport(vec![discovered(server.port(), &pin, "g1")]);
    let first = rotation_request(&transport, Some(&identity)).unwrap();
    assert_eq!(
        rotation_request(&transport, Some(&identity)).unwrap(),
        first
    );
    *shared.write().unwrap() = vec![discovered(server.port(), &pin, "g2")];
    let renewed = rotation_request(&transport, Some(&identity)).unwrap();
    assert_ne!(renewed, first);
    assert_eq!(
        rotation_request(&transport, Some(&identity)).unwrap(),
        renewed
    );
    // Replacing the shared set object also discards pooled state.
    transport.set_endpoints(Arc::new(std::sync::RwLock::new(vec![discovered(
        server.port(),
        &pin,
        "g2",
    )])));
    assert_ne!(
        rotation_request(&transport, Some(&identity)).unwrap(),
        renewed
    );
}
