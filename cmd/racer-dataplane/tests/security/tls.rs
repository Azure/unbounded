// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use openssl::{
    bn::BigNum,
    pkey::Private,
    x509::extension::{
        AuthorityKeyIdentifier, BasicConstraints, ExtendedKeyUsage, KeyUsage, SubjectKeyIdentifier,
    },
};
use std::{
    io::Write,
    net::{TcpListener, TcpStream},
    os::fd::{AsFd, FromRawFd},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

pub(crate) struct Authority {
    pub(crate) cert: X509,
    pub(crate) key: PKey<Private>,
}

fn key() -> PKey<Private> {
    let group = EcGroup::from_curve_name(Nid::X9_62_PRIME256V1).unwrap();
    PKey::from_ec_key(EcKey::generate(&group).unwrap()).unwrap()
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64
}

impl Authority {
    pub(crate) fn new() -> Self {
        let key = key();
        let mut name = X509NameBuilder::new().unwrap();
        name.append_entry_by_text("CN", "Racer test root").unwrap();
        let name = name.build();
        let mut cert = X509::builder().unwrap();
        cert.set_version(2).unwrap();
        cert.set_serial_number(&BigNum::from_u32(1).unwrap().to_asn1_integer().unwrap())
            .unwrap();
        cert.set_subject_name(&name).unwrap();
        cert.set_issuer_name(&name).unwrap();
        cert.set_pubkey(&key).unwrap();
        cert.set_not_before(&Asn1Time::from_unix(now() - 60).unwrap())
            .unwrap();
        cert.set_not_after(&Asn1Time::from_unix(now() + 86400).unwrap())
            .unwrap();
        cert.append_extension(BasicConstraints::new().critical().ca().build().unwrap())
            .unwrap();
        cert.append_extension(
            KeyUsage::new()
                .critical()
                .key_cert_sign()
                .crl_sign()
                .build()
                .unwrap(),
        )
        .unwrap();
        let ski = SubjectKeyIdentifier::new()
            .build(&cert.x509v3_context(None, None))
            .unwrap();
        cert.append_extension(ski).unwrap();
        cert.sign(&key, MessageDigest::sha256()).unwrap();
        Self {
            cert: cert.build(),
            key,
        }
    }

    pub(crate) fn leaf(
        &self,
        uris: &[&str],
        dns: Option<&str>,
        start: i64,
        end: i64,
    ) -> (Vec<u8>, Vec<u8>) {
        let key = key();
        let mut cert = X509::builder().unwrap();
        cert.set_version(2).unwrap();
        cert.set_serial_number(&BigNum::from_u32(2).unwrap().to_asn1_integer().unwrap())
            .unwrap();
        let mut name = X509NameBuilder::new().unwrap();
        name.append_entry_by_text("CN", "not-used-for-identity")
            .unwrap();
        cert.set_subject_name(&name.build()).unwrap();
        cert.set_issuer_name(self.cert.subject_name()).unwrap();
        cert.set_pubkey(&key).unwrap();
        cert.set_not_before(&Asn1Time::from_unix(start).unwrap())
            .unwrap();
        cert.set_not_after(&Asn1Time::from_unix(end).unwrap())
            .unwrap();
        cert.append_extension(BasicConstraints::new().critical().build().unwrap())
            .unwrap();
        cert.append_extension(
            KeyUsage::new()
                .critical()
                .digital_signature()
                .build()
                .unwrap(),
        )
        .unwrap();
        cert.append_extension(
            ExtendedKeyUsage::new()
                .server_auth()
                .client_auth()
                .build()
                .unwrap(),
        )
        .unwrap();
        let aki = AuthorityKeyIdentifier::new()
            .keyid(true)
            .build(&cert.x509v3_context(Some(&self.cert), None))
            .unwrap();
        cert.append_extension(aki).unwrap();
        let mut san = SubjectAlternativeName::new();
        for uri in uris {
            san.uri(uri);
        }
        if let Some(dns) = dns {
            san.dns(dns);
        }
        let san = san
            .build(&cert.x509v3_context(Some(&self.cert), None))
            .unwrap();
        cert.append_extension(san).unwrap();
        cert.sign(&self.key, MessageDigest::sha256()).unwrap();
        (
            cert.build().to_pem().unwrap(),
            key.private_key_to_pem_pkcs8().unwrap(),
        )
    }

    pub(crate) fn bundle_bytes(&self, generation: u64) -> Vec<u8> {
        serde_json::to_vec(&serde_json::json!({
            "version": 1, "generation": generation,
            "active": hex(&Sha256::digest(self.cert.to_der().unwrap())),
            "certificates": String::from_utf8(self.cert.to_pem().unwrap()).unwrap(),
        }))
        .unwrap()
    }

    pub(crate) fn bundle(&self) -> TrustBundle {
        TrustBundle::parse(&self.bundle_bytes(1), None).unwrap()
    }

    pub(crate) fn context(&self, identity: &PeerIdentity, ktls: bool) -> TlsContext {
        let (cert, key) = self.leaf(&[&identity.uri()], None, now() - 60, now() + 3600);
        TlsContext::build(&self.bundle(), Some((&cert, &key)), ktls).unwrap()
    }
}

fn identity(node: char) -> PeerIdentity {
    PeerIdentity::new(&"a".repeat(64), &node.to_string().repeat(64), "pod-123").unwrap()
}

fn sockets() -> (OwnedFd, OwnedFd) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let client = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    let (server, _) = listener.accept().unwrap();
    client.set_nonblocking(true).unwrap();
    server.set_nonblocking(true).unwrap();
    (client.into(), server.into())
}

fn sessions(
    client: &TlsContext,
    server: &TlsContext,
    expected_server: ExpectedPeer,
) -> (TlsSession, TlsSession) {
    let (a, b) = sockets();
    (
        TlsSession::client(client, a, expected_server).unwrap(),
        TlsSession::server(server, b, ExpectedPeer::Identity(identity('b'))).unwrap(),
    )
}

fn handshake(a: &mut TlsSession, b: &mut TlsSession) -> io::Result<()> {
    let deadline = Instant::now() + Duration::from_secs(3);
    while Instant::now() < deadline {
        let a_done = matches!(a.handshake()?, TlsProgress::Complete(()));
        let b_done = matches!(b.handshake()?, TlsProgress::Complete(()));
        if a_done && b_done {
            return Ok(());
        }
        std::thread::sleep(Duration::from_millis(1));
    }
    Err(io::Error::new(
        io::ErrorKind::TimedOut,
        "test handshake deadline",
    ))
}

fn read_exact(session: &mut TlsSession, expected: &[u8]) {
    let deadline = Instant::now() + Duration::from_secs(3);
    let mut got = vec![0; expected.len()];
    let mut offset = 0;
    while offset < got.len() {
        assert!(Instant::now() < deadline);
        match session.read(&mut got[offset..]).unwrap() {
            TlsProgress::Complete(n) => {
                assert!(n > 0);
                offset += n;
            }
            TlsProgress::WantRead | TlsProgress::WantWrite => {
                std::thread::sleep(Duration::from_millis(1))
            }
            TlsProgress::Eof => panic!("premature EOF"),
        }
    }
    assert_eq!(got, expected);
}

#[test]
fn real_socket_mutual_auth_encrypted_fallback_and_close_notify() {
    let ca = Authority::new();
    let (mut client, mut server) = sessions(
        &ca.context(&identity('b'), false),
        &ca.context(&identity('c'), false),
        ExpectedPeer::Identity(identity('c')),
    );
    assert!(client.read(&mut [0]).is_err());
    assert!(matches!(client.handshake().unwrap(), TlsProgress::WantRead));
    handshake(&mut client, &mut server).unwrap();
    assert_eq!(server.peer_identity(), Some(&identity('b')));
    assert_eq!(client.ssl.version_str(), "TLSv1.3");
    assert!(!client.ssl.session_reused());
    assert_eq!(
        client.offload(),
        Offload {
            tx: false,
            rx: false
        }
    );
    assert_eq!(client.counters().encrypted_fallback_connections, 1);
    let message = b"secret application bytes never go to the socket in plaintext";
    assert_eq!(
        client.write(message).unwrap(),
        TlsProgress::Complete(message.len())
    );
    // Peek without consuming the record: software TLS must expose ciphertext on
    // the actual TCP socket, while SSL_read recovers the exact original payload.
    let mut wire = [0u8; 4096];
    let deadline = Instant::now() + Duration::from_secs(3);
    let n = loop {
        let n = unsafe {
            libc::recv(
                server.as_raw_fd(),
                wire.as_mut_ptr().cast(),
                wire.len(),
                libc::MSG_PEEK,
            )
        };
        if n > message.len() as isize {
            break n;
        }
        assert!(Instant::now() < deadline);
        std::thread::sleep(Duration::from_millis(1));
    };
    assert!(n > message.len() as isize);
    assert_eq!(wire[0], 23); // TLS application-data record.
    assert!(
        !wire[..n as usize]
            .windows(message.len())
            .any(|w| w == message)
    );
    read_exact(&mut server, message);
    assert_eq!(client.counters().tx_bytes, message.len() as u64);
    assert_eq!(server.counters().rx_bytes, message.len() as u64);
    assert_eq!(client.shutdown().unwrap(), TlsProgress::WantRead);
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        assert!(Instant::now() < deadline);
        if server.read(&mut [0]).unwrap() == TlsProgress::Eof {
            break;
        }
        std::thread::sleep(Duration::from_millis(1));
    }
    assert_eq!(server.shutdown().unwrap(), TlsProgress::Complete(()));
}

#[test]
fn sendfile_uses_actual_offload_or_encrypted_fallback() {
    for ktls in [false, true] {
        let ca = Authority::new();
        let (mut client, mut server) = sessions(
            &ca.context(&identity('b'), ktls),
            &ca.context(&identity('c'), ktls),
            ExpectedPeer::Identity(identity('c')),
        );
        handshake(&mut client, &mut server).unwrap();
        let native_bits = unsafe { racer_tls_offload(client.ssl.as_ptr().cast()) };
        assert_eq!(client.offload().tx, native_bits & 1 != 0);
        assert_eq!(client.offload().rx, native_bits & 2 != 0);
        let fd = unsafe { libc::memfd_create(c"racer-tls-test".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(fd >= 0);
        let mut file = unsafe { std::fs::File::from_raw_fd(fd) };
        file.write_all(b"skip:encrypted file-backed payload")
            .unwrap();
        let payload = b"encrypted file-backed payload";
        assert_eq!(
            client.sendfile(file.as_fd(), 5, payload.len()).unwrap(),
            TlsProgress::Complete(payload.len())
        );
        read_exact(&mut server, payload);
        assert_eq!(
            client.sendfile(file.as_fd(), 1000, 1).unwrap(),
            TlsProgress::Complete(0)
        );
        assert_eq!(
            client.counters().sendfile_bytes,
            if client.offload.tx {
                payload.len() as u64
            } else {
                0
            }
        );
        assert_eq!(
            client.counters().fallback_sendfile_bytes,
            if client.offload.tx {
                0
            } else {
                payload.len() as u64
            }
        );
        eprintln!(
            "native TLS test kTLS enabled={ktls} actual={:?}",
            client.offload()
        );
    }
}

#[test]
fn exact_peer_identity_rejects_wrong_universe_node_pod_and_duplicate_uri() {
    let ca = Authority::new();
    let client_context = ca.context(&identity('b'), false);
    let actual = identity('c');
    let mut wrong_universe = actual.clone();
    wrong_universe.universe = "d".repeat(64);
    let mut wrong_pod = actual.clone();
    wrong_pod.pod_uid = "pod-other".into();
    for expected in [wrong_universe, identity('d'), wrong_pod] {
        let (mut client, mut server) = sessions(
            &client_context,
            &ca.context(&actual, false),
            ExpectedPeer::Identity(expected),
        );
        assert!(handshake(&mut client, &mut server).is_err());
        assert!(client.write(b"must not escape").is_err());
        assert!(client.handshake().is_err());
    }
    let (cert, key) = ca.leaf(
        &[&actual.uri(), &identity('d').uri()],
        None,
        now() - 60,
        now() + 3600,
    );
    let context = TlsContext::new(&ca.bundle(), &cert, &key).unwrap();
    let (mut client, mut server) =
        sessions(&client_context, &context, ExpectedPeer::Identity(actual));
    assert!(handshake(&mut client, &mut server).is_err());
}

#[test]
fn mutual_auth_rejects_missing_untrusted_and_expired_certificates() {
    let ca = Authority::new();
    let stranger = Authority::new();
    let server_context = ca.context(&identity('c'), false);
    let (untrusted_cert, untrusted_key) =
        stranger.leaf(&[&identity('b').uri()], None, now() - 60, now() + 3600);
    let untrusted = TlsContext::new(&ca.bundle(), &untrusted_cert, &untrusted_key).unwrap();
    let (expired_cert, expired_key) =
        ca.leaf(&[&identity('b').uri()], None, now() - 3600, now() - 60);
    let expired = TlsContext::new(&ca.bundle(), &expired_cert, &expired_key).unwrap();
    for context in [
        TlsContext::bootstrap(&ca.bundle()).unwrap(),
        untrusted,
        expired,
    ] {
        let (mut client, mut server) = sessions(
            &context,
            &server_context,
            ExpectedPeer::Identity(identity('c')),
        );
        assert!(handshake(&mut client, &mut server).is_err());
        assert!(server.write(b"unauthorized").is_err());
    }
}

#[test]
fn control_plane_requires_uri_and_dns_san() {
    let ca = Authority::new();
    for (uri, dns, succeeds) in [
        (CONTROL_PLANE_URI.to_owned(), "control.racer.test", true),
        (identity('c').uri(), "control.racer.test", false),
        (CONTROL_PLANE_URI.to_owned(), "wrong.racer.test", false),
        (CONTROL_PLANE_URI.to_owned(), "*.racer.test", false),
    ] {
        let (cert, key) = ca.leaf(&[&uri], Some(dns), now() - 60, now() + 3600);
        let server_context = TlsContext::new(&ca.bundle(), &cert, &key).unwrap();
        let (mut client, mut server) = sessions(
            &ca.context(&identity('b'), false),
            &server_context,
            ExpectedPeer::ControlPlane {
                dns_name: "control.racer.test".into(),
            },
        );
        assert_eq!(handshake(&mut client, &mut server).is_ok(), succeeds);
    }
}

#[test]
fn abrupt_tcp_close_is_not_authenticated_tls_eof() {
    let ca = Authority::new();
    let (mut client, mut server) = sessions(
        &ca.context(&identity('b'), false),
        &ca.context(&identity('c'), false),
        ExpectedPeer::Identity(identity('c')),
    );
    handshake(&mut client, &mut server).unwrap();
    drop(client);
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        assert!(Instant::now() < deadline);
        match server.read(&mut [0]) {
            Err(_) => break,
            Ok(TlsProgress::WantRead) => std::thread::sleep(Duration::from_millis(1)),
            other => panic!("unexpected abrupt-close result {other:?}"),
        }
    }
}

#[test]
fn csr_leaf_binding_expiry_and_verified_issuer() {
    let ca = Authority::new();
    let who = identity('b');
    let request = generate_key_and_csr(&who).unwrap();
    let csr = X509Req::from_pem(&request.csr_pem).unwrap();
    let key = PKey::private_key_from_pem(&request.private_key_pem).unwrap();
    assert!(csr.verify(&key).unwrap());
    assert!(csr.public_key().unwrap().public_eq(&key));
    let (cert, key) = ca.leaf(&[&who.uri()], None, now() - 60, now() + 3600);
    let info = validate_leaf(&ca.bundle(), &cert, &key, &who).unwrap();
    assert_eq!(info.issuer, ca.bundle().active);
    assert_eq!(info.expires_unix, leaf_expiry_unix(&cert).unwrap());
    assert!(info.issued_unix < now() as u64 && info.expires_unix > now() as u64);
    assert!(validate_leaf(&ca.bundle(), &cert, &request.private_key_pem, &who).is_err());
    assert!(validate_leaf(&ca.bundle(), &cert, &key, &identity('c')).is_err());
    assert!(validate_leaf(&Authority::new().bundle(), &cert, &key, &who).is_err());
    let (expired, key) = ca.leaf(&[&who.uri()], None, now() - 3600, now() - 60);
    assert!(validate_leaf(&ca.bundle(), &expired, &key, &who).is_err());
}

#[test]
fn trust_exact_bytes_generation_rollback_equivocation_and_roots() {
    let ca = Authority::new();
    let bytes = ca.bundle_bytes(4);
    let bundle = TrustBundle::parse(&bytes, None).unwrap();
    assert!(TrustBundle::parse(&ca.bundle_bytes(0), None).is_err());
    assert_eq!(bundle.digest, <[u8; 32]>::from(Sha256::digest(&bytes)));
    assert!(TrustBundle::parse(&bytes, Some(&bundle)).is_ok());
    assert!(TrustBundle::parse(&ca.bundle_bytes(3), Some(&bundle)).is_err());
    let mut different_bytes = bytes.clone();
    different_bytes.push(b'\n');
    assert!(TrustBundle::parse(&different_bytes, Some(&bundle)).is_err());
    assert!(TrustBundle::parse(&ca.bundle_bytes(5), Some(&bundle)).is_ok());
    let mut wire: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    wire["active"] = "0".repeat(64).into();
    assert!(TrustBundle::parse(&serde_json::to_vec(&wire).unwrap(), None).is_err());
    wire["active"] = bundle.active.into();
    wire["certificates"] = format!("{}garbage", wire["certificates"].as_str().unwrap()).into();
    assert!(TrustBundle::parse(&serde_json::to_vec(&wire).unwrap(), None).is_err());
    assert!(PeerIdentity::parse(&format!("{}/extra", identity('b').uri())).is_err());
    assert!(PeerIdentity::new(&"A".repeat(64), &"b".repeat(64), "pod").is_err());
    assert!(PeerIdentity::new(&"a".repeat(64), &"b".repeat(64), "pod%2fextra").is_err());
}

#[test]
fn trust_allows_two_rotation_roots_but_rejects_three() {
    let roots = [Authority::new(), Authority::new(), Authority::new()];
    let mut wire: serde_json::Value = serde_json::from_slice(&roots[0].bundle_bytes(1)).unwrap();
    let mut pem = String::new();
    for (index, root) in roots.iter().enumerate() {
        pem.push_str(&String::from_utf8(root.cert.to_pem().unwrap()).unwrap());
        wire["certificates"] = pem.clone().into();
        assert_eq!(
            TrustBundle::parse(&serde_json::to_vec(&wire).unwrap(), None).is_ok(),
            index < 2
        );
    }
}

#[test]
fn certificate_lifetime_bounds_admission_without_aborting_transfers() {
    let ca = Authority::new();
    let start = now() - 60;
    let client_end = now() + 120;
    let server_end = client_end + 120;
    let (cert, key) = ca.leaf(&[&identity('b').uri()], None, start, client_end);
    let client_context = TlsContext::new(&ca.bundle(), &cert, &key).unwrap();
    let (cert, key) = ca.leaf(&[&identity('c').uri()], None, start, server_end);
    let server_context = TlsContext::new(&ca.bundle(), &cert, &key).unwrap();
    let (mut client, mut server) = sessions(
        &client_context,
        &server_context,
        ExpectedPeer::Identity(identity('c')),
    );
    assert_eq!(client.local_expiry_unix(), Some(client_end as u64));
    assert_eq!(client.peer_expiry_unix(), None);
    assert_eq!(client.valid_until(), None);
    assert!(!client.admits_new_request(now() as u64));
    handshake(&mut client, &mut server).unwrap();
    assert_eq!(client.peer_expiry_unix(), Some(server_end as u64));
    assert_eq!(server.local_expiry_unix(), Some(server_end as u64));
    assert_eq!(server.peer_expiry_unix(), Some(client_end as u64));
    for session in [&client, &server] {
        assert_eq!(session.valid_until(), Some(client_end as u64));
        assert!(session.admits_new_request(client_end as u64 - 1));
        assert!(!session.admits_new_request(client_end as u64));
        assert!(!session.admits_new_request(server_end as u64));
    }
    // Rotate and release context owners. Sessions keep their original credentials.
    drop(client_context);
    drop(server_context);
    let replacement_ca = Authority::new();
    let _replacement = replacement_ca.context(&identity('c'), true);
    assert_eq!(client.valid_until(), Some(client_end as u64));
    // Simulate an already-admitted transfer outliving both leaf deadlines without
    // a wall-clock sleep. Expiry controls admission, never application I/O.
    client.local_expiry_unix = Some(now() as u64 - 1);
    server.peer_expiry_unix = client.local_expiry_unix;
    assert!(!client.admits_new_request(now() as u64));
    assert!(!server.admits_new_request(now() as u64));
    assert_eq!(
        client.write(b"still draining").unwrap(),
        TlsProgress::Complete(14)
    );
    read_exact(&mut server, b"still draining");
    client.failed = true;
    assert_eq!(client.valid_until(), None);
}

unsafe extern "C" {
    fn SSL_key_update(ssl: *mut c_void, update_type: i32) -> i32;
    fn SSL_set_options(ssl: *mut c_void, options: u64) -> u64;
}

#[test]
fn tls13_key_update_preserves_application_and_file_transfers() {
    for ktls in [false, true] {
        let ca = Authority::new();
        let (mut client, mut server) = sessions(
            &ca.context(&identity('b'), ktls),
            &ca.context(&identity('c'), ktls),
            ExpectedPeer::Identity(identity('c')),
        );
        handshake(&mut client, &mut server).unwrap();
        if openssl::version::number() < 0x30500000 {
            assert!(!client.offload().tx && !client.offload().rx);
            assert!(!server.offload().tx && !server.offload().rx);
        }
        eprintln!(
            "{} production KeyUpdate ktls requested={ktls} actual={:?}",
            openssl::version::version(),
            server.offload()
        );
        // Both directions, both update types, and repeated updates on one
        // connection. Read the response too: SSL_write success alone missed
        // OpenSSL 3.0's stale kernel TX key.
        for requested in [0, 1, 0, 1] {
            key_update_roundtrip(&mut client, &mut server, requested);
            key_update_roundtrip(&mut server, &mut client, requested);
        }
    }
}

fn key_update_roundtrip(sender: &mut TlsSession, receiver: &mut TlsSession, requested: i32) {
    assert_eq!(
        unsafe { SSL_key_update(sender.ssl.as_ptr().cast(), requested) },
        1
    );
    assert_eq!(
        sender.write(b"updated keys").unwrap(),
        TlsProgress::Complete(12)
    );
    read_exact(receiver, b"updated keys");
    assert_eq!(
        receiver.write(b"response").unwrap(),
        TlsProgress::Complete(8)
    );
    read_exact(sender, b"response");
    let fd = unsafe { libc::memfd_create(c"racer-rekey-file".as_ptr(), libc::MFD_CLOEXEC) };
    assert!(fd >= 0);
    let mut file = unsafe { std::fs::File::from_raw_fd(fd) };
    let payload = b"file after key update";
    file.write_all(payload).unwrap();
    assert_eq!(
        sender.sendfile(file.as_fd(), 0, payload.len()).unwrap(),
        TlsProgress::Complete(payload.len())
    );
    read_exact(receiver, payload);
    assert_eq!(
        receiver.sendfile(file.as_fd(), 0, payload.len()).unwrap(),
        TlsProgress::Complete(payload.len())
    );
    read_exact(sender, payload);
    for session in [sender, receiver] {
        let native_bits = unsafe { racer_tls_offload(session.ssl.as_ptr().cast()) };
        assert_eq!(session.offload().tx, native_bits & 1 != 0);
        assert_eq!(session.offload().rx, native_bits & 2 != 0);
        if session.offload().tx {
            assert!(session.counters().sendfile_bytes >= payload.len() as u64);
            assert_eq!(session.counters().fallback_sendfile_bytes, 0);
        } else {
            assert!(session.counters().fallback_sendfile_bytes >= payload.len() as u64);
            assert_eq!(session.counters().sendfile_bytes, 0);
        }
    }
}

#[test]
fn tls13_key_update_with_actual_ktls() {
    for requested in [0, 1] {
        let ca = Authority::new();
        let (mut client, mut server) = sessions(
            &ca.context(&identity('b'), false),
            &ca.context(&identity('c'), true),
            ExpectedPeer::Identity(identity('c')),
        );
        let openssl_30 = openssl::version::number() < 0x30100000;
        if openssl_30 {
            // Test-only bypass reproduces the reason for the production gate.
            // SSL_OP_ENABLE_KTLS = SSL_OP_BIT(3) in OpenSSL 3's public ssl.h.
            unsafe { SSL_set_options(server.ssl.as_ptr().cast(), 1 << 3) };
        }
        handshake(&mut client, &mut server).unwrap();
        eprintln!(
            "{} KeyUpdate requested={requested} receiver offload={:?}",
            openssl::version::version(),
            server.offload()
        );
        if !server.offload().tx {
            assert!(
                std::env::var_os("RACER_REQUIRE_KTLS").is_none(),
                "actual TX kTLS required but unavailable"
            );
            eprintln!("UNAVAILABLE: actual TX kTLS KeyUpdate coverage on this library/kernel");
            return;
        }
        if !openssl_30 || requested == 0 {
            key_update_roundtrip(&mut client, &mut server, requested);
            continue;
        }
        assert_eq!(
            unsafe { SSL_key_update(client.ssl.as_ptr().cast(), requested) },
            1
        );
        assert_eq!(
            client.write(b"updated keys").unwrap(),
            TlsProgress::Complete(12)
        );
        read_exact(&mut server, b"updated keys");
        assert_eq!(server.write(b"response").unwrap(), TlsProgress::Complete(8));
        let deadline = Instant::now() + Duration::from_secs(3);
        loop {
            assert!(Instant::now() < deadline);
            match client.read(&mut [0; 8]) {
                Ok(TlsProgress::WantRead | TlsProgress::WantWrite) => {
                    std::thread::sleep(Duration::from_millis(1))
                }
                Err(error) => {
                    assert!(error.to_string().contains("bad record mac"), "{error}");
                    assert!(client.write(b"failed connection").is_err());
                    eprintln!(
                        "VERIFIED: OpenSSL 3.0 TX kTLS requested KeyUpdate leaves stale keys: {error}"
                    );
                    break;
                }
                other => panic!("expected OpenSSL 3.0 stale-key failure, got {other:?}"),
            }
        }
    }
}

#[test]
fn nonblocking_write_backpressure_retains_retry_bytes() {
    let ca = Authority::new();
    let (mut client, mut server) = sessions(
        &ca.context(&identity('b'), false),
        &ca.context(&identity('c'), false),
        ExpectedPeer::Identity(identity('c')),
    );
    handshake(&mut client, &mut server).unwrap();
    let size: libc::c_int = 4096;
    assert_eq!(
        unsafe {
            libc::setsockopt(
                client.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_SNDBUF,
                (&size as *const libc::c_int).cast(),
                std::mem::size_of_val(&size) as _,
            )
        },
        0
    );
    let bytes = vec![0x5a; WRITE_CHUNK];
    let mut sent = 0;
    let mut wanted = false;
    for _ in 0..4096 {
        match client.write(&bytes).unwrap() {
            TlsProgress::Complete(n) => sent += n,
            TlsProgress::WantWrite => {
                wanted = true;
                break;
            }
            other => panic!("unexpected write state {other:?}"),
        }
    }
    assert!(
        wanted,
        "small nonblocking TCP send buffer must exert backpressure"
    );
    assert!(client.write(b"different retry").is_err());
    let deadline = Instant::now() + Duration::from_secs(5);
    let mut received = 0;
    loop {
        assert!(Instant::now() < deadline);
        let mut buffer = [0; WRITE_CHUNK];
        if let TlsProgress::Complete(n) = server.read(&mut buffer).unwrap() {
            assert!(buffer[..n].iter().all(|b| *b == 0x5a));
            received += n;
        }
        match client.write(&bytes).unwrap() {
            TlsProgress::Complete(n) => {
                sent += n;
                break;
            }
            TlsProgress::WantWrite => (),
            other => panic!("unexpected retry state {other:?}"),
        }
        std::thread::sleep(Duration::from_millis(1));
    }
    read_exact(&mut server, &vec![0x5a; sent - received]);
    assert_eq!(client.counters().tx_bytes, sent as u64);
    assert_eq!(server.counters().rx_bytes, sent as u64);
}
