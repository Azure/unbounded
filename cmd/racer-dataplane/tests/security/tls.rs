// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
const CONTROL_PLANE_URI: &str = "spiffe://racer/controlplane";
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

/// Fixture conversion only: production TLS rejects every pre-cutover URI.
pub(crate) fn signed_uri(uri: &str) -> String {
    let (kind, universe, node, pod_uid) = if uri == "spiffe://racer/controlplane" {
        (
            "controlplane",
            String::new(),
            String::new(),
            "test-cp".into(),
        )
    } else if let Ok(peer) = PeerIdentity::parse(uri) {
        ("node", peer.universe, peer.node, peer.pod_uid)
    } else {
        return uri.into();
    };
    SignedClaims {
        version: 1,
        namespace: "test-namespace".into(),
        identity: ProcessIdentity {
            kind: kind.into(),
            universe,
            node,
            pod_uid,
            boot_id: "03".repeat(32),
            pod_name: "test-name".into(),
            container_id: String::new(),
        },
    }
    .uri()
    .unwrap()
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
        let cp = uris.iter().any(|u| {
            *u == CONTROL_PLANE_URI
                || SignedClaims::parse(u).is_ok_and(|c| c.identity.kind == "controlplane")
        });
        let mut eku = ExtendedKeyUsage::new();
        eku.server_auth();
        if !cp {
            eku.client_auth();
        }
        cert.append_extension(eku.build().unwrap()).unwrap();
        let aki = AuthorityKeyIdentifier::new()
            .keyid(true)
            .build(&cert.x509v3_context(Some(&self.cert), None))
            .unwrap();
        cert.append_extension(aki).unwrap();
        let mut san = SubjectAlternativeName::new();
        if cp {
            san.dns("racer-controlplane.test-namespace.svc");
        }
        for uri in uris {
            san.uri(&signed_uri(uri));
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

    pub(crate) fn context(&self, identity: &PeerIdentity) -> TlsContext {
        let (cert, key) = self.leaf(&[&identity.uri()], None, now() - 60, now() + 3600);
        TlsContext::new(&self.bundle(), &cert, &key).unwrap()
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
            assert_offload(a);
            assert_offload(b);
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
fn real_socket_mutual_auth_strict_ktls_and_close_notify() {
    let ca = Authority::new();
    let (mut client, mut server) = sessions(
        &ca.context(&identity('b')),
        &ca.context(&identity('c')),
        ExpectedPeer::Identity(identity('c')),
    );
    assert!(client.read(&mut [0]).is_err());
    assert!(matches!(client.handshake().unwrap(), TlsProgress::WantRead));
    handshake(&mut client, &mut server).unwrap();
    assert_eq!(server.peer_identity(), Some(&identity('b')));
    assert_eq!(client.ssl.version_str(), "TLSv1.3");
    assert!(!client.ssl.session_reused());
    assert!(matches!(
        client.ssl.current_cipher().unwrap().name(),
        "TLS_AES_256_GCM_SHA384" | "TLS_AES_128_GCM_SHA256"
    ));
    let message = b"secret application bytes never go to the socket in plaintext";
    assert_eq!(
        client.write(message).unwrap(),
        TlsProgress::Complete(message.len())
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

pub(crate) fn assert_offload(session: &TlsSession) {
    assert_eq!(session.offload(), Offload { tx: true, rx: true });
    assert_eq!(unsafe { racer_tls_offload(session.ssl.as_ptr().cast()) }, 3);
    assert_eq!(session.counters().handshakes, 1);
    assert_eq!(session.counters().ktls_tx_connections, 1);
    assert_eq!(session.counters().ktls_rx_connections, 1);
}

pub(crate) fn assert_channel_offload(channel: &TlsChannel) {
    channel.assert_offload_for_test();
}

pub(crate) fn send_fatal_alert(session: &TlsSession) {
    let mut alert = [2u8, 20]; // fatal bad_record_mac
    let mut iov = libc::iovec {
        iov_base: alert.as_mut_ptr().cast(),
        iov_len: alert.len(),
    };
    // usize storage provides cmsghdr alignment and room for the one-byte type.
    let mut control = [0usize; 8];
    let mut message: libc::msghdr = unsafe { std::mem::zeroed() };
    message.msg_iov = &mut iov;
    message.msg_iovlen = 1;
    message.msg_control = control.as_mut_ptr().cast();
    message.msg_controllen = unsafe { libc::CMSG_SPACE(1) } as usize;
    unsafe {
        let header = libc::CMSG_FIRSTHDR(&message);
        (*header).cmsg_level = 282; // SOL_TLS
        (*header).cmsg_type = 1; // TLS_SET_RECORD_TYPE
        (*header).cmsg_len = libc::CMSG_LEN(1) as usize;
        *libc::CMSG_DATA(header) = 21; // alert
        assert_eq!(
            libc::sendmsg(session.as_raw_fd(), &message, libc::MSG_NOSIGNAL),
            2
        );
    }
}

#[test]
fn sendfile_uses_actual_bidirectional_offload() {
    let ca = Authority::new();
    let (mut client, mut server) = sessions(
        &ca.context(&identity('b')),
        &ca.context(&identity('c')),
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
    assert_eq!(client.counters().sendfile_bytes, payload.len() as u64);
}

#[test]
fn exact_peer_identity_rejects_wrong_universe_node_pod_and_duplicate_uri() {
    let ca = Authority::new();
    let client_context = ca.context(&identity('b'));
    let actual = identity('c');
    let mut wrong_universe = actual.clone();
    wrong_universe.universe = "d".repeat(64);
    let mut wrong_pod = actual.clone();
    wrong_pod.pod_uid = "pod-other".into();
    for expected in [wrong_universe, identity('d'), wrong_pod] {
        let (mut client, mut server) = sessions(
            &client_context,
            &ca.context(&actual),
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
    let server_context = ca.context(&identity('c'));
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
            &ca.context(&identity('b')),
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
        &ca.context(&identity('b')),
        &ca.context(&identity('c')),
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
fn certificate_lifetime_bounds_admission_and_record_io() {
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
    let _replacement = replacement_ca.context(&identity('c'));
    assert_eq!(client.valid_until(), Some(client_end as u64));
    // Simulate an already-admitted transfer outliving both leaf deadlines without
    // a wall-clock sleep. Expiry rejects admission and further record I/O.
    client.local_expiry_unix = Some(now() as u64 - 1);
    server.peer_expiry_unix = client.local_expiry_unix;
    assert!(!client.admits_new_request(now() as u64));
    assert!(!server.admits_new_request(now() as u64));
    assert!(client.write(b"expired transfer").is_err());
    assert!(server.read(&mut [0; 16]).is_err());
    client.failed = true;
    assert_eq!(client.valid_until(), None);
}

unsafe extern "C" {
    fn SSL_key_update(ssl: *mut c_void, update_type: i32) -> i32;
}

#[test]
fn signed_claims_reject_legacy_malformed_and_unknown_versions() {
    let uri = signed_uri(&identity('b').uri());
    let claims = SignedClaims::parse(&uri).unwrap();
    assert_eq!(claims.identity.boot_id, "03".repeat(32));
    assert_eq!(claims.identity.pod_name, "test-name");
    assert_eq!(claims.namespace, "test-namespace");
    assert!(SignedClaims::parse(&identity('b').uri()).is_err());
    assert!(SignedClaims::parse(CONTROL_PLANE_URI).is_err());
    for field in ["version", "boot", "role", "namespace", "pod"] {
        let mut invalid = claims.clone();
        match field {
            "version" => invalid.version = 2,
            "boot" => invalid.identity.boot_id.clear(),
            "role" => invalid.identity.kind = "admin".into(),
            "namespace" => invalid.namespace = "../system".into(),
            "pod" => invalid.identity.pod_name.clear(),
            _ => unreachable!(),
        }
        assert!(
            SignedClaims::parse(&invalid.uri().unwrap()).is_err(),
            "{field}"
        );
    }
    let ca = Authority::new();
    let claims = SignedClaims::parse(&signed_uri(CONTROL_PLANE_URI)).unwrap();
    let (leaf, _) = ca.leaf(&[&claims.uri().unwrap()], None, now() - 1, now() + 60);
    let leaf = X509::from_pem(&leaf).unwrap();
    assert!(certificate_claims(&leaf).is_ok());
}

#[test]
fn tls13_key_update_preserves_application_and_file_transfers() {
    let ca = Authority::new();
    let contexts = (ca.context(&identity('b')), ca.context(&identity('c')));
    // Keep repeated updates on each successful connection. On older kernels,
    // each direction/type still gets its own attempt and recovery check.
    for reverse in [false, true] {
        for requested in [0, 1] {
            let (mut client, mut server) = sessions(
                &contexts.0,
                &contexts.1,
                ExpectedPeer::Identity(identity('c')),
            );
            handshake(&mut client, &mut server).unwrap();
            for _ in 0..3 {
                let result = if reverse {
                    key_update_roundtrip(&mut server, &mut client, requested)
                } else {
                    key_update_roundtrip(&mut client, &mut server, requested)
                };
                if let Err(error) = result {
                    assert!(
                        !kernel_supports_rekey(),
                        "supported kernel rekey failed: {error}"
                    );
                    assert_rekey_failure(&error);
                    assert!(client.failed || server.failed);
                    for session in [&mut client, &mut server] {
                        if session.failed {
                            assert_terminal(session);
                        }
                    }
                    eprintln!(
                        "unsupported kernel rekey closed: reverse={reverse} requested={requested}: {error}"
                    );
                    break;
                }
            }
            assert_reconnect(&contexts.0, &contexts.1);
        }
    }
}

fn key_update_roundtrip(
    sender: &mut TlsSession,
    receiver: &mut TlsSession,
    requested: i32,
) -> io::Result<()> {
    assert_eq!(
        unsafe { SSL_key_update(sender.ssl.as_ptr().cast(), requested) },
        1
    );
    assert_eq!(
        complete(|| sender.write(b"updated keys"))?,
        TlsProgress::Complete(12)
    );
    read_exact_result(receiver, b"updated keys")?;
    assert_eq!(
        complete(|| receiver.write(b"response"))?,
        TlsProgress::Complete(8)
    );
    read_exact_result(sender, b"response")?;
    let fd = unsafe { libc::memfd_create(c"racer-rekey-file".as_ptr(), libc::MFD_CLOEXEC) };
    assert!(fd >= 0);
    let mut file = unsafe { std::fs::File::from_raw_fd(fd) };
    let payload = b"file after key update";
    file.write_all(payload).unwrap();
    assert_eq!(
        complete(|| sender.sendfile(file.as_fd(), 0, payload.len()))?,
        TlsProgress::Complete(payload.len())
    );
    read_exact_result(receiver, payload)?;
    assert_eq!(
        complete(|| receiver.sendfile(file.as_fd(), 0, payload.len()))?,
        TlsProgress::Complete(payload.len())
    );
    read_exact_result(sender, payload)?;
    for session in [sender, receiver] {
        assert_offload(session);
        assert!(session.counters().sendfile_bytes >= payload.len() as u64);
    }
    Ok(())
}

#[test]
fn tls13_key_update_with_actual_ktls() {
    // Regression replacing the OpenSSL 3.0 stale-TX-key repro: rejected key
    // replacement must be terminal, even if the caller ignores the first error.
    for requested in [0, 1] {
        for direction in [1, 2] {
            let ca = Authority::new();
            let client_context = ca.context(&identity('b'));
            let server_context = ca.context(&identity('c'));
            let recovery = client_context.clone();
            std::thread::spawn(move || {
                let (mut session, peer) = external_peer(&ca, &client_context, Some(requested));
                assert_eq!(
                    complete(|| session.handshake()).unwrap(),
                    TlsProgress::Complete(())
                );
                assert_offload(&session);
                reject_keys(session.as_raw_fd(), direction);
                let error = if direction == 1 {
                    assert_eq!(
                        unsafe { SSL_key_update(session.ssl.as_ptr().cast(), requested) },
                        1
                    );
                    complete(|| session.write(b"ping")).unwrap_err()
                } else {
                    assert_eq!(
                        complete(|| session.write(b"ping")).unwrap(),
                        TlsProgress::Complete(4)
                    );
                    complete(|| session.read(&mut [0; 4])).unwrap_err()
                };
                assert_rekey_failure(&error);
                assert_terminal(&mut session);
                drop(session);
                peer.join().unwrap();
            })
            .join()
            .unwrap();
            assert_reconnect(&recovery, &server_context);
        }
    }
}

fn complete<T>(
    mut operation: impl FnMut() -> io::Result<TlsProgress<T>>,
) -> io::Result<TlsProgress<T>> {
    let end = Instant::now() + Duration::from_secs(3);
    loop {
        match operation()? {
            TlsProgress::WantRead | TlsProgress::WantWrite => {
                assert!(Instant::now() < end, "TLS operation stalled");
                std::thread::sleep(Duration::from_millis(1));
            }
            result => return Ok(result),
        }
    }
}

fn read_exact_result(session: &mut TlsSession, expected: &[u8]) -> io::Result<()> {
    let mut bytes = vec![0; expected.len()];
    let mut offset = 0;
    while offset < bytes.len() {
        let TlsProgress::Complete(n) = complete(|| session.read(&mut bytes[offset..]))? else {
            panic!("unexpected TLS EOF")
        };
        assert!(n > 0);
        offset += n;
    }
    assert_eq!(bytes, expected);
    Ok(())
}

fn kernel_supports_rekey() -> bool {
    let mut name = std::mem::MaybeUninit::<libc::utsname>::uninit();
    assert_eq!(unsafe { libc::uname(name.as_mut_ptr()) }, 0);
    let name = unsafe { name.assume_init() };
    let release = unsafe { std::ffi::CStr::from_ptr(name.release.as_ptr()) }
        .to_str()
        .unwrap();
    let mut parts = release.split('.');
    let major: u32 = parts.next().unwrap().parse().unwrap();
    let minor: u32 = parts.next().unwrap().parse().unwrap();
    (major, minor) >= (6, 14)
}

fn assert_rekey_failure(error: &io::Error) {
    let message = error.to_string();
    assert!(
        message.contains("record layer failure") || message.contains("no suitable record layer"),
        "unexpected rekey error: {error}"
    );
}

fn assert_terminal(session: &mut TlsSession) {
    let counters = session.counters();
    for _ in 0..2 {
        assert!(session.handshake().is_err());
        assert!(session.read(&mut [0; 8]).is_err());
        assert!(session.read(&mut []).is_err());
        assert!(session.write(b"must not escape").is_err());
        assert!(session.write(b"").is_err());
        let file = std::fs::File::open("Cargo.toml").unwrap();
        assert!(session.sendfile(file.as_fd(), 0, 1).is_err());
        assert!(session.sendfile(file.as_fd(), 0, 0).is_err());
        assert!(session.shutdown().is_err());
        assert_eq!(session.valid_until(), None);
        assert!(!session.admits_new_request(now() as u64));
    }
    assert_eq!(session.counters().tx_bytes, counters.tx_bytes);
    assert_eq!(session.counters().rx_bytes, counters.rx_bytes);
    assert_eq!(session.counters().sendfile_bytes, counters.sendfile_bytes);
}

fn assert_reconnect(client: &TlsContext, server: &TlsContext) {
    let (mut client, mut server) = sessions(client, server, ExpectedPeer::Identity(identity('c')));
    handshake(&mut client, &mut server).unwrap();
    assert_eq!(
        complete(|| client.write(b"reconnected")).unwrap(),
        TlsProgress::Complete(11)
    );
    read_exact(&mut server, b"reconnected");
    assert_eq!(
        complete(|| server.write(b"recovered")).unwrap(),
        TlsProgress::Complete(9)
    );
    read_exact(&mut client, b"recovered");
}

// Thread-local seccomp filter, scoped to one live socket. It cannot affect other
// tests or the peer thread. No TSYNC, production hooks, or synthetic BIO bits.
fn reject_keys(fd: RawFd, direction: u32) {
    let insn = |code, jt, jf, k| libc::sock_filter { code, jt, jf, k };
    let mut filter = [
        insn(0x20, 0, 0, 0),
        insn(0x15, 0, 7, libc::SYS_setsockopt as u32),
        insn(0x20, 0, 0, 16), // args[0]: fd
        insn(0x15, 0, 5, fd as u32),
        insn(0x20, 0, 0, 24),  // args[1]: level
        insn(0x15, 0, 3, 282), // SOL_TLS
        insn(0x20, 0, 0, 32),  // args[2]: TLS_TX (1), TLS_RX (2), both (0)
        insn(0x15, 0, if direction == 0 { 0 } else { 1 }, direction),
        insn(0x06, 0, 0, 0x00050000 | libc::EOPNOTSUPP as u32),
        insn(0x06, 0, 0, 0x7fff0000),
    ];
    let program = libc::sock_fprog {
        len: filter.len() as u16,
        filter: filter.as_mut_ptr(),
    };
    unsafe {
        assert_eq!(libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0), 0);
        assert_eq!(libc::prctl(libc::PR_SET_SECCOMP, 2, &program), 0);
    }
}

// An independent OpenSSL peer can send KeyUpdate on Linux 5.15 even though the
// production endpoint's kernel cannot replace keys. This is fixture-only TLS.
fn external_peer(
    ca: &Authority,
    context: &TlsContext,
    update: Option<i32>,
) -> (TlsSession, std::thread::JoinHandle<()>) {
    use std::io::Read;
    let (cert, key) = ca.leaf(&[CONTROL_PLANE_URI], None, now() - 60, now() + 3600);
    let mut builder = SslContextBuilder::new(SslMethod::tls()).unwrap();
    builder
        .set_min_proto_version(Some(SslVersion::TLS1_3))
        .unwrap();
    builder
        .set_max_proto_version(Some(SslVersion::TLS1_3))
        .unwrap();
    builder
        .set_certificate(&X509::from_pem(&cert).unwrap())
        .unwrap();
    builder
        .set_private_key(&PKey::private_key_from_pem(&key).unwrap())
        .unwrap();
    builder.set_num_tickets(0).unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let socket = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    socket.set_nonblocking(true).unwrap();
    let peer = std::thread::spawn(move || {
        let socket = listener.accept().unwrap().0;
        socket
            .set_read_timeout(Some(Duration::from_secs(3)))
            .unwrap();
        socket
            .set_write_timeout(Some(Duration::from_secs(3)))
            .unwrap();
        let ssl = Ssl::new(&builder.build()).unwrap();
        let mut stream = openssl::ssl::SslStream::new(ssl, socket).unwrap();
        if stream.accept().is_err() {
            return;
        }
        let mut bytes = [0; 4];
        if stream.read_exact(&mut bytes).is_err() {
            return;
        }
        assert_eq!(&bytes, b"ping");
        if let Some(requested) = update {
            use foreign_types::ForeignTypeRef;
            assert_eq!(
                unsafe { SSL_key_update(stream.ssl().as_ptr().cast(), requested) },
                1
            );
        }
        let _ = stream.write_all(b"pong");
    });
    (
        TlsSession::client(
            context,
            socket.into(),
            ExpectedPeer::ControlPlane {
                dns_name: "racer-controlplane.test-namespace.svc".into(),
            },
        )
        .unwrap(),
        peer,
    )
}

#[test]
fn normal_and_bootstrap_require_both_offload_directions() {
    for bootstrap in [false, true] {
        for missing in [None, Some(0), Some(1), Some(2)] {
            std::thread::spawn(move || {
                let ca = Authority::new();
                let context = if bootstrap {
                    TlsContext::bootstrap(&ca.bundle()).unwrap()
                } else {
                    ca.context(&identity('b'))
                };
                let (mut session, peer) = external_peer(&ca, &context, None);
                if let Some(direction) = missing {
                    reject_keys(session.as_raw_fd(), direction);
                }
                let result = complete(|| session.handshake());
                if let Some(direction) = missing {
                    let error = result.unwrap_err();
                    assert!(
                        error.to_string().contains("requires actual TX and RX"),
                        "{error}"
                    );
                    assert_eq!(
                        session.offload(),
                        Offload {
                            tx: direction == 2,
                            rx: direction == 1
                        }
                    );
                    assert_eq!(session.counters().handshakes, 0);
                    assert_terminal(&mut session);
                } else {
                    assert_eq!(result.unwrap(), TlsProgress::Complete(()));
                    assert_offload(&session);
                    assert_eq!(session.local_expiry_unix().is_none(), bootstrap);
                    assert_eq!(
                        complete(|| session.write(b"ping")).unwrap(),
                        TlsProgress::Complete(4)
                    );
                    read_exact(&mut session, b"pong");
                }
                drop(session);
                peer.join().unwrap();
            })
            .join()
            .unwrap();
        }
    }
}

#[test]
fn inbound_key_update_succeeds_or_closes_and_reconnects_on_older_kernels() {
    let ca = Authority::new();
    let context = ca.context(&identity('b'));
    let server = ca.context(&identity('c'));
    for requested in [0, 1, 0, 1] {
        let (mut session, peer) = external_peer(&ca, &context, Some(requested));
        assert_eq!(
            complete(|| session.handshake()).unwrap(),
            TlsProgress::Complete(())
        );
        assert_offload(&session);
        assert_eq!(
            complete(|| session.write(b"ping")).unwrap(),
            TlsProgress::Complete(4)
        );
        match read_exact_result(&mut session, b"pong") {
            Ok(()) => assert_offload(&session),
            Err(error) => {
                assert!(
                    !kernel_supports_rekey(),
                    "supported kernel RX rekey failed: {error}"
                );
                assert_rekey_failure(&error);
                assert_terminal(&mut session);
            }
        }
        drop(session);
        peer.join().unwrap();
        assert_reconnect(&context, &server);
    }
}

#[test]
fn inbound_admission_rejects_missing_offload_and_latches_failure() {
    for direction in [0, 1, 2] {
        std::thread::spawn(move || {
            let ca = Authority::new();
            let (mut client, mut server) = sessions(
                &ca.context(&identity('b')),
                &ca.context(&identity('c')),
                ExpectedPeer::Identity(identity('c')),
            );
            reject_keys(server.as_raw_fd(), direction);
            let error = handshake(&mut client, &mut server).unwrap_err();
            assert!(
                error.to_string().contains("requires actual TX and RX"),
                "{error}"
            );
            assert_eq!(
                server.offload(),
                Offload {
                    tx: direction == 2,
                    rx: direction == 1
                }
            );
            assert_eq!(server.counters().handshakes, 0);
            assert_terminal(&mut server);
        })
        .join()
        .unwrap();
    }
}

#[test]
fn negotiation_requires_tls13_aes_gcm() {
    let ca = Authority::new();
    for cipher in [
        "TLS_AES_128_GCM_SHA256",
        "TLS_AES_256_GCM_SHA384",
        "TLS_CHACHA20_POLY1305_SHA256",
        "TLSv1.2",
    ] {
        let (mut client, mut server) = sessions(
            &ca.context(&identity('b')),
            &ca.context(&identity('c')),
            ExpectedPeer::Identity(identity('c')),
        );
        if cipher == "TLSv1.2" {
            server
                .ssl
                .set_min_proto_version(Some(SslVersion::TLS1_2))
                .unwrap();
            server
                .ssl
                .set_max_proto_version(Some(SslVersion::TLS1_2))
                .unwrap();
        } else {
            server.ssl.set_ciphersuites(cipher).unwrap();
        }
        let result = handshake(&mut client, &mut server);
        if cipher.starts_with("TLS_AES_") {
            result.unwrap();
            assert_eq!(client.ssl.current_cipher().unwrap().name(), cipher);
            assert_eq!(client.write(b"AES-GCM").unwrap(), TlsProgress::Complete(7));
            read_exact(&mut server, b"AES-GCM");
        } else {
            assert!(result.is_err(), "must reject {cipher}");
            assert_terminal(&mut server);
            assert!(client.write(b"no application admission").is_err());
        }
    }
}

#[test]
fn nonblocking_write_backpressure_retains_retry_bytes() {
    let ca = Authority::new();
    let (mut client, mut server) = sessions(
        &ca.context(&identity('b')),
        &ca.context(&identity('c')),
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
