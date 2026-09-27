//! Minimal real TLS controller fixture: bearer enrollment, then mTLS publication.
use super::*;
use base64::{Engine, engine::general_purpose::STANDARD};
use racer_dataplane::{
    control::{
        caches::{CacheDefinition, canonical_socket_paths},
        wire,
    },
    model::identity::{CacheId, ClusterId, MembershipVersion, NodeId},
    topology::membership::Member,
};
use std::{num::NonZeroU32, os::unix::fs::symlink, time::SystemTime};

pub struct Control {
    pub endpoint: String,
    pub enrollments: Arc<AtomicUsize>,
    stop: Arc<AtomicBool>,
    thread: Option<thread::JoinHandle<()>>,
}
impl Control {
    pub fn start(root: &Path) -> Self {
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let ca = params.self_signed(&ca_key).unwrap();
        let server_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let mut params = rcgen::CertificateParams::new(vec!["127.0.0.1".into()]).unwrap();
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
        let cert = params.signed_by(&server_key, &ca, &ca_key).unwrap();
        let mut roots = rustls::RootCertStore::empty();
        roots.add(ca.der().clone()).unwrap();
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
            Arc::new(roots),
            provider.clone(),
        )
        .allow_unauthenticated()
        .build()
        .unwrap();
        let tls = Arc::new(
            rustls::ServerConfig::builder_with_provider(provider)
                .with_safe_default_protocol_versions()
                .unwrap()
                .with_client_cert_verifier(verifier)
                .with_single_cert(
                    vec![cert.der().clone()],
                    rustls::pki_types::PrivatePkcs8KeyDer::from(server_key.serialize_der()).into(),
                )
                .unwrap(),
        );
        fs::write(root.join("trust.pem"), ca.pem()).unwrap();
        fs::write(root.join("token"), "fixture.token").unwrap();
        fs::create_dir_all(root.join("secrets/epoch")).unwrap();
        // Runtime-generated test material, distinct for the two encryption purposes.
        let keys: Vec<_> = [("page", 7u8), ("origin_credentials", 8u8)].into_iter().map(|(purpose, id)| {
            let mut material = [0; 32];
            getrandom::getrandom(&mut material).unwrap();
            serde_json::json!({"cache": CACHE, "id": STANDARD.encode([id; 16]), "purpose": purpose, "state": "active", "material": STANDARD.encode(material)})
        }).collect();
        let bundle = serde_json::json!({"schema_version": 1, "cluster": CLUSTER, "generation": "1", "peer_trust_roots": [STANDARD.encode(ca.der())], "cache_keys": keys});
        fs::write(
            root.join("secrets/epoch/bundle.json"),
            serde_json::to_vec(&bundle).unwrap(),
        )
        .unwrap();
        symlink("epoch", root.join("secrets/..data")).unwrap();
        let (client_socket, origin_socket) = canonical_socket_paths(NAME).unwrap();
        let publication = wire::encode_publication(&wire::Publication {
            schema_version: 1,
            cluster: ClusterId(CLUSTER.into()),
            sequence: wire::PublicationSequence(1),
            membership_version: MembershipVersion(1),
            members: vec![Member {
                node: NodeId(NODE.into()),
                shares: NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:7443".into(),
                rails: vec![],
                alignment_enabled: false,
            }],
            caches: vec![CacheDefinition {
                id: CacheId(CACHE.into()),
                name: NAME.into(),
                client_socket,
                origin_socket,
            }],
        })
        .unwrap();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = format!("https://{}", listener.local_addr().unwrap());
        listener.set_nonblocking(true).unwrap();
        let stop = Arc::new(AtomicBool::new(false));
        let enrollments = Arc::new(AtomicUsize::new(0));
        let (stopping, issued) = (stop.clone(), enrollments.clone());
        let thread = thread::spawn(move || {
            while !stopping.load(Ordering::Acquire) {
                let (socket, _) = match listener.accept() {
                    Ok(pair) => pair,
                    Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(2));
                        continue;
                    }
                    Err(error) => panic!("control accept: {error}"),
                };
                socket
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                socket
                    .set_write_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                let mut stream = rustls::StreamOwned::new(
                    rustls::ServerConnection::new(tls.clone()).unwrap(),
                    socket,
                );
                let Ok(head) = read_head(&mut stream) else {
                    continue;
                };
                let fields = fields(&head);
                let length = fields
                    .get("content-length")
                    .map_or(0, |n| n.parse::<usize>().unwrap());
                assert!(length <= wire::MAX_ENROLLMENT_BYTES);
                let mut body = vec![0; length];
                if stream.read_exact(&mut body).is_err() {
                    continue;
                }
                let (status, body) = if head.starts_with("POST /v1/bootstrap ") {
                    assert_eq!(fields["authorization"], "Bearer fixture.token");
                    assert!(stream.conn.peer_certificates().is_none());
                    let request = wire::decode_enrollment_request(&body).unwrap();
                    assert_eq!(request.cluster.0, CLUSTER);
                    let der =
                        rustls::pki_types::CertificateSigningRequestDer::from(request.csr_der);
                    let mut csr = rcgen::CertificateSigningRequestParams::from_der(&der).unwrap();
                    csr.params.not_before = (SystemTime::now() - Duration::from_secs(1)).into();
                    csr.params.not_after = (SystemTime::now() + Duration::from_secs(86399)).into();
                    csr.params.subject_alt_names = vec![rcgen::SanType::URI(
                        format!("spiffe://{CLUSTER}/node/{NODE}")
                            .try_into()
                            .unwrap(),
                    )];
                    csr.params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
                    csr.params.extended_key_usages =
                        vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
                    let certificate = csr.signed_by(&ca, &ca_key).unwrap();
                    issued.fetch_add(1, Ordering::Release);
                    (
                        200,
                        wire::encode_enrollment_response(&wire::EnrollmentResponse {
                            schema_version: 1,
                            cluster: request.cluster,
                            node: NodeId(NODE.into()),
                            enrollment: request.enrollment,
                            certificate_chain: vec![certificate.der().to_vec()],
                        })
                        .unwrap(),
                    )
                } else {
                    assert!(head.starts_with("GET /v1/snapshot"));
                    assert!(stream.conn.peer_certificates().is_some());
                    if head.lines().next().unwrap().contains("?after=1") {
                        thread::sleep(Duration::from_millis(20));
                        (204, Vec::new())
                    } else {
                        (200, publication.clone())
                    }
                };
                let response = format!(
                    "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                let _ = stream
                    .write_all(response.as_bytes())
                    .and_then(|()| stream.write_all(&body))
                    .and_then(|()| stream.flush());
            }
        });
        Self {
            endpoint,
            enrollments,
            stop,
            thread: Some(thread),
        }
    }
}
impl Drop for Control {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        if let Some(thread) = self.thread.take() {
            thread.join().unwrap();
        }
    }
}
