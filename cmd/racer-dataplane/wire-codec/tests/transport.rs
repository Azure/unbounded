//! Socket-level transport contracts exercised through the public client API.
use std::{
    io::{Read, Write},
    rc::Rc,
    sync::Arc,
    time::Duration,
};
use wire_codec::rest::{Error as RestError, Method, Operation, Transport};
#[path = "support/mod.rs"]
mod testing;
use testing::{Error, RotationServer, TestTransport as _, WeeklyCertificates, rotation_request};
use testing::{FixtureIo, TestIdentity};
/// Concrete transport used by loopback fixtures.
type ControlTransport = Transport<FixtureIo>;
const SNAPSHOT_PATH: &str = "/snapshot";
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
