// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

impl Provider {}

#[test]
fn control_reuse_rotates_leaf_trust_and_lifetime() {
    use super::super::ControlTransport;
    let first = Fixture::new();
    let second = Fixture::new();
    let provider = first.provider(0);
    let contexts = [
        first.context("spiffe://racer/controlplane", Some("localhost")),
        first.context("spiffe://racer/controlplane", Some("localhost")),
        second.context("spiffe://racer/controlplane", Some("localhost")),
        second.context("spiffe://racer/controlplane", Some("localhost")),
    ];
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let server = std::thread::spawn(move || {
        for context in contexts {
            let (socket, _) = listener.accept().unwrap();
            let mut socket = server(socket, &context);
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                socket.read_exact(&mut byte).unwrap();
                request.push(byte[0]);
            }
            assert!(
                String::from_utf8(request)
                    .unwrap()
                    .contains("X-Racer-Old-Connections: 0\r\n")
            );
            socket
                .write_all(b"HTTP/1.1 204 No Content\r\n\r\n")
                .unwrap();
            // Prove the old authenticated socket is closed before accepting its replacement.
            assert!(matches!(socket.read(&mut [0]), Ok(0) | Err(_)));
        }
    });
    let mut transport = ControlTransport {
        address,
        host: "localhost".into(),
        target: "/".into(),
        provider: provider.clone(),
        idle: None,
        lifetime: Duration::from_secs(240),
    };
    transport.fetch(None, &[], &mut || Ok(())).unwrap();
    // Renew the actual key/leaf with unchanged trust generation, then replace
    // the actual root and leaf. Both must invalidate the cached TLS session.
    for authority in [&first, &second] {
        let old = provider.current();
        provider.state.lock().unwrap().current = Arc::new(Snapshot {
            revision: old.revision + 1,
            generation: old.generation + u64::from(authority.trust.active != old.issuer),
            digest: hex(&authority.trust.digest),
            issuer: authority.trust.active.clone(),
            context: Arc::new(authority.context(&provider.identity.uri(), None)),
            expires_unix: old.expires_unix,
        });
        assert_eq!(provider.headers()[3].1, "1");
        transport.fetch(None, &[], &mut || Ok(())).unwrap();
        assert_eq!(provider.headers()[3].1, "0");
    }
    // Force the monotonic lifetime boundary without a multi-minute sleep.
    transport.idle.as_mut().unwrap().end = Instant::now();
    transport.fetch(None, &[], &mut || Ok(())).unwrap();
    let old = provider.current();
    provider.state.lock().unwrap().current = Arc::new(Snapshot {
        revision: old.revision + 1,
        generation: old.generation,
        digest: old.digest.clone(),
        issuer: old.issuer.clone(),
        context: old.context.clone(),
        expires_unix: unix(),
    });
    assert!(transport.fetch(None, &[], &mut || Ok(())).is_err());
    assert!(
        transport.idle.is_none(),
        "expired identity must not keep an old socket"
    );
    drop(transport);
    assert!(provider.state.lock().unwrap().connections.is_empty());
    server.join().unwrap();
}
use openssl::{
    asn1::Asn1Time,
    bn::BigNum,
    ec::{EcGroup, EcKey},
    hash::MessageDigest,
    nid::Nid,
    pkey::{PKey, Private},
    x509::{
        X509, X509NameBuilder,
        extension::{
            AuthorityKeyIdentifier, BasicConstraints, ExtendedKeyUsage, KeyUsage,
            SubjectAlternativeName, SubjectKeyIdentifier,
        },
    },
};

pub(crate) struct Fixture {
    pub trust: TrustBundle,
    root: X509,
    key: PKey<Private>,
}
impl Fixture {
    pub fn advance(provider: &Provider) {
        let mut state = provider.state.lock().unwrap();
        let old = &state.current;
        state.current = Arc::new(Snapshot {
            revision: old.revision + 1,
            generation: old.generation + 1,
            digest: "new-context".into(),
            issuer: old.issuer.clone(),
            context: old.context.clone(),
            expires_unix: old.expires_unix,
        });
    }
    pub fn new() -> Self {
        let key = key();
        let root = certificate(&key, None, None, "root", None);
        let issuer = hex(&root.digest(MessageDigest::sha256()).unwrap());
        let bytes = serde_json::to_vec(
            &serde_json::json!({"version":1,"generation":1,"active":issuer,
            "certificates":String::from_utf8(root.to_pem().unwrap()).unwrap()}),
        )
        .unwrap();
        Self {
            trust: TrustBundle::parse(&bytes, None).unwrap(),
            root,
            key,
        }
    }
    pub fn bundle(&self, generation: u64) -> Vec<u8> {
        serde_json::to_vec(
            &serde_json::json!({"version":1,"generation":generation,"active":self.trust.active,
            "certificates":String::from_utf8(self.root.to_pem().unwrap()).unwrap()}),
        )
        .unwrap()
    }
    pub fn context(&self, uri: &str, dns: Option<&str>) -> TlsContext {
        let key = key();
        let cert = certificate(&key, Some((&self.root, &self.key)), Some(uri), "leaf", dns);
        TlsContext::new(
            &self.trust,
            &cert.to_pem().unwrap(),
            &key.private_key_to_pem_pkcs8().unwrap(),
        )
        .unwrap()
    }
    pub fn provider(&self, workers: usize) -> Arc<Provider> {
        let identity = PeerIdentity::new(&"01".repeat(32), &"02".repeat(32), "test-pod").unwrap();
        Arc::new(Provider {
            proof_wake: Default::default(),
            state: Mutex::new(State {
                current: Arc::new(Snapshot {
                    revision: 1,
                    generation: self.trust.generation,
                    digest: hex(&self.trust.digest),
                    issuer: self.trust.active.clone(),
                    context: Arc::new(self.context(&identity.uri(), None)),
                    expires_unix: unix() + 3600,
                }),
                installed: BTreeMap::new(),
                error: None,
                connections: BTreeMap::new(),
            }),
            workers,
            identity,
            server_name: "localhost".into(),
        })
    }
}
fn key() -> PKey<Private> {
    PKey::from_ec_key(
        EcKey::generate(&EcGroup::from_curve_name(Nid::X9_62_PRIME256V1).unwrap()).unwrap(),
    )
    .unwrap()
}
fn certificate(
    key: &PKey<Private>,
    issuer: Option<(&X509, &PKey<Private>)>,
    uri: Option<&str>,
    name: &str,
    dns: Option<&str>,
) -> X509 {
    let mut b = X509::builder().unwrap();
    b.set_version(2).unwrap();
    let mut serial = BigNum::new().unwrap();
    serial
        .rand(128, openssl::bn::MsbOption::MAYBE_ZERO, false)
        .unwrap();
    b.set_serial_number(&serial.to_asn1_integer().unwrap())
        .unwrap();
    let mut n = X509NameBuilder::new().unwrap();
    n.append_entry_by_text("CN", name).unwrap();
    let n = n.build();
    b.set_subject_name(&n).unwrap();
    b.set_issuer_name(issuer.map(|(c, _)| c.subject_name()).unwrap_or(&n))
        .unwrap();
    b.set_pubkey(key).unwrap();
    b.set_not_before(&Asn1Time::from_unix((unix() - 60) as i64).unwrap())
        .unwrap();
    b.set_not_after(&Asn1Time::from_unix((unix() + 3600) as i64).unwrap())
        .unwrap();
    if issuer.is_none() {
        b.append_extension(BasicConstraints::new().critical().ca().build().unwrap())
            .unwrap();
        b.append_extension(
            KeyUsage::new()
                .critical()
                .key_cert_sign()
                .crl_sign()
                .build()
                .unwrap(),
        )
        .unwrap();
    } else {
        b.append_extension(BasicConstraints::new().critical().build().unwrap())
            .unwrap();
        b.append_extension(
            KeyUsage::new()
                .critical()
                .digital_signature()
                .build()
                .unwrap(),
        )
        .unwrap();
        let cp = uri == Some("spiffe://racer/controlplane");
        let mut eku = ExtendedKeyUsage::new();
        eku.server_auth();
        if !cp {
            eku.client_auth();
        }
        b.append_extension(eku.build().unwrap()).unwrap();
        let mut san = SubjectAlternativeName::new();
        if cp {
            san.dns("racer-controlplane.test-namespace.svc");
        }
        san.uri(&crate::tls::tests::signed_uri(uri.unwrap()));
        if let Some(dns) = dns {
            san.dns(dns);
        }
        let extension = san
            .build(&b.x509v3_context(issuer.map(|(c, _)| c.as_ref()), None))
            .unwrap();
        b.append_extension(extension).unwrap();
    }
    let ski = SubjectKeyIdentifier::new()
        .build(&b.x509v3_context(issuer.map(|(c, _)| c.as_ref()), None))
        .unwrap();
    b.append_extension(ski).unwrap();
    if let Some((root, _)) = issuer {
        let aki = AuthorityKeyIdentifier::new()
            .keyid(true)
            .build(&b.x509v3_context(Some(root), None))
            .unwrap();
        b.append_extension(aki).unwrap();
    }
    b.sign(
        issuer.map(|(_, k)| k).unwrap_or(key),
        MessageDigest::sha256(),
    )
    .unwrap();
    b.build()
}
pub(crate) fn server(socket: TcpStream, context: &TlsContext) -> Stream {
    socket.set_nonblocking(true).unwrap();
    let session = TlsSession::server(
        context,
        socket.into(),
        ExpectedPeer::Universe("01".repeat(32)),
    )
    .unwrap();
    let mut stream = Stream {
        owner: None,
        session,
        read_timeout: Duration::from_secs(3),
        write_timeout: Duration::from_secs(3),
        end: Instant::now() + Duration::from_secs(30),
    };
    loop {
        match stream.session.handshake().unwrap() {
            TlsProgress::Complete(()) => return stream,
            TlsProgress::WantRead => stream.wait(false, stream.end).unwrap(),
            TlsProgress::WantWrite => stream.wait(true, stream.end).unwrap(),
            TlsProgress::Eof => panic!("TLS handshake EOF"),
        }
    }
}
#[test]
fn worker_installation_barrier_rejects_stale_and_foreign_acknowledgments() {
    let fixture = Fixture::new();
    let provider = fixture.provider(2);
    let generation = |p: &Provider| p.headers()[0].1.clone();
    assert_eq!(generation(&provider), "0");
    provider.installed(0, 1, 2);
    provider.installed(2, 1, 0);
    provider.installed(1, 0, 0);
    assert_eq!(generation(&provider), "0");
    provider.installed(1, 1, 3);
    assert_eq!(generation(&provider), "1");
    assert_eq!(provider.headers()[3].1, "5");
    let current = provider.current();
    provider.state.lock().unwrap().current = Arc::new(Snapshot {
        revision: 2,
        generation: 2,
        digest: "new".into(),
        issuer: current.issuer.clone(),
        context: current.context.clone(),
        expires_unix: current.expires_unix,
    });
    provider.installed(0, 1, 0);
    assert_eq!(generation(&provider), "0");
    provider.installed(0, 2, 1);
    provider.installed(1, 2, 0);
    assert_eq!(generation(&provider), "2");
    assert_eq!(provider.headers()[3].1, "1");
}
#[test]
fn bundle_rejects_rollback_equivocation_and_invalid_projection() {
    let fixture = Fixture::new();
    let next = TrustBundle::parse(&fixture.bundle(2), Some(&fixture.trust)).unwrap();
    assert!(TrustBundle::parse(&fixture.bundle(1), Some(&next)).is_err());
    let mut different = fixture.bundle(2);
    different.push(b' ');
    assert!(TrustBundle::parse(&different, Some(&next)).is_err());
    assert!(TrustBundle::parse(b"partial", Some(&next)).is_err());
    assert_eq!(next.generation, 2);
}

struct Scratch(PathBuf);
impl Scratch {
    fn new() -> Self {
        static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!(
                "credential-test-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        std::fs::create_dir_all(&path).unwrap();
        Self(path)
    }
    fn settings(&self, address: SocketAddr) -> Settings {
        std::fs::write(self.0.join("token"), "test-bound-token\n").unwrap();
        Settings {
            boot: "03".repeat(32),
            proof: url::Url::parse("https://127.0.0.1:1/v3/proof").unwrap(),
            trust_dir: self.0.clone(),
            enroll: url::Url::parse(&format!("https://{address}/v3/enroll")).unwrap(),
            token: self.0.join("token"),
            namespace: "test-namespace".into(),
            pod: "test-name".into(),
            server_name: "localhost".into(),
            identity: PeerIdentity::new(&"01".repeat(32), &"02".repeat(32), "test-pod").unwrap(),
        }
    }
}
impl Drop for Scratch {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

// Enrollment intentionally has server authentication only. The bearer token and
// CSR are checked here, then the returned leaf is checked by the real client.
fn enroll_server(
    authority: &Fixture,
    replies: Vec<(Fixture, u64, Option<&'static str>)>,
) -> (SocketAddr, std::thread::JoinHandle<()>) {
    let authorities = (0..replies.len())
        .map(|_| copy_fixture(authority))
        .collect();
    enroll_server_with_authorities(authorities, replies)
}

fn enroll_server_with_authorities(
    authorities: Vec<Fixture>,
    replies: Vec<(Fixture, u64, Option<&'static str>)>,
) -> (SocketAddr, std::thread::JoinHandle<()>) {
    use openssl::ssl::{SslAcceptor, SslMethod, SslVersion};
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let thread = std::thread::spawn(move || {
        use std::io::BufRead;
        for (authority, (issuer, generation, bad_identity)) in authorities.into_iter().zip(replies)
        {
            let key = key();
            let cert = certificate(
                &key,
                Some((&authority.root, &authority.key)),
                Some("spiffe://racer/controlplane"),
                "control",
                Some("localhost"),
            );
            let mut acceptor =
                SslAcceptor::mozilla_intermediate_v5(SslMethod::tls_server()).unwrap();
            acceptor
                .set_min_proto_version(Some(SslVersion::TLS1_3))
                .unwrap();
            acceptor.set_certificate(&cert).unwrap();
            acceptor.set_private_key(&key).unwrap();
            let acceptor = acceptor.build();
            let (socket, _) = listener.accept().unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            let mut stream = acceptor.accept(socket).unwrap();
            let mut reader = io::BufReader::new(&mut stream);
            let mut headers = String::new();
            loop {
                let mut line = String::new();
                reader.read_line(&mut line).unwrap();
                assert!(!line.is_empty());
                headers.push_str(&line);
                if line == "\r\n" {
                    break;
                }
            }
            assert!(headers.starts_with("POST /v3/enroll HTTP/1.1\r\n"));
            assert!(headers.contains("Authorization: Bearer test-bound-token\r\n"));
            assert!(headers.contains(&format!("X-Racer-Boot: {}\r\n", "03".repeat(32))));
            let length: usize = headers
                .lines()
                .find_map(|line| line.strip_prefix("Content-Length: "))
                .unwrap()
                .parse()
                .unwrap();
            let mut body = vec![0; length];
            reader.read_exact(&mut body).unwrap();
            let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
            assert_eq!(body["pod_namespace"], "test-namespace");
            assert_eq!(body["pod_name"], "test-name");
            assert_eq!(body["expected_universe"], "01".repeat(32));
            assert_eq!(body["expected_node"], "02".repeat(32));
            let csr =
                openssl::x509::X509Req::from_pem(body["csr"].as_str().unwrap().as_bytes()).unwrap();
            let public = csr.public_key().unwrap();
            assert!(csr.verify(&public).unwrap());
            let identity =
                PeerIdentity::new(&"01".repeat(32), &"02".repeat(32), "test-pod").unwrap();
            let mut leaf = X509::builder().unwrap();
            leaf.set_version(2).unwrap();
            leaf.set_serial_number(&BigNum::from_u32(17).unwrap().to_asn1_integer().unwrap())
                .unwrap();
            leaf.set_subject_name(csr.subject_name()).unwrap();
            leaf.set_issuer_name(issuer.root.subject_name()).unwrap();
            leaf.set_pubkey(&public).unwrap();
            leaf.set_not_before(&Asn1Time::from_unix((unix() - 60) as i64).unwrap())
                .unwrap();
            leaf.set_not_after(&Asn1Time::from_unix((unix() + 3600) as i64).unwrap())
                .unwrap();
            leaf.append_extension(BasicConstraints::new().critical().build().unwrap())
                .unwrap();
            leaf.append_extension(KeyUsage::new().digital_signature().build().unwrap())
                .unwrap();
            leaf.append_extension(
                ExtendedKeyUsage::new()
                    .client_auth()
                    .server_auth()
                    .build()
                    .unwrap(),
            )
            .unwrap();
            let san = SubjectAlternativeName::new()
                .critical()
                .uri(&crate::tls::tests::signed_uri(
                    bad_identity.unwrap_or(&identity.uri()),
                ))
                .build(&leaf.x509v3_context(Some(&issuer.root), None))
                .unwrap();
            leaf.append_extension(san).unwrap();
            let aki = AuthorityKeyIdentifier::new()
                .keyid(true)
                .build(&leaf.x509v3_context(Some(&issuer.root), None))
                .unwrap();
            leaf.append_extension(aki).unwrap();
            leaf.sign(&issuer.key, MessageDigest::sha256()).unwrap();
            let body = serde_json::to_vec(&serde_json::json!({
                "certificate": String::from_utf8(leaf.build().to_pem().unwrap()).unwrap(),
                "generation":generation, "issuer":issuer.trust.active,
            }))
            .unwrap();
            write!(
                stream,
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                body.len()
            )
            .unwrap();
            stream.write_all(&body).unwrap();
        }
    });
    (address, thread)
}

fn copy_fixture(f: &Fixture) -> Fixture {
    Fixture {
        trust: TrustBundle::parse(&f.bundle(1), None).unwrap(),
        root: f.root.clone(),
        key: f.key.clone(),
    }
}

#[test]
fn enrollment_proves_local_key_identity_and_generation() {
    let fixture = Fixture::new();
    let scratch = Scratch::new();
    for (generation, wrong_identity, succeeds) in [
        (1, None, true),
        (2, None, false),
        (1, Some("spiffe://racer/controlplane"), false),
    ] {
        let (address, server) = enroll_server(
            &fixture,
            vec![(copy_fixture(&fixture), generation, wrong_identity)],
        );
        let settings = scratch.settings(address);
        let result = Leaf::enroll(&settings, &fixture.trust);
        assert_eq!(result.is_ok(), succeeds, "{:?}", result.as_ref().err());
        if let Ok(leaf) = result {
            assert_eq!(leaf.issuer, fixture.trust.active);
            assert!(leaf.renew > unix() + 1400 && leaf.renew < unix() + 2100);
            assert!(leaf.snapshot(&fixture.trust, 1, &settings.identity).is_ok());
            let other_key = key().private_key_to_pem_pkcs8().unwrap();
            assert!(
                tls::validate_leaf(
                    &fixture.trust,
                    &leaf.certificate,
                    &other_key,
                    &settings.identity
                )
                .is_err()
            );
        }
        server.join().unwrap();
    }
}

#[test]
fn manager_retains_last_good_and_renews_on_active_root_change() {
    let first = Fixture::new();
    let second = Fixture::new();
    let scratch = Scratch::new();
    std::fs::write(scratch.0.join("bundle.json"), first.bundle(1)).unwrap();
    let (address, server) = enroll_server(
        &first,
        vec![
            (copy_fixture(&first), 1, None),
            (copy_fixture(&second), 2, None),
        ],
    );
    let updates = Arc::new(super::super::Updates::default());
    let manager = Manager::start_with_settings(1, &updates, scratch.settings(address)).unwrap();
    let provider = updates.credentials().unwrap();
    let initial = provider.current();
    provider.installed(0, initial.revision, 0);
    std::fs::write(scratch.0.join("bundle.json"), b"partial projection").unwrap();
    wait_until(|| provider.status()["error"].is_string());
    assert_eq!(provider.current().digest, initial.digest);
    let roots = format!(
        "{}{}",
        String::from_utf8(first.root.to_pem().unwrap()).unwrap(),
        String::from_utf8(second.root.to_pem().unwrap()).unwrap()
    );
    let bundle = serde_json::to_vec(&serde_json::json!({
        "version":1,"generation":2,"active":second.trust.active,"certificates":roots,
    }))
    .unwrap();
    std::fs::write(scratch.0.join("bundle.json"), &bundle).unwrap();
    wait_until(|| provider.current().issuer == second.trust.active);
    let current = provider.current();
    assert_eq!(current.generation, 2);
    assert_eq!(
        current.digest,
        hex(&TrustBundle::parse(&bundle, None).unwrap().digest)
    );
    assert_eq!(provider.headers()[0].1, "0");
    provider.installed(0, current.revision, 1);
    assert_eq!(provider.headers()[0].1, "2");
    assert_eq!(provider.headers()[3].1, "1");
    std::fs::write(scratch.0.join("bundle.json"), first.bundle(1)).unwrap();
    wait_until(|| {
        provider.status()["error"]
            .as_str()
            .is_some_and(|e| e.contains("rollback"))
    });
    assert_eq!(provider.current().digest, current.digest);
    drop(manager);
    server.join().unwrap();
}

#[test]
fn reenrollment_recovers_when_offline_root_is_already_retired() {
    let first = Fixture::new();
    let second = Fixture::new();
    let scratch = Scratch::new();
    std::fs::write(scratch.0.join("bundle.json"), first.bundle(1)).unwrap();
    let (initial_address, initial_server) =
        enroll_server(&first, vec![(copy_fixture(&first), 1, None)]);
    let mut settings = scratch.settings(initial_address);
    let mut leaf = Leaf::enroll(&settings, &first.trust).unwrap();
    initial_server.join().unwrap();
    // The normal overlap has completed while this node was disconnected.
    let replacement = TrustBundle::parse(&second.bundle(2), Some(&first.trust)).unwrap();
    assert!(leaf.snapshot(&replacement, 2, &settings.identity).is_err());
    let (address, server) = enroll_server(&second, vec![(copy_fixture(&second), 2, None)]);
    settings.enroll = url::Url::parse(&format!("https://{address}/v3/enroll")).unwrap();
    leaf = Leaf::enroll(&settings, &replacement).unwrap();
    let snapshot = leaf.snapshot(&replacement, 2, &settings.identity).unwrap();
    assert_eq!(snapshot.issuer, second.trust.active);
    assert_eq!(snapshot.generation, 2);
    server.join().unwrap();
}

#[test]
fn manager_recovers_after_offline_root_retirement() {
    let first = Fixture::new();
    let second = Fixture::new();
    let scratch = Scratch::new();
    std::fs::write(scratch.0.join("bundle.json"), first.bundle(1)).unwrap();
    let (address, server) = enroll_server_with_authorities(
        vec![copy_fixture(&first), copy_fixture(&second)],
        vec![
            (copy_fixture(&first), 1, None),
            (copy_fixture(&second), 2, None),
        ],
    );
    let updates = Arc::new(super::super::Updates::default());
    let manager = Manager::start_with_settings(1, &updates, scratch.settings(address)).unwrap();
    let provider = updates.credentials().unwrap();
    let old = provider.current();
    std::fs::write(scratch.0.join("bundle.json"), second.bundle(2)).unwrap();
    wait_until(|| provider.current().generation == 2);
    assert_eq!(provider.current().issuer, second.trust.active);
    assert_eq!(old.generation, 1);
    assert_eq!(provider.headers()[0].1, "0");
    drop(manager);
    server.join().unwrap();
}

#[test]
fn pending_enrollment_preserves_observed_generation_floor() {
    let first = Fixture::new();
    let second = Fixture::new();
    let scratch = Scratch::new();
    std::fs::write(scratch.0.join("bundle.json"), first.bundle(1)).unwrap();
    let (address, server) = enroll_server(&first, vec![(copy_fixture(&first), 1, None)]);
    let updates = Arc::new(super::super::Updates::default());
    let manager = Manager::start_with_settings(1, &updates, scratch.settings(address)).unwrap();
    server.join().unwrap();
    let provider = updates.credentials().unwrap();
    // Make reenrollment fail deterministically without relying on an unbound
    // ephemeral port staying unused while other network tests run.
    std::fs::write(scratch.0.join("token"), "").unwrap();
    std::fs::write(scratch.0.join("bundle.json"), second.bundle(2)).unwrap();
    wait_until(|| {
        provider.status()["error"]
            .as_str()
            .is_some_and(|e| e.contains("invalid enrollment bearer token"))
    });
    assert_eq!(provider.current().generation, 1);
    std::fs::write(scratch.0.join("bundle.json"), first.bundle(1)).unwrap();
    wait_until(|| {
        provider.status()["error"]
            .as_str()
            .is_some_and(|e| e.contains("rollback"))
    });
    assert_eq!(provider.current().issuer, first.trust.active);
    drop(manager);
}

fn wait_until(mut ready: impl FnMut() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !ready() {
        assert!(Instant::now() < deadline, "credential transition timed out");
        std::thread::sleep(Duration::from_millis(20));
    }
}

#[test]
fn proof_requires_installed_context_and_fresh_mutual_tls() {
    use std::io::BufRead;
    let fixture = Fixture::new();
    let provider = fixture.provider(1);
    let scratch = Scratch::new();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let mut settings = scratch.settings(address);
    settings.proof = url::Url::parse(&format!("https://{address}/v3/proof")).unwrap();
    // A not-yet-installed context must not even establish a proof connection.
    prove(&settings, &provider).unwrap();
    listener.set_nonblocking(true).unwrap();
    assert_eq!(
        listener.accept().unwrap_err().kind(),
        io::ErrorKind::WouldBlock
    );
    listener.set_nonblocking(false).unwrap();
    provider.installed(0, 1, 2);
    let context = fixture.context("spiffe://racer/controlplane", Some("localhost"));
    let server = std::thread::spawn(move || {
        for _ in 0..2 {
            let (socket, _) = listener.accept().unwrap();
            let mut stream = server(socket, &context);
            let mut reader = io::BufReader::new(&mut stream);
            let mut headers = String::new();
            loop {
                let mut line = String::new();
                reader.read_line(&mut line).unwrap();
                assert!(!line.is_empty());
                headers.push_str(&line);
                if line == "\r\n" {
                    break;
                }
            }
            assert!(headers.starts_with("POST /v3/proof HTTP/1.1\r\n"));
            assert!(headers.contains("X-Racer-Trust-Generation: 1\r\n"));
            assert!(headers.contains("X-Racer-Old-Connections: 2\r\n"));
            assert!(headers.contains(&format!("X-Racer-Boot: {}\r\n", "03".repeat(32))));
            stream
                .write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
                .unwrap();
        }
    });
    prove(&settings, &provider).unwrap();
    prove(&settings, &provider).unwrap();
    server.join().unwrap();
    assert!(provider.state.lock().unwrap().connections.is_empty());
}
#[test]
fn proof_refresh_is_jittered_within_security_lifetime() {
    let mut random = 1234;
    let mut delays = std::collections::BTreeSet::new();
    for _ in 0..100 {
        let delay = super::proof_refresh_delay(&mut random);
        assert!(delay >= Duration::from_secs(180));
        assert!(delay <= Duration::from_secs(240));
        assert!(delay + Duration::from_secs(10) < Duration::from_secs(300));
        delays.insert(delay);
    }
    assert!(delays.len() > 20);
}

#[test]
fn manager_proves_installation_and_drain_immediately_then_holds_refresh() {
    let fixture = Fixture::new();
    let scratch = Scratch::new();
    std::fs::write(scratch.0.join("bundle.json"), fixture.bundle(1)).unwrap();
    let (enroll, enrollment) = enroll_server(&fixture, vec![(copy_fixture(&fixture), 1, None)]);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let mut settings = scratch.settings(enroll);
    settings.proof = url::Url::parse(&format!(
        "https://{}/v3/proof",
        listener.local_addr().unwrap()
    ))
    .unwrap();
    let context = fixture.context("spiffe://racer/controlplane", Some("localhost"));
    let stop = Arc::new(AtomicBool::new(false));
    let stopping = stop.clone();
    let (tx, rx) = std::sync::mpsc::channel();
    let proofs = std::thread::spawn(move || {
        while !stopping.load(Ordering::Acquire) {
            let (socket, _) = match listener.accept() {
                Ok(socket) => socket,
                Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                    std::thread::sleep(Duration::from_millis(1));
                    continue;
                }
                Err(error) => panic!("{error}"),
            };
            let mut stream = server(socket, &context);
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                stream.read_exact(&mut byte).unwrap();
                request.push(byte[0]);
            }
            stream
                .write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
                .unwrap();
            tx.send(String::from_utf8(request).unwrap()).unwrap();
        }
    });
    let updates = Arc::new(super::super::Updates::default());
    let manager = Manager::start_with_settings(1, &updates, settings).unwrap();
    enrollment.join().unwrap();
    let provider = updates.credentials().unwrap();
    provider.installed(0, 1, 2);
    assert!(
        rx.recv_timeout(Duration::from_secs(2))
            .unwrap()
            .contains("X-Racer-Old-Connections: 2\r\n")
    );
    assert!(rx.recv_timeout(Duration::from_millis(200)).is_err());
    // A completed drain must wake a sleeping manager, not wait for periodic proof.
    // Invalid projection cannot suppress proof of the still-installed last-good trust.
    std::fs::write(scratch.0.join("bundle.json"), b"invalid projection").unwrap();
    provider.installed(0, 1, 0);
    assert!(
        rx.recv_timeout(Duration::from_millis(600))
            .unwrap()
            .contains("X-Racer-Old-Connections: 0\r\n")
    );
    assert!(rx.recv_timeout(Duration::from_millis(200)).is_err());
    drop(manager);
    stop.store(true, Ordering::Release);
    proofs.join().unwrap();
}
