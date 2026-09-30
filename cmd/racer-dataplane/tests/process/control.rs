//! Minimal real TLS controller fixture: bearer enrollment, then mTLS publication.
use super::*;
use base64::{Engine, engine::general_purpose::STANDARD};
use racer_dataplane::{
    control::{
        caches::{CacheDefinition, canonical_socket_paths},
        wire,
    },
    model::{CacheId, ClusterId, MembershipVersion, NodeId},
    topology::membership::Member,
};
use std::{num::NonZeroU32, time::SystemTime};

pub struct Control {
    pub endpoint: String,
    pub bundle: Vec<u8>,
    pub enrollments: Arc<AtomicUsize>,
    pub polls: Arc<AtomicUsize>,
    pub blocked: Arc<AtomicBool>,
    pub publication: Arc<Mutex<wire::Publication>>,
    stop: Arc<AtomicBool>,
    thread: Option<thread::JoinHandle<()>>,
}
impl Control {
    pub fn start(root: &Path) -> Self {
        Self::start_with(root, vec![(CACHE.into(), NAME.into())])
    }
    pub fn start_with(root: &Path, caches: Vec<(String, String)>) -> Self {
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
        // Runtime-generated test material, distinct for the two encryption purposes.
        let keys: Vec<_> = caches.iter().flat_map(|(cache, _)| [("page", 7u8), ("origin_credentials", 8u8)].into_iter().map(move |(purpose, id)| {
            let mut material = [0; 32];
            getrandom::getrandom(&mut material).unwrap();
            serde_json::json!({"cache": cache, "id": STANDARD.encode([id; 16]), "purpose": purpose, "state": "active", "material": STANDARD.encode(material)})
        })).collect();
        let bundle = serde_json::json!({"schema_version": 1, "cluster": CLUSTER, "generation": "1", "peer_trust_roots": [STANDARD.encode(ca.der())], "cache_keys": keys});
        let bundle_bytes = serde_json::to_vec(&bundle).unwrap();
        let publication = Arc::new(Mutex::new(wire::Publication {
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
            caches: caches
                .into_iter()
                .map(|(id, name)| {
                    let (client_socket, origin_socket) = canonical_socket_paths(&name).unwrap();
                    CacheDefinition {
                        id: CacheId(id),
                        name,
                        client_socket,
                        origin_socket,
                    }
                })
                .collect(),
        }));
        let published = publication.clone();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = format!("https://{}", listener.local_addr().unwrap());
        listener.set_nonblocking(true).unwrap();
        let stop = Arc::new(AtomicBool::new(false));
        let enrollments = Arc::new(AtomicUsize::new(0));
        let polls = Arc::new(AtomicUsize::new(0));
        let blocked = Arc::new(AtomicBool::new(false));
        let (observed, paused) = (polls.clone(), blocked.clone());
        let (stopping, issued) = (stop.clone(), enrollments.clone());
        let ca = Arc::new(ca);
        let ca_key = Arc::new(ca_key);
        let thread = thread::spawn(move || {
            let mut handlers = Vec::new();
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
                let (tls, published, observed, paused, stopping, issued) = (
                    tls.clone(),
                    published.clone(),
                    observed.clone(),
                    paused.clone(),
                    stopping.clone(),
                    issued.clone(),
                );
                let (ca, ca_key, bundle) = (ca.clone(), ca_key.clone(), bundle.clone());
                handlers.push(thread::spawn(move || {
                let mut stream = rustls::StreamOwned::new(
                    rustls::ServerConnection::new(tls.clone()).unwrap(),
                    socket,
                );
                let Ok(head) = read_head(&mut stream) else {
                    return;
                };
                let fields = fields(&head);
                let length = fields
                    .get("content-length")
                    .map_or(0, |n| n.parse::<usize>().unwrap());
                assert!(length <= wire::MAX_ENROLLMENT_BYTES);
                let mut body = vec![0; length];
                if stream.read_exact(&mut body).is_err() {
                    return;
                }
                let (status, body) = if head.starts_with("GET /v1/keyring") {
                    assert!(stream.conn.peer_certificates().is_some() || fields["authorization"].starts_with("Bearer fixture.token"));
                    if head.starts_with("GET /v1/keyring?after=1 ") {
                        (204, Vec::new())
                    } else {
                        (200, serde_json::to_vec(&bundle).unwrap())
                    }
                } else if head.starts_with("POST /v1/bootstrap ") {
                    let binding = fields["authorization"]
                        .strip_prefix("Bearer fixture.token")
                        .unwrap();
                    let node = if binding.is_empty() {
                        NODE
                    } else {
                        binding.strip_prefix('.').unwrap()
                    };
                    assert!(
                        published
                            .lock()
                            .unwrap()
                            .members
                            .iter()
                            .any(|member| member.node.0 == node)
                    );
                    assert!(stream.conn.peer_certificates().is_none());
                    let request = wire::decode_enrollment_request(&body).unwrap();
                    assert_eq!(request.cluster.0, CLUSTER);
                    let der =
                        rustls::pki_types::CertificateSigningRequestDer::from(request.csr_der);
                    let mut csr = rcgen::CertificateSigningRequestParams::from_der(&der).unwrap();
                    csr.params.not_before = (SystemTime::now() - Duration::from_secs(1)).into();
                    csr.params.not_after = (SystemTime::now() + Duration::from_secs(86399)).into();
                    csr.params.subject_alt_names = vec![rcgen::SanType::URI(
                        format!("spiffe://{CLUSTER}/node/{node}")
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
                            node: NodeId(node.into()),
                            enrollment: request.enrollment,
                            certificate_chain: vec![certificate.der().to_vec()],
                        })
                        .unwrap(),
                    )
                } else {
                    assert!(head.starts_with("GET /v1/snapshot"));
                    assert!(stream.conn.peer_certificates().is_some());
                    observed.fetch_add(1, Ordering::Release);
                    while paused.load(Ordering::Acquire) && !stopping.load(Ordering::Acquire) {
                        thread::sleep(Duration::from_millis(2));
                    }
                    let publication = published.lock().unwrap();
                    if head
                        .lines()
                        .next()
                        .unwrap()
                        .contains(&format!("?after={} ", publication.sequence.0))
                    {
                        thread::sleep(Duration::from_millis(20));
                        (204, Vec::new())
                    } else {
                        (200, wire::encode_publication(&publication).unwrap())
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
                }));
                let mut i = 0;
                while i < handlers.len() {
                    if handlers[i].is_finished() {
                        handlers.swap_remove(i).join().unwrap();
                    } else {
                        i += 1;
                    }
                }
            }
            for handler in handlers {
                handler.join().unwrap();
            }
        });
        Self {
            bundle: bundle_bytes,
            endpoint,
            enrollments,
            polls,
            blocked,
            publication,
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
